package domain

import (
	"fmt"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// State is an air movement's state; FINAL comes with AirDocFacts v1 once PHY's measures and screening reach AIR.
type State string

const (
	StateOpen      State = "OPEN"
	StateCancelled State = "CANCELLED"
)

// MAWB is the movement's master air waybill number, taken from the airline's booking confirmation (OD-7); it is
// voided only with the movement, since another MAWB is another movement.
type MAWB struct {
	Number          string
	Voided          bool
	BookingID       string
	CapacityVersion int
}

// Booking is what AIR reads of a CarrierBooking v1 to link a movement to it (T3).
type Booking struct {
	BookingID       string
	Revision        int64
	AirConsol       bool
	InForce         bool // CONFIRMED, CHANGE_REQUESTED or CANCELLATION_REQUESTED, with capacity
	ConsumerID      string
	LegalEntityID   string
	Airline         Airline
	Origin          Airport
	Destination     Airport
	Edition         string
	MAWB            string
	CapacityVersion int
	Lines           int
	Segments        []Segment // the legs of its one capacity line
}

// Link is one immutable link to a capacity version of the booking, with its flights; the latest is current.
type Link struct {
	No              int
	BookingID       string
	BookingRevision int64
	CapacityVersion int
	Segments        []Segment
	By              string
	At              time.Time
	EnteredAt       time.Time
}

// Plan is what AIR reads of FUL's AirOperationPlan v1 for a house (T3).
type Plan struct {
	ShipmentID      string
	Revision        int64
	Status          string // DRAFT, PLANNED or CANCELLED
	PlanVersion     int
	LegalEntityID   string
	Mode            string
	Origin          kernel.LocationCode
	Destination     kernel.LocationCode
	Window          kernel.ValidityInterval
	Pieces          int64
	GrossWeight     kernel.Quantity
	Volume          kernel.Quantity
	Parts           []Part
	ConsolidationID string
}

// Facts are a house's frozen quantities with its documented chargeable weight in grams.
type Facts struct {
	Pieces          int64
	GrossWeight     kernel.Quantity
	Volume          kernel.Quantity
	ChargeableGrams int64
	Rule            string
	Basis           string
	Parts           []Part
}

// Start and end kinds of an entry.
const (
	StartAssociated = "ASSOCIATED"
	StartRefreshed  = "REFRESHED"
	EndRefreshed    = "REFRESHED"
	EndDetached     = "DETACHED"
	EndTransferred  = "TRANSFERRED"
	EndCancelled    = "SHIPMENT_CANCELLED"
)

// Entry is one immutable house air waybill entry; only its end is written later, once. A refresh ends one entry
// and starts the next with the same HAWB.
type Entry struct {
	No           int
	ShipmentID   string
	HAWB         string
	FULRevision  int64
	PlanVersion  int
	Window       kernel.ValidityInterval
	Facts        Facts
	Start        string
	Predecessor  int
	By           string
	At           time.Time
	EnteredAt    time.Time
	End          string
	EndedBy      string
	EndedAt      time.Time
	EndEnteredAt time.Time
	Successor    int
}

// Live reports whether the entry has not ended.
func (e Entry) Live() bool { return e.End == "" }

// MovementState is a stored movement, for persistence.
type MovementState struct {
	TenantID        TenantID
	MovementID      MovementID
	ConsolidationID string
	LegalEntityID   string
	Airline         Airline
	Origin          Airport
	Destination     Airport
	Edition         string
	State           State
	MAWB            MAWB
	Links           []Link
	Entries         []Entry
	CancelReason    string
	CancelledBy     string
	CancelledAt     time.Time
}

// Movement is the aggregate root (AirMovement). Links and entries are only added; an entry ends once.
type Movement struct {
	state   MovementState
	links   int
	entries int
	ended   []int
	pending []Event
}

// Event carries identifiers only.
type Event struct {
	Type            string
	At              time.Time
	TenantID        TenantID
	SubjectID       string
	ConsolidationID string
	ShipmentID      string
	EntryNo         int
	BookingID       string
	CapacityVersion int
	Reason          string
}

const (
	eventOpened     = "AirMovementOpened.v1"
	eventLinked     = "FlightSegmentsLinked.v1"
	eventAssigned   = "MawbAssigned.v1"
	eventVoided     = "MawbVoided.v1"
	eventAssociated = "HouseWaybillAssociated.v1"
	eventRefreshed  = "HouseWaybillFactsRefreshed.v1"
	eventEnded      = "HouseWaybillAssociationEnded.v1"
	eventCancelled  = "AirMovementCancelled.v1"
)

// OpenMovement opens the air movement of a consolidation from its booking: an air consolidation booking in force,
// allocated to the consolidation, with one capacity line and a MAWB of the airline's prefix. The movement takes the
// airline, operating legal entity, route, MAWB and flights from it.
func OpenMovement(tenant TenantID, id MovementID, consolidation string, b Booking, by string, at time.Time) (*Movement, error) {
	if !identifier(string(tenant)) || !identifier(string(id)) || !identifier(consolidation) || blank(by) || at.IsZero() {
		return nil, ErrInvalidMovement
	}
	if err := b.check(consolidation); err != nil {
		return nil, err
	}
	m := &Movement{state: MovementState{TenantID: tenant, MovementID: id, ConsolidationID: consolidation, LegalEntityID: b.LegalEntityID,
		Airline: b.Airline, Origin: b.Origin, Destination: b.Destination, Edition: b.Edition, State: StateOpen,
		MAWB: MAWB{Number: b.MAWB, BookingID: b.BookingID, CapacityVersion: b.CapacityVersion}}}
	m.addLink(b, by, at)
	m.record(eventOpened, at, "", 0, "")
	m.record(eventAssigned, at, "", 0, "")
	m.record(eventLinked, at, "", 0, "")
	return m, nil
}

// check validates a booking for a consolidation.
func (b Booking) check(consolidation string) error {
	bad := func(what string) error { return fmt.Errorf("%w: %s", ErrInvalidBooking, what) }
	switch {
	case !b.AirConsol || !b.InForce:
		return bad("an air consolidation booking with confirmed capacity in force")
	case b.ConsumerID != consolidation:
		return bad("allocated to another consolidation")
	case !identifier(b.BookingID) || b.Revision < 1 || b.CapacityVersion < 1 || !identifier(b.LegalEntityID) || !b.Airline.valid():
		return bad("booking, capacity version, legal entity and airline codes")
	case !ValidMAWB(b.MAWB) || b.MAWB[:3] != b.Airline.AWBPrefix:
		return bad("a MAWB number of the airline's prefix")
	case b.Lines != 1:
		return bad("one capacity line; a split booking is not carried in V0.1")
	case !b.Origin.valid() || !b.Destination.valid() || b.Origin.Code == b.Destination.Code || !editionForm.MatchString(b.Edition):
		return bad("a resolved airport route")
	}
	return checkSegments(b.Segments, b.Origin, b.Destination, b.Airline.Designator)
}

func checkSegments(segments []Segment, origin, destination Airport, designator string) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidBooking, fmt.Sprintf(format, args...))
	}
	if len(segments) == 0 || len(segments) > 9 {
		return bad("one to nine flights")
	}
	for i, s := range segments {
		switch {
		case !flightForm.MatchString(s.Flight) || s.Flight[:2] != designator:
			return bad("flight %d %q of the airline %s", i+1, s.Flight, designator)
		case !s.From.valid() || !s.To.valid() || s.From.Code == s.To.Code:
			return bad("flight %d airports", i+1)
		case i == 0 && s.From.Code != origin.Code, i == len(segments)-1 && s.To.Code != destination.Code:
			return bad("the flights run from the movement's origin to its destination")
		case i > 0 && (s.From.Code != segments[i-1].To.Code || s.ETD.Before(segments[i-1].ETD)):
			return bad("flight %d does not follow flight %d", i+1, i)
		case s.ETD.IsZero() || (!s.ETA.IsZero() && !s.ETA.After(s.ETD)):
			return bad("flight %d estimated times", i+1)
		}
	}
	return nil
}

