package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	docapi "go.idp.xyz/idp-cargo/internal/contexts/doc/api"
	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// ReworkedContainerReport answers, in one read, what became of a container that was opened for rework (V2.95, step 7 of
// "a house leaves a sealed container"): the reworks of its stuffing, where each house that was in it is now, who bears
// each cost of the original consolidation's pool, what there is to recover, and what FIN still lists as pending. It
// composes the contexts' query sides and lives here, where the contexts are composed, so that no context depends on
// another for it. It decides nothing and writes nothing: every figure and every party is FIN's attribution as it
// stands, and the line for the customer of a house that bore a loss only says that the loss may be charged to it.
//
// The reads are not one snapshot. Each is made on its own, outside any transaction, as FIN's pool queries read PHY's
// basis, so a context that moves between two of them can make the parts disagree; read the report again if it matters.

// The sources of the report. Any of SEA, DOC and FIN may be nil, and then, like one that fails, the part it answers
// says so (Unanswered) and the rest is still given. PHY anchors the report: without it nobody can tell which houses
// the container held, so a PHY that is missing or fails fails the report.
type (
	// ReworkPHY is PHY's cost basis of a consolidation and the membership of a shipment.
	ReworkPHY interface {
		phyapi.PhysicalCostBasisQueries
		phyapi.ConsolMembershipQueries
	}
	// ReworkSEA is SEA's movement, with its container visits.
	ReworkSEA interface {
		GetOceanMovement(ctx context.Context, q seaapi.GetOceanMovement) (seaapi.OceanMovement, error)
	}
	// ReworkDOC is DOC's documents of a shipment.
	ReworkDOC interface {
		ListDocumentsForShipment(ctx context.Context, q docapi.ListDocumentsForShipment) ([]docapi.DocumentSummary, error)
		GetTransportDocument(ctx context.Context, q docapi.GetTransportDocument) (docapi.TransportDocument, error)
		GetDocumentVersion(ctx context.Context, q docapi.GetDocumentVersion) (docapi.DocumentVersion, error)
	}
	// ReworkFIN is FIN's cost pool of a consolidation, its economics and its pending items.
	ReworkFIN interface {
		GetConsolProfit(ctx context.Context, q finapi.GetConsolProfit) (finapi.ConsolProfit, error)
		GetConsolCostPool(ctx context.Context, q finapi.GetConsolCostPool) (finapi.ConsolCostPool, error)
		GetConsolCostPoolPendingItems(ctx context.Context, q finapi.GetConsolCostPoolPendingItems) ([]finapi.PoolPendingItem, error)
	}
)

// ReworkedContainerReader reads the report from the contexts it is given.
type ReworkedContainerReader struct {
	phy ReworkPHY
	sea ReworkSEA
	doc ReworkDOC
	fin ReworkFIN
}

// NewReworkedContainerReader wires the report to the contexts' query sides. Pass nil for a context that is not
// assembled.
func NewReworkedContainerReader(phy ReworkPHY, sea ReworkSEA, doc ReworkDOC, fin ReworkFIN) *ReworkedContainerReader {
	return &ReworkedContainerReader{phy: phy, sea: sea, doc: doc, fin: fin}
}

// ErrReworkedContainerUnknown is returned for a consolidation PHY does not know and for a load unit that PHY's basis of
// the consolidation does not list (planned and not removed) and that has no stuffing, in force or given up after a
// rework. A unit that is planned and was never stuffed is a unit: its report is empty, not an error.
var ErrReworkedContainerUnknown = errors.New("bootstrap: unknown consolidation or load unit")

// ErrReworkPHYMissing is returned when PHY is not wired or does not answer.
var ErrReworkPHYMissing = errors.New("bootstrap: PHY does not answer, so the houses of the container are not known")

// ReworkedContainerQuery names the container: the load unit of the consolidation, which is the unit of PHY's stuffing.
type ReworkedContainerQuery struct {
	TenantID        string
	ConsolidationID string
	LoadUnitNo      int
}

