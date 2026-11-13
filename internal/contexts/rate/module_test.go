package rate

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	bentorepo "go.idp.xyz/idp-bento-go/repository"
	"go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/contexts/rate/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/rate/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/rate/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

func TestQuoteEventsMatchPublishedContract(t *testing.T) {
	at := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)
	quote, err := domain.PriceQuote(domain.QuoteDraft{
		TenantID: "T", QuoteID: "Q", LegalEntityID: "LE", Customer: domain.CustomerRef{PartyID: "P", PartyRevision: 2},
		Recipient: domain.RecipientCustomer, Currency: "USD",
		Lines:    []domain.Line{{ChargeCode: "AIR_FREIGHT", Sell: kernel.Money{Currency: "USD", Minor: 100}, Cost: kernel.Money{Currency: "USD", Minor: 50}}},
		Validity: kernel.ValidityInterval{From: at, Until: at.Add(time.Hour)}, Author: "sales-1",
	}, at)
	if err != nil {
		t.Fatal(err)
	}
	if err = quote.Accept("sales-1", domain.EvidenceSignedDocument, "signed-quote", 2, at, at); err != nil {
		t.Fatal(err)
	}
	expired, err := domain.PriceQuote(domain.QuoteDraft{
		TenantID: "T", QuoteID: "Q", LegalEntityID: "LE", Customer: domain.CustomerRef{PartyID: "P", PartyRevision: 2},
		Recipient: domain.RecipientCustomer, Currency: "USD",
		Lines:    []domain.Line{{ChargeCode: "AIR_FREIGHT", Sell: kernel.Money{Currency: "USD", Minor: 100}, Cost: kernel.Money{Currency: "USD", Minor: 50}}},
		Validity: kernel.ValidityInterval{From: at, Until: at.Add(time.Hour)}, Author: "sales-1",
	}, at)
	if err == nil {
		err = expired.Expire(at.Add(time.Hour))
	}
	if err != nil {
		t.Fatal(err)
	}
	events := append(quote.Pending(), expired.Pending()[1])
	for i, want := range []string{api.QuotePriced, api.QuoteAccepted, api.QuoteExpired} {
		e := events[i]
		envelope, err := postgres.RATEEnvelope("cmd-1", e, at)
		if err != nil {
			t.Fatal(err)
		}
		var payload api.EventPayload
		if err = json.Unmarshal(envelope.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if envelope.Source != api.Source || string(envelope.Type) != want || payload != (api.EventPayload{TenantID: "T", SubjectID: "Q"}) || envelope.PartitionKey != "T:Q" {
			t.Fatalf("envelope %d: source=%s type=%s payload=%+v partition=%s", i, envelope.Source, envelope.Type, payload, envelope.PartitionKey)
		}
	}
}

func TestClassifyMapsCausesToAPIKinds(t *testing.T) {
	for _, tc := range []struct{ cause, want error }{
		{application.ErrUnauthenticated, api.ErrUnauthenticated},
		{fmt.Errorf("%w: policy", application.ErrUnauthorized), api.ErrUnauthorized},
		{bento.ErrTenantScopeConflict, api.ErrUnauthorized},
		{application.ErrIdempotencyConflict, api.ErrIdempotencyConflict},
		{domain.ErrConflict, api.ErrConflict},
		{bentorepo.ErrConflict, api.ErrConflict},
		{bentorepo.ErrAlreadyExists, api.ErrConflict},
		{bentorepo.ErrNotFound, api.ErrNotFound},
		{domain.ErrSelfApproval, api.ErrRejected},
		{domain.ErrOutsideValidity, api.ErrRejected},
		{fmt.Errorf("%w: org said no", application.ErrCustomerNotConfirmed), api.ErrRejected},
		{application.ErrQuoteNotAccepted, api.ErrRejected},
		{kernel.ErrCurrencyMismatch, api.ErrRejected},
		{kernel.ErrOutsideRecordWindow, api.ErrRejected},
		{domain.ErrInvalidTransition, api.ErrRejected},
	} {
		got := classify(tc.cause)
		if !errors.Is(got, tc.want) || !errors.Is(got, tc.cause) {
			t.Errorf("classify(%v) = %v, want %v wrapping the cause", tc.cause, got, tc.want)
		}
	}
	other := errors.New("connection reset")
	if classify(other) != other {
		t.Error("unknown causes must pass through unchanged")
	}
}

func TestStoredTimesKeepMicroseconds(t *testing.T) {
	in := time.Date(2026, 11, 6, 8, 0, 0, 123456789, time.FixedZone("CST", 8*3600))
	if got := stored(in); got.Nanosecond() != 123456000 || got.Location() != time.UTC || !stored(time.Time{}).IsZero() {
		t.Fatalf("stored(%v) = %v", in, got)
	}
}