// LinkBooking takes a newer capacity version of the movement's booking: same booking, airline, consolidation,
// route and MAWB; the flights are replaced and the old ones kept. Houses stay as they are.
func (m *Movement) LinkBooking(b Booking, by string, at time.Time) error {
	if m.state.State != StateOpen {
		return ErrInvalidTransition
	}
	if err := b.check(m.state.ConsolidationID); err != nil {
		return err
	}
	current := m.Current()
	switch {
	case b.BookingID != current.BookingID:
		return fmt.Errorf("%w: another booking is another movement", ErrInvalidBooking)
	case b.CapacityVersion <= current.CapacityVersion:
		return fmt.Errorf("%w: capacity version %d is linked already", ErrInvalidChange, current.CapacityVersion)
	case b.MAWB != m.state.MAWB.Number || b.Airline.PartyID != m.state.Airline.PartyID || b.Origin.Code != m.state.Origin.Code ||
		b.Destination.Code != m.state.Destination.Code || b.LegalEntityID != m.state.LegalEntityID:
		return fmt.Errorf("%w: another MAWB, airline, route or legal entity is another movement", ErrInvalidBooking)
	}
	if err := m.changeable(by, at); err != nil {
		return err
	}
	m.addLink(b, by, at)
	m.record(eventLinked, at, "", 0, "")
	return nil
}

