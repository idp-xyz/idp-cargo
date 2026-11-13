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
)

var cfsShanghai = api.Facility{Location: unlocode("CNSHG"), Code: "CFS-1"}

// stuffingDay builds a CLOSED own-container consolidation C-S with a 22G1 (unit 1) and a 42G1 (unit 2) planned,
// SEA houses SH-S1 and SH-S2 attached, and their lots L-S1 and L-S2 of 6 cartons received at the CFS, weighed at
// 300 kg and completed. The consolidation is at revision 4.
func stuffingDay(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	f := newFixture(t)
	f.ful.sea["SH-S1"], f.ful.sea["SH-S2"] = true, true
	f.createSea(t, "C-S", api.KindSeaLCLOwn)
	ok(t, 2)(f.module.PlanLoadUnit(ctx, f.planUnit("plan-1", "C-S", 1, "22G1")))
	ok(t, 3)(f.module.PlanLoadUnit(ctx, f.planUnit("plan-2", "C-S", 2, "42G1")))
	for i, shipment := range []string{"SH-S1", "SH-S2"} {
		if _, err := f.module.AttachHouse(ctx, f.attach(fmt.Sprint("attach-", i), "C-S", shipment, 0)); err != nil {
			t.Fatal(err)
		}
		f.ful.allocate(shipment, "C-S")
	}
	if _, err := f.module.OpenLoadExecution(ctx, f.stuff("open-early", "X-0", 1, f.at.Add(time.Hour))); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a container is stuffed once the houses are frozen: %v", err)
	}
	close := f.closeCmd("close", "C-S", 3)
	ok(t, 4)(f.module.CloseConsolidation(ctx, close))
	at := f.at.Add(3 * time.Hour)
	for i, lot := range []string{"L-S1", "L-S2"} {
		rcv := f.receive("rcv-"+lot, lot, fmt.Sprint("SH-S", i+1), 0, 6, api.ConditionGood, at)
		rcv.Facility = cfsShanghai
		ok(t, 1)(f.module.ReceiveCargo(ctx, rcv))
		ok(t, 2)(f.module.RecordCargoMeasurement(ctx, api.RecordCargoMeasurement{CommandID: "measure-" + lot, TenantID: f.tenant, ActorID: "ops-1",
			LotID: lot, ExpectedRevision: 1, GrossWeight: kgm(300), Method: api.MethodScale, Evidence: evidence("measure-"+lot, at), OccurredAt: at}))
		ok(t, 3)(f.module.CompleteReceipt(ctx, api.CompleteReceipt{CommandID: "complete-" + lot, TenantID: f.tenant, ActorID: "ops-1", LotID: lot,
			ExpectedRevision: 2, OccurredAt: at}))
	}
	return f
}

func (f *fixture) stuff(command, execution string, unit int, at time.Time) api.OpenLoadExecution {
	return api.OpenLoadExecution{CommandID: command, TenantID: f.tenant, ActorID: "ops-1", ExecutionID: execution, ConsolidationID: "C-S",
		Kind: api.ExecutionStuffing, Facility: cfsShanghai, LoadUnitNo: unit, OccurredAt: at}
}

func (f *fixture) unit(t *testing.T, no int) api.PlannedLoadUnit {
	t.Helper()
	return f.consolidation(t, "C-S").LoadUnits[no-1]
}

