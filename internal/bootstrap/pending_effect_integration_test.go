//go:build integration

package bootstrap

import (
	"slices"
	"strings"
	"testing"
	"time"

	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// A pool's pending items and what a preview would change (V2.94), on the owner's box A with the real ORG, RATE, COM,
// FUL, PHY and FIN: C-1 with H01, H02 and H03 of 10, 15 and 5 W/M, freight CNY 3,000.00 apportioned 1,000.00, 1,500.00
// and 500.00; the rework asked by ops-1 unloads H03, the container is sealed again and ops-9 transfers H03 to C-2.

func (d *oceanDay) pending() []finapi.PoolPendingItem {
	d.t.Helper()
	items, err := d.fin.GetConsolCostPoolPendingItems(d.ctx, finapi.GetConsolCostPoolPendingItems{TenantID: d.tenant, PoolID: "P-1"})
	d.must("the pending items of P-1", err)
	return items
}

func (d *oceanDay) effect(preview int) finapi.PreviewEffect {
	d.t.Helper()
	e, err := d.fin.GetConsolCostPreviewEffect(d.ctx, finapi.GetConsolCostPreviewEffect{TenantID: d.tenant, PoolID: "P-1", PreviewNo: preview})
	d.must("the effect of preview", err)
	return e
}

func pendingKinds(items []finapi.PoolPendingItem) (out []string) {
	for _, it := range items {
		out = append(out, it.Kind)
	}
	return out
}

// transferH03Only is the transfer of transferH03 without C-2's pool: H03 is unloaded and sealed again before it.
func (d *oceanDay) transferH03Only() {
	d.t.Helper()
	unlocode := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	departs := d.at.Add(72 * time.Hour)
	_, err := d.phy.CreateConsolidation(d.ctx, phyapi.CreateConsolidation{CommandID: "consol-2", TenantID: d.tenant, ActorID: "ops-1", ConsolidationID: "C-2", Mode: phyapi.ModeSea,
		Kind: phyapi.KindSeaLCLOwn, OperatingLegalEntityID: "LE", Origin: unlocode("CNSHA"), Destination: unlocode("DEHAM"), PlannedDeparture: departs.Add(6 * time.Hour),
		HouseCutOff: departs.Add(-6 * time.Hour), OccurredAt: d.hours(8)})
	d.must("create C-2", err)
	_, err = d.phy.PlanLoadUnit(d.ctx, phyapi.PlanLoadUnit{CommandID: "plan-c2", TenantID: d.tenant, ActorID: "ops-1", ConsolidationID: "C-2", ExpectedRevision: 1,
		Kind: phyapi.LoadUnitContainer, TypeCode: "22G1", OccurredAt: d.hours(8)})
	d.must("plan the unit of C-2", err)
	_, err = d.phy.TransferHouse(d.ctx, phyapi.TransferHouse{CommandID: "transfer-h03", TenantID: d.tenant, ActorID: "ops-9", ConsolidationID: "C-1", ToConsolidationID: "C-2",
		ShipmentID: "SH-H03", ExpectedMembershipRevision: 1, Reason: phyapi.ReasonWrongConsol, OccurredAt: d.hours(26)})
	d.must("transfer H03", err)
}

// The owner's day, read through the list and the effect: during the rework IN_WORK; sealed again with H03 still
// attached and nothing loaded, the line is not on the loaded basis; after the transfer FORMER_SHARE_PENDING of 500.00
// naming ops-1 and ops-9; the effect of a preview with a loss of 100.00 and an exception cost of 200.00 pending reads
// H01 1,000.00 to 1,080.00 (profit -80.00), H02 1,500.00 to 1,620.00 (-120.00), H03 500.00 pending to a loss of 100.00
// (-100.00) and the line 3,000.00 on both sides; after the submission only the exception cost whose party is pending is
// listed, and the pool closes with it.
func TestAPoolsPendingItemsAndAPreviewsEffectWithEveryContext(t *testing.T) {
	d := boxA(t, "cargo-box-a-pending-")
	first, err := d.previewBy("fin-1", "fin-pre-1")
	d.must("the first preview", err)
	_, err = d.submitBy("fin-1", "fin-sub-1", first.Version)
	d.must("the first submission", err)
	if got := d.pending(); len(got) != 1 || got[0].Kind != finapi.PendingCostNotFinal {
		t.Fatalf("the freight is not matched: %+v", got)
	}
	d.settleFreight()
	if got := d.pending(); got == nil || len(got) != 0 {
		t.Fatalf("before the rework: %#v", got)
	}

	d.reworkBy("rework-1", "ops-1", 22)
	got := d.pending()
	if len(got) != 1 || got[0].Kind != finapi.PendingInWork || got[0].UnitNo != 1 || got[0].Actor != finapi.ActorOperations || !strings.Contains(got[0].Message, "load unit 1 (stuffing X-1) is in rework") {
		t.Fatalf("during the rework: %+v", got)
	}
	d.unloadForRework("H03", 23)
	if k := pendingKinds(d.pending()); !slices.Equal(k, []string{finapi.PendingInWork}) {
		t.Fatalf("H03 unloaded, the container in rework: %v", k)
	}
	d.resealAt("reseal-1", 24)
	got = d.pending()
	if len(got) != 1 || got[0].Kind != finapi.PendingLineNotOnBasis || got[0].LineNo != 1 || !strings.Contains(got[0].Message, "cost line 1 is not apportioned on the current loaded basis") {
		t.Fatalf("sealed again, H03 attached with nothing loaded: %+v", got)
	}

	d.transferH03Only()
	got = d.pending()
	if len(got) != 1 || got[0].Kind != finapi.PendingFormerSharePending || got[0].LineNo != 1 || got[0].ShipmentID != "SH-H03" || got[0].Amount.Minor != 50_000 ||
		got[0].AmountLocal.Minor != 50_000 || got[0].Actor != finapi.ActorFinance {
		t.Fatalf("after the transfer: %+v", got)
	}
	for _, part := range []string{"CNY 500.00", "ops-1", "ops-9", "voucher", "cannot submit it"} {
		if !strings.Contains(got[0].Message, part) {
			t.Fatalf("the item does not say %q: %q", part, got[0].Message)
		}
	}

	pre, err := d.previewWith("ops-2", "fin-pre-attr", statement("H03", 10_000, 20_000, finapi.PartyPending))
	d.must("the preview with the attribution", err)
	e := d.effect(pre.Version)
	type row struct {
		house, nowKind, kind string
		now, will, profit    int64
	}
	var rows []row
	for _, h := range e.Houses {
		rows = append(rows, row{h.ShipmentID, h.NowKind, h.PreviewKind, h.Now.Minor, h.Preview.Minor, h.ProfitChange.Minor})
	}
	want := []row{{"SH-H01", "SHARE", "SHARE", 100_000, 108_000, -8_000}, {"SH-H02", "SHARE", "SHARE", 150_000, 162_000, -12_000},
		{"SH-H03", "FORMER_SHARE_PENDING", "LOSS", 50_000, 10_000, -10_000}}
	if !slices.Equal(rows, want) {
		t.Fatalf("the houses: %+v, want %+v", rows, want)
	}
	if len(e.Lines) != 1 || e.Lines[0].CostNow.Minor != 300_000 || e.Lines[0].Cost.Minor != 300_000 || !e.Lines[0].Conserved || e.Lines[0].Shares.Minor != 270_000 ||
		e.Lines[0].Losses.Minor != 10_000 || e.Lines[0].Exceptions.Minor != 20_000 || e.Lines[0].ToAllocate.Minor != 0 {
		t.Fatalf("the line: %+v", e.Lines)
	}
	if len(e.Exceptions) != 1 || e.Exceptions[0].Party != finapi.PartyPending || e.Exceptions[0].Before.Minor != 0 || e.Exceptions[0].After.Minor != 20_000 {
		t.Fatalf("the exception costs: %+v", e.Exceptions)
	}
	if len(e.Reasons) != 1 || e.Reasons[0].End != phyapi.FormerTransferred || e.Reasons[0].EndedBy != "ops-9" || e.Reasons[0].Voucher != "CLAIM-1" || e.Reasons[0].TransferredTo != "C-2" {
		t.Fatalf("the reasons: %+v", e.Reasons)
	}
	if !e.Current || e.SubmittedAs != 0 || e.InWork {
		t.Fatalf("the state: %+v", e)
	}

	_, err = d.submitBy("fin-1", "fin-sub-fin-1", pre.Version)
	d.must("the submission", err)
	if e = d.effect(pre.Version); e.SubmittedAs != 2 || e.Current {
		t.Fatalf("submitted: %+v", e)
	}
	got = d.pending()
	if len(got) != 1 || got[0].Kind != finapi.PendingExceptionCause || got[0].LineNo != 1 || got[0].ShipmentID != "SH-H03" || got[0].Amount.Minor != 20_000 ||
		got[0].Actor != finapi.ActorFinance || !strings.Contains(got[0].Message, "does not hold back the close") {
		t.Fatalf("after the submission: %+v", got)
	}
	d.must("the close with the exception cost pending", d.closeP1("fin-close"))
	if p := d.pool(); p.State != finapi.PoolClosed {
		t.Fatalf("the pool: %+v", p.State)
	}
	got = d.pending()
	if k := pendingKinds(got); !slices.Equal(k, []string{finapi.PendingExceptionCause}) || !strings.Contains(got[0].Message, "ReopenConsolCostPool") {
		t.Fatalf("closed: %+v", got)
	}
}

// A pool closed before the house left shows REOPEN_TO_ATTRIBUTE, for finance, and nothing reopens it.
func TestAPoolClosedBeforeTheHouseLeftIsToBeReopenedWithEveryContext(t *testing.T) {
	d := boxA(t, "cargo-box-a-reopen-")
	first, err := d.previewBy("fin-1", "fin-pre-1")
	d.must("the first preview", err)
	_, err = d.submitBy("fin-1", "fin-sub-1", first.Version)
	d.must("the first submission", err)
	d.settleFreight()
	d.must("the close", d.closeP1("fin-close-early"))
	if got := d.pending(); len(got) != 0 {
		t.Fatalf("closed, nobody has left: %+v", got)
	}
	d.reworkBy("rework-1", "ops-1", 22)
	d.unloadForRework("H03", 23)
	d.resealAt("reseal-1", 24)
	d.transferH03Only()
	got := d.pending()
	if len(got) != 1 || got[0].Kind != finapi.PendingReopenToAttribute || got[0].LineNo != 1 || got[0].ShipmentID != "SH-H03" || got[0].Amount.Minor != 50_000 ||
		got[0].Actor != finapi.ActorFinance || !strings.Contains(got[0].Message, "reopen the pool with a reason (ReopenConsolCostPool), then preview the line again") ||
		!strings.Contains(got[0].Message, "ops-1") || !strings.Contains(got[0].Message, "ops-9") {
		t.Fatalf("closed before H03 left: %+v", got)
	}
	if p := d.pool(); p.State != finapi.PoolClosed {
		t.Fatalf("the list reopened the pool: %+v", p.State)
	}
}
