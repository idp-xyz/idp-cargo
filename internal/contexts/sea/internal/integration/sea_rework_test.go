//go:build integration

package integration

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	seamodule "go.idp.xyz/idp-cargo/internal/contexts/sea"
	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// V2.87 on PostgreSQL, through SEA's real commands, with FUL's plans as a stand-in sets them. The day: houses SH-1 and
// SH-2 in unit 1 of C-1 (PHY's stuffing X-1, sealed S1 at hour 1), a container CV-1 taken as stuffed at hour 3 with a
// VGM at hour 4; PHY seals X-1 again with S2 at hour 5, FUL relays it and SEA refreshes the houses.

// reworkWorld is the stand-in for FUL that also says, per shipment, whether FUL's evidence is behind PHY's.
type reworkWorld struct {
	*world
	mu     sync.Mutex
	behind map[string]bool
}

func (r *reworkWorld) GetOceanEvidenceCurrency(_ context.Context, q fulapi.GetShipmentPlanSnapshot) (fulapi.OceanEvidenceCurrency, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return fulapi.OceanEvidenceCurrency{TenantID: q.TenantID, ShipmentID: q.ShipmentID, NewVersionDue: r.behind[q.ShipmentID]}, nil
}

func (r *reworkWorld) setBehind(shipment string, behind bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.behind[shipment] = behind
}

type reworkFixture struct {
	*fixture
	rw *reworkWorld
	t  *testing.T
}

func newReworkFixture(t *testing.T) *reworkFixture {
	t.Helper()
	f := newFixture(t)
	rw := &reworkWorld{world: f.w, behind: map[string]bool{}}
	m, err := seamodule.New(f.db, f.w, rw, allow{})
	if err != nil {
		t.Fatal(err)
	}
	f.module = m
	return &reworkFixture{fixture: f, rw: rw, t: t}
}

// phySealed makes the shipment's plan carry PHY's stuffing X-1, sealed with seal at the hour, as FUL relays it with
// evidence version evidence.
func (f *reworkFixture) phySealed(shipment, seal string, hour, evidence int) {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	p := f.w.plans[shipment]
	p.EvidenceVersion, p.MeasuredBasis, p.MeasuredPacks, p.MeasuredGrossWeight = evidence, fulapi.MeasuredTendered, 6, kg(318)
	p.Stuffings = []fulapi.ContainerStuffing{{ExecutionID: "X-1", LoadUnitNo: 1, TypeCode: "22G1", Packs: 6, GrossWeight: kg(318), LineNos: []int{1}, Seal: seal,
		SealedAt: f.hours(hour)}}
	f.w.plans[shipment] = p
}

// phyInRework makes the plan carry no stuffing: PHY relays only closed executions, and X-1 is open.
func (f *reworkFixture) phyInRework(shipment string, evidence int) {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	p := f.w.plans[shipment]
	p.EvidenceVersion, p.MeasuredBasis, p.MeasuredPacks, p.MeasuredGrossWeight = evidence, fulapi.MeasuredReceived, 6, kg(300)
	p.Stuffings = nil
	f.w.plans[shipment] = p
}

func (f *reworkFixture) refresh(command, shipment string, hours int) (api.Receipt, error) {
	return f.module.RefreshHouseBillFacts(context.Background(), api.RefreshHouseBillFacts{CommandID: command, TenantID: f.tenant, ActorID: "ops-1", MovementID: "OM-1",
		ExpectedRevision: f.get(f.t, "OM-1").Revision, ShipmentID: shipment, OccurredAt: f.hours(hours)})
}

func (f *reworkFixture) vgm(command string, revision int64, hours int) (api.Receipt, error) {
	return f.module.AssertVgm(context.Background(), api.AssertVgm{CommandID: command, TenantID: f.tenant, ActorID: "ops-1", VisitID: "CV-1", ExpectedRevision: revision,
		Mass: kernel.Quantity{Unit: "KGM", Amount: 3700}, Method: api.VgmMethod2, ResponsiblePartyID: "LE", ResponsiblePartyRevision: 1, DeterminedAt: f.hours(hours),
		Signatory: "Ada Lovelace", OccurredAt: f.hours(hours)})
}

func (f *reworkFixture) finalize(command string, hours int) (api.Receipt, error) {
	return f.module.FinalizeOceanDocFacts(context.Background(), api.FinalizeOceanDocFacts{CommandID: command, TenantID: f.tenant, ActorID: "ops-2", MovementID: "OM-1",
		ExpectedRevision: f.get(f.t, "OM-1").Revision, OccurredAt: f.hours(hours)})
}

