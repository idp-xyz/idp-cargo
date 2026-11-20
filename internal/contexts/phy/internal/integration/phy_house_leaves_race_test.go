//go:build integration

package integration

import (
	"context"
	"testing"

	"go.idp.xyz/idp-cargo/internal/contexts/phy/api"
)

// The locks of a house that leaves a CLOSED consolidation after a rework (V2.86): the commands wait behind a lock held
// on a second connection, in each order, so that the order is certain.

const lockLedger = `SELECT 1 FROM cargo_phy.consol_membership WHERE tenant_id=$1 AND shipment_id=$2 AND stage='MAIN' FOR UPDATE`

// TestADetachAfterReworkAndALoadOfTheSameHouseElsewhereWaitForEachOther: H03 was unloaded from X-1 (still in REWORK)
// and its lot, 2 packs, can be loaded into X-2 of the same consolidation. The house's ledger is held and the detach and
// the load queue behind it in each order. The detach first: H03 leaves, and the load is refused, nothing of H03 in X-2.
// The load first: the detach is refused for the packs loaded, and H03 stays with its load. Never a house that left with
// a live load.
func TestADetachAfterReworkAndALoadOfTheSameHouseElsewhereWaitForEachOther(t *testing.T) {
	ctx := context.Background()
	for _, detachFirst := range []bool{true, false} {
		name := map[bool]string{true: "the detach first", false: "the load first"}[detachFirst]
		t.Run(name, func(t *testing.T) {
			f := reworkDay(t)
			f.withoutRetry(t)
			f.begin(t, "X-1", 1)
			f.begin(t, "X-2", 2)
			at := f.hoursIn(4)
			ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("a-h03", "X-1", 2, "L-H03", 2, at)))
			ok(t, 4)(f.module.RecordCargoLoaded(ctx, f.load("a-h01", "X-1", 3, "L-H01", 4, at)))
			ok(t, 5)(f.module.CloseLoadExecution(ctx, f.sealOf("X-1", 4, "SEAL-1")))
			ok(t, 6)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-1", "X-1", 5, "x", "ops-asker", f.hoursIn(6))))
			ok(t, 7)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("u-h03", "X-1", 6, "H03", f.hoursIn(7))))
			detach := func() error {
				_, err := f.module.DetachHouse(ctx, f.detach("detach-h03", "C-S", "H03", 1, api.ReasonRollover, "", f.hoursIn(8)))
				return err
			}
			load := func() error {
				_, err := f.module.RecordCargoLoaded(ctx, f.load("b-h03", "X-2", 2, "L-H03", 2, f.hoursIn(8)))
				return err
			}
			pid, release := f.holdRow(t, lockLedger, f.tenant, "H03")
			var got []error
			if detachFirst {
				got = f.queue(t, pid, release, detach, load)
			} else {
				got = f.queue(t, pid, release, load, detach)
			}
			m, x2 := f.membership(t, "H03"), f.execution(t, "X-2")
			live := x2.TenderedPacks
			if detachFirst {
				f.refusedCommand(t, "the load after the detach", got[1], api.ErrRejected, "")
				if got[0] != nil || m.Live || live != 0 || m.History[0].ReworkNo != 1 {
					t.Fatalf("the detach first: %v, %+v, X-2 %d packs", got[0], m, live)
				}
			} else {
				f.refusedCommand(t, "the detach after the load", got[1], api.ErrRejected, "packs")
				if got[0] != nil || !m.Live || live != 2 || m.Entry.End != "" {
					t.Fatalf("the load first: %v, %+v, X-2 %d packs", got[0], m, live)
				}
			}
			if !m.Live && live > 0 {
				t.Fatal("a house that left with a live load")
			}
		})
	}
}

