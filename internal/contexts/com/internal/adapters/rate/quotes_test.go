package rate

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	app "go.idp.xyz/idp-cargo/internal/contexts/com/internal/application"
	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
	rateapi "go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

type rateStub struct {
	snapshot rateapi.QuoteSnapshotV2
	v3       *rateapi.QuoteSnapshotV3 // when set, the V3 answer; otherwise snapshot as a ONE_OFF quote
	err      error
	levels   map[string]string
}

func (r rateStub) GetQuoteSnapshotV2(context.Context, rateapi.GetQuoteSnapshot) (rateapi.QuoteSnapshotV2, error) {
	return r.snapshot, r.err
}

func (r rateStub) GetQuoteSnapshotV3(context.Context, rateapi.GetQuoteSnapshot) (rateapi.QuoteSnapshotV3, error) {
	if r.v3 != nil {
		return *r.v3, r.err
	}
	return rateapi.QuoteSnapshotV3{QuoteSnapshotV2: r.snapshot, Kind: rateapi.QuoteOneOff}, r.err
}

func (r rateStub) GetQuoteSnapshot(context.Context, rateapi.GetQuoteSnapshot) (rateapi.QuoteSnapshot, error) {
	return rateapi.QuoteSnapshot{}, errors.New("COM reads QuoteSnapshotV3")
}

func (r rateStub) GetServiceLevel(_ context.Context, q rateapi.GetServiceLevel) (rateapi.ServiceLevel, error) {
	if r.err != nil {
		return rateapi.ServiceLevel{}, r.err
	}
	name, ok := r.levels[q.Code]
	if !ok {
		return rateapi.ServiceLevel{}, fmt.Errorf("%w: %s", rateapi.ErrNotFound, q.Code)
	}
	return rateapi.ServiceLevel{Code: q.Code, Name: name}, nil
}

func (r rateStub) ListServiceLevels(context.Context, rateapi.ListServiceLevels) ([]rateapi.ServiceLevel, error) {
	return nil, errors.New("COM asks for one service level")
}

func snapshot() rateapi.QuoteSnapshotV2 {
	at := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)
	usd := func(minor int64) kernel.Money { return kernel.Money{Currency: "USD", Minor: minor} }
	eur := func(minor int64) kernel.Money { return kernel.Money{Currency: "EUR", Minor: minor} }
	return rateapi.QuoteSnapshotV2{TenantID: "T", QuoteID: "Q-1", QuoteRevision: 3, LegalEntityID: "LE",
		Customer: rateapi.CustomerRef{PartyID: "P", PartyRevision: 2}, RecipientRole: "OVERSEAS_AGENT", Currency: "USD",
		Lines: []rateapi.SnapshotLineV2{{ChargeCode: "FREIGHT", Amount: usd(90_000), InQuoteCurrency: usd(90_000)},
			{ChargeCode: "THC", Amount: eur(9_000), InQuoteCurrency: usd(10_000)}},
		Total:           usd(100_000),
		ExchangeRates:   []rateapi.ExchangeRate{{From: "EUR", Rate: 1_111_111, Scale: 6, FromDigits: 2, QuoteDigits: 2, Source: "ECB", AsOf: at}},
		Rounding:        "HALF_UP_PER_LINE",
		Terms:           rateapi.CommercialTerms{IncotermsRule: "FOB", IncotermsVersion: "2020", NamedPlace: "Shanghai port", ServiceLevel: "STANDARD"},
		Validity:        kernel.ValidityInterval{From: at, Until: at.Add(14 * 24 * time.Hour)},
		PriceApprovedBy: "manager-1", AcceptedAt: at.Add(time.Hour), AcceptanceRecordedAt: at.Add(2 * time.Hour),
		AcceptanceEvidenceKind: "EMAIL", AcceptanceEvidenceRef: "signed-quote"}
}

