//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	finmodule "go.idp.xyz/idp-cargo/internal/contexts/fin"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	finpg "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/adapters/postgres"
	scope "go.idp.xyz/idp-cargo/internal/foundation/bento"
)

// FIN V2.99, the cost review, on PostgreSQL 16 with stand-ins for the other contexts. JOB-A (revenue 1,000.00, cost
// 1,500.00) and JOB-B (revenue 800.00, cost 1,000.00) are losses, JOB-C (revenue 900.00, cost 500.00) is not.

// reviewWorld is the used houses' world with three jobs that have a sell and a cost line each. It answers the revisions
// of the jobs.
func reviewWorld(t *testing.T) (usedWorld, map[string]int64) {
	t.Helper()
	u := previewWorld(t)
	revision := map[string]int64{}
	for _, c := range []struct {
		job        string
		sell, cost int64
	}{{"JOB-A", 100_000, 150_000}, {"JOB-B", 80_000, 100_000}, {"JOB-C", 90_000, 50_000}} {
		r := u.must(u.module.RecordChargeLine(u.ctx, api.RecordChargeLine{CommandID: "line-" + c.job, TenantID: u.tenant, ActorID: "fin-1", JobID: c.job,
			ExpectedRevision: 1, ChargeCode: "AIR_FREIGHT", Sell: &api.ChargeSide{PartyID: "party-a", Amount: cny(c.sell)},
			Cost: &api.ChargeSide{PartyID: "airline-mu", Amount: cny(c.cost)}, Reason: "entered for the review tests", OccurredAt: u.hours(3)}))
		revision[c.job] = r.NewRevision
	}
	return u, revision
}

func (u usedWorld) profit(job string) api.JobProfit {
	u.t.Helper()
	p, err := u.module.GetJobProfit(u.ctx, api.GetJobProfit{TenantID: u.tenant, ActorID: "alice", JobID: job})
	if err != nil {
		u.t.Fatal(err)
	}
	return p
}

func (u usedWorld) requestCmd(id, job, version, actor string) api.RequestCostReview {
	return api.RequestCostReview{CommandID: id, TenantID: u.tenant, ActorID: actor, JobID: job, ProfitVersion: version, Reason: "the freight cost looks double"}
}

func (u usedWorld) resolveCmd(id, request, actor string) api.ResolveCostReview {
	return api.ResolveCostReview{CommandID: id, TenantID: u.tenant, ActorID: actor, RequestID: request, Outcome: api.ReviewNoChange, Note: "the costs are right"}
}

func (u usedWorld) refused(name string, err error, parts ...string) {
	u.t.Helper()
	if !errors.Is(err, api.ErrRejected) {
		u.t.Errorf("%s: %v, want a refusal", name, err)
		return
	}
	for _, p := range parts {
		if !strings.Contains(err.Error(), p) {
			u.t.Errorf("%s: %v, want it to say %q", name, err, p)
		}
	}
}

// The profit pages: every job that is not cancelled, a loss only on request, in job id order and in pages; each row
// carries the version GetJobProfit answers, and a cancelled job is not listed.
func TestJobProfitsArePagedAndCarryTheProfitVersionOnPostgres(t *testing.T) {
	u, _ := reviewWorld(t)
	all, err := u.module.ListJobProfits(u.ctx, api.ListJobProfits{TenantID: u.tenant, ActorID: "alice", LegalEntityID: "LE", Limit: 100})
	if err != nil || len(all.Rows) != 3 || all.NextAfter != "" || all.AsOf.IsZero() {
		t.Fatalf("all: %+v %v", all, err)
	}
	for i, want := range []struct {
		job             string
		revenue, profit int64
	}{{"JOB-A", 100_000, -50_000}, {"JOB-B", 80_000, -20_000}, {"JOB-C", 90_000, 40_000}} {
		r := all.Rows[i]
		if r.JobID != want.job || r.Revenue.Minor != want.revenue || r.Profit.Minor != want.profit || r.Cost.Minor != want.revenue-want.profit || r.Currency != "CNY" ||
			r.Revenue.Currency != "CNY" || r.State != "OPEN" || r.CustomerPartyID != "party-a" || r.ShipmentID != "SH-"+strings.TrimPrefix(want.job, "JOB-") ||
			r.ProfitBasis != "SEPARATE" || r.ProfitVersion != u.profit(want.job).ProfitVersion || len(r.ProfitVersion) != 64 {
			t.Errorf("row %d: %+v", i, r)
		}
	}
	losses, err := u.module.ListJobProfits(u.ctx, api.ListJobProfits{TenantID: u.tenant, ActorID: "alice", LegalEntityID: "LE", LossOnly: true, Limit: 100})
	if err != nil || len(losses.Rows) != 2 || losses.Rows[0].JobID != "JOB-A" || losses.Rows[1].JobID != "JOB-B" || losses.NextAfter != "" {
		t.Fatalf("losses: %+v %v", losses, err)
	}
	first, err := u.module.ListJobProfits(u.ctx, api.ListJobProfits{TenantID: u.tenant, ActorID: "alice", LegalEntityID: "LE", LossOnly: true, Limit: 1})
	if err != nil || len(first.Rows) != 1 || first.Rows[0].JobID != "JOB-A" || first.NextAfter != "JOB-A" {
		t.Fatalf("first page of losses: %+v %v", first, err)
	}
	second, err := u.module.ListJobProfits(u.ctx, api.ListJobProfits{TenantID: u.tenant, ActorID: "alice", LegalEntityID: "LE", LossOnly: true, Limit: 1, After: first.NextAfter})
	if err != nil || len(second.Rows) != 1 || second.Rows[0].JobID != "JOB-B" {
		t.Fatalf("second page of losses: %+v %v", second, err)
	}
	tail, err := u.module.ListJobProfits(u.ctx, api.ListJobProfits{TenantID: u.tenant, ActorID: "alice", LegalEntityID: "LE", Limit: 2, After: "JOB-A"})
	if err != nil || len(tail.Rows) != 2 || tail.Rows[0].JobID != "JOB-B" || tail.NextAfter != "" {
		t.Fatalf("after JOB-A: %+v %v", tail, err)
	}
	for _, limit := range []int{0, 501, -1} {
		if _, err = u.module.ListJobProfits(u.ctx, api.ListJobProfits{TenantID: u.tenant, ActorID: "alice", LegalEntityID: "LE", Limit: limit}); !errors.Is(err, api.ErrRejected) {
			t.Errorf("a page of %d: %v", limit, err)
		}
	}
	other, err := u.module.ListJobProfits(u.ctx, api.ListJobProfits{TenantID: u.tenant, ActorID: "alice", LegalEntityID: "LE-2", Limit: 10})
	if err != nil || len(other.Rows) != 0 {
		t.Fatalf("another legal entity: %+v %v", other, err)
	}
	u.must(u.module.CancelFreightJob(u.ctx, api.CancelFreightJob{CommandID: "cancel-c", TenantID: u.tenant, ActorID: "fin-1", JobID: "JOB-C",
		ExpectedRevision: 2, Reason: "the shipment was cancelled", OccurredAt: u.hours(5)}))
	if after, err := u.module.ListJobProfits(u.ctx, api.ListJobProfits{TenantID: u.tenant, ActorID: "alice", LegalEntityID: "LE", Limit: 100}); err != nil || len(after.Rows) != 2 {
		t.Fatalf("a cancelled job is listed: %+v %v", after, err)
	}
}

