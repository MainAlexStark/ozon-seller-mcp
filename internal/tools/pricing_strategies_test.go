package tools

import (
	"net/http"
	"testing"

	"github.com/MainAlexStark/ozon-seller-mcp/ozon"
)

func TestStrategyListsFillPage(t *testing.T) {
	// Без page методы стратегий отвечают ошибкой валидации, хотя
	// первая страница — единственное разумное значение.
	var body map[string]any
	_, server, closeFn := fakeOzon(t, ModeReadOnly, captureBody(t, &body))
	defer closeFn()

	for _, tool := range []string{"ozon_pricing_strategies_list", "ozon_pricing_competitors"} {
		if _, isErr := callTool(t, server, tool, map[string]any{}); isErr {
			t.Fatalf("%s: вызов без аргументов должен проходить", tool)
		}
		if body["page"] != float64(1) {
			t.Errorf("%s: страница не достроена: %v", tool, body)
		}
		if body["limit"] != float64(maxStrategiesPerPage) {
			t.Errorf("%s: лимит не достроен: %v", tool, body)
		}
	}

	// Ozon принимает не больше 50 на страницу.
	callTool(t, server, "ozon_pricing_strategies_list", map[string]any{"limit": 500})
	if body["limit"] != float64(maxStrategiesPerPage) {
		t.Errorf("лимит должен опускаться до %d, отправлено %v", maxStrategiesPerPage, body["limit"])
	}
}

func TestStrategyIDGoesAsString(t *testing.T) {
	var body map[string]any
	_, server, closeFn := fakeOzon(t, ModeReadOnly, captureBody(t, &body))
	defer closeFn()

	if _, isErr := callTool(t, server, "ozon_pricing_strategy_info", map[string]any{
		"strategy_id": 4242,
	}); isErr {
		t.Fatal("числовой идентификатор должен приниматься")
	}
	if body["strategy_id"] != "4242" {
		t.Errorf("идентификатор ушёл не строкой: %T (%v)", body["strategy_id"], body["strategy_id"])
	}
}

func TestStrategyCreateChecksCoefficient(t *testing.T) {
	// Коэффициент — единственное число, которым стратегия управляет
	// ценой, и применяет она его сама, снова и снова. Ошибка в нём
	// работает постоянно, а не один раз.
	var reached bool
	_, server, closeFn := fakeOzon(t, ModeWrite, func(w http.ResponseWriter, r *http.Request) {
		reached = true
		_, _ = w.Write([]byte(`{"result":{}}`))
	})
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_pricing_strategy_create", map[string]any{
		"strategy_name": "Догонять маркетплейсы",
		"competitors":   []any{map[string]any{"competitor_id": 1, "coefficient": 0.2}},
	})
	if !isErr {
		t.Fatal("коэффициент вне диапазона должен отклоняться до отправки")
	}
	if reached {
		t.Error("запрос не должен был уйти в Ozon")
	}
	if !contains(body, "0.5") || !contains(body, "1.2") {
		t.Errorf("сообщение должно называть допустимый диапазон: %s", body)
	}

	if _, isErr = callTool(t, server, "ozon_pricing_strategy_create", map[string]any{
		"strategy_name": "Догонять маркетплейсы",
		"competitors":   []any{map[string]any{"competitor_id": 1, "coefficient": 0.95}},
	}); isErr {
		t.Errorf("коэффициент внутри диапазона должен приниматься: %s", body)
	}
	if !reached {
		t.Error("верный запрос должен уходить в Ozon")
	}
}

func TestStrategyCreateNeedsCompetitors(t *testing.T) {
	_, server, closeFn := fakeOzon(t, ModeWrite, func(w http.ResponseWriter, r *http.Request) {
		t.Error("запрос не должен был уйти в Ozon")
	})
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_pricing_strategy_create", map[string]any{
		"strategy_name": "Пустая",
		"competitors":   []any{},
	})
	if !isErr {
		t.Fatal("стратегия без конкурентов должна отклоняться")
	}
	if !contains(body, "ozon_pricing_competitors") {
		t.Errorf("сообщение должно подсказывать, где взять конкурентов: %s", body)
	}
}