// day builds the day up to the VGM: the movement OM-1 with SH-1 and SH-2, a master bill, CV-1 taken as stuffed with S1
// and a VGM v1.
func (f *reworkFixture) day(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	f.w.booking("BK-1", "C-1", "EVER GIVEN", "9811000", "0123W", 1, f.hours(72))
	if _, err := f.open("OM-1", "C-1", "BK-1"); err != nil {
		t.Fatal(err)
	}
	f.series(t)
	for _, s := range []string{"SH-1", "SH-2"} {
		f.w.plan(s, "C-1", 1)
		f.phySealed(s, "S1", 1, 1)
	}
	for i, s := range []string{"SH-1", "SH-2"} {
		if _, err := f.associateAt("assoc-"+s, "OM-1", s, "", f.get(t, "OM-1").Revision, 2); err != nil {
			t.Fatalf("associate %d: %v", i, err)
		}
	}
	if _, err := f.masterBill("bill-1", "OM-1", "MAEU123456789", api.MasterBillCarrierMBL, f.get(t, "OM-1").Revision, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.AssignContainer(ctx, api.AssignContainer{CommandID: "assign-CV-1", TenantID: f.tenant, ActorID: "ops-1", VisitID: "CV-1", MovementID: "OM-1",
		LoadUnitNo: 1, ContainerNumber: "CSQU3054383", SizeType: "22G1", TareKg: 2200, MaxGrossKg: 30480, PlateSource: api.PlateCSC, OccurredAt: f.hours(2)}); err != nil {
		t.Fatal(err)
	}
	r, err := f.seal("seal-S1", "CV-1", 1, "S1", api.SealShipper, 2)
	if err != nil {
		t.Fatal(err)
	}
	if r, err = f.stuffingDone("done-1", "CV-1", r.NewRevision, "", 3); err != nil {
		t.Fatal(err)
	}
	if _, err = f.vgm("vgm-1", r.NewRevision, 4); err != nil {
		t.Fatal(err)
	}
}

// reseal is PHY sealing X-1 again with S2 at hour 5, as FUL relays it for both houses.
func (f *reworkFixture) reseal() {
	for _, s := range []string{"SH-1", "SH-2"} {
		f.phySealed(s, "S2", 5, 2)
	}
}

func sealNumbers(v api.ContainerVisit) []string {
	var out []string
	for i := len(v.Seals) - 1; i >= 0; i-- { // the visit lists its seals latest first
		out = append(out, v.Seals[i].Number+"/"+v.Seals[i].Party)
	}
	return out
}

func TestSEAFollowsPHYsNewSealOnPostgres(t *testing.T) {
	f := newReworkFixture(t)
	f.day(t)
	before := f.visit(t, "CV-1")
	f.reseal()
	// the first house: the seal is followed, on the visit, in the command's transaction
	r, err := f.refresh("refresh-1", "SH-1", 6)
	if err != nil {
		t.Fatal(err)
	}
	v := f.visit(t, "CV-1")
	if got := strings.Join(sealNumbers(v), " "); got != "S1/SHIPPER S2/SHIPPER" || v.Revision != before.Revision+1 || v.Status != api.VisitVGM {
		t.Fatalf("seals %s, revision %d (was %d), %s", got, v.Revision, before.Revision, v.Status)
	}
	s2 := v.Seals[0]
	if s2.SealNo != 2 || s2.Party != api.SealShipper || !s2.AppliedAt.Equal(f.hours(5)) || !s2.RecordedAt.Equal(f.hours(6)) || s2.RecordedBy != "ops-1" ||
		!strings.Contains(s2.Source, "PHY's stuffing X-1") || r.EntryNo != 3 {
		t.Fatalf("the followed seal: %+v receipt %+v", s2, r)
	}
	// the second house carries the same seal: found held, nothing recorded, the visit is not written
	if _, err = f.refresh("refresh-2", "SH-2", 6); err != nil {
		t.Fatal(err)
	}
	if again := f.visit(t, "CV-1"); len(again.Seals) != 2 || again.Revision != v.Revision {
		t.Fatalf("the second house: %+v", sealNumbers(again))
	}
	// the seal is numbered and evented as any seal: SealRecorded names seal 2 of CV-1, after the refresh's own event
	var kinds []string
	for _, e := range f.outbox(t) {
		if e.Subject == "CV-1" || e.Type == api.HouseBillFactsRefreshed {
			kinds = append(kinds, e.Type+" "+e.Subject)
			if e.Type == api.SealRecorded && e.Payload.SealNo == 2 && (e.Payload.Reason != "" || strings.Contains(e.Raw, "S2")) {
				t.Fatalf("the seal event: %s", e.Raw)
			}
		}
	}
	if got := strings.Join(kinds[len(kinds)-3:], ", "); got != "HouseBillFactsRefreshed.v1 OM-1, SealRecorded.v1 CV-1, HouseBillFactsRefreshed.v1 OM-1" {
		t.Fatalf("events: %s", got)
	}
	// finalize: the VGM predates PHY's seal (F4); a new one lets it through, with S2 published
	if _, err = f.finalize("fin-1", 7); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "no VGM submitted after PHY sealed it") {
		t.Fatalf("finalize before a new VGM: %v", err)
	}
	if _, err = f.vgm("vgm-2", v.Revision, 7); err != nil {
		t.Fatal(err)
	}
	if _, err = f.finalize("fin-2", 8); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	facts, err := f.module.GetOceanDocFacts(context.Background(), api.GetOceanDocFacts{TenantID: f.tenant, MovementID: "OM-1"})
	if err != nil || facts.Containers[0].SealNumber != "S2" || facts.Containers[0].SealParty != api.SealShipper || facts.Containers[0].VGMAssertionID != "CV-1:2" ||
		facts.Containers[0].Packs != 12 {
		t.Fatalf("published: %+v %v", facts.Containers, err)
	}
}

