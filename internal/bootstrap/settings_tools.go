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
	"go.idp.xyz/idp-cargo/internal/foundation/identity"
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
	// LeftOut are the modules the caller may not see, which the export does not carry, and why.
	LeftOut []LeftOut `json:"leftOut,omitempty"`
}

// LeftOut is a module an export or a diff does not answer, and why: the caller may not see it.
type LeftOut struct {
	Module string `json:"module"`
	Reason string `json:"reason"`
}

// visibleModules says which of FIN and SEA the caller may see, that is holds ConfigureSettings or ApproveSettings, by name,
// in that module, and which are left out and why. A caller who may see neither is refused.
func visibleModules(ctx context.Context, tenant, actor string) (fin, sea bool, left []LeftOut, err error) {
	for _, m := range []struct {
		module, bc string
		actions    []string
		ok         *bool
	}{{ModuleFIN, "fin", []string{finapi.ActionConfigureSettings, finapi.ActionApproveSettings}, &fin}, {ModuleSEA, "sea", []string{seaapi.ActionConfigureSettings, seaapi.ActionApproveSettings}, &sea}} {
		if holdsEither(ctx, tenant, actor, m.bc, m.actions...) == nil {
			*m.ok = true
			continue
		}
		left = append(left, LeftOut{Module: m.module, Reason: fmt.Sprintf("the caller holds neither %s:%s nor %s:%s, by name; ask for one of them to see %s's configuration",
			m.bc, m.actions[0], m.bc, m.actions[1], m.module)})
	}
	if !fin && !sea {
		return false, false, left, fmt.Errorf("%w: the configuration of a module is told to a holder of ConfigureSettings or ApproveSettings, by name, in that module, and the caller holds "+
			"neither in FIN or in SEA; ask for fin:ConfigureSettings, fin:ApproveSettings, sea:ConfigureSettings or sea:ApproveSettings", orgapi.ErrUnauthorized)
	}
	return fin, sea, left, nil
}

// ExportSettings answers a tenant's whole configuration in force, per module, with each catalogue's version, for each
// module the caller may see: a holder of ConfigureSettings or ApproveSettings, by name, in that module. The answer says
// which modules it left out and why; a caller who may see neither is refused.
func (t *SettingsTools) ExportSettings(ctx context.Context, tenant, actor string) (SettingsExport, error) {
	seeFin, seeSea, left, err := visibleModules(ctx, tenant, actor)
	if err != nil {
		return SettingsExport{}, err
	}
	out := SettingsExport{Format: ExportFormat, TenantID: tenant, ExportedAt: time.Now().UTC().Truncate(time.Microsecond), Modules: map[string]ExportedModule{}, LeftOut: left}
	if seeFin {
		if out.Modules[ModuleFIN], err = t.exportFin(ctx, tenant); err != nil {
			return out, err
		}
	}
	if seeSea {
		if out.Modules[ModuleSEA], err = t.exportSea(ctx, tenant); err != nil {
			return out, err
		}
	}
	return out, nil
}

