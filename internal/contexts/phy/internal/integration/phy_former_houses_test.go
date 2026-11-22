//go:build integration

package integration

import (
	"context"
	"reflect"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/phy/api"
)

// The houses that left a consolidation and the reworks of a given-up stuffing are in PHY's cost basis (V2.92), with the
// real commands and the real database. The days are the ones of the V2.86 and V2.87 tests: C-S with H01, H02 and H03 in
// X-1 (H03 unloaded in a rework), and C-S with H04 shut out before stuffing.

// formerOf returns the former house of the shipment, and fails when the basis has none.
func formerOf(t *testing.T, b api.PhysicalCostBasis, house string) api.FormerHouse {
	t.Helper()
	for _, h := range b.FormerHouses {
		if h.ShipmentID == house {
			return h
		}
	}
	t.Fatalf("no former house %s in %+v", house, b.FormerHouses)
	return api.FormerHouse{}
}

// noFormer checks the basis lists no former house.
func noFormer(t *testing.T, what string, b api.PhysicalCostBasis) {
	t.Helper()
	if len(b.FormerHouses) != 0 {
		t.Fatalf("%s: former houses %+v, want none", what, b.FormerHouses)
	}
}

// entered checks the time PHY recorded the end: it is set, and after the business time, which lies in the past in these
// tests, so a basis that told the business time for it is caught.
func entered(t *testing.T, h api.FormerHouse) {
	t.Helper()
	if h.EndedEnteredAt.IsZero() || !h.EndedEnteredAt.After(h.EndedAt) {
		t.Fatalf("former house %s: recorded at %s for business time %s", h.ShipmentID, h.EndedEnteredAt, h.EndedAt)
	}
}

// TestAHouseDetachedFromTheClosedConsolidationAfterAReworkIsAFormerHouse: H03 is unloaded in rework 1 (asked by
// ops-asker) and detached while X-1 is still in REWORK; the basis lists it with the rework, and still does once the
// container is sealed again. H01 and H02 are live and are not listed.
func TestAHouseDetachedFromTheClosedConsolidationAfterAReworkIsAFormerHouse(t *testing.T) {
	ctx := context.Background()
	f := leaveDay(t)
	noFormer(t, "before H03 leaves", f.basis(t))
	ok(t, 2)(f.module.DetachHouse(ctx, f.detach("detach-h03", "C-S", "H03", 1, api.ReasonSpaceShortage, "", f.hoursIn(8))))
	for _, when := range []string{"X-1 in REWORK", "X-1 sealed again"} {
		b := f.basis(t)
		if len(b.Houses) != 2 || len(b.FormerHouses) != 1 {
			t.Fatalf("%s: houses %+v, former %+v", when, b.Houses, b.FormerHouses)
		}
		h := formerOf(t, b, "H03")
		entered(t, h)
		want := api.FormerHouse{ShipmentID: "H03", End: api.FormerDetached, Reason: api.ReasonSpaceShortage, EndedBy: "ops-1", EndedAt: f.hoursIn(8),
			EndedEnteredAt: h.EndedEnteredAt, ReworkFrom: api.ReworkFromEntry, ReworkExecutionID: "X-1", ReworkNo: 1, ReworkUnitNo: 1, ReworkAskedBy: "ops-asker"}
		if !reflect.DeepEqual(h, want) {
			t.Fatalf("%s: former house %+v, want %+v", when, h, want)
		}
		if when == "X-1 in REWORK" {
			f.reseal(t, 8)
		}
	}
}

// TestAHouseTransferredAfterAReworkNamesTheConsolidationItWentTo: H03 is transferred to C-T; C-S lists it with C-T and
// the rework, and C-T, where it is live, lists nobody.
func TestAHouseTransferredAfterAReworkNamesTheConsolidationItWentTo(t *testing.T) {
	ctx := context.Background()
	f := leaveDay(t)
	f.reseal(t, 8)
	f.targetOf(t)
	f.ful.sea["H03"] = true
	ok(t, 2)(f.module.TransferHouse(ctx, f.transfer("transfer-h03", "C-S", "C-T", "H03", 1, api.ReasonWrongConsol, "", f.hoursIn(9))))
	b := f.basis(t)
	h := formerOf(t, b, "H03")
	entered(t, h)
	want := api.FormerHouse{ShipmentID: "H03", End: api.FormerTransferred, TransferredTo: "C-T", Reason: api.ReasonWrongConsol, EndedBy: "ops-1",
		EndedAt: f.hoursIn(9), EndedEnteredAt: h.EndedEnteredAt, ReworkFrom: api.ReworkFromEntry, ReworkExecutionID: "X-1", ReworkNo: 1, ReworkUnitNo: 1, ReworkAskedBy: "ops-asker"}
	if len(b.FormerHouses) != 1 || !reflect.DeepEqual(h, want) {
		t.Fatalf("former houses %+v, want %+v", b.FormerHouses, want)
	}
	if tb := f.basisOf(t, "C-T"); len(tb.Houses) != 1 || tb.Houses[0].ShipmentID != "H03" {
		t.Fatalf("C-T: %+v", tb.Houses)
	} else {
		noFormer(t, "C-T, where H03 is live", tb)
	}
}

