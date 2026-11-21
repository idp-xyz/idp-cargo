package sea

import (
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/contexts/sea/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/sea/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// V2.89: the container taken back out of the terminal is the existing container event with a new kind as its reason;
// the published kinds are the model's, and the event's shape and its id rule are the gate-in's.
func TestAReturnIsPublishedAsTheContainerEventWithTheNewKind(t *testing.T) {
	for _, pair := range [][2]string{{api.EventGateOut, domain.EventGateOut}, {api.EventGateIn, domain.EventGateIn}, {api.WarningReturnDatedAfterReseal, domain.WarnReturnDatedAfterReseal}, {api.WarningCarrierLoadingBeforeReseal, domain.WarnCarrierLoadingBeforeReseal}} {
		if pair[0] != pair[1] || pair[0] == "" {
			t.Errorf("api %q, model %q", pair[0], pair[1])
		}
	}
	if api.EventGateOut != "GATE_OUT_TERMINAL" || api.WarningReturnDatedAfterReseal != "RETURN_DATED_AFTER_RESEAL" {
		t.Fatalf("the published kinds: %s %s", api.EventGateOut, api.WarningReturnDatedAfterReseal)
	}
	out := domain.Event{Type: api.ContainerEventRecorded, At: at, TenantID: "T", SubjectID: "CV-1", ConsolidationID: "C-1", MovementID: "OM-1", EventNo: 2, Reason: api.EventGateOut}
	returned, err := postgres.SEAEnvelope("cmd-8", out, at)
	if err != nil || string(returned.Payload) != `{"tenant_id":"T","subject_id":"CV-1","consolidation_id":"C-1","movement_id":"OM-1","event_no":2,"reason":"GATE_OUT_TERMINAL"}` ||
		returned.PartitionKey != "T:CV-1" || returned.Subject != "CV-1" {
		t.Fatalf("returned: %s %v", returned.Payload, err)
	}
	out.EventNo = 3
	if next, _ := postgres.SEAEnvelope("cmd-8", out, at); next.ID == returned.ID {
		t.Fatal("two container events share an event id")
	}
}

// releaseEvents are the events of a conflict with a carrier's loading report released by the operator: a container
// taken as stuffed with PHY's seal S1, PHY's reseal S2 followed, the carrier's report that names the container dated
// before the reseal, and the release.
func releaseEvents(t *testing.T) []domain.Event {
	t.Helper()
	port := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	sha, ham := port("CNSHG"), port("DEHAM")
	booking := domain.Booking{BookingID: "BK-1", Revision: 3, Kind: domain.KindOwn, InForce: true, ConsumerID: "C-1", LegalEntityID: "LE",
		Carrier: domain.Carrier{PartyID: "line-1", PartyRevision: 1, Role: "CARRIER"}, Origin: sha, Destination: ham, Edition: "E1",
		CapacityVersion: 1, Lines: 1, Legs: []domain.Leg{{Vessel: "EVER GIVEN", Voyage: "0123W", From: sha, To: ham, ETD: at.Add(96 * time.Hour)}}}
	kg := kernel.Quantity{Unit: "KGM", Amount: 100}
	p := domain.Plan{ShipmentID: "SH-1", Revision: 1, Status: "PLANNED", PlanVersion: 1, LegalEntityID: "LE", Mode: "SEA", ContainerMode: "LCL",
		Origin: sha, Destination: ham, Window: kernel.ValidityInterval{From: at, Until: at.Add(120 * time.Hour)}, Packs: 2, GrossWeight: kg,
		ConsolidationID: "C-1", Parts: []domain.Part{{ObligationID: "OB-1", ObligationVersion: 1, OrderID: "O-1", Packs: 2, PackageType: "CT", GrossWeight: kg}}}
	om, err := domain.OpenMovement("T", "OM-3", "C-1", booking, "ops", at)
	if err != nil {
		t.Fatal(err)
	}
	p.EvidenceVersion, p.MeasuredBasis, p.MeasuredPacks, p.MeasuredWeight = 1, domain.BasisTendered, 2, p.GrossWeight
	p.Stuffings = []domain.Stuffing{{ExecutionID: "X-1", LoadUnitNo: 1, TypeCode: "22G1", Packs: 2, GrossWeight: p.GrossWeight, LineNos: []int{1}, Seal: "S1", SealedAt: at}}
	v, err := domain.AssignContainer("CV-1", om, 1, "CSQU3054383", "22G1", domain.Plate{}, "ops", at)
	if err != nil {
		t.Fatal(err)
	}
	if err = om.Associate(p, "H1", "ops", at); err != nil {
		t.Fatal(err)
	}
	if err = v.RecordSeal("S1", domain.SealShipper, at, "tally", "ops", at); err != nil {
		t.Fatal(err)
	}
	if err = v.StuffingDone(om, "", "ops", at); err != nil {
		t.Fatal(err)
	}
	p.Revision++
	p.Stuffings = []domain.Stuffing{p.Stuffings[0]}
	p.Stuffings[0].Seal, p.Stuffings[0].SealedAt = "S2", at.Add(2*time.Hour)
	if err = om.Refresh(p, "ops", at.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err = domain.FollowSeals(om.State().Entries[len(om.State().Entries)-1], []*domain.Visit{v}, "ops", at.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	fact := domain.CarrierFact{Kind: domain.CarrierLoaded, ContainerNumber: "CSQU3054383", Vessel: "EVER GIVEN", Voyage: "0123W", Source: "line", Reference: "LD-1",
		SHA256: strings.Repeat("ab", 32), OccurredAt: at.Add(time.Hour)}
	if _, _, err = om.RecordCarrierFact(fact, []string{"CSQU3054383"}, "ops", at.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err = v.AcceptCarrierLoadingConflict(om, 1, "the line confirmed it loaded another box", "ops", at.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	return v.Pending()
}

// The release is published as CarrierLoadingConflictReleased with its number, and the payload carries identifiers only.
func TestTheReleaseOfAConflictIsPublishedWithItsNumber(t *testing.T) {
	events := releaseEvents(t)
	e := events[len(events)-1]
	if e.Type != api.CarrierLoadingConflictReleased || e.ReleaseNo != 1 || e.SubjectID != "CV-1" || e.Reason != "" {
		t.Fatalf("the event: %+v", e)
	}
	envelope, err := postgres.SEAEnvelope("cmd-9", e, at)
	if err != nil || string(envelope.Payload) != `{"tenant_id":"T","subject_id":"CV-1","consolidation_id":"C-1","movement_id":"OM-3","release_no":1}` ||
		envelope.PartitionKey != "T:CV-1" || strings.Contains(string(envelope.Payload), "line confirmed") {
		t.Fatalf("envelope: %s %v", envelope.Payload, err)
	}
	e.ReleaseNo = 2
	if next, _ := postgres.SEAEnvelope("cmd-9", e, at); next.ID == envelope.ID {
		t.Fatal("two releases share an event id")
	}
	if api.CarrierLoadingConflictReleased != "CarrierLoadingConflictReleased.v1" || api.WarningCarrierConflictReleased != domain.WarnCarrierConflictReleased {
		t.Fatal("the published names")
	}
}
