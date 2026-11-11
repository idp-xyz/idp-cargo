package phy

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	bentorepo "go.idp.xyz/idp-bento-go/repository"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

var at = time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)

func TestEventsMatchPublishedContract(t *testing.T) {
	created := domain.Event{Type: api.ConsolidationCreated, At: at, TenantID: "T", SubjectID: "C-1", ConsolidationID: "C-1", Version: 1}
	moved := domain.Event{Type: api.HouseTransferred, At: at, TenantID: "T", SubjectID: "SH-1", ConsolidationID: "C-2", ShipmentID: "SH-1",
		EntryNo: 2, FromConsolidationID: "C-1", Reason: api.ReasonRollover}
	for _, tc := range []struct {
		event     domain.Event
		want      string
		partition string
	}{
		{created, `{"tenant_id":"T","subject_id":"C-1","consolidation_id":"C-1","version":1}`, "T:C-1"},
		{moved, `{"tenant_id":"T","subject_id":"SH-1","consolidation_id":"C-2","shipment_id":"SH-1","entry_no":2,"from_consolidation_id":"C-1","reason":"ROLLOVER"}`, "T:SH-1"},
	} {
		envelope, err := postgres.PHYEnvelope("cmd-1", tc.event, at)
		if err != nil {
			t.Fatal(err)
		}
		if envelope.Source != api.Source || string(envelope.Type) != tc.event.Type || string(envelope.Payload) != tc.want ||
			envelope.PartitionKey != tc.partition {
			t.Fatalf("envelope: source=%s type=%s payload=%s partition=%s", envelope.Source, envelope.Type, envelope.Payload, envelope.PartitionKey)
		}
		var payload api.EventPayload
		if err = json.Unmarshal(envelope.Payload, &payload); err != nil || payload.SubjectID != tc.event.SubjectID {
			t.Fatalf("payload %+v, %v", payload, err)
		}
	}
	first, _ := postgres.PHYEnvelope("cmd-1", moved, at)
	again, _ := postgres.PHYEnvelope("cmd-1", moved, at.Add(time.Hour))
	other, _ := postgres.PHYEnvelope("cmd-2", moved, at)
	if first.ID != again.ID || first.ID == other.ID {
		t.Fatalf("event ids: retried %s vs %s, other command %s", first.ID, again.ID, other.ID)
	}
}

// TestTheModelEmitsThePublishedEventTypes runs every command of the model and checks that its events are
// exactly the types package api publishes.
func TestTheModelEmitsThePublishedEventTypes(t *testing.T) {
	loc := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	route := domain.Route{Origin: loc("CNSHG"), Destination: loc("DEHAM"), LocationEdition: "E1"}
	consol := func(id domain.ConsolidationID) *domain.Consolidation {
		c, err := domain.CreateConsolidation("T", id, domain.ModeSea, domain.KindSeaLCLOwn, "LE", route, at.Add(72*time.Hour), at.Add(48*time.Hour), "ops", at)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	intent := func(plan int, status domain.IntentStatus) domain.LoadIntent {
		kg := kernel.Quantity{Unit: "KGM", Amount: 100}
		return domain.LoadIntent{ShipmentID: "SH-1", Revision: int64(plan), Status: status, PlanVersion: plan, LegalEntityID: "LE", Mode: domain.ModeSea,
			Route: route, DepartureWindow: kernel.ValidityInterval{From: at, Until: at.Add(96 * time.Hour)},
			Scope: domain.CargoScope{Packs: 2, GrossWeight: kg},
			Parts: []domain.Part{{ObligationID: "OB-1", ObligationVersion: 1, OrderID: "O-1", Packs: 2, PackageType: "PX", GrossWeight: kg}}}
	}
	a, b := consol("C-1"), consol("C-2")
	m, _ := domain.NewMembership("T", "SH-1")
	if err := errors.Join(
		a.Amend(domain.Route{}, at.Add(80*time.Hour), at.Add(50*time.Hour), "vessel delayed", "ops", at, 0),
		m.Attach(a, intent(1, domain.IntentPlanned), "ops", at),
		m.Transfer(a, b, intent(1, domain.IntentPlanned), domain.ReasonShutOut, "", "ops", at),
		m.Refresh(b, intent(2, domain.IntentPlanned), "ops", at),
		b.Close(1, at, "ops", at),
		b.Reopen("late cargo", "ops", at),
		m.Detach(b, domain.ReasonCustomerRequest, "", "ops", at),
		a.Cancel("no cargo", "ops", at, 0),
	); err != nil {
		t.Fatal(err)
	}
	published := map[string]bool{api.ConsolidationCreated: true, api.ConsolidationAmended: true, api.ConsolidationClosed: true,
		api.ConsolidationReopened: true, api.ConsolidationCancelled: true, api.HouseAttached: true, api.HouseDetached: true,
		api.HouseTransferred: true, api.HouseIntentRefreshed: true}
	emitted := map[string]bool{}
	for _, e := range append(append(a.Pending(), b.Pending()...), m.Pending()...) {
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
		{bentorepo.ErrNotFound, api.ErrNotFound},
		{domain.ErrNotOpen, api.ErrRejected},
		{domain.ErrWindowMismatch, api.ErrRejected},
		{domain.ErrMemberStale, api.ErrRejected},
		{domain.ErrMemberNotAllocated, api.ErrRejected},
		{domain.ErrAlreadyAttached, api.ErrRejected},
		{application.ErrUnknownShipment, api.ErrRejected},
		{application.ErrLocationRefused, api.ErrRejected},
		{kernel.ErrOutsideRecordWindow, api.ErrRejected},
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
