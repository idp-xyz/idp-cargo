//go:build integration

package bootstrap

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/foundation/identity"
)

// V2.102 batch F, the owner's four acceptance criteria (design section 9, item 10), with the real ORG, FIN and SEA over one database and
// the platform's own permissions. ann is the consultant, olga the owner, dan the single administrator the owner names, rita holds the
// review permissions, pete holds ProposeOnboarding only.

var (
	reviewer = []string{"fin:ReviewSettingsExceptions", "sea:ReviewSettingsExceptions", "fin:ReviewAfterTheFact", "fin:ReadProfit"}
	// everyName is a token that carries every name a person or the platform has: none of it stands in for the platform's mark.
	everyName = []string{"*", "org:*", "fin:*", "sea:*", "org:ProposeOnboarding", "org:ConfirmOnboarding", "org:RecordOnboardingModule", "fin:ConfigureSettings", "fin:ApproveSettings",
		"sea:ConfigureSettings", "sea:ApproveSettings", "fin:DeclareSingleAdministrator", "sea:DeclareSingleAdministrator", "fin:ProposeHeldSettings", "fin:ApproveHeldSettings",
		"sea:ProposeHeldSettings", "sea:ApproveHeldSettings"}
)

// withdraw is the withdrawal by who, with the permissions of the consultant or of the owner.
func (d *settingsDay) withdraw(id, who string, perms []string, reason string) (orgapi.OnboardingReceipt, error) {
	return d.onboarding.WithdrawOnboarding(d.as(who, perms...), orgapi.WithdrawOnboarding{CommandID: "withdraw-" + id + "-" + who, TenantID: d.tenant, ActorID: who, OnboardingID: id, Reason: reason})
}

// Criterion 1: every submission and confirmation is made in the identity of the person actually signed in. The consultant's proposal
// calls FIN's and SEA's own propose commands with the consultant's own context, the owner's confirmation their own approve commands with the
// owner's: the modules' records name ann as the proposer and olga as the approver, with their times and the settings version each made;
// no one else can hold a change set for an onboarding or approve one, whatever names their token carries.
func TestTheSubmissionAndTheConfirmationAreMadeInTheIdentityOfThePersonSignedIn(t *testing.T) {
	d := newSettingsDay(t, "cargo-onb-f1-")
	// no module holds a change set for an onboarding for anyone but the platform's onboarding, which calls them in the consultant's own context
	_, err := d.fin.ProposeFinSettingsChange(d.as("ann", everyName...), finapi.ProposeFinSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "ann", ChangeID: "made-up",
		Entries: []finapi.FinSettingEntry{{ScopeKind: "TENANT", Key: "fin.F7", Value: "BLOCKS"}}, Reason: "a made-up onboarding", OnboardingID: "ob-made-up"})
	saidKind(t, "FIN, a held change set for a made-up onboarding", err, finapi.ErrUnauthorized, "the platform's own")
	_, err = d.sea.ProposeSeaSettingsChange(d.as("ann", everyName...), seaapi.ProposeSeaSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "ann", ChangeID: "made-up",
		Entries: []seaapi.SeaSettingEntry{{ScopeKind: "TENANT", Key: "sea.S1", Value: "SUPERVISOR_ONLY"}}, Reason: "a made-up onboarding", OnboardingID: "ob-made-up"})
	saidKind(t, "SEA, a held change set for a made-up onboarding", err, seaapi.ErrUnauthorized, "the platform's own")
	if f, s := d.count(); f != 0 || s != 0 {
		t.Fatalf("a refused hold made a change set: %d, %d", f, s)
	}
	// another person's principal is not the consultant's
	_, err = d.onboarding.ProposeOnboarding(d.as("eve", consultant...), orgapi.ProposeOnboarding{CommandID: "p-1", TenantID: d.tenant, ActorID: "ann", OnboardingID: "ob-1", Profile: "LEAN",
		RiskStatement: riskStatement, FinanceStaff: 3})
	saidKind(t, "eve's principal for ann's command", err, orgapi.ErrUnauthorized)

	if _, err = d.propose("ob-1", "LEAN", 3, false, nil); err != nil {
		t.Fatal(err)
	}
	d.heldOnly("ob-1") // proposed by ann, held, not approved even by the platform
	proposed := d.org0("ob-1")
	if proposed.Modules[0].State != "PROPOSED" || proposed.Modules[1].State != "PROPOSED" || proposed.Modules[0].ChangeID != "onb-ob-1-fin" || proposed.Modules[1].ChangeID != "onb-ob-1-sea" ||
		proposed.Modules[0].Version != 0 {
		t.Fatalf("the held change ids are in ORG's record: %+v", proposed.Modules)
	}
	// nobody approves a held change set but the onboarding, in the owner's own name: not the owner, not an approver, whatever the token carries
	for name, who := range map[string][]string{"the owner's names": owner, "every name": everyName} {
		_, err = d.fin.ApproveFinSettingsChange(d.as("olga", who...), finapi.ApproveFinSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "olga", ChangeID: "onb-ob-1-fin"})
		saidKind(t, "FIN, "+name, err, finapi.ErrUnauthorized, "is held for onboarding ob-1", "ConfirmOnboarding", "WithdrawOnboarding")
		_, err = d.sea.ApproveSeaSettingsChange(d.as("olga", who...), seaapi.ApproveSeaSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "olga", ChangeID: "onb-ob-1-sea"})
		saidKind(t, "SEA, "+name, err, seaapi.ErrUnauthorized, "is held for onboarding ob-1", "ConfirmOnboarding", "WithdrawOnboarding")
	}
	d.heldOnly("ob-1")

	got, err := d.confirm("ob-1")
	if err != nil || got.Receipt.State != "COMPLETE" {
		t.Fatalf("the confirmation: %+v, %v", got, err)
	}
	fc, _ := d.finChange("onb-ob-1-fin")
	list, err := d.sea.ListSeaSettingsChanges(context.Background(), seaapi.ListSeaSettingsChanges{TenantID: d.tenant})
	d.must("SEA's change sets", err)
	sc := list[0]
	o := got.Onboarding
	if fc.ProposedBy != "ann" || fc.ApprovedBy != "olga" || fc.AutoApproved || fc.Version != 1 || fc.Status != finapi.SettingsInForce || fc.ApprovedAt.Before(fc.ProposedAt) ||
		sc.ProposedBy != "ann" || sc.ApprovedBy != "olga" || sc.AutoApproved || sc.Version != 1 || sc.ApprovedAt.Before(sc.ProposedAt) {
		t.Fatalf("the modules' records: %+v and %+v", fc, sc)
	}
	// the audit trail traces to the person and to the configuration version: the record says the version each module put in force
	if o.Modules[0].Version != fc.Version || o.Modules[1].Version != sc.Version || o.ProposedBy != fc.ProposedBy || o.ConfirmedBy != fc.ApprovedBy {
		t.Fatalf("ORG's record: %+v", o)
	}
	// a second onboarding makes the next version in each module, and the record says so
	if _, err = d.propose("ob-2", "STRICT", 5, false, nil); err != nil {
		t.Fatal(err)
	}
	if got, err = d.confirm("ob-2"); err != nil || got.Onboarding.Modules[0].Version != 2 || got.Onboarding.Modules[1].Version != 2 {
		t.Fatalf("the second confirmation: %+v, %v", got, err)
	}
	if v := d.finValues("TENANT", "")["version"]; v != "2" {
		t.Fatalf("FIN's version in force: %s", v)
	}
}

