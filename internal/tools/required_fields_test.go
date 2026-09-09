package tools

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// Ozon требует поля, о существовании которых спрашивающий не знает:
// список статусов там, где спрашивают «какие у меня поставки», лимит
// не меньше двадцати там, где просят «пару отзывов», один день там,
// где ждут период. Каждый такой случай раньше отвечал ошибкой
// валидации, и по её тексту было не догадаться, чего от тебя хотят.
//
// Тесты ниже фиксируют, что запрос уходит собранным.

func TestSupplyOrdersListFillsRequiredFilter(t *testing.T) {
	var body map[string]any
	_, server, closeFn := fakeOzon(t, ModeReadOnly, captureBody(t, &body))
	defer closeFn()

	if _, isErr := callTool(t, server, "ozon_supply_orders_list", map[string]any{}); isErr {
		t.Fatal("вызов без фильтра должен проходить")
	}

	if body["sort_by"] != defaultSupplySortBy {
		t.Errorf("сортировка обязательна, отправлено %v", body["sort_by"])
	}

	filter, ok := body["filter"].(map[string]any)
	if !ok {
		t.Fatalf("filter не отправлен: %v", body)
	}
	states, ok := filter["states"].([]any)
	if !ok || len(states) == 0 {
		t.Fatalf("список статусов должен быть непустым: %v", filter)
	}
	if len(states) != len(supplyOrderStates) {
		t.Errorf("без фильтра ожидались все статусы (%d), отправлено %d", len(supplyOrderStates), len(states))
	}
}

func TestSupplyOrdersListStripsStatePrefix(t *testing.T) {
	// ozon_supply_orders_counters отдаёт статусы с префиксом
	// ORDER_STATE_, а фильтр принимает их без него и молча
	// выбрасывает незнакомые значения — фильтр становится пустым,
	// и Ozon отвечает «States: value must contain at least 1 item».
	var body map[string]any
	_, server, closeFn := fakeOzon(t, ModeReadOnly, captureBody(t, &body))
	defer closeFn()

	callTool(t, server, "ozon_supply_orders_list", map[string]any{
		"filter": map[string]any{"states": []any{"ORDER_STATE_COMPLETED", "IN_TRANSIT"}},
	})

	filter, _ := body["filter"].(map[string]any)
	states, _ := filter["states"].([]any)
	if len(states) != 2 || states[0] != "COMPLETED" || states[1] != "IN_TRANSIT" {
		t.Errorf("статусы должны уходить без префикса: %v", states)
	}
}

func TestReviewsLimitIsRaisedToMinimum(t *testing.T) {
	var body map[string]any
	_, server, closeFn := fakeOzon(t, ModeReadOnly, captureBody(t, &body))
	defer closeFn()

	// Ozon принимает limit только в диапазоне [20, 100] и требует его
	// всегда — даже когда о нём не просили.
	callTool(t, server, "ozon_reviews_list", map[string]any{})
	if body["limit"] != float64(minReviewsLimit) {
		t.Errorf("без limit должно уходить %d, отправлено %v", minReviewsLimit, body["limit"])
	}

	callTool(t, server, "ozon_reviews_list", map[string]any{"limit": 3})
	if body["limit"] != float64(minReviewsLimit) {
		t.Errorf("слишком маленький limit должен подниматься до %d, отправлено %v", minReviewsLimit, body["limit"])
	}

	callTool(t, server, "ozon_reviews_list", map[string]any{"limit": 500})
	if body["limit"] != float64(maxReviewsLimit) {
		t.Errorf("слишком большой limit должен опускаться до %d, отправлено %v", maxReviewsLimit, body["limit"])
	}
}

func TestFinanceByDayAsksForOneDay(t *testing.T) {
	var body map[string]any
	_, server, closeFn := fakeOzon(t, ModeReadOnly, captureBody(t, &body))
	defer closeFn()

	// Метод про один день, а не про период: у него нет date_from/date_to,
	// и попытка отдать диапазон возвращала ошибку про «10 runes».
	callTool(t, server, "ozon_finance_by_day", map[string]any{})

	date, _ := body["date"].(string)
	if date == "" {
		t.Fatalf("день не отправлен: %v", body)
	}
	if _, err := time.Parse("2006-01-02", date); err != nil {
		t.Errorf("день должен быть в формате YYYY-MM-DD, отправлено %q", date)
	}
	if want := time.Now().AddDate(0, 0, -1).Format("2006-01-02"); date != want {
		t.Errorf("по умолчанию ожидался вчерашний день (%s), отправлено %s", want, date)
	}

	if body["date_from"] != nil || body["date_to"] != nil {
		t.Errorf("период этот метод не принимает: %v", body)
	}
}

