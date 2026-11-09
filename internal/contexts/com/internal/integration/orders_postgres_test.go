//go:build integration

// Package integration runs COM against PostgreSQL 16 with stand-ins for the query sides of ORG and RATE.
// The real ORG, RATE and COM together are tested by the composition root.
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
	commodule "go.idp.xyz/idp-cargo/internal/contexts/com"
	"go.idp.xyz/idp-cargo/internal/contexts/com/api"
	rateacl "go.idp.xyz/idp-cargo/internal/contexts/com/internal/adapters/rate"
	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	rateapi "go.idp.xyz/idp-cargo/internal/contexts/rate/api"
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

// orgStub answers ORG's party-role query for any party, or refuses with the given kind.
type orgStub struct {
	at     time.Time
	refuse error
}

func (o *orgStub) ResolvePartyRole(_ context.Context, q orgapi.ResolvePartyRole) (orgapi.PartyRole, error) {
	if o.refuse != nil {
		return orgapi.PartyRole{}, fmt.Errorf("%w: stub", o.refuse)
	}
	return orgapi.PartyRole{
		Ref:       orgapi.PartyRef{TenantID: q.TenantID, PartyID: q.PartyID, PartyRevision: 2, IdentitySource: orgapi.IdentitySource, LegalEntityScope: q.LegalEntityID},
		LegalName: "Fixture Corp", CountryCode: "US", RoleCode: q.RoleCode,
		Validity: kernel.ValidityInterval{From: o.at.Add(-time.Hour)}, EvidenceRef: "customer-contract",
	}, nil
}

// rateStub answers RATE's snapshot query: every quote is accepted for P-1, or the query is refused.
type rateStub struct {
	at     time.Time
	refuse error
}

func (r *rateStub) GetQuoteSnapshotV2(context.Context, rateapi.GetQuoteSnapshot) (rateapi.QuoteSnapshotV2, error) {
	return rateapi.QuoteSnapshotV2{}, errors.New("COM reads QuoteSnapshot v1 only")
}

func (r *rateStub) GetQuoteSnapshot(_ context.Context, q rateapi.GetQuoteSnapshot) (rateapi.QuoteSnapshot, error) {
	if r.refuse != nil {
		return rateapi.QuoteSnapshot{}, fmt.Errorf("%w: stub", r.refuse)
	}
	return snapshot(q.TenantID, q.QuoteID, r.at), nil
}

func snapshot(tenant, quote string, at time.Time) rateapi.QuoteSnapshot {
	usd := kernel.Money{Currency: "USD", Minor: 100_000}
	return rateapi.QuoteSnapshot{TenantID: tenant, QuoteID: quote, QuoteRevision: 3, LegalEntityID: "LE",
		Customer: rateapi.CustomerRef{PartyID: "P-1", PartyRevision: 2}, Lines: []rateapi.SnapshotLine{{ChargeCode: "FREIGHT", Amount: usd}},
		Total: usd, Validity: kernel.ValidityInterval{From: at.Add(-time.Hour), Until: at.Add(14 * 24 * time.Hour)},
		AcceptedAt: at.Add(-30 * time.Minute), AcceptanceEvidenceRef: "signed-quote"}
}

// orderPolicy forbids the actor "intruder".
type orderPolicy struct{}

func (orderPolicy) AuthorizeOrderAction(_ context.Context, _, actorID, _, _ string) error {
	if actorID == "intruder" {
		return errors.New("test policy: forbidden")
	}
	return nil
}

func kg(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }

func dm3(n int64) kernel.Quantity { return kernel.Quantity{Unit: "DMQ", Amount: n} }

var (
	pallets  = api.OrderLine{Description: "Pallets of machine parts", Packs: 10, PackageType: "PX", GrossWeight: kg(500), Volume: dm3(12_000)}
	cartons  = api.OrderLine{Description: "Cartons of spare parts", Packs: 12, PackageType: "CT", GrossWeight: kg(60)}
	twoLines = []api.OrderLine{pallets, cartons}
)

// cargo is the packs, gross weight and volume an obligation commits.
type cargo struct {
	packs          int64
	weight, volume kernel.Quantity
}

