package domain

import (
	"fmt"
	"math/big"
	"math/bits"
	"strings"
	"unicode/utf8"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// RATE V0.5 part 3a: quote lines entered by hand or rated (R6). A RATED line is a copy of a RateSnapshot line of
// purpose QUOTE_LINE recorded for the quote's scope, legal entity and customer; a RATED_OVERRIDDEN line keeps the rated
// sell and the sell entered, with a reason; a sell overridden below the rated one by more than the legal entity's
// threshold needs a price approval (RATE_CARD_DISCOUNT, R2 option 4, OD-6 A). New lines take codes of the catalogue;
// free-text codes of earlier quotes are kept and marked unlisted.

// LineSource is where a quote line's price comes from.
type LineSource string

const (
	SourceManual          LineSource = "MANUAL"
	SourceRated           LineSource = "RATED"
	SourceRatedOverridden LineSource = "RATED_OVERRIDDEN"
)

// ReasonRateCardDiscount: an overridden sell below the rated sell by more than the threshold.
const ReasonRateCardDiscount ApprovalReason = "RATE_CARD_DISCOUNT"

// Line bases: a lump sum, or a rate per unit of a quantity, so that a per-unit line can be re-evaluated on the quantity
// shipped (OD-7).
const (
	BasisLumpSum = "LUMP_SUM"
	BasisPerUnit = "PER_UNIT"
)

// LineBasis is how a line was priced: a lump sum, or Rate per Unit on Quantity base units. A line is priced per unit only
// when it is RATED from a PER_UNIT snapshot line that no minimum or maximum bounded, so that its rate on its quantity is
// its sell; a manual line, an overridden one and one a minimum or maximum bounded are lump sums.
type LineBasis struct {
	Kind     string
	Unit     RateUnit
	Rate     UnitRate
	Quantity int64
}

// RatedRef is what a rated line keeps of its snapshot: the snapshot, the SELL line (and the BUY line of the same
// charge code when it priced the cost), the rated sell, and for an override its reason.
type RatedRef struct {
	SnapshotID     string
	SellLineNo     int
	CostLineNo     int
	RatedSell      kernel.Money
	OverrideReason string
}

// QuoteScope is the lane and cargo a quote is priced for: required when a line is rated.
type QuoteScope struct {
	Mode          Mode
	ContainerMode ContainerMode
	Origin        string
	Destination   string
	Via           string
	Carrier       string
	Cargo         Cargo
}

func (s QuoteScope) validate() error {
	switch {
	case !(s.Mode == ModeAir && s.ContainerMode == ContainerLoose) && !(s.Mode == ModeSea && s.ContainerMode == ContainerLCL):
		return fmt.Errorf("%w: a scope is AIR LOOSE or SEA LCL", ErrInvalidQuote)
	case !locode(s.Origin) || !locode(s.Destination) || (s.Via != "" && !locode(s.Via)) || s.Origin == s.Destination:
		return fmt.Errorf("%w: a scope names an origin, a destination and an optional via", ErrInvalidQuote)
	case s.Carrier != "" && !identifier(s.Carrier):
		return fmt.Errorf("%w: carrier %q", ErrInvalidQuote, s.Carrier)
	case s.Cargo.Pieces < 0 || s.Cargo.GrossGrams < 0 || s.Cargo.VolumeCM3 < 0 || s.Cargo.Documents < 0 || s.Cargo.GrossGrams > MaxQuantity ||
		s.Cargo.VolumeCM3 > MaxVolume || s.Cargo.Pieces > MaxCount || s.Cargo.Documents > MaxCount:
		return fmt.Errorf("%w: the scope's cargo", ErrInvalidQuote)
	}
	return nil
}

// source reads a line stored before line sources existed as MANUAL.
func (l Line) source() LineSource {
	if l.Source == "" {
		return SourceManual
	}
	return l.Source
}

func (l Line) validateSource(scope *QuoteScope) error {
	switch l.source() {
	case SourceManual:
		if l.Rated != nil {
			return fmt.Errorf("%w: a manual line %s names no snapshot", ErrInvalidQuote, l.ChargeCode)
		}
	case SourceRated, SourceRatedOverridden:
		r := l.Rated
		switch {
		case scope == nil:
			return fmt.Errorf("%w: a rated line needs the quote's scope", ErrInvalidQuote)
		case r == nil || !identifier(r.SnapshotID) || r.SellLineNo < 1 || r.CostLineNo < 0 || !r.RatedSell.Currency.Valid() || r.RatedSell.Minor < 0:
			return fmt.Errorf("%w: a rated line %s names its snapshot line and the rated sell", ErrInvalidQuote, l.ChargeCode)
		case l.Source == SourceRated && (l.Sell != r.RatedSell || r.OverrideReason != ""):
			return fmt.Errorf("%w: a RATED line %s sells what was rated", ErrInvalidQuote, l.ChargeCode)
		case l.Source == SourceRatedOverridden && (l.Sell.Currency != r.RatedSell.Currency || l.Sell == r.RatedSell || l.Sell.Minor < 0):
			return fmt.Errorf("%w: an overridden line %s sells another amount in the rated currency, not below zero", ErrInvalidQuote, l.ChargeCode)
		case l.Source == SourceRatedOverridden && (strings.TrimSpace(r.OverrideReason) == "" || utf8.RuneCountInString(r.OverrideReason) > 500):
			return fmt.Errorf("%w: an overridden line %s gives a reason of 1 to 500 characters", ErrInvalidQuote, l.ChargeCode)
		}
	default:
		return fmt.Errorf("%w: line source %q", ErrInvalidQuote, l.Source)
	}
	switch b := l.Basis; b.Kind {
	case "", BasisLumpSum:
		if b.Unit != UnitNone || b.Rate != (UnitRate{}) || b.Quantity != 0 {
			return fmt.Errorf("%w: a lump sum has no unit", ErrInvalidQuote)
		}
	case BasisPerUnit:
		switch amount, ok := b.amount(); {
		case l.Source != SourceRated:
			return fmt.Errorf("%w: only a RATED line %s is priced per unit; a manual or overridden one is a lump sum", ErrInvalidQuote, l.ChargeCode)
		case !b.Unit.Valid() || b.Rate.Minor < 0 || b.Rate.Scale < 0 || b.Rate.Scale > 6 || b.Quantity < 1:
			return fmt.Errorf("%w: a per-unit basis names its unit, rate and quantity", ErrInvalidQuote)
		case !ok || amount != l.Sell.Minor:
			return fmt.Errorf("%w: the per-unit basis of %s does not reproduce its sell", ErrInvalidQuote, l.ChargeCode)
		}
	default:
		return fmt.Errorf("%w: line basis %q", ErrInvalidQuote, b.Kind)
	}
	switch v := l.CodeVersion; {
	case v != nil && *v < 0:
		return fmt.Errorf("%w: code version %d", ErrInvalidQuote, *v)
	case l.CodeUnlisted && v != nil:
		return fmt.Errorf("%w: an unlisted code has no catalogue version", ErrInvalidQuote)
	}
	return nil
}

// amount is what a per-unit basis prices: Rate per Unit on Quantity base units, rounded half away from zero once, as
// RATE rates a PER_UNIT line that no minimum or maximum bounds.
func (b LineBasis) amount() (int64, bool) {
	den := new(big.Int).Mul(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(b.Rate.Scale)), nil), big.NewInt(b.Unit.Base()))
	num := new(big.Int).Mul(big.NewInt(b.Rate.Minor), big.NewInt(b.Quantity))
	rounded := new(big.Int).Quo(new(big.Int).Add(new(big.Int).Mul(num, big.NewInt(2)), den), new(big.Int).Mul(den, big.NewInt(2)))
	return rounded.Int64(), rounded.IsInt64()
}

// rateCardDiscount reports whether an overridden line sells below its rated sell by more than thresholdBP basis points
// of it, compared exactly as (rated − sell) × 10000 > threshold × rated.
func (l Line) rateCardDiscount(thresholdBP int) bool {
	if l.source() != SourceRatedOverridden || l.Rated == nil || l.Sell.Minor >= l.Rated.RatedSell.Minor {
		return false
	}
	hiCut, loCut := bits.Mul64(uint64(l.Rated.RatedSell.Minor-l.Sell.Minor), 10000)
	hiMax, loMax := bits.Mul64(uint64(thresholdBP), uint64(l.Rated.RatedSell.Minor))
	return hiCut > hiMax || (hiCut == hiMax && loCut > loMax)
}
