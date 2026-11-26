//go:build integration

package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/foundation/identity"
)

// V2.102 batch D, the corrections after the independent review, with the real ORG, FIN and SEA over one database and the
// platform's own permissions. ann is the consultant, olga the owner, dan the single administrator the owner names.

// F5: the proposal stores what will be applied. The profile changes in the build between the proposal and the owner's
// confirmation (the profiles are data to be adjusted), and the confirmation still applies the stored entries; a module whose
// catalogue is another version by then is refused, the other module goes on, and the way out is a new onboarding.
func TestTheConfirmationAppliesWhatTheProposalStoredNotTheBuildsProfile(t *testing.T) {
	d := newSettingsDay(t, "cargo-onb-plan-")
	if _, err := d.propose("ob-1", "STRICT", 3, false, map[string][]orgapi.OnboardingEntry{
		"FIN": {{ScopeKind: "TENANT", Key: "fin.F9", Value: "KEEP_AMOUNT"}}}); err != nil {
		t.Fatal(err)
	}
	o, err := d.org.GetOnboarding(context.Background(), orgapi.GetOnboarding{TenantID: d.tenant, OnboardingID: "ob-1"})
	d.must("the onboarding", err)
	finCat, _ := d.fin.GetFinSettingsCatalogue(context.Background(), finapi.GetFinSettingsCatalogue{})
	seaCat, _ := d.sea.GetSeaSettingsCatalogue(context.Background(), seaapi.GetSeaSettingsCatalogue{})
	fin, sea := o.Plan[ModuleFIN], o.Plan[ModuleSEA]
	got := map[string]string{}
	for _, e := range fin.Entries {
		got[e.Key] = e.Value
	}
	if len(o.Plan) != 2 || fin.CatalogueVersion != finCat.Version || sea.CatalogueVersion != seaCat.Version || len(fin.Entries) != 8 || got["fin.F7"] != "BLOCKS" || got["fin.F9"] != "KEEP_AMOUNT" ||
		len(sea.Entries) != 1 || sea.Entries[0].Value != "SUPERVISOR_ONLY" {
		t.Fatalf("the stored plan: %+v", o.Plan)
	}
	// the build's STRICT now says something else: F7 does not block
	saved := slices.Clone(profiles)
	t.Cleanup(func() { profiles = saved })
	profiles = slices.Clone(saved)
	for i := range profiles {
		if profiles[i].ID == "STRICT" {
			profiles[i].Items = derived(map[string]string{"fin.F2": "SUBMITTER_AND_PREVIEWER", "fin.F7": "DOES_NOT_BLOCK", "sea.S1": "WITH_REASON"})
		}
	}
	if r, err := d.confirm("ob-1"); err != nil || r.Receipt.State != "COMPLETE" {
		t.Fatalf("the confirmation: %+v, %v", r, err)
	}
	f := d.finValues("TENANT", "")
	if f["fin.F7"] != "BLOCKS" || f["fin.F2"] != "SUBMITTER_AND_PREVIEWER" || f["fin.F9"] != "KEEP_AMOUNT" {
		t.Fatalf("FIN applied the build's profile and not the proposal's: %+v", f)
	}
	if v, _ := d.seaValue("TENANT", ""); v != "SUPERVISOR_ONLY" {
		t.Fatalf("SEA applied %s", v)
	}
	if c, ok := d.finChange("onb-ob-1-fin"); !ok || len(c.Entries) != 8 {
		t.Fatalf("FIN's change set: %+v", c)
	}
}

// otherCatalogue answers another version of FIN's catalogue, as a build deployed between the proposal and the confirmation would.
type otherCatalogue struct{ FINSettings }

func (o otherCatalogue) GetFinSettingsCatalogue(ctx context.Context, q finapi.GetFinSettingsCatalogue) (finapi.FinSettingsCatalogue, error) {
	c, err := o.FINSettings.GetFinSettingsCatalogue(ctx, q)
	c.Version = "0123456789abcdef"
	return c, err
}

