package domain

import (
	"errors"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

var t0 = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

// airFreight is CNY 30.00/kg below 45 kg, 25.00 from 45 kg and 20.00 from 100 kg, chargeable weight at 6000 cm3/kg
// rounded up to 0.5 kg, minimum 300.00.
func airFreight(no int) RateLine {
	return RateLine{No: no, ChargeCode: "AIR_FREIGHT", Currency: "CNY", Calculator: CalcPerUnit, Unit: UnitKgChargeable,
		Rate: UnitRate{Minor: 3000}, Minimum: 30_000, Step: 500, VolumeFactor: 6000,
		Breaks: []RateBreak{{From: 45_000, Rate: 2500}, {From: 100_000, Rate: 2000}}}
}

func flat(no int, code ChargeCode, currency kernel.CurrencyCode, minor int64) RateLine {
	return RateLine{No: no, ChargeCode: code, Currency: currency, Calculator: CalcFlat, Rate: UnitRate{Minor: minor}}
}

func airEntry(no int, lines ...RateLine) RateEntry {
	return RateEntry{No: no, Mode: ModeAir, ContainerMode: ContainerLoose, Origin: "CNPVG", Destination: "DEFRA", Edition: "2024-2",
		ServiceLevel: "STANDARD", Commodity: CommodityGeneral, Lines: lines}
}

func version(id RateCardID, scope CardScope, party string, entries ...RateEntry) PublishedVersion {
	return PublishedVersion{RateCardID: id, Side: scope.sideOf(), Scope: scope, LegalEntityID: "LE", PartyID: party, No: 1,
		Validity: VersionValidity{From: t0}, Entries: entries}
}

func input(sides ...Side) EvaluationInput {
	return EvaluationInput{Purpose: PurposeQuoteLine, LegalEntityID: "LE", Sides: sides, Mode: ModeAir, ContainerMode: ContainerLoose,
		Origin: "CNPVG", Destination: "DEFRA", ServiceLevel: "STANDARD", Commodity: CommodityGeneral, At: t0.Add(24 * time.Hour),
		Cargo: Cargo{Pieces: 2, GrossGrams: 100_000, VolumeCM3: 900_000, Documents: 1}}
}

func line(t *testing.T, lines []SnapshotLine, side Side, code ChargeCode) SnapshotLine {
	t.Helper()
	for _, l := range lines {
		if l.Side == side && l.ChargeCode == code {
			return l
		}
	}
	t.Fatalf("no %s %s line in %+v", side, code, lines)
	return SnapshotLine{}
}

func TestBreaksMinimumAndChargeableWeight(t *testing.T) {
	tariff := version("T1", ScopeTariff, "", airEntry(1, airFreight(1)))
	for _, tc := range []struct {
		name          string
		gross, volume int64
		qty, charged  int64
		band          int64
		amount        int64
		minimum       bool
	}{
		{"volume weight 150 kg beats 100 kg gross; band from 100 kg", 100_000, 900_000, 150_000, 150_000, 100_000, 300_000, false},
		{"90 kg: the 100 kg band's 2,000.00 beats 90 x 25.00", 90_000, 0, 90_000, 100_000, 100_000, 200_000, false},
		{"40 kg: the 45 kg band's 1,125.00 beats 40 x 30.00", 40_000, 0, 40_000, 45_000, 45_000, 112_500, false},
		{"40.2 kg rounds up to 40.5 kg", 40_200, 0, 40_500, 45_000, 45_000, 112_500, false},
		{"5 kg: 150.00 raised to the minimum", 5_000, 0, 5_000, 5_000, 0, 30_000, true},
		{"120 kg in the top band", 120_000, 0, 120_000, 120_000, 100_000, 240_000, false},
	} {
		in := input(SideSell)
		in.Cargo.GrossGrams, in.Cargo.VolumeCM3 = tc.gross, tc.volume
		_, lines, err := Evaluate(in, []PublishedVersion{tariff})
		if err != nil {
			t.Fatal(err)
		}
		l := line(t, lines, SideSell, "AIR_FREIGHT")
		if l.Quantity != tc.qty || l.Charged != tc.charged || l.BandFrom != tc.band || l.Amount != cny(tc.amount) || l.MinimumApplied != tc.minimum {
			t.Errorf("%s: %+v", tc.name, l)
		}
	}
}

// The charge never falls as the weight rises (OD-3: a higher band is charged when it costs less), and it is never
// more than the band rate on the whole quantity.
func TestBreaksNeverChargeMoreForLess(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 11))
	for round := 0; round < 200; round++ {
		rate := int64(1000 + r.IntN(5000))
		l := RateLine{No: 1, ChargeCode: "AIR_FREIGHT", Currency: "CNY", Calculator: CalcPerUnit, Unit: UnitKgGross, Rate: UnitRate{Minor: rate, Scale: r.IntN(3)},
			Step: int64(1 + r.IntN(1000)), Minimum: int64(r.IntN(50_000))}
		from := int64(0)
		for b := 0; b < 1+r.IntN(5); b++ {
			from += int64(1 + r.IntN(200_000))
			rate = rate * int64(60+r.IntN(40)) / 100
			l.Breaks = append(l.Breaks, RateBreak{From: from, Rate: rate})
		}
		if err := l.check(); err != nil {
			t.Fatal(err)
		}
		prev := int64(-1)
		for g := int64(0); g <= from+100_000; g += int64(1 + r.IntN(3000)) {
			var s SnapshotLine
			if err := rateLine(&s, l, Cargo{GrossGrams: g}); err != nil {
				t.Fatal(err)
			}
			if s.Amount.Minor < prev {
				t.Fatalf("round %d: %d g costs %d, less than %d for less weight; line %+v", round, g, s.Amount.Minor, prev, l)
			}
			prev = s.Amount.Minor
		}
	}
}

