package tools

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/MainAlexStark/ozon-seller-mcp/ozon"
)

// actionOzon поднимает подставной Ozon, который отвечает на списки
// товаров акции заданным набором, а на добавление — успехом,
// запоминая тело запроса.
func actionOzon(t *testing.T, candidates []map[string]any, sent *map[string]any) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case ozon.PathActionCandidates:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"result": map[string]any{"products": candidates, "total": len(candidates)},
			})
		case ozon.PathActionProducts:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"result": map[string]any{"products": []any{}, "total": 0},
			})
		case ozon.PathActionActivate:
			body := map[string]any{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if sent != nil {
				*sent = body
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"result": map[string]any{"product_ids": []int64{1}, "rejected": []any{}},
			})
		default:
			_, _ = w.Write([]byte(`{"result":{}}`))
		}
	}
}

func TestActionsListGoesAsGET(t *testing.T) {
	// Список акций — единственный метод, отвечающий на GET. На POST
	// он отдаёт 404, и это выглядит как отключённая версия.
	var method, path string
	_, server, closeFn := fakeOzon(t, ModeReadOnly, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		_, _ = w.Write([]byte(`{"result":[]}`))
	})
	defer closeFn()

	if _, isErr := callTool(t, server, "ozon_actions_list", map[string]any{}); isErr {
		t.Fatal("вызов должен проходить")
	}
	if method != http.MethodGet {
		t.Errorf("метод должен уходить GET, ушёл %s", method)
	}
	if path != ozon.PathActionsList {
		t.Errorf("неверный путь: %s", path)
	}
}

func TestActionActivateRejectsPriceAboveCeiling(t *testing.T) {
	// Потолок задаёт площадка: цена выше него не пройдёт всё равно,
	// и отказ придёт списком причин, где нужная теряется.
	var sent map[string]any
	_, server, closeFn := fakeOzon(t, ModeWrite, actionOzon(t, []map[string]any{
		{"id": 111, "price": 1000.0, "max_action_price": 800.0},
	}, &sent))
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_action_products_activate", map[string]any{
		"action_id": 7,
		"products":  []any{map[string]any{"product_id": 111, "action_price": 900}},
	})
	if !isErr {
		t.Fatal("цена выше потолка должна отклоняться до отправки")
	}
	if sent != nil {
		t.Error("запрос на добавление не должен был уйти")
	}
	if !contains(body, "800") {
		t.Errorf("сообщение должно называть потолок: %s", body)
	}
}

func TestActionActivateStopsDeepDiscount(t *testing.T) {
	// Акционная цена — это цена, и опечатка в её порядке величины
	// стоит столько же. Отличие в том, что «в акции же дёшево»
	// делает её незаметной.
	var sent map[string]any
	_, server, closeFn := fakeOzon(t, ModeWrite, actionOzon(t, []map[string]any{
		{"id": 111, "price": 6900.0, "max_action_price": 6500.0},
	}, &sent))
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_action_products_activate", map[string]any{
		"action_id": 7,
		"products":  []any{map[string]any{"product_id": 111, "action_price": 690}},
	})
	if !isErr {
		t.Fatal("скидка глубже порога должна останавливать запись")
	}
	if sent != nil {
		t.Error("запрос не должен был уйти в Ozon")
	}
	if !contains(body, "confirm_large_change") {
		t.Errorf("сообщение должно объяснять, как подтвердить: %s", body)
	}

	// С подтверждением человека — проходит.
	if _, isErr = callTool(t, server, "ozon_action_products_activate", map[string]any{
		"action_id":            7,
		"products":             []any{map[string]any{"product_id": 111, "action_price": 690}},
		"confirm_large_change": true,
	}); isErr {
		t.Fatal("подтверждённое изменение должно проходить")
	}
	if sent == nil {
		t.Fatal("запрос не ушёл в Ozon")
	}
	if sent["action_id"] != float64(7) {
		t.Errorf("action_id не отправлен: %v", sent)
	}
}

