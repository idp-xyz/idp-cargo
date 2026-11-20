//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	phymodule "go.idp.xyz/idp-cargo/internal/contexts/phy"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// These tests are the corrections of V2.83 after its independent review: the stuffings a command did not address are
// held only when the command raised their cargo, a receipt is checked as well, the other stuffings are locked without
// waiting but a sealed one never, and a house with lines in two weight units is published by its amounts added.

// newRunOf is newSplitRun with units containers of the 22G1 type, a house that plans planned packs, and a lot L-M of
// received packs weighed at kilos and volume (volume.Amount 0: none), completed or still receiving. The lot is at
// revision 2, or 3 when completed.
func newRunOf(t *testing.T, units int, planned, received, kilos int64, volume kernel.Quantity, complete bool) *splitRun {
	t.Helper()
	ctx := context.Background()
	f := newFixture(t)
	f.ful.sea["SH-M"] = true
	f.ful.packs["SH-M"] = planned
	f.createSea(t, "C-S", api.KindSeaLCLOwn)
	for i := 1; i <= units; i++ {
		ok(t, int64(i)+1)(f.module.PlanLoadUnit(ctx, f.planUnit(fmt.Sprint("plan-", i), "C-S", int64(i), "22G1")))
	}
	if _, err := f.module.AttachHouse(ctx, f.attach("attach", "C-S", "SH-M", 0)); err != nil {
		t.Fatal(err)
	}
	f.ful.allocate("SH-M", "C-S")
	ok(t, int64(units)+2)(f.module.CloseConsolidation(ctx, f.closeCmd("close", "C-S", int64(units)+1)))
	at := f.hoursIn(3)
	rcv := f.receive("rcv", "L-M", "SH-M", 0, received, api.ConditionGood, at)
	rcv.Facility = cfsShanghai
	ok(t, 1)(f.module.ReceiveCargo(ctx, rcv))
	m := api.RecordCargoMeasurement{CommandID: "measure", TenantID: f.tenant, ActorID: "ops-1", LotID: "L-M", ExpectedRevision: 1, GrossWeight: kgm(kilos),
		Method: api.MethodScale, Evidence: evidence("measure", at), OccurredAt: at}
	if volume.Amount > 0 {
		m.Volume = volume
	}
	ok(t, 2)(f.module.RecordCargoMeasurement(ctx, m))
	s := &splitRun{t: t, f: f, rev: map[string]int64{}, lot: 2}
	if complete {
		ok(t, 3)(f.module.CompleteReceipt(ctx, api.CompleteReceipt{CommandID: "complete", TenantID: f.tenant, ActorID: "ops-1", LotID: "L-M", ExpectedRevision: 2,
			OccurredAt: at}))
		s.lot = 3
	}
	return s
}

// cfsTwo is a second place in Shanghai: a shipment has one lot in force at a facility, so a second lot of the house is
// received there (a facility is not checked when a lot is loaded).
var cfsTwo = api.Facility{Location: unlocode("CNSHG"), Code: "CFS-2"}

// receiveLot receives packs of a lot of the house SH-M at the second CFS and weighs it, unless kilos is 0; the lot is
// at revision 1, or 2 once weighed.
func (s *splitRun) receiveLot(lot string, packs, kilos int64) {
	s.t.Helper()
	ctx, at := context.Background(), s.f.hoursIn(3)
	rcv := s.f.receive("rcv-"+lot, lot, "SH-M", 0, packs, api.ConditionGood, at)
	rcv.Facility = cfsTwo
	ok(s.t, 1)(s.f.module.ReceiveCargo(ctx, rcv))
	if kilos > 0 {
		ok(s.t, 2)(s.f.module.RecordCargoMeasurement(ctx, api.RecordCargoMeasurement{CommandID: "measure-" + lot, TenantID: s.f.tenant, ActorID: "ops-1",
			LotID: lot, ExpectedRevision: 1, GrossWeight: kgm(kilos), Method: api.MethodScale, Evidence: evidence("measure-"+lot, at), OccurredAt: at}))
	}
}

// loadAs loads packs of a lot into a stuffing as the actor at the hour, weighed when kilos is given, and keeps the
// revision of the stuffing; it returns the receipt's item.
func (s *splitRun) loadAs(actor, x, lot string, packs, kilos, cm3 int64, hour int) int {
	s.t.Helper()
	cur := s.f.execution(s.t, x).Revision
	cmd := s.f.load(s.id("load"), x, cur, lot, packs, s.f.hoursIn(hour))
	cmd.ActorID = actor
	if kilos > 0 {
		cmd.GrossWeight, cmd.Volume = kgm(kilos), cmq(cm3)
	}
	r, err := s.f.module.RecordCargoLoaded(context.Background(), cmd)
	if err != nil {
		s.t.Fatalf("load of %d packs of %s into %s: %v", packs, lot, x, err)
	}
	s.rev[x] = r.NewRevision
	return r.ItemNo
}

