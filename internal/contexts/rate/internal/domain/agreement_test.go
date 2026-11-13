package domain

import (
	"errors"
	"slices"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

func agreementDraft() QuoteDraft {
	d := draft(2000)
	d.Currency, d.Kind, d.Agreement = "CNY", KindAgreement, &AgreementRef{RateCardID: "C1", Version: 1}
	d.Scope = &QuoteScope{Mode: ModeAir, ContainerMode: ContainerLoose}
	return d
}

// customerCard is P's CUSTOMER card with a DRAFT version 1 over the draft's validity holding the entries.
func customerCard(t *testing.T, d QuoteDraft, entries ...RateEntry) *RateCard {
	t.Helper()
	c, err := NewRateCard(RateCardDraft{TenantID: "T", RateCardID: "C1", Side: SideSell, Scope: ScopeCustomer, LegalEntityID: "LE", PartyID: "P",
		PartyRevision: 2, Name: "P agreement"}, "sales-1", at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.DraftVersion(VersionInput{Validity: VersionValidity{From: d.Validity.From, Until: d.Validity.Until}, Currency: "CNY"}, entries,
		"sales-1", at); err != nil {
		t.Fatal(err)
	}
	return c
}

func buyCard(id RateCardID, lines ...RateLine) PublishedVersion {
	v := version(id, ScopeSupplier, "coloader-"+string(id), airEntry(1, lines...))
	v.Validity.From = at.Add(-time.Hour)
	return v
}

func perKg(currency kernel.CurrencyCode, minor int64) RateLine {
	return RateLine{No: 1, ChargeCode: "AIR_FREIGHT", Currency: currency, Calculator: CalcPerUnit, Unit: UnitKgChargeable, Rate: UnitRate{Minor: minor},
		Step: 1, VolumeFactor: 6000}
}

// AGREEMENT quotes (RATE V0.5 part 3b): the version an agreement takes, the margin check of each lane at 100 kg against
// the buy rates (OD-4), the approval reason, and the publication by acceptance.
func TestAgreements(t *testing.T) {
	d := agreementDraft()
	card := customerCard(t, d, airEntry(1, airFreight(1)))
	v, err := card.AgreementVersion(1, "LE", "P", d.Validity, *d.Scope, catalogue{})
	if err != nil || len(v.Entries) != 1 {
		t.Fatalf("the agreement's version: %v %v", v, err)
	}
	for name, try := range map[string]func() error{
		"another customer": func() error {
			_, err := card.AgreementVersion(1, "LE", "Q", d.Validity, *d.Scope, catalogue{})
			return err
		},
		"another validity": func() error {
			_, err := card.AgreementVersion(1, "LE", "P", kernel.ValidityInterval{From: d.Validity.From, Until: d.Validity.Until.Add(time.Hour)},
				*d.Scope, catalogue{})
			return err
		},
		"another mode": func() error {
			_, err := card.AgreementVersion(1, "LE", "P", d.Validity, QuoteScope{Mode: ModeSea, ContainerMode: ContainerLCL}, catalogue{})
			return err
		},
	} {
		if err := try(); !errors.Is(err, ErrInvalidQuote) {
			t.Errorf("%s: %v", name, err)
		}
	}
	e := v.Entries[0]
	st := card.State()
	usdRate := ExchangeRate{From: "USD", Rate: 72000, Scale: 4, FromDigits: 2, QuoteDigits: 2, Source: "Bank of China selling rate", AsOf: at}
	// 100 kg sells at 20.00/kg = 2,000.00
	for name, tc := range map[string]struct {
		buys          []PublishedVersion
		rates         []ExchangeRate
		cost          int64
		rated, failed bool
	}{
		"the lowest buy rate":                  {buys: []PublishedVersion{buyCard("S1", perKg("CNY", 1500)), buyCard("S2", perKg("CNY", 1200))}, cost: 120_000, rated: true},
		"a USD buy rate":                       {buys: []PublishedVersion{buyCard("S1", perKg("USD", 200))}, rates: []ExchangeRate{usdRate}, cost: 144_000, rated: true},
		"no buy rate":                          {},
		"a buy rate not in full":               {buys: []PublishedVersion{buyCard("S1", RateLine{No: 1, ChargeCode: "AIR_FREIGHT", Currency: "CNY", Calculator: CalcFlat, CallForPricing: true})}},
		"a USD rate without its exchange rate": {buys: []PublishedVersion{buyCard("S1", perKg("USD", 200))}, failed: true},
		"a tariff is no buy rate":              {buys: []PublishedVersion{version("T1", ScopeTariff, "", airEntry(1, perKg("CNY", 100)))}},
	} {
		q := d
		q.ExchangeRates = tc.rates
		check, err := CheckAgreementEntry(q, st, v, e, tc.buys, at)
		switch {
		case tc.failed:
			if !errors.Is(err, ErrInvalidQuote) {
				t.Errorf("%s: %v", name, err)
			}
		case err != nil || check.Sell != cny(200_000) || check.Cost != cny(tc.cost) || check.CostRated != tc.rated || !check.At.Equal(at) || check.EntryNo != 1:
			t.Errorf("%s: %+v %v", name, check, err)
		}
	}
	// a lane that starts later is checked when it starts
	later := e
	later.Validity = kernel.ValidityInterval{From: at.Add(48 * time.Hour)}
	if when := AgreementCheckTime(v, later, at); !when.Equal(at.Add(48 * time.Hour)) {
		t.Errorf("a later lane: %s", when)
	}

	// the floor of 20%: exactly 20% passes, below it or without a cost rated needs approval
	for name, tc := range map[string]struct {
		check  AgreementCheck
		status QuoteStatus
	}{
		"at the floor":    {AgreementCheck{EntryNo: 1, At: at, Sell: cny(100_000), Cost: cny(80_000), CostRated: true}, QuotePriced},
		"below the floor": {AgreementCheck{EntryNo: 1, At: at, Sell: cny(100_000), Cost: cny(80_001), CostRated: true}, QuoteAwaitingApproval},
		"no cost rated":   {AgreementCheck{EntryNo: 1, At: at, Sell: cny(100_000), Cost: cny(0)}, QuoteAwaitingApproval},
		"no sell":         {AgreementCheck{EntryNo: 1, At: at, Sell: cny(0), Cost: cny(0), CostRated: true}, QuoteAwaitingApproval},
		"above the sell":  {AgreementCheck{EntryNo: 1, At: at, Sell: cny(100_000), Cost: cny(150_000), CostRated: true}, QuoteAwaitingApproval},
	} {
		q := d
		q.AgreementChecks = []AgreementCheck{tc.check}
		quote, err := PriceQuote(q, at)
		if err != nil || quote.Status() != tc.status || quote.Kind() != KindAgreement {
			t.Errorf("%s: %v %v", name, quote, err)
			continue
		}
		reasons, _ := quote.ApprovalReasons()
		if (tc.status == QuoteAwaitingApproval) != slices.Equal(reasons, []ApprovalReason{ReasonAgreementBelowFloor}) {
			t.Errorf("%s: reasons %v", name, reasons)
		}
		if _, err = RestoreQuote(quote.State()); err != nil {
			t.Errorf("%s: restored: %v", name, err)
		}
	}
	ok := AgreementCheck{EntryNo: 1, At: at, Sell: cny(100_000), Cost: cny(50_000), CostRated: true}
	for name, edit := range map[string]func(*QuoteDraft){
		"lines": func(q *QuoteDraft) { q.Lines = []Line{freight(1, 0)} },
		"a lane in scope": func(q *QuoteDraft) {
			q.Scope = &QuoteScope{Mode: ModeAir, ContainerMode: ContainerLoose, Origin: "CNPVG", Destination: "DEFRA"}
		},
		"no scope":          func(q *QuoteDraft) { q.Scope = nil },
		"no version":        func(q *QuoteDraft) { q.Agreement = nil },
		"no checks":         func(q *QuoteDraft) { q.AgreementChecks = nil },
		"a lane twice":      func(q *QuoteDraft) { q.AgreementChecks = []AgreementCheck{ok, ok} },
		"another currency":  func(q *QuoteDraft) { c := ok; c.Sell = usd(1); q.AgreementChecks = []AgreementCheck{c} },
		"a cost not rated":  func(q *QuoteDraft) { c := ok; c.CostRated = false; q.AgreementChecks = []AgreementCheck{c} },
		"a ONE_OFF's check": func(q *QuoteDraft) { q.Kind, q.Lines = KindOneOff, []Line{freight(1, 0)} },
		"another kind":      func(q *QuoteDraft) { q.Kind = "FRAMEWORK" },
	} {
		q := d
		q.AgreementChecks = []AgreementCheck{ok}
		edit(&q)
		if _, err := PriceQuote(q, at); !errors.Is(err, ErrInvalidQuote) {
			t.Errorf("%s: %v", name, err)
		}
	}

	// accepting publishes the version, once, over the quote's validity, and not over another version
	if err = card.PublishAgreement(1, "Q-1", kernel.ValidityInterval{From: d.Validity.From, Until: d.Validity.Until.Add(time.Hour)}, catalogue{},
		"sales-2", at); !errors.Is(err, ErrNotPublishable) {
		t.Fatalf("another validity: %v", err)
	}
	if err = card.PublishAgreement(1, "Q-1", d.Validity, catalogue{}, "sales-2", at); err != nil {
		t.Fatal(err)
	}
	if v := card.State().Versions[0]; v.Status != VersionPublished || v.PublishedBy != "sales-2" || v.ApprovedBy != "" || len(card.Pending()) != 1 {
		t.Fatalf("published: %+v", v)
	}
	if err = card.PublishAgreement(1, "Q-1", d.Validity, catalogue{}, "sales-2", at); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("twice: %v", err)
	}
	if _, err = card.DraftVersion(VersionInput{Validity: VersionValidity{From: d.Validity.From.Add(24 * time.Hour), Until: d.Validity.Until}, Currency: "CNY"},
		[]RateEntry{airEntry(1, airFreight(1))}, "sales-1", at); err != nil {
		t.Fatal(err)
	}
	if err = card.PublishAgreement(2, "Q-2", kernel.ValidityInterval{From: d.Validity.From.Add(24 * time.Hour), Until: d.Validity.Until}, catalogue{},
		"sales-2", at); !errors.Is(err, ErrVersionOverlap) {
		t.Fatalf("over version 1: %v", err)
	}
	if _, err = tariffCard(t).AgreementVersion(1, "LE", "P", d.Validity, *d.Scope, catalogue{}); !errors.Is(err, ErrInvalidQuote) {
		t.Fatalf("a tariff: %v", err)
	}
}
