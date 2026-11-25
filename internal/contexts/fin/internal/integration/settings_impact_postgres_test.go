//go:build integration

package integration

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	finmodule "go.idp.xyz/idp-cargo/internal/contexts/fin"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
)

// FIN V2.100: the impact of a settings change before it takes effect, through FIN's query on PostgreSQL, on the worlds
// of V2.97 and V2.98: previewWorld and formerWorld (SH-C leaves with a former share of CNY 230.77).

func (u usedWorld) impact(entries ...api.FinSettingEntry) api.FinSettingsChangeImpact {
	u.t.Helper()
	got, err := u.module.GetFinSettingsChangeImpact(u.ctx, api.GetFinSettingsChangeImpact{TenantID: u.tenant, ActorID: "ann", Entries: entries})
	if err != nil {
		u.t.Fatal(err)
	}
	return got
}

// nothingElse fails when the answer holds anything but what the test looked at.
func nothingElse(t *testing.T, name string, got api.FinSettingsChangeImpact, closes, jobs, defaults, carried, oneStep, checks int) {
	t.Helper()
	if len(got.Closes) != closes || len(got.Jobs) != jobs || len(got.Defaults) != defaults || len(got.Carried) != carried || len(got.OneStep) != oneStep ||
		len(got.Checks) != checks || len(got.NotAssessed) != 0 {
		t.Fatalf("%s: closes %d jobs %d defaults %d carried %d one-step %d checks %d, not assessed %v: %+v", name, len(got.Closes), len(got.Jobs), len(got.Defaults),
			len(got.Carried), len(got.OneStep), len(got.Checks), got.NotAssessed, got)
	}
}

// Each answer lists the items that change for the legal entities whose settings differ, and the unsubmitted previews of
// the tenant's open pools, which every change makes stale (the version is the tenant's); an item that does not change
// changes nothing; a change for another legal entity does not report this one's pools.
func TestTheImpactListsTheItemsAndTheStalePreviewsOnPostgres(t *testing.T) {
	u := formerWorld(t)
	u.depart()
	u.settleInvoice(7)
	u.attribute("attribute", 6, statementOf("C", 5_000, 10_000, api.PartyPending)) // an exception cost whose party is pending
	p1 := u.must(u.preview("preview-1", 8, nil))
	u.must(u.module.OpenFinancePolicy(u.ctx, api.OpenFinancePolicy{CommandID: "policy-2", TenantID: u.tenant, ActorID: "acct-1", LegalEntityID: "LE2",
		FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: u.hours(1)}))

	got := u.impact(le("fin.F7", "BLOCKS"))
	if got.VersionNow != 0 || got.VersionAfter != 1 || len(got.Entities) != 1 || got.Entities[0].LegalEntityID != "LE" || len(got.Entities[0].Changes) != 1 ||
		got.Entities[0].Changes[0] != (api.FinItemChange{Key: "fin.F7", From: "DOES_NOT_BLOCK", To: "BLOCKS"}) {
		t.Fatalf("the items that change: %+v", got)
	}
	if len(got.StalePreviews) != 1 || got.StalePreviews[0].PoolID != u.pool || got.StalePreviews[0].PreviewNo != int(p1.Version) || got.StalePreviews[0].PreviewedBy != "fin-1" {
		t.Fatalf("the preview that becomes stale: %+v", got.StalePreviews)
	}
	// an item that does not change changes nothing; the version still is the tenant's next, so the preview still goes
	// stale only when a change set is made
	same := u.impact(le("fin.F7", "DOES_NOT_BLOCK"))
	if len(same.Entities) != 0 {
		t.Fatalf("an unchanged item: %+v", same.Entities)
	}
	nothingElse(t, "an unchanged item", same, 0, 0, 0, 0, 0, 0)
	// a change for another legal entity does not report this one's pools, and one for the company reports
	// both legal entities
	other := u.impact(api.FinSettingEntry{ScopeKind: "LEGAL_ENTITY", ScopeID: "LE2", Key: "fin.F7", Value: "BLOCKS"})
	if len(other.Entities) != 1 || other.Entities[0].LegalEntityID != "LE2" {
		t.Fatalf("a change for LE2: %+v", other.Entities)
	}
	nothingElse(t, "a change for another legal entity", other, 0, 0, 0, 0, 0, 0)
	company := u.impact(api.FinSettingEntry{ScopeKind: "TENANT", Key: "fin.F7", Value: "BLOCKS"})
	if len(company.Entities) != 2 || len(company.Closes) != 1 {
		t.Fatalf("a change for the company: %+v", company)
	}
	// a proposal that cannot be made is refused as a proposal is, with its way out
	_, err := u.module.GetFinSettingsChangeImpact(u.ctx, api.GetFinSettingsChangeImpact{TenantID: u.tenant, ActorID: "ann", Entries: []api.FinSettingEntry{le("fin.F8", f8Dflt)}})
	u.refusedWith("F8 without F3", err, "SETTINGS_RULE")
	_, err = u.module.GetFinSettingsChangeImpact(u.ctx, api.GetFinSettingsChangeImpact{TenantID: u.tenant, ActorID: "ann",
		Entries: []api.FinSettingEntry{{ScopeKind: "LEGAL_ENTITY", ScopeID: "LE-9", Key: "fin.F7", Value: "BLOCKS"}}})
	u.refusedWith("a legal entity without a policy", err, "no finance policy")
}

