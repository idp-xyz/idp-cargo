//go:build integration

// Package integration runs PHY against PostgreSQL 16 with stand-ins for FUL's LoadIntent v1 and the location
// reference data. The real FUL and PHY together are tested by the composition root.
package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	phymodule "go.idp.xyz/idp-cargo/internal/contexts/phy"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	scope "go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/kernel"
	refapi "go.idp.xyz/idp-cargo/internal/reference/api"
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

type closerKey struct{}

// fulStub answers LoadIntent v1: every shipment is PLANNED by AIR from Shanghai Pudong to New York JFK for
// legal entity LE, 6 cartons of 300 kg, departing within days 2 to 5 of the fixture; tests change the plan
// version, the packs (50 kg each), the status and the consolidation FUL recorded. It notes which shipments a
// close asked about.
type fulStub struct {
	mu        sync.Mutex
	at        time.Time
	plan      map[string]int
	packs     map[string]int64
	status    map[string]string
	allocated map[string]string
	asked     map[int][]string // per close attempt
	delay     time.Duration
}

func (f *fulStub) GetLoadIntent(ctx context.Context, q fulapi.GetShipmentPlanSnapshot) (fulapi.LoadIntent, error) {
	attempt, closing := ctx.Value(closerKey{}).(int)
	if closing && f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if q.ShipmentID == "SH-404" {
		return fulapi.LoadIntent{}, fmt.Errorf("%w: stub", fulapi.ErrNotFound)
	}
	if closing {
		f.asked[attempt] = append(f.asked[attempt], q.ShipmentID)
	}
	plan, status := f.plan[q.ShipmentID], f.status[q.ShipmentID]
	if plan == 0 {
		plan = 2
	}
	if status == "" {
		status = fulapi.StatusPlanned
	}
	packs := int64(6)
	if p := f.packs[q.ShipmentID]; p > 0 {
		packs = p
	}
	kg := kernel.Quantity{Unit: "KGM", Amount: 50 * packs}
	return fulapi.LoadIntent{TenantID: q.TenantID, ShipmentID: q.ShipmentID, Revision: int64(plan + 1), Status: status, PlanVersion: plan,
		ConfirmedVersion: plan, CustomerPartyID: "P-1", LegalEntityID: "LE", TransportMode: fulapi.ModeAir, Origin: unlocode("CNPVG"),
		Destination: unlocode("USJFK"), OriginIATA: "PVG", DestinationIATA: "JFK", LocationEdition: "TEST-EDITION",
		DepartureWindow: kernel.ValidityInterval{From: f.at.Add(48 * time.Hour), Until: f.at.Add(120 * time.Hour)}, Packs: packs, GrossWeight: kg,
		Parts: []fulapi.LoadIntentPart{{ObligationID: "OB-" + q.ShipmentID, ObligationVersion: 1, OrderID: "O-1", Packs: packs, PackageType: "CT", GrossWeight: kg}},
		Stage: fulapi.StageMain, ConsolidationID: f.allocated[q.ShipmentID]}, nil
}

// replan changes a shipment's plan as FUL would: a new plan version with the packs given.
func (f *fulStub) replan(shipment string, packs int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.plan[shipment] == 0 {
		f.plan[shipment] = 2
	}
	f.plan[shipment]++
	f.packs[shipment] = packs
}

func (f *fulStub) allocate(shipment, consolidation string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.allocated[shipment] = consolidation
}

// locationStub resolves every well-formed UN/LOCODE to itself in a test edition, with its last three letters
// as the IATA airport code for AIR. The real data is exercised by the bootstrap tests.
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

// policy forbids the actor "intruder".
type policy struct{}

func (policy) AuthorizeConsolidationAction(_ context.Context, _, actorID, _, _ string) error {
	if actorID == "intruder" {
		return errors.New("test policy: forbidden")
	}
	return nil
}

