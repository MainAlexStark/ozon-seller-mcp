package tools

import "testing"

func TestWithFilterAddsMissingFilter(t *testing.T) {
	got := withFilter(map[string]any{"limit": 1})

	f, ok := got["filter"].(map[string]any)
	if !ok {
		t.Fatalf("filter не добавлен: %#v", got)
	}
	if f["visibility"] != "ALL" {
		t.Errorf("visibility = %v, want ALL", f["visibility"])
	}
}

func TestWithFilterAddsMissingVisibility(t *testing.T) {
	got := withFilter(map[string]any{
		"filter": map[string]any{"offer_id": []any{"pp-1"}},
	})

	f := got["filter"].(map[string]any)
	if f["visibility"] != "ALL" {
		t.Errorf("visibility должен достроиться, получено %v", f["visibility"])
	}
	if _, ok := f["offer_id"]; !ok {
		t.Error("заданный пользователем offer_id потерялся")
	}
}

func TestWithFilterKeepsExplicitVisibility(t *testing.T) {
	// Явно заданное значение переопределять нельзя.
	got := withFilter(map[string]any{
		"filter": map[string]any{"visibility": "EMPTY_STOCK"},
	})

	f := got["filter"].(map[string]any)
	if f["visibility"] != "EMPTY_STOCK" {
		t.Errorf("visibility перезаписан: %v", f["visibility"])
	}
}

func TestWithFilterHandlesNilArgs(t *testing.T) {
	got := withFilter(nil)
	if _, ok := got["filter"]; !ok {
		t.Error("на nil-аргументах filter всё равно должен появиться")
	}
}

func TestWithLimit(t *testing.T) {
	if got := withLimit(map[string]any{}, 100); got["limit"] != 100 {
		t.Errorf("лимит не проставлен: %v", got["limit"])
	}
	if got := withLimit(map[string]any{"limit": 5}, 100); got["limit"] != 5 {
		t.Errorf("заданный лимит перезаписан: %v", got["limit"])
	}
}
