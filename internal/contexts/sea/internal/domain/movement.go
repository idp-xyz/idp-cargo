package domain

import (
	"fmt"
	"slices"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// State is an ocean movement's state: OPEN or CANCELLED. FINAL comes with the facts finalized for DOC.
type State string

const (
	StateOpen      State = "OPEN"
	StateFinal     State = "FINAL"
	StateCancelled State = "CANCELLED"
)

// Booking is what SEA reads of a CarrierBooking v1 to link a movement to it (T3).
type Booking struct {
	BookingID       string
	Revision        int64
	Kind            Kind // KindOwn for a container booking, KindCoLoad for a co-load; empty for any other booking
	InForce         bool // CONFIRMED, CHANGE_REQUESTED or CANCELLATION_REQUESTED, with capacity
	ConsumerID      string
	LegalEntityID   string
	Carrier         Carrier
	Origin          kernel.LocationCode
	Destination     kernel.LocationCode
	Edition         string
	CapacityVersion int
	Lines           int
	Legs            []Leg // the legs of its one capacity line
}

// Link is one immutable link to a capacity version of the booking, with its voyage legs; the latest is current.
type Link struct {
	No              int
	BookingID       string
	BookingRevision int64
	CapacityVersion int
	Legs            []Leg
	By              string
	At              time.Time
	EnteredAt       time.Time
}

// Master bill kinds and statuses.
const (
	BillCarrierMBL        = "CARRIER_MBL"
	BillCarrierSeaWaybill = "CARRIER_SEA_WAYBILL"
	BillCoLoad            = "COLOAD_BILL"

	BillCurrent    = "CURRENT"
	BillSuperseded = "SUPERSEDED"
	BillVoided     = "VOIDED"
)

// Evidence references the stored document a master bill number was read from.
type Evidence struct {
	SHA256     string
	StorageID  string
	MediaType  string
	CapturedAt time.Time
}

func (e Evidence) valid() bool {
	return hexForm.MatchString(e.SHA256) && short(e.StorageID, 256) && short(e.MediaType, 127) && !e.CapturedAt.IsZero()
}

// MasterBill is one master bill reference as the movement's carrier or co-loader stated it. Only its status changes:
// the latest is CURRENT until a further one supersedes it or the movement is cancelled.
type MasterBill struct {
	No        int
	Number    string
	Kind      string
	Evidence  Evidence
	Status    string
	By        string
	At        time.Time
	EnteredAt time.Time
}

// fits reports whether a master bill kind belongs to a movement kind: the shipping line's bill or sea waybill on an
// own container, the co-loader's bill on a co-load.
func fits(bill string, k Kind) bool {
	if k == KindCoLoad {
		return bill == BillCoLoad
	}
	return bill == BillCarrierMBL || bill == BillCarrierSeaWaybill
}

// Plan is what SEA reads of FUL's OceanOperationPlan v1 for a house (T3).
type Plan struct {
	ShipmentID      string
	Revision        int64
	Status          string // DRAFT, PLANNED or CANCELLED
	PlanVersion     int
	LegalEntityID   string
	Mode            string
	ContainerMode   string // LCL, FCL or empty when not planned
	Origin          kernel.LocationCode
	Destination     kernel.LocationCode
	Window          kernel.ValidityInterval
	Packs           int64
	GrossWeight     kernel.Quantity
	Volume          kernel.Quantity
	Parts           []Part
	ConsolidationID string
	// PHY's verified measures and sealed containers as FUL relays them; MeasuredBasis is empty while none.
	EvidenceVersion int
	MeasuredBasis   string
	MeasuredPacks   int64
	MeasuredWeight  kernel.Quantity
	MeasuredVolume  kernel.Quantity
	Stuffings       []Stuffing
	// EvidenceBehind is FUL's own answer, read with the plan, that RecordVerifiedLoadEvidence would record a new
	// evidence version now: PHY's facts of the house are newer than the ones the plan carries. It is not part of
	// what SEA freezes.
	EvidenceBehind bool
}

// Facts are a house's frozen quantities, parts and per-container breakdown.
type Facts struct {
	Packs       int64
	GrossWeight kernel.Quantity
	Volume      kernel.Quantity
	Basis       string
	Evidence    int // FUL's evidence version the measures or containers came from; 0 when neither did
	Parts       []Part
	Stuffings   []Stuffing
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

// Entry is one immutable house bill entry; only its end is written later, once. A refresh ends one entry and starts
// the next with the same HBL.
type Entry struct {
	No            int
	ShipmentID    string
	HBL           string
	FULRevision   int64
	PlanVersion   int
	ContainerMode string
	Window        kernel.ValidityInterval
	Facts         Facts
	Start         string
	Predecessor   int
	By            string
	At            time.Time
	EnteredAt     time.Time
	End           string
	EndedBy       string
	EndedAt       time.Time
	EndEnteredAt  time.Time
	Successor     int
}

// Live reports whether the entry has not ended.
func (e Entry) Live() bool { return e.End == "" }

// MovementState is a stored movement, for persistence.
type MovementState struct {
	TenantID        TenantID
	MovementID      MovementID
	ConsolidationID string
	Kind            Kind
	LegalEntityID   string
	Carrier         Carrier
	Origin          kernel.LocationCode
	Destination     kernel.LocationCode
	Edition         string
	State           State
	Links           []Link
	Bills           []MasterBill
	Entries         []Entry
	DocFacts        []DocFactsVersion
	CarrierFacts    []CarrierFact
	CancelReason    string
	CancelledBy     string
	CancelledAt     time.Time
}

// Movement is the aggregate root (OceanMovement). Links, master bills and entries are only added; a master bill
// leaves CURRENT once and an entry ends once.
type Movement struct {
	state   MovementState
	links   int
	bills   int
	entries int
	docs    int
	facts   int
	retired []int // stored master bills that left CURRENT since the movement was loaded, by index
	ended   []int // stored entries ended since the movement was loaded, by index
	pending []Event
}

// Event carries identifiers only. Its subject is a movement, a legal entity (series) or a container visit, whose
// events also name their movement.
type Event struct {
	Type            string
	At              time.Time
	TenantID        TenantID
	SubjectID       string
	ConsolidationID string
	MovementID      string
	ShipmentID      string
	EntryNo         int
	BillNo          int
	SealNo          int
	AssertionNo     int
	EventNo         int
	FactNo          int
	FactsVersion    int
	BookingID       string
	CapacityVersion int
	Reason          string
}

const (
	eventOpened     = "OceanMovementOpened.v1"
	eventLinked     = "VoyageLegsLinked.v1"
	eventBill       = "MasterBillRecorded.v1"
	eventAssociated = "HouseBillAssociated.v1"
	eventRefreshed  = "HouseBillFactsRefreshed.v1"
	eventEnded      = "HouseBillAssociationEnded.v1"
	eventFinalized  = "OceanDocFactsFinalized.v1"
	eventSuperseded = "OceanDocFactsSuperseded.v1"
	eventCarrier    = "OceanCarrierFactRecorded.v1"
	eventCancelled  = "OceanMovementCancelled.v1"

	CarrierLoaded     = "LOADED_ON_BOARD"
	CarrierDeparted   = "DEPARTED"
	CarrierRolled     = "ROLLED"
	CarrierDischarged = "DISCHARGED"
)

// CarrierFact is one carrier report about the movement: a container loaded, the vessel departed, a roll or a
// discharge. It names a live container of the movement or its current master bill or a live house's HBL, never both.
// Recording it does not change the movement's state.
type CarrierFact struct {
	No              int
	Kind            string
	ContainerNumber string
	BillNumber      string
	Vessel          string
	Voyage          string
	Source          string
	Reference       string
	SHA256          string
	OccurredAt      time.Time
	By              string
	At              time.Time
	EnteredAt       time.Time
}

func carrierKind(kind string) bool {
	return kind == CarrierLoaded || kind == CarrierDeparted || kind == CarrierRolled || kind == CarrierDischarged
}

// OpenMovement opens the ocean movement of a consolidation from its booking: a sea LCL booking in force, allocated
// to the consolidation, with one capacity line. The movement takes its kind, the carrier or co-loader, the
// operating legal entity, the port route and the voyage legs from it.
func OpenMovement(tenant TenantID, id MovementID, consolidation string, b Booking, by string, at time.Time) (*Movement, error) {
	if !identifier(string(tenant)) || !identifier(string(id)) || !identifier(consolidation) || blank(by) || at.IsZero() {
		return nil, ErrInvalidMovement
	}
	if err := b.check(consolidation); err != nil {
		return nil, err
	}
	m := &Movement{state: MovementState{TenantID: tenant, MovementID: id, ConsolidationID: consolidation, Kind: b.Kind, LegalEntityID: b.LegalEntityID,
		Carrier: b.Carrier, Origin: b.Origin, Destination: b.Destination, Edition: b.Edition, State: StateOpen}}
	m.addLink(b, by, at)
	m.record(eventOpened, at, "", 0, 0, "")
	m.record(eventLinked, at, "", 0, 0, "")
	return m, nil
}

// check validates a booking for a consolidation.
func (b Booking) check(consolidation string) error {
	bad := func(what string) error { return fmt.Errorf("%w: %s", ErrInvalidBooking, what) }
	switch {
	case !b.Kind.valid() || !b.InForce:
		return bad("a sea LCL container or co-load booking with confirmed capacity in force")
	case b.ConsumerID != consolidation:
		return bad("allocated to another consolidation")
	case !identifier(b.BookingID) || b.Revision < 1 || b.CapacityVersion < 1 || !identifier(b.LegalEntityID) || !b.Carrier.valid(b.Kind):
		return bad("booking, capacity version, legal entity and the carrier in the role of the booking's kind")
	case b.Lines != 1:
		return bad("one capacity line; a split booking is not carried in V0.1")
	case !port(b.Origin) || !port(b.Destination) || b.Origin == b.Destination || !editionForm.MatchString(b.Edition):
		return bad("a resolved port route")
	}
	return checkLegs(b.Legs, b.Origin, b.Destination)
}

func checkLegs(legs []Leg, origin, destination kernel.LocationCode) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidBooking, fmt.Sprintf(format, args...))
	}
	if len(legs) == 0 || len(legs) > 9 {
		return bad("one to nine voyage legs")
	}
	for i, l := range legs {
		switch {
		case !short(l.Vessel, 35) || !voyageForm.MatchString(l.Voyage) || (l.VesselIMO != "" && !ValidIMO(l.VesselIMO)):
			return bad("leg %d vessel, voyage and IMO number", i+1)
		case !port(l.From) || !port(l.To) || l.From == l.To:
			return bad("leg %d ports", i+1)
		case i == 0 && l.From != origin, i == len(legs)-1 && l.To != destination:
			return bad("the legs run from the movement's port of loading to its port of discharge")
		case i > 0 && (l.From != legs[i-1].To || l.ETD.Before(legs[i-1].ETD)):
			return bad("leg %d does not follow leg %d", i+1, i)
		case l.ETD.IsZero() || (!l.ETA.IsZero() && !l.ETA.After(l.ETD)):
			return bad("leg %d estimated times", i+1)
		}
		kinds := map[string]bool{}
		for _, c := range l.CutOffs {
			if !c.valid() || kinds[c.Kind] {
				return bad("leg %d cut-off %s: a kind of SEA, once, with its instant and IANA zone", i+1, c.Kind)
			}
			kinds[c.Kind] = true
		}
	}
	return nil
}

