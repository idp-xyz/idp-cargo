package bootstrap

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	docapi "go.idp.xyz/idp-cargo/internal/contexts/doc/api"
	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// The report reads the contexts' query sides and nothing else, so its rules are tested here with stand-ins for the
// contexts, one test for each part. The same report is run with the real contexts in the integration tests
// (reworked_container_integration_test.go): the acceptance of box A and the cases that need PHY's and FIN's own rules.

var t0 = time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)

func cny(minor int64) kernel.Money { return kernel.Money{Currency: "CNY", Minor: minor} }

type fakePHY struct {
	bases       map[string]phyapi.PhysicalCostBasis
	memberships map[string]phyapi.ConsolMembership
	failBasis   map[string]error
	failMember  error
	reads       map[string]int // the basis reads of each consolidation
}

func (f *fakePHY) GetPhysicalCostBasis(_ context.Context, q phyapi.GetPhysicalCostBasis) (phyapi.PhysicalCostBasis, error) {
	f.reads[q.ConsolidationID]++
	if err := f.failBasis[q.ConsolidationID]; err != nil {
		return phyapi.PhysicalCostBasis{}, err
	}
	b, ok := f.bases[q.ConsolidationID]
	if !ok {
		return phyapi.PhysicalCostBasis{}, phyapi.ErrNotFound
	}
	return b, nil
}

func (f *fakePHY) GetConsolMembership(_ context.Context, q phyapi.GetConsolMembership) (phyapi.ConsolMembership, error) {
	if f.failMember != nil {
		return phyapi.ConsolMembership{}, f.failMember
	}
	m, ok := f.memberships[q.ShipmentID]
	if !ok {
		return phyapi.ConsolMembership{}, phyapi.ErrNotFound
	}
	return m, nil
}

type fakeSEA struct {
	movements map[string]seaapi.OceanMovement
	err       error
}

func (f *fakeSEA) GetOceanMovement(_ context.Context, q seaapi.GetOceanMovement) (seaapi.OceanMovement, error) {
	if f.err != nil {
		return seaapi.OceanMovement{}, f.err
	}
	m, ok := f.movements[q.MovementID]
	if !ok {
		return seaapi.OceanMovement{}, seaapi.ErrNotFound
	}
	return m, nil
}

type fakeDOC struct {
	byShipment map[string][]docapi.TransportDocument
	onBoard    map[string]time.Time
	err        error
	getErr     error // the error of reading one document
	versionErr error // the error of reading a version
}

func (f *fakeDOC) ListDocumentsForShipment(_ context.Context, q docapi.ListDocumentsForShipment) ([]docapi.DocumentSummary, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []docapi.DocumentSummary
	for _, d := range f.byShipment[q.ShipmentID] {
		out = append(out, docapi.DocumentSummary{DocumentID: d.DocumentID, Kind: d.Kind, State: d.State, Number: d.Number})
	}
	return out, nil
}

func (f *fakeDOC) GetTransportDocument(_ context.Context, q docapi.GetTransportDocument) (docapi.TransportDocument, error) {
	if f.getErr != nil {
		return docapi.TransportDocument{}, f.getErr
	}
	for _, docs := range f.byShipment {
		for _, d := range docs {
			if d.DocumentID == q.DocumentID {
				return d, nil
			}
		}
	}
	return docapi.TransportDocument{}, docapi.ErrNotFound
}

func (f *fakeDOC) GetDocumentVersion(_ context.Context, q docapi.GetDocumentVersion) (docapi.DocumentVersion, error) {
	if f.versionErr != nil {
		return docapi.DocumentVersion{}, f.versionErr
	}
	return docapi.DocumentVersion{DocumentID: q.DocumentID, Doc: docapi.DocumentContent{OnBoardAt: f.onBoard[q.DocumentID]}}, nil
}

type fakeFIN struct {
	profit     finapi.ConsolProfit
	pool       finapi.ConsolCostPool
	pending    []finapi.PoolPendingItem
	noPool     bool
	profitErr  error
	poolErr    error
	pendingErr error
}

func (f *fakeFIN) GetConsolProfit(context.Context, finapi.GetConsolProfit) (finapi.ConsolProfit, error) {
	if f.profitErr != nil {
		return finapi.ConsolProfit{}, f.profitErr
	}
	if f.noPool {
		return finapi.ConsolProfit{}, finapi.ErrNotFound
	}
	return f.profit, nil
}

func (f *fakeFIN) GetConsolCostPool(context.Context, finapi.GetConsolCostPool) (finapi.ConsolCostPool, error) {
	return f.pool, f.poolErr
}

func (f *fakeFIN) GetConsolCostPoolPendingItems(context.Context, finapi.GetConsolCostPoolPendingItems) ([]finapi.PoolPendingItem, error) {
	return f.pending, f.pendingErr
}

