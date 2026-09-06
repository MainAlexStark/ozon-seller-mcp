package oauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"
)

// handleToken — выдача и обновление токенов.
//
// Тело — форма (application/x-www-form-urlencoded), как требует RFC 6749.
// Соседний /oauth/register принимает JSON; перепутать их легко, а платой
// будет 415 в середине потока.
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "не разобрано тело запроса")
		return
	}

	switch r.PostFormValue("grant_type") {
	case "authorization_code":
		s.exchangeCode(w, r)
	case "refresh_token":
		s.refresh(w, r)
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type",
			"поддерживаются authorization_code и refresh_token")
	}
}

// exchangeCode обменивает код авторизации на токены.
func (s *Server) exchangeCode(w http.ResponseWriter, r *http.Request) {
	code := r.PostFormValue("code")
	if code == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "не передан code")
		return
	}

	// Код извлекается и сразу удаляется: он одноразовый.
	authCode, err := s.store.TakeCode(code)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "код неизвестен или истёк")
		return
	}

	if authCode.ClientID != r.PostFormValue("client_id") {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "код выдан другому клиенту")
		return
	}
	if authCode.RedirectURI != r.PostFormValue("redirect_uri") {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri не совпадает с тем, для которого выдан код")
		return
	}

	// PKCE: только тот, кто начинал поток, знает verifier.
	verifier := r.PostFormValue("code_verifier")
	if !verifyPKCE(authCode.CodeChallenge, verifier) {
		s.logger.Warn("не сошёлся code_verifier", "client_id", authCode.ClientID)
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "code_verifier не соответствует code_challenge")
		return
	}

	// RFC 8707: если клиент называет ресурс, он должен быть нашим.
	if res := r.PostFormValue("resource"); res != "" && !sameResource(res, authCode.Resource) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_target", "токен запрошен для другого ресурса")
		return
	}

	s.issue(w, authCode.ClientID, authCode.Scopes, authCode.Resource)
}

// refresh обновляет пару токенов.
func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	token := r.PostFormValue("refresh_token")
	if token == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "не передан refresh_token")
		return
	}

	// Извлечение удаляет старый refresh — это ротация, которой OAuth 2.1
	// требует для публичных клиентов.
	rt, err := s.store.TakeRefreshToken(token)
	if err != nil {
		// Именно invalid_grant: по этому коду Claude поймёт, что надо
		// заново пройти согласие. На другой код он будет повторять
		// обновление и не покажет пользователю, что делать.
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "refresh-токен неизвестен, отозван или истёк")
		return
	}

	if cid := r.PostFormValue("client_id"); cid != "" && cid != rt.ClientID {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "токен выдан другому клиенту")
		return
	}

	scopes := rt.Scopes
	// Клиент вправе попросить меньше прав, чем имеет, но не больше.
	if requested := parseScopes(r.PostFormValue("scope")); len(requested) > 0 {
		narrowed, ok := narrowScopes(rt.Scopes, requested)
		if !ok {
			writeOAuthError(w, http.StatusBadRequest, "invalid_scope", "запрошены права шире выданных")
			return
		}
		scopes = narrowed
	}

	// Старая выдача уничтожается целиком: оставить прежний access
	// означало бы, что отзыв ничего не отзывает.
	if _, err := s.store.RevokeGrant(rt.GrantID); err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "не удалось обновить выдачу")
		return
	}

	s.issue(w, rt.ClientID, scopes, rt.Resource)
}

// issue выпускает пару токенов и отдаёт ответ.
func (s *Server) issue(w http.ResponseWriter, clientID string, scopes []string, resource string) {
	access, err := randomToken()
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "не удалось выдать токен")
		return
	}
	grantID, err := randomToken()
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "не удалось выдать токен")
		return
	}

	now := time.Now()
	at := &Token{
		ClientID:  clientID,
		Scopes:    scopes,
		Resource:  resource,
		IssuedAt:  now,
		ExpiresAt: now.Add(AccessTokenTTL),
		GrantID:   grantID,
	}

	resp := map[string]any{
		"access_token": access,
		"token_type":   "Bearer",
		"expires_in":   int(AccessTokenTTL.Seconds()),
		"scope":        strings.Join(scopes, " "),
	}

	var (
		refresh string
		rt      *RefreshToken
	)
	// Refresh выдаём только тем, кто просил offline_access. Иначе
	// доступ, который человек считал разовым, продлевался бы месяц.
	if containsScope(scopes, ScopeOfflineAccess) {
		refresh, err = randomToken()
		if err != nil {
			writeOAuthError(w, http.StatusInternalServerError, "server_error", "не удалось выдать токен обновления")
			return
		}
		rt = &RefreshToken{
			ClientID:  clientID,
			Scopes:    scopes,
			Resource:  resource,
			ExpiresAt: now.Add(RefreshTokenTTL),
			GrantID:   grantID,
		}
		resp["refresh_token"] = refresh
	}

	if err := s.store.SaveTokens(access, at, refresh, rt); err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "не удалось сохранить токен")
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleRevoke — RFC 7009.
//
// Отзыв работает по любому токену пары и убивает всю выдачу: оставшийся
// refresh иначе тут же выпустил бы новый access.
func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "не разобрано тело запроса")
		return
	}

	token := r.PostFormValue("token")
	if grantID, ok := s.store.GrantIDByToken(token); ok {
		n, err := s.store.RevokeGrant(grantID)
		if err != nil {
			writeOAuthError(w, http.StatusInternalServerError, "server_error", "не удалось отозвать")
			return
		}
		s.logger.Info("выдача отозвана", "grant", grantID, "tokens", n)
	}

	// RFC 7009: неизвестный токен — тоже успех. Иначе эндпоинт
	// превращается в оракул, отвечающий, существует ли токен.
	w.WriteHeader(http.StatusOK)
}

// ErrInvalidToken — токен не принят.
var ErrInvalidToken = errors.New("oauth: недействительный токен")

// Validate проверяет предъявленный access-токен.
//
// Проверка audience обязательна: сервер не должен принимать токен,
// выпущенный для другого ресурса, даже если он подлинный. Без этого
// токен, выданный соседнему сервису, открывал бы и этот.
func (s *Server) Validate(token string) (*Token, error) {
	t, err := s.store.AccessToken(token)
	if err != nil {
		return nil, ErrInvalidToken
	}
	if !sameResource(t.Resource, s.cfg.ResourceURL) {
		s.logger.Warn("токен выпущен для другого ресурса",
			"expected", s.cfg.ResourceURL, "got", t.Resource)
		return nil, ErrInvalidToken
	}
	return t, nil
}

// verifyPKCE сверяет verifier с challenge по методу S256.
func verifyPKCE(challenge, verifier string) bool {
	if challenge == "" || verifier == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}

// narrowScopes оставляет только те права, что уже были выданы.
func narrowScopes(granted, requested []string) ([]string, bool) {
	out := make([]string, 0, len(requested))
	for _, r := range requested {
		if !containsScope(granted, r) {
			return nil, false
		}
		out = append(out, r)
	}
	return out, true
}

// HasScope сообщает, есть ли у токена нужное право.
func (t *Token) HasScope(scope string) bool { return containsScope(t.Scopes, scope) }
