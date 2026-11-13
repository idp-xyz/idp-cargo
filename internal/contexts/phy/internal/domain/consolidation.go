package domain

import (
	"fmt"
	"time"
)

// Change is what a consolidation version records.
type Change string

const (
	ChangeCreated    Change = "CREATED"
	ChangeAmended    Change = "AMENDED"
	ChangeClosed     Change = "CLOSED"
	ChangeReopened   Change = "REOPENED"
	ChangeCancelled  Change = "CANCELLED"
	ChangeReconciled Change = "RECONCILED"
)

// state returns the state a change leaves, given the state before.
func (c Change) state(before State) (State, bool) {
	switch c {
	case ChangeCreated, ChangeReopened:
		return StateOpen, true
	case ChangeClosed:
		return StateClosed, true
	case ChangeCancelled:
		return StateCancelled, true
	case ChangeReconciled:
		return StateReconciled, true
	case ChangeAmended:
		return before, before == StateOpen || before == StateClosed
	}
	return "", false
}

func (c Change) needsReason() bool {
	return c == ChangeAmended || c == ChangeReopened || c == ChangeCancelled
}

// Version is one immutable version of a consolidation: the route, the planned departure and house cut-off as
// instants, and the state after the change, with the reason, author, business time and recording time (T1).
// Every change adds a version, so the original plan stays visible after a rollover (I-06).
type Version struct {
	No               int
	Change           Change
	State            State
	Route            Route
	PlannedDeparture time.Time
	HouseCutOff      time.Time
	Reason           string
	By               string
	At               time.Time
	EnteredAt        time.Time
}

// ConsolidationState is a stored consolidation, for persistence. Mode, kind and the operating legal entity
// are fixed at creation.
type ConsolidationState struct {
	TenantID               TenantID
	ConsolidationID        ConsolidationID
	Mode                   Mode
	Kind                   Kind
	OperatingLegalEntityID string
	State                  State
	Versions               []Version
}

// Consolidation is the aggregate root of a consolidation. Its identity is its own, never a MAWB, MBL,
// booking or co-load bill number (G04-06). Versions are only added.
type Consolidation struct {
	state    ConsolidationState
	versions int // versions already persisted
	pending  []Event
}

// CreateConsolidation opens a consolidation of a kind that fits its mode, run by an operating legal entity,
// on a resolved route, with a planned departure after the business time and a house cut-off not after it.
func CreateConsolidation(tenant TenantID, id ConsolidationID, mode Mode, kind Kind, operator string, route Route,
	departure, cutOff time.Time, by string, at time.Time) (*Consolidation, error) {
	if !identifier(string(tenant)) || !identifier(string(id)) || !identifier(operator) || blank(by) || at.IsZero() {
		return nil, ErrInvalidConsolidation
	}
	if !mode.Valid() || !kind.fits(mode) {
		return nil, fmt.Errorf("%w: kind %s for mode %s", ErrInvalidConsolidation, kind, mode)
	}
	if !route.valid(mode) {
		return nil, ErrInvalidRoute
	}
	if !departure.After(at) || !schedule(departure, cutOff) {
		return nil, ErrInvalidSchedule
	}
	c := &Consolidation{state: ConsolidationState{TenantID: tenant, ConsolidationID: id, Mode: mode, Kind: kind,
		OperatingLegalEntityID: operator, State: StateOpen}}
	c.add(Version{Change: ChangeCreated, Route: route, PlannedDeparture: departure.UTC(), HouseCutOff: cutOff.UTC(), By: by, At: at.UTC()})
	return c, nil
}

func schedule(departure, cutOff time.Time) bool {
	return !departure.IsZero() && !cutOff.IsZero() && !cutOff.After(departure)
}