// LinkBooking takes a newer capacity version of the movement's booking: same booking, kind, carrier, consolidation,
// route and legal entity; the voyage legs are replaced and the old ones kept. Houses and the master bill stay.
func (m *Movement) LinkBooking(b Booking, by string, at time.Time) error {
	if !m.open() {
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
	case b.Kind != m.state.Kind || b.Carrier.PartyID != m.state.Carrier.PartyID || b.Origin != m.state.Origin || b.Destination != m.state.Destination ||
		b.LegalEntityID != m.state.LegalEntityID:
		return fmt.Errorf("%w: another kind, carrier, route or legal entity is another movement", ErrInvalidBooking)
	}
	if err := m.changeable(by, at); err != nil {
		return err
	}
	m.reopen(at)
	m.addLink(b, by, at)
	m.record(eventLinked, at, "", 0, 0, "")
	return nil
}

func (m *Movement) addLink(b Booking, by string, at time.Time) {
	legs := make([]Leg, len(b.Legs))
	for i, l := range b.Legs {
		l.ETD, l.ETA = l.ETD.UTC(), utc(l.ETA)
		l.CutOffs = append([]CutOff(nil), l.CutOffs...)
		for j := range l.CutOffs {
			l.CutOffs[j].At = l.CutOffs[j].At.UTC()
		}
		legs[i] = l
	}
	m.state.Links = append(m.state.Links, Link{No: len(m.state.Links) + 1, BookingID: b.BookingID, BookingRevision: b.Revision,
		CapacityVersion: b.CapacityVersion, Legs: legs, By: by, At: at.UTC()})
}

