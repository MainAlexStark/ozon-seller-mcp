package ozon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
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

// Money — денежная сумма из ответа Ozon. Новые методы акций отдают
// её объектом {"amount":"88","currency":"RUB"}, старые — числом;
// разбираются оба вида, чтобы смена версии не обнуляла цены молча.
type Money float64

// UnmarshalJSON принимает число, строку с числом и объект Money.
func (m *Money) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*m = 0
		return nil
	}
	switch b[0] {
	case '{':
		var o struct {
			Amount json.RawMessage `json:"amount"`
		}
		if err := json.Unmarshal(b, &o); err != nil {
			return err
		}
		if len(o.Amount) == 0 {
			*m = 0
			return nil
		}
		return m.UnmarshalJSON(o.Amount)
	case '"':
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		str = strings.ReplaceAll(strings.TrimSpace(str), ",", ".")
		if str == "" {
			*m = 0
			return nil
		}
		f, err := strconv.ParseFloat(str, 64)
		if err != nil {
			return fmt.Errorf("сумма %q: %w", str, err)
		}
		*m = Money(f)
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	*m = Money(f)
	return nil
}

// MoneyRUB — сумма в виде, который принимают новые методы акций.
func MoneyRUB(v float64) map[string]string {
	return map[string]string{"amount": strconv.FormatFloat(math.Round(v*100)/100, 'f', -1, 64), "currency": "RUB"}
}

// ActionCandidate — товар в разрезе одной акции: и тот, что доступен
// для участия, и тот, что уже участвует. Ozon отдаёт их одинаковой
// структурой из двух разных методов.
type ActionCandidate struct {
	ID    int64 `json:"id"`
	Price Money `json:"price"`

	// ActionPrice — цена по акции: у кандидата предложенная,
	// у участника действующая.
	ActionPrice Money `json:"action_price"`

	// MaxActionPrice — потолок (лимит акции), выше которого Ozon в эту
	// акцию не пустит. Главное число во всей структуре: оно превращает
	// вопрос «стоит ли участвовать» из догадки в арифметику. Цена выше
	// лимита в /v1/actions/products/update выводит товар из бустингов.
	MaxActionPrice Money `json:"max_action_price"`

	AddMode  string  `json:"add_mode"`
	MinStock float64 `json:"min_stock"`
	Stock    float64 `json:"stock"`
}

// actionProductsPage — общий ответ списков товаров акции: v2 отдаёт
// products на верхнем уровне, v1 — внутри result.
type actionProductsPage struct {
	Products []ActionCandidate `json:"products"`
	LastID   string            `json:"last_id"`
	Result   struct {
		Products []ActionCandidate `json:"products"`
		LastID   string            `json:"last_id"`
	} `json:"result"`
}

func (p actionProductsPage) items() ([]ActionCandidate, string) {
	if len(p.Products) > 0 || p.LastID != "" {
		return p.Products, p.LastID
	}
	return p.Result.Products, p.Result.LastID
}

// ActionProductsPageSize — сколько товаров запрашивается за раз при
// обходе списков акции (у v2 это и есть максимум).
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

	lastID := ""
	for page := 0; page < maxPages && len(found) < len(want); page++ {
		req := map[string]any{"action_id": actionID, "limit": ActionProductsPageSize}
		if lastID != "" {
			req["last_id"] = lastID
		}
		raw, err := c.Call(ctx, path, req)
		if err != nil {
			// v2 отвечает 404 «Resource not found», когда товаров дальше
			// (или вовсе) нет — это конец списка, а не сбой.
			if isNotFound(err) {
				return found, nil
			}
			return found, err
		}

		var resp actionProductsPage
		if err := json.Unmarshal(raw, &resp); err != nil {
			return found, fmt.Errorf("ozon: разбор ответа %s: %w", path, err)
		}
		products, next := resp.items()

		for _, product := range products {
			if want[product.ID] {
				found[product.ID] = product
			}
		}

		// Страница неполная или курсор не сдвинулся — дальше ничего нет.
		if len(products) < ActionProductsPageSize || next == "" || next == lastID {
			break
		}
		lastID = next
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

// isNotFound — 404 «Resource not found» от Ozon.
func isNotFound(err error) bool {
	var api *APIError
	return errors.As(err, &api) && api.StatusCode == 404 &&
		strings.Contains(strings.ToLower(api.Message), "not found")
}
