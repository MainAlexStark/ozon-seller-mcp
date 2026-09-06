package httpx

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStaticScopeSeparatesReadAndWrite(t *testing.T) {
	a := NewStaticAuth(strings.Repeat("r", 64), strings.Repeat("w", 64))

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

func TestStaticAuthEnabled(t *testing.T) {
	if NewStaticAuth("", "").Enabled() {
		t.Error("без токенов проверка не должна считаться настроенной")
	}
	if !NewStaticAuth(strings.Repeat("r", 64), "").Enabled() {
		t.Error("один токен уже включает проверку")
	}
}

func TestReadOnlyDeploymentHasNoWriteToken(t *testing.T) {
	// Разворот только на чтение: пишущего токена нет вообще,
	// и предъявить его невозможно.
	a := NewStaticAuth(strings.Repeat("r", 64), "")

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

func TestBearerTokenFromHeader(t *testing.T) {
	r := httptest.NewRequest("POST", "/mcp", nil)
	r.Header.Set("Authorization", "Bearer abc123")

	if got := bearerToken(r); got != "abc123" {
		t.Errorf("токен из заголовка: %q", got)
	}
}

func TestBearerTokenIgnoresOtherSchemes(t *testing.T) {
	r := httptest.NewRequest("POST", "/mcp", nil)
	r.Header.Set("Authorization", "Basic dXNlcjpwYXNz")

	if got := bearerToken(r); got != "" {
		t.Errorf("схема Basic не должна приниматься за Bearer: %q", got)
	}
}

func TestTokenInURLNoLongerAccepted(t *testing.T) {
	// Спецификация MCP запрещает токен в строке запроса: адреса
	// оседают в журналах прокси и истории браузера. Проверяем, что
	// прежний способ действительно убран, а не остался «на всякий».
	secret := strings.Repeat("a", 64)
	r := httptest.NewRequest("POST", "/mcp/"+secret, nil)

	if got := bearerToken(r); got != "" {
		t.Errorf("токен из адреса не должен извлекаться, получено %q", got)
	}
}
