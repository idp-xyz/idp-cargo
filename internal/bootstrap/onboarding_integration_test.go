//go:build integration

package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	finmodule "go.idp.xyz/idp-cargo/internal/contexts/fin"
	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	orgmodule "go.idp.xyz/idp-cargo/internal/contexts/org"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	seamodule "go.idp.xyz/idp-cargo/internal/contexts/sea"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/foundation/identity"
)

// V2.102: customer onboarding, a single administrator, the exception report, and the export and import of the settings,
// with the real ORG, FIN and SEA over one database and the platform's own permissions (permissions{}): every call names the
// principal it acts as. ann is the implementation consultant, olga the customer's appointed owner.

var (
	consultant    = []string{"org:ProposeOnboarding", "fin:ConfigureSettings", "sea:ConfigureSettings"}
	owner         = []string{"org:ConfirmOnboarding", "fin:ApproveSettings", "sea:ApproveSettings"}
	riskStatement = "The customer accepts the settings of the profile, including what they leave to one person's judgement."
)

type settingsDay struct {
	t          *testing.T
	platform   *Platform
	org        *orgmodule.Module
	fin        *finmodule.Module
	sea        *seamodule.Module
	onboarding *Onboarding
	tools      *SettingsTools
	tenant     string
	n          int
}

func newSettingsDay(t *testing.T, prefix string) *settingsDay {
	t.Helper()
	platform := openPlatform(t)
	orgModule, err := platform.ORG(permissions{})
	if err != nil {
		t.Fatal(err)
	}
	rateModule, err := platform.RATE(orgModule, permissions{}, margins{floor: 2000})
	if err != nil {
		t.Fatal(err)
	}
	comModule, err := platform.COM(orgModule, rateModule, permissions{})
	if err != nil {
		t.Fatal(err)
	}
	fulModule, phyModule, err := platform.FULAndPHY(comModule, permissions{}, permissions{})
	if err != nil {
		t.Fatal(err)
	}
	bookModule, err := platform.BOOK(orgModule, permissions{})
	if err != nil {
		t.Fatal(err)
	}
	seaModule, err := platform.SEA(bookModule, fulModule, permissions{})
	if err != nil {
		t.Fatal(err)
	}
	finModule, err := platform.FIN(orgModule, rateModule, fulModule, &DocumentSnapshots{}, phyModule, permissions{})
	if err != nil {
		t.Fatal(err)
	}
	d := &settingsDay{t: t, platform: platform, org: orgModule, fin: finModule, sea: seaModule, tenant: uniqueTenant(prefix)}
	if d.onboarding, err = NewOnboarding(orgModule, finModule, seaModule); err != nil {
		t.Fatal(err)
	}
	if d.tools, err = NewSettingsTools(finModule, seaModule); err != nil {
		t.Fatal(err)
	}
	seedORG(t, platform, d.tenant)
	return d
}

func (d *settingsDay) as(actor string, permissions ...string) context.Context {
	return identity.WithPrincipal(context.Background(), identity.Principal{TenantID: d.tenant, ActorID: actor, Permissions: permissions})
}

func (d *settingsDay) command() string {
	d.n++
	return fmt.Sprintf("cmd-%d", d.n)
}

func (d *settingsDay) must(what string, err error) {
	d.t.Helper()
	if err != nil {
		d.t.Fatalf("%s: %v", what, err)
	}
}

// legalEntity gives LE a finance policy in FIN and an HBL series in SEA, so that both know it.
func (d *settingsDay) legalEntity() {
	d.t.Helper()
	_, err := d.fin.OpenFinancePolicy(d.as("acct-1", "fin:*"), finapi.OpenFinancePolicy{CommandID: d.command(), TenantID: d.tenant, ActorID: "acct-1", LegalEntityID: "LE",
		FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: time.Now().UTC()})
	d.must("FIN's policy of LE", err)
	_, err = d.sea.RegisterHblSeries(d.as("ops-1", "sea:*"), seaapi.RegisterHblSeries{CommandID: d.command(), TenantID: d.tenant, ActorID: "ops-1", LegalEntityID: "LE",
		Template: "SHAL{seq:7}", FirstNumber: 1, OccurredAt: time.Now().UTC()})
	d.must("SEA's series of LE", err)
}

