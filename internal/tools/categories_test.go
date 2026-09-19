package tools

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// categoryNode собирает узел дерева категорий той же формы, что отдаёт Ozon.
// children и types — массивы или nil: у узла может не быть ни детей, ни типов.
func categoryNode(id int, name string, children any, types any) map[string]any {
	m := map[string]any{
		"description_category_id": id,
		"category_name":           name,
	}
	if arr, ok := children.([]any); ok && len(arr) > 0 {
		m["children"] = children
	}
	if arr, ok := types.([]any); ok && len(arr) > 0 {
		m["types"] = types
	}
	return m
}

// treeServer подставляет ответ /v1/description-category/tree.
func treeServer(tree map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/description-category/tree") {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_ = json.NewEncoder(w).Encode(tree)
	}
}

func TestCategoryTreeFlat(t *testing.T) {
	smartphone := map[string]any{"id": 10, "name": "Смартфон"}
	twoChamber := map[string]any{"id": 30, "name": "Двухкамерный"}

	tree := map[string]any{"result": []any{
		categoryNode(1, "Электроника", nil, []any{smartphone}),
		categoryNode(2, "Бытовая техника",
			[]any{categoryNode(21, "Холодильники", nil, []any{twoChamber})},
			nil),
	}}

	_, server, closeFn := fakeOzon(t, ModeReadOnly, treeServer(tree))
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_category_tree", map[string]any{
		"language": "RU",
	})
	if isErr {
		t.Fatalf("дерево не должно падать: %s", body)
	}

	// Обе категории и оба типа видны, в плоском виде.
	for _, want := range []string{
		"1 | 10 | Электроника — Смартфон",
		"21 | 30 | Бытовая техника / Холодильники — Двухкамерный",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("в плоском дереве нет строки %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `"children"`) {
		t.Errorf("плоский ответ не должен содержать вложенный JSON:\n%s", body)
	}
}

func TestCategoryTreeQueryFilters(t *testing.T) {
	tree := map[string]any{"result": []any{
		categoryNode(1, "Электроника", nil, []any{
			map[string]any{"id": 10, "name": "Смартфон"},
			map[string]any{"id": 11, "name": "Планшет"},
		}),
		categoryNode(2, "Бытовая техника", nil, []any{
			map[string]any{"id": 30, "name": "Холодильник"},
		}),
	}}

	_, server, closeFn := fakeOzon(t, ModeReadOnly, treeServer(tree))
	defer closeFn()

	// Поиск без учёта регистра возвращает только подходящую строку.
	body, isErr := callTool(t, server, "ozon_category_tree", map[string]any{
		"query": "хо",
	})
	if isErr {
		t.Fatalf("фильтр не должен падать: %s", body)
	}
	if !strings.Contains(body, "Холодильник") {
		t.Errorf("по query «хо» должна найтись строка с Холодильником:\n%s", body)
	}
	if strings.Contains(body, "Смартфон") {
		t.Errorf("по query «хо» не должна найтись строка со Смартфоном:\n%s", body)
	}
}

func TestCategoryTreeRaw(t *testing.T) {
	tree := map[string]any{"result": []any{
		categoryNode(1, "Электроника", nil, []any{map[string]any{"id": 10, "name": "Смартфон"}}),
	}}

	_, server, closeFn := fakeOzon(t, ModeReadOnly, treeServer(tree))
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_category_tree", map[string]any{
		"format": "tree",
	})
	if isErr {
		t.Fatalf("raw-режим не должен падать: %s", body)
	}
	if !strings.Contains(body, `"category_name"`) {
		t.Errorf("raw-режим должен вернуть вложенный JSON:\n%s", body)
	}
}