// RecordMasterBill records the master bill reference the movement's carrier or co-loader states, with its evidence:
// a kind of the movement's kind and a number in its form. A further number or kind supersedes the current one.
func (m *Movement) RecordMasterBill(number, kind string, evidence Evidence, by string, at time.Time) error {
	if m.state.State != StateOpen {
		return ErrInvalidTransition
	}
	switch {
	case !ValidBillNumber(number):
		return fmt.Errorf("%w: a number of capital letters and digits with single hyphens or slashes inside, 35 characters at most", ErrInvalidMasterBill)
	case !fits(kind, m.state.Kind):
		return fmt.Errorf("%w: kind %q on a %s movement", ErrInvalidMasterBill, kind, m.state.Kind)
	case !evidence.valid() || evidence.CapturedAt.After(at):
		return fmt.Errorf("%w: evidence needs its SHA-256, storage id, media type and a capture time not after the business time", ErrInvalidMasterBill)
	}
	i, has := m.currentBill()
	if has && m.state.Bills[i].Number == number && m.state.Bills[i].Kind == kind {
		return fmt.Errorf("%w: %s is the current master bill reference", ErrInvalidChange, number)
	}
	if err := m.changeable(by, at); err != nil {
		return err
	}
	if has {
		m.retire(i, BillSuperseded)
	}
	evidence.CapturedAt = evidence.CapturedAt.UTC()
	no := len(m.state.Bills) + 1
	m.state.Bills = append(m.state.Bills, MasterBill{No: no, Number: number, Kind: kind, Evidence: evidence, Status: BillCurrent, By: by, At: at.UTC()})
	m.record(eventBill, at, "", 0, no, "")
	return nil
}

