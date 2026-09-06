package tools

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
