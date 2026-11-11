package domain

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// PHY V0.2: receipt of a house's cargo into a cargo lot, its measurement and screening, and the differences
// against FUL's plan. Received and measured quantities are facts with their source; plans never create them and
// they never change a plan (C4): a difference is a discrepancy that FUL resolves by changing parts (F1).

type CargoLotID string

// LotState is a cargo lot's state.
type LotState string

const (
	LotReceiving LotState = "RECEIVING"
	LotReceived  LotState = "RECEIVED"
	LotClosed    LotState = "CLOSED"
)

var (
	ErrInvalidLot       = errors.New("phy: invalid cargo lot")
	ErrLotClosed        = errors.New("phy: the cargo lot is closed; its packs stay as loaded")
	ErrLotInForce       = errors.New("phy: the shipment has a cargo lot in force at the facility")
	ErrInvalidEvidence  = errors.New("phy: a physical fact needs its evidence: source, reference and time, with a SHA-256 of the raw payload where there is one")
	ErrInvalidReceipt   = errors.New("phy: invalid receipt, measurement or screening")
	ErrNoMeasurement    = errors.New("phy: the lot has no measurement")
	ErrOpenDiscrepancy  = errors.New("phy: the lot has an open discrepancy")
	ErrUnaccounted      = errors.New("phy: received packs are neither loaded nor accounted for by FUL's plan")
	ErrScreeningForAir  = errors.New("phy: screening results are recorded for AIR cargo only")
	ErrNotResolvable    = errors.New("phy: the discrepancy cannot be resolved so")
	ErrLiveLoadLines    = errors.New("phy: the house has live load lines in the consolidation; unload them first")
	ErrConservation     = errors.New("phy: CONSERVATION_VIOLATION: loaded packs would exceed received packs or the plan")
	ErrExecutionsClosed = errors.New("phy: a load execution of the consolidation is closed; it cannot reopen")
	ErrNotReconciled    = errors.New("phy: the consolidation's houses are not reconciled")
)

// Facility is a place cargo is received or tendered at: the UN/LOCODE of the place and the facility's code
// there (the forwarder's warehouse, a CFS, an airline terminal). PHY has no relationship to ORG, so it is a
// reference, not a party.
type Facility struct {
	Location kernel.LocationCode
	Code     string
}

var (
	facilityForm = regexp.MustCompile(`^[0-9A-Z][0-9A-Z./-]{0,34}$`)
	hexForm      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	codeForm     = regexp.MustCompile(`^[0-9A-Z][0-9A-Z_-]{0,34}$`)
)

func (f Facility) valid() bool {
	return f.Location.Scheme == kernel.UNLOCODE && f.Location.Valid() && facilityForm.MatchString(f.Code)
}

// Evidence is the external evidence of a physical fact: the source (a scan device, a CFS tally, the airline
// terminal, the airport security station), its reference, when it occurred, and the SHA-256 of the raw payload
// where there is one (EvidenceRef).
type Evidence struct {
	Source     string
	Reference  string
	SHA256     string
	OccurredAt time.Time
}

func (e Evidence) valid(at time.Time) bool {
	return text(e.Source) && len([]rune(e.Source)) <= 64 && text(e.Reference) && len([]rune(e.Reference)) <= 128 &&
		(e.SHA256 == "" || hexForm.MatchString(e.SHA256)) && !e.OccurredAt.IsZero() && !e.OccurredAt.After(at)
}

// Condition of received packs.
type Condition string

const (
	ConditionGood    Condition = "GOOD"
	ConditionDamaged Condition = "DAMAGED"
	ConditionWet     Condition = "WET"
	ConditionOpened  Condition = "OPENED"
)

func (c Condition) valid() bool {
	return c == ConditionGood || c == ConditionDamaged || c == ConditionWet || c == ConditionOpened
}

// ReceiptLine is one immutable receipt of packs into the lot.
type ReceiptLine struct {
	No          int
	Packs       int64
	PackageType string
	Condition   Condition
	Evidence    Evidence
	By          string
	At          time.Time
	EnteredAt   time.Time
}

// Method of a measurement.
type Method string

