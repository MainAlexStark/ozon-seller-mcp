// Package httpx — сетевой транспорт MCP-сервера и его авторизация.
//
// Пока сервер работал по stdio, защита была неявной, но надёжной: его
// запускал сам пользователь на своей машине, и добраться до сервера мог
// только процесс, который его породил. Как только тот же сервер выставлен
// в интернет, эта защита исчезает целиком: любой, кто знает адрес, может
// менять цены в магазине.
//
// Поэтому здесь авторизация — не опция, а условие запуска: сервер не
// стартует без настроенного способа проверки.
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
	// ScopeRead — только чтение.
	ScopeRead Scope = "read"
	// ScopeWrite — чтение и запись.
	ScopeWrite Scope = "write"
)

// ErrNoAuth возвращается, когда сервер пытаются поднять без защиты.
var ErrNoAuth = errors.New(
	"httpx: сетевой режим требует авторизации. Настройте OAuth (OZON_PUBLIC_URL + " +
		"OZON_OWNER_PASSWORD_HASH, см. docs/OAUTH.md) либо, для автоматизации, " +
		"задайте статический токен OZON_TOKEN_READ / OZON_TOKEN_WRITE")

// StaticAuth — статические токены в заголовке Authorization.
//
// Оставлены для автоматизации: скрипт или соседний сервис не может
// пройти экран согласия, а заводить ради него человека в цикле
// бессмысленно. Для людей и клиентов Claude способ по умолчанию — OAuth.
//
// Чего здесь больше НЕТ — токена в адресе. Он позволял подключить
// телефон, где интерфейс принимает только URL, но спецификация MCP
// прямо запрещает передавать токен в строке запроса: адреса оседают
// в журналах прокси и истории браузера. OAuth закрывает ту же задачу
// без этой платы.
type StaticAuth struct {
	read  []byte
	write []byte
}

// NewStaticAuth создаёт проверку статических токенов.
// Пустая строка означает, что токен с такими правами не выдан.
func NewStaticAuth(readToken, writeToken string) *StaticAuth {
	a := &StaticAuth{}
	if readToken != "" {
		a.read = []byte(readToken)
	}
	if writeToken != "" {
		a.write = []byte(writeToken)
	}
	return a
}

// Enabled — задан ли хоть один статический токен.
func (a *StaticAuth) Enabled() bool {
	return a != nil && (len(a.read) > 0 || len(a.write) > 0)
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
// Сравнение за постоянное время: обычное == раскрывает длину совпавшего
// префикса и позволяет подобрать токен посимвольно. Оба сравнения
// выполняются всегда, чтобы по времени ответа нельзя было понять,
// какой именно токен предъявлен.
func (a *StaticAuth) Scope(token string) (Scope, bool) {
	if a == nil || token == "" {
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

// bearerToken достаёт токен из заголовка Authorization.
//
// Только заголовок: спецификация MCP требует именно его и запрещает
// строку запроса.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	after, ok := strings.CutPrefix(h, "Bearer ")
	if !ok {
		// Регистр схемы по RFC 7235 не важен.
		if after, ok = strings.CutPrefix(h, "bearer "); !ok {
			return ""
		}
	}
	return strings.TrimSpace(after)
}
