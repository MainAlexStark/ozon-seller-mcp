// Package tools — реестр инструментов MCP-сервера и защита от
// разрушительных действий.
//
// Ключевая мысль этого пакета: сервер отдаёт языковой модели доступ
// к живому магазину. Ошибка здесь стоит не времени, а денег и рейтинга
// продавца. Поэтому опасные действия закрыты тремя независимыми
// заслонами:
//
//  1. Режим только для чтения по умолчанию. Запись включается явно
//     переменной окружения — то есть осознанным действием человека,
//     а не удачной формулировкой в чате.
//
//  2. Страховка от опечатки в порядке величины. Смена цены с 690 на 69
//     проходит все проверки формата и уничтожает маржу. Поэтому перед
//     записью новая цена сверяется с текущей, и слишком резкое
//     изменение требует явного подтверждения.
//
//  3. Ограничение размера ответа. Ответ на 5 МБ не «просто длинный» —
//     клиент режет результат инструмента по числу токенов, и такой
//     ответ до модели не доезжает вовсе. Обрезка здесь — не забота
//     о контексте, а разница между «данные неполны» и молчаливым
//     циклом из повторных вызовов.
package tools

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

// Mode — режим работы сервера.
type Mode string

const (
	// ModeReadOnly — только чтение. Значение по умолчанию.
	ModeReadOnly Mode = "read-only"
	// ModeWrite — чтение и запись.
	ModeWrite Mode = "write"
)

// Safety — настройки защиты.
type Safety struct {
	Mode Mode

	// MaxPriceDeltaPct — на сколько процентов цена может отличаться
	// от текущей без явного подтверждения. 0 отключает проверку.
	MaxPriceDeltaPct float64

	// MaxResponseBytes — потолок размера ответа инструмента.
	//
	// Значение выбирается не «чтобы влезло», а с оглядкой на клиента:
	// он режет результат инструмента по числу токенов (обычно 25 000),
	// и ответ сверх этого не доезжает вовсе — модель видит отказ
	// вместо данных и, не понимая причины, повторяет вызов. Плотный
	// JSON — это примерно три байта на токен, поэтому потолок в байтах
	// имеет смысл держать заметно ниже, чем «сколько не жалко».
	MaxResponseBytes int

	// MaxItemsPerWrite — сколько позиций разрешено менять за один вызов.
	MaxItemsPerWrite int
}

// DefaultSafety — осторожные значения по умолчанию.
func DefaultSafety() Safety {
	return Safety{
		Mode:             ModeReadOnly,
		MaxPriceDeltaPct: 30,
		MaxResponseBytes: 40_000,
		MaxItemsPerWrite: 100,
	}
}

// ErrWriteDisabled возвращается, когда инструмент записи вызван
// в режиме чтения.
type ErrWriteDisabled struct {
	Tool string
}

func (e ErrWriteDisabled) Error() string {
	// Сообщение покрывает оба транспорта, потому что причина отказа
	// у них разная, а инструмент один и тот же: локально режим задан
	// переменной окружения, по сети — тем, какой токен прописан
	// в этом клиенте. Не назвать оба варианта — значит отправить
	// человека править не то место.
	return fmt.Sprintf(
		"Инструмент %s изменяет данные в магазине, а у этого подключения прав на запись нет.\n\n"+
			"Это защита по умолчанию: доступ на запись открывает человек в конфигурации,\n"+
			"а не удачная формулировка в разговоре.\n\n"+
			"Если сервер запущен локально (stdio) — добавьте в его конфигурацию:\n"+
			"    OZON_ALLOW_WRITES=true\n"+
			"и перезапустите Claude.\n\n"+
			"Если сервер сетевой — в этом клиенте прописан токен только на чтение.\n"+
			"Запись возможна там, где прописан пишущий токен; телефону он обычно не нужен.\n\n"+
			"Текущее состояние подключения показывает ozon_status.\n"+
			"Все инструменты чтения при этом работают как обычно.",
		e.Tool)
}

