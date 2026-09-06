package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/MainAlexStark/ozon-seller-mcp/internal/mcp"
	"github.com/MainAlexStark/ozon-seller-mcp/ozon"
)

// Registry собирает инструменты и раздаёт им общий контекст:
// клиента, настройки безопасности и режим.
type Registry struct {
	client *ozon.Client
	safety Safety
	server *mcp.Server
}

// NewRegistry создаёт реестр.
func NewRegistry(client *ozon.Client, safety Safety, server *mcp.Server) *Registry {
	return &Registry{client: client, safety: safety, server: server}
}

// Spec — описание простого инструмента-обёртки над одним методом API.
//
// Большая часть read-инструментов исчерпывается этой структурой:
// принять аргументы, отправить их как тело запроса, вернуть JSON.
// Именно поэтому добавление нового метода стоит несколько строк, а не
// новой функции с копипастой обработки ошибок.
type Spec struct {
	Name string
	Path string
	Desc string

	// Write помечает инструмент как изменяющий данные. Такие
	// проходят через Safety.CheckWrite.
	Write bool

	// Schema — JSON Schema аргументов. Модель видит именно её.
	Schema map[string]any

	// Build превращает аргументы инструмента в тело запроса к Ozon.
	// Если nil, аргументы уходят как есть.
	Build func(args map[string]any) (any, error)
}

// Add регистрирует инструмент, описанный спецификацией.
func (r *Registry) Add(s Spec) {
	r.server.Register(mcp.Tool{
		Name:        s.Name,
		Description: s.Desc,
		InputSchema: s.Schema,
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			args := map[string]any{}
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &args); err != nil {
					return "", fmt.Errorf("не разобрать аргументы: %w", err)
				}
			}

			if s.Write {
				if err := r.safety.CheckWrite(s.Name); err != nil {
					return "", err
				}
			}

			payload := any(args)
			if s.Build != nil {
				built, err := s.Build(args)
				if err != nil {
					return "", err
				}
				payload = built
			}

			return r.call(ctx, s.Path, payload)
		},
	})
}

// AddCustom регистрирует инструмент с собственным обработчиком —
// для тех случаев, где одной обёртки над путём мало (цены со
// страховкой, ожидание импорта, самодиагностика).
func (r *Registry) AddCustom(t mcp.Tool) {
	r.server.Register(t)
}

// call выполняет запрос и приводит ответ к виду, удобному для чтения
// моделью: отформатированный JSON, обрезанный по размеру, а ошибка —
// с подсказкой, что делать дальше.
func (r *Registry) call(ctx context.Context, path string, payload any) (string, error) {
	raw, err := r.client.Call(ctx, path, payload)
	if err != nil {
		return "", decorate(err)
	}
	return r.format(raw), nil
}

// format красиво печатает JSON и обрезает по лимиту.
func (r *Registry) format(raw json.RawMessage) string {
	var pretty json.RawMessage
	if out, err := json.MarshalIndent(json.RawMessage(raw), "", "  "); err == nil {
		pretty = out
	} else {
		pretty = raw
	}
	return r.safety.TrimResponse(string(pretty))
}

// decorate добавляет к ошибке Ozon человеческую подсказку.
func decorate(err error) error {
	var apiErr *ozon.APIError
	if !asAPI(err, &apiErr) {
		return err
	}
	if hint := apiErr.Hint(); hint != "" {
		return fmt.Errorf("%w\n\n%s", err, hint)
	}
	return err
}
