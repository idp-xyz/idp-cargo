package domain

import (
	"errors"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

var t0 = time.Date(2026, 11, 11, 8, 0, 0, 0, time.UTC)

func hour(h int) time.Time { return t0.Add(time.Duration(h) * time.Hour) }

func cny(n int64) kernel.Money { return kernel.Money{Currency: "CNY", Minor: n} }
func usd(n int64) kernel.Money { return kernel.Money{Currency: "USD", Minor: n} }

func policy(t *testing.T) *FinancePolicy {
	t.Helper()
	p, err := OpenPolicy("T", "LE", "CNY", "MAIN", "acct-1", hour(0))
	if err != nil {
		t.Fatal(err)
	}
	if err = p.RecordRate(RateEntry{Type: RateEstimate, From: "USD", Rate: 71_234, Scale: 4, FromDigits: 2, ToDigits: 2, ValidFrom: t0.AddDate(0, 0, -1),
		ValidUntil: t0.AddDate(0, 1, 0), Source: "CFETS central parity 2026-11-10"}, "acct-1", hour(0)); err != nil {
		t.Fatal(err)
	}
	return p
}

// Rates: never overlapping per type and currency, never for the functional currency, converted per line half up;
// a missing rate refuses instead of falling back to another type.
func TestFinancePolicyRates(t *testing.T) {
	p := policy(t)
	overlap := RateEntry{Type: RateEstimate, From: "USD", Rate: 71_000, Scale: 4, FromDigits: 2, ToDigits: 2, ValidFrom: t0.AddDate(0, 0, 10),
		ValidUntil: t0.AddDate(0, 2, 0), Source: "bank"}
	if err := p.RecordRate(overlap, "acct-1", hour(1)); !errors.Is(err, ErrRateOverlap) {
		t.Fatalf("overlap: %v", err)
	}
	for name, e := range map[string]RateEntry{
		"open end":         {Type: RateEstimate, From: "EUR", Rate: 78_000, Scale: 4, FromDigits: 2, ToDigits: 2, ValidFrom: t0, Source: "bank"},
		"over a year":      {Type: RateEstimate, From: "EUR", Rate: 78_000, Scale: 4, FromDigits: 2, ToDigits: 2, ValidFrom: t0, ValidUntil: t0.AddDate(1, 1, 0), Source: "bank"},
		"wrong digits":     {Type: RateEstimate, From: "JPY", Rate: 480, Scale: 4, FromDigits: 2, ToDigits: 2, ValidFrom: t0, ValidUntil: t0.AddDate(0, 1, 0), Source: "bank"},
		"unknown currency": {Type: RateEstimate, From: "XXX", Rate: 1, Scale: 0, FromDigits: 2, ToDigits: 2, ValidFrom: t0, ValidUntil: t0.AddDate(0, 1, 0), Source: "bank"},
	} {
		if err := p.RecordRate(e, "acct-1", hour(1)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	accounting := overlap
	accounting.Type = RateAccounting
	if err := p.RecordRate(accounting, "acct-1", hour(1)); err != nil {
		t.Fatalf("another type may cover the same period: %v", err)
	}
	if err := p.RecordRate(RateEntry{Type: RateEstimate, From: "CNY", Rate: 1, Scale: 0, FromDigits: 2, ToDigits: 2, ValidFrom: t0, Source: "x"}, "acct-1",
		hour(1)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a rate for the functional currency: %v", err)
	}
	if local, fx, err := p.Local(usd(10_050), RateEstimate, hour(2)); err != nil || local != 71_590 || fx == nil || fx.EntryNo != 1 {
		t.Fatalf("USD 100.50 at 7.1234: %d %+v %v", local, fx, err)
	}
	if local, fx, err := p.Local(cny(500), RateEstimate, hour(2)); err != nil || local != 500 || fx != nil {
		t.Fatalf("the functional currency converts as itself: %d %v", local, err)
	}
	if _, _, err := p.Local(usd(1), RateInvoice, hour(2)); !errors.Is(err, ErrRateMissing) {
		t.Fatalf("no INVOICE rate: %v", err)
	}
	if _, _, err := p.Local(kernel.Money{Currency: "EUR", Minor: 1}, RateEstimate, hour(2)); !errors.Is(err, ErrRateMissing) {
		t.Fatalf("no EUR rate: %v", err)
	}
	if _, _, err := p.Local(usd(1), RateEstimate, t0.AddDate(0, 2, 0)); !errors.Is(err, ErrRateMissing) {
		t.Fatalf("after the period: %v", err)
	}
	if local, fx, err := p.Local(kernel.Money{Currency: "EUR", Minor: 0}, RateEstimate, hour(2)); err != nil || local != 0 || fx != nil {
		t.Fatalf("a zero amount needs no rate: %d %v", local, err)
	}
	back, err := RestorePolicy(p.State())
	if err != nil || len(back.AddedRates()) != 0 || len(back.State().Rates) != 2 {
		t.Fatalf("restore: %v", err)
	}
	// a wrong entry is ended early, once, and a corrected one follows
	if err = back.EndRate(1, t0.AddDate(0, 2, 0), "wrong", "acct-1", hour(3)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an end after the current end: %v", err)
	}
	if err = back.EndRate(1, hour(3), "the bank quoted the wrong pair", "acct-1", hour(3)); err != nil {
		t.Fatal(err)
	}
	if err = back.EndRate(1, hour(2), "again", "acct-1", hour(3)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("ended twice: %v", err)
	}
	if ended := back.EndedRates(); len(ended) != 1 || !ended[0].ValidUntil.Equal(hour(3)) {
		t.Fatalf("ended %+v", ended)
	}
	if err = back.RecordRate(RateEntry{Type: RateEstimate, From: "USD", Rate: 71_100, Scale: 4, FromDigits: 2, ToDigits: 2, ValidFrom: hour(3),
		ValidUntil: t0.AddDate(0, 1, 0), Source: "corrected"}, "acct-1", hour(3)); err != nil {
		t.Fatalf("the corrected entry: %v", err)
	}
	if _, fx, _ := back.Local(usd(100), RateEstimate, hour(4)); fx == nil || fx.EntryNo != 3 {
		t.Fatalf("the corrected entry converts after the end: %+v", fx)
	}
}

func job(t *testing.T, p *FinancePolicy) *FreightJob {
	t.Helper()
	j, err := OpenJob(JobState{TenantID: "T", JobID: "JOB-1", LegalEntityID: "LE", ShipmentID: "SH-A", Kind: KindAirExportHouse, CustomerPartyID: "party-a",
		PlanVersion: 2}, p, "fin-1", hour(1))
	if err != nil {
		t.Fatal(err)
	}
	return j
}

var quoteLines = []BasisLine{
	{LineNo: 1, ChargeCode: "AIR_FREIGHT", Sell: usd(120_000), Cost: usd(95_000)},
	{LineNo: 2, ChargeCode: "DOC_FEE", Sell: cny(30_000)},
	{LineNo: 3, ChargeCode: "LOYALTY", Sell: cny(-10_000)},
}

// A job: one quote per order applied once, lines with sell and cost estimates converted at the ESTIMATE rate,
// manual lines with a reason, revisions as new versions, the estimated profit, cancellation.
func TestFreightJobCharges(t *testing.T) {
	p := policy(t)
	j := job(t, p)
	if st := j.State(); st.FunctionalCurrency != "CNY" || st.BookCode != "MAIN" || st.State != JobOpen {
		t.Fatalf("opened: %+v", st)
	}
	basis := Basis{BasisID: "QUOTE:Q-1@2", QuoteID: "Q-1", QuoteRevision: 2, OrderID: "O-A"}
	if err := j.ApplyBasis(basis, "party-b", 1, quoteLines, p, "fin-1", hour(2)); !errors.Is(err, ErrNotPriceable) {
		t.Fatalf("another customer's quote: %v", err)
	}
	if err := j.ApplyBasis(basis, "party-a", 1, quoteLines, p, "fin-1", hour(2)); err != nil {
		t.Fatal(err)
	}
	if err := j.ApplyBasis(basis, "party-a", 1, quoteLines, p, "fin-1", hour(2)); !errors.Is(err, ErrBasisApplied) {
		t.Fatalf("the same quote and order twice: %v", err)
	}
	other := basis
	other.OrderID = "O-B"
	if err := j.ApplyBasis(other, "party-a", 1, quoteLines, p, "fin-1", hour(2)); !errors.Is(err, ErrBasisApplied) {
		t.Fatalf("the same quote for another order of the shipment: %v", err)
	}
	st := j.State()
	if len(st.Lines) != 3 || st.Lines[0].Sell[0].Local != 854_808 || st.Lines[0].Cost[0].Local != 676_723 || st.Lines[0].Cost[0].PartyID != "" ||
		len(st.Lines[1].Cost) != 0 || st.Lines[2].Sell[0].Local != -10_000 || st.Lines[0].Source != SourceQuote || st.Lines[0].BasisNo != 1 {
		t.Fatalf("lines from the quote: %+v", st.Lines)
	}
	if err := j.RecordLine("TRUCKING", "pickup from Songjiang", &SideInput{PartyID: "party-a", PartyRevision: 1, Amount: cny(80_000)},
		&SideInput{PartyID: "trucker", Amount: cny(60_000)}, "pickup booked after the quote", p, "fin-1", hour(3)); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]error{
		"no reason":       j.RecordLine("X", "", &SideInput{PartyID: "party-a", Amount: cny(1)}, nil, "", p, "fin-1", hour(3)),
		"no side":         j.RecordLine("X", "", nil, nil, "r", p, "fin-1", hour(3)),
		"negative manual": j.RecordLine("X", "", &SideInput{PartyID: "party-a", Amount: cny(-1)}, nil, "r", p, "fin-1", hour(3)),
		"no rate":         j.RecordLine("X", "", &SideInput{PartyID: "party-a", Amount: kernel.Money{Currency: "EUR", Minor: 1}}, nil, "r", p, "fin-1", hour(3)),
		"before":          j.RecordLine("X", "", &SideInput{PartyID: "party-a", Amount: cny(1)}, nil, "r", p, "fin-1", hour(0)),
	} {
		if tc == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := j.ReviseSide(1, SideCost, 0, SideInput{PartyID: "airline-mu", Amount: usd(96_000)}, "the airline's rate", p, "fin-1", hour(4)); !errors.Is(err, ErrSideVersion) {
		t.Fatalf("a stale side version: %v", err)
	}
	if err := j.ReviseSide(1, SideCost, 1, SideInput{PartyID: "airline-mu", Amount: usd(96_000)}, "the airline's rate", p, "fin-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	if err := j.ReviseSide(2, SideCost, 0, SideInput{Amount: cny(5_000)}, "courier for the documents", p, "fin-1", hour(4)); err != nil {
		t.Fatalf("adding a cost side: %v", err)
	}
	if err := j.ReviseSide(3, SideSell, 1, SideInput{PartyID: "party-a", Amount: cny(-5_000)}, "smaller loyalty discount", p, "fin-1", hour(4)); err != nil {
		t.Fatalf("a quote discount stays negative: %v", err)
	}
	if err := j.ReviseSide(2, SideSell, 1, SideInput{PartyID: "party-a", Amount: cny(-1)}, "x", p, "fin-1", hour(4)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a non-discount sell turned negative: %v", err)
	}
	if err := j.ReviseSide(3, SideCost, 0, SideInput{Amount: cny(1)}, "x", p, "fin-1", hour(4)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a cost on a discount line: %v", err)
	}
	if err := j.RecordLine("X", "", &SideInput{PartyID: "party-a", Amount: cny(MaxAmount + 1)}, nil, "r", p, "fin-1", hour(4)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an amount beyond the cap: %v", err)
	}
	profit := j.Profit()
	// revenue 854808 + 30000 - 5000 + 80000; cost 683846 (USD 960 at 7.1234) + 5000 + 60000
	if profit.Currency != "CNY" || profit.Revenue != 959_808 || profit.Cost != 748_846 || profit.Profit != 210_962 || len(profit.Lines) != 4 {
		t.Fatalf("profit %+v", profit)
	}
	if unbilled, billed := j.Exposure("party-a"); unbilled != 959_808 || billed != 0 {
		t.Fatalf("exposure %d %d", unbilled, billed)
	}
	if unbilled, _ := j.Exposure("party-b"); unbilled != 0 {
		t.Fatalf("exposure of another debtor %d", unbilled)
	}
	bases, lines, sides, _ := j.Added()
	if len(bases) != 1 || len(lines) != 4 || len(sides) != 9 {
		t.Fatalf("added %d %d %d", len(bases), len(lines), len(sides))
	}
	j.Stamp(hour(5))
	back, err := RestoreJob(j.State())
	if err != nil {
		t.Fatal(err)
	}
	if b, l, s, _ := back.Added(); len(b)+len(l)+len(s) != 0 || back.Profit().Profit != profit.Profit {
		t.Fatalf("restored job adds %d %d %d", len(b), len(l), len(s))
	}
	if err = back.Cancel("", "fin-1", hour(6)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a cancellation without reason: %v", err)
	}
	if err = back.Cancel("shipment cancelled by the customer", "fin-1", hour(6)); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if unbilled, _ := back.Exposure("party-a"); unbilled != 0 {
		t.Fatalf("cancel: %v", err)
	}
	if err = back.RecordLine("X", "", &SideInput{PartyID: "party-a", Amount: cny(1)}, nil, "r", p, "fin-1", hour(7)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a line on a cancelled job: %v", err)
	}
	if _, err = OpenJob(JobState{TenantID: "T", JobID: "JOB-2", LegalEntityID: "LE-2", ShipmentID: "SH-B", Kind: KindAirExportHouse, CustomerPartyID: "c",
		PlanVersion: 1}, p, "fin-1", hour(1)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a job under another legal entity's policy: %v", err)
	}
}

// Credit: terms versions, a manual hold and its release, the status DOC reads.
func TestCreditAccount(t *testing.T) {
	a, err := OpenAccount("T", "LE", "party-a", "CNY")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.SetTerms(usd(1), 30, TermsFromInvoiceDate, "credit-1", hour(1)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a limit in another currency: %v", err)
	}
	if err = a.SetTerms(cny(5_000_000), 30, TermsFromMonthEnd, "credit-1", hour(1)); err != nil || a.Status() != CreditOK {
		t.Fatalf("terms: %v", err)
	}
	if err = a.ReleaseHold("nothing held", "credit-1", hour(2)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("release without a hold: %v", err)
	}
	if err = a.PlaceHold("LATE", "x", "credit-1", hour(2)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an unknown hold code: %v", err)
	}
	if err = a.PlaceHold(HoldOverdue, "invoice 2026-10-001 unpaid 45 days", "credit-1", hour(2)); err != nil || a.Status() != CreditOnHold {
		t.Fatalf("hold: %v", err)
	}
	if err = a.PlaceHold(HoldManual, "again", "credit-1", hour(3)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a second hold: %v", err)
	}
	a.Stamp(hour(3))
	back, err := RestoreAccount(a.State())
	if err != nil {
		t.Fatal(err)
	}
	if err = back.ReleaseHold("paid on 2026-11-12", "credit-1", hour(1)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a release before the hold: %v", err)
	}
	if err = back.ReleaseHold("paid on 2026-11-12", "credit-2", hour(4)); err != nil || back.Status() != CreditOK {
		t.Fatalf("release: %v", err)
	}
	terms, holds, released := back.Added()
	if len(terms) != 0 || len(holds) != 0 || len(released) != 1 || released[0].ReleasedBy != "credit-2" {
		t.Fatalf("added %d %d %d", len(terms), len(holds), len(released))
	}
	if h, ok := back.OpenHold(); ok {
		t.Fatalf("open hold %+v", h)
	}
	bad := a.State()
	bad.State = AccountActive
	if _, err = RestoreAccount(bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an open hold on an active account: %v", err)
	}
}
