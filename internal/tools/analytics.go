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
	// /v3/finance/transaction/list отключён в 2026 году. Три метода
	// ниже — его замена, и у них жёсткое ограничение: окно запроса
	// не больше месяца. Поэтому здесь стоит собственная проверка,
	// иначе модель регулярно будет получать невнятную ошибку.

	r.Add(Spec{
		Name: "ozon_finance_by_day",
		Path: ozon.PathFinanceAccrualByDay,
		Desc: "Начисления по дням: сводка выплат, комиссий и удержаний. " +
			"Пришёл на смену отключённому /v3/finance/transaction/list. Период — не больше месяца за запрос.",
		Schema: schema(obj{
			"date_from": str("Начало периода, YYYY-MM-DD"),
			"date_to":   str("Конец периода, YYYY-MM-DD (не более месяца от date_from)"),
		}, "date_from", "date_to"),
		Build: func(a map[string]any) (any, error) { return a, checkPeriod(a, 31) },
	})

	r.Add(Spec{
		Name: "ozon_finance_postings",
		Path: ozon.PathFinanceAccrualPostings,
		Desc: "Начисления в разрезе отправлений: что именно удержано по каждому заказу. " +
			"Здесь видно реальную экономику конкретной продажи. Период — не больше месяца.",
		Schema: schema(obj{
			"date_from": str("Начало периода, YYYY-MM-DD"),
			"date_to":   str("Конец периода, YYYY-MM-DD (не более месяца от date_from)"),
			"page":      obj{"type": "integer", "default": 1},
			"page_size": obj{"type": "integer", "default": 100},
		}, "date_from", "date_to"),
		Build: func(a map[string]any) (any, error) { return a, checkPeriod(a, 31) },
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
			"Вход для планирования того, что нужно произвести и отгрузить.",
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
			if _, ok := a["limit"]; !ok {
				a["limit"] = 50
			}
			return a, nil
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
		Desc: "Отзывы на товары. Полезно, чтобы понять, что покупатели пишут про размер, качество печати и упаковку.",
		Schema: schema(obj{
			"limit":    obj{"type": "integer", "default": 20},
			"status":   obj{"type": "string", "enum": []string{"ALL", "UNPROCESSED", "PROCESSED"}},
			"sort_dir": obj{"type": "string", "enum": []string{"ASC", "DESC"}},
			"last_id":  str("Курсор постраничного обхода"),
		}),
	})
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
