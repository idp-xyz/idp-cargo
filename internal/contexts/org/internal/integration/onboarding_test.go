//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	orgmodule "go.idp.xyz/idp-cargo/internal/contexts/org"
	"go.idp.xyz/idp-cargo/internal/contexts/org/api"
	scope "go.idp.xyz/idp-cargo/internal/foundation/bento"
)

// V2.102, migration org/000012: customer onboarding, the record of a consultant's proposal, the owner's confirmation and
// each module's change set and state.

// onboardingAuth grants the actions by name: ann proposes, olga confirms, dan does both, olga and dan record a module (as the
// platform does for the confirmer), and nobody else does anything.
type onboardingAuth struct{ testAuthorizer }

func (onboardingAuth) AuthorizePartyAction(_ context.Context, _, actor, action, _ string) error {
	grants := map[string][]string{"ann": {api.ActionProposeOnboarding}, "olga": {api.ActionConfirmOnboarding, api.ActionRecordOnboardingModule},
		"dan": {api.ActionProposeOnboarding, api.ActionConfirmOnboarding, api.ActionRecordOnboardingModule}}
	for _, a := range grants[actor] {
		if a == action {
			return nil
		}
	}
	return errors.New("test policy: " + actor + " may not " + action)
}

type onboardingWorld struct {
	t      *testing.T
	module *orgmodule.Module
	tenant string
	n      int
	raw    func(stmts ...string) error
}

func newOnboardingWorld(t *testing.T) *onboardingWorld {
	t.Helper()
	db, _ := testDB(t)
	m, err := orgmodule.New(db, onboardingAuth{})
	if err != nil {
		t.Fatal(err)
	}
	w := &onboardingWorld{t: t, module: m, tenant: "cargo-onboarding-" + uniqueSuffix()}
	w.raw = func(stmts ...string) error {
		return (scope.TenantTransactor{DB: db, Tenant: w.tenant}).WithinTransaction(context.Background(), func(txCtx context.Context) error {
			e, err := db.RequireExecutor(txCtx)
			if err != nil {
				return err
			}
			for _, s := range stmts {
				if _, err := e.Exec(txCtx, s, w.tenant); err != nil {
					return err
				}
			}
			return nil
		})
	}
	return w
}

func (w *onboardingWorld) command() string {
	w.n++
	return fmt.Sprintf("cmd-%d", w.n)
}

const riskText = "The customer accepts that the STRICT profile blocks closing a pool while an attribution is pending."

// plan is a plan as the platform's onboarding resolves it: ORG keeps it and does not read it.
func plan(entries ...api.OnboardingEntry) map[string]api.ModulePlan {
	if len(entries) == 0 {
		entries = []api.OnboardingEntry{{ScopeKind: "TENANT", Key: "fin.F7", Value: "BLOCKS"}}
	}
	return map[string]api.ModulePlan{"FIN": {CatalogueVersion: "fin-v1", Entries: entries}, "SEA": {CatalogueVersion: "sea-v1", Entries: []api.OnboardingEntry{{ScopeKind: "TENANT", Key: "sea.S1", Value: "WITH_REASON"}}}}
}

func (w *onboardingWorld) propose(id, actor, profile string, staff int, single bool) (api.OnboardingReceipt, error) {
	return w.module.ProposeOnboarding(context.Background(), api.ProposeOnboarding{CommandID: "p-" + id, TenantID: w.tenant, ActorID: actor, OnboardingID: id, Profile: profile,
		Plan: plan(), RiskStatement: riskText, FinanceStaff: staff, SingleAdministrator: single})
}

// confirm is the owner's confirmation; a proposal that declares a single administrator is confirmed naming dan.
func (w *onboardingWorld) confirm(command, id, actor, text string) (api.OnboardingReceipt, error) {
	admin := ""
	if o, err := w.module.GetOnboarding(context.Background(), api.GetOnboarding{TenantID: w.tenant, OnboardingID: id}); err == nil && o.SingleAdministrator {
		admin = "dan"
	}
	return w.confirmNaming(command, id, actor, text, admin)
}

