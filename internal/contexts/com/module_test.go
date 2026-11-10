package com

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	revoked := domain.Event{Type: api.FulfillmentObligationRevoked, At: at, TenantID: "T", SubjectID: "O-1", ObligationID: "OB-1", ObligationVersion: 2}
	reserved := domain.Event{Type: api.ObligationReserved, At: at, TenantID: "T", SubjectID: "O-1", ObligationID: "OB-1", ObligationVersion: 1, ShipmentID: "S-1"}
	cancelled := domain.Event{Type: api.CustomerOrderCancelled, At: at, TenantID: "T", SubjectID: "O-1"}
	for _, tc := range []struct {
		event domain.Event
		want  string
	}{
		{recorded, `{"tenant_id":"T","subject_id":"O-1"}`},
		{allocated, `{"tenant_id":"T","subject_id":"O-1","obligation_id":"OB-1","obligation_version":1}`},
		{revoked, `{"tenant_id":"T","subject_id":"O-1","obligation_id":"OB-1","obligation_version":2}`},
		{reserved, `{"tenant_id":"T","subject_id":"O-1","obligation_id":"OB-1","obligation_version":1,"shipment_id":"S-1"}`},
		{cancelled, `{"tenant_id":"T","subject_id":"O-1"}`},
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
	otherShipment := reserved
	otherShipment.ShipmentID = "S-2"
	one, _ := postgres.COMEnvelope("cmd-1", reserved, at)
	two, _ := postgres.COMEnvelope("cmd-1", otherShipment, at)
	if one.ID == two.ID {
		t.Fatal("reservations for different shipments need different event ids")
	}
}

// TestTheModelEmitsThePublishedEventTypes runs every command of the model and checks that its events are
// exactly the types package api publishes.
func TestTheModelEmitsThePublishedEventTypes(t *testing.T) {
	at := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)
	pallets := domain.Measure{Packs: 10, GrossWeight: kernel.Quantity{Unit: "KGM", Amount: 500}}
	draft := domain.OrderDraft{TenantID: "T", OrderID: "O-1", RecordedBy: "sales-1",
		Lines: []domain.OrderLine{{Description: "Pallets", PackageType: "PX", Measure: pallets}},
		Quote: domain.AcceptedQuote{QuoteID: "Q-1", QuoteRevision: 1, LegalEntityID: "LE", CustomerPartyID: "P", CustomerRevision: 1,
			RecipientRole: domain.RoleCustomer, Total: kernel.Money{Currency: "USD", Minor: 1},
			Validity:   kernel.ValidityInterval{From: at, Until: at.Add(48 * time.Hour)},
			AcceptedAt: at, EvidenceRef: "signed", TermsContract: domain.QuoteSnapshotV2, TermsHash: strings.Repeat("ab", 32)},
		Customer: domain.CustomerSnapshot{PartyID: "P", PartyRevision: 1, LegalName: "Fixture Corp", CountryCode: "US", RoleCode: domain.RoleCustomer,
			RoleValidity: kernel.ValidityInterval{From: at}, RoleEvidenceRef: "contract"},
		Policy: domain.BuiltInOrderPolicy(),
	}
	o, err := domain.RecordOrder(draft, 0, at, at)
	if err != nil {
		t.Fatal(err)
	}
	draft.OrderID = "O-2"
	closed, err := domain.RecordOrder(draft, 0, at, at)
	if err != nil {
		t.Fatal(err)
	}
	erasable := domain.RetentionEnd(at, domain.MinContactRetentionMonths)
	heavier := domain.Measure{Packs: 10, GrossWeight: kernel.Quantity{Unit: "KGM", Amount: 520}}
	if err = errors.Join(
		o.AmendLine(1, "Pallets", "PX", heavier, "customer corrected the weight", "cs-1", at, at),
		o.AddLine("Cartons", "CT", pallets, "customer added cartons", "cs-1", at, at),
		o.Allocate(1, "OB-1", pallets, "planner-1", at),
		o.Reserve("OB-1", 1, "S-1", "fulfilment", at),
		o.ReleaseReservation("OB-1", 1, "S-1", "fulfilment", at),
		o.Amend("OB-1", "OB-2", pallets, "planner-1", at),
		o.Revoke("OB-2", "planner-1", at),
		o.Cancel("sales-1", at, domain.MinContactRetentionMonths),
		o.EraseContacts("privacy-officer", erasable, erasable),
		closed.Allocate(1, "OB-9", pallets, "planner-1", at),
		closed.Close("cs-1", at, domain.MinContactRetentionMonths),
	); err != nil {
		t.Fatal(err)
	}
	published := map[string]bool{api.CustomerOrderRecorded: true, api.CustomerOrderCancelled: true, api.FulfillmentObligationAllocated: true,
		api.FulfillmentObligationRevoked: true, api.ObligationReserved: true, api.ObligationReservationReleased: true,
		api.CustomerOrderLineAmended: true, api.CustomerOrderLineAdded: true, api.CustomerOrderClosed: true, api.CustomerOrderContactsErased: true}
	emitted := map[string]bool{}
	for _, e := range append(o.Pending(), closed.Pending()...) {
		if !published[e.Type] {
			t.Errorf("event type %s is not published by package api", e.Type)
		}
		emitted[e.Type] = true
	}
	if len(emitted) != len(published) {
		t.Fatalf("emitted %v, published %v", emitted, published)
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
		{domain.ErrQuoteFullyOrdered, api.ErrConflict},
		{domain.ErrPolicyVersionStale, api.ErrConflict},
		{domain.ErrTermsMismatch, api.ErrRejected},
		{domain.ErrInvalidPolicy, api.ErrRejected},
		{application.ErrServiceLevelNotListed, api.ErrRejected},
		{domain.ErrInvalidParty, api.ErrRejected},
		{domain.ErrPartyRule, api.ErrRejected},
		{domain.ErrAddressRequired, api.ErrRejected},
		{application.ErrPartyNotConfirmed, api.ErrRejected},
		{domain.ErrContactsRetained, api.ErrRejected},
		{domain.ErrInvalidRetention, api.ErrRejected},
		{kernel.ErrOutsideRecordWindow, api.ErrRejected},
		{bentorepo.ErrNotFound, api.ErrNotFound},
		{domain.ErrUnknownObligation, api.ErrNotFound},
		{domain.ErrOverAllocation, api.ErrRejected},
		{domain.ErrQuoteMismatch, api.ErrRejected},
		{domain.ErrOutsideQuoteValidity, api.ErrRejected},
		{domain.ErrInvalidTransition, api.ErrRejected},
		{domain.ErrInvalidChange, api.ErrRejected},
		{domain.ErrObligationNotActive, api.ErrRejected},
		{domain.ErrObligationReserved, api.ErrRejected},
		{domain.ErrNotReserved, api.ErrRejected},
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
