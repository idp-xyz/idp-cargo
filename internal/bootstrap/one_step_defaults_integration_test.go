//go:build integration

package bootstrap

import (
	"strings"
	"testing"

	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FIN's rules read F4, F8 and F9 (V2.98), on the owner's box A with the real ORG, RATE, COM, FUL, PHY and FIN: C-1 with
// H01, H02 and H03 of 10, 15 and 5 W/M, freight CNY 3,000.00 apportioned 1,000.00, 1,500.00 and 500.00; H03 is unloaded
// in a rework asked by ops-1, the container sealed again and H03 transferred to C-2 by ops-9. H03's former share is CNY
// 500.00. The settings of the legal entity LE are changed through FIN's own commands (cfg-1 proposes, cfg-2 approves what
// loosens).

// boxAOpenAfterTheTransfer is box A on the day after, as boxAAfterTheTransfer has it, except that the freight invoice is
// not matched: the line's cost is still an accrual, which a revision of the line changes.
func boxAOpenAfterTheTransfer(t *testing.T, prefix string, entries ...finapi.FinSettingEntry) (*oceanDay, int) {
	t.Helper()
	d := boxA(t, prefix)
	first, err := d.previewBy("fin-1", "fin-pre-1")
	d.must("the first preview", err)
	_, err = d.submitBy("fin-1", "fin-sub-1", first.Version)
	d.must("the first submission", err)
	d.transferH03()
	version := 0
	if len(entries) > 0 {
		version = d.configure(entries...)
	}
	return d, version
}

// reviseFreight revises the freight, line 1 of P-1, to the amount.
func (d *oceanDay) reviseFreight(id string, version int, amount int64) {
	d.t.Helper()
	_, err := d.fin.ReviseConsolCost(d.ctx, finapi.ReviseConsolCost{CommandID: id, TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-1", ExpectedRevision: d.pool().Revision,
		LineNo: 1, ExpectedVersion: version, Value: finapi.ChargeSide{Amount: kernel.Money{Currency: "CNY", Minor: amount}}, Reason: "the line re-invoiced the freight",
		OccurredAt: d.later()})
	d.must("revise the freight to "+id, err)
}

func (d *oceanDay) oneStep(who, id string, lines ...int) (finapi.Receipt, error) {
	d.t.Helper()
	return d.fin.ApportionConsolCostInOneStep(d.ctx, finapi.ApportionConsolCostInOneStep{CommandID: id, TenantID: d.tenant, ActorID: who, PoolID: "P-1",
		ExpectedRevision: d.pool().Revision, LineNos: lines, OccurredAt: d.later()})
}

func (d *oceanDay) lastLine(no int) finapi.ApportionedLine {
	d.t.Helper()
	a := d.pool().Apportionments
	for _, la := range a[len(a)-1].Lines {
		if la.LineNo == no {
			return la
		}
	}
	d.t.Fatalf("line %d is not in the last apportionment: %+v", no, a[len(a)-1])
	return finapi.ApportionedLine{}
}

// conservedBox checks that the parts of a line add up to its cost in both currencies.
func conservedBox(t *testing.T, what string, la finapi.ApportionedLine) {
	t.Helper()
	amount, local := la.ToAllocate.Minor, la.ToAllocateLocal.Minor
	for _, s := range la.Shares {
		if !s.NotUsed {
			amount, local = amount+s.Amount.Minor, local+s.Local.Minor
		}
	}
	for _, x := range la.Attributions {
		amount, local = amount+x.Loss.Minor+x.Exception.Minor, local+x.LossLocal.Minor+x.ExceptionLocal.Minor
	}
	if amount != la.Amount.Minor || local != la.Local.Minor {
		t.Fatalf("%s: the parts add up to %d (%d local), the line is %d (%d local): %+v", what, amount, local, la.Amount.Minor, la.Local.Minor, la)
	}
}

// Under F8 DEFAULT_TO_REMAINING (with F3 ONLY_WHEN_AMOUNT_MOVES, which it needs) fin-2 previews and submits line 1 without
// stating H03: H03 is attributed nothing by default, its CNY 500.00 goes to the cargo that remains, and H01 and H02 bear
// 1,200.00 and 1,800.00. It is a confirmation like any other: ops-1, who asked for the rework, submitting the same preview
// is refused under F1 NOT_ALLOWED. Without the setting the preview is refused as before.
func TestADefaultAttributionGoesToTheCargoThatRemainsWithEveryContext(t *testing.T) {
	d, v := boxAAfterTheTransfer(t, "cargo-box-a-f8-", leSetting("fin.F3", "ONLY_WHEN_AMOUNT_MOVES"), leSetting("fin.F8", "DEFAULT_TO_REMAINING"))
	if v == 0 {
		t.Fatal("the settings are not in force")
	}
	pre, err := d.previewBy("fin-2", "fin-pre-default")
	d.must("fin-2 previews without stating H03", err)
	e := d.effect(pre.Version)
	reasons := 0
	for _, r := range e.Reasons {
		if r.ShipmentID == "SH-H03" && r.ByDefault && strings.Contains(r.Says, "F8 DEFAULT_TO_REMAINING") && strings.Contains(r.Says, "CNY 500.00") {
			reasons++
		}
	}
	if reasons != 1 || !e.Current {
		t.Fatalf("the effect says what the default does: %+v", e)
	}
	_, err = d.submitBy("ops-1", "fin-sub-ops-1", pre.Version)
	said(t, "ops-1 under F1 NOT_ALLOWED", err, "FIN_ATTRIBUTION_CONFIRMER", "ops-1 asked for rework 1", "house SH-H03 on line 1", "another finance user submits")
	if len(d.pool().Apportionments) != 1 {
		t.Fatalf("a refused submission was written: %+v", d.pool().Apportionments)
	}
	if _, err = d.submitBy("fin-2", "fin-sub-fin-2", pre.Version); err != nil {
		t.Fatalf("fin-2: %v", err)
	}
	p := d.pool()
	a := p.Apportionments[1]
	la := d.lastLine(1)
	x := la.Attributions[0]
	if a.By != "fin-2" || a.SettingsVersion != v || len(la.Attributions) != 1 || x.ShipmentID != "SH-H03" || !x.ByDefault || x.Carried || x.Loss.Minor != 0 || x.Exception.Minor != 0 ||
		x.Voucher != "" || x.Party != "" || !sameFigures(d.sharesOf(a), 120_000, 180_000) {
		t.Fatalf("the default: %+v", a)
	}
	conservedBox(t, "the default", la)
	if d.jobBears("H01") != 120_000 || d.jobBears("H02") != 180_000 {
		t.Fatalf("H01 and H02 bear %d and %d", d.jobBears("H01"), d.jobBears("H02"))
	}
	if h := d.jobProfit("H03"); h.FormerSharesPending.Minor != 0 || len(h.FormerShares) != 0 {
		t.Fatalf("H03 holds nothing pending: %+v", h)
	}
	// without the settings the same preview is refused, as before
	e2, _ := boxAAfterTheTransfer(t, "cargo-box-a-f8-off-")
	_, err = e2.previewBy("fin-2", "fin-pre-default")
	said(t, "REJECT_WITH_DEFAULT", err, "FIN_ATTRIBUTION_MISSING", "house SH-H03", "50000 CNY")
}

// Under F9 the line's cost changes after H03's attribution of loss CNY 200.00 and exception cost CNY 300.00 (the
// warehouse's) and the freight is revised to CNY 3,600.00. KEEP_AMOUNT keeps 200.00 and 300.00 (H01 and H02 bear 1,240.00
// and 1,860.00); KEEP_RATIO gives 240.00 and 360.00 (1,200.00 and 1,800.00); RECONFIRM refuses a preview that does not
// state H03 again, naming the house, the line and the amounts, and accepts the equal restatement, recorded as stated.
func TestAConfirmedAttributionWhenTheCostChangesWithEveryContext(t *testing.T) {
	for _, c := range []struct {
		f9               string
		loss, exception  int64
		carried, byRatio bool
		h01, h02         int64
	}{{"KEEP_AMOUNT", 20_000, 30_000, true, false, 124_000, 186_000}, {"KEEP_RATIO", 24_000, 36_000, true, true, 120_000, 180_000}, {"RECONFIRM", 20_000, 30_000, false, false, 124_000, 186_000}} {
		d, v := boxAOpenAfterTheTransfer(t, "cargo-box-a-f9-"+strings.ToLower(strings.ReplaceAll(c.f9, "_", "-"))+"-", leSetting("fin.F9", c.f9))
		if v == 0 {
			t.Fatal("F9 is not in force")
		}
		warehouse := statement("H03", 20_000, 30_000, finapi.PartyWarehouse)
		pre, err := d.previewWith("ops-2", "fin-pre-attr", warehouse)
		d.must(c.f9+": the attribution is previewed", err)
		_, err = d.submitBy("fin-1", "fin-sub-attr", pre.Version)
		d.must(c.f9+": the attribution is submitted", err)
		d.reviseFreight("fin-revise", 1, 360_000)
		if c.f9 == "RECONFIRM" {
			_, err = d.previewBy("fin-2", "fin-pre-again")
			said(t, "RECONFIRM", err, "FIN_ATTRIBUTION_RECONFIRM", "house SH-H03", "line 1", "CNY 3,000.00", "CNY 3,600.00", "state the attribution of house SH-H03 on line 1 again")
			if len(d.pool().Apportionments) != 2 {
				t.Fatalf("a refused preview: %+v", d.pool().Apportionments)
			}
			pre, err = d.previewWith("ops-2", "fin-pre-restate", warehouse)
			d.must("the equal restatement is previewed", err)
			_, err = d.submitBy("ops-1", "fin-sub-ops-1", pre.Version)
			said(t, "the asker restates", err, "FIN_ATTRIBUTION_CONFIRMER", "ops-1 asked for rework 1")
		} else {
			pre, err = d.previewBy("fin-2", "fin-pre-again")
			d.must(c.f9+": the line is previewed again with nothing stated", err)
		}
		if _, err = d.submitBy("fin-1", "fin-sub-again", pre.Version); err != nil {
			t.Fatalf("%s: %v", c.f9, err)
		}
		a := d.pool().Apportionments[2]
		la := d.lastLine(1)
		x := la.Attributions[0]
		if a.SettingsVersion != v || la.Amount.Minor != 360_000 || x.Carried != c.carried || x.ByRatio != c.byRatio || x.ByDefault || x.Loss.Minor != c.loss || x.Exception.Minor != c.exception ||
			x.Party != finapi.PartyWarehouse || x.Voucher != "CLAIM-1" || !sameFigures(d.sharesOf(a), c.h01, c.h02) {
			t.Fatalf("%s: %+v", c.f9, a)
		}
		conservedBox(t, c.f9, la)
		if d.jobBears("H01") != c.h01 || d.jobBears("H02") != c.h02 {
			t.Fatalf("%s: H01 and H02 bear %d and %d", c.f9, d.jobBears("H01"), d.jobBears("H02"))
		}
	}
}

// Under F4 a second line is apportioned in one step once H03 is attributed, and refused before that, naming the former
// share and the way out: the one-step command is a preview and a submission in one transaction, with no preview stored.
func TestASecondLineIsApportionedInOneStepOnceTheFormerShareIsAttributedWithEveryContext(t *testing.T) {
	d, v := boxAOpenAfterTheTransfer(t, "cargo-box-a-f4-", leSetting("fin.F4", "ONE_STEP_WITHOUT_OPENED_OR_LEFT"))
	if v == 0 {
		t.Fatal("F4 is not in force")
	}
	_, err := d.fin.RecordConsolCost(d.ctx, finapi.RecordConsolCost{CommandID: "fin-handling", TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-1", ExpectedRevision: d.pool().Revision,
		ChargeCode: "HANDLING", Method: finapi.MethodChargeable, Cost: finapi.ChargeSide{PartyID: "line-1", Amount: kernel.Money{Currency: "CNY", Minor: 100_000}},
		Reason: "the handling at the origin", OccurredAt: d.later()})
	d.must("the second cost line", err)
	previews := len(d.pool().Previews)
	_, err = d.oneStep("fin-2", "fin-one-step-1", 2)
	said(t, "before H03 is attributed", err, "FIN_ONE_STEP_REFUSED", "former share pending", "house SH-H03 on line 1", "preview the lines and submit the preview", "stating who bears it")
	if p := d.pool(); len(p.Apportionments) != 1 || len(p.Previews) != previews {
		t.Fatalf("a refused command wrote: %+v", p)
	}
	pre, err := d.previewWith("ops-2", "fin-pre-attr", statement("H03", 20_000, 30_000, finapi.PartyWarehouse))
	d.must("the attribution is previewed", err)
	_, err = d.submitBy("fin-1", "fin-sub-attr", pre.Version)
	d.must("the attribution is submitted", err)
	previews = len(d.pool().Previews)
	r, err := d.oneStep("fin-2", "fin-one-step-2", 2)
	if err != nil {
		t.Fatalf("once H03 is attributed: %v", err)
	}
	p := d.pool()
	a := p.Apportionments[len(p.Apportionments)-1]
	la := d.lastLine(2)
	if int(r.Version) != a.ApportionmentNo || a.PreviewNo != 0 || a.By != "fin-2" || a.SettingsVersion != v || len(p.Previews) != previews || len(a.Lines) != 1 || la.LineNo != 2 ||
		la.Amount.Minor != 100_000 || !sameFigures(d.sharesOf(a)[:2], 40_000, 60_000) {
		t.Fatalf("the one step: %+v", a)
	}
	conservedBox(t, "the second line", la)
	// without F4 the same command is refused naming the way out
	e, _ := boxAOpenAfterTheTransfer(t, "cargo-box-a-f4-off-")
	_, err = e.oneStep("fin-2", "fin-one-step", 1)
	said(t, "ALL_PREVIEWED", err, "FIN_ONE_STEP_REFUSED", "F4", "preview the lines and submit the preview", "ask a configurer")
}
