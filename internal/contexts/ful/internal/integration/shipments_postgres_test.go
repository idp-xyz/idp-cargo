//go:build integration

// Package integration runs FUL against PostgreSQL 16 with a stand-in for COM's query side. The real COM
// and FUL together are tested by the composition root.
package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
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

// comStub answers COM's ObligationSnapshot v2 for obligations of customer P-1: OB-1 to OB-4 hold 2
// pallets of 100 kg each and OB-Q belongs to another customer. The holding shipment is SH-1, or the one
// the test puts in the context, which lets two shipments both believe they hold an obligation.
type comStub struct {
	mu      sync.Mutex
	revoked map[string]bool
}

func (c *comStub) GetObligationSnapshot(context.Context, comapi.GetObligationSnapshot) (comapi.ObligationSnapshot, error) {
	return comapi.ObligationSnapshot{}, errors.New("FUL reads ObligationSnapshot v2 only")
}

func (c *comStub) GetObligationSnapshotV2(ctx context.Context, q comapi.GetObligationSnapshot) (comapi.ObligationSnapshotV2, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if q.ObligationID == "OB-404" {
		return comapi.ObligationSnapshotV2{}, fmt.Errorf("%w: stub", comapi.ErrNotFound)
	}
	holder, _ := ctx.Value(holderKey{}).(string)
	if holder == "" {
		holder = "SH-1"
	}
	s := comapi.ObligationSnapshotV2{TenantID: q.TenantID, ObligationID: q.ObligationID, ObligationVersion: q.Version, LatestVersion: 1,
		Status: comapi.ObligationActive, OrderID: "O-1", OrderLineNo: 1, Description: "Pallets", Packs: 2, PackageType: "PX",
		GrossWeight: kernel.Quantity{Unit: "KGM", Amount: 100}, LegalEntityID: "LE", Customer: comapi.CustomerRef{PartyID: "P-1", PartyRevision: 2},
		QuoteID: "Q-1", QuoteRevision: 3, AllocatedAt: time.Now().UTC(), HeldByShipmentID: holder}
	if q.ObligationID == "OB-Q" {
		s.Customer.PartyID = "P-2"
	}
	if c.revoked[q.ObligationID] {
		s.Status, s.LatestVersion, s.HeldByShipmentID = comapi.ObligationRevoked, 2, ""
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

func newFixture(t *testing.T) *fixture {
	t.Helper()
	at := time.Now().UTC().Truncate(time.Microsecond)
	f := &fixture{db: testDB(t), com: &comStub{revoked: map[string]bool{}}, at: at,
		tenant: "cargo-ful-" + time.Now().UTC().Format("20060102T150405.000000000")}
	module, err := fulmodule.New(f.db, f.com, shipmentPolicy{})
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
          legal_entity_id,packs,package_type,gross_weight_unit,gross_weight,added_by,added_at)
          VALUES ($1,'SH-1',3,'OB-Q',1,'O-9','P-2','LE',1,'PX','KGM',1,'rogue',now())`, "23514", "one customer per shipment"},
		{`INSERT INTO cargo_ful.shipment_obligation (tenant_id,shipment_id,entry_no,obligation_id,obligation_version,order_id,customer_party_id,
          legal_entity_id,packs,package_type,gross_weight_unit,gross_weight,added_by,added_at)
          VALUES ($1,'SH-1',3,'OB-1',1,'O-1','P-1','LE',1,'PX','KGM',1,'rogue',now())`, "23505", "OB-1 is planned already"},
		{`INSERT INTO cargo_ful.shipment_plan_confirmation (tenant_id,shipment_id,plan_no,confirmed_by,confirmed_at)
          VALUES ($1,'SH-1',2,'rogue',now())`, "23514", "only the latest plan version is confirmed"},
		{`INSERT INTO cargo_ful.shipment_plan_version (tenant_id,shipment_id,plan_no,change,scope_packs,scope_weight_unit,scope_weight,written_by,written_at)
          VALUES ($1,'SH-1',4,'AMENDED',4,'KGM',200,'rogue',now())`, "23514", "an amendment has a reason"},
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
          legal_entity_id,packs,package_type,gross_weight_unit,gross_weight,added_by,added_at)
          VALUES ($1,'SH-1',3,'OB-3',1,'O-1','P-1','LE',1,'PX','KGM',1,'rogue',now())`, "23514", "a cancelled shipment plans nothing"},
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
// each of them; FUL's own unique index keeps the obligation in one shipment only.
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
