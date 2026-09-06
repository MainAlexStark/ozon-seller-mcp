package oauth

import (
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// handleAuthorize — начало потока: показываем форму входа и согласия.
//
// Порядок проверок важен. Всё, что связано с redirect_uri, проверяется
// ДО того, как мы соглашаемся куда-то перенаправлять: ошибку в самом
// адресе возврата нельзя сообщать перенаправлением на него же, иначе
// сервер превращается в инструмент открытого редиректа.
func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	clientID := q.Get("client_id")
	redirectURI := q.Get("redirect_uri")

	client, err := s.store.Client(clientID)
	if err != nil {
		// Клиент неизвестен — перенаправлять некуда, показываем страницу.
		s.renderError(w, http.StatusBadRequest,
			"Неизвестное приложение",
			"Клиент с таким client_id не зарегистрирован. Попробуйте подключить сервер в Claude заново.")
		return
	}

	if !redirectURIAllowed(client.RedirectURIs, redirectURI) {
		s.renderError(w, http.StatusBadRequest,
			"Недопустимый адрес возврата",
			"Адрес redirect_uri не совпадает с зарегистрированным. Это защита от перенаправления на чужой сайт.")
		return
	}

	// С этого момента ошибки можно возвращать клиенту перенаправлением.

	if q.Get("response_type") != "code" {
		s.redirectError(w, r, redirectURI, q.Get("state"), "unsupported_response_type",
			"поддерживается только response_type=code")
		return
	}

	challenge := q.Get("code_challenge")
	method := q.Get("code_challenge_method")
	if challenge == "" || method != "S256" {
		s.redirectError(w, r, redirectURI, q.Get("state"), "invalid_request",
			"обязателен PKCE с code_challenge_method=S256")
		return
	}

	// RFC 8707: токен выпускается для конкретного ресурса. Если клиент
	// не сказал, для какого, считаем, что для нашего единственного.
	resource := q.Get("resource")
	if resource == "" {
		resource = s.cfg.ResourceURL
	}
	if !sameResource(resource, s.cfg.ResourceURL) {
		s.redirectError(w, r, redirectURI, q.Get("state"), "invalid_target",
			"запрошен токен для другого ресурса: "+resource)
		return
	}

	requested := parseScopes(q.Get("scope"))
	if len(requested) == 0 {
		requested = []string{ScopeRead}
	}

	sessionID, err := randomToken()
	if err != nil {
		s.redirectError(w, r, redirectURI, q.Get("state"), "server_error", "не удалось начать сессию")
		return
	}

	sess := &LoginSession{
		ID:            sessionID,
		ClientID:      client.ID,
		RedirectURI:   redirectURI,
		State:         q.Get("state"),
		Scopes:        requested,
		Resource:      resource,
		CodeChallenge: challenge,
		ExpiresAt:     time.Now().Add(LoginSessionTTL),
	}
	if err := s.store.SaveSession(sess); err != nil {
		s.redirectError(w, r, redirectURI, q.Get("state"), "server_error", "не удалось сохранить сессию")
		return
	}

	s.renderConsent(w, client, sess, "")
}