func (d *settingsDay) propose(id, profile string, staff int, single bool, overrides map[string][]orgapi.OnboardingEntry) (orgapi.OnboardingReceipt, error) {
	return d.onboarding.ProposeOnboarding(d.as("ann", consultant...), orgapi.ProposeOnboarding{CommandID: "propose-" + id, TenantID: d.tenant, ActorID: "ann", OnboardingID: id,
		Profile: profile, Overrides: overrides, RiskStatement: riskStatement, FinanceStaff: staff, SingleAdministrator: single})
}

func (d *settingsDay) confirmCommand(id string) orgapi.ConfirmOnboarding {
	return orgapi.ConfirmOnboarding{CommandID: "confirm-" + id, TenantID: d.tenant, ActorID: "olga", OnboardingID: id, AcknowledgedText: riskStatement}
}

func (d *settingsDay) confirm(id string) (OnboardingConfirmation, error) {
	return d.onboarding.ConfirmOnboarding(d.as("olga", owner...), d.confirmCommand(id))
}

func (d *settingsDay) finValues(scopeKind, scopeID string) map[string]string {
	d.t.Helper()
	got, err := d.fin.GetFinSettings(context.Background(), finapi.GetFinSettings{TenantID: d.tenant, ScopeKind: scopeKind, ScopeID: scopeID})
	d.must("FIN's settings", err)
	out := map[string]string{"version": fmt.Sprint(got.Version)}
	for _, it := range got.Items {
		out[it.Key] = it.Value
	}
	return out
}

func (d *settingsDay) seaValue(scopeKind, scopeID string) (string, int) {
	d.t.Helper()
	got, err := d.sea.GetSeaSettings(context.Background(), seaapi.GetSeaSettings{TenantID: d.tenant, ScopeKind: scopeKind, ScopeID: scopeID})
	d.must("SEA's settings", err)
	return got.Items[0].Value, got.Version
}

func (d *settingsDay) finChange(id string) (finapi.FinSettingsChange, bool) {
	d.t.Helper()
	list, err := d.fin.ListFinSettingsChanges(context.Background(), finapi.ListFinSettingsChanges{TenantID: d.tenant})
	d.must("FIN's change sets", err)
	for _, c := range list {
		if c.ChangeID == id {
			return c, true
		}
	}
	return finapi.FinSettingsChange{}, false
}

func (d *settingsDay) count() (fin, sea int) {
	d.t.Helper()
	f, err := d.fin.ListFinSettingsChanges(context.Background(), finapi.ListFinSettingsChanges{TenantID: d.tenant})
	d.must("FIN's change sets", err)
	s, err := d.sea.ListSeaSettingsChanges(context.Background(), seaapi.ListSeaSettingsChanges{TenantID: d.tenant})
	d.must("SEA's change sets", err)
	return len(f), len(s)
}