func (t *SettingsTools) exportFin(ctx context.Context, tenant string) (ExportedModule, error) {
	cat, err := t.fin.GetFinSettingsCatalogue(ctx, finapi.GetFinSettingsCatalogue{})
	if err != nil {
		return ExportedModule{}, err
	}
	changes, err := t.fin.ListFinSettingsChanges(ctx, finapi.ListFinSettingsChanges{TenantID: tenant})
	if err != nil {
		return ExportedModule{}, err
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
			return module, err
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
	return module, nil
}

func (t *SettingsTools) exportSea(ctx context.Context, tenant string) (ExportedModule, error) {
	cat, err := t.sea.GetSeaSettingsCatalogue(ctx, seaapi.GetSeaSettingsCatalogue{})
	if err != nil {
		return ExportedModule{}, err
	}
	changes, err := t.sea.ListSeaSettingsChanges(ctx, seaapi.ListSeaSettingsChanges{TenantID: tenant})
	if err != nil {
		return ExportedModule{}, err
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
		got, err := t.sea.GetSeaSettings(ctx, seaapi.GetSeaSettings{TenantID: tenant, ScopeKind: kind, ScopeID: scope})
		if err != nil {
			return module, err
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
	return module, nil
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
// ProposeSeaSettingsChange: the usual approval applies), the items this environment does not know, notes on
// catalogues that are not at the version the file was exported from, and the modules left out because the caller may not
// see them. It proposes nothing.
type SettingsDiff struct {
	Fin     []finapi.FinSettingEntry
	Sea     []seaapi.SeaSettingEntry
	Unknown []UnknownItem
	Notes   []string
	LeftOut []LeftOut
}

// DiffSettingsImport reads an exported configuration and answers the entries that differ from what is in force for the
// tenant, per module the caller may see, and the items unknown here. The comparison is scope by scope: the company's
// entry against what is in force for the company (its own value, else the default), and a legal entity's entry against
// that legal entity's own entry, so that an entry the file has at a legal entity stays that legal entity's own, even when
// its value equals the company's here, unless this environment has the same entry at that legal entity. The file is
// refused when it is not JSON of this format. The caller sees the modules where they hold ConfigureSettings or
// ApproveSettings, by name; the answer says which it left out and why, and a caller who may see neither is refused. A module
// that the file's own LeftOut names, because its exporter could not see it, is named in the answer's LeftOut too, with why:
// the diff answers nothing about that module's configuration.
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
	seeFin, seeSea, left, err := visibleModules(ctx, tenant, actor)
	if err != nil {
		return SettingsDiff{}, err
	}
	out := SettingsDiff{LeftOut: left}
	// a module the file's own exporter could not see is not in the file: the diff answers nothing about it, and says so
	for _, l := range in.LeftOut {
		if _, carried := in.Modules[l.Module]; !carried && (l.Module == ModuleFIN || l.Module == ModuleSEA) {
			out.LeftOut = append(out.LeftOut, LeftOut{Module: l.Module, Reason: fmt.Sprintf("the file does not carry %s: its exporter might not see it (%s); this diff answers nothing about %s's "+
				"configuration, so an empty answer for %s does not mean that it matches: export %s again as a caller who sees it", l.Module, l.Reason, l.Module, l.Module, l.Module)})
		}
	}
	for module := range in.Modules {
		if module != ModuleFIN && module != ModuleSEA {
			out.Unknown = append(out.Unknown, UnknownItem{Module: module, Reason: "this environment has no module " + module + "; remove it from the file"})
		}
	}
	if m, ok := in.Modules[ModuleFIN]; ok && seeFin {
		if err := t.diffFin(ctx, tenant, m, &out); err != nil {
			return out, err
		}
	}
	if m, ok := in.Modules[ModuleSEA]; ok && seeSea {
		if err := t.diffSea(ctx, tenant, m, &out); err != nil {
			return out, err
		}
	}
	return out, nil
}

// ownEntryHere says whether what is in force here for the item may stand for the file's entry at the scope: for the
// company, what is in force for it (its own value, else the default); for a legal entity, only its own entry, not the
// company's value that it inherits and not a default.
func ownEntryHere(kind, id string, fromKind, fromID string, isDefault bool) bool {
	return kind == "TENANT" || (!isDefault && fromKind == kind && fromID == id)
}

func (t *SettingsTools) diffFin(ctx context.Context, tenant string, m ExportedModule, out *SettingsDiff) error {
	cat, err := t.fin.GetFinSettingsCatalogue(ctx, finapi.GetFinSettingsCatalogue{})
	if err != nil {
		return err
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
			return err
		}
		for _, it := range sc.Items {
			if why := unknownWhy("FIN", cat.Version, it, known); why != "" {
				out.Unknown = append(out.Unknown, UnknownItem{Module: ModuleFIN, Key: it.Key, Value: it.Value, Reason: why})
				continue
			}
			i := slices.IndexFunc(now.Items, func(c finapi.FinSetting) bool { return c.Key == it.Key })
			if i >= 0 && ownEntryHere(sc.ScopeKind, sc.ScopeID, now.Items[i].FromScopeKind, now.Items[i].FromScopeID, now.Items[i].Default) &&
				now.Items[i].Value == it.Value && sameThreshold(now.Items[i].Threshold, it) {
				continue
			}
			entry := finapi.FinSettingEntry{ScopeKind: sc.ScopeKind, ScopeID: sc.ScopeID, Key: it.Key, Value: it.Value}
			if it.ThresholdMinor != nil {
				entry.Threshold = &kernel.Money{Currency: kernel.CurrencyCode(it.ThresholdCurrency), Minor: *it.ThresholdMinor}
			}
			out.Fin = append(out.Fin, entry)
		}
	}
	return nil
}

func (t *SettingsTools) diffSea(ctx context.Context, tenant string, m ExportedModule, out *SettingsDiff) error {
	cat, err := t.sea.GetSeaSettingsCatalogue(ctx, seaapi.GetSeaSettingsCatalogue{})
	if err != nil {
		return err
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
			return err
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
			if i >= 0 && ownEntryHere(sc.ScopeKind, sc.ScopeID, now.Items[i].FromScopeKind, now.Items[i].FromScopeID, now.Items[i].Default) && now.Items[i].Value == it.Value {
				continue
			}
			out.Sea = append(out.Sea, seaapi.SeaSettingEntry{ScopeKind: sc.ScopeKind, ScopeID: sc.ScopeID, Key: it.Key, Value: it.Value})
		}
	}
	return nil
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

// ReviewQueue is what waits for review in a tenant, for a holder of the review permissions (V2.102, batch F): each module's part to that
// module's holders, nil for a part the actor may not see (LeftOut says which and why). It is read-only.
type ReviewQueue struct {
	TenantID string
	From, To time.Time
	// FinSelfApprovals is FIN's self-approved change sets of the period, with no review and with theirs, for a holder of fin:ReviewSettingsExceptions.
	FinSelfApprovals *finapi.FinSelfApprovalReviews
	// SeaSelfApprovals is SEA's, for a holder of sea:ReviewSettingsExceptions.
	SeaSelfApprovals *seaapi.SeaSelfApprovalReviews
	// FinAfterTheFact is, for each legal entity asked, the lines of the ended weeks of the period that no review covers and the ended weeks with no
	// review, for a holder of fin:ReviewAfterTheFact who also holds fin:ReadProfit.
	FinAfterTheFact []finapi.AfterTheFactPending
	// LeftOut says, for each part the actor may not see or that was not asked for, which and why.
	LeftOut []ReviewQueueLeftOut
}

// ReviewQueueLeftOut is a part of the review queue that is not answered, and why.
type ReviewQueueLeftOut struct {
	Part   string
	Reason string
}

// GetReviewQueue answers what waits for review in the tenant for the period from (included) to to (excluded): FIN's and SEA's self-approved
// change sets with no review (and the reviewed ones with their reviewer and conclusion), and, for each of the legal entities named, the
// weeks of the period that have ended: the lines of their lists that no review covers and the weeks with no review. Each part goes to the
// holders of its module's permission, by name: fin:ReviewSettingsExceptions, sea:ReviewSettingsExceptions, and fin:ReviewAfterTheFact with
// fin:ReadProfit. An actor who holds none is refused; the parts left out are named, with the reason. The period is at most 53 weeks.
func (t *SettingsTools) GetReviewQueue(ctx context.Context, tenant, actor string, from, to time.Time, legalEntities []string) (ReviewQueue, error) {
	out := ReviewQueue{TenantID: tenant, From: from, To: to}
	if tenant == "" || actor == "" || from.IsZero() || to.IsZero() || !to.After(from) {
		return out, fmt.Errorf("%w: the review queue asks for a tenant, an actor and a period, from before to", orgapi.ErrRejected)
	}
	leftOut := func(part string, err error) {
		out.LeftOut = append(out.LeftOut, ReviewQueueLeftOut{Part: part, Reason: err.Error()})
	}
	// the weeks that the period covers
	firstWeek, lastWeek := isoWeek(from), isoWeek(to.Add(-time.Nanosecond))
	f, ferr := t.fin.ListFinSelfApprovalReviews(ctx, finapi.ListFinSelfApprovalReviews{TenantID: tenant, ActorID: actor, From: from, To: to})
	s, serr := t.sea.ListSeaSelfApprovalReviews(ctx, seaapi.ListSeaSelfApprovalReviews{TenantID: tenant, ActorID: actor, From: from, To: to})
	if ferr != nil && !errors.Is(ferr, finapi.ErrUnauthorized) {
		return out, ferr
	}
	if serr != nil && !errors.Is(serr, seaapi.ErrUnauthorized) {
		return out, serr
	}
	if ferr == nil {
		out.FinSelfApprovals = &f
	} else {
		leftOut("FIN's self-approved change sets", errors.New("fin:ReviewSettingsExceptions is granted by name only and the actor does not hold it"))
	}
	if serr == nil {
		out.SeaSelfApprovals = &s
	} else {
		leftOut("SEA's self-approved change sets", errors.New("sea:ReviewSettingsExceptions is granted by name only and the actor does not hold it"))
	}
	holdsWeekly := identity.AuthorizeNamed(ctx, "fin", tenant, actor, finapi.ActionReviewAfterTheFact) == nil && identity.Authorize(ctx, "fin", tenant, actor, finapi.ActionReadProfit) == nil
	switch {
	case !holdsWeekly:
		leftOut("FIN's after-the-fact review", errors.New("fin:ReviewAfterTheFact is granted by name only, and fin:ReadProfit is needed to read the lines; the actor does not hold both"))
	case len(legalEntities) == 0:
		leftOut("FIN's after-the-fact review", errors.New("no legal entity was named: name the legal entities whose weeks are to be read"))
	default:
		for _, le := range legalEntities {
			p, err := t.fin.ListAfterTheFactPending(ctx, finapi.ListAfterTheFactPending{TenantID: tenant, ActorID: actor, LegalEntityID: le, FromWeek: firstWeek, ToWeek: lastWeek})
			if err != nil {
				return out, err
			}
			out.FinAfterTheFact = append(out.FinAfterTheFact, p)
		}
	}
	if ferr != nil && serr != nil && !holdsWeekly {
		return out, fmt.Errorf("%w: the review queue is told to a holder of fin:ReviewSettingsExceptions, sea:ReviewSettingsExceptions or fin:ReviewAfterTheFact (with fin:ReadProfit), "+
			"granted by name; ask for one of them: %v", orgapi.ErrUnauthorized, ferr)
	}
	return out, nil
}

// isoWeek is the ISO week, YYYY-Www, that contains the time in UTC.
func isoWeek(at time.Time) string {
	y, w := at.UTC().ISOWeek()
	return fmt.Sprintf("%04d-W%02d", y, w)
}
