//go:build integration

package integration

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

func withShipment(d fulapi.CostDriver, shipment string) fulapi.CostDriver {
	d.ShipmentID = shipment
	return d
}

// ap drives FIN V0.5 commands of one fixture.
type ap struct {
	*fixture
	t   *testing.T
	ctx context.Context
}

func (a ap) day(h int) string { return a.hours(h).UTC().Format(time.DateOnly) }

func (a ap) must(r api.Receipt, err error) api.Receipt {
	a.t.Helper()
	if err != nil {
		a.t.Fatal(err)
	}
	return r
}

func (a ap) rejected(name string, err error, code string) {
	a.t.Helper()
	if !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), code) {
		a.t.Fatalf("%s: %v, want %s", name, err, code)
	}
}

func (a ap) invoice(id string) api.CreditorInvoice {
	a.t.Helper()
	c, err := a.module.GetCreditorInvoice(a.ctx, api.GetCreditorInvoice{TenantID: a.tenant, InvoiceID: id})
	if err != nil {
		a.t.Fatal(err)
	}
	return c
}

func (a ap) pool(id string) api.ConsolCostPool {
	a.t.Helper()
	p, err := a.module.GetConsolCostPool(a.ctx, api.GetConsolCostPool{TenantID: a.tenant, PoolID: id})
	if err != nil {
		a.t.Fatal(err)
	}
	return p
}

func (a ap) job(id string) api.FreightJob {
	a.t.Helper()
	j, err := a.module.GetFreightJob(a.ctx, api.GetFreightJob{TenantID: a.tenant, JobID: id})
	if err != nil {
		a.t.Fatal(err)
	}
	return j
}

func (a ap) record(id, creditor, number string, h int, currency kernel.CurrencyCode, lines ...api.CreditorInvoiceLine) api.RecordCreditorInvoice {
	return api.RecordCreditorInvoice{CommandID: "record-" + id, TenantID: a.tenant, ActorID: "ap-1", InvoiceID: id, LegalEntityID: "LE",
		CreditorPartyID: creditor, SupplierInvoiceNo: number, InvoiceDate: a.day(h), Currency: currency, Lines: lines, OccurredAt: a.hours(h)}
}

func (a ap) match(id, actor string, h int, matches ...api.CreditorMatch) api.MatchCreditorInvoice {
	return api.MatchCreditorInvoice{CommandID: "match-" + id + "-" + a.day(h) + actor, TenantID: a.tenant, ActorID: actor, InvoiceID: id,
		ExpectedRevision: a.invoice(id).Revision, Matches: matches, OccurredAt: a.hours(h)}
}

func line(code string, amount int64) api.CreditorInvoiceLine {
	return api.CreditorInvoiceLine{ChargeCode: code, Amount: amount}
}

func onJob(job string, lineNo int, invoiceLine int, amount int64, final bool) api.CreditorMatch {
	return api.CreditorMatch{LineNo: invoiceLine, Target: api.AccrualRef{Kind: api.TargetJobCost, ID: job, LineNo: lineNo}, Amount: amount, Final: final}
}

func onPool(pool string, lineNo int, invoiceLine int, amount int64, final bool) api.CreditorMatch {
	return api.CreditorMatch{LineNo: invoiceLine, Target: api.AccrualRef{Kind: api.TargetPoolCost, ID: pool, LineNo: lineNo}, Amount: amount, Final: final}
}

// creditorWorld is LE's policy (CNY; ESTIMATE USD 7.1234, ACCOUNTING USD 7.2000; a variance threshold of 100.00 CNY or
// 1%), JOB-A (SH-A) with a trucker's pickup accrued at 1,500.00 CNY, JOB-B (SH-B) with a delivery accrued at 300.00
// CNY, and P-1 on CONSOL-1 with MU's freight accrued at 1,000.00 USD (CHARGEABLE) and the CFS's handling at 500.00 CNY
// (PIECES), apportioned on the loaded basis.
func creditorWorld(t *testing.T) ap {
	ctx := context.Background()
	a := ap{fixture: newFixture(t), t: t, ctx: ctx}
	a.w.costBases["CONSOL-1"] = airConsol(a.fixture, "CONSOL-1")
	a.w.costBases["CONSOL-2"] = airConsol(a.fixture, "CONSOL-2")
	a.must(a.module.OpenFinancePolicy(ctx, api.OpenFinancePolicy{CommandID: "policy", TenantID: a.tenant, ActorID: "acct-1", LegalEntityID: "LE",
		FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: a.hours(0)}))
	a.must(a.module.RecordExchangeRate(ctx, api.RecordExchangeRate{CommandID: "rate-est", TenantID: a.tenant, ActorID: "acct-1", LegalEntityID: "LE",
		ExpectedRevision: 1, RateType: api.RateEstimate, From: "USD", Rate: 71_234, Scale: 4, FromDigits: 2, ToDigits: 2, ValidFrom: a.at.AddDate(0, 0, -1),
		ValidUntil: a.at.AddDate(0, 1, 0), Source: "CFETS central parity", OccurredAt: a.hours(0)}))
	a.must(a.module.RecordExchangeRate(ctx, api.RecordExchangeRate{CommandID: "rate-acct", TenantID: a.tenant, ActorID: "acct-1", LegalEntityID: "LE",
		ExpectedRevision: 2, RateType: api.RateAccounting, From: "USD", Rate: 72_000, Scale: 4, FromDigits: 2, ToDigits: 2, ValidFrom: a.at.AddDate(0, 0, -1),
		ValidUntil: a.at.AddDate(0, 1, 0), Source: "monthly rate 2026-11", OccurredAt: a.hours(0)}))
	a.must(a.module.SetCostVarianceThreshold(ctx, api.SetCostVarianceThreshold{CommandID: "threshold", TenantID: a.tenant, ActorID: "acct-1",
		LegalEntityID: "LE", ExpectedRevision: 3, Amount: 10_000, RateBP: 100, OccurredAt: a.hours(0)}))
	for _, j := range []struct{ job, shipment string }{{"JOB-A", "SH-A"}, {"JOB-B", "SH-B"}} {
		a.must(a.module.OpenFreightJob(ctx, api.OpenFreightJob{CommandID: "open-" + j.job, TenantID: a.tenant, ActorID: "fin-1", JobID: j.job,
			ShipmentID: j.shipment, OccurredAt: a.hours(1)}))
	}
	a.must(a.module.RecordChargeLine(ctx, api.RecordChargeLine{CommandID: "pickup", TenantID: a.tenant, ActorID: "fin-1", JobID: "JOB-A", ExpectedRevision: 1,
		ChargeCode: "PICKUP", Sell: &api.ChargeSide{PartyID: "party-a", Amount: cny(200_000)}, Cost: &api.ChargeSide{PartyID: "trucker", Amount: cny(150_000)},
		Reason: "trucked to the CFS", OccurredAt: a.hours(1)}))
	a.must(a.module.RecordChargeLine(ctx, api.RecordChargeLine{CommandID: "delivery", TenantID: a.tenant, ActorID: "fin-1", JobID: "JOB-B",
		ExpectedRevision: 1, ChargeCode: "DELIVERY", Cost: &api.ChargeSide{PartyID: "trucker", Amount: cny(30_000)}, Reason: "delivery to the CFS",
		OccurredAt: a.hours(1)}))
	a.must(a.module.OpenConsolCostPool(ctx, api.OpenConsolCostPool{CommandID: "pool", TenantID: a.tenant, ActorID: "fin-1", PoolID: "P-1",
		ConsolidationID: "CONSOL-1", OccurredAt: a.hours(2)}))
	a.must(a.module.RecordConsolCost(ctx, api.RecordConsolCost{CommandID: "freight", TenantID: a.tenant, ActorID: "fin-1", PoolID: "P-1", ExpectedRevision: 1,
		ChargeCode: "AIR_FREIGHT", SupplierRef: "784-12345675", Method: api.MethodChargeable, Cost: api.ChargeSide{PartyID: "airline-mu", Amount: usd(100_000)},
		Reason: "MU's buy rate", OccurredAt: a.hours(2)}))
	a.must(a.module.RecordConsolCost(ctx, api.RecordConsolCost{CommandID: "cfs", TenantID: a.tenant, ActorID: "fin-1", PoolID: "P-1", ExpectedRevision: 2,
		ChargeCode: "CFS_HANDLING", Method: api.MethodPieces, Cost: api.ChargeSide{PartyID: "trucker", Amount: cny(50_000)}, Reason: "CFS tariff",
		OccurredAt: a.hours(2)}))
	a.must(a.module.ApportionConsolCosts(ctx, api.ApportionConsolCosts{CommandID: "apportion", TenantID: a.tenant, ActorID: "fin-1", PoolID: "P-1",
		ExpectedRevision: 3, OccurredAt: a.hours(2)}))
	return a
}

