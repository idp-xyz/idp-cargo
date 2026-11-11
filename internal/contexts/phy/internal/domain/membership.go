package domain

import (
	"fmt"
	"time"
)

// StartKind says how a membership entry started.
type StartKind string

const (
	StartAttached    StartKind = "ATTACHED"
	StartTransferred StartKind = "TRANSFERRED" // from another consolidation
	StartRefreshed   StartKind = "REFRESHED"   // a changed FUL plan, in the same consolidation
)

// EndKind says how a membership entry ended.
type EndKind string

const (
	EndDetached          EndKind = "DETACHED"
	EndTransferred       EndKind = "TRANSFERRED"
	EndRefreshed         EndKind = "REFRESHED"
	EndShipmentCancelled EndKind = "SHIPMENT_CANCELLED"
)

// Reason is why a house was detached or transferred (MembershipReason).
type Reason string

const (
	ReasonRollover             Reason = "ROLLOVER"
	ReasonFlightOrVesselChange Reason = "FLIGHT_OR_VESSEL_CHANGE"
	ReasonCarrierChange        Reason = "CARRIER_CHANGE"
	ReasonShutOut              Reason = "SHUT_OUT"
	ReasonSpaceShortage        Reason = "SPACE_SHORTAGE"
	ReasonCustomerRequest      Reason = "CUSTOMER_REQUEST"
	ReasonWrongConsol          Reason = "WRONG_CONSOL"
	ReasonShipmentCancelled    Reason = "SHIPMENT_CANCELLED"
	ReasonOther                Reason = "OTHER"
)

// check validates a reason an operator gives; SHIPMENT_CANCELLED is PHY's own, and OTHER needs a text.
func (r Reason) check(text string) error {
	switch r {
	case ReasonRollover, ReasonFlightOrVesselChange, ReasonCarrierChange, ReasonShutOut, ReasonSpaceShortage, ReasonCustomerRequest,
		ReasonWrongConsol, ReasonOther:
	default:
		return fmt.Errorf("%w: %q", ErrInvalidReason, r)
	}
	if (r == ReasonOther && blank(text)) || !optionalText(text) {
		return fmt.Errorf("%w: OTHER needs a text of at most %d characters", ErrInvalidReason, maxText)
	}
	return nil
}

func optionalText(s string) bool { return s == "" || text(s) }

// Entry is one entry of a house's membership ledger: the consolidation, the FUL revision and plan version
// PHY verified and the scope and parts it froze from them, who attached it and when. Only its end is written
// later, once. A transfer or refresh ends one entry and starts another linked to it, so the original
// consolidation stays visible (I-06).
type Entry struct {
	No              int
	ConsolidationID ConsolidationID
	Start           StartKind
	Predecessor     int // the entry a transfer or refresh continues; 0 for ATTACHED
	FULRevision     int64
	PlanVersion     int
	Scope           CargoScope
	Parts           []Part
	By              string
	At              time.Time
	EnteredAt       time.Time
	End             EndKind // empty while live
	Reason          Reason  // DETACHED, TRANSFERRED and SHIPMENT_CANCELLED
	ReasonText      string
	EndedBy         string
	EndedAt         time.Time
	EndEnteredAt    time.Time
	Successor       int // TRANSFERRED and REFRESHED
}

// Live reports whether the entry has not ended.
func (e Entry) Live() bool { return e.End == "" }

// Confirm checks, before a consolidation closes, that FUL still reports the house as PHY verified it (rule
// T3): PLANNED at the frozen plan version, and allocated to this consolidation in FUL. FUL's revision moves
// when FUL records the attachment itself, so the plan version is compared, not the revision.
func (e Entry) Confirm(i LoadIntent) error {
	if i.Status != IntentPlanned || i.PlanVersion != e.PlanVersion {
		return fmt.Errorf("%w: %s is %s at plan version %d, verified at %d", ErrMemberStale, i.ShipmentID, i.Status, i.PlanVersion, e.PlanVersion)
	}
	if i.ConsolidationID != e.ConsolidationID {
		return fmt.Errorf("%w: %s", ErrMemberNotAllocated, i.ShipmentID)
	}
	return nil
}

