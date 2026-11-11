//go:build integration

// Package integration runs FUL against PostgreSQL 16 with a stand-in for COM's query side. The real COM
// and FUL together are tested by the composition root.
package integration

import (
	"context"
	"errors"
	"fmt"
	refapi "go.idp.xyz/idp-cargo/internal/reference/api"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	fulmodule "go.idp.xyz/idp-cargo/internal/contexts/ful"
	"go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	scope "go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

func testDB(t *testing.T) *bentopg.DB {
	t.Helper()
	dsn := os.Getenv("IDP_CARGO_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("IDP_CARGO_POSTGRES_DSN required in CI; skipping forbidden")
		}
		t.Skip("set IDP_CARGO_POSTGRES_DSN to a PostgreSQL 16 application role")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var major int
	var bypass, super bool
	if err = pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int / 10000, rolbypassrls, rolsuper
        FROM pg_roles WHERE rolname = current_user`).Scan(&major, &bypass, &super); err != nil {
		t.Fatal(err)
	}
	if major != 16 || bypass || super {
		t.Fatalf("need PostgreSQL 16 and an application role without BYPASSRLS or superuser (major %d, bypass %v, super %v)", major, bypass, super)
	}
	db, err := bentopg.NewDB(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.CheckSchema(ctx); err != nil {
		t.Fatal(err)
	}
	return db
}

type holderKey struct{}

// comStub answers COM's ObligationSnapshot v3 for obligations of customer P-1: OB-1 to OB-4 hold 2
// pallets of 100 kg each, OB-Q belongs to another customer, and OB-P* hold 10 pallets of 500 kg and
// 1 000 dm³ for parts (F1). The holding shipment is SH-1, or the one the test puts in the context, which
// lets several shipments each believe they hold an obligation, as COM allows since F1.
type comStub struct {
	mu      sync.Mutex
	revoked map[string]bool
}

func (c *comStub) GetObligationSnapshot(context.Context, comapi.GetObligationSnapshot) (comapi.ObligationSnapshot, error) {
	return comapi.ObligationSnapshot{}, errors.New("FUL reads ObligationSnapshot v3 only")
}

func (c *comStub) GetObligationSnapshotV2(context.Context, comapi.GetObligationSnapshot) (comapi.ObligationSnapshotV2, error) {
	return comapi.ObligationSnapshotV2{}, errors.New("FUL reads ObligationSnapshot v3 only")
}

func (c *comStub) GetObligationSnapshotV3(ctx context.Context, q comapi.GetObligationSnapshot) (comapi.ObligationSnapshotV3, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if q.ObligationID == "OB-404" {
		return comapi.ObligationSnapshotV3{}, fmt.Errorf("%w: stub", comapi.ErrNotFound)
	}
	holder, _ := ctx.Value(holderKey{}).(string)
	if holder == "" {
		holder = "SH-1"
	}
	s := comapi.ObligationSnapshotV3{TenantID: q.TenantID, ObligationID: q.ObligationID, ObligationVersion: q.Version, LatestVersion: 1,
		Status: comapi.ObligationActive, OrderID: "O-1", OrderLineNo: 1, Description: "Pallets", Packs: 2, PackageType: "PX",
		GrossWeight: kernel.Quantity{Unit: "KGM", Amount: 100}, LegalEntityID: "LE", Customer: comapi.CustomerRef{PartyID: "P-1", PartyRevision: 2},
		QuoteID: "Q-1", QuoteRevision: 3, AllocatedAt: time.Now().UTC(), HeldByShipmentIDs: []string{holder}}
	if strings.HasPrefix(q.ObligationID, "OB-P") {
		s.Packs, s.GrossWeight, s.Volume = 10, kernel.Quantity{Unit: "KGM", Amount: 500}, kernel.Quantity{Unit: "DMQ", Amount: 1000}
	}
	if q.ObligationID == "OB-Q" {
		s.Customer.PartyID = "P-2"
	}
	if c.revoked[q.ObligationID] {
		s.Status, s.LatestVersion, s.HeldByShipmentIDs = comapi.ObligationRevoked, 2, nil
	}
	return s, nil
}

// shipmentPolicy forbids the actor "intruder".
type shipmentPolicy struct{}

func (shipmentPolicy) AuthorizeShipmentAction(_ context.Context, _, actorID, _, _ string) error {
	if actorID == "intruder" {
		return errors.New("test policy: forbidden")
	}
	return nil
}

type fixture struct {
	module *fulmodule.Module
	db     *bentopg.DB
	com    *comStub
	tenant string
	at     time.Time
}

// locationStub stands in for the location reference data: every well-formed UN/LOCODE resolves to itself in a
// test edition, with its last three letters as the IATA airport code for AIR. The real data is exercised by
// the bootstrap tests.
type locationStub struct{}

func (locationStub) ResolveLocation(_ context.Context, q refapi.ResolveLocation) (refapi.Location, error) {
	if q.Scheme != refapi.SchemeUNLOCODE || len(q.Code) != 5 {
		return refapi.Location{}, refapi.ErrUnknownLocation
	}
	loc := refapi.Location{UNLocode: q.Code, CountryCode: q.Code[:2], Functions: "1234----", Edition: "TEST-EDITION", Via: refapi.ViaUNLOCODE}
	if q.Mode == refapi.ModeAir {
		loc.IATAAirport = q.Code[2:]
	}
	return loc, nil
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond) // business times lie in the past (T1)
	f := &fixture{db: testDB(t), com: &comStub{revoked: map[string]bool{}}, at: at,
		tenant: "cargo-ful-" + time.Now().UTC().Format("20060102T150405.000000000")}
	module, err := fulmodule.New(f.db, f.com, shipmentPolicy{}, fulmodule.WithLocations(locationStub{}))
	if err != nil {
		t.Fatal(err)
	}
	f.module = module
	return f
}

func unlocode(code string) kernel.LocationCode {
	return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code}
}

func (f *fixture) create(shipment, command, obligation string) api.CreateShipmentFromObligation {
	return api.CreateShipmentFromObligation{CommandID: command, TenantID: f.tenant, ActorID: "planner-1", ShipmentID: shipment,
		ObligationID: obligation, ObligationVersion: 1, OccurredAt: f.at}
}

func (f *fixture) add(command string, revision int64, obligation string) api.AddObligationToShipment {
	return api.AddObligationToShipment{CommandID: command, TenantID: f.tenant, ActorID: "planner-1", ShipmentID: "SH-1",
		ExpectedRevision: revision, ObligationID: obligation, ObligationVersion: 1, OccurredAt: f.at.Add(time.Hour)}
}

func (f *fixture) amend(command string, revision int64, departureDays int, reason string) api.AmendShipmentPlan {
	from := f.at.Add(time.Duration(departureDays) * 24 * time.Hour)
	return api.AmendShipmentPlan{CommandID: command, TenantID: f.tenant, ActorID: "planner-1", ShipmentID: "SH-1", ExpectedRevision: revision,
		TransportMode: api.ModeSea, Origin: unlocode("CNSHA"), Destination: unlocode("DEHAM"),
		DepartureWindow: kernel.ValidityInterval{From: from, Until: from.Add(48 * time.Hour)}, Reason: reason, OccurredAt: f.at.Add(2 * time.Hour)}
}

func (f *fixture) confirm(command string, revision int64) api.ConfirmShipmentPlan {
	return api.ConfirmShipmentPlan{CommandID: command, TenantID: f.tenant, ActorID: "planner-2", ShipmentID: "SH-1", ExpectedRevision: revision,
		OccurredAt: f.at.Add(3 * time.Hour)}
}

func (f *fixture) snapshot(t *testing.T, shipment string) api.ShipmentPlanSnapshot {
	t.Helper()
	s, err := f.module.GetShipmentPlanSnapshot(context.Background(), api.GetShipmentPlanSnapshot{TenantID: f.tenant, ShipmentID: shipment})
	if err != nil {
		t.Fatalf("snapshot of %s: %v", shipment, err)
	}
	return s
}

// within runs fn in a tenant-bound transaction on the application role.
func (f *fixture) within(t *testing.T, fn func(context.Context, bentopg.Executor) error) error {
	t.Helper()
	return (scope.TenantTransactor{DB: f.db, Tenant: f.tenant}).WithinTransaction(context.Background(), func(txCtx context.Context) error {
		e, err := f.db.RequireExecutor(txCtx)
		if err != nil {
			return err
		}
		return fn(txCtx, e)
	})
}

func (f *fixture) outboxTypes(t *testing.T) []string {
	t.Helper()
	q, err := f.db.ReadExecutor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rows, err := q.Query(context.Background(), `SELECT event_type FROM bento.outbox WHERE scope=$1 AND source=$2 ORDER BY enqueued_seq`, f.tenant, api.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var types []string
	for rows.Next() {
		var kind string
		if err = rows.Scan(&kind); err != nil {
			t.Fatal(err)
		}
		types = append(types, kind)
	}
	return types
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func TestShipmentLifecycleOnPostgres(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	create := f.create("SH-1", "create-1", "OB-1")
	r, err := f.module.CreateShipmentFromObligation(ctx, create)
	if err != nil || r.NewRevision != 1 || r.ShipmentID != "SH-1" {
		t.Fatalf("create: %+v, %v", r, err)
	}
	if again, err := f.module.CreateShipmentFromObligation(ctx, create); err != nil || again != r {
		t.Fatalf("replay: %+v, %v", again, err)
	}
	if r, err = f.module.AddObligationToShipment(ctx, f.add("add-2", 1, "OB-2")); err != nil || r.NewRevision != 2 {
		t.Fatalf("add OB-2: %+v, %v", r, err)
	}
	if _, err = f.module.ConfirmShipmentPlan(ctx, f.confirm("confirm-0", 2)); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("confirm without a route: err = %v", err)
	}
	if r, err = f.module.AmendShipmentPlan(ctx, f.amend("amend-1", 2, 3, "carrier schedule received")); err != nil || r.NewRevision != 3 {
		t.Fatalf("amend: %+v, %v", r, err)
	}
	if r, err = f.module.ConfirmShipmentPlan(ctx, f.confirm("confirm-1", 3)); err != nil || r.NewRevision != 4 {
		t.Fatalf("confirm: %+v, %v", r, err)
	}
	s := f.snapshot(t, "SH-1")
	want := api.ShipmentPlanSnapshot{TenantID: f.tenant, ShipmentID: "SH-1", Revision: 4, Status: api.StatusPlanned, PlanVersion: 3,
		ConfirmedVersion: 3, CustomerPartyID: "P-1", LegalEntityID: "LE", TransportMode: api.ModeSea, Origin: unlocode("CNSHA"),
		Destination: unlocode("DEHAM"), DepartureWindow: s.DepartureWindow, Packs: 4, GrossWeight: kernel.Quantity{Unit: "KGM", Amount: 200},
		Obligations: []api.ObligationRef{{ObligationID: "OB-1", ObligationVersion: 1, OrderID: "O-1"}, {ObligationID: "OB-2", ObligationVersion: 1, OrderID: "O-1"}},
		PlannedAt:   s.PlannedAt}
	if !reflect.DeepEqual(s, want) || !s.DepartureWindow.From.Equal(f.at.Add(72*time.Hour)) || !s.PlannedAt.Equal(f.at.Add(2*time.Hour)) {
		t.Fatalf("snapshot %+v", s)
	}
	// CostDriver v1 (kind SHIPMENT_PLANNED) for FIN: one order, its accepted quote as COM named it at planning
	if d, err := f.module.GetShipmentCostDriver(ctx, api.GetShipmentCostDriver{TenantID: f.tenant, ShipmentID: "SH-1"}); err != nil ||
		d.Kind != api.DriverShipmentPlanned || d.Status != api.StatusPlanned || d.PlanVersion != 3 || d.LegalEntityID != "LE" || d.CustomerPartyID != "P-1" ||
		d.TransportMode != api.ModeSea || d.Packs != 4 || !reflect.DeepEqual(d.Orders, []api.CostOrder{{OrderID: "O-1", QuoteID: "Q-1", QuoteRevision: 3, Packs: 4}}) {
		t.Fatalf("cost driver %+v %v", d, err)
	}
	release := api.ReleaseObligationFromShipment{CommandID: "release-1", TenantID: f.tenant, ActorID: "planner-1", ShipmentID: "SH-1",
		ExpectedRevision: 4, ObligationID: "OB-1", ObligationVersion: 1, OccurredAt: f.at.Add(4 * time.Hour)}
	if r, err = f.module.ReleaseObligationFromShipment(ctx, release); err != nil || r.NewRevision != 5 {
		t.Fatalf("release: %+v, %v", r, err)
	}
	if s = f.snapshot(t, "SH-1"); s.Status != api.StatusDraft || s.PlanVersion != 4 || s.ConfirmedVersion != 3 || s.Packs != 2 || len(s.Obligations) != 1 {
		t.Fatalf("after the release: %+v", s)
	}
	// The released obligation may join another shipment once COM holds it for that one.
	other := context.WithValue(ctx, holderKey{}, "SH-2")
	if _, err = f.module.CreateShipmentFromObligation(other, f.create("SH-2", "create-2", "OB-1")); err != nil {
		t.Fatalf("OB-1 in SH-2 after SH-1 released it: %v", err)
	}
	cancel := api.CancelShipment{CommandID: "cancel-1", TenantID: f.tenant, ActorID: "planner-1", ShipmentID: "SH-1", ExpectedRevision: 5,
		Reason: "customer withdrew the cargo", OccurredAt: f.at.Add(5 * time.Hour)}
	if r, err = f.module.CancelShipment(ctx, cancel); err != nil || r.NewRevision != 6 {
		t.Fatalf("cancel: %+v, %v", r, err)
	}
	if s = f.snapshot(t, "SH-1"); s.Status != api.StatusCancelled || len(s.Obligations) != 0 || s.Packs != 0 || s.PlanVersion != 4 {
		t.Fatalf("cancelled snapshot %+v", s)
	}
	if d, err := f.module.GetShipmentCostDriver(ctx, api.GetShipmentCostDriver{TenantID: f.tenant, ShipmentID: "SH-1"}); err != nil ||
		d.Status != api.StatusCancelled || len(d.Orders) != 0 || d.Packs != 0 || d.CancelledAt.IsZero() {
		t.Fatalf("cancelled cost driver %+v %v", d, err)
	}
	if _, err = f.module.GetShipmentCostDriver(ctx, api.GetShipmentCostDriver{TenantID: f.tenant, ShipmentID: "SH-404"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("an unknown shipment: %v", err)
	}
	wantEvents := []string{api.ShipmentCreated, api.ShipmentObligationAdded, api.ShipmentPlanAmended, api.ShipmentPlanConfirmed,
		api.ShipmentObligationReleased, api.ShipmentCreated, api.ShipmentCancelled}
	if got := f.outboxTypes(t); !reflect.DeepEqual(got, wantEvents) {
		t.Fatalf("outbox events %v, want %v", got, wantEvents)
	}
	var plans, confirmations, entries, live int
	if err = f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT
            (SELECT count(*) FROM cargo_ful.shipment_plan_version WHERE tenant_id=$1 AND shipment_id='SH-1'),
            (SELECT count(*) FROM cargo_ful.shipment_plan_confirmation WHERE tenant_id=$1 AND shipment_id='SH-1'),
            (SELECT count(*) FROM cargo_ful.shipment_obligation WHERE tenant_id=$1 AND shipment_id='SH-1'),
            (SELECT count(*) FROM cargo_ful.shipment_obligation WHERE tenant_id=$1 AND released_at IS NULL)`, f.tenant).Scan(&plans, &confirmations, &entries, &live)
	}); err != nil {
		t.Fatal(err)
	}
	if plans != 4 || confirmations != 1 || entries != 2 || live != 1 {
		t.Fatalf("stored: %d plan versions, %d confirmations, %d entries of SH-1, %d live entries", plans, confirmations, entries, live)
	}
}

