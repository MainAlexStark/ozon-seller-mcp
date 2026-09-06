package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MainAlexStark/ozon-seller-mcp/internal/mcp"
	"github.com/MainAlexStark/ozon-seller-mcp/ozon"
)

// fakeOzon поднимает подставной Seller API и возвращает реестр,
// собранный поверх него. Тесты ниже проходят весь путь целиком:
// вызов инструмента -> защита -> клиент -> HTTP.
func fakeOzon(t *testing.T, mode Mode, handler http.HandlerFunc) (*Registry, *mcp.Server, func()) {
	t.Helper()

	srv := httptest.NewServer(handler)
	client := ozon.New("cid", "key", ozon.WithBaseURL(srv.URL))

	safety := DefaultSafety()
	safety.Mode = mode

	server := mcp.NewServer("test", "0.0.1")
	reg := NewRegistry(client, safety, server)
	reg.RegisterCatalog()
	reg.RegisterPricing()
	reg.RegisterAnalytics()
	reg.RegisterDiagnostics()

	return reg, server, srv.Close
}

// callTool вызывает инструмент напрямую через протокол MCP.
func callTool(t *testing.T, server *mcp.Server, name string, args any) (string, bool) {
	t.Helper()

	argsJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("аргументы: %v", err)
	}
	req := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": json.RawMessage(argsJSON)},
	}
	line, _ := json.Marshal(req)

	var out strings.Builder
	if err := server.Serve(context.Background(), strings.NewReader(string(line)+"\n"), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &resp); err != nil {
		t.Fatalf("ответ не разобрался: %v\n%s", err, out.String())
	}
	if len(resp.Result.Content) == 0 {
		t.Fatalf("пустой ответ: %s", out.String())
	}
	return resp.Result.Content[0].Text, resp.Result.IsError
}

func TestAllToolsRegisteredWithoutCollisions(t *testing.T) {
	_, server, closeFn := fakeOzon(t, ModeReadOnly, func(w http.ResponseWriter, r *http.Request) {})
	defer closeFn()

	names := server.ToolNames()
	if len(names) < 20 {
		t.Errorf("ожидалось не меньше 20 инструментов, зарегистрировано %d", len(names))
	}

	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Errorf("дубликат инструмента: %s", n)
		}
		seen[n] = true
		if !strings.HasPrefix(n, "ozon_") {
			t.Errorf("инструмент %s без общего префикса ozon_", n)
		}
	}
}

