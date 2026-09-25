// Команда ozon-seller-mcp — MCP-сервер для Ozon Seller API.
//
// Два режима:
//
//   - stdio (по умолчанию) — Claude запускает сервер как подпроцесс на
//     той же машине. Один магазин, ключи из окружения, права задаются
//     переменной OZON_ALLOW_WRITES.
//
//   - сервис (--http) — многопользовательский сервер: регистрация,
//     кабинет, магазины пользователей в PostgreSQL, OAuth для Claude.
//     Каждое подключение привязано к одному магазину одного пользователя.
//
// Переменные окружения, stdio:
//
//	OZON_CLIENT_ID           обязательно
//	OZON_API_KEY             обязательно
//	OZON_ALLOW_WRITES        true разрешает изменение данных
//
// Переменные окружения, сервис:
//
//	OZON_DATABASE_URL        обязательно: postgres://user:pass@host:5432/db
//	OZON_SECRET_KEY          обязательно: мастер-ключ шифрования ключей магазинов (--gen-secret)
//	OZON_PUBLIC_URL          обязательно: внешний адрес, https://ozon-mcp.example.com
//	OZON_HTTP_ADDR           адрес прослушивания, если не задан --http
//	OZON_RESOURCE_URL        адрес MCP, если отличается от <public>/mcp
//	OZON_SIGNUP              open (по умолчанию) или closed
//	OZON_ALLOWED_ORIGINS     разрешённые Origin для /mcp через запятую
//
// Общие:
//
//	OZON_MAX_PRICE_DELTA     порог смены цены в процентах (30)
//	OZON_MAX_ITEMS_PER_WRITE позиций за один вызов записи (100)
//	OZON_MAX_RESPONSE_BYTES  потолок размера ответа (40000)
//	OZON_PROXY               прокси для запросов к Ozon
//	OZON_BASE_URL            подмена хоста Ozon для тестов
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/MainAlexStark/ozon-seller-mcp/internal/mcp"
	"github.com/MainAlexStark/ozon-seller-mcp/internal/tools"
	"github.com/MainAlexStark/ozon-seller-mcp/ozon"
)

// version подменяется при сборке: -ldflags "-X main.version=v0.3.0".
var version = "dev"

func main() {
	var (
		httpAddr    = flag.String("http", "", "поднять сервис на указанном адресе, например :8571")
		check       = flag.Bool("check", false, "проверить ключи из окружения и выйти")
		genSecret   = flag.Bool("gen-secret", false, "сгенерировать OZON_SECRET_KEY")
		migrate     = flag.Bool("migrate", false, "применить миграции базы и выйти")
		users       = flag.Bool("users", false, "показать пользователей сервиса")
		disable     = flag.String("disable", "", "заблокировать пользователя по почте")
		enable      = flag.String("enable", "", "разблокировать пользователя по почте")
		setPassword = flag.String("set-password", "", "задать пароль пользователю (пароль читается со stdin)")
		showVersion = flag.Bool("version", false, "показать версию")
	)
	flag.Parse()

	switch {
	case *showVersion:
		fmt.Println("ozon-seller-mcp", version)
		return
	case *genSecret:
		printSecretKey()
		return
	case *migrate:
		runMigrate()
		return
	case *users:
		listUsers()
		return
	case *disable != "":
		setDisabled(*disable, true)
		return
	case *enable != "":
		setDisabled(*enable, false)
		return
	case *setPassword != "":
		resetPassword(*setPassword)
		return
	}

	safety := tools.DefaultSafety()
	safety.MaxPriceDeltaPct = envFloat("OZON_MAX_PRICE_DELTA", safety.MaxPriceDeltaPct)
	safety.MaxItemsPerWrite = envInt("OZON_MAX_ITEMS_PER_WRITE", safety.MaxItemsPerWrite)
	safety.MaxResponseBytes = envInt("OZON_MAX_RESPONSE_BYTES", safety.MaxResponseBytes)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr := *httpAddr
	if addr == "" {
		addr = os.Getenv("OZON_HTTP_ADDR")
	}
	if addr != "" {
		runService(ctx, safety, addr)
		return
	}

	client := envClient()

	// В stdio режим доступа — свойство процесса: сервер запускает сам
	// владелец на своей машине.
	if envBool("OZON_ALLOW_WRITES") {
		safety.Mode = tools.ModeWrite
	}

	srv := newMCP(client, safety)

	if *check {
		runCheck(client, srv, safety)
		return
	}

	runStdio(ctx, srv, safety)
}

