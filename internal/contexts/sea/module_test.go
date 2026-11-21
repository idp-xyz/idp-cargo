package sea

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	// A container visit's event has the visit as subject and names its movement and, for a seal, the seal's number
	// in the visit's history; its id covers both, so that two seals never share one.
	seal := domain.Event{Type: api.SealRecorded, At: at, TenantID: "T", SubjectID: "CV-1", ConsolidationID: "C-1", MovementID: "OM-1", SealNo: 2}
	sealed, err := postgres.SEAEnvelope("cmd-4", seal, at)
	if err != nil || string(sealed.Payload) != `{"tenant_id":"T","subject_id":"CV-1","consolidation_id":"C-1","movement_id":"OM-1","seal_no":2}` ||
		sealed.PartitionKey != "T:CV-1" || sealed.Subject != "CV-1" {
		t.Fatalf("seal recorded: %s %v", sealed.Payload, err)
	}
	seal.SealNo = 3
	if next, _ := postgres.SEAEnvelope("cmd-4", seal, at); next.ID == sealed.ID {
		t.Fatal("two seals share an event id")
	}
	// A verified-gross-mass event names the assertion's version and nothing of the signatory.
	vgm := domain.Event{Type: api.VgmAsserted, At: at, TenantID: "T", SubjectID: "CV-1", ConsolidationID: "C-1", MovementID: "OM-1", AssertionNo: 1}
	asserted, err := postgres.SEAEnvelope("cmd-5", vgm, at)
	if err != nil || string(asserted.Payload) != `{"tenant_id":"T","subject_id":"CV-1","consolidation_id":"C-1","movement_id":"OM-1","assertion_no":1}` ||
		strings.Contains(string(asserted.Payload), "Ada") {
		t.Fatalf("vgm asserted: %s %v", asserted.Payload, err)
	}
	vgm.AssertionNo = 2
	if next, _ := postgres.SEAEnvelope("cmd-5", vgm, at); next.ID == asserted.ID {
		t.Fatal("two assertions share an event id")
	}
	// A container event names its number and its kind. A stuffing recorded before this carries no event number, so
	// its id is unchanged.
	happened := domain.Event{Type: api.ContainerEventRecorded, At: at, TenantID: "T", SubjectID: "CV-1", ConsolidationID: "C-1", MovementID: "OM-1",
		EventNo: 1, Reason: api.EventGateIn}
	gated, err := postgres.SEAEnvelope("cmd-6", happened, at)
	if err != nil || string(gated.Payload) != `{"tenant_id":"T","subject_id":"CV-1","consolidation_id":"C-1","movement_id":"OM-1","event_no":1,"reason":"GATE_IN_TERMINAL"}` {
		t.Fatalf("gated in: %s %v", gated.Payload, err)
	}
	happened.EventNo = 2
	if next, _ := postgres.SEAEnvelope("cmd-6", happened, at); next.ID == gated.ID {
		t.Fatal("two container events share an event id")
	}
	stuffedEvent := domain.Event{Type: api.ContainerEventRecorded, At: at, TenantID: "T", SubjectID: "CV-1", ConsolidationID: "C-1", MovementID: "OM-1", Reason: api.ContainerEventStuffed}
	stuffedEnvelope, _ := postgres.SEAEnvelope("cmd-7", stuffedEvent, at)
	if strings.Contains(string(stuffedEnvelope.Payload), "event_no") {
		t.Fatalf("a stuffing event gained a number: %s", stuffedEnvelope.Payload)
	}
	// an event of a movement keeps the id it had before SEA recorded visits: a command replayed across the upgrade
	// yields the same event
	sum := sha256.Sum256([]byte("cmd-1\x00" + api.HouseBillAssociated + "\x00T\x00OM-1\x00SH-1\x001\x000"))
	if string(envelope.ID) != hex.EncodeToString(sum[:]) {
		t.Fatalf("the id of a movement's event changed: %s", envelope.ID)
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
		m.Associate(plan(1, "C-1"), "H1", "ops", at), m.Refresh(plan(2, "C-1"), "ops", at), m.EndAssociation(plan(2, "C-2"), "ops", at)); err != nil {
		t.Fatal(err)
	}
	if _, _, err = m.RecordCarrierFact(domain.CarrierFact{Kind: domain.CarrierDeparted, BillNumber: "MAEU123456789", Vessel: "EVER GIVEN", Voyage: "0123W",
		Source: "line", Reference: "DEP-1", SHA256: strings.Repeat("ab", 32), OccurredAt: at}, nil, "ops", at); err != nil {
		t.Fatal(err)
	}
	if err = m.Cancel("no cargo", "ops", at); err != nil {
		t.Fatal(err)
	}
	s, err := domain.RegisterSeries("T", "LE", "SHAL{seq:7}", 1, "", "ops", at)
	if err != nil {
		t.Fatal(err)
	}
	// a container assigned, sealed and stuffed on a second movement, and one given back unused
	om, err := domain.OpenMovement("T", "OM-2", "C-1", booking(1), "ops", at)
	if err != nil {
		t.Fatal(err)
	}
	stuffed := plan(1, "C-1")
	stuffed.EvidenceVersion, stuffed.MeasuredBasis, stuffed.MeasuredPacks, stuffed.MeasuredWeight = 1, domain.BasisTendered, 2, stuffed.GrossWeight
	stuffed.Stuffings = []domain.Stuffing{{ExecutionID: "X-1", LoadUnitNo: 1, TypeCode: "22G1", Packs: 2, GrossWeight: stuffed.GrossWeight, LineNos: []int{1},
		Seal: "S1", SealedAt: at}}
	v, err := domain.AssignContainer("CV-1", om, 1, "CSQU3054383", "22G1", domain.Plate{}, "ops", at)
	if err != nil {
		t.Fatal(err)
	}
	free, err := domain.AssignContainer("CV-2", om, 2, "TEMU1234565", "22G1", domain.Plate{}, "ops", at)
	if err = errors.Join(err, om.Associate(stuffed, "H1", "ops", at), v.RecordSeal("S1", domain.SealShipper, at, "tally", "ops", at),
		v.StuffingDone(om, "", "ops", at), v.AssertVGM(om, kernel.Quantity{Unit: "KGM", Amount: 2500}, domain.VgmMethod1, "LE", 1, at, "", "Ada Lovelace", "ops", at),
		v.RecordVgmResponse(domain.VgmAccepted, "carrier portal", "ops", at), free.Release("not used", "ops", at)); err != nil {
		t.Fatal(err)
	}
	if _, _, err = v.RecordContainerEvent(domain.EventGateIn, sha, at, "terminal", "GIN-1", "ops", at); err != nil {
		t.Fatal(err)
	}
	if _, _, err = v.RecordContainerEvent(domain.EventLoaded, sha, at, "vessel", "LOA-1", "ops", at); err != nil {
		t.Fatal(err)
	}
	published := map[string]bool{api.OceanMovementOpened: true, api.VoyageLegsLinked: true, api.MasterBillRecorded: true, api.HouseBillAssociated: true,
		api.HouseBillFactsRefreshed: true, api.HouseBillAssociationEnded: true, api.OceanMovementCancelled: true, api.HblSeriesRegistered: true,
		api.ContainerAssigned: true, api.SealRecorded: true, api.ContainerEventRecorded: true, api.ContainerReleased: true,
		api.VgmAsserted: true, api.VgmResponseRecorded: true, api.OceanCarrierFactRecorded: true, api.CarrierLoadingConflictReleased: true}
	emitted := map[string]bool{}
	for _, e := range slices.Concat(m.Pending(), s.Pending(), v.Pending(), free.Pending(), releaseEvents(t)) {
		if e.Type == api.ContainerEventRecorded && e.Reason != api.ContainerEventStuffed && e.Reason != api.EventGateIn && e.Reason != api.EventLoaded {
			t.Errorf("%s carries the reason %q", e.Type, e.Reason)
		}
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
		api.StateOpen != string(domain.StateOpen) || api.StateFinal != string(domain.StateFinal) || api.StateCancelled != string(domain.StateCancelled) || api.EntryAssociated != domain.StartAssociated ||
		api.EntryRefreshed != domain.StartRefreshed || api.EntryDetached != domain.EndDetached || api.EntryTransferred != domain.EndTransferred ||
		api.EntryShipmentCancelled != domain.EndCancelled || api.BasisPlanned != domain.BasisPlanned || api.BasisReceived != domain.BasisReceived ||
		api.BasisTendered != domain.BasisTendered || api.VisitAssigned != string(domain.VisitAssigned) || api.VisitStuffed != string(domain.VisitStuffed) ||
		api.VisitReleased != string(domain.VisitReleased) || api.VisitVGM != string(domain.VisitVGM) ||
		api.VisitGatedIn != string(domain.VisitGatedIn) || api.VisitLoaded != string(domain.VisitLoaded) ||
		api.EventGateIn != domain.EventGateIn || api.EventLoaded != domain.EventLoaded || api.EventRolled != domain.EventRolled ||
		api.VgmMethod1 != domain.VgmMethod1 || api.VgmMethod2 != domain.VgmMethod2 || api.VgmAccepted != domain.VgmAccepted || api.VgmRejected != domain.VgmRejected ||
		api.SealCarrier != domain.SealCarrier || api.SealShipper != domain.SealShipper ||
		api.SealCustoms != domain.SealCustoms || api.PlateCSC != domain.PlateCSC || api.PlateCarrier != domain.PlateCarrier ||
		api.ContainerEventStuffed != domain.ContainerStuffedEvent {
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
		{application.ErrContainerInUse, api.ErrRejected},
		{application.ErrUnitHasContainer, api.ErrRejected},
		{application.ErrContainersAssigned, api.ErrRejected},
		{domain.ErrInvalidVisit, api.ErrRejected},
		{domain.ErrInvalidContainer, api.ErrRejected},
		{domain.ErrInvalidSeal, api.ErrRejected},
		{domain.ErrNotStuffed, api.ErrRejected},
		{domain.ErrStuffingDiffers, api.ErrRejected},
		{domain.ErrCargoInContainer, api.ErrRejected},
		{domain.ErrInvalidVGM, api.ErrRejected},
		{domain.ErrInvalidEvent, api.ErrRejected},
		{domain.ErrNotFinalizable, api.ErrRejected},
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