// TestStuffingOnPostgres stuffs a 22G1 at the forwarder's CFS: one stuffing per unit, the unit IN_USE and kept, a line
// over the container's default maximum gross recorded and holding the stuffing, the hold released with the plate,
// and the container sealed: the unit CLOSED, the consolidation no longer reopened.
func TestStuffingOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := stuffingDay(t)
	at := f.at.Add(4 * time.Hour)

	opened := ok(t, 1)(f.module.OpenLoadExecution(ctx, f.stuff("open-1", "X-S1", 1, at)))
	if opened.ItemNo != 1 || opened.ExecutionID != "X-S1" {
		t.Fatalf("the stuffing's receipt names its unit: %+v", opened)
	}
	if again, err := f.module.OpenLoadExecution(ctx, f.stuff("open-1", "X-S1", 1, at)); err != nil || again != opened {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	tenderOwn := f.stuff("tender-own", "X-T", 0, at)
	tenderOwn.Kind = api.ExecutionTender
	tenderWithUnit := f.stuff("tender-unit", "X-U", 2, at)
	tenderWithUnit.Kind = api.ExecutionTender
	for name, tc := range map[string]struct {
		cmd  api.OpenLoadExecution
		want error
	}{
		"a second stuffing of unit 1": {f.stuff("open-again", "X-S9", 1, at), api.ErrRejected},
		"no unit":                     {f.stuff("open-none", "X-S8", 0, at), api.ErrRejected},
		"an unknown unit":             {f.stuff("open-unknown", "X-S7", 3, at), api.ErrRejected},
		"a tender of an own LCL":      {tenderOwn, api.ErrRejected},
		"a tender naming a unit":      {tenderWithUnit, api.ErrRejected},
	} {
		if _, err := f.module.OpenLoadExecution(ctx, tc.cmd); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	if _, err := f.module.RemovePlannedLoadUnit(ctx, api.RemovePlannedLoadUnit{CommandID: "remove-1", TenantID: f.tenant, ActorID: "ops-1",
		ConsolidationID: "C-S", ExpectedRevision: 4, UnitNo: 1, Reason: "rolled", OccurredAt: at}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a unit with its stuffing is not removed: %v", err)
	}
	if u := f.unit(t, 1); u.Status != api.UnitStatusInUse || u.ExecutionID != "X-S1" {
		t.Fatalf("unit 1 = %+v", u)
	}
	ok(t, 2)(f.module.StartLoading(ctx, api.StartLoading{CommandID: "start-1", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-S1",
		ExpectedRevision: 1, OccurredAt: at}))
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("stuff-1", "X-S1", 2, "L-S1", 6, at)))
	heavy := f.load("stuff-2", "X-S1", 3, "L-S2", 6, at)
	heavy.GrossWeight = kgm(28000) // with 300 kg loaded and 2,250 kg of tare: 30,550 kg, over 30,480 kg
	ok(t, 4)(f.module.RecordCargoLoaded(ctx, heavy))
	x, err := f.module.GetLoadExecution(ctx, api.GetLoadExecution{TenantID: f.tenant, ExecutionID: "X-S1"})
	if err != nil || x.State != api.ExecutionOnHold || x.Kind != api.ExecutionStuffing || x.LoadUnitNo != 1 || len(x.Holds) != 1 ||
		x.Holds[0].Reason != api.HoldOverweight || !strings.Contains(x.Holds[0].Text, "30550.000 kg over the default maximum gross 30480 kg") ||
		x.TenderedPacks != 12 {
		t.Fatalf("stuffing = %+v, %v", x, err)
	}
	seal := api.CloseLoadExecution{CommandID: "seal", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-S1", ExpectedRevision: 4,
		Evidence: api.Evidence{Source: api.EvidenceSeal, Reference: "CN-SEAL-0042", OccurredAt: at}, OccurredAt: at}
	if _, err = f.module.CloseLoadExecution(ctx, seal); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("no seal while held: %v", err)
	}
	ok(t, 5)(f.module.ReleaseLoadExecutionHold(ctx, api.ReleaseLoadExecutionHold{CommandID: "release", TenantID: f.tenant, ActorID: "ops-1",
		ExecutionID: "X-S1", ExpectedRevision: 4, HoldNo: 1, Evidence: api.Evidence{Source: "CSC_PLATE", Reference: "MSKU1234565", OccurredAt: at},
		OccurredAt: at}))
	receipt := seal
	receipt.CommandID, receipt.ExpectedRevision = "receipt", 5
	receipt.Evidence = api.Evidence{Source: "CFS", Reference: "R-1", OccurredAt: at}
	if _, err = f.module.CloseLoadExecution(ctx, receipt); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a stuffing closes with its seal: %v", err)
	}
	seal.ExpectedRevision = 5
	ok(t, 6)(f.module.CloseLoadExecution(ctx, seal))

	if u := f.unit(t, 1); u.Status != api.UnitStatusClosed || u.ExecutionID != "X-S1" {
		t.Fatalf("unit 1 = %+v", u)
	}
	if u := f.unit(t, 2); u.Status != api.UnitStatusPlanned || u.ExecutionID != "" {
		t.Fatalf("unit 2 = %+v", u)
	}
	basis, err := f.module.GetPhysicalCostBasis(ctx, api.GetPhysicalCostBasis{TenantID: f.tenant, ConsolidationID: "C-S"})
	if err != nil || len(basis.LoadUnits) != 2 || basis.LoadUnits[0].Status != api.UnitStatusClosed || basis.Houses[1].LoadedGrossWeight != kgm(28000) {
		t.Fatalf("cost basis = %+v, %v", basis, err)
	}
	ev, err := f.module.GetPhysicalLoadEvidence(ctx, api.GetPhysicalLoadEvidence{TenantID: f.tenant, ShipmentID: "SH-S2"})
	if err != nil || len(ev.Tenders) != 1 || ev.Tenders[0].Kind != api.ExecutionStuffing || ev.Tenders[0].LoadUnitNo != 1 ||
		ev.Tenders[0].CloseEvidence.Reference != "CN-SEAL-0042" || ev.Tenders[0].LoadedPacks != 6 {
		t.Fatalf("evidence = %+v, %v", ev, err)
	}
	if _, err = f.module.ReopenConsolidation(ctx, api.ReopenConsolidation{CommandID: "reopen", TenantID: f.tenant, ActorID: "ops-1",
		ConsolidationID: "C-S", ExpectedRevision: 4, Reason: "late cargo", OccurredAt: at}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a sealed container's consolidation does not reopen: %v", err)
	}
	types := strings.Join(f.outboxTypes(t), " ")
	for _, want := range []string{api.LoadExecutionOpened, api.CargoLoaded, api.LoadExecutionHeld, api.LoadExecutionHoldReleased, api.LoadExecutionClosed} {
		if !strings.Contains(types, want) {
			t.Errorf("no %s in %s", want, types)
		}
	}
}

