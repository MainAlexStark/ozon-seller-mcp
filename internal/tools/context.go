package tools

import (
	"context"

	"github.com/MainAlexStark/ozon-seller-mcp/ozon"
)

// Режим доступа — свойство ЗАПРОСА, а не процесса.
//
// Пока сервер работал по stdio, это было одно и то же: его запускал сам
// пользователь на своей машине, и переменной окружения хватало. Как
// только тот же сервер выставлен в интернет, разные обращения приходят
// с разными правами — с телефона читающим токеном, с рабочей машины
// пишущим, — и глобальная настройка перестаёт что-либо значить.
//
// Поэтому Safety кладётся в контекст запроса на входе (HTTP-транспорт
// делает это после проверки токена), а инструменты читают её оттуда.
// Настройка процесса остаётся только запасным значением для stdio.

type safetyKey struct{}

// WithSafety кладёт права запроса в контекст.
func WithSafety(ctx context.Context, s Safety) context.Context {
	return context.WithValue(ctx, safetyKey{}, s)
}

// SafetyFrom достаёт права запроса.
//
// Второе значение — были ли права заданы явно. Вызывающий код обязан
// его проверять: молча подставить разрешающие настройки, когда в
// контексте ничего нет, — ровно тот способ, которым такие проверки
// и обходят.
func SafetyFrom(ctx context.Context) (Safety, bool) {
	s, ok := ctx.Value(safetyKey{}).(Safety)
	return s, ok
}

// safetyFor возвращает права для текущего запроса: из контекста, если
// транспорт их проставил, иначе настройки процесса.
func (r *Registry) safetyFor(ctx context.Context) Safety {
	if s, ok := SafetyFrom(ctx); ok {
		return s
	}
	return r.safety
}

// Клиент Ozon — тоже свойство запроса.
//
// В stdio магазин один, и клиент реестра создаётся при старте из
// окружения. В сервисном режиме у каждого подключения свой магазин:
// транспорт после проверки токена кладёт в контекст клиента именно
// этого магазина, со своими ключами и своим лимитером (Ozon считает
// лимиты по Client-Id, и один шумный продавец не должен тормозить
// остальных).

type clientKey struct{}

// WithClient кладёт клиента Ozon запроса в контекст.
func WithClient(ctx context.Context, c *ozon.Client) context.Context {
	return context.WithValue(ctx, clientKey{}, c)
}

// clientFor возвращает клиента текущего запроса. Обёртка register
// гарантирует, что до обработчика инструмента он не доходит пустым.
func (r *Registry) clientFor(ctx context.Context) *ozon.Client {
	if c, ok := ctx.Value(clientKey{}).(*ozon.Client); ok && c != nil {
		return c
	}
	return r.client
}

// CallHook узнаёт о каждом вызове инструмента — для учёта вызовов.
type CallHook func(tool string, failed bool)

type hookKey struct{}

// WithCallHook подвешивает к запросу наблюдателя за вызовами.
func WithCallHook(ctx context.Context, h CallHook) context.Context {
	return context.WithValue(ctx, hookKey{}, h)
}