// The profit version is equal while nothing changes, and changes with a revision of the job, a share in a pool and the
// settings version, with the figures.
func TestTheProfitVersionChangesWithItsSourcesOnPostgres(t *testing.T) {
	u, revision := reviewWorld(t)
	v0 := u.profit("JOB-A").ProfitVersion
	if again := u.profit("JOB-A").ProfitVersion; again != v0 {
		t.Fatalf("the version changed with nothing: %s %s", v0, again)
	}
	if v := u.profit("JOB-B").ProfitVersion; v == v0 {
		t.Fatal("two jobs have one version")
	}
	u.must(u.module.ReviseChargeSide(u.ctx, api.ReviseChargeSide{CommandID: "revise-a", TenantID: u.tenant, ActorID: "fin-1", JobID: "JOB-A", ExpectedRevision: revision["JOB-A"],
		LineNo: 1, Side: api.SideCost, ExpectedSideVersion: 1, Value: api.ChargeSide{Amount: cny(160_000)}, Reason: "the carrier raised it", OccurredAt: u.hours(4)}))
	v1 := u.profit("JOB-A").ProfitVersion
	if v1 == v0 {
		t.Fatal("a cost revised leaves the version")
	}
	u.record("cost-2", "AIR_FREIGHT", api.MethodChargeable, 100_000)
	u.apportionAll("apportion-v", 5, []int{1})
	if v2 := u.profit("JOB-A").ProfitVersion; v2 == v1 {
		t.Fatal("a share in a pool leaves the version")
	}
	v2 := u.profit("JOB-A").ProfitVersion
	// a pool's revision moves the version even when the job's own figures do not: a line recorded and not apportioned
	u.must(u.module.RecordConsolCost(u.ctx, api.RecordConsolCost{CommandID: "cost-3", TenantID: u.tenant, ActorID: "fin-1", PoolID: u.pool, ExpectedRevision: u.revision(),
		ChargeCode: "AIR_FREIGHT", Method: api.MethodChargeable, Cost: api.ChargeSide{PartyID: "airline-mu", Amount: cny(7_000)}, Reason: "entered for the consolidation",
		OccurredAt: u.hours(7)}))
	v3 := u.profit("JOB-A").ProfitVersion
	if v3 == v2 {
		t.Fatal("a revision of a pool the job has a share in leaves the version")
	}
	u.configure(time0, le("fin.F6", "INCLUDED_MARKED"))
	if v4 := u.profit("JOB-A").ProfitVersion; v4 == v3 || u.profit("JOB-A").SettingsVersion != 1 {
		t.Fatalf("a settings version leaves the version: %s", v4)
	}
}

// A job at exactly zero is listed, and is not a loss: a loss-only page leaves it out, and a request for it is refused.
func TestAJobAtZeroIsNotALossOnPostgres(t *testing.T) {
	u, revision := reviewWorld(t)
	u.must(u.module.ReviseChargeSide(u.ctx, api.ReviseChargeSide{CommandID: "revise-c", TenantID: u.tenant, ActorID: "fin-1", JobID: "JOB-C", ExpectedRevision: revision["JOB-C"],
		LineNo: 1, Side: api.SideCost, ExpectedSideVersion: 1, Value: api.ChargeSide{Amount: cny(90_000)}, Reason: "the carrier's final rate", OccurredAt: u.hours(4)}))
	if p := u.profit("JOB-C"); p.Profit.Minor != 0 {
		t.Fatalf("JOB-C: %+v", p)
	}
	all, err := u.module.ListJobProfits(u.ctx, api.ListJobProfits{TenantID: u.tenant, ActorID: "alice", LegalEntityID: "LE", Limit: 10})
	if err != nil || len(all.Rows) != 3 || all.Rows[2].Profit.Minor != 0 {
		t.Fatalf("all: %+v %v", all, err)
	}
	losses, err := u.module.ListJobProfits(u.ctx, api.ListJobProfits{TenantID: u.tenant, ActorID: "alice", LegalEntityID: "LE", LossOnly: true, Limit: 10})
	if err != nil || len(losses.Rows) != 2 {
		t.Fatalf("losses: %+v %v", losses, err)
	}
	u.refused("a job at zero", func() error {
		_, err := u.module.RequestCostReview(u.ctx, u.requestCmd("r-zero", "JOB-C", u.profit("JOB-C").ProfitVersion, "alice"))
		return err
	}(), "FIN_REVIEW_NOT_A_LOSS")
}