// A stuffing's houses stay frozen: once the consolidation is reopened, nothing more is stuffed until it is closed.
func TestStuffingWaitsForTheHousesToBeFrozen(t *testing.T) {
	ctx := context.Background()
	f := stuffingDay(t)
	at := f.at.Add(4 * time.Hour)
	ok(t, 1)(f.module.OpenLoadExecution(ctx, f.stuff("open-1", "X-S1", 1, at)))
	ok(t, 2)(f.module.StartLoading(ctx, api.StartLoading{CommandID: "start-1", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-S1",
		ExpectedRevision: 1, OccurredAt: at}))
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("stuff-1", "X-S1", 2, "L-S1", 3, at)))
	ok(t, 5)(f.module.ReopenConsolidation(ctx, api.ReopenConsolidation{CommandID: "reopen", TenantID: f.tenant, ActorID: "ops-1",
		ConsolidationID: "C-S", ExpectedRevision: 4, Reason: "a late house", OccurredAt: at}))
	if _, err := f.module.RecordCargoLoaded(ctx, f.load("stuff-2", "X-S1", 3, "L-S1", 3, at)); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("stuffed from an open house list: %v", err)
	}
	// The database refuses it too.
	err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		_, err := e.Exec(ctx, `INSERT INTO cargo_phy.load_line (tenant_id,execution_id,line_no,consolidation_id,shipment_id,lot_id,entry_no,packs,
            gross_weight_unit,gross_weight,basis,evidence_source,evidence_reference,evidence_occurred_at,loaded_by,loaded_at,entered_at)
            SELECT tenant_id,execution_id,2,consolidation_id,shipment_id,lot_id,entry_no,1,gross_weight_unit,1,'MEASURED',evidence_source,
              'x',evidence_occurred_at,loaded_by,loaded_at,now() FROM cargo_phy.load_line WHERE tenant_id=$1 AND execution_id='X-S1'`, f.tenant)
		return err
	})
	if sqlState(err) != "23514" || !strings.Contains(err.Error(), "stuffed once it is CLOSED") {
		t.Fatalf("a line into a stuffing of an open consolidation: %v", err)
	}
}

