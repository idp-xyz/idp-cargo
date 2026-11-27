package domain

import (
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// PHY V0.2: the tender of a consolidation's houses, as a load execution; PHY V0.3 adds the stuffing of a planned
// container. A loaded or tendered fact needs its own evidence; it is never derived from a booking, a membership or
// a plan (UL-04, DDD-011). Corrections are reversal lines (DDD-014); a fact that breaks a rule PHY cannot enforce
// on the carrier is recorded and flagged, never refused (G06-05). Building up a ULD comes with ULD load units.

type LoadExecutionID string

// ExecutionKind is how cargo is loaded: a TENDER of loose AIR cargo to the airline terminal or of a co-load's
// houses to the co-loader's CFS (OD-3), or the STUFFING of a planned container of an own-container LCL
// consolidation at the forwarder's CFS (PHY V0.3). BUILD_UP comes with ULD load units.
type ExecutionKind string

const (
	ExecutionTender   ExecutionKind = "TENDER"
	ExecutionStuffing ExecutionKind = "STUFFING"
)

// EvidenceSeal is the source of a stuffing's closing evidence: the seal applied to the container, its number the
// reference. The seal itself, its party and its history, are SEA's.
const EvidenceSeal = "SEAL"

// ExecutionState is a load execution's state.
type ExecutionState string

const (
	ExecutionOpen    ExecutionState = "OPEN"
	ExecutionLoading ExecutionState = "LOADING"
	ExecutionOnHold  ExecutionState = "ON_HOLD"
	ExecutionClosed  ExecutionState = "CLOSED"
	// ExecutionRework is a sealed stuffing opened again (OpenStuffingForRework): its seal is broken and kept in the
	// execution's reworks, it has no close facts, and it is unloaded by whole houses until it is sealed again (CLOSED,
	// a new seal) or given up with no live packs (CANCELLED). It is the same execution throughout, never a new one.
	ExecutionRework ExecutionState = "REWORK"
	// ExecutionCancelled is a stuffing given up with no live packs (CancelLoadExecution): terminal, kept as history.
	ExecutionCancelled ExecutionState = "CANCELLED"
)

var (
	ErrInvalidExecution  = errors.New("phy: invalid load execution")
	ErrKindNotTendered   = errors.New("phy: only AIR_CONSOL and SEA_LCL_COLOAD consolidations are tendered; SEA_LCL_OWN ones are stuffed")
	ErrKindNotStuffed    = errors.New("phy: PHY_NOT_STUFFED: only planned containers of SEA_LCL_OWN consolidations are stuffed")
	ErrNotFrozen         = errors.New("phy: PHY_HOUSES_NOT_FROZEN: a container is stuffed once the consolidation is CLOSED, its houses frozen")
	ErrNotLoading        = errors.New("phy: the load execution is not loading")
	ErrInvalidLoad       = errors.New("phy: invalid load line, reversal or hold")
	ErrOnHold            = errors.New("phy: the load execution has a hold placed")
	ErrHoldNotReleasable = errors.New("phy: the hold cannot be released yet")
	ErrInvalidCarrier    = errors.New("phy: invalid carrier evidence")
	ErrEvidenceReused    = errors.New("phy: the source reference is recorded already with other content")
)

// ReworkEnd is how a rework ended.
type ReworkEnd string

const (
	ReworkSealed  ReworkEnd = "SEALED"   // the container was sealed again, with a new seal
	ReworkGivenUp ReworkEnd = "GIVEN_UP" // the container was emptied and the stuffing given up
)

// Rework is one rework of a sealed stuffing, and with the broken seals of all the reworks the history of the seals the
// execution carried. The opening is who asked for the rework, when and why, and the seal that was broken (the whole
// close evidence, who had sealed it and when); the ending is written once: sealed again (who, when, the new seal's
// reference) or given up (who, when). A rework is never changed or removed.
type Rework struct {
	No                    int
	Reason                string
	AskedBy               string
	AskedAt               time.Time
	AskedEnteredAt        time.Time
	BrokenSeal            Evidence
	BrokenSealedBy        string
	BrokenSealedAt        time.Time
	BrokenSealedEnteredAt time.Time
	End                   ReworkEnd // empty while the rework is open
	EndedBy               string
	EndedAt               time.Time
	EndedEnteredAt        time.Time
	NewSeal               string // the new seal's reference, for SEALED
}

// Open reports whether the rework has not ended.
func (r Rework) Open() bool { return r.End == "" }

// Basis of a load line's weight and volume.
type Basis string

const (
	BasisMeasured    Basis = "MEASURED"    // measured for the line
	BasisApportioned Basis = "APPORTIONED" // from the lot's measurement, the last packs taking the rest
)

// LoadLine is one immutable line of cargo loaded or tendered from a lot, under the house's live membership entry.
type LoadLine struct {
	No            int
	ShipmentID    ShipmentID
	LotID         CargoLotID
	EntryNo       int
	Packs         int64
	GrossWeight   kernel.Quantity
	Volume        kernel.Quantity
	Basis         Basis
	MeasurementNo int // the lot measurement apportioned from; 0 when measured for the line
	Evidence      Evidence
	By            string
	At            time.Time
	EnteredAt     time.Time
}

// ReversalReason is why loaded packs come out again.
type ReversalReason string

const (
	ReversalUnstuffed            ReversalReason = "UNSTUFFED"
	ReversalOffloadedByForwarder ReversalReason = "OFFLOADED_BY_FORWARDER"
	ReversalMisloaded            ReversalReason = "MISLOADED"
)

func (r ReversalReason) valid() bool {
	return r == ReversalUnstuffed || r == ReversalOffloadedByForwarder || r == ReversalMisloaded
}

// Reversal reverses packs of a load line; the line stays (DDD-014).
type Reversal struct {
	No          int
	LineNo      int
	Packs       int64
	GrossWeight kernel.Quantity
	Volume      kernel.Quantity
	Reason      ReversalReason
	Evidence    Evidence
	By          string
	At          time.Time
	EnteredAt   time.Time
}

// HoldReason is why loading stops.
type HoldReason string

const (
	HoldScreeningFailed HoldReason = "SECURITY_SCREENING_FAILED" // placed by PHY on a FAILED screening result
	HoldDGQuery         HoldReason = "DG_QUERY"
	HoldCustoms         HoldReason = "CUSTOMS_HOLD"
	HoldOverweight      HoldReason = "OVERWEIGHT"
	HoldDamage          HoldReason = "DAMAGE"
)

func (r HoldReason) manual() bool {
	return r == HoldDGQuery || r == HoldCustoms || r == HoldOverweight || r == HoldDamage
}

// Hold is a hold placed on the execution; only its release is written later, once.
type Hold struct {
	No               int
	Reason           HoldReason
	LotID            CargoLotID // the lot that failed screening
	Text             string
	Evidence         Evidence
	By               string
	At               time.Time
	EnteredAt        time.Time
	ReleaseEvidence  Evidence
	ReleasedBy       string
	ReleasedAt       time.Time
	ReleaseEnteredAt time.Time
}

// Placed reports whether the hold is not released.
func (h Hold) Placed() bool { return h.ReleasedAt.IsZero() }

// CarrierFact is a carrier's physical status of tendered cargo (OD-4: entered by hand in N1).
type CarrierFact string

const (
	CarrierAccepted      CarrierFact = "ACCEPTED"
	CarrierManifested    CarrierFact = "MANIFESTED"
	CarrierDeparted      CarrierFact = "DEPARTED"
	CarrierOffloaded     CarrierFact = "OFFLOADED"
	CarrierArrived       CarrierFact = "ARRIVED"
	CarrierLoadedOnBoard CarrierFact = "LOADED_ON_BOARD"
)

func (f CarrierFact) valid() bool {
	switch f {
	case CarrierAccepted, CarrierManifested, CarrierDeparted, CarrierOffloaded, CarrierArrived, CarrierLoadedOnBoard:
		return true
	}
	return false
}

// CarrierEvidence is a carrier's status of the tendered pieces, of the whole execution (the master) or of one
// house, on a flight or voyage; the source reference identifies the carrier message or document.
type CarrierEvidence struct {
	No          int
	Fact        CarrierFact
	ShipmentID  ShipmentID // empty for the master
	Pieces      int64
	GrossWeight kernel.Quantity // zero when the carrier gave none
	Transport   string          // flight or voyage reference
	Evidence    Evidence
	By          string
	At          time.Time
	EnteredAt   time.Time
}

func (e CarrierEvidence) same(o CarrierEvidence) bool {
	return e.Fact == o.Fact && e.ShipmentID == o.ShipmentID && e.Pieces == o.Pieces && e.GrossWeight == o.GrossWeight &&
		e.Transport == o.Transport && e.Evidence.SHA256 == o.Evidence.SHA256 && e.Evidence.OccurredAt.Equal(o.Evidence.OccurredAt)
}

// LoadingDiscrepancyKind is a kind of difference the carrier's evidence shows.
type LoadingDiscrepancyKind string

const (
	LoadConflict  LoadingDiscrepancyKind = "LOAD_CONFLICT" // more pieces than tendered (DDD-013)
	LoadOffloaded LoadingDiscrepancyKind = "OFFLOADED"     // the carrier offloaded pieces
)

// LoadingDiscrepancy is a difference against the tender; only its resolution is written later, once.
type LoadingDiscrepancy struct {
	No                  int
	Kind                LoadingDiscrepancyKind
	EvidenceNo          int
	ShipmentID          ShipmentID // empty for the master
	Expected            int64      // pieces tendered
	Observed            int64
	RaisedAt            time.Time
	EnteredAt           time.Time
	Resolution          Resolution // ACCEPTED or CLAIM_OPENED
	Reference           string
	Text                string
	ResolvedBy          string
	ResolvedAt          time.Time
	ResolutionEnteredAt time.Time
}

// Open reports whether the discrepancy is unresolved.
func (d LoadingDiscrepancy) Open() bool { return d.Resolution == "" }

// LoadExecutionState is a stored load execution, for persistence.
type LoadExecutionState struct {
	TenantID         TenantID
	ExecutionID      LoadExecutionID
	ConsolidationID  ConsolidationID
	Kind             ExecutionKind
	Mode             Mode
	Facility         Facility // where the cargo is tendered or stuffed: the airline terminal, the co-loader's CFS or the forwarder's
	LoadUnitNo       int      // the planned load unit a STUFFING fills; none for a TENDER
	State            ExecutionState
	OpenedBy         string
	OpenedAt         time.Time
	OpenedEnteredAt  time.Time
	StartedBy        string
	StartedAt        time.Time
	StartedEnteredAt time.Time
	ClosedBy         string
	ClosedAt         time.Time
	ClosedEnteredAt  time.Time
	CloseEvidence    Evidence
	CancelledBy      string // who gave up the stuffing, when and why: set exactly when CANCELLED
	CancelledAt      time.Time
	CancelEnteredAt  time.Time
	CancelReason     string
	Reworks          []Rework // the reworks of a stuffing, numbered from 1; the last is open exactly when the state is REWORK
	Lines            []LoadLine
	Reversals        []Reversal
	Holds            []Hold
	CarrierEvidence  []CarrierEvidence
	Discrepancies    []LoadingDiscrepancy
}

// LoadExecution is the aggregate root of one loading or tender of a consolidation's houses.
type LoadExecution struct {
	state    LoadExecutionState
	stored   [5]int // lines, reversals, holds, carrier evidence, discrepancies persisted
	reworks  int    // reworks persisted
	released []int  // persisted holds released since, by index
	resolved []int  // persisted discrepancies resolved since, by index
	ended    []int  // persisted reworks ended since, by index
	pending  []Event
}

// Event types of load executions, as package api publishes them.
const (
	eventLoadExecutionOpened         = "LoadExecutionOpened.v1"
	eventLoadingStarted              = "LoadingStarted.v1"
	eventCargoLoaded                 = "CargoLoaded.v1"
	eventCargoUnloaded               = "CargoUnloaded.v1"
	eventLoadExecutionHeld           = "LoadExecutionHeld.v1"
	eventLoadExecutionHoldReleased   = "LoadExecutionHoldReleased.v1"
	eventLoadExecutionResumed        = "LoadExecutionResumed.v1"
	eventLoadExecutionClosed         = "LoadExecutionClosed.v1"
	eventLoadExecutionCancelled      = "LoadExecutionCancelled.v1"
	eventLoadExecutionReworkOpened   = "LoadExecutionReworkOpened.v1"
	eventCarrierLoadEvidenceRecorded = "CarrierLoadEvidenceRecorded.v1"
	eventLoadingDiscrepancyRecorded  = "LoadingDiscrepancyRecorded.v1"
	eventLoadingDiscrepancyResolved  = "LoadingDiscrepancyResolved.v1"
)

var transportForm = regexp.MustCompile(`^[0-9A-Z][0-9A-Z /._-]{0,34}$`)

// datedFlight is an air flight with its date (V2.111): the carrier's two-character designator, the flight number with
// an optional suffix, and the day and month of departure, as IATA's status messages write it (MU587/02DEC).
var datedFlight = regexp.MustCompile(`^[0-9A-Z]{2}[0-9]{1,4}[A-Z]?/(0[1-9]|[12][0-9]|3[01])(JAN|FEB|MAR|APR|MAY|JUN|JUL|AUG|SEP|OCT|NOV|DEC)$`)

// OpenLoadExecution opens a tender of an OPEN or CLOSED consolidation at a facility of its origin.
func OpenLoadExecution(tenant TenantID, id LoadExecutionID, c *Consolidation, kind ExecutionKind, facility Facility, by string, at time.Time) (*LoadExecution, error) {
	if c == nil || !identifier(string(tenant)) || !identifier(string(id)) || c.Tenant() != tenant || !facility.valid() || blank(by) || at.IsZero() {
		return nil, ErrInvalidExecution
	}
	if kind != ExecutionTender {
		return nil, fmt.Errorf("%w: kind %q; a STUFFING opens on its load unit, BUILD_UP comes with ULD load units", ErrInvalidExecution, kind)
	}
	if k := c.State().Kind; k != KindAirConsol && k != KindSeaLCLCoload {
		return nil, ErrKindNotTendered
	}
	current := c.Current()
	if s := current.State; s != StateOpen && s != StateClosed {
		return nil, ErrInvalidTransition
	}
	if facility.Location != current.Route.Origin {
		return nil, fmt.Errorf("%w: the facility lies at %s, the consolidation departs %s", ErrInvalidExecution, facility.Location.Code, current.Route.Origin.Code)
	}
	if at.Before(c.state.Versions[0].At) {
		return nil, ErrInvalidChange
	}
	x := &LoadExecution{state: LoadExecutionState{TenantID: tenant, ExecutionID: id, ConsolidationID: c.ID(), Kind: kind, Mode: c.Mode(),
		Facility: facility, State: ExecutionOpen, OpenedBy: by, OpenedAt: at.UTC()}}
	x.record(eventLoadExecutionOpened, at, "", "", 0, "")
	return x, nil
}

// OpenStuffing opens the stuffing of a planned container of a CLOSED own-container LCL consolidation (its houses
// frozen) at a facility of its origin, the forwarder's CFS. A unit has at most one stuffing that is not cancelled:
// taken reports whether one was opened on it already (a cancelled one, given up with nothing loaded, does not count).
// Business time is not before the consolidation's latest change (its close).
func OpenStuffing(tenant TenantID, id LoadExecutionID, c *Consolidation, unitNo int, facility Facility, taken bool, by string, at time.Time) (*LoadExecution, error) {
	return OpenStuffingAfter(tenant, id, c, unitNo, facility, taken, time.Time{}, by, at)
}

// OpenStuffingAfter is OpenStuffing for a unit whose earlier stuffings were given up: the new stuffing is dated not
// before the latest cancellation of a stuffing of the unit, lastCancelled (zero when none was).
func OpenStuffingAfter(tenant TenantID, id LoadExecutionID, c *Consolidation, unitNo int, facility Facility, taken bool, lastCancelled time.Time,
	by string, at time.Time) (*LoadExecution, error) {
	if c == nil || !identifier(string(tenant)) || !identifier(string(id)) || c.Tenant() != tenant || !facility.valid() || blank(by) || at.IsZero() {
		return nil, ErrInvalidExecution
	}
	if c.State().Kind != KindSeaLCLOwn {
		return nil, ErrKindNotStuffed
	}
	current := c.Current()
	switch current.State {
	case StateClosed:
	case StateOpen:
		return nil, fmt.Errorf("%w: it is OPEN", ErrNotFrozen)
	default:
		return nil, ErrInvalidTransition
	}
	if unitNo < 1 || unitNo > len(c.state.LoadUnits) || c.state.LoadUnits[unitNo-1].Status != UnitPlanned ||
		c.state.LoadUnits[unitNo-1].Kind != UnitContainer {
		return nil, fmt.Errorf("%w: no planned container %d", ErrInvalidLoadUnit, unitNo)
	}
	if taken {
		return nil, fmt.Errorf("%w: unit %d has its stuffing", ErrLoadUnitInUse, unitNo)
	}
	if facility.Location != current.Route.Origin {
		return nil, fmt.Errorf("%w: the facility lies at %s, the consolidation departs %s", ErrInvalidExecution, facility.Location.Code, current.Route.Origin.Code)
	}
	if at.Before(c.lastChange()) {
		return nil, fmt.Errorf("%w: a stuffing is dated not before the consolidation's close and its units' changes", ErrInvalidChange)
	}
	if at.Before(lastCancelled) {
		return nil, fmt.Errorf("%w: a stuffing of unit %d is dated not before the cancellation of the one before it", ErrInvalidChange, unitNo)
	}
	x := &LoadExecution{state: LoadExecutionState{TenantID: tenant, ExecutionID: id, ConsolidationID: c.ID(), Kind: ExecutionStuffing, Mode: c.Mode(),
		Facility: facility, LoadUnitNo: unitNo, State: ExecutionOpen, OpenedBy: by, OpenedAt: at.UTC()}}
	x.record(eventLoadExecutionOpened, at, "", "", unitNo, "")
	return x, nil
}

// Start starts loading.
func (x *LoadExecution) Start(by string, at time.Time) error {
	if x.state.State == ExecutionRework {
		return fmt.Errorf("%w: a container in rework is not started; it is unloaded by whole houses and sealed again", ErrInvalidTransition)
	}
	if x.state.State != ExecutionOpen {
		return ErrInvalidTransition
	}
	if err := x.changeable(by, at); err != nil {
		return err
	}
	x.state.State, x.state.StartedBy, x.state.StartedAt = ExecutionLoading, by, at.UTC()
	x.record(eventLoadingStarted, at, "", "", 0, "")
	return nil
}

// LotLoad is what is live loaded of a lot across every load execution: packs, gross weight and, in the unit of
// the lot's latest measurement, volume.
type LotLoad struct {
	Packs       int64
	GrossWeight int64
	Volume      int64
}

// Load records packs of a lot loaded or tendered under the house's live membership entry in the execution's
// consolidation. lotLoaded is the lot's live load across every execution and shipmentLoaded the house's live
// loaded packs in the consolidation, both read under the lot's and the ledger's locks: neither may exceed what
// was received or what the house plans (CONSERVATION_VIOLATION). The line's weight and volume are measured for
// it, or apportioned from the lot's latest measurement: its share of the packs received, rounded down, and the
// packs that complete the lot take what the live lines leave of the measure. Cargo whose lot failed screening is
// recorded and holds the execution (G06-05).
func (x *LoadExecution) Load(in LoadLine, c *Consolidation, lot *CargoLot, entry Entry, lotLoaded LotLoad, shipmentLoaded int64, by string, at time.Time) error {
	if x.state.State == ExecutionRework {
		return fmt.Errorf("%w: a container in rework takes no load; it is unloaded by whole houses and sealed again", ErrNotLoading)
	}
	if x.state.State != ExecutionLoading {
		return ErrNotLoading
	}
	if c == nil || c.ID() != x.state.ConsolidationID || lot == nil || lot.state.TenantID != x.state.TenantID {
		return ErrInvalidLoad
	}
	if s := c.Current().State; s != StateOpen && s != StateClosed {
		return ErrInvalidTransition
	}
	if x.state.Kind == ExecutionStuffing && c.Current().State != StateClosed {
		return fmt.Errorf("%w: it is %s", ErrNotFrozen, c.Current().State)
	}
	if x.state.Kind == ExecutionStuffing && at.Before(c.lastChange()) {
		return fmt.Errorf("%w: a stuffed line is dated not before the consolidation's latest change", ErrInvalidChange)
	}
	if !entry.Live() || entry.ConsolidationID != x.state.ConsolidationID {
		return fmt.Errorf("%w: %s in %s", ErrNotAttached, lot.Shipment(), x.state.ConsolidationID)
	}
	if lot.state.State == LotClosed || lot.state.Mode != x.state.Mode {
		return fmt.Errorf("%w: lot %s is %s", ErrInvalidLoad, lot.ID(), lot.state.State)
	}
	if in.Packs < 1 {
		return fmt.Errorf("%w: packs", ErrInvalidLoad)
	}
	if !in.Evidence.valid(at) {
		return ErrInvalidEvidence
	}
	received := lot.Received()
	if lotLoaded.Packs < 0 || shipmentLoaded < 0 || lotLoaded.Packs+in.Packs > received || shipmentLoaded+in.Packs > entry.Scope.Packs {
		return fmt.Errorf("%w: %d more packs of lot %s, %d of %d received loaded; house %s plans %d, %d loaded", ErrConservation, in.Packs,
			lot.ID(), lotLoaded.Packs, received, lot.Shipment(), entry.Scope.Packs, shipmentLoaded)
	}
	line := LoadLine{ShipmentID: lot.Shipment(), LotID: lot.ID(), EntryNo: entry.No, Packs: in.Packs, Evidence: in.Evidence}
	line.Evidence.OccurredAt = line.Evidence.OccurredAt.UTC()
	if in.GrossWeight != (kernel.Quantity{}) {
		weight, volume := entry.Scope.GrossWeight.Unit, entry.Scope.Volume.Unit
		if in.GrossWeight.Unit != weight || in.GrossWeight.Amount < 1 || (in.Volume != (kernel.Quantity{}) &&
			(in.Volume.Amount < 1 || !in.Volume.Unit.Valid() || (volume != "" && in.Volume.Unit != volume))) {
			return fmt.Errorf("%w: weight in %s and volume in the plan's unit", ErrInvalidLoad, weight)
		}
		line.GrossWeight, line.Volume, line.Basis = in.GrossWeight, in.Volume, BasisMeasured
	} else {
		if in.Volume != (kernel.Quantity{}) {
			return fmt.Errorf("%w: a line measured for itself gives its gross weight", ErrInvalidLoad)
		}
		m, ok := lot.Measured()
		if !ok {
			return fmt.Errorf("%w: lot %s; measure it or give the line's weight", ErrNoMeasurement, lot.ID())
		}
		last := lotLoaded.Packs+in.Packs == received
		line.GrossWeight = kernel.Quantity{Unit: m.GrossWeight.Unit, Amount: apportion(m.GrossWeight.Amount, lotLoaded.GrossWeight, in.Packs, received, last)}
		if m.Volume != (kernel.Quantity{}) {
			line.Volume = kernel.Quantity{Unit: m.Volume.Unit, Amount: apportion(m.Volume.Amount, lotLoaded.Volume, in.Packs, received, last)}
		}
		line.Basis, line.MeasurementNo = BasisApportioned, m.No
	}
	if err := x.changeable(by, at); err != nil {
		return err
	}
	line.No, line.By, line.At = len(x.state.Lines)+1, by, at.UTC()
	x.state.Lines = append(x.state.Lines, line)
	x.record(eventCargoLoaded, at, line.ShipmentID, line.LotID, line.No, "")
	if x.state.Mode == ModeAir && lot.Screened() == ScreeningFailed {
		s := lot.state.Screenings[len(lot.state.Screenings)-1]
		x.HoldForScreening(lot.ID(), s.Evidence, by, at)
	}
	return nil
}

// LimitsCheck is a stuffing's cargo by the allotted figures against its unit's default limits. It is what the hold rule
// reads and what the cost basis's packing list reports, worked out by one function (CheckLimits). When a measure is in
// a unit PHY does not convert, Checked is false, Unchecked names the unit, and the figures and the two Over flags are
// not to be relied on: the cargo is then not known.
type LimitsCheck struct {
	Checked      bool
	Unchecked    kernel.UnitCode
	CargoGrams   int64
	CargoCubicCm int64
	GrossGrams   int64 // the tare and the cargo
	OverMaxGross bool
	OverCapacity bool
}

// RaisedOver reports in which dimensions a command raised a stuffing's cargo into excess, after is the check after the
// command and c the check before it: the weight when the container is over its maximum gross and its grams are greater
// than before, the volume when it is over its capacity and its cubic centimetres are greater than before, and the check
// itself when it could be made before and cannot be now. It is the condition under which a command that did not address
// a stuffing may hold it: a container is not held again for a dimension that did not rise, whatever the other did.
func (c LimitsCheck) RaisedOver(after LimitsCheck) (weight, volume, unchecked bool) {
	return after.OverMaxGross && after.CargoGrams > c.CargoGrams, after.OverCapacity && after.CargoCubicCm > c.CargoCubicCm, c.Checked && !after.Checked
}

// CheckLimits works out the stuffing's cargo and sets it against the limits of the unit the stuffing fills in c. It
// reports false when the stuffing is not one of c's or c has no such unit. The cargo is the sum over the stuffing's
// live lines: lines measured for themselves as measured, apportioned lines at the part of their lot's latest measure
// that the allotment gives this stuffing (AllotLots over every execution that holds live packs of the lots); each
// weight and volume is converted to grams and cubic centimetres, rounded up, and added, and the sum is capped. lots
// holds the lots of the stuffing's lines. A stuffing is checked in every state; whether a hold is placed is the
// decision of HoldIfOverLimitsAllotted.
//
// The check and the packing list's Limits come from this function, so they agree with each other. They do not always
// equal the sum of the entries the cost basis publishes for the container's houses (HouseMeasureAllotted): a house with
// a line or a lot measure without a volume beside others with one publishes no volume ("a volume not known for every
// line is not known") while the check counts the volume that is known, so the check can read more volume than the
// published figure and never less; and the check converts every line and rounds each up, while the published figure of
// a house whose lines share one unit is added as it is. A house with lines in different units is published, since
// V2.84, by converting each line to grams or cubic centimetres and rounding it up as the check does (WeightSum,
// VolumeSum), so the two read the same there, as they do for a lot measured in a unit and a line measured for itself in
// another (the allotment converts before it subtracts). A unit that cannot be converted is "limits not checked" here
// and a figure in that unit in the cost basis.
func (x *LoadExecution) CheckLimits(c *Consolidation, lots map[CargoLotID]*CargoLot, a Allotment) (LimitsCheck, bool) {
	if x.state.Kind != ExecutionStuffing || c == nil || c.ID() != x.state.ConsolidationID || x.state.LoadUnitNo < 1 ||
		x.state.LoadUnitNo > len(c.state.LoadUnits) {
		return LimitsCheck{}, false
	}
	l := c.state.LoadUnits[x.state.LoadUnitNo-1].Limits
	grams, cubicCm, unchecked := x.unitCargo(lots, a)
	r := LimitsCheck{Checked: unchecked == "", Unchecked: unchecked, CargoGrams: grams, CargoCubicCm: cubicCm,
		GrossGrams: addCapped(grams, scaled(l.TareKg, 1000))}
	if r.Checked {
		r.OverMaxGross, r.OverCapacity = r.GrossGrams > scaled(l.MaxGrossKg, 1000), cubicCm > l.CapacityCM3
	}
	return r, true
}

// HoldIfOverLimitsAllotted holds a loading stuffing whose container, its tare and its live cargo, weighs more than its
// type's default maximum gross, or whose live cargo takes more than the type's capacity. The cargo is that of
// CheckLimits: the part of each lot's measure that the allotment gives this stuffing (AllotLots over every execution
// that holds live packs of the lots). A weight or volume in a unit PHY does not convert holds it too, as not checked.
// The line or measurement that crossed the limits stays recorded (G06-05); an OVERWEIGHT hold already placed is not
// placed again, and a hold is never released here. The limits are defaults: the container's plate (SEA's) may allow
// more, which releasing the hold with its evidence records, and a later line or measurement over them holds again. A
// sealed container is not held; the packing list's Limits tell that it is over. It reports whether it placed a hold;
// the hold takes the business time at, or the execution's last change if that is later.
//
// Before V2.83 the check measured each stuffing by itself, as HouseMeasure did before V2.82, so it could read more or
// less than the allotted figure. Reading more: a lot of 6 packs and 600 kg with 2 packs weighed for themselves at
// 300 kg in container 1 and the other 4 apportioned in container 2. Container 2 alone reads 600 x 4/6 = 400 kg, the
// allotted figure is the 300 kg the weighed packs leave. Reading less: a lot of 10 packs and 1,000 kg with 2 packs
// weighed at 100 kg and 4 apportioned in container 1, and 4 apportioned in container 2. Container 2 alone read
// 1,000 x 4/10 = 400 kg, the allotted figure is (1,000 - 100) x 4/8 = 450 kg. With one stuffing per lot the allotment
// is the stuffing's own measure and the outcome, the hold and its text are those of before.
func (x *LoadExecution) HoldIfOverLimitsAllotted(c *Consolidation, lots map[CargoLotID]*CargoLot, a Allotment, evidence Evidence, by string,
	at time.Time) bool {
	return x.holdOver(c, lots, a, true, true, true, evidence, by, at)
}

// holdOver places the OVERWEIGHT hold for the dimensions wanted: the weight when the container is over its maximum
// gross, the volume when it is over its capacity, the check when it cannot be made. The text names the first of them
// that is wanted and true, the weight before the volume.
func (x *LoadExecution) holdOver(c *Consolidation, lots map[CargoLotID]*CargoLot, a Allotment, weight, volume, unchecked bool, evidence Evidence,
	by string, at time.Time) bool {
	if (x.state.State != ExecutionLoading && x.state.State != ExecutionOnHold) || blank(by) || at.IsZero() {
		return false
	}
	for _, h := range x.state.Holds {
		if h.Placed() && h.Reason == HoldOverweight {
			return false
		}
	}
	check, ok := x.CheckLimits(c, lots, a)
	if !ok {
		return false
	}
	l := c.state.LoadUnits[x.state.LoadUnitNo-1].Limits
	var text string
	switch {
	case !check.Checked && unchecked:
		text = fmt.Sprintf("unit %d (%s): limits not checked, a measure in %s", x.state.LoadUnitNo, l.TypeCode, check.Unchecked)
	case check.OverMaxGross && weight:
		text = fmt.Sprintf("unit %d (%s): tare and cargo %s kg over the default maximum gross %d kg (edition %s)", x.state.LoadUnitNo, l.TypeCode,
			decimal(check.GrossGrams, 3), l.MaxGrossKg, l.Edition)
	case check.OverCapacity && volume:
		text = fmt.Sprintf("unit %d (%s): cargo %s m3 over the default capacity %s m3 (edition %s)", x.state.LoadUnitNo, l.TypeCode,
			decimal(check.CargoCubicCm, 6), decimal(l.CapacityCM3, 6), l.Edition)
	default:
		return false
	}
	x.hold(Hold{Reason: HoldOverweight, Text: text, Evidence: evidence}, by, later(at, x.lastChange()))
	return true
}

// HoldIfRaisedOverLimits is HoldIfOverLimitsAllotted for a stuffing a command did not address: it holds it only for a
// dimension the command raised into excess (before is what CheckLimits said before the command's change, after it is
// worked out here, see LimitsCheck.RaisedOver). So a hold a person released does not come back because of another
// container's command that left its figure where it was, or lowered the figure it is over in.
func (x *LoadExecution) HoldIfRaisedOverLimits(c *Consolidation, lots map[CargoLotID]*CargoLot, a Allotment, before LimitsCheck, evidence Evidence,
	by string, at time.Time) bool {
	after, ok := x.CheckLimits(c, lots, a)
	if !ok {
		return false
	}
	weight, volume, unchecked := before.RaisedOver(after)
	if !weight && !volume && !unchecked {
		return false
	}
	return x.holdOver(c, lots, a, weight, volume, unchecked, evidence, by, at)
}

// HoldIfOverLimits is HoldIfOverLimitsAllotted for a stuffing that holds all the live packs of its lots: the allotment
// is worked out over the stuffing alone. It has no caller in the commands any more: they read the other stuffings of
// the lots and call HoldIfOverLimitsAllotted or HoldIfRaisedOverLimits. It is kept for the tests of the single
// stuffing.
func (x *LoadExecution) HoldIfOverLimits(c *Consolidation, lots map[CargoLotID]*CargoLot, evidence Evidence, by string, at time.Time) bool {
	return x.HoldIfOverLimitsAllotted(c, lots, AllotLots([]*LoadExecution{x}, lots), evidence, by, at)
}

// unitCargo is the stuffing's live cargo in grams and cubic centimetres, line by line as HouseMeasureAllotted reads
// them: an apportioned line's lot has the part the allotment gives this stuffing, or the stuffing's own measure of it
// when the allotment does not know it. It also returns the first unit it could not convert.
func (x *LoadExecution) unitCargo(lots map[CargoLotID]*CargoLot, a Allotment) (grams, cubicCm int64, unchecked kernel.UnitCode) {
	add := func(total *int64, q kernel.Quantity, convert func(kernel.Quantity) (int64, bool)) {
		if q.Amount == 0 && q.Unit == "" {
			return
		}
		v, ok := convert(q)
		if !ok && unchecked == "" {
			unchecked = q.Unit
		}
		*total = addCapped(*total, v)
	}
	type lotShare struct {
		apportioned, measuredPacks int64
		weights, volumes           []kernel.Quantity
	}
	byLot := map[CargoLotID]*lotShare{}
	var order []CargoLotID
	for _, line := range x.state.Lines {
		live, w, v := x.lineLive(line.No)
		if live == 0 {
			continue
		}
		lot, ok := lots[line.LotID]
		if ok {
			if _, measured := lot.Measured(); measured && lot.Received() > 0 {
				s := byLot[line.LotID]
				if s == nil {
					s = &lotShare{}
					byLot[line.LotID], order = s, append(order, line.LotID)
				}
				if line.Basis == BasisApportioned {
					s.apportioned += live
					continue
				}
				s.measuredPacks += live
				s.weights = append(s.weights, kernel.Quantity{Unit: line.GrossWeight.Unit, Amount: w})
				s.volumes = append(s.volumes, kernel.Quantity{Unit: line.Volume.Unit, Amount: v})
			}
		}
		add(&grams, kernel.Quantity{Unit: line.GrossWeight.Unit, Amount: w}, toGrams)
		if line.Volume != (kernel.Quantity{}) {
			add(&cubicCm, kernel.Quantity{Unit: line.Volume.Unit, Amount: v}, toCubicCm)
		}
	}
	for _, id := range order {
		s := byLot[id]
		if s.apportioned == 0 {
			continue
		}
		m, _ := lots[id].Measured()
		w, v, known := a.part(x.state.ExecutionID, id)
		if !known {
			w, v = lotPart(m, lots[id].Received(), s.measuredPacks, s.weights, s.volumes, s.apportioned)
		}
		add(&grams, w, toGrams)
		if m.Volume != (kernel.Quantity{}) {
			add(&cubicCm, v, toCubicCm)
		}
	}
	return grams, cubicCm, unchecked
}

// LastChange is the business time of the execution's latest change.
func (x *LoadExecution) LastChange() time.Time { return x.lastChange() }

// apportion is the part of a lot's measure that packs of received take: their share rounded down, or, for the
// packs that complete the lot, what the live lines leave of it (never less than zero).
func apportion(total, live, packs, received int64, last bool) int64 {
	if last {
		return max(total-live, 0)
	}
	return share(total, packs, received)
}

// share is total's part for packs of all, rounded down.
func share(total, packs, all int64) int64 {
	v := new(big.Int).Mul(big.NewInt(total), big.NewInt(packs))
	return v.Quo(v, big.NewInt(all)).Int64()
}

// Unload reverses packs of a live load line of lot, with a reason and evidence, while loading or on hold; after
// the tender only packs taken back from the carrier (OFFLOADED_BY_FORWARDER: retrieved from the terminal or the
// co-loader's CFS after an offload or a cancellation). A closed lot's packs stay as loaded. Reversing every live
// pack of the line takes its live weight and volume; fewer take their share, rounded down.
func (x *LoadExecution) Unload(lineNo int, packs int64, reason ReversalReason, evidence Evidence, lot *CargoLot, by string, at time.Time) error {
	switch x.state.State {
	case ExecutionLoading, ExecutionOnHold:
	case ExecutionRework:
		return fmt.Errorf("%w: a container in rework is unloaded by whole houses (UnloadHouseForRework), not by line", ErrNotLoading)
	case ExecutionClosed:
		if x.state.Kind == ExecutionStuffing {
			return fmt.Errorf("%w: a sealed container is opened for rework first (OpenStuffingForRework) and then unloaded by whole houses", ErrNotLoading)
		}
		if reason != ReversalOffloadedByForwarder {
			return fmt.Errorf("%w: after the tender only packs taken back from the carrier (%s)", ErrNotLoading, ReversalOffloadedByForwarder)
		}
	default:
		return ErrNotLoading
	}
	if lineNo < 1 || lineNo > len(x.state.Lines) || !reason.valid() {
		return fmt.Errorf("%w: line %d, reason %q", ErrInvalidLoad, lineNo, reason)
	}
	if lot == nil || lot.ID() != x.state.Lines[lineNo-1].LotID {
		return fmt.Errorf("%w: line %d is of lot %s", ErrInvalidLoad, lineNo, x.state.Lines[lineNo-1].LotID)
	}
	if lot.state.State == LotClosed {
		return fmt.Errorf("%w: %s", ErrLotClosed, lot.ID())
	}
	live, _, _ := x.lineLive(lineNo)
	if packs < 1 || packs > live {
		return fmt.Errorf("%w: %d packs of line %d, %d live", ErrInvalidLoad, packs, lineNo, live)
	}
	if !evidence.valid(at) {
		return ErrInvalidEvidence
	}
	if err := x.changeable(by, at); err != nil {
		return err
	}
	x.reverse(lineNo, packs, reason, evidence, by, at)
	return nil
}

// reverse reverses packs of a live line, which the caller has checked: the packs and the evidence, the reason and the
// time. Reversing every live pack of the line takes its live weight and volume; fewer take their share, rounded down.
func (x *LoadExecution) reverse(lineNo int, packs int64, reason ReversalReason, evidence Evidence, by string, at time.Time) {
	live, weight, volume := x.lineLive(lineNo)
	line := x.state.Lines[lineNo-1]
	r := Reversal{No: len(x.state.Reversals) + 1, LineNo: lineNo, Packs: packs, Reason: reason, Evidence: evidence, By: by, At: at.UTC(),
		GrossWeight: kernel.Quantity{Unit: line.GrossWeight.Unit, Amount: weight}}
	r.Evidence.OccurredAt = r.Evidence.OccurredAt.UTC()
	if line.Volume != (kernel.Quantity{}) {
		r.Volume = kernel.Quantity{Unit: line.Volume.Unit, Amount: volume}
	}
	if packs < live {
		r.GrossWeight.Amount = share(weight, packs, live)
		if line.Volume != (kernel.Quantity{}) {
			r.Volume.Amount = share(volume, packs, live)
		}
	}
	x.state.Reversals = append(x.state.Reversals, r)
	x.record(eventCargoUnloaded, at, line.ShipmentID, line.LotID, r.No, string(reason))
}

// lineLive returns a line's live packs, gross weight and volume after its reversals.
func (x *LoadExecution) lineLive(lineNo int) (packs, weight, volume int64) {
	line := x.state.Lines[lineNo-1]
	packs, weight, volume = line.Packs, line.GrossWeight.Amount, line.Volume.Amount
	for _, r := range x.state.Reversals {
		if r.LineNo == lineNo {
			packs, weight, volume = packs-r.Packs, weight-r.GrossWeight.Amount, volume-r.Volume.Amount
		}
	}
	return packs, weight, volume
}

// PlaceHold holds the execution for a reason an operator gives, with evidence.
func (x *LoadExecution) PlaceHold(reason HoldReason, text string, evidence Evidence, by string, at time.Time) error {
	if x.state.State == ExecutionRework {
		return fmt.Errorf("%w: a container in rework takes no hold", ErrNotLoading)
	}
	if x.state.State != ExecutionLoading && x.state.State != ExecutionOnHold {
		return ErrNotLoading
	}
	if !reason.manual() || !optionalText(text) {
		return fmt.Errorf("%w: hold reason %q", ErrInvalidLoad, reason)
	}
	if !evidence.valid(at) {
		return ErrInvalidEvidence
	}
	if err := x.changeable(by, at); err != nil {
		return err
	}
	x.hold(Hold{Reason: reason, Text: text, Evidence: evidence}, by, at)
	return nil
}

// HoldForScreening holds a loading execution that has live packs of a lot whose screening failed, unless a
// screening hold for the lot is placed already. The hold takes the screening's business time, or the
// execution's last change if that is later (a result entered after further lines). It reports whether it placed
// one.
func (x *LoadExecution) HoldForScreening(lot CargoLotID, evidence Evidence, by string, at time.Time) bool {
	if (x.state.State != ExecutionLoading && x.state.State != ExecutionOnHold) || x.LiveFromLot(lot) == 0 || blank(by) || at.IsZero() {
		return false
	}
	at = later(at, x.lastChange())
	for _, h := range x.state.Holds {
		if h.Placed() && h.Reason == HoldScreeningFailed && h.LotID == lot {
			return false
		}
	}
	x.hold(Hold{Reason: HoldScreeningFailed, LotID: lot, Evidence: evidence}, by, at)
	return true
}

func (x *LoadExecution) hold(h Hold, by string, at time.Time) {
	h.No, h.By, h.At = len(x.state.Holds)+1, by, at.UTC()
	h.Evidence.OccurredAt = h.Evidence.OccurredAt.UTC()
	x.state.Holds = append(x.state.Holds, h)
	x.state.State = ExecutionOnHold
	x.record(eventLoadExecutionHeld, at, "", h.LotID, h.No, string(h.Reason))
}

// ReleaseHold releases a placed hold with evidence; a screening hold only once the lot's latest screening is
// PASSED or NOT_REQUIRED, or none of its packs is live in the execution. Releasing the last hold resumes
// loading.
func (x *LoadExecution) ReleaseHold(no int, evidence Evidence, lotOutcome Outcome, by string, at time.Time) error {
	if x.state.State == ExecutionCancelled {
		return fmt.Errorf("%w: a cancelled execution takes no further command", ErrInvalidTransition)
	}
	if x.state.State == ExecutionRework {
		return fmt.Errorf("%w: a container in rework has no hold to release", ErrHoldNotReleasable)
	}
	if no < 1 || no > len(x.state.Holds) || !x.state.Holds[no-1].Placed() {
		return fmt.Errorf("%w: hold %d", ErrHoldNotReleasable, no)
	}
	h := &x.state.Holds[no-1]
	if h.Reason == HoldScreeningFailed && !lotOutcome.Cleared() && x.LiveFromLot(h.LotID) > 0 {
		return fmt.Errorf("%w: lot %s is not cleared by screening and has live packs", ErrHoldNotReleasable, h.LotID)
	}
	if !evidence.valid(at) {
		return ErrInvalidEvidence
	}
	if err := x.changeable(by, at); err != nil {
		return err
	}
	h.ReleaseEvidence, h.ReleasedBy, h.ReleasedAt = evidence, by, at.UTC()
	h.ReleaseEvidence.OccurredAt = h.ReleaseEvidence.OccurredAt.UTC()
	if no-1 < x.stored[2] {
		x.released = append(x.released, no-1)
	}
	x.record(eventLoadExecutionHoldReleased, at, "", h.LotID, no, string(h.Reason))
	if x.Placed() == 0 {
		x.state.State = ExecutionLoading
		x.record(eventLoadExecutionResumed, at, "", "", 0, "")
	}
	return nil
}

// Close closes a loading tender with its closing evidence: the tender receipt of the airline terminal or the
// co-loader's CFS. A stuffing is closed by Seal.
func (x *LoadExecution) Close(evidence Evidence, by string, at time.Time) error {
	if x.state.Kind == ExecutionStuffing {
		return fmt.Errorf("%w: a stuffing closes with the container's seal", ErrInvalidExecution)
	}
	return x.close(evidence, by, at)
}

// Seal closes a loading stuffing with the container's seal as evidence (source SEAL, the seal number as reference),
// with cargo in the container, while its consolidation c is CLOSED: a container is sealed on a frozen house list. A
// container in rework is sealed again the same way, with a seal that differs from every seal the execution carried
// before: a broken seal is not put back. The rework ends, sealed again by this actor at this time with the new seal.
func (x *LoadExecution) Seal(c *Consolidation, evidence Evidence, by string, at time.Time) error {
	if x.state.Kind != ExecutionStuffing || c == nil || c.ID() != x.state.ConsolidationID {
		return fmt.Errorf("%w: only a stuffing is sealed, with its consolidation", ErrInvalidExecution)
	}
	if s := c.Current().State; s != StateClosed {
		return fmt.Errorf("%w: it is %s", ErrNotFrozen, s)
	}
	if at.Before(c.lastChange()) {
		return fmt.Errorf("%w: a seal is dated not before the consolidation's latest change", ErrInvalidChange)
	}
	sealing := x.state.State == ExecutionLoading || x.state.State == ExecutionRework
	if sealing && evidence.valid(at) && evidence.Source != EvidenceSeal {
		return fmt.Errorf("%w: a stuffing closes with the container's seal (source %s)", ErrInvalidEvidence, EvidenceSeal)
	}
	if sealing && evidence.valid(at) && evidence.Reference != strings.TrimSpace(evidence.Reference) {
		return fmt.Errorf("%w: the seal's reference %q has a blank at an end; it is the number on the seal, as written", ErrInvalidEvidence, evidence.Reference)
	}
	if sealing && x.Tendered() == 0 {
		return fmt.Errorf("%w: nothing is stuffed in the container", ErrInvalidLoad)
	}
	if x.state.State == ExecutionRework && evidence.valid(at) {
		for _, r := range x.state.Reworks {
			if sealKey(r.BrokenSeal.Reference) == sealKey(evidence.Reference) {
				return fmt.Errorf("%w: seal %s was on this container before and was broken in rework %d; a new seal is applied", ErrInvalidEvidence,
					evidence.Reference, r.No)
			}
		}
	}
	return x.close(evidence, by, at)
}

// sealKey is a seal's reference by its letters and digits, each in capitals, keeping what is ASCII: seal-1, SEAL 1 and
// SEAL1 are one seal, so a seal that was broken is not put back by keying it another way. Upper-case comes first, then
// the ASCII test, so U+0131 (dotless i) and U+017F (long s), letters whose capitals are I and S, count as I and S. That
// makes this key exactly the ASCII part of SEA's sealKey (sea/internal/domain/visit.go: every Unicode letter and digit,
// in capitals): two references SEA keys alike always key alike here, and references that differ only in non-ASCII
// letters, which SEA keeps apart, are one seal here. Being the stricter rule, it is applied before the database sees a
// seal; the database's cargo_phy.seal_key is the same function.
func sealKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if u := unicode.ToUpper(r); u < unicode.MaxASCII {
				b.WriteRune(u)
			}
		}
	}
	return b.String()
}

