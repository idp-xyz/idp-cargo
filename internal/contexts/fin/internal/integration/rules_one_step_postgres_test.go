//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	finmodule "go.idp.xyz/idp-cargo/internal/contexts/fin"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
)

// FIN V2.98: FIN's rules read F4, F8 and F9, through FIN's commands on PostgreSQL, with the settings changed through
// FIN's own settings commands (a change set that loosens is proposed by ann and approved by bob). The worlds are those
// of V2.97: previewWorld (CNY 1,000.00 of freight over SH-A and SH-B, SH-C left out) and formerWorld (the freight
// apportioned 461.54, 307.69 and 230.77, then SH-C is transferred away: its former share is CNY 230.77).

const (
	f4One   = "ONE_STEP_WITHOUT_OPENED_OR_LEFT"
	f8Dflt  = "DEFAULT_TO_REMAINING"
	f3Moves = "ONLY_WHEN_AMOUNT_MOVES"
)

func (u usedWorld) oneStepCmd(id, actor string, hour int, lines []int, notUsed ...string) api.ApportionConsolCostInOneStep {
	return api.ApportionConsolCostInOneStep{CommandID: id, TenantID: u.tenant, ActorID: actor, PoolID: u.pool, ExpectedRevision: u.revision(), LineNos: lines, NotUsed: notUsed,
		OccurredAt: u.hours(hour)}
}

func (u usedWorld) oneStep(id, actor string, hour int, lines []int, notUsed ...string) (api.Receipt, error) {
	return u.module.ApportionConsolCostInOneStep(u.ctx, u.oneStepCmd(id, actor, hour, lines, notUsed...))
}

// reviseLine revises the cost of line 1 to amount, after version.
func (u usedWorld) reviseLine(id string, version int, amount int64, hour int) {
	u.t.Helper()
	u.must(u.module.ReviseConsolCost(u.ctx, api.ReviseConsolCost{CommandID: id, TenantID: u.tenant, ActorID: "fin-1", PoolID: u.pool, ExpectedRevision: u.revision(), LineNo: 1,
		ExpectedVersion: version, Value: api.ChargeSide{Amount: cny(amount)}, Reason: "the carrier re-invoiced", OccurredAt: u.hours(hour)}))
}

func lineOf(a api.PoolApportionment, no int) api.ApportionedLine {
	for _, la := range a.Lines {
		if la.LineNo == no {
			return la
		}
	}
	return api.ApportionedLine{}
}

// conservedLine checks that the parts of a line of the pool's view add up to its cost, in the line's currency and in the
// functional currency.
func conservedLine(t *testing.T, what string, la api.ApportionedLine) {
	t.Helper()
	amount, local := la.ToAllocate.Minor, la.ToAllocateLocal.Minor
	for _, s := range la.Shares {
		if !s.NotUsed {
			amount, local = amount+s.Amount.Minor, local+s.Local.Minor
		}
	}
	for _, x := range la.Attributions {
		amount += x.Loss.Minor + x.Exception.Minor
		local += x.LossLocal.Minor + x.ExceptionLocal.Minor
	}
	if amount != la.Amount.Minor || local != la.Local.Minor {
		t.Fatalf("%s: the parts add up to %d (%d local), the line is %d (%d local): %+v", what, amount, local, la.Amount.Minor, la.Local.Minor, la)
	}
}

func (u usedWorld) shareOf(a api.PoolApportionment, house string) int64 {
	for _, s := range lineOf(a, 1).Shares {
		if s.ShipmentID == house {
			return s.Amount.Minor
		}
	}
	u.t.Fatalf("no share of %s in %+v", house, a)
	return 0
}