// TestAHouseShutOutBeforeStuffingIsAFormerHouseWithNoRework: H04 leaves with the reason SHUT_OUT and no rework; it is
// listed with the rework left empty.
func TestAHouseShutOutBeforeStuffingIsAFormerHouseWithNoRework(t *testing.T) {
	ctx := context.Background()
	f := shutOutDay(t, false)
	noFormer(t, "before H04 leaves", f.basis(t))
	ok(t, 2)(f.module.DetachHouse(ctx, f.detach("detach-h04", "C-S", "H04", 1, api.ReasonShutOut, "", f.hoursIn(6))))
	b := f.basis(t)
	h := formerOf(t, b, "H04")
	entered(t, h)
	want := api.FormerHouse{ShipmentID: "H04", End: api.FormerDetached, Reason: api.ReasonShutOut, EndedBy: "ops-1", EndedAt: f.hoursIn(6),
		EndedEnteredAt: h.EndedEnteredAt, ReworkFrom: api.ReworkFromNone}
	if len(b.FormerHouses) != 1 || !reflect.DeepEqual(h, want) || len(b.Houses) != 2 {
		t.Fatalf("former houses %+v, want %+v; houses %+v", b.FormerHouses, want, b.Houses)
	}
	if len(b.GivenUpStuffings) != 0 {
		t.Fatalf("no stuffing was given up: %+v", b.GivenUpStuffings)
	}
}

// TestFormerHousesOfAnOpenConsolidationInShipmentOrder: three houses of an OPEN air consolidation, two of them detached
// (the later shipment first): both are listed in shipment order with the text of the reason, no rework and no target,
// and the third is live.
func TestFormerHousesOfAnOpenConsolidationInShipmentOrder(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.create(t, "C-1")
	for i, h := range []string{"SH-1", "SH-2", "SH-3"} {
		if _, err := f.module.AttachHouse(ctx, f.attach("attach-"+h, "C-1", h, 0)); err != nil {
			t.Fatalf("attach %d: %v", i, err)
		}
		f.ful.allocate(h, "C-1")
	}
	at := f.at.Add(5 * time.Hour)
	ok(t, 2)(f.module.DetachHouse(ctx, f.detach("detach-3", "C-1", "SH-3", 1, api.ReasonOther, "the customer changed its mind", at)))
	ok(t, 2)(f.module.DetachHouse(ctx, f.detach("detach-1", "C-1", "SH-1", 1, api.ReasonRollover, "", at.Add(time.Hour))))
	b := f.basisOf(t, "C-1")
	if len(b.Houses) != 1 || b.Houses[0].ShipmentID != "SH-2" || len(b.FormerHouses) != 2 {
		t.Fatalf("houses %+v, former %+v", b.Houses, b.FormerHouses)
	}
	first, second := b.FormerHouses[0], b.FormerHouses[1]
	entered(t, first)
	entered(t, second)
	wantFirst := api.FormerHouse{ShipmentID: "SH-1", End: api.FormerDetached, Reason: api.ReasonRollover, EndedBy: "ops-1", EndedAt: at.Add(time.Hour),
		EndedEnteredAt: first.EndedEnteredAt, ReworkFrom: api.ReworkFromNone}
	wantSecond := api.FormerHouse{ShipmentID: "SH-3", End: api.FormerDetached, Reason: api.ReasonOther, ReasonText: "the customer changed its mind", EndedBy: "ops-1",
		EndedAt: at, EndedEnteredAt: second.EndedEnteredAt, ReworkFrom: api.ReworkFromNone}
	if !reflect.DeepEqual(first, wantFirst) || !reflect.DeepEqual(second, wantSecond) {
		t.Fatalf("former houses %+v, want %+v and %+v", b.FormerHouses, wantFirst, wantSecond)
	}
}

