package tools

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// accrual собирает начисление той же формы, что отдаёт Ozon: сумма
// строкой, виды услуг вложены в отправление.
func accrual(date string, total string, category string, services map[int]string) map[string]any {
	list := make([]any, 0, len(services))
	for id, amount := range services {
		list = append(list, map[string]any{
			"type_id": id,
			"accrued": map[string]any{"amount": amount, "currency": "RUB"},
		})
	}
	return map[string]any{
		"date":             date,
		"total_amount":     map[string]any{"amount": total, "currency": "RUB"},
		"accrued_category": category,
		"posting": map[string]any{
			"delivery_schema": "Fbs",
			"products": []any{
				map[string]any{"sku": 1, "delivery": map[string]any{"services": list}},
			},
		},
	}
}

// financeServer подставляет ответы начислений по дням.
func financeServer(t *testing.T, byDay map[string][]any) (http.HandlerFunc, *int32) {
	t.Helper()
	var calls int32

	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/finance/accrual/by-day") {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		atomic.AddInt32(&calls, 1)

		var req struct {
			Date string `json:"date"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		out := map[string]any{"accruals": byDay[req.Date], "last_id": ""}
		_ = json.NewEncoder(w).Encode(out)
	}, &calls
}

func TestFinanceSummaryAggregatesPeriod(t *testing.T) {
	byDay := map[string][]any{
		"2025-12-01": {
			accrual("2025-12-01", "500", "POSTING", map[int]string{32: "-80"}),
			accrual("2025-12-01", "-50", "NON_ITEM", nil),
		},
		"2025-12-02": {
			accrual("2025-12-02", "250.50", "POSTING", map[int]string{32: "-20"}),
		},
		"2025-12-03": {},
	}
	handler, calls := financeServer(t, byDay)

	_, server, closeFn := fakeOzon(t, ModeReadOnly, handler)
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_finance_summary", map[string]any{
		"date_from": "2025-12-01",
		"date_to":   "2025-12-03",
	})
	if isErr {
		t.Fatalf("свод не должен падать: %s", body)
	}

	// Три дня — три запроса, а не по запросу на страницу.
	if got := int(*calls); got != 3 {
		t.Errorf("ожидалось 3 запроса к Ozon, сделано %d", got)
	}

	// 500 + 250.50 начислено, −50 удержано, чистыми 700.50.
	for _, want := range []string{"750.50", "-50.00", "700.50"} {
		if !strings.Contains(body, want) {
			t.Errorf("в своде нет суммы %s:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "POSTING") || !strings.Contains(body, "NON_ITEM") {
		t.Errorf("свод должен показывать категории:\n%s", body)
	}
	if !strings.Contains(body, "-100.00") {
		t.Errorf("свод должен складывать услуги по type_id (32: -80 и -20):\n%s", body)
	}

	// Главное ради чего всё затевалось: ответ должен быть коротким.
	if len(body) > 4_000 {
		t.Errorf("свод за три дня раздулся до %d байт", len(body))
	}
}

func TestFinanceSummaryRejectsTooLongPeriod(t *testing.T) {
	handler, _ := financeServer(t, nil)
	_, server, closeFn := fakeOzon(t, ModeReadOnly, handler)
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_finance_summary", map[string]any{
		"date_from": "2025-01-01",
		"date_to":   "2025-12-31",
	})
	if !isErr {
		t.Fatalf("год должен отклоняться, получено: %s", body)
	}
	if !strings.Contains(body, "по месяцам") && !strings.Contains(body, "Разбейте") {
		t.Errorf("отказ должен подсказывать, как разбить период: %s", body)
	}
}

func TestFinanceByDayReturnsSummaryNotDump(t *testing.T) {
	// День с сотней начислений: сырым он весит десятки килобайт,
	// сводом — несколько строк.
	var many []any
	for i := 0; i < 100; i++ {
		many = append(many, accrual("2025-12-15", "110", "POSTING", map[int]string{
			16: "-10", 32: "-80", 17: "-20",
		}))
	}
	handler, _ := financeServer(t, map[string][]any{"2025-12-15": many})

	_, server, closeFn := fakeOzon(t, ModeReadOnly, handler)
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_finance_by_day", map[string]any{"date": "2025-12-15"})
	if isErr {
		t.Fatalf("свод за день не должен падать: %s", body)
	}
	if len(body) > 2_000 {
		t.Errorf("свод за день должен быть коротким, получено %d байт:\n%s", len(body), body)
	}
	if !strings.Contains(body, "11000.00") {
		t.Errorf("свод должен складывать суммы за день:\n%s", body)
	}

	// А детали по-прежнему доступны — но по явной просьбе.
	detail, isErr := callTool(t, server, "ozon_finance_by_day", map[string]any{
		"date": "2025-12-15", "detail": true,
	})
	if isErr {
		t.Fatalf("детальный режим не должен падать: %s", detail)
	}
	if !strings.Contains(detail, "accrued_category") {
		t.Errorf("детальный режим должен отдавать сырые начисления:\n%s", detail)
	}
}

// TestFetchDayStopsOnRepeatedCursor — страховка от цикла внутри сервера:
// Ozon отдаёт last_id и на последней странице тоже.
func TestFetchDayStopsOnRepeatedCursor(t *testing.T) {
	var calls int32
	handler := func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = fmt.Fprint(w, `{"accruals":[{"total_amount":{"amount":"1","currency":"RUB"},`+
			`"accrued_category":"ITEM"}],"last_id":"always-the-same"}`)
	}

	_, server, closeFn := fakeOzon(t, ModeReadOnly, handler)
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_finance_by_day", map[string]any{"date": "2025-12-15"})
	if isErr {
		t.Fatalf("вызов не должен падать: %s", body)
	}
	if got := int(atomic.LoadInt32(&calls)); got > 2 {
		t.Errorf("неподвижный курсор должен останавливать обход, сделано %d запросов", got)
	}
}

func TestStaleAnalyticsHintExplainsMisleadingRefusal(t *testing.T) {
	_, server, closeFn := fakeOzon(t, ModeReadOnly, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"code":3,"message":"date_to must be greater than date_from"}`)
	})
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_analytics_data", map[string]any{
		"date_from": "2025-12-01",
		"date_to":   "2025-12-31",
		"metrics":   []string{"revenue"},
		"dimension": []string{"day"},
	})
	if !isErr {
		t.Fatalf("отказ Ozon должен доходить как ошибка: %s", body)
	}
	if !strings.Contains(body, "ozon_finance_summary") {
		t.Errorf("подсказка должна отправлять к своду начислений:\n%s", body)
	}
	if !strings.Contains(body, "окно хранения") {
		t.Errorf("подсказка должна объяснять настоящую причину отказа:\n%s", body)
	}
	if strings.Contains(body, "характеристика категории") {
		t.Errorf("общая подсказка про характеристики здесь не к месту:\n%s", body)
	}
}

