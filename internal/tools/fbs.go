package tools

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/MainAlexStark/ozon-seller-mcp/ozon"
)

// RegisterFBS добавляет рабочий день продавца по схеме FBS: что собрать,
// собрать, напечатать этикетки, сформировать акт и отгрузить.
//
// Порядок в Ozon жёсткий, и почти каждая ошибка здесь — это шаг не
// в том порядке:
//
//  1. ozon_fbs_unfulfilled_list — что ждёт сборки и до какого времени;
//  2. ozon_fbs_ship — собрать (awaiting_packaging → awaiting_deliver);
//  3. ozon_fbs_labels — этикетки; готовы через 45–60 секунд после сборки;
//  4. ozon_fbs_act_create — акт и накладная на отгрузку;
//  5. ozon_fbs_act_documents — PDF акта, накладной, штрихкод для пропуска.
//
// Списки и подробности отправлений (ozon_postings_list,
// ozon_posting_get) остались в analytics.go под прежними именами: на них
// уже опираются чужие сценарии, а переименование ради порядка в коде
// сломало бы их молча.
func (r *Registry) RegisterFBS() {
	r.registerFBSRead()
	r.registerFBSDocuments()
	r.registerFBSWrite()
}

// --- Чтение ---

func (r *Registry) registerFBSRead() {
	r.Add(Spec{
		Name: "ozon_fbs_unfulfilled_list",
		Path: ozon.PathPostingFBSUnfulfilled,
		Desc: "Необработанные отправления FBS — главный список дня: что нужно собрать и отгрузить и к какому сроку " +
			"(shipment_date — крайний срок сборки). Если сроки не заданы, берётся окно ±14 дней от сейчас, " +
			"так что просроченные тоже попадут. Для сборки фильтруйте status=awaiting_packaging, " +
			"для отгрузки — awaiting_deliver.",
		Schema: schema(obj{
			"status": obj{"type": "string", "enum": fbsUnfulfilledStatuses,
				"description": "Короткий способ задать filter.status"},
			"filter": schema(obj{
				"cutoff_from":          str("Крайний срок сборки: от, RFC3339"),
				"cutoff_to":            str("Крайний срок сборки: до, RFC3339"),
				"delivering_date_from": str("Дата передачи в доставку: от, RFC3339"),
				"delivering_date_to":   str("Дата передачи в доставку: до, RFC3339"),
				"status":               obj{"type": "string", "enum": fbsUnfulfilledStatuses},
				"delivery_method_id":   arr(obj{"type": "integer"}, "Методы доставки"),
				"warehouse_id":         arr(obj{"type": "integer"}, "Склады"),
			}),
			"dir":    obj{"type": "string", "enum": []string{"ASC", "DESC"}},
			"limit":  obj{"type": "integer", "default": 100, "maximum": 1000},
			"offset": num("Смещение"),
			"with":   obj{"type": "object", "description": "Что добавить: analytics_data, barcodes, financial_data, translit"},
		}),
		Build: func(a map[string]any) (any, error) {
			return capLimit(withLimit(withUnfulfilledWindow(a, 14), 100), 1000), nil
		},
	})

	r.Add(Spec{
		Name: "ozon_fbs_posting_by_barcode",
		Path: ozon.PathPostingFBSByBarcode,
		Desc: "Найти отправление FBS по штрихкоду с этикетки — когда в руках коробка, а номера нет.",
		Schema: schema(obj{
			"barcode": str("Штрихкод отправления с этикетки"),
		}, "barcode"),
	})

	r.Add(Spec{
		Name: "ozon_fbs_restrictions",
		Path: ozon.PathPostingFBSRestrictions,
		Desc: "Ограничения пункта приёма для отправления: максимальный вес, габариты и объявленная стоимость. " +
			"Проверяйте до сборки крупного заказа: не прошедшую по габаритам коробку не примут.",
		Schema: schema(obj{
			"posting_number": str("Номер отправления"),
		}, "posting_number"),
	})

	r.Add(Spec{
		Name: "ozon_fbs_cancel_reasons",
		Desc: "Причины отмены отправлений FBS. С posting_numbers — причины, допустимые именно для этих отправлений " +
			"(часть причин доступна не на любом статусе); без них — общий справочник. " +
			"cancel_reason_id отсюда нужен ozon_fbs_cancel.",
		Schema: schema(obj{
			"posting_numbers": arr(obj{"type": "string"}, "Номера отправлений; пусто — общий справочник"),
		}),
		Do: func(ctx context.Context, c *ozon.Client, payload any) (any, error) {
			a, _ := payload.(map[string]any)
			if nums, err := stringList(a["posting_numbers"]); err == nil {
				return c.Call(ctx, ozon.PathPostingFBSCancelReasonsFor, obj{"related_posting_numbers": nums})
			}
			return c.Call(ctx, ozon.PathPostingFBSCancelReasons, obj{})
		},
	})

	// --- Отгрузки (акты) ---

	r.Add(Spec{
		Name: "ozon_fbs_acts_list",
		Path: ozon.PathFBSActList,
		Desc: "Отгрузки FBS (акты приёма-передачи) за период: номер, метод доставки, статус, число отправлений. " +
			"Без дат — последние 14 дней. id отсюда передаётся в ozon_fbs_act_status, ozon_fbs_act_postings " +
			"и ozon_fbs_act_documents.",
		Schema: schema(obj{
			"filter": schema(obj{
				"date_from": str("Начало периода, YYYY-MM-DD"),
				"date_to":   str("Конец периода, YYYY-MM-DD"),
				"integration_type": obj{"type": "string",
					"enum":        []string{"ozon", "3pl_tracking", "non_integrated", "aggregator"},
					"description": "Кто доставляет: ozon — Ozon; non_integrated — сторонняя служба"},
				"status": arr(obj{"type": "string", "enum": fbsActStatuses}, "Статусы отгрузки"),
			}),
			"limit": obj{"type": "integer", "default": 50, "maximum": 1000},
		}),
		Build: func(a map[string]any) (any, error) {
			return withLimit(withActPeriod(a, 14), 50), nil
		},
	})

	r.Add(Spec{
		Name: "ozon_fbs_act_status",
		Path: ozon.PathFBSActStatus,
		Desc: "Готовы ли документы отгрузки. Акт формируется не мгновенно: после ozon_fbs_act_create ждите " +
			"статуса FORMED (или CONFIRMED*) и только потом запрашивайте PDF.",
		Schema: schema(obj{
			"id": num("Идентификатор отгрузки из ozon_fbs_act_create или ozon_fbs_acts_list"),
		}, "id"),
	})

	r.Add(Spec{
		Name: "ozon_fbs_act_postings",
		Path: ozon.PathFBSActPostings,
		Desc: "Какие отправления вошли в отгрузку. Проверка перед поездкой: всё ли собранное попало в акт.",
		Schema: schema(obj{
			"id": num("Идентификатор отгрузки"),
		}, "id"),
	})

	r.Add(Spec{
		Name: "ozon_fbs_carriages_available",
		Path: ozon.PathCarriageAvailable,
		Desc: "Отгрузки, доступные к передаче сегодня (или в указанный день): сколько отправлений ждёт, " +
			"есть ли уже акт. delivery_method_id берётся из отправления (delivery_method.id).",
		Schema: schema(obj{
			"delivery_method_id": num("Метод доставки; пусто — все"),
			"departure_date":     str("День отгрузки, YYYY-MM-DD или RFC3339; пусто — сегодня"),
		}),
		Build: func(a map[string]any) (any, error) {
			if v, ok := a["departure_date"].(string); ok && v != "" {
				t, err := parseDayOrTime(v)
				if err != nil {
					return nil, fmt.Errorf("departure_date: %w", err)
				}
				a["departure_date"] = t.Format(time.RFC3339)
			}
			return a, nil
		},
	})

	r.Add(Spec{
		Name: "ozon_fbs_carriage_get",
		Path: ozon.PathCarriageGet,
		Desc: "Подробности отгрузки: статус, дата, метод доставки, сколько отправлений и контейнеров.",
		Schema: schema(obj{
			"carriage_id": num("Идентификатор отгрузки"),
		}, "carriage_id"),
	})
}