func TestQuotesTranslateQuoteSnapshotV2(t *testing.T) {
	s := snapshot()
	q, err := Quotes{RATE: rateStub{snapshot: s}}.AcceptedQuote(context.Background(), "T", "Q-1")
	if err != nil {
		t.Fatal(err)
	}
	base, _ := TermsHashV2(s)
	terms := com.CommercialTerms{IncotermsRule: "FOB", IncotermsVersion: "2020", NamedPlace: "Shanghai port", ServiceLevel: "STANDARD"}
	if q.QuoteRevision != 3 || q.LegalEntityID != "LE" || q.CustomerPartyID != "P" || q.CustomerRevision != 2 || q.Total != s.Total ||
		q.RecipientRole != "OVERSEAS_AGENT" || q.Terms != terms || q.TermsContract != com.QuoteSnapshotV2 ||
		!q.AcceptedAt.Equal(s.AcceptedAt) || q.EvidenceRef != "signed-quote" || q.TermsHash != base || len(base) != 64 {
		t.Fatalf("accepted quote %+v", q)
	}
	if again, _ := TermsHashV2(snapshot()); again != base {
		t.Fatal("the terms hash must be deterministic")
	}
	recorded := snapshot()
	recorded.AcceptanceRecordedAt = recorded.AcceptanceRecordedAt.Add(time.Hour)
	if h, _ := TermsHashV2(recorded); h != base {
		t.Fatal("when RATE recorded the acceptance is not a term")
	}
	for name, edit := range map[string]func(*rateapi.QuoteSnapshotV2){
		"revision":        func(s *rateapi.QuoteSnapshotV2) { s.QuoteRevision = 4 },
		"legal entity":    func(s *rateapi.QuoteSnapshotV2) { s.LegalEntityID = "LE-2" },
		"customer":        func(s *rateapi.QuoteSnapshotV2) { s.Customer.PartyRevision = 3 },
		"recipient role":  func(s *rateapi.QuoteSnapshotV2) { s.RecipientRole = "CUSTOMER" },
		"line amount":     func(s *rateapi.QuoteSnapshotV2) { s.Lines[1].Amount.Minor = 9_001 },
		"converted line":  func(s *rateapi.QuoteSnapshotV2) { s.Lines[1].InQuoteCurrency.Minor = 10_001 },
		"discount flag":   func(s *rateapi.QuoteSnapshotV2) { s.Lines[1].Discount = true },
		"line order":      func(s *rateapi.QuoteSnapshotV2) { s.Lines[0], s.Lines[1] = s.Lines[1], s.Lines[0] },
		"total":           func(s *rateapi.QuoteSnapshotV2) { s.Total.Minor = 100_001 },
		"exchange rate":   func(s *rateapi.QuoteSnapshotV2) { s.ExchangeRates[0].Rate++ },
		"rate source":     func(s *rateapi.QuoteSnapshotV2) { s.ExchangeRates[0].Source = "BOC" },
		"Incoterms place": func(s *rateapi.QuoteSnapshotV2) { s.Terms.NamedPlace = "Ningbo port" },
		"service level":   func(s *rateapi.QuoteSnapshotV2) { s.Terms.ServiceLevel = "EXPRESS" },
		"unlisted mark":   func(s *rateapi.QuoteSnapshotV2) { s.ServiceLevelUnlisted = true },
		"validity":        func(s *rateapi.QuoteSnapshotV2) { s.Validity.Until = s.Validity.Until.Add(time.Microsecond) },
		"approver":        func(s *rateapi.QuoteSnapshotV2) { s.PriceApprovedBy = "" },
		"self-approval":   func(s *rateapi.QuoteSnapshotV2) { s.PriceSelfApproved = true },
		"approval reason": func(s *rateapi.QuoteSnapshotV2) { s.PriceApprovalReasons = []string{"DISCOUNT"} },
		"acceptance":      func(s *rateapi.QuoteSnapshotV2) { s.AcceptedAt = s.AcceptedAt.Add(time.Second) },
		"evidence kind":   func(s *rateapi.QuoteSnapshotV2) { s.AcceptanceEvidenceKind = "SIGNED_DOCUMENT" },
		"evidence":        func(s *rateapi.QuoteSnapshotV2) { s.AcceptanceEvidenceRef = "email" },
	} {
		changed := snapshot()
		edit(&changed)
		if h, _ := TermsHashV2(changed); h == base {
			t.Errorf("changing the %s must change the terms hash", name)
		}
	}
	repositoryNotFound := errors.New("repository: not found")
	for name, stub := range map[string]rateStub{
		"quote not accepted": {err: fmt.Errorf("%w: pricing", rateapi.ErrRejected)},
		"unknown quote":      {err: fmt.Errorf("%w: %w", rateapi.ErrNotFound, repositoryNotFound)},
		"another quote":      {snapshot: func() rateapi.QuoteSnapshotV2 { s := snapshot(); s.QuoteID = "Q-2"; return s }()},
	} {
		_, err := (Quotes{RATE: stub}).AcceptedQuote(context.Background(), "T", "Q-1")
		if !errors.Is(err, app.ErrQuoteNotAccepted) || errors.Is(err, rateapi.ErrNotFound) || errors.Is(err, repositoryNotFound) {
			t.Errorf("%s: err = %v, want only ErrQuoteNotAccepted in its chain", name, err)
		}
	}
}

