package domain

import (
	"fmt"
	"math/big"
	"slices"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FIN V0.4 (FB3): the consol cost pool of a consolidation under one legal entity. Carrier and co-loader costs of the
// whole consolidation are recorded on the pool and apportioned to the house jobs: by chargeable weight (air, OD-6 A),
// by W/M freight tonnes (LCL), by volume, by pieces, by gross weight, by shipment, or by hand with a reason. FIN V0.5
// (FB4): creditor invoices match the pool's lines; an apportionment divides each line's current cost (what is matched
// plus what is still accrued), a person releases what no invoice will consume, and the pool closes once every line is
// matched final or released and apportioned on the loaded basis at its final cost; it reopens with a reason.

// Pool states.
const (
	PoolOpen   = "OPEN"
	PoolClosed = "CLOSED"
)

// Pool modes, from the PHY consolidation kind.
const (
	PoolModeAir    = "AIR"     // AIR_CONSOL: chargeable weight at 6000 cm³ per kg, rounded up to 0.5 kg
	PoolModeSeaLCL = "SEA_LCL" // SEA_LCL_COLOAD and SEA_LCL_OWN: freight tonnes, 1 m³ per 1000 kg (W/M)
)

// Apportionment methods (OD-6 A: CHARGEABLE is the default of both modes).
const (
	MethodChargeable = "CHARGEABLE"
	MethodVolume     = "VOLUME"
	MethodPieces     = "PIECES"
	MethodManual     = "MANUAL"
	// MethodGrossWeight divides by each house's gross weight in grams (planned at PLANNED, loaded at ACTUAL), as
	// CargoWise's Gross Weight method does.
	MethodGrossWeight = "GROSS_WEIGHT"
	// MethodShipment gives each house that takes part an equal share, as CargoWise's Shipment method does.
	MethodShipment = "SHIPMENT"
	// MethodContainerCount divides by the number of containers each house is in, as CargoWise's Container Count method
	// does. It exists at ACTUAL only: a house's containers are known once it is stuffed.
	MethodContainerCount = "CONTAINER_COUNT"
	// MethodTEU divides by the total TEU of the containers each house is in, as CargoWise's TEU method does, in tenths of
	// a TEU (a 20-foot container is 10, a 40-foot 20). It exists at ACTUAL only.
	MethodTEU = "TEU"
	// MethodRevenue divides by the revenue of the same charge code on each house's freight job, in the functional
	// currency at the estimate rate, as CargoWise's Revenue method does. It reads FIN's own jobs, not PHY, so it exists
	// at both stages.
	MethodRevenue = "REVENUE"
	// MethodRetained keeps a line's cost on the consolidation, with a reason, once PHY reports it cancelled or without
	// a house left to bear it: as CargoWise leaves a consol charge on the consol when it is not apportioned, the cost is
	// the consolidation's own (a dead freight or cancellation charge), not a house's.
	MethodRetained = "RETAINED"
)

// What an apportionment used as its basis: the houses' loaded facts, or the plan scope PHY verified at attach while
// loading evidence is not complete (marked PLANNED, to be re-apportioned once it is).
const (
	BasisActual  = "ACTUAL"
	BasisPlanned = "PLANNED"
)

// Basis quantity units on shares: grams (chargeable under CHARGEABLE in air, gross under GROSS_WEIGHT), cubic
// centimetres, pieces, milli freight tonnes (W/M: one thousandth of a tonne or of a cubic metre, whichever is greater),
// shipments (one per house that takes part), containers, tenths of a TEU and minor units of revenue.
const (
	UnitChargeableGrams = "GRM" // chargeable or gross grams, by the line's method
	UnitShipments       = "SHP"
	UnitCubicCm         = "CMQ"
	UnitPieces          = "PCS"
	UnitMilliFreightTon = "MWM"
	UnitContainers      = "CNT"
	UnitTEUTenths       = "DTE"
	UnitRevenueMinor    = "REV" // minor units of a house job's revenue under the line's charge code
)

// The air chargeable-weight rule recorded on apportionments: 6000 cm³ per kg, rounded up to 500 g.
const (
	AirVolumetricCCPerKg = 6000
	AirRoundStepGrams    = 500
)

var (
	ErrCrossLegalEntity = fmt.Errorf("fin: FIN_CROSS_LEGAL_ENTITY: the consolidation is operated by another legal entity; cross-entity pools come with intercompany charges")
	ErrPoolJobMissing   = fmt.Errorf("fin: FIN_POOL_JOB_MISSING: a house of the consolidation has no open job of the pool's legal entity; open it first")
	ErrPoolUnit         = fmt.Errorf("fin: FIN_POOL_UNIT: a quantity of the basis is in a unit FIN cannot convert (weights GRM, KGM or TNE; volumes CMQ, DMQ or MTQ), or units differ between houses; for TEU, a load unit a house is in that the basis lacks or gives without a TEU")
	ErrPoolNoBasis      = fmt.Errorf("fin: FIN_POOL_NO_BASIS: the method's quantities are zero for every house, or it cannot be worked out on this basis yet; apportion by another method or by hand")
	ErrManualShares     = fmt.Errorf("fin: FIN_MANUAL_SHARES: manual shares name every house of the consolidation exactly once and sum to the line's amount")
	ErrCostVersion      = fmt.Errorf("fin: the cost line's version is not the one expected")
	ErrPoolInWork       = fmt.Errorf("fin: FIN_POOL_IN_WORK: a container of the consolidation is being reworked, so the pool is frozen: no apportionment is previewed or submitted and the pool is not closed; wait for PHY to seal the container again or to give its stuffing up, then preview again")
	ErrPreviewStale     = fmt.Errorf("fin: FIN_PREVIEW_STALE: what the preview was computed on has changed since; preview again and submit the new preview")
	ErrPreviewSubmitted = fmt.Errorf("fin: FIN_PREVIEW_SUBMITTED: the preview was submitted already, and a preview is submitted once; preview again to submit another apportionment")
	ErrConfirmer        = fmt.Errorf("fin: FIN_CONFIRMER_ASKED_REWORK: the apportionment after a rework is not submitted by the person who asked for the rework; another finance user submits it, one who did not ask for an ended rework of these lines that is not yet carried")
)

// HouseFacts is what PhysicalCostBasis v1 tells of one live house: the plan scope PHY verified at attach, and the
// live loaded facts across the consolidation's tenders (zero while nothing is loaded; a volume or weight PHY cannot
// state in one unit is zero).
type HouseFacts struct {
	ShipmentID        string
	Packs             int64
	GrossWeight       kernel.Quantity
	Volume            kernel.Quantity
	LoadedPacks       int64
	LoadedGrossWeight kernel.Quantity
	LoadedVolume      kernel.Quantity
	LastLoadedAt      time.Time
	// Units is what of the house is loaded in each load unit, in unit order: the container methods read its units.
	Units []HouseUnitFacts
}

// HouseUnitFacts is what of one house is loaded in one load unit (container): the live packs of its lines in the
// unit's stuffing, with their weight and volume as PHY measures them.
type HouseUnitFacts struct {
	UnitNo            int
	LoadedPacks       int64
	LoadedGrossWeight kernel.Quantity
	LoadedVolume      kernel.Quantity
}

// LoadUnitClosed is the status PHY gives a load unit whose stuffing is sealed.
const LoadUnitClosed = "CLOSED"

// LoadUnitFacts is what PhysicalCostBasis v1 tells of one load unit (container) that is not removed: its type and TEU
// in tenths, its status (PLANNED, IN_USE or CLOSED) and the state and revision of the record of the stuffing opened on
// it, empty and zero while none is. The revision moves when the stuffing itself is written; it does not see a lot
// measured again, more packs received of a lot or the consolidation reopened (the packing list's digest does). The
// container methods read its TEU; the state and the revision of its stuffing are carried for the checks that come
// next.
type LoadUnitFacts struct {
	UnitNo            int
	TypeCode          string
	TEUTenths         int
	Status            string
	ExecutionID       string
	ExecutionState    string
	ExecutionRevision int64
}

// How a rework of a stuffing ended in PHY, and the execution state of a container opened for rework.
const (
	ReworkSealed    = "SEALED"
	ReworkGivenUp   = "GIVEN_UP"
	ExecutionRework = "REWORK"
)

// ReworkFacts is one rework of a unit's stuffing as PHY reads it: its number, who asked for it, and how it ended (End
// is empty while it is open, SEALED when the container was sealed again, GIVEN_UP when the stuffing was given up) and
// when.
type ReworkFacts struct {
	No      int
	AskedBy string
	End     string
	EndedAt time.Time
}

// UnitPackingListFacts is the identity of what one load unit holds as PHY reads it: the stuffing, its state and the
// digest of its packing list (the lines are the houses' Units), and the reworks of the stuffing. Two reads with the
// same digest read the same unit, except that a rework which unloaded nothing leaves the digest as it was: the reworks
// are read for that. The apportionment's fingerprint and the freeze of the pool read them (since V2.91).
type UnitPackingListFacts struct {
	UnitNo         int
	ExecutionID    string
	ExecutionState string
	Digest         string
	Reworks        []ReworkFacts
}

// FormerHouseFacts is a house that left the consolidation, as PHY reads it (since V2.92): how its entry ended
// (DETACHED, TRANSFERRED or SHIPMENT_CANCELLED, and for a transfer the consolidation it went to), the operator's
// reason, who ended it and when (the business time and when PHY recorded it), and the rework of a sealed container in
// which it was unloaded, when it was (the stuffing, the rework's number, the load unit and who asked for it; empty and
// zero otherwise). The fingerprint reads the shipment, the end, the target, the time and the rework; nothing else reads
// them yet.
type FormerHouseFacts struct {
	ShipmentID        string
	End               string
	TransferredTo     string
	Reason            string
	EndedBy           string
	EndedAt           time.Time
	EndedEnteredAt    time.Time
	ReworkFrom        string // ENTRY, REVERSAL or NONE: where PHY found the rework
	ReworkExecutionID string
	ReworkNo          int
	ReworkUnitNo      int
	ReworkAskedBy     string
}

// GivenUpStuffingFacts is a stuffing of the consolidation given up after a rework, with its reworks as PHY reads them
// (since V2.92). PHY gives it no packing list, so its reworks are not in UnitPackingListFacts.
type GivenUpStuffingFacts struct {
	UnitNo      int
	ExecutionID string
	Reworks     []ReworkFacts
}

// CostBasis is what FIN reads of PHY's PhysicalCostBasis v1: the consolidation, its operating legal entity and its
// live houses. LoadUnits are the load units PHY answers that are not removed, in unit order; the container methods
// read them. FormerHouses and GivenUpStuffings are what V2.92 reads of the houses that left and of the stuffings given
// up.
type CostBasis struct {
	ConsolidationID        string
	Revision               int64
	Kind                   string // AIR_CONSOL, SEA_LCL_COLOAD or SEA_LCL_OWN
	OperatingLegalEntityID string
	State                  string
	Houses                 []HouseFacts
	LoadUnits              []LoadUnitFacts
	PackingLists           []UnitPackingListFacts
	FormerHouses           []FormerHouseFacts
	GivenUpStuffings       []GivenUpStuffingFacts
}

// UnitsInRework is the numbers of the load units whose stuffing is in REWORK, in order, each once.
func (b CostBasis) UnitsInRework() []int {
	var units []int
	for _, u := range b.LoadUnits {
		if u.ExecutionState == ExecutionRework {
			units = append(units, u.UnitNo)
		}
	}
	for _, l := range b.PackingLists {
		if l.ExecutionState == ExecutionRework {
			units = append(units, l.UnitNo)
		}
	}
	slices.Sort(units)
	return slices.Compact(units)
}

// PoolCostVersion is one immutable version of a pool cost line: the creditor (may not be known yet), the amount in
// its currency, the local amount at the ESTIMATE rate, and, from the second version on, the reason.
type PoolCostVersion struct {
	No               int
	CreditorPartyID  string
	CreditorRevision int64
	Amount           kernel.Money
	Local            int64
	FX               *FxSnapshot
	Status           string // ACCRUED, or RELEASED (amount zero) once a person released what no invoice will consume
	Reason           string
	By               string
	At               time.Time
	EnteredAt        time.Time
}

// PoolCostLine is one cost of the whole consolidation: a charge code, the supplier's reference (the MAWB, the
// co-loader or CFS booking) and its versions; Method is its default apportionment method.
type PoolCostLine struct {
	No          int
	ChargeCode  string
	Description string
	SupplierRef string
	Method      string
	Source      string // MANUAL (empty reads as it) or RATED
	BasisID     string // RATED: RATE's CONSOL_COST_ESTIMATE basis and its line
	BasisLineNo int
	By          string
	At          time.Time
	EnteredAt   time.Time
	Versions    []PoolCostVersion
}

// Share is one house job's share of a pool cost line under an apportionment: the basis quantity it was weighted by
// and the share in the line currency and in the functional currency (each conserved by largest remainder). A house
// that took no part in the apportionment (Used false) keeps a share of quantity 0, no unit and amounts 0.
type Share struct {
	No         int
	ShipmentID string
	JobID      string
	Used       bool
	Quantity   int64
	Unit       string
	Amount     int64
	Local      int64
}

// LineApportionment is one line's shares under an apportionment, at the line version it divided: the line's cost then
// (Amount in its currency, Local in the functional currency: what creditor invoices matched plus what was still
// accrued; the version's own figures before FIN V0.5), and its shares.
type LineApportionment struct {
	LineNo      int
	LineVersion int
	Method      string
	Reason      string // the MANUAL reason
	Amount      int64
	Local       int64
	Shares      []Share
	// Attributions are the attributions of the houses that left, under this apportionment (since V2.93): the loss and
	// the exception cost of each, taken out of the line's cost before the shares divide what remains. ToAllocate is
	// what remains when no live house takes part (to be allocated), in the line's currency and in the functional
	// currency.
	Attributions    []Attribution
	ToAllocate      int64
	ToAllocateLocal int64
}

// Apportionment is one immutable division of pool cost lines over the house jobs: the PHY basis revision and stage
// it used, and its lines. A line's current shares are those of the latest apportionment that covers it.
type Apportionment struct {
	No            int
	BasisRevision int64
	Stage         string
	By            string
	At            time.Time
	EnteredAt     time.Time
	Lines         []LineApportionment
	// PreviewNo is the preview this apportionment was submitted from (since V2.91; 0 on one written before).
	PreviewNo int
}

// PoolState is a stored pool, for persistence.
type PoolState struct {
	TenantID           TenantID
	PoolID             string
	ConsolidationID    string
	LegalEntityID      string
	Mode               string
	FunctionalCurrency kernel.CurrencyCode
	BookCode           string
	State              string
	OpenedBy           string
	OpenedAt           time.Time
	EnteredAt          time.Time
	Lines              []PoolCostLine
	Apportionments     []Apportionment
	Previews           []ApportionmentPreview
	Closures           []PoolClosure
}

// PoolClosure is one closing of the pool, reopened at most once with a reason.
type PoolClosure struct {
	No              int
	By              string
	At              time.Time
	EnteredAt       time.Time
	ReopenedBy      string
	ReopenReason    string
	ReopenedAt      time.Time
	ReopenEnteredAt time.Time
}

// ConsolCostPool is the aggregate root of a consolidation's shared costs under one legal entity (UL-08; one pool per
// consolidation and legal entity).
type ConsolCostPool struct {
	state      PoolState
	storedPool storedPool
	consumed   map[int]Consumption         // what creditor invoices consume of each line, set under the pool's lock
	revenue    map[string]map[string]int64 // the houses' revenue by charge code, set under the jobs' locks for the close
	pending    []Event
}

type storedPool struct {
	lines, apportionments, previews, closures int
	versions                                  map[int]int
	reopened                                  []int
}

const (
	eventPoolOpened       = "ConsolCostPoolOpened.v1"
	eventPoolCostRecorded = "ConsolCostRecorded.v1"
	eventPoolCostRevised  = "ConsolCostRevised.v1"
	eventPoolApportioned  = "ConsolCostsApportioned.v1"
	eventAccrualReleased  = "AccrualReleased.v1"
	eventPoolClosed       = "ConsolCostPoolClosed.v1"
	eventPoolReopened     = "ConsolCostPoolReopened.v1"
)

// OpenPool opens the pool of a consolidation under the legal entity's policy. PHY must know the consolidation, its
// kind must be an air consolidation, an LCL co-load or an LCL consolidation in the forwarder's own container
// (SEA_LCL_OWN, which has the pool mode of a co-load), and its operating legal entity must be the pool's (S04,
// cross-entity, is out of scope).
func OpenPool(st PoolState, policy *FinancePolicy, basis CostBasis, by string, at time.Time) (*ConsolCostPool, error) {
	p := policy.State()
	mode := ""
	switch basis.Kind {
	case "AIR_CONSOL":
		mode = PoolModeAir
	case "SEA_LCL_COLOAD", "SEA_LCL_OWN":
		mode = PoolModeSeaLCL
	}
	switch {
	case st.TenantID == "" || !identifier(st.PoolID) || !identifier(st.ConsolidationID) || blank(by) || at.IsZero():
		return nil, fmt.Errorf("%w: a pool names its consolidation", ErrInvalid)
	case basis.ConsolidationID != st.ConsolidationID || mode == "" || basis.State == "CANCELLED":
		return nil, fmt.Errorf("%w: PHY reports consolidation %s %s of kind %q", ErrNotOpenable, st.ConsolidationID, basis.State, basis.Kind)
	case p.TenantID != st.TenantID || p.LegalEntityID != st.LegalEntityID:
		return nil, fmt.Errorf("%w: the policy is another legal entity's", ErrInvalid)
	case basis.OperatingLegalEntityID != st.LegalEntityID:
		return nil, fmt.Errorf("%w: consolidation %s is operated by %s", ErrCrossLegalEntity, st.ConsolidationID, basis.OperatingLegalEntityID)
	}
	st.Mode, st.FunctionalCurrency, st.BookCode, st.State, st.OpenedBy, st.OpenedAt = mode, p.FunctionalCurrency, p.BookCode, PoolOpen, by, at.UTC()
	st.Lines, st.Apportionments, st.Previews, st.Closures = nil, nil, nil, nil
	pool := &ConsolCostPool{state: st, storedPool: storedPool{versions: map[int]int{}}}
	pool.record(eventPoolOpened, at, 0)
	return pool, nil
}

func (p *ConsolCostPool) open(by string, at time.Time) error {
	switch {
	case p.state.State != PoolOpen:
		return fmt.Errorf("%w: the pool is %s", ErrInvalidTransition, p.state.State)
	case blank(by) || at.IsZero():
		return fmt.Errorf("%w: a change names its author and time", ErrInvalid)
	case at.Before(p.lastChange()):
		return fmt.Errorf("%w: business time %s before the pool's last change", ErrInvalid, at.UTC().Format(time.RFC3339))
	}
	return nil
}

func (p *ConsolCostPool) lastChange() time.Time {
	last := p.state.OpenedAt
	for _, l := range p.state.Lines {
		last = later(last, l.At)
		for _, v := range l.Versions {
			last = later(last, v.At)
		}
	}
	for _, a := range p.state.Apportionments {
		last = later(last, a.At)
	}
	for _, c := range p.state.Closures {
		last = later(later(last, c.At), c.ReopenedAt)
	}
	return last
}

// Matched sets what the live matches of creditor invoices consume of the pool's lines, read under the pool's lock
// (lines without matches consume nothing).
func (p *ConsolCostPool) Matched(consumed map[int]Consumption) { p.consumed = consumed }

// Consumed is what creditor invoices consume of a line, as set.
func (p *ConsolCostPool) Consumed(lineNo int) Consumption { return p.consumed[lineNo] }

// LineStatus is a line's accrual status: ACCRUED, PARTLY_MATCHED, MATCHED or RELEASED.
func (p *ConsolCostPool) LineStatus(lineNo int) string {
	versions := p.state.Lines[lineNo-1].Versions
	return AccrualStatus(versions[len(versions)-1].Status, p.consumed[lineNo])
}

// LineCost is a line's current cost in its currency and in the functional currency: what is matched plus what is
// still accrued; once matched final or released, what is matched.
func (p *ConsolCostPool) LineCost(lineNo int) (int64, int64) {
	versions := p.state.Lines[lineNo-1].Versions
	v := versions[len(versions)-1]
	return CurrentCost(v.Amount.Minor, v.Local, v.Status, p.consumed[lineNo])
}

func (p *ConsolCostPool) costVersion(no int, in SideInput, reason string, conv Converter, by string, at time.Time) (PoolCostVersion, error) {
	switch {
	case !in.Amount.Currency.Valid() || in.Amount.Minor < 0 || in.Amount.Minor > MaxAmount:
		return PoolCostVersion{}, fmt.Errorf("%w: a pool cost names its currency and is not negative, at most %d minor units", ErrInvalid, MaxAmount)
	case in.PartyID != "" && (!identifier(in.PartyID) || in.PartyRevision < 1):
		return PoolCostVersion{}, fmt.Errorf("%w: a creditor names its ORG party and revision", ErrInvalid)
	}
	local, fx, err := conv.Local(in.Amount, RateEstimate, at)
	if err != nil {
		return PoolCostVersion{}, err
	}
	if local > MaxAmount {
		return PoolCostVersion{}, fmt.Errorf("%w: a local amount beyond %d minor units", ErrInvalid, MaxAmount)
	}
	return PoolCostVersion{No: no, CreditorPartyID: in.PartyID, CreditorRevision: in.PartyRevision, Amount: in.Amount, Local: local, FX: fx,
		Status: StatusAccrued, Reason: reason, By: by, At: at.UTC()}, nil
}

// RecordCost adds a cost line of the whole consolidation, entered by hand with a reason (RATE's CONSOL_COST_ESTIMATE
// comes later); the creditor may not be known yet. Method is the line's default apportionment method.
func (p *ConsolCostPool) RecordCost(code, description, supplierRef, method string, in SideInput, reason string, conv Converter, by string,
	at time.Time) error {
	if err := p.open(by, at); err != nil {
		return err
	}
	switch {
	case !ValidChargeCode(code) || (description != "" && !text(description)) || !text(reason) || (supplierRef != "" && !text(supplierRef)):
		return fmt.Errorf("%w: a pool cost names its charge code and a reason", ErrInvalid)
	case !validLineMethod(method):
		return fmt.Errorf("%w: apportionment method %q", ErrInvalid, method)
	}
	v, err := p.costVersion(1, in, "", conv, by, at)
	if err != nil {
		return err
	}
	line := PoolCostLine{No: len(p.state.Lines) + 1, ChargeCode: code, Description: description, SupplierRef: supplierRef, Method: method,
		By: by, At: at.UTC(), Versions: []PoolCostVersion{v}}
	p.state.Lines = append(p.state.Lines, line)
	p.record(eventPoolCostRecorded, at, line.No)
	return nil
}

// RecordEstimate adds the cost lines of RATE's rated estimate of the consolidation (CONSOL_COST_ESTIMATE): one RATED
// line per rated cost, for the supplier whose card priced it, ACCRUED at the ESTIMATE rate, apportioned by method. A
// pool takes one rated estimate (FIN_BASIS_APPLIED); a charge RATE could not rate is left to a person.
func (p *ConsolCostPool) RecordEstimate(basisID string, lines []EstimateLine, method string, conv Converter, by string, at time.Time) error {
	if err := p.open(by, at); err != nil {
		return err
	}
	switch {
	case !identifier(basisID):
		return fmt.Errorf("%w: a rated estimate names its basis", ErrInvalid)
	case !validLineMethod(method):
		return fmt.Errorf("%w: apportionment method %q", ErrInvalid, method)
	case len(lines) == 0:
		return fmt.Errorf("%w: RATE rated no cost for the consolidation", ErrNotPriceable)
	}
	for _, l := range p.state.Lines {
		if l.Source == SourceRated {
			return fmt.Errorf("%w: line %d of the pool comes from rated estimate %s", ErrBasisApplied, l.No, l.BasisID)
		}
	}
	for _, l := range lines {
		if !ValidChargeCode(l.ChargeCode) || l.LineNo < 1 || l.HasSell || !l.HasCost || !identifier(l.CreditorPartyID) || l.CreditorRevision < 1 {
			return fmt.Errorf("%w: rated cost %d is a cost of a supplier ORG confirmed", ErrInvalid, l.LineNo)
		}
		v, err := p.costVersion(1, SideInput{PartyID: l.CreditorPartyID, PartyRevision: l.CreditorRevision, Amount: l.Cost}, "", conv, by, at)
		if err != nil {
			return err
		}
		line := PoolCostLine{No: len(p.state.Lines) + 1, ChargeCode: l.ChargeCode, Method: method, Source: SourceRated, BasisID: basisID,
			BasisLineNo: l.LineNo, By: by, At: at.UTC(), Versions: []PoolCostVersion{v}}
		p.state.Lines = append(p.state.Lines, line)
		p.record(eventPoolCostRecorded, at, line.No)
	}
	return nil
}

// ReviseCost writes the next version of a cost line with a reason, while it is neither matched final nor released;
// once creditor invoices consume part of it, the new version stays in their currency and not below what they consume.
func (p *ConsolCostPool) ReviseCost(lineNo, expected int, in SideInput, reason string, conv Converter, by string, at time.Time) error {
	if err := p.open(by, at); err != nil {
		return err
	}
	if lineNo < 1 || lineNo > len(p.state.Lines) || !text(reason) {
		return fmt.Errorf("%w: a revision names a cost line of the pool and a reason", ErrInvalid)
	}
	line := &p.state.Lines[lineNo-1]
	if expected != len(line.Versions) {
		return fmt.Errorf("%w: cost line %d is at version %d", ErrCostVersion, lineNo, len(line.Versions))
	}
	c := p.consumed[lineNo]
	switch status := p.LineStatus(lineNo); {
	case status == StatusMatched || status == StatusReleased:
		return fmt.Errorf("%w: cost line %d is %s", ErrAccrualClosed, lineNo, status)
	case c.Amount > 0 && (in.Amount.Currency != line.Versions[len(line.Versions)-1].Amount.Currency || in.Amount.Minor < c.Amount || !c.names(in.PartyID)):
		return fmt.Errorf("%w: creditor invoices of %v consume %d %s of cost line %d", ErrAccrualMatched, c.Creditors, c.Amount,
			line.Versions[len(line.Versions)-1].Amount.Currency, lineNo)
	}
	v, err := p.costVersion(expected+1, in, reason, conv, by, at)
	if err != nil {
		return err
	}
	line.Versions = append(line.Versions, v)
	p.record(eventPoolCostRevised, at, lineNo)
	return nil
}

// ReleaseCost releases what no creditor invoice will consume of a cost line, with a reason (an approved act: the
// command is authorized on its own): the next version is RELEASED with amount zero in the line's currency, and the
// line's cost is what invoices matched. A line matched final or released already is refused.
func (p *ConsolCostPool) ReleaseCost(lineNo, expected int, reason string, by string, at time.Time) error {
	if err := p.open(by, at); err != nil {
		return err
	}
	if lineNo < 1 || lineNo > len(p.state.Lines) || !text(reason) {
		return fmt.Errorf("%w: a release names a cost line of the pool and a reason", ErrInvalid)
	}
	line := &p.state.Lines[lineNo-1]
	if expected != len(line.Versions) {
		return fmt.Errorf("%w: cost line %d is at version %d", ErrCostVersion, lineNo, len(line.Versions))
	}
	if status := p.LineStatus(lineNo); status == StatusMatched || status == StatusReleased {
		return fmt.Errorf("%w: cost line %d is %s", ErrAccrualClosed, lineNo, status)
	}
	last := line.Versions[len(line.Versions)-1]
	v := PoolCostVersion{No: expected + 1, CreditorPartyID: last.CreditorPartyID, CreditorRevision: last.CreditorRevision,
		Amount: kernel.Money{Currency: last.Amount.Currency}, Status: StatusReleased, Reason: reason, By: by, At: at.UTC()}
	line.Versions = append(line.Versions, v)
	p.pending = append(p.pending, Event{Type: eventAccrualReleased, At: at.UTC(), TenantID: p.state.TenantID, SubjectID: p.state.PoolID,
		LegalEntityID: p.state.LegalEntityID, Version: v.No, ShipmentID: p.state.ConsolidationID, LineNo: lineNo})
	return nil
}

// ManualShare is a hand-given share of one house.
type ManualShare struct {
	ShipmentID string
	Amount     int64
}

// ApportionInput is an apportionment as asked: the lines (every line when empty), a method overriding each line's
// default (or MANUAL with one line, shares and a reason), the houses that take no part in these lines (none named: each
// line keeps the houses its latest apportionment left out, or all take part), and the jobs the application resolved:
// the open job of the pool's legal entity per house shipment.
type ApportionInput struct {
	LineNos []int
	Method  string
	Manual  []ManualShare
	NotUsed []string
	// AllHouses takes every live house into the lines, whatever their latest apportionment left out. Without it and
	// without NotUsed each line keeps the houses its latest apportionment left out.
	AllHouses bool
	Reason    string
	Jobs      map[string]string
	// Revenue is each house job's revenue by charge code at the estimate rate (FreightJob.EstimatedRevenue), per house
	// shipment as Jobs is, read by the application for a line by REVENUE.
	Revenue map[string]map[string]int64
	// Attributions state, per line and house that left, who bears the house's former share (since V2.93). Jobs also
	// names the open job of each house that left, which bears its loss.
	Attributions []AttributionInput
}

// validLineMethod is a method a cost line may carry as its default and an apportionment may name (RETAINED is only
// named by an apportionment).
func validLineMethod(method string) bool {
	switch method {
	case MethodChargeable, MethodVolume, MethodPieces, MethodManual, MethodGrossWeight, MethodShipment, MethodContainerCount, MethodTEU, MethodRevenue:
		return true
	}
	return false
}

// apportionment divides the named cost lines' latest versions over the house jobs by the basis PHY answers: the loaded
// facts when every house that takes part has loading evidence (ACTUAL), the attach plan scope otherwise (PLANNED).
// Shares conserve the line amount and its local amount by largest remainder (ties to the first shipment in order).
// Houses named in NotUsed take no part: the quantities, the amounts and the stage are those of the houses that take
// part, and a house left out keeps a share marked not used with zero amounts. It changes nothing in the pool: Preview
// stores what it returns.
func (p *ConsolCostPool) apportionment(in ApportionInput, basis CostBasis, by string, at time.Time) (Apportionment, error) {
	if in.Method == MethodRetained {
		if len(in.NotUsed) > 0 || in.AllHouses {
			return Apportionment{}, fmt.Errorf("%w: a cost retained on the consolidation has no houses to leave out", ErrInvalid)
		}
		return p.retain(in, basis, by, at)
	}
	// With no live house a line's cost, less the attributions, is to be allocated: that needs a house that left (since
	// V2.93).
	if basis.ConsolidationID != p.state.ConsolidationID || (len(basis.Houses) == 0 && len(basis.FormerHouses) == 0) {
		return Apportionment{}, fmt.Errorf("%w: PHY reports no live houses of consolidation %s", ErrPoolNoBasis, p.state.ConsolidationID)
	}
	lineNos := slices.Clone(in.LineNos)
	if len(lineNos) == 0 {
		for _, l := range p.state.Lines {
			lineNos = append(lineNos, l.No)
		}
	}
	slices.Sort(lineNos)
	if len(lineNos) == 0 || slices.Contains(lineNos, 0) || lineNos[len(lineNos)-1] > len(p.state.Lines) || len(slices.Compact(lineNos)) != len(lineNos) {
		return Apportionment{}, fmt.Errorf("%w: lines of the pool, each once", ErrInvalid)
	}
	houses := slices.Clone(basis.Houses)
	slices.SortFunc(houses, func(a, b HouseFacts) int { return cmpString(a.ShipmentID, b.ShipmentID) })
	if in.AllHouses && len(in.NotUsed) > 0 {
		return Apportionment{}, fmt.Errorf("%w: every house takes part or the houses left out are named, not both", ErrInvalid)
	}
	named, err := notUsedHouses(in.NotUsed, houses)
	if err != nil {
		return Apportionment{}, err
	}
	// each line's houses left out: those named, or with none named (and not AllHouses) those its latest apportionment left
	// out that are still live; the stage is PLANNED while a house that takes part in any of the lines is not loaded
	leftOut := map[int]map[string]bool{}
	stage := BasisActual
	for _, no := range lineNos {
		leftOut[no] = named
		if len(in.NotUsed) == 0 && !in.AllHouses {
			if leftOut[no], err = p.carriedLeftOut(no, houses); err != nil {
				return Apportionment{}, err
			}
		}
		for _, h := range houses {
			if !leftOut[no][h.ShipmentID] && h.LoadedPacks == 0 {
				stage = BasisPlanned
			}
		}
	}
	jobs := make([]string, len(houses))
	for i, h := range houses {
		if jobs[i] = in.Jobs[h.ShipmentID]; jobs[i] == "" {
			return Apportionment{}, fmt.Errorf("%w: house %s", ErrPoolJobMissing, h.ShipmentID)
		}
	}
	attributed, err := p.attribute(in, basis, lineNos)
	if err != nil {
		return Apportionment{}, err
	}
	a := Apportionment{No: len(p.state.Apportionments) + 1, BasisRevision: basis.Revision, Stage: stage, By: by, At: at.UTC()}
	for _, no := range lineNos {
		line := p.state.Lines[no-1]
		v := line.Versions[len(line.Versions)-1]
		cost, local := p.LineCost(no)
		method := line.Method
		if in.Method != "" {
			method = in.Method
		}
		la := LineApportionment{LineNo: no, LineVersion: v.No, Method: method, Amount: cost, Local: local, Attributions: attributed[no].rows}
		// what remains after the attributions is divided over the houses that take part
		remainder, remainderLocal := attributed[no].remainder, attributed[no].remainderLocal
		notUsed := leftOut[no]
		var taking []HouseFacts // the houses that take part in this line, in shipment order
		for _, h := range houses {
			if !notUsed[h.ShipmentID] {
				taking = append(taking, h)
			}
		}
		if len(taking) == 0 {
			// no house is live: the remainder is to be allocated on the line, with no share
			if method == MethodManual {
				if len(lineNos) != 1 || !text(in.Reason) || len(in.Manual) > 0 {
					return Apportionment{}, fmt.Errorf("%w: a manual apportionment divides one line with a reason, and no share when no house is live", ErrInvalid)
				}
				la.Reason = in.Reason
			}
			if len(la.Attributions) == 0 && remainder == 0 {
				return Apportionment{}, fmt.Errorf("%w: cost line %d costs nothing and no house is live: it needs no apportionment and does not hold back the close; "+
					"preview the other lines", ErrInvalid, no)
			}
			la.ToAllocate, la.ToAllocateLocal = remainder, remainderLocal
			a.Lines = append(a.Lines, la)
			continue
		}
		var quantities []int64
		var unit string
		if method == MethodManual {
			if len(lineNos) != 1 || !text(in.Reason) {
				return Apportionment{}, fmt.Errorf("%w: a manual apportionment divides one line with a reason", ErrInvalid)
			}
			la.Reason = in.Reason
			if quantities, err = manualQuantities(taking, in.Manual, remainder); err != nil {
				return Apportionment{}, err
			}
			unit = ""
		} else if method == MethodRevenue {
			if quantities, err = revenueQuantities(line.ChargeCode, taking, in.Revenue); err != nil {
				return Apportionment{}, err
			}
			unit = UnitRevenueMinor
		} else {
			if quantities, unit, err = methodQuantities(method, p.state.Mode, stage, taking, basis.LoadUnits); err != nil {
				return Apportionment{}, err
			}
		}
		amounts, err := split(remainder, quantities)
		if err != nil {
			return Apportionment{}, err
		}
		locals, err := split(remainderLocal, quantities)
		if err != nil {
			return Apportionment{}, err
		}
		k := 0 // the next house that takes part
		for i, h := range houses {
			share := Share{No: i + 1, ShipmentID: h.ShipmentID, JobID: jobs[i]}
			if !notUsed[h.ShipmentID] {
				share.Used, share.Quantity, share.Unit, share.Amount, share.Local = true, quantities[k], unit, amounts[k], locals[k]
				k++
			}
			la.Shares = append(la.Shares, share)
		}
		a.Lines = append(a.Lines, la)
	}
	return a, nil
}

// carriedLeftOut is the houses a line's latest apportionment left out that are still live houses of the consolidation
// (none for a line not apportioned yet, one retained, or one that used every house): the line keeps them out until
// another list is named or AllHouses is asked. Not every live house may be among them.
func (p *ConsolCostPool) carriedLeftOut(lineNo int, houses []HouseFacts) (map[string]bool, error) {
	out := map[string]bool{}
	_, la, ok := p.SharesOfLine(lineNo)
	if !ok {
		return out, nil
	}
	for _, s := range la.Shares {
		if !s.Used && slices.ContainsFunc(houses, func(h HouseFacts) bool { return h.ShipmentID == s.ShipmentID }) {
			out[s.ShipmentID] = true
		}
	}
	if len(out) == len(houses) && len(houses) > 0 {
		return nil, fmt.Errorf("%w: every house still live was left out of line %d; name the houses that take part or ask for all", ErrInvalid, lineNo)
	}
	return out, nil
}

// notUsedHouses reads the houses an apportionment leaves out: each a live house once, and not every house.
func notUsedHouses(named []string, houses []HouseFacts) (map[string]bool, error) {
	out := map[string]bool{}
	for _, id := range named {
		switch {
		case out[id]:
			return nil, fmt.Errorf("%w: house %s is left out twice", ErrInvalid, id)
		case !slices.ContainsFunc(houses, func(h HouseFacts) bool { return h.ShipmentID == id }):
			return nil, fmt.Errorf("%w: %s is not a live house of the consolidation", ErrInvalid, id)
		}
		out[id] = true
	}
	if len(out) == len(houses) && len(houses) > 0 {
		return nil, fmt.Errorf("%w: every house is left out", ErrInvalid)
	}
	return out, nil
}

// retain keeps the named lines' costs (every line when none is named) on the consolidation, with a reason: allowed
// only once PHY reports the consolidation cancelled or without a house left, so that while houses bear a consolidation
// its costs reach their jobs. The lines' current costs are recorded and no share is written; a later apportionment
// supersedes it as any other. Like any apportionment it is previewed first and submitted.
func (p *ConsolCostPool) retain(in ApportionInput, basis CostBasis, by string, at time.Time) (Apportionment, error) {
	lineNos := slices.Clone(in.LineNos)
	if len(lineNos) == 0 {
		for _, l := range p.state.Lines {
			lineNos = append(lineNos, l.No)
		}
	}
	slices.Sort(lineNos)
	switch {
	case basis.ConsolidationID != p.state.ConsolidationID || basis.Revision < 1:
		return Apportionment{}, fmt.Errorf("%w: PHY answered consolidation %s for %s", ErrInvalid, basis.ConsolidationID, p.state.ConsolidationID)
	case basis.State != "CANCELLED" && len(basis.Houses) > 0:
		return Apportionment{}, fmt.Errorf("%w: a cost is retained on the consolidation only once PHY reports it cancelled or without houses; %s has %d",
			ErrInvalid, p.state.ConsolidationID, len(basis.Houses))
	case !text(in.Reason) || len(in.Manual) > 0:
		return Apportionment{}, fmt.Errorf("%w: a cost retained on the consolidation needs a reason and takes no shares", ErrInvalid)
	case len(lineNos) == 0 || slices.Contains(lineNos, 0) || lineNos[len(lineNos)-1] > len(p.state.Lines) || len(slices.Compact(lineNos)) != len(lineNos):
		return Apportionment{}, fmt.Errorf("%w: lines of the pool, each once", ErrInvalid)
	}
	attributed, err := p.attribute(in, basis, lineNos)
	if err != nil {
		return Apportionment{}, err
	}
	a := Apportionment{No: len(p.state.Apportionments) + 1, BasisRevision: basis.Revision, Stage: BasisActual, By: by, At: at.UTC()}
	for _, no := range lineNos {
		versions := p.state.Lines[no-1].Versions
		cost, local := p.LineCost(no)
		a.Lines = append(a.Lines, LineApportionment{LineNo: no, LineVersion: versions[len(versions)-1].No, Method: MethodRetained, Reason: in.Reason,
			Amount: cost, Local: local, Attributions: attributed[no].rows})
	}
	return a, nil
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// manualQuantities reads hand-given shares: every house that takes part exactly once, non-negative, summing to the
// line amount; the shares are their own weights, so largest remainder reproduces them exactly.
func manualQuantities(houses []HouseFacts, manual []ManualShare, amount int64) ([]int64, error) {
	if len(manual) != len(houses) {
		return nil, fmt.Errorf("%w: %d shares for %d houses", ErrManualShares, len(manual), len(houses))
	}
	byShipment := map[string]int64{}
	var sum int64
	for _, m := range manual {
		if _, twice := byShipment[m.ShipmentID]; twice || m.Amount < 0 {
			return nil, fmt.Errorf("%w: share of %s", ErrManualShares, m.ShipmentID)
		}
		byShipment[m.ShipmentID] = m.Amount
		sum += m.Amount
	}
	if sum != amount {
		return nil, fmt.Errorf("%w: shares sum to %d, the line is %d", ErrManualShares, sum, amount)
	}
	out := make([]int64, len(houses))
	for i, h := range houses {
		q, ok := byShipment[h.ShipmentID]
		if !ok {
			return nil, fmt.Errorf("%w: no share of %s", ErrManualShares, h.ShipmentID)
		}
		out[i] = q
	}
	return out, nil
}

// methodQuantities computes each house's basis quantity for a method at a stage. units are the basis's load units,
// which the container methods look up.
func methodQuantities(method, mode, stage string, houses []HouseFacts, units []LoadUnitFacts) ([]int64, string, error) {
	if method == MethodContainerCount || method == MethodTEU {
		return containerQuantities(method, stage, houses, units)
	}
	out := make([]int64, len(houses))
	unit := ""
	var sum int64
	for i, h := range houses {
		packs, weight, volume := h.Packs, h.GrossWeight, h.Volume
		if stage == BasisActual {
			packs, weight, volume = h.LoadedPacks, h.LoadedGrossWeight, h.LoadedVolume
		}
		var err error
		switch method {
		case MethodPieces:
			out[i], unit = packs, UnitPieces
		case MethodShipment:
			out[i], unit = 1, UnitShipments
		case MethodGrossWeight:
			if out[i], err = grams(weight); err != nil {
				return nil, "", err
			}
			unit = UnitChargeableGrams
		case MethodVolume:
			if out[i], err = cubicCm(volume); err != nil {
				return nil, "", err
			}
			unit = UnitCubicCm
		case MethodChargeable:
			g, err := grams(weight)
			if err != nil {
				return nil, "", err
			}
			cc, err := cubicCm(volume)
			if err != nil {
				return nil, "", err
			}
			if mode == PoolModeAir {
				out[i], unit = chargeableGrams(g, cc), UnitChargeableGrams
			} else {
				out[i], unit = freightMilliTonnes(g, cc), UnitMilliFreightTon
			}
		default:
			return nil, "", fmt.Errorf("%w: apportionment method %q", ErrInvalid, method)
		}
		sum += out[i]
	}
	if sum == 0 {
		return nil, "", fmt.Errorf("%w: %s at the %s stage", ErrPoolNoBasis, method, stage)
	}
	return out, unit, nil
}

// revenueQuantities is each house's revenue under a charge code, in minor units of the functional currency at the
// estimate rate, from the jobs' EstimatedRevenue by house shipment. A house without revenue under the code is zero and
// gets a zero share; when none has any the basis is refused. A cost is never divided on a negative weight: a negative
// revenue (a quote's discount line under the code can make the code's sum negative) refuses the apportionment, naming
// the house.
func revenueQuantities(code string, houses []HouseFacts, revenue map[string]map[string]int64) ([]int64, error) {
	out := make([]int64, len(houses))
	var sum int64
	for i, h := range houses {
		if out[i] = revenue[h.ShipmentID][code]; out[i] < 0 {
			return nil, fmt.Errorf("%w: house %s has a negative revenue (%d) under charge code %s; a cost is not divided on a negative weight",
				ErrPoolNoBasis, h.ShipmentID, out[i], code)
		}
		sum += out[i]
	}
	if sum == 0 {
		return nil, fmt.Errorf("%w: no house that takes part has revenue under charge code %s", ErrPoolNoBasis, code)
	}
	return out, nil
}

// HouseRevenue sets each house job's revenue by charge code at the estimate rate (FreightJob.EstimatedRevenue), per
// house shipment, read under the jobs' locks: Close compares the lines apportioned by REVENUE against it.
func (p *ConsolCostPool) HouseRevenue(revenue map[string]map[string]int64) { p.revenue = revenue }

// ByRevenue tells whether a line's latest apportionment is by REVENUE: the close then needs the houses' revenue.
func (p *ConsolCostPool) ByRevenue() bool {
	for _, l := range p.state.Lines {
		if _, la, ok := p.SharesOfLine(l.No); ok && la.Method == MethodRevenue {
			return true
		}
	}
	return false
}

// containerQuantities is each house's container count (CONTAINER_COUNT, unit CNT) or the tenths of TEU of its
// containers (TEU, unit DTE). A house's containers are the distinct load units of its Units entries that hold loaded
// packs of it (an entry without loaded packs is not a container the house is in; PHY gives none). PHY gives the entries
// only once the house is stuffed, so these methods exist at ACTUAL only, and no plan stands in for them at PLANNED
// (no plan says which house goes into which container).
//
// At ACTUAL every load unit of the basis must also be sealed (status LoadUnitClosed): while a unit is planned or
// loading, a house part-loaded into the sealed ones would be counted in fewer containers than it ends up in, and the
// figure would stay wrong once the pool is closed. The houses' containers are final once every container of the
// consolidation is sealed. A unit that was planned and never opened is removed in PHY first. A stuffing opened on a
// unit and left empty is given up in PHY (CancelLoadExecution), and the unit is then removed or stuffed again. This
// does not require that a house is wholly loaded: packs of a house left outside every sealed container count for
// nothing, and whether they still travel is PHY's matter. The same rule holds at the close comparison, which calls this
// function.
//
// A house that takes part and is loaded but has no entry while others have has no container: its quantity is 0 and it
// gets a zero share, as a house with no volume does under VOLUME. When no house has one (air, or a co-load loaded by
// tender) every quantity is zero and the basis is refused as FIN_POOL_NO_BASIS. PHY's contract gives every unit of an
// entry among its load units, with a positive TEU; a basis that does not is inconsistent and is refused for TEU as
// FIN_POOL_UNIT, never divided on silently. CONTAINER_COUNT does not read the tenths.
func containerQuantities(method, stage string, houses []HouseFacts, units []LoadUnitFacts) ([]int64, string, error) {
	if stage != BasisActual {
		return nil, "", fmt.Errorf("%w: %s needs the houses' containers, which are known once a house is stuffed", ErrPoolNoBasis, method)
	}
	for _, u := range units {
		if u.Status != LoadUnitClosed {
			return nil, "", fmt.Errorf("%w: %s needs every container sealed: load unit %d is %s; a container still being stuffed is sealed in PHY, a stuffing opened and left empty is given up in PHY (CancelLoadExecution), a unit never opened is removed in PHY",
				ErrPoolNoBasis, method, u.UnitNo, u.Status)
		}
	}
	out := make([]int64, len(houses))
	var sum int64
	for i, h := range houses {
		seen := map[int]bool{}
		for _, u := range h.Units {
			if seen[u.UnitNo] || u.LoadedPacks <= 0 {
				continue
			}
			seen[u.UnitNo] = true
			if method == MethodContainerCount {
				out[i]++
				continue
			}
			at := slices.IndexFunc(units, func(l LoadUnitFacts) bool { return l.UnitNo == u.UnitNo })
			if at < 0 || units[at].TEUTenths <= 0 {
				return nil, "", fmt.Errorf("%w: house %s is in load unit %d, which PHY's basis does not give with a TEU", ErrPoolUnit, h.ShipmentID, u.UnitNo)
			}
			out[i] += int64(units[at].TEUTenths)
		}
		sum += out[i]
	}
	if sum == 0 {
		return nil, "", fmt.Errorf("%w: %s at the %s stage", ErrPoolNoBasis, method, stage)
	}
	if method == MethodContainerCount {
		return out, UnitContainers, nil
	}
	return out, UnitTEUTenths, nil
}

// Grams converts a weight into grams and CubicCm a volume into cubic centimetres (GRM, KGM, TNE; CMQ, DMQ, MTQ).
func Grams(q kernel.Quantity) (int64, error)   { return grams(q) }
func CubicCm(q kernel.Quantity) (int64, error) { return cubicCm(q) }

// grams converts a weight into grams; a zero quantity is zero.
func grams(q kernel.Quantity) (int64, error) {
	switch {
	case q.Amount == 0:
		return 0, nil
	case q.Amount < 0:
		return 0, fmt.Errorf("%w: weight %d %s", ErrPoolUnit, q.Amount, q.Unit)
	}
	switch q.Unit {
	case "GRM":
		return q.Amount, nil
	case "KGM":
		return mulInRange(q.Amount, 1000)
	case "TNE":
		return mulInRange(q.Amount, 1_000_000)
	}
	return 0, fmt.Errorf("%w: weight in %s", ErrPoolUnit, q.Unit)
}

// cubicCm converts a volume into cubic centimetres; a zero quantity is zero.
func cubicCm(q kernel.Quantity) (int64, error) {
	switch {
	case q.Amount == 0:
		return 0, nil
	case q.Amount < 0:
		return 0, fmt.Errorf("%w: volume %d %s", ErrPoolUnit, q.Amount, q.Unit)
	}
	switch q.Unit {
	case "CMQ":
		return q.Amount, nil
	case "DMQ":
		return mulInRange(q.Amount, 1000)
	case "MTQ":
		return mulInRange(q.Amount, 1_000_000)
	}
	return 0, fmt.Errorf("%w: volume in %s", ErrPoolUnit, q.Unit)
}

func mulInRange(a, factor int64) (int64, error) {
	if a > (1<<62)/factor {
		return 0, fmt.Errorf("%w: quantity %d overflows", ErrInvalid, a)
	}
	return a * factor, nil
}

// chargeableGrams is the air chargeable weight in grams: the greater of the gross weight and the volumetric weight
// at 6000 cm³ per kg, rounded up to 500 g (the IATA half-kilo step).
func chargeableGrams(g, cc int64) int64 {
	volumetric := ceilDiv(cc, AirVolumetricCCPerKg/1000) // cm³ to grams: ÷6
	return ceilDiv(max(g, volumetric), AirRoundStepGrams) * AirRoundStepGrams
}

// freightMilliTonnes is the LCL W/M freight measure in milli units: the greater of the weight in kilograms (one
// thousandth of a tonne) and the volume in litres (one thousandth of a cubic metre), each rounded up.
func freightMilliTonnes(g, cc int64) int64 {
	return max(ceilDiv(g, 1000), ceilDiv(cc, 1000))
}

func ceilDiv(a, b int64) int64 { return (a + b - 1) / b }

// split divides amount in proportion to the quantities, conserving it exactly: each share is the floor of its exact
// proportion, and the minor units left go to the largest remainders (ties to the first). Quantities are non-negative
// and not all zero; a zero amount yields zero shares.
func split(amount int64, quantities []int64) ([]int64, error) {
	if amount < 0 {
		return nil, fmt.Errorf("%w: a negative amount is never apportioned", ErrInvalid)
	}
	total := new(big.Int)
	for _, q := range quantities {
		if q < 0 {
			return nil, fmt.Errorf("%w: a negative quantity", ErrInvalid)
		}
		total.Add(total, big.NewInt(q))
	}
	if total.Sign() == 0 {
		return nil, ErrPoolNoBasis
	}
	out := make([]int64, len(quantities))
	remainders := make([]*big.Int, len(quantities))
	rest := amount
	for i, q := range quantities {
		product := new(big.Int).Mul(big.NewInt(amount), big.NewInt(q))
		share, remainder := new(big.Int).QuoRem(product, total, new(big.Int))
		out[i] = share.Int64() // share ≤ amount ≤ MaxAmount
		remainders[i] = remainder
		rest -= out[i]
	}
	order := make([]int, len(quantities))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return remainders[b].Cmp(remainders[a]) })
	// the floors leave less than one unit per share, so rest < len(out)
	for i := 0; int64(i) < rest; i++ {
		out[order[i]]++
	}
	return out, nil
}