// Amend writes a new version of an OPEN or CLOSED consolidation with a reason. A zero route keeps the current
// one; the route changes only while no house is attached. The planned departure and house cut-off may move at
// any time before RECONCILED: a flight or vessel change moves the whole consolidation and its houses.
func (c *Consolidation) Amend(route Route, departure, cutOff time.Time, reason, by string, at time.Time, liveHouses int) error {
	current := c.Current()
	if current.State != StateOpen && current.State != StateClosed {
		return ErrInvalidTransition
	}
	if !text(reason) {
		return fmt.Errorf("%w: an amendment needs a reason", ErrInvalidChange)
	}
	if route.zero() {
		route = current.Route
	}
	if !route.valid(c.state.Mode) {
		return ErrInvalidRoute
	}
	if !schedule(departure, cutOff) {
		return ErrInvalidSchedule
	}
	if !route.sameLocations(current.Route) && liveHouses > 0 {
		return fmt.Errorf("%w: the route changes only while no house is attached", ErrHousesAttached)
	}
	if route.sameLocations(current.Route) && departure.Equal(current.PlannedDeparture) && cutOff.Equal(current.HouseCutOff) {
		return fmt.Errorf("%w: nothing changes", ErrInvalidChange)
	}
	if !route.sameLocations(current.Route) {
		current.Route = route
	}
	if err := c.changeable(by, at); err != nil {
		return err
	}
	c.add(Version{Change: ChangeAmended, Route: current.Route, PlannedDeparture: departure.UTC(), HouseCutOff: cutOff.UTC(),
		Reason: reason, By: by, At: at.UTC()})
	return nil
}

// Close is the house cut-off: it freezes the membership until the consolidation is reopened (OD-5). At least
// one house must be attached, and none later than the business time. Whether FUL still reports every house as
// PHY verified it is checked by the caller with FUL (Entry.Confirm).
func (c *Consolidation) Close(liveHouses int, lastAttached time.Time, by string, at time.Time) error {
	if c.state.State != StateOpen {
		return ErrInvalidTransition
	}
	if liveHouses < 1 {
		return ErrNoHouses
	}
	if at.Before(lastAttached) {
		return fmt.Errorf("%w: a house was attached after the business time of the close", ErrInvalidChange)
	}
	return c.change(ChangeClosed, "", by, at)
}

// Reopen reopens a CLOSED consolidation with a reason (OD-5), until cargo is tendered: tendered counts the live
// packs of its closed load executions, which carrier evidence follows.
func (c *Consolidation) Reopen(reason, by string, at time.Time, tendered int64) error {
	if c.state.State != StateClosed {
		return ErrInvalidTransition
	}
	if tendered > 0 {
		return fmt.Errorf("%w: %d packs tendered", ErrExecutionsClosed, tendered)
	}
	return c.change(ChangeReopened, reason, by, at)
}

// Cancel cancels an OPEN consolidation without houses and without a load execution in progress, with a reason.
func (c *Consolidation) Cancel(reason, by string, at time.Time, liveHouses, openExecutions int) error {
	if c.state.State != StateOpen {
		return ErrInvalidTransition
	}
	if liveHouses > 0 {
		return ErrHousesAttached
	}
	if openExecutions > 0 {
		return fmt.Errorf("%w: %d load executions are not closed", ErrInvalidChange, openExecutions)
	}
	return c.change(ChangeCancelled, reason, by, at)
}

// MemberCheck is what reconciling a live house compares: its live entry, FUL's current LoadIntent v1 and the
// house's live loaded packs in the consolidation.
type MemberCheck struct {
	Entry  Entry
	Intent LoadIntent
	Loaded int64
}