type fixture struct {
	module *phymodule.Module
	db     *bentopg.DB
	ful    *fulStub
	tenant string
	at     time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	at := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Microsecond) // business times lie in the past (T1)
	f := &fixture{db: testDB(t), at: at, tenant: "cargo-phy-" + time.Now().UTC().Format("20060102T150405.000000000"),
		ful: &fulStub{at: at, plan: map[string]int{}, packs: map[string]int64{}, status: map[string]string{}, allocated: map[string]string{},
			asked: map[int][]string{}}}
	module, err := phymodule.New(f.db, f.ful, policy{}, phymodule.WithLocations(locationStub{}))
	if err != nil {
		t.Fatal(err)
	}
	f.module = module
	return f
}

func unlocode(code string) kernel.LocationCode {
	return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code}
}

// create opens an AIR consolidation PVG-JFK departing on day 3 with the house cut-off on day 2.
func (f *fixture) create(t *testing.T, id string) {
	t.Helper()
	if _, err := f.module.CreateConsolidation(context.Background(), api.CreateConsolidation{CommandID: "create-" + id, TenantID: f.tenant,
		ActorID: "ops-1", ConsolidationID: id, Mode: api.ModeAir, Kind: api.KindAirConsol, OperatingLegalEntityID: "LE",
		Origin: unlocode("CNPVG"), Destination: unlocode("USJFK"), PlannedDeparture: f.at.Add(72 * time.Hour),
		HouseCutOff: f.at.Add(48 * time.Hour), OccurredAt: f.at}); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) attach(command, consolidation, shipment string, expected int64) api.AttachHouse {
	return api.AttachHouse{CommandID: command, TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: consolidation, ShipmentID: shipment,
		ExpectedMembershipRevision: expected, OccurredAt: f.at.Add(time.Hour)}
}

func (f *fixture) closeCmd(command, consolidation string, expected int64) api.CloseConsolidation {
	return api.CloseConsolidation{CommandID: command, TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: consolidation,
		ExpectedRevision: expected, OccurredAt: f.at.Add(2 * time.Hour)}
}

