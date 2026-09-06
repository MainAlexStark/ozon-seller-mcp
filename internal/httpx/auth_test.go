package httpx

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewAuthRequiresAtLeastOneToken(t *testing.T) {
	if _, err := NewAuth("", ""); err == nil {
		t.Fatal("сервер не должен подниматься в сеть без токенов")
	}
}

func TestScopeSeparatesReadAndWrite(t *testing.T) {
	a, err := NewAuth(strings.Repeat("r", 64), strings.Repeat("w", 64))
	if err != nil {
		t.Fatal(err)
	}

	if s, ok := a.Scope(strings.Repeat("r", 64)); !ok || s != ScopeRead {
		t.Errorf("читающий токен: got %q ok=%v", s, ok)
	}
	if s, ok := a.Scope(strings.Repeat("w", 64)); !ok || s != ScopeWrite {
		t.Errorf("пишущий токен: got %q ok=%v", s, ok)
	}
	if _, ok := a.Scope("что-то не то"); ok {
		t.Error("чужой токен не должен приниматься")
	}
	if _, ok := a.Scope(""); ok {
		t.Error("пустой токен не должен приниматься")
	}
}

func TestReadOnlyDeploymentHasNoWriteToken(t *testing.T) {
	// Разворот только на чтение: пишущего токена нет вообще,
	// и предъявить его невозможно.
	a, err := NewAuth(strings.Repeat("r", 64), "")
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := a.Scope(strings.Repeat("r", 64)); !ok || s != ScopeRead {
		t.Errorf("читающий токен должен работать: %q %v", s, ok)
	}
	if _, ok := a.Scope(strings.Repeat("w", 64)); ok {
		t.Error("невыданный пишущий токен не должен приниматься")
	}
}

func TestGenerateTokenIsLongAndUnique(t *testing.T) {
	a, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := GenerateToken()

	if len(a) != 64 {
		t.Errorf("длина токена %d, ожидалось 64 шестнадцатеричных символа", len(a))
	}
	if a == b {
		t.Error("два вызова дали одинаковый токен")
	}
}

func TestTokenFromHeader(t *testing.T) {
	r := httptest.NewRequest("POST", "/mcp", nil)
	r.Header.Set("Authorization", "Bearer abc123")

	token, base := TokenFromRequest(r)
	if token != "abc123" {
		t.Errorf("токен из заголовка: %q", token)
	}
	if base != "/mcp" {
		t.Errorf("базовый путь: %q", base)
	}
}

func TestTokenFromPath(t *testing.T) {
	secret := strings.Repeat("a", 64)
	r := httptest.NewRequest("POST", "/mcp/"+secret, nil)

	token, base := TokenFromRequest(r)
	if token != secret {
		t.Errorf("токен из пути: %q", token)
	}
	if base != "/mcp" {
		t.Errorf("базовый путь должен быть без токена, получен %q", base)
	}
}

func TestShortLastSegmentIsNotMistakenForToken(t *testing.T) {
	// /mcp — это адрес, а не секрет.
	r := httptest.NewRequest("POST", "/mcp", nil)
	if token, _ := TokenFromRequest(r); token != "" {
		t.Errorf("короткий сегмент принят за токен: %q", token)
	}
}

func TestHeaderWinsOverPath(t *testing.T) {
	r := httptest.NewRequest("POST", "/mcp/"+strings.Repeat("a", 64), nil)
	r.Header.Set("Authorization", "Bearer из-заголовка")

	if token, _ := TokenFromRequest(r); token != "из-заголовка" {
		t.Errorf("заголовок должен иметь приоритет, получено %q", token)
	}
}
