package application

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	bentorepo "go.idp.xyz/idp-bento-go/repository"
	sea "go.idp.xyz/idp-cargo/internal/contexts/sea/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/settings"
)

// SEA V2.100: SEA's settings, as FIN's (V2.96) run in FIN. The commands take the tenant's settings lock first, before
// they look for their own receipt (a second copy of a command that waited for the first answers its receipt, as a
// replay does), and read the whole ledger, because what a change set means depends on the others in force (whether it
// loosens, which version it makes, whether the configuration stays valid). They read SEA's clock only once they hold
// the lock, so that commands are stamped in the order they commit, and then commit the rows, the Outbox events and the
// receipt in one transaction. They record the system's time (T1), never a business time: the effective time a proposal
// names may be later, not earlier, and not more than 366 days later.

// SettingsStore keeps the change sets of SEA's settings: a proposal with its entries, an approval and a cancellation,
// each a row of its own that is only inserted. Lock and LockShared take the tenant's settings lock until the
// transaction ends: a settings command takes it exclusively, and the release of a conflict, which reads the settings,
// in shared mode. Load reads the ledger in a write transaction and Read in a query's snapshot.
type SettingsStore interface {
	Lock(ctx context.Context, tenant sea.TenantID) error
	LockShared(ctx context.Context, tenant sea.TenantID) error
	Load(ctx context.Context, tenant sea.TenantID) (*settings.Ledger, error)
	Read(ctx context.Context, tenant sea.TenantID) (*settings.Ledger, error)
	InsertChange(ctx context.Context, tenant sea.TenantID, c settings.ChangeSet) error
	InsertApproval(ctx context.Context, tenant sea.TenantID, id string, a settings.Approval) error
	InsertCancellation(ctx context.Context, tenant sea.TenantID, id string, c settings.Cancellation) error
	// KnowsLegalEntity says whether SEA knows the legal entity: it has an HBL series or a movement.
	KnowsLegalEntity(ctx context.Context, tenant sea.TenantID, legalEntity string) (bool, error)
	// LegalEntities lists the legal entities SEA knows, in the order of their IDs.
	LegalEntities(ctx context.Context, tenant sea.TenantID) ([]string, error)
	// OpenMovements lists the OPEN movements of a legal entity, in the order of their IDs.
	OpenMovements(ctx context.Context, tenant sea.TenantID, legalEntity string) ([]sea.MovementID, error)
}

// seaWords says the shared ledger's refusals in SEA's words: the ledger names FIN's commands and clock in the ways out
// it gives (it was written for FIN), and a way out that names a command SEA does not have is no way out. The refusal
// keeps its kinds.
type seaWords struct{ err error }

var seaReplacer = strings.NewReplacer("CancelFinSettingsChange", "CancelSeaSettingsChange", "ListFinSettingsChanges", "ListSeaSettingsChanges",
	"FIN's clock", "SEA's clock", "FIN's own call", "SEA's own call")

func (e seaWords) Error() string { return seaReplacer.Replace(e.err.Error()) }
func (e seaWords) Unwrap() error { return e.err }

// inSeaWords puts a refusal of the ledger into SEA's words; any other error is as it was.
func inSeaWords(err error) error {
	if err != nil && errors.Is(err, settings.ErrRefused) {
		return seaWords{err}
	}
	return err
}

// writeSettings runs a settings command: the tenant's lock, then the receipt, then mutate with the time read
// under the lock.
func (d Deps) writeSettings(ctx context.Context, c Command, hash string, mutate func(context.Context, time.Time) (Receipt, aggregate, error)) (Receipt, error) {
	return d.writeAfter(ctx, c, hash, "SeaSettings", func(txCtx context.Context) error { return d.Settings.Lock(txCtx, c.Scope.TenantID) },
		func(txCtx context.Context) (Receipt, []aggregate, error) {
			r, agg, err := mutate(txCtx, d.Clock.Now().UTC().Truncate(time.Microsecond))
			return r, []aggregate{agg}, inSeaWords(err)
		})
}

