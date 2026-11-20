//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	scope "go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// A sealed container is opened for rework, a whole house is unloaded and the container is sealed again (V2.85), with
// the real commands and the real database. The day is the owner's: three houses H01, H02 and H03 of 10, 15 and 5 W/M
// (the greater of the weight in tonnes and the volume in cubic metres) in one container, H03 leaves.

// houseFigures are the packs, weight (kg) and volume (cm3) of a house of the day: H01 4 packs, 200 kg and 10 m3 (10
// W/M), H02 6 packs, 300 kg and 15 m3 (15 W/M), H03 2 packs, 100 kg and 5 m3 (5 W/M). The weights are FUL's plan, 50 kg
// a pack, so that no lot has a weight variance to resolve before it is closed; the volumes decide the W/M.
var houseFigures = map[string][3]int64{"H01": {4, 200, 10_000_000}, "H02": {6, 300, 15_000_000}, "H03": {2, 100, 5_000_000}}

// wm is a figure in W/M: the greater of the weight in tonnes and the volume in cubic metres, rounded up.
func wm(weight, volume kernel.Quantity) int64 {
	tonnes := (weight.Amount + 999) / 1000
	cubic := (volume.Amount + 999_999) / 1_000_000
	return max(tonnes, cubic)
}

// reworkDay builds a CLOSED own-container consolidation C-S with two 22G1 planned (units 1 and 2), the houses H01, H02
// and H03 attached (the consolidation at revision 4) and their lots L-H01, L-H02 and L-H03 received at the CFS, weighed
// and completed (each lot at revision 3).
func reworkDay(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	f := newFixture(t)
	f.createSea(t, "C-S", api.KindSeaLCLOwn)
	ok(t, 2)(f.module.PlanLoadUnit(ctx, f.planUnit("plan-1", "C-S", 1, "22G1")))
	ok(t, 3)(f.module.PlanLoadUnit(ctx, f.planUnit("plan-2", "C-S", 2, "22G1")))
	for i, h := range []string{"H01", "H02", "H03"} {
		f.ful.sea[h] = true
		f.ful.packs[h] = houseFigures[h][0]
		if _, err := f.module.AttachHouse(ctx, f.attach(fmt.Sprint("attach-", i), "C-S", h, 0)); err != nil {
			t.Fatal(err)
		}
		f.ful.allocate(h, "C-S")
	}
	ok(t, 4)(f.module.CloseConsolidation(ctx, f.closeCmd("close", "C-S", 3)))
	at := f.hoursIn(3)
	for _, h := range []string{"H01", "H02", "H03"} {
		lot, fig := "L-"+h, houseFigures[h]
		rcv := f.receive("rcv-"+lot, lot, h, 0, fig[0], api.ConditionGood, at)
		rcv.Facility = cfsShanghai
		ok(t, 1)(f.module.ReceiveCargo(ctx, rcv))
		ok(t, 2)(f.module.RecordCargoMeasurement(ctx, api.RecordCargoMeasurement{CommandID: "measure-" + lot, TenantID: f.tenant, ActorID: "ops-1",
			LotID: lot, ExpectedRevision: 1, GrossWeight: kgm(fig[1]), Volume: cmq(fig[2]), Method: api.MethodScale, Evidence: evidence("measure-"+lot, at), OccurredAt: at}))
		ok(t, 3)(f.module.CompleteReceipt(ctx, api.CompleteReceipt{CommandID: "complete-" + lot, TenantID: f.tenant, ActorID: "ops-1", LotID: lot,
			ExpectedRevision: 2, OccurredAt: at}))
	}
	return f
}

// stuffedAndSealed stuffs unit 1 with X-1 (all of H01, H02 and H03), seals it with SEAL-1 (X-1 at revision 6) and
// closes the lots of H01 and H03 (each lot at revision 4), as the warehouse does when everything of a house is loaded.
func (f *fixture) stuffedAndSealed(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	f.begin(t, "X-1", 1)
	at := f.hoursIn(4)
	for i, h := range []string{"H01", "H02", "H03"} {
		ok(t, int64(3+i))(f.module.RecordCargoLoaded(ctx, f.load("load-"+h, "X-1", int64(2+i), "L-"+h, houseFigures[h][0], at)))
	}
	ok(t, 6)(f.module.CloseLoadExecution(ctx, f.sealOf("X-1", 5, "SEAL-1")))
	for _, h := range []string{"H01", "H03"} {
		ok(t, 4)(f.module.CloseCargoLot(ctx, api.CloseCargoLot{CommandID: "close-lot-" + h, TenantID: f.tenant, ActorID: "ops-1", LotID: "L-" + h,
			ExpectedRevision: 3, OccurredAt: f.hoursIn(5)}))
	}
}

func (f *fixture) openRework(command, execution string, expected int64, reason, actor string, at time.Time) api.OpenStuffingForRework {
	return api.OpenStuffingForRework{CommandID: command, TenantID: f.tenant, ActorID: actor, ExecutionID: execution, ExpectedRevision: expected, Reason: reason, OccurredAt: at}
}

func (f *fixture) unloadHouse(command, execution string, expected int64, house string, at time.Time) api.UnloadHouseForRework {
	return api.UnloadHouseForRework{CommandID: command, TenantID: f.tenant, ActorID: "wh-1", ExecutionID: execution, ExpectedRevision: expected, ShipmentID: house,
		Evidence: api.Evidence{Source: "WMS", Reference: "UNSTUFF-" + house + "-" + command, OccurredAt: at}, OccurredAt: at}
}

func (f *fixture) lot(t *testing.T, id string) api.CargoLot {
	t.Helper()
	l, err := f.module.GetCargoLot(context.Background(), api.GetCargoLot{TenantID: f.tenant, LotID: id})
	if err != nil {
		t.Fatalf("lot %s: %v", id, err)
	}
	return l
}

func (f *fixture) house(t *testing.T, b api.PhysicalCostBasis, id string) api.HouseCostBasis {
	t.Helper()
	for _, h := range b.Houses {
		if h.ShipmentID == id {
			return h
		}
	}
	t.Fatalf("no house %s in %+v", id, b.Houses)
	return api.HouseCostBasis{}
}

// packing checks a packing list: its state, the houses it lists with their figures, and its digest, which is the
// canonical text's.
func packing(t *testing.T, what string, l api.UnitPackingList, execution, state string, houses ...string) {
	t.Helper()
	if l.ExecutionID != execution || l.ExecutionState != state || len(l.Lines) != len(houses) || l.Digest != digestOf(l) {
		t.Fatalf("%s: packing list %+v, want %s %s with %v and the digest of its text %s", what, l, execution, state, houses, digestOf(l))
	}
	for i, h := range houses {
		fig, line := houseFigures[h], l.Lines[i]
		if line.ShipmentID != h || line.LoadedPacks != fig[0] || line.LoadedGrossWeight != kgm(fig[1]) || line.LoadedVolume != cmq(fig[2]) {
			t.Fatalf("%s: line %d is %+v, want %s %v", what, i, line, h, fig)
		}
	}
}

func (f *fixture) types(t *testing.T) []string { return f.outboxTypes(t) }

