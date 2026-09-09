package tools

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Значения по умолчанию для тел запросов.
//
// У большинства методов Seller API поле filter обязательно даже тогда,
// когда фильтровать нечего: без него Ozon отвечает
//
//	400 Request validation error: invalid ...Request.Filter: value is required
//
// Модель об этом не знает и будет спотыкаться раз за разом, а из текста
// ошибки не очевидно, что «фильтр» здесь — не про сужение выборки,
// а про обязательную структуру. Поэтому недостающий filter достраивается
// здесь, до отправки.

// defaultVisibility — «все товары». Значение, при котором filter
// присутствует, но ничего не отсекает.
const defaultVisibility = "ALL"

// withFilter достраивает обязательный filter и его visibility.
//
// Уже заданные пользователем значения не трогаются: помощь не должна
// молча переопределять то, что попросили явно.
func withFilter(a map[string]any) map[string]any {
	if a == nil {
		a = map[string]any{}
	}

	raw, ok := a["filter"]
	if !ok || raw == nil {
		a["filter"] = map[string]any{"visibility": defaultVisibility}
		return a
	}

	filter, ok := raw.(map[string]any)
	if !ok {
		// filter задан чем-то, что мы не понимаем, — отдаём как есть
		// и позволяем Ozon объяснить, что не так.
		return a
	}
	if v, ok := filter["visibility"]; !ok || v == nil || v == "" {
		filter["visibility"] = defaultVisibility
	}
	a["filter"] = filter
	return a
}

// withLimit проставляет лимит, если он не задан.
func withLimit(a map[string]any, n int) map[string]any {
	if a == nil {
		a = map[string]any{}
	}
	if v, ok := a["limit"]; !ok || v == nil {
		a["limit"] = n
	}
	return a
}

// capLimit опускает limit до потолка метода, не трогая меньшие
// значения и не подставляя своего.
//
// Отличие от clampLimit в том, что нижней границы у таких методов нет:
// «покажи пару возвратов» — законная просьба, а вот запрос пятисот
// с лишним записей Ozon отвергнет целиком, и в ответ придёт ошибка
// вместо данных.
func capLimit(a map[string]any, max int) map[string]any {
	if a == nil {
		return map[string]any{}
	}
	if n, ok := toInt(a["limit"]); ok && n > max {
		a["limit"] = max
	}
	return a
}

// clampLimit держит limit в границах, которые принимает метод.
//
// Границы у Ozon разные и в описании инструмента не всегда очевидны.
// Отказ на «покажи пару отзывов» — плохой ответ на разумную просьбу,
// поэтому значение приводится к ближайшей границе.
func clampLimit(a map[string]any, min, max int) map[string]any {
	if a == nil {
		a = map[string]any{}
	}

	limit, ok := toInt(a["limit"])
	switch {
	case !ok:
		limit = min
	case limit < min:
		limit = min
	case limit > max:
		limit = max
	}
	a["limit"] = limit
	return a
}

// toInt приводит число из JSON к int: любое число приезжает как
// float64, но модель может прислать и строку.
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(n))
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

// stringID приводит одиночный идентификатор к строке.
//
// Та же история, что и со списками в stringList: идентификатор вопроса
// у Ozon — строка, но выглядит как число, и модель, увидев его
// в предыдущем ответе, отправляет число. Ozon отвечает ошибкой типа
// при верном значении.
func stringID(v any) (string, error) {
	switch id := v.(type) {
	case string:
		if strings.TrimSpace(id) == "" {
			return "", fmt.Errorf("пустой идентификатор")
		}
		return id, nil
	case float64:
		return strconv.FormatInt(int64(id), 10), nil
	case nil:
		return "", fmt.Errorf("идентификатор не задан")
	default:
		return "", fmt.Errorf("идентификатор должен быть строкой или числом, получено %T", v)
	}
}

// withRecentPeriod достраивает filter.since/filter.to за последние
// days дней.
//
// Методы отправлений требуют период обязательно, а вопрос к ним чаще
// всего звучит как «что там с заказами» — без дат вообще. Отказывать
// на это ошибкой значит заставлять модель угадывать формат RFC3339
// со второй попытки; ответ за последнюю неделю ближе к тому, что
// человек имел в виду.
func withRecentPeriod(a map[string]any, days int) map[string]any {
	if a == nil {
		a = map[string]any{}
	}

	filter, _ := a["filter"].(map[string]any)
	if filter == nil {
		filter = map[string]any{}
	}

	since, hasSince := filter["since"]
	to, hasTo := filter["to"]
	if (hasSince && since != nil && since != "") || (hasTo && to != nil && to != "") {
		// Хотя бы одна граница задана — вторую не додумываем: Ozon
		// сам скажет, что не так, и это честнее нашей догадки.
		a["filter"] = filter
		return a
	}

	now := time.Now()
	filter["since"] = now.AddDate(0, 0, -days).Format(time.RFC3339)
	filter["to"] = now.Format(time.RFC3339)
	a["filter"] = filter
	return a
}
