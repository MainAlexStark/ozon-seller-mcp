package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MainAlexStark/ozon-seller-mcp/internal/mcp"
	"github.com/MainAlexStark/ozon-seller-mcp/ozon"
)

// Финансы.
//
// /v3/finance/transaction/list отключён в 2026 году, и замена устроена
// иначе: не «выгрузка за период», а начисления за один день и
// начисления по конкретным отправлениям. Просить у них диапазон дат
// бессмысленно — такого параметра там нет.
//
// Отсюда главная ловушка этих методов, ради которой написан этот файл.
// Вопрос «сколько вышло за декабрь» превращается в тридцать один вызов,
// каждый из которых возвращает все начисления дня со всей вложенностью:
// на живом магазине это 30 КБ плотного JSON за день и под мегабайт за
// месяц. Клиент режет ответ инструмента по числу токенов, поэтому такой
// ответ до модели не доезжает вовсе — она видит отказ, не понимает
// причины и повторяет вызов. Так вопрос о выручке заканчивался не
// ответом, а циклом.
//
// Поэтому наружу по умолчанию отдаётся не выгрузка, а свод: суммы по
// дням, категориям и видам начислений. Сырые начисления никуда не
// делись — они за флагом detail, и за один день, а не за месяц.

const (
	// maxSummaryDays — предел длины периода. Не техническое
	// ограничение, а защита от «посчитай за год» одним вызовом:
	// это 365 запросов к Ozon, и лимитер растянет их на час.
	maxSummaryDays = 92

	// maxPagesPerDay — предел постраничного обхода одного дня.
	// Страховка от курсора, который перестал двигаться: цикл внутри
	// сервера так же вреден, как цикл в разговоре, и заметить его
	// снаружи ещё труднее.
	maxPagesPerDay = 50

	// summaryBudget — сколько времени свод тратит на обход дней,
	// прежде чем вернуть посчитанное и курсор для продолжения.
	//
	// Лимитер пропускает к финансовым методам десять запросов в
	// минуту, то есть месяц по дням физически не собирается за один
	// вызов. Выбор такой: молча упереться в таймаут клиента и не
	// вернуть ничего — или вернуть часть периода и сказать, с какого
	// дня продолжить. Второе честнее и, главное, конечно.
	summaryBudget = 40 * time.Second

	// budgetReserve — запас до дедлайна запроса, чтобы успеть
	// напечатать ответ, а не оборваться на середине.
	budgetReserve = 5 * time.Second
)

