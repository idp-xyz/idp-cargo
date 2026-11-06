package kernel

import (
	"errors"
	"math"
	"testing"
)

func TestCurrencyCodeForm(t *testing.T) {
	for code, want := range map[CurrencyCode]bool{"USD": true, "CNY": true, "usd": false, "US": false, "USDT": false, "U5D": false, "": false} {
		if got := code.Valid(); got != want {
			t.Errorf("%q.Valid() = %v, want %v", code, got, want)
		}
	}
}

func TestMoneyArithmetic(t *testing.T) {
	usd := func(minor int64) Money { return Money{Currency: "USD", Minor: minor} }
	if got, err := usd(150_000).Add(usd(2_550)); err != nil || got != usd(152_550) {
		t.Fatalf("Add = %+v, %v", got, err)
	}
	if got, err := usd(100).Sub(usd(250)); err != nil || got != usd(-150) {
		t.Fatalf("Sub = %+v, %v", got, err)
	}
	if _, err := usd(1).Add(Money{Currency: "EUR", Minor: 1}); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("mixed currencies: err = %v", err)
	}
	if _, err := usd(math.MaxInt64).Add(usd(1)); !errors.Is(err, ErrAmountOverflow) {
		t.Fatalf("overflow: err = %v", err)
	}
	if _, err := usd(0).Sub(usd(math.MinInt64)); !errors.Is(err, ErrAmountOverflow) {
		t.Fatalf("negating the minimum: err = %v", err)
	}
}