// TestAContainerIsReworkedAndSealedAgain is the owner's day: a sealed container with H01, H02 and H03 (10, 15 and 5
// W/M) is opened for rework, H03 is unloaded (its lot was CLOSED and is open again), and the container is sealed again
// with a new seal; the cost basis before, during and after; the physical facts; the events in order; the replay of each
// command. Then a second rework unloads the rest and the stuffing is given up: the unit is planned again and stuffed
// anew.
func TestAContainerIsReworkedAndSealedAgain(t *testing.T) {
	ctx := context.Background()
	f := reworkDay(t)
	f.stuffedAndSealed(t)
	typesBefore := len(f.types(t))

	// before: sealed, CLOSED, the three houses at 10, 15 and 5 W/M
	before := f.basis(t)
	packing(t, "before", before.PackingLists[0], "X-1", api.ExecutionClosed, "H01", "H02", "H03")
	if u := before.LoadUnits[0]; u.Status != api.UnitStatusClosed || u.ExecutionID != "X-1" || u.ExecutionState != api.ExecutionClosed || u.ExecutionRevision != 6 {
		t.Fatalf("unit 1 before: %+v", u)
	}
	for h, want := range map[string]int64{"H01": 10, "H02": 15, "H03": 5} {
		hb := f.house(t, before, h)
		if got := wm(hb.LoadedGrossWeight, hb.LoadedVolume); got != want || hb.LoadedPacks != houseFigures[h][0] || len(hb.Units) != 1 {
			t.Fatalf("%s before: %d W/M (want %d), %+v", h, got, want, hb)
		}
	}
	if !before.PackingLists[0].Limits.Checked || before.PackingLists[0].Limits.CargoGrams != 600_000 || before.PackingLists[0].Limits.OverMaxGross {
		t.Fatalf("limits before: %+v", before.PackingLists[0].Limits)
	}
	if l := f.lot(t, "L-H03"); l.State != "CLOSED" || l.ClosedAt.IsZero() || len(l.Reopens) != 0 || l.Revision != 4 {
		t.Fatalf("lot L-H03 before: %+v", l)
	}
	if x := f.execution(t, "X-1"); len(x.Reworks) != 0 || x.State != api.ExecutionClosed || x.CloseEvidence.Reference != "SEAL-1" {
		t.Fatalf("X-1 before: %+v", x)
	}

	// the rework is opened by ops-asker, who is recorded with the reason
	open := f.openRework("rework-1", "X-1", 6, "the consignee of H03 withdrew its cargo", "ops-asker", f.hoursIn(6))
	r := ok(t, 7)(f.module.OpenStuffingForRework(ctx, open))
	if r.ExecutionID != "X-1" || r.ConsolidationID != "C-S" || r.ItemNo != 1 {
		t.Fatalf("receipt = %+v", r)
	}
	if again, err := f.module.OpenStuffingForRework(ctx, open); err != nil || again != r {
		t.Fatalf("replay = %+v, %v; want %+v", again, err, r)
	}
	if bad := open; true {
		bad.Reason = "another reason"
		if _, err := f.module.OpenStuffingForRework(ctx, bad); !errors.Is(err, api.ErrIdempotencyConflict) {
			t.Fatalf("a command id reused with another reason: %v", err)
		}
	}
	x := f.execution(t, "X-1")
	if x.State != api.ExecutionRework || !x.ClosedAt.IsZero() || x.CloseEvidence.Reference != "" || len(x.Reworks) != 1 || x.TenderedPacks != 12 || x.Revision != 7 {
		t.Fatalf("X-1 in rework: %+v", x)
	}
	rw := x.Reworks[0]
	if rw.ReworkNo != 1 || rw.AskedBy != "ops-asker" || !rw.AskedAt.Equal(f.hoursIn(6)) || rw.AskedEnteredAt.IsZero() || rw.Reason != "the consignee of H03 withdrew its cargo" ||
		rw.BrokenSeal.Source != api.EvidenceSeal || rw.BrokenSeal.Reference != "SEAL-1" || rw.BrokenSealedBy != "ops-1" || !rw.BrokenSealedAt.Equal(f.hoursIn(4)) || rw.End != "" {
		t.Fatalf("the rework's record: %+v", rw)
	}

	// during: IN_USE, REWORK, the same three houses, a digest that moved with the state, the rework in the packing list
	during := f.basis(t)
	packing(t, "during", during.PackingLists[0], "X-1", api.ExecutionRework, "H01", "H02", "H03")
	if during.PackingLists[0].Digest == before.PackingLists[0].Digest {
		t.Fatal("the digest moves with the state")
	}
	if u := during.LoadUnits[0]; u.Status != api.UnitStatusInUse || u.ExecutionID != "X-1" || u.ExecutionState != api.ExecutionRework || u.ExecutionRevision != 7 {
		t.Fatalf("unit 1 during: %+v", u)
	}
	if rs := during.PackingLists[0].Reworks; len(rs) != 1 || rs[0].AskedBy != "ops-asker" || rs[0].BrokenSeal.Reference != "SEAL-1" {
		t.Fatalf("who asked is where FIN reads it: %+v", rs)
	}
	if !during.PackingLists[0].Limits.Checked || during.PackingLists[0].Limits.CargoGrams != 600_000 {
		t.Fatalf("limits during are worked out as for any other: %+v", during.PackingLists[0].Limits)
	}
	if during.State != api.StateClosed {
		t.Fatalf("the consolidation stays CLOSED: %s", during.State)
	}
	evidenceOf, err := f.module.GetPhysicalLoadEvidence(ctx, api.GetPhysicalLoadEvidence{TenantID: f.tenant, ShipmentID: "H03"})
	if err != nil || len(evidenceOf.Tenders) != 1 {
		t.Fatalf("physical facts: %+v %v", evidenceOf, err)
	}
	if tn := evidenceOf.Tenders[0]; tn.State != api.ExecutionRework || tn.LoadedPacks != 2 || tn.GrossWeight != kgm(100) || !tn.ClosedAt.IsZero() || tn.CloseEvidence.Reference != "" ||
		len(tn.Reworks) != 1 || tn.Reworks[0].AskedBy != "ops-asker" || tn.Reworks[0].BrokenSeal.Reference != "SEAL-1" || !tn.Reworks[0].BrokenSealedAt.Equal(f.hoursIn(4)) ||
		tn.Reworks[0].End != "" {
		t.Fatalf("the physical facts during the rework: %+v", tn)
	}

	// H03 is unloaded whole: its lot, CLOSED, is open again and records what caused it
	unload := f.unloadHouse("unload-h03", "X-1", 7, "H03", f.hoursIn(7))
	r = ok(t, 8)(f.module.UnloadHouseForRework(ctx, unload))
	if r.ItemNo != 1 || r.ExecutionID != "X-1" {
		t.Fatalf("the receipt names the lines reversed: %+v", r)
	}
	if again, err := f.module.UnloadHouseForRework(ctx, unload); err != nil || again != r {
		t.Fatalf("replay = %+v, %v; want %+v", again, err, r)
	}
	lot := f.lot(t, "L-H03")
	if lot.State != "RECEIVED" || !lot.ClosedAt.IsZero() || lot.LoadedPacks != 0 || lot.ReceivedPacks != 2 || lot.Revision != 5 || len(lot.Reopens) != 1 {
		t.Fatalf("lot L-H03 after: %+v", lot)
	}
	if ro := lot.Reopens[0]; ro.ReopenNo != 1 || ro.ExecutionID != "X-1" || ro.ReworkNo != 1 || ro.ReversalNo != 1 || ro.ClosedBy != "ops-1" || !ro.ClosedAt.Equal(f.hoursIn(5)) ||
		ro.ReopenedBy != "wh-1" || !ro.ReopenedAt.Equal(f.hoursIn(7)) || ro.EnteredAt.IsZero() {
		t.Fatalf("the lot's record of its reopening: %+v", ro)
	}
	x = f.execution(t, "X-1")
	if x.State != api.ExecutionRework || x.TenderedPacks != 10 || len(x.Reversals) != 1 || x.Reversals[0].Packs != 2 || x.Reversals[0].Reason != api.ReversalUnstuffed ||
		x.Reversals[0].Evidence.Reference != "UNSTUFF-H03-unload-h03" || !x.Reversals[0].ReversedAt.Equal(f.hoursIn(7)) || x.Reversals[0].GrossWeight != kgm(100) {
		t.Fatalf("X-1 after the unloading: %+v", x)
	}
	// the events, in order: the rework opened, the lot opened again, the packs unloaded; the replays wrote none
	types := f.types(t)[typesBefore:]
	if !slices.Equal(types, []string{api.LoadExecutionReworkOpened, api.CargoLotReopened, api.CargoUnloaded}) {
		t.Fatalf("events = %v", types)
	}

	// during, H03 out: zero packs, no unit entry, the packing list without it, the digest moved again
	after := f.basis(t)
	h03 := f.house(t, after, "H03")
	if h03.LoadedPacks != 0 || h03.LoadedGrossWeight.Amount != 0 || h03.LoadedVolume.Amount != 0 || len(h03.Units) != 0 || h03.Packs != 2 || !h03.LastLoadedAt.IsZero() {
		t.Fatalf("H03 stays attached with nothing loaded: %+v", h03)
	}
	packing(t, "H03 out", after.PackingLists[0], "X-1", api.ExecutionRework, "H01", "H02")
	if after.PackingLists[0].Digest == during.PackingLists[0].Digest || after.PackingLists[0].Limits.CargoGrams != 500_000 {
		t.Fatalf("limits and digest after the unloading: %+v", after.PackingLists[0])
	}
	if len(after.Houses) != 3 {
		t.Fatalf("the house stays attached: %d houses", len(after.Houses))
	}

	// sealed again: the seal that was broken is not put back; a new one is
	putBack := f.sealOf("X-1", 8, "SEAL-1")
	putBack.CommandID = "seal-put-back"
	if _, err := f.module.CloseLoadExecution(ctx, putBack); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "was broken in rework 1") {
		t.Fatalf("the broken seal put back: %v", err)
	}
	reseal := api.CloseLoadExecution{CommandID: "reseal-1", TenantID: f.tenant, ActorID: "ops-sealer", ExecutionID: "X-1", ExpectedRevision: 8,
		Evidence: api.Evidence{Source: api.EvidenceSeal, Reference: "SEAL-2", OccurredAt: f.hoursIn(8)}, OccurredAt: f.hoursIn(8)}
	r = ok(t, 9)(f.module.CloseLoadExecution(ctx, reseal))
	if again, err := f.module.CloseLoadExecution(ctx, reseal); err != nil || again != r {
		t.Fatalf("replay = %+v, %v; want %+v", again, err, r)
	}
	final := f.basis(t)
	packing(t, "after", final.PackingLists[0], "X-1", api.ExecutionClosed, "H01", "H02")
	if u := final.LoadUnits[0]; u.Status != api.UnitStatusClosed || u.ExecutionState != api.ExecutionClosed || u.ExecutionRevision != 9 {
		t.Fatalf("unit 1 after: %+v", u)
	}
	for h, want := range map[string]int64{"H01": 10, "H02": 15, "H03": 0} {
		hb := f.house(t, final, h)
		if got := wm(hb.LoadedGrossWeight, hb.LoadedVolume); got != want {
			t.Fatalf("%s after: %d W/M, want %d: %+v", h, got, want, hb)
		}
	}
	if final.PackingLists[0].Digest == after.PackingLists[0].Digest || final.PackingLists[0].Digest == before.PackingLists[0].Digest {
		t.Fatal("the digest of the container sealed again is its own")
	}
	x = f.execution(t, "X-1")
	rw = x.Reworks[0]
	if x.State != api.ExecutionClosed || x.CloseEvidence.Reference != "SEAL-2" || x.ClosedAt.IsZero() || rw.End != api.ReworkSealed || rw.EndedBy != "ops-sealer" ||
		!rw.EndedAt.Equal(f.hoursIn(8)) || rw.NewSealReference != "SEAL-2" || rw.BrokenSeal.Reference != "SEAL-1" || rw.AskedBy != "ops-asker" {
		t.Fatalf("X-1 sealed again: %+v", x)
	}
	if want := []string{api.LoadExecutionReworkOpened, api.CargoLotReopened, api.CargoUnloaded, api.LoadExecutionClosed}; !slices.Equal(f.types(t)[typesBefore:], want) {
		t.Fatalf("events = %v, want %v", f.types(t)[typesBefore:], want)
	}

	// a second rework, of the same execution: the rest is unloaded and the stuffing is given up
	open2 := f.openRework("rework-2", "X-1", 9, "the whole booking is rolled", "ops-asker-2", f.hoursIn(9))
	r = ok(t, 10)(f.module.OpenStuffingForRework(ctx, open2))
	if r.ItemNo != 2 {
		t.Fatalf("the second rework is number 2: %+v", r)
	}
	if rs := f.execution(t, "X-1").Reworks; len(rs) != 2 || rs[0].End != api.ReworkSealed || rs[1].AskedBy != "ops-asker-2" || rs[1].BrokenSeal.Reference != "SEAL-2" ||
		rs[1].BrokenSealedBy != "ops-sealer" || rs[1].End != "" {
		t.Fatalf("the history of the seals: %+v", rs)
	}
	if _, err := f.module.CloseLoadExecution(ctx, f.sealOf("X-1", 10, "SEAL-2")); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "was broken in rework 2") {
		t.Fatalf("the second seal is not put back either: %v", err)
	}
	if _, err := f.module.CancelLoadExecution(ctx, f.cancelCmd("cancel-early", "X-1", 10, "rolled", f.hoursIn(10))); !errors.Is(err, api.ErrRejected) ||
		!strings.Contains(err.Error(), "10 packs are still loaded") {
		t.Fatalf("not given up with packs in it: %v", err)
	}
	ok(t, 11)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("unload-h02", "X-1", 10, "H02", f.hoursIn(10))))
	if l := f.lot(t, "L-H02"); l.State != "RECEIVED" || len(l.Reopens) != 0 {
		t.Fatalf("H02's lot was never closed and is not reopened: %+v", l)
	}
	ok(t, 12)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("unload-h01", "X-1", 11, "H01", f.hoursIn(11))))
	if l := f.lot(t, "L-H01"); l.State != "RECEIVED" || len(l.Reopens) != 1 || l.Reopens[0].ReworkNo != 2 || l.Reopens[0].ReversalNo != 3 {
		t.Fatalf("H01's lot was closed and is open again by the second rework: %+v", l)
	}
	if _, err := f.module.CloseLoadExecution(ctx, f.sealOf("X-1", 12, "SEAL-9")); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "nothing is stuffed") {
		t.Fatalf("an emptied container is not sealed: %v", err)
	}
	ok(t, 13)(f.module.CancelLoadExecution(ctx, f.cancelCmd("cancel-1", "X-1", 12, "the booking is rolled to the next sailing", f.hoursIn(12))))
	x = f.execution(t, "X-1")
	if x.State != api.ExecutionCancelled || x.CancelledBy != "ops-1" || len(x.Reworks) != 2 || x.Reworks[1].End != api.ReworkGivenUp || x.Reworks[1].EndedBy != "ops-1" ||
		!x.Reworks[1].EndedAt.Equal(f.hoursIn(12)) || x.Reworks[1].NewSealReference != "" || x.Reworks[0].NewSealReference != "SEAL-2" {
		t.Fatalf("X-1 given up: %+v", x)
	}
	planned := f.basis(t)
	if u := planned.LoadUnits[0]; u.Status != api.UnitStatusPlanned || u.ExecutionID != "" || u.ExecutionState != "" || u.ExecutionRevision != 0 || len(planned.PackingLists) != 0 {
		t.Fatalf("unit 1 is planned again: %+v, %d packing lists", u, len(planned.PackingLists))
	}
	for _, h := range planned.Houses {
		if h.LoadedPacks != 0 || len(h.Units) != 0 {
			t.Fatalf("%s: nothing is loaded: %+v", h.ShipmentID, h)
		}
	}
	// the unit is stuffed anew: a new execution, the lots as they are, and the same seal number is a new container's
	// seal
	ok(t, 1)(f.module.OpenLoadExecution(ctx, f.stuff("open-x2", "X-2", 1, f.hoursIn(13))))
	ok(t, 2)(f.module.StartLoading(ctx, api.StartLoading{CommandID: "start-x2", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-2", ExpectedRevision: 1, OccurredAt: f.hoursIn(13)}))
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load2-h01", "X-2", 2, "L-H01", 4, f.hoursIn(14))))
	ok(t, 4)(f.module.RecordCargoLoaded(ctx, f.load("load2-h02", "X-2", 3, "L-H02", 6, f.hoursIn(14))))
	seal := f.sealOf("X-2", 4, "SEAL-1")
	seal.OccurredAt, seal.Evidence.OccurredAt = f.hoursIn(15), f.hoursIn(15)
	ok(t, 5)(f.module.CloseLoadExecution(ctx, seal))
	anew := f.basis(t)
	packing(t, "stuffed anew", anew.PackingLists[0], "X-2", api.ExecutionClosed, "H01", "H02")
	if u := anew.LoadUnits[0]; u.Status != api.UnitStatusClosed || u.ExecutionID != "X-2" {
		t.Fatalf("unit 1 stuffed anew: %+v", u)
	}
	if len(f.execution(t, "X-2").Reworks) != 0 {
		t.Fatal("a new execution has no rework")
	}
}

// ---- the database's rules, with raw writes as the application role ----

// rawTx runs the statements, each with the tenant as $1, in one transaction of the application role: the error of the
// first that fails or of the commit, where the deferred rules are checked.
func (f *fixture) rawTx(t *testing.T, statements ...string) error {
	t.Helper()
	return f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		for _, s := range statements {
			if _, err := e.Exec(ctx, s, f.tenant); err != nil {
				return err
			}
		}
		return nil
	})
}

// refused runs the statements and checks that the database refuses them with the SQLSTATE and, when given, the
// constraint or a part of the message.
func (f *fixture) refused(t *testing.T, what, state, constraintOrText string, statements ...string) {
	t.Helper()
	err := f.rawTx(t, statements...)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != state || (constraintOrText != "" && pgErr.ConstraintName != constraintOrText && !strings.Contains(pgErr.Message, constraintOrText)) {
		t.Errorf("%s: %v, want %s %q", what, err, state, constraintOrText)
	}
}

const (
	// reworkRow inserts a rework of X-1 numbered %d, taking over the close facts X-1 has, asked two hours after the
	// seal (the %s are the expressions of the reason, the time asked and the broken seal's reference; "" keeps the
	// default).
	reworkRow = `INSERT INTO cargo_phy.stuffing_rework (tenant_id,execution_id,rework_no,reason,asked_by,asked_at,asked_entered_at,broken_seal_source,
        broken_seal_reference,broken_seal_sha256,broken_seal_occurred_at,broken_sealed_by,broken_sealed_at,broken_sealed_entered_at)
        SELECT tenant_id,execution_id,%d,%s,'raw-asker',%s,now(),close_evidence_source,%s,close_evidence_sha256,close_evidence_occurred_at,closed_by,closed_at,closed_entered_at
        FROM cargo_phy.load_execution WHERE tenant_id=$1 AND execution_id='X-1'`
	// breakSeal moves X-1 into REWORK with its close facts cleared.
	breakSeal = `UPDATE cargo_phy.load_execution SET state='REWORK', closed_by=NULL, closed_at=NULL, closed_entered_at=NULL, close_evidence_source=NULL,
        close_evidence_reference=NULL, close_evidence_sha256=NULL, close_evidence_occurred_at=NULL WHERE tenant_id=$1 AND execution_id='X-1'`
	// sealAgain seals X-1 again (from REWORK) with the seal named by %s at the time of the expression %s.
	sealAgain = `UPDATE cargo_phy.load_execution SET state='CLOSED', closed_by='raw-sealer', closed_at=%s, closed_entered_at=now(),
        close_evidence_source='SEAL', close_evidence_reference='%s', close_evidence_occurred_at=%s WHERE tenant_id=$1 AND execution_id='X-1'`
	// reversalRow reverses %d packs of line %d of X-1 as reversal %d for the reason %s, at the time of the expression
	// %s.
	reversalRow = `INSERT INTO cargo_phy.reversal_line (tenant_id,execution_id,reversal_no,line_no,packs,gross_weight,reason,evidence_source,evidence_reference,
        evidence_occurred_at,reversed_by,reversed_at,entered_at) VALUES ($1,'X-1',%d,%d,%d,1,'%s','x','raw-%d-%d',%s,'raw',%s,now())`
	// consolidationVersion writes the next version of C-S, a change after the last one.
	consolidationVersion = `INSERT INTO cargo_phy.consolidation_version (tenant_id,consolidation_id,version_no,change,state,origin_code,destination_code,origin_iata,
        destination_iata,location_edition,planned_departure,house_cut_off,reason,written_by,written_at,entered_at)
        SELECT tenant_id,consolidation_id,version_no+1,'%s','%s',origin_code,destination_code,origin_iata,destination_iata,location_edition,planned_departure,
          house_cut_off,%s,'raw',written_at + interval '10 hours',now() FROM cargo_phy.consolidation_version WHERE tenant_id=$1 AND consolidation_id='C-S'
        ORDER BY version_no DESC LIMIT 1`
	lotClosed = `UPDATE cargo_phy.cargo_lot SET state='RECEIVED', closed_by=NULL, closed_at=NULL, closed_entered_at=NULL WHERE tenant_id=$1 AND lot_id='%s'`
	// reopenRow records a reopening of the lot %s by the rework %d of X-1 and the reversal %d, with the closed facts
	// the lot has.
	reopenRow = `INSERT INTO cargo_phy.cargo_lot_reopen (tenant_id,lot_id,reopen_no,execution_id,rework_no,reversal_no,closed_by,closed_at,closed_entered_at,
        reopened_by,reopened_at,reopened_entered_at) SELECT tenant_id,lot_id,%d,'X-1',%d,%d,closed_by,closed_at,closed_entered_at,'raw',closed_at + interval '3 hours',now()
        FROM cargo_phy.cargo_lot WHERE tenant_id=$1 AND lot_id='%s'`
)