// cancelShipment has FUL report the shipment CANCELLED and ends the house's membership for it, as the person given.
func (f *fixture) cancelShipment(t *testing.T, command, consolidation, house, actor string, expected int64, at time.Time) {
	t.Helper()
	f.ful.mu.Lock()
	f.ful.status[house], f.ful.allocated[house] = "CANCELLED", ""
	f.ful.mu.Unlock()
	ok(t, expected+1)(f.module.EndMembershipForCancelledShipment(context.Background(), api.EndMembershipForCancelledShipment{CommandID: command, TenantID: f.tenant,
		ActorID: actor, ConsolidationID: consolidation, ShipmentID: house, ExpectedMembershipRevision: expected, OccurredAt: at}))
}

// TestAHouseWhoseLatestEntryEndedByRefreshOrCancellationIsListedByTheLatestEnd: a refresh is no departure (the house is
// live with the next entry); the shipment's cancellation is one, whatever came before. SH-1 is refreshed and its
// shipment cancelled (listed as cancelled); SH-2 is only refreshed (live, not listed); SH-3 is cancelled with no
// earlier entry that ended; SH-4 is detached, attached again and cancelled: it is listed as cancelled, by the person
// who ended the entry, not with the departure the re-attach undid; SH-5 is refreshed and then detached.
func TestAHouseWhoseLatestEntryEndedByRefreshOrCancellationIsListedByTheLatestEnd(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.create(t, "C-1")
	for _, h := range []string{"SH-1", "SH-2", "SH-3", "SH-4", "SH-5"} {
		if _, err := f.module.AttachHouse(ctx, f.attach("attach-"+h, "C-1", h, 0)); err != nil {
			t.Fatal(err)
		}
		f.ful.allocate(h, "C-1")
	}
	at := f.at.Add(5 * time.Hour)
	refresh := func(h, command string, expected int64) {
		t.Helper()
		f.ful.replan(h, 3)
		ok(t, expected+1)(f.module.RefreshHouseIntent(ctx, api.RefreshHouseIntent{CommandID: command, TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: "C-1",
			ShipmentID: h, ExpectedMembershipRevision: expected, OccurredAt: at}))
	}
	refresh("SH-1", "refresh-1", 1)
	refresh("SH-2", "refresh-2", 1)
	refresh("SH-5", "refresh-5", 1)
	if m := f.membership(t, "SH-1"); !m.Live || m.History[0].End != api.EntryRefreshed || len(m.History) != 2 {
		t.Fatalf("SH-1 refreshed: %+v", m)
	}
	noFormer(t, "refreshed houses, all live", f.basisOf(t, "C-1"))
	f.cancelShipment(t, "cancel-1", "C-1", "SH-1", "ops-8", 2, at.Add(time.Hour))
	f.cancelShipment(t, "cancel-3", "C-1", "SH-3", "ops-7", 1, at.Add(time.Hour))
	ok(t, 2)(f.module.DetachHouse(ctx, f.detach("detach-4", "C-1", "SH-4", 1, api.ReasonRollover, "", at)))
	back := f.attach("attach-4-again", "C-1", "SH-4", 2)
	back.OccurredAt = at.Add(time.Hour)
	ok(t, 3)(f.module.AttachHouse(ctx, back))
	f.cancelShipment(t, "cancel-4", "C-1", "SH-4", "ops-9", 3, at.Add(2*time.Hour))
	ok(t, 3)(f.module.DetachHouse(ctx, f.detach("detach-5", "C-1", "SH-5", 2, api.ReasonCarrierChange, "", at.Add(time.Hour))))

	b := f.basisOf(t, "C-1")
	if len(b.Houses) != 1 || b.Houses[0].ShipmentID != "SH-2" || len(b.FormerHouses) != 4 {
		t.Fatalf("houses %+v, former %+v", b.Houses, b.FormerHouses)
	}
	for _, w := range []struct {
		house, end, reason, by string
		at                     time.Time
	}{{"SH-1", api.FormerCancelled, api.ReasonShipmentCancelled, "ops-8", at.Add(time.Hour)}, {"SH-3", api.FormerCancelled, api.ReasonShipmentCancelled, "ops-7", at.Add(time.Hour)},
		{"SH-4", api.FormerCancelled, api.ReasonShipmentCancelled, "ops-9", at.Add(2 * time.Hour)}, {"SH-5", api.FormerDetached, api.ReasonCarrierChange, "ops-1", at.Add(time.Hour)}} {
		h := formerOf(t, b, w.house)
		entered(t, h)
		want := api.FormerHouse{ShipmentID: w.house, End: w.end, Reason: w.reason, EndedBy: w.by, EndedAt: w.at, EndedEnteredAt: h.EndedEnteredAt, ReworkFrom: api.ReworkFromNone}
		if !reflect.DeepEqual(h, want) {
			t.Fatalf("%s: %+v, want %+v", w.house, h, want)
		}
	}
}