// release releases the hold of the stuffing, with the evidence of a person who looked at the container's plate.
func (s *splitRun) release(x string, no int) {
	s.t.Helper()
	cur := s.f.execution(s.t, x).Revision
	ok(s.t, cur+1)(s.f.module.ReleaseLoadExecutionHold(context.Background(), api.ReleaseLoadExecutionHold{CommandID: s.id("release"), TenantID: s.f.tenant,
		ActorID: "releaser-1", ExecutionID: x, ExpectedRevision: cur, HoldNo: no, Evidence: evidence(s.id("plate"), s.f.hoursIn(4)), OccurredAt: s.f.hoursIn(4)}))
	s.rev[x] = cur + 1
}

// holds says the stuffing is in the state with n holds placed in all, and that the events of holds in the outbox that
// name it are as many.
func (s *splitRun) holds(x, state string, n int) {
	s.t.Helper()
	got, placed := s.f.execution(s.t, x), s.f.placed(s.t, x)
	if got.State != state || len(placed) != n || s.f.heldEvents(s.t, x) != n {
		s.t.Errorf("%s is %s with %d holds and %d held events, want %s and %d of each: %+v", x, got.State, len(placed), s.f.heldEvents(s.t, x), state, n, placed)
	}
}

// TestAReleasedHoldDoesNotComeBackFromAnotherContainersLoad is the review's case (F2). A lot of 6 packs and 60,000 kg,
// 3 apportioned packs in container 1: 60,000 x 3/6 = 30,000 kg, with the tare 32,250 kg over the 30,480 kg: the load
// holds it, and a person releases the hold.
//   - Container 2 takes 1 apportioned pack: the 4 apportioned packs take 60,000 x 4/6 = 40,000 kg, 30,000 + 10,000:
//     container 1's figure stays at 30,000 kg, which the load did not raise, so it is not held again.
//   - Container 2 takes 2 more packs, weighed for themselves at 1,000 kg: the 4 apportioned packs complete the lot and
//     take 60,000 - 1,000 = 59,000 kg, 44,250 + 14,750: container 1's figure rose to 44,250 kg and it is held again, by
//     the actor and evidence of that load.
//
// And the stuffing a load goes into is held when it is over, whether or not the load raised its figure: a lot of 2
// packs and 1 kg, 1 of its packs apportioned into container 1 takes 1 x 1/2 = 0.5 kg, rounded down to nothing, and
// container 1 is held again by it at 30,000 kg as before.
func TestAReleasedHoldDoesNotComeBackFromAnotherContainersLoad(t *testing.T) {
	s := newSplitRun(t, 60000, 600)
	s.open("X-1", 1)
	s.open("X-2", 2)
	s.loadAs("ops-1", "X-1", "L-M", 3, 0, 0, 4)
	s.holds("X-1", api.ExecutionOnHold, 1)
	s.release("X-1", 1)
	s.holds("X-1", api.ExecutionLoading, 1)
	s.loadAs("ops-1", "X-2", "L-M", 1, 0, 0, 4)
	s.holds("X-1", api.ExecutionLoading, 1)
	if l := s.limits(1); l.CargoGrams != 30000000 || !l.OverMaxGross {
		t.Errorf("container 1 reads %+v; it is over still, and unchanged", l)
	}
	s.loadAs("loader-2", "X-2", "L-M", 2, 1000, 100, 5)
	s.holds("X-1", api.ExecutionOnHold, 2)
	if h := s.f.nthHold(t, "X-1", 2); h.By != "loader-2" || !strings.Contains(h.Text, "tare and cargo 46500.000 kg") {
		t.Errorf("the second hold: %+v", h)
	}
	s.holds("X-2", api.ExecutionLoading, 0)

	s = newSplitRun(t, 60000, 600)
	s.open("X-1", 1)
	s.open("X-2", 2)
	s.receiveLot("L-N", 2, 1)
	s.loadAs("ops-1", "X-1", "L-M", 3, 0, 0, 4)
	s.release("X-1", 1)
	s.loadAs("loader-3", "X-1", "L-N", 1, 0, 0, 5)
	s.holds("X-1", api.ExecutionOnHold, 2)
	if h := s.f.nthHold(t, "X-1", 2); h.By != "loader-3" || s.limits(1).CargoGrams != 30000000 {
		t.Errorf("the second hold: %+v, figure %+v", h, s.limits(1))
	}
}