func rework(no int, reason, askedAt, reference string) string {
	if reason == "" {
		reason = "'raw reason'"
	}
	if askedAt == "" {
		askedAt = "closed_at + interval '2 hours'"
	}
	if reference == "" {
		reference = "close_evidence_reference"
	}
	return fmt.Sprintf(reworkRow, no, reason, askedAt, reference)
}

func reversal(no, line, packs int, reason, evidenceAt, at string) string {
	return fmt.Sprintf(reversalRow, no, line, packs, reason, no, line, evidenceAt, at)
}

// TestReworkDatabaseRules walks one container through its rework and tries, at each step, the raw writes the database
// must refuse, one rule at a time: the rows the domain never writes.
func TestReworkDatabaseRules(t *testing.T) {
	ctx := context.Background()
	f := reworkDay(t)
	f.stuffedAndSealed(t) // X-1 sealed with SEAL-1 at revision 6; the lots of H01 and H03 closed
	f.begin(t, "X-2", 2)  // a stuffing of unit 2 that is loading

	// X-1 sealed: it goes to REWORK only with its rework, whole
	f.refused(t, "REWORK with no rework recorded", "23514", "load_execution_rework_record", breakSeal)
	f.refused(t, "REWORK from a stuffing that is loading", "23001", "cannot go from LOADING to REWORK",
		`UPDATE cargo_phy.load_execution SET state='REWORK' WHERE tenant_id=$1 AND execution_id='X-2'`)
	f.refused(t, "REWORK from OPEN", "23001", "cannot go from", `UPDATE cargo_phy.load_execution SET state='REWORK' WHERE tenant_id=$1 AND execution_id='X-2'`)
	f.refused(t, "REWORK with the close facts kept", "23514", "load_execution_closed", rework(1, "", "", ""),
		`UPDATE cargo_phy.load_execution SET state='REWORK' WHERE tenant_id=$1 AND execution_id='X-1'`)
	f.refused(t, "REWORK with another change of the row", "23001", "cannot go from CLOSED to REWORK", rework(1, "", "", ""),
		strings.Replace(breakSeal, "state='REWORK',", "state='REWORK', started_by='someone',", 1))
	f.refused(t, "a rework only without the execution's change", "23514", "stuffing_rework_agrees", rework(1, "", "", ""))
	f.refused(t, "a rework on a stuffing that is not sealed", "23514", "stuffing_rework_state",
		strings.Replace(rework(1, "", "", ""), "execution_id='X-1'", "execution_id='X-2'", 1))
	f.refused(t, "a rework that breaks another seal", "23514", "stuffing_rework_broken_seal", rework(1, "", "", "'SEAL-OTHER'"))
	f.refused(t, "a rework numbered after none", "23514", "stuffing_rework_number", rework(2, "", "", ""))
	f.refused(t, "a rework asked before the seal", "23514", "stuffing_rework_times", rework(1, "", "closed_at - interval '1 second'", ""))
	f.refused(t, "a rework with no reason", "23514", "stuffing_rework_reason_check", rework(1, "' '", "", ""))
	f.refused(t, "a rework with a reason of 501 characters", "23514", "stuffing_rework_reason_check", rework(1, "repeat('r',501)", "", ""))
	f.refused(t, "two reworks open at once", "23505", "stuffing_rework_one_open", rework(1, "", "", ""), rework(2, "", "", ""))
	f.refused(t, "REWORK of a sealed stuffing whose rework names another execution's seal", "23514", "stuffing_rework_broken_seal", rework(1, "", "", "'X'"))
	f.refused(t, "a reversal of a sealed container", "23514", "reversal_line_sealed", reversal(1, 2, 3, "OFFLOADED_BY_FORWARDER", "now()", "now()"))
	f.refused(t, "a reversal of a sealed container for the reason of a rework", "23514", "nor after the tender but packs taken back", reversal(1, 2, 6, "UNSTUFFED", "now()", "now()"))
	f.refused(t, "a lot opened again by nothing", "23001", "cannot go from CLOSED to RECEIVED", fmt.Sprintf(lotClosed, "L-H03"))
	f.refused(t, "a reopen of a lot while the container is sealed", "23514", "cargo_lot_reopen_rework", fmt.Sprintf(reopenRow, 1, 1, 1, "L-H03"))
	f.refused(t, "the consolidation reopened with a sealed container", "23514", "consolidation_reopen_before_tender", fmt.Sprintf(consolidationVersion, "REOPENED", "OPEN", "'x'"))
	f.refused(t, "a sealed stuffing given up", "23001", "cannot go from CLOSED to CANCELLED", `UPDATE cargo_phy.load_execution SET state='CANCELLED', closed_by=NULL, closed_at=NULL, closed_entered_at=NULL,
        close_evidence_source=NULL, close_evidence_reference=NULL, close_evidence_sha256=NULL, close_evidence_occurred_at=NULL, cancelled_by='x', cancelled_at=now(),
        cancelled_entered_at=now(), cancel_reason='x' WHERE tenant_id=$1 AND execution_id='X-1'`)
	f.refused(t, "a sealed stuffing back to LOADING", "23001", "cannot go from CLOSED to LOADING", `UPDATE cargo_phy.load_execution SET state='LOADING', closed_by=NULL, closed_at=NULL,
        closed_entered_at=NULL, close_evidence_source=NULL, close_evidence_reference=NULL, close_evidence_sha256=NULL, close_evidence_occurred_at=NULL WHERE tenant_id=$1 AND execution_id='X-1'`)
	f.refused(t, "a sealed stuffing's seal changed", "23001", "cannot go from CLOSED to CLOSED", `UPDATE cargo_phy.load_execution SET close_evidence_reference='SEAL-0' WHERE tenant_id=$1 AND execution_id='X-1'`)

	// the carrier's word: a rework is not opened on a container with carrier evidence, in the database either
	carrierDay := reworkDay(t)
	carrierDay.stuffedAndSealed(t)
	ok(t, 7)(carrierDay.module.RecordCarrierLoadEvidence(ctx, api.RecordCarrierLoadEvidence{CommandID: "carrier-1", TenantID: carrierDay.tenant, ActorID: "ops-1",
		ExecutionID: "X-1", ExpectedRevision: 6, Fact: api.CarrierDeparted, Pieces: 12, Transport: "MV-EVER 12", Evidence: evidence("carrier-1", carrierDay.hoursIn(6)),
		OccurredAt: carrierDay.hoursIn(6)}))
	carrierDay.refused(t, "REWORK with carrier evidence", "23514", "load_execution_rework_carrier", rework(1, "", "", ""), breakSeal)

	// the command opens the rework
	ok(t, 7)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-1", "X-1", 6, "H03 withdrawn", "ops-asker", f.hoursIn(6))))
	for name, state := range map[string]string{"OPEN": "OPEN", "LOADING": "LOADING", "ON_HOLD": "ON_HOLD"} {
		f.refused(t, "REWORK back to "+name, "23001", "cannot go from REWORK to "+state, fmt.Sprintf(`UPDATE cargo_phy.load_execution SET state='%s' WHERE tenant_id=$1 AND execution_id='X-1'`, state))
	}
	f.refused(t, "REWORK changing another column", "23001", "cannot go from REWORK to REWORK", `UPDATE cargo_phy.load_execution SET started_by='someone' WHERE tenant_id=$1 AND execution_id='X-1'`)
	f.refused(t, "REWORK given up with live packs", "23514", "load_execution_cancelled_packs", `UPDATE cargo_phy.load_execution SET state='CANCELLED', cancelled_by='x', cancelled_at=now(),
        cancelled_entered_at=now(), cancel_reason='x' WHERE tenant_id=$1 AND execution_id='X-1'`)
	f.refused(t, "sealed again with the broken seal", "23514", "load_execution_reseal_new", fmt.Sprintf(sealAgain, "now()", "SEAL-1", "now()"))
	// SEA compares seals by their letters and digits in capitals: each of these is the seal that was broken, put back
	for _, ref := range []string{"seal-1", "Seal-1", "SEAL 1", "SEAL1", "SEAL_1", "SEAL-1\u0345", "SEAL\u0301-1", "SEAL-1\u0364"} {
		f.refused(t, "sealed again with "+ref, "23514", "load_execution_reseal_new", fmt.Sprintf(sealAgain, "now()", ref, "now()"))
	}
	for _, ref := range []string{" SEAL-9", "SEAL-9 ", "\u00a0SEAL-9", "SEAL-9\u00a0", "SEAL-9\u3000", "\u0085SEAL-9", "SEAL-9\u2003", "\u1680SEAL-9", "SEAL-9\u202f", "SEAL-9\t", "SEAL-9\n"} {
		f.refused(t, fmt.Sprintf("sealed again with %q", ref), "23514", "load_execution_seal_blank", fmt.Sprintf(sealAgain, "now()", ref, "now()"))
	}
	f.refused(t, "a first seal with a blank at an end", "23514", "load_execution_seal_blank", `UPDATE cargo_phy.load_execution SET state='CLOSED', closed_by='raw',
        closed_at=now(), closed_entered_at=now(), close_evidence_source='SEAL', close_evidence_reference='SEAL-9 ', close_evidence_occurred_at=now()
        WHERE tenant_id=$1 AND execution_id='X-2'`)
	f.refused(t, "sealed again before the consolidation's latest version", "23514", "load_execution_seal_frozen",
		fmt.Sprintf(sealAgain, "(SELECT max(written_at) - interval '1 second' FROM cargo_phy.consolidation_version WHERE tenant_id=$1 AND consolidation_id='C-S')", "SEAL-7", "now()"))
	f.refused(t, "sealed again with something that is not a seal", "23514", "load_execution_seal", `UPDATE cargo_phy.load_execution SET state='CLOSED', closed_by='raw', closed_at=now(),
        closed_entered_at=now(), close_evidence_source='CFS', close_evidence_reference='SEAL-7', close_evidence_occurred_at=now() WHERE tenant_id=$1 AND execution_id='X-1'`)
	f.refused(t, "sealed again without the rework ending", "23514", "stuffing_rework_agrees", fmt.Sprintf(sealAgain, "now()", "SEAL-7", "now()"))
	f.refused(t, "a rework ended with no execution change", "23514", "stuffing_rework_agrees", `UPDATE cargo_phy.stuffing_rework SET ended_kind='SEALED', ended_by='raw',
        ended_at=now(), ended_entered_at=now(), new_seal_reference='SEAL-7' WHERE tenant_id=$1 AND execution_id='X-1' AND rework_no=1`)
	f.refused(t, "a rework ended sealed with the broken seal", "23514", "stuffing_rework_new_seal", `UPDATE cargo_phy.stuffing_rework SET ended_kind='SEALED', ended_by='raw',
        ended_at=now(), ended_entered_at=now(), new_seal_reference='SEAL-1' WHERE tenant_id=$1 AND execution_id='X-1' AND rework_no=1`)
	f.refused(t, "a rework ended sealed with no seal", "23514", "stuffing_rework_ended", `UPDATE cargo_phy.stuffing_rework SET ended_kind='SEALED', ended_by='raw',
        ended_at=now(), ended_entered_at=now() WHERE tenant_id=$1 AND execution_id='X-1' AND rework_no=1`)
	f.refused(t, "a rework ended before it was asked", "23514", "stuffing_rework_ended", `UPDATE cargo_phy.stuffing_rework SET ended_kind='GIVEN_UP', ended_by='raw',
        ended_at=asked_at - interval '1 second', ended_entered_at=now() WHERE tenant_id=$1 AND execution_id='X-1' AND rework_no=1`)
	f.refused(t, "a rework's opening changed", "42501", "", `UPDATE cargo_phy.stuffing_rework SET reason='other' WHERE tenant_id=$1 AND execution_id='X-1' AND rework_no=1`)
	f.refused(t, "a rework removed", "42501", "", `DELETE FROM cargo_phy.stuffing_rework WHERE tenant_id=$1 AND execution_id='X-1'`)
	f.refused(t, "a second rework while one is open", "23514", "stuffing_rework_state", rework(2, "", "", ""))

	// in REWORK a line, a hold, carrier evidence and a discrepancy are not accepted, and a reversal only whole and
	// UNSTUFFED
	f.refused(t, "a line in REWORK", "23514", "takes no load_line while REWORK", `INSERT INTO cargo_phy.load_line (tenant_id,execution_id,line_no,consolidation_id,shipment_id,lot_id,
        entry_no,packs,gross_weight_unit,gross_weight,basis,evidence_source,evidence_reference,evidence_occurred_at,loaded_by,loaded_at,entered_at)
        SELECT tenant_id,execution_id,9,consolidation_id,shipment_id,lot_id,entry_no,1,gross_weight_unit,1,'MEASURED',evidence_source,'x',evidence_occurred_at,loaded_by,loaded_at,now()
        FROM cargo_phy.load_line WHERE tenant_id=$1 AND execution_id='X-1' AND line_no=2`)
	f.refused(t, "a hold in REWORK", "23514", "takes no hold_entry while REWORK", `INSERT INTO cargo_phy.hold_entry (tenant_id,execution_id,hold_no,reason,evidence_source,evidence_reference,
        evidence_occurred_at,placed_by,placed_at,entered_at) VALUES ($1,'X-1',1,'DAMAGE','x','x',now(),'x',now(),now())`)
	f.refused(t, "carrier evidence in REWORK", "23514", "takes no carrier_load_evidence while REWORK", `INSERT INTO cargo_phy.carrier_load_evidence (tenant_id,execution_id,evidence_no,fact,pieces,
        transport_ref,evidence_source,evidence_reference,evidence_occurred_at,recorded_by,recorded_at,entered_at) VALUES ($1,'X-1',1,'DEPARTED',1,'MV 1','x','x',now(),'x',now(),now())`)
	f.refused(t, "a discrepancy in REWORK", "23514", "takes no loading_discrepancy while REWORK", `INSERT INTO cargo_phy.loading_discrepancy (tenant_id,execution_id,discrepancy_no,kind,evidence_no,
        expected_pieces,observed_pieces,raised_at,entered_at) VALUES ($1,'X-1',1,'OFFLOADED',1,1,1,now(),now())`)
	f.refused(t, "part of a line reversed in REWORK", "23514", "reversal_line_rework", reversal(1, 2, 3, "UNSTUFFED", "now()", "now()"))
	f.refused(t, "a line reversed in REWORK for another reason", "23514", "reversal_line_rework", reversal(1, 2, 6, "MISLOADED", "now()", "now()"))
	f.refused(t, "a line of a closed lot reversed in REWORK", "23514", "closed lot", reversal(1, 3, 2, "UNSTUFFED", "now()", "now()"))
	f.refused(t, "the consolidation reopened with a container in rework", "23514", "consolidation_reopen_during_rework", fmt.Sprintf(consolidationVersion, "REOPENED", "OPEN", "'x'"))
	f.refused(t, "the consolidation reconciled with a container in rework", "23514", "consolidation_executions_closed", fmt.Sprintf(consolidationVersion, "RECONCILED", "RECONCILED", "NULL"))
	f.refused(t, "a second stuffing of a unit whose stuffing is in rework", "23505", "load_execution_one_per_unit", `INSERT INTO cargo_phy.load_execution (tenant_id,execution_id,consolidation_id,kind,
        transport_mode,facility_location,facility_code,load_unit_no,state,opened_by,opened_at,opened_entered_at)
        VALUES ($1,'X-DUP','C-S','STUFFING','SEA','CNSHG','CFS-1',1,'OPEN','x',now(),now())`)
	if _, err := f.module.RemovePlannedLoadUnit(ctx, api.RemovePlannedLoadUnit{CommandID: "remove-1", TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: "C-S",
		ExpectedRevision: 4, UnitNo: 1, Reason: "x", OccurredAt: f.hoursIn(7)}); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "PHY_LOAD_UNIT_IN_USE") {
		t.Fatalf("the unit of a stuffing in rework is not removed: %v", err)
	}
	f.refused(t, "the unit of a stuffing in rework removed in the database", "23514", "planned_load_unit_in_use", `UPDATE cargo_phy.planned_load_unit SET status='REMOVED',
        remove_reason='x', removed_by='x', removed_at=now(), remove_entered_at=now() WHERE tenant_id=$1 AND consolidation_id='C-S' AND unit_no=1`)

	// X-1's H03 is unloaded by the command: reversal 1 is written, and the lot of H03 is open again
	ok(t, 8)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("unload-h03", "X-1", 7, "H03", f.hoursIn(7))))

	// the lot opened again: only with a reopen row and a reversal of its packs in the same transaction, for the
	// stuffing in that rework. L-H01 is closed and its house is in the container: reversal 1 is H03's, from another
	// transaction
	reopen := func(extra ...string) []string {
		return append([]string{fmt.Sprintf(reopenRow, 1, 1, 1, "L-H01")}, extra...)
	}
	open01 := fmt.Sprintf(lotClosed, "L-H01")
	f.refused(t, "a reopen row with no change of the lot", "23514", "cargo_lot_reopen_reversal", reopen()...)
	f.refused(t, "a lot opened again on another lot's reversal from another transaction", "23514", "cargo_lot_reopen_reversal", reopen(open01)...)
	f.refused(t, "a lot opened again with no reopen row", "23001", "cannot go from CLOSED to RECEIVED", open01)
	f.refused(t, "a reopen row naming the wrong closing", "23514", "cargo_lot_reopen_closed", strings.Replace(fmt.Sprintf(reopenRow, 1, 1, 1, "L-H01"),
		"SELECT tenant_id,lot_id,1,'X-1',1,1,closed_by,", "SELECT tenant_id,lot_id,1,'X-1',1,1,'someone-else',", 1))
	f.refused(t, "a reopen row of a lot that is not closed", "23514", "cargo_lot_reopen_closed", fmt.Sprintf(reopenRow, 1, 1, 1, "L-H02"))
	f.refused(t, "a reopen row numbered after none", "23514", "cargo_lot_reopen_number", fmt.Sprintf(reopenRow, 2, 1, 1, "L-H01"))
	f.refused(t, "a closed lot changed in another column with its reopen row", "23001", "cannot go from CLOSED to RECEIVED",
		reopen(strings.Replace(open01, "closed_by=NULL,", "closed_by=NULL, expected_packs=99,", 1))...)
	f.refused(t, "a closed lot opened again into the wrong state", "23001", "cannot go from CLOSED to RECEIVING",
		reopen(strings.Replace(open01, "state='RECEIVED'", "state='RECEIVING'", 1))...)
	f.refused(t, "a closed lot opened again keeping its close facts", "23001", "cannot go from CLOSED to RECEIVED",
		reopen(`UPDATE cargo_phy.cargo_lot SET state='RECEIVED' WHERE tenant_id=$1 AND lot_id='L-H01'`)...)
	f.refused(t, "a closed lot's close facts changed", "23001", "cannot go from CLOSED to CLOSED",
		`UPDATE cargo_phy.cargo_lot SET closed_by='other' WHERE tenant_id=$1 AND lot_id='L-H01'`)
	f.refused(t, "a reopen row changed", "42501", "", `UPDATE cargo_phy.cargo_lot_reopen SET reopened_by='x' WHERE tenant_id=$1`)
	// a lot opened again on an earlier transaction's reversal of its own packs: L-H03 was opened again by the command
	// and is closed again by a raw write (the domain closes such a lot only for a cancelled shipment), and its
	// reopening row names reversal 1, which is H03's, from the command's transaction. Only "written in the same
	// transaction" refuses it
	closeH03 := `UPDATE cargo_phy.cargo_lot l SET state='CLOSED', closed_by=o.closed_by, closed_at=o.closed_at, closed_entered_at=o.closed_entered_at
        FROM cargo_phy.cargo_lot_reopen o WHERE l.tenant_id=$1 AND l.lot_id='L-H03' AND o.tenant_id=l.tenant_id AND o.lot_id=l.lot_id AND o.reopen_no=1`
	if err := f.rawTx(t, closeH03); err != nil {
		t.Fatalf("a lot closed again by a raw write: %v", err)
	}
	f.refused(t, "a lot closed again with the facts of its earlier reopening, opened again with no row of this transaction", "23001", "cannot go from CLOSED to RECEIVED",
		fmt.Sprintf(lotClosed, "L-H03"))
	f.refused(t, "a lot opened again on another lot's reversal of this transaction", "23514", "cargo_lot_reopen_reversal", fmt.Sprintf(reopenRow, 1, 1, 2, "L-H01"),
		fmt.Sprintf(lotClosed, "L-H01"), reversal(2, 2, 6, "UNSTUFFED", fmt.Sprintf("'%s'::timestamptz", f.hoursIn(7).Add(time.Minute).Format(time.RFC3339Nano)),
			fmt.Sprintf("'%s'::timestamptz", f.hoursIn(7).Add(time.Minute).Format(time.RFC3339Nano))))
	f.refused(t, "a reopening dated before the rework was asked", "23514", "cargo_lot_reopen_rework",
		strings.Replace(fmt.Sprintf(reopenRow, 1, 1, 1, "L-H01"), "interval '3 hours'", "interval '0 hours'", 1))
	f.refused(t, "a lot opened again on an earlier transaction's reversal of its own packs", "23514", "cargo_lot_reopen_reversal",
		fmt.Sprintf(reopenRow, 2, 1, 1, "L-H03"), fmt.Sprintf(lotClosed, "L-H03"))
	// a reopen row whose lot is closed again before commit: the reversal and the reopening are there, the lot is CLOSED
	s13 := fmt.Sprintf("'%s'::timestamptz", f.hoursIn(7).Add(time.Minute).Format(time.RFC3339Nano))
	f.refused(t, "a reopened lot that is closed again in the same transaction", "23514", "cargo_lot_reopen_reversal", fmt.Sprintf(reopenRow, 1, 1, 2, "L-H01"),
		fmt.Sprintf(lotClosed, "L-H01"), reversal(2, 1, 4, "UNSTUFFED", s13, s13), `UPDATE cargo_phy.cargo_lot SET state='CLOSED', closed_by='raw', closed_at=completed_at + interval '2 hours',
        closed_entered_at=now() WHERE tenant_id=$1 AND lot_id='L-H01'`)
	// the same reopening, whole, is accepted: a reopen row, the lot opened again and the reversal of its packs, in one
	// transaction
	t7 := fmt.Sprintf("'%s'::timestamptz", f.hoursIn(7).Add(time.Minute).Format(time.RFC3339Nano))
	if err := f.rawTx(t, fmt.Sprintf(reopenRow, 1, 1, 2, "L-H01"), open01, reversal(2, 1, 4, "UNSTUFFED", t7, t7)); err != nil {
		t.Fatalf("a whole reopening written as the command writes it: %v", err)
	}
	if l := f.lot(t, "L-H01"); l.State != "RECEIVED" || len(l.Reopens) != 1 || l.Reopens[0].ReworkNo != 1 || l.Reopens[0].ReversalNo != 2 || l.Reopens[0].ReopenedBy != "raw" {
		t.Fatalf("L-H01 after the raw reopening: %+v", l)
	}

	// the rework and the execution agree at commit, whichever state the execution is in: the seal, who sealed it and
	// when
	atEnd := `(SELECT asked_at + interval '1 minute' FROM cargo_phy.stuffing_rework WHERE tenant_id=$1 AND execution_id='X-1' AND rework_no=1)`
	ending := func(by, ref, ended string) string {
		return `UPDATE cargo_phy.stuffing_rework SET ended_kind='SEALED', ended_by='` + by + `', ended_at=` + ended + `, ended_entered_at=now(), new_seal_reference='` + ref +
			`' WHERE tenant_id=$1 AND execution_id='X-1' AND rework_no=1`
	}
	f.refused(t, "sealed again by one and the rework says another", "23514", "stuffing_rework_agrees", fmt.Sprintf(sealAgain, atEnd, "SEAL-7", atEnd), ending("someone-else", "SEAL-7", atEnd))
	f.refused(t, "sealed again with a seal and the rework says another", "23514", "stuffing_rework_agrees", fmt.Sprintf(sealAgain, atEnd, "SEAL-7", atEnd), ending("raw-sealer", "SEAL-8", atEnd))
	f.refused(t, "sealed again at a time and the rework says another", "23514", "stuffing_rework_agrees", fmt.Sprintf(sealAgain, atEnd, "SEAL-7", atEnd),
		ending("raw-sealer", "SEAL-7", atEnd+" + interval '1 second'"))
	// what the database accepts and the domain would never write: a seal dated before the unloadings it follows (the
	// database checks a seal against the consolidation's versions and the rework it follows, not against the
	// execution's own lines), by a person nothing ties to the one who asked, in a transaction of its own
	at := `(SELECT asked_at + interval '1 minute' FROM cargo_phy.stuffing_rework WHERE tenant_id=$1 AND execution_id='X-1' AND rework_no=1)`
	if err := f.rawTx(t, fmt.Sprintf(sealAgain, at, "SEAL-7", at), `UPDATE cargo_phy.stuffing_rework SET ended_kind='SEALED', ended_by='raw-sealer', ended_at=`+at+`,
        ended_entered_at=now(), new_seal_reference='SEAL-7' WHERE tenant_id=$1 AND execution_id='X-1' AND rework_no=1`); err != nil {
		t.Fatalf("a seal dated before the unloadings is the domain's rule, not the database's: %v", err)
	}
	x := f.execution(t, "X-1")
	if x.State != api.ExecutionClosed || x.CloseEvidence.Reference != "SEAL-7" || x.Reworks[0].End != api.ReworkSealed || !x.ClosedAt.Before(x.Reversals[0].ReversedAt) {
		t.Fatalf("X-1 sealed again by raw writes: %+v", x)
	}
	f.refused(t, "a rework ended twice", "23001", "is ended once", `UPDATE cargo_phy.stuffing_rework SET ended_kind='GIVEN_UP', ended_by='x', ended_at=now(),
        ended_entered_at=now(), new_seal_reference=NULL WHERE tenant_id=$1 AND execution_id='X-1' AND rework_no=1`)

	// a second rework and its seals: the new seal is neither of the seals carried before, in the database
	ok(t, x.Revision+1)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-2", "X-1", x.Revision, "again", "ops-asker-2", f.hoursIn(12))))
	for _, ref := range []string{"SEAL-1", "SEAL-7", "seal 1", "Seal7"} {
		f.refused(t, "rework 2 ended sealed with "+ref, "23514", "stuffing_rework_new_seal", fmt.Sprintf(`UPDATE cargo_phy.stuffing_rework SET ended_kind='SEALED',
            ended_by='raw', ended_at=asked_at + interval '1 hour', ended_entered_at=now(), new_seal_reference='%s' WHERE tenant_id=$1 AND execution_id='X-1' AND rework_no=2`, ref))
		f.refused(t, "X-1 sealed again with "+ref, "23514", "load_execution_reseal_new", fmt.Sprintf(sealAgain, "now()", ref, "now()"))
	}
	// emptied by the command, the container is sealed again by nobody: the database wants cargo in it
	rev := f.execution(t, "X-1").Revision
	ok(t, rev+1)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("unload-h02", "X-1", rev, "H02", f.hoursIn(13))))
	f.refused(t, "X-1 sealed with nothing in it", "23514", "load_execution_reseal_cargo", fmt.Sprintf(sealAgain, "now()", "SEAL-8", "now()"))
	// given up by one and the rework says another, or at another time
	giveUp := `UPDATE cargo_phy.load_execution SET state='CANCELLED', cancelled_by='raw-canceller', cancelled_at=(SELECT asked_at + interval '2 hours' FROM cargo_phy.stuffing_rework
        WHERE tenant_id=$1 AND execution_id='X-1' AND rework_no=2), cancelled_entered_at=now(), cancel_reason='x' WHERE tenant_id=$1 AND execution_id='X-1'`
	endGiveUp := func(by, at string) string {
		return `UPDATE cargo_phy.stuffing_rework SET ended_kind='GIVEN_UP', ended_by='` + by + `', ended_at=` + at + `, ended_entered_at=now() WHERE tenant_id=$1 AND execution_id='X-1' AND rework_no=2`
	}
	two := `(SELECT asked_at + interval '2 hours' FROM cargo_phy.stuffing_rework WHERE tenant_id=$1 AND execution_id='X-1' AND rework_no=2)`
	f.refused(t, "given up by one and the rework says another", "23514", "stuffing_rework_agrees", giveUp, endGiveUp("someone-else", two))
	f.refused(t, "given up at a time and the rework says another", "23514", "stuffing_rework_agrees", giveUp, endGiveUp("raw-canceller", two+" + interval '1 second'"))
	if err := f.rawTx(t, giveUp, endGiveUp("raw-canceller", two)); err != nil {
		t.Fatalf("given up as the rework says: %v", err)
	}
}

