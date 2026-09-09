package ozon

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Ozon держит не только лимиты по операциям, но и общий предел
// в секунду. Пачка параллельных запросов укладывалась в минутные
// лимиты каждой группы и всё равно ловила 429 — ozon_api_selftest
// начал спотыкаться об это, когда методов стало больше десятка.
func TestLimiterHoldsPerSecondCap(t *testing.T) {
	l := NewLimiter()
	l.rates[globalGroup] = Rate{Burst: 3, Period: time.Second}

	ctx := context.Background()
	start := time.Now()
	for i := 0; i < 5; i++ {
		// Пути из разных групп: групповые вёдра тут ни при чём.
		if err := l.Wait(ctx, "/v3/product/list"); err != nil {
			t.Fatal(err)
		}
	}

	// Три токена уходят сразу, четвёртый и пятый ждут пополнения.
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Errorf("общий лимит не сработал: пять запросов ушли за %s", elapsed)
	}
}

func TestLimiterDoesNotBurnGroupTokensWhileWaiting(t *testing.T) {
	l := NewLimiter()
	l.rates[globalGroup] = Rate{Burst: 1, Period: time.Hour}
	l.rates["product"] = Rate{Burst: 2, Period: time.Hour}

	if _, ok := l.take("/v3/product/list"); !ok {
		t.Fatal("первый запрос должен проходить")
	}
	// Общее ведро пусто: следующий запрос ждёт — но токен группы
	// при этом тратиться не должен, иначе минутный запас утекает
	// на ожидание секундного лимита.
	if _, ok := l.take("/v3/product/list"); ok {
		t.Fatal("общий лимит должен останавливать запрос")
	}
	if got := l.buckets["product"].tokens; got != 1 {
		t.Errorf("токен группы потрачен впустую: осталось %d, ожидалось 1", got)
	}
}

// 403 приходит и на «ключ не тот», и на «метод не входит в подписку».
// Советы противоположные, и раньше второй случай отправлял человека
// перевыпускать вполне рабочий ключ.
func TestForbiddenTellsSubscriptionFromBadKey(t *testing.T) {
	subscription := &APIError{
		StatusCode: 403,
		Message:    "Implementation.ReviewList: rpc error: code = PermissionDenied desc = not available with existing subscription",
	}
	if hint := subscription.Hint(); !strings.Contains(hint, "тариф") {
		t.Errorf("отказ по подписке должен говорить про тариф: %s", hint)
	}

	badKey := &APIError{StatusCode: 403, Message: "client-id or api-key is invalid"}
	if hint := badKey.Hint(); !strings.Contains(hint, "OZON_API_KEY") {
		t.Errorf("отказ по ключу должен говорить про ключ: %s", hint)
	}
}
