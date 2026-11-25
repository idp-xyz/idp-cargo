package settings

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrRefused is every refusal of the ledger; each also wraps one of the kinds below, and each message names the item or
// the change set and the way out.
var ErrRefused = errors.New("settings: refused")

type kind string

func (k kind) Error() string { return string(k) }

// Kinds of refusal.
const (
	ErrInvalid         kind = "SETTINGS_INVALID"
	ErrDuplicate       kind = "SETTINGS_DUPLICATE"
	ErrUnknownItem     kind = "SETTINGS_UNKNOWN_ITEM"
	ErrUnknownValue    kind = "SETTINGS_UNKNOWN_VALUE"
	ErrScope           kind = "SETTINGS_SCOPE"
	ErrParameter       kind = "SETTINGS_PARAMETER"
	ErrRule            kind = "SETTINGS_RULE"
	ErrEffective       kind = "SETTINGS_EFFECTIVE_TIME"
	ErrUnknownChange   kind = "SETTINGS_UNKNOWN_CHANGE"
	ErrSameActor       kind = "SETTINGS_SAME_ACTOR"
	ErrAlreadyApproved kind = "SETTINGS_ALREADY_APPROVED"
	ErrCancelled       kind = "SETTINGS_CANCELLED"
	ErrInForce         kind = "SETTINGS_IN_FORCE"
	ErrUnapproved      kind = "SETTINGS_WOULD_LOOSEN_UNAPPROVED"
	ErrOrder           kind = "SETTINGS_ORDER"
	ErrClock           kind = "SETTINGS_CLOCK"
	ErrClockSkew       kind = "SETTINGS_CLOCK_SKEW"
	ErrRefusedChange   kind = "SETTINGS_REFUSED"
)

// Refuse is a refusal of the given kind; a context's cross-item Rule answers one of kind ErrRule.
func Refuse(k error, format string, args ...any) error {
	return fmt.Errorf("%w: %w: %s", ErrRefused, k, fmt.Sprintf(format, args...))
}

// Entry is one value proposed for one item in one scope; Param is the parameter of the value, in minor units, when it
// has one.
type Entry struct {
	Scope Scope
	Key   string
	Value string
	Param *int64
}

// Approval makes a change set count. A change set that does not loosen anything is approved by its proposer when it is
// proposed (Auto); a loosening one by another person. Version numbers the approvals of the tenant from 1 without a
// gap. InForce is when the change set takes effect: its effective time, or the time it was approved when that is
// later, so that no approval makes a change retroactive.
type Approval struct {
	By      string
	At      time.Time
	Version int
	InForce time.Time
	Auto    bool
}

// Cancellation withdraws a change set before it is in force.
type Cancellation struct {
	By     string
	At     time.Time
	Reason string
	// Refused is a cancellation by an approver who will not approve the change set: it was never approved.
	Refused bool
	// Role is the permission the canceller acted under: RoleConfigurer or RoleApprover (a refusal is always the approver's).
	Role string
}

// The roles a change set is cancelled in: by a holder of the permission to propose and cancel, or by a holder of the
// permission to approve, who may cancel too so that one proposer cannot hold up every later change.
const (
	RoleConfigurer = "CONFIGURER"
	RoleApprover   = "APPROVER"
)

// WhoMayCancel says who may cancel a change set, for the refusals whose way out is to cancel one.
const WhoMayCancel = "CancelFinSettingsChange, which a holder of ConfigureSettings or of ApproveSettings may do while it is not in force"

// ChangeSet is one or more entries proposed together by an actor, with a reason, at the system's clock, to take effect
// at Effective (not before At). Loosening is fixed when it is proposed.
type ChangeSet struct {
	ID           string
	Entries      []Entry
	Reason       string
	By           string
	At           time.Time
	Effective    time.Time
	Loosening    bool
	Approval     *Approval
	Cancellation *Cancellation
}

// Status of a change set at a time.
type Status string

