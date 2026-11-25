//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	phymodule "go.idp.xyz/idp-cargo/internal/contexts/phy"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/internal/application"
	phy "go.idp.xyz/idp-cargo/internal/contexts/phy/internal/domain"
	scope "go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// These tests are the retry of V2.84: a command that finds a stuffing busy, or the stuffings loading a lot changed, is
// run again in the command path. They build the commands' Deps themselves, as the module does, so that the attempts can
// be counted: every attempt looks its command id's receipt up once, at the start of its transaction, and the replay
// check before the first attempt looks it up once more.

// attemptLog counts the receipt look-ups of a command. before, if set, runs at the start of each attempt's transaction
// with the number of the look-up (1 is the replay check, 2 the first attempt, ...), holding nothing yet.
type attemptLog struct {
	mu       sync.Mutex
	finds    int
	returned int // transactions that have ended, committed or rolled back
	before   func(find int)
}

func (l *attemptLog) attempts() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return max(l.finds-1, 0)
}

type countingReceipts struct {
	application.Receipts
	log *attemptLog
}

func (c countingReceipts) Find(ctx context.Context, tenant phy.TenantID, id string) (application.Receipt, bool, error) {
	c.log.mu.Lock()
	c.log.finds++
	n, before := c.log.finds, c.log.before
	c.log.mu.Unlock()
	if before != nil {
		before(n)
	}
	return c.Receipts.Find(ctx, tenant, id)
}

// countingTx counts the transactions that have ended, so that a test can tell a command is between two attempts.
type countingTx struct {
	scope.TenantTransactor
	log *attemptLog
}

func (c countingTx) WithinTransaction(ctx context.Context, fn func(context.Context) error) error {
	err := c.TenantTransactor.WithinTransaction(ctx, fn)
	c.log.mu.Lock()
	c.log.returned++
	c.log.mu.Unlock()
	return err
}

func (l *attemptLog) ended() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.returned
}

type allowAll struct{}

func (allowAll) RequireConsolidationAction(context.Context, application.Scope, application.Action, phy.ConsolidationID) error {
	return nil
}
func (allowAll) RequireLotAction(context.Context, application.Scope, application.Action, phy.CargoLotID) error {
	return nil
}

type noIntents struct{}

func (noIntents) LoadIntent(context.Context, phy.TenantID, phy.ShipmentID) (phy.LoadIntent, error) {
	return phy.LoadIntent{}, errors.New("the retry tests receive more packs of a lot that exists: FUL is not asked")
}

// retryDeps is the Deps of one command of the fixture's tenant, with the attempts and the first wait given.
func (f *fixture) retryDeps(t *testing.T, commandID string, attempts int, backoff time.Duration) (application.Deps, *attemptLog) {
	t.Helper()
	outbox, err := bentooutbox.NewStore(f.db)
	if err != nil {
		t.Fatal(err)
	}
	log := &attemptLog{}
	return application.Deps{Tx: countingTx{scope.TenantTransactor{DB: f.db, Tenant: f.tenant}, log}, Consolidations: postgres.ConsolidationStore{DB: f.db},
		Memberships: postgres.MembershipStore{DB: f.db}, Lots: postgres.LotStore{DB: f.db}, Executions: postgres.ExecutionStore{DB: f.db},
		Receipts: countingReceipts{postgres.CommandReceipts{DB: f.db}, log}, Outbox: postgres.Outbox{Writer: outbox, CommandID: commandID, RecordedAt: time.Now()},
		Auth: allowAll{}, Clock: scope.SystemClock{}, Window: kernel.DefaultRecordWindow, Attempts: attempts, Backoff: backoff}, log
}