// Reconcile ends a CLOSED consolidation once its physical facts are complete: every live house is PLANNED by FUL
// at the plan version PHY froze, allocated to the consolidation, and loaded exactly as planned (a difference has
// gone to another shipment through FUL and the house was refreshed, F1); no load execution is open; no
// discrepancy of a house's lots or of the executions is open.
func (c *Consolidation) Reconcile(members []MemberCheck, openExecutions, openDiscrepancies int, by string, at time.Time) error {
	if c.state.State != StateClosed {
		return ErrInvalidTransition
	}
	if len(members) == 0 {
		return ErrNoHouses
	}
	for _, m := range members {
		i := m.Intent
		switch {
		case !m.Entry.Live() || m.Entry.ConsolidationID != c.ID() || i.ShipmentID == "":
			return ErrInvalidMembership
		case i.Status != IntentPlanned || i.PlanVersion != m.Entry.PlanVersion:
			return fmt.Errorf("%w: %s; refresh it", ErrMemberStale, i.ShipmentID)
		case i.ConsolidationID != c.ID():
			return fmt.Errorf("%w: %s", ErrMemberNotAllocated, i.ShipmentID)
		case m.Loaded != i.Scope.Packs:
			return fmt.Errorf("%w: %s plans %d packs, %d loaded", ErrNotReconciled, i.ShipmentID, i.Scope.Packs, m.Loaded)
		}
	}
	if openExecutions > 0 || openDiscrepancies > 0 {
		return fmt.Errorf("%w: %d load executions not closed, %d discrepancies open", ErrNotReconciled, openExecutions, openDiscrepancies)
	}
	return c.change(ChangeReconciled, "", by, at)
}

// Admit checks that a house may join the consolidation now: the consolidation is OPEN, and FUL reports the
// shipment PLANNED, of the consolidation's mode, origin and destination (OD-8), its departure window
// containing the planned departure, and of the operating legal entity (OD-10). An LCL consolidation takes no shipment
// FUL plans as FCL (N1-PHY OD-9); one whose container mode is not planned yet is taken, as before.
func (c *Consolidation) Admit(i LoadIntent) error {
	current := c.Current()
	switch {
	case current.State != StateOpen:
		return ErrNotOpen
	case !i.complete():
		return ErrInvalidIntent
	case i.Status != IntentPlanned:
		return fmt.Errorf("%w: %s is %s", ErrIntentNotPlanned, i.ShipmentID, i.Status)
	case i.Mode != c.state.Mode:
		return fmt.Errorf("%w: %s is %s, the consolidation %s", ErrModeMismatch, i.ShipmentID, i.Mode, c.state.Mode)
	case (c.state.Kind == KindSeaLCLOwn || c.state.Kind == KindSeaLCLCoload) && i.ContainerMode == ContainerFCL:
		return fmt.Errorf("%w: %s", ErrFCLHouse, i.ShipmentID)
	case !i.Route.sameLocations(current.Route):
		return fmt.Errorf("%w: %s runs %s-%s, the consolidation %s-%s", ErrRouteMismatch, i.ShipmentID, i.Route.Origin.Code,
			i.Route.Destination.Code, current.Route.Origin.Code, current.Route.Destination.Code)
	case !i.DepartureWindow.Valid() || i.DepartureWindow.Until.IsZero() || !i.DepartureWindow.Contains(current.PlannedDeparture):
		return fmt.Errorf("%w: %s; FUL amends its plan first", ErrWindowMismatch, i.ShipmentID)
	case i.LegalEntityID != c.state.OperatingLegalEntityID:
		return ErrOtherLegalEntity
	}
	return nil
}

func (c *Consolidation) change(change Change, reason, by string, at time.Time) error {
	if change.needsReason() && !text(reason) {
		return fmt.Errorf("%w: %s needs a reason", ErrInvalidChange, change)
	}
	if err := c.changeable(by, at); err != nil {
		return err
	}
	current := c.Current()
	c.add(Version{Change: change, Route: current.Route, PlannedDeparture: current.PlannedDeparture, HouseCutOff: current.HouseCutOff,
		Reason: reason, By: by, At: at.UTC()})
	return nil
}

func (c *Consolidation) changeable(by string, at time.Time) error {
	if blank(by) || at.IsZero() || at.Before(c.Current().At) {
		return ErrInvalidChange
	}
	return nil
}