const (
	MethodScale          Method = "SCALE"
	MethodDimensioner    Method = "DIMENSIONER"
	MethodCFSTally       Method = "CFS_TALLY"
	MethodTerminalWeight Method = "TERMINAL_WEIGHT"
)

func (m Method) valid() bool {
	return m == MethodScale || m == MethodDimensioner || m == MethodCFSTally || m == MethodTerminalWeight
}

// PackageGroup is a count of packages of one type and size, in millimetres.
type PackageGroup struct {
	Count       int64
	PackageType string
	LengthMM    int64
	WidthMM     int64
	HeightMM    int64
}

// Measurement is one immutable measurement version of the whole lot; a later measurement adds a version.
type Measurement struct {
	No          int
	GrossWeight kernel.Quantity
	Volume      kernel.Quantity
	Groups      []PackageGroup
	Method      Method
	Evidence    Evidence
	By          string
	At          time.Time
	EnteredAt   time.Time
}

// Outcome of a screening.
type Outcome string

const (
	ScreeningPassed      Outcome = "PASSED"
	ScreeningFailed      Outcome = "FAILED"
	ScreeningPending     Outcome = "PENDING"
	ScreeningNotRequired Outcome = "NOT_REQUIRED"
)

func (o Outcome) valid() bool {
	return o == ScreeningPassed || o == ScreeningFailed || o == ScreeningPending || o == ScreeningNotRequired
}

// Cleared reports whether the outcome lets AIR cargo be tendered.
func (o Outcome) Cleared() bool { return o == ScreeningPassed || o == ScreeningNotRequired }

// Screening is an immutable screening result: external evidence of the airport security station or another
// screening party (CN-7); AIR never marks cargo secure on its own.
type Screening struct {
	No        int
	Outcome   Outcome
	Screener  string // the screening party's reference
	Method    string
	Reference string
	Evidence  Evidence
	By        string
	At        time.Time
	EnteredAt time.Time
}

// DiscrepancyKind is a kind of difference against the plan.
type DiscrepancyKind string

const (
	DiscrepancyShort          DiscrepancyKind = "SHORT"
	DiscrepancyOver           DiscrepancyKind = "OVER"
	DiscrepancyDamaged        DiscrepancyKind = "DAMAGED"
	DiscrepancyWeightVariance DiscrepancyKind = "WEIGHT_VARIANCE"
	DiscrepancyVolumeVariance DiscrepancyKind = "VOLUME_VARIANCE"
)

// Resolution of a discrepancy.
type Resolution string

const (
	ResolvedByReplan   Resolution = "FUL_REPLANNED" // FUL changed the parts so that the plan equals what was received
	ResolvedByAccepted Resolution = "ACCEPTED"      // accepted by the customer service owner, with a reason
	ResolvedByClaim    Resolution = "CLAIM_OPENED"  // a claim, with its reference
)

// Variance tolerances above which measured weight and volume differ from the plan (PHY V0.2 defaults).
const (
	WeightTolerancePercent = 5
	VolumeTolerancePercent = 10
)

// Discrepancy is a difference against the plan found at receipt; only its resolution is written later, once.
type Discrepancy struct {
	No         int
	Kind       DiscrepancyKind
	Expected   int64
	Observed   int64
	Unit       string // PCS for packs, else the quantity's unit
	RaisedAt   time.Time
	Resolution Resolution
	Reference  string
	Text       string
	ResolvedBy string
	ResolvedAt time.Time
	EnteredAt  time.Time // when PHY recorded the discrepancy
	// ResolutionEnteredAt is when PHY recorded the resolution.
	ResolutionEnteredAt time.Time
}

// Open reports whether the discrepancy is unresolved.
func (d Discrepancy) Open() bool { return d.Resolution == "" }

// Expected is what FUL planned for the shipment when PHY last compared (LoadIntent v1).
type Expected struct {
	Revision    int64
	PlanVersion int
	Scope       CargoScope
}

