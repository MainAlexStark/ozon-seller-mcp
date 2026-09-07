package httpx

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MainAlexStark/ozon-seller-mcp/internal/mcp"
	"github.com/MainAlexStark/ozon-seller-mcp/internal/tools"
	"github.com/MainAlexStark/ozon-seller-mcp/ozon"
)

const (
	readToken  = "r000000000000000000000000000000000000000000000000000000000000000"
	writeToken = "w000000000000000000000000000000000000000000000000000000000000000"
)

// testServer поднимает подставной Ozon и сетевой MCP поверх него.
func testServer(t *testing.T, ozonHandler http.HandlerFunc) (*httptest.Server, func()) {
	t.Helper()

	fakeOzon := httptest.NewServer(ozonHandler)
	client := ozon.New("cid", "key", ozon.WithBaseURL(fakeOzon.URL))

	m := mcp.NewServer("test", "0.0.1")
	reg := tools.NewRegistry(client, tools.DefaultSafety(), m)
	reg.RegisterCatalog()
	reg.RegisterPricing()
	reg.RegisterAnalytics()
	reg.RegisterDiagnostics()

	auth := Auth{Static: NewStaticAuth(readToken, writeToken)}

	s, err := NewServer(m, auth, Config{Safety: tools.DefaultSafety()})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(s.Handler())

	return front, func() {
		front.Close()
		fakeOzon.Close()
	}
}

// rpc отправляет JSON-RPC сообщение по HTTP.
func rpc(t *testing.T, base, path, token, method string, params any) (*http.Response, string) {
	t.Helper()

	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	raw, _ := json.Marshal(body)

	url := base + path
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	raw2, _ := io.ReadAll(resp.Body)
	return resp, string(raw2)
}

// toolCall — параметры вызова инструмента.
func toolCall(name string, args any) map[string]any {
	return map[string]any{"name": name, "arguments": args}
}

// isToolError разбирает ответ и говорит, вернул ли инструмент ошибку.
func isToolError(t *testing.T, body string) (bool, string) {
	t.Helper()

	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("ответ не разобрался: %v\n%s", err, body)
	}
	text := ""
	if len(resp.Result.Content) > 0 {
		text = resp.Result.Content[0].Text
	}
	return resp.Result.IsError, text
}

func TestNoTokenRejected(t *testing.T) {
	front, closeFn := testServer(t, func(w http.ResponseWriter, r *http.Request) {})
	defer closeFn()

	resp, _ := rpc(t, front.URL, "/mcp", "", "tools/list", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("без токена ожидался 401, получен %d", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Error("на 401 должен возвращаться WWW-Authenticate")
	}
}

func TestWrongTokenRejected(t *testing.T) {
	front, closeFn := testServer(t, func(w http.ResponseWriter, r *http.Request) {})
	defer closeFn()

	resp, _ := rpc(t, front.URL, "/mcp", strings.Repeat("x", 64), "tools/list", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("с чужим токеном ожидался 401, получен %d", resp.StatusCode)
	}
}

func TestTokenInPathRejected(t *testing.T) {
	// Раньше так подключался телефон. Спецификация MCP это запрещает:
	// адрес с токеном оседает в журналах прокси и истории браузера.
	// Теперь для телефона есть OAuth, а этот путь закрыт.
	front, closeFn := testServer(t, func(w http.ResponseWriter, r *http.Request) {})
	defer closeFn()

	resp, _ := rpc(t, front.URL, "/mcp/"+readToken, "", "tools/list", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("токен в адресе больше не должен приниматься, получен %d", resp.StatusCode)
	}
}

func TestReadTokenCannotWrite(t *testing.T) {
	// Центральная проверка всей затеи: телефон с читающим токеном
	// не должен уметь менять цены, даже если очень попросить.
	var ozonTouched bool

	front, closeFn := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		ozonTouched = true
		_, _ = w.Write([]byte(`{"result":"ok"}`))
	})
	defer closeFn()

	resp, body := rpc(t, front.URL, "/mcp", readToken, "tools/call",
		toolCall("ozon_prices_update", map[string]any{
			"prices": []map[string]any{{"offer_id": "pp-1", "price": "100"}},
		}))

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ожидался 200 с ошибкой инструмента, получен %d", resp.StatusCode)
	}

	isErr, text := isToolError(t, body)
	if !isErr {
		t.Fatal("читающий токен не должен допускаться к записи")
	}
	if ozonTouched {
		t.Fatal("запрос не должен был дойти до Ozon")
	}
	if !strings.Contains(text, "прав на запись нет") {
		t.Errorf("сообщение должно объяснять причину: %s", text)
	}
}

