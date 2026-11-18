//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// TestOceanDocFactsOnPostgres publishes a movement's facts once its container has a seal and a VGM. The carrier's
// acceptance and a gate-in are recorded afterwards and leave the movement FINAL. A loading returns it to OPEN,
// because the published facts state no on-board date, and the next finalization states it. A later report of the
// loading leaves that version current. No published version is rewritten.
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
	if err = f.past(t, `WITH v AS (UPDATE cargo_sea.container_visit SET revision=revision+1 WHERE tenant_id=$1 AND visit_id='CV-1' RETURNING visit_id)
        UPDATE cargo_sea.vgm_assertion a SET response='REJECTED', response_source='carrier portal', responded_at=a.submitted_at,
          response_by='x', response_entered_at=now()
        FROM v WHERE a.tenant_id=$1 AND a.visit_id=v.visit_id AND a.version=1`); sqlState(err) != "23514" || !strings.Contains(err.Error(), "rejected while the movement is open") {
		t.Fatalf("a rejection that leaves the movement final: %v", err)
	}
	shanghai := kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: "CNSHG"}
	answered, err := f.module.RecordVgmResponse(ctx, api.RecordVgmResponse{CommandID: "vgm-answer", TenantID: f.tenant, ActorID: "line-1", VisitID: "CV-1",
		ExpectedRevision: 4, Response: api.VgmAccepted, Source: "carrier portal", OccurredAt: f.hours(5)})
	if err != nil || answered.AssertionNo != 1 || answered.NewRevision != 5 || f.get(t, "OM-1").State != api.StateFinal {
		t.Fatalf("the carrier's answer leaves the facts final: %+v %v", answered, err)
	}
	if err = f.past(t, `WITH v AS (UPDATE cargo_sea.container_visit SET revision=revision+1 WHERE tenant_id=$1 AND visit_id='CV-1' RETURNING tenant_id, visit_id),
        a AS (INSERT INTO cargo_sea.vgm_assertion (tenant_id,visit_id,version,mass_grams,method,responsible_party_id,responsible_party_revision,
          determined_at,submitted_at,recorded_by,recorded_at,entered_at)
          SELECT tenant_id,visit_id,2,2200000,'METHOD_1','LE',1,'2020-01-01','2020-01-01','x','2020-01-01',now() FROM v
          RETURNING tenant_id, visit_id, version)
        INSERT INTO cargo_sea.vgm_signatory (tenant_id,visit_id,version,name) SELECT tenant_id,visit_id,version,'Grace Hopper' FROM a`); sqlState(err) != "23514" || !strings.Contains(err.Error(), "while the movement is open") {
		t.Fatalf("a VGM assertion that leaves the movement final: %v", err)
	}
	gated, err := f.module.RecordContainerEvent(ctx, api.RecordContainerEvent{CommandID: "gate-1", TenantID: f.tenant, ActorID: "ops-1", VisitID: "CV-1",
		ExpectedRevision: 5, Kind: api.EventGateIn, Location: shanghai, HappenedAt: f.hours(5), Source: "terminal", Reference: "GIN-1", OccurredAt: f.hours(5)})
	if err != nil || gated.EventNo != 1 || gated.NewRevision != 6 || f.get(t, "OM-1").State != api.StateFinal {
		t.Fatalf("a gate-in leaves the facts final: %+v %v", gated, err)
	}
	if err = f.past(t, `WITH v AS (UPDATE cargo_sea.container_visit SET revision=revision+1 WHERE tenant_id=$1 AND visit_id='CV-1' RETURNING visit_id)
        INSERT INTO cargo_sea.container_seal (tenant_id,visit_id,seal_no,number,party,applied_at,source,recorded_by,recorded_at,entered_at)
        SELECT $1,visit_id,2,'CUS-9','CUSTOMS',now(),'x','x',now(),now() FROM v`); sqlState(err) != "23514" || !strings.Contains(err.Error(), "while the movement is open") {
		t.Fatalf("a seal that leaves the movement final: %v", err)
	}
	// the published facts state no on-board date, and say so by leaving it out
	if content := f.publishedContent(t, "OM-1", 1); strings.Contains(content, "on_board_at") || strings.Contains(content, "0001-01-01") {
		t.Fatalf("facts without a loading: %s", content)
	}
	if err = f.past(t, `WITH v AS (UPDATE cargo_sea.container_visit SET status='LOADED', revision=revision+1 WHERE tenant_id=$1 AND visit_id='CV-1' RETURNING visit_id)
        INSERT INTO cargo_sea.container_event (tenant_id,visit_id,event_no,kind,location_code,occurred_at,source,reference,recorded_by,recorded_at,entered_at)
        SELECT $1,visit_id,2,'LOADED_ON_BOARD','CNSHG',now(),'x','LD-PAST','x',now(),now() FROM v`); sqlState(err) != "23514" || !strings.Contains(err.Error(), "on-board date") {
		t.Fatalf("a loading that leaves the movement final: %v", err)
	}
	// a loading the published facts do not state returns the movement to OPEN
	loaded, err := f.module.RecordContainerEvent(ctx, api.RecordContainerEvent{CommandID: "load-1", TenantID: f.tenant, ActorID: "ops-1", VisitID: "CV-1",
		ExpectedRevision: 6, Kind: api.EventLoaded, Location: shanghai, HappenedAt: f.hours(6), Source: "terminal", Reference: "LD-1", OccurredAt: f.hours(6)})
	first, firstErr := f.module.GetOceanDocFacts(ctx, api.GetOceanDocFacts{TenantID: f.tenant, MovementID: "OM-1"})
	if m := f.get(t, "OM-1"); err != nil || firstErr != nil || loaded.EventNo != 2 || loaded.NewRevision != 7 || f.visit(t, "CV-1").Status != api.VisitLoaded ||
		m.State != api.StateOpen || m.Revision != 6 || first.Version != 1 || first.Status != api.DocFactsSuperseded || !first.OnBoardAt.IsZero() {
		t.Fatalf("a loading supersedes facts that state no on-board date: %+v %s rev %d %+v %v %v", loaded, m.State, m.Revision, first.Status, err, firstErr)
	}
	sealed, err := f.seal("seal-late", "CV-1", 7, "CUS-1", api.SealCustoms, 7)
	if m := f.get(t, "OM-1"); err != nil || sealed.SealNo != 2 || sealed.NewRevision != 8 || m.State != api.StateOpen || m.Revision != 6 {
		t.Fatalf("a seal on an open movement: %+v %v", sealed, err)
	}
	f.w.plan("SH-1", "C-1", 2)
	f.stuffing("SH-1", "X-1", 1, "22G1", "CN-SEAL-1")
	if _, err = f.module.RefreshHouseBillFacts(ctx, api.RefreshHouseBillFacts{CommandID: "refresh-2", TenantID: f.tenant, ActorID: "ops-1", MovementID: "OM-1",
		ExpectedRevision: 6, ShipmentID: "SH-1", OccurredAt: f.hours(8)}); err != nil {
		t.Fatal(err)
	}
	if m := f.get(t, "OM-1"); m.State != api.StateOpen || m.DocFactsVersion != 1 {
		t.Fatalf("reopened: %s version %d", m.State, m.DocFactsVersion)
	}
	again, err := f.module.GetOceanDocFacts(ctx, api.GetOceanDocFacts{TenantID: f.tenant, MovementID: "OM-1", Version: 1})
	if err != nil || again.Status != api.DocFactsSuperseded || again.Version != 1 {
		t.Fatalf("the published version: %+v %v", again.Status, err)
	}
	// the next finalization states the on-board date and the seal recorded since
	second, err := f.module.FinalizeOceanDocFacts(ctx, api.FinalizeOceanDocFacts{CommandID: "fin-2", TenantID: f.tenant, ActorID: "ops-1", MovementID: "OM-1",
		ExpectedRevision: 7, OccurredAt: f.hours(9)})
	current, currentErr := f.module.GetOceanDocFacts(ctx, api.GetOceanDocFacts{TenantID: f.tenant, MovementID: "OM-1"})
	if err != nil || currentErr != nil || second.FactsVersion != 2 || second.NewRevision != 8 || current.Status != api.DocFactsFinal ||
		!current.OnBoardAt.Equal(f.hours(6)) || current.Containers[0].SealNumber != "CUS-1" || !strings.Contains(f.publishedContent(t, "OM-1", 2), `"on_board_at":"`) {
		t.Fatalf("the second version: %+v %+v %v %v", second, current, err, currentErr)
	}
	// the carrier's later report of the same loading leaves the published date, and the movement FINAL
	report, err := f.module.RecordOceanCarrierFact(ctx, api.RecordOceanCarrierFact{CommandID: "load-report", TenantID: f.tenant, ActorID: "line-1", MovementID: "OM-1",
		ExpectedRevision: 8, Kind: api.CarrierLoaded, ContainerNumber: "CSQU3054383", Vessel: "EVER GIVEN", Voyage: "0123W", Source: "line", Reference: "LD-REPORT",
		SHA256: strings.Repeat("cd", 32), HappenedAt: f.hours(7), OccurredAt: f.hours(9)})
	if m := f.get(t, "OM-1"); err != nil || report.FactNo != 1 || report.NewRevision != 8 || m.State != api.StateFinal || m.DocFactsVersion != 2 {
		t.Fatalf("a later loading leaves the facts final: %+v %s %v", report, m.State, err)
	}
	var finalized, superseded bool
	for _, e := range f.outbox(t) {
		if e.Type == api.OceanDocFactsFinalized && e.Payload.FactsVersion == 1 {
			finalized = true
		}
		if e.Type == api.OceanDocFactsSuperseded && e.Payload.FactsVersion == 1 {
			superseded = true
		}
		if strings.Contains(e.Raw, "Ada") {
			t.Fatalf("an event carries the name: %s", e.Raw)
		}
	}
	if !finalized || !superseded {
		t.Fatal("the finalization and its supersession were not both published")
	}
}