func (m *Movement) currentBill() (int, bool) {
	if n := len(m.state.Bills); n > 0 && m.state.Bills[n-1].Status == BillCurrent {
		return n - 1, true
	}
	return -1, false
}

func (m *Movement) retire(i int, status string) {
	m.state.Bills[i].Status = status
	if i < m.bills {
		m.retired = append(m.retired, i)
	}
}

// admits checks a house's plan against the movement.
func (m *Movement) admits(p Plan) error {
	switch {
	case !m.open():
		return ErrInvalidTransition
	case p.Status != "PLANNED" || p.Mode != "SEA" || p.ContainerMode == "FCL" || p.ConsolidationID != m.state.ConsolidationID ||
		p.LegalEntityID != m.state.LegalEntityID || p.Origin != m.state.Origin || p.Destination != m.state.Destination:
		return fmt.Errorf("%w: %s is %s %s %s in %q of %s, %s-%s", ErrNotAttached, p.ShipmentID, p.Status, p.Mode, p.ContainerMode, p.ConsolidationID,
			p.LegalEntityID, p.Origin.Code, p.Destination.Code)
	case !identifier(p.ShipmentID) || p.Revision < 1 || p.PlanVersion < 1 || p.Packs < 1 || len(p.Parts) == 0 ||
		(p.ContainerMode != "" && p.ContainerMode != "LCL"):
		return fmt.Errorf("%w: plan of %s", ErrInvalidChange, p.ShipmentID)
	}
	return nil
}

// facts are the quantities SEA freezes for a house: PHY's measures when FUL relays them, else FUL's plan; the parts
// stay as planned (PHY counts a house's packs, not its parts'); the sealed containers as FUL relays them.
func facts(p Plan) (Facts, error) {
	bad := func(what string) (Facts, error) {
		return Facts{}, fmt.Errorf("%w: %s of %s", ErrInvalidChange, what, p.ShipmentID)
	}
	f := Facts{Packs: p.Packs, GrossWeight: p.GrossWeight, Volume: p.Volume, Basis: BasisPlanned, Parts: append([]Part(nil), p.Parts...)}
	for i := range f.Parts {
		if f.Parts[i].Volume.Amount == 0 {
			f.Parts[i].Volume = kernel.Quantity{}
		}
		if !f.Parts[i].valid() {
			return bad("parts")
		}
	}
	if p.MeasuredBasis != "" {
		if !basisValid(p.MeasuredBasis) || p.MeasuredBasis == BasisPlanned || p.EvidenceVersion < 1 || p.MeasuredPacks < 1 {
			return bad("measures")
		}
		f.Packs, f.GrossWeight, f.Volume, f.Basis, f.Evidence = p.MeasuredPacks, p.MeasuredWeight, p.MeasuredVolume, p.MeasuredBasis, p.EvidenceVersion
	}
	if f.Volume.Amount == 0 {
		f.Volume = kernel.Quantity{}
	}
	if !measure(f.GrossWeight) || f.GrossWeight.Amount < 1 || !measure(f.Volume) {
		return bad("gross weight and volume")
	}
	executions := map[string]bool{}
	for _, s := range p.Stuffings {
		if p.EvidenceVersion < 1 || !s.valid() || executions[s.ExecutionID] {
			return bad("sealed containers")
		}
		f.Evidence = p.EvidenceVersion
		executions[s.ExecutionID] = true
		s.LineNos, s.SealedAt = append([]int(nil), s.LineNos...), s.SealedAt.UTC()
		if s.GrossWeight.Amount == 0 {
			s.GrossWeight = kernel.Quantity{}
		}
		if s.Volume.Amount == 0 {
			s.Volume = kernel.Quantity{}
		}
		f.Stuffings = append(f.Stuffings, s)
	}
	return f, nil
}

// sameMeasures reports whether two facts document the same quantities and the same sealed containers.
func sameMeasures(a, b Facts) bool {
	return a.Packs == b.Packs && a.GrossWeight == b.GrossWeight && a.Volume == b.Volume && a.Basis == b.Basis &&
		slices.EqualFunc(a.Stuffings, b.Stuffings, Stuffing.same)
}

// Associate associates a house that FUL reports attached to the consolidation, with its HBL number.
func (m *Movement) Associate(p Plan, hbl, by string, at time.Time) error {
	if m.state.State != StateOpen {
		return ErrInvalidTransition
	}
	if err := m.admits(p); err != nil {
		return err
	}
	if !ValidHBL(hbl) {
		return ErrInvalidHBL
	}
	for _, e := range m.state.Entries {
		if e.Live() && (e.ShipmentID == p.ShipmentID || e.HBL == hbl) {
			return fmt.Errorf("%w: %s / %s", ErrAlreadyAssociated, e.ShipmentID, e.HBL)
		}
	}
	f, err := facts(p)
	if err != nil {
		return err
	}
	if err = m.changeable(by, at); err != nil {
		return err
	}
	no := m.addEntry(p, hbl, f, StartAssociated, 0, by, at)
	m.record(eventAssociated, at, p.ShipmentID, no, 0, "")
	return nil
}

