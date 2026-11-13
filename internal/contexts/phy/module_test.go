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
		b.Close(make([]domain.CargoScope, 1), at, "", "ops", at),
		b.Reopen("late cargo", "ops", at, 0, domain.Stuffed{}),
		m.Detach(b, domain.ReasonCustomerRequest, "", "ops", at),
		a.Cancel("no cargo", "ops", at, 0, 0),
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

// TestThePhysicalModelEmitsThePublishedEventTypes runs a tender of an AIR consolidation through every cargo lot
// and load execution command and checks that its events are exactly the types package api publishes for them.
func TestThePhysicalModelEmitsThePublishedEventTypes(t *testing.T) {
	loc := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	kg := kernel.Quantity{Unit: "KGM", Amount: 300}
	route := domain.Route{Origin: loc("CNPVG"), Destination: loc("USJFK"), OriginIATA: "PVG", DestinationIATA: "JFK", LocationEdition: "E1"}
	intent := domain.LoadIntent{ShipmentID: "SH-1", Revision: 2, Status: domain.IntentPlanned, PlanVersion: 1, LegalEntityID: "LE", Mode: domain.ModeAir,
		Route: route, DepartureWindow: kernel.ValidityInterval{From: at, Until: at.Add(96 * time.Hour)}, Scope: domain.CargoScope{Packs: 6, GrossWeight: kg},
		Parts: []domain.Part{{ObligationID: "OB-1", ObligationVersion: 1, OrderID: "O-1", Packs: 6, PackageType: "CT", GrossWeight: kg}}, ConsolidationID: "C-1"}
	ev := func(ref string) domain.Evidence {
		return domain.Evidence{Source: "WMS", Reference: ref, OccurredAt: at}
	}
	c, err := domain.CreateConsolidation("T", "C-1", domain.ModeAir, domain.KindAirConsol, "LE", route, at.Add(72*time.Hour), at.Add(48*time.Hour), "ops", at)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := domain.NewMembership("T", "SH-1")
	lot, err := domain.ReceiveCargo("T", "L-1", domain.Facility{Location: loc("CNPVG"), Code: "WH-1"}, intent,
		domain.ReceiptLine{Packs: 6, PackageType: "CT", Condition: domain.ConditionWet, Evidence: ev("R-1")}, "ops", at)
	if err != nil {
		t.Fatal(err)
	}
	screening := func(o domain.Outcome) domain.Screening {
		return domain.Screening{Outcome: o, Screener: "PVG", Method: "XRAY", Reference: "SC-" + string(o), Evidence: ev("SC-" + string(o))}
	}
	if err = errors.Join(m.Attach(c, intent, "ops", at), c.Close(make([]domain.CargoScope, 1), at, "", "ops", at),
		lot.Measure(domain.Measurement{GrossWeight: kg, Method: domain.MethodScale, Evidence: ev("M-1")}, "ops", at),
		lot.Screen(screening(domain.ScreeningFailed), "ops", at), lot.Complete(intent, "ops", at),
		lot.Resolve(1, domain.ResolvedByAccepted, "", "dried, repacked", intent, "ops", at)); err != nil {
		t.Fatal(err)
	}
	x, err := domain.OpenLoadExecution("T", "X-1", c, domain.ExecutionTender, domain.Facility{Location: loc("CNPVG"), Code: "T1"}, "ops", at)
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := m.Live()
	if err = errors.Join(x.Start("ops", at), x.Load(domain.LoadLine{Packs: 6, Evidence: ev("TD-1")}, c, lot, entry, domain.LotLoad{}, 0, "ops", at),
		lot.Screen(screening(domain.ScreeningPassed), "ops", at), x.ReleaseHold(1, ev("REL-1"), lot.Screened(), "ops", at),
		x.Unload(1, 1, domain.ReversalMisloaded, ev("UN-1"), lot, "ops", at),
		x.Load(domain.LoadLine{Packs: 1, Evidence: ev("TD-2")}, c, lot, entry, domain.LotLoad{Packs: 5, GrossWeight: 250}, 5, "ops", at),
		x.Close(ev("TR-1"), "ops", at)); err != nil {
		t.Fatal(err)
	}
	if _, _, err = x.RecordCarrierEvidence(domain.CarrierEvidence{Fact: domain.CarrierOffloaded, Pieces: 1, Transport: "MU587", Evidence: ev("FSU-1")}, "ops", at); err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(x.ResolveDiscrepancy(1, domain.ResolvedByAccepted, "", "flies on MU589 under the same MAWB", "ops", at),
		c.Reconcile([]domain.MemberCheck{{Entry: entry, Intent: intent, Loaded: x.Tendered()}}, 0, x.OpenDiscrepancies(), "ops", at),
		lot.Close(6, 6, intent, "ops", at)); err != nil {
		t.Fatal(err)
	}
	published := map[string]bool{api.ConsolidationReconciled: true, api.CargoReceived: true, api.CargoMeasured: true, api.ScreeningResultRecorded: true,
		api.CargoReceiptCompleted: true, api.CargoDiscrepancyRecorded: true, api.CargoDiscrepancyResolved: true, api.CargoLotClosed: true,
		api.LoadExecutionOpened: true, api.LoadingStarted: true, api.CargoLoaded: true, api.CargoUnloaded: true, api.LoadExecutionHeld: true,
		api.LoadExecutionHoldReleased: true, api.LoadExecutionResumed: true, api.LoadExecutionClosed: true, api.CarrierLoadEvidenceRecorded: true,
		api.LoadingDiscrepancyRecorded: true, api.LoadingDiscrepancyResolved: true}
	emitted := map[string]bool{}
	for _, e := range append(append(c.Pending(), lot.Pending()...), x.Pending()...) {
		if e.Type == api.ConsolidationCreated || e.Type == api.ConsolidationClosed {
			continue
		}
		if !published[e.Type] {
			t.Errorf("event type %s is not published by package api", e.Type)
		}
		emitted[e.Type] = true
	}
	if len(emitted) != len(published) {
		t.Fatalf("emitted %v, published %v", emitted, published)
	}
	loaded := x.Pending()[2]
	envelope, err := postgres.PHYEnvelope("cmd-1", loaded, at)
	if err != nil || string(envelope.Payload) != `{"tenant_id":"T","subject_id":"X-1","consolidation_id":"C-1","shipment_id":"SH-1","lot_id":"L-1","execution_id":"X-1","item_no":1}` ||
		envelope.PartitionKey != "T:X-1" {
		t.Fatalf("CargoLoaded envelope: %s %s %v", envelope.Payload, envelope.PartitionKey, err)
	}
	second, _ := postgres.PHYEnvelope("cmd-1", lot.Pending()[1], at)
	third, _ := postgres.PHYEnvelope("cmd-1", domain.Event{Type: lot.Pending()[1].Type, At: at, TenantID: "T", SubjectID: "L-1", LotID: "L-1", ItemNo: 2}, at)
	if second.ID == third.ID {
		t.Fatal("events of one command about two items have two ids")
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
		{domain.ErrConservation, api.ErrRejected},
		{domain.ErrLiveLoadLines, api.ErrRejected},
		{domain.ErrLotInForce, api.ErrRejected},
		{domain.ErrEvidenceReused, api.ErrRejected},
		{domain.ErrNotReconciled, api.ErrRejected},
		{domain.ErrHoldNotReleasable, api.ErrRejected},
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