// A request is made with the version read, replays as its receipt, and is resolved by another person once; nothing it
// does changes an amount, a job's revision or its profit version.
func TestACostReviewIsRequestedAndResolvedOnPostgres(t *testing.T) {
	u, revision := reviewWorld(t)
	f := u.fixture
	before := u.profit("JOB-A")
	cmd := u.requestCmd("request-a", "JOB-A", before.ProfitVersion, "alice")
	r := u.must(u.module.RequestCostReview(u.ctx, cmd))
	if r.SubjectID != "CRR-1" || r.NewRevision != 1 || r.Version != 1 || r.CommandID != "request-a" {
		t.Fatalf("the receipt: %+v", r)
	}
	if again, err := u.module.RequestCostReview(u.ctx, cmd); err != nil || again != r {
		t.Fatalf("the replay: %+v %v", again, err)
	}
	changed := cmd
	changed.Reason = "another reason"
	if _, err := u.module.RequestCostReview(u.ctx, changed); !errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("the command id with another reason: %v", err)
	}
	asOther := cmd
	asOther.ActorID = "carol"
	if _, err := u.module.RequestCostReview(u.ctx, asOther); !errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("the command id as another person: %v", err)
	}
	got, err := u.module.GetCostReviewRequest(u.ctx, api.GetCostReviewRequest{TenantID: u.tenant, ActorID: "alice", RequestID: "CRR-1"})
	if err != nil || got.JobID != "JOB-A" || got.ShipmentID != "SH-A" || got.LegalEntityID != "LE" || got.RequestedBy != "alice" || got.Reason != cmd.Reason || got.State != api.ReviewOpen ||
		got.Revenue.Minor != 100_000 || got.Cost.Minor != 150_000 || got.Profit.Minor != -50_000 || got.Profit.Currency != "CNY" || got.ProfitBasis != "SEPARATE" ||
		got.ProfitVersion != before.ProfitVersion || got.Resolution != nil || got.RequestedAt.IsZero() {
		t.Fatalf("the request: %+v %v", got, err)
	}
	// the refusals, each with its way out
	other := u.profit("JOB-B")
	u.refused("a profit above 0", func() error {
		_, err := u.module.RequestCostReview(u.ctx, u.requestCmd("r-c", "JOB-C", u.profit("JOB-C").ProfitVersion, "alice"))
		return err
	}(),
		"FIN_REVIEW_NOT_A_LOSS", "not a loss, no review is needed")
	u.refused("a stale version", func() error {
		_, err := u.module.RequestCostReview(u.ctx, u.requestCmd("r-b-old", "JOB-B", "0123", "alice"))
		return err
	}(),
		"FIN_REVIEW_STALE", "read it again")
	u.refused("a second request", func() error {
		_, err := u.module.RequestCostReview(u.ctx, u.requestCmd("request-a2", "JOB-A", before.ProfitVersion, "bob"))
		return err
	}(), "FIN_REVIEW_OPEN", "request CRR-1 is open: wait for its resolution")
	if _, err := u.module.RequestCostReview(u.ctx, u.requestCmd("r-none", "JOB-NONE", "x", "alice")); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("an unknown job: %v", err)
	}
	bad := u.requestCmd("r-reason", "JOB-B", other.ProfitVersion, "alice")
	bad.Reason = " "
	if _, err = u.module.RequestCostReview(u.ctx, bad); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("an empty reason: %v", err)
	}
	// the numbers have no gap: the refusals took none
	second := u.must(u.module.RequestCostReview(u.ctx, u.requestCmd("request-b", "JOB-B", other.ProfitVersion, "alice")))
	if second.SubjectID != "CRR-2" || second.Version != 2 {
		t.Fatalf("the second request: %+v", second)
	}
	// the resolution: not by the requester, once
	res := u.resolveCmd("resolve-1", "CRR-1", "alice")
	if _, err = u.module.ResolveCostReview(u.ctx, res); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "FIN_REVIEW_SAME_ACTOR") ||
		!strings.Contains(err.Error(), "another person") {
		t.Fatalf("the requester resolves: %v", err)
	}
	res.ActorID = "bob"
	rr := u.must(u.module.ResolveCostReview(u.ctx, res))
	if rr.SubjectID != "CRR-1" || rr.NewRevision != 2 || rr.CommandID != "resolve-1" {
		t.Fatalf("the resolution's receipt: %+v", rr)
	}
	if again, err := u.module.ResolveCostReview(u.ctx, res); err != nil || again != rr {
		t.Fatalf("the replay of the resolution: %+v %v", again, err)
	}
	third := u.resolveCmd("resolve-2", "CRR-1", "carol")
	u.refused("a second resolution", func() error { _, err := u.module.ResolveCostReview(u.ctx, third); return err }(), "FIN_REVIEW_RESOLVED", "read it")
	wrong := u.resolveCmd("resolve-3", "CRR-2", "bob")
	wrong.Outcome = "DONE"
	if _, err = u.module.ResolveCostReview(u.ctx, wrong); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("an outcome that is none: %v", err)
	}
	if _, err = u.module.ResolveCostReview(u.ctx, u.resolveCmd("resolve-4", "CRR-99", "bob")); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("an unknown request: %v", err)
	}
	got, err = u.module.GetCostReviewRequest(u.ctx, api.GetCostReviewRequest{TenantID: u.tenant, ActorID: "alice", RequestID: "CRR-1"})
	if err != nil || got.State != api.ReviewResolved || got.Resolution == nil || got.Resolution.ResolvedBy != "bob" || got.Resolution.Outcome != api.ReviewNoChange ||
		got.Resolution.Note != "the costs are right" || got.Resolution.ResolvedAt.Before(got.RequestedAt) {
		t.Fatalf("the resolved request: %+v %v", got, err)
	}
	// a resolved request lets the job be asked about again; nothing changed meanwhile, so the version is the same
	after := u.profit("JOB-A")
	if after.ProfitVersion != before.ProfitVersion || after.Profit != before.Profit || after.Revision != revision["JOB-A"] {
		t.Fatalf("the review changed the job: %+v then %+v", before, after)
	}
	again := u.must(u.module.RequestCostReview(u.ctx, u.requestCmd("request-a3", "JOB-A", after.ProfitVersion, "alice")))
	if again.SubjectID != "CRR-3" {
		t.Fatalf("the request after the resolution: %+v", again)
	}
	// the lists
	byJob, err := u.module.ListCostReviewRequests(u.ctx, api.ListCostReviewRequests{TenantID: u.tenant, ActorID: "alice", JobID: "JOB-A", Limit: 10})
	if err != nil || len(byJob.Requests) != 2 || byJob.Requests[0].RequestID != "CRR-1" || byJob.Requests[1].RequestID != "CRR-3" || byJob.NextAfter != "" {
		t.Fatalf("by job: %+v %v", byJob, err)
	}
	open, err := u.module.ListCostReviewRequests(u.ctx, api.ListCostReviewRequests{TenantID: u.tenant, ActorID: "alice", State: api.ReviewOpen, Limit: 10})
	if err != nil || len(open.Requests) != 2 || open.Requests[0].RequestID != "CRR-2" || open.Requests[1].RequestID != "CRR-3" {
		t.Fatalf("open: %+v %v", open, err)
	}
	done, err := u.module.ListCostReviewRequests(u.ctx, api.ListCostReviewRequests{TenantID: u.tenant, ActorID: "alice", State: api.ReviewResolved, Limit: 10})
	if err != nil || len(done.Requests) != 1 || done.Requests[0].RequestID != "CRR-1" || done.Requests[0].Resolution == nil {
		t.Fatalf("resolved: %+v %v", done, err)
	}
	pg, err := u.module.ListCostReviewRequests(u.ctx, api.ListCostReviewRequests{TenantID: u.tenant, ActorID: "alice", Limit: 2})
	if err != nil || len(pg.Requests) != 2 || pg.NextAfter != "CRR-2" {
		t.Fatalf("a page of 2: %+v %v", pg, err)
	}
	pg, err = u.module.ListCostReviewRequests(u.ctx, api.ListCostReviewRequests{TenantID: u.tenant, ActorID: "alice", Limit: 2, After: "CRR-2"})
	if err != nil || len(pg.Requests) != 1 || pg.Requests[0].RequestID != "CRR-3" || pg.NextAfter != "" {
		t.Fatalf("the next page: %+v %v", pg, err)
	}
	for _, q := range []api.ListCostReviewRequests{{Limit: 0}, {Limit: 501}, {Limit: 5, State: "DONE"}, {Limit: 5, After: "x"}} {
		q.TenantID, q.ActorID = u.tenant, "alice"
		if _, err = u.module.ListCostReviewRequests(u.ctx, q); !errors.Is(err, api.ErrRejected) {
			t.Errorf("%+v: %v", q, err)
		}
	}
	types := f.outboxTypes(t)
	if countOf(types, "CostReviewRequested") != 3 || countOf(types, "CostReviewResolved") != 1 {
		t.Fatalf("the events: %v", types)
	}
	// a cancelled job is refused with its way out
	u.must(u.module.CancelFreightJob(u.ctx, api.CancelFreightJob{CommandID: "cancel-b", TenantID: u.tenant, ActorID: "fin-1", JobID: "JOB-B",
		ExpectedRevision: revision["JOB-B"], Reason: "the shipment was cancelled", OccurredAt: u.hours(5)}))
	u.refused("a cancelled job", func() error {
		_, err := u.module.RequestCostReview(u.ctx, u.requestCmd("r-cancelled", "JOB-B", other.ProfitVersion, "alice"))
		return err
	}(),
		"FIN_REVIEW_JOB_NOT_OPEN", "ask about its shipment's open job")
}