// MembershipState is a stored membership ledger, for persistence.
type MembershipState struct {
	TenantID   TenantID
	ShipmentID ShipmentID
	Stage      string
	Entries    []Entry
}

// Membership is the aggregate root of one shipment's membership on one carriage stage (UL
// PHY-ConsolMembership): at most one live entry, so a shipment is never in two consolidations for its main
// carriage at once (I-02).
type Membership struct {
	state   MembershipState
	entries int   // entries already persisted
	ended   []int // persisted entries ended since, by index
	pending []Event
}

// NewMembership starts the empty ledger of a shipment that was never attached.
func NewMembership(tenant TenantID, shipment ShipmentID) (*Membership, error) {
	if !identifier(string(tenant)) || !identifier(string(shipment)) {
		return nil, ErrInvalidMembership
	}
	return &Membership{state: MembershipState{TenantID: tenant, ShipmentID: shipment, Stage: StageMain}}, nil
}

// Attach attaches the shipment, whole, to a consolidation that admits it. Cargo need not be received yet.
func (m *Membership) Attach(c *Consolidation, i LoadIntent, by string, at time.Time) error {
	if err := m.belongs(c, i.ShipmentID); err != nil {
		return err
	}
	if live, ok := m.Live(); ok {
		return fmt.Errorf("%w: %s", ErrAlreadyAttached, live.ConsolidationID)
	}
	if err := c.Admit(i); err != nil {
		return err
	}
	if err := m.changeable(c, by, at); err != nil {
		return err
	}
	no := m.add(c, i, StartAttached, 0, by, at)
	m.record(eventHouseAttached, at, c.ID(), no, "", "")
	return nil
}

// Detach takes the house out of an OPEN consolidation with a reason.
func (m *Membership) Detach(c *Consolidation, reason Reason, text, by string, at time.Time) error {
	i, err := m.liveIn(c)
	if err != nil {
		return err
	}
	if c.Current().State != StateOpen {
		return ErrNotOpen
	}
	if err = reason.check(text); err != nil {
		return err
	}
	if err = m.changeable(c, by, at); err != nil {
		return err
	}
	m.end(i, EndDetached, reason, text, 0, by, at)
	m.record(eventHouseDetached, at, c.ID(), m.state.Entries[i].No, "", reason)
	return nil
}

// Transfer moves the house from one OPEN consolidation to another of the same mode that admits it, in one
// step: the live entry ends as TRANSFERRED and the new entry continues it.
func (m *Membership) Transfer(from, to *Consolidation, intent LoadIntent, reason Reason, text, by string, at time.Time) error {
	i, err := m.liveIn(from)
	if err != nil {
		return err
	}
	if err = m.belongs(to, intent.ShipmentID); err != nil {
		return err
	}
	switch {
	case from.ID() == to.ID():
		return ErrSameConsolidation
	case from.Current().State != StateOpen:
		return ErrNotOpen
	case from.Mode() != to.Mode():
		return fmt.Errorf("%w: %s is %s, %s is %s", ErrModeMismatch, from.ID(), from.Mode(), to.ID(), to.Mode())
	}
	if err = reason.check(text); err != nil {
		return err
	}
	if err = to.Admit(intent); err != nil {
		return err
	}
	if err = m.changeable(to, by, at); err != nil {
		return err
	}
	previous := m.state.Entries[i].No
	m.end(i, EndTransferred, reason, text, previous+1, by, at)
	no := m.add(to, intent, StartTransferred, previous, by, at)
	m.record(eventHouseTransferred, at, to.ID(), no, from.ID(), reason)
	return nil
}

// Refresh takes a changed FUL plan into the membership: the consolidation must still admit the shipment at
// its new plan version, and a new entry freezes it. Conservation against received and loaded cargo is
// checked once cargo lots exist.
func (m *Membership) Refresh(c *Consolidation, intent LoadIntent, by string, at time.Time) error {
	i, err := m.liveIn(c)
	if err != nil {
		return err
	}
	if intent.ShipmentID != m.state.ShipmentID {
		return ErrInvalidIntent
	}
	if intent.PlanVersion == m.state.Entries[i].PlanVersion {
		return ErrNothingToRefresh
	}
	if err = c.Admit(intent); err != nil {
		return err
	}
	if err = m.changeable(c, by, at); err != nil {
		return err
	}
	previous := m.state.Entries[i].No
	m.end(i, EndRefreshed, "", "", previous+1, by, at)
	no := m.add(c, intent, StartRefreshed, previous, by, at)
	m.record(eventHouseIntentRefreshed, at, c.ID(), no, "", "")
	return nil
}

