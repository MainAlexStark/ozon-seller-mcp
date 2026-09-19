package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/MainAlexStark/ozon-seller-mcp/internal/mcp"
	"github.com/MainAlexStark/ozon-seller-mcp/ozon"
)

// Дерево категорий — особый случай среди read-методов, поэтому у него
// собственный обработчик, а не простая обёртка над путём.
//
// Полное дерево Ozon — это десятки тысяч категорий и типов товара,
// вложенных друг в друга. Отдать его модели как есть означает:
//
//  1. Раздуть ответ сотнями килобайт. Клиент режет результат по числу
//     токенов, и такой ответ до модели не доезжает вовсе — она видит
//     обрезанный текст и повторяет вызов, не понимая, что суть в том,
//     чтобы сузить выборку.
//
//  2. Скармливать LLM дорого. Каждая категория — это объект с
//     ключами, фигурными скобками и повторами, то есть в N раз больше
//     токенов, чем нужно, чтобы понять, какая категория подходит.
//
// Чтобы получить те же id дешевле, дерево здесь сплющивается: одна
// строка на пару «категория × тип товара» вместо вложенного JSON.
// А чтобы не отдавать сотни строк за раз, поддержан фильтр query:
// модель сужает выборку по названию и получает всего несколько строк,
// которые точно нужны.

// treeFormat — режим ответа инструмента ozon_category_tree.
const (
	// TreeFormatFlat — сплющенный список строк «id | type_id | путь — тип».
	// Значение по умолчанию: самый дешёвый для модели формат.
	TreeFormatFlat = "flat"
	// TreeFormatRaw — исходное вложенное дерево как его отдаёт Ozon.
	// Нужно только когда важна точная структура узлов.
	TreeFormatRaw = "tree"
)

// categoriesResult — итог сплющивания дерева.
type categoriesResult struct {
	// Lines — одна строка на категорию с типом товара.
	Lines []string
	// Categories — сколько категорий в итоге нашлось (до фильтра).
	Categories int
	// Types — сколько типов товара в итоге нашлось (до фильтра).
	Types int
}