func TestReadToolWorksInReadOnlyMode(t *testing.T) {
	_, server, closeFn := fakeOzon(t, ModeReadOnly, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"result":{"items":[{"offer_id":"pp-1"}]}}`))
	})
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_product_list", map[string]any{})
	if isErr {
		t.Fatalf("чтение должно работать в режиме read-only: %s", body)
	}
	if !strings.Contains(body, "pp-1") {
		t.Errorf("данные не доехали: %s", body)
	}
}

func TestWriteToolBlockedInReadOnlyMode(t *testing.T) {
	var reached bool
	_, server, closeFn := fakeOzon(t, ModeReadOnly, func(w http.ResponseWriter, r *http.Request) {
		reached = true
		_, _ = w.Write([]byte(`{"result":"ok"}`))
	})
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_prices_update", map[string]any{
		"prices": []map[string]any{{"offer_id": "pp-1", "price": "100"}},
	})

	if !isErr {
		t.Fatal("запись в режиме чтения должна отклоняться")
	}
	if reached {
		t.Fatal("запрос не должен был дойти до Ozon")
	}
	if !strings.Contains(body, "OZON_ALLOW_WRITES") {
		t.Errorf("сообщение должно объяснять, как включить запись: %s", body)
	}
}

func TestPriceGuardStopsOrderOfMagnitudeTypoEndToEnd(t *testing.T) {
	var wrote bool

	_, server, closeFn := fakeOzon(t, ModeWrite, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case ozon.PathPricesInfo:
			// Текущая цена 6900.
			_, _ = w.Write([]byte(`{"items":[{"offer_id":"pp-1","product_id":1,"price":{"price":"6900","old_price":"0"}}]}`))
		case ozon.PathPricesImport:
			wrote = true
			_, _ = w.Write([]byte(`{"result":[{"offer_id":"pp-1","updated":true}]}`))
		}
	})
	defer closeFn()

	// Потерян ноль: 6900 -> 690.
	body, isErr := callTool(t, server, "ozon_prices_update", map[string]any{
		"prices": []map[string]any{{"offer_id": "pp-1", "price": "690"}},
	})

	if !isErr {
		t.Fatal("падение цены в 10 раз должно останавливать запись")
	}
	if wrote {
		t.Fatal("запись не должна была уйти в Ozon")
	}
	if !strings.Contains(body, "6900") || !strings.Contains(body, "690") {
		t.Errorf("сообщение должно показывать старую и новую цену: %s", body)
	}
}

func TestPriceGuardPassesWithConfirmation(t *testing.T) {
	var wrote bool

	_, server, closeFn := fakeOzon(t, ModeWrite, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case ozon.PathPricesInfo:
			_, _ = w.Write([]byte(`{"items":[{"offer_id":"pp-1","product_id":1,"price":{"price":"6900","old_price":"0"}}]}`))
		case ozon.PathPricesImport:
			wrote = true
			_, _ = w.Write([]byte(`{"result":[{"offer_id":"pp-1","updated":true}]}`))
		}
	})
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_prices_update", map[string]any{
		"prices":               []map[string]any{{"offer_id": "pp-1", "price": "690"}},
		"confirm_large_change": true,
	})

	if isErr {
		t.Fatalf("подтверждённое изменение должно проходить: %s", body)
	}
	if !wrote {
		t.Fatal("запись должна была уйти в Ozon")
	}
}

func TestPriceGuardAllowsNormalChange(t *testing.T) {
	var wrote bool

	_, server, closeFn := fakeOzon(t, ModeWrite, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case ozon.PathPricesInfo:
			_, _ = w.Write([]byte(`{"items":[{"offer_id":"pp-1","product_id":1,"price":{"price":"1000","old_price":"0"}}]}`))
		case ozon.PathPricesImport:
			wrote = true
			_, _ = w.Write([]byte(`{"result":[{"offer_id":"pp-1","updated":true}]}`))
		}
	})
	defer closeFn()

	// +15 % — обычная корректировка.
	if _, isErr := callTool(t, server, "ozon_prices_update", map[string]any{
		"prices": []map[string]any{{"offer_id": "pp-1", "price": "1150"}},
	}); isErr {
		t.Fatal("изменение на 15 % должно проходить без подтверждения")
	}
	if !wrote {
		t.Fatal("запись должна была уйти в Ozon")
	}
}

func TestFinancePeriodLimitCheckedLocally(t *testing.T) {
	var reached bool
	_, server, closeFn := fakeOzon(t, ModeReadOnly, func(w http.ResponseWriter, r *http.Request) {
		reached = true
		_, _ = w.Write([]byte(`{"result":{}}`))
	})
	defer closeFn()

	// Три месяца при лимите в один.
	body, isErr := callTool(t, server, "ozon_finance_by_day", map[string]any{
		"date_from": "2026-01-01",
		"date_to":   "2026-04-01",
	})

	if !isErr {
		t.Fatal("период больше месяца должен отклоняться до запроса")
	}
	if reached {
		t.Error("запрос не должен был уйти в Ozon")
	}
	if !strings.Contains(body, "месяц") {
		t.Errorf("сообщение должно объяснять ограничение периода: %s", body)
	}
}

func TestSwappedDatesCaught(t *testing.T) {
	_, server, closeFn := fakeOzon(t, ModeReadOnly, func(w http.ResponseWriter, r *http.Request) {})
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_analytics_data", map[string]any{
		"date_from": "2026-09-01",
		"date_to":   "2026-08-01",
		"metrics":   []string{"revenue"},
		"dimension": []string{"sku"},
	})
	if !isErr {
		t.Fatal("перепутанные даты должны отклоняться")
	}
	if !strings.Contains(body, "перепутаны") {
		t.Errorf("сообщение должно называть причину: %s", body)
	}
}

func TestErrorCarriesHint(t *testing.T) {
	_, server, closeFn := fakeOzon(t, ModeReadOnly, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"UNAUTHORIZED","message":"invalid key"}`))
	})
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_product_list", map[string]any{})
	if !isErr {
		t.Fatal("401 должен быть ошибкой")
	}
	if !strings.Contains(body, "OZON_API_KEY") {
		t.Errorf("к ошибке авторизации должна прилагаться подсказка: %s", body)
	}
}

func TestStatusToolMasksClientID(t *testing.T) {
	_, server, closeFn := fakeOzon(t, ModeReadOnly, func(w http.ResponseWriter, r *http.Request) {})
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_status", map[string]any{})
	if isErr {
		t.Fatalf("ozon_status не должен падать: %s", body)
	}
	if strings.Contains(body, "cid") && !strings.Contains(body, "*") {
		t.Errorf("Client-Id должен маскироваться: %s", body)
	}
	if !strings.Contains(body, "read-only") {
		t.Errorf("статус должен показывать режим: %s", body)
	}
}

