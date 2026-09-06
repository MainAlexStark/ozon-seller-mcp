package ozon

import (
	"context"
	"strings"
	"sync"
	"time"
)

// Limiter — простой token bucket с раздельными вёдрами по группам методов.
//
// У Ozon лимиты заданы не на весь API целиком, а по операциям: импорт
// товаров, цены и остатки живут по разным правилам. Одно общее ведро
// либо душит быстрые методы, либо не спасает от 429 на медленных.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rates   map[string]Rate
}

// Rate — сколько запросов за какой период разрешено группе.
type Rate struct {
	Burst  int
	Period time.Duration
}

type bucket struct {
	tokens int
	last   time.Time
}

// DefaultRates — стартовые ограничения. ЗАНИЖЕНЫ НАМЕРЕННО: получить
// 429 дороже, чем публиковать чуть медленнее. Реальные значения см.
// в документации Ozon и уточняйте по факту.
func DefaultRates() map[string]Rate {
	return map[string]Rate{
		"product":   {Burst: 10, Period: time.Minute},
		"prices":    {Burst: 30, Period: time.Minute},
		"stocks":    {Burst: 30, Period: time.Minute},
		"posting":   {Burst: 60, Period: time.Minute},
		"analytics": {Burst: 10, Period: time.Minute}, // аналитика тяжёлая, лимиты жёстче
		"finance":   {Burst: 10, Period: time.Minute},
		"other":     {Burst: 60, Period: time.Minute},
	}
}

// NewLimiter создаёт лимитер со стартовыми ограничениями.
func NewLimiter() *Limiter {
	return &Limiter{
		buckets: make(map[string]*bucket),
		rates:   DefaultRates(),
	}
}

// groupOf относит путь метода к группе лимитов.
func groupOf(path string) string {
	// Порядок ветвей значим: /v1/analytics/stocks должен попасть
	// в "analytics", а не в "stocks" — у аналитики свои лимиты.
	switch {
	case strings.Contains(path, "/analytics/"):
		return "analytics"
	case strings.Contains(path, "/finance/"):
		return "finance"
	case strings.Contains(path, "/prices"):
		return "prices"
	case strings.Contains(path, "/stocks"):
		return "stocks"
	case strings.Contains(path, "/posting"):
		return "posting"
	case strings.Contains(path, "/product"):
		return "product"
	default:
		return "other"
	}
}

// Wait блокирует вызов, пока в ведре группы не появится токен,
// либо пока не истечёт контекст.
func (l *Limiter) Wait(ctx context.Context, path string) error {
	for {
		wait, ok := l.take(path)
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// take пытается забрать токен. Возвращает время до следующей попытки,
// если токенов нет.
func (l *Limiter) take(path string) (time.Duration, bool) {
	g := groupOf(path)

	l.mu.Lock()
	defer l.mu.Unlock()

	rate, ok := l.rates[g]
	if !ok {
		rate = l.rates["other"]
	}

	now := time.Now()
	b, ok := l.buckets[g]
	if !ok {
		b = &bucket{tokens: rate.Burst, last: now}
		l.buckets[g] = b
	}

	// Пополняем пропорционально прошедшему времени.
	elapsed := now.Sub(b.last)
	if elapsed > 0 && rate.Period > 0 {
		refill := int(float64(rate.Burst) * (float64(elapsed) / float64(rate.Period)))
		if refill > 0 {
			b.tokens = min(b.tokens+refill, rate.Burst)
			b.last = now
		}
	}

	if b.tokens > 0 {
		b.tokens--
		return 0, true
	}

	// Ждём ровно столько, сколько нужно для одного токена.
	return rate.Period / time.Duration(max(rate.Burst, 1)), false
}
