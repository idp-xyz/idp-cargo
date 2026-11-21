//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
)

// changeBasis changes the stand-in PHY's PhysicalCostBasis v1 of the world's consolidation.
func (u usedWorld) changeBasis(change func(b *phyapi.PhysicalCostBasis)) {
	u.w.mu.Lock()
	defer u.w.mu.Unlock()
	b := u.w.costBases["CONSOL-U"]
	change(&b)
	u.w.costBases["CONSOL-U"] = b
}

// sealedUnit gives the consolidation one sealed container X-1 with its packing list.
func (u usedWorld) sealedUnit() {
	u.changeBasis(func(b *phyapi.PhysicalCostBasis) {
		b.LoadUnits = []phyapi.PlannedLoadUnit{{UnitNo: 1, Kind: "CONTAINER", TypeCode: "22G1", TEUTenths: 10, Status: phyapi.UnitStatusClosed, ExecutionID: "X-1",
			ExecutionState: phyapi.ExecutionClosed, ExecutionRevision: 3}}
		b.PackingLists = []phyapi.UnitPackingList{{UnitNo: 1, ExecutionID: "X-1", ExecutionState: phyapi.ExecutionClosed, Digest: "digest-1"}}
	})
}

// openRework puts the container in rework, asked by who; reseal ends the rework SEALED at the given hour and moves the
// digest.
func (u usedWorld) openRework(who string) {
	u.changeBasis(func(b *phyapi.PhysicalCostBasis) {
		b.LoadUnits[0].Status, b.LoadUnits[0].ExecutionState = phyapi.UnitStatusInUse, phyapi.ExecutionRework
		b.PackingLists[0].ExecutionState, b.PackingLists[0].Digest = phyapi.ExecutionRework, "digest-rework"
		b.PackingLists[0].Reworks = []phyapi.Rework{{ReworkNo: 1, AskedBy: who, AskedAt: u.hours(3)}}
	})
}

func (u usedWorld) reseal(hour int) {
	u.changeBasis(func(b *phyapi.PhysicalCostBasis) {
		b.LoadUnits[0].Status, b.LoadUnits[0].ExecutionState = phyapi.UnitStatusClosed, phyapi.ExecutionClosed
		b.PackingLists[0].ExecutionState, b.PackingLists[0].Digest = phyapi.ExecutionClosed, "digest-resealed"
		b.PackingLists[0].Reworks[0].End, b.PackingLists[0].Reworks[0].EndedAt = phyapi.ReworkSealed, u.hours(hour)
	})
}

func (u usedWorld) preview(id string, hour int, lines []int, notUsed ...string) (api.Receipt, error) {
	return u.module.PreviewConsolCostApportionment(u.ctx, u.apportionCmd(id, hour, lines, notUsed...))
}

func (u usedWorld) submitCmd(id, actor string, previewNo int, hour int) api.SubmitConsolCostApportionment {
	return api.SubmitConsolCostApportionment{CommandID: id, TenantID: u.tenant, ActorID: actor, PoolID: u.pool, ExpectedRevision: u.revision(), PreviewNo: previewNo,
		OccurredAt: u.hours(hour)}
}

func (u usedWorld) submit(id, actor string, previewNo int, hour int) (api.Receipt, error) {
	return u.module.SubmitConsolCostApportionment(u.ctx, u.submitCmd(id, actor, previewNo, hour))
}

func (u usedWorld) inWork(name string, err error) {
	u.t.Helper()
	u.refusedWith(name, err, "FIN_POOL_IN_WORK")
	if !strings.Contains(err.Error(), "load unit 1 (stuffing X-1)") || !strings.Contains(err.Error(), "wait for PHY to seal the container again or to give its stuffing up") {
		u.t.Errorf("%s: the refusal does not name the container and the way out: %v", name, err)
	}
}

// previewWorld is the world of the used houses with one sealed container and one cost line of CNY 1,000.00 by
// CHARGEABLE (air: SH-A 300 kg, SH-B 200 kg: 600.00 and 400.00; SH-C, nothing loaded, is left out).
func previewWorld(t *testing.T) usedWorld {
	t.Helper()
	u := newUsedWorld(t)
	u.sealedUnit()
	u.record("cost-1", "AIR_FREIGHT", api.MethodChargeable, 100_000)
	return u
}

// A preview stores what FIN computed and changes nothing else; the submission writes it as an apportionment that names
// its preview, by the person who submits; both commands replay; the outbox has the apportionment's event and none for
// the preview, and the event carries no actor. The view lists the previews and the apportionments apart.
func TestAPreviewIsSubmittedOnPostgres(t *testing.T) {
	u := previewWorld(t)
	f := u.fixture
	previewCmd := u.apportionCmd("preview-1", 4, nil, "SH-C")
	r, err := u.module.PreviewConsolCostApportionment(u.ctx, previewCmd)
	if err != nil || r.NewRevision != 3 || r.Version != 1 || r.SubjectID != "P-U" {
		t.Fatalf("preview: %+v %v", r, err)
	}
	if again, err := u.module.PreviewConsolCostApportionment(u.ctx, previewCmd); err != nil || again != r {
		t.Fatalf("the replay of the preview: %+v %v", again, err)
	}
	p := u.ap.pool(u.pool)
	if p.Revision != 3 || len(p.Apportionments) != 0 || len(p.Previews) != 1 || len(p.Lines) != 1 || len(p.Lines[0].Versions) != 1 || p.InWork {
		t.Fatalf("a preview changed the pool: %+v", p)
	}
	v := p.Previews[0]
	if v.PreviewNo != 1 || v.By != "fin-1" || v.Stage != api.BasisActual || v.BasisRevision != 7 || v.SubmittedAs != 0 || len(v.FingerprintSHA256) != 64 || v.EnteredAt.IsZero() {
		t.Fatalf("the preview: %+v", v)
	}
	u.checkLine("previewed", api.PoolApportionment{Lines: v.Lines}, 1, api.MethodChargeable,
		api.PoolShare{ShareNo: 1, ShipmentID: "SH-A", JobID: "JOB-A", Quantity: 300_000, Unit: "GRM", Amount: cny(60_000), Local: cny(60_000)},
		api.PoolShare{ShareNo: 2, ShipmentID: "SH-B", JobID: "JOB-B", Quantity: 200_000, Unit: "GRM", Amount: cny(40_000), Local: cny(40_000)},
		api.PoolShare{ShareNo: 3, ShipmentID: "SH-C", JobID: "JOB-C", NotUsed: true, Amount: cny(0), Local: cny(0)})
	if cost := u.poolCost("JOB-A"); cost != 0 {
		t.Fatalf("a preview charged JOB-A %d", cost)
	}
	if types := f.outboxTypes(t); countOf(types, "ConsolCostsApportioned") != 0 {
		t.Fatalf("a preview has an event: %v", types)
	}

	// a preview that cannot be computed is not stored (TEU at the stage PLANNED: SH-C takes part and is not loaded)
	bad := u.apportionCmd("preview-bad", 4, nil)
	bad.Method = api.MethodTEU
	if r, err = u.module.PreviewConsolCostApportionment(u.ctx, bad); err == nil || !strings.Contains(err.Error(), "FIN_POOL_NO_BASIS") {
		t.Fatalf("a preview that cannot be computed: %+v %v", r, err)
	}
	if p = u.ap.pool(u.pool); len(p.Previews) != 1 || p.Revision != 3 {
		t.Fatalf("a refused preview was stored: %+v", p)
	}

	cmd := u.submitCmd("submit-1", "fin-2", 1, 5)
	s, err := u.module.SubmitConsolCostApportionment(u.ctx, cmd)
	if err != nil || s.NewRevision != 4 || s.Version != 1 || s.SubjectID != "P-U" {
		t.Fatalf("submit: %+v %v", s, err)
	}
	if again, err := u.module.SubmitConsolCostApportionment(u.ctx, cmd); err != nil || again != s {
		t.Fatalf("the replay of the submission: %+v %v", again, err)
	}
	changed := cmd
	changed.PreviewNo = 2
	if _, err = u.module.SubmitConsolCostApportionment(u.ctx, changed); !errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("the command id of the submission with another payload: %v", err)
	}
	p = u.ap.pool(u.pool)
	a := p.Apportionments[0]
	if len(p.Apportionments) != 1 || a.ApportionmentNo != 1 || a.PreviewNo != 1 || a.By != "fin-2" || a.Stage != api.BasisActual || a.BasisRevision != 7 ||
		p.Previews[0].SubmittedAs != 1 || p.Previews[0].By != "fin-1" || p.Revision != 4 {
		t.Fatalf("the submitted apportionment: %+v", p)
	}
	u.checkLine("submitted", a, 1, api.MethodChargeable, v.Lines[0].Shares[0], v.Lines[0].Shares[1], v.Lines[0].Shares[2])
	if cost := u.poolCost("JOB-A"); cost != 60_000 || u.poolCost("JOB-B") != 40_000 || u.poolCost("JOB-C") != 0 {
		t.Fatalf("job costs: %d %d %d", cost, u.poolCost("JOB-B"), u.poolCost("JOB-C"))
	}
	types := f.outboxTypes(t)
	if countOf(types, "ConsolCostsApportioned") != 1 {
		t.Fatalf("events: %v", types)
	}
	q, err := f.db.ReadExecutor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var payload string
	if err = q.QueryRow(context.Background(), `SELECT payload::text FROM bento.outbox WHERE scope=$1 AND source=$2 AND event_type='ConsolCostsApportioned.v1'`,
		f.tenant, api.Source).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	for _, person := range []string{"fin-1", "fin-2", "ops-1"} {
		if strings.Contains(payload, `"`+person+`"`) {
			t.Fatalf("the event names a person: %s", payload)
		}
	}
	if !strings.Contains(payload, `"subject_id": "P-U"`) || !strings.Contains(payload, `"shipment_id": "CONSOL-U"`) || !strings.Contains(payload, `"version": 1`) {
		t.Fatalf("the event: %s", payload)
	}

	// the preview is submitted once
	_, err = u.submit("submit-again", "fin-2", 1, 6)
	u.refusedWith("the preview submitted twice", err, "FIN_PREVIEW_SUBMITTED")
	if _, err = u.submit("submit-nothing", "fin-2", 7, 6); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "has no preview 7") {
		t.Fatalf("a preview the pool does not have: %v", err)
	}
	// nor is a submission dated before its preview made
	if _, err = u.preview("preview-3", 6, nil, "SH-C"); err != nil {
		t.Fatal(err)
	}
	if _, err = u.submit("submit-early", "fin-2", 2, 5); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "before the preview") {
		t.Fatalf("a submission dated before its preview: %v", err)
	}
}

