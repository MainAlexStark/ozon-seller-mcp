package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/MainAlexStark/ozon-seller-mcp/internal/httpx"
	"github.com/MainAlexStark/ozon-seller-mcp/internal/pgstore"
	"github.com/MainAlexStark/ozon-seller-mcp/internal/secure"
	"github.com/MainAlexStark/ozon-seller-mcp/internal/shops"
)

// flashes — сообщения после действий. Код в адресе, а не текст:
// иначе ссылкой можно подсунуть человеку любую фразу от имени сервиса.
var flashes = map[string]string{
	"shop_added":    "Магазин подключён. Теперь добавьте сервер в Claude — адрес выше.",
	"shop_ok":       "Ключ проверен: Ozon его принимает.",
	"shop_key":      "Ключ обновлён и проверен.",
	"shop_deleted":  "Магазин удалён вместе со всеми его подключениями и токенами.",
	"grant_revoked": "Подключение отозвано — приложение потеряло доступ немедленно.",
	"token_deleted": "Токен отозван.",
	"password":      "Пароль изменён. Остальные браузеры вышли из кабинета.",
}

type shopView struct {
	pgstore.Shop
	CheckedAgo string
}

type accountView struct {
	Email       string
	Plan        string
	CSRF        string
	ResourceURL string
	Flash       string
	Error       string
	Shops       []shopView
	Grants      []pgstore.Grant
	Tokens      []pgstore.APIToken
	Calls30d    int64
	NewToken    string
	NewTokenFor string
}

func (w *Web) account(rw http.ResponseWriter, r *request) {
	w.renderAccount(rw, r, http.StatusOK, flashes[r.URL.Query().Get("ok")], "", nil)
}

// renderAccount собирает кабинет. extra дописывает вид (новый токен).
func (w *Web) renderAccount(rw http.ResponseWriter, r *request, status int, flash, errMsg string, extra func(*accountView)) {
	ctx := r.Context()
	uid := r.sess.UserID

	user, err := w.db.UserByID(ctx, uid)
	if err != nil {
		w.fail(rw, err)
		return
	}
	list, err := w.db.Shops(ctx, uid)
	if err != nil {
		w.fail(rw, err)
		return
	}
	grants, err := w.db.UserGrants(ctx, uid)
	if err != nil {
		w.fail(rw, err)
		return
	}
	tokens, err := w.db.APITokens(ctx, uid)
	if err != nil {
		w.fail(rw, err)
		return
	}
	calls, err := w.db.UsageSince(ctx, uid, 30)
	if err != nil {
		w.fail(rw, err)
		return
	}

	view := accountView{
		Email:       user.Email,
		Plan:        user.Plan,
		CSRF:        r.sess.CSRF,
		ResourceURL: w.cfg.ResourceURL,
		Flash:       flash,
		Error:       errMsg,
		Grants:      grants,
		Tokens:      tokens,
		Calls30d:    calls,
	}
	for _, s := range list {
		v := shopView{Shop: s}
		if s.CheckedAt != nil {
			v.CheckedAgo = s.CheckedAt.Format("02.01.2006 15:04")
		}
		view.Shops = append(view.Shops, v)
	}
	if extra != nil {
		extra(&view)
	}
	w.render(rw, status, "account", view)
}

// --- магазины ---

type shopFormView struct {
	CSRF     string
	Next     string
	Name     string
	ClientID string
	Error    string
	First    bool
}

func (w *Web) shopForm(rw http.ResponseWriter, r *request) {
	list, err := w.db.Shops(r.Context(), r.sess.UserID)
	if err != nil {
		w.fail(rw, err)
		return
	}
	w.render(rw, http.StatusOK, "shop_new", shopFormView{
		CSRF:  r.sess.CSRF,
		Next:  safeNext(r.URL.Query().Get("next")),
		First: len(list) == 0,
	})
}

// maxShopsPerUser — потолок магазинов на пользователя: защита от
// злоупотребления, а не тарифное ограничение.
const maxShopsPerUser = 20