// Criterion 2: the record says who proposed, who confirmed, which version was confirmed and when; the plan's SHA-256 is the version the owner
// read, and each module's change id and the settings version it put in force are in the same record.
func TestTheOnboardingRecordSaysWhoProposedWhoConfirmedWhichPlanAndWhen(t *testing.T) {
	d := newSettingsDay(t, "cargo-onb-f2-")
	r, err := d.propose("ob-1", "STRICT", 5, false, nil)
	d.must("the proposal", err)
	got, err := d.confirm("ob-1")
	d.must("the confirmation", err)
	o := d.org0("ob-1")
	if o.ProposedBy != "ann" || o.ProposedAt.IsZero() || o.ConfirmedBy != "olga" || o.ConfirmedAt.Before(o.ProposedAt) || o.PlanSHA256 != r.PlanSHA256 || o.PlanSHA256 != orgapi.PlanSHA256(o.Plan) ||
		o.State != "COMPLETE" || got.Onboarding.PlanSHA256 != o.PlanSHA256 || o.AcknowledgedText != riskStatement {
		t.Fatalf("the record: %+v", o)
	}
	for i, m := range o.Modules {
		module := []string{"FIN", "SEA"}[i]
		if m.Module != module || m.ChangeID != "onb-ob-1-"+strings.ToLower(module) || m.State != "APPROVED" || m.Version != 1 || o.Plan[module].CatalogueVersion == "" || len(o.Plan[module].Entries) == 0 {
			t.Fatalf("%s: %+v", module, m)
		}
	}
	list, err := d.org.ListOnboardings(context.Background(), orgapi.ListOnboardings{TenantID: d.tenant})
	if err != nil || len(list) != 1 || list[0].ConfirmedBy != "olga" || list[0].Modules[1].Version != 1 {
		t.Fatalf("the list: %+v, %v", list, err)
	}
}

// failingCancel makes FIN's cancel command fail once, as a module that is down when the onboarding is withdrawn.
type failingCancel struct {
	FINSettings
	fails int
}

func (f *failingCancel) CancelFinSettingsChange(ctx context.Context, cmd finapi.CancelFinSettingsChange) (finapi.Receipt, error) {
	if f.fails > 0 {
		f.fails--
		return finapi.Receipt{}, errors.New("FIN is down for a moment")
	}
	return f.FINSettings.CancelFinSettingsChange(ctx, cmd)
}