// fbsUnfulfilledStatuses — статусы необработанных отправлений.
var fbsUnfulfilledStatuses = []string{
	"awaiting_registration",
	"acceptance_in_progress",
	"awaiting_approve",
	"awaiting_packaging",
	"awaiting_deliver",
	"arbitration",
	"client_arbitration",
	"delivering",
	"driver_pickup",
	"not_accepted",
}

// fbsActStatuses — статусы отгрузок в фильтре списка актов.
var fbsActStatuses = []string{
	"new", "awaiting-retry", "in_process", "success", "error",
	"sent", "received", "formed", "cancelled", "pending",
}

// withUnfulfilledWindow достраивает окно по крайнему сроку сборки.
//
// Метод требует хотя бы одну пару дат — или по сроку сборки, или по дате
// передачи в доставку, — а спрашивают его как «что собирать». Окно
// строится вокруг «сейчас», а не от «сейчас» вперёд: просроченный заказ
// и есть самый срочный, и он не должен выпасть из списка из-за того,
// что его срок уже прошёл.
func withUnfulfilledWindow(a map[string]any, days int) map[string]any {
	if a == nil {
		a = map[string]any{}
	}
	filter, _ := a["filter"].(map[string]any)
	if filter == nil {
		filter = map[string]any{}
	}

	if st, ok := a["status"].(string); ok && st != "" {
		if cur, _ := filter["status"].(string); cur == "" {
			filter["status"] = st
		}
	}
	delete(a, "status")

	has := func(k string) bool {
		v, ok := filter[k]
		return ok && v != nil && v != ""
	}
	if !has("cutoff_from") && !has("cutoff_to") &&
		!has("delivering_date_from") && !has("delivering_date_to") {
		now := time.Now().UTC()
		filter["cutoff_from"] = now.AddDate(0, 0, -days).Format(time.RFC3339)
		filter["cutoff_to"] = now.AddDate(0, 0, days).Format(time.RFC3339)
	}

	a["filter"] = filter
	return a
}