// ReworkedContainerReport is the answer. A part that a context did not answer has its Unanswered set to why, and
// nothing else in it is to be read.
type ReworkedContainerReport struct {
	TenantID        string
	ConsolidationID string
	LoadUnitNo      int
	// Stuffings are the stuffings of the unit that were reworked, the ones given up first and the one in force last,
	// with every rework as PHY holds it: who asked, when and why, the seal broken, how and when it ended and the seal
	// put on (NewSealReference). A unit that was never reworked has the stuffing in force with no reworks, or none.
	Stuffings []ReworkedStuffing
	// Houses are the houses in the container now (loaded in the unit), the live houses a rework of this unit's stuffing
	// unloaded (PHY's UnloadedBy: still attached with nothing loaded, or loaded again elsewhere) and the houses PHY
	// lists as having left the consolidation after a rework of this unit's stuffing, in shipment order. Not listed: a
	// house that left with no rework, one that left from another unit's stuffing, and a house that no rework unloaded
	// and that is not loaded in the unit now.
	Houses []ReworkedHouse
	// Costs says who bears each cost of the consolidation's pool.
	Costs ReworkedCosts
	// Recoveries is what there is to recover, derived from FIN's attributions, of the houses this report lists only:
	// what the pool's other attributions give rise to is in Costs, as the pool's. Claims are not built: nothing is
	// opened or recorded against any party or customer.
	Recoveries     []Recovery
	RecoveriesNote string
	// Pending is FIN's pending items of the pool, as FIN lists them: the pool's, all containers' (PendingScope says
	// so).
	Pending           []finapi.PoolPendingItem
	PendingNoPool     bool // the consolidation has no cost pool
	PendingScope      string
	PendingUnanswered string // why FIN did not list them; empty when it did
}

// ReworkedStuffing is a stuffing of the unit with its reworks.
type ReworkedStuffing struct {
	ExecutionID string
	State       string // PHY's state of the stuffing; empty for a stuffing that was given up (it is CANCELLED)
	GivenUp     bool
	Reworks     []phyapi.Rework
}

// ReworkedHouse is where a house is now.
type ReworkedHouse struct {
	ShipmentID string
	// InContainer is true while the house has packs loaded in the unit. Left is set for a house PHY lists as having
	// left the consolidation: how (detached, transferred to which consolidation, shipment cancelled), why, who and
	// when.
	InContainer bool
	// UnloadedBy is the rework that last unloaded the house since its latest entry began, as PHY's basis says it for a
	// live house; nil when none did. A house loaded in the unit now can have it (loaded again after the rework).
	UnloadedBy    *phyapi.HouseUnloadedBy
	Left          *phyapi.FormerHouse
	Consolidation HouseConsolidation
	Loaded        HouseLoad
	Movements     HouseMovements
	Documents     HouseDocuments
}

// HouseConsolidation is the consolidation the house is attached to now, from PHY's membership. A house in no
// consolidation says so (InNone).
type HouseConsolidation struct {
	InNone          bool
	ConsolidationID string
	EntryNo         int
	Unanswered      string
}

// HouseLoad is what PHY has of the house loaded in the consolidation it is in now. Nothing says so when it is attached
// and has no packs loaded; for a house in no consolidation there is nothing to ask.
type HouseLoad struct {
	Units      []LoadedUnit
	Nothing    bool
	Unanswered string
}

// LoadedUnit is a load unit with the house's packs in it and the state of the stuffing, which PHY reads for the unit.
type LoadedUnit struct {
	UnitNo         int
	ExecutionID    string
	ExecutionState string
	Packs          int64
}

// HouseMovements are the SEA movements of the house, found through the movements its transport documents name (SEA
// has no query by shipment or consolidation). A house with no document has none found, and says so.
type HouseMovements struct {
	Found      []HouseMovement
	NoDocument bool
	Unanswered string
}

// HouseMovement is one movement: its state, whether the house is still associated with it, and the container visit of
// the unit PHY has the house in now, with whether it is on board and when. NoVisit is true when no live visit of the
// movement is of that unit (or PHY has nothing of the house loaded).
type HouseMovement struct {
	MovementID      string
	State           string
	ConsolidationID string
	Associated      bool
	Visit           *HouseVisit
	NoVisit         bool
}

// HouseVisit is a container visit as SEA has it now.
type HouseVisit struct {
	VisitID         string
	LoadUnitNo      int
	ContainerNumber string
	Status          string
	OnBoard         bool
	OnBoardAt       time.Time
}

// HouseDocuments are the house's transport documents in DOC, in DOC's order.
type HouseDocuments struct {
	Documents  []HouseDocument
	Unanswered string
}

// HouseDocument is one transport document: its kind, number and state, the movement it is on and, for an issued one,
// the on-board date it states.
type HouseDocument struct {
	DocumentID string
	Kind       string
	Number     string
	State      string
	MovementID string
	OnBoardAt  time.Time
}

// ReworkedCosts is the pool of the consolidation, line by line, with the totals.
type ReworkedCosts struct {
	// Scope says whose costs these are: the consolidation's pool, not the container's. Cost lines are not per container
	// (the owner's design, question 5), so the lines, the totals and the attributions of every house of the pool are
	// here, each loss, exception cost and former share tagged with its house and the unit it left from or was unloaded
	// from (0 when PHY does not say). What this container's houses give rise to is in Recoveries.
	Scope      string
	NoPool     bool
	Unanswered string
	PoolID     string
	State      string
	InWork     bool
	Lines      []CostLineBorne
	Totals     CostTotals
}