// Associating a house follows the seal as refreshing one does.
func TestAssociatingAHouseFollowsPHYsNewSealOnPostgres(t *testing.T) {
	f := newReworkFixture(t)
	f.day(t)
	f.w.plan("SH-3", "C-1", 1)
	f.phySealed("SH-3", "S2", 5, 1)
	if _, err := f.associateAt("assoc-SH-3", "OM-1", "SH-3", "", f.get(t, "OM-1").Revision, 6); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sealNumbers(f.visit(t, "CV-1")), " "); got != "S1/SHIPPER S2/SHIPPER" {
		t.Fatalf("seals: %s", got)
	}
}

func (f *reworkFixture) pending(t *testing.T) []api.PendingItem {
	t.Helper()
	items, err := f.module.GetPendingItems(context.Background(), api.GetPendingItems{TenantID: f.tenant, MovementID: "OM-1"})
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func pendingKinds(items []api.PendingItem) string {
	var out []string
	for _, it := range items {
		b := ""
		if !it.Blocking {
			b = " (warning)"
		}
		out = append(out, it.Kind+b)
	}
	return strings.Join(out, ", ")
}

// A seal SEA's rules cannot take is not an error: the refresh is written, no seal is recorded and a pending item says
// what to do; Finalize refuses with it; a later refresh records the seal by itself once its business time allows.
func TestASealTheRulesCannotTakeIsWrittenAndPendingOnPostgres(t *testing.T) {
	f := newReworkFixture(t)
	f.day(t)
	f.reseal()
	before := f.get(t, "OM-1")
	// the business time is before PHY sealed it again
	r, err := f.refresh("refresh-early", "SH-1", 4)
	if err != nil || r.EntryNo != 3 {
		t.Fatalf("a refresh before PHY sealed it is written: %+v %v", r, err)
	}
	if after := f.get(t, "OM-1"); after.Revision != before.Revision+1 || len(f.visit(t, "CV-1").Seals) != 1 {
		t.Fatalf("revision %d -> %d, seals %v", before.Revision, after.Revision, sealNumbers(f.visit(t, "CV-1")))
	}
	if _, err = f.refresh("refresh-early-2", "SH-2", 4); err != nil {
		t.Fatal(err)
	}
	items := f.pending(t)
	var seal api.PendingItem
	for _, it := range items {
		if it.Kind == api.PendingSealNotOnContainer {
			seal = it
		}
	}
	if !seal.Blocking || seal.Scope != api.PendingScopeContainer || seal.SubjectID != "CSQU3054383" || !strings.Contains(seal.Message, "at a business time not before PHY sealed it (") ||
		!strings.Contains(seal.Message, "and not before the container's last change (") {
		t.Fatalf("pending: %s: %+v", pendingKinds(items), seal)
	}
	if _, err = f.finalize("fin-pending", 8); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "has none of the seals PHY sealed the stuffing X-1 with") {
		t.Fatalf("finalize: %v", err)
	}
	// a later refresh at a time that allows it records the seal, though the house's facts are as SEA froze them
	if _, err = f.refresh("refresh-later", "SH-1", 6); err != nil {
		t.Fatalf("the later refresh: %v", err)
	}
	if got := strings.Join(sealNumbers(f.visit(t, "CV-1")), " "); got != "S1/SHIPPER S2/SHIPPER" {
		t.Fatalf("seals: %s", got)
	}
	if got := pendingKinds(f.pending(t)); got != api.PendingVGMBeforeReseal {
		t.Fatalf("pending after the seal: %s", got)
	}
	// and once nothing is left to follow it is nothing to refresh, as before
	revision, visits := f.get(t, "OM-1").Revision, f.visit(t, "CV-1").Revision
	if _, err = f.refresh("refresh-again", "SH-2", 6); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "FUL reports the plan version, the measures and the containers SEA froze") {
		t.Fatalf("nothing to refresh: %v", err)
	}
	if f.get(t, "OM-1").Revision != revision || f.visit(t, "CV-1").Revision != visits {
		t.Fatal("a refresh that follows nothing and has no new facts wrote")
	}
}

