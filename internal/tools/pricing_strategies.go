package tools

import (
	"encoding/json"
	"fmt"

	"github.com/MainAlexStark/ozon-seller-mcp/ozon"
)

// RegisterPricingStrategies добавляет автостратегии цен.
//
// Чем это отличается от ozon_prices_update. Обычная смена цены —
// однократное действие: вы поставили число, оно стоит, пока его не
// поменяют. Автостратегия — правило: Ozon сам сравнивает цену
// с ценами выбранных конкурентов и переставляет вашу, снова и снова,
// без чьего-либо участия. Ошибка в стратегии поэтому не сравнима
// с ошибкой в цене: одна портит одну запись, вторая работает
// постоянно и обнаруживается по выручке, а не по логам.
//
// Отсюда устройство набора. Читающих инструментов больше, чем
// пишущих, и первым делом даётся ответ на вопрос «почему у этого
// товара такая цена» (ozon_pricing_strategy_product_info) — потому
// что именно с него начинается разбор, когда маржа поехала. Из
// записи заведено только то, что нужно для управления уже принятым
// решением: создать стратегию, добавить и убрать товары, включить
// и выключить. Аварийный тормоз — ozon_pricing_strategy_status
// с enabled=false: он останавливает правило целиком, не разбирая
// товары по одному.
func (r *Registry) RegisterPricingStrategies() {
	// --- Чтение ---

	r.Add(Spec{
		Name: "ozon_pricing_strategies_list",
		Path: ozon.PathStrategyList,
		Desc: "Автостратегии цен: название, тип, включена ли, сколько товаров и конкурентов. " +
			"Тип MIN_EXT_PRICE — системная стратегия Ozon, COMP_PRICE — ваша собственная.",
		Schema: schema(obj{
			"page":  obj{"type": "integer", "default": 1, "minimum": 1},
			"limit": obj{"type": "integer", "default": maxStrategiesPerPage, "maximum": maxStrategiesPerPage},
		}),
		Build: func(a map[string]any) (any, error) {
			return capLimit(withLimit(withPage(a), maxStrategiesPerPage), maxStrategiesPerPage), nil
		},
	})

	r.Add(Spec{
		Name: "ozon_pricing_strategy_info",
		Path: ozon.PathStrategyInfo,
		Desc: "Устройство стратегии: список конкурентов и коэффициент к цене каждого. " +
			"Коэффициент — единственное число, которым стратегия управляет ценой: " +
			"1.0 держит вровень с минимальной ценой конкурентов, 0.9 ставит на 10 % ниже.",
		Schema: schema(obj{
			"strategy_id": str("Идентификатор стратегии из ozon_pricing_strategies_list"),
		}, "strategy_id"),
		Build: func(a map[string]any) (any, error) {
			id, err := stringID(a["strategy_id"])
			if err != nil {
				return nil, fmt.Errorf("strategy_id: %w", err)
			}
			return obj{"strategy_id": id}, nil
		},
	})

	r.Add(Spec{
		Name: "ozon_pricing_strategy_products",
		Path: ozon.PathStrategyItems,
		Desc: "Идентификаторы товаров, которыми управляет стратегия. " +
			"Цену конкретного товара по стратегии показывает ozon_pricing_strategy_product_info.",
		Schema: schema(obj{
			"strategy_id": str("Идентификатор стратегии"),
		}, "strategy_id"),
		Build: func(a map[string]any) (any, error) {
			id, err := stringID(a["strategy_id"])
			if err != nil {
				return nil, fmt.Errorf("strategy_id: %w", err)
			}
			return obj{"strategy_id": id}, nil
		},
	})

	r.Add(Spec{
		Name: "ozon_pricing_strategy_product_info",
		Path: ozon.PathStrategyItemInfo,
		Desc: "Почему у товара такая цена: в какой он стратегии, какую цену она выставила, когда и по какому конкуренту. " +
			"Первый инструмент при разборе «цена уехала, а я её не трогал».",
		Schema: schema(obj{
			"product_id": num("ID товара"),
		}, "product_id"),
	})

	r.Add(Spec{
		Name: "ozon_pricing_competitors",
		Path: ozon.PathStrategyCompetitors,
		Desc: "Конкуренты, доступные для автостратегий: идентификатор и название площадки. " +
			"competitor_id отсюда нужен для создания стратегии.",
		Schema: schema(obj{
			"page":  obj{"type": "integer", "default": 1, "minimum": 1},
			"limit": obj{"type": "integer", "default": maxStrategiesPerPage, "maximum": maxStrategiesPerPage},
		}),
		Build: func(a map[string]any) (any, error) {
			return capLimit(withLimit(withPage(a), maxStrategiesPerPage), maxStrategiesPerPage), nil
		},
	})

	// --- Запись ---

	r.Add(Spec{
		Name: "ozon_pricing_strategy_create",
		Path: ozon.PathStrategyCreate,
		Desc: "Создать автостратегию цен. ИЗМЕНЯЕТ ДАННЫЕ И БУДЕТ МЕНЯТЬ ЦЕНЫ ДАЛЬШЕ САМА. " +
			"Коэффициент к цене конкурента принимается в диапазоне 0.5–1.2 и проверяется до отправки: " +
			"это правило работает постоянно, а не один раз.",
		Write: true,
		Schema: schema(obj{
			"strategy_name": str("Название стратегии"),
			"competitors": arr(schema(obj{
				"competitor_id": num("ID конкурента из ozon_pricing_competitors"),
				"coefficient":   obj{"type": "number", "description": "Множитель к минимальной цене конкурентов, 0.5–1.2"},
			}, "competitor_id", "coefficient"), "Конкуренты и коэффициенты"),
		}, "strategy_name", "competitors"),
		Batch: func(a map[string]any) int {
			items, _ := a["competitors"].([]any)
			return len(items)
		},
		Build: buildStrategyCompetitors,
	})

	r.Add(Spec{
		Name: "ozon_pricing_strategy_products_add",
		Path: ozon.PathStrategyItemsAdd,
		Desc: "Передать товары под управление стратегии. ИЗМЕНЯЕТ ДАННЫЕ: с этого момента цену товара " +
			"определяет правило, а не вы. Не больше 50 товаров за вызов.",
		Write: true,
		Schema: schema(obj{
			"strategy_id": str("Идентификатор стратегии"),
			"product_id":  arr(obj{"type": "integer"}, "ID товаров, не больше 50"),
		}, "strategy_id", "product_id"),
		Batch: func(a map[string]any) int {
			ids, _ := a["product_id"].([]any)
			return len(ids)
		},
		Build: func(a map[string]any) (any, error) {
			id, err := stringID(a["strategy_id"])
			if err != nil {
				return nil, fmt.Errorf("strategy_id: %w", err)
			}
			ids, err := strategyProductIDs(a["product_id"])
			if err != nil {
				return nil, err
			}
			return obj{"strategy_id": id, "product_id": ids}, nil
		},
	})

	r.Add(Spec{
		Name: "ozon_pricing_strategy_products_delete",
		Path: ozon.PathStrategyItemsDelete,
		Desc: "Вывести товары из-под управления стратегий. ИЗМЕНЯЕТ ДАННЫЕ: цена перестаёт меняться сама " +
			"и остаётся на последнем выставленном стратегией значении — проверьте её после этого. " +
			"Идентификатор стратегии не нужен: товар состоит не более чем в одной.",
		Write: true,
		Schema: schema(obj{
			"product_id": arr(obj{"type": "integer"}, "ID товаров, не больше 50"),
		}, "product_id"),
		Batch: func(a map[string]any) int {
			ids, _ := a["product_id"].([]any)
			return len(ids)
		},
		Build: func(a map[string]any) (any, error) {
			ids, err := strategyProductIDs(a["product_id"])
			if err != nil {
				return nil, err
			}
			return obj{"product_id": ids}, nil
		},
	})

	r.Add(Spec{
		Name: "ozon_pricing_strategy_status",
		Path: ozon.PathStrategyStatus,
		Desc: "Включить или остановить стратегию целиком. ИЗМЕНЯЕТ ДАННЫЕ. " +
			"enabled=false — аварийный тормоз: цены перестают меняться сами, товары остаются в стратегии. " +
			"Это то, что нужно, когда непонятно, что происходит с ценами.",
		Write: true,
		Schema: schema(obj{
			"strategy_id": str("Идентификатор стратегии"),
			"enabled":     boolean("true — включить, false — остановить"),
		}, "strategy_id", "enabled"),
		Build: func(a map[string]any) (any, error) {
			id, err := stringID(a["strategy_id"])
			if err != nil {
				return nil, fmt.Errorf("strategy_id: %w", err)
			}
			enabled, ok := a["enabled"].(bool)
			if !ok {
				return nil, fmt.Errorf("нужен enabled: true — включить стратегию, false — остановить")
			}
			return obj{"strategy_id": id, "enabled": enabled}, nil
		},
	})
}