func (m *Movement) addLink(b Booking, by string, at time.Time) {
	segments := append([]Segment(nil), b.Segments...)
	for i := range segments {
		segments[i].ETD, segments[i].ETA = segments[i].ETD.UTC(), utc(segments[i].ETA)
	}
	m.state.Links = append(m.state.Links, Link{No: len(m.state.Links) + 1, BookingID: b.BookingID, BookingRevision: b.Revision,
		CapacityVersion: b.CapacityVersion, Segments: segments, By: by, At: at.UTC()})
}

// admits checks a house's plan against the movement.
func (m *Movement) admits(p Plan) error {
	switch {
	case m.state.State != StateOpen:
		return ErrInvalidTransition
	case p.Status != "PLANNED" || p.Mode != "AIR" || p.ConsolidationID != m.state.ConsolidationID || p.LegalEntityID != m.state.LegalEntityID ||
		p.Origin != m.state.Origin.Code || p.Destination != m.state.Destination.Code:
		return fmt.Errorf("%w: %s is %s %s in %q of %s, %s-%s", ErrNotAttached, p.ShipmentID, p.Status, p.Mode, p.ConsolidationID, p.LegalEntityID,
			p.Origin.Code, p.Destination.Code)
	case !identifier(p.ShipmentID) || p.Revision < 1 || p.PlanVersion < 1 || p.Pieces < 1 || len(p.Parts) == 0:
		return fmt.Errorf("%w: plan of %s", ErrInvalidChange, p.ShipmentID)
	}
	return nil
}

func facts(p Plan) (Facts, error) {
	grams, err := Grams(p.GrossWeight)
	if err != nil {
		return Facts{}, err
	}
	cm3, err := CubicCentimetres(p.Volume)
	if err != nil {
		return Facts{}, err
	}
	if grams < 1 {
		return Facts{}, fmt.Errorf("%w: a house needs its gross weight", ErrInvalidChange)
	}
	return Facts{Pieces: p.Pieces, GrossWeight: p.GrossWeight, Volume: p.Volume, ChargeableGrams: Chargeable(grams, cm3), Rule: ChargeableRule,
		Basis: BasisPlanned, Parts: append([]Part(nil), p.Parts...)}, nil
}

