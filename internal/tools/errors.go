package tools

import (
	"errors"

	"github.com/MainAlexStark/ozon-seller-mcp/ozon"
)

// asAPI — обёртка над errors.As, чтобы registry.go читался ровнее.
func asAPI(err error, target **ozon.APIError) bool {
	return errors.As(err, target)
}

// obj — сокращение для описания схем: карты в JSON Schema встречаются
// на каждой строке, и без него объявления инструментов тонут в скобках.
type obj = map[string]any

// schema собирает JSON Schema объекта с перечисленными свойствами.
func schema(props obj, required ...string) obj {
	s := obj{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// str описывает строковое поле схемы.
func str(desc string) obj { return obj{"type": "string", "description": desc} }

// num описывает целочисленное поле схемы.
func num(desc string) obj { return obj{"type": "integer", "description": desc} }

// boolean описывает булево поле схемы.
func boolean(desc string) obj { return obj{"type": "boolean", "description": desc} }

// arr описывает массив элементов заданного типа.
func arr(items obj, desc string) obj {
	return obj{"type": "array", "items": items, "description": desc}
}