// CargoLotState is a stored cargo lot, for persistence.
type CargoLotState struct {
	TenantID           TenantID
	LotID              CargoLotID
	ShipmentID         ShipmentID
	Facility           Facility
	Mode               Mode
	Expected           Expected
	State              LotState
	Lines              []ReceiptLine
	Measurements       []Measurement
	Screenings         []Screening
	Discrepancies      []Discrepancy
	CompletedBy        string
	CompletedAt        time.Time
	CompletedEnteredAt time.Time
	ClosedBy           string
	ClosedAt           time.Time
	ClosedEnteredAt    time.Time
}

// CargoLot is the aggregate root of a house's cargo at a facility: one lot per shipment and facility in force.
type CargoLot struct {
	state    CargoLotState
	stored   [4]int // lines, measurements, screenings, discrepancies persisted
	resolved []int  // persisted discrepancies resolved since, by index
	pending  []Event
}

// ReceiveCargo opens a lot for a shipment FUL reports PLANNED, with its first receipt line. Cargo may arrive
// before its house is attached to a consolidation.
func ReceiveCargo(tenant TenantID, id CargoLotID, facility Facility, i LoadIntent, line ReceiptLine, by string, at time.Time) (*CargoLot, error) {
	if !identifier(string(tenant)) || !identifier(string(id)) || !facility.valid() || !i.Mode.Valid() || !identifier(string(i.ShipmentID)) {
		return nil, ErrInvalidLot
	}
	if i.Status != IntentPlanned || !i.Scope.valid() {
		return nil, fmt.Errorf("%w: %s", ErrIntentNotPlanned, i.ShipmentID)
	}
	l := &CargoLot{state: CargoLotState{TenantID: tenant, LotID: id, ShipmentID: i.ShipmentID, Facility: facility, Mode: i.Mode, State: LotReceiving,
		Expected: Expected{Revision: i.Revision, PlanVersion: i.PlanVersion, Scope: i.Scope}}}
	if err := l.Receive(line, by, at); err != nil {
		return nil, err
	}
	return l, nil
}

// Receive adds a receipt line while the lot is receiving.
func (l *CargoLot) Receive(line ReceiptLine, by string, at time.Time) error {
	if l.state.State != LotReceiving {
		return ErrInvalidTransition
	}
	if line.Packs < 1 || !packageType(line.PackageType) || !line.Condition.valid() {
		return fmt.Errorf("%w: packs, package type and condition", ErrInvalidReceipt)
	}
	if !line.Evidence.valid(at) {
		return ErrInvalidEvidence
	}
	if err := l.changeable(by, at); err != nil {
		return err
	}
	line.No, line.By, line.At, line.EnteredAt = len(l.state.Lines)+1, by, at.UTC(), time.Time{}
	line.Evidence.OccurredAt = line.Evidence.OccurredAt.UTC()
	l.state.Lines = append(l.state.Lines, line)
	l.record(eventCargoReceived, at, line.No)
	return nil
}

// Measure adds a measurement version of the whole lot, in the plan's weight unit and, where the plan declares a
// volume, its volume unit.
func (l *CargoLot) Measure(m Measurement, by string, at time.Time) error {
	if l.state.State == LotClosed {
		return ErrInvalidTransition
	}
	expected := l.state.Expected.Scope
	switch {
	case m.GrossWeight.Unit != expected.GrossWeight.Unit || m.GrossWeight.Amount < 1:
		return fmt.Errorf("%w: gross weight in %s", ErrInvalidReceipt, expected.GrossWeight.Unit)
	case m.Volume != (kernel.Quantity{}) && (!m.Volume.Unit.Valid() || m.Volume.Amount < 1 ||
		(expected.Volume.Unit != "" && m.Volume.Unit != expected.Volume.Unit)):
		return fmt.Errorf("%w: volume in the plan's unit", ErrInvalidReceipt)
	case !m.Method.valid():
		return fmt.Errorf("%w: method", ErrInvalidReceipt)
	case !m.Evidence.valid(at):
		return ErrInvalidEvidence
	}
	for _, g := range m.Groups {
		if g.Count < 1 || !packageType(g.PackageType) || g.LengthMM < 1 || g.WidthMM < 1 || g.HeightMM < 1 {
			return fmt.Errorf("%w: package group", ErrInvalidReceipt)
		}
	}
	if err := l.changeable(by, at); err != nil {
		return err
	}
	m.No, m.By, m.At, m.EnteredAt, m.Groups = len(l.state.Measurements)+1, by, at.UTC(), time.Time{}, append([]PackageGroup(nil), m.Groups...)
	m.Evidence.OccurredAt = m.Evidence.OccurredAt.UTC()
	l.state.Measurements = append(l.state.Measurements, m)
	l.record(eventCargoMeasured, at, m.No)
	return nil
}