type fixture struct {
	module *commodule.Module
	db     *bentopg.DB
	org    *orgStub
	rate   *rateStub
	tenant string
	at     time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	at := time.Now().UTC().Truncate(time.Microsecond)
	f := &fixture{db: testDB(t), org: &orgStub{at: at}, rate: &rateStub{at: at}, at: at,
		tenant: "cargo-com-" + time.Now().UTC().Format("20060102T150405.000000000")}
	module, err := commodule.New(f.db, f.org, f.rate, orderPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	f.module = module
	return f
}

func (f *fixture) record(order, quote string, lines ...api.OrderLine) api.RecordCustomerOrder {
	return api.RecordCustomerOrder{CommandID: "record-" + order, TenantID: f.tenant, ActorID: "sales-1", OrderID: order, QuoteID: quote,
		CustomerPartyID: "P-1", Lines: lines, OccurredAt: f.at}
}

// The commands below happen in this order: allocation one hour after the order, reservation two, release
// three, revocation and amendment four, cancellation five.

func (f *fixture) allocate(order, command string, revision int64, line int, obligation string, c cargo) api.AllocateOrderToObligation {
	return api.AllocateOrderToObligation{CommandID: command, TenantID: f.tenant, ActorID: "planner-1", OrderID: order,
		ExpectedRevision: revision, LineNo: line, ObligationID: obligation, Packs: c.packs, GrossWeight: c.weight, Volume: c.volume,
		OccurredAt: f.at.Add(time.Hour)}
}

func (f *fixture) reserve(command, obligation string, version int64, shipment string) api.ReserveObligationForFulfillment {
	return api.ReserveObligationForFulfillment{CommandID: command, TenantID: f.tenant, ActorID: "fulfilment", ObligationID: obligation,
		ObligationVersion: version, ShipmentID: shipment, OccurredAt: f.at.Add(2 * time.Hour)}
}

func (f *fixture) release(command, obligation string, version int64, shipment string) api.ReleaseObligationReservation {
	return api.ReleaseObligationReservation{CommandID: command, TenantID: f.tenant, ActorID: "fulfilment", ObligationID: obligation,
		ObligationVersion: version, ShipmentID: shipment, OccurredAt: f.at.Add(3 * time.Hour)}
}

func (f *fixture) revoke(order, command string, revision int64, obligation string) api.RevokeObligation {
	return api.RevokeObligation{CommandID: command, TenantID: f.tenant, ActorID: "planner-1", OrderID: order,
		ExpectedRevision: revision, ObligationID: obligation, OccurredAt: f.at.Add(4 * time.Hour)}
}

func (f *fixture) amend(order, command string, revision int64, obligation, replacement string, c cargo) api.AmendObligation {
	return api.AmendObligation{CommandID: command, TenantID: f.tenant, ActorID: "planner-1", OrderID: order, ExpectedRevision: revision,
		ObligationID: obligation, NewObligationID: replacement, Packs: c.packs, GrossWeight: c.weight, Volume: c.volume,
		OccurredAt: f.at.Add(4 * time.Hour)}
}

func (f *fixture) cancel(order, command string, revision int64) api.CancelCustomerOrder {
	return api.CancelCustomerOrder{CommandID: command, TenantID: f.tenant, ActorID: "sales-1", OrderID: order,
		ExpectedRevision: revision, OccurredAt: f.at.Add(5 * time.Hour)}
}

func (f *fixture) snapshotV2(t *testing.T, obligation string, version int64) api.ObligationSnapshotV2 {
	t.Helper()
	s, err := f.module.GetObligationSnapshotV2(context.Background(), api.GetObligationSnapshot{TenantID: f.tenant, ObligationID: obligation, Version: version})
	if err != nil {
		t.Fatalf("snapshot v2 of %s version %d: %v", obligation, version, err)
	}
	return s
}

// within runs fn in a tenant-bound transaction on the application role.
func (f *fixture) within(t *testing.T, fn func(context.Context, bentopg.Executor) error) error {
	t.Helper()
	ctx := context.Background()
	return (scope.TenantTransactor{DB: f.db, Tenant: f.tenant}).WithinTransaction(ctx, func(txCtx context.Context) error {
		e, err := f.db.RequireExecutor(txCtx)
		if err != nil {
			return err
		}
		return fn(txCtx, e)
	})
}

func (f *fixture) exec(t *testing.T, sql string, args ...any) error {
	t.Helper()
	return f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		_, err := e.Exec(ctx, sql, append([]any{f.tenant}, args...)...)
		return err
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

func (f *fixture) revision(t *testing.T, order string) (rev int64) {
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT revision FROM cargo_com.customer_order WHERE tenant_id=$1 AND order_id=$2`, f.tenant, order).Scan(&rev)
	}); err != nil {
		t.Error(err)
	}
	return rev
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// Rogue writes as the application role: a revocation copied from version 1, and a reservation.
const (
	revokeSQL = `INSERT INTO cargo_com.fulfillment_obligation (tenant_id,obligation_id,obligation_version,order_id,line_no,packs,
        gross_weight_unit,gross_weight,volume_unit,volume,status,allocated_by,allocated_at,revoked_by,revoked_at)
      SELECT tenant_id,obligation_id,2,order_id,line_no,packs,gross_weight_unit,gross_weight,volume_unit,volume,'REVOKED',
        allocated_by,allocated_at,'rogue',$3 FROM cargo_com.fulfillment_obligation WHERE tenant_id=$1 AND obligation_id=$2 AND obligation_version=1`
	reserveSQL = `INSERT INTO cargo_com.obligation_reservation (tenant_id,order_id,reservation_no,obligation_id,obligation_version,
        shipment_id,reserved_by,reserved_at)
      SELECT tenant_id,order_id,$3,obligation_id,1,'S-SQL','rogue',$4 FROM cargo_com.fulfillment_obligation
        WHERE tenant_id=$1 AND obligation_id=$2 AND obligation_version=1`
)

func TestOrderLifecycleOnPostgres(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	record := f.record("O-1", "Q-1", twoLines...)
	receipt, err := f.module.RecordCustomerOrder(ctx, record)
	if err != nil || receipt.NewRevision != 1 {
		t.Fatalf("record: %+v, %v", receipt, err)
	}
	if again, err := f.module.RecordCustomerOrder(ctx, record); err != nil || again != receipt {
		t.Fatalf("replay: %+v, %v", again, err)
	}
	four := cargo{4, kg(200), dm3(5_000)}
	if _, err = f.module.AllocateOrderToObligation(ctx, f.allocate("O-1", "allocate-0", 2, 1, "OB-1", four)); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("stale revision: err = %v", err)
	}
	if r, err := f.module.AllocateOrderToObligation(ctx, f.allocate("O-1", "allocate-1", 1, 1, "OB-1", four)); err != nil || r.NewRevision != 2 {
		t.Fatalf("allocate OB-1: %+v, %v", r, err)
	}
	for name, c := range map[string]cargo{
		"more packs":  {7, kg(1), dm3(1)},
		"more weight": {1, kg(301), dm3(1)},
		"more volume": {1, kg(1), dm3(7_001)},
	} {
		if _, err = f.module.AllocateOrderToObligation(ctx, f.allocate("O-1", "allocate-"+name, 2, 1, "OB-2", c)); !errors.Is(err, api.ErrRejected) ||
			!errors.Is(err, com.ErrOverAllocation) {
			t.Fatalf("%s than line 1 has left: err = %v", name, err)
		}
	}
	if _, err = f.module.AllocateOrderToObligation(ctx, f.allocate("O-1", "allocate-no-volume", 2, 1, "OB-2", cargo{1, kg(1), kernel.Quantity{}})); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("no volume on a line with volume: err = %v", err)
	}
	if r, err := f.module.AllocateOrderToObligation(ctx, f.allocate("O-1", "allocate-3", 2, 1, "OB-2", cargo{6, kg(300), dm3(7_000)})); err != nil || r.NewRevision != 3 {
		t.Fatalf("allocate OB-2: %+v, %v", r, err)
	}
	if r, err := f.module.AllocateOrderToObligation(ctx, f.allocate("O-1", "allocate-4", 3, 2, "OB-3", cargo{12, kg(60), kernel.Quantity{}})); err != nil || r.NewRevision != 4 {
		t.Fatalf("allocate OB-3: %+v, %v", r, err)
	}
	v1, err := f.module.GetObligationSnapshot(ctx, api.GetObligationSnapshot{TenantID: f.tenant, ObligationID: "OB-1", Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	wantV1 := api.ObligationSnapshot{TenantID: f.tenant, ObligationID: "OB-1", ObligationVersion: 1, OrderID: "O-1", OrderLineNo: 1,
		Description: "Pallets of machine parts", Quantity: kernel.Quantity{Unit: "XPX", Amount: 4}, LegalEntityID: "LE",
		Customer: api.CustomerRef{PartyID: "P-1", PartyRevision: 2}, QuoteID: "Q-1", QuoteRevision: 3, AllocatedAt: v1.AllocatedAt}
	if v1 != wantV1 || !v1.AllocatedAt.Equal(f.at.Add(time.Hour)) {
		t.Fatalf("snapshot v1 %+v", v1)
	}
	v2 := f.snapshotV2(t, "OB-3", 1)
	wantV2 := api.ObligationSnapshotV2{TenantID: f.tenant, ObligationID: "OB-3", ObligationVersion: 1, LatestVersion: 1,
		Status: api.ObligationActive, OrderID: "O-1", OrderLineNo: 2, Description: "Cartons of spare parts", Packs: 12, PackageType: "CT",
		GrossWeight: kg(60), LegalEntityID: "LE", Customer: api.CustomerRef{PartyID: "P-1", PartyRevision: 2}, QuoteID: "Q-1",
		QuoteRevision: 3, AllocatedAt: v2.AllocatedAt}
	if v2 != wantV2 || !v2.AllocatedAt.Equal(f.at.Add(time.Hour)) {
		t.Fatalf("snapshot v2 %+v", v2)
	}
	if _, err = f.module.GetObligationSnapshotV2(ctx, api.GetObligationSnapshot{TenantID: f.tenant, ObligationID: "OB-1", Version: 2}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("version 2 of OB-1: err = %v", err)
	}
	wantEvents := []string{api.CustomerOrderRecorded, api.FulfillmentObligationAllocated, api.FulfillmentObligationAllocated, api.FulfillmentObligationAllocated}
	if got := f.outboxTypes(t); !reflect.DeepEqual(got, wantEvents) {
		t.Fatalf("outbox events %v, want %v", got, wantEvents)
	}
	terms, _ := rateacl.TermsHash(snapshot(f.tenant, "Q-1", f.at))
	var status, hash, name string
	var revision int64
	if err = f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT status,revision,accepted_terms_sha256,customer_legal_name FROM cargo_com.customer_order
            WHERE tenant_id=$1 AND order_id='O-1'`, f.tenant).Scan(&status, &revision, &hash, &name)
	}); err != nil {
		t.Fatal(err)
	}
	if status != "ALLOCATED" || revision != 4 || hash != terms || name != "Fixture Corp" {
		t.Fatalf("stored order: status %s, revision %d, terms %s (want %s), name %s", status, revision, hash, terms, name)
	}
}

