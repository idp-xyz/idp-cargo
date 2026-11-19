//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/api"
)

// These tests run the container limit check by the allotted figure (V2.83) with the real commands, in the split lot of
// splitRun: a lot L-M of 6 cartons in two 22G1 containers, which take 28,230 kg of cargo (30,480 kg gross less the
// 2,250 kg tare) and 33.2 m3. Every figure is worked out by hand in the comment of its test.

type placedHold struct {
	No                          int
	Reason, By, Reference, Text string
	At, EvidenceAt, EnteredAt   time.Time
}

// placed reads the holds of a stuffing as the database holds them: who placed them, with what evidence and when.
func (f *fixture) placed(t *testing.T, execution string) []placedHold {
	t.Helper()
	var out []placedHold
	err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		rows, err := e.Query(ctx, `SELECT hold_no, reason, placed_by, evidence_reference, coalesce(hold_text, ''), placed_at, evidence_occurred_at, entered_at
          FROM cargo_phy.hold_entry WHERE tenant_id=$1 AND execution_id=$2 ORDER BY hold_no`, f.tenant, execution)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h placedHold
			if err = rows.Scan(&h.No, &h.Reason, &h.By, &h.Reference, &h.Text, &h.At, &h.EvidenceAt, &h.EnteredAt); err != nil {
				return err
			}
			h.At, h.EvidenceAt, h.EnteredAt = h.At.UTC(), h.EvidenceAt.UTC(), h.EnteredAt.UTC()
			out = append(out, h)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// nthHold is the n-th hold of the stuffing (from 1), and fails the test, not the run, when there is none.
func (f *fixture) nthHold(t *testing.T, execution string, n int) placedHold {
	t.Helper()
	holds := f.placed(t, execution)
	if len(holds) < n {
		t.Fatalf("%s has %d holds, want a hold %d: %+v", execution, len(holds), n, holds)
	}
	return holds[n-1]
}

// heldEvents counts the LoadExecutionHeld events in the outbox that name the stuffing ("" for all of them).
func (f *fixture) heldEvents(t *testing.T, execution string) int {
	t.Helper()
	q, err := f.db.ReadExecutor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err = q.QueryRow(context.Background(), `SELECT count(*) FROM bento.outbox WHERE scope=$1 AND source=$2 AND event_type=$3 AND payload::text LIKE '%'||$4||'%'`,
		f.tenant, api.Source, api.LoadExecutionHeld, `"`+execution).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// wantHeld checks that the stuffing is held by one OVERWEIGHT hold that names the gross given, or, when gross is empty,
// that it is loading and holds nothing.
func (s *splitRun) wantHeld(x, gross string) {
	s.t.Helper()
	got, holds := s.f.execution(s.t, x), s.f.placed(s.t, x)
	if gross == "" {
		if got.State != api.ExecutionLoading || len(holds) != 0 {
			s.t.Errorf("%s: %s with holds %+v, want loading and no hold", x, got.State, holds)
		}
		return
	}
	if got.State != api.ExecutionOnHold || len(holds) != 1 || holds[0].Reason != "OVERWEIGHT" || !strings.Contains(holds[0].Text, "tare and cargo "+gross+" kg over the default maximum gross 30480 kg") {
		s.t.Errorf("%s: %s with holds %+v, want on hold by tare and cargo %s kg", x, got.State, holds, gross)
	}
}

// limits returns the Limits of the packing list of the unit.
func (s *splitRun) limits(unit int) api.UnitLimitsCheck {
	s.t.Helper()
	for _, l := range s.f.basis(s.t).PackingLists {
		if l.UnitNo == unit {
			return l.Limits
		}
	}
	s.t.Fatalf("no packing list of unit %d", unit)
	return api.UnitLimitsCheck{}
}

// digestOf is the canonical text of a packing list as api.UnitPackingList writes it, worked out here again so that the
// form is pinned from outside the package: the lines "unit", "execution" and "state", and a line for each house.
func digestOf(l api.UnitPackingList) string {
	var b strings.Builder
	fmt.Fprintf(&b, "unit %d\nexecution %q\nstate %q\n", l.UnitNo, l.ExecutionID, l.ExecutionState)
	for _, x := range l.Lines {
		fmt.Fprintf(&b, "line %q %d %q %d %q %d\n", x.ShipmentID, x.LoadedPacks, string(x.LoadedGrossWeight.Unit), x.LoadedGrossWeight.Amount,
			string(x.LoadedVolume.Unit), x.LoadedVolume.Amount)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func (s *splitRun) packingList(unit int) api.UnitPackingList {
	s.t.Helper()
	for _, l := range s.f.basis(s.t).PackingLists {
		if l.UnitNo == unit {
			return l
		}
	}
	s.t.Fatalf("no packing list of unit %d", unit)
	return api.UnitPackingList{}
}

// TestTheLimitCheckReadsTheAllottedFigureOnPostgres runs the two errors of before. A lot of 6 packs and 50,000 kg, 600
// cm3: container 1 holds 1 pack weighed at 2,000 kg (100 cm3) and 2 apportioned, container 2 takes 3 apportioned. The 5
// apportioned packs complete the lot and take the 50,000 - 2,000 = 48,000 kg the weighed pack leaves: container 2 3/5
// of it, 28,800 kg, with the tare 31,050 kg over the 30,480 kg, and container 1 2,000 + 48,000 x 2/5 = 21,200 kg.
// Alone, container 2 read 50,000 x 3/6 = 25,000 kg and was not held. The other way: a lot of 42,900 kg with 2 packs
// weighed at 21,450 kg in container 1 and 4 apportioned in container 2: container 2 reads 42,900 - 21,450 = 21,450 kg
// and is not held; alone it read 42,900 x 4/6 = 28,600 kg, over the 28,230 kg of cargo.
func TestTheLimitCheckReadsTheAllottedFigureOnPostgres(t *testing.T) {
	s := newSplitRun(t, 50000, 600)
	s.open("X-1", 1)
	s.open("X-2", 2)
	s.load("X-1", 1, 2000, 100)
	s.load("X-1", 2, 0, 0)
	s.wantHeld("X-1", "")
	s.load("X-2", 3, 0, 0)
	s.wantHeld("X-2", "31050.000")
	s.wantHeld("X-1", "")
	if a, b := s.limits(1), s.limits(2); a != (api.UnitLimitsCheck{Checked: true, CargoGrams: 21200000, CargoCubicCm: 100 + 500*2/5}) ||
		b != (api.UnitLimitsCheck{Checked: true, CargoGrams: 28800000, CargoCubicCm: 500 * 3 / 5, OverMaxGross: true}) {
		t.Errorf("limits of the units: %+v, %+v", a, b)
	}

	s = newSplitRun(t, 42900, 600)
	s.open("X-1", 1)
	s.open("X-2", 2)
	s.load("X-1", 2, 21450, 100)
	s.load("X-2", 4, 0, 0)
	s.wantHeld("X-1", "")
	s.wantHeld("X-2", "")
	if b := s.limits(2); b != (api.UnitLimitsCheck{Checked: true, CargoGrams: 21450000, CargoCubicCm: 500}) {
		t.Errorf("limits of unit 2: %+v", b)
	}
}

// TestALoadIntoAnotherContainerHoldsTheOneItPushesOver: a lot of 6 packs and 50,000 kg, 600 cm3, 3 packs apportioned in
// container 1, which read 50,000 x 3/6 = 25,000 kg, within the limits. Container 2 then takes the other 3, weighed for
// themselves at 1,000 kg, 100 cm3: the 3 apportioned packs complete the lot and take 50,000 - 1,000 = 49,000 kg, with
// the tare 51,250 kg over the 30,480 kg. The command of the load into container 2 places the hold on container 1, with
// its own actor, evidence and time, and one LoadExecutionHeld event for it; container 2 is not held.
func TestALoadIntoAnotherContainerHoldsTheOneItPushesOver(t *testing.T) {
	s := newSplitRun(t, 50000, 600)
	s.open("X-1", 1)
	s.open("X-2", 2)
	s.load("X-1", 3, 0, 0)
	s.wantHeld("X-1", "")
	at := s.f.hoursIn(5)
	cmd := s.f.load("load-by-2", "X-2", s.rev["X-2"], "L-M", 3, at)
	cmd.ActorID, cmd.GrossWeight, cmd.Volume = "loader-2", kgm(1000), cmq(100)
	ok(t, 3)(s.f.module.RecordCargoLoaded(context.Background(), cmd))
	s.rev["X-2"]++
	s.rev["X-1"]++ // the hold is a write of its record
	s.wantHeld("X-1", "51250.000")
	s.wantHeld("X-2", "")
	h := s.f.nthHold(t, "X-1", 1)
	if h.By != "loader-2" || h.Reference != "load-by-2" || !h.EvidenceAt.Equal(at) || !h.At.Equal(at) {
		t.Errorf("the hold: %+v, want placed by loader-2 on the evidence of its command at %v", h, at)
	}
	if age := time.Since(h.EnteredAt); h.EnteredAt.IsZero() || age < 0 || age > 5*time.Minute {
		t.Errorf("the hold was entered at %v: it is stamped with the time the command was entered", h.EnteredAt)
	}
	if x := s.f.execution(t, "X-1"); x.Revision != s.rev["X-1"] {
		t.Errorf("container 1's revision %d, want %d", x.Revision, s.rev["X-1"])
	}
	if n, all := s.f.heldEvents(t, "X-1"), s.f.heldEvents(t, ""); n != 1 || all != 1 {
		t.Errorf("held events: %d for container 1, %d in all; want 1 and 1", n, all)
	}
	if l := s.limits(1); !l.Checked || !l.OverMaxGross || l.CargoGrams != 49000000 {
		t.Errorf("limits of unit 1: %+v", l)
	}
	// The next unload that touches the lot does not hold it again: one OVERWEIGHT hold is placed at a time.
	later := s.f.hoursIn(6)
	ok(t, 4)(s.f.module.RecordCargoUnloaded(context.Background(), api.RecordCargoUnloaded{CommandID: "unload-by-2", TenantID: s.f.tenant,
		ActorID: "unloader-2", ExecutionID: "X-2", ExpectedRevision: s.rev["X-2"], LineNo: 1, Packs: 1, Reason: api.ReversalMisloaded,
		Evidence: evidence("tally-2", later), OccurredAt: later}))
	s.wantHeld("X-1", "51250.000")
}

// TestAnUnloadHoldsTheOtherContainerItPushesOver: a lot of 6 packs and 45,000 kg, 600 cm3, 2 packs weighed in container
// 1 at 20,000 kg (100 cm3) and 4 apportioned in container 2, which complete the lot and take 45,000 - 20,000 = 25,000
// kg, within the limits. The 2 weighed packs are unloaded: the 4 apportioned packs now take 45,000 x 4/6 = 30,000 kg,
// with the tare 32,250 kg over the 30,480 kg. The unload's command holds container 2, by its actor and evidence;
// container 1, unloaded from, holds nothing.
func TestAnUnloadHoldsTheOtherContainerItPushesOver(t *testing.T) {
	s := newSplitRun(t, 45000, 600)
	s.open("X-1", 1)
	s.open("X-2", 2)
	line := s.load("X-1", 2, 20000, 100)
	s.load("X-2", 4, 0, 0)
	s.wantHeld("X-2", "")
	at := s.f.hoursIn(5)
	ok(t, 4)(s.f.module.RecordCargoUnloaded(context.Background(), api.RecordCargoUnloaded{CommandID: "unload-by-1", TenantID: s.f.tenant,
		ActorID: "unloader-1", ExecutionID: "X-1", ExpectedRevision: s.rev["X-1"], LineNo: line, Packs: 2, Reason: api.ReversalMisloaded,
		Evidence: evidence("tally-unload", at), OccurredAt: at}))
	s.rev["X-1"]++
	s.rev["X-2"]++
	s.wantHeld("X-2", "32250.000")
	s.wantHeld("X-1", "")
	h := s.f.nthHold(t, "X-2", 1)
	if h.By != "unloader-1" || h.Reference != "tally-unload" || !h.EvidenceAt.Equal(at) || !h.At.Equal(at) {
		t.Errorf("the hold: %+v", h)
	}
	if n, all := s.f.heldEvents(t, "X-2"), s.f.heldEvents(t, ""); n != 1 || all != 1 {
		t.Errorf("held events: %d for container 2, %d in all; want 1 and 1", n, all)
	}
	if l := s.limits(2); !l.OverMaxGross || l.CargoGrams != 30000000 {
		t.Errorf("limits of unit 2: %+v", l)
	}
}

// TestALotMeasuredAgainHoldsTheContainersByTheAllottedFigure: a lot of 6 packs weighed at 50,000 kg with 3 apportioned
// packs in each container: 25,000 kg each, within the limits. Weighed again at 60,000 kg both read 30,000 kg, with the
// tare 32,250 kg over the 30,480 kg: the measurement holds both. A second lot of 6 packs and 40,000 kg, with 1 pack
// weighed at 2,000 kg and 2 apportioned in container 1 and 3 apportioned in container 2, is 38,000 x 2/5 + 2,000 =
// 17,200 kg in container 1 and 38,000 x 3/5 = 22,800 kg in container 2; weighed again at 50,000 kg container 2 reads
// 48,000 x 3/5 = 28,800 kg, 31,050 kg with the tare, held, and container 1 2,000 + 19,200 = 21,200 kg, not. Alone,
// container 2 read 50,000 x 3/6 = 25,000 kg and was not held.
func TestALotMeasuredAgainHoldsTheContainersByTheAllottedFigure(t *testing.T) {
	again := func(s *splitRun, kilos int64) {
		t.Helper()
		at := s.f.hoursIn(5)
		ok(t, 4)(s.f.module.RecordCargoMeasurement(context.Background(), api.RecordCargoMeasurement{CommandID: "measure-again", TenantID: s.f.tenant,
			ActorID: "weigher-1", LotID: "L-M", ExpectedRevision: s.lot, GrossWeight: kgm(kilos), Volume: cmq(600), Method: api.MethodScale,
			Evidence: evidence("weigh-again", at), OccurredAt: at}))
		s.rev["X-1"]++
		s.rev["X-2"]++
	}
	s := newSplitRun(t, 50000, 600)
	s.open("X-1", 1)
	s.open("X-2", 2)
	s.load("X-1", 3, 0, 0)
	s.load("X-2", 3, 0, 0)
	s.wantHeld("X-1", "")
	s.wantHeld("X-2", "")
	again(s, 60000)
	s.wantHeld("X-1", "32250.000")
	s.wantHeld("X-2", "32250.000")
	for _, x := range []string{"X-1", "X-2"} {
		if h := s.f.nthHold(t, x, 1); h.By != "weigher-1" || h.Reference != "weigh-again" {
			t.Errorf("%s: %+v", x, h)
		}
		if n := s.f.heldEvents(t, x); n != 1 {
			t.Errorf("%s: %d held events", x, n)
		}
	}

	s = newSplitRun(t, 40000, 600)
	s.open("X-1", 1)
	s.open("X-2", 2)
	s.load("X-1", 1, 2000, 100)
	s.load("X-1", 2, 0, 0)
	s.load("X-2", 3, 0, 0)
	again(s, 50000)
	s.wantHeld("X-1", "")
	s.wantHeld("X-2", "31050.000")
}

// A sealed container is not held and not refused: the command that pushes it over its limits succeeds, the container
// keeps no hold, and the packing list's Limits say it is over, with the digest of the packing list computed by the
// unchanged canonical text. A lot of 6 packs and 50,000 kg, 600 cm3, 3 apportioned packs in container 1, which read
// 25,000 kg and is sealed.
//   - Container 2 takes the other 3 weighed for themselves at 1,000 kg: the 3 apportioned packs take 49,000 kg, with
//     the tare 51,250 kg over the 30,480 kg.
//   - Or the lot is weighed again at 70,000 kg: 70,000 x 3/6 = 35,000 kg, 37,250 kg with the tare.
func TestASealedContainerPushedOverIsNotHeldAndSaysSo(t *testing.T) {
	sealed := func() *splitRun {
		s := newSplitRun(t, 50000, 600)
		s.open("X-1", 1)
		s.open("X-2", 2)
		s.load("X-1", 3, 0, 0)
		s.seal("X-1")
		before := s.packingList(1)
		if l := before.Limits; l != (api.UnitLimitsCheck{Checked: true, CargoGrams: 25000000, CargoCubicCm: 300}) || before.ExecutionState != "CLOSED" {
			t.Fatalf("before the push: %+v in state %s", l, before.ExecutionState)
		}
		return s
	}
	check := func(s *splitRun, name string, grams, cubic int64, held2 bool) {
		t.Helper()
		after := s.packingList(1)
		if l := after.Limits; l != (api.UnitLimitsCheck{Checked: true, CargoGrams: grams, CargoCubicCm: cubic, OverMaxGross: true}) {
			t.Errorf("%s: limits of unit 1 after the push: %+v", name, l)
		}
		if got := s.f.execution(t, "X-1"); got.State != api.ExecutionClosed || len(got.Holds) != 0 || got.Revision != s.rev["X-1"] {
			t.Errorf("%s: the sealed container is %s with %d holds at revision %d (want %d): not held, not written", name, got.State, len(got.Holds), got.Revision, s.rev["X-1"])
		}
		if after.Digest != digestOf(after) {
			t.Errorf("%s: the digest %s is not that of the canonical text, %s", name, after.Digest, digestOf(after))
		}
		if n := s.f.heldEvents(t, "X-1"); n != 0 {
			t.Errorf("%s: %d held events for the sealed container", name, n)
		}
		_ = held2
	}

	s := sealed()
	before := s.packingList(1)
	ok(t, 3)(s.f.module.RecordCargoLoaded(context.Background(), func() api.RecordCargoLoaded {
		c := s.f.load("load-2", "X-2", s.rev["X-2"], "L-M", 3, s.f.hoursIn(5))
		c.GrossWeight, c.Volume = kgm(1000), cmq(100)
		return c
	}()))
	check(s, "load", 49000000, 500, false)
	s.wantHeld("X-2", "")
	if after := s.packingList(1); after.Digest == before.Digest {
		t.Error("the digest of the packing list moves with the figure")
	}

	s = sealed()
	at := s.f.hoursIn(5)
	ok(t, 4)(s.f.module.RecordCargoMeasurement(context.Background(), api.RecordCargoMeasurement{CommandID: "weigh-again", TenantID: s.f.tenant,
		ActorID: "weigher-1", LotID: "L-M", ExpectedRevision: s.lot, GrossWeight: kgm(70000), Volume: cmq(600), Method: api.MethodScale,
		Evidence: evidence("weigh-again", at), OccurredAt: at}))
	check(s, "measurement", 35000000, 300, false)
}

// holdRow holds a FOR UPDATE lock on the row, on a connection of its own, until the returned function releases it; pid
// is the connection's backend, which the commands that wait behind it are counted by.
func (f *fixture) holdRow(t *testing.T, query string, args ...any) (pid int, release func()) {
	t.Helper()
	locked, stop, done, backend := make(chan struct{}), make(chan struct{}), make(chan error, 1), make(chan int, 1)
	go func() {
		done <- f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			if _, err := e.Exec(ctx, query, args...); err != nil {
				return err
			}
			var p int
			if err := e.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&p); err != nil {
				return err
			}
			backend <- p
			close(locked)
			<-stop
			return nil
		})
	}()
	select {
	case <-locked:
	case err := <-done:
		t.Fatal(err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			close(stop)
			if err := <-done; err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(release)
	return <-backend, release
}

// waitingBehind waits until n backends are blocked by the backend pid, directly or through one that is.
func (f *fixture) waitingBehind(t *testing.T, pid, n int) {
	t.Helper()
	q, err := f.db.ReadExecutor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var got int
		if err = q.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity a WHERE a.pid <> pg_backend_pid()
          AND ($1 = ANY(pg_blocking_pids(a.pid)) OR EXISTS (SELECT 1 FROM unnest(pg_blocking_pids(a.pid)) b WHERE $1 = ANY(pg_blocking_pids(b))))`, pid).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got >= n {
			return
		}
	}
	t.Fatalf("%d commands are not waiting behind backend %d", n, pid)
}

const lockExecution = `SELECT 1 FROM cargo_phy.load_execution WHERE tenant_id=$1 AND execution_id=$2 FOR UPDATE`

// quickly runs a command that must answer at once, not wait for a lock.
func quickly[T any](t *testing.T, call func(context.Context) (T, error)) (T, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	r, err := call(ctx)
	if ctx.Err() != nil || time.Since(start) > 4*time.Second {
		t.Fatalf("the command waited for a lock: %v after %v", err, time.Since(start))
	}
	return r, err
}

// TestALoadOrAnUnloadDoesNotWaitForAnotherStuffingsLock: a lot of 6 packs and 301 kg in two containers, 4 packs in
// container 1 and 1 in container 2; 5 live packs take 301 x 5/6 = 250.83, 250 kg, 4 and 1 of them 200 and 50 kg.
// Container 1's execution row is held on a second connection. A load into container 2 of the same lot takes the lot's
// lock and then needs container 1, to check its limits: it answers the conflict at once and writes nothing (no line, no
// hold, no event, no new revision), and once the lock is released the same command succeeds. The same for an unload
// from container 2.
func TestALoadOrAnUnloadDoesNotWaitForAnotherStuffingsLock(t *testing.T) {
	s := newSplitRun(t, 301, 600)
	s.open("X-1", 1)
	s.open("X-2", 2)
	s.load("X-1", 4, 0, 0)
	line := s.load("X-2", 1, 0, 0)
	events := len(s.f.outboxTypes(t))
	_, release := s.f.holdRow(t, lockExecution, s.f.tenant, "X-1")

	before := s.f.execution(t, "X-2")
	load := s.f.load("load-more", "X-2", s.rev["X-2"], "L-M", 1, s.f.hoursIn(5))
	unload := api.RecordCargoUnloaded{CommandID: "unload-one", TenantID: s.f.tenant, ActorID: "ops-1", ExecutionID: "X-2", ExpectedRevision: s.rev["X-2"],
		LineNo: line, Packs: 1, Reason: api.ReversalMisloaded, Evidence: evidence("tally", s.f.hoursIn(5)), OccurredAt: s.f.hoursIn(5)}
	for name, run := range map[string]func() error{
		"load": func() error {
			_, err := quickly(t, func(ctx context.Context) (api.Receipt, error) { return s.f.module.RecordCargoLoaded(ctx, load) })
			return err
		},
		"unload": func() error {
			_, err := quickly(t, func(ctx context.Context) (api.Receipt, error) { return s.f.module.RecordCargoUnloaded(ctx, unload) })
			return err
		},
	} {
		if err := run(); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "X-1") {
			t.Fatalf("%s while the other container is busy: %v, want the conflict naming X-1", name, err)
		}
		after := s.f.execution(t, "X-2")
		if after.Revision != before.Revision || len(after.Lines) != len(before.Lines) || len(after.Reversals) != len(before.Reversals) || len(after.Holds) != 0 ||
			len(s.f.outboxTypes(t)) != events {
			t.Fatalf("%s: the refused command wrote: %+v, %d events (was %d)", name, after, len(s.f.outboxTypes(t)), events)
		}
		if got := s.f.execution(t, "X-1"); len(got.Holds) != 0 {
			t.Fatalf("%s: container 1 holds %+v", name, got.Holds)
		}
	}
	release()
	ok(t, before.Revision+1)(s.f.module.RecordCargoLoaded(context.Background(), load))
	s.rev["X-2"]++
	// The unload, after the load: its line is still there and the lock is free.
	unload.ExpectedRevision = s.rev["X-2"]
	ok(t, before.Revision+2)(s.f.module.RecordCargoUnloaded(context.Background(), unload))
	s.rev["X-2"]++
	s.expect("after the load and the unload", figure{5, 250, 500}, map[int]figure{1: {4, 200, 400}, 2: {1, 50, 100}})
}

// TestTwoLoadsOfOneLotQueuedBehindItsLock: two loads of one lot into the two containers, each queued behind a lock on
// the lot's row held on a second connection, in each order. Each command holds its own execution row (and the house's
// ledger) before it waits for the lot. A lot of 6 packs and 301 kg.
//   - Neither container holds packs of the lot yet: the command that gets the lot first has no other stuffing to lock,
//     commits, and the other, which sees its line, commits after it: 5 packs in container 1 and 1 in container 2 are
//     301 x 5/6 = 250.83 and 50.17 kg, the floors 250 + 50 leave 1 kg for the larger remainder (.83): 251 + 50 kg,
//     whichever container loaded first.
//   - Each container holds 1 pack already, and takes 3 and 1 more: the command that gets the lot first needs the other
//     container's lock, which the other command holds while it waits for the lot. It must not wait for it: it answers
//     the conflict and writes nothing, the other commits, and the first, tried again, commits and sees the other's
//     line: 4 and 2 packs complete the lot and take its 301 kg, 200.67 and 100.33, the floors 200 + 100 leave 1 kg for
//     the larger remainder: 201 + 100 kg.
func TestTwoLoadsOfOneLotQueuedBehindItsLock(t *testing.T) {
	for _, prior := range []bool{false, true} {
		for _, order := range [][2]string{{"X-1", "X-2"}, {"X-2", "X-1"}} {
			t.Run(fmt.Sprint("prior ", prior, ", ", order[0], " first"), func(t *testing.T) {
				s := newSplitRun(t, 301, 600)
				s.open("X-1", 1)
				s.open("X-2", 2)
				packs := map[string]int64{"X-1": 5, "X-2": 1}
				if prior {
					s.load("X-1", 1, 0, 0)
					s.load("X-2", 1, 0, 0)
					packs = map[string]int64{"X-1": 3, "X-2": 1}
				}
				cmd := func(x string) api.RecordCargoLoaded {
					return s.f.load("load-"+x, x, s.rev[x], "L-M", packs[x], s.f.hoursIn(5))
				}
				revision := map[string]int64{"X-1": s.rev["X-1"], "X-2": s.rev["X-2"]}
				pid, release := s.f.holdRow(t, `SELECT 1 FROM cargo_phy.cargo_lot WHERE tenant_id=$1 AND lot_id='L-M' FOR UPDATE`, s.f.tenant)
				results := map[string]chan error{order[0]: make(chan error, 1), order[1]: make(chan error, 1)}
				for i, x := range order {
					go func() {
						_, err := s.f.module.RecordCargoLoaded(context.Background(), cmd(x))
						results[x] <- err
					}()
					s.f.waitingBehind(t, pid, i+1)
				}
				release()
				var failed, committed []string
				for _, x := range order {
					switch err := <-results[x]; {
					case err == nil:
						committed = append(committed, x)
					case errors.Is(err, api.ErrConflict):
						failed = append(failed, x)
					default:
						t.Fatalf("%s: %v", x, err)
					}
				}
				if want := map[bool]int{false: 0, true: 1}[prior]; len(failed) != want || len(committed) != 2-want {
					t.Fatalf("committed %v, conflict %v; want %d conflicts", committed, failed, want)
				}
				for _, x := range failed {
					if got := s.f.execution(t, x); got.Revision != revision[x] || len(got.Holds) != 0 {
						t.Fatalf("the refused load wrote: %+v", got)
					}
					ok(t, revision[x]+1)(s.f.module.RecordCargoLoaded(context.Background(), cmd(x)))
				}
				if prior {
					s.expect("both loads committed", figure{6, 301, 600}, map[int]figure{1: {4, 201, 400}, 2: {2, 100, 200}})
				} else {
					s.expect("both loads committed", figure{6, 301, 600}, map[int]figure{1: {5, 251, 500}, 2: {1, 50, 100}})
				}
				s.wantHeld("X-1", "")
				s.wantHeld("X-2", "")
			})
		}
	}
}

// TestLoadsOfOneLotIntoTwoContainersAtOnceRace: a lot of 6 packs and 50,000 kg, 600 cm3; container 2 takes 3 packs
// weighed for themselves at 1,000 kg while container 1 takes the other 3, apportioned, at the same time. Alone
// container 1 reads 50,000 x 3/6 = 25,000 kg, within the limits; with the weighed packs in container 2 it reads
// 50,000 - 1,000 = 49,000 kg, 51,250 kg with the tare, over. Whatever the order, at the end container 1 is held (by its
// own load if the weighed packs came first, by the load into container 2 if they came after) and container 2 is not,
// and each load committed once: a load that answered the conflict is tried again.
func TestLoadsOfOneLotIntoTwoContainersAtOnceRace(t *testing.T) {
	for round := 0; round < 5; round++ {
		s := newSplitRun(t, 50000, 600)
		s.open("X-1", 1)
		s.open("X-2", 2)
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for _, x := range []string{"X-1", "X-2"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for attempt := 0; attempt < 50; attempt++ {
					cur, err := s.f.module.GetLoadExecution(context.Background(), api.GetLoadExecution{TenantID: s.f.tenant, ExecutionID: x})
					if err != nil {
						errs <- err
						return
					}
					cmd := s.f.load(fmt.Sprint("race-", x), x, cur.Revision, "L-M", 3, s.f.hoursIn(5))
					if x == "X-2" {
						cmd.GrossWeight, cmd.Volume = kgm(1000), cmq(100)
					}
					_, err = s.f.module.RecordCargoLoaded(context.Background(), cmd)
					if errors.Is(err, api.ErrConflict) {
						continue
					}
					errs <- err
					return
				}
				errs <- fmt.Errorf("%s: 50 conflicts", x)
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
		}
		for _, x := range []string{"X-1", "X-2"} {
			if got := s.f.execution(t, x); len(got.Lines) != 1 {
				t.Fatalf("round %d: %s has %d lines", round, x, len(got.Lines))
			}
		}
		s.wantHeld("X-1", "51250.000")
		s.wantHeld("X-2", "")
		if n := s.f.heldEvents(t, ""); n != 1 {
			t.Fatalf("round %d: %d held events, want 1", round, n)
		}
		if l := s.limits(1); !l.OverMaxGross || l.CargoGrams != 49000000 {
			t.Fatalf("round %d: limits of unit 1: %+v", round, l)
		}
	}
}

// TestTheAllotmentCountsTheSealedContainersPacks: the packs in a sealed container take their part of the lot's measure
// whether or not the container can be held, so a check of another container reads them (without locking the sealed
// one, which no command changes). A lot of 6 packs and 42,900 kg, 600 cm3, 2 packs weighed at 21,450 kg (100 cm3) in
// container 1, which is sealed, and 4 apportioned in container 2: container 2 reads 42,900 - 21,450 = 21,450 kg, within
// the limits. Container 2 alone reads 42,900 x 4/6 = 28,600 kg, over the 28,230 kg. When the lot is weighed again at
// 43,000 kg it reads 43,000 - 21,450 = 21,550 kg, not 43,000 x 4/6 = 28,667 kg: not held, by the measurement or by the
// load.
func TestTheAllotmentCountsTheSealedContainersPacks(t *testing.T) {
	s := newSplitRun(t, 42900, 600)
	s.open("X-1", 1)
	s.open("X-2", 2)
	s.load("X-1", 2, 21450, 100)
	s.seal("X-1")
	s.load("X-2", 4, 0, 0)
	s.wantHeld("X-2", "")
	at := s.f.hoursIn(5)
	ok(t, 4)(s.f.module.RecordCargoMeasurement(context.Background(), api.RecordCargoMeasurement{CommandID: "weigh-again", TenantID: s.f.tenant,
		ActorID: "weigher-1", LotID: "L-M", ExpectedRevision: s.lot, GrossWeight: kgm(43000), Volume: cmq(600), Method: api.MethodScale,
		Evidence: evidence("weigh-again", at), OccurredAt: at}))
	s.wantHeld("X-2", "")
	if b := s.limits(2); b != (api.UnitLimitsCheck{Checked: true, CargoGrams: 21550000, CargoCubicCm: 500}) {
		t.Errorf("limits of unit 2: %+v", b)
	}
}
