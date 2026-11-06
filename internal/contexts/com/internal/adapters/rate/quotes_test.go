package rate

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	app "go.idp.xyz/idp-cargo/internal/contexts/com/internal/application"
	rateapi "go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

type rateStub struct {
	snapshot rateapi.QuoteSnapshot
	err      error
}

func (r rateStub) GetQuoteSnapshot(context.Context, rateapi.GetQuoteSnapshot) (rateapi.QuoteSnapshot, error) {
	return r.snapshot, r.err
}

func snapshot() rateapi.QuoteSnapshot {
	at := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)
	usd := func(minor int64) kernel.Money { return kernel.Money{Currency: "USD", Minor: minor} }
	return rateapi.QuoteSnapshot{TenantID: "T", QuoteID: "Q-1", QuoteRevision: 3, LegalEntityID: "LE",
		Customer: rateapi.CustomerRef{PartyID: "P", PartyRevision: 2},
		Lines:    []rateapi.SnapshotLine{{ChargeCode: "FREIGHT", Amount: usd(90_000)}, {ChargeCode: "FUEL", Amount: usd(10_000)}},
		Total:    usd(100_000), Validity: kernel.ValidityInterval{From: at, Until: at.Add(14 * 24 * time.Hour)},
		PriceApprovedBy: "manager-1", AcceptedAt: at.Add(time.Hour), AcceptanceEvidenceRef: "signed-quote"}
}

func TestQuotesTranslateQuoteSnapshotV1(t *testing.T) {
	s := snapshot()
	q, err := Quotes{RATE: rateStub{snapshot: s}}.AcceptedQuote(context.Background(), "T", "Q-1")
	if err != nil {
		t.Fatal(err)
	}
	base, _ := TermsHash(s)
	if q.QuoteRevision != 3 || q.LegalEntityID != "LE" || q.CustomerPartyID != "P" || q.CustomerRevision != 2 || q.Total != s.Total ||
		!q.AcceptedAt.Equal(s.AcceptedAt) || q.EvidenceRef != "signed-quote" || q.TermsHash != base || len(base) != 64 {
		t.Fatalf("accepted quote %+v", q)
	}
	if again, _ := TermsHash(snapshot()); again != base {
		t.Fatal("the terms hash must be deterministic")
	}
	for name, edit := range map[string]func(*rateapi.QuoteSnapshot){
		"revision":     func(s *rateapi.QuoteSnapshot) { s.QuoteRevision = 4 },
		"legal entity": func(s *rateapi.QuoteSnapshot) { s.LegalEntityID = "LE-2" },
		"customer":     func(s *rateapi.QuoteSnapshot) { s.Customer.PartyRevision = 3 },
		"line amount":  func(s *rateapi.QuoteSnapshot) { s.Lines[1].Amount.Minor = 10_001 },
		"line order":   func(s *rateapi.QuoteSnapshot) { s.Lines[0], s.Lines[1] = s.Lines[1], s.Lines[0] },
		"total":        func(s *rateapi.QuoteSnapshot) { s.Total.Minor = 100_001 },
		"validity":     func(s *rateapi.QuoteSnapshot) { s.Validity.Until = s.Validity.Until.Add(time.Microsecond) },
		"approver":     func(s *rateapi.QuoteSnapshot) { s.PriceApprovedBy = "" },
		"acceptance":   func(s *rateapi.QuoteSnapshot) { s.AcceptedAt = s.AcceptedAt.Add(time.Second) },
		"evidence":     func(s *rateapi.QuoteSnapshot) { s.AcceptanceEvidenceRef = "email" },
	} {
		changed := snapshot()
		edit(&changed)
		if h, _ := TermsHash(changed); h == base {
			t.Errorf("changing the %s must change the terms hash", name)
		}
	}
	repositoryNotFound := errors.New("repository: not found")
	for name, stub := range map[string]rateStub{
		"quote not accepted": {err: fmt.Errorf("%w: pricing", rateapi.ErrRejected)},
		"unknown quote":      {err: fmt.Errorf("%w: %w", rateapi.ErrNotFound, repositoryNotFound)},
		"another quote":      {snapshot: func() rateapi.QuoteSnapshot { s := snapshot(); s.QuoteID = "Q-2"; return s }()},
	} {
		_, err := (Quotes{RATE: stub}).AcceptedQuote(context.Background(), "T", "Q-1")
		if !errors.Is(err, app.ErrQuoteNotAccepted) || errors.Is(err, rateapi.ErrNotFound) || errors.Is(err, repositoryNotFound) {
			t.Errorf("%s: err = %v, want only ErrQuoteNotAccepted in its chain", name, err)
		}
	}
}