// TestAHouseThatLeftAndCameBackIsLiveAndAHouseThatLeftTwiceIsListedOnce: PHY lets a house that was detached from an
// OPEN consolidation be attached to it again (the ledger's next entry). It is live then and is not listed; detached a
// second time it is listed once, with its second departure.
func TestAHouseThatLeftAndCameBackIsLiveAndAHouseThatLeftTwiceIsListedOnce(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.create(t, "C-1")
	if _, err := f.module.AttachHouse(ctx, f.attach("attach-1", "C-1", "SH-1", 0)); err != nil {
		t.Fatal(err)
	}
	f.ful.allocate("SH-1", "C-1")
	at := f.at.Add(5 * time.Hour)
	ok(t, 2)(f.module.DetachHouse(ctx, f.detach("detach-1", "C-1", "SH-1", 1, api.ReasonRollover, "", at)))
	if h := formerOf(t, f.basisOf(t, "C-1"), "SH-1"); h.Reason != api.ReasonRollover {
		t.Fatalf("the first departure: %+v", h)
	}
	back := f.attach("attach-again", "C-1", "SH-1", 2)
	back.OccurredAt = at.Add(time.Hour)
	ok(t, 3)(f.module.AttachHouse(ctx, back))
	b := f.basisOf(t, "C-1")
	noFormer(t, "a house back in the consolidation", b)
	if len(b.Houses) != 1 || b.Houses[0].ShipmentID != "SH-1" {
		t.Fatalf("houses: %+v", b.Houses)
	}
	ok(t, 4)(f.module.DetachHouse(ctx, f.detach("detach-2", "C-1", "SH-1", 3, api.ReasonCustomerRequest, "", at.Add(2*time.Hour))))
	b = f.basisOf(t, "C-1")
	if len(b.FormerHouses) != 1 || b.FormerHouses[0].Reason != api.ReasonCustomerRequest || !b.FormerHouses[0].EndedAt.Equal(at.Add(2*time.Hour)) {
		t.Fatalf("the second departure only: %+v", b.FormerHouses)
	}
}

// TestAStuffingGivenUpAfterTwoReworksKeepsBothAndItsHousesNameThem: rework 1 (ops-asker) unloads H03 and ends sealed
// again; rework 2 (ops-asker-2) unloads H02 and H01 and gives the stuffing up. PHY's basis has no packing list for the
// unit; GivenUpStuffings lists the stuffing with both reworks, and the three houses, detached, name the rework that
// unloaded each, with its unit and asker, though the stuffing is cancelled.
func TestAStuffingGivenUpAfterTwoReworksKeepsBothAndItsHousesNameThem(t *testing.T) {
	ctx := context.Background()
	f := leaveDay(t)
	f.reseal(t, 8)
	ok(t, 10)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-2", "X-1", 9, "the whole booking is rolled", "ops-asker-2", f.hoursIn(9))))
	ok(t, 11)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("unload-h02", "X-1", 10, "H02", f.hoursIn(10))))
	ok(t, 12)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("unload-h01", "X-1", 11, "H01", f.hoursIn(11))))
	before := f.basis(t)
	if len(before.GivenUpStuffings) != 0 || len(before.PackingLists) != 1 {
		t.Fatalf("while rework 2 is open the stuffing is not given up: %+v", before.GivenUpStuffings)
	}
	ok(t, 13)(f.module.CancelLoadExecution(ctx, f.cancelCmd("cancel-1", "X-1", 12, "the booking is rolled to the next sailing", f.hoursIn(12))))
	for _, h := range []string{"H03", "H02", "H01"} {
		ok(t, 2)(f.module.DetachHouse(ctx, f.detach("detach-"+h, "C-S", h, 1, api.ReasonRollover, "", f.hoursIn(13))))
	}
	b := f.basis(t)
	if len(b.PackingLists) != 0 || len(b.Houses) != 0 {
		t.Fatalf("a cancelled stuffing has no packing list: %+v, houses %+v", b.PackingLists, b.Houses)
	}
	if len(b.GivenUpStuffings) != 1 {
		t.Fatalf("given-up stuffings: %+v", b.GivenUpStuffings)
	}
	g := b.GivenUpStuffings[0]
	x := f.execution(t, "X-1")
	if g.UnitNo != 1 || g.ExecutionID != "X-1" || !reflect.DeepEqual(g.Reworks, x.Reworks) || len(g.Reworks) != 2 {
		t.Fatalf("given-up stuffing %+v, want unit 1, X-1 and the reworks %+v", g, x.Reworks)
	}
	if r := g.Reworks[0]; r.ReworkNo != 1 || r.AskedBy != "ops-asker" || r.End != api.ReworkSealed || r.NewSealReference != "SEAL-2" || !r.EndedAt.Equal(f.hoursIn(8)) {
		t.Fatalf("rework 1 ended sealed: %+v", r)
	}
	if r := g.Reworks[1]; r.ReworkNo != 2 || r.AskedBy != "ops-asker-2" || r.End != api.ReworkGivenUp || !r.EndedAt.Equal(f.hoursIn(12)) || r.EndedBy != "ops-1" {
		t.Fatalf("rework 2 given up: %+v", r)
	}
	if len(b.FormerHouses) != 3 {
		t.Fatalf("former houses: %+v", b.FormerHouses)
	}
	for i, h := range []struct {
		house string
		no    int
		asker string
	}{{"H01", 2, "ops-asker-2"}, {"H02", 2, "ops-asker-2"}, {"H03", 1, "ops-asker"}} {
		got := b.FormerHouses[i]
		if got.ShipmentID != h.house || got.ReworkExecutionID != "X-1" || got.ReworkNo != h.no || got.ReworkUnitNo != 1 || got.ReworkAskedBy != h.asker ||
			got.End != api.FormerDetached {
			t.Fatalf("former house %d = %+v, want %s in rework %d asked by %s", i, got, h.house, h.no, h.asker)
		}
	}
}

