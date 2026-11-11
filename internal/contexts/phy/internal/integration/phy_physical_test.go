//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

var (
	warehouse = api.Facility{Location: unlocode("CNPVG"), Code: "WH-1"}
	terminal  = api.Facility{Location: unlocode("CNPVG"), Code: "PVG-T1"}
)

func evidence(ref string, at time.Time) api.Evidence {
	return api.Evidence{Source: "WMS", Reference: ref, OccurredAt: at}
}

func kgm(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }

// ok returns a check that fails the test unless a command committed at the revision wanted.
func ok(t *testing.T, revision int64) func(api.Receipt, error) api.Receipt {
	return func(r api.Receipt, err error) api.Receipt {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if r.NewRevision != revision {
			t.Fatalf("revision %d, want %d: %+v", r.NewRevision, revision, r)
		}
		return r
	}
}

func (f *fixture) receive(command, lot, shipment string, expected, packs int64, condition string, at time.Time) api.ReceiveCargo {
	return api.ReceiveCargo{CommandID: command, TenantID: f.tenant, ActorID: "ops-1", LotID: lot, ShipmentID: shipment, Facility: warehouse,
		ExpectedRevision: expected, Packs: packs, PackageType: "CT", Condition: condition, Evidence: evidence(command, at), OccurredAt: at}
}

func (f *fixture) load(command, execution string, expected int64, lot string, packs int64, at time.Time) api.RecordCargoLoaded {
	return api.RecordCargoLoaded{CommandID: command, TenantID: f.tenant, ActorID: "ops-1", ExecutionID: execution, ExpectedRevision: expected,
		LotID: lot, Packs: packs, Evidence: evidence(command, at), OccurredAt: at}
}

// tender opens and starts a tender of a consolidation at the airline terminal.
func (f *fixture) tender(t *testing.T, execution, consolidation string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	ok(t, 1)(f.module.OpenLoadExecution(ctx, api.OpenLoadExecution{CommandID: "open-" + execution, TenantID: f.tenant, ActorID: "ops-1",
		ExecutionID: execution, ConsolidationID: consolidation, Kind: api.ExecutionTender, Facility: terminal, OccurredAt: at}))
	ok(t, 2)(f.module.StartLoading(ctx, api.StartLoading{CommandID: "start-" + execution, TenantID: f.tenant, ActorID: "ops-1", ExecutionID: execution,
		ExpectedRevision: 1, OccurredAt: at}))
}