func saidKind(t *testing.T, what string, err error, kind error, contains ...string) {
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

// The permissions of an onboarding are granted by name: org:ProposeOnboarding and org:ConfirmOnboarding, and, because the
// platform acts for the two people later, the modules' ConfigureSettings of the consultant and ApproveSettings of the
// owner; "org:*", "fin:*", "sea:*" and "*" grant none of them. A refusal before anything is recorded names the way out.
func TestOnboardingNeedsItsPermissionsByName(t *testing.T) {
	d := newSettingsDay(t, "cargo-onb-perm-")
	propose := orgapi.ProposeOnboarding{CommandID: "p-1", TenantID: d.tenant, ActorID: "ann", OnboardingID: "ob-1", Profile: "STANDARD", RiskStatement: riskStatement, FinanceStaff: 3}
	for name, perms := range map[string][]string{
		"the wildcards":                  {"*", "org:*", "fin:*", "sea:*"},
		"the consultant's org name only": {"org:ProposeOnboarding"},
		"no sea name":                    {"org:ProposeOnboarding", "fin:ConfigureSettings"},
		"no fin name":                    {"org:ProposeOnboarding", "sea:ConfigureSettings"},
		"the owner's names":              owner,
	} {
		_, err := d.onboarding.ProposeOnboarding(d.as("ann", perms...), propose)
		saidKind(t, name, err, orgapi.ErrUnauthorized)
	}
	_, err := d.onboarding.ProposeOnboarding(d.as("ann", "org:ProposeOnboarding", "fin:ConfigureSettings", "sea:ConfigureSettings", "*"), propose)
	d.must("the consultant with the three names", err)
	d.must("the same proposal again", func() error {
		_, err := d.onboarding.ProposeOnboarding(d.as("ann", consultant...), propose)
		return err
	}())
	// the consultant acting for another tenant or actor is not the principal of the command
	_, err = d.onboarding.ProposeOnboarding(d.as("eve", consultant...), propose)
	saidKind(t, "another actor's principal", err, orgapi.ErrUnauthorized)

	confirm := d.confirmCommand("ob-1")
	for name, perms := range map[string][]string{
		"the wildcards": {"*", "org:*", "fin:*", "sea:*"}, "no org name": {"fin:ApproveSettings", "sea:ApproveSettings"}, "no fin name": {"org:ConfirmOnboarding", "sea:ApproveSettings"},
		"no sea name": {"org:ConfirmOnboarding", "fin:ApproveSettings"}, "the consultant's names": consultant,
	} {
		_, err = d.onboarding.ConfirmOnboarding(d.as("olga", perms...), confirm)
		saidKind(t, name, err, orgapi.ErrUnauthorized)
	}
	if o, err := d.org.GetOnboarding(context.Background(), orgapi.GetOnboarding{TenantID: d.tenant, OnboardingID: "ob-1"}); err != nil || o.State != "PROPOSED" {
		t.Fatalf("after the refusals: %+v, %v", o, err)
	}
	// the consultant cannot confirm his own proposal even holding the owner's names too
	_, err = d.onboarding.ConfirmOnboarding(d.as("ann", append(append([]string{}, consultant...), owner...)...), orgapi.ConfirmOnboarding{CommandID: "c-ann", TenantID: d.tenant, ActorID: "ann",
		OnboardingID: "ob-1", AcknowledgedText: riskStatement})
	saidKind(t, "the proposer confirming", err, orgapi.ErrRejected, "may not confirm it", "appointed owner")
	if f, s := d.count(); f != 0 || s != 0 {
		t.Fatalf("change sets made by refused confirmations: %d, %d", f, s)
	}
}

// A profile and its overrides are checked with FIN's and SEA's own rules when the consultant proposes, not when the owner
// confirms: an override that breaks a rule across items is refused naming the item.
func TestAnInvalidProfileOrOverrideIsRefusedWhenProposed(t *testing.T) {
	d := newSettingsDay(t, "cargo-onb-invalid-")
	_, err := d.propose("ob-1", "STANDARD", 3, false, map[string][]orgapi.OnboardingEntry{"FIN": {{ScopeKind: "TENANT", Key: "fin.F8", Value: "DEFAULT_TO_REMAINING"}}})
	saidKind(t, "F8 without F3", err, orgapi.ErrRejected, "fin.F8", "fin.F3", "not a valid configuration")
	_, err = d.propose("ob-1", "STANDARD", 3, false, map[string][]orgapi.OnboardingEntry{"SEA": {{ScopeKind: "TENANT", Key: "sea.S1", Value: "NO_SUCH_VALUE"}}})
	saidKind(t, "an unknown value", err, orgapi.ErrRejected, "NO_SUCH_VALUE")
	_, err = d.propose("ob-1", "STANDARD", 3, false, map[string][]orgapi.OnboardingEntry{"FIN": {{ScopeKind: "TENANT", Key: "fin.F1", Value: "NOT_ABOVE_THRESHOLD"}}})
	saidKind(t, "a threshold at the company", err, orgapi.ErrRejected, "fin.F1")
	_, err = d.propose("ob-1", "STANDARD", 3, false, map[string][]orgapi.OnboardingEntry{"SEA": {{ScopeKind: "TENANT", Key: "sea.S1", Value: "WITH_REASON", ThresholdMinor: new(int64), ThresholdCurrency: "CNY"}}})
	saidKind(t, "a threshold for SEA", err, orgapi.ErrRejected, "no threshold")
	_, err = d.propose("ob-1", "FANCY", 3, false, nil)
	saidKind(t, "another profile", err, orgapi.ErrRejected, "STANDARD, STRICT and LEAN")
	_, err = d.propose("ob-1", "STANDARD", 3, false, map[string][]orgapi.OnboardingEntry{"AIR": nil})
	saidKind(t, "another module", err, orgapi.ErrRejected, "FIN and SEA")
	// a valid override is accepted: F8 with F3 in the same overrides
	_, err = d.propose("ob-1", "STANDARD", 3, false, map[string][]orgapi.OnboardingEntry{"FIN": {{ScopeKind: "TENANT", Key: "fin.F8", Value: "DEFAULT_TO_REMAINING"},
		{ScopeKind: "TENANT", Key: "fin.F3", Value: "ONLY_WHEN_AMOUNT_MOVES"}}})
	d.must("F8 with F3", err)
}

// After the owner's confirmation each module gets its change set from the profile, proposed by the consultant and approved
// by the owner through the modules' own commands: STRICT only tightens, so each module approves it when it is proposed;
// LEAN afterwards loosens, so each waits for the owner, who is not the consultant. Each change set carries the
// onboarding's name, the record keeps it and its state, and a retry makes nothing twice.
func TestAnOnboardingAppliesItsProfileInEachModule(t *testing.T) {
	d := newSettingsDay(t, "cargo-onb-apply-")
	r, err := d.propose("ob-1", "STRICT", 2, false, nil)
	if err != nil || !strings.Contains(r.Notice, "at least 3 finance staff") || r.State != "PROPOSED" {
		t.Fatalf("the proposal: %+v, %v", r, err)
	}
	if f, s := d.count(); f != 0 || s != 0 {
		t.Fatalf("a proposal changes no module: %d, %d", f, s)
	}
	got, err := d.confirm("ob-1")
	if err != nil || got.Receipt.State != "COMPLETE" || len(got.Failures) != 0 || !strings.Contains(got.Receipt.Notice, "at least 3 finance staff") {
		t.Fatalf("the confirmation: %+v, %v", got, err)
	}
	fin := d.finValues("TENANT", "")
	for key, want := range map[string]string{"fin.F1": "NOT_ALLOWED", "fin.F2": "SUBMITTER_AND_PREVIEWER", "fin.F3": "EVERY_ATTRIBUTION", "fin.F4": "ALL_PREVIEWED", "fin.F6": "SEPARATE",
		"fin.F7": "BLOCKS", "fin.F8": "REJECT_WITH_DEFAULT", "fin.F9": "RECONFIRM", "version": "1"} {
		if fin[key] != want {
			t.Errorf("FIN %s is %s, want %s", key, fin[key], want)
		}
	}
	if v, version := d.seaValue("TENANT", ""); v != "SUPERVISOR_ONLY" || version != 1 {
		t.Fatalf("SEA's S1: %s, version %d", v, version)
	}
	c, ok := d.finChange("onb-ob-1-fin")
	if !ok || c.ProposedBy != "ann" || !c.AutoApproved || c.Loosening || !strings.Contains(c.Reason, "ob-1") || len(c.Entries) != 8 {
		t.Fatalf("FIN's change set: %+v", c)
	}
	o := got.Onboarding
	if o.State != "COMPLETE" || o.ConfirmedBy != "olga" || o.AcknowledgedText != riskStatement || o.Modules[0].ChangeID != "onb-ob-1-fin" || o.Modules[0].State != "APPROVED" ||
		o.Modules[1].ChangeID != "onb-ob-1-sea" || o.Modules[1].State != "APPROVED" {
		t.Fatalf("the record: %+v", o)
	}
	// the same confirmation again answers the record and makes nothing: one change set in each module
	again, err := d.confirm("ob-1")
	if err != nil || again.Receipt.State != "COMPLETE" {
		t.Fatalf("a retry: %+v, %v", again, err)
	}
	if f, s := d.count(); f != 1 || s != 1 {
		t.Fatalf("change sets after a retry: %d, %d", f, s)
	}
	// LEAN loosens FIN (F1, F3, F4, F8) and SEA (S1) after STRICT: each module's change set waits for the owner, who approves it
	if _, err = d.propose("ob-2", "LEAN", 1, false, nil); err != nil {
		t.Fatal(err)
	}
	if got, err = d.confirm("ob-2"); err != nil || got.Receipt.State != "COMPLETE" {
		t.Fatalf("LEAN: %+v, %v", got, err)
	}
	fin = d.finValues("TENANT", "")
	for key, want := range map[string]string{"fin.F1": "UNRESTRICTED", "fin.F2": "SUBMITTER_ONLY", "fin.F3": "ONLY_WHEN_AMOUNT_MOVES", "fin.F4": "ONE_STEP_WITHOUT_OPENED_OR_LEFT",
		"fin.F7": "DOES_NOT_BLOCK", "fin.F8": "DEFAULT_TO_REMAINING", "fin.F9": "KEEP_AMOUNT", "version": "2"} {
		if fin[key] != want {
			t.Errorf("FIN %s is %s, want %s", key, fin[key], want)
		}
	}
	if v, version := d.seaValue("TENANT", ""); v != "WITH_REASON" || version != 2 {
		t.Fatalf("SEA's S1: %s, version %d", v, version)
	}
	if c, _ = d.finChange("onb-ob-2-fin"); c.ProposedBy != "ann" || c.ApprovedBy != "olga" || c.AutoApproved || !c.Loosening || c.RiskAcknowledgement != "" {
		t.Fatalf("FIN's loosening: %+v", c)
	}
	list, err := d.sea.ListSeaSettingsChanges(context.Background(), seaapi.ListSeaSettingsChanges{TenantID: d.tenant})
	d.must("SEA's change sets", err)
	if len(list) != 2 || list[1].ChangeID != "onb-ob-2-sea" || list[1].ProposedBy != "ann" || list[1].ApprovedBy != "olga" || !list[1].Loosening {
		t.Fatalf("SEA's change sets: %+v", list)
	}
	// STANDARD is every default: it tightens what LEAN loosened, and each module approves it when it is proposed
	if _, err = d.propose("ob-3", "STANDARD", 1, false, nil); err != nil {
		t.Fatal(err)
	}
	if got, err = d.confirm("ob-3"); err != nil || got.Receipt.State != "COMPLETE" {
		t.Fatalf("STANDARD: %+v, %v", got, err)
	}
	if fin = d.finValues("TENANT", ""); fin["fin.F2"] != "SUBMITTER_ONLY" || fin["fin.F1"] != "NOT_ALLOWED" || fin["version"] != "3" {
		t.Fatalf("FIN after STANDARD: %+v", fin)
	}
	// ORG lists the three onboardings in the order proposed, each complete
	all, err := d.org.ListOnboardings(context.Background(), orgapi.ListOnboardings{TenantID: d.tenant})
	if err != nil || len(all) != 3 || all[0].OnboardingID != "ob-1" || all[2].State != "COMPLETE" {
		t.Fatalf("the onboardings: %+v, %v", all, err)
	}
}

// failingFIN makes FIN's approval fail once, as a module that fails after it has proposed.
type failingFIN struct {
	FINSettings
	fails int
}

func (f *failingFIN) ApproveFinSettingsChange(ctx context.Context, cmd finapi.ApproveFinSettingsChange) (finapi.Receipt, error) {
	if f.fails > 0 {
		f.fails--
		return finapi.Receipt{}, errors.New("FIN is down for a moment")
	}
	return f.FINSettings.ApproveFinSettingsChange(ctx, cmd)
}

// A module that fails does not stop the other, and a retry of the same confirmation completes it without a second change
// set: SEA first refuses a legal entity it does not know (the way out is its HBL series), then, with the series, takes the
// same change set; FIN proposes, fails to approve once, and finishes on the retry with the change set it already made.
func TestARetryCompletesAModuleThatFailedAndMakesNoSecondChangeSet(t *testing.T) {
	d := newSettingsDay(t, "cargo-onb-retry-")
	d.legalEntity()
	// the legal entity LE-X is one SEA does not know yet
	overrides := map[string][]orgapi.OnboardingEntry{"SEA": {{ScopeKind: "LEGAL_ENTITY", ScopeID: "LE-X", Key: "sea.S1", Value: "WITH_REASON"}}}
	if _, err := d.propose("ob-1", "STRICT", 5, false, overrides); err != nil {
		t.Fatal(err)
	}
	got, err := d.confirm("ob-1")
	saidKind(t, "SEA with an unknown legal entity", err, ErrOnboardingIncomplete, "SEA:", "RegisterHblSeries", "send the same ConfirmOnboarding again")
	if len(got.Failures) != 1 || got.Failures[0].Module != "SEA" || got.Receipt.State != "CONFIRMED" {
		t.Fatalf("the confirmation: %+v", got)
	}
	if got.Onboarding.Modules[0].State != "APPROVED" || got.Onboarding.Modules[1].State != "NOT_STARTED" {
		t.Fatalf("the modules: %+v", got.Onboarding.Modules)
	}
	if f, s := d.count(); f != 1 || s != 0 {
		t.Fatalf("FIN committed on its own, SEA made nothing: %d, %d", f, s)
	}
	// the way out the refusal names, then the same command again
	_, err = d.sea.RegisterHblSeries(d.as("ops-1", "sea:*"), seaapi.RegisterHblSeries{CommandID: d.command(), TenantID: d.tenant, ActorID: "ops-1", LegalEntityID: "LE-X",
		Template: "SHAX{seq:7}", FirstNumber: 1, OccurredAt: time.Now().UTC()})
	d.must("SEA's series of LE-X", err)
	if got, err = d.confirm("ob-1"); err != nil || got.Receipt.State != "COMPLETE" || len(got.Failures) != 0 {
		t.Fatalf("the retry: %+v, %v", got, err)
	}
	if f, s := d.count(); f != 1 || s != 1 {
		t.Fatalf("change sets after the retry: FIN %d, SEA %d", f, s)
	}
	if v, _ := d.seaValue("LEGAL_ENTITY", "LE-X"); v != "WITH_REASON" {
		t.Fatalf("LE-X's S1: %s", v)
	}

	// a module that proposed and failed to approve: LEAN after STRICT waits for the owner, and FIN's approval fails once
	flaky := &failingFIN{FINSettings: d.fin, fails: 1}
	onboarding, err := NewOnboarding(d.org, flaky, d.sea)
	d.must("the onboarding", err)
	if _, err = d.propose("ob-2", "LEAN", 1, false, nil); err != nil {
		t.Fatal(err)
	}
	got, err = onboarding.ConfirmOnboarding(d.as("olga", owner...), d.confirmCommand("ob-2"))
	saidKind(t, "FIN's failing approval", err, ErrOnboardingIncomplete, "FIN:", "FIN is down")
	if got.Onboarding.Modules[0].State != "PROPOSED" || got.Onboarding.Modules[1].State != "APPROVED" {
		t.Fatalf("FIN proposed and SEA approved: %+v", got.Onboarding.Modules)
	}
	if c, ok := d.finChange("onb-ob-2-fin"); !ok || c.Status != finapi.SettingsProposed {
		t.Fatalf("FIN's change set waits: %+v", c)
	}
	if got, err = onboarding.ConfirmOnboarding(d.as("olga", owner...), d.confirmCommand("ob-2")); err != nil || got.Receipt.State != "COMPLETE" {
		t.Fatalf("the retry: %+v, %v", got, err)
	}
	if f, s := d.count(); f != 2 || s != 2 {
		t.Fatalf("change sets: FIN %d, SEA %d, want 2 and 2 (one each for ob-1, one each for ob-2)", f, s)
	}
	if c, _ := d.finChange("onb-ob-2-fin"); c.Status != finapi.SettingsInForce || c.ApprovedBy != "olga" {
		t.Fatalf("FIN's change set: %+v", c)
	}
}
