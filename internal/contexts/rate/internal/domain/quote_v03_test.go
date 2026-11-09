package domain

import (
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

func cny(minor int64) kernel.Money { return kernel.Money{Currency: "CNY", Minor: minor} }

func jpy(minor int64) kernel.Money { return kernel.Money{Currency: "JPY", Minor: minor} }

// usdToCNY is 1 USD = 7.1234 CNY.
var usdToCNY = ExchangeRate{From: "USD", Rate: 71234, Scale: 4, FromDigits: 2, QuoteDigits: 2, Source: "Bank of China selling rate", AsOf: at}

// R8: ocean freight in USD and local charges in CNY, totalled in CNY at the frozen rate.
func TestLinesInSeveralCurrenciesTotalInTheQuoteCurrency(t *testing.T) {
	d := draft(1000,
		Line{ChargeCode: "OCEAN_FREIGHT", Sell: usd(150_000), Cost: usd(120_000)}, // 1 500.00 USD sell, 1 200.00 USD cost
		Line{ChargeCode: "THC", Sell: cny(80_000), Cost: cny(60_000)},             // 800.00 CNY
	)
	d.Currency, d.ExchangeRates = "CNY", []ExchangeRate{usdToCNY}
	q, err := PriceQuote(d, at)
	if err != nil {
		t.Fatal(err)
	}
	sell, cost, err := q.Totals()
	// 1 500.00 USD = 10 685.10 CNY; 1 200.00 USD = 8 548.08 CNY.
	if err != nil || sell != cny(1_068_510+80_000) || cost != cny(854_808+60_000) {
		t.Fatalf("totals %+v %+v, %v", sell, cost, err)
	}
	converted, err := q.Converted()
	if err != nil || !reflect.DeepEqual(converted, []kernel.Money{cny(1_068_510), cny(80_000)}) {
		t.Fatalf("converted %+v, %v", converted, err)
	}
	if q.Status() != QuotePriced {
		t.Fatalf("a margin of 20.3%% meets a 10%% floor: %s", q.Status())
	}
}

func TestConversionRoundsHalfAwayFromZeroAndRespectsMinorDigits(t *testing.T) {
	for name, tc := range map[string]struct {
		rate   ExchangeRate
		amount int64
		want   int64
	}{
		"1.00 USD at 7.1234 is 7.12 CNY":           {usdToCNY, 100, 712},
		"0.07 USD at 7.1234 is 0.4986 → 0.50 CNY":  {usdToCNY, 7, 50},
		"0.01 USD at 7.1234 is 0.0712 → 0.07 CNY":  {usdToCNY, 1, 7},
		"a half rounds up":                         {ExchangeRate{From: "USD", Rate: 5, Scale: 1, FromDigits: 2, QuoteDigits: 2}, 1, 1},
		"a negative half rounds away from zero":    {ExchangeRate{From: "USD", Rate: 5, Scale: 1, FromDigits: 2, QuoteDigits: 2}, -1, -1},
		"1000 JPY (no minor digits) at 0.0485 CNY": {ExchangeRate{From: "JPY", Rate: 485, Scale: 4, FromDigits: 0, QuoteDigits: 2}, 1000, 4850},
		"1.000 KWD (3 digits) at 23.15 CNY":        {ExchangeRate{From: "KWD", Rate: 2315, Scale: 2, FromDigits: 3, QuoteDigits: 2}, 1000, 2315},
	} {
		got, err := tc.rate.convert(tc.amount, "CNY")
		if err != nil || got != cny(tc.want) {
			t.Errorf("%s: %+v, %v; want %d", name, got, err, tc.want)
		}
	}
	if _, err := usdToCNY.convert(math.MaxInt64, "CNY"); !errors.Is(err, kernel.ErrAmountOverflow) {
		t.Errorf("overflow: err = %v", err)
	}
}

func TestExchangeRatesMustBeCompleteAndUsed(t *testing.T) {
	base := func() QuoteDraft {
		d := draft(0, Line{ChargeCode: "OCEAN_FREIGHT", Sell: usd(100), Cost: usd(50)}, Line{ChargeCode: "THC", Sell: cny(100), Cost: cny(50)})
		d.Currency, d.ExchangeRates = "CNY", []ExchangeRate{usdToCNY}
		return d
	}
	for name, edit := range map[string]func(*QuoteDraft){
		"no rate for USD": func(d *QuoteDraft) { d.ExchangeRates = nil },
		"a rate no line uses": func(d *QuoteDraft) {
			d.ExchangeRates = append(d.ExchangeRates, ExchangeRate{From: "JPY", Rate: 485, Scale: 4, QuoteDigits: 2, Source: "x", AsOf: at})
		},
		"a rate repeated":             func(d *QuoteDraft) { d.ExchangeRates = append(d.ExchangeRates, usdToCNY) },
		"a rate into itself":          func(d *QuoteDraft) { d.ExchangeRates[0].From = "CNY" },
		"a zero rate":                 func(d *QuoteDraft) { d.ExchangeRates[0].Rate = 0 },
		"a rate without a source":     func(d *QuoteDraft) { d.ExchangeRates[0].Source = " " },
		"a rate without a date":       func(d *QuoteDraft) { d.ExchangeRates[0].AsOf = time.Time{} },
		"five minor digits":           func(d *QuoteDraft) { d.ExchangeRates[0].FromDigits = 5 },
		"cost in a currency, no rate": func(d *QuoteDraft) { d.Lines[1].Cost = kernel.Money{Currency: "EUR", Minor: 1} },
	} {
		d := base()
		edit(&d)
		if _, err := PriceQuote(d, at); !errors.Is(err, ErrInvalidQuote) {
			t.Errorf("%s: err = %v, want %v", name, err, ErrInvalidQuote)
		}
	}
	if _, err := PriceQuote(base(), at); err != nil {
		t.Fatalf("the base draft is valid: %v", err)
	}
}

// R9: a charge included elsewhere is a zero line; a discount is a negative line without cost, and needs a
// price approval even when the margin meets the floor.
func TestZeroAndDiscountLines(t *testing.T) {
	included := priced(t, 1000, freight(100_000, 50_000), Line{ChargeCode: "CUSTOMS_DOCS", Sell: usd(0), Cost: usd(0)})
	if included.Status() != QuotePriced {
		t.Fatalf("an included charge needs no approval: %s", included.Status())
	}
	q := priced(t, 1000, freight(100_000, 50_000), Line{ChargeCode: "LOYALTY_DISCOUNT", Sell: usd(-5_000), Cost: usd(0)})
	reasons, err := q.ApprovalReasons()
	sell, _, _ := q.Totals()
	if err != nil || q.Status() != QuoteAwaitingApproval || !reflect.DeepEqual(reasons, []ApprovalReason{ReasonDiscount}) || sell != usd(95_000) {
		t.Fatalf("discount: status %s, reasons %v, sell %+v, %v", q.Status(), reasons, sell, err)
	}
	both := priced(t, 6000, freight(100_000, 50_000), Line{ChargeCode: "LOYALTY_DISCOUNT", Sell: usd(-5_000), Cost: usd(0)})
	if reasons, _ := both.ApprovalReasons(); !reflect.DeepEqual(reasons, []ApprovalReason{ReasonBelowMarginFloor, ReasonDiscount}) {
		t.Fatalf("both reasons: %v", reasons)
	}
	if _, err := PriceQuote(draft(0, Line{ChargeCode: "INCLUDED", Sell: usd(0), Cost: usd(0)}), at); err != nil {
		t.Fatalf("a quote of included charges only totals zero, which is not below zero: %v", err)
	}
}

// R3: the author may approve their own quote only with a designated role, and the approval says so.
func TestSelfApprovalNeedsADesignatedRoleAndIsFlagged(t *testing.T) {
	q := priced(t, 2000, freight(100_000, 90_000))
	if err := q.ApprovePrice("sales-1", "margin-exception-1", false, at); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("without the role: err = %v", err)
	}
	if err := q.ApprovePrice("sales-1", "margin-exception-1", true, at); err != nil {
		t.Fatal(err)
	}
	if a := q.State().Approval; a == nil || !a.SelfApproved || a.ApprovedBy != "sales-1" {
		t.Fatalf("approval %+v", a)
	}
	other := priced(t, 2000, freight(100_000, 90_000))
	if err := other.ApprovePrice("manager-1", "margin-exception-2", true, at); err != nil || other.State().Approval.SelfApproved {
		t.Fatalf("another approver is never flagged: %+v, %v", other.State().Approval, err)
	}
}