func TestRoundingAndScale(t *testing.T) {
	// CNY 123.5 minor units per kg (scale 1) on 1 kg: 123.5 rounds half away from zero to 124
	l := RateLine{No: 1, ChargeCode: "AIR_FREIGHT", Currency: "CNY", Calculator: CalcPerUnit, Unit: UnitKgGross, Rate: UnitRate{Minor: 1235, Scale: 1}, Step: 1}
	var s SnapshotLine
	if err := rateLine(&s, l, Cargo{GrossGrams: 1000}); err != nil || s.Amount != cny(124) {
		t.Fatalf("%+v %v", s, err)
	}
	// 12.355 minor units per kg (12355 at scale 3) on 10.5 kg = 129.7275 -> 130
	l.Rate = UnitRate{Minor: 12355, Scale: 3}
	if err := rateLine(&s, l, Cargo{GrossGrams: 10_500}); err != nil || s.Amount != cny(130) {
		t.Fatalf("%+v %v", s, err)
	}
	// a maximum caps
	l.Rate, l.Maximum = UnitRate{Minor: 1000}, 5_000
	if err := rateLine(&s, l, Cargo{GrossGrams: 10_000}); err != nil || s.Amount != cny(5_000) || !s.MaximumApplied {
		t.Fatalf("%+v %v", s, err)
	}
	// an amount beyond int64 is refused, not wrapped
	l.Rate, l.Maximum = UnitRate{Minor: 1 << 60}, 0
	if err := rateLine(&s, l, Cargo{GrossGrams: 1 << 40}); !errors.Is(err, kernel.ErrAmountOverflow) {
		t.Fatalf("overflow: %v", err)
	}
}

func TestFreightTonnes(t *testing.T) {
	ocean := RateLine{No: 1, ChargeCode: "OCEAN_FREIGHT", Currency: "USD", Calculator: CalcPerUnit, Unit: UnitFreightTonne, Rate: UnitRate{Minor: 4500},
		Step: 1, MinQuantity: 1000, VolumeFactor: 1_000_000}
	for _, tc := range []struct {
		gross, volume, qty, amount int64
	}{
		{800_000, 2_500_000, 2500, 11_250}, // 2.5 cbm beat 0.8 t
		{3_200_000, 2_500_000, 3200, 14_400},
		{100_000, 300_000, 1000, 4_500}, // 0.3 W/M raised to 1 W/M
	} {
		var s SnapshotLine
		if err := rateLine(&s, ocean, Cargo{GrossGrams: tc.gross, VolumeCM3: tc.volume}); err != nil || s.Quantity != tc.qty || s.Amount != usd(tc.amount) {
			t.Errorf("%+v: %+v %v", tc, s, err)
		}
	}
}