// Границы, которые проверяет Ozon.
const (
	maxStrategiesPerPage = 50
	maxStrategyProducts  = 50
)

// withPage достраивает номер страницы: без него методы стратегий
// отвечают ошибкой валидации, хотя первая страница — единственное
// разумное значение по умолчанию.
func withPage(a map[string]any) map[string]any {
	if a == nil {
		a = map[string]any{}
	}
	if n, ok := toInt(a["page"]); !ok || n < 1 {
		a["page"] = 1
	}
	return a
}

// strategyProductIDs приводит список товаров к числам и проверяет
// размер пачки, названный в документации.
func strategyProductIDs(v any) ([]int, error) {
	ids, err := intList(v)
	if err != nil {
		return nil, fmt.Errorf("product_id: %w", err)
	}
	if len(ids) > maxStrategyProducts {
		return nil, fmt.Errorf(
			"за вызов принимается не больше %d товаров, передано %d",
			maxStrategyProducts, len(ids))
	}
	return ids, nil
}

// buildStrategyCompetitors собирает и проверяет список конкурентов.
func buildStrategyCompetitors(a map[string]any) (any, error) {
	name, _ := a["strategy_name"].(string)
	if name == "" {
		return nil, fmt.Errorf("нужно название стратегии: по нему её потом искать в кабинете")
	}

	raw, err := json.Marshal(a["competitors"])
	if err != nil {
		return nil, fmt.Errorf("competitors: %w", err)
	}
	var competitors []ozon.StrategyCompetitor
	if err := json.Unmarshal(raw, &competitors); err != nil {
		return nil, fmt.Errorf("competitors: ожидается список вида "+
			"[{competitor_id, coefficient}], разбор не удался: %w", err)
	}
	if len(competitors) == 0 {
		return nil, fmt.Errorf("нужен хотя бы один конкурент: стратегия сравнивает цену с чужой, " +
			"а без конкурентов сравнивать не с чем. Список даёт ozon_pricing_competitors")
	}
	for _, c := range competitors {
		if err := c.Validate(); err != nil {
			return nil, err
		}
	}

	return obj{"strategy_name": name, "competitors": competitors}, nil
}
