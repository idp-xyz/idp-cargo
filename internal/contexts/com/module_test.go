package com

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	bentorepo "go.idp.xyz/idp-bento-go/repository"
	"go.idp.xyz/idp-cargo/internal/contexts/com/api"
	"go.idp.xyz/idp-cargo/internal/contexts/com/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/com/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

func TestOrderEventsMatchPublishedContract(t *testing.T) {
	at := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)
	recorded := domain.Event{Type: api.CustomerOrderRecorded, At: at, TenantID: "T", SubjectID: "O-1"}
	allocated := domain.Event{Type: api.FulfillmentObligationAllocated, At: at, TenantID: "T", SubjectID: "O-1", ObligationID: "OB-1", ObligationVersion: 1}
	for _, tc := range []struct {
		event domain.Event
		want  string
	}{
		{recorded, `{"tenant_id":"T","subject_id":"O-1"}`},
		{allocated, `{"tenant_id":"T","subject_id":"O-1","obligation_id":"OB-1","obligation_version":1}`},
	} {
		envelope, err := postgres.COMEnvelope("cmd-1", tc.event, at)
		if err != nil {
			t.Fatal(err)
		}
		if envelope.Source != api.Source || string(envelope.Type) != tc.event.Type || string(envelope.Payload) != tc.want ||
			envelope.Subject != "O-1" || envelope.PartitionKey != "T:O-1" {
			t.Fatalf("envelope: source=%s type=%s payload=%s subject=%s partition=%s", envelope.Source, envelope.Type, envelope.Payload, envelope.Subject, envelope.PartitionKey)
		}
		var payload api.EventPayload
		if err = json.Unmarshal(envelope.Payload, &payload); err != nil || payload.TenantID != "T" {
			t.Fatalf("payload %+v, %v", payload, err)
		}
	}
	first, _ := postgres.COMEnvelope("cmd-1", allocated, at)
	again, _ := postgres.COMEnvelope("cmd-1", allocated, at.Add(time.Hour))
	other := allocated
	other.ObligationID = "OB-2"
	second, _ := postgres.COMEnvelope("cmd-1", other, at)
	if first.ID != again.ID || first.ID == second.ID {
		t.Fatalf("event ids: retried %s vs %s, other obligation %s", first.ID, again.ID, second.ID)
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
		{application.ErrQuoteAlreadyOrdered, api.ErrConflict},
		{bentorepo.ErrNotFound, api.ErrNotFound},
		{domain.ErrOverAllocation, api.ErrRejected},
		{domain.ErrQuoteMismatch, api.ErrRejected},
		{domain.ErrOutsideQuoteValidity, api.ErrRejected},
		{domain.ErrInvalidTransition, api.ErrRejected},
		{fmt.Errorf("%w: rate said no", application.ErrQuoteNotAccepted), api.ErrRejected},
		{fmt.Errorf("%w: org said no", application.ErrCustomerNotConfirmed), api.ErrRejected},
		{application.ErrInvalidQuery, api.ErrRejected},
		{kernel.ErrUnitMismatch, api.ErrRejected},
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