// CheckWrite пропускает запись только в режиме ModeWrite.
func (s Safety) CheckWrite(tool string) error {
	if s.Mode != ModeWrite {
		return ErrWriteDisabled{Tool: tool}
	}
	return nil
}

// CheckBatchSize ограничивает размер пачки на запись.
func (s Safety) CheckBatchSize(n int) error {
	if s.MaxItemsPerWrite > 0 && n > s.MaxItemsPerWrite {
		return fmt.Errorf(
			"за один вызов разрешено менять не более %d позиций, запрошено %d. "+
				"Разбейте на несколько вызовов — так ошибка затронет меньше товаров",
			s.MaxItemsPerWrite, n)
	}
	return nil
}

// PriceChange — предполагаемое изменение цены одного товара.
type PriceChange struct {
	OfferID string
	Old     float64
	New     float64
}

// DeltaPct — насколько новая цена отличается от текущей, в процентах.
// Возвращает 0, если текущая цена неизвестна: сравнивать не с чем,
// и блокировать первое назначение цены неправильно.
func (c PriceChange) DeltaPct() float64 {
	if c.Old <= 0 {
		return 0
	}
	return math.Abs(c.New-c.Old) / c.Old * 100
}

// CheckPriceChanges находит изменения, выходящие за порог.
//
// confirmed == true означает, что человек в чате явно подтвердил
// намерение (аргумент confirm_large_change), и проверка пропускается.
func (s Safety) CheckPriceChanges(changes []PriceChange, confirmed bool) error {
	if s.MaxPriceDeltaPct <= 0 || confirmed {
		return nil
	}

	var suspicious []PriceChange
	for _, c := range changes {
		if c.DeltaPct() > s.MaxPriceDeltaPct {
			suspicious = append(suspicious, c)
		}
	}
	if len(suspicious) == 0 {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Изменение цены больше чем на %.0f%% — запись остановлена.\n\n", s.MaxPriceDeltaPct)
	fmt.Fprintf(&b, "Чаще всего это опечатка в порядке величины (690 вместо 6900), "+
		"и она уничтожает маржу молча: Ozon примет такую цену без возражений.\n\n")
	for _, c := range suspicious {
		fmt.Fprintf(&b, "  %-24s %.2f → %.2f  (%.0f%%)\n", c.OfferID, c.Old, c.New, c.DeltaPct())
	}
	fmt.Fprintf(&b, "\nЕсли изменение верное, повторите вызов с confirm_large_change: true.")

	return fmt.Errorf("%s", b.String())
}

// TrimResponse укорачивает слишком длинный ответ, объясняя, что
// произошло.
//
// JSON укорачивается по элементам — так, чтобы наружу вышел валидный
// документ с отметкой об обрезке (см. trim.go). Байтовый срез остаётся
// только для ответов, которые не JSON: там резать по элементам нечего,
// зато важно, чтобы обрубок нельзя было принять за полные данные.
func (s Safety) TrimResponse(body string) string {
	if s.MaxResponseBytes <= 0 || len(body) <= s.MaxResponseBytes {
		return body
	}

	// Хвост с объяснением сам занимает место, и без запаса под него
	// обрезанный ответ снова оказывается длиннее потолка.
	const noteBudget = 400
	limit := s.MaxResponseBytes - noteBudget
	if limit < 0 {
		limit = 0
	}

	if trimmed, ok := trimJSON(body, limit); ok {
		return trimmed
	}

	if limit > len(body) {
		limit = len(body)
	}
	// Срез по байтам легко приходится на середину кириллической буквы,
	// а битый UTF-8 ломает уже не смысл ответа, а сам протокол.
	for limit > 0 && !utf8.ValidString(body[:limit]) {
		limit--
	}
	return body[:limit] + fmt.Sprintf(
		"\n\n[…ответ обрезан на %d байтах из %d — это фрагмент, а не документ, "+
			"и разбирать его как целое нельзя. Повторный вызов с теми же аргументами "+
			"вернёт ровно то же самое: сузьте выборку — меньший limit, фильтр, "+
			"конкретные offer_id или один день.]",
		limit, len(body))
}
