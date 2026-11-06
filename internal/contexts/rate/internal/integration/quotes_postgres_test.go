//go:build integration

// Package integration runs RATE against PostgreSQL 16 with a stand-in for ORG's query side. The real
// ORG and RATE together are tested by the composition root.
package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	ratemodule "go.idp.xyz/idp-cargo/internal/contexts/rate"
	"go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	rate "go.idp.xyz/idp-cargo/internal/contexts/rate/internal/domain"
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

// orgStub answers ORG's party-role query: a fixed customer revision, or a refusal of the given kind.
type orgStub struct {
	revision int64
	refuse   error
}

func (o *orgStub) ResolvePartyRole(_ context.Context, q orgapi.ResolvePartyRole) (orgapi.PartyRole, error) {
	if o.refuse != nil {
		return orgapi.PartyRole{}, fmt.Errorf("%w: stub", o.refuse)
	}
	return orgapi.PartyRole{
		Ref:      orgapi.PartyRef{TenantID: q.TenantID, PartyID: q.PartyID, PartyRevision: o.revision, IdentitySource: orgapi.IdentitySource, LegalEntityScope: q.LegalEntityID},
		RoleCode: q.RoleCode,
	}, nil
}

// quotePolicy forbids the actor "intruder" and sets a 20% margin floor.
type quotePolicy struct{}

func (quotePolicy) AuthorizeQuoteAction(_ context.Context, _, actorID, _, _ string) error {
	if actorID == "intruder" {
		return errors.New("test policy: forbidden")
	}
	return nil
}

func (quotePolicy) MarginFloorBP(context.Context, string, string) (int, error) { return 2000, nil }

func usd(minor int64) kernel.Money { return kernel.Money{Currency: "USD", Minor: minor} }

type fixture struct {
	module *ratemodule.Module
	db     *bentopg.DB
	org    *orgStub
	tenant string
	at     time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{db: testDB(t), org: &orgStub{revision: 2}, tenant: "cargo-rate-" + time.Now().UTC().Format("20060102T150405.000000000"),
		at: time.Now().UTC().Truncate(time.Microsecond)}
	module, err := ratemodule.New(f.db, f.org, quotePolicy{}, quotePolicy{})
	if err != nil {
		t.Fatal(err)
	}
	f.module = module
	return f
}

func (f *fixture) create(id string, sell, cost int64) api.CreateQuote {
	return api.CreateQuote{CommandID: "create-" + id, TenantID: f.tenant, ActorID: "sales-1", QuoteID: id, LegalEntityID: "LE",
		CustomerPartyID: "P-1", Currency: "USD", Lines: []api.QuoteLine{{ChargeCode: "FREIGHT", Sell: usd(sell), Cost: usd(cost)}},
		Validity: kernel.ValidityInterval{From: f.at, Until: f.at.Add(14 * 24 * time.Hour)}, OccurredAt: f.at}
}

func (f *fixture) accept(id, command string, revision int64, at time.Time) api.AcceptQuote {
	return api.AcceptQuote{CommandID: command, TenantID: f.tenant, ActorID: "sales-1", QuoteID: id, ExpectedRevision: revision,
		EvidenceRef: "signed-quote", OccurredAt: at}
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

func TestQuoteLifecycleOnPostgres(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	create := f.create("Q-1", 100_000, 90_000)
	receipt, err := f.module.CreateQuote(ctx, create)
	if err != nil || receipt.NewRevision != 1 {
		t.Fatalf("create: %+v, %v", receipt, err)
	}
	if again, err := f.module.CreateQuote(ctx, create); err != nil || again != receipt {
		t.Fatalf("replay: %+v, %v", again, err)
	}
	if _, err = f.module.GetQuoteSnapshot(ctx, api.GetQuoteSnapshot{TenantID: f.tenant, QuoteID: "Q-1"}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("snapshot before acceptance: err = %v", err)
	}
	approve := api.ApproveQuotePrice{CommandID: "approve-1", TenantID: f.tenant, ActorID: "sales-1", QuoteID: "Q-1",
		ExpectedRevision: 1, ReasonRef: "margin-exception-1", OccurredAt: f.at.Add(time.Minute)}
	if _, err = f.module.ApproveQuotePrice(ctx, approve); !errors.Is(err, api.ErrRejected) || !errors.Is(err, rate.ErrSelfApproval) {
		t.Fatalf("author approving: err = %v", err)
	}
	approve.CommandID, approve.ActorID = "approve-2", "manager-1"
	if r, err := f.module.ApproveQuotePrice(ctx, approve); err != nil || r.NewRevision != 2 {
		t.Fatalf("approve: %+v, %v", r, err)
	}
	if _, err = f.module.AcceptQuote(ctx, f.accept("Q-1", "accept-1", 1, f.at.Add(time.Hour))); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("stale revision: err = %v", err)
	}
	f.org.revision = 3
	if r, err := f.module.AcceptQuote(ctx, f.accept("Q-1", "accept-2", 2, f.at.Add(time.Hour))); err != nil || r.NewRevision != 3 {
		t.Fatalf("accept: %+v, %v", r, err)
	}
	s, err := f.module.GetQuoteSnapshot(ctx, api.GetQuoteSnapshot{TenantID: f.tenant, QuoteID: "Q-1"})
	if err != nil {
		t.Fatal(err)
	}
	if s.QuoteRevision != 3 || s.Customer != (api.CustomerRef{PartyID: "P-1", PartyRevision: 3}) || s.Total != usd(100_000) ||
		!reflect.DeepEqual(s.Lines, []api.SnapshotLine{{ChargeCode: "FREIGHT", Amount: usd(100_000)}}) || s.PriceApprovedBy != "manager-1" ||
		s.AcceptanceEvidenceRef != "signed-quote" || !s.AcceptedAt.Equal(f.at.Add(time.Hour)) || !s.Validity.From.Equal(f.at) {
		t.Fatalf("snapshot %+v", s)
	}
	if got, want := f.outboxTypes(t), []string{api.QuotePriced, api.QuotePriceApproved, api.QuoteAccepted}; !reflect.DeepEqual(got, want) {
		t.Fatalf("outbox events %v, want %v", got, want)
	}
}

