package domain

import (
	"fmt"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// RATE V0.5 part 3b-2 (N1-RATE OD-7 A): when the cargo shipped differs from what an accepted ONE_OFF quote assumed,
// its per-unit lines are re-evaluated on the quantity shipped with the quote's frozen unit rates, and its lump sums
// are unchanged. A per-unit line takes its quantity from the cargo by the rule of the card line it was rated from (its
// unit, chargeable weight at the line's volume factor, its step and its minimum quantity).

// ShippedLine is a quote line on the cargo shipped: Sell and Cost as quoted, or, Reevaluated, Quantity base units of
// Unit at the quote's frozen rate (the cost too when its BUY line was priced per unit without a minimum or maximum).
type ShippedLine struct {
	LineNo      int
	ChargeCode  ChargeCode
	Sell        kernel.Money
	Cost        kernel.Money
	Reevaluated bool
	Unit        RateUnit
	Quantity    int64
}

// Shipped is the quantity a card line's rule takes from shipped cargo, in base units: rounded up to its step and at
// least its minimum quantity. It is 0 when the cargo has none of the unit, and for documents, which a shipment's cost
// driver does not report.
func Shipped(l RateLine, c Cargo) int64 {
	if l.Unit == UnitDocument {
		return 0
	}
	q := Quantity(l, c)
	if q <= 0 {
		return 0
	}
	q = ceilDiv(q, max(l.Step, 1)) * max(l.Step, 1)
	return max(q, l.MinQuantity)
}

// atRate is rate per unit on quantity base units in currency, rounded half away from zero once, as RATE rates a
// PER_UNIT line that no minimum or maximum bounds.
func atRate(unit RateUnit, rate UnitRate, quantity int64, currency kernel.CurrencyCode) (kernel.Money, error) {
	amount, ok := LineBasis{Kind: BasisPerUnit, Unit: unit, Rate: rate, Quantity: quantity}.amount()
	if !ok {
		return kernel.Money{}, fmt.Errorf("%w: %d at %d×10^-%d per %s", kernel.ErrAmountOverflow, quantity, rate.Minor, rate.Scale, unit)
	}
	return kernel.Money{Currency: currency, Minor: amount}, nil
}

// RatedRule is what re-evaluating a RATED per-unit line needs of its snapshot: the card line its SELL line was rated
// from, and, when the line's cost came from a BUY line priced per unit without a minimum or maximum, that BUY line's
// card line and frozen rate.
type RatedRule struct {
	Sell     RateLine
	Cost     *RateLine
	CostRate UnitRate
	CostUnit RateUnit
}

// OnShipment re-evaluates an accepted ONE_OFF quote's lines on the cargo shipped (OD-7 A): a PER_UNIT line, with the
// rule of its line number in rules, at its frozen rate on the quantity its card line takes from the cargo, its cost
// likewise when the rule names one; every other line, and a per-unit line whose unit the cargo does not carry, as
// quoted.
func (q *Quote) OnShipment(cargo Cargo, rules map[int]RatedRule) ([]ShippedLine, error) {
	if q.state.kind() != KindOneOff || q.state.Status != QuoteAccepted {
		return nil, fmt.Errorf("%w: only an accepted ONE_OFF quote is re-evaluated on a shipment", ErrInvalidQuote)
	}
	if cargo.Pieces < 0 || cargo.GrossGrams < 0 || cargo.VolumeCM3 < 0 || cargo.GrossGrams > MaxQuantity || cargo.VolumeCM3 > MaxVolume ||
		cargo.Pieces > MaxCount {
		return nil, fmt.Errorf("%w: the cargo shipped", ErrInvalidQuote)
	}
	out := make([]ShippedLine, 0, len(q.state.Lines))
	for i, l := range q.state.Lines {
		s := ShippedLine{LineNo: i + 1, ChargeCode: l.ChargeCode, Sell: l.Sell, Cost: l.Cost}
		if rule, ok := rules[i+1]; ok && l.Basis.Kind == BasisPerUnit {
			if rule.Sell.Unit != l.Basis.Unit {
				return nil, fmt.Errorf("%w: line %d is priced per %s, its card line per %s", ErrInvalidQuote, i+1, l.Basis.Unit, rule.Sell.Unit)
			}
			if qty := Shipped(rule.Sell, cargo); qty > 0 {
				sell, err := atRate(l.Basis.Unit, l.Basis.Rate, qty, l.Sell.Currency)
				if err != nil {
					return nil, err
				}
				s.Sell, s.Reevaluated, s.Unit, s.Quantity = sell, true, l.Basis.Unit, qty
				if rule.Cost != nil {
					if cq := Shipped(*rule.Cost, cargo); cq > 0 {
						if s.Cost, err = atRate(rule.CostUnit, rule.CostRate, cq, l.Cost.Currency); err != nil {
							return nil, err
						}
					}
				}
			}
		}
		out = append(out, s)
	}
	return out, nil
}