// TestReworkChecksOfTheMigrator tries the rules a CHECK keeps with the triggers off, and the rows nothing may change,
// as the owner of the tables.
func TestReworkChecksOfTheMigrator(t *testing.T) {
	ctx := context.Background()
	f := reworkDay(t)
	f.stuffedAndSealed(t)
	f.begin(t, "X-2", 2)
	ok(t, 7)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-1", "X-1", 6, "H03 withdrawn", "ops-asker", f.hoursIn(6))))
	ok(t, 8)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("unload-h03", "X-1", 7, "H03", f.hoursIn(7))))
	m := f.migrator(t)
	if m == nil {
		t.Skip("set IDP_CARGO_MIGRATOR_DSN")
	}
	for name, tc := range map[string]struct{ sql, text string }{
		"a rework's opening changed":         {`UPDATE cargo_phy.stuffing_rework SET reason='other' WHERE tenant_id=$1 AND execution_id='X-1' AND rework_no=1`, "keeps its opening"},
		"a rework removed":                   {`DELETE FROM cargo_phy.stuffing_rework WHERE tenant_id=$1 AND execution_id='X-1'`, "rows are immutable"},
		"a rework updated with no ending":    {`UPDATE cargo_phy.stuffing_rework SET reason=reason WHERE tenant_id=$1 AND execution_id='X-1' AND rework_no=1`, "is ended once"},
		"a reopen row removed":               {`DELETE FROM cargo_phy.cargo_lot_reopen WHERE tenant_id=$1`, "rows are immutable"},
		"an execution with a rework removed": {`DELETE FROM cargo_phy.load_execution WHERE tenant_id=$1 AND execution_id='X-1'`, "cannot be deleted"},
	} {
		if _, err := m.Exec(ctx, tc.sql, f.tenant); sqlState(err) != "23001" || !strings.Contains(err.Error(), tc.text) {
			t.Errorf("%s: %v, want 23001 %q", name, err, tc.text)
		}
	}
	// the migrator reaches the CHECK constraints only with the triggers off, in a transaction that is rolled back
	check := func(what, sql, constraint string) {
		t.Helper()
		tx, err := m.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		for _, trigger := range []string{"load_execution_rules", "load_execution_rework_agrees"} {
			if _, err = tx.Exec(ctx, `ALTER TABLE cargo_phy.load_execution DISABLE TRIGGER `+trigger); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = tx.Exec(ctx, sql, f.tenant); sqlState(err) != "23514" || !strings.Contains(err.Error(), constraint) {
			t.Errorf("%s: %v, want 23514 %s", what, err, constraint)
		}
	}
	check("a state outside the list", `UPDATE cargo_phy.load_execution SET state='UNSEALED' WHERE tenant_id=$1 AND execution_id='X-2'`, "load_execution_state_check")
	check("a REWORK stuffing that never started", `UPDATE cargo_phy.load_execution SET state='REWORK', started_by=NULL, started_at=NULL, started_entered_at=NULL
        WHERE tenant_id=$1 AND execution_id='X-2'`, "load_execution_rework")
	check("REWORK with a seal's close facts", `UPDATE cargo_phy.load_execution SET state='REWORK', closed_by='x', closed_at=now(), closed_entered_at=now(),
        close_evidence_source='SEAL', close_evidence_reference='S', close_evidence_occurred_at=now() WHERE tenant_id=$1 AND execution_id='X-2'`, "load_execution_closed")
}

