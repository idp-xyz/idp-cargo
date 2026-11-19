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
