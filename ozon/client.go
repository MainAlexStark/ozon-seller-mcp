// Package ozon — Go-клиент Ozon Seller API.
//
// Устройство пакета отражает то, как API используется на практике:
//
//   - ЧТЕНИЕ идёт через Call и возвращает сырой JSON. Ozon регулярно
//     добавляет поля в ответы; жёсткие структуры для чтения означали бы
//     терять новые данные и чинить клиент после каждого обновления.
//
//   - ЗАПИСЬ типизирована строго. Здесь цена ошибки — испорченные
//     карточки в живом магазине, поэтому структура запроса описана
//     явно и валидируется до отправки.
//
// Авторизация — заголовки Client-Id и Api-Key, хост api-seller.ozon.ru.
// Ключ выпускается в кабинете продавца.
package ozon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DefaultBaseURL — боевой хост Seller API.
const DefaultBaseURL = "https://api-seller.ozon.ru"

// Client — клиент Seller API.
type Client struct {
	baseURL  string
	clientID string
	apiKey   string
	http     *http.Client
	limiter  *Limiter

	// maxRetries — сколько раз повторить запрос при 429 и 5xx.
	maxRetries int
}

// Option настраивает клиент.
type Option func(*Client)

// WithBaseURL подменяет хост (тесты, песочница).
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = u } }

// WithHTTPClient подставляет свой http.Client.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithLimiter задаёт собственный лимитер.
func WithLimiter(l *Limiter) Option { return func(c *Client) { c.limiter = l } }

// WithMaxRetries меняет число повторов при 429 и 5xx.
func WithMaxRetries(n int) Option { return func(c *Client) { c.maxRetries = n } }

// New создаёт клиент.
func New(clientID, apiKey string, opts ...Option) *Client {
	c := &Client{
		baseURL:    DefaultBaseURL,
		clientID:   clientID,
		apiKey:     apiKey,
		http:       &http.Client{Timeout: 60 * time.Second},
		limiter:    NewLimiter(),
		maxRetries: 3,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// ClientID возвращает идентификатор клиента. Нужен инструменту
// самодиагностики, чтобы показать, под каким кабинетом работаем.
func (c *Client) ClientID() string { return c.clientID }

// Call выполняет POST к методу Seller API и возвращает сырой JSON.
//
// Это основная точка входа для чтения: ответ отдаётся как есть, без
// разбора в структуры, поэтому новые поля Ozon доезжают до вызывающего
// автоматически.
func (c *Client) Call(ctx context.Context, path string, payload any) (json.RawMessage, error) {
	if payload == nil {
		payload = map[string]any{}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("ozon: сериализация запроса %s: %w", path, err)
	}

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if c.limiter != nil {
			if err := c.limiter.Wait(ctx, path); err != nil {
				return nil, err
			}
		}

		raw, err := c.do(ctx, path, body)
		if err == nil {
			return raw, nil
		}
		lastErr = err

		var apiErr *APIError
		if !asAPIError(err, &apiErr) || !apiErr.Retryable() || attempt == c.maxRetries {
			return nil, err
		}

		// Экспоненциальная пауза: 1, 2, 4 секунды.
		wait := time.Duration(1<<attempt) * time.Second
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	return nil, lastErr
}

// CallInto выполняет запрос и разбирает ответ в out.
// Применяется там, где структура ответа нужна коду (например, чтобы
// сравнить текущую цену с новой перед записью).
func (c *Client) CallInto(ctx context.Context, path string, payload, out any) error {
	raw, err := c.Call(ctx, path, payload)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("ozon: разбор ответа %s: %w", path, err)
	}
	return nil
}

func (c *Client) do(ctx context.Context, path string, body []byte) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ozon: сборка запроса %s: %w", path, err)
	}
	req.Header.Set("Client-Id", c.clientID)
	req.Header.Set("Api-Key", c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ozon: запрос %s: %w", path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ozon: чтение ответа %s: %w", path, err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, newAPIError(resp.StatusCode, path, raw)
	}
	return raw, nil
}