func countOf(list []string, item string) (n int) {
	for _, x := range list {
		if x == item {
			n++
		}
	}
	return n
}

// A submission whose basis or pool is not what the preview read is refused as stale, naming what changed, for each kind
// of change, and the preview is neither lost nor changed: a new preview is submitted.
func TestAStaleSubmissionIsRefusedOnPostgres(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(u usedWorld)
		part   string
	}{
		"the basis revision": {func(u usedWorld) { u.changeBasis(func(b *phyapi.PhysicalCostBasis) { b.Revision++ }) }, "basis changed"},
		"a unit digest": {func(u usedWorld) {
			u.changeBasis(func(b *phyapi.PhysicalCostBasis) { b.PackingLists[0].Digest = "digest-2" })
		}, "packing.1 changed"},
		"a unit status": {func(u usedWorld) {
			u.changeBasis(func(b *phyapi.PhysicalCostBasis) { b.LoadUnits[0].Status = phyapi.UnitStatusInUse })
		}, "unit.1 changed"},
		"a rework that unloaded nothing": {func(u usedWorld) {
			u.changeBasis(func(b *phyapi.PhysicalCostBasis) {
				b.PackingLists[0].Reworks = []phyapi.Rework{{ReworkNo: 1, AskedBy: "ops-a", End: phyapi.ReworkSealed, EndedAt: u.hours(4)}}
			})
		}, "rework.1"},
		"a house loaded again": {func(u usedWorld) {
			u.changeBasis(func(b *phyapi.PhysicalCostBasis) { b.Houses[1].LoadedPacks = 1 })
		}, `house."SH-B" changed`},
		"a house gone": {func(u usedWorld) {
			u.changeBasis(func(b *phyapi.PhysicalCostBasis) { b.Houses = b.Houses[:2] })
		}, `house."SH-C" [`},
		"a cost revised": {func(u usedWorld) {
			u.must(u.module.ReviseConsolCost(u.ctx, api.ReviseConsolCost{CommandID: "revise", TenantID: u.tenant, ActorID: "fin-1", PoolID: u.pool,
				ExpectedRevision: u.revision(), LineNo: 1, ExpectedVersion: 1, Value: api.ChargeSide{Amount: cny(90_000)}, Reason: "re-quoted", OccurredAt: u.hours(5)}))
		}, "line.1 changed"},
		"a cost matched below its accrual, the version as it was": {func(u usedWorld) {
			u.must(u.module.RecordCreditorInvoice(u.ctx, api.RecordCreditorInvoice{CommandID: "invoice", TenantID: u.tenant, ActorID: "ap-1", InvoiceID: "CI-S",
				LegalEntityID: "LE", CreditorPartyID: "airline-mu", SupplierInvoiceNo: "S-1", InvoiceDate: u.hours(5).Format(time.DateOnly), Currency: "CNY",
				Lines: []api.CreditorInvoiceLine{{ChargeCode: "AIR_FREIGHT", Amount: 99_000}}, OccurredAt: u.hours(5)}))
			u.must(u.module.MatchCreditorInvoice(u.ctx, u.match("CI-S", "ap-1", 5, onPool("P-U", 1, 1, 99_000, true))))
		}, "line.1 changed from [version 1 cost 100000"},
		"a cost released": {func(u usedWorld) {
			u.must(u.module.ReleasePoolAccrual(u.ctx, api.ReleasePoolAccrual{CommandID: "release", TenantID: u.tenant, ActorID: "fin-1", PoolID: u.pool,
				ExpectedRevision: u.revision(), LineNo: 1, ExpectedVersion: 1, Reason: "no invoice will come", OccurredAt: u.hours(5)}))
		}, "line.1 changed"},
	} {
		u := previewWorld(t)
		r, err := u.preview("preview", 4, nil, "SH-C")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		tc.change(u)
		_, err = u.submit("submit", "fin-2", int(r.Version), 6)
		u.refusedWith(name, err, "FIN_PREVIEW_STALE")
		if !strings.Contains(err.Error(), tc.part) || !strings.Contains(err.Error(), "preview again") {
			t.Errorf("%s: the refusal does not name what changed and the way out: %v", name, err)
		}
		if p := u.ap.pool(u.pool); len(p.Apportionments) != 0 || len(p.Previews) != 1 {
			t.Errorf("%s: %+v", name, p)
		}
	}

	// the lines the preview did not divide are not part of what it read: a new line and a revision of another line
	for name, change := range map[string]func(u usedWorld){
		"a line recorded": func(u usedWorld) { u.record("cost-2", "HANDLING", api.MethodShipment, 5_000) },
		"a revision of another line": func(u usedWorld) {
			u.record("cost-2", "HANDLING", api.MethodShipment, 5_000)
			u.must(u.module.ReviseConsolCost(u.ctx, api.ReviseConsolCost{CommandID: "revise-2", TenantID: u.tenant, ActorID: "fin-1", PoolID: u.pool,
				ExpectedRevision: u.revision(), LineNo: 2, ExpectedVersion: 1, Value: api.ChargeSide{Amount: cny(6_000)}, Reason: "re-quoted", OccurredAt: u.hours(5)}))
		},
	} {
		u := previewWorld(t)
		u.record("cost-0", "WEIGHT", api.MethodGrossWeight, 10_000)
		r := u.must(u.preview("preview", 4, []int{1}, "SH-C"))
		change(u)
		if _, err := u.submit("submit", "fin-2", int(r.Version), 6); err != nil {
			t.Errorf("%s: a preview of line 1 is refused: %v", name, err)
		}
	}

	// the way out: preview again, and submit that
	u := previewWorld(t)
	r, _ := u.preview("preview", 4, nil, "SH-C")
	u.changeBasis(func(b *phyapi.PhysicalCostBasis) { b.Revision++ })
	if _, err := u.submit("submit", "fin-2", int(r.Version), 6); !errors.Is(err, api.ErrRejected) {
		t.Fatal(err)
	}
	r = u.must(u.preview("preview-again", 6, nil, "SH-C"))
	if _, err := u.submit("submit-again", "fin-2", int(r.Version), 6); err != nil {
		t.Fatalf("the new preview: %v", err)
	}
	if p := u.ap.pool(u.pool); len(p.Apportionments) != 1 || p.Apportionments[0].PreviewNo != 2 || p.Apportionments[0].BasisRevision != 8 {
		t.Fatalf("the pool: %+v", p)
	}
}