// TestAReleasedHoldDoesNotComeBackFromAnUnload: the same for an unload, the stuffing unloaded from included. A lot of 6
// packs and 100,000 kg, 3 apportioned packs in container 1: 50,000 kg, held and released.
//   - Container 2 takes 2 apportioned packs: 5 of 6 packs take 83,333 kg, 3/5 of it 49,999.8, 2/5 33,333.2, the floors
//     49,999 + 33,333 leave 1 kg for the larger remainder (.8): 50,000 + 33,333. Container 1 is where it was, not held.
//   - One of container 2's packs is unloaded: 4 of 6 take 66,666 kg, 49,999.5 and 16,666.5, the floors leave 1 kg,
//     equal remainders go to the lower unit: 50,000 + 16,666. Container 1 is where it was, not held.
//   - One of container 1's own packs is unloaded: 3 of 6 take 50,000 kg, 2/3 of it 33,333: container 1 fell and is over
//     still (35,583 kg with the tare), but it is not held again.
//
// And an unload that does raise a figure holds: 2 packs weighed at 40,000 kg in container 2, 3 apportioned in container
// 1, which take (100,000 - 40,000) x 3/4 = 45,000 kg, held and released. The weighed packs are unloaded: the 3
// apportioned packs take 100,000 x 3/6 = 50,000 kg, container 1 is held again by the actor and evidence of the unload.
func TestAReleasedHoldDoesNotComeBackFromAnUnload(t *testing.T) {
	s := newSplitRun(t, 100000, 600)
	s.open("X-1", 1)
	s.open("X-2", 2)
	line1 := s.loadAs("ops-1", "X-1", "L-M", 3, 0, 0, 4)
	s.release("X-1", 1)
	line2 := s.loadAs("ops-1", "X-2", "L-M", 2, 0, 0, 4)
	s.holds("X-1", api.ExecutionLoading, 1)
	s.holds("X-2", api.ExecutionOnHold, 1)
	s.unload("X-2", line2, 1)
	s.holds("X-1", api.ExecutionLoading, 1)
	if l := s.limits(1); l.CargoGrams != 50000000 {
		t.Errorf("container 1 reads %+v", l)
	}
	s.unload("X-1", line1, 1)
	s.holds("X-1", api.ExecutionLoading, 1)
	if l := s.limits(1); l.CargoGrams != 33333000 || !l.OverMaxGross {
		t.Errorf("container 1 reads %+v; it fell and is over still", l)
	}

	s = newSplitRun(t, 100000, 600)
	s.open("X-1", 1)
	s.open("X-2", 2)
	weighed := s.loadAs("ops-1", "X-2", "L-M", 2, 40000, 100, 4)
	s.loadAs("ops-1", "X-1", "L-M", 3, 0, 0, 4)
	s.holds("X-1", api.ExecutionOnHold, 1)
	s.release("X-1", 1)
	at := s.f.hoursIn(5)
	cur := s.f.execution(t, "X-2").Revision
	ok(t, cur+1)(s.f.module.RecordCargoUnloaded(context.Background(), api.RecordCargoUnloaded{CommandID: "unload-by-3", TenantID: s.f.tenant, ActorID: "unloader-3",
		ExecutionID: "X-2", ExpectedRevision: cur, LineNo: weighed, Packs: 2, Reason: api.ReversalMisloaded, Evidence: evidence("tally-3", at), OccurredAt: at}))
	s.holds("X-1", api.ExecutionOnHold, 2)
	if h := s.f.nthHold(t, "X-1", 2); h.By != "unloader-3" || h.Reference != "tally-3" || s.limits(1).CargoGrams != 50000000 {
		t.Errorf("the second hold: %+v, figure %+v", h, s.limits(1))
	}
}