// F7: the close of a pool that holds an exception cost whose party is pending becomes refused under BLOCKS, and allowed
// again when BLOCKS is set and DOES_NOT_BLOCK is asked.
func TestTheImpactOfF7OnTheCloseOnPostgres(t *testing.T) {
	u := formerWorld(t)
	u.depart()
	u.attribute("attribute", 6, statementOf("C", 5_000, 10_000, api.PartyPending))
	u.settleInvoice(7)
	got := u.impact(le("fin.F7", "BLOCKS"))
	if len(got.Closes) != 1 || got.Closes[0].PoolID != u.pool || got.Closes[0].Now != "ALLOWED" || got.Closes[0].After != "REFUSED" || !strings.Contains(got.Closes[0].Refusal, "F7 BLOCKS") {
		t.Fatalf("under BLOCKS: %+v", got.Closes)
	}
	nothingElse(t, "F7", got, 1, 0, 0, 0, 0, 0)
	u.configure(time0, le("fin.F7", "BLOCKS"))
	got = u.impact(le("fin.F7", "DOES_NOT_BLOCK"))
	if len(got.Closes) != 1 || got.Closes[0].Now != "REFUSED" || got.Closes[0].After != "ALLOWED" || !strings.Contains(got.Closes[0].Refusal, "F7 BLOCKS") {
		t.Fatalf("back to DOES_NOT_BLOCK: %+v", got.Closes)
	}
	if again := u.impact(le("fin.F7", "BLOCKS")); len(again.Closes) != 0 || len(again.Entities) != 0 {
		t.Fatalf("BLOCKS again: %+v", again)
	}
	// a pool that cannot be closed for another reason is not reported: the outcome does not change
	other := formerWorld(t)
	other.depart()
	other.attribute("attribute", 6, statementOf("C", 5_000, 10_000, api.PartyPending))
	if got = other.impact(le("fin.F7", "BLOCKS")); len(got.Closes) != 0 {
		t.Fatalf("a pool whose cost is not final: %+v", got.Closes)
	}
}

