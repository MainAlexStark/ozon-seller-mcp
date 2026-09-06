package ozon

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

// PriceUpdate — изменение цены одного товара.
//
// Ozon принимает цены строками. Здесь они тоже строки, но в них
// проверяется, что это действительно число: опечатка вида "1 990"
// молча уедет в API и вернётся невнятной ошибкой валидации.
type PriceUpdate struct {
	OfferID   string `json:"offer_id,omitempty"`
	ProductID int64  `json:"product_id,omitempty"`

	// Price — цена со скидкой, которую увидит покупатель.
	Price string `json:"price"`
	// OldPrice — зачёркнутая цена. Пустая строка — не менять.
	OldPrice string `json:"old_price,omitempty"`
	// MinPrice — минимальная цена для автостратегий.
	MinPrice string `json:"min_price,omitempty"`

	CurrencyCode      string `json:"currency_code,omitempty"`
	AutoActionEnabled string `json:"auto_action_enabled,omitempty"` // ENABLED | DISABLED | UNKNOWN
}

// Validate проверяет запись до отправки.
func (p PriceUpdate) Validate() error {
	if p.OfferID == "" && p.ProductID == 0 {
		return fmt.Errorf("ozon: нужен offer_id или product_id")
	}
	if p.Price == "" {
		return fmt.Errorf("ozon: не задана цена для %s", p.key())
	}
	for name, v := range map[string]string{"price": p.Price, "old_price": p.OldPrice, "min_price": p.MinPrice} {
		if v == "" {
			continue
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("ozon: %s=%q для %s — не число (Ozon ждёт строку вида \"690\" или \"690.00\")", name, v, p.key())
		}
		if f < 0 {
			return fmt.Errorf("ozon: %s=%q для %s — отрицательная цена", name, v, p.key())
		}
	}
	return nil
}

func (p PriceUpdate) key() string {
	if p.OfferID != "" {
		return p.OfferID
	}
	return fmt.Sprintf("product_id=%d", p.ProductID)
}

// Key — публичный идентификатор товара в сообщениях об ошибках.
func (p PriceUpdate) Key() string { return p.key() }

// PriceFloat возвращает цену числом.
func (p PriceUpdate) PriceFloat() (float64, error) {
	return strconv.ParseFloat(p.Price, 64)
}

// UpdatePrices отправляет изменение цен.
//
// Метод сознательно не делает никаких проверок «разумности» цены —
// это задача уровня выше (см. internal/tools: страховка от опечатки
// в порядке величины). Клиент отвечает только за корректность формата.
func (c *Client) UpdatePrices(ctx context.Context, updates []PriceUpdate) (json.RawMessage, error) {
	if len(updates) == 0 {
		return nil, fmt.Errorf("ozon: пустой список цен")
	}
	for _, u := range updates {
		if err := u.Validate(); err != nil {
			return nil, err
		}
	}
	return c.Call(ctx, PathPricesImport, map[string]any{"prices": updates})
}

// CurrentPrice — текущая цена товара, как её отдаёт Ozon.
type CurrentPrice struct {
	OfferID   string
	ProductID int64
	Price     float64
	OldPrice  float64
}

// pricesInfoResponse описывает ровно те поля /v5/product/info/prices,
// которые нужны для сверки перед записью. Остальное не разбирается
// намеренно: чем меньше полей, тем реже клиент ломается на обновлениях.
type pricesInfoResponse struct {
	Items []struct {
		OfferID   string `json:"offer_id"`
		ProductID int64  `json:"product_id"`
		Price     struct {
			Price    string `json:"price"`
			OldPrice string `json:"old_price"`
		} `json:"price"`
	} `json:"items"`
}

// GetPrices возвращает текущие цены по списку offer_id.
//
// Нужен страховке от опечатки: чтобы понять, что новая цена
// отличается от текущей в разы, надо знать текущую.
func (c *Client) GetPrices(ctx context.Context, offerIDs []string) (map[string]CurrentPrice, error) {
	if len(offerIDs) == 0 {
		return map[string]CurrentPrice{}, nil
	}

	payload := map[string]any{
		"filter": map[string]any{
			"offer_id":   offerIDs,
			"visibility": "ALL",
		},
		"limit": len(offerIDs),
	}

	var resp pricesInfoResponse
	if err := c.CallInto(ctx, PathPricesInfo, payload, &resp); err != nil {
		return nil, err
	}

	out := make(map[string]CurrentPrice, len(resp.Items))
	for _, it := range resp.Items {
		price, _ := strconv.ParseFloat(it.Price.Price, 64)
		oldPrice, _ := strconv.ParseFloat(it.Price.OldPrice, 64)
		out[it.OfferID] = CurrentPrice{
			OfferID:   it.OfferID,
			ProductID: it.ProductID,
			Price:     price,
			OldPrice:  oldPrice,
		}
	}
	return out, nil
}