// withActPeriod достраивает период списка отгрузок.
func withActPeriod(a map[string]any, days int) map[string]any {
	if a == nil {
		a = map[string]any{}
	}
	filter, _ := a["filter"].(map[string]any)
	if filter == nil {
		filter = map[string]any{}
	}
	from, _ := filter["date_from"].(string)
	to, _ := filter["date_to"].(string)
	now := time.Now()
	if from == "" && to == "" {
		filter["date_from"] = now.AddDate(0, 0, -days).Format(dayLayout)
		filter["date_to"] = now.Format(dayLayout)
	} else if to == "" {
		filter["date_to"] = now.Format(dayLayout)
	} else if from == "" {
		if t, err := time.Parse(dayLayout, to); err == nil {
			filter["date_from"] = t.AddDate(0, 0, -days).Format(dayLayout)
		}
	}
	a["filter"] = filter
	return a
}

// parseDayOrTime принимает и день, и полное время: модель пишет
// «2026-09-28», а API ждёт RFC3339. День понимается как полночь
// по Москве — в этом времени живёт кабинет и сроки отгрузок.
func parseDayOrTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation(dayLayout, s, moscow()); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("%q: ожидается YYYY-MM-DD или RFC3339", s)
}

// moscow — часовой пояс кабинета. Если базы поясов в системе нет
// (distroless без tzdata), берётся фиксированное смещение: в Москве
// нет перехода на летнее время, так что это не приближение.
func moscow() *time.Location {
	if loc, err := time.LoadLocation("Europe/Moscow"); err == nil {
		return loc
	}
	return time.FixedZone("MSK", 3*60*60)
}

// --- Документы ---

// maxSyncLabels — сколько отправлений принимает синхронный метод
// этикеток. Больше — только через задание.
const maxSyncLabels = 20

// maxLabelPostings — потолок одного вызова ozon_fbs_labels. Не лимит
// Ozon, а здравый смысл: пачка в тысячу этикеток — это уже выгрузка,
// и ждать её в одном вызове инструмента не стоит.
const maxLabelPostings = 500

// labelTaskWait — сколько ждать асинхронные этикетки внутри вызова.
const labelTaskWait = 90 * time.Second

func (r *Registry) registerFBSDocuments() {
	r.Add(Spec{
		Name: "ozon_fbs_labels",
		Desc: "Этикетки (маркировка) для собранных отправлений FBS — PDF для печати. " +
			"Сервер сохраняет файл и возвращает ссылку или путь, а не содержимое. " +
			"До 20 отправлений — один файл сразу; больше — задание Ozon, файл по ссылке Ozon. " +
			"Этикетка появляется через 45–60 секунд после сборки: «not ready» сразу после ozon_fbs_ship — норма, повторите.",
		Schema: schema(obj{
			"posting_numbers": arr(obj{"type": "string"}, "Номера собранных отправлений (статус awaiting_deliver)"),
		}, "posting_numbers"),
		Build: func(a map[string]any) (any, error) {
			nums, err := stringList(a["posting_numbers"])
			if err != nil {
				return nil, fmt.Errorf("posting_numbers: %w", err)
			}
			nums = uniqueStrings(nums)
			if len(nums) > maxLabelPostings {
				return nil, fmt.Errorf("за один вызов — не больше %d отправлений, передано %d", maxLabelPostings, len(nums))
			}
			return nums, nil
		},
		Do: func(ctx context.Context, c *ozon.Client, payload any) (any, error) {
			nums := payload.([]string)
			if len(nums) <= maxSyncLabels {
				f, err := c.CallFile(ctx, ozon.PathPostingFBSLabel, obj{"posting_number": nums})
				if err != nil {
					return nil, err
				}
				return r.saveFile(f, labelsName(nums), fmt.Sprintf("Этикетки: %d отправлений", len(nums)))
			}
			return labelsViaTask(ctx, c, nums)
		},
		Hint: labelsNotReadyHint,
	})

	r.Add(Spec{
		Name: "ozon_fbs_act_documents",
		Desc: "Документы отгрузки FBS файлом: акт приёма-передачи и накладная, электронные акты, " +
			"этикетки грузовых мест, штрихкод для пропуска на склад. Файл сохраняется, в ответе — ссылка или путь. " +
			"Сначала дождитесь готовности в ozon_fbs_act_status.",
		Schema: schema(obj{
			"id": num("Идентификатор отгрузки из ozon_fbs_act_create или ozon_fbs_acts_list"),
			"doc": obj{"type": "string", "enum": fbsActDocs, "default": "act",
				"description": "act — акт и накладная (при ЭДО — только накладная); " +
					"act_of_acceptance / act_of_mismatch / act_of_excess — электронные акты приёмки, расхождений, излишков; " +
					"container_labels — этикетки грузовых мест; barcode — штрихкод отгрузки картинкой и текстом"},
		}, "id"),
		Build: func(a map[string]any) (any, error) {
			id, ok := toInt(a["id"])
			if !ok || id <= 0 {
				return nil, fmt.Errorf("id: нужен идентификатор отгрузки")
			}
			doc, _ := a["doc"].(string)
			if doc == "" {
				doc = "act"
			}
			if !slices.Contains(fbsActDocs, doc) {
				return nil, fmt.Errorf("doc=%q: допустимо %s", doc, strings.Join(fbsActDocs, ", "))
			}
			return actDocRequest{ID: int64(id), Doc: doc}, nil
		},
		Do: func(ctx context.Context, c *ozon.Client, payload any) (any, error) {
			return r.actDocument(ctx, c, payload.(actDocRequest))
		},
		Hint: actNotReadyHint,
	})
}