// Associate associates a house that FUL reports attached to the consolidation, with its HAWB number.
func (m *Movement) Associate(p Plan, hawb, by string, at time.Time) error {
	if err := m.admits(p); err != nil {
		return err
	}
	if !ValidHAWB(hawb) {
		return ErrInvalidHAWB
	}
	for _, e := range m.state.Entries {
		if e.Live() && (e.ShipmentID == p.ShipmentID || e.HAWB == hawb) {
			return fmt.Errorf("%w: %s / %s", ErrAlreadyAssociated, e.ShipmentID, e.HAWB)
		}
	}
	f, err := facts(p)
	if err != nil {
		return err
	}
	if err = m.changeable(by, at); err != nil {
		return err
	}
	no := m.addEntry(p, hawb, f, StartAssociated, 0, by, at)
	m.record(eventAssociated, at, p.ShipmentID, no, "")
	return nil
}

// Refresh takes FUL's newer plan version of a live house into a new entry with the same HAWB.
func (m *Movement) Refresh(p Plan, by string, at time.Time) error {
	i, err := m.live(p.ShipmentID)
	if err != nil {
		return err
	}
	if err = m.admits(p); err != nil {
		return err
	}
	if p.PlanVersion == m.state.Entries[i].PlanVersion {
		return ErrNothingToRefresh
	}
	f, err := facts(p)
	if err != nil {
		return err
	}
	if err = m.changeable(by, at); err != nil {
		return err
	}
	old := m.state.Entries[i]
	m.end(i, EndRefreshed, len(m.state.Entries)+1, by, at) // entries are numbered across the movement
	no := m.addEntry(p, old.HAWB, f, StartRefreshed, old.No, by, at)
	m.record(eventRefreshed, at, p.ShipmentID, no, "")
	return nil
}

// EndAssociation ends a house's entry once FUL no longer reports the shipment attached to the consolidation:
// TRANSFERRED when attached to another, DETACHED when to none, SHIPMENT_CANCELLED when cancelled.
func (m *Movement) EndAssociation(p Plan, by string, at time.Time) error {
	if m.state.State != StateOpen {
		return ErrInvalidTransition
	}
	i, err := m.live(p.ShipmentID)
	if err != nil {
		return err
	}
	kind := EndDetached
	switch {
	case p.Status == "CANCELLED":
		kind = EndCancelled
	case p.ConsolidationID == m.state.ConsolidationID:
		return ErrStillAttached
	case p.ConsolidationID != "":
		kind = EndTransferred
	}
	if err = m.changeable(by, at); err != nil {
		return err
	}
	m.end(i, kind, 0, by, at)
	m.record(eventEnded, at, p.ShipmentID, m.state.Entries[i].No, kind)
	return nil
}

// Cancel cancels an open movement without houses and voids its MAWB.
func (m *Movement) Cancel(reason, by string, at time.Time) error {
	if m.state.State != StateOpen {
		return ErrInvalidTransition
	}
	if len(m.Houses()) > 0 {
		return ErrHousesLive
	}
	if !text(reason) {
		return fmt.Errorf("%w: a cancellation needs a reason", ErrInvalidChange)
	}
	if err := m.changeable(by, at); err != nil {
		return err
	}
	m.state.State, m.state.MAWB.Voided = StateCancelled, true
	m.state.CancelReason, m.state.CancelledBy, m.state.CancelledAt = reason, by, at.UTC()
	m.record(eventVoided, at, "", 0, "")
	m.record(eventCancelled, at, "", 0, "")
	return nil
}

func (m *Movement) live(shipment string) (int, error) {
	for i, e := range m.state.Entries {
		if e.Live() && e.ShipmentID == shipment {
			return i, nil
		}
	}
	return -1, fmt.Errorf("%w: %s", ErrNotAssociated, shipment)
}

