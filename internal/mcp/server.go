// Package mcp — минимальный каркас MCP-сервера поверх stdio.
//
// Здесь реализован транспорт и диспетчеризация JSON-RPC: initialize,
// tools/list, tools/call. Бизнес-логики в MCP-серверах нет и не должно
// быть — они только описывают схему тула и вызывают функцию из pkg/*.
//
// Почему свой мини-каркас, а не сразу официальный SDK: на этапе Ф0
// важно, чтобы репозиторий собирался без единой внешней зависимости.
// Когда дойдёт до продакшена, здесь останется тот же интерфейс Tool,
// а внутренности заменит github.com/modelcontextprotocol/go-sdk.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// ProtocolVersion — версия MCP, о которой договариваемся при initialize.
const ProtocolVersion = "2025-06-18"

// Tool — один инструмент, доступный модели.
type Tool struct {
	Name        string
	Description string
	// InputSchema — JSON Schema аргументов. Модель видит именно её,
	// поэтому описание полей здесь важнее, чем кажется.
	InputSchema map[string]any
	// Handler получает сырые аргументы и возвращает текст результата.
	Handler func(ctx context.Context, args json.RawMessage) (string, error)
}

// Server — набор инструментов, обслуживаемый по stdio.
type Server struct {
	name    string
	version string

	mu    sync.RWMutex
	tools map[string]Tool
	order []string
}

// NewServer создаёт сервер с именем и версией.
func NewServer(name, version string) *Server {
	return &Server{
		name:    name,
		version: version,
		tools:   make(map[string]Tool),
	}
}

// Register добавляет инструмент. Повторная регистрация имени — ошибка
// программиста, поэтому паникуем на старте, а не молча перетираем.
func (s *Server) Register(t Tool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, dup := s.tools[t.Name]; dup {
		panic("mcp: инструмент уже зарегистрирован: " + t.Name)
	}
	s.tools[t.Name] = t
	s.order = append(s.order, t.Name)
}

// ToolNames перечисляет инструменты в порядке регистрации.
func (s *Server) ToolNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.order...)
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string  `json:"jsonrpc"`
	ID      any     `json:"id,omitempty"`
	Result  any     `json:"result,omitempty"`
	Error   *rpcErr `json:"error,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve обслуживает поток JSON-RPC: по одному сообщению на строку.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	enc := json.NewEncoder(out)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}

		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			_ = enc.Encode(response{JSONRPC: "2.0", Error: &rpcErr{Code: -32700, Message: "parse error"}})
			continue
		}

		resp := s.handle(ctx, req)

		// Уведомления (без id) ответа не требуют.
		if len(req.ID) == 0 {
			continue
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (s *Server) handle(ctx context.Context, req request) response {
	out := response{JSONRPC: "2.0"}
	if len(req.ID) > 0 {
		var id any
		_ = json.Unmarshal(req.ID, &id)
		out.ID = id
	}

	switch req.Method {
	case "initialize":
		out.Result = map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": s.name, "version": s.version},
		}

	case "tools/list":
		out.Result = map[string]any{"tools": s.describe()}

	case "tools/call":
		text, err := s.call(ctx, req.Params)
		if err != nil {
			// Ошибка инструмента возвращается как результат с isError,
			// а не как ошибка протокола: модель должна её прочитать
			// и попробовать исправиться, а не считать сервер сломанным.
			out.Result = map[string]any{
				"content": []map[string]any{{"type": "text", "text": err.Error()}},
				"isError": true,
			}
			break
		}
		out.Result = map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
		}

	default:
		out.Error = &rpcErr{Code: -32601, Message: "method not found: " + req.Method}
	}

	return out
}

func (s *Server) describe() []map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()

	list := make([]map[string]any, 0, len(s.order))
	for _, name := range s.order {
		t := s.tools[name]
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		list = append(list, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": schema,
		})
	}
	return list
}

func (s *Server) call(ctx context.Context, params json.RawMessage) (string, error) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", fmt.Errorf("mcp: неразбираемые параметры вызова: %w", err)
	}

	s.mu.RLock()
	t, ok := s.tools[p.Name]
	s.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("mcp: неизвестный инструмент %q", p.Name)
	}

	return t.Handler(ctx, p.Arguments)
}