// F6: the profit of the job that bears a former share pending would count it, with the profit now and after.
func TestTheImpactOfF6OnTheJobProfitOnPostgres(t *testing.T) {
	u := formerWorld(t)
	u.depart()
	now := u.jobProfit("JOB-C")
	got := u.impact(le("fin.F6", "INCLUDED_MARKED"))
	if len(got.Jobs) != 1 || got.Jobs[0].JobID != "JOB-C" || got.Jobs[0].ProfitNow != now.Profit || got.Jobs[0].ProfitAfter.Minor != now.Profit.Minor-23_077 ||
		got.Jobs[0].BasisNow != "SEPARATE" || got.Jobs[0].BasisAfter != "INCLUDED_MARKED" {
		t.Fatalf("the jobs: %+v (profit now %+v)", got.Jobs, now.Profit)
	}
	nothingElse(t, "F6", got, 0, 1, 0, 0, 0, 0)
	if same := u.impact(le("fin.F6", "SEPARATE")); len(same.Jobs) != 0 {
		t.Fatalf("an unchanged item: %+v", same.Jobs)
	}
	// once the former share is attributed nothing is pending and no profit changes
	u.attribute("attribute", 6, statementOf("C", 5_000, 10_000, api.PartyCarrier))
	if got = u.impact(le("fin.F6", "INCLUDED_MARKED")); len(got.Jobs) != 0 {
		t.Fatalf("nothing pending: %+v", got.Jobs)
	}
	// and under INCLUDED_MARKED the way back lists the job
	v := formerWorld(t)
	v.depart()
	v.configure(time0, le("fin.F6", "INCLUDED_MARKED"))
	if got = v.impact(le("fin.F6", "SEPARATE")); len(got.Jobs) != 1 || got.Jobs[0].ProfitAfter.Minor != got.Jobs[0].ProfitNow.Minor+23_077 {
		t.Fatalf("back to SEPARATE: %+v", got.Jobs)
	}
}

// F8: a line that holds a former share nobody attributed would be defaulted to the cargo that remains, or would
// stop being defaulted.
func TestTheImpactOfF8OnTheDefaultOnPostgres(t *testing.T) {
	u := formerWorld(t)
	u.depart()
	got := u.impact(le("fin.F8", f8Dflt), le("fin.F3", f3Moves))
	if len(got.Defaults) != 1 || got.Defaults[0].PoolID != u.pool || got.Defaults[0].LineNo != 1 || !slices.Equal(got.Defaults[0].Houses, []string{"SH-C"}) ||
		got.Defaults[0].Change != "WOULD_BE_DEFAULTED" {
		t.Fatalf("the defaults: %+v", got.Defaults)
	}
	nothingElse(t, "F8", got, 0, 0, 1, 0, 0, 0)
	if same := u.impact(le("fin.F8", "REJECT_WITH_DEFAULT")); len(same.Defaults) != 0 {
		t.Fatalf("an unchanged item: %+v", same.Defaults)
	}
	u.configure(time0, le("fin.F8", f8Dflt), le("fin.F3", f3Moves))
	if got = u.impact(le("fin.F8", "REJECT_WITH_DEFAULT")); len(got.Defaults) != 1 || got.Defaults[0].Change != "WOULD_STOP_BEING_DEFAULTED" {
		t.Fatalf("back to REJECT_WITH_DEFAULT: %+v", got.Defaults)
	}
	// a line with nothing pending is not listed
	v := formerWorld(t)
	if got = v.impact(le("fin.F8", f8Dflt), le("fin.F3", f3Moves)); len(got.Defaults) != 0 {
		t.Fatalf("nothing pending: %+v", got.Defaults)
	}
}

