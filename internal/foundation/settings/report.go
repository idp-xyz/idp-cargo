package settings

import (
	"slices"
	"strings"
	"time"
)

// The exception report of a tenant's ledger (V2.102): what a reader who audits the controls asks of the settings.

// ExceptionLine is an entry of a scope below the tenant that is looser than the tenant's value, an approved exception,
// with who proposed and who approved the change set it comes from.
type ExceptionLine struct {
	Exception
	ProposedBy, ApprovedBy string
}

// LooserLine is an entry in force that is looser than the standard profile's value (the item's default), at the scope
// it is set at, with who proposed and who approved the change set it comes from, and when it came into force.
type LooserLine struct {
	Scope                  Scope
	Key, Value             string
	Param                  *int64
	Standard               string
	ChangeID               string
	ProposedBy, ApprovedBy string
	InForce                time.Time
}

// SelfApprovedLine is a loosening that its own proposer approved as a single administrator, with the written risk
// acknowledgement and the entries it changed.
type SelfApprovedLine struct {
	ChangeID        string
	By              string
	At              time.Time
	Acknowledgement string
	Entries         []Entry
}

// Changer is an actor who proposed or approved change sets in a period, and how many of each (an approval by the
// proposer of a change set that only tightens counts as an approval too).
type Changer struct {
	Actor              string
	Proposed, Approved int
}

// Report is the exception report of one ledger: the exceptions and the looser-than-standard entries in force at a time,
// the self-approved loosenings approved in the period [From, To), and who changed settings in it.
type Report struct {
	Exceptions   []ExceptionLine
	Looser       []LooserLine
	SelfApproved []SelfApprovedLine
	Changers     []Changer
}

// ExceptionReport answers the ledger's part of the exception report: the exceptions and the entries looser than the
// standard profile as in force at the time at, and, for the period from (included) to to (excluded), the self-approved
// loosenings and the actors who proposed or approved a change set.
func (l *Ledger) ExceptionReport(cat *Catalogue, at, from, to time.Time) Report {
	var r Report
	sets := map[string]ChangeSet{}
	for _, c := range l.sets {
		sets[c.ID] = c
	}
	approvedBy := func(id string) string {
		if a := sets[id].Approval; a != nil {
			return a.By
		}
		return ""
	}
	for _, x := range l.Exceptions(cat, at) {
		r.Exceptions = append(r.Exceptions, ExceptionLine{Exception: x, ProposedBy: sets[x.ChangeID].By, ApprovedBy: approvedBy(x.ChangeID)})
	}
	st := state{}
	for _, c := range liveOf(l.sets) {
		if c.Approval.InForce.After(at) {
			break
		}
		st.apply(c.Entries, c.ID)
	}
	for k, e := range st {
		it, ok := cat.Item(k.key)
		if !ok {
			continue
		}
		rank, _ := it.Rank(e.Value)
		std, _ := it.Rank(it.Default)
		if rank > std {
			c := sets[e.change]
			r.Looser = append(r.Looser, LooserLine{Scope: k.scope, Key: k.key, Value: e.Value, Param: e.Param, Standard: it.Default, ChangeID: e.change,
				ProposedBy: c.By, ApprovedBy: approvedBy(e.change), InForce: c.Approval.InForce})
		}
	}
	slices.SortFunc(r.Looser, func(a, b LooserLine) int {
		if c := strings.Compare(a.Scope.Kind+"/"+a.Scope.ID, b.Scope.Kind+"/"+b.Scope.ID); c != 0 {
			return c
		}
		return strings.Compare(a.Key, b.Key)
	})
	in := func(t time.Time) bool { return !t.Before(from) && t.Before(to) }
	counts := map[string]*Changer{}
	count := func(actor string) *Changer {
		if counts[actor] == nil {
			counts[actor] = &Changer{Actor: actor}
		}
		return counts[actor]
	}
	for _, c := range l.sets {
		if in(c.At) {
			count(c.By).Proposed++
		}
		a := c.Approval
		if a == nil || !in(a.At) {
			continue
		}
		count(a.By).Approved++
		if a.SelfApproved() {
			r.SelfApproved = append(r.SelfApproved, SelfApprovedLine{ChangeID: c.ID, By: a.By, At: a.At, Acknowledgement: a.Acknowledgement, Entries: slices.Clone(c.Entries)})
		}
	}
	for _, c := range counts {
		r.Changers = append(r.Changers, *c)
	}
	slices.SortFunc(r.Changers, func(a, b Changer) int { return strings.Compare(a.Actor, b.Actor) })
	slices.SortFunc(r.SelfApproved, func(a, b SelfApprovedLine) int { return strings.Compare(a.ChangeID, b.ChangeID) })
	return r
}

// Declaration is a tenant's declaration of a single administrator: the onboarding that made it, the consultant who
// proposed that onboarding, the customer's owner who confirmed it, the administrator the owner named (not the
// consultant), and when the context recorded it.
type Declaration struct {
	OnboardingID  string
	ProposedBy    string
	ConfirmedBy   string
	Administrator string
	At            time.Time
}
