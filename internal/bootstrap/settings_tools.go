package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// The settings tools of V2.102 (design section 9, items 4 and 6): the exception report, and the export and import of a
// tenant's configuration. They compose FIN's and SEA's own queries where the contexts are composed, so neither depends
// on the other, and they change nothing: an import proposes nothing, the caller proposes what it answers through the
// modules' commands, with the usual approval.

// SettingsTools composes FIN's and SEA's settings queries.
type SettingsTools struct {
	fin FINSettings
	sea SEASettings
}

// NewSettingsTools composes the tools over FIN and SEA.
func NewSettingsTools(fin FINSettings, sea SEASettings) (*SettingsTools, error) {
	if fin == nil || sea == nil {
		return nil, errors.New("bootstrap: the settings tools need FIN and SEA")
	}
	return &SettingsTools{fin: fin, sea: sea}, nil
}

// ExceptionReport is the exception report of a tenant for a period: each module's part for a holder of that module's
// ApproveSettings (nil for a part the actor may not see), and the actors who in the period both changed settings
// (proposed or approved) and submitted an apportionment or confirmed an attribution in FIN, computed from the records.
type ExceptionReport struct {
	TenantID string
	From, To time.Time
	Fin      *finapi.FinExceptionReport
	Sea      *seaapi.SeaExceptionReport
	// Both is computed for a holder of FIN's part, over the settings changes of the parts the actor may see.
	Both []ActorBoth
}

// ActorBoth is an actor who changed settings and did FIN's work in the period.
type ActorBoth struct {
	ActorID   string
	Proposed  int
	Approved  int
	Submitted int
	Confirmed int
}

// GetExceptionReport answers the exception report. The actor needs fin:ApproveSettings or sea:ApproveSettings, by name,
// for the module's part; with neither it is refused. The report is read-only.
func (t *SettingsTools) GetExceptionReport(ctx context.Context, tenant, actor string, from, to time.Time) (ExceptionReport, error) {
	out := ExceptionReport{TenantID: tenant, From: from, To: to}
	f, ferr := t.fin.GetFinExceptionReport(ctx, finapi.GetFinExceptionReport{TenantID: tenant, ActorID: actor, From: from, To: to})
	s, serr := t.sea.GetSeaExceptionReport(ctx, seaapi.GetSeaExceptionReport{TenantID: tenant, ActorID: actor, From: from, To: to})
	if ferr != nil && !errors.Is(ferr, finapi.ErrUnauthorized) {
		return out, ferr
	}
	if serr != nil && !errors.Is(serr, seaapi.ErrUnauthorized) {
		return out, serr
	}
	if ferr != nil && serr != nil {
		return out, fmt.Errorf("%w: the exception report is told to a holder of fin:ApproveSettings or sea:ApproveSettings, granted by name; ask for one of them: %v",
			orgapi.ErrUnauthorized, ferr)
	}
	if ferr == nil {
		out.Fin = &f
	}
	if serr == nil {
		out.Sea = &s
	}
	if out.Fin != nil {
		changed := map[string]*ActorBoth{}
		note := func(actor string, proposed, approved int) {
			if changed[actor] == nil {
				changed[actor] = &ActorBoth{ActorID: actor}
			}
			changed[actor].Proposed += proposed
			changed[actor].Approved += approved
		}
		for _, c := range f.Changers {
			note(c.ActorID, c.Proposed, c.Approved)
		}
		if out.Sea != nil {
			for _, c := range s.Changers {
				note(c.ActorID, c.Proposed, c.Approved)
			}
		}
		for _, w := range f.Work {
			if c := changed[w.ActorID]; c != nil && w.Submitted > 0 {
				c.Submitted, c.Confirmed = w.Submitted, w.Confirmed
				out.Both = append(out.Both, *c)
			}
		}
		slices.SortFunc(out.Both, func(a, b ActorBoth) int {
			switch {
			case a.ActorID < b.ActorID:
				return -1
			case a.ActorID > b.ActorID:
				return 1
			}
			return 0
		})
	}
	return out, nil
}

// ExportFormat names the format of an exported configuration; an import of another is refused.
const ExportFormat = "cargo-settings/1"

// ExportedItem is the value of one item at a scope, with the amount of a value that asks for one. Default says the
// item has no entry and takes its default.
type ExportedItem struct {
	Key               string `json:"key"`
	Value             string `json:"value"`
	Default           bool   `json:"default,omitempty"`
	ThresholdMinor    *int64 `json:"thresholdMinor,omitempty"`
	ThresholdCurrency string `json:"thresholdCurrency,omitempty"`
}