// fbsActDocs — какие документы отгрузки умеет отдавать сервер.
var fbsActDocs = []string{
	"act", "act_of_acceptance", "act_of_mismatch", "act_of_excess", "container_labels", "barcode",
}

type actDocRequest struct {
	ID  int64
	Doc string
}

// actDocument достаёт документ отгрузки и сохраняет его.
func (r *Registry) actDocument(ctx context.Context, c *ozon.Client, req actDocRequest) (string, error) {
	body := obj{"id": req.ID}
	switch req.Doc {
	case "act":
		f, err := c.CallFile(ctx, ozon.PathFBSActPDF, body)
		if err != nil {
			return "", err
		}
		return r.saveFile(f, fmt.Sprintf("act_%d.pdf", req.ID), fmt.Sprintf("Акт и накладная отгрузки %d", req.ID))

	case "container_labels":
		f, err := c.CallFile(ctx, ozon.PathFBSActContainerLabels, body)
		if err != nil {
			return "", err
		}
		return r.saveFile(f, fmt.Sprintf("containers_%d.pdf", req.ID), fmt.Sprintf("Этикетки грузовых мест отгрузки %d", req.ID))

	case "barcode":
		// Текстовое значение — то, что можно продиктовать или вбить
		// руками, если картинку не отсканировать. Его достаём первым:
		// без него картинка бесполезна при плохом принтере.
		var text struct {
			Result string `json:"result"`
		}
		textErr := c.CallInto(ctx, ozon.PathFBSActBarcodeText, body, &text)

		f, err := c.CallFile(ctx, ozon.PathFBSActBarcode, body)
		if err != nil {
			if textErr == nil && text.Result != "" {
				return fmt.Sprintf("Штрихкод отгрузки %d: %s\n(картинку получить не удалось: %v)", req.ID, text.Result, err), nil
			}
			return "", err
		}
		out, err := r.saveFile(f, fmt.Sprintf("barcode_%d.png", req.ID), fmt.Sprintf("Штрихкод отгрузки %d", req.ID))
		if err != nil {
			return "", err
		}
		if textErr == nil && text.Result != "" {
			out += "\nЗначение штрихкода: " + text.Result
		}
		return out, nil

	default: // электронные акты
		body["doc_type"] = req.Doc
		f, err := c.CallFile(ctx, ozon.PathFBSDigitalActPDF, body)
		if err != nil {
			return "", err
		}
		return r.saveFile(f, fmt.Sprintf("%s_%d.pdf", req.Doc, req.ID), fmt.Sprintf("Электронный акт %s отгрузки %d", req.Doc, req.ID))
	}
}

// ErrNoFileStore — сохранять файлы некуда.
var ErrNoFileStore = errors.New("сервер не настроен сохранять файлы: " +
	"локально задайте OZON_FILES_DIR (или оставьте по умолчанию «Загрузки/Ozon»), " +
	"в сервисе хранилище поднимается само — сообщите администратору")

// saveFile кладёт файл в хранилище и описывает, где его взять.
func (r *Registry) saveFile(f ozon.File, fallbackName, what string) (string, error) {
	if r.files == nil {
		return "", ErrNoFileStore
	}
	name := f.Name
	if name == "" {
		name = fallbackName
	}
	ct := f.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	saved, err := r.files.Save(name, ct, f.Data)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s — готово.\n", what)
	fmt.Fprintf(&b, "Файл: %s\n", saved.Location)
	fmt.Fprintf(&b, "Размер: %s, тип: %s\n", humanBytes(saved.Size), ct)
	if !saved.Expires.IsZero() {
		fmt.Fprintf(&b, "Ссылка действует до %s (МСК). Покажите её человеку как есть — "+
			"открыть и распечатать может только он.\n", saved.Expires.In(moscow()).Format("02.01.2006 15:04"))
	} else {
		b.WriteString("Файл сохранён на этом компьютере — назовите человеку путь, чтобы он открыл и распечатал.\n")
	}
	return b.String(), nil
}

