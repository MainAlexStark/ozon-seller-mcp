package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MainAlexStark/ozon-seller-mcp/internal/mcp"
	"github.com/MainAlexStark/ozon-seller-mcp/internal/oauth"
	"github.com/MainAlexStark/ozon-seller-mcp/internal/secure"
	"github.com/MainAlexStark/ozon-seller-mcp/internal/tools"
	"github.com/MainAlexStark/ozon-seller-mcp/ozon"
)

const (
	readToken  = "osm_read0000000000000000000000000000000000000000000"
	writeToken = "osm_write000000000000000000000000000000000000000000"
	// otherShopToken — токен второго пользователя к его магазину.
	otherShopToken = "osm_other000000000000000000000000000000000000000000"
)

// fakeShops — магазины теста: у каждого свой подставной Ozon.
type fakeShops struct {
	clients map[[2]int64]*ozon.Client
}

func (f *fakeShops) Client(_ context.Context, userID, shopID int64) (*ozon.Client, error) {
	c, ok := f.clients[[2]int64{userID, shopID}]
	if !ok {
		return nil, errors.New("нет такого магазина")
	}
	return c, nil
}

type usageRecord struct {
	user, shop int64
	tool       string
	failed     bool
}

// harness — сетевой MCP поверх подставных магазинов.
type harness struct {
	front *httptest.Server
	oauth *oauth.Server
	store *oauth.MemoryStore

	mu    sync.Mutex
	usage []usageRecord
}

func (h *harness) records() []usageRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]usageRecord(nil), h.usage...)
}

// newHarness поднимает MCP с двумя пользователями: пользователь 1 с
// магазином 10 (ozonHandler) и пользователь 2 с магазином 20
// (otherHandler, может быть nil).
func newHarness(t *testing.T, ozonHandler, otherHandler http.HandlerFunc) *harness {
	t.Helper()

	fakeOzon := httptest.NewServer(ozonHandler)
	t.Cleanup(fakeOzon.Close)
	if otherHandler == nil {
		otherHandler = func(http.ResponseWriter, *http.Request) {}
	}
	otherOzon := httptest.NewServer(otherHandler)
	t.Cleanup(otherOzon.Close)

	shops := &fakeShops{clients: map[[2]int64]*ozon.Client{
		{1, 10}: ozon.New("111", "key-1", ozon.WithBaseURL(fakeOzon.URL)),
		{2, 20}: ozon.New("222", "key-2", ozon.WithBaseURL(otherOzon.URL)),
	}}

	// В сервисном режиме у реестра нет своего клиента: магазин
	// приходит только из запроса.
	m := mcp.NewServer("test", "0.0.1")
	reg := tools.NewRegistry(nil, tools.DefaultSafety(), m)
	reg.RegisterCatalog()
	reg.RegisterPricing()
	reg.RegisterAnalytics()
	reg.RegisterFBO()
	reg.RegisterDiagnostics()

	h := &harness{store: oauth.NewMemoryStore()}

	apiTokens := func(_ context.Context, token string) (Principal, bool) {
		switch token {
		case readToken:
			return Principal{UserID: 1, ShopID: 10, Scope: ScopeRead}, true
		case writeToken:
			return Principal{UserID: 1, ShopID: 10, Scope: ScopeWrite}, true
		case otherShopToken:
			return Principal{UserID: 2, ShopID: 20, Scope: ScopeRead}, true
		}
		return Principal{}, false
	}

	mux := http.NewServeMux()
	front := httptest.NewServer(mux)
	t.Cleanup(front.Close)

	o, err := oauth.New(oauth.Config{Issuer: front.URL, Store: h.store, Accounts: noAccounts{}})
	if err != nil {
		t.Fatal(err)
	}
	h.oauth = o

	usage := func(user, shop int64, tool string, failed bool) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.usage = append(h.usage, usageRecord{user, shop, tool, failed})
	}

	s, err := NewServer(m, Auth{OAuth: o, APITokens: apiTokens}, shops, usage,
		Config{Safety: tools.DefaultSafety()})
	if err != nil {
		t.Fatal(err)
	}
	mux.Handle("/", s.Handler())
	h.front = front
	return h
}