// Close closes the pool once its costs are final: every line matched with a final match or released, by creditor
// invoices neither disputed nor awaiting a variance approval; and every line's latest apportionment divided its final
// cost on PHY's current basis with every house it used loaded (ACTUAL), naming exactly the live houses and, for a
// weighed method, the used houses' current quantities; a house it left out is compared for its presence only. For a
// line by REVENUE the quantities are the used houses' revenue under the line's charge code now (HouseRevenue), and the
// close is refused when any differs from what the apportionment recorded. The stage rule is the same for every
// method: a line apportioned by REVENUE at the stage PLANNED must be apportioned again once every house that takes part
// is loaded before the pool can close. A line that holds a former share of a house that left, which no apportionment
// has attributed, or an amount still to be allocated, is not closed, whatever PHY reports (since V2.93); an exception
// cost whose party is PENDING does not stop it. A line whose final cost is zero needs no apportionment (or one of
// zero). Once PHY cancelled the consolidation or no house is left on it, there is no basis to compare: the latest
// apportionments at the final costs stand. The pool then takes no change until reopened.
func (p *ConsolCostPool) Close(basis CostBasis, by string, at time.Time) error {
	if err := p.open(by, at); err != nil {
		return err
	}
	houses, gone := housesInOrder(basis)
	if basis.ConsolidationID != p.state.ConsolidationID {
		return fmt.Errorf("%w: PHY answered consolidation %s for %s", ErrInvalid, basis.ConsolidationID, p.state.ConsolidationID)
	}
	if err := inWork(basis); err != nil {
		return err
	}
	for _, l := range p.state.Lines {
		if _, err := p.accrualRefusal(l); err != nil {
			return err
		}
		if _, err := p.lineRefusal(l, basis, houses, gone); err != nil {
			return err
		}
	}
	c := PoolClosure{No: len(p.state.Closures) + 1, By: by, At: at.UTC()}
	p.state.Closures, p.state.State = append(p.state.Closures, c), PoolClosed
	p.record(eventPoolClosed, at, c.No)
	return nil
}