// F4: the one-step command end to end. Refused while F4 is ALL_PREVIEWED, naming the way out; once a configurer sets
// F4 (proposed by ann, approved by bob) it writes the apportionment of a preview and a submission in one transaction,
// with no preview, the settings version in force and its submitter; one receipt, one event with no preview in it; the
// command replays; the figures are the preview's; and the pool closes on it.
func TestOneStepApportionmentEndToEndOnPostgres(t *testing.T) {
	u := previewWorld(t)
	before := u.revision()
	_, err := u.oneStep("one-step-0", "fin-1", 4, nil, "SH-C")
	u.refusedWith("F4 ALL_PREVIEWED", err, "FIN_ONE_STEP_REFUSED")
	for _, part := range []string{"F4", "preview the lines and submit the preview", "ask a configurer"} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("the refusal does not say %q: %v", part, err)
		}
	}
	if p := u.ap.pool(u.pool); p.Revision != before || len(p.Apportionments) != 0 || len(p.Previews) != 0 {
		t.Fatalf("a refused command wrote: %+v", p)
	}
	v := u.configure(time0, le("fin.F4", f4One))
	if v == 0 {
		t.Fatal("F4 is not in force")
	}
	cmd := u.oneStepCmd("one-step-1", "fin-1", 4, nil, "SH-C")
	r, err := u.module.ApportionConsolCostInOneStep(u.ctx, cmd)
	if err != nil || r.Version != 1 || r.SubjectID != "P-U" || r.NewRevision != cmd.ExpectedRevision+1 {
		t.Fatalf("the one step: %+v %v", r, err)
	}
	p := u.ap.pool(u.pool)
	if len(p.Previews) != 0 || len(p.Apportionments) != 1 {
		t.Fatalf("a preview was stored: %+v", p)
	}
	a := p.Apportionments[0]
	if a.PreviewNo != 0 || a.SettingsVersion != v || a.By != "fin-1" || a.Stage != api.BasisActual || a.EnteredAt.IsZero() {
		t.Fatalf("the apportionment: %+v", a)
	}
	// the figures are those of a preview and its submission
	u.checkLine("one step", a, 1, api.MethodChargeable,
		api.PoolShare{ShareNo: 1, ShipmentID: "SH-A", JobID: "JOB-A", Quantity: 300_000, Unit: "GRM", Amount: cny(60_000), Local: cny(60_000)},
		api.PoolShare{ShareNo: 2, ShipmentID: "SH-B", JobID: "JOB-B", Quantity: 200_000, Unit: "GRM", Amount: cny(40_000), Local: cny(40_000)},
		api.PoolShare{ShareNo: 3, ShipmentID: "SH-C", JobID: "JOB-C", NotUsed: true, Amount: cny(0), Local: cny(0)})
	if cost := u.poolCost("JOB-A"); cost != 60_000 {
		t.Fatalf("JOB-A bears %d", cost)
	}
	// the database row names no preview, and the event is the submission's, with no preview in it
	var noPreview bool
	var payload string
	if err = u.fixture.within(t, func(ctx context.Context, e bentopg.Executor) error {
		if err := e.QueryRow(ctx, `SELECT preview_no IS NULL FROM cargo_fin.pool_apportionment WHERE tenant_id=$1 AND pool_id='P-U'`, u.tenant).Scan(&noPreview); err != nil {
			return err
		}
		return e.QueryRow(ctx, `SELECT payload::text FROM bento.outbox WHERE scope=$1 AND source=$2 AND event_type='ConsolCostsApportioned.v1'`, u.tenant, api.Source).Scan(&payload)
	}); err != nil || !noPreview || strings.Contains(strings.ToLower(payload), "preview") || strings.Contains(payload, "fin-1") {
		t.Fatalf("the row names no preview: %v; the event: %s (%v)", noPreview, payload, err)
	}
	// the replay writes nothing
	if again, err := u.module.ApportionConsolCostInOneStep(u.ctx, cmd); err != nil || again != r {
		t.Fatalf("the replay: %+v %v", again, err)
	}
	if got := u.ap.pool(u.pool); got.Revision != r.NewRevision || len(got.Apportionments) != 1 || countOf(u.outboxTypes(t), "ConsolCostsApportioned") != 1 {
		t.Fatalf("the replay wrote: %+v %v", got, u.outboxTypes(t))
	}
	// the command id is its payload's: another payload under the same id is a conflict
	other := cmd
	other.NotUsed = nil
	if _, err = u.module.ApportionConsolCostInOneStep(u.ctx, other); !errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("the same command id with another payload: %v", err)
	}
	// asked again at the revision it was asked at, another command is a conflict; at the new one it is accepted
	stale := u.oneStepCmd("one-step-2", "fin-1", 5, nil, "SH-C")
	stale.ExpectedRevision = cmd.ExpectedRevision
	if _, err = u.module.ApportionConsolCostInOneStep(u.ctx, stale); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("a stale revision: %v", err)
	}
	if _, err = u.oneStep("one-step-3", "fin-1", 5, nil, "SH-C"); err != nil {
		t.Fatal(err)
	}
	if got := u.ap.pool(u.pool).Apportionments; len(got) != 2 || got[1].PreviewNo != 0 || got[1].SettingsVersion != v {
		t.Fatalf("the second apportionment: %+v", got)
	}
	u.closePool(6)
	if c := u.ap.pool(u.pool); c.State != "CLOSED" {
		t.Fatalf("the pool closes on a one-step apportionment: %+v", c)
	}
}