func TestWriteTokenCanWrite(t *testing.T) {
	var wrote bool

	front, closeFn := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case ozon.PathPricesInfo:
			_, _ = w.Write([]byte(`{"items":[{"offer_id":"pp-1","product_id":1,"price":{"price":"1000","old_price":"0"}}]}`))
		case ozon.PathPricesImport:
			wrote = true
			_, _ = w.Write([]byte(`{"result":[{"offer_id":"pp-1","updated":true}]}`))
		}
	})
	defer closeFn()

	_, body := rpc(t, front.URL, "/mcp", writeToken, "tools/call",
		toolCall("ozon_prices_update", map[string]any{
			"prices": []map[string]any{{"offer_id": "pp-1", "price": "1100"}},
		}))

	if isErr, text := isToolError(t, body); isErr {
		t.Fatalf("пишущий токен должен допускаться к записи: %s", text)
	}
	if !wrote {
		t.Fatal("запись должна была уйти в Ozon")
	}
}

func TestPriceGuardStillAppliesToWriteToken(t *testing.T) {
	// Пишущий токен даёт право менять, но не отменяет страховку
	// от потерянного нуля.
	var wrote bool

	front, closeFn := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case ozon.PathPricesInfo:
			_, _ = w.Write([]byte(`{"items":[{"offer_id":"pp-1","product_id":1,"price":{"price":"6900","old_price":"0"}}]}`))
		case ozon.PathPricesImport:
			wrote = true
			_, _ = w.Write([]byte(`{"result":[]}`))
		}
	})
	defer closeFn()

	_, body := rpc(t, front.URL, "/mcp", writeToken, "tools/call",
		toolCall("ozon_prices_update", map[string]any{
			"prices": []map[string]any{{"offer_id": "pp-1", "price": "690"}},
		}))

	isErr, text := isToolError(t, body)
	if !isErr {
		t.Fatal("падение цены в 10 раз должно останавливаться и по сети")
	}
	if wrote {
		t.Fatal("запись не должна была уйти в Ozon")
	}
	if !strings.Contains(text, "confirm_large_change") {
		t.Errorf("сообщение должно объяснять, как подтвердить: %s", text)
	}
}

func TestStatusReportsScopeOfConnection(t *testing.T) {
	front, closeFn := testServer(t, func(w http.ResponseWriter, r *http.Request) {})
	defer closeFn()

	_, readBody := rpc(t, front.URL, "/mcp", readToken, "tools/call", toolCall("ozon_status", map[string]any{}))
	_, _ = isToolError(t, readBody)
	if !strings.Contains(readBody, "read-only") {
		t.Errorf("для читающего токена статус должен показывать read-only: %s", readBody)
	}

	_, writeBody := rpc(t, front.URL, "/mcp", writeToken, "tools/call", toolCall("ozon_status", map[string]any{}))
	if !strings.Contains(writeBody, "write") {
		t.Errorf("для пишущего токена статус должен показывать write: %s", writeBody)
	}
}

func TestForeignOriginRejected(t *testing.T) {
	// Защита от DNS rebinding: сайт в браузере не должен ходить
	// к серверу от имени пользователя.
	front, closeFn := testServer(t, func(w http.ResponseWriter, r *http.Request) {})
	defer closeFn()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+readToken)
	req.Header.Set("Origin", "https://evil.example")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("чужой Origin должен отклоняться, получен %d", resp.StatusCode)
	}
}

func TestNotificationGets202(t *testing.T) {
	front, closeFn := testServer(t, func(w http.ResponseWriter, r *http.Request) {})
	defer closeFn()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	req.Header.Set("Authorization", "Bearer "+readToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("на уведомление ожидался 202, получен %d", resp.StatusCode)
	}
}

func TestAcceptMustNameBothFormats(t *testing.T) {
	// Спецификация требует от клиента готовности принять оба формата.
	// Прежняя реализация была снисходительнее и отвечала потоком
	// событий тому, кто просил только его; SDK такие запросы отклоняет.
	// Для автоматизации это значит: заголовки обязательны — см.
	// docs/REMOTE.md.
	front, closeFn := testServer(t, func(w http.ResponseWriter, r *http.Request) {})
	defer closeFn()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+readToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("неполный Accept должен отклоняться, получен %d", resp.StatusCode)
	}
}

func TestHostFromReverseProxyIsAccepted(t *testing.T) {
	// Штатная схема развёртывания: Caddy на 443 и сервер на localhost.
	// Запрос приходит на петлевой адрес с внешним Host — встроенная
	// в SDK защита от DNS rebinding отклонила бы его, и сервер за
	// прокси перестал бы работать целиком.
	front, closeFn := testServer(t, func(w http.ResponseWriter, r *http.Request) {})
	defer closeFn()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+readToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Host = "ozon-mcp.example.com"

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("запрос из-за обратного прокси должен проходить, получен %d: %s", resp.StatusCode, body)
	}
}

func TestGetIsNotAllowed(t *testing.T) {
	front, closeFn := testServer(t, func(w http.ResponseWriter, r *http.Request) {})
	defer closeFn()

	req, _ := http.NewRequest(http.MethodGet, front.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+readToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET должен отвечать 405, получен %d", resp.StatusCode)
	}
}

func TestHealthNeedsNoToken(t *testing.T) {
	front, closeFn := testServer(t, func(w http.ResponseWriter, r *http.Request) {})
	defer closeFn()

	resp, err := http.Get(front.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz должен отвечать без токена, получен %d", resp.StatusCode)
	}
}
