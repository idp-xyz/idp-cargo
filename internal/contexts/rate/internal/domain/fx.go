package domain

import (
	"fmt"
	"time"
	"unicode/utf8"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// RoundingHalfUpPerLine is how a quote converts: each line amount separately, halves away from zero.
const RoundingHalfUpPerLine = "HALF_UP_PER_LINE"

// ExchangeRate converts amounts in From into the quote's currency (owner decision R8): one unit of From is
// Rate × 10^−Scale units of the quote currency. FromDigits and QuoteDigits are the ISO 4217 minor digits
// the conversion used, so a converted amount can be recomputed from the snapshot alone. The rate prices
// the quote and checks its margin; invoicing and accounting rates belong to FIN.
type ExchangeRate struct {
	From        kernel.CurrencyCode
	Rate        int64
	Scale       int
	FromDigits  int
	QuoteDigits int
	Source      string // such as "Bank of China selling rate"
	AsOf        time.Time
}

func (r ExchangeRate) valid(quote kernel.CurrencyCode) bool {
	return r.From.Valid() && r.From != quote && r.Rate > 0 && r.Scale >= 0 && r.Scale <= 12 && r.FromDigits >= 0 && r.FromDigits <= 4 &&
		r.QuoteDigits >= 0 && r.QuoteDigits <= 4 && !blank(r.Source) && utf8.RuneCountInString(r.Source) <= 200 && !r.AsOf.IsZero()
}

// convert returns amount × Rate × 10^(QuoteDigits − FromDigits − Scale) in quote minor units, rounded half
// away from zero (kernel.ConvertMinor).
func (r ExchangeRate) convert(amount int64, quote kernel.CurrencyCode) (kernel.Money, error) {
	minor, err := kernel.ConvertMinor(amount, r.Rate, r.Scale, r.FromDigits, r.QuoteDigits)
	if err != nil {
		return kernel.Money{}, err
	}
	return kernel.Money{Currency: quote, Minor: minor}, nil
}

// converter turns line amounts into the quote currency with the quote's frozen rates.
type converter struct {
	quote kernel.CurrencyCode
	rates map[kernel.CurrencyCode]ExchangeRate
}

// newConverter checks a quote's rates: each converts a different currency into the quote currency, and
// all of them agree on the quote currency's minor digits.
func newConverter(quote kernel.CurrencyCode, rates []ExchangeRate) (converter, error) {
	c := converter{quote: quote, rates: map[kernel.CurrencyCode]ExchangeRate{}}
	for _, r := range rates {
		if !r.valid(quote) {
			return converter{}, fmt.Errorf("%w: exchange rate from %q", ErrInvalidQuote, r.From)
		}
		if _, dup := c.rates[r.From]; dup || (len(c.rates) > 0 && r.QuoteDigits != rates[0].QuoteDigits) {
			return converter{}, fmt.Errorf("%w: exchange rates repeat %q or disagree on %s's minor digits", ErrInvalidQuote, r.From, quote)
		}
		c.rates[r.From] = r
	}
	return c, nil
}

// convert returns m in the quote currency.
func (c converter) convert(m kernel.Money) (kernel.Money, error) {
	if m.Currency == c.quote {
		return m, nil
	}
	r, ok := c.rates[m.Currency]
	if !ok {
		return kernel.Money{}, fmt.Errorf("%w: no exchange rate from %s to %s", ErrInvalidQuote, m.Currency, c.quote)
	}
	return r.convert(m.Minor, c.quote)
}