// TestCargoLotsAndTenderOnPostgres runs the air consolidation's physical day: two houses received at the
// forwarder's warehouse, weighed, screened and compared with FUL's plan; the consolidation closed and its houses
// tendered to the airline terminal; the airline's departure and offload entered by hand (OD-4 C); the
// consolidation reconciled and the lots closed.
func TestCargoLotsAndTenderOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.create(t, "C-1")
	for i, shipment := range []string{"SH-1", "SH-2"} {
		if _, err := f.module.AttachHouse(ctx, f.attach(fmt.Sprint("attach-", i), "C-1", shipment, 0)); err != nil {
			t.Fatal(err)
		}
		f.ful.allocate(shipment, "C-1")
	}
	ok(t, 2)(f.module.CloseConsolidation(ctx, f.closeCmd("close", "C-1", 1)))

	at := f.at.Add(3 * time.Hour)
	first := ok(t, 1)(f.module.ReceiveCargo(ctx, f.receive("rcv-1", "L-1", "SH-1", 0, 6, api.ConditionGood, at)))
	if replay, err := f.module.ReceiveCargo(ctx, f.receive("rcv-1", "L-1", "SH-1", 0, 6, api.ConditionGood, at)); err != nil || replay != first {
		t.Fatalf("replay: %+v %v, want %+v", replay, err, first)
	}
	if _, err := f.module.ReceiveCargo(ctx, f.receive("rcv-1b", "L-9", "SH-1", 0, 1, api.ConditionGood, at)); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("one lot in force per shipment and facility: %v", err)
	}
	ok(t, 1)(f.module.ReceiveCargo(ctx, f.receive("rcv-2", "L-2", "SH-2", 0, 5, api.ConditionGood, at)))
	if r := ok(t, 2)(f.module.ReceiveCargo(ctx, f.receive("rcv-3", "L-2", "SH-2", 1, 1, api.ConditionDamaged, at))); r.ItemNo != 2 || r.LotID != "L-2" {
		t.Fatalf("the second receipt line: %+v", r)
	}
	measure := func(command, lot string, expected, weight int64) {
		t.Helper()
		ok(t, expected+1)(f.module.RecordCargoMeasurement(ctx, api.RecordCargoMeasurement{CommandID: command, TenantID: f.tenant, ActorID: "ops-1", LotID: lot,
			ExpectedRevision: expected, GrossWeight: kgm(weight), Volume: kernel.Quantity{Unit: "MTQ", Amount: 2}, Method: api.MethodScale,
			PackageGroups: []api.PackageGroup{{Count: 6, PackageType: "CT", LengthMM: 600, WidthMM: 400, HeightMM: 400}},
			Evidence:      evidence(command, at), OccurredAt: at}))
	}
	screen := func(command, lot string, expected int64, outcome string) (api.Receipt, error) {
		return f.module.RecordScreeningResult(ctx, api.RecordScreeningResult{CommandID: command, TenantID: f.tenant, ActorID: "ops-1", LotID: lot,
			ExpectedRevision: expected, Outcome: outcome, Screener: "PVG security station", Method: "XRAY", Reference: "SC-" + command,
			Evidence: api.Evidence{Source: "PVG-SECURITY", Reference: "SC-" + command, OccurredAt: at}, OccurredAt: at})
	}
	measure("measure-1", "L-1", 1, 300)
	measure("measure-2", "L-2", 2, 330)
	r, err := screen("screen-1", "L-1", 2, api.ScreeningPassed)
	ok(t, 3)(r, err)
	r, err = screen("screen-2", "L-2", 3, api.ScreeningPassed)
	ok(t, 4)(r, err)
	complete := func(command, lot string, expected int64) {
		t.Helper()
		ok(t, expected+1)(f.module.CompleteReceipt(ctx, api.CompleteReceipt{CommandID: command, TenantID: f.tenant, ActorID: "ops-1", LotID: lot,
			ExpectedRevision: expected, OccurredAt: at.Add(10 * time.Minute)}))
	}
	complete("complete-1", "L-1", 3)
	complete("complete-2", "L-2", 4)
	open, err := f.module.ListOpenDiscrepancies(ctx, api.ListOpenDiscrepancies{TenantID: f.tenant, ConsolidationID: "C-1"})
	if err != nil || len(open) != 2 || open[0].LotID != "L-2" || open[0].Kind != "DAMAGED" || open[1].Kind != "WEIGHT_VARIANCE" || open[1].Observed != 330 {
		t.Fatalf("one damaged carton and 330 of 300 kg: %+v %v", open, err)
	}
	for i := range 2 {
		ok(t, int64(6+i))(f.module.ResolveCargoDiscrepancy(ctx, api.ResolveCargoDiscrepancy{CommandID: fmt.Sprint("resolve-", i), TenantID: f.tenant, ActorID: "ops-1",
			LotID: "L-2", ExpectedRevision: int64(5 + i), DiscrepancyNo: i + 1, Resolution: api.ResolvedByAccepted, Reason: "customer accepts, repacked",
			OccurredAt: at.Add(20 * time.Minute)}))
	}

	at = f.at.Add(4 * time.Hour)
	f.tender(t, "X-1", "C-1", at)
	if line := ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load-1", "X-1", 2, "L-1", 6, at))); line.ItemNo != 1 || line.ConsolidationID != "C-1" {
		t.Fatalf("line 1: %+v", line)
	}
	measured := f.load("load-2", "X-1", 3, "L-2", 6, at)
	measured.GrossWeight, measured.Volume = kgm(332), kernel.Quantity{Unit: "MTQ", Amount: 2}
	ok(t, 4)(f.module.RecordCargoLoaded(ctx, measured))
	ok(t, 5)(f.module.CloseLoadExecution(ctx, api.CloseLoadExecution{CommandID: "tender-receipt", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-1",
		ExpectedRevision: 4, Evidence: api.Evidence{Source: "PVG-TERMINAL", Reference: "RCV-778812", OccurredAt: at}, OccurredAt: at}))
	if _, err = f.module.ReopenConsolidation(ctx, api.ReopenConsolidation{CommandID: "reopen", TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ExpectedRevision: 2, Reason: "late cargo", OccurredAt: at}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a tendered consolidation does not reopen: %v", err)
	}

	at = f.at.Add(6 * time.Hour)
	carrier := func(command string, expected int64, fact, shipment string, pieces int64, ref string) (api.Receipt, error) {
		return f.module.RecordCarrierLoadEvidence(ctx, api.RecordCarrierLoadEvidence{CommandID: command, TenantID: f.tenant, ActorID: "ops-1",
			ExecutionID: "X-1", ExpectedRevision: expected, Fact: fact, ShipmentID: shipment, Pieces: pieces, Transport: "MU587",
			Evidence: api.Evidence{Source: "MU-CARGO", Reference: ref, OccurredAt: at}, OccurredAt: at})
	}
	r, err = carrier("departed", 5, api.CarrierDeparted, "", 11, "FSU-DEP-1")
	ok(t, 6)(r, err)
	if r, err = carrier("departed-again", 6, api.CarrierDeparted, "", 11, "FSU-DEP-1"); err != nil || r.NewRevision != 6 || r.ItemNo != 1 {
		t.Fatalf("the same message entered again changes nothing: %+v %v", r, err)
	}
	if _, err = carrier("departed-other", 6, api.CarrierDeparted, "", 10, "FSU-DEP-1"); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("the same reference with other pieces: %v", err)
	}
	r, err = carrier("offloaded", 6, api.CarrierOffloaded, "SH-2", 1, "FSU-DIS-1")
	ok(t, 7)(r, err)
	if _, err = f.module.ReconcileConsolidation(ctx, api.ReconcileConsolidation{CommandID: "reconcile-1", TenantID: f.tenant, ActorID: "ops-1",
		ConsolidationID: "C-1", ExpectedRevision: 2, OccurredAt: at}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("the offload is open: %v", err)
	}
	ok(t, 8)(f.module.ResolveLoadingDiscrepancy(ctx, api.ResolveLoadingDiscrepancy{CommandID: "accept-offload", TenantID: f.tenant, ActorID: "ops-1",
		ExecutionID: "X-1", ExpectedRevision: 7, DiscrepancyNo: 1, Resolution: api.ResolvedByAccepted, Reason: "flies on MU589 under the same MAWB",
		OccurredAt: at}))
	ok(t, 3)(f.module.ReconcileConsolidation(ctx, api.ReconcileConsolidation{CommandID: "reconcile-2", TenantID: f.tenant, ActorID: "ops-1",
		ConsolidationID: "C-1", ExpectedRevision: 2, OccurredAt: at}))
	if c := f.consolidation(t, "C-1"); c.State != api.StateReconciled || c.Current.Change != "RECONCILED" {
		t.Fatalf("reconciled: %+v", c.Current)
	}
	for i, lot := range []string{"L-1", "L-2"} {
		ok(t, []int64{5, 8}[i])(f.module.CloseCargoLot(ctx, api.CloseCargoLot{CommandID: "close-" + lot, TenantID: f.tenant, ActorID: "ops-1", LotID: lot,
			ExpectedRevision: []int64{4, 7}[i], OccurredAt: at}))
	}

	x, err := f.module.GetLoadExecution(ctx, api.GetLoadExecution{TenantID: f.tenant, ExecutionID: "X-1"})
	if err != nil || x.State != api.ExecutionClosed || x.TenderedPacks != 12 || len(x.Lines) != 2 || x.Lines[0].Basis != "APPORTIONED" ||
		x.Lines[0].GrossWeight != kgm(300) || x.Lines[1].Basis != "MEASURED" || x.CloseEvidence.Reference != "RCV-778812" || len(x.CarrierEvidence) != 2 ||
		x.Discrepancies[0].Kind != api.LoadOffloaded || x.Discrepancies[0].Resolution != api.ResolvedByAccepted {
		t.Fatalf("execution: %+v %v", x, err)
	}
	lot, err := f.module.GetCargoLot(ctx, api.GetCargoLot{TenantID: f.tenant, LotID: "L-2"})
	if err != nil || lot.State != "CLOSED" || lot.ReceivedPacks != 6 || lot.LoadedPacks != 6 || len(lot.Lines) != 2 || lot.Lines[1].Condition != api.ConditionDamaged ||
		lot.Measurements[0].PackageGroups[0].LengthMM != 600 || lot.Screening != api.ScreeningPassed || lot.Discrepancies[1].Resolution != api.ResolvedByAccepted ||
		lot.Lines[0].EnteredAt.IsZero() || lot.Discrepancies[0].ResolvedAt.IsZero() {
		t.Fatalf("lot: %+v %v", lot, err)
	}
	evidenceOf, err := f.module.GetPhysicalLoadEvidence(ctx, api.GetPhysicalLoadEvidence{TenantID: f.tenant, ShipmentID: "SH-2"})
	if err != nil || evidenceOf.ReceivedPacks != 6 || evidenceOf.LoadedPacks != 6 || len(evidenceOf.Lots) != 1 || evidenceOf.Lots[0].Measurement.Version != 1 ||
		evidenceOf.Lots[0].Screening.Outcome != api.ScreeningPassed || len(evidenceOf.Tenders) != 1 || evidenceOf.Tenders[0].GrossWeight != kgm(332) ||
		len(evidenceOf.Tenders[0].Lines) != 1 || len(evidenceOf.Tenders[0].CarrierEvidence) != 2 || len(evidenceOf.Tenders[0].OpenDiscrepancies) != 0 {
		t.Fatalf("PhysicalLoadEvidence v1: %+v %v", evidenceOf, err)
	}
	none, err := f.module.GetPhysicalLoadEvidence(ctx, api.GetPhysicalLoadEvidence{TenantID: f.tenant, ShipmentID: "SH-7"})
	if err != nil || len(none.Lots) != 0 || len(none.Tenders) != 0 {
		t.Fatalf("a shipment PHY holds nothing of: %+v %v", none, err)
	}
	types := f.outboxTypes(t)
	for _, want := range []string{api.CargoReceived, api.CargoMeasured, api.ScreeningResultRecorded, api.CargoReceiptCompleted, api.CargoDiscrepancyRecorded,
		api.CargoDiscrepancyResolved, api.LoadExecutionOpened, api.LoadingStarted, api.CargoLoaded, api.LoadExecutionClosed,
		api.CarrierLoadEvidenceRecorded, api.LoadingDiscrepancyRecorded, api.LoadingDiscrepancyResolved, api.ConsolidationReconciled, api.CargoLotClosed} {
		if !slices.Contains(types, want) {
			t.Errorf("no %s in the Outbox", want)
		}
	}
	if _, err = f.module.AmendConsolidation(ctx, api.AmendConsolidation{CommandID: "amend-late", TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ExpectedRevision: 3, PlannedDeparture: f.at.Add(80 * time.Hour), HouseCutOff: f.at.Add(48 * time.Hour), Reason: "late", OccurredAt: at}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a reconciled consolidation never changes: %v", err)
	}
}

