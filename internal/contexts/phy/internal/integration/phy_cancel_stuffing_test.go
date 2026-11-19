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

	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/api"
)

// Giving up a stuffing (CancelLoadExecution, V2.82) with the real commands and the real database.

func (f *fixture) cancelCmd(command, execution string, expected int64, reason string, at time.Time) api.CancelLoadExecution {
	return api.CancelLoadExecution{CommandID: command, TenantID: f.tenant, ActorID: "ops-1", ExecutionID: execution, ExpectedRevision: expected,
		Reason: reason, OccurredAt: at}
}

func (f *fixture) execution(t *testing.T, id string) api.LoadExecution {
	t.Helper()
	x, err := f.module.GetLoadExecution(context.Background(), api.GetLoadExecution{TenantID: f.tenant, ExecutionID: id})
	if err != nil {
		t.Fatalf("execution %s: %v", id, err)
	}
	return x
}

// begin opens the stuffing of a unit and starts loading it; the stuffing is then at revision 2.
func (f *fixture) begin(t *testing.T, x string, unit int) { f.beginAt(t, x, unit, f.hoursIn(4)) }

// beginAt is begin at the business time given.
func (f *fixture) beginAt(t *testing.T, x string, unit int, at time.Time) {
	t.Helper()
	ctx := context.Background()
	ok(t, 1)(f.module.OpenLoadExecution(ctx, f.stuff("open-"+x, x, unit, at)))
	ok(t, 2)(f.module.StartLoading(ctx, api.StartLoading{CommandID: "start-" + x, TenantID: f.tenant, ActorID: "ops-1", ExecutionID: x,
		ExpectedRevision: 1, OccurredAt: at}))
}

// sealOf seals the stuffing, which is at the revision given, with the seal named.
func (f *fixture) sealOf(x string, revision int64, seal string) api.CloseLoadExecution {
	at := f.hoursIn(4)
	return api.CloseLoadExecution{CommandID: "seal-" + x + "-" + seal, TenantID: f.tenant, ActorID: "ops-1", ExecutionID: x, ExpectedRevision: revision,
		Evidence: api.Evidence{Source: api.EvidenceSeal, Reference: seal, OccurredAt: at}, OccurredAt: at}
}

func (f *fixture) outboxCount(t *testing.T, eventType string) int {
	t.Helper()
	n := 0
	for _, kind := range f.outboxTypes(t) {
		if kind == eventType {
			n++
		}
	}
	return n
}

// cancelDay is the day of the V2.80 review's two probes: the own-container consolidation C-S of stuffingDay (units 1
// and 2; the first container is sealed with all of SH-S1's 6 packs and 3 packs of SH-S2) and a stuffing X-2 opened on
// unit 2 that holds nothing: left empty, or, if unloaded, loaded with 2 packs of SH-S2 which came out again. X-2 is at
// revision 2, or 4 once unloaded.
func cancelDay(t *testing.T, unloaded bool) (*fixture, int64) {
	t.Helper()
	ctx := context.Background()
	f := stuffingDay(t)
	at := f.hoursIn(4)
	f.begin(t, "X-1", 1)
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load-1a", "X-1", 2, "L-S1", 6, at)))
	ok(t, 4)(f.module.RecordCargoLoaded(ctx, f.load("load-1b", "X-1", 3, "L-S2", 3, at)))
	ok(t, 5)(f.module.CloseLoadExecution(ctx, f.sealOf("X-1", 4, "SEAL-1")))
	f.begin(t, "X-2", 2)
	if !unloaded {
		return f, 2
	}
	line := ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load-2", "X-2", 2, "L-S2", 2, at)))
	ok(t, 4)(f.module.RecordCargoUnloaded(ctx, api.RecordCargoUnloaded{CommandID: "unload-2", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-2",
		ExpectedRevision: 3, LineNo: line.ItemNo, Packs: 2, Reason: api.ReversalMisloaded, Evidence: evidence("unload-2", at), OccurredAt: at}))
	return f, 4
}

