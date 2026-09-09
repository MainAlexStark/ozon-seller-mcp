package tools

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/MainAlexStark/ozon-seller-mcp/ozon"
)

// RegisterQuestions добавляет вопросы покупателей о товаре.
//
// Вопросы — не разновидность отзывов, хотя лежат рядом. Отзыв пишут
// после покупки, и он объясняет прошлое: почему вернули, за что снизили
// оценку. Вопрос задают ДО покупки, и он держит решение о ней: пока
// «влезет ли на полку 30 см» висит без ответа, товар не покупает не
// один человек, а все, кто открыл карточку после него. Поэтому здесь
// есть и запись: увидеть вопрос и не иметь возможности ответить —
// ровно та половина работы, от которой нет пользы.
//
// Важное ограничение доступа: методы вопросов Ozon открывает только
// продавцам с подпиской Premium Plus. Без неё они отвечают отказом
// прав, а не пустым списком, — см. premiumHint.
func (r *Registry) RegisterQuestions() {
	r.Add(Spec{
		Name: "ozon_questions_count",
		Path: ozon.PathQuestionCount,
		Desc: "Сводка вопросов по статусам: сколько всего, новых, просмотренных, обработанных. " +
			"Дешёвый способ понять, есть ли вообще что разбирать, до выгрузки самих вопросов.",
		Schema: schema(obj{}),
		Hint:   premiumHint,
	})

	r.Add(Spec{
		Name: "ozon_questions_list",
		Path: ozon.PathQuestionList,
		Desc: "Вопросы покупателей о товарах. Статус NEW — ещё не отвеченные: с них и стоит начинать. " +
			"Если статус не задан, возвращаются все.",
		Schema: schema(obj{
			"filter": schema(obj{
				"date_from": str("Начало периода, RFC3339"),
				"date_to":   str("Конец периода, RFC3339"),
				"status": obj{
					"type":        "string",
					"enum":        questionListStatuses,
					"description": "NEW — новый, VIEWED — просмотренный, PROCESSED — обработанный, UNPROCESSED — необработанный, ALL — все",
				},
			}),
			"last_id": str("Курсор постраничного обхода из предыдущего ответа"),
		}),
		Build: func(a map[string]any) (any, error) {
			return withQuestionStatus(a), nil
		},
		Hint: premiumHint,
	})

	r.Add(Spec{
		Name: "ozon_question_info",
		Path: ozon.PathQuestionInfo,
		Desc: "Подробности одного вопроса: текст, товар, автор, число ответов и ссылка на карточку.",
		Schema: schema(obj{
			"question_id": str("Идентификатор вопроса из ozon_questions_list"),
		}, "question_id"),
		Build: func(a map[string]any) (any, error) {
			id, err := stringID(a["question_id"])
			if err != nil {
				return nil, fmt.Errorf("question_id: %w", err)
			}
			return obj{"question_id": id}, nil
		},
		Hint: premiumHint,
	})

	r.Add(Spec{
		Name: "ozon_question_answers",
		Path: ozon.PathQuestionAnswerList,
		Desc: "Ответы на вопрос: что уже написано и кем. " +
			"Смотрите сюда перед тем, как отвечать: у вопроса может быть ответ от другого продавца того же товара.",
		Schema: schema(obj{
			"question_id": str("Идентификатор вопроса"),
			"sku":         num("SKU товара, к которому задан вопрос"),
			"last_id":     str("Курсор постраничного обхода"),
		}, "question_id", "sku"),
		Build: func(a map[string]any) (any, error) {
			id, err := stringID(a["question_id"])
			if err != nil {
				return nil, fmt.Errorf("question_id: %w", err)
			}
			a["question_id"] = id
			return a, nil
		},
		Hint: premiumHint,
	})

	// --- Запись ---

	r.Add(Spec{
		Name: "ozon_question_answer_create",
		Path: ozon.PathQuestionAnswerCreate,
		Desc: "Ответить на вопрос покупателя. ИЗМЕНЯЕТ ДАННЫЕ: ответ публикуется в карточке " +
			"под именем магазина и виден всем. Длина текста — от 2 до 3000 символов.",
		Write: true,
		Schema: schema(obj{
			"question_id": str("Идентификатор вопроса из ozon_questions_list"),
			"sku":         num("SKU товара из того же ответа"),
			"text":        str("Текст ответа, от 2 до 3000 символов"),
		}, "question_id", "sku", "text"),
		Build: func(a map[string]any) (any, error) {
			id, err := stringID(a["question_id"])
			if err != nil {
				return nil, fmt.Errorf("question_id: %w", err)
			}

			text, _ := a["text"].(string)
			if err := checkAnswerText(text); err != nil {
				return nil, err
			}

			if _, ok := toInt(a["sku"]); !ok {
				return nil, fmt.Errorf("нужен sku товара: он приходит вместе с вопросом " +
					"в ozon_questions_list и без него ответ не принимается")
			}

			a["question_id"] = id
			a["text"] = text
			return a, nil
		},
		Hint: premiumHint,
	})

	r.Add(Spec{
		Name: "ozon_question_change_status",
		Path: ozon.PathQuestionChangeStatus,
		Desc: "Пометить вопросы просмотренными или обработанными. ИЗМЕНЯЕТ ДАННЫЕ, но только разметку " +
			"в вашем кабинете: покупатель этого не видит. Нужен, чтобы разобранные вопросы " +
			"не попадали в выборку NEW снова и снова.",
		Write: true,
		Schema: schema(obj{
			"question_ids": arr(obj{"type": "string"}, "Идентификаторы вопросов"),
			"status": obj{
				"type":        "string",
				"enum":        questionWriteStatuses,
				"description": "NEW — вернуть в новые, VIEWED — просмотрен, PROCESSED — обработан",
			},
		}, "question_ids", "status"),
		Batch: func(a map[string]any) int {
			ids, _ := a["question_ids"].([]any)
			return len(ids)
		},
		Build: func(a map[string]any) (any, error) {
			ids, err := stringList(a["question_ids"])
			if err != nil {
				return nil, fmt.Errorf("question_ids: %w", err)
			}

			status, _ := a["status"].(string)
			if !slices.Contains(questionWriteStatuses, status) {
				return nil, fmt.Errorf(
					"status=%q: этот метод принимает только %s. "+
						"Значения UNPROCESSED и ALL есть у фильтра списка, но проставить их нельзя",
					status, strings.Join(questionWriteStatuses, ", "))
			}

			return obj{"question_ids": ids, "status": status}, nil
		},
		Hint: premiumHint,
	})
}