// Whoever FIN's authorizer refuses reads and writes nothing.
func TestTheCostReviewAndTheProfitNeedTheirPermissionsOnPostgres(t *testing.T) {
	u, _ := reviewWorld(t)
	v := u.profit("JOB-A").ProfitVersion
	if _, err := u.module.RequestCostReview(u.ctx, u.requestCmd("r-i", "JOB-A", v, "intruder")); !errors.Is(err, api.ErrUnauthorized) {
		t.Errorf("request: %v", err)
	}
	if _, err := u.module.ResolveCostReview(u.ctx, u.resolveCmd("s-i", "CRR-1", "intruder")); !errors.Is(err, api.ErrUnauthorized) {
		t.Errorf("resolve: %v", err)
	}
	// a command id another person used is not answered to a person who may not use it: the refusal comes before the
	// replay
	u.must(u.module.RequestCostReview(u.ctx, u.requestCmd("r-alice", "JOB-A", v, "alice")))
	if _, err := u.module.RequestCostReview(u.ctx, u.requestCmd("r-alice", "JOB-A", v, "intruder")); !errors.Is(err, api.ErrUnauthorized) {
		t.Errorf("a replay by someone who may not: %v", err)
	}
	if _, err := u.module.ResolveCostReview(u.ctx, u.resolveCmd("s-bob", "CRR-1", "bob")); err != nil {
		t.Fatal(err)
	}
	if _, err := u.module.ResolveCostReview(u.ctx, u.resolveCmd("s-bob", "CRR-1", "intruder")); !errors.Is(err, api.ErrUnauthorized) {
		t.Errorf("a replay of a resolution by someone who may not: %v", err)
	}
	if _, err := u.module.ListJobProfits(u.ctx, api.ListJobProfits{TenantID: u.tenant, ActorID: "intruder", LegalEntityID: "LE", Limit: 5}); !errors.Is(err, api.ErrUnauthorized) {
		t.Errorf("list: %v", err)
	}
	if _, err := u.module.ListJobProfits(u.ctx, api.ListJobProfits{TenantID: u.tenant, LegalEntityID: "LE", Limit: 5}); !errors.Is(err, api.ErrUnauthenticated) {
		t.Errorf("list without an actor: %v", err)
	}
	if _, err := u.module.GetJobProfit(u.ctx, api.GetJobProfit{TenantID: u.tenant, ActorID: "intruder", JobID: "JOB-A"}); !errors.Is(err, api.ErrUnauthorized) {
		t.Errorf("profit: %v", err)
	}
	if _, err := u.module.GetCostReviewRequest(u.ctx, api.GetCostReviewRequest{TenantID: u.tenant, ActorID: "intruder", RequestID: "CRR-1"}); !errors.Is(err, api.ErrUnauthorized) {
		t.Errorf("get: %v", err)
	}
	if _, err := u.module.ListCostReviewRequests(u.ctx, api.ListCostReviewRequests{TenantID: u.tenant, ActorID: "intruder", Limit: 5}); !errors.Is(err, api.ErrUnauthorized) {
		t.Errorf("list requests: %v", err)
	}
}