// A visit that is not yet stuffed is not followed: RecordStuffingDone compares as before.
func TestAnAssignedVisitIsNotFollowedOnPostgres(t *testing.T) {
	f := newReworkFixture(t)
	f.w.booking("BK-1", "C-1", "EVER GIVEN", "9811000", "0123W", 1, f.hours(72))
	if _, err := f.open("OM-1", "C-1", "BK-1"); err != nil {
		t.Fatal(err)
	}
	f.series(t)
	f.w.plan("SH-1", "C-1", 1)
	f.phySealed("SH-1", "S1", 1, 1)
	if _, err := f.associateAt("assoc-1", "OM-1", "SH-1", "", 1, 2); err != nil {
		t.Fatal(err)
	}
	r, err := f.assign("CV-1", "OM-1", 1, "CSQU3054383", 2)
	if err != nil {
		t.Fatal(err)
	}
	if r, err = f.seal("seal-S1", "CV-1", r.NewRevision, "S1", api.SealShipper, 2); err != nil {
		t.Fatal(err)
	}
	f.phySealed("SH-1", "S2", 5, 2)
	if _, err = f.refresh("refresh-1", "SH-1", 6); err != nil {
		t.Fatal(err)
	}
	if v := f.visit(t, "CV-1"); len(v.Seals) != 1 || v.Revision != r.NewRevision {
		t.Fatalf("an ASSIGNED visit was written: %v", sealNumbers(v))
	}
	// as today: PHY's S2 is none of the container's seals, and the container is taken as stuffed only with a reason
	if _, err = f.stuffingDone("done-1", "CV-1", r.NewRevision, "", 7); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), `seal "S2"`) {
		t.Fatalf("stuffing done: %v", err)
	}
	if r, err = f.seal("seal-S2", "CV-1", r.NewRevision, "S2", api.SealShipper, 7); err != nil {
		t.Fatal(err)
	}
	if _, err = f.stuffingDone("done-2", "CV-1", r.NewRevision, "", 8); err != nil {
		t.Fatalf("with S2 recorded: %v", err)
	}
}

// F0 for Finalize, then STALE_HOUSE, then F1 to F4, through the real commands.
func TestFinalizeFollowsPHYsRework(t *testing.T) {
	f := newReworkFixture(t)
	f.day(t)
	// PHY opened the rework: FUL has read nothing yet
	f.rw.setBehind("SH-1", true)
	f.rw.setBehind("SH-2", true)
	if _, err := f.finalize("fin-f0", 5); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "FUL has not recorded PHY's latest facts of") ||
		!strings.Contains(err.Error(), "record them in FUL, and refresh the house if SEA then reports it STALE_HOUSE") {
		t.Fatalf("F0: %v", err)
	}
	// FUL records the evidence of the rework: the stuffing is not relayed while PHY has it open
	f.rw.setBehind("SH-1", false)
	f.rw.setBehind("SH-2", false)
	f.phyInRework("SH-1", 2)
	f.phyInRework("SH-2", 2)
	if _, err := f.finalize("fin-stale", 5); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "STALE_HOUSE") {
		t.Fatalf("STALE_HOUSE: %v", err)
	}
	// PHY seals again: FUL records, H1 alone is refreshed: the houses disagree (F1)
	f.reseal()
	f.phySealed("SH-1", "S2", 5, 3)
	f.phySealed("SH-2", "S2", 5, 3)
	if _, err := f.finalize("fin-stale-2", 5); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "STALE_HOUSE") {
		t.Fatalf("STALE_HOUSE after the reseal: %v", err)
	}
	// SH-1 is refreshed from the plan that still has no stuffing, then from the resealed one
	f.phyInRework("SH-1", 2)
	if _, err := f.refresh("refresh-rework", "SH-1", 5); err != nil {
		t.Fatal(err)
	}
	f.phySealed("SH-1", "S2", 5, 3)
	if _, err := f.refresh("refresh-reseal", "SH-1", 6); err != nil {
		t.Fatal(err)
	}
	f.phyInRework("SH-2", 2)
	if _, err := f.refresh("refresh-rework-2", "SH-2", 6); err != nil {
		t.Fatal(err)
	}
	// H2 is still refreshed from a rework, with no stuffing: the container's PHY seal is H1's, and no VGM is after it
	if _, err := f.finalize("fin-f1", 7); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "no VGM submitted after PHY sealed it") {
		t.Fatalf("a house refreshed in the rework only: %v", err)
	}
	f.phySealed("SH-2", "S2", 5, 3)
	if _, err := f.refresh("refresh-reseal-2", "SH-2", 7); err != nil {
		t.Fatal(err)
	}
	if _, err := f.finalize("fin-f4", 8); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "no VGM submitted after PHY sealed it") {
		t.Fatalf("F4: %v", err)
	}
	v := f.visit(t, "CV-1")
	if _, err := f.vgm("vgm-2", v.Revision, 8); err != nil {
		t.Fatal(err)
	}
	// a later SHIPPER seal that is not PHY's is refused (F2), a customs seal after it reopens nothing here and passes
	r, err := f.seal("seal-shp", "CV-1", v.Revision+1, "SHP-9", api.SealShipper, 9)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.finalize("fin-f2", 10); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "only a customs or carrier seal may follow it") {
		t.Fatalf("F2: %v", err)
	}
	if r, err = f.seal("seal-s2-again", "CV-1", r.NewRevision, "S2", api.SealShipper, 10); err != nil {
		t.Fatal(err)
	}
	if r, err = f.seal("seal-cus", "CV-1", r.NewRevision, "CUS-7", api.SealCustoms, 11); err != nil {
		t.Fatal(err)
	}
	if _, err = f.finalize("fin-ok", 12); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	facts, err := f.module.GetOceanDocFacts(context.Background(), api.GetOceanDocFacts{TenantID: f.tenant, MovementID: "OM-1"})
	if err != nil || facts.Containers[0].SealNumber != "CUS-7" || facts.Containers[0].SealParty != api.SealCustoms || facts.Containers[0].VGMAssertionID != "CV-1:2" {
		t.Fatalf("published: %+v %v", facts.Containers, err)
	}
}