// Статусы вопросов. Списки разные не по недосмотру: ALL и UNPROCESSED
// описывают выборку, а не состояние, и проставить их нельзя.
var (
	questionListStatuses  = []string{"ALL", "NEW", "VIEWED", "PROCESSED", "UNPROCESSED"}
	questionWriteStatuses = []string{"NEW", "VIEWED", "PROCESSED"}
)

// Границы текста ответа, которые проверяет Ozon.
const (
	minAnswerRunes = 2
	maxAnswerRunes = 3000
)

// checkAnswerText проверяет длину ответа до публикации.
//
// Считаем именно символы, а не байты: в русском тексте байтов вдвое
// больше, и проверка по длине строки отсекала бы половину допустимых
// ответов — при том что отказ приходил бы уже после отправки.
func checkAnswerText(text string) error {
	n := utf8.RuneCountInString(strings.TrimSpace(text))
	switch {
	case n < minAnswerRunes:
		return fmt.Errorf("текст ответа пуст: Ozon принимает от %d символов", minAnswerRunes)
	case n > maxAnswerRunes:
		return fmt.Errorf("в ответе %d символов, а Ozon принимает не больше %d", n, maxAnswerRunes)
	}
	return nil
}

// withQuestionStatus достраивает фильтр статуса.
//
// Без фильтра метод отвечает ошибкой валидации, а спрашивают обычно
// «что там за вопросы» — без всякого статуса. ALL здесь значит
// «фильтр есть, но ничего не отсекает», как visibility в каталоге.
func withQuestionStatus(a map[string]any) map[string]any {
	if a == nil {
		a = map[string]any{}
	}

	filter, _ := a["filter"].(map[string]any)
	if filter == nil {
		filter = map[string]any{}
	}
	if v, ok := filter["status"]; !ok || v == nil || v == "" {
		filter["status"] = "ALL"
	}
	a["filter"] = filter
	return a
}

// premiumHint объясняет отказ прав на методах вопросов.
//
// Ozon отвечает на них 403, если у продавца нет подписки Premium Plus.
// Сообщение при этом общее — про доступ, — и по нему одинаково хорошо
// читается и «неверный ключ», и «метод отключён». Разница существенная:
// ключ чинится за минуту, а подписка стоит денег и решается не сейчас.
func premiumHint(_ map[string]any, err error) string {
	var apiErr *ozon.APIError
	if !asAPI(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden {
		return ""
	}

	return "Методы вопросов о товаре Ozon открывает только продавцам с подпиской Premium Plus.\n" +
		"Отказ прав здесь чаще означает именно её отсутствие, а не проблему с ключом:\n" +
		"если остальные инструменты отвечают (ozon_api_selftest покажет), ключ в порядке.\n\n" +
		"Отзывы (ozon_reviews_list) живут по тому же правилу; каталог, цены и заказы — нет."
}
