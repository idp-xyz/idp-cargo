package sea

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	bentorepo "go.idp.xyz/idp-bento-go/repository"
	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/contexts/sea/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/sea/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/sea/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

var at = time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)

func TestEventsMatchPublishedContract(t *testing.T) {
	e := domain.Event{Type: api.HouseBillAssociated, At: at, TenantID: "T", SubjectID: "OM-1", ConsolidationID: "C-1", ShipmentID: "SH-1", EntryNo: 1,
		BookingID: "BK-1", CapacityVersion: 1}
	envelope, err := postgres.SEAEnvelope("cmd-1", e, at)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"tenant_id":"T","subject_id":"OM-1","consolidation_id":"C-1","shipment_id":"SH-1","entry_no":1,"booking_id":"BK-1","capacity_version":1}`
	if envelope.Source != api.Source || string(envelope.Payload) != want || envelope.PartitionKey != "T:OM-1" {
		t.Fatalf("envelope: %s %s", envelope.Payload, envelope.PartitionKey)
	}
	bill, err := postgres.SEAEnvelope("cmd-3", domain.Event{Type: api.MasterBillRecorded, At: at, TenantID: "T", SubjectID: "OM-1",
		ConsolidationID: "C-1", BillNo: 2, BookingID: "BK-1", CapacityVersion: 1}, at)
	if err != nil || string(bill.Payload) != `{"tenant_id":"T","subject_id":"OM-1","consolidation_id":"C-1","bill_no":2,"booking_id":"BK-1","capacity_version":1}` {
		t.Fatalf("master bill recorded: %s %v", bill.Payload, err)
	}
	var payload api.EventPayload
	if err = json.Unmarshal(envelope.Payload, &payload); err != nil || payload.ShipmentID != "SH-1" {
		t.Fatalf("payload %+v %v", payload, err)
	}
	again, _ := postgres.SEAEnvelope("cmd-1", e, at.Add(time.Hour))
	other, _ := postgres.SEAEnvelope("cmd-2", e, at)
	if envelope.ID != again.ID || envelope.ID == other.ID {
		t.Fatal("event ids")
	}
}

// TestTheModelEmitsThePublishedEventTypes runs every command of the model and checks that its events are
// exactly the types package api publishes.
func TestTheModelEmitsThePublishedEventTypes(t *testing.T) {
	port := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	sha, ham := port("CNSHG"), port("DEHAM")
	booking := func(v int) domain.Booking {
		return domain.Booking{BookingID: "BK-1", Revision: 3, Kind: domain.KindOwn, InForce: true, ConsumerID: "C-1", LegalEntityID: "LE",
			Carrier: domain.Carrier{PartyID: "line-1", PartyRevision: 1, Role: "CARRIER"}, Origin: sha, Destination: ham, Edition: "E1",
			CapacityVersion: v, Lines: 1, Legs: []domain.Leg{{Vessel: "EVER GIVEN", Voyage: "0123W", From: sha, To: ham, ETD: at.Add(96 * time.Hour)}}}
	}
	plan := func(v int, consol string) domain.Plan {
		kg := kernel.Quantity{Unit: "KGM", Amount: 100}
		return domain.Plan{ShipmentID: "SH-1", Revision: int64(v), Status: "PLANNED", PlanVersion: v, LegalEntityID: "LE", Mode: "SEA", ContainerMode: "LCL",
			Origin: sha, Destination: ham, Window: kernel.ValidityInterval{From: at, Until: at.Add(120 * time.Hour)}, Packs: 2, GrossWeight: kg,
			ConsolidationID: consol, Parts: []domain.Part{{ObligationID: "OB-1", ObligationVersion: 1, OrderID: "O-1", Packs: 2, PackageType: "CT", GrossWeight: kg}}}
	}
	evidence := domain.Evidence{SHA256: strings.Repeat("0f", 32), StorageID: "mail/bl.pdf", MediaType: "application/pdf", CapturedAt: at}
	m, err := domain.OpenMovement("T", "OM-1", "C-1", booking(1), "ops", at)
	if err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(m.LinkBooking(booking(2), "ops", at), m.RecordMasterBill("MAEU123456789", domain.BillCarrierMBL, evidence, "ops", at),
		m.Associate(plan(1, "C-1"), "H1", "ops", at), m.Refresh(plan(2, "C-1"), "ops", at), m.EndAssociation(plan(2, "C-2"), "ops", at),
		m.Cancel("no cargo", "ops", at)); err != nil {
		t.Fatal(err)
	}
	s, err := domain.RegisterSeries("T", "LE", "SHAL{seq:7}", 1, "", "ops", at)
	if err != nil {
		t.Fatal(err)
	}
	published := map[string]bool{api.OceanMovementOpened: true, api.VoyageLegsLinked: true, api.MasterBillRecorded: true, api.HouseBillAssociated: true,
		api.HouseBillFactsRefreshed: true, api.HouseBillAssociationEnded: true, api.OceanMovementCancelled: true, api.HblSeriesRegistered: true}
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
	// the published vocabulary is the model's
	if api.KindSeaLCLOwn != string(domain.KindOwn) || api.KindSeaLCLCoLoad != string(domain.KindCoLoad) || api.RoleCoLoader != domain.KindCoLoad.Role() ||
		api.RoleCarrier != domain.KindOwn.Role() || api.MasterBillCarrierMBL != domain.BillCarrierMBL ||
		api.MasterBillCarrierSeaWaybill != domain.BillCarrierSeaWaybill || api.MasterBillCoLoad != domain.BillCoLoad ||
		api.MasterBillCurrent != domain.BillCurrent || api.MasterBillSuperseded != domain.BillSuperseded || api.MasterBillVoided != domain.BillVoided ||
		api.StateOpen != string(domain.StateOpen) || api.StateCancelled != string(domain.StateCancelled) || api.EntryAssociated != domain.StartAssociated ||
		api.EntryRefreshed != domain.StartRefreshed || api.EntryDetached != domain.EndDetached || api.EntryTransferred != domain.EndTransferred ||
		api.EntryShipmentCancelled != domain.EndCancelled || api.BasisPlanned != domain.BasisPlanned || api.BasisReceived != domain.BasisReceived ||
		api.BasisTendered != domain.BasisTendered {
		t.Fatal("package api and the model name a kind, role, status or basis differently")
	}
}

func TestViewStatesTotalsOnlyInConvertedUnits(t *testing.T) {
	port := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	sha, ham := port("CNSHG"), port("DEHAM")
	m, err := domain.OpenMovement("T", "OM-1", "C-1", domain.Booking{BookingID: "BK-1", Revision: 3, Kind: domain.KindCoLoad, InForce: true, ConsumerID: "C-1",
		LegalEntityID: "LE", Carrier: domain.Carrier{PartyID: "coloader-x", PartyRevision: 1, Role: "CO_LOADER"}, Origin: sha, Destination: ham, Edition: "E1",
		CapacityVersion: 1, Lines: 1, Legs: []domain.Leg{{Vessel: "EVER GIVEN", Voyage: "0123W", From: sha, To: ham, ETD: at.Add(96 * time.Hour)}}}, "ops", at)
	if err != nil {
		t.Fatal(err)
	}
	house := func(shipment string, weight kernel.Quantity) domain.Plan {
		return domain.Plan{ShipmentID: shipment, Revision: 1, Status: "PLANNED", PlanVersion: 1, LegalEntityID: "LE", Mode: "SEA", Origin: sha, Destination: ham,
			Window: kernel.ValidityInterval{From: at, Until: at.Add(120 * time.Hour)}, Packs: 2, GrossWeight: weight, ConsolidationID: "C-1",
			Parts: []domain.Part{{ObligationID: "OB-" + shipment, ObligationVersion: 1, OrderID: "O-1", Packs: 2, PackageType: "CT", GrossWeight: weight}}}
	}
	if err = m.Associate(house("SH-1", kernel.Quantity{Unit: "KGM", Amount: 250}), "H1", "ops", at); err != nil {
		t.Fatal(err)
	}
	v := view(m, 2)
	if v.Kind != api.KindSeaLCLCoLoad || v.CarrierRole != api.RoleCoLoader || v.Packs != 2 || v.GrossWeight != (kernel.Quantity{Unit: "GRM", Amount: 250_000}) ||
		v.Volume != (kernel.Quantity{}) || v.MasterBillNumber != "" || len(v.Legs) != 1 || !v.Legs[0].Current || len(v.Houses) != 1 || v.Revision != 2 {
		t.Fatalf("view: %+v", v)
	}
	if err = m.Associate(house("SH-2", kernel.Quantity{Unit: "LBR", Amount: 500}), "H2", "ops", at); err != nil {
		t.Fatal(err)
	}
	if v = view(m, 3); v.Packs != 4 || v.GrossWeight != (kernel.Quantity{}) || v.Houses[1].GrossWeight.Unit != "LBR" {
		t.Fatalf("view with a house in pounds: %+v", v)
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
		{domain.ErrNotAttached, api.ErrRejected},
		{domain.ErrInvalidBooking, api.ErrRejected},
		{domain.ErrInvalidMasterBill, api.ErrRejected},
		{domain.ErrInvalidHBL, api.ErrRejected},
		{domain.ErrSeriesExhausted, api.ErrRejected},
		{application.ErrHBLInUse, api.ErrRejected},
		{application.ErrHBLDiffers, api.ErrRejected},
		{application.ErrMasterBillInUse, api.ErrRejected},
		{application.ErrNoSeries, api.ErrRejected},
		{application.ErrUnknownBooking, api.ErrRejected},
		{application.ErrUnknownShipment, api.ErrRejected},
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
