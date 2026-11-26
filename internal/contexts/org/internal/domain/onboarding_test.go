package domain

import (
	"errors"
	"strings"
	"testing"
)

func validOnboarding() Onboarding {
	e := OnboardingEntry{ScopeKind: "TENANT", Key: "fin.F7", Value: "BLOCKS"}
	return Onboarding{ID: "ob-1", Profile: ProfileLean, RiskStatement: "the statement", FinanceStaff: 3, ProposedBy: "ann",
		Plan: map[string]ModulePlan{"FIN": {CatalogueVersion: "v1", Entries: []OnboardingEntry{e}}, "SEA": {CatalogueVersion: "v1", Entries: []OnboardingEntry{e}}}}
}

// V2.102 batch E (R2-F8): the plan's catalogue version and its entries are checked in the domain too, and an actor id with spaces
// around it is not a proposer.
func TestAPlanNamesItsCatalogueVersionAndItsEntriesAndTheProposerIsAnActorId(t *testing.T) {
	if err := validOnboarding().Validate(); err != nil {
		t.Fatalf("a valid onboarding: %v", err)
	}
	entries := func(n int) []OnboardingEntry {
		out := make([]OnboardingEntry, n)
		for i := range out {
			out[i] = OnboardingEntry{ScopeKind: "TENANT", Key: "fin.F7", Value: "BLOCKS"}
		}
		return out
	}
	for name, c := range map[string]struct {
		change func(o *Onboarding)
		say    string
	}{
		"no plan for SEA": {func(o *Onboarding) { delete(o.Plan, "SEA") }, "the plan names the catalogue version of SEA"},
		"a blank version for FIN": {func(o *Onboarding) {
			o.Plan["FIN"] = ModulePlan{CatalogueVersion: "  ", Entries: o.Plan["FIN"].Entries}
		}, "the plan names the catalogue version of FIN"},
		"no version for SEA": {func(o *Onboarding) { o.Plan["SEA"] = ModulePlan{Entries: o.Plan["SEA"].Entries} }, "the plan names the catalogue version of SEA"},
		"a version of 65 characters": {func(o *Onboarding) {
			o.Plan["FIN"] = ModulePlan{CatalogueVersion: strings.Repeat("v", 65), Entries: o.Plan["FIN"].Entries}
		}, "the plan names the catalogue version of FIN"},
		"no entries for FIN": {func(o *Onboarding) { o.Plan["FIN"] = ModulePlan{CatalogueVersion: "v1"} }, "the plan of FIN is 1 to 64 entries"},
		"65 entries for SEA": {func(o *Onboarding) { o.Plan["SEA"] = ModulePlan{CatalogueVersion: "v1", Entries: entries(65)} }, "the plan of SEA is 1 to 64 entries"},
		"an entry with no value": {func(o *Onboarding) {
			o.Plan["FIN"] = ModulePlan{CatalogueVersion: "v1", Entries: []OnboardingEntry{{ScopeKind: "TENANT", Key: "k"}}}
		}, "the plan of FIN"},
		"a plan of another module": {func(o *Onboarding) { o.Plan["AIR"] = o.Plan["FIN"] }, "the plan is for FIN and SEA"},
		"a proposer with a space":  {func(o *Onboarding) { o.ProposedBy = "ann " }, "no spaces around it"},
		"a proposer of 129 bytes":  {func(o *Onboarding) { o.ProposedBy = strings.Repeat("a", 129) }, "1 to 128 bytes"},
		"a blank proposer":         {func(o *Onboarding) { o.ProposedBy = " " }, "1 to 128 bytes"},
	} {
		o := validOnboarding()
		c.change(&o)
		if err := o.Validate(); !errors.Is(err, ErrInvalidOnboarding) || !strings.Contains(err.Error(), c.say) {
			t.Errorf("%s: %v, want %q", name, err, c.say)
		}
	}
	for name, change := range map[string]func(o *Onboarding){
		"a version of 64 characters": func(o *Onboarding) {
			o.Plan["FIN"] = ModulePlan{CatalogueVersion: strings.Repeat("v", 64), Entries: o.Plan["FIN"].Entries}
		},
		"64 entries":              func(o *Onboarding) { o.Plan["SEA"] = ModulePlan{CatalogueVersion: "v1", Entries: entries(64)} },
		"a proposer of 128 bytes": func(o *Onboarding) { o.ProposedBy = strings.Repeat("a", 128) },
	} {
		o := validOnboarding()
		change(&o)
		if err := o.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestThePlanDigestIsThePlansJSONAndChangesWithAnyPartOfIt(t *testing.T) {
	o := validOnboarding()
	d := PlanDigest(o.Plan)
	if len(d) != 64 || d != PlanDigest(validOnboarding().Plan) {
		t.Fatalf("the digest: %q", d)
	}
	for name, change := range map[string]func(o *Onboarding){
		"a version": func(o *Onboarding) {
			o.Plan["FIN"] = ModulePlan{CatalogueVersion: "v2", Entries: o.Plan["FIN"].Entries}
		},
		"a value": func(o *Onboarding) {
			o.Plan["FIN"] = ModulePlan{CatalogueVersion: "v1", Entries: []OnboardingEntry{{ScopeKind: "TENANT", Key: "fin.F7", Value: "DOES_NOT_BLOCK"}}}
		},
		"another entry": func(o *Onboarding) {
			o.Plan["SEA"] = ModulePlan{CatalogueVersion: "v1", Entries: append(o.Plan["SEA"].Entries, OnboardingEntry{ScopeKind: "TENANT", Key: "k", Value: "v"})}
		},
	} {
		o := validOnboarding()
		change(&o)
		if PlanDigest(o.Plan) == d {
			t.Errorf("a changed %s keeps the digest", name)
		}
	}
	if ValidActor("") || ValidActor(" a") || ValidActor("a ") || ValidActor(strings.Repeat("a", 129)) || !ValidActor("a") || !ValidActor(strings.Repeat("a", 128)) {
		t.Error("ValidActor")
	}
}
