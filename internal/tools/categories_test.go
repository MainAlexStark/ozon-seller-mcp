package tools

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// typeLeaf собирает лист-тип товара: у Ozon это дочерний узел категории
// с полями type_id/type_name и пустым children.
func typeLeaf(id int, name string) map[string]any {
	return map[string]any{
		"children":  []any{},
		"disabled":  false,
		"type_id":   id,
		"type_name": name,
	}
}

// categoryNode собирает узел категории: у него category_name и
// description_category_id (иерархия), а типы — в children как листья.
func categoryNode(id int, name string, children []any) map[string]any {
	m := map[string]any{
		"description_category_id": id,
		"category_name":           name,
		"disabled":                false,
	}
	if len(children) > 0 {
		m["children"] = children
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
	tree := map[string]any{"result": []any{
		categoryNode(1, "Электроника", []any{
			typeLeaf(10, "Смартфон"),
			typeLeaf(11, "Планшет"),
		}),
		// Вложенная категория: у "Бытовая техника" id 2, у дочерней
		// "Холодильники" — свой id 21, под которым и лежат типы.
		categoryNode(2, "Бытовая техника", []any{
			categoryNode(21, "Холодильники", []any{
				typeLeaf(30, "Двухкамерный"),
			}),
		}),
	}}

	_, server, closeFn := fakeOzon(t, ModeReadOnly, treeServer(tree))
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_category_tree", map[string]any{
		"language": "RU",
	})
	if isErr {
		t.Fatalf("дерево не должно падать: %s", body)
	}

	// Тип привязывается к категории-родителю, которая несёт id.
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
		categoryNode(1, "Электроника", []any{
			typeLeaf(10, "Смартфон"),
			typeLeaf(11, "Планшет"),
		}),
		categoryNode(2, "Бытовая техника", []any{
			typeLeaf(30, "Холодильник"),
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

func TestCategoryTreeRealShape(t *testing.T) {
	// Точная форма, которую вернул Ozon: верхний узел без своего id,
	// его дети несут description_category_id, а типы лежат внутри
	// этих детей листьями с type_id/type_name.
	tree := map[string]any{"result": []any{
		map[string]any{
			// верхний узел: только название, без description_category_id
			"category_name": "Аптека",
			"children": []any{
				map[string]any{
					"category_name":           "Сопутствующие товары",
					"description_category_id": 200001537,
					"disabled":                false,
					"children": []any{
						typeLeaf(97221, "Контейнер для зубных протезов, капы"),
						typeLeaf(91828, "Таблетница"),
					},
				},
			},
		},
	}}

	_, server, closeFn := fakeOzon(t, ModeReadOnly, treeServer(tree))
	defer closeFn()

	body, isErr := callTool(t, server, "ozon_category_tree", map[string]any{
		"language": "RU",
	})
	if isErr {
		t.Fatalf("дерево не должно падать: %s", body)
	}
	if !strings.Contains(body, "200001537 | 97221 | Аптека / Сопутствующие товары — Контейнер для зубных протезов, капы") {
		t.Errorf("тип должен привязаться к id родительской категории:\n%s", body)
	}
	if !strings.Contains(body, "200001537 | 91828 | Аптека / Сопутствующие товары — Таблетница") {
		t.Errorf("второй тип той же категории не найден:\n%s", body)
	}
}

func TestCategoryTreeRaw(t *testing.T) {
	tree := map[string]any{"result": []any{
		categoryNode(1, "Электроника", []any{typeLeaf(10, "Смартфон")}),
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