// Refresh takes FUL's newer plan version of a live house, or the measures and sealed containers FUL newly relays
// from PHY, into a new entry with the same HBL.
func (m *Movement) Refresh(p Plan, by string, at time.Time) error {
	i, err := m.live(p.ShipmentID)
	if err != nil {
		return err
	}
	if err = m.admits(p); err != nil {
		return err
	}
	f, err := facts(p)
	if err != nil {
		return err
	}
	if p.PlanVersion < m.state.Entries[i].PlanVersion || p.Revision < m.state.Entries[i].FULRevision {
		return fmt.Errorf("%w: FUL reports %s at plan version %d, revision %d, older than the entry's %d, %d", ErrInvalidChange, p.ShipmentID,
			p.PlanVersion, p.Revision, m.state.Entries[i].PlanVersion, m.state.Entries[i].FULRevision)
	}
	if p.PlanVersion == m.state.Entries[i].PlanVersion && sameMeasures(f, m.state.Entries[i].Facts) {
		return ErrNothingToRefresh
	}
	if err = m.changeable(by, at); err != nil {
		return err
	}
	m.reopen(at)
	old := m.state.Entries[i]
	m.end(i, EndRefreshed, len(m.state.Entries)+1, by, at) // entries are numbered across the movement
	no := m.addEntry(p, old.HBL, f, StartRefreshed, old.No, by, at)
	m.record(eventRefreshed, at, p.ShipmentID, no, 0, "")
	return nil
}

// EndAssociation ends a house's entry once FUL no longer reports the shipment attached to the consolidation:
// TRANSFERRED when attached to another, DETACHED when to none, SHIPMENT_CANCELLED when cancelled.
func (m *Movement) EndAssociation(p Plan, by string, at time.Time) error {
	if !m.open() {
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
	m.reopen(at)
	m.end(i, kind, 0, by, at)
	m.record(eventEnded, at, p.ShipmentID, m.state.Entries[i].No, 0, kind)
	return nil
}

// Cancel cancels an open movement without houses and voids its current master bill reference.
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
	if i, has := m.currentBill(); has {
		m.retire(i, BillVoided)
	}
	m.state.State = StateCancelled
	m.state.CancelReason, m.state.CancelledBy, m.state.CancelledAt = reason, by, at.UTC()
	m.record(eventCancelled, at, "", 0, 0, "")
	return nil
}

// open reports whether the movement takes a link, a refresh or an end: OPEN, or FINAL, which that change returns to OPEN.
func (m *Movement) open() bool { return m.state.State == StateOpen || m.state.State == StateFinal }

// reopen returns a FINAL movement to OPEN and says so. The published version stays as it was.
func (m *Movement) reopen(at time.Time) {
	if m.state.State != StateFinal {
		return
	}
	m.state.State = StateOpen
	m.record(eventSuperseded, at, "", 0, 0, "")
	m.pending[len(m.pending)-1].FactsVersion = len(m.state.DocFacts)
}

// Supersede returns a FINAL movement to OPEN because a new seal, a new VGM assertion, or the carrier's rejection
// changes what the published facts say. The published version stays. A movement that is not FINAL is left as it is.
func (m *Movement) Supersede(by string, at time.Time) error {
	if m.state.State != StateFinal {
		return nil
	}
	if err := m.changeable(by, at); err != nil {
		return err
	}
	m.reopen(at)
	return nil
}

// changesOnBoard reports whether a loading at occurred changes the on-board date the latest published facts state:
// they state none, or a later one. The on-board date is the earliest loading recorded.
func (m *Movement) changesOnBoard(occurred time.Time) bool {
	n := len(m.state.DocFacts)
	if n == 0 {
		return false
	}
	published := m.state.DocFacts[n-1].Facts.OnBoardAt
	return published.IsZero() || occurred.Before(published)
}

// LoadedOnBoard returns a FINAL movement to OPEN when a container's loading at occurred changes the on-board date
// its published facts state, as a new seal does. A loading that leaves the published date as it is, and a movement
// that is not FINAL, are left as they are.
func (m *Movement) LoadedOnBoard(occurred time.Time, by string, at time.Time) error {
	if m.state.State != StateFinal || !m.changesOnBoard(occurred) {
		return nil
	}
	if err := m.changeable(by, at); err != nil {
		return err
	}
	m.reopen(at)
	return nil
}

