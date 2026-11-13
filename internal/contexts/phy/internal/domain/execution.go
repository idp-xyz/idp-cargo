package domain

import (
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"time"

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
	released []int  // persisted holds released since, by index
	resolved []int  // persisted discrepancies resolved since, by index
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
	eventCarrierLoadEvidenceRecorded = "CarrierLoadEvidenceRecorded.v1"
	eventLoadingDiscrepancyRecorded  = "LoadingDiscrepancyRecorded.v1"
	eventLoadingDiscrepancyResolved  = "LoadingDiscrepancyResolved.v1"
)

var transportForm = regexp.MustCompile(`^[0-9A-Z][0-9A-Z /._-]{0,34}$`)

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
// frozen) at a facility of its origin, the forwarder's CFS. A unit takes one stuffing: taken reports whether an
// execution was opened on it already. Business time is not before the consolidation's latest change (its close).
func OpenStuffing(tenant TenantID, id LoadExecutionID, c *Consolidation, unitNo int, facility Facility, taken bool, by string, at time.Time) (*LoadExecution, error) {
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
	x := &LoadExecution{state: LoadExecutionState{TenantID: tenant, ExecutionID: id, ConsolidationID: c.ID(), Kind: ExecutionStuffing, Mode: c.Mode(),
		Facility: facility, LoadUnitNo: unitNo, State: ExecutionOpen, OpenedBy: by, OpenedAt: at.UTC()}}
	x.record(eventLoadExecutionOpened, at, "", "", unitNo, "")
	return x, nil
}