func TestShipmentRefusals(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.module.CreateShipmentFromObligation(ctx, f.create("SH-1", "create-1", "OB-1")); err != nil {
		t.Fatal(err)
	}
	f.com.revoked["OB-3"] = true
	intruder := f.create("SH-9", "create-9", "OB-2")
	intruder.ActorID = "intruder"
	for name, tc := range map[string]struct {
		run  func() error
		want error
	}{
		"an obligation COM does not know": {func() error {
			_, err := f.module.CreateShipmentFromObligation(ctx, f.create("SH-2", "create-2", "OB-404"))
			return err
		}, api.ErrRejected},
		"an obligation reserved for another shipment": {func() error {
			_, err := f.module.CreateShipmentFromObligation(ctx, f.create("SH-3", "create-3", "OB-2"))
			return err
		}, api.ErrRejected},
		"a revoked obligation": {func() error { _, err := f.module.AddObligationToShipment(ctx, f.add("add-3", 1, "OB-3")); return err }, api.ErrRejected},
		"another customer's obligation": {func() error {
			_, err := f.module.AddObligationToShipment(ctx, f.add("add-q", 1, "OB-Q"))
			return err
		}, api.ErrRejected},
		"the same shipment id again": {func() error {
			_, err := f.module.CreateShipmentFromObligation(ctx, f.create("SH-1", "create-again", "OB-2"))
			return err
		}, api.ErrConflict},
		"a stale revision": {func() error { _, err := f.module.AmendShipmentPlan(ctx, f.amend("amend-0", 7, 3, "x")); return err }, api.ErrConflict},
		"an unknown shipment": {func() error {
			_, err := f.module.GetShipmentPlanSnapshot(ctx, api.GetShipmentPlanSnapshot{TenantID: f.tenant, ShipmentID: "SH-404"})
			return err
		}, api.ErrNotFound},
		"another tenant's shipment": {func() error {
			_, err := f.module.GetShipmentPlanSnapshot(ctx, api.GetShipmentPlanSnapshot{TenantID: f.tenant + "-other", ShipmentID: "SH-1"})
			return err
		}, api.ErrNotFound},
		"an actor the authorizer refuses": {func() error { _, err := f.module.CreateShipmentFromObligation(ctx, intruder); return err }, api.ErrUnauthorized},
	} {
		if err := tc.run(); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

func TestShipmentRulesInPostgres(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	steps := []func() (api.Receipt, error){
		func() (api.Receipt, error) {
			return f.module.CreateShipmentFromObligation(ctx, f.create("SH-1", "create-1", "OB-1"))
		},
		func() (api.Receipt, error) { return f.module.AddObligationToShipment(ctx, f.add("add-2", 1, "OB-2")) },
		func() (api.Receipt, error) {
			return f.module.AmendShipmentPlan(ctx, f.amend("amend-1", 2, 3, "schedule"))
		},
		func() (api.Receipt, error) { return f.module.ConfirmShipmentPlan(ctx, f.confirm("confirm-1", 3)) },
	}
	for _, step := range steps {
		if _, err := step(); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ sql, state, why string }{
		{`UPDATE cargo_ful.shipment SET customer_party_id='P-2' WHERE tenant_id=$1`, "23001", "a shipment's customer never changes"},
		{`UPDATE cargo_ful.shipment SET status='CANCELLED' WHERE tenant_id=$1`, "23514", "a cancellation records who, when and why"},
		{`DELETE FROM cargo_ful.shipment WHERE tenant_id=$1`, "42501", "the application role cannot delete shipments"},
		{`UPDATE cargo_ful.shipment_plan_version SET reason='rewritten' WHERE tenant_id=$1`, "42501", "plan versions are immutable"},
		{`DELETE FROM cargo_ful.shipment_plan_confirmation WHERE tenant_id=$1`, "42501", "confirmations are kept"},
		{`UPDATE cargo_ful.shipment_obligation SET packs=1 WHERE tenant_id=$1`, "42501", "only the release columns are writable"},
		{`INSERT INTO cargo_ful.shipment_obligation (tenant_id,shipment_id,entry_no,obligation_id,obligation_version,order_id,customer_party_id,
          legal_entity_id,packs,package_type,gross_weight_unit,gross_weight,part_packs,part_gross_weight,added_by,added_at)
          VALUES ($1,'SH-1',3,'OB-Q',1,'O-9','P-2','LE',1,'PX','KGM',1,1,1,'rogue',now())`, "23514", "one customer per shipment"},
		{`INSERT INTO cargo_ful.shipment_obligation (tenant_id,shipment_id,entry_no,obligation_id,obligation_version,order_id,customer_party_id,
          legal_entity_id,packs,package_type,gross_weight_unit,gross_weight,part_packs,part_gross_weight,added_by,added_at)
          VALUES ($1,'SH-1',3,'OB-1',1,'O-1','P-1','LE',2,'PX','KGM',100,1,1,'rogue',now())`, "23514", "all of OB-1 is planned already"},
		{`INSERT INTO cargo_ful.shipment_obligation (tenant_id,shipment_id,entry_no,obligation_id,obligation_version,order_id,customer_party_id,
          legal_entity_id,packs,package_type,gross_weight_unit,gross_weight,part_packs,part_gross_weight,added_by,added_at)
          VALUES ($1,'SH-1',3,'OB-3',1,'O-1','P-1','LE',2,'PX','KGM',100,3,100,'rogue',now())`, "23514", "a part is never more than its obligation"},
		{`INSERT INTO cargo_ful.shipment_plan_confirmation (tenant_id,shipment_id,plan_no,confirmed_by,confirmed_at)
          VALUES ($1,'SH-1',2,'rogue',now())`, "23514", "only the latest plan version is confirmed"},
		{`INSERT INTO cargo_ful.shipment_plan_version (tenant_id,shipment_id,plan_no,change,scope_packs,scope_weight_unit,scope_weight,written_by,written_at)
          VALUES ($1,'SH-1',4,'AMENDED',4,'KGM',200,'rogue',now())`, "23514", "an amendment has a reason"},
		{`INSERT INTO cargo_ful.shipment_plan_version (tenant_id,shipment_id,plan_no,change,scope_packs,scope_weight_unit,scope_weight,written_by,written_at)
          VALUES ($1,'SH-1',4,'PART_CHANGED',4,'KGM',200,'rogue',now())`, "23514", "a changed part has a reason"},
	} {
		err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			_, err := e.Exec(ctx, tc.sql, f.tenant)
			return err
		})
		if sqlState(err) != tc.state {
			t.Errorf("%s: err = %v, want SQLSTATE %s (%s)", tc.sql, err, tc.state, tc.why)
		}
	}
	if s := f.snapshot(t, "SH-1"); s.Status != api.StatusPlanned || s.Packs != 4 || s.PlanVersion != 3 {
		t.Fatalf("snapshot after the attempts: %+v", s)
	}
	cancel := api.CancelShipment{CommandID: "cancel-1", TenantID: f.tenant, ActorID: "planner-1", ShipmentID: "SH-1", ExpectedRevision: 4,
		Reason: "withdrawn", OccurredAt: f.at.Add(5 * time.Hour)}
	if _, err := f.module.CancelShipment(ctx, cancel); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ sql, state, why string }{
		{`UPDATE cargo_ful.shipment SET status='DRAFT', cancelled_by=NULL, cancelled_at=NULL, cancel_reason=NULL WHERE tenant_id=$1`, "23001",
			"a cancelled shipment never changes"},
		{`UPDATE cargo_ful.shipment_obligation SET released_at=released_at + interval '1 hour' WHERE tenant_id=$1`, "23001", "released once"},
		{`INSERT INTO cargo_ful.shipment_obligation (tenant_id,shipment_id,entry_no,obligation_id,obligation_version,order_id,customer_party_id,
          legal_entity_id,packs,package_type,gross_weight_unit,gross_weight,part_packs,part_gross_weight,added_by,added_at)
          VALUES ($1,'SH-1',3,'OB-3',1,'O-1','P-1','LE',1,'PX','KGM',1,1,1,'rogue',now())`, "23514", "a cancelled shipment plans nothing"},
	} {
		err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			_, err := e.Exec(ctx, tc.sql, f.tenant)
			return err
		})
		if sqlState(err) != tc.state {
			t.Errorf("cancelled: %s: err = %v, want SQLSTATE %s (%s)", tc.sql, err, tc.state, tc.why)
		}
	}
}