func TestStrategyProductsLimitBatch(t *testing.T) {
	var reached bool
	_, server, closeFn := fakeOzon(t, ModeWrite, func(w http.ResponseWriter, r *http.Request) {
		reached = true
	})
	defer closeFn()

	ids := make([]any, maxStrategyProducts+1)
	for i := range ids {
		ids[i] = i + 1
	}

	body, isErr := callTool(t, server, "ozon_pricing_strategy_products_add", map[string]any{
		"strategy_id": "s1", "product_id": ids,
	})
	if !isErr {
		t.Fatal("превышение размера пачки должно отклоняться")
	}
	if reached {
		t.Error("запрос не должен был уйти в Ozon")
	}
	if !contains(body, "50") {
		t.Errorf("сообщение должно называть предел: %s", body)
	}
}

func TestStrategyProductsDeleteNeedsNoStrategyID(t *testing.T) {
	// Товар состоит не более чем в одной стратегии, поэтому удаление
	// принимает только список товаров.
	var body map[string]any
	_, server, closeFn := fakeOzon(t, ModeWrite, captureBody(t, &body))
	defer closeFn()

	if _, isErr := callTool(t, server, "ozon_pricing_strategy_products_delete", map[string]any{
		"product_id": []any{111, 222},
	}); isErr {
		t.Fatal("вызов без strategy_id должен проходить")
	}
	ids, ok := body["product_id"].([]any)
	if !ok || len(ids) != 2 {
		t.Fatalf("товары не отправлены: %v", body)
	}
	if body["strategy_id"] != nil {
		t.Errorf("этот метод идентификатор стратегии не принимает: %v", body)
	}
}

func TestStrategyStatusNeedsExplicitFlag(t *testing.T) {
	// enabled без значения по умолчанию: и включение, и остановка —
	// осознанные действия, а тихая подстановка одного из них меняет
	// поведение живых цен.
	_, server, closeFn := fakeOzon(t, ModeWrite, func(w http.ResponseWriter, r *http.Request) {
		t.Error("запрос не должен был уйти в Ozon")
	})
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_pricing_strategy_status", map[string]any{
		"strategy_id": "s1",
	})
	if !isErr {
		t.Fatal("без enabled вызов должен отклоняться")
	}
	if !contains(body, "enabled") {
		t.Errorf("сообщение должно называть поле: %s", body)
	}
}

func TestStrategyWritesNeedWriteMode(t *testing.T) {
	_, server, closeFn := fakeOzon(t, ModeReadOnly, func(w http.ResponseWriter, r *http.Request) {
		t.Error("запрос не должен был уйти в Ozon")
	})
	defer closeFn()

	cases := []struct {
		tool string
		args map[string]any
	}{
		{"ozon_pricing_strategy_create", map[string]any{
			"strategy_name": "x",
			"competitors":   []any{map[string]any{"competitor_id": 1, "coefficient": 1.0}},
		}},
		{"ozon_pricing_strategy_products_add", map[string]any{"strategy_id": "s1", "product_id": []any{1}}},
		{"ozon_pricing_strategy_products_delete", map[string]any{"product_id": []any{1}}},
		{"ozon_pricing_strategy_status", map[string]any{"strategy_id": "s1", "enabled": false}},
	}

	for _, c := range cases {
		if _, isErr := callTool(t, server, c.tool, c.args); !isErr {
			t.Errorf("%s должен отказывать в режиме чтения", c.tool)
		}
	}
}

func TestPromotionPathsAreProbedBySelftest(t *testing.T) {
	probed := map[string]bool{}
	for _, p := range probes() {
		probed[p.Path] = true
	}

	for _, path := range []string{
		ozon.PathActionsList,
		ozon.PathStrategyList,
		ozon.PathStrategyCompetitors,
	} {
		if !probed[path] {
			t.Errorf("путь %s не проверяется в ozon_api_selftest", path)
		}
	}

	// Список акций должен проверяться именно GET: перепутанный глагол
	// даёт тот же 404, что и отключённая версия метода.
	for _, p := range probes() {
		if p.Path == ozon.PathActionsList && !p.Get {
			t.Error("список акций должен проверяться GET-запросом")
		}
	}
}