// RegisterCategories добавляет работу с категориями. Вынесено отдельно
// от каталога, потому что у дерева свой обработчик: сплющивание и
// фильтр — это не пара строк обёртки.
func (r *Registry) RegisterCategories() {
	r.AddCustom(mcp.Tool{
		Name: "ozon_category_tree",
		Description: "Дерево категорий и типов товара Ozon. Нужен, чтобы получить " +
			"description_category_id и type_id — без них товар не создать. " +
			"Полное дерево огромно, поэтому по умолчанию ответ сплющивается в строки " +
			"«id | type_id | путь — тип» и умеет сужаться по query: укажите кусок названия " +
			"категории или типа, и вернётся только подходящее. " +
			"format=tree вернёт исходное вложенное дерево как у Ozon.",
		InputSchema: schema(obj{
			"language": obj{"type": "string", "enum": []string{"RU", "EN"}, "default": "RU"},
			"query":    str("Кусок названия категории или типа товара (без учёта регистра). Вместо всего дерева вернутся только подходящие строки"),
			"format": obj{
				"type":        "string",
				"enum":        []string{TreeFormatFlat, TreeFormatRaw},
				"default":     TreeFormatFlat,
				"description": "flat — одна строка на категорию с типом (дёшево для модели); tree — исходное вложенное дерево Ozon",
			},
		}),
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var a struct {
				Language string `json:"language"`
				Query    string `json:"query"`
				Format   string `json:"format"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return "", err
			}
			if a.Language == "" {
				a.Language = "RU"
			}
			if a.Format == "" {
				a.Format = TreeFormatFlat
			}

			tree, err := r.client.Call(ctx, ozon.PathCategoryTree, map[string]any{"language": a.Language})
			if err != nil {
				return "", decorate(err)
			}

			// Сырое дерево — вернуть как есть, через обычную обрезку.
			if a.Format == TreeFormatRaw {
				return r.format(ctx, tree), nil
			}

			body := renderFlat(tree, a.Language, a.Query)
			return r.safetyFor(ctx).TrimResponse(body), nil
		},
	})
}

// renderFlat сплющивает сырое дерево в текст для модели.
func renderFlat(tree json.RawMessage, language, query string) string {
	res := flattenTree(tree)
	if res.Lines == nil {
		return "Не удалось разобрать дерево категорий — Ozon вернул неожиданную структуру."
	}

	if query != "" {
		needle := strings.ToLower(strings.TrimSpace(query))
		if needle != "" {
			var kept []string
			for _, line := range res.Lines {
				if strings.Contains(strings.ToLower(line), needle) {
					kept = append(kept, line)
				}
			}
			res.Lines = kept
		}
	}

	slices.Sort(res.Lines)

	var b strings.Builder
	fmt.Fprintf(&b, "Категорий найдено: %d, типов товара: %d (это строки \"description_category_id | type_id | путь — название типа\").\n",
		res.Categories, res.Types)
	if query != "" {
		fmt.Fprintf(&b, "Отфильтровано по query %q: показано %d строк.\n", query, len(res.Lines))
	} else if len(res.Lines) > 400 {
		b.WriteString("Дерево большое: если ищете конкретную категорию, повторите вызов с query — фрагментом её названия.\n")
	}
	b.WriteString("\nГлубина пути — от корня до листа. {{откл}} означает disabled (такую категорию не выбрать).\n")
	for _, line := range res.Lines {
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// flattenTree обходит дерево и собирает строки «id | type_id | путь — тип».
func flattenTree(tree json.RawMessage) categoriesResult {
	var doc any
	if err := json.Unmarshal([]byte(string(tree)), &doc); err != nil {
		return categoriesResult{}
	}

	root, ok := doc.(map[string]any)
	var list []any
	switch {
	case ok:
		// Ozon заворачивает список в {"result": [...]}.
		if raw, hasResult := root["result"]; hasResult {
			if arr, isArr := raw.([]any); isArr {
				list = arr
			}
		}
	default:
		if arr, isArr := doc.([]any); isArr {
			list = arr
		}
	}

	var lines []string
	var res categoriesResult
	if len(list) == 0 {
		return res
	}

	for _, rootNode := range list {
		walkNode(rootNode, "", &lines, &res)
	}
	res.Lines = lines
	return res
}

// walkNode обходит один узел дерева: печатает его типы товара и
// спускается в детей.
func walkNode(node any, prefix string, lines *[]string, res *categoriesResult) {
	m, ok := node.(map[string]any)
	if !ok {
		return
	}

	name, hasName := m["category_name"]
	categoryID, _ := toInt(m["description_category_id"])
	disabledVal, _ := m["disabled"]
	disabled := disabledVal == true

	nameStr := ""
	if hasName && name != nil {
		if s, isStr := name.(string); isStr {
			nameStr = s
		}
	}
	path := prefix
	if nameStr != "" {
		if path != "" {
			path = path + " / " + nameStr
		} else {
			path = nameStr
		}
	}

	res.Categories++

	// Типы товара, если есть, живут в списке types.
	if rawTypes, hasTypes := m["types"]; hasTypes {
		if types, isArr := rawTypes.([]any); isArr {
			for _, t := range types {
				typeID, ok := toInt(nodeField(t, "id", "type_id"))
				if !ok {
					typeID = 0
				}
				typeName := ""
				if rawName := nodeField(t, "name", "type_name"); rawName != nil {
					if s, isStr := rawName.(string); isStr {
						typeName = s
					}
				}
				marker := ""
				if disabled {
					marker = "  {{откл}}"
				}
				*lines = append(*lines, fmt.Sprintf("%d | %d | %s — %s%s",
					categoryID, typeID, path, typeName, marker))
				res.Types++
			}
		}
	}

	// Дочерние узлы.
	if rawChildren, hasChildren := m["children"]; hasChildren {
		if children, isArr := rawChildren.([]any); isArr {
			for _, child := range children {
				walkNode(child, path, lines, res)
			}
		}
	}
}

// nodeField достаёт из объекта типа товара значение по одному из
// вариантов имени ключа: у Ozon бывает и id/name, и type_id/type_name.
func nodeField(node any, firstKey, secondKey string) any {
	m, ok := node.(map[string]any)
	if !ok {
		return nil
	}
	if v, has := m[firstKey]; has {
		return v
	}
	if v, has := m[secondKey]; has {
		return v
	}
	return nil
}
