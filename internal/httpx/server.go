package httpx

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/MainAlexStark/ozon-seller-mcp/internal/mcp"
	"github.com/MainAlexStark/ozon-seller-mcp/internal/oauth"
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

// Auth — способ проверки входящих запросов.
//
// Способов два, и они не равноценны. OAuth — основной: короткие токены,
// поштучный отзыв, согласие на конкретные права. Статические токены
// оставлены для автоматизации, которая не может пройти экран согласия.
type Auth struct {
	OAuth  *oauth.Server
	Static *StaticAuth
}

// Server — сетевая обвязка вокруг MCP: авторизация, права подключения
// и проверка Origin.
//
// Сам протокол обслуживает обработчик из internal/mcp, работающий без
// состояния: сервер не выдаёт Mcp-Session-Id и не требует его. Каждый
// запрос самодостаточен, поэтому переживает перезапуск процесса и не
// ломается за балансировщиком.
type Server struct {
	mcpHTTP http.Handler
	auth    Auth
	cfg     Config
	logger  *slog.Logger
}

// maxRequestBytes — потолок размера тела запроса. Без него один запрос
// может съесть память сервера.
const maxRequestBytes = 8 << 20

// NewServer собирает сетевой транспорт.
func NewServer(m *mcp.Server, auth Auth, cfg Config) (*Server, error) {
	if auth.OAuth == nil && !auth.Static.Enabled() {
		return nil, ErrNoAuth
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		mcpHTTP: m.StreamableHandler(mcp.StreamableOptions{
			Logger:          logger,
			MaxRequestBytes: maxRequestBytes,
		}),
		auth:   auth,
		cfg:    cfg,
		logger: logger,
	}, nil
}

// Handler возвращает HTTP-обработчик.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.health)

	// Эндпоинты авторизации регистрируются до общего обработчика,
	// иначе они попали бы под проверку токена — которого у клиента
	// на этом этапе ещё нет.
	if s.auth.OAuth != nil {
		s.auth.OAuth.Mount(mux)
	}

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

	scope, ok := s.authorize(w, r)
	if !ok {
		return // ответ уже отправлен
	}

	// Права подключения кладём в контекст: инструменты запрета записи
	// читают именно оттуда, а не из настроек процесса. Обработчик
	// протокола передаёт контекст запроса дальше, до вызова инструмента,
	// поэтому подмена прав по дороге невозможна.
	safety := s.cfg.Safety
	safety.Mode = tools.ModeReadOnly
	if scope == ScopeWrite {
		safety.Mode = tools.ModeWrite
	}
	ctx := tools.WithSafety(r.Context(), safety)

	// Тело не логируем: в нём бывают чувствительные данные. Метода
	// протокола здесь тоже нет — чтобы его узнать, пришлось бы читать
	// тело раньше обработчика.
	s.logger.Info("запрос",
		"http", r.Method,
		"scope", string(scope),
		"remote", clientIP(r))

	s.mcpHTTP.ServeHTTP(w, r.WithContext(ctx))
}

// authorize определяет права запроса.
//
// Порядок важен: сначала OAuth, потом статический токен. Токен OAuth
// и статический токен внешне неразличимы — оба приходят как Bearer, —
// поэтому решает не форма, а то, знает ли о нём сервер авторизации.
//
// При отказе ответ обязан быть 401 с заголовком WWW-Authenticate,
// указывающим на метаданные ресурса: именно по нему клиент понимает,
// куда идти за токеном. Без этого заголовка Claude просто не узнает,
// что сервер вообще поддерживает OAuth, и подключение молча не
// состоится.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) (Scope, bool) {
	token := bearerToken(r)

	if s.auth.OAuth != nil {
		if t, err := s.auth.OAuth.Validate(token); err == nil {
			if t.HasScope(oauth.ScopeWrite) {
				return ScopeWrite, true
			}
			return ScopeRead, true
		}
	}

	if scope, ok := s.auth.Static.Scope(token); ok {
		return scope, true
	}

	// Тело и адрес не логируем: в них могут быть чувствительные данные.
	s.logger.Warn("отказ в доступе", "remote", clientIP(r), "method", r.Method)

	if s.auth.OAuth != nil {
		desc := "требуется авторизация"
		if token != "" {
			desc = "токен недействителен или истёк"
		}
		s.auth.OAuth.WriteUnauthorized(w, desc)
		return "", false
	}

	w.Header().Set("WWW-Authenticate", `Bearer realm="ozon-seller-mcp"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return "", false
}

func (s *Server) originAllowed(origin string) bool {
	for _, allowed := range s.cfg.AllowedOrigins {
		if strings.EqualFold(strings.TrimSpace(allowed), origin) {
			return true
		}
	}
	return false
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