// The revenue a line by REVENUE read is part of what a submission compares, read under the jobs' lock.
func TestAStaleRevenueIsRefusedOnPostgres(t *testing.T) {
	u := jobsPool(t, api.MethodRevenue, false)
	r, err := u.module.PreviewConsolCostApportionment(u.ctx, u.apportionCmd("preview", 5, nil))
	if err != nil {
		t.Fatal(err)
	}
	u.reviseSell("revise-b", "JOB-B", 1, 90_000, 6)
	_, err = u.module.SubmitConsolCostApportionment(u.ctx, u.submitCmd("submit", "fin-1", int(r.Version), 7))
	u.refusedWith("the revenue of a house changed", err, "FIN_PREVIEW_STALE")
	if !strings.Contains(err.Error(), `revenue.1."SH-B" changed from [30000] to [90000]`) {
		t.Fatalf("the refusal does not name the house and its revenue: %v", err)
	}
	r = u.must(u.module.PreviewConsolCostApportionment(u.ctx, u.apportionCmd("preview-again", 7, nil)))
	if _, err = u.module.SubmitConsolCostApportionment(u.ctx, u.submitCmd("submit-again", "fin-1", int(r.Version), 7)); err != nil {
		t.Fatalf("the new preview: %v", err)
	}
	if got := u.ap.pool(u.pool).Apportionments[0].Lines[0].Shares[1].Quantity; got != 90_000 {
		t.Fatalf("SH-B's revenue in the apportionment: %d", got)
	}
}

// While the stand-in PHY reads the container in REWORK, a preview, a submission and the close are refused naming the
// container and the way out; recording, revising and releasing costs go on; the pool says it is in work and shows the
// last submitted apportionment. After the reseal the freeze is gone, the preview made before is stale, and the
// apportionment is submitted by someone other than the asker of the rework, once.
func TestAFrozenPoolOnPostgres(t *testing.T) {
	u := previewWorld(t)
	u.record("cost-2", "HANDLING", api.MethodShipment, 5_000)
	first := u.must(u.preview("preview-1", 4, []int{1}, "SH-C"))
	u.must(u.submit("submit-1", "fin-1", int(first.Version), 4))
	before := u.must(u.preview("preview-before", 4, []int{2}, "SH-C")) // a preview made before the rework
	u.openRework("ops-asker")

	_, err := u.preview("preview-open", 5, []int{1}, "SH-C")
	u.inWork("a preview", err)
	_, err = u.submit("submit-open", "fin-2", int(before.Version), 5)
	u.inWork("a submission", err)
	_, err = u.module.CloseConsolCostPool(u.ctx, api.CloseConsolCostPool{CommandID: "close-open", TenantID: u.tenant, ActorID: "fin-1", PoolID: u.pool,
		ExpectedRevision: u.revision(), OccurredAt: u.hours(5)})
	u.inWork("the close", err)

	p := u.ap.pool(u.pool)
	if !p.InWork || len(p.WorkUnits) != 1 || p.WorkUnits[0] != (api.WorkUnit{UnitNo: 1, ExecutionID: "X-1"}) || len(p.Apportionments) != 1 || len(p.Previews) != 2 || p.State != api.PoolOpen {
		t.Fatalf("the pool in work: %+v", p)
	}
	profit, err := u.module.GetConsolProfit(u.ctx, api.GetConsolProfit{TenantID: u.tenant, ConsolidationID: "CONSOL-U"})
	if err != nil || !profit.InWork || len(profit.WorkUnits) != 1 || profit.Apportioned.Minor != 100_000 {
		t.Fatalf("the consolidation's economics in work: %+v %v", profit, err)
	}
	if cost := u.poolCost("JOB-A"); cost != 60_000 {
		t.Fatalf("what JOB-A bears while the pool is in work is the last submitted apportionment: %d", cost)
	}

	// recording, revising and releasing go on
	u.must(u.module.RecordConsolCost(u.ctx, api.RecordConsolCost{CommandID: "cost-3", TenantID: u.tenant, ActorID: "fin-1", PoolID: u.pool, ExpectedRevision: u.revision(),
		ChargeCode: "CUSTOMS", Method: api.MethodShipment, Cost: api.ChargeSide{PartyID: "airline-mu", Amount: cny(3_000)}, Reason: "entered while the pool is in work",
		OccurredAt: u.hours(5)}))
	u.must(u.module.ReviseConsolCost(u.ctx, api.ReviseConsolCost{CommandID: "revise-open", TenantID: u.tenant, ActorID: "fin-1", PoolID: u.pool,
		ExpectedRevision: u.revision(), LineNo: 3, ExpectedVersion: 1, Value: api.ChargeSide{Amount: cny(3_500)}, Reason: "re-quoted", OccurredAt: u.hours(5)}))
	u.must(u.module.ReleasePoolAccrual(u.ctx, api.ReleasePoolAccrual{CommandID: "release-open", TenantID: u.tenant, ActorID: "fin-1", PoolID: u.pool,
		ExpectedRevision: u.revision(), LineNo: 3, ExpectedVersion: 2, Reason: "no invoice will come", OccurredAt: u.hours(5)}))

	// and so does the matching of a creditor's invoice
	u.must(u.module.RecordCreditorInvoice(u.ctx, api.RecordCreditorInvoice{CommandID: "invoice-open", TenantID: u.tenant, ActorID: "ap-1", InvoiceID: "CI-F",
		LegalEntityID: "LE", CreditorPartyID: "airline-mu", SupplierInvoiceNo: "F-1", InvoiceDate: u.hours(5).Format(time.DateOnly), Currency: "CNY",
		Lines: []api.CreditorInvoiceLine{{ChargeCode: "AIR_FREIGHT", Amount: 100_000}}, OccurredAt: u.hours(5)}))
	u.must(u.module.MatchCreditorInvoice(u.ctx, u.match("CI-F", "ap-1", 5, onPool("P-U", 1, 1, 100_000, true))))
	if p = u.ap.pool(u.pool); !p.InWork || p.Lines[0].Status != api.StatusMatched {
		t.Fatalf("the pool in work after a match: %+v", p)
	}

	// sealed again: not in work; the old preview is stale
	u.reseal(6)
	if p = u.ap.pool(u.pool); p.InWork || len(p.WorkUnits) != 0 {
		t.Fatalf("after the reseal: %+v", p)
	}
	_, err = u.submit("submit-old", "fin-2", int(before.Version), 7)
	u.refusedWith("the preview made before the rework", err, "FIN_PREVIEW_STALE")
	if !strings.Contains(err.Error(), "packing.1 changed") || !strings.Contains(err.Error(), "rework.1") {
		t.Fatalf("the stale refusal does not name the container and the rework: %v", err)
	}
	now := u.must(u.preview("preview-after", 7, []int{1, 2, 3}, "SH-C"))
	_, err = u.submit("submit-asker", "ops-asker", int(now.Version), 7)
	u.refusedWith("the asker of the rework", err, "FIN_CONFIRMER_ASKED_REWORK")
	if !strings.Contains(err.Error(), "ops-asker asked for rework 1 of load unit 1 (stuffing X-1)") || !strings.Contains(err.Error(), "another finance user submits") {
		t.Fatalf("the refusal does not name the rework and the way out: %v", err)
	}
	if p = u.ap.pool(u.pool); len(p.Apportionments) != 1 {
		t.Fatalf("the asker's submission was written: %+v", p)
	}
	u.must(u.submit("submit-other", "fin-2", int(now.Version), 7))
	p = u.ap.pool(u.pool)
	if len(p.Apportionments) != 2 || p.Apportionments[1].By != "fin-2" || p.Apportionments[1].PreviewNo != int(now.Version) {
		t.Fatalf("the submission of another finance user: %+v", p)
	}

	// carried: the asker's next submission is accepted
	next := u.must(u.preview("preview-next", 8, []int{1}, "SH-C"))
	u.must(u.submit("submit-next", "ops-asker", int(next.Version), 8))
}