// Screen records a screening result of AIR cargo, as external evidence.
func (l *CargoLot) Screen(s Screening, by string, at time.Time) error {
	if l.state.State == LotClosed {
		return ErrInvalidTransition
	}
	if l.state.Mode != ModeAir {
		return ErrScreeningForAir
	}
	if !s.Outcome.valid() || !text(s.Screener) || !codeForm.MatchString(s.Method) || !text(s.Reference) {
		return fmt.Errorf("%w: outcome, screener, method code and reference", ErrInvalidReceipt)
	}
	if !s.Evidence.valid(at) {
		return ErrInvalidEvidence
	}
	if err := l.changeable(by, at); err != nil {
		return err
	}
	s.No, s.By, s.At, s.EnteredAt = len(l.state.Screenings)+1, by, at.UTC(), time.Time{}
	s.Evidence.OccurredAt = s.Evidence.OccurredAt.UTC()
	l.state.Screenings = append(l.state.Screenings, s)
	l.record(eventScreeningResultRecorded, at, s.No)
	return nil
}

// Complete ends receiving against FUL's current plan: differences are recorded as discrepancies, never as a
// change of the plan (C4).
func (l *CargoLot) Complete(i LoadIntent, by string, at time.Time) error {
	if l.state.State != LotReceiving {
		return ErrInvalidTransition
	}
	m, ok := l.Measured()
	if !ok {
		return ErrNoMeasurement
	}
	if i.ShipmentID != l.state.ShipmentID || i.Status != IntentPlanned || !i.Scope.valid() || i.Scope.GrossWeight.Unit != m.GrossWeight.Unit {
		return fmt.Errorf("%w: %s", ErrIntentNotPlanned, i.ShipmentID)
	}
	if err := l.changeable(by, at); err != nil {
		return err
	}
	l.state.Expected = Expected{Revision: i.Revision, PlanVersion: i.PlanVersion, Scope: i.Scope}
	raise := func(kind DiscrepancyKind, expected, observed int64, unit string) {
		l.state.Discrepancies = append(l.state.Discrepancies, Discrepancy{No: len(l.state.Discrepancies) + 1, Kind: kind, Expected: expected,
			Observed: observed, Unit: unit, RaisedAt: at.UTC()})
		l.record(eventCargoDiscrepancyRecorded, at, len(l.state.Discrepancies))
	}
	received, damaged := l.Received(), int64(0)
	for _, line := range l.state.Lines {
		if line.Condition != ConditionGood {
			damaged += line.Packs
		}
	}
	switch {
	case received < i.Scope.Packs:
		raise(DiscrepancyShort, i.Scope.Packs, received, "PCS")
	case received > i.Scope.Packs:
		raise(DiscrepancyOver, i.Scope.Packs, received, "PCS")
	}
	if damaged > 0 {
		raise(DiscrepancyDamaged, 0, damaged, "PCS")
	}
	if beyond(m.GrossWeight.Amount, i.Scope.GrossWeight.Amount, WeightTolerancePercent) {
		raise(DiscrepancyWeightVariance, i.Scope.GrossWeight.Amount, m.GrossWeight.Amount, string(m.GrossWeight.Unit))
	}
	if m.Volume != (kernel.Quantity{}) && i.Scope.Volume.Unit == m.Volume.Unit && beyond(m.Volume.Amount, i.Scope.Volume.Amount, VolumeTolerancePercent) {
		raise(DiscrepancyVolumeVariance, i.Scope.Volume.Amount, m.Volume.Amount, string(m.Volume.Unit))
	}
	l.state.State, l.state.CompletedBy, l.state.CompletedAt = LotReceived, by, at.UTC()
	l.record(eventCargoReceiptCompleted, at, 0)
	return nil
}

