package application

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	bentorepo "go.idp.xyz/idp-bento-go/repository"
	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/settings"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FIN V2.96: the settings (step C1a). The commands take the tenant's settings lock first, before they look for their
// own receipt (a second copy of a command that waited for the first answers its receipt, as a replay does), and read
// the whole ledger, because what a change set means depends on the others in force (whether it loosens, which version
// it makes, whether the configuration stays valid). They read FIN's clock only once they hold the lock, so that
// commands are stamped in the order they commit, and then commit the rows, the Outbox events and the receipt in one
// transaction. They record the system's time (T1), never a business time: the effective time a proposal names may be
// later, not earlier, and not more than a year later.

// SettingEntryAsk is an entry as asked: when it carries a parameter, the currency the amount is given in.
type SettingEntryAsk struct {
	settings.Entry
	Currency kernel.CurrencyCode
}

// writeSettings runs a settings command: the tenant's lock, then the receipt, then mutate with the time read under the
// lock.
func (d Deps) writeSettings(ctx context.Context, c Command, hash string, mutate func(context.Context, time.Time) (Receipt, aggregate, error)) (Receipt, error) {
	return d.writeAfter(ctx, c, hash, "FinSettings", func(txCtx context.Context) error { return d.Settings.Lock(txCtx, c.Scope.TenantID) },
		func(txCtx context.Context) (Receipt, aggregate, error) {
			return mutate(txCtx, d.Clock.Now().UTC().Truncate(time.Microsecond))
		})
}

// ProposeSettings proposes a change set of the entries named; one that only tightens or keeps is approved at once.
// Each legal entity an entry names must have a finance policy, which is also where its functional currency is, the one
// an amount parameter must be given in. The receipt's revision is the records the change set has: its proposal, and its
// approval when it only tightens (an approval and a cancellation come later). The notice names the legal entities whose
// own values stay looser than what the company's entries of the change set now say.
func (d Deps) ProposeSettings(ctx context.Context, c Command, asks []SettingEntryAsk, reason string, effective time.Time) (Receipt, error) {
	if d.Settings == nil || d.Policies == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, _, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	return d.writeSettings(ctx, c, hash, func(txCtx context.Context, now time.Time) (Receipt, aggregate, error) {
		ledger, err := d.Settings.Load(txCtx, c.Scope.TenantID)
		if err != nil {
			return Receipt{}, nil, err
		}
		entries := make([]settings.Entry, len(asks))
		for i, a := range asks {
			entries[i] = a.Entry
			if a.Scope.Kind != settings.KindLegalEntity {
				continue
			}
			p, found, err := d.Policies.Find(txCtx, c.Scope.TenantID, a.Scope.ID)
			switch {
			case err != nil:
				return Receipt{}, nil, err
			case !found:
				return Receipt{}, nil, fmt.Errorf("%w (legal entity %s)", ErrNoPolicy, a.Scope.ID)
			case a.Param != nil && a.Currency != p.State().FunctionalCurrency:
				return Receipt{}, nil, fmt.Errorf("%w: %s of %s is an amount in %s, the functional currency of legal entity %s; give it in %s",
					fin.ErrInvalid, a.Key, a.Value, a.Currency, a.Scope.ID, p.State().FunctionalCurrency)
			}
		}
		cat := FinSettingsCatalogue()
		cs, err := ledger.Propose(cat, settings.Proposal{ID: c.Subject, Entries: entries, Reason: reason, By: c.Scope.ActorID, At: now, Effective: effective})
		if err != nil {
			return Receipt{}, nil, err
		}
		if err = d.Settings.InsertChange(txCtx, c.Scope.TenantID, cs); err != nil {
			return Receipt{}, nil, err
		}
		r := Receipt{SubjectID: cs.ID, NewRevision: 1, Notice: exceptionsNotice(cat, ledger, cs)}
		if cs.Approval != nil {
			r.NewRevision, r.Version = 2, cs.Approval.Version
		}
		return r, SettingsProposed(c.Scope.TenantID, cs), nil
	})
}