// WithdrawOnboarding: the consultant who proposed an onboarding, or the customer's owner, withdraws it while it is not confirmed; ORG records
// who, when and why, and each module's held change set is cancelled in the withdrawing person's own identity; a withdrawn onboarding is never
// confirmed, and its tenant is free of the refusal of other changes.
func TestAnOnboardingNotConfirmedIsWithdrawnAndItsHeldChangeSetsAreCancelled(t *testing.T) {
	d := newSettingsDay(t, "cargo-onb-f3-")
	if _, err := d.propose("ob-1", "LEAN", 3, false, nil); err != nil {
		t.Fatal(err)
	}
	// refusals, before anything is recorded: the form, the org permission, and the modules' cancel permissions the person needs
	for name, c := range map[string]struct {
		who    string
		perms  []string
		reason string
		kind   error
		say    string
	}{
		"a blank actor id":                     {" ", consultant, "x", ErrInvalidActor, "ONBOARDING_INVALID_ACTOR"},
		"the wildcards":                        {"ann", []string{"*", "org:*", "fin:*", "sea:*"}, "x", orgapi.ErrUnauthorized, "by name"},
		"a consultant who did not propose it":  {"pete", consultant, "x", orgapi.ErrUnauthorized, "org:ConfirmOnboarding"},
		"the owner without the modules' names": {"olga", []string{"org:ConfirmOnboarding"}, "x", orgapi.ErrUnauthorized, "fin:ConfigureSettings or fin:ApproveSettings"},
		"the consultant without the SEA names": {"ann", []string{"org:ProposeOnboarding", "fin:ConfigureSettings"}, "x", orgapi.ErrUnauthorized, "sea:ConfigureSettings or sea:ApproveSettings"},
		"the owner's names for the consultant": {"ann", owner, "x", orgapi.ErrUnauthorized, "org:ProposeOnboarding"},
		"no reason":                            {"ann", consultant, "  ", orgapi.ErrRejected, "1 to 2000 characters"},
		"a reason of 2001 characters":          {"ann", consultant, strings.Repeat("r", 2001), orgapi.ErrRejected, "1 to 2000 characters"},
	} {
		_, err := d.withdraw("ob-1", c.who, c.perms, c.reason)
		saidKind(t, name, err, c.kind, c.say)
	}
	if o := d.org0("ob-1"); o.State != "PROPOSED" || o.WithdrawnBy != "" {
		t.Fatalf("a refused withdrawal recorded: %+v", o)
	}
	d.heldOnly("ob-1")

	// the withdrawal fails in FIN once, after ORG recorded it: the same command again completes it, and nothing is cancelled twice
	flaky, err := NewOnboarding(d.org, &failingCancel{FINSettings: d.fin, fails: 1}, d.sea)
	d.must("the onboarding", err)
	cmd := orgapi.WithdrawOnboarding{CommandID: "withdraw-1", TenantID: d.tenant, ActorID: "ann", OnboardingID: "ob-1", Reason: "the customer chose another profile"}
	_, err = flaky.WithdrawOnboarding(d.as("ann", consultant...), cmd)
	saidKind(t, "FIN's failing cancellation", err, ErrOnboardingIncomplete, "FIN:", "FIN is down", "send the same WithdrawOnboarding again")
	if o := d.org0("ob-1"); o.State != "WITHDRAWN" || o.WithdrawnBy != "ann" || o.WithdrawReason != "the customer chose another profile" || o.WithdrawnAt.IsZero() {
		t.Fatalf("ORG recorded the withdrawal: %+v", o)
	}
	if c, _ := d.finChange("onb-ob-1-fin"); c.Status != finapi.SettingsProposed {
		t.Fatalf("FIN's change set waits: %+v", c)
	}
	if list, _ := d.sea.ListSeaSettingsChanges(context.Background(), seaapi.ListSeaSettingsChanges{TenantID: d.tenant}); list[0].Status != seaapi.SettingsCancelled {
		t.Fatalf("SEA's change set was cancelled by the first try: %+v", list[0])
	}
	r, err := flaky.WithdrawOnboarding(d.as("ann", consultant...), cmd)
	if err != nil || r.State != "WITHDRAWN" || !strings.Contains(r.Notice, "withdrawn by ann") || !strings.Contains(r.Notice, "are cancelled in ann's own name") {
		t.Fatalf("the retry: %+v, %v", r, err)
	}
	fc, _ := d.finChange("onb-ob-1-fin")
	list, err := d.sea.ListSeaSettingsChanges(context.Background(), seaapi.ListSeaSettingsChanges{TenantID: d.tenant})
	d.must("SEA's change sets", err)
	for name, c := range map[string][4]string{"FIN": {fc.Status, fc.CancelledBy, fc.CancelledRole, fc.CancelReason}, "SEA": {list[0].Status, list[0].CancelledBy, list[0].CancelledRole, list[0].CancelReason}} {
		if c[0] != "CANCELLED" || c[1] != "ann" || c[2] != "CONFIGURER" || !strings.Contains(c[3], "onboarding ob-1 withdrawn by ann") {
			t.Errorf("%s's change set: %v", name, c)
		}
	}
	if f, s := d.count(); f != 1 || s != 1 {
		t.Fatalf("change sets: %d, %d", f, s)
	}
	// a withdrawn onboarding is never confirmed, and nothing is approved
	_, err = d.confirm("ob-1")
	saidKind(t, "confirming a withdrawn onboarding", err, orgapi.ErrRejected, "withdrawn by ann", "never confirmed", "propose a new onboarding under another id")
	if v := d.finValues("TENANT", "")["version"]; v != "0" {
		t.Fatalf("a version was made: %s", v)
	}
	// the same proposal again changes nothing for a withdrawn onboarding, and the tenant is free: another change is accepted, and a new onboarding is proposed
	if _, err = d.propose("ob-1", "LEAN", 3, false, nil); err != nil {
		t.Fatalf("the proposal again: %v", err)
	}
	if f, s := d.count(); f != 1 || s != 1 {
		t.Fatalf("a replayed proposal held a change set for a withdrawn onboarding: %d, %d", f, s)
	}
	_, err = d.fin.ProposeFinSettingsChange(d.as("dan", adminFin...), finapi.ProposeFinSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "dan", ChangeID: "after",
		Entries: []finapi.FinSettingEntry{{ScopeKind: "TENANT", Key: "fin.F7", Value: "BLOCKS"}}, Reason: "after the withdrawal"})
	d.must("a change after the withdrawal", err)
	d2 := d
	if _, err = d2.propose("ob-2", "STANDARD", 3, false, nil); err != nil {
		t.Fatalf("a new onboarding under another id: %v", err)
	}

	// the owner withdraws another: the cancellations are the owner's own, as an approver; a change set the owner approved of is not touched later
	e := newSettingsDay(t, "cargo-onb-f3b-")
	if _, err = e.propose("ob-1", "STRICT", 3, false, nil); err != nil {
		t.Fatal(err)
	}
	// carl refused FIN's change set before the withdrawal: it is done already
	if _, err = e.fin.RefuseFinSettingsChange(e.as("carl", "fin:ApproveSettings"), finapi.RefuseFinSettingsChange{CommandID: "carl-1", TenantID: e.tenant, ActorID: "carl", ChangeID: "onb-ob-1-fin", Reason: "no"}); err != nil {
		t.Fatal(err)
	}
	if _, err = e.withdraw("ob-1", "olga", owner, "the wrong customer"); err != nil {
		t.Fatalf("the owner's withdrawal: %v", err)
	}
	fc, _ = e.finChange("onb-ob-1-fin")
	list, _ = e.sea.ListSeaSettingsChanges(context.Background(), seaapi.ListSeaSettingsChanges{TenantID: e.tenant})
	if fc.Status != finapi.SettingsRefused || list[0].Status != "CANCELLED" || list[0].CancelledBy != "olga" || list[0].CancelledRole != "APPROVER" {
		t.Fatalf("FIN's refused and SEA's cancelled by the owner: %+v, %+v", fc, list[0])
	}
	if o := e.org0("ob-1"); o.State != "WITHDRAWN" || o.WithdrawnBy != "olga" {
		t.Fatalf("the record: %+v", o)
	}

	// a confirmed onboarding is not withdrawn, and its change sets are left as they are
	g := newSettingsDay(t, "cargo-onb-f3c-")
	if _, err = g.propose("ob-1", "STRICT", 3, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = g.confirm("ob-1"); err != nil {
		t.Fatal(err)
	}
	_, err = g.withdraw("ob-1", "ann", consultant, "too late")
	saidKind(t, "withdrawing a confirmed onboarding", err, orgapi.ErrRejected, "confirmed by olga", "is not withdrawn", "restores the earlier values")
	if c, _ := g.finChange("onb-ob-1-fin"); c.Status != finapi.SettingsInForce {
		t.Fatalf("a confirmed onboarding's change set: %+v", c)
	}
}

