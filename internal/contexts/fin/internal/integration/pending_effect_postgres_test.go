//go:build integration

package integration

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
)

// A pool's pending items and what a preview would change (V2.94), through FIN's commands on PostgreSQL, in the world of
// formerWorld (see former_share_postgres_test.go): CNY 1,000.00 of freight apportioned 461.54, 307.69 and 230.77 to
// SH-A, SH-B and SH-C, then SH-C leaves (transferred by ops-9 after a rework ops-1 asked for).

func (u usedWorld) pending() []api.PoolPendingItem {
	u.t.Helper()
	items, err := u.module.GetConsolCostPoolPendingItems(u.ctx, api.GetConsolCostPoolPendingItems{TenantID: u.tenant, PoolID: u.pool})
	if err != nil {
		u.t.Fatal(err)
	}
	return items
}

func kindsOfPending(items []api.PoolPendingItem) (out []string) {
	for _, it := range items {
		out = append(out, it.Kind)
	}
	return out
}

func (u usedWorld) effect(no int) api.PreviewEffect {
	u.t.Helper()
	e, err := u.module.GetConsolCostPreviewEffect(u.ctx, api.GetConsolCostPreviewEffect{TenantID: u.tenant, PoolID: u.pool, PreviewNo: no})
	if err != nil {
		u.t.Fatal(err)
	}
	return e
}

// closePool matches the carrier's invoice final and closes the pool.
func (u usedWorld) closePool(hour int) {
	u.t.Helper()
	u.settleInvoice(hour)
	u.closeOnly(hour)
}

// settleInvoice records the carrier's invoice for line 1 and matches it final.
func (u usedWorld) settleInvoice(hour int) {
	u.t.Helper()
	u.must(u.module.RecordCreditorInvoice(u.ctx, api.RecordCreditorInvoice{CommandID: "invoice", TenantID: u.tenant, ActorID: "ap-1", InvoiceID: "CI-U",
		LegalEntityID: "LE", CreditorPartyID: "airline-mu", SupplierInvoiceNo: "MU-U-1", InvoiceDate: u.hours(hour).Format(time.DateOnly), Currency: "CNY",
		Lines: []api.CreditorInvoiceLine{{ChargeCode: "AIR_FREIGHT", Amount: 100_000}}, OccurredAt: u.hours(hour)}))
	u.must(u.module.MatchCreditorInvoice(u.ctx, u.match("CI-U", "ap-1", hour, onPool("P-U", 1, 1, 100_000, true))))
}

func (u usedWorld) closeOnly(hour int) {
	u.t.Helper()
	u.must(u.module.CloseConsolCostPool(u.ctx, api.CloseConsolCostPool{CommandID: "close", TenantID: u.tenant, ActorID: "fin-1", PoolID: u.pool,
		ExpectedRevision: u.revision(), OccurredAt: u.hours(hour)}))
}