// TestRevocationReservationAndCancellationOnPostgres runs the C6 commands through COM's api: a reserved
// obligation cannot be revoked nor its order cancelled until FUL releases it; an amendment revokes and
// allocates in one command; cancellation revokes what is still active.
func TestRevocationReservationAndCancellationOnPostgres(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.module.RecordCustomerOrder(ctx, f.record("O-1", "Q-1", twoLines...)); err != nil {
		t.Fatal(err)
	}
	for i, a := range []api.AllocateOrderToObligation{
		f.allocate("O-1", "allocate-1", 1, 1, "OB-1", cargo{10, kg(500), dm3(12_000)}),
		f.allocate("O-1", "allocate-2", 2, 2, "OB-2", cargo{12, kg(60), kernel.Quantity{}}),
	} {
		if r, err := f.module.AllocateOrderToObligation(ctx, a); err != nil || r.NewRevision != int64(i+2) {
			t.Fatalf("allocate %s: %+v, %v", a.ObligationID, r, err)
		}
	}
	reserve := f.reserve("reserve-1", "OB-1", 1, "S-1")
	r, err := f.module.ReserveObligationForFulfillment(ctx, reserve)
	if err != nil || r.OrderID != "O-1" || r.NewRevision != 4 {
		t.Fatalf("reserve: %+v, %v", r, err)
	}
	if again, err := f.module.ReserveObligationForFulfillment(ctx, reserve); err != nil || again != r {
		t.Fatalf("replay of the reservation: %+v, %v", again, err)
	}
	if s := f.snapshotV2(t, "OB-1", 1); s.HeldByShipmentID != "S-1" || s.Status != api.ObligationActive || s.LatestVersion != 1 {
		t.Fatalf("reserved snapshot %+v", s)
	}
	for name, run := range map[string]func() error{
		"revoke": func() error {
			_, err := f.module.RevokeObligation(ctx, f.revoke("O-1", "revoke-0", 4, "OB-1"))
			return err
		},
		"amend": func() error {
			_, err := f.module.AmendObligation(ctx, f.amend("O-1", "amend-0", 4, "OB-1", "OB-9", cargo{1, kg(1), dm3(1)}))
			return err
		},
		"cancel": func() error { _, err := f.module.CancelCustomerOrder(ctx, f.cancel("O-1", "cancel-0", 4)); return err },
		"reserve again for S-1": func() error {
			_, err := f.module.ReserveObligationForFulfillment(ctx, f.reserve("reserve-2", "OB-1", 1, "S-1"))
			return err
		},
		"release for S-2": func() error {
			_, err := f.module.ReleaseObligationReservation(ctx, f.release("release-0", "OB-1", 1, "S-2"))
			return err
		},
	} {
		if err := run(); !errors.Is(err, api.ErrRejected) {
			t.Errorf("%s while S-1 holds OB-1: err = %v", name, err)
		}
	}
	if r, err = f.module.ReleaseObligationReservation(ctx, f.release("release-1", "OB-1", 1, "S-1")); err != nil || r.NewRevision != 5 {
		t.Fatalf("release: %+v, %v", r, err)
	}
	if r, err = f.module.AmendObligation(ctx, f.amend("O-1", "amend-1", 5, "OB-1", "OB-3", cargo{8, kg(400), dm3(9_000)})); err != nil || r.NewRevision != 6 {
		t.Fatalf("amend: %+v, %v", r, err)
	}
	old, revoked, replacement := f.snapshotV2(t, "OB-1", 1), f.snapshotV2(t, "OB-1", 2), f.snapshotV2(t, "OB-3", 1)
	if old.Status != api.ObligationActive || old.LatestVersion != 2 || old.HeldByShipmentID != "" ||
		revoked.Status != api.ObligationRevoked || !revoked.RevokedAt.Equal(f.at.Add(4*time.Hour)) || revoked.Packs != 10 ||
		replacement.Packs != 8 || replacement.GrossWeight != kg(400) || replacement.Volume != dm3(9_000) || replacement.OrderLineNo != 1 {
		t.Fatalf("after the amendment: OB-1 v1 %+v, OB-1 v2 %+v, OB-3 %+v", old, revoked, replacement)
	}
	if _, err = f.module.GetObligationSnapshot(ctx, api.GetObligationSnapshot{TenantID: f.tenant, ObligationID: "OB-1", Version: 2}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("ObligationSnapshot v1 of a revocation: err = %v", err)
	}
	if r, err = f.module.RevokeObligation(ctx, f.revoke("O-1", "revoke-2", 6, "OB-2")); err != nil || r.NewRevision != 7 {
		t.Fatalf("revoke OB-2: %+v, %v", r, err)
	}
	cancel := f.cancel("O-1", "cancel-1", 7)
	if r, err = f.module.CancelCustomerOrder(ctx, cancel); err != nil || r.NewRevision != 8 {
		t.Fatalf("cancel: %+v, %v", r, err)
	}
	if again, err := f.module.CancelCustomerOrder(ctx, cancel); err != nil || again != r {
		t.Fatalf("replay of the cancellation: %+v, %v", again, err)
	}
	if s := f.snapshotV2(t, "OB-3", 2); s.Status != api.ObligationRevoked || !s.RevokedAt.Equal(f.at.Add(5*time.Hour)) {
		t.Fatalf("OB-3 after the cancellation: %+v", s)
	}
	if _, err = f.module.AllocateOrderToObligation(ctx, f.allocate("O-1", "allocate-9", 8, 1, "OB-9", cargo{1, kg(1), dm3(1)})); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("allocate on a cancelled order: err = %v", err)
	}
	if _, err = f.module.ReserveObligationForFulfillment(ctx, f.reserve("reserve-9", "OB-3", 1, "S-9")); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("reserve on a cancelled order: err = %v", err)
	}
	want := []string{api.CustomerOrderRecorded, api.FulfillmentObligationAllocated, api.FulfillmentObligationAllocated,
		api.ObligationReserved, api.ObligationReservationReleased, api.FulfillmentObligationRevoked, api.FulfillmentObligationAllocated,
		api.FulfillmentObligationRevoked, api.FulfillmentObligationRevoked, api.CustomerOrderCancelled}
	if got := f.outboxTypes(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("outbox events %v, want %v", got, want)
	}
	var status, cancelledBy string
	var versions, revocations, reservations, held int
	if err = f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT o.status, o.cancelled_by,
            (SELECT count(*) FROM cargo_com.fulfillment_obligation f WHERE f.tenant_id=o.tenant_id AND f.order_id=o.order_id),
            (SELECT count(*) FROM cargo_com.fulfillment_obligation f WHERE f.tenant_id=o.tenant_id AND f.order_id=o.order_id AND f.status='REVOKED'),
            (SELECT count(*) FROM cargo_com.obligation_reservation r WHERE r.tenant_id=o.tenant_id AND r.order_id=o.order_id),
            (SELECT count(*) FROM cargo_com.obligation_reservation r WHERE r.tenant_id=o.tenant_id AND r.order_id=o.order_id AND r.released_at IS NULL)
          FROM cargo_com.customer_order o WHERE o.tenant_id=$1 AND o.order_id='O-1'`, f.tenant).Scan(&status, &cancelledBy, &versions, &revocations, &reservations, &held)
	}); err != nil {
		t.Fatal(err)
	}
	if status != "CANCELLED" || cancelledBy != "sales-1" || versions != 6 || revocations != 3 || reservations != 1 || held != 0 {
		t.Fatalf("stored: status %s by %s, %d versions, %d revocations, %d reservations, %d held", status, cancelledBy, versions, revocations, reservations, held)
	}
}

func TestOrderCommitmentCannotBeChangedInPostgres(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.module.RecordCustomerOrder(ctx, f.record("O-1", "Q-1", twoLines...)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.AllocateOrderToObligation(ctx, f.allocate("O-1", "allocate-1", 1, 1, "OB-1", cargo{4, kg(200), dm3(5_000)})); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.ReserveObligationForFulfillment(ctx, f.reserve("reserve-1", "OB-1", 1, "S-1")); err != nil {
		t.Fatal(err)
	}
	reservedAt, revokedAt := f.at.Add(2*time.Hour), f.at.Add(4*time.Hour)
	type attempt struct {
		sql   string
		args  []any
		state string
		why   string
	}
	attempts := func(phase string, cases []attempt) {
		for _, tc := range cases {
			if err := f.exec(t, tc.sql, tc.args...); sqlState(err) != tc.state {
				t.Errorf("%s: %s: err = %v, want SQLSTATE %s (%s)", phase, tc.sql, err, tc.state, tc.why)
			}
		}
	}
	attempts("reserved", []attempt{
		{`UPDATE cargo_com.customer_order SET customer_legal_name='Renamed Corp' WHERE tenant_id=$1`, nil, "23001", "the trigger freezes the commitment"},
		{`UPDATE cargo_com.customer_order SET status='CANCELLED' WHERE tenant_id=$1`, nil, "23514", "a cancellation records who and when"},
		{`UPDATE cargo_com.order_line SET packs=1 WHERE tenant_id=$1`, nil, "42501", "the application role cannot change lines"},
		{`UPDATE cargo_com.fulfillment_obligation SET status='REVOKED' WHERE tenant_id=$1`, nil, "42501", "obligation versions never change"},
		{`DELETE FROM cargo_com.fulfillment_obligation WHERE tenant_id=$1`, nil, "42501", "the application role cannot delete obligations"},
		{`DELETE FROM cargo_com.customer_order WHERE tenant_id=$1`, nil, "42501", "the application role cannot delete orders"},
		{`DELETE FROM cargo_com.obligation_reservation WHERE tenant_id=$1`, nil, "42501", "reservations are kept"},
		{`UPDATE cargo_com.obligation_reservation SET shipment_id='S-X' WHERE tenant_id=$1`, nil, "42501", "only the release columns are writable"},
		{`INSERT INTO cargo_com.fulfillment_obligation (tenant_id,obligation_id,obligation_version,order_id,line_no,packs,gross_weight_unit,
          gross_weight,volume_unit,volume,status,allocated_by,allocated_at) VALUES ($1,'OB-X',1,'O-1',1,7,'KGM',1,'DMQ',1,'ACTIVE','rogue',now())`,
			nil, "23514", "the line has 6 packs left"},
		{`INSERT INTO cargo_com.fulfillment_obligation (tenant_id,obligation_id,obligation_version,order_id,line_no,packs,gross_weight_unit,
          gross_weight,volume_unit,volume,status,allocated_by,allocated_at) VALUES ($1,'OB-Y',1,'O-1',1,1,'LBR',1,'DMQ',1,'ACTIVE','rogue',now())`,
			nil, "23514", "the line weighs in KGM"},
		{`INSERT INTO cargo_com.fulfillment_obligation (tenant_id,obligation_id,obligation_version,order_id,line_no,packs,gross_weight_unit,
          gross_weight,status,allocated_by,allocated_at) VALUES ($1,'OB-Z',1,'O-1',1,1,'KGM',1,'ACTIVE','rogue',now())`,
			nil, "23514", "the line declares a volume"},
		{revokeSQL, []any{"OB-1", revokedAt}, "23514", "S-1 holds OB-1"},
		{`INSERT INTO cargo_com.obligation_reservation (tenant_id,order_id,reservation_no,obligation_id,obligation_version,shipment_id,
          reserved_by,reserved_at) SELECT tenant_id,order_id,2,obligation_id,1,'S-1','rogue',$2 FROM cargo_com.fulfillment_obligation
          WHERE tenant_id=$1 AND obligation_id='OB-1' AND obligation_version=1`, []any{reservedAt}, "23505",
			"a shipment holds one reservation of an obligation (other shipments may hold one too, F1)"},
	})
	if _, err := f.module.ReleaseObligationReservation(ctx, f.release("release-1", "OB-1", 1, "S-1")); err != nil {
		t.Fatal(err)
	}
	attempts("released", []attempt{
		{`UPDATE cargo_com.obligation_reservation SET released_by='rogue', released_at=released_at + interval '1 hour' WHERE tenant_id=$1`,
			nil, "23001", "a reservation is released once"},
		{`INSERT INTO cargo_com.fulfillment_obligation (tenant_id,obligation_id,obligation_version,order_id,line_no,packs,gross_weight_unit,
          gross_weight,volume_unit,volume,status,allocated_by,allocated_at,revoked_by,revoked_at)
          SELECT tenant_id,obligation_id,2,order_id,line_no,packs-1,gross_weight_unit,gross_weight,volume_unit,volume,'REVOKED',
            allocated_by,allocated_at,'rogue',$2 FROM cargo_com.fulfillment_obligation WHERE tenant_id=$1 AND obligation_id='OB-1'`,
			[]any{revokedAt}, "23514", "a revocation repeats the measure"},
		{`INSERT INTO cargo_com.fulfillment_obligation (tenant_id,obligation_id,obligation_version,order_id,line_no,packs,gross_weight_unit,
          gross_weight,volume_unit,volume,status,allocated_by,allocated_at)
          SELECT tenant_id,obligation_id,2,order_id,line_no,packs,gross_weight_unit,gross_weight,volume_unit,volume,'ACTIVE',
            allocated_by,allocated_at FROM cargo_com.fulfillment_obligation WHERE tenant_id=$1 AND obligation_id='OB-1'`,
			nil, "23514", "a later version only revokes"},
	})
	if s := f.snapshotV2(t, "OB-1", 1); s.Packs != 4 || s.HeldByShipmentID != "" || s.LatestVersion != 1 {
		t.Fatalf("snapshot after the attempts: %+v", s)
	}
	if r, err := f.module.CancelCustomerOrder(ctx, f.cancel("O-1", "cancel-1", 4)); err != nil || r.NewRevision != 5 {
		t.Fatalf("the order still cancels normally: %+v, %v", r, err)
	}
	attempts("cancelled", []attempt{
		{`UPDATE cargo_com.customer_order SET status='RECORDED', cancelled_by=NULL, cancelled_at=NULL WHERE tenant_id=$1`,
			nil, "23001", "a cancelled order never changes"},
		{`INSERT INTO cargo_com.fulfillment_obligation (tenant_id,obligation_id,obligation_version,order_id,line_no,packs,gross_weight_unit,
          gross_weight,status,allocated_by,allocated_at) VALUES ($1,'OB-W',1,'O-1',2,1,'KGM',1,'ACTIVE','rogue',now())`,
			nil, "23514", "a cancelled order takes no obligation"},
		{reserveSQL, []any{"OB-1", 2, reservedAt}, "23514", "a revoked obligation cannot be reserved"},
	})
}

// TestConcurrentAllocationsNeverExceedTheLine races allocations for one order line. Against the same
// revision exactly one commits; retrying with fresh revisions, allocations stop when the line is full.
func TestConcurrentAllocationsNeverExceedTheLine(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	drums := api.OrderLine{Description: "Drums of resin", Packs: 10, PackageType: "DR", GrossWeight: kg(2_000)}
	if _, err := f.module.RecordCustomerOrder(ctx, f.record("O-1", "Q-1", drums)); err != nil {
		t.Fatal(err)
	}
	four := cargo{4, kg(800), kernel.Quantity{}}
	const workers = 8
	race := func(attempt func(i int) error) []error {
		errs := make([]error, workers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				errs[i] = attempt(i)
			}(i)
		}
		close(start)
		wg.Wait()
		return errs
	}
	count := func(errs []error, kind error) (n int) {
		for _, err := range errs {
			if (kind == nil && err == nil) || (kind != nil && errors.Is(err, kind)) {
				n++
			}
		}
		return n
	}
	first := race(func(i int) error {
		_, err := f.module.AllocateOrderToObligation(ctx, f.allocate("O-1", fmt.Sprintf("first-%d", i), 1, 1, fmt.Sprintf("OB-A%d", i), four))
		return err
	})
	if count(first, nil) != 1 || count(first, api.ErrConflict) != workers-1 {
		t.Fatalf("same revision: %d committed, %d conflicts; errors %v", count(first, nil), count(first, api.ErrConflict), first)
	}
	second := race(func(i int) error {
		for try := 0; try < 50; try++ {
			_, err := f.module.AllocateOrderToObligation(ctx, f.allocate("O-1", fmt.Sprintf("second-%d-%d", i, try), f.revision(t, "O-1"), 1,
				fmt.Sprintf("OB-B%d-%d", i, try), four))
			if !errors.Is(err, api.ErrConflict) {
				return err
			}
		}
		return errors.New("still conflicting after 50 tries")
	})
	if count(second, nil) != 1 || count(second, com.ErrOverAllocation) != workers-1 {
		t.Fatalf("fresh revisions: %d committed, %d over-allocations; errors %v", count(second, nil), count(second, com.ErrOverAllocation), second)
	}
	var allocated, obligations, rev int64
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT coalesce(sum(packs),0), count(*), (SELECT revision FROM cargo_com.customer_order WHERE tenant_id=$1 AND order_id='O-1')
            FROM cargo_com.fulfillment_obligation WHERE tenant_id=$1 AND order_id='O-1'`, f.tenant).Scan(&allocated, &obligations, &rev)
	}); err != nil {
		t.Fatal(err)
	}
	events := 0
	for _, kind := range f.outboxTypes(t) {
		if kind == api.FulfillmentObligationAllocated {
			events++
		}
	}
	if allocated != 8 || obligations != 2 || rev != 3 || events != 2 {
		t.Fatalf("allocated %d packs in %d obligations at revision %d with %d events, want 8, 2, 3, 2", allocated, obligations, rev, events)
	}
}

