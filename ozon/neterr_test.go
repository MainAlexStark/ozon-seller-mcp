package ozon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTimeoutIsNetworkErrorNotAPIError(t *testing.T) {
	// Сервер, который никогда не отвечает: так выглядит блокировка
	// маршрута — соединение висит и обрывается по времени.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	c := New("cid", "key",
		WithBaseURL(srv.URL),
		WithHTTPClient(&http.Client{Timeout: 200 * time.Millisecond}),
	)

	_, err := c.Call(context.Background(), PathProductList, nil)
	if err == nil {
		t.Fatal("ожидалась ошибка")
	}

	// Ключевое различие: это НЕ APIError. Иначе подсказка посоветует
	// проверять ключ там, где виноват маршрут.
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Fatalf("таймаут не должен быть APIError, получено %v", apiErr)
	}

	var netErr *NetworkError
	if !errors.As(err, &netErr) {
		t.Fatalf("ожидалась *NetworkError, получено %T", err)
	}
	if netErr.Kind() != KindTimeout {
		t.Errorf("вид сбоя: got %s, want %s", netErr.Kind(), KindTimeout)
	}

	hint := netErr.Hint()
	for _, want := range []string{"не дошёл", "VPN", "OZON_PROXY", "VPS"} {
		if !strings.Contains(hint, want) {
			t.Errorf("подсказка должна упоминать %q:\n%s", want, hint)
		}
	}
	// Подсказка вправе упомянуть ключ — но только чтобы сказать, что
	// дело не в нём. Советовать его проверять здесь нельзя.
	for _, wrong := range []string{"Проверьте OZON_CLIENT_ID", "проверьте ключ", "Проверьте ключ"} {
		if strings.Contains(hint, wrong) {
			t.Errorf("подсказка про маршрут не должна советовать проверять ключ: %q", wrong)
		}
	}
}

func TestConnectionRefusedClassified(t *testing.T) {
	// Порт, на котором заведомо никто не слушает.
	c := New("cid", "key", WithBaseURL("http://127.0.0.1:1"))

	_, err := c.Call(context.Background(), PathProductList, nil)
	if err == nil {
		t.Fatal("ожидалась ошибка")
	}

	var netErr *NetworkError
	if !errors.As(err, &netErr) {
		t.Fatalf("ожидалась *NetworkError, получено %T", err)
	}
	if k := netErr.Kind(); k != KindRefused && k != KindOtherNet {
		t.Errorf("вид сбоя: got %s", k)
	}
}

func TestDNSFailureClassified(t *testing.T) {
	// Классификатор проверяется напрямую: реальный сбой DNS в тестовом
	// окружении может прийти в любой обёртке (например, через прокси
	// типизированной *net.DNSError не будет вовсе), и тест превратился бы
	// в проверку окружения вместо проверки кода.
	cases := []struct {
		name string
		err  error
	}{
		{"типизированная ошибка", &net.DNSError{Err: "no such host", Name: "api-seller.ozon.ru", IsNotFound: true}},
		{"обёрнутая в текст", errors.New(`Post "https://api-seller.ozon.ru/v3/product/list": dial tcp: lookup api-seller.ozon.ru: no such host`)},
	}

	for _, tc := range cases {
		netErr := &NetworkError{Path: PathProductList, Err: tc.err}
		if got := netErr.Kind(); got != KindDNS {
			t.Errorf("%s: Kind() = %s, want %s", tc.name, got, KindDNS)
		}
		if !strings.Contains(netErr.Hint(), "DNS") {
			t.Errorf("%s: подсказка должна упоминать DNS:\n%s", tc.name, netErr.Hint())
		}
	}
}

func TestHintMentionsProxyWhenConfigured(t *testing.T) {
	// Если прокси задан, а соединение не идёт — виноват скорее всего он,
	// и советовать «настройте прокси» человеку, который его уже настроил,
	// бессмысленно.
	c := New("cid", "key",
		WithBaseURL("http://127.0.0.1:1"),
		WithProxy("socks5://127.0.0.1:1080"),
	)

	_, err := c.Call(context.Background(), PathProductList, nil)
	var netErr *NetworkError
	if !errors.As(err, &netErr) {
		t.Fatalf("ожидалась *NetworkError, получено %T", err)
	}

	hint := netErr.Hint()
	if !strings.Contains(hint, "127.0.0.1:1080") {
		t.Errorf("подсказка должна называть прокси:\n%s", hint)
	}
	if strings.Contains(hint, "OZON_PROXY=") {
		t.Error("не надо предлагать настроить прокси тому, у кого он уже настроен")
	}
}

func TestNetworkErrorNotRetried(t *testing.T) {
	// Повторять запрос, который не доходит, бессмысленно: маршрут
	// за секунду не починится, а человек ждёт четыре таймаута вместо
	// одного и дольше не видит причину.
	// Счётчик атомарный: обработчик продолжает спать после того, как
	// клиент отвалился по таймауту, поэтому запись и чтение идут из
	// разных горутин одновременно.
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	c := New("cid", "key",
		WithBaseURL(srv.URL),
		WithHTTPClient(&http.Client{Timeout: 150 * time.Millisecond}),
		WithMaxRetries(3),
	)

	_, _ = c.Call(context.Background(), PathProductList, nil)

	if n := attempts.Load(); n != 1 {
		t.Errorf("сетевой сбой не должен повторяться, попыток было %d", n)
	}
}

func TestProxyIsRedactedInDiagnostics(t *testing.T) {
	c := New("cid", "key", WithProxy("socks5://user:s3cret@vps.example.com:1080"))

	if err := c.ProxyError(); err != nil {
		t.Fatalf("адрес должен разобраться: %v", err)
	}
	got := c.Proxy()
	if strings.Contains(got, "s3cret") {
		t.Errorf("пароль не должен попадать в диагностику: %s", got)
	}
	if !strings.Contains(got, "vps.example.com:1080") {
		t.Errorf("хост должен остаться видимым: %s", got)
	}
}

func TestBadProxyURLReported(t *testing.T) {
	// Молча пойти напрямую, когда человек просил через прокси, —
	// худший исход: он будет думать, что трафик идёт куда надо.
	c := New("cid", "key", WithProxy("://это-не-адрес"))
	if c.ProxyError() == nil {
		t.Fatal("неразбираемый адрес прокси должен давать ошибку")
	}
}

func TestProxyActuallyUsed(t *testing.T) {
	var proxied atomic.Bool

	// Подставной прокси: любой запрос к нему считаем доказательством,
	// что трафик пошёл через прокси, а не напрямую.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Store(true)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":"через прокси"}`))
	}))
	defer proxy.Close()

	c := New("cid", "key",
		WithBaseURL("http://api-seller.ozon.ru.invalid"),
		WithProxy(proxy.URL),
	)

	_, _ = c.Call(context.Background(), PathProductList, nil)

	if !proxied.Load() {
		t.Fatal("запрос должен был уйти через прокси")
	}
}