// ozonOptions — настройки, общие для всех клиентов Ozon процесса.
func ozonOptions() []ozon.Option {
	var opts []ozon.Option
	if base := os.Getenv("OZON_BASE_URL"); base != "" {
		opts = append(opts, ozon.WithBaseURL(base))
	}
	if proxy := os.Getenv("OZON_PROXY"); proxy != "" {
		opts = append(opts, ozon.WithProxy(proxy))
	}
	return opts
}

// envClient — клиент единственного магазина stdio-режима.
func envClient() *ozon.Client {
	clientID := os.Getenv("OZON_CLIENT_ID")
	apiKey := os.Getenv("OZON_API_KEY")
	if clientID == "" || apiKey == "" {
		fatal("нужны переменные окружения OZON_CLIENT_ID и OZON_API_KEY.\n" +
			"Ключ выпускается в кабинете продавца: Настройки → API-ключи.")
	}
	client := ozon.New(clientID, apiKey, ozonOptions()...)

	// Неверный адрес прокси — остановка, а не работа напрямую: молча
	// пойти в обход того, что человек просил проксировать, хуже отказа.
	if err := client.ProxyError(); err != nil {
		fatal(err.Error())
	}
	return client
}

// newMCP собирает сервер со всеми инструментами. client == nil — сервис:
// клиент каждого запроса приходит из контекста.
func newMCP(client *ozon.Client, safety tools.Safety) *mcp.Server {
	srv := mcp.NewServer("ozon-seller-mcp", version)
	reg := tools.NewRegistry(client, safety, srv)
	reg.RegisterCatalog()
	reg.RegisterPricing()
	reg.RegisterAnalytics()
	reg.RegisterFinance()
	reg.RegisterFBO()
	reg.RegisterReturns()
	reg.RegisterQuestions()
	reg.RegisterCertificates()
	reg.RegisterPromotions()
	reg.RegisterPricingStrategies()
	reg.RegisterDiagnostics()
	return srv
}

// runStdio — локальный режим: Claude запускает сервер подпроцессом.
func runStdio(ctx context.Context, srv *mcp.Server, safety tools.Safety) {
	// Диагностика уходит в stderr: stdout занят протоколом.
	fmt.Fprintf(os.Stderr, "ozon-seller-mcp %s: stdio, %d инструментов, режим %s\n",
		version, len(srv.ToolNames()), safety.Mode)

	if err := srv.Serve(ctx, os.Stdin, os.Stdout); err != nil {
		fatal(err.Error())
	}
}

// runCheck проверяет ключи до того, как сервер пропишут в Claude.
func runCheck(client *ozon.Client, srv *mcp.Server, safety tools.Safety) {
	fmt.Printf("ozon-seller-mcp %s\n", version)
	fmt.Printf("Инструментов: %d\n", len(srv.ToolNames()))
	fmt.Printf("Режим stdio:  %s\n", safety.Mode)
	if px := client.Proxy(); px != "" {
		fmt.Printf("Прокси:       %s\n", px)
	} else {
		fmt.Printf("Прокси:       не задан (прямое соединение)\n")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	// С какого адреса нас видит внешний мир. При разборе «почему Ozon
	// не отвечает» это первый вопрос: если тут адрес VPN-выхода, а Ozon
	// молчит по таймауту, дальше можно не искать.
	if ip := outboundIP(ctx, client); ip != "" {
		fmt.Printf("Внешний IP:   %s\n", ip)
	}
	fmt.Println()

	fmt.Print("Проверяю ключи… ")
	// filter обязателен даже когда фильтровать нечего: без него Ozon
	// отвечает 400 ещё до проверки ключа, и понять, приняты ли ключи,
	// становится невозможно.
	_, err := client.Call(ctx, ozon.PathProductList, map[string]any{
		"filter": map[string]any{"visibility": "ALL"},
		"limit":  1,
	})
	if err != nil {
		fmt.Println("не прошло")

		var netErr *ozon.NetworkError
		if netErrorsAs(err, &netErr) {
			fmt.Printf("\n%s\n\n%s\n", netErr.Error(), netErr.Hint())
			os.Exit(1)
		}

		var apiErr *ozon.APIError
		if errorsAs(err, &apiErr) {
			fmt.Printf("\n%s\n", apiErr.Error())
			if hint := apiErr.Hint(); hint != "" {
				fmt.Printf("\n%s\n", hint)
			}
		} else {
			fmt.Printf("\n%v\n", err)
		}
		os.Exit(1)
	}

	fmt.Println("ключи приняты")
	fmt.Println("\nСервер готов. Пропишите его в Claude как stdio-сервер;")
	fmt.Println("сервис для многих пользователей — см. docs/REMOTE.md.")
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "ozon-seller-mcp:", msg)
	os.Exit(1)
}

func envBool(key string) bool {
	switch os.Getenv(key) {
	case "1", "true", "TRUE", "yes":
		return true
	}
	return false
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}