// TestSummaryTellsHowToContinue — когда период не пройден целиком,
// ответ обязан говорить, с какого дня продолжить. Без этого модель
// либо решит, что месяц посчитан, либо начнёт перебирать дни сама.
func TestSummaryTellsHowToContinue(t *testing.T) {
	from, to := mustDay("2025-12-01"), mustDay("2025-12-31")
	s := newSummary(from, to)
	s.addDay("2025-12-01", []map[string]any{
		{"total_amount": map[string]any{"amount": "100"}, "accrued_category": "POSTING"},
	}, false)
	s.next = mustDay("2025-12-02")

	got := s.render(true)
	if !strings.Contains(got, "date_from=2025-12-02") {
		t.Errorf("свод должен называть день продолжения:\n%s", got)
	}
	if !strings.Contains(got, "2025-12-01") {
		t.Errorf("свод должен показывать пройденную часть периода:\n%s", got)
	}
}

// TestSummaryWarnsAboutIncompleteDay — неполные суммы под видом полных
// хуже отказа, поэтому упёршийся в предел страниц день назван прямо.
func TestSummaryWarnsAboutIncompleteDay(t *testing.T) {
	s := newSummary(mustDay("2025-12-01"), mustDay("2025-12-01"))
	s.addDay("2025-12-01", nil, true)

	got := s.render(false)
	if !strings.Contains(got, "неполные") || !strings.Contains(got, "2025-12-01") {
		t.Errorf("день с оборванным обходом должен быть назван:\n%s", got)
	}
}