// TestReservationAndRevocationRaceThroughTheAPI races, for each of several obligations, FUL's reservation
// against a planner's revocation. For every obligation exactly one of the two commits.
func TestReservationAndRevocationRaceThroughTheAPI(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	const n = 6
	drums := api.OrderLine{Description: "Drums of resin", Packs: n, PackageType: "DR", GrossWeight: kg(200 * n)}
	if _, err := f.module.RecordCustomerOrder(ctx, f.record("O-1", "Q-1", drums)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("OB-%d", i)
		if _, err := f.module.AllocateOrderToObligation(ctx, f.allocate("O-1", "allocate-"+id, int64(i+1), 1, id, cargo{1, kg(200), kernel.Quantity{}})); err != nil {
			t.Fatal(err)
		}
	}
	reserved, revoked := make([]error, n), make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("OB-%d", i)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, reserved[i] = f.module.ReserveObligationForFulfillment(ctx, f.reserve("reserve-"+id, id, 1, "S-"+id))
		}()
		go func() {
			defer wg.Done()
			<-start
			for try := 0; try < 200; try++ {
				_, revoked[i] = f.module.RevokeObligation(ctx, f.revoke("O-1", fmt.Sprintf("revoke-%s-%d", id, try), f.revision(t, "O-1"), id))
				if !errors.Is(revoked[i], api.ErrConflict) {
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	for i := 0; i < n; i++ {
		switch {
		case reserved[i] == nil && errors.Is(revoked[i], com.ErrObligationReserved):
		case revoked[i] == nil && errors.Is(reserved[i], com.ErrObligationNotActive):
		default:
			t.Errorf("OB-%d: reservation %v, revocation %v; want exactly one to commit", i, reserved[i], revoked[i])
		}
	}
	var held, revocations, both int
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT
            (SELECT count(*) FROM cargo_com.obligation_reservation WHERE tenant_id=$1 AND released_at IS NULL),
            (SELECT count(*) FROM cargo_com.fulfillment_obligation WHERE tenant_id=$1 AND status='REVOKED'),
            (SELECT count(*) FROM cargo_com.obligation_reservation r JOIN cargo_com.fulfillment_obligation f
               ON f.tenant_id=r.tenant_id AND f.obligation_id=r.obligation_id AND f.status='REVOKED' WHERE r.tenant_id=$1)`,
			f.tenant).Scan(&held, &revocations, &both)
	}); err != nil {
		t.Fatal(err)
	}
	if held+revocations != n || both != 0 {
		t.Fatalf("%d held reservations and %d revocations of %d obligations, %d obligations both", held, revocations, n, both)
	}
	if _, err := f.module.GetObligationSnapshotV2(ctx, api.GetObligationSnapshot{TenantID: f.tenant, ObligationID: "OB-0", Version: 1}); err != nil {
		t.Fatalf("the order still reads back consistently: %v", err)
	}
}

// TestReservationAndRevocationRaceInSQL shows that PostgreSQL alone keeps a reservation and a revocation of
// one obligation from both committing: whichever transaction locks the order row first wins, and the
// other, once the lock is free, sees what the winner committed.
func TestReservationAndRevocationRaceInSQL(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	drums := api.OrderLine{Description: "Drums of resin", Packs: 2, PackageType: "DR", GrossWeight: kg(400)}
	if _, err := f.module.RecordCustomerOrder(ctx, f.record("O-1", "Q-1", drums)); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"OB-A", "OB-B"} {
		if _, err := f.module.AllocateOrderToObligation(ctx, f.allocate("O-1", "allocate-"+id, int64(i+1), 1, id, cargo{1, kg(200), kernel.Quantity{}})); err != nil {
			t.Fatal(err)
		}
	}
	reserve := func(id string, no int) func(context.Context, bentopg.Executor) error {
		return func(ctx context.Context, e bentopg.Executor) error {
			_, err := e.Exec(ctx, reserveSQL, f.tenant, id, no, f.at.Add(2*time.Hour))
			return err
		}
	}
	revoke := func(id string) func(context.Context, bentopg.Executor) error {
		return func(ctx context.Context, e bentopg.Executor) error {
			_, err := e.Exec(ctx, revokeSQL, f.tenant, id, f.at.Add(4*time.Hour))
			return err
		}
	}
	// race runs first in a transaction that stays open for a moment after its write, and second as soon as
	// first has written, so that second waits for first's order row lock.
	race := func(first, second func(context.Context, bentopg.Executor) error) (error, error) {
		written := make(chan struct{})
		var firstErr, secondErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			firstErr = f.within(t, func(ctx context.Context, e bentopg.Executor) error {
				err := first(ctx, e)
				close(written)
				time.Sleep(300 * time.Millisecond)
				return err
			})
		}()
		go func() {
			defer wg.Done()
			<-written
			secondErr = f.within(t, second)
		}()
		wg.Wait()
		return firstErr, secondErr
	}
	if a, b := race(reserve("OB-A", 1), revoke("OB-A")); a != nil || sqlState(b) != "23514" {
		t.Fatalf("reservation first: reservation %v, revocation %v (want SQLSTATE 23514)", a, b)
	}
	if a, b := race(revoke("OB-B"), reserve("OB-B", 2)); a != nil || sqlState(b) != "23514" {
		t.Fatalf("revocation first: revocation %v, reservation %v (want SQLSTATE 23514)", a, b)
	}
	var heldA, revokedB, versions int
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT
            (SELECT count(*) FROM cargo_com.obligation_reservation WHERE tenant_id=$1 AND obligation_id='OB-A' AND released_at IS NULL),
            (SELECT count(*) FROM cargo_com.fulfillment_obligation WHERE tenant_id=$1 AND obligation_id='OB-B' AND status='REVOKED'),
            (SELECT count(*) FROM cargo_com.fulfillment_obligation WHERE tenant_id=$1)`, f.tenant).Scan(&heldA, &revokedB, &versions)
	}); err != nil {
		t.Fatal(err)
	}
	if heldA != 1 || revokedB != 1 || versions != 3 {
		t.Fatalf("OB-A held %d, OB-B revoked %d, %d obligation versions; want 1, 1, 3", heldA, revokedB, versions)
	}
}