// Start starts loading.
func (x *LoadExecution) Start(by string, at time.Time) error {
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

// HoldIfOverLimits holds a loading stuffing whose container, its tare and its live cargo, weighs more than its
// type's default maximum gross, or whose live cargo takes more than the type's capacity. The cargo is measured as
// PHY publishes it (HouseMeasure): lines measured for themselves as measured, apportioned lines from their lot's
// latest measurement; volumes not known are not counted. lots holds the lots of the stuffing's lines. A weight or
// volume in a unit PHY does not convert holds it too, as not checked. The line or measurement that crossed the
// limits stays recorded (G06-05); an OVERWEIGHT hold already placed is not placed again. The limits are defaults:
// the container's plate (SEA's) may allow more, which releasing the hold with its evidence records, and a later
// line or measurement over them holds again. It reports whether it placed a hold; the hold takes the business time
// at, or the execution's last change if that is later.
func (x *LoadExecution) HoldIfOverLimits(c *Consolidation, lots map[CargoLotID]*CargoLot, evidence Evidence, by string, at time.Time) bool {
	if x.state.Kind != ExecutionStuffing || (x.state.State != ExecutionLoading && x.state.State != ExecutionOnHold) || c == nil ||
		c.ID() != x.state.ConsolidationID || x.state.LoadUnitNo > len(c.state.LoadUnits) || blank(by) || at.IsZero() {
		return false
	}
	for _, h := range x.state.Holds {
		if h.Placed() && h.Reason == HoldOverweight {
			return false
		}
	}
	l := c.state.LoadUnits[x.state.LoadUnitNo-1].Limits
	grams, cubicCm, unchecked := x.unitCargo(lots)
	gross := addCapped(grams, scaled(l.TareKg, 1000))
	var text string
	switch {
	case unchecked != "":
		text = fmt.Sprintf("unit %d (%s): limits not checked, a measure in %s", x.state.LoadUnitNo, l.TypeCode, unchecked)
	case gross > scaled(l.MaxGrossKg, 1000):
		text = fmt.Sprintf("unit %d (%s): tare and cargo %s kg over the default maximum gross %d kg (edition %s)", x.state.LoadUnitNo, l.TypeCode,
			decimal(gross, 3), l.MaxGrossKg, l.Edition)
	case cubicCm > l.CapacityCM3:
		text = fmt.Sprintf("unit %d (%s): cargo %s m3 over the default capacity %s m3 (edition %s)", x.state.LoadUnitNo, l.TypeCode,
			decimal(cubicCm, 6), decimal(l.CapacityCM3, 6), l.Edition)
	default:
		return false
	}
	x.hold(Hold{Reason: HoldOverweight, Text: text, Evidence: evidence}, by, later(at, x.lastChange()))
	return true
}

// unitCargo is the stuffing's live cargo in grams and cubic centimetres, by HouseMeasure's rule over every house, and
// the first unit it could not convert.
func (x *LoadExecution) unitCargo(lots map[CargoLotID]*CargoLot) (grams, cubicCm int64, unchecked kernel.UnitCode) {
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
	type lotShare struct{ apportioned, measuredPacks, measuredWeight, measuredVolume int64 }
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
				s.measuredPacks, s.measuredWeight, s.measuredVolume = s.measuredPacks+live, s.measuredWeight+w, s.measuredVolume+v
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
		rest := lots[id].Received() - s.measuredPacks
		w, v := max(m.GrossWeight.Amount-s.measuredWeight, 0), max(m.Volume.Amount-s.measuredVolume, 0)
		if s.apportioned < rest {
			w, v = share(w, s.apportioned, rest), share(v, s.apportioned, rest)
		}
		add(&grams, kernel.Quantity{Unit: m.GrossWeight.Unit, Amount: w}, toGrams)
		if m.Volume != (kernel.Quantity{}) {
			add(&cubicCm, kernel.Quantity{Unit: m.Volume.Unit, Amount: v}, toCubicCm)
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
	case ExecutionClosed:
		if x.state.Kind == ExecutionStuffing {
			return fmt.Errorf("%w: a sealed container is unstuffed once it can be sealed again", ErrNotLoading)
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
	live, weight, volume := x.lineLive(lineNo)
	if packs < 1 || packs > live {
		return fmt.Errorf("%w: %d packs of line %d, %d live", ErrInvalidLoad, packs, lineNo, live)
	}
	if !evidence.valid(at) {
		return ErrInvalidEvidence
	}
	if err := x.changeable(by, at); err != nil {
		return err
	}
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
	return nil
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
// with cargo in the container, while its consolidation c is CLOSED: a container is sealed on a frozen house list.
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
	if x.state.State == ExecutionLoading && evidence.valid(at) && evidence.Source != EvidenceSeal {
		return fmt.Errorf("%w: a stuffing closes with the container's seal (source %s)", ErrInvalidEvidence, EvidenceSeal)
	}
	if x.state.State == ExecutionLoading && x.Tendered() == 0 {
		return fmt.Errorf("%w: nothing is stuffed in the container", ErrInvalidLoad)
	}
	return x.close(evidence, by, at)
}

func (x *LoadExecution) close(evidence Evidence, by string, at time.Time) error {
	if x.state.State == ExecutionOnHold {
		return ErrOnHold
	}
	if x.state.State != ExecutionLoading {
		return ErrInvalidTransition
	}
	if !evidence.valid(at) {
		return ErrInvalidEvidence
	}
	if err := x.changeable(by, at); err != nil {
		return err
	}
	evidence.OccurredAt = evidence.OccurredAt.UTC()
	x.state.State, x.state.ClosedBy, x.state.ClosedAt, x.state.CloseEvidence = ExecutionClosed, by, at.UTC(), evidence
	x.record(eventLoadExecutionClosed, at, "", "", 0, "")
	return nil
}

// RecordCarrierEvidence records the carrier's status of the tendered pieces, of the master or of a house with
// live packs in the execution. A source reference recorded already with the same content changes nothing and
// returns its number; with other content it is refused. A fact reported with more pieces than tendered on a
// transport, or departed and offloaded reports together exceeding them, is a LOAD_CONFLICT; offloaded pieces
// are an OFFLOADED discrepancy. The evidence is recorded either way (G06-05). It reports the evidence's number
// and whether it was added.
func (x *LoadExecution) RecordCarrierEvidence(e CarrierEvidence, by string, at time.Time) (int, bool, error) {
	if x.state.State != ExecutionClosed {
		return 0, false, fmt.Errorf("%w: carrier evidence follows the tender", ErrInvalidTransition)
	}
	if !e.Fact.valid() || e.Pieces < 1 || !transportForm.MatchString(e.Transport) || (e.GrossWeight != (kernel.Quantity{}) &&
		(!e.GrossWeight.Unit.Valid() || e.GrossWeight.Amount < 1)) {
		return 0, false, fmt.Errorf("%w: fact, pieces, weight and flight or voyage", ErrInvalidCarrier)
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
	last := later(later(st.OpenedAt, st.StartedAt), st.ClosedAt)
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
	x := &LoadExecution{state: st, stored: [5]int{len(st.Lines), len(st.Reversals), len(st.Holds), len(st.CarrierEvidence), len(st.Discrepancies)}}
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
	default:
		return bad("state")
	}
	if len(st.CarrierEvidence) > 0 && st.State != ExecutionClosed {
		return bad("carrier evidence before the tender closed")
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

// State returns a copy of the execution for persistence and read models.
func (x *LoadExecution) State() LoadExecutionState {
	st := x.state
	st.Lines = append([]LoadLine(nil), st.Lines...)
	st.Reversals = append([]Reversal(nil), st.Reversals...)
	st.Holds = append([]Hold(nil), st.Holds...)
	st.CarrierEvidence = append([]CarrierEvidence(nil), st.CarrierEvidence...)
	st.Discrepancies = append([]LoadingDiscrepancy(nil), st.Discrepancies...)
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
	return c
}

// Acknowledge is permitted only after the transaction holding the execution and its Outbox events commits.
func (x *LoadExecution) Acknowledge() {
	st := x.state
	x.pending, x.released, x.resolved = nil, nil, nil
	x.stored = [5]int{len(st.Lines), len(st.Reversals), len(st.Holds), len(st.CarrierEvidence), len(st.Discrepancies)}
}

// HousePieces are a house's pieces in a tender as the carrier's evidence shows them.
type HousePieces struct {
	Departed  int64
	Arrived   int64
	Offloaded int64
}

// HousePieces attributes the carrier's evidence to a house of the tender. Each fact counts its largest report
// per flight or voyage, summed over flights (a house split over two flights; N1 has direct flights only). A
// house's own reports count as they are. Otherwise a master report covers the house: all its live pieces when
// the master's pieces cover the whole tender, its live pieces less its own offloaded ones when they cover the
// tender less the pieces offloaded from named houses, and none when PHY cannot tell which houses they were (an
// offload reported for the master alone names no house).
func (x *LoadExecution) HousePieces(shipment ShipmentID) HousePieces {
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
// themselves as measured; apportioned lines at their lot's latest measurement, less what the lot's lines measured
// for themselves took, spread over the lot's other packs (the packs that complete the lot take the rest), so a
// later terminal weight counts and a lot never weighs more than its measure. A volume not known for every
// line, or in different units, is not known: the volume is then zero.
func (x *LoadExecution) HouseMeasure(shipment ShipmentID, lots map[CargoLotID]*CargoLot) (int64, kernel.Quantity, kernel.Quantity) {
	var packs int64
	var weight, volume kernel.Quantity
	unknown := false
	addVolume := func(unit kernel.UnitCode, amount int64) {
		switch {
		case unit == "":
			unknown = true
		case volume.Unit == "" || volume.Unit == unit:
			volume = kernel.Quantity{Unit: unit, Amount: volume.Amount + amount}
		default:
			unknown = true
		}
	}
	type lotShare struct{ apportioned, measuredPacks, measuredWeight, measuredVolume int64 }
	byLot := map[CargoLotID]*lotShare{}
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
				byLot[line.LotID] = &lotShare{}
			}
			s := byLot[line.LotID]
			if line.Basis == BasisApportioned {
				s.apportioned += live
				continue
			}
			s.measuredPacks, s.measuredWeight, s.measuredVolume = s.measuredPacks+live, s.measuredWeight+w, s.measuredVolume+v
		}
		weight = kernel.Quantity{Unit: line.GrossWeight.Unit, Amount: weight.Amount + w}
		addVolume(line.Volume.Unit, v)
	}
	for id, s := range byLot {
		if s.apportioned == 0 {
			continue
		}
		lot := lots[id]
		m, _ := lot.Measured()
		rest := lot.Received() - s.measuredPacks
		w, v := max(m.GrossWeight.Amount-s.measuredWeight, 0), max(m.Volume.Amount-s.measuredVolume, 0)
		if s.apportioned < rest {
			w, v = share(w, s.apportioned, rest), share(v, s.apportioned, rest)
		}
		weight = kernel.Quantity{Unit: m.GrossWeight.Unit, Amount: weight.Amount + w}
		addVolume(m.Volume.Unit, v)
	}
	if unknown {
		volume = kernel.Quantity{}
	}
	return packs, weight, volume
}