// F9: after the line's cost changed, the attribution carried would be rescaled (with its amounts now and after) or have
// to be stated again; nothing is told while the cost is what it was.
func TestTheImpactOfF9OnTheCarriedAttributionsOnPostgres(t *testing.T) {
	u := formerWorld(t)
	u.depart()
	u.attribute("attribute", 6, statementOf("C", 5_000, 10_000, api.PartyWarehouse))
	if got := u.impact(le("fin.F9", "KEEP_RATIO")); len(got.Carried) != 0 {
		t.Fatalf("the cost has not changed: %+v", got.Carried)
	}
	u.reviseLine("revise-1", 1, 120_000, 7)
	got := u.impact(le("fin.F9", "KEEP_RATIO"))
	if len(got.Carried) != 1 || got.Carried[0].LineNo != 1 || got.Carried[0].Now != "KEPT" || got.Carried[0].After != "RESCALED" || len(got.Carried[0].Houses) != 1 {
		t.Fatalf("under KEEP_RATIO: %+v", got.Carried)
	}
	h := got.Carried[0].Houses[0]
	if h.ShipmentID != "SH-C" || h.LossNow.Minor != 5_000 || h.ExceptionNow.Minor != 10_000 || h.LossAfter.Minor != 6_000 || h.ExceptionAfter.Minor != 12_000 || !h.AfterKnown {
		t.Fatalf("the amounts now and after: %+v", h)
	}
	nothingElse(t, "F9", got, 0, 0, 0, 1, 0, 0)
	got = u.impact(le("fin.F9", "RECONFIRM"))
	if len(got.Carried) != 1 || got.Carried[0].Now != "KEPT" || got.Carried[0].After != "STATE_AGAIN" || got.Carried[0].Houses[0].AfterKnown {
		t.Fatalf("under RECONFIRM: %+v", got.Carried)
	}
	if same := u.impact(le("fin.F9", "KEEP_AMOUNT")); len(same.Carried) != 0 {
		t.Fatalf("an unchanged item: %+v", same.Carried)
	}
	u.configure(time0, le("fin.F9", "KEEP_RATIO"))
	got = u.impact(le("fin.F9", "KEEP_AMOUNT"))
	if len(got.Carried) != 1 || got.Carried[0].Now != "RESCALED" || got.Carried[0].After != "KEPT" || got.Carried[0].Houses[0].LossNow.Minor != 6_000 ||
		got.Carried[0].Houses[0].LossAfter.Minor != 5_000 {
		t.Fatalf("back to KEEP_AMOUNT: %+v", got.Carried)
	}
}

// F4: a pool that can be apportioned in one step only once F4 allows it, and cannot once F4 does not; a pool that holds
// a former share pending is refused in both, so it is not reported.
func TestTheImpactOfF4OnTheOneStepOnPostgres(t *testing.T) {
	u := previewWorld(t)
	got := u.impact(le("fin.F4", f4One))
	if len(got.OneStep) != 1 || got.OneStep[0].PoolID != u.pool || got.OneStep[0].Change != "BECOMES_POSSIBLE" || !strings.Contains(got.OneStep[0].Refusal, "F4 is not") {
		t.Fatalf("under F4: %+v", got.OneStep)
	}
	nothingElse(t, "F4", got, 0, 0, 0, 0, 1, 0)
	if same := u.impact(le("fin.F4", "ALL_PREVIEWED")); len(same.OneStep) != 0 {
		t.Fatalf("an unchanged item: %+v", same.OneStep)
	}
	u.configure(time0, le("fin.F4", f4One))
	if got = u.impact(le("fin.F4", "ALL_PREVIEWED")); len(got.OneStep) != 1 || got.OneStep[0].Change != "BECOMES_REFUSED" {
		t.Fatalf("back to ALL_PREVIEWED: %+v", got.OneStep)
	}
	v := formerWorld(t)
	v.depart()
	if got = v.impact(le("fin.F4", f4One)); len(got.OneStep) != 0 {
		t.Fatalf("a former share pending: %+v", got.OneStep)
	}
}

