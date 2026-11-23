//go:build integration

package integration

import (
	"context"
	"testing"

	"go.idp.xyz/idp-cargo/internal/contexts/phy/api"
)

// HouseCostBasis.UnloadedBy (V2.95) names the rework that last unloaded a live house since its latest entry began, read
// in the basis's snapshot with the reversal lines that name the rework of a former house. It is part of no digest and
// does not change the basis's revision. The days are those of the V2.85 and V2.86 tests.

func unloadedBy(t *testing.T, f *fixture, house string) api.HouseUnloadedBy {
	t.Helper()
	return f.house(t, f.basis(t), house).UnloadedBy
}

// TestAHouseUnloadedInAReworkAndStillAttachedSaysWhichReworkUnloadedIt: H03 is unloaded in rework 1 of X-1 (asked by
// ops-asker), while X-1 is in REWORK and after it is sealed again. H03 is live, has nothing loaded and says so; H01 and
// H02, never unloaded, say nothing. The basis's revision is the consolidation's, and every packing list still has the
// digest of its text.
func TestAHouseUnloadedInAReworkAndStillAttachedSaysWhichReworkUnloadedIt(t *testing.T) {
	f := reworkDay(t)
	f.stuffedAndSealed(t)
	for _, h := range []string{"H01", "H02", "H03"} {
		if u := unloadedBy(t, f, h); u != (api.HouseUnloadedBy{}) {
			t.Fatalf("%s before any rework: %+v", h, u)
		}
	}
	ctx := context.Background()
	ok(t, 7)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-1", "X-1", 6, "the consignee of H03 withdrew its cargo", "ops-asker", f.hoursIn(6))))
	ok(t, 8)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("unload-h03", "X-1", 7, "H03", f.hoursIn(7))))
	want := api.HouseUnloadedBy{ExecutionID: "X-1", ReworkNo: 1, UnitNo: 1, AskedBy: "ops-asker"}
	check := func(when string) {
		t.Helper()
		b := f.basis(t)
		c, err := f.module.GetConsolidation(ctx, api.GetConsolidation{TenantID: f.tenant, ConsolidationID: "C-S"})
		if err != nil {
			t.Fatal(err)
		}
		if h := f.house(t, b, "H03"); h.UnloadedBy != want || h.LoadedPacks != 0 || len(h.Units) != 0 {
			t.Fatalf("%s: H03 %+v", when, h)
		}
		for _, h := range []string{"H01", "H02"} {
			if x := f.house(t, b, h); x.UnloadedBy != (api.HouseUnloadedBy{}) || x.LoadedPacks == 0 {
				t.Fatalf("%s: %s %+v", when, h, x)
			}
		}
		if b.Revision != c.Revision || len(b.FormerHouses) != 0 {
			t.Fatalf("%s: basis revision %d, consolidation %d, former %+v", when, b.Revision, c.Revision, b.FormerHouses)
		}
		for _, l := range b.PackingLists {
			if l.Digest != digestOf(l) {
				t.Fatalf("%s: the digest of %+v is not its text's", when, l)
			}
		}
	}
	check("X-1 in REWORK")
	f.reseal(t, 8)
	check("X-1 sealed again")
}

// TestAHouseUnloadedAndLoadedAgainInAnotherContainerHasBoth: H03 is unloaded in rework 1 of X-1 and loaded into X-2: it
// says which rework unloaded it, and its units say where it is loaded now.
func TestAHouseUnloadedAndLoadedAgainInAnotherContainerHasBoth(t *testing.T) {
	ctx := context.Background()
	f := reworkDay(t)
	f.begin(t, "X-1", 1)
	f.begin(t, "X-2", 2)
	at := f.hoursIn(4)
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load-H03", "X-1", 2, "L-H03", 2, at)))
	ok(t, 4)(f.module.RecordCargoLoaded(ctx, f.load("load-H01", "X-1", 3, "L-H01", 4, at)))
	ok(t, 5)(f.module.CloseLoadExecution(ctx, f.sealOf("X-1", 4, "SEAL-1")))
	ok(t, 6)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-1", "X-1", 5, "x", "ops-asker", f.hoursIn(6))))
	ok(t, 7)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("unload-h03", "X-1", 6, "H03", f.hoursIn(7))))
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load-h03-x2", "X-2", 2, "L-H03", 2, f.hoursIn(8))))
	h := f.house(t, f.basis(t), "H03")
	if h.UnloadedBy != (api.HouseUnloadedBy{ExecutionID: "X-1", ReworkNo: 1, UnitNo: 1, AskedBy: "ops-asker"}) || h.LoadedPacks != 2 || len(h.Units) != 1 || h.Units[0].UnitNo != 2 ||
		h.Units[0].ExecutionID != "X-2" {
		t.Fatalf("H03 unloaded in X-1 and loaded in X-2: %+v", h)
	}
	if u := unloadedBy(t, f, "H01"); u != (api.HouseUnloadedBy{}) {
		t.Fatalf("H01 was never unloaded: %+v", u)
	}
}

