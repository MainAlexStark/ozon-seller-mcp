// Команда ozon-seller-mcp — MCP-сервер для Ozon Seller API.
//
// Два транспорта, одна реализация протокола:
//
//   - stdio (по умолчанию) — Claude запускает сервер как подпроцесс на
//     той же машине. Права задаются переменной OZON_ALLOW_WRITES.
//
//   - Streamable HTTP (--http) — сервер живёт на VPS, к нему подключаются
//     все устройства. Права определяются токеном: читающий или пишущий.
//
// Переменные окружения:
//
//	OZON_CLIENT_ID           обязательно
//	OZON_API_KEY             обязательно
//	OZON_ALLOW_WRITES        stdio: true разрешает изменение данных
//	OZON_HTTP_ADDR           сетевой режим: адрес прослушивания
//	OZON_TOKEN_READ          сетевой режим: токен только на чтение
//	OZON_TOKEN_WRITE         сетевой режим: токен на чтение и запись
//	OZON_ALLOWED_ORIGINS     сетевой режим: разрешённые Origin через запятую
//	OZON_MAX_PRICE_DELTA     порог смены цены в процентах (30)
//	OZON_MAX_ITEMS_PER_WRITE позиций за один вызов записи (100)
//	OZON_MAX_RESPONSE_BYTES  потолок размера ответа (120000)
//	OZON_BASE_URL            подмена хоста Ozon для тестов
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/MainAlexStark/ozon-seller-mcp/internal/httpx"
	"github.com/MainAlexStark/ozon-seller-mcp/internal/mcp"
	"github.com/MainAlexStark/ozon-seller-mcp/internal/tools"
	"github.com/MainAlexStark/ozon-seller-mcp/ozon"
)

// version подменяется при сборке: -ldflags "-X main.version=v0.3.0".
var version = "dev"

func main() {
	var (
		httpAddr    = flag.String("http", "", "поднять сетевой сервер на указанном адресе, например :8571")
		check       = flag.Bool("check", false, "проверить ключи и выйти")
		genToken    = flag.Bool("gen-token", false, "сгенерировать токен доступа и выйти")
		showVersion = flag.Bool("version", false, "показать версию")
	)
	flag.Parse()

	switch {
	case *showVersion:
		fmt.Println("ozon-seller-mcp", version)
		return
	case *genToken:
		printNewToken()
		return
	}

	clientID := os.Getenv("OZON_CLIENT_ID")
	apiKey := os.Getenv("OZON_API_KEY")
	if clientID == "" || apiKey == "" {
		fatal("нужны переменные окружения OZON_CLIENT_ID и OZON_API_KEY.\n" +
			"Ключ выпускается в кабинете продавца: Настройки → API-ключи.")
	}

	var opts []ozon.Option
	if base := os.Getenv("OZON_BASE_URL"); base != "" {
		opts = append(opts, ozon.WithBaseURL(base))
	}
	if proxy := os.Getenv("OZON_PROXY"); proxy != "" {
		opts = append(opts, ozon.WithProxy(proxy))
	}
	client := ozon.New(clientID, apiKey, opts...)

	// Неверный адрес прокси — остановка, а не работа напрямую: молча
	// пойти в обход того, что человек просил проксировать, хуже отказа.
	if err := client.ProxyError(); err != nil {
		fatal(err.Error())
	}

	safety := tools.DefaultSafety()
	safety.MaxPriceDeltaPct = envFloat("OZON_MAX_PRICE_DELTA", safety.MaxPriceDeltaPct)
	safety.MaxItemsPerWrite = envInt("OZON_MAX_ITEMS_PER_WRITE", safety.MaxItemsPerWrite)
	safety.MaxResponseBytes = envInt("OZON_MAX_RESPONSE_BYTES", safety.MaxResponseBytes)

	// В сетевом режиме режим доступа определяется токеном запроса,
	// поэтому OZON_ALLOW_WRITES здесь ни на что не влияет и читается
	// только для stdio.
	if envBool("OZON_ALLOW_WRITES") {
		safety.Mode = tools.ModeWrite
	}

	srv := mcp.NewServer("ozon-seller-mcp", version)
	reg := tools.NewRegistry(client, safety, srv)
	reg.RegisterCatalog()
	reg.RegisterPricing()
	reg.RegisterAnalytics()
	reg.RegisterDiagnostics()

	if *check {
		runCheck(client, srv, safety)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr := *httpAddr
	if addr == "" {
		addr = os.Getenv("OZON_HTTP_ADDR")
	}
	if addr != "" {
		runHTTP(ctx, srv, safety, addr)
		return
	}

	runStdio(ctx, srv, safety)
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

// runHTTP — сетевой режим: сервер доступен всем устройствам.
func runHTTP(ctx context.Context, srv *mcp.Server, safety tools.Safety, addr string) {
	auth, err := httpx.NewAuth(os.Getenv("OZON_TOKEN_READ"), os.Getenv("OZON_TOKEN_WRITE"))
	if err != nil {
		fatal(err.Error())
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	var origins []string
	if v := os.Getenv("OZON_ALLOWED_ORIGINS"); v != "" {
		for _, o := range strings.Split(v, ",") {
			if o = strings.TrimSpace(o); o != "" {
				origins = append(origins, o)
			}
		}
	}

	http := httpx.NewServer(srv, auth, httpx.Config{
		Addr:           addr,
		AllowedOrigins: origins,
		Safety:         safety,
		Logger:         logger,
	})

	logger.Info("сетевой сервер запущен",
		"addr", addr,
		"tools", len(srv.ToolNames()),
		"read_token", os.Getenv("OZON_TOKEN_READ") != "",
		"write_token", os.Getenv("OZON_TOKEN_WRITE") != "")

	if os.Getenv("OZON_TOKEN_WRITE") == "" {
		logger.Info("пишущий токен не задан: через сеть магазин изменить нельзя")
	}

	if err := http.ListenAndServe(ctx); err != nil {
		fatal(err.Error())
	}
}

func printNewToken() {
	token, err := httpx.GenerateToken()
	if err != nil {
		fatal(err.Error())
	}
	fmt.Println(token)
	fmt.Fprintln(os.Stderr,
		"\nЗадайте его как OZON_TOKEN_READ (только чтение) или OZON_TOKEN_WRITE (чтение и запись).\n"+
			"Генерируйте разные токены: тогда телефон с читающим токеном физически не сможет менять цены.")
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
	fmt.Println("\nСервер готов. Локально — пропишите в Claude как stdio;")
	fmt.Println("на все устройства — поднимите с --http, см. docs/REMOTE.md.")
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