// issueOAuth кладёт в хранилище действующий OAuth-токен.
func (h *harness) issueOAuth(t *testing.T, userID, shopID int64, scopes ...string) string {
	t.Helper()
	token := "oauth-" + strings.Repeat("t", 40) + fmt.Sprint(userID, shopID, len(scopes))
	err := h.store.SaveTokens(context.Background(), &oauth.Token{
		TokenHash: secure.HashToken(token),
		ClientID:  "claude",
		Scopes:    scopes,
		Resource:  h.oauth.ResourceURL(),
		IssuedAt:  time.Now(),
		ExpiresAt: time.Now().Add(time.Hour),
		GrantID:   "g-" + token,
		Subject:   oauth.Subject{UserID: userID, ShopID: shopID},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

type noAccounts struct{}

func (noAccounts) CurrentUser(*http.Request) (oauth.User, bool)       { return oauth.User{}, false }
func (noAccounts) Shops(context.Context, int64) ([]oauth.Shop, error) { return nil, nil }
func (noAccounts) LoginURL(string) string                             { return "/login" }
func (noAccounts) AddShopURL(string) string                           { return "/account" }

// testServer — прежняя обёртка для тестов, которым нужен один магазин.
func testServer(t *testing.T, ozonHandler http.HandlerFunc) (*httptest.Server, func()) {
	t.Helper()
	h := newHarness(t, ozonHandler, nil)
	return h.front, func() {}
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

func TestOAuthTokenReachesItsOwnShop(t *testing.T) {
	// Главное свойство сервиса: запрос уходит в Ozon с ключами того
	// магазина, к которому выдан токен, и ни в какой другой.
	var mine, other bool
	h := newHarness(t,
		func(w http.ResponseWriter, r *http.Request) {
			mine = true
			if r.Header.Get("Client-Id") != "111" {
				t.Errorf("в Ozon ушёл чужой Client-Id: %s", r.Header.Get("Client-Id"))
			}
			_, _ = w.Write([]byte(`{"result":{"items":[],"total":0}}`))
		},
		func(w http.ResponseWriter, r *http.Request) {
			other = true
			_, _ = w.Write([]byte(`{"result":{"items":[],"total":0}}`))
		})

	token := h.issueOAuth(t, 1, 10, oauth.ScopeRead)
	resp, body := rpc(t, h.front.URL, "/mcp", token, "tools/call",
		toolCall("ozon_product_list", map[string]any{}))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус %d: %s", resp.StatusCode, body)
	}
	if isErr, text := isToolError(t, body); isErr {
		t.Fatalf("инструмент вернул ошибку: %s", text)
	}
	if !mine || other {
		t.Fatalf("запрос ушёл не в тот магазин: свой=%v чужой=%v", mine, other)
	}
}

func TestOtherUsersTokenReachesOnlyTheirShop(t *testing.T) {
	var mine, other bool
	h := newHarness(t,
		func(w http.ResponseWriter, r *http.Request) { mine = true },
		func(w http.ResponseWriter, r *http.Request) {
			other = true
			_, _ = w.Write([]byte(`{"result":{"items":[],"total":0}}`))
		})

	_, body := rpc(t, h.front.URL, "/mcp", otherShopToken, "tools/call",
		toolCall("ozon_product_list", map[string]any{}))
	if isErr, text := isToolError(t, body); isErr {
		t.Fatalf("инструмент вернул ошибку: %s", text)
	}
	if mine || !other {
		t.Fatalf("запрос второго пользователя ушёл не туда: первый=%v второй=%v", mine, other)
	}
}

func TestOAuthReadScopeCannotWrite(t *testing.T) {
	var touched bool
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { touched = true }, nil)

	token := h.issueOAuth(t, 1, 10, oauth.ScopeRead)
	_, body := rpc(t, h.front.URL, "/mcp", token, "tools/call",
		toolCall("ozon_prices_update", map[string]any{
			"prices": []map[string]any{{"offer_id": "pp-1", "price": "100"}},
		}))
	if isErr, _ := isToolError(t, body); !isErr || touched {
		t.Fatal("OAuth-токен без ozon:write не должен допускаться к записи")
	}
}

func TestDeletedShopGets403NotLoop(t *testing.T) {
	// Токен жив, а магазина нет. 401 здесь заставил бы Claude
	// бесконечно обновлять токен.
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {}, nil)

	token := h.issueOAuth(t, 1, 99, oauth.ScopeRead)
	resp, _ := rpc(t, h.front.URL, "/mcp", token, "tools/list", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("к удалённому магазину ожидался 403, получен %d", resp.StatusCode)
	}
}

func TestUsageIsRecordedPerShop(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"result":{"items":[],"total":0}}`))
	}, nil)

	rpc(t, h.front.URL, "/mcp", readToken, "tools/call", toolCall("ozon_product_list", map[string]any{}))
	rpc(t, h.front.URL, "/mcp", readToken, "tools/call",
		toolCall("ozon_prices_update", map[string]any{
			"prices": []map[string]any{{"offer_id": "pp-1", "price": "100"}},
		}))
	rpc(t, h.front.URL, "/mcp", readToken, "tools/list", nil)

	got := h.records()
	if len(got) != 2 {
		t.Fatalf("учтено %d вызовов, ожидалось 2 (tools/list не вызов инструмента): %+v", len(got), got)
	}
	if got[0] != (usageRecord{1, 10, "ozon_product_list", false}) {
		t.Errorf("первый вызов: %+v", got[0])
	}
	if got[1] != (usageRecord{1, 10, "ozon_prices_update", true}) {
		t.Errorf("отказ в записи должен учитываться как ошибка: %+v", got[1])
	}
}

func TestRootIsNotMCP(t *testing.T) {
	// MCP живёт только на /mcp; корень — страницы сервиса.
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {}, nil)
	resp, _ := rpc(t, h.front.URL, "/", readToken, "tools/list", nil)
	if resp.StatusCode == http.StatusOK {
		t.Fatal("корень не должен обслуживать MCP")
	}
}