// TestConservationAndHoldsOnPostgres tenders a house of an OPEN consolidation: PHY refuses more packs than were
// received, a detach and a smaller plan while the house is loaded, holds the tender when the lot fails
// screening and releases it once the lot's packs are out; the database refuses the same breaches at commit.
func TestConservationAndHoldsOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.create(t, "C-1")
	if _, err := f.module.AttachHouse(ctx, f.attach("attach", "C-1", "SH-1", 0)); err != nil {
		t.Fatal(err)
	}
	f.ful.allocate("SH-1", "C-1")
	at := f.at.Add(3 * time.Hour)
	ok(t, 1)(f.module.ReceiveCargo(ctx, f.receive("rcv", "L-1", "SH-1", 0, 4, api.ConditionGood, at)))
	ok(t, 2)(f.module.RecordCargoMeasurement(ctx, api.RecordCargoMeasurement{CommandID: "measure", TenantID: f.tenant, ActorID: "ops-1", LotID: "L-1",
		ExpectedRevision: 1, GrossWeight: kgm(201), Method: api.MethodScale, Evidence: evidence("measure", at), OccurredAt: at}))
	f.tender(t, "X-1", "C-1", at)
	if _, err := f.module.RecordCargoLoaded(ctx, f.load("load-5", "X-1", 2, "L-1", 5, at)); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("5 of 4 received: %v", err)
	}
	ok(t, 3)(f.module.RecordCargoLoaded(ctx, f.load("load-4", "X-1", 2, "L-1", 4, at)))
	if _, err := f.module.DetachHouse(ctx, api.DetachHouse{CommandID: "detach-early", TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ShipmentID: "SH-1", ExpectedMembershipRevision: 1, Reason: api.ReasonShutOut, OccurredAt: at}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a loaded house does not leave: %v", err)
	}
	f.ful.replan("SH-1", 3)
	if _, err := f.module.RefreshHouseIntent(ctx, api.RefreshHouseIntent{CommandID: "refresh-3", TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ShipmentID: "SH-1", ExpectedMembershipRevision: 1, OccurredAt: at}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a plan of 3 with 4 loaded: %v", err)
	}

	// The database refuses the same breaches.
	refused := "42501|23001"
	for name, tc := range map[string]struct {
		sql        string
		state      string
		constraint string
	}{
		"receipt immutable": {`UPDATE cargo_phy.receipt_line SET packs=9 WHERE tenant_id=$1`, refused, ""},
		"line kept":         {`DELETE FROM cargo_phy.load_line WHERE tenant_id=$1`, refused, ""},
		"lot keeps its shipment": {`UPDATE cargo_phy.cargo_lot SET expected_packs=9, state='RECEIVING', shipment_id='SH-9' WHERE tenant_id=$1`,
			"42501|23001", ""},
		"beyond received": {`INSERT INTO cargo_phy.load_line (tenant_id,execution_id,line_no,consolidation_id,shipment_id,lot_id,entry_no,packs,
            gross_weight_unit,gross_weight,basis,evidence_source,evidence_reference,evidence_occurred_at,loaded_by,loaded_at,entered_at)
            VALUES ($1,'X-1',9,'C-1','SH-1','L-1',1,1,'KGM',1,'MEASURED','x','x',now(),'x',now(),now())`, "23514", "load_line_conservation"},
		"leaves loaded": {`UPDATE cargo_phy.membership_entry SET end_kind='DETACHED', end_reason='SHUT_OUT', ended_by='x', ended_at=now(),
            end_entered_at=now() WHERE tenant_id=$1`, "23514", "membership_entry_loaded"},
	} {
		err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			_, err := e.Exec(ctx, tc.sql, f.tenant)
			return err
		})
		var pgErr *pgconn.PgError
		if state := sqlState(err); state == "" || !contains(tc.state, state) || (tc.constraint != "" && (!errors.As(err, &pgErr) || pgErr.ConstraintName != tc.constraint)) {
			t.Errorf("%s: %v (state %s), want %s %s", name, err, sqlState(err), tc.state, tc.constraint)
		}
	}

	failed, err := f.module.RecordScreeningResult(ctx, api.RecordScreeningResult{CommandID: "screen-failed", TenantID: f.tenant, ActorID: "ops-1",
		LotID: "L-1", ExpectedRevision: 2, Outcome: api.ScreeningFailed, Screener: "PVG security station", Method: "XRAY", Reference: "SC-9",
		Evidence: api.Evidence{Source: "PVG-SECURITY", Reference: "SC-9", OccurredAt: at}, OccurredAt: at})
	ok(t, 3)(failed, err)
	x, err := f.module.GetLoadExecution(ctx, api.GetLoadExecution{TenantID: f.tenant, ExecutionID: "X-1"})
	if err != nil || x.State != api.ExecutionOnHold || x.Revision != 4 || len(x.Holds) != 1 || x.Holds[0].Reason != api.HoldScreeningFailed || x.Holds[0].LotID != "L-1" {
		t.Fatalf("a failed screening holds the tender: %+v %v", x, err)
	}
	release := api.ReleaseLoadExecutionHold{CommandID: "release-early", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-1", ExpectedRevision: 4,
		HoldNo: 1, Evidence: evidence("release", at), OccurredAt: at}
	if _, err = f.module.ReleaseLoadExecutionHold(ctx, release); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("not while the failed lot is loaded: %v", err)
	}
	ok(t, 5)(f.module.RecordCargoUnloaded(ctx, api.RecordCargoUnloaded{CommandID: "unload", TenantID: f.tenant, ActorID: "ops-1", ExecutionID: "X-1",
		ExpectedRevision: 4, LineNo: 1, Packs: 4, Reason: api.ReversalOffloadedByForwarder, Evidence: evidence("unload", at), OccurredAt: at}))
	release.CommandID, release.ExpectedRevision = "release", 5
	ok(t, 6)(f.module.ReleaseLoadExecutionHold(ctx, release))
	if x, err = f.module.GetLoadExecution(ctx, api.GetLoadExecution{TenantID: f.tenant, ExecutionID: "X-1"}); err != nil || x.State != api.ExecutionLoading ||
		x.TenderedPacks != 0 || x.Lines[0].LivePacks != 0 || x.Reversals[0].GrossWeight != kgm(201) || !x.Holds[0].Released {
		t.Fatalf("released and loading again: %+v %v", x, err)
	}
	ok(t, 2)(f.module.RefreshHouseIntent(ctx, api.RefreshHouseIntent{CommandID: "refresh-3b", TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ShipmentID: "SH-1", ExpectedMembershipRevision: 1, OccurredAt: at}))
	ok(t, 3)(f.module.DetachHouse(ctx, api.DetachHouse{CommandID: "detach", TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ShipmentID: "SH-1", ExpectedMembershipRevision: 2, Reason: api.ReasonShutOut, OccurredAt: at}))
	if _, err = f.module.CancelConsolidation(ctx, api.CancelConsolidation{CommandID: "cancel", TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ExpectedRevision: 1, Reason: "no cargo", OccurredAt: at}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a tender is in progress: %v", err)
	}
}