// CostLineBorne is one cost line and who bears it under the apportionment in force: the shares of the houses still in
// the consolidation, each former house's loss, the exception costs by party, the amount to be allocated, what the
// consolidation retains, the former shares not attributed yet, and what no apportionment covers. The parts sum to the
// cost, in both currencies. Cost is in the line's currency, Local in the functional one.
type CostLineBorne struct {
	LineNo          int
	ChargeCode      string
	Method          string
	Cost            kernel.Money
	Local           kernel.Money
	ApportionmentNo int // the apportionment in force; 0 when no apportionment covers the line
	Shares          []finapi.PoolShare
	Losses          []LossBorne
	Exceptions      []ExceptionBorne
	ToAllocate      kernel.Money
	ToAllocateLocal kernel.Money
	Retained        kernel.Money
	RetainedLocal   kernel.Money
	FormerPending   []FormerShareBorne
	Unapportioned   kernel.Money
	UnapportionedLo kernel.Money
}

// FormerShareBorne is a former share not attributed yet, with the unit its house left from.
type FormerShareBorne struct {
	ShipmentID string
	JobID      string
	Amount     kernel.Money
	Local      kernel.Money
	UnitNo     int
}

// LossBorne is a former house's loss, borne by its own job, with its voucher and the house's departure.
type LossBorne struct {
	UnitNo     int // the unit the house left from; 0 when PHY does not say
	ShipmentID string
	JobID      string
	Amount     kernel.Money
	Local      kernel.Money
	Voucher    string
	DepartedAt time.Time
}

// ExceptionBorne is an exception cost of the original consolidation, with the party responsible (PENDING while it is
// not established) and the reference finance wrote.
type ExceptionBorne struct {
	UnitNo     int // the unit the house left from; 0 when PHY does not say
	ShipmentID string
	Party      string
	Reference  string
	Amount     kernel.Money
	Local      kernel.Money
	Voucher    string
}

// CostTotals are FIN's totals of the pool in the functional currency, the pool's and not the container's. Pooled =
// Apportioned + Retained + Losses + Exceptions + ToAllocated + FormerSharesPending + Unapportioned. Conserved says that
// they do and that the lines agree: every line's parts add up to its cost in both currencies and the lines add up to
// each of FIN's totals. ExceptionsPending is the part of Exceptions whose party is PENDING.
type CostTotals struct {
	Pooled              kernel.Money
	Apportioned         kernel.Money
	Retained            kernel.Money
	Losses              kernel.Money
	Exceptions          kernel.Money
	ExceptionsPending   kernel.Money
	ToAllocated         kernel.Money
	FormerSharesPending kernel.Money
	Unapportioned       kernel.Money
	Conserved           bool
}

// The kinds of Recovery.
const (
	RecoverFromParty         = "RECOVER_FROM_PARTY"               // an exception cost of the carrier or the warehouse
	RecoverToEstablish       = "RESPONSIBILITY_TO_BE_ESTABLISHED" // an exception cost whose party is pending
	RecoverNotRecoverable    = "NOT_RECOVERABLE"                  // an exception cost of the forwarder's own operations
	RecoverChargeToCustomer  = "MAY_BE_CHARGED_TO_CUSTOMER"       // a loss borne by a house, if the terms allow it
	RecoverFormerSharePend   = "FORMER_SHARE_PENDING"             // nothing is decided yet
	RecoverUnclassifiedParty = "EXCEPTION_PARTY_NOT_CLASSIFIED"   // a party this report does not know
	recoveriesNote           = "Claims are not built: this lists what there is to recover of the houses listed and opens no claim."
	pendingScope             = "the pool's pending items, all containers' of the consolidation"
	costsScope               = "the consolidation's pool: cost lines are not per container"
	chargeToCustomerNote     = "may be charged to the house's customer if the commercial terms allow; the system does not decide"
	formerShareNote          = "nothing is decided yet: finance has not attributed it"
	notRecoverableNote       = "own operations: not recoverable; listed so that the total is complete"
	toEstablishNote          = "responsibility is to be established"
	unclassifiedPartyNote    = "the party is none of CARRIER, WAREHOUSE, OWN_OPERATIONS or PENDING"
	recoverFromPartyNote     = "to recover from the party"
)

// Recovery is one amount to recover, or that is not recoverable, derived from one attribution of FIN. Party and
// Reference are the exception cost's; ShipmentID is the house whose loss or former share it is, or the house whose
// departure the exception cost came from.
type Recovery struct {
	UnitNo     int // the unit the house left from; 0 when PHY does not say
	Kind       string
	LineNo     int
	ShipmentID string
	Party      string
	Reference  string
	Amount     kernel.Money
	Local      kernel.Money
	Voucher    string
	Note       string
}

