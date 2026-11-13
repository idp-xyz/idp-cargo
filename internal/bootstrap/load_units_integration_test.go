//go:build integration

package bootstrap

import (
	"context"
	"errors"
	"testing"
	"time"

	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// PHY plans containers against the real equipment reference data (PB8) on an own-container LCL consolidation whose
// route the real location reference data resolved.
func TestLoadUnitsAgainstTheReferenceData(t *testing.T) {
	ctx := context.Background()
	platform := openPlatform(t)
	orgModule, err := platform.ORG(allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	rateModule, err := platform.RATE(orgModule, allowAll{}, marginFloor(2000))
	if err != nil {
		t.Fatal(err)
	}
	comModule, err := platform.COM(orgModule, rateModule, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	_, phyModule, err := platform.FULAndPHY(comModule, allowAll{}, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	tenant := uniqueTenant("cargo-units-")
	at := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Microsecond)
	unlocode := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	if _, err = phyModule.CreateConsolidation(ctx, phyapi.CreateConsolidation{CommandID: "create", TenantID: tenant, ActorID: "ops", ConsolidationID: "C-1",
		Mode: phyapi.ModeSea, Kind: phyapi.KindSeaLCLOwn, OperatingLegalEntityID: "LE", Origin: unlocode("CNSHA"), Destination: unlocode("DEHAM"),
		PlannedDeparture: at.Add(96 * time.Hour), HouseCutOff: at.Add(72 * time.Hour), OccurredAt: at}); err != nil {
		t.Fatal(err)
	}
	plan := func(command string, expected int64, code string) (phyapi.Receipt, error) {
		return phyModule.PlanLoadUnit(ctx, phyapi.PlanLoadUnit{CommandID: command, TenantID: tenant, ActorID: "ops", ConsolidationID: "C-1",
			ExpectedRevision: expected, Kind: phyapi.LoadUnitContainer, TypeCode: code, OccurredAt: at.Add(time.Hour)})
	}
	if r, err := plan("plan-45", 1, "45G1"); err != nil || r.ItemNo != 1 || r.NewRevision != 2 {
		t.Fatalf("45G1: %+v, %v", r, err)
	}
	if r, err := plan("plan-22", 2, "22G1"); err != nil || r.ItemNo != 2 {
		t.Fatalf("22G1: %+v, %v", r, err)
	}
	for _, code := range []string{"22R1", "40HC"} {
		if _, err := plan("plan-"+code, 3, code); !errors.Is(err, phyapi.ErrRejected) {
			t.Fatalf("%s: %v", code, err)
		}
	}
	c, err := phyModule.GetConsolidation(ctx, phyapi.GetConsolidation{TenantID: tenant, ConsolidationID: "C-1"})
	if err != nil || len(c.LoadUnits) != 2 || c.Current.Origin.Code != "CNSHG" {
		t.Fatalf("consolidation: %+v, %v", c, err)
	}
	if u := c.LoadUnits[0]; u.TypeCode != "45G1" || u.TEUTenths != 20 || u.MaxGrossKg != 30480 || u.TareKg != 3900 || u.CapacityCM3 != 76300000 ||
		u.EquipmentEdition != "EQUIPMENT-2026-11-S1" || u.Status != phyapi.UnitStatusPlanned {
		t.Fatalf("45G1 unit: %+v", u)
	}
	if u := c.LoadUnits[1]; u.TypeCode != "22G1" || u.TEUTenths != 10 || u.TareKg != 2250 {
		t.Fatalf("22G1 unit: %+v", u)
	}
}