// accrualRefusal is what the close would refuse a cost line for in its accrual: it is not matched final or released, or
// an invoice matched to it is disputed or awaits a variance approval. It answers the kind of what it found
// (COST_NOT_FINAL or INVOICE_HELD, as PendingItems names them) with the close's own error, and nothing when the line
// passes. Close and PendingItems both read the line through it, as they do through lineRefusal.
func (p *ConsolCostPool) accrualRefusal(l PoolCostLine) (string, error) {
	status := p.LineStatus(l.No)
	switch {
	case status != StatusMatched && status != StatusReleased:
		return pendingCostNotFinal, fmt.Errorf("%w: cost line %d is %s", ErrPoolNotClosable, l.No, status)
	case p.consumed[l.No].Held:
		return pendingInvoiceHeld, fmt.Errorf("%w: a creditor invoice matched to cost line %d is disputed or awaits a variance approval", ErrPoolNotClosable, l.No)
	}
	return "", nil
}

// housesInOrder is the basis's live houses in shipment order, and whether the close has no basis to compare: PHY
// cancelled the consolidation or no house is left.
func housesInOrder(basis CostBasis) ([]HouseFacts, bool) {
	houses := slices.Clone(basis.Houses)
	slices.SortFunc(houses, func(a, b HouseFacts) int { return cmpString(a.ShipmentID, b.ShipmentID) })
	return houses, basis.State == "CANCELLED" || len(houses) == 0
}

