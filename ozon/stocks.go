package ozon

import (
	"context"
	"encoding/json"
	"fmt"
)

// StockUpdate — изменение остатка одного товара на складе.
type StockUpdate struct {
	OfferID     string `json:"offer_id,omitempty"`
	ProductID   int64  `json:"product_id,omitempty"`
	Stock       int    `json:"stock"`
	WarehouseID int64  `json:"warehouse_id"`
}

// Validate проверяет запись до отправки.
func (s StockUpdate) Validate() error {
	if s.OfferID == "" && s.ProductID == 0 {
		return fmt.Errorf("ozon: нужен offer_id или product_id")
	}
	if s.WarehouseID == 0 {
		return fmt.Errorf("ozon: не задан warehouse_id (список складов — ozon_warehouse_list)")
	}
	if s.Stock < 0 {
		return fmt.Errorf("ozon: отрицательный остаток %d", s.Stock)
	}
	return nil
}

// UpdateStocks отправляет изменение остатков.
func (c *Client) UpdateStocks(ctx context.Context, updates []StockUpdate) (json.RawMessage, error) {
	if len(updates) == 0 {
		return nil, fmt.Errorf("ozon: пустой список остатков")
	}
	for _, u := range updates {
		if err := u.Validate(); err != nil {
			return nil, err
		}
	}
	return c.Call(ctx, PathStocksImport, map[string]any{"stocks": updates})
}