// No other change set is proposed or approved while an onboarding waits: the refusal names the onboarding and the way out, in FIN and in SEA; a
// cancellation, the way out, is not refused; a second onboarding is not proposed; once the onboarding is confirmed or withdrawn, changes are
// accepted again.
func TestNoOtherChangeIsMadeWhileAnOnboardingWaits(t *testing.T) {
	d := newSettingsDay(t, "cargo-onb-f4-")
	dan := append(append([]string{}, adminFin...), adminSea...)
	finChange := func(id, key, value string) error {
		_, err := d.fin.ProposeFinSettingsChange(d.as("dan", dan...), finapi.ProposeFinSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "dan", ChangeID: id,
			Entries: []finapi.FinSettingEntry{{ScopeKind: "TENANT", Key: key, Value: value}}, Reason: "change " + id})
		return err
	}
	seaChange := func(id, value string) error {
		_, err := d.sea.ProposeSeaSettingsChange(d.as("dan", dan...), seaapi.ProposeSeaSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "dan", ChangeID: id,
			Entries: []seaapi.SeaSettingEntry{{ScopeKind: "TENANT", Key: "sea.S1", Value: value}}, Reason: "change " + id})
		return err
	}
	// each module has a loosening waiting for a second person, proposed before the onboarding (F4 starts at its strictest value, and S1 is tightened first)
	d.must("a loosening waiting for a second person, proposed before the onboarding", finChange("before-f", "fin.F4", "ONE_STEP_WITHOUT_OPENED_OR_LEFT"))
	d.must("SEA tightens", seaChange("pre-s", "SUPERVISOR_ONLY"))
	d.must("SEA's loosening", seaChange("before-s", "WITH_REASON"))
	if _, err := d.propose("ob-1", "LEAN", 3, false, nil); err != nil {
		t.Fatal(err)
	}
	saidKind(t, "FIN's proposal", finChange("n-f", "fin.F7", "BLOCKS"), finapi.ErrRejected, "SETTINGS_ONBOARDING_WAITING", "onboarding ob-1 waits", "change set onb-ob-1-fin", "confirm the onboarding", "withdraw it")
	saidKind(t, "SEA's proposal", seaChange("n-s", "WITH_REASON"), seaapi.ErrRejected, "SETTINGS_ONBOARDING_WAITING", "onboarding ob-1 waits", "change set onb-ob-1-sea", "confirm the onboarding")
	_, err := d.fin.ApproveFinSettingsChange(d.as("olga", owner...), finapi.ApproveFinSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "olga", ChangeID: "before-f"})
	saidKind(t, "FIN's approval of an older change set", err, finapi.ErrRejected, "SETTINGS_ONBOARDING_WAITING", "the approval of change set before-f")
	_, err = d.sea.ApproveSeaSettingsChange(d.as("olga", owner...), seaapi.ApproveSeaSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "olga", ChangeID: "before-s"})
	saidKind(t, "SEA's approval of an older change set", err, seaapi.ErrRejected, "SETTINGS_ONBOARDING_WAITING", "the approval of change set before-s")
	// a second onboarding is not proposed (nothing is recorded), and the same one again is answered
	_, err = d.propose("ob-2", "STRICT", 3, false, nil)
	saidKind(t, "a second onboarding", err, orgapi.ErrRejected, "onboarding ob-1 of this tenant waits", "ConfirmOnboarding", "WithdrawOnboarding")
	if _, err = d.org.GetOnboarding(context.Background(), orgapi.GetOnboarding{TenantID: d.tenant, OnboardingID: "ob-2"}); !errors.Is(err, orgapi.ErrNotFound) {
		t.Fatalf("a refused proposal was recorded: %v", err)
	}
	if _, err = d.propose("ob-1", "LEAN", 3, false, nil); err != nil {
		t.Fatalf("the same proposal again: %v", err)
	}
	// the way out is not refused: a person cancels their own pending change set
	if _, err = d.fin.CancelFinSettingsChange(d.as("dan", dan...), finapi.CancelFinSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "dan", ChangeID: "before-f", Reason: "not now"}); err != nil {
		t.Fatalf("a cancellation while an onboarding waits: %v", err)
	}
	if got, err := d.confirm("ob-1"); err != nil || got.Receipt.State != "COMPLETE" {
		t.Fatalf("the confirmation: %+v, %v", got, err)
	}
	d.must("FIN's proposal after the confirmation", finChange("n-f", "fin.F7", "BLOCKS"))
	d.must("SEA's approval after the confirmation", func() error {
		_, err := d.sea.ApproveSeaSettingsChange(d.as("olga", owner...), seaapi.ApproveSeaSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "olga", ChangeID: "before-s"})
		return err
	}())
}

