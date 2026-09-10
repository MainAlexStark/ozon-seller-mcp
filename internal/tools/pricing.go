package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/MainAlexStark/ozon-seller-mcp/internal/mcp"
	"github.com/MainAlexStark/ozon-seller-mcp/ozon"
)

// RegisterPricing добавляет инструменты цен и остатков.
func (r *Registry) RegisterPricing() {
	// --- Чтение ---

	r.Add(Spec{
		Name: "ozon_prices_info",
		Path: ozon.PathPricesInfo,
		Desc: "Цены товаров вместе с комиссиями Ozon и тарифами логистики. " +
			"Это единственный способ узнать реальную выплату по товару — считать маржу по одной цене витрины бессмысленно.",
		Schema: schema(obj{
			"filter": schema(obj{
				"offer_id":   arr(obj{"type": "string"}, "Артикулы продавца"),
				"product_id": arr(obj{"type": "integer"}, "ID товаров"),
				"visibility": obj{"type": "string", "default": "ALL"},
			}),
			"limit":  obj{"type": "integer", "default": 100, "maximum": 1000},
			"cursor": str("Курсор постраничного обхода"),
		}),
		Build: func(a map[string]any) (any, error) {
			return withLimit(withFilter(a), 100), nil
		},
	})

	r.Add(Spec{
		Name: "ozon_stocks_info",
		Path: ozon.PathStocksInfo,
		Desc: "Остатки товаров: сколько на складах, сколько зарезервировано.",
		Schema: schema(obj{
			"filter": schema(obj{
				"offer_id":   arr(obj{"type": "string"}, "Артикулы продавца"),
				"product_id": arr(obj{"type": "integer"}, "ID товаров"),
				"visibility": obj{"type": "string", "default": "ALL"},
			}),
			"limit":  obj{"type": "integer", "default": 100},
			"cursor": str("Курсор постраничного обхода"),
		}),
		Build: func(a map[string]any) (any, error) {
			return withLimit(withFilter(a), 100), nil
		},
	})

	r.Add(Spec{
		Name: "ozon_stocks_by_warehouse",
		Path: ozon.PathStocksByWarehouse,
		Desc: "Остатки FBS в разрезе складов. Нужен список SKU или артикулов: " +
			"по всему каталогу сразу метод не отвечает.",
		Schema: schema(obj{
			"sku":      arr(obj{"type": "string"}, "Список SKU"),
			"offer_id": arr(obj{"type": "string"}, "Артикулы продавца — альтернатива sku"),
			"limit":    obj{"type": "integer", "default": 100, "maximum": 1000},
			"cursor":   str("Курсор постраничного обхода"),
		}),
		Build: func(a map[string]any) (any, error) {
			if a == nil {
				a = map[string]any{}
			}

			// SKU приезжают числами из предыдущих ответов, а метод ждёт
			// строки — приводим, как и в остальных местах.
			for _, field := range []string{"sku", "offer_id"} {
				raw, ok := a[field]
				if !ok || raw == nil {
					continue
				}
				list, err := stringList(raw)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", field, err)
				}
				a[field] = list
			}

			// Без выборки Ozon отвечает ошибкой про limit, хотя дело
			// не в нём: метод просто не умеет отдавать всё подряд.
			if a["sku"] == nil && a["offer_id"] == nil {
				return nil, fmt.Errorf("нужен список sku или offer_id: остатки по всему каталогу " +
					"этот метод не отдаёт. Идентификаторы можно взять из ozon_product_list")
			}

			// limit обязателен, и его отсутствие выглядит как ошибка
			// формы запроса, а не как «вы не указали лимит».
			return clampLimit(a, 1, 1000), nil
		},
	})

	// --- Запись: цены со страховкой ---

	r.AddCustom(mcp.Tool{
		Name: "ozon_prices_update",
		Description: "Изменить цены товаров. ИЗМЕНЯЕТ ДАННЫЕ В ЖИВОМ МАГАЗИНЕ. " +
			"Перед записью новые цены сверяются с текущими: изменение больше порога (по умолчанию 30 %) " +
			"останавливается и требует confirm_large_change — это защита от потерянного нуля.",
		InputSchema: schema(obj{
			"prices": arr(schema(obj{
				"offer_id":   str("Артикул продавца"),
				"product_id": num("ID товара (альтернатива offer_id)"),
				"price":      str("Новая цена строкой, например \"690\""),
				"old_price":  str("Зачёркнутая цена; \"0\" убирает скидку"),
				"min_price":  str("Минимальная цена для автостратегий"),
			}, "price"), "Список изменений цен"),
			"confirm_large_change": boolean("Подтвердить изменение, выходящее за порог. Ставьте только когда цена проверена человеком."),
		}, "prices"),
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var a struct {
				Prices  []ozon.PriceUpdate `json:"prices"`
				Confirm bool               `json:"confirm_large_change"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return "", fmt.Errorf("не разобрать аргументы: %w", err)
			}

			safety := r.safetyFor(ctx)
			if err := safety.CheckWrite("ozon_prices_update"); err != nil {
				return "", err
			}
			if err := safety.CheckBatchSize(len(a.Prices)); err != nil {
				return "", err
			}
			if len(a.Prices) == 0 {
				return "", fmt.Errorf("пустой список цен")
			}

			// Формат проверяем до всякого обращения к сети.
			for _, p := range a.Prices {
				if err := p.Validate(); err != nil {
					return "", err
				}
			}

			// Страховка: сверяем с текущими ценами.
			changes, warn, err := r.priceChanges(ctx, a.Prices)
			if err != nil {
				return "", err
			}
			if err := safety.CheckPriceChanges(changes, a.Confirm); err != nil {
				return "", err
			}

			resp, err := r.client.UpdatePrices(ctx, a.Prices)
			if err != nil {
				return "", decorate(err)
			}

			body := r.format(ctx, resp)
			if warn != "" {
				body = warn + "\n\n" + body
			}
			return body, nil
		},
	})

	// --- Запись: остатки ---

	r.AddCustom(mcp.Tool{
		Name: "ozon_stocks_update",
		Description: "Изменить остатки товаров на складе. ИЗМЕНЯЕТ ДАННЫЕ В ЖИВОМ МАГАЗИНЕ. " +
			"warehouse_id берите из ozon_warehouse_list. Остаток 0 снимает товар с продажи.",
		InputSchema: schema(obj{
			"stocks": arr(schema(obj{
				"offer_id":     str("Артикул продавца"),
				"product_id":   num("ID товара (альтернатива offer_id)"),
				"stock":        num("Новый остаток"),
				"warehouse_id": num("ID склада из ozon_warehouse_list"),
			}, "stock", "warehouse_id"), "Список изменений остатков"),
		}, "stocks"),
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var a struct {
				Stocks []ozon.StockUpdate `json:"stocks"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return "", fmt.Errorf("не разобрать аргументы: %w", err)
			}

			safety := r.safetyFor(ctx)
			if err := safety.CheckWrite("ozon_stocks_update"); err != nil {
				return "", err
			}
			if err := safety.CheckBatchSize(len(a.Stocks)); err != nil {
				return "", err
			}

			resp, err := r.client.UpdateStocks(ctx, a.Stocks)
			if err != nil {
				return "", decorate(err)
			}
			return r.format(ctx, resp), nil
		},
	})
}