func (w *onboardingWorld) confirmNaming(command, id, actor, text, admin string) (api.OnboardingReceipt, error) {
	return w.module.ConfirmOnboarding(context.Background(), api.ConfirmOnboarding{CommandID: command, TenantID: w.tenant, ActorID: actor, OnboardingID: id, AcknowledgedText: text, AdministratorID: admin})
}

func (w *onboardingWorld) record(id, actor, module, state string) (api.OnboardingReceipt, error) {
	return w.module.RecordOnboardingModule(context.Background(), api.RecordOnboardingModule{CommandID: w.command(), TenantID: w.tenant, ActorID: actor, OnboardingID: id,
		Module: module, ChangeID: "onb-" + id + "-" + strings.ToLower(module), State: state})
}

func (w *onboardingWorld) get(id string) api.Onboarding {
	w.t.Helper()
	o, err := w.module.GetOnboarding(context.Background(), api.GetOnboarding{TenantID: w.tenant, OnboardingID: id})
	if err != nil {
		w.t.Fatal(err)
	}
	return o
}

func said(t *testing.T, what string, err error, kind error, contains ...string) {
	t.Helper()
	if !errors.Is(err, kind) {
		t.Fatalf("%s: %v, want %v", what, err, kind)
	}
	for _, c := range contains {
		if !strings.Contains(err.Error(), c) {
			t.Fatalf("%s: %v does not say %q", what, err, c)
		}
	}
}