// F3 on PostgreSQL: a container gated in before PHY sealed it again is refused with F3's message, not the VGM's, and a
// new VGM cannot be asserted on it.
func TestFinalizeAContainerGatedInBeforeTheResealOnPostgres(t *testing.T) {
	f := newReworkFixture(t)
	f.day(t)
	shanghai := kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: "CNSHG"}
	v := f.visit(t, "CV-1")
	r, err := f.module.RecordContainerEvent(context.Background(), api.RecordContainerEvent{CommandID: "gate-1", TenantID: f.tenant, ActorID: "ops-1", VisitID: "CV-1",
		ExpectedRevision: v.Revision, Kind: api.EventGateIn, Location: shanghai, HappenedAt: f.hours(4), Source: "terminal", Reference: "GIN-1", OccurredAt: f.hours(4)})
	if err != nil {
		t.Fatal(err)
	}
	f.reseal()
	if _, err = f.refresh("refresh-1", "SH-1", 6); err != nil {
		t.Fatal(err)
	}
	if _, err = f.refresh("refresh-2", "SH-2", 6); err != nil {
		t.Fatal(err)
	}
	v = f.visit(t, "CV-1")
	if got := strings.Join(sealNumbers(v), " "); got != "S1/SHIPPER S2/SHIPPER" || v.Status != api.VisitGatedIn {
		t.Fatalf("S2 is followed on a gated-in container: %s %s", got, v.Status)
	}
	_, err = f.finalize("fin-1", 7)
	if !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "gated in or was loaded before PHY sealed it again") ||
		!strings.Contains(err.Error(), "its return from the terminal is not recorded yet") || strings.Contains(err.Error(), "assert a new one") {
		t.Fatalf("F3: %v", err)
	}
	if _, err = f.vgm("vgm-2", r.NewRevision+1, 7); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a VGM on a gated-in container is refused as today: %v", err)
	}
}