func TestFinanceByDayRejectsPeriodFormat(t *testing.T) {
	_, server, closeFn := fakeOzon(t, ModeReadOnly, captureBody(t, new(map[string]any)))
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_finance_by_day", map[string]any{"date": "2026-08-01/2026-08-31"})
	if !isErr {
		t.Fatal("диапазон вместо дня должен отклоняться до отправки")
	}
	if !contains(body, "YYYY-MM-DD") {
		t.Errorf("сообщение должно называть ожидаемый формат: %s", body)
	}
}

func TestFinancePostingsNeedsPostingNumbers(t *testing.T) {
	var body map[string]any
	_, server, closeFn := fakeOzon(t, ModeReadOnly, captureBody(t, &body))
	defer closeFn()

	// Номера отправлений — единственный вход этого метода; период он
	// не принимает вовсе.
	if _, isErr := callTool(t, server, "ozon_finance_postings", map[string]any{
		"posting_numbers": []any{"35029174-0818-1"},
	}); isErr {
		t.Fatal("вызов с номерами должен проходить")
	}
	numbers, ok := body["posting_numbers"].([]any)
	if !ok || len(numbers) != 1 {
		t.Fatalf("номера не отправлены: %v", body)
	}

	tooMany := make([]any, maxPostingsPerAccrualRequest+1)
	for i := range tooMany {
		tooMany[i] = "0000000000-0000-1"
	}
	msg, isErr := callTool(t, server, "ozon_finance_postings", map[string]any{"posting_numbers": tooMany})
	if !isErr {
		t.Fatal("превышение лимита должно отклоняться до отправки")
	}
	if !contains(msg, "200") {
		t.Errorf("сообщение должно называть предел: %s", msg)
	}
}

func TestFBSPostingsGetPeriodWhenNoneAsked(t *testing.T) {
	// Без периода Ozon отвечает «processed_at_to must be set», хотя
	// спрашивают обычно просто «что там с заказами».
	var body map[string]any
	_, server, closeFn := fakeOzon(t, ModeReadOnly, captureBody(t, &body))
	defer closeFn()

	if _, isErr := callTool(t, server, "ozon_postings_list", map[string]any{}); isErr {
		t.Fatal("вызов без периода должен проходить")
	}

	filter, ok := body["filter"].(map[string]any)
	if !ok || filter["since"] == "" || filter["to"] == "" {
		t.Fatalf("период не достроен: %v", body)
	}
}

func TestStocksByWarehouseNeedsSelection(t *testing.T) {
	var reached bool
	_, server, closeFn := fakeOzon(t, ModeReadOnly, func(w http.ResponseWriter, r *http.Request) {
		reached = true
	})
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_stocks_by_warehouse", map[string]any{})
	if !isErr {
		t.Fatal("без sku и offer_id вызов должен отклоняться")
	}
	if reached {
		t.Error("запрос не должен был уйти в Ozon")
	}
	if !contains(body, "sku") {
		t.Errorf("сообщение должно объяснять, чего не хватает: %s", body)
	}
}

func TestStocksByWarehouseSendsStringsAndLimit(t *testing.T) {
	var body map[string]any
	_, server, closeFn := fakeOzon(t, ModeReadOnly, captureBody(t, &body))
	defer closeFn()

	// SKU приезжают числами из предыдущих ответов, а метод ждёт строки.
	if _, isErr := callTool(t, server, "ozon_stocks_by_warehouse", map[string]any{
		"sku": []any{3045304299},
	}); isErr {
		t.Fatal("числовые SKU должны приниматься")
	}

	sku, ok := body["sku"].([]any)
	if !ok || len(sku) != 1 {
		t.Fatalf("sku не отправлены: %v", body)
	}
	if _, isString := sku[0].(string); !isString {
		t.Errorf("SKU должен уходить строкой: %T", sku[0])
	}
	if body["limit"] == nil {
		t.Error("limit обязателен, без него метод отвечает ошибкой формы запроса")
	}
}

// contains — короткая обёртка, чтобы проверки читались одной строкой.
func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