func TestOnboardingIsProposedConfirmedAndRecordedPerModule(t *testing.T) {
	w := newOnboardingWorld(t)
	// permissions: a proposal is the consultant's, a confirmation the owner's; neither by anyone else
	_, err := w.propose("ob-1", "olga", "STRICT", 2, false)
	said(t, "the owner proposing", err, api.ErrUnauthorized)
	_, err = w.propose("ob-1", "nobody", "STRICT", 2, false)
	said(t, "someone with neither permission proposing", err, api.ErrUnauthorized)

	// the STRICT notice, and a proposal that is not refused for it
	r, err := w.propose("ob-1", "ann", "STRICT", 2, true)
	if err != nil || r.State != "PROPOSED" || r.NewRevision != 1 || r.OnboardingID != "ob-1" || !strings.Contains(r.Notice, "at least 3 finance staff") || !strings.Contains(r.Notice, "declared 2") {
		t.Fatalf("a STRICT proposal with 2 finance staff: %+v, %v", r, err)
	}
	if again, err := w.propose("ob-1", "ann", "STRICT", 2, true); err != nil || again != r {
		t.Fatalf("a replay: %+v, %v", again, err)
	}
	for name, c := range map[string]struct {
		profile string
		staff   int
	}{"STRICT with 3": {"STRICT", 3}, "STRICT with 40": {"STRICT", 40}, "LEAN with 1": {"LEAN", 1}, "STANDARD with 1": {"STANDARD", 1}} {
		if r, err = w.propose("ob-"+strings.ReplaceAll(name, " ", "-"), "ann", c.profile, c.staff, false); err != nil || r.Notice != "" {
			t.Errorf("%s: %+v, %v", name, r, err)
		}
	}
	// the same onboarding with another content, or by another command, is not a replay
	_, err = w.module.ProposeOnboarding(context.Background(), api.ProposeOnboarding{CommandID: "p-ob-1", TenantID: w.tenant, ActorID: "ann", OnboardingID: "ob-1", Profile: "STRICT",
		Plan: plan(), RiskStatement: riskText, FinanceStaff: 5})
	said(t, "another content", err, api.ErrIdempotencyConflict, "propose with another onboarding id")
	_, err = w.module.ProposeOnboarding(context.Background(), api.ProposeOnboarding{CommandID: "p-other", TenantID: w.tenant, ActorID: "ann", OnboardingID: "ob-1", Profile: "STRICT",
		Plan: plan(), RiskStatement: riskText, FinanceStaff: 2, SingleAdministrator: true})
	said(t, "another command for the same content", err, api.ErrIdempotencyConflict)

	// what a proposal must be
	for name, c := range map[string]api.ProposeOnboarding{
		"an id that is no identifier": {OnboardingID: "-x", Profile: "LEAN", RiskStatement: "r", FinanceStaff: 1},
		"another profile":             {OnboardingID: "o", Profile: "FANCY", RiskStatement: "r", FinanceStaff: 1},
		"no risk statement":           {OnboardingID: "o", Profile: "LEAN", RiskStatement: "  ", FinanceStaff: 1},
		"too long a risk statement":   {OnboardingID: "o", Profile: "LEAN", RiskStatement: strings.Repeat("x", 2001), FinanceStaff: 1},
		"no finance staff":            {OnboardingID: "o", Profile: "LEAN", RiskStatement: "r", FinanceStaff: 0},
		"too many finance staff":      {OnboardingID: "o", Profile: "LEAN", RiskStatement: "r", FinanceStaff: 10001},
		"overrides of another module": {OnboardingID: "o", Profile: "LEAN", RiskStatement: "r", FinanceStaff: 1, Overrides: map[string][]api.OnboardingEntry{"AIR": {{ScopeKind: "TENANT", Key: "a", Value: "b"}}}},
		"an override of no scope":     {OnboardingID: "o", Profile: "LEAN", RiskStatement: "r", FinanceStaff: 1, Overrides: map[string][]api.OnboardingEntry{"FIN": {{ScopeKind: "BRANCH", Key: "a", Value: "b"}}}},
		"a company scope with an id":  {OnboardingID: "o", Profile: "LEAN", RiskStatement: "r", FinanceStaff: 1, Overrides: map[string][]api.OnboardingEntry{"FIN": {{ScopeKind: "TENANT", ScopeID: "x", Key: "a", Value: "b"}}}},
		"an override without a value": {OnboardingID: "o", Profile: "LEAN", RiskStatement: "r", FinanceStaff: 1, Overrides: map[string][]api.OnboardingEntry{"FIN": {{ScopeKind: "TENANT", Key: "a"}}}},
		"a threshold without currency": {OnboardingID: "o", Profile: "LEAN", RiskStatement: "r", FinanceStaff: 1, Overrides: map[string][]api.OnboardingEntry{
			"FIN": {{ScopeKind: "LEGAL_ENTITY", ScopeID: "LE", Key: "a", Value: "b", ThresholdMinor: new(int64)}}}},
	} {
		c.CommandID, c.TenantID, c.ActorID = w.command(), w.tenant, "ann"
		if c.Plan == nil {
			c.Plan = plan()
		}
		_, err = w.module.ProposeOnboarding(context.Background(), c)
		said(t, name, err, api.ErrRejected, "ORG_INVALID_ONBOARDING")
	}

	// the confirmation: by the owner, who is not the proposer, acknowledging the statement as proposed
	_, err = w.confirm(w.command(), "ob-1", "ann", riskText)
	said(t, "the consultant confirming", err, api.ErrUnauthorized)
	_, err = w.confirm(w.command(), "ob-1", "nobody", riskText)
	said(t, "someone without the permission confirming", err, api.ErrUnauthorized)
	_, err = w.propose("ob-both", "dan", "LEAN", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.confirm(w.command(), "ob-both", "dan", riskText)
	said(t, "a holder of both permissions confirming their own proposal", err, api.ErrRejected, "may not confirm it", "appointed owner")
	_, err = w.confirm(w.command(), "ob-404", "olga", riskText)
	said(t, "an unknown onboarding", err, api.ErrNotFound, "ListOnboardings")
	_, err = w.confirm(w.command(), "ob-1", "olga", riskText+" ")
	said(t, "another text", err, api.ErrRejected, "not the risk statement proposed")
	if w.get("ob-1").State != "PROPOSED" {
		t.Fatal("a refused confirmation changed the onboarding")
	}
	c1, err := w.confirm("conf-1", "ob-1", "olga", riskText)
	if err != nil || c1.State != "CONFIRMED" || c1.NewRevision != 2 || !strings.Contains(c1.Notice, "at least 3 finance staff") {
		t.Fatalf("the confirmation: %+v, %v", c1, err)
	}
	if again, err := w.confirm("conf-1", "ob-1", "olga", riskText); err != nil || again != c1 {
		t.Fatalf("a retry: %+v, %v", again, err)
	}
	if again, err := w.confirm("conf-retry", "ob-1", "olga", riskText); err != nil || again.State != "CONFIRMED" || again.NewRevision != 2 {
		t.Fatalf("a retry under another command id: %+v, %v", again, err)
	}
	_, err = w.confirm(w.command(), "ob-1", "dan", riskText)
	said(t, "a second confirmer", err, api.ErrIdempotencyConflict, "confirmed by olga already")
	o := w.get("ob-1")
	if o.ConfirmedBy != "olga" || o.AcknowledgedText != riskText || o.ProposedBy != "ann" || !o.SingleAdministrator || o.FinanceStaff != 2 || o.Profile != "STRICT" ||
		o.ConfirmedAt.Before(o.ProposedAt) || len(o.Modules) != 2 || o.Modules[0].Module != "FIN" || o.Modules[1].Module != "SEA" || o.Modules[0].State != "NOT_STARTED" {
		t.Fatalf("the onboarding: %+v", o)
	}

	// the modules: recorded by the confirmer, PROPOSED then APPROVED, once each, with the change set derived from the onboarding
	_, err = w.record("ob-both", "olga", "FIN", "PROPOSED")
	said(t, "a module of an onboarding not confirmed", err, api.ErrRejected, "not confirmed")
	_, err = w.record("ob-1", "ann", "FIN", "PROPOSED")
	said(t, "the consultant recording a module", err, api.ErrUnauthorized)
	_, err = w.record("ob-1", "dan", "FIN", "PROPOSED")
	said(t, "someone else who may confirm recording a module", err, api.ErrRejected, "recorded by its confirmer, olga")
	_, err = w.module.RecordOnboardingModule(context.Background(), api.RecordOnboardingModule{CommandID: w.command(), TenantID: w.tenant, ActorID: "olga", OnboardingID: "ob-1",
		Module: "FIN", ChangeID: "another-change", State: "PROPOSED"})
	said(t, "a change set that is not the derived one", err, api.ErrRejected, "onb-ob-1-fin")
	_, err = w.record("ob-1", "olga", "AIR", "PROPOSED")
	said(t, "another module", err, api.ErrRejected)
	_, err = w.record("ob-1", "olga", "FIN", "DONE")
	said(t, "another state", err, api.ErrRejected)
	if _, err = w.record("ob-1", "olga", "FIN", "APPROVED"); err == nil || !strings.Contains(err.Error(), "approved before it is proposed") {
		t.Fatalf("approved before proposed: %v", err)
	}
	r1, err := w.record("ob-1", "olga", "FIN", "PROPOSED")
	if err != nil || r1.State != "CONFIRMED" || r1.NewRevision != 3 {
		t.Fatalf("FIN proposed: %+v, %v", r1, err)
	}
	if again, err := w.record("ob-1", "olga", "FIN", "PROPOSED"); err != nil || again.NewRevision != 3 {
		t.Fatalf("a record again: %+v, %v", again, err)
	}
	if m := w.get("ob-1").Modules[0]; m.State != "PROPOSED" || m.ChangeID != "onb-ob-1-fin" {
		t.Fatalf("FIN: %+v", m)
	}
	for _, step := range [][2]string{{"FIN", "APPROVED"}, {"SEA", "PROPOSED"}} {
		if r, err = w.record("ob-1", "olga", step[0], step[1]); err != nil || r.State != "CONFIRMED" {
			t.Fatalf("%v: %+v, %v", step, r, err)
		}
	}
	if r, err = w.record("ob-1", "olga", "SEA", "APPROVED"); err != nil || r.State != "COMPLETE" || r.NewRevision != 6 {
		t.Fatalf("SEA approved: %+v, %v", r, err)
	}
	if o = w.get("ob-1"); o.State != "COMPLETE" || o.Modules[0].State != "APPROVED" || o.Modules[1].State != "APPROVED" || o.Modules[1].ChangeID != "onb-ob-1-sea" {
		t.Fatalf("the onboarding: %+v", o)
	}

	// the queries: the tenant's onboardings in the order proposed; another tenant's none
	list, err := w.module.ListOnboardings(context.Background(), api.ListOnboardings{TenantID: w.tenant})
	if err != nil || len(list) != 6 || list[0].OnboardingID != "ob-1" {
		t.Fatalf("the list: %d, %v", len(list), err)
	}
	other, err := w.module.ListOnboardings(context.Background(), api.ListOnboardings{TenantID: w.tenant + "-other"})
	if err != nil || len(other) != 0 {
		t.Fatalf("another tenant's list: %+v, %v", other, err)
	}
	_, err = w.module.GetOnboarding(context.Background(), api.GetOnboarding{TenantID: w.tenant + "-other", OnboardingID: "ob-1"})
	said(t, "another tenant's onboarding", err, api.ErrNotFound)
}

// What the database refuses past ORG's commands on the application role, each by its name.
func TestOnboardingDatabaseRulesRefuseRawWrites(t *testing.T) {
	w := newOnboardingWorld(t)
	const hash = "0000000000000000000000000000000000000000000000000000000000000000"
	const entry = `{"ScopeKind":"TENANT","ScopeID":"","Key":"fin.F7","Value":"BLOCKS"}`
	const goodPlan = `{"FIN":{"CatalogueVersion":"v1","Entries":[` + entry + `]},"SEA":{"CatalogueVersion":"v1","Entries":[` + entry + `]}}`
	many := func(n int) string { return strings.TrimSuffix(strings.Repeat(entry+",", n), ",") }
	proposal := func(id, by, cmd, extra string) string { // extra overrides a column: "profile='FANCY'"
		cols := map[string]string{"profile": "'LEAN'", "overrides": "'{}'", "risk_statement": "'the statement'", "finance_staff": "2", "single_administrator": "false",
			"proposed_by": "'" + by + "'", "command_id": "'" + cmd + "'", "onboarding_id": "'" + id + "'", "plan": "'" + goodPlan + "'"}
		if extra != "" {
			kv := strings.SplitN(extra, "=", 2)
			cols[kv[0]] = kv[1]
		}
		return fmt.Sprintf(`INSERT INTO cargo_org.onboarding (tenant_id,onboarding_id,command_id,request_hash,profile,overrides,plan,risk_statement,finance_staff,single_administrator,
        proposed_by,proposed_at) VALUES ($1,%s,%s,'%s',%s,%s,%s,%s,%s,%s,%s,now())`, cols["onboarding_id"], cols["command_id"], hash, cols["profile"], cols["overrides"], cols["plan"],
			cols["risk_statement"], cols["finance_staff"], cols["single_administrator"], cols["proposed_by"])
	}
	// confirmationBy confirms with the proposal's own flag and the administrator named ("NULL" or a quoted id)
	confirmationBy := func(id, proposer, confirmer, text, at, administrator string) string {
		return fmt.Sprintf(`INSERT INTO cargo_org.onboarding_confirmation (tenant_id,onboarding_id,command_id,proposed_by,proposed_at,confirmed_by,confirmed_at,acknowledged_text,
        single_administrator,administrator) SELECT tenant_id,onboarding_id,'c-'||onboarding_id,'%s',proposed_at,'%s',%s,'%s',single_administrator,%s
        FROM cargo_org.onboarding WHERE tenant_id=$1 AND onboarding_id='%s'`, proposer, confirmer, at, text, administrator, id)
	}
	confirmation := func(id, proposer, confirmer, text, at string) string {
		return confirmationBy(id, proposer, confirmer, text, at, "NULL")
	}
	module := func(id, by, mod, state, change, at string) string {
		return fmt.Sprintf(`INSERT INTO cargo_org.onboarding_module (tenant_id,onboarding_id,module,state,change_id,recorded_by,confirmed_at,recorded_at)
        SELECT tenant_id,onboarding_id,'%s','%s','%s','%s',confirmed_at,%s FROM cargo_org.onboarding_confirmation WHERE tenant_id=$1 AND onboarding_id='%s'`, mod, state, change, by, at, id)
	}
	type rule struct {
		name       string
		stmts      []string
		state      string
		constraint string
	}
	long := strings.Repeat("x", 2001)
	for _, c := range []rule{
		{"another profile", []string{proposal("a", "ann", "c1", "profile='FANCY'")}, "23514", "onboarding_profile"},
		{"overrides that are not an object", []string{proposal("a", "ann", "c1", "overrides='[]'")}, "23514", "onboarding_overrides"},
		{"overrides of another module", []string{proposal("a", "ann", "c1", `overrides='{"AIR":[]}'`)}, "23514", "onboarding_overrides"},
		{"a plan that is not an object", []string{proposal("a", "ann", "c1", `plan='[]'`)}, "23514", "onboarding_plan"},
		{"a plan without SEA", []string{proposal("a", "ann", "c1", `plan='{"FIN":{"CatalogueVersion":"v","Entries":[`+entry+`]}}'`)}, "23514", "onboarding_plan"},
		{"a plan of another module", []string{proposal("a", "ann", "c1", `plan='{"AIR":{},"FIN":{"CatalogueVersion":"v","Entries":[`+entry+`]},"SEA":{"CatalogueVersion":"v","Entries":[`+entry+`]}}'`)},
			"23514", "onboarding_plan"},
		{"a plan with no catalogue version", []string{proposal("a", "ann", "c1", `plan='{"FIN":{"CatalogueVersion":" ","Entries":[`+entry+`]},"SEA":{"CatalogueVersion":"v","Entries":[`+entry+`]}}'`)},
			"23514", "onboarding_plan"},
		{"a plan with no entries", []string{proposal("a", "ann", "c1", `plan='{"FIN":{"CatalogueVersion":"v","Entries":[]},"SEA":{"CatalogueVersion":"v","Entries":[`+entry+`]}}'`)},
			"23514", "onboarding_plan"},
		{"a plan of 65 entries", []string{proposal("a", "ann", "c1", `plan='{"FIN":{"CatalogueVersion":"v","Entries":[`+many(65)+`]},"SEA":{"CatalogueVersion":"v","Entries":[`+entry+`]}}'`)},
			"23514", "onboarding_plan"},
		{"a blank risk statement", []string{proposal("a", "ann", "c1", "risk_statement='  '")}, "23514", "onboarding_risk_statement"},
		{"a risk statement of 2001 characters", []string{proposal("a", "ann", "c1", "risk_statement='"+long+"'")}, "23514", "onboarding_risk_statement"},
		{"no finance staff", []string{proposal("a", "ann", "c1", "finance_staff=0")}, "23514", "onboarding_finance_staff"},
		{"10001 finance staff", []string{proposal("a", "ann", "c1", "finance_staff=10001")}, "23514", "onboarding_finance_staff"},
		{"an id that is no identifier", []string{proposal("-a", "ann", "c1", "")}, "23514", "onboarding_id_shape"},
		{"an id of 101 characters", []string{proposal(strings.Repeat("a", 101), "ann", "c1", "")}, "23514", "onboarding_id_shape"},
		{"a command id used twice", []string{proposal("a", "ann", "c1", ""), proposal("b", "ann", "c1", "")}, "23505", "onboarding_command_once"},
		{"an onboarding twice", []string{proposal("a", "ann", "c1", ""), proposal("a", "ann", "c2", "")}, "23505", "onboarding_pkey"},
		{"a confirmation by the proposer", []string{proposal("a", "ann", "c1", ""), confirmation("a", "ann", "ann", "the statement", "proposed_at")}, "23514", "onboarding_confirmer_not_proposer"},
		{"a confirmation that names another proposer", []string{proposal("a", "ann", "c1", ""), confirmation("a", "zed", "olga", "the statement", "proposed_at")}, "23503",
			"onboarding_confirmation_matches_proposal"},
		{"a confirmation of another text", []string{proposal("a", "ann", "c1", ""), confirmation("a", "ann", "olga", "the statement ", "proposed_at")}, "23503", "onboarding_confirmation_matches_proposal"},
		{"an administrator named when the proposal declares none", []string{proposal("a", "ann", "c1", ""), confirmationBy("a", "ann", "olga", "the statement", "proposed_at", "'dan'")}, "23514",
			"onboarding_administrator_declared"},
		{"no administrator named when the proposal declares one", []string{proposal("a", "ann", "c1", "single_administrator=true"), confirmationBy("a", "ann", "olga", "the statement", "proposed_at", "NULL")},
			"23514", "onboarding_administrator_declared"},
		{"a flag that differs from the proposal's", []string{proposal("a", "ann", "c1", ""), strings.Replace(confirmationBy("a", "ann", "olga", "the statement", "proposed_at", "'dan'"),
			"single_administrator,'dan'", "true,'dan'", 1)}, "23503", "onboarding_confirmation_matches_proposal"},
		{"the consultant as the administrator", []string{proposal("a", "ann", "c1", "single_administrator=true"), confirmationBy("a", "ann", "olga", "the statement", "proposed_at", "'ann'")},
			"23514", "onboarding_administrator_not_proposer"},
		{"an administrator with spaces around the id", []string{proposal("a", "ann", "c1", "single_administrator=true"), confirmationBy("a", "ann", "olga", "the statement", "proposed_at", "' dan'")},
			"23514", "onboarding_administrator_length"},
		{"a confirmation before the proposal", []string{proposal("a", "ann", "c1", ""), confirmation("a", "ann", "olga", "the statement", "proposed_at - interval '1 second'")}, "23514",
			"onboarding_confirmation_after_proposal"},
		{"a second confirmation", []string{proposal("a", "ann", "c1", ""), confirmation("a", "ann", "olga", "the statement", "proposed_at"),
			strings.Replace(confirmation("a", "ann", "olga", "the statement", "proposed_at"), "'c-'||onboarding_id", "'c-2'", 1)}, "23505", "onboarding_confirmation_pkey"},
		{"a module of an onboarding not confirmed", []string{proposal("a", "ann", "c1", ""), `INSERT INTO cargo_org.onboarding_module (tenant_id,onboarding_id,module,state,change_id,
          recorded_by,confirmed_at,recorded_at) VALUES ($1,'a','FIN','PROPOSED','onb-a-fin','olga',now(),now())`}, "23503", "onboarding_module_by_confirmer"},
		{"a module recorded by someone but the confirmer", []string{proposal("a", "ann", "c1", ""), confirmation("a", "ann", "olga", "the statement", "proposed_at"),
			module("a", "dan", "FIN", "PROPOSED", "onb-a-fin", "now()")}, "23503", "onboarding_module_by_confirmer"},
		{"a module recorded before the confirmation", []string{proposal("a", "ann", "c1", ""), confirmation("a", "ann", "olga", "the statement", "proposed_at"),
			module("a", "olga", "FIN", "PROPOSED", "onb-a-fin", "confirmed_at - interval '1 second'")}, "23514", "onboarding_module_after_confirmation"},
		{"a change set that is not derived", []string{proposal("a", "ann", "c1", ""), confirmation("a", "ann", "olga", "the statement", "proposed_at"),
			module("a", "olga", "FIN", "PROPOSED", "onb-a-sea", "now()")}, "23514", "onboarding_module_change_derived"},
		{"a second change set for a module", []string{proposal("a", "ann", "c1", ""), confirmation("a", "ann", "olga", "the statement", "proposed_at"),
			module("a", "olga", "FIN", "PROPOSED", "onb-a-fin", "now()"), module("a", "olga", "FIN", "PROPOSED", "onb-a-fin", "now()")}, "23505", "onboarding_module_pkey"},
		{"another module", []string{proposal("a", "ann", "c1", ""), confirmation("a", "ann", "olga", "the statement", "proposed_at"),
			module("a", "olga", "AIR", "PROPOSED", "onb-a-air", "now()")}, "23514", "onboarding_module_known"},
		{"another state", []string{proposal("a", "ann", "c1", ""), confirmation("a", "ann", "olga", "the statement", "proposed_at"),
			module("a", "olga", "FIN", "DONE", "onb-a-fin", "now()")}, "23514", "onboarding_module_state"},
		{"approved before proposed", []string{proposal("a", "ann", "c1", ""), confirmation("a", "ann", "olga", "the statement", "proposed_at"),
			module("a", "olga", "FIN", "APPROVED", "onb-a-fin", "now()")}, "23514", "onboarding_module_approved_after_proposed"},
		{"another tenant's onboarding", []string{`INSERT INTO cargo_org.onboarding (tenant_id,onboarding_id,command_id,request_hash,profile,overrides,risk_statement,finance_staff,
          plan,single_administrator,proposed_by,proposed_at) VALUES ($1 || '-other','a','c1','` + hash + `','LEAN','{}','s',1,'` + goodPlan + `',false,'ann',now())`}, "42501", ""},
	} {
		err := w.raw(c.stmts...)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != c.state || (c.constraint != "" && pgErr.ConstraintName != c.constraint) {
			t.Errorf("%s: %v, want %s %s", c.name, err, c.state, c.constraint)
		}
	}
	// written as ORG writes it, the same rows are accepted, so that what was refused above was the rule
	if err := w.raw(proposal("ok", "ann", "c-ok", ""), confirmation("ok", "ann", "olga", "the statement", "proposed_at"), module("ok", "olga", "FIN", "PROPOSED", "onb-ok-fin", "now()"),
		module("ok", "olga", "FIN", "APPROVED", "onb-ok-fin", "now()")); err != nil {
		t.Fatalf("an onboarding written as ORG writes it: %v", err)
	}
	if err := w.raw(proposal("ok2", "ann", "c-ok2", "single_administrator=true"), confirmationBy("ok2", "ann", "olga", "the statement", "proposed_at", "'olga'"),
		proposal("ok3", "ann", "c-ok3", `plan='{"FIN":{"CatalogueVersion":"v","Entries":[`+many(64)+`]},"SEA":{"CatalogueVersion":"v","Entries":[`+many(64)+`]}}'`)); err != nil {
		t.Fatalf("an owner who names themselves as the administrator, and plans of 64 entries: %v", err)
	}
	// the rows are only inserted
	for _, table := range []string{"onboarding", "onboarding_confirmation", "onboarding_module"} {
		for _, stmt := range []string{`UPDATE cargo_org.` + table + ` SET tenant_id = tenant_id WHERE tenant_id = $1`, `DELETE FROM cargo_org.` + table + ` WHERE tenant_id = $1`} {
			err := w.raw(stmt)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || (pgErr.Code != "23001" && pgErr.Code != "42501") {
				t.Errorf("%s: %v", stmt, err)
			}
		}
	}
	// row security is enabled and forced on each table
	db, _ := testDB(t)
	for _, table := range []string{"onboarding", "onboarding_confirmation", "onboarding_module"} {
		var enabled, forced bool
		q, err := db.ReadExecutor(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err = q.QueryRow(context.Background(), `SELECT c.relrowsecurity, c.relforcerowsecurity FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
          WHERE n.nspname = 'cargo_org' AND c.relname = $1`, table).Scan(&enabled, &forced); err != nil || !enabled || !forced {
			t.Errorf("%s: row security enabled %v, forced %v, %v", table, enabled, forced, err)
		}
	}
}