// ExportedScope is a scope's items: the company's every item, a legal entity's only the items it sets itself.
type ExportedScope struct {
	ScopeKind string         `json:"scopeKind"`
	ScopeID   string         `json:"scopeId,omitempty"`
	Items     []ExportedItem `json:"items"`
}

// ExportedModule is a module's configuration in force, with the version of its catalogue and of its settings.
type ExportedModule struct {
	CatalogueVersion string          `json:"catalogueVersion"`
	SettingsVersion  int             `json:"settingsVersion"`
	Scopes           []ExportedScope `json:"scopes"`
}

// SettingsExport is a tenant's whole configuration in force, per module.
type SettingsExport struct {
	Format     string                    `json:"format"`
	TenantID   string                    `json:"tenant"`
	ExportedAt time.Time                 `json:"exportedAt"`
	Modules    map[string]ExportedModule `json:"modules"`
}

// ExportSettings answers a tenant's whole configuration in force, per module, with each catalogue's version. The actor
// needs ConfigureSettings or ApproveSettings, by name, in each module.
func (t *SettingsTools) ExportSettings(ctx context.Context, tenant, actor string) (SettingsExport, error) {
	if err := holdsEither(ctx, tenant, actor, "fin", finapi.ActionConfigureSettings, finapi.ActionApproveSettings); err != nil {
		return SettingsExport{}, err
	}
	if err := holdsEither(ctx, tenant, actor, "sea", seaapi.ActionConfigureSettings, seaapi.ActionApproveSettings); err != nil {
		return SettingsExport{}, err
	}
	out := SettingsExport{Format: ExportFormat, TenantID: tenant, ExportedAt: time.Now().UTC().Truncate(time.Microsecond), Modules: map[string]ExportedModule{}}
	cat, err := t.fin.GetFinSettingsCatalogue(ctx, finapi.GetFinSettingsCatalogue{})
	if err != nil {
		return out, err
	}
	changes, err := t.fin.ListFinSettingsChanges(ctx, finapi.ListFinSettingsChanges{TenantID: tenant})
	if err != nil {
		return out, err
	}
	var entities []string
	for _, c := range changes {
		for _, e := range c.Entries {
			if e.ScopeKind == "LEGAL_ENTITY" && !slices.Contains(entities, e.ScopeID) {
				entities = append(entities, e.ScopeID)
			}
		}
	}
	slices.Sort(entities)
	module := ExportedModule{CatalogueVersion: cat.Version}
	for _, scope := range append([]string{""}, entities...) {
		kind := "TENANT"
		if scope != "" {
			kind = "LEGAL_ENTITY"
		}
		got, err := t.fin.GetFinSettings(ctx, finapi.GetFinSettings{TenantID: tenant, ScopeKind: kind, ScopeID: scope})
		if err != nil {
			return out, err
		}
		module.SettingsVersion = max(module.SettingsVersion, got.Version)
		s := ExportedScope{ScopeKind: kind, ScopeID: scope, Items: []ExportedItem{}}
		for _, it := range got.Items {
			if scope != "" && (it.Default || it.FromScopeKind != kind || it.FromScopeID != scope) {
				continue // a legal entity's own entries only
			}
			item := ExportedItem{Key: it.Key, Value: it.Value, Default: it.Default}
			if it.Threshold != nil {
				minor := it.Threshold.Minor
				item.ThresholdMinor, item.ThresholdCurrency = &minor, string(it.Threshold.Currency)
			}
			s.Items = append(s.Items, item)
		}
		if len(s.Items) > 0 {
			module.Scopes = append(module.Scopes, s)
		}
	}
	out.Modules[ModuleFIN] = module

	seaCat, err := t.sea.GetSeaSettingsCatalogue(ctx, seaapi.GetSeaSettingsCatalogue{})
	if err != nil {
		return out, err
	}
	seaChanges, err := t.sea.ListSeaSettingsChanges(ctx, seaapi.ListSeaSettingsChanges{TenantID: tenant})
	if err != nil {
		return out, err
	}
	entities = nil
	for _, c := range seaChanges {
		for _, e := range c.Entries {
			if e.ScopeKind == "LEGAL_ENTITY" && !slices.Contains(entities, e.ScopeID) {
				entities = append(entities, e.ScopeID)
			}
		}
	}
	slices.Sort(entities)
	module = ExportedModule{CatalogueVersion: seaCat.Version}
	for _, scope := range append([]string{""}, entities...) {
		kind := "TENANT"
		if scope != "" {
			kind = "LEGAL_ENTITY"
		}
		got, err := t.sea.GetSeaSettings(ctx, seaapi.GetSeaSettings{TenantID: tenant, ScopeKind: kind, ScopeID: scope})
		if err != nil {
			return out, err
		}
		module.SettingsVersion = max(module.SettingsVersion, got.Version)
		s := ExportedScope{ScopeKind: kind, ScopeID: scope, Items: []ExportedItem{}}
		for _, it := range got.Items {
			if scope != "" && (it.Default || it.FromScopeKind != kind || it.FromScopeID != scope) {
				continue
			}
			s.Items = append(s.Items, ExportedItem{Key: it.Key, Value: it.Value, Default: it.Default})
		}
		if len(s.Items) > 0 {
			module.Scopes = append(module.Scopes, s)
		}
	}
	out.Modules[ModuleSEA] = module
	return out, nil
}

