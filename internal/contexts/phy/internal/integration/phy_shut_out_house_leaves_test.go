//go:build integration

package integration

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/phy/api"
)

// A house shut out before stuffing leaves the CLOSED consolidation (V2.87), with the real commands and the real
// database. The day: C-S, an own-container consolidation with H01 and H02 (10 and 15 W/M) stuffed in X-1 and sealed,
// and H04 (2 packs, 5 W/M) attached and never loaded; the consolidation is CLOSED, and it cannot reconcile while H04 is
// attached.

func init() { houseFigures["H04"] = [3]int64{2, 100, 5_000_000} }

// receiveLot receives, weighs and completes the lot L-<house> at the CFS (the lot at revision 3).
func (f *fixture) receiveLot(t *testing.T, h string) {
	t.Helper()
	ctx := context.Background()
	lot, fig, at := "L-"+h, houseFigures[h], f.hoursIn(3)
	rcv := f.receive("rcv-"+lot, lot, h, 0, fig[0], api.ConditionGood, at)
	rcv.Facility = cfsShanghai
	ok(t, 1)(f.module.ReceiveCargo(ctx, rcv))
	ok(t, 2)(f.module.RecordCargoMeasurement(ctx, api.RecordCargoMeasurement{CommandID: "measure-" + lot, TenantID: f.tenant, ActorID: "ops-1",
		LotID: lot, ExpectedRevision: 1, GrossWeight: kgm(fig[1]), Volume: cmq(fig[2]), Method: api.MethodScale, Evidence: evidence("measure-"+lot, at), OccurredAt: at}))
	ok(t, 3)(f.module.CompleteReceipt(ctx, api.CompleteReceipt{CommandID: "complete-" + lot, TenantID: f.tenant, ActorID: "ops-1", LotID: lot,
		ExpectedRevision: 2, OccurredAt: at}))
}

// attachAll plans two 22G1 on C-S (revision 3), attaches the houses (each allocated in FUL's stub) and closes it
// (revision 4).
func (f *fixture) attachAll(t *testing.T, houses ...string) {
	t.Helper()
	ctx := context.Background()
	f.createSea(t, "C-S", api.KindSeaLCLOwn)
	ok(t, 2)(f.module.PlanLoadUnit(ctx, f.planUnit("plan-1", "C-S", 1, "22G1")))
	ok(t, 3)(f.module.PlanLoadUnit(ctx, f.planUnit("plan-2", "C-S", 2, "22G1")))
	for i, h := range houses {
		f.ful.sea[h] = true
		f.ful.packs[h] = houseFigures[h][0]
		if _, err := f.module.AttachHouse(ctx, f.attach(fmt.Sprint("attach-", i), "C-S", h, 0)); err != nil {
			t.Fatal(err)
		}
		f.ful.allocate(h, "C-S")
	}
	ok(t, 4)(f.module.CloseConsolidation(ctx, f.closeCmd("close", "C-S", 3)))
}

// shutOutDay builds C-S CLOSED (revision 4) with H01, H02 and H04 attached, X-1 stuffed with H01 and H02 and sealed
// (SEAL-1, X-1 at revision 5), the lots of H01 and H02 received (each at revision 3), and H04's lot received too when
// receiveH04 says so (it is, in the day whose house moves to another consolidation; it never is in the day that is a
// shut-out at the gate).
func shutOutDay(t *testing.T, receiveH04 bool) *fixture {
	t.Helper()
	ctx := context.Background()
	f := newFixture(t)
	f.attachAll(t, "H01", "H02", "H04")
	f.receiveLot(t, "H01")
	f.receiveLot(t, "H02")
	if receiveH04 {
		f.receiveLot(t, "H04")
	}
	f.begin(t, "X-1", 1)
	at := f.hoursIn(4)
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load-H01", "X-1", 2, "L-H01", 4, at)))
	ok(t, 4)(f.module.RecordCargoLoaded(ctx, f.load("load-H02", "X-1", 3, "L-H02", 6, at)))
	ok(t, 5)(f.module.CloseLoadExecution(ctx, f.sealOf("X-1", 4, "SEAL-1")))
	return f
}

func (f *fixture) reconcile(command string, expected int64, hour int) (api.Receipt, error) {
	return f.module.ReconcileConsolidation(context.Background(), api.ReconcileConsolidation{CommandID: command, TenantID: f.tenant, ActorID: "ops-1",
		ConsolidationID: "C-S", ExpectedRevision: expected, OccurredAt: f.hoursIn(hour)})
}

