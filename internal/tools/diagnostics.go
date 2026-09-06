package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/MainAlexStark/ozon-seller-mcp/internal/mcp"
	"github.com/MainAlexStark/ozon-seller-mcp/ozon"
)

// probe — read-метод, который безопасно дёрнуть с минимальным телом.
type probe struct {
	Tool    string
	Path    string
	Payload any
}

// probes — методы для самодиагностики. Все только читают.
func probes() []probe {
	return []probe{
		// filter обязателен даже когда фильтровать нечего — см. defaults.go.
		{"ozon_product_list", ozon.PathProductList, obj{"filter": obj{"visibility": "ALL"}, "limit": 1}},
		{"ozon_prices_info", ozon.PathPricesInfo, obj{"filter": obj{"visibility": "ALL"}, "limit": 1}},
		{"ozon_stocks_info", ozon.PathStocksInfo, obj{"filter": obj{"visibility": "ALL"}, "limit": 1}},
		{"ozon_warehouse_list", ozon.PathWarehouseList, obj{}},
		{"ozon_category_tree", ozon.PathCategoryTree, obj{"language": "RU"}},
		{"ozon_finance_accrual_types", ozon.PathFinanceAccrualTypes, obj{}},
		{"ozon_postings_list", ozon.PathPostingFBSList, obj{
			"filter": obj{
				"since": time.Now().AddDate(0, 0, -7).Format(time.RFC3339),
				"to":    time.Now().Format(time.RFC3339),
			},
			"limit": 1,
		}},
	}
}

// RegisterDiagnostics добавляет инструменты состояния и самопроверки.
func (r *Registry) RegisterDiagnostics() {
	r.AddCustom(mcp.Tool{
		Name: "ozon_status",
		Description: "Состояние сервера: режим (только чтение или запись), пороги защиты, кабинет продавца. " +
			"Вызовите первым, если непонятно, почему инструмент отказывается что-то менять.",
		InputSchema: schema(obj{}),
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			safety := r.safetyFor(ctx)

			var b strings.Builder

			fmt.Fprintf(&b, "Режим этого подключения: %s\n", safety.Mode)
			if safety.Mode == ModeReadOnly {
				// Формулировка зависит от транспорта: по stdio режим
				// задаётся переменной окружения, по HTTP — тем, какой
				// токен вы прописали в этом клиенте.
				b.WriteString("                         (запись запрещена: локально — OZON_ALLOW_WRITES,\n")
				b.WriteString("                          по сети — использован токен только на чтение)\n")
			}
			fmt.Fprintf(&b, "Client-Id:               %s\n", maskID(r.client.ClientID()))
			fmt.Fprintf(&b, "Порог смены цены:        %.0f%%\n", safety.MaxPriceDeltaPct)
			fmt.Fprintf(&b, "Позиций за одну запись:  %d\n", safety.MaxItemsPerWrite)
			fmt.Fprintf(&b, "Потолок ответа:          %d байт\n", safety.MaxResponseBytes)
			fmt.Fprintf(&b, "\nИнструментов зарегистрировано: %d\n", len(r.server.ToolNames()))
			b.WriteString("\nПроверить, что ключи приняты и методы живы: ozon_api_selftest.")

			return b.String(), nil
		},
	})

	r.AddCustom(mcp.Tool{
		Name: "ozon_api_selftest",
		Description: "Проверить связь с Ozon: по одному минимальному запросу к каждому read-методу. " +
			"Показывает, какие методы отвечают, а какие отключены или требуют других прав. " +
			"Ozon регулярно отключает старые версии методов — это способ узнать об этом до того, как сломается работа.",
		InputSchema: schema(obj{}),
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			list := probes()
			results := make([]string, len(list))

			var wg sync.WaitGroup
			for i, p := range list {
				wg.Add(1)
				go func(i int, p probe) {
					defer wg.Done()
					results[i] = runProbe(ctx, r.client, p)
				}(i, p)
			}
			wg.Wait()

			var b strings.Builder
			b.WriteString("Проверка методов Ozon Seller API\n\n")
			for _, line := range results {
				b.WriteString(line)
				b.WriteString("\n")
			}
			b.WriteString("\nЕсли метод отвечает 404 — Ozon отключил эту версию. " +
				"Сверьтесь с docs.ozon.ru/api/seller и поправьте ozon/paths.go.")
			return b.String(), nil
		},
	})
}

// runProbe выполняет один пробный запрос и описывает результат строкой.
func runProbe(ctx context.Context, c *ozon.Client, p probe) string {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	start := time.Now()
	_, err := c.Call(ctx, p.Path, p.Payload)
	elapsed := time.Since(start).Round(time.Millisecond)

	if err == nil {
		return fmt.Sprintf("  OK       %-28s %-42s %s", p.Tool, p.Path, elapsed)
	}

	var apiErr *ozon.APIError
	if asAPI(err, &apiErr) {
		return fmt.Sprintf("  HTTP %-3d %-28s %-42s %s", apiErr.StatusCode, p.Tool, p.Path, apiErr.Message)
	}
	return fmt.Sprintf("  ОШИБКА   %-28s %-42s %v", p.Tool, p.Path, err)
}

// maskID показывает идентификатор, не выкладывая его целиком в чат.
func maskID(id string) string {
	if id == "" {
		return "(не задан)"
	}
	if len(id) <= 4 {
		return strings.Repeat("*", len(id))
	}
	return strings.Repeat("*", len(id)-4) + id[len(id)-4:]
}
