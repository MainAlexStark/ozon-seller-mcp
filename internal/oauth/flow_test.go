package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const testPassword = "очень-длинный-пароль-владельца"

// PBKDF2 намеренно медленный — в этом его смысл. Но считать его заново
// в каждом тесте значит платить эту цену два десятка раз: прогон пакета
// растягивался с секунды до сорока. Хеш один и тот же, считаем однажды.
var testPasswordHash = sync.OnceValue(func() string {
	h, err := HashPassword(testPassword)
	if err != nil {
		panic(err)
	}
	return h
})

// harness — сервер авторизации на настоящем HTTP.
type harness struct {
	srv    *httptest.Server
	oauth  *Server
	client *http.Client
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	store, err := NewStore("") // только в памяти
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	httpSrv := httptest.NewServer(mux)
	t.Cleanup(httpSrv.Close)

	o, err := New(Config{
		Issuer:       httpSrv.URL,
		ResourceURL:  httpSrv.URL + "/mcp",
		PasswordHash: testPasswordHash(),
		Store:        store,
	})
	if err != nil {
		t.Fatal(err)
	}
	o.Mount(mux)

	// Редиректы не следуем: нам нужно прочитать сам Location.
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return &harness{srv: httpSrv, oauth: o, client: client}
}

func (h *harness) getJSON(t *testing.T, path string) map[string]any {
	t.Helper()

	resp, err := h.client.Get(h.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: статус %d", path, resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return out
}

// register регистрирует клиента через DCR.
func (h *harness) register(t *testing.T, redirectURI string) string {
	t.Helper()

	body, _ := json.Marshal(map[string]any{
		"client_name":                "Claude (тест)",
		"redirect_uris":              []string{redirectURI},
		"token_endpoint_auth_method": "none",
	})

	resp, err := h.client.Post(h.srv.URL+"/oauth/register", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("регистрация: статус %d, тело %s", resp.StatusCode, raw)
	}

	var out struct {
		ClientID string `json:"client_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.ClientID == "" {
		t.Fatal("не выдан client_id")
	}
	return out.ClientID
}

// pkce возвращает пару verifier/challenge.
func pkce(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// consent проходит экран согласия и возвращает код авторизации.
func (h *harness) consent(t *testing.T, clientID, redirectURI, challenge, scope string, allowWrite bool) (code string, status int) {
	t.Helper()

	authURL := h.srv.URL + "/oauth/authorize?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"scope":                 {scope},
		"state":                 {"состояние-123"},
		"resource":              {h.oauth.ResourceURL()},
	}.Encode()

	resp, err := h.client.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", resp.StatusCode
	}

	sessionID := extractSession(t, string(page))

	form := url.Values{
		"session":    {sessionID},
		"action":     {"allow"},
		"password":   {testPassword},
		"scope_read": {"on"},
	}
	if allowWrite {
		form.Set("scope_write", "on")
	}

	resp2, err := h.client.PostForm(h.srv.URL+"/oauth/authorize", form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusFound {
		raw, _ := io.ReadAll(resp2.Body)
		t.Fatalf("подтверждение: статус %d, тело %s", resp2.StatusCode, raw)
	}

	loc, err := url.Parse(resp2.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if e := loc.Query().Get("error"); e != "" {
		t.Fatalf("вернулась ошибка: %s (%s)", e, loc.Query().Get("error_description"))
	}
	if got := loc.Query().Get("state"); got != "состояние-123" {
		t.Errorf("state не вернулся: %q", got)
	}
	if got := loc.Query().Get("iss"); got != h.oauth.Issuer() {
		t.Errorf("iss не вернулся или неверен: %q", got)
	}
	return loc.Query().Get("code"), resp2.StatusCode
}

func extractSession(t *testing.T, page string) string {
	t.Helper()

	const marker = `name="session" value="`
	i := strings.Index(page, marker)
	if i < 0 {
		t.Fatalf("на странице согласия нет идентификатора сессии:\n%s", page)
	}
	rest := page[i+len(marker):]
	j := strings.Index(rest, `"`)
	return rest[:j]
}

// exchange меняет код на токены.
func (h *harness) exchange(t *testing.T, clientID, code, verifier, redirectURI string) map[string]any {
	t.Helper()

	resp, err := h.client.PostForm(h.srv.URL+"/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
		"resource":      {h.oauth.ResourceURL()},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("обмен кода: статус %d, тело %v", resp.StatusCode, out)
	}
	return out
}

// --- Тесты ---

func TestDiscoveryDocuments(t *testing.T) {
	h := newHarness(t)

	prm := h.getJSON(t, "/.well-known/oauth-protected-resource")
	if prm["resource"] != h.oauth.ResourceURL() {
		t.Errorf("resource = %v, want %v", prm["resource"], h.oauth.ResourceURL())
	}
	servers, _ := prm["authorization_servers"].([]any)
	if len(servers) == 0 || servers[0] != h.oauth.Issuer() {
		t.Errorf("authorization_servers = %v", prm["authorization_servers"])
	}

	asm := h.getJSON(t, "/.well-known/oauth-authorization-server")
	if asm["issuer"] != h.oauth.Issuer() {
		t.Errorf("issuer = %v", asm["issuer"])
	}
	// Без объявленного S256 совместимый клиент не начнёт поток.
	methods, _ := asm["code_challenge_methods_supported"].([]any)
	if len(methods) != 1 || methods[0] != "S256" {
		t.Errorf("code_challenge_methods_supported = %v", asm["code_challenge_methods_supported"])
	}
	if asm["registration_endpoint"] == nil {
		t.Error("нет registration_endpoint: Claude не сможет зарегистрироваться через DCR")
	}
}

func TestDiscoveryWorksUnderResourcePathSuffix(t *testing.T) {
	// Claude сначала пробует путь с суффиксом ресурса и лишь потом
	// корневой. Отвечать надо на оба.
	h := newHarness(t)

	prm := h.getJSON(t, "/.well-known/oauth-protected-resource/mcp")
	if prm["resource"] != h.oauth.ResourceURL() {
		t.Errorf("resource = %v", prm["resource"])
	}
}

func TestFullAuthorizationCodeFlow(t *testing.T) {
	h := newHarness(t)
	const redirectURI = "https://claude.ai/api/mcp/auth_callback"

	clientID := h.register(t, redirectURI)

	verifier := "проверочная-строка-достаточной-длины-для-pkce-1234567890"
	code, _ := h.consent(t, clientID, redirectURI, pkce(verifier), ScopeRead+" "+ScopeWrite+" "+ScopeOfflineAccess, true)
	if code == "" {
		t.Fatal("код авторизации не выдан")
	}

	tok := h.exchange(t, clientID, code, verifier, redirectURI)

	access, _ := tok["access_token"].(string)
	if access == "" {
		t.Fatal("не выдан access_token")
	}
	if tok["token_type"] != "Bearer" {
		t.Errorf("token_type = %v", tok["token_type"])
	}
	if tok["refresh_token"] == nil {
		t.Error("при offline_access должен выдаваться refresh_token")
	}

	validated, err := h.oauth.Validate(access)
	if err != nil {
		t.Fatalf("выданный токен не проходит проверку: %v", err)
	}
	if !validated.HasScope(ScopeWrite) {
		t.Error("подтверждена запись, но её нет в токене")
	}
}

func TestOwnerCanNarrowScopesAtConsent(t *testing.T) {
	// Смысл экрана согласия: решает владелец, а не клиент. Клиент
	// просит запись, владелец её не даёт — токен выходит без неё.
	h := newHarness(t)
	const redirectURI = "https://claude.ai/api/mcp/auth_callback"

	clientID := h.register(t, redirectURI)
	verifier := "verifier-для-телефона-1234567890123456789012345"

	code, _ := h.consent(t, clientID, redirectURI, pkce(verifier), ScopeRead+" "+ScopeWrite, false)
	tok := h.exchange(t, clientID, code, verifier, redirectURI)

	validated, err := h.oauth.Validate(tok["access_token"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if validated.HasScope(ScopeWrite) {
		t.Fatal("владелец не давал права записи, а оно в токене есть")
	}
	if !validated.HasScope(ScopeRead) {
		t.Error("чтение должно остаться")
	}
}

func TestWrongPasswordDoesNotIssueCode(t *testing.T) {
	h := newHarness(t)
	const redirectURI = "https://claude.ai/api/mcp/auth_callback"

	clientID := h.register(t, redirectURI)
	verifier := "verifier-1234567890123456789012345678901234567"

	authURL := h.srv.URL + "/oauth/authorize?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"code_challenge":        {pkce(verifier)},
		"code_challenge_method": {"S256"},
		"scope":                 {ScopeRead},
	}.Encode()

	resp, _ := h.client.Get(authURL)
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	sessionID := extractSession(t, string(page))

	resp2, err := h.client.PostForm(h.srv.URL+"/oauth/authorize", url.Values{
		"session":    {sessionID},
		"action":     {"allow"},
		"password":   {"неверный"},
		"scope_read": {"on"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode == http.StatusFound {
		t.Fatal("с неверным паролем код выдаваться не должен")
	}
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("ожидался 401, получен %d", resp2.StatusCode)
	}
}

func TestPKCEMismatchRejected(t *testing.T) {
	// Перехваченный код бесполезен без verifier — ради этого PKCE и есть.
	h := newHarness(t)
	const redirectURI = "https://claude.ai/api/mcp/auth_callback"

	clientID := h.register(t, redirectURI)
	verifier := "настоящий-verifier-12345678901234567890123456"

	code, _ := h.consent(t, clientID, redirectURI, pkce(verifier), ScopeRead, false)

	resp, err := h.client.PostForm(h.srv.URL+"/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
		"code_verifier": {"чужой-verifier-1234567890123456789012345678"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("чужой verifier должен отклоняться, статус %d", resp.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["error"] != "invalid_grant" {
		t.Errorf("код ошибки = %v, want invalid_grant", out["error"])
	}
}

func TestAuthCodeIsSingleUse(t *testing.T) {
	// Повторное использование кода — признак перехвата.
	h := newHarness(t)
	const redirectURI = "https://claude.ai/api/mcp/auth_callback"

	clientID := h.register(t, redirectURI)
	verifier := "verifier-одноразовый-12345678901234567890123"

	code, _ := h.consent(t, clientID, redirectURI, pkce(verifier), ScopeRead, false)
	h.exchange(t, clientID, code, verifier, redirectURI) // первый раз — успех

	resp, err := h.client.PostForm(h.srv.URL+"/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("повторный обмен кода должен отклоняться, статус %d", resp.StatusCode)
	}
}

func TestRefreshRotatesAndInvalidatesOldTokens(t *testing.T) {
	h := newHarness(t)
	const redirectURI = "https://claude.ai/api/mcp/auth_callback"

	clientID := h.register(t, redirectURI)
	verifier := "verifier-для-обновления-123456789012345678901"

	code, _ := h.consent(t, clientID, redirectURI, pkce(verifier), ScopeRead+" "+ScopeOfflineAccess, false)
	first := h.exchange(t, clientID, code, verifier, redirectURI)

	oldAccess := first["access_token"].(string)
	oldRefresh := first["refresh_token"].(string)

	resp, err := h.client.PostForm(h.srv.URL+"/oauth/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {oldRefresh},
		"client_id":     {clientID},
	})
	if err != nil {
		t.Fatal(err)
	}
	var second map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&second)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("обновление должно проходить, статус %d, тело %v", resp.StatusCode, second)
	}

	newRefresh, _ := second["refresh_token"].(string)
	if newRefresh == "" || newRefresh == oldRefresh {
		t.Error("refresh должен ротироваться: OAuth 2.1 требует этого для публичных клиентов")
	}

	// Старый access обязан умереть вместе с выдачей, иначе отзыв
	// ничего не отзывает.
	if _, err := h.oauth.Validate(oldAccess); err == nil {
		t.Error("старый access-токен должен становиться недействительным после обновления")
	}
	if _, err := h.oauth.Validate(second["access_token"].(string)); err != nil {
		t.Errorf("новый access-токен должен работать: %v", err)
	}
}

func TestReusedRefreshTokenRejectedWithInvalidGrant(t *testing.T) {
	h := newHarness(t)
	const redirectURI = "https://claude.ai/api/mcp/auth_callback"

	clientID := h.register(t, redirectURI)
	verifier := "verifier-повторное-обновление-1234567890123"

	code, _ := h.consent(t, clientID, redirectURI, pkce(verifier), ScopeRead+" "+ScopeOfflineAccess, false)
	first := h.exchange(t, clientID, code, verifier, redirectURI)
	refresh := first["refresh_token"].(string)

	// Используем один раз.
	resp, _ := h.client.PostForm(h.srv.URL+"/oauth/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
	})
	resp.Body.Close()

	// И ещё раз тем же.
	resp2, err := h.client.PostForm(h.srv.URL+"/oauth/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()

	var out map[string]any
	_ = json.NewDecoder(resp2.Body).Decode(&out)

	// Именно invalid_grant: по нему Claude поймёт, что надо заново
	// пройти согласие, а не повторять обновление до бесконечности.
	if out["error"] != "invalid_grant" {
		t.Errorf("код ошибки = %v, want invalid_grant", out["error"])
	}
}

func TestRevokeKillsWholeGrant(t *testing.T) {
	// То, ради чего вообще затевался OAuth: отозвать доступ одного
	// устройства, не трогая остальные.
	h := newHarness(t)
	const redirectURI = "https://claude.ai/api/mcp/auth_callback"

	clientID := h.register(t, redirectURI)
	verifier := "verifier-для-отзыва-12345678901234567890123"

	code, _ := h.consent(t, clientID, redirectURI, pkce(verifier), ScopeRead+" "+ScopeOfflineAccess, false)
	tok := h.exchange(t, clientID, code, verifier, redirectURI)

	access := tok["access_token"].(string)
	refresh := tok["refresh_token"].(string)

	resp, err := h.client.PostForm(h.srv.URL+"/oauth/revoke", url.Values{"token": {access}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if _, err := h.oauth.Validate(access); err == nil {
		t.Error("отозванный access-токен всё ещё действует")
	}

	// И refresh тоже: иначе доступ вернётся первым же обновлением.
	resp2, err := h.client.PostForm(h.srv.URL+"/oauth/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode == http.StatusOK {
		t.Fatal("после отзыва refresh не должен выпускать новые токены")
	}
}

func TestUnknownTokenRevocationIsSuccess(t *testing.T) {
	// RFC 7009: иначе эндпоинт становится оракулом «существует ли токен».
	h := newHarness(t)

	resp, err := h.client.PostForm(h.srv.URL+"/oauth/revoke", url.Values{"token": {"такого-нет"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("отзыв неизвестного токена должен возвращать 200, получен %d", resp.StatusCode)
	}
}

func TestTokenForOtherResourceRejected(t *testing.T) {
	// Подлинный токен, выпущенный для другого сервиса, не должен
	// открывать этот. Без проверки audience один общий провайдер
	// личности открывал бы все сервисы сразу.
	h := newHarness(t)

	store, _ := NewStore("")
	access := "чужой-токен"
	err := store.SaveTokens(access, &Token{
		ClientID:  "someone",
		Scopes:    []string{ScopeRead},
		Resource:  "https://другой-сервис.example/mcp",
		ExpiresAt: timeNowPlusHour(),
		GrantID:   "g1",
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	h.oauth.store = store
	if _, err := h.oauth.Validate(access); err == nil {
		t.Fatal("токен для другого ресурса должен отклоняться")
	}
}

func TestRedirectURIMustBeRegistered(t *testing.T) {
	// Иначе сервер становится инструментом открытого редиректа.
	h := newHarness(t)
	clientID := h.register(t, "https://claude.ai/api/mcp/auth_callback")

	resp, err := h.client.Get(h.srv.URL + "/oauth/authorize?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {"https://злоумышленник.example/забрать"},
		"code_challenge":        {pkce("v")},
		"code_challenge_method": {"S256"},
	}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("незарегистрированный адрес возврата должен отклоняться, статус %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Fatalf("на ошибку в самом redirect_uri нельзя отвечать перенаправлением: %s", loc)
	}
}

func TestPKCERequired(t *testing.T) {
	h := newHarness(t)
	const redirectURI = "https://claude.ai/api/mcp/auth_callback"
	clientID := h.register(t, redirectURI)

	resp, err := h.client.Get(h.srv.URL + "/oauth/authorize?" + url.Values{
		"response_type": {"code"},
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
	}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("ожидалось перенаправление с ошибкой, статус %d", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc.Query().Get("error") != "invalid_request" {
		t.Errorf("без PKCE ожидалась ошибка invalid_request, получено %q", loc.Query().Get("error"))
	}
}

func timeNowPlusHour() time.Time { return time.Now().Add(time.Hour) }