// lineRefusal is what the close would refuse a cost line for, apart from its accrual and its creditor invoices: the
// first of a line that is not apportioned (or apportioned at another cost), an amount still to be allocated, a former
// share nobody attributed, and a line that is not on the current loaded basis (houses is the basis's live houses in
// shipment order, gone that PHY cancelled the consolidation or no house is left). It answers the kind of what it found
// (one of the pending kinds of PendingItems) with the close's own error, and nothing when the line passes. Close and
// PendingItems both read the line through it, so that the list never says what the close would not.
func (p *ConsolCostPool) lineRefusal(l PoolCostLine, basis CostBasis, houses []HouseFacts, gone bool) (string, error) {
	cost, local := p.LineCost(l.No)
	a, la, ok := p.SharesOfLine(l.No)
	switch {
	case !ok && (cost != 0 || local != 0):
		return pendingLineNotApportioned, fmt.Errorf("%w: cost line %d is not apportioned", ErrPoolNotClosable, l.No)
	case !ok:
		return "", nil
	case la.Amount != cost || la.Local != local:
		return pendingLineNotApportioned, fmt.Errorf("%w: cost line %d was apportioned at %d (%d local); its final cost is %d (%d local)", ErrPoolNotClosable, l.No, la.Amount,
			la.Local, cost, local)
	case la.ToAllocate != 0 || la.ToAllocateLocal != 0:
		return pendingToBeAllocated, fmt.Errorf("%w: cost line %d has %d to be allocated, as no house of the consolidation is live; add cargo and apportion the line again, or "+
			"attribute it as a loss or an exception cost of a house that left, or cancel the consolidation and retain it", ErrPoolNotClosable, l.No, la.ToAllocate)
	case len(formerSharesOf(la, basis)) > 0:
		return pendingFormerShare, fmt.Errorf("%w: cost line %d holds a former share of house %s that left the consolidation and that nobody has attributed; attribute it by "+
			"apportioning the line again with its loss and its exception cost", ErrPoolNotClosable, l.No, formerSharesOf(la, basis)[0].ShipmentID)
	case la.Method == MethodRetained && !gone && (cost != 0 || local != 0):
		return pendingLineNotOnBasis, fmt.Errorf("%w: cost line %d is retained on the consolidation while PHY reports houses on it", ErrPoolNotClosable, l.No)
	case cost == 0 && local == 0, gone:
		return "", nil // every share is zero, or no basis is left to compare
	case a.Stage != BasisActual || len(la.Shares) != len(houses) || !usedLoaded(la.Shares, houses):
		return pendingLineNotOnBasis, fmt.Errorf("%w: cost line %d is not apportioned on the current loaded basis", ErrPoolNotClosable, l.No)
	}
	var taking []HouseFacts // the houses the apportionment used
	for i, h := range houses {
		if la.Shares[i].ShipmentID != h.ShipmentID {
			return pendingLineNotOnBasis, fmt.Errorf("%w: cost line %d is not apportioned on the current loaded basis (house %s)", ErrPoolNotClosable, l.No, h.ShipmentID)
		}
		if la.Shares[i].Used {
			taking = append(taking, h)
		}
	}
	var quantities []int64
	var unit string
	if la.Method == MethodRevenue {
		// A house the jobs do not answer cannot be compared, and counts as changed. No command reaches this: a used
		// share is on an open job, and the commit trigger check_job_pool_shares_at_commit (fin/000012) refuses
		// cancelling a job that holds a current used share, so the open jobs of the houses that take part are always
		// there. It is kept as a safeguard, so that a close never passes a house it could not compare.
		for _, s := range la.Shares {
			if now, known := p.revenue[s.ShipmentID]; s.Used && (!known || s.Quantity != now[l.ChargeCode] || s.Unit != UnitRevenueMinor) {
				return pendingLineNotOnBasis, fmt.Errorf("%w: cost line %d: the houses' revenue under charge code %s has changed since it was apportioned (house %s); "+
					"apportion it again", ErrPoolNotClosable, l.No, l.ChargeCode, s.ShipmentID)
			}
		}
	} else if la.Method != MethodManual {
		var err error
		if quantities, unit, err = methodQuantities(la.Method, p.state.Mode, BasisActual, taking, basis.LoadUnits); err != nil {
			return pendingLineNotOnBasis, fmt.Errorf("%w: cost line %d: %w", ErrPoolNotClosable, l.No, err)
		}
	}
	k := 0 // the next house used
	for _, s := range la.Shares {
		if !s.Used {
			continue // compared for its presence only
		}
		if quantities != nil && (s.Quantity != quantities[k] || s.Unit != unit) {
			return pendingLineNotOnBasis, fmt.Errorf("%w: cost line %d is not apportioned on the current loaded basis (house %s)", ErrPoolNotClosable, l.No, s.ShipmentID)
		}
		k++
	}
	return "", nil
}

