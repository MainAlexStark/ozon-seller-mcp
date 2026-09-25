package web_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/MainAlexStark/ozon-seller-mcp/internal/httpx"
	"github.com/MainAlexStark/ozon-seller-mcp/internal/mcp"
	"github.com/MainAlexStark/ozon-seller-mcp/internal/oauth"
	"github.com/MainAlexStark/ozon-seller-mcp/internal/pgstore"
	"github.com/MainAlexStark/ozon-seller-mcp/internal/pgstore/pgtest"
	"github.com/MainAlexStark/ozon-seller-mcp/internal/secure"
	"github.com/MainAlexStark/ozon-seller-mcp/internal/shops"
	"github.com/MainAlexStark/ozon-seller-mcp/internal/tools"
	"github.com/MainAlexStark/ozon-seller-mcp/internal/web"
	"github.com/MainAlexStark/ozon-seller-mcp/ozon"
)

// Сквозные тесты сервиса: настоящая база, настоящие HTTP-обработчики
// кабинета, OAuth и MCP, подставной Ozon. Собирается так же, как в main.

const (
	goodClientID = "111"
	goodKey      = "good-key-0123456789abcdef"
	password     = "длинный-пароль-1"
)

type service struct {
	url string
	db  *pgstore.DB

	mu        sync.Mutex
	ozonCalls []string // Client-Id:Api-Key каждого запроса к Ozon
}

func (s *service) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ozonCalls...)
}

func newService(t *testing.T) *service {
	t.Helper()
	db := pgtest.Open(t)
	svc := &service{db: db}

	fakeOzon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cid, key := r.Header.Get("Client-Id"), r.Header.Get("Api-Key")
		svc.mu.Lock()
		svc.ozonCalls = append(svc.ozonCalls, cid+":"+key)
		svc.mu.Unlock()
		if cid != goodClientID || key != goodKey {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":7,"message":"Invalid Api-Key, please contact support"}`))
			return
		}
		_, _ = w.Write([]byte(`{"result":{"items":[{"product_id":1,"offer_id":"pp-1"}],"total":1}}`))
	}))
	t.Cleanup(fakeOzon.Close)

	key, _ := secure.GenerateSecretKey()
	cipher, err := secure.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	shopSvc := shops.New(db, cipher, ozon.WithBaseURL(fakeOzon.URL), ozon.WithMaxRetries(0))

	mux := http.NewServeMux()
	front := httptest.NewServer(mux)
	t.Cleanup(front.Close)
	svc.url = front.URL

	site, err := web.New(db, shopSvc, web.Config{PublicURL: front.URL, SignupOpen: true})
	if err != nil {
		t.Fatal(err)
	}
	o, err := oauth.New(oauth.Config{Issuer: front.URL, Store: db.OAuth(), Accounts: site})
	if err != nil {
		t.Fatal(err)
	}

	apiTokens := func(ctx context.Context, token string) (httpx.Principal, bool) {
		tk, err := db.APITokenByHash(ctx, secure.HashToken(token))
		if err != nil {
			return httpx.Principal{}, false
		}
		p := httpx.Principal{UserID: tk.UserID, ShopID: tk.ShopID, Scope: httpx.ScopeRead}
		for _, s := range tk.Scopes {
			if s == "write" {
				p.Scope = httpx.ScopeWrite
			}
		}
		return p, true
	}
	usage := func(user, shop int64, tool string, failed bool) {
		_ = db.RecordUsage(context.Background(), user, shop, tool, failed)
	}

	m := mcp.NewServer("test", "0.0.1")
	reg := tools.NewRegistry(nil, tools.DefaultSafety(), m)
	reg.RegisterCatalog()
	reg.RegisterPricing()
	reg.RegisterDiagnostics()

	h, err := httpx.NewServer(m, httpx.Auth{OAuth: o, APITokens: apiTokens}, shopSvc, usage,
		httpx.Config{Safety: tools.DefaultSafety(), Web: site, Ready: db.Ping})
	if err != nil {
		t.Fatal(err)
	}
	mux.Handle("/", h.Handler())
	return svc
}

// browser — посетитель со своими cookie; редиректы не следует.
type browser struct {
	t    *testing.T
	base string
	c    *http.Client
}

func (s *service) browser(t *testing.T) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: t, base: s.url, c: &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (b *browser) get(path string) (int, string, string) {
	b.t.Helper()
	resp, err := b.c.Get(b.base + path)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Location"), string(body)
}

