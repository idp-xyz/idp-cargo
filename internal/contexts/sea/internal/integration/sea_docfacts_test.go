//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// TestOceanDocFactsOnPostgres publishes a movement's facts once its container has a seal and a VGM, and a later
// refresh returns the movement to OPEN without rewriting the published version.
func TestOceanDocFactsOnPostgres(t *testing.T) {
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
	if _, err := f.masterBill("bill-1", "OM-1", "MAEU123456789", api.MasterBillCarrierMBL, 2, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.AssignContainer(ctx, api.AssignContainer{CommandID: "assign-CV-1", TenantID: f.tenant, ActorID: "ops-1", VisitID: "CV-1",
		MovementID: "OM-1", LoadUnitNo: 1, ContainerNumber: "CSQU3054383", SizeType: "22G1", TareKg: 2200, MaxGrossKg: 30480, PlateSource: api.PlateCSC,
		OccurredAt: f.hours(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.seal("seal-1", "CV-1", 1, "CN-SEAL-1", api.SealShipper, 2); err != nil {
		t.Fatal(err)
	}
	f.stuffing("SH-1", "X-1", 1, "22G1", "CN-SEAL-1")
	if _, err := f.module.RefreshHouseBillFacts(ctx, api.RefreshHouseBillFacts{CommandID: "refresh-1", TenantID: f.tenant, ActorID: "ops-1", MovementID: "OM-1",
		ExpectedRevision: 3, ShipmentID: "SH-1", OccurredAt: f.hours(3)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.stuffingDone("done-1", "CV-1", 2, "", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.FinalizeOceanDocFacts(ctx, api.FinalizeOceanDocFacts{CommandID: "fin-early", TenantID: f.tenant, ActorID: "ops-1", MovementID: "OM-1",
		ExpectedRevision: 4, OccurredAt: f.hours(4)}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("before the VGM: %v", err)
	}
	if _, err := f.module.AssertVgm(ctx, api.AssertVgm{CommandID: "vgm-1", TenantID: f.tenant, ActorID: "ops-1", VisitID: "CV-1", ExpectedRevision: 3,
		Mass: kernel.Quantity{Unit: "KGM", Amount: 2200}, Method: api.VgmMethod1, ResponsiblePartyID: "LE", ResponsiblePartyRevision: 1,
		DeterminedAt: f.hours(4), Signatory: "Ada Lovelace", OccurredAt: f.hours(4)}); err != nil {
		t.Fatal(err)
	}
	got, err := f.module.FinalizeOceanDocFacts(ctx, api.FinalizeOceanDocFacts{CommandID: "fin-1", TenantID: f.tenant, ActorID: "ops-1", MovementID: "OM-1",
		ExpectedRevision: 4, OccurredAt: f.hours(5)})
	if err != nil || got.FactsVersion != 1 || got.NewRevision != 5 {
		t.Fatalf("finalized: %+v %v", got, err)
	}
	if m := f.get(t, "OM-1"); m.State != api.StateFinal || m.DocFactsVersion != 1 {
		t.Fatalf("the movement: %+v", m.State)
	}
	facts, err := f.module.GetOceanDocFacts(ctx, api.GetOceanDocFacts{TenantID: f.tenant, MovementID: "OM-1"})
	body, _ := json.Marshal(facts)
	if err != nil || facts.Status != api.DocFactsFinal || facts.MasterBillNumber != "MAEU123456789" || len(facts.Containers) != 1 ||
		facts.Containers[0].VGMAssertionID != "CV-1:1" || facts.Containers[0].VGMGrams != 2_200_000 || facts.Houses[0].Packs != 6 ||
		strings.Contains(string(body), "Ada") || strings.Contains(string(body), "Lovelace") || facts.SHA256 == "" {
		t.Fatalf("facts: %s %v", body, err)
	}
	if _, err = f.seal("seal-late", "CV-1", 4, "CUS-1", api.SealCustoms, 6); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a seal after the facts were finalized: %v", err)
	}
	f.w.plan("SH-1", "C-1", 2)
	f.stuffing("SH-1", "X-1", 1, "22G1", "CN-SEAL-1")
	if _, err = f.module.RefreshHouseBillFacts(ctx, api.RefreshHouseBillFacts{CommandID: "refresh-2", TenantID: f.tenant, ActorID: "ops-1", MovementID: "OM-1",
		ExpectedRevision: 5, ShipmentID: "SH-1", OccurredAt: f.hours(6)}); err != nil {
		t.Fatal(err)
	}
	if m := f.get(t, "OM-1"); m.State != api.StateOpen || m.DocFactsVersion != 1 {
		t.Fatalf("reopened: %s version %d", m.State, m.DocFactsVersion)
	}
	again, err := f.module.GetOceanDocFacts(ctx, api.GetOceanDocFacts{TenantID: f.tenant, MovementID: "OM-1", Version: 1})
	if err != nil || again.Status != api.DocFactsSuperseded || again.Version != 1 {
		t.Fatalf("the published version: %+v %v", again.Status, err)
	}
	var saw bool
	for _, e := range f.outbox(t) {
		if e.Type == api.OceanDocFactsFinalized && e.Payload.FactsVersion == 1 {
			saw = true
		}
		if strings.Contains(e.Raw, "Ada") {
			t.Fatalf("an event carries the name: %s", e.Raw)
		}
	}
	if !saw {
		t.Fatal("the finalization was not published")
	}
}