// RecordCarrierFact records a carrier's report. liveContainers are the movement's containers that are not released.
// The same source reference with the same content returns the fact already recorded and leaves the movement as it
// is; with other content it is refused. A LOADED_ON_BOARD report that changes the on-board date the published facts
// state returns a FINAL movement to OPEN. Any other report leaves the movement's state as it is.
func (m *Movement) RecordCarrierFact(f CarrierFact, liveContainers []string, by string, at time.Time) (int, bool, error) {
	if m.state.State != StateOpen && m.state.State != StateFinal {
		return 0, false, ErrInvalidTransition
	}
	f.OccurredAt = f.OccurredAt.UTC()
	container := f.ContainerNumber != ""
	bill := f.BillNumber != ""
	switch {
	case !carrierKind(f.Kind) || container == bill || (container && !ValidContainerNumber(f.ContainerNumber)) || (bill && !ValidBillNumber(f.BillNumber)):
		return 0, false, fmt.Errorf("%w: the fact names a container of the movement or its bill, and a kind", ErrInvalidChange)
	case !short(f.Vessel, 35) || !voyageForm.MatchString(f.Voyage) || !short(f.Source, 64) || !short(f.Reference, 128) || !hexForm.MatchString(f.SHA256):
		return 0, false, fmt.Errorf("%w: vessel, voyage, source reference and the message's SHA-256", ErrInvalidChange)
	case f.OccurredAt.IsZero() || f.OccurredAt.After(at):
		return 0, false, fmt.Errorf("%w: the fact is not recorded before it happened", ErrInvalidChange)
	}
	for _, o := range m.state.CarrierFacts {
		if o.Source == f.Source && o.Reference == f.Reference {
			if o.Kind == f.Kind && o.ContainerNumber == f.ContainerNumber && o.BillNumber == f.BillNumber && o.Vessel == f.Vessel &&
				o.Voyage == f.Voyage && o.SHA256 == f.SHA256 && o.OccurredAt.Equal(f.OccurredAt) {
				return o.No, false, nil
			}
			return 0, false, fmt.Errorf("%w: %s %s was recorded with other content", ErrInvalidChange, f.Source, f.Reference)
		}
	}
	if container {
		found := false
		for _, n := range liveContainers {
			if n == f.ContainerNumber {
				found = true
			}
		}
		if !found {
			return 0, false, fmt.Errorf("%w: %s is not a live container of the movement", ErrInvalidChange, f.ContainerNumber)
		}
	} else if !m.namesBill(f.BillNumber) {
		return 0, false, fmt.Errorf("%w: %s is not the current master bill or a live house bill", ErrInvalidChange, f.BillNumber)
	}
	if err := m.changeable(by, at); err != nil {
		return 0, false, err
	}
	if f.Kind == CarrierLoaded && m.state.State == StateFinal && m.changesOnBoard(f.OccurredAt) {
		m.reopen(at)
	}
	f.No, f.By, f.At = len(m.state.CarrierFacts)+1, by, at.UTC()
	m.state.CarrierFacts = append(m.state.CarrierFacts, f)
	m.record(eventCarrier, at, "", 0, 0, f.Kind)
	m.pending[len(m.pending)-1].FactNo = f.No
	return f.No, true, nil
}

// namesBill reports whether number is the current master bill or a live house's HBL.
func (m *Movement) namesBill(number string) bool {
	if b, ok := m.CurrentBill(); ok && b.Number == number {
		return true
	}
	for _, e := range m.state.Entries {
		if e.Live() && e.HBL == number {
			return true
		}
	}
	return false
}

func (m *Movement) live(shipment string) (int, error) {
	for i, e := range m.state.Entries {
		if e.Live() && e.ShipmentID == shipment {
			return i, nil
		}
	}
	return -1, fmt.Errorf("%w: %s", ErrNotAssociated, shipment)
}