func (x *LoadExecution) close(evidence Evidence, by string, at time.Time) error {
	if x.state.State == ExecutionOnHold {
		return ErrOnHold
	}
	reseal := x.state.State == ExecutionRework && x.state.Kind == ExecutionStuffing
	if x.state.State != ExecutionLoading && !reseal {
		return ErrInvalidTransition
	}
	if !evidence.valid(at) {
		return ErrInvalidEvidence
	}
	if err := x.changeable(by, at); err != nil {
		return err
	}
	evidence.OccurredAt = evidence.OccurredAt.UTC()
	if reseal {
		i := len(x.state.Reworks) - 1
		r := &x.state.Reworks[i]
		r.End, r.EndedBy, r.EndedAt, r.NewSeal = ReworkSealed, by, at.UTC(), evidence.Reference
		if i < x.reworks {
			x.ended = append(x.ended, i)
		}
	}
	x.state.State, x.state.ClosedBy, x.state.ClosedAt, x.state.CloseEvidence = ExecutionClosed, by, at.UTC(), evidence
	x.record(eventLoadExecutionClosed, at, "", "", 0, "")
	return nil
}

// Cancel gives up a stuffing that holds no live packs: OPEN, LOADING or ON_HOLD, with nothing loaded or every loaded
// pack unloaded again, or in REWORK once the rework has unloaded every house (the rework ends, given up; a sealed
// container is opened for rework first, and a tender is closed with its receipt). The reason is required text of at
// most 500 characters, and the business time is not before the execution's last change. The execution becomes
// CANCELLED, terminal: it keeps its lines, reversals and holds as history and takes no further command, and its
// container is planned again (the unit's status is derived from a stuffing that is not cancelled). A hold still placed
// ends with the execution and stays recorded as placed: nothing is released, since nothing is loaded to release.
// Carrier evidence and discrepancies follow a closed tender, so a stuffing that can be given up has none open. Like
// Start and the hold commands, a cancellation is dated against the execution's own last change and not against the
// consolidation's versions: one dated before a later reopen of the consolidation is accepted. The event carries the
// load unit as its item, as the opening's does.
func (x *LoadExecution) Cancel(reason, by string, at time.Time) error {
	if x.state.Kind != ExecutionStuffing {
		return fmt.Errorf("%w: only a stuffing is given up; a tender is closed with its receipt", ErrInvalidExecution)
	}
	switch x.state.State {
	case ExecutionOpen, ExecutionLoading, ExecutionOnHold, ExecutionRework:
	default:
		return fmt.Errorf("%w: a stuffing is given up while OPEN, LOADING, ON_HOLD or in REWORK, this one is %s", ErrInvalidTransition, x.state.State)
	}
	if live := x.Tendered(); live > 0 {
		return fmt.Errorf("%w: %d packs are still loaded; unload them before the stuffing is given up", ErrInvalidExecution, live)
	}
	if !text(reason) {
		return fmt.Errorf("%w: giving up a stuffing needs a reason (500 characters)", ErrInvalidExecution)
	}
	if err := x.changeable(by, at); err != nil {
		return err
	}
	if x.state.State == ExecutionRework {
		i := len(x.state.Reworks) - 1
		r := &x.state.Reworks[i]
		r.End, r.EndedBy, r.EndedAt = ReworkGivenUp, by, at.UTC()
		if i < x.reworks {
			x.ended = append(x.ended, i)
		}
	}
	x.state.State, x.state.CancelledBy, x.state.CancelledAt, x.state.CancelReason = ExecutionCancelled, by, at.UTC(), reason
	x.record(eventLoadExecutionCancelled, at, "", "", x.state.LoadUnitNo, "")
	return nil
}

