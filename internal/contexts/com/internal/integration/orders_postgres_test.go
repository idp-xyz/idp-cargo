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

func pieces(n int64) kernel.Quantity { return kernel.Quantity{Unit: "H87", Amount: n} }

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

func (f *fixture) allocate(order, command string, revision int64, line int, obligation string, q kernel.Quantity) api.AllocateOrderToObligation {
	return api.AllocateOrderToObligation{CommandID: command, TenantID: f.tenant, ActorID: "planner-1", OrderID: order,
		ExpectedRevision: revision, LineNo: line, ObligationID: obligation, Quantity: q, OccurredAt: f.at.Add(time.Hour)}
}

var twoLines = []api.OrderLine{{Description: "Pallets of machine parts", Quantity: kg(500)}, {Description: "Cartons of spare parts", Quantity: pieces(12)}}

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
	if _, err = f.module.AllocateOrderToObligation(ctx, f.allocate("O-1", "allocate-0", 2, 1, "OB-1", kg(200))); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("stale revision: err = %v", err)
	}
	if r, err := f.module.AllocateOrderToObligation(ctx, f.allocate("O-1", "allocate-1", 1, 1, "OB-1", kg(200))); err != nil || r.NewRevision != 2 {
		t.Fatalf("allocate OB-1: %+v, %v", r, err)
	}
	if _, err = f.module.AllocateOrderToObligation(ctx, f.allocate("O-1", "allocate-2", 2, 1, "OB-2", kg(301))); !errors.Is(err, api.ErrRejected) || !errors.Is(err, com.ErrOverAllocation) {
		t.Fatalf("over-allocation: err = %v", err)
	}
	if r, err := f.module.AllocateOrderToObligation(ctx, f.allocate("O-1", "allocate-3", 2, 1, "OB-2", kg(300))); err != nil || r.NewRevision != 3 {
		t.Fatalf("allocate OB-2: %+v, %v", r, err)
	}
	if r, err := f.module.AllocateOrderToObligation(ctx, f.allocate("O-1", "allocate-4", 3, 2, "OB-3", pieces(12))); err != nil || r.NewRevision != 4 {
		t.Fatalf("allocate OB-3: %+v, %v", r, err)
	}
	s, err := f.module.GetObligationSnapshot(ctx, api.GetObligationSnapshot{TenantID: f.tenant, ObligationID: "OB-1", Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	want := api.ObligationSnapshot{TenantID: f.tenant, ObligationID: "OB-1", ObligationVersion: 1, OrderID: "O-1", OrderLineNo: 1,
		Description: "Pallets of machine parts", Quantity: kg(200), LegalEntityID: "LE", Customer: api.CustomerRef{PartyID: "P-1", PartyRevision: 2},
		QuoteID: "Q-1", QuoteRevision: 3, AllocatedAt: s.AllocatedAt}
	if s != want || !s.AllocatedAt.Equal(f.at.Add(time.Hour)) {
		t.Fatalf("snapshot %+v", s)
	}
	if _, err = f.module.GetObligationSnapshot(ctx, api.GetObligationSnapshot{TenantID: f.tenant, ObligationID: "OB-1", Version: 2}); !errors.Is(err, api.ErrNotFound) {
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

func TestOrderCommitmentCannotBeChangedInPostgres(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.module.RecordCustomerOrder(ctx, f.record("O-1", "Q-1", twoLines...)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.AllocateOrderToObligation(ctx, f.allocate("O-1", "allocate-1", 1, 1, "OB-1", kg(200))); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ sql, state, why string }{
		{`UPDATE cargo_com.customer_order SET customer_legal_name='Renamed Corp' WHERE tenant_id=$1`, "23001", "the trigger freezes the commitment"},
		{`UPDATE cargo_com.customer_order SET status='RECORDED' WHERE tenant_id=$1`, "23001", "the status only moves forward"},
		{`UPDATE cargo_com.order_line SET quantity=1 WHERE tenant_id=$1`, "42501", "the application role cannot change lines"},
		{`DELETE FROM cargo_com.fulfillment_obligation WHERE tenant_id=$1`, "42501", "the application role cannot delete obligations"},
		{`DELETE FROM cargo_com.customer_order WHERE tenant_id=$1`, "42501", "the application role cannot delete orders"},
		{`INSERT INTO cargo_com.fulfillment_obligation (tenant_id,obligation_id,obligation_version,order_id,line_no,unit_code,quantity,allocated_by,allocated_at)
          VALUES ($1,'OB-X',1,'O-1',1,'KGM',301,'rogue',now())`, "23514", "the line has 300 left"},
		{`INSERT INTO cargo_com.fulfillment_obligation (tenant_id,obligation_id,obligation_version,order_id,line_no,unit_code,quantity,allocated_by,allocated_at)
          VALUES ($1,'OB-Y',1,'O-1',1,'H87',1,'rogue',now())`, "23514", "the line is in KGM"},
	} {
		err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			_, err := e.Exec(ctx, tc.sql, f.tenant)
			return err
		})
		if sqlState(err) != tc.state {
			t.Errorf("%s: err = %v, want SQLSTATE %s (%s)", tc.sql, err, tc.state, tc.why)
		}
	}
	if s, err := f.module.GetObligationSnapshot(ctx, api.GetObligationSnapshot{TenantID: f.tenant, ObligationID: "OB-1", Version: 1}); err != nil || s.Quantity != kg(200) {
		t.Fatalf("snapshot after the attempts: %+v, %v", s, err)
	}
	if r, err := f.module.AllocateOrderToObligation(ctx, f.allocate("O-1", "allocate-2", 2, 1, "OB-2", kg(300))); err != nil || r.NewRevision != 3 {
		t.Fatalf("the order still allocates normally: %+v, %v", r, err)
	}
}

// TestConcurrentAllocationsNeverExceedTheLine races allocations for one order line. Against the same
// revision exactly one commits; retrying with fresh revisions, allocations stop when the line is full.
func TestConcurrentAllocationsNeverExceedTheLine(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.module.RecordCustomerOrder(ctx, f.record("O-1", "Q-1", api.OrderLine{Description: "Bulk resin", Quantity: kg(10)})); err != nil {
		t.Fatal(err)
	}
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
		_, err := f.module.AllocateOrderToObligation(ctx, f.allocate("O-1", fmt.Sprintf("first-%d", i), 1, 1, fmt.Sprintf("OB-A%d", i), kg(4)))
		return err
	})
	if count(first, nil) != 1 || count(first, api.ErrConflict) != workers-1 {
		t.Fatalf("same revision: %d committed, %d conflicts; errors %v", count(first, nil), count(first, api.ErrConflict), first)
	}
	revision := func() (rev int64) {
		if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			return e.QueryRow(ctx, `SELECT revision FROM cargo_com.customer_order WHERE tenant_id=$1 AND order_id='O-1'`, f.tenant).Scan(&rev)
		}); err != nil {
			t.Error(err)
		}
		return rev
	}
	second := race(func(i int) error {
		for try := 0; try < 50; try++ {
			_, err := f.module.AllocateOrderToObligation(ctx, f.allocate("O-1", fmt.Sprintf("second-%d-%d", i, try), revision(), 1, fmt.Sprintf("OB-B%d-%d", i, try), kg(4)))
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
		return e.QueryRow(ctx, `SELECT coalesce(sum(quantity),0), count(*), (SELECT revision FROM cargo_com.customer_order WHERE tenant_id=$1 AND order_id='O-1')
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
		t.Fatalf("allocated %d KGM in %d obligations at revision %d with %d events, want 8, 2, 3, 2", allocated, obligations, rev, events)
	}
}

func TestOrderRefusalsOnPostgres(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	line := api.OrderLine{Description: "Pallets of machine parts", Quantity: kg(500)}
	f.rate.refuse = rateapi.ErrRejected
	if _, err := f.module.RecordCustomerOrder(ctx, f.record("O-1", "Q-1", line)); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("quote not accepted by RATE: err = %v", err)
	}
	if _, err := f.module.AllocateOrderToObligation(ctx, f.allocate("O-1", "allocate-0", 1, 1, "OB-0", kg(1))); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("a refused order must not be stored: err = %v", err)
	}
	f.rate.refuse, f.org.refuse = nil, orgapi.ErrRejected
	if _, err := f.module.RecordCustomerOrder(ctx, f.record("O-1", "Q-1", line)); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("customer not confirmed by ORG: err = %v", err)
	}
	f.org.refuse = nil
	if _, err := f.module.RecordCustomerOrder(ctx, f.record("O-1", "Q-1", line)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.AllocateOrderToObligation(ctx, f.allocate("O-1", "allocate-1", 1, 1, "OB-1", kg(100))); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.RecordCustomerOrder(ctx, f.record("O-3", "Q-3", line)); err != nil {
		t.Fatal(err)
	}
	otherCustomer := f.record("O-4", "Q-4", line)
	otherCustomer.CustomerPartyID = "P-2"
	sameOrder := f.record("O-1", "Q-2", line)
	sameOrder.CommandID = "record-O-1-again"
	sameQuote := f.record("O-2", "Q-1", line)
	intruder := f.record("O-5", "Q-5", line)
	intruder.ActorID = "intruder"
	for name, tc := range map[string]struct {
		run  func() error
		want error
	}{
		"the quote's customer is another party": {func() error { _, err := f.module.RecordCustomerOrder(ctx, otherCustomer); return err }, api.ErrRejected},
		"same order id under a new command":     {func() error { _, err := f.module.RecordCustomerOrder(ctx, sameOrder); return err }, api.ErrConflict},
		"a second order for the same quote":     {func() error { _, err := f.module.RecordCustomerOrder(ctx, sameQuote); return err }, api.ErrConflict},
		"obligation id used by another order": {func() error {
			_, err := f.module.AllocateOrderToObligation(ctx, f.allocate("O-3", "allocate-3", 1, 1, "OB-1", kg(1)))
			return err
		}, api.ErrConflict},
		"actor refused by the authorizer": {func() error { _, err := f.module.RecordCustomerOrder(ctx, intruder); return err }, api.ErrUnauthorized},
		"obligation of another tenant": {func() error {
			_, err := f.module.GetObligationSnapshot(ctx, api.GetObligationSnapshot{TenantID: f.tenant + "-other", ObligationID: "OB-1", Version: 1})
			return err
		}, api.ErrNotFound},
	} {
		if err := tc.run(); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}
