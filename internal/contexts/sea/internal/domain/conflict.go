package domain

import (
	"fmt"
	"time"
)

// ConflictRelease is an operator's release of a conflict between SEA's records and a carrier's loading report at a
// reseal (V2.89): the carrier reported a loading of the container (or, with one live container, of the movement) dated
// before PHY sealed it again, while the container's own events show neither a loading nor a stay at the terminal
// then. The operator, having checked, states a reason, which is kept with who and when. It releases that report (its
// number on the movement) against that seal of PHY's (as the houses carry it, and when it was sealed) and nothing
// else: another reseal or another report is a new conflict. Releases are only added, numbered from 1 per visit.
type ConflictRelease struct {
	No          int
	FactNo      int // the carrier fact released
	PHYSeal     string
	PHYSealedAt time.Time
	Reason      string
	By          string
	At          time.Time
	EnteredAt   time.Time
	// SettingsVersion and S1 are the version of SEA's settings and the value of S1 the release was decided under
	// (V2.100); S1 is empty, and the version 0, for a release made without the settings.
	SettingsVersion int
	S1              string
}

func (r ConflictRelease) valid() bool {
	return r.FactNo >= 1 && text(r.PHYSeal) && !r.PHYSealedAt.IsZero() && text(r.Reason) && !blank(r.By) && !r.At.IsZero() &&
		r.SettingsVersion >= 0 && validS1(r.S1)
}

// releases reports whether the release is of this report against this seal of PHY's, by the seal's letters and digits
// and its sealed-at.
func (r ConflictRelease) releases(fact CarrierFact, p Stuffing) bool {
	return r.FactNo == fact.No && sealKey(r.PHYSeal) == sealKey(p.Seal) && r.PHYSealedAt.Equal(p.SealedAt)
}

// carrierLoadingFacts are the carrier's loading reports dated before PHY sealed the container again that count for it:
// those that name its number and, with exactly one live container (only), those that name no container, which can only
// be about it.
func (m *Movement) carrierLoadingFacts(v VisitState, p Stuffing, only bool) []CarrierFact {
	var out []CarrierFact
	for _, c := range m.state.CarrierFacts {
		if c.Kind == CarrierLoaded && (c.ContainerNumber == v.Number || only && c.ContainerNumber == "") && c.OccurredAt.Before(p.SealedAt) {
			out = append(out, c)
		}
	}
	return out
}

// released is the release of the report against p, if the visit has one.
func (v VisitState) released(fact CarrierFact, p Stuffing) (ConflictRelease, bool) {
	for _, r := range v.Releases {
		if r.releases(fact, p) {
			return r, true
		}
	}
	return ConflictRelease{}, false
}

// carrierConflict is the report that raises GATED_IN_BEFORE_RESEAL for the container only because the carrier said it
// was loaded: the container's own events show neither a loading before PHY's sealed-at nor a stay at the terminal then,
// and no release covers the report against this seal. ok is false when there is none.
func (m *Movement) carrierConflict(v VisitState, only bool) (CarrierFact, Stuffing, bool) {
	p, carried, disagree := phySeal(v, m.Houses())
	if v.Status == VisitAssigned || v.Status == VisitReleased || len(v.Seals) == 0 || disagree != "" || !carried || !v.resealed(p, m.sealHistory(v.ExecutionID)) {
		return CarrierFact{}, Stuffing{}, false
	}
	if at, _ := v.atTerminal(p.SealedAt); at || v.loadedBefore(p.SealedAt) != "" {
		return CarrierFact{}, Stuffing{}, false
	}
	for _, c := range m.carrierLoadingFacts(v, p, only) {
		if _, done := v.released(c, p); !done {
			return c, p, true
		}
	}
	return CarrierFact{}, Stuffing{}, false
}

// AcceptCarrierLoadingConflict releases the conflict with a carrier's loading report the container's pending item
// GATED_IN_BEFORE_RESEAL reports (and only such a one: an item raised by the container's own loading or by its gate-in
// with no return is not released this way). The movement is OPEN; live is the number of the movement's live
// containers, which says whether a report that names no container counts for this one (exactly one). A reason is
// required; the release is kept with who and when, and the item then reads as a warning. The published facts do not
// change.
func (v *Visit) AcceptCarrierLoadingConflict(m *Movement, live int, reason, by string, at time.Time) error {
	return v.AcceptCarrierLoadingConflictUnder(m, live, reason, by, at, RuleSettings{}, false)
}

// AcceptCarrierLoadingConflictUnder is AcceptCarrierLoadingConflict under SEA's settings of the movement's legal entity
// (V2.100): the release records the version and the value of S1 and is entered at the time they were read at. Under S1
// SUPERVISOR_ONLY it is refused unless supervisor says the actor holds the permission SuperviseCarrierConflicts; the
// way out is that a supervisor releases it. Under WITH_REASON, and with the zero RuleSettings, nobody is asked for it.
func (v *Visit) AcceptCarrierLoadingConflictUnder(m *Movement, live int, reason, by string, at time.Time, rules RuleSettings, supervisor bool) error {
	st := v.State()
	if st.Status == VisitReleased || st.Status == VisitAssigned {
		return ErrInvalidTransition
	}
	if m == nil || m.ID() != st.MovementID || m.state.TenantID != st.TenantID || m.state.State != StateOpen {
		return fmt.Errorf("%w: a conflict is released while the movement is open", ErrInvalidTransition)
	}
	if !text(reason) {
		return fmt.Errorf("%w: state the reason, at most %d characters", ErrInvalidChange, maxText)
	}
	fact, p, ok := m.carrierConflict(st, live == 1)
	if !ok {
		return fmt.Errorf("%w: the container has no unreleased conflict with a carrier's loading report to release (the report may be released already, or the item may be raised by the container's own events)", ErrInvalidChange)
	}
	if rules.S1 == S1SupervisorOnly && !supervisor {
		return fmt.Errorf("%w: S1 is SUPERVISOR_ONLY for the movement's legal entity, so only a supervisor releases a conflict with a carrier's loading report; "+
			"ask a holder of SuperviseCarrierConflicts (and AcceptCarrierLoadingConflict) to release it", ErrNotSupervisor)
	}
	if err := v.changeable(by, at); err != nil {
		return err
	}
	r := ConflictRelease{No: len(v.state.Releases) + 1, FactNo: fact.No, PHYSeal: p.Seal, PHYSealedAt: p.SealedAt.UTC(), Reason: reason, By: by, At: at.UTC(),
		EnteredAt: rules.At.UTC(), SettingsVersion: rules.Version, S1: rules.S1}
	v.state.Releases = append(v.state.Releases, r)
	v.pending = append(v.pending, Event{Type: eventConflictReleased, At: at.UTC(), TenantID: v.state.TenantID, SubjectID: string(v.state.VisitID),
		ConsolidationID: v.state.ConsolidationID, MovementID: string(v.state.MovementID), ReleaseNo: r.No})
	return nil
}