// handleAuthorizeSubmit — владелец ввёл пароль и выбрал права.
func (s *Server) handleAuthorizeSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, http.StatusBadRequest, "Не разобрана форма", err.Error())
		return
	}

	sessionID := r.PostFormValue("session")
	sess, err := s.store.Session(sessionID)
	if err != nil {
		s.renderError(w, http.StatusBadRequest,
			"Время истекло",
			"Страница подтверждения устарела. Начните подключение в Claude заново.")
		return
	}

	client, err := s.store.Client(sess.ClientID)
	if err != nil {
		s.renderError(w, http.StatusBadRequest, "Неизвестное приложение", "Клиент не найден.")
		return
	}

	// Отказ владельца — обычный исход, а не ошибка.
	if r.PostFormValue("action") != "allow" {
		_, _ = s.store.TakeSession(sessionID)
		s.redirectError(w, r, sess.RedirectURI, sess.State, "access_denied", "владелец отклонил подключение")
		return
	}

	if !VerifyPassword(s.cfg.PasswordHash, r.PostFormValue("password")) {
		s.logger.Warn("неверный пароль при подтверждении доступа",
			"client", client.Name, "remote", r.RemoteAddr)
		// Сессию не трогаем: человек мог просто опечататься.
		s.renderConsent(w, client, sess, "Неверный пароль.")
		return
	}

	// Права берём из галочек, а не из того, что запросил клиент:
	// смысл экрана согласия в том, что решает владелец. Снятая галочка
	// записи — это токен, которым магазин изменить нельзя.
	granted := []string{}
	if r.PostFormValue("scope_read") == "on" {
		granted = append(granted, ScopeRead)
	}
	if r.PostFormValue("scope_write") == "on" {
		granted = append(granted, ScopeWrite)
	}
	if len(granted) == 0 {
		s.renderConsent(w, client, sess, "Выберите хотя бы одно право, иначе подключать нечего.")
		return
	}
	// offline_access отдаём, если его просили: без него Claude будет
	// требовать подтверждение каждый час.
	if containsScope(sess.Scopes, ScopeOfflineAccess) {
		granted = append(granted, ScopeOfflineAccess)
	}

	if _, err := s.store.TakeSession(sessionID); err != nil {
		s.renderError(w, http.StatusBadRequest, "Время истекло", "Начните подключение заново.")
		return
	}

	code, err := randomToken()
	if err != nil {
		s.redirectError(w, r, sess.RedirectURI, sess.State, "server_error", "не удалось выдать код")
		return
	}

	err = s.store.SaveCode(code, &AuthCode{
		ClientID:      sess.ClientID,
		RedirectURI:   sess.RedirectURI,
		Scopes:        granted,
		Resource:      sess.Resource,
		CodeChallenge: sess.CodeChallenge,
		ExpiresAt:     time.Now().Add(AuthCodeTTL),
	})
	if err != nil {
		s.redirectError(w, r, sess.RedirectURI, sess.State, "server_error", "не удалось сохранить код")
		return
	}

	s.logger.Info("доступ разрешён",
		"client", client.Name,
		"client_id", client.ID,
		"scopes", granted)

	u, _ := url.Parse(sess.RedirectURI)
	q := u.Query()
	q.Set("code", code)
	if sess.State != "" {
		q.Set("state", sess.State)
	}
	// RFC 9207: клиент должен убедиться, что ответ пришёл от того же
	// издателя, у которого он начинал поток.
	q.Set("iss", s.cfg.Issuer)
	u.RawQuery = q.Encode()

	http.Redirect(w, r, u.String(), http.StatusFound)
}

// redirectError возвращает ошибку клиенту через адрес возврата.
func (s *Server) redirectError(w http.ResponseWriter, r *http.Request, redirectURI, state, code, description string) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		s.renderError(w, http.StatusBadRequest, "Ошибка", description)
		return
	}
	q := u.Query()
	q.Set("error", code)
	q.Set("error_description", description)
	if state != "" {
		q.Set("state", state)
	}
	q.Set("iss", s.cfg.Issuer)
	u.RawQuery = q.Encode()

	http.Redirect(w, r, u.String(), http.StatusFound)
}

// sameResource сравнивает адреса ресурса, прощая завершающий слэш.
func sameResource(a, b string) bool {
	return strings.TrimSuffix(a, "/") == strings.TrimSuffix(b, "/")
}

func parseScopes(s string) []string {
	var out []string
	for _, f := range strings.Fields(s) {
		out = append(out, f)
	}
	return out
}

func containsScope(scopes []string, want string) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}

// --- Страницы ---

type consentView struct {
	SessionID    string
	ClientName   string
	RedirectHost string
	IsLoopback   bool
	WantsWrite   bool
	Error        string
}