// TestAHouseShutOutBeforeStuffingLeavesTheClosedConsolidation: H04 is attached and never loaded, the container is
// sealed with H01 and H02, and the consolidation cannot reconcile; H04 leaves with the operator's reason, no rework
// recorded, and the consolidation reconciles with the two houses loaded as planned.
func TestAHouseShutOutBeforeStuffingLeavesTheClosedConsolidation(t *testing.T) {
	ctx := context.Background()
	f := shutOutDay(t, false)
	typesBefore := len(f.types(t))

	// the consolidation cannot reconcile while H04 is attached
	_, err := f.reconcile("reconcile-early", 4, 6)
	f.refusedCommand(t, "reconcile with H04 attached and never loaded", err, api.ErrRejected, "")
	// a house with live packs stays frozen, whatever the reason
	_, err = f.module.DetachHouse(ctx, f.detach("detach-h01", "C-S", "H01", 1, api.ReasonShutOut, "", f.hoursIn(6)))
	f.refusedCommand(t, "H01, loaded", err, api.ErrRejected, "packs")
	_, err = f.module.TransferHouse(ctx, f.transfer("transfer-h02", "C-S", "C-S", "H02", 1, api.ReasonRollover, "", f.hoursIn(6)))
	f.refusedCommand(t, "H02, loaded", err, api.ErrRejected, "packs")
	// the reasons: PHY's own, OTHER with no text, one outside the list, none, a text too long
	for name, tc := range map[string]struct{ reason, text string }{
		"SHIPMENT_CANCELLED is PHY's own": {api.ReasonShipmentCancelled, ""},
		"OTHER without a text":            {api.ReasonOther, ""},
		"NO_SUCH_REASON":                  {"NO_SUCH_REASON", ""},
		"no reason":                       {"", ""},
		"a text of 501 characters":        {api.ReasonOther, strings.Repeat("x", 501)},
	} {
		_, err = f.module.DetachHouse(ctx, f.detach("detach-"+name, "C-S", "H04", 1, tc.reason, tc.text, f.hoursIn(6)))
		f.refusedCommand(t, name, err, api.ErrRejected, "reason")
	}
	_, err = f.module.DetachHouse(ctx, f.detach("detach-nobody", "C-S", "H04", 1, api.ReasonShutOut, "", f.at))
	f.refusedCommand(t, "before the consolidation was created", err, api.ErrRejected, "invalid change")
	_, err = f.module.DetachHouse(ctx, f.detach("detach-stale", "C-S", "H04", 7, api.ReasonShutOut, "", f.hoursIn(6)))
	f.refusedCommand(t, "an expected revision that is not the ledger's", err, api.ErrConflict, "")
	f.unchanged(t, "the refusals", "H04", 1, typesBefore)

	// the operator's reason: H04 leaves
	cmd := f.detach("detach-h04", "C-S", "H04", 1, api.ReasonShutOut, "", f.hoursIn(6))
	r := ok(t, 2)(f.module.DetachHouse(ctx, cmd))
	if r.ConsolidationID != "C-S" || r.ShipmentID != "H04" || r.EntryNo != 1 {
		t.Fatalf("receipt = %+v", r)
	}
	if again, err := f.module.DetachHouse(ctx, cmd); err != nil || again != r {
		t.Fatalf("replay = %+v, %v; want %+v", again, err, r)
	}
	m := f.membership(t, "H04")
	if e := m.History[0]; m.Live || len(m.History) != 1 || m.Revision != 2 || e.End != api.EntryDetached || e.Reason != api.ReasonShutOut || e.ConsolidationID != "C-S" ||
		!e.EndedAt.Equal(f.hoursIn(6)) || e.ReworkExecutionID != "" || e.ReworkNo != 0 || e.SuccessorNo != 0 || e.Packs != 2 {
		t.Fatalf("the entry that ended names no rework: %+v", m)
	}
	if got, want := f.types(t)[typesBefore:], []string{api.HouseDetached}; !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if p := f.payloads(t, api.HouseDetached); len(p) != 1 || !strings.Contains(p[0], "SHUT_OUT") || !strings.Contains(p[0], "H04") || !strings.Contains(p[0], "C-S") {
		t.Fatalf("HouseDetached.v1 carries the reason: %v", p)
	}
	if c := f.consolidation(t, "C-S"); c.State != api.StateClosed || len(c.Members) != 2 || c.Revision != 4 {
		t.Fatalf("C-S: %+v", c)
	}
	basis := f.basis(t)
	if len(basis.Houses) != 2 || basis.Houses[0].ShipmentID != "H01" || basis.Houses[1].ShipmentID != "H02" || wm(basis.Houses[0].LoadedGrossWeight, basis.Houses[0].LoadedVolume) != 10 ||
		wm(basis.Houses[1].LoadedGrossWeight, basis.Houses[1].LoadedVolume) != 15 {
		t.Fatalf("the cost basis lists the two houses that are attached, at 10 and 15 W/M: %+v", basis.Houses)
	}
	_, err = f.module.DetachHouse(ctx, f.detach("detach-again", "C-S", "H04", 2, api.ReasonShutOut, "", f.hoursIn(7)))
	f.refusedCommand(t, "a second detach", err, api.ErrRejected, "")

	// the consolidation reconciles: H01 and H02 are loaded as planned, X-1 is sealed
	reconciled := ok(t, 5)(f.reconcile("reconcile", 4, 8))
	if reconciled.ConsolidationID != "C-S" || f.consolidation(t, "C-S").State != api.StateReconciled {
		t.Fatalf("C-S reconciled: %+v", reconciled)
	}
	// RECONCILED: the houses stay, and H04's history does not change
	_, err = f.module.DetachHouse(ctx, f.detach("detach-reconciled", "C-S", "H01", 1, api.ReasonShutOut, "", f.hoursIn(9)))
	f.refusedCommand(t, "H01 of a reconciled consolidation", err, api.ErrRejected, "")
	if again := f.membership(t, "H04"); again.Revision != 2 || again.History[0].ReworkNo != 0 {
		t.Fatalf("H04: %+v", again)
	}
}