// ---- the commands' refusals ----

func (f *fixture) refusedCommand(t *testing.T, what string, err error, kind error, text string) {
	t.Helper()
	if !errors.Is(err, kind) || (text != "" && !strings.Contains(err.Error(), text)) {
		t.Errorf("%s: %v, want %v containing %q", what, err, kind, text)
	}
}

// TestReworkRefusalsThroughTheCommands tries each rule of the three commands and of the reseal and give-up from REWORK
// with the real commands, and that nothing else is accepted in REWORK. Every refusal changes nothing: no revision, no
// event.
func TestReworkRefusalsThroughTheCommands(t *testing.T) {
	ctx := context.Background()
	f := reworkDay(t)
	f.stuffedAndSealed(t)
	f.begin(t, "X-2", 2) // a stuffing that is loading, to be refused
	events := len(f.types(t))
	at := f.hoursIn(6)
	r := func(command string, rev int64) api.OpenStuffingForRework {
		return f.openRework(command, "X-1", rev, "H03 withdrawn", "ops-asker", at)
	}

	// opening
	_, err := f.module.OpenStuffingForRework(ctx, r("o-stale", 5))
	f.refusedCommand(t, "a stale revision", err, api.ErrConflict, "")
	cmd := r("o-blank", 6)
	cmd.Reason = " "
	_, err = f.module.OpenStuffingForRework(ctx, cmd)
	f.refusedCommand(t, "no reason", err, api.ErrRejected, "needs a reason")
	cmd.CommandID, cmd.Reason = "o-long", strings.Repeat("r", 501)
	_, err = f.module.OpenStuffingForRework(ctx, cmd)
	f.refusedCommand(t, "a reason of 501 characters", err, api.ErrRejected, "needs a reason")
	cmd = r("o-early", 6)
	cmd.OccurredAt = f.hoursIn(3)
	_, err = f.module.OpenStuffingForRework(ctx, cmd)
	f.refusedCommand(t, "before the seal", err, api.ErrRejected, "not before the execution's last change")
	_, err = f.module.OpenStuffingForRework(ctx, f.openRework("o-loading", "X-2", 2, "x", "ops-asker", at))
	f.refusedCommand(t, "a stuffing that is loading", err, api.ErrRejected, "this one is LOADING")
	_, err = f.module.OpenStuffingForRework(ctx, f.openRework("o-none", "X-NONE", 1, "x", "ops-asker", at))
	f.refusedCommand(t, "an execution nobody opened", err, api.ErrNotFound, "")
	cmd = r("o-intruder", 6)
	cmd.ActorID = "intruder"
	_, err = f.module.OpenStuffingForRework(ctx, cmd)
	f.refusedCommand(t, "an actor the policy forbids", err, api.ErrUnauthorized, "")
	cmd = r("o-late", 6)
	cmd.OccurredAt = time.Now().Add(-100 * 24 * time.Hour)
	_, err = f.module.OpenStuffingForRework(ctx, cmd)
	f.refusedCommand(t, "a time outside the record window", err, api.ErrRejected, "")
	// unloading and the ordinary unload of a sealed container
	_, err = f.module.UnloadHouseForRework(ctx, f.unloadHouse("u-sealed", "X-1", 6, "H03", f.hoursIn(7)))
	f.refusedCommand(t, "a house unloaded from a sealed container", err, api.ErrRejected, "this one is CLOSED")
	_, err = f.module.RecordCargoUnloaded(ctx, api.RecordCargoUnloaded{CommandID: "ord-sealed", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-1", ExpectedRevision: 6,
		LineNo: 3, Packs: 2, Reason: api.ReversalUnstuffed, Evidence: evidence("ord", f.hoursIn(7)), OccurredAt: f.hoursIn(7)})
	f.refusedCommand(t, "an ordinary unload of a sealed container", err, api.ErrRejected, "opened for rework first")
	if n := len(f.types(t)); n != events || f.execution(t, "X-1").Revision != 6 {
		t.Fatalf("refusals changed something: %d events, revision %d", n, f.execution(t, "X-1").Revision)
	}

	ok(t, 7)(f.module.OpenStuffingForRework(ctx, r("o-1", 6)))
	events = len(f.types(t))
	_, err = f.module.OpenStuffingForRework(ctx, r("o-again", 7))
	f.refusedCommand(t, "a container already in rework", err, api.ErrRejected, "this one is REWORK")

	// nothing else is accepted in REWORK
	_, err = f.module.RecordCargoLoaded(ctx, f.load("rw-load", "X-1", 7, "L-H02", 1, f.hoursIn(7)))
	f.refusedCommand(t, "a load", err, api.ErrRejected, "takes no load")
	_, err = f.module.StartLoading(ctx, api.StartLoading{CommandID: "rw-start", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-1", ExpectedRevision: 7, OccurredAt: f.hoursIn(7)})
	f.refusedCommand(t, "a start", err, api.ErrRejected, "is not started")
	_, err = f.module.PlaceLoadExecutionOnHold(ctx, api.PlaceLoadExecutionOnHold{CommandID: "rw-hold", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-1", ExpectedRevision: 7,
		Reason: api.HoldCustoms, Evidence: evidence("hold", f.hoursIn(7)), OccurredAt: f.hoursIn(7)})
	f.refusedCommand(t, "a hold", err, api.ErrRejected, "takes no hold")
	_, err = f.module.ReleaseLoadExecutionHold(ctx, api.ReleaseLoadExecutionHold{CommandID: "rw-release", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-1", ExpectedRevision: 7,
		HoldNo: 1, Evidence: evidence("rel", f.hoursIn(7)), OccurredAt: f.hoursIn(7)})
	f.refusedCommand(t, "a release", err, api.ErrRejected, "no hold to release")
	_, err = f.module.RecordCarrierLoadEvidence(ctx, api.RecordCarrierLoadEvidence{CommandID: "rw-carrier", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-1", ExpectedRevision: 7,
		Fact: api.CarrierDeparted, Pieces: 12, Transport: "MV-EVER 12", Evidence: evidence("carrier", f.hoursIn(7)), OccurredAt: f.hoursIn(7)})
	f.refusedCommand(t, "carrier evidence", err, api.ErrRejected, "takes no carrier evidence")
	_, err = f.module.ResolveLoadingDiscrepancy(ctx, api.ResolveLoadingDiscrepancy{CommandID: "rw-resolve", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-1", ExpectedRevision: 7,
		DiscrepancyNo: 1, Resolution: api.ResolvedByAccepted, Reason: "x", OccurredAt: f.hoursIn(7)})
	f.refusedCommand(t, "a discrepancy resolved", err, api.ErrRejected, "")
	_, err = f.module.RecordCargoUnloaded(ctx, api.RecordCargoUnloaded{CommandID: "rw-ord", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-1", ExpectedRevision: 7, LineNo: 2,
		Packs: 6, Reason: api.ReversalUnstuffed, Evidence: evidence("ord", f.hoursIn(7)), OccurredAt: f.hoursIn(7)})
	f.refusedCommand(t, "an ordinary unload in REWORK", err, api.ErrRejected, "by whole houses")
	_, err = f.module.CancelLoadExecution(ctx, f.cancelCmd("rw-cancel", "X-1", 7, "x", f.hoursIn(7)))
	f.refusedCommand(t, "a give-up with packs in the container", err, api.ErrRejected, "12 packs are still loaded")
	// the consolidation: not reopened, reconciled or cancelled
	_, err = f.module.ReopenConsolidation(ctx, api.ReopenConsolidation{CommandID: "rw-reopen", TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: "C-S", ExpectedRevision: 4,
		Reason: "a late house", OccurredAt: f.hoursIn(7)})
	f.refusedCommand(t, "a reopen of the consolidation", err, api.ErrRejected, "opened for rework")
	_, err = f.module.ReconcileConsolidation(ctx, api.ReconcileConsolidation{CommandID: "rw-reconcile", TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: "C-S", ExpectedRevision: 4,
		OccurredAt: f.hoursIn(7)})
	f.refusedCommand(t, "a reconcile of the consolidation", err, api.ErrRejected, "2 load executions not closed")
	_, err = f.module.CancelConsolidation(ctx, api.CancelConsolidation{CommandID: "rw-cancel-c", TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: "C-S", ExpectedRevision: 4,
		Reason: "x", OccurredAt: f.hoursIn(7)})
	f.refusedCommand(t, "a cancel of the consolidation", err, api.ErrRejected, "")
	// unloading a house
	_, err = f.module.UnloadHouseForRework(ctx, f.unloadHouse("u-stale", "X-1", 6, "H03", f.hoursIn(7)))
	f.refusedCommand(t, "a stale revision", err, api.ErrConflict, "")
	_, err = f.module.UnloadHouseForRework(ctx, f.unloadHouse("u-nobody", "X-1", 7, "NO_SUCH_HOUSE", f.hoursIn(7)))
	f.refusedCommand(t, "a house nobody attached", err, api.ErrRejected, "")
	bad := f.unloadHouse("u-noevidence", "X-1", 7, "H03", f.hoursIn(7))
	bad.Evidence = api.Evidence{}
	_, err = f.module.UnloadHouseForRework(ctx, bad)
	f.refusedCommand(t, "no evidence", err, api.ErrRejected, "evidence")
	_, err = f.module.UnloadHouseForRework(ctx, f.unloadHouse("u-early", "X-1", 7, "H03", f.hoursIn(5)))
	f.refusedCommand(t, "before the rework was asked", err, api.ErrRejected, "not before the execution's last change")
	bad = f.unloadHouse("u-intruder", "X-1", 7, "H03", f.hoursIn(7))
	bad.ActorID = "intruder"
	_, err = f.module.UnloadHouseForRework(ctx, bad)
	f.refusedCommand(t, "an actor the policy forbids", err, api.ErrUnauthorized, "")
	_, err = f.module.UnloadHouseForRework(ctx, f.unloadHouse("u-other", "X-2", 2, "H03", f.hoursIn(7)))
	f.refusedCommand(t, "a stuffing that is not in rework", err, api.ErrRejected, "this one is LOADING")
	// sealing again
	putBack := f.sealOf("X-1", 7, "SEAL-1")
	putBack.CommandID = "rw-put-back"
	_, err = f.module.CloseLoadExecution(ctx, putBack)
	f.refusedCommand(t, "the seal that was broken, put back at once", err, api.ErrRejected, "was broken in rework 1")
	tally := f.sealOf("X-1", 7, "SEAL-5")
	tally.CommandID, tally.Evidence.Source = "rw-tally", "TALLY"
	_, err = f.module.CloseLoadExecution(ctx, tally)
	f.refusedCommand(t, "an evidence that is not a seal", err, api.ErrRejected, "closes with the container's seal")
	if n := len(f.types(t)); n != events || f.execution(t, "X-1").Revision != 7 || len(f.execution(t, "X-1").Reversals) != 0 {
		t.Fatalf("refusals changed something: %d events (want %d), revision %d", n, events, f.execution(t, "X-1").Revision)
	}
	// what goes on as today: a stuffing of another unit is loaded, its lots are those of houses of the consolidation
	if _, err = f.module.RecordCargoLoaded(ctx, f.load("x2-load", "X-2", 2, "L-H03", 1, f.hoursIn(7))); err == nil {
		t.Fatal("L-H03 is closed and takes no load")
	}
	if got := f.lot(t, "L-H02"); got.LoadedPacks != 6 {
		t.Fatalf("L-H02: %+v", got)
	}
	// the container is sealed again once a house is out, and cannot be opened again with carrier evidence: depart it
	ok(t, 8)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("u-1", "X-1", 7, "H03", f.hoursIn(7))))
	if _, err = f.module.UnloadHouseForRework(ctx, f.unloadHouse("u-again", "X-1", 8, "H03", f.hoursIn(7))); !errors.Is(err, api.ErrRejected) ||
		!strings.Contains(err.Error(), "has no live packs in this container") {
		t.Fatalf("a house unloaded twice: %v", err)
	}
	reseal := f.sealOf("X-1", 8, "SEAL-2")
	reseal.OccurredAt, reseal.Evidence.OccurredAt = f.hoursIn(8), f.hoursIn(8)
	ok(t, 9)(f.module.CloseLoadExecution(ctx, reseal))
	carrier := api.RecordCarrierLoadEvidence{CommandID: "after-carrier", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-1", ExpectedRevision: 9, Fact: api.CarrierDeparted,
		Pieces: 10, Transport: "MV-EVER 12", Evidence: evidence("after-carrier", f.hoursIn(9)), OccurredAt: f.hoursIn(9)}
	ok(t, 10)(f.module.RecordCarrierLoadEvidence(ctx, carrier))
	_, err = f.module.OpenStuffingForRework(ctx, f.openRework("o-departed", "X-1", 10, "too late", "ops-asker", f.hoursIn(10)))
	f.refusedCommand(t, "a container with carrier evidence", err, api.ErrRejected, "carrier evidence is recorded on the container")
	carrier.CommandID, carrier.Fact, carrier.Pieces, carrier.Evidence, carrier.ExpectedRevision = "after-offload", api.CarrierOffloaded, 2, evidence("after-offload", f.hoursIn(9)), 10
	ok(t, 11)(f.module.RecordCarrierLoadEvidence(ctx, carrier))
	_, err = f.module.OpenStuffingForRework(ctx, f.openRework("o-offloaded", "X-1", 11, "too late", "ops-asker", f.hoursIn(10)))
	f.refusedCommand(t, "a container with a loading discrepancy", err, api.ErrRejected, "a loading discrepancy is open")
}

