package tools

import (
	"bytes"
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

	// Get помечает метод, отвечающий на GET вместо POST. Такой
	// инструмент аргументов не передаёт: у методов Seller API,
	// сделанных через GET, их и нет.
	Get bool

	// Write помечает инструмент как изменяющий данные. Такие
	// проходят через Safety.CheckWrite.
	Write bool

	// Batch сообщает, сколько позиций затрагивает вызов.
	//
	// Задаётся у записи, аргумент которой — список. Ограничение
	// размера пачки придумано не ради экономии запросов: чем меньше
	// позиций в одном вызове, тем меньше товаров затронет ошибка,
	// которую заметят не сразу.
	Batch func(args map[string]any) int

	// Schema — JSON Schema аргументов. Модель видит именно её.
	Schema map[string]any

	// Build превращает аргументы инструмента в тело запроса к Ozon.
	// Если nil, аргументы уходят как есть.
	Build func(args map[string]any) (any, error)

	// Hint объясняет отказ Ozon там, где общий разбор ошибок
	// промахивается: один и тот же 400 у разных методов означает
	// разное, и совет не из того класса стоит человеку вечера.
	// Пустая строка — «объяснить нечем, работает общий разбор».
	Hint func(args map[string]any, err error) string
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

			safety := r.safetyFor(ctx)
			if s.Write {
				if err := safety.CheckWrite(s.Name); err != nil {
					return "", err
				}
				if s.Batch != nil {
					if err := safety.CheckBatchSize(s.Batch(args)); err != nil {
						return "", err
					}
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

			var (
				resp json.RawMessage
				err  error
			)
			if s.Get {
				resp, err = r.client.Get(ctx, s.Path)
			} else {
				resp, err = r.client.Call(ctx, s.Path, payload)
			}
			if err != nil {
				if s.Hint != nil {
					if hint := s.Hint(args, err); hint != "" {
						return "", fmt.Errorf("%w\n\n%s", err, hint)
					}
				}
				return "", decorate(err)
			}

			return r.format(ctx, resp), nil
		},
	})
}

// AddCustom регистрирует инструмент с собственным обработчиком —
// для тех случаев, где одной обёртки над путём мало (цены со
// страховкой, ожидание импорта, самодиагностика).
func (r *Registry) AddCustom(t mcp.Tool) {
	r.server.Register(t)
}

// prettyLimit — до какого размера ответ печатается с отступами.
//
// Отступы стоят дороже, чем кажется: на живом ответе начислений за день
// они раздули 31 КБ до 78 КБ, то есть больше чем вдвое, и ровно на этом
// ответ перестал доезжать до модели. Пока ответ короткий, читаемость
// важнее — на длинном она не стоит удвоенного счёта.
const prettyLimit = 8_000

// format приводит ответ к виду, удобному для чтения моделью, и
// обрезает по лимиту запроса.
func (r *Registry) format(ctx context.Context, raw json.RawMessage) string {
	return r.safetyFor(ctx).TrimResponse(formatJSON(raw))
}

// formatJSON печатает JSON: короткий — с отступами, длинный — плотно.
func formatJSON(raw json.RawMessage) string {
	body := string(raw)

	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err == nil {
		body = compact.String()
	}

	if len(body) <= prettyLimit {
		if pretty, err := json.MarshalIndent(json.RawMessage(raw), "", "  "); err == nil {
			body = string(pretty)
		}
	}

	return body
}

// decorate добавляет к ошибке человеческую подсказку.
//
// Два класса ошибок разбираются отдельно, потому что советы у них
// противоположные: на отказ Ozon надо чинить запрос или ключ, на сбой
// соединения — маршрут. Совет не из того класса стоит человеку вечера.
func decorate(err error) error {
	var netErr *ozon.NetworkError
	if asNet(err, &netErr) {
		return fmt.Errorf("%w\n\n%s", err, netErr.Hint())
	}

	var apiErr *ozon.APIError
	if !asAPI(err, &apiErr) {
		return err
	}
	if hint := apiErr.Hint(); hint != "" {
		return fmt.Errorf("%w\n\n%s", err, hint)
	}
	return err
}