// F1, F2 and F3: an unsubmitted preview that holds an attribution would be checked differently, with the rule; nobody
// is named as the submitter. A preview with nothing to check is not reported for F2.
func TestTheImpactOnTheCheckOfAPreviewOnPostgres(t *testing.T) {
	u := formerWorld(t)
	u.depart()
	statement := statementOf("C", 5_000, 10_000, api.PartyCarrier)
	preview := func(w usedWorld, previewer string, s api.AttributionStatement) int {
		r := w.must(w.module.PreviewConsolCostApportionment(w.ctx, w.attributeCmd("preview-"+previewer, previewer, 6, s)))
		return int(r.Version)
	}
	no := preview(u, "ops-2", statement)
	has := func(got api.FinSettingsChangeImpact, rule, now, after string) bool {
		return slices.ContainsFunc(got.Checks, func(c api.FinImpactCheck) bool {
			return c.Rule == rule && c.PreviewNo == no && c.PoolID == u.pool && strings.Contains(c.Now, now) && strings.Contains(c.After, after)
		})
	}
	got := u.impact(le("fin.F1", "UNRESTRICTED"))
	if len(got.Checks) != 1 || !has(got, "F1", "checked", "not checked") || got.Checks[0].LineNo != 1 || got.Checks[0].ShipmentID != "SH-C" {
		t.Fatalf("F1 UNRESTRICTED: %+v", got.Checks)
	}
	nothingElse(t, "F1", got, 0, 0, 0, 0, 0, 1)
	// a threshold above the former share of CNY 230.77: the person is no longer barred; below it, nothing changes
	if got = u.impact(thresholdOf(100_000)); !has(got, "F1", "checked", "not checked") {
		t.Fatalf("a threshold above the share: %+v", got.Checks)
	}
	if got = u.impact(thresholdOf(10_000)); len(got.Checks) != 0 {
		t.Fatalf("a threshold below the share: %+v", got.Checks)
	}
	if got = u.impact(le("fin.F1", "NOT_ALLOWED")); len(got.Checks) != 0 {
		t.Fatalf("an unchanged item: %+v", got.Checks)
	}
	got = u.impact(le("fin.F2", "SUBMITTER_AND_PREVIEWER"))
	if len(got.Checks) != 1 || !has(got, "F2", "submitter only", "previewer ops-2") {
		t.Fatalf("F2: %+v", got.Checks)
	}
	// F3: an attribution that moves no amount needs no voucher under ONLY_WHEN_AMOUNT_MOVES
	z := formerWorld(t)
	z.depart()
	no = preview(z, "ops-2", api.AttributionStatement{LineNo: 1, ShipmentID: "SH-C", Voucher: "CLAIM-1"})
	got = z.impact(le("fin.F3", f3Moves))
	if len(got.Checks) != 1 || got.Checks[0].Rule != "F3" || got.Checks[0].Now != "voucher needed" || got.Checks[0].After != "no voucher needed" || got.Checks[0].PreviewNo != no {
		t.Fatalf("F3: %+v", got.Checks)
	}
	// an attribution that moves an amount needs its voucher either way
	if got = u.impact(le("fin.F3", f3Moves)); len(got.Checks) != 0 {
		t.Fatalf("F3 on an attribution that moves an amount: %+v", got.Checks)
	}
	// F2 with nothing to check: a preview that holds no attribution and no rework
	w := previewWorld(t)
	w.must(w.preview("preview-plain", 4, nil, "SH-C"))
	if got = w.impact(le("fin.F2", "SUBMITTER_AND_PREVIEWER")); len(got.Checks) != 0 || len(got.StalePreviews) != 1 {
		t.Fatalf("a preview with nothing to check: %+v", got)
	}
}

