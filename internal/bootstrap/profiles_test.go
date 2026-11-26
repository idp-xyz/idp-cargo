package bootstrap

import (
	"os"
	"slices"
	"strings"
	"testing"

	"go.idp.xyz/idp-cargo/internal/contexts/fin"
	"go.idp.xyz/idp-cargo/internal/contexts/sea"
)

// V2.102: the profiles are data, each listing every item of FIN and SEA, and each valid for both modules.

func TestEachProfileListsEveryItemOnceWithAValueOfTheCatalogue(t *testing.T) {
	cats := map[string]map[string][]string{ModuleFIN: {}, ModuleSEA: {}}
	defaults := map[string]string{}
	for _, it := range fin.SettingsCatalogue().Items {
		for _, v := range it.Values {
			cats[ModuleFIN][it.Key] = append(cats[ModuleFIN][it.Key], v.ID)
		}
		defaults[it.Key] = it.Default
	}
	for _, it := range sea.SettingsCatalogue().Items {
		for _, v := range it.Values {
			cats[ModuleSEA][it.Key] = append(cats[ModuleSEA][it.Key], v.ID)
		}
		defaults[it.Key] = it.Default
	}
	ps := SettingsProfiles()
	if len(ps) != 3 || ps[0].ID != "STANDARD" || ps[1].ID != "STRICT" || ps[2].ID != "LEAN" {
		t.Fatalf("profiles: %+v", ps)
	}
	for _, p := range ps {
		n := 0
		for module, items := range cats {
			for key, values := range items {
				v, ok := p.Value(module, key)
				if !ok || !slices.Contains(values, v) {
					t.Errorf("%s: %s %s = %q, %v", p.ID, module, key, v, ok)
				}
				n++
			}
		}
		if len(p.Items) != n {
			t.Errorf("%s lists %d items, the catalogues have %d", p.ID, len(p.Items), n)
		}
	}
	// STANDARD is every default; STRICT and LEAN differ from it exactly where the design says
	want := map[string]map[string]string{
		"STANDARD": {},
		"STRICT":   {"fin.F2": "SUBMITTER_AND_PREVIEWER", "fin.F7": "BLOCKS", "fin.F9": "RECONFIRM", "sea.S1": "SUPERVISOR_ONLY"},
		"LEAN":     {"fin.F1": "UNRESTRICTED", "fin.F3": "ONLY_WHEN_AMOUNT_MOVES", "fin.F4": "ONE_STEP_WITHOUT_OPENED_OR_LEFT", "fin.F8": "DEFAULT_TO_REMAINING"},
	}
	for _, p := range ps {
		for _, it := range p.Items {
			expected := defaults[it.Key]
			if v, ok := want[p.ID][it.Key]; ok {
				expected = v
			}
			if it.Value != expected {
				t.Errorf("%s %s is %s, want %s", p.ID, it.Key, it.Value, expected)
			}
		}
	}
}

func TestEachProfileValidatesInEachModule(t *testing.T) {
	for _, p := range SettingsProfiles() {
		if err := checkModules(p, nil); err != nil {
			t.Errorf("%s: %v", p.ID, err)
		}
	}
	// the check is real: a profile that breaks F8's rule is refused
	bad, _ := SettingsProfile("STANDARD")
	bad.Items = slices.Clone(bad.Items)
	for i := range bad.Items {
		if bad.Items[i].Key == "fin.F8" {
			bad.Items[i].Value = "DEFAULT_TO_REMAINING"
		}
	}
	if err := checkModules(bad, nil); err == nil || !strings.Contains(err.Error(), "fin.F3") {
		t.Errorf("a profile that breaks a rule across items: %v", err)
	}
}

func TestSettingsProfilesAreCopiesAndOverridesReplaceTheirItem(t *testing.T) {
	ps := SettingsProfiles()
	ps[0].Items[0].Value = "CHANGED"
	if v, _ := SettingsProfiles()[0].Value(ModuleFIN, "fin.F1"); v != "NOT_ALLOWED" {
		t.Fatal("the answer is not a copy")
	}
	p, _ := SettingsProfile("STRICT")
	got := profileEntries(p, ModuleFIN, []onboardingEntry{{ScopeKind: "TENANT", Key: "fin.F7", Value: "DOES_NOT_BLOCK"}, {ScopeKind: "LEGAL_ENTITY", ScopeID: "LE", Key: "fin.F7", Value: "BLOCKS"}})
	if len(got) != 9 { // eight items of FIN, the legal entity's override added
		t.Fatalf("%d entries", len(got))
	}
	for _, e := range got {
		if e.Key == "fin.F7" && e.ScopeKind == "TENANT" && e.Value != "DOES_NOT_BLOCK" {
			t.Fatalf("the override did not replace: %+v", e)
		}
	}
	if _, ok := SettingsProfile("FANCY"); ok {
		t.Fatal("an unknown profile")
	}
}

// The committed catalogue document is what the program writes.
func TestTheCommittedCatalogueDocumentIsWhatTheProgramWrites(t *testing.T) {
	committed, err := os.ReadFile("../../" + SettingsCatalogueDocPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(committed) != SettingsCatalogueDocument() {
		t.Fatal("docs/settings/catalogue.md differs from what cmd/cargo-settings-doc writes; run `go run ./cmd/cargo-settings-doc` from the repository's root and commit the result")
	}
	doc := SettingsCatalogueDocument()
	for _, must := range []string{"fin.F1", "fin.F9", "sea.S1", "fin.E1", "STRICT", "LEAN", "**NOT_ALLOWED", "SUBMITTER_AND_PREVIEWER", "Catalogue version `" + fin.SettingsCatalogue().Version + "`"} {
		if !strings.Contains(doc, must) {
			t.Errorf("the document does not say %q", must)
		}
	}
}
