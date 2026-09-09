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

// globalGroup — ведро, общее для всех методов.
//
// Кроме лимитов по операциям у Ozon есть ещё и общий предел на число
// запросов в секунду: «You have reached request rate limit per second».
// Групповые вёдра его не ловят — они считают минуты, а пачка запросов,
// выпущенная разом, укладывается в минутный лимит каждой группы и всё
// равно упирается в секундный. Так ozon_api_selftest, дёргающий все
// read-методы параллельно, начал получать 429 на ровном месте.
const globalGroup = "*в секунду*"

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

		globalGroup: {Burst: 5, Period: time.Second},
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

// take пытается забрать по токену из ведра группы и общего ведра.
// Возвращает время до следующей попытки, если где-то токенов нет.
//
// Токены забираются только когда есть оба: иначе запрос, упёршийся
// в секундный лимит, тратил бы минутный запас своей группы впустую.
func (l *Limiter) take(path string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()

	groupRate, group := l.refill(groupOf(path), now)
	globalRate, global := l.refill(globalGroup, now)

	if group.tokens > 0 && global.tokens > 0 {
		group.tokens--
		global.tokens--
		return 0, true
	}

	// Ждём столько, сколько нужно тому ведру, которое опустело.
	// Если пусты оба — дольшему из двух.
	wait := time.Duration(0)
	if group.tokens == 0 {
		wait = tokenInterval(groupRate)
	}
	if global.tokens == 0 {
		wait = max(wait, tokenInterval(globalRate))
	}
	return wait, false
}

// refill пополняет ведро пропорционально прошедшему времени и отдаёт
// его вместе с ограничением группы.
func (l *Limiter) refill(group string, now time.Time) (Rate, *bucket) {
	rate, ok := l.rates[group]
	if !ok {
		rate = l.rates["other"]
	}

	b, ok := l.buckets[group]
	if !ok {
		b = &bucket{tokens: rate.Burst, last: now}
		l.buckets[group] = b
	}

	elapsed := now.Sub(b.last)
	if elapsed > 0 && rate.Period > 0 {
		refill := int(float64(rate.Burst) * (float64(elapsed) / float64(rate.Period)))
		if refill > 0 {
			b.tokens = min(b.tokens+refill, rate.Burst)
			b.last = now
		}
	}
	return rate, b
}

// tokenInterval — сколько ждать до появления одного токена.
func tokenInterval(rate Rate) time.Duration {
	return rate.Period / time.Duration(max(rate.Burst, 1))
}