// withoutRetry makes the fixture's module answer a busy stuffing at once, as it did before V2.84.
func (f *fixture) withoutRetry(t *testing.T) {
	t.Helper()
	module, err := phymodule.New(f.db, f.ful, policy{}, phymodule.WithLocations(locationStub{}), phymodule.WithEquipment(equipmentStub{}),
		phymodule.WithConflictRetry(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	f.module = module
}

// receipts counts the receipts stored for a command id.
func (f *fixture) receipts(t *testing.T, commandID string) int {
	t.Helper()
	var n int
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT count(*) FROM cargo_phy.command_receipt WHERE tenant_id=$1 AND command_id=$2`, f.tenant, commandID).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func (s *splitRun) evidenceOf(ref string, hour int) phy.Evidence {
	return phy.Evidence{Source: "WMS", Reference: ref, OccurredAt: s.f.hoursIn(hour)}
}

func (s *splitRun) scopeOf() application.Scope {
	return application.Scope{TenantID: phy.TenantID(s.f.tenant), ActorID: "ops-1"}
}

// busyRun is a lot L-M of 6 packs received (of 12 planned, still receiving at revision 2) and 50,000 kg, with 3 of its
// packs in container 1 and none in container 2, whose stuffing is open. Container 1's row is what the tests hold.
func busyRun(t *testing.T) *splitRun {
	t.Helper()
	s := newRunOf(t, 2, 12, 6, 50000, kernel.Quantity{Unit: "CMQ", Amount: 600}, false)
	s.open("X-1", 1)
	s.open("X-2", 2)
	s.loadAs("ops-1", "X-1", "L-M", 3, 0, 0, 4)
	return s
}

// A retried command: a load of 2 packs into container 2 (which locks the lot and then needs container 1, the busy one)
// and a receipt of 1 more pack of the lot (which does the same); written says how much of the command is stored.
type retried struct {
	name    string
	run     func(ctx context.Context, s *splitRun, d application.Deps, expected int64) error
	current func(s *splitRun) int64 // the revision a command must expect
	written func(t *testing.T, s *splitRun) int
	stale   int64 // a revision that is not the current one
	reads   int   // transactions a command runs before its first attempt: the replay check and, for a load, the execution's consolidation
}

func retriedCommands() []retried {
	return []retried{
		{name: "a load", stale: 1, reads: 2,
			run: func(ctx context.Context, s *splitRun, d application.Deps, expected int64) error {
				_, err := application.RecordCargoLoaded{Deps: d}.Handle(ctx, application.RecordCargoLoadedCommand{
					ExecutionCommand: application.ExecutionCommand{CommandID: "retried", Scope: s.scopeOf(), ExecutionID: "X-2", ExpectedRevision: expected,
						OccurredAt: s.f.hoursIn(5)}, LotID: "L-M", Packs: 2, Evidence: s.evidenceOf("retried", 5)})
				return err
			},
			current: func(s *splitRun) int64 { return s.rev["X-2"] },
			written: func(t *testing.T, s *splitRun) int { return len(s.f.execution(t, "X-2").Lines) }},
		{name: "a receipt", stale: 1, reads: 1,
			run: func(ctx context.Context, s *splitRun, d application.Deps, expected int64) error {
				_, err := application.ReceiveCargo{Deps: d, Intents: noIntents{}}.Handle(ctx, application.ReceiveCargoCommand{
					CommandID: "retried", Scope: s.scopeOf(), LotID: "L-M", ShipmentID: "SH-M", Facility: phy.Facility{Location: cfsShanghai.Location, Code: cfsShanghai.Code},
					ExpectedRevision: expected, Line: phy.ReceiptLine{Packs: 1, PackageType: "CT", Condition: phy.ConditionGood, Evidence: s.evidenceOf("retried", 5)},
					OccurredAt: s.f.hoursIn(5)})
				return err
			},
			current: func(s *splitRun) int64 { return s.lot },
			written: func(t *testing.T, s *splitRun) int {
				lot, err := s.f.module.GetCargoLot(context.Background(), api.GetCargoLot{TenantID: s.f.tenant, LotID: "L-M"})
				if err != nil {
					t.Fatal(err)
				}
				return len(lot.Lines)
			}},
	}
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestABusyStuffingIsRetriedUntilItIsFree: container 1's row is held on a second connection and a load into container
// 2, or a receipt of the lot, finds it busy. The command is run again, with 50 ms first, and when the row is released
// after the second attempt has been made the command succeeds: the caller never sees a conflict, and exactly one line,
// one event and one receipt are stored. With the waits of 50 ms doubling six attempts last 1.5 s at least, so the
// release (made as soon as the second attempt is seen) comes before the last one. The second attempt may already find
// the row free or be refused too, so between 2 and 6 attempts are counted.
func TestABusyStuffingIsRetriedUntilItIsFree(t *testing.T) {
	for _, c := range retriedCommands() {
		t.Run(c.name, func(t *testing.T) {
			s := busyRun(t)
			before, events := c.written(t, s), len(s.f.outboxTypes(t))
			_, release := s.f.holdRow(t, lockExecution, s.f.tenant, "X-1")
			deps, log := s.f.retryDeps(t, "retried", 6, 50*time.Millisecond)
			done := make(chan error, 1)
			go func() { done <- c.run(context.Background(), s, deps, c.current(s)) }()
			waitUntil(t, "a second attempt", func() bool { return log.attempts() >= 2 })
			release()
			if err := <-done; err != nil {
				t.Fatalf("the command after the stuffing was released: %v", err)
			}
			if got := log.attempts(); got < 2 || got > 6 {
				t.Errorf("%d attempts; at least 2 (one refused, one committed) and at most 6 expected", got)
			}
			if got, ev, rc := c.written(t, s), len(s.f.outboxTypes(t)), s.f.receipts(t, "retried"); got != before+1 || ev != events+1 || rc != 1 {
				t.Errorf("stored %d lines (was %d), %d events (was %d), %d receipts; want one more line, one more event, one receipt", got, before, ev, events, rc)
			}
		})
	}
}

// TestAStuffingBusyForLongerThanAllAttemptsIsAConflict: the row stays held through every attempt. Three attempts are
// made, no more and no fewer, and the caller gets the conflict, naming the busy stuffing, with nothing written: no
// line, no event, no receipt, no new revision; and once the row is released the same command succeeds.
func TestAStuffingBusyForLongerThanAllAttemptsIsAConflict(t *testing.T) {
	for _, c := range retriedCommands() {
		t.Run(c.name, func(t *testing.T) {
			s := busyRun(t)
			before, events, revision := c.written(t, s), len(s.f.outboxTypes(t)), c.current(s)
			_, release := s.f.holdRow(t, lockExecution, s.f.tenant, "X-1")
			deps, log := s.f.retryDeps(t, "retried", 3, time.Millisecond)
			err := c.run(context.Background(), s, deps, revision)
			if !errors.Is(err, phy.ErrConflict) || !errors.Is(err, phy.ErrRetryable) || log.attempts() != 3 {
				t.Fatalf("%v after %d attempts, want the conflict after 3", err, log.attempts())
			}
			if got, ev, rc := c.written(t, s), len(s.f.outboxTypes(t)), s.f.receipts(t, "retried"); got != before || ev != events || rc != 0 {
				t.Fatalf("the refused command wrote: %d lines (was %d), %d events (was %d), %d receipts", got, before, ev, events, rc)
			}
			release()
			deps, log = s.f.retryDeps(t, "retried", 3, time.Millisecond)
			if err = c.run(context.Background(), s, deps, revision); err != nil || log.attempts() != 1 {
				t.Fatalf("after the release: %v, %d attempts", err, log.attempts())
			}
		})
	}
}

// TestAStaleRevisionIsNeverRetried: the stuffing is busy and the caller's revision is not the current one: the command
// answers the plain conflict at its first attempt, though a retry would have been allowed five times. A stale revision
// with nothing busy is the same.
func TestAStaleRevisionIsNeverRetried(t *testing.T) {
	for _, c := range retriedCommands() {
		for _, held := range []bool{true, false} {
			t.Run(fmt.Sprint(c.name, ", busy ", held), func(t *testing.T) {
				s := busyRun(t)
				before := c.written(t, s)
				if held {
					s.f.holdRow(t, lockExecution, s.f.tenant, "X-1")
				}
				deps, log := s.f.retryDeps(t, "retried", 6, time.Millisecond)
				err := c.run(context.Background(), s, deps, c.current(s)-c.stale)
				if !errors.Is(err, phy.ErrConflict) || errors.Is(err, phy.ErrRetryable) || log.attempts() != 1 {
					t.Fatalf("%v after %d attempts, want the plain conflict after 1", err, log.attempts())
				}
				if got := c.written(t, s); got != before || s.f.receipts(t, "retried") != 0 {
					t.Fatalf("the refused command wrote: %d lines (was %d)", got, before)
				}
			})
		}
	}
}

// TestACancelledOrExpiredContextEndsTheWait: the stuffing is busy and the first wait is two seconds at least. A command
// whose context is cancelled after its first attempt, or expires, returns that context's error within moments, not the
// conflict, having made that one attempt.
func TestACancelledOrExpiredContextEndsTheWait(t *testing.T) {
	for _, c := range retriedCommands() {
		for _, how := range []string{"cancelled", "expired"} {
			t.Run(c.name+", "+how, func(t *testing.T) {
				s := busyRun(t)
				s.f.holdRow(t, lockExecution, s.f.tenant, "X-1")
				deps, log := s.f.retryDeps(t, "retried", 6, 2*time.Second)
				ctx, cancel := context.WithCancel(context.Background())
				if how == "expired" {
					ctx, cancel = context.WithTimeout(context.Background(), 300*time.Millisecond)
				}
				defer cancel()
				done := make(chan error, 1)
				start := time.Now()
				go func() { done <- c.run(ctx, s, deps, c.current(s)) }()
				if how == "cancelled" {
					// Cancel once the first attempt's transaction has ended (the replay check's transaction is the
					// first to end), so that the cancellation never reaches the driver while it writes: the error of
					// a cancelled write is a network error (see TestAFailedAttemptAnswersTheContextsErrorWhenItEnded).
					waitUntil(t, "the first attempt", func() bool { return log.attempts() >= 1 && log.ended() >= 2 })
					cancel()
				}
				err := <-done
				want := map[string]error{"cancelled": context.Canceled, "expired": context.DeadlineExceeded}[how]
				if !errors.Is(err, want) || errors.Is(err, phy.ErrConflict) || log.attempts() != 1 || time.Since(start) > 1900*time.Millisecond {
					t.Fatalf("%v after %d attempts and %v, want %v after 1 attempt within the first wait", err, log.attempts(), time.Since(start), want)
				}
			})
		}
	}
}

// TestAMeasurementIsRetriedWhenTheStuffingsChanged: a lot with live packs in container 1 (3) and container 2 (2) is
// measured. Between the measurement's reading of the stuffings that load the lot and its transaction, an unload takes
// all of container 2's packs out and commits (a real command, run when the measurement's transaction starts, before
// it holds anything), so the stuffings differ and the measurement answers "changed; try again" to itself. The second
// attempt reads the stuffings again, finds container 1 alone, and commits. One measurement version, one event of the
// measurement (and the unload's), one receipt, the lot's revision up by one: nothing the first attempt did in memory
// survived.
func TestAMeasurementIsRetriedWhenTheStuffingsChanged(t *testing.T) {
	s := busyRun(t)
	s.loadAs("ops-1", "X-2", "L-M", 2, 0, 0, 4)
	events := len(s.f.outboxTypes(t))
	deps, log := s.f.retryDeps(t, "measure-again", 3, time.Millisecond)
	unloaded := false
	log.before = func(find int) {
		if find == 2 && !unloaded { // the first attempt's transaction
			unloaded = true
			s.unload("X-2", 1, 2)
		}
	}
	_, err := application.RecordCargoMeasurement{Deps: deps}.Handle(context.Background(), application.RecordCargoMeasurementCommand{
		CommandID: "measure-again", Scope: s.scopeOf(), LotID: "L-M", ExpectedRevision: s.lot,
		Measurement: phy.Measurement{GrossWeight: kgm(48000), Method: phy.MethodScale, Evidence: s.evidenceOf("measure-again", 5)}, OccurredAt: s.f.hoursIn(5)})
	if err != nil || log.attempts() != 2 {
		t.Fatalf("%v after %d attempts, want success after 2", err, log.attempts())
	}
	lot, err := s.f.module.GetCargoLot(context.Background(), api.GetCargoLot{TenantID: s.f.tenant, LotID: "L-M"})
	if err != nil || len(lot.Measurements) != 2 || lot.Revision != s.lot+1 || lot.Measurements[1].GrossWeight != kgm(48000) {
		t.Fatalf("the lot after the measurement: %d versions, revision %d (was %d): %+v %v", len(lot.Measurements), lot.Revision, s.lot, lot.Measurements, err)
	}
	if got, rc := len(s.f.outboxTypes(t)), s.f.receipts(t, "measure-again"); got != events+2 || rc != 1 {
		t.Errorf("%d events (was %d, plus the unload's and the measurement's), %d receipts", got, events, rc)
	}
}

// TestAMeasurementWhoseStuffingsKeepChangingIsAConflict: the stuffings change again between every attempt's reading and
// its transaction (container 2's packs are taken out and put back by real commands, alternately), so every one of three
// attempts answers "changed; try again". The caller gets the conflict, after exactly three attempts, and the lot has no
// new measurement, no receipt and no event of the measurement.
func TestAMeasurementWhoseStuffingsKeepChangingIsAConflict(t *testing.T) {
	s := busyRun(t)
	s.loadAs("ops-1", "X-2", "L-M", 2, 0, 0, 4)
	deps, log := s.f.retryDeps(t, "measure-never", 3, time.Millisecond)
	events := 0
	log.before = func(find int) {
		if find < 2 {
			return
		}
		x := s.f.execution(t, "X-2")
		if live := x.Lines[len(x.Lines)-1].LivePacks; live > 0 {
			s.unload("X-2", len(x.Lines), live)
		} else {
			s.loadAs("ops-1", "X-2", "L-M", 2, 0, 0, 4)
		}
		events = len(s.f.outboxTypes(t))
	}
	_, err := application.RecordCargoMeasurement{Deps: deps}.Handle(context.Background(), application.RecordCargoMeasurementCommand{
		CommandID: "measure-never", Scope: s.scopeOf(), LotID: "L-M", ExpectedRevision: s.lot,
		Measurement: phy.Measurement{GrossWeight: kgm(48000), Method: phy.MethodScale, Evidence: s.evidenceOf("measure-never", 5)}, OccurredAt: s.f.hoursIn(5)})
	if !errors.Is(err, phy.ErrConflict) || !errors.Is(err, phy.ErrRetryable) || log.attempts() != 3 {
		t.Fatalf("%v after %d attempts, want the conflict after 3", err, log.attempts())
	}
	lot, gerr := s.f.module.GetCargoLot(context.Background(), api.GetCargoLot{TenantID: s.f.tenant, LotID: "L-M"})
	if gerr != nil || len(lot.Measurements) != 1 || lot.Revision != s.lot || s.f.receipts(t, "measure-never") != 0 || len(s.f.outboxTypes(t)) != events {
		t.Fatalf("the refused measurement wrote: %d versions, revision %d (was %d), %d events (was %d): %v", len(lot.Measurements), lot.Revision, s.lot,
			len(s.f.outboxTypes(t)), events, gerr)
	}
}

// TestAMeasurementWithAStaleRevisionIsNotRetried: one attempt, the plain conflict.
func TestAMeasurementWithAStaleRevisionIsNotRetried(t *testing.T) {
	s := busyRun(t)
	deps, log := s.f.retryDeps(t, "measure-stale", 6, time.Millisecond)
	_, err := application.RecordCargoMeasurement{Deps: deps}.Handle(context.Background(), application.RecordCargoMeasurementCommand{
		CommandID: "measure-stale", Scope: s.scopeOf(), LotID: "L-M", ExpectedRevision: s.lot - 1,
		Measurement: phy.Measurement{GrossWeight: kgm(48000), Method: phy.MethodScale, Evidence: s.evidenceOf("measure-stale", 5)}, OccurredAt: s.f.hoursIn(5)})
	if !errors.Is(err, phy.ErrConflict) || errors.Is(err, phy.ErrRetryable) || log.attempts() != 1 {
		t.Fatalf("%v after %d attempts, want the plain conflict after 1", err, log.attempts())
	}
}

// TestTwoLoadsOfOneLotRetryTheConflictThemselves is TestTwoLoadsOfOneLotQueuedBehindItsLock's second case (each
// container holds 1 pack already, and takes 3 and 1 more) with the module as it is by default: the command that gets
// the lot first needs the container the other command holds, answers the conflict inside and is tried again after the
// other committed, so both commands return success and neither caller sees a conflict. A lot of 6 packs and 301 kg: 4
// and 2 packs complete the lot and take its 301 kg, 200.67 and 100.33, the floors 200 + 100 leave 1 kg for the larger
// remainder: 201 + 100 kg, whichever container loaded first. Each load is stored once (container 1 has the line of its
// first pack and of its 3 more, container 2 two lines).
func TestTwoLoadsOfOneLotRetryTheConflictThemselves(t *testing.T) {
	for _, order := range [][2]string{{"X-1", "X-2"}, {"X-2", "X-1"}} {
		t.Run(order[0]+" first", func(t *testing.T) {
			s := newSplitRun(t, 301, 600)
			s.open("X-1", 1)
			s.open("X-2", 2)
			s.load("X-1", 1, 0, 0)
			s.load("X-2", 1, 0, 0)
			packs := map[string]int64{"X-1": 3, "X-2": 1}
			cmd := func(x string) api.RecordCargoLoaded {
				return s.f.load("load-"+x, x, s.rev[x], "L-M", packs[x], s.f.hoursIn(5))
			}
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
			for _, x := range order {
				if err := <-results[x]; err != nil {
					t.Fatalf("%s: %v", x, err)
				}
			}
			s.rev["X-1"]++
			s.rev["X-2"]++
			s.expect("both loads committed", figure{6, 301, 600}, map[int]figure{1: {4, 201, 400}, 2: {2, 100, 200}})
			for x, lines := range map[string]int{"X-1": 2, "X-2": 2} {
				if got := len(s.f.execution(t, x).Lines); got != lines {
					t.Errorf("%s has %d lines, want %d", x, got, lines)
				}
			}
		})
	}
}

// TestACommandBetweenAttemptsHoldsNothing: a load, or a receipt, has found the stuffing busy, its first attempt's
// transaction has ended and it waits two seconds at least for the second. During the wait every row the attempt locked
// is free to another connection, which takes each at once with NOWAIT: the execution it addresses, the lot, the house's
// membership ledger and the consolidation (FOR UPDATE, the strongest of the locks the command took), and the
// transaction of the attempt is over, so its connection went back to the pool when the transaction ended
// (WithinTransaction returns after the rollback). The wait is ended by cancelling the command.
func TestACommandBetweenAttemptsHoldsNothing(t *testing.T) {
	for _, c := range retriedCommands() {
		t.Run(c.name, func(t *testing.T) {
			s := busyRun(t)
			s.f.holdRow(t, lockExecution, s.f.tenant, "X-1")
			deps, log := s.f.retryDeps(t, "retried", 6, 2*time.Second)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- c.run(ctx, s, deps, c.current(s)) }()
			waitUntil(t, "the first attempt to end", func() bool { return log.ended() > c.reads })
			if log.attempts() != 1 {
				t.Fatalf("%d attempts, want the first one only", log.attempts())
			}
			if err := s.f.within(t, func(ctx context.Context, e bentopg.Executor) error {
				for _, q := range []string{
					`SELECT 1 FROM cargo_phy.load_execution WHERE tenant_id=$1 AND execution_id='X-2' FOR UPDATE NOWAIT`,
					`SELECT 1 FROM cargo_phy.cargo_lot WHERE tenant_id=$1 AND lot_id='L-M' FOR UPDATE NOWAIT`,
					`SELECT 1 FROM cargo_phy.consol_membership WHERE tenant_id=$1 AND shipment_id='SH-M' FOR UPDATE NOWAIT`,
					`SELECT 1 FROM cargo_phy.consolidation WHERE tenant_id=$1 AND consolidation_id='C-S' FOR UPDATE NOWAIT`,
				} {
					if _, err := e.Exec(ctx, q, s.f.tenant); err != nil {
						return fmt.Errorf("%s: %w", q, err)
					}
				}
				return nil
			}); err != nil {
				t.Fatalf("a row the waiting command locked is still locked: %v", err)
			}
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("the cancelled command answered %v", err)
			}
		})
	}
}

// airRun is an AIR consolidation C-1 closed with house SH-1 (6 packs), lot L-1 of its 6 packs received at the warehouse
// and weighed at 300 kg (revision 2), and tender X-1 started (revision 2) at the airline terminal with 2 packs of the
// lot loaded (revision 3). A FAILED screening result of the lot holds every tender with live packs of it.
func airRun(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	f := newFixture(t)
	f.create(t, "C-1")
	if _, err := f.module.AttachHouse(ctx, f.attach("attach", "C-1", "SH-1", 0)); err != nil {
		t.Fatal(err)
	}
	f.ful.allocate("SH-1", "C-1")
	ok(t, 2)(f.module.CloseConsolidation(ctx, f.closeCmd("close", "C-1", 1)))
	at := f.hoursIn(3)
	ok(t, 1)(f.module.ReceiveCargo(ctx, f.receive("rcv", "L-1", "SH-1", 0, 6, api.ConditionGood, at)))
	ok(t, 2)(f.module.RecordCargoMeasurement(ctx, api.RecordCargoMeasurement{CommandID: "measure", TenantID: f.tenant, ActorID: "ops-1", LotID: "L-1",
		ExpectedRevision: 1, GrossWeight: kgm(300), Method: api.MethodScale, Evidence: evidence("measure", at), OccurredAt: at}))
	f.tender(t, "X-1", "C-1", at)
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load-1", "X-1", 2, "L-1", 2, at)))
	return f
}

func screeningFailed(f *fixture, id string, expected int64) application.RecordScreeningResultCommand {
	at := f.hoursIn(5)
	return application.RecordScreeningResultCommand{CommandID: id, Scope: application.Scope{TenantID: phy.TenantID(f.tenant), ActorID: "ops-1"}, LotID: "L-1",
		ExpectedRevision: expected, OccurredAt: at, Screening: phy.Screening{Outcome: phy.ScreeningFailed, Screener: "PVG security station", Method: "XRAY",
			Reference: "SC-" + id, Evidence: phy.Evidence{Source: "PVG-SECURITY", Reference: "SC-" + id, OccurredAt: at}}}
}

// toggleTender puts 1 pack of lot L-1 into tender X-2 (opened and started when it does not exist yet) when it holds
// none, and takes it out again when it does: each is a real command that changes the set of executions with live packs
// of the lot, between the screening result's reading of the set and its transaction.
func toggleTender(t *testing.T, f *fixture, n int) {
	t.Helper()
	ctx, at := context.Background(), f.hoursIn(4)
	if _, err := f.module.GetLoadExecution(ctx, api.GetLoadExecution{TenantID: f.tenant, ExecutionID: "X-2"}); err != nil {
		f.tender(t, "X-2", "C-1", at)
	}
	x := f.execution(t, "X-2")
	if live := len(x.Lines) > 0 && x.Lines[len(x.Lines)-1].LivePacks > 0; live {
		last := x.Lines[len(x.Lines)-1]
		if _, err := f.module.RecordCargoUnloaded(ctx, api.RecordCargoUnloaded{CommandID: fmt.Sprint("untoggle-", n), TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-2",
			ExpectedRevision: x.Revision, LineNo: last.LineNo, Packs: last.LivePacks, Reason: api.ReversalMisloaded, Evidence: evidence(fmt.Sprint("tally-", n), at), OccurredAt: at}); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := f.module.RecordCargoLoaded(ctx, f.load(fmt.Sprint("toggle-", n), "X-2", x.Revision, "L-1", 1, at)); err != nil {
		t.Fatal(err)
	}
}

// TestAFailedScreeningResultIsRetriedWhenTheExecutionsChanged: a FAILED result of lot L-1 reads the tenders with live
// packs of it (X-1), and before its transaction a real load puts a pack of the lot into a second tender, X-2. The first
// attempt finds "the executions loading lot L-1 changed", a retryable conflict; the second reads both tenders, holds
// each once and commits. By hand: two attempts; the lot's revision 2 + 1 = 3; one screening and one receipt; each of
// the two tenders ON_HOLD with exactly one hold.
func TestAFailedScreeningResultIsRetriedWhenTheExecutionsChanged(t *testing.T) {
	f := airRun(t)
	deps, log := f.retryDeps(t, "screen-failed", 3, time.Millisecond)
	toggled := false
	log.before = func(find int) {
		if find == 2 && !toggled { // the first attempt's transaction
			toggled = true
			toggleTender(t, f, 1)
		}
	}
	_, err := application.RecordScreeningResult{Deps: deps}.Handle(context.Background(), screeningFailed(f, "screen-failed", 2))
	if err != nil || log.attempts() != 2 {
		t.Fatalf("%v after %d attempts, want success after 2", err, log.attempts())
	}
	lot, gerr := f.module.GetCargoLot(context.Background(), api.GetCargoLot{TenantID: f.tenant, LotID: "L-1"})
	if gerr != nil || len(lot.Screenings) != 1 || lot.Revision != 3 || f.receipts(t, "screen-failed") != 1 {
		t.Fatalf("the lot: %d screenings, revision %d (want 3), %d receipts: %v", len(lot.Screenings), lot.Revision, f.receipts(t, "screen-failed"), gerr)
	}
	for _, x := range []string{"X-1", "X-2"} {
		if got := f.execution(t, x); got.State != api.ExecutionOnHold || len(got.Holds) != 1 {
			t.Errorf("%s is %s with %d holds, want ON_HOLD with one", x, got.State, len(got.Holds))
		}
	}
}

// TestAFailedScreeningResultWhoseExecutionsKeepChangingIsAConflict: the set of tenders with live packs of the lot
// changes again between every attempt's reading and its transaction (a pack goes into X-2 and out again, by real
// commands, alternately), so every one of three attempts answers "changed; try again". The caller gets the conflict
// after exactly three attempts, and the lot has no new screening, no receipt, and no tender is held.
func TestAFailedScreeningResultWhoseExecutionsKeepChangingIsAConflict(t *testing.T) {
	f := airRun(t)
	deps, log := f.retryDeps(t, "screen-never", 3, time.Millisecond)
	log.before = func(find int) {
		if find >= 2 {
			toggleTender(t, f, find)
		}
	}
	_, err := application.RecordScreeningResult{Deps: deps}.Handle(context.Background(), screeningFailed(f, "screen-never", 2))
	if !errors.Is(err, phy.ErrConflict) || !errors.Is(err, phy.ErrRetryable) || log.attempts() != 3 {
		t.Fatalf("%v after %d attempts, want the conflict after 3", err, log.attempts())
	}
	lot, gerr := f.module.GetCargoLot(context.Background(), api.GetCargoLot{TenantID: f.tenant, LotID: "L-1"})
	if gerr != nil || len(lot.Screenings) != 0 || lot.Revision != 2 || f.receipts(t, "screen-never") != 0 {
		t.Fatalf("the refused result wrote: %d screenings, revision %d (want 2), %d receipts: %v", len(lot.Screenings), lot.Revision, f.receipts(t, "screen-never"), gerr)
	}
	for _, x := range []string{"X-1", "X-2"} {
		if got := f.execution(t, x); got.State != api.ExecutionLoading || len(got.Holds) != 0 {
			t.Errorf("%s is %s with %d holds, want LOADING with none", x, got.State, len(got.Holds))
		}
	}
}