// Criterion 3, the normal course: a loosening is approved by another person who holds the permission, and the approval says so; under the
// single administrator's exception the written acknowledgement and the declaration it rests on (its onboarding, the administrator it names and when
// it was made) are recorded separately, in each module and in the exception report.
func TestLooseningIsApprovedByAnotherPersonOrBySingleAdministratorWithItsBasis(t *testing.T) {
	d := newSettingsDay(t, "cargo-onb-f5-")
	both := append(append([]string{}, adminFin...), adminSea...)
	if _, err := d.propose("ob-1", "STRICT", 3, true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := d.confirm("ob-1"); err != nil {
		t.Fatal(err)
	}
	propose := func(who, id, value string) {
		_, err := d.fin.ProposeFinSettingsChange(d.as(who, both...), finapi.ProposeFinSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: who, ChangeID: "f-" + id,
			Entries: []finapi.FinSettingEntry{{ScopeKind: "TENANT", Key: "fin.F7", Value: value}}, Reason: "change " + id})
		d.must("FIN's change "+id, err)
		_, err = d.sea.ProposeSeaSettingsChange(d.as(who, both...), seaapi.ProposeSeaSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: who, ChangeID: "s-" + id,
			Entries: []seaapi.SeaSettingEntry{{ScopeKind: "TENANT", Key: "sea.S1", Value: map[string]string{"DOES_NOT_BLOCK": "WITH_REASON", "BLOCKS": "SUPERVISOR_ONLY"}[value]}}, Reason: "change " + id})
		d.must("SEA's change "+id, err)
	}
	// the normal course: frank proposes a loosening, and carl, another person who holds the permission, approves it; no acknowledgement, no basis
	propose("frank", "normal", "DOES_NOT_BLOCK")
	if _, err := d.fin.ApproveFinSettingsChange(d.as("carl", both...), finapi.ApproveFinSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "carl", ChangeID: "f-normal"}); err != nil {
		t.Fatal(err)
	}
	if c, _ := d.finChange("f-normal"); c.ApprovedBy != "carl" || c.ProposedBy != "frank" || c.RiskAcknowledgement != "" || c.BasisOnboardingID != "" || c.Status != finapi.SettingsInForce {
		t.Fatalf("the normal course: %+v", c)
	}
	// the exception: dan, the administrator the owner named, approves their own loosening (after a tightening, which its proposer approves at once)
	propose("frank", "tight", "BLOCKS")
	propose("dan", "own", "DOES_NOT_BLOCK")
	if _, err := d.fin.ApproveFinSettingsChange(d.as("dan", both...), finapi.ApproveFinSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "dan", ChangeID: "f-own",
		RiskAcknowledgement: "I am the customer's only administrator and I accept the risk."}); err != nil {
		t.Fatalf("FIN's self-approval: %v", err)
	}
	if _, err := d.sea.ApproveSeaSettingsChange(d.as("dan", both...), seaapi.ApproveSeaSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "dan", ChangeID: "s-own",
		RiskAcknowledgement: "the same administrator, the same risk"}); err != nil {
		t.Fatalf("SEA's self-approval: %v", err)
	}
	fc, _ := d.finChange("f-own")
	seaList, err := d.sea.ListSeaSettingsChanges(context.Background(), seaapi.ListSeaSettingsChanges{TenantID: d.tenant})
	d.must("SEA's change sets", err)
	var sc seaapi.SeaSettingsChange
	for _, c := range seaList {
		if c.ChangeID == "s-own" {
			sc = c
		}
	}
	if fc.ApprovedBy != "dan" || fc.RiskAcknowledgement == "" || fc.BasisOnboardingID != "ob-1" || sc.ApprovedBy != "dan" || sc.RiskAcknowledgement == "" || sc.BasisOnboardingID != "ob-1" {
		t.Fatalf("the self-approvals: %+v and %+v", fc, sc)
	}
	now := time.Now().UTC()
	rep, err := d.tools.GetExceptionReport(d.as("olga", owner...), d.tenant, "olga", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil || len(rep.Fin.SelfApproved) != 1 || len(rep.Sea.SelfApproved) != 1 {
		t.Fatalf("the exception report: %+v, %v", rep, err)
	}
	for name, line := range map[string][5]any{"FIN": {rep.Fin.SelfApproved[0].ApprovedBy, rep.Fin.SelfApproved[0].Acknowledgement, rep.Fin.SelfApproved[0].BasisOnboardingID,
		rep.Fin.SelfApproved[0].BasisAdministrator, rep.Fin.SelfApproved[0].BasisDeclaredAt}, "SEA": {rep.Sea.SelfApproved[0].ApprovedBy, rep.Sea.SelfApproved[0].Acknowledgement,
		rep.Sea.SelfApproved[0].BasisOnboardingID, rep.Sea.SelfApproved[0].BasisAdministrator, rep.Sea.SelfApproved[0].BasisDeclaredAt}} {
		if line[0] != "dan" || line[1] == "" || line[2] != "ob-1" || line[3] != "dan" || line[4].(time.Time).IsZero() {
			t.Errorf("%s's self-approved line: %v", name, line)
		}
	}
}