// TestAnEmptyStuffingIsGivenUp ends the two probes of the review of V2.80 with the cancel. A stuffing opened on a
// container and left empty could be neither sealed ("nothing is stuffed in the container") nor removed with its
// unit, which stayed IN_USE for ever, and FIN's container methods were refused for the pool for ever; the same when
// packs were loaded and unloaded again. Now the stuffing is given up: CANCELLED, kept as history with its lines and
// reversals, with who, when and why; the unit reads PLANNED again, with no execution, no state and revision 0, and
// has no packing list; then either the unit is removed, or a new stuffing is opened on it (a new execution: ids are
// never reused), loaded and sealed, and the unit reads CLOSED with the new execution.
func TestAnEmptyStuffingIsGivenUp(t *testing.T) {
	ctx := context.Background()
	for _, unloaded := range []bool{false, true} {
		for _, restuff := range []bool{false, true} {
			name := map[bool]string{false: "opened and left empty", true: "loaded and unloaded again"}[unloaded] + ", " +
				map[bool]string{false: "the unit is removed", true: "the unit is stuffed again"}[restuff]
			t.Run(name, func(t *testing.T) {
				f, revision := cancelDay(t, unloaded)
				at := f.hoursIn(5)
				if _, err := f.module.CloseLoadExecution(ctx, f.sealOf("X-2", revision, "SEAL-2")); !errors.Is(err, api.ErrRejected) ||
					!strings.Contains(err.Error(), "nothing is stuffed") {
					t.Fatalf("the empty container is not sealed: %v", err)
				}
				remove := api.RemovePlannedLoadUnit{CommandID: "remove-2", TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: "C-S", ExpectedRevision: 4,
					UnitNo: 2, Reason: "one container is enough", OccurredAt: at}
				if _, err := f.module.RemovePlannedLoadUnit(ctx, remove); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "PHY_LOAD_UNIT_IN_USE") {
					t.Fatalf("the unit with its stuffing is not removed: %v", err)
				}
				before := f.basis(t)
				if u := before.LoadUnits[1]; u.Status != api.UnitStatusInUse || u.ExecutionID != "X-2" || u.ExecutionState != api.ExecutionLoading || u.ExecutionRevision != revision ||
					len(before.PackingLists) != 2 || before.PackingLists[1].UnitNo != 2 || before.PackingLists[1].ExecutionID != "X-2" {
					t.Fatalf("before: unit 2 %+v, %d packing lists", u, len(before.PackingLists))
				}

				cancel := f.cancelCmd("cancel-2", "X-2", revision, "the container was rolled to the next sailing", at)
				receipt := ok(t, revision+1)(f.module.CancelLoadExecution(ctx, cancel))
				if receipt.ExecutionID != "X-2" || receipt.ConsolidationID != "C-S" {
					t.Fatalf("receipt = %+v", receipt)
				}
				if again, err := f.module.CancelLoadExecution(ctx, cancel); err != nil || again != receipt {
					t.Fatalf("replay = %+v, %v; want the same receipt %+v", again, err, receipt)
				}
				if n := f.outboxCount(t, api.LoadExecutionCancelled); n != 1 {
					t.Fatalf("%d LoadExecutionCancelled events, want 1 (the replay writes none)", n)
				}
				var payload string
				if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
					return e.QueryRow(ctx, `SELECT payload::text FROM bento.outbox WHERE scope=$1 AND source=$2 AND event_type=$3`, f.tenant, api.Source,
						api.LoadExecutionCancelled).Scan(&payload)
				}); err != nil || !strings.Contains(payload, `"execution_id": "X-2"`) || !strings.Contains(payload, `"consolidation_id": "C-S"`) ||
					!strings.Contains(payload, `"item_no": 2`) ||
					strings.Contains(payload, "rolled") {
					t.Fatalf("the event's payload names the execution and carries no reason text: %s, %v", payload, err)
				}

				x := f.execution(t, "X-2")
				if x.State != api.ExecutionCancelled || x.Revision != revision+1 || x.Kind != api.ExecutionStuffing || x.LoadUnitNo != 2 || x.TenderedPacks != 0 ||
					x.CancelledBy != "ops-1" || !x.CancelledAt.Equal(at) || x.CancelReason != "the container was rolled to the next sailing" {
					t.Fatalf("the cancelled stuffing: %+v", x)
				}
				if unloaded && (len(x.Lines) != 1 || x.Lines[0].LivePacks != 0 || len(x.Reversals) != 1) {
					t.Fatalf("a cancelled stuffing keeps its lines and reversals: %+v", x)
				}
				for what, u := range map[string]api.PlannedLoadUnit{"the consolidation": f.unit(t, 2), "the cost basis": f.basis(t).LoadUnits[1]} {
					if u.Status != api.UnitStatusPlanned || u.ExecutionID != "" || u.ExecutionState != "" || u.ExecutionRevision != 0 {
						t.Fatalf("%s says unit 2 is %+v, want PLANNED with no stuffing", what, u)
					}
				}
				after := f.basis(t)
				if len(after.PackingLists) != 1 || after.PackingLists[0].UnitNo != 1 || after.PackingLists[0].Digest != before.PackingLists[0].Digest {
					t.Fatalf("a cancelled stuffing has no packing list, and unit 1's is as it was: %+v", after.PackingLists)
				}
				if _, err := f.module.CloseLoadExecution(ctx, f.sealOf("X-2", revision+1, "SEAL-2")); !errors.Is(err, api.ErrRejected) {
					t.Fatalf("a cancelled stuffing is not sealed: %v", err)
				}

				if !restuff {
					remove.CommandID = "remove-2-again"
					ok(t, 5)(f.module.RemovePlannedLoadUnit(ctx, remove))
					if u := f.unit(t, 2); u.Status != api.UnitStatusRemoved || u.RemoveReason != "one container is enough" {
						t.Fatalf("unit 2 = %+v", u)
					}
					if _, err := f.module.OpenLoadExecution(ctx, f.stuff("open-removed", "X-9", 2, at)); !errors.Is(err, api.ErrRejected) {
						t.Fatalf("a removed unit takes no stuffing: %v", err)
					}
					return
				}
				// the unit is stuffed again, by a new execution: the cancelled one's id is never reused
				if _, err := f.module.OpenLoadExecution(ctx, f.stuff("open-reuse", "X-2", 2, at)); !errors.Is(err, api.ErrConflict) {
					t.Fatalf("an execution id is not reused: %v", err)
				}
				opened := ok(t, 1)(f.module.OpenLoadExecution(ctx, f.stuff("open-3", "X-3", 2, at)))
				if opened.ItemNo != 2 || opened.ExecutionID != "X-3" {
					t.Fatalf("receipt = %+v", opened)
				}
				if u := f.unit(t, 2); u.Status != api.UnitStatusInUse || u.ExecutionID != "X-3" || u.ExecutionState != api.ExecutionOpen || u.ExecutionRevision != 1 {
					t.Fatalf("unit 2 = %+v", u)
				}
				if _, err := f.module.OpenLoadExecution(ctx, f.stuff("open-4", "X-4", 2, at)); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "PHY_LOAD_UNIT_IN_USE") {
					t.Fatalf("a unit has one stuffing that is not cancelled: %v", err)
				}
				remove.CommandID = "remove-2-stuffed"
				if _, err := f.module.RemovePlannedLoadUnit(ctx, remove); !errors.Is(err, api.ErrRejected) {
					t.Fatalf("a unit with a stuffing that is not cancelled is not removed: %v", err)
				}
				ok(t, 2)(f.module.StartLoading(ctx, api.StartLoading{CommandID: "start-3", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-3",
					ExpectedRevision: 1, OccurredAt: at}))
				ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load-3", "X-3", 2, "L-S2", 3, at)))
				seal := f.sealOf("X-3", 3, "SEAL-3")
				seal.OccurredAt, seal.Evidence.OccurredAt = at, at
				ok(t, 4)(f.module.CloseLoadExecution(ctx, seal))
				done := f.basis(t)
				if len(done.PackingLists) != 2 || done.PackingLists[1].ExecutionID != "X-3" || done.PackingLists[1].ExecutionState != api.ExecutionClosed ||
					len(done.PackingLists[1].Lines) != 1 || done.PackingLists[1].Lines[0].ShipmentID != "SH-S2" || done.PackingLists[1].Lines[0].LoadedPacks != 3 {
					t.Fatalf("the new stuffing's packing list: %+v", done.PackingLists)
				}
				for i, want := range []struct {
					execution string
					revision  int64
				}{{"X-1", 5}, {"X-3", 4}} {
					if u := done.LoadUnits[i]; u.Status != api.UnitStatusClosed || u.ExecutionState != api.ExecutionClosed || u.ExecutionID != want.execution ||
						u.ExecutionRevision != want.revision {
						t.Fatalf("unit %d once both are sealed: %+v, want %+v", i+1, u, want)
					}
				}
				if x := f.execution(t, "X-2"); x.State != api.ExecutionCancelled {
					t.Fatalf("the first stuffing of unit 2 stays cancelled: %+v", x)
				}
				// SH-S2's lot of 300 kg is loaded 3 + 3 packs in the two containers: 150 + 150 kg, the lot
				for _, h := range done.Houses {
					if h.ShipmentID == "SH-S2" && (h.LoadedPacks != 6 || h.LoadedGrossWeight != kgm(300) || len(h.Units) != 2 || h.Units[0].LoadedGrossWeight != kgm(150) ||
						h.Units[1].LoadedGrossWeight != kgm(150)) {
						t.Fatalf("SH-S2 over the two containers: %+v", h)
					}
				}
			})
		}
	}
}