// labelsViaTask заказывает этикетки заданием и ждёт готовый файл.
//
// Файл в этом случае не скачивается: Ozon отдаёт ссылку на свой
// сервер, и она доступна человеку напрямую. Тащить сотни этикеток
// через наш процесс ради того, чтобы выдать другую ссылку на тот же
// файл, незачем.
func labelsViaTask(ctx context.Context, c *ozon.Client, nums []string) (string, error) {
	var created struct {
		Result struct {
			Tasks []struct {
				TaskID   int64  `json:"task_id"`
				TaskType string `json:"task_type"`
			} `json:"tasks"`
			TaskID int64 `json:"task_id"`
		} `json:"result"`
	}
	if err := c.CallInto(ctx, ozon.PathPostingFBSLabelCreate, obj{"posting_number": nums}, &created); err != nil {
		return "", err
	}

	type task struct {
		id  int64
		typ string
	}
	var tasks []task
	for _, t := range created.Result.Tasks {
		tasks = append(tasks, task{t.TaskID, t.TaskType})
	}
	if len(tasks) == 0 && created.Result.TaskID != 0 {
		tasks = append(tasks, task{created.Result.TaskID, "big_label"})
	}
	if len(tasks) == 0 {
		return "", fmt.Errorf("Ozon принял задание на этикетки, но не вернул его идентификатор")
	}

	deadline := time.Now().Add(labelTaskWait)
	var b strings.Builder
	fmt.Fprintf(&b, "Этикетки для %d отправлений (через задание Ozon):\n", len(nums))

	for _, t := range tasks {
		label := labelTypeName(t.typ)
		for {
			var got struct {
				Result struct {
					Status  string `json:"status"`
					FileURL string `json:"file_url"`
					Error   string `json:"error"`
				} `json:"result"`
			}
			if err := c.CallInto(ctx, ozon.PathPostingFBSLabelGet, obj{"task_id": t.id}, &got); err != nil {
				return "", err
			}
			res := got.Result
			if res.Status == "completed" && res.FileURL != "" {
				fmt.Fprintf(&b, "  %s: %s\n", label, res.FileURL)
				break
			}
			if res.Status == "error" {
				fmt.Fprintf(&b, "  %s: ошибка формирования — %s\n", label, res.Error)
				break
			}
			if time.Now().After(deadline) {
				fmt.Fprintf(&b, "  %s: ещё формируется (задание %d, статус %s). "+
					"Повторите вызов через минуту — Ozon отдаст уже готовый файл.\n", label, t.id, res.Status)
				break
			}
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(3 * time.Second):
			}
		}
	}
	b.WriteString("Ссылки ведут на сервер Ozon — покажите их человеку как есть, чтобы он открыл и распечатал.")
	return b.String(), nil
}

func labelTypeName(t string) string {
	switch t {
	case "big_label":
		return "обычные этикетки"
	case "small_label":
		return "маленькие этикетки"
	}
	if t == "" {
		return "этикетки"
	}
	return t
}

func labelsName(nums []string) string {
	if len(nums) == 1 {
		return "label_" + nums[0] + ".pdf"
	}
	return fmt.Sprintf("labels_%d.pdf", len(nums))
}

// labelsNotReadyHint объясняет самый частый отказ этикеток.
//
// Этикетка появляется не в момент сборки, а через минуту после неё, и
// всё это время Ozon отвечает ошибкой, которая выглядит как поломка.
// Без подсказки модель начинает менять аргументы или делать вывод, что
// заказ не собран.
func labelsNotReadyHint(_ map[string]any, err error) string {
	var apiErr *ozon.APIError
	if !asAPI(err, &apiErr) {
		return ""
	}
	msg := strings.ToLower(apiErr.Message + " " + apiErr.Code + " " + apiErr.Raw)
	switch {
	case strings.Contains(msg, "not ready") || strings.Contains(msg, "not_ready"):
		return "Этикетка ещё не сформирована: Ozon готовит её 45–60 секунд после сборки.\n" +
			"Ничего менять не нужно — повторите тот же вызов через минуту."
	case strings.Contains(msg, "status") || strings.Contains(msg, "packag"):
		return "Этикетки печатаются только для собранных отправлений (статус awaiting_deliver).\n" +
			"Проверьте статус в ozon_posting_get; несобранное сначала соберите ozon_fbs_ship.\n" +
			"Если в пачке хоть одно отправление не готово, Ozon не отдаёт этикетки ни на одно из них."
	}
	return ""
}

// actNotReadyHint — документы отгрузки запрошены раньше, чем готовы.
func actNotReadyHint(_ map[string]any, err error) string {
	var apiErr *ozon.APIError
	if !asAPI(err, &apiErr) || apiErr.StatusCode == http.StatusForbidden {
		return ""
	}
	msg := strings.ToLower(apiErr.Message + " " + apiErr.Code + " " + apiErr.Raw)
	if strings.Contains(msg, "not ready") || strings.Contains(msg, "not found") ||
		strings.Contains(msg, "in_process") || strings.Contains(msg, "not formed") {
		return "Документы отгрузки формируются не сразу. Проверьте ozon_fbs_act_status и запросите файл,\n" +
			"когда статус станет FORMED (или CONFIRMED). Электронные акты появляются только после приёмки на складе."
	}
	return ""
}