// ProposeSettings proposes a change set of the entries named; one that only tightens or keeps is approved at once. Each
// legal entity an entry names must be one SEA knows. The receipt's revision is the records the change set has: its
// proposal, and its approval when it only tightens (an approval and a cancellation come later). The notice names the
// legal entities whose own values stay looser than what the company's entries of the change set now say.
func (d Deps) ProposeSettings(ctx context.Context, c Command, entries []settings.Entry, reason string, effective time.Time) (Receipt, error) {
	if d.Settings == nil {
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
		for _, e := range entries {
			if e.Scope.Kind != settings.KindLegalEntity {
				continue
			}
			known, err := d.Settings.KnowsLegalEntity(txCtx, c.Scope.TenantID, e.Scope.ID)
			switch {
			case err != nil:
				return Receipt{}, nil, err
			case !known:
				return Receipt{}, nil, unknownLegalEntity(e.Scope.ID)
			}
		}
		cat := SeaSettingsCatalogue()
		cs, err := ledger.Propose(cat, settings.Proposal{ID: c.Subject, Entries: entries, Reason: reason, By: c.Scope.ActorID, At: now, Effective: effective})
		if err != nil {
			return Receipt{}, nil, err
		}
		if err = d.Settings.InsertChange(txCtx, c.Scope.TenantID, cs); err != nil {
			return Receipt{}, nil, err
		}
		r := Receipt{SubjectID: cs.ID, NewRevision: 1, Notice: exceptionsNotice(cat, ledger, cs)}
		if cs.Approval != nil {
			r.NewRevision, r.SettingsVersion = 2, cs.Approval.Version
		}
		return r, SettingsProposed(c.Scope.TenantID, cs), nil
	})
}

func unknownLegalEntity(id string) error {
	return fmt.Errorf("%w: %s; register its HBL series (RegisterHblSeries) or open a movement of it first, or check the ID of the legal entity",
		ErrUnknownLegalEntity, id)
}

// exceptionsNotice names the legal entities whose own values are looser than a company entry of the change set, which
// stay in force as approved exceptions; empty when there are none.
func exceptionsNotice(cat *settings.Catalogue, ledger *settings.Ledger, cs settings.ChangeSet) string {
	after := settings.NewLedger(append(ledger.Sets(), tentative(cs, cs.Effective)))
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
	return exceptionsText(parts)
}

// maxNotice is what the receipt's notice holds: 4000 characters; the text keeps within it with room to spare.
const maxNotice = 3900

// exceptionsText lists as many of the legal entities as fit in the receipt, then says how many more there are and where
// to read them, so that a proposal is never refused for the length of its notice.
func exceptionsText(parts []string) string {
	const head = "legal entities that stay looser than the company, as approved exceptions: "
	const tail = "; to tighten one, propose a change set for that legal entity"
	kept, size := 0, len(head)+len(tail)
	for kept < len(parts) {
		more := fmt.Sprintf("; and %d more legal entities; see %s", len(parts)-kept-1, "GetSeaSettings")
		next := size + len(parts[kept]) + 2
		if kept < len(parts)-1 && next+len(more) > maxNotice || kept == len(parts)-1 && next > maxNotice {
			break
		}
		size, kept = next, kept+1
	}
	text := head + strings.Join(parts[:kept], "; ")
	if kept < len(parts) {
		text += fmt.Sprintf("; and %d more legal entities; see %s", len(parts)-kept, "GetSeaSettings")
	}
	return text + tail
}

