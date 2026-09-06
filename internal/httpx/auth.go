// Package httpx — сетевой транспорт MCP-сервера и его авторизация.
//
// Пока сервер работал по stdio, защита была неявной, но надёжной: его
// запускал сам пользователь на своей машине, и добраться до сервера мог
// только процесс, который его породил. Как только тот же сервер выставлен
// в интернет, эта защита исчезает целиком: любой, кто знает адрес, может
// менять цены в магазине.
//
// Поэтому здесь авторизация — не опция, а условие запуска: сервер не
// стартует без токенов.
package httpx

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
)

// Scope — что разрешает предъявленный токен.
type Scope string

const (
	// ScopeRead — только чтение. Токен для телефона и любых мест,
	// где вы не собираетесь ничего менять.
	ScopeRead Scope = "read"
	// ScopeWrite — чтение и запись. Должен жить только там, где вы
	// действительно правите магазин.
	ScopeWrite Scope = "write"
)

// ErrNoTokens возвращается, когда сервер пытаются поднять без токенов.
var ErrNoTokens = errors.New(
	"httpx: сетевой режим требует хотя бы один токен. " +
		"Сгенерируйте: ozon-seller-mcp --gen-token, затем задайте OZON_TOKEN_READ и/или OZON_TOKEN_WRITE")

// Auth хранит токены и определяет права входящего запроса.
type Auth struct {
	read  []byte
	write []byte
}

// NewAuth создаёт проверку токенов. Пустая строка означает, что токен
// с такими правами не выдан и подключиться с ним нельзя.
func NewAuth(readToken, writeToken string) (*Auth, error) {
	if readToken == "" && writeToken == "" {
		return nil, ErrNoTokens
	}
	a := &Auth{}
	if readToken != "" {
		a.read = []byte(readToken)
	}
	if writeToken != "" {
		a.write = []byte(writeToken)
	}
	return a, nil
}

// GenerateToken выдаёт случайный токен на 32 байта.
func GenerateToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// Scope сопоставляет предъявленный токен с выданными.
//
// Сравнение идёт за постоянное время: обычное == раскрывает длину
// совпавшего префикса и позволяет подобрать токен посимвольно.
//
// Пишущий токен проверяется первым, но оба сравнения выполняются
// всегда — чтобы по времени ответа нельзя было понять, какой именно
// токен предъявлен.
func (a *Auth) Scope(token string) (Scope, bool) {
	if token == "" {
		return "", false
	}
	got := []byte(token)

	writeOK := len(a.write) > 0 && subtle.ConstantTimeCompare(got, a.write) == 1
	readOK := len(a.read) > 0 && subtle.ConstantTimeCompare(got, a.read) == 1

	switch {
	case writeOK:
		return ScopeWrite, true
	case readOK:
		return ScopeRead, true
	default:
		return "", false
	}
}

// TokenFromRequest достаёт токен из запроса.
//
// Поддерживаются два способа, и это не избыточность:
//
//   - Заголовок Authorization: Bearer — правильный вариант, доступен
//     там, где клиент разрешает задать свои заголовки (Claude Code).
//
//   - Последний сегмент пути: /mcp/<токен> — единственный вариант там,
//     где интерфейс принимает только адрес и ничего больше. Мобильное
//     приложение относится именно к таким.
//
// Секрет в адресе — сознательный компромисс, а не недосмотр: он может
// осесть в логах прокси и в истории браузера. Ради этого он и сделан
// отзываемым одной строкой в конфигурации.
func TokenFromRequest(r *http.Request) (token, basePath string) {
	if h := r.Header.Get("Authorization"); h != "" {
		if after, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(after), r.URL.Path
		}
	}

	// /mcp/<токен> -> токен, базовый путь /mcp
	trimmed := strings.Trim(r.URL.Path, "/")
	if trimmed == "" {
		return "", r.URL.Path
	}
	parts := strings.Split(trimmed, "/")
	last := parts[len(parts)-1]

	// Токен всегда длинный: короткий последний сегмент — это часть
	// адреса (например /mcp), а не секрет.
	if len(parts) >= 2 && len(last) >= 32 {
		return last, "/" + strings.Join(parts[:len(parts)-1], "/")
	}
	return "", r.URL.Path
}
