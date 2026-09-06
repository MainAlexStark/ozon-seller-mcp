package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Store — хранилище клиентов, кодов и токенов.
//
// Хранится на диске и переживает перезапуск. Это не удобство, а условие
// работоспособности: без него каждый перезапуск сервера отключал бы все
// устройства и требовал заново проходить согласие.
//
// Токены непрозрачные (случайные строки), а не подписанные JWT.
// Подписанный токен нельзя отозвать до истечения срока, не заводя
// отдельный список отозванных, — а возможность отозвать доступ и есть
// главная причина, по которой мы вообще ушли от статического секрета.
//
// На диск кладётся не сам токен, а его SHA-256: утечка файла тогда не
// даёт доступа. Пароли и секреты клиентов — тем же способом.
type Store struct {
	mu   sync.RWMutex
	path string

	data storeData
}

type storeData struct {
	Clients       map[string]*Client       `json:"clients"`
	Codes         map[string]*AuthCode     `json:"codes"`
	AccessTokens  map[string]*Token        `json:"access_tokens"`
	RefreshTokens map[string]*RefreshToken `json:"refresh_tokens"`
	Sessions      map[string]*LoginSession `json:"sessions"`
}

// Client — зарегистрированный OAuth-клиент.
type Client struct {
	ID           string    `json:"client_id"`
	SecretHash   string    `json:"client_secret_hash,omitempty"`
	Name         string    `json:"client_name"`
	RedirectURIs []string  `json:"redirect_uris"`
	CreatedAt    time.Time `json:"created_at"`
}

// AuthCode — выданный код авторизации.
type AuthCode struct {
	CodeHash      string    `json:"code_hash"`
	ClientID      string    `json:"client_id"`
	RedirectURI   string    `json:"redirect_uri"`
	Scopes        []string  `json:"scopes"`
	Resource      string    `json:"resource"`
	CodeChallenge string    `json:"code_challenge"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// Token — выданный access-токен.
type Token struct {
	TokenHash string    `json:"token_hash"`
	ClientID  string    `json:"client_id"`
	Scopes    []string  `json:"scopes"`
	Resource  string    `json:"resource"`
	ExpiresAt time.Time `json:"expires_at"`
	IssuedAt  time.Time `json:"issued_at"`

	// GrantID связывает access и refresh одной выдачи: отзыв одного
	// должен убивать всю пару, иначе отозванный доступ воскресает
	// первым же обновлением.
	GrantID string `json:"grant_id"`
}

// RefreshToken — токен обновления.
type RefreshToken struct {
	TokenHash string    `json:"token_hash"`
	ClientID  string    `json:"client_id"`
	Scopes    []string  `json:"scopes"`
	Resource  string    `json:"resource"`
	ExpiresAt time.Time `json:"expires_at"`
	GrantID   string    `json:"grant_id"`
}

// LoginSession — незавершённый вход: пользователь открыл форму
// согласия, но ещё не подтвердил.
type LoginSession struct {
	ID            string    `json:"id"`
	ClientID      string    `json:"client_id"`
	RedirectURI   string    `json:"redirect_uri"`
	State         string    `json:"state"`
	Scopes        []string  `json:"scopes"`
	Resource      string    `json:"resource"`
	CodeChallenge string    `json:"code_challenge"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// ErrNotFound — записи нет либо она истекла.
var ErrNotFound = errors.New("oauth: запись не найдена")

// NewStore открывает или создаёт хранилище по указанному пути.
func NewStore(path string) (*Store, error) {
	s := &Store{
		path: path,
		data: storeData{
			Clients:       map[string]*Client{},
			Codes:         map[string]*AuthCode{},
			AccessTokens:  map[string]*Token{},
			RefreshTokens: map[string]*RefreshToken{},
			Sessions:      map[string]*LoginSession{},
		},
	}
	if path == "" {
		return s, nil // хранилище только в памяти (тесты)
	}

	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, s.flush()
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &s.data); err != nil {
		return nil, err
	}
	s.ensureMaps()
	return s, nil
}

func (s *Store) ensureMaps() {
	if s.data.Clients == nil {
		s.data.Clients = map[string]*Client{}
	}
	if s.data.Codes == nil {
		s.data.Codes = map[string]*AuthCode{}
	}
	if s.data.AccessTokens == nil {
		s.data.AccessTokens = map[string]*Token{}
	}
	if s.data.RefreshTokens == nil {
		s.data.RefreshTokens = map[string]*RefreshToken{}
	}
	if s.data.Sessions == nil {
		s.data.Sessions = map[string]*LoginSession{}
	}
}

// flush пишет состояние на диск атомарно: сначала во временный файл,
// потом переименование. Иначе падение в момент записи оставило бы
// обрезанный файл, и при следующем запуске отвалились бы все устройства.
func (s *Store) flush() error {
	if s.path == "" {
		return nil
	}

	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".oauth-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	// Права 600: внутри хеши секретов, файл не для чужих глаз.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}

// --- Клиенты ---

// SaveClient сохраняет клиента.
func (s *Store) SaveClient(c *Client) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Clients[c.ID] = c
	return s.flush()
}

// Client возвращает клиента по идентификатору.
func (s *Store) Client(id string) (*Client, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.data.Clients[id]
	if !ok {
		return nil, ErrNotFound
	}
	return c, nil
}

// --- Сессии входа ---

// SaveSession сохраняет незавершённый вход.
func (s *Store) SaveSession(sess *LoginSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Sessions[sess.ID] = sess
	return s.flush()
}

// TakeSession извлекает сессию и сразу удаляет её: форма согласия
// одноразовая.
func (s *Store) TakeSession(id string) (*LoginSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.data.Sessions[id]
	if !ok {
		return nil, ErrNotFound
	}
	delete(s.data.Sessions, id)
	if err := s.flush(); err != nil {
		return nil, err
	}
	if time.Now().After(sess.ExpiresAt) {
		return nil, ErrNotFound
	}
	return sess, nil
}