// publishedContent reads a published version's JSON as the database holds it.
func (f *fixture) publishedContent(t *testing.T, movement string, version int) string {
	t.Helper()
	var content string
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT content FROM cargo_sea.ocean_doc_facts_version WHERE tenant_id=$1 AND movement_id=$2 AND version_no=$3`,
			f.tenant, movement, version).Scan(&content)
	}); err != nil {
		t.Fatalf("version %d of %s: %v", version, movement, err)
	}
	return content
}

// publishOwnFacts finalizes one own-container movement whose visit is VGM_SUBMITTED at revision 4.
func (f *fixture) publishOwnFacts(t *testing.T) {
	t.Helper()
	f.stuffOwnContainer(t)
	got, err := f.module.FinalizeOceanDocFacts(context.Background(), api.FinalizeOceanDocFacts{CommandID: "fin-1", TenantID: f.tenant, ActorID: "ops-1",
		MovementID: "OM-1", ExpectedRevision: 4, OccurredAt: f.hours(5)})
	if err != nil || got.FactsVersion != 1 || got.NewRevision != 5 || f.get(t, "OM-1").State != api.StateFinal {
		t.Fatalf("finalized: %+v %v", got, err)
	}
}

// stuffOwnContainer leaves the own-container movement OM-1 OPEN at revision 4, ready to be finalized, with its visit
// CV-1 VGM_SUBMITTED at revision 4.
func (f *fixture) stuffOwnContainer(t *testing.T) {
	t.Helper()
	ctx := context.Background()
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
	if _, err := f.module.AssertVgm(ctx, api.AssertVgm{CommandID: "vgm-1", TenantID: f.tenant, ActorID: "ops-1", VisitID: "CV-1", ExpectedRevision: 3,
		Mass: kernel.Quantity{Unit: "KGM", Amount: 2200}, Method: api.VgmMethod1, ResponsiblePartyID: "LE", ResponsiblePartyRevision: 1,
		DeterminedAt: f.hours(4), Signatory: "Ada Lovelace", OccurredAt: f.hours(4)}); err != nil {
		t.Fatal(err)
	}
}

func TestANewVGMReopensTheFacts(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.publishOwnFacts(t)
	got, err := f.module.AssertVgm(ctx, api.AssertVgm{CommandID: "vgm-2", TenantID: f.tenant, ActorID: "ops-1", VisitID: "CV-1", ExpectedRevision: 4,
		Mass: kernel.Quantity{Unit: "KGM", Amount: 2300}, Method: api.VgmMethod2, ResponsiblePartyID: "LE", ResponsiblePartyRevision: 1,
		DeterminedAt: f.hours(6), Signatory: "Grace Hopper", OccurredAt: f.hours(6)})
	v := f.visit(t, "CV-1")
	facts, factsErr := f.module.GetOceanDocFacts(ctx, api.GetOceanDocFacts{TenantID: f.tenant, MovementID: "OM-1", Version: 1})
	if err != nil || factsErr != nil || got.AssertionNo != 2 || got.NewRevision != 5 || f.get(t, "OM-1").State != api.StateOpen ||
		facts.Status != api.DocFactsSuperseded || v.Status != api.VisitVGM || len(v.Assertions) != 2 || v.Assertions[0].MassGrams != 2_300_000 {
		t.Fatalf("a new assertion reopens the movement: %+v %+v %v %v", got, facts.Status, err, factsErr)
	}
	for _, e := range f.outbox(t) {
		if strings.Contains(e.Raw, "Grace") || strings.Contains(e.Raw, "Hopper") || strings.Contains(e.Raw, "Ada") {
			t.Fatalf("an event carries a name: %s", e.Raw)
		}
	}
}

func TestARejectedVGMReopensTheFacts(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.publishOwnFacts(t)
	got, err := f.module.RecordVgmResponse(ctx, api.RecordVgmResponse{CommandID: "vgm-reject", TenantID: f.tenant, ActorID: "line-1", VisitID: "CV-1",
		ExpectedRevision: 4, Response: api.VgmRejected, Source: "carrier portal", OccurredAt: f.hours(6)})
	v := f.visit(t, "CV-1")
	facts, factsErr := f.module.GetOceanDocFacts(ctx, api.GetOceanDocFacts{TenantID: f.tenant, MovementID: "OM-1", Version: 1})
	if err != nil || factsErr != nil || got.AssertionNo != 1 || got.NewRevision != 5 || f.get(t, "OM-1").State != api.StateOpen ||
		facts.Status != api.DocFactsSuperseded || v.Status != api.VisitVGM || len(v.Assertions) != 1 || v.Assertions[0].Response != api.VgmRejected {
		t.Fatalf("a rejection reopens the movement: %+v %+v %v %v", got, facts.Status, err, factsErr)
	}
	var superseded bool
	for _, e := range f.outbox(t) {
		if e.Type == api.OceanDocFactsSuperseded && e.Payload.FactsVersion == 1 {
			superseded = true
		}
		if strings.Contains(e.Raw, "Ada") {
			t.Fatalf("an event carries the name: %s", e.Raw)
		}
	}
	if !superseded {
		t.Fatal("the rejection did not supersede the published facts")
	}
}