func (w *Web) shopAdd(rw http.ResponseWriter, r *request) {
	view := shopFormView{
		CSRF:     r.sess.CSRF,
		Next:     safeNext(r.PostFormValue("next")),
		Name:     strings.TrimSpace(r.PostFormValue("name")),
		ClientID: strings.TrimSpace(r.PostFormValue("client_id")),
	}

	list, err := w.db.Shops(r.Context(), r.sess.UserID)
	if err != nil {
		w.fail(rw, err)
		return
	}
	view.First = len(list) == 0
	if len(list) >= maxShopsPerUser {
		view.Error = "Достигнут предел магазинов на одну учётную запись."
		w.render(rw, http.StatusBadRequest, "shop_new", view)
		return
	}

	_, err = w.shops.Connect(r.Context(), r.sess.UserID, view.Name, view.ClientID, r.PostFormValue("api_key"))
	if err != nil {
		view.Error = userMessage(err)
		if view.Error == "" {
			w.logger.Error("подключение магазина", "err", err)
			view.Error = "Не удалось проверить ключ: " + err.Error()
		}
		w.render(rw, http.StatusBadRequest, "shop_new", view)
		return
	}
	w.logger.Info("магазин подключён", "user", r.sess.UserID)

	next := view.Next
	if next == "/account" {
		next = "/account?ok=shop_added"
	}
	http.Redirect(rw, r.Request, next, http.StatusFound)
}

func (w *Web) shopCheck(rw http.ResponseWriter, r *request) {
	id, ok := pathID(r)
	if !ok {
		w.notFound(rw, r.Request)
		return
	}
	if err := w.shops.Check(r.Context(), r.sess.UserID, id); err != nil {
		if errors.Is(err, pgstore.ErrNotFound) {
			w.notFound(rw, r.Request)
			return
		}
		msg := userMessage(err)
		if msg == "" {
			msg = err.Error()
		}
		w.renderAccount(rw, r, http.StatusOK, "", "Проверка не прошла: "+msg, nil)
		return
	}
	http.Redirect(rw, r.Request, "/account?ok=shop_ok", http.StatusFound)
}

func (w *Web) shopKey(rw http.ResponseWriter, r *request) {
	id, ok := pathID(r)
	if !ok {
		w.notFound(rw, r.Request)
		return
	}
	if err := w.shops.UpdateKey(r.Context(), r.sess.UserID, id, r.PostFormValue("api_key")); err != nil {
		if errors.Is(err, pgstore.ErrNotFound) {
			w.notFound(rw, r.Request)
			return
		}
		msg := userMessage(err)
		if msg == "" {
			msg = err.Error()
		}
		w.renderAccount(rw, r, http.StatusBadRequest, "", "Ключ не обновлён: "+msg, nil)
		return
	}
	http.Redirect(rw, r.Request, "/account?ok=shop_key", http.StatusFound)
}

func (w *Web) shopDelete(rw http.ResponseWriter, r *request) {
	id, ok := pathID(r)
	if !ok {
		w.notFound(rw, r.Request)
		return
	}
	if err := w.db.DeleteShop(r.Context(), r.sess.UserID, id); err != nil {
		if errors.Is(err, pgstore.ErrNotFound) {
			w.notFound(rw, r.Request)
			return
		}
		w.fail(rw, err)
		return
	}
	w.logger.Info("магазин удалён", "user", r.sess.UserID, "shop", id)
	http.Redirect(rw, r.Request, "/account?ok=shop_deleted", http.StatusFound)
}

// --- подключения ---

func (w *Web) grantRevoke(rw http.ResponseWriter, r *request) {
	if err := w.db.RevokeUserGrant(r.Context(), r.sess.UserID, r.PathValue("id")); err != nil {
		if errors.Is(err, pgstore.ErrNotFound) {
			w.notFound(rw, r.Request)
			return
		}
		w.fail(rw, err)
		return
	}
	http.Redirect(rw, r.Request, "/account?ok=grant_revoked", http.StatusFound)
}

// --- токены ---

const maxTokensPerUser = 20