// An AGREEMENT is read from QuoteSnapshotV3: no total, the v3 contract, a hash of its card version and mode
// (OD-5 A); a ONE_OFF quote read the same way keeps its v2 contract and hash.
func TestQuotesTranslateAgreements(t *testing.T) {
	agreement := func() rateapi.QuoteSnapshotV3 {
		s := snapshot()
		s.Lines, s.Total, s.ExchangeRates = nil, kernel.Money{}, nil
		return rateapi.QuoteSnapshotV3{QuoteSnapshotV2: s, Kind: rateapi.QuoteAgreement, Scope: &rateapi.QuoteScope{Mode: "AIR", ContainerMode: "LOOSE"},
			Agreement: &rateapi.AgreementRef{RateCardID: "C-ACME", Version: 1}}
	}
	a := agreement()
	q, err := Quotes{RATE: rateStub{v3: &a}}.AcceptedQuote(context.Background(), "T", "Q-1")
	if err != nil {
		t.Fatal(err)
	}
	base, _ := TermsHashV3(a)
	if q.Kind != com.QuoteAgreement || q.TermsContract != com.QuoteSnapshotV3 || q.TermsHash != base || len(base) != 64 ||
		q.Total != (kernel.Money{Currency: "USD"}) || !q.Validity.Until.Equal(a.Validity.Until) {
		t.Fatalf("an agreement: %+v", q)
	}
	for name, edit := range map[string]func(*rateapi.QuoteSnapshotV3){
		"card":          func(s *rateapi.QuoteSnapshotV3) { s.Agreement.RateCardID = "C-OTHER" },
		"version":       func(s *rateapi.QuoteSnapshotV3) { s.Agreement.Version = 2 },
		"mode":          func(s *rateapi.QuoteSnapshotV3) { s.Scope.Mode, s.Scope.ContainerMode = "SEA", "LCL" },
		"validity":      func(s *rateapi.QuoteSnapshotV3) { s.Validity.Until = s.Validity.Until.Add(time.Hour) },
		"approval":      func(s *rateapi.QuoteSnapshotV3) { s.PriceApprovalReasons = []string{"AGREEMENT_BELOW_FLOOR"} },
		"recipient":     func(s *rateapi.QuoteSnapshotV3) { s.RecipientRole = "CUSTOMER" },
		"evidence kind": func(s *rateapi.QuoteSnapshotV3) { s.AcceptanceEvidenceKind = "SIGNED_DOCUMENT" },
	} {
		changed := agreement()
		edit(&changed)
		if h, _ := TermsHashV3(changed); h == base {
			t.Errorf("changing the %s must change the terms hash", name)
		}
	}
	oneOff := snapshot()
	q, err = Quotes{RATE: rateStub{v3: &rateapi.QuoteSnapshotV3{QuoteSnapshotV2: oneOff, Kind: rateapi.QuoteOneOff}}}.AcceptedQuote(context.Background(), "T", "Q-1")
	if v2, _ := TermsHashV2(oneOff); err != nil || q.Kind != com.QuoteOneOff || q.TermsContract != com.QuoteSnapshotV2 || q.TermsHash != v2 || q.Total != oneOff.Total {
		t.Fatalf("a ONE_OFF quote read from V3: %+v %v", q, err)
	}
	for name, v3 := range map[string]rateapi.QuoteSnapshotV3{
		"an agreement with a total": func() rateapi.QuoteSnapshotV3 {
			s := agreement()
			s.Total = kernel.Money{Currency: "USD", Minor: 1}
			return s
		}(),
		"an agreement without its card": func() rateapi.QuoteSnapshotV3 { s := agreement(); s.Agreement = nil; return s }(),
		"a quote of another kind":       func() rateapi.QuoteSnapshotV3 { s := agreement(); s.Kind = "FRAMEWORK"; return s }(),
	} {
		if _, err := (Quotes{RATE: rateStub{v3: &v3}}).AcceptedQuote(context.Background(), "T", "Q-1"); !errors.Is(err, app.ErrQuoteNotAccepted) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestServiceLevelsAskRATEWhetherACodeIsListed(t *testing.T) {
	stub := rateStub{levels: map[string]string{"STANDARD": "Standard"}}
	if err := (ServiceLevels{RATE: stub}).Listed(context.Background(), "T", "STANDARD"); err != nil {
		t.Fatalf("listed: %v", err)
	}
	if err := (ServiceLevels{RATE: stub}).Listed(context.Background(), "T", "PRIORITY"); !errors.Is(err, app.ErrServiceLevelNotListed) || errors.Is(err, rateapi.ErrNotFound) {
		t.Fatalf("not listed: err = %v", err)
	}
	reset := errors.New("connection reset")
	if err := (ServiceLevels{RATE: rateStub{err: reset}}).Listed(context.Background(), "T", "STANDARD"); !errors.Is(err, reset) {
		t.Fatalf("other failures pass through: err = %v", err)
	}
}
