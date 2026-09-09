package tools

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MainAlexStark/ozon-seller-mcp/ozon"
)

// RegisterFBO добавляет инструменты схемы FBO — той, где товар лежит
// на складе Ozon, а не у продавца.
//
// Почему это отдельный набор, а не пара методов рядом с FBS. При FBS
// вопрос звучит как «что отгрузить сегодня», и на него отвечают
// отправления. При FBO вопрос другой — «что и куда везти заранее»,
// и на него одними заказами не ответить: нужны поставки, их состав,
// свободные таймслоты приёмки и остатки в разрезе складов Ozon.
// Поэтому здесь всё это есть, и все инструменты только читают:
// создание поставки — многошаговый сценарий с асинхронными статусами,
// и цена ошибки в нём измеряется не карточками, а машиной, приехавшей
// не в тот день.
func (r *Registry) RegisterFBO() {
	// --- Заказы FBO ---

	r.Add(Spec{
		Name: "ozon_fbo_postings_list",
		Path: ozon.PathPostingFBOList,
		Desc: "Заказы FBO за период: что купили с ваших остатков на складах Ozon. " +
			"Если период не задан, берутся последние 7 дней. " +
			"Постраничность курсорная: cursor из предыдущего ответа, признак продолжения — has_next.",
		Schema: schema(obj{
			"filter": schema(obj{
				"since":           str("Начало периода, RFC3339"),
				"to":              str("Конец периода, RFC3339"),
				"statuses":        arr(obj{"type": "string"}, "Статусы отправлений, например awaiting_deliver"),
				"order_numbers":   arr(obj{"type": "string"}, "Номера заказов"),
				"posting_numbers": arr(obj{"type": "string"}, "Номера отправлений"),
			}),
			"cursor":   str("Курсор из предыдущего ответа"),
			"limit":    obj{"type": "integer", "default": 50, "maximum": 1000},
			"sort_dir": obj{"type": "string", "enum": []string{"ASC", "DESC"}},
			"with": obj{
				"type":        "object",
				"description": "Что добавить в ответ: analytics_data, financial_data, legal_info",
			},
		}),
		Build: func(a map[string]any) (any, error) {
			return withLimit(withRecentPeriod(a, 7), 50), nil
		},
	})

	r.Add(Spec{
		Name: "ozon_fbo_posting_get",
		Path: ozon.PathPostingFBOGet,
		Desc: "Подробности одного заказа FBO по номеру отправления.",
		Schema: schema(obj{
			"posting_number": str("Номер отправления"),
			"with": obj{
				"type":        "object",
				"description": "Что добавить в ответ: analytics_data, financial_data",
			},
		}, "posting_number"),
	})

	// --- Остатки на складах Ozon ---

	r.Add(Spec{
		Name: "ozon_fbo_stocks",
		Path: ozon.PathStockOnWarehouses,
		Desc: "Остатки на складах Ozon в разрезе складов: сколько свободно к продаже, сколько в резерве, сколько едет. " +
			"Отвечает на вопрос «где товар кончается», на который сводный остаток ответить не может: " +
			"нуль в одном кластере не виден, пока в другом лежит запас.",
		Schema: schema(obj{
			"limit":  obj{"type": "integer", "default": 100, "maximum": 1000},
			"offset": num("Смещение"),
			"warehouse_type": obj{
				"type":        "string",
				"enum":        []string{"ALL", "EXPRESS_DARK_STORE", "NOT_EXPRESS_DARK_STORE"},
				"default":     "ALL",
				"description": "Тип складов: все или только (не) экспресс-склады",
			},
		}),
		Build: func(a map[string]any) (any, error) {
			a = withLimit(a, 100)
			if v, ok := a["warehouse_type"]; !ok || v == nil || v == "" {
				a["warehouse_type"] = "ALL"
			}
			return a, nil
		},
	})

	// --- Поставки на склады Ozon ---

	r.Add(Spec{
		Name: "ozon_supply_orders_list",
		Path: ozon.PathSupplyOrderList,
		Desc: "Заявки на поставку: что вы уже везёте или собираетесь везти на склады Ozon. " +
			"Возвращает идентификаторы заявок для ozon_supply_order_get. " +
			"Без фильтра показывает заявки во всех статусах.",
		Schema: schema(obj{
			"filter": schema(obj{
				"states": arr(obj{"type": "string", "enum": supplyOrderStates},
					"Статусы заявок. Пусто — все статусы"),
				"dropoff_warehouse_ids": arr(obj{"type": "string"}, "Склады отгрузки"),
				"order_number_search":   str("Поиск по номеру заявки"),
				"timeslot_from_range": schema(obj{
					"from":                 str("Начало диапазона таймслота, RFC3339"),
					"to":                   str("Конец диапазона таймслота, RFC3339"),
					"timeslot_filter_type": str("Тип фильтра по таймслоту"),
				}),
			}),
			"limit":    obj{"type": "integer", "default": 100, "maximum": 1000},
			"last_id":  str("Курсор постраничного обхода"),
			"sort_by":  obj{"type": "string", "default": defaultSupplySortBy, "description": "Поле сортировки"},
			"sort_dir": obj{"type": "string", "enum": []string{"ASC", "DESC"}},
		}),
		Build: func(a map[string]any) (any, error) {
			return withSupplyFilter(withLimit(a, 100)), nil
		},
	})

	r.Add(Spec{
		Name: "ozon_supply_order_get",
		Path: ozon.PathSupplyOrderGet,
		Desc: "Подробности заявок на поставку: склад, таймслот, статус, состав. " +
			"Идентификатор состава (bundle_id) из ответа передаётся в ozon_supply_order_items.",
		Schema: schema(obj{
			"order_ids": arr(obj{"type": "string"}, "Идентификаторы заявок из ozon_supply_orders_list"),
		}, "order_ids"),
		Build: func(a map[string]any) (any, error) {
			ids, err := stringList(a["order_ids"])
			if err != nil {
				return nil, fmt.Errorf("order_ids: %w", err)
			}
			return map[string]any{"order_ids": ids}, nil
		},
	})

	r.Add(Spec{
		Name: "ozon_supply_order_items",
		Path: ozon.PathSupplyOrderBundle,
		Desc: "Состав поставки: какие товары и в каком количестве едут по заявке. " +
			"bundle_id берите из ozon_supply_order_get.",
		Schema: schema(obj{
			"bundle_ids": arr(obj{"type": "string"}, "Идентификаторы составов из ozon_supply_order_get"),
			"limit":      obj{"type": "integer", "default": 100, "maximum": 1000},
			"last_id":    str("Курсор постраничного обхода"),
			"query":      str("Поиск по названию или артикулу"),
			"sort_field": str("Поле сортировки"),
			"is_asc":     boolean("Сортировать по возрастанию"),
		}, "bundle_ids"),
		Build: func(a map[string]any) (any, error) {
			ids, err := stringList(a["bundle_ids"])
			if err != nil {
				return nil, fmt.Errorf("bundle_ids: %w", err)
			}
			a["bundle_ids"] = ids
			return withLimit(a, 100), nil
		},
	})

	r.Add(Spec{
		Name: "ozon_supply_orders_counters",
		Path: ozon.PathSupplyOrderCounters,
		Desc: "Сводка заявок на поставку по статусам: сколько в каком состоянии. " +
			"Заодно показывает сами названия статусов — их можно передать в фильтр ozon_supply_orders_list.",
		Schema: schema(obj{}),
	})

	r.Add(Spec{
		Name: "ozon_supply_timeslots",
		Path: ozon.PathSupplyTimeslots,
		Desc: "Доступные таймслоты приёмки по заявке на поставку — когда склад готов принять машину.",
		Schema: schema(obj{
			"order_id": num("Идентификатор заявки из ozon_supply_orders_list"),
		}, "order_id"),
	})

	// --- Склады Ozon ---

	r.Add(Spec{
		Name: "ozon_fbo_clusters",
		Path: ozon.PathClusterList,
		Desc: "Кластеры Ozon и склады внутри них. Нужен, чтобы понять, куда вообще можно везти " +
			"и к какому кластеру относится склад из остатков.",
		Schema: schema(obj{}),
	})
}