// TestAStuffingGivenUpWithNoReworkIsNotListed: a stuffing given up that was never reworked (opened, unloaded and
// cancelled before it was sealed) is not in GivenUpStuffings: only a stuffing with a rework is.
func TestAStuffingGivenUpWithNoReworkIsNotListed(t *testing.T) {
	ctx := context.Background()
	f, revision := cancelDay(t, false)
	ok(t, revision+1)(f.module.CancelLoadExecution(ctx, f.cancelCmd("cancel-2", "X-2", revision, "the container was rolled", f.hoursIn(6))))
	b := f.basisOf(t, "C-S")
	if len(b.GivenUpStuffings) != 0 || len(b.FormerHouses) != 0 {
		t.Fatalf("given up %+v, former %+v", b.GivenUpStuffings, b.FormerHouses)
	}
}

// TestStuffingsGivenUpAreListedInUnitOrder: X-1 is on unit 2 and X-2 on unit 1, so the order of their ids is not the
// order of their units; each is sealed, opened for rework by another person, emptied and given up. GivenUpStuffings
// lists unit 1 (X-2) before unit 2 (X-1).
func TestStuffingsGivenUpAreListedInUnitOrder(t *testing.T) {
	ctx := context.Background()
	f := reworkDay(t)
	at := f.hoursIn(4)
	for _, x := range []struct {
		id, house, asker string
		unit             int
		packs            int64
	}{{"X-1", "H01", "ops-a", 2, 4}, {"X-2", "H02", "ops-b", 1, 6}} {
		f.begin(t, x.id, x.unit)
		ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load-"+x.id, x.id, 2, "L-"+x.house, x.packs, at)))
		ok(t, 4)(f.module.CloseLoadExecution(ctx, f.sealOf(x.id, 3, "SEAL-"+x.id)))
		ok(t, 5)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-"+x.id, x.id, 4, "rolled", x.asker, f.hoursIn(6))))
		ok(t, 6)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("unload-"+x.id, x.id, 5, x.house, f.hoursIn(7))))
		ok(t, 7)(f.module.CancelLoadExecution(ctx, f.cancelCmd("cancel-"+x.id, x.id, 6, "rolled", f.hoursIn(8))))
	}
	b := f.basis(t)
	if len(b.GivenUpStuffings) != 2 || b.GivenUpStuffings[0].UnitNo != 1 || b.GivenUpStuffings[0].ExecutionID != "X-2" || b.GivenUpStuffings[1].UnitNo != 2 ||
		b.GivenUpStuffings[1].ExecutionID != "X-1" || b.GivenUpStuffings[0].Reworks[0].AskedBy != "ops-b" || b.GivenUpStuffings[1].Reworks[0].AskedBy != "ops-a" {
		t.Fatalf("given-up stuffings: %+v", b.GivenUpStuffings)
	}
	// two consolidations' worth of reworks in one: each house that left names the rework and the stuffing that unloaded
	// it, with that stuffing's unit and asker
	ok(t, 2)(f.module.DetachHouse(ctx, f.detach("detach-H01", "C-S", "H01", 1, api.ReasonRollover, "", f.hoursIn(9))))
	ok(t, 2)(f.module.DetachHouse(ctx, f.detach("detach-H02", "C-S", "H02", 1, api.ReasonRollover, "", f.hoursIn(9))))
	b = f.basis(t)
	for i, w := range []struct {
		house, execution, asker string
		unit                    int
	}{{"H01", "X-1", "ops-a", 2}, {"H02", "X-2", "ops-b", 1}} {
		h := b.FormerHouses[i]
		if h.ShipmentID != w.house || h.ReworkFrom != api.ReworkFromEntry || h.ReworkExecutionID != w.execution || h.ReworkNo != 1 || h.ReworkUnitNo != w.unit ||
			h.ReworkAskedBy != w.asker {
			t.Fatalf("former house %d = %+v, want %s in rework 1 of %s on unit %d asked by %s", i, h, w.house, w.execution, w.unit, w.asker)
		}
	}
}