// The stand-in data: C-1 has units 1 and 2. Unit 1's stuffing X-1 holds H01 and H07; H05 is loaded in unit 2 only. H02
// was detached with no new consolidation, H03 and H06 went to C-2, and H04's shipment was cancelled and PHY has no
// membership of it.
func standIns() (*fakePHY, *fakeSEA, *fakeDOC, *fakeFIN) {
	rework := phyapi.Rework{ReworkNo: 1, Reason: "the consignee of H03 withdrew", AskedBy: "ops-1", AskedAt: t0, BrokenSeal: phyapi.Evidence{Reference: "S1"}, BrokenSealedBy: "cfs-1",
		BrokenSealedAt: t0.Add(-time.Hour), End: phyapi.ReworkSealed, EndedBy: "cfs-2", EndedAt: t0.Add(2 * time.Hour), NewSealReference: "S2"}
	former := func(house, end, to, by string, at time.Duration) phyapi.FormerHouse {
		return phyapi.FormerHouse{ShipmentID: house, End: end, TransferredTo: to, EndedBy: by, EndedAt: t0.Add(at), ReworkUnitNo: 1, ReworkExecutionID: "X-1", ReworkAskedBy: "ops-1"}
	}
	// H08 left from container 2 after a rework of its stuffing X-2; H09 was shut out before stuffing and no rework
	// unloaded it
	unit2 := former("SH-H08", phyapi.FormerDetached, "", "ops-3", 6*time.Hour)
	unit2.ReworkUnitNo, unit2.ReworkExecutionID = 2, "X-2"
	shutOut := phyapi.FormerHouse{ShipmentID: "SH-H09", End: phyapi.FormerDetached, EndedBy: "ops-3", EndedAt: t0.Add(7 * time.Hour), ReworkFrom: phyapi.ReworkFromNone}
	f2, f4 := former("SH-H02", phyapi.FormerDetached, "", "ops-1", 3*time.Hour), former("SH-H04", phyapi.FormerCancelled, "", "ops-1", 5*time.Hour)
	f2.Reason, f4.Reason = phyapi.ReasonSpaceShortage, phyapi.ReasonShipmentCancelled
	load := func(house string, unit int, execution string, packs int64) phyapi.HouseCostBasis {
		return phyapi.HouseCostBasis{ShipmentID: house, Units: []phyapi.HouseUnitLoad{{UnitNo: unit, ExecutionID: execution, LoadedPacks: packs}}}
	}
	unloaded := func(h phyapi.HouseCostBasis, execution string, no, unit int, by string) phyapi.HouseCostBasis {
		h.UnloadedBy = phyapi.HouseUnloadedBy{ExecutionID: execution, ReworkNo: no, UnitNo: unit, AskedBy: by}
		return h
	}
	c1 := phyapi.PhysicalCostBasis{
		ConsolidationID: "C-1",
		LoadUnits:       []phyapi.PlannedLoadUnit{{UnitNo: 1, Status: phyapi.UnitStatusClosed, ExecutionID: "X-1"}, {UnitNo: 2}},
		PackingLists: []phyapi.UnitPackingList{{UnitNo: 1, ExecutionID: "X-1", ExecutionState: phyapi.ExecutionClosed, Reworks: []phyapi.Rework{rework}},
			{UnitNo: 2, ExecutionID: "X-2", ExecutionState: phyapi.ExecutionClosed, Reworks: []phyapi.Rework{{ReworkNo: 1, AskedBy: "ops-3", End: phyapi.ReworkSealed}}}},
		// H12 was unloaded in the rework of X-1 and is attached with nothing loaded, H13 in the rework of X-2, and H14
		// was unloaded in the rework of X-1 and loaded in it again
		Houses: []phyapi.HouseCostBasis{load("SH-H01", 1, "X-1", 6), load("SH-H05", 2, "X-2", 6), load("SH-H07", 1, "X-1", 2),
			unloaded(phyapi.HouseCostBasis{ShipmentID: "SH-H12"}, "X-1", 1, 1, "ops-1"), unloaded(phyapi.HouseCostBasis{ShipmentID: "SH-H13"}, "X-2", 1, 2, "ops-3"),
			unloaded(load("SH-H14", 1, "X-1", 3), "X-1", 1, 1, "ops-1"),
			// H15 was unloaded in the rework of X-1 and is loaded in unit 2 now; H16 has an entry of no packs in unit 1
			unloaded(load("SH-H15", 2, "X-2", 2), "X-1", 1, 1, "ops-1"), load("SH-H16", 1, "X-1", 0),
			// H17 was unloaded in the rework of X-1 and has an entry of no packs in unit 1: listed for the rework, not
			// in the container
			unloaded(load("SH-H17", 1, "X-1", 0), "X-1", 1, 1, "ops-1")},
		FormerHouses: []phyapi.FormerHouse{f2, former("SH-H03", phyapi.FormerTransferred, "C-2", "ops-9", 4*time.Hour), f4,
			former("SH-H06", phyapi.FormerTransferred, "C-2", "ops-9", 4*time.Hour), unit2, shutOut},
	}
	c2 := phyapi.PhysicalCostBasis{
		ConsolidationID: "C-2",
		LoadUnits:       []phyapi.PlannedLoadUnit{{UnitNo: 1, ExecutionID: "X-3"}},
		PackingLists:    []phyapi.UnitPackingList{{UnitNo: 1, ExecutionID: "X-3", ExecutionState: phyapi.ExecutionClosed}},
		Houses:          []phyapi.HouseCostBasis{load("SH-H03", 1, "X-3", 6), load("SH-H06", 1, "X-3", 2)},
	}
	entry := func(consolidation string, no int) phyapi.ConsolMembership {
		return phyapi.ConsolMembership{Live: true, Entry: phyapi.MembershipEntry{ConsolidationID: consolidation, EntryNo: no}}
	}
	phy := &fakePHY{reads: map[string]int{}, bases: map[string]phyapi.PhysicalCostBasis{"C-1": c1, "C-2": c2}, memberships: map[string]phyapi.ConsolMembership{
		"SH-H01": entry("C-1", 1), "SH-H02": {Live: false}, "SH-H03": entry("C-2", 2), "SH-H05": entry("C-1", 1), "SH-H06": entry("C-2", 3), "SH-H07": entry("C-1", 1),
		"SH-H12": entry("C-1", 1), "SH-H13": entry("C-1", 1), "SH-H14": entry("C-1", 1), "SH-H15": entry("C-1", 1), "SH-H16": entry("C-1", 1), "SH-H17": entry("C-1", 1),
	}}
	loaded := func(id, consolidation string, at time.Time) seaapi.ContainerVisit {
		return seaapi.ContainerVisit{VisitID: id, LoadUnitNo: 1, ConsolidationID: consolidation, ContainerNumber: "CSQU3054383", Status: seaapi.VisitLoaded,
			// latest first: the container was loaded again after it was rolled
			Events: []seaapi.ContainerEvent{{Kind: seaapi.EventGateOut, HappenedAt: at.Add(time.Hour)}, {Kind: seaapi.EventLoaded, HappenedAt: at}, {Kind: seaapi.EventGateIn, HappenedAt: at.Add(-time.Hour)},
				{Kind: seaapi.EventLoaded, HappenedAt: at.Add(-20 * time.Hour)}}}
	}
	// OM-1 holds, after CV-1, a visit of another unit and a visit of unit 1 of another consolidation: neither is H01's
	sea := &fakeSEA{movements: map[string]seaapi.OceanMovement{
		"OM-1": {MovementID: "OM-1", State: seaapi.StateFinal, ConsolidationID: "C-1", Houses: []seaapi.House{{ShipmentID: "SH-H01"}, {ShipmentID: "SH-H02"}, {ShipmentID: "SH-H07"}},
			Containers: []seaapi.ContainerVisit{loaded("CV-1", "C-1", t0.Add(30*time.Hour)), {VisitID: "CV-2", LoadUnitNo: 2, ConsolidationID: "C-1", Status: seaapi.VisitStuffed},
				{VisitID: "CV-9", LoadUnitNo: 1, ConsolidationID: "C-0", Status: seaapi.VisitStuffed}}},
		"OM-2": {MovementID: "OM-2", State: seaapi.StateOpen, ConsolidationID: "C-2", Containers: []seaapi.ContainerVisit{loaded("CV-3", "C-2", t0.Add(40*time.Hour))},
			Houses: []seaapi.House{{ShipmentID: "SH-H03"}}},
	}}
	bill := func(id, shipment, state, number, movement string) docapi.TransportDocument {
		return docapi.TransportDocument{DocumentID: id, Kind: docapi.KindHBL, State: state, Number: number, MovementID: movement, ShipmentID: shipment}
	}
	issued, approved, cancelled := string(docapi.StateIssued), string(docapi.StateApproved), string(docapi.StateCancelled)
	doc := &fakeDOC{byShipment: map[string][]docapi.TransportDocument{
		"SH-H01": {bill("DOC-H01", "SH-H01", issued, "SHAL1", "OM-1"), bill("DOC-H01-OLD", "SH-H01", cancelled, "SHAL9", "OM-1")},
		"SH-H02": {bill("DOC-H02", "SH-H02", approved, "SHAL2", "OM-1")},
		"SH-H03": {bill("DOC-H03", "SH-H03", issued, "SHAL3", "OM-2")},
	}, onBoard: map[string]time.Time{"DOC-H01": t0.Add(30 * time.Hour), "DOC-H02": t0.Add(31 * time.Hour), "DOC-H03": t0.Add(40 * time.Hour)}}
	return phy, sea, doc, &fakeFIN{noPool: true}
}