// Each condition that refuses a one-step apportionment, on its own, through the commands, with its way out.
func TestOneStepApportionmentIsRefusedOnPostgres(t *testing.T) {
	// a former share pending on a line; once attributed, it is accepted and carries the attribution
	u := formerWorld(t)
	u.configure(time0, le("fin.F4", f4One))
	u.depart()
	before := u.ap.pool(u.pool)
	_, err := u.oneStep("one-step-1", "fin-2", 6, []int{1})
	u.refusedWith("a former share", err, "FIN_ONE_STEP_REFUSED")
	for _, part := range []string{"former share pending", "house SH-C on line 1", "preview the lines and submit the preview", "stating who bears it"} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("the refusal does not say %q: %v", part, err)
		}
	}
	if p := u.ap.pool(u.pool); p.Revision != before.Revision || len(p.Apportionments) != 1 || len(p.Previews) != len(before.Previews) {
		t.Fatalf("a refused command wrote: %+v", p)
	}
	u.attribute("attribute-1", 7, statementOf("C", 5_000, 10_000, api.PartyWarehouse))
	r, err := u.oneStep("one-step-2", "fin-2", 8, []int{1})
	if err != nil {
		t.Fatalf("once SH-C is attributed: %v", err)
	}
	p := u.ap.pool(u.pool)
	last := p.Apportionments[len(p.Apportionments)-1]
	x := lineOf(last, 1).Attributions[0]
	if int(r.Version) != last.ApportionmentNo || last.PreviewNo != 0 || len(x.Voucher) == 0 || !x.Carried || x.Loss.Minor != 5_000 || x.Exception.Minor != 10_000 || x.ByRatio || x.ByDefault {
		t.Fatalf("the one step carries the attribution in force: %+v %+v", last, x)
	}
	conservedLine(t, "carried in one step", lineOf(last, 1))

	// a container in rework: the pool is frozen
	w := previewWorld(t)
	w.configure(time0, le("fin.F4", f4One))
	w.openRework("ops-1")
	_, err = w.oneStep("one-step-w", "fin-2", 4, nil, "SH-C")
	w.inWork("in rework", err)

	// a line apportioned and not since a rework ended; a line not apportioned yet is not named, and the asker of the rework
	// is kept from its first apportionment
	k := previewWorld(t)
	k.record("cost-2", "HANDLING", api.MethodChargeable, 10_000)
	k.apportion("apportion-1", 4, []int{1}, "SH-C")
	k.configure(time0, le("fin.F4", f4One))
	k.openRework("ops-1")
	k.reseal(5)
	for _, lines := range [][]int{{1}, {2}} {
		_, err = k.oneStep(fmt.Sprintf("one-step-k-%v", lines), "fin-2", 6, lines, "SH-C")
		k.refusedWith(fmt.Sprintf("line %v with a rework not carried", lines), err, "FIN_ONE_STEP_REFUSED")
		for _, part := range []string{"rework 1 of load unit 1 (stuffing X-1)", "for line 1", "preview the lines and submit the preview"} {
			if !strings.Contains(err.Error(), part) {
				t.Fatalf("the refusal does not say %q: %v", part, err)
			}
		}
		if strings.Contains(err.Error(), "for line 1, 2") || strings.Contains(err.Error(), "line 2") {
			t.Fatalf("a line not apportioned yet is not named: %v", err)
		}
	}
	pre := k.must(k.preview("preview-k", 6, []int{1}, "SH-C"))
	k.must(k.submit("submit-k", "fin-2", int(pre.Version), 6))
	_, err = k.oneStep("one-step-asker", "ops-1", 7, []int{2}, "SH-C")
	k.refusedWith("the asker of the rework", err, "FIN_CONFIRMER_ASKED_REWORK")
	if _, err = k.oneStep("one-step-k2", "fin-2", 7, []int{2}, "SH-C"); err != nil {
		t.Fatalf("line 2, with the rework carried for line 1: %v", err)
	}
	if got := k.ap.pool(k.pool).Apportionments; len(got) != 3 || got[2].PreviewNo != 0 || len(got[2].Lines) != 1 || got[2].Lines[0].LineNo != 2 {
		t.Fatalf("line 2: %+v", got)
	}
}

