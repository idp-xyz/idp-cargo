package kernel

import (
	"errors"
	"math"
)

var ErrUnitMismatch = errors.New("kernel: unit mismatch")

// UnitCode is a unit of measure in the form of UN/ECE Recommendation 20, such as KGM or H87. The kernel
// checks only its form; which units a context accepts belongs to that context and to reference data.
type UnitCode string

// Valid reports whether u is one to three upper-case ASCII letters or digits.
func (u UnitCode) Valid() bool {
	if len(u) == 0 || len(u) > 3 {
		return false
	}
	for i := 0; i < len(u); i++ {
		if (u[i] < 'A' || u[i] > 'Z') && (u[i] < '0' || u[i] > '9') {
			return false
		}
	}
	return true
}

// Quantity is a whole number of units. Pick a unit fine enough for the precision needed, such as grams
// rather than fractions of a kilogram; quantities in different units are never combined.
type Quantity struct {
	Unit   UnitCode
	Amount int64
}

// Add returns q + o.
func (q Quantity) Add(o Quantity) (Quantity, error) {
	if q.Unit != o.Unit {
		return Quantity{}, ErrUnitMismatch
	}
	if (o.Amount > 0 && q.Amount > math.MaxInt64-o.Amount) || (o.Amount < 0 && q.Amount < math.MinInt64-o.Amount) {
		return Quantity{}, ErrAmountOverflow
	}
	return Quantity{Unit: q.Unit, Amount: q.Amount + o.Amount}, nil
}

// Sub returns q − o.
func (q Quantity) Sub(o Quantity) (Quantity, error) {
	if o.Amount == math.MinInt64 {
		return Quantity{}, ErrAmountOverflow
	}
	return q.Add(Quantity{Unit: o.Unit, Amount: -o.Amount})
}
