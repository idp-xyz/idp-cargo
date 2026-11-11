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

// minorDigits are the ISO 4217 minor units of the currencies the China export lines deal in.
var minorDigits = map[CurrencyCode]int{
	"CNY": 2, "USD": 2, "EUR": 2, "GBP": 2, "HKD": 2, "MOP": 2, "TWD": 2, "SGD": 2, "AUD": 2, "NZD": 2, "CAD": 2, "CHF": 2, "SEK": 2, "NOK": 2,
	"DKK": 2, "PLN": 2, "CZK": 2, "HUF": 2, "RUB": 2, "TRY": 2, "AED": 2, "SAR": 2, "QAR": 2, "ILS": 2, "INR": 2, "PKR": 2, "BDT": 2, "LKR": 2,
	"THB": 2, "MYR": 2, "IDR": 2, "PHP": 2, "MXN": 2, "BRL": 2, "ARS": 2, "COP": 2, "PEN": 2, "ZAR": 2, "EGP": 2, "NGN": 2, "KES": 2, "MAD": 2,
	"JPY": 0, "KRW": 0, "VND": 0, "CLP": 0, "ISK": 0, "PYG": 0, "UGX": 0, "XAF": 0, "XOF": 0,
	"KWD": 3, "BHD": 3, "OMR": 3, "JOD": 3, "TND": 3, "IQD": 3, "LYD": 3,
}

// MinorDigits returns a currency's ISO 4217 minor digits, when the kernel lists the currency.
func MinorDigits(c CurrencyCode) (int, bool) {
	d, ok := minorDigits[c]
	return d, ok
}