func (w *Web) tokenCreate(rw http.ResponseWriter, r *request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		name = "Токен"
	}
	if utf8.RuneCountInString(name) > 60 {
		w.renderAccount(rw, r, http.StatusBadRequest, "", "Название токена длиннее 60 символов.", nil)
		return
	}
	shopID, _ := strconv.ParseInt(r.PostFormValue("shop"), 10, 64)

	existing, err := w.db.APITokens(r.Context(), r.sess.UserID)
	if err != nil {
		w.fail(rw, err)
		return
	}
	if len(existing) >= maxTokensPerUser {
		w.renderAccount(rw, r, http.StatusBadRequest, "", "Достигнут предел токенов — отзовите ненужные.", nil)
		return
	}

	scopes := []string{"read"}
	if r.PostFormValue("write") == "on" {
		scopes = append(scopes, "write")
	}

	raw, err := secure.RandomToken()
	if err != nil {
		w.fail(rw, err)
		return
	}
	token := httpx.APITokenPrefix + raw

	if _, err := w.db.CreateAPIToken(r.Context(), r.sess.UserID, shopID, name,
		secure.HashToken(token), scopes); err != nil {
		if errors.Is(err, pgstore.ErrNotFound) {
			w.renderAccount(rw, r, http.StatusBadRequest, "", "Выберите магазин для токена.", nil)
			return
		}
		w.fail(rw, err)
		return
	}
	w.logger.Info("выпущен API-токен", "user", r.sess.UserID, "shop", shopID, "scopes", scopes)

	// Токен показывается один раз, прямо в ответе на POST: в базе
	// только хеш, и повторить его потом невозможно.
	w.renderAccount(rw, r, http.StatusOK, "", "", func(v *accountView) {
		v.NewToken = token
		v.NewTokenFor = name
	})
}

func (w *Web) tokenDelete(rw http.ResponseWriter, r *request) {
	id, ok := pathID(r)
	if !ok {
		w.notFound(rw, r.Request)
		return
	}
	if err := w.db.DeleteAPIToken(r.Context(), r.sess.UserID, id); err != nil {
		if errors.Is(err, pgstore.ErrNotFound) {
			w.notFound(rw, r.Request)
			return
		}
		w.fail(rw, err)
		return
	}
	http.Redirect(rw, r.Request, "/account?ok=token_deleted", http.StatusFound)
}

// --- учётная запись ---

func (w *Web) passwordChange(rw http.ResponseWriter, r *request) {
	if !w.limiter.allow(w.cfg.ClientIP(r.Request)) {
		w.message(rw, http.StatusTooManyRequests, "Слишком много попыток", "Подождите минуту.")
		return
	}
	user, err := w.db.UserByID(r.Context(), r.sess.UserID)
	if err != nil {
		w.fail(rw, err)
		return
	}
	if !secure.VerifyPassword(user.PasswordHash, r.PostFormValue("current")) {
		w.renderAccount(rw, r, http.StatusBadRequest, "", "Текущий пароль указан неверно.", nil)
		return
	}
	next := r.PostFormValue("new")
	if next != r.PostFormValue("new2") {
		w.renderAccount(rw, r, http.StatusBadRequest, "", "Новые пароли не совпадают.", nil)
		return
	}
	hash, err := secure.HashPassword(next)
	if errors.Is(err, secure.ErrShortPassword) {
		w.renderAccount(rw, r, http.StatusBadRequest, "", "Новый пароль короче 8 символов.", nil)
		return
	}
	if err != nil {
		w.fail(rw, err)
		return
	}
	if err := w.db.SetPassword(r.Context(), user.ID, hash, r.sessHash); err != nil {
		w.fail(rw, err)
		return
	}
	http.Redirect(rw, r.Request, "/account?ok=password", http.StatusFound)
}

func (w *Web) accountDelete(rw http.ResponseWriter, r *request) {
	if !w.limiter.allow(w.cfg.ClientIP(r.Request)) {
		w.message(rw, http.StatusTooManyRequests, "Слишком много попыток", "Подождите минуту.")
		return
	}
	user, err := w.db.UserByID(r.Context(), r.sess.UserID)
	if err != nil {
		w.fail(rw, err)
		return
	}
	if !secure.VerifyPassword(user.PasswordHash, r.PostFormValue("password")) {
		w.renderAccount(rw, r, http.StatusBadRequest, "", "Пароль указан неверно — учётная запись не удалена.", nil)
		return
	}
	if err := w.db.DeleteUser(r.Context(), user.ID); err != nil {
		w.fail(rw, err)
		return
	}
	w.logger.Info("учётная запись удалена", "user", user.ID)
	w.clearCookie(rw)
	w.message(rw, http.StatusOK, "Учётная запись удалена",
		"Ключи магазинов, подключения и токены удалены. Доступ всех приложений прекращён.")
}

func pathID(r *request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

// userMessage — текст ошибки, который можно показать человеку.
func userMessage(err error) string {
	var in *shops.InputError
	if errors.As(err, &in) {
		return in.Msg
	}
	return ""
}