// TestAHouseShutOutBeforeStuffingIsTransferredStuffedAndSealed: H04, with its lot received, moves to the OPEN
// consolidation C-T, which is closed, stuffed with the lot and sealed; C-S reconciles with H01 and H02 and C-T with H04
// at 5 W/M.
func TestAHouseShutOutBeforeStuffingIsTransferredStuffedAndSealed(t *testing.T) {
	ctx := context.Background()
	f := shutOutDay(t, true)
	f.targetOf(t)
	typesBefore := len(f.types(t))
	f.ful.sea["H04"] = true

	// the target is OPEN, of the mode and admits the house; the reason is the operator's
	_, err := f.module.TransferHouse(ctx, f.transfer("transfer-same", "C-S", "C-S", "H04", 1, api.ReasonShutOut, "", f.hoursIn(6)))
	f.refusedCommand(t, "to the same consolidation", err, api.ErrRejected, "")
	_, err = f.module.TransferHouse(ctx, f.transfer("transfer-no-text", "C-S", "C-T", "H04", 1, api.ReasonOther, "", f.hoursIn(6)))
	f.refusedCommand(t, "OTHER without a text", err, api.ErrRejected, "")
	_, err = f.module.TransferHouse(ctx, f.transfer("transfer-own", "C-S", "C-T", "H04", 1, api.ReasonShipmentCancelled, "", f.hoursIn(6)))
	f.refusedCommand(t, "SHIPMENT_CANCELLED is PHY's own", err, api.ErrRejected, "")
	_, err = f.module.TransferHouse(ctx, f.transfer("transfer-h01", "C-S", "C-T", "H01", 1, api.ReasonRollover, "", f.hoursIn(6)))
	f.refusedCommand(t, "H01, loaded in the sealed container", err, api.ErrRejected, "packs")
	f.create(t, "C-AIR")
	_, err = f.module.TransferHouse(ctx, f.transfer("transfer-air", "C-S", "C-AIR", "H04", 1, api.ReasonShutOut, "", f.hoursIn(6)))
	f.refusedCommand(t, "to a consolidation of another mode", err, api.ErrRejected, "")
	f.unchanged(t, "the refusals", "H04", 1, typesBefore+1) // the air consolidation's creation is an event

	cmd := f.transfer("transfer-h04", "C-S", "C-T", "H04", 1, api.ReasonWrongConsol, "", f.hoursIn(6))
	r := ok(t, 2)(f.module.TransferHouse(ctx, cmd))
	if r.ConsolidationID != "C-T" || r.ShipmentID != "H04" || r.EntryNo != 2 {
		t.Fatalf("receipt = %+v", r)
	}
	if again, err := f.module.TransferHouse(ctx, cmd); err != nil || again != r {
		t.Fatalf("replay = %+v, %v; want %+v", again, err, r)
	}
	m := f.membership(t, "H04")
	if !m.Live || m.Entry.ConsolidationID != "C-T" || m.Entry.EntryNo != 2 || m.Entry.Start != api.EntryTransferred || m.Entry.PredecessorNo != 1 || m.Entry.ReworkNo != 0 ||
		len(m.History) != 2 {
		t.Fatalf("H04 is in C-T: %+v", m)
	}
	if e := m.History[0]; e.End != api.EntryTransferred || e.Reason != api.ReasonWrongConsol || e.ConsolidationID != "C-S" || e.SuccessorNo != 2 || e.ReworkExecutionID != "" ||
		e.ReworkNo != 0 || !e.EndedAt.Equal(f.hoursIn(6)) {
		t.Fatalf("the entry in C-S records no rework: %+v", e)
	}
	if got, want := f.types(t)[typesBefore+1:], []string{api.HouseTransferred}; !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if l := f.lot(t, "L-H04"); l.State != "RECEIVED" || l.ReceivedPacks != 2 || l.LoadedPacks != 0 {
		t.Fatalf("the lot of H04 stays RECEIVED, loaded nowhere: %+v", l)
	}

	f.ful.allocate("H04", "C-T")
	closeT := f.closeCmd("close-t", "C-T", 2)
	closeT.OccurredAt = f.hoursIn(10)
	ok(t, 3)(f.module.CloseConsolidation(ctx, closeT))
	open := f.stuff("open-xt", "X-T", 1, f.hoursIn(11))
	open.ConsolidationID = "C-T"
	ok(t, 1)(f.module.OpenLoadExecution(ctx, open))
	ok(t, 2)(f.module.StartLoading(ctx, api.StartLoading{CommandID: "start-xt", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-T", ExpectedRevision: 1, OccurredAt: f.hoursIn(11)}))
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load-xt-h04", "X-T", 2, "L-H04", 2, f.hoursIn(12))))
	seal := f.sealOf("X-T", 3, "SEAL-T")
	seal.OccurredAt, seal.Evidence.OccurredAt = f.hoursIn(13), f.hoursIn(13)
	ok(t, 4)(f.module.CloseLoadExecution(ctx, seal))
	tb := f.basisOf(t, "C-T")
	if len(tb.Houses) != 1 || tb.Houses[0].ShipmentID != "H04" || tb.Houses[0].LoadedPacks != 2 || wm(tb.Houses[0].LoadedGrossWeight, tb.Houses[0].LoadedVolume) != 5 {
		t.Fatalf("C-T's cost basis: H04 at 5 W/M: %+v", tb.Houses)
	}
	packing(t, "C-T", tb.PackingLists[0], "X-T", api.ExecutionClosed, "H04")

	ok(t, 5)(f.reconcile("reconcile", 4, 14))
	if c := f.consolidation(t, "C-S"); c.State != api.StateReconciled || len(c.Members) != 2 {
		t.Fatalf("C-S: %+v", c)
	}
	ok(t, 4)(f.module.ReconcileConsolidation(ctx, api.ReconcileConsolidation{CommandID: "reconcile-t", TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: "C-T",
		ExpectedRevision: 3, OccurredAt: f.hoursIn(14)}))
}

// TestAHouseOfAnAirConsolidationWithNoReworkStaysFrozen: in a CLOSED AIR_CONSOL a house with no live packs and no
// rework stays attached, for the command and for the database (AIR's documents were not analysed for it).
func TestAHouseOfAnAirConsolidationWithNoReworkStaysFrozen(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.create(t, "C-A")
	f.create(t, "C-B")
	if _, err := f.module.AttachHouse(ctx, f.attach("attach-1", "C-A", "SH-1", 0)); err != nil {
		t.Fatal(err)
	}
	f.ful.allocate("SH-1", "C-A")
	ok(t, 2)(f.module.CloseConsolidation(ctx, f.closeCmd("close", "C-A", 1)))
	_, err := f.module.DetachHouse(ctx, f.detach("detach", "C-A", "SH-1", 1, api.ReasonShutOut, "", f.hoursIn(6)))
	f.refusedCommand(t, "a house of a CLOSED air consolidation, never loaded", err, api.ErrRejected, "not open")
	_, err = f.module.TransferHouse(ctx, f.transfer("transfer", "C-A", "C-B", "SH-1", 1, api.ReasonShutOut, "", f.hoursIn(6)))
	f.refusedCommand(t, "the same, transferred", err, api.ErrRejected, "not open")
	f.unchanged(t, "the refusals", "SH-1", 1, len(f.outboxTypes(t)))
	f.refused(t, "a raw end of it, with no rework", "23514", "membership_entry_closed_sea_only", endEntry("SH-1", 1, "DETACHED", "", 0, "attached_at + interval '10 hours'"))
	f.refused(t, "a raw transfer of it, with no rework", "23514", "membership_entry_closed_sea_only", endEntry("SH-1", 1, "TRANSFERRED", "", 0, "attached_at + interval '10 hours'"))
	f.refused(t, "a raw end of it, naming a rework that never existed", "23514", "membership_entry_rework_link", endEntry("SH-1", 1, "DETACHED", "X-1", 1, "attached_at + interval '10 hours'"))
	if m := f.membership(t, "SH-1"); !m.Live || m.Revision != 1 {
		t.Fatalf("SH-1 stays: %+v", m)
	}
}

// TestTheDatabaseRulesOfAHouseShutOut: the raw writes of the application role to the entry of H04 (never loaded) and of
// H01 (loaded) in the CLOSED sea consolidation, one rule at a time, and what the database accepts that only the domain
// keeps.
func TestTheDatabaseRulesOfAHouseShutOut(t *testing.T) {
	f := shutOutDay(t, false)
	at := "attached_at + interval '10 hours'"
	// no live packs, no reworks anywhere: the end with no rework is accepted, as a detach and as a transfer (the
	// successor's entry is the domain's: nothing here inserts it)
	f.accepted(t, "H04 ends as DETACHED with no rework", endEntry("H04", 1, "DETACHED", "", 0, at))
	f.accepted(t, "H04 ends as TRANSFERRED with no rework", endEntry("H04", 1, "TRANSFERRED", "", 0, at))
	// the rule of the rework: an end that names one, where the house was unloaded by none, is refused
	f.refused(t, "H04 naming a rework that does not exist", "23514", "membership_entry_rework_link", endEntry("H04", 1, "DETACHED", "X-1", 1, at))
	f.refused(t, "H04 naming a rework of an execution that does not exist", "23514", "membership_entry_rework_link",
		endEntry("H04", 1, "DETACHED", "NO_SUCH_EXECUTION", 1, at))
	f.refused(t, "an execution without its number", "23514", "membership_entry_rework",
		`UPDATE cargo_phy.membership_entry SET end_kind='DETACHED', end_reason='ROLLOVER', ended_by='raw', ended_at=attached_at + interval '10 hours', end_entered_at=now(),
          end_rework_execution_id='X-1' WHERE tenant_id=$1 AND shipment_id='H04' AND entry_no=1`)
	// the rule of the packs: H01 is loaded, with no rework or with one that never existed, and the entry's own rule
	// says so before the commit's
	f.refused(t, "H01, loaded, with no rework", "23514", "membership_entry_rework_loaded", endEntry("H01", 1, "DETACHED", "", 0, at))
	f.refused(t, "H01, loaded, naming a rework that never existed", "23514", "membership_entry_rework_link", endEntry("H01", 1, "DETACHED", "X-1", 1, at))
	// the other ends and states: a refresh of the entry is no end of it; SHIPMENT_CANCELLED ends as before
	f.refused(t, "an end that is no detach, with a link", "23514", "membership_entry_rework",
		`UPDATE cargo_phy.membership_entry SET end_kind='SHIPMENT_CANCELLED', end_reason='SHIPMENT_CANCELLED', ended_by='raw', ended_at=attached_at + interval '10 hours',
          end_entered_at=now(), end_rework_execution_id='X-1', end_rework_no=1 WHERE tenant_id=$1 AND shipment_id='H04' AND entry_no=1`)
	f.refused(t, "an end as REFRESHED of a house of a CLOSED consolidation", "23514", "membership_entry_frozen",
		`UPDATE cargo_phy.membership_entry SET end_kind='REFRESHED', ended_by='raw', ended_at=attached_at + interval '10 hours', end_entered_at=now(), successor_no=2
         WHERE tenant_id=$1 AND shipment_id='H04' AND entry_no=1`)
	f.refused(t, "a second end of an entry that ended", "23001", "can only be ended, once",
		endEntry("H04", 1, "DETACHED", "", 0, at), endEntry("H04", 1, "DETACHED", "", 0, at+" + interval '1 hour'"))
	// what only the domain keeps, run: the database refuses an end dated before the house was attached (the checks of
	// the entry's end), but it accepts one dated long after the consolidation departed, and the TRANSFERRED end above
	// that no new entry continues
	f.refused(t, "an end dated before the house was attached", "23514", "membership_entry_end", endEntry("H04", 1, "DETACHED", "", 0, "attached_at - interval '1 hour'"))
	f.accepted(t, "an end dated long after the departure", endEntry("H04", 1, "DETACHED", "", 0, "attached_at + interval '30 days'"))
	// the rule holds for a ledger the command ended: after the leaving the live packs rule still sees H01
	ok(t, 2)(f.module.DetachHouse(context.Background(), f.detach("detach-h04", "C-S", "H04", 1, api.ReasonShutOut, "", f.hoursIn(6))))
	f.refused(t, "H01, loaded, with the consolidation holding one house less", "23514", "membership_entry_rework_loaded", endEntry("H01", 1, "DETACHED", "", 0, at))
}

// TestAHouseOfACoLoadConsolidationNeverTenderedLeaves: the co-loader's consolidation C-K is CLOSED with H01 tendered
// at the CFS and H04 attached and never tendered; H04 leaves with the operator's reason and no rework, and C-K
// reconciles.
func TestAHouseOfACoLoadConsolidationNeverTenderedLeaves(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.createSea(t, "C-K", api.KindSeaLCLCoload)
	for i, h := range []string{"H01", "H04"} {
		f.ful.sea[h] = true
		f.ful.packs[h] = houseFigures[h][0]
		if _, err := f.module.AttachHouse(ctx, f.attach(fmt.Sprint("attach-", i), "C-K", h, 0)); err != nil {
			t.Fatal(err)
		}
		f.ful.allocate(h, "C-K")
	}
	ok(t, 2)(f.module.CloseConsolidation(ctx, f.closeCmd("close", "C-K", 1)))
	f.receiveLot(t, "H01")
	at := f.hoursIn(4)
	ok(t, 1)(f.module.OpenLoadExecution(ctx, api.OpenLoadExecution{CommandID: "open-xk", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-K", ConsolidationID: "C-K",
		Kind: api.ExecutionTender, Facility: cfsShanghai, OccurredAt: at}))
	ok(t, 2)(f.module.StartLoading(ctx, api.StartLoading{CommandID: "start-xk", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-K", ExpectedRevision: 1, OccurredAt: at}))
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("tender-h01", "X-K", 2, "L-H01", 4, at)))
	ok(t, 4)(f.module.CloseLoadExecution(ctx, api.CloseLoadExecution{CommandID: "tender-receipt", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-K", ExpectedRevision: 3,
		Evidence: api.Evidence{Source: "CFS-WMS", Reference: "RCV-K-1", OccurredAt: at}, OccurredAt: at}))
	typesBefore := len(f.types(t))

	_, err := f.reconcileK("reconcile-early", 2, 6)
	f.refusedCommand(t, "reconcile with H04 attached and never tendered", err, api.ErrRejected, "")
	_, err = f.module.DetachHouse(ctx, f.detach("detach-h01", "C-K", "H01", 1, api.ReasonShutOut, "", f.hoursIn(6)))
	f.refusedCommand(t, "H01, tendered", err, api.ErrRejected, "packs")
	ok(t, 2)(f.module.DetachHouse(ctx, f.detach("detach-h04", "C-K", "H04", 1, api.ReasonSpaceShortage, "", f.hoursIn(6))))
	if e := f.membership(t, "H04").History[0]; e.End != api.EntryDetached || e.Reason != api.ReasonSpaceShortage || e.ReworkNo != 0 || e.ReworkExecutionID != "" || e.ConsolidationID != "C-K" {
		t.Fatalf("H04: %+v", e)
	}
	if got, want := f.types(t)[typesBefore:], []string{api.HouseDetached}; !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	ok(t, 3)(f.reconcileK("reconcile", 2, 7))
	if c := f.consolidation(t, "C-K"); c.State != api.StateReconciled || len(c.Members) != 1 {
		t.Fatalf("C-K: %+v", c)
	}
}

func (f *fixture) reconcileK(command string, expected int64, hour int) (api.Receipt, error) {
	return f.module.ReconcileConsolidation(context.Background(), api.ReconcileConsolidation{CommandID: command, TenantID: f.tenant, ActorID: "ops-1",
		ConsolidationID: "C-K", ExpectedRevision: expected, OccurredAt: f.hoursIn(hour)})
}

// TestAHouseUnloadedByTheReworkOfAnotherConsolidationLeavesWithNoLink: H03 was unloaded in a rework of C-S and moved to
// C-T, where it was loaded and taken out again by an ordinary reversal before the seal. The rework is C-S's: the
// entry in C-T records none, and the database refuses a raw end that names it.
func TestAHouseUnloadedByTheReworkOfAnotherConsolidationLeavesWithNoLink(t *testing.T) {
	ctx := context.Background()
	f := leaveDay(t)
	f.reseal(t, 8)
	f.targetOf(t)
	f.ful.sea["H03"] = true
	ok(t, 2)(f.module.TransferHouse(ctx, f.transfer("transfer-h03", "C-S", "C-T", "H03", 1, api.ReasonWrongConsol, "", f.hoursIn(9))))
	f.ful.allocate("H03", "C-T")
	closeT := f.closeCmd("close-t", "C-T", 2)
	closeT.OccurredAt = f.hoursIn(10)
	ok(t, 3)(f.module.CloseConsolidation(ctx, closeT))
	open := f.stuff("open-xt", "X-T", 1, f.hoursIn(11))
	open.ConsolidationID = "C-T"
	ok(t, 1)(f.module.OpenLoadExecution(ctx, open))
	ok(t, 2)(f.module.StartLoading(ctx, api.StartLoading{CommandID: "start-xt", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-T", ExpectedRevision: 1, OccurredAt: f.hoursIn(11)}))
	loaded := ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load-xt-h03", "X-T", 2, "L-H03", 2, f.hoursIn(12))))
	_, err := f.module.DetachHouse(ctx, f.detach("detach-loaded", "C-T", "H03", 2, api.ReasonShutOut, "", f.hoursIn(13)))
	f.refusedCommand(t, "H03 loaded in C-T", err, api.ErrRejected, "packs")
	ok(t, 4)(f.module.RecordCargoUnloaded(ctx, api.RecordCargoUnloaded{CommandID: "unload-xt", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-T", ExpectedRevision: 3,
		LineNo: loaded.ItemNo, Packs: 2, Reason: api.ReversalMisloaded, Evidence: evidence("unload-xt", f.hoursIn(13)), OccurredAt: f.hoursIn(13)}))
	f.refused(t, "a raw end naming the rework of C-S, which is not a rework of C-T", "23514", "membership_entry_rework_link",
		endEntry("H03", 2, "DETACHED", "X-1", 1, "attached_at + interval '20 hours'"))
	f.accepted(t, "a raw end with no rework", endEntry("H03", 2, "DETACHED", "", 0, "attached_at + interval '20 hours'"))
	ok(t, 3)(f.module.DetachHouse(ctx, f.detach("detach-h03", "C-T", "H03", 2, api.ReasonShutOut, "", f.hoursIn(14))))
	m := f.membership(t, "H03")
	if m.Live || len(m.History) != 2 || m.History[0].ReworkNo != 1 || m.History[0].ReworkExecutionID != "X-1" || m.History[1].End != api.EntryDetached ||
		m.History[1].ConsolidationID != "C-T" || m.History[1].ReworkNo != 0 || m.History[1].ReworkExecutionID != "" {
		t.Fatalf("the entry in C-S names X-1's rework, the entry in C-T none: %+v", m)
	}
}

// TestADetachOfAHouseWithNoLoadAndALoadOfTheSameHouseWaitForEachOther: H04's lot is received and its house is attached,
// never loaded; X-2, an open stuffing of the second unit of the same CLOSED consolidation, can take it. The house's
// ledger is held and the detach and the load queue behind it in each order. The detach first: H04 leaves and the load
// is refused, nothing of H04 in X-2. The load first: the detach is refused for the packs loaded, and H04 stays with its
// load. Never a house that left with a live load.
func TestADetachOfAHouseWithNoLoadAndALoadOfTheSameHouseWaitForEachOther(t *testing.T) {
	ctx := context.Background()
	for _, detachFirst := range []bool{true, false} {
		name := map[bool]string{true: "the detach first", false: "the load first"}[detachFirst]
		t.Run(name, func(t *testing.T) {
			f := shutOutDay(t, true)
			f.withoutRetry(t)
			f.begin(t, "X-2", 2)
			detach := func() error {
				_, err := f.module.DetachHouse(ctx, f.detach("detach-h04", "C-S", "H04", 1, api.ReasonShutOut, "", f.hoursIn(8)))
				return err
			}
			load := func() error {
				_, err := f.module.RecordCargoLoaded(ctx, f.load("b-h04", "X-2", 2, "L-H04", 2, f.hoursIn(8)))
				return err
			}
			pid, release := f.holdRow(t, lockLedger, f.tenant, "H04")
			var got []error
			if detachFirst {
				got = f.queue(t, pid, release, detach, load)
			} else {
				got = f.queue(t, pid, release, load, detach)
			}
			m, x2 := f.membership(t, "H04"), f.execution(t, "X-2")
			live := x2.TenderedPacks
			if detachFirst {
				f.refusedCommand(t, "the load after the detach", got[1], api.ErrRejected, "")
				if got[0] != nil || m.Live || live != 0 || m.History[0].ReworkNo != 0 || m.History[0].End != api.EntryDetached {
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

// TestAHouseWithNoReworkOfItsOwnIsNotRefusedForAnotherHousesReworkInAnotherExecution: line numbers repeat from one
// execution to the next. H01 is unloaded in a rework of X-1, a reversal of line 1 of X-1; H04 is loaded as line 1 of
// X-2 and taken out by an ordinary reversal. H04 has no rework of its own, and leaves with none: the database does not
// read the rework reversal of X-1's line 1 as a reversal of H04's line 1 of another execution (with that join dropped
// it would refuse the end, naming a link, for a house no rework unloaded).
func TestAHouseWithNoReworkOfItsOwnIsNotRefusedForAnotherHousesReworkInAnotherExecution(t *testing.T) {
	ctx := context.Background()
	f := shutOutDay(t, true)
	ok(t, 6)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-1", "X-1", 5, "H01 is rolled", "ops-asker", f.hoursIn(6))))
	ok(t, 7)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("unload-h01", "X-1", 6, "H01", f.hoursIn(7))))
	f.beginAt(t, "X-2", 2, f.hoursIn(8))
	loaded := ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("b-h04", "X-2", 2, "L-H04", 2, f.hoursIn(9))))
	if loaded.ItemNo != 1 {
		t.Fatalf("H04's line in X-2 is line %d, not 1", loaded.ItemNo)
	}
	if x := f.execution(t, "X-1"); len(x.Reversals) != 1 || x.Reversals[0].LineNo != 1 {
		t.Fatalf("the rework's reversal is of line 1 of X-1: %+v", x.Reversals)
	}
	ok(t, 4)(f.module.RecordCargoUnloaded(ctx, api.RecordCargoUnloaded{CommandID: "u-h04", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-2", ExpectedRevision: 3,
		LineNo: loaded.ItemNo, Packs: 2, Reason: api.ReversalMisloaded, Evidence: evidence("u-h04", f.hoursIn(10)), OccurredAt: f.hoursIn(10)}))
	f.accepted(t, "raw: H04 ends with no rework", endEntry("H04", 1, "DETACHED", "", 0, "attached_at + interval '30 hours'"))
	f.refused(t, "raw: H04 names the rework that unloaded H01", "23514", "membership_entry_rework_link", endEntry("H04", 1, "DETACHED", "X-1", 1, "attached_at + interval '30 hours'"))
	ok(t, 2)(f.module.DetachHouse(ctx, f.detach("d-h04", "C-S", "H04", 1, api.ReasonShutOut, "", f.hoursIn(11))))
	if e := f.membership(t, "H04").History[0]; e.End != api.EntryDetached || e.ReworkNo != 0 || e.ReworkExecutionID != "" {
		t.Fatalf("H04 left with no rework: %+v", e)
	}
}

// TestALeavingWithNoReworkIsNotDatedBeforeTheHousesLatestReversal: H04 is loaded into X-2 at hour 8 and taken out by an
// ordinary reversal at hour 9. It may not leave at hour 8, by a detach or a transfer; it may at hour 9, the time of the
// reversal itself, and a house that was never loaded has nothing to be dated after (the first test of this file
// detaches one at will).
func TestALeavingWithNoReworkIsNotDatedBeforeTheHousesLatestReversal(t *testing.T) {
	ctx := context.Background()
	f := shutOutDay(t, true)
	f.targetOf(t)
	f.ful.sea["H04"] = true
	f.beginAt(t, "X-2", 2, f.hoursIn(7))
	loaded := ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("b-h04", "X-2", 2, "L-H04", 2, f.hoursIn(8))))
	ok(t, 4)(f.module.RecordCargoUnloaded(ctx, api.RecordCargoUnloaded{CommandID: "u-h04", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-2", ExpectedRevision: 3,
		LineNo: loaded.ItemNo, Packs: 2, Reason: api.ReversalMisloaded, Evidence: evidence("u-h04", f.hoursIn(9)), OccurredAt: f.hoursIn(9)}))
	typesBefore := len(f.types(t))
	_, err := f.module.DetachHouse(ctx, f.detach("d-early", "C-S", "H04", 1, api.ReasonShutOut, "", f.hoursIn(8)))
	f.refusedCommand(t, "a detach dated before the reversal", err, api.ErrRejected, "invalid change")
	_, err = f.module.TransferHouse(ctx, f.transfer("t-early", "C-S", "C-T", "H04", 1, api.ReasonShutOut, "", f.hoursIn(8)))
	f.refusedCommand(t, "a transfer dated before the reversal", err, api.ErrRejected, "invalid change")
	_, err = f.module.DetachHouse(ctx, f.detach("d-just-before", "C-S", "H04", 1, api.ReasonShutOut, "", f.hoursIn(9).Add(-time.Microsecond)))
	f.refusedCommand(t, "a detach a microsecond before the reversal", err, api.ErrRejected, "invalid change")
	f.unchanged(t, "the refusals", "H04", 1, typesBefore)
	ok(t, 2)(f.module.DetachHouse(ctx, f.detach("d-equal", "C-S", "H04", 1, api.ReasonShutOut, "", f.hoursIn(9))))
	if e := f.membership(t, "H04").History[0]; !e.EndedAt.Equal(f.hoursIn(9)) || e.ReworkNo != 0 {
		t.Fatalf("H04 left at the time of its reversal: %+v", e)
	}

	// a transfer, at the time of the reversal
	g := shutOutDay(t, true)
	g.targetOf(t)
	g.ful.sea["H04"] = true
	g.beginAt(t, "X-2", 2, g.hoursIn(7))
	loaded = ok(t, 3)(g.module.RecordCargoLoaded(ctx, g.load("b-h04", "X-2", 2, "L-H04", 2, g.hoursIn(8))))
	ok(t, 4)(g.module.RecordCargoUnloaded(ctx, api.RecordCargoUnloaded{CommandID: "u-h04", TenantID: g.tenant, ActorID: "ops-1", ExecutionID: "X-2", ExpectedRevision: 3,
		LineNo: loaded.ItemNo, Packs: 2, Reason: api.ReversalMisloaded, Evidence: evidence("u-h04", g.hoursIn(9)), OccurredAt: g.hoursIn(9)}))
	ok(t, 2)(g.module.TransferHouse(ctx, g.transfer("t-equal", "C-S", "C-T", "H04", 1, api.ReasonShutOut, "", g.hoursIn(9))))
}

// TestAHouseTakenBackFromTheTenderBeforeItClosedLeavesTheCoLoadConsolidation: in the co-loader's consolidation C-K the
// packs of H04 were tendered and taken out again by an ordinary reversal before the tender was closed with the
// terminal's receipt; the consolidation is CLOSED, H04 leaves with a reason and C-K reconciles with H01.
func TestAHouseTakenBackFromTheTenderBeforeItClosedLeavesTheCoLoadConsolidation(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.createSea(t, "C-K", api.KindSeaLCLCoload)
	for _, h := range []string{"H01", "H04"} {
		f.ful.sea[h] = true
		f.ful.packs[h] = houseFigures[h][0]
		if _, err := f.module.AttachHouse(ctx, f.attach("attach-"+h, "C-K", h, 0)); err != nil {
			t.Fatal(err)
		}
		f.ful.allocate(h, "C-K")
	}
	ok(t, 2)(f.module.CloseConsolidation(ctx, f.closeCmd("close", "C-K", 1)))
	f.receiveLot(t, "H01")
	f.receiveLot(t, "H04")
	at := f.hoursIn(4)
	ok(t, 1)(f.module.OpenLoadExecution(ctx, api.OpenLoadExecution{CommandID: "open-xk", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-K", ConsolidationID: "C-K",
		Kind: api.ExecutionTender, Facility: cfsShanghai, OccurredAt: at}))
	ok(t, 2)(f.module.StartLoading(ctx, api.StartLoading{CommandID: "start-xk", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-K", ExpectedRevision: 1, OccurredAt: at}))
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("tender-h01", "X-K", 2, "L-H01", 4, at)))
	l4 := ok(t, 4)(f.module.RecordCargoLoaded(ctx, f.load("tender-h04", "X-K", 3, "L-H04", 2, at)))
	_, err := f.module.DetachHouse(ctx, f.detach("d-tendered", "C-K", "H04", 1, api.ReasonShutOut, "", f.hoursIn(6)))
	f.refusedCommand(t, "H04 with its packs in the tender", err, api.ErrRejected, "packs")
	ok(t, 5)(f.module.RecordCargoUnloaded(ctx, api.RecordCargoUnloaded{CommandID: "back", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-K", ExpectedRevision: 4,
		LineNo: l4.ItemNo, Packs: 2, Reason: api.ReversalMisloaded, Evidence: evidence("back", f.hoursIn(5)), OccurredAt: f.hoursIn(5)}))
	ok(t, 6)(f.module.CloseLoadExecution(ctx, api.CloseLoadExecution{CommandID: "tender-receipt", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-K", ExpectedRevision: 5,
		Evidence: api.Evidence{Source: "CFS-WMS", Reference: "RCV-K-1", OccurredAt: f.hoursIn(5)}, OccurredAt: f.hoursIn(5)}))
	if x := f.execution(t, "X-K"); x.State != api.ExecutionClosed || len(x.Discrepancies) != 0 || x.TenderedPacks != 4 {
		t.Fatalf("X-K: closed with H01's 4 packs and no discrepancy: %+v", x)
	}
	_, err = f.module.DetachHouse(ctx, f.detach("d-early", "C-K", "H04", 1, api.ReasonShutOut, "", f.hoursIn(4)))
	f.refusedCommand(t, "dated before the reversal", err, api.ErrRejected, "invalid change")
	ok(t, 2)(f.module.DetachHouse(ctx, f.detach("d-h04", "C-K", "H04", 1, api.ReasonShutOut, "", f.hoursIn(6))))
	if e := f.membership(t, "H04").History[0]; e.End != api.EntryDetached || e.ReworkNo != 0 {
		t.Fatalf("H04: %+v", e)
	}
	ok(t, 3)(f.reconcileK("rec", 2, 7))
	if c := f.consolidation(t, "C-K"); c.State != api.StateReconciled || len(c.Members) != 1 {
		t.Fatalf("C-K: %+v", c)
	}
}

// TestAHouseOffloadedByTheCarrierLeavesTheCoLoadConsolidationOnceItsDiscrepancyIsResolved: carrier evidence that H04
// was offloaded raises a loading discrepancy on the closed tender and the forwarder takes the packs back by a reversal.
// The discrepancy changes nothing for the leaving (H04 has no live packs; DetachHouse succeeds with no rework), but the
// consolidation does not reconcile while it is open: it does once the discrepancy is resolved.
func TestAHouseOffloadedByTheCarrierLeavesTheCoLoadConsolidationOnceItsDiscrepancyIsResolved(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.createSea(t, "C-K", api.KindSeaLCLCoload)
	for _, h := range []string{"H01", "H04"} {
		f.ful.sea[h] = true
		f.ful.packs[h] = houseFigures[h][0]
		if _, err := f.module.AttachHouse(ctx, f.attach("attach-"+h, "C-K", h, 0)); err != nil {
			t.Fatal(err)
		}
		f.ful.allocate(h, "C-K")
	}
	ok(t, 2)(f.module.CloseConsolidation(ctx, f.closeCmd("close", "C-K", 1)))
	f.receiveLot(t, "H01")
	f.receiveLot(t, "H04")
	at := f.hoursIn(4)
	ok(t, 1)(f.module.OpenLoadExecution(ctx, api.OpenLoadExecution{CommandID: "open-xk", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-K", ConsolidationID: "C-K",
		Kind: api.ExecutionTender, Facility: cfsShanghai, OccurredAt: at}))
	ok(t, 2)(f.module.StartLoading(ctx, api.StartLoading{CommandID: "start-xk", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-K", ExpectedRevision: 1, OccurredAt: at}))
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("tender-h01", "X-K", 2, "L-H01", 4, at)))
	l4 := ok(t, 4)(f.module.RecordCargoLoaded(ctx, f.load("tender-h04", "X-K", 3, "L-H04", 2, at)))
	ok(t, 5)(f.module.CloseLoadExecution(ctx, api.CloseLoadExecution{CommandID: "tender-receipt", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-K", ExpectedRevision: 4,
		Evidence: api.Evidence{Source: "CFS-WMS", Reference: "RCV-K-1", OccurredAt: at}, OccurredAt: at}))
	ok(t, 6)(f.module.RecordCarrierLoadEvidence(ctx, api.RecordCarrierLoadEvidence{CommandID: "carrier", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-K",
		ExpectedRevision: 5, Fact: api.CarrierOffloaded, ShipmentID: "H04", Pieces: 2, Transport: "MV-X", Evidence: evidence("carrier", f.hoursIn(5)), OccurredAt: f.hoursIn(5)}))
	ok(t, 7)(f.module.RecordCargoUnloaded(ctx, api.RecordCargoUnloaded{CommandID: "back", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-K", ExpectedRevision: 6,
		LineNo: l4.ItemNo, Packs: 2, Reason: api.ReversalOffloadedByForwarder, Evidence: evidence("back", f.hoursIn(5)), OccurredAt: f.hoursIn(5)}))
	if x := f.execution(t, "X-K"); len(x.Discrepancies) != 1 || x.Discrepancies[0].Kind != api.LoadOffloaded || x.Discrepancies[0].ShipmentID != "H04" || x.Discrepancies[0].Resolution != "" {
		t.Fatalf("the offload is an open discrepancy of H04: %+v", x.Discrepancies)
	}
	ok(t, 2)(f.module.DetachHouse(ctx, f.detach("d-h04", "C-K", "H04", 1, api.ReasonShutOut, "", f.hoursIn(6))))
	_, err := f.reconcileK("rec-early", 2, 7)
	f.refusedCommand(t, "reconcile with the discrepancy open", err, api.ErrRejected, "1 discrepancies open")
	ok(t, 8)(f.module.ResolveLoadingDiscrepancy(ctx, api.ResolveLoadingDiscrepancy{CommandID: "accept-offload", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-K",
		ExpectedRevision: 7, DiscrepancyNo: 1, Resolution: api.ResolvedByAccepted, Reason: "H04 shut out at the terminal and left C-K", OccurredAt: f.hoursIn(7)}))
	ok(t, 3)(f.reconcileK("rec", 2, 8))
	if c := f.consolidation(t, "C-K"); c.State != api.StateReconciled || len(c.Members) != 1 {
		t.Fatalf("C-K: %+v", c)
	}
}

// TestALeavingWithNoReworkIsNotDatedBeforeTheLatestReversalInASecondExecution: H03 is loaded into X-1 at hour 4 and
// taken out by an ordinary reversal at hour 5, loaded into X-2 at hour 6 and taken out there at hour 8. Its latest
// reversal in the consolidation is the one in the second execution: a leaving at hour 7, after the first reversal and
// before the second, is refused, one a microsecond before hour 8 too, and one at hour 8 is accepted.
func TestALeavingWithNoReworkIsNotDatedBeforeTheLatestReversalInASecondExecution(t *testing.T) {
	ctx := context.Background()
	f := reworkDay(t)
	f.begin(t, "X-1", 1)
	f.begin(t, "X-2", 2)
	first := ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("a-h03", "X-1", 2, "L-H03", 2, f.hoursIn(4))))
	ok(t, 4)(f.module.RecordCargoUnloaded(ctx, api.RecordCargoUnloaded{CommandID: "ua-h03", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-1", ExpectedRevision: 3,
		LineNo: first.ItemNo, Packs: 2, Reason: api.ReversalMisloaded, Evidence: evidence("ua-h03", f.hoursIn(5)), OccurredAt: f.hoursIn(5)}))
	second := ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("b-h03", "X-2", 2, "L-H03", 2, f.hoursIn(6))))
	ok(t, 4)(f.module.RecordCargoUnloaded(ctx, api.RecordCargoUnloaded{CommandID: "ub-h03", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-2", ExpectedRevision: 3,
		LineNo: second.ItemNo, Packs: 2, Reason: api.ReversalMisloaded, Evidence: evidence("ub-h03", f.hoursIn(8)), OccurredAt: f.hoursIn(8)}))
	if first.ItemNo != 1 || second.ItemNo != 1 {
		t.Fatalf("each of H03's lines is line 1 of its execution: %d, %d", first.ItemNo, second.ItemNo)
	}
	typesBefore := len(f.types(t))
	for name, at := range map[string]time.Time{"between the two reversals": f.hoursIn(7), "a microsecond before the later one": f.hoursIn(8).Add(-time.Microsecond),
		"at the first reversal": f.hoursIn(5)} {
		_, err := f.module.DetachHouse(ctx, f.detach("d-"+name, "C-S", "H03", 1, api.ReasonShutOut, "", at))
		f.refusedCommand(t, "a detach "+name, err, api.ErrRejected, "invalid change")
	}
	f.unchanged(t, "the refusals", "H03", 1, typesBefore)
	ok(t, 2)(f.module.DetachHouse(ctx, f.detach("d-equal", "C-S", "H03", 1, api.ReasonShutOut, "", f.hoursIn(8))))
	if e := f.membership(t, "H03").History[0]; !e.EndedAt.Equal(f.hoursIn(8)) || e.ReworkNo != 0 || e.ReworkExecutionID != "" {
		t.Fatalf("H03 left at the time of its latest reversal, with no rework: %+v", e)
	}
}