// A request waits for a job change that holds the job's lock, and is then refused as stale: a second connection holds
// the row lock a change of the job takes, a revision of a cost queues behind it, then the request with the version read
// before; the queue is checked with pg_blocking_pids. The revision commits first, the request reads the new profit.
func TestARequestWaitsForAJobChangeAndIsRefusedAsStaleOnPostgres(t *testing.T) {
	u, revision := reviewWorld(t)
	v := u.profit("JOB-A").ProfitVersion
	errs := queuedOnJob(t, u, "JOB-A",
		func() error {
			_, err := u.module.ReviseChargeSide(u.ctx, api.ReviseChargeSide{CommandID: "revise-q", TenantID: u.tenant, ActorID: "fin-1", JobID: "JOB-A", ExpectedRevision: revision["JOB-A"],
				LineNo: 1, Side: api.SideCost, ExpectedSideVersion: 1, Value: api.ChargeSide{Amount: cny(170_000)}, Reason: "the carrier raised it", OccurredAt: u.hours(4)})
			return err
		},
		func() error {
			_, err := u.module.RequestCostReview(u.ctx, u.requestCmd("request-q", "JOB-A", v, "alice"))
			return err
		})
	if errs[0] != nil {
		t.Fatalf("the revision: %v", errs[0])
	}
	u.refused("the request that waited", errs[1], "FIN_REVIEW_STALE", "read it again")
	if _, err := u.module.GetCostReviewRequest(u.ctx, api.GetCostReviewRequest{TenantID: u.tenant, ActorID: "alice", RequestID: "CRR-1"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("a stale request was written: %v", err)
	}
	if now := u.profit("JOB-A"); now.ProfitVersion == v || now.Profit.Minor != -70_000 {
		t.Fatalf("the profit after the revision: %+v", now)
	}
	// the version read again is accepted
	u.must(u.module.RequestCostReview(u.ctx, u.requestCmd("request-q2", "JOB-A", u.profit("JOB-A").ProfitVersion, "alice")))
}

// Two requests that are one command, sent at once while the job's lock is held, answer one receipt and write one row.
func TestTwoIdenticalRequestsAtOnceAnswerOneReceiptOnPostgres(t *testing.T) {
	u, _ := reviewWorld(t)
	v := u.profit("JOB-A").ProfitVersion
	cmd := u.requestCmd("request-same", "JOB-A", v, "alice")
	var receipts [2]api.Receipt
	run := func(i int) func() error {
		return func() (err error) { receipts[i], err = u.module.RequestCostReview(u.ctx, cmd); return err }
	}
	errs := queuedOnJob(t, u, "JOB-A", run(0), run(1))
	if errs[0] != nil || errs[1] != nil || receipts[0] != receipts[1] || receipts[0].SubjectID != "CRR-1" {
		t.Fatalf("receipts %+v %+v, errors %v", receipts[0], receipts[1], errs)
	}
	list, err := u.module.ListCostReviewRequests(u.ctx, api.ListCostReviewRequests{TenantID: u.tenant, ActorID: "alice", Limit: 10})
	if err != nil || len(list.Requests) != 1 {
		t.Fatalf("the requests: %+v %v", list, err)
	}
	if n := countOf(u.outboxTypes(t), "CostReviewRequested"); n != 1 {
		t.Fatalf("%d events", n)
	}
}

// Two different requests for one job at once: the one that came first wins, the other is refused naming it.
func TestTwoDifferentRequestsAtOnceOneWinsAndTheOtherIsRefusedNamingItOnPostgres(t *testing.T) {
	u, _ := reviewWorld(t)
	v := u.profit("JOB-A").ProfitVersion
	errs := queuedOnJob(t, u, "JOB-A",
		func() error {
			_, err := u.module.RequestCostReview(u.ctx, u.requestCmd("request-1", "JOB-A", v, "alice"))
			return err
		},
		func() error {
			_, err := u.module.RequestCostReview(u.ctx, u.requestCmd("request-2", "JOB-A", v, "bob"))
			return err
		})
	if errs[0] != nil {
		t.Fatalf("the first: %v", errs[0])
	}
	u.refused("the second", errs[1], "FIN_REVIEW_OPEN", "request CRR-1 is open: wait for its resolution")
	if _, err := u.module.GetCostReviewRequest(u.ctx, api.GetCostReviewRequest{TenantID: u.tenant, ActorID: "alice", RequestID: "CRR-2"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("the loser took a number: %v", err)
	}
}

// Two resolutions of one request at once: one wins, the other is refused as resolved.
func TestTwoResolutionsAtOnceOneWinsOnPostgres(t *testing.T) {
	u, _ := reviewWorld(t)
	u.must(u.module.RequestCostReview(u.ctx, u.requestCmd("request-1", "JOB-A", u.profit("JOB-A").ProfitVersion, "alice")))
	errs := queuedOnJob(t, u, "JOB-A",
		func() error {
			_, err := u.module.ResolveCostReview(u.ctx, u.resolveCmd("resolve-bob", "CRR-1", "bob"))
			return err
		},
		func() error {
			_, err := u.module.ResolveCostReview(u.ctx, u.resolveCmd("resolve-carol", "CRR-1", "carol"))
			return err
		})
	if errs[0] != nil {
		t.Fatalf("the first: %v", errs[0])
	}
	u.refused("the second", errs[1], "FIN_REVIEW_RESOLVED")
}

// Requests for different jobs of one tenant at once take the numbers 1 to n without a gap or a repeat.
func TestRequestsOfDifferentJobsAtOnceAreNumberedWithoutAGapOnPostgres(t *testing.T) {
	u, _ := reviewWorld(t)
	jobs := []string{"JOB-A", "JOB-B"}
	var wg sync.WaitGroup
	receipts := make([]api.Receipt, len(jobs))
	errs := make([]error, len(jobs))
	for i, job := range jobs {
		v := u.profit(job).ProfitVersion
		wg.Add(1)
		go func() {
			defer wg.Done()
			receipts[i], errs[i] = u.module.RequestCostReview(u.ctx, u.requestCmd("request-"+job, job, v, "alice"))
		}()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || receipts[0].Version+receipts[1].Version != 3 || receipts[0].Version == receipts[1].Version {
		t.Fatalf("receipts %+v, errors %v", receipts, errs)
	}
}

// What the database refuses past FIN, as the application role writes it.
func TestTheDatabaseKeepsTheCostReviewRulesOnPostgres(t *testing.T) {
	u, _ := reviewWorld(t)
	f := u.fixture
	u.must(u.module.RequestCostReview(u.ctx, u.requestCmd("request-1", "JOB-A", u.profit("JOB-A").ProfitVersion, "alice")))
	reqIn := func(no int, job, shipment, entity, currency string, revenue, cost, profit int64) string {
		return fmt.Sprintf(`INSERT INTO cargo_fin.cost_review_request (tenant_id,request_no,request_id,job_id,shipment_id,legal_entity_id,requested_by,reason,currency,
            revenue_minor,cost_minor,profit_minor,profit_basis,settings_version,profit_version,requested_at) VALUES ($1,%d,'CRR-%d','%s','%s','%s','x','r','%s',%d,%d,%d,
            'SEPARATE',0,repeat('a',64),now())`, no, no, job, shipment, entity, currency, revenue, cost, profit)
	}
	req := func(no int, job, shipment string, revenue, cost, profit int64) string {
		return reqIn(no, job, shipment, "LE", "CNY", revenue, cost, profit)
	}
	with := func(sql, old, new string) string { return strings.Replace(sql, old, new, 1) }
	for name, tc := range map[string]struct{ sql, state, check string }{
		"a request rewritten":                 {`UPDATE cargo_fin.cost_review_request SET reason='y' WHERE tenant_id=$1`, "42501|23001", ""},
		"a request removed":                   {`DELETE FROM cargo_fin.cost_review_request WHERE tenant_id=$1`, "42501|23001", ""},
		"a number with a gap":                 {req(3, "JOB-B", "SH-B", 100, 200, -100), "23514", "cost_review_request_next_number"},
		"a number taken":                      {req(1, "JOB-B", "SH-B", 100, 200, -100), "23514", "cost_review_request_next_number"},
		"a second unresolved request":         {req(2, "JOB-A", "SH-A", 100, 200, -100), "23514", "cost_review_one_open"},
		"figures that do not add up":          {req(2, "JOB-B", "SH-B", 100, 200, -50), "23514", "cost_review_request_figures"},
		"a profit that is not a loss":         {req(2, "JOB-B", "SH-B", 300, 200, 100), "23514", "cost_review_request_loss"},
		"a zero profit":                       {req(2, "JOB-B", "SH-B", 200, 200, 0), "23514", "cost_review_request_loss"},
		"another shipment than the job's":     {req(2, "JOB-B", "SH-A", 100, 200, -100), "23514", "cost_review_request_job"},
		"a job it does not know":              {req(2, "JOB-NONE", "SH-B", 100, 200, -100), "23503", ""},
		"a request id that is not its no":     {strings.Replace(req(2, "JOB-B", "SH-B", 100, 200, -100), "'CRR-2'", "'CRR-9'", 1), "23514", "cost_review_request_id_form"},
		"another legal entity than the job's": {reqIn(2, "JOB-B", "SH-B", "LE-X", "CNY", 100, 200, -100), "23514", "cost_review_request_job"},
		"another currency than the job's":     {reqIn(2, "JOB-B", "SH-B", "LE", "USD", 100, 200, -100), "23514", "cost_review_request_job"},
		"a version in capitals":               {with(req(2, "JOB-B", "SH-B", 100, 200, -100), "repeat('a',64)", "repeat('A',64)"), "23514", "cost_review_request_profit_version_check"},
		"a version too short":                 {with(req(2, "JOB-B", "SH-B", 100, 200, -100), "repeat('a',64)", "repeat('a',63)"), "23514", "cost_review_request_profit_version_check"},
		"an empty reason":                     {with(req(2, "JOB-B", "SH-B", 100, 200, -100), "'LE','x','r'", "'LE','x',''"), "23514", "cost_review_request_reason_check"},
		"a reason of 501 characters":          {with(req(2, "JOB-B", "SH-B", 100, 200, -100), "'LE','x','r'", "'LE','x',repeat('r',501)"), "23514", "cost_review_request_reason_check"},
		"a profit basis that is none":         {with(req(2, "JOB-B", "SH-B", 100, 200, -100), "'SEPARATE'", "'OTHER'"), "23514", "cost_review_request_profit_basis_check"},
		"a negative settings version":         {with(req(2, "JOB-B", "SH-B", 100, 200, -100), "'SEPARATE',0", "'SEPARATE',-1"), "23514", "cost_review_request_settings_version_check"},
		"a resolution by the requester":       {resolution(1, "alice", "alice"), "23514", "cost_review_resolution_other_actor"},
		"a resolution of an unknown pair":     {resolution(1, "bob", "carol"), "23503", ""},
		"a resolution of an unknown no":       {resolution(7, "alice", "bob"), "23503", ""},
		"a resolution before its request":     {resolutionAt(1, "alice", "bob", "- interval '1 day'"), "23514", "cost_review_resolution_after_request"},
		"an empty note":                       {with(resolution(1, "alice", "bob"), "'the costs are right'", "''"), "23514", "cost_review_resolution_note_check"},
		"a note of 501 characters":            {with(resolution(1, "alice", "bob"), "'the costs are right'", "repeat('n',501)"), "23514", "cost_review_resolution_note_check"},
		"an empty resolver":                   {with(resolution(1, "alice", "bob"), "'bob'", "''"), "23514", "cost_review_resolution_resolved_by_check"},
		"an outcome that is none":             {strings.Replace(resolution(1, "alice", "bob"), "'NO_CHANGE'", "'DONE'", 1), "23514", ""},
		"a resolution that is not written":    {`DELETE FROM cargo_fin.cost_review_resolution WHERE tenant_id=$1`, "42501|23001", ""},
	} {
		err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			_, err := e.Exec(ctx, tc.sql, f.tenant)
			return err
		})
		state, constraint := sqlState(err)
		if state == "" || !strings.Contains(tc.state, state) || tc.check != "" && constraint != tc.check {
			t.Errorf("%s: %v (state %s, constraint %s), want %s %s", name, err, state, constraint, tc.state, tc.check)
		}
	}
	// one resolution, then no second; and after it a request for the job is allowed
	good := resolution(1, "alice", "bob")
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error { _, err := e.Exec(ctx, good, f.tenant); return err }); err != nil {
		t.Fatal(err)
	}
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		_, err := e.Exec(ctx, resolution(1, "alice", "carol"), f.tenant)
		return err
	}); err == nil {
		t.Error("a second resolution of a request was written")
	} else if state, _ := sqlState(err); state != "23505" {
		t.Errorf("a second resolution: %v", err)
	}
	// the number of the next request, past FIN, is the tenant's next, and a job with its request resolved takes one
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		_, err := e.Exec(ctx, req(2, "JOB-A", "SH-A", 100, 200, -100), f.tenant)
		return err
	}); err != nil {
		t.Errorf("a request after the resolution: %v", err)
	}
	// a row is never changed or removed: the privilege refuses first, and as the migrator role, which holds the
	// privilege, the trigger does; a write at REPEATABLE READ is refused by name
	dsn := os.Getenv("IDP_CARGO_MIGRATOR_DSN")
	if dsn == "" {
		t.Fatal("IDP_CARGO_MIGRATOR_DSN is required")
	}
	migrator, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer migrator.Close(context.Background())
	for name, sql := range map[string]string{"a request changed": `UPDATE cargo_fin.cost_review_request SET reason='y' WHERE tenant_id=$1`,
		"a request removed": `DELETE FROM cargo_fin.cost_review_request WHERE tenant_id=$1`, "a resolution changed": `UPDATE cargo_fin.cost_review_resolution SET note='y' WHERE tenant_id=$1`,
		"a resolution removed": `DELETE FROM cargo_fin.cost_review_resolution WHERE tenant_id=$1`} {
		_, err := migrator.Exec(context.Background(), sql, f.tenant)
		if state, _ := sqlState(err); err == nil || state != "23001" || !strings.Contains(err.Error(), "rows are immutable") {
			t.Errorf("%s by the migrator role: %v", name, err)
		}
	}
	conn, err := pgx.Connect(context.Background(), os.Getenv("IDP_CARGO_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	rr, err := conn.BeginTx(context.Background(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`SELECT set_config('app.tenant_id',$1,true)`, resolution(1, "alice", "carol")} {
		if _, err = rr.Exec(context.Background(), q, f.tenant); err != nil {
			break
		}
	}
	if state, constraint := sqlState(err); state != "23514" || constraint != "fin_read_committed" {
		t.Errorf("a resolution at REPEATABLE READ: %v", err)
	}
	_ = rr.Rollback(context.Background())
	// row security: another tenant sees none of them
	var n int
	err = (scope.TenantTransactor{DB: f.db, Tenant: "someone-else"}).WithinTransaction(context.Background(), func(txCtx context.Context) error {
		e, err := f.db.RequireExecutor(txCtx)
		if err != nil {
			return err
		}
		return e.QueryRow(txCtx, `SELECT (SELECT count(*) FROM cargo_fin.cost_review_request) + (SELECT count(*) FROM cargo_fin.cost_review_resolution)`).Scan(&n)
	})
	if err != nil || n != 0 {
		t.Errorf("another tenant reads %d rows: %v", n, err)
	}
}

