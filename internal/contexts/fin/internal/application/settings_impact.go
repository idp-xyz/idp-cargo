package application

import (
	"context"
	"fmt"
	"slices"
	"time"

	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/settings"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FIN V2.100: the impact of a settings change before it takes effect. The change is put into the ledger as if it were
// approved and in force now, and each rule that the settings steer is asked under the settings in force and under those
// the change would make, through the function a command asks it by; what answers differently is the impact. Nothing is
// written and no lock is taken. FIN's rows are read in one read-only snapshot; PHY's basis of each open pool is read
// before it (T3, as the other pool queries do), and PHY's basis of the consolidations the jobs have a share in just
// after it, to tell a former share from a share.

// ImpactAsk names a change set not in force yet, or the entries of a proposal.
type ImpactAsk struct {
	ChangeID string
	Entries  []SettingEntryAsk
	Actor    string
}

// ItemChange is an item of a legal entity's settings that would change.
type ItemChange struct {
	Key       string
	From, To  string
	FromParam *int64
	ToParam   *int64
}

// EntityImpact is a legal entity whose resolved settings differ, with its currency (the threshold's).
type EntityImpact struct {
	LegalEntityID string
	Currency      kernel.CurrencyCode
	Changes       []ItemChange
}

// PreviewRef is an unsubmitted preview of an open pool.
type PreviewRef struct {
	PoolID, LegalEntityID string
	PreviewNo             int
	By                    string
	At                    time.Time
	SettingsVersion       int
}

// CloseImpact is a pool whose close would be refused or allowed.
type CloseImpact struct {
	PoolID, LegalEntityID string
	NowAllowed            bool
	Refusal               string // of the one that is refused
}

// OneStepImpact is a pool that would become able or unable to be apportioned in one step.
type OneStepImpact struct {
	PoolID, LegalEntityID string
	NowAble, AfterAble    bool
	Refusal               string // of the one that is refused
}

// DefaultImpact is a line whose pending former shares would be defaulted (F8) or would stop being.
type DefaultImpact struct {
	PoolID, LegalEntityID string
	LineNo                int
	Houses                []string
	Defaulted             bool // after the change
}

// CarriedHouseImpact is a carried attribution's amounts now and after, in the line's currency.
type CarriedHouseImpact struct {
	ShipmentID                string
	LossNow, ExceptionNow     int64
	LossAfter, ExceptionAfter int64
	Known                     bool // the amounts after are known (not when it has to be stated again)
}

// CarriedImpact is a line whose carried attributions would be treated differently (F9): Kind now and after is KEPT,
// RESCALED or STATE_AGAIN.
type CarriedImpact struct {
	PoolID, LegalEntityID string
	LineNo                int
	Currency              kernel.CurrencyCode
	Now, After            string
	Houses                []CarriedHouseImpact
}

// CheckImpact is a rule of the submission of an unsubmitted preview that would be checked differently (F1, F2, F3).
type CheckImpact struct {
	PoolID, LegalEntityID string
	PreviewNo             int
	fin.PreviewCheck
}

// JobImpact is a job whose profit would change (F6).
type JobImpact struct {
	JobID, ShipmentID, LegalEntityID string
	Currency                         string
	Now, After                       int64
	BasisNow, BasisAfter             string
}

// SettingsImpact is the answer.
type SettingsImpact struct {
	At                       time.Time
	VersionNow, VersionAfter int
	Entities                 []EntityImpact
	StalePreviews            []PreviewRef
	Closes                   []CloseImpact
	Jobs                     []JobImpact
	Defaults                 []DefaultImpact
	Carried                  []CarriedImpact
	OneStep                  []OneStepImpact
	Checks                   []CheckImpact
	NotAssessed              []string
}

// hypothetical puts the change into the ledger as approved and in force at now: the named change set, which is not in
// force, or the proposal of the entries, checked as a proposal is.
func (d Deps) hypothetical(ledger *settings.Ledger, cat *settings.Catalogue, ask ImpactAsk, currencies map[string]string,
	now time.Time) (*settings.Ledger, string, error) {
	var cs settings.ChangeSet
	switch {
	case ask.ChangeID != "" && len(ask.Entries) > 0, ask.ChangeID == "" && len(ask.Entries) == 0:
		return nil, "", fmt.Errorf("%w: name the change set not in force yet (ChangeID) or the entries of a proposal (Entries), not both and not neither; "+
			"ListFinSettingsChanges lists the change sets", fin.ErrInvalid)
	case ask.ChangeID != "":
		found, ok := ledger.Find(ask.ChangeID)
		switch {
		case !ok:
			return nil, "", fmt.Errorf("%w: there is no change set %s; ListFinSettingsChanges lists the change sets", fin.ErrInvalid, ask.ChangeID)
		case found.Status(now) != settings.StatusProposed && found.Status(now) != settings.StatusApproved:
			return nil, "", fmt.Errorf("%w: change set %s is %s; the impact is told of a change set that is proposed or approved and not in force yet",
				fin.ErrInvalid, ask.ChangeID, found.Status(now))
		}
		cs = found
	default:
		entries := make([]settings.Entry, len(ask.Entries))
		for i, a := range ask.Entries {
			entries[i] = a.Entry
			if a.Scope.Kind != settings.KindLegalEntity {
				continue
			}
			currency, known := currencies[a.Scope.ID]
			switch {
			case !known:
				return nil, "", fmt.Errorf("%w (legal entity %s)", ErrNoPolicy, a.Scope.ID)
			case a.Param != nil && string(a.Currency) != currency:
				return nil, "", fmt.Errorf("%w: %s of %s is an amount in %s, the functional currency of legal entity %s; give it in %s",
					fin.ErrInvalid, a.Key, a.Value, a.Currency, a.Scope.ID, currency)
			}
		}
		var err error
		if cs, err = ledger.Propose(cat, settings.Proposal{ID: "impact-of-a-proposal", Entries: entries, Reason: "the impact of a proposal", By: ask.Actor, At: now,
			Effective: now}); err != nil {
			return nil, "", err
		}
	}
	version := 0
	for _, c := range ledger.Sets() {
		if c.Approval != nil {
			version = max(version, c.Approval.Version)
		}
	}
	cs.Approval = &settings.Approval{By: "impact", At: now, Version: version + 1, InForce: now}
	sets := slices.DeleteFunc(ledger.Sets(), func(c settings.ChangeSet) bool { return c.ID == cs.ID })
	return settings.NewLedger(append(sets, cs)), cs.ID, nil
}

func rulesDiffer(a, b fin.RuleSettings) bool {
	a.Version, b.Version = 0, 0
	return a != b
}

type poolRef struct{ pool, consolidation, legalEntity string }

// SettingsChangeImpact answers what would behave differently if the change were in force now. It reads FIN's clock
// once, and changes nothing.
func (d Deps) SettingsChangeImpact(ctx context.Context, tenant fin.TenantID, ask ImpactAsk) (SettingsImpact, error) {
	if d.Settings == nil || d.Clock == nil || d.Policies == nil || d.Pools == nil || d.Jobs == nil || d.Creditors == nil || d.Physical == nil {
		return SettingsImpact{}, ErrMissingDependency
	}
	now := d.Clock.Now().UTC().Truncate(time.Microsecond)
	cat := FinSettingsCatalogue()
	// the open pools, and the consolidations of those of the legal entities whose settings would change
	var refs []poolRef
	if err := d.Reader(ctx, func(txCtx context.Context) error {
		ledger, err := d.Settings.Read(txCtx, tenant)
		if err != nil {
			return err
		}
		entities, err := d.Policies.LegalEntities(txCtx, tenant)
		if err != nil {
			return err
		}
		after, _, err := d.hypothetical(ledger, cat, ask, entities, now)
		if err != nil {
			return err
		}
		changed, err := changedEntities(cat, ledger, after, entities, now)
		if err != nil {
			return err
		}
		pools, _, err := d.Pools.OpenPools(txCtx, tenant)
		if err != nil {
			return err
		}
		for _, p := range pools {
			if st := p.State(); slices.ContainsFunc(changed, func(e EntityImpact) bool { return e.LegalEntityID == st.LegalEntityID }) {
				refs = append(refs, poolRef{st.PoolID, st.ConsolidationID, st.LegalEntityID})
			}
		}
		return nil
	}); err != nil {
		return SettingsImpact{}, err
	}
	bases := map[string]fin.CostBasis{}
	for _, r := range refs {
		if _, read := bases[r.consolidation]; read {
			continue
		}
		b, err := d.Physical.CostBasis(ctx, tenant, r.consolidation)
		if err != nil {
			return SettingsImpact{}, err
		}
		bases[r.consolidation] = b
	}

	out := SettingsImpact{At: now}
	type nowAfter struct{ now, after fin.RuleSettings }
	rules := map[string]nowAfter{}
	var reads []struct {
		read  ProfitRead
		le    string
		pairs nowAfter
	}
	err := d.Reader(ctx, func(txCtx context.Context) error {
		ledger, err := d.Settings.Read(txCtx, tenant)
		if err != nil {
			return err
		}
		entities, err := d.Policies.LegalEntities(txCtx, tenant)
		if err != nil {
			return err
		}
		after, _, err := d.hypothetical(ledger, cat, ask, entities, now)
		if err != nil {
			return err
		}
		if out.Entities, err = changedEntities(cat, ledger, after, entities, now); err != nil {
			return err
		}
		for le := range entities {
			scope := settings.Scope{Kind: settings.KindLegalEntity, ID: le}
			cn, vn, err := ledger.ConfigAt(cat, scope, now)
			if err != nil {
				return err
			}
			ca, va, err := after.ConfigAt(cat, scope, now)
			if err != nil {
				return err
			}
			var pair nowAfter
			if pair.now, err = ruleSettingsFrom(cn, vn); err != nil {
				return err
			}
			if pair.after, err = ruleSettingsFrom(ca, va); err != nil {
				return err
			}
			rules[le] = pair
		}
		if _, out.VersionNow, err = ledger.ConfigAt(cat, settings.TenantScope, now); err != nil {
			return err
		}
		if _, out.VersionAfter, err = after.ConfigAt(cat, settings.TenantScope, now); err != nil {
			return err
		}
		pools, _, err := d.Pools.OpenPools(txCtx, tenant)
		if err != nil {
			return err
		}
		for _, pool := range pools {
			st := pool.State()
			for _, v := range pool.UnsubmittedPreviews() { // the version is the tenant's: every one becomes stale
				out.StalePreviews = append(out.StalePreviews, PreviewRef{PoolID: st.PoolID, LegalEntityID: st.LegalEntityID, PreviewNo: v.No, By: v.By, At: v.At,
					SettingsVersion: v.SettingsVersion})
			}
			r := rules[st.LegalEntityID]
			if !rulesDiffer(r.now, r.after) {
				continue
			}
			basis, read := bases[st.ConsolidationID]
			if !read {
				out.NotAssessed = append(out.NotAssessed, st.PoolID)
				continue
			}
			consumed, err := d.Creditors.Consumed(txCtx, tenant, fin.TargetPoolCost, st.PoolID)
			if err != nil {
				return err
			}
			pool.Matched(consumed)
			shipments := shipmentsOf(basis)
			shipments = append(shipments, pool.HousesToAttribute(basis)...)
			jobs, revenue, err := d.ReadOpenJobs(txCtx, tenant, st.LegalEntityID, shipments)
			if err != nil {
				return err
			}
			pool.HouseRevenue(revenue)
			out.addPool(pool, r.now, r.after, basis, jobs, revenue)
		}
		for le, r := range rules {
			if r.now.F6 == r.after.F6 {
				continue
			}
			for last := ""; ; {
				jobs, revisions, err := d.Jobs.PageOf(txCtx, tenant, le, last, 500)
				if err != nil {
					return err
				}
				for i, j := range jobs {
					read, err := d.ReadProfit(txCtx, tenant, j, revisions[i], r.now)
					if err != nil {
						return err
					}
					reads = append(reads, struct {
						read  ProfitRead
						le    string
						pairs nowAfter
					}{read, le, r})
				}
				if len(jobs) < 500 {
					break
				}
				last = jobs[len(jobs)-1].State().JobID
			}
		}
		return nil
	})
	if err != nil {
		return SettingsImpact{}, err
	}
	var shares []PoolShare
	for _, r := range reads {
		shares = append(shares, r.read.Shares...)
	}
	jobBases, err := d.FormerBases(ctx, tenant, shares)
	if err != nil {
		return SettingsImpact{}, err
	}
	for _, r := range reads {
		was := r.read.Figures(jobBases)
		r.read.Rules = r.pairs.after
		will := r.read.Figures(jobBases)
		if was.Profit != will.Profit {
			st := r.read.Job.State()
			out.Jobs = append(out.Jobs, JobImpact{JobID: st.JobID, ShipmentID: st.ShipmentID, LegalEntityID: r.le, Currency: was.Currency, Now: was.Profit, After: will.Profit,
				BasisNow: was.Basis, BasisAfter: will.Basis})
		}
	}
	slices.SortFunc(out.Jobs, func(a, b JobImpact) int { return compareText(a.JobID, b.JobID) })
	return out, nil
}

func compareText(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func shipmentsOf(basis fin.CostBasis) []string {
	var out []string
	for _, h := range basis.Houses {
		out = append(out, h.ShipmentID)
	}
	return out
}

// changedEntities lists, in legal entity order, the legal entities whose resolved settings differ with the change.
func changedEntities(cat *settings.Catalogue, now, after *settings.Ledger, entities map[string]string, at time.Time) ([]EntityImpact, error) {
	var out []EntityImpact
	for _, le := range slices.Sorted(mapKeys(entities)) {
		scope := settings.Scope{Kind: settings.KindLegalEntity, ID: le}
		cn, _, err := now.ConfigAt(cat, scope, at)
		if err != nil {
			return nil, err
		}
		ca, _, err := after.ConfigAt(cat, scope, at)
		if err != nil {
			return nil, err
		}
		e := EntityImpact{LegalEntityID: le, Currency: kernel.CurrencyCode(entities[le])}
		for _, it := range cat.Items {
			a, b := cn[it.Key], ca[it.Key]
			if a.Value != b.Value || !sameParam(a.Param, b.Param) {
				e.Changes = append(e.Changes, ItemChange{Key: it.Key, From: a.Value, To: b.Value, FromParam: a.Param, ToParam: b.Param})
			}
		}
		if len(e.Changes) > 0 {
			out = append(out, e)
		}
	}
	return out, nil
}

func sameParam(a, b *int64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

func mapKeys(m map[string]string) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// addPool asks the pool's rules under both settings and notes what answers differently.
func (o *SettingsImpact) addPool(pool *fin.ConsolCostPool, now, after fin.RuleSettings, basis fin.CostBasis, jobs map[string]string, revenue map[string]map[string]int64) {
	st := pool.State()
	// F7: the close
	if was, will := pool.CloseRefusal(now, basis), pool.CloseRefusal(after, basis); (was == "") != (will == "") {
		refusal := was + will
		o.Closes = append(o.Closes, CloseImpact{PoolID: st.PoolID, LegalEntityID: st.LegalEntityID, NowAllowed: was == "", Refusal: refusal})
	}
	// F4 (and what else a one-step apportionment asks)
	if was, will := pool.OneStepRefusal(now, basis, jobs, revenue), pool.OneStepRefusal(after, basis, jobs, revenue); (was == "") != (will == "") {
		o.OneStep = append(o.OneStep, OneStepImpact{PoolID: st.PoolID, LegalEntityID: st.LegalEntityID, NowAble: was == "", AfterAble: will == "", Refusal: was + will})
	}
	// F8 and F9: the next preview of a line, with nobody stating an attribution
	if now.F8 != after.F8 || now.F9 != after.F9 {
		was, will := pool.LineOutcomes(now, basis, jobs), pool.LineOutcomes(after, basis, jobs)
		for _, a := range was {
			bi := slices.IndexFunc(will, func(o fin.LineOutcome) bool { return o.LineNo == a.LineNo })
			if bi < 0 {
				continue
			}
			b := will[bi]
			if (len(a.Defaulted) > 0) != (len(b.Defaulted) > 0) {
				houses := a.Defaulted
				if len(b.Defaulted) > 0 {
					houses = b.Defaulted
				}
				o.Defaults = append(o.Defaults, DefaultImpact{PoolID: st.PoolID, LegalEntityID: st.LegalEntityID, LineNo: a.LineNo, Houses: houses, Defaulted: len(b.Defaulted) > 0})
			}
			if kindNow, kindAfter := carryKind(a), carryKind(b); kindNow != kindAfter && len(a.InForce) > 0 {
				c := CarriedImpact{PoolID: st.PoolID, LegalEntityID: st.LegalEntityID, LineNo: a.LineNo, Currency: a.Currency, Now: kindNow, After: kindAfter}
				for _, h := range a.InForce {
					x := CarriedHouseImpact{ShipmentID: h.ShipmentID, LossNow: h.Loss, ExceptionNow: h.Exception, LossAfter: h.Loss, ExceptionAfter: h.Exception, Known: !b.Restate}
					for _, r := range a.Carried {
						if r.ShipmentID == h.ShipmentID {
							x.LossNow, x.ExceptionNow = r.Loss, r.Exception
						}
					}
					for _, r := range b.Carried {
						if r.ShipmentID == h.ShipmentID {
							x.LossAfter, x.ExceptionAfter = r.Loss, r.Exception
						}
					}
					c.Houses = append(c.Houses, x)
				}
				o.Carried = append(o.Carried, c)
			}
		}
	}
	// F1, F2 and F3: the submission of each unsubmitted preview
	for _, v := range pool.UnsubmittedPreviews() {
		for _, c := range pool.PreviewChecks(now, after, basis, v) {
			o.Checks = append(o.Checks, CheckImpact{PoolID: st.PoolID, LegalEntityID: st.LegalEntityID, PreviewNo: v.No, PreviewCheck: c})
		}
	}
}

// carryKind is how the carried attributions of a line come out of its next preview.
func carryKind(o fin.LineOutcome) string {
	switch {
	case o.Restate:
		return "STATE_AGAIN"
	case slices.ContainsFunc(o.Carried, func(h fin.CarriedHouse) bool { return h.ByRatio }):
		return "RESCALED"
	}
	return "KEPT"
}