func TestAModuleWhoseCatalogueChangedBeforeTheConfirmationIsRefusedWithTheWayOut(t *testing.T) {
	d := newSettingsDay(t, "cargo-onb-version-")
	if _, err := d.propose("ob-1", "LEAN", 3, false, nil); err != nil {
		t.Fatal(err)
	}
	later, err := NewOnboarding(d.org, otherCatalogue{d.fin}, d.sea)
	d.must("the onboarding", err)
	got, err := later.ConfirmOnboarding(d.as("olga", owner...), d.confirmCommand("ob-1"))
	// batch E (R2-F5): the build is checked before ORG records the confirmation, so nothing is recorded, no module goes on, and the
	// difference is not called permanent (the build the plan was made with cures it); the way out names both causes
	saidKind(t, "FIN's catalogue changed", err, ErrOnboardingBuild, "the catalogue of FIN is version 0123456789abcdef in this build", "nothing is recorded",
		"propose a new onboarding under another id")
	if len(got.Failures) != 0 || got.Receipt.State != "" || errors.Is(err, ErrOnboardingBlocked) {
		t.Fatalf("a difference of build before the confirmation records nothing and is not permanent: %+v, %v", got, err)
	}
	if o, _ := d.org.GetOnboarding(context.Background(), orgapi.GetOnboarding{TenantID: d.tenant, OnboardingID: "ob-1"}); o.State != "PROPOSED" {
		t.Fatalf("the owner's confirmation was recorded: %+v", o)
	}
	if f, s := d.count(); f != 0 || s != 0 {
		t.Fatalf("change sets: FIN %d, SEA %d", f, s)
	}
}

// F3 and F7: the owner declares the single administrator when confirming, by naming them; the declaration must agree with
// the proposal; FIN and SEA record the administrator; only that administrator approves their own loosening, with an
// acknowledgement; another proposer who holds both permissions is refused, in both modules.
func TestOnlyTheAdministratorTheOwnerNamedApprovesTheirOwnLoosening(t *testing.T) {
	d := newSettingsDay(t, "cargo-onb-admin-")
	if _, err := d.propose("ob-1", "STRICT", 3, true, nil); err != nil {
		t.Fatal(err)
	}
	c := d.confirmCommand("ob-1")
	for name, admin := range map[string]string{"none named": "", "the consultant": "ann"} {
		c.AdministratorID = admin
		_, err := d.onboarding.ConfirmOnboarding(d.as("olga", owner...), c)
		saidKind(t, name, err, orgapi.ErrRejected, "administrator")
	}
	if o, _ := d.org.GetOnboarding(context.Background(), orgapi.GetOnboarding{TenantID: d.tenant, OnboardingID: "ob-1"}); o.State != "PROPOSED" {
		t.Fatalf("a refused confirmation: %+v", o)
	}
	if _, err := d.propose("ob-2", "STRICT", 3, false, nil); err != nil {
		t.Fatal(err)
	}
	c2 := d.confirmCommand("ob-2")
	c2.AdministratorID = "dan"
	_, err := d.onboarding.ConfirmOnboarding(d.as("olga", owner...), c2)
	saidKind(t, "one named for a proposal that declares none", err, orgapi.ErrRejected, "leave AdministratorID empty")
	if got, err := d.confirm("ob-1"); err != nil || got.Receipt.State != "COMPLETE" || got.Onboarding.AdministratorID != "dan" {
		t.Fatalf("the confirmation: %+v, %v", got, err)
	}
	both := append(append([]string{}, adminFin...), adminSea...)
	now := time.Now().UTC()
	rep, err := d.tools.GetExceptionReport(d.as("olga", owner...), d.tenant, "olga", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil || len(rep.Fin.Declarations) != 1 || rep.Fin.Declarations[0].AdministratorID != "dan" || rep.Fin.Declarations[0].ConfirmedBy != "olga" || rep.Fin.Declarations[0].ProposedBy != "ann" ||
		len(rep.Sea.Declarations) != 1 || rep.Sea.Declarations[0].AdministratorID != "dan" {
		t.Fatalf("the declarations: %+v, %v", rep, err)
	}
	// STRICT tightened F7 and S1; each person proposes a loosening of both
	for _, who := range []string{"dan", "frank"} {
		_, err = d.fin.ProposeFinSettingsChange(d.as(who, both...), finapi.ProposeFinSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: who, ChangeID: "f-" + who,
			Entries: []finapi.FinSettingEntry{{ScopeKind: "TENANT", Key: "fin.F7", Value: "DOES_NOT_BLOCK"}}, Reason: "loosen " + who})
		d.must("FIN's loosening by "+who, err)
		_, err = d.sea.ProposeSeaSettingsChange(d.as(who, both...), seaapi.ProposeSeaSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: who, ChangeID: "s-" + who,
			Entries: []seaapi.SeaSettingEntry{{ScopeKind: "TENANT", Key: "sea.S1", Value: "WITH_REASON"}}, Reason: "loosen " + who})
		d.must("SEA's loosening by "+who, err)
	}
	for _, ack := range []string{"", "I accept the risk."} {
		_, err = d.fin.ApproveFinSettingsChange(d.as("frank", both...), finapi.ApproveFinSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "frank", ChangeID: "f-frank", RiskAcknowledgement: ack})
		saidKind(t, "FIN, a proposer who is not the administrator", err, finapi.ErrRejected, "SETTINGS_SAME_ACTOR", "not the single administrator this tenant declared (dan)")
		_, err = d.sea.ApproveSeaSettingsChange(d.as("frank", both...), seaapi.ApproveSeaSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "frank", ChangeID: "s-frank", RiskAcknowledgement: ack})
		saidKind(t, "SEA, a proposer who is not the administrator", err, seaapi.ErrRejected, "SETTINGS_SAME_ACTOR", "not the single administrator this tenant declared (dan)")
	}
	if _, err = d.fin.ApproveFinSettingsChange(d.as("dan", both...), finapi.ApproveFinSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "dan", ChangeID: "f-dan",
		RiskAcknowledgement: "I am the only administrator."}); err != nil {
		t.Fatalf("FIN, the administrator: %v", err)
	}
	if _, err = d.sea.ApproveSeaSettingsChange(d.as("dan", both...), seaapi.ApproveSeaSettingsChange{CommandID: d.command(), TenantID: d.tenant, ActorID: "dan", ChangeID: "s-dan",
		RiskAcknowledgement: "I am the only administrator."}); err != nil {
		t.Fatalf("SEA, the administrator: %v", err)
	}
}