// Criterion 4: each single administrator's self-approval is reviewed one by one, by someone else who holds the review permission, with a
// conclusion; one query says, for a tenant and for a holder of the review permissions, what is reviewed (by whom, with what conclusion) and what
// still waits, each module's part to that module's holders.
func TestTheReviewQueueSaysWhatIsReviewedAndWhatStillWaits(t *testing.T) {
	d := newSettingsDay(t, "cargo-onb-f6-")
	d.legalEntity()
	both := append(append([]string{}, adminFin...), adminSea...)
	if _, err := d.propose("ob-1", "STRICT", 3, true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := d.confirm("ob-1"); err != nil {
		t.Fatal(err)
	}
	_, err := d.fin.ProposeFinSettingsChange(d.as("dan", both...), finapi.ProposeFinSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "dan", ChangeID: "f-own",
		Entries: []finapi.FinSettingEntry{{ScopeKind: "TENANT", Key: "fin.F7", Value: "DOES_NOT_BLOCK"}}, Reason: "loosen"})
	d.must("FIN's loosening", err)
	_, err = d.sea.ProposeSeaSettingsChange(d.as("dan", both...), seaapi.ProposeSeaSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "dan", ChangeID: "s-own",
		Entries: []seaapi.SeaSettingEntry{{ScopeKind: "TENANT", Key: "sea.S1", Value: "WITH_REASON"}}, Reason: "loosen"})
	d.must("SEA's loosening", err)
	_, err = d.fin.ApproveFinSettingsChange(d.as("dan", both...), finapi.ApproveFinSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "dan", ChangeID: "f-own", RiskAcknowledgement: "I accept"})
	d.must("FIN's self-approval", err)
	_, err = d.sea.ApproveSeaSettingsChange(d.as("dan", both...), seaapi.ApproveSeaSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "dan", ChangeID: "s-own", RiskAcknowledgement: "I accept"})
	d.must("SEA's self-approval", err)

	now := time.Now().UTC()
	from, to := now.AddDate(0, 0, -21), now.Add(time.Hour)
	queue := func(who string, perms []string, entities ...string) (ReviewQueue, error) {
		return d.tools.GetReviewQueue(d.as(who, perms...), d.tenant, who, from, to, entities)
	}
	q, err := queue("rita", reviewer, "LE")
	if err != nil || q.FinSelfApprovals == nil || q.SeaSelfApprovals == nil || len(q.FinSelfApprovals.Pending) != 1 || len(q.SeaSelfApprovals.Pending) != 1 || len(q.FinSelfApprovals.Reviewed) != 0 ||
		len(q.LeftOut) != 0 || len(q.FinAfterTheFact) != 1 {
		t.Fatalf("the queue before any review: %+v, %v", q, err)
	}
	if p := q.FinSelfApprovals.Pending[0]; p.ChangeID != "f-own" || p.ApprovedBy != "dan" || p.BasisOnboardingID != "ob-1" || p.BasisAdministrator != "dan" {
		t.Fatalf("FIN's pending line: %+v", p)
	}
	// the weeks of the period that have ended have no review: each is listed, with its lines (none here)
	week := q.FinAfterTheFact[0]
	if week.LegalEntityID != "LE" || len(week.WeeksWithoutReview) < 2 || len(week.Lines) != 0 || week.WeeksWithoutReview[0].ListedLines != 0 {
		t.Fatalf("FIN's weeks: %+v", week)
	}

	// the reviewers: the approver does not review, a person without the permission does not, and the wildcards grant none of it
	for name, c := range map[string]struct {
		who   string
		perms []string
	}{"the approver": {"dan", append([]string{"fin:ReviewSettingsExceptions"}, both...)}, "the owner": {"olga", owner}, "the wildcards": {"rita", []string{"*", "fin:*"}}} {
		_, err = d.fin.ReviewFinSelfApprovedSettingsChange(d.as(c.who, c.perms...), finapi.ReviewFinSelfApprovedSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: c.who,
			ChangeID: "f-own", Conclusion: "ACCEPTED"})
		if err == nil {
			t.Errorf("%s reviewed", name)
		}
	}
	if _, err = d.fin.ReviewFinSelfApprovedSettingsChange(d.as("rita", reviewer...), finapi.ReviewFinSelfApprovedSettingsChange{CommandID: "rev-f", TenantID: d.tenant, ActorID: "rita",
		ChangeID: "f-own", Conclusion: "ACCEPTED"}); err != nil {
		t.Fatalf("FIN's review: %v", err)
	}
	if _, err = d.sea.ReviewSeaSelfApprovedSettingsChange(d.as("rita", reviewer...), seaapi.ReviewSeaSelfApprovedSettingsChange{CommandID: "rev-s", TenantID: d.tenant, ActorID: "rita",
		ChangeID: "s-own", Conclusion: "NOT_ACCEPTED", Note: "S1 WITH_REASON is not a risk this customer may take"}); err != nil {
		t.Fatalf("SEA's review: %v", err)
	}
	if q, err = queue("rita", reviewer, "LE"); err != nil || len(q.FinSelfApprovals.Pending) != 0 || len(q.SeaSelfApprovals.Pending) != 0 || len(q.FinSelfApprovals.Reviewed) != 1 || len(q.SeaSelfApprovals.Reviewed) != 1 {
		t.Fatalf("the queue after the reviews: %+v, %v", q, err)
	}
	if r := q.FinSelfApprovals.Reviewed[0]; r.ChangeID != "f-own" || r.ReviewedBy != "rita" || r.Conclusion != "ACCEPTED" || r.Note != "" || r.ReviewedAt.IsZero() || r.ApprovedBy != "dan" {
		t.Fatalf("FIN's reviewed line: %+v", r)
	}
	if r := q.SeaSelfApprovals.Reviewed[0]; r.ReviewedBy != "rita" || r.Conclusion != "NOT_ACCEPTED" || r.Note != "S1 WITH_REASON is not a risk this customer may take" {
		t.Fatalf("SEA's reviewed line: %+v", r)
	}
	// NOT_ACCEPTED changed no setting: SEA's S1 is still what dan approved
	if v, _ := d.seaValue("TENANT", ""); v != "WITH_REASON" {
		t.Fatalf("a review changed a setting: %s", v)
	}

	// each module's part goes to that module's holders; a holder of one is not told the other's; a person who holds none is refused
	q, err = queue("sam", []string{"sea:ReviewSettingsExceptions"}, "LE")
	if err != nil || q.FinSelfApprovals != nil || q.SeaSelfApprovals == nil || len(q.FinAfterTheFact) != 0 || len(q.LeftOut) != 2 ||
		!strings.Contains(q.LeftOut[0].Part, "FIN's self-approved") || !strings.Contains(q.LeftOut[0].Reason, "fin:ReviewSettingsExceptions") || !strings.Contains(q.LeftOut[1].Part, "after-the-fact") {
		t.Fatalf("a holder of SEA's permission: %+v, %v", q, err)
	}
	_, err = queue("fiona", []string{"fin:ReviewAfterTheFact"}, "LE") // without ReadProfit the weeks' lines are not told, and nothing else is hers
	saidKind(t, "a reviewer of weeks without ReadProfit", err, orgapi.ErrUnauthorized, "fin:ReviewAfterTheFact (with fin:ReadProfit)")
	q, err = queue("fiona", []string{"fin:ReviewAfterTheFact", "fin:ReadProfit"}) // no legal entity named
	if err != nil || q.FinSelfApprovals != nil || q.SeaSelfApprovals != nil || len(q.FinAfterTheFact) != 0 || len(q.LeftOut) != 3 || !strings.Contains(q.LeftOut[2].Reason, "no legal entity") {
		t.Fatalf("no legal entity named: %+v, %v", q, err)
	}
	for name, perms := range map[string][]string{"the wildcards": {"*", "fin:*", "sea:*"}, "the approver's names": both, "nothing": nil} {
		_, err = queue("pat", perms, "LE")
		saidKind(t, name, err, orgapi.ErrUnauthorized, "fin:ReviewSettingsExceptions, sea:ReviewSettingsExceptions or fin:ReviewAfterTheFact", "ask for one of them")
	}
	_, err = d.tools.GetReviewQueue(d.as("rita", reviewer...), d.tenant, "rita", from, from, []string{"LE"})
	saidKind(t, "an empty period", err, orgapi.ErrRejected, "from before to")
	_, err = d.tools.GetReviewQueue(d.as("rita", reviewer...), d.tenant, "rita", now.AddDate(-2, 0, 0), to, []string{"LE"})
	if err == nil || !strings.Contains(err.Error(), "at most 53 weeks") {
		t.Fatalf("a period of two years: %v", err)
	}
}