// OpenForRework opens a sealed stuffing (CLOSED) for rework: its seal is broken and the execution is in REWORK, the
// same execution and not a new one on the unit (SEA keeps its id). The consolidation c must be CLOSED (not RECONCILED
// or CANCELLED, and it cannot be OPEN again with a sealed container); no carrier evidence has been recorded on the
// execution, since the cargo then left the forwarder's hands, and no loading discrepancy is open on it; the reason is
// required text of at most 500 characters; and the time is not before the execution's last change. The broken seal, the
// whole close evidence with who sealed the container and when, moves into the rework's record with who asked for the
// rework, when and why, and the execution carries no close facts while it is in REWORK. The event carries the load unit
// as its item, as the opening's and the cancellation's do; its reason, free text, stays out of it.
func (x *LoadExecution) OpenForRework(c *Consolidation, reason, by string, at time.Time) error {
	if x.state.Kind != ExecutionStuffing || c == nil || c.ID() != x.state.ConsolidationID {
		return fmt.Errorf("%w: only a stuffing is opened for rework, with its consolidation", ErrInvalidExecution)
	}
	if x.state.State != ExecutionClosed {
		return fmt.Errorf("%w: only a sealed container (CLOSED) is opened for rework, this one is %s", ErrInvalidTransition, x.state.State)
	}
	switch s := c.Current().State; s {
	case StateClosed:
	case StateOpen:
		return fmt.Errorf("%w: it is OPEN", ErrNotFrozen)
	default:
		return fmt.Errorf("%w: the consolidation is %s; only a CLOSED one has a container opened for rework", ErrInvalidTransition, s)
	}
	if x.OpenDiscrepancies() > 0 {
		return fmt.Errorf("%w: a loading discrepancy is open on the container; its cargo has left the forwarder's hands", ErrInvalidTransition)
	}
	if len(x.state.CarrierEvidence) > 0 {
		return fmt.Errorf("%w: carrier evidence is recorded on the container; its cargo has left the forwarder's hands", ErrInvalidTransition)
	}
	if !text(reason) {
		return fmt.Errorf("%w: opening a container for rework needs a reason (500 characters)", ErrInvalidExecution)
	}
	if err := x.changeable(by, at); err != nil {
		return err
	}
	st := &x.state
	st.Reworks = append(st.Reworks, Rework{No: len(st.Reworks) + 1, Reason: reason, AskedBy: by, AskedAt: at.UTC(), BrokenSeal: st.CloseEvidence,
		BrokenSealedBy: st.ClosedBy, BrokenSealedAt: st.ClosedAt, BrokenSealedEnteredAt: st.ClosedEnteredAt})
	st.State, st.ClosedBy, st.ClosedAt, st.ClosedEnteredAt, st.CloseEvidence = ExecutionRework, "", time.Time{}, time.Time{}, Evidence{}
	x.record(eventLoadExecutionReworkOpened, at, "", "", st.LoadUnitNo, "")
	return nil
}

