// Package shops — магазины пользователей: проверка ключа при
// подключении, шифрование и клиенты Ozon на каждый магазин.
package shops

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/MainAlexStark/ozon-seller-mcp/internal/pgstore"
	"github.com/MainAlexStark/ozon-seller-mcp/internal/secure"
	"github.com/MainAlexStark/ozon-seller-mcp/ozon"
)

// Store — то, что сервису нужно от базы.
type Store interface {
	AddShop(ctx context.Context, s pgstore.Shop) (pgstore.Shop, error)
	Shop(ctx context.Context, userID, shopID int64) (pgstore.Shop, error)
	UpdateShopKey(ctx context.Context, userID, shopID int64, keyEnc []byte, hint string) error
	SetShopCheck(ctx context.Context, shopID int64, checkErr string) error
}

// Service — магазины и их клиенты.
type Service struct {
	store  Store
	cipher *secure.Cipher
	opts   []ozon.Option

	mu      sync.Mutex
	clients map[int64]cached
}

type cached struct {
	fingerprint [32]byte
	client      *ozon.Client
}

// New создаёт сервис. opts применяются ко всем клиентам Ozon (прокси,
// подмена адреса в тестах).
func New(store Store, cipher *secure.Cipher, opts ...ozon.Option) *Service {
	return &Service{store: store, cipher: cipher, opts: opts, clients: map[int64]cached{}}
}

// InputError — ошибка во введённых данных, её показывают человеку как есть.
type InputError struct{ Msg string }

func (e *InputError) Error() string { return e.Msg }

func inputErr(format string, a ...any) error { return &InputError{Msg: fmt.Sprintf(format, a...)} }

// Connect проверяет ключ на живом Ozon и, если он принят, сохраняет
// магазин. Нерабочий ключ не сохраняется: пользователь узнаёт о
// проблеме сейчас, а не в чате с Claude через неделю.
func (s *Service) Connect(ctx context.Context, userID int64, name, clientID, apiKey string) (pgstore.Shop, error) {
	name, clientID, apiKey = strings.TrimSpace(name), strings.TrimSpace(clientID), strings.TrimSpace(apiKey)
	if err := validate(clientID, apiKey); err != nil {
		return pgstore.Shop{}, err
	}
	if name == "" {
		name = "Магазин " + clientID
	}
	if utf8.RuneCountInString(name) > 80 {
		return pgstore.Shop{}, inputErr("Название длиннее 80 символов.")
	}

	if err := s.Verify(ctx, clientID, apiKey); err != nil {
		return pgstore.Shop{}, err
	}

	enc, err := s.cipher.Encrypt([]byte(apiKey), aad(userID, clientID))
	if err != nil {
		return pgstore.Shop{}, err
	}
	now := time.Now()
	shop, err := s.store.AddShop(ctx, pgstore.Shop{
		UserID:       userID,
		Name:         name,
		OzonClientID: clientID,
		APIKeyEnc:    enc,
		APIKeyHint:   hint(apiKey),
		CheckedAt:    &now,
	})
	if errors.Is(err, pgstore.ErrShopExists) {
		return pgstore.Shop{}, inputErr("Магазин с Client-Id %s уже подключён. Чтобы сменить ключ, воспользуйтесь «Обновить ключ».", clientID)
	}
	return shop, err
}

// UpdateKey меняет ключ магазина, предварительно проверив новый.
func (s *Service) UpdateKey(ctx context.Context, userID, shopID int64, apiKey string) error {
	apiKey = strings.TrimSpace(apiKey)
	shop, err := s.store.Shop(ctx, userID, shopID)
	if err != nil {
		return err
	}
	if err := validate(shop.OzonClientID, apiKey); err != nil {
		return err
	}
	if err := s.Verify(ctx, shop.OzonClientID, apiKey); err != nil {
		return err
	}
	enc, err := s.cipher.Encrypt([]byte(apiKey), aad(userID, shop.OzonClientID))
	if err != nil {
		return err
	}
	return s.store.UpdateShopKey(ctx, userID, shopID, enc, hint(apiKey))
}

