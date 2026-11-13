package domain

import (
	"fmt"
	"math/big"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// RATE V0.5 part 3b-2 (N1-RATE OD-7 A): when the cargo shipped differs from what an accepted ONE_OFF quote assumed,
// its rated per-unit lines are re-evaluated on the quantity shipped with the quote's frozen unit rates, and its lump
// sums are unchanged. The frozen rates are those of the card line each RATED line was rated from, which never changes
// once published: its rate and weight breaks, its quantity rule (unit, chargeable weight at its volume factor, step,
// minimum quantity) and its minimum and maximum. A RATED per-unit line is rated again by that line on the cargo
// shipped, as RATE rated it; a RATED percentage line again on its re-evaluated base. On the cargo quoted each gives
// the quoted line.

// ShippedLine is a quote line on the cargo shipped: Sell and Cost as quoted, or, Reevaluated, as rated on the cargo
// (a per-unit line on Quantity base units of Unit; a percentage line on its re-evaluated base).
type ShippedLine struct {
	LineNo      int
	ChargeCode  ChargeCode
	Sell        kernel.Money
	Cost        kernel.Money
	Reevaluated bool
	Unit        RateUnit
	Quantity    int64
}

// UnitRule is how a PER_UNIT snapshot line is rated again on other cargo: the card line it was rated from.
type UnitRule struct {
	Line RateLine
}

// PercentRule is how a PERCENT_OF snapshot line is rated again: the card line it was rated from (minimum, maximum,
// currency, base code) and its frozen percentage, in basis points, of its base charge.
type PercentRule struct {
	Line RateLine
	BP   int64
}

// RatedRule is what re-evaluating a RATED quote line needs of its snapshot: the rule of its SELL line, and of the BUY
// line its cost came from when it named one; a FLAT line has none.
type RatedRule struct {
	Sell, Cost               *UnitRule
	SellPercent, CostPercent *PercentRule
}

// rated is amount bounded by the card line's minimum and maximum and rounded half away from zero once, as RATE rates.
func rated(l RateLine, amount *big.Rat) (kernel.Money, error) {
	s := SnapshotLine{ChargeCode: l.ChargeCode}
	if err := bound(&s, l, amount); err != nil {
		return kernel.Money{}, err
	}
	return s.Amount, nil
}

// onCargo rates a per-unit rule's card line on the cargo, as RATE rates it. It answers false when the cargo carries
// none of the unit (documents, which a shipment's cost driver does not report, included).
func (r UnitRule) onCargo(c Cargo) (kernel.Money, int64, bool, error) {
	if r.Line.Unit == UnitDocument {
		return kernel.Money{}, 0, false, nil
	}
	s := SnapshotLine{ChargeCode: r.Line.ChargeCode}
	if err := rateLine(&s, r.Line, c); err != nil || s.Gap != GapNone {
		return kernel.Money{}, 0, false, err
	}
	return s.Amount, s.Charged, true, nil
}

// of rates a percentage rule on its base amount.
func (r PercentRule) of(base kernel.Money) (kernel.Money, error) {
	return rated(r.Line, new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(base.Minor), big.NewInt(r.BP)), big.NewInt(10_000)))
}

// OnShipment re-evaluates an accepted ONE_OFF quote's lines on the cargo shipped (OD-7 A), with the rule of each RATED
// line by its line number: a per-unit line as rated on the cargo, its cost likewise when its BUY line was per unit; then
// a percentage line on its base line from the same snapshot, when that line was re-evaluated and is in its currency.
// Manual, overridden and flat lines, and a per-unit line whose unit the cargo does not carry, are as quoted.
func (q *Quote) OnShipment(cargo Cargo, rules map[int]RatedRule) ([]ShippedLine, error) {
	if q.state.kind() != KindOneOff || q.state.Status != QuoteAccepted {
		return nil, fmt.Errorf("%w: only an accepted ONE_OFF quote is re-evaluated on a shipment", ErrInvalidQuote)
	}
	if cargo.Pieces < 0 || cargo.GrossGrams < 0 || cargo.VolumeCM3 < 0 || cargo.Documents < 0 || cargo.GrossGrams > MaxQuantity ||
		cargo.VolumeCM3 > MaxVolume || cargo.Pieces > MaxCount || cargo.Documents > MaxCount {
		return nil, fmt.Errorf("%w: the cargo shipped", ErrInvalidQuote)
	}
	out := make([]ShippedLine, len(q.state.Lines))
	for i, l := range q.state.Lines {
		out[i] = ShippedLine{LineNo: i + 1, ChargeCode: l.ChargeCode, Sell: l.Sell, Cost: l.Cost}
		rule, ok := rules[i+1]
		if !ok || l.Source != SourceRated || rule.Sell == nil {
			continue
		}
		sell, qty, on, err := rule.Sell.onCargo(cargo)
		if err != nil || !on {
			if err != nil {
				return nil, err
			}
			continue
		}
		out[i].Sell, out[i].Reevaluated, out[i].Unit, out[i].Quantity = sell, true, rule.Sell.Line.Unit, qty
		if rule.Cost != nil {
			if cost, _, on, err := rule.Cost.onCargo(cargo); err != nil {
				return nil, err
			} else if on {
				out[i].Cost = cost
			}
		}
	}
	for i, l := range q.state.Lines {
		rule, ok := rules[i+1]
		if !ok || l.Source != SourceRated || rule.SellPercent == nil {
			continue
		}
		for j, b := range q.state.Lines {
			if b.ChargeCode != rule.SellPercent.Line.BaseCode || b.Rated == nil || b.Rated.SnapshotID != l.Rated.SnapshotID || !out[j].Reevaluated ||
				out[j].Sell.Currency != l.Sell.Currency {
				continue
			}
			sell, err := rule.SellPercent.of(out[j].Sell)
			if err != nil {
				return nil, err
			}
			out[i].Sell, out[i].Reevaluated = sell, true
			if p := rule.CostPercent; p != nil && out[j].Cost.Currency == l.Cost.Currency {
				if out[i].Cost, err = p.of(out[j].Cost); err != nil {
					return nil, err
				}
			}
		}
	}
	return out, nil
}
