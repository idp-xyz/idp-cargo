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
	// ReworkExecutionID and ReworkNo name the rework of a sealed container in which the house was unloaded, when the
	// entry ended in a CLOSED consolidation as DETACHED or TRANSFERRED (V2.86); empty and 0 otherwise.
	ReworkExecutionID LoadExecutionID
	ReworkNo          int
}

// ReworkRef is the rework of a sealed container in which a house was unloaded whole: the execution, the rework's number
// and the business time of the house's latest reversal in it. A house that was unloaded in a rework of the
// consolidation may leave it though it is CLOSED; a house that was never loaded has no such rework, and leaves a CLOSED
// sea consolidation with no link since V2.87.
type ReworkRef struct {
	ExecutionID LoadExecutionID
	No          int
	UnloadedAt  time.Time
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

// Detach takes the house out of a consolidation with a reason: an OPEN one, or a CLOSED sea one when the caller found
// no rework that unloaded the house (it is then a house with no live packs, shut out before stuffing or taken out by
// ordinary reversals; the caller has checked that nothing of the house is loaded). The entry records no rework.
func (m *Membership) Detach(c *Consolidation, reason Reason, text, by string, at time.Time) error {
	return m.detach(c, nil, time.Time{}, reason, text, by, at)
}

// DetachAfterReversals is Detach for a house of a CLOSED sea consolidation that no rework unloaded, when the caller has
// found the business time of the house's latest reversal in the consolidation (the zero time when it was never loaded):
// the leaving is not dated before it. The caller has checked that nothing of the house is loaded.
func (m *Membership) DetachAfterReversals(c *Consolidation, lastReversed time.Time, reason Reason, text, by string, at time.Time) error {
	return m.detach(c, nil, lastReversed, reason, text, by, at)
}

// DetachAfterRework takes the house out of a CLOSED consolidation (or an OPEN one) with a reason, when it was unloaded
// whole in the rework of a sealed container of it; the entry that ends records the rework. Whether the container is
// still in rework or sealed again does not matter. The caller has checked that nothing of the house is loaded.
func (m *Membership) DetachAfterRework(c *Consolidation, rework ReworkRef, reason Reason, text, by string, at time.Time) error {
	return m.detach(c, &rework, time.Time{}, reason, text, by, at)
}

func (m *Membership) detach(c *Consolidation, rework *ReworkRef, lastReversed time.Time, reason Reason, text, by string, at time.Time) error {
	i, err := m.liveIn(c)
	if err != nil {
		return err
	}
	link, err := leaving(c, rework, lastReversed, at)
	if err != nil {
		return err
	}
	if err = reason.check(text); err != nil {
		return err
	}
	if err = m.changeable(c, by, at); err != nil {
		return err
	}
	m.end(i, EndDetached, reason, text, 0, by, at)
	m.linkRework(i, link)
	m.record(eventHouseDetached, at, c.ID(), m.state.Entries[i].No, "", reason)
	return nil
}

// leaving says whether a house may leave the consolidation and which rework the ended entry records. An OPEN
// consolidation lets it go and records none (a rework is only ever opened on a CLOSED consolidation). A CLOSED one lets
// it go with the rework that unloaded it, and the leaving is not dated before that rework's own unloading (its latest
// reversal in it; a later ordinary reversal of the house, in another container, is not looked at). With no rework a
// CLOSED consolidation of a sea kind lets it go too and records none (V2.87: the house was shut out before stuffing,
// or taken out by ordinary reversals only), and a CLOSED air consolidation does not (AIR's documents were not
// analysed for it, so a house of one stays frozen). A leaving with no rework is not dated before the house's latest
// reversal in the consolidation, which the caller hands over (the zero time when the house was never loaded; V2.87).
// Every other state refuses.
func leaving(c *Consolidation, rework *ReworkRef, lastReversed, at time.Time) (ReworkRef, error) {
	switch c.Current().State {
	case StateOpen:
		return ReworkRef{}, nil
	case StateClosed:
		if rework == nil {
			if k := c.state.Kind; k != KindSeaLCLOwn && k != KindSeaLCLCoload {
				return ReworkRef{}, ErrNotOpen
			}
			if at.IsZero() || at.Before(lastReversed) {
				return ReworkRef{}, ErrInvalidChange
			}
			return ReworkRef{}, nil
		}
		if !identifier(string(rework.ExecutionID)) || rework.No < 1 || at.IsZero() || at.Before(rework.UnloadedAt) {
			return ReworkRef{}, ErrInvalidChange
		}
		return *rework, nil
	}
	return ReworkRef{}, ErrNotOpen
}

// Transfer moves the house from one OPEN consolidation to another of the same mode that admits it, in one
// step: the live entry ends as TRANSFERRED and the new entry continues it.
func (m *Membership) Transfer(from, to *Consolidation, intent LoadIntent, reason Reason, text, by string, at time.Time) error {
	return m.transfer(from, to, intent, nil, time.Time{}, reason, text, by, at)
}

// TransferAfterRework is Transfer for a house that was unloaded whole in the rework of a sealed container of a CLOSED
// consolidation (see DetachAfterRework); the target must be OPEN and admit the house, as for any transfer.
func (m *Membership) TransferAfterRework(from, to *Consolidation, intent LoadIntent, rework ReworkRef, reason Reason, text, by string, at time.Time) error {
	return m.transfer(from, to, intent, &rework, time.Time{}, reason, text, by, at)
}

// TransferAfterReversals is Transfer for a house leaving a CLOSED sea consolidation that no rework unloaded (see
// DetachAfterReversals).
func (m *Membership) TransferAfterReversals(from, to *Consolidation, intent LoadIntent, lastReversed time.Time, reason Reason, text, by string, at time.Time) error {
	return m.transfer(from, to, intent, nil, lastReversed, reason, text, by, at)
}

func (m *Membership) transfer(from, to *Consolidation, intent LoadIntent, rework *ReworkRef, lastReversed time.Time, reason Reason, text, by string, at time.Time) error {
	i, err := m.liveIn(from)
	if err != nil {
		return err
	}
	if err = m.belongs(to, intent.ShipmentID); err != nil {
		return err
	}
	if from.ID() == to.ID() {
		return ErrSameConsolidation
	}
	link, err := leaving(from, rework, lastReversed, at)
	if err != nil {
		return err
	}
	if from.Mode() != to.Mode() {
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
	m.linkRework(i, link)
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

// linkRework records on the entry just ended the rework that unloaded its house; no rework records nothing.
func (m *Membership) linkRework(i int, rework ReworkRef) {
	e := &m.state.Entries[i]
	e.ReworkExecutionID, e.ReworkNo = rework.ExecutionID, rework.No
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

func (e Entry) noRework() bool { return e.ReworkExecutionID == "" && e.ReworkNo == 0 }

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
		return e.Reason == "" && e.ReasonText == "" && e.EndedBy == "" && e.EndedAt.IsZero() && e.Successor == 0 && e.noRework()
	}
	if blank(e.EndedBy) || e.EndedAt.Before(e.At) {
		return false
	}
	// Only a detach or a transfer names a rework, and a name is an execution and a number together.
	if (e.End != EndDetached && e.End != EndTransferred && !e.noRework()) || (e.ReworkExecutionID == "") != (e.ReworkNo == 0) || e.ReworkNo < 0 ||
		(e.ReworkExecutionID != "" && !identifier(string(e.ReworkExecutionID))) {
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

// Leavable checks that a house leaving its consolidation (detached, transferred, its shipment cancelled) has
// nothing loaded there: loaded is its live loaded packs in the consolidation.
func Leavable(loaded int64) error {
	if loaded > 0 {
		return fmt.Errorf("%w: %d packs", ErrLiveLoadLines, loaded)
	}
	return nil
}

// Replannable checks that FUL's new plan of a house keeps at least the packs loaded of it in its consolidation.
func Replannable(i LoadIntent, loaded int64) error {
	if loaded > i.Scope.Packs {
		return fmt.Errorf("%w: FUL plans %d packs of %s, %d are loaded; unload first", ErrConservation, i.Scope.Packs, i.ShipmentID, loaded)
	}
	return nil
}