func (m *Movement) addEntry(p Plan, hawb string, f Facts, start string, predecessor int, by string, at time.Time) int {
	no := len(m.state.Entries) + 1
	m.state.Entries = append(m.state.Entries, Entry{No: no, ShipmentID: p.ShipmentID, HAWB: hawb, FULRevision: p.Revision, PlanVersion: p.PlanVersion,
		Window: kernel.ValidityInterval{From: p.Window.From.UTC(), Until: utc(p.Window.Until)}, Facts: f, Start: start, Predecessor: predecessor,
		By: by, At: at.UTC()})
	return no
}

func (m *Movement) end(i int, kind string, successor int, by string, at time.Time) {
	e := &m.state.Entries[i]
	e.End, e.Successor, e.EndedBy, e.EndedAt = kind, successor, by, at.UTC()
	if i < m.entries {
		m.ended = append(m.ended, i)
	}
}

func (m *Movement) changeable(by string, at time.Time) error {
	if blank(by) || at.IsZero() || at.Before(m.lastChange()) {
		return fmt.Errorf("%w: author and business time not before the movement's last change", ErrInvalidChange)
	}
	return nil
}

func (m *Movement) lastChange() time.Time {
	last := m.state.CancelledAt
	for _, l := range m.state.Links {
		last = later(last, l.At)
	}
	for _, e := range m.state.Entries {
		last = later(later(last, e.At), e.EndedAt)
	}
	return last
}

func (m *Movement) record(kind string, at time.Time, shipment string, entry int, reason string) {
	current := m.Current()
	m.pending = append(m.pending, Event{Type: kind, At: at.UTC(), TenantID: m.state.TenantID, SubjectID: string(m.state.MovementID),
		ConsolidationID: m.state.ConsolidationID, ShipmentID: shipment, EntryNo: entry, BookingID: current.BookingID,
		CapacityVersion: current.CapacityVersion, Reason: reason})
}

// Current returns the current booking link.
func (m *Movement) Current() Link {
	if len(m.state.Links) == 0 {
		return Link{}
	}
	return m.state.Links[len(m.state.Links)-1]
}

// Houses returns the live entries.
func (m *Movement) Houses() []Entry {
	var out []Entry
	for _, e := range m.state.Entries {
		if e.Live() {
			out = append(out, e)
		}
	}
	return out
}

// Totals returns the master totals of the live houses, in grams and cubic centimetres, and the master
// chargeable weight computed on them.
func (m *Movement) Totals() (pieces, grams, cm3, chargeable int64) {
	for _, e := range m.Houses() {
		g, _ := Grams(e.Facts.GrossWeight)
		v, _ := CubicCentimetres(e.Facts.Volume)
		pieces, grams, cm3 = pieces+e.Facts.Pieces, grams+g, cm3+v
	}
	if pieces > 0 {
		chargeable = Chargeable(grams, cm3)
	}
	return pieces, grams, cm3, chargeable
}

// WindowDrift reports whether a house's departure window no longer contains the current first flight's ETD.
func (m *Movement) WindowDrift(e Entry) bool {
	segments := m.Current().Segments
	return len(segments) > 0 && !e.Window.Contains(segments[0].ETD)
}

// Stamp records when AIR entered what was added or ended since the movement was loaded (T1).
func (m *Movement) Stamp(entered time.Time) {
	for i := m.links; i < len(m.state.Links); i++ {
		m.state.Links[i].EnteredAt = entered.UTC()
	}
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

// RestoreMovement rebuilds a stored movement without emitting events.
func RestoreMovement(st MovementState) (*Movement, error) {
	st.Links = append([]Link(nil), st.Links...)
	st.Entries = append([]Entry(nil), st.Entries...)
	m := &Movement{state: st, links: len(st.Links), entries: len(st.Entries)}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidMovement, err)
	}
	return m, nil
}