func (m *Movement) addEntry(p Plan, hbl string, f Facts, start string, predecessor int, by string, at time.Time) int {
	no := len(m.state.Entries) + 1
	m.state.Entries = append(m.state.Entries, Entry{No: no, ShipmentID: p.ShipmentID, HBL: hbl, FULRevision: p.Revision, PlanVersion: p.PlanVersion,
		ContainerMode: p.ContainerMode, Window: kernel.ValidityInterval{From: p.Window.From.UTC(), Until: utc(p.Window.Until)}, Facts: f, Start: start,
		Predecessor: predecessor, By: by, At: at.UTC()})
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

// VGMCutOff is the VGM cut-off of the current voyage's first leg, when the booking recorded one.
func (m *Movement) VGMCutOff() (time.Time, bool) {
	if m == nil || len(m.Current().Legs) == 0 {
		return time.Time{}, false
	}
	for _, c := range m.Current().Legs[0].CutOffs {
		if c.Kind == "VGM" {
			return c.At, true
		}
	}
	return time.Time{}, false
}

// openedAt is the business time the movement was opened: its first link's.
func (m *Movement) openedAt() time.Time {
	if len(m.state.Links) == 0 {
		return time.Time{}
	}
	return m.state.Links[0].At
}

func (m *Movement) lastChange() time.Time {
	last := m.state.CancelledAt
	for _, l := range m.state.Links {
		last = later(last, l.At)
	}
	for _, b := range m.state.Bills {
		last = later(last, b.At)
	}
	for _, e := range m.state.Entries {
		last = later(later(last, e.At), e.EndedAt)
	}
	for _, v := range m.state.DocFacts {
		last = later(last, v.At)
	}
	for _, f := range m.state.CarrierFacts {
		last = later(last, f.At)
	}
	return last
}

func (m *Movement) record(kind string, at time.Time, shipment string, entry, bill int, reason string) {
	current := m.Current()
	m.pending = append(m.pending, Event{Type: kind, At: at.UTC(), TenantID: m.state.TenantID, SubjectID: string(m.state.MovementID),
		ConsolidationID: m.state.ConsolidationID, ShipmentID: shipment, EntryNo: entry, BillNo: bill, BookingID: current.BookingID,
		CapacityVersion: current.CapacityVersion, Reason: reason})
}

// Current returns the current booking link.
func (m *Movement) Current() Link {
	if len(m.state.Links) == 0 {
		return Link{}
	}
	return m.state.Links[len(m.state.Links)-1]
}

// CurrentBill returns the current master bill reference, if one is recorded and the movement is not cancelled.
func (m *Movement) CurrentBill() (MasterBill, bool) {
	if i, has := m.currentBill(); has {
		return m.state.Bills[i], true
	}
	return MasterBill{}, false
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

// Totals returns the packs of the live houses and, when every live house's weight is in a unit SEA converts, their
// gross weight in grams; likewise their volume in cubic centimetres, houses without a volume adding none.
func (m *Movement) Totals() (packs, grams, cm3 int64, weighed, measured bool) {
	houses := m.Houses()
	weighed, measured = len(houses) > 0, len(houses) > 0
	for _, e := range houses {
		packs += e.Facts.Packs
		g, ok := Grams(e.Facts.GrossWeight)
		weighed = weighed && ok && g <= maxTotal-grams
		if weighed {
			grams += g
		}
		if e.Facts.Volume == (kernel.Quantity{}) {
			continue
		}
		v, ok := CubicCentimetres(e.Facts.Volume)
		measured = measured && ok && v <= maxTotal-cm3
		if measured {
			cm3 += v
		}
	}
	if !weighed {
		grams = 0
	}
	if !measured {
		cm3 = 0
	}
	return packs, grams, cm3, weighed, measured
}

const maxTotal = int64(1) << 62

// WindowDrift reports whether a house's departure window no longer contains the current first leg's ETD.
func (m *Movement) WindowDrift(e Entry) bool {
	legs := m.Current().Legs
	return len(legs) > 0 && !e.Window.Contains(legs[0].ETD)
}

// Stamp records when SEA entered what was added or ended since the movement was loaded (T1).
func (m *Movement) Stamp(entered time.Time) {
	for i := m.links; i < len(m.state.Links); i++ {
		m.state.Links[i].EnteredAt = entered.UTC()
	}
	for i := m.bills; i < len(m.state.Bills); i++ {
		m.state.Bills[i].EnteredAt = entered.UTC()
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
	for i := m.docs; i < len(m.state.DocFacts); i++ {
		m.state.DocFacts[i].EnteredAt = entered.UTC()
	}
	for i := m.facts; i < len(m.state.CarrierFacts); i++ {
		m.state.CarrierFacts[i].EnteredAt = entered.UTC()
	}
}

// RestoreMovement rebuilds a stored movement without emitting events.
func RestoreMovement(st MovementState) (*Movement, error) {
	st.Links = append([]Link(nil), st.Links...)
	st.Bills = append([]MasterBill(nil), st.Bills...)
	st.Entries = append([]Entry(nil), st.Entries...)
	st.DocFacts = append([]DocFactsVersion(nil), st.DocFacts...)
	st.CarrierFacts = append([]CarrierFact(nil), st.CarrierFacts...)
	m := &Movement{state: st, links: len(st.Links), bills: len(st.Bills), entries: len(st.Entries), docs: len(st.DocFacts), facts: len(st.CarrierFacts)}
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
		!st.Kind.valid() || !st.Carrier.valid(st.Kind) || !port(st.Origin) || !port(st.Destination) || st.Origin == st.Destination:
		return fmt.Errorf("identity")
	case st.State != StateOpen && st.State != StateFinal && !cancelled:
		return fmt.Errorf("state %s", st.State)
	case cancelled != text(st.CancelReason) || cancelled == blank(st.CancelledBy):
		return fmt.Errorf("cancellation")
	case len(st.Links) == 0:
		return fmt.Errorf("no booking link")
	}
	for i, l := range st.Links {
		if l.No != i+1 || l.BookingID != st.Links[0].BookingID || (i > 0 && l.CapacityVersion <= st.Links[i-1].CapacityVersion) ||
			checkLegs(l.Legs, st.Origin, st.Destination) != nil {
			return fmt.Errorf("link %d", i+1)
		}
	}
	for i, b := range st.Bills {
		status := BillSuperseded
		if i == len(st.Bills)-1 {
			status = BillCurrent
			if cancelled {
				status = BillVoided
			}
		}
		if b.No != i+1 || !ValidBillNumber(b.Number) || !fits(b.Kind, st.Kind) || !b.Evidence.valid() || b.Status != status || blank(b.By) || b.At.IsZero() {
			return fmt.Errorf("master bill %d", i+1)
		}
	}
	liveShipments, liveHBLs := map[string]bool{}, map[string]bool{}
	for i, e := range st.Entries {
		f := e.Facts
		if e.No != i+1 || !identifier(e.ShipmentID) || !ValidHBL(e.HBL) || e.FULRevision < 1 || e.PlanVersion < 1 || f.Packs < 1 || !measure(f.GrossWeight) ||
			f.GrossWeight.Amount < 1 || !measure(f.Volume) || blank(e.By) || e.At.IsZero() || !basisValid(f.Basis) || f.Evidence < 0 ||
			(f.Basis != BasisPlanned && f.Evidence < 1) || (e.ContainerMode != "" && e.ContainerMode != "LCL") || len(f.Parts) == 0 {
			return fmt.Errorf("entry %d", i+1)
		}
		for _, p := range f.Parts {
			if !p.valid() {
				return fmt.Errorf("entry %d part", i+1)
			}
		}
		executions := map[string]bool{}
		for _, s := range f.Stuffings {
			if f.Evidence < 1 || !s.valid() || executions[s.ExecutionID] {
				return fmt.Errorf("entry %d container", i+1)
			}
			executions[s.ExecutionID] = true
		}
		switch e.Start {
		case StartAssociated:
			if e.Predecessor != 0 {
				return fmt.Errorf("entry %d predecessor", i+1)
			}
		case StartRefreshed:
			if e.Predecessor < 1 || e.Predecessor > i || st.Entries[e.Predecessor-1].Successor != e.No || st.Entries[e.Predecessor-1].HBL != e.HBL ||
				st.Entries[e.Predecessor-1].ShipmentID != e.ShipmentID {
				return fmt.Errorf("entry %d predecessor", i+1)
			}
		default:
			return fmt.Errorf("entry %d start", i+1)
		}
		if e.Live() {
			if liveShipments[e.ShipmentID] || liveHBLs[e.HBL] || cancelled || e.Successor != 0 {
				return fmt.Errorf("entry %d live", i+1)
			}
			liveShipments[e.ShipmentID], liveHBLs[e.HBL] = true, true
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
	if err := m.readDocFacts(); err != nil {
		return err
	}
	for i, f := range st.CarrierFacts {
		container := f.ContainerNumber != ""
		bill := f.BillNumber != ""
		if f.No != i+1 || !carrierKind(f.Kind) || container == bill || (container && !ValidContainerNumber(f.ContainerNumber)) ||
			(bill && !ValidBillNumber(f.BillNumber)) || !short(f.Vessel, 35) || !voyageForm.MatchString(f.Voyage) ||
			!short(f.Source, 64) || !short(f.Reference, 128) || !hexForm.MatchString(f.SHA256) || f.OccurredAt.IsZero() ||
			f.OccurredAt.After(f.At) || blank(f.By) || f.At.IsZero() {
			return fmt.Errorf("carrier fact %d", i+1)
		}
	}
	return nil
}

// State returns a copy of the movement for persistence and read models.
func (m *Movement) State() MovementState {
	st := m.state
	st.Links = append([]Link(nil), st.Links...)
	st.Bills = append([]MasterBill(nil), st.Bills...)
	st.Entries = append([]Entry(nil), st.Entries...)
	st.DocFacts = append([]DocFactsVersion(nil), st.DocFacts...)
	st.CarrierFacts = append([]CarrierFact(nil), st.CarrierFacts...)
	for i := range st.DocFacts {
		st.DocFacts[i].Content = append([]byte(nil), m.state.DocFacts[i].Content...)
	}
	return st
}

func (m *Movement) ID() MovementID   { return m.state.MovementID }
func (m *Movement) Pending() []Event { return append([]Event(nil), m.pending...) }

// Added returns the links, master bills and entries written since the movement was loaded.
func (m *Movement) Added() ([]Link, []MasterBill, []Entry) {
	return m.state.Links[m.links:], m.state.Bills[m.bills:], m.state.Entries[m.entries:]
}

// AddedCarrierFacts returns the carrier facts recorded since the movement was loaded.
func (m *Movement) AddedCarrierFacts() []CarrierFact { return m.state.CarrierFacts[m.facts:] }

// Changed returns the stored master bills that left CURRENT and the stored entries ended since the movement was
// loaded, each by number.
func (m *Movement) Changed() (map[int]MasterBill, map[int]Entry) {
	bills, entries := map[int]MasterBill{}, map[int]Entry{}
	for _, i := range m.retired {
		bills[i+1] = m.state.Bills[i]
	}
	for _, i := range m.ended {
		entries[i+1] = m.state.Entries[i]
	}
	return bills, entries
}

// Acknowledge is permitted only after the transaction holding the movement and its Outbox events commits.
func (m *Movement) Acknowledge() {
	m.pending, m.retired, m.ended = nil, nil, nil
	m.links, m.bills, m.entries, m.docs, m.facts = len(m.state.Links), len(m.state.Bills), len(m.state.Entries), len(m.state.DocFacts), len(m.state.CarrierFacts)
}