// TestGivingUpAStuffingIsRefusedOutsideItsRules runs each rule of the command and what a cancelled stuffing refuses
// afterwards: every refusal is ErrRejected, and changes neither the execution's revision nor the events.
func TestGivingUpAStuffingIsRefusedOutsideItsRules(t *testing.T) {
	ctx := context.Background()
	f := stuffingDay(t)
	at := f.hoursIn(4)
	// unit 1: a stuffing with live packs; unit 2: a sealed container
	f.begin(t, "X-S1", 1)
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load-1", "X-S1", 2, "L-S1", 6, at)))
	f.begin(t, "X-S2", 2)
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load-2", "X-S2", 2, "L-S2", 6, at)))
	ok(t, 4)(f.module.CloseLoadExecution(ctx, f.sealOf("X-S2", 3, "SEAL-2")))
	// a tender: an AIR consolidation of its own, in the same tenant
	f.create(t, "C-A")
	f.tender(t, "X-T", "C-A", at)

	refused := func(what, execution string, revision int64, want string, err error) {
		t.Helper()
		if !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want ErrRejected saying %q", what, err, want)
		}
		if x := f.execution(t, execution); x.Revision != revision {
			t.Errorf("%s: the revision moved to %d", what, x.Revision)
		}
	}
	events := f.outboxCount(t, api.LoadExecutionCancelled)
	_, err := f.module.CancelLoadExecution(ctx, f.cancelCmd("c-live", "X-S1", 3, "rolled", at.Add(time.Hour)))
	refused("a stuffing with live packs", "X-S1", 3, "6 packs are still loaded", err)
	_, err = f.module.CancelLoadExecution(ctx, f.cancelCmd("c-sealed", "X-S2", 4, "rolled", at.Add(time.Hour)))
	refused("a sealed container", "X-S2", 4, "CLOSED", err)
	_, err = f.module.CancelLoadExecution(ctx, f.cancelCmd("c-tender", "X-T", 2, "no flight", at.Add(time.Hour)))
	refused("a tender", "X-T", 2, "only a stuffing", err)
	if x := f.execution(t, "X-S1"); x.State != api.ExecutionLoading || x.TenderedPacks != 6 {
		t.Fatalf("the stuffing with live packs is as it was: %+v", x)
	}

	// X-S1's packs come out; the rest of the rules
	ok(t, 4)(f.module.RecordCargoUnloaded(ctx, api.RecordCargoUnloaded{CommandID: "unload-1", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-S1",
		ExpectedRevision: 3, LineNo: 1, Packs: 5, Reason: api.ReversalMisloaded, Evidence: evidence("unload-1", f.hoursIn(5)), OccurredAt: f.hoursIn(5)}))
	_, err = f.module.CancelLoadExecution(ctx, f.cancelCmd("c-one", "X-S1", 4, "rolled", f.hoursIn(6)))
	refused("a pack still live", "X-S1", 4, "1 packs are still loaded", err)
	ok(t, 5)(f.module.RecordCargoUnloaded(ctx, api.RecordCargoUnloaded{CommandID: "unload-1b", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-S1",
		ExpectedRevision: 4, LineNo: 1, Packs: 1, Reason: api.ReversalMisloaded, Evidence: evidence("unload-1b", f.hoursIn(5)), OccurredAt: f.hoursIn(5)}))
	for name, tc := range map[string]struct {
		reason string
		at     time.Time
		want   string
	}{
		"no reason":                 {"", f.hoursIn(6), "reason"},
		"a blank reason":            {"  \t", f.hoursIn(6), "reason"},
		"a reason of 501":           {strings.Repeat("x", 501), f.hoursIn(6), "reason"},
		"before the last unload":    {"rolled", f.hoursIn(5).Add(-time.Minute), "ErrInvalidChange"},
		"before the stuffing began": {"rolled", f.hoursIn(3), "ErrInvalidChange"},
	} {
		_, err = f.module.CancelLoadExecution(ctx, f.cancelCmd("c-"+name, "X-S1", 5, tc.reason, tc.at))
		if !errors.Is(err, api.ErrRejected) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err = f.module.CancelLoadExecution(ctx, f.cancelCmd("c-unknown", "X-NO-SUCH", 1, "rolled", f.hoursIn(6))); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("an unknown execution: %v", err)
	}
	if _, err = f.module.CancelLoadExecution(ctx, f.cancelCmd("c-stale", "X-S1", 4, "rolled", f.hoursIn(6))); !errors.Is(err, api.ErrConflict) {
		t.Errorf("a stale revision: %v", err)
	}
	intruder := f.cancelCmd("c-intruder", "X-S1", 5, "rolled", f.hoursIn(6))
	intruder.ActorID = "intruder"
	if _, err = f.module.CancelLoadExecution(ctx, intruder); !errors.Is(err, api.ErrUnauthorized) {
		t.Errorf("an actor the policy refuses: %v", err)
	}
	if n := f.outboxCount(t, api.LoadExecutionCancelled); n != events {
		t.Fatalf("refused cancellations wrote %d events", n-events)
	}
	// a reason of 500 characters, counted as characters, is accepted
	reason := strings.Repeat("é", 500)
	ok(t, 6)(f.module.CancelLoadExecution(ctx, f.cancelCmd("c-ok", "X-S1", 5, reason, f.hoursIn(6))))
	if x := f.execution(t, "X-S1"); x.State != api.ExecutionCancelled || x.CancelReason != reason || len(x.Lines) != 1 || len(x.Reversals) != 2 {
		t.Fatalf("X-S1 = %+v", x)
	}
	refused("a second cancel", "X-S1", 6, "CANCELLED", func() error {
		_, err := f.module.CancelLoadExecution(ctx, f.cancelCmd("c-twice", "X-S1", 6, "again", f.hoursIn(7)))
		return err
	}())

	// from ON_HOLD with a hold placed, and from OPEN: both on unit 1, freed by the first cancellation. The hold ends
	// with
	// the stuffing and is not released: releasing the last hold would resume loading.
	f.beginAt(t, "X-S3", 1, f.hoursIn(6))
	at = f.hoursIn(6)
	ok(t, 3)(f.module.PlaceLoadExecutionOnHold(ctx, api.PlaceLoadExecutionOnHold{CommandID: "hold-3", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-S3",
		ExpectedRevision: 2, Reason: api.HoldCustoms, Evidence: evidence("hold-3", at), OccurredAt: at}))
	if x := f.execution(t, "X-S3"); x.State != api.ExecutionOnHold {
		t.Fatalf("X-S3 = %+v", x)
	}
	ok(t, 4)(f.module.CancelLoadExecution(ctx, f.cancelCmd("c-held", "X-S3", 3, "customs sent the container back", f.hoursIn(7))))
	held := f.execution(t, "X-S3")
	if held.State != api.ExecutionCancelled || len(held.Holds) != 1 || held.Holds[0].Released {
		t.Fatalf("a hold ends with the stuffing and stays as it was: %+v", held)
	}
	ok(t, 1)(f.module.OpenLoadExecution(ctx, f.stuff("open-s4", "X-S4", 1, f.hoursIn(8))))
	ok(t, 2)(f.module.CancelLoadExecution(ctx, f.cancelCmd("c-open", "X-S4", 1, "opened by mistake", f.hoursIn(8))))
	if x := f.execution(t, "X-S4"); x.State != api.ExecutionCancelled || !x.StartedAt.IsZero() {
		t.Fatalf("a stuffing given up while OPEN never started: %+v", x)
	}

	// a cancelled stuffing takes no further command
	later := f.hoursIn(9)
	events = f.outboxCount(t, api.LoadExecutionCancelled)
	for name, err := range map[string]error{
		"start": func() error {
			_, err := f.module.StartLoading(ctx, api.StartLoading{CommandID: "n-start", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-S3", ExpectedRevision: 4,
				OccurredAt: later})
			return err
		}(),
		"load": func() error {
			_, err := f.module.RecordCargoLoaded(ctx, f.load("n-load", "X-S3", 4, "L-S1", 1, later))
			return err
		}(),
		"unload": func() error {
			_, err := f.module.RecordCargoUnloaded(ctx, api.RecordCargoUnloaded{CommandID: "n-unload", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-S1",
				ExpectedRevision: 6, LineNo: 1, Packs: 1, Reason: api.ReversalMisloaded, Evidence: evidence("n-unload", later), OccurredAt: later})
			return err
		}(),
		"hold": func() error {
			_, err := f.module.PlaceLoadExecutionOnHold(ctx, api.PlaceLoadExecutionOnHold{CommandID: "n-hold", TenantID: f.tenant, ActorID: "ops-1",
				ExecutionID: "X-S3", ExpectedRevision: 4, Reason: api.HoldDamage, Evidence: evidence("n-hold", later), OccurredAt: later})
			return err
		}(),
		"release": func() error {
			_, err := f.module.ReleaseLoadExecutionHold(ctx, api.ReleaseLoadExecutionHold{CommandID: "n-release", TenantID: f.tenant, ActorID: "ops-1",
				ExecutionID: "X-S3", ExpectedRevision: 4, HoldNo: 1, Evidence: evidence("n-release", later), OccurredAt: later})
			return err
		}(),
		"seal": func() error { _, err := f.module.CloseLoadExecution(ctx, f.sealOf("X-S3", 4, "SEAL-N")); return err }(),
		"carrier evidence": func() error {
			_, err := f.module.RecordCarrierLoadEvidence(ctx, api.RecordCarrierLoadEvidence{CommandID: "n-carrier", TenantID: f.tenant, ActorID: "ops-1",
				ExecutionID: "X-S3", ExpectedRevision: 4, Fact: api.CarrierAccepted, Pieces: 1, Transport: "V1", Evidence: evidence("n-carrier", later), OccurredAt: later})
			return err
		}(),
		"resolution": func() error {
			_, err := f.module.ResolveLoadingDiscrepancy(ctx, api.ResolveLoadingDiscrepancy{CommandID: "n-resolve", TenantID: f.tenant, ActorID: "ops-1",
				ExecutionID: "X-S3", ExpectedRevision: 4, DiscrepancyNo: 1, Resolution: api.ResolvedByAccepted, Reason: "ok", OccurredAt: later})
			return err
		}(),
		"cancel": func() error {
			_, err := f.module.CancelLoadExecution(ctx, f.cancelCmd("n-cancel", "X-S3", 4, "again", later))
			return err
		}(),
	} {
		if !errors.Is(err, api.ErrRejected) {
			t.Errorf("%s on a cancelled stuffing: %v, want ErrRejected", name, err)
		}
	}
	if x := f.execution(t, "X-S3"); x.State != api.ExecutionCancelled || x.Revision != 4 || len(x.Holds) != 1 || x.Holds[0].Released {
		t.Fatalf("a cancelled stuffing is as it was: %+v", x)
	}
	if n := f.outboxCount(t, api.LoadExecutionCancelled); n != events {
		t.Fatalf("refused commands wrote %d events", n-events)
	}
}

