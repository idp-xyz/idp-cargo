//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"

	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
)

// TestOceanCarrierFactsOnPostgres records a carrier report after the facts are final without moving the movement.
func TestOceanCarrierFactsOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.publishOwnFacts(t)
	sha := strings.Repeat("cd", 32)
	got, err := f.module.RecordOceanCarrierFact(ctx, api.RecordOceanCarrierFact{CommandID: "dep-1", TenantID: f.tenant, ActorID: "line-1", MovementID: "OM-1",
		ExpectedRevision: 5, Kind: api.CarrierDeparted, BillNumber: "MAEU123456789", Vessel: "EVER GIVEN", Voyage: "0123W", Source: "line", Reference: "DEP-1",
		SHA256: sha, HappenedAt: f.hours(6), OccurredAt: f.hours(6)})
	facts, factsErr := f.module.GetOceanDocFacts(ctx, api.GetOceanDocFacts{TenantID: f.tenant, MovementID: "OM-1"})
	if err != nil || factsErr != nil || got.FactNo != 1 || got.NewRevision != 5 || f.get(t, "OM-1").State != api.StateFinal || facts.Status != api.DocFactsFinal {
		t.Fatalf("a departure leaves the facts final: %+v %s %v %v", got, facts.Status, err, factsErr)
	}
	again, err := f.module.RecordOceanCarrierFact(ctx, api.RecordOceanCarrierFact{CommandID: "dep-1", TenantID: f.tenant, ActorID: "line-1", MovementID: "OM-1",
		ExpectedRevision: 5, Kind: api.CarrierDeparted, BillNumber: "MAEU123456789", Vessel: "EVER GIVEN", Voyage: "0123W", Source: "line", Reference: "DEP-1",
		SHA256: sha, HappenedAt: f.hours(6), OccurredAt: f.hours(6)})
	if err != nil || again.FactNo != 1 || again.NewRevision != 5 {
		t.Fatalf("replay: %+v %v", again, err)
	}
	same, err := f.module.RecordOceanCarrierFact(ctx, api.RecordOceanCarrierFact{CommandID: "dep-2", TenantID: f.tenant, ActorID: "line-1", MovementID: "OM-1",
		ExpectedRevision: 5, Kind: api.CarrierDeparted, BillNumber: "MAEU123456789", Vessel: "EVER GIVEN", Voyage: "0123W", Source: "line", Reference: "DEP-1",
		SHA256: sha, HappenedAt: f.hours(6), OccurredAt: f.hours(7)})
	if err != nil || same.FactNo != 1 || same.NewRevision != 5 {
		t.Fatalf("the same report: %+v %v", same, err)
	}
	if _, err = f.module.RecordOceanCarrierFact(ctx, api.RecordOceanCarrierFact{CommandID: "dep-other", TenantID: f.tenant, ActorID: "line-1", MovementID: "OM-1",
		ExpectedRevision: 5, Kind: api.CarrierRolled, BillNumber: "MAEU123456789", Vessel: "EVER GIVEN", Voyage: "0124W", Source: "line", Reference: "DEP-1",
		SHA256: sha, HappenedAt: f.hours(7), OccurredAt: f.hours(7)}); err == nil {
		t.Fatal("the same reference with other content was accepted")
	}
	loaded, err := f.module.RecordOceanCarrierFact(ctx, api.RecordOceanCarrierFact{CommandID: "load-fact", TenantID: f.tenant, ActorID: "line-1", MovementID: "OM-1",
		ExpectedRevision: 5, Kind: api.CarrierLoaded, ContainerNumber: "CSQU3054383", Vessel: "EVER GIVEN", Voyage: "0123W", Source: "line", Reference: "LD-9",
		SHA256: sha, HappenedAt: f.hours(6), OccurredAt: f.hours(7)})
	mv := f.get(t, "OM-1")
	if err != nil || loaded.FactNo != 2 || loaded.NewRevision != 5 || mv.State != api.StateFinal || len(mv.CarrierFacts) != 2 || mv.CarrierFacts[1].ContainerNumber != "CSQU3054383" {
		t.Fatalf("a loading fact: %+v %+v %v", loaded, mv.CarrierFacts, err)
	}
	if err = f.past(t, `INSERT INTO cargo_sea.ocean_carrier_fact (tenant_id,movement_id,fact_no,kind,container_number,vessel,voyage,source,reference,
        content_sha256,occurred_at,recorded_by,recorded_at,entered_at)
        VALUES ($1,'OM-1',3,'LOADED_ON_BOARD','MSKU0000080','EVER GIVEN','0123W','line','NO-1','`+sha+`',now(),'x',now(),now())`); sqlState(err) != "23514" || !strings.Contains(err.Error(), "live container") {
		t.Fatalf("a container the movement does not have: %v", err)
	}
	var saw bool
	for _, e := range f.outbox(t) {
		if e.Type == api.OceanCarrierFactRecorded && e.Payload.FactNo == 1 && e.Payload.Reason == api.CarrierDeparted {
			saw = true
		}
		if strings.Contains(e.Raw, "MAEU") || strings.Contains(e.Raw, "EVER") {
			t.Fatalf("an event carries the bill or the vessel: %s", e.Raw)
		}
	}
	if !saw {
		t.Fatal("the departure was not published")
	}
}