func reportOf(t *testing.T, phy ReworkPHY, sea ReworkSEA, doc ReworkDOC, fin ReworkFIN) ReworkedContainerReport {
	t.Helper()
	r, err := NewReworkedContainerReader(phy, sea, doc, fin).ReworkedContainerReport(context.Background(), ReworkedContainerQuery{TenantID: "T", ConsolidationID: "C-1", LoadUnitNo: 1})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// The reworks of the stuffing: the ones of a stuffing given up come first and the stuffing in force last, each rework
// as PHY holds it; a container given up with no stuffing in force has the given-up one only; a container never reworked
// has no reworks, and a unit that PHY does not have is unknown.
func TestTheReportListsTheReworksOfTheStuffing(t *testing.T) {
	phy, _, _, _ := standIns()
	r := reportOf(t, phy, nil, nil, nil)
	if len(r.Stuffings) != 1 || r.Stuffings[0].ExecutionID != "X-1" || r.Stuffings[0].State != phyapi.ExecutionClosed || r.Stuffings[0].GivenUp || len(r.Stuffings[0].Reworks) != 1 {
		t.Fatalf("the stuffing: %+v", r.Stuffings)
	}
	if w := r.Stuffings[0].Reworks[0]; w.ReworkNo != 1 || w.AskedBy != "ops-1" || w.Reason != "the consignee of H03 withdrew" || w.BrokenSeal.Reference != "S1" || w.End != phyapi.ReworkSealed ||
		!w.EndedAt.Equal(t0.Add(2*time.Hour)) || w.NewSealReference != "S2" || r.LoadUnitNo != 1 || r.ConsolidationID != "C-1" {
		t.Fatalf("the rework: %+v", w)
	}

	// given up after a second rework: the first stuffing X-1 was given up with two reworks; the unit was stuffed again
	// as X-2
	b := phy.bases["C-1"]
	b.GivenUpStuffings = []phyapi.GivenUpStuffing{{UnitNo: 1, ExecutionID: "X-0", Reworks: []phyapi.Rework{{ReworkNo: 1, AskedBy: "ask-1", End: phyapi.ReworkSealed}, {ReworkNo: 2, AskedBy: "ask-2", End: phyapi.ReworkGivenUp}}}, {UnitNo: 2, ExecutionID: "X-9"}}
	phy.bases["C-1"] = b
	r = reportOf(t, phy, nil, nil, nil)
	if len(r.Stuffings) != 2 || r.Stuffings[0].ExecutionID != "X-0" || !r.Stuffings[0].GivenUp || r.Stuffings[0].State != "" || len(r.Stuffings[0].Reworks) != 2 ||
		r.Stuffings[0].Reworks[1].End != phyapi.ReworkGivenUp || r.Stuffings[0].Reworks[1].AskedBy != "ask-2" || r.Stuffings[1].ExecutionID != "X-1" || r.Stuffings[1].GivenUp {
		t.Fatalf("a stuffing given up and the one in force: %+v", r.Stuffings)
	}

	// the container is given up and not stuffed again: the unit is planned, there is no packing list
	b.PackingLists = nil
	b.Houses, b.FormerHouses = nil, nil
	phy.bases["C-1"] = b
	r = reportOf(t, phy, nil, nil, nil)
	if len(r.Stuffings) != 1 || !r.Stuffings[0].GivenUp || r.Stuffings[0].ExecutionID != "X-0" || len(r.Houses) != 0 {
		t.Fatalf("a container given up: %+v", r)
	}

	// a unit never reworked and never stuffed is a unit: no stuffing, no reworks; one the consolidation does not have
	// is not
	b.GivenUpStuffings = nil
	phy.bases["C-1"] = b
	if r = reportOf(t, phy, nil, nil, nil); len(r.Stuffings) != 0 {
		t.Fatalf("a unit with no stuffing: %+v", r.Stuffings)
	}
	_, err := NewReworkedContainerReader(phy, nil, nil, nil).ReworkedContainerReport(context.Background(), ReworkedContainerQuery{TenantID: "T", ConsolidationID: "C-1", LoadUnitNo: 7})
	if !errors.Is(err, ErrReworkedContainerUnknown) || !strings.Contains(err.Error(), "no load unit 7") {
		t.Fatalf("an unknown unit: %v", err)
	}
	_, err = NewReworkedContainerReader(phy, nil, nil, nil).ReworkedContainerReport(context.Background(), ReworkedContainerQuery{TenantID: "T", ConsolidationID: "C-9", LoadUnitNo: 1})
	if !errors.Is(err, ErrReworkedContainerUnknown) || !strings.Contains(err.Error(), "C-9") {
		t.Fatalf("an unknown consolidation: %v", err)
	}
}

// Where each house is now: H01 still in the container, loaded on board and its bill issued; H02 detached with no new
// consolidation (in none, its bill a draft and no longer associated in SEA); H03 transferred to C-2, in its container
// there; H04 whose shipment was cancelled (in none and no document at all).
func TestTheReportSaysWhereEachHouseIsNow(t *testing.T) {
	phy, sea, doc, _ := standIns()
	m := sea.movements["OM-1"]
	m.Houses = []seaapi.House{{ShipmentID: "SH-H01"}, {ShipmentID: "SH-H07"}} // H02's association is ended
	sea.movements["OM-1"] = m
	r := reportOf(t, phy, sea, doc, nil)
	var ids []string
	for _, h := range r.Houses {
		ids = append(ids, h.ShipmentID)
	}
	// H05 is loaded in unit 2 only, so it is not the container's; the houses are in shipment order
	if want := []string{"SH-H01", "SH-H02", "SH-H03", "SH-H04", "SH-H06", "SH-H07", "SH-H12", "SH-H14", "SH-H15", "SH-H17"}; !slices.Equal(ids, want) {
		t.Fatalf("the houses: %v, want %v", ids, want)
	}
	if phy.reads["C-1"] != 1 || phy.reads["C-2"] != 1 {
		t.Fatalf("each consolidation's basis is read once: %v", phy.reads)
	}
	h01, h02, h03, h04, h06, h07, h12, h14 := r.Houses[0], r.Houses[1], r.Houses[2], r.Houses[3], r.Houses[4], r.Houses[5], r.Houses[6], r.Houses[7]
	h15, h17 := r.Houses[8], r.Houses[9]
	// H15 was unloaded in unit 1's rework and is loaded in unit 2: in this container's report it is not in the
	// container; H16's entry of no packs is no load a house a rework unloaded and that is attached with nothing loaded:
	// listed, with the rework; one loaded again in the unit is listed once, in the container and with the rework
	if h12.InContainer || h12.UnloadedBy == nil || *h12.UnloadedBy != (phyapi.HouseUnloadedBy{ExecutionID: "X-1", ReworkNo: 1, UnitNo: 1, AskedBy: "ops-1"}) || h12.Left != nil ||
		h12.Consolidation.ConsolidationID != "C-1" || !h12.Loaded.Nothing || !h12.Movements.NoDocument || !h14.InContainer || h14.UnloadedBy == nil || len(h14.Loaded.Units) != 1 ||
		h01.UnloadedBy != nil || h17.InContainer || h17.UnloadedBy == nil || h15.InContainer || h15.UnloadedBy == nil || len(h15.Loaded.Units) != 1 || h15.Loaded.Units[0].UnitNo != 2 {
		t.Fatalf("H12 and H14: %+v %+v", h12, h14)
	}
	if h06.Left == nil || h06.Consolidation.EntryNo != 3 || h06.Consolidation.ConsolidationID != "C-2" || !h07.InContainer || h07.Left != nil || !h07.Movements.NoDocument {
		t.Fatalf("H06 and H07: %+v %+v", h06, h07)
	}
	if h01.ShipmentID != "SH-H01" || !h01.InContainer || h01.Left != nil || h01.Consolidation.ConsolidationID != "C-1" || h01.Consolidation.EntryNo != 1 || len(h01.Loaded.Units) != 1 ||
		h01.Loaded.Units[0].ExecutionState != phyapi.ExecutionClosed || h01.Loaded.Units[0].Packs != 6 || len(h01.Movements.Found) != 1 {
		t.Fatalf("H01: %+v", h01)
	}
	if mv := h01.Movements.Found[0]; mv.MovementID != "OM-1" || mv.State != seaapi.StateFinal || !mv.Associated || mv.Visit == nil || mv.Visit.VisitID != "CV-1" || !mv.Visit.OnBoard ||
		!mv.Visit.OnBoardAt.Equal(t0.Add(30*time.Hour)) || mv.NoVisit {
		t.Fatalf("H01 in SEA: %+v", mv)
	}
	if d := h01.Documents.Documents; len(d) != 2 || d[1].State != string(docapi.StateCancelled) || d[0].Kind != docapi.KindHBL || d[0].Number != "SHAL1" || d[0].State != string(docapi.StateIssued) || !d[0].OnBoardAt.Equal(t0.Add(30*time.Hour)) {
		t.Fatalf("H01's documents: %+v", d)
	}
	// detached, in no consolidation: it says so; PHY is not asked what it has loaded; SEA's association is ended and
	// the bill is a draft with no date
	if h02.ShipmentID != "SH-H02" || h02.InContainer || h02.Left == nil || h02.Left.End != phyapi.FormerDetached || h02.Left.EndedBy != "ops-1" || !h02.Consolidation.InNone ||
		h02.Consolidation.ConsolidationID != "" || len(h02.Loaded.Units) != 0 || h02.Loaded.Nothing {
		t.Fatalf("H02: %+v", h02)
	}
	if mv := h02.Movements.Found[0]; mv.MovementID != "OM-1" || mv.Associated || mv.Visit != nil || !mv.NoVisit {
		t.Fatalf("H02 in SEA: %+v", mv)
	}
	if d := h02.Documents.Documents[0]; d.State != string(docapi.StateApproved) || !d.OnBoardAt.IsZero() {
		t.Fatalf("H02's document: %+v", d)
	}
	// transferred: how, to where, by whom and when, and where it is loaded now
	if h03.InContainer || h03.Left == nil || h03.Left.End != phyapi.FormerTransferred || h03.Left.TransferredTo != "C-2" || h03.Left.EndedBy != "ops-9" || h03.Consolidation.ConsolidationID != "C-2" ||
		h03.Consolidation.EntryNo != 2 || len(h03.Loaded.Units) != 1 || h03.Loaded.Units[0].ExecutionID != "X-3" {
		t.Fatalf("H03: %+v", h03)
	}
	if mv := h03.Movements.Found[0]; mv.MovementID != "OM-2" || mv.State != seaapi.StateOpen || !mv.Associated || mv.Visit == nil || mv.Visit.VisitID != "CV-3" || !mv.Visit.OnBoardAt.Equal(t0.Add(40*time.Hour)) {
		t.Fatalf("H03 in SEA: %+v", mv)
	}
	// the shipment cancelled: in none, no bill, so no movement is found and it is said so
	if h04.Left == nil || h04.Left.End != phyapi.FormerCancelled || h04.Left.Reason != phyapi.ReasonShipmentCancelled || !h04.Consolidation.InNone || len(h04.Documents.Documents) != 0 ||
		!h04.Movements.NoDocument || len(h04.Movements.Found) != 0 || h04.Movements.Unanswered != "" || h04.Documents.Unanswered != "" {
		t.Fatalf("H04: %+v", h04)
	}

	// a visit not yet loaded is not on board; a house loaded in two units has no visit to name; a house attached with
	// nothing loaded says so
	sea.movements["OM-2"] = seaapi.OceanMovement{MovementID: "OM-2", State: seaapi.StateOpen, ConsolidationID: "C-2", Houses: []seaapi.House{{ShipmentID: "SH-H03"}},
		Containers: []seaapi.ContainerVisit{{VisitID: "CV-3", LoadUnitNo: 1, ConsolidationID: "C-2", Status: seaapi.VisitStuffed}}}
	if v := reportOf(t, phy, sea, doc, nil).Houses[2].Movements.Found[0].Visit; v == nil || v.OnBoard || !v.OnBoardAt.IsZero() || v.Status != seaapi.VisitStuffed {
		t.Fatalf("a visit not loaded: %+v", v)
	}
	c2 := phy.bases["C-2"]
	c2.Houses = []phyapi.HouseCostBasis{{ShipmentID: "SH-H03", Units: []phyapi.HouseUnitLoad{{UnitNo: 1, ExecutionID: "X-3", LoadedPacks: 3}, {UnitNo: 2, ExecutionID: "X-4", LoadedPacks: 3}}}}
	phy.bases["C-2"] = c2
	if g := reportOf(t, phy, sea, doc, nil).Houses[2]; len(g.Loaded.Units) != 2 || !g.Movements.Found[0].NoVisit || g.Movements.Found[0].Visit != nil {
		t.Fatalf("a house in two units: %+v", g)
	}
	c2.Houses = []phyapi.HouseCostBasis{{ShipmentID: "SH-H03"}}
	phy.bases["C-2"] = c2
	// a visit assigned to no unit is nobody's: it is not named for a house with nothing loaded
	om2 := sea.movements["OM-2"]
	om2.Containers = append(slices.Clone(om2.Containers), seaapi.ContainerVisit{VisitID: "CV-0", LoadUnitNo: 0, ConsolidationID: "C-2", Status: seaapi.VisitAssigned})
	sea.movements["OM-2"] = om2
	if g := reportOf(t, phy, sea, doc, nil).Houses[2]; !g.Loaded.Nothing || len(g.Loaded.Units) != 0 || !g.Movements.Found[0].NoVisit {
		t.Fatalf("a house attached with nothing loaded: %+v", g)
	}
}

func share(house string, amount int64) finapi.PoolShare {
	return finapi.PoolShare{ShipmentID: house, JobID: "JOB-" + strings.TrimPrefix(house, "SH-"), Amount: cny(amount), Local: cny(amount), Unit: "MWM"}
}

func attribution(house string, loss, exception int64, party, reference string) finapi.PoolAttribution {
	a := finapi.PoolAttribution{ShipmentID: house, JobID: "JOB-" + strings.TrimPrefix(house, "SH-"), Loss: cny(loss), LossLocal: cny(loss), Exception: cny(exception), ExceptionLocal: cny(exception),
		Party: party, PartyReference: reference, Voucher: "CLAIM-1", DepartedAt: t0.Add(3 * time.Hour)}
	return a
}

// poolOf is a pool of CNY lines with one apportionment covering the lines given.
func poolOf(lines []finapi.PoolCostLine, covered ...finapi.ApportionedLine) finapi.ConsolCostPool {
	p := finapi.ConsolCostPool{PoolID: "P-1", ConsolidationID: "C-1", State: finapi.PoolOpen, Lines: lines}
	if len(covered) > 0 {
		p.Apportionments = []finapi.PoolApportionment{{ApportionmentNo: 1, Lines: covered}}
	}
	return p
}

func cnyLine(no int, code, method string, cost int64) finapi.PoolCostLine {
	return finapi.PoolCostLine{LineNo: no, ChargeCode: code, Method: method, Cost: cny(cost), CostLocal: cny(cost)}
}

// Who bears each cost, line by line, under the apportionment in force: a line shared by the live houses with the loss
// and the exception cost of a house that left; a line with a former share still pending, a line to be allocated, a line
// retained, one no apportionment covers and one in USD whose two currencies conserve each. The totals are FIN's.
func TestTheReportSaysWhoBearsEachCost(t *testing.T) {
	phy, _, _, _ := standIns()
	usd := func(minor, local int64) (kernel.Money, kernel.Money) {
		return kernel.Money{Currency: "USD", Minor: minor}, cny(local)
	}
	ucost, ulocal := usd(10_000, 71_234)
	uloss, ulossLocal := usd(1_000, 7_123)
	uline := finapi.PoolCostLine{LineNo: 6, ChargeCode: "THC", Method: finapi.MethodChargeable, Cost: ucost, CostLocal: ulocal}
	lines := []finapi.PoolCostLine{cnyLine(1, "OCEAN_FREIGHT", finapi.MethodChargeable, 300_000), cnyLine(2, "THC", finapi.MethodTEU, 50_000), cnyLine(3, "LASHING", finapi.MethodGrossWeight, 30_000),
		cnyLine(4, "DOCS", finapi.MethodRetained, 20_000), cnyLine(5, "INSURANCE", finapi.MethodChargeable, 7_000), uline,
		cnyLine(7, "CUSTOMS", finapi.MethodChargeable, 10_000)}
	attr1 := attribution("SH-H03", 10_000, 20_000, finapi.PartyCarrier, "line-1")
	attr3 := attribution("SH-H03", 0, 5_000, finapi.PartyPending, "")
	attr4 := attribution("SH-H02", 0, 5_000, finapi.PartyWarehouse, "cfs-1")
	usdAttr := attribution("SH-H03", 0, 0, finapi.PartyCarrier, "line-1")
	uexc, uexcLocal := usd(2_000, 14_247)
	usdAttr.Loss, usdAttr.LossLocal = uloss, ulossLocal
	usdAttr.Exception, usdAttr.ExceptionLocal = uexc, uexcLocal
	pool := poolOf(lines,
		finapi.ApportionedLine{LineNo: 1, Method: finapi.MethodChargeable, Amount: cny(300_000), Local: cny(300_000), Shares: []finapi.PoolShare{share("SH-H01", 108_000), share("SH-H02", 162_000)},
			Attributions: []finapi.PoolAttribution{attr1}},
		// line 2: H01 bears 25,000, H03's 25,000 is a former share nobody attributed yet, and its NotUsed share is not
		// shown
		finapi.ApportionedLine{LineNo: 2, Method: finapi.MethodTEU, Amount: cny(50_000), Local: cny(50_000), Shares: []finapi.PoolShare{share("SH-H01", 25_000), share("SH-H03", 25_000),
			{ShipmentID: "SH-H02", NotUsed: true, Amount: cny(0), Local: cny(0)}, {ShipmentID: "SH-H05", NotUsed: true, Amount: cny(0), Local: cny(0)}}},
		// line 3: no house is live, what an exception cost of 5,000 leaves is to be allocated
		finapi.ApportionedLine{LineNo: 3, Method: finapi.MethodGrossWeight, Amount: cny(30_000), Local: cny(30_000), ToAllocate: cny(25_000), ToAllocateLocal: cny(25_000), Attributions: []finapi.PoolAttribution{attr3}},
		finapi.ApportionedLine{LineNo: 4, Method: finapi.MethodRetained, Amount: cny(20_000), Local: cny(20_000), Attributions: []finapi.PoolAttribution{attr4}},
		finapi.ApportionedLine{LineNo: 6, Method: finapi.MethodChargeable, Amount: ucost, Local: ulocal, Shares: []finapi.PoolShare{{ShipmentID: "SH-H01", Amount: kernel.Money{Currency: "USD", Minor: 7_000}, Local: cny(49_864)}},
			Attributions: []finapi.PoolAttribution{usdAttr}},
		// line 7: its cost was revised to 10,000 after it was apportioned at 8,000, so 2,000 is not apportioned
		finapi.ApportionedLine{LineNo: 7, Method: finapi.MethodChargeable, Amount: cny(8_000), Local: cny(8_000), Shares: []finapi.PoolShare{share("SH-H01", 8_000)}})
	pool.Apportionments[0].ApportionmentNo = 3
	pool.Attributions = []finapi.LineAttributions{{LineNo: 2, FormerShares: []finapi.FormerShare{{ShipmentID: "SH-H03", Amount: cny(25_000), Local: cny(25_000)}}}}
	fin := &fakeFIN{pool: pool, profit: finapi.ConsolProfit{PoolID: "P-1", Pooled: cny(300_000 + 50_000 + 30_000 + 20_000 + 7_000 + 71_234 + 10_000), Apportioned: cny(108_000 + 162_000 + 25_000 + 49_864 + 8_000),
		Retained: cny(15_000), Losses: cny(10_000 + 7_123), Exceptions: cny(20_000 + 5_000 + 5_000 + 14_247), ExceptionsPending: cny(5_000), ToAllocated: cny(25_000), FormerSharesPending: cny(25_000),
		Unapportioned: cny(7_000 + 2_000)}}
	// the totals are FIN's: Pooled 488,234 = 352,864 + 15,000 + 17,123 + 44,247 + 25,000 + 25,000 + 9,000
	r := reportOf(t, phy, nil, nil, fin)
	c := r.Costs
	if c.NoPool || c.Unanswered != "" || c.PoolID != "P-1" || c.State != finapi.PoolOpen || len(c.Lines) != 7 || !strings.Contains(c.Scope, "the consolidation's pool") {
		t.Fatalf("the costs: %+v", c)
	}
	l1 := c.Lines[0]
	if l1.LineNo != 1 || l1.ChargeCode != "OCEAN_FREIGHT" || l1.Cost != cny(300_000) || l1.ApportionmentNo != 3 || len(l1.Shares) != 2 || l1.Shares[0].Amount != cny(108_000) || l1.Shares[1].Amount != cny(162_000) ||
		len(l1.Losses) != 1 || l1.Losses[0].ShipmentID != "SH-H03" || l1.Losses[0].UnitNo != 1 || l1.Exceptions[0].UnitNo != 1 || l1.Losses[0].Amount != cny(10_000) || l1.Losses[0].Voucher != "CLAIM-1" || !l1.Losses[0].DepartedAt.Equal(t0.Add(3*time.Hour)) ||
		len(l1.Exceptions) != 1 || l1.Exceptions[0].Party != finapi.PartyCarrier || l1.Exceptions[0].Reference != "line-1" || l1.Exceptions[0].Amount != cny(20_000) || l1.Unapportioned != cny(0) ||
		l1.UnapportionedLo != cny(0) || len(l1.FormerPending) != 0 {
		t.Fatalf("line 1: %+v", l1)
	}
	l2 := c.Lines[1]
	// H05 is live and was left out of the line: its share stays, as a share of nothing
	if len(l2.Shares) != 2 || l2.Shares[0].ShipmentID != "SH-H01" || l2.Shares[1].ShipmentID != "SH-H05" || !l2.Shares[1].NotUsed || len(l2.FormerPending) != 1 || l2.FormerPending[0].ShipmentID != "SH-H03" ||
		l2.FormerPending[0].UnitNo != 1 || l2.FormerPending[0].Amount != cny(25_000) || l2.Unapportioned != cny(0) {
		t.Fatalf("line 2, a former share still pending: %+v", l2)
	}
	if l3 := c.Lines[2]; len(l3.Shares) != 0 || len(l3.Exceptions) != 1 || l3.Exceptions[0].Party != finapi.PartyPending || l3.ToAllocate != cny(25_000) || l3.ToAllocateLocal != cny(25_000) || l3.Unapportioned != cny(0) {
		t.Fatalf("line 3, to be allocated and an exception cost pending: %+v", l3)
	}
	// a retained line: what the exception cost leaves is what the consolidation retains
	if l4 := c.Lines[3]; l4.Retained != cny(15_000) || l4.RetainedLocal != cny(15_000) || l4.Unapportioned != cny(0) || len(l4.Exceptions) != 1 || l4.Exceptions[0].Party != finapi.PartyWarehouse {
		t.Fatalf("line 4, retained: %+v", l4)
	}
	// no apportionment covers line 5: the whole cost is unapportioned, in both currencies
	if l5 := c.Lines[4]; l5.ApportionmentNo != 0 || l5.Unapportioned != cny(7_000) || l5.UnapportionedLo != cny(7_000) || len(l5.Shares) != 0 {
		t.Fatalf("line 5, not apportioned: %+v", l5)
	}
	// the USD line conserves in USD and in CNY: 7,000 + 1,000 + 2,000 = 10,000 and 49,864 + 7,123 + 14,247 = 71,234
	if l6 := c.Lines[5]; l6.Cost != ucost || l6.Local != ulocal || l6.Unapportioned.Minor != 0 || l6.UnapportionedLo.Minor != 0 || l6.Unapportioned.Currency != "USD" || l6.UnapportionedLo.Currency != "CNY" ||
		len(l6.Losses) != 1 || l6.Losses[0].Amount != uloss || l6.Losses[0].Local != ulossLocal || len(l6.Exceptions) != 1 || l6.Exceptions[0].Amount != uexc || l6.Exceptions[0].Local != uexcLocal {
		t.Fatalf("line 6, USD: %+v", l6)
	}
	// a line whose cost was revised after its apportionment: what the apportionment does not cover is unapportioned
	if l7 := c.Lines[6]; l7.ApportionmentNo != 3 || l7.Cost != cny(10_000) || l7.Unapportioned != cny(2_000) || l7.UnapportionedLo != cny(2_000) || len(l7.Shares) != 1 {
		t.Fatalf("line 7, revised after its apportionment: %+v", l7)
	}
	if tt := c.Totals; tt.Pooled != cny(488_234) || tt.Apportioned != cny(352_864) || tt.Retained != cny(15_000) || tt.Losses != cny(17_123) || tt.Exceptions != cny(44_247) ||
		tt.ExceptionsPending != cny(5_000) || tt.ToAllocated != cny(25_000) || tt.FormerSharesPending != cny(25_000) || tt.Unapportioned != cny(9_000) || !tt.Conserved {
		t.Fatalf("the totals: %+v", tt)
	}
	// totals that do not conserve are told so, not repaired
	fin.profit.Apportioned = cny(1)
	if r = reportOf(t, phy, nil, nil, fin); r.Costs.Totals.Conserved {
		t.Fatalf("totals that do not conserve: %+v", r.Costs.Totals)
	}
	// totals that conserve while the lines say otherwise are not conserved either: a loss and an exception cost differ
	// by one unit in the totals and not in the lines
	fin.profit.Apportioned = cny(352_864)
	fin.profit.Losses, fin.profit.Exceptions = cny(17_123+1), cny(44_247-1)
	if r = reportOf(t, phy, nil, nil, fin); r.Costs.Totals.Conserved {
		t.Fatalf("lines that do not add up to FIN's totals: %+v", r.Costs.Totals)
	}
	// a closed pool and a pool in work are told so; a consolidation with no pool says so
	fin.pool.State, fin.pool.InWork = finapi.PoolClosed, true
	if r = reportOf(t, phy, nil, nil, fin); r.Costs.State != finapi.PoolClosed || !r.Costs.InWork {
		t.Fatalf("the pool: %+v", r.Costs)
	}
	fin.noPool = true
	if r = reportOf(t, phy, nil, nil, fin); !r.Costs.NoPool || len(r.Costs.Lines) != 0 || !r.PendingNoPool || r.Costs.Unanswered != "" || len(r.Recoveries) != 0 {
		t.Fatalf("no pool: %+v", r)
	}
}

// What there is to recover is FIN's attribution read as the owner's rule says: an exception cost of the carrier or the
// warehouse is to be recovered from that party (its reference), a pending one needs the responsibility to be
// established, one of the own operations is not recoverable and is listed so that the total is complete, a loss may be
// charged to the house's customer and the system does not decide, and a former share pending decides nothing. Claims
// are not built.
func TestTheReportSaysWhatThereIsToRecover(t *testing.T) {
	phy, _, _, _ := standIns()
	lines := []finapi.PoolCostLine{cnyLine(1, "OCEAN_FREIGHT", finapi.MethodChargeable, 300_000), cnyLine(2, "THC", finapi.MethodChargeable, 50_000)}
	pool := poolOf(lines,
		finapi.ApportionedLine{LineNo: 1, Method: finapi.MethodChargeable, Amount: cny(300_000), Local: cny(300_000), Shares: []finapi.PoolShare{share("SH-H01", 245_000)},
			Attributions: []finapi.PoolAttribution{attribution("SH-H02", 0, 10_000, finapi.PartyOwnOperations, ""), attribution("SH-H03", 20_000, 5_000, finapi.PartyCarrier, "line-1"),
				attribution("SH-H04", 0, 20_000, finapi.PartyPending, ""),
				attribution("SH-H07", 4_000, 0, "", "")}},
		finapi.ApportionedLine{LineNo: 2, Method: finapi.MethodChargeable, Amount: cny(50_000), Local: cny(50_000), Shares: []finapi.PoolShare{share("SH-H01", 25_000), share("SH-H02", 10_000)},
			Attributions: []finapi.PoolAttribution{attribution("SH-H04", 0, 5_000, finapi.PartyWarehouse, "cfs-1"), attribution("SH-H03", 0, 3_000, "STRANGER", "x")}})
	pool.Attributions = []finapi.LineAttributions{{LineNo: 2, FormerShares: []finapi.FormerShare{{ShipmentID: "SH-H06", Amount: cny(7_000), Local: cny(7_000)},
		{ShipmentID: "SH-H05", Amount: cny(9_000), Local: cny(9_000)}}}}
	fin := &fakeFIN{pool: pool, profit: finapi.ConsolProfit{PoolID: "P-1"}}
	r := reportOf(t, phy, nil, nil, fin)
	type row struct {
		kind, house, party, ref string
		line                    int
		amount                  int64
	}
	var got []row
	for _, x := range r.Recoveries {
		if x.Local != x.Amount || x.Note == "" || x.Amount.Minor == 0 {
			t.Fatalf("a recovery with no note or another local amount: %+v", x)
		}
		got = append(got, row{x.Kind, x.ShipmentID, x.Party, x.Reference, x.LineNo, x.Amount.Minor})
	}
	want := []row{
		{RecoverFromParty, "SH-H03", finapi.PartyCarrier, "line-1", 1, 5_000},
		{RecoverFromParty, "SH-H04", finapi.PartyWarehouse, "cfs-1", 2, 5_000},
		{RecoverToEstablish, "SH-H04", finapi.PartyPending, "", 1, 20_000},
		{RecoverNotRecoverable, "SH-H02", finapi.PartyOwnOperations, "", 1, 10_000},
		{RecoverChargeToCustomer, "SH-H03", "", "", 1, 20_000},
		{RecoverChargeToCustomer, "SH-H07", "", "", 1, 4_000},
		{RecoverFormerSharePend, "SH-H06", "", "", 2, 7_000},
		{RecoverUnclassifiedParty, "SH-H03", "STRANGER", "x", 2, 3_000},
	}
	if l := r.Costs.Lines[1]; len(l.FormerPending) != 2 || l.FormerPending[1].ShipmentID != "SH-H05" {
		t.Fatalf("the former share of a house this container does not list is still in the pool's costs: %+v", l.FormerPending)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("what there is to recover:\n got %+v\nwant %+v", got, want)
	}
	for _, x := range r.Recoveries {
		if x.Kind == RecoverFormerSharePend && x.UnitNo != 1 {
			t.Fatalf("the former share of a house that left from unit 1: %+v", x)
		}
		switch x.Kind {
		case RecoverChargeToCustomer:
			if !strings.Contains(x.Note, "the system does not decide") || x.Voucher != "CLAIM-1" {
				t.Fatalf("the loss: %+v", x)
			}
		case RecoverFormerSharePend:
			if !strings.Contains(x.Note, "nothing is decided yet") {
				t.Fatalf("the former share: %+v", x)
			}
		case RecoverNotRecoverable:
			if !strings.Contains(x.Note, "not recoverable") {
				t.Fatalf("own operations: %+v", x)
			}
		}
	}
	if !strings.Contains(r.RecoveriesNote, "Claims are not built") {
		t.Fatalf("the note: %q", r.RecoveriesNote)
	}
	// a line with nothing to attribute recovers nothing
	pool = poolOf(lines[:1], finapi.ApportionedLine{LineNo: 1, Method: finapi.MethodChargeable, Amount: cny(300_000), Local: cny(300_000), Shares: []finapi.PoolShare{share("SH-H01", 300_000)}})
	if r = reportOf(t, phy, nil, nil, &fakeFIN{pool: pool, profit: finapi.ConsolProfit{PoolID: "P-1"}}); len(r.Recoveries) != 0 {
		t.Fatalf("nothing to recover: %+v", r.Recoveries)
	}
}

// FIN's pending items are listed as FIN gives them, in FIN's order; an empty list is an empty list, and a consolidation
// with no pool has none.
func TestTheReportListsFINsPendingItems(t *testing.T) {
	phy, _, _, _ := standIns()
	items := []finapi.PoolPendingItem{{Kind: finapi.PendingFormerSharePending, LineNo: 1, ShipmentID: "SH-H03", Amount: cny(50_000), Actor: finapi.ActorFinance, Message: "m1"},
		{Kind: finapi.PendingExceptionCause, LineNo: 1, ShipmentID: "SH-H03", Amount: cny(20_000), Actor: finapi.ActorFinance, Message: "m2"}}
	fin := &fakeFIN{pool: poolOf(nil), profit: finapi.ConsolProfit{PoolID: "P-1"}, pending: items}
	r := reportOf(t, phy, nil, nil, fin)
	if !slices.Equal(r.Pending, items) || r.PendingNoPool || r.PendingUnanswered != "" {
		t.Fatalf("the pending items: %+v", r)
	}
	fin.pending = []finapi.PoolPendingItem{}
	if r = reportOf(t, phy, nil, nil, fin); len(r.Pending) != 0 || r.PendingUnanswered != "" || r.PendingNoPool {
		t.Fatalf("nothing pending: %+v", r)
	}
	fin.noPool = true
	if r = reportOf(t, phy, nil, nil, fin); !r.PendingNoPool || len(r.Pending) != 0 || r.PendingUnanswered != "" {
		t.Fatalf("no pool: %+v", r)
	}
}

// A context that does not answer makes its own part say so and nothing else: SEA, DOC and FIN that are not wired or
// fail, PHY's membership or another consolidation's basis that fails. PHY's basis of the consolidation is the anchor:
// without it the report fails.
func TestTheReportSaysWhatItsContextsDoNotAnswer(t *testing.T) {
	phy, sea, doc, _ := standIns()
	boom := errors.New("down")

	r := reportOf(t, phy, nil, nil, nil)
	h := r.Houses[0]
	if h.Movements.Unanswered != "SEA is not wired" || h.Documents.Unanswered != "DOC is not wired" || r.Costs.Unanswered != "FIN is not wired" || r.PendingUnanswered != "FIN is not wired" ||
		r.Pending != nil || len(r.Recoveries) != 0 || len(r.Stuffings) != 1 || h.Consolidation.ConsolidationID != "C-1" {
		t.Fatalf("SEA, DOC and FIN not wired: %+v", r)
	}
	// DOC down: SEA is found through DOC's documents, so SEA says that
	r = reportOf(t, phy, sea, &fakeDOC{err: boom}, nil)
	if h = r.Houses[0]; !strings.Contains(h.Documents.Unanswered, "DOC: down") || !strings.Contains(h.Movements.Unanswered, "DOC did not answer") || len(h.Documents.Documents) != 0 {
		t.Fatalf("DOC down: %+v", h)
	}
	// a document DOC lists and fails to read
	doc.getErr = boom
	if h = reportOf(t, phy, sea, doc, nil).Houses[0]; !strings.Contains(h.Documents.Unanswered, "DOC: down") || len(h.Documents.Documents) != 0 {
		t.Fatalf("a document that fails to read: %+v", h)
	}
	doc.getErr = nil
	// the version of an issued bill that DOC fails to read: not a bill with no date
	doc.versionErr = boom
	if h = reportOf(t, phy, sea, doc, nil).Houses[0]; !strings.Contains(h.Documents.Unanswered, "DOC: down") || len(h.Documents.Documents) != 0 {
		t.Fatalf("a version that fails to read: %+v", h)
	}
	doc.versionErr = nil
	// SEA down
	r = reportOf(t, phy, &fakeSEA{err: boom}, doc, nil)
	if h = r.Houses[0]; !strings.Contains(h.Movements.Unanswered, "SEA: down") || len(h.Movements.Found) != 0 || len(h.Documents.Documents) != 2 {
		t.Fatalf("SEA down: %+v", h)
	}
	// FIN down at each of its three reads
	for name, fin := range map[string]*fakeFIN{"the profit": {profitErr: boom}, "the pool": {profit: finapi.ConsolProfit{PoolID: "P-1"}, poolErr: boom},
		"the pending items": {pool: poolOf(nil), profit: finapi.ConsolProfit{PoolID: "P-1"}, pendingErr: boom}} {
		r = reportOf(t, phy, sea, doc, fin)
		if name == "the pending items" {
			if r.PendingUnanswered != "FIN: down" || r.Costs.Unanswered != "" || r.Costs.PoolID != "P-1" {
				t.Fatalf("FIN down at %s: %+v", name, r)
			}
			continue
		}
		if r.Costs.Unanswered != "FIN: down" || r.PendingUnanswered != "FIN: down" || len(r.Costs.Lines) != 0 || len(r.Recoveries) != 0 || len(r.Houses) != 10 {
			t.Fatalf("FIN down at %s: %+v", name, r)
		}
	}
	// PHY's membership fails: the consolidation of the house is not answered, SEA still is
	phy.failMember = boom
	if h = reportOf(t, phy, sea, doc, nil).Houses[0]; h.Consolidation.Unanswered != "PHY: down" || h.Consolidation.InNone || len(h.Loaded.Units) != 0 || len(h.Documents.Documents) != 2 {
		t.Fatalf("PHY's membership down: %+v", h)
	}
	phy.failMember = nil
	// the basis of the consolidation a house is in now fails: what it has loaded there is not answered
	phy.failBasis = map[string]error{"C-2": boom}
	if h = reportOf(t, phy, sea, doc, nil).Houses[2]; h.Loaded.Unanswered != "PHY: down" || h.Consolidation.ConsolidationID != "C-2" || h.Movements.Found[0].Visit != nil || !h.Movements.Found[0].NoVisit {
		t.Fatalf("PHY's basis of C-2 down: %+v", h)
	}
	phy.failBasis = map[string]error{"C-1": boom}
	_, err := NewReworkedContainerReader(phy, sea, doc, nil).ReworkedContainerReport(context.Background(), ReworkedContainerQuery{TenantID: "T", ConsolidationID: "C-1", LoadUnitNo: 1})
	if !errors.Is(err, ErrReworkPHYMissing) || !strings.Contains(err.Error(), "down") {
		t.Fatalf("PHY down: %v", err)
	}
	_, err = NewReworkedContainerReader(nil, sea, doc, nil).ReworkedContainerReport(context.Background(), ReworkedContainerQuery{TenantID: "T", ConsolidationID: "C-1", LoadUnitNo: 1})
	if !errors.Is(err, ErrReworkPHYMissing) {
		t.Fatalf("PHY not wired: %v", err)
	}
}

// The report is of one container: in a consolidation with two containers, each reworked, a house that left from
// container 2 and a house shut out before stuffing (no rework unloaded it) are not in container 1's report, and
// container 1's houses are not in container 2's. A former house counts for a unit only when both the unit and the
// execution of its rework are the unit's, a stuffing given up included.
func TestTheReportListsOnlyTheHousesOfItsOwnContainer(t *testing.T) {
	phy, _, _, _ := standIns()
	houses := func(unit int) (out []string) {
		r, err := NewReworkedContainerReader(phy, nil, nil, nil).ReworkedContainerReport(context.Background(), ReworkedContainerQuery{TenantID: "T", ConsolidationID: "C-1", LoadUnitNo: unit})
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range r.Houses {
			out = append(out, h.ShipmentID)
		}
		return out
	}
	if got, want := houses(1), []string{"SH-H01", "SH-H02", "SH-H03", "SH-H04", "SH-H06", "SH-H07", "SH-H12", "SH-H14", "SH-H15", "SH-H17"}; !slices.Equal(got, want) {
		t.Fatalf("container 1: %v, want %v", got, want)
	}
	if got, want := houses(2), []string{"SH-H05", "SH-H08", "SH-H13", "SH-H15"}; !slices.Equal(got, want) {
		t.Fatalf("container 2: %v, want %v", got, want)
	}
	// a house whose rework names the unit but another execution is not the unit's (the stuffing is not one of the
	// unit's)
	b := phy.bases["C-1"]
	b.FormerHouses = append(slices.Clone(b.FormerHouses), phyapi.FormerHouse{ShipmentID: "SH-H10", End: phyapi.FormerDetached, ReworkUnitNo: 1, ReworkExecutionID: "X-7"})
	phy.bases["C-1"] = b
	if got := houses(1); slices.Contains(got, "SH-H10") {
		t.Fatalf("a rework of another execution: %v", got)
	}
	// a rework of the unit's execution recorded against another unit is not the unit's either
	b.FormerHouses = append(slices.Clone(b.FormerHouses), phyapi.FormerHouse{ShipmentID: "SH-H11", End: phyapi.FormerDetached, ReworkUnitNo: 2, ReworkExecutionID: "X-1"})
	phy.bases["C-1"] = b
	if got := houses(1); slices.Contains(got, "SH-H11") {
		t.Fatalf("a rework of another unit: %v", got)
	}
	// a stuffing given up after a rework counts: its former house is the unit's
	b.GivenUpStuffings = []phyapi.GivenUpStuffing{{UnitNo: 1, ExecutionID: "X-7", Reworks: []phyapi.Rework{{ReworkNo: 1, End: phyapi.ReworkGivenUp}}}}
	phy.bases["C-1"] = b
	if got := houses(1); !slices.Contains(got, "SH-H10") {
		t.Fatalf("a rework of a stuffing given up: %v", got)
	}
}

// The costs are the consolidation's pool and are the same in every container's report, each loss and exception cost
// tagged with its house and the unit it left from; what there is to recover is of the container's own houses. H03 left
// from container 1 with a loss, H08 from container 2 with a loss and an exception cost of the carrier: container 1
// recovers H03's and not H08's, container 2 recovers H08's and not H03's.
func TestTheReportRecoversOnlyWhatItsOwnHousesGiveRiseTo(t *testing.T) {
	phy, _, _, _ := standIns()
	lines := []finapi.PoolCostLine{cnyLine(1, "OCEAN_FREIGHT", finapi.MethodChargeable, 100_000)}
	pool := poolOf(lines, finapi.ApportionedLine{LineNo: 1, Method: finapi.MethodChargeable, Amount: cny(100_000), Local: cny(100_000), Shares: []finapi.PoolShare{share("SH-H01", 65_000)},
		Attributions: []finapi.PoolAttribution{attribution("SH-H03", 5_000, 0, "", ""), attribution("SH-H08", 10_000, 20_000, finapi.PartyCarrier, "line-1")}})
	fin := &fakeFIN{pool: pool, profit: finapi.ConsolProfit{PoolID: "P-1", Pooled: cny(100_000), Apportioned: cny(65_000), Losses: cny(15_000), Exceptions: cny(20_000)}}
	read := func(unit int) ReworkedContainerReport {
		t.Helper()
		r, err := NewReworkedContainerReader(phy, nil, nil, fin).ReworkedContainerReport(context.Background(), ReworkedContainerQuery{TenantID: "T", ConsolidationID: "C-1", LoadUnitNo: unit})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	kinds := func(r ReworkedContainerReport) (out []string) {
		for _, x := range r.Recoveries {
			out = append(out, x.Kind+" "+x.ShipmentID)
		}
		return out
	}
	r1, r2 := read(1), read(2)
	if got, want := kinds(r1), []string{RecoverChargeToCustomer + " SH-H03"}; !slices.Equal(got, want) || r1.Recoveries[0].UnitNo != 1 {
		t.Fatalf("container 1: %v %+v", got, r1.Recoveries)
	}
	if got, want := kinds(r2), []string{RecoverFromParty + " SH-H08", RecoverChargeToCustomer + " SH-H08"}; !slices.Equal(got, want) || r2.Recoveries[0].UnitNo != 2 {
		t.Fatalf("container 2: %v %+v", got, r2.Recoveries)
	}
	for _, r := range []ReworkedContainerReport{r1, r2} {
		l := r.Costs.Lines[0]
		if len(l.Losses) != 2 || l.Losses[0].UnitNo != 1 || l.Losses[1].UnitNo != 2 || len(l.Exceptions) != 1 || l.Exceptions[0].UnitNo != 2 || !r.Costs.Totals.Conserved ||
			r.Costs.Totals.Losses != cny(15_000) || !strings.Contains(r.Costs.Scope, "the consolidation's pool") {
			t.Fatalf("the pool's costs in unit %d's report: %+v", r.LoadUnitNo, r.Costs)
		}
	}
}

// The conservation check is called on lines and totals that agree and then on each way they can disagree, one at a
// time: a line whose parts do not add up to its cost, and lines that do not add up to each of FIN's totals.
func TestLinesAgreeWithTheirPartsAndWithEachOfFINsTotals(t *testing.T) {
	line := CostLineBorne{LineNo: 1, Local: cny(100), ToAllocateLocal: cny(5), RetainedLocal: cny(5), UnapportionedLo: cny(15),
		Shares:        []finapi.PoolShare{{Local: cny(40)}},
		Losses:        []LossBorne{{Local: cny(10)}},
		Exceptions:    []ExceptionBorne{{Party: finapi.PartyCarrier, Local: cny(10)}, {Party: finapi.PartyPending, Local: cny(5)}},
		FormerPending: []FormerShareBorne{{Local: cny(10)}}}
	totals := CostTotals{Pooled: cny(100), Apportioned: cny(40), Retained: cny(5), Losses: cny(10), Exceptions: cny(15), ExceptionsPending: cny(5), ToAllocated: cny(5),
		FormerSharesPending: cny(10), Unapportioned: cny(15)}
	if !linesAgree([]CostLineBorne{line}, totals) {
		t.Fatal("lines and totals that agree")
	}
	// a second line adds to the totals the same way
	two := line
	two.LineNo = 2
	doubled := CostTotals{Pooled: cny(200), Apportioned: cny(80), Retained: cny(10), Losses: cny(20), Exceptions: cny(30), ExceptionsPending: cny(10), ToAllocated: cny(10),
		FormerSharesPending: cny(20), Unapportioned: cny(30)}
	if !linesAgree([]CostLineBorne{line, two}, doubled) {
		t.Fatal("two lines and totals that agree")
	}
	// a line whose parts do not add up to its cost, the totals following the cost
	bad := line
	bad.Local = cny(101)
	badTotals := totals
	badTotals.Pooled = cny(101)
	if linesAgree([]CostLineBorne{bad}, badTotals) {
		t.Fatal("a line whose parts do not add up to its cost")
	}
	for name, change := range map[string]func(*CostTotals){
		"the pooled cost":              func(c *CostTotals) { c.Pooled.Minor++ },
		"the apportioned part":         func(c *CostTotals) { c.Apportioned.Minor++ },
		"the retained part":            func(c *CostTotals) { c.Retained.Minor++ },
		"the losses":                   func(c *CostTotals) { c.Losses.Minor++ },
		"the exception costs":          func(c *CostTotals) { c.Exceptions.Minor++ },
		"the pending exception costs":  func(c *CostTotals) { c.ExceptionsPending.Minor++ },
		"the amount to be allocated":   func(c *CostTotals) { c.ToAllocated.Minor++ },
		"the former shares pending":    func(c *CostTotals) { c.FormerSharesPending.Minor++ },
		"what no apportionment covers": func(c *CostTotals) { c.Unapportioned.Minor++ },
	} {
		c := totals
		change(&c)
		if linesAgree([]CostLineBorne{line}, c) {
			t.Fatalf("lines that do not add up to FIN's total of %s", name)
		}
	}
}

// A recovery's local amount is the attribution's own, not the line-currency amount: a USD line with a loss of USD 10.00
// (CNY 71.23) and an exception cost of the carrier of USD 20.00 (CNY 142.47).
func TestARecoveryOfAForeignCurrencyLineCarriesItsLocalAmount(t *testing.T) {
	phy, _, _, _ := standIns()
	usd := func(minor int64) kernel.Money { return kernel.Money{Currency: "USD", Minor: minor} }
	attr := attribution("SH-H03", 0, 0, finapi.PartyCarrier, "line-1")
	attr.Loss, attr.LossLocal = usd(1_000), cny(7_123)
	attr.Exception, attr.ExceptionLocal = usd(2_000), cny(14_247)
	line := finapi.PoolCostLine{LineNo: 1, ChargeCode: "THC", Method: finapi.MethodChargeable, Cost: usd(10_000), CostLocal: cny(71_234)}
	pool := poolOf([]finapi.PoolCostLine{line}, finapi.ApportionedLine{LineNo: 1, Method: finapi.MethodChargeable, Amount: usd(10_000), Local: cny(71_234),
		Shares: []finapi.PoolShare{{ShipmentID: "SH-H01", Amount: usd(7_000), Local: cny(49_864)}}, Attributions: []finapi.PoolAttribution{attr}})
	r := reportOf(t, phy, nil, nil, &fakeFIN{pool: pool, profit: finapi.ConsolProfit{PoolID: "P-1"}})
	if len(r.Recoveries) != 2 || r.Recoveries[0].Kind != RecoverFromParty || r.Recoveries[0].Amount != usd(2_000) || r.Recoveries[0].Local != cny(14_247) ||
		r.Recoveries[1].Kind != RecoverChargeToCustomer || r.Recoveries[1].Amount != usd(1_000) || r.Recoveries[1].Local != cny(7_123) {
		t.Fatalf("the recoveries of a USD line: %+v", r.Recoveries)
	}
}
