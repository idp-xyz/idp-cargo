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
// by W/M freight tonnes (LCL), by volume, by pieces, or by hand with a reason. FIN V0.5 (FB4): creditor invoices match
// the pool's lines; an apportionment divides each line's current cost (what is matched plus what is still accrued), a
// person releases what no invoice will consume, and the pool closes once every line is matched final or released and
// apportioned on the loaded basis at its final cost; it reopens with a reason.

// Pool states.
const (
	PoolOpen   = "OPEN"
	PoolClosed = "CLOSED"
)

// Pool modes, from the PHY consolidation kind.
const (
	PoolModeAir    = "AIR"     // AIR_CONSOL: chargeable weight at 6000 cm³ per kg, rounded up to 0.5 kg
	PoolModeSeaLCL = "SEA_LCL" // SEA_LCL_COLOAD: freight tonnes, 1 m³ per 1000 kg (W/M)
)

// Apportionment methods (OD-6 A: CHARGEABLE is the default of both modes).
const (
	MethodChargeable = "CHARGEABLE"
	MethodVolume     = "VOLUME"
	MethodPieces     = "PIECES"
	MethodManual     = "MANUAL"
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

// Basis quantity units on shares: chargeable grams, cubic centimetres, pieces, milli freight tonnes (W/M: one
// thousandth of a tonne or of a cubic metre, whichever is greater).
const (
	UnitChargeableGrams = "GRM"
	UnitCubicCm         = "CMQ"
	UnitPieces          = "PCS"
	UnitMilliFreightTon = "MWM"
)

// The air chargeable-weight rule recorded on apportionments: 6000 cm³ per kg, rounded up to 500 g.
const (
	AirVolumetricCCPerKg = 6000
	AirRoundStepGrams    = 500
)

var (
	ErrCrossLegalEntity = fmt.Errorf("fin: FIN_CROSS_LEGAL_ENTITY: the consolidation is operated by another legal entity; cross-entity pools come with intercompany charges")
	ErrPoolJobMissing   = fmt.Errorf("fin: FIN_POOL_JOB_MISSING: a house of the consolidation has no open job of the pool's legal entity; open it first")
	ErrPoolUnit         = fmt.Errorf("fin: FIN_POOL_UNIT: a quantity of the basis is in a unit FIN cannot convert (weights GRM, KGM or TNE; volumes CMQ, DMQ or MTQ), or units differ between houses")
	ErrPoolNoBasis      = fmt.Errorf("fin: FIN_POOL_NO_BASIS: the method's quantities are zero for every house; apportion by another method or by hand")
	ErrManualShares     = fmt.Errorf("fin: FIN_MANUAL_SHARES: manual shares name every house of the consolidation exactly once and sum to the line's amount")
	ErrCostVersion      = fmt.Errorf("fin: the cost line's version is not the one expected")
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
}

// CostBasis is what FIN reads of PHY's PhysicalCostBasis v1: the consolidation, its operating legal entity and its
// live houses.
type CostBasis struct {
	ConsolidationID        string
	Revision               int64
	Kind                   string // AIR_CONSOL or SEA_LCL_COLOAD
	OperatingLegalEntityID string
	State                  string
	Houses                 []HouseFacts
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
// and the share in the line currency and in the functional currency (each conserved by largest remainder).
type Share struct {
	No         int
	ShipmentID string
	JobID      string
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
	consumed   map[int]Consumption // what creditor invoices consume of each line, set under the pool's lock
	pending    []Event
}

type storedPool struct {
	lines, apportionments, closures int
	versions                        map[int]int
	reopened                        []int
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
// kind must be an air consolidation or an LCL co-load, and its operating legal entity must be the pool's (S04,
// cross-entity, is out of scope).
func OpenPool(st PoolState, policy *FinancePolicy, basis CostBasis, by string, at time.Time) (*ConsolCostPool, error) {
	p := policy.State()
	mode := ""
	switch basis.Kind {
	case "AIR_CONSOL":
		mode = PoolModeAir
	case "SEA_LCL_COLOAD":
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
	st.Lines, st.Apportionments, st.Closures = nil, nil, nil
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
	case method != MethodChargeable && method != MethodVolume && method != MethodPieces && method != MethodManual:
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
	case method != MethodChargeable && method != MethodVolume && method != MethodPieces && method != MethodManual:
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
// default (or MANUAL with one line, shares and a reason), and the jobs the application resolved: the open job of the
// pool's legal entity per house shipment.
type ApportionInput struct {
	LineNos []int
	Method  string
	Manual  []ManualShare
	Reason  string
	Jobs    map[string]string
}

// Apportion divides the named cost lines' latest versions over the house jobs by the basis PHY answers: the loaded
// facts when every house has loading evidence (ACTUAL), the attach plan scope otherwise (PLANNED). Shares conserve
// the line amount and its local amount by largest remainder (ties to the first shipment in order). A new
// apportionment supersedes an earlier one line by line.
func (p *ConsolCostPool) Apportion(in ApportionInput, basis CostBasis, by string, at time.Time) error {
	if err := p.open(by, at); err != nil {
		return err
	}
	if in.Method == MethodRetained {
		return p.retain(in, basis, by, at)
	}
	if basis.ConsolidationID != p.state.ConsolidationID || len(basis.Houses) == 0 {
		return fmt.Errorf("%w: PHY reports no live houses of consolidation %s", ErrPoolNoBasis, p.state.ConsolidationID)
	}
	lineNos := slices.Clone(in.LineNos)
	if len(lineNos) == 0 {
		for _, l := range p.state.Lines {
			lineNos = append(lineNos, l.No)
		}
	}
	slices.Sort(lineNos)
	if len(lineNos) == 0 || slices.Contains(lineNos, 0) || lineNos[len(lineNos)-1] > len(p.state.Lines) || len(slices.Compact(lineNos)) != len(lineNos) {
		return fmt.Errorf("%w: lines of the pool, each once", ErrInvalid)
	}
	houses := slices.Clone(basis.Houses)
	slices.SortFunc(houses, func(a, b HouseFacts) int { return cmpString(a.ShipmentID, b.ShipmentID) })
	stage := BasisActual
	for _, h := range houses {
		if h.LoadedPacks == 0 {
			stage = BasisPlanned
			break
		}
	}
	jobs := make([]string, len(houses))
	for i, h := range houses {
		if jobs[i] = in.Jobs[h.ShipmentID]; jobs[i] == "" {
			return fmt.Errorf("%w: house %s", ErrPoolJobMissing, h.ShipmentID)
		}
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
		la := LineApportionment{LineNo: no, LineVersion: v.No, Method: method, Amount: cost, Local: local}
		var quantities []int64
		var unit string
		var err error
		if method == MethodManual {
			if len(lineNos) != 1 || !text(in.Reason) {
				return fmt.Errorf("%w: a manual apportionment divides one line with a reason", ErrInvalid)
			}
			la.Reason = in.Reason
			if quantities, err = manualQuantities(houses, in.Manual, cost); err != nil {
				return err
			}
			unit = ""
		} else {
			if quantities, unit, err = methodQuantities(method, p.state.Mode, stage, houses); err != nil {
				return err
			}
		}
		amounts, err := split(cost, quantities)
		if err != nil {
			return err
		}
		locals, err := split(local, quantities)
		if err != nil {
			return err
		}
		for i, h := range houses {
			la.Shares = append(la.Shares, Share{No: i + 1, ShipmentID: h.ShipmentID, JobID: jobs[i], Quantity: quantities[i], Unit: unit,
				Amount: amounts[i], Local: locals[i]})
		}
		a.Lines = append(a.Lines, la)
	}
	p.state.Apportionments = append(p.state.Apportionments, a)
	p.record(eventPoolApportioned, at, a.No)
	return nil
}

// retain keeps the named lines' costs (every line when none is named) on the consolidation, with a reason: allowed
// only once PHY reports the consolidation cancelled or without a house left, so that while houses bear a consolidation
// its costs reach their jobs. The lines' current costs are recorded and no share is written; a later apportionment
// supersedes it as any other.
func (p *ConsolCostPool) retain(in ApportionInput, basis CostBasis, by string, at time.Time) error {
	lineNos := slices.Clone(in.LineNos)
	if len(lineNos) == 0 {
		for _, l := range p.state.Lines {
			lineNos = append(lineNos, l.No)
		}
	}
	slices.Sort(lineNos)
	switch {
	case basis.ConsolidationID != p.state.ConsolidationID || basis.Revision < 1:
		return fmt.Errorf("%w: PHY answered consolidation %s for %s", ErrInvalid, basis.ConsolidationID, p.state.ConsolidationID)
	case basis.State != "CANCELLED" && len(basis.Houses) > 0:
		return fmt.Errorf("%w: a cost is retained on the consolidation only once PHY reports it cancelled or without houses; %s has %d",
			ErrInvalid, p.state.ConsolidationID, len(basis.Houses))
	case !text(in.Reason) || len(in.Manual) > 0:
		return fmt.Errorf("%w: a cost retained on the consolidation needs a reason and takes no shares", ErrInvalid)
	case len(lineNos) == 0 || slices.Contains(lineNos, 0) || lineNos[len(lineNos)-1] > len(p.state.Lines) || len(slices.Compact(lineNos)) != len(lineNos):
		return fmt.Errorf("%w: lines of the pool, each once", ErrInvalid)
	}
	a := Apportionment{No: len(p.state.Apportionments) + 1, BasisRevision: basis.Revision, Stage: BasisActual, By: by, At: at.UTC()}
	for _, no := range lineNos {
		versions := p.state.Lines[no-1].Versions
		cost, local := p.LineCost(no)
		a.Lines = append(a.Lines, LineApportionment{LineNo: no, LineVersion: versions[len(versions)-1].No, Method: MethodRetained, Reason: in.Reason,
			Amount: cost, Local: local})
	}
	p.state.Apportionments = append(p.state.Apportionments, a)
	p.record(eventPoolApportioned, at, a.No)
	return nil
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

// manualQuantities reads hand-given shares: every house exactly once, non-negative, summing to the line amount; the
// shares are their own weights, so largest remainder reproduces them exactly.
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

// methodQuantities computes each house's basis quantity for a method at a stage.
func methodQuantities(method, mode, stage string, houses []HouseFacts) ([]int64, string, error) {
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
// cost on PHY's current basis with every house loaded (ACTUAL), naming exactly the live houses and, for a weighed
// method, their current quantities. A line whose final cost is zero needs no apportionment (or one of zero). Once PHY
// cancelled the consolidation or no house is left on it, there is no basis to compare: the latest apportionments at
// the final costs stand. The pool then takes no change until reopened.
func (p *ConsolCostPool) Close(basis CostBasis, by string, at time.Time) error {
	if err := p.open(by, at); err != nil {
		return err
	}
	houses := slices.Clone(basis.Houses)
	slices.SortFunc(houses, func(a, b HouseFacts) int { return cmpString(a.ShipmentID, b.ShipmentID) })
	if basis.ConsolidationID != p.state.ConsolidationID {
		return fmt.Errorf("%w: PHY answered consolidation %s for %s", ErrInvalid, basis.ConsolidationID, p.state.ConsolidationID)
	}
	gone := basis.State == "CANCELLED" || len(houses) == 0
	loaded := !gone
	for _, h := range houses {
		loaded = loaded && h.LoadedPacks > 0
	}
	for _, l := range p.state.Lines {
		c := p.consumed[l.No]
		status := p.LineStatus(l.No)
		switch {
		case status != StatusMatched && status != StatusReleased:
			return fmt.Errorf("%w: cost line %d is %s", ErrPoolNotClosable, l.No, status)
		case c.Held:
			return fmt.Errorf("%w: a creditor invoice matched to cost line %d is disputed or awaits a variance approval", ErrPoolNotClosable, l.No)
		}
		cost, local := p.LineCost(l.No)
		a, la, ok := p.SharesOfLine(l.No)
		switch {
		case !ok && (cost != 0 || local != 0):
			return fmt.Errorf("%w: cost line %d is not apportioned", ErrPoolNotClosable, l.No)
		case !ok:
			continue
		case la.Amount != cost || la.Local != local:
			return fmt.Errorf("%w: cost line %d was apportioned at %d (%d local); its final cost is %d (%d local)", ErrPoolNotClosable, l.No, la.Amount,
				la.Local, cost, local)
		case la.Method == MethodRetained && !gone && (cost != 0 || local != 0):
			return fmt.Errorf("%w: cost line %d is retained on the consolidation while PHY reports houses on it", ErrPoolNotClosable, l.No)
		case cost == 0 && local == 0, gone:
			continue // every share is zero, or no basis is left to compare
		case a.Stage != BasisActual || !loaded || len(la.Shares) != len(houses):
			return fmt.Errorf("%w: cost line %d is not apportioned on the current loaded basis", ErrPoolNotClosable, l.No)
		}
		var quantities []int64
		var unit string
		if la.Method != MethodManual {
			var err error
			if quantities, unit, err = methodQuantities(la.Method, p.state.Mode, BasisActual, houses); err != nil {
				return fmt.Errorf("%w: cost line %d: %w", ErrPoolNotClosable, l.No, err)
			}
		}
		for i, h := range houses {
			s := la.Shares[i]
			if s.ShipmentID != h.ShipmentID || (quantities != nil && (s.Quantity != quantities[i] || s.Unit != unit)) {
				return fmt.Errorf("%w: cost line %d is not apportioned on the current loaded basis (house %s)", ErrPoolNotClosable, l.No, h.ShipmentID)
			}
		}
	}
	c := PoolClosure{No: len(p.state.Closures) + 1, By: by, At: at.UTC()}
	p.state.Closures, p.state.State = append(p.state.Closures, c), PoolClosed
	p.record(eventPoolClosed, at, c.No)
	return nil
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
	s := storedPool{lines: len(st.Lines), apportionments: len(st.Apportionments), closures: len(st.Closures), versions: map[int]int{}}
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
	for i := range st.Apportionments {
		a := &st.Apportionments[i]
		a.Lines = append([]LineApportionment(nil), a.Lines...)
		if a.No != i+1 {
			return nil, fmt.Errorf("%w: apportionment %d", ErrInvalid, i+1)
		}
		for k := range a.Lines {
			a.Lines[k].Shares = append([]Share(nil), a.Lines[k].Shares...)
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
			st.Apportionments[i].Lines[k].Shares = append([]Share(nil), st.Apportionments[i].Lines[k].Shares...)
		}
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
	p.storedPool = storedPool{lines: len(p.state.Lines), apportionments: len(p.state.Apportionments), closures: len(p.state.Closures), versions: map[int]int{}}
	for _, l := range p.state.Lines {
		p.storedPool.versions[l.No] = len(l.Versions)
	}
}