// TestADetachAfterReworkAndTheReworkOfAnotherContainerDoNotBlockEachOther: X-1 (H02, H03) is in rework and H03 is
// unloaded; X-2 (H01) is sealed. The consolidation's row is held with a lock stronger than the commands' share, and the
// detach of H03 and the opening of X-2 for rework queue behind it in each order. Both commit either way: the house that
// left, the other container in rework with its cargo, no deadlock.
func TestADetachAfterReworkAndTheReworkOfAnotherContainerDoNotBlockEachOther(t *testing.T) {
	ctx := context.Background()
	for _, detachFirst := range []bool{true, false} {
		name := map[bool]string{true: "the detach first", false: "the rework first"}[detachFirst]
		t.Run(name, func(t *testing.T) {
			f := reworkDay(t)
			f.withoutRetry(t)
			f.begin(t, "X-1", 1)
			f.begin(t, "X-2", 2)
			at := f.hoursIn(4)
			ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("a-h03", "X-1", 2, "L-H03", 2, at)))
			ok(t, 4)(f.module.RecordCargoLoaded(ctx, f.load("a-h02", "X-1", 3, "L-H02", 6, at)))
			ok(t, 5)(f.module.CloseLoadExecution(ctx, f.sealOf("X-1", 4, "SEAL-1")))
			ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("b-h01", "X-2", 2, "L-H01", 4, at)))
			ok(t, 4)(f.module.CloseLoadExecution(ctx, f.sealOf("X-2", 3, "SEAL-2")))
			ok(t, 6)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-1", "X-1", 5, "x", "ops-asker", f.hoursIn(6))))
			ok(t, 7)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("u-h03", "X-1", 6, "H03", f.hoursIn(7))))
			detach := func() error {
				_, err := f.module.DetachHouse(ctx, f.detach("detach-h03", "C-S", "H03", 1, api.ReasonRollover, "", f.hoursIn(8)))
				return err
			}
			rework := func() error {
				_, err := f.module.OpenStuffingForRework(ctx, f.openRework("rework-2", "X-2", 4, "y", "ops-asker", f.hoursIn(8)))
				return err
			}
			pid, release := f.holdRow(t, `SELECT 1 FROM cargo_phy.consolidation WHERE tenant_id=$1 AND consolidation_id=$2 FOR NO KEY UPDATE`, f.tenant, "C-S")
			var got []error
			if detachFirst {
				got = f.queue(t, pid, release, detach, rework)
			} else {
				got = f.queue(t, pid, release, rework, detach)
			}
			if got[0] != nil || got[1] != nil {
				t.Fatalf("%v, %v", got[0], got[1])
			}
			m, x1, x2 := f.membership(t, "H03"), f.execution(t, "X-1"), f.execution(t, "X-2")
			if m.Live || m.History[0].ReworkExecutionID != "X-1" || x1.State != api.ExecutionRework || x2.State != api.ExecutionRework || x2.TenderedPacks != 4 || x1.TenderedPacks != 6 {
				t.Fatalf("%+v, X-1 %s %d, X-2 %s %d", m, x1.State, x1.TenderedPacks, x2.State, x2.TenderedPacks)
			}
			if c := f.consolidation(t, "C-S"); c.State != api.StateClosed || len(c.Members) != 2 {
				t.Fatalf("C-S: %+v", c)
			}
		})
	}
}

// TestTheEntryRuleLocksTheLedgerWithoutWaiting: a raw end of H03's entry, with the rework named, needs the house's
// ledger, which a load of the house takes first. With the ledger held on a second connection the end is refused at once
// (lock_not_available), not left to wait into a deadlock with a command that holds the ledger and waits for the entry's
// row; with it free, the end is accepted, and so is one by a writer that locked the consolidation and the ledger itself
// first, as the commands do.
func TestTheEntryRuleLocksTheLedgerWithoutWaiting(t *testing.T) {
	f := leaveDay(t)
	f.reseal(t, 8)
	end := endEntry("H03", 1, "DETACHED", "X-1", 1, "attached_at + interval '10 hours'")
	_, release := f.holdRow(t, lockLedger, f.tenant, "H03")
	f.refused(t, "the ledger held by another transaction", "55P03", "could not obtain lock", `SELECT set_config('lock_timeout', '1500ms', true) WHERE $1 <> ''`, end)
	release()
	f.accepted(t, "the end with the ledger free", end)
	if err := f.rawTx(t, `SELECT 1 FROM cargo_phy.consolidation WHERE tenant_id=$1 AND consolidation_id='C-S' FOR SHARE`,
		`SELECT 1 FROM cargo_phy.consol_membership WHERE tenant_id=$1 AND shipment_id='H03' AND stage='MAIN' FOR UPDATE`, end); err != nil {
		t.Fatalf("the end by a writer that locks in the commands' order: %v", err)
	}
	if m := f.membership(t, "H03"); m.Live || m.History[0].ReworkNo != 1 {
		t.Fatalf("H03: %+v", m)
	}
}

// TestTheEntryRuleReadsTheConsolidationUnderItsShareLock: the consolidation's row is held with a lock that its share
// lock waits for (the lock a reopen, a cancel or a reconcile takes), and a raw end of H03's entry waits behind it, so
// that the state it reads is the one the other transaction leaves. The trigger takes the locks in the commands' order,
// the consolidation first: while it waits, it does not yet hold the house's ledger, which another transaction takes
// without waiting (a trigger that took the ledger first would hold it here, and deadlock with a command that holds the
// consolidation and waits for the ledger). Released, the end is accepted.
func TestTheEntryRuleReadsTheConsolidationUnderItsShareLock(t *testing.T) {
	f := leaveDay(t)
	f.reseal(t, 8)
	pid, release := f.holdRow(t, `SELECT 1 FROM cargo_phy.consolidation WHERE tenant_id=$1 AND consolidation_id=$2 FOR NO KEY UPDATE`, f.tenant, "C-S")
	done := make(chan error, 1)
	go func() {
		done <- f.rawTx(t, endEntry("H03", 1, "DETACHED", "X-1", 1, "attached_at + interval '10 hours'"))
	}()
	f.waitingBehind(t, pid, 1)
	if err := f.rawTx(t, `SELECT 1 FROM cargo_phy.consol_membership WHERE tenant_id=$1 AND shipment_id='H03' AND stage='MAIN' FOR UPDATE NOWAIT`); err != nil {
		t.Errorf("the ledger is held by a trigger that still waits for the consolidation: %v", err)
	}
	release()
	if err := <-done; err != nil {
		t.Fatalf("the end of the entry once the consolidation is free: %v", err)
	}
}