// TestAReceiptHoldsAContainerItPushesOverAndNotOneItDoesNot is F4. A lot of 38 packs weighed at 200 kg and 213 m3 (MTQ,
// the plan's volume unit is free) and 1 pack of a second lot weighed for itself at 1 kg and 16,500,000 cm3, in
// container 1; 3, 5 and 5 packs of the first lot are apportioned in containers 1, 2 and 3 (container 1 last: with 3 and
// 5 packs loaded the tie of the remainders would give it 17 m3 already). The 13 apportioned packs take 213 x 13/38 =
// 72.9 m3, rounded down to 72, divided 72 x 3/13 = 16.6, 72 x 5/13 = 27.7 twice: the floors 16 + 27 + 27 leave 2 m3 for
// the larger remainders (.69 twice): 16, 28 and 28 m3. Container 1 reads 16 + 16.5 = 32.5 m3 of the 33.2 m3 of a 22G1.
// A 39th pack is received: 213 x 13/39 = 71 m3, 16.4, 27.3 twice, the floors 16 + 27 + 27 leave 1 m3 for the larger
// remainder (.38 against .31): 17, 27 and 27 m3. Container 1 reads 33.5 m3, over the capacity, and is held by the
// command of the receipt, as the review said one unit of the measure is a cubic metre when the lot's volume is in MTQ.
// Containers 2 and 3 fell and are not held.
func TestAReceiptHoldsAContainerItPushesOver(t *testing.T) {
	s := newRunOf(t, 3, 40, 38, 200, kernel.Quantity{Unit: "MTQ", Amount: 213}, false)
	s.open("X-1", 1)
	s.open("X-2", 2)
	s.open("X-3", 3)
	s.receiveLot("L-N", 1, 0)
	s.loadAs("ops-1", "X-2", "L-M", 5, 0, 0, 4)
	s.loadAs("ops-1", "X-3", "L-M", 5, 0, 0, 4)
	s.loadAs("ops-1", "X-1", "L-M", 3, 0, 0, 4)
	s.loadAs("ops-1", "X-1", "L-N", 1, 1, 16500000, 4)
	for _, x := range []string{"X-1", "X-2", "X-3"} {
		s.holds(x, api.ExecutionLoading, 0)
	}
	if a, b, c := s.limits(1), s.limits(2), s.limits(3); a.CargoCubicCm != 32500000 || b.CargoCubicCm != 28000000 || c.CargoCubicCm != 28000000 || a.OverCapacity {
		t.Fatalf("limits before the receipt: %+v %+v %+v", a, b, c)
	}
	at := s.f.hoursIn(5)
	more := s.f.receive("rcv-more", "L-M", "SH-M", 2, 1, api.ConditionGood, at)
	more.Facility, more.ActorID = cfsShanghai, "receiver-1"
	events := s.f.heldEvents(t, "")
	r := ok(t, 3)(s.f.module.ReceiveCargo(context.Background(), more))
	if r.ItemNo != 2 {
		t.Fatalf("receipt: %+v", r)
	}
	s.holds("X-1", api.ExecutionOnHold, 1)
	s.holds("X-2", api.ExecutionLoading, 0)
	s.holds("X-3", api.ExecutionLoading, 0)
	h := s.f.nthHold(t, "X-1", 1)
	if age := time.Since(h.EnteredAt); h.EnteredAt.IsZero() || age < 0 || age > 5*time.Minute {
		t.Errorf("the hold was entered at %v: it is stamped with the time the receipt was entered", h.EnteredAt)
	}
	if h.By != "receiver-1" || h.Reference != "rcv-more" || !h.EvidenceAt.Equal(at) || !strings.Contains(h.Text, "cargo 33.500000 m3 over the default capacity 33.200000 m3") ||
		s.f.heldEvents(t, "") != events+1 {
		t.Errorf("the hold: %+v", h)
	}
	if a, b := s.limits(1), s.limits(2); a.CargoCubicCm != 33500000 || !a.OverCapacity || b.CargoCubicCm != 27000000 {
		t.Errorf("limits after the receipt: %+v %+v", a, b)
	}
}

// A receipt does not hold again a container whose hold a person released and whose figure it lowered: a lot of 6 packs
// and 100,000 kg, 3 apportioned packs in container 1 (50,000 kg, held, released); a seventh pack is received and the 3
// packs take 100,000 x 3/7 = 42,857 kg, over still.
func TestAReceiptDoesNotHoldAgainAContainerWhoseFigureFell(t *testing.T) {
	s := newRunOf(t, 2, 12, 6, 100000, kernel.Quantity{Unit: "CMQ", Amount: 600}, false)
	s.open("X-1", 1)
	s.open("X-2", 2)
	s.loadAs("ops-1", "X-1", "L-M", 3, 0, 0, 4)
	s.holds("X-1", api.ExecutionOnHold, 1)
	s.release("X-1", 1)
	more := s.f.receive("rcv-more", "L-M", "SH-M", 2, 1, api.ConditionGood, s.f.hoursIn(5))
	more.Facility = cfsShanghai
	ok(t, 3)(s.f.module.ReceiveCargo(context.Background(), more))
	s.holds("X-1", api.ExecutionLoading, 1)
	if l := s.limits(1); l.CargoGrams != 42857000 || !l.OverMaxGross {
		t.Errorf("container 1 reads %+v", l)
	}
}