// Session читает сессию, не удаляя (для показа формы).
func (s *Store) Session(id string) (*LoginSession, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sess, ok := s.data.Sessions[id]
	if !ok || time.Now().After(sess.ExpiresAt) {
		return nil, ErrNotFound
	}
	return sess, nil
}

// --- Коды авторизации ---

// SaveCode сохраняет код.
func (s *Store) SaveCode(code string, c *AuthCode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c.CodeHash = hashToken(code)
	s.data.Codes[c.CodeHash] = c
	return s.flush()
}

// TakeCode извлекает код и удаляет его.
//
// Удаление обязательно и немедленно: код одноразовый, а повторное
// использование — признак того, что его перехватили.
func (s *Store) TakeCode(code string) (*AuthCode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	h := hashToken(code)
	c, ok := s.data.Codes[h]
	if !ok {
		return nil, ErrNotFound
	}
	delete(s.data.Codes, h)
	if err := s.flush(); err != nil {
		return nil, err
	}
	if time.Now().After(c.ExpiresAt) {
		return nil, ErrNotFound
	}
	return c, nil
}

// --- Токены ---

// SaveTokens сохраняет пару access + refresh одной выдачи.
func (s *Store) SaveTokens(access string, at *Token, refresh string, rt *RefreshToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	at.TokenHash = hashToken(access)
	s.data.AccessTokens[at.TokenHash] = at

	if refresh != "" && rt != nil {
		rt.TokenHash = hashToken(refresh)
		s.data.RefreshTokens[rt.TokenHash] = rt
	}
	return s.flush()
}

// AccessToken находит действующий access-токен.
func (s *Store) AccessToken(token string) (*Token, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	t, ok := s.data.AccessTokens[hashToken(token)]
	if !ok {
		return nil, ErrNotFound
	}
	if time.Now().After(t.ExpiresAt) {
		return nil, ErrNotFound
	}
	return t, nil
}

// TakeRefreshToken извлекает refresh и удаляет его.
//
// Удаление при использовании — это ротация, которой требует OAuth 2.1
// для публичных клиентов: перехваченный refresh становится бесполезен,
// как только настоящий клиент им воспользуется.
func (s *Store) TakeRefreshToken(token string) (*RefreshToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	h := hashToken(token)
	rt, ok := s.data.RefreshTokens[h]
	if !ok {
		return nil, ErrNotFound
	}
	delete(s.data.RefreshTokens, h)
	if err := s.flush(); err != nil {
		return nil, err
	}
	if time.Now().After(rt.ExpiresAt) {
		return nil, ErrNotFound
	}
	return rt, nil
}

// RevokeGrant удаляет всю выдачу целиком — и access, и refresh.
//
// Отзывать по одному токену бессмысленно: оставшийся refresh тут же
// выпустит новый access, и доступ вернётся.
func (s *Store) RevokeGrant(grantID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := 0
	for h, t := range s.data.AccessTokens {
		if t.GrantID == grantID {
			delete(s.data.AccessTokens, h)
			n++
		}
	}
	for h, rt := range s.data.RefreshTokens {
		if rt.GrantID == grantID {
			delete(s.data.RefreshTokens, h)
			n++
		}
	}
	return n, s.flush()
}

// GrantIDByToken находит выдачу по любому из её токенов.
func (s *Store) GrantIDByToken(token string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	h := hashToken(token)
	if t, ok := s.data.AccessTokens[h]; ok {
		return t.GrantID, true
	}
	if rt, ok := s.data.RefreshTokens[h]; ok {
		return rt.GrantID, true
	}
	return "", false
}

// Grants перечисляет действующие выдачи для показа владельцу.
type Grant struct {
	GrantID    string
	ClientID   string
	ClientName string
	Scopes     []string
	IssuedAt   time.Time
	ExpiresAt  time.Time
}

// Grants возвращает список действующих выдач.
func (s *Store) Grants() []Grant {
	s.mu.RLock()
	defer s.mu.RUnlock()

	seen := map[string]Grant{}
	for _, t := range s.data.AccessTokens {
		if time.Now().After(t.ExpiresAt) {
			continue
		}
		name := t.ClientID
		if c, ok := s.data.Clients[t.ClientID]; ok && c.Name != "" {
			name = c.Name
		}
		seen[t.GrantID] = Grant{
			GrantID:    t.GrantID,
			ClientID:   t.ClientID,
			ClientName: name,
			Scopes:     t.Scopes,
			IssuedAt:   t.IssuedAt,
			ExpiresAt:  t.ExpiresAt,
		}
	}

	out := make([]Grant, 0, len(seen))
	for _, g := range seen {
		out = append(out, g)
	}
	return out
}

// Cleanup удаляет истёкшие записи.
func (s *Store) Cleanup() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for h, c := range s.data.Codes {
		if now.After(c.ExpiresAt) {
			delete(s.data.Codes, h)
		}
	}
	for h, t := range s.data.AccessTokens {
		if now.After(t.ExpiresAt) {
			delete(s.data.AccessTokens, h)
		}
	}
	for h, rt := range s.data.RefreshTokens {
		if now.After(rt.ExpiresAt) {
			delete(s.data.RefreshTokens, h)
		}
	}
	for id, sess := range s.data.Sessions {
		if now.After(sess.ExpiresAt) {
			delete(s.data.Sessions, id)
		}
	}
	return s.flush()
}

// --- вспомогательное ---

// hashToken хеширует секрет для хранения.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// randomToken выдаёт криптостойкую случайную строку.
func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
