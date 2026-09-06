package httpx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/MainAlexStark/ozon-seller-mcp/internal/mcp"
	"github.com/MainAlexStark/ozon-seller-mcp/internal/tools"
)

// Config — настройки сетевого транспорта.
type Config struct {
	Addr string

	// AllowedOrigins — источники, которым разрешено обращаться из
	// браузера. Пусто означает «браузерные запросы с Origin
	// отклоняются», и для сервера, к которому ходит только Claude,
	// это правильное значение по умолчанию.
	AllowedOrigins []string

	// Safety — пороги защиты, общие для всех подключений. Режим
	// (чтение/запись) сюда не входит: он определяется токеном.
	Safety tools.Safety

	Logger *slog.Logger
}

// Server — MCP поверх Streamable HTTP.
//
// Сервер намеренно сделан без состояния: не выдаёт Mcp-Session-Id и не
// требует его. Каждый запрос самодостаточен, поэтому переживает
// перезапуск процесса и не ломается за балансировщиком.
type Server struct {
	mcp    *mcp.Server
	auth   *Auth
	cfg    Config
	logger *slog.Logger
}

// NewServer собирает сетевой транспорт.
func NewServer(m *mcp.Server, auth *Auth, cfg Config) *Server {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{mcp: m, auth: auth, cfg: cfg, logger: logger}
}

// Handler возвращает HTTP-обработчик.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.health)
	mux.HandleFunc("/", s.mcpEndpoint)
	return mux
}

// ListenAndServe поднимает сервер.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      6 * time.Minute, // ozon_import_status с wait ждёт до 5 минут
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	}
}

// health отвечает без авторизации: он ничего не рассказывает о магазине
// и нужен, чтобы обратный прокси и мониторинг видели живость процесса.
func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"status":"ok"}`)
}

func (s *Server) mcpEndpoint(w http.ResponseWriter, r *http.Request) {
	// Защита от DNS rebinding: браузер на постороннем сайте не должен
	// уметь обращаться к серверу от имени пользователя.
	if origin := r.Header.Get("Origin"); origin != "" && !s.originAllowed(origin) {
		s.logger.Warn("отклонён запрос с чужого Origin", "origin", origin)
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}

	token, _ := TokenFromRequest(r)
	scope, ok := s.auth.Scope(token)
	if !ok {
		// Ни адрес, ни тело не логируются: в адресе может быть токен.
		s.logger.Warn("отказ в доступе", "remote", clientIP(r), "method", r.Method)
		w.Header().Set("WWW-Authenticate", `Bearer realm="ozon-seller-mcp"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	switch r.Method {
	case http.MethodPost:
		s.handlePost(w, r, scope)
	case http.MethodGet:
		// Поток server-to-client не нужен: сервер сам ничего не
		// инициирует. Спецификация разрешает ответить 405.
		http.Error(w, "server-initiated stream not supported", http.StatusMethodNotAllowed)
	case http.MethodDelete:
		// Сессий нет, удалять нечего.
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request, scope Scope) {
	body, err := readLimited(w, r, 8<<20)
	if err != nil {
		writeRPCError(w, http.StatusBadRequest, -32700, "не удалось прочитать тело запроса")
		return
	}

	var probe struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		writeRPCError(w, http.StatusBadRequest, -32700, "тело не является JSON-RPC сообщением")
		return
	}

	// Уведомление или ответ — подтверждаем и ничего не возвращаем.
	if len(probe.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// Права подключения кладём в контекст: инструменты запрета записи
	// читают именно оттуда, а не из настроек процесса.
	safety := s.cfg.Safety
	safety.Mode = tools.ModeReadOnly
	if scope == ScopeWrite {
		safety.Mode = tools.ModeWrite
	}
	ctx := tools.WithSafety(r.Context(), safety)

	resp, err := s.mcp.HandleMessage(ctx, body)
	if err != nil {
		writeRPCError(w, http.StatusInternalServerError, -32603, "внутренняя ошибка")
		return
	}

	s.logger.Info("вызов",
		"method", probe.Method,
		"scope", string(scope),
		"remote", clientIP(r))

	// Клиент может принимать только поток событий — отвечаем в SSE.
	if wantsSSEOnly(r.Header.Get("Accept")) {
		writeSSE(w, resp)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp)
}

func (s *Server) originAllowed(origin string) bool {
	for _, allowed := range s.cfg.AllowedOrigins {
		if strings.EqualFold(strings.TrimSpace(allowed), origin) {
			return true
		}
	}
	return false
}

// wantsSSEOnly — клиент готов принять только text/event-stream.
func wantsSSEOnly(accept string) bool {
	if accept == "" {
		return false
	}
	a := strings.ToLower(accept)
	hasJSON := strings.Contains(a, "application/json") || strings.Contains(a, "*/*")
	hasSSE := strings.Contains(a, "text/event-stream")
	return hasSSE && !hasJSON
}

func writeSSE(w http.ResponseWriter, payload []byte) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func writeRPCError(w http.ResponseWriter, status, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"error":   map[string]any{"code": code, "message": msg},
	})
}

// readLimited читает тело с потолком: без него один запрос может
// съесть память сервера.
func readLimited(w http.ResponseWriter, r *http.Request, max int64) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(http.MaxBytesReader(w, r.Body, max))
}

// clientIP берёт адрес из X-Forwarded-For, если сервер стоит за прокси.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	return r.RemoteAddr
}
