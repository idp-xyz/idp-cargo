//go:build integration

// Package integration runs FIN against PostgreSQL 16 with stand-ins for FUL's CostDriver v1, RATE's PricingBasis v1,
// DOC's TransportDocumentSnapshot v1 and ORG. The real contexts together are tested by the composition root.
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
	docapi "go.idp.xyz/idp-cargo/internal/contexts/doc/api"
	finmodule "go.idp.xyz/idp-cargo/internal/contexts/fin"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
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

func cny(n int64) kernel.Money { return kernel.Money{Currency: "CNY", Minor: n} }
func usd(n int64) kernel.Money { return kernel.Money{Currency: "USD", Minor: n} }

// world stands in for FUL, RATE, DOC and ORG.
type world struct {
	mu         sync.Mutex
	drivers    map[string]fulapi.CostDriver
	bases      map[string]rateapi.PricingBasis
	roles      map[string][]string // party -> roles held for LE
	identities map[string]orgapi.PartyIdentity
	documents  map[string]docapi.TransportDocumentSnapshot   // shipment -> valid house document, at its valid version
	earlier    map[string][]docapi.TransportDocumentSnapshot // shipment -> earlier issued versions of that document, oldest first
	costBases  map[string]phyapi.PhysicalCostBasis           // consolidation -> PhysicalCostBasis v1
	suspended  bool
	// RATE's rated bases (V0.5 part 2): what the stand-in rates a shipment and a consolidation into, the bases it
	// issued by id, the requests it was asked, and a refusal to answer with
	shipmentLines []rateapi.PricingLine
	consolLines   []rateapi.PricingLine
	rated         map[string]rateapi.PricingBasis
	asked         []any
	rateRefusal   error
}

func (w *world) issue(b rateapi.PricingBasis, asked any) (rateapi.Receipt, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.asked = append(w.asked, asked)
	if w.rateRefusal != nil {
		return rateapi.Receipt{}, fmt.Errorf("%w: stub: %v", rateapi.ErrRejected, w.rateRefusal)
	}
	if w.rated == nil {
		w.rated = map[string]rateapi.PricingBasis{}
	}
	if _, ok := w.rated[b.BasisID]; !ok {
		w.rated[b.BasisID] = b
	}
	return rateapi.Receipt{SubjectID: b.BasisID, NewRevision: 1}, nil
}

func (w *world) RateShipmentEstimate(_ context.Context, c rateapi.RateShipmentEstimate) (rateapi.Receipt, error) {
	return w.issue(rateapi.PricingBasis{TenantID: c.TenantID, BasisID: c.BasisID, Purpose: rateapi.BasisShipmentEstimate, LegalEntityID: c.LegalEntityID,
		Customer: rateapi.CustomerRef{PartyID: c.CustomerPartyID, PartyRevision: 3}, SubjectKind: "SHIPMENT", SubjectID: c.ShipmentID,
		SubjectVersion: c.PlanVersion, SnapshotID: c.BasisID, Lines: w.shipmentLines,
		Gaps: []rateapi.PricingGap{{Side: rateapi.SideSell, ChargeCode: "CUSTOMS_CLEARING", Gap: rateapi.GapCallForPricing}}}, c)
}

func (w *world) RateConsolCost(_ context.Context, c rateapi.RateConsolCost) (rateapi.Receipt, error) {
	return w.issue(rateapi.PricingBasis{TenantID: c.TenantID, BasisID: c.BasisID, Purpose: rateapi.BasisConsolCostEstimate, LegalEntityID: c.LegalEntityID,
		SubjectKind: "CONSOLIDATION", SubjectID: c.ConsolidationID, SubjectVersion: c.ConsolidationRevision, SnapshotID: c.BasisID, Lines: w.consolLines}, c)
}

func (w *world) GetPhysicalCostBasis(_ context.Context, q phyapi.GetPhysicalCostBasis) (phyapi.PhysicalCostBasis, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	b, ok := w.costBases[q.ConsolidationID]
	if !ok {
		return phyapi.PhysicalCostBasis{}, fmt.Errorf("%w: stub", phyapi.ErrNotFound)
	}
	return b, nil
}

func (w *world) GetPartyIdentity(_ context.Context, q orgapi.GetPartyIdentity) (orgapi.PartyIdentity, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	id, ok := w.identities[q.PartyID]
	if !ok {
		return orgapi.PartyIdentity{}, fmt.Errorf("%w: stub", orgapi.ErrNotFound)
	}
	return id, nil
}

