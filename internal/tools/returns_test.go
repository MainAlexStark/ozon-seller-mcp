package tools

import (
	"net/http"
	"testing"
)

func TestReturnsListFillsLimit(t *testing.T) {
	// «Что там с возвратами» — вопрос без единого аргумента, а limit
	// у метода обязателен.
	var body map[string]any
	_, server, closeFn := fakeOzon(t, ModeReadOnly, captureBody(t, &body))
	defer closeFn()

	if _, isErr := callTool(t, server, "ozon_returns_list", map[string]any{}); isErr {
		t.Fatal("вызов без аргументов должен проходить")
	}
	if body["limit"] != float64(defaultReturnsLimit) {
		t.Errorf("без limit должно уходить %d, отправлено %v", defaultReturnsLimit, body["limit"])
	}

	callTool(t, server, "ozon_returns_list", map[string]any{"limit": 5000})
	if body["limit"] != float64(maxReturnsLimit) {
		t.Errorf("limit должен опускаться до %d, отправлено %v", maxReturnsLimit, body["limit"])
	}

	// Меньшее значение — законная просьба, и поднимать его не за чем:
	// нижней границы у метода нет.
	callTool(t, server, "ozon_returns_list", map[string]any{"limit": 2})
	if body["limit"] != float64(2) {
		t.Errorf("маленький limit не должен подменяться: %v", body["limit"])
	}
}

func TestReturnsListRejectsTooManyPostings(t *testing.T) {
	var reached bool
	_, server, closeFn := fakeOzon(t, ModeReadOnly, func(w http.ResponseWriter, r *http.Request) {
		reached = true
	})
	defer closeFn()

	numbers := make([]any, maxReturnPostingNumbers+1)
	for i := range numbers {
		numbers[i] = "0000000000-0000-1"
	}

	body, isErr := callTool(t, server, "ozon_returns_list", map[string]any{
		"filter": map[string]any{"posting_numbers": numbers},
	})
	if !isErr {
		t.Fatal("превышение размера фильтра должно отклоняться до отправки")
	}
	if reached {
		t.Error("запрос не должен был уйти в Ozon")
	}
	if !contains(body, "50") {
		t.Errorf("сообщение должно называть предел: %s", body)
	}
}

func TestReturnsListExplainsSingleFilterRule(t *testing.T) {
	// Ozon принимает одно условие фильтра за вызов и отвечает на два
	// общей ошибкой валидации, из которой правило не следует никак.
	_, server, closeFn := fakeOzon(t, ModeReadOnly, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":3,"message":"invalid argument"}`))
	})
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_returns_list", map[string]any{
		"filter": map[string]any{
			"offer_id":             "pp-1",
			"logistic_return_date": map[string]any{"time_from": "2026-08-01T00:00:00Z", "time_to": "2026-08-31T00:00:00Z"},
		},
	})
	if !isErr {
		t.Fatal("отказ Ozon должен доезжать как ошибка инструмента")
	}
	if !contains(body, "только одно") {
		t.Errorf("подсказка про одно условие не показана: %s", body)
	}
	if !contains(body, "offer_id") || !contains(body, "logistic_return_date") {
		t.Errorf("подсказка должна называть конфликтующие условия: %s", body)
	}
}

func TestReturnsListKeepsSilentOnSingleFilter(t *testing.T) {
	// Одно условие — значит, дело не в правиле, и придумывать
	// объяснение нельзя: подсказка не из того класса стоит вечера.
	_, server, closeFn := fakeOzon(t, ModeReadOnly, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":3,"message":"invalid argument"}`))
	})
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_returns_list", map[string]any{
		"filter": map[string]any{"offer_id": "pp-1"},
	})
	if !isErr {
		t.Fatal("отказ должен доезжать как ошибка")
	}
	if contains(body, "только одно") {
		t.Errorf("подсказка не должна появляться при одном условии: %s", body)
	}
}

func TestReturnsDropoffBuildsNestedRequest(t *testing.T) {
	// Метод ждёт две вложенные структуры, а модель присылает плоские
	// аргументы — форму собирает инструмент.
	var body map[string]any
	_, server, closeFn := fakeOzon(t, ModeReadOnly, captureBody(t, &body))
	defer closeFn()

	if _, isErr := callTool(t, server, "ozon_returns_dropoff_points", map[string]any{"place_id": 42}); isErr {
		t.Fatal("вызов должен проходить")
	}

	filter, ok := body["filter"].(map[string]any)
	if !ok || filter["place_id"] != float64(42) {
		t.Fatalf("place_id должен уходить внутри filter: %v", body)
	}
	pagination, ok := body["pagination"].(map[string]any)
	if !ok || pagination["limit"] == nil {
		t.Fatalf("постраничность должна уходить отдельной структурой: %v", body)
	}
	if body["place_id"] != nil || body["limit"] != nil {
		t.Errorf("плоские аргументы не должны уходить в Ozon: %v", body)
	}
}

func TestReturnsToolsAreReadOnly(t *testing.T) {
	// Ни один инструмент возвратов ничего не меняет. Если однажды
	// здесь появится запись — например, решение по спору, — этот тест
	// напомнит поставить ей заслон.
	_, server, closeFn := fakeOzon(t, ModeReadOnly, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"result":{}}`))
	})
	defer closeFn()

	for _, tool := range []string{"ozon_returns_list", "ozon_returns_dropoff_points"} {
		if body, isErr := callTool(t, server, tool, map[string]any{}); isErr {
			t.Errorf("%s не работает в режиме чтения: %s", tool, body)
		}
	}
}
