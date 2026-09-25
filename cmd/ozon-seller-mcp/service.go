package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/MainAlexStark/ozon-seller-mcp/internal/httpx"
	"github.com/MainAlexStark/ozon-seller-mcp/internal/oauth"
	"github.com/MainAlexStark/ozon-seller-mcp/internal/pgstore"
	"github.com/MainAlexStark/ozon-seller-mcp/internal/secure"
	"github.com/MainAlexStark/ozon-seller-mcp/internal/shops"
	"github.com/MainAlexStark/ozon-seller-mcp/internal/tools"
	"github.com/MainAlexStark/ozon-seller-mcp/internal/web"
	"github.com/MainAlexStark/ozon-seller-mcp/ozon"
)

// runService — многопользовательский сервис.
func runService(ctx context.Context, safety tools.Safety, addr string) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	publicURL := strings.TrimSuffix(os.Getenv("OZON_PUBLIC_URL"), "/")
	if publicURL == "" {
		fatal("сервису нужен OZON_PUBLIC_URL — внешний адрес, например https://ozon-mcp.example.com")
	}
	if os.Getenv("OZON_CLIENT_ID") != "" || os.Getenv("OZON_API_KEY") != "" {
		// Раньше сетевой режим обслуживал магазин из окружения. Теперь
		// магазины у пользователей, и ключ в окружении — почти наверняка
		// остаток старой настройки, который молча ни на что не влияет.
		logger.Warn("OZON_CLIENT_ID/OZON_API_KEY в сервисном режиме не используются: " +
			"зарегистрируйтесь в кабинете и подключите магазин там")
	}

	cipher, err := secure.NewCipher(os.Getenv("OZON_SECRET_KEY"))
	if err != nil {
		fatal(err.Error())
	}

	db := openDB(ctx)
	defer db.Close()

	// Проверяем прокси один раз на старте, а не на первом запросе
	// первого пользователя.
	opts := ozonOptions()
	if err := ozon.New("", "", opts...).ProxyError(); err != nil {
		fatal(err.Error())
	}
	shopSvc := shops.New(db, cipher, opts...)

	resourceURL := os.Getenv("OZON_RESOURCE_URL")

	site, err := web.New(db, shopSvc, web.Config{
		PublicURL:   publicURL,
		ResourceURL: resourceURL,
		SignupOpen:  !strings.EqualFold(os.Getenv("OZON_SIGNUP"), "closed"),
		ClientIP:    httpx.ClientIP,
		Logger:      logger,
	})
	if err != nil {
		fatal(err.Error())
	}

	oauthSrv, err := oauth.New(oauth.Config{
		Issuer:      publicURL,
		ResourceURL: resourceURL,
		Store:       db.OAuth(),
		Accounts:    site,
		Logger:      logger,
	})
	if err != nil {
		fatal(err.Error())
	}

	apiTokens := func(ctx context.Context, token string) (httpx.Principal, bool) {
		t, err := db.APITokenByHash(ctx, secure.HashToken(token))
		if err != nil {
			if !errors.Is(err, pgstore.ErrNotFound) {
				logger.Error("проверка API-токена", "err", err)
			}
			return httpx.Principal{}, false
		}
		p := httpx.Principal{UserID: t.UserID, ShopID: t.ShopID, Scope: httpx.ScopeRead}
		for _, s := range t.Scopes {
			if s == "write" {
				p.Scope = httpx.ScopeWrite
			}
		}
		return p, true
	}

	usage := newUsageRecorder(ctx, db, logger)

	var origins []string
	for _, o := range strings.Split(os.Getenv("OZON_ALLOWED_ORIGINS"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			origins = append(origins, o)
		}
	}

	srv := newMCP(nil, safety)
	http, err := httpx.NewServer(srv,
		httpx.Auth{OAuth: oauthSrv, APITokens: apiTokens},
		shopSvc,
		usage,
		httpx.Config{
			Addr:           addr,
			AllowedOrigins: origins,
			Safety:         safety,
			Web:            site,
			Ready:          db.Ping,
			Logger:         logger,
		})
	if err != nil {
		fatal(err.Error())
	}

	go cleanupLoop(ctx, db, logger)

	logger.Info("сервис запущен",
		"version", version,
		"addr", addr,
		"public", publicURL,
		"resource", oauthSrv.ResourceURL(),
		"tools", len(srv.ToolNames()))

	if err := http.ListenAndServe(ctx); err != nil {
		fatal(err.Error())
	}
}

// openDB подключается к базе и применяет миграции. Миграции на старте,
// а не отдельным шагом: забытый шаг развёртывания выглядел бы как
// «сервер падает на первом запросе», а не как внятная ошибка.
func openDB(ctx context.Context) *pgstore.DB {
	url := os.Getenv("OZON_DATABASE_URL")
	if url == "" {
		fatal("нужен OZON_DATABASE_URL, например postgres://ozon:пароль@postgres:5432/ozon")
	}
	var (
		db  *pgstore.DB
		err error
	)
	// База в compose может подниматься дольше сервера: ждём до минуты,
	// прежде чем сдаться.
	for i := 0; i < 20; i++ {
		db, err = pgstore.Open(ctx, url)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			fatal(ctx.Err().Error())
		case <-time.After(3 * time.Second):
		}
	}
	if err != nil {
		fatal("база: " + err.Error())
	}
	if err := db.Migrate(ctx); err != nil {
		db.Close()
		fatal(err.Error())
	}
	return db
}

// cleanupLoop раз в час убирает истёкшие сессии, коды и токены.
func cleanupLoop(ctx context.Context, db *pgstore.DB, logger *slog.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if err := db.Cleanup(ctx); err != nil && ctx.Err() == nil {
			logger.Warn("чистка базы", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type usageEvent struct {
	user, shop int64
	tool       string
	failed     bool
}

// newUsageRecorder пишет учёт вызовов в фоне: запись в базу не должна
// задерживать ответ инструмента. Если очередь переполнена (база
// тормозит), событие теряется — учёт не стоит того, чтобы из-за него
// вставал сервис.
func newUsageRecorder(ctx context.Context, db *pgstore.DB, logger *slog.Logger) httpx.UsageFunc {
	ch := make(chan usageEvent, 1000)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case e := <-ch:
				wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				if err := db.RecordUsage(wctx, e.user, e.shop, e.tool, e.failed); err != nil {
					logger.Warn("учёт вызова", "err", err)
				}
				cancel()
			}
		}
	}()
	return func(user, shop int64, tool string, failed bool) {
		select {
		case ch <- usageEvent{user, shop, tool, failed}:
		default:
			logger.Warn("очередь учёта переполнена, вызов не учтён", "tool", tool)
		}
	}
}