// RegisterFinance добавляет финансовые инструменты.
func (r *Registry) RegisterFinance() {
	r.AddCustom(mcp.Tool{
		Name: "ozon_finance_summary",
		Description: "Свод начислений за период: сколько начислено, сколько удержано и что вышло чистыми — " +
			"по дням, категориям и видам начислений. Сервер сам обходит дни и складывает суммы, " +
			"наружу отдаёт несколько десятков строк вместо мегабайта. " +
			"Это ответ на вопросы вида «сколько заработали за декабрь»: " +
			"ozon_finance_by_day нужен, только когда разбираешь один конкретный день. " +
			"Если период не уместился в отведённое время, в конце ответа будет день, с которого продолжить.",
		InputSchema: schema(obj{
			"date_from": str("Начало периода, YYYY-MM-DD"),
			"date_to":   str("Конец периода включительно, YYYY-MM-DD"),
			"by_day":    boolean("Показывать разбивку по дням; по умолчанию true"),
		}, "date_from", "date_to"),
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			args, err := decodeArgs(raw)
			if err != nil {
				return "", err
			}

			from, to, err := summaryPeriod(args)
			if err != nil {
				return "", err
			}

			sum := newSummary(from, to)
			deadline := budgetDeadline(ctx)

			day := from
			for !day.After(to) {
				// Бюджет проверяется перед днём, а не после: прерваться
				// на середине дня значит отдать неполные суммы под
				// видом полных.
				if time.Now().After(deadline) && day.After(from) {
					sum.next = day
					break
				}

				accruals, partial, err := fetchDay(ctx, r.client, day.Format(dayLayout))
				if err != nil {
					// Часть периода уже посчитана — выбрасывать её
					// вместе с ошибкой незачем.
					if sum.days > 0 {
						sum.next = day
						sum.failure = err
						break
					}
					return "", decorate(err)
				}
				sum.addDay(day.Format(dayLayout), accruals, partial)
				day = day.AddDate(0, 0, 1)
			}

			return r.safetyFor(ctx).TrimResponse(sum.render(argBool(args["by_day"], true))), nil
		},
	})

	r.AddCustom(mcp.Tool{
		Name: "ozon_finance_by_day",
		Description: "Начисления за один день: выплаты, комиссии, удержания. " +
			"По умолчанию — свод за день (итоги, категории, виды начислений); " +
			"сырые начисления со всей вложенностью включает detail: true. " +
			"Именно за день, а не за период: за месяц есть ozon_finance_summary, " +
			"собирать месяц вызовами этого метода не нужно. " +
			"Без даты берётся вчерашний день — сегодняшние начисления ещё неполные.",
		InputSchema: schema(obj{
			"date":    str("День, YYYY-MM-DD. По умолчанию вчера"),
			"detail":  boolean("Отдать сырые начисления вместо свода. Осторожно: за день это десятки килобайт"),
			"last_id": str("Курсор постраничного обхода; только вместе с detail"),
		}),
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			args, err := decodeArgs(raw)
			if err != nil {
				return "", err
			}

			day, _ := args["date"].(string)
			if day == "" {
				day = time.Now().AddDate(0, 0, -1).Format(dayLayout)
			}
			if err := checkDay(day); err != nil {
				return "", err
			}

			safety := r.safetyFor(ctx)

			// Детальный режим отдаёт ровно то, что ответил Ozon: раз
			// человек просит разбираться в начислениях поимённо,
			// подменять их сводом нельзя. Страницу за раз — потолок
			// ответа тут единственная защита.
			if argBool(args["detail"], false) {
				body := map[string]any{"date": day}
				if cursor, _ := args["last_id"].(string); cursor != "" {
					body["last_id"] = cursor
				}
				page, err := r.client.Call(ctx, ozon.PathFinanceAccrualByDay, body)
				if err != nil {
					return "", decorate(err)
				}
				return safety.TrimResponse(formatJSON(page)), nil
			}

			accruals, partial, err := fetchDay(ctx, r.client, day)
			if err != nil {
				return "", decorate(err)
			}

			sum := newSummary(mustDay(day), mustDay(day))
			sum.addDay(day, accruals, partial)
			return safety.TrimResponse(sum.render(false)), nil
		},
	})

	r.Add(Spec{
		Name: "ozon_finance_postings",
		Path: ozon.PathFinanceAccrualPostings,
		Desc: "Начисления по конкретным отправлениям: что именно удержано по каждому заказу. " +
			"Здесь видно реальную экономику продажи. Номера отправлений берите из " +
			"ozon_postings_list или ozon_fbo_postings_list — по периоду этот метод не ищет.",
		Schema: schema(obj{
			"posting_numbers": arr(obj{"type": "string"}, "Номера отправлений, до 200 за раз"),
		}, "posting_numbers"),
		Build: func(a map[string]any) (any, error) {
			numbers, err := stringList(a["posting_numbers"])
			if err != nil {
				return nil, fmt.Errorf("posting_numbers: %w", err)
			}
			// Ozon отвечает на превышение невнятной ошибкой валидации,
			// поэтому считаем сами.
			if len(numbers) > maxPostingsPerAccrualRequest {
				return nil, fmt.Errorf(
					"за один вызов принимается не больше %d отправлений, передано %d — разбейте на несколько вызовов",
					maxPostingsPerAccrualRequest, len(numbers))
			}
			return map[string]any{"posting_numbers": numbers}, nil
		},
	})

	r.Add(Spec{
		Name:   "ozon_finance_accrual_types",
		Path:   ozon.PathFinanceAccrualTypes,
		Desc:   "Справочник видов начислений — расшифровка кодов type_id из финансовых сводов и отчётов.",
		Schema: schema(obj{}),
	})
}