// A receipt of a lot no container holds, and one whose containers are sealed, locks no stuffing and conflicts with
// nothing: a sealed stuffing is read, never locked, because it is never written. Container 1 is sealed with 3 of the
// lot's 6 packs, and its row is held on a second connection while container 2 takes packs, the lot is received more and
// packs are unloaded from container 2.
func TestASealedStuffingIsReadAndNeverLocked(t *testing.T) {
	s := newRunOf(t, 2, 12, 6, 50000, kernel.Quantity{Unit: "CMQ", Amount: 600}, false)
	s.open("X-1", 1)
	s.open("X-2", 2)
	s.loadAs("ops-1", "X-1", "L-M", 3, 0, 0, 4)
	s.seal("X-1")
	_, release := s.f.holdRow(t, lockExecution, s.f.tenant, "X-1")
	defer release()
	line := 0
	if _, err := quickly(t, func(ctx context.Context) (api.Receipt, error) {
		cmd := s.f.load("load-2", "X-2", s.rev["X-2"], "L-M", 2, s.f.hoursIn(5))
		r, err := s.f.module.RecordCargoLoaded(ctx, cmd)
		line = r.ItemNo
		return r, err
	}); err != nil {
		t.Fatalf("a load while the sealed container's row is locked: %v", err)
	}
	s.rev["X-2"]++
	more := s.f.receive("rcv-more", "L-M", "SH-M", 2, 1, api.ConditionGood, s.f.hoursIn(5))
	more.Facility = cfsShanghai
	if _, err := quickly(t, func(ctx context.Context) (api.Receipt, error) { return s.f.module.ReceiveCargo(ctx, more) }); err != nil {
		t.Fatalf("a receipt while the sealed container's row is locked: %v", err)
	}
	if _, err := quickly(t, func(ctx context.Context) (api.Receipt, error) {
		return s.f.module.RecordCargoUnloaded(ctx, api.RecordCargoUnloaded{CommandID: "unload-2", TenantID: s.f.tenant, ActorID: "ops-1", ExecutionID: "X-2",
			ExpectedRevision: s.rev["X-2"], LineNo: line, Packs: 1, Reason: api.ReversalMisloaded, Evidence: evidence("tally", s.f.hoursIn(5)), OccurredAt: s.f.hoursIn(5)})
	}); err != nil {
		t.Fatalf("an unload while the sealed container's row is locked: %v", err)
	}
}

// TestAThirdContainerBusyMakesTheLoadConflictAndHoldsNothing: three containers, a lot of 6 packs and 50,000 kg. 3 packs
// apportioned in container 1 (25,000 kg) and 1 in container 2; container 3 then takes 2 packs weighed at 1,000 kg,
// which leaves 49,000 kg for the 4 apportioned packs: container 1 3/4 of it, 36,750 kg, over (raised from 25,000),
// container 2 12,250 kg. With container 2's row held on a second connection (container 1 free), the load into container
// 3 answers the conflict at once, names container 2, writes nothing, and container 1 is not held either: the set is
// locked whole or not at all. Released, the same command holds container 1 and not container 2.
func TestAPartlyBusySetOfStuffingsMakesTheLoadConflict(t *testing.T) {
	s := newRunOf(t, 3, 6, 6, 50000, kernel.Quantity{Unit: "CMQ", Amount: 600}, true)
	s.f.withoutRetry(t) // V2.84 retries a busy stuffing; this test is of the answer when no retry is allowed
	s.open("X-1", 1)
	s.open("X-2", 2)
	s.open("X-3", 3)
	s.loadAs("ops-1", "X-1", "L-M", 3, 0, 0, 4)
	s.loadAs("ops-1", "X-2", "L-M", 1, 0, 0, 4)
	events := len(s.f.outboxTypes(t))
	_, release := s.f.holdRow(t, lockExecution, s.f.tenant, "X-2")
	defer release()
	cur := s.f.execution(t, "X-3").Revision
	cmd := s.f.load("load-3", "X-3", cur, "L-M", 2, s.f.hoursIn(5))
	cmd.GrossWeight, cmd.Volume = kgm(1000), cmq(100)
	_, err := quickly(t, func(ctx context.Context) (api.Receipt, error) { return s.f.module.RecordCargoLoaded(ctx, cmd) })
	if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "X-2") || strings.Contains(err.Error(), "X-1") {
		t.Fatalf("the load while one of the two other containers is busy: %v, want the conflict naming X-2 only", err)
	}
	if x := s.f.execution(t, "X-3"); x.Revision != cur || len(x.Lines) != 0 || len(s.f.outboxTypes(t)) != events {
		t.Fatalf("the refused load wrote: %+v", x)
	}
	s.holds("X-1", api.ExecutionLoading, 0)
	release()
	ok(t, cur+1)(s.f.module.RecordCargoLoaded(context.Background(), cmd))
	s.holds("X-1", api.ExecutionOnHold, 1)
	s.holds("X-2", api.ExecutionLoading, 0)
	s.holds("X-3", api.ExecutionLoading, 0)
}

