package tools

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/MainAlexStark/ozon-seller-mcp/ozon"
)

// RegisterReturns добавляет инструменты возвратов.
//
// Зачем они нужны отдельно от финансов. Финансовые инструменты
// показывают удержания: сколько денег ушло обратно и по какой статье
// начисления. Чего они не показывают — из-за чего именно: какой товар
// вернули, по какой причине, где он сейчас лежит и вернётся ли он
// вообще на склад или поедет на утилизацию. Пока этого не видно,
// «месяц вышел в минус» остаётся фактом без объяснения, а объяснение
// как раз и решает, что делать дальше: править описание карточки,
// менять упаковку или снимать товар с продажи.
func (r *Registry) RegisterReturns() {
	r.Add(Spec{
		Name: "ozon_returns_list",
		Path: ozon.PathReturnsList,
		Desc: "Возвраты FBS и FBO одним списком: что вернули, по какой причине, в каком статусе и где лежит. " +
			"Отвечает на вопрос «из-за чего удержания», на который финансовые инструменты ответить не могут. " +
			"Фильтры взаимоисключающие: за один вызов работает только одно условие.",
		Schema: schema(obj{
			"filter": schema(obj{
				"logistic_return_date": schema(obj{
					"time_from": str("Начало периода, RFC3339"),
					"time_to":   str("Конец периода, RFC3339"),
				}, "time_from", "time_to"),
				"visual_status_change_moment": schema(obj{
					"time_from": str("Начало периода, RFC3339"),
					"time_to":   str("Конец периода, RFC3339"),
				}, "time_from", "time_to"),
				"storage_tariffication_start_date": schema(obj{
					"time_from": str("Начало периода, RFC3339"),
					"time_to":   str("Конец периода, RFC3339"),
				}, "time_from", "time_to"),
				"order_id":        num("Идентификатор заказа"),
				"posting_numbers": arr(obj{"type": "string"}, "Номера отправлений, не больше 50 за вызов"),
				"product_name":    str("Название товара"),
				"offer_id":        str("Артикул продавца"),
				"visual_status_name": str("Статус возврата. Частые значения: " +
					"ArrivedAtReturnPlace — в пункте выдачи; MovingToSeller — едет к вам; " +
					"ReceivedBySeller — вы его получили; MoneyReturned — покупателю вернули деньги; " +
					"Utilizing и Utilized — на утилизации и утилизирован; " +
					"OnSellerApproval — ждёт вашего решения; DisputeOpened — открыт спор"),
				"warehouse_id":  num("Идентификатор склада"),
				"barcode":       str("Штрихкод возвратной этикетки"),
				"return_schema": obj{"type": "string", "enum": []string{"FBS", "FBO"}, "description": "Схема доставки"},
			}),
			"limit":   obj{"type": "integer", "default": defaultReturnsLimit, "maximum": maxReturnsLimit},
			"last_id": num("Идентификатор последнего возврата из предыдущего ответа"),
		}),
		Build: func(a map[string]any) (any, error) {
			if err := checkReturnsFilter(a); err != nil {
				return nil, err
			}
			return capLimit(withLimit(a, defaultReturnsLimit), maxReturnsLimit), nil
		},
		Hint: singleFilterHint,
	})

	r.Add(Spec{
		Name: "ozon_returns_dropoff_points",
		Path: ozon.PathReturnsDropoffInfo,
		Desc: "Пункты выдачи, где лежат ваши возвраты FBS, и сколько их там. " +
			"Нужен ровно перед поездкой: список возвратов говорит, что вернули, а этот — куда ехать.",
		Schema: schema(obj{
			"place_id": num("Идентификатор пункта выдачи; пусто — все пункты"),
			"limit":    obj{"type": "integer", "default": defaultReturnsLimit, "maximum": maxReturnsLimit},
			"last_id":  num("Идентификатор последнего пункта из предыдущего ответа"),
		}),
		Build: func(a map[string]any) (any, error) {
			// Метод принимает не плоские аргументы, а две вложенные
			// структуры. Модель об этом не знает и присылает плоские —
			// собираем нужную форму здесь, вместо того чтобы объяснять
			// её в описании и получать 400 на каждой второй попытке.
			pagination := obj{"limit": defaultReturnsLimit}
			if n, ok := toInt(a["limit"]); ok {
				pagination["limit"] = min(n, maxReturnsLimit)
			}
			if v, ok := a["last_id"]; ok && v != nil {
				pagination["last_id"] = v
			}

			filter := obj{}
			if v, ok := a["place_id"]; ok && v != nil {
				filter["place_id"] = v
			}

			return obj{"filter": filter, "pagination": pagination}, nil
		},
	})
}

// Границы метода возвратов.
const (
	defaultReturnsLimit = 50
	maxReturnsLimit     = 500

	// maxReturnPostingNumbers — сколько отправлений принимает фильтр.
	maxReturnPostingNumbers = 50
)

// checkReturnsFilter ловит превышение размера фильтра до отправки.
func checkReturnsFilter(a map[string]any) error {
	filter, _ := a["filter"].(map[string]any)
	if filter == nil {
		return nil
	}

	numbers, ok := filter["posting_numbers"].([]any)
	if ok && len(numbers) > maxReturnPostingNumbers {
		return fmt.Errorf(
			"фильтр принимает не больше %d номеров отправлений за вызов, передано %d. "+
				"Разбейте на несколько вызовов",
			maxReturnPostingNumbers, len(numbers))
	}
	return nil
}

// singleFilterHint объясняет отказ, который выглядит как ошибка формата.
//
// Метод возвратов принимает ровно одно условие фильтра за вызов.
// Просьба «покажи возвраты по этому артикулу за август» переводится
// в два условия совершенно естественно, и Ozon отвечает на это общей
// ошибкой валидации, из которой правило не следует никак. Без подсказки
// дальше идёт перебор формулировок, а помогает только выбор одного
// условия и отбор второго уже по ответу.
func singleFilterHint(a map[string]any, err error) string {
	var apiErr *ozon.APIError
	if !asAPI(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		return ""
	}

	filter, _ := a["filter"].(map[string]any)
	used := activeFilterKeys(filter)
	if len(used) < 2 {
		return ""
	}

	return fmt.Sprintf(
		"В фильтре сразу %d условия: %s. Этот метод принимает только одно за вызов —\n"+
			"поэтому отказ приходит на верно составленный запрос.\n\n"+
			"Оставьте одно условие (обычно период logistic_return_date или артикул offer_id),\n"+
			"а остальное отберите уже по ответу.",
		len(used), strings.Join(used, ", "))
}

// activeFilterKeys перечисляет заполненные условия фильтра.
func activeFilterKeys(filter map[string]any) []string {
	var used []string
	for key, value := range filter {
		switch v := value.(type) {
		case nil:
		case string:
			if v != "" {
				used = append(used, key)
			}
		case []any:
			if len(v) > 0 {
				used = append(used, key)
			}
		case map[string]any:
			if len(v) > 0 {
				used = append(used, key)
			}
		default:
			used = append(used, key)
		}
	}
	// Порядок перебора карты случаен, а подсказка должна читаться
	// одинаково при каждом вызове.
	slices.Sort(used)
	return used
}