// F9 and the one step: a carried attribution of a line whose cost changed. RECONFIRM refuses it, naming F9 and the way
// out; KEEP_RATIO carries it scaled, marked; KEEP_AMOUNT carries it as it is.
func TestOneStepApportionmentCarriesTheAttributionsUnderF9OnPostgres(t *testing.T) {
	for _, c := range []struct {
		f9              string
		refused         bool
		loss, exception int64
		ratio           bool
	}{{"RECONFIRM", true, 0, 0, false}, {"KEEP_RATIO", false, 6_000, 12_000, true}, {"KEEP_AMOUNT", false, 5_000, 10_000, false}} {
		u := formerWorld(t)
		u.configure(time0, le("fin.F4", f4One), le("fin.F9", c.f9))
		u.depart()
		u.attribute("attribute-1", 6, statementOf("C", 5_000, 10_000, api.PartyWarehouse))
		u.reviseLine("revise-1", 1, 120_000, 7)
		_, err := u.oneStep("one-step-1", "fin-2", 8, []int{1})
		if c.refused {
			u.refusedWith(c.f9, err, "FIN_ONE_STEP_REFUSED")
			for _, part := range []string{"F9 RECONFIRM", "house SH-C", "line 1", "CNY 1,000.00", "CNY 1,200.00", "preview the lines and submit the preview"} {
				if !strings.Contains(err.Error(), part) {
					t.Fatalf("the refusal does not say %q: %v", part, err)
				}
			}
			if !errors.Is(err, api.ErrRejected) || len(u.ap.pool(u.pool).Apportionments) != 2 {
				t.Fatalf("a refused command wrote: %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", c.f9, err)
		}
		p := u.ap.pool(u.pool)
		last := p.Apportionments[len(p.Apportionments)-1]
		x := lineOf(last, 1).Attributions[0]
		if last.PreviewNo != 0 || !x.Carried || x.ByRatio != c.ratio || x.Loss.Minor != c.loss || x.Exception.Minor != c.exception || lineOf(last, 1).Amount.Minor != 120_000 {
			t.Fatalf("%s: %+v", c.f9, last)
		}
		conservedLine(t, c.f9, lineOf(last, 1))
	}
}

// F9 end to end, in formerWorld: SH-C's attribution of loss CNY 50.00 and exception cost CNY 100.00 (the warehouse's)
// is stated at the line's cost of CNY 1,000.00; the line is revised to CNY 1,200.00 or CNY 800.00 and previewed again
// with nothing stated. Every line is conserved in both currencies under every value.
func TestF9EndToEndOnPostgres(t *testing.T) {
	for _, c := range []struct {
		name            string
		f9              string
		cost            int64
		refusal         string
		loss, exception int64
		carried, ratio  bool
		houses          [2]int64 // SH-A and SH-B, the remainder on 300,000 : 200,000
	}{
		{"KEEP_AMOUNT raised", "KEEP_AMOUNT", 120_000, "", 5_000, 10_000, true, false, [2]int64{63_000, 42_000}},
		{"KEEP_AMOUNT lowered", "KEEP_AMOUNT", 80_000, "", 5_000, 10_000, true, false, [2]int64{39_000, 26_000}},
		{"KEEP_RATIO raised", "KEEP_RATIO", 120_000, "", 6_000, 12_000, true, true, [2]int64{61_200, 40_800}},
		{"KEEP_RATIO lowered", "KEEP_RATIO", 80_000, "", 4_000, 8_000, true, true, [2]int64{40_800, 27_200}},
		{"RECONFIRM raised", "RECONFIRM", 120_000, "FIN_ATTRIBUTION_RECONFIRM", 0, 0, false, false, [2]int64{}},
		{"RECONFIRM lowered", "RECONFIRM", 80_000, "FIN_ATTRIBUTION_RECONFIRM", 0, 0, false, false, [2]int64{}},
	} {
		u := formerWorld(t)
		u.configure(time0, le("fin.F9", c.f9))
		u.depart()
		u.attribute("attribute-1", 6, statementOf("C", 5_000, 10_000, api.PartyWarehouse))
		u.reviseLine("revise-1", 1, c.cost, 7)
		var err error
		if c.refusal != "" {
			_, err = u.module.PreviewConsolCostApportionment(u.ctx, u.attributeCmd("again", "ops-2", 8))
			u.refusedWith(c.name, err, c.refusal)
			cost := fmt.Sprintf("CNY %d,%03d.00", c.cost/100_000, c.cost/100%1000)
			if c.cost < 100_000 {
				cost = fmt.Sprintf("CNY %d.00", c.cost/100)
			}
			for _, part := range []string{"house SH-C", "line 1", "CNY 1,000.00", cost, "state the attribution of house SH-C on line 1 again"} {
				if !strings.Contains(err.Error(), part) {
					t.Fatalf("%s: the refusal does not say %q: %v", c.name, part, err)
				}
			}
			if p := u.ap.pool(u.pool); len(p.Apportionments) != 2 || len(p.Previews) != 2 {
				t.Fatalf("%s: a refused preview was stored: %+v", c.name, p)
			}
			continue
		}
		if _, err = u.attributeBy("again", "ops-2", "fin-2", 8); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		a := u.lastApportionment()
		la := lineOf(a, 1)
		x := la.Attributions[0]
		if x.Carried != c.carried || x.ByRatio != c.ratio || x.ByDefault || x.Loss.Minor != c.loss || x.Exception.Minor != c.exception || x.Voucher != "CLAIM-1" ||
			la.Amount.Minor != c.cost || u.shareOf(a, "SH-A") != c.houses[0] || u.shareOf(a, "SH-B") != c.houses[1] {
			t.Fatalf("%s: %+v", c.name, a)
		}
		conservedLine(t, c.name, la)
	}
	// RECONFIRM: an equal restatement is recorded as stated, with no reason, and is a confirmation (the asker is barred);
	// the next preview on the unchanged line carries it
	u := formerWorld(t)
	u.configure(time0, le("fin.F9", "RECONFIRM"))
	u.depart()
	u.attribute("attribute-1", 6, statementOf("C", 5_000, 10_000, api.PartyWarehouse))
	u.reviseLine("revise-1", 1, 120_000, 7)
	same := statementOf("C", 5_000, 10_000, api.PartyWarehouse)
	_, err := u.attributeBy("restate-asker", "ops-2", "ops-1", 8, same)
	u.refusedWith("the asker restates", err, "FIN_ATTRIBUTION_CONFIRMER")
	if _, err = u.attributeBy("restate", "ops-2", "fin-2", 8, same); err != nil {
		t.Fatalf("the equal restatement: %v", err)
	}
	a := u.lastApportionment()
	x := lineOf(a, 1).Attributions[0]
	if x.Carried || x.ByRatio || x.Reason != "" || x.Loss.Minor != 5_000 || lineOf(a, 1).Amount.Minor != 120_000 {
		t.Fatalf("the restatement is recorded as stated: %+v", x)
	}
	conservedLine(t, "restated", lineOf(a, 1))
	if _, err = u.attributeBy("next", "ops-2", "fin-2", 9); err != nil {
		t.Fatalf("the restated line carries: %v", err)
	}
	if x = lineOf(u.lastApportionment(), 1).Attributions[0]; !x.Carried {
		t.Fatalf("carried: %+v", x)
	}
	u.reviseLine("revise-2", 2, 130_000, 10)
	_, err = u.module.PreviewConsolCostApportionment(u.ctx, u.attributeCmd("again", "ops-2", 11))
	u.refusedWith("measured from the restatement", err, "FIN_ATTRIBUTION_RECONFIRM")
	if !strings.Contains(err.Error(), "CNY 1,200.00") || !strings.Contains(err.Error(), "CNY 1,300.00") {
		t.Fatalf("the amounts then and now: %v", err)
	}
}

// F8 end to end: SH-C's former share is attributed by default when nobody states it, with no voucher, and the whole of
// it goes to the cargo that remains; the preview shows it, the effect and the pending item say what it does; the bars of
// F1 and F2 apply to the confirmation; the pool, the job and the consolidation read back what it is.
func TestF8DefaultEndToEndOnPostgres(t *testing.T) {
	u := formerWorld(t)
	u.depart()
	_, err := u.module.PreviewConsolCostApportionment(u.ctx, u.attributeCmd("preview-0", "ops-2", 6))
	u.refusedWith("REJECT_WITH_DEFAULT", err, "FIN_ATTRIBUTION_MISSING")
	v := u.configure(time0, le("fin.F3", f3Moves), le("fin.F8", f8Dflt))
	if v == 0 {
		t.Fatal("F8 is not in force")
	}
	item := func() string {
		for _, it := range u.pending() {
			if it.Kind == api.PendingFormerSharePending {
				return it.Message
			}
		}
		return ""
	}
	if m := item(); !strings.Contains(m, "Under F8 DEFAULT_TO_REMAINING") || !strings.Contains(m, "loss 0 and exception cost 0") || !strings.Contains(m, "cargo that remains") {
		t.Fatalf("the pending item does not say what the default does: %q", m)
	}
	pre := u.must(u.module.PreviewConsolCostApportionment(u.ctx, u.attributeCmd("preview-1", "ops-2", 6)))
	e := u.effect(int(pre.Version))
	var reason api.PreviewReason
	for _, r := range e.Reasons {
		if r.ShipmentID == "SH-C" {
			reason = r
		}
	}
	if !e.Current || !reason.ByDefault || reason.Carried || !strings.Contains(reason.Says, "F8 DEFAULT_TO_REMAINING") || !strings.Contains(reason.Says, "CNY 230.77") {
		t.Fatalf("the effect: %+v", e)
	}
	// the bars of F1 apply: the asker and the one who ended the house's place are refused, another finance user is not
	for _, who := range []string{"ops-1", "ops-9"} {
		_, err = u.submit("submit-"+who, who, int(pre.Version), 6)
		u.refusedWith(who, err, "FIN_ATTRIBUTION_CONFIRMER")
	}
	if _, err = u.submit("submit-fin-2", "fin-2", int(pre.Version), 6); err != nil {
		t.Fatal(err)
	}
	a := u.lastApportionment()
	la := lineOf(a, 1)
	x := la.Attributions[0]
	if a.SettingsVersion != v || a.PreviewNo != int(pre.Version) || x.ShipmentID != "SH-C" || !x.ByDefault || x.Carried || x.ByRatio || x.Loss.Minor != 0 || x.Exception.Minor != 0 || x.Party != "" ||
		x.Voucher != "" || u.shareOf(a, "SH-A") != 60_000 || u.shareOf(a, "SH-B") != 40_000 {
		t.Fatalf("the default: %+v", a)
	}
	conservedLine(t, "the default", la)
	if c := u.jobProfit("JOB-C"); c.PoolCost.Minor != 0 || c.FormerSharesPending.Minor != 0 || len(c.FormerShares) != 0 {
		t.Fatalf("JOB-C bears nothing and holds nothing pending: %+v", c)
	}
	if v := u.ap.pool(u.pool).Attributions; len(v) != 1 || len(v[0].FormerShares) != 0 || len(v[0].Losses) != 0 {
		t.Fatalf("the pool's view: %+v", v)
	}
	if got := u.consolProfit(); got.Unapportioned.Minor != 0 || got.FormerSharesPending.Minor != 0 {
		t.Fatalf("the consolidation: %+v", got)
	}
	// the next apportionment carries it, marked as it was made
	if _, err = u.attributeBy("next", "ops-2", "ops-1", 7); err != nil {
		t.Fatalf("a carried default is no new confirmation: %v", err)
	}
	if x = lineOf(u.lastApportionment(), 1).Attributions[0]; !x.Carried || !x.ByDefault {
		t.Fatalf("carried: %+v", x)
	}

	// F1's threshold is read on the former share (CNY 230.77) and F2 on the previewer, as for a stated attribution
	for _, c := range []struct {
		name                 string
		entries              []api.FinSettingEntry
		previewer, submitter string
		refusal              string
	}{
		{"threshold below the share", []api.FinSettingEntry{thresholdOf(23_076)}, "ops-2", "ops-1", "FIN_ATTRIBUTION_CONFIRMER"},
		{"threshold at the share", []api.FinSettingEntry{thresholdOf(23_077)}, "ops-2", "ops-1", ""},
		{"UNRESTRICTED", []api.FinSettingEntry{le("fin.F1", "UNRESTRICTED")}, "ops-2", "ops-9", ""},
		{"F2 both, the previewer asked", []api.FinSettingEntry{le("fin.F2", "SUBMITTER_AND_PREVIEWER")}, "ops-1", "fin-2", "FIN_ATTRIBUTION_CONFIRMER"},
	} {
		w := formerWorld(t)
		w.configure(time0, append([]api.FinSettingEntry{le("fin.F3", f3Moves), le("fin.F8", f8Dflt)}, c.entries...)...)
		w.depart()
		_, err = w.attributeBy("default", c.previewer, c.submitter, 6)
		if c.refusal != "" {
			w.refusedWith(c.name, err, c.refusal)
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
}

// A preview made before a settings change and submitted after it is stale, for F4's, F8's and F9's changes as for any
// other: the version is in the fingerprint, and the refusal says what changed and the way out.
func TestAPreviewMadeBeforeAChangeOfF4F8OrF9IsStaleOnPostgres(t *testing.T) {
	for _, entries := range [][]api.FinSettingEntry{{le("fin.F9", "KEEP_RATIO")}, {le("fin.F9", "RECONFIRM")}, {le("fin.F4", f4One)}, {le("fin.F3", f3Moves), le("fin.F8", f8Dflt)}} {
		u := formerWorld(t)
		u.depart()
		pre := u.must(u.module.PreviewConsolCostApportionment(u.ctx, u.attributeCmd("preview-1", "ops-2", 6, statementOf("C", 5_000, 10_000, api.PartyWarehouse))))
		v := u.configure(time0, entries...)
		_, err := u.submit("submit-1", "fin-2", int(pre.Version), 6)
		u.refusedWith(entries[0].Key, err, "FIN_PREVIEW_STALE")
		if !strings.Contains(err.Error(), fmt.Sprintf("the settings changed from version 0 to %d", v)) || !strings.Contains(err.Error(), "preview again") {
			t.Fatalf("%s: %v", entries[0].Key, err)
		}
		if n := len(u.ap.pool(u.pool).Apportionments); n != 1 {
			t.Fatalf("a stale submission was written: %d", n)
		}
	}
}

// A one-step command and a preview race for one pool, asked at the same revision: whichever is granted the pool's lock
// first commits and the other is a conflict, deterministically (the lock is held, the commands queue behind it in order,
// and are released together). The one that lost is asked again at the new revision and is accepted.
func TestOneStepAndAPreviewRaceForOnePoolOnPostgres(t *testing.T) {
	for _, oneStepFirst := range []bool{true, false} {
		u := previewWorld(t)
		u.configure(time0, le("fin.F4", f4One))
		one := u.oneStepCmd("one-step", "fin-1", 4, nil, "SH-C")
		preview := u.apportionCmd("preview", 4, nil, "SH-C")
		doOne := func() error { _, err := u.module.ApportionConsolCostInOneStep(u.ctx, one); return err }
		doPreview := func() error { _, err := u.module.PreviewConsolCostApportionment(u.ctx, preview); return err }
		var errs []error
		if oneStepFirst {
			errs = queued(t, u, doOne, doPreview)
		} else {
			errs = queued(t, u, doPreview, doOne)
		}
		if errs[0] != nil || !errors.Is(errs[1], api.ErrConflict) {
			t.Fatalf("one step first %v: the first returned %v, the second %v; want the first committed and the second a conflict", oneStepFirst, errs[0], errs[1])
		}
		p := u.ap.pool(u.pool)
		if oneStepFirst {
			if len(p.Apportionments) != 1 || p.Apportionments[0].PreviewNo != 0 || len(p.Previews) != 0 {
				t.Fatalf("the one step committed first: %+v", p)
			}
			if _, err := u.preview("preview-again", 5, nil, "SH-C"); err != nil {
				t.Fatalf("the preview, asked again: %v", err)
			}
			continue
		}
		if len(p.Apportionments) != 0 || len(p.Previews) != 1 {
			t.Fatalf("the preview committed first: %+v", p)
		}
		if _, err := u.oneStep("one-step-again", "fin-1", 5, nil, "SH-C"); err != nil {
			t.Fatalf("the one step, asked again: %v", err)
		}
		if p = u.ap.pool(u.pool); len(p.Apportionments) != 1 || p.Apportionments[0].PreviewNo != 0 || p.Previews[0].SubmittedAs != 0 {
			t.Fatalf("the preview is not the apportionment's: %+v", p)
		}
	}
}

// recordingAuthorizer allows every action and notes which it was asked about.
type recordingAuthorizer struct{ actions *[]string }

func (r recordingAuthorizer) AuthorizeFinAction(_ context.Context, _, _, action, _ string) error {
	*r.actions = append(*r.actions, action)
	return nil
}

// The one-step command asks the authorizer about its own action, as the other commands of the pool ask about theirs.
func TestOneStepApportionmentAsksForItsOwnActionOnPostgres(t *testing.T) {
	u := previewWorld(t)
	u.configure(time0, le("fin.F4", f4One))
	var asked []string
	m, err := finmodule.New(u.db, u.w, u.w, u.w, u.w, u.w, recordingAuthorizer{&asked})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.ApportionConsolCostInOneStep(u.ctx, u.oneStepCmd("one-step-auth", "fin-1", 4, nil, "SH-C")); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0] != "ApportionConsolCostInOneStep" {
		t.Fatalf("the actions asked about: %v", asked)
	}
}

// An apportionment of the pool that names no preview does not make another one's stated attribution a refused one: the
// database reads the apportionment the attribution belongs to.
func TestAStatedAttributionAfterAOneStepApportionmentOnPostgres(t *testing.T) {
	u := formerWorld(t)
	u.configure(time0, le("fin.F4", f4One))
	if _, err := u.oneStep("one-step-1", "fin-1", 5, []int{1}); err != nil {
		t.Fatal(err)
	}
	u.depart()
	a := u.attribute("attribute-1", 6, statementOf("C", 5_000, 10_000, api.PartyWarehouse))
	if a.PreviewNo == 0 || lineOf(a, 1).Attributions[0].Carried {
		t.Fatalf("the attribution is stated in an apportionment of a preview: %+v", a)
	}
}

// The one-step command hands the application everything a preview's input has: the houses taken in again, the method,
// the manual amounts and the reason; a command that dropped one on the way would write another apportionment.
func TestOneStepApportionmentPassesItsWholeInputOnPostgres(t *testing.T) {
	// AllHouses takes in the house the line's latest apportionment left out
	u := previewWorld(t)
	u.configure(time0, le("fin.F4", f4One))
	u.apportion("apportion-1", 4, []int{1}, "SH-C")
	cmd := u.oneStepCmd("one-step-all", "fin-1", 5, []int{1})
	cmd.AllHouses = true
	if _, err := u.module.ApportionConsolCostInOneStep(u.ctx, cmd); err != nil {
		t.Fatal(err)
	}
	la := lineOf(u.lastApportionment(), 1)
	used := 0
	for _, s := range la.Shares {
		if !s.NotUsed {
			used++
		}
	}
	if used != 3 {
		t.Fatalf("AllHouses: %+v", la)
	}
	// the method
	v := previewWorld(t)
	v.configure(time0, le("fin.F4", f4One))
	cmd = v.oneStepCmd("one-step-volume", "fin-1", 4, []int{1}, "SH-C")
	cmd.Method = api.MethodVolume
	if _, err := v.module.ApportionConsolCostInOneStep(v.ctx, cmd); err != nil {
		t.Fatal(err)
	}
	if got := lineOf(v.lastApportionment(), 1); got.Method != api.MethodVolume || got.Shares[0].Unit != "CMQ" {
		t.Fatalf("the method: %+v", got)
	}
	// the manual amounts and their reason
	w := previewWorld(t)
	w.configure(time0, le("fin.F4", f4One))
	cmd = w.oneStepCmd("one-step-manual", "fin-1", 4, []int{1}, "SH-C")
	cmd.Method, cmd.Reason = api.MethodManual, "agreed with the carrier"
	cmd.Manual = []api.ManualShare{{ShipmentID: "SH-A", Amount: 70_000}, {ShipmentID: "SH-B", Amount: 30_000}}
	if _, err := w.module.ApportionConsolCostInOneStep(w.ctx, cmd); err != nil {
		t.Fatal(err)
	}
	got := lineOf(w.lastApportionment(), 1)
	if got.Method != api.MethodManual || got.Reason != "agreed with the carrier" || got.Shares[0].Amount.Minor != 70_000 || got.Shares[1].Amount.Minor != 30_000 {
		t.Fatalf("manual: %+v", got)
	}
}

// timeClock is FIN's clock that notes every time it answers.
type timeClock struct {
	mu    sync.Mutex
	times []time.Time
}

func (c *timeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := time.Now().UTC()
	c.times = append(c.times, t)
	return t
}

// A one-step apportionment is stamped with the time its settings were read at, as a submission is: that time is the
// decision's own reading of the clock, not the one the command read first.
func TestOneStepApportionmentIsStampedWithTheTimeOfItsSettingsOnPostgres(t *testing.T) {
	u := previewWorld(t)
	u.configure(time0, le("fin.F4", f4One))
	clock := &timeClock{}
	m, err := finmodule.New(u.db, u.w, u.w, u.w, u.w, u.w, allow{}, finmodule.WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.ApportionConsolCostInOneStep(u.ctx, u.oneStepCmd("one-step-stamp", "fin-1", 4, nil, "SH-C")); err != nil {
		t.Fatal(err)
	}
	entered := u.ap.pool(u.pool).Apportionments[0].EnteredAt
	if len(clock.times) < 3 || !entered.Equal(clock.times[2].Truncate(time.Microsecond)) {
		t.Fatalf("the apportionment is entered at %s; the clock answered %v", entered, clock.times)
	}
}