// TestACancelledStuffingDatabaseRules writes past PHY, as the application role, to see each rule of migration 000006
// refuse by itself: the constraint or trigger named is the one that refuses, and the same write succeeds where the
// rule allows it.
func TestACancelledStuffingDatabaseRules(t *testing.T) {
	ctx := context.Background()
	f := stuffingDay(t)
	at := f.hoursIn(4)
	plan := f.planUnit("plan-3", "C-S", 4, "22G1")
	plan.OccurredAt = f.hoursIn(3)
	ok(t, 5)(f.module.PlanLoadUnit(ctx, plan))
	// X-S1 on unit 1 holds live packs; X-S2 on unit 2 loaded 2 packs, unloaded them, was held and given up; X-S3 on
	// unit 3 is loading and empty
	f.begin(t, "X-S1", 1)
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load-1", "X-S1", 2, "L-S1", 6, at)))
	f.begin(t, "X-S2", 2)
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load-2", "X-S2", 2, "L-S2", 2, at)))
	ok(t, 4)(f.module.RecordCargoUnloaded(ctx, api.RecordCargoUnloaded{CommandID: "unload-2", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-S2",
		ExpectedRevision: 3, LineNo: 1, Packs: 2, Reason: api.ReversalMisloaded, Evidence: evidence("unload-2", at), OccurredAt: at}))
	ok(t, 5)(f.module.PlaceLoadExecutionOnHold(ctx, api.PlaceLoadExecutionOnHold{CommandID: "hold-2", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-S2",
		ExpectedRevision: 4, Reason: api.HoldDamage, Evidence: evidence("hold-2", at), OccurredAt: at}))
	ok(t, 6)(f.module.CancelLoadExecution(ctx, f.cancelCmd("cancel-2", "X-S2", 5, "damaged container", f.hoursIn(5))))
	f.begin(t, "X-S3", 3)
	f.create(t, "C-A")
	f.tender(t, "X-T", "C-A", at)

	write := func(sql string, args ...any) error {
		return f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			_, err := e.Exec(ctx, sql, append([]any{f.tenant}, args...)...)
			return err
		})
	}
	var pgErr *pgconn.PgError
	refuses := func(what, sql, code, constraint string, args ...any) {
		t.Helper()
		if err := write(sql, args...); !errors.As(err, &pgErr) || pgErr.Code != code || pgErr.ConstraintName != constraint {
			t.Errorf("%s: %v, want %s %s", what, err, code, constraint)
		}
	}
	const facts = `cancelled_by='x', cancelled_at=now(), cancelled_entered_at=now(), cancel_reason='r'`
	refuses("a state outside the list", `UPDATE cargo_phy.load_execution SET state='NO_SUCH_STATE' WHERE tenant_id=$1 AND execution_id='X-S3'`,
		"23514", "load_execution_state_check")
	refuses("given up without its facts", `UPDATE cargo_phy.load_execution SET state='CANCELLED' WHERE tenant_id=$1 AND execution_id='X-S3'`,
		"23514", "load_execution_cancelled")
	refuses("the facts of a cancellation on a loading stuffing", `UPDATE cargo_phy.load_execution SET `+facts+` WHERE tenant_id=$1 AND execution_id='X-S3'`,
		"23514", "load_execution_cancelled")
	refuses("given up before it opened", `UPDATE cargo_phy.load_execution SET state='CANCELLED', cancelled_by='x', cancelled_at=opened_at - interval '1 second',
        cancelled_entered_at=now(), cancel_reason='r' WHERE tenant_id=$1 AND execution_id='X-S3'`, "23514", "load_execution_cancelled")
	refuses("given up before it started", `UPDATE cargo_phy.load_execution SET state='CANCELLED', cancelled_by='x', cancelled_at=started_at - interval '1 second',
        cancelled_entered_at=now(), cancel_reason='r' WHERE tenant_id=$1 AND execution_id='X-S3'`, "23514", "load_execution_cancelled")
	refuses("a reason of 501 characters", `UPDATE cargo_phy.load_execution SET state='CANCELLED', cancelled_by='x', cancelled_at=now(), cancelled_entered_at=now(),
        cancel_reason=repeat('x', 501) WHERE tenant_id=$1 AND execution_id='X-S3'`, "23514", "load_execution_cancel_reason_check")
	refuses("a blank author", `UPDATE cargo_phy.load_execution SET state='CANCELLED', cancelled_by=' ', cancelled_at=now(), cancelled_entered_at=now(),
        cancel_reason='r' WHERE tenant_id=$1 AND execution_id='X-S3'`, "23514", "load_execution_cancelled_by_check")
	refuses("a tender given up", `UPDATE cargo_phy.load_execution SET state='CANCELLED', `+facts+` WHERE tenant_id=$1 AND execution_id='X-T'`,
		"23514", "load_execution_cancelled")
	refuses("a stuffing with live packs given up", `UPDATE cargo_phy.load_execution SET state='CANCELLED', `+facts+` WHERE tenant_id=$1 AND execution_id='X-S1'`,
		"23514", "load_execution_cancelled_packs")

	// a cancellation changes nothing of the row but its own columns, revision and updated_at: not the start, not the
	// close
	for what, set := range map[string]string{
		"the start erased": `started_by=NULL, started_at=NULL, started_entered_at=NULL`,
		"the start moved":  `started_at=started_at + interval '1 second'`,
		"a close evidence": `close_evidence_source='SEAL'`,
		"a closing author": `closed_by='x'`,
	} {
		refuses("a cancellation with "+what, `UPDATE cargo_phy.load_execution SET state='CANCELLED', `+facts+`, `+set+` WHERE tenant_id=$1 AND execution_id='X-S3'`,
			"23001", "")
	}
	// a cancelled execution changes no more (the application role may not write its unit or delete it at all)
	for what, tc := range map[string]struct{ sql, code string }{
		"its revision":   {`UPDATE cargo_phy.load_execution SET revision=revision+1 WHERE tenant_id=$1 AND execution_id='X-S2'`, "23001"},
		"its state back": {`UPDATE cargo_phy.load_execution SET state='LOADING' WHERE tenant_id=$1 AND execution_id='X-S2'`, "23001"},
		"its reason":     {`UPDATE cargo_phy.load_execution SET cancel_reason='other' WHERE tenant_id=$1 AND execution_id='X-S2'`, "23001"},
		"its start":      {`UPDATE cargo_phy.load_execution SET started_at=started_at + interval '1 second' WHERE tenant_id=$1 AND execution_id='X-S2'`, "23001"},
		"to be sealed": {`UPDATE cargo_phy.load_execution SET state='CLOSED', closed_by='x', closed_at=now(), closed_entered_at=now(), close_evidence_source='SEAL',
            close_evidence_reference='S', close_evidence_occurred_at=now() WHERE tenant_id=$1 AND execution_id='X-S2'`, "23001"},
		"its unit":   {`UPDATE cargo_phy.load_execution SET load_unit_no=3 WHERE tenant_id=$1 AND execution_id='X-S2'`, "42501"},
		"be deleted": {`DELETE FROM cargo_phy.load_execution WHERE tenant_id=$1 AND execution_id='X-S2'`, "42501"},
	} {
		err := write(tc.sql)
		if !errors.As(err, &pgErr) || pgErr.Code != tc.code || (tc.code == "23001" && !strings.Contains(pgErr.Message, "cancelled")) {
			t.Errorf("a cancelled execution changes %s: %v, want %s", what, err, tc.code)
		}
	}
	// and takes no line, reversal, hold, evidence, discrepancy or release of its hold
	rejected := func(what, sql string) {
		t.Helper()
		if err := write(sql); !errors.As(err, &pgErr) || pgErr.Code != "23514" || !strings.Contains(pgErr.Message, "CANCELLED") {
			t.Errorf("a cancelled execution takes no %s: %v", what, err)
		}
	}
	rejected("line", `INSERT INTO cargo_phy.load_line (tenant_id,execution_id,line_no,consolidation_id,shipment_id,lot_id,entry_no,packs,gross_weight_unit,gross_weight,
        basis,measurement_no,evidence_source,evidence_reference,evidence_occurred_at,loaded_by,loaded_at,entered_at)
        VALUES ($1,'X-S2',2,'C-S','SH-S2','L-S2',1,1,'KGM',1,'APPORTIONED',1,'x','x',now(),'x',now(),now())`)
	rejected("reversal", `INSERT INTO cargo_phy.reversal_line (tenant_id,execution_id,reversal_no,line_no,packs,gross_weight,reason,evidence_source,evidence_reference,
        evidence_occurred_at,reversed_by,reversed_at,entered_at) VALUES ($1,'X-S2',2,1,1,1,'MISLOADED','x','x',now(),'x',now(),now())`)
	rejected("hold", `INSERT INTO cargo_phy.hold_entry (tenant_id,execution_id,hold_no,reason,evidence_source,evidence_reference,evidence_occurred_at,placed_by,placed_at,
        entered_at) VALUES ($1,'X-S2',2,'DAMAGE','x','x',now(),'x',now(),now())`)
	rejected("carrier evidence", `INSERT INTO cargo_phy.carrier_load_evidence (tenant_id,execution_id,evidence_no,fact,pieces,transport_ref,evidence_source,
        evidence_reference,evidence_occurred_at,recorded_by,recorded_at,entered_at) VALUES ($1,'X-S2',1,'ACCEPTED',1,'V1','x','x',now(),'x',now(),now())`)
	rejected("discrepancy", `INSERT INTO cargo_phy.loading_discrepancy (tenant_id,execution_id,discrepancy_no,kind,evidence_no,expected_pieces,observed_pieces,raised_at,
        entered_at) VALUES ($1,'X-S2',1,'LOAD_CONFLICT',1,1,2,now(),now())`)
	refuses("a hold of a cancelled execution released", `UPDATE cargo_phy.hold_entry SET release_evidence_source='x', release_evidence_reference='x',
        release_evidence_occurred_at=now(), released_by='x', released_at=now(), release_entered_at=now() WHERE tenant_id=$1 AND execution_id='X-S2' AND hold_no=1`,
		"23514", "hold_entry_cancelled")

	// one stuffing that is not cancelled per unit; a unit whose stuffings are all cancelled takes another, and is not
	// removed while it has one that is not cancelled
	insert := func(id, unit string) string {
		return fmt.Sprintf(`INSERT INTO cargo_phy.load_execution (tenant_id,execution_id,consolidation_id,kind,transport_mode,facility_location,facility_code,
            load_unit_no,state,opened_by,opened_at,opened_entered_at) VALUES ($1,'%s','C-S','STUFFING','SEA','CNSHG','CFS-1',%s,'OPEN','x',now(),now())`, id, unit)
	}
	refuses("a second stuffing of unit 3", insert("X-D1", "3"), "23505", "load_execution_one_per_unit")
	remove := func(unit string) string {
		return `UPDATE cargo_phy.planned_load_unit SET status='REMOVED', remove_reason='x', removed_by='x', removed_at=now(), remove_entered_at=now()
            WHERE tenant_id=$1 AND consolidation_id='C-S' AND unit_no=` + unit
	}
	refuses("unit 3 removed with its stuffing", remove("3"), "23514", "planned_load_unit_in_use")
	if err := write(insert("X-D2", "2")); err != nil {
		t.Fatalf("a stuffing alongside a cancelled one: %v", err)
	}
	refuses("a second stuffing of unit 2, which has a cancelled one and a live one", insert("X-D3", "2"), "23505", "load_execution_one_per_unit")
	refuses("unit 2 removed with a cancelled stuffing and a live one", remove("2"), "23514", "planned_load_unit_in_use")

	// once sealed, a container is not given up
	ok(t, 4)(f.module.CloseLoadExecution(ctx, f.sealOf("X-S1", 3, "SEAL-1")))
	refuses("a sealed container given up", `UPDATE cargo_phy.load_execution SET state='CANCELLED', `+facts+` WHERE tenant_id=$1 AND execution_id='X-S1'`,
		"23001", "")
	// a cancellation of the unit's stuffing is accepted with nothing else changed: the control of the refusals above
	if err := write(`UPDATE cargo_phy.load_execution SET state='CANCELLED', ` + facts + `, revision=revision+1, updated_at=now() WHERE tenant_id=$1 AND execution_id='X-S3'`); err != nil {
		t.Fatalf("a cancellation that changes its own columns only: %v", err)
	}

}

