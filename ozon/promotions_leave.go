package ozon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// flexID — идентификатор товара, который новые методы акций отдают то
// строкой ("88888"), то числом.
type flexID int64

func (f *flexID) UnmarshalJSON(b []byte) error {
	b = bytes.Trim(bytes.TrimSpace(b), `"`)
	if len(b) == 0 || string(b) == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return err
	}
	*f = flexID(n)
	return nil
}

func flexIDs(in []flexID) []int64 {
	out := make([]int64, 0, len(in))
	for _, id := range in {
		out = append(out, int64(id))
	}
	return out
}

// ActionUpdateRows переводит товары в тело /v1/actions/products/update:
// цена — объектом Money, остаток — строкой.
func ActionUpdateRows(products []ActionProduct) []map[string]any {
	rows := make([]map[string]any, 0, len(products))
	for _, p := range products {
		row := map[string]any{"product_id": p.ProductID, "action_price": MoneyRUB(p.ActionPrice)}
		if p.Stock > 0 {
			row["stock"] = strconv.FormatFloat(p.Stock, 'f', -1, 64)
		}
		rows = append(rows, row)
	}
	return rows
}

// ActionUpdateResult — разобранный ответ /v1/actions/products/update.
type ActionUpdateResult struct {
	Active      []int64
	Deactivated []int64
	// Rejected — товары, которые Ozon не принял, с причиной.
	Rejected map[int64]string
	// Warnings — товары, которые Ozon вывел из акции, с причиной.
	Warnings map[int64]string
}

// ParseActionUpdate разбирает ответ /v1/actions/products/update (и, на
// всякий случай, старый вид с обёрткой result и product_ids).
func ParseActionUpdate(raw []byte) ActionUpdateResult {
	type reason struct {
		ProductID flexID `json:"product_id"`
		Reason    string `json:"reason"`
	}
	type body struct {
		Active      []flexID `json:"active_product_ids"`
		ProductIDs  []flexID `json:"product_ids"`
		Deactivated []flexID `json:"deactivated_product_ids"`
		Rejected    []reason `json:"rejected"`
		Warnings    []reason `json:"warnings"`
	}
	var resp struct {
		body
		Result *body `json:"result"`
	}
	res := ActionUpdateResult{Rejected: map[int64]string{}, Warnings: map[int64]string{}}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return res
	}
	b := resp.body
	if resp.Result != nil {
		b = *resp.Result
	}
	res.Active = flexIDs(append(b.Active, b.ProductIDs...))
	res.Deactivated = flexIDs(b.Deactivated)
	for _, r := range b.Rejected {
		res.Rejected[int64(r.ProductID)] = r.Reason
	}
	for _, r := range b.Warnings {
		res.Warnings[int64(r.ProductID)] = r.Reason
	}
	return res
}

// LeavePrice — акционная цена, которая выводит товар из бустинга:
// строго выше лимита акции, целыми рублями.
func LeavePrice(limit float64) float64 { return math.Floor(limit) + 1 }

// LeaveResult — итог вывода товаров из акции.
type LeaveResult struct {
	ActionID int64
	Left     []int64
	// Failed — товары, которые вывести не удалось, с причиной.
	Failed map[int64]string
}

// Summary — короткий отчёт для человека.
func (r LeaveResult) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Акция %d: выведено %d", r.ActionID, len(r.Left))
	if len(r.Left) > 0 {
		ids := make([]string, 0, len(r.Left))
		for _, id := range r.Left {
			ids = append(ids, strconv.FormatInt(id, 10))
		}
		b.WriteString(" (" + strings.Join(ids, ", ") + ")")
	}
	if len(r.Failed) > 0 {
		ids := make([]int64, 0, len(r.Failed))
		for id := range r.Failed {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		fmt.Fprintf(&b, ", не удалось %d:", len(ids))
		for _, id := range ids {
			fmt.Fprintf(&b, "\n  %-12d %s", id, r.Failed[id])
		}
	}
	return b.String()
}

// JSON — итог в прежней форме ответа deactivate
// ({"result":{"product_ids","rejected"}}), чтобы клиенты, разбиравшие
// старый метод, работали без изменений.
func (r LeaveResult) JSON() string {
	type rej struct {
		ProductID int64  `json:"product_id"`
		Reason    string `json:"reason"`
	}
	var out struct {
		Result struct {
			ProductIDs []int64 `json:"product_ids"`
			Rejected   []rej   `json:"rejected"`
		} `json:"result"`
	}
	out.Result.ProductIDs = append([]int64{}, r.Left...)
	out.Result.Rejected = []rej{}
	ids := make([]int64, 0, len(r.Failed))
	for id := range r.Failed {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		out.Result.Rejected = append(out.Result.Rejected, rej{id, r.Failed[id]})
	}
	raw, _ := json.Marshal(out)
	return string(raw)
}