// ReworkedContainerReport reads the report. It fails with ErrReworkedContainerUnknown for a consolidation or a unit
// PHY does not know and with ErrReworkPHYMissing when PHY does not answer; a context other than PHY that is not wired
// or fails makes only its own part unanswered.
func (r *ReworkedContainerReader) ReworkedContainerReport(ctx context.Context, q ReworkedContainerQuery) (ReworkedContainerReport, error) {
	if r.phy == nil {
		return ReworkedContainerReport{}, ErrReworkPHYMissing
	}
	basis, err := r.phy.GetPhysicalCostBasis(ctx, phyapi.GetPhysicalCostBasis{TenantID: q.TenantID, ConsolidationID: q.ConsolidationID})
	if errors.Is(err, phyapi.ErrNotFound) {
		return ReworkedContainerReport{}, fmt.Errorf("%w: consolidation %s", ErrReworkedContainerUnknown, q.ConsolidationID)
	}
	if err != nil {
		return ReworkedContainerReport{}, fmt.Errorf("%w: %v", ErrReworkPHYMissing, err)
	}
	out := ReworkedContainerReport{TenantID: q.TenantID, ConsolidationID: q.ConsolidationID, LoadUnitNo: q.LoadUnitNo, RecoveriesNote: recoveriesNote}
	if out.Stuffings = stuffingsOfUnit(basis, q.LoadUnitNo); len(out.Stuffings) == 0 && !hasUnit(basis, q.LoadUnitNo) {
		return ReworkedContainerReport{}, fmt.Errorf("%w: consolidation %s has no load unit %d", ErrReworkedContainerUnknown, q.ConsolidationID, q.LoadUnitNo)
	}
	bases := map[string]phyapi.PhysicalCostBasis{q.ConsolidationID: basis}
	for _, shipment := range housesOfContainer(basis, q.LoadUnitNo, out.Stuffings) {
		out.Houses = append(out.Houses, r.house(ctx, q, basis, bases, shipment))
	}
	out.Costs, out.Recoveries = r.costs(ctx, q, basis)
	out.Recoveries = slices.DeleteFunc(out.Recoveries, func(x Recovery) bool {
		return !slices.ContainsFunc(out.Houses, func(h ReworkedHouse) bool { return h.ShipmentID == x.ShipmentID })
	})
	slices.SortStableFunc(out.Recoveries, func(a, b Recovery) int {
		return recoveryRank(a.Kind) - recoveryRank(b.Kind)
	})
	out.Pending, out.PendingNoPool, out.PendingUnanswered = r.pending(ctx, q, out.Costs)
	out.PendingScope = pendingScope
	return out, nil
}

func hasUnit(b phyapi.PhysicalCostBasis, unit int) bool {
	return slices.ContainsFunc(b.LoadUnits, func(u phyapi.PlannedLoadUnit) bool { return u.UnitNo == unit })
}

// stuffingsOfUnit are the stuffings of the unit that were given up after a rework, then the one in force.
func stuffingsOfUnit(b phyapi.PhysicalCostBasis, unit int) (out []ReworkedStuffing) {
	for _, g := range b.GivenUpStuffings {
		if g.UnitNo == unit {
			out = append(out, ReworkedStuffing{ExecutionID: g.ExecutionID, GivenUp: true, Reworks: g.Reworks})
		}
	}
	for _, l := range b.PackingLists {
		if l.UnitNo == unit {
			out = append(out, ReworkedStuffing{ExecutionID: l.ExecutionID, State: l.ExecutionState, Reworks: l.Reworks})
		}
	}
	return out
}

