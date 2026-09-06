package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func newTestServer() *Server {
	s := NewServer("test", "0.0.1")
	s.Register(Tool{
		Name:        "echo",
		Description: "возвращает переданный текст",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}},
			"required":   []string{"text"},
		},
		Handler: func(_ context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}
			if a.Text == "" {
				return "", errors.New("text обязателен")
			}
			return a.Text, nil
		},
	})
	return s
}

func run(t *testing.T, s *Server, lines ...string) []map[string]any {
	t.Helper()

	var out bytes.Buffer
	in := strings.NewReader(strings.Join(lines, "\n") + "\n")
	if err := s.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	var got []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("ответ не JSON: %q", l)
		}
		got = append(got, m)
	}
	return got
}

func TestInitializeAndToolsList(t *testing.T) {
	got := run(t, newTestServer(),
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	)

	if len(got) != 2 {
		t.Fatalf("ожидалось 2 ответа, получено %d", len(got))
	}

	info := got[0]["result"].(map[string]any)["serverInfo"].(map[string]any)
	if info["name"] != "test" {
		t.Errorf("serverInfo.name = %v", info["name"])
	}

	tools := got[1]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "echo" {
		t.Errorf("tools/list вернул %v", tools)
	}
}

func TestToolsCall(t *testing.T) {
	got := run(t, newTestServer(),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"привет"}}}`,
	)

	content := got[0]["result"].(map[string]any)["content"].([]any)
	if content[0].(map[string]any)["text"] != "привет" {
		t.Errorf("неожиданный ответ: %v", content)
	}
}

func TestToolErrorIsResultNotProtocolError(t *testing.T) {
	// Ошибка инструмента должна доходить до модели как текст,
	// иначе она не сможет исправить аргументы.
	got := run(t, newTestServer(),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{}}}`,
	)

	if _, isProtocolErr := got[0]["error"]; isProtocolErr {
		t.Fatal("ошибка инструмента не должна быть ошибкой протокола")
	}
	res := got[0]["result"].(map[string]any)
	if res["isError"] != true {
		t.Errorf("ожидался isError=true, получено %v", res)
	}
}

func TestUnknownMethod(t *testing.T) {
	got := run(t, newTestServer(), `{"jsonrpc":"2.0","id":1,"method":"nope"}`)
	if got[0]["error"] == nil {
		t.Fatal("неизвестный метод должен давать ошибку протокола")
	}
}

func TestNotificationGetsNoResponse(t *testing.T) {
	got := run(t, newTestServer(), `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if len(got) != 0 {
		t.Fatalf("на уведомление не должно быть ответа, получено %v", got)
	}
}

func TestDuplicateRegistrationPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("повторная регистрация имени должна паниковать на старте")
		}
	}()
	s := newTestServer()
	s.Register(Tool{Name: "echo"})
}
