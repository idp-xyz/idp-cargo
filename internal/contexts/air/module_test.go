package air

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	bentorepo "go.idp.xyz/idp-bento-go/repository"
	"go.idp.xyz/idp-cargo/internal/contexts/air/api"
	"go.idp.xyz/idp-cargo/internal/contexts/air/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/air/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/air/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

var at = time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)

func TestEventsMatchPublishedContract(t *testing.T) {
	e := domain.Event{Type: api.HouseWaybillAssociated, At: at, TenantID: "T", SubjectID: "AM-1", ConsolidationID: "C-1", ShipmentID: "SH-1", EntryNo: 1,
		BookingID: "BK-1", CapacityVersion: 1}
	envelope, err := postgres.AIREnvelope("cmd-1", e, at)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"tenant_id":"T","subject_id":"AM-1","consolidation_id":"C-1","shipment_id":"SH-1","entry_no":1,"booking_id":"BK-1","capacity_version":1}`
	if envelope.Source != api.Source || string(envelope.Payload) != want || envelope.PartitionKey != "T:AM-1" {
		t.Fatalf("envelope: %s %s", envelope.Payload, envelope.PartitionKey)
	}
	var payload api.EventPayload
	if err = json.Unmarshal(envelope.Payload, &payload); err != nil || payload.ShipmentID != "SH-1" {
		t.Fatalf("payload %+v %v", payload, err)
	}
	again, _ := postgres.AIREnvelope("cmd-1", e, at.Add(time.Hour))
	other, _ := postgres.AIREnvelope("cmd-2", e, at)
	if envelope.ID != again.ID || envelope.ID == other.ID {
		t.Fatal("event ids")
	}
}

// TestTheModelEmitsThePublishedEventTypes runs every command of the model and checks that its events are
// exactly the types package api publishes.
func TestTheModelEmitsThePublishedEventTypes(t *testing.T) {
	ap := func(code, iata string) domain.Airport {
		return domain.Airport{Code: kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code}, IATA: iata}
	}
	pvg, jfk := ap("CNPVG", "PVG"), ap("USJFK", "JFK")
	booking := func(v int) domain.Booking {
		return domain.Booking{BookingID: "BK-1", Revision: 3, AirConsol: true, InForce: true, ConsumerID: "C-1", LegalEntityID: "LE",
			Airline: domain.Airline{PartyID: "MU", PartyRevision: 1, Designator: "MU", AWBPrefix: "781"}, Origin: pvg, Destination: jfk, Edition: "E1",
			MAWB: "781-12345675", CapacityVersion: v, Lines: 1, Segments: []domain.Segment{{Flight: "MU587", From: pvg, To: jfk, ETD: at.Add(72 * time.Hour)}}}
	}
	plan := func(v int, consol, status string) domain.Plan {
		kg := kernel.Quantity{Unit: "KGM", Amount: 100}
		return domain.Plan{ShipmentID: "SH-1", Revision: int64(v), Status: status, PlanVersion: v, LegalEntityID: "LE", Mode: "AIR", Origin: pvg.Code,
			Destination: jfk.Code, Window: kernel.ValidityInterval{From: at, Until: at.Add(96 * time.Hour)}, Pieces: 2, GrossWeight: kg, ConsolidationID: consol,
			Parts: []domain.Part{{ObligationID: "OB-1", ObligationVersion: 1, OrderID: "O-1", Packs: 2, PackageType: "CT", GrossWeight: kg}}}
	}
	m, err := domain.OpenMovement("T", "AM-1", "C-1", booking(1), "ops", at)
	if err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(m.LinkBooking(booking(2), "ops", at), m.Associate(plan(1, "C-1", "PLANNED"), "H1", "ops", at),
		m.Refresh(plan(2, "C-1", "PLANNED"), "ops", at), m.EndAssociation(plan(2, "C-2", "PLANNED"), "ops", at), m.Cancel("no cargo", "ops", at)); err != nil {
		t.Fatal(err)
	}
	s, err := domain.RegisterSeries("T", "LE", "SHA", 7, 1, "ops", at)
	if err != nil {
		t.Fatal(err)
	}
	published := map[string]bool{api.AirMovementOpened: true, api.FlightSegmentsLinked: true, api.MawbAssigned: true, api.MawbVoided: true,
		api.HouseWaybillAssociated: true, api.HouseWaybillFactsRefreshed: true, api.HouseWaybillAssociationEnded: true, api.AirMovementCancelled: true,
		api.HawbSeriesRegistered: true}
	emitted := map[string]bool{}
	for _, e := range append(m.Pending(), s.Pending()...) {
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
		{bentorepo.ErrNotFound, api.ErrNotFound},
		{domain.ErrNotAttached, api.ErrRejected},
		{domain.ErrInvalidBooking, api.ErrRejected},
		{domain.ErrSeriesExhausted, api.ErrRejected},
		{application.ErrHAWBInUse, api.ErrRejected},
		{application.ErrHAWBDiffers, api.ErrRejected},
		{application.ErrMAWBInUse, api.ErrRejected},
		{application.ErrNoSeries, api.ErrRejected},
		{kernel.ErrOutsideRecordWindow, api.ErrRejected},
	} {
		got := classify(tc.cause)
		if !errors.Is(got, tc.want) || !errors.Is(got, tc.cause) {
			t.Errorf("classify(%v) = %v, want %v", tc.cause, got, tc.want)
		}
	}
	if other := errors.New("connection reset"); classify(other) != other {
		t.Error("unknown causes must pass through unchanged")
	}
}
