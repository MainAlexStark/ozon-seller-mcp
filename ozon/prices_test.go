package ozon

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPriceUpdatePromoFlags(t *testing.T) {
	on := true
	p := PriceUpdate{OfferID: "a", Price: "300", MinPrice: "299", MinPriceForAutoActions: &on,
		AutoActionEnabled: "DISABLED", AutoAddEnabled: "DISABLED"}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(p)
	for _, want := range []string{`"auto_add_to_ozon_actions_list_enabled":"DISABLED"`, `"min_price_for_auto_actions_enabled":true`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("нет %s в %s", want, raw)
		}
	}
	p.AutoAddEnabled = "OFF"
	if p.Validate() == nil {
		t.Error("неизвестное значение флага должно отклоняться")
	}
}