// The apportionment of a pool that was never reworked is previewed and submitted as it was written in one step: the
// figures of a pool by every method, once with a house left out.
func TestAPoolWithoutReworkIsApportionedAsBefore(t *testing.T) {
	u := previewWorld(t)
	u.record("cost-2", "HANDLING", api.MethodShipment, 90_000)
	u.record("cost-3", "WEIGHT", api.MethodGrossWeight, 100_000)
	u.record("cost-4", "PIECES", api.MethodPieces, 100_000)
	r := u.must(u.preview("preview", 4, nil, "SH-C"))
	u.must(u.submit("submit", "fin-1", int(r.Version), 4))
	a := u.ap.pool(u.pool).Apportionments[0]
	u.checkLine("chargeable", a, 1, api.MethodChargeable,
		api.PoolShare{ShareNo: 1, ShipmentID: "SH-A", JobID: "JOB-A", Quantity: 300_000, Unit: "GRM", Amount: cny(60_000), Local: cny(60_000)},
		api.PoolShare{ShareNo: 2, ShipmentID: "SH-B", JobID: "JOB-B", Quantity: 200_000, Unit: "GRM", Amount: cny(40_000), Local: cny(40_000)},
		api.PoolShare{ShareNo: 3, ShipmentID: "SH-C", JobID: "JOB-C", NotUsed: true, Amount: cny(0), Local: cny(0)})
	u.checkLine("shipment", a, 2, api.MethodShipment,
		api.PoolShare{ShareNo: 1, ShipmentID: "SH-A", JobID: "JOB-A", Quantity: 1, Unit: "SHP", Amount: cny(45_000), Local: cny(45_000)},
		api.PoolShare{ShareNo: 2, ShipmentID: "SH-B", JobID: "JOB-B", Quantity: 1, Unit: "SHP", Amount: cny(45_000), Local: cny(45_000)},
		api.PoolShare{ShareNo: 3, ShipmentID: "SH-C", JobID: "JOB-C", NotUsed: true, Amount: cny(0), Local: cny(0)})
	u.checkLine("gross weight", a, 3, api.MethodGrossWeight,
		api.PoolShare{ShareNo: 1, ShipmentID: "SH-A", JobID: "JOB-A", Quantity: 300_000, Unit: "GRM", Amount: cny(75_000), Local: cny(75_000)},
		api.PoolShare{ShareNo: 2, ShipmentID: "SH-B", JobID: "JOB-B", Quantity: 100_000, Unit: "GRM", Amount: cny(25_000), Local: cny(25_000)},
		api.PoolShare{ShareNo: 3, ShipmentID: "SH-C", JobID: "JOB-C", NotUsed: true, Amount: cny(0), Local: cny(0)})
	u.checkLine("pieces", a, 4, api.MethodPieces,
		api.PoolShare{ShareNo: 1, ShipmentID: "SH-A", JobID: "JOB-A", Quantity: 4, Unit: "PCS", Amount: cny(66_667), Local: cny(66_667)},
		api.PoolShare{ShareNo: 2, ShipmentID: "SH-B", JobID: "JOB-B", Quantity: 2, Unit: "PCS", Amount: cny(33_333), Local: cny(33_333)},
		api.PoolShare{ShareNo: 3, ShipmentID: "SH-C", JobID: "JOB-C", NotUsed: true, Amount: cny(0), Local: cny(0)})
	// the houses left out are carried to the next apportionment, previewed as they were
	r = u.must(u.preview("preview-2", 5, []int{1}))
	u.must(u.submit("submit-2", "fin-1", int(r.Version), 5))
	if got := u.ap.pool(u.pool).Apportionments[1].Lines[0].Shares[2]; !got.NotUsed {
		t.Fatalf("SH-C was taken back in: %+v", got)
	}
}

// The close of a pool apportioned by preview and submission passes the same checks as before.
func TestAPoolApportionedByPreviewCloses(t *testing.T) {
	u := previewWorld(t)
	r := u.must(u.preview("preview", 4, nil, "SH-C"))
	u.must(u.submit("submit", "fin-1", int(r.Version), 4))
	u.must(u.module.RecordCreditorInvoice(u.ctx, api.RecordCreditorInvoice{CommandID: "invoice", TenantID: u.tenant, ActorID: "ap-1", InvoiceID: "CI-X",
		LegalEntityID: "LE", CreditorPartyID: "airline-mu", SupplierInvoiceNo: "X-1", InvoiceDate: u.hours(5).Format(time.DateOnly), Currency: "CNY",
		Lines: []api.CreditorInvoiceLine{{ChargeCode: "AIR_FREIGHT", Amount: 100_000}}, OccurredAt: u.hours(5)}))
	u.must(u.module.MatchCreditorInvoice(u.ctx, u.match("CI-X", "ap-1", 5, onPool("P-U", 1, 1, 100_000, true))))
	_, err := u.module.CloseConsolCostPool(u.ctx, api.CloseConsolCostPool{CommandID: "close", TenantID: u.tenant, ActorID: "fin-1", PoolID: u.pool,
		ExpectedRevision: u.revision(), OccurredAt: u.hours(6)})
	if err != nil { // SH-C is not loaded, and a house left out is compared for its presence only
		t.Fatalf("the close of a pool apportioned by preview and submission: %v", err)
	}
	if p := u.ap.pool(u.pool); p.State != api.PoolClosed {
		t.Fatalf("the pool: %+v", p)
	}
	// a closed pool takes no preview and no submission
	_, err = u.preview("preview-closed", 7, nil, "SH-C")
	if !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a preview of a closed pool: %v", err)
	}
}