// --- Запись ---

func (r *Registry) registerFBSWrite() {
	r.Add(Spec{
		Name:  "ozon_fbs_ship",
		Write: true,
		Desc: "Собрать отправления FBS: перевести из «ожидает сборки» в «ожидает отгрузки». " +
			"Простой случай — posting_numbers: каждое отправление целиком в одну коробку, состав сервер берёт сам. " +
			"Несколько коробок — posting_number + packages: Ozon разделит отправление на несколько, по одному на коробку, " +
			"и вернёт новые номера — этикетки нужны уже на них. После сборки: ozon_fbs_labels через минуту.",
		Schema: schema(obj{
			"posting_numbers": arr(obj{"type": "string"}, "Отправления, каждое целиком в одну коробку"),
			"posting_number":  str("Одно отправление, если нужна раскладка по коробкам"),
			"packages": arr(schema(obj{
				"products": arr(schema(obj{
					"product_id": num("SKU товара (products[].sku из отправления)"),
					"quantity":   num("Сколько штук в этой коробке"),
				}, "product_id", "quantity"), "Товары в коробке"),
			}, "products"), "Коробки с составом. Вместе с posting_number"),
		}),
		Batch: func(a map[string]any) int {
			if items, ok := a["posting_numbers"].([]any); ok {
				return len(items)
			}
			return 1
		},
		Build: buildShip,
		Do: func(ctx context.Context, c *ozon.Client, payload any) (any, error) {
			return shipPostings(ctx, c, payload.([]shipOrder))
		},
		Hint: shipHint,
	})

	r.Add(Spec{
		Name:  "ozon_fbs_cancel",
		Path:  ozon.PathPostingFBSCancel,
		Write: true,
		Desc: "Отменить отправление FBS по инициативе продавца. Это бьёт по рейтингу магазина (индекс отмен), " +
			"поэтому требует confirm_cancel: true — ставьте только после явного согласия человека. " +
			"cancel_reason_id — из ozon_fbs_cancel_reasons; для причины 402 («другое») обязателен cancel_reason_message.",
		Schema: schema(obj{
			"posting_number":        str("Номер отправления"),
			"cancel_reason_id":      num("Причина из ozon_fbs_cancel_reasons"),
			"cancel_reason_message": str("Пояснение; обязательно для причины 402"),
			"confirm_cancel":        boolean("Человек подтвердил отмену"),
		}, "posting_number", "cancel_reason_id", "confirm_cancel"),
		Build: func(a map[string]any) (any, error) {
			if !argBool(a["confirm_cancel"], false) {
				return nil, fmt.Errorf("отмена отправления остановлена: нужен confirm_cancel: true.\n\n" +
					"Отмена по инициативе продавца учитывается в индексе отмен и снижает рейтинг магазина, " +
					"а покупатель получает уведомление сразу — вернуть заказ нельзя.\n" +
					"Спросите человека и, если он согласен, повторите вызов с confirm_cancel: true.")
			}
			id, ok := toInt(a["cancel_reason_id"])
			if !ok || id <= 0 {
				return nil, fmt.Errorf("cancel_reason_id: нужна причина из ozon_fbs_cancel_reasons")
			}
			msg, _ := a["cancel_reason_message"].(string)
			if id == 402 && strings.TrimSpace(msg) == "" {
				return nil, fmt.Errorf("для причины 402 («другое») Ozon требует cancel_reason_message — опишите причину словами")
			}
			num, _ := a["posting_number"].(string)
			if strings.TrimSpace(num) == "" {
				return nil, fmt.Errorf("posting_number: не задан")
			}
			body := obj{"posting_number": num, "cancel_reason_id": id}
			if msg != "" {
				body["cancel_reason_message"] = msg
			}
			return body, nil
		},
	})

	r.Add(Spec{
		Name:  "ozon_fbs_act_create",
		Path:  ozon.PathFBSActCreate,
		Write: true,
		Desc: "Сформировать отгрузку FBS: акт приёма-передачи и накладную на все собранные отправления метода доставки. " +
			"Возвращает id — по нему ozon_fbs_act_status и, когда готово, ozon_fbs_act_documents. " +
			"delivery_method_id — из отправления (delivery_method.id) или ozon_fbs_carriages_available.",
		Schema: schema(obj{
			"delivery_method_id": num("Метод доставки"),
			"departure_date":     str("День отгрузки, YYYY-MM-DD или RFC3339; пусто — сегодня"),
			"containers_count":   num("Число грузовых мест — только при доверительной приёмке, иначе не указывайте"),
		}, "delivery_method_id"),
		Build: func(a map[string]any) (any, error) {
			id, ok := toInt(a["delivery_method_id"])
			if !ok || id <= 0 {
				return nil, fmt.Errorf("delivery_method_id: нужен метод доставки")
			}
			day := time.Now().In(moscow())
			if v, ok := a["departure_date"].(string); ok && v != "" {
				t, err := parseDayOrTime(v)
				if err != nil {
					return nil, fmt.Errorf("departure_date: %w", err)
				}
				day = t
			}
			body := obj{"delivery_method_id": id, "departure_date": day.Format(time.RFC3339)}
			if n, ok := toInt(a["containers_count"]); ok && n > 0 {
				body["containers_count"] = n
			}
			return body, nil
		},
	})

	r.Add(Spec{
		Name:  "ozon_fbs_tracking_set",
		Path:  ozon.PathFBSTrackingSet,
		Write: true,
		Desc: "Передать трек-номера отправлений, которые вы везёте сторонней службой доставки (не Ozon). " +
			"Для доставки Ozon не нужно — трек там свой.",
		Schema: schema(obj{
			"tracking_numbers": arr(schema(obj{
				"posting_number":  str("Номер отправления"),
				"tracking_number": str("Трек-номер службы доставки"),
			}, "posting_number", "tracking_number"), "Пары «отправление — трек-номер»"),
		}, "tracking_numbers"),
		Batch: func(a map[string]any) int {
			items, _ := a["tracking_numbers"].([]any)
			return len(items)
		},
	})

	r.Add(Spec{
		Name:  "ozon_fbs_status_set",
		Write: true,
		Desc: "Сменить статус отправлений, которые вы доставляете сами или сторонней службой: " +
			"delivering — передано в доставку, last_mile — курьер в пути, delivered — доставлено, " +
			"sent_by_seller — отправлено продавцом. Для доставки Ozon статусы меняет сам Ozon.",
		Schema: schema(obj{
			"posting_numbers": arr(obj{"type": "string"}, "Номера отправлений"),
			"status":          obj{"type": "string", "enum": []string{"delivering", "last_mile", "delivered", "sent_by_seller"}},
		}, "posting_numbers", "status"),
		Batch: func(a map[string]any) int {
			items, _ := a["posting_numbers"].([]any)
			return len(items)
		},
		Build: func(a map[string]any) (any, error) {
			path, ok := fbsStatusPaths[fmt.Sprint(a["status"])]
			if !ok {
				return nil, fmt.Errorf("status=%v: допустимо delivering, last_mile, delivered, sent_by_seller", a["status"])
			}
			nums, err := stringList(a["posting_numbers"])
			if err != nil {
				return nil, fmt.Errorf("posting_numbers: %w", err)
			}
			return statusRequest{path: path, body: obj{"posting_number": nums}}, nil
		},
		Do: func(ctx context.Context, c *ozon.Client, payload any) (any, error) {
			req := payload.(statusRequest)
			return c.Call(ctx, req.path, req.body)
		},
	})
}