// R4: a quote that was not accepted expires once its validity has passed, and then changes no more.
func TestAQuoteExpiresAfterItsValidity(t *testing.T) {
	q := priced(t, 1000, freight(100_000, 90_000))
	until := q.State().Validity.Until
	if err := q.Expire(until.Add(-time.Second)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("expire while valid: err = %v", err)
	}
	if err := q.Expire(until); err != nil {
		t.Fatal(err)
	}
	if q.Status() != QuoteExpired || !q.State().ExpiredAt.Equal(until) || q.Pending()[1].Type != "QuoteExpired.v1" {
		t.Fatalf("after expiry: %+v, events %+v", q.State(), q.Pending())
	}
	for name, err := range map[string]error{
		"expire again": q.Expire(until),
		"accept":       q.Accept("sales-1", EvidenceEmail, "late", 2, until.Add(-time.Hour), until),
		"approve":      q.ApprovePrice("manager-1", "late", false, until),
	} {
		if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s after expiry: err = %v", name, err)
		}
	}
	waiting := priced(t, 2000, freight(100_000, 90_000))
	if err := waiting.Expire(until); err != nil || waiting.Status() != QuoteExpired {
		t.Fatalf("a quote awaiting approval expires too: %s, %v", waiting.Status(), err)
	}
	if _, err := RestoreQuote(waiting.State()); err != nil {
		t.Fatalf("an expired quote restores: %v", err)
	}
	accepted := priced(t, 1000, freight(100_000, 90_000))
	if err := errors.Join(accepted.Accept("sales-1", EvidenceCustomerPortal, "portal-1", 2, at, at), accepted.Expire(until)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("an accepted quote never expires: err = %v", err)
	}
}

