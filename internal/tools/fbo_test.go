package tools

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MainAlexStark/ozon-seller-mcp/ozon"
)

// captureBody поднимает подставной Ozon, запоминает тело запроса
// и отвечает пустым результатом.
func captureBody(t *testing.T, got *map[string]any) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		*got = body
		_, _ = w.Write([]byte(`{"result":{}}`))
	}
}

func TestFBOPostingsGetPeriodWhenNoneAsked(t *testing.T) {
	// «Покажи заказы FBO» без дат — самый частый вопрос. Метод требует
	// период обязательно, поэтому его достраивает сервер.
	var body map[string]any
	_, server, closeFn := fakeOzon(t, ModeReadOnly, captureBody(t, &body))
	defer closeFn()

	if _, isErr := callTool(t, server, "ozon_fbo_postings_list", map[string]any{}); isErr {
		t.Fatal("вызов без периода должен проходить")
	}

	filter, ok := body["filter"].(map[string]any)
	if !ok {
		t.Fatalf("filter не отправлен: %v", body)
	}

	since, _ := filter["since"].(string)
	to, _ := filter["to"].(string)
	if since == "" || to == "" {
		t.Fatalf("границы периода не проставлены: %v", filter)
	}

	from, err := time.Parse(time.RFC3339, since)
	if err != nil {
		t.Fatalf("since не в формате RFC3339: %q", since)
	}
	if days := time.Since(from).Hours() / 24; days < 6.5 || days > 7.5 {
		t.Errorf("ожидалась неделя, получено %.1f дней", days)
	}
	if body["limit"] == nil {
		t.Error("лимит должен проставляться по умолчанию")
	}
}

func TestFBOPostingsKeepAskedPeriod(t *testing.T) {
	// Заданные даты не трогаем: помощь не должна молча переопределять
	// то, о чём попросили явно.
	var body map[string]any
	_, server, closeFn := fakeOzon(t, ModeReadOnly, captureBody(t, &body))
	defer closeFn()

	since := "2026-08-01T00:00:00Z"
	callTool(t, server, "ozon_fbo_postings_list", map[string]any{
		"filter": map[string]any{"since": since, "to": "2026-08-31T00:00:00Z"},
	})

	filter, _ := body["filter"].(map[string]any)
	if filter["since"] != since {
		t.Errorf("since подменён: %v", filter["since"])
	}
}

func TestSupplyOrderIdsBecomeStrings(t *testing.T) {
	// Идентификаторы заявок выглядят как числа, и модель отправляет
	// числа. Ozon ждёт строки — приводим до отправки, иначе ответ
	// звучит как «invalid type» при верном значении.
	var body map[string]any
	_, server, closeFn := fakeOzon(t, ModeReadOnly, captureBody(t, &body))
	defer closeFn()

	if _, isErr := callTool(t, server, "ozon_supply_order_get", map[string]any{
		"order_ids": []any{123456, "789"},
	}); isErr {
		t.Fatal("числовые идентификаторы должны приниматься")
	}

	ids, ok := body["order_ids"].([]any)
	if !ok || len(ids) != 2 {
		t.Fatalf("order_ids не отправлены: %v", body)
	}
	for _, id := range ids {
		if _, isString := id.(string); !isString {
			t.Errorf("идентификатор ушёл не строкой: %T (%v)", id, id)
		}
	}
	if ids[0] != "123456" {
		t.Errorf("число преобразовано неверно: %v", ids[0])
	}
}

func TestSupplyOrderGetRejectsEmptyList(t *testing.T) {
	var reached bool
	_, server, closeFn := fakeOzon(t, ModeReadOnly, func(w http.ResponseWriter, r *http.Request) {
		reached = true
	})
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_supply_order_get", map[string]any{"order_ids": []any{}})
	if !isErr {
		t.Fatal("пустой список идентификаторов должен отклоняться")
	}
	if reached {
		t.Error("запрос не должен был уйти в Ozon")
	}
	if !strings.Contains(body, "order_ids") {
		t.Errorf("сообщение должно называть поле: %s", body)
	}
}

func TestFBOStocksDefaultWarehouseType(t *testing.T) {
	var body map[string]any
	_, server, closeFn := fakeOzon(t, ModeReadOnly, captureBody(t, &body))
	defer closeFn()

	callTool(t, server, "ozon_fbo_stocks", map[string]any{})

	if body["warehouse_type"] != "ALL" {
		t.Errorf("по умолчанию нужны все склады, отправлено %v", body["warehouse_type"])
	}
	if body["limit"] == nil {
		t.Error("лимит должен проставляться по умолчанию")
	}
}

func TestFBOToolsAreReadOnly(t *testing.T) {
	// Ни один инструмент FBO ничего не меняет, поэтому все они обязаны
	// работать в режиме только для чтения. Если однажды здесь появится
	// запись, этот тест напомнит поставить ей заслон.
	_, server, closeFn := fakeOzon(t, ModeReadOnly, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"result":{}}`))
	})
	defer closeFn()

	cases := []struct {
		tool string
		args map[string]any
	}{
		{"ozon_fbo_postings_list", map[string]any{}},
		{"ozon_fbo_posting_get", map[string]any{"posting_number": "1234-0001-1"}},
		{"ozon_fbo_stocks", map[string]any{}},
		{"ozon_supply_orders_list", map[string]any{}},
		{"ozon_supply_order_get", map[string]any{"order_ids": []any{"1"}}},
		{"ozon_supply_order_items", map[string]any{"bundle_ids": []any{"b1"}}},
		{"ozon_supply_orders_counters", map[string]any{}},
		{"ozon_supply_timeslots", map[string]any{"order_id": 1}},
		{"ozon_fbo_clusters", map[string]any{}},
	}

	for _, c := range cases {
		if body, isErr := callTool(t, server, c.tool, c.args); isErr {
			t.Errorf("%s не работает в режиме чтения: %s", c.tool, body)
		}
	}
}

func TestFBOPathsAreProbedBySelftest(t *testing.T) {
	// Пути FBO собраны из документации, но проверить их можно только
	// живым ключом. Самодиагностика — единственное место, где это
	// произойдёт, поэтому каждый новый путь обязан там быть.
	probed := map[string]bool{}
	for _, p := range probes() {
		probed[p.Path] = true
	}

	for _, path := range []string{
		ozon.PathPostingFBOList,
		ozon.PathStockOnWarehouses,
		ozon.PathSupplyOrderList,
		ozon.PathSupplyOrderCounters,
		ozon.PathClusterList,
	} {
		if !probed[path] {
			t.Errorf("путь %s не проверяется в ozon_api_selftest", path)
		}
	}
}