func TestAcceptedQuoteCannotBeChangedInPostgres(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.module.CreateQuote(ctx, f.create("Q-1", 100_000, 50_000)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.AcceptQuote(ctx, f.accept("Q-1", "accept-1", 1, f.at.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string) error {
		return (scope.TenantTransactor{DB: f.db, Tenant: f.tenant}).WithinTransaction(ctx, func(txCtx context.Context) error {
			e, err := f.db.RequireExecutor(txCtx)
			if err != nil {
				return err
			}
			_, err = e.Exec(txCtx, sql, f.tenant)
			return err
		})
	}
	for _, tc := range []struct{ sql, state, why string }{
		{`UPDATE cargo_rate.quote SET author='someone-else' WHERE tenant_id=$1`, "23001", "the trigger guards accepted quotes"},
		{`UPDATE cargo_rate.quote_line SET sell_minor=1 WHERE tenant_id=$1`, "42501", "the application role cannot change lines"},
		{`DELETE FROM cargo_rate.quote WHERE tenant_id=$1`, "42501", "the application role cannot delete quotes"},
	} {
		if err := exec(tc.sql); sqlState(err) != tc.state {
			t.Errorf("%s: err = %v, want SQLSTATE %s (%s)", tc.sql, err, tc.state, tc.why)
		}
	}
	if s, err := f.module.GetQuoteSnapshot(ctx, api.GetQuoteSnapshot{TenantID: f.tenant, QuoteID: "Q-1"}); err != nil || s.Total != usd(100_000) {
		t.Fatalf("snapshot after the attempts: %+v, %v", s, err)
	}
}

func TestQuoteRefusalsOnPostgres(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.org.refuse = orgapi.ErrRejected
	if _, err := f.module.CreateQuote(ctx, f.create("Q-1", 100_000, 50_000)); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("customer not confirmed by ORG: err = %v", err)
	}
	if _, err := f.module.GetQuoteSnapshot(ctx, api.GetQuoteSnapshot{TenantID: f.tenant, QuoteID: "Q-1"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("a refused quote must not be stored: err = %v", err)
	}
	f.org.refuse = nil
	if _, err := f.module.CreateQuote(ctx, f.create("Q-2", 100_000, 50_000)); err != nil {
		t.Fatal(err)
	}
	again := f.create("Q-2", 100_000, 50_000)
	again.CommandID = "create-Q-2-again"
	intruder := f.create("Q-3", 100_000, 50_000)
	intruder.ActorID = "intruder"
	for name, tc := range map[string]struct {
		run  func() error
		want error
	}{
		"same quote id under a new command": {func() error { _, err := f.module.CreateQuote(ctx, again); return err }, api.ErrConflict},
		"acceptance when the validity ends": {func() error {
			_, err := f.module.AcceptQuote(ctx, f.accept("Q-2", "accept-late", 1, f.at.Add(14*24*time.Hour)))
			return err
		}, api.ErrRejected},
		"actor refused by the authorizer": {func() error { _, err := f.module.CreateQuote(ctx, intruder); return err }, api.ErrUnauthorized},
		"quote of another tenant": {func() error {
			_, err := f.module.GetQuoteSnapshot(ctx, api.GetQuoteSnapshot{TenantID: f.tenant + "-other", QuoteID: "Q-2"})
			return err
		}, api.ErrNotFound},
	} {
		if err := tc.run(); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}