func (w *world) GetHouseDocumentSnapshot(_ context.Context, q docapi.GetHouseDocumentSnapshot) (docapi.TransportDocumentSnapshot, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	d, ok := w.documents[q.ShipmentID]
	if !ok {
		return docapi.TransportDocumentSnapshot{}, fmt.Errorf("%w: stub", docapi.ErrNotFound)
	}
	if q.At.IsZero() {
		return d, nil
	}
	// as DOC: the version of the valid document issued last at or before At
	var found *docapi.TransportDocumentSnapshot
	for _, v := range append(slices.Clone(w.earlier[q.ShipmentID]), d) {
		if !v.IssuedAt.After(q.At) {
			found = &v
		}
	}
	if found == nil {
		return docapi.TransportDocumentSnapshot{}, fmt.Errorf("%w: stub, none issued by then", docapi.ErrNotFound)
	}
	return *found, nil
}

func (w *world) GetShipmentCostDriver(_ context.Context, q fulapi.GetShipmentCostDriver) (fulapi.CostDriver, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	d, ok := w.drivers[q.ShipmentID]
	if !ok {
		return fulapi.CostDriver{}, fmt.Errorf("%w: stub", fulapi.ErrNotFound)
	}
	return d, nil
}

func (w *world) GetPricingBasis(_ context.Context, q rateapi.GetPricingBasis) (rateapi.PricingBasis, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if q.BasisID != "" {
		b, ok := w.rated[q.BasisID]
		if !ok {
			return rateapi.PricingBasis{}, fmt.Errorf("%w: stub", rateapi.ErrNotFound)
		}
		return b, nil
	}
	b, ok := w.bases[q.QuoteID]
	if !ok {
		return rateapi.PricingBasis{}, fmt.Errorf("%w: stub", rateapi.ErrRejected)
	}
	return b, nil
}

func (w *world) ResolvePartyRole(_ context.Context, q orgapi.ResolvePartyRole) (orgapi.PartyRole, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if q.LegalEntityID != "LE" || !slices.Contains(w.roles[q.PartyID], q.RoleCode) {
		return orgapi.PartyRole{}, fmt.Errorf("%w: stub", orgapi.ErrRejected)
	}
	return orgapi.PartyRole{Ref: orgapi.PartyRef{TenantID: q.TenantID, PartyID: q.PartyID, PartyRevision: 4}, RoleCode: q.RoleCode}, nil
}

func (w *world) GetLegalEntity(_ context.Context, q orgapi.GetLegalEntity) (orgapi.LegalEntityRef, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	status := orgapi.LegalEntityVerified
	if w.suspended || q.LegalEntityID != "LE" {
		status = orgapi.LegalEntitySuspended
	}
	return orgapi.LegalEntityRef{TenantID: q.TenantID, LegalEntityID: q.LegalEntityID, Revision: 1, RegisteredName: "LE", Jurisdiction: "CN",
		Status: status}, nil
}

type allow struct{}

func (allow) AuthorizeFinAction(_ context.Context, _, actor, action, _ string) error {
	if actor == "intruder" || (action == api.ActionApproveOwnCostVariance && actor != "controller") {
		return errors.New("forbidden")
	}
	return nil
}