var fbsStatusPaths = map[string]string{
	"delivering":     ozon.PathFBSSetDelivering,
	"last_mile":      ozon.PathFBSSetLastMile,
	"delivered":      ozon.PathFBSSetDelivered,
	"sent_by_seller": ozon.PathFBSSetSentBySeller,
}

type statusRequest struct {
	path string
	body obj
}

// shipOrder — одно отправление к сборке. packages == nil означает
// «целиком в одну коробку»: состав сервер возьмёт из самого отправления.
type shipOrder struct {
	PostingNumber string
	Packages      []any
}

// buildShip разбирает две формы вызова сборки.
func buildShip(a map[string]any) (any, error) {
	single, _ := a["posting_number"].(string)
	single = strings.TrimSpace(single)
	packages, hasPackages := a["packages"].([]any)
	_, hasList := a["posting_numbers"]

	switch {
	case hasPackages && single == "":
		return nil, fmt.Errorf("packages задаётся вместе с posting_number: раскладка по коробкам — всегда для одного отправления")
	case single != "" && hasList:
		return nil, fmt.Errorf("задайте что-то одно: posting_numbers (каждое целиком в коробку) или posting_number с packages")
	case single != "":
		if hasPackages && len(packages) == 0 {
			return nil, fmt.Errorf("packages пуст: уберите его, чтобы собрать отправление в одну коробку")
		}
		return []shipOrder{{PostingNumber: single, Packages: packages}}, nil
	}

	nums, err := stringList(a["posting_numbers"])
	if err != nil {
		return nil, fmt.Errorf("posting_numbers: %w", err)
	}
	var orders []shipOrder
	for _, n := range uniqueStrings(nums) {
		orders = append(orders, shipOrder{PostingNumber: n})
	}
	return orders, nil
}

// shipResult — итог сборки одного отправления.
type shipResult struct {
	PostingNumber string   `json:"posting_number"`
	Shipped       bool     `json:"shipped"`
	NewPostings   []string `json:"new_posting_numbers,omitempty"`
	Error         string   `json:"error,omitempty"`
	Hint          string   `json:"hint,omitempty"`
}

