package phy

import (
	"context"
	"reflect"
	"testing"
	"time"

	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

type basisStub struct{ basis phyapi.PhysicalCostBasis }

func (s basisStub) GetPhysicalCostBasis(context.Context, phyapi.GetPhysicalCostBasis) (phyapi.PhysicalCostBasis, error) {
	return s.basis, nil
}

func kg(n int64) kernel.Quantity  { return kernel.Quantity{Unit: "KGM", Amount: n} }
func cmq(n int64) kernel.Quantity { return kernel.Quantity{Unit: "CMQ", Amount: n} }

func TestTheAdapterCarriesTheLoadUnitsAndEachHousesUnitLoads(t *testing.T) {
	at := time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)
	basis := phyapi.PhysicalCostBasis{TenantID: "t", ConsolidationID: "C-1", Revision: 5, Mode: "SEA", Kind: "SEA_LCL_OWN", OperatingLegalEntityID: "LE", State: "CLOSED",
		Houses: []phyapi.HouseCostBasis{
			{ShipmentID: "SH-A", Packs: 6, GrossWeight: kg(312), Volume: cmq(800_000), LoadedPacks: 6, LoadedGrossWeight: kg(312), LoadedVolume: cmq(800_000), LastLoadedAt: at,
				Units: []phyapi.HouseUnitLoad{{UnitNo: 1, ExecutionID: "X-1", LoadedPacks: 4, LoadedGrossWeight: kg(208), LoadedVolume: cmq(533_333)},
					{UnitNo: 3, ExecutionID: "X-3", LoadedPacks: 2, LoadedGrossWeight: kg(104), LoadedVolume: cmq(266_666)}}},
			{ShipmentID: "SH-B", Packs: 6, GrossWeight: kg(288)},
		},
		LoadUnits: []phyapi.PlannedLoadUnit{
			{UnitNo: 1, Kind: "CONTAINER", TypeCode: "22G1", TEUTenths: 10, Status: phyapi.UnitStatusClosed, ExecutionID: "X-1", ExecutionState: phyapi.ExecutionClosed, ExecutionRevision: 7},
			{UnitNo: 2, TypeCode: "22G1", TEUTenths: 10, Status: phyapi.UnitStatusRemoved, RemoveReason: "rolled"},
			{UnitNo: 3, TypeCode: "42G1", TEUTenths: 20, Status: phyapi.UnitStatusPlanned},
		},
		PackingLists: []phyapi.UnitPackingList{{UnitNo: 1, ExecutionID: "X-1", ExecutionState: phyapi.ExecutionClosed, Digest: "abc123",
			Lines: []phyapi.UnitPackingLine{{ShipmentID: "SH-A", LoadedPacks: 4, LoadedGrossWeight: kg(208), LoadedVolume: cmq(533_333)}}}}}
	got, err := Physical{PHY: basisStub{basis}}.CostBasis(context.Background(), "t", "C-1")
	if err != nil {
		t.Fatal(err)
	}
	want := fin.CostBasis{ConsolidationID: "C-1", Revision: 5, Kind: "SEA_LCL_OWN", OperatingLegalEntityID: "LE", State: "CLOSED",
		Houses: []fin.HouseFacts{
			{ShipmentID: "SH-A", Packs: 6, GrossWeight: kg(312), Volume: cmq(800_000), LoadedPacks: 6, LoadedGrossWeight: kg(312), LoadedVolume: cmq(800_000), LastLoadedAt: at,
				Units: []fin.HouseUnitFacts{{UnitNo: 1, LoadedPacks: 4, LoadedGrossWeight: kg(208), LoadedVolume: cmq(533_333)},
					{UnitNo: 3, LoadedPacks: 2, LoadedGrossWeight: kg(104), LoadedVolume: cmq(266_666)}}},
			{ShipmentID: "SH-B", Packs: 6, GrossWeight: kg(288)},
		},
		LoadUnits: []fin.LoadUnitFacts{
			{UnitNo: 1, TypeCode: "22G1", TEUTenths: 10, Status: "CLOSED", ExecutionID: "X-1", ExecutionState: "CLOSED", ExecutionRevision: 7},
			{UnitNo: 3, TypeCode: "42G1", TEUTenths: 20, Status: "PLANNED"},
		},
		PackingLists: []fin.UnitPackingListFacts{{UnitNo: 1, ExecutionID: "X-1", ExecutionState: "CLOSED", Digest: "abc123"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cost basis =\n%+v\nwant\n%+v", got, want)
	}
}

func TestABasisWithoutUnitsOrEntriesMapsToNone(t *testing.T) {
	basis := phyapi.PhysicalCostBasis{ConsolidationID: "C-2", Revision: 2, Kind: "AIR_CONSOL", OperatingLegalEntityID: "LE", State: "OPEN",
		Houses: []phyapi.HouseCostBasis{{ShipmentID: "SH-1", Packs: 3, GrossWeight: kg(90), LoadedPacks: 3, LoadedGrossWeight: kg(90)}}}
	got, err := Physical{PHY: basisStub{basis}}.CostBasis(context.Background(), "t", "C-2")
	if err != nil {
		t.Fatal(err)
	}
	want := fin.CostBasis{ConsolidationID: "C-2", Revision: 2, Kind: "AIR_CONSOL", OperatingLegalEntityID: "LE", State: "OPEN",
		Houses: []fin.HouseFacts{{ShipmentID: "SH-1", Packs: 3, GrossWeight: kg(90), LoadedPacks: 3, LoadedGrossWeight: kg(90)}}}
	if !reflect.DeepEqual(got, want) || len(got.LoadUnits) != 0 || len(got.PackingLists) != 0 || len(got.Houses[0].Units) != 0 {
		t.Fatalf("cost basis = %+v, want %+v", got, want)
	}
}

// A unit PHY calls CLOSED (its stuffing is sealed) arrives as the status FIN's container methods look for.
func TestAClosedUnitArrivesAsTheDomainConstant(t *testing.T) {
	basis := phyapi.PhysicalCostBasis{ConsolidationID: "C-1", Kind: "SEA_LCL_OWN",
		LoadUnits: []phyapi.PlannedLoadUnit{{UnitNo: 1, TypeCode: "22G1", TEUTenths: 10, Status: phyapi.UnitStatusClosed}}}
	got, err := Physical{PHY: basisStub{basis}}.CostBasis(context.Background(), "t", "C-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.LoadUnits) != 1 || got.LoadUnits[0].Status != fin.LoadUnitClosed {
		t.Fatalf("load units: %+v", got.LoadUnits)
	}
}

// The reworks of a unit's stuffing arrive with their number, who asked, how each ended and when; a stuffing in REWORK
// arrives as the state the domain reads to freeze the pool, and the units in rework are told in order.
func TestTheAdapterCarriesTheReworksOfEachPackingList(t *testing.T) {
	ended := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	basis := phyapi.PhysicalCostBasis{ConsolidationID: "C-3", Revision: 9, Kind: "SEA_LCL_OWN", OperatingLegalEntityID: "LE", State: "CLOSED",
		LoadUnits: []phyapi.PlannedLoadUnit{
			{UnitNo: 1, TypeCode: "22G1", TEUTenths: 10, Status: phyapi.UnitStatusClosed, ExecutionID: "X-1", ExecutionState: phyapi.ExecutionClosed},
			{UnitNo: 2, TypeCode: "22G1", TEUTenths: 10, Status: phyapi.UnitStatusInUse, ExecutionID: "X-2", ExecutionState: phyapi.ExecutionRework}},
		PackingLists: []phyapi.UnitPackingList{
			{UnitNo: 1, ExecutionID: "X-1", ExecutionState: phyapi.ExecutionClosed, Digest: "d1", Reworks: []phyapi.Rework{
				{ReworkNo: 1, AskedBy: "ops-a", Reason: "wrong seal", End: phyapi.ReworkSealed, EndedBy: "cfs-1", EndedAt: ended, NewSealReference: "S-2"},
				{ReworkNo: 2, AskedBy: "ops-b", End: phyapi.ReworkSealed, EndedAt: ended.Add(time.Hour)}}},
			{UnitNo: 2, ExecutionID: "X-2", ExecutionState: phyapi.ExecutionRework, Digest: "d2", Reworks: []phyapi.Rework{{ReworkNo: 1, AskedBy: "ops-c"}}}}}
	got, err := Physical{PHY: basisStub{basis}}.CostBasis(context.Background(), "t", "C-3")
	if err != nil {
		t.Fatal(err)
	}
	want := []fin.UnitPackingListFacts{
		{UnitNo: 1, ExecutionID: "X-1", ExecutionState: "CLOSED", Digest: "d1", Reworks: []fin.ReworkFacts{
			{No: 1, AskedBy: "ops-a", End: "SEALED", EndedAt: ended}, {No: 2, AskedBy: "ops-b", End: "SEALED", EndedAt: ended.Add(time.Hour)}}},
		{UnitNo: 2, ExecutionID: "X-2", ExecutionState: "REWORK", Digest: "d2", Reworks: []fin.ReworkFacts{{No: 1, AskedBy: "ops-c"}}}}
	if !reflect.DeepEqual(got.PackingLists, want) {
		t.Fatalf("packing lists =\n%+v\nwant\n%+v", got.PackingLists, want)
	}
	if units := got.UnitsInRework(); !reflect.DeepEqual(units, []int{2}) {
		t.Fatalf("units in rework: %v", units)
	}
}

// The houses that left and the stuffings given up arrive as the facts the fingerprint reads, in PHY's order, with the
// reworks as a packing list's are; a basis that tells of none maps to none.
func TestTheAdapterCarriesTheFormerHousesAndTheGivenUpStuffings(t *testing.T) {
	ended := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	entered := ended.Add(time.Minute)
	basis := phyapi.PhysicalCostBasis{ConsolidationID: "C-4", Revision: 3, Kind: "SEA_LCL_OWN", OperatingLegalEntityID: "LE", State: "CLOSED",
		FormerHouses: []phyapi.FormerHouse{
			{ShipmentID: "SH-1", End: phyapi.FormerTransferred, TransferredTo: "C-5", Reason: "WRONG_CONSOL", ReasonText: "a text", EndedBy: "ops-1", EndedAt: ended,
				EndedEnteredAt: entered, ReworkFrom: "REVERSAL", ReworkExecutionID: "X-1", ReworkNo: 2, ReworkUnitNo: 1, ReworkAskedBy: "ask-2"},
			{ShipmentID: "SH-2", End: phyapi.FormerDetached, Reason: "SHUT_OUT", EndedBy: "ops-2", EndedAt: ended, EndedEnteredAt: entered}},
		GivenUpStuffings: []phyapi.GivenUpStuffing{{UnitNo: 1, ExecutionID: "X-1", Reworks: []phyapi.Rework{
			{ReworkNo: 1, AskedBy: "ask-1", End: phyapi.ReworkSealed, EndedBy: "cfs-1", EndedAt: ended, NewSealReference: "S-2"},
			{ReworkNo: 2, AskedBy: "ask-2", End: phyapi.ReworkGivenUp, EndedBy: "ops-1", EndedAt: ended.Add(time.Hour)}}}}}
	got, err := Physical{PHY: basisStub{basis}}.CostBasis(context.Background(), "t", "C-4")
	if err != nil {
		t.Fatal(err)
	}
	wantFormer := []fin.FormerHouseFacts{
		{ShipmentID: "SH-1", End: "TRANSFERRED", TransferredTo: "C-5", Reason: "WRONG_CONSOL", EndedBy: "ops-1", EndedAt: ended, EndedEnteredAt: entered, ReworkFrom: "REVERSAL",
			ReworkExecutionID: "X-1", ReworkNo: 2, ReworkUnitNo: 1, ReworkAskedBy: "ask-2"},
		{ShipmentID: "SH-2", End: "DETACHED", Reason: "SHUT_OUT", EndedBy: "ops-2", EndedAt: ended, EndedEnteredAt: entered}}
	wantGiven := []fin.GivenUpStuffingFacts{{UnitNo: 1, ExecutionID: "X-1", Reworks: []fin.ReworkFacts{
		{No: 1, AskedBy: "ask-1", End: "SEALED", EndedAt: ended}, {No: 2, AskedBy: "ask-2", End: "GIVEN_UP", EndedAt: ended.Add(time.Hour)}}}}
	if !reflect.DeepEqual(got.FormerHouses, wantFormer) || !reflect.DeepEqual(got.GivenUpStuffings, wantGiven) || len(got.PackingLists) != 0 {
		t.Fatalf("former houses\n%+v\nwant\n%+v\ngiven up\n%+v\nwant\n%+v", got.FormerHouses, wantFormer, got.GivenUpStuffings, wantGiven)
	}
	none, err := Physical{PHY: basisStub{phyapi.PhysicalCostBasis{ConsolidationID: "C-6", Kind: "AIR_CONSOL"}}}.CostBasis(context.Background(), "t", "C-6")
	if err != nil || len(none.FormerHouses) != 0 || len(none.GivenUpStuffings) != 0 {
		t.Fatalf("a basis with none: %+v, %v", none, err)
	}
}