type fixture struct {
	module *finmodule.Module
	db     *bentopg.DB
	w      *world
	tenant string
	at     time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	at := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Microsecond)
	w := &world{drivers: map[string]fulapi.CostDriver{}, bases: map[string]rateapi.PricingBasis{},
		roles: map[string][]string{"party-a": {"CUSTOMER"}, "agent-ny": {"OVERSEAS_AGENT"}, "airline-mu": {"CARRIER"}, "trucker": {"CO_LOADER"}},
		identities: map[string]orgapi.PartyIdentity{
			"party-a": {PartyID: "party-a", Revision: 4, Status: "ACTIVE", LegalName: "上海甲贸易有限公司", EnglishName: "Shanghai Jia Trading Co., Ltd.",
				CountryCode: "CN", Identifiers: []orgapi.PartyIdentifier{{Type: orgapi.IdentifierCNUSCC, Country: "CN", Value: "91310000MA1FL8TQ2X"}}},
			"agent-ny":   {PartyID: "agent-ny", Revision: 2, Status: "ACTIVE", LegalName: "NY Forwarding Inc.", CountryCode: "US"},
			"airline-mu": {PartyID: "airline-mu", Revision: 4, Status: "ACTIVE", LegalName: "China Eastern Airlines Corporation Limited", CountryCode: "CN"},
			"trucker":    {PartyID: "trucker", Revision: 4, Status: "ACTIVE", LegalName: "上海乙运输有限公司", CountryCode: "CN"},
		},
		documents: map[string]docapi.TransportDocumentSnapshot{}, earlier: map[string][]docapi.TransportDocumentSnapshot{},
		costBases: map[string]phyapi.PhysicalCostBasis{}}
	f := &fixture{db: testDB(t), w: w, at: at, tenant: "cargo-fin-" + time.Now().UTC().Format("20060102T150405.000000000")}
	m, err := finmodule.New(f.db, w, w, w, w, w, allow{})
	if err != nil {
		t.Fatal(err)
	}
	f.module = m
	for _, s := range []string{"SH-A", "SH-B"} {
		w.drivers[s] = fulapi.CostDriver{TenantID: f.tenant, ShipmentID: s, Kind: fulapi.DriverShipmentPlanned, Revision: 3, Status: fulapi.StatusPlanned,
			PlanVersion: 2, TransportMode: "AIR", LegalEntityID: "LE", CustomerPartyID: "party-a", Packs: 6,
			Orders: []fulapi.CostOrder{{OrderID: "O-A", QuoteID: "Q-1", QuoteRevision: 2, Packs: 6}}}
	}
	// SH-A also holds a part of O-B, which Q-1 backs too (a quote may back several orders)
	a := w.drivers["SH-A"]
	a.Orders = append(a.Orders, fulapi.CostOrder{OrderID: "O-B", QuoteID: "Q-1", QuoteRevision: 2, Packs: 2})
	w.drivers["SH-A"] = a
	w.bases["Q-1"] = rateapi.PricingBasis{TenantID: f.tenant, BasisID: "QUOTE:Q-1@2", Purpose: rateapi.BasisQuoteAccepted, QuoteID: "Q-1", QuoteRevision: 2,
		LegalEntityID: "LE", Customer: rateapi.CustomerRef{PartyID: "party-a", PartyRevision: 3}, Currency: "USD",
		Lines: []rateapi.PricingLine{{LineNo: 1, ChargeCode: "AIR_FREIGHT", Sell: usd(120_000), Cost: usd(95_000)}, {LineNo: 2, ChargeCode: "DOC_FEE", Sell: cny(30_000)}}}
	return f
}

func (f *fixture) hours(h int) time.Time { return f.at.Add(time.Duration(h) * time.Hour) }

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

func sqlState(err error) (string, string) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code, pgErr.ConstraintName
	}
	return "", ""
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
		types = append(types, strings.TrimSuffix(kind, ".v1"))
	}
	return types
}

