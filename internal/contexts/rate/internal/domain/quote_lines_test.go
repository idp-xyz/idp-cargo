package domain

import (
	"errors"
	"slices"
	"testing"
)

func scopeAir() *QuoteScope {
	return &QuoteScope{Mode: ModeAir, ContainerMode: ContainerLoose, Origin: "CNPVG", Destination: "DEFRA", Cargo: Cargo{Pieces: 2, GrossGrams: 100_000}}
}

// ratedLine is a line rated at 10.00 per chargeable kg on 100 kg, sold at sell: RATED and per unit when sell is the
// rated sell, else overridden as a lump sum.
func ratedLine(code ChargeCode, rated, sell int64, reason string) Line {
	l := Line{ChargeCode: code, Sell: usd(sell), Cost: usd(10_000), Source: SourceRated,
		Rated: &RatedRef{SnapshotID: "RS-1", SellLineNo: 1, RatedSell: usd(rated), OverrideReason: reason},
		Basis: LineBasis{Kind: BasisPerUnit, Unit: UnitKgChargeable, Rate: UnitRate{Minor: rated / 100}, Quantity: 100_000}}
	if sell != rated {
		l.Source, l.Basis = SourceRatedOverridden, LineBasis{Kind: BasisLumpSum}
	}
	return l
}

// Rated and overridden quote lines (RATE V0.5 part 3a).
func TestRatedQuoteLines(t *testing.T) {
	d := draft(0, ratedLine("AIR_FREIGHT", 100_000, 100_000, ""))
	if _, err := PriceQuote(d, at); !errors.Is(err, ErrInvalidQuote) {
		t.Fatalf("a rated line without a scope: %v", err)
	}
	d.Scope = scopeAir()
	q, err := PriceQuote(d, at)
	if err != nil || q.Status() != QuotePriced {
		t.Fatalf("rated: %v %v", q, err)
	}
	// an override 4% below the rated sell passes a 5% threshold; 6% below needs approval
	d.DiscountThresholdBP = 500
	d.Lines = []Line{ratedLine("AIR_FREIGHT", 100_000, 96_000, "long-standing customer")}
	if q, err = PriceQuote(d, at); err != nil || q.Status() != QuotePriced {
		t.Fatalf("within the threshold: %v %v", q, err)
	}
	d.Lines = []Line{ratedLine("AIR_FREIGHT", 100_000, 94_000, "match the competitor")}
	q, err = PriceQuote(d, at)
	if err != nil || q.Status() != QuoteAwaitingApproval {
		t.Fatalf("beyond the threshold: %v %v", q, err)
	}
	if reasons, _ := q.ApprovalReasons(); !slices.Contains(reasons, ReasonRateCardDiscount) {
		t.Fatalf("reasons: %v", reasons)
	}
	// selling above the rated sell needs no approval
	d.Lines = []Line{ratedLine("AIR_FREIGHT", 100_000, 120_000, "peak season")}
	if q, err = PriceQuote(d, at); err != nil || q.Status() != QuotePriced {
		t.Fatalf("above the rated sell: %v %v", q, err)
	}
	for name, l := range map[string]Line{
		"a RATED line selling another amount": func() Line { l := ratedLine("AIR_FREIGHT", 100_000, 100_000, ""); l.Sell = usd(1); return l }(),
		"an override without a reason":        ratedLine("AIR_FREIGHT", 100_000, 90_000, ""),
		"an override in another currency": func() Line {
			l := ratedLine("AIR_FREIGHT", 100_000, 90_000, "x")
			l.Sell.Currency = "CNY"
			return l
		}(),
		"a manual line naming a snapshot": func() Line { l := ratedLine("AIR_FREIGHT", 1, 1, ""); l.Source = SourceManual; return l }(),
		"a per-unit basis without a unit": func() Line { l := ratedLine("AIR_FREIGHT", 100, 100, ""); l.Basis.Unit = UnitNone; return l }(),
		"a per-unit basis that is not the sell": func() Line {
			l := ratedLine("AIR_FREIGHT", 100_000, 100_000, "")
			l.Basis.Quantity = 99_999
			return l
		}(),
		"an override priced per unit": func() Line {
			l := ratedLine("AIR_FREIGHT", 100_000, 100_000, "")
			l.Source, l.Sell, l.Rated.OverrideReason, l.Basis.Quantity = SourceRatedOverridden, usd(90_000), "spot", 90_000
			return l
		}(),
		"a manual line priced per unit": {ChargeCode: "AIR_FREIGHT", Sell: usd(100_000), Cost: usd(0),
			Basis: LineBasis{Kind: BasisPerUnit, Unit: UnitKgChargeable, Rate: UnitRate{Minor: 1000}, Quantity: 100_000}},
		"an override with a blank reason": ratedLine("AIR_FREIGHT", 100_000, 90_000, "\t\u3000"),
		"a negative code version": func() Line {
			l := ratedLine("AIR_FREIGHT", 100_000, 100_000, "")
			v := -1
			l.CodeVersion = &v
			return l
		}(),
		"an unlisted code on a new quote": {ChargeCode: "FREIGHT", Sell: usd(1), Cost: usd(0), CodeUnlisted: true},
		"a code of another form":          {ChargeCode: "freight", Sell: usd(1), Cost: usd(0)},
	} {
		d := draft(0, l)
		d.Scope = scopeAir()
		if _, err := PriceQuote(d, at); !errors.Is(err, ErrInvalidQuote) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// a stored quote of before the catalogue keeps its free-text code
	old := q.State()
	old.Lines = []Line{{ChargeCode: "FREIGHT", Sell: usd(100_000), Cost: usd(50_000), CodeUnlisted: true}}
	old.Scope, old.Status = nil, QuotePriced
	if _, err = RestoreQuote(old); err != nil {
		t.Fatalf("an old quote: %v", err)
	}
}
