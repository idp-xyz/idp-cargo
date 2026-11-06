package domain

import (
	"errors"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

var at = time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)

func usd(minor int64) kernel.Money { return kernel.Money{Currency: "USD", Minor: minor} }

func draft(floorBP int, lines ...Line) QuoteDraft {
	return QuoteDraft{
		TenantID: "T", QuoteID: "Q-1", LegalEntityID: "LE", Customer: CustomerRef{PartyID: "P", PartyRevision: 2},
		Currency: "USD", Lines: lines, Validity: kernel.ValidityInterval{From: at, Until: at.Add(14 * 24 * time.Hour)},
		MarginFloorBP: floorBP, Author: "sales-1",
	}
}

func freight(sell, cost int64) Line {
	return Line{ChargeCode: "FREIGHT", Sell: usd(sell), Cost: usd(cost)}
}

func priced(t *testing.T, floorBP int, lines ...Line) *Quote {
	t.Helper()
	q, err := PriceQuote(draft(floorBP, lines...), at)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func TestMarginFloorDecidesWhetherApprovalIsNeeded(t *testing.T) {
	for name, tc := range map[string]struct {
		floor      int
		lines      []Line
		wantStatus QuoteStatus
	}{
		"margin equal to the floor":   {1000, []Line{freight(100_000, 90_000)}, QuotePriced},
		"one basis point below":       {1001, []Line{freight(100_000, 90_000)}, QuoteAwaitingApproval},
		"lines summed before compare": {1000, []Line{freight(60_000, 50_000), {ChargeCode: "THC", Sell: usd(40_000), Cost: usd(40_000)}}, QuotePriced},
		"negative margin, zero floor": {0, []Line{freight(100_000, 110_000)}, QuoteAwaitingApproval},
		"zero margin, zero floor":     {0, []Line{freight(100_000, 100_000)}, QuotePriced},
	} {
		q := priced(t, tc.floor, tc.lines...)
		if q.Status() != tc.wantStatus {
			t.Errorf("%s: status %s, want %s", name, q.Status(), tc.wantStatus)
		}
		if events := q.Pending(); len(events) != 1 || events[0].Type != "QuotePriced.v1" || events[0].SubjectID != "Q-1" {
			t.Errorf("%s: events %+v", name, events)
		}
	}
}

func TestPriceQuoteRejectsInvalidDrafts(t *testing.T) {
	for name, edit := range map[string]func(*QuoteDraft){
		"no lines":                  func(d *QuoteDraft) { d.Lines = nil },
		"line in another currency":  func(d *QuoteDraft) { d.Lines[0].Sell = kernel.Money{Currency: "EUR", Minor: 1} },
		"free line":                 func(d *QuoteDraft) { d.Lines[0].Sell = usd(0) },
		"negative cost":             func(d *QuoteDraft) { d.Lines[0].Cost = usd(-1) },
		"open-ended validity":       func(d *QuoteDraft) { d.Validity.Until = time.Time{} },
		"validity already ended":    func(d *QuoteDraft) { d.Validity = kernel.ValidityInterval{From: at.Add(-48 * time.Hour), Until: at} },
		"malformed currency":        func(d *QuoteDraft) { d.Currency = "usd" },
		"floor above 100%":          func(d *QuoteDraft) { d.MarginFloorBP = 10001 },
		"customer without revision": func(d *QuoteDraft) { d.Customer.PartyRevision = 0 },
	} {
		d := draft(1000, freight(100_000, 90_000))
		edit(&d)
		if _, err := PriceQuote(d, at); !errors.Is(err, ErrInvalidQuote) {
			t.Errorf("%s: err = %v, want %v", name, err, ErrInvalidQuote)
		}
	}
}

func TestPriceApprovalNeedsAnIndependentApproverAndReason(t *testing.T) {
	q := priced(t, 2000, freight(100_000, 90_000))
	if err := q.ApprovePrice("sales-1", "margin-exception-1", at); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("author approving: err = %v", err)
	}
	if err := q.ApprovePrice("manager-1", "", at); !errors.Is(err, ErrInvalidQuote) {
		t.Fatalf("approval without reason: err = %v", err)
	}
	if err := q.ApprovePrice("manager-1", "margin-exception-1", at); err != nil {
		t.Fatal(err)
	}
	s := q.State()
	if q.Status() != QuotePriced || s.Approval == nil || s.Approval.ApprovedBy != "manager-1" || s.MarginFloorBP != 2000 {
		t.Fatalf("after approval: %+v", s)
	}
	if err := q.ApprovePrice("manager-2", "again", at); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("second approval: err = %v", err)
	}
	if events := q.Pending(); len(events) != 2 || events[1].Type != "QuotePriceApproved.v1" {
		t.Fatalf("events %+v", events)
	}
}

func TestAcceptanceWithinValidityFreezesTheQuote(t *testing.T) {
	waiting := priced(t, 2000, freight(100_000, 90_000))
	if err := waiting.Accept("sales-1", "signed-quote", 2, at); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("accepting before the price approval: err = %v", err)
	}
	q := priced(t, 1000, freight(100_000, 90_000))
	validity := q.State().Validity
	for name, tc := range map[string]struct {
		at       time.Time
		revision int64
		want     error
	}{
		"before the validity":     {at.Add(-time.Second), 2, ErrOutsideValidity},
		"when the validity ends":  {validity.Until, 2, ErrOutsideValidity},
		"older customer revision": {at, 1, ErrInvalidQuote},
	} {
		if err := q.Accept("sales-1", "signed-quote", tc.revision, tc.at); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	if err := q.Accept("sales-1", "signed-quote", 3, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if s := q.State(); q.Status() != QuoteAccepted || s.Acceptance.CustomerRevision != 3 || s.Acceptance.EvidenceRef != "signed-quote" {
		t.Fatalf("after acceptance: %+v", s)
	}
	if err := q.Accept("sales-1", "again", 3, at.Add(2*time.Hour)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("second acceptance: err = %v", err)
	}
	if err := q.ApprovePrice("manager-1", "late", at.Add(2*time.Hour)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("approval after acceptance: err = %v", err)
	}
}

func TestRestoreQuoteChecksConsistency(t *testing.T) {
	below := QuoteState{QuoteDraft: draft(2000, freight(100_000, 90_000))}
	for name, tc := range map[string]struct {
		edit func(*QuoteState)
		ok   bool
	}{
		"awaiting approval": {func(s *QuoteState) { s.Status = QuoteAwaitingApproval }, true},
		"priced below floor, approved": {func(s *QuoteState) {
			s.Status, s.Approval = QuotePriced, &PriceApproval{ApprovedBy: "m", ApprovedAt: at, ReasonRef: "r"}
		}, true},
		"priced below floor, unapproved": {func(s *QuoteState) { s.Status = QuotePriced }, false},
		"accepted without acceptance": {func(s *QuoteState) {
			s.Status, s.Approval = QuoteAccepted, &PriceApproval{ApprovedBy: "m", ApprovedAt: at, ReasonRef: "r"}
		}, false},
		"unknown status": {func(s *QuoteState) { s.Status = "WITHDRAWN" }, false},
	} {
		s := below
		tc.edit(&s)
		q, err := RestoreQuote(s)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", name, err, tc.ok)
		}
		if err == nil && len(q.Pending()) != 0 {
			t.Errorf("%s: restoring must not emit events", name)
		}
	}
	q := priced(t, 1000, freight(100_000, 90_000))
	s := q.State()
	s.Lines[0].Sell = usd(1)
	if q.State().Lines[0].Sell != usd(100_000) {
		t.Fatal("State must return a copy")
	}
}