func beyond(observed, expected int64, percent int64) bool {
	diff := observed - expected
	if diff < 0 {
		diff = -diff
	}
	return diff*100 > expected*percent
}

// Resolve resolves an open discrepancy: FUL_REPLANNED when FUL's current plan now equals what was received (for
// a short or over receipt), ACCEPTED with a reason, or CLAIM_OPENED with the claim's reference.
func (l *CargoLot) Resolve(no int, r Resolution, reference, reason string, i LoadIntent, by string, at time.Time) error {
	if no < 1 || no > len(l.state.Discrepancies) || !l.state.Discrepancies[no-1].Open() || l.state.State == LotClosed {
		return fmt.Errorf("%w: discrepancy %d", ErrNotResolvable, no)
	}
	d := &l.state.Discrepancies[no-1]
	switch r {
	case ResolvedByReplan:
		if (d.Kind != DiscrepancyShort && d.Kind != DiscrepancyOver) || i.ShipmentID != l.state.ShipmentID || i.Status != IntentPlanned ||
			i.Scope.Packs != l.Received() {
			return fmt.Errorf("%w: FUL's plan does not equal the %d packs received", ErrNotResolvable, l.Received())
		}
	case ResolvedByAccepted:
		if !text(reason) {
			return fmt.Errorf("%w: a reason", ErrNotResolvable)
		}
	case ResolvedByClaim:
		if !text(reference) {
			return fmt.Errorf("%w: the claim's reference", ErrNotResolvable)
		}
	default:
		return fmt.Errorf("%w: %q", ErrNotResolvable, r)
	}
	if !optionalText(reference) || !optionalText(reason) {
		return fmt.Errorf("%w: a reference and a reason of at most %d characters", ErrNotResolvable, maxText)
	}
	if err := l.changeable(by, at); err != nil {
		return err
	}
	d.Resolution, d.Reference, d.Text, d.ResolvedBy, d.ResolvedAt = r, reference, reason, by, at.UTC()
	if no-1 < l.stored[3] {
		l.resolved = append(l.resolved, no-1)
	}
	l.record(eventCargoDiscrepancyResolved, at, no)
	return nil
}

// Close closes a received lot without open discrepancies once every received pack is loaded, or the packs not
// loaded are accounted for by FUL: the shipment's plan equals what was loaded of it (the rest went to another
// shipment, F1), or the shipment is cancelled and nothing of it is loaded. A cancelled shipment's lot closes so
// while still receiving, too (the cargo goes back to the shipper). loaded counts this lot's live load lines;
// shipmentLoaded the shipment's in its consolidation.
func (l *CargoLot) Close(loaded, shipmentLoaded int64, i LoadIntent, by string, at time.Time) error {
	if i.ShipmentID != l.state.ShipmentID {
		return ErrInvalidIntent
	}
	cancelled := i.Status == IntentCancelled && shipmentLoaded == 0
	if l.state.State != LotReceived && !(l.state.State == LotReceiving && cancelled && loaded == 0) {
		return ErrInvalidTransition
	}
	if l.OpenDiscrepancies() > 0 {
		return ErrOpenDiscrepancy
	}
	accounted := (i.Status == IntentPlanned && shipmentLoaded == i.Scope.Packs) || cancelled
	if received := l.Received(); loaded != received && !(loaded < received && accounted) {
		return fmt.Errorf("%w: %d received, %d loaded; FUL plans %d of the shipment (%s), %d loaded", ErrUnaccounted, received, loaded,
			i.Scope.Packs, i.Status, shipmentLoaded)
	}
	if err := l.changeable(by, at); err != nil {
		return err
	}
	l.state.State, l.state.ClosedBy, l.state.ClosedAt = LotClosed, by, at.UTC()
	l.record(eventCargoLotClosed, at, 0)
	return nil
}