// Eight stuffings of one container opened at once: one is opened, the others refused.
func TestStuffingsRaceForOneUnit(t *testing.T) {
	ctx := context.Background()
	f := stuffingDay(t)
	at := f.at.Add(4 * time.Hour)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = f.module.OpenLoadExecution(ctx, f.stuff(fmt.Sprint("open-", i), fmt.Sprint("X-R", i), 2, at))
		}()
	}
	wg.Wait()
	opened := 0
	for _, err := range errs {
		switch {
		case err == nil:
			opened++
		case errors.Is(err, api.ErrRejected) && strings.Contains(err.Error(), "PHY_LOAD_UNIT_IN_USE"):
		default:
			t.Errorf("unexpected: %v", err)
		}
	}
	if u := f.unit(t, 2); opened != 1 || u.Status != api.UnitStatusInUse || !strings.HasPrefix(u.ExecutionID, "X-R") {
		t.Fatalf("%d opened; unit 2 = %+v", opened, u)
	}
}

func TestStuffingDatabaseRules(t *testing.T) {
	ctx := context.Background()
	f := stuffingDay(t)
	at := f.at.Add(4 * time.Hour)
	f.createSea(t, "C-K", api.KindSeaLCLCoload)
	ok(t, 1)(f.module.OpenLoadExecution(ctx, f.stuff("open-1", "X-S1", 1, at)))
	ok(t, 2)(f.module.StartLoading(ctx, api.StartLoading{CommandID: "start-1", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-S1",
		ExpectedRevision: 1, OccurredAt: at}))
	insert := func(id, consolidation, kind, unit string) string {
		return fmt.Sprintf(`INSERT INTO cargo_phy.load_execution (tenant_id,execution_id,consolidation_id,kind,transport_mode,facility_location,
            facility_code,load_unit_no,state,opened_by,opened_at,opened_entered_at) VALUES ($1,'%s','%s','%s','SEA','CNSHG','CFS-1',%s,'OPEN','x',now(),now())`,
			id, consolidation, kind, unit)
	}
	for name, tc := range map[string]struct {
		sql, state, constraint string
	}{
		"a second stuffing of a unit": {insert("X-D1", "C-S", "STUFFING", "1"), "23505", "load_execution_one_per_unit"},
		"a stuffing without its unit": {insert("X-D2", "C-S", "STUFFING", "NULL"), "23514", "load_execution_stuffing"},
		"a tender naming a unit":      {insert("X-D3", "C-K", "TENDER", "2"), "23514", "load_execution_unit"},
		"a tender of an own LCL":      {insert("X-D6", "C-S", "TENDER", "NULL"), "23514", "load_execution_consolidation"},
		"a stuffing of a co-load":     {insert("X-D4", "C-K", "STUFFING", "1"), "23514", "load_execution_stuffing"},
		"an unknown unit":             {insert("X-D5", "C-S", "STUFFING", "9"), "23514", "load_execution_stuffing"},
		"the unit fixed":              {`UPDATE cargo_phy.load_execution SET load_unit_no=2 WHERE tenant_id=$1 AND execution_id='X-S1'`, "42501", ""},
		"a unit in use removed": {`UPDATE cargo_phy.planned_load_unit SET status='REMOVED', remove_reason='x', removed_by='x', removed_at=now(),
            remove_entered_at=now() WHERE tenant_id=$1 AND consolidation_id='C-S' AND unit_no=1`, "23514", "planned_load_unit_in_use"},
		"sealed without a seal": {`UPDATE cargo_phy.load_execution SET state='CLOSED', closed_by='x', closed_at=now(), closed_entered_at=now(),
            close_evidence_source='CFS', close_evidence_reference='R', close_evidence_occurred_at=now()
            WHERE tenant_id=$1 AND execution_id='X-S1'`, "23514", "load_execution_seal"},
	} {
		err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			_, err := e.Exec(ctx, tc.sql, f.tenant)
			return err
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != tc.state || pgErr.ConstraintName != tc.constraint {
			t.Errorf("%s: %v, want %s %s", name, err, tc.state, tc.constraint)
		}
	}
	// As the migrator, past the grants, the trigger keeps the unit too.
	if migrator := f.migrator(t); migrator != nil {
		if _, err := migrator.Exec(ctx, `UPDATE cargo_phy.load_execution SET load_unit_no=2 WHERE tenant_id=$1 AND execution_id='X-S1'`,
			f.tenant); sqlState(err) != "23001" {
			t.Fatalf("the unit rewritten by the migrator: %v", err)
		}
	}
}