func (f *fixture) consolidation(t *testing.T, id string) api.Consolidation {
	t.Helper()
	c, err := f.module.GetConsolidation(context.Background(), api.GetConsolidation{TenantID: f.tenant, ConsolidationID: id})
	if err != nil {
		t.Fatalf("consolidation %s: %v", id, err)
	}
	return c
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

func TestConsolidationAndHousesOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.create(t, "C-1")
	if _, err := f.module.CreateConsolidation(ctx, api.CreateConsolidation{CommandID: "create-C-1", TenantID: f.tenant, ActorID: "ops-1",
		ConsolidationID: "C-1", Mode: api.ModeSea, Kind: api.KindSeaLCLOwn, OperatingLegalEntityID: "LE", Origin: unlocode("CNSHG"),
		Destination: unlocode("DEHAM"), PlannedDeparture: f.at.Add(72 * time.Hour), HouseCutOff: f.at.Add(48 * time.Hour), OccurredAt: f.at}); !errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("a command id reused for another consolidation: %v", err)
	}
	c := f.consolidation(t, "C-1")
	if c.State != api.StateOpen || c.Current.Origin != unlocode("CNPVG") || c.Current.OriginIATA != "PVG" || c.Current.DestinationIATA != "JFK" ||
		c.Current.LocationEdition != "TEST-EDITION" || c.Current.EnteredAt.IsZero() || c.Revision != 1 {
		t.Fatalf("created: %+v", c)
	}
	if _, err := f.module.AttachHouse(ctx, api.AttachHouse{CommandID: "intruder", TenantID: f.tenant, ActorID: "intruder", ConsolidationID: "C-1",
		ShipmentID: "SH-1", OccurredAt: f.at.Add(time.Hour)}); !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("intruder: %v", err)
	}
	if _, err := f.module.AttachHouse(ctx, f.attach("attach-404", "C-1", "SH-404", 0)); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a shipment FUL does not know: %v", err)
	}
	r1, err := f.module.AttachHouse(ctx, f.attach("attach-1", "C-1", "SH-1", 0))
	if err != nil || r1.NewRevision != 1 || r1.EntryNo != 1 || r1.ConsolidationID != "C-1" || r1.ShipmentID != "SH-1" {
		t.Fatalf("attach SH-1: %+v %v", r1, err)
	}
	if again, err := f.module.AttachHouse(ctx, f.attach("attach-1", "C-1", "SH-1", 0)); err != nil || again != r1 {
		t.Fatalf("replay: %+v %v", again, err)
	}
	if _, err = f.module.AttachHouse(ctx, f.attach("attach-2", "C-1", "SH-2", 0)); err != nil {
		t.Fatal(err)
	}
	if _, err = f.module.AttachHouse(ctx, f.attach("attach-1-again", "C-1", "SH-1", 0)); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("attach at a stale membership revision: %v", err)
	}
	if _, err = f.module.AttachHouse(ctx, f.attach("attach-1-twice", "C-1", "SH-1", 1)); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("attach a house twice: %v", err)
	}
	if c = f.consolidation(t, "C-1"); len(c.Members) != 2 || c.Members[0].ShipmentID != "SH-1" || c.Members[0].PlanVersion != 2 || c.Members[0].Packs != 6 {
		t.Fatalf("members: %+v", c.Members)
	}
	open, err := f.module.ListOpenConsolidations(ctx, api.ListOpenConsolidations{TenantID: f.tenant, Mode: api.ModeAir, Destination: unlocode("USJFK")})
	if err != nil || len(open) != 1 || open[0].ConsolidationID != "C-1" || open[0].LiveHouses != 2 {
		t.Fatalf("open consolidations: %+v %v", open, err)
	}

	// Close re-confirms every house with FUL: SH-2 is not yet allocated in FUL, then SH-1's plan changes.
	f.ful.allocate("SH-1", "C-1")
	if _, err = f.module.CloseConsolidation(ctx, f.closeCmd("close-unallocated", "C-1", 1)); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("close with a house FUL has not allocated: %v", err)
	}
	f.ful.allocate("SH-2", "C-1")
	f.ful.mu.Lock()
	f.ful.plan["SH-1"] = 3
	f.ful.mu.Unlock()
	if _, err = f.module.CloseConsolidation(ctx, f.closeCmd("close-stale", "C-1", 1)); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("close with a stale house: %v", err)
	}
	if _, err = f.module.RefreshHouseIntent(ctx, api.RefreshHouseIntent{CommandID: "refresh-1", TenantID: f.tenant, ActorID: "ops-1",
		ConsolidationID: "C-1", ShipmentID: "SH-1", ExpectedMembershipRevision: 1, OccurredAt: f.at.Add(90 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	closed, err := f.module.CloseConsolidation(ctx, f.closeCmd("close", "C-1", 1))
	if err != nil || closed.NewRevision != 2 {
		t.Fatalf("close: %+v %v", closed, err)
	}
	if _, err = f.module.AttachHouse(ctx, f.attach("attach-3", "C-1", "SH-3", 0)); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("attach after the house cut-off: %v", err)
	}
	if _, err = f.module.DetachHouse(ctx, api.DetachHouse{CommandID: "detach-closed", TenantID: f.tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ShipmentID: "SH-2", ExpectedMembershipRevision: 1, Reason: api.ReasonCustomerRequest, OccurredAt: f.at.Add(3 * time.Hour)}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("detach after the house cut-off: %v", err)
	}

	// Rollover: reopen, move SH-2 to a later consolidation, amend the flight of C-1, close again.
	if _, err = f.module.ReopenConsolidation(ctx, api.ReopenConsolidation{CommandID: "reopen", TenantID: f.tenant, ActorID: "ops-1",
		ConsolidationID: "C-1", ExpectedRevision: 2, Reason: "SH-2 shut out by the airline", OccurredAt: f.at.Add(3 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	f.create(t, "C-2")
	moved, err := f.module.TransferHouse(ctx, api.TransferHouse{CommandID: "transfer-2", TenantID: f.tenant, ActorID: "ops-1",
		ConsolidationID: "C-1", ToConsolidationID: "C-2", ShipmentID: "SH-2", ExpectedMembershipRevision: 1, Reason: api.ReasonShutOut,
		OccurredAt: f.at.Add(4 * time.Hour)})
	if err != nil || moved.ConsolidationID != "C-2" || moved.EntryNo != 2 || moved.NewRevision != 2 {
		t.Fatalf("transfer: %+v %v", moved, err)
	}
	if _, err = f.module.AmendConsolidation(ctx, api.AmendConsolidation{CommandID: "amend-route", TenantID: f.tenant, ActorID: "ops-1",
		ConsolidationID: "C-1", ExpectedRevision: 3, Destination: unlocode("USLAX"), Origin: unlocode("CNPVG"),
		PlannedDeparture: f.at.Add(72 * time.Hour), HouseCutOff: f.at.Add(48 * time.Hour), Reason: "other airport", OccurredAt: f.at.Add(5 * time.Hour)}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("route change with houses attached: %v", err)
	}
	if _, err = f.module.AmendConsolidation(ctx, api.AmendConsolidation{CommandID: "amend-flight", TenantID: f.tenant, ActorID: "ops-1",
		ConsolidationID: "C-1", ExpectedRevision: 3, PlannedDeparture: f.at.Add(96 * time.Hour), HouseCutOff: f.at.Add(72 * time.Hour),
		Reason: "flight retimed", OccurredAt: f.at.Add(5 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	membership, err := f.module.GetConsolMembership(ctx, api.GetConsolMembership{TenantID: f.tenant, ShipmentID: "SH-2"})
	if err != nil || !membership.Live || membership.Entry.ConsolidationID != "C-2" || membership.Consolidation.ConsolidationID != "C-2" ||
		len(membership.History) != 2 || membership.History[0].End != api.EntryTransferred || membership.History[0].Reason != api.ReasonShutOut ||
		membership.History[1].Start != api.EntryTransferred || membership.History[1].PredecessorNo != 1 || membership.Revision != 2 {
		t.Fatalf("membership of SH-2: %+v %v", membership, err)
	}
	if _, err = f.module.GetConsolMembership(ctx, api.GetConsolMembership{TenantID: f.tenant, ShipmentID: "SH-9"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("a shipment never attached: %v", err)
	}

	// A cancelled shipment leaves; a consolidation without houses can be cancelled.
	f.ful.mu.Lock()
	f.ful.status["SH-2"] = fulapi.StatusCancelled
	f.ful.mu.Unlock()
	if _, err = f.module.EndMembershipForCancelledShipment(ctx, api.EndMembershipForCancelledShipment{CommandID: "end-2", TenantID: f.tenant,
		ActorID: "ops-1", ConsolidationID: "C-2", ShipmentID: "SH-2", ExpectedMembershipRevision: 2, OccurredAt: f.at.Add(6 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.module.CancelConsolidation(ctx, api.CancelConsolidation{CommandID: "cancel-2", TenantID: f.tenant, ActorID: "ops-1",
		ConsolidationID: "C-2", ExpectedRevision: 1, Reason: "no cargo left", OccurredAt: f.at.Add(7 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.module.CancelConsolidation(ctx, api.CancelConsolidation{CommandID: "cancel-1", TenantID: f.tenant, ActorID: "ops-1",
		ConsolidationID: "C-1", ExpectedRevision: 4, Reason: "x", OccurredAt: f.at.Add(7 * time.Hour)}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("cancel with houses attached: %v", err)
	}
	c = f.consolidation(t, "C-1")
	if len(c.Versions) != 4 || c.Versions[3].Change != "AMENDED" || !c.Versions[0].PlannedDeparture.Equal(f.at.Add(72*time.Hour)) || !c.Current.PlannedDeparture.Equal(f.at.Add(96*time.Hour)) {
		t.Fatalf("versions: %+v", c.Versions)
	}
	want := []string{api.ConsolidationCreated, api.HouseAttached, api.HouseAttached, api.HouseIntentRefreshed, api.ConsolidationClosed,
		api.ConsolidationReopened, api.ConsolidationCreated, api.HouseTransferred, api.ConsolidationAmended, api.HouseDetached, api.ConsolidationCancelled}
	if got := f.outboxTypes(t); !slices.Equal(got, want) {
		t.Fatalf("outbox %v, want %v", got, want)
	}
}

// TestDatabaseRules writes past the application on the application role: versions and parts are immutable
// (refused by the grants, 42501, or by the triggers, 23001), an entry ends only once and only its end, a closed
// consolidation's houses are frozen, and a house cannot be inserted into a closed consolidation even by hand.
func TestDatabaseRules(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.create(t, "C-1")
	if _, err := f.module.AttachHouse(ctx, f.attach("attach-1", "C-1", "SH-1", 0)); err != nil {
		t.Fatal(err)
	}
	f.ful.allocate("SH-1", "C-1")
	if _, err := f.module.CloseConsolidation(ctx, f.closeCmd("close", "C-1", 1)); err != nil {
		t.Fatal(err)
	}
	refused := "42501|23001"
	for name, tc := range map[string]struct {
		sql   string
		state string
	}{
		"version immutable":     {`UPDATE cargo_phy.consolidation_version SET reason='x' WHERE tenant_id=$1`, refused},
		"part immutable":        {`UPDATE cargo_phy.membership_entry_part SET packs=1 WHERE tenant_id=$1`, refused},
		"entry scope immutable": {`UPDATE cargo_phy.membership_entry SET packs=1 WHERE tenant_id=$1`, refused},
		"entry ends only":       {`UPDATE cargo_phy.membership_entry SET end_text='x' WHERE tenant_id=$1`, "23001"},
		"no delete":             {`DELETE FROM cargo_phy.membership_entry WHERE tenant_id=$1`, refused},
		"mode fixed":            {`UPDATE cargo_phy.consolidation SET transport_mode='SEA', kind='SEA_LCL_OWN' WHERE tenant_id=$1`, "23001"},
		"frozen houses": {`UPDATE cargo_phy.membership_entry SET end_kind='DETACHED', end_reason='CUSTOMER_REQUEST', ended_by='x',
            ended_at=now(), end_entered_at=now() WHERE tenant_id=$1`, "23514"},
		"house into closed": {"", "23514"}, // two statements, below; refused at commit
	} {
		err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			if name == "house into closed" {
				if _, err := e.Exec(ctx, `INSERT INTO cargo_phy.consol_membership (tenant_id,shipment_id,stage) VALUES ($1,'SH-X','MAIN')`, f.tenant); err != nil {
					return err
				}
				_, err := e.Exec(ctx, `INSERT INTO cargo_phy.membership_entry (tenant_id,shipment_id,stage,entry_no,consolidation_id,start_kind,
                    ful_revision,plan_version,packs,gross_weight_unit,gross_weight,attached_by,attached_at,entered_at)
                    VALUES ($1,'SH-X','MAIN',1,'C-1','ATTACHED',1,1,1,'KGM',1,'x',now(),now())`, f.tenant)
				return err
			}
			_, err := e.Exec(ctx, tc.sql, f.tenant)
			return err
		})
		if state := sqlState(err); state == "" || !strings.Contains(tc.state, state) {
			t.Errorf("%s: %v (state %s), want %s", name, err, sqlState(err), tc.state)
		}
	}
	// A cancelled shipment's entry may end in a closed consolidation.
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		_, err := e.Exec(ctx, `UPDATE cargo_phy.membership_entry SET end_kind='SHIPMENT_CANCELLED', end_reason='SHIPMENT_CANCELLED', ended_by='x',
            ended_at=now(), end_entered_at=now() WHERE tenant_id=$1`, f.tenant)
		return err
	}); err != nil {
		t.Fatalf("a cancelled shipment leaves a closed consolidation: %v", err)
	}
}

// TestEightAttachesRaceAClose races attaches against a close of the same consolidation that is retried on
// conflict, as a client would: four attaches start with the close, a few milliseconds apart, and four more
// once it returned. A closed consolidation holds only houses its successful close confirmed with FUL; the
// late attaches are all refused.
func TestEightAttachesRaceAClose(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.ful.delay = 3 * time.Millisecond
	closedRounds, refusedAttaches := 0, 0
	for round := range 6 {
		id := fmt.Sprintf("C-R%d", round)
		f.create(t, id)
		seed := fmt.Sprintf("SH-R%d-0", round)
		if _, err := f.module.AttachHouse(ctx, f.attach("attach-"+seed, id, seed, 0)); err != nil {
			t.Fatal(err)
		}
		f.ful.allocate(seed, id)
		const workers = 8
		errs := make([]error, workers)
		var closeErr error
		closedBy := -1
		start, closed := make(chan struct{}), make(chan struct{})
		var wg sync.WaitGroup
		for i := range workers {
			shipment := fmt.Sprintf("SH-R%d-%d", round, i+1)
			f.ful.allocate(shipment, id)
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if i >= workers/2 {
					<-closed
				}
				time.Sleep(time.Duration(i*(round+1)) * time.Millisecond)
				_, errs[i] = f.module.AttachHouse(ctx, f.attach("attach-"+shipment, id, shipment, 0))
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(closed)
			<-start
			for attempt := range 50 {
				key := round*100 + attempt
				_, closeErr = f.module.CloseConsolidation(context.WithValue(ctx, closerKey{}, key), f.closeCmd(fmt.Sprintf("close-%s-%d", id, attempt), id, 1))
				if !errors.Is(closeErr, api.ErrConflict) {
					closedBy = key
					return
				}
			}
		}()
		close(start)
		wg.Wait()
		for i, err := range errs {
			switch {
			case err == nil && i >= workers/2:
				t.Fatalf("round %d: attach %d after the close committed", round, i)
			case err == nil:
			case errors.Is(err, api.ErrRejected), errors.Is(err, api.ErrConflict):
				refusedAttaches++
			default:
				t.Fatalf("round %d: attach %d: %v", round, i, err)
			}
		}
		c := f.consolidation(t, id)
		t.Logf("round %d: close %v, %d houses, state %s", round, closeErr, len(c.Members), c.State)
		if closeErr != nil {
			t.Fatalf("round %d: close: %v", round, closeErr)
		}
		closedRounds++
		f.ful.mu.Lock()
		asked := append([]string(nil), f.ful.asked[closedBy]...)
		f.ful.mu.Unlock()
		if c.State != api.StateClosed || len(asked) != len(c.Members) {
			t.Fatalf("round %d: state %s, %d houses, the close confirmed %v", round, c.State, len(c.Members), asked)
		}
		for _, m := range c.Members {
			if !slices.Contains(asked, m.ShipmentID) {
				t.Fatalf("round %d: %s is in the closed consolidation but the close never confirmed it (asked %v)", round, m.ShipmentID, asked)
			}
		}
	}
	t.Logf("%d rounds closed, %d attaches refused after the house cut-off", closedRounds, refusedAttaches)
}

// TestTwoConsolidationsRaceForOneShipment attaches one shipment to eight consolidations at once: one wins.
func TestTwoConsolidationsRaceForOneShipment(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	const workers = 8
	for i := range workers {
		f.create(t, fmt.Sprintf("C-%d", i))
	}
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = f.module.AttachHouse(ctx, f.attach(fmt.Sprintf("attach-%d", i), fmt.Sprintf("C-%d", i), "SH-1", 0))
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
	var ledgers, live int
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT (SELECT count(*) FROM cargo_phy.consol_membership WHERE tenant_id=$1),
            (SELECT count(*) FROM cargo_phy.membership_entry WHERE tenant_id=$1 AND ended_at IS NULL)`, f.tenant).Scan(&ledgers, &live)
	}); err != nil {
		t.Fatal(err)
	}
	if ledgers != 1 || live != 1 {
		t.Fatalf("%d ledgers and %d live entries stored, want 1 and 1", ledgers, live)
	}
}