// exceptionsNotice names the legal entities whose own values are looser than a company entry of the change set, which
// stay in force as approved exceptions; empty when there are none.
func exceptionsNotice(cat *settings.Catalogue, ledger *settings.Ledger, cs settings.ChangeSet) string {
	tentative := cs
	if tentative.Approval == nil {
		tentative.Approval = &settings.Approval{By: "", At: cs.At, Version: 1 << 30, InForce: cs.Effective}
	}
	after := settings.NewLedger(append(ledger.Sets(), tentative))
	var parts []string
	for _, x := range after.Exceptions(cat, cs.Effective) {
		for _, e := range cs.Entries {
			if e.Scope == settings.TenantScope && e.Key == x.Key {
				parts = append(parts, fmt.Sprintf("%s keeps its own %s %s (change set %s), looser than the company's %s", x.Scope.ID, x.Key, x.Value, x.ChangeID, x.TenantValue))
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "legal entities that stay looser than the company, as approved exceptions: " + strings.Join(parts, "; ") +
		"; to tighten one, propose a change set for that legal entity"
}

// settingsChange reads the whole ledger and the change set the command names, under the lock the command holds.
func (d Deps) settingsChange(ctx context.Context, c Command) (*settings.Ledger, settings.ChangeSet, error) {
	ledger, err := d.Settings.Load(ctx, c.Scope.TenantID)
	if err != nil {
		return nil, settings.ChangeSet{}, err
	}
	cs, ok := ledger.Find(c.Subject)
	if !ok {
		return nil, settings.ChangeSet{}, fmt.Errorf("%w: change set %s; list the tenant's change sets (ListFinSettingsChanges) for the IDs", bentorepo.ErrNotFound, c.Subject)
	}
	return ledger, cs, nil
}

// ApproveSettings approves a change set that waits for a second person: the approver is not its proposer.
func (d Deps) ApproveSettings(ctx context.Context, c Command) (Receipt, error) {
	if d.Settings == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, _, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	return d.writeSettings(ctx, c, hash, func(txCtx context.Context, now time.Time) (Receipt, aggregate, error) {
		ledger, _, err := d.settingsChange(txCtx, c)
		if err != nil {
			return Receipt{}, nil, err
		}
		a, err := ledger.Approve(FinSettingsCatalogue(), c.Subject, c.Scope.ActorID, now)
		if err != nil {
			return Receipt{}, nil, err
		}
		if err = d.Settings.InsertApproval(txCtx, c.Scope.TenantID, c.Subject, a); err != nil {
			return Receipt{}, nil, err
		}
		return Receipt{SubjectID: c.Subject, NewRevision: 2, Version: a.Version}, SettingsApproved(c.Scope.TenantID, c.Subject, a), nil
	})
}

// CancelSettings cancels a change set that is not in force yet, with a reason.
func (d Deps) CancelSettings(ctx context.Context, c Command, reason, role string) (Receipt, error) {
	if d.Settings == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, _, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	return d.writeSettings(ctx, c, hash, func(txCtx context.Context, now time.Time) (Receipt, aggregate, error) {
		ledger, cs, err := d.settingsChange(txCtx, c)
		if err != nil {
			return Receipt{}, nil, err
		}
		x, err := ledger.Cancel(FinSettingsCatalogue(), c.Subject, c.Scope.ActorID, reason, now)
		if err != nil {
			return Receipt{}, nil, err
		}
		x.Role = role
		if err = d.Settings.InsertCancellation(txCtx, c.Scope.TenantID, c.Subject, x); err != nil {
			return Receipt{}, nil, err
		}
		revision := int64(2)
		if cs.Approval != nil {
			revision = 3
		}
		return Receipt{SubjectID: c.Subject, NewRevision: revision}, SettingsCancelled(c.Scope.TenantID, c.Subject, x), nil
	})
}

// RefuseSettings is an approver's refusal of a change set that waits for approval, with a reason, recorded as a
// cancellation of kind refused.
func (d Deps) RefuseSettings(ctx context.Context, c Command, reason string) (Receipt, error) {
	if d.Settings == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, _, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	return d.writeSettings(ctx, c, hash, func(txCtx context.Context, now time.Time) (Receipt, aggregate, error) {
		ledger, _, err := d.settingsChange(txCtx, c)
		if err != nil {
			return Receipt{}, nil, err
		}
		x, err := ledger.Refuse(c.Subject, c.Scope.ActorID, reason, now)
		if err != nil {
			return Receipt{}, nil, err
		}
		if err = d.Settings.InsertCancellation(txCtx, c.Scope.TenantID, c.Subject, x); err != nil {
			return Receipt{}, nil, err
		}
		return Receipt{SubjectID: c.Subject, NewRevision: 2}, SettingsRefused(c.Scope.TenantID, c.Subject, x), nil
	})
}

// SettingsInForce is the settings of a scope at a time, with the version in force and the functional currency of a
// legal entity (empty for the company, which has none).
type SettingsInForce struct {
	At       time.Time
	Version  int
	Config   settings.Config
	Currency string
}

// SettingsAt reads what is in force for the scope at a time (the time of the read when at is zero), in one snapshot.
func (d Deps) SettingsAt(ctx context.Context, tenant fin.TenantID, scope settings.Scope, at time.Time) (SettingsInForce, error) {
	if d.Settings == nil || d.Clock == nil || d.Policies == nil {
		return SettingsInForce{}, ErrMissingDependency
	}
	if at.IsZero() {
		at = d.Clock.Now().UTC().Truncate(time.Microsecond)
	}
	out := SettingsInForce{At: at}
	err := d.Reader(ctx, func(txCtx context.Context) error {
		ledger, err := d.Settings.Read(txCtx, tenant)
		if err != nil {
			return err
		}
		if out.Config, out.Version, err = ledger.ConfigAt(FinSettingsCatalogue(), scope, at); err != nil {
			return err
		}
		if scope.Kind == settings.KindLegalEntity {
			p, found, err := d.Policies.Find(txCtx, tenant, scope.ID)
			switch {
			case err != nil:
				return err
			case found:
				out.Currency = string(p.State().FunctionalCurrency)
			}
		}
		return nil
	})
	return out, err
}

// SettingsChanges reads the change sets of the tenant in the order proposed, with the time they are read at.
func (d Deps) SettingsChanges(ctx context.Context, tenant fin.TenantID) ([]settings.ChangeSet, time.Time, error) {
	if d.Settings == nil || d.Clock == nil {
		return nil, time.Time{}, ErrMissingDependency
	}
	now := d.Clock.Now().UTC().Truncate(time.Microsecond)
	var sets []settings.ChangeSet
	err := d.Reader(ctx, func(txCtx context.Context) error {
		ledger, err := d.Settings.Read(txCtx, tenant)
		if err == nil {
			sets = slices.Clone(ledger.Sets())
		}
		return err
	})
	return sets, now, err
}

// RuleSettingsOf reads, for a query (since V2.97), the settings in force for a legal entity at FIN's clock now, with
// the version in force, in the caller's read-only snapshot: the pool or job the query reads is read in the same
// snapshot, and the answer says the version of that snapshot. A query takes no lock. A threshold is in the functional
// currency of the legal entity, as the change set that set it was checked to be given. With no settings store wired the
// standard profile is answered, version 0.
func (d Deps) RuleSettingsOf(ctx context.Context, tenant fin.TenantID, legalEntity string) (fin.RuleSettings, error) {
	if d.Settings == nil {
		return fin.RuleSettings{}, nil
	}
	if d.Clock == nil {
		return fin.RuleSettings{}, ErrMissingDependency
	}
	return d.rulesAt(ctx, tenant, legalEntity, d.Settings.Read, d.Clock.Now().UTC().Truncate(time.Microsecond))
}

// DecisionSettings reads the settings for a decision that FIN records (a preview, a submission, a close), inside the
// decision's own transaction: it takes the tenant's settings lock in shared mode, reads FIN's clock after that lock,
// and reads the ledger as of that time. The time is the system time the decision is stamped with (the caller stamps its
// records with it), so the version recorded is the version the ledger says was in force at the recorded time: a
// settings command stamps its time under the exclusive lock, so it is stamped before this time when it committed before
// the lock was granted and after it when it waited for this transaction. With no settings store wired the standard
// profile is answered, version 0.
func (d Deps) DecisionSettings(ctx context.Context, tenant fin.TenantID, legalEntity string) (fin.RuleSettings, time.Time, error) {
	if d.Clock == nil {
		return fin.RuleSettings{}, time.Time{}, ErrMissingDependency
	}
	if d.Settings == nil {
		return fin.RuleSettings{}, d.Clock.Now().UTC().Truncate(time.Microsecond), nil
	}
	if err := d.Settings.LockShared(ctx, tenant); err != nil {
		return fin.RuleSettings{}, time.Time{}, err
	}
	at := d.Clock.Now().UTC().Truncate(time.Microsecond)
	rules, err := d.rulesAt(ctx, tenant, legalEntity, d.Settings.Load, at)
	return rules, at, err
}

func (d Deps) rulesAt(ctx context.Context, tenant fin.TenantID, legalEntity string, load func(context.Context, fin.TenantID) (*settings.Ledger, error),
	at time.Time) (fin.RuleSettings, error) {
	ledger, err := load(ctx, tenant)
	if err != nil {
		return fin.RuleSettings{}, err
	}
	cfg, version, err := ledger.ConfigAt(FinSettingsCatalogue(), settings.Scope{Kind: settings.KindLegalEntity, ID: legalEntity}, at)
	if err != nil {
		return fin.RuleSettings{}, err
	}
	rules := fin.RuleSettings{Version: version, F1: cfg[SettingF1].Value, F2: cfg[SettingF2].Value, F3: cfg[SettingF3].Value, F6: cfg[SettingF6].Value,
		F7: cfg[SettingF7].Value}
	if rules.F1 == F1ThresholdValue {
		param := cfg[SettingF1].Param
		if param == nil { // the ledger refuses a threshold value without its amount; kept so that a rule never reads a missing one as 0
			return fin.RuleSettings{}, fmt.Errorf("%w: %s %s has no threshold; set the threshold in the legal entity's functional currency", fin.ErrInvalid, SettingF1, F1ThresholdValue)
		}
		rules.F1Threshold = *param
	}
	return rules, nil
}
