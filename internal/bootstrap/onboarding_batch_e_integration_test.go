//go:build integration

package bootstrap

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
)

// V2.102 batch E, the corrections after the second review, with the real ORG, FIN and SEA over one database and the platform's
// own permissions. ann is the consultant, olga the owner, dan and eve single administrators the owner names.

var bothAdmin = append(append([]string{}, adminFin...), adminSea...)

// loosen proposes, as who, a loosening of F7 in FIN and of S1 in SEA under the ids "<kind>-<tag>", each followed by a tightening
// back (which the proposer approves as it is proposed), and answers what approving each as who with an acknowledgement says.
func (d *settingsDay) loosen(who, tag string) (finErr, seaErr error) {
	d.t.Helper()
	_, err := d.fin.ProposeFinSettingsChange(d.as(who, bothAdmin...), finapi.ProposeFinSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: who, ChangeID: "f-" + tag,
		Entries: []finapi.FinSettingEntry{{ScopeKind: "TENANT", Key: "fin.F7", Value: "DOES_NOT_BLOCK"}}, Reason: "loosen " + tag})
	d.must("FIN's loosening "+tag, err)
	_, err = d.sea.ProposeSeaSettingsChange(d.as(who, bothAdmin...), seaapi.ProposeSeaSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: who, ChangeID: "s-" + tag,
		Entries: []seaapi.SeaSettingEntry{{ScopeKind: "TENANT", Key: "sea.S1", Value: "WITH_REASON"}}, Reason: "loosen " + tag})
	d.must("SEA's loosening "+tag, err)
	_, finErr = d.fin.ApproveFinSettingsChange(d.as(who, bothAdmin...), finapi.ApproveFinSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: who, ChangeID: "f-" + tag,
		RiskAcknowledgement: "I accept the risk."})
	_, seaErr = d.sea.ApproveSeaSettingsChange(d.as(who, bothAdmin...), seaapi.ApproveSeaSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: who, ChangeID: "s-" + tag,
		RiskAcknowledgement: "I accept the risk."})
	return finErr, seaErr
}

// tightenBack makes F7 and S1 strict again, as who, so that the next loosening loosens.
func (d *settingsDay) tightenBack(who, tag string) {
	d.t.Helper()
	_, err := d.fin.ProposeFinSettingsChange(d.as(who, bothAdmin...), finapi.ProposeFinSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: who, ChangeID: "tf-" + tag,
		Entries: []finapi.FinSettingEntry{{ScopeKind: "TENANT", Key: "fin.F7", Value: "BLOCKS"}}, Reason: "tighten " + tag})
	d.must("FIN's tightening "+tag, err)
	_, err = d.sea.ProposeSeaSettingsChange(d.as(who, bothAdmin...), seaapi.ProposeSeaSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: who, ChangeID: "ts-" + tag,
		Entries: []seaapi.SeaSettingEntry{{ScopeKind: "TENANT", Key: "sea.S1", Value: "SUPERVISOR_ONLY"}}, Reason: "tighten " + tag})
	d.must("SEA's tightening "+tag, err)
}

