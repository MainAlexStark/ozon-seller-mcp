package ozon

import (
	"context"
	"encoding/json"
	"fmt"
)

// Типы акций описаны здесь, а не в инструментах, по той же причине,
// что цены и остатки: это ЗАПИСЬ в живой магазин, и структура запроса
// должна проверяться до отправки, а не выясняться из отказа Ozon.

// ActionProduct — товар, добавляемый в акцию.
type ActionProduct struct {
	ProductID int64 `json:"product_id"`

	// ActionPrice — цена, по которой товар будет продаваться в акции.
	ActionPrice float64 `json:"action_price"`

	// Stock — число единиц для акций типа «Скидка на сток».
	// В остальных акциях поле игнорируется.
	Stock float64 `json:"stock,omitempty"`
}

// Validate проверяет форму до отправки.
func (p ActionProduct) Validate() error {
	if p.ProductID <= 0 {
		return fmt.Errorf("нужен product_id товара (его показывает ozon_action_candidates)")
	}
	if p.ActionPrice <= 0 {
		return fmt.Errorf("товар %d: акционная цена должна быть больше нуля", p.ProductID)
	}
	return nil
}

// ActionCandidate — товар в разрезе одной акции: и тот, что доступен
// для участия, и тот, что уже участвует. Ozon отдаёт их одинаковой
// структурой из двух разных методов.
type ActionCandidate struct {
	ID    int64   `json:"id"`
	Price float64 `json:"price"`

	// ActionPrice — цена по акции: у кандидата предложенная,
	// у участника действующая.
	ActionPrice float64 `json:"action_price"`

	// MaxActionPrice — потолок, выше которого Ozon в эту акцию не
	// пустит. Главное число во всей структуре: оно превращает вопрос
	// «стоит ли участвовать» из догадки в арифметику.
	MaxActionPrice float64 `json:"max_action_price"`

	AddMode  string  `json:"add_mode"`
	MinStock float64 `json:"min_stock"`
	Stock    float64 `json:"stock"`
}

// actionProductsPage — общий ответ списков товаров акции.
type actionProductsPage struct {
	Result struct {
		Products []ActionCandidate `json:"products"`
		Total    float64           `json:"total"`
	} `json:"result"`
}

// ActionProductsPageSize — сколько товаров запрашивается за раз при
// обходе списков акции.
const ActionProductsPageSize = 100

// ActionProductsFor собирает товары акции по нужным идентификаторам.
//
// У Ozon нет метода «дай кандидата по product_id» — есть только список
// страницами. Поэтому страницы обходятся до тех пор, пока не найдены
// все запрошенные товары либо не исчерпан лимит обхода: тянуть весь
// список ради пяти товаров бессмысленно, а не найти их вовсе — значит
// остаться без потолка цены и без страховки.
//
// path — PathActionCandidates для товаров, которые ещё не в акции,
// либо PathActionProducts для уже участвующих.
func (c *Client) ActionProductsFor(ctx context.Context, path string, actionID int64, want map[int64]bool, maxPages int) (map[int64]ActionCandidate, error) {
	found := make(map[int64]ActionCandidate, len(want))

	for page := 0; page < maxPages && len(found) < len(want); page++ {
		raw, err := c.Call(ctx, path, map[string]any{
			"action_id": actionID,
			"limit":     ActionProductsPageSize,
			"offset":    page * ActionProductsPageSize,
		})
		if err != nil {
			return found, err
		}

		var resp actionProductsPage
		if err := json.Unmarshal(raw, &resp); err != nil {
			return found, fmt.Errorf("ozon: разбор ответа %s: %w", path, err)
		}

		for _, product := range resp.Result.Products {
			if want[product.ID] {
				found[product.ID] = product
			}
		}

		// Страница пришла неполной — дальше ничего нет.
		if len(resp.Result.Products) < ActionProductsPageSize {
			break
		}
	}
	return found, nil
}

// Границы коэффициента автостратегии, которые проверяет Ozon.
const (
	MinCompetitorCoefficient = 0.5
	MaxCompetitorCoefficient = 1.2
)

// StrategyCompetitor — конкурент в автостратегии.
type StrategyCompetitor struct {
	CompetitorID int64 `json:"competitor_id"`

	// Coefficient — множитель к минимальной цене конкурентов.
	// 1.0 — держать цену вровень, 0.9 — на 10 % ниже.
	Coefficient float64 `json:"coefficient"`
}

// Validate проверяет коэффициент до создания стратегии.
//
// Проверка здесь не формальность: коэффициент — единственное число,
// которым стратегия управляет ценой, и она применяет его сама, снова
// и снова, без участия человека. Ошибка в нём не портит одну запись,
// а работает постоянно.
func (c StrategyCompetitor) Validate() error {
	if c.CompetitorID <= 0 {
		return fmt.Errorf("нужен competitor_id (его показывает ozon_pricing_competitors)")
	}
	if c.Coefficient < MinCompetitorCoefficient || c.Coefficient > MaxCompetitorCoefficient {
		return fmt.Errorf(
			"коэффициент %.2f вне диапазона %.1f–%.1f, который принимает Ozon. "+
				"1.0 — держать цену вровень с минимальной ценой конкурентов, 0.9 — на 10 %% ниже",
			c.Coefficient, MinCompetitorCoefficient, MaxCompetitorCoefficient)
	}
	return nil
}