func (b *browser) post(path string, form url.Values) (int, string, string) {
	b.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, b.base+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", b.base)
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Location"), string(body)
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (b *browser) csrf() string {
	b.t.Helper()
	_, _, page := b.get("/account")
	m := csrfRe.FindStringSubmatch(page)
	if m == nil {
		b.t.Fatalf("на странице нет csrf:\n%s", page)
	}
	return m[1]
}

func (b *browser) signup(email string) {
	b.t.Helper()
	code, loc, body := b.post("/signup", url.Values{
		"email": {email}, "password": {password}, "password2": {password}, "next": {"/account"},
	})
	if code != http.StatusFound || !strings.HasPrefix(loc, "/account/shops/new") {
		b.t.Fatalf("регистрация: %d %s\n%s", code, loc, body)
	}
}

func (b *browser) addShop(name, clientID, key string) (int, string, string) {
	b.t.Helper()
	return b.post("/account/shops/new", url.Values{
		"csrf": {b.csrf()}, "name": {name}, "client_id": {clientID}, "api_key": {key}, "next": {"/account"},
	})
}

var shopIDRe = regexp.MustCompile(`/account/shops/(\d+)/delete`)

func (b *browser) firstShopID() string {
	b.t.Helper()
	_, _, page := b.get("/account")
	m := shopIDRe.FindStringSubmatch(page)
	if m == nil {
		b.t.Fatal("в кабинете нет магазина")
	}
	return m[1]
}

// mcpCall вызывает инструмент по HTTP с токеном.
func (s *service) mcpCall(t *testing.T, token, tool string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": map[string]any{}}})
	req, _ := http.NewRequest(http.MethodPost, s.url+"/mcp", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// oauthConnect проходит подключение Claude целиком и возвращает access-токен.
func (s *service) oauthConnect(t *testing.T, b *browser, shopID string, write bool) string {
	t.Helper()
	const redirect = "https://claude.ai/api/mcp/auth_callback"
	reg, _ := json.Marshal(map[string]any{"client_name": "Claude", "redirect_uris": []string{redirect},
		"token_endpoint_auth_method": "none"})
	resp, err := http.Post(s.url+"/oauth/register", "application/json", strings.NewReader(string(reg)))
	if err != nil {
		t.Fatal(err)
	}
	var client struct {
		ClientID string `json:"client_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&client)
	resp.Body.Close()

	verifier := "verifier-0123456789-0123456789-0123456789-abc"
	sum := sha256.Sum256([]byte(verifier))
	authPath := "/oauth/authorize?" + url.Values{
		"response_type": {"code"}, "client_id": {client.ClientID}, "redirect_uri": {redirect},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
		"scope": {"ozon:read ozon:write offline_access"}, "state": {"s1"},
	}.Encode()

	code, _, page := b.get(authPath)
	if code != http.StatusOK {
		t.Fatalf("экран согласия: %d\n%s", code, page)
	}
	m := regexp.MustCompile(`name="session" value="([^"]+)"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("нет сессии согласия:\n%s", page)
	}
	form := url.Values{"session": {m[1]}, "action": {"allow"}, "shop": {shopID}, "scope_read": {"on"}}
	if write {
		form.Set("scope_write", "on")
	}
	code, loc, page := b.post("/oauth/authorize", form)
	if code != http.StatusFound {
		t.Fatalf("согласие: %d\n%s", code, page)
	}
	u, _ := url.Parse(loc)
	resp, err = http.PostForm(s.url+"/oauth/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {u.Query().Get("code")}, "client_id": {client.ClientID},
		"redirect_uri": {redirect}, "code_verifier": {verifier},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var tok map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&tok)
	access, _ := tok["access_token"].(string)
	if access == "" {
		t.Fatalf("токен не выдан: %v", tok)
	}
	return access
}

// --- тесты ---

func TestFullJourney(t *testing.T) {
	// Путь нового пользователя целиком: регистрация → ключ Ozon →
	// подключение Claude → вызов инструмента уходит в Ozon с его ключом.
	s := newService(t)
	b := s.browser(t)

	if code, _, page := b.get("/"); code != http.StatusOK || !strings.Contains(page, "Зарегистрироваться") {
		t.Fatalf("главная: %d", code)
	}
	b.signup(pgtest.Email("alex"))

	// Нерабочий ключ не сохраняется, человек видит понятную причину.
	code, _, page := b.addShop("Мой", goodClientID, "wrong-key-0123456789abc")
	if code != http.StatusBadRequest || !strings.Contains(page, "Ozon не принял ключ") {
		t.Fatalf("плохой ключ: %d\n%s", code, page)
	}
	code, loc, page := b.addShop("Мой магазин", goodClientID, goodKey)
	if code != http.StatusFound || loc != "/account?ok=shop_added" {
		t.Fatalf("подключение магазина: %d %s\n%s", code, loc, page)
	}
	_, _, page = b.get("/account")
	if !strings.Contains(page, "Мой магазин") || !strings.Contains(page, "…cdef") {
		t.Fatalf("магазина нет в кабинете:\n%s", page)
	}
	if strings.Contains(page, goodKey) {
		t.Fatal("ключ целиком показан в кабинете")
	}
	shopID := b.firstShopID()

	token := s.oauthConnect(t, b, shopID, false)
	before := len(s.calls())
	code, body := s.mcpCall(t, token, "ozon_product_list")
	if code != http.StatusOK || strings.Contains(body, `"isError":true`) {
		t.Fatalf("вызов инструмента: %d %s", code, body)
	}
	calls := s.calls()
	if len(calls) != before+1 || calls[len(calls)-1] != goodClientID+":"+goodKey {
		t.Fatalf("в Ozon ушли не те ключи: %v", calls[before:])
	}

	// Подключение без записи не пишет.
	_, body = s.mcpCall(t, token, "ozon_prices_update")
	if !strings.Contains(body, `"isError":true`) {
		t.Fatalf("запись без права: %s", body)
	}

	_, _, page = b.get("/account")
	if !strings.Contains(page, "вызовов за 30 дней: 2") {
		t.Errorf("учёт вызовов не виден в кабинете")
	}
	if !strings.Contains(page, "Claude") || !strings.Contains(page, "Отозвать") {
		t.Errorf("подключение не видно в кабинете")
	}

	// Удаление магазина обрывает подключение сразу.
	code, _, _ = b.post("/account/shops/"+shopID+"/delete", url.Values{"csrf": {b.csrf()}})
	if code != http.StatusFound {
		t.Fatalf("удаление магазина: %d", code)
	}
	if code, _ := s.mcpCall(t, token, "ozon_product_list"); code == http.StatusOK {
		t.Fatal("подключение к удалённому магазину продолжает работать")
	}
}

func TestAnonymousOAuthGoesThroughLoginAndBack(t *testing.T) {
	s := newService(t)
	b := s.browser(t)
	email := pgtest.Email("oauth")
	b.signup(email)
	b.addShop("", goodClientID, goodKey)
	b.post("/logout", url.Values{"csrf": {b.csrf()}})

	// Куда вернуться после входа, oauth-пакет передаёт в next; здесь
	// проверяется, что кабинет честно туда возвращает.
	next := "/oauth/authorize?client_id=x&response_type=code"
	code, loc, _ := b.post("/login", url.Values{"email": {email}, "password": {password}, "next": {next}})
	if code != http.StatusFound || loc != next {
		t.Fatalf("после входа не вернулись к согласию: %d %s", code, loc)
	}
}

func TestLoginRejectsWrongPasswordAndOpenRedirect(t *testing.T) {
	s := newService(t)
	b := s.browser(t)
	email := pgtest.Email("login")
	b.signup(email)
	b.post("/logout", url.Values{"csrf": {b.csrf()}})

	if code, _, page := b.post("/login", url.Values{"email": {email}, "password": {"не-тот-пароль"}}); code != http.StatusUnauthorized || !strings.Contains(page, "Неверная почта или пароль") {
		t.Fatalf("неверный пароль: %d", code)
	}
	// Та же фраза для несуществующего адреса — форма не выдаёт, кто зарегистрирован.
	if _, _, page := b.post("/login", url.Values{"email": {"nobody@example.com"}, "password": {"x"}}); !strings.Contains(page, "Неверная почта или пароль") {
		t.Fatal("для неизвестного адреса другой ответ")
	}
	for _, evil := range []string{"//evil.example", "https://evil.example", "/\\evil.example"} {
		code, loc, _ := b.post("/login", url.Values{"email": {email}, "password": {password}, "next": {evil}})
		if code != http.StatusFound || loc != "/account" {
			t.Fatalf("открытый редирект через next=%q: %d %s", evil, code, loc)
		}
	}
}

func TestDuplicateSignupAndShortPassword(t *testing.T) {
	s := newService(t)
	email := pgtest.Email("dup")
	s.browser(t).signup(email)

	b := s.browser(t)
	code, _, page := b.post("/signup", url.Values{"email": {strings.ToUpper(email)}, "password": {password}, "password2": {password}})
	if code != http.StatusBadRequest || !strings.Contains(page, "уже зарегистрирован") {
		t.Fatalf("повторная регистрация: %d", code)
	}
	code, _, _ = b.post("/signup", url.Values{"email": {pgtest.Email("short")}, "password": {"короткий"}, "password2": {"короткий"}})
	if code != http.StatusBadRequest {
		t.Fatalf("короткий пароль принят: %d", code)
	}
}

func TestCSRFAndForeignOriginRejected(t *testing.T) {
	s := newService(t)
	b := s.browser(t)
	b.signup(pgtest.Email("csrf"))

	if code, _, _ := b.post("/account/tokens", url.Values{"name": {"x"}}); code != http.StatusForbidden {
		t.Fatalf("форма без csrf прошла: %d", code)
	}

	req, _ := http.NewRequest(http.MethodPost, s.url+"/login", strings.NewReader("email=a&password=b"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("вход с чужого Origin: %d", resp.StatusCode)
	}
}

func TestUsersCannotTouchEachOthersShops(t *testing.T) {
	s := newService(t)
	alice := s.browser(t)
	alice.signup(pgtest.Email("alice"))
	alice.addShop("Алиса", goodClientID, goodKey)
	aliceShop := alice.firstShopID()

	bob := s.browser(t)
	bob.signup(pgtest.Email("bob"))
	for _, action := range []string{"delete", "check", "key"} {
		code, _, _ := bob.post("/account/shops/"+aliceShop+"/"+action, url.Values{"csrf": {bob.csrf()}, "api_key": {goodKey}})
		if code != http.StatusNotFound {
			t.Fatalf("чужой магазин, действие %s: %d", action, code)
		}
	}
	// Токен к чужому магазину не выпускается.
	_, _, page := bob.post("/account/tokens", url.Values{"csrf": {bob.csrf()}, "shop": {aliceShop}, "name": {"x"}})
	if strings.Contains(page, httpx.APITokenPrefix) {
		t.Fatal("выпущен токен к чужому магазину")
	}
	if strings.Contains(page, "Алиса") {
		t.Fatal("в кабинете Боба видно магазин Алисы")
	}
}

var tokenRe = regexp.MustCompile(`osm_[A-Za-z0-9_-]{20,}`)

func TestAPITokenLifecycle(t *testing.T) {
	s := newService(t)
	b := s.browser(t)
	b.signup(pgtest.Email("api"))
	b.addShop("Мой", goodClientID, goodKey)
	shopID := b.firstShopID()

	_, _, page := b.post("/account/tokens", url.Values{"csrf": {b.csrf()}, "shop": {shopID}, "name": {"PrintPipe"}})
	token := tokenRe.FindString(page)
	if token == "" {
		t.Fatalf("токен не показан:\n%s", page)
	}
	if code, body := s.mcpCall(t, token, "ozon_product_list"); code != http.StatusOK || strings.Contains(body, `"isError":true`) {
		t.Fatalf("вызов по API-токену: %d %s", code, body)
	}
	// Второй раз токен не показывается.
	if _, _, page := b.get("/account"); strings.Contains(page, token) {
		t.Fatal("токен виден повторно")
	}

	id := regexp.MustCompile(`/account/tokens/(\d+)/delete`).FindStringSubmatch(page)
	if id == nil {
		t.Fatal("нет кнопки отзыва")
	}
	b.post("/account/tokens/"+id[1]+"/delete", url.Values{"csrf": {b.csrf()}})
	if code, _ := s.mcpCall(t, token, "ozon_product_list"); code != http.StatusUnauthorized {
		t.Fatalf("отозванный токен: %d", code)
	}
}

func TestAccountDeletion(t *testing.T) {
	s := newService(t)
	b := s.browser(t)
	email := pgtest.Email("gone")
	b.signup(email)
	b.addShop("Мой", goodClientID, goodKey)
	token := s.oauthConnect(t, b, b.firstShopID(), true)

	if code, _, _ := b.post("/account/delete", url.Values{"csrf": {b.csrf()}, "password": {"не-тот"}}); code != http.StatusBadRequest {
		t.Fatalf("удаление с неверным паролем: %d", code)
	}
	if code, _, _ := b.post("/account/delete", url.Values{"csrf": {b.csrf()}, "password": {password}}); code != http.StatusOK {
		t.Fatalf("удаление: %d", code)
	}
	if _, err := s.db.UserByEmail(context.Background(), email); err == nil {
		t.Fatal("пользователь остался в базе")
	}
	if code, _ := s.mcpCall(t, token, "ozon_product_list"); code == http.StatusOK {
		t.Fatal("подключение удалённого пользователя работает")
	}
}

func TestHealthChecksDatabase(t *testing.T) {
	s := newService(t)
	resp, err := http.Get(s.url + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %d", resp.StatusCode)
	}
}
