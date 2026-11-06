package kernel

import (
	"errors"
	"math"
)

var (
	ErrCurrencyMismatch = errors.New("kernel: currency mismatch")
	ErrAmountOverflow   = errors.New("kernel: amount overflow")
)

// CurrencyCode is an ISO 4217 alphabetic code. The kernel checks only its form; whether a code is in
// use belongs to reference data.
type CurrencyCode string

// Valid reports whether c is three upper-case ASCII letters.
func (c CurrencyCode) Valid() bool {
	if len(c) != 3 {
		return false
	}
	for i := 0; i < len(c); i++ {
		if c[i] < 'A' || c[i] > 'Z' {
			return false
		}
	}
	return true
}

// Money is an amount in minor units of one currency, such as cents for USD. It never uses floating
// point and never combines amounts of different currencies.
type Money struct {
	Currency CurrencyCode
	Minor    int64
}

// Add returns m + o.
func (m Money) Add(o Money) (Money, error) {
	if m.Currency != o.Currency {
		return Money{}, ErrCurrencyMismatch
	}
	if (o.Minor > 0 && m.Minor > math.MaxInt64-o.Minor) || (o.Minor < 0 && m.Minor < math.MinInt64-o.Minor) {
		return Money{}, ErrAmountOverflow
	}
	return Money{Currency: m.Currency, Minor: m.Minor + o.Minor}, nil
}

// Sub returns m − o.
func (m Money) Sub(o Money) (Money, error) {
	if o.Minor == math.MinInt64 {
		return Money{}, ErrAmountOverflow
	}
	return m.Add(Money{Currency: o.Currency, Minor: -o.Minor})
}