// RecordStuffingDone refuses a stuffing while FUL is behind PHY or SEA has not read FUL (F0, STALE_HOUSE); with the
// houses read after the reseal it is accepted as before.
func TestStuffingDoneChecksFULAndSEAHaveReadPHYOnPostgres(t *testing.T) {
	f := newReworkFixture(t)
	f.w.booking("BK-1", "C-1", "EVER GIVEN", "9811000", "0123W", 1, f.hours(72))
	if _, err := f.open("OM-1", "C-1", "BK-1"); err != nil {
		t.Fatal(err)
	}
	f.series(t)
	for _, s := range []string{"SH-1", "SH-2"} {
		f.w.plan(s, "C-1", 1)
		f.phySealed(s, "S1", 1, 1)
		if _, err := f.associateAt("assoc-"+s, "OM-1", s, "", f.get(t, "OM-1").Revision, 2); err != nil {
			t.Fatal(err)
		}
	}
	r, err := f.assign("CV-1", "OM-1", 1, "CSQU3054383", 2)
	if err != nil {
		t.Fatal(err)
	}
	if r, err = f.seal("seal-S1", "CV-1", r.NewRevision, "S1", api.SealShipper, 2); err != nil {
		t.Fatal(err)
	}
	// the rework is open in PHY and FUL has not read it: the stuffing SEA holds is PHY's broken one
	f.rw.setBehind("SH-2", true)
	if _, err = f.stuffingDone("done-0", "CV-1", r.NewRevision, "", 3); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "FUL has not recorded PHY's latest facts") ||
		!errors.Is(err, api.ErrRejected) {
		t.Fatalf("F0: %v", err)
	}
	// FUL has read it, SEA has not
	f.rw.setBehind("SH-2", false)
	f.phyInRework("SH-1", 2)
	f.phyInRework("SH-2", 2)
	if _, err = f.stuffingDone("done-1", "CV-1", r.NewRevision, "", 3); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "STALE_HOUSE") {
		t.Fatalf("STALE_HOUSE: %v", err)
	}
	// SEA has refreshed in the rework: the houses carry no stuffing
	for i, s := range []string{"SH-1", "SH-2"} {
		if _, err = f.refresh("refresh-rework-"+s, s, 3+i); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = f.stuffingDone("done-2", "CV-1", r.NewRevision, "", 6); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "refresh the houses when FUL has newer evidence") {
		t.Fatalf("no stuffing: %v", err)
	}
	// resealed, FUL and SEA have read it; S2 is recorded as today
	f.reseal()
	for i, s := range []string{"SH-1", "SH-2"} {
		if _, err = f.refresh("refresh-reseal-"+s, s, 6+i); err != nil {
			t.Fatal(err)
		}
	}
	if r, err = f.seal("seal-S2", "CV-1", r.NewRevision, "S2", api.SealShipper, 8); err != nil {
		t.Fatal(err)
	}
	if _, err = f.stuffingDone("done-3", "CV-1", r.NewRevision, "", 9); err != nil {
		t.Fatalf("stuffing done: %v", err)
	}
}

// ---- the lock order, deterministically ----

// blockedByHolder counts the sessions blocked by the test's own lock holder, directly or through another waiting
// session (a command queued behind one that holds the visit): the sessions another test of the database blocks are not
// counted.
const blockedByHolder = `WITH RECURSIVE chain(pid) AS (
    SELECT a.pid FROM pg_stat_activity a WHERE $1 = ANY(pg_blocking_pids(a.pid))
  UNION
    SELECT a.pid FROM pg_stat_activity a JOIN chain c ON c.pid = ANY(pg_blocking_pids(a.pid)))
  SELECT count(*) FROM chain`

// lockedMovement holds the movement's row lock on a second connection, as a command of the movement does while it
// writes; release commits it. waiting counts the sessions the holder blocks, directly or through another waiter: a
// session blocked on a pooled connection reports the last statement it completed, so it is told by its blocker, not by
// its query.
type lockedMovement struct {
	t       *testing.T
	f       *reworkFixture
	tx      pgx.Tx
	watch   *pgx.Conn
	waiting func() int
}

