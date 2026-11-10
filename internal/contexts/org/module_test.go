package org

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
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

// The JSON field names are those of PartyRef v1; verified_at is optional there and ORG does not record it.
func TestPartyRefMatchesPublishedContract(t *testing.T) {
	blob, err := json.Marshal(api.PartyRef{TenantID: "T", PartyID: "P", PartyRevision: 3, IdentitySource: api.IdentitySource, LegalEntityScope: "LE"})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err = json.Unmarshal(blob, &fields); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"tenant_id": "T", "party_id": "P", "party_revision": float64(3), "identity_source": "ORG", "legal_entity_scope": "LE"}
	if !reflect.DeepEqual(fields, want) {
		t.Fatalf("PartyRef JSON = %s, want the PartyRef v1 fields %v", blob, want)
	}
	if blob, _ = json.Marshal(api.PartyRef{TenantID: "T", PartyID: "P", PartyRevision: 1, IdentitySource: api.IdentitySource}); strings.Contains(string(blob), "legal_entity_scope") {
		t.Fatalf("legal_entity_scope is optional in PartyRef v1: %s", blob)
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
		{fmt.Errorf("%w: party is SUSPENDED", domain.ErrPartyNotActive), api.ErrRejected},
		{domain.ErrRoleNotEffective, api.ErrRejected},
		{domain.ErrLegalEntityNotVerified, api.ErrRejected},
		{application.ErrInvalidQuery, api.ErrRejected},
		{application.ErrInvalidCommand, api.ErrRejected},
		{domain.ErrEntryExists, api.ErrConflict},
		{domain.ErrUnknownAddress, api.ErrNotFound},
		{domain.ErrUnknownContact, api.ErrNotFound},
		{domain.ErrInvalidAddress, api.ErrRejected},
		{domain.ErrInvalidContact, api.ErrRejected},
		{domain.ErrInvalidChange, api.ErrRejected},
		{domain.ErrShortCodeInUse, api.ErrRejected},
		{domain.ErrEntryNotActive, api.ErrRejected},
		{domain.ErrNothingChanged, api.ErrRejected},
		{domain.ErrPartyArchived, api.ErrRejected},
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

// Address book events are published under the api's names and carry identifiers only: the party, the
// address or contact, its version, the book revision and the purpose of a new default.
func TestAddressBookEventsMatchPublishedContract(t *testing.T) {
	pairs := map[string]string{
		domain.EventPartyAddressAdded: api.PartyAddressAdded, domain.EventPartyAddressChanged: api.PartyAddressChanged,
		domain.EventPartyAddressDeactivated: api.PartyAddressDeactivated, domain.EventPartyDefaultAddressSet: api.PartyDefaultAddressSet,
		domain.EventPartyContactAdded: api.PartyContactAdded, domain.EventPartyContactChanged: api.PartyContactChanged,
		domain.EventPartyContactDeactivated: api.PartyContactDeactivated, domain.EventPartyContactErased: api.PartyContactErased,
		domain.EventPartyDefaultContactSet: api.PartyDefaultContactSet,
	}
	for d, a := range pairs {
		if d != a {
			t.Errorf("domain event %s is published as %s", d, a)
		}
	}
	at := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	book, err := domain.NewAddressBook("T", "P")
	if err != nil {
		t.Fatal(err)
	}
	contact := domain.Contact{Purposes: []domain.ContactPurpose{domain.ContactBooking}, Name: "Test Person A", Email: "desk-a@example.test"}
	if _, err = book.AddContact("C1", contact, nil, "clerk", at); err != nil {
		t.Fatal(err)
	}
	if _, err = book.SetDefaultContact(domain.ContactBooking, "C1", "clerk", at); err != nil {
		t.Fatal(err)
	}
	events := book.Pending()
	want := []api.EventPayload{
		{TenantID: "T", SubjectID: "P", EntryID: "C1", EntryVersion: 1, Revision: 1},
		{TenantID: "T", SubjectID: "P", EntryID: "C1", EntryVersion: 1, Revision: 2, Purpose: api.ContactBooking},
	}
	for i, e := range events {
		envelope, err := postgres.ORGEnvelope(fmt.Sprintf("cmd-%d", i), e, at)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(envelope.Payload), "Test Person") || strings.Contains(string(envelope.Payload), "example.test") {
			t.Fatalf("event payload carries personal data: %s", envelope.Payload)
		}
		var payload api.EventPayload
		if err = json.Unmarshal(envelope.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload != want[i] || envelope.Subject != "P" {
			t.Fatalf("event %d payload = %+v, want %+v", i, payload, want[i])
		}
	}
}