func contains(states, state string) bool {
	for i := 0; i+len(state) <= len(states); i++ {
		if states[i:i+len(state)] == state {
			return true
		}
	}
	return false
}

// TestConcurrentLoadsOfOneLot races eight tenders loading two packs each of one lot of six (PB10): the lot's
// lock lets three through and refuses the rest; the lot never holds more live packs than it received.
func TestConcurrentLoadsOfOneLot(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.create(t, "C-1")
	if _, err := f.module.AttachHouse(ctx, f.attach("attach", "C-1", "SH-1", 0)); err != nil {
		t.Fatal(err)
	}
	at := f.at.Add(3 * time.Hour)
	ok(t, 1)(f.module.ReceiveCargo(ctx, f.receive("rcv", "L-1", "SH-1", 0, 6, api.ConditionGood, at)))
	ok(t, 2)(f.module.RecordCargoMeasurement(ctx, api.RecordCargoMeasurement{CommandID: "measure", TenantID: f.tenant, ActorID: "ops-1", LotID: "L-1",
		ExpectedRevision: 1, GrossWeight: kgm(300), Method: api.MethodScale, Evidence: evidence("measure", at), OccurredAt: at}))
	for i := range 8 {
		f.tender(t, fmt.Sprint("X-", i), "C-1", at)
	}
	var wg sync.WaitGroup
	results := make([]error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = f.module.RecordCargoLoaded(ctx, f.load(fmt.Sprint("load-", i), fmt.Sprint("X-", i), 2, "L-1", 2, at))
		}()
	}
	wg.Wait()
	loaded := 0
	for i, err := range results {
		switch {
		case err == nil:
			loaded++
		case errors.Is(err, api.ErrRejected) || errors.Is(err, api.ErrConflict):
		default:
			t.Fatalf("load %d: %v", i, err)
		}
	}
	lot, err := f.module.GetCargoLot(ctx, api.GetCargoLot{TenantID: f.tenant, LotID: "L-1"})
	if err != nil || loaded != 3 || lot.LoadedPacks != 6 {
		t.Fatalf("%d loads of 2 committed, %d packs live of 6: %v", loaded, lot.LoadedPacks, err)
	}
}
