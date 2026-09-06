package ozon

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Attribute — характеристика карточки.
type Attribute struct {
	ID        int64            `json:"id"`
	ComplexID int64            `json:"complex_id,omitempty"`
	Values    []AttributeValue `json:"values"`
}

// AttributeValue — одно значение характеристики.
type AttributeValue struct {
	DictionaryValueID int64  `json:"dictionary_value_id,omitempty"`
	Value             string `json:"value,omitempty"`
}

// ProductItem — товар для импорта.
type ProductItem struct {
	OfferID               string   `json:"offer_id"`
	Name                  string   `json:"name"`
	DescriptionCategoryID int64    `json:"description_category_id"`
	TypeID                int64    `json:"type_id"`
	Price                 string   `json:"price"`
	OldPrice              string   `json:"old_price,omitempty"`
	VAT                   string   `json:"vat"`
	Weight                int      `json:"weight"`
	WeightUnit            string   `json:"weight_unit"`
	Depth                 int      `json:"depth"`
	Width                 int      `json:"width"`
	Height                int      `json:"height"`
	DimensionUnit         string   `json:"dimension_unit"`
	Images                []string `json:"images,omitempty"`
	PrimaryImage          string   `json:"primary_image,omitempty"`

	Attributes []Attribute `json:"attributes"`
}

// Validate ловит то, что Ozon вернёт невнятной ошибкой валидации.
func (p ProductItem) Validate() error {
	switch {
	case p.OfferID == "":
		return fmt.Errorf("ozon: не задан offer_id")
	case p.Name == "":
		return fmt.Errorf("ozon: не задано название для %s", p.OfferID)
	case p.DescriptionCategoryID == 0:
		return fmt.Errorf("ozon: не задана категория для %s (дерево — ozon_category_tree)", p.OfferID)
	case p.TypeID == 0:
		return fmt.Errorf("ozon: не задан type_id для %s (он приходит вместе с категорией)", p.OfferID)
	case p.Price == "":
		return fmt.Errorf("ozon: не задана цена для %s", p.OfferID)
	case p.VAT == "":
		return fmt.Errorf("ozon: не задана ставка НДС для %s (\"0\" — без НДС, \"0.05\", \"0.07\", \"0.1\", \"0.2\")", p.OfferID)
	}
	return nil
}

type importResponse struct {
	Result struct {
		TaskID int64 `json:"task_id"`
	} `json:"result"`
}

// ImportProducts отправляет товары на импорт.
//
// Метод асинхронный: ответ означает «задача принята», а не «товар
// создан». Реальный результат — только через ImportInfo или WaitImport.
func (c *Client) ImportProducts(ctx context.Context, items []ProductItem) (int64, error) {
	if len(items) == 0 {
		return 0, fmt.Errorf("ozon: пустой список товаров на импорт")
	}
	for _, it := range items {
		if err := it.Validate(); err != nil {
			return 0, err
		}
	}

	var resp importResponse
	if err := c.CallInto(ctx, PathProductImport, map[string]any{"items": items}, &resp); err != nil {
		return 0, err
	}
	return resp.Result.TaskID, nil
}

// ImportItemStatus — судьба одного товара в задаче импорта.
type ImportItemStatus struct {
	OfferID   string `json:"offer_id"`
	ProductID int64  `json:"product_id"`
	Status    string `json:"status"`
	Errors    []struct {
		Code    string `json:"code"`
		Field   string `json:"field"`
		Message string `json:"message"`
	} `json:"errors"`
}

type importInfoResponse struct {
	Result struct {
		Items []ImportItemStatus `json:"items"`
		Total int                `json:"total"`
	} `json:"result"`
}

// ImportInfo возвращает статус задачи импорта.
func (c *Client) ImportInfo(ctx context.Context, taskID int64) ([]ImportItemStatus, error) {
	var resp importInfoResponse
	err := c.CallInto(ctx, PathProductImportInfo, map[string]any{"task_id": taskID}, &resp)
	if err != nil {
		return nil, err
	}
	return resp.Result.Items, nil
}

// WaitImport опрашивает задачу до завершения всех позиций.
func (c *Client) WaitImport(ctx context.Context, taskID int64, timeout time.Duration) ([]ImportItemStatus, error) {
	deadline := time.Now().Add(timeout)
	delay := 2 * time.Second

	for {
		items, err := c.ImportInfo(ctx, taskID)
		if err != nil {
			return nil, err
		}
		if allSettled(items) {
			return items, nil
		}
		if time.Now().After(deadline) {
			return items, fmt.Errorf("ozon: задача импорта %d не завершилась за %s", taskID, timeout)
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		if delay < 30*time.Second {
			delay += 2 * time.Second
		}
	}
}

func allSettled(items []ImportItemStatus) bool {
	if len(items) == 0 {
		return false
	}
	for _, it := range items {
		switch it.Status {
		case "", "pending", "processing", "imported_pending":
			return false
		}
	}
	return true
}

// UpdateAttributes меняет характеристики уже существующих карточек.
func (c *Client) UpdateAttributes(ctx context.Context, items []map[string]any) (json.RawMessage, error) {
	if len(items) == 0 {
		return nil, fmt.Errorf("ozon: пустой список характеристик")
	}
	return c.Call(ctx, PathProductAttrUpdate, map[string]any{"items": items})
}