// Check перепроверяет сохранённый ключ и записывает итог.
func (s *Service) Check(ctx context.Context, userID, shopID int64) error {
	shop, err := s.store.Shop(ctx, userID, shopID)
	if err != nil {
		return err
	}
	key, err := s.decrypt(shop)
	if err != nil {
		return err
	}
	checkErr := s.Verify(ctx, shop.OzonClientID, key)
	msg := ""
	if checkErr != nil {
		msg = checkErr.Error()
	}
	if err := s.store.SetShopCheck(ctx, shop.ID, msg); err != nil {
		return err
	}
	return checkErr
}

// Verify проверяет пару Client-Id + ключ на живом Ozon.
//
// filter обязателен даже когда фильтровать нечего: без него Ozon
// отвечает 400 ещё до проверки ключа, и понять, принят ли ключ,
// становится невозможно.
func (s *Service) Verify(ctx context.Context, clientID, apiKey string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	opts := append(append([]ozon.Option(nil), s.opts...), ozon.WithMaxRetries(1))
	c := ozon.New(clientID, apiKey, opts...)
	if err := c.ProxyError(); err != nil {
		return err
	}
	_, err := c.Call(ctx, ozon.PathProductList, map[string]any{
		"filter": map[string]any{"visibility": "ALL"},
		"limit":  1,
	})
	if err == nil {
		return nil
	}

	var apiErr *ozon.APIError
	if errors.As(err, &apiErr) && (apiErr.StatusCode == 401 || apiErr.StatusCode == 403) {
		return inputErr("Ozon не принял ключ: проверьте Client-Id и API-ключ. " +
			"Ключ выпускается в кабинете продавца: Настройки → API-ключи. Роль ключа должна позволять " +
			"работу с товарами (например, «Администратор»).")
	}
	var netErr *ozon.NetworkError
	if errors.As(err, &netErr) {
		return fmt.Errorf("не удалось связаться с Ozon, попробуйте позже: %w", err)
	}
	return fmt.Errorf("Ozon ответил ошибкой: %w", err)
}

// Client возвращает клиента Ozon для магазина пользователя.
//
// Клиент кешируется: у каждого свой лимитер, и пересоздавать его на
// каждый запрос значило бы обнулять учёт лимитов Ozon. Строка магазина
// читается из базы всегда — так удалённый магазин перестаёт работать
// сразу, а сменённый ключ подхватывается без перезапуска.
func (s *Service) Client(ctx context.Context, userID, shopID int64) (*ozon.Client, error) {
	shop, err := s.store.Shop(ctx, userID, shopID)
	if err != nil {
		return nil, err
	}
	fp := sha256.Sum256(append([]byte(shop.OzonClientID+"\x00"), shop.APIKeyEnc...))

	s.mu.Lock()
	if c, ok := s.clients[shopID]; ok && c.fingerprint == fp {
		s.mu.Unlock()
		return c.client, nil
	}
	s.mu.Unlock()

	key, err := s.decrypt(shop)
	if err != nil {
		return nil, err
	}
	client := ozon.New(shop.OzonClientID, key, s.opts...)

	s.mu.Lock()
	s.clients[shopID] = cached{fingerprint: fp, client: client}
	s.mu.Unlock()
	return client, nil
}

func (s *Service) decrypt(shop pgstore.Shop) (string, error) {
	key, err := s.cipher.Decrypt(shop.APIKeyEnc, aad(shop.UserID, shop.OzonClientID))
	if err != nil {
		return "", err
	}
	return string(key), nil
}

// aad привязывает шифротекст к владельцу и Client-Id.
func aad(userID int64, clientID string) []byte {
	return []byte(fmt.Sprintf("ozon-shop:%d:%s", userID, clientID))
}

func validate(clientID, apiKey string) error {
	if clientID == "" || apiKey == "" {
		return inputErr("Нужны и Client-Id, и API-ключ.")
	}
	for _, r := range clientID {
		if r < '0' || r > '9' {
			return inputErr("Client-Id — это число из кабинета Ozon (Настройки → API-ключи).")
		}
	}
	if len(clientID) > 20 {
		return inputErr("Client-Id слишком длинный.")
	}
	if len(apiKey) < 16 || len(apiKey) > 200 || strings.ContainsAny(apiKey, " \t\r\n") {
		return inputErr("Это не похоже на API-ключ Ozon: скопируйте его целиком, без пробелов.")
	}
	return nil
}

// hint — последние 4 символа ключа для показа в списке.
func hint(apiKey string) string {
	if len(apiKey) <= 4 {
		return "****"
	}
	return "…" + apiKey[len(apiKey)-4:]
}
