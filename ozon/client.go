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
	"net/url"
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

	// proxy — адрес прокси без пароля, для диагностики.
	proxy    string
	proxyErr error
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

// WithProxy направляет запросы к Ozon через прокси.
//
// Нужен в одной конкретной, но частой ситуации: машина целиком сидит
// под VPN (без него не работает что-то другое), а Ozon из-под этого
// VPN недоступен — запросы к api-seller.ozon.ru просто не доходят.
//
// Прокси здесь задаётся ЯВНО и действует ТОЛЬКО на трафик к Ozon.
// Полагаться на переменные HTTPS_PROXY нельзя: они глобальные, их
// выставляют для других задач, и тогда либо ваш трафик уедет не туда,
// либо чужой — сюда. Одна настройка — один эффект.
//
// Поддерживаются http://, https:// и socks5://; можно с логином:
//
//	socks5://user:pass@vps.example.com:1080
func WithProxy(rawURL string) Option {
	return func(c *Client) {
		if rawURL == "" {
			return
		}
		u, err := url.Parse(rawURL)
		if err != nil {
			c.proxyErr = fmt.Errorf("ozon: не разобрать адрес прокси %q: %w", rawURL, err)
			return
		}

		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = http.ProxyURL(u)
		c.http.Transport = transport
		c.proxy = u.Redacted() // без пароля: строка попадает в диагностику
	}
}

// Proxy возвращает адрес прокси без пароля, либо пустую строку.
func (c *Client) Proxy() string { return c.proxy }

// ProxyError возвращает ошибку разбора адреса прокси, если она была.
// Проверяется на старте: молча ходить напрямую, когда человек просил
// через прокси, — худший из возможных исходов.
func (c *Client) ProxyError() error { return c.proxyErr }

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

	return c.call(ctx, http.MethodPost, path, body)
}

// Get выполняет GET к методу Seller API.
//
// Почти весь Seller API — это POST с телом, даже там, где запрос
// ничего не меняет и ничего не фильтрует. Но несколько справочных
// методов сделаны иначе и на POST отвечают 404 — например, список
// доступных акций. Отдельный метод здесь нужен ровно поэтому: чтобы
// «метод не найден» не приходилось расследовать заново каждый раз,
// когда на самом деле не совпал глагол.
func (c *Client) Get(ctx context.Context, path string) (json.RawMessage, error) {
	return c.call(ctx, http.MethodGet, path, nil)
}

// call выполняет запрос с повторами и лимитером.
func (c *Client) call(ctx context.Context, method, path string, body []byte) (json.RawMessage, error) {
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if c.limiter != nil {
			if err := c.limiter.Wait(ctx, path); err != nil {
				return nil, err
			}
		}

		raw, err := c.do(ctx, method, path, body)
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

func (c *Client) do(ctx context.Context, method, path string, body []byte) (json.RawMessage, error) {
	// GET идёт без тела: nil-тело и заголовок Content-Type — вещи
	// разные, и отправлять второе без первого значит объявить формат
	// того, чего нет.
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("ozon: сборка запроса %s: %w", path, err)
	}
	req.Header.Set("Client-Id", c.clientID)
	req.Header.Set("Api-Key", c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// Сбой соединения — не то же самое, что отказ Ozon: причина
		// в маршруте, и подсказка нужна другая. См. neterr.go.
		return nil, &NetworkError{Path: path, Proxy: c.proxy, Err: err}
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

// HTTPClient возвращает используемый HTTP-клиент.
//
// Нужен диагностике: чтобы узнать, с какого адреса нас видит внешний
// мир, запрос должен идти через тот же транспорт (и тот же прокси),
// что и обращения к Ozon. Иначе показанный адрес не имеет отношения
// к тому, что видит Ozon.
func (c *Client) HTTPClient() *http.Client { return c.http }