// TestAHouseUnloadedInTwoReworksSaysTheLatest: H03 is unloaded in rework 1 of X-1 (asked by ops-asker), loaded into
// X-2, which is sealed and opened for rework by ops-second, and unloaded there: the basis names X-2's rework, its unit
// and its asker.
func TestAHouseUnloadedInTwoReworksSaysTheLatest(t *testing.T) {
	ctx := context.Background()
	f := reworkDay(t)
	f.begin(t, "X-1", 1)
	f.begin(t, "X-2", 2)
	at := f.hoursIn(4)
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load-H03", "X-1", 2, "L-H03", 2, at)))
	ok(t, 4)(f.module.RecordCargoLoaded(ctx, f.load("load-H01", "X-1", 3, "L-H01", 4, at)))
	ok(t, 5)(f.module.CloseLoadExecution(ctx, f.sealOf("X-1", 4, "SEAL-1")))
	ok(t, 6)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-1", "X-1", 5, "x", "ops-asker", f.hoursIn(6))))
	ok(t, 7)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("unload-h03", "X-1", 6, "H03", f.hoursIn(7))))
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load-h03-x2", "X-2", 2, "L-H03", 2, f.hoursIn(8))))
	sealB := f.sealOf("X-2", 3, "SEAL-B")
	sealB.OccurredAt, sealB.Evidence.OccurredAt = f.hoursIn(8), f.hoursIn(8)
	ok(t, 4)(f.module.CloseLoadExecution(ctx, sealB))
	ok(t, 5)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-x2", "X-2", 4, "again", "ops-second", f.hoursIn(9))))
	ok(t, 6)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("unload-h03-x2", "X-2", 5, "H03", f.hoursIn(10))))
	want := api.HouseUnloadedBy{ExecutionID: "X-2", ReworkNo: 1, UnitNo: 2, AskedBy: "ops-second"}
	if h := f.house(t, f.basis(t), "H03"); h.UnloadedBy != want || h.LoadedPacks != 0 {
		t.Fatalf("H03 unloaded twice: %+v, want %+v", h, want)
	}
}

// TestAHouseAttachedAgainAfterItWasUnloadedAndLeftSaysNothing: every house is unloaded and detached from the closed
// consolidation, the stuffing is given up and the consolidation reopens; H03 is attached again. Its earlier reversals
// belong to an earlier stay, so the basis does not say that a rework unloaded it since its latest entry began.
func TestAHouseAttachedAgainAfterItWasUnloadedAndLeftSaysNothing(t *testing.T) {
	ctx := context.Background()
	f := leaveDay(t)
	ok(t, 9)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("unload-h02", "X-1", 8, "H02", f.hoursIn(8))))
	ok(t, 10)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("unload-h01", "X-1", 9, "H01", f.hoursIn(9))))
	for _, h := range []string{"H01", "H02", "H03"} {
		ok(t, 2)(f.module.DetachHouse(ctx, f.detach("detach-"+h, "C-S", h, 1, api.ReasonRollover, "", f.hoursIn(10))))
	}
	ok(t, 11)(f.module.CancelLoadExecution(ctx, f.cancelCmd("cancel-x1", "X-1", 10, "every house left", f.hoursIn(11))))
	ok(t, 5)(f.module.ReopenConsolidation(ctx, api.ReopenConsolidation{CommandID: "reopen", TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: "C-S",
		ExpectedRevision: 4, Reason: "every house left the container", OccurredAt: f.hoursIn(12)}))
	back := f.attach("attach-h03-again", "C-S", "H03", 2)
	back.OccurredAt = f.hoursIn(13)
	ok(t, 3)(f.module.AttachHouse(ctx, back))
	b := f.basis(t)
	if h := f.house(t, b, "H03"); h.UnloadedBy != (api.HouseUnloadedBy{}) || len(b.Houses) != 1 || len(b.FormerHouses) != 2 {
		t.Fatalf("H03 attached again: %+v, former %+v", h, b.FormerHouses)
	}
}