// A legal entity's policy and rates, the job of SH-A priced from the accepted quote and by hand, its profit, the quote
// refused on a second job of the order, credit terms, a hold, CreditStatus v1, and what the database refuses past FIN.
func TestFreightJobsAndCreditOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	policy := api.OpenFinancePolicy{CommandID: "policy", TenantID: f.tenant, ActorID: "acct-1", LegalEntityID: "LE", FunctionalCurrency: "CNY", BookCode: "MAIN",
		OccurredAt: f.hours(0)}
	if _, err := f.module.OpenFinancePolicy(ctx, api.OpenFinancePolicy{CommandID: "policy-x", TenantID: f.tenant, ActorID: "acct-1", LegalEntityID: "LE-X",
		FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: f.hours(0)}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a legal entity not VERIFIED: %v", err)
	}
	r, err := f.module.OpenFinancePolicy(ctx, policy)
	if err != nil || r.NewRevision != 1 {
		t.Fatalf("policy: %+v %v", r, err)
	}
	if again, err := f.module.OpenFinancePolicy(ctx, policy); err != nil || again != r {
		t.Fatalf("replay: %+v %v", again, err)
	}
	rate := api.RecordExchangeRate{CommandID: "rate-usd", TenantID: f.tenant, ActorID: "acct-1", LegalEntityID: "LE", ExpectedRevision: 1, RateType: api.RateEstimate,
		From: "USD", Rate: 71_234, Scale: 4, FromDigits: 2, ToDigits: 2, ValidFrom: f.at.AddDate(0, 0, -1), ValidUntil: f.at.AddDate(0, 1, 0),
		Source: "CFETS central parity", OccurredAt: f.hours(0)}
	if r, err = f.module.RecordExchangeRate(ctx, rate); err != nil || r.Version != 1 {
		t.Fatalf("rate: %+v %v", r, err)
	}
	overlap := rate
	overlap.CommandID, overlap.ExpectedRevision, overlap.ValidFrom, overlap.ValidUntil = "rate-overlap", 2, f.at, f.at.AddDate(0, 2, 0)
	if _, err = f.module.RecordExchangeRate(ctx, overlap); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "FIN_RATE_OVERLAP") {
		t.Fatalf("an overlapping rate: %v", err)
	}
	openEnded := overlap
	openEnded.CommandID, openEnded.RateType, openEnded.ValidUntil = "rate-open", api.RateAccounting, time.Time{}
	if _, err = f.module.RecordExchangeRate(ctx, openEnded); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a rate without an end: %v", err)
	}
	wrong := rate
	wrong.CommandID, wrong.ExpectedRevision, wrong.RateType, wrong.Rate = "rate-wrong", 2, api.RateAccounting, 17_234
	if _, err = f.module.RecordExchangeRate(ctx, wrong); err != nil {
		t.Fatal(err)
	}
	if r, err = f.module.EndExchangeRate(ctx, api.EndExchangeRate{CommandID: "rate-end", TenantID: f.tenant, ActorID: "acct-1", LegalEntityID: "LE",
		ExpectedRevision: 3, EntryNo: 2, ValidUntil: f.at, Reason: "typed 1.7234 for 7.1234", OccurredAt: f.hours(0)}); err != nil || r.Version != 2 || r.NewRevision != 4 {
		t.Fatalf("a wrong entry ended: %+v %v", r, err)
	}

	open := api.OpenFreightJob{CommandID: "open-a", TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-A", ShipmentID: "SH-A", OccurredAt: f.hours(1)}
	if r, err = f.module.OpenFreightJob(ctx, open); err != nil || r.NewRevision != 1 {
		t.Fatalf("open: %+v %v", r, err)
	}
	second := open
	second.CommandID, second.JobID = "open-a2", "JOB-A2"
	if _, err = f.module.OpenFreightJob(ctx, second); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "FIN_NOT_OPENABLE") {
		t.Fatalf("a second job of the shipment and legal entity: %v", err)
	}
	apply := func(command, job string, revision int64) (api.Receipt, error) {
		return f.module.ApplyPricingBasis(ctx, api.ApplyPricingBasis{CommandID: command, TenantID: f.tenant, ActorID: "fin-1", JobID: job, ExpectedRevision: revision,
			QuoteID: "Q-1", OrderID: "O-A", OccurredAt: f.hours(2)})
	}
	if r, err = apply("apply-a", "JOB-A", 1); err != nil || r.NewRevision != 2 || r.Version != 1 {
		t.Fatalf("apply: %+v %v", r, err)
	}
	if _, err = f.module.ApplyPricingBasis(ctx, api.ApplyPricingBasis{CommandID: "apply-a-ob", TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-A",
		ExpectedRevision: 2, QuoteID: "Q-1", OrderID: "O-B", OccurredAt: f.hours(2)}); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "FIN_BASIS_APPLIED") {
		t.Fatalf("the quote once more on the job for another order: %v", err)
	}
	if _, err = f.module.EndExchangeRate(ctx, api.EndExchangeRate{CommandID: "rate-end-used", TenantID: f.tenant, ActorID: "acct-1", LegalEntityID: "LE",
		ExpectedRevision: 4, EntryNo: 1, ValidUntil: f.hours(1), Reason: "x", OccurredAt: f.hours(2)}); !errors.Is(err, api.ErrRejected) ||
		!strings.Contains(err.Error(), "FIN_RATE_IN_USE") {
		t.Fatalf("an end before a charge converted at the entry: %v", err)
	}
	if _, err = f.module.OpenFreightJob(ctx, api.OpenFreightJob{CommandID: "open-b", TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-B", ShipmentID: "SH-B",
		OccurredAt: f.hours(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err = apply("apply-b", "JOB-B", 1); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "FIN_BASIS_APPLIED") {
		t.Fatalf("the order's quote on a second job: %v", err)
	}
	line := api.RecordChargeLine{CommandID: "line-truck", TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-A", ExpectedRevision: 2, ChargeCode: "TRUCKING",
		Description: "pickup", Sell: &api.ChargeSide{PartyID: "party-a", Amount: cny(80_000)}, Cost: &api.ChargeSide{PartyID: "trucker", Amount: cny(60_000)},
		Reason: "pickup booked after the quote", OccurredAt: f.hours(3)}
	stranger := line
	stranger.CommandID, stranger.Sell = "line-stranger", &api.ChargeSide{PartyID: "airline-mu", Amount: cny(1)}
	if _, err = f.module.RecordChargeLine(ctx, stranger); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "FIN_PARTY_ROLE") {
		t.Fatalf("a debtor without a billing role: %v", err)
	}
	if r, err = f.module.RecordChargeLine(ctx, line); err != nil || r.Version != 3 {
		t.Fatalf("manual line: %+v %v", r, err)
	}
	if r, err = f.module.ReviseChargeSide(ctx, api.ReviseChargeSide{CommandID: "revise-freight", TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-A",
		ExpectedRevision: 3, LineNo: 1, Side: api.SideCost, ExpectedSideVersion: 1, Value: api.ChargeSide{PartyID: "airline-mu", Amount: usd(96_000)},
		Reason: "MU's rate for the flight", OccurredAt: f.hours(4)}); err != nil || r.Version != 2 {
		t.Fatalf("revise: %+v %v", r, err)
	}
	if _, err = f.module.ReviseChargeSide(ctx, api.ReviseChargeSide{CommandID: "revise-truck", TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-A",
		ExpectedRevision: 4, LineNo: 3, Side: api.SideCost, ExpectedSideVersion: 1, Value: api.ChargeSide{Amount: cny(65_000)},
		Reason: "waiting time at the factory", OccurredAt: f.hours(4)}); err != nil {
		t.Fatal(err)
	}
	j, err := f.module.GetFreightJob(ctx, api.GetFreightJob{TenantID: f.tenant, JobID: "JOB-A"})
	if err != nil || j.FunctionalCurrency != "CNY" || j.BookCode != "MAIN" || j.CustomerPartyID != "party-a" || len(j.Bases) != 1 || len(j.Lines) != 3 ||
		j.Lines[0].Sell[0].Local != cny(854_808) || j.Lines[0].Sell[0].FX == nil || j.Lines[0].Sell[0].FX.Rate != 71_234 || j.Lines[0].Cost[1].PartyID != "airline-mu" ||
		j.Lines[0].Cost[1].Reason != "MU's rate for the flight" || j.Lines[1].Sell[0].FX != nil || j.Lines[2].Source != api.SourceManual ||
		j.Lines[2].Cost[1].PartyID != "trucker" || j.Lines[2].Cost[1].Amount != cny(65_000) {
		t.Fatalf("job: %+v %v", j, err)
	}
	profit, err := f.module.GetJobProfit(ctx, api.GetJobProfit{TenantID: f.tenant, JobID: "JOB-A"})
	if err != nil || profit.Revenue != cny(964_808) || profit.Cost != cny(748_846) || profit.Profit != cny(215_962) || len(profit.ByCode) != 3 {
		t.Fatalf("profit: %+v %v", profit, err)
	}
	if jobs, err := f.module.ListJobsForShipment(ctx, api.ListJobsForShipment{TenantID: f.tenant, ShipmentID: "SH-A"}); err != nil || len(jobs) != 1 {
		t.Fatalf("jobs of SH-A: %d %v", len(jobs), err)
	}
	// cancelling JOB-A frees the quote for JOB-B
	if _, err = f.module.CancelFreightJob(ctx, api.CancelFreightJob{CommandID: "cancel-a", TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-A",
		ExpectedRevision: 5, Reason: "opened on the wrong shipment", OccurredAt: f.hours(5)}); err != nil {
		t.Fatal(err)
	}
	if _, err = apply("apply-b2", "JOB-B", 1); err != nil {
		t.Fatalf("the quote after the first job's cancellation: %v", err)
	}
	if _, err = f.module.RecordChargeLine(ctx, api.RecordChargeLine{CommandID: "line-agent", TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-B",
		ExpectedRevision: 2, ChargeCode: "DEST_HANDLING", Sell: &api.ChargeSide{PartyID: "agent-ny", Amount: cny(20_000)}, Reason: "collected at destination",
		OccurredAt: f.hours(5)}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.module.OpenFreightJob(ctx, api.OpenFreightJob{CommandID: "open-a3", TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-A3", ShipmentID: "SH-A",
		OccurredAt: f.hours(6)}); err != nil {
		t.Fatalf("a new job once the first is cancelled: %v", err)
	}

	// credit
	terms := api.SetCreditTerms{CommandID: "terms", TenantID: f.tenant, ActorID: "credit-1", LegalEntityID: "LE", DebtorPartyID: "party-a", Limit: cny(500_000),
		PaymentDays: 30, PaymentBasis: api.TermsFromMonthEnd, OccurredAt: f.hours(6)}
	status := func() api.CreditStatus {
		t.Helper()
		s, err := f.module.GetCreditStatus(ctx, api.GetCreditStatus{TenantID: f.tenant, LegalEntityID: "LE", DebtorPartyID: "party-a"})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if s := status(); s.Status != api.CreditNoAccount || s.DecisionID != "" {
		t.Fatalf("before an account: %+v", s)
	}
	if r, err = f.module.SetCreditTerms(ctx, terms); err != nil || r.NewRevision != 1 || r.SubjectID != "LE/party-a" {
		t.Fatalf("terms: %+v %v", r, err)
	}
	if s := status(); s.Status != api.CreditOK || s.DecisionID != "LE/party-a@1" {
		t.Fatalf("an account: %+v", s)
	}
	account, err := f.module.GetCreditAccount(ctx, api.GetCreditAccount{TenantID: f.tenant, LegalEntityID: "LE", DebtorPartyID: "party-a"})
	if err != nil || account.Exposure != cny(884_808) || !account.OverLimit {
		t.Fatalf("exposure of the open job JOB-B (USD 1200 + CNY 300): %+v %v", account, err)
	}
	if _, err = f.module.PlaceCreditHold(ctx, api.PlaceCreditHold{CommandID: "hold", TenantID: f.tenant, ActorID: "credit-1", LegalEntityID: "LE",
		DebtorPartyID: "party-a", ExpectedRevision: 1, Code: api.HoldOverLimit, Reason: "over the limit on the daily review", OccurredAt: f.hours(7)}); err != nil {
		t.Fatal(err)
	}
	if s := status(); s.Status != api.CreditOnHold || s.HoldCode != api.HoldOverLimit || s.DecisionID != "LE/party-a@2" {
		t.Fatalf("held: %+v", s)
	}
	if list, err := f.module.ListCreditExposure(ctx, api.ListCreditExposure{TenantID: f.tenant, LegalEntityID: "LE"}); err != nil || len(list) != 2 ||
		list[0].Status != api.CreditOnHold || list[1].DebtorPartyID != "agent-ny" || list[1].Status != api.CreditNoAccount || list[1].Exposure != cny(20_000) {
		t.Fatalf("exposure list: %+v %v", list, err)
	}
	if _, err = f.module.ReleaseCreditHold(ctx, api.ReleaseCreditHold{CommandID: "release", TenantID: f.tenant, ActorID: "credit-2", LegalEntityID: "LE",
		DebtorPartyID: "party-a", ExpectedRevision: 2, Reason: "limit raised by the CFO", OccurredAt: f.hours(8)}); err != nil {
		t.Fatal(err)
	}
	if s := status(); s.Status != api.CreditOK || s.Revision != 3 {
		t.Fatalf("released: %+v", s)
	}
	if _, err = f.module.SetCreditTerms(ctx, api.SetCreditTerms{CommandID: "terms-stranger", TenantID: f.tenant, ActorID: "credit-1", LegalEntityID: "LE",
		DebtorPartyID: "airline-mu", Limit: cny(1), PaymentDays: 1, PaymentBasis: api.TermsFromInvoiceDate, OccurredAt: f.hours(8)}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("an account for a party without a debtor role: %v", err)
	}
	want := []string{"FinancePolicyOpened", "ExchangeRateRecorded", "ExchangeRateRecorded", "ExchangeRateEnded", "FreightJobOpened", "ChargeLineRecorded",
		"ChargeLineRecorded", "PricingBasisApplied", "FreightJobOpened", "ChargeLineRecorded", "ChargeSideRevised", "ChargeSideRevised", "FreightJobCancelled",
		"ChargeLineRecorded", "ChargeLineRecorded", "PricingBasisApplied", "ChargeLineRecorded", "FreightJobOpened", "CreditTermsSet", "CreditHoldPlaced",
		"CreditHoldReleased"}
	if got := f.outboxTypes(t); !slices.Equal(got, want) {
		t.Fatalf("outbox %v", got)
	}

	// past FIN on the application role
	for name, tc := range map[string]struct {
		sql, state, check string
	}{
		"functional currency changed": {`UPDATE cargo_fin.finance_policy SET functional_currency='USD' WHERE tenant_id=$1`, "42501|23001", ""},
		"rate rewritten":              {`UPDATE cargo_fin.exchange_rate_entry SET rate=1 WHERE tenant_id=$1`, "42501|23001", ""},
		"side rewritten":              {`UPDATE cargo_fin.charge_side_version SET amount_minor=1 WHERE tenant_id=$1`, "42501|23001", ""},
		"cancelled job reopened":      {`UPDATE cargo_fin.freight_job SET state='OPEN',cancel_reason=NULL,cancelled_by=NULL,cancelled_at=NULL,cancel_entered_at=NULL WHERE tenant_id=$1 AND job_id='JOB-A'`, "23001", ""},
		"basis made live again":       {`UPDATE cargo_fin.job_basis SET live=true WHERE tenant_id=$1 AND job_id='JOB-A'`, "23001", ""},
		"hold released twice":         {`UPDATE cargo_fin.credit_hold SET released_by='x' WHERE tenant_id=$1`, "23001", ""},
		"a side behind the job": {`INSERT INTO cargo_fin.charge_side_version (tenant_id,job_id,line_no,side,version_no,party_id,party_revision,currency,amount_minor,
            local_minor,status,reason,recorded_by,recorded_at,entered_at) VALUES ($1,'JOB-B',1,'SELL',9,'party-a',1,'CNY',1,1,'ESTIMATED','x','x',now(),now())`,
			"23514", "fin_records_with_parent"},
		"a local amount not converted at the policy's rate": {`UPDATE cargo_fin.freight_job SET revision=revision+1 WHERE tenant_id=$1 AND job_id='JOB-B';
            INSERT INTO cargo_fin.charge_side_version (tenant_id,job_id,line_no,side,version_no,party_id,party_revision,currency,amount_minor,local_minor,
            rate_entry_no,rate_type,rate,scale,from_digits,to_digits,rate_source,status,reason,recorded_by,recorded_at,entered_at)
            SELECT tenant_id,job_id,line_no,side,2,party_id,party_revision,currency,amount_minor,local_minor,rate_entry_no,rate_type,rate+1,scale,from_digits,
            to_digits,rate_source,status,'x','x',recorded_at,now() FROM cargo_fin.charge_side_version WHERE tenant_id=$1 AND job_id='JOB-B' AND line_no=1
            AND side='SELL'`, "23514", "freight_job_consistent"},
		"overlapping rates": {`UPDATE cargo_fin.finance_policy SET revision=revision+1 WHERE tenant_id=$1;
            INSERT INTO cargo_fin.exchange_rate_entry (tenant_id,legal_entity_id,entry_no,rate_type,from_currency,rate,scale,from_digits,to_digits,valid_from,
            valid_until,source,recorded_by,recorded_at,entered_at) VALUES ($1,'LE',3,'ESTIMATE','USD',70000,4,2,2,now(),now()+interval '10 days','x','x',now(),now())`,
			"23514", "finance_policy_consistent"},
		"an account on hold without a hold": {`UPDATE cargo_fin.credit_account SET state='ON_HOLD' WHERE tenant_id=$1`, "23514", "credit_account_consistent"},
		"a local amount not its conversion": {`UPDATE cargo_fin.freight_job SET revision=revision+1 WHERE tenant_id=$1 AND job_id='JOB-B';
            INSERT INTO cargo_fin.charge_side_version (tenant_id,job_id,line_no,side,version_no,party_id,party_revision,currency,amount_minor,local_minor,
            rate_entry_no,rate_type,rate,scale,from_digits,to_digits,rate_source,status,reason,recorded_by,recorded_at,entered_at)
            SELECT tenant_id,job_id,line_no,side,2,party_id,party_revision,currency,amount_minor,local_minor*1000,rate_entry_no,rate_type,rate,scale,from_digits,
            to_digits,rate_source,status,'x','x',recorded_at,now() FROM cargo_fin.charge_side_version WHERE tenant_id=$1 AND job_id='JOB-B' AND line_no=1
            AND side='SELL'`, "23514", "freight_job_consistent"},
		"a rate entry ended twice": {`UPDATE cargo_fin.exchange_rate_entry SET end_reason='again' WHERE tenant_id=$1 AND entry_no=2`, "23001", ""},
		"a rate entry lengthened":  {`UPDATE cargo_fin.exchange_rate_entry SET valid_until=valid_until+interval '1 day' WHERE tenant_id=$1 AND entry_no=1`, "23001", ""},
		"a rate rewritten on its end": {`UPDATE cargo_fin.exchange_rate_entry SET rate=1,valid_until=valid_until-interval '1 day',ended_by='x',ended_at=now(),
            end_reason='x',end_entered_at=now() WHERE tenant_id=$1 AND entry_no=1`, "42501", ""},
		"a second open job of the shipment": {`INSERT INTO cargo_fin.freight_job (tenant_id,job_id,legal_entity_id,shipment_id,kind,functional_currency,book_code,
            customer_party_id,plan_version,state,opened_by,opened_at,entered_at) VALUES ($1,'JOB-X','LE','SH-A','AIR_EXPORT_HOUSE','CNY','MAIN','party-a',2,'OPEN',
            'x',now(),now())`, "23505", "freight_job_one_per_shipment"},
		"a job deleted": {`DELETE FROM cargo_fin.freight_job WHERE tenant_id=$1 AND job_id='JOB-B'`, "42501|23001", ""},
	} {
		statements := strings.Split(tc.sql, ";\n")
		err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			for _, s := range statements {
				if _, err := e.Exec(ctx, s, f.tenant); err != nil {
					return err
				}
			}
			return nil
		})
		state, constraint := sqlState(err)
		if state == "" || !strings.Contains(tc.state, state) || tc.check != "" && constraint != tc.check {
			t.Errorf("%s: %v (state %s, constraint %s), want %s %s", name, err, state, constraint, tc.state, tc.check)
		}
	}
}