// EndForCancelledShipment ends the membership of a shipment FUL reports CANCELLED, in an OPEN or CLOSED
// consolidation.
func (m *Membership) EndForCancelledShipment(c *Consolidation, intent LoadIntent, by string, at time.Time) error {
	i, err := m.liveIn(c)
	if err != nil {
		return err
	}
	if intent.ShipmentID != m.state.ShipmentID || intent.Status != IntentCancelled {
		return ErrShipmentNotCancelled
	}
	if s := c.Current().State; s != StateOpen && s != StateClosed {
		return ErrInvalidTransition
	}
	if err = m.changeable(c, by, at); err != nil {
		return err
	}
	m.end(i, EndShipmentCancelled, ReasonShipmentCancelled, "", 0, by, at)
	m.record(eventHouseDetached, at, c.ID(), m.state.Entries[i].No, "", ReasonShipmentCancelled)
	return nil
}

// belongs checks that a consolidation and a shipment belong to this ledger's tenant and shipment.
func (m *Membership) belongs(c *Consolidation, shipment ShipmentID) error {
	if c == nil || c.Tenant() != m.state.TenantID || shipment != m.state.ShipmentID {
		return ErrInvalidMembership
	}
	return nil
}

// liveIn returns the index of the live entry, which must be in c.
func (m *Membership) liveIn(c *Consolidation) (int, error) {
	if err := m.belongs(c, m.state.ShipmentID); err != nil {
		return -1, err
	}
	for i, e := range m.state.Entries {
		if e.Live() {
			if e.ConsolidationID != c.ID() {
				break
			}
			return i, nil
		}
	}
	return -1, fmt.Errorf("%w: %s in %s", ErrNotAttached, m.state.ShipmentID, c.ID())
}

// changeable checks who changes the ledger and that the business time is not before anything recorded in it,
// nor before the consolidation was created.
func (m *Membership) changeable(c *Consolidation, by string, at time.Time) error {
	if blank(by) || at.IsZero() || at.Before(m.lastChange()) || at.Before(c.state.Versions[0].At) {
		return ErrInvalidChange
	}
	return nil
}

func (m *Membership) lastChange() time.Time {
	var last time.Time
	for _, e := range m.state.Entries {
		last = later(later(last, e.At), e.EndedAt)
	}
	return last
}

func (m *Membership) add(c *Consolidation, i LoadIntent, start StartKind, predecessor int, by string, at time.Time) int {
	no := len(m.state.Entries) + 1
	m.state.Entries = append(m.state.Entries, Entry{No: no, ConsolidationID: c.ID(), Start: start, Predecessor: predecessor,
		FULRevision: i.Revision, PlanVersion: i.PlanVersion, Scope: i.Scope, Parts: append([]Part(nil), i.Parts...), By: by, At: at.UTC()})
	return no
}

func (m *Membership) end(i int, kind EndKind, reason Reason, text string, successor int, by string, at time.Time) {
	e := &m.state.Entries[i]
	e.End, e.Reason, e.ReasonText, e.Successor, e.EndedBy, e.EndedAt = kind, reason, text, successor, by, at.UTC()
	if i < m.entries {
		m.ended = append(m.ended, i)
	}
}

func (m *Membership) record(kind string, at time.Time, consolidation ConsolidationID, entry int, from ConsolidationID, reason Reason) {
	m.pending = append(m.pending, Event{Type: kind, At: at.UTC(), TenantID: m.state.TenantID, SubjectID: string(m.state.ShipmentID),
		ConsolidationID: consolidation, ShipmentID: m.state.ShipmentID, EntryNo: entry, FromConsolidationID: from, Reason: string(reason)})
}

// Live returns the live entry.
func (m *Membership) Live() (Entry, bool) {
	for _, e := range m.state.Entries {
		if e.Live() {
			return e, true
		}
	}
	return Entry{}, false
}