func TestSellPrecedenceAndGaps(t *testing.T) {
	customer := version("C-ACME", ScopeCustomer, "acme", airEntry(1,
		RateLine{No: 1, ChargeCode: "AIR_FREIGHT", Currency: "CNY", Calculator: CalcPerUnit, Unit: UnitKgChargeable, Rate: UnitRate{Minor: 1800},
			Step: 500, VolumeFactor: 6000}))
	tariff := version("T1", ScopeTariff, "", airEntry(1, airFreight(1), flat(2, "DOC_FEE", "CNY", 15_000),
		RateLine{No: 3, ChargeCode: "FUEL_SURCHARGE", Currency: "CNY", Calculator: CalcPercentOf, PercentBP: 1000, BaseCode: "AIR_FREIGHT"},
		RateLine{No: 4, ChargeCode: "CUSTOMS_CLEARING", Currency: "CNY", Calculator: CalcFlat, CallForPricing: true},
		RateLine{No: 5, ChargeCode: "SECURITY_CHARGE", Currency: "USD", Calculator: CalcPercentOf, PercentBP: 500, BaseCode: "DOC_FEE"}))
	in := input(SideSell)
	in.Customer = CustomerRef{PartyID: "acme", PartyRevision: 3}
	cands, lines, err := Evaluate(in, []PublishedVersion{tariff, customer})
	if err != nil {
		t.Fatal(err)
	}
	// 150 kg at the customer's 18.00 = 2,700.00; the tariff fills the rest; fuel is 10% of the customer's freight
	if l := line(t, lines, SideSell, "AIR_FREIGHT"); l.RateCardID != "C-ACME" || l.Amount != cny(270_000) {
		t.Errorf("freight: %+v", l)
	}
	if l := line(t, lines, SideSell, "DOC_FEE"); l.RateCardID != "T1" || l.Amount != cny(15_000) {
		t.Errorf("doc fee: %+v", l)
	}
	if l := line(t, lines, SideSell, "FUEL_SURCHARGE"); l.Amount != cny(27_000) || l.BaseAmount != 270_000 {
		t.Errorf("fuel: %+v", l)
	}
	if l := line(t, lines, SideSell, "CUSTOMS_CLEARING"); l.Gap != GapCallForPricing || l.Amount.Minor != 0 {
		t.Errorf("call for pricing: %+v", l)
	}
	if l := line(t, lines, SideSell, "SECURITY_CHARGE"); l.Gap != GapBaseCurrency {
		t.Errorf("a percentage of a charge in another currency: %+v", l)
	}
	if len(cands) != 2 || !cands[0].Taken || cands[0].Reason != "CUSTOMER_CARD" || !cands[1].Taken || cands[1].Reason != "TARIFF" {
		t.Errorf("candidates: %+v", cands)
	}
	// without the customer the tariff prices the freight; another customer's card is not used
	in.Customer = CustomerRef{PartyID: "other", PartyRevision: 1}
	_, lines, _ = Evaluate(in, []PublishedVersion{tariff, customer})
	if l := line(t, lines, SideSell, "AIR_FREIGHT"); l.RateCardID != "T1" || l.Amount != cny(300_000) {
		t.Errorf("tariff freight: %+v", l)
	}
	// a lane nothing serves is a gap, never zero
	in.Destination = "USLAX"
	_, lines, _ = Evaluate(in, []PublishedVersion{tariff, customer})
	if len(lines) != 1 || lines[0].Gap != GapNoRate || lines[0].Side != SideSell {
		t.Errorf("no rate: %+v", lines)
	}
}