// resolution is a statement that resolves request no, recorded as made by requestedBy, by resolvedBy a minute later.
func resolution(no int, requestedBy, resolvedBy string) string {
	return resolutionAt(no, requestedBy, resolvedBy, "+ interval '1 minute'")
}

// resolutionAt is resolution with the resolution's time given as an offset from now.
func resolutionAt(no int, requestedBy, resolvedBy, offset string) string {
	return fmt.Sprintf(`INSERT INTO cargo_fin.cost_review_resolution (tenant_id,request_no,requested_by,requested_at,resolved_by,outcome,note,resolved_at)
        SELECT $1,%d,'%s',coalesce((SELECT requested_at FROM cargo_fin.cost_review_request WHERE tenant_id=$1 AND request_no=%d),now()),'%s','NO_CHANGE','the costs are right',now()
        %s`, no, requestedBy, no, resolvedBy, offset)
}

// A request takes the settings lock in shared mode and reads the settings in force under it: while a settings command
// holds the lock the request waits, and the version it is asked with is the one of the settings version in force.
func TestARequestWaitsForTheSettingsLockAndReadsTheSettingsVersionOnPostgres(t *testing.T) {
	u, _ := reviewWorld(t)
	u.configure(time0, le("fin.F6", "INCLUDED_MARKED"))
	v := u.profit("JOB-A").ProfitVersion
	if u.profit("JOB-A").SettingsVersion != 1 {
		t.Fatalf("the settings version: %+v", u.profit("JOB-A"))
	}
	pid, release := holdSettingsLock(t, u.tenant, finpg.SettingsLockSQL)
	done := make(chan error, 1)
	var r api.Receipt
	go func() {
		var err error
		r, err = u.module.RequestCostReview(u.ctx, u.requestCmd("request-s", "JOB-A", v, "alice"))
		done <- err
	}()
	waitBlockedBy(t, pid, 1)
	if n := waitingInTheSettingsLock(t, u.tenant); n != 1 {
		t.Fatalf("%d sessions wait in the settings lock, want 1", n)
	}
	release()
	if err := <-done; err != nil || r.SubjectID != "CRR-1" {
		t.Fatalf("the request: %+v %v", r, err)
	}
	if got, err := u.module.GetCostReviewRequest(u.ctx, api.GetCostReviewRequest{TenantID: u.tenant, ActorID: "alice", RequestID: "CRR-1"}); err != nil || got.SettingsVersion != 1 {
		t.Fatalf("the request: %+v %v", got, err)
	}
}