// usedLoaded tells that every house a line's shares used has loaded packs on PHY's current basis (a share is read
// against the house at its place; the caller compares the names).
func usedLoaded(shares []Share, houses []HouseFacts) bool {
	for i, s := range shares {
		if s.Used && i < len(houses) && houses[i].LoadedPacks == 0 {
			return false
		}
	}
	return true
}

// Reopen reopens a CLOSED pool with a reason, such as a creditor invoice arriving late (OD-10 A).
func (p *ConsolCostPool) Reopen(reason, by string, at time.Time) error {
	switch {
	case p.state.State != PoolClosed:
		return fmt.Errorf("%w: the pool is %s", ErrInvalidTransition, p.state.State)
	case !text(reason) || blank(by) || at.IsZero() || at.Before(p.lastChange()):
		return fmt.Errorf("%w: a reopening names its reason, author and a time not before the pool's last change", ErrInvalid)
	}
	c := &p.state.Closures[len(p.state.Closures)-1]
	c.ReopenedBy, c.ReopenReason, c.ReopenedAt = by, reason, at.UTC()
	if c.No <= p.storedPool.closures {
		p.storedPool.reopened = append(p.storedPool.reopened, c.No)
	}
	p.state.State = PoolOpen
	p.record(eventPoolReopened, at, c.No)
	return nil
}