func (m *Movement) validate() error {
	st := m.state
	cancelled := st.State == StateCancelled
	switch {
	case !identifier(string(st.TenantID)) || !identifier(string(st.MovementID)) || !identifier(st.ConsolidationID) || !identifier(st.LegalEntityID) ||
		!st.Airline.valid() || !st.Origin.valid() || !st.Destination.valid():
		return fmt.Errorf("identity")
	case st.State != StateOpen && !cancelled:
		return fmt.Errorf("state %s", st.State)
	case !ValidMAWB(st.MAWB.Number) || st.MAWB.Number[:3] != st.Airline.AWBPrefix || st.MAWB.Voided != cancelled:
		return fmt.Errorf("MAWB")
	case cancelled != text(st.CancelReason) || cancelled == blank(st.CancelledBy):
		return fmt.Errorf("cancellation")
	case len(st.Links) == 0:
		return fmt.Errorf("no booking link")
	}
	for i, l := range st.Links {
		if l.No != i+1 || l.BookingID != st.Links[0].BookingID || (i > 0 && l.CapacityVersion <= st.Links[i-1].CapacityVersion) ||
			checkSegments(l.Segments, st.Origin, st.Destination, st.Airline.Designator) != nil {
			return fmt.Errorf("link %d", i+1)
		}
	}
	live := map[string]bool{}
	for i, e := range st.Entries {
		if e.No != i+1 || !identifier(e.ShipmentID) || !ValidHAWB(e.HAWB) || e.FULRevision < 1 || e.PlanVersion < 1 || e.Facts.Pieces < 1 ||
			e.Facts.ChargeableGrams < 1 || blank(e.By) || e.At.IsZero() {
			return fmt.Errorf("entry %d", i+1)
		}
		switch e.Start {
		case StartAssociated:
			if e.Predecessor != 0 {
				return fmt.Errorf("entry %d predecessor", i+1)
			}
		case StartRefreshed:
			if e.Predecessor < 1 || e.Predecessor > i || st.Entries[e.Predecessor-1].Successor != e.No || st.Entries[e.Predecessor-1].HAWB != e.HAWB ||
				st.Entries[e.Predecessor-1].ShipmentID != e.ShipmentID {
				return fmt.Errorf("entry %d predecessor", i+1)
			}
		default:
			return fmt.Errorf("entry %d start", i+1)
		}
		if e.Live() {
			if live[e.ShipmentID] || live["hawb:"+e.HAWB] || cancelled || e.Successor != 0 {
				return fmt.Errorf("entry %d live", i+1)
			}
			live[e.ShipmentID], live["hawb:"+e.HAWB] = true, true
			continue
		}
		switch e.End {
		case EndRefreshed:
			if e.Successor <= e.No || e.Successor > len(st.Entries) || st.Entries[e.Successor-1].Predecessor != e.No {
				return fmt.Errorf("entry %d successor", i+1)
			}
		case EndDetached, EndTransferred, EndCancelled:
			if e.Successor != 0 {
				return fmt.Errorf("entry %d successor", i+1)
			}
		default:
			return fmt.Errorf("entry %d end", i+1)
		}
		if blank(e.EndedBy) || e.EndedAt.Before(e.At) {
			return fmt.Errorf("entry %d end", i+1)
		}
	}
	return nil
}

// State returns a copy of the movement for persistence and read models.
func (m *Movement) State() MovementState {
	st := m.state
	st.Links = append([]Link(nil), st.Links...)
	st.Entries = append([]Entry(nil), st.Entries...)
	return st
}

func (m *Movement) ID() MovementID   { return m.state.MovementID }
func (m *Movement) Pending() []Event { return append([]Event(nil), m.pending...) }

// Added returns the links and entries written since the movement was loaded, and the stored entries ended since,
// by number.
func (m *Movement) Added() ([]Link, []Entry, map[int]Entry) {
	ended := map[int]Entry{}
	for _, i := range m.ended {
		ended[i+1] = m.state.Entries[i]
	}
	return m.state.Links[m.links:], m.state.Entries[m.entries:], ended
}

// Acknowledge is permitted only after the transaction holding the movement and its Outbox events commits.
func (m *Movement) Acknowledge() {
	m.pending, m.ended = nil, nil
	m.links, m.entries = len(m.state.Links), len(m.state.Entries)
}

func utc(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC()
}
