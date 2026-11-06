package org

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	bentorepo "go.idp.xyz/idp-bento-go/repository"
	"go.idp.xyz/idp-cargo/internal/contexts/org/api"
	"go.idp.xyz/idp-cargo/internal/contexts/org/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/org/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
)

func TestApproveRoleEventMatchesPublishedContract(t *testing.T) {
	at := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)
	party, err := domain.RestoreParty("T", "P", "Legal", "US", domain.PartyActive, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = party.ApproveRole(domain.Role{Code: domain.RoleCustomer, LegalEntityID: "LE", ValidFrom: at, EvidenceRef: "e", ApprovedBy: "a"}, at); err != nil {
		t.Fatal(err)
	}
	events := party.Pending()
	if len(events) != 1 || events[0].Type != api.PartnerRoleApproved {
		t.Fatalf("domain events = %+v, want one %s", events, api.PartnerRoleApproved)
	}
	envelope, err := postgres.ORGEnvelope("cmd-1", events[0], at)
	if err != nil {
		t.Fatal(err)
	}
	var payload api.EventPayload
	if err = json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if envelope.Source != api.Source || string(envelope.Type) != api.PartnerRoleApproved || payload != (api.EventPayload{TenantID: "T", SubjectID: "P"}) {
		t.Fatalf("envelope source=%s type=%s payload=%+v does not match the api contract", envelope.Source, envelope.Type, payload)
	}
}

func TestClassifyMapsCausesToAPIKinds(t *testing.T) {
	for _, tc := range []struct{ cause, want error }{
		{application.ErrUnauthenticated, api.ErrUnauthenticated},
		{fmt.Errorf("%w: policy", application.ErrUnauthorized), api.ErrUnauthorized},
		{bento.ErrTenantScopeConflict, api.ErrUnauthorized},
		{domain.ErrCrossTenant, api.ErrUnauthorized},
		{application.ErrIdempotencyConflict, api.ErrIdempotencyConflict},
		{domain.ErrConflict, api.ErrConflict},
		{bentorepo.ErrConflict, api.ErrConflict},
		{bentorepo.ErrNotFound, api.ErrNotFound},
		{domain.ErrRoleOverlap, api.ErrRejected},
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