func TestActionActivateAllowsPriceWithinThreshold(t *testing.T) {
	var sent map[string]any
	_, server, closeFn := fakeOzon(t, ModeWrite, actionOzon(t, []map[string]any{
		{"id": 111, "price": 1000.0, "max_action_price": 900.0},
	}, &sent))
	defer closeFn()

	// Скидка 10 % — обычное участие в акции, останавливать нечего.
	if body, isErr := callTool(t, server, "ozon_action_products_activate", map[string]any{
		"action_id": 7,
		"products":  []any{map[string]any{"product_id": 111, "action_price": 900}},
	}); isErr {
		t.Fatalf("обычная скидка должна проходить: %s", body)
	}
	if sent == nil {
		t.Fatal("запрос не ушёл в Ozon")
	}
}

func TestActionActivateWarnsWhenCeilingUnknown(t *testing.T) {
	// Товар, не найденный ни среди кандидатов, ни среди участников,
	// остаётся без страховки. Молча пропущенная проверка хуже
	// отсутствующей: она выглядит как сработавшая.
	var sent map[string]any
	_, server, closeFn := fakeOzon(t, ModeWrite, actionOzon(t, []map[string]any{}, &sent))
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_action_products_activate", map[string]any{
		"action_id": 7,
		"products":  []any{map[string]any{"product_id": 999, "action_price": 100}},
	})
	if isErr {
		t.Fatalf("неизвестный потолок не должен блокировать запись: %s", body)
	}
	if !contains(body, "ПРЕДУПРЕЖДЕНИЕ") {
		t.Errorf("пропущенная страховка должна быть названа вслух: %s", body)
	}
	if !contains(body, "999") {
		t.Errorf("предупреждение должно называть товар: %s", body)
	}
}

func TestActionActivateSurfacesRejected(t *testing.T) {
	// Вызов успешен, а часть товаров отклонена — это легко пропустить.
	_, server, closeFn := fakeOzon(t, ModeWrite, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == ozon.PathActionActivate {
			_, _ = w.Write([]byte(`{"result":{"product_ids":[111],"rejected":[{"product_id":222,"reason":"цена выше максимальной"}]}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{"products": []map[string]any{
				{"id": 111, "price": 1000.0, "max_action_price": 900.0},
				{"id": 222, "price": 1000.0, "max_action_price": 900.0},
			}, "total": 2},
		})
	})
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_action_products_activate", map[string]any{
		"action_id": 7,
		"products": []any{
			map[string]any{"product_id": 111, "action_price": 900},
			map[string]any{"product_id": 222, "action_price": 900},
		},
	})
	if isErr {
		t.Fatalf("вызов должен проходить: %s", body)
	}
	if !contains(body, "отклонил") || !contains(body, "222") {
		t.Errorf("отклонённые товары должны выноситься наверх: %s", body)
	}
}

func TestActionWritesNeedWriteMode(t *testing.T) {
	_, server, closeFn := fakeOzon(t, ModeReadOnly, func(w http.ResponseWriter, r *http.Request) {
		t.Error("запрос не должен был уйти в Ozon")
	})
	defer closeFn()

	cases := []struct {
		tool string
		args map[string]any
	}{
		{"ozon_action_products_activate", map[string]any{
			"action_id": 7,
			"products":  []any{map[string]any{"product_id": 1, "action_price": 100}},
		}},
		{"ozon_action_products_deactivate", map[string]any{
			"action_id": 7, "product_ids": []any{1},
		}},
	}

	for _, c := range cases {
		body, isErr := callTool(t, server, c.tool, c.args)
		if !isErr {
			t.Errorf("%s должен отказывать в режиме чтения", c.tool)
		}
		if !contains(body, "OZON_ALLOW_WRITES") {
			t.Errorf("%s: отказ должен объяснять, как включить запись", c.tool)
		}
	}
}

func TestActionReadsWorkInReadOnly(t *testing.T) {
	_, server, closeFn := fakeOzon(t, ModeReadOnly, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"result":{}}`))
	})
	defer closeFn()

	cases := []struct {
		tool string
		args map[string]any
	}{
		{"ozon_actions_list", map[string]any{}},
		{"ozon_action_candidates", map[string]any{"action_id": 7}},
		{"ozon_action_products", map[string]any{"action_id": 7}},
	}

	for _, c := range cases {
		if body, isErr := callTool(t, server, c.tool, c.args); isErr {
			t.Errorf("%s не работает в режиме чтения: %s", c.tool, body)
		}
	}
}