// TestAHouseInTwoContainersIsRefusedAndAnotherLotInForceBlocksTheReopening: H02 is split over two containers, 4 packs
// in X-1 and 2 in X-2: unloading it from X-1 is partial unloading and refused, in the command and, for a raw reversal,
// in the database at commit; H01 unloads. Then H03's closed lot meets a new lot of H03 received at the same facility:
// the unloading is refused for it, in the command and in the database.
func TestAHouseInTwoContainersIsRefusedAndAnotherLotInForceBlocksTheReopening(t *testing.T) {
	ctx := context.Background()
	f := reworkDay(t)
	f.begin(t, "X-1", 1)
	f.begin(t, "X-2", 2)
	at := f.hoursIn(4)
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("a-h01", "X-1", 2, "L-H01", 4, at)))
	ok(t, 4)(f.module.RecordCargoLoaded(ctx, f.load("a-h02", "X-1", 3, "L-H02", 4, at)))
	ok(t, 5)(f.module.RecordCargoLoaded(ctx, f.load("a-h03", "X-1", 4, "L-H03", 2, at)))
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("b-h02", "X-2", 2, "L-H02", 2, at)))
	ok(t, 6)(f.module.CloseLoadExecution(ctx, f.sealOf("X-1", 5, "SEAL-1")))
	ok(t, 4)(f.module.CloseCargoLot(ctx, api.CloseCargoLot{CommandID: "close-lot-h03", TenantID: f.tenant, ActorID: "ops-1", LotID: "L-H03", ExpectedRevision: 3, OccurredAt: f.hoursIn(5)}))
	ok(t, 7)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-1", "X-1", 6, "x", "ops-asker", f.hoursIn(6))))
	events := len(f.types(t))
	_, err := f.module.UnloadHouseForRework(ctx, f.unloadHouse("u-h02", "X-1", 7, "H02", f.hoursIn(7)))
	f.refusedCommand(t, "a house in two containers", err, api.ErrRejected, "unloading part of a house is not built")
	f.refused(t, "a house in two containers, written raw", "23514", "reversal_line_rework_house", reversal(1, 2, 4, "UNSTUFFED", "now()", "now()"))
	if len(f.types(t)) != events || len(f.execution(t, "X-1").Reversals) != 0 || f.lot(t, "L-H02").LoadedPacks != 6 {
		t.Fatal("the refused unloading changed something")
	}
	// a new lot of H03 at the CFS while its lot is closed: one lot is in force per house and facility
	rcv := f.receive("rcv-h03b", "L-H03B", "H03", 0, 1, api.ConditionGood, f.hoursIn(7))
	rcv.Facility = cfsShanghai
	ok(t, 1)(f.module.ReceiveCargo(ctx, rcv))
	_, err = f.module.UnloadHouseForRework(ctx, f.unloadHouse("u-h03", "X-1", 7, "H03", f.hoursIn(8)))
	f.refusedCommand(t, "a closed lot while the shipment has another in force", err, api.ErrRejected, "is in force at CNSHG CFS-1")
	f.refused(t, "the same in the database", "23505", "cargo_lot_one_live", fmt.Sprintf(reopenRow, 1, 1, 1, "L-H03"), fmt.Sprintf(lotClosed, "L-H03"))
	ok(t, 8)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("u-h01", "X-1", 7, "H01", f.hoursIn(8))))
	if l := f.lot(t, "L-H03"); l.State != "CLOSED" || len(l.Reopens) != 0 {
		t.Fatalf("L-H03 is closed still: %+v", l)
	}
}

// TestAConsolidationWithTheContainersReconciledTakesNoRework: houses wholly loaded, the consolidation reconciled: the
// container is not opened for rework (the database says so too).
func TestAConsolidationWithTheContainersReconciledTakesNoRework(t *testing.T) {
	ctx := context.Background()
	f := reworkDay(t)
	f.stuffedAndSealed(t)
	for _, h := range []string{"H02"} {
		ok(t, 4)(f.module.CloseCargoLot(ctx, api.CloseCargoLot{CommandID: "close-lot-" + h, TenantID: f.tenant, ActorID: "ops-1", LotID: "L-" + h, ExpectedRevision: 3, OccurredAt: f.hoursIn(5)}))
	}
	ok(t, 5)(f.module.ReconcileConsolidation(ctx, api.ReconcileConsolidation{CommandID: "reconcile", TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: "C-S", ExpectedRevision: 4,
		OccurredAt: f.hoursIn(6)}))
	_, err := f.module.OpenStuffingForRework(ctx, f.openRework("rework-late", "X-1", 6, "x", "ops-asker", f.hoursIn(7)))
	f.refusedCommand(t, "a consolidation that is reconciled", err, api.ErrRejected, "the consolidation is RECONCILED")
	f.refused(t, "REWORK on a reconciled consolidation in the database", "23514", "load_execution_rework_consolidation", rework(1, "", "", ""), breakSeal)
}

// ---- locks: commands queued behind a lock held on a second connection, in each order ----

// queue starts the commands one after the other, each only once the one before it is waiting behind the lock held by
// the backend pid, so that the order they wait in is the order they are given; the lock is then released and the
// commands' answers come back in the order they were given.
func (f *fixture) queue(t *testing.T, pid int, release func(), commands ...func() error) []error {
	t.Helper()
	results := make([]chan error, len(commands))
	for i, run := range commands {
		results[i] = make(chan error, 1)
		go func() { results[i] <- run() }()
		f.waitingBehind(t, pid, i+1)
	}
	release()
	out := make([]error, len(commands))
	for i := range commands {
		out[i] = <-results[i]
	}
	return out
}

// TestAReworkOpeningAndACarrierEvidenceWaitForEachOther: the execution's row is held on a second connection, and the
// opening of the rework and the recording of carrier evidence queue behind it in each order. Whichever comes second
// reads what the first committed: carrier evidence first, the container is not opened for rework; the opening first,
// the container takes no carrier evidence. Never a container in REWORK with carrier evidence recorded.
func TestAReworkOpeningAndACarrierEvidenceWaitForEachOther(t *testing.T) {
	ctx := context.Background()
	for _, carrierFirst := range []bool{true, false} {
		name := map[bool]string{true: "the carrier evidence first", false: "the opening first"}[carrierFirst]
		t.Run(name, func(t *testing.T) {
			f := reworkDay(t)
			f.stuffedAndSealed(t)
			f.withoutRetry(t)
			first, second := int64(6), int64(7)
			carrier := func(rev int64) func() error {
				return func() error {
					_, err := f.module.RecordCarrierLoadEvidence(ctx, api.RecordCarrierLoadEvidence{CommandID: fmt.Sprint("carrier-", rev), TenantID: f.tenant, ActorID: "ops-1",
						ExecutionID: "X-1", ExpectedRevision: rev, Fact: api.CarrierDeparted, Pieces: 12, Transport: "MV-EVER 12", Evidence: evidence("carrier", f.hoursIn(6)), OccurredAt: f.hoursIn(6)})
					return err
				}
			}
			open := func(rev int64) func() error {
				return func() error {
					_, err := f.module.OpenStuffingForRework(ctx, f.openRework(fmt.Sprint("open-", rev), "X-1", rev, "x", "ops-asker", f.hoursIn(6)))
					return err
				}
			}
			pid, release := f.holdRow(t, lockExecution, f.tenant, "X-1")
			var got []error
			if carrierFirst {
				got = f.queue(t, pid, release, carrier(first), open(second))
			} else {
				got = f.queue(t, pid, release, open(first), carrier(second))
			}
			x := f.execution(t, "X-1")
			if carrierFirst {
				f.refusedCommand(t, "the opening after the carrier evidence", got[1], api.ErrRejected, "carrier evidence is recorded on the container")
				if got[0] != nil || x.State != api.ExecutionClosed || len(x.CarrierEvidence) != 1 || len(x.Reworks) != 0 {
					t.Fatalf("carrier evidence first: %v, %+v", got[0], x)
				}
			} else {
				f.refusedCommand(t, "the carrier evidence after the opening", got[1], api.ErrRejected, "takes no carrier evidence")
				if got[0] != nil || x.State != api.ExecutionRework || len(x.CarrierEvidence) != 0 || len(x.Reworks) != 1 {
					t.Fatalf("opening first: %v, %+v", got[0], x)
				}
			}
			if x.State == api.ExecutionRework && len(x.CarrierEvidence) > 0 {
				t.Fatal("a container in rework with carrier evidence")
			}
		})
	}
}