// TestTwoShipmentsRaceForOneObligation lets eight shipments believe COM holds the same obligation for
// each of them, and each asks for all of it; the obligation's lock and FUL's trigger keep it in one
// shipment only.
func TestTwoShipmentsRaceForOneObligation(t *testing.T) {
	f := newFixture(t)
	const workers = 8
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			shipment := fmt.Sprintf("SH-R%d", i)
			ctx := context.WithValue(context.Background(), holderKey{}, shipment)
			<-start
			_, errs[i] = f.module.CreateShipmentFromObligation(ctx, f.create(shipment, "create-"+shipment, "OB-1"))
		}()
	}
	close(start)
	wg.Wait()
	committed, conflicts := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			committed++
		case errors.Is(err, api.ErrConflict):
			conflicts++
		}
	}
	if committed != 1 || conflicts != workers-1 {
		t.Fatalf("%d committed, %d conflicts; errors %v", committed, conflicts, errs)
	}
	var shipments, live int
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT (SELECT count(*) FROM cargo_ful.shipment WHERE tenant_id=$1),
            (SELECT count(*) FROM cargo_ful.shipment_obligation WHERE tenant_id=$1 AND obligation_id='OB-1' AND released_at IS NULL)`,
			f.tenant).Scan(&shipments, &live)
	}); err != nil {
		t.Fatal(err)
	}
	if shipments != 1 || live != 1 {
		t.Fatalf("%d shipments and %d live entries of OB-1 stored, want 1 and 1", shipments, live)
	}
}

// TestConcurrentAmendmentsAgainstOneRevision races eight amendments of one plan at the same revision:
// exactly one commits, the others conflict and change nothing.
func TestConcurrentAmendmentsAgainstOneRevision(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.module.CreateShipmentFromObligation(ctx, f.create("SH-1", "create-1", "OB-1")); err != nil {
		t.Fatal(err)
	}
	const workers = 8
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = f.module.AmendShipmentPlan(ctx, f.amend(fmt.Sprintf("amend-%d", i), 1, 3+i, fmt.Sprintf("option %d", i)))
		}()
	}
	close(start)
	wg.Wait()
	committed, conflicts := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			committed++
		case errors.Is(err, api.ErrConflict):
			conflicts++
		}
	}
	if s := f.snapshot(t, "SH-1"); committed != 1 || conflicts != workers-1 || s.Revision != 2 || s.PlanVersion != 2 {
		t.Fatalf("%d committed, %d conflicts, snapshot revision %d plan %d; errors %v", committed, conflicts, s.Revision, s.PlanVersion, errs)
	}
}

// as is a context in which COM's stand-in reports the obligation held by shipment.
func as(shipment string) context.Context {
	return context.WithValue(context.Background(), holderKey{}, shipment)
}

func kgm(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }

func dmq(n int64) kernel.Quantity { return kernel.Quantity{Unit: "DMQ", Amount: n} }

// TestPartsOfOneObligationOnPostgres splits one obligation of 10 pallets over shipments (F1): a part, the
// rest by default, a shortload replanned with a reason, and the database rules that back the model.
func TestPartsOfOneObligationOnPostgres(t *testing.T) {
	f := newFixture(t)
	part := func(shipment, command string, packs, weight, volume int64) api.CreateShipmentFromObligation {
		c := f.create(shipment, command, "OB-P1")
		c.Packs, c.GrossWeight, c.Volume = packs, kgm(weight), dmq(volume)
		return c
	}
	if _, err := f.module.CreateShipmentFromObligation(as("SH-A"), part("SH-A", "create-A", 6, 300, 600)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.CreateShipmentFromObligation(as("SH-B"), f.create("SH-B", "create-B", "OB-P1")); err != nil {
		t.Fatalf("the rest by default: %v", err)
	}
	b, err := f.module.GetShipmentPlanSnapshotV2(context.Background(), api.GetShipmentPlanSnapshot{TenantID: f.tenant, ShipmentID: "SH-B"})
	if err != nil || len(b.Parts) != 1 || b.Parts[0].Packs != 4 || b.Parts[0].GrossWeight != kgm(200) || b.Parts[0].Volume != dmq(400) ||
		b.Parts[0].ObligationPacks != 10 || b.Packs != 4 || b.GrossWeight != kgm(200) {
		t.Fatalf("snapshot v2 of SH-B: %+v, %v", b, err)
	}
	if _, err = f.module.CreateShipmentFromObligation(as("SH-C"), f.create("SH-C", "create-C", "OB-P1")); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("nothing left: err = %v", err)
	}
	if _, err = f.module.CreateShipmentFromObligation(as("SH-C"), part("SH-C", "create-C2", 1, 50, 100)); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a part beyond what is left: err = %v", err)
	}
	shortload := api.ChangeShipmentPart{CommandID: "shortload-A", TenantID: f.tenant, ActorID: "planner-1", ShipmentID: "SH-A", ExpectedRevision: 1,
		ObligationID: "OB-P1", ObligationVersion: 1, Packs: 4, GrossWeight: kgm(200), Volume: dmq(400),
		Reason: "MU587 loaded 4 of 6 pallets", OccurredAt: f.at.Add(time.Hour)}
	if r, err := f.module.ChangeShipmentPart(context.Background(), shortload); err != nil || r.NewRevision != 2 {
		t.Fatalf("shortload: %+v, %v", r, err)
	}
	if _, err = f.module.CreateShipmentFromObligation(as("SH-C"), f.create("SH-C", "create-C3", "OB-P1")); err != nil {
		t.Fatalf("the 2 pallets freed by the shortload: %v", err)
	}
	a, err := f.module.GetShipmentPlanSnapshotV2(context.Background(), api.GetShipmentPlanSnapshot{TenantID: f.tenant, ShipmentID: "SH-A"})
	c := f.snapshot(t, "SH-C")
	if err != nil || a.Packs != 4 || a.PlanVersion != 2 || c.Packs != 2 || c.GrossWeight != kgm(100) || c.Volume != dmq(200) {
		t.Fatalf("after the shortload: SH-A %+v (%v), SH-C %+v", a, err, c)
	}
	var history, live, livePacks int64
	if err = f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE released_at IS NULL), coalesce(sum(part_packs) FILTER (WHERE released_at IS NULL), 0)
            FROM cargo_ful.shipment_obligation WHERE tenant_id=$1 AND obligation_id='OB-P1'`, f.tenant).Scan(&history, &live, &livePacks)
	}); err != nil {
		t.Fatal(err)
	}
	if history != 4 || live != 3 || livePacks != 10 {
		t.Fatalf("OB-P1 entries %d, live %d, live packs %d; want 4, 3 and 10", history, live, livePacks)
	}
	got := f.outboxTypes(t)
	if want := []string{api.ShipmentCreated, api.ShipmentCreated, api.ShipmentPartChanged, api.ShipmentCreated}; !reflect.DeepEqual(got, want) {
		t.Fatalf("outbox %v, want %v", got, want)
	}
	insert := `INSERT INTO cargo_ful.shipment_obligation (tenant_id,shipment_id,entry_no,obligation_id,obligation_version,order_id,customer_party_id,
      legal_entity_id,packs,package_type,gross_weight_unit,gross_weight,volume_unit,volume,part_packs,part_gross_weight,part_volume,added_by,added_at)
      VALUES ($1,$2,$3,$4,$5,'O-1','P-1','LE',10,'PX','KGM',500,'DMQ',1000,$6,$7,$8,'rogue',now())`
	d := f.create("SH-D", "create-D", "OB-P2")
	d.Packs, d.GrossWeight, d.Volume = 4, kgm(200), dmq(400)
	if _, err = f.module.CreateShipmentFromObligation(as("SH-D"), d); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		shipment, obligation string
		entry                int
		version, packs       int64
		weight, volume       int64
		state, why           string
	}{
		{"SH-C", "OB-P1", 2, 1, 1, 1, 1, "23514", "the live parts of OB-P1 already make up all of it"},
		{"SH-A", "OB-P3", 2, 1, 11, 50, 100, "23514", "a part is never more than its obligation"},
		{"SH-D", "OB-P2", 2, 1, 1, 50, 100, "23505", "a shipment plans one live part of an obligation"},
		{"SH-A", "OB-P2", 3, 2, 1, 50, 100, "23514", "the live parts of an obligation are of one version"},
	} {
		err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			_, err := e.Exec(ctx, insert, f.tenant, tc.shipment, tc.entry, tc.obligation, tc.version, tc.packs, tc.weight, tc.volume)
			return err
		})
		if sqlState(err) != tc.state {
			t.Errorf("%s/%s: err = %v, want SQLSTATE %s (%s)", tc.shipment, tc.obligation, err, tc.state, tc.why)
		}
	}
}