// tentative is the change set as if it were approved, at its own approval when it has one: a proposal that waits is
// given the last version and takes effect at the later of its effective time and now.
func tentative(cs settings.ChangeSet, now time.Time) settings.ChangeSet {
	if cs.Approval == nil {
		cs.Approval = &settings.Approval{At: cs.At, Version: 1 << 30, InForce: laterOf(cs.Effective, now)}
	}
	return cs
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// settingsChange reads the whole ledger and the change set the command names, under the lock the command holds.
func (d Deps) settingsChange(ctx context.Context, c Command) (*settings.Ledger, settings.ChangeSet, error) {
	ledger, err := d.Settings.Load(ctx, c.Scope.TenantID)
	if err != nil {
		return nil, settings.ChangeSet{}, err
	}
	cs, ok := ledger.Find(c.Subject)
	if !ok {
		return nil, settings.ChangeSet{}, fmt.Errorf("%w: change set %s; list the tenant's change sets (ListSeaSettingsChanges) for the IDs", bentorepo.ErrNotFound, c.Subject)
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
		a, err := ledger.Approve(SeaSettingsCatalogue(), c.Subject, c.Scope.ActorID, now)
		if err != nil {
			return Receipt{}, nil, err
		}
		if err = d.Settings.InsertApproval(txCtx, c.Scope.TenantID, c.Subject, a); err != nil {
			return Receipt{}, nil, err
		}
		return Receipt{SubjectID: c.Subject, NewRevision: 2, SettingsVersion: a.Version}, SettingsApproved(c.Scope.TenantID, c.Subject, a), nil
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
		x, err := ledger.Cancel(SeaSettingsCatalogue(), c.Subject, c.Scope.ActorID, reason, now)
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

// DecisionSettings reads the settings for a decision that SEA records (the release of a conflict with a carrier's
// loading report), inside the decision's own transaction: it takes the tenant's settings lock in shared mode, reads
// SEA's clock after that lock, and reads the ledger as of that time. The time is the system time the decision is
// stamped with (the release's entered-at), so the version recorded is the version the ledger says was in force at the
// time recorded: a settings command stamps its time under the exclusive lock, so it is stamped before this time when it
// committed before the lock was granted and after it when it waited for this transaction. When the clock is behind the
// latest time the ledger records, the decision is timed at that latest time (V2.100). The legal entity must be one the
// movement names. With no settings store wired the standard profile is answered, version 0.
func (d Deps) DecisionSettings(ctx context.Context, tenant sea.TenantID, legalEntity string) (sea.RuleSettings, error) {
	if d.Clock == nil {
		return sea.RuleSettings{}, ErrMissingDependency
	}
	if d.Settings == nil {
		return sea.RuleSettings{S1: sea.S1WithReason, At: d.Clock.Now().UTC().Truncate(time.Microsecond)}, nil
	}
	if strings.TrimSpace(legalEntity) == "" {
		return sea.RuleSettings{}, fmt.Errorf("%w: the movement names no legal entity, so SEA cannot tell which settings steer the release; "+
			"this is SEA's own data, so ask the platform's owners", sea.ErrInvalidChange)
	}
	if err := d.Settings.LockShared(ctx, tenant); err != nil {
		return sea.RuleSettings{}, err
	}
	at := d.Clock.Now().UTC().Truncate(time.Microsecond)
	ledger, err := d.Settings.Load(ctx, tenant)
	if err != nil {
		return sea.RuleSettings{}, err
	}
	if latest := ledger.Latest(); latest.After(at) {
		at = latest // a clock behind a change already committed: the decision is timed after it, and reads it
	}
	cfg, version, err := ledger.ConfigAt(SeaSettingsCatalogue(), settings.Scope{Kind: settings.KindLegalEntity, ID: legalEntity}, at)
	if err != nil {
		return sea.RuleSettings{}, inSeaWords(err)
	}
	return sea.RuleSettings{Version: version, S1: cfg[SettingS1].Value, At: at}, nil
}

// SettingsInForce is the settings of a scope at a time, with the version in force.
type SettingsInForce struct {
	At      time.Time
	Version int
	Config  settings.Config
}

// SettingsAt reads what is in force for the scope at a time (the time of the read when at is zero), in one snapshot.
func (r Reads) SettingsAt(ctx context.Context, tenant sea.TenantID, scope settings.Scope, at time.Time) (SettingsInForce, error) {
	if r.Settings == nil || r.Clock == nil {
		return SettingsInForce{}, ErrMissingDependency
	}
	if tenant == "" {
		return SettingsInForce{}, ErrInvalidQuery
	}
	if at.IsZero() {
		at = r.Clock.Now().UTC().Truncate(time.Microsecond)
	}
	out := SettingsInForce{At: at}
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		ledger, err := r.Settings.Read(txCtx, tenant)
		if err != nil {
			return err
		}
		out.Config, out.Version, err = ledger.ConfigAt(SeaSettingsCatalogue(), scope, at)
		return inSeaWords(err)
	})
	return out, err
}

// SettingsChanges reads the change sets of the tenant in the order proposed, with the time they are read at.
func (r Reads) SettingsChanges(ctx context.Context, tenant sea.TenantID) ([]settings.ChangeSet, time.Time, error) {
	if r.Settings == nil || r.Clock == nil {
		return nil, time.Time{}, ErrMissingDependency
	}
	if tenant == "" {
		return nil, time.Time{}, ErrInvalidQuery
	}
	now := r.Clock.Now().UTC().Truncate(time.Microsecond)
	var sets []settings.ChangeSet
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		ledger, err := r.Settings.Read(txCtx, tenant)
		if err == nil {
			sets = slices.Clone(ledger.Sets())
		}
		return err
	})
	return sets, now, err
}

// ImpactConflict is an open conflict with a carrier's loading report, on a container of an open movement.
type ImpactConflict struct {
	Movement sea.MovementID
	Visit    sea.VisitID
	Number   string
	FactNo   int
}

// ImpactEntity is a legal entity whose S1 would differ, with its open conflicts.
type ImpactEntity struct {
	LegalEntity string
	From, To    string
	Conflicts   []ImpactConflict
}

// SettingsImpact is what a change set not yet in force would change.
type SettingsImpact struct {
	ChangeID    string
	TakesEffect time.Time
	Entities    []ImpactEntity
}