// priceChanges сопоставляет новые цены с текущими.
//
// Если текущие цены получить не удалось, запись НЕ блокируется: иначе
// временный сбой чтения парализовал бы работу. Но в ответ добавляется
// предупреждение, чтобы отсутствие проверки не осталось незамеченным.
func (r *Registry) priceChanges(ctx context.Context, updates []ozon.PriceUpdate) ([]PriceChange, string, error) {
	var offerIDs []string
	for _, u := range updates {
		if u.OfferID != "" {
			offerIDs = append(offerIDs, u.OfferID)
		}
	}
	if len(offerIDs) == 0 {
		return nil, "ПРЕДУПРЕЖДЕНИЕ: цены заданы по product_id, поэтому сверка с текущими ценами не выполнялась.", nil
	}

	current, err := r.client.GetPrices(ctx, offerIDs)
	if err != nil {
		return nil, fmt.Sprintf(
			"ПРЕДУПРЕЖДЕНИЕ: не удалось прочитать текущие цены (%v), страховка от опечатки не сработала. "+
				"Проверьте результат в кабинете.", err), nil
	}

	var changes []PriceChange
	for _, u := range updates {
		if u.OfferID == "" {
			continue
		}
		newPrice, err := u.PriceFloat()
		if err != nil {
			return nil, "", err
		}
		changes = append(changes, PriceChange{
			Item: u.OfferID,
			Old:  current[u.OfferID].Price,
			New:  newPrice,
		})
	}
	return changes, "", nil
}