// R5: an acceptance names its evidence kind; UNRECORDED marks only quotes accepted before kinds existed.
func TestAcceptanceNeedsAnEvidenceKind(t *testing.T) {
	q := priced(t, 1000, freight(100_000, 90_000))
	for _, kind := range []EvidenceKind{"", EvidenceUnrecorded, "PHONE_CALL"} {
		if err := q.Accept("sales-1", kind, "ref", 2, at, at); !errors.Is(err, ErrInvalidQuote) {
			t.Errorf("kind %q: err = %v", kind, err)
		}
	}
	if err := q.Accept("sales-1", EvidenceEmail, "mail-1", 2, at, time.Time{}); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("without a recording time: err = %v", err)
	}
	legacy := q.State()
	legacy.Status = QuoteAccepted
	legacy.Acceptance = &Acceptance{AcceptedBy: "sales-1", AcceptedAt: at, EvidenceKind: EvidenceUnrecorded, EvidenceRef: "signed", CustomerRevision: 2}
	if _, err := RestoreQuote(legacy); err != nil {
		t.Fatalf("a quote accepted before evidence kinds restores: %v", err)
	}
	legacy.Acceptance.RecordedAt = at
	if _, err := RestoreQuote(legacy); !errors.Is(err, ErrInvalidQuote) {
		t.Fatalf("UNRECORDED with a recording time: err = %v", err)
	}
}