// R2-F1: a token that carries the declaration's permission declares nothing, in FIN and in SEA, alone or with every other name
// (the review's probe B): the declaration, for an onboarding ORG never heard of, is refused, and so is the self-approval it
// was meant to allow.
func TestATokenThatCarriesTheDeclarationsNameDeclaresNothing(t *testing.T) {
	d := newSettingsDay(t, "cargo-onb-e1-")
	for name, perms := range map[string][]string{
		"the name alone": {"fin:DeclareSingleAdministrator", "sea:DeclareSingleAdministrator"},
		"the name with the configurer's and the approver's": {"fin:ConfigureSettings", "fin:ApproveSettings", "fin:DeclareSingleAdministrator", "sea:ConfigureSettings", "sea:ApproveSettings", "sea:DeclareSingleAdministrator"},
		"every name": {"*", "fin:*", "sea:*", "org:*", "org:ProposeOnboarding", "org:ConfirmOnboarding", "org:RecordOnboardingModule", "fin:ConfigureSettings", "fin:ApproveSettings",
			"fin:DeclareSingleAdministrator", "sea:ConfigureSettings", "sea:ApproveSettings", "sea:DeclareSingleAdministrator"},
	} {
		_, err := d.fin.DeclareFinSingleAdministrator(d.as("mal", perms...), finapi.DeclareFinSingleAdministrator{CommandID: d.command(), TenantID: d.tenant, ActorID: "mal",
			OnboardingID: "fake", ProposedBy: "x", AdministratorID: "mal"})
		saidKind(t, "FIN, "+name, err, finapi.ErrUnauthorized, "platform's own")
		_, err = d.sea.DeclareSeaSingleAdministrator(d.as("mal", perms...), seaapi.DeclareSeaSingleAdministrator{CommandID: d.command(), TenantID: d.tenant, ActorID: "mal",
			OnboardingID: "fake", ProposedBy: "x", AdministratorID: "mal"})
		saidKind(t, "SEA, "+name, err, seaapi.ErrUnauthorized, "platform's own")
	}
	// the self-approval that the declaration was meant to allow is refused, in both modules
	d.tightenBack("mal", "0")
	finErr, seaErr := d.loosen("mal", "1")
	saidKind(t, "FIN's self-approval", finErr, finapi.ErrRejected, "SETTINGS_SAME_ACTOR", "has not declared a single administrator")
	saidKind(t, "SEA's self-approval", seaErr, seaapi.ErrRejected, "SETTINGS_SAME_ACTOR", "has not declared a single administrator")
	now := time.Now().UTC()
	rep, err := d.tools.GetExceptionReport(d.as("olga", owner...), d.tenant, "olga", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil || len(rep.Fin.Declarations) != 0 || len(rep.Sea.Declarations) != 0 {
		t.Fatalf("declarations after the refusals: %+v, %v", rep, err)
	}
}

// R2-F2: a tenant has one single administrator in force, the one its latest declaration names. A later onboarding that names
// another replaces the earlier one, who may no longer approve their own loosening; a later onboarding that declares none
// changes nothing. Both modules.
func TestATenantHasOneSingleAdministratorInForceTheLatestDeclarations(t *testing.T) {
	d := newSettingsDay(t, "cargo-onb-e2-")
	confirmNaming := func(id, admin string, single bool) {
		t.Helper()
		_, err := d.propose(id, "STRICT", 3, single, nil)
		d.must("propose "+id, err)
		c := d.confirmCommand(id)
		c.AdministratorID = admin
		got, err := d.onboarding.ConfirmOnboarding(d.as("olga", owner...), c)
		if err != nil || got.Receipt.State != "COMPLETE" {
			t.Fatalf("confirm %s: %+v, %v", id, got, err)
		}
	}
	allowed := func(who, tag string) {
		t.Helper()
		d.tightenBack(who, tag)
		finErr, seaErr := d.loosen(who, tag)
		if finErr != nil || seaErr != nil {
			t.Fatalf("%s %s may approve their own loosening: FIN %v, SEA %v", who, tag, finErr, seaErr)
		}
	}
	refused := func(who, tag, named string) {
		t.Helper()
		d.tightenBack(who, tag)
		finErr, seaErr := d.loosen(who, tag)
		saidKind(t, "FIN, "+who+" "+tag, finErr, finapi.ErrRejected, "SETTINGS_SAME_ACTOR", "not the single administrator this tenant declared ("+named+")")
		saidKind(t, "SEA, "+who+" "+tag, seaErr, seaapi.ErrRejected, "SETTINGS_SAME_ACTOR", "not the single administrator this tenant declared ("+named+")")
	}
	confirmNaming("ob-1", "dan", true)
	allowed("dan", "a")
	confirmNaming("ob-2", "eve", true) // replaces dan from its declaration's time
	refused("dan", "b", "eve")
	allowed("eve", "c")
	confirmNaming("ob-3", "", false) // declares none: eve stays, dan stays replaced
	refused("dan", "d", "eve")
	allowed("eve", "e")
	// the exception report lists both declarations: visible, and the latest is the one in force
	now := time.Now().UTC()
	rep, err := d.tools.GetExceptionReport(d.as("olga", owner...), d.tenant, "olga", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil || len(rep.Fin.Declarations) != 2 || len(rep.Sea.Declarations) != 2 {
		t.Fatalf("the declarations: %+v, %v", rep, err)
	}
}

// R2-F3: ORG's own ProposeOnboarding and ConfirmOnboarding run only under the platform's mark: a person who holds their names, and
// every other name, is refused when calling ORG directly (the review's probe A), so that the only way in is the onboarding, which
// checks the plan and the consultant. The people's permissions stay the people's: the onboarding asks for them by name first.
func TestOrgsOnboardingCommandsRunOnlyThroughThePlatformsOnboarding(t *testing.T) {
	d := newSettingsDay(t, "cargo-onb-e3-")
	fc, err := d.fin.GetFinSettingsCatalogue(context.Background(), finapi.GetFinSettingsCatalogue{})
	d.must("FIN's catalogue", err)
	sc, err := d.sea.GetSeaSettingsCatalogue(context.Background(), seaapi.GetSeaSettingsCatalogue{})
	d.must("SEA's catalogue", err)
	plan := map[string]orgapi.ModulePlan{
		"FIN": {CatalogueVersion: fc.Version, Entries: []orgapi.OnboardingEntry{{ScopeKind: "TENANT", Key: "fin.F7", Value: "DOES_NOT_BLOCK"}}},
		"SEA": {CatalogueVersion: sc.Version, Entries: []orgapi.OnboardingEntry{{ScopeKind: "TENANT", Key: "sea.S1", Value: "WITH_REASON"}}}}
	every := append(append([]string{"*", "org:*"}, consultant...), owner...)
	for name, perms := range map[string][]string{"the name alone": {"org:ProposeOnboarding"}, "every name": every} {
		_, err := d.org.ProposeOnboarding(d.as("mallory", perms...), orgapi.ProposeOnboarding{CommandID: "p-" + name, TenantID: d.tenant, ActorID: "mallory", OnboardingID: "ob-x",
			Profile: "STRICT", Plan: plan, RiskStatement: riskStatement, FinanceStaff: 5})
		saidKind(t, "ORG's ProposeOnboarding, "+name, err, orgapi.ErrUnauthorized, "platform's own")
	}
	if _, err := d.org.GetOnboarding(context.Background(), orgapi.GetOnboarding{TenantID: d.tenant, OnboardingID: "ob-x"}); !errors.Is(err, orgapi.ErrNotFound) {
		t.Fatalf("a refused proposal was recorded: %v", err)
	}
	_, err = d.propose("ob-1", "STRICT", 3, false, nil)
	d.must("the consultant's proposal through the onboarding", err)
	for name, perms := range map[string][]string{"the name alone": {"org:ConfirmOnboarding"}, "every name": every} {
		_, err := d.org.ConfirmOnboarding(d.as("olga", perms...), d.confirmCommand("ob-1"))
		saidKind(t, "ORG's ConfirmOnboarding, "+name, err, orgapi.ErrUnauthorized, "platform's own")
	}
	if o, _ := d.org.GetOnboarding(context.Background(), orgapi.GetOnboarding{TenantID: d.tenant, OnboardingID: "ob-1"}); o.State != "PROPOSED" {
		t.Fatalf("a refused confirmation was recorded: %+v", o)
	}
	// the onboarding asks for the person's org permission by name before it calls ORG, and nothing is called without it
	_, err = d.onboarding.ProposeOnboarding(d.as("ann", "fin:ConfigureSettings", "sea:ConfigureSettings", "org:*", "*"), orgapi.ProposeOnboarding{CommandID: "p-2", TenantID: d.tenant, ActorID: "ann",
		OnboardingID: "ob-2", Profile: "STRICT", RiskStatement: riskStatement, FinanceStaff: 3})
	saidKind(t, "no org:ProposeOnboarding by name", err, orgapi.ErrUnauthorized, "holds org:ProposeOnboarding by name")
	_, err = d.onboarding.ConfirmOnboarding(d.as("olga", "fin:ApproveSettings", "sea:ApproveSettings", "org:*", "*"), d.confirmCommand("ob-1"))
	saidKind(t, "no org:ConfirmOnboarding by name", err, orgapi.ErrUnauthorized, "holds org:ConfirmOnboarding by name")
	if got, err := d.confirm("ob-1"); err != nil || got.Receipt.State != "COMPLETE" {
		t.Fatalf("the confirmation through the onboarding: %+v, %v", got, err)
	}
}

// R2-F4: the proposal's receipt carries the plan and its SHA-256, and the confirmation carries the digest of the plan the owner
// read; another, or none, is refused with the way out, and nothing is recorded.
func TestTheOwnerConfirmsThePlanTheyRead(t *testing.T) {
	d := newSettingsDay(t, "cargo-onb-e4-")
	r, err := d.propose("ob-1", "STRICT", 3, false, map[string][]orgapi.OnboardingEntry{"FIN": {{ScopeKind: "TENANT", Key: "fin.F9", Value: "KEEP_AMOUNT"}}})
	d.must("the proposal", err)
	if len(r.Plan["FIN"].Entries) == 0 || len(r.Plan["SEA"].Entries) == 0 || r.Plan["FIN"].CatalogueVersion == "" || r.PlanSHA256 != orgapi.PlanSHA256(r.Plan) || len(r.PlanSHA256) != 64 {
		t.Fatalf("the proposal's receipt: %+v", r)
	}
	o, err := d.org.GetOnboarding(context.Background(), orgapi.GetOnboarding{TenantID: d.tenant, OnboardingID: "ob-1"})
	d.must("GetOnboarding", err)
	if !reflect.DeepEqual(o.Plan, r.Plan) || o.PlanSHA256 != r.PlanSHA256 {
		t.Fatalf("GetOnboarding shows another plan than the receipt:\n%+v\n%+v", o.Plan, r.Plan)
	}
	again, err := d.propose("ob-1", "STRICT", 3, false, map[string][]orgapi.OnboardingEntry{"FIN": {{ScopeKind: "TENANT", Key: "fin.F9", Value: "KEEP_AMOUNT"}}})
	if err != nil || again.PlanSHA256 != r.PlanSHA256 {
		t.Fatalf("a replay: %+v, %v", again, err)
	}
	c := d.confirmCommand("ob-1")
	for name, digest := range map[string]string{"none": "", "another": strings.Repeat("0", 64), "the right one in capitals": strings.ToUpper(r.PlanSHA256), "the right one padded": r.PlanSHA256 + " "} {
		c.PlanSHA256 = digest
		_, err := d.onboarding.ConfirmOnboarding(d.as("olga", owner...), c)
		saidKind(t, "the digest: "+name, err, orgapi.ErrRejected, "read the onboarding again", "GetOnboarding shows the plan and its PlanSHA256", "confirm what it shows")
	}
	if o, _ = d.org.GetOnboarding(context.Background(), orgapi.GetOnboarding{TenantID: d.tenant, OnboardingID: "ob-1"}); o.State != "PROPOSED" {
		t.Fatalf("a refused confirmation was recorded: %+v", o)
	}
	c.PlanSHA256 = r.PlanSHA256
	got, err := d.onboarding.ConfirmOnboarding(d.as("olga", owner...), c)
	if err != nil || got.Receipt.State != "COMPLETE" || got.Receipt.PlanSHA256 != r.PlanSHA256 || !reflect.DeepEqual(got.Receipt.Plan, r.Plan) {
		t.Fatalf("the confirmation of the plan read: %+v, %v", got, err)
	}
}

// otherBuild is FIN as another build of it would be: its catalogue is another version.
type otherBuild struct{ FINSettings }

func (o otherBuild) GetFinSettingsCatalogue(ctx context.Context, q finapi.GetFinSettingsCatalogue) (finapi.FinSettingsCatalogue, error) {
	c, err := o.FINSettings.GetFinSettingsCatalogue(ctx, q)
	c.Version = "fedcba9876543210"
	return c, err
}

// R2-F5: the build is checked, for both modules, before ORG records the owner's confirmation: when it is not the plan's nothing is
// recorded, the answer names both ways out and the owner confirms again with the right build; after the confirmation the same
// difference is not permanent, and the same confirmation completes the module with the right build. A replay of the same
// proposal across a deploy answers the stored receipt.
func TestTheBuildIsCheckedBeforeAnythingIsRecordedAndIsNotPermanent(t *testing.T) {
	d := newSettingsDay(t, "cargo-onb-e5-")
	r, err := d.propose("ob-1", "LEAN", 3, false, nil)
	d.must("the proposal", err)
	later, err := NewOnboarding(d.org, otherBuild{d.fin}, d.sea)
	d.must("the onboarding of another build", err)
	got, err := later.ConfirmOnboarding(d.as("olga", owner...), d.confirmCommand("ob-1"))
	saidKind(t, "FIN's catalogue changed", err, ErrOnboardingBuild, "the catalogue of FIN is version fedcba9876543210 in this build", "nothing is recorded",
		"run the build the plan was made with and confirm again", "propose a new onboarding under another id")
	saidKind(t, "the refusal is the business rules'", err, orgapi.ErrRejected)
	if errors.Is(err, ErrOnboardingBlocked) || strings.Contains(err.Error(), "permanent") || len(got.Failures) != 0 {
		t.Fatalf("a difference of build is not permanent: %v, %+v", err, got)
	}
	if o, _ := d.org.GetOnboarding(context.Background(), orgapi.GetOnboarding{TenantID: d.tenant, OnboardingID: "ob-1"}); o.State != "PROPOSED" || o.ConfirmedBy != "" {
		t.Fatalf("the owner's confirmation was recorded: %+v", o)
	}
	d.heldOnly("ob-1") // the refused confirmation approved nothing
	// SEA's catalogue is checked too: the module that is not FIN
	seaLater, err := NewOnboarding(d.org, d.fin, otherSeaBuild{d.sea})
	d.must("the onboarding of another SEA build", err)
	_, err = seaLater.ConfirmOnboarding(d.as("olga", owner...), d.confirmCommand("ob-1"))
	saidKind(t, "SEA's catalogue changed", err, ErrOnboardingBuild, "the catalogue of SEA is version", "nothing is recorded")
	if got, err = d.confirm("ob-1"); err != nil || got.Receipt.State != "COMPLETE" {
		t.Fatalf("the owner confirms again with the right build: %+v, %v", got, err)
	}

	// after the confirmation: the same difference is not permanent, and the right build completes the module (a day of its own: the
	// change set of FIN must loosen for the owner's approval to be the step that fails)
	d2 := newSettingsDay(t, "cargo-onb-e5c-")
	if _, err = d2.propose("ob-2", "LEAN", 3, false, nil); err != nil {
		t.Fatal(err)
	}
	flaky := &failingFIN{FINSettings: d2.fin, fails: 1}
	first, err := NewOnboarding(d2.org, flaky, d2.sea)
	d2.must("the onboarding", err)
	_, err = first.ConfirmOnboarding(d2.as("olga", owner...), d2.confirmCommand("ob-2"))
	saidKind(t, "FIN down for a moment", err, ErrOnboardingIncomplete)
	laterOnD2, err := NewOnboarding(d2.org, otherBuild{d2.fin}, d2.sea)
	d2.must("the onboarding of another build", err)
	got, err = laterOnD2.ConfirmOnboarding(d2.as("olga", owner...), d2.confirmCommand("ob-2"))
	saidKind(t, "the catalogue differs after the confirmation", err, ErrOnboardingIncomplete, "FIN:", "the owner's confirmation is recorded and stays",
		"run the build the plan was made with and send the same ConfirmOnboarding again", "propose a new onboarding under another id")
	if errors.Is(err, ErrOnboardingBlocked) || len(got.Failures) != 1 || got.Failures[0].Permanent {
		t.Fatalf("a difference of build after the confirmation is not permanent: %v, %+v", err, got.Failures)
	}
	if got, err = d2.onboarding.ConfirmOnboarding(d2.as("olga", owner...), d2.confirmCommand("ob-2")); err != nil || got.Receipt.State != "COMPLETE" {
		t.Fatalf("the same confirmation with the right build: %+v, %v", got, err)
	}
	if f, s := d2.count(); f != 1 || s != 1 {
		t.Fatalf("change sets: %d, %d, want 1 and 1", f, s)
	}

	// a replay of the same proposal (same command id, same command) after a deploy that resolves it otherwise answers the stored
	// receipt: the request's hash covers what the caller sent, not what the build resolved
	replay, err := later.ProposeOnboarding(d.as("ann", consultant...), orgapi.ProposeOnboarding{CommandID: "propose-ob-1", TenantID: d.tenant, ActorID: "ann", OnboardingID: "ob-1",
		Profile: "LEAN", RiskStatement: riskStatement, FinanceStaff: 3})
	if err != nil || !reflect.DeepEqual(replay.Plan, r.Plan) || replay.PlanSHA256 != r.PlanSHA256 || replay.Plan["FIN"].CatalogueVersion == "fedcba9876543210" {
		t.Fatalf("the replay across a deploy: %+v, %v; want the stored plan of %+v", replay, err, r)
	}
	// what the caller sent still counts: another profile under the same command id is a conflict
	_, err = later.ProposeOnboarding(d.as("ann", consultant...), orgapi.ProposeOnboarding{CommandID: "propose-ob-1", TenantID: d.tenant, ActorID: "ann", OnboardingID: "ob-1",
		Profile: "STRICT", RiskStatement: riskStatement, FinanceStaff: 3})
	saidKind(t, "another command under the same id", err, orgapi.ErrIdempotencyConflict)
}

// otherSeaBuild is SEA as another build of it would be.
type otherSeaBuild struct{ SEASettings }

func (o otherSeaBuild) GetSeaSettingsCatalogue(ctx context.Context, q seaapi.GetSeaSettingsCatalogue) (seaapi.SeaSettingsCatalogue, error) {
	c, err := o.SEASettings.GetSeaSettingsCatalogue(ctx, q)
	c.Version = "0011223344556677"
	return c, err
}

// R2-F5 and R2-F6: a stored plan that this build's own rules refuse (a rule that changed under the same catalogue version) is
// refused before the confirmation is recorded, naming both ways out, and is not permanent.
func TestAPlanThatThisBuildsRulesRefuseIsRefusedBeforeTheConfirmation(t *testing.T) {
	d := newSettingsDay(t, "cargo-onb-e5b-")
	fc, err := d.fin.GetFinSettingsCatalogue(context.Background(), finapi.GetFinSettingsCatalogue{})
	d.must("FIN's catalogue", err)
	sc, err := d.sea.GetSeaSettingsCatalogue(context.Background(), seaapi.GetSeaSettingsCatalogue{})
	d.must("SEA's catalogue", err)
	loose, err := d.platform.ORG(allowAll{})
	d.must("a loose ORG", err)
	// F8 without F3 breaks a rule across items: the build that made the plan accepted it, this one does not
	plan := map[string]orgapi.ModulePlan{
		"FIN": {CatalogueVersion: fc.Version, Entries: []orgapi.OnboardingEntry{{ScopeKind: "TENANT", Key: "fin.F8", Value: "DEFAULT_TO_REMAINING"}}},
		"SEA": {CatalogueVersion: sc.Version, Entries: []orgapi.OnboardingEntry{{ScopeKind: "TENANT", Key: "sea.S1", Value: "WITH_REASON"}}}}
	_, err = loose.ProposeOnboarding(d.as("ann", "*"), orgapi.ProposeOnboarding{CommandID: "p-1", TenantID: d.tenant, ActorID: "ann", OnboardingID: "ob-1", Profile: "STANDARD", Plan: plan,
		RiskStatement: riskStatement, FinanceStaff: 3})
	d.must("a plan this build refuses, proposed through a loose ORG", err)
	_, err = d.onboarding.ConfirmOnboarding(d.as("olga", owner...), d.confirmCommand("ob-1"))
	saidKind(t, "the build's rules", err, ErrOnboardingBuild, "this build's rules of FIN refuse the entries of the plan", "fin.F8", "nothing is recorded", "run the build the plan was made with",
		"propose a new onboarding under another id")
	if errors.Is(err, ErrOnboardingBlocked) {
		t.Fatalf("not permanent: %v", err)
	}
	if o, _ := d.org.GetOnboarding(context.Background(), orgapi.GetOnboarding{TenantID: d.tenant, OnboardingID: "ob-1"}); o.State != "PROPOSED" {
		t.Fatalf("the owner's confirmation was recorded: %+v", o)
	}
}

// callCounts counts what the onboarding calls of ORG and of FIN, to show that a refusal comes before anything is called.
type callCounts struct {
	mu sync.Mutex
	n  int
}

func (c *callCounts) hit() { c.mu.Lock(); c.n++; c.mu.Unlock() }

type countingORG struct {
	ORGOnboarding
	c *callCounts
}

func (o countingORG) ProposeOnboarding(ctx context.Context, cmd orgapi.ProposeOnboarding) (orgapi.OnboardingReceipt, error) {
	o.c.hit()
	return o.ORGOnboarding.ProposeOnboarding(ctx, cmd)
}
func (o countingORG) ConfirmOnboarding(ctx context.Context, cmd orgapi.ConfirmOnboarding) (orgapi.OnboardingReceipt, error) {
	o.c.hit()
	return o.ORGOnboarding.ConfirmOnboarding(ctx, cmd)
}
func (o countingORG) GetOnboarding(ctx context.Context, q orgapi.GetOnboarding) (orgapi.Onboarding, error) {
	o.c.hit()
	return o.ORGOnboarding.GetOnboarding(ctx, q)
}
func (o countingORG) RecordOnboardingModule(ctx context.Context, cmd orgapi.RecordOnboardingModule) (orgapi.OnboardingReceipt, error) {
	o.c.hit()
	return o.ORGOnboarding.RecordOnboardingModule(ctx, cmd)
}

type countingFIN struct {
	FINSettings
	c *callCounts
}

func (f countingFIN) GetFinSettingsCatalogue(ctx context.Context, q finapi.GetFinSettingsCatalogue) (finapi.FinSettingsCatalogue, error) {
	f.c.hit()
	return f.FINSettings.GetFinSettingsCatalogue(ctx, q)
}
func (f countingFIN) ProposeFinSettingsChange(ctx context.Context, cmd finapi.ProposeFinSettingsChange) (finapi.Receipt, error) {
	f.c.hit()
	return f.FINSettings.ProposeFinSettingsChange(ctx, cmd)
}
func (f countingFIN) DeclareFinSingleAdministrator(ctx context.Context, cmd finapi.DeclareFinSingleAdministrator) (finapi.Receipt, error) {
	f.c.hit()
	return f.FINSettings.DeclareFinSingleAdministrator(ctx, cmd)
}

// R2-F6: an actor id, or an administrator's, that is blank, padded or longer than 128 characters is refused before anything is
// called (the review's probe I), with a code and the way out; nothing is recorded, and the onboarding waits for a right id.
func TestAnActorIdNoModuleAcceptsIsRefusedBeforeAnythingIsCalled(t *testing.T) {
	d := newSettingsDay(t, "cargo-onb-e6-")
	calls := &callCounts{}
	counted, err := NewOnboarding(countingORG{d.org, calls}, countingFIN{d.fin, calls}, d.sea)
	d.must("the counting onboarding", err)
	bad := map[string]string{"blank": " ", "padded": " olga", "120 characters and 40 spaces": strings.Repeat("o", 120) + strings.Repeat(" ", 40), "129 characters": strings.Repeat("o", 129)}
	for name, id := range bad {
		_, err := counted.ProposeOnboarding(d.as(id, consultant...), orgapi.ProposeOnboarding{CommandID: d.command(), TenantID: d.tenant, ActorID: id, OnboardingID: "ob-x",
			Profile: "STANDARD", RiskStatement: riskStatement, FinanceStaff: 3})
		saidKind(t, "a consultant that is "+name, err, ErrInvalidActor, "ONBOARDING_INVALID_ACTOR", "give the actor id exactly as the identity platform names the person", "nothing is recorded")
	}
	if _, err = d.propose("ob-1", "STRICT", 3, true, nil); err != nil {
		t.Fatal(err)
	}
	calls.n = 0
	for name, id := range bad {
		c := d.confirmCommand("ob-1")
		c.ActorID = id
		_, err := counted.ConfirmOnboarding(d.as(id, owner...), c)
		saidKind(t, "an owner that is "+name, err, ErrInvalidActor, "ONBOARDING_INVALID_ACTOR", "the owner's actor id", "nothing is recorded")
		c = d.confirmCommand("ob-1")
		c.AdministratorID = id
		_, err = counted.ConfirmOnboarding(d.as("olga", owner...), c)
		saidKind(t, "an administrator that is "+name, err, ErrInvalidActor, "ONBOARDING_INVALID_ACTOR", "the administrator's actor id", "nothing is recorded")
	}
	if calls.n != 0 {
		t.Fatalf("%d calls were made before the refusals", calls.n)
	}
	if o, _ := d.org.GetOnboarding(context.Background(), orgapi.GetOnboarding{TenantID: d.tenant, OnboardingID: "ob-1"}); o.State != "PROPOSED" {
		t.Fatalf("a refused confirmation was recorded: %+v", o)
	}
	// an owner of exactly 128 characters is accepted, and no module fails for it
	long := strings.Repeat("o", 128)
	got, err := d.onboarding.ConfirmOnboarding(d.as(long, owner...), orgapi.ConfirmOnboarding{CommandID: "c-long", TenantID: d.tenant, ActorID: long, OnboardingID: "ob-1",
		AcknowledgedText: riskStatement, PlanSHA256: d.confirmCommand("ob-1").PlanSHA256, AdministratorID: "dan"})
	if err != nil || got.Receipt.State != "COMPLETE" {
		t.Fatalf("an owner of 128 characters: %+v, %v", got, err)
	}
}

// R2-F7: the diff of a partial file, one whose own leftOut names a module, says for that module that the file does not carry it
// and why, and that the diff answers nothing about it (the review's probe G).
func TestTheDiffOfAPartialFileSaysWhatTheFileLacks(t *testing.T) {
	a := newSettingsDay(t, "cargo-onb-e7a-")
	if _, err := a.propose("ob-1", "STRICT", 3, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.confirm("ob-1"); err != nil {
		t.Fatal(err)
	}
	part, err := a.tools.ExportSettingsJSON(a.as("sam", "sea:ConfigureSettings"), a.tenant, "sam") // a SEA-only exporter: FIN is left out
	a.must("the partial export", err)
	b := settingsDayOn(t, uniqueTenant("cargo-onb-e7b-"))
	seedORG(t, b.platform, b.tenant)
	diff, err := b.tools.DiffSettingsImport(b.as("bob", bothAdmin...), b.tenant, "bob", part)
	if err != nil || len(diff.Fin) != 0 || len(diff.Sea) == 0 || len(diff.LeftOut) != 1 || diff.LeftOut[0].Module != ModuleFIN {
		t.Fatalf("the diff of the partial file: %+v, %v", diff, err)
	}
	for _, want := range []string{"the file does not carry FIN", "its exporter might not see it", "fin:ConfigureSettings", "answers nothing about FIN's configuration", "does not mean that it matches", "export FIN again"} {
		if !strings.Contains(diff.LeftOut[0].Reason, want) {
			t.Errorf("the reason %q does not say %q", diff.LeftOut[0].Reason, want)
		}
	}
	// a caller who also lacks FIN is told both: why the caller cannot see it, and why the file lacks it
	seaOnly, err := b.tools.DiffSettingsImport(b.as("sam", "sea:ConfigureSettings"), b.tenant, "sam", part)
	if err != nil || len(seaOnly.LeftOut) != 2 {
		t.Fatalf("a SEA-only caller's diff of the partial file: %+v, %v", seaOnly, err)
	}
	// a full file says nothing of the kind
	full, err := a.tools.ExportSettingsJSON(a.as("bob", bothAdmin...), a.tenant, "bob")
	a.must("the full export", err)
	if diff, err = b.tools.DiffSettingsImport(b.as("bob", bothAdmin...), b.tenant, "bob", full); err != nil || len(diff.LeftOut) != 0 {
		t.Fatalf("the diff of a full file: %+v, %v", diff, err)
	}
	// a file that carries a module and, by hand, also lists it as left out does lack nothing: the diff says nothing of the kind
	edited := strings.Replace(string(full), `"modules": {`, `"leftOut": [{"module": "FIN", "reason": "edited by hand"}],
  "modules": {`, 1)
	if edited == string(full) {
		t.Fatal("the fixture: the file was not edited")
	}
	if diff, err = b.tools.DiffSettingsImport(b.as("bob", bothAdmin...), b.tenant, "bob", []byte(edited)); err != nil || len(diff.LeftOut) != 0 || len(diff.Fin) == 0 {
		t.Fatalf("the diff of a file that carries FIN and lists it as left out: %+v, %v", diff, err)
	}
}

// finEntriesOf is the plan's entries of a module as FIN's own entries.
func finEntriesOf(t *testing.T, plan orgapi.ModulePlan) []finapi.FinSettingEntry {
	t.Helper()
	f, _, err := storedEntries(ModuleFIN, entriesOfOnboarding(plan.Entries))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// R2-F8 (batch F: the proposal holds the change sets): the change set under the derived id is the onboarding's only when its proposer, its
// reason, its hold and its entries all agree with the onboarding's: one that differs in exactly one of the four, and agrees in the others, is
// not the onboarding's and is blocked with the way out; one that agrees in all four is taken for the onboarding's, and no second change set is made.
// The change set is made between ORG's record of the proposal and the platform's own proposal of it, which the same ProposeOnboarding then finds.
func TestAChangeSetUnderTheDerivedIdIsTheOnboardingsOnlyWhenProposerReasonHoldAndEntriesAllAgree(t *testing.T) {
	for _, c := range []struct {
		name          string
		by            string
		wrongReason   bool
		fewerEntries  bool
		notHeld       bool
		blocked       bool
		sayProposedBy string
		say           string
	}{
		{"the proposer differs", "bob", false, false, false, true, "proposed by bob", "proposed by bob, not by the consultant ann"},
		{"the reason differs", "ann", true, false, false, true, "", "its reason is not the onboarding's"},
		{"the entries differ", "ann", false, true, false, true, "", "its entries are not the plan's"},
		{"it is not held for the onboarding", "ann", false, false, true, true, "", "it is not held for this onboarding"},
		{"all four agree", "ann", false, false, false, false, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newSettingsDay(t, "cargo-onb-e8-")
			cmd := orgapi.ProposeOnboarding{CommandID: "propose-ob-1", TenantID: d.tenant, ActorID: "ann", OnboardingID: "ob-1", Profile: "LEAN", RiskStatement: riskStatement, FinanceStaff: 3}
			profile, _ := SettingsProfile("LEAN")
			plan, err := d.onboarding.resolve(context.Background(), profile, nil)
			d.must("the plan", err)
			cmd.Plan = plan
			if _, err = d.org.ProposeOnboarding(asPlatform(d.as("ann", "org:ProposeOnboarding")), cmd); err != nil { // ORG's record alone
				t.Fatal(err)
			}
			onb, err := d.org.GetOnboarding(context.Background(), orgapi.GetOnboarding{TenantID: d.tenant, OnboardingID: "ob-1"})
			d.must("the onboarding", err)
			reason := heldReason("ob-1", onb.ProposedBy, onb.ProposedAt)
			if c.wrongReason {
				reason = "another reason"
			}
			entries := finEntriesOf(t, onb.Plan["FIN"])
			if c.fewerEntries {
				entries = entries[:len(entries)-1]
			}
			hold := "ob-1"
			if c.notHeld {
				hold = ""
			}
			// the change set is made as the platform's onboarding would (held), or as an ordinary change set
			_, err = d.fin.ProposeFinSettingsChange(asPlatform(d.as(c.by, "fin:ConfigureSettings")), finapi.ProposeFinSettingsChange{CommandID: "someone-" + c.by, TenantID: d.tenant, ActorID: c.by,
				ChangeID: "onb-ob-1-fin", Entries: entries, Reason: reason, OnboardingID: hold})
			d.must("the change set under the derived id", err)
			_, err = d.onboarding.ProposeOnboarding(d.as("ann", consultant...), orgapi.ProposeOnboarding{CommandID: "propose-ob-1", TenantID: d.tenant, ActorID: "ann", OnboardingID: "ob-1",
				Profile: "LEAN", RiskStatement: riskStatement, FinanceStaff: 3})
			if c.blocked {
				saidKind(t, c.name, err, ErrOnboardingBlocked, "FIN:", "change set onb-ob-1-fin of FIN exists and is not this onboarding's", c.say, "propose a new onboarding under another id")
				return
			}
			d.must("the proposal that finds the onboarding's own change set", err)
			if f, s := d.count(); f != 1 || s != 1 {
				t.Fatalf("a second change set was made: %d, %d", f, s)
			}
			got, err := d.confirm("ob-1")
			if err != nil || got.Receipt.State != "COMPLETE" {
				t.Fatalf("%s: %+v, %v", c.name, got, err)
			}
		})
	}
}

// R2-F8 (batch F): the reason of a held change set names the onboarding and the consultant and the time of their proposal, and must fit the 500
// characters a change set's reason has whoever they are (an onboarding of 100 characters, a consultant of 128 bytes); the owner's confirmation
// adds nothing to it, since the approval is the module's own record, so the longest ids propose, and an owner of 128 bytes confirms.
func TestTheReasonOfAHeldChangeSetFitsTheLongestIds(t *testing.T) {
	d := newSettingsDay(t, "cargo-onb-e8b-")
	id, consultantID, ownerID := strings.Repeat("o", 100), strings.Repeat("c", 128), strings.Repeat("w", 128)
	if n := len(heldReason(id, consultantID, time.Now())); n > maxReasonOfAChangeSet || n < 300 {
		t.Fatalf("the longest reason is %d characters, and a change set's reason is at most %d", n, maxReasonOfAChangeSet)
	}
	r, err := d.onboarding.ProposeOnboarding(d.as(consultantID, consultant...), orgapi.ProposeOnboarding{CommandID: "p-long", TenantID: d.tenant, ActorID: consultantID,
		OnboardingID: id, Profile: "STANDARD", RiskStatement: riskStatement, FinanceStaff: 3})
	d.must("the longest ids", err)
	got, err := d.onboarding.ConfirmOnboarding(d.as(ownerID, owner...), orgapi.ConfirmOnboarding{CommandID: "c-long", TenantID: d.tenant, ActorID: ownerID, OnboardingID: id,
		AcknowledgedText: riskStatement, PlanSHA256: r.PlanSHA256})
	if err != nil || got.Receipt.State != "COMPLETE" {
		t.Fatalf("an owner of 128 bytes confirms: %+v, %v", got, err)
	}
}

// maxReasonOfAChangeSet is the longest reason a change set has (settings.MaxReason).
const maxReasonOfAChangeSet = 500