// What the database refuses past FIN on the application role (V2.91): a preview written without its pool, with a
// fingerprint whose SHA-256 is not its own, with no figures, into a closed pool, out of number; one that is changed or
// removed; an apportionment that names a preview another pool has, or one named already, or one it does not match (its
// stage, its basis revision, its date, a share); and the control that an apportionment equal to its preview is
// accepted.
func TestPreviewsPastFINOnPostgres(t *testing.T) {
	u := previewWorld(t)
	f := u.fixture
	u.must(u.preview("preview-1", 4, nil, "SH-C"))
	u.must(u.preview("preview-2", 4, nil, "SH-C"))
	p := u.ap.pool(u.pool)
	prev := p.Previews[0]

	const bump = `UPDATE cargo_fin.consol_cost_pool SET revision=revision+1 WHERE tenant_id=$1 AND pool_id='P-U';
`
	const figures = `'[{"line_no":1,"line_version":1,"method":"SHIPMENT","amount_minor":100000,"local_minor":100000,"shares":[
{"share_no":1,"shipment_id":"SH-A","job_id":"JOB-A","used":true,"quantity":1,"unit":"SHP","amount_minor":100000,"local_minor":100000}]}]'::jsonb`
	// a text and the SHA-256 of it
	const text = `'basis'||chr(9)||'revision 7'||chr(10)`
	sha := `encode(sha256(convert_to(` + text + `,'UTF8')),'hex')`
	insertPreview := func(no int, figs, fingerprint, hash string) string {
		return fmt.Sprintf(`INSERT INTO cargo_fin.pool_apportionment_preview (tenant_id,pool_id,preview_no,basis_revision,stage,figures,fingerprint,fingerprint_sha256,
            made_by,made_at,entered_at) VALUES ($1,'P-U',%d,7,'ACTUAL',%s,%s,%s,'x',now(),now())`, no, figs, fingerprint, hash)
	}
	// an apportionment as the preview 1 reads it, with the changes the cases make; 'now()' is after the preview's own
	// date
	insertApportionment := func(no int, preview any, stage string, basis int, made string) string {
		return fmt.Sprintf(`INSERT INTO cargo_fin.pool_apportionment (tenant_id,pool_id,apportionment_no,basis_revision,stage,made_by,made_at,entered_at,preview_no)
            VALUES ($1,'P-U',%d,%d,'%s','x',%s,now(),%v)`, no, basis, stage, made, preview)
	}
	// the lines and shares of preview 1 (CHARGEABLE, SH-A 60,000, SH-B 40,000, SH-C not used), with an amount changed
	// in the cases
	const columns = `(tenant_id,pool_id,apportionment_no,line_no,share_no,shipment_id,job_id,used,quantity,unit,amount_minor,local_minor)`
	body := func(no int, a, b int64) string {
		return fmt.Sprintf(`;
INSERT INTO cargo_fin.pool_apportionment_line (tenant_id,pool_id,apportionment_no,line_no,line_version,method,amount_minor,local_minor)
            VALUES ($1,'P-U',%d,1,1,'CHARGEABLE',100000,100000);
INSERT INTO cargo_fin.pool_apportionment_share %s VALUES ($1,'P-U',%d,1,1,'SH-A','JOB-A',true,300000,'GRM',%d,%d);
INSERT INTO cargo_fin.pool_apportionment_share %s VALUES ($1,'P-U',%d,1,2,'SH-B','JOB-B',true,200000,'GRM',%d,%d);
INSERT INTO cargo_fin.pool_apportionment_share %s VALUES ($1,'P-U',%d,1,3,'SH-C','JOB-C',false,0,NULL,0,0)`, no, columns, no, a, a, columns, no, b, b, columns, no)
	}
	// a preview of a MANUAL line (reason "agreed") and an apportionment of it with the reason it is given
	const manualFigures = `'[{"line_no":1,"line_version":1,"method":"MANUAL","reason":"agreed","amount_minor":100000,"local_minor":100000,"shares":[
{"share_no":1,"shipment_id":"SH-A","job_id":"JOB-A","used":true,"quantity":60000,"amount_minor":60000,"local_minor":60000},
{"share_no":2,"shipment_id":"SH-B","job_id":"JOB-B","used":true,"quantity":40000,"amount_minor":40000,"local_minor":40000},
{"share_no":3,"shipment_id":"SH-C","job_id":"JOB-C","used":false,"quantity":0,"amount_minor":0,"local_minor":0}]}]'::jsonb`
	manualPreview := insertPreview(3, manualFigures, text, sha) + ";\n"
	manualBody := func(no int, reason string) string {
		return fmt.Sprintf(`;
INSERT INTO cargo_fin.pool_apportionment_line (tenant_id,pool_id,apportionment_no,line_no,line_version,method,reason,amount_minor,local_minor)
            VALUES ($1,'P-U',%[1]d,1,1,'MANUAL','%[2]s',100000,100000);
INSERT INTO cargo_fin.pool_apportionment_share %[3]s VALUES ($1,'P-U',%[1]d,1,1,'SH-A','JOB-A',true,60000,NULL,60000,60000);
INSERT INTO cargo_fin.pool_apportionment_share %[3]s VALUES ($1,'P-U',%[1]d,1,2,'SH-B','JOB-B',true,40000,NULL,40000,40000);
INSERT INTO cargo_fin.pool_apportionment_share %[3]s VALUES ($1,'P-U',%[1]d,1,3,'SH-C','JOB-C',false,0,NULL,0,0)`, no, reason, columns)
	}
	run := func(sql string) error {
		return f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			for _, s := range strings.Split(sql, ";\n") {
				if _, err := e.Exec(ctx, s, f.tenant); err != nil {
					return err
				}
			}
			return nil
		})
	}
	for name, tc := range map[string]struct{ sql, state, check string }{
		"a preview written without its pool":                       {insertPreview(3, figures, text, sha), "23514", "fin_records_with_parent"},
		"a preview whose SHA-256 is another's":                     {bump + insertPreview(3, figures, text, `repeat('0',64)`), "23514", "pool_preview_fingerprint_sha256"},
		"a preview with no figures":                                {bump + insertPreview(3, `'[]'::jsonb`, text, sha), "23514", "pool_preview_figures"},
		"a preview with figures that are not a list":               {bump + insertPreview(3, `'{}'::jsonb`, text, sha), "23514", "pool_preview_figures"},
		"a preview out of number":                                  {bump + insertPreview(5, figures, text, sha), "23514", "consol_cost_pool_consistent"},
		"a preview of a stage outside the list":                    {bump + strings.Replace(insertPreview(3, figures, text, sha), "7,'ACTUAL',", "7,'NO_SUCH_STAGE',", 1), "23514", "pool_apportionment_preview_stage_check"},
		"a preview of no basis revision":                           {bump + strings.Replace(insertPreview(3, figures, text, sha), "7,'ACTUAL',", "0,'ACTUAL',", 1), "23514", "pool_apportionment_preview_basis_revision_check"},
		"a preview by nobody":                                      {bump + strings.Replace(insertPreview(3, figures, text, sha), "'x',now(),now())", "' ',now(),now())", 1), "23514", "pool_apportionment_preview_made_by_check"},
		"a preview numbered zero":                                  {bump + strings.Replace(insertPreview(3, figures, text, sha), "'P-U',3,", "'P-U',0,", 1), "23514", "pool_apportionment_preview_preview_no_check"},
		"a preview with no fingerprint":                            {bump + insertPreview(3, figures, `''`, `encode(sha256(''::bytea),'hex')`), "23514", "pool_apportionment_preview_fingerprint_check"},
		"a preview of the same number":                             {bump + insertPreview(1, figures, text, sha), "23505", "pool_apportionment_preview_pkey"},
		"a preview of another tenant":                              {bump + strings.Replace(insertPreview(3, figures, text, sha), "VALUES ($1,", "VALUES ($1||'-other',", 1), "42501", ""},
		"a preview changed":                                        {`UPDATE cargo_fin.pool_apportionment_preview SET made_by='y' WHERE tenant_id=$1`, "42501", ""},
		"a preview removed":                                        {`DELETE FROM cargo_fin.pool_apportionment_preview WHERE tenant_id=$1`, "42501", ""},
		"an apportionment of a preview the pool lacks":             {bump + insertApportionment(1, 9, "ACTUAL", 7, "now()") + body(1, 60_000, 40_000), "23503", "pool_apportionment_preview_same_pool"},
		"an apportionment of another stage":                        {bump + insertApportionment(1, 1, "PLANNED", 7, "now()") + body(1, 60_000, 40_000), "23514", "pool_apportionment_preview_matches"},
		"an apportionment of another basis revision":               {bump + insertApportionment(1, 1, "ACTUAL", 8, "now()") + body(1, 60_000, 40_000), "23514", "pool_apportionment_preview_matches"},
		"an apportionment made before its preview":                 {bump + insertApportionment(1, 1, "ACTUAL", 7, "now() - interval '1 day'") + body(1, 60_000, 40_000), "23514", "pool_apportionment_preview_matches"},
		"an apportionment with another method":                     {bump + insertApportionment(1, 1, "ACTUAL", 7, "now()") + strings.Replace(body(1, 60_000, 40_000), "'CHARGEABLE'", "'VOLUME'", 1), "23514", "pool_apportionment_preview_matches"},
		"an apportionment of another line version":                 {bump + insertApportionment(1, 1, "ACTUAL", 7, "now()") + strings.Replace(body(1, 60_000, 40_000), ",1,1,'CHARGEABLE'", ",1,2,'CHARGEABLE'", 1), "23503", ""},
		"an apportionment with the shares of another house":        {bump + insertApportionment(1, 1, "ACTUAL", 7, "now()") + strings.Replace(body(1, 60_000, 40_000), "'SH-B','JOB-B'", "'SH-D','JOB-B'", 1), "23514", "pool_apportionment_preview_matches"},
		"an apportionment with the shares of another job":          {bump + insertApportionment(1, 1, "ACTUAL", 7, "now()") + strings.Replace(body(1, 60_000, 40_000), "'SH-B','JOB-B'", "'SH-B','JOB-C'", 1), "23514", "pool_apportionment_preview_matches"},
		"a preview of a pool that does not exist":                  {bump + strings.Replace(insertPreview(3, figures, text, sha), "'P-U'", "'P-NONE'", 1), "23503", "pool_apportionment_preview_tenant_id_pool_id_fkey"},
		"an apportionment with another reason for its manual line": {bump + manualPreview + insertApportionment(1, 3, "ACTUAL", 7, "now()") + manualBody(1, "another reason"), "23514", "pool_apportionment_preview_matches"},
		"an apportionment with other shares":                       {bump + insertApportionment(1, 1, "ACTUAL", 7, "now()") + body(1, 50_000, 50_000), "23514", "pool_apportionment_preview_matches"},
	} {
		err := run(tc.sql)
		if state, constraint := sqlState(err); state != tc.state || (tc.check != "" && constraint != tc.check) {
			t.Errorf("%s: %v (state %s, constraint %s), want %s %s", name, err, state, constraint, tc.state, tc.check)
		}
	}

	// the privilege refuses a change or a removal first; as the migrator role, which holds them, the trigger does
	if dsn := os.Getenv("IDP_CARGO_MIGRATOR_DSN"); dsn != "" {
		conn, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(context.Background())
		for name, sql := range map[string]string{"changed": `UPDATE cargo_fin.pool_apportionment_preview SET made_by='y' WHERE tenant_id=$1`,
			"removed": `DELETE FROM cargo_fin.pool_apportionment_preview WHERE tenant_id=$1`} {
			_, err := conn.Exec(context.Background(), sql, f.tenant)
			if state, _ := sqlState(err); state != "23001" || err == nil || !strings.Contains(err.Error(), "pool_apportionment_preview rows are immutable") {
				t.Errorf("a preview %s by the migrator role: %v", name, err)
			}
		}
	}

	// a preview made by FIN reads back as it was written: its SHA-256 is that of its fingerprint, in the table
	q, err := f.db.ReadExecutor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ok bool
	if err = f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT fingerprint_sha256 = encode(sha256(convert_to(fingerprint,'UTF8')),'hex') AND jsonb_array_length(figures) = 1
            FROM cargo_fin.pool_apportionment_preview WHERE tenant_id=$1 AND pool_id='P-U' AND preview_no=1`, f.tenant).Scan(&ok)
	}); err != nil || !ok || len(prev.FingerprintSHA256) != 64 {
		t.Fatalf("the stored preview: %v %v", ok, err)
	}
	_ = q

	// the control: an apportionment as its preview reads is accepted, names it, and a second naming it is refused by
	// name
	if err = run(bump + insertApportionment(1, 1, "ACTUAL", 7, "now()") + body(1, 60_000, 40_000)); err != nil {
		t.Fatalf("an apportionment equal to its preview: %v", err)
	}
	if p = u.ap.pool(u.pool); len(p.Apportionments) != 1 || p.Apportionments[0].PreviewNo != 1 || p.Previews[0].SubmittedAs != 1 {
		t.Fatalf("the control: %+v", p)
	}
	err = run(bump + insertApportionment(2, 1, "ACTUAL", 7, "now()") + body(2, 60_000, 40_000))
	if state, constraint := sqlState(err); state != "23505" || constraint != "pool_apportionment_one_per_preview" {
		t.Errorf("a second apportionment of one preview: %v (state %s, constraint %s)", err, state, constraint)
	}
	// an apportionment with no preview is what the rows of FIN before V2.91 are: still accepted by the schema
	if err = run(bump + insertApportionment(2, "NULL", "ACTUAL", 7, "now()") + body(2, 50_000, 50_000)); err != nil {
		t.Fatalf("an apportionment that names no preview: %v", err)
	}
	// and the apportionment of a preview of a MANUAL line, with the reason it has, is accepted
	if err = run(bump + manualPreview + insertApportionment(3, 3, "ACTUAL", 7, "now()") + manualBody(3, "agreed")); err != nil {
		t.Fatalf("an apportionment equal to the preview of a manual line: %v", err)
	}
}

// A closed pool takes no preview written past FIN either, and the numbering of the previews is checked at commit.
func TestAPreviewIntoAClosedPoolPastFINOnPostgres(t *testing.T) {
	u := newUsedWorld(t)
	f := u.fixture
	u.must(f.module.CloseConsolCostPool(u.ctx, api.CloseConsolCostPool{CommandID: "close-empty", TenantID: f.tenant, ActorID: "fin-1", PoolID: u.pool,
		ExpectedRevision: u.revision(), OccurredAt: u.hours(3)}))
	err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		if _, err := e.Exec(ctx, `UPDATE cargo_fin.consol_cost_pool SET revision=revision+1 WHERE tenant_id=$1 AND pool_id='P-U'`, f.tenant); err != nil {
			return err
		}
		_, err := e.Exec(ctx, `INSERT INTO cargo_fin.pool_apportionment_preview (tenant_id,pool_id,preview_no,basis_revision,stage,figures,fingerprint,fingerprint_sha256,
            made_by,made_at,entered_at) VALUES ($1,'P-U',1,7,'ACTUAL','[{}]'::jsonb,'x',encode(sha256(convert_to('x','UTF8')),'hex'),'x',now(),now())`, f.tenant)
		return err
	})
	if state, constraint := sqlState(err); state != "23514" || constraint != "consol_cost_pool_open" {
		t.Fatalf("a preview into a closed pool: %v (state %s, constraint %s)", err, state, constraint)
	}
}

// holdingPool begins a transaction on a connection of its own and takes the row lock FIN's pool commands take
// (PoolStore.Load: SELECT ... FOR NO KEY UPDATE), which it keeps until release is called. pid is its backend.
func holdingPool(t *testing.T, u usedWorld) (pid int, release func()) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("IDP_CARGO_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var one int
	if _, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, u.tenant); err == nil {
		err = tx.QueryRow(ctx, `SELECT 1 FROM cargo_fin.consol_cost_pool WHERE tenant_id=$1 AND pool_id=$2 FOR NO KEY UPDATE`, u.tenant, u.pool).Scan(&one)
	}
	if err == nil {
		err = tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid)
	}
	if err != nil {
		t.Fatalf("holding the lock of the pool: %v", err)
	}
	released := false
	release = func() {
		if !released {
			released = true
			_ = tx.Rollback(ctx)
			conn.Close(ctx)
		}
	}
	t.Cleanup(release)
	return pid, release
}

// waitBlockedBy waits until n sessions other than the holder are blocked by the holder's transaction, directly or
// behind one that is (a lock waiter queues behind the waiters before it): a session is counted only when
// pg_blocking_pids leads from it to the test's own lock holder, so a command that is merely slow, or blocked by
// something else, is not counted.
func waitBlockedBy(t *testing.T, holder, n int) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("IDP_CARGO_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	deadline := time.Now().Add(15 * time.Second)
	var blocked int
	for time.Now().Before(deadline) {
		if err = conn.QueryRow(ctx, `WITH RECURSIVE chain(pid) AS (
            SELECT $1::int
            UNION
            SELECT a.pid FROM pg_stat_activity a JOIN chain c ON c.pid = ANY(pg_blocking_pids(a.pid)))
          SELECT count(*) - 1 FROM chain`, holder).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%d sessions are blocked by the lock holder, want %d", blocked, n)
}

// queued holds the pool's lock, starts the commands one after another (each only once the sessions before it are
// blocked by the holder), releases the lock, and answers what each returned: the first started is granted the lock
// first (Postgres grants a waiting lock in the order it was asked), so the order of the commits is forced.
func queued(t *testing.T, u usedWorld, commands ...func() error) []error {
	t.Helper()
	pid, release := holdingPool(t, u)
	errs := make([]error, len(commands))
	var wg sync.WaitGroup
	for i, run := range commands {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = run() }()
		waitBlockedBy(t, pid, i+1)
	}
	release()
	wg.Wait()
	return errs
}

// A submission races the revision of a cost of the same pool, both asked at the pool's revision after the preview.
// Exactly one commits, whichever is granted the pool's lock first: the submission, which leaves the cost as it was, or
// the revision, which leaves the submission refused by the pool's revision; and a submission asked at the revision
// after the revision is refused as stale, naming the line.
func TestASubmissionRacesACostRevision(t *testing.T) {
	for _, submissionFirst := range []bool{true, false} {
		u := previewWorld(t)
		r := u.must(u.preview("preview", 4, nil, "SH-C"))
		submit := u.submitCmd("submit", "fin-2", int(r.Version), 5)
		revise := api.ReviseConsolCost{CommandID: "revise", TenantID: u.tenant, ActorID: "fin-1", PoolID: u.pool, ExpectedRevision: submit.ExpectedRevision, LineNo: 1,
			ExpectedVersion: 1, Value: api.ChargeSide{Amount: cny(90_000)}, Reason: "re-quoted", OccurredAt: u.hours(5)}
		doSubmit := func() error { _, err := u.module.SubmitConsolCostApportionment(u.ctx, submit); return err }
		doRevise := func() error { _, err := u.module.ReviseConsolCost(u.ctx, revise); return err }
		var errs []error
		if submissionFirst {
			errs = queued(t, u, doSubmit, doRevise)
		} else {
			errs = queued(t, u, doRevise, doSubmit)
		}
		first, second := errs[0], errs[1]
		if first != nil || !errors.Is(second, api.ErrConflict) {
			t.Fatalf("submission first %v: the first returned %v, the second %v; want the first committed and the second a conflict", submissionFirst, first, second)
		}
		p := u.ap.pool(u.pool)
		if submissionFirst {
			if len(p.Apportionments) != 1 || len(p.Lines[0].Versions) != 1 || p.Lines[0].Cost.Minor != 100_000 {
				t.Fatalf("the submission committed first: %+v", p)
			}
			continue
		}
		if len(p.Apportionments) != 0 || len(p.Lines[0].Versions) != 2 || p.Lines[0].Cost.Minor != 90_000 {
			t.Fatalf("the revision committed first: %+v", p)
		}
		// asked at the revision after the revision, the submission meets the other guard
		_, err := u.submit("submit-fresh", "fin-2", int(r.Version), 5)
		u.refusedWith("the submission after the revision", err, "FIN_PREVIEW_STALE")
		if !strings.Contains(err.Error(), "line.1 changed") {
			t.Fatalf("the refusal does not name the line: %v", err)
		}
	}
}

// Two submissions of one preview race, asked at the same revision: exactly one commits (the pool's revision refuses the
// other), and one apportionment and one event are written. Asked at the new revision, the second is refused because the
// preview is submitted.
func TestTwoSubmissionsOfOnePreviewRace(t *testing.T) {
	u := previewWorld(t)
	r := u.must(u.preview("preview", 4, nil, "SH-C"))
	one, two := u.submitCmd("submit-1", "fin-2", int(r.Version), 5), u.submitCmd("submit-2", "fin-3", int(r.Version), 5)
	errs := queued(t, u,
		func() error { _, err := u.module.SubmitConsolCostApportionment(u.ctx, one); return err },
		func() error { _, err := u.module.SubmitConsolCostApportionment(u.ctx, two); return err })
	if errs[0] != nil || !errors.Is(errs[1], api.ErrConflict) {
		t.Fatalf("the first %v, the second %v", errs[0], errs[1])
	}
	p := u.ap.pool(u.pool)
	if len(p.Apportionments) != 1 || p.Apportionments[0].By != "fin-2" || countOf(u.outboxTypes(t), "ConsolCostsApportioned") != 1 {
		t.Fatalf("the pool: %+v", p)
	}
	_, err := u.submit("submit-3", "fin-3", int(r.Version), 5)
	u.refusedWith("the second at the new revision", err, "FIN_PREVIEW_SUBMITTED")
}

// The confirmer is per line, through FIN's commands (the review's scenario): after a rework ended, another person's
// apportionment of line 2 alone does not let the asker submit line 1; another person's submission of line 1 does, once.
func TestTheConfirmerIsPerLineOnPostgres(t *testing.T) {
	u := previewWorld(t)
	u.record("cost-2", "HANDLING", api.MethodChargeable, 20_000)
	first := u.must(u.preview("pre-before", 4, nil, "SH-C"))
	u.must(u.submit("sub-before", "fin-1", int(first.Version), 5))
	u.openRework("fin-asker")
	u.reseal(8)
	manual := u.apportionCmd("pre-b", 9, []int{2}, "SH-C")
	manual.Method, manual.Manual, manual.Reason, manual.ActorID = api.MethodManual,
		[]api.ManualShare{{ShipmentID: "SH-A", Amount: 10_000}, {ShipmentID: "SH-B", Amount: 10_000}}, "by hand", "fin-2"
	pb, err := u.module.PreviewConsolCostApportionment(u.ctx, manual)
	if err != nil {
		t.Fatal(err)
	}
	u.must(u.submit("sub-b", "fin-2", pb.Version, 10))
	pa := u.must(u.preview("pre-a", 11, []int{1}, "SH-C"))
	_, err = u.submit("sub-a", "fin-asker", pa.Version, 12)
	u.refusedWith("the asker's submission of line 1 after another person's of line 2", err, "FIN_CONFIRMER_ASKED_REWORK")
	if !strings.Contains(err.Error(), "for line 1") || !strings.Contains(err.Error(), "another finance user submits") {
		t.Fatalf("the refusal names neither the line nor the way out: %v", err)
	}
	u.must(u.submit("sub-a2", "fin-2", pa.Version, 12))
	next := u.must(u.preview("pre-a-next", 13, []int{1}, "SH-C"))
	if _, err = u.submit("sub-a-next", "fin-asker", next.Version, 14); err != nil {
		t.Fatalf("the asker's next submission of line 1: %v", err)
	}
	// a line recorded after the rework ended is carried by nothing
	u.must(u.module.RecordConsolCost(u.ctx, api.RecordConsolCost{CommandID: "cost-3", TenantID: u.tenant, ActorID: "fin-1", PoolID: u.pool, ExpectedRevision: u.revision(),
		ChargeCode: "CUSTOMS", Method: api.MethodShipment, Cost: api.ChargeSide{PartyID: "airline-mu", Amount: cny(3_000)}, Reason: "after the rework", OccurredAt: u.hours(15)}))
	late := u.must(u.preview("pre-late", 16, []int{3}, "SH-C"))
	_, err = u.submit("sub-late", "fin-asker", late.Version, 17)
	u.refusedWith("the asker's submission of a line recorded after the rework", err, "FIN_CONFIRMER_ASKED_REWORK")
	if !strings.Contains(err.Error(), "for line 3") {
		t.Fatalf("the refusal does not name line 3: %v", err)
	}
}

// A preview that carries the houses left out of the latest apportionment is stale once another apportionment of its
// line was submitted after it (the review's F5): the older preview no longer silently undoes the newer apportionment.
func TestAnOlderPreviewDoesNotUndoANewerApportionment(t *testing.T) {
	u := previewWorld(t)
	a1 := u.must(u.preview("p1", 4, nil, "SH-C"))
	u.must(u.submit("s1", "fin-1", a1.Version, 4))
	p2 := u.must(u.preview("p2", 5, nil)) // carries SH-C left out
	all := u.apportionCmd("p3", 5, nil)
	all.AllHouses = true
	p3, err := u.module.PreviewConsolCostApportionment(u.ctx, all)
	if err != nil {
		t.Fatal(err)
	}
	u.must(u.submit("s3", "fin-1", p3.Version, 6))
	_, err = u.submit("s2", "fin-1", p2.Version, 7)
	u.refusedWith("the older preview", err, "FIN_PREVIEW_STALE")
	if !strings.Contains(err.Error(), "line.1 changed") || !strings.Contains(err.Error(), "apportioned 1] to [") {
		t.Fatalf("the refusal does not name the line: %v", err)
	}
	if p := u.ap.pool(u.pool); len(p.Apportionments) != 2 || p.Apportionments[1].Lines[0].Shares[2].NotUsed {
		t.Fatalf("the newer apportionment was undone: %+v", p.Apportionments)
	}
}

// A submission locks the houses' jobs: a job row locked on a second connection makes it wait, and it commits once the
// lock is released. (The race with a job's cancellation shows the same lock from the other side.)
func TestASubmissionWaitsForTheJobsLock(t *testing.T) {
	u := previewWorld(t)
	r := u.must(u.preview("preview", 4, nil, "SH-C"))
	cmd := u.submitCmd("submit", "fin-2", int(r.Version), 5)
	pid, release := holdingJobPID(t, u, "JOB-B")
	done := make(chan error, 1)
	go func() { _, err := u.module.SubmitConsolCostApportionment(u.ctx, cmd); done <- err }()
	waitBlockedBy(t, pid, 1)
	// it waits in its read of the jobs (OpenJobsFor), not later at the commit, where the database takes the same lock
	conn, err := pgx.Connect(context.Background(), os.Getenv("IDP_CARGO_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var query string
	if err = conn.QueryRow(context.Background(), `SELECT query FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, pid).Scan(&query); err != nil ||
		!strings.Contains(query, "FROM cargo_fin.freight_job") || !strings.Contains(query, "FOR NO KEY UPDATE") {
		t.Fatalf("the submission waits in %q (%v), not in the lock of its read of the jobs", query, err)
	}
	if err, returned := returnsWithin(done, 200*time.Millisecond); returned {
		t.Fatalf("the submission returned (%v) while a job's lock was held", err)
	}
	release()
	if err, returned := returnsWithin(done, 10*time.Second); !returned || err != nil {
		t.Fatalf("the submission after the lock was released: %v (returned %v)", err, returned)
	}
	if p := u.ap.pool(u.pool); len(p.Apportionments) != 1 {
		t.Fatalf("the pool: %+v", p)
	}
}