// TestAConsolidationEndsOverCancelledStuffings: a stuffing left open stops the consolidation from being cancelled, a
// stuffing given up does not (the database's trigger counts only the executions that are neither closed nor cancelled),
// and the reopen, dated not before the cancellation, pauses nothing that is left.
func TestAConsolidationEndsOverCancelledStuffings(t *testing.T) {
	ctx := context.Background()
	f := stuffingDay(t)
	f.begin(t, "X-1", 1)
	reopen := api.ReopenConsolidation{CommandID: "reopen-early", TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: "C-S", ExpectedRevision: 4,
		Reason: "the houses change", OccurredAt: f.hoursIn(6)}
	ok(t, 5)(f.module.ReopenConsolidation(ctx, reopen))
	for _, s := range []string{"SH-S1", "SH-S2"} {
		m, err := f.module.GetConsolMembership(ctx, api.GetConsolMembership{TenantID: f.tenant, ShipmentID: s})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.module.DetachHouse(ctx, api.DetachHouse{CommandID: "detach-" + s, TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: "C-S", ShipmentID: s,
			ExpectedMembershipRevision: m.Revision, Reason: api.ReasonCustomerRequest, OccurredAt: f.hoursIn(7)}); err != nil {
			t.Fatalf("detach %s: %v", s, err)
		}
	}
	c := f.consolidation(t, "C-S")
	cancel := api.CancelConsolidation{CommandID: "cancel-consol", TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: "C-S", ExpectedRevision: c.Revision,
		Reason: "no cargo left", OccurredAt: f.hoursIn(9)}
	if _, err := f.module.CancelConsolidation(ctx, cancel); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "not closed") {
		t.Fatalf("a consolidation with a stuffing left open is not cancelled: %v", err)
	}
	ok(t, 3)(f.module.CancelLoadExecution(ctx, f.cancelCmd("cancel-1", "X-1", 2, "no cargo", f.hoursIn(8))))
	cancel.CommandID = "cancel-consol-2"
	if r, err := f.module.CancelConsolidation(ctx, cancel); err != nil || r.NewRevision != c.Revision+1 {
		t.Fatalf("a consolidation whose only stuffing was given up is cancelled: %+v, %v", r, err)
	}
	if got := f.consolidation(t, "C-S"); got.State != "CANCELLED" {
		t.Fatalf("consolidation = %+v", got)
	}

	// a reopen is dated not before the cancellation of the stuffing it pauses
	g := stuffingDay(t)
	g.begin(t, "X-1", 1)
	ok(t, 3)(g.module.CancelLoadExecution(ctx, g.cancelCmd("cancel-1", "X-1", 2, "no cargo", g.hoursIn(8))))
	early := api.ReopenConsolidation{CommandID: "reopen-before", TenantID: g.tenant, ActorID: "ops-1", ConsolidationID: "C-S", ExpectedRevision: 4,
		Reason: "late house", OccurredAt: g.hoursIn(7)}
	if _, err := g.module.ReopenConsolidation(ctx, early); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a reopen dated before the cancellation: %v", err)
	}
	var pgErr *pgconn.PgError
	err := g.within(t, func(ctx context.Context, e bentopg.Executor) error {
		_, err := e.Exec(ctx, `INSERT INTO cargo_phy.consolidation_version (tenant_id,consolidation_id,version_no,change,state,origin_code,
            destination_code,location_edition,planned_departure,house_cut_off,reason,written_by,written_at,entered_at)
            SELECT tenant_id,consolidation_id,version_no+1,'REOPENED','OPEN',origin_code,destination_code,location_edition,planned_departure,
              house_cut_off,'x','x',$2,now() FROM cargo_phy.consolidation_version WHERE tenant_id=$1 AND consolidation_id='C-S'
            ORDER BY version_no DESC LIMIT 1`, g.tenant, g.hoursIn(7))
		return err
	})
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "consolidation_reopen_after_stuffing" {
		t.Fatalf("a reopen dated before the cancellation in the database: %v", err)
	}
	early.CommandID, early.OccurredAt = "reopen-after", g.hoursIn(9)
	ok(t, 5)(g.module.ReopenConsolidation(ctx, early))
}

