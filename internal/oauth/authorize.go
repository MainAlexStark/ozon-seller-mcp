package oauth

import (
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/MainAlexStark/ozon-seller-mcp/internal/ui"
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

	client, err := s.store.Client(r.Context(), clientID)
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

	// Кто подключает. Не вошёл — на вход и обратно сюда же: весь
	// запрос авторизации переживает круг через форму входа в адресе.
	user, ok := s.accounts.CurrentUser(r)
	if !ok {
		http.Redirect(w, r, s.accounts.LoginURL(r.URL.RequestURI()), http.StatusFound)
		return
	}

	shops, err := s.accounts.Shops(r.Context(), user.ID)
	if err != nil {
		s.redirectError(w, r, redirectURI, q.Get("state"), "server_error", "не удалось прочитать магазины")
		return
	}
	// Без магазина подключать нечего: сначала ключ Ozon, потом сюда.
	if len(shops) == 0 {
		http.Redirect(w, r, s.accounts.AddShopURL(r.URL.RequestURI()), http.StatusFound)
		return
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
		UserID:        user.ID,
		ExpiresAt:     time.Now().Add(LoginSessionTTL),
	}
	if err := s.store.SaveSession(r.Context(), sess); err != nil {
		s.redirectError(w, r, redirectURI, q.Get("state"), "server_error", "не удалось сохранить сессию")
		return
	}

	s.renderConsent(w, client, sess, user, shops, "")
}