// TestTheCheckReadsTheLotsOfTheContainerThatWereNotChanged: a container holds two lots and the command changes one. Lot
// L-N, 2 packs weighed at 40,000 kg, 1 pack apportioned in container 1: 20,000 kg; weighed again at 50,000 kg: 25,000
// kg, within the 28,230 kg. Then 1 pack of lot L-M (6 packs, 24,000 kg) is loaded into container 1: 4,000 kg, which
// make 29,000 kg, over. The load changes L-M only; the figure it checks is the one of L-N's latest measure (25,000 kg),
// not the 20,000 kg its line was recorded at.
func TestTheCheckReadsTheLotsOfTheContainerThatWereNotChanged(t *testing.T) {
	s := newSplitRun(t, 24000, 600)
	s.open("X-1", 1)
	s.receiveLot("L-N", 2, 40000)
	ctx, at := context.Background(), s.f.hoursIn(4)
	ok(t, 3)(s.f.module.RecordCargoLoaded(ctx, s.f.load("load-n", "X-1", 2, "L-N", 1, at)))
	s.rev["X-1"] = 3
	ok(t, 3)(s.f.module.RecordCargoMeasurement(ctx, api.RecordCargoMeasurement{CommandID: "measure-n-again", TenantID: s.f.tenant, ActorID: "ops-1", LotID: "L-N",
		ExpectedRevision: 2, GrossWeight: kgm(50000), Method: api.MethodScale, Evidence: evidence("measure-n-again", at), OccurredAt: at}))
	s.holds("X-1", api.ExecutionLoading, 0)
	s.rev["X-1"] = 3
	s.loadAs("ops-1", "X-1", "L-M", 1, 0, 0, 4)
	s.holds("X-1", api.ExecutionOnHold, 1)
	if h := s.f.nthHold(t, "X-1", 1); !strings.Contains(h.Text, "tare and cargo 31250.000 kg") {
		t.Errorf("the hold: %+v", h)
	}
}

// unitFUL is the stub of FUL's load intents with the plan's weight unit changed after a point in the test: the house's
// plan, and so the lots opened after it, are in another unit.
type unitFUL struct {
	*fulStub
	mu   sync.Mutex
	unit kernel.UnitCode
}

func (u *unitFUL) set(unit kernel.UnitCode) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.unit = unit
}

func (u *unitFUL) GetLoadIntent(ctx context.Context, q fulapi.GetShipmentPlanSnapshot) (fulapi.LoadIntent, error) {
	i, err := u.fulStub.GetLoadIntent(ctx, q)
	u.mu.Lock()
	defer u.mu.Unlock()
	if err == nil && u.unit != "" {
		i.GrossWeight.Unit = u.unit
		for k := range i.Parts {
			i.Parts[k].GrossWeight.Unit = u.unit
		}
	}
	return i, err
}

