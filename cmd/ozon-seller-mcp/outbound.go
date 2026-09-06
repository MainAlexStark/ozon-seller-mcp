package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/MainAlexStark/ozon-seller-mcp/ozon"
)

// outboundIP узнаёт, с какого адреса нас видит внешний мир.
//
// Нужен ровно для одного разбирательства: «почему Ozon молчит». Если
// здесь адрес VPN-выхода, а запросы к Ozon уходят в таймаут — причина
// найдена, и дальше можно не искать.
//
// Запрос идёт через тот же транспорт, что и обращения к Ozon (то есть
// через OZON_PROXY, если он задан) — иначе показанный адрес не будет
// иметь отношения к тому, что видит Ozon, и подсказка станет вредной.
//
// Best-effort: если сервис недоступен, просто ничего не показываем.
// Диагностика не должна ронять проверку ключей.
func outboundIP(ctx context.Context, c *ozon.Client) string {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.ipify.org?format=json", nil)
	if err != nil {
		return ""
	}

	resp, err := c.HTTPClient().Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	var out struct {
		IP string `json:"ip"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ""
	}
	return strings.TrimSpace(out.IP)
}