// Creditor invoices on PostgreSQL (FB4): recorded once per supplier number, matched in full to job and pool accruals
// at the ACCOUNTING rate, a final variance beyond the threshold approved by another person (or the controller allowed
// to approve their own), disputes holding a pool from closing, the pool closed once its costs are final and
// apportioned at them, reopened for a late change, accruals released, and what the database refuses past FIN.
func TestCreditorInvoicesOnPostgres(t *testing.T) {
	a := creditorWorld(t)
	ctx, f := a.ctx, a.fixture

	// recording: a creditor in a creditor role, a date not ahead, a supplier number once
	mu1 := a.record("CI-MU-1", "airline-mu", "MU-0001", 3, "USD", api.CreditorInvoiceLine{ChargeCode: "AIR_FREIGHT", Description: "MAWB freight",
		SupplierRef: "784-12345675", Amount: 60_000})
	mu1.TaxDocumentKind, mu1.TaxDocumentNumber = api.TaxDocSpecialFapiao, "25312000000012345678"
	customer := mu1
	customer.CommandID, customer.InvoiceID, customer.CreditorPartyID = "record-customer", "CI-X", "party-a"
	_, err := f.module.RecordCreditorInvoice(ctx, customer)
	a.rejected("a creditor without a creditor role", err, "FIN_PARTY_ROLE")
	ahead := mu1
	ahead.CommandID, ahead.InvoiceID, ahead.InvoiceDate = "record-ahead", "CI-Y", a.day(60)
	_, err = f.module.RecordCreditorInvoice(ctx, ahead)
	a.rejected("an invoice dated ahead", err, "invoice date")
	r := a.must(f.module.RecordCreditorInvoice(ctx, mu1))
	if r.SubjectID != "CI-MU-1" || r.NewRevision != 1 {
		t.Fatalf("recorded: %+v", r)
	}
	if again, err := f.module.RecordCreditorInvoice(ctx, mu1); err != nil || again != r {
		t.Fatalf("replay: %+v %v", again, err)
	}
	twice := mu1
	twice.CommandID, twice.InvoiceID = "record-twice", "CI-MU-1B"
	_, err = f.module.RecordCreditorInvoice(ctx, twice)
	a.rejected("a supplier number twice", err, "FIN_SUPPLIER_INVOICE_USED")

	// a match that is not final consumes part of MU's freight; the rest stays accrued
	m := a.match("CI-MU-1", "ap-1", 4, onPool("P-1", 2, 1, 60_000, false))
	_, err = f.module.MatchCreditorInvoice(ctx, m)
	a.rejected("a USD line to a CNY accrual", err, "FIN_MATCH_CURRENCY")
	stale := a.match("CI-MU-1", "ap-1", 4, onPool("P-1", 1, 1, 60_000, false))
	stale.CommandID, stale.ExpectedRevision = "match-stale", 9
	if _, err = f.module.MatchCreditorInvoice(ctx, stale); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("a stale revision: %v", err)
	}
	if r = a.must(f.module.MatchCreditorInvoice(ctx, a.match("CI-MU-1", "ap-1", 4, onPool("P-1", 1, 1, 60_000, false)))); r.Version != 1 || r.NewRevision != 2 {
		t.Fatalf("matched: %+v", r)
	}
	pl := a.pool("P-1").Lines[0]
	if pl.Status != api.StatusPartlyMatched || pl.Matched != usd(60_000) || pl.MatchedLocal != cny(432_000) || pl.Cost != usd(100_000) ||
		pl.CostLocal != cny(432_000+284_936) {
		t.Fatalf("P-1 line 1 partly matched: %+v", pl)
	}
	open, err := f.module.ListOpenAccruals(ctx, api.ListOpenAccruals{TenantID: f.tenant, LegalEntityID: "LE", CreditorPartyID: "airline-mu"})
	if err != nil || len(open) != 1 || open[0].Target != (api.AccrualRef{Kind: api.TargetPoolCost, ID: "P-1", LineNo: 1}) || open[0].Reference != "CONSOL-1" ||
		open[0].SupplierRef != "784-12345675" || open[0].Open != usd(40_000) || open[0].OpenLocal != cny(284_936) || open[0].Status != api.StatusPartlyMatched {
		t.Fatalf("MU's open accruals: %+v %v", open, err)
	}
	if all, err := f.module.ListOpenAccruals(ctx, api.ListOpenAccruals{TenantID: f.tenant, LegalEntityID: "LE"}); err != nil || len(all) != 4 ||
		all[0].Target.ID != "JOB-A" || all[1].Target.ID != "JOB-B" || all[3].Target.LineNo != 2 {
		t.Fatalf("all open accruals: %+v %v", all, err)
	}

	// MU's second invoice closes the accrual with a final match 50.00 USD above it: 436.60 CNY beyond the threshold
	a.must(f.module.RecordCreditorInvoice(ctx, a.record("CI-MU-2", "airline-mu", "MU-0002", 5, "USD", line("AIR_FREIGHT", 45_000))))
	_, err = f.module.MatchCreditorInvoice(ctx, a.match("CI-MU-2", "ap-1", 5, onPool("P-1", 1, 1, 45_000, false)))
	a.rejected("more than the accrual has open", err, "FIN_MATCH_EXCEEDS")
	a.must(f.module.MatchCreditorInvoice(ctx, a.match("CI-MU-2", "ap-1", 5, onPool("P-1", 1, 1, 45_000, true))))
	mu2 := a.invoice("CI-MU-2")
	if x := mu2.Matches[0]; mu2.State != api.CreditorMatched || !mu2.AwaitingApproval || mu2.ThresholdVersion != 1 || !x.Final || x.Local != cny(324_000) ||
		x.Prior != usd(60_000) || x.PriorLocal != cny(432_000) || x.Accrual != usd(100_000) || x.Variance != usd(5_000) || x.VarianceLocal != cny(43_660) ||
		x.FX == nil || x.FX.RateType != api.RateAccounting {
		t.Fatalf("CI-MU-2: %+v", mu2)
	}

	// the trucker's invoice: the pickup 200.00 CNY above its accrual, the CFS handling as accrued; the controller matches
	// and approves their own variance, as the authorizer allows them
	a.must(f.module.RecordCreditorInvoice(ctx, a.record("CI-TR-1", "trucker", "TR-88", 6, "CNY", line("PICKUP", 170_000), line("CFS_HANDLING", 50_000))))
	a.must(f.module.MatchCreditorInvoice(ctx, a.match("CI-TR-1", "controller", 6, onJob("JOB-A", 1, 1, 170_000, true), onPool("P-1", 2, 2, 50_000, true))))
	if r = a.must(f.module.ApproveCostVariance(ctx, api.ApproveCostVariance{CommandID: "approve-tr", TenantID: f.tenant, ActorID: "controller",
		InvoiceID: "CI-TR-1", ExpectedRevision: a.invoice("CI-TR-1").Revision, Reason: "waiting time agreed with the trucker",
		OccurredAt: a.hours(7)})); r.Version != 0 {
		t.Fatalf("approved: %+v", r)
	}
	if tr := a.invoice("CI-TR-1"); !tr.SelfApproved || tr.AwaitingApproval || tr.ApprovedBy != "controller" {
		t.Fatalf("CI-TR-1 approved by its matcher: %+v", tr)
	}
	awaiting, err := f.module.ListCreditorInvoices(ctx, api.ListCreditorInvoices{TenantID: f.tenant, LegalEntityID: "LE", AwaitingApproval: true})
	if err != nil || len(awaiting) != 1 || awaiting[0].InvoiceID != "CI-MU-2" {
		t.Fatalf("awaiting approval: %+v %v", awaiting, err)
	}
	_, err = f.module.ApproveCostVariance(ctx, api.ApproveCostVariance{CommandID: "approve-own", TenantID: f.tenant, ActorID: "ap-1", InvoiceID: "CI-MU-2",
		ExpectedRevision: mu2.Revision, Reason: "fine", OccurredAt: a.hours(7)})
	a.rejected("the matcher approving their own", err, "FIN_VARIANCE_APPROVAL")
	_, err = f.module.CloseConsolCostPool(ctx, api.CloseConsolCostPool{CommandID: "close-early", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-1",
		ExpectedRevision: a.pool("P-1").Revision, OccurredAt: a.hours(7)})
	a.rejected("closed while a variance awaits approval", err, "FIN_POOL_NOT_CLOSABLE")
	a.must(f.module.ApproveCostVariance(ctx, api.ApproveCostVariance{CommandID: "approve-mu", TenantID: f.tenant, ActorID: "ctrl-1", InvoiceID: "CI-MU-2",
		ExpectedRevision: mu2.Revision, Reason: "MU's chargeable weight checked", OccurredAt: a.hours(7)}))

	// JOB-A: the pickup matched final; its profit carries the actual cost and its pool shares
	jl := a.job("JOB-A").Lines[0]
	if jl.CostStatus != api.StatusMatched || jl.CostMatched != cny(170_000) || jl.CostMatchedLocal != cny(170_000) {
		t.Fatalf("JOB-A pickup: %+v", jl)
	}
	profit, err := f.module.GetJobProfit(ctx, api.GetJobProfit{TenantID: f.tenant, JobID: "JOB-A"})
	if err != nil || profit.Cost != cny(170_000) || profit.CostMatched != cny(170_000) || profit.CostAccrued != cny(0) || profit.PoolCost != cny(427_404+33_333) ||
		profit.Profit != cny(200_000-170_000-427_404-33_333) {
		t.Fatalf("JOB-A profit: %+v %v", profit, err)
	}
	_, err = f.module.ReviseChargeSide(ctx, api.ReviseChargeSide{CommandID: "revise-pickup", TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-A",
		ExpectedRevision: a.job("JOB-A").Revision, LineNo: 1, Side: "COST", ExpectedSideVersion: 1, Value: api.ChargeSide{Amount: cny(1)}, Reason: "x",
		OccurredAt: a.hours(8)})
	a.rejected("a cost side revised once matched final", err, "FIN_ACCRUAL_CLOSED")
	_, err = f.module.CancelFreightJob(ctx, api.CancelFreightJob{CommandID: "cancel-a", TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-A",
		ExpectedRevision: a.job("JOB-A").Revision, Reason: "x", OccurredAt: a.hours(8)})
	a.rejected("a job with a matched cost side cancelled", err, "FIN_ACCRUAL_MATCHED")

	// closing: the apportionment divided MU's freight at its accrual; it is apportioned again at its final cost
	closeCmd := func(id string, h int) api.CloseConsolCostPool {
		return api.CloseConsolCostPool{CommandID: id, TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-1", ExpectedRevision: a.pool("P-1").Revision,
			OccurredAt: a.hours(h)}
	}
	_, err = f.module.CloseConsolCostPool(ctx, closeCmd("close-stale", 8))
	a.rejected("closed on an apportionment at the accrual", err, "FIN_POOL_NOT_CLOSABLE")
	a.must(f.module.ApportionConsolCosts(ctx, api.ApportionConsolCosts{CommandID: "apportion-final", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-1",
		ExpectedRevision: a.pool("P-1").Revision, OccurredAt: a.hours(8)}))
	final := a.pool("P-1").Apportionments[1].Lines[0]
	if final.Amount != usd(105_000) || final.Local != cny(756_000) || final.Shares[0].Amount != usd(63_000) || final.Shares[0].Local != cny(453_600) ||
		final.Shares[1].Local != cny(302_400) {
		t.Fatalf("MU's freight apportioned at its final cost: %+v", final)
	}
	dispute := func(id, invoice string, h int) api.DisputeCreditorInvoice {
		return api.DisputeCreditorInvoice{CommandID: id, TenantID: f.tenant, ActorID: "ap-1", InvoiceID: invoice, ExpectedRevision: a.invoice(invoice).Revision,
			Reason: "MU billed the wrong chargeable weight", OccurredAt: a.hours(h)}
	}
	if r = a.must(f.module.DisputeCreditorInvoice(ctx, dispute("dispute-mu2", "CI-MU-2", 9))); r.Version != 1 {
		t.Fatalf("disputed: %+v", r)
	}
	_, err = f.module.CloseConsolCostPool(ctx, closeCmd("close-disputed", 9))
	a.rejected("closed while an invoice is disputed", err, "FIN_POOL_NOT_CLOSABLE")
	a.must(f.module.ResolveCreditorInvoiceDispute(ctx, api.ResolveCreditorInvoiceDispute{CommandID: "resolve-mu2", TenantID: f.tenant, ActorID: "ap-1",
		InvoiceID: "CI-MU-2", ExpectedRevision: a.invoice("CI-MU-2").Revision, Reason: "the weight was right", OccurredAt: a.hours(9)}))
	if r = a.must(f.module.CloseConsolCostPool(ctx, closeCmd("close", 10))); r.Version != 1 {
		t.Fatalf("closed: %+v", r)
	}
	if p := a.pool("P-1"); p.State != api.PoolClosed || len(p.Closures) != 1 || p.Closures[0].ClosedBy != "fin-1" || p.Lines[0].Status != api.StatusMatched ||
		p.Lines[1].Status != api.StatusMatched {
		t.Fatalf("closed pool: %+v", p)
	}
	consol, err := f.module.GetConsolProfit(ctx, api.GetConsolProfit{TenantID: f.tenant, ConsolidationID: "CONSOL-1"})
	if err != nil || consol.State != api.PoolClosed || consol.Pooled != cny(806_000) || consol.Matched != cny(806_000) || consol.Apportioned != cny(806_000) ||
		consol.Unapportioned != cny(0) {
		t.Fatalf("consol profit: %+v %v", consol, err)
	}
	_, err = f.module.RecordConsolCost(ctx, api.RecordConsolCost{CommandID: "late-cost", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-1",
		ExpectedRevision: a.pool("P-1").Revision, ChargeCode: "X", Method: api.MethodPieces, Cost: api.ChargeSide{Amount: cny(1)}, Reason: "late",
		OccurredAt: a.hours(11)})
	a.rejected("a closed pool takes a cost", err, "CLOSED")
	_, err = f.module.DisputeCreditorInvoice(ctx, dispute("dispute-mu1", "CI-MU-1", 11))
	a.rejected("a dispute of an invoice of a closed pool", err, "FIN_TARGET_CLOSED")

	// reopened for MU's credit: CI-MU-2 disputed and cancelled, the rest of the freight released, the pool closed again
	a.must(f.module.ReopenConsolCostPool(ctx, api.ReopenConsolCostPool{CommandID: "reopen", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-1",
		ExpectedRevision: a.pool("P-1").Revision, Reason: "MU withdrew its second invoice", OccurredAt: a.hours(11)}))
	a.must(f.module.DisputeCreditorInvoice(ctx, dispute("dispute-mu2-again", "CI-MU-2", 11)))
	_, err = f.module.CancelCreditorInvoice(ctx, api.CancelCreditorInvoice{CommandID: "cancel-mu1", TenantID: f.tenant, ActorID: "ap-1", InvoiceID: "CI-MU-1",
		ExpectedRevision: a.invoice("CI-MU-1").Revision, Reason: "x", OccurredAt: a.hours(11)})
	a.rejected("a matched invoice cancelled", err, "dispute")
	a.must(f.module.CancelCreditorInvoice(ctx, api.CancelCreditorInvoice{CommandID: "cancel-mu2", TenantID: f.tenant, ActorID: "ap-1", InvoiceID: "CI-MU-2",
		ExpectedRevision: a.invoice("CI-MU-2").Revision, Reason: "withdrawn by MU", OccurredAt: a.hours(11)}))
	if pl = a.pool("P-1").Lines[0]; pl.Status != api.StatusPartlyMatched || pl.Matched != usd(60_000) {
		t.Fatalf("P-1 line 1 open again: %+v", pl)
	}
	if r = a.must(f.module.ReleasePoolAccrual(ctx, api.ReleasePoolAccrual{CommandID: "release-freight", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-1",
		ExpectedRevision: a.pool("P-1").Revision, LineNo: 1, ExpectedVersion: 1, Reason: "MU will not invoice the rest", OccurredAt: a.hours(12)})); r.Version != 2 {
		t.Fatalf("released: %+v", r)
	}
	if pl = a.pool("P-1").Lines[0]; pl.Status != api.StatusReleased || pl.Cost != usd(60_000) || pl.CostLocal != cny(432_000) || pl.Versions[1].Amount != usd(0) {
		t.Fatalf("P-1 line 1 released: %+v", pl)
	}
	a.must(f.module.RecordCreditorInvoice(ctx, a.record("CI-MU-3", "airline-mu", "MU-0003", 12, "USD", line("AIR_FREIGHT", 1_000))))
	_, err = f.module.MatchCreditorInvoice(ctx, a.match("CI-MU-3", "ap-1", 12, onPool("P-1", 1, 1, 1_000, true)))
	a.rejected("a match to a released accrual", err, "FIN_ACCRUAL_CLOSED")
	a.must(f.module.RecordCreditorInvoice(ctx, a.record("CI-MU-2B", "airline-mu", "MU-0002", 12, "USD", line("AIR_FREIGHT", 45_000))))
	a.must(f.module.ApportionConsolCosts(ctx, api.ApportionConsolCosts{CommandID: "apportion-released", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-1",
		ExpectedRevision: a.pool("P-1").Revision, LineNos: []int{1}, OccurredAt: a.hours(12)}))
	if r = a.must(f.module.CloseConsolCostPool(ctx, closeCmd("close-again", 13))); r.Version != 2 {
		t.Fatalf("closed again: %+v", r)
	}
	if p := a.pool("P-1"); len(p.Closures) != 2 || p.Closures[0].ReopenReason != "MU withdrew its second invoice" || p.State != api.PoolClosed {
		t.Fatalf("closures: %+v", p.Closures)
	}

	// JOB-B's delivery released without an invoice: its cost is nothing, and the job is not cancelled
	if r = a.must(f.module.ReleaseJobAccrual(ctx, api.ReleaseJobAccrual{CommandID: "release-delivery", TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-B",
		ExpectedRevision: a.job("JOB-B").Revision, LineNo: 1, ExpectedSideVersion: 1, Reason: "delivered by the consignee's own truck",
		OccurredAt: a.hours(13)})); r.Version != 2 {
		t.Fatalf("job accrual released: %+v", r)
	}
	if jl = a.job("JOB-B").Lines[0]; jl.CostStatus != api.StatusReleased || jl.Cost[1].Status != api.StatusReleased {
		t.Fatalf("JOB-B delivery: %+v", jl)
	}
	if p, err := f.module.GetJobProfit(ctx, api.GetJobProfit{TenantID: f.tenant, JobID: "JOB-B"}); err != nil || p.Cost != cny(0) {
		t.Fatalf("JOB-B profit: %+v %v", p, err)
	}
	if policy, err := f.module.GetFinancePolicy(ctx, api.GetFinancePolicy{TenantID: f.tenant, LegalEntityID: "LE"}); err != nil ||
		len(policy.VarianceThresholds) != 1 || policy.VarianceThresholds[0].Version != 1 || policy.VarianceThresholds[0].Amount != cny(10_000) ||
		policy.VarianceThresholds[0].RateBP != 100 || !policy.VarianceThresholds[0].SetAt.Equal(a.hours(0)) {
		t.Fatalf("thresholds: %+v %v", policy.VarianceThresholds, err)
	}

	// JOB-C and P-2 for what the database refuses past FIN: JOB-C's screening matched final, its freight in part
	a.w.drivers["SH-C"] = withShipment(a.w.drivers["SH-B"], "SH-C")
	a.must(f.module.OpenFreightJob(ctx, api.OpenFreightJob{CommandID: "open-c", TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-C", ShipmentID: "SH-C",
		OccurredAt: a.hours(13)}))
	for i, code := range []string{"SCREENING", "AIR_FREIGHT"} {
		a.must(f.module.RecordChargeLine(ctx, api.RecordChargeLine{CommandID: "c-" + code, TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-C",
			ExpectedRevision: int64(i + 1), ChargeCode: code, Cost: &api.ChargeSide{Amount: usd(10_000)}, Reason: "estimate", OccurredAt: a.hours(13)}))
	}
	a.must(f.module.RecordCreditorInvoice(ctx, a.record("CI-MU-4", "airline-mu", "MU-0004", 14, "USD", line("SCREENING", 10_000))))
	a.must(f.module.MatchCreditorInvoice(ctx, a.match("CI-MU-4", "ap-1", 14, onJob("JOB-C", 1, 1, 10_000, true))))
	if mu4 := a.invoice("CI-MU-4"); mu4.ApprovalRequired || mu4.Matches[0].VarianceLocal != cny(766) {
		t.Fatalf("an exchange difference within the threshold: %+v", mu4)
	}
	a.must(f.module.RecordCreditorInvoice(ctx, a.record("CI-MU-5", "airline-mu", "MU-0005", 14, "USD", line("AIR_FREIGHT", 6_000))))
	a.must(f.module.MatchCreditorInvoice(ctx, a.match("CI-MU-5", "ap-1", 14, onJob("JOB-C", 2, 1, 6_000, false))))
	a.must(f.module.OpenConsolCostPool(ctx, api.OpenConsolCostPool{CommandID: "pool-2", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-2",
		ConsolidationID: "CONSOL-2", OccurredAt: a.hours(14)}))
	a.must(f.module.RecordConsolCost(ctx, api.RecordConsolCost{CommandID: "p2-cost", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-2", ExpectedRevision: 1,
		ChargeCode: "AIR_FREIGHT", Method: api.MethodChargeable, Cost: api.ChargeSide{Amount: cny(10_000)}, Reason: "estimate", OccurredAt: a.hours(14)}))

	counts := map[string]int{}
	for _, kind := range f.outboxTypes(t) {
		counts[kind]++
	}
	for kind, want := range map[string]int{"CostVarianceThresholdSet": 1, "CreditorInvoiceRecorded": 7, "CreditorInvoiceMatched": 5, "CostVarianceApproved": 2,
		"CreditorInvoiceDisputed": 2, "CreditorInvoiceDisputeResolved": 1, "CreditorInvoiceCancelled": 1, "AccrualReleased": 2, "ConsolCostPoolClosed": 2,
		"ConsolCostPoolReopened": 1} {
		if counts[kind] != want {
			t.Errorf("outbox: %d %s, want %d", counts[kind], kind, want)
		}
	}

	matchRow := `INSERT INTO cargo_fin.creditor_invoice_match (tenant_id,invoice_id,match_no,line_no,target_kind,job_id,cost_side,target_line,target_version,
        amount_minor,final,reason,local_minor,rate_entry_no,rate_type,rate,scale,from_digits,to_digits,rate_source,accrual_amount_minor,accrual_local_minor,
        prior_amount_minor,prior_local_minor,variance_minor,variance_local_minor) VALUES `
	matchInvoice := func(required bool) string {
		return `UPDATE cargo_fin.creditor_invoice SET state='MATCHED',matched_by='x',matched_at=now(),match_entered_at=now(),approval_required=` +
			map[bool]string{true: "true", false: "false"}[required] + `,threshold_no=1,revision=revision+1 WHERE tenant_id=$1 AND invoice_id='CI-MU-2B';
`
	}
	for name, tc := range map[string]struct {
		sql, state, check string
	}{
		"a match rewritten": {`UPDATE cargo_fin.creditor_invoice_match SET amount_minor=1 WHERE tenant_id=$1`, "42501|23001", ""},
		"a match deleted":   {`DELETE FROM cargo_fin.creditor_invoice_match WHERE tenant_id=$1`, "42501|23001", ""},
		"a match behind its invoice": {matchRow + `($1,'CI-MU-2B',1,1,'JOB_COST','JOB-C','COST',2,1,4000,false,NULL,28800,2,'ACCOUNTING',72000,4,2,2,
            'monthly rate 2026-11',10000,71234,6000,43200,0,0)`, "23514", "fin_records_with_parent"},
		"an approval dropped": {`UPDATE cargo_fin.creditor_invoice SET approval_required=false,revision=revision+1 WHERE tenant_id=$1 AND invoice_id='CI-TR-1'`,
			"23001", ""},
		"a matched invoice cancelled": {`UPDATE cargo_fin.creditor_invoice SET state='CANCELLED',cancel_reason='x',cancelled_by='x',cancelled_at=now(),
            cancel_entered_at=now(),revision=revision+1 WHERE tenant_id=$1 AND invoice_id='CI-MU-1'`, "23001", ""},
		"a dispute of an invoice of a closed pool": {`UPDATE cargo_fin.creditor_invoice SET state='DISPUTED',revision=revision+1 WHERE tenant_id=$1
            AND invoice_id='CI-MU-1';
INSERT INTO cargo_fin.creditor_invoice_dispute (tenant_id,invoice_id,dispute_no,from_state,reason,disputed_by,disputed_at,entered_at)
            VALUES ($1,'CI-MU-1',1,'MATCHED','x','x',now(),now())`, "23514", "creditor_invoice_targets_open"},
		"a match beyond its accrual": {matchInvoice(false) + matchRow + `($1,'CI-MU-2B',1,1,'JOB_COST','JOB-C','COST',2,1,45000,false,NULL,324000,2,
            'ACCOUNTING',72000,4,2,2,'monthly rate 2026-11',10000,71234,6000,43200,0,0)`, "23514", "creditor_match_consistent"},
		"a second final match": {matchInvoice(true) + matchRow + `($1,'CI-MU-2B',1,1,'JOB_COST','JOB-C','COST',1,1,45000,true,'x',324000,2,'ACCOUNTING',
            72000,4,2,2,'monthly rate 2026-11',10000,71234,10000,72000,45000,324766)`, "23514", "creditor_match_consistent"},
		"an approval not required where it is": {matchInvoice(false) + matchRow + `($1,'CI-MU-2B',1,1,'JOB_COST','JOB-C','COST',2,1,45000,true,NULL,324000,2,
            'ACCOUNTING',72000,4,2,2,'monthly rate 2026-11',10000,71234,6000,43200,41000,295966)`, "23514", "creditor_invoice_consistent"},
		"a cost side revised below its matches": {`UPDATE cargo_fin.freight_job SET revision=revision+1 WHERE tenant_id=$1 AND job_id='JOB-C';
INSERT INTO cargo_fin.charge_side_version (tenant_id,job_id,line_no,side,version_no,party_revision,currency,amount_minor,local_minor,rate_entry_no,rate_type,
            rate,scale,from_digits,to_digits,rate_source,status,reason,recorded_by,recorded_at,entered_at) VALUES ($1,'JOB-C',2,'COST',2,0,'USD',5000,35617,1,
            'ESTIMATE',71234,4,2,2,'CFETS central parity','ACCRUED','x','x',now(),now())`, "23514", "accrual_consumption_consistent"},
		"a job with matched costs cancelled": {`UPDATE cargo_fin.freight_job SET state='CANCELLED',cancel_reason='x',cancelled_by='x',cancelled_at=now(),
            cancel_entered_at=now(),revision=revision+1 WHERE tenant_id=$1 AND job_id='JOB-C'`, "23514", "accrual_consumption_consistent"},
		"a version after a release": {`UPDATE cargo_fin.freight_job SET revision=revision+1 WHERE tenant_id=$1 AND job_id='JOB-B';
INSERT INTO cargo_fin.charge_side_version (tenant_id,job_id,line_no,side,version_no,party_id,party_revision,currency,amount_minor,local_minor,status,reason,
            recorded_by,recorded_at,entered_at) VALUES ($1,'JOB-B',1,'COST',3,'trucker',4,'CNY',1000,1000,'ACCRUED','x','x',now(),now())`,
			"23514", "accrual_consumption_consistent"},
		"a release with an amount": {`INSERT INTO cargo_fin.charge_side_version (tenant_id,job_id,line_no,side,version_no,party_revision,currency,amount_minor,
            local_minor,status,reason,recorded_by,recorded_at,entered_at) VALUES ($1,'JOB-C',2,'COST',2,0,'USD',5,36,'RELEASED','x','x',now(),now())`,
			"23514", "charge_side_released"},
		"a pool closed with a line accrued": {`UPDATE cargo_fin.consol_cost_pool SET state='CLOSED',revision=revision+1 WHERE tenant_id=$1 AND pool_id='P-2';
INSERT INTO cargo_fin.consol_cost_pool_closure (tenant_id,pool_id,closure_no,closed_by,closed_at,entered_at) VALUES ($1,'P-2',1,'x',now(),now())`,
			"23514", "consol_cost_pool_closable"},
		"a closure reopened twice": {`UPDATE cargo_fin.consol_cost_pool_closure SET reopened_by='y' WHERE tenant_id=$1 AND pool_id='P-1' AND closure_no=1`,
			"42501|23001", ""},
		"an apportionment at another cost": {`UPDATE cargo_fin.consol_cost_pool SET revision=revision+1 WHERE tenant_id=$1 AND pool_id='P-2';
INSERT INTO cargo_fin.pool_apportionment (tenant_id,pool_id,apportionment_no,basis_revision,stage,made_by,made_at,entered_at)
            VALUES ($1,'P-2',1,7,'ACTUAL','x',now(),now());
INSERT INTO cargo_fin.pool_apportionment_line (tenant_id,pool_id,apportionment_no,line_no,line_version,method,reason,amount_minor,local_minor)
            VALUES ($1,'P-2',1,1,1,'MANUAL','x',9999,9999);
INSERT INTO cargo_fin.pool_apportionment_share (tenant_id,pool_id,apportionment_no,line_no,share_no,shipment_id,job_id,quantity,unit,amount_minor,local_minor)
            VALUES ($1,'P-2',1,1,1,'SH-A','JOB-A',9999,NULL,9999,9999)`, "23514", "pool_apportionment_cost"},
		"a supplier number twice": {`INSERT INTO cargo_fin.creditor_invoice (tenant_id,invoice_id,legal_entity_id,creditor_party_id,creditor_revision,
            supplier_invoice_no,invoice_date,currency,total_minor,state,recorded_by,recorded_at,entered_at) VALUES ($1,'CI-DUP','LE','airline-mu',4,'MU-0001',
            current_date,'USD',1,'RECORDED','x',now(),now())`, "23505", "creditor_invoice_number_once"},
		"a threshold out of turn": {`UPDATE cargo_fin.finance_policy SET revision=revision+1 WHERE tenant_id=$1 AND legal_entity_id='LE';
INSERT INTO cargo_fin.cost_variance_threshold (tenant_id,legal_entity_id,threshold_no,amount_minor,rate_bp,set_by,set_at,entered_at)
            VALUES ($1,'LE',3,0,0,'x',now(),now())`, "23514", "finance_policy_consistent"},
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

// race runs two commands at once, eight rounds each on a fresh world, and requires exactly one of them to commit; lost
// checks the loser's error. One side starts up to 3 ms later, the first in odd rounds and the second in even ones, so
// that each wins some rounds although one of them asks before its transaction; it reports how often each lost.
func race(t *testing.T, setup func(t *testing.T) ap, first, second func(a ap) error, lost func(round int, firstLost bool, err error)) {
	t.Helper()
	var losses [2]int
	for round := range 8 {
		a := setup(t)
		var errs [2]error
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		for i, fn := range []func(ap) error{first, second} {
			go func() {
				defer wg.Done()
				<-start
				if i == round%2 {
					time.Sleep(time.Duration(rand.IntN(3000)) * time.Microsecond)
				}
				errs[i] = fn(a)
			}()
		}
		close(start)
		wg.Wait()
		switch {
		case errs[0] == nil && errs[1] == nil:
			t.Fatalf("round %d: both committed", round)
		case errs[0] != nil && errs[1] != nil:
			t.Fatalf("round %d: both refused: %v / %v", round, errs[0], errs[1])
		case errs[0] != nil:
			losses[0]++
			lost(round, true, errs[0])
		default:
			losses[1]++
			lost(round, false, errs[1])
		}
	}
	t.Logf("the first lost %d rounds, the second %d", losses[0], losses[1])
}

// TestMatchesRaceForOneAccrual races two trucker invoices, each matching 600.00 of JOB-A's 1,500.00 pickup accrual
// in part, with JOB-A's pickup revised to 1,000.00 first: together they would consume more than it has open, so
// exactly one commits and the other hears FIN_MATCH_EXCEEDS.
func TestMatchesRaceForOneAccrual(t *testing.T) {
	setup := func(t *testing.T) ap {
		a := creditorWorld(t)
		a.must(a.module.ReviseChargeSide(a.ctx, api.ReviseChargeSide{CommandID: "lower", TenantID: a.tenant, ActorID: "fin-1", JobID: "JOB-A",
			ExpectedRevision: a.job("JOB-A").Revision, LineNo: 1, Side: "COST", ExpectedSideVersion: 1, Value: api.ChargeSide{Amount: cny(100_000)},
			Reason: "trucker's quote", OccurredAt: a.hours(3)}))
		for _, id := range []string{"CI-1", "CI-2"} {
			a.must(a.module.RecordCreditorInvoice(a.ctx, a.record(id, "trucker", "TR-"+id, 3, "CNY", line("PICKUP", 60_000))))
		}
		return a
	}
	matchOne := func(id string) func(a ap) error {
		return func(a ap) error {
			_, err := a.module.MatchCreditorInvoice(a.ctx, api.MatchCreditorInvoice{CommandID: "m-" + id, TenantID: a.tenant, ActorID: "ap-1", InvoiceID: id,
				ExpectedRevision: 1, Matches: []api.CreditorMatch{onJob("JOB-A", 1, 1, 60_000, false)}, OccurredAt: a.hours(4)})
			return err
		}
	}
	race(t, setup, matchOne("CI-1"), matchOne("CI-2"), func(round int, _ bool, err error) {
		if !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "FIN_MATCH_EXCEEDS") {
			t.Fatalf("round %d: refused for another reason: %v", round, err)
		}
	})
}