const dayLayout = "2006-01-02"

// maxPostingsPerAccrualRequest — граница, которую Ozon проверяет сам,
// но объясняет плохо.
const maxPostingsPerAccrualRequest = 200

// summaryPeriod разбирает и проверяет границы периода.
func summaryPeriod(a map[string]any) (time.Time, time.Time, error) {
	fromStr, _ := a["date_from"].(string)
	toStr, _ := a["date_to"].(string)

	from, err := time.Parse(dayLayout, fromStr)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("date_from=%q: ожидается формат YYYY-MM-DD", fromStr)
	}
	to, err := time.Parse(dayLayout, toStr)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("date_to=%q: ожидается формат YYYY-MM-DD", toStr)
	}
	if to.Before(from) {
		return time.Time{}, time.Time{}, fmt.Errorf(
			"date_to (%s) раньше date_from (%s) — даты перепутаны местами", toStr, fromStr)
	}
	if days := int(to.Sub(from).Hours()/24) + 1; days > maxSummaryDays {
		return time.Time{}, time.Time{}, fmt.Errorf(
			"период %s…%s — это %d дней, а свод собирается не больше чем за %d: "+
				"начисления Ozon отдаёт по одному дню за запрос, и год превратился бы в 365 запросов. "+
				"Разбейте по месяцам",
			fromStr, toStr, days, maxSummaryDays)
	}
	return from, to, nil
}

// budgetDeadline выбирает момент, после которого обход дней
// прекращается: свой бюджет или дедлайн запроса, что раньше.
func budgetDeadline(ctx context.Context) time.Time {
	own := time.Now().Add(summaryBudget)
	if deadline, ok := ctx.Deadline(); ok {
		if reserved := deadline.Add(-budgetReserve); reserved.Before(own) {
			return reserved
		}
	}
	return own
}

