package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MainAlexStark/ozon-seller-mcp/internal/mcp"
	"github.com/MainAlexStark/ozon-seller-mcp/ozon"
)

// RegisterPromotions добавляет акции Ozon.
//
// Акция — единственное место в кабинете, где цену предлагает не
// продавец, а площадка: Ozon сам зовёт товар в распродажу и сам
// называет потолок цены, выше которого в неё не пустят. Отсюда
// главное свойство раздела: решение здесь всегда об одном и том же
// компромиссе — показы и место в поиске против маржи, — и принимается
// оно по числам, которые до сих пор были видны только глазами
// в кабинете.
//
// Поэтому у добавления в акцию та же страховка, что у смены цены,
// и по той же причине. Акционная цена — это цена: ошибка в её порядке
// величины стоит ровно столько же, но выглядит безобиднее, потому что
// «в акции же должно быть дёшево». Разница лишь в том, что здесь
// известен ещё и потолок Ozon, и его нарушение ловится до отправки.
func (r *Registry) RegisterPromotions() {
	r.Add(Spec{
		Name: "ozon_actions_list",
		Path: ozon.PathActionsList,
		Get:  true,
		Desc: "Акции, доступные вашему магазину: тип, сроки, размер скидки, сколько товаров доступно и сколько уже участвует. " +
			"Отправная точка раздела — action_id отсюда нужен всем остальным инструментам акций.",
		Schema: schema(obj{}),
	})

	r.Add(Spec{
		Name: "ozon_action_candidates",
		Path: ozon.PathActionCandidates,
		Desc: "Товары, которые можно добавить в акцию, с текущей ценой и потолком акционной цены (max_action_price). " +
			"Это главные числа для решения: разница между ценой и потолком показывает, во сколько обойдётся участие, " +
			"ещё до того, как что-то менять.",
		Schema: schema(obj{
			"action_id": num("Идентификатор акции из ozon_actions_list"),
			"limit":     obj{"type": "integer", "default": ozon.ActionProductsPageSize, "maximum": 1000},
			"offset":    num("Смещение для постраничного обхода"),
		}, "action_id"),
		Build: func(a map[string]any) (any, error) {
			return withLimit(a, ozon.ActionProductsPageSize), nil
		},
	})

	r.Add(Spec{
		Name: "ozon_action_products",
		Path: ozon.PathActionProducts,
		Desc: "Товары, уже участвующие в акции, с действующей акционной ценой. " +
			"Нужен, чтобы понять, что происходит с маржой прямо сейчас, и найти товары, которые пора вывести.",
		Schema: schema(obj{
			"action_id": num("Идентификатор акции из ozon_actions_list"),
			"limit":     obj{"type": "integer", "default": ozon.ActionProductsPageSize, "maximum": 1000},
			"offset":    num("Смещение для постраничного обхода"),
		}, "action_id"),
		Build: func(a map[string]any) (any, error) {
			return withLimit(a, ozon.ActionProductsPageSize), nil
		},
	})

	r.Add(Spec{
		Name: "ozon_action_products_deactivate",
		Path: ozon.PathActionDeactivate,
		Desc: "Вывести товары из акции. ИЗМЕНЯЕТ ДАННЫЕ: цена вернётся к обычной, товар потеряет место в подборке акции. " +
			"Обратная операция — ozon_action_products_activate.",
		Write: true,
		Schema: schema(obj{
			"action_id":   num("Идентификатор акции"),
			"product_ids": arr(obj{"type": "integer"}, "ID товаров, которые нужно вывести"),
		}, "action_id", "product_ids"),
		Batch: func(a map[string]any) int {
			ids, _ := a["product_ids"].([]any)
			return len(ids)
		},
		Build: func(a map[string]any) (any, error) {
			id, ok := toInt(a["action_id"])
			if !ok {
				return nil, fmt.Errorf("нужен action_id — его показывает ozon_actions_list")
			}
			ids, err := intList(a["product_ids"])
			if err != nil {
				return nil, fmt.Errorf("product_ids: %w", err)
			}
			return obj{"action_id": id, "product_ids": ids}, nil
		},
	})

	// --- Добавление в акцию: собственный обработчик из-за страховки ---

	r.AddCustom(mcp.Tool{
		Name: "ozon_action_products_activate",
		Description: "Добавить товары в акцию по указанной цене. ИЗМЕНЯЕТ ЦЕНЫ В ЖИВОМ МАГАЗИНЕ. " +
			"Перед отправкой каждая акционная цена сверяется с текущей ценой товара и с потолком Ozon: " +
			"цена выше потолка отклоняется сразу, а скидка глубже порога (по умолчанию 30 %) " +
			"требует confirm_large_change.",
		InputSchema: schema(obj{
			"action_id": num("Идентификатор акции из ozon_actions_list"),
			"products": arr(schema(obj{
				"product_id":   num("ID товара"),
				"action_price": num("Цена товара в акции"),
				"stock":        num("Число единиц для акций типа «Скидка на сток»"),
			}, "product_id", "action_price"), "Товары и их акционные цены"),
			"confirm_large_change": boolean("Подтвердить глубокую скидку. Ставьте только когда цену проверил человек."),
		}, "action_id", "products"),
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var a struct {
				ActionID int64                `json:"action_id"`
				Products []ozon.ActionProduct `json:"products"`
				Confirm  bool                 `json:"confirm_large_change"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return "", fmt.Errorf("не разобрать аргументы: %w", err)
			}

			safety := r.safetyFor(ctx)
			if err := safety.CheckWrite("ozon_action_products_activate"); err != nil {
				return "", err
			}
			if err := safety.CheckBatchSize(len(a.Products)); err != nil {
				return "", err
			}
			if a.ActionID <= 0 {
				return "", fmt.Errorf("нужен action_id — его показывает ozon_actions_list")
			}
			if len(a.Products) == 0 {
				return "", fmt.Errorf("пустой список товаров")
			}

			// Формат проверяем до всякого обращения к сети.
			for _, p := range a.Products {
				if err := p.Validate(); err != nil {
					return "", err
				}
			}

			known, warn := r.actionPrices(ctx, a.ActionID, a.Products)

			// Потолок Ozon — не предмет обсуждения: цена выше него
			// не пройдёт всё равно, и отказ придёт списком причин
			// по каждому товару, где «цена слишком высокая» теряется
			// среди прочих.
			if err := checkActionCeiling(a.Products, known); err != nil {
				return "", err
			}
			if err := safety.CheckPriceChanges(actionPriceChanges(a.Products, known), a.Confirm); err != nil {
				return "", err
			}

			resp, err := r.client.Call(ctx, ozon.PathActionActivate, obj{
				"action_id": a.ActionID,
				"products":  a.Products,
			})
			if err != nil {
				return "", decorate(err)
			}

			body := r.format(ctx, resp)
			if warn != "" {
				body = warn + "\n\n" + body
			}
			// Ответ Ozon делится на принятые и отклонённые товары, и
			// отклонённые легко пропустить: вызов при этом успешен.
			if note := summarizeActivation(resp); note != "" {
				body = note + "\n\n" + body
			}
			return body, nil
		},
	})
}

// maxActionPages — сколько страниц списка товаров акции обходить
// в поисках нужных. Сто товаров на страницу: пяти страниц хватает
// на витрину, ради которой вообще стоит городить автоматизацию.
const maxActionPages = 5

// actionPrices собирает текущие цены и потолки по товарам вызова.
//
// Как и при смене цен, неудача чтения НЕ блокирует запись: временный
// сбой не должен парализовать работу. Но в ответ уходит предупреждение
// — молча пропущенная страховка хуже отсутствующей, потому что
// выглядит как сработавшая.
func (r *Registry) actionPrices(ctx context.Context, actionID int64, products []ozon.ActionProduct) (map[int64]ozon.ActionCandidate, string) {
	want := make(map[int64]bool, len(products))
	for _, p := range products {
		want[p.ProductID] = true
	}

	known, err := r.client.ActionProductsFor(ctx, ozon.PathActionCandidates, actionID, want, maxActionPages)
	if err != nil {
		return known, fmt.Sprintf(
			"ПРЕДУПРЕЖДЕНИЕ: не удалось прочитать кандидатов акции (%v), "+
				"страховка от опечатки в цене не сработала. Проверьте результат в кабинете.", err)
	}

	// Товар, уже участвующий в акции, среди кандидатов не значится:
	// у Ozon это разные списки. Смена цены для такого товара — самый
	// обычный случай, и оставлять его без страховки нельзя.
	if len(known) < len(want) {
		for id := range known {
			delete(want, id)
		}
		participating, err := r.client.ActionProductsFor(ctx, ozon.PathActionProducts, actionID, want, maxActionPages)
		if err == nil {
			for id, product := range participating {
				known[id] = product
			}
		}
	}

	if len(known) < len(products) {
		var missing []string
		for _, p := range products {
			if _, ok := known[p.ProductID]; !ok {
				missing = append(missing, fmt.Sprintf("%d", p.ProductID))
			}
		}
		return known, fmt.Sprintf(
			"ПРЕДУПРЕЖДЕНИЕ: товары %s не найдены ни среди кандидатов акции, ни среди участников — "+
				"для них потолок цены неизвестен и страховка не сработала. "+
				"Обычно это значит, что товар в эту акцию не приглашён и Ozon его отклонит.",
			strings.Join(missing, ", "))
	}
	return known, ""
}

// checkActionCeiling отклоняет цены выше потолка акции.
func checkActionCeiling(products []ozon.ActionProduct, known map[int64]ozon.ActionCandidate) error {
	var over []string
	for _, p := range products {
		candidate, ok := known[p.ProductID]
		if !ok || candidate.MaxActionPrice <= 0 {
			continue
		}
		if p.ActionPrice > candidate.MaxActionPrice {
			over = append(over, fmt.Sprintf("  товар %-12d %.2f > потолка %.2f",
				p.ProductID, p.ActionPrice, candidate.MaxActionPrice))
		}
	}
	if len(over) == 0 {
		return nil
	}

	return fmt.Errorf(
		"Цена выше потолка акции — Ozon такие товары не примет:\n\n%s\n\n"+
			"Потолок задаёт площадка, обсуждению он не подлежит. "+
			"Либо снизьте цену до него, либо не добавляйте товар в эту акцию: "+
			"участие ниже себестоимости — не сделка, а убыток с показами.",
		strings.Join(over, "\n"))
}

// actionPriceChanges переводит добавление в акцию в изменения цены —
// в тот же вид, в котором страховка проверяет обычную смену цен.
func actionPriceChanges(products []ozon.ActionProduct, known map[int64]ozon.ActionCandidate) []PriceChange {
	changes := make([]PriceChange, 0, len(products))
	for _, p := range products {
		candidate, ok := known[p.ProductID]
		if !ok {
			continue
		}
		changes = append(changes, PriceChange{
			Item: fmt.Sprintf("товар %d", p.ProductID),
			Old:  candidate.Price,
			New:  p.ActionPrice,
		})
	}
	return changes
}

// summarizeActivation выносит наверх товары, которые Ozon не принял.
//
// Вызов при этом успешен, а отклонённые лежат отдельным списком
// с причинами — и теряются при беглом чтении ответа ровно так же,
// как ошибки валидации импорта.
func summarizeActivation(raw json.RawMessage) string {
	var resp struct {
		Result struct {
			ProductIDs []int64 `json:"product_ids"`
			Rejected   []struct {
				ProductID int64  `json:"product_id"`
				Reason    string `json:"reason"`
			} `json:"rejected"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return ""
	}
	if len(resp.Result.Rejected) == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "ВНИМАНИЕ: Ozon принял %d товаров, отклонил %d:\n",
		len(resp.Result.ProductIDs), len(resp.Result.Rejected))
	for _, item := range resp.Result.Rejected {
		fmt.Fprintf(&b, "  %-12d %s\n", item.ProductID, item.Reason)
	}
	return strings.TrimRight(b.String(), "\n")
}