// TestDisputeRacesPoolClose races the dispute of MU's final invoice against closing its pool: a dispute first holds
// the pool from closing (FIN_POOL_NOT_CLOSABLE); a closing first leaves the dispute refused (FIN_TARGET_CLOSED).
func TestDisputeRacesPoolClose(t *testing.T) {
	setup := func(t *testing.T) ap {
		a := creditorWorld(t)
		a.must(a.module.RecordCreditorInvoice(a.ctx, a.record("CI-MU", "airline-mu", "MU-1", 3, "USD", line("AIR_FREIGHT", 100_000))))
		// an exchange difference of 76.60 CNY: within the threshold, no approval
		a.must(a.module.MatchCreditorInvoice(a.ctx, a.match("CI-MU", "ap-1", 3, onPool("P-1", 1, 1, 100_000, true))))
		a.must(a.module.RecordCreditorInvoice(a.ctx, a.record("CI-CFS", "trucker", "CFS-1", 3, "CNY", line("CFS_HANDLING", 50_000))))
		a.must(a.module.MatchCreditorInvoice(a.ctx, a.match("CI-CFS", "ap-1", 3, onPool("P-1", 2, 1, 50_000, true))))
		a.must(a.module.ApportionConsolCosts(a.ctx, api.ApportionConsolCosts{CommandID: "apportion-final", TenantID: a.tenant, ActorID: "fin-1",
			PoolID: "P-1", ExpectedRevision: a.pool("P-1").Revision, OccurredAt: a.hours(3)}))
		return a
	}
	closing := func(a ap) error {
		_, err := a.module.CloseConsolCostPool(a.ctx, api.CloseConsolCostPool{CommandID: "close", TenantID: a.tenant, ActorID: "fin-1", PoolID: "P-1",
			ExpectedRevision: 5, OccurredAt: a.hours(4)})
		return err
	}
	disputing := func(a ap) error {
		_, err := a.module.DisputeCreditorInvoice(a.ctx, api.DisputeCreditorInvoice{CommandID: "dispute", TenantID: a.tenant, ActorID: "ap-1",
			InvoiceID: "CI-MU", ExpectedRevision: 2, Reason: "weight", OccurredAt: a.hours(4)})
		return err
	}
	race(t, setup, closing, disputing, func(round int, closeLost bool, err error) {
		want := "FIN_TARGET_CLOSED"
		if closeLost {
			want = "FIN_POOL_NOT_CLOSABLE"
		}
		if !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), want) {
			t.Fatalf("round %d: refused for another reason: %v, want %s", round, err, want)
		}
	})
}