// TestAHouseWithLinesInTwoWeightUnitsIsPublishedInGrams is the latent defect found by the review of V2.83 (F1 d) and
// closed by V2.84. HouseMeasure added the amounts of a house's lines under the unit of the last one, without converting
// them, and PHY has no guard that a lot's measurement unit equals the unit of the house's other lots (a lot's
// measurement must be in the unit of the plan the lot was opened under, CargoLot.Measure, domain/lot.go; an apportioned
// line takes its lot's measurement unit; a line weighed for itself must be in the unit of the house's live entry). The
// case here is built with a stand-in for FUL (unitFUL), which changes the plan's weight unit between two lots'
// receipts: FUL as built refuses the change of unit that would be needed, so this repository's FUL does not send PHY
// such a plan (run against the real FUL and COM in V2.84: an obligation in another unit is refused by
// AddObligationToShipment, a part in another unit than its obligation too, and the last obligation cannot be released).
// Lot L-A (3 packs, 3,000 KGM) is opened under a plan in KGM and L-B (3 packs, 3 TNE) under a plan in TNE, both loaded
// apportioned into container 1. By hand: 3,000 KGM = 3,000 x 1,000 = 3,000,000 g and 3 TNE = 3 x 1,000,000 = 3,000,000
// g, 6,000,000 g in all. V2.83 published 3,003 TNE (3,000 + 3 under the last lot's unit); the house is now published as
// 6,000,000 GRM in container 1's entry, in the house's total, in the unit's packing-list line (and so in its digest)
// and in the shipment's physical facts, and the limits check, which always converted, still reads 6,000,000 g. What FIN
// makes of the figure is the same conversion it applies to a weight (GRM, KGM and TNE): 6,000,000 g.
func TestAHouseWithLinesInTwoWeightUnitsIsPublishedInGrams(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	u := &unitFUL{fulStub: f.ful}
	module, err := phymodule.New(f.db, u, policy{}, phymodule.WithLocations(locationStub{}), phymodule.WithEquipment(equipmentStub{}))
	if err != nil {
		t.Fatal(err)
	}
	f.module = module
	f.ful.sea["SH-M"] = true
	f.createSea(t, "C-S", api.KindSeaLCLOwn)
	ok(t, 2)(f.module.PlanLoadUnit(ctx, f.planUnit("plan-1", "C-S", 1, "22G1")))
	if _, err = f.module.AttachHouse(ctx, f.attach("attach", "C-S", "SH-M", 0)); err != nil {
		t.Fatal(err)
	}
	f.ful.allocate("SH-M", "C-S")
	ok(t, 3)(f.module.CloseConsolidation(ctx, f.closeCmd("close", "C-S", 2)))
	s := &splitRun{t: t, f: f, rev: map[string]int64{}, lot: 2}
	at := f.hoursIn(3)
	for _, lot := range []struct {
		id       string
		unit     kernel.UnitCode
		amount   int64
		facility api.Facility
	}{{"L-A", "KGM", 3000, cfsShanghai}, {"L-B", "TNE", 3, cfsTwo}} {
		u.set(lot.unit)
		rcv := f.receive("rcv-"+lot.id, lot.id, "SH-M", 0, 3, api.ConditionGood, at)
		rcv.Facility = lot.facility
		ok(t, 1)(f.module.ReceiveCargo(ctx, rcv))
		ok(t, 2)(f.module.RecordCargoMeasurement(ctx, api.RecordCargoMeasurement{CommandID: "measure-" + lot.id, TenantID: f.tenant, ActorID: "ops-1", LotID: lot.id,
			ExpectedRevision: 1, GrossWeight: kernel.Quantity{Unit: lot.unit, Amount: lot.amount}, Method: api.MethodScale, Evidence: evidence("measure-"+lot.id, at), OccurredAt: at}))
	}
	s.open("X-1", 1)
	s.loadAs("ops-1", "X-1", "L-A", 3, 0, 0, 4)
	s.loadAs("ops-1", "X-1", "L-B", 3, 0, 0, 4)
	b := f.basis(t)
	var unit api.HouseUnitLoad
	var house api.HouseCostBasis
	for _, h := range b.Houses {
		if h.ShipmentID == "SH-M" && len(h.Units) == 1 {
			house, unit = h, h.Units[0]
		}
	}
	grams := kernel.Quantity{Unit: "GRM", Amount: 6000000}
	if unit.LoadedGrossWeight != grams || house.LoadedGrossWeight != grams {
		t.Errorf("the house in container 1 is published as %+v and its total as %+v; want 6,000,000 GRM", unit.LoadedGrossWeight, house.LoadedGrossWeight)
	}
	list := b.PackingLists[0]
	if len(list.Lines) != 1 || list.Lines[0].LoadedGrossWeight != grams || list.Digest != digestOf(list) {
		t.Errorf("the packing list's line is %+v (digest %s, worked out here %s); want 6,000,000 GRM", list.Lines, list.Digest, digestOf(list))
	}
	evidenceOf, err := f.module.GetPhysicalLoadEvidence(ctx, api.GetPhysicalLoadEvidence{TenantID: f.tenant, ShipmentID: "SH-M"})
	if err != nil || len(evidenceOf.Tenders) != 1 || evidenceOf.Tenders[0].GrossWeight != grams {
		t.Errorf("the physical facts read %+v: %v; want 6,000,000 GRM", evidenceOf.Tenders, err)
	}
	if l := list.Limits; !l.Checked || l.CargoGrams != 6000000 {
		t.Errorf("the packing list's Limits read %+v; the cargo is 6,000,000 g", l)
	}
	if g, err := finGrams(unit.LoadedGrossWeight); err != nil || g != 6000000 {
		t.Errorf("FIN's conversion of the entry gives %d g: %v; want 6,000,000", g, err)
	}
	s.holds("X-1", api.ExecutionLoading, 0)
}

