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
	refapi "go.idp.xyz/idp-cargo/internal/reference/api"
)

const equipmentS1 = "EQUIPMENT-2026-11-S1"

// equipmentStub answers the size-types of edition S1 as migrations/ref/000004 loads them, which the database checks
// a planned unit against; "22T0" answers 22G1 with a wrong tare. The real reference data is exercised by the
// bootstrap tests.
type equipmentStub struct{}

func (equipmentStub) ResolveContainerType(_ context.Context, code string) (refapi.ContainerType, error) {
	types := map[string]refapi.ContainerType{
		"22G1": {Code: "22G1", TEUTenths: 10, MaxGrossKg: 30480, TareKg: 2250, CapacityCM3: 33200000, Edition: equipmentS1},
		"42G1": {Code: "42G1", TEUTenths: 20, MaxGrossKg: 30480, TareKg: 3750, CapacityCM3: 67700000, Edition: equipmentS1},
		"45G1": {Code: "45G1", TEUTenths: 20, MaxGrossKg: 30480, TareKg: 3900, CapacityCM3: 76300000, Edition: equipmentS1},
	}
	if code == "22T0" {
		t := types["22G1"]
		t.TareKg = 2000
		return t, nil
	}
	t, ok := types[code]
	if !ok {
		return refapi.ContainerType{}, fmt.Errorf("%w: size-type %s", refapi.ErrUnknownEquipment, code)
	}
	return t, nil
}

// createSea opens a SEA consolidation Shanghai-Hamburg of the kind given.
func (f *fixture) createSea(t *testing.T, id, kind string) {
	t.Helper()
	if _, err := f.module.CreateConsolidation(context.Background(), api.CreateConsolidation{CommandID: "create-" + id, TenantID: f.tenant,
		ActorID: "ops-1", ConsolidationID: id, Mode: api.ModeSea, Kind: kind, OperatingLegalEntityID: "LE", Origin: unlocode("CNSHG"),
		Destination: unlocode("DEHAM"), PlannedDeparture: f.at.Add(72 * time.Hour), HouseCutOff: f.at.Add(48 * time.Hour), OccurredAt: f.at}); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) planUnit(command, consolidation string, expected int64, typeCode string) api.PlanLoadUnit {
	return api.PlanLoadUnit{CommandID: command, TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: consolidation, ExpectedRevision: expected,
		Kind: api.LoadUnitContainer, TypeCode: typeCode, OccurredAt: f.at.Add(time.Hour)}
}

func TestLoadUnitsOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.createSea(t, "C-S", api.KindSeaLCLOwn)
	f.createSea(t, "C-K", api.KindSeaLCLCoload)
	f.create(t, "C-A")

	first, err := f.module.PlanLoadUnit(ctx, f.planUnit("plan-1", "C-S", 1, "22G1"))
	if err != nil || first.ItemNo != 1 || first.NewRevision != 2 || first.ConsolidationID != "C-S" {
		t.Fatalf("plan 22G1 = %+v, %v", first, err)
	}
	if again, err := f.module.PlanLoadUnit(ctx, f.planUnit("plan-1", "C-S", 1, "22G1")); err != nil || again != first {
		t.Fatalf("replay = %+v, %v; want %+v", again, err, first)
	}
	if _, err = f.module.PlanLoadUnit(ctx, f.planUnit("plan-1", "C-S", 1, "42G1")); !errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("command id reused for another type: %v", err)
	}
	second, err := f.module.PlanLoadUnit(ctx, f.planUnit("plan-2", "C-S", 2, "42G1"))
	if err != nil || second.ItemNo != 2 || second.NewRevision != 3 {
		t.Fatalf("plan 42G1 = %+v, %v", second, err)
	}

	for name, tc := range map[string]struct {
		cmd  api.PlanLoadUnit
		want error
	}{
		"stale revision":       {f.planUnit("plan-stale", "C-S", 2, "45G1"), api.ErrConflict},
		"unknown size-type":    {f.planUnit("plan-unknown", "C-S", 3, "99X9"), api.ErrRejected},
		"not a size-type":      {f.planUnit("plan-bad", "C-S", 3, "40HC"), api.ErrRejected},
		"container on co-load": {f.planUnit("plan-coload", "C-K", 1, "22G1"), api.ErrRejected},
		"container on air":     {f.planUnit("plan-air", "C-A", 1, "22G1"), api.ErrRejected},
		"ULD on own LCL":       {func() api.PlanLoadUnit { c := f.planUnit("plan-uld", "C-S", 3, "22G1"); c.Kind = "ULD"; return c }(), api.ErrRejected},
		"intruder": {func() api.PlanLoadUnit {
			c := f.planUnit("plan-intruder", "C-S", 3, "22G1")
			c.ActorID = "intruder"
			return c
		}(), api.ErrUnauthorized},
		"unknown consolidation": {f.planUnit("plan-404", "C-404", 1, "22G1"), api.ErrNotFound},
	} {
		if r, err := f.module.PlanLoadUnit(ctx, tc.cmd); !errors.Is(err, tc.want) {
			t.Errorf("%s: %+v, %v; want %v", name, r, err, tc.want)
		}
	}
	// The database checks the limits against the reference data: a stand-in answering a wrong tare is refused.
	if _, err = f.module.PlanLoadUnit(ctx, f.planUnit("plan-wrong-tare", "C-S", 3, "22T0")); sqlState(err) != "23514" {
		t.Fatalf("wrong limits: %v (state %s)", err, sqlState(err))
	}

	remove := api.RemovePlannedLoadUnit{CommandID: "remove-1", TenantID: f.tenant, ActorID: "ops-2", ConsolidationID: "C-S", ExpectedRevision: 3,
		UnitNo: 1, Reason: "the cargo fits one 40ft", OccurredAt: f.at.Add(2 * time.Hour)}
	removed, err := f.module.RemovePlannedLoadUnit(ctx, remove)
	if err != nil || removed.ItemNo != 1 || removed.NewRevision != 4 {
		t.Fatalf("remove = %+v, %v", removed, err)
	}
	if again, err := f.module.RemovePlannedLoadUnit(ctx, remove); err != nil || again != removed {
		t.Fatalf("replayed removal = %+v, %v", again, err)
	}
	for name, cmd := range map[string]api.RemovePlannedLoadUnit{
		"removed already": {CommandID: "remove-again", TenantID: f.tenant, ActorID: "ops-2", ConsolidationID: "C-S", ExpectedRevision: 4, UnitNo: 1,
			Reason: "again", OccurredAt: f.at.Add(2 * time.Hour)},
		"no reason": {CommandID: "remove-no-reason", TenantID: f.tenant, ActorID: "ops-2", ConsolidationID: "C-S", ExpectedRevision: 4, UnitNo: 2,
			OccurredAt: f.at.Add(2 * time.Hour)},
		"no such unit": {CommandID: "remove-3", TenantID: f.tenant, ActorID: "ops-2", ConsolidationID: "C-S", ExpectedRevision: 4, UnitNo: 3,
			Reason: "x", OccurredAt: f.at.Add(2 * time.Hour)},
	} {
		if _, err := f.module.RemovePlannedLoadUnit(ctx, cmd); !errors.Is(err, api.ErrRejected) {
			t.Errorf("%s: %v", name, err)
		}
	}

	c := f.consolidation(t, "C-S")
	if c.Revision != 4 || len(c.LoadUnits) != 2 || len(c.Versions) != 1 {
		t.Fatalf("consolidation = revision %d, %d versions, units %+v", c.Revision, len(c.Versions), c.LoadUnits)
	}
	u1, u2 := c.LoadUnits[0], c.LoadUnits[1]
	if u1.UnitNo != 1 || u1.TypeCode != "22G1" || u1.Status != api.UnitStatusRemoved || u1.RemoveReason != "the cargo fits one 40ft" ||
		!u1.RemovedAt.Equal(f.at.Add(2*time.Hour)) || u1.TEUTenths != 10 || u1.EquipmentEdition != equipmentS1 {
		t.Fatalf("unit 1 = %+v", u1)
	}
	if u2.UnitNo != 2 || u2.Kind != api.LoadUnitContainer || u2.TypeCode != "42G1" || u2.Status != api.UnitStatusPlanned || u2.TEUTenths != 20 ||
		u2.MaxGrossKg != 30480 || u2.TareKg != 3750 || u2.CapacityCM3 != 67700000 || !u2.PlannedAt.Equal(f.at.Add(time.Hour)) || !u2.RemovedAt.IsZero() {
		t.Fatalf("unit 2 = %+v", u2)
	}
	basis, err := f.module.GetPhysicalCostBasis(ctx, api.GetPhysicalCostBasis{TenantID: f.tenant, ConsolidationID: "C-S"})
	if err != nil || basis.Revision != 4 || len(basis.LoadUnits) != 1 || basis.LoadUnits[0] != u2 {
		t.Fatalf("cost basis = %+v, %v", basis, err)
	}
	if k := f.consolidation(t, "C-K"); len(k.LoadUnits) != 0 {
		t.Fatalf("co-load units = %+v", k.LoadUnits)
	}
	types := f.outboxTypes(t)
	if n := strings.Count(strings.Join(types, " "), api.LoadUnitPlanned); n != 2 || !slices.Contains(types, api.LoadUnitRemoved) {
		t.Fatalf("events = %v", types)
	}

	// A cancelled consolidation keeps its units as they are.
	if _, err = f.module.CancelConsolidation(ctx, api.CancelConsolidation{CommandID: "cancel", TenantID: f.tenant, ActorID: "ops-1",
		ConsolidationID: "C-S", ExpectedRevision: 4, Reason: "vessel omitted the port", OccurredAt: f.at.Add(3 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.module.PlanLoadUnit(ctx, func() api.PlanLoadUnit {
		c := f.planUnit("plan-cancelled", "C-S", 5, "22G1")
		c.OccurredAt = f.at.Add(4 * time.Hour)
		return c
	}()); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("plan on a cancelled consolidation: %v", err)
	}
	if _, err = f.module.RemovePlannedLoadUnit(ctx, api.RemovePlannedLoadUnit{CommandID: "remove-cancelled", TenantID: f.tenant, ActorID: "ops-1",
		ConsolidationID: "C-S", ExpectedRevision: 5, UnitNo: 2, Reason: "x", OccurredAt: f.at.Add(4 * time.Hour)}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("removal on a cancelled consolidation: %v", err)
	}
}

func TestLoadUnitDatabaseRules(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.createSea(t, "C-S", api.KindSeaLCLOwn)
	f.createSea(t, "C-K", api.KindSeaLCLCoload)
	f.createSea(t, "C-X", api.KindSeaLCLOwn)
	if _, err := f.module.PlanLoadUnit(ctx, f.planUnit("plan-1", "C-S", 1, "22G1")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.PlanLoadUnit(ctx, f.planUnit("plan-x", "C-X", 1, "22G1")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.CancelConsolidation(ctx, api.CancelConsolidation{CommandID: "cancel-x", TenantID: f.tenant, ActorID: "ops-1",
		ConsolidationID: "C-X", ExpectedRevision: 2, Reason: "no space", OccurredAt: f.at.Add(2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	insert := func(consolidation string, no int, edition, code string, teu int, tare int64) string {
		return fmt.Sprintf(`INSERT INTO cargo_phy.planned_load_unit (tenant_id,consolidation_id,unit_no,kind,type_code,equipment_edition,teu_tenths,
            max_gross_kg,tare_kg,capacity_cm3,status,planned_by,planned_at,planned_entered_at)
            VALUES ($1,'%s',%d,'CONTAINER','%s','%s',%d,30480,%d,33200000,'PLANNED','x',now(),now())`, consolidation, no, code, edition, teu, tare)
	}
	refused := "42501|23001"
	for name, tc := range map[string]struct {
		sql   string
		state string
	}{
		"limits fixed":            {`UPDATE cargo_phy.planned_load_unit SET tare_kg=1 WHERE tenant_id=$1`, refused},
		"type fixed":              {`UPDATE cargo_phy.planned_load_unit SET type_code='42G1' WHERE tenant_id=$1`, refused},
		"no delete":               {`DELETE FROM cargo_phy.planned_load_unit WHERE tenant_id=$1`, refused},
		"removal needs a reason":  {`UPDATE cargo_phy.planned_load_unit SET status='REMOVED' WHERE tenant_id=$1 AND consolidation_id='C-S'`, "23514"},
		"next unit only":          {insert("C-S", 3, equipmentS1, "22G1", 10, 2250), "23514"},
		"reference limits":        {insert("C-S", 2, equipmentS1, "22G1", 10, 2000), "23514"},
		"reference TEU":           {insert("C-S", 2, equipmentS1, "22G1", 20, 2250), "23514"},
		"unknown edition":         {insert("C-S", 2, "EQUIPMENT-NONE", "22G1", 10, 2250), "23514|23503"}, // the trigger finds no type first
		"no units on a co-load":   {insert("C-K", 1, equipmentS1, "22G1", 10, 2250), "23514"},
		"none on a cancelled one": {insert("C-X", 2, equipmentS1, "22G1", 10, 2250), "23514"},
		"frozen once cancelled": {`UPDATE cargo_phy.planned_load_unit SET status='REMOVED', remove_reason='x', removed_by='x', removed_at=now(),
            remove_entered_at=now() WHERE tenant_id=$1 AND consolidation_id='C-X'`, "23514"},
	} {
		err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			_, err := e.Exec(ctx, tc.sql, f.tenant)
			return err
		})
		if state := sqlState(err); state == "" || !strings.Contains(tc.state, state) {
			t.Errorf("%s: %v (state %s), want %s", name, err, state, tc.state)
		}
	}
	// A removal is written once, and never undone.
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		_, err := e.Exec(ctx, `UPDATE cargo_phy.planned_load_unit SET status='REMOVED', remove_reason='x', removed_by='x', removed_at=now(),
            remove_entered_at=now() WHERE tenant_id=$1 AND consolidation_id='C-S'`, f.tenant)
		if err != nil {
			return err
		}
		_, err = e.Exec(ctx, `UPDATE cargo_phy.planned_load_unit SET status='PLANNED', remove_reason=NULL, removed_by=NULL, removed_at=NULL,
            remove_entered_at=NULL WHERE tenant_id=$1 AND consolidation_id='C-S'`, f.tenant)
		return err
	}); sqlState(err) != "23001" {
		t.Fatalf("undoing a removal: %v (state %s)", err, sqlState(err))
	}
	// Another tenant sees no units.
	other := &fixture{db: f.db, tenant: f.tenant + "-other"}
	if err := other.within(t, func(ctx context.Context, e bentopg.Executor) error {
		var n int
		if err := e.QueryRow(ctx, `SELECT count(*) FROM cargo_phy.planned_load_unit`).Scan(&n); err != nil || n != 0 {
			return fmt.Errorf("%d units visible, %v", n, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