// TestRevisionRacesAMatch races lowering JOB-A's pickup accrual to 500.00 against a trucker's invoice matching 800.00
// of it in part: a revision first leaves the match beyond the accrual (FIN_MATCH_EXCEEDS); a match first leaves the
// revision below what the invoice consumes (FIN_ACCRUAL_MATCHED).
func TestRevisionRacesAMatch(t *testing.T) {
	setup := func(t *testing.T) ap {
		a := creditorWorld(t)
		a.must(a.module.RecordCreditorInvoice(a.ctx, a.record("CI-TR", "trucker", "TR-1", 3, "CNY", line("PICKUP", 80_000))))
		return a
	}
	revising := func(a ap) error {
		_, err := a.module.ReviseChargeSide(a.ctx, api.ReviseChargeSide{CommandID: "lower", TenantID: a.tenant, ActorID: "fin-1", JobID: "JOB-A",
			ExpectedRevision: 2, LineNo: 1, Side: "COST", ExpectedSideVersion: 1, Value: api.ChargeSide{Amount: cny(50_000)}, Reason: "cheaper truck",
			OccurredAt: a.hours(4)})
		return err
	}
	matching := func(a ap) error {
		_, err := a.module.MatchCreditorInvoice(a.ctx, api.MatchCreditorInvoice{CommandID: "match", TenantID: a.tenant, ActorID: "ap-1", InvoiceID: "CI-TR",
			ExpectedRevision: 1, Matches: []api.CreditorMatch{onJob("JOB-A", 1, 1, 80_000, false)}, OccurredAt: a.hours(4)})
		return err
	}
	race(t, setup, revising, matching, func(round int, revisionLost bool, err error) {
		want := "FIN_MATCH_EXCEEDS"
		if revisionLost {
			want = "FIN_ACCRUAL_MATCHED"
		}
		if !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), want) {
			t.Fatalf("round %d: refused for another reason: %v, want %s", round, err, want)
		}
	})
}