// fetchDay забирает все начисления за день, проходя постранично.
//
// Курсор проверяется на движение: Ozon отдаёт last_id и на последней
// странице тоже, и доверчивый цикл по «пока курсор непустой» крутится
// вечно.
func fetchDay(ctx context.Context, client *ozon.Client, day string) ([]map[string]any, bool, error) {
	var (
		all    []map[string]any
		cursor string
		seen   = map[string]bool{}
	)

	for page := 0; page < maxPagesPerDay; page++ {
		body := map[string]any{"date": day}
		if cursor != "" {
			body["last_id"] = cursor
		}

		raw, err := client.Call(ctx, ozon.PathFinanceAccrualByDay, body)
		if err != nil {
			return nil, false, err
		}

		var doc struct {
			Accruals []map[string]any `json:"accruals"`
			LastID   string           `json:"last_id"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, false, fmt.Errorf("разбор начислений за %s: %w", day, err)
		}

		all = append(all, doc.Accruals...)

		if doc.LastID == "" || len(doc.Accruals) == 0 || seen[doc.LastID] {
			return all, false, nil
		}
		seen[doc.LastID] = true
		cursor = doc.LastID
	}

	// Страницы кончились не потому, что данные кончились. Молчать об
	// этом нельзя: неполные суммы под видом полных хуже отказа.
	return all, true, nil
}

// --- Свод ---

// bucket — накопитель сумм по одному разрезу.
type bucket struct {
	credit float64 // начислено, суммы со знаком плюс
	debit  float64 // удержано, суммы со знаком минус
	count  int
}

func (b *bucket) add(amount float64) {
	if amount >= 0 {
		b.credit += amount
	} else {
		b.debit += amount
	}
	b.count++
}

func (b bucket) net() float64 { return b.credit + b.debit }

// summary — свод начислений за период.
type summary struct {
	from, to time.Time

	total      bucket
	byDay      []dayBucket
	byCategory map[string]*bucket
	byType     map[int]*bucket

	days int

	// partial — дни, у которых обход страниц упёрся в предел.
	partial []string

	// next — день, с которого продолжать, если период пройден
	// не до конца.
	next time.Time
	// failure — ошибка, оборвавшая обход. Пустой свод её не
	// переживает: она возвращается вызывающему как есть.
	failure error
}

type dayBucket struct {
	date string
	b    bucket
}

func newSummary(from, to time.Time) *summary {
	return &summary{
		from:       from,
		to:         to,
		byCategory: map[string]*bucket{},
		byType:     map[int]*bucket{},
	}
}

// addDay складывает начисления одного дня во все разрезы свода.
func (s *summary) addDay(date string, accruals []map[string]any, partial bool) {
	s.days++
	if partial {
		s.partial = append(s.partial, date)
	}
	day := dayBucket{date: date}

	for _, a := range accruals {
		amount, ok := amountOf(a["total_amount"])
		if ok {
			s.total.add(amount)
			day.b.add(amount)

			category, _ := a["accrued_category"].(string)
			if category == "" {
				category = "БЕЗ КАТЕГОРИИ"
			}
			s.categoryBucket(category).add(amount)
		}

		// Виды начислений разбросаны по разным веткам ответа — у
		// отправления они лежат в услугах доставки, у товарных сборов
		// в item_fees, у прочих прямо в non_item_fee. Перечислять
		// ветки поимённо значит терять новые, которые Ozon добавит
		// завтра, поэтому ищем по форме: объект с type_id и accrued.
		walkServices(a, func(typeID int, amount float64) {
			s.typeBucket(typeID).add(amount)
		})
	}

	s.byDay = append(s.byDay, day)
}

func (s *summary) categoryBucket(name string) *bucket {
	b, ok := s.byCategory[name]
	if !ok {
		b = &bucket{}
		s.byCategory[name] = b
	}
	return b
}

func (s *summary) typeBucket(id int) *bucket {
	b, ok := s.byType[id]
	if !ok {
		b = &bucket{}
		s.byType[id] = b
	}
	return b
}

// render печатает свод текстом.
//
// Текстом, а не JSON: у таблицы сумм нет вложенности, ради которой
// стоило бы платить скобками и кавычками, а читать её будут и модель,
// и человек через её плечо.
func (s *summary) render(byDay bool) string {
	var b strings.Builder

	covered := s.to
	if !s.next.IsZero() {
		covered = s.next.AddDate(0, 0, -1)
	}

	if s.from.Equal(s.to) {
		fmt.Fprintf(&b, "Начисления за %s — %d шт.\n", s.from.Format(dayLayout), s.total.count)
	} else {
		fmt.Fprintf(&b, "Начисления %s…%s — %d дн., %d шт.\n",
			s.from.Format(dayLayout), covered.Format(dayLayout), s.days, s.total.count)
	}
	b.WriteString("Это движение денег по начислениям Ozon, а не валовые продажи: " +
		"плюс — начислено вам, минус — удержано.\n\n")

	fmt.Fprintf(&b, "Итого: начислено %s, удержано %s, чистыми %s RUB\n",
		rub(s.total.credit), rub(s.total.debit), rub(s.total.net()))

	if s.total.count == 0 {
		b.WriteString("\nЗа этот период начислений нет. Если ждали их — проверьте даты: " +
			"начисления появляются не в день заказа, а в день расчёта.\n")
	}

	if len(s.byCategory) > 0 {
		b.WriteString("\nПо категориям (начислено / удержано / чистыми / шт.):\n")
		for _, name := range sortedKeys(s.byCategory) {
			c := s.byCategory[name]
			fmt.Fprintf(&b, "  %-14s %12s %12s %12s %6d\n",
				name, rub(c.credit), rub(c.debit), rub(c.net()), c.count)
		}
	}

	if len(s.byType) > 0 {
		b.WriteString("\nПо видам начислений, type_id (чистыми / шт.):\n")
		for _, id := range sortedTypeIDs(s.byType) {
			t := s.byType[id]
			fmt.Fprintf(&b, "  %-6d %12s %6d\n", id, rub(t.net()), t.count)
		}
		b.WriteString("  расшифровка кодов: ozon_finance_accrual_types\n")
	}

	if byDay && len(s.byDay) > 1 {
		b.WriteString("\nПо дням (начислено / удержано / чистыми / шт.):\n")
		for _, d := range s.byDay {
			fmt.Fprintf(&b, "  %s %12s %12s %12s %6d\n",
				d.date, rub(d.b.credit), rub(d.b.debit), rub(d.b.net()), d.b.count)
		}
	}

	if len(s.partial) > 0 {
		fmt.Fprintf(&b, "\nВнимание: за %s начислений больше, чем сервер обошёл за %d страниц — "+
			"суммы за эти дни неполные. Разберите их отдельно: ozon_finance_by_day с detail: true.\n",
			strings.Join(s.partial, ", "), maxPagesPerDay)
	}

	if !s.next.IsZero() {
		b.WriteString("\n")
		if s.failure != nil {
			fmt.Fprintf(&b, "Обход прервался на %s: %v\n", s.next.Format(dayLayout), s.failure)
		} else {
			fmt.Fprintf(&b, "Период пройден не до конца: начисления Ozon отдаёт по одному дню за запрос, "+
				"и остаток не уместился в отведённое время.\n")
		}
		fmt.Fprintf(&b, "Продолжите вызовом ozon_finance_summary с date_from=%s, date_to=%s "+
			"и сложите итоги с этими.\n",
			s.next.Format(dayLayout), s.to.Format(dayLayout))
	}

	return b.String()
}

// --- Разбор сумм ---

// amountOf достаёт число из денежного объекта Ozon {amount, currency}.
// Сумма приезжает строкой («-110», «-3.13»), потому что деньги в JSON
// числом с плавающей точкой — способ потерять копейки.
func amountOf(v any) (float64, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return 0, false
	}
	switch raw := m["amount"].(type) {
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	case float64:
		return raw, true
	}
	return 0, false
}

// walkServices обходит начисление и вызывает fn на каждом объекте вида
// {type_id, accrued: {amount}} — на любой глубине.
func walkServices(v any, fn func(typeID int, amount float64)) {
	switch node := v.(type) {
	case map[string]any:
		if id, ok := toInt(node["type_id"]); ok {
			if amount, ok := amountOf(node["accrued"]); ok {
				fn(id, amount)
			}
		}
		for _, child := range node {
			walkServices(child, fn)
		}
	case []any:
		for _, child := range node {
			walkServices(child, fn)
		}
	}
}

// rub печатает сумму с двумя знаками и без разделителей разрядов:
// пробел внутри числа модель иногда читает как границу колонки.
func rub(v float64) string {
	return strconv.FormatFloat(v, 'f', 2, 64)
}

func sortedKeys(m map[string]*bucket) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortedTypeIDs ставит вперёд виды с наибольшими суммами по модулю:
// в длинном списке кодов сверху должно быть то, что двигает деньги.
func sortedTypeIDs(m map[int]*bucket) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := abs(m[out[i]].net()), abs(m[out[j]].net())
		if a == b {
			return out[i] < out[j]
		}
		return a > b
	})
	return out
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// --- Аргументы ---

func decodeArgs(raw json.RawMessage) (map[string]any, error) {
	args := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, fmt.Errorf("не разобрать аргументы: %w", err)
		}
	}
	return args, nil
}

// argBool читает булев аргумент, не придираясь к форме: модели
// присылают и true, и "true".
func argBool(v any, def bool) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(b))
		if err != nil {
			return def
		}
		return parsed
	}
	return def
}

// mustDay разбирает дату, уже прошедшую проверку checkDay.
func mustDay(s string) time.Time {
	t, _ := time.Parse(dayLayout, s)
	return t
}