// A stuffing that meets the one-per-unit index, the other committing meanwhile, is refused as the unit's stuffing,
// not as a conflict to retry.
func TestStuffingMeetsTheOnePerUnitIndex(t *testing.T) {
	ctx := context.Background()
	f := stuffingDay(t)
	at := f.at.Add(4 * time.Hour)
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			if _, err := e.Exec(ctx, `INSERT INTO cargo_phy.load_execution (tenant_id,execution_id,consolidation_id,kind,transport_mode,facility_location,
                facility_code,load_unit_no,state,opened_by,opened_at,opened_entered_at)
                VALUES ($1,'X-H','C-S','STUFFING','SEA','CNSHG','CFS-1',2,'OPEN','x',now(),now())`, f.tenant); err != nil {
				return err
			}
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	result := make(chan error, 1)
	go func() {
		_, err := f.module.OpenLoadExecution(ctx, f.stuff("open-late", "X-L", 2, at))
		result <- err
	}()
	time.Sleep(300 * time.Millisecond) // the opening waits on the index
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "PHY_LOAD_UNIT_IN_USE") {
		t.Fatalf("the second stuffing of unit 2: %v", err)
	}
}

// A lot weighed again while it is being stuffed holds the stuffing when its container is then over the limits.
func TestStuffingHeldWhenTheLotIsWeighedAgain(t *testing.T) {
	ctx := context.Background()
	f := stuffingDay(t)
	at := f.at.Add(4 * time.Hour)
	ok(t, 1)(f.module.OpenLoadExecution(ctx, f.stuff("open-1", "X-S1", 1, at)))
	ok(t, 2)(f.module.StartLoading(ctx, api.StartLoading{CommandID: "start-1", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-S1",
		ExpectedRevision: 1, OccurredAt: at}))
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("stuff-1", "X-S1", 2, "L-S1", 6, at)))
	weigh := api.RecordCargoMeasurement{CommandID: "weigh-again", TenantID: f.tenant, ActorID: "cfs-1", LotID: "L-S1", ExpectedRevision: 3,
		GrossWeight: kgm(30000), Method: api.MethodScale, Evidence: evidence("weigh-again", at), OccurredAt: at}
	if r := ok(t, 4)(f.module.RecordCargoMeasurement(ctx, weigh)); r.ItemNo != 2 {
		t.Fatalf("measurement 2: %+v", r)
	}
	x, err := f.module.GetLoadExecution(ctx, api.GetLoadExecution{TenantID: f.tenant, ExecutionID: "X-S1"})
	if err != nil || x.State != api.ExecutionOnHold || len(x.Holds) != 1 || x.Holds[0].Evidence.Reference != "weigh-again" ||
		!strings.Contains(x.Holds[0].Text, "32250.000 kg over the default maximum gross 30480 kg") || x.Revision != 4 {
		t.Fatalf("stuffing after the weighing: %+v %v", x, err)
	}
	types := f.outboxTypes(t)
	if !slices.Contains(types, api.LoadExecutionHeld) || !slices.Contains(types, api.CargoMeasured) {
		t.Fatalf("events = %v", types)
	}
}