func (c *Consolidation) add(v Version) {
	v.No = len(c.state.Versions) + 1
	v.State, _ = v.Change.state(c.state.State)
	c.state.Versions = append(c.state.Versions, v)
	c.state.State = v.State
	types := map[Change]string{ChangeCreated: eventConsolidationCreated, ChangeAmended: eventConsolidationAmended,
		ChangeClosed: eventConsolidationClosed, ChangeReopened: eventConsolidationReopened, ChangeCancelled: eventConsolidationCancelled,
		ChangeReconciled: eventConsolidationReconciled}
	c.pending = append(c.pending, Event{Type: types[v.Change], At: v.At, TenantID: c.state.TenantID, SubjectID: string(c.state.ConsolidationID),
		ConsolidationID: c.state.ConsolidationID, Version: v.No})
}

// Stamp records when PHY entered the versions added since the consolidation was loaded (T1).
func (c *Consolidation) Stamp(entered time.Time) {
	for i := c.versions; i < len(c.state.Versions); i++ {
		c.state.Versions[i].EnteredAt = entered.UTC()
	}
}

// RestoreConsolidation rebuilds a stored consolidation without emitting events.
func RestoreConsolidation(st ConsolidationState) (*Consolidation, error) {
	st.Versions = append([]Version(nil), st.Versions...)
	c := &Consolidation{state: st, versions: len(st.Versions)}
	if !identifier(string(st.TenantID)) || !identifier(string(st.ConsolidationID)) || !identifier(st.OperatingLegalEntityID) ||
		!st.Mode.Valid() || !st.Kind.fits(st.Mode) || len(st.Versions) == 0 {
		return nil, ErrInvalidConsolidation
	}
	state := State("")
	for i, v := range st.Versions {
		next, ok := v.Change.state(state)
		if v.No != i+1 || !ok || v.State != next || (v.Change == ChangeCreated) != (i == 0) || !v.Route.valid(st.Mode) ||
			!schedule(v.PlannedDeparture, v.HouseCutOff) || v.Change.needsReason() != text(v.Reason) || blank(v.By) || v.At.IsZero() ||
			(i > 0 && (v.At.Before(st.Versions[i-1].At) || !transition(state, v.Change))) {
			return nil, fmt.Errorf("%w: version %d", ErrInvalidConsolidation, i+1)
		}
		state = next
	}
	if st.State != state {
		return nil, fmt.Errorf("%w: state %s, but the versions make it %s", ErrInvalidConsolidation, st.State, state)
	}
	return c, nil
}

// transition reports whether a change may follow a state.
func transition(from State, change Change) bool {
	switch change {
	case ChangeAmended:
		return from == StateOpen || from == StateClosed
	case ChangeClosed, ChangeCancelled:
		return from == StateOpen
	case ChangeReopened, ChangeReconciled:
		return from == StateClosed
	}
	return false
}

// Current returns the latest version.
func (c *Consolidation) Current() Version { return c.state.Versions[len(c.state.Versions)-1] }

// State returns a copy of the consolidation for persistence and read models.
func (c *Consolidation) State() ConsolidationState {
	st := c.state
	st.Versions = append([]Version(nil), st.Versions...)
	return st
}

func (c *Consolidation) ID() ConsolidationID { return c.state.ConsolidationID }
func (c *Consolidation) Tenant() TenantID    { return c.state.TenantID }
func (c *Consolidation) Mode() Mode          { return c.state.Mode }
func (c *Consolidation) Pending() []Event    { return append([]Event(nil), c.pending...) }

// AddedVersions returns the versions written since the consolidation was created or restored.
func (c *Consolidation) AddedVersions() []Version {
	return append([]Version(nil), c.state.Versions[c.versions:]...)
}

// Acknowledge is permitted only after the transaction holding the consolidation and its Outbox events commits.
func (c *Consolidation) Acknowledge() {
	c.pending = nil
	c.versions = len(c.state.Versions)
}
