package domain

import (
	"errors"
	"time"
)

// SEA V2.100: the rules that SEA's settings steer. A rule reads the settings of the movement's legal entity as a
// RuleSettings, which the application reads in the rule's own transaction; the domain never reads a store.

// The values of S1, "a carrier report that contradicts SEA's records", from strict to loose. The empty value is the
// standard profile's: WITH_REASON, which is how a release was made before V2.100.
const (
	S1SupervisorOnly = "SUPERVISOR_ONLY"
	S1WithReason     = "WITH_REASON"
)

// ErrNotSupervisor is the refusal of a release of a conflict with a carrier's loading report under S1 SUPERVISOR_ONLY
// by someone who is not a supervisor.
var ErrNotSupervisor = errors.New("sea: the release needs a supervisor")

// RuleSettings is the settings a rule reads for one legal entity: the version of SEA's settings in force (0 before any
// change set), the value of S1 and the system time they were read at. The decision's records are stamped with that
// time, so that the version recorded is the version the ledger says was in force at the time recorded. The zero value
// is the standard profile at no time: a rule that is handed it records no time and no value of S1.
type RuleSettings struct {
	Version int
	S1      string
	At      time.Time
}

// validS1 reports whether v is a value of S1, or empty.
func validS1(v string) bool { return v == "" || v == S1SupervisorOnly || v == S1WithReason }

// ReadUnder says which value of S1 the movement's pending items are read under, so that the item of a conflict with a
// carrier's loading report says who may release it. With the empty value (the default: Finalize reads the items without
// the settings) the item says both.
func (m *Movement) ReadUnder(s1 string) { m.s1 = s1 }

// OpenConflict is a conflict between SEA's records and a carrier's loading report that a live container has and that no
// release covers: the container, and the report's number on the movement.
type OpenConflict struct {
	VisitID VisitID
	Number  string
	FactNo  int
}

// OpenConflicts lists the conflicts of the movement's live containers that a release would clear, as the pending items
// read them (a container has one at a time: its first report not released), in the order of their load units. The
// caller lists the visits of an open movement: a release is accepted while the movement is open.
func (m *Movement) OpenConflicts(visits []VisitState) []OpenConflict {
	live := liveVisits(visits)
	var out []OpenConflict
	for _, v := range live {
		if fact, _, ok := m.carrierConflict(v, len(live) == 1); ok {
			out = append(out, OpenConflict{VisitID: v.VisitID, Number: v.Number, FactNo: fact.No})
		}
	}
	return out
}

// whoReleases says, for the item of a conflict, who may release it under the value of S1 it is read under.
func whoReleases(s1 string) string {
	switch s1 {
	case S1SupervisorOnly:
		return "S1 is SUPERVISOR_ONLY for this legal entity: only a supervisor, who holds SuperviseCarrierConflicts as well as AcceptCarrierLoadingConflict, may release it, so ask a supervisor"
	case S1WithReason:
		return "S1 is WITH_REASON for this legal entity: whoever holds AcceptCarrierLoadingConflict may release it"
	}
	return "who may release it depends on S1 of the legal entity: under WITH_REASON whoever holds AcceptCarrierLoadingConflict, under SUPERVISOR_ONLY only a supervisor, who also holds SuperviseCarrierConflicts"
}
