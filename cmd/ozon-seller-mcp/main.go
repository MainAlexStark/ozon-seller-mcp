// Команда ozon-seller-mcp — MCP-сервер для Ozon Seller API.
//
// Запускается Claude по stdio. Ключи и режим задаются переменными
// окружения — так доступ на запись включает человек в конфигурации,
// а не удачная формулировка в переписке.
//
//	OZON_CLIENT_ID          обязательно
//	OZON_API_KEY            обязательно
//	OZON_ALLOW_WRITES       true разрешает изменение данных (по умолчанию нет)
//	OZON_MAX_PRICE_DELTA    порог смены цены в процентах (по умолчанию 30)
//	OZON_MAX_ITEMS_PER_WRITE позиций за один вызов записи (по умолчанию 100)
//	OZON_MAX_RESPONSE_BYTES  потолок размера ответа (по умолчанию 120000)
//	OZON_BASE_URL            подмена хоста для тестов
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

// version подменяется при сборке: -ldflags "-X main.version=v0.2.0".
var version = "dev"

func main() {
	check := flag.Bool("check", false, "проверить ключи и доступность методов, затем выйти")
	showVersion := flag.Bool("version", false, "показать версию")
	flag.Parse()

	if *showVersion {
		fmt.Println("ozon-seller-mcp", version)
		return
	}

	clientID := os.Getenv("OZON_CLIENT_ID")
	apiKey := os.Getenv("OZON_API_KEY")
	if clientID == "" || apiKey == "" {
		fatal("нужны переменные окружения OZON_CLIENT_ID и OZON_API_KEY.\n" +
			"Ключ выпускается в кабинете продавца: Настройки → API-ключи.")
	}

	opts := []ozon.Option{}
	if base := os.Getenv("OZON_BASE_URL"); base != "" {
		opts = append(opts, ozon.WithBaseURL(base))
	}
	client := ozon.New(clientID, apiKey, opts...)

	safety := tools.DefaultSafety()
	if envBool("OZON_ALLOW_WRITES") {
		safety.Mode = tools.ModeWrite
	}
	safety.MaxPriceDeltaPct = envFloat("OZON_MAX_PRICE_DELTA", safety.MaxPriceDeltaPct)
	safety.MaxItemsPerWrite = envInt("OZON_MAX_ITEMS_PER_WRITE", safety.MaxItemsPerWrite)
	safety.MaxResponseBytes = envInt("OZON_MAX_RESPONSE_BYTES", safety.MaxResponseBytes)

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

	// Диагностика уходит в stderr: stdout занят протоколом.
	fmt.Fprintf(os.Stderr, "ozon-seller-mcp %s: %d инструментов, режим %s\n",
		version, len(srv.ToolNames()), safety.Mode)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := srv.Serve(ctx, os.Stdin, os.Stdout); err != nil {
		fatal(err.Error())
	}
}

// runCheck проверяет ключи до того, как сервер пропишут в Claude.
// Отлаживать молчащий MCP-сервер внутри клиента неприятно — гораздо
// быстрее увидеть ошибку в терминале.
func runCheck(client *ozon.Client, srv *mcp.Server, safety tools.Safety) {
	fmt.Printf("ozon-seller-mcp %s\n", version)
	fmt.Printf("Инструментов: %d\n", len(srv.ToolNames()))
	fmt.Printf("Режим:        %s\n\n", safety.Mode)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

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
		var apiErr *ozon.APIError
		if ok := errorsAs(err, &apiErr); ok {
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
	fmt.Println("\nСервер готов. Пропишите его в конфигурацию Claude — см. README.")
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "ozon-seller-mcp:", msg)
	os.Exit(1)
}

func envBool(key string) bool {
	v := os.Getenv(key)
	return v == "1" || v == "true" || v == "TRUE" || v == "yes"
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
