package ozon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// APIError — ошибка, вернувшаяся от Ozon.
type APIError struct {
	StatusCode int
	Path       string
	Code       string `json:"code"`
	Message    string `json:"message"`
	Details    any    `json:"details,omitempty"`

	// Raw — тело ответа, если оно не разобралось как JSON.
	Raw string
}

func newAPIError(status int, path string, raw []byte) *APIError {
	e := &APIError{StatusCode: status, Path: path}
	if err := json.Unmarshal(raw, e); err != nil {
		e.Raw = strings.TrimSpace(string(raw))
	}
	return e
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = e.Raw
	}
	return fmt.Sprintf("ozon %s: HTTP %d %s %s", e.Path, e.StatusCode, e.Code, msg)
}

// Retryable сообщает, имеет ли смысл повторить запрос.
func (e *APIError) Retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

// Hint возвращает человеческую подсказку по типовым кодам.
//
// Она попадает прямо в ответ инструмента, чтобы модель (и вы) сразу
// видели, что делать, а не гадали по номеру статуса.
func (e *APIError) Hint() string {
	switch e.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		// 403 приходит и на «ключ не тот», и на «метод не входит в вашу
		// подписку». Советы тут противоположные, а перепутать легко:
		// человек идёт перевыпускать рабочий ключ вместо того, чтобы
		// посмотреть тариф. Различаем по формулировке Ozon.
		if isSubscriptionDenied(e.Message) {
			return "Метод недоступен на вашем тарифе — дело не в ключе. Часть методов " +
				"(например, отзывы) Ozon открывает только с платной подпиской. " +
				"Проверить можно в кабинете, в разделе подписок."
		}
		return "Ключ не принят. Проверьте OZON_CLIENT_ID и OZON_API_KEY. " +
			"Учтите: с сентября 2026 новые ключи в кабинете выпускаются на 3 месяца — возможно, срок истёк."
	case http.StatusNotFound:
		return "Метод не найден. Вероятно, Ozon отключил эту версию — сверьтесь с docs.ozon.ru/api/seller " +
			"и запустите ozon_api_selftest, чтобы увидеть все затронутые методы разом."
	case http.StatusTooManyRequests:
		return "Превышен лимит запросов. Клиент уже делает повторы с паузой; если повторяется — снизьте частоту."
	case http.StatusBadRequest:
		// У 400 два принципиально разных источника, и путать их дорого:
		// либо неверно собрано тело запроса, либо не заполнено то, что
		// требует категория. Ozon сам говорит, какой это случай, —
		// подсказку выбираем по его формулировке, а не гадаем.
		if isRequestValidation(e.Message) {
			return "Ozon отверг форму запроса: не хватает обязательного поля или оно не того типа. " +
				"Точное имя поля названо в сообщении выше — обычно это filter, который у большинства " +
				"методов обязателен даже когда фильтровать нечего (передайте {\"visibility\": \"ALL\"})."
		}
		return "Ozon отверг содержимое. Чаще всего это незаполненная обязательная характеристика категории: " +
			"посмотрите ozon_category_attributes для нужной категории и типа товара."
	}
	if e.StatusCode >= 500 {
		return "Ошибка на стороне Ozon. Клиент повторил запрос несколько раз — стоит попробовать позже."
	}
	return ""
}

// isSubscriptionDenied узнаёт отказ по тарифу, а не по ключу.
func isSubscriptionDenied(msg string) bool {
	m := strings.ToLower(msg)
	for _, marker := range []string{"subscription", "подписк", "permissiondenied", "тариф"} {
		if strings.Contains(m, marker) {
			return true
		}
	}
	return false
}

// isRequestValidation отличает «тело собрано неправильно» от
// «данные не прошли проверку по существу».
func isRequestValidation(msg string) bool {
	m := strings.ToLower(msg)
	for _, marker := range []string{"validation error", "value is required", "invalid ", "cannot unmarshal"} {
		if strings.Contains(m, marker) {
			return true
		}
	}
	return false
}

// asAPIError — обёртка над errors.As для читаемости в client.go.
func asAPIError(err error, target **APIError) bool {
	return errors.As(err, target)
}
