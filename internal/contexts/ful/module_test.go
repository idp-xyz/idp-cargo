package ful

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	bentorepo "go.idp.xyz/idp-bento-go/repository"
	"go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	"go.idp.xyz/idp-cargo/internal/contexts/ful/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/ful/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/ful/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

var at = time.Date(2026, 11, 7, 8, 0, 0, 0, time.UTC)

func TestShipmentEventsMatchPublishedContract(t *testing.T) {
	created := domain.Event{Type: api.ShipmentCreated, At: at, TenantID: "T", SubjectID: "SH-1", ObligationID: "OB-1", ObligationVersion: 1}
	amended := domain.Event{Type: api.ShipmentPlanAmended, At: at, TenantID: "T", SubjectID: "SH-1"}
	for _, tc := range []struct {
		event domain.Event
		want  string
	}{
		{created, `{"tenant_id":"T","subject_id":"SH-1","obligation_id":"OB-1","obligation_version":1}`},
		{amended, `{"tenant_id":"T","subject_id":"SH-1"}`},
	} {
		envelope, err := postgres.FULEnvelope("cmd-1", tc.event, at)
		if err != nil {
			t.Fatal(err)
		}
		if envelope.Source != api.Source || string(envelope.Type) != tc.event.Type || string(envelope.Payload) != tc.want ||
			envelope.Subject != "SH-1" || envelope.PartitionKey != "T:SH-1" {
			t.Fatalf("envelope: source=%s type=%s payload=%s subject=%s partition=%s", envelope.Source, envelope.Type, envelope.Payload, envelope.Subject, envelope.PartitionKey)
		}
		var payload api.EventPayload
		if err = json.Unmarshal(envelope.Payload, &payload); err != nil || payload.SubjectID != "SH-1" {
			t.Fatalf("payload %+v, %v", payload, err)
		}
	}
	first, _ := postgres.FULEnvelope("cmd-1", created, at)
	again, _ := postgres.FULEnvelope("cmd-1", created, at.Add(time.Hour))
	other, _ := postgres.FULEnvelope("cmd-2", created, at)
	if first.ID != again.ID || first.ID == other.ID {
		t.Fatalf("event ids: retried %s vs %s, other command %s", first.ID, again.ID, other.ID)
	}
}

// TestTheModelEmitsThePublishedEventTypes runs every command of the model and checks that its events are
// exactly the types package api publishes.
func TestTheModelEmitsThePublishedEventTypes(t *testing.T) {
	ref := func(id string) domain.ObligationRef {
		return domain.ObligationRef{ObligationID: domain.ObligationID(id), ObligationVersion: 1, OrderID: "O-1", CustomerPartyID: "P",
			LegalEntityID: "LE", Packs: 2, PackageType: "PX", GrossWeight: kernel.Quantity{Unit: "KGM", Amount: 20}}
	}
	half := domain.CargoScope{Packs: 1, GrossWeight: kernel.Quantity{Unit: "KGM", Amount: 10}}
	s, err := domain.CreateShipment("T", "SH-1", ref("OB-1"), "planner-1", at)
	if err != nil {
		t.Fatal(err)
	}
	route := domain.Route{Mode: domain.ModeSea, Origin: kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: "CNSHA"},
		Destination: kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: "DEHAM"}, Departure: kernel.ValidityInterval{From: at, Until: at.Add(time.Hour)},
		LocationEdition: "TEST-EDITION"}
	if err = errors.Join(
		s.AddObligation(ref("OB-2"), "planner-1", at),
		s.AmendPlan(route, "schedule", "planner-1", at),
		s.Confirm("planner-2", at),
		s.ReleaseObligation("OB-2", 1, "planner-1", at),
		s.ChangePart("OB-1", 1, half, domain.CargoScope{}, "shortload", "planner-1", at),
		s.Cancel("withdrawn", "planner-1", at),
	); err != nil {
		t.Fatal(err)
	}
	published := map[string]bool{api.ShipmentCreated: true, api.ShipmentObligationAdded: true, api.ShipmentPlanAmended: true,
		api.ShipmentPlanConfirmed: true, api.ShipmentObligationReleased: true, api.ShipmentPartChanged: true, api.ShipmentCancelled: true}
	emitted := map[string]bool{}
	for _, e := range s.Pending() {
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
		{application.ErrPlannedElsewhere, api.ErrConflict},
		{domain.ErrInvalidPart, api.ErrRejected},
		{bentorepo.ErrNotFound, api.ErrNotFound},
		{domain.ErrInvalidShipment, api.ErrRejected},
		{domain.ErrInvalidObligation, api.ErrRejected},
		{domain.ErrOtherCustomer, api.ErrRejected},
		{domain.ErrAlreadyPlanned, api.ErrRejected},
		{domain.ErrNotPlanned, api.ErrRejected},
		{domain.ErrLastObligation, api.ErrRejected},
		{domain.ErrInvalidPlan, api.ErrRejected},
		{domain.ErrIncompletePlan, api.ErrRejected},
		{domain.ErrInvalidTransition, api.ErrRejected},
		{domain.ErrInvalidChange, api.ErrRejected},
		{fmt.Errorf("%w: COM said no", application.ErrNotPlannable), api.ErrRejected},
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