// shipPostings собирает отправления по одному.
//
// По одному, а не пачкой, потому что у Ozon и нет пачки: метод сборки
// принимает одно отправление. Ошибка на одном не останавливает
// остальные — иначе человек получил бы «сломалось» без понимания,
// какие заказы уже собраны, а какие нет. Отчёт перечисляет и те, и другие.
func shipPostings(ctx context.Context, c *ozon.Client, orders []shipOrder) (any, error) {
	var results []shipResult
	shipped := 0
	for _, o := range orders {
		res := shipResult{PostingNumber: o.PostingNumber}
		packages := o.Packages
		if packages == nil {
			var err error
			packages, err = wholePackage(ctx, c, o.PostingNumber)
			if err != nil {
				res.Error = err.Error()
				results = append(results, res)
				continue
			}
		}

		var out struct {
			Result []string `json:"result"`
		}
		err := c.CallInto(ctx, ozon.PathPostingFBSShip, obj{
			"posting_number": o.PostingNumber,
			"packages":       packages,
			"with":           obj{"additional_data": true},
		}, &out)
		if err != nil {
			res.Error = err.Error()
			res.Hint = shipHint(nil, err)
		} else {
			res.Shipped = true
			res.NewPostings = out.Result
			shipped++
		}
		results = append(results, res)
	}

	// Если не собралось ничего — это ошибка вызова, а не отчёт: модель
	// должна увидеть isError, а не зелёный ответ со списком провалов.
	if shipped == 0 {
		var b strings.Builder
		b.WriteString("Ни одно отправление не собрано.\n")
		for _, r := range results {
			fmt.Fprintf(&b, "\n%s: %s", r.PostingNumber, r.Error)
			if r.Hint != "" {
				fmt.Fprintf(&b, "\n  → %s", r.Hint)
			}
		}
		return nil, errors.New(b.String())
	}

	return shipReport{
		Shipped: shipped,
		Failed:  len(results) - shipped,
		Results: results,
		Next: "Этикетки — ozon_fbs_labels на номера из new_posting_numbers через 45–60 секунд. " +
			"Если коробок было несколько, Ozon разделил отправление: этикетки нужны на новые номера.",
	}, nil
}

// shipReport — отчёт о сборке. Структура, а не карта: порядок полей
// в ответе должен читаться сверху вниз одинаково при каждом вызове.
type shipReport struct {
	Shipped int          `json:"shipped"`
	Failed  int          `json:"failed"`
	Results []shipResult `json:"results"`
	Next    string       `json:"next"`
}

// wholePackage собирает состав «всё в одну коробку» из самого отправления.
func wholePackage(ctx context.Context, c *ozon.Client, postingNumber string) ([]any, error) {
	var got struct {
		Result struct {
			Status   string `json:"status"`
			Products []struct {
				SKU      int64 `json:"sku"`
				Quantity int   `json:"quantity"`
			} `json:"products"`
		} `json:"result"`
	}
	if err := c.CallInto(ctx, ozon.PathPostingFBSGet, obj{"posting_number": postingNumber}, &got); err != nil {
		return nil, fmt.Errorf("не получить состав отправления: %w", err)
	}
	if st := got.Result.Status; st != "" && st != "awaiting_packaging" {
		return nil, fmt.Errorf("отправление в статусе %s, а собрать можно только awaiting_packaging", st)
	}
	if len(got.Result.Products) == 0 {
		return nil, fmt.Errorf("в отправлении нет товаров — собирать нечего")
	}
	var products []any
	for _, p := range got.Result.Products {
		products = append(products, obj{"product_id": p.SKU, "quantity": p.Quantity})
	}
	return []any{obj{"products": products}}, nil
}

// shipHint разбирает отказы сборки.
func shipHint(_ map[string]any, err error) string {
	var apiErr *ozon.APIError
	if !asAPI(err, &apiErr) {
		return ""
	}
	msg := strings.ToLower(apiErr.Message + " " + apiErr.Code + " " + apiErr.Raw)
	switch {
	case strings.Contains(msg, "exemplar") || strings.Contains(msg, "mandatory") || strings.Contains(msg, "mark"):
		return "Товар требует маркировки («Честный знак») или данных экземпляров (ГТД, РНПТ). " +
			"Такие отправления собираются через кабинет Ozon: передача экземпляров в этом сервере не поддерживается."
	case strings.Contains(msg, "status") || strings.Contains(msg, "already"):
		return "Отправление уже собрано или отменено: собрать можно только статус awaiting_packaging. " +
			"Проверьте ozon_posting_get."
	case strings.Contains(msg, "quantity") || strings.Contains(msg, "product"):
		return "Состав коробок не совпадает с отправлением: product_id — это SKU (products[].sku), " +
			"и сумма quantity по коробкам должна равняться количеству в заказе."
	}
	return ""
}

// --- Мелочи ---

func uniqueStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0:0]
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f МБ", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f КБ", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d Б", n)
}