// TestACancelRacesALoadIntoTheSameStuffing: a load into a stuffing and its cancellation wait for each other on the
// execution's lock, so exactly one wins and a cancelled stuffing never holds a live pack. Each round races them on a
// fresh stuffing; the one that loses is refused. The same race is run on the database alone below.
func TestACancelRacesALoadIntoTheSameStuffing(t *testing.T) {
	ctx := context.Background()
	f := stuffingDay(t)
	for round := range 8 {
		x := fmt.Sprint("X-R", round)
		unit := 1 + round%2
		f.begin2(t, x, unit, round)
		at := f.hoursIn(5)
		var wg sync.WaitGroup
		var loadErr, cancelErr error
		wg.Add(2)
		start := make(chan struct{})
		go func() {
			defer wg.Done()
			<-start
			_, loadErr = f.module.RecordCargoLoaded(ctx, f.load(fmt.Sprint("race-load-", round), x, 2, "L-S1", 1, at))
		}()
		go func() {
			defer wg.Done()
			<-start
			_, cancelErr = f.module.CancelLoadExecution(ctx, f.cancelCmd(fmt.Sprint("race-cancel-", round), x, 2, "raced", at))
		}()
		close(start)
		wg.Wait()
		for _, err := range []error{loadErr, cancelErr} {
			if err != nil && !errors.Is(err, api.ErrRejected) && !errors.Is(err, api.ErrConflict) {
				t.Fatalf("round %d: unexpected %v", round, err)
			}
		}
		got := f.execution(t, x)
		switch {
		case loadErr == nil && cancelErr != nil:
			if got.State != api.ExecutionLoading || got.TenderedPacks != 1 {
				t.Fatalf("round %d: the load won, the stuffing is %+v", round, got)
			}
			// take the pack out again and give the stuffing up, to free the unit for the next round
			ok(t, 4)(f.module.RecordCargoUnloaded(ctx, api.RecordCargoUnloaded{CommandID: fmt.Sprint("race-unload-", round), TenantID: f.tenant, ActorID: "ops-1",
				ExecutionID: x, ExpectedRevision: 3, LineNo: 1, Packs: 1, Reason: api.ReversalMisloaded, Evidence: evidence(fmt.Sprint("race-unload-", round), at), OccurredAt: at}))
			ok(t, 5)(f.module.CancelLoadExecution(ctx, f.cancelCmd(fmt.Sprint("race-cancel-after-", round), x, 4, "raced", at)))
		case loadErr != nil && cancelErr == nil:
			if got.State != api.ExecutionCancelled || got.TenderedPacks != 0 || len(got.Lines) != 0 {
				t.Fatalf("round %d: the cancellation won, the stuffing is %+v", round, got)
			}
		default:
			t.Fatalf("round %d: load %v, cancel %v: exactly one must win", round, loadErr, cancelErr)
		}
		if lot, err := f.module.GetCargoLot(ctx, api.GetCargoLot{TenantID: f.tenant, LotID: "L-S1"}); err != nil || lot.LoadedPacks != 0 {
			t.Fatalf("round %d: the lot has %d live packs: %v", round, lot.LoadedPacks, err)
		}
	}
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		var bad int
		if err := e.QueryRow(ctx, `SELECT count(*) FROM cargo_phy.load_execution x WHERE x.tenant_id=$1 AND x.state='CANCELLED'
            AND cargo_phy.live_packs_of_lot(x.tenant_id, 'L-S1') <> 0`, f.tenant).Scan(&bad); err != nil {
			return err
		}
		if bad != 0 {
			return fmt.Errorf("%d cancelled stuffings beside live packs", bad)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// begin2 opens and starts the stuffing of round r on the unit; a unit still held by an earlier round's stuffing was
// freed by its cancellation.
func (f *fixture) begin2(t *testing.T, x string, unit, round int) {
	t.Helper()
	ctx, at := context.Background(), f.hoursIn(5)
	ok(t, 1)(f.module.OpenLoadExecution(ctx, f.stuff(fmt.Sprint("open-r-", round), x, unit, at)))
	ok(t, 2)(f.module.StartLoading(ctx, api.StartLoading{CommandID: fmt.Sprint("start-r-", round), TenantID: f.tenant, ActorID: "ops-1", ExecutionID: x,
		ExpectedRevision: 1, OccurredAt: at}))
}

// TestTheDatabaseAloneKeepsALoadAndACancellationApart runs the same race below PHY's revision check: a cancellation and
// the insert of a load line into the same stuffing, in two transactions, one holding the execution's lock while the
// other waits. Whichever goes first, the other is refused when the first commits.
func TestTheDatabaseAloneKeepsALoadAndACancellationApart(t *testing.T) {
	ctx := context.Background()
	for _, cancelFirst := range []bool{true, false} {
		f := stuffingDay(t)
		f.begin(t, "X-1", 1)
		const cancel = `UPDATE cargo_phy.load_execution SET state='CANCELLED', cancelled_by='x', cancelled_at=now(), cancelled_entered_at=now(), cancel_reason='r'
            WHERE tenant_id=$1 AND execution_id='X-1'`
		const load = `INSERT INTO cargo_phy.load_line (tenant_id,execution_id,line_no,consolidation_id,shipment_id,lot_id,entry_no,packs,gross_weight_unit,gross_weight,
            basis,measurement_no,evidence_source,evidence_reference,evidence_occurred_at,loaded_by,loaded_at,entered_at)
            VALUES ($1,'X-1',1,'C-S','SH-S1','L-S1',1,1,'KGM',50,'APPORTIONED',1,'x','x',$2,'x',$2,now())`
		first, second := load, cancel
		if cancelFirst {
			first, second = cancel, load
		}
		holding, release := make(chan struct{}), make(chan struct{})
		var firstErr, secondErr error
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			firstErr = f.within(t, func(ctx context.Context, e bentopg.Executor) error {
				args := []any{f.tenant}
				if first == load {
					args = append(args, f.hoursIn(5))
				}
				if _, err := e.Exec(ctx, first, args...); err != nil {
					close(holding)
					return err
				}
				close(holding)
				<-release
				return nil
			})
		}()
		<-holding
		done := make(chan struct{})
		go func() {
			defer close(done)
			secondErr = f.within(t, func(ctx context.Context, e bentopg.Executor) error {
				args := []any{f.tenant}
				if second == load {
					args = append(args, f.hoursIn(5))
				}
				_, err := e.Exec(ctx, second, args...)
				return err
			})
		}()
		select {
		case <-done:
			t.Fatalf("cancelFirst=%v: the second write did not wait for the execution's lock: %v", cancelFirst, secondErr)
		case <-time.After(300 * time.Millisecond):
		}
		close(release)
		wg.Wait()
		<-done
		var pgErr *pgconn.PgError
		if firstErr != nil || !errors.As(secondErr, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("cancelFirst=%v: first %v, second %v; the second must be refused when the first commits", cancelFirst, firstErr, secondErr)
		}
		wantState, wantPacks := api.ExecutionLoading, int64(1)
		if cancelFirst {
			wantState, wantPacks = api.ExecutionCancelled, 0
		}
		if x := f.execution(t, "X-1"); string(x.State) != wantState || x.TenderedPacks != wantPacks {
			t.Fatalf("cancelFirst=%v: the stuffing is %+v", cancelFirst, x)
		}
	}
	_ = ctx
}

// TestACancelRacesTheOpeningOfASecondStuffing: the first stuffing of a unit is given up while another is opened on the
// same unit. The cancellation always succeeds; the opening is refused (the first stuffing was not cancelled yet when it
// looked) or, once the cancellation committed, accepted; a unit never has two stuffings that are not cancelled.
func TestACancelRacesTheOpeningOfASecondStuffing(t *testing.T) {
	ctx := context.Background()
	f := stuffingDay(t)
	f.begin(t, "X-1", 1)
	at := f.hoursIn(5)
	var wg sync.WaitGroup
	var openErr, cancelErr error
	start := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, openErr = f.module.OpenLoadExecution(ctx, f.stuff("race-open", "X-2", 1, at))
	}()
	go func() {
		defer wg.Done()
		<-start
		_, cancelErr = f.module.CancelLoadExecution(ctx, f.cancelCmd("race-cancel", "X-1", 2, "raced", at))
	}()
	close(start)
	wg.Wait()
	if cancelErr != nil {
		t.Fatalf("the cancellation: %v", cancelErr)
	}
	if openErr != nil && !(errors.Is(openErr, api.ErrRejected) && strings.Contains(openErr.Error(), "PHY_LOAD_UNIT_IN_USE")) {
		t.Fatalf("the opening: %v", openErr)
	}
	var live int
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT count(*) FROM cargo_phy.load_execution WHERE tenant_id=$1 AND consolidation_id='C-S' AND load_unit_no=1 AND state<>'CANCELLED'`,
			f.tenant).Scan(&live)
	}); err != nil {
		t.Fatal(err)
	}
	u := f.unit(t, 1)
	switch {
	case openErr == nil && live == 1 && u.Status == api.UnitStatusInUse && u.ExecutionID == "X-2":
	case openErr != nil && live == 0 && u.Status == api.UnitStatusPlanned && u.ExecutionID == "":
	default:
		t.Fatalf("opening %v; %d stuffings of unit 1 are not cancelled; unit 1 = %+v", openErr, live, u)
	}
	if x := f.execution(t, "X-1"); x.State != api.ExecutionCancelled {
		t.Fatalf("X-1 = %+v", x)
	}
}

// TestAConsolidationIsReconciledOverACancelledStuffing: a stuffing left open keeps a consolidation from being
// reconciled, a stuffing given up does not.
func TestAConsolidationIsReconciledOverACancelledStuffing(t *testing.T) {
	ctx := context.Background()
	f := stuffingDay(t)
	at := f.hoursIn(4)
	f.begin(t, "X-1", 1)
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load-1a", "X-1", 2, "L-S1", 6, at)))
	ok(t, 4)(f.module.RecordCargoLoaded(ctx, f.load("load-1b", "X-1", 3, "L-S2", 6, at)))
	ok(t, 5)(f.module.CloseLoadExecution(ctx, f.sealOf("X-1", 4, "SEAL-1")))
	f.begin(t, "X-2", 2)
	reconcile := api.ReconcileConsolidation{CommandID: "reconcile-1", TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: "C-S", ExpectedRevision: 4,
		OccurredAt: f.hoursIn(6)}
	if _, err := f.module.ReconcileConsolidation(ctx, reconcile); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a consolidation with a stuffing left open is not reconciled: %v", err)
	}
	ok(t, 3)(f.module.CancelLoadExecution(ctx, f.cancelCmd("cancel-2", "X-2", 2, "nothing for it", f.hoursIn(5))))
	reconcile.CommandID = "reconcile-2"
	ok(t, 5)(f.module.ReconcileConsolidation(ctx, reconcile))
}

// TestTheOrderOfTimesAroundACancellation: a new stuffing of a unit, and the unit's removal, are dated not before the
// latest cancellation of a stuffing of the unit, through the commands and, past PHY as the application role, in the
// database (the stuffing's opening rule `load_execution_stuffing`, the removal's `planned_load_unit_cancelled_time`).
func TestTheOrderOfTimesAroundACancellation(t *testing.T) {
	ctx := context.Background()
	f := stuffingDay(t)
	f.begin(t, "X-1", 1)
	ok(t, 3)(f.module.CancelLoadExecution(ctx, f.cancelCmd("cancel-1", "X-1", 2, "rolled", f.hoursIn(6))))
	if _, err := f.module.OpenLoadExecution(ctx, f.stuff("open-early", "X-2", 1, f.hoursIn(5))); !errors.Is(err, api.ErrRejected) ||
		!strings.Contains(err.Error(), "cancellation") {
		t.Fatalf("a new stuffing dated before the cancellation: %v", err)
	}
	remove := api.RemovePlannedLoadUnit{CommandID: "remove-early", TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: "C-S", ExpectedRevision: 4,
		UnitNo: 1, Reason: "rolled", OccurredAt: f.hoursIn(5)}
	if _, err := f.module.RemovePlannedLoadUnit(ctx, remove); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "cancellation") {
		t.Fatalf("a removal dated before the cancellation: %v", err)
	}
	var pgErr *pgconn.PgError
	write := func(sql string, args ...any) error {
		return f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			_, err := e.Exec(ctx, sql, append([]any{f.tenant}, args...)...)
			return err
		})
	}
	err := write(`INSERT INTO cargo_phy.load_execution (tenant_id,execution_id,consolidation_id,kind,transport_mode,facility_location,facility_code,load_unit_no,
        state,opened_by,opened_at,opened_entered_at) VALUES ($1,'X-D1','C-S','STUFFING','SEA','CNSHG','CFS-1',1,'OPEN','x',$2,now())`, f.hoursIn(5))
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "load_execution_stuffing" {
		t.Fatalf("a stuffing dated before the cancellation in the database: %v", err)
	}
	err = write(`UPDATE cargo_phy.planned_load_unit SET status='REMOVED', remove_reason='x', removed_by='x', removed_at=$2, remove_entered_at=now()
        WHERE tenant_id=$1 AND consolidation_id='C-S' AND unit_no=1`, f.hoursIn(5))
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "planned_load_unit_cancelled_time" {
		t.Fatalf("a removal dated before the cancellation in the database: %v", err)
	}
	// dated at the cancellation, both are accepted; a later cancellation of the unit moves the bar
	remove.CommandID, remove.OccurredAt = "remove-1", f.hoursIn(6)
	ok(t, 5)(f.module.RemovePlannedLoadUnit(ctx, remove))
	g := stuffingDay(t)
	g.begin(t, "X-1", 1)
	ok(t, 3)(g.module.CancelLoadExecution(ctx, g.cancelCmd("cancel-1", "X-1", 2, "rolled", g.hoursIn(6))))
	ok(t, 1)(g.module.OpenLoadExecution(ctx, g.stuff("open-ok", "X-2", 1, g.hoursIn(6))))
	ok(t, 2)(g.module.StartLoading(ctx, api.StartLoading{CommandID: "start-2", TenantID: g.tenant, ActorID: "ops-1", ExecutionID: "X-2", ExpectedRevision: 1,
		OccurredAt: g.hoursIn(6)}))
	ok(t, 3)(g.module.CancelLoadExecution(ctx, g.cancelCmd("cancel-2", "X-2", 2, "rolled again", g.hoursIn(8))))
	if _, err = g.module.OpenLoadExecution(ctx, g.stuff("open-7", "X-3", 1, g.hoursIn(7))); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("dated before the latest of two cancellations: %v", err)
	}
	ok(t, 1)(g.module.OpenLoadExecution(ctx, g.stuff("open-8", "X-3", 1, g.hoursIn(8))))
}

// The kept and the added rules about the start of a stuffing, past PHY: a stuffing that is not cancelled is OPEN
// exactly
// when it has not started, and a cancellation is dated not before the start.
func TestTheStartedRulesOfAStuffingOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := stuffingDay(t)
	ok(t, 1)(f.module.OpenLoadExecution(ctx, f.stuff("open-1", "X-1", 1, f.hoursIn(4))))
	ok(t, 1)(f.module.OpenLoadExecution(ctx, f.stuff("open-2", "X-2", 2, f.hoursIn(4))))
	ok(t, 2)(f.module.StartLoading(ctx, api.StartLoading{CommandID: "start-2", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-2", ExpectedRevision: 1,
		OccurredAt: f.hoursIn(5)}))
	var pgErr *pgconn.PgError
	write := func(sql string) error {
		return f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			_, err := e.Exec(ctx, sql, f.tenant)
			return err
		})
	}
	for what, sql := range map[string]string{
		"an OPEN stuffing with a start":  `UPDATE cargo_phy.load_execution SET started_by='x', started_at=now(), started_entered_at=now() WHERE tenant_id=$1 AND execution_id='X-1'`,
		"a LOADING stuffing without one": `UPDATE cargo_phy.load_execution SET started_by=NULL, started_at=NULL, started_entered_at=NULL WHERE tenant_id=$1 AND execution_id='X-2'`,
		"a cancellation dated between the opening and the start": `UPDATE cargo_phy.load_execution SET state='CANCELLED', cancelled_by='x', cancelled_at=opened_at + interval '1 minute',
            cancelled_entered_at=now(), cancel_reason='r' WHERE tenant_id=$1 AND execution_id='X-2'`,
	} {
		err := write(sql)
		want := "load_execution_started"
		if strings.Contains(what, "cancellation") {
			want = "load_execution_cancelled"
		}
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != want {
			t.Errorf("%s: %v, want %s", what, err, want)
		}
	}
}

// The share lock of the hold-release trigger, deterministically: a cancellation and the release of a hold of the same
// stuffing, in each order, one holding the execution row until the other has been tried. The release behind an
// uncommitted cancellation waits for it and is refused (hold_entry_cancelled): a hold is not released on a stuffing
// that is already cancelled. The cancellation behind an uncommitted release waits for it and then finds the hold
// released and the
// stuffing still held (the domain would have resumed it; the database does not resume it, nor cancel it back).
func TestAHoldReleaseAndACancellationWaitForEachOther(t *testing.T) {
	const cancel = `UPDATE cargo_phy.load_execution SET state='CANCELLED', cancelled_by='x', cancelled_at=now(), cancelled_entered_at=now(), cancel_reason='r'
        WHERE tenant_id=$1 AND execution_id='X-1'`
	const release = `UPDATE cargo_phy.hold_entry SET release_evidence_source='x', release_evidence_reference='x', release_evidence_occurred_at=$2, released_by='x',
        released_at=$2, release_entered_at=now() WHERE tenant_id=$1 AND execution_id='X-1' AND hold_no=1`
	for _, cancelFirst := range []bool{true, false} {
		ctx := context.Background()
		f := stuffingDay(t)
		f.begin(t, "X-1", 1)
		ok(t, 3)(f.module.PlaceLoadExecutionOnHold(ctx, api.PlaceLoadExecutionOnHold{CommandID: "hold", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-1",
			ExpectedRevision: 2, Reason: api.HoldCustoms, Evidence: evidence("hold", f.hoursIn(4)), OccurredAt: f.hoursIn(4)}))
		first, second := release, cancel
		if cancelFirst {
			first, second = cancel, release
		}
		args := func(sql string) []any {
			if sql == release {
				return []any{f.tenant, f.hoursIn(5)}
			}
			return []any{f.tenant}
		}
		holding, finish := make(chan struct{}), make(chan struct{})
		var firstErr, secondErr error
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			firstErr = f.within(t, func(ctx context.Context, e bentopg.Executor) error {
				_, err := e.Exec(ctx, first, args(first)...)
				close(holding)
				if err != nil {
					return err
				}
				<-finish
				return nil
			})
		}()
		<-holding
		done := make(chan struct{})
		go func() {
			defer close(done)
			secondErr = f.within(t, func(ctx context.Context, e bentopg.Executor) error {
				_, err := e.Exec(ctx, second, args(second)...)
				return err
			})
		}()
		select {
		case <-done:
			close(finish)
			wg.Wait()
			t.Fatalf("cancelFirst=%v: the second write did not wait for the first: %v", cancelFirst, secondErr)
		case <-time.After(300 * time.Millisecond):
		}
		close(finish)
		wg.Wait()
		<-done
		var pgErr *pgconn.PgError
		x := f.execution(t, "X-1")
		if firstErr != nil {
			t.Fatalf("cancelFirst=%v: the first write: %v", cancelFirst, firstErr)
		}
		if cancelFirst {
			if !errors.As(secondErr, &pgErr) || pgErr.ConstraintName != "hold_entry_cancelled" || x.State != api.ExecutionCancelled || x.Holds[0].Released {
				t.Fatalf("release behind a cancellation: %v; %+v", secondErr, x)
			}
		} else if secondErr != nil || x.State != api.ExecutionCancelled || !x.Holds[0].Released {
			t.Fatalf("cancellation behind a release: %v; %+v", secondErr, x)
		}
	}
}

// The earlier rule "a consolidation with tendered cargo does not reopen", which migration 000006 carries in the
// function it
// replaces, past PHY: an AIR consolidation with a closed tender holding live packs refuses a REOPENED version.
func TestAConsolidationWithTenderedCargoDoesNotReopenInTheDatabase(t *testing.T) {
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
	f.tender(t, "X-1", "C-1", f.hoursIn(4))
	load := f.load("load", "X-1", 2, "L-1", 6, f.hoursIn(4))
	load.GrossWeight = kgm(300)
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, load))
	ok(t, 4)(f.module.CloseLoadExecution(ctx, api.CloseLoadExecution{CommandID: "receipt", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-1",
		ExpectedRevision: 3, Evidence: api.Evidence{Source: "PVG-T1", Reference: "RCPT-1", OccurredAt: f.hoursIn(5)}, OccurredAt: f.hoursIn(5)}))
	err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		_, err := e.Exec(ctx, `INSERT INTO cargo_phy.consolidation_version SELECT (jsonb_populate_record(NULL::cargo_phy.consolidation_version,
            to_jsonb(v) || jsonb_build_object('version_no', v.version_no + 1, 'change', 'REOPENED', 'state', 'OPEN', 'reason', 'x', 'written_at', $2::timestamptz))).*
            FROM cargo_phy.consolidation_version v WHERE v.tenant_id=$1 AND v.consolidation_id='C-1' ORDER BY v.version_no DESC LIMIT 1`, f.tenant, f.hoursIn(6))
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "consolidation_reopen_before_tender" {
		t.Fatalf("a reopen with tendered cargo in the database: %v", err)
	}
}