// defaultSupplySortBy — сортировка по умолчанию. Поле обязательное:
// без него Ozon отвечает «invalid SortBy: value must not be in list [0]»,
// то есть отказывается от значения по умолчанию своего же протокола.
const defaultSupplySortBy = "ORDER_CREATION"

// supplyOrderStates — статусы заявок на поставку.
//
// Осторожно: ozon_supply_orders_counters возвращает те же статусы
// с префиксом ORDER_STATE_, а фильтр списка принимает их без префикса
// и молча выбрасывает всё, что не узнал, — превращая непустой фильтр
// в пустой и отвечая «States: value must contain at least 1 item».
// Поэтому префикс здесь срезается, а не передаётся как есть.
var supplyOrderStates = []string{
	"DATA_FILLING",
	"READY_TO_SUPPLY",
	"ACCEPTED_AT_SUPPLY_WAREHOUSE",
	"IN_TRANSIT",
	"ACCEPTANCE_AT_STORAGE_WAREHOUSE",
	"REPORTS_CONFIRMATION_AWAITING",
	"REPORT_REJECTED",
	"COMPLETED",
	"REJECTED_AT_SUPPLY_WAREHOUSE",
	"CANCELLED",
}

// withSupplyFilter достраивает обязательные поля списка заявок.
//
// Ozon требует и сортировку, и непустой список статусов — при том что
// вопрос «какие у меня поставки» никаких фильтров не подразумевает.
// Без этого метод отвечал ошибкой на самый естественный вызов.
func withSupplyFilter(a map[string]any) map[string]any {
	if a == nil {
		a = map[string]any{}
	}

	if v, ok := a["sort_by"]; !ok || v == nil || v == "" {
		a["sort_by"] = defaultSupplySortBy
	}

	filter, _ := a["filter"].(map[string]any)
	if filter == nil {
		filter = map[string]any{}
	}

	states := normalizeSupplyStates(filter["states"])
	if len(states) == 0 {
		states = append([]string(nil), supplyOrderStates...)
	}
	filter["states"] = states

	a["filter"] = filter
	return a
}

