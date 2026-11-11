package book

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	bentorepo "go.idp.xyz/idp-bento-go/repository"
	"go.idp.xyz/idp-cargo/internal/contexts/book/api"
	"go.idp.xyz/idp-cargo/internal/contexts/book/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/book/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/book/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

var at = time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)

func TestEventsMatchPublishedContract(t *testing.T) {
	e := domain.Event{Type: api.BookingCapacityAllocated, At: at, TenantID: "T", BookingID: "B-1", CapacityVersion: 1, AllocationNo: 1,
		ConsumerKind: api.ConsumerConsolidation, ConsumerID: "C-1"}
	envelope, err := postgres.BOOKEnvelope("cmd-1", e, at)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"tenant_id":"T","booking_id":"B-1","capacity_version":1,"allocation_no":1,"consumer_kind":"CONSOL","consumer_id":"C-1"}`
	if envelope.Source != api.Source || string(envelope.Payload) != want || envelope.PartitionKey != "T:B-1" || envelope.Subject != "B-1" {
		t.Fatalf("envelope: %s %s %s", envelope.Source, envelope.Payload, envelope.PartitionKey)
	}
	var payload api.EventPayload
	if err = json.Unmarshal(envelope.Payload, &payload); err != nil || payload.ConsumerID != "C-1" {
		t.Fatalf("payload %+v, %v", payload, err)
	}
	again, _ := postgres.BOOKEnvelope("cmd-1", e, at.Add(time.Hour))
	other, _ := postgres.BOOKEnvelope("cmd-2", e, at)
	if envelope.ID != again.ID || envelope.ID == other.ID {
		t.Fatal("event ids")
	}
}

// TestTheModelEmitsThePublishedEventTypes runs every command of the model and checks that its events are
// exactly the types package api publishes.
func TestTheModelEmitsThePublishedEventTypes(t *testing.T) {
	loc := func(code, iata string) domain.Location {
		return domain.Location{Code: kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code}, IATA: iata}
	}
	pvg, jfk := loc("CNPVG", "PVG"), loc("USJFK", "JFK")
	kg := kernel.Quantity{Unit: "KGM", Amount: 100}
	carrier := domain.Carrier{PartyID: "MU", PartyRevision: 1, Role: domain.RoleCarrier, ValidFrom: at, RoleEvidence: "x", IATADesignator: "MU", AWBPrefix: "781"}
	request := domain.Request{Legs: []domain.RequestedLeg{{Loading: pvg, Discharge: jfk, DepartureDate: "2026-11-13"}},
		Capacity: domain.Capacity{Pieces: 2, GrossWeight: kg}, LocationEdition: "E1"}
	evidence := domain.Evidence{SHA256: strings.Repeat("aa", 32), StorageID: "s", MediaType: "application/pdf", CapturedAt: at}
	answer := domain.Answer{Channel: domain.ChannelEmail, Evidence: evidence, ReasonText: "x",
		References: []domain.Reference{{Type: domain.ReferenceMAWBNumber, Value: "781-12345675"}}}
	grant := domain.Grant{Edition: "E1", Lines: []domain.Line{{No: 1, Capacity: domain.Capacity{Pieces: 2, GrossWeight: kg},
		Legs: []domain.Leg{{Loading: pvg, Discharge: jfk, Flight: "MU587", ETD: at.Add(72 * time.Hour)}}}}}
	published := map[string]bool{}
	for _, kind := range []string{api.CarrierBookingDrafted, api.BookingRequestAmended, api.BookingRequested, api.CarrierBookingAcknowledged,
		api.BookingConfirmed, api.CarrierBookingRejected, api.BookingCapacityChanged, api.BookingChangeRequested, api.BookingChangeRefused,
		api.BookingDeclinedByCarrier, api.BookingCancellationRequested, api.CarrierBookingCancelled, api.BookingCapacityAllocated, api.BookingCapacityReleased} {
		published[kind] = true
	}
	emitted := map[string]bool{}
	run := func(steps ...func(*domain.Booking) error) {
		b, err := domain.DraftBooking("T", "B-1", domain.KindAirConsol, "LE", carrier, "C-1", "", request, "ops", at)
		if err != nil {
			t.Fatal(err)
		}
		for i, step := range steps {
			if err = step(b); err != nil {
				t.Fatalf("step %d: %v", i+1, err)
			}
		}
		for _, e := range b.Pending() {
			if !published[e.Type] {
				t.Errorf("event type %s is not published by package api", e.Type)
			}
			emitted[e.Type] = true
		}
	}
	sent := func(b *domain.Booking) error { return b.RecordSent(domain.ChannelEmail, evidence, "ops", at) }
	run(func(b *domain.Booking) error { return b.AmendRequest(request, "again", "ops", at) }, sent,
		func(b *domain.Booking) error { return b.RecordAcknowledgement(answer, "ops", at) },
		func(b *domain.Booking) error { return b.RecordConfirmation(answer, grant, "ops", at) },
		func(b *domain.Booking) error {
			return b.RecordCarrierChange(domain.CapacityRollover, answer, grant, "ops", at)
		},
		func(b *domain.Booking) error {
			return b.RequestChange(request, "more", domain.ChannelEmail, evidence, "ops", at)
		},
		func(b *domain.Booking) error { return b.RecordChangeRefused(answer, "ops", at) },
		func(b *domain.Booking) error { return b.Reassign("C-2", "replaced", "ops", at) },
		func(b *domain.Booking) error { return b.RecordDecline(answer, "ops", at) })
	run(sent, func(b *domain.Booking) error { return b.RecordRejection(answer, "ops", at) })
	run(sent, func(b *domain.Booking) error {
		return b.RequestCancellation("no", domain.ChannelEmail, evidence, "ops", at)
	},
		func(b *domain.Booking) error { return b.RecordCancellationConfirmed(answer, "ops", at) })
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
		{domain.ErrMAWB, api.ErrRejected},
		{domain.ErrExceedsRequest, api.ErrRejected},
		{domain.ErrInvalidEvidence, api.ErrRejected},
		{application.ErrCarrierNotConfirmed, api.ErrRejected},
		{application.ErrReferenceInUse, api.ErrRejected},
		{application.ErrLocationRefused, api.ErrRejected},
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