// SharesOfLine is the latest apportionment's shares of a cost line, with the apportionment they come from; ok is
// false while the line is not apportioned.
func (p *ConsolCostPool) SharesOfLine(lineNo int) (Apportionment, LineApportionment, bool) {
	for i := len(p.state.Apportionments) - 1; i >= 0; i-- {
		for _, la := range p.state.Apportionments[i].Lines {
			if la.LineNo == lineNo {
				return p.state.Apportionments[i], la, true
			}
		}
	}
	return Apportionment{}, LineApportionment{}, false
}

func (p *ConsolCostPool) record(kind string, at time.Time, version int) {
	p.pending = append(p.pending, Event{Type: kind, At: at.UTC(), TenantID: p.state.TenantID, SubjectID: p.state.PoolID,
		LegalEntityID: p.state.LegalEntityID, Version: version, ShipmentID: p.state.ConsolidationID})
}

// Stamp records when FIN entered what was written since the pool was loaded (T1).
func (p *ConsolCostPool) Stamp(entered time.Time) {
	e := entered.UTC()
	st := &p.state
	if st.EnteredAt.IsZero() {
		st.EnteredAt = e
	}
	for i := range st.Lines {
		if st.Lines[i].EnteredAt.IsZero() {
			st.Lines[i].EnteredAt = e
		}
		for k := range st.Lines[i].Versions {
			if st.Lines[i].Versions[k].EnteredAt.IsZero() {
				st.Lines[i].Versions[k].EnteredAt = e
			}
		}
	}
	for i := range st.Previews {
		if st.Previews[i].EnteredAt.IsZero() {
			st.Previews[i].EnteredAt = e
		}
	}
	for i := range st.Apportionments {
		if st.Apportionments[i].EnteredAt.IsZero() {
			st.Apportionments[i].EnteredAt = e
		}
	}
	for i := range st.Closures {
		c := &st.Closures[i]
		if c.EnteredAt.IsZero() {
			c.EnteredAt = e
		}
		if !c.ReopenedAt.IsZero() && c.ReopenEnteredAt.IsZero() {
			c.ReopenEnteredAt = e
		}
	}
}