// The pool's day: nothing is pending; in rework the container is listed for operations; once SH-C left its former share
// is listed for finance with the people who cannot confirm it; the preview's effect is read before anyone submits it
// (and says when the preview is stale); once it is submitted only the exception cost whose party is pending is left,
// the pool closes with it and the list says how it is named once the pool is reopened.
func TestAPoolsPendingItemsAndAPreviewsEffectOnPostgres(t *testing.T) {
	u := formerWorld(t)
	if got := u.pending(); len(got) != 1 || got[0].Kind != api.PendingCostNotFinal || got[0].LineNo != 1 || got[0].Actor != api.ActorFinance {
		t.Fatalf("the accrual is not matched: %+v", got)
	}
	u.settleInvoice(5)
	if got := u.pending(); got == nil || len(got) != 0 {
		t.Fatalf("a pool with nothing pending: %#v", got)
	}
	u.openRework("ops-1")
	got := u.pending()
	if len(got) != 1 || got[0].Kind != api.PendingInWork || got[0].UnitNo != 1 || got[0].Actor != api.ActorOperations || got[0].Amount.Minor != 0 ||
		!strings.Contains(got[0].Message, "load unit 1 (stuffing X-1) is in rework") {
		t.Fatalf("in rework: %+v", got)
	}
	u.reseal(4)
	if got = u.pending(); len(got) != 0 {
		t.Fatalf("sealed again: %+v", got)
	}

	u.depart()
	got = u.pending()
	if len(got) != 1 || got[0].Kind != api.PendingFormerSharePending || got[0].LineNo != 1 || got[0].ShipmentID != "SH-C" || got[0].Amount != cny(23_077) ||
		got[0].AmountLocal != cny(23_077) || got[0].Actor != api.ActorFinance {
		t.Fatalf("after SH-C left: %+v", got)
	}
	for _, part := range []string{"CNY 230.77", "attribute it", "voucher", "ops-1", "ops-9", "cannot submit it"} {
		if !strings.Contains(got[0].Message, part) {
			t.Fatalf("the item does not say %q: %q", part, got[0].Message)
		}
	}

	// the effect of a preview: a loss of 50.00 and an exception cost of 200.00, party pending
	r := u.must(u.module.PreviewConsolCostApportionment(u.ctx, u.attributeCmd("preview-1", "ops-2", 6, statementOf("C", 5_000, 20_000, api.PartyPending))))
	pools := len(u.ap.pool(u.pool).Apportionments)
	e := u.effect(int(r.Version))
	if after := len(u.ap.pool(u.pool).Apportionments); after != pools {
		t.Fatalf("the effect wrote an apportionment: %d to %d", pools, after)
	}
	wantHouses := []api.PreviewHouseEffect{
		{LineNo: 1, ShipmentID: "SH-A", JobID: "JOB-A", Live: true, NowKind: "SHARE", Now: cny(46_154), NowLocal: cny(46_154), PreviewKind: "SHARE", Preview: cny(45_000),
			PreviewLocal: cny(45_000), Difference: cny(-1_154), DifferenceLocal: cny(-1_154), ProfitChange: cny(1_154)},
		{LineNo: 1, ShipmentID: "SH-B", JobID: "JOB-B", Live: true, NowKind: "SHARE", Now: cny(30_769), NowLocal: cny(30_769), PreviewKind: "SHARE", Preview: cny(30_000),
			PreviewLocal: cny(30_000), Difference: cny(-769), DifferenceLocal: cny(-769), ProfitChange: cny(769)},
		{LineNo: 1, ShipmentID: "SH-C", JobID: "JOB-C", Former: true, NowKind: "FORMER_SHARE_PENDING", Now: cny(23_077), NowLocal: cny(23_077), PreviewKind: "LOSS",
			Preview: cny(5_000), PreviewLocal: cny(5_000), Difference: cny(-18_077), DifferenceLocal: cny(-18_077), ProfitChange: cny(-5_000)}}
	if !slices.Equal(e.Houses, wantHouses) {
		t.Fatalf("the houses:\n%+v\nwant\n%+v", e.Houses, wantHouses)
	}
	l := e.Lines[0]
	if len(e.Lines) != 1 || l.CostNow != cny(100_000) || l.Cost != cny(100_000) || l.CostChanged || l.Shares != cny(75_000) || l.Losses != cny(5_000) || l.Exceptions != cny(20_000) ||
		l.ToAllocate != cny(0) || !l.Conserved || l.Method != api.MethodChargeable {
		t.Fatalf("the line: %+v", l)
	}
	if len(e.Exceptions) != 1 || e.Exceptions[0].Party != api.PartyPending || e.Exceptions[0].Before != cny(0) || e.Exceptions[0].After != cny(20_000) {
		t.Fatalf("the exception costs: %+v", e.Exceptions)
	}
	if len(e.Reasons) != 1 || e.Reasons[0].End != "TRANSFERRED" || e.Reasons[0].EndReason != "WRONG_CONSOL" || e.Reasons[0].EndedBy != "ops-9" || e.Reasons[0].Voucher != "CLAIM-1" ||
		!e.Reasons[0].EndedAt.Equal(u.hours(5)) || e.Reasons[0].TransferredTo != "CONSOL-2" {
		t.Fatalf("the reasons: %+v", e.Reasons)
	}
	if !e.Current || e.SubmittedAs != 0 || e.PreviewedBy != "ops-2" || len(e.HouseTotals) != 3 || e.HouseTotals[2].ProfitChange != cny(-5_000) {
		t.Fatalf("the state: %+v", e)
	}
	// the basis moves: the preview is flagged stale, not refused, and current again when it moves back
	u.changeBasis(func(b *phyapi.PhysicalCostBasis) { b.Revision = 9 })
	if e = u.effect(int(r.Version)); e.Current || len(e.Changes) != 1 || !strings.Contains(e.Changes[0], "basis changed from [revision 8] to [revision 9]") {
		t.Fatalf("stale: %+v", e)
	}
	u.changeBasis(func(b *phyapi.PhysicalCostBasis) { b.Revision = 8 })
	if e = u.effect(int(r.Version)); !e.Current {
		t.Fatalf("current again: %+v", e)
	}
	// the effect needs no particular person: SH-C's departure is not the asker's business to read
	if _, err := u.module.GetConsolCostPreviewEffect(u.ctx, api.GetConsolCostPreviewEffect{TenantID: u.tenant, PoolID: u.pool, PreviewNo: 7}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("an unknown preview: %v", err)
	}
	if _, err := u.module.GetConsolCostPreviewEffect(u.ctx, api.GetConsolCostPreviewEffect{TenantID: u.tenant, PoolID: "NO_SUCH_POOL", PreviewNo: 1}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("an unknown pool: %v", err)
	}
	if _, err := u.module.GetConsolCostPoolPendingItems(u.ctx, api.GetConsolCostPoolPendingItems{TenantID: u.tenant, PoolID: "NO_SUCH_POOL"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("an unknown pool's list: %v", err)
	}

	// submitted by another finance user: the effect says so, and the list holds the exception cost only
	u.must(u.submit("submit-1", "fin-2", int(r.Version), 6))
	if e = u.effect(int(r.Version)); e.SubmittedAs != 2 || e.Current || len(e.Changes) != 0 {
		t.Fatalf("submitted: %+v", e)
	}
	got = u.pending()
	if len(got) != 1 || got[0].Kind != api.PendingExceptionCause || got[0].ShipmentID != "SH-C" || got[0].Amount != cny(20_000) || got[0].Actor != api.ActorFinance ||
		!strings.Contains(got[0].Message, "does not hold back the close") {
		t.Fatalf("after the submission: %+v", got)
	}
	u.closeOnly(8)
	got = u.pending()
	if len(got) != 1 || got[0].Kind != api.PendingExceptionCause || !strings.Contains(got[0].Message, "reopen the pool with a reason (ReopenConsolCostPool)") {
		t.Fatalf("closed: %+v", got)
	}
	// PHY does not answer: the list fails and is not empty-handed
	u.w.mu.Lock()
	basis := u.w.costBases["CONSOL-U"]
	delete(u.w.costBases, "CONSOL-U")
	u.w.mu.Unlock()
	if items, err := u.module.GetConsolCostPoolPendingItems(u.ctx, api.GetConsolCostPoolPendingItems{TenantID: u.tenant, PoolID: u.pool}); err == nil || items != nil {
		t.Fatalf("PHY does not answer: %+v %v", items, err)
	}
	if _, err := u.module.GetConsolCostPreviewEffect(u.ctx, api.GetConsolCostPreviewEffect{TenantID: u.tenant, PoolID: u.pool, PreviewNo: 1}); err == nil {
		t.Fatal("the effect with PHY silent")
	}
	u.w.mu.Lock()
	u.w.costBases["CONSOL-U"] = basis
	u.w.mu.Unlock()
}

// A pool closed before the house left holds a former share only a reopening can attribute; the lines the close would
// refuse are listed with the close's words; an amount to be allocated is listed once every house is gone.
func TestAClosedPoolAndTheLinesTheCloseRefusesAreListedOnPostgres(t *testing.T) {
	u := formerWorld(t)
	u.closePool(6)
	u.depart()
	got := u.pending()
	if len(got) != 1 || got[0].Kind != api.PendingReopenToAttribute || got[0].ShipmentID != "SH-C" || got[0].Amount != cny(23_077) || got[0].Actor != api.ActorFinance ||
		!strings.Contains(got[0].Message, "reopen the pool with a reason (ReopenConsolCostPool), then preview the line again") {
		t.Fatalf("closed before SH-C left: %+v", got)
	}
	u.must(u.module.ReopenConsolCostPool(u.ctx, api.ReopenConsolCostPool{CommandID: "reopen", TenantID: u.tenant, ActorID: "fin-1", PoolID: u.pool,
		ExpectedRevision: u.revision(), Reason: "SH-C left after the close", OccurredAt: u.hours(7)}))
	if got = u.pending(); len(got) != 1 || got[0].Kind != api.PendingFormerSharePending {
		t.Fatalf("reopened: %+v", got)
	}

	// every house gone: SH-A and SH-B left as well; the remainder after SH-C's loss is to be allocated
	v := formerWorld(t)
	v.settleInvoice(5)
	v.changeBasis(func(b *phyapi.PhysicalCostBasis) {
		b.Revision = 8
		b.Houses = nil
		left := func(shipment string) phyapi.FormerHouse {
			return phyapi.FormerHouse{ShipmentID: shipment, End: phyapi.FormerTransferred, TransferredTo: "CONSOL-2", Reason: "WRONG_CONSOL", EndedBy: "ops-9", EndedAt: v.hours(5),
				EndedEnteredAt: v.hours(5)}
		}
		b.FormerHouses = []phyapi.FormerHouse{left("SH-A"), left("SH-B"), left("SH-C")}
	})
	none := func(house string) api.AttributionStatement { return statementOf(house, 0, 0, "") }
	v.attribute("attribute-all", 6, none("A"), none("B"), statementOf("C", 5_000, 20_000, api.PartyCarrier))
	got = v.pending()
	if len(got) != 1 || got[0].Kind != api.PendingToBeAllocated || got[0].LineNo != 1 || got[0].Amount != cny(75_000) || got[0].ShipmentID != "" || got[0].Actor != api.ActorFinance ||
		!strings.Contains(got[0].Message, "add cargo") || !strings.Contains(got[0].Message, "cancel the consolidation and retain") {
		t.Fatalf("every house gone: %+v", got)
	}

	// the rest attributed to SH-C: the effect tells what was to be allocated in force and what the preview leaves
	rest := statementOf("C", 5_000, 95_000, api.PartyCarrier)
	rest.Reason = "the rest of the booking is the consolidation's"
	r := v.must(v.module.PreviewConsolCostApportionment(v.ctx, v.attributeCmd("preview-rest", "ops-2", 7, rest)))
	if l := v.effect(int(r.Version)).Lines[0]; l.ToAllocateNow != cny(75_000) || l.ToAllocate != cny(0) || !l.Conserved {
		t.Fatalf("the line: %+v", l)
	}

	// lines: a second cost not apportioned, and the first no longer on the loaded basis
	w := previewWorld(t)
	w.record("cost-2", "CFS_HANDLING", api.MethodPieces, 30_000)
	w.apportion("apportion-1", 4, []int{1}, "SH-C")
	w.must(w.module.RecordCreditorInvoice(w.ctx, api.RecordCreditorInvoice{CommandID: "invoice-w", TenantID: w.tenant, ActorID: "ap-1", InvoiceID: "CI-W",
		LegalEntityID: "LE", CreditorPartyID: "airline-mu", SupplierInvoiceNo: "MU-W-1", InvoiceDate: w.hours(5).Format(time.DateOnly), Currency: "CNY",
		Lines: []api.CreditorInvoiceLine{{ChargeCode: "AIR_FREIGHT", Amount: 100_000}, {ChargeCode: "CFS_HANDLING", Amount: 30_000}}, OccurredAt: w.hours(5)}))
	w.must(w.module.MatchCreditorInvoice(w.ctx, w.match("CI-W", "ap-1", 5, onPool("P-U", 1, 1, 100_000, true), onPool("P-U", 2, 2, 30_000, true))))
	got = w.pending()
	if len(got) != 1 || got[0].Kind != api.PendingLineNotApportioned || got[0].LineNo != 2 || got[0].Actor != api.ActorFinance ||
		!strings.Contains(got[0].Message, "cost line 2 is not apportioned") || !strings.Contains(got[0].Message, "preview the line again and submit it") {
		t.Fatalf("a line not apportioned: %+v", got)
	}
	w.changeBasis(func(b *phyapi.PhysicalCostBasis) {
		b.Houses[1] = ownHouse(w.fixture, "SH-B", 2, 100, 1_200_000, 1, 50, 600_000)
	})
	got = w.pending()
	if k := kindsOfPending(got); !slices.Equal(k, []string{api.PendingLineNotApportioned, api.PendingLineNotOnBasis}) || got[1].LineNo != 1 ||
		!strings.Contains(got[1].Message, "cost line 1 is not apportioned on the current loaded basis (house SH-B)") {
		t.Fatalf("a line not on the basis: %+v", got)
	}
	// in rework the lines wait
	w.openRework("ops-1")
	if k := kindsOfPending(w.pending()); !slices.Equal(k, []string{api.PendingInWork}) {
		t.Fatalf("in rework: %v", k)
	}
}

// A line by REVENUE is read against the houses' revenue as the close reads it, without its locks: apportioned on the
// revenue the houses have, nothing is pending; once a house's sell side is revised the line is not on the basis, and a
// preview made before the revision is flagged stale with the revenue named.
func TestALineByRevenueIsListedAndAPreviewByRevenueIsStaleOnPostgres(t *testing.T) {
	u, _ := revenueWorld(t)
	u.sellOn("JOB-A", "AIR_FREIGHT", party(api.ChargeSide{Amount: usd(120_000)}))
	u.sellOn("JOB-B", "AIR_FREIGHT", party(api.ChargeSide{Amount: cny(300_000)}))
	u.sellOn("JOB-C", "AIR_FREIGHT", party(api.ChargeSide{Amount: cny(50_000)}))
	u.record("cost-1", "AIR_FREIGHT", api.MethodRevenue, 100_001)
	u.apportion("apportion-1", 4, []int{1})
	u.must(u.module.RecordCreditorInvoice(u.ctx, api.RecordCreditorInvoice{CommandID: "invoice-rev", TenantID: u.tenant, ActorID: "ap-1", InvoiceID: "CI-REV",
		LegalEntityID: "LE", CreditorPartyID: "airline-mu", SupplierInvoiceNo: "MU-REV-1", InvoiceDate: u.hours(5).Format(time.DateOnly), Currency: "CNY",
		Lines: []api.CreditorInvoiceLine{{ChargeCode: "AIR_FREIGHT", Amount: 100_001}}, OccurredAt: u.hours(5)}))
	u.must(u.module.MatchCreditorInvoice(u.ctx, u.match("CI-REV", "ap-1", 5, onPool("P-U", 1, 1, 100_001, true))))
	if got := u.pending(); len(got) != 0 {
		t.Fatalf("apportioned on the revenue the houses have: %+v", got)
	}
	r := u.must(u.preview("preview-2", 5, []int{1}))
	if e := u.effect(int(r.Version)); !e.Current {
		t.Fatalf("the preview by revenue: %+v", e)
	}
	u.reviseSell("revise-c", "JOB-C", 1, 60_000, 6)
	got := u.pending()
	if len(got) != 1 || got[0].Kind != api.PendingLineNotOnBasis || got[0].LineNo != 1 || !strings.Contains(got[0].Message, "the houses' revenue under charge code AIR_FREIGHT has changed") {
		t.Fatalf("a sell side revised: %+v", got)
	}
	if e := u.effect(int(r.Version)); e.Current || len(e.Changes) != 1 || !strings.Contains(e.Changes[0], `revenue.1."SH-C"`) {
		t.Fatalf("the preview after the revision: %+v", e)
	}
}

// The effect reads the houses' open jobs as a submission does: a preview made while a house's job was open is stale
// once that job is cancelled.
func TestAPreviewIsStaleOnceAHousesJobIsCancelledOnPostgres(t *testing.T) {
	u := previewWorld(t)
	r := u.must(u.preview("preview-1", 4, nil, "SH-C"))
	if e := u.effect(int(r.Version)); !e.Current {
		t.Fatalf("before: %+v", e)
	}
	if err := u.cancelJob("cancel-c", "JOB-C"); err != nil {
		t.Fatal(err)
	}
	if e := u.effect(int(r.Version)); e.Current || len(e.Changes) != 1 || !strings.Contains(e.Changes[0], `house."SH-C"`) {
		t.Fatalf("after: %+v", e)
	}
}

// A shipment that has a job under another legal entity as well is read by the pool's legal entity's job: the preview
// stays current when the other entity's job is opened.
func TestTheEffectTakesThePoolsLegalEntitysJobOnPostgres(t *testing.T) {
	u := previewWorld(t)
	r := u.must(u.preview("preview-1", 4, nil, "SH-C"))
	u.must(u.module.OpenFinancePolicy(u.ctx, api.OpenFinancePolicy{CommandID: "policy-2", TenantID: u.tenant, ActorID: "acct-1", LegalEntityID: "LE2",
		FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: u.hours(0)}))
	driver := u.w.drivers["SH-C"]
	other := driver
	other.LegalEntityID = "LE2"
	u.w.drivers["SH-C"] = other
	u.must(u.module.OpenFreightJob(u.ctx, api.OpenFreightJob{CommandID: "open-c2", TenantID: u.tenant, ActorID: "fin-1", JobID: "JOB-C2", ShipmentID: "SH-C",
		OccurredAt: u.hours(3)}))
	u.w.drivers["SH-C"] = driver
	if e := u.effect(int(r.Version)); !e.Current || len(e.Changes) != 0 {
		t.Fatalf("with a job of another legal entity: %+v", e)
	}
}

// Through FIN's real commands the list is empty exactly when the close is accepted: the accrual not matched final, an
// invoice disputed, the dispute resolved; and the effect of a preview of the pool that is closed since says so.
func TestThePendingListIsEmptyExactlyWhenTheCloseIsAcceptedOnPostgres(t *testing.T) {
	u := previewWorld(t)
	u.apportion("apportion-1", 4, []int{1}, "SH-C")
	closeCmd := func(id string, hour int) api.CloseConsolCostPool {
		return api.CloseConsolCostPool{CommandID: id, TenantID: u.tenant, ActorID: "fin-1", PoolID: u.pool, ExpectedRevision: u.revision(), OccurredAt: u.hours(hour)}
	}
	notClosable := func(id string, hour int) {
		t.Helper()
		_, err := u.module.CloseConsolCostPool(u.ctx, closeCmd(id, hour))
		u.refusedWith("the close", err, "FIN_POOL_NOT_CLOSABLE")
	}
	// the accrual is not matched
	got := u.pending()
	if len(got) != 1 || got[0].Kind != api.PendingCostNotFinal || !strings.Contains(got[0].Message, "cost line 1 is ACCRUED") || !strings.Contains(got[0].Message, "match the creditor invoice as final") {
		t.Fatalf("not matched: %+v", got)
	}
	notClosable("close-1", 5)
	u.must(u.module.RecordCreditorInvoice(u.ctx, api.RecordCreditorInvoice{CommandID: "invoice", TenantID: u.tenant, ActorID: "ap-1", InvoiceID: "CI-U",
		LegalEntityID: "LE", CreditorPartyID: "airline-mu", SupplierInvoiceNo: "MU-U-1", InvoiceDate: u.hours(5).Format(time.DateOnly), Currency: "CNY",
		Lines: []api.CreditorInvoiceLine{{ChargeCode: "AIR_FREIGHT", Amount: 100_000}}, OccurredAt: u.hours(5)}))
	u.must(u.module.MatchCreditorInvoice(u.ctx, u.match("CI-U", "ap-1", 5, onPool("P-U", 1, 1, 100_000, true))))
	if got = u.pending(); len(got) != 0 {
		t.Fatalf("matched final: %+v", got)
	}
	// the invoice is disputed
	u.must(u.module.DisputeCreditorInvoice(u.ctx, api.DisputeCreditorInvoice{CommandID: "dispute", TenantID: u.tenant, ActorID: "ap-1", InvoiceID: "CI-U",
		ExpectedRevision: u.invoice("CI-U").Revision, Reason: "MU billed the wrong weight", OccurredAt: u.hours(6)}))
	got = u.pending()
	if len(got) != 1 || got[0].Kind != api.PendingInvoiceHeld || got[0].LineNo != 1 || got[0].Actor != api.ActorFinance ||
		!strings.Contains(got[0].Message, "disputed or awaits a variance approval") || !strings.Contains(got[0].Message, "settle the dispute or approve the variance") {
		t.Fatalf("disputed: %+v", got)
	}
	notClosable("close-2", 7)
	u.must(u.module.ResolveCreditorInvoiceDispute(u.ctx, api.ResolveCreditorInvoiceDispute{CommandID: "resolve", TenantID: u.tenant, ActorID: "ap-1", InvoiceID: "CI-U",
		ExpectedRevision: u.invoice("CI-U").Revision, Reason: "the weight was right", OccurredAt: u.hours(7)}))
	if got = u.pending(); len(got) != 0 {
		t.Fatalf("resolved: %+v", got)
	}
	// a preview made while the pool was open: once the pool is closed its effect says so and names the way out
	r := u.must(u.preview("preview-2", 8, []int{1}, "SH-C"))
	if e := u.effect(int(r.Version)); !e.Current || e.PoolClosed || e.WayOut != "" {
		t.Fatalf("the open pool: %+v", e)
	}
	u.must(u.module.CloseConsolCostPool(u.ctx, closeCmd("close-3", 9)))
	if got = u.pending(); len(got) != 0 {
		t.Fatalf("closed: %+v", got)
	}
	e := u.effect(int(r.Version))
	if e.Current || !e.PoolClosed || !strings.Contains(e.WayOut, "reopen it with a reason (ReopenConsolCostPool)") || e.SubmittedAs != 0 {
		t.Fatalf("the closed pool: %+v", e)
	}
}

// A line in a foreign currency: a former share's local amount in the list is the functional-currency amount and its
// message writes the line's currency, with thousands separated.
func TestAForeignCurrencyFormerShareIsListedInBothCurrenciesOnPostgres(t *testing.T) {
	u, _ := revenueWorld(t)
	u.must(u.module.RecordConsolCost(u.ctx, api.RecordConsolCost{CommandID: "cost-usd", TenantID: u.tenant, ActorID: "fin-1", PoolID: u.pool, ExpectedRevision: u.revision(),
		ChargeCode: "AIR_FREIGHT", Method: api.MethodChargeable, Cost: api.ChargeSide{PartyID: "airline-mu", Amount: usd(120_000)}, Reason: "entered for the consolidation",
		OccurredAt: u.hours(3)}))
	u.apportionAll("apportion-1", 4, []int{1})
	u.depart()
	var it api.PoolPendingItem
	for _, x := range u.pending() {
		if x.Kind == api.PendingFormerSharePending {
			it = x
		}
	}
	if it.Kind == "" {
		t.Fatalf("the list: %+v", u.pending())
	}
	if it.Amount.Currency != "USD" || it.AmountLocal.Currency != "CNY" || it.Amount.Minor <= 0 || it.AmountLocal.Minor <= it.Amount.Minor {
		t.Fatalf("the amounts: %+v", it)
	}
	if want := fmt.Sprintf("a former share of USD %s", usdText(it.Amount.Minor)); !strings.Contains(it.Message, want) {
		t.Fatalf("the message %q does not say %q", it.Message, want)
	}
}

func usdText(minor int64) string {
	whole := fmt.Sprint(minor / 100)
	for i := len(whole) - 3; i > 0; i -= 3 {
		whole = whole[:i] + "," + whole[i:]
	}
	return fmt.Sprintf("%s.%02d", whole, minor%100)
}
