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

	var res categoriesResult
	if len(list) == 0 {
		return res
	}

	for _, rootNode := range list {
		walkNode(rootNode, 0, "", false, &res)
	}
	return res
}

// walkNode обходит один узел дерева и печатает типы товара под ним.
//
// У Ozon дерево устроено так: узел категории несёт category_name и
// description_category_id (иерархия), а собственно типы товара живут
// внутри него прямо в children — листьями с полями type_id и type_name.
// Поэтому тип привязывается к категории-родителю, которая уже содержит
// description_category_id. Она передаётся аргументом catID вместе с
// собранным путём catPath: они спускаются вглубь, пока дерево это
// позволяет, а при встрече листа-типа подставляются в строку.
func walkNode(node any, catID int, catPath string, catDisabled bool, res *categoriesResult) {
	m, ok := node.(map[string]any)
	if !ok {
		return
	}

	// Лист-тип: есть type_id, своей категории у него нет — id и путь
	// берутся от родителя.
	if typeID, isType := toInt(m["type_id"]); isType {
		typeName := ""
		if rawName, hasName := m["type_name"]; hasName && rawName != nil {
			if s, isStr := rawName.(string); isStr {
				typeName = s
			}
		}
		disabledVal, _ := m["disabled"]
		disabled := disabledVal == true || catDisabled
		marker := ""
		if disabled {
			marker = "  {{откл}}"
		}
		res.Types++
		res.Lines = append(res.Lines,
			fmt.Sprintf("%d | %d | %s — %s%s", catID, typeID, catPath, typeName, marker))
		return
	}

	// Узел категории. Его собственный description_category_id, если есть,
	// становится текущим для всех типов внутри; имя добавляется к пути.
	name, hasName := m["category_name"]
	ownID, hasOwnID := toInt(m["description_category_id"])
	if hasOwnID {
		catID = ownID
	}
	disabledVal, _ := m["disabled"]
	catDisabled = catDisabled || disabledVal == true

	if hasName && name != nil {
		if nameStr, isStr := name.(string); isStr && nameStr != "" {
			res.Categories++
			if catPath != "" {
				catPath = catPath + " / " + nameStr
			} else {
				catPath = nameStr
			}
		}
	}

	if rawChildren, hasChildren := m["children"]; hasChildren {
		if children, isArr := rawChildren.([]any); isArr {
			for _, child := range children {
				walkNode(child, catID, catPath, catDisabled, res)
			}
		}
	}
}