// holdORGLock takes the onboarding's lock on a second connection, inside a transaction, and answers its backend pid.
func holdORGLock(t *testing.T, tenant, id string) (pid int, release func()) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("IDP_CARGO_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('cargo_org.onboarding/' || $1::text || '/' || $2::text, 0))`, tenant, id); err == nil {
		err = tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid)
	}
	if err != nil {
		t.Fatal(err)
	}
	var once bool
	release = func() {
		if !once {
			once = true
			tx.Commit(ctx)
			conn.Close(ctx)
		}
	}
	t.Cleanup(release)
	return pid, release
}

// blockedBy waits until n sessions are blocked, directly or through others, by the holder (pg_blocking_pids).
func blockedBy(t *testing.T, holder, n int) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("IDP_CARGO_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	deadline := time.Now().Add(15 * time.Second)
	var blocked int
	for time.Now().Before(deadline) {
		if err = conn.QueryRow(ctx, `WITH RECURSIVE chain(pid) AS (
            SELECT $1::int
            UNION
            SELECT a.pid FROM pg_stat_activity a JOIN chain c ON c.pid = ANY(pg_blocking_pids(a.pid)))
          SELECT count(*) - 1 FROM chain`, holder).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%d sessions are blocked by the lock holder, want %d", blocked, n)
}

// The consultant's withdrawal and the owner's confirmation of one onboarding, at once, take turns on the onboarding's lock: it is held on a second
// connection, the two are started one after another, each only once the sessions before it are blocked by the holder (pg_blocking_pids), and when it is
// released the first started wins: a withdrawal first cancels the held change sets and the confirmation is refused with nothing approved; a
// confirmation first approves them and the withdrawal is refused with them in force.
func TestAWithdrawalAndAConfirmationAtOnceTakeTurns(t *testing.T) {
	for _, c := range []struct {
		name          string
		withdrawFirst bool
	}{{"the withdrawal first", true}, {"the confirmation first", false}} {
		t.Run(c.name, func(t *testing.T) {
			d := newSettingsDay(t, "cargo-onb-f7-")
			if _, err := d.propose("ob-1", "STRICT", 3, false, nil); err != nil {
				t.Fatal(err)
			}
			pid, release := holdORGLock(t, d.tenant, "ob-1")
			var withdrawErr, confirmErr error
			var confirmed OnboardingConfirmation
			var wg sync.WaitGroup
			doWithdraw := func() {
				defer wg.Done()
				_, withdrawErr = d.withdraw("ob-1", "ann", consultant, "the customer chose another profile")
			}
			doConfirm := func() {
				defer wg.Done()
				confirmed, confirmErr = d.confirm("ob-1")
			}
			first, second := doConfirm, doWithdraw
			if c.withdrawFirst {
				first, second = doWithdraw, doConfirm
			}
			wg.Add(1)
			go first()
			blockedBy(t, pid, 1)
			wg.Add(1)
			go second()
			blockedBy(t, pid, 2)
			if o := d.org0("ob-1"); o.State != "PROPOSED" { // nothing was written while the lock was held
				t.Fatalf("a command went past the lock: %+v", o)
			}
			release()
			wg.Wait()
			o := d.org0("ob-1")
			fc, _ := d.finChange("onb-ob-1-fin")
			if c.withdrawFirst {
				if withdrawErr != nil || o.State != "WITHDRAWN" || fc.Status != "CANCELLED" {
					t.Fatalf("the withdrawal: %v, %+v, %+v", withdrawErr, o, fc)
				}
				saidKind(t, "the confirmation after the withdrawal", confirmErr, orgapi.ErrRejected, "withdrawn by ann", "never confirmed")
				if v := d.finValues("TENANT", "")["version"]; v != "0" {
					t.Fatalf("a version was made: %s", v)
				}
				return
			}
			if confirmErr != nil || confirmed.Receipt.State != "COMPLETE" || o.State != "COMPLETE" || fc.Status != finapi.SettingsInForce || fc.ApprovedBy != "olga" {
				t.Fatalf("the confirmation: %v, %+v, %+v, %+v", confirmErr, confirmed, o, fc)
			}
			saidKind(t, "the withdrawal after the confirmation", withdrawErr, orgapi.ErrRejected, "confirmed by olga", "is not withdrawn")
		})
	}
}

// spyFIN records the principal of the context each propose and approve command of the onboarding reaches FIN with.
type spyFIN struct {
	FINSettings
	proposed, approved []identity.Principal
}

func (s *spyFIN) ProposeFinSettingsChange(ctx context.Context, cmd finapi.ProposeFinSettingsChange) (finapi.Receipt, error) {
	p, _ := identity.PrincipalFrom(ctx)
	s.proposed = append(s.proposed, p)
	return s.FINSettings.ProposeFinSettingsChange(ctx, cmd)
}

func (s *spyFIN) ApproveFinSettingsChange(ctx context.Context, cmd finapi.ApproveFinSettingsChange) (finapi.Receipt, error) {
	p, _ := identity.PrincipalFrom(ctx)
	s.approved = append(s.approved, p)
	return s.FINSettings.ApproveFinSettingsChange(ctx, cmd)
}

// The platform builds no principal to act in a person's name: the module's own propose command is called with the consultant's own principal, as
// the token gave it (here with a permission no principal the platform makes would carry), and its own approve command with the owner's.
func TestTheModulesAreCalledWithThePrincipalOfThePersonSignedIn(t *testing.T) {
	d := newSettingsDay(t, "cargo-onb-f1b-")
	spy := &spyFIN{FINSettings: d.fin}
	onboarding, err := NewOnboarding(d.org, spy, d.sea)
	d.must("the onboarding", err)
	ann := append(append([]string{}, consultant...), "fin:ReadProfit")
	olga := append(append([]string{}, owner...), "fin:ReadProfit")
	r, err := onboarding.ProposeOnboarding(d.as("ann", ann...), orgapi.ProposeOnboarding{CommandID: "p-spy", TenantID: d.tenant, ActorID: "ann", OnboardingID: "ob-1", Profile: "LEAN",
		RiskStatement: riskStatement, FinanceStaff: 3})
	d.must("the proposal", err)
	if len(spy.proposed) != 1 || spy.proposed[0].ActorID != "ann" || spy.proposed[0].TenantID != d.tenant || !slices.Equal(spy.proposed[0].Permissions, ann) {
		t.Fatalf("FIN's propose command was called with %+v, want ann's own principal with %v", spy.proposed, ann)
	}
	c := d.confirmCommand("ob-1")
	c.PlanSHA256 = r.PlanSHA256
	if _, err = onboarding.ConfirmOnboarding(d.as("olga", olga...), c); err != nil {
		t.Fatal(err)
	}
	if len(spy.approved) != 1 || spy.approved[0].ActorID != "olga" || spy.approved[0].TenantID != d.tenant || !slices.Equal(spy.approved[0].Permissions, olga) {
		t.Fatalf("FIN's approve command was called with %+v, want olga's own principal with %v", spy.approved, olga)
	}
}