// The impact changes nothing: the pool, the change sets, the events and the commands received are what they were, and
// asking for a proposal proposes nothing.
func TestTheImpactChangesNothingOnPostgres(t *testing.T) {
	u := formerWorld(t)
	u.depart()
	u.attribute("attribute", 6, statementOf("C", 5_000, 10_000, api.PartyPending))
	u.must(u.preview("preview-1", 8, nil))
	state := func() []any {
		changes, err := u.module.ListFinSettingsChanges(u.ctx, api.ListFinSettingsChanges{TenantID: u.tenant})
		if err != nil {
			t.Fatal(err)
		}
		return []any{u.revision(), len(changes), len(u.outboxTypes(t)), u.settingsVersion(), len(u.ap.pool(u.pool).Previews), u.jobProfit("JOB-C").ProfitVersion}
	}
	was := state()
	for _, entries := range [][]api.FinSettingEntry{{le("fin.F7", "BLOCKS")}, {le("fin.F6", "INCLUDED_MARKED")}, {le("fin.F9", "RECONFIRM")}, {le("fin.F4", f4One)},
		{le("fin.F8", f8Dflt), le("fin.F3", f3Moves)}, {le("fin.F1", "UNRESTRICTED")}} {
		u.impact(entries...)
	}
	if now := state(); !slices.Equal(was, now) {
		t.Fatalf("the impact changed something: %v, then %v", was, now)
	}
}

// A change set that is proposed, or approved with a time to come, is named by its ID and answers as its entries do; one
// in force, cancelled or unknown, and neither or both of the two ways, are refused with the way out.
func TestTheImpactOfAChangeSetOnPostgres(t *testing.T) {
	u := formerWorld(t)
	u.depart()
	propose := func(id string, effective time.Time, entries ...api.FinSettingEntry) {
		u.must(u.module.ProposeFinSettingsChange(u.ctx, api.ProposeFinSettingsChange{CommandID: id + "-propose", TenantID: u.tenant, ActorID: "ann", ChangeID: id,
			Entries: entries, Reason: "because " + id, EffectiveAt: effective}))
	}
	byID := func(id string) (api.FinSettingsChangeImpact, error) {
		return u.module.GetFinSettingsChangeImpact(u.ctx, api.GetFinSettingsChangeImpact{TenantID: u.tenant, ActorID: "ann", ChangeID: id})
	}
	propose("inforce", time0, le("fin.F9", "KEEP_RATIO"))      // a tightening: in force at once
	propose("pending", time0, le("fin.F6", "INCLUDED_MARKED")) // a loosening: waits for bob
	got, err := byID("pending")
	want := u.impact(le("fin.F6", "INCLUDED_MARKED"))
	if err != nil || got.ChangeID != "pending" || len(got.Jobs) != 1 || !reflect.DeepEqual(got.Jobs, want.Jobs) || !reflect.DeepEqual(got.Entities, want.Entities) ||
		got.VersionNow != 1 || got.VersionAfter != 2 {
		t.Fatalf("a proposed change set: %+v %v, as its entries %+v", got, err, want)
	}
	// approved with a time to come: not in force yet
	propose("later", time.Now().Add(time.Hour), le("fin.F7", "BLOCKS"))
	if got, err = byID("later"); err != nil || len(got.Entities) != 1 {
		t.Fatalf("a change set that takes effect later: %+v %v", got, err)
	}
	for name, c := range map[string]struct {
		ask  api.GetFinSettingsChangeImpact
		want string
	}{
		"one in force":   {api.GetFinSettingsChangeImpact{ChangeID: "inforce"}, ""},
		"an unknown one": {api.GetFinSettingsChangeImpact{ChangeID: "nothing"}, "there is no change set"},
		"neither":        {api.GetFinSettingsChangeImpact{}, "name the change set"},
		"both":           {api.GetFinSettingsChangeImpact{ChangeID: "later", Entries: []api.FinSettingEntry{le("fin.F7", "BLOCKS")}}, "not both"},
	} {
		c.ask.TenantID, c.ask.ActorID = u.tenant, "ann"
		_, err = u.module.GetFinSettingsChangeImpact(u.ctx, c.ask)
		if !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// cancelled: not told either
	u.must(u.module.CancelFinSettingsChange(u.ctx, api.CancelFinSettingsChange{CommandID: "later-cancel", TenantID: u.tenant, ActorID: "ann", ChangeID: "later", Reason: "no"}))
	if _, err = byID("later"); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("a cancelled change set: %v", err)
	}
}

