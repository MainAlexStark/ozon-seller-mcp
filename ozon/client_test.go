package ozon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCallSendsAuthHeaders(t *testing.T) {
	var gotClientID, gotAPIKey, gotContentType, gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotClientID = r.Header.Get("Client-Id")
		gotAPIKey = r.Header.Get("Api-Key")
		gotContentType = r.Header.Get("Content-Type")
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"result":"ok"}`))
	}))
	defer srv.Close()

	c := New("cid", "key", WithBaseURL(srv.URL))

	raw, err := c.Call(context.Background(), PathProductList, map[string]any{"limit": 1})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if string(raw) != `{"result":"ok"}` {
		t.Errorf("ответ отдан не как есть: %s", raw)
	}
	if gotClientID != "cid" || gotAPIKey != "key" {
		t.Errorf("заголовки авторизации: Client-Id=%q Api-Key=%q", gotClientID, gotAPIKey)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type: %q", gotContentType)
	}
	if gotPath != PathProductList {
		t.Errorf("путь: %q", gotPath)
	}
}

func TestCallRetriesOn429(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"code":"TOO_MANY","message":"rate limit"}`))
			return
		}
		_, _ = w.Write([]byte(`{"result":"ok"}`))
	}))
	defer srv.Close()

	// Ускоряем: пауза между попытками экспоненциальная, но тест
	// должен уложиться в разумное время.
	c := New("cid", "key", WithBaseURL(srv.URL), WithMaxRetries(3))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := c.Call(ctx, PathProductList, nil); err != nil {
		t.Fatalf("после повторов запрос должен пройти: %v", err)
	}
	if calls != 3 {
		t.Errorf("ожидалось 3 попытки, было %d", calls)
	}
}

func TestCallDoesNotRetryOn400(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"BAD","message":"нет обязательной характеристики"}`))
	}))
	defer srv.Close()

	c := New("cid", "key", WithBaseURL(srv.URL))

	_, err := c.Call(context.Background(), PathProductImport, nil)
	if err == nil {
		t.Fatal("400 должен вернуть ошибку")
	}
	if calls != 1 {
		t.Errorf("400 не подлежит повтору, попыток было %d", calls)
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("ожидался *APIError, получено %T", err)
	}
	if apiErr.Message != "нет обязательной характеристики" {
		t.Errorf("сообщение Ozon потерялось: %q", apiErr.Message)
	}
	if !strings.Contains(apiErr.Hint(), "характеристика") {
		t.Errorf("подсказка для 400 должна говорить про характеристики, получено %q", apiErr.Hint())
	}
}

func TestAPIErrorHints(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusUnauthorized, "3 месяца"}, // напоминание про срок жизни ключа
		{http.StatusNotFound, "отключил"},
		{http.StatusTooManyRequests, "лимит"},
	}
	for _, c := range cases {
		e := &APIError{StatusCode: c.status}
		if !strings.Contains(e.Hint(), c.want) {
			t.Errorf("подсказка для %d не содержит %q: %q", c.status, c.want, e.Hint())
		}
	}
}

func TestPriceUpdateValidate(t *testing.T) {
	cases := []struct {
		name    string
		p       PriceUpdate
		wantErr string
	}{
		{"нет идентификатора", PriceUpdate{Price: "100"}, "offer_id"},
		{"нет цены", PriceUpdate{OfferID: "a"}, "не задана цена"},
		{"цена с пробелом", PriceUpdate{OfferID: "a", Price: "1 990"}, "не число"},
		{"отрицательная", PriceUpdate{OfferID: "a", Price: "-5"}, "отрицательная"},
		{"нормальная", PriceUpdate{OfferID: "a", Price: "690.00"}, ""},
	}
	for _, tc := range cases {
		err := tc.p.Validate()
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("%s: неожиданная ошибка %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: ожидалась ошибка про %q, получено %v", tc.name, tc.wantErr, err)
		}
	}
}

func TestStockUpdateValidate(t *testing.T) {
	if err := (StockUpdate{OfferID: "a", Stock: 5}).Validate(); err == nil {
		t.Error("без warehouse_id остаток обновлять нельзя")
	}
	if err := (StockUpdate{OfferID: "a", Stock: -1, WarehouseID: 1}).Validate(); err == nil {
		t.Error("отрицательный остаток должен отклоняться")
	}
	if err := (StockUpdate{OfferID: "a", Stock: 0, WarehouseID: 1}).Validate(); err != nil {
		t.Errorf("нулевой остаток допустим (снятие с продажи): %v", err)
	}
}

func TestProductItemValidateNamesMissingField(t *testing.T) {
	item := ProductItem{OfferID: "pp-1", Name: "Фигурка"}
	err := item.Validate()
	if err == nil || !strings.Contains(err.Error(), "категория") {
		t.Fatalf("должна ругаться на категорию, получено %v", err)
	}
}

func TestGetPricesParsesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[
			{"offer_id":"pp-1","product_id":10,"price":{"price":"690.00","old_price":"890.00"}},
			{"offer_id":"pp-2","product_id":11,"price":{"price":"1200","old_price":"0"}}
		]}`))
	}))
	defer srv.Close()

	c := New("cid", "key", WithBaseURL(srv.URL))

	got, err := c.GetPrices(context.Background(), []string{"pp-1", "pp-2"})
	if err != nil {
		t.Fatalf("GetPrices: %v", err)
	}
	if got["pp-1"].Price != 690 {
		t.Errorf("pp-1 цена = %v, want 690", got["pp-1"].Price)
	}
	if got["pp-2"].Price != 1200 {
		t.Errorf("pp-2 цена = %v, want 1200", got["pp-2"].Price)
	}
}