// TestTwoStuffingsGivenUpOnOneUnitAreListedByStuffingId: unit 1 is stuffed, reworked and given up, then stuffed again
// with a new stuffing, reworked and given up. Both are listed on unit 1, by stuffing id in byte order (X-C before X-b:
// the database's collation, en_US, puts X-b first, so a list that kept the database's order would fail). That holds on
// a glibc database such as the local one. CI's database is postgres:16.12-alpine (musl, code-point collation), where
// SQL also puts X-C first and the test passes with or without the tie-break: it discriminates only on a glibc database.
func TestTwoStuffingsGivenUpOnOneUnitAreListedByStuffingId(t *testing.T) {
	ctx := context.Background()
	f := reworkDay(t)
	for i, x := range []struct {
		id, house, asker string
		hour             int
		packs            int64
	}{{"X-b", "H01", "ops-a", 4, 4}, {"X-C", "H02", "ops-b", 9, 6}} {
		at := f.hoursIn(x.hour)
		f.beginAt(t, x.id, 1, at)
		ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load-"+x.id, x.id, 2, "L-"+x.house, x.packs, at)))
		seal := f.sealOf(x.id, 3, "SEAL-"+x.id)
		seal.OccurredAt, seal.Evidence.OccurredAt = at, at
		ok(t, 4)(f.module.CloseLoadExecution(ctx, seal))
		ok(t, 5)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-"+x.id, x.id, 4, "rolled", x.asker, f.hoursIn(x.hour+2))))
		ok(t, 6)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("unload-"+x.id, x.id, 5, x.house, f.hoursIn(x.hour+3))))
		ok(t, 7)(f.module.CancelLoadExecution(ctx, f.cancelCmd("cancel-"+x.id, x.id, 6, "rolled", f.hoursIn(x.hour+4))))
		_ = i
	}
	b := f.basis(t)
	if len(b.GivenUpStuffings) != 2 || b.GivenUpStuffings[0].ExecutionID != "X-C" || b.GivenUpStuffings[1].ExecutionID != "X-b" || b.GivenUpStuffings[0].UnitNo != 1 ||
		b.GivenUpStuffings[1].UnitNo != 1 || b.GivenUpStuffings[0].Reworks[0].AskedBy != "ops-b" || b.GivenUpStuffings[1].Reworks[0].AskedBy != "ops-a" {
		t.Fatalf("given-up stuffings: %+v", b.GivenUpStuffings)
	}
}