// handleAuthorizeSubmit — пользователь выбрал магазин и права.
func (s *Server) handleAuthorizeSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, http.StatusBadRequest, "Не разобрана форма", err.Error())
		return
	}

	ctx := r.Context()
	sessionID := r.PostFormValue("session")
	sess, err := s.store.Session(ctx, sessionID)
	if err != nil {
		s.renderError(w, http.StatusBadRequest,
			"Время истекло",
			"Страница подтверждения устарела. Начните подключение в Claude заново.")
		return
	}

	client, err := s.store.Client(ctx, sess.ClientID)
	if err != nil {
		s.renderError(w, http.StatusBadRequest, "Неизвестное приложение", "Клиент не найден.")
		return
	}

	// Подтвердить может только тот, кто открыл форму. Номер сессии
	// в скрытом поле непредсказуем, но сверка с cookie закрывает и тот
	// случай, когда он всё же утёк: чужим браузером его не отправить.
	user, ok := s.accounts.CurrentUser(r)
	if !ok || user.ID != sess.UserID {
		s.renderError(w, http.StatusForbidden,
			"Сессия не ваша",
			"Эта страница подтверждения открыта под другой учётной записью. Начните подключение в Claude заново.")
		return
	}

	// Отказ — обычный исход, а не ошибка.
	if r.PostFormValue("action") != "allow" {
		_, _ = s.store.TakeSession(ctx, sessionID)
		s.redirectError(w, r, sess.RedirectURI, sess.State, "access_denied", "пользователь отклонил подключение")
		return
	}

	shops, err := s.accounts.Shops(ctx, user.ID)
	if err != nil {
		s.renderError(w, http.StatusInternalServerError, "Ошибка", "Не удалось прочитать магазины.")
		return
	}

	// Магазин сверяется со списком самого пользователя: номер из формы —
	// это ввод, и чужой номер не должен давать доступ к чужому магазину.
	shopID, _ := strconv.ParseInt(r.PostFormValue("shop"), 10, 64)
	var shop *Shop
	for i := range shops {
		if shops[i].ID == shopID {
			shop = &shops[i]
		}
	}
	if shop == nil {
		s.renderConsent(w, client, sess, user, shops, "Выберите магазин.")
		return
	}

	// Права берём из галочек, а не из того, что запросил клиент:
	// смысл экрана согласия в том, что решает пользователь. Снятая галочка
	// записи — это токен, которым магазин изменить нельзя.
	granted := []string{}
	if r.PostFormValue("scope_read") == "on" {
		granted = append(granted, ScopeRead)
	}
	if r.PostFormValue("scope_write") == "on" {
		granted = append(granted, ScopeWrite)
	}
	if len(granted) == 0 {
		s.renderConsent(w, client, sess, user, shops, "Выберите хотя бы одно право, иначе подключать нечего.")
		return
	}
	// offline_access отдаём, если его просили: без него Claude будет
	// требовать подтверждение каждый час.
	if containsScope(sess.Scopes, ScopeOfflineAccess) {
		granted = append(granted, ScopeOfflineAccess)
	}

	if _, err := s.store.TakeSession(ctx, sessionID); err != nil {
		s.renderError(w, http.StatusBadRequest, "Время истекло", "Начните подключение заново.")
		return
	}

	code, err := randomToken()
	if err != nil {
		s.redirectError(w, r, sess.RedirectURI, sess.State, "server_error", "не удалось выдать код")
		return
	}

	err = s.store.SaveCode(ctx, &AuthCode{
		CodeHash:      hashToken(code),
		Subject:       Subject{UserID: user.ID, ShopID: shop.ID},
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
		"user", user.ID,
		"shop", shop.ID,
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
	Email        string
	Shops        []Shop
	Selected     int64
	Style        template.CSS
	Error        string
}

// renderConsent показывает экран согласия.
func (s *Server) renderConsent(w http.ResponseWriter, client *Client, sess *LoginSession, user User, shops []Shop, errMsg string) {
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
		Email:        user.Email,
		Shops:        shops,
		Style:        ui.Style,
		Error:        errMsg,
	}
	if len(shops) > 0 {
		view.Selected = shops[0].ID
	}

	ui.NoCache(w)
	if errMsg != "" {
		w.WriteHeader(http.StatusBadRequest)
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
	ui.RenderMessage(w, status, title, message)
}

var consentTemplate = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="ru"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Доступ к магазину Ozon</title><style>{{.Style}}</style></head>
<body><div class="card">
<h1>Разрешить доступ?</h1>
<p class="sub">Приложение запрашивает доступ к вашему кабинету продавца Ozon.</p>

{{if .Error}}<div class="err">{{.Error}}</div>{{end}}

<div class="row"><span>Приложение</span><span>{{.ClientName}}</span></div>
<div class="row"><span>Вернётся на</span><span class="mono">{{.RedirectHost}}</span></div>
<div class="row"><span>Учётная запись</span><span>{{.Email}}</span></div>

{{if .IsLoopback}}
<div class="warn">Приложение возвращается на адрес вашего же компьютера.
Убедитесь, что вы только что сами начали подключение — такой адрес может
занять любая программа на этой машине.</div>
{{end}}

<form method="post" action="/oauth/authorize">
<input type="hidden" name="session" value="{{.SessionID}}">

<fieldset>
<legend>Магазин</legend>
{{$sel := .Selected}}
{{range .Shops}}
<label class="check">
  <input type="radio" name="shop" value="{{.ID}}" {{if eq .ID $sel}}checked{{end}}>
  <span><b>{{.Name}}</b><small>Client-Id {{.ClientID}}</small></span>
</label>
{{end}}
<p class="hint"><a href="/account">Подключить другой магазин</a></p>
</fieldset>

<fieldset>
<legend>Что разрешаем</legend>

<label class="check">
  <input type="checkbox" name="scope_read" checked>
  <span><b>Чтение</b><small>Каталог, цены, остатки, заказы, аналитика, финансы</small></span>
</label>

<label class="check">
  <input type="checkbox" name="scope_write">
  <span><b>Изменение</b><small>Создание и правка карточек, цены, остатки.
  {{if .WantsWrite}}Приложение просит это право. {{end}}Телефону обычно не нужно — оставьте выключенным.</small></span>
</label>
</fieldset>

<div class="actions">
  <button type="submit" name="action" value="deny" class="deny">Отклонить</button>
  <button type="submit" name="action" value="allow" class="allow">Разрешить</button>
</div>
</form>
</div></body></html>`))