// Received returns the packs received.
func (l *CargoLot) Received() int64 {
	var n int64
	for _, line := range l.state.Lines {
		n += line.Packs
	}
	return n
}

// Measured returns the latest measurement version.
func (l *CargoLot) Measured() (Measurement, bool) {
	if len(l.state.Measurements) == 0 {
		return Measurement{}, false
	}
	return l.state.Measurements[len(l.state.Measurements)-1], true
}

// Screened returns the latest screening outcome; empty when none was recorded.
func (l *CargoLot) Screened() Outcome {
	if len(l.state.Screenings) == 0 {
		return ""
	}
	return l.state.Screenings[len(l.state.Screenings)-1].Outcome
}

func (l *CargoLot) changeable(by string, at time.Time) error {
	if blank(by) || at.IsZero() || at.Before(l.lastChange()) {
		return fmt.Errorf("%w: author and business time not before the lot's last change", ErrInvalidChange)
	}
	return nil
}

func (l *CargoLot) lastChange() time.Time {
	last := later(l.state.CompletedAt, l.state.ClosedAt)
	for _, x := range l.state.Lines {
		last = later(last, x.At)
	}
	for _, x := range l.state.Measurements {
		last = later(last, x.At)
	}
	for _, x := range l.state.Screenings {
		last = later(last, x.At)
	}
	for _, x := range l.state.Discrepancies {
		last = later(later(last, x.RaisedAt), x.ResolvedAt)
	}
	return last
}

func (l *CargoLot) record(kind string, at time.Time, item int) {
	l.pending = append(l.pending, Event{Type: kind, At: at.UTC(), TenantID: l.state.TenantID, SubjectID: string(l.state.LotID),
		ShipmentID: l.state.ShipmentID, LotID: l.state.LotID, ItemNo: item})
}

// Cargo lot event types, as package api publishes them.
const (
	eventCargoReceived            = "CargoReceived.v1"
	eventCargoMeasured            = "CargoMeasured.v1"
	eventScreeningResultRecorded  = "ScreeningResultRecorded.v1"
	eventCargoReceiptCompleted    = "CargoReceiptCompleted.v1"
	eventCargoDiscrepancyRecorded = "CargoDiscrepancyRecorded.v1"
	eventCargoDiscrepancyResolved = "CargoDiscrepancyResolved.v1"
	eventCargoLotClosed           = "CargoLotClosed.v1"
)

// Stamp records when PHY entered what was added, completed, resolved or closed since the lot was loaded (T1).
func (l *CargoLot) Stamp(entered time.Time) {
	entered = entered.UTC()
	st := &l.state
	stamp := func(t *time.Time, happened time.Time) {
		if t.IsZero() && !happened.IsZero() {
			*t = entered
		}
	}
	stamp(&st.CompletedEnteredAt, st.CompletedAt)
	stamp(&st.ClosedEnteredAt, st.ClosedAt)
	for i := range st.Lines {
		stamp(&st.Lines[i].EnteredAt, st.Lines[i].At)
	}
	for i := range st.Measurements {
		stamp(&st.Measurements[i].EnteredAt, st.Measurements[i].At)
	}
	for i := range st.Screenings {
		stamp(&st.Screenings[i].EnteredAt, st.Screenings[i].At)
	}
	for i := range st.Discrepancies {
		stamp(&st.Discrepancies[i].EnteredAt, st.Discrepancies[i].RaisedAt)
		stamp(&st.Discrepancies[i].ResolutionEnteredAt, st.Discrepancies[i].ResolvedAt)
	}
}