// TestAHouseUnloadedByAReworkAndDetachedAfterTheConsolidationReopenedNamesTheReworkFromItsReversals is the review's
// case: every house is unloaded in rework 1 (asked by ops-asker), the emptied stuffing is given up, the consolidation
// reopens with the houses still attached, and they are detached while it is OPEN. The database records no rework for an
// entry ended in an OPEN consolidation, so the entries name none; the reversal lines of each house carry the rework
// number, and the former house names it with the stuffing, its unit and its asker.
func TestAHouseUnloadedByAReworkAndDetachedAfterTheConsolidationReopenedNamesTheReworkFromItsReversals(t *testing.T) {
	ctx := context.Background()
	f := leaveDay(t)
	ok(t, 9)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("unload-h02", "X-1", 8, "H02", f.hoursIn(8))))
	ok(t, 10)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("unload-h01", "X-1", 9, "H01", f.hoursIn(9))))
	ok(t, 11)(f.module.CancelLoadExecution(ctx, f.cancelCmd("cancel-x1", "X-1", 10, "every house is unloaded", f.hoursIn(10))))
	ok(t, 5)(f.module.ReopenConsolidation(ctx, api.ReopenConsolidation{CommandID: "reopen", TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: "C-S",
		ExpectedRevision: 4, Reason: "every house is out of the container", OccurredAt: f.hoursIn(11)}))
	for _, h := range []string{"H01", "H02", "H03"} {
		ok(t, 2)(f.module.DetachHouse(ctx, f.detach("detach-"+h, "C-S", h, 1, api.ReasonRollover, "", f.hoursIn(12))))
		if e := f.membership(t, h).History[0]; e.ReworkNo != 0 || e.ReworkExecutionID != "" {
			t.Fatalf("%s: an entry ended while the consolidation was OPEN names no rework: %+v", h, e)
		}
	}
	b := f.basis(t)
	if len(b.FormerHouses) != 3 || len(b.GivenUpStuffings) != 1 {
		t.Fatalf("former houses %+v, given up %+v", b.FormerHouses, b.GivenUpStuffings)
	}
	for _, h := range b.FormerHouses {
		entered(t, h)
		if h.End != api.FormerDetached || h.ReworkFrom != api.ReworkFromReversal || h.ReworkExecutionID != "X-1" || h.ReworkNo != 1 || h.ReworkUnitNo != 1 ||
			h.ReworkAskedBy != "ops-asker" {
			t.Fatalf("former house %+v: want rework 1 of X-1 on unit 1 asked by ops-asker, from the reversals", h)
		}
	}
}

// TestAHouseUnloadedByAReworkLoadedAgainAndCancelledNamesTheReworkThatUnloadedIt: H03 is unloaded in rework 1
// (ops-asker) and the container sealed again; H03 is then loaded in a second stuffing X-2 and taken out of it by an
// ordinary reversal, and its shipment is cancelled. The cancellation names no rework, and the former house names rework
// 1: the latest rework that unloaded it, though it was loaded again afterwards and left by an ordinary reversal.
func TestAHouseUnloadedByAReworkLoadedAgainAndCancelledNamesTheReworkThatUnloadedIt(t *testing.T) {
	ctx := context.Background()
	f := leaveDay(t)
	f.reseal(t, 8)
	f.beginAt(t, "X-2", 2, f.hoursIn(9))
	line := ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load-x2-h03", "X-2", 2, "L-H03", 2, f.hoursIn(9))))
	ok(t, 4)(f.module.RecordCargoUnloaded(ctx, api.RecordCargoUnloaded{CommandID: "unload-x2-h03", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-2",
		ExpectedRevision: 3, LineNo: line.ItemNo, Packs: 2, Reason: api.ReversalMisloaded, Evidence: evidence("unload-x2-h03", f.hoursIn(10)), OccurredAt: f.hoursIn(10)}))
	f.cancelShipment(t, "cancel-h03", "C-S", "H03", "ops-9", 1, f.hoursIn(11))
	b := f.basis(t)
	h := formerOf(t, b, "H03")
	entered(t, h)
	want := api.FormerHouse{ShipmentID: "H03", End: api.FormerCancelled, Reason: api.ReasonShipmentCancelled, EndedBy: "ops-9", EndedAt: f.hoursIn(11),
		EndedEnteredAt: h.EndedEnteredAt, ReworkFrom: api.ReworkFromReversal, ReworkExecutionID: "X-1", ReworkNo: 1, ReworkUnitNo: 1, ReworkAskedBy: "ops-asker"}
	if len(b.FormerHouses) != 1 || !reflect.DeepEqual(h, want) {
		t.Fatalf("former houses %+v, want %+v", b.FormerHouses, want)
	}
}

// reopenedAfterAReworkEmptiedIt is the review's day up to the reopening: every house is unloaded in rework 1 of X-1
// (asked by ops-asker), the emptied stuffing is given up and C-S reopens with H01, H02 and H03 still attached (C-S at
// revision 5, each ledger at revision 1).
func reopenedAfterAReworkEmptiedIt(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	f := leaveDay(t)
	ok(t, 9)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("unload-h02", "X-1", 8, "H02", f.hoursIn(8))))
	ok(t, 10)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("unload-h01", "X-1", 9, "H01", f.hoursIn(9))))
	ok(t, 11)(f.module.CancelLoadExecution(ctx, f.cancelCmd("cancel-x1", "X-1", 10, "every house is unloaded", f.hoursIn(10))))
	ok(t, 5)(f.module.ReopenConsolidation(ctx, api.ReopenConsolidation{CommandID: "reopen", TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: "C-S",
		ExpectedRevision: 4, Reason: "every house is out of the container", OccurredAt: f.hoursIn(11)}))
	return f
}