// ExportSettingsJSON is ExportSettings as JSON.
func (t *SettingsTools) ExportSettingsJSON(ctx context.Context, tenant, actor string) ([]byte, error) {
	e, err := t.ExportSettings(ctx, tenant, actor)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(e, "", "  ")
}

func holdsEither(ctx context.Context, tenant, actor, bc string, actions ...string) error {
	var last error
	for _, a := range actions {
		if last = holdsNamed(ctx, bc, tenant, actor, a); last == nil {
			return nil
		}
	}
	return fmt.Errorf("%w: %v; the configuration of %s is told to a holder of %s:%s, by name", orgapi.ErrUnauthorized, last, bc, bc, strings.Join(actions, " or "+bc+":"))
}

// UnknownItem is an entry of an import that this environment's catalogues do not know: an item or a value it has not, or
// a module. It is refused, with the way out.
type UnknownItem struct {
	Module string
	Key    string
	Value  string
	Reason string
}

// SettingsDiff is what an import of an exported configuration would change in this environment for the tenant: the
// entries that differ from what is in force, as entries a proposal takes (propose them with ProposeFinSettingsChange and
// ProposeSeaSettingsChange: the usual approval applies), the items this environment does not know, and notes on
// catalogues that are not at the version the file was exported from. It proposes nothing.
type SettingsDiff struct {
	Fin     []finapi.FinSettingEntry
	Sea     []seaapi.SeaSettingEntry
	Unknown []UnknownItem
	Notes   []string
}