// The independent review of FIN V0.5, on PostgreSQL: a threshold never strands an invoice matched before it and is
// not set behind a matching; an invoice is not cancelled behind another's final match; large amounts round alike in
// Go and SQL; and the database refuses, past FIN, an approval where none is required, records written into a closed
// pool or onto a cancelled job, a match at REPEATABLE READ, a cancellation behind a final match and an accrual naming
// another creditor than its matching invoices.
func TestReviewFindingsOnPostgres(t *testing.T) {
	a := creditorWorld(t)
	ctx, f := a.ctx, a.fixture
	policyRevision := func() int64 {
		p, err := f.module.GetFinancePolicy(ctx, api.GetFinancePolicy{TenantID: f.tenant, LegalEntityID: "LE"})
		if err != nil {
			t.Fatal(err)
		}
		return p.Revision
	}
	threshold := func(id string, h int, amount int64) error {
		_, err := f.module.SetCostVarianceThreshold(ctx, api.SetCostVarianceThreshold{CommandID: id, TenantID: f.tenant, ActorID: "acct-1", LegalEntityID: "LE",
			ExpectedRevision: policyRevision(), Amount: amount, RateBP: 100, OccurredAt: a.hours(h)})
		return err
	}

	// the trucker's pickup 200.00 above its accrual awaits approval under threshold 1; a threshold dated before that
	// matching is refused, a later one leaves the invoice as it was
	a.must(f.module.RecordCreditorInvoice(ctx, a.record("CI-T1", "trucker", "T1", 5, "CNY", line("PICKUP", 170_000))))
	a.must(f.module.MatchCreditorInvoice(ctx, a.match("CI-T1", "ap-1", 5, onJob("JOB-A", 1, 1, 170_000, true))))
	a.rejected("a threshold dated before a matching", threshold("threshold-back", 3, 50_000), "a threshold is set after it")
	a.must(api.Receipt{}, threshold("threshold-2", 6, 50_000))
	a.must(f.module.ApproveCostVariance(ctx, api.ApproveCostVariance{CommandID: "approve-t1", TenantID: f.tenant, ActorID: "ctrl-1", InvoiceID: "CI-T1",
		ExpectedRevision: a.invoice("CI-T1").Revision, Reason: "waiting time", OccurredAt: a.hours(7)}))
	if t1 := a.invoice("CI-T1"); t1.ThresholdVersion != 1 || t1.AwaitingApproval {
		t.Fatalf("CI-T1 keeps threshold 1 and is approved: %+v", t1)
	}

	// JOB-B's delivery: CI-B1 matches part, CI-B2 closes it; CI-B1 is not cancelled behind CI-B2's final match
	a.must(f.module.RecordCreditorInvoice(ctx, a.record("CI-B1", "trucker", "B1", 5, "CNY", line("DELIVERY", 10_000))))
	a.must(f.module.MatchCreditorInvoice(ctx, a.match("CI-B1", "ap-1", 5, onJob("JOB-B", 1, 1, 10_000, false))))
	a.must(f.module.RecordCreditorInvoice(ctx, a.record("CI-B2", "trucker", "B2", 5, "CNY", line("DELIVERY", 20_000))))
	a.must(f.module.MatchCreditorInvoice(ctx, a.match("CI-B2", "ap-1", 5, onJob("JOB-B", 1, 1, 20_000, true))))
	a.must(f.module.DisputeCreditorInvoice(ctx, api.DisputeCreditorInvoice{CommandID: "dispute-b1", TenantID: f.tenant, ActorID: "ap-1", InvoiceID: "CI-B1",
		ExpectedRevision: a.invoice("CI-B1").Revision, Reason: "withdrawn", OccurredAt: a.hours(7)}))
	_, err := f.module.CancelCreditorInvoice(ctx, api.CancelCreditorInvoice{CommandID: "cancel-b1", TenantID: f.tenant, ActorID: "ap-1", InvoiceID: "CI-B1",
		ExpectedRevision: a.invoice("CI-B1").Revision, Reason: "withdrawn", OccurredAt: a.hours(7)})
	a.rejected("cancelled behind a final match", err, "FIN_FINAL_MATCH_BEHIND")

	// 1,234,567.89 USD accrued and 354,665.27 matched in part: the open part's local amount rounds once, as in Go
	a.must(f.module.OpenConsolCostPool(ctx, api.OpenConsolCostPool{CommandID: "pool-2", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-2",
		ConsolidationID: "CONSOL-2", OccurredAt: a.hours(3)}))
	a.must(f.module.RecordConsolCost(ctx, api.RecordConsolCost{CommandID: "p2-freight", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-2",
		ExpectedRevision: 1, ChargeCode: "AIR_FREIGHT", Method: api.MethodChargeable, Cost: api.ChargeSide{PartyID: "airline-mu", Amount: usd(123_456_789)},
		Reason: "MU's charter rate", OccurredAt: a.hours(3)}))
	a.must(f.module.RecordCreditorInvoice(ctx, a.record("CI-BIG", "airline-mu", "MU-BIG", 5, "USD", line("AIR_FREIGHT", 35_466_527))))
	a.must(f.module.MatchCreditorInvoice(ctx, a.match("CI-BIG", "ap-1", 7, onPool("P-2", 1, 1, 35_466_527, false))))
	a.must(f.module.ApportionConsolCosts(ctx, api.ApportionConsolCosts{CommandID: "p2-apportion", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-2",
		ExpectedRevision: a.pool("P-2").Revision, OccurredAt: a.hours(7)}))
	p2 := a.pool("P-2")
	if la := p2.Apportionments[0].Lines[0]; la.Local != p2.Lines[0].CostLocal || la.Amount != usd(123_456_789) {
		t.Fatalf("P-2 apportioned at %+v, its cost %v", la, p2.Lines[0].CostLocal)
	}

	// P-1 closed: MU's freight and the CFS handling matched final and apportioned at their final costs
	a.must(f.module.RecordCreditorInvoice(ctx, a.record("CI-MU", "airline-mu", "MU-1", 5, "USD", line("AIR_FREIGHT", 100_000))))
	a.must(f.module.MatchCreditorInvoice(ctx, a.match("CI-MU", "ap-1", 7, onPool("P-1", 1, 1, 100_000, true))))
	a.must(f.module.RecordCreditorInvoice(ctx, a.record("CI-CFS", "trucker", "CFS-1", 5, "CNY", line("CFS_HANDLING", 50_000))))
	a.must(f.module.MatchCreditorInvoice(ctx, a.match("CI-CFS", "ap-1", 7, onPool("P-1", 2, 1, 50_000, true))))
	a.must(f.module.ApportionConsolCosts(ctx, api.ApportionConsolCosts{CommandID: "p1-final", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-1",
		ExpectedRevision: a.pool("P-1").Revision, OccurredAt: a.hours(8)}))
	a.must(f.module.CloseConsolCostPool(ctx, api.CloseConsolCostPool{CommandID: "p1-close", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-1",
		ExpectedRevision: a.pool("P-1").Revision, OccurredAt: a.hours(8)}))

	// JOB-C, open with a delivery accrued; CI-R recorded
	a.w.drivers["SH-C"] = withShipment(a.w.drivers["SH-B"], "SH-C")
	a.must(f.module.OpenFreightJob(ctx, api.OpenFreightJob{CommandID: "open-c", TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-C", ShipmentID: "SH-C",
		OccurredAt: a.hours(8)}))
	a.must(f.module.RecordChargeLine(ctx, api.RecordChargeLine{CommandID: "c-delivery", TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-C",
		ExpectedRevision: 1, ChargeCode: "DELIVERY", Cost: &api.ChargeSide{Amount: cny(10_000)}, Reason: "estimate", OccurredAt: a.hours(8)}))
	a.must(f.module.RecordCreditorInvoice(ctx, a.record("CI-R", "trucker", "R1", 8, "CNY", line("DELIVERY", 5_000))))
	p2Local := p2.Lines[0].Versions[0].Local.Minor

	for name, tc := range map[string]struct {
		sql, state, check string
	}{
		"an approval of a recorded invoice": {`UPDATE cargo_fin.creditor_invoice SET approved_by='x',approval_reason='x',self_approved=false,approved_at=now(),
            approval_entered_at=now(),revision=revision+1 WHERE tenant_id=$1 AND invoice_id='CI-R'`, "23514", "creditor_invoice_approved"},
		"an apportionment of a closed pool": {`UPDATE cargo_fin.consol_cost_pool SET revision=revision+1 WHERE tenant_id=$1 AND pool_id='P-1';
INSERT INTO cargo_fin.pool_apportionment (tenant_id,pool_id,apportionment_no,basis_revision,stage,made_by,made_at,entered_at)
            VALUES ($1,'P-1',3,7,'ACTUAL','x',now(),now());
INSERT INTO cargo_fin.pool_apportionment_line (tenant_id,pool_id,apportionment_no,line_no,line_version,method,reason,amount_minor,local_minor)
            VALUES ($1,'P-1',3,2,1,'MANUAL','x',50000,50000);
INSERT INTO cargo_fin.pool_apportionment_share (tenant_id,pool_id,apportionment_no,line_no,share_no,shipment_id,job_id,quantity,unit,amount_minor,local_minor)
            VALUES ($1,'P-1',3,2,1,'SH-B','JOB-B',50000,NULL,50000,50000)`, "23514", "consol_cost_pool_open"},
		"a line in a closed pool at once": {`SET CONSTRAINTS ALL IMMEDIATE;
UPDATE cargo_fin.consol_cost_pool SET revision=revision+1 WHERE tenant_id=$1 AND pool_id='P-1';
INSERT INTO cargo_fin.pool_cost_line (tenant_id,pool_id,line_no,charge_code,method,recorded_by,recorded_at,entered_at)
            VALUES ($1,'P-1',3,'X','PIECES','x',now(),now())`, "23514", "consol_cost_pool_open"},
		"a release on a job cancelled at once": {`SET CONSTRAINTS ALL IMMEDIATE;
UPDATE cargo_fin.freight_job SET state='CANCELLED',cancel_reason='x',cancelled_by='x',cancelled_at=now(),cancel_entered_at=now(),revision=revision+1
            WHERE tenant_id=$1 AND job_id='JOB-C';
INSERT INTO cargo_fin.charge_side_version (tenant_id,job_id,line_no,side,version_no,party_revision,currency,amount_minor,local_minor,status,reason,
            recorded_by,recorded_at,entered_at) VALUES ($1,'JOB-C',1,'COST',2,0,'CNY',0,0,'RELEASED','x','x',now(),now())`,
			"23514", "accrual_consumption_consistent"},
		"a cancellation behind a final match": {`UPDATE cargo_fin.creditor_invoice SET state='CANCELLED',cancel_reason='x',cancelled_by='x',cancelled_at=now(),
            cancel_entered_at=now(),revision=revision+1 WHERE tenant_id=$1 AND invoice_id='CI-B1'`, "23514", "creditor_cancel_behind_final"},
		"an accrual naming another creditor than its invoices": {`UPDATE cargo_fin.consol_cost_pool SET revision=revision+1 WHERE tenant_id=$1 AND pool_id='P-2';
INSERT INTO cargo_fin.pool_cost_version (tenant_id,pool_id,line_no,version_no,creditor_party_id,creditor_revision,currency,amount_minor,local_minor,
            rate_entry_no,rate_type,rate,scale,from_digits,to_digits,rate_source,status,reason,recorded_by,recorded_at,entered_at) VALUES ($1,'P-2',1,2,
            'trucker',4,'USD',123456789,` + strconv.FormatInt(p2Local, 10) + `,1,'ESTIMATE',71234,4,2,2,'CFETS central parity','ACCRUED','x','x',now(),now())`,
			"23514", "accrual_consumption_consistent"},
	} {
		statements := strings.Split(tc.sql, ";\n")
		err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			for _, s := range statements {
				var args []any
				if strings.Contains(s, "$1") {
					args = append(args, f.tenant)
				}
				if _, err := e.Exec(ctx, s, args...); err != nil {
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

	// a match written at REPEATABLE READ, where the read after the lock would miss what others committed
	conn, err := pgx.Connect(ctx, os.Getenv("IDP_CARGO_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`SELECT set_config('app.tenant_id',$1,true)`,
		`UPDATE cargo_fin.creditor_invoice SET state='MATCHED',matched_by='x',matched_at=now(),match_entered_at=now(),approval_required=false,threshold_no=2,
            revision=revision+1 WHERE tenant_id=$1 AND invoice_id='CI-R'`,
		`INSERT INTO cargo_fin.creditor_invoice_match (tenant_id,invoice_id,match_no,line_no,target_kind,job_id,cost_side,target_line,target_version,amount_minor,
            final,local_minor,accrual_amount_minor,accrual_local_minor,prior_amount_minor,prior_local_minor,variance_minor,variance_local_minor)
            VALUES ($1,'CI-R',1,1,'JOB_COST','JOB-C','COST',1,1,5000,false,5000,10000,10000,0,0,0,0)`} {
		if _, err = tx.Exec(ctx, s, f.tenant); err != nil {
			t.Fatal(err)
		}
	}
	if state, constraint := sqlState(tx.Commit(ctx)); state != "23514" || constraint != "fin_read_committed" {
		t.Fatalf("a match at REPEATABLE READ: state %s constraint %s", state, constraint)
	}
}