// UnloadHouse unloads one whole house from a container in rework: every live line of the house in this execution is
// reversed completely, each as a reversal with the reason UNSTUFFED and the evidence of the warehouse's confirmation,
// in one command; nothing of the house stays in the container. elsewhere is the house's live packs in the
// consolidation's other executions: a house loaded in more than one container is partial unloading, which is refused.
// lots holds the lots of the house's live lines, read under their locks: one that is CLOSED is opened again first, by
// this command and no other (CargoLot.ReopenForRework), and records the rework and the first reversal of its packs. It
// returns how many lines it reversed. The house stays attached to its consolidation with nothing loaded in it; it
// leaves the closed consolidation in a later step. Until then the consolidation cannot be reconciled, since the house's
// loaded packs are fewer than its plan, and the cost basis reads the house with no unit entry. A refused command
// changes neither the execution nor a lot.
func (x *LoadExecution) UnloadHouse(shipment ShipmentID, lots map[CargoLotID]*CargoLot, elsewhere int64, evidence Evidence, by string, at time.Time) (int, error) {
	if x.state.State != ExecutionRework {
		return 0, fmt.Errorf("%w: a house is unloaded for rework from a container in REWORK, this one is %s", ErrNotLoading, x.state.State)
	}
	if !identifier(string(shipment)) {
		return 0, fmt.Errorf("%w: a house to unload", ErrInvalidLoad)
	}
	if x.LiveFor(shipment) == 0 {
		return 0, fmt.Errorf("%w: house %s has no live packs in this container", ErrInvalidLoad, shipment)
	}
	if elsewhere > 0 {
		return 0, fmt.Errorf("%w: house %s has %d packs in another container of the consolidation; unloading part of a house is not built", ErrInvalidLoad,
			shipment, elsewhere)
	}
	if !evidence.valid(at) {
		return 0, ErrInvalidEvidence
	}
	if err := x.changeable(by, at); err != nil {
		return 0, err
	}
	var live []int
	for _, line := range x.state.Lines {
		if line.ShipmentID != shipment {
			continue
		}
		if n, _, _ := x.lineLive(line.No); n == 0 {
			continue
		}
		lot := lots[line.LotID]
		if lot == nil || lot.ID() != line.LotID {
			return 0, fmt.Errorf("%w: line %d is of lot %s, which was not read", ErrInvalidLoad, line.No, line.LotID)
		}
		if err := lot.reopenable(by, at); err != nil {
			return 0, err
		}
		live = append(live, line.No)
	}
	rework := x.state.Reworks[len(x.state.Reworks)-1]
	for _, no := range live {
		line := x.state.Lines[no-1]
		if lot := lots[line.LotID]; lot.state.State == LotClosed {
			lot.reopenForRework(x.state.ExecutionID, rework.No, len(x.state.Reversals)+1, by, at)
		}
		n, _, _ := x.lineLive(no)
		x.reverse(no, n, ReversalUnstuffed, evidence, by, at)
	}
	return len(live), nil
}