// RestorePool rebuilds a stored pool.
func RestorePool(st PoolState) (*ConsolCostPool, error) {
	st.Lines = slices.Clone(st.Lines)
	st.Apportionments = slices.Clone(st.Apportionments)
	st.Closures = slices.Clone(st.Closures)
	st.Previews = slices.Clone(st.Previews)
	s := storedPool{lines: len(st.Lines), apportionments: len(st.Apportionments), previews: len(st.Previews), closures: len(st.Closures), versions: map[int]int{}}
	for i, c := range st.Closures {
		if c.No != i+1 || (i < len(st.Closures)-1 && c.ReopenedAt.IsZero()) {
			return nil, fmt.Errorf("%w: closure %d of pool %s", ErrInvalid, i+1, st.PoolID)
		}
	}
	if closed := len(st.Closures) > 0 && st.Closures[len(st.Closures)-1].ReopenedAt.IsZero(); closed != (st.State == PoolClosed) ||
		(st.State != PoolOpen && st.State != PoolClosed) {
		return nil, fmt.Errorf("%w: pool %s is %s", ErrInvalid, st.PoolID, st.State)
	}
	for i := range st.Lines {
		l := &st.Lines[i]
		l.Versions = append([]PoolCostVersion(nil), l.Versions...)
		if l.No != i+1 || len(l.Versions) == 0 {
			return nil, fmt.Errorf("%w: pool cost line %d", ErrInvalid, i+1)
		}
		for k, v := range l.Versions {
			if v.No != k+1 {
				return nil, fmt.Errorf("%w: version of pool cost line %d", ErrInvalid, i+1)
			}
		}
		s.versions[l.No] = len(l.Versions)
	}
	for i := range st.Previews {
		v := &st.Previews[i]
		v.Lines = cloneLines(v.Lines)
		if v.No != i+1 {
			return nil, fmt.Errorf("%w: preview %d of pool %s", ErrInvalid, i+1, st.PoolID)
		}
	}
	for i := range st.Apportionments {
		a := &st.Apportionments[i]
		a.Lines = append([]LineApportionment(nil), a.Lines...)
		if a.No != i+1 || a.PreviewNo < 0 || a.PreviewNo > len(st.Previews) {
			return nil, fmt.Errorf("%w: apportionment %d", ErrInvalid, i+1)
		}
		for k := range a.Lines {
			a.Lines[k] = cloneApportionedLine(a.Lines[k])
			if a.Lines[k].LineNo < 1 || a.Lines[k].LineNo > len(st.Lines) {
				return nil, fmt.Errorf("%w: apportionment %d of line %d", ErrInvalid, a.No, a.Lines[k].LineNo)
			}
		}
	}
	return &ConsolCostPool{state: st, storedPool: s}, nil
}