// normalizeSupplyStates приводит статусы к тому виду, который понимает
// фильтр: без префикса ORDER_STATE_ и без пустых значений.
func normalizeSupplyStates(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}

	var out []string
	for _, item := range items {
		state, ok := item.(string)
		if !ok {
			continue
		}
		state = strings.TrimSpace(state)
		state = strings.TrimPrefix(state, "ORDER_STATE_")
		if state != "" && state != "UNSPECIFIED" {
			out = append(out, state)
		}
	}
	return out
}

// stringList приводит список идентификаторов к строкам.
//
// Ozon ждёт здесь именно строки, а идентификаторы заявок выглядят как
// числа — и модель, увидев число в предыдущем ответе, отправляет число.
// Ошибка при этом приходит от Ozon и звучит как «invalid type», хотя
// значение верное. Дешевле привести тип здесь, чем объяснять это
// в описании каждого инструмента.
func stringList(v any) ([]string, error) {
	items, ok := v.([]any)
	if !ok {
		// Список, приехавший строкой ("[\"1\",\"2\"]"), — не редкость:
		// так бывает, когда клиент не знает схемы поля и передаёт
		// аргумент как есть. Разбираем, вместо того чтобы отказывать.
		if raw, isString := v.(string); isString {
			var parsed []any
			if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
				items = parsed
				ok = true
			}
		}
		if !ok {
			return nil, fmt.Errorf("ожидается список, получено %T", v)
		}
	}

	out := make([]string, 0, len(items))
	for _, item := range items {
		switch id := item.(type) {
		case string:
			out = append(out, id)
		case float64:
			// JSON не различает целые и дробные: любое число приезжает
			// как float64, а идентификатор — всегда целое.
			out = append(out, fmt.Sprintf("%.0f", id))
		default:
			return nil, fmt.Errorf("идентификатор должен быть строкой или числом, получено %T", item)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("список пуст")
	}
	return out, nil
}