// C8: Incoterms with their edition and named place, and a service level, are optional on a quote.
func TestCommercialTerms(t *testing.T) {
	for name, tc := range map[string]struct {
		terms CommercialTerms
		ok    bool
	}{
		"none":                          {CommercialTerms{}, true},
		"FOB 2020 Shanghai":             {CommercialTerms{IncotermsRule: "FOB", IncotermsVersion: "2020", NamedPlace: "Shanghai port"}, true},
		"DAP 2020 with a service level": {CommercialTerms{IncotermsRule: "DAP", IncotermsVersion: "2020", NamedPlace: "Hamburg", ServiceLevel: "STANDARD"}, true},
		"a service level alone":         {CommercialTerms{ServiceLevel: "EXPRESS"}, true},
		"DAT in 2010":                   {CommercialTerms{IncotermsRule: "DAT", IncotermsVersion: "2010", NamedPlace: "Rotterdam terminal"}, true},
		"DAT in 2020":                   {CommercialTerms{IncotermsRule: "DAT", IncotermsVersion: "2020", NamedPlace: "Rotterdam terminal"}, false},
		"DPU in 2010":                   {CommercialTerms{IncotermsRule: "DPU", IncotermsVersion: "2010", NamedPlace: "Rotterdam terminal"}, false},
		"no named place":                {CommercialTerms{IncotermsRule: "FOB", IncotermsVersion: "2020"}, false},
		"no edition":                    {CommercialTerms{IncotermsRule: "FOB", NamedPlace: "Shanghai port"}, false},
		"a blank service level":         {CommercialTerms{ServiceLevel: " "}, false},
		"an unlisted code":              {CommercialTerms{ServiceLevel: "PRIORITY"}, false},
		"free text":                     {CommercialTerms{ServiceLevel: "next flight out"}, false},
		"a legacy unlisted level":       {CommercialTerms{ServiceLevel: "next flight out", ServiceLevelUnlisted: true}, false},
	} {
		d := draft(1000, freight(100_000, 90_000))
		d.Terms = tc.terms
		if _, err := PriceQuote(d, at); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok %v", name, err, tc.ok)
		}
	}
}

func TestRestoreChecksTheV03State(t *testing.T) {
	q := priced(t, 2000, freight(100_000, 90_000))
	if err := q.ApprovePrice("sales-1", "exception", true, at); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*QuoteState){
		"self-approval flag without self":   func(s *QuoteState) { s.Approval.ApprovedBy = "manager-1" },
		"expired without an expiry time":    func(s *QuoteState) { s.Status = QuoteExpired },
		"expired before the validity ended": func(s *QuoteState) { s.Status, s.ExpiredAt = QuoteExpired, at },
		"an expiry time on a priced quote":  func(s *QuoteState) { s.ExpiredAt = s.Validity.Until },
	} {
		s := q.State()
		edit(&s)
		if _, err := RestoreQuote(s); !errors.Is(err, ErrInvalidQuote) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// A service level recorded as free text before the list existed is kept when the quote is restored, but no
// new quote takes one (C8, amended 2026-11-09).
func TestUnlistedServiceLevelsAreKeptButNotAcceptedAnew(t *testing.T) {
	q := priced(t, 1000, freight(100_000, 90_000))
	s := q.State()
	s.Terms = CommercialTerms{ServiceLevel: "next flight out", ServiceLevelUnlisted: true}
	if _, err := RestoreQuote(s); err != nil {
		t.Fatalf("restore with an unlisted service level: %v", err)
	}
	s.Terms = CommercialTerms{ServiceLevel: "next flight out"}
	if _, err := RestoreQuote(s); err == nil {
		t.Fatal("restored free text that is not marked unlisted")
	}
	s.Terms = CommercialTerms{ServiceLevelUnlisted: true}
	if _, err := RestoreQuote(s); err == nil {
		t.Fatal("restored an unlisted mark without a service level")
	}
	for _, l := range DefaultServiceLevels {
		if !ServiceLevelCode(l.Code) {
			t.Errorf("listed code %q does not have the form of a code", l.Code)
		}
		if got, ok := ListedServiceLevel(l.Code); !ok || got != l {
			t.Errorf("ListedServiceLevel(%q) = %v, %v", l.Code, got, ok)
		}
	}
	for _, code := range []string{"", "S", "standard", "1STANDARD", "STANDARD-1", "ABCDEFGHIJKLMNOPQ"} {
		if ServiceLevelCode(code) {
			t.Errorf("%q has the form of a code", code)
		}
	}
}