// TestTheReworkOfAFormerHouseIsBoundedToItsLatestEntry: H03 was unloaded in rework 1 of X-1, detached from the reopened
// consolidation (the rework found in the reversals), attached again and detached again with nothing loaded in between.
// The second departure names no rework: the reversals of the earlier stay do not count for the latest entry.
func TestTheReworkOfAFormerHouseIsBoundedToItsLatestEntry(t *testing.T) {
	ctx := context.Background()
	f := reopenedAfterAReworkEmptiedIt(t)
	ok(t, 2)(f.module.DetachHouse(ctx, f.detach("detach-h03", "C-S", "H03", 1, api.ReasonRollover, "", f.hoursIn(12))))
	if h := formerOf(t, f.basis(t), "H03"); h.ReworkFrom != api.ReworkFromReversal || h.ReworkExecutionID != "X-1" || h.ReworkNo != 1 {
		t.Fatalf("the first departure names rework 1 of X-1: %+v", h)
	}
	back := f.attach("attach-again", "C-S", "H03", 2)
	back.OccurredAt = f.hoursIn(13)
	ok(t, 3)(f.module.AttachHouse(ctx, back))
	ok(t, 4)(f.module.DetachHouse(ctx, f.detach("detach-again", "C-S", "H03", 3, api.ReasonCustomerRequest, "", f.hoursIn(14))))
	h := formerOf(t, f.basis(t), "H03")
	if h.End != api.FormerDetached || h.Reason != api.ReasonCustomerRequest || h.ReworkFrom != api.ReworkFromNone || h.ReworkExecutionID != "" || h.ReworkNo != 0 ||
		h.ReworkUnitNo != 0 || h.ReworkAskedBy != "" {
		t.Fatalf("the second departure, with nothing loaded since the first: %+v", h)
	}
}

// TestAHouseAttachedAgainLoadedAndUnloadedByASecondReworkNamesTheSecond: after the reopening H03 is detached, attached
// again, the consolidation closed again, H03 loaded in a new stuffing X-2, sealed, unloaded in rework 1 of X-2 (asked
// by ops-second), and its shipment cancelled. The cancellation names no rework, and the former house names X-2's rework
// 1, not X-1's, which is older than the entry.
func TestAHouseAttachedAgainLoadedAndUnloadedByASecondReworkNamesTheSecond(t *testing.T) {
	ctx := context.Background()
	f := reopenedAfterAReworkEmptiedIt(t)
	ok(t, 2)(f.module.DetachHouse(ctx, f.detach("detach-h03", "C-S", "H03", 1, api.ReasonRollover, "", f.hoursIn(12))))
	back := f.attach("attach-again", "C-S", "H03", 2)
	back.OccurredAt = f.hoursIn(13)
	ok(t, 3)(f.module.AttachHouse(ctx, back))
	f.ful.allocate("H03", "C-S")
	closeAgain := f.closeCmd("close-again", "C-S", f.consolidation(t, "C-S").Revision)
	closeAgain.OccurredAt = f.hoursIn(14)
	if _, err := f.module.CloseConsolidation(ctx, closeAgain); err != nil {
		t.Fatal(err)
	}
	f.beginAt(t, "X-2", 1, f.hoursIn(15))
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load-x2-h03", "X-2", 2, "L-H03", 2, f.hoursIn(15))))
	seal := f.sealOf("X-2", 3, "SEAL-X-2")
	seal.OccurredAt, seal.Evidence.OccurredAt = f.hoursIn(16), f.hoursIn(16)
	ok(t, 4)(f.module.CloseLoadExecution(ctx, seal))
	ok(t, 5)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-x2", "X-2", 4, "H03 again", "ops-second", f.hoursIn(17))))
	ok(t, 6)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("unload-x2", "X-2", 5, "H03", f.hoursIn(18))))
	f.cancelShipment(t, "cancel-h03", "C-S", "H03", "ops-9", 3, f.hoursIn(19))
	h := formerOf(t, f.basis(t), "H03")
	if h.End != api.FormerCancelled || h.ReworkFrom != api.ReworkFromReversal || h.ReworkExecutionID != "X-2" || h.ReworkNo != 1 || h.ReworkUnitNo != 1 ||
		h.ReworkAskedBy != "ops-second" {
		t.Fatalf("the second stay's rework: %+v", h)
	}
}