// Stamp records when PHY entered the entries and ends written since the ledger was loaded (T1).
func (m *Membership) Stamp(entered time.Time) {
	for i := range m.state.Entries {
		e := &m.state.Entries[i]
		if e.EnteredAt.IsZero() {
			e.EnteredAt = entered.UTC()
		}
		if !e.Live() && e.EndEnteredAt.IsZero() {
			e.EndEnteredAt = entered.UTC()
		}
	}
}

// RestoreMembership rebuilds a stored ledger without emitting events.
func RestoreMembership(st MembershipState) (*Membership, error) {
	st.Entries = append([]Entry(nil), st.Entries...)
	m := &Membership{state: st, entries: len(st.Entries)}
	if !identifier(string(st.TenantID)) || !identifier(string(st.ShipmentID)) || st.Stage != StageMain || len(st.Entries) == 0 {
		return nil, ErrInvalidMembership
	}
	live := 0
	for i, e := range st.Entries {
		if !e.valid(i, st.Entries) {
			return nil, fmt.Errorf("%w: entry %d", ErrInvalidMembership, i+1)
		}
		if e.Live() {
			live++
		}
	}
	if live > 1 {
		return nil, fmt.Errorf("%w: %d live entries", ErrInvalidMembership, live)
	}
	return m, nil
}

func (e Entry) valid(i int, all []Entry) bool {
	if e.No != i+1 || !identifier(string(e.ConsolidationID)) || e.FULRevision < 1 || e.PlanVersion < 1 || !e.Scope.valid() ||
		len(e.Parts) == 0 || blank(e.By) || e.At.IsZero() || (i > 0 && e.At.Before(all[i-1].At)) {
		return false
	}
	for _, p := range e.Parts {
		if !p.valid(e.Scope) {
			return false
		}
	}
	switch e.Start {
	case StartAttached:
		if e.Predecessor != 0 {
			return false
		}
	case StartTransferred, StartRefreshed:
		if e.Predecessor < 1 || e.Predecessor > i {
			return false
		}
		p := all[e.Predecessor-1]
		if p.Successor != e.No || string(p.End) != string(e.Start) || (p.ConsolidationID == e.ConsolidationID) != (e.Start == StartRefreshed) {
			return false
		}
	default:
		return false
	}
	if e.Live() {
		return e.Reason == "" && e.ReasonText == "" && e.EndedBy == "" && e.EndedAt.IsZero() && e.Successor == 0
	}
	if blank(e.EndedBy) || e.EndedAt.Before(e.At) {
		return false
	}
	switch e.End {
	case EndDetached:
		return e.Reason.check(e.ReasonText) == nil && e.Successor == 0
	case EndTransferred:
		return e.Reason.check(e.ReasonText) == nil && e.Successor == e.No+1
	case EndRefreshed:
		return e.Reason == "" && e.ReasonText == "" && e.Successor == e.No+1
	case EndShipmentCancelled:
		return e.Reason == ReasonShipmentCancelled && e.ReasonText == "" && e.Successor == 0
	}
	return false
}

// State returns a copy of the ledger for persistence and read models.
func (m *Membership) State() MembershipState {
	st := m.state
	st.Entries = append([]Entry(nil), st.Entries...)
	return st
}

func (m *Membership) ShipmentID() ShipmentID { return m.state.ShipmentID }
func (m *Membership) Pending() []Event       { return append([]Event(nil), m.pending...) }

// AddedEntries returns the entries written since the ledger was created or restored; EndedEntries the stored
// entries ended since, by number. Both for persistence.
func (m *Membership) AddedEntries() []Entry {
	return append([]Entry(nil), m.state.Entries[m.entries:]...)
}

func (m *Membership) EndedEntries() map[int]Entry {
	ended := map[int]Entry{}
	for _, i := range m.ended {
		ended[i+1] = m.state.Entries[i]
	}
	return ended
}

// Acknowledge is permitted only after the transaction holding the ledger and its Outbox events commits.
func (m *Membership) Acknowledge() {
	m.pending, m.ended = nil, nil
	m.entries = len(m.state.Entries)
}
