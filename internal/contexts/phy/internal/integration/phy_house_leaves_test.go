//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/api"
)

// A house unloaded whole in the rework of a sealed container leaves the CLOSED consolidation (V2.86), with the real
// commands and the real database. The day is the owner's, as in phy_rework_test.go: H01, H02 and H03 of 10, 15 and 5
// W/M in one container, H03 is unloaded in a rework.

// leaveDay is the rework day up to the unloading: X-1 sealed with H01, H02 and H03 (SEAL-1), opened for rework by
// ops-asker and H03 unloaded, so that X-1 is in REWORK at revision 8, C-S is CLOSED at revision 4, H03's ledger is at
// revision 1 and its lot L-H03 is RECEIVED again.
func leaveDay(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	f := reworkDay(t)
	f.stuffedAndSealed(t)
	ok(t, 7)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-1", "X-1", 6, "the consignee of H03 withdrew its cargo", "ops-asker", f.hoursIn(6))))
	ok(t, 8)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("unload-h03", "X-1", 7, "H03", f.hoursIn(7))))
	return f
}

// reseal seals X-1 again with SEAL-2 (X-1 at the revision given).
func (f *fixture) reseal(t *testing.T, revision int64) {
	t.Helper()
	ok(t, revision+1)(f.module.CloseLoadExecution(context.Background(), api.CloseLoadExecution{CommandID: "reseal-1", TenantID: f.tenant, ActorID: "ops-sealer",
		ExecutionID: "X-1", ExpectedRevision: revision, Evidence: api.Evidence{Source: api.EvidenceSeal, Reference: "SEAL-2", OccurredAt: f.hoursIn(8)}, OccurredAt: f.hoursIn(8)}))
}

func (f *fixture) detach(command, consolidation, house string, expected int64, reason, text string, at time.Time) api.DetachHouse {
	return api.DetachHouse{CommandID: command, TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: consolidation, ShipmentID: house, ExpectedMembershipRevision: expected,
		Reason: reason, ReasonText: text, OccurredAt: at}
}

func (f *fixture) transfer(command, from, to, house string, expected int64, reason, text string, at time.Time) api.TransferHouse {
	return api.TransferHouse{CommandID: command, TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: from, ToConsolidationID: to, ShipmentID: house,
		ExpectedMembershipRevision: expected, Reason: reason, ReasonText: text, OccurredAt: at}
}

func (f *fixture) membership(t *testing.T, house string) api.ConsolMembership {
	t.Helper()
	m, err := f.module.GetConsolMembership(context.Background(), api.GetConsolMembership{TenantID: f.tenant, ShipmentID: house})
	if err != nil {
		t.Fatalf("membership of %s: %v", house, err)
	}
	return m
}