func TestSpecificityValidityAndDeterminism(t *testing.T) {
	plain := airEntry(1, flat(1, "HANDLING", "CNY", 10_000))
	via := airEntry(2, flat(1, "HANDLING", "CNY", 20_000))
	via.Via = "AEDXB"
	carrier := airEntry(3, flat(1, "HANDLING", "CNY", 30_000))
	carrier.Carrier = "airline-mu"
	later := airEntry(4, flat(1, "PICKUP", "CNY", 5_000))
	later.Validity = kernel.ValidityInterval{From: t0.Add(48 * time.Hour), Until: t0.Add(96 * time.Hour)}
	tariff := version("T1", ScopeTariff, "", plain, via, carrier, later)
	expired := version("T0", ScopeTariff, "", airEntry(1, flat(1, "DOC_FEE", "CNY", 1)))
	expired.Validity = VersionValidity{From: t0.Add(-48 * time.Hour), Until: t0}

	in := input(SideSell)
	_, lines, _ := Evaluate(in, []PublishedVersion{tariff, expired})
	if len(lines) != 1 || lines[0].Amount != cny(10_000) || lines[0].EntryNo != 1 {
		t.Errorf("plain lane, no expired version, no entry not yet valid: %+v", lines)
	}
	in.Via = "AEDXB"
	_, lines, _ = Evaluate(in, []PublishedVersion{tariff})
	if lines[0].EntryNo != 2 {
		t.Errorf("the entry naming the via wins: %+v", lines)
	}
	in.Via, in.Carrier = "", "airline-mu"
	_, lines, _ = Evaluate(in, []PublishedVersion{tariff})
	if lines[0].EntryNo != 3 {
		t.Errorf("the entry naming the carrier wins: %+v", lines)
	}
	in.At = t0.Add(72 * time.Hour)
	_, lines, _ = Evaluate(in, []PublishedVersion{tariff})
	if len(lines) != 2 || line(t, lines, SideSell, "PICKUP").Amount != cny(5_000) {
		t.Errorf("an entry valid then: %+v", lines)
	}

	// the same input and versions in any order give the same result
	supplierA := version("S-A", ScopeSupplier, "airline-mu", airEntry(1, airFreight(1)))
	supplierB := version("S-B", ScopeSupplier, "airline-ca", airEntry(1, airFreight(1)))
	all := []PublishedVersion{tariff, supplierA, supplierB, expired}
	in = input(SideBuy, SideSell)
	c1, l1, err := Evaluate(in, all)
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 20; i++ {
		shuffled := slices.Clone(all)
		r.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		c2, l2, _ := Evaluate(in, shuffled)
		if !reflect.DeepEqual(c1, c2) || !reflect.DeepEqual(l1, l2) {
			t.Fatalf("not deterministic:\n%+v\n%+v", l1, l2)
		}
	}
}

func TestBuyLowestTotal(t *testing.T) {
	cheap := airFreight(1)
	cheap.Rate.Minor, cheap.Breaks = 2500, []RateBreak{{From: 100_000, Rate: 1500}}
	a := version("S-A", ScopeSupplier, "airline-mu", airEntry(1, airFreight(1)))              // 150 kg at 20.00 = 3,000.00
	b := version("S-B", ScopeSupplier, "airline-ca", airEntry(1, cheap))                      // 150 kg at 15.00 = 2,250.00
	c := version("S-C", ScopeSupplier, "coloader", airEntry(1, airFreight(1), RateLine{No: 2, // incomplete
		ChargeCode: "THC", Currency: "CNY", Calculator: CalcFlat, CallForPricing: true}))
	usdCard := version("S-D", ScopeSupplier, "agent", airEntry(1, flat(1, "AIR_FREIGHT", "USD", 10)))
	in := input(SideBuy)
	cands, lines, err := Evaluate(in, []PublishedVersion{a, b, c})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0].RateCardID != "S-B" || lines[0].Amount != cny(225_000) {
		t.Fatalf("lowest total: %+v", lines)
	}
	for _, x := range cands {
		if (x.RateCardID == "S-B") != x.Taken || (x.Taken && x.Reason != "LOWEST_TOTAL") {
			t.Errorf("candidate %+v", x)
		}
	}
	// the consol's carrier is given: only its cards
	in.Carrier = "airline-mu"
	_, lines, _ = Evaluate(in, []PublishedVersion{a, b, c})
	if lines[0].RateCardID != "S-A" || lines[0].Amount != cny(300_000) {
		t.Errorf("carrier given: %+v", lines)
	}
	// totals in different currencies are not compared: the first card by id
	in.Carrier = ""
	cands, lines, _ = Evaluate(in, []PublishedVersion{usdCard, b})
	if lines[0].RateCardID != "S-B" || cands[0].Reason != "CURRENCIES_DIFFER_FIRST_BY_ID" {
		t.Errorf("currencies differ: %+v %+v", lines, cands)
	}
	// another legal entity's cards are not used
	b.LegalEntityID = "LE2"
	_, lines, _ = Evaluate(in, []PublishedVersion{b})
	if lines[0].Gap != GapNoRate {
		t.Errorf("another legal entity: %+v", lines)
	}
}