// onlyActions allows the actions it names.
type onlyActions []string

func (o onlyActions) AuthorizeFinAction(_ context.Context, _, actor, action, _ string) error {
	if actor == "intruder" || !slices.Contains(o, action) {
		return errors.New("forbidden")
	}
	return nil
}

// The impact is told to a holder of ReadProfit, of ConfigureSettings or of ApproveSettings, any one of them, and to no
// other; an actor must be named.
func TestTheImpactIsToldToThreePermissionsOnPostgres(t *testing.T) {
	u := previewWorld(t)
	ask := api.GetFinSettingsChangeImpact{TenantID: u.tenant, ActorID: "ann", Entries: []api.FinSettingEntry{le("fin.F7", "BLOCKS")}}
	for _, action := range []string{api.ActionReadProfit, api.ActionConfigureSettings, api.ActionApproveSettings} {
		m, err := finmodule.New(u.db, u.w, u.w, u.w, u.w, u.w, onlyActions{action})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = m.GetFinSettingsChangeImpact(u.ctx, ask); err != nil {
			t.Errorf("a holder of %s: %v", action, err)
		}
	}
	m, err := finmodule.New(u.db, u.w, u.w, u.w, u.w, u.w, onlyActions{api.ActionRequestCostReview})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.GetFinSettingsChangeImpact(u.ctx, ask); !errors.Is(err, api.ErrUnauthorized) {
		t.Errorf("a holder of none: %v", err)
	}
	ask.ActorID = ""
	if _, err = u.module.GetFinSettingsChangeImpact(u.ctx, ask); !errors.Is(err, api.ErrUnauthenticated) {
		t.Errorf("no actor: %v", err)
	}
}

// The catalogue lists E1 to E4 as reference data, each with its name, what it governs, where it is changed and whether
// a change needs approval, and its table includes them.
func TestTheCatalogueListsTheFinancePolicyOnPostgres(t *testing.T) {
	f := newFixture(t)
	cat, err := f.module.GetFinSettingsCatalogue(context.Background(), api.GetFinSettingsCatalogue{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Items) != 8 || len(cat.Reference) != 4 {
		t.Fatalf("the catalogue: %d items, %d reference items", len(cat.Items), len(cat.Reference))
	}
	approval := map[string]bool{"fin.E1": true, "fin.E2": false, "fin.E3": false, "fin.E4": false}
	// the design's order: E1 the cost variance threshold, E2 the tax treatment of the charge codes, E3 the exchange
	// rates and E4 the invoice number series
	for key, title := range map[string]string{"fin.E1": "Cost variance threshold", "fin.E2": "Tax treatment of charge codes", "fin.E3": "Exchange rates",
		"fin.E4": "Invoice number series"} {
		found := false
		for _, r := range cat.Reference {
			found = found || (r.Key == key && r.TitleEN == title)
		}
		if !found {
			t.Errorf("%s is not %q", key, title)
		}
	}
	for i, r := range cat.Reference {
		key := "fin.E" + string(rune('1'+i))
		if r.Key != key || r.TitleEN == "" || r.TitleZH == "" || r.Governs == "" || r.ChangedBy == "" || r.ApprovalNote == "" || r.NeedsApproval != approval[key] {
			t.Errorf("%s: %+v", key, r)
		}
		if !strings.Contains(cat.Table, "| "+key+" |") {
			t.Errorf("the table lacks %s:\n%s", key, cat.Table)
		}
	}
	for _, part := range []string{"SetCostVarianceThreshold", "ApproveCostVarianceThreshold", "RecordExchangeRate", "SetChargeTaxTreatment", "RegisterInvoiceSeries"} {
		if !strings.Contains(cat.Table, part) {
			t.Errorf("the table does not name %s", part)
		}
	}
}
