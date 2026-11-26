package bootstrap

import (
	"fmt"
	"slices"

	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// The profiles of the rules (V2.102, design section 9 item 1): STANDARD, STRICT and LEAN, as data, each listing the value
// of every item of FIN (F1 to F9) and SEA (S1). They live here, where both modules' catalogues are known, and a test
// checks that each lists exactly the items of the catalogues and passes the whole-configuration validation of each
// module (fin.CheckSettingsEntries, sea.CheckSettingsEntries), so an item added to a catalogue fails it until the
// profiles say what they make of it.

// Modules an onboarding applies a profile to.
const (
	ModuleFIN = "FIN"
	ModuleSEA = "SEA"
)

// ProfileItem is the value a profile gives one item of one module.
type ProfileItem struct {
	Module string
	Key    string
	Value  string
}

// Profile is a named set of values for every item of FIN and SEA, and what it is for.
type Profile struct {
	ID      string
	TitleEN string
	TitleZH string
	// For says which customers it suits and how it differs from STANDARD.
	For   string
	Items []ProfileItem
}

// Value answers the profile's value for an item.
func (p Profile) Value(module, key string) (string, bool) {
	i := slices.IndexFunc(p.Items, func(it ProfileItem) bool { return it.Module == module && it.Key == key })
	if i < 0 {
		return "", false
	}
	return p.Items[i].Value, true
}

// standardItems are the defaults of the catalogues, in the order of the items.
var standardItems = []ProfileItem{
	{ModuleFIN, "fin.F1", "NOT_ALLOWED"},
	{ModuleFIN, "fin.F2", "SUBMITTER_ONLY"},
	{ModuleFIN, "fin.F3", "EVERY_ATTRIBUTION"},
	{ModuleFIN, "fin.F4", "ALL_PREVIEWED"},
	{ModuleFIN, "fin.F6", "SEPARATE"},
	{ModuleFIN, "fin.F7", "DOES_NOT_BLOCK"},
	{ModuleFIN, "fin.F8", "REJECT_WITH_DEFAULT"},
	{ModuleFIN, "fin.F9", "KEEP_AMOUNT"},
	{ModuleSEA, "sea.S1", "WITH_REASON"},
}

// derived answers the standard items with the values given instead.
func derived(changes map[string]string) []ProfileItem {
	out := slices.Clone(standardItems)
	for i, it := range out {
		if v, ok := changes[it.Key]; ok {
			out[i].Value = v
		}
	}
	return out
}

var profiles = []Profile{
	{ID: "STANDARD", TitleEN: "Standard", TitleZH: "标准", Items: standardItems,
		For: "The default, for most freight forwarders: every item takes its default, which is how the system behaved before the settings."},
	{ID: "STRICT", TitleEN: "Strict", TitleZH: "严格", For: "For a company audited or with demanding internal control: both the submitter and the previewer of an apportionment " +
		"are checked against its confirmer, which wants at least three finance staff; a pending attribution blocks closing a pool; a confirmed attribution is " +
		"confirmed again when its cost changes; and only a supervisor releases a carrier's contradiction.",
		Items: derived(map[string]string{"fin.F2": "SUBMITTER_AND_PREVIEWER", "fin.F7": "BLOCKS", "fin.F9": "RECONFIRM", "sea.S1": "SUPERVISOR_ONLY"})},
	{ID: "LEAN", TitleEN: "Lean", TitleZH: "精简", For: "For a small team of one or two finance staff: no separation of duties for confirming an attribution, a voucher only when an " +
		"amount moves, a one-step apportionment when no house opened or left, and a missing attribution defaults to the cargo that remains. The compensation " +
		"is a weekly after-the-fact review list: the attributions of a week that have no second person, no voucher or were made by default, which finance's head reviews once the week has ended.",
		Items: derived(map[string]string{"fin.F1": "UNRESTRICTED", "fin.F3": "ONLY_WHEN_AMOUNT_MOVES", "fin.F4": "ONE_STEP_WITHOUT_OPENED_OR_LEFT", "fin.F8": "DEFAULT_TO_REMAINING"})},
}

// SettingsProfiles answers the profiles, STANDARD, STRICT and LEAN, with the value of every item in each. It is the
// query of the profiles; the answer is a copy.
func SettingsProfiles() []Profile {
	out := make([]Profile, len(profiles))
	for i, p := range profiles {
		p.Items = slices.Clone(p.Items)
		out[i] = p
	}
	return out
}

// SettingsProfile answers one profile by its identifier.
func SettingsProfile(id string) (Profile, bool) {
	for _, p := range SettingsProfiles() {
		if p.ID == id {
			return p, true
		}
	}
	return Profile{}, false
}

// onboardingEntry is an entry of a profile or an override, as the onboarding applies it to a module (the same shape as
// ORG's record of the overrides).
type onboardingEntry struct {
	ScopeKind         string
	ScopeID           string
	Key               string
	Value             string
	ThresholdMinor    *int64
	ThresholdCurrency string
}

// profileEntries are a profile's values for a module at the company's scope, then the overrides: an override replaces
// the profile's entry of the same item and scope, or is added after them.
func profileEntries(p Profile, module string, overrides []onboardingEntry) []onboardingEntry {
	var out []onboardingEntry
	for _, it := range p.Items {
		if it.Module == module {
			out = append(out, onboardingEntry{ScopeKind: "TENANT", Key: it.Key, Value: it.Value})
		}
	}
	for _, o := range overrides {
		if i := slices.IndexFunc(out, func(e onboardingEntry) bool {
			return e.ScopeKind == o.ScopeKind && e.ScopeID == o.ScopeID && e.Key == o.Key
		}); i >= 0 {
			out[i] = o
		} else {
			out = append(out, o)
		}
	}
	return out
}

func finEntries(in []onboardingEntry) ([]finapi.FinSettingEntry, error) {
	out := make([]finapi.FinSettingEntry, len(in))
	for i, e := range in {
		out[i] = finapi.FinSettingEntry{ScopeKind: e.ScopeKind, ScopeID: e.ScopeID, Key: e.Key, Value: e.Value}
		if e.ThresholdMinor != nil {
			out[i].Threshold = &kernel.Money{Currency: kernel.CurrencyCode(e.ThresholdCurrency), Minor: *e.ThresholdMinor}
		}
	}
	return out, nil
}

func seaEntries(in []onboardingEntry) ([]seaapi.SeaSettingEntry, error) {
	out := make([]seaapi.SeaSettingEntry, len(in))
	for i, e := range in {
		if e.ThresholdMinor != nil {
			return nil, fmt.Errorf("SEA's items take no parameter, so %s has no threshold; leave it out", e.Key)
		}
		out[i] = seaapi.SeaSettingEntry{ScopeKind: e.ScopeKind, ScopeID: e.ScopeID, Key: e.Key, Value: e.Value}
	}
	return out, nil
}