func TestEvaluationInputChecks(t *testing.T) {
	for name, mutate := range map[string]func(*EvaluationInput){
		"no side":           func(in *EvaluationInput) { in.Sides = nil },
		"a side twice":      func(in *EvaluationInput) { in.Sides = []Side{SideSell, SideSell} },
		"FCL":               func(in *EvaluationInput) { in.Mode, in.ContainerMode = ModeSea, "FCL" },
		"same ends":         func(in *EvaluationInput) { in.Destination = in.Origin },
		"no business time":  func(in *EvaluationInput) { in.At = time.Time{} },
		"negative weight":   func(in *EvaluationInput) { in.Cargo.GrossGrams = -1 },
		"unconfirmed party": func(in *EvaluationInput) { in.Customer = CustomerRef{PartyID: "acme"} },
		"subject id only":   func(in *EvaluationInput) { in.SubjectID = "Q-1" },
		"other commodity":   func(in *EvaluationInput) { in.Commodity = "DG" },
	} {
		in := input(SideSell)
		mutate(&in)
		if _, _, err := Evaluate(in, nil); !errors.Is(err, ErrInvalidEvaluation) {
			t.Errorf("%s: %v", name, err)
		}
	}
	s, err := RecordSnapshot("t", "RS-1", input(SideBuy, SideSell), nil, "pricing-1", t0)
	if err != nil {
		t.Fatal(err)
	}
	st := s.State()
	if st.EngineVersion != EngineVersion || st.Input.Sides[0] != SideSell || len(st.Lines) != 2 || len(s.Pending()) != 1 ||
		s.Pending()[0].Type != EventRatesEvaluated {
		t.Fatalf("snapshot: %+v", st)
	}
}

func TestIssueBasis(t *testing.T) {
	tariff := version("T1", ScopeTariff, "", airEntry(1, airFreight(1), flat(2, "DOC_FEE", "CNY", 15_000),
		RateLine{No: 3, ChargeCode: "CUSTOMS_CLEARING", Currency: "CNY", Calculator: CalcFlat, CallForPricing: true}))
	supplier := version("S-A", ScopeSupplier, "airline-mu", airEntry(1, airFreight(1), flat(2, "THC", "CNY", 8_000)))
	in := input(SideSell, SideBuy)
	in.Purpose, in.Customer, in.SubjectKind, in.SubjectID, in.SubjectVersion = PurposeShipmentEstimate, CustomerRef{PartyID: "acme", PartyRevision: 2},
		"SHIPMENT", "SH-1", 3
	s, err := RecordSnapshot("t", "B-1", in, []PublishedVersion{tariff, supplier}, "fin-1", t0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := IssueBasis("B-1", s)
	if err != nil {
		t.Fatal(err)
	}
	st := b.State()
	// AIR_FREIGHT sells and costs, DOC_FEE sells, THC costs; customs clearing is a gap
	if len(st.Lines) != 3 || len(st.Gaps) != 1 || st.Gaps[0].Gap != GapCallForPricing || st.SubjectID != "SH-1" || st.Customer.PartyID != "acme" {
		t.Fatalf("basis: %+v", st)
	}
	freight, doc, thc := st.Lines[0], st.Lines[1], st.Lines[2]
	if freight.ChargeCode != "AIR_FREIGHT" || !freight.HasSell || !freight.HasCost || freight.Sell != cny(300_000) || freight.Cost != cny(300_000) ||
		freight.CreditorPartyID != "airline-mu" || freight.SellLineNo == 0 || freight.CostLineNo == 0 {
		t.Errorf("freight: %+v", freight)
	}
	if doc.ChargeCode != "DOC_FEE" || !doc.HasSell || doc.HasCost || doc.CreditorPartyID != "" {
		t.Errorf("doc fee: %+v", doc)
	}
	if thc.ChargeCode != "THC" || thc.HasSell || !thc.HasCost || thc.Cost != cny(8_000) {
		t.Errorf("THC: %+v", thc)
	}
	if p := b.Pending(); len(p) != 1 || p[0].Type != EventPricingBasisIssued {
		t.Errorf("events: %+v", p)
	}
	// a consolidation estimate rates the BUY side only, without a customer
	in = input(SideBuy)
	in.Purpose, in.SubjectKind, in.SubjectID, in.Carrier = PurposeConsolCostEstimate, "CONSOLIDATION", "C-1", "airline-mu"
	s, _ = RecordSnapshot("t", "B-2", in, []PublishedVersion{tariff, supplier}, "fin-1", t0)
	if b, err = IssueBasis("B-2", s); err != nil || len(b.State().Lines) != 2 || b.State().Lines[0].HasSell {
		t.Fatalf("consol basis: %+v %v", b, err)
	}
	// a quote-line snapshot is not a basis
	in = input(SideSell)
	s, _ = RecordSnapshot("t", "B-3", in, nil, "fin-1", t0)
	if _, err = IssueBasis("B-3", s); !errors.Is(err, ErrInvalidBasis) {
		t.Fatalf("a quote-line snapshot: %v", err)
	}
}