// The new table has row security enabled and forced, as the tables of DOC have (a catalog check: the application role
// cannot tell FORCE from its absence by writing).
func TestThePreviewTableHasForcedRowSecurity(t *testing.T) {
	f := newFixture(t)
	var enabled, forced bool
	err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT c.relrowsecurity, c.relforcerowsecurity FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
            WHERE n.nspname = 'cargo_fin' AND c.relname = 'pool_apportionment_preview'`).Scan(&enabled, &forced)
	})
	if err != nil || !enabled || !forced {
		t.Fatalf("row security of the preview table: enabled %v, forced %v, %v", enabled, forced, err)
	}
}

// A rework opened in PHY after a command read PHY and before it commits is not seen by that command (FIN cannot lock
// PHY): it takes effect as of its read, as if made just before the rework. Run with a stand-in PHY (FIN's integration
// package has no real one; the real contexts cannot be held between the read and the commit): the pool's row lock is
// held on a second connection, the submission starts and reads PHY, waits for the lock, the rework is opened, the lock
// is released. The submission commits and is the last submitted apportionment of a pool in work; the rework, ended
// later, is not carried by it, so the asker's next submission is refused and another person's accepted. A close in the
// same window commits, and ReopenConsolCostPool is the way out.
func TestAReworkOpenedInTheWindowIsNotSeenByACommandThatReadPHYBefore(t *testing.T) {
	u := previewWorld(t)
	r := u.must(u.preview("preview", 4, nil, "SH-C"))
	cmd := u.submitCmd("submit", "fin-2", int(r.Version), 5)
	pid, release := holdingPool(t, u)
	done := make(chan error, 1)
	go func() { _, err := u.module.SubmitConsolCostApportionment(u.ctx, cmd); done <- err }()
	waitBlockedBy(t, pid, 1) // PHY has been read; the transaction waits for the pool
	u.openRework("fin-asker")
	release()
	if err, returned := returnsWithin(done, 10*time.Second); !returned || err != nil {
		t.Fatalf("the submission in the window: %v (returned %v)", err, returned)
	}
	p := u.ap.pool(u.pool)
	if !p.InWork || len(p.Apportionments) != 1 || p.Apportionments[0].By != "fin-2" {
		t.Fatalf("the pool after the window: %+v", p)
	}
	// the next command meets the freeze
	_, err := u.preview("preview-in-work", 6, nil, "SH-C")
	u.inWork("a preview after the window", err)
	// the rework ends: it was not read by the submission, so it is not carried
	u.reseal(8)
	next := u.must(u.preview("preview-next", 9, nil, "SH-C"))
	_, err = u.submit("submit-asker", "fin-asker", int(next.Version), 10)
	u.refusedWith("the asker after the window", err, "FIN_CONFIRMER_ASKED_REWORK")
	u.must(u.submit("submit-other", "fin-3", int(next.Version), 10))

	// a close in the same window
	c := previewWorld(t)
	pc := c.must(c.preview("preview", 4, nil, "SH-C"))
	c.must(c.submit("submit", "fin-1", int(pc.Version), 4))
	c.must(c.module.RecordCreditorInvoice(c.ctx, api.RecordCreditorInvoice{CommandID: "invoice", TenantID: c.tenant, ActorID: "ap-1", InvoiceID: "CI-W",
		LegalEntityID: "LE", CreditorPartyID: "airline-mu", SupplierInvoiceNo: "W-1", InvoiceDate: c.hours(5).Format(time.DateOnly), Currency: "CNY",
		Lines: []api.CreditorInvoiceLine{{ChargeCode: "AIR_FREIGHT", Amount: 100_000}}, OccurredAt: c.hours(5)}))
	c.must(c.module.MatchCreditorInvoice(c.ctx, c.match("CI-W", "ap-1", 5, onPool("P-U", 1, 1, 100_000, true))))
	closeCmd := api.CloseConsolCostPool{CommandID: "close", TenantID: c.tenant, ActorID: "fin-1", PoolID: c.pool, ExpectedRevision: c.revision(), OccurredAt: c.hours(6)}
	cpid, crelease := holdingPool(t, c)
	closed := make(chan error, 1)
	go func() { _, err := c.module.CloseConsolCostPool(c.ctx, closeCmd); closed <- err }()
	waitBlockedBy(t, cpid, 1)
	c.openRework("fin-asker")
	crelease()
	if err, returned := returnsWithin(closed, 10*time.Second); !returned || err != nil {
		t.Fatalf("the close in the window: %v (returned %v)", err, returned)
	}
	if p = c.ap.pool(c.pool); p.State != api.PoolClosed || !p.InWork {
		t.Fatalf("the pool closed in the window: %+v", p)
	}
	c.must(c.module.ReopenConsolCostPool(c.ctx, api.ReopenConsolCostPool{CommandID: "reopen", TenantID: c.tenant, ActorID: "fin-1", PoolID: c.pool, ExpectedRevision: c.revision(),
		Reason: "a container was opened for rework while the pool was closing", OccurredAt: c.hours(7)}))
	if p = c.ap.pool(c.pool); p.State != api.PoolOpen || !p.InWork {
		t.Fatalf("the pool reopened: %+v", p)
	}
}