// RecordCarrierEvidence records the carrier's status of the tendered pieces, of the master or of a house with
// live packs in the execution. A source reference recorded already with the same content changes nothing and
// returns its number; with other content it is refused. A fact reported with more pieces than tendered on a
// transport, or departed and offloaded reports together exceeding them, is a LOAD_CONFLICT; offloaded pieces
// are an OFFLOADED discrepancy. The evidence is recorded either way (G06-05). It reports the evidence's number
// and whether it was added.
func (x *LoadExecution) RecordCarrierEvidence(e CarrierEvidence, by string, at time.Time) (int, bool, error) {
	if x.state.State == ExecutionRework {
		return 0, false, fmt.Errorf("%w: a container in rework takes no carrier evidence; it is sealed again first", ErrInvalidTransition)
	}
	if x.state.State != ExecutionClosed {
		return 0, false, fmt.Errorf("%w: carrier evidence follows the tender", ErrInvalidTransition)
	}
	if !e.Fact.valid() || e.Pieces < 1 || !transportForm.MatchString(e.Transport) || (e.GrossWeight != (kernel.Quantity{}) &&
		(!e.GrossWeight.Unit.Valid() || e.GrossWeight.Amount < 1)) {
		return 0, false, fmt.Errorf("%w: fact, pieces, weight and flight or voyage", ErrInvalidCarrier)
	}
	if x.state.Mode == ModeAir && e.Fact != CarrierAccepted && !datedFlight.MatchString(e.Transport) {
		return 0, false, fmt.Errorf("%w: an air flight is named with its date as IATA's status messages name it, such as MU587/02DEC (V2.111): "+
			"the same flight number on another day is another flight", ErrInvalidCarrier)
	}
	if e.ShipmentID != "" && x.LiveFor(e.ShipmentID) == 0 {
		return 0, false, fmt.Errorf("%w: house %s has no live packs in the execution", ErrInvalidCarrier, e.ShipmentID)
	}
	if !e.Evidence.valid(at) {
		return 0, false, ErrInvalidEvidence
	}
	e.Evidence.OccurredAt = e.Evidence.OccurredAt.UTC()
	for _, o := range x.state.CarrierEvidence {
		if o.Evidence.Source == e.Evidence.Source && o.Evidence.Reference == e.Evidence.Reference {
			if o.same(e) {
				return o.No, false, nil
			}
			return 0, false, fmt.Errorf("%w: %s %s", ErrEvidenceReused, e.Evidence.Source, e.Evidence.Reference)
		}
	}
	if err := x.changeable(by, at); err != nil {
		return 0, false, err
	}
	e.No, e.By, e.At = len(x.state.CarrierEvidence)+1, by, at.UTC()
	x.state.CarrierEvidence = append(x.state.CarrierEvidence, e)
	x.record(eventCarrierLoadEvidenceRecorded, at, e.ShipmentID, "", e.No, string(e.Fact))
	tendered := x.Tendered()
	if e.ShipmentID != "" {
		tendered = x.LiveFor(e.ShipmentID)
	}
	// A fact's pieces on a transport are its largest report: the same departure reported by the airline and the
	// ground handler, or sent again under another reference, is one departure.
	reported := map[CarrierFact]int64{}
	for _, o := range x.state.CarrierEvidence {
		if o.ShipmentID == e.ShipmentID && o.Transport == e.Transport {
			reported[o.Fact] = max(reported[o.Fact], o.Pieces)
		}
	}
	same, flown := reported[e.Fact], reported[CarrierDeparted]+reported[CarrierOffloaded]
	raise := func(kind LoadingDiscrepancyKind, observed int64) {
		d := LoadingDiscrepancy{No: len(x.state.Discrepancies) + 1, Kind: kind, EvidenceNo: e.No, ShipmentID: e.ShipmentID, Expected: tendered,
			Observed: observed, RaisedAt: at.UTC()}
		x.state.Discrepancies = append(x.state.Discrepancies, d)
		x.record(eventLoadingDiscrepancyRecorded, at, e.ShipmentID, "", d.No, string(kind))
	}
	switch {
	case same > tendered:
		raise(LoadConflict, same)
	case (e.Fact == CarrierDeparted || e.Fact == CarrierOffloaded) && flown > tendered:
		raise(LoadConflict, flown)
	}
	if e.Fact == CarrierOffloaded {
		raise(LoadOffloaded, e.Pieces)
	}
	return e.No, true, nil
}