// payloads returns the payloads of the events of one type the tenant's PHY wrote, in order.
func (f *fixture) payloads(t *testing.T, eventType string) []string {
	t.Helper()
	q, err := f.db.ReadExecutor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rows, err := q.Query(context.Background(), `SELECT payload::text FROM bento.outbox WHERE scope=$1 AND source=$2 AND event_type=$3 ORDER BY enqueued_seq`, f.tenant, api.Source, eventType)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err = rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// unchanged checks that a refused command wrote nothing: the ledger's revision and the events are as they were.
func (f *fixture) unchanged(t *testing.T, what, house string, revision int64, events int) {
	t.Helper()
	if got := f.membership(t, house).Revision; got != revision {
		t.Errorf("%s: the ledger of %s moved to revision %d, want %d", what, house, got, revision)
	}
	if got := len(f.outboxTypes(t)); got != events {
		t.Errorf("%s: %d events, want %d", what, got, events)
	}
}

// TestAHouseUnloadedInAReworkIsDetachedFromTheClosedConsolidation: H03 is unloaded while X-1 is still in REWORK and
// leaves C-S with a reason; the container is sealed again and the consolidation reconciles with the two houses left.
func TestAHouseUnloadedInAReworkIsDetachedFromTheClosedConsolidation(t *testing.T) {
	ctx := context.Background()
	f := reworkDay(t)
	f.stuffedAndSealed(t)
	typesBefore := len(f.types(t))

	// frozen as before while H03 is loaded: sealed, and in rework with its packs still in the container
	_, err := f.module.DetachHouse(ctx, f.detach("detach-sealed", "C-S", "H03", 1, api.ReasonSpaceShortage, "", f.hoursIn(6)))
	f.refusedCommand(t, "H03 sealed in the container", err, api.ErrRejected, "packs")
	ok(t, 7)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-1", "X-1", 6, "the consignee of H03 withdrew its cargo", "ops-asker", f.hoursIn(6))))
	_, err = f.module.DetachHouse(ctx, f.detach("detach-loaded", "C-S", "H03", 1, api.ReasonSpaceShortage, "", f.hoursIn(6)))
	f.refusedCommand(t, "H03 loaded in the container in rework", err, api.ErrRejected, "packs")
	ok(t, 8)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("unload-h03", "X-1", 7, "H03", f.hoursIn(7))))
	events := len(f.outboxTypes(t))

	// every other house stays frozen
	_, err = f.module.DetachHouse(ctx, f.detach("detach-h02", "C-S", "H02", 1, api.ReasonSpaceShortage, "", f.hoursIn(8)))
	f.refusedCommand(t, "H02, loaded", err, api.ErrRejected, "packs")
	_, err = f.module.TransferHouse(ctx, f.transfer("transfer-h01", "C-S", "C-S", "H01", 1, api.ReasonRollover, "", f.hoursIn(8)))
	f.refusedCommand(t, "H01, loaded", err, api.ErrRejected, "packs")

	// the reasons: PHY's own, OTHER with no text, one outside the list, none, a text too long
	for name, tc := range map[string]struct{ reason, text string }{
		"SHIPMENT_CANCELLED is PHY's own": {api.ReasonShipmentCancelled, ""},
		"OTHER without a text":            {api.ReasonOther, ""},
		"NO_SUCH_REASON":                  {"NO_SUCH_REASON", ""},
		"no reason":                       {"", ""},
		"a text of 501 characters":        {api.ReasonOther, strings.Repeat("x", 501)},
	} {
		_, err = f.module.DetachHouse(ctx, f.detach("detach-"+name, "C-S", "H03", 1, tc.reason, tc.text, f.hoursIn(8)))
		f.refusedCommand(t, name, err, api.ErrRejected, "reason")
	}
	// not before it was unloaded (at hour 7), and not by nobody
	_, err = f.module.DetachHouse(ctx, f.detach("detach-early", "C-S", "H03", 1, api.ReasonSpaceShortage, "", f.hoursIn(6)))
	f.refusedCommand(t, "before the unloading", err, api.ErrRejected, "invalid change")
	f.unchanged(t, "the refusals", "H03", 1, events)
	if m := f.membership(t, "H03"); !m.Live || m.Entry.End != "" || m.Entry.ReworkNo != 0 {
		t.Fatalf("H03 is still attached: %+v", m)
	}

	// the operator's reason: H03 leaves, the container still in REWORK
	cmd := f.detach("detach-h03", "C-S", "H03", 1, api.ReasonSpaceShortage, "", f.hoursIn(8))
	r := ok(t, 2)(f.module.DetachHouse(ctx, cmd))
	if r.ConsolidationID != "C-S" || r.ShipmentID != "H03" || r.EntryNo != 1 {
		t.Fatalf("receipt = %+v", r)
	}
	if again, err := f.module.DetachHouse(ctx, cmd); err != nil || again != r {
		t.Fatalf("replay = %+v, %v; want %+v", again, err, r)
	}
	if bad := cmd; true {
		bad.Reason = api.ReasonRollover
		if _, err = f.module.DetachHouse(ctx, bad); !errors.Is(err, api.ErrIdempotencyConflict) {
			t.Fatalf("a command id reused with another reason: %v", err)
		}
	}
	m := f.membership(t, "H03")
	e := m.Entry
	if m.Live || len(m.History) != 1 || m.Revision != 2 {
		t.Fatalf("H03 is attached nowhere: %+v", m)
	}
	if e = m.History[0]; e.End != api.EntryDetached || e.Reason != api.ReasonSpaceShortage || e.ConsolidationID != "C-S" || !e.EndedAt.Equal(f.hoursIn(8)) ||
		e.ReworkExecutionID != "X-1" || e.ReworkNo != 1 || e.SuccessorNo != 0 || e.Packs != 2 {
		t.Fatalf("the entry that ended names the rework that unloaded the house: %+v", e)
	}
	if c := f.consolidation(t, "C-S"); c.State != api.StateClosed || len(c.Members) != 2 || c.Revision != 4 {
		t.Fatalf("C-S: %+v", c)
	}
	basis := f.basis(t)
	if len(basis.Houses) != 2 || basis.Houses[0].ShipmentID != "H01" || basis.Houses[1].ShipmentID != "H02" {
		t.Fatalf("the cost basis lists the houses that are attached: %+v", basis.Houses)
	}
	packing(t, "H03 left, X-1 in rework", basis.PackingLists[0], "X-1", api.ExecutionRework, "H01", "H02")
	if l := f.lot(t, "L-H03"); l.State != "RECEIVED" || l.ReceivedPacks != 2 || l.LoadedPacks != 0 {
		t.Fatalf("the lot of H03, open again, is attached nowhere: %+v", l)
	}
	if got, want := f.types(t)[typesBefore:], []string{api.LoadExecutionReworkOpened, api.CargoLotReopened, api.CargoUnloaded, api.HouseDetached}; !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if p := f.payloads(t, api.HouseDetached); len(p) != 1 || !strings.Contains(p[0], `"reason": "SPACE_SHORTAGE"`) && !strings.Contains(p[0], `"reason":"SPACE_SHORTAGE"`) ||
		!strings.Contains(p[0], "H03") || !strings.Contains(p[0], "C-S") || strings.Contains(p[0], "X-1") {
		t.Fatalf("the event carries the reason as before, and names no rework: %v", p)
	}
	// H03 cannot be loaded, nor detached again
	_, err = f.module.DetachHouse(ctx, f.detach("detach-again", "C-S", "H03", 2, api.ReasonSpaceShortage, "", f.hoursIn(9)))
	f.refusedCommand(t, "a second detach", err, api.ErrRejected, "")

	// the consolidation cannot reconcile while the container is in rework; sealed again, it does, with the two houses
	// loaded as planned
	_, err = f.module.ReconcileConsolidation(ctx, api.ReconcileConsolidation{CommandID: "reconcile-early", TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: "C-S",
		ExpectedRevision: 4, OccurredAt: f.hoursIn(9)})
	f.refusedCommand(t, "reconcile with the container in rework", err, api.ErrRejected, "")
	f.reseal(t, 8)
	reconciled := ok(t, 5)(f.module.ReconcileConsolidation(ctx, api.ReconcileConsolidation{CommandID: "reconcile", TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: "C-S",
		ExpectedRevision: 4, OccurredAt: f.hoursIn(9)}))
	if reconciled.ConsolidationID != "C-S" || f.consolidation(t, "C-S").State != api.StateReconciled {
		t.Fatalf("C-S reconciled: %+v", reconciled)
	}
	// RECONCILED: every house stays, and the history of H03 does not change
	_, err = f.module.DetachHouse(ctx, f.detach("detach-reconciled", "C-S", "H01", 1, api.ReasonSpaceShortage, "", f.hoursIn(10)))
	f.refusedCommand(t, "H01 of a reconciled consolidation", err, api.ErrRejected, "")
	f.refused(t, "a raw end of H01 in the RECONCILED consolidation", "23514", "membership_entry_frozen",
		`UPDATE cargo_phy.membership_entry SET end_kind='DETACHED', end_reason='ROLLOVER', ended_by='raw', ended_at=attached_at + interval '20 hours', end_entered_at=now(),
          end_rework_execution_id='X-1', end_rework_no=1 WHERE tenant_id=$1 AND shipment_id='H01' AND entry_no=1`)
	if again := f.membership(t, "H03"); again.Revision != 2 || again.History[0].ReworkNo != 1 {
		t.Fatalf("H03: %+v", again)
	}
}