// F2: the platform acts on the consultant's proposal, and the trail says so: each module's change set is proposed in the
// consultant's name and its reason names the onboarding, the consultant and the time of their proposal, and the owner and
// the time of the confirmation; the confirmation's receipt says the same and that the consultant's permissions were checked
// when they proposed.
func TestTheTrailSaysThePlatformActsOnTheConsultantsProposal(t *testing.T) {
	d := newSettingsDay(t, "cargo-onb-trail-")
	if _, err := d.propose("ob-1", "STRICT", 2, false, nil); err != nil {
		t.Fatal(err)
	}
	got, err := d.confirm("ob-1")
	d.must("the confirmation", err)
	o := got.Onboarding
	proposed, confirmed := o.ProposedAt.UTC().Format(time.RFC3339), o.ConfirmedAt.UTC().Format(time.RFC3339)
	c, ok := d.finChange("onb-ob-1-fin")
	if !ok || c.ProposedBy != "ann" {
		t.Fatalf("FIN's change set: %+v", c)
	}
	list, err := d.sea.ListSeaSettingsChanges(context.Background(), seaapi.ListSeaSettingsChanges{TenantID: d.tenant})
	d.must("SEA's change sets", err)
	for name, reason := range map[string]string{"FIN": c.Reason, "SEA": list[0].Reason} {
		for _, want := range []string{"onboarding ob-1", "proposed by ann at " + proposed, "confirmed by olga at " + confirmed, "the platform applies the proposal",
			"the consultant's permissions checked when they proposed"} {
			if !strings.Contains(reason, want) {
				t.Errorf("%s's reason %q does not say %q", name, reason, want)
			}
		}
	}
	for _, want := range []string{"the platform applies the proposal of ann made at " + proposed, "as confirmed by olga at " + confirmed, "permissions were checked when they proposed, not at the confirmation",
		"at least 3 finance staff"} {
		if !strings.Contains(got.Receipt.Notice, want) {
			t.Errorf("the receipt's notice %q does not say %q", got.Receipt.Notice, want)
		}
	}
	// the reason fits a change set's 500 characters whoever the owner is (up to 128 bytes): an id of 100 characters with a short
	// consultant does, and an id of 100 characters with a consultant of 128 does not, which is refused when proposed
	long := strings.Repeat("o", 100)
	if _, err = d.onboarding.ProposeOnboarding(d.as("ann", consultant...), orgapi.ProposeOnboarding{CommandID: "p-long", TenantID: d.tenant, ActorID: "ann", OnboardingID: long, Profile: "STANDARD",
		RiskStatement: riskStatement, FinanceStaff: 3}); err != nil {
		t.Fatalf("an onboarding id of 100 characters and a short consultant: %v", err)
	}
	wordy := strings.Repeat("c", 128)
	_, err = d.onboarding.ProposeOnboarding(d.as(wordy, consultant...), orgapi.ProposeOnboarding{CommandID: "p-long-2", TenantID: d.tenant, ActorID: wordy, OnboardingID: long, Profile: "STANDARD",
		RiskStatement: riskStatement, FinanceStaff: 3})
	saidKind(t, "an id and a consultant too long for the reason", err, orgapi.ErrRejected, "too long for the reason of the modules' change sets", "propose under a shorter onboarding id")
}