// A container is sealed on a frozen house list; a sealed container is not unstuffed and its consolidation does not
// reopen; a reopen is not dated before the stuffing it pauses.
func TestSealingOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := stuffingDay(t)
	at := f.at.Add(4 * time.Hour)
	ok(t, 1)(f.module.OpenLoadExecution(ctx, f.stuff("open-1", "X-S1", 1, at)))
	ok(t, 2)(f.module.StartLoading(ctx, api.StartLoading{CommandID: "start-1", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-S1",
		ExpectedRevision: 1, OccurredAt: at}))
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("stuff-1", "X-S1", 2, "L-S1", 6, at.Add(time.Hour))))
	reopen := api.ReopenConsolidation{CommandID: "reopen-early", TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: "C-S", ExpectedRevision: 4,
		Reason: "a late house", OccurredAt: at.Add(30 * time.Minute)}
	if _, err := f.module.ReopenConsolidation(ctx, reopen); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a reopen dated before the line it pauses: %v", err)
	}
	reopen.CommandID, reopen.OccurredAt = "reopen", at.Add(2*time.Hour)
	ok(t, 5)(f.module.ReopenConsolidation(ctx, reopen))
	seal := api.CloseLoadExecution{CommandID: "seal", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-S1", ExpectedRevision: 3,
		Evidence: api.Evidence{Source: api.EvidenceSeal, Reference: "CN-SEAL-0042", OccurredAt: at.Add(2 * time.Hour)}, OccurredAt: at.Add(2 * time.Hour)}
	if _, err := f.module.CloseLoadExecution(ctx, seal); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "PHY_HOUSES_NOT_FROZEN") {
		t.Fatalf("sealed on an open house list: %v", err)
	}
	// The database refuses it too.
	err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		_, err := e.Exec(ctx, `UPDATE cargo_phy.load_execution SET state='CLOSED', closed_by='x', closed_at=now(), closed_entered_at=now(),
            close_evidence_source='SEAL', close_evidence_reference='S', close_evidence_occurred_at=now() WHERE tenant_id=$1 AND execution_id='X-S1'`, f.tenant)
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "load_execution_seal_frozen" {
		t.Fatalf("sealed on an open house list in the database: %v", err)
	}
	closeAgain := f.closeCmd("close-again", "C-S", 5)
	closeAgain.OccurredAt = f.at.Add(6*time.Hour + 30*time.Minute)
	ok(t, 6)(f.module.CloseConsolidation(ctx, closeAgain))
	seal.OccurredAt, seal.Evidence.OccurredAt = f.at.Add(7*time.Hour), f.at.Add(7*time.Hour)
	ok(t, 4)(f.module.CloseLoadExecution(ctx, seal))

	unstuff := api.RecordCargoUnloaded{CommandID: "unstuff", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-S1", ExpectedRevision: 4,
		LineNo: 1, Packs: 6, Reason: api.ReversalOffloadedByForwarder, Evidence: evidence("unstuff", f.at.Add(7*time.Hour)), OccurredAt: f.at.Add(7 * time.Hour)}
	if _, err = f.module.RecordCargoUnloaded(ctx, unstuff); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a sealed container unstuffed: %v", err)
	}
	reopen.CommandID, reopen.ExpectedRevision, reopen.OccurredAt = "reopen-sealed", 6, f.at.Add(8*time.Hour)
	if _, err = f.module.ReopenConsolidation(ctx, reopen); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "cannot reopen") {
		t.Fatalf("a consolidation with a sealed container reopened: %v", err)
	}
	for name, tc := range map[string]struct{ sql, constraint string }{
		"a reversal of a sealed container": {`INSERT INTO cargo_phy.reversal_line (tenant_id,execution_id,reversal_no,line_no,packs,gross_weight,reason,
            evidence_source,evidence_reference,evidence_occurred_at,reversed_by,reversed_at,entered_at)
            VALUES ($1,'X-S1',1,1,1,1,'OFFLOADED_BY_FORWARDER','x','x',now(),'x',now(),now())`, "reversal_line_sealed"},
	} {
		err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			_, err := e.Exec(ctx, tc.sql, f.tenant)
			return err
		})
		if !errors.As(err, &pgErr) || pgErr.ConstraintName != tc.constraint {
			t.Errorf("%s: %v, want %s", name, err, tc.constraint)
		}
	}

	// Emptied past the triggers (as the migrator), the sealed container still keeps its consolidation from reopening.
	migrator := f.migrator(t)
	if migrator == nil {
		return
	}
	tx, err := migrator.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO cargo_phy.reversal_line (tenant_id,execution_id,reversal_no,line_no,packs,gross_weight,reason,evidence_source,
        evidence_reference,evidence_occurred_at,reversed_by,reversed_at,entered_at)
        SELECT tenant_id,execution_id,1,1,packs,gross_weight,'OFFLOADED_BY_FORWARDER','x','x',loaded_at,'x',loaded_at,now()
        FROM cargo_phy.load_line WHERE tenant_id=$1 AND execution_id='X-S1' AND line_no=1`, f.tenant); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	reopen.CommandID = "reopen-emptied"
	if _, err = f.module.ReopenConsolidation(ctx, reopen); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "1 containers are sealed") {
		t.Fatalf("a consolidation with an emptied sealed container reopened: %v", err)
	}
	err = f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		_, err := e.Exec(ctx, `INSERT INTO cargo_phy.consolidation_version (tenant_id,consolidation_id,version_no,change,state,origin_code,
            destination_code,location_edition,planned_departure,house_cut_off,reason,written_by,written_at,entered_at)
            SELECT tenant_id,consolidation_id,version_no+1,'REOPENED','OPEN',origin_code,destination_code,location_edition,planned_departure,
              house_cut_off,'x','x',now(),now() FROM cargo_phy.consolidation_version WHERE tenant_id=$1 AND consolidation_id='C-S'
            ORDER BY version_no DESC LIMIT 1`, f.tenant)
		return err
	})
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "consolidation_reopen_before_seal" {
		t.Fatalf("a reopen after the seal in the database: %v", err)
	}
}

// The database's opening rules for a stuffing: a CLOSED consolidation, a PLANNED unit, not dated before the close,
// and opened OPEN.
func TestStuffingOpeningRulesInTheDatabase(t *testing.T) {
	ctx := context.Background()
	f := stuffingDay(t)
	at := f.at.Add(4 * time.Hour)
	f.createSea(t, "C-O", api.KindSeaLCLOwn)
	ok(t, 2)(f.module.PlanLoadUnit(ctx, func() api.PlanLoadUnit { c := f.planUnit("plan-o", "C-O", 1, "22G1"); return c }()))
	insert := func(id, consolidation, state, unit, opened string) string {
		return fmt.Sprintf(`INSERT INTO cargo_phy.load_execution (tenant_id,execution_id,consolidation_id,kind,transport_mode,facility_location,
            facility_code,load_unit_no,state,opened_by,opened_at,opened_entered_at,started_by,started_at,started_entered_at)
            VALUES ($1,'%s','%s','STUFFING','SEA','CNSHG','CFS-1',%s,'%s','x',%s,now(),%s)`, id, consolidation, unit, state, opened,
			map[bool]string{true: "'x',now(),now()", false: "NULL,NULL,NULL"}[state != "OPEN"])
	}
	if _, err := f.module.RemovePlannedLoadUnit(ctx, api.RemovePlannedLoadUnit{CommandID: "remove-2", TenantID: f.tenant, ActorID: "ops-1",
		ConsolidationID: "C-S", ExpectedRevision: 4, UnitNo: 2, Reason: "one container is enough", OccurredAt: at}); err != nil {
		t.Fatal(err)
	}
	for name, sql := range map[string]string{
		"an OPEN consolidation":  insert("X-1", "C-O", "OPEN", "1", "now()"),
		"a REMOVED unit":         insert("X-2", "C-S", "OPEN", "2", "now()"),
		"dated before the close": insert("X-3", "C-S", "OPEN", "1", "'"+f.at.Add(time.Hour).Format(time.RFC3339Nano)+"'"),
		"opened already loading": insert("X-4", "C-S", "LOADING", "1", "now()"),
	} {
		err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			_, err := e.Exec(ctx, sql, f.tenant)
			return err
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.ConstraintName != "load_execution_stuffing" {
			t.Errorf("%s: %v", name, err)
		}
	}
}