// TestAnUnloadForReworkAndALoadOfTheSameHouseElsewhereWaitForEachOther: H02 has 4 of its 6 packs in X-1 (in rework) and
// the other 2 can be loaded into X-2. The house's ledger is held on a second connection and the unloading and the load
// queue behind it in each order: the unloading first, the house is whole in X-1 and leaves, and the load goes into X-2
// after it; the load first, the house is then in two containers and the unloading is refused, partial.
func TestAnUnloadForReworkAndALoadOfTheSameHouseElsewhereWaitForEachOther(t *testing.T) {
	ctx := context.Background()
	for _, unloadFirst := range []bool{true, false} {
		name := map[bool]string{true: "the unloading first", false: "the load first"}[unloadFirst]
		t.Run(name, func(t *testing.T) {
			f := reworkDay(t)
			f.withoutRetry(t)
			f.begin(t, "X-1", 1)
			f.begin(t, "X-2", 2)
			at := f.hoursIn(4)
			ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("a-h02", "X-1", 2, "L-H02", 4, at)))
			ok(t, 4)(f.module.CloseLoadExecution(ctx, f.sealOf("X-1", 3, "SEAL-1")))
			ok(t, 5)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-1", "X-1", 4, "x", "ops-asker", f.hoursIn(6))))
			unload := func() error {
				_, err := f.module.UnloadHouseForRework(ctx, f.unloadHouse("u-h02", "X-1", 5, "H02", f.hoursIn(7)))
				return err
			}
			load := func() error {
				_, err := f.module.RecordCargoLoaded(ctx, f.load("b-h02", "X-2", 2, "L-H02", 2, f.hoursIn(7)))
				return err
			}
			pid, release := f.holdRow(t, `SELECT 1 FROM cargo_phy.consol_membership WHERE tenant_id=$1 AND shipment_id=$2 AND stage='MAIN' FOR UPDATE`, f.tenant, "H02")
			var got []error
			if unloadFirst {
				got = f.queue(t, pid, release, unload, load)
			} else {
				got = f.queue(t, pid, release, load, unload)
			}
			x1, x2 := f.execution(t, "X-1"), f.execution(t, "X-2")
			if unloadFirst {
				if got[0] != nil || got[1] != nil || x1.TenderedPacks != 0 || x2.TenderedPacks != 2 || len(x1.Reversals) != 1 {
					t.Fatalf("the unloading first: %v %v, X-1 %d packs, X-2 %d packs", got[0], got[1], x1.TenderedPacks, x2.TenderedPacks)
				}
			} else {
				f.refusedCommand(t, "the unloading after the load", got[1], api.ErrRejected, "unloading part of a house is not built")
				if got[0] != nil || x1.TenderedPacks != 4 || x2.TenderedPacks != 2 || len(x1.Reversals) != 0 {
					t.Fatalf("the load first: %v, X-1 %d packs, X-2 %d packs", got[0], x1.TenderedPacks, x2.TenderedPacks)
				}
			}
		})
	}
}

// TestASealAgainAndAReopenOfTheConsolidationWaitForEachOther: the consolidation's row is held on a second connection
// and the reseal and the reopen queue behind it in each order. The reopen is refused either way, in the first order
// because a container is in rework and in the second because it is sealed (its cargo counts as tendered), and the
// container is sealed again either way.
func TestASealAgainAndAReopenOfTheConsolidationWaitForEachOther(t *testing.T) {
	ctx := context.Background()
	for _, resealFirst := range []bool{true, false} {
		name := map[bool]string{true: "the reseal first", false: "the reopen first"}[resealFirst]
		t.Run(name, func(t *testing.T) {
			f := reworkDay(t)
			f.stuffedAndSealed(t)
			ok(t, 7)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-1", "X-1", 6, "x", "ops-asker", f.hoursIn(6))))
			ok(t, 8)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("u-h03", "X-1", 7, "H03", f.hoursIn(7))))
			reseal := f.sealOf("X-1", 8, "SEAL-2")
			reseal.OccurredAt, reseal.Evidence.OccurredAt = f.hoursIn(8), f.hoursIn(8)
			resealCmd := func() error { _, err := f.module.CloseLoadExecution(ctx, reseal); return err }
			reopenCmd := func() error {
				_, err := f.module.ReopenConsolidation(ctx, api.ReopenConsolidation{CommandID: "reopen", TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: "C-S",
					ExpectedRevision: 4, Reason: "a late house", OccurredAt: f.hoursIn(9)})
				return err
			}
			pid, release := f.holdRow(t, `SELECT 1 FROM cargo_phy.consolidation WHERE tenant_id=$1 AND consolidation_id=$2 FOR UPDATE`, f.tenant, "C-S")
			var got []error
			if resealFirst {
				got = f.queue(t, pid, release, resealCmd, reopenCmd)
				f.refusedCommand(t, "the reopen after the reseal", got[1], api.ErrRejected, "10 packs tendered")
			} else {
				got = f.queue(t, pid, release, reopenCmd, resealCmd)
				f.refusedCommand(t, "the reopen before the reseal", got[0], api.ErrRejected, "opened for rework")
			}
			resealed := got[0]
			if !resealFirst {
				resealed = got[1]
			}
			x, c := f.execution(t, "X-1"), f.consolidation(t, "C-S")
			if resealed != nil || x.State != api.ExecutionClosed || x.CloseEvidence.Reference != "SEAL-2" || c.State != api.StateClosed || c.Revision != 4 {
				t.Fatalf("the container is sealed again and the consolidation stays CLOSED: %v, %s %q, %s at %d", resealed, x.State, x.CloseEvidence.Reference, c.State, c.Revision)
			}
		})
	}
}

// TestManyOpeningsOfOneContainerOpenItOnce: eight openings of one sealed container, each with its own command id and
// the revision the container was read at, run at once: one commits, the others find the revision moved (a conflict,
// which is not retried) and the container has one rework.
func TestManyOpeningsOfOneContainerOpenItOnce(t *testing.T) {
	ctx := context.Background()
	f := reworkDay(t)
	f.stuffedAndSealed(t)
	results := make(chan error, 8)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.module.OpenStuffingForRework(ctx, f.openRework(fmt.Sprint("race-", i), "X-1", 6, fmt.Sprint("reason ", i), fmt.Sprint("asker-", i), f.hoursIn(6)))
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	won, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			won++
		case errors.Is(err, api.ErrConflict):
			conflicts++
		default:
			t.Errorf("an opening answered %v", err)
		}
	}
	x := f.execution(t, "X-1")
	if won != 1 || conflicts != 7 || x.State != api.ExecutionRework || len(x.Reworks) != 1 || x.Revision != 7 || f.outboxCount(t, api.LoadExecutionReworkOpened) != 1 {
		t.Fatalf("%d won, %d conflicts, %+v", won, conflicts, x)
	}
}

// TestTwoHousesUnloadedAtOnceAreSerialised: H02 and H03 are unloaded at once at the revision they were read at: one
// commits and the other is a conflict (the execution's revision moved), and the second, run again at the new revision,
// commits.
func TestTwoHousesUnloadedAtOnceAreSerialised(t *testing.T) {
	ctx := context.Background()
	f := reworkDay(t)
	f.stuffedAndSealed(t)
	ok(t, 7)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-1", "X-1", 6, "x", "ops-asker", f.hoursIn(6))))
	results := make(map[string]chan error)
	var wg sync.WaitGroup
	for _, h := range []string{"H02", "H03"} {
		results[h] = make(chan error, 1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.module.UnloadHouseForRework(ctx, f.unloadHouse("u-"+h, "X-1", 7, h, f.hoursIn(7)))
			results[h] <- err
		}()
	}
	wg.Wait()
	var lost string
	for h, c := range results {
		if err := <-c; err != nil {
			if !errors.Is(err, api.ErrConflict) || lost != "" {
				t.Fatalf("%s: %v", h, err)
			}
			lost = h
		}
	}
	if lost == "" {
		t.Fatal("both unloadings committed at the revision 7")
	}
	ok(t, 9)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("u2-"+lost, "X-1", 8, lost, f.hoursIn(7))))
	if x := f.execution(t, "X-1"); x.TenderedPacks != 4 || len(x.Reversals) != 2 {
		t.Fatalf("both houses are out: %+v", x)
	}
}

// TestAContainerInReworkIsReadAndNeverLockedByTheCommandsOfAnotherContainer: H02 has 4 packs in X-1, which is in
// rework, and 2 in X-2, which is loading; the lot's measure is divided over both. X-1's row is held on a second
// connection, and an unload, a load and a measurement of the lot, all of which read the lot's other executions for the
// allotment and lock the other stuffings that are LOADING or ON_HOLD, answer at once: a stuffing in REWORK is read and
// never locked (it only loses cargo, and it is held by nothing), as a sealed one is. The two containers' figures for
// H02 add up to the lot's latest measure, the one in rework included; and the house of the lot that is not unloaded,
// H02, is why unloading H02 from X-1 is refused.
func TestAContainerInReworkIsReadAndNeverLockedByTheCommandsOfAnotherContainer(t *testing.T) {
	ctx := context.Background()
	f := reworkDay(t)
	f.begin(t, "X-1", 1)
	f.begin(t, "X-2", 2)
	at := f.hoursIn(4)
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("a-h01", "X-1", 2, "L-H01", 4, at)))
	ok(t, 4)(f.module.RecordCargoLoaded(ctx, f.load("a-h02", "X-1", 3, "L-H02", 4, at)))
	line := ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("b-h02", "X-2", 2, "L-H02", 2, at))).ItemNo
	ok(t, 5)(f.module.CloseLoadExecution(ctx, f.sealOf("X-1", 4, "SEAL-1")))
	ok(t, 6)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-1", "X-1", 5, "x", "ops-asker", f.hoursIn(6))))
	f.withoutRetry(t)
	_, release := f.holdRow(t, lockExecution, f.tenant, "X-1")
	defer release()
	if _, err := quickly(t, func(ctx context.Context) (api.Receipt, error) {
		return f.module.RecordCargoUnloaded(ctx, api.RecordCargoUnloaded{CommandID: "x2-unload", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-2", ExpectedRevision: 3,
			LineNo: line, Packs: 1, Reason: api.ReversalMisloaded, Evidence: evidence("x2-unload", f.hoursIn(7)), OccurredAt: f.hoursIn(7)})
	}); err != nil {
		t.Fatalf("an unload while the container in rework is locked: %v", err)
	}
	if _, err := quickly(t, func(ctx context.Context) (api.Receipt, error) {
		return f.module.RecordCargoLoaded(ctx, f.load("x2-load", "X-2", 4, "L-H02", 1, f.hoursIn(7)))
	}); err != nil {
		t.Fatalf("a load while the container in rework is locked: %v", err)
	}
	if _, err := quickly(t, func(ctx context.Context) (api.Receipt, error) {
		return f.module.RecordCargoMeasurement(ctx, api.RecordCargoMeasurement{CommandID: "x2-weigh", TenantID: f.tenant, ActorID: "ops-1", LotID: "L-H02", ExpectedRevision: 3,
			GrossWeight: kgm(310), Volume: cmq(15_000_000), Method: api.MethodScale, Evidence: evidence("x2-weigh", f.hoursIn(7)), OccurredAt: f.hoursIn(7)})
	}); err != nil {
		t.Fatalf("a measurement while the container in rework is locked: %v", err)
	}
	release()
	b := f.basis(t)
	h02 := f.house(t, b, "H02")
	if len(h02.Units) != 2 || h02.Units[0].LoadedPacks != 4 || h02.Units[1].LoadedPacks != 2 || h02.LoadedGrossWeight != kgm(310) ||
		h02.Units[0].LoadedGrossWeight.Amount+h02.Units[1].LoadedGrossWeight.Amount != 310 || b.PackingLists[0].ExecutionState != api.ExecutionRework {
		t.Fatalf("H02's two entries add up to the lot's measure: %+v", h02)
	}
	if h02.Units[0].LoadedGrossWeight != kgm(207) || h02.Units[1].LoadedGrossWeight != kgm(103) {
		t.Fatalf("310 kg over 4 and 2 packs by the largest remainder (206.67 and 103.33): %+v", h02.Units)
	}
	// the house H01 of X-1 leaves, and the figures of H02 do not move
	ok(t, 7)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("u-h01", "X-1", 6, "H01", f.hoursIn(8))))
	if again := f.house(t, f.basis(t), "H02"); again.Units[0].LoadedGrossWeight != kgm(207) || again.Units[1].LoadedGrossWeight != kgm(103) {
		t.Fatalf("H02 after H01 left: %+v", again.Units)
	}
}