// ResolveDiscrepancy resolves an open loading discrepancy: ACCEPTED with a reason (the carrier flies the pieces
// on a later flight under the same master, say) or CLAIM_OPENED with the claim's reference.
func (x *LoadExecution) ResolveDiscrepancy(no int, r Resolution, reference, reason string, by string, at time.Time) error {
	if no < 1 || no > len(x.state.Discrepancies) || !x.state.Discrepancies[no-1].Open() {
		return fmt.Errorf("%w: loading discrepancy %d", ErrNotResolvable, no)
	}
	switch {
	case r == ResolvedByAccepted && text(reason) && optionalText(reference):
	case r == ResolvedByClaim && text(reference) && optionalText(reason):
	default:
		return fmt.Errorf("%w: ACCEPTED with a reason or CLAIM_OPENED with a reference", ErrNotResolvable)
	}
	if err := x.changeable(by, at); err != nil {
		return err
	}
	d := &x.state.Discrepancies[no-1]
	d.Resolution, d.Reference, d.Text, d.ResolvedBy, d.ResolvedAt = r, reference, reason, by, at.UTC()
	if no-1 < x.stored[4] {
		x.resolved = append(x.resolved, no-1)
	}
	x.record(eventLoadingDiscrepancyResolved, at, d.ShipmentID, "", no, string(r))
	return nil
}

// Tendered returns the execution's live packs.
func (x *LoadExecution) Tendered() int64 {
	var n int64
	for _, line := range x.state.Lines {
		live, _, _ := x.lineLive(line.No)
		n += live
	}
	return n
}

// LiveFor returns a house's live packs in the execution.
func (x *LoadExecution) LiveFor(shipment ShipmentID) int64 {
	var n int64
	for _, line := range x.state.Lines {
		if line.ShipmentID == shipment {
			live, _, _ := x.lineLive(line.No)
			n += live
		}
	}
	return n
}

// LiveFromLot returns a lot's live packs in the execution.
func (x *LoadExecution) LiveFromLot(lot CargoLotID) int64 {
	var n int64
	for _, line := range x.state.Lines {
		if line.LotID == lot {
			live, _, _ := x.lineLive(line.No)
			n += live
		}
	}
	return n
}

// Placed returns the number of holds placed and not released.
func (x *LoadExecution) Placed() int {
	n := 0
	for _, h := range x.state.Holds {
		if h.Placed() {
			n++
		}
	}
	return n
}

// OpenDiscrepancies returns the number of unresolved loading discrepancies.
func (x *LoadExecution) OpenDiscrepancies() int {
	n := 0
	for _, d := range x.state.Discrepancies {
		if d.Open() {
			n++
		}
	}
	return n
}

func (x *LoadExecution) changeable(by string, at time.Time) error {
	if blank(by) || at.IsZero() || at.Before(x.lastChange()) {
		return fmt.Errorf("%w: author and business time not before the execution's last change", ErrInvalidChange)
	}
	return nil
}

func (x *LoadExecution) lastChange() time.Time {
	st := x.state
	last := later(later(later(st.OpenedAt, st.StartedAt), st.ClosedAt), st.CancelledAt)
	for _, v := range st.Lines {
		last = later(last, v.At)
	}
	for _, v := range st.Reversals {
		last = later(last, v.At)
	}
	for _, v := range st.Holds {
		last = later(later(last, v.At), v.ReleasedAt)
	}
	for _, v := range st.CarrierEvidence {
		last = later(last, v.At)
	}
	for _, v := range st.Discrepancies {
		last = later(later(last, v.RaisedAt), v.ResolvedAt)
	}
	for _, v := range st.Reworks {
		last = later(later(last, v.AskedAt), v.EndedAt)
	}
	return last
}

func (x *LoadExecution) record(kind string, at time.Time, shipment ShipmentID, lot CargoLotID, item int, reason string) {
	x.pending = append(x.pending, Event{Type: kind, At: at.UTC(), TenantID: x.state.TenantID, SubjectID: string(x.state.ExecutionID),
		ConsolidationID: x.state.ConsolidationID, ShipmentID: shipment, ExecutionID: x.state.ExecutionID, LotID: lot, ItemNo: item, Reason: reason})
}

// Stamp records when PHY entered what was added, released or resolved since the execution was loaded (T1).
func (x *LoadExecution) Stamp(entered time.Time) {
	entered = entered.UTC()
	st := &x.state
	stamp := func(t *time.Time, happened time.Time) {
		if t.IsZero() && !happened.IsZero() {
			*t = entered
		}
	}
	stamp(&st.OpenedEnteredAt, st.OpenedAt)
	stamp(&st.StartedEnteredAt, st.StartedAt)
	stamp(&st.ClosedEnteredAt, st.ClosedAt)
	stamp(&st.CancelEnteredAt, st.CancelledAt)
	for i := range st.Reworks {
		stamp(&st.Reworks[i].AskedEnteredAt, st.Reworks[i].AskedAt)
		stamp(&st.Reworks[i].EndedEnteredAt, st.Reworks[i].EndedAt)
	}
	for i := range st.Lines {
		stamp(&st.Lines[i].EnteredAt, st.Lines[i].At)
	}
	for i := range st.Reversals {
		stamp(&st.Reversals[i].EnteredAt, st.Reversals[i].At)
	}
	for i := range st.Holds {
		stamp(&st.Holds[i].EnteredAt, st.Holds[i].At)
		stamp(&st.Holds[i].ReleaseEnteredAt, st.Holds[i].ReleasedAt)
	}
	for i := range st.CarrierEvidence {
		stamp(&st.CarrierEvidence[i].EnteredAt, st.CarrierEvidence[i].At)
	}
	for i := range st.Discrepancies {
		stamp(&st.Discrepancies[i].EnteredAt, st.Discrepancies[i].RaisedAt)
		stamp(&st.Discrepancies[i].ResolutionEnteredAt, st.Discrepancies[i].ResolvedAt)
	}
}

