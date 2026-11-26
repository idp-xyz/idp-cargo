package bootstrap

import (
	"slices"
	"testing"

	"go.idp.xyz/idp-cargo/internal/contexts/fin"
	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	"go.idp.xyz/idp-cargo/internal/contexts/sea"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/foundation/settings/settingstest"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// entriesOfConfiguration is a configuration as the modules' proposals take it: each item at the company, except F1's
// threshold, which is an amount in the functional currency of a legal entity and so is set at LE.
func entriesOfConfiguration(c settingstest.Config) ([]finapi.FinSettingEntry, []seaapi.SeaSettingEntry) {
	var f []finapi.FinSettingEntry
	var s []seaapi.SeaSettingEntry
	for _, it := range settingstest.Items {
		value := c[it.Name]
		switch {
		case it.Name == "sea.S1":
			s = append(s, seaapi.SeaSettingEntry{ScopeKind: "TENANT", Key: it.Name, Value: value})
		case it.Name == "fin.F1" && c.ThresholdOf() > 0:
			f = append(f, finapi.FinSettingEntry{ScopeKind: "LEGAL_ENTITY", ScopeID: "LE", Key: it.Name, Value: c.F1Value(),
				Threshold: &kernel.Money{Currency: "CNY", Minor: c.ThresholdOf()}})
		default:
			f = append(f, finapi.FinSettingEntry{ScopeKind: "TENANT", Key: it.Name, Value: value})
		}
	}
	return f, s
}

// The combination space is the catalogues': every item of FIN and SEA with exactly its values (F1's threshold in two levels),
// so that an item or value added to a catalogue fails here until the covering array takes it.
func TestTheCombinationSpaceIsTheCataloguesItems(t *testing.T) {
	cats := map[string][]string{}
	for _, it := range fin.SettingsCatalogue().Items {
		for _, v := range it.Values {
			cats[it.Key] = append(cats[it.Key], v.ID)
		}
	}
	for _, it := range sea.SettingsCatalogue().Items {
		for _, v := range it.Values {
			cats[it.Key] = append(cats[it.Key], v.ID)
		}
	}
	if len(cats) != len(settingstest.Items) {
		t.Fatalf("the catalogues have %d items, the space %d", len(cats), len(settingstest.Items))
	}
	for _, f := range settingstest.Items {
		want := cats[f.Name]
		var got []string
		for _, l := range f.Levels {
			if l == settingstest.ThresholdLevelLow || l == settingstest.ThresholdLevelHigh {
				l = "NOT_ABOVE_THRESHOLD"
			}
			if !slices.Contains(got, l) {
				got = append(got, l)
			}
		}
		if !slices.Equal(got, want) && !sameSet(got, want) {
			t.Errorf("%s: the space has %v, the catalogue %v", f.Name, got, want)
		}
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		if !slices.Contains(b, x) {
			return false
		}
	}
	return true
}

// Each configuration of the covering array validates in its module (the whole-configuration validation, the rule across
// items included), and the one pair the rule forbids is refused: F8 DEFAULT_TO_REMAINING with F3 EVERY_ATTRIBUTION.
func TestEveryConfigurationOfTheCoveringArrayValidatesInItsModule(t *testing.T) {
	rows := settingstest.Configurations()
	for i, r := range rows {
		c := settingstest.ConfigOf(r)
		f, s := entriesOfConfiguration(c)
		if err := fin.CheckSettingsEntries(f); err != nil {
			t.Errorf("configuration %d %v: FIN: %v", i, map[string]string(c), err)
		}
		if err := sea.CheckSettingsEntries(s); err != nil {
			t.Errorf("configuration %d %v: SEA: %v", i, map[string]string(c), err)
		}
	}
	c := settingstest.ConfigOf(rows[0])
	c["fin.F8"], c["fin.F3"] = "DEFAULT_TO_REMAINING", "EVERY_ATTRIBUTION"
	f, _ := entriesOfConfiguration(c)
	if err := fin.CheckSettingsEntries(f); err == nil {
		t.Error("F8 DEFAULT_TO_REMAINING with F3 EVERY_ATTRIBUTION validates")
	}
	t.Logf("%d configurations validate", len(rows))
}