const (
	StatusProposed  Status = "PROPOSED"  // waiting for a second person
	StatusApproved  Status = "APPROVED"  // approved, its time not yet come
	StatusInForce   Status = "IN_FORCE"  // its time has come
	StatusCancelled Status = "CANCELLED" // withdrawn before it was in force
	StatusRefused   Status = "REFUSED"   // an approver refused it; it was never approved
)

// Status answers the status at now.
func (c ChangeSet) Status(now time.Time) Status {
	switch {
	case c.Cancellation != nil && c.Cancellation.Refused:
		return StatusRefused
	case c.Cancellation != nil:
		return StatusCancelled
	case c.Approval == nil:
		return StatusProposed
	case c.Approval.InForce.After(now):
		return StatusApproved
	}
	return StatusInForce
}

// Limits of a proposal.
const (
	MaxEntries = 64
	MaxReason  = 500
	// MaxAhead is how far after the proposal a change set may take effect: a change set far ahead sits in every
	// check of the ones after it.
	MaxAhead = 366 * 24 * time.Hour
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

// Ledger is the change sets of one tenant in one context, in the order proposed.
type Ledger struct{ sets []ChangeSet }

// NewLedger restores a ledger from stored change sets.
func NewLedger(sets []ChangeSet) *Ledger { return &Ledger{sets: slices.Clone(sets)} }

// Sets answers the change sets, in the order proposed.
func (l *Ledger) Sets() []ChangeSet { return slices.Clone(l.sets) }

// Find answers the change set with the ID.
func (l *Ledger) Find(id string) (ChangeSet, bool) {
	i := slices.IndexFunc(l.sets, func(c ChangeSet) bool { return c.ID == id })
	if i < 0 {
		return ChangeSet{}, false
	}
	return l.sets[i], true
}

// latest is the latest time recorded in the ledger: a proposal, an approval or a cancellation.
func (l *Ledger) latest() time.Time {
	var t time.Time
	for _, c := range l.sets {
		t = laterOf(t, c.At)
		if c.Approval != nil {
			t = laterOf(t, c.Approval.At)
		}
		if c.Cancellation != nil {
			t = laterOf(t, c.Cancellation.At)
		}
	}
	return t
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// clockBehind refuses a time before the latest recorded: history is written forwards, so that what the ledger answered
// for a moment in the past does not change.
func (l *Ledger) clockBehind(at time.Time) error {
	if latest := l.latest(); at.Before(latest) {
		return Refuse(ErrClock, "the time %s is before the latest recorded for the tenant's settings (%s), because the command waited behind another or "+
			"FIN's clock stepped back; try the command again", at.Format(time.RFC3339Nano), latest.Format(time.RFC3339Nano))
	}
	return nil
}

// inOrder refuses an approval, with its version, that takes effect before an approved change set with a lower version
// that is still to come, so that the version in force never goes back in time. The way out names both.
func inOrder(sets []ChangeSet, id string, a Approval) error {
	for _, c := range liveOf(sets) {
		if c.ID != id && c.Approval.Version < a.Version && c.Approval.InForce.After(a.InForce) {
			return Refuse(ErrOrder, "change set %s would take effect at %s, before change set %s (version %d) which takes effect at %s; "+
				"change sets take effect in the order of their versions: choose a time at or after %s, or cancel change set %s first ("+WhoMayCancel+")", id,
				a.InForce.Format(time.RFC3339), c.ID, c.Approval.Version, c.Approval.InForce.Format(time.RFC3339), c.Approval.InForce.Format(time.RFC3339), c.ID)
		}
	}
	return nil
}

func (l *Ledger) nextVersion() int {
	n := 0
	for _, c := range l.sets {
		if c.Approval != nil {
			n = max(n, c.Approval.Version)
		}
	}
	return n + 1
}

// liveOf are the approved change sets not cancelled, in the order they take effect: by the time in force, then by
// version.
func liveOf(sets []ChangeSet) []ChangeSet {
	var out []ChangeSet
	for _, c := range sets {
		if c.Approval != nil && c.Cancellation == nil {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b ChangeSet) int {
		if c := a.Approval.InForce.Compare(b.Approval.InForce); c != 0 {
			return c
		}
		return a.Approval.Version - b.Approval.Version
	})
	return out
}

type slot struct {
	scope Scope
	key   string
}

// held is an entry in force and the change set it comes from.
type held struct {
	Entry
	change string
}

// state is the entries in force: the latest of each scope and key.
type state map[slot]held

func (s state) apply(entries []Entry, change string) {
	for _, e := range entries {
		s[slot{e.Scope, e.Key}] = held{e, change}
	}
}

func (s state) clone() state {
	out := make(state, len(s))
	for k, v := range s {
		out[k] = v
	}
	return out
}

// setting resolves an item through the chain: the first scope that has an entry, else the item's default.
func (s state) setting(it Item, chain Chain) Setting {
	for _, sc := range chain {
		if e, ok := s[slot{sc, it.Key}]; ok {
			return Setting{Value: e.Value, Param: e.Param, From: sc, Set: true, ChangeID: e.change}
		}
	}
	return Setting{Value: it.Default, From: TenantScope}
}

// config resolves every item through the chain and marks each value of a scope below the tenant that is looser than the
// tenant's as an exception.
func (s state) config(cat *Catalogue, chain Chain) Config {
	out := make(Config, len(cat.Items))
	for _, it := range cat.Items {
		v := s.setting(it, chain)
		if len(chain) > 1 && v.Set && v.From != chain[len(chain)-1] {
			floor := s.setting(it, chain[len(chain)-1:])
			rv, _ := it.Rank(v.Value)
			rf, _ := it.Rank(floor.Value)
			v.Exception = rv > rf
		}
		out[it.Key] = v
	}
	return out
}

// ConfigAt answers what is in force for the scope at a time: each item resolved through the scope's chain (its own
// value, else the tenant's, else the default) and the version in force, the number of the approval of the latest change
// set in force at or before the time, 0 when none is.
func (l *Ledger) ConfigAt(cat *Catalogue, scope Scope, at time.Time) (Config, int, error) {
	chain, err := cat.Hierarchy(scope)
	if err != nil {
		return nil, 0, Refuse(ErrScope, "%v; choose a scope of the context", err)
	}
	st, version := state{}, 0
	for _, c := range liveOf(l.sets) {
		if c.Approval.InForce.After(at) {
			break
		}
		st.apply(c.Entries, c.ID)
		version = c.Approval.Version
	}
	return st.config(cat, chain), version, nil
}

// Exception is an entry of a scope below the tenant that is looser than the tenant's value: an approved exception,
// which stays in force when the tenant tightens.
type Exception struct {
	Scope       Scope
	Key         string
	Value       string
	TenantValue string
	ChangeID    string
}

// Exceptions lists, as of a time, each entry of a scope below the tenant that is looser than the tenant's value for its
// item, in the order of scope and key.
func (l *Ledger) Exceptions(cat *Catalogue, at time.Time) []Exception {
	st := state{}
	for _, c := range liveOf(l.sets) {
		if c.Approval.InForce.After(at) {
			break
		}
		st.apply(c.Entries, c.ID)
	}
	var out []Exception
	for k, e := range st {
		if k.scope == TenantScope {
			continue
		}
		it, ok := cat.Item(k.key)
		if !ok {
			continue
		}
		rv, _ := it.Rank(e.Value)
		tenant := st.setting(it, Chain{TenantScope})
		if rt, _ := it.Rank(tenant.Value); rv > rt {
			out = append(out, Exception{Scope: k.scope, Key: k.key, Value: e.Value, TenantValue: tenant.Value, ChangeID: e.change})
		}
	}
	slices.SortFunc(out, func(a, b Exception) int {
		if c := strings.Compare(a.Scope.Kind+"/"+a.Scope.ID, b.Scope.Kind+"/"+b.Scope.ID); c != 0 {
			return c
		}
		return strings.Compare(a.Key, b.Key)
	})
	return out
}

// looser reports whether a value, with its parameter, is looser than the setting in force: further along the item's
// values, or the same value with a parameter that lets more through.
func looser(it Item, value string, param *int64, cur Setting) bool {
	nr, _ := it.Rank(value)
	cr, _ := it.Rank(cur.Value)
	if nr != cr {
		return nr > cr
	}
	p := it.Values[nr].Param
	return p != nil && p.LooserWhenHigher && param != nil && cur.Param != nil && *param > *cur.Param
}

// loosening answers what loosens in a set of entries against the state before it, or "" when nothing does. An entry
// loosens when it moves its item looser than the value in force at its scope, or sets a value looser than the
// tenant's, which is the floor, taking the tenant's entries of the same set into account.
func loosening(cat *Catalogue, before state, entries []Entry) (string, error) {
	after := before.clone()
	for _, e := range entries {
		if e.Scope == TenantScope {
			after.apply([]Entry{e}, "")
		}
	}
	for _, e := range entries {
		it, _ := cat.Item(e.Key)
		chain, err := cat.Hierarchy(e.Scope)
		if err != nil {
			return "", Refuse(ErrScope, "%s: %v; choose a scope of the context", e.Key, err)
		}
		if cur := before.setting(it, chain); looser(it, e.Value, e.Param, cur) {
			return fmt.Sprintf("%s at %s moves from %s to %s", e.Key, scopeName(e.Scope), cur.Value, e.Value), nil
		}
		if len(chain) > 1 {
			if floor := after.setting(it, chain[len(chain)-1:]); looser(it, e.Value, e.Param, floor) {
				return fmt.Sprintf("%s at %s is looser than the tenant's %s", e.Key, scopeName(e.Scope), floor.Value), nil
			}
		}
	}
	return "", nil
}

// kindName is a kind of scope in words: "legal entity".
func kindName(kinds ...string) string {
	return strings.ToLower(strings.ReplaceAll(strings.Join(kinds, " or "), "_", " "))
}

func scopeName(s Scope) string {
	if s.ID == "" {
		return kindName(s.Kind)
	}
	return kindName(s.Kind) + " " + s.ID
}

// checkEntry checks one entry on its own: the item and value are known, the scope is one the item may be set at, and
// the parameter is there when the value asks for one and only then.
func checkEntry(cat *Catalogue, e Entry) error {
	it, ok := cat.Item(e.Key)
	if !ok {
		return Refuse(ErrUnknownItem, "%s is not an item of the %s catalogue; read the catalogue for the items", e.Key, cat.Context)
	}
	rank, ok := it.Rank(e.Value)
	if !ok {
		var ids []string
		for _, v := range it.Values {
			ids = append(ids, v.ID)
		}
		return Refuse(ErrUnknownValue, "%s has no value %q; choose one of %s", e.Key, e.Value, strings.Join(ids, ", "))
	}
	if _, err := cat.Hierarchy(e.Scope); err != nil {
		return Refuse(ErrScope, "%s: %v; choose a scope of the context", e.Key, err)
	}
	if !slices.Contains(it.Kinds, e.Scope.Kind) {
		return Refuse(ErrScope, "%s is set at %s only, not at %s; set it there", e.Key, kindName(it.Kinds...), kindName(e.Scope.Kind))
	}
	p := it.Values[rank].Param
	switch {
	case p == nil && e.Param != nil:
		return Refuse(ErrParameter, "%s %s takes no parameter; leave it out", e.Key, e.Value)
	case p != nil && e.Param == nil:
		return Refuse(ErrParameter, "%s %s needs its %s, an amount in the functional currency in minor units; give it, or choose another value",
			e.Key, e.Value, p.Name)
	case p != nil && !slices.Contains(p.Kinds, e.Scope.Kind):
		where := kindName(p.Kinds...)
		return Refuse(ErrParameter, "%s %s needs its %s in the functional currency of a %s; set it for each %s, or choose another value",
			e.Key, e.Value, p.Name, where, where)
	case p != nil && (*e.Param < 1 || *e.Param > MaxParam):
		return Refuse(ErrParameter, "%s %s: the %s is 1 to %d minor units", e.Key, e.Value, p.Name, MaxParam)
	}
	return nil
}

// validateConfig checks a whole configuration: each value known and each parameter present exactly where its value asks
// for one, then the context's rules across items.
func validateConfig(cat *Catalogue, cfg Config) error {
	for _, it := range cat.Items {
		s := cfg[it.Key]
		rank, ok := it.Rank(s.Value)
		switch {
		case !ok:
			return Refuse(ErrUnknownValue, "%s has the value %q, which is not one of its values", it.Key, s.Value)
		case (it.Values[rank].Param != nil) != (s.Param != nil):
			return Refuse(ErrParameter, "%s %s and its parameter do not go together; set them in one entry", it.Key, s.Value)
		}
	}
	for _, rule := range cat.Rules {
		if err := rule(cfg); err != nil {
			return err
		}
	}
	return nil
}

// check runs over the change sets in force as they would be and answers the first refusal for what takes effect at or
// after from: a change set approved without a second person that would now loosen something (a change set approved or
// cancelled after it was proposed moved what it is measured against), and a configuration that breaks the item or
// cross-item rules, for each scope a change set touches (for a tenant entry, each scope with entries of its own).
func check(cat *Catalogue, sets []ChangeSet, from time.Time) error {
	st := state{}
	for _, c := range liveOf(sets) {
		late := !c.Approval.InForce.Before(from)
		if late && c.Approval.Auto {
			if why, err := loosening(cat, st, c.Entries); err != nil {
				return err
			} else if why != "" {
				return Refuse(ErrUnapproved, "change set %s was approved by its proposer alone because it only tightens, and with this change %s; "+
					"to go on, cancel change set %s (it is not in force yet; "+WhoMayCancel+") and propose it again for a second person to approve", c.ID, why, c.ID)
			}
		}
		st.apply(c.Entries, c.ID)
		if !late {
			continue
		}
		touched := map[Scope]bool{}
		for _, e := range c.Entries {
			touched[e.Scope] = true
			if e.Scope == TenantScope {
				for k := range st {
					touched[k.scope] = true
				}
			}
		}
		for sc := range touched {
			chain, err := cat.Hierarchy(sc)
			if err == nil {
				err = validateConfig(cat, st.config(cat, chain))
			}
			if err != nil {
				return fmt.Errorf("the configuration of %s from %s (change set %s) would be wrong: %w", scopeName(sc),
					c.Approval.InForce.Format(time.RFC3339), c.ID, err)
			}
		}
	}
	return nil
}

// Proposal is a change set as asked: the actor, the system's time of the proposal and the effective time (zero for at
// once).
type Proposal struct {
	ID        string
	Entries   []Entry
	Reason    string
	By        string
	At        time.Time
	Effective time.Time
}

// Propose checks the proposal and answers the change set: each entry on its own, the configuration it would make, and
// whether it loosens, in which case it waits for approval and otherwise is approved by its proposer on the spot.
func (l *Ledger) Propose(cat *Catalogue, p Proposal) (ChangeSet, error) {
	switch {
	case !idPattern.MatchString(p.ID):
		return ChangeSet{}, Refuse(ErrInvalid, "a change set ID is 1 to 128 letters, digits and . _ : / -, starting with a letter or digit; choose one")
	case strings.TrimSpace(p.By) == "" || len(p.By) > 128 || p.At.IsZero():
		return ChangeSet{}, Refuse(ErrInvalid, "a change set names its actor (1 to 128 bytes) and the time it was proposed; this is FIN's own call, so ask the platform's owners if you see it")
	case utf8.RuneCountInString(strings.TrimSpace(p.Reason)) < 1 || utf8.RuneCountInString(p.Reason) > MaxReason:
		return ChangeSet{}, Refuse(ErrInvalid, "a change set has a reason of 1 to %d characters; give it", MaxReason)
	case len(p.Entries) < 1 || len(p.Entries) > MaxEntries:
		return ChangeSet{}, Refuse(ErrInvalid, "a change set has 1 to %d entries; propose the rest in another change set", MaxEntries)
	}
	if _, ok := l.Find(p.ID); ok {
		return ChangeSet{}, fmt.Errorf("%w: %w: change set %s exists; propose with another ID", ErrRefused, ErrDuplicate, p.ID)
	}
	effective := p.Effective
	if effective.IsZero() {
		effective = p.At
	}
	if effective.Before(p.At) {
		return ChangeSet{}, Refuse(ErrEffective, "the effective time %s is before now (%s); a change takes effect now or later, and a "+
			"correction is a new change set", effective.Format(time.RFC3339), p.At.Format(time.RFC3339))
	}
	if effective.After(p.At.Add(MaxAhead)) {
		return ChangeSet{}, Refuse(ErrEffective, "the effective time %s is more than %d days after now (%s); choose a nearer time, and propose again "+
			"then for a later change", effective.Format(time.RFC3339), int(MaxAhead.Hours()/24), p.At.Format(time.RFC3339))
	}
	if err := l.clockBehind(p.At); err != nil {
		return ChangeSet{}, err
	}
	seen := map[slot]bool{}
	entries := make([]Entry, len(p.Entries))
	for i, e := range p.Entries {
		if err := checkEntry(cat, e); err != nil {
			return ChangeSet{}, err
		}
		if seen[slot{e.Scope, e.Key}] {
			return ChangeSet{}, Refuse(ErrInvalid, "%s is proposed twice for %s; propose one value", e.Key, scopeName(e.Scope))
		}
		seen[slot{e.Scope, e.Key}] = true
		entries[i] = e
		if e.Param != nil {
			v := *e.Param
			entries[i].Param = &v
		}
	}
	before := state{}
	for _, c := range liveOf(l.sets) {
		if c.Approval.InForce.After(effective) {
			break
		}
		before.apply(c.Entries, c.ID)
	}
	why, err := loosening(cat, before, entries)
	if err != nil {
		return ChangeSet{}, err
	}
	cs := ChangeSet{ID: p.ID, Entries: entries, Reason: strings.TrimSpace(p.Reason), By: p.By, At: p.At, Effective: effective, Loosening: why != ""}
	tentative := cs
	tentative.Approval = &Approval{By: p.By, At: p.At, Version: l.nextVersion(), InForce: effective, Auto: !cs.Loosening}
	if !cs.Loosening {
		if err = inOrder(l.sets, cs.ID, *tentative.Approval); err != nil {
			return ChangeSet{}, err
		}
	}
	if err = check(cat, append(slices.Clone(l.sets), tentative), effective); err != nil {
		return ChangeSet{}, err
	}
	if !cs.Loosening {
		cs.Approval = tentative.Approval
	}
	return cs, nil
}

// Approve answers the approval of a change set waiting for a second person: by someone other than its proposer, at the
// system's time, with the next version number. The configuration is checked again, since the change sets in force may
// have moved since the proposal.
func (l *Ledger) Approve(cat *Catalogue, id, by string, at time.Time) (Approval, error) {
	cs, ok := l.Find(id)
	switch {
	case !ok:
		return Approval{}, Refuse(ErrUnknownChange, "there is no change set %s; list the tenant's change sets (ListFinSettingsChanges) for the IDs", id)
	case cs.Cancellation != nil:
		return Approval{}, Refuse(ErrCancelled, "change set %s was cancelled or refused; propose it again", id)
	case cs.Approval != nil:
		return Approval{}, Refuse(ErrAlreadyApproved, "change set %s is approved already (version %d); there is nothing more to do", id, cs.Approval.Version)
	case strings.TrimSpace(by) == "" || len(by) > 128 || at.IsZero() || at.Before(cs.At):
		return Approval{}, Refuse(ErrInvalid, "an approval names its actor (1 to 128 bytes) and a time not before the proposal; try the command again")
	case by == cs.By:
		return Approval{}, Refuse(ErrSameActor, "change set %s was proposed by %s, who may not approve it; have another approver approve it", id, by)
	}
	if err := l.clockBehind(at); err != nil {
		return Approval{}, err
	}
	a := Approval{By: by, At: at, Version: l.nextVersion(), InForce: cs.Effective}
	if at.After(a.InForce) {
		a.InForce = at
	}
	sets := l.Sets()
	for i := range sets {
		if sets[i].ID == id {
			sets[i].Approval = &a
		}
	}
	if err := inOrder(l.sets, id, a); err != nil {
		return Approval{}, err
	}
	if err := check(cat, sets, at); err != nil {
		return Approval{}, err
	}
	return a, nil
}

// Cancel answers the cancellation of a change set not yet in force, proposed or approved with a time to come, with a
// reason. One in force cannot be cancelled: a rollback is a new change set.
func (l *Ledger) Cancel(cat *Catalogue, id, by, reason string, at time.Time) (Cancellation, error) {
	cs, ok := l.Find(id)
	switch {
	case !ok:
		return Cancellation{}, Refuse(ErrUnknownChange, "there is no change set %s; list the tenant's change sets (ListFinSettingsChanges) for the IDs", id)
	case cs.Cancellation != nil:
		return Cancellation{}, Refuse(ErrCancelled, "change set %s was cancelled or refused already; there is nothing more to do", id)
	case cs.Status(at) == StatusInForce:
		return Cancellation{}, Refuse(ErrInForce, "change set %s is in force; propose a new change set that restores the earlier values", id)
	case strings.TrimSpace(by) == "" || len(by) > 128 || at.IsZero() || at.Before(cs.At):
		return Cancellation{}, Refuse(ErrInvalid, "a cancellation names its actor (1 to 128 bytes) and a time not before the proposal; try the command again")
	case utf8.RuneCountInString(strings.TrimSpace(reason)) < 1 || utf8.RuneCountInString(reason) > MaxReason:
		return Cancellation{}, Refuse(ErrInvalid, "a cancellation has a reason of 1 to %d characters; give it", MaxReason)
	}
	if err := l.clockBehind(at); err != nil {
		return Cancellation{}, err
	}
	c := Cancellation{By: by, At: at, Reason: strings.TrimSpace(reason), Role: RoleConfigurer}
	sets := l.Sets()
	for i := range sets {
		if sets[i].ID == id {
			sets[i].Cancellation = &c
		}
	}
	if err := check(cat, sets, at); err != nil {
		return Cancellation{}, err
	}
	return c, nil
}

// Refuse answers the refusal of a change set that waits for approval, by an approver who will not approve it, with a
// reason: a cancellation of kind refused. A change set approved already is cancelled instead, while it is not in force.
func (l *Ledger) Refuse(id, by, reason string, at time.Time) (Cancellation, error) {
	cs, ok := l.Find(id)
	switch {
	case !ok:
		return Cancellation{}, Refuse(ErrUnknownChange, "there is no change set %s; list the tenant's change sets (ListFinSettingsChanges) for the IDs", id)
	case cs.Cancellation != nil:
		return Cancellation{}, Refuse(ErrCancelled, "change set %s was cancelled or refused already; there is nothing more to do", id)
	case cs.Approval != nil:
		return Cancellation{}, Refuse(ErrRefusedChange, "change set %s is approved already (version %d); cancel it with a reason ("+WhoMayCancel+")"+
			"; once it is in force, propose a new change set that restores the earlier values", id, cs.Approval.Version)
	case strings.TrimSpace(by) == "" || len(by) > 128 || at.IsZero() || at.Before(cs.At):
		return Cancellation{}, Refuse(ErrInvalid, "a refusal names its actor (1 to 128 bytes) and a time not before the proposal; try the command again")
	case utf8.RuneCountInString(strings.TrimSpace(reason)) < 1 || utf8.RuneCountInString(reason) > MaxReason:
		return Cancellation{}, Refuse(ErrInvalid, "a refusal has a reason of 1 to %d characters; give it", MaxReason)
	}
	if err := l.clockBehind(at); err != nil {
		return Cancellation{}, err
	}
	return Cancellation{By: by, At: at, Reason: strings.TrimSpace(reason), Refused: true, Role: RoleApprover}, nil
}