// DiffSettingsImport reads an exported configuration and answers the entries that differ from what is in force for the
// tenant, per module, and the items unknown here. Entries are compared at their scope against what is in force there
// (a legal entity's against its own value, else the company's, else the default). The file is refused when it is not
// JSON of this format. The actor needs ConfigureSettings or ApproveSettings, by name, in each module.
func (t *SettingsTools) DiffSettingsImport(ctx context.Context, tenant, actor string, file []byte) (SettingsDiff, error) {
	var in SettingsExport
	dec := json.NewDecoder(bytes.NewReader(file))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return SettingsDiff{}, fmt.Errorf("%w: the file is not an exported configuration (%v); export one with ExportSettings", orgapi.ErrRejected, err)
	}
	if in.Format != ExportFormat {
		return SettingsDiff{}, fmt.Errorf("%w: the file's format is %q, and this environment reads %q; export it again from a release that writes %q", orgapi.ErrRejected, in.Format, ExportFormat, ExportFormat)
	}
	if err := holdsEither(ctx, tenant, actor, "fin", finapi.ActionConfigureSettings, finapi.ActionApproveSettings); err != nil {
		return SettingsDiff{}, err
	}
	if err := holdsEither(ctx, tenant, actor, "sea", seaapi.ActionConfigureSettings, seaapi.ActionApproveSettings); err != nil {
		return SettingsDiff{}, err
	}
	var out SettingsDiff
	for module := range in.Modules {
		if module != ModuleFIN && module != ModuleSEA {
			out.Unknown = append(out.Unknown, UnknownItem{Module: module, Reason: "this environment has no module " + module + "; remove it from the file"})
		}
	}
	if m, ok := in.Modules[ModuleFIN]; ok {
		cat, err := t.fin.GetFinSettingsCatalogue(ctx, finapi.GetFinSettingsCatalogue{})
		if err != nil {
			return out, err
		}
		if m.CatalogueVersion != cat.Version {
			out.Notes = append(out.Notes, fmt.Sprintf("FIN's catalogue here is version %s, the file's is %s: the entries are checked against this one", cat.Version, m.CatalogueVersion))
		}
		known := map[string]map[string]bool{}
		for _, it := range cat.Items {
			known[it.Key] = map[string]bool{}
			for _, v := range it.Values {
				known[it.Key][v.ID] = true
			}
		}
		for _, sc := range m.Scopes {
			now, err := t.fin.GetFinSettings(ctx, finapi.GetFinSettings{TenantID: tenant, ScopeKind: sc.ScopeKind, ScopeID: sc.ScopeID})
			if err != nil {
				return out, err
			}
			for _, it := range sc.Items {
				if why := unknownWhy("FIN", cat.Version, it, known); why != "" {
					out.Unknown = append(out.Unknown, UnknownItem{Module: ModuleFIN, Key: it.Key, Value: it.Value, Reason: why})
					continue
				}
				i := slices.IndexFunc(now.Items, func(c finapi.FinSetting) bool { return c.Key == it.Key })
				if i >= 0 && now.Items[i].Value == it.Value && sameThreshold(now.Items[i].Threshold, it) {
					continue
				}
				entry := finapi.FinSettingEntry{ScopeKind: sc.ScopeKind, ScopeID: sc.ScopeID, Key: it.Key, Value: it.Value}
				if it.ThresholdMinor != nil {
					entry.Threshold = &kernel.Money{Currency: kernel.CurrencyCode(it.ThresholdCurrency), Minor: *it.ThresholdMinor}
				}
				out.Fin = append(out.Fin, entry)
			}
		}
	}
	if m, ok := in.Modules[ModuleSEA]; ok {
		cat, err := t.sea.GetSeaSettingsCatalogue(ctx, seaapi.GetSeaSettingsCatalogue{})
		if err != nil {
			return out, err
		}
		if m.CatalogueVersion != cat.Version {
			out.Notes = append(out.Notes, fmt.Sprintf("SEA's catalogue here is version %s, the file's is %s: the entries are checked against this one", cat.Version, m.CatalogueVersion))
		}
		known := map[string]map[string]bool{}
		for _, it := range cat.Items {
			known[it.Key] = map[string]bool{}
			for _, v := range it.Values {
				known[it.Key][v.ID] = true
			}
		}
		for _, sc := range m.Scopes {
			now, err := t.sea.GetSeaSettings(ctx, seaapi.GetSeaSettings{TenantID: tenant, ScopeKind: sc.ScopeKind, ScopeID: sc.ScopeID})
			if err != nil {
				return out, err
			}
			for _, it := range sc.Items {
				if why := unknownWhy("SEA", cat.Version, it, known); why != "" {
					out.Unknown = append(out.Unknown, UnknownItem{Module: ModuleSEA, Key: it.Key, Value: it.Value, Reason: why})
					continue
				}
				if it.ThresholdMinor != nil {
					out.Unknown = append(out.Unknown, UnknownItem{Module: ModuleSEA, Key: it.Key, Value: it.Value, Reason: "SEA's items take no parameter; remove the threshold from the file"})
					continue
				}
				i := slices.IndexFunc(now.Items, func(c seaapi.SeaSetting) bool { return c.Key == it.Key })
				if i >= 0 && now.Items[i].Value == it.Value {
					continue
				}
				out.Sea = append(out.Sea, seaapi.SeaSettingEntry{ScopeKind: sc.ScopeKind, ScopeID: sc.ScopeID, Key: it.Key, Value: it.Value})
			}
		}
	}
	return out, nil
}

func unknownWhy(module, version string, it ExportedItem, known map[string]map[string]bool) string {
	values, ok := known[it.Key]
	switch {
	case !ok:
		return fmt.Sprintf("this environment's %s catalogue (version %s) has no item %s; upgrade this environment to the release that exported the file, or remove the item from the file", module, version, it.Key)
	case !values[it.Value]:
		return fmt.Sprintf("item %s of this environment's %s catalogue (version %s) has no value %s; upgrade this environment to the release that exported the file, or choose a value it has", it.Key, module, version, it.Value)
	}
	return ""
}

func sameThreshold(now *kernel.Money, it ExportedItem) bool {
	switch {
	case now == nil && it.ThresholdMinor == nil:
		return true
	case now == nil || it.ThresholdMinor == nil:
		return false
	}
	return now.Minor == *it.ThresholdMinor && string(now.Currency) == it.ThresholdCurrency
}
