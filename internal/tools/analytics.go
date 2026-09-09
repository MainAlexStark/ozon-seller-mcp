package tools

import (
	"fmt"
	"time"

	"github.com/MainAlexStark/ozon-seller-mcp/ozon"
)

// RegisterAnalytics добавляет аналитику, финансы и отправления.
func (r *Registry) RegisterAnalytics() {
	r.Add(Spec{
		Name: "ozon_analytics_data",
		Path: ozon.PathAnalyticsData,
		Desc: "Аналитика продаж: метрики (выручка, заказы, показы, конверсия) с группировкой по товару и дате. " +
			"Основной инструмент вопросов вида «что продавалось в прошлом месяце» и «какие карточки не показываются».",
		Schema: schema(obj{
			"date_from": str("Начало периода, YYYY-MM-DD"),
			"date_to":   str("Конец периода, YYYY-MM-DD"),
			"metrics": arr(obj{"type": "string"},
				"Метрики: revenue, ordered_units, hits_view, hits_tocart, conv_tocart, returns, cancellations, position_category"),
			"dimension": arr(obj{"type": "string"}, "Группировка: sku, day, week, month, category1..category4"),
			"filters":   arr(obj{"type": "object"}, "Дополнительные фильтры"),
			"sort":      arr(obj{"type": "object"}, "Сортировка: [{key, order}]"),
			"limit":     obj{"type": "integer", "default": 100, "maximum": 1000},
			"offset":    num("Смещение"),
		}, "date_from", "date_to", "metrics", "dimension"),
		Build: func(a map[string]any) (any, error) {
			if _, ok := a["limit"]; !ok {
				a["limit"] = 100
			}
			return a, checkPeriod(a, 0)
		},
	})

	r.Add(Spec{
		Name: "ozon_analytics_stocks",
		Path: ozon.PathAnalyticsStocks,
		Desc: "Остатки на складах в реальном времени с аналитикой оборачиваемости: сколько дней хватит запаса, " +
			"что заканчивается. Свежее, чем ozon_stocks_info.",
		Schema: schema(obj{
			"skus":            arr(obj{"type": "integer"}, "Список SKU; пусто — все товары"),
			"warehouse_ids":   arr(obj{"type": "integer"}, "Фильтр по складам"),
			"item_tags":       arr(obj{"type": "string"}, "Фильтр по тегам товара"),
			"turnover_grades": arr(obj{"type": "string"}, "Фильтр по оборачиваемости"),
		}),
	})

	// --- Финансы ---
	//
	// /v3/finance/transaction/list отключён в 2026 году, и замена
	// устроена иначе, чем он: не «выгрузка за период», а два узких
	// метода — начисления за один день и начисления по конкретным
	// отправлениям. Просить у них диапазон дат бессмысленно: такого
	// параметра там просто нет.

	r.Add(Spec{
		Name: "ozon_finance_by_day",
		Path: ozon.PathFinanceAccrualByDay,
		Desc: "Начисления за один день: выплаты, комиссии, удержания. " +
			"Именно за день, а не за период: чтобы собрать месяц, метод вызывают по дням. " +
			"Без даты берётся вчерашний день — сегодняшние начисления ещё неполные.",
		Schema: schema(obj{
			"date":    str("День, YYYY-MM-DD"),
			"last_id": str("Курсор постраничного обхода из предыдущего ответа"),
		}),
		Build: func(a map[string]any) (any, error) {
			day, _ := a["date"].(string)
			if day == "" {
				day = time.Now().AddDate(0, 0, -1).Format("2006-01-02")
			}
			if err := checkDay(day); err != nil {
				return nil, err
			}

			// Тело собираем заново, а не дополняем аргументы: у метода
			// ровно два поля, и лишнее в нём — повод для отказа,
			// а не для снисходительности.
			out := map[string]any{"date": day}
			if cursor, _ := a["last_id"].(string); cursor != "" {
				out["last_id"] = cursor
			}
			return out, nil
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
		Desc:   "Справочник видов начислений — расшифровка кодов из финансовых отчётов.",
		Schema: schema(obj{}),
	})

	// --- Отправления ---

	r.Add(Spec{
		Name: "ozon_postings_list",
		Path: ozon.PathPostingFBSList,
		Desc: "Отправления FBS за период: новые заказы, статусы, дедлайны отгрузки. " +
			"Вход для планирования того, что нужно произвести и отгрузить. " +
			"Если период не задан, берутся последние 7 дней.",
		Schema: schema(obj{
			"filter": schema(obj{
				"since":  str("Начало периода, RFC3339"),
				"to":     str("Конец периода, RFC3339"),
				"status": str("Статус отправления, например awaiting_packaging"),
			}),
			"limit":  obj{"type": "integer", "default": 50, "maximum": 1000},
			"offset": num("Смещение"),
			"with":   obj{"type": "object", "description": "Что включить в ответ: analytics_data, financial_data"},
		}),
		Build: func(a map[string]any) (any, error) {
			// Период обязателен: без него Ozon отвечает «processed_at_to
			// must be set», а спрашивают обычно просто «что там с
			// заказами» — без дат вообще.
			return withLimit(withRecentPeriod(a, 7), 50), nil
		},
	})

	r.Add(Spec{
		Name: "ozon_posting_get",
		Path: ozon.PathPostingFBSGet,
		Desc: "Подробности одного отправления по номеру.",
		Schema: schema(obj{
			"posting_number": str("Номер отправления"),
			"with":           obj{"type": "object", "description": "analytics_data, financial_data, product_exemplars"},
		}, "posting_number"),
	})

	r.Add(Spec{
		Name: "ozon_reviews_list",
		Path: ozon.PathReviewList,
		Desc: "Отзывы на товары. Полезно, чтобы понять, что покупатели пишут про размер, качество печати и упаковку. " +
			"Меньше 20 отзывов за раз Ozon не отдаёт, поэтому меньший limit поднимается до 20.",
		Schema: schema(obj{
			"limit":    obj{"type": "integer", "default": minReviewsLimit, "minimum": minReviewsLimit, "maximum": maxReviewsLimit},
			"status":   obj{"type": "string", "enum": []string{"ALL", "UNPROCESSED", "PROCESSED"}},
			"sort_dir": obj{"type": "string", "enum": []string{"ASC", "DESC"}},
			"last_id":  str("Курсор постраничного обхода"),
		}),
		Build: func(a map[string]any) (any, error) {
			// limit обязателен и принимается только в диапазоне
			// [20, 100]. Просьба «покажи пару отзывов» выглядит
			// естественно и приводила к отказу — приводим к границе
			// вместо ошибки: лишние отзывы не мешают, отказ мешает.
			return clampLimit(a, minReviewsLimit, maxReviewsLimit), nil
		},
	})
}

// Границы, которые Ozon проверяет сам, но объясняет плохо.
const (
	minReviewsLimit = 20
	maxReviewsLimit = 100

	maxPostingsPerAccrualRequest = 200
)

// checkDay проверяет формат дня до обращения к API: Ozon отвечает на
// неверный формат сообщением про «10 runes», по которому не догадаться,
// что речь про YYYY-MM-DD.
func checkDay(v any) error {
	day, _ := v.(string)
	if _, err := time.Parse("2006-01-02", day); err != nil {
		return fmt.Errorf("date=%q: ожидается один день в формате YYYY-MM-DD", day)
	}
	return nil
}

// checkPeriod проверяет даты периода до обращения к API.
//
// maxDays == 0 означает «длину не ограничиваем», но формат всё равно
// проверяется: перепутанные местами даты — самая частая причина
// пустого ответа, и её лучше поймать здесь.
func checkPeriod(a map[string]any, maxDays int) error {
	fromStr, _ := a["date_from"].(string)
	toStr, _ := a["date_to"].(string)
	if fromStr == "" || toStr == "" {
		return nil
	}

	const layout = "2006-01-02"
	from, err := time.Parse(layout, fromStr)
	if err != nil {
		return fmt.Errorf("date_from=%q: ожидается формат YYYY-MM-DD", fromStr)
	}
	to, err := time.Parse(layout, toStr)
	if err != nil {
		return fmt.Errorf("date_to=%q: ожидается формат YYYY-MM-DD", toStr)
	}

	if to.Before(from) {
		return fmt.Errorf("date_to (%s) раньше date_from (%s) — даты перепутаны местами", toStr, fromStr)
	}

	if maxDays > 0 {
		if days := int(to.Sub(from).Hours() / 24); days > maxDays {
			return fmt.Errorf(
				"период %s…%s — это %d дней, а метод принимает не больше %d. "+
					"Разбейте запрос по месяцам и сложите результаты",
				fromStr, toStr, days, maxDays)
		}
	}
	return nil
}