func TestOrderRefusalsOnPostgres(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.rate.refuse = rateapi.ErrRejected
	if _, err := f.module.RecordCustomerOrder(ctx, f.record("O-1", "Q-1", pallets)); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("quote not accepted by RATE: err = %v", err)
	}
	one := cargo{1, kg(1), dm3(1)}
	if _, err := f.module.AllocateOrderToObligation(ctx, f.allocate("O-1", "allocate-0", 1, 1, "OB-0", one)); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("a refused order must not be stored: err = %v", err)
	}
	f.rate.refuse, f.org.refuse = nil, orgapi.ErrRejected
	if _, err := f.module.RecordCustomerOrder(ctx, f.record("O-1", "Q-1", pallets)); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("customer not confirmed by ORG: err = %v", err)
	}
	f.org.refuse = nil
	if _, err := f.module.RecordCustomerOrder(ctx, f.record("O-1", "Q-1", pallets)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.AllocateOrderToObligation(ctx, f.allocate("O-1", "allocate-1", 1, 1, "OB-1", one)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.RecordCustomerOrder(ctx, f.record("O-3", "Q-3", pallets)); err != nil {
		t.Fatal(err)
	}
	otherCustomer := f.record("O-4", "Q-4", pallets)
	otherCustomer.CustomerPartyID = "P-2"
	sameOrder := f.record("O-1", "Q-2", pallets)
	sameOrder.CommandID = "record-O-1-again"
	sameQuote := f.record("O-2", "Q-1", pallets)
	intruder := f.record("O-5", "Q-5", pallets)
	intruder.ActorID = "intruder"
	invalidPackage := f.record("O-6", "Q-6", api.OrderLine{Description: "Pallets", Packs: 1, PackageType: "pallet", GrossWeight: kg(1)})
	for name, tc := range map[string]struct {
		run  func() error
		want error
	}{
		"the quote's customer is another party": {func() error { _, err := f.module.RecordCustomerOrder(ctx, otherCustomer); return err }, api.ErrRejected},
		"same order id under a new command":     {func() error { _, err := f.module.RecordCustomerOrder(ctx, sameOrder); return err }, api.ErrConflict},
		"a second order for the same quote":     {func() error { _, err := f.module.RecordCustomerOrder(ctx, sameQuote); return err }, api.ErrConflict},
		"a package type that is not a code":     {func() error { _, err := f.module.RecordCustomerOrder(ctx, invalidPackage); return err }, api.ErrRejected},
		"obligation id used by another order": {func() error {
			_, err := f.module.AllocateOrderToObligation(ctx, f.allocate("O-3", "allocate-3", 1, 1, "OB-1", one))
			return err
		}, api.ErrConflict},
		"actor refused by the authorizer": {func() error { _, err := f.module.RecordCustomerOrder(ctx, intruder); return err }, api.ErrUnauthorized},
		"obligation of another tenant": {func() error {
			_, err := f.module.GetObligationSnapshot(ctx, api.GetObligationSnapshot{TenantID: f.tenant + "-other", ObligationID: "OB-1", Version: 1})
			return err
		}, api.ErrNotFound},
		"reservation of an unknown obligation": {func() error {
			_, err := f.module.ReserveObligationForFulfillment(ctx, f.reserve("reserve-9", "OB-9", 1, "S-1"))
			return err
		}, api.ErrNotFound},
		"reservation of an unknown version": {func() error {
			_, err := f.module.ReserveObligationForFulfillment(ctx, f.reserve("reserve-8", "OB-1", 2, "S-1"))
			return err
		}, api.ErrNotFound},
		"revocation of an obligation the order does not hold": {func() error {
			_, err := f.module.RevokeObligation(ctx, f.revoke("O-3", "revoke-3", 1, "OB-1"))
			return err
		}, api.ErrNotFound},
		"reservation by an actor the authorizer refuses": {func() error {
			cmd := f.reserve("reserve-7", "OB-1", 1, "S-1")
			cmd.ActorID = "intruder"
			_, err := f.module.ReserveObligationForFulfillment(ctx, cmd)
			return err
		}, api.ErrUnauthorized},
	} {
		if err := tc.run(); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}