// failingRecords makes ORG's record of a module's APPROVED fail, as a crash between the module's approval and ORG's record.
type failingRecords struct {
	ORGOnboarding
	fails int
}

func (f *failingRecords) RecordOnboardingModule(ctx context.Context, cmd orgapi.RecordOnboardingModule) (orgapi.OnboardingReceipt, error) {
	if cmd.State == orgapi.ModuleApproved && f.fails > 0 {
		f.fails--
		return orgapi.OnboardingReceipt{}, errors.New("ORG is down for a moment")
	}
	return f.ORGOnboarding.RecordOnboardingModule(ctx, cmd)
}

// F1: only the platform records a module's progress, and it trusts the module and not ORG's record. The owner calling
// RecordOnboardingModule is refused (the review's probe B), also with the permission's name in the token (the platform's
// mark is not in a token); a crash between a module's approval and ORG's record is completed by a retry without a second
// change set; and a record that says APPROVED while the module made nothing (written by a loose ORG) is not believed.
func TestOnlyThePlatformRecordsAModulesProgressAndItTrustsTheModule(t *testing.T) {
	d := newSettingsDay(t, "cargo-onb-record-")
	if _, err := d.propose("ob-1", "STRICT", 3, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := d.org.ConfirmOnboarding(asPlatform(d.as("olga", "org:ConfirmOnboarding")), d.confirmCommand("ob-1")); err != nil {
		t.Fatal(err)
	}
	record := func(ctx context.Context, m, state string) error {
		_, err := d.org.RecordOnboardingModule(ctx, orgapi.RecordOnboardingModule{CommandID: d.command(), TenantID: d.tenant, ActorID: "olga", OnboardingID: "ob-1", Module: m,
			ChangeID: "onb-ob-1-" + strings.ToLower(m), State: state})
		return err
	}
	for name, perms := range map[string][]string{"the owner's names": owner, "the wildcards": {"*", "org:*", "fin:*", "sea:*"}, "the permission's name in a token": {"org:RecordOnboardingModule"},
		"every name in a token": append(append([]string{"org:RecordOnboardingModule"}, owner...), consultant...)} {
		for _, m := range []string{"FIN", "SEA"} {
			err := record(d.as("olga", perms...), m, "PROPOSED")
			saidKind(t, name+", "+m, err, orgapi.ErrUnauthorized, "no person holds", "send ConfirmOnboarding again")
		}
	}
	if o, _ := d.org.GetOnboarding(context.Background(), orgapi.GetOnboarding{TenantID: d.tenant, OnboardingID: "ob-1"}); o.Modules[0].State != "NOT_STARTED" || o.Modules[1].State != "NOT_STARTED" {
		t.Fatalf("a refused record wrote: %+v", o.Modules)
	}
	// a loose ORG, which any caller may record in, says both modules are APPROVED while neither made a change set: the platform does not believe it
	loose, err := d.platform.ORG(allowAll{})
	d.must("a loose ORG", err)
	for _, m := range []string{"FIN", "SEA"} {
		for _, state := range []string{"PROPOSED", "APPROVED"} {
			_, err = loose.RecordOnboardingModule(context.Background(), orgapi.RecordOnboardingModule{CommandID: "loose-" + m + state, TenantID: d.tenant, ActorID: "olga", OnboardingID: "ob-1",
				Module: m, ChangeID: "onb-ob-1-" + strings.ToLower(m), State: state})
			d.must("the loose record", err)
		}
	}
	if f, s := d.count(); f != 0 || s != 0 {
		t.Fatalf("change sets before the platform: %d, %d", f, s)
	}
	got, err := d.confirm("ob-1")
	if err != nil || got.Receipt.State != "COMPLETE" {
		t.Fatalf("the platform's confirmation: %+v, %v", got, err)
	}
	if f, s := d.count(); f != 1 || s != 1 {
		t.Fatalf("the platform believed ORG's record: %d, %d change sets, want 1 and 1", f, s)
	}
	if f := d.finValues("TENANT", ""); f["fin.F7"] != "BLOCKS" {
		t.Fatalf("STRICT is not in force: %+v", f)
	}

	// a crash between the module's approval and ORG's record: the retry writes the record and makes no second change set
	if _, err = d.propose("ob-2", "LEAN", 3, false, nil); err != nil {
		t.Fatal(err)
	}
	flaky := &failingRecords{ORGOnboarding: d.org, fails: 1}
	onboarding, err := NewOnboarding(flaky, d.fin, d.sea)
	d.must("the onboarding", err)
	got, err = onboarding.ConfirmOnboarding(d.as("olga", owner...), d.confirmCommand("ob-2"))
	saidKind(t, "ORG's record fails", err, ErrOnboardingIncomplete, "FIN:", "ORG is down", "send the same ConfirmOnboarding again")
	if c, _ := d.finChange("onb-ob-2-fin"); c.Status != finapi.SettingsInForce || got.Onboarding.Modules[0].State != "PROPOSED" {
		t.Fatalf("FIN's change set is approved and ORG's record is behind: %+v, %+v", c, got.Onboarding.Modules)
	}
	if got, err = onboarding.ConfirmOnboarding(d.as("olga", owner...), d.confirmCommand("ob-2")); err != nil || got.Receipt.State != "COMPLETE" || got.Onboarding.Modules[0].State != "APPROVED" {
		t.Fatalf("the retry: %+v, %v", got, err)
	}
	if f, s := d.count(); f != 2 || s != 2 {
		t.Fatalf("change sets after the retry: %d, %d, want 2 and 2", f, s)
	}
}

// F9: a failure that sending the same ConfirmOnboarding again cannot cure says so, with the way out, a new onboarding under
// another id: the derived change set id or command id taken by another change (the review's probe D), the change set
// refused or cancelled (probe E). A cause a retry can cure keeps "send the same ConfirmOnboarding again".
func TestAFailureARetryCannotCureSaysSoWithTheWayOut(t *testing.T) {
	cannot := func(t *testing.T, what string, err error, got OnboardingConfirmation, contains ...string) {
		t.Helper()
		saidKind(t, what, err, ErrOnboardingBlocked, append(contains, "FIN:", "cannot cure FIN: propose a new onboarding under another id")...)
		if errors.Is(err, ErrOnboardingIncomplete) || strings.Contains(err.Error(), "send the same ConfirmOnboarding again, which completes") {
			t.Fatalf("%s: the answer says to retry what a retry cannot cure: %v", what, err)
		}
		if len(got.Failures) == 0 || !got.Failures[0].Permanent || got.Receipt.State != "CONFIRMED" {
			t.Fatalf("%s: %+v", what, got)
		}
	}
	bob := func(d *settingsDay, command, change string) error {
		_, err := d.fin.ProposeFinSettingsChange(d.as("bob", "fin:ConfigureSettings"), finapi.ProposeFinSettingsChange{CommandID: command, TenantID: d.tenant, ActorID: "bob",
			ChangeID: change, Entries: []finapi.FinSettingEntry{{ScopeKind: "TENANT", Key: "fin.F1", Value: "UNRESTRICTED"}}, Reason: "bob's"})
		return err
	}
	t.Run("the change set id taken by another configurer", func(t *testing.T) {
		d := newSettingsDay(t, "cargo-onb-d1-")
		if _, err := d.propose("ob-1", "LEAN", 3, false, nil); err != nil {
			t.Fatal(err)
		}
		d.must("bob takes the id with the derived command id", bob(d, "onb-ob-1-fin-propose", "onb-ob-1-fin"))
		for i := 0; i < 2; i++ { // and again: a retry changes nothing
			got, err := d.confirm("ob-1")
			cannot(t, "the id taken", err, got, "is not this onboarding's", "proposed by bob")
		}
		if c, _ := d.finChange("onb-ob-1-fin"); c.Status != finapi.SettingsProposed || c.ProposedBy != "bob" || c.ApprovedBy != "" {
			t.Fatalf("bob's change set was approved as the onboarding's: %+v", c)
		}
		if v, _ := d.seaValue("TENANT", ""); v != "WITH_REASON" {
			t.Fatalf("SEA goes on: %s", v)
		}
	})
	t.Run("the derived command id taken by another change", func(t *testing.T) {
		d := newSettingsDay(t, "cargo-onb-d2-")
		if _, err := d.propose("ob-1", "LEAN", 3, false, nil); err != nil {
			t.Fatal(err)
		}
		d.must("bob uses the derived command id for his own change set", bob(d, "onb-ob-1-fin-propose", "bob-own"))
		got, err := d.confirm("ob-1")
		cannot(t, "the command id taken", err, got, "a command id the onboarding derived for FIN is taken by another command")
		if _, ok := d.finChange("onb-ob-1-fin"); ok {
			t.Fatal("FIN made the onboarding's change set")
		}
	})
	t.Run("the change set refused or cancelled", func(t *testing.T) {
		for _, how := range []string{"refused", "cancelled"} {
			d := newSettingsDay(t, "cargo-onb-d3-"+how+"-")
			if _, err := d.propose("ob-1", "LEAN", 3, false, nil); err != nil {
				t.Fatal(err)
			}
			flaky := &failingFIN{FINSettings: d.fin, fails: 1}
			onboarding, err := NewOnboarding(d.org, flaky, d.sea)
			d.must("the onboarding", err)
			_, err = onboarding.ConfirmOnboarding(d.as("olga", owner...), d.confirmCommand("ob-1"))
			saidKind(t, "the first confirmation, FIN's approval down for a moment", err, ErrOnboardingIncomplete, "send the same ConfirmOnboarding again")
			if how == "refused" {
				_, err = d.fin.RefuseFinSettingsChange(d.as("carl", "fin:ApproveSettings"), finapi.RefuseFinSettingsChange{CommandID: "carl-1", TenantID: d.tenant, ActorID: "carl", ChangeID: "onb-ob-1-fin", Reason: "no"})
			} else {
				_, err = d.fin.CancelFinSettingsChange(d.as("carl", "fin:ApproveSettings"), finapi.CancelFinSettingsChange{CommandID: "carl-1", TenantID: d.tenant, ActorID: "carl", ChangeID: "onb-ob-1-fin", Reason: "no"})
			}
			d.must("carl "+how, err)
			got, err := d.confirm("ob-1")
			cannot(t, "the change set "+how, err, got, "was "+how)
		}
	})
}

// F12: export and diff answer each module the caller may see, ConfigureSettings or ApproveSettings by name in that module, and
// say which modules were left out and why; a caller who may see neither is refused with the way out. A file exported by such
// a caller carries what was left out and is read again.
func TestExportAndDiffAnswerEachModuleTheCallerMaySee(t *testing.T) {
	d := newSettingsDay(t, "cargo-export-see-")
	d.legalEntity()
	if _, err := d.propose("ob-1", "STRICT", 3, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := d.confirm("ob-1"); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		perms    []string
		fin, sea bool
	}{
		"fin:ConfigureSettings":                         {[]string{"fin:ConfigureSettings"}, true, false},
		"fin:ApproveSettings":                           {[]string{"fin:ApproveSettings"}, true, false},
		"sea:ConfigureSettings":                         {[]string{"sea:ConfigureSettings"}, false, true},
		"sea:ApproveSettings":                           {[]string{"sea:ApproveSettings"}, false, true},
		"fin:ConfigureSettings and sea:ApproveSettings": {[]string{"fin:ConfigureSettings", "sea:ApproveSettings"}, true, true},
	} {
		exp, err := d.tools.ExportSettings(d.as("bob", c.perms...), d.tenant, "bob")
		_, hasFin, hasSea := exp.Modules[ModuleFIN], false, false
		_, hasFin = exp.Modules[ModuleFIN]
		_, hasSea = exp.Modules[ModuleSEA]
		if err != nil || hasFin != c.fin || hasSea != c.sea || len(exp.LeftOut) != btoi(!c.fin)+btoi(!c.sea) {
			t.Fatalf("%s: export %+v, %v", name, exp, err)
		}
		for _, l := range exp.LeftOut {
			m := strings.ToLower(l.Module)
			if !strings.Contains(l.Reason, "the caller holds neither "+m+":ConfigureSettings nor "+m+":ApproveSettings, by name; ask for one of them") {
				t.Errorf("%s: left out without its reason: %+v", name, l)
			}
		}
	}
	for name, perms := range map[string][]string{"the wildcards": {"*", "fin:*", "sea:*"}, "another action": {"fin:ReadProfit", "sea:ReadVgmSignatory"}, "none": nil} {
		_, err := d.tools.ExportSettings(d.as("bob", perms...), d.tenant, "bob")
		saidKind(t, "an export with "+name, err, orgapi.ErrUnauthorized, "neither in FIN or in SEA", "ask for fin:ConfigureSettings")
		_, err = d.tools.DiffSettingsImport(d.as("bob", perms...), d.tenant, "bob", []byte(`{"format":"cargo-settings/1"}`))
		saidKind(t, "a diff with "+name, err, orgapi.ErrUnauthorized, "neither in FIN or in SEA")
	}
	// the diff of a file from a tenant with other values: FIN-only and SEA-only callers get their module's entries only
	src := newSettingsDay(t, "cargo-export-see-src-")
	src.legalEntity()
	if _, err := src.propose("ob-1", "LEAN", 3, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := src.confirm("ob-1"); err != nil {
		t.Fatal(err)
	}
	file, err := src.tools.ExportSettingsJSON(src.as("bob", "fin:ApproveSettings", "sea:ApproveSettings"), src.tenant, "bob")
	d.must("the source's export", err)
	finOnly, err := d.tools.DiffSettingsImport(d.as("bob", "fin:ApproveSettings"), d.tenant, "bob", file)
	if err != nil || len(finOnly.Fin) == 0 || len(finOnly.Sea) != 0 || len(finOnly.LeftOut) != 1 || finOnly.LeftOut[0].Module != ModuleSEA || !strings.Contains(finOnly.LeftOut[0].Reason, "sea:ConfigureSettings") {
		t.Fatalf("a FIN-only caller's diff: %+v, %v", finOnly, err)
	}
	seaOnly, err := d.tools.DiffSettingsImport(d.as("bob", "sea:ConfigureSettings"), d.tenant, "bob", file)
	if err != nil || len(seaOnly.Sea) != 1 || seaOnly.Sea[0].Value != "WITH_REASON" || len(seaOnly.Fin) != 0 || len(seaOnly.LeftOut) != 1 || seaOnly.LeftOut[0].Module != ModuleFIN {
		t.Fatalf("a SEA-only caller's diff: %+v, %v", seaOnly, err)
	}
	// a partial export, with what it left out, is read again as a file
	part, err := d.tools.ExportSettingsJSON(d.as("bob", "fin:ApproveSettings"), d.tenant, "bob")
	d.must("a FIN-only export", err)
	if !strings.Contains(string(part), `"leftOut"`) {
		t.Fatalf("the file does not say what it left out: %s", part)
	}
	if diff, err := d.tools.DiffSettingsImport(d.as("bob", adminFin...), d.tenant, "bob", part); err == nil && len(diff.Fin) != 0 {
		t.Fatalf("a partial export of the same tenant differs from it: %+v", diff)
	} else if err != nil {
		t.Fatalf("a partial export is not read again: %v", err)
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// F12: the diff compares scope by scope. An entry the file has at a legal entity stays that legal entity's entry even when
// its value equals the company's here, unless this environment has the same entry at that legal entity; the company's
// entry is compared with what is in force for the company.
func TestTheDiffKeepsALegalEntitysOwnEntryEvenWhenItEqualsTheCompanys(t *testing.T) {
	src := newSettingsDay(t, "cargo-diff-le-src-")
	src.legalEntity()
	if _, err := src.propose("ob-1", "STANDARD", 3, false, map[string][]orgapi.OnboardingEntry{
		"FIN": {{ScopeKind: "LEGAL_ENTITY", ScopeID: "LE", Key: "fin.F7", Value: "DOES_NOT_BLOCK"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.confirm("ob-1"); err != nil {
		t.Fatal(err)
	}
	file, err := src.tools.ExportSettingsJSON(src.as("ann", consultant...), src.tenant, "ann")
	src.must("the export", err)
	if !strings.Contains(string(file), `"scopeId": "LE"`) {
		t.Fatalf("the legal entity's own entry is exported: %s", file)
	}
	diffOf := func(d *settingsDay) SettingsDiff {
		t.Helper()
		diff, err := d.tools.DiffSettingsImport(d.as("ann", consultant...), d.tenant, "ann", file)
		d.must("the diff", err)
		return diff
	}
	entityEntries := func(diff SettingsDiff) (out []finapi.FinSettingEntry) {
		for _, e := range diff.Fin {
			if e.ScopeKind == "LEGAL_ENTITY" {
				out = append(out, e)
			}
		}
		return out
	}
	// a target with the company's value and no entry of the legal entity: the entry is proposed for the legal entity
	dst := newSettingsDay(t, "cargo-diff-le-dst-")
	dst.legalEntity()
	if _, err = dst.propose("ob-1", "STANDARD", 3, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = dst.confirm("ob-1"); err != nil {
		t.Fatal(err)
	}
	if got := entityEntries(diffOf(dst)); len(got) != 1 || got[0].ScopeID != "LE" || got[0].Key != "fin.F7" || got[0].Value != "DOES_NOT_BLOCK" {
		t.Fatalf("the legal entity's own entry equal to the company's is dropped: %+v", got)
	}
	// a target with the same entry at the legal entity: nothing differs
	same := newSettingsDay(t, "cargo-diff-le-same-")
	same.legalEntity()
	if _, err = same.propose("ob-1", "STANDARD", 3, false, map[string][]orgapi.OnboardingEntry{
		"FIN": {{ScopeKind: "LEGAL_ENTITY", ScopeID: "LE", Key: "fin.F7", Value: "DOES_NOT_BLOCK"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err = same.confirm("ob-1"); err != nil {
		t.Fatal(err)
	}
	if diff := diffOf(same); len(diff.Fin) != 0 || len(diff.Sea) != 0 || len(diff.Unknown) != 0 {
		t.Fatalf("the same entry at the same legal entity differs: %+v", diff)
	}
	// a target whose legal entity has another value of its own: the file's entry is proposed
	other := newSettingsDay(t, "cargo-diff-le-other-")
	other.legalEntity()
	if _, err = other.propose("ob-1", "STRICT", 3, false, map[string][]orgapi.OnboardingEntry{
		"FIN": {{ScopeKind: "LEGAL_ENTITY", ScopeID: "LE", Key: "fin.F7", Value: "DOES_NOT_BLOCK"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err = other.confirm("ob-1"); err != nil {
		t.Fatal(err)
	}
	if got := entityEntries(diffOf(other)); len(got) != 0 {
		t.Fatalf("the same own entry differs: %+v", got)
	}
}

// SEA's part of the diff refuses what this environment does not know, each with its way out: an item, a value, and a
// threshold, which SEA's items do not take.
func TestSeasPartOfTheDiffRefusesWhatItDoesNotKnow(t *testing.T) {
	d := newSettingsDay(t, "cargo-diff-sea-")
	d.legalEntity()
	file, err := json.Marshal(SettingsExport{Format: ExportFormat, TenantID: "x", Modules: map[string]ExportedModule{ModuleSEA: {CatalogueVersion: "old", Scopes: []ExportedScope{
		{ScopeKind: "TENANT", Items: []ExportedItem{{Key: "sea.S9", Value: "X"}, {Key: "sea.S1", Value: "NO_SUCH"}, {Key: "sea.S1", Value: "WITH_REASON", ThresholdMinor: ptr(5), ThresholdCurrency: "CNY"},
			{Key: "sea.S1", Value: "SUPERVISOR_ONLY"}}}}}}})
	d.must("the file", err)
	diff, err := d.tools.DiffSettingsImport(d.as("ann", consultant...), d.tenant, "ann", file)
	if err != nil || len(diff.Unknown) != 3 || len(diff.Sea) != 1 || diff.Sea[0].Value != "SUPERVISOR_ONLY" || len(diff.Notes) != 1 || !strings.Contains(diff.Notes[0], "SEA's catalogue here is version") {
		t.Fatalf("the diff: %+v, %v", diff, err)
	}
	want := map[string]string{"sea.S9": "has no item sea.S9; upgrade this environment", "NO_SUCH": "has no value NO_SUCH; upgrade this environment", "WITH_REASON": "SEA's items take no parameter; remove the threshold from the file"}
	for _, u := range diff.Unknown {
		key := u.Key
		if u.Key == "sea.S1" {
			key = u.Value
		}
		if u.Module != ModuleSEA || !strings.Contains(u.Reason, want[key]) {
			t.Errorf("an unknown item: %+v", u)
		}
	}
}

var _ = identity.ErrDenied