// targetOf plans the container of an OPEN own-container consolidation C-T (at revision 2 with one 22G1) for a house to
// be moved to.
func (f *fixture) targetOf(t *testing.T) {
	t.Helper()
	f.createSea(t, "C-T", api.KindSeaLCLOwn)
	ok(t, 2)(f.module.PlanLoadUnit(context.Background(), f.planUnit("plan-t1", "C-T", 1, "22G1")))
}

// TestAHouseUnloadedInAReworkIsTransferredStuffedAgainAndSealed: X-1 is sealed again, H03 moves to the OPEN
// consolidation C-T, which is closed, stuffed with the lot that was opened again, and sealed; C-S reconciles with H01
// and H02.
func TestAHouseUnloadedInAReworkIsTransferredStuffedAgainAndSealed(t *testing.T) {
	ctx := context.Background()
	f := leaveDay(t)
	f.reseal(t, 8)
	f.targetOf(t)
	typesBefore := len(f.types(t))
	f.ful.sea["H03"] = true

	// the target must be OPEN and of the mode, and the reason is the operator's
	_, err := f.module.TransferHouse(ctx, f.transfer("transfer-same", "C-S", "C-S", "H03", 1, api.ReasonRollover, "", f.hoursIn(9)))
	f.refusedCommand(t, "to the same consolidation", err, api.ErrRejected, "")
	_, err = f.module.TransferHouse(ctx, f.transfer("transfer-no-text", "C-S", "C-T", "H03", 1, api.ReasonOther, "", f.hoursIn(9)))
	f.refusedCommand(t, "OTHER without a text", err, api.ErrRejected, "")
	_, err = f.module.TransferHouse(ctx, f.transfer("transfer-own", "C-S", "C-T", "H03", 1, api.ReasonShipmentCancelled, "", f.hoursIn(9)))
	f.refusedCommand(t, "SHIPMENT_CANCELLED is PHY's own", err, api.ErrRejected, "")
	_, err = f.module.TransferHouse(ctx, f.transfer("transfer-h01", "C-S", "C-T", "H01", 1, api.ReasonRollover, "", f.hoursIn(9)))
	f.refusedCommand(t, "H01, loaded in the sealed container", err, api.ErrRejected, "")
	f.unchanged(t, "the refusals", "H03", 1, typesBefore)

	cmd := f.transfer("transfer-h03", "C-S", "C-T", "H03", 1, api.ReasonWrongConsol, "", f.hoursIn(9))
	r := ok(t, 2)(f.module.TransferHouse(ctx, cmd))
	if r.ConsolidationID != "C-T" || r.ShipmentID != "H03" || r.EntryNo != 2 {
		t.Fatalf("receipt = %+v", r)
	}
	if again, err := f.module.TransferHouse(ctx, cmd); err != nil || again != r {
		t.Fatalf("replay = %+v, %v; want %+v", again, err, r)
	}
	m := f.membership(t, "H03")
	if !m.Live || m.Entry.ConsolidationID != "C-T" || m.Entry.EntryNo != 2 || m.Entry.Start != api.EntryTransferred || m.Entry.PredecessorNo != 1 || m.Entry.ReworkNo != 0 ||
		m.Entry.ReworkExecutionID != "" || len(m.History) != 2 {
		t.Fatalf("H03 is in C-T: %+v", m)
	}
	if e := m.History[0]; e.End != api.EntryTransferred || e.Reason != api.ReasonWrongConsol || e.ConsolidationID != "C-S" || e.SuccessorNo != 2 || e.ReworkExecutionID != "X-1" ||
		e.ReworkNo != 1 || !e.EndedAt.Equal(f.hoursIn(9)) {
		t.Fatalf("the entry in C-S: %+v", e)
	}
	if got, want := f.types(t)[typesBefore:], []string{api.HouseTransferred}; !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if p := f.payloads(t, api.HouseTransferred); len(p) != 1 || !strings.Contains(p[0], "WRONG_CONSOL") || !strings.Contains(p[0], "C-T") || !strings.Contains(p[0], "C-S") || strings.Contains(p[0], "X-1") {
		t.Fatalf("HouseTransferred.v1 carries the reason and both consolidations, no rework: %v", p)
	}
	if c := f.consolidation(t, "C-S"); len(c.Members) != 2 || c.State != api.StateClosed {
		t.Fatalf("C-S: %+v", c)
	}
	if c := f.consolidation(t, "C-T"); len(c.Members) != 1 || c.State != api.StateOpen {
		t.Fatalf("C-T: %+v", c)
	}

	f.refused(t, "a rework's execution without its number", "23514", "membership_entry_rework",
		`UPDATE cargo_phy.membership_entry SET end_kind='DETACHED', end_reason='ROLLOVER', ended_by='raw', ended_at=attached_at + interval '1 hour', end_entered_at=now(),
          end_rework_execution_id='X-1' WHERE tenant_id=$1 AND shipment_id='H03' AND entry_no=2`)
	// in the target's own freeze: an end of its entry that names the rework is refused, open and closed
	f.refused(t, "an entry that ends in the OPEN consolidation names no rework", "23514", "membership_entry_rework_link",
		`UPDATE cargo_phy.membership_entry SET end_kind='DETACHED', end_reason='ROLLOVER', ended_by='raw', ended_at=attached_at + interval '1 hour', end_entered_at=now(),
          end_rework_execution_id='X-1', end_rework_no=1 WHERE tenant_id=$1 AND shipment_id='H03' AND entry_no=2`)
	f.ful.allocate("H03", "C-T")
	closeT := f.closeCmd("close-t", "C-T", 2)
	closeT.OccurredAt = f.hoursIn(10)
	ok(t, 3)(f.module.CloseConsolidation(ctx, closeT))
	f.refused(t, "the rework is a rework of the consolidation the entry is in, not another", "23514", "membership_entry_rework_link",
		`UPDATE cargo_phy.membership_entry SET end_kind='DETACHED', end_reason='ROLLOVER', ended_by='raw', ended_at=attached_at + interval '10 hours', end_entered_at=now(),
          end_rework_execution_id='X-1', end_rework_no=1 WHERE tenant_id=$1 AND shipment_id='H03' AND entry_no=2`)
	// H03 was never loaded in C-T, which is CLOSED and of a sea kind: it may leave it, with no rework (V2.87; V2.86
	// refused it). Run as a raw write here, rolled back, since the test goes on to stuff it there.
	f.accepted(t, "H03, never loaded in C-T, ends there with no rework", endEntry("H03", 2, "DETACHED", "", 0, "attached_at + interval '10 hours'"))

	// stuffed in C-T from the lot that was opened again, and sealed
	open := f.stuff("open-xt", "X-T", 1, f.hoursIn(11))
	open.ConsolidationID = "C-T"
	ok(t, 1)(f.module.OpenLoadExecution(ctx, open))
	ok(t, 2)(f.module.StartLoading(ctx, api.StartLoading{CommandID: "start-xt", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-T", ExpectedRevision: 1, OccurredAt: f.hoursIn(11)}))
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load-xt-h03", "X-T", 2, "L-H03", 2, f.hoursIn(12))))
	seal := f.sealOf("X-T", 3, "SEAL-T")
	seal.OccurredAt, seal.Evidence.OccurredAt = f.hoursIn(13), f.hoursIn(13)
	ok(t, 4)(f.module.CloseLoadExecution(ctx, seal))
	if x := f.execution(t, "X-T"); x.State != api.ExecutionClosed || x.TenderedPacks != 2 || x.CloseEvidence.Reference != "SEAL-T" || len(x.Reworks) != 0 {
		t.Fatalf("X-T: %+v", x)
	}
	tb := f.basisOf(t, "C-T")
	if len(tb.Houses) != 1 || tb.Houses[0].ShipmentID != "H03" || tb.Houses[0].LoadedPacks != 2 || wm(tb.Houses[0].LoadedGrossWeight, tb.Houses[0].LoadedVolume) != 5 {
		t.Fatalf("C-T's cost basis: H03 at 5 W/M: %+v", tb.Houses)
	}
	packing(t, "C-T", tb.PackingLists[0], "X-T", api.ExecutionClosed, "H03")
	// with live packs in C-T and no rework of C-T the house stays frozen, by the command (V2.86 refused it before the
	// stuffing too; since V2.87 a house of C-T with nothing loaded there may leave)
	_, err = f.module.DetachHouse(ctx, f.detach("detach-from-c-t", "C-T", "H03", 2, api.ReasonRollover, "", f.hoursIn(14)))
	f.refusedCommand(t, "H03, loaded in C-T, which no rework of C-T unloaded", err, api.ErrRejected, "packs")

	// C-S has two houses left, each loaded as planned, X-1 sealed: it reconciles
	ok(t, 5)(f.module.ReconcileConsolidation(ctx, api.ReconcileConsolidation{CommandID: "reconcile", TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: "C-S",
		ExpectedRevision: 4, OccurredAt: f.hoursIn(14)}))
	if c := f.consolidation(t, "C-S"); c.State != api.StateReconciled || len(c.Members) != 2 {
		t.Fatalf("C-S: %+v", c)
	}
	ok(t, 4)(f.module.ReconcileConsolidation(ctx, api.ReconcileConsolidation{CommandID: "reconcile-t", TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: "C-T",
		ExpectedRevision: 3, OccurredAt: f.hoursIn(14)}))
}

func (f *fixture) basisOf(t *testing.T, consolidation string) api.PhysicalCostBasis {
	t.Helper()
	b, err := f.module.GetPhysicalCostBasis(context.Background(), api.GetPhysicalCostBasis{TenantID: f.tenant, ConsolidationID: consolidation})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// accepted runs the statements in a transaction of the application role, makes the deferred rules check, and rolls it
// back: it reports what the database accepts.
func (f *fixture) accepted(t *testing.T, what string, statements ...string) {
	t.Helper()
	errRollback := errors.New("rolled back")
	err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		for _, s := range statements {
			if _, err := e.Exec(ctx, s, f.tenant); err != nil {
				return err
			}
		}
		if _, err := e.Exec(ctx, `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
			return err
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Errorf("%s: the database refused: %v", what, err)
	}
}

// endEntry ends the entry of the house as the end kind, naming the rework (an empty execution names none), at the
// time of the expression.
func endEntry(house string, entry int, kind, execution string, no int, at string) string {
	link := "NULL, NULL"
	if execution != "" {
		link = fmt.Sprintf("'%s', %d", execution, no)
	}
	succ := "NULL"
	if kind == "TRANSFERRED" {
		succ = fmt.Sprint(entry + 1)
	}
	return fmt.Sprintf(`UPDATE cargo_phy.membership_entry SET end_kind='%s', end_reason='ROLLOVER', ended_by='raw', ended_at=%s, end_entered_at=now(), successor_no=%s,
        end_rework_execution_id=%s WHERE tenant_id=$1 AND shipment_id='%s' AND entry_no=%d`, kind, at, succ, strings.Replace(link, ", ", ", end_rework_no=", 1), house, entry)
}

// TestAHouseNeverLoadedLeavesAClosedConsolidationAndOnlyTheUnloadedOneNamesARework (V2.86 called it
// TestOnlyAHouseUnloadedInAReworkLeavesAClosedConsolidation): a house that was never loaded leaves a CLOSED sea
// consolidation with no rework recorded though a rework of the consolidation unloaded another (the database says the
// same to raw writes), the one that was unloaded leaves naming its rework, and the one still loaded stays.
func TestAHouseNeverLoadedLeavesAClosedConsolidationAndOnlyTheUnloadedOneNamesARework(t *testing.T) {
	ctx := context.Background()
	f := reworkDay(t)
	f.begin(t, "X-1", 1)
	at := f.hoursIn(4)
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load-H01", "X-1", 2, "L-H01", 4, at)))
	ok(t, 4)(f.module.RecordCargoLoaded(ctx, f.load("load-H02", "X-1", 3, "L-H02", 6, at)))
	ok(t, 5)(f.module.CloseLoadExecution(ctx, f.sealOf("X-1", 4, "SEAL-1")))
	// H03 is attached and was never loaded: a shut-out before stuffing, and the consolidation is CLOSED. V2.86 refused
	// its leaving; since V2.87 the database accepts the end with no rework (the command, below, is run at the end).
	_, err := f.module.TransferHouse(ctx, f.transfer("transfer-h03", "C-S", "C-S", "H03", 1, api.ReasonShutOut, "", f.hoursIn(6)))
	f.refusedCommand(t, "a house never loaded, transferred to the consolidation it is in", err, api.ErrRejected, "")
	f.accepted(t, "a raw end of a house never loaded, with no rework", endEntry("H03", 1, "DETACHED", "", 0, "attached_at + interval '10 hours'"))
	f.refused(t, "a raw end of a house never loaded, naming a rework that does not exist", "23514", "membership_entry_rework_link",
		endEntry("H03", 1, "DETACHED", "X-1", 1, "attached_at + interval '10 hours'"))

	// a rework unloads H01: H01 leaves; H03, never loaded, stays though the consolidation has a rework now; H02 stays
	ok(t, 6)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-1", "X-1", 5, "H01 is rolled", "ops-asker", f.hoursIn(6))))
	ok(t, 7)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("unload-h01", "X-1", 6, "H01", f.hoursIn(7))))
	_, err = f.module.DetachHouse(ctx, f.detach("detach-h02", "C-S", "H02", 1, api.ReasonShutOut, "", f.hoursIn(8)))
	f.refusedCommand(t, "a house still loaded", err, api.ErrRejected, "")
	f.refused(t, "a raw end of a house never loaded, naming the rework that unloaded another", "23514", "membership_entry_rework_link",
		endEntry("H03", 1, "DETACHED", "X-1", 1, "attached_at + interval '10 hours'"))
	f.refused(t, "a raw end of a house still loaded, naming that rework", "23514", "membership_entry_rework_link", endEntry("H02", 1, "DETACHED", "X-1", 1, "attached_at + interval '10 hours'"))
	f.refused(t, "a link on an end that is no detach", "23514", "membership_entry_rework",
		`UPDATE cargo_phy.membership_entry SET end_kind='SHIPMENT_CANCELLED', end_reason='SHIPMENT_CANCELLED', ended_by='raw', ended_at=attached_at + interval '10 hours',
          end_entered_at=now(), end_rework_execution_id='X-1', end_rework_no=1 WHERE tenant_id=$1 AND shipment_id='H01' AND entry_no=1`)
	f.accepted(t, "the end the command writes, as a raw write", endEntry("H01", 1, "DETACHED", "X-1", 1, "attached_at + interval '10 hours'"))
	// the database does not say when the house must be dated: before it was unloaded is accepted; the domain refuses it
	f.accepted(t, "an end dated before the unloading", endEntry("H01", 1, "DETACHED", "X-1", 1, "attached_at + interval '1 minute'"))
	r := ok(t, 2)(f.module.DetachHouse(ctx, f.detach("detach-h01", "C-S", "H01", 1, api.ReasonRollover, "", f.hoursIn(8))))
	if m := f.membership(t, "H01"); r.EntryNo != 1 || m.Live || m.History[0].ReworkExecutionID != "X-1" || m.History[0].ReworkNo != 1 {
		t.Fatalf("H01: %+v", m)
	}
	// H03, never loaded, leaves the consolidation that has a rework of another house, and records none
	ok(t, 2)(f.module.DetachHouse(ctx, f.detach("detach-h03-after", "C-S", "H03", 1, api.ReasonShutOut, "", f.hoursIn(8))))
	if m := f.membership(t, "H03"); m.Live || m.Revision != 2 || m.History[0].End != api.EntryDetached || m.History[0].ReworkNo != 0 || m.History[0].ReworkExecutionID != "" {
		t.Fatalf("H03 left with no rework: %+v", m)
	}
}

// TestAHouseTakenOutByOrdinaryReversalsLeavesWithNoLinkWhileAnotherHousesReworkExists (V2.86 called it
// TestAHouseUnloadedForAnotherReasonStays): H03 was loaded and taken out again before the container was sealed
// (MISLOADED, an ordinary reversal), so it has no live packs and a reversal, but none that a rework wrote. It leaves
// with no rework recorded though a rework of the consolidation unloaded H01, and a raw end that names H01's rework is
// refused.
func TestAHouseTakenOutByOrdinaryReversalsLeavesWithNoLinkWhileAnotherHousesReworkExists(t *testing.T) {
	ctx := context.Background()
	f := reworkDay(t)
	f.begin(t, "X-1", 1)
	at := f.hoursIn(4)
	loaded := ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load-H03", "X-1", 2, "L-H03", 2, at)))
	ok(t, 4)(f.module.RecordCargoLoaded(ctx, f.load("load-H01", "X-1", 3, "L-H01", 4, at)))
	ok(t, 5)(f.module.RecordCargoUnloaded(ctx, api.RecordCargoUnloaded{CommandID: "unload-H03", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-1", ExpectedRevision: 4,
		LineNo: loaded.ItemNo, Packs: 2, Reason: api.ReversalMisloaded, Evidence: evidence("unload-H03", at), OccurredAt: at}))
	ok(t, 6)(f.module.CloseLoadExecution(ctx, f.sealOf("X-1", 5, "SEAL-1")))
	f.accepted(t, "a raw end of a house taken out for another reason, before any rework, with no rework", endEntry("H03", 1, "DETACHED", "", 0, "attached_at + interval '10 hours'"))
	f.refused(t, "a raw end of that house naming a rework, before any", "23514", "membership_entry_rework_link", endEntry("H03", 1, "DETACHED", "X-1", 1, "attached_at + interval '10 hours'"))
	ok(t, 7)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-1", "X-1", 6, "H01 is rolled", "ops-asker", f.hoursIn(6))))
	ok(t, 8)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("unload-h01", "X-1", 7, "H01", f.hoursIn(7))))
	f.refused(t, "a raw end of that house naming the rework that unloaded another", "23514", "membership_entry_rework_link",
		endEntry("H03", 1, "DETACHED", "X-1", 1, "attached_at + interval '10 hours'"))
	f.accepted(t, "a raw end of that house with no rework, a rework of the consolidation being in rework of another house", endEntry("H03", 1, "DETACHED", "", 0, "attached_at + interval '10 hours'"))
	ok(t, 2)(f.module.DetachHouse(ctx, f.detach("detach-h03", "C-S", "H03", 1, api.ReasonShutOut, "", f.hoursIn(8))))
	if e := f.membership(t, "H03").History[0]; e.End != api.EntryDetached || e.ReworkNo != 0 || e.ReworkExecutionID != "" {
		t.Fatalf("H03 left with no rework, though the consolidation has one of H01: %+v", e)
	}
	ok(t, 2)(f.module.DetachHouse(ctx, f.detach("detach-h01", "C-S", "H01", 1, api.ReasonRollover, "", f.hoursIn(8))))
	if e := f.membership(t, "H01").History[0]; e.ReworkExecutionID != "X-1" || e.ReworkNo != 1 {
		t.Fatalf("H01 left naming its rework: %+v", e)
	}
}

// TestAHouseLoadedAgainAfterTheReworkStaysUntilItIsUnloaded: after its unloading in the rework H03 is loaded into the
// other container; with live packs it may not leave, in the database either (the entry's own rule, before the deferred
// one of the commit); unloaded from there for an ordinary reason it leaves, naming the rework that unloaded it before.
func TestAHouseLoadedAgainAfterTheReworkStaysUntilItIsUnloaded(t *testing.T) {
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
	loaded := ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load-h03-x2", "X-2", 2, "L-H03", 2, f.hoursIn(8))))
	_, err := f.module.DetachHouse(ctx, f.detach("detach-loaded-again", "C-S", "H03", 1, api.ReasonRollover, "", f.hoursIn(9)))
	f.refusedCommand(t, "H03 loaded in X-2", err, api.ErrRejected, "")
	f.refused(t, "the database's rule for live packs, before the commit's", "23514", "membership_entry_rework_loaded", endEntry("H03", 1, "DETACHED", "X-1", 1, "attached_at + interval '10 hours'"))
	ok(t, 4)(f.module.RecordCargoUnloaded(ctx, api.RecordCargoUnloaded{CommandID: "unload-x2", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-2", ExpectedRevision: 3,
		LineNo: loaded.ItemNo, Packs: 2, Reason: api.ReversalMisloaded, Evidence: evidence("unload-x2", f.hoursIn(9)), OccurredAt: f.hoursIn(9)}))
	ok(t, 2)(f.module.DetachHouse(ctx, f.detach("detach-h03", "C-S", "H03", 1, api.ReasonRollover, "", f.hoursIn(10))))
	if e := f.membership(t, "H03").History[0]; e.End != api.EntryDetached || e.ReworkExecutionID != "X-1" || e.ReworkNo != 1 {
		t.Fatalf("H03: %+v", e)
	}
}

// TestTheEntryRecordsTheLatestReworkThatUnloadedTheHouse: H03 is unloaded in a rework of X-1, loaded into X-2, which is
// sealed and opened for rework, and unloaded there. Two containers, two rounds; the entry names X-2's. (One container
// cannot unload the same house twice: a container in rework takes no load, and a sealed one is no loading.) The
// database accepts the earlier rework too, as a raw write.
func TestTheEntryRecordsTheLatestReworkThatUnloadedTheHouse(t *testing.T) {
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
	ok(t, 5)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-x2", "X-2", 4, "again", "ops-asker", f.hoursIn(9))))
	ok(t, 6)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("unload-h03-x2", "X-2", 5, "H03", f.hoursIn(10))))
	f.accepted(t, "an earlier rework of the house, as a raw write", endEntry("H03", 1, "DETACHED", "X-1", 1, "attached_at + interval '11 hours'"))
	f.accepted(t, "the latest, as a raw write", endEntry("H03", 1, "DETACHED", "X-2", 1, "attached_at + interval '11 hours'"))
	ok(t, 2)(f.module.DetachHouse(ctx, f.detach("detach-h03", "C-S", "H03", 1, api.ReasonCustomerRequest, "", f.hoursIn(11))))
	if e := f.membership(t, "H03").History[0]; e.End != api.EntryDetached || e.ReworkExecutionID != "X-2" || e.ReworkNo != 1 {
		t.Fatalf("the latest round: %+v", e)
	}
}

// TestTheDatabaseRulesOfAHouseThatLeaves: the raw writes of the application role to the entry of H03 in a CLOSED
// consolidation, one rule at a time.
func TestTheDatabaseRulesOfAHouseThatLeaves(t *testing.T) {
	f := leaveDay(t)
	f.reseal(t, 8)
	end := func(house string, link string) string {
		return fmt.Sprintf(`UPDATE cargo_phy.membership_entry SET end_kind='DETACHED', end_reason='ROLLOVER', ended_by='raw', ended_at=attached_at + interval '10 hours',
          end_entered_at=now() %s WHERE tenant_id=$1 AND shipment_id='%s' AND entry_no=1`, link, house)
	}
	const rework1 = `, end_rework_execution_id='X-1', end_rework_no=1`
	// an entry is inserted live: an ended one, naming a rework that never existed or none, is refused by name
	insertEnded := func(link string) string {
		return `INSERT INTO cargo_phy.membership_entry (tenant_id,shipment_id,stage,entry_no,consolidation_id,start_kind,ful_revision,plan_version,packs,
          gross_weight_unit,gross_weight,attached_by,attached_at,entered_at,end_kind,end_reason,ended_by,ended_at,end_entered_at` + link + `)
          SELECT tenant_id,shipment_id,stage,2,consolidation_id,'ATTACHED',ful_revision,plan_version,packs,gross_weight_unit,gross_weight,attached_by,attached_at,
          entered_at,'DETACHED','ROLLOVER','raw',attached_at + interval '10 hours',now()` + map[bool]string{true: ",'NO_SUCH',99", false: ""}[link != ""] + `
          FROM cargo_phy.membership_entry WHERE tenant_id=$1 AND shipment_id='H03' AND entry_no=1`
	}
	f.refused(t, "an ended entry inserted, naming a rework that never existed", "23514", "membership_entry_inserted_live", insertEnded(`,end_rework_execution_id,end_rework_no`))
	f.refused(t, "an ended entry inserted, naming none", "23514", "membership_entry_inserted_live", insertEnded(""))
	f.refused(t, "a live entry inserted with a rework named", "23514", "membership_entry_inserted_live",
		`INSERT INTO cargo_phy.membership_entry (tenant_id,shipment_id,stage,entry_no,consolidation_id,start_kind,ful_revision,plan_version,packs,gross_weight_unit,
          gross_weight,attached_by,attached_at,entered_at,end_rework_execution_id,end_rework_no) SELECT tenant_id,shipment_id,stage,2,consolidation_id,'ATTACHED',
          ful_revision,plan_version,packs,gross_weight_unit,gross_weight,attached_by,attached_at,entered_at,'X-1',1
          FROM cargo_phy.membership_entry WHERE tenant_id=$1 AND shipment_id='H03' AND entry_no=1`)
	f.refused(t, "CLOSED, no rework named", "23514", "membership_entry_rework_link", end("H03", ""))
	f.refused(t, "CLOSED, the rework of another house", "23514", "membership_entry_rework_link", end("H02", rework1))
	f.refused(t, "CLOSED, a rework number the execution does not have", "23514", "membership_entry_rework_link", end("H03", `, end_rework_execution_id='X-1', end_rework_no=2`))
	f.refused(t, "CLOSED, a rework of an execution that does not exist", "23514", "membership_entry_rework_link", end("H03", `, end_rework_execution_id='NO_SUCH_EXECUTION', end_rework_no=1`))
}

// TestTheReworkReversalIsJoinedToItsOwnExecutionsLine: line numbers repeat from one execution to the next. X-1 holds
// H01 as its line 1 and H02 as its line 2, X-2 holds H03 as its line 1. A rework of X-1 unloads H01, a reversal of line
// 1 of X-1. H03, whose own line 1 is in X-2, did not leave by that rework: the database does not read the reversal of
// X-1's line 1 as a reversal of H03's line 1 of another execution, and refuses the end by the rework link, not by H03's
// packs.
func TestTheReworkReversalIsJoinedToItsOwnExecutionsLine(t *testing.T) {
	ctx := context.Background()
	f := reworkDay(t)
	f.begin(t, "X-1", 1)
	f.begin(t, "X-2", 2)
	at := f.hoursIn(4)
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("a-h01", "X-1", 2, "L-H01", 4, at)))
	ok(t, 4)(f.module.RecordCargoLoaded(ctx, f.load("a-h02", "X-1", 3, "L-H02", 6, at)))
	ok(t, 5)(f.module.CloseLoadExecution(ctx, f.sealOf("X-1", 4, "SEAL-1")))
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("b-h03", "X-2", 2, "L-H03", 2, at)))
	ok(t, 4)(f.module.CloseLoadExecution(ctx, f.sealOf("X-2", 3, "SEAL-2")))
	ok(t, 6)(f.module.OpenStuffingForRework(ctx, f.openRework("rework-1", "X-1", 5, "H01 is rolled", "ops-asker", f.hoursIn(6))))
	ok(t, 7)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("unload-h01", "X-1", 6, "H01", f.hoursIn(7))))
	if x := f.execution(t, "X-1"); len(x.Reversals) != 1 || x.Reversals[0].LineNo != 1 {
		t.Fatalf("the rework's reversal is of line 1: %+v", x.Reversals)
	}
	if x := f.execution(t, "X-2"); len(x.Lines) != 1 || x.Lines[0].LineNo != 1 || x.Lines[0].ShipmentID != "H03" {
		t.Fatalf("H03's line 1 is in X-2: %+v", x.Lines)
	}
	f.refused(t, "H03 named with the rework that unloaded H01, whose line has H03's number", "23514", "membership_entry_rework_link",
		endEntry("H03", 1, "DETACHED", "X-1", 1, "attached_at + interval '10 hours'"))
	_, err := f.module.DetachHouse(ctx, f.detach("detach-h03", "C-S", "H03", 1, api.ReasonShutOut, "", f.hoursIn(8)))
	f.refusedCommand(t, "H03, loaded and never unloaded in a rework", err, api.ErrRejected, "packs")
}

// TestEveryHouseLeavingEmptiesTheClosedConsolidationAndItReopens: all three houses are unloaded in one rework and
// detached one by one, the container, empty, is given up, and the consolidation, which no sealed container holds any
// more, reopens: it is OPEN again before departure, with a rework in its history. This is how a closed consolidation
// can be emptied and reopened now; the rule that froze its houses made that impossible.
func TestEveryHouseLeavingEmptiesTheClosedConsolidationAndItReopens(t *testing.T) {
	ctx := context.Background()
	f := leaveDay(t)
	ok(t, 9)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("unload-h02", "X-1", 8, "H02", f.hoursIn(8))))
	ok(t, 10)(f.module.UnloadHouseForRework(ctx, f.unloadHouse("unload-h01", "X-1", 9, "H01", f.hoursIn(9))))
	for _, h := range []string{"H01", "H02", "H03"} {
		ok(t, 2)(f.module.DetachHouse(ctx, f.detach("detach-"+h, "C-S", h, 1, api.ReasonRollover, "", f.hoursIn(10))))
		if m := f.membership(t, h); m.Live || m.History[0].ReworkExecutionID != "X-1" || m.History[0].ReworkNo != 1 {
			t.Fatalf("%s: %+v", h, m)
		}
	}
	if c := f.consolidation(t, "C-S"); c.State != api.StateClosed || len(c.Members) != 0 {
		t.Fatalf("C-S, emptied: %+v", c)
	}
	ok(t, 11)(f.module.CancelLoadExecution(ctx, f.cancelCmd("cancel-x1", "X-1", 10, "every house left", f.hoursIn(11))))
	ok(t, 5)(f.module.ReopenConsolidation(ctx, api.ReopenConsolidation{CommandID: "reopen", TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: "C-S",
		ExpectedRevision: 4, Reason: "every house left the container", OccurredAt: f.hoursIn(12)}))
	if c := f.consolidation(t, "C-S"); c.State != api.StateOpen || len(c.Members) != 0 {
		t.Fatalf("C-S reopened: %+v", c)
	}
	if x := f.execution(t, "X-1"); x.State != api.ExecutionCancelled || len(x.Reworks) != 1 {
		t.Fatalf("X-1: %+v", x)
	}
}