// renderConsent показывает экран согласия.
func (s *Server) renderConsent(w http.ResponseWriter, client *Client, sess *LoginSession, errMsg string) {
	host := sess.RedirectURI
	if u, err := url.Parse(sess.RedirectURI); err == nil {
		host = u.Host
		if host == "" {
			host = u.String()
		}
	}

	view := consentView{
		SessionID:    sess.ID,
		ClientName:   client.Name,
		RedirectHost: host,
		IsLoopback:   isLoopbackRedirect(client.RedirectURIs),
		WantsWrite:   containsScope(sess.Scopes, ScopeWrite),
		Error:        errMsg,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if errMsg != "" {
		w.WriteHeader(http.StatusUnauthorized)
	}
	_ = consentTemplate.Execute(w, view)
}

// isLoopbackRedirect — все адреса возврата ведут на эту же машину.
//
// Такой клиент невозможно отличить от любого другого процесса на том же
// компьютере, поэтому спецификация требует предупредить об этом явно.
func isLoopbackRedirect(uris []string) bool {
	if len(uris) == 0 {
		return false
	}
	for _, raw := range uris {
		u, err := url.Parse(raw)
		if err != nil || !isLoopbackHost(u.Hostname()) {
			return false
		}
	}
	return true
}

func (s *Server) renderError(w http.ResponseWriter, status int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = errorTemplate.Execute(w, map[string]string{"Title": title, "Message": message})
}

const pageStyle = `
:root{color-scheme:light dark}
*{box-sizing:border-box}
body{font:16px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;
  margin:0;padding:40px 20px;background:#f4f5f6;color:#15181b;display:flex;
  justify-content:center;align-items:flex-start;min-height:100vh}
@media(prefers-color-scheme:dark){body{background:#121518;color:#e2e6ea}}
.card{background:#fff;max-width:460px;width:100%;padding:28px;border-radius:10px;
  border:1px solid #d8dde1}
@media(prefers-color-scheme:dark){.card{background:#191d21;border-color:#2b3238}}
h1{font-size:20px;margin:0 0 4px}
.sub{color:#5b6670;font-size:14px;margin:0 0 20px}
@media(prefers-color-scheme:dark){.sub{color:#98a3ad}}
.row{display:flex;justify-content:space-between;gap:12px;padding:9px 0;
  border-bottom:1px solid #eceef0;font-size:14px}
@media(prefers-color-scheme:dark){.row{border-color:#232930}}
.row span:first-child{color:#5b6670}
@media(prefers-color-scheme:dark){.row span:first-child{color:#98a3ad}}
.mono{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:13px}
fieldset{border:0;padding:0;margin:22px 0 0}
legend{font-size:12px;letter-spacing:.08em;text-transform:uppercase;
  color:#5b6670;padding:0 0 8px}
@media(prefers-color-scheme:dark){legend{color:#98a3ad}}
label.check{display:flex;gap:10px;align-items:flex-start;padding:10px;
  border:1px solid #d8dde1;border-radius:8px;margin-bottom:8px;cursor:pointer}
@media(prefers-color-scheme:dark){label.check{border-color:#2b3238}}
label.check b{display:block;font-weight:600}
label.check small{color:#5b6670}
@media(prefers-color-scheme:dark){label.check small{color:#98a3ad}}
input[type=password]{width:100%;padding:10px 12px;font-size:15px;
  border:1px solid #c5ccd2;border-radius:8px;background:#fff;color:inherit}
@media(prefers-color-scheme:dark){input[type=password]{background:#12161a;border-color:#2b3238}}
.actions{display:flex;gap:10px;margin-top:22px}
button{flex:1;padding:11px;font-size:15px;font-weight:600;border-radius:8px;
  border:1px solid transparent;cursor:pointer}
button.allow{background:#1f6feb;color:#fff}
button.deny{background:transparent;border-color:#c5ccd2;color:inherit}
@media(prefers-color-scheme:dark){button.deny{border-color:#2b3238}}
.warn{background:#fdf1e7;border:1px solid #e8b98c;color:#7a4510;
  padding:11px 13px;border-radius:8px;font-size:13.5px;margin:16px 0 0}
@media(prefers-color-scheme:dark){.warn{background:#33220f;border-color:#6b4a1e;color:#e6b878}}
.err{background:#fdeaea;border:1px solid #e5a0a0;color:#8a2020;
  padding:11px 13px;border-radius:8px;font-size:14px;margin:0 0 16px}
@media(prefers-color-scheme:dark){.err{background:#341b1b;border-color:#6b3030;color:#e79a9a}}
`

var consentTemplate = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="ru"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Доступ к магазину Ozon</title><style>` + pageStyle + `</style></head>
<body><div class="card">
<h1>Разрешить доступ?</h1>
<p class="sub">Приложение запрашивает доступ к вашему кабинету продавца Ozon.</p>

{{if .Error}}<div class="err">{{.Error}}</div>{{end}}

<div class="row"><span>Приложение</span><span>{{.ClientName}}</span></div>
<div class="row"><span>Вернётся на</span><span class="mono">{{.RedirectHost}}</span></div>

{{if .IsLoopback}}
<div class="warn">Приложение возвращается на адрес вашего же компьютера.
Убедитесь, что вы только что сами начали подключение — такой адрес может
занять любая программа на этой машине.</div>
{{end}}

<form method="post" action="/oauth/authorize">
<input type="hidden" name="session" value="{{.SessionID}}">

<fieldset>
<legend>Что разрешаем</legend>

<label class="check">
  <input type="checkbox" name="scope_read" checked>
  <span><b>Чтение</b><small>Каталог, цены, остатки, заказы, аналитика, финансы</small></span>
</label>

<label class="check">
  <input type="checkbox" name="scope_write" {{if .WantsWrite}}{{end}}>
  <span><b>Изменение</b><small>Создание и правка карточек, цены, остатки.
  Телефону это обычно не нужно — оставьте выключенным.</small></span>
</label>
</fieldset>

<fieldset>
<legend>Пароль владельца</legend>
<input type="password" name="password" autocomplete="current-password" autofocus required>
</fieldset>

<div class="actions">
  <button type="submit" name="action" value="deny" class="deny">Отклонить</button>
  <button type="submit" name="action" value="allow" class="allow">Разрешить</button>
</div>
</form>
</div></body></html>`))

var errorTemplate = template.Must(template.New("error").Parse(`<!doctype html>
<html lang="ru"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}}</title><style>` + pageStyle + `</style></head>
<body><div class="card">
<h1>{{.Title}}</h1>
<p class="sub">{{.Message}}</p>
</div></body></html>`))