func TestLimiterGroups(t *testing.T) {
	cases := map[string]string{
		PathProductImport:       "product",
		PathPricesImport:        "prices",
		PathStocksInfo:          "stocks",
		PathPostingFBSList:      "posting",
		PathAnalyticsData:       "analytics",
		PathAnalyticsStocks:     "analytics", // не "stocks": у аналитики свои лимиты
		PathFinanceAccrualByDay: "finance",
		PathCategoryTree:        "other",
	}
	for path, want := range cases {
		if got := groupOf(path); got != want {
			t.Errorf("groupOf(%s) = %s, want %s", path, got, want)
		}
	}
}

func TestWaitImportPolls(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		status := "pending"
		if calls >= 2 {
			status = "imported"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{
				"items": []map[string]any{{"offer_id": "pp-1", "product_id": 7, "status": status}},
			},
		})
	}))
	defer srv.Close()

	c := New("cid", "key", WithBaseURL(srv.URL))

	items, err := c.WaitImport(context.Background(), 42, 30*time.Second)
	if err != nil {
		t.Fatalf("WaitImport: %v", err)
	}
	if len(items) != 1 || items[0].Status != "imported" {
		t.Fatalf("неожиданный результат: %+v", items)
	}
}

// Get существует ради нескольких справочных методов Seller API,
// которые на POST отвечают 404. Проверяется здесь не только глагол:
// заголовок Content-Type без тела объявляет формат того, чего нет,
// и часть серверов на это обижается.
func TestGetSendsNoBody(t *testing.T) {
	var (
		method      string
		contentType string
		hasBody     bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		contentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		hasBody = len(body) > 0

		if r.Header.Get("Client-Id") == "" || r.Header.Get("Api-Key") == "" {
			t.Error("ключи должны уходить и с GET")
		}
		_, _ = w.Write([]byte(`{"result":[]}`))
	}))
	defer srv.Close()

	c := New("cid", "key", WithBaseURL(srv.URL))
	if _, err := c.Get(context.Background(), PathActionsList); err != nil {
		t.Fatalf("Get: %v", err)
	}

	if method != http.MethodGet {
		t.Errorf("метод: %s", method)
	}
	if hasBody {
		t.Error("GET не должен отправлять тело")
	}
	if contentType != "" {
		t.Errorf("без тела не должно быть и Content-Type, получено %q", contentType)
	}
}
