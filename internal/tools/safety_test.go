package tools

import (
	"errors"
	"strings"
	"testing"
)

func TestReadOnlyByDefault(t *testing.T) {
	s := DefaultSafety()
	if s.Mode != ModeReadOnly {
		t.Fatalf("по умолчанию режим должен быть только для чтения, получен %s", s.Mode)
	}

	err := s.CheckWrite("ozon_prices_update")
	if err == nil {
		t.Fatal("запись в режиме чтения должна быть запрещена")
	}

	var disabled ErrWriteDisabled
	if !errors.As(err, &disabled) {
		t.Fatalf("ожидалась ErrWriteDisabled, получено %T", err)
	}
	if !strings.Contains(err.Error(), "OZON_ALLOW_WRITES") {
		t.Error("сообщение должно объяснять, как включить запись")
	}
}

func TestWriteAllowedInWriteMode(t *testing.T) {
	s := DefaultSafety()
	s.Mode = ModeWrite
	if err := s.CheckWrite("ozon_prices_update"); err != nil {
		t.Fatalf("в режиме записи запись должна проходить: %v", err)
	}
}

func TestPriceGuardCatchesOrderOfMagnitudeTypo(t *testing.T) {
	s := DefaultSafety()
	s.Mode = ModeWrite

	// Классическая опечатка: потерян ноль.
	err := s.CheckPriceChanges([]PriceChange{
		{OfferID: "pp-mw-1", Old: 6900, New: 690},
	}, false)

	if err == nil {
		t.Fatal("падение цены в 10 раз должно останавливать запись")
	}
	if !strings.Contains(err.Error(), "confirm_large_change") {
		t.Error("сообщение должно объяснять, как подтвердить намеренное изменение")
	}
	if !strings.Contains(err.Error(), "pp-mw-1") {
		t.Error("сообщение должно называть проблемный товар")
	}
}

func TestPriceGuardAllowsSmallChange(t *testing.T) {
	s := DefaultSafety() // порог 30 %
	s.Mode = ModeWrite

	err := s.CheckPriceChanges([]PriceChange{
		{OfferID: "pp-mw-1", Old: 1000, New: 1200}, // +20 %
	}, false)
	if err != nil {
		t.Fatalf("изменение на 20 %% должно проходить: %v", err)
	}
}

func TestPriceGuardRespectsConfirmation(t *testing.T) {
	s := DefaultSafety()
	s.Mode = ModeWrite

	err := s.CheckPriceChanges([]PriceChange{
		{OfferID: "pp-mw-1", Old: 6900, New: 690},
	}, true)
	if err != nil {
		t.Fatalf("подтверждённое изменение должно проходить: %v", err)
	}
}

func TestPriceGuardIgnoresUnknownCurrentPrice(t *testing.T) {
	// Первое назначение цены: сравнивать не с чем, блокировать нельзя.
	s := DefaultSafety()
	s.Mode = ModeWrite

	err := s.CheckPriceChanges([]PriceChange{
		{OfferID: "новый-товар", Old: 0, New: 1500},
	}, false)
	if err != nil {
		t.Fatalf("товар без текущей цены не должен блокироваться: %v", err)
	}
}

func TestDeltaPct(t *testing.T) {
	cases := []struct {
		old, new, want float64
	}{
		{1000, 1200, 20},
		{1000, 800, 20}, // падение считается так же
		{6900, 690, 90},
		{0, 500, 0},
	}
	for _, c := range cases {
		got := PriceChange{Old: c.old, New: c.new}.DeltaPct()
		if got < c.want-0.01 || got > c.want+0.01 {
			t.Errorf("DeltaPct(%.0f -> %.0f) = %.2f, want %.2f", c.old, c.new, got, c.want)
		}
	}
}

func TestBatchSizeLimit(t *testing.T) {
	s := DefaultSafety()
	if err := s.CheckBatchSize(50); err != nil {
		t.Errorf("50 позиций должны проходить: %v", err)
	}
	if err := s.CheckBatchSize(500); err == nil {
		t.Error("500 позиций должны отклоняться")
	}
}

func TestTrimResponse(t *testing.T) {
	s := DefaultSafety()
	s.MaxResponseBytes = 20

	short := "коротко"
	if got := s.TrimResponse(short); got != short {
		t.Errorf("короткий ответ не должен меняться, получено %q", got)
	}

	long := strings.Repeat("x", 100)
	got := s.TrimResponse(long)
	if !strings.Contains(got, "обрезан") {
		t.Error("обрезанный ответ должен объяснять, что произошло")
	}
	if !strings.Contains(got, "limit") {
		t.Error("сообщение должно подсказывать, как сузить выборку")
	}
}
