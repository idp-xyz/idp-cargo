package kernel

import (
	"errors"
	"math"
	"testing"
)

func TestUnitCodeForm(t *testing.T) {
	for code, want := range map[UnitCode]bool{"KGM": true, "H87": true, "C62": true, "kgm": false, "KG-": false, "KGMS": false, "": false} {
		if got := code.Valid(); got != want {
			t.Errorf("%q.Valid() = %v, want %v", code, got, want)
		}
	}
}

func TestQuantityArithmetic(t *testing.T) {
	kg := func(n int64) Quantity { return Quantity{Unit: "KGM", Amount: n} }
	if got, err := kg(500).Add(kg(250)); err != nil || got != kg(750) {
		t.Fatalf("Add = %+v, %v", got, err)
	}
	if got, err := kg(200).Sub(kg(300)); err != nil || got != kg(-100) {
		t.Fatalf("Sub = %+v, %v", got, err)
	}
	if _, err := kg(1).Add(Quantity{Unit: "H87", Amount: 1}); !errors.Is(err, ErrUnitMismatch) {
		t.Fatalf("mixed units: err = %v", err)
	}
	if _, err := kg(math.MaxInt64).Add(kg(1)); !errors.Is(err, ErrAmountOverflow) {
		t.Fatalf("overflow: err = %v", err)
	}
	if _, err := kg(0).Sub(kg(math.MinInt64)); !errors.Is(err, ErrAmountOverflow) {
		t.Fatalf("negating the minimum: err = %v", err)
	}
}
