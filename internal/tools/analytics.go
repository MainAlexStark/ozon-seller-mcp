package tools

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/MainAlexStark/ozon-seller-mcp/ozon"
)

// RegisterAnalytics добавляет аналитику и отправления.
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
		Hint: staleAnalyticsHint,
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

	// Финансовые инструменты живут в finance.go: там же лежит свод,
	// без которого вопрос «сколько вышло за месяц» превращается
	// в тридцать вызовов.

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
)

// staleAnalyticsHint объясняет самый обманчивый отказ аналитики.
//
// На период старше окна хранения Ozon отвечает «date_to must be greater
// than date_from» — то есть жалуется на порядок дат, который в запросе
// в полном порядке. Модель верит сообщению, начинает переставлять даты
// местами, каждый раз получает тот же отказ и в конце концов уходит
// собирать период по дням через начисления. Так вопрос про выручку за
// прошлый год превращался в тридцать вызовов вместо ответа «этих данных
// в аналитике уже нет».
func staleAnalyticsHint(a map[string]any, err error) string {
	var apiErr *ozon.APIError
	if !asAPI(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		return ""
	}
	if !strings.Contains(strings.ToLower(apiErr.Message), "date_to must be greater than date_from") {
		return ""
	}

	// Если даты и правда перепутаны, сообщение Ozon верное — и подменять
	// его догадкой нельзя.
	if checkPeriod(a, 0) != nil {
		return ""
	}

	from, _ := a["date_from"].(string)
	to, _ := a["date_to"].(string)
	return fmt.Sprintf(
		"Даты %s…%s заданы верно, и переставлять их местами не нужно: так Ozon отвечает,\n"+
			"когда период выходит за окно хранения аналитики (примерно последние три месяца).\n"+
			"Повторный вызов вернёт тот же отказ.\n\n"+
			"За более ранний период считайте по деньгам, а не по витрине:\n"+
			"    ozon_finance_summary date_from=%s date_to=%s\n"+
			"Он отдаёт начисления — сколько начислено, удержано и вышло чистыми.",
		from, to, from, to)
}

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