// RestoreCargoLot rebuilds a stored lot without emitting events.
func RestoreCargoLot(st CargoLotState) (*CargoLot, error) {
	st.Lines = append([]ReceiptLine(nil), st.Lines...)
	st.Measurements = append([]Measurement(nil), st.Measurements...)
	st.Screenings = append([]Screening(nil), st.Screenings...)
	st.Discrepancies = append([]Discrepancy(nil), st.Discrepancies...)
	l := &CargoLot{state: st, stored: [4]int{len(st.Lines), len(st.Measurements), len(st.Screenings), len(st.Discrepancies)}}
	bad := func(what string) (*CargoLot, error) { return nil, fmt.Errorf("%w: %s", ErrInvalidLot, what) }
	if !identifier(string(st.TenantID)) || !identifier(string(st.LotID)) || !identifier(string(st.ShipmentID)) || !st.Facility.valid() ||
		!st.Mode.Valid() || !st.Expected.Scope.valid() || len(st.Lines) == 0 {
		return bad("identity")
	}
	switch st.State {
	case LotReceiving:
		if !st.CompletedAt.IsZero() || !st.ClosedAt.IsZero() || len(st.Discrepancies) > 0 {
			return bad("receiving")
		}
	case LotReceived, LotClosed:
		completed := !st.CompletedAt.IsZero()
		if (st.State == LotReceived && !completed) || (completed && (blank(st.CompletedBy) || len(st.Measurements) == 0)) ||
			(!completed && len(st.Discrepancies) > 0) || (st.State == LotClosed) == st.ClosedAt.IsZero() {
			return bad(string(st.State))
		}
	default:
		return bad("state")
	}
	for i, x := range st.Lines {
		if x.No != i+1 || x.Packs < 1 || !packageType(x.PackageType) || !x.Condition.valid() || blank(x.By) {
			return bad(fmt.Sprintf("line %d", i+1))
		}
	}
	for i, x := range st.Measurements {
		if x.No != i+1 || x.GrossWeight.Amount < 1 || !x.Method.valid() || blank(x.By) {
			return bad(fmt.Sprintf("measurement %d", i+1))
		}
	}
	for i, x := range st.Screenings {
		if x.No != i+1 || !x.Outcome.valid() || st.Mode != ModeAir || blank(x.By) {
			return bad(fmt.Sprintf("screening %d", i+1))
		}
	}
	for i, x := range st.Discrepancies {
		if x.No != i+1 || x.Open() != x.ResolvedAt.IsZero() || (st.State == LotClosed && x.Open()) {
			return bad(fmt.Sprintf("discrepancy %d", i+1))
		}
	}
	return l, nil
}

// State returns a copy of the lot for persistence and read models.
func (l *CargoLot) State() CargoLotState {
	st := l.state
	st.Lines = append([]ReceiptLine(nil), st.Lines...)
	st.Measurements = append([]Measurement(nil), st.Measurements...)
	st.Screenings = append([]Screening(nil), st.Screenings...)
	st.Discrepancies = append([]Discrepancy(nil), st.Discrepancies...)
	return st
}

func (l *CargoLot) ID() CargoLotID       { return l.state.LotID }
func (l *CargoLot) Shipment() ShipmentID { return l.state.ShipmentID }
func (l *CargoLot) Current() LotState    { return l.state.State }

// Completed reports whether the lot's receipt was completed (a cancelled shipment's lot may close without).
func (l *CargoLot) Completed() bool  { return !l.state.CompletedAt.IsZero() }
func (l *CargoLot) Pending() []Event { return append([]Event(nil), l.pending...) }

// OpenDiscrepancies returns the number of unresolved discrepancies.
func (l *CargoLot) OpenDiscrepancies() int {
	n := 0
	for _, d := range l.state.Discrepancies {
		if d.Open() {
			n++
		}
	}
	return n
}

// Added returns what was written since the lot was loaded, and the stored discrepancies resolved since.
func (l *CargoLot) Added() (lines []ReceiptLine, measurements []Measurement, screenings []Screening, discrepancies []Discrepancy, resolved []Discrepancy) {
	st := l.state
	for _, i := range l.resolved {
		resolved = append(resolved, st.Discrepancies[i])
	}
	return st.Lines[l.stored[0]:], st.Measurements[l.stored[1]:], st.Screenings[l.stored[2]:], st.Discrepancies[l.stored[3]:], resolved
}

// Acknowledge is permitted only after the transaction holding the lot and its Outbox events commits.
func (l *CargoLot) Acknowledge() {
	l.pending, l.resolved = nil, nil
	l.stored = [4]int{len(l.state.Lines), len(l.state.Measurements), len(l.state.Screenings), len(l.state.Discrepancies)}
}