// TestPartsRaceForOneObligation lets eight shipments each ask for 3 of the 10 pallets of one obligation at
// once: the obligation's lock serializes them, so exactly three fit and the live parts never exceed it.
func TestPartsRaceForOneObligation(t *testing.T) {
	f := newFixture(t)
	const workers = 8
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			shipment := fmt.Sprintf("SH-P%d", i)
			c := f.create(shipment, "create-"+shipment, "OB-P9")
			c.Packs, c.GrossWeight, c.Volume = 3, kgm(150), dmq(300)
			<-start
			_, errs[i] = f.module.CreateShipmentFromObligation(as(shipment), c)
		}()
	}
	close(start)
	wg.Wait()
	committed, rejected := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			committed++
		case errors.Is(err, api.ErrRejected):
			rejected++
		}
	}
	if committed != 3 || rejected != workers-3 {
		t.Fatalf("%d committed, %d rejected; errors %v", committed, rejected, errs)
	}
	var packs, weight, volume int64
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT sum(part_packs), sum(part_gross_weight), sum(part_volume) FROM cargo_ful.shipment_obligation
            WHERE tenant_id=$1 AND obligation_id='OB-P9' AND released_at IS NULL`, f.tenant).Scan(&packs, &weight, &volume)
	}); err != nil {
		t.Fatal(err)
	}
	if packs != 9 || weight != 450 || volume != 900 {
		t.Fatalf("live parts of OB-P9: %d packs, %d kg, %d dm³; want 9, 450 and 900", packs, weight, volume)
	}
}