// TestAClosedTenderIsNotOpenedForRework: the transition belongs to a sealed STUFFING, in the trigger and in the rework
// table.
func TestAClosedTenderIsNotOpenedForRework(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.create(t, "C-A")
	if _, err := f.module.AttachHouse(ctx, f.attach("attach-a", "C-A", "SH-A1", 0)); err != nil {
		t.Fatal(err)
	}
	f.ful.allocate("SH-A1", "C-A")
	ok(t, 2)(f.module.CloseConsolidation(ctx, f.closeCmd("close-a", "C-A", 1)))
	f.tender(t, "X-T", "C-A", f.hoursIn(4))
	if err := f.rawTx(t, `UPDATE cargo_phy.load_execution SET state='CLOSED', closed_by='raw', closed_at=started_at + interval '1 minute', closed_entered_at=now(),
        close_evidence_source='RECEIPT', close_evidence_reference='R-1', close_evidence_occurred_at=started_at WHERE tenant_id=$1 AND execution_id='X-T'`); err != nil {
		t.Fatalf("a tender closed by a raw write: %v", err)
	}
	insert := `INSERT INTO cargo_phy.stuffing_rework (tenant_id,execution_id,rework_no,reason,asked_by,asked_at,asked_entered_at,broken_seal_source,broken_seal_reference,
        broken_seal_sha256,broken_seal_occurred_at,broken_sealed_by,broken_sealed_at,broken_sealed_entered_at) SELECT tenant_id,execution_id,1,'r','raw',closed_at + interval '1 hour',now(),
        'SEAL',close_evidence_reference,close_evidence_sha256,close_evidence_occurred_at,closed_by,closed_at,closed_entered_at FROM cargo_phy.load_execution
        WHERE tenant_id=$1 AND execution_id='X-T'`
	f.refused(t, "a rework of a tender", "23514", "stuffing_rework_state", insert)
	f.refused(t, "a tender opened for rework", "23001", "cannot go from CLOSED to REWORK", `UPDATE cargo_phy.load_execution SET state='REWORK', closed_by=NULL, closed_at=NULL,
        closed_entered_at=NULL, close_evidence_source=NULL, close_evidence_reference=NULL, close_evidence_sha256=NULL, close_evidence_occurred_at=NULL
        WHERE tenant_id=$1 AND execution_id='X-T'`)
	// and through the command
	x := f.execution(t, "X-T")
	if _, err := f.module.OpenStuffingForRework(ctx, f.openRework("rework-t", "X-T", x.Revision, "x", "ops-asker", f.hoursIn(6))); !errors.Is(err, api.ErrRejected) ||
		!strings.Contains(err.Error(), "only a stuffing is opened for rework") {
		t.Fatalf("a tender opened for rework by the command: %v", err)
	}
}

// TestTheNewTablesAreIsolatedByTenant: another tenant sees no rework and no reopening, and cannot write one.
func TestTheNewTablesAreIsolatedByTenant(t *testing.T) {
	ctx := context.Background()
	f := reworkDay(t)
	f.stuffedAndSealed(t)
	ok(t, 7)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-1", "X-1", 6, "x", "ops-asker", f.hoursIn(6))))
	ok(t, 8)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("u-h03", "X-1", 7, "H03", f.hoursIn(7))))
	count := func(tenant, table string) int {
		var n int
		err := (scope.TenantTransactor{DB: f.db, Tenant: tenant}).WithinTransaction(ctx, func(txCtx context.Context) error {
			e, err := f.db.RequireExecutor(txCtx)
			if err != nil {
				return err
			}
			return e.QueryRow(txCtx, `SELECT count(*) FROM cargo_phy.`+table).Scan(&n)
		})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, table := range []string{"stuffing_rework", "cargo_lot_reopen"} {
		if own, other := count(f.tenant, table), count("another-tenant-"+f.tenant, table); own != 1 || other != 0 {
			t.Errorf("%s: %d rows of the tenant, %d seen by another", table, own, other)
		}
	}
}

// TestAPartOfAHouseIsNotUnloadedAndSealedInOneTransaction: H02 has two lines in X-1 (3 packs each). A raw transaction
// that reverses one of them and seals the container again before commit is refused: a reversal written in a rework is
// checked at commit for its whole house whatever the execution's state is by then (the reversal records the rework it
// was written in). Both lines and the reseal in one transaction are accepted.
func TestAPartOfAHouseIsNotUnloadedAndSealedInOneTransaction(t *testing.T) {
	ctx := context.Background()
	f := reworkDay(t)
	f.begin(t, "X-1", 1)
	at := f.hoursIn(4)
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("a-h02", "X-1", 2, "L-H02", 3, at)))
	ok(t, 4)(f.module.RecordCargoLoaded(ctx, f.load("b-h02", "X-1", 3, "L-H02", 3, at)))
	ok(t, 5)(f.module.RecordCargoLoaded(ctx, f.load("a-h01", "X-1", 4, "L-H01", 4, at)))
	ok(t, 6)(f.module.CloseLoadExecution(ctx, f.sealOf("X-1", 5, "SEAL-1")))
	ok(t, 7)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-1", "X-1", 6, "x", "ops-asker", f.hoursIn(6))))
	t7 := fmt.Sprintf("'%s'::timestamptz", f.hoursIn(7).Format(time.RFC3339Nano))
	when := `(SELECT asked_at + interval '1 minute' FROM cargo_phy.stuffing_rework WHERE tenant_id=$1 AND execution_id='X-1' AND rework_no=1)`
	end := `UPDATE cargo_phy.stuffing_rework SET ended_kind='SEALED', ended_by='raw-sealer', ended_at=` + when + `, ended_entered_at=now(), new_seal_reference='SEAL-9'
        WHERE tenant_id=$1 AND execution_id='X-1' AND rework_no=1`
	f.refused(t, "one of a house's lines reversed and the container sealed again", "23514", "reversal_line_rework_house", reversal(1, 1, 3, "UNSTUFFED", t7, t7),
		fmt.Sprintf(sealAgain, when, "SEAL-9", when), end)
	f.refused(t, "one of a house's lines reversed in REWORK", "23514", "reversal_line_rework_house", reversal(1, 1, 3, "UNSTUFFED", t7, t7))
	if err := f.rawTx(t, reversal(1, 1, 3, "UNSTUFFED", t7, t7), reversal(2, 2, 3, "UNSTUFFED", t7, t7), fmt.Sprintf(sealAgain, when, "SEAL-9", when), end); err != nil {
		t.Fatalf("the whole house and the reseal in one transaction: %v", err)
	}
	if x := f.execution(t, "X-1"); x.State != api.ExecutionClosed || x.TenderedPacks != 4 {
		t.Fatalf("X-1: %+v", x)
	}
}

// TestTheWholeHouseCheckWaitsForTheHouseLedger: the house's ledger row is held on a second connection and a raw
// transaction reverses the house's line in REWORK. The reversal is written, and the commit waits in the constraint
// trigger for the ledger, as a load of the house does; released, it commits. A trigger that did not take the lock would
// commit at once.
func TestTheWholeHouseCheckWaitsForTheHouseLedger(t *testing.T) {
	ctx := context.Background()
	f := reworkDay(t)
	f.stuffedAndSealed(t)
	ok(t, 7)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-1", "X-1", 6, "x", "ops-asker", f.hoursIn(6))))
	pid, release := f.holdRow(t, `SELECT 1 FROM cargo_phy.consol_membership WHERE tenant_id=$1 AND shipment_id=$2 AND stage='MAIN' FOR UPDATE`, f.tenant, "H02")
	t7 := fmt.Sprintf("'%s'::timestamptz", f.hoursIn(7).Format(time.RFC3339Nano))
	done := make(chan error, 1)
	go func() { done <- f.rawTx(t, reversal(1, 2, 6, "UNSTUFFED", t7, t7)) }()
	f.waitingBehind(t, pid, 1)
	select {
	case err := <-done:
		t.Fatalf("the commit did not wait for the ledger: %v", err)
	default:
	}
	release()
	if err := <-done; err != nil {
		t.Fatalf("the reversal after the ledger was released: %v", err)
	}
}

// TestTheDigestIsNotAWitnessOfARework: a container opened for rework and sealed again with a new seal, with nothing
// unloaded, has the state and the lines it had, so its packing list has the digest it had; Reworks tells.
func TestTheDigestIsNotAWitnessOfARework(t *testing.T) {
	ctx := context.Background()
	f := reworkDay(t)
	f.stuffedAndSealed(t)
	before := f.basis(t).PackingLists[0]
	ok(t, 7)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-1", "X-1", 6, "x", "ops-asker", f.hoursIn(6))))
	reseal := f.sealOf("X-1", 7, "SEAL-2")
	reseal.OccurredAt, reseal.Evidence.OccurredAt = f.hoursIn(8), f.hoursIn(8)
	ok(t, 8)(f.module.CloseLoadExecution(ctx, reseal))
	after := f.basis(t).PackingLists[0]
	if after.Digest != before.Digest || len(before.Reworks) != 0 || len(after.Reworks) != 1 || after.Reworks[0].NewSealReference != "SEAL-2" {
		t.Fatalf("before %+v, after %+v", before, after)
	}
}

// TestTheTriggerOverwritesTheReworkNumberAReversalSupplies: a reversal on a LOADING execution that supplies rework_no
// is stored with NULL; reversals in REWORK that supply a number, another one or NULL are stored with the open rework's
// number.
func TestTheTriggerOverwritesTheReworkNumberAReversalSupplies(t *testing.T) {
	ctx := context.Background()
	f := reworkDay(t)
	f.begin(t, "X-1", 1)
	at := f.hoursIn(4)
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("a-h01", "X-1", 2, "L-H01", 4, at)))
	ok(t, 4)(f.module.RecordCargoLoaded(ctx, f.load("a-h02", "X-1", 3, "L-H02", 6, at)))
	ok(t, 5)(f.module.RecordCargoLoaded(ctx, f.load("a-h03", "X-1", 4, "L-H03", 2, at)))
	t4 := fmt.Sprintf("'%s'::timestamptz", at.Format(time.RFC3339Nano))
	insert := func(no, line, packs int, supplied string) string {
		return fmt.Sprintf(`INSERT INTO cargo_phy.reversal_line (tenant_id,execution_id,reversal_no,line_no,packs,gross_weight,reason,evidence_source,evidence_reference,
            evidence_occurred_at,reversed_by,reversed_at,entered_at,rework_no) VALUES ($1,'X-1',%d,%d,%d,1,'UNSTUFFED','x','raw-%d',%s,'raw',%s,now(),%s)`, no, line, packs, no, t4, t4, supplied)
	}
	stored := func() map[int]*int {
		out := map[int]*int{}
		if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			rows, err := e.Query(ctx, `SELECT reversal_no, rework_no FROM cargo_phy.reversal_line WHERE tenant_id=$1 AND execution_id='X-1'`, f.tenant)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var no int
				var rw *int
				if err = rows.Scan(&no, &rw); err != nil {
					return err
				}
				out[no] = rw
			}
			return rows.Err()
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	// LOADING: a part of a line reversed, with a number supplied
	if err := f.rawTx(t, insert(1, 1, 1, "1")); err != nil {
		t.Fatal(err)
	}
	if got := stored(); got[1] != nil {
		t.Fatalf("a reversal while loading supplied rework 1 and kept it: %v", *got[1])
	}
	ok(t, 6)(f.module.CloseLoadExecution(ctx, f.sealOf("X-1", 5, "SEAL-1")))
	ok(t, 7)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-1", "X-1", 6, "x", "ops-asker", f.hoursIn(6))))
	// REWORK: H02 whole, supplying another number; H03 whole, supplying NULL (and H01 has 3 packs left, so it is not
	// touched)
	if err := f.rawTx(t, insert(2, 2, 6, "7"), insert(3, 3, 2, "NULL")); err != nil {
		t.Fatal(err)
	}
	if got := stored(); got[2] == nil || *got[2] != 1 || got[3] == nil || *got[3] != 1 || got[1] != nil {
		t.Fatalf("reversals in rework 1 supplied 7 and NULL: %v %v %v", got[1], got[2], got[3])
	}
}

// TestSealsWithLettersWhoseCapitalsAreAsciiAreTheSameSealInTheDatabase: U+0131 (dotless i) and U+017F (long s) are the
// letters I and S for SEA's key. After SEALI1 was broken on X-1 and SEALS1 on X-2, a raw reseal, or an ending of the
// rework, with the spelling that uses them is refused as the broken seal put back; and the database's key is the
// domain's for these.
func TestSealsWithLettersWhoseCapitalsAreAsciiAreTheSameSealInTheDatabase(t *testing.T) {
	ctx := context.Background()
	f := reworkDay(t)
	f.begin(t, "X-1", 1)
	f.begin(t, "X-2", 2)
	at := f.hoursIn(4)
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("a-h01", "X-1", 2, "L-H01", 4, at)))
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("b-h02", "X-2", 2, "L-H02", 6, at)))
	ok(t, 4)(f.module.CloseLoadExecution(ctx, f.sealOf("X-1", 3, "SEALI1")))
	ok(t, 4)(f.module.CloseLoadExecution(ctx, f.sealOf("X-2", 3, "SEALS1")))
	ok(t, 5)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-1", "X-1", 4, "x", "ops-asker", f.hoursIn(6))))
	ok(t, 5)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-2", "X-2", 4, "x", "ops-asker", f.hoursIn(6))))
	for x, tc := range map[string]struct{ broken, again string }{"X-1": {"SEALI1", "SEALı1"}, "X-2": {"SEALS1", "SEALſ1"}} {
		reseal := strings.Replace(fmt.Sprintf(sealAgain, "now()", tc.again, "now()"), "'X-1'", "'"+x+"'", 1)
		f.refused(t, "X-1 sealed again with "+tc.again, "23514", "load_execution_reseal_new", reseal)
		f.refused(t, x+" rework ended with "+tc.again, "23514", "stuffing_rework_new_seal", fmt.Sprintf(`UPDATE cargo_phy.stuffing_rework SET ended_kind='SEALED',
            ended_by='raw', ended_at=asked_at + interval '1 hour', ended_entered_at=now(), new_seal_reference='%s' WHERE tenant_id=$1 AND execution_id='%s' AND rework_no=1`, tc.again, x))
		var same bool
		if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			return e.QueryRow(ctx, `SELECT cargo_phy.seal_key($1::text) = cargo_phy.seal_key($2::text)`, tc.broken, tc.again).Scan(&same)
		}); err != nil || !same {
			t.Fatalf("the database's key of %q and %q: %v %v", tc.broken, tc.again, same, err)
		}
	}
}
