// Package httpx — сетевой транспорт MCP-сервера и его авторизация.
//
// Пока сервер работал по stdio, защита была неявной, но надёжной: его
// запускал сам пользователь на своей машине. В сети эта защита исчезает
// целиком, а в сервисе к тому же у каждого запроса свой хозяин и свой
// магазин. Поэтому здесь на входе отвечается сразу на три вопроса:
// кто пришёл, с какими правами и к какому магазину.
package httpx

import (
	"context"
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

// Principal — чей это запрос.
type Principal struct {
	UserID int64
	ShopID int64
	Scope  Scope
	// Via — чем подтверждён: "oauth" или "api_token". Для журнала.
	Via string
}

// APITokenFunc проверяет персональный токен (автоматизация).
// ok=false — токен неизвестен.
type APITokenFunc func(ctx context.Context, token string) (p Principal, ok bool)

// APITokenPrefix — начало персональных токенов. По нему токен
// отличается от OAuth без похода в базу, а человек узнаёт его
// в конфиге скрипта.
const APITokenPrefix = "osm_"

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