// State returns a copy for persistence and read models.
func (p *ConsolCostPool) State() PoolState {
	st := p.state
	st.Lines = slices.Clone(st.Lines)
	for i := range st.Lines {
		st.Lines[i].Versions = append([]PoolCostVersion(nil), st.Lines[i].Versions...)
	}
	st.Apportionments = slices.Clone(st.Apportionments)
	for i := range st.Apportionments {
		st.Apportionments[i].Lines = append([]LineApportionment(nil), st.Apportionments[i].Lines...)
		for k := range st.Apportionments[i].Lines {
			st.Apportionments[i].Lines[k] = cloneApportionedLine(st.Apportionments[i].Lines[k])
		}
	}
	st.Previews = slices.Clone(st.Previews)
	for i := range st.Previews {
		st.Previews[i].Lines = cloneLines(st.Previews[i].Lines)
	}
	st.Closures = slices.Clone(st.Closures)
	return st
}

// Added returns what was written since the pool was loaded: new lines, new versions of any line, and new
// apportionments.
func (p *ConsolCostPool) Added() (lines []PoolCostLine, versions []PoolCostVersion, lineOf []int, apportionments []Apportionment) {
	st := p.State()
	lines = st.Lines[p.storedPool.lines:]
	for _, l := range st.Lines {
		for _, v := range l.Versions[min(p.storedPool.versions[l.No], len(l.Versions)):] {
			versions, lineOf = append(versions, v), append(lineOf, l.No)
		}
	}
	return lines, versions, lineOf, st.Apportionments[p.storedPool.apportionments:]
}

// AddedPreviews returns the previews written since the pool was loaded.
func (p *ConsolCostPool) AddedPreviews() []ApportionmentPreview {
	return p.State().Previews[p.storedPool.previews:]
}

// AddedClosures returns the closures written since the pool was loaded and the stored ones reopened since.
func (p *ConsolCostPool) AddedClosures() (closures, reopened []PoolClosure) {
	st := p.State()
	for _, no := range p.storedPool.reopened {
		reopened = append(reopened, st.Closures[no-1])
	}
	return st.Closures[p.storedPool.closures:], reopened
}

func (p *ConsolCostPool) Pending() []Event { return append([]Event(nil), p.pending...) }

// Acknowledge is permitted only after the transaction holding the pool and its Outbox events commits.
func (p *ConsolCostPool) Acknowledge() {
	p.pending = nil
	p.storedPool = storedPool{lines: len(p.state.Lines), apportionments: len(p.state.Apportionments), previews: len(p.state.Previews),
		closures: len(p.state.Closures), versions: map[int]int{}}
	for _, l := range p.state.Lines {
		p.storedPool.versions[l.No] = len(l.Versions)
	}
}