// housesOfContainer are the shipments the container held: the live houses with packs loaded in the unit, the live
// houses whose latest unloading was a rework of one of this unit's stuffings, and the houses PHY lists as having left
// whose rework is a rework of one of this unit's stuffings (the unit and the execution both match, whatever way PHY
// found the rework), in shipment order and each once. Not listed, because PHY's basis does not say they were in this
// container: a house that left with no rework (shut out before stuffing, or never loaded), a house that left from
// another unit's stuffing, and a live house that no rework unloaded and that is not loaded in the unit now.
func housesOfContainer(b phyapi.PhysicalCostBasis, unit int, stuffings []ReworkedStuffing) []string {
	ours := func(execution string, unitNo int) bool {
		return unitNo == unit && slices.ContainsFunc(stuffings, func(x ReworkedStuffing) bool { return x.ExecutionID == execution })
	}
	var out []string
	for _, h := range b.Houses {
		if slices.ContainsFunc(h.Units, func(u phyapi.HouseUnitLoad) bool { return u.UnitNo == unit && u.LoadedPacks > 0 }) ||
			(h.UnloadedBy.ReworkNo != 0 && ours(h.UnloadedBy.ExecutionID, h.UnloadedBy.UnitNo)) {
			out = append(out, h.ShipmentID)
		}
	}
	for _, f := range b.FormerHouses {
		if ours(f.ReworkExecutionID, f.ReworkUnitNo) {
			out = append(out, f.ShipmentID)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func (r *ReworkedContainerReader) house(ctx context.Context, q ReworkedContainerQuery, basis phyapi.PhysicalCostBasis, bases map[string]phyapi.PhysicalCostBasis, shipment string) ReworkedHouse {
	h := ReworkedHouse{ShipmentID: shipment}
	for _, live := range basis.Houses {
		if live.ShipmentID == shipment {
			h.InContainer = slices.ContainsFunc(live.Units, func(u phyapi.HouseUnitLoad) bool { return u.UnitNo == q.LoadUnitNo && u.LoadedPacks > 0 })
			if live.UnloadedBy.ReworkNo != 0 {
				unloaded := live.UnloadedBy
				h.UnloadedBy = &unloaded
			}
		}
	}
	for _, f := range basis.FormerHouses {
		if f.ShipmentID == shipment {
			left := f
			h.Left = &left
		}
	}
	m, err := r.phy.GetConsolMembership(ctx, phyapi.GetConsolMembership{TenantID: q.TenantID, ShipmentID: shipment})
	var unit int
	switch {
	case errors.Is(err, phyapi.ErrNotFound) || (err == nil && !m.Live):
		h.Consolidation = HouseConsolidation{InNone: true}
	case err != nil:
		h.Consolidation = HouseConsolidation{Unanswered: "PHY: " + err.Error()}
	default:
		h.Consolidation = HouseConsolidation{ConsolidationID: m.Entry.ConsolidationID, EntryNo: m.Entry.EntryNo}
		h.Loaded, unit = r.loaded(ctx, q, bases, m.Entry.ConsolidationID, shipment)
	}
	h.Documents = r.documents(ctx, q, shipment)
	h.Movements = r.movements(ctx, q, h, unit)
	return h
}

// loaded reads what PHY has of the house loaded in the consolidation it is in now (the cost basis of that
// consolidation, read once). It answers the unit the house is loaded in, which names the SEA visit, and 0 when there
// is none or more than one.
func (r *ReworkedContainerReader) loaded(ctx context.Context, q ReworkedContainerQuery, bases map[string]phyapi.PhysicalCostBasis, consolidation, shipment string) (HouseLoad, int) {
	b, ok := bases[consolidation]
	if !ok {
		var err error
		if b, err = r.phy.GetPhysicalCostBasis(ctx, phyapi.GetPhysicalCostBasis{TenantID: q.TenantID, ConsolidationID: consolidation}); err != nil {
			return HouseLoad{Unanswered: "PHY: " + err.Error()}, 0
		}
		bases[consolidation] = b
	}
	var out HouseLoad
	for _, h := range b.Houses {
		if h.ShipmentID != shipment {
			continue
		}
		for _, u := range h.Units {
			state := ""
			for _, l := range b.PackingLists {
				if l.UnitNo == u.UnitNo {
					state = l.ExecutionState
				}
			}
			out.Units = append(out.Units, LoadedUnit{UnitNo: u.UnitNo, ExecutionID: u.ExecutionID, ExecutionState: state, Packs: u.LoadedPacks})
		}
	}
	out.Nothing = len(out.Units) == 0
	if len(out.Units) == 1 {
		return out, out.Units[0].UnitNo
	}
	return out, 0
}

func (r *ReworkedContainerReader) documents(ctx context.Context, q ReworkedContainerQuery, shipment string) HouseDocuments {
	if r.doc == nil {
		return HouseDocuments{Unanswered: "DOC is not wired"}
	}
	list, err := r.doc.ListDocumentsForShipment(ctx, docapi.ListDocumentsForShipment{TenantID: q.TenantID, ShipmentID: shipment})
	if err != nil {
		return HouseDocuments{Unanswered: "DOC: " + err.Error()}
	}
	var out HouseDocuments
	for _, s := range list {
		d := HouseDocument{DocumentID: s.DocumentID, Kind: s.Kind, Number: s.Number, State: s.State}
		doc, err := r.doc.GetTransportDocument(ctx, docapi.GetTransportDocument{TenantID: q.TenantID, DocumentID: s.DocumentID})
		if err != nil {
			return HouseDocuments{Unanswered: "DOC: " + err.Error()}
		}
		d.MovementID = doc.MovementID
		if s.State == string(docapi.StateIssued) {
			v, err := r.doc.GetDocumentVersion(ctx, docapi.GetDocumentVersion{TenantID: q.TenantID, DocumentID: s.DocumentID})
			if err != nil {
				return HouseDocuments{Unanswered: "DOC: " + err.Error()}
			}
			d.OnBoardAt = v.Doc.OnBoardAt
		}
		out.Documents = append(out.Documents, d)
	}
	return out
}

func (r *ReworkedContainerReader) movements(ctx context.Context, q ReworkedContainerQuery, h ReworkedHouse, unit int) HouseMovements {
	if r.sea == nil {
		return HouseMovements{Unanswered: "SEA is not wired"}
	}
	if h.Documents.Unanswered != "" {
		return HouseMovements{Unanswered: "SEA is found through DOC's documents, and DOC did not answer"}
	}
	var ids []string
	for _, d := range h.Documents.Documents {
		if d.MovementID != "" && !slices.Contains(ids, d.MovementID) {
			ids = append(ids, d.MovementID)
		}
	}
	if len(ids) == 0 {
		return HouseMovements{NoDocument: true}
	}
	var out HouseMovements
	for _, id := range ids {
		m, err := r.sea.GetOceanMovement(ctx, seaapi.GetOceanMovement{TenantID: q.TenantID, MovementID: id})
		if err != nil {
			return HouseMovements{Unanswered: "SEA: " + err.Error()}
		}
		hm := HouseMovement{MovementID: id, State: m.State, ConsolidationID: m.ConsolidationID, NoVisit: true}
		hm.Associated = slices.ContainsFunc(m.Houses, func(x seaapi.House) bool { return x.ShipmentID == h.ShipmentID })
		for _, v := range m.Containers {
			if unit == 0 || v.LoadUnitNo != unit || (h.Consolidation.ConsolidationID != "" && v.ConsolidationID != h.Consolidation.ConsolidationID) {
				continue
			}
			hv := HouseVisit{VisitID: v.VisitID, LoadUnitNo: v.LoadUnitNo, ContainerNumber: v.ContainerNumber, Status: v.Status, OnBoard: v.Status == seaapi.VisitLoaded}
			for _, e := range v.Events { // latest first
				if e.Kind == seaapi.EventLoaded {
					hv.OnBoardAt = e.HappenedAt
					break
				}
			}
			hm.Visit, hm.NoVisit = &hv, false
		}
		out.Found = append(out.Found, hm)
	}
	return out
}

// pending lists FIN's pending items of the pool the costs part found.
func (r *ReworkedContainerReader) pending(ctx context.Context, q ReworkedContainerQuery, costs ReworkedCosts) (items []finapi.PoolPendingItem, noPool bool, unanswered string) {
	if costs.NoPool || costs.Unanswered != "" {
		return nil, costs.NoPool, costs.Unanswered
	}
	items, err := r.fin.GetConsolCostPoolPendingItems(ctx, finapi.GetConsolCostPoolPendingItems{TenantID: q.TenantID, PoolID: costs.PoolID})
	if err != nil {
		return nil, false, "FIN: " + err.Error()
	}
	return items, false, ""
}

func (r *ReworkedContainerReader) costs(ctx context.Context, q ReworkedContainerQuery, basis phyapi.PhysicalCostBasis) (ReworkedCosts, []Recovery) {
	if r.fin == nil {
		return ReworkedCosts{Unanswered: "FIN is not wired"}, nil
	}
	profit, err := r.fin.GetConsolProfit(ctx, finapi.GetConsolProfit{TenantID: q.TenantID, ConsolidationID: q.ConsolidationID})
	if errors.Is(err, finapi.ErrNotFound) {
		return ReworkedCosts{NoPool: true}, nil
	}
	if err != nil {
		return ReworkedCosts{Unanswered: "FIN: " + err.Error()}, nil
	}
	pool, err := r.fin.GetConsolCostPool(ctx, finapi.GetConsolCostPool{TenantID: q.TenantID, PoolID: profit.PoolID})
	if err != nil {
		return ReworkedCosts{Unanswered: "FIN: " + err.Error()}, nil
	}
	out := ReworkedCosts{Scope: costsScope, PoolID: pool.PoolID, State: pool.State, InWork: pool.InWork}
	out.Totals = CostTotals{Pooled: profit.Pooled, Apportioned: profit.Apportioned, Retained: profit.Retained, Losses: profit.Losses, Exceptions: profit.Exceptions,
		ExceptionsPending: profit.ExceptionsPending, ToAllocated: profit.ToAllocated, FormerSharesPending: profit.FormerSharesPending, Unapportioned: profit.Unapportioned}
	t := out.Totals
	totalsConserve := t.Pooled.Minor == t.Apportioned.Minor+t.Retained.Minor+t.Losses.Minor+t.Exceptions.Minor+t.ToAllocated.Minor+t.FormerSharesPending.Minor+t.Unapportioned.Minor
	units := unitsOfHouses(basis)
	var recoveries []Recovery
	for _, line := range pool.Lines {
		l, rec := costLine(pool, units, basis, line)
		out.Lines = append(out.Lines, l)
		recoveries = append(recoveries, rec...)
	}
	out.Totals.Conserved = totalsConserve && linesAgree(out.Lines, t)
	return out, recoveries
}

// costLine says who bears one line under the latest apportionment that covers it.
func costLine(pool finapi.ConsolCostPool, units map[string]int, basis phyapi.PhysicalCostBasis, line finapi.PoolCostLine) (CostLineBorne, []Recovery) {
	l := CostLineBorne{LineNo: line.LineNo, ChargeCode: line.ChargeCode, Method: line.Method, Cost: line.Cost, Local: line.CostLocal}
	var covering *finapi.ApportionedLine
	for i := len(pool.Apportionments) - 1; i >= 0 && covering == nil; i-- {
		for j := range pool.Apportionments[i].Lines {
			if pool.Apportionments[i].Lines[j].LineNo == line.LineNo {
				l.ApportionmentNo, covering = pool.Apportionments[i].ApportionmentNo, &pool.Apportionments[i].Lines[j]
			}
		}
	}
	for _, a := range pool.Attributions {
		if a.LineNo == line.LineNo {
			for _, f := range a.FormerShares {
				l.FormerPending = append(l.FormerPending, FormerShareBorne{ShipmentID: f.ShipmentID, JobID: f.JobID, Amount: f.Amount, Local: f.Local, UnitNo: units[f.ShipmentID]})
			}
		}
	}
	var recoveries []Recovery
	if covering == nil {
		l.Unapportioned, l.UnapportionedLo = line.Cost, line.CostLocal
		return l, nil
	}
	pending := map[string]bool{}
	for _, f := range l.FormerPending {
		pending[f.ShipmentID] = true
	}
	for _, s := range covering.Shares {
		if !pending[s.ShipmentID] && !(s.NotUsed && isFormer(basis, s.ShipmentID)) {
			l.Shares = append(l.Shares, s)
		}
	}
	for _, x := range covering.Attributions {
		if x.Loss.Minor > 0 {
			l.Losses = append(l.Losses, LossBorne{UnitNo: units[x.ShipmentID], ShipmentID: x.ShipmentID, JobID: x.JobID, Amount: x.Loss, Local: x.LossLocal, Voucher: x.Voucher, DepartedAt: x.DepartedAt})
			recoveries = append(recoveries, Recovery{UnitNo: units[x.ShipmentID], Kind: RecoverChargeToCustomer, LineNo: line.LineNo, ShipmentID: x.ShipmentID, Amount: x.Loss, Local: x.LossLocal,
				Voucher: x.Voucher, Note: chargeToCustomerNote})
		}
		if x.Exception.Minor > 0 {
			l.Exceptions = append(l.Exceptions, ExceptionBorne{UnitNo: units[x.ShipmentID], ShipmentID: x.ShipmentID, Party: x.Party, Reference: x.PartyReference, Amount: x.Exception,
				Local: x.ExceptionLocal, Voucher: x.Voucher})
			kind, note := exceptionRecovery(x.Party)
			recoveries = append(recoveries, Recovery{UnitNo: units[x.ShipmentID], Kind: kind, LineNo: line.LineNo, ShipmentID: x.ShipmentID, Party: x.Party, Reference: x.PartyReference,
				Amount: x.Exception, Local: x.ExceptionLocal, Voucher: x.Voucher, Note: note})
		}
	}
	for _, f := range l.FormerPending {
		recoveries = append(recoveries, Recovery{UnitNo: f.UnitNo, Kind: RecoverFormerSharePend, LineNo: line.LineNo, ShipmentID: f.ShipmentID, Amount: f.Amount, Local: f.Local, Note: formerShareNote})
	}
	l.ToAllocate, l.ToAllocateLocal = covering.ToAllocate, covering.ToAllocateLocal
	taken, takenLocal := sums(l)
	if covering.Method == finapi.MethodRetained {
		l.Retained = kernel.Money{Currency: covering.Amount.Currency, Minor: covering.Amount.Minor - taken}
		l.RetainedLocal = kernel.Money{Currency: covering.Local.Currency, Minor: covering.Local.Minor - takenLocal}
		taken, takenLocal = taken+l.Retained.Minor, takenLocal+l.RetainedLocal.Minor
	}
	l.Unapportioned = kernel.Money{Currency: line.Cost.Currency, Minor: line.Cost.Minor - taken}
	l.UnapportionedLo = kernel.Money{Currency: line.CostLocal.Currency, Minor: line.CostLocal.Minor - takenLocal}
	return l, recoveries
}

// sums are what the line's parts other than the retained amount and the unapportioned rest take, in the line's currency
// and in the functional currency.
func sums(l CostLineBorne) (amount, local int64) {
	for _, s := range l.Shares {
		amount, local = amount+s.Amount.Minor, local+s.Local.Minor
	}
	for _, x := range l.Losses {
		amount, local = amount+x.Amount.Minor, local+x.Local.Minor
	}
	for _, x := range l.Exceptions {
		amount, local = amount+x.Amount.Minor, local+x.Local.Minor
	}
	for _, f := range l.FormerPending {
		amount, local = amount+f.Amount.Minor, local+f.Local.Minor
	}
	return amount + l.ToAllocate.Minor, local + l.ToAllocateLocal.Minor
}

func isFormer(b phyapi.PhysicalCostBasis, shipment string) bool {
	return slices.ContainsFunc(b.FormerHouses, func(f phyapi.FormerHouse) bool { return f.ShipmentID == shipment })
}

// exceptionRecovery is what an exception cost of the party means for recovery.
func exceptionRecovery(party string) (kind, note string) {
	switch party {
	case finapi.PartyCarrier, finapi.PartyWarehouse:
		return RecoverFromParty, recoverFromPartyNote
	case finapi.PartyPending:
		return RecoverToEstablish, toEstablishNote
	case finapi.PartyOwnOperations:
		return RecoverNotRecoverable, notRecoverableNote
	}
	return RecoverUnclassifiedParty, unclassifiedPartyNote
}

// recoveryRank orders the recoveries as the owner listed them: what is to be recovered from a party, what is still to
// be established, what is not recoverable, what may be charged to a customer and what is not decided yet.
func recoveryRank(kind string) int {
	switch kind {
	case RecoverFromParty:
		return 0
	case RecoverToEstablish:
		return 1
	case RecoverNotRecoverable:
		return 2
	case RecoverChargeToCustomer:
		return 3
	case RecoverFormerSharePend:
		return 4
	}
	return 5
}

// unitsOfHouses is the unit each former house left from, as PHY's basis says it: the unit of its rework. Only a former
// house has a former share or an attribution, so only these are tagged; a house PHY says nothing of has no entry (unit
// 0).
func unitsOfHouses(b phyapi.PhysicalCostBasis) map[string]int {
	out := map[string]int{}
	for _, f := range b.FormerHouses {
		if f.ReworkUnitNo != 0 {
			out[f.ShipmentID] = f.ReworkUnitNo
		}
	}
	return out
}

// linesAgree checks the lines against each other and against FIN's totals, in the functional currency: every line's
// parts add up to its cost, and the lines add up to each of FIN's totals.
func linesAgree(lines []CostLineBorne, t CostTotals) bool {
	var pooled, shares, retained, losses, exceptions, pending, allocate, former, rest int64
	for _, l := range lines {
		parts := l.ToAllocateLocal.Minor + l.RetainedLocal.Minor + l.UnapportionedLo.Minor
		for _, s := range l.Shares {
			shares, parts = shares+s.Local.Minor, parts+s.Local.Minor
		}
		for _, x := range l.Losses {
			losses, parts = losses+x.Local.Minor, parts+x.Local.Minor
		}
		for _, x := range l.Exceptions {
			exceptions, parts = exceptions+x.Local.Minor, parts+x.Local.Minor
			if x.Party == finapi.PartyPending {
				pending += x.Local.Minor
			}
		}
		for _, f := range l.FormerPending {
			former, parts = former+f.Local.Minor, parts+f.Local.Minor
		}
		if parts != l.Local.Minor {
			return false
		}
		pooled, retained, allocate, rest = pooled+l.Local.Minor, retained+l.RetainedLocal.Minor, allocate+l.ToAllocateLocal.Minor, rest+l.UnapportionedLo.Minor
	}
	return pooled == t.Pooled.Minor && shares == t.Apportioned.Minor && retained == t.Retained.Minor && losses == t.Losses.Minor && exceptions == t.Exceptions.Minor &&
		pending == t.ExceptionsPending.Minor && allocate == t.ToAllocated.Minor && former == t.FormerSharesPending.Minor && rest == t.Unapportioned.Minor
}