// A request takes the tenant's next number under the tenant's numbering lock: a second connection holds the lock with a
// request of its own written and not yet committed, and the request that waited for it takes the number after.
func TestARequestWaitsForTheNumberingLockOnPostgres(t *testing.T) {
	u, _ := reviewWorld(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("IDP_CARGO_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var pid int
	if _, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, u.tenant); err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO cargo_fin.cost_review_request (tenant_id,request_no,request_id,job_id,shipment_id,legal_entity_id,requested_by,reason,currency,
            revenue_minor,cost_minor,profit_minor,profit_basis,settings_version,profit_version,requested_at) VALUES ($1,1,'CRR-1','JOB-B','SH-B','LE','x','r','CNY',1,2,-1,
            'SEPARATE',0,repeat('a',64),now())`, u.tenant)
	}
	if err == nil {
		err = tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid)
	}
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	var r api.Receipt
	go func() {
		var err error
		r, err = u.module.RequestCostReview(u.ctx, u.requestCmd("request-n", "JOB-A", u.profit("JOB-A").ProfitVersion, "alice"))
		done <- err
	}()
	waitBlockedBy(t, pid, 1)
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil || r.SubjectID != "CRR-2" {
		t.Fatalf("the request that waited: %+v %v", r, err)
	}
}

// A page follows the job id, not the order the jobs were opened in, and a row's cost is the job's cost and pool cost
// together.
func TestJobProfitsAreInJobIdOrderAndTheirCostIncludesThePoolOnPostgres(t *testing.T) {
	u, _ := reviewWorld(t)
	// a fourth job opened last and with the smallest id
	u.w.mu.Lock()
	u.w.drivers["SH-D"] = u.w.drivers["SH-C"]
	d := u.w.drivers["SH-D"]
	d.ShipmentID = "SH-D"
	u.w.drivers["SH-D"] = d
	u.w.mu.Unlock()
	u.must(u.module.OpenFreightJob(u.ctx, api.OpenFreightJob{CommandID: "open-0", TenantID: u.tenant, ActorID: "fin-1", JobID: "JOB-0", ShipmentID: "SH-D", OccurredAt: u.hours(9)}))
	u.apportionAll("apportion-order", 5, []int{1})
	page, err := u.module.ListJobProfits(u.ctx, api.ListJobProfits{TenantID: u.tenant, ActorID: "alice", LegalEntityID: "LE", Limit: 2})
	if err != nil || len(page.Rows) != 2 || page.Rows[0].JobID != "JOB-0" || page.Rows[1].JobID != "JOB-A" || page.NextAfter != "JOB-A" {
		t.Fatalf("the first page: %+v %v", page, err)
	}
	next, err := u.module.ListJobProfits(u.ctx, api.ListJobProfits{TenantID: u.tenant, ActorID: "alice", LegalEntityID: "LE", Limit: 5, After: page.NextAfter})
	if err != nil || len(next.Rows) != 2 || next.Rows[0].JobID != "JOB-B" || next.Rows[1].JobID != "JOB-C" {
		t.Fatalf("the next page: %+v %v", next, err)
	}
	for _, r := range append(page.Rows, next.Rows...) {
		p := u.profit(r.JobID)
		if r.Cost.Minor != p.Cost.Minor+p.PoolCost.Minor || r.Revenue.Minor-r.Cost.Minor != r.Profit.Minor || r.Profit != p.Profit || r.ProfitVersion != p.ProfitVersion {
			t.Errorf("%s: row %+v, profit %+v", r.JobID, r, p)
		}
	}
	if p := u.profit("JOB-A"); p.PoolCost.Minor == 0 {
		t.Fatalf("JOB-A bears nothing of the pool: %+v", p)
	}
}

// A job that bears pool cost is asked about with its cost including the pool's share: the request records the same
// revenue, cost and profit as the profit query, and revenue less cost is profit.
func TestARequestForAJobThatBearsPoolCostRecordsItsCostOnPostgres(t *testing.T) {
	u, _ := reviewWorld(t)
	u.apportionAll("apportion-pool", 5, []int{1})
	p := u.profit("JOB-A")
	if p.PoolCost.Minor <= 0 || p.Cost.Minor != 150_000 || p.Profit.Minor >= 0 {
		t.Fatalf("JOB-A: %+v", p)
	}
	u.must(u.module.RequestCostReview(u.ctx, u.requestCmd("request-pool", "JOB-A", p.ProfitVersion, "alice")))
	got, err := u.module.GetCostReviewRequest(u.ctx, api.GetCostReviewRequest{TenantID: u.tenant, ActorID: "alice", RequestID: "CRR-1"})
	if err != nil || got.Revenue != p.Revenue || got.Cost.Minor != p.Cost.Minor+p.PoolCost.Minor || got.Profit != p.Profit ||
		got.Revenue.Minor-got.Cost.Minor != got.Profit.Minor {
		t.Fatalf("the request: %+v %v, the profit: %+v", got, err, p)
	}
}

// The commit trigger on the numbers stands alone: with the numbering trigger switched off by the owner of the table (in
// a transaction that is rolled back), a request that skips a number is refused at commit.
func TestTheCommitCheckOfTheNumbersStandsAloneOnPostgres(t *testing.T) {
	u, _ := reviewWorld(t)
	ctx := context.Background()
	dsn := os.Getenv("IDP_CARGO_MIGRATOR_DSN")
	if dsn == "" {
		t.Fatal("IDP_CARGO_MIGRATOR_DSN is required")
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, q := range []string{`ALTER TABLE cargo_fin.cost_review_request DISABLE TRIGGER cost_review_request_number`,
		`INSERT INTO cargo_fin.cost_review_request (tenant_id,request_no,request_id,job_id,shipment_id,legal_entity_id,requested_by,reason,currency,revenue_minor,cost_minor,
            profit_minor,profit_basis,settings_version,profit_version,requested_at) VALUES ('` + u.tenant + `',2,'CRR-2','JOB-B','SH-B','LE','x','r','CNY',1,2,-1,'SEPARATE',0,
            repeat('a',64),now())`} {
		if _, err = tx.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	err = tx.Commit(ctx)
	if state, constraint := sqlState(err); state != "23514" || constraint != "cost_review_numbers_consistent" {
		t.Fatalf("a request that skips a number: %v", err)
	}
}

// The one-open trigger locks the job's row at commit: a second connection holds the row, and the commit of a request
// written on a third waits for it (pg_blocking_pids) and commits when it is released.
func TestTheCommitOfARequestWaitsForTheJobsRowOnPostgres(t *testing.T) {
	u, _ := reviewWorld(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("IDP_CARGO_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, u.tenant); err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO cargo_fin.cost_review_request (tenant_id,request_no,request_id,job_id,shipment_id,legal_entity_id,requested_by,reason,currency,
            revenue_minor,cost_minor,profit_minor,profit_basis,settings_version,profit_version,requested_at) VALUES ($1,1,'CRR-1','JOB-B','SH-B','LE','x','r','CNY',1,2,-1,
            'SEPARATE',0,repeat('a',64),now())`, u.tenant)
	}
	if err != nil {
		t.Fatal(err)
	}
	var committer int
	if err = tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&committer); err != nil {
		t.Fatal(err)
	}
	pid, release := holdingJobPID(t, u, "JOB-B")
	done := make(chan error, 1)
	go func() { done <- tx.Commit(ctx) }()
	waitBlockedBy(t, pid, 1)
	release()
	if err = <-done; err != nil {
		t.Fatalf("the commit after the release: %v (the committing backend %d)", err, committer)
	}
}