// SettingsImpact answers, in one snapshot, what a change set would change when it takes effect: the change set named
// (proposed and waiting, or approved with a time to come) or, with no ID, the entries asked about, which are checked as
// a proposal is. It takes effect at its effective time, or now when that is earlier (a change set waiting for approval
// can be approved now at the earliest). For each legal entity SEA knows whose S1 then differs from what it would be
// without the change set, it lists from what to what and the open conflicts of the legal entity's open movements (the
// ones a release would clear), so that who could release them before and after can be told. It writes nothing.
func (r Reads) SettingsImpact(ctx context.Context, tenant sea.TenantID, changeID string, entries []settings.Entry) (SettingsImpact, error) {
	if r.Settings == nil || r.Clock == nil || r.Movements == nil || r.Visits == nil {
		return SettingsImpact{}, ErrMissingDependency
	}
	if tenant == "" || (changeID == "") == (len(entries) == 0) {
		return SettingsImpact{}, fmt.Errorf("%w: name a change set (ChangeID) or the entries to ask about (Entries), one of the two", ErrInvalidQuery)
	}
	now := r.Clock.Now().UTC().Truncate(time.Microsecond)
	cat := SeaSettingsCatalogue()
	out := SettingsImpact{ChangeID: changeID}
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		ledger, err := r.Settings.Read(txCtx, tenant)
		if err != nil {
			return err
		}
		var cs settings.ChangeSet
		if changeID != "" {
			var ok bool
			if cs, ok = ledger.Find(changeID); !ok {
				return fmt.Errorf("%w: change set %s; list the tenant's change sets (ListSeaSettingsChanges) for the IDs", bentorepo.ErrNotFound, changeID)
			}
			if st := cs.Status(now); st != settings.StatusProposed && st != settings.StatusApproved {
				return fmt.Errorf("%w: change set %s is %s, so it has no impact still to come; ask about a change set that is proposed or approved "+
					"with a time to come, or give the entries to ask about", ErrInvalidQuery, changeID, st)
			}
		} else {
			for _, e := range entries {
				if e.Scope.Kind != settings.KindLegalEntity {
					continue
				}
				if known, err := r.Settings.KnowsLegalEntity(txCtx, tenant, e.Scope.ID); err != nil {
					return err
				} else if !known {
					return unknownLegalEntity(e.Scope.ID)
				}
			}
			id := "impact-preview"
			for _, taken := ledger.Find(id); taken; _, taken = ledger.Find(id) {
				id += "-"
			}
			if cs, err = ledger.Propose(cat, settings.Proposal{ID: id, Entries: entries, Reason: "impact", By: "impact", At: now}); err != nil {
				return inSeaWords(err)
			}
		}
		with := tentative(cs, now)
		out.TakesEffect = with.Approval.InForce
		var sets, without []settings.ChangeSet
		for _, c := range ledger.Sets() {
			if c.ID != cs.ID {
				without = append(without, c)
			}
		}
		sets = append(slices.Clone(without), with)
		before, after := settings.NewLedger(without), settings.NewLedger(sets)
		entities, err := r.Settings.LegalEntities(txCtx, tenant)
		if err != nil {
			return err
		}
		for _, le := range entities {
			scope := settings.Scope{Kind: settings.KindLegalEntity, ID: le}
			from, _, err := before.ConfigAt(cat, scope, out.TakesEffect)
			if err != nil {
				return err
			}
			to, _, err := after.ConfigAt(cat, scope, out.TakesEffect)
			if err != nil {
				return err
			}
			if from[SettingS1].Value == to[SettingS1].Value {
				continue
			}
			entity := ImpactEntity{LegalEntity: le, From: from[SettingS1].Value, To: to[SettingS1].Value}
			if entity.Conflicts, err = r.openConflicts(txCtx, tenant, le); err != nil {
				return err
			}
			out.Entities = append(out.Entities, entity)
		}
		return nil
	})
	return out, err
}

// openConflicts lists the conflicts with a carrier's loading report that a release would clear on the open movements of
// a legal entity.
func (r Reads) openConflicts(txCtx context.Context, tenant sea.TenantID, legalEntity string) ([]ImpactConflict, error) {
	ids, err := r.Settings.OpenMovements(txCtx, tenant, legalEntity)
	if err != nil {
		return nil, err
	}
	var out []ImpactConflict
	for _, id := range ids {
		read, err := r.movement(txCtx, tenant, id)
		if err != nil {
			return nil, err
		}
		states := make([]sea.VisitState, len(read.Visits))
		for i, v := range read.Visits {
			states[i] = v.Visit.State()
		}
		for _, c := range read.Movement.OpenConflicts(states) {
			out = append(out, ImpactConflict{Movement: id, Visit: c.VisitID, Number: c.Number, FactNo: c.FactNo})
		}
	}
	return out, nil
}