// TestJobsRaceForOneShipment opens eight jobs of one shipment at once: one commits.
func TestJobsRaceForOneShipment(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if _, err := f.module.OpenFinancePolicy(ctx, api.OpenFinancePolicy{CommandID: "policy", TenantID: f.tenant, ActorID: "acct-1", LegalEntityID: "LE",
		FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: f.hours(0)}); err != nil {
		t.Fatal(err)
	}
	const workers = 8
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = f.module.OpenFreightJob(ctx, api.OpenFreightJob{CommandID: fmt.Sprintf("open-%d", i), TenantID: f.tenant, ActorID: "fin-1",
				JobID: fmt.Sprintf("JOB-%d", i), ShipmentID: "SH-A", OccurredAt: f.hours(1)})
		}()
	}
	close(start)
	wg.Wait()
	committed, refused := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			committed++
		case errors.Is(err, api.ErrRejected) && strings.Contains(err.Error(), "FIN_NOT_OPENABLE"):
			refused++
		}
	}
	if committed != 1 || refused != workers-1 {
		t.Fatalf("%d committed, %d refused: %v", committed, refused, errs)
	}
}

// TestRateEndRacesAConversion ends a rate entry while a charge is converted at it, eight times: at most one of the two
// commits, and the job keeps taking changes.
func TestRateEndRacesAConversion(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if _, err := f.module.OpenFinancePolicy(ctx, api.OpenFinancePolicy{CommandID: "policy", TenantID: f.tenant, ActorID: "acct-1", LegalEntityID: "LE",
		FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: f.hours(0)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.OpenFreightJob(ctx, api.OpenFreightJob{CommandID: "open", TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-R", ShipmentID: "SH-B",
		OccurredAt: f.hours(1)}); err != nil {
		t.Fatal(err)
	}
	policyRevision, jobRevision := int64(1), int64(1)
	for i, currency := range []kernel.CurrencyCode{"EUR", "GBP", "HKD", "SGD", "AUD", "CAD", "CHF", "NZD"} {
		r, err := f.module.RecordExchangeRate(ctx, api.RecordExchangeRate{CommandID: fmt.Sprintf("rate-%d", i), TenantID: f.tenant, ActorID: "acct-1",
			LegalEntityID: "LE", ExpectedRevision: policyRevision, RateType: api.RateEstimate, From: currency, Rate: 50_000, Scale: 4, FromDigits: 2,
			ToDigits: 2, ValidFrom: f.at.AddDate(0, 0, -1), ValidUntil: f.at.AddDate(0, 1, 0), Source: "bank", OccurredAt: f.hours(1)})
		if err != nil {
			t.Fatal(err)
		}
		policyRevision = r.NewRevision
		var endErr, lineErr error
		var endR, lineR api.Receipt
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			endR, endErr = f.module.EndExchangeRate(ctx, api.EndExchangeRate{CommandID: fmt.Sprintf("end-%d", i), TenantID: f.tenant, ActorID: "acct-1",
				LegalEntityID: "LE", ExpectedRevision: policyRevision, EntryNo: r.Version, ValidUntil: f.hours(2 + 2*i), Reason: "corrected",
				OccurredAt: f.hours(3 + 2*i)})
		}()
		go func() {
			defer wg.Done()
			<-start
			lineR, lineErr = f.module.RecordChargeLine(ctx, api.RecordChargeLine{CommandID: fmt.Sprintf("line-%d", i), TenantID: f.tenant, ActorID: "fin-1",
				JobID: "JOB-R", ExpectedRevision: jobRevision, ChargeCode: "HANDLING", Sell: &api.ChargeSide{PartyID: "party-a",
					Amount: kernel.Money{Currency: currency, Minor: 10_000}}, Reason: "race", OccurredAt: f.hours(3 + 2*i)})
		}()
		close(start)
		wg.Wait()
		if endErr == nil && lineErr == nil {
			t.Fatalf("round %d: the rate was ended before a charge it converted after the end", i)
		}
		if endErr != nil && !strings.Contains(endErr.Error(), "FIN_RATE_IN_USE") || lineErr != nil && !strings.Contains(lineErr.Error(), "FIN_RATE_MISSING") {
			t.Fatalf("round %d: refused for another reason: %v / %v", i, endErr, lineErr)
		}
		if endErr == nil {
			policyRevision = endR.NewRevision
		}
		if lineErr == nil {
			jobRevision = lineR.NewRevision
		}
		t.Logf("round %d: end %v / conversion %v", i, endErr, lineErr)
		r2, err := f.module.RecordChargeLine(ctx, api.RecordChargeLine{CommandID: fmt.Sprintf("after-%d", i), TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-R",
			ExpectedRevision: jobRevision, ChargeCode: "DOC_FEE", Sell: &api.ChargeSide{PartyID: "party-a", Amount: cny(100)}, Reason: "after the race",
			OccurredAt: f.hours(4 + 2*i)})
		if err != nil {
			t.Fatalf("round %d: the job takes no change after the race: %v", i, err)
		}
		jobRevision = r2.NewRevision
	}
}
