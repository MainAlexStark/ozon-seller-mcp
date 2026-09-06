package ozon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
)

// NetworkError — запрос не дошёл до Ozon.
//
// Это принципиально другой класс ошибок, чем APIError. APIError означает
// «Ozon вас услышал и отказал» — там виноват запрос или ключ. NetworkError
// означает «до Ozon не добрались» — и тогда виноват маршрут, а искать
// причину надо не в коде.
//
// Различать их важно, потому что подсказки противоположны: на 401 надо
// проверять ключ, а на таймаут — маршрут, и человек, которому на таймаут
// посоветовали проверить ключ, потратит вечер впустую.
type NetworkError struct {
	Path  string
	Proxy string // адрес прокси без пароля, если задан
	Err   error
}

func (e *NetworkError) Error() string {
	return fmt.Sprintf("ozon %s: запрос не дошёл до Ozon: %v", e.Path, e.Err)
}

func (e *NetworkError) Unwrap() error { return e.Err }

// Kind — что именно случилось с соединением.
type Kind string

const (
	KindTimeout  Kind = "timeout" // соединение зависло и оборвалось по времени
	KindRefused  Kind = "refused" // на том конце активно отказали
	KindDNS      Kind = "dns"     // имя не разрешилось
	KindTLS      Kind = "tls"     // не удалось согласовать шифрование
	KindOtherNet Kind = "network" // прочие сетевые
)

// Kind классифицирует сбой.
func (e *NetworkError) Kind() Kind {
	var dnsErr *net.DNSError
	if errors.As(e.Err, &dnsErr) {
		return KindDNS
	}

	var netErr net.Error
	if errors.As(e.Err, &netErr) && netErr.Timeout() {
		return KindTimeout
	}
	if errors.Is(e.Err, context.DeadlineExceeded) {
		return KindTimeout
	}

	msg := strings.ToLower(e.Err.Error())
	switch {
	// Строковые признаки — запасной путь: обёртки бывают разные
	// (например, когда соединение идёт через прокси, типизированной
	// *net.DNSError может и не быть).
	case strings.Contains(msg, "no such host"), strings.Contains(msg, "name resolution"):
		return KindDNS
	case strings.Contains(msg, "connection refused"):
		return KindRefused
	case strings.Contains(msg, "tls"), strings.Contains(msg, "certificate"):
		return KindTLS
	case strings.Contains(msg, "deadline exceeded"), strings.Contains(msg, "timeout"):
		return KindTimeout
	default:
		return KindOtherNet
	}
}

// Hint объясняет, что делать. Формулировки разные, потому что причины
// разные: таймаут — это чаще всего маршрут, отказ — не тот адрес прокси.
func (e *NetworkError) Hint() string {
	var b strings.Builder

	switch e.Kind() {
	case KindTimeout:
		b.WriteString("Соединение с api-seller.ozon.ru не установилось и оборвалось по времени.\n" +
			"Ozon не отказал — запрос до него не дошёл. Так выглядит блокировка маршрута,\n" +
			"а не проблема с ключом или кодом.\n\n" +
			"Самая частая причина: машина работает через VPN, а Ozon из-под этого VPN недоступен.")
	case KindRefused:
		b.WriteString("Соединение активно отклонено. Если задан прокси — проверьте его адрес и порт;\n" +
			"если нет — что-то на машине или в сети закрывает исходящие соединения.")
	case KindDNS:
		b.WriteString("Не удалось разрешить имя api-seller.ozon.ru. Проверьте DNS:\n" +
			"под VPN он часто подменяется, и имя перестаёт разрешаться.")
	case KindTLS:
		b.WriteString("Не удалось согласовать TLS. Обычно это означает, что соединение\n" +
			"перехватывается — корпоративный фильтр или прокси с подменой сертификата.")
	default:
		b.WriteString("Сетевая ошибка при обращении к Ozon.")
	}

	if e.Proxy != "" {
		fmt.Fprintf(&b, "\n\nЗапросы идут через прокси %s — проверьте в первую очередь его.", e.Proxy)
	} else {
		b.WriteString("\n\nЧто можно сделать:\n" +
			"  1. Поднять сервер на VPS в России — тогда ни VPN на вашей машине,\n" +
			"     ни её маршруты вообще не участвуют (см. docs/REMOTE.md).\n" +
			"  2. Исключить api-seller.ozon.ru из VPN-туннеля (split tunneling).\n" +
			"  3. Направить в Ozon только этот сервер через прокси: OZON_PROXY=socks5://...\n" +
			"     Остальная машина при этом остаётся под VPN.\n\n" +
			"Подробнее: docs/NETWORK.md")
	}

	return b.String()
}

// asNetworkError — обёртка над errors.As для читаемости.
func asNetworkError(err error, target **NetworkError) bool {
	return errors.As(err, target)
}