// TestAMeasurementHoldsAgainAContainerWhoseHoldWasReleased pins the one place where a released hold can come back
// without a rise of the figure, as since V2.58: the measurement addresses the lot of every stuffing it checks. A lot of
// 6 packs and 60,000 kg, 3 apportioned packs in container 1 (30,000 kg, held, released); the lot is weighed again at
// 60,000 kg: the figure is the same and the container is held again, by the actor and evidence of the measurement.
func TestAMeasurementHoldsAgainAContainerWhoseHoldWasReleased(t *testing.T) {
	s := newSplitRun(t, 60000, 600)
	s.open("X-1", 1)
	s.open("X-2", 2)
	s.loadAs("ops-1", "X-1", "L-M", 3, 0, 0, 4)
	s.release("X-1", 1)
	at := s.f.hoursIn(5)
	ok(t, 4)(s.f.module.RecordCargoMeasurement(context.Background(), api.RecordCargoMeasurement{CommandID: "weigh-same", TenantID: s.f.tenant, ActorID: "weigher-1",
		LotID: "L-M", ExpectedRevision: s.lot, GrossWeight: kgm(60000), Volume: cmq(600), Method: api.MethodScale, Evidence: evidence("weigh-same", at), OccurredAt: at}))
	s.holds("X-1", api.ExecutionOnHold, 2)
	if h := s.f.nthHold(t, "X-1", 2); h.By != "weigher-1" || h.Reference != "weigh-same" {
		t.Errorf("the second hold: %+v", h)
	}
}

// TestALoadIntoAContainerThatHoldsTheLotAlreadyHoldsItOnce: a lot of 6 packs and 60,000 kg, container 1 takes 2
// apportioned packs (20,000 kg, within the limits) and then 1 more (30,000 kg, over). The container is both the
// stuffing the load goes into and a stuffing of the lot: it is held once, by one hold and one event, and the load
// commits.
func TestALoadIntoAContainerThatHoldsTheLotAlreadyHoldsItOnce(t *testing.T) {
	s := newSplitRun(t, 60000, 600)
	s.open("X-1", 1)
	s.open("X-2", 2)
	s.loadAs("ops-1", "X-1", "L-M", 2, 0, 0, 4)
	s.holds("X-1", api.ExecutionLoading, 0)
	s.loadAs("ops-1", "X-1", "L-M", 1, 0, 0, 4)
	s.holds("X-1", api.ExecutionOnHold, 1)
	if x := s.f.execution(t, "X-1"); len(x.Lines) != 2 || x.Revision != 4 {
		t.Errorf("container 1: %+v", x)
	}
}

// TestAReceiptDoesNotHoldAgainAContainerOverInWeightWhoseVolumeRoseByRounding is G2 of the second review. A lot of 7
// packs weighed at 420,000 kg and 4 cm3, 3, 3 and 1 apportioned packs in three containers: the 7 packs complete the
// lot, so the weight is divided 180,000 + 180,000 + 60,000 kg and the volume 4 x 3/7 = 1.71, 1.71 and 0.57, floors 1 +
// 1 + 0, two cm3 left for the largest remainders: 2 + 2 + 0. Container 3 is over in weight, held by its own load and
// released by a person. An eighth pack is received: the 7 packs take 420,000 x 7/8 = 367,500 kg, 157,500 + 157,500 +
// 52,500, and 3 cm3, 1 + 1 + 1. Container 3's weight fell, to 52,500 kg, over still, and its volume rose by 1 cm3: it
// is not held again, for the dimension it is over in did not rise. (Held for any rise, as in the second version of the
// rule, it would be.)
func TestAReceiptDoesNotHoldAgainAContainerOverInWeightWhoseVolumeRoseByRounding(t *testing.T) {
	s := newRunOf(t, 3, 12, 7, 420000, kernel.Quantity{Unit: "CMQ", Amount: 4}, false)
	s.open("X-1", 1)
	s.open("X-2", 2)
	s.open("X-3", 3)
	s.loadAs("ops-1", "X-1", "L-M", 3, 0, 0, 4)
	s.loadAs("ops-1", "X-2", "L-M", 3, 0, 0, 4)
	s.loadAs("ops-1", "X-3", "L-M", 1, 0, 0, 4)
	s.holds("X-3", api.ExecutionOnHold, 1)
	if l := s.limits(3); l.CargoGrams != 60000000 || l.CargoCubicCm != 0 {
		t.Fatalf("container 3 before the receipt: %+v", l)
	}
	s.release("X-3", 1)
	more := s.f.receive("rcv-more", "L-M", "SH-M", 2, 1, api.ConditionGood, s.f.hoursIn(5))
	more.Facility = cfsShanghai
	ok(t, 3)(s.f.module.ReceiveCargo(context.Background(), more))
	if l := s.limits(3); l.CargoGrams != 52500000 || l.CargoCubicCm != 1 || !l.OverMaxGross {
		t.Errorf("container 3 after the receipt: %+v", l)
	}
	s.holds("X-3", api.ExecutionLoading, 1)
}