func (f *reworkFixture) holdMovement(t *testing.T) *lockedMovement {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, os.Getenv("IDP_CARGO_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, f.tenant); err != nil {
		t.Fatal(err)
	}
	var one int
	if err = tx.QueryRow(ctx, `SELECT 1 FROM cargo_sea.ocean_movement WHERE tenant_id=$1 AND movement_id='OM-1' FOR NO KEY UPDATE`, f.tenant).Scan(&one); err != nil {
		t.Fatal(err)
	}
	watch, err := pgx.Connect(ctx, os.Getenv("IDP_CARGO_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { watch.Close(context.Background()) })
	l := &lockedMovement{t: t, f: f, tx: tx, watch: watch}
	l.waiting = func() int {
		var n int
		if err := watch.QueryRow(ctx, blockedByHolder, tx.Conn().PgConn().PID()).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	return l
}

// start runs a command in the background and returns once it is waiting on a lock, one more session than before.
func (l *lockedMovement) start(name string, run func() error, results map[string]chan error) {
	l.t.Helper()
	before := l.waiting()
	done := make(chan error, 1)
	results[name] = done
	go func() { done <- run() }()
	for i := 0; i < 1200; i++ { // up to 30 seconds
		select {
		case err := <-done:
			l.t.Fatalf("%s did not wait for the lock: %v", name, err)
		case <-time.After(25 * time.Millisecond):
		}
		if l.waiting() > before {
			return
		}
	}
	l.t.Fatalf("%s is not waiting on a lock", name)
}

func (l *lockedMovement) release() {
	l.t.Helper()
	if err := l.tx.Commit(context.Background()); err != nil {
		l.t.Fatal(err)
	}
}

func result(t *testing.T, name string, ch chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not finish: a deadlock or a lock never released", name)
		return nil
	}
}

// TestARefreshThatFollowsASealTakesTheVisitBeforeTheMovement: a refresh that follows a seal races RecordContainerSeal
// and AssertVgm on the same visit while a command of the movement holds the movement's lock. The visit's lock is taken
// first by each (ChangeVisit takes the command id, the visit, then the movement; the refresh takes the command id, the
// visits, then the movement), so that however they queue none waits for another that waits for it: no deadlock, and
// each outcome is that of the order the commands took the visit in. The commands name the visit's revision as it was
// before them: the one that takes the visit first is accepted and the later ones, finding the revision moved, are
// conflicts, which then pass with the new revision.
func TestARefreshThatFollowsASealTakesTheVisitBeforeTheMovement(t *testing.T) {
	for _, order := range [][]string{{"refresh", "seal", "vgm"}, {"seal", "refresh", "vgm"}, {"vgm", "refresh", "seal"}} {
		t.Run(strings.Join(order, "-"), func(t *testing.T) {
			ctx := context.Background()
			f := newReworkFixture(t)
			f.day(t)
			f.reseal()
			visit := f.visit(t, "CV-1")
			movement := f.get(t, "OM-1").Revision
			hold := f.holdMovement(t)
			commands := map[string]func() error{
				"refresh": func() error {
					_, err := f.module.RefreshHouseBillFacts(ctx, api.RefreshHouseBillFacts{CommandID: "race-refresh", TenantID: f.tenant, ActorID: "ops-1", MovementID: "OM-1",
						ExpectedRevision: movement, ShipmentID: "SH-1", OccurredAt: f.hours(8)})
					return err
				},
				"seal": func() error {
					_, err := f.seal("race-seal", "CV-1", visit.Revision, "CUS-5", api.SealCustoms, 7)
					return err
				},
				"vgm": func() error {
					_, err := f.vgm("race-vgm", visit.Revision, 7)
					return err
				},
			}
			results := map[string]chan error{}
			for _, name := range order {
				hold.start(name, commands[name], results)
			}
			hold.release()
			outcome := map[string]error{}
			for _, name := range order {
				outcome[name] = result(t, name, results[name])
			}
			// the first command of the order is accepted; the visit it changed moves the revision on for the others
			// that name the old one. The refresh names the movement's revision, which is the movement's own.
			first := order[0]
			if outcome[first] != nil {
				t.Fatalf("%s went first: %v", first, outcome[first])
			}
			for _, name := range order[1:] {
				if name == "refresh" {
					continue // it names the movement, which only a later command of the movement would move
				}
				if err := outcome[name]; !errors.Is(err, api.ErrConflict) {
					t.Fatalf("%s after %s with the old revision: %v", name, first, err)
				}
			}
			if outcome["refresh"] != nil {
				t.Fatalf("the refresh: %v", outcome["refresh"])
			}
			// retried with the revision now, the others pass; S2 is on the visit once, whatever the order
			v := f.visit(t, "CV-1")
			if first != "seal" && outcome["seal"] != nil {
				if _, err := f.seal("race-seal-2", "CV-1", v.Revision, "CUS-5", api.SealCustoms, 9); err != nil {
					t.Fatal(err)
				}
				v = f.visit(t, "CV-1")
			}
			if first != "vgm" && outcome["vgm"] != nil {
				if _, err := f.vgm("race-vgm-2", v.Revision, 10); err != nil {
					t.Fatal(err)
				}
				v = f.visit(t, "CV-1")
			}
			s2 := 0
			for _, s := range v.Seals {
				if s.Number == "S2" {
					s2++
				}
			}
			if s2 != 1 {
				t.Fatalf("S2 is on the visit %d times: %v", s2, sealNumbers(v))
			}
			if got := f.visit(t, "CV-1").Seals; got[len(got)-1].Number != "S1" || got[len(got)-1].SealNo != 1 {
				t.Fatalf("the history: %v", got)
			}
		})
	}
}

// TestTheRefreshWaitsForTheVisitNotForTheMovement shows the order itself: while a command holds only the visit's lock
// (as ChangeVisit does before it asks for the movement), a refresh that follows a seal waits, and the movement's lock
// is still free for another session (a refresh that took the movement first would hold it).
func TestTheRefreshWaitsForTheVisitNotForTheMovement(t *testing.T) {
	ctx := context.Background()
	f := newReworkFixture(t)
	f.day(t)
	f.reseal()
	c, err := pgx.Connect(ctx, os.Getenv("IDP_CARGO_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var one int
	if _, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, f.tenant); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(ctx, `SELECT 1 FROM cargo_sea.container_visit WHERE tenant_id=$1 AND visit_id='CV-1' FOR NO KEY UPDATE`, f.tenant).Scan(&one); err != nil {
		t.Fatal(err)
	}
	probe, err := pgx.Connect(ctx, os.Getenv("IDP_CARGO_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { probe.Close(context.Background()) })
	ptx, err := probe.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ptx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, f.tenant); err != nil {
		t.Fatal(err)
	}
	hold := &lockedMovement{t: t, f: f, watch: probe}
	hold.waiting = func() int {
		var n int
		if err := probe.QueryRow(ctx, blockedByHolder, tx.Conn().PgConn().PID()).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	results := map[string]chan error{}
	hold.start("refresh", func() error {
		_, err := f.module.RefreshHouseBillFacts(ctx, api.RefreshHouseBillFacts{CommandID: "refresh-1", TenantID: f.tenant, ActorID: "ops-1", MovementID: "OM-1",
			ExpectedRevision: f.get(t, "OM-1").Revision, ShipmentID: "SH-1", OccurredAt: f.hours(6)})
		return err
	}, results)
	// the probe asks for the movement's lock without waiting: the refresh has not taken it
	if err = ptx.QueryRow(ctx, `SELECT 1 FROM cargo_sea.ocean_movement WHERE tenant_id=$1 AND movement_id='OM-1' FOR NO KEY UPDATE NOWAIT`, f.tenant).Scan(&one); err != nil {
		t.Fatalf("the refresh holds the movement while it waits for the visit: %v", err)
	}
	_ = ptx.Rollback(ctx)
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = result(t, "refresh", results["refresh"]); err != nil {
		t.Fatalf("the refresh: %v", err)
	}
	if got := strings.Join(sealNumbers(f.visit(t, "CV-1")), " "); got != "S1/SHIPPER S2/SHIPPER" {
		t.Fatalf("seals: %s", got)
	}
}

// TestAContainerAssignedWhileTheVisitsAreTakenMakesTheRefreshAConflict: the movement's lock is held on a second
// connection; AssignContainer of a second container queues first (it waits for the movement), then a refresh that
// follows a seal (it lists the visits, locks the one it finds, and waits for the movement). When the lock is released
// the assignment commits and the refresh, finding two visits where it took one, is a conflict, with nothing written;
// tried again it takes both and follows the seal.
func TestAContainerAssignedWhileTheVisitsAreTakenMakesTheRefreshAConflict(t *testing.T) {
	ctx := context.Background()
	f := newReworkFixture(t)
	f.day(t)
	f.reseal()
	movement := f.get(t, "OM-1").Revision
	hold := f.holdMovement(t)
	results := map[string]chan error{}
	hold.start("assign", func() error {
		_, err := f.assign("CV-2", "OM-1", 2, "TEMU1234565", 6)
		return err
	}, results)
	hold.start("refresh", func() error {
		_, err := f.module.RefreshHouseBillFacts(ctx, api.RefreshHouseBillFacts{CommandID: "race-refresh", TenantID: f.tenant, ActorID: "ops-1", MovementID: "OM-1",
			ExpectedRevision: movement, ShipmentID: "SH-1", OccurredAt: f.hours(7)})
		return err
	}, results)
	hold.release()
	if err := result(t, "assign", results["assign"]); err != nil {
		t.Fatalf("the assignment: %v", err)
	}
	err := result(t, "refresh", results["refresh"])
	if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "a container was assigned while the command was taking the movement's visits") {
		t.Fatalf("the refresh: %v", err)
	}
	if got := strings.Join(sealNumbers(f.visit(t, "CV-1")), " "); got != "S1/SHIPPER" || f.get(t, "OM-1").Revision != movement {
		t.Fatalf("the refused refresh wrote: %s", got)
	}
	if _, err = f.refresh("race-refresh-2", "SH-1", 7); err != nil {
		t.Fatalf("tried again: %v", err)
	}
	if got := strings.Join(sealNumbers(f.visit(t, "CV-1")), " "); got != "S1/SHIPPER S2/SHIPPER" {
		t.Fatalf("seals: %s", got)
	}
}

// A released visit is not listed by the pending query and does not block Finalize.
func TestAReleasedVisitIsNotPendingOnPostgres(t *testing.T) {
	f := newReworkFixture(t)
	f.day(t)
	if _, err := f.assign("CV-2", "OM-1", 2, "TEMU1234565", 5); err != nil {
		t.Fatal(err)
	}
	if got := pendingKinds(f.pending(t)); got != api.PendingContainerNoSeal {
		t.Fatalf("an assigned container with no seal: %s", got)
	}
	if _, err := f.release("release-CV-2", "CV-2", 1, "not used", 5); err != nil {
		t.Fatal(err)
	}
	if got := pendingKinds(f.pending(t)); got != "" {
		t.Fatalf("a released container: %s", got)
	}
	if _, err := f.finalize("fin-1", 6); err != nil {
		t.Fatalf("finalize with a released container: %v", err)
	}
}