// RestoreLoadExecution rebuilds a stored execution without emitting events.
func RestoreLoadExecution(st LoadExecutionState) (*LoadExecution, error) {
	st.Lines = append([]LoadLine(nil), st.Lines...)
	st.Reversals = append([]Reversal(nil), st.Reversals...)
	st.Holds = append([]Hold(nil), st.Holds...)
	st.CarrierEvidence = append([]CarrierEvidence(nil), st.CarrierEvidence...)
	st.Discrepancies = append([]LoadingDiscrepancy(nil), st.Discrepancies...)
	st.Reworks = append([]Rework(nil), st.Reworks...)
	x := &LoadExecution{state: st, stored: [5]int{len(st.Lines), len(st.Reversals), len(st.Holds), len(st.CarrierEvidence), len(st.Discrepancies)},
		reworks: len(st.Reworks)}
	bad := func(what string) (*LoadExecution, error) { return nil, fmt.Errorf("%w: %s", ErrInvalidExecution, what) }
	if !identifier(string(st.TenantID)) || !identifier(string(st.ExecutionID)) || !identifier(string(st.ConsolidationID)) ||
		(st.Kind != ExecutionTender && st.Kind != ExecutionStuffing) || (st.Kind == ExecutionStuffing) != (st.LoadUnitNo > 0) ||
		st.LoadUnitNo < 0 || (st.Kind == ExecutionStuffing && st.Mode != ModeSea) || !st.Mode.Valid() || !st.Facility.valid() ||
		blank(st.OpenedBy) || st.OpenedAt.IsZero() {
		return bad("identity")
	}
	if st.Kind == ExecutionStuffing && st.State == ExecutionClosed && st.CloseEvidence.Source != EvidenceSeal {
		return bad("a stuffing closed without its seal")
	}
	started, closed := !st.StartedAt.IsZero(), !st.ClosedAt.IsZero()
	if cancelled := st.State == ExecutionCancelled; cancelled != !st.CancelledAt.IsZero() || cancelled != !blank(st.CancelledBy) ||
		cancelled != (st.CancelReason != "") {
		return bad("cancellation")
	}
	switch st.State {
	case ExecutionOpen:
		if started || closed || len(st.Lines) > 0 || len(st.Holds) > 0 {
			return bad("open")
		}
	case ExecutionLoading, ExecutionOnHold:
		if !started || closed || (st.State == ExecutionOnHold) != (x.Placed() > 0) {
			return bad(string(st.State))
		}
	case ExecutionClosed:
		if !started || !closed || x.Placed() > 0 || blank(st.ClosedBy) {
			return bad("closed")
		}
	case ExecutionRework:
		if st.Kind != ExecutionStuffing || !started || closed || x.Placed() > 0 || !blank(st.ClosedBy) || st.CloseEvidence != (Evidence{}) {
			return bad("rework")
		}
	case ExecutionCancelled:
		if st.Kind != ExecutionStuffing || closed || len(st.CarrierEvidence) > 0 || len(st.Discrepancies) > 0 ||
			(started && st.CancelledAt.Before(st.StartedAt)) || st.CancelledAt.Before(st.OpenedAt) {
			return bad("cancelled")
		}
	default:
		return bad("state")
	}
	if len(st.CarrierEvidence) > 0 && st.State != ExecutionClosed {
		return bad("carrier evidence before the tender closed")
	}
	if err := restoreReworks(st); err != nil {
		return bad(err.Error())
	}
	for i, v := range st.Lines {
		if v.No != i+1 || v.Packs < 1 || !identifier(string(v.ShipmentID)) || !identifier(string(v.LotID)) || v.EntryNo < 1 || blank(v.By) ||
			(v.Basis == BasisApportioned) != (v.MeasurementNo > 0) || (v.Basis != BasisApportioned && v.Basis != BasisMeasured) {
			return bad(fmt.Sprintf("line %d", i+1))
		}
	}
	for i, v := range st.Reversals {
		if v.No != i+1 || v.LineNo < 1 || v.LineNo > len(st.Lines) || v.Packs < 1 || !v.Reason.valid() || blank(v.By) {
			return bad(fmt.Sprintf("reversal %d", i+1))
		}
	}
	for _, line := range st.Lines {
		if live, weight, volume := x.lineLive(line.No); live < 0 || weight < 0 || volume < 0 {
			return bad(fmt.Sprintf("line %d reversed beyond its packs", line.No))
		}
	}
	if st.State == ExecutionCancelled && x.Tendered() > 0 {
		return bad("cancelled with live packs")
	}
	for i, v := range st.Holds {
		if v.No != i+1 || (!v.Reason.manual() && v.Reason != HoldScreeningFailed) || (v.Reason == HoldScreeningFailed) != (v.LotID != "") || blank(v.By) {
			return bad(fmt.Sprintf("hold %d", i+1))
		}
	}
	for i, v := range st.CarrierEvidence {
		if v.No != i+1 || !v.Fact.valid() || v.Pieces < 1 || blank(v.By) {
			return bad(fmt.Sprintf("carrier evidence %d", i+1))
		}
	}
	for i, v := range st.Discrepancies {
		if v.No != i+1 || v.EvidenceNo < 1 || v.EvidenceNo > len(st.CarrierEvidence) || v.Open() != v.ResolvedAt.IsZero() {
			return bad(fmt.Sprintf("discrepancy %d", i+1))
		}
	}
	return x, nil
}

// restoreReworks checks a stored execution's reworks against its state: they are numbered from 1; each is asked by
// someone for a reason, after the seal it broke, which is a seal; only the last is open, and exactly when the execution
// is in REWORK; each that ended was sealed again with a new seal, which the next rework broke, or the last was given up
// with the stuffing; and no seal is carried twice. A stuffing that was never reworked has none, and one that was is
// CLOSED only after a rework that sealed it again, and CANCELLED only after one that was given up.
func restoreReworks(st LoadExecutionState) error {
	last := len(st.Reworks) - 1
	if last >= 0 && st.Kind != ExecutionStuffing {
		return fmt.Errorf("a tender has no rework")
	}
	switch {
	case last >= 0 && (st.State == ExecutionOpen || st.State == ExecutionLoading || st.State == ExecutionOnHold):
		return fmt.Errorf("a rework of a container that is %s", st.State)
	case last < 0 && st.State == ExecutionRework:
		return fmt.Errorf("a container in rework with no rework")
	}
	seen := map[string]int{}
	for i, r := range st.Reworks {
		switch {
		case r.No != i+1:
			return fmt.Errorf("rework %d is numbered %d", i+1, r.No)
		case !text(r.Reason) || blank(r.AskedBy) || r.AskedAt.IsZero():
			return fmt.Errorf("rework %d: who asked, when and why", r.No)
		case r.BrokenSeal.Source != EvidenceSeal || blank(r.BrokenSeal.Reference) || r.BrokenSeal.OccurredAt.IsZero() || blank(r.BrokenSealedBy) ||
			r.BrokenSealedAt.IsZero() || r.AskedAt.Before(r.BrokenSealedAt):
			return fmt.Errorf("rework %d: the seal it broke", r.No)
		case seen[sealKey(r.BrokenSeal.Reference)] > 0:
			return fmt.Errorf("rework %d: seal %s was carried before", r.No, r.BrokenSeal.Reference)
		}
		seen[sealKey(r.BrokenSeal.Reference)] = r.No
		if i > 0 && st.Reworks[i-1].NewSeal != r.BrokenSeal.Reference {
			return fmt.Errorf("rework %d breaks a seal other than the one that sealed the container again", r.No)
		}
		ended := r.End != ""
		if ended != (i < last || st.State != ExecutionRework) || (ended && (blank(r.EndedBy) || r.EndedAt.IsZero() || r.EndedAt.Before(r.AskedAt))) ||
			(!ended && (!blank(r.EndedBy) || !r.EndedAt.IsZero() || r.NewSeal != "")) {
			return fmt.Errorf("rework %d: its ending", r.No)
		}
		switch r.End {
		case "":
		case ReworkSealed:
			if blank(r.NewSeal) || seen[sealKey(r.NewSeal)] > 0 {
				return fmt.Errorf("rework %d: its new seal", r.No)
			}
		case ReworkGivenUp:
			if i != last || st.State != ExecutionCancelled || r.NewSeal != "" || r.EndedBy != st.CancelledBy || !r.EndedAt.Equal(st.CancelledAt) {
				return fmt.Errorf("rework %d: it was given up", r.No)
			}
		default:
			return fmt.Errorf("rework %d: how it ended", r.No)
		}
	}
	if last < 0 {
		return nil
	}
	switch l := st.Reworks[last]; st.State {
	case ExecutionClosed:
		if l.End != ReworkSealed || l.NewSeal != st.CloseEvidence.Reference || l.EndedBy != st.ClosedBy || !l.EndedAt.Equal(st.ClosedAt) {
			return fmt.Errorf("a container sealed again whose last rework did not end with that seal")
		}
	case ExecutionCancelled:
		if l.End != ReworkGivenUp {
			return fmt.Errorf("a given up container whose last rework was not given up")
		}
	}
	return nil
}

// State returns a copy of the execution for persistence and read models.
func (x *LoadExecution) State() LoadExecutionState {
	st := x.state
	st.Lines = append([]LoadLine(nil), st.Lines...)
	st.Reversals = append([]Reversal(nil), st.Reversals...)
	st.Holds = append([]Hold(nil), st.Holds...)
	st.CarrierEvidence = append([]CarrierEvidence(nil), st.CarrierEvidence...)
	st.Discrepancies = append([]LoadingDiscrepancy(nil), st.Discrepancies...)
	st.Reworks = append([]Rework(nil), st.Reworks...)
	return st
}

func (x *LoadExecution) ID() LoadExecutionID                   { return x.state.ExecutionID }
func (x *LoadExecution) Consolidation() ConsolidationID        { return x.state.ConsolidationID }
func (x *LoadExecution) Current() ExecutionState               { return x.state.State }
func (x *LoadExecution) Pending() []Event                      { return append([]Event(nil), x.pending...) }
func (x *LoadExecution) Lines() []LoadLine                     { return append([]LoadLine(nil), x.state.Lines...) }
func (x *LoadExecution) LineLive(no int) (int64, int64, int64) { return x.lineLive(no) }

// ExecutionChanges is what a command wrote to an execution since it was loaded, for persistence.
type ExecutionChanges struct {
	Lines         []LoadLine
	Reversals     []Reversal
	Holds         []Hold
	Released      []Hold
	Evidence      []CarrierEvidence
	Discrepancies []LoadingDiscrepancy
	Resolved      []LoadingDiscrepancy
	Reworks       []Rework // opened since the execution was loaded
	EndedReworks  []Rework // stored reworks that ended since
}

// Added returns what was written since the execution was loaded.
func (x *LoadExecution) Added() ExecutionChanges {
	st := x.state
	c := ExecutionChanges{Lines: st.Lines[x.stored[0]:], Reversals: st.Reversals[x.stored[1]:], Holds: st.Holds[x.stored[2]:],
		Evidence: st.CarrierEvidence[x.stored[3]:], Discrepancies: st.Discrepancies[x.stored[4]:]}
	for _, i := range x.released {
		c.Released = append(c.Released, st.Holds[i])
	}
	for _, i := range x.resolved {
		c.Resolved = append(c.Resolved, st.Discrepancies[i])
	}
	c.Reworks = st.Reworks[x.reworks:]
	for _, i := range x.ended {
		c.EndedReworks = append(c.EndedReworks, st.Reworks[i])
	}
	return c
}

// Acknowledge is permitted only after the transaction holding the execution and its Outbox events commits.
func (x *LoadExecution) Acknowledge() {
	st := x.state
	x.pending, x.released, x.resolved, x.ended = nil, nil, nil, nil
	x.stored = [5]int{len(st.Lines), len(st.Reversals), len(st.Holds), len(st.CarrierEvidence), len(st.Discrepancies)}
	x.reworks = len(st.Reworks)
}

// HousePieces are a house's pieces in a tender as the carrier's evidence shows them.
type HousePieces struct {
	Departed  int64
	Arrived   int64
	Offloaded int64
}

// FlightPieces are a house's pieces on one flight or voyage of a tender, as the carrier's evidence shows them (V2.111).
// A flight is its transport as reported: an air flight is written with its date as IATA's status messages write it,
// such as MU587/02DEC, so that the same flight number on another day is another flight.
type FlightPieces struct {
	Transport string
	HousePieces
}

// HousePieces attributes the carrier's evidence to a house of the tender: its pieces on each flight or voyage added up
// (HouseFlights).
func (x *LoadExecution) HousePieces(shipment ShipmentID) HousePieces {
	var out HousePieces
	for _, f := range x.HouseFlights(shipment) {
		out.Departed += f.Departed
		out.Arrived += f.Arrived
		out.Offloaded += f.Offloaded
	}
	return out
}

// HouseFlights attributes the carrier's evidence to a house of the tender, flight by flight in the carrier's order (by
// the earliest time any report on a flight occurred, then the order recorded) since V2.111. On each flight a fact counts
// its largest report (the same departure reported by the airline and the ground handler is one departure). The house's
// own report on a flight counts as it is. Otherwise the master's departure on the flight covers the house when PHY can
// tell which pieces it was: its live pieces less those offloaded from it on the flight when the master's covers the
// tender, or the tender less the pieces offloaded from named houses there (the airline flew all but those); and, when the
// pieces still to fly are only those offloaded from named houses on earlier flights, those of the house, when the
// master's covers them all or the house is the only one with pieces still to fly (up to the master's). A master's
// arrival on a flight covers the house's pieces departed and not yet arrived when it covers the tender, and those
// departed on the flight when it covers all departed on it. A house's own offloads are its offloaded pieces; an offload
// reported for the master alone names no house.
//
// A house's totals are never less than the earlier rule over all flights gives (until V2.111: the house's own reports
// summed over all flights, else the master's summed over all flights when they cover the tender, or the tender less the
// pieces offloaded from named houses): what that rule finds departed beyond the flights' attribution, or arrived up to
// what departed, is put on the flight whose master report completed the cover, since PHY can tell the pieces flew but not
// on which flight. A house's departed and arrived pieces never add up to more than its live pieces. Flights with nothing
// of the house are left out.
func (x *LoadExecution) HouseFlights(shipment ShipmentID) []FlightPieces {
	return x.flights()[shipment]
}