func TestResponseTrimmed(t *testing.T) {
	big := strings.Repeat("a", 500_000)
	_, server, closeFn := fakeOzon(t, ModeReadOnly, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"result":"` + big + `"}`))
	})
	defer closeFn()

	body, _ := callTool(t, server, "ozon_product_list", map[string]any{})
	if len(body) > 200_000 {
		t.Errorf("ответ должен быть обрезан, получено %d байт", len(body))
	}
	if !strings.Contains(body, "обрезан") {
		t.Error("обрезка должна объясняться в тексте")
	}
}

func TestMaskID(t *testing.T) {
	cases := map[string]string{
		"":         "(не задан)",
		"123":      "***",
		"12345678": "****5678",
	}
	for in, want := range cases {
		if got := maskID(in); got != want {
			t.Errorf("maskID(%q) = %q, want %q", in, got, want)
		}
	}
}

// Ozon требует filter даже когда фильтровать нечего. Раньше этого теста
// не было, и --check падал с 400 ещё до проверки ключей.
func TestRequiredFilterIsSentEvenWhenNotAsked(t *testing.T) {
	type sent struct {
		Filter map[string]any `json:"filter"`
		Limit  int            `json:"limit"`
	}
	var got map[string]sent // путь -> тело

	got = map[string]sent{}
	_, server, closeFn := fakeOzon(t, ModeReadOnly, func(w http.ResponseWriter, r *http.Request) {
		var body sent
		_ = json.NewDecoder(r.Body).Decode(&body)
		got[r.URL.Path] = body
		_, _ = w.Write([]byte(`{"result":{}}`))
	})
	defer closeFn()

	cases := []struct {
		tool string
		path string
	}{
		{"ozon_product_list", ozon.PathProductList},
		{"ozon_product_attributes", ozon.PathProductAttributes},
		{"ozon_prices_info", ozon.PathPricesInfo},
		{"ozon_stocks_info", ozon.PathStocksInfo},
	}

	for _, c := range cases {
		// Вызываем совсем без аргументов — так, как это сделает модель,
		// когда её просто спросили «покажи мои товары».
		if _, isErr := callTool(t, server, c.tool, map[string]any{}); isErr {
			t.Errorf("%s: вызов без аргументов не должен падать", c.tool)
			continue
		}
		body, ok := got[c.path]
		if !ok {
			t.Errorf("%s: запрос не дошёл до %s", c.tool, c.path)
			continue
		}
		if body.Filter == nil {
			t.Errorf("%s: filter не отправлен — Ozon ответит 400", c.tool)
			continue
		}
		if body.Filter["visibility"] != "ALL" {
			t.Errorf("%s: visibility = %v, want ALL", c.tool, body.Filter["visibility"])
		}
		if body.Limit == 0 {
			t.Errorf("%s: лимит не проставлен", c.tool)
		}
	}
}

func TestExplicitFilterNotOverwritten(t *testing.T) {
	var gotVisibility any

	_, server, closeFn := fakeOzon(t, ModeReadOnly, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Filter map[string]any `json:"filter"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotVisibility = body.Filter["visibility"]
		_, _ = w.Write([]byte(`{"result":{}}`))
	})
	defer closeFn()

	callTool(t, server, "ozon_product_list", map[string]any{
		"filter": map[string]any{"visibility": "EMPTY_STOCK"},
	})

	if gotVisibility != "EMPTY_STOCK" {
		t.Errorf("явно заданный visibility перезаписан на %v", gotVisibility)
	}
}

func TestRequestValidationErrorGetsRightHint(t *testing.T) {
	_, server, closeFn := fakeOzon(t, ModeReadOnly, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"BAD_REQUEST","message":"Request validation error: invalid GetProductListRequest.Filter: value is required"}`))
	})
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_product_list", map[string]any{})
	if !isErr {
		t.Fatal("400 должен быть ошибкой")
	}
	// Раньше здесь советовали смотреть характеристики категории —
	// к форме запроса это отношения не имеет и уводило в сторону.
	if strings.Contains(body, "характеристика категории") {
		t.Errorf("ошибка формы запроса не должна советовать смотреть характеристики: %s", body)
	}
	if !strings.Contains(body, "filter") {
		t.Errorf("подсказка должна называть filter: %s", body)
	}
}
