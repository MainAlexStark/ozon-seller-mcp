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
//     он вытесняет из контекста задачу, ради которой вызывался.
package tools

import (
	"fmt"
	"math"
	"strings"
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
	MaxResponseBytes int

	// MaxItemsPerWrite — сколько позиций разрешено менять за один вызов.
	MaxItemsPerWrite int
}

// DefaultSafety — осторожные значения по умолчанию.
func DefaultSafety() Safety {
	return Safety{
		Mode:             ModeReadOnly,
		MaxPriceDeltaPct: 30,
		MaxResponseBytes: 120_000,
		MaxItemsPerWrite: 100,
	}
}

// ErrWriteDisabled возвращается, когда инструмент записи вызван
// в режиме чтения.
type ErrWriteDisabled struct {
	Tool string
}

func (e ErrWriteDisabled) Error() string {
	return fmt.Sprintf(
		"Инструмент %s изменяет данные в магазине, а сервер запущен в режиме только для чтения.\n\n"+
			"Это защита по умолчанию: доступ на запись включается человеком, а не по ходу разговора.\n"+
			"Чтобы разрешить запись, добавьте в конфигурацию сервера переменную окружения:\n\n"+
			"    OZON_ALLOW_WRITES=true\n\n"+
			"и перезапустите Claude. Пока режим не изменён, все read-инструменты работают как обычно.",
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

// TrimResponse обрезает слишком длинный ответ, объясняя, что произошло.
func (s Safety) TrimResponse(body string) string {
	if s.MaxResponseBytes <= 0 || len(body) <= s.MaxResponseBytes {
		return body
	}
	return body[:s.MaxResponseBytes] + fmt.Sprintf(
		"\n\n[…ответ обрезан на %d байтах из %d. "+
			"Сузьте выборку: уменьшите limit, добавьте фильтр или запросите конкретные offer_id.]",
		s.MaxResponseBytes, len(body))
}