// LeaveAction выводит товары из акции так, как это устроено после
// перехода Ozon на единую предельную цену (19.10.2026).
//
// Единого метода больше нет. Из «Эластичного» и «Максимального
// бустинга» и «Скидки на сток» товар выводится через
// /v1/actions/products/update ценой выше лимита акции (max_action_price
// из /v2/actions/products). Из «Промокодов» — /v2/actions/products/
// deactivate. Тип акции в ответах товаров не виден, поэтому сначала
// пробуем цену выше лимита, а то, что Ozon так не вывел, — v2.
func LeaveAction(ctx context.Context, c *Client, actionID int64, productIDs []int64) (LeaveResult, error) {
	return LeaveActionWithLimits(ctx, c, actionID, productIDs, nil)
}

// LeaveActionWithLimits — LeaveAction с известными заранее лимитами
// акции по товарам (product_id → max_action_price). Ozon отдаёт по
// некоторым акциям 404 «Resource not found» на /v2/actions/products, и
// без лимита вывести товар из бустинга нечем; лимит из прошлой сверки
// клиента выручает. Лимит из ответа Ozon, если он есть, главнее.
func LeaveActionWithLimits(ctx context.Context, c *Client, actionID int64, productIDs []int64, limits map[int64]float64) (LeaveResult, error) {
	res := LeaveResult{ActionID: actionID, Failed: map[int64]string{}}
	want := make(map[int64]bool, len(productIDs))
	for _, id := range productIDs {
		want[id] = true
	}
	left := map[int64]bool{}

	participants, perr := c.ActionProductsFor(ctx, PathActionProducts, actionID, want, 10)
	var rows []map[string]any
	noLimit := map[int64]bool{}
	for _, id := range productIDs {
		limit := limits[id]
		if p, ok := participants[id]; ok && p.MaxActionPrice > 0 {
			limit = float64(p.MaxActionPrice)
		}
		if limit <= 0 {
			noLimit[id] = true
			continue
		}
		rows = append(rows, map[string]any{"product_id": id, "action_price": MoneyRUB(LeavePrice(limit))})
	}
	var updErr error
	if len(rows) > 0 {
		raw, err := c.Call(ctx, PathActionUpdate, map[string]any{"action_id": actionID, "products": rows})
		if err != nil {
			updErr = err
		} else {
			upd := ParseActionUpdate(raw)
			for _, id := range upd.Deactivated {
				left[id] = true
			}
			for id := range upd.Warnings {
				if want[id] {
					left[id] = true
				}
			}
			for id, reason := range upd.Rejected {
				if !left[id] {
					res.Failed[id] = reason
				}
			}
		}
	}

	var rest []string
	for _, id := range productIDs {
		if !left[id] {
			rest = append(rest, strconv.FormatInt(id, 10))
		}
	}
	if len(rest) > 0 {
		raw, err := c.Call(ctx, PathActionDeactivate, map[string]any{"action_id": actionID, "product_ids": rest})
		if err == nil {
			var resp struct {
				ProductIDs []flexID `json:"product_ids"`
				Result     struct {
					ProductIDs []flexID `json:"product_ids"`
				} `json:"result"`
			}
			if json.Unmarshal(raw, &resp) == nil {
				for _, id := range flexIDs(append(resp.ProductIDs, resp.Result.ProductIDs...)) {
					left[id] = true
				}
			}
		}
		for _, id := range productIDs {
			if left[id] {
				delete(res.Failed, id)
				continue
			}
			if _, has := res.Failed[id]; has {
				continue
			}
			var why []string
			if updErr != nil {
				why = append(why, "цена выше лимита: "+updErr.Error())
			}
			if noLimit[id] {
				if perr != nil {
					why = append(why, "лимит акции не прочитать: "+brief(perr))
				} else {
					why = append(why, "Ozon не отдал лимит акции для товара — вывести ценой выше лимита нечем")
				}
			}
			if err != nil {
				why = append(why, "v2 deactivate (только «Промокоды»): "+brief(err))
			}
			if len(why) == 0 {
				why = append(why, "Ozon не подтвердил вывод товара из акции")
			}
			res.Failed[id] = strings.Join(why, "; ")
		}
	}

	for _, id := range productIDs {
		if left[id] {
			res.Left = append(res.Left, id)
		}
	}
	return res, nil
}

// brief — ошибка Ozon без подсказки клиента: подсказки к 404/400 общие
// («метод не найден») и здесь только путают.
func brief(err error) string {
	var api *APIError
	if errors.As(err, &api) {
		return fmt.Sprintf("HTTP %d %s", api.StatusCode, strings.TrimSpace(api.Message))
	}
	return err.Error()
}
