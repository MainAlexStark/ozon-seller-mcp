package ozon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMoneyAcceptsAllForms(t *testing.T) {
	var v struct{ A, B, C, D Money }
	if err := json.Unmarshal([]byte(`{"A":{"amount":"362.5","currency":"RUB"},"B":299,"C":"12,5","D":null}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.A != 362.5 || v.B != 299 || v.C != 12.5 || v.D != 0 {
		t.Fatalf("разобрано неверно: %+v", v)
	}
}

func TestLeaveActionBoostingByPriceAboveLimit(t *testing.T) {
	var update, deactivate map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case PathActionProducts:
			_, _ = w.Write([]byte(`{"products":[{"id":5,"price":{"amount":"299","currency":"RUB"},"action_price":{"amount":"280","currency":"RUB"},"max_action_price":{"amount":"362.4","currency":"RUB"}}],"total":1,"last_id":""}`))
		case PathActionUpdate:
			update = body
			_, _ = w.Write([]byte(`{"active_product_ids":[],"deactivated_product_ids":["5"],"rejected":[],"warnings":[]}`))
		case PathActionDeactivate:
			deactivate = body
			_, _ = w.Write([]byte(`{"product_ids":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New("id", "key", WithBaseURL(srv.URL), WithMaxRetries(0))
	res, err := LeaveAction(context.Background(), c, 77, []int64{5})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Left) != 1 || res.Left[0] != 5 || len(res.Failed) != 0 {
		t.Fatalf("товар должен быть выведен: %+v", res)
	}
	rows, _ := update["products"].([]any)
	if len(rows) != 1 {
		t.Fatalf("тело update: %v", update)
	}
	price := rows[0].(map[string]any)["action_price"].(map[string]any)["amount"]
	if price != "363" {
		t.Errorf("цена вывода должна быть выше лимита 362.4, ушло %v", price)
	}
	if deactivate != nil {
		t.Error("v2 deactivate не нужен, когда бустинг вывел товар")
	}
}

func TestLeaveActionPromocodesFallsBackToV2(t *testing.T) {
	var deactivate map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case PathActionProducts:
			_, _ = w.Write([]byte(`{"products":[],"total":0}`))
		case PathActionDeactivate:
			deactivate = body
			_, _ = w.Write([]byte(`{"product_ids":["9"]}`))
		default:
			t.Errorf("неожиданный вызов %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New("id", "key", WithBaseURL(srv.URL), WithMaxRetries(0))
	res, err := LeaveAction(context.Background(), c, 3, []int64{9})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Left) != 1 || deactivate == nil {
		t.Fatalf("ожидался вывод через v2: %+v, %v", res, deactivate)
	}
	if ids, _ := deactivate["product_ids"].([]any); len(ids) != 1 || ids[0] != "9" {
		t.Errorf("v2 ждёт product_ids строками: %v", deactivate["product_ids"])
	}
}
