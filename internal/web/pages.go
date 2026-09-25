package web

import (
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"sync"

	"github.com/MainAlexStark/ozon-seller-mcp/internal/pgstore"
	"github.com/MainAlexStark/ozon-seller-mcp/internal/secure"
)

// dummyHash — хеш, против которого проверяется пароль несуществующего
// пользователя. Без этого по времени ответа видно, зарегистрирован ли
// адрес: PBKDF2 считается только для настоящих учётных записей.
var dummyHash = sync.OnceValue(func() string {
	h, _ := secure.HashPassword("несуществующий-пользователь")
	return h
})

type authForm struct {
	Email      string
	Next       string
	Error      string
	SignupOpen bool
}

func (w *Web) landing(rw http.ResponseWriter, r *http.Request) {
	if _, _, ok := w.session(r); ok {
		http.Redirect(rw, r, "/account", http.StatusFound)
		return
	}
	w.render(rw, http.StatusOK, "landing", map[string]any{
		"SignupOpen":  w.cfg.SignupOpen,
		"ResourceURL": w.cfg.ResourceURL,
	})
}

func (w *Web) notFound(rw http.ResponseWriter, _ *http.Request) {
	w.message(rw, http.StatusNotFound, "Страница не найдена", "Такой страницы здесь нет.")
}

// --- регистрация ---

func (w *Web) signupForm(rw http.ResponseWriter, r *http.Request) {
	if !w.cfg.SignupOpen {
		w.message(rw, http.StatusForbidden, "Регистрация закрыта", "Сейчас новые учётные записи не создаются.")
		return
	}
	w.render(rw, http.StatusOK, "signup", authForm{Next: safeNext(r.URL.Query().Get("next"))})
}

func (w *Web) signup(rw http.ResponseWriter, r *http.Request) {
	if !w.cfg.SignupOpen {
		w.message(rw, http.StatusForbidden, "Регистрация закрыта", "Сейчас новые учётные записи не создаются.")
		return
	}
	form := authForm{
		Email: strings.TrimSpace(r.PostFormValue("email")),
		Next:  safeNext(r.PostFormValue("next")),
	}
	fail := func(msg string) {
		form.Error = msg
		w.render(rw, http.StatusBadRequest, "signup", form)
	}

	email, ok := validEmail(form.Email)
	if !ok {
		fail("Это не похоже на адрес почты.")
		return
	}
	password := r.PostFormValue("password")
	if password != r.PostFormValue("password2") {
		fail("Пароли не совпадают.")
		return
	}
	hash, err := secure.HashPassword(password)
	if errors.Is(err, secure.ErrShortPassword) {
		fail("Пароль короче 8 символов: он защищает доступ к вашему магазину.")
		return
	}
	if err != nil {
		w.fail(rw, err)
		return
	}

	user, err := w.db.CreateUser(r.Context(), email, hash)
	if errors.Is(err, pgstore.ErrEmailTaken) {
		fail("Этот адрес уже зарегистрирован. Войдите или используйте другой.")
		return
	}
	if err != nil {
		w.fail(rw, err)
		return
	}
	w.logger.Info("регистрация", "user", user.ID)

	if err := w.startSession(rw, r, user.ID); err != nil {
		w.fail(rw, err)
		return
	}
	// Сразу к подключению магазина: без него сервисом пользоваться
	// нечем. Возврат на next (например, к согласию OAuth) сохраняется.
	http.Redirect(rw, r, w.AddShopURL(form.Next), http.StatusFound)
}

// validEmail проверяет адрес и приводит его к хранимому виду.
func validEmail(s string) (string, bool) {
	if len(s) > 254 || strings.ContainsAny(s, " \t\r\n<>") {
		return "", false
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || !strings.Contains(s[strings.LastIndexByte(s, '@')+1:], ".") {
		return "", false
	}
	return pgstore.NormalizeEmail(s), true
}

// --- вход ---

func (w *Web) loginForm(rw http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))
	if _, _, ok := w.session(r); ok {
		http.Redirect(rw, r, next, http.StatusFound)
		return
	}
	w.render(rw, http.StatusOK, "login", authForm{Next: next, SignupOpen: w.cfg.SignupOpen})
}

func (w *Web) login(rw http.ResponseWriter, r *http.Request) {
	form := authForm{
		Email:      strings.TrimSpace(r.PostFormValue("email")),
		Next:       safeNext(r.PostFormValue("next")),
		SignupOpen: w.cfg.SignupOpen,
	}
	password := r.PostFormValue("password")

	user, err := w.db.UserByEmail(r.Context(), form.Email)
	if err != nil && !errors.Is(err, pgstore.ErrNotFound) {
		w.fail(rw, err)
		return
	}
	hash := user.PasswordHash
	if err != nil {
		hash = dummyHash()
	}
	if !secure.VerifyPassword(hash, password) || err != nil {
		w.logger.Warn("неудачный вход", "remote", w.cfg.ClientIP(r))
		// Одна формулировка на «нет такого адреса» и «не тот пароль»:
		// иначе форма входа отвечает, кто зарегистрирован в сервисе.
		form.Error = "Неверная почта или пароль."
		w.render(rw, http.StatusUnauthorized, "login", form)
		return
	}
	if user.Disabled {
		form.Error = "Учётная запись заблокирована. Напишите в поддержку."
		w.render(rw, http.StatusForbidden, "login", form)
		return
	}

	if err := w.startSession(rw, r, user.ID); err != nil {
		w.fail(rw, err)
		return
	}
	http.Redirect(rw, r, form.Next, http.StatusFound)
}

func (w *Web) logout(rw http.ResponseWriter, r *request) {
	_ = w.db.DeleteWebSession(r.Context(), r.sessHash)
	w.clearCookie(rw)
	http.Redirect(rw, r.Request, "/login", http.StatusFound)
}

// --- общее ---

func (w *Web) message(rw http.ResponseWriter, status int, title, msg string) {
	w.render(rw, status, "message", map[string]string{"Title": title, "Message": msg})
}

// fail — непредвиденная ошибка: подробности в журнал, человеку — общая фраза.
func (w *Web) fail(rw http.ResponseWriter, err error) {
	w.logger.Error("ошибка страницы", "err", err)
	w.message(rw, http.StatusInternalServerError, "Что-то сломалось",
		"Мы уже видим ошибку в журнале. Попробуйте ещё раз через минуту.")
}
