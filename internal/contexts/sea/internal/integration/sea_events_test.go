//go:build integration

package integration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// TestContainerEventsOnPostgres gates a container in only after its VGM, then records it loaded, and keeps the
// facts off the house. The same terminal message again changes nothing.
func TestContainerEventsOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.w.booking("BK-1", "C-1", "EVER GIVEN", "9811000", "0123W", 1, f.hours(72))
	if _, err := f.open("OM-1", "C-1", "BK-1"); err != nil {
		t.Fatal(err)
	}
	f.series(t)
	f.w.plan("SH-1", "C-1", 1)
	if _, err := f.associate("assoc-1", "OM-1", "SH-1", "", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.AssignContainer(ctx, api.AssignContainer{CommandID: "assign-CV-1", TenantID: f.tenant, ActorID: "ops-1", VisitID: "CV-1",
		MovementID: "OM-1", LoadUnitNo: 1, ContainerNumber: "CSQU3054383", SizeType: "22G1", TareKg: 2200, MaxGrossKg: 30480, PlateSource: api.PlateCSC,
		OccurredAt: f.hours(1)}); err != nil {
		t.Fatal(err)
	}
	shanghai := kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: "CNSHG"}
	if _, err := f.module.RecordContainerEvent(ctx, api.RecordContainerEvent{CommandID: "gate-early", TenantID: f.tenant, ActorID: "ops-1", VisitID: "CV-1",
		ExpectedRevision: 1, Kind: api.EventGateIn, Location: shanghai, HappenedAt: f.hours(1), Source: "terminal", Reference: "GIN-1",
		OccurredAt: f.hours(1)}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("gated in before the VGM: %v", err)
	}
	picked, err := f.module.RecordContainerEvent(ctx, api.RecordContainerEvent{CommandID: "pickup-1", TenantID: f.tenant, ActorID: "ops-1", VisitID: "CV-1",
		ExpectedRevision: 1, Kind: api.EventEmptyPickedUp, Location: shanghai, HappenedAt: f.hours(1), Source: "depot", Reference: "EMP-1", OccurredAt: f.hours(1)})
	if err != nil || picked.EventNo != 1 || picked.NewRevision != 2 || f.visit(t, "CV-1").Status != api.VisitAssigned {
		t.Fatalf("picked up empty: %+v %v", picked, err)
	}
	if _, err = f.seal("seal-1", "CV-1", 2, "CN-SEAL-1", api.SealShipper, 2); err != nil {
		t.Fatal(err)
	}
	f.stuffing("SH-1", "X-1", 1, "22G1", "CN-SEAL-1")
	if _, err = f.module.RefreshHouseBillFacts(ctx, api.RefreshHouseBillFacts{CommandID: "refresh-1", TenantID: f.tenant, ActorID: "ops-1", MovementID: "OM-1",
		ExpectedRevision: 2, ShipmentID: "SH-1", OccurredAt: f.hours(3)}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.stuffingDone("done-1", "CV-1", 3, "", 3); err != nil {
		t.Fatal(err)
	}
	if _, err = f.module.AssertVgm(ctx, api.AssertVgm{CommandID: "vgm-1", TenantID: f.tenant, ActorID: "ops-1", VisitID: "CV-1", ExpectedRevision: 4,
		Mass: kernel.Quantity{Unit: "KGM", Amount: 2200}, Method: api.VgmMethod1, ResponsiblePartyID: "LE", ResponsiblePartyRevision: 1,
		DeterminedAt: f.hours(4), Signatory: "Ada Lovelace", OccurredAt: f.hours(4)}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.module.RecordContainerEvent(ctx, api.RecordContainerEvent{CommandID: "load-early", TenantID: f.tenant, ActorID: "ops-1", VisitID: "CV-1",
		ExpectedRevision: 5, Kind: api.EventLoaded, Location: shanghai, HappenedAt: f.hours(4), Source: "vessel", Reference: "LOA-1",
		OccurredAt: f.hours(4)}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("loaded before the gate: %v", err)
	}
	gated, err := f.module.RecordContainerEvent(ctx, api.RecordContainerEvent{CommandID: "gate-1", TenantID: f.tenant, ActorID: "ops-1", VisitID: "CV-1",
		ExpectedRevision: 5, Kind: api.EventGateIn, Location: shanghai, HappenedAt: f.hours(4), Source: "terminal", Reference: "GIN-1", OccurredAt: f.hours(5)})
	if err != nil || gated.EventNo != 2 || gated.NewRevision != 6 || f.visit(t, "CV-1").Status != api.VisitGatedIn {
		t.Fatalf("gated in: %+v %v", gated, err)
	}
	if again, err := f.module.RecordContainerEvent(ctx, api.RecordContainerEvent{CommandID: "gate-1", TenantID: f.tenant, ActorID: "ops-1", VisitID: "CV-1",
		ExpectedRevision: 5, Kind: api.EventGateIn, Location: shanghai, HappenedAt: f.hours(4), Source: "terminal", Reference: "GIN-1", OccurredAt: f.hours(5)}); err != nil || again != gated {
		t.Fatalf("replayed: %+v %v", again, err)
	}
	same, err := f.module.RecordContainerEvent(ctx, api.RecordContainerEvent{CommandID: "gate-again", TenantID: f.tenant, ActorID: "ops-1", VisitID: "CV-1",
		ExpectedRevision: 6, Kind: api.EventGateIn, Location: shanghai, HappenedAt: f.hours(4), Source: "terminal", Reference: "GIN-1", OccurredAt: f.hours(5)})
	if err != nil || same.EventNo != 2 || same.NewRevision != 6 {
		t.Fatalf("the same message again: %+v %v", same, err)
	}
	if _, err = f.module.RecordContainerEvent(ctx, api.RecordContainerEvent{CommandID: "gate-other", TenantID: f.tenant, ActorID: "ops-1", VisitID: "CV-1",
		ExpectedRevision: 6, Kind: api.EventGateIn, Location: shanghai, HappenedAt: f.hours(5), Source: "terminal", Reference: "GIN-1",
		OccurredAt: f.hours(5)}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("the same reference with another time: %v", err)
	}
	if err = f.past(t, `UPDATE cargo_sea.container_visit SET status='LOADED', revision=revision+1 WHERE tenant_id=$1 AND visit_id='CV-1'`); sqlState(err) != "23514" || !strings.Contains(err.Error(), "loaded") {
		t.Fatalf("loaded without the fact: %v", err)
	}
	loaded, err := f.module.RecordContainerEvent(ctx, api.RecordContainerEvent{CommandID: "load-1", TenantID: f.tenant, ActorID: "ops-1", VisitID: "CV-1",
		ExpectedRevision: 6, Kind: api.EventLoaded, Location: shanghai, HappenedAt: f.hours(6), Source: "vessel", Reference: "LOA-1", OccurredAt: f.hours(6)})
	if err != nil || loaded.EventNo != 3 || loaded.NewRevision != 7 || f.visit(t, "CV-1").Status != api.VisitLoaded {
		t.Fatalf("loaded: %+v %v", loaded, err)
	}
	m := f.get(t, "OM-1")
	if len(m.Houses) != 1 || len(m.Houses[0].Containers) != 1 || m.Houses[0].Containers[0].ContainerNumber != "CSQU3054383" {
		t.Fatalf("the house still names its container: %+v", m.Houses)
	}
	var sawGate bool
	for _, e := range f.outbox(t) {
		if e.Type != api.ContainerEventRecorded {
			continue
		}
		if strings.Contains(e.Raw, "GIN-1") || strings.Contains(e.Raw, "CNSHG") || strings.Contains(e.Raw, "Ada") {
			t.Fatalf("an event carries the place, the source or the name: %s", e.Raw)
		}
		if e.Payload.Reason == api.EventGateIn && e.Payload.EventNo == 2 {
			sawGate = true
		}
	}
	if !sawGate {
		t.Fatal("the gate-in was not published")
	}
	if err = f.past(t, `INSERT INTO cargo_sea.container_event (tenant_id,visit_id,event_no,kind,location_code,occurred_at,source,reference,recorded_by,recorded_at,entered_at)
        VALUES ($1,'CV-1',9,'ROLLED','CNSHG',now(),'line','ROL-9','x',now(),now())`); sqlState(err) != "23514" || !strings.Contains(err.Error(), "step of its visit") {
		t.Fatalf("an event without a step of the visit: %v", err)
	}
}