func (x *LoadExecution) flights() map[ShipmentID][]FlightPieces {
	type key struct {
		fact      CarrierFact
		shipment  ShipmentID
		transport string
	}
	type first struct {
		at time.Time
		no int
	}
	firsts := map[string]first{}
	var transports []string
	largest := map[key]int64{}
	for _, e := range x.state.CarrierEvidence {
		if f, seen := firsts[e.Transport]; !seen {
			transports = append(transports, e.Transport)
			firsts[e.Transport] = first{e.Evidence.OccurredAt, e.No}
		} else if e.Evidence.OccurredAt.Before(f.at) {
			firsts[e.Transport] = first{e.Evidence.OccurredAt, f.no}
		}
		k := key{e.Fact, e.ShipmentID, e.Transport}
		largest[k] = max(largest[k], e.Pieces)
	}
	slices.SortStableFunc(transports, func(a, b string) int {
		fa, fb := firsts[a], firsts[b]
		if c := fa.at.Compare(fb.at); c != 0 {
			return c
		}
		return fa.no - fb.no
	})
	var houses []ShipmentID
	live := map[ShipmentID]int64{}
	for _, line := range x.state.Lines {
		if _, seen := live[line.ShipmentID]; !seen {
			if n := x.LiveFor(line.ShipmentID); n > 0 {
				houses = append(houses, line.ShipmentID)
				live[line.ShipmentID] = n
			}
		}
	}
	tendered := x.Tendered()
	departed, arrived, offloaded := map[ShipmentID]int64{}, map[ShipmentID]int64{}, map[ShipmentID]int64{}
	out := map[ShipmentID][]FlightPieces{}
	index := map[ShipmentID]map[string]int{}
	add := func(h ShipmentID, t string, p HousePieces) {
		if p == (HousePieces{}) {
			return
		}
		if index[h] == nil {
			index[h] = map[string]int{}
		}
		if i, ok := index[h][t]; ok {
			f := &out[h][i]
			f.Departed, f.Arrived, f.Offloaded = f.Departed+p.Departed, f.Arrived+p.Arrived, f.Offloaded+p.Offloaded
			return
		}
		index[h][t] = len(out[h])
		out[h] = append(out[h], FlightPieces{Transport: t, HousePieces: p})
	}
	for _, t := range transports {
		master := func(f CarrierFact) int64 { return largest[key{f, "", t}] }
		own := func(f CarrierFact, h ShipmentID) int64 { return largest[key{f, h, t}] }
		var named, waiting, remaining int64
		toFly := map[ShipmentID]int64{}
		var withPieces []ShipmentID
		for _, h := range houses {
			named += own(CarrierOffloaded, h)
			toFly[h] = max(0, min(offloaded[h], live[h]-departed[h]))
			waiting += toFly[h]
			remaining += live[h] - departed[h]
			if toFly[h] > 0 {
				withPieces = append(withPieces, h)
			}
		}
		onlyNamedLeft := waiting > 0 && remaining == waiting
		dep := map[ShipmentID]int64{}
		var depAll int64
		for _, h := range houses {
			m := master(CarrierDeparted)
			switch d := own(CarrierDeparted, h); {
			case d > 0:
				dep[h] = d
			case m <= 0:
			case m >= tendered || (named > 0 && m >= tendered-named):
				dep[h] = live[h] - own(CarrierOffloaded, h)
			case onlyNamedLeft && m >= waiting:
				dep[h] = toFly[h]
			case onlyNamedLeft && len(withPieces) == 1 && withPieces[0] == h:
				dep[h] = min(m, toFly[h])
			}
			dep[h] = max(0, min(dep[h], live[h]-departed[h]))
			depAll += dep[h]
		}
		for _, h := range houses {
			var arr int64
			m := master(CarrierArrived)
			inFlight := departed[h] + dep[h] - arrived[h] // departed, on this flight or before, and not yet arrived
			switch a := own(CarrierArrived, h); {
			case a > 0:
				arr = a
			case m <= 0:
			case m >= tendered:
				arr = inFlight
			case depAll > 0 && m >= depAll:
				arr = dep[h]
			}
			arr = max(0, min(arr, live[h]-arrived[h]))
			off := own(CarrierOffloaded, h)
			departed[h], arrived[h], offloaded[h] = departed[h]+dep[h], arrived[h]+arr, offloaded[h]+off
			add(h, t, HousePieces{Departed: dep[h], Arrived: arr, Offloaded: off})
		}
	}
	// the earlier rule over all flights is a floor: what it finds beyond the flights goes on the flight that completed it
	var namedAll int64
	for _, h := range houses {
		for _, t := range transports {
			namedAll += largest[key{CarrierOffloaded, h, t}]
		}
	}
	completing := func(fact CarrierFact) string {
		var sum int64
		for _, t := range transports {
			sum += largest[key{fact, "", t}]
		}
		threshold := tendered
		if sum < tendered {
			threshold = tendered - namedAll
		}
		var running int64
		for _, t := range transports {
			running += largest[key{fact, "", t}]
			if running > 0 && running >= threshold {
				return t
			}
		}
		return ""
	}
	for _, h := range houses {
		floor := x.wholeTender(h)
		if extra := min(floor.Departed, live[h]) - departed[h]; extra > 0 {
			if t := completing(CarrierDeparted); t != "" {
				add(h, t, HousePieces{Departed: extra})
				departed[h] += extra
			}
		}
		if extra := min(floor.Arrived, departed[h]) - arrived[h]; extra > 0 { // an arrival of the earlier rule counts what departed
			if t := completing(CarrierArrived); t != "" {
				add(h, t, HousePieces{Arrived: extra})
			}
		}
	}
	return out
}

// wholeTender is the rule before V2.111, over all flights at once: the house's own reports, largest per flight, summed;
// else the master's summed, covering all the house's live pieces when they cover the tender, and its live pieces less its
// own offloaded ones when they cover the tender less the pieces offloaded from named houses.
func (x *LoadExecution) wholeTender(shipment ShipmentID) HousePieces {
	type key struct {
		fact      CarrierFact
		shipment  ShipmentID
		transport string
	}
	largest := map[key]int64{}
	for _, e := range x.state.CarrierEvidence {
		k := key{e.Fact, e.ShipmentID, e.Transport}
		largest[k] = max(largest[k], e.Pieces)
	}
	sum := func(fact CarrierFact, of func(ShipmentID) bool) int64 {
		var n int64
		for k, pieces := range largest {
			if k.fact == fact && of(k.shipment) {
				n += pieces
			}
		}
		return n
	}
	house := func(s ShipmentID) bool { return s == shipment }
	master := func(s ShipmentID) bool { return s == "" }
	anyHouse := func(s ShipmentID) bool { return s != "" }
	live, tendered := x.LiveFor(shipment), x.Tendered()
	own := HousePieces{Departed: sum(CarrierDeparted, house), Arrived: sum(CarrierArrived, house), Offloaded: sum(CarrierOffloaded, house)}
	offloaded := sum(CarrierOffloaded, anyHouse)
	cover := func(masterPieces int64) int64 {
		switch {
		case masterPieces <= 0 || live == 0:
			return 0
		case masterPieces >= tendered:
			return live
		case masterPieces >= tendered-offloaded:
			return max(live-own.Offloaded, 0)
		}
		return 0
	}
	out := own
	if out.Departed == 0 {
		out.Departed = cover(sum(CarrierDeparted, master))
	}
	if out.Arrived == 0 {
		out.Arrived = cover(sum(CarrierArrived, master))
	}
	return out
}

// HouseMeasure returns a house's live packs in the tender with their gross weight and volume: lines measured for
// themselves as measured; apportioned lines at their lot's latest measurement, less what the lot's lines measured for
// themselves took, spread over the lot's other packs (the packs that complete the lot take the rest), so a later
// terminal weight counts and a lot never weighs more than its measure. Quantities in different units are converted
// before they are added (WeightSum, VolumeSum: to grams and to cubic centimetres) and before a line measured for itself
// is taken off its lot's measure (lotPart), so with one unit throughout every figure is what it was. A volume that is
// not known for a line (a line with none, an apportioned line of a lot measured without one) makes the house's volume
// not known: it is then zero. A weight is never zero for want of a figure: a zero volume is read as a house with no
// volume, which a house with none has, whereas a zero weight would be read as a weight. It works out the lot's measure
// for this execution alone, which is right when the execution holds all the lot's live packs; a lot loaded in several
// executions is measured over all of them by HouseMeasureAllotted.
func (x *LoadExecution) HouseMeasure(shipment ShipmentID, lots map[CargoLotID]*CargoLot) (int64, kernel.Quantity, kernel.Quantity) {
	return x.houseMeasure(shipment, lots, nil)
}

// HouseMeasureAllotted is HouseMeasure with the apportioned part of each lot's measure taken from a, the allotment of
// the lot over every execution that holds live packs of it (AllotLots). An execution a does not know falls back to
// the measure of the execution alone.
func (x *LoadExecution) HouseMeasureAllotted(shipment ShipmentID, lots map[CargoLotID]*CargoLot, a Allotment) (int64, kernel.Quantity, kernel.Quantity) {
	return x.houseMeasure(shipment, lots, &a)
}

func (x *LoadExecution) houseMeasure(shipment ShipmentID, lots map[CargoLotID]*CargoLot, a *Allotment) (int64, kernel.Quantity, kernel.Quantity) {
	var packs int64
	var weight WeightSum
	var volume VolumeSum
	unknown := false
	addVolume := func(q kernel.Quantity) {
		if q.Unit == "" {
			unknown = true
			return
		}
		volume.Add(q)
	}
	type lotShare struct {
		apportioned, measuredPacks int64
		weights, volumes           []kernel.Quantity
	}
	byLot := map[CargoLotID]*lotShare{}
	var order []CargoLotID // lots in the order their lines were loaded, so that the result never depends on map order
	for _, line := range x.state.Lines {
		if line.ShipmentID != shipment {
			continue
		}
		live, w, v := x.lineLive(line.No)
		if live == 0 {
			continue
		}
		packs += live
		lot, ok := lots[line.LotID]
		_, measured := lot.Measured()
		if ok && measured && lot.Received() > 0 {
			if byLot[line.LotID] == nil {
				byLot[line.LotID], order = &lotShare{}, append(order, line.LotID)
			}
			s := byLot[line.LotID]
			if line.Basis == BasisApportioned {
				s.apportioned += live
				continue
			}
			s.measuredPacks += live
			s.weights = append(s.weights, kernel.Quantity{Unit: line.GrossWeight.Unit, Amount: w})
			s.volumes = append(s.volumes, kernel.Quantity{Unit: line.Volume.Unit, Amount: v})
		}
		weight.Add(kernel.Quantity{Unit: line.GrossWeight.Unit, Amount: w})
		addVolume(kernel.Quantity{Unit: line.Volume.Unit, Amount: v})
	}
	for _, id := range order {
		s := byLot[id]
		if s.apportioned == 0 {
			continue
		}
		lot := lots[id]
		m, _ := lot.Measured()
		w, v, known := kernel.Quantity{}, kernel.Quantity{}, false
		if a != nil {
			w, v, known = a.part(x.state.ExecutionID, id)
		}
		if !known {
			w, v = lotPart(m, lot.Received(), s.measuredPacks, s.weights, s.volumes, s.apportioned)
		}
		weight.Add(w)
		addVolume(v)
	}
	if unknown {
		return packs, weight.Quantity(), kernel.Quantity{}
	}
	return packs, weight.Quantity(), volume.Quantity()
}