// A job whose only tie to a pool is a re-routing loss has the pool's revision in its profit version.
func TestAPoolTheJobBearsOnlyALossOfIsInItsProfitVersionOnPostgres(t *testing.T) {
	u := formerWorld(t)
	u.depart()
	u.attribute("attribute-loss", 6, statementOf("C", 5_000, 10_000, api.PartyWarehouse))
	p := u.jobProfit("JOB-C")
	if p.ReroutingLoss.Minor != 5_000 || len(p.PoolShares) != 0 {
		t.Fatalf("JOB-C: %+v", p)
	}
	before := p.ProfitVersion
	u.must(u.module.RecordConsolCost(u.ctx, api.RecordConsolCost{CommandID: "cost-late", TenantID: u.tenant, ActorID: "fin-1", PoolID: u.pool, ExpectedRevision: u.revision(),
		ChargeCode: "AIR_FREIGHT", Method: api.MethodChargeable, Cost: api.ChargeSide{PartyID: "airline-mu", Amount: cny(7_000)}, Reason: "entered for the consolidation",
		OccurredAt: u.hours(9)}))
	after := u.jobProfit("JOB-C")
	if after.Profit != p.Profit || after.ProfitVersion == before {
		t.Fatalf("a revision of the pool: the version %s then %s, the profit %v then %v", before, after.ProfitVersion, p.Profit, after.Profit)
	}
}

type behindClock struct{ at time.Time }

func (c behindClock) Now() time.Time { return c.at }

// A resolution stamped earlier than its request (a server whose clock is behind) is refused with the cause and the way
// out.
func TestAResolutionStampedBeforeItsRequestIsRefusedWithTheWayOutOnPostgres(t *testing.T) {
	u, _ := reviewWorld(t)
	u.must(u.module.RequestCostReview(u.ctx, u.requestCmd("request-clock", "JOB-A", u.profit("JOB-A").ProfitVersion, "alice")))
	behind, err := finmodule.New(u.db, u.w, u.w, u.w, u.w, u.w, allow{}, finmodule.WithClock(behindClock{time.Now().UTC().Add(-time.Hour)}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = behind.ResolveCostReview(u.ctx, u.resolveCmd("resolve-clock", "CRR-1", "bob"))
	u.refused("a server whose clock is behind", err, "FIN_REVIEW_CLOCK_BEHIND", "synchronise the servers' clocks (for example with NTP) and try again")
	if got, err := u.module.GetCostReviewRequest(u.ctx, api.GetCostReviewRequest{TenantID: u.tenant, ActorID: "alice", RequestID: "CRR-1"}); err != nil || got.State != api.ReviewOpen {
		t.Fatalf("the request: %+v %v", got, err)
	}
	u.must(u.module.ResolveCostReview(u.ctx, u.resolveCmd("resolve-clock", "CRR-1", "bob")))
}

// With a scan window of two jobs, a loss-only page that reads a full window and finds fewer than the limit still gives
// the cursor, and the page after it, empty, ends the list.
func TestALossOnlyPageOfAFullScanWindowGivesItsCursorOnPostgres(t *testing.T) {
	u, _ := reviewWorld(t)
	small, err := finmodule.New(u.db, u.w, u.w, u.w, u.w, u.w, allow{}, finmodule.WithScanWindow(2))
	if err != nil {
		t.Fatal(err)
	}
	q := api.ListJobProfits{TenantID: u.tenant, ActorID: "alice", LegalEntityID: "LE", LossOnly: true, Limit: 5}
	first, err := small.ListJobProfits(u.ctx, q)
	if err != nil || len(first.Rows) != 2 || first.NextAfter != "JOB-B" {
		t.Fatalf("the first page: %+v %v", first, err)
	}
	q.After = first.NextAfter
	last, err := small.ListJobProfits(u.ctx, q)
	if err != nil || len(last.Rows) != 0 || last.NextAfter != "" {
		t.Fatalf("the last page: %+v %v", last, err)
	}
	if _, err = finmodule.New(u.db, u.w, u.w, u.w, u.w, u.w, allow{}, finmodule.WithScanWindow(0)); err == nil {
		t.Fatal("a scan window of none")
	}
}
