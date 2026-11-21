//go:build integration

package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	rateapi "go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// ownHouse is one house of an own-container consolidation as PHY reports it: planned packs, weight (kg) and volume
// (cm³), and what is loaded of them (a zero loaded pack count is a house not loaded yet).
func ownHouse(f *fixture, shipment string, packs, kilos, cubicCm, loadedPacks, loadedKilos, loadedCubicCm int64) phyapi.HouseCostBasis {
	h := phyapi.HouseCostBasis{ShipmentID: shipment, MembershipRevision: 2, Packs: packs, GrossWeight: kg(kilos), Volume: cc(cubicCm)}
	if loadedPacks > 0 {
		h.LoadedPacks, h.LoadedGrossWeight, h.LoadedVolume, h.LastLoadedAt = loadedPacks, kg(loadedKilos), cc(loadedCubicCm), f.hours(1)
	}
	return h
}

// ownContainerConsol is PhysicalCostBasis v1 of an LCL consolidation in the forwarder's own container, operated by LE,
// with three houses. Planned W/M in milli freight tonnes: SH-1 1200 kg over 0.9 m³ is 1200, SH-2 200 kg under 2.0 m³ is
// 2000, SH-3 300 kg over 0.3 m³ is 300. Loaded: SH-1 1250 kg, 0.8 m³ is 1250; SH-2 200 kg, 2.4 m³ is 2400; SH-3 351 kg,
// 0.3 m³ is 351. Weight wins for SH-1 and SH-3, volume for SH-2.
func ownContainerConsol(f *fixture, id string, loaded bool) phyapi.PhysicalCostBasis {
	houses := []phyapi.HouseCostBasis{
		ownHouse(f, "SH-1", 6, 1200, 900_000, 6, 1250, 800_000),
		ownHouse(f, "SH-2", 4, 200, 2_000_000, 4, 200, 2_400_000),
		ownHouse(f, "SH-3", 2, 300, 300_000, 2, 351, 300_000),
	}
	if !loaded {
		for i := range houses {
			houses[i].LoadedPacks, houses[i].LoadedGrossWeight, houses[i].LoadedVolume = 0, kernel.Quantity{}, kernel.Quantity{}
		}
	}
	return phyapi.PhysicalCostBasis{TenantID: f.tenant, ConsolidationID: id, Revision: 5, Mode: "SEA", Kind: "SEA_LCL_OWN", OperatingLegalEntityID: "LE",
		State: "OPEN", Houses: houses}
}

// An own-container LCL consolidation has a cost pool on PostgreSQL (V2.77): it opens in mode SEA_LCL from PHY's kind
// SEA_LCL_OWN; a line by CHARGEABLE (W/M) is PLANNED while the houses are not loaded and re-apportioned ACTUAL once
// they are; VOLUME, PIECES and MANUAL lines; houses without volume; the jobs and the consolidation's profit; the
// consolidations a pool is refused for.
func TestOwnContainerPoolOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.w.costBases["CONSOL-OWN"] = ownContainerConsol(f, "CONSOL-OWN", false)
	cancelled := ownContainerConsol(f, "CONSOL-CANCELLED", true)
	cancelled.State = "CANCELLED"
	f.w.costBases["CONSOL-CANCELLED"] = cancelled
	otherEntity := ownContainerConsol(f, "CONSOL-LE2", true)
	otherEntity.OperatingLegalEntityID = "LE-2"
	f.w.costBases["CONSOL-LE2"] = otherEntity

	must := func(r api.Receipt, err error) api.Receipt {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	must(f.module.OpenFinancePolicy(ctx, api.OpenFinancePolicy{CommandID: "policy", TenantID: f.tenant, ActorID: "acct-1", LegalEntityID: "LE",
		FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: f.hours(0)}))
	for _, s := range []string{"SH-1", "SH-2", "SH-3"} {
		f.w.drivers[s] = fulapi.CostDriver{TenantID: f.tenant, ShipmentID: s, Kind: fulapi.DriverShipmentPlanned, Revision: 3, Status: fulapi.StatusPlanned,
			PlanVersion: 2, TransportMode: "SEA", LegalEntityID: "LE", CustomerPartyID: "party-a", Packs: 6,
			Orders: []fulapi.CostOrder{{OrderID: "O-" + s, QuoteID: "Q-1", QuoteRevision: 2, Packs: 6}}}
	}
	revenue := map[string]int64{"SH-1": 800_000, "SH-2": 500_000, "SH-3": 200_000}
	for i, s := range []string{"SH-1", "SH-2", "SH-3"} {
		job := "JOB-" + s[3:]
		must(f.module.OpenFreightJob(ctx, api.OpenFreightJob{CommandID: "open-" + job, TenantID: f.tenant, ActorID: "fin-1", JobID: job, ShipmentID: s,
			OccurredAt: f.hours(1)}))
		must(f.module.RecordChargeLine(ctx, api.RecordChargeLine{CommandID: "sell-" + job, TenantID: f.tenant, ActorID: "fin-1", JobID: job,
			ExpectedRevision: 1, ChargeCode: "OCEAN_FREIGHT", Sell: &api.ChargeSide{PartyID: "party-a", Amount: cny(revenue[s])}, Reason: "sold as per booking",
			OccurredAt: f.hours(1 + i)}))
	}

	// the pool opens in mode SEA_LCL; a consolidation PHY reports cancelled, or one operated by another legal entity, is refused
	open := api.OpenConsolCostPool{CommandID: "pool", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-OWN", ConsolidationID: "CONSOL-OWN", OccurredAt: f.hours(4)}
	gone := open
	gone.CommandID, gone.PoolID, gone.ConsolidationID = "pool-cancelled", "P-CANCELLED", "CONSOL-CANCELLED"
	if _, err := f.module.OpenConsolCostPool(ctx, gone); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "FIN_NOT_OPENABLE") {
		t.Fatalf("the pool of a cancelled consolidation: %v", err)
	}
	foreign := open
	foreign.CommandID, foreign.PoolID, foreign.ConsolidationID = "pool-le2", "P-LE2", "CONSOL-LE2"
	if _, err := f.module.OpenConsolCostPool(ctx, foreign); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "not VERIFIED") {
		t.Fatalf("the pool of another legal entity's consolidation: %v", err)
	}
	if r := must(f.module.OpenConsolCostPool(ctx, open)); r.SubjectID != "P-OWN" || r.NewRevision != 1 {
		t.Fatalf("open pool: %+v", r)
	}
	if pool, err := f.module.GetConsolCostPool(ctx, api.GetConsolCostPool{TenantID: f.tenant, PoolID: "P-OWN"}); err != nil || pool.Mode != "SEA_LCL" ||
		pool.ConsolidationID != "CONSOL-OWN" || pool.State != "OPEN" {
		t.Fatalf("pool: %+v %v", pool, err)
	}

	// line 1 by CHARGEABLE while no house is loaded: PLANNED, W/M of the planned scope, 700000 / 3500 = 200 per milli tonne
	rev := int64(1)
	record := func(id, code, method string, amount int64, hour int) {
		t.Helper()
		r := must(f.module.RecordConsolCost(ctx, api.RecordConsolCost{CommandID: id, TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-OWN", ExpectedRevision: rev,
			ChargeCode: code, Method: method, Cost: api.ChargeSide{PartyID: "airline-mu", Amount: cny(amount)}, Reason: "entered for the container", OccurredAt: f.hours(hour)}))
		rev = r.NewRevision
	}
	apportion := func(in api.PreviewConsolCostApportionment, hour int) {
		t.Helper()
		in.TenantID, in.ActorID, in.PoolID, in.ExpectedRevision, in.OccurredAt = f.tenant, "fin-1", "P-OWN", rev, f.hours(hour)
		rev = must(previewAndSubmit(ctx, f.module, in)).NewRevision
	}
	pool := func() api.ConsolCostPool {
		t.Helper()
		p, err := f.module.GetConsolCostPool(ctx, api.GetConsolCostPool{TenantID: f.tenant, PoolID: "P-OWN"})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	// shares checks the latest apportionment of the pool: its stage, the line, the method, and each house's quantity, unit and amount
	shares := func(stage string, line int, method, unit string, quantities, amounts []int64) {
		t.Helper()
		p := pool()
		a := p.Apportionments[len(p.Apportionments)-1]
		if a.Stage != stage {
			t.Fatalf("line %d: stage %s, want %s", line, a.Stage, stage)
		}
		for _, l := range a.Lines {
			if l.LineNo != line {
				continue
			}
			var sum int64
			if l.Method != method || len(l.Shares) != 3 {
				t.Fatalf("line %d: %+v", line, l)
			}
			for i, s := range l.Shares {
				if s.ShipmentID != "SH-"+string(rune('1'+i)) || s.JobID != "JOB-"+string(rune('1'+i)) || s.Quantity != quantities[i] || s.Unit != unit ||
					s.Amount != cny(amounts[i]) || s.Local != cny(amounts[i]) {
					t.Fatalf("line %d share %d: %+v, want %d %s %d", line, i+1, s, quantities[i], unit, amounts[i])
				}
				sum += s.Amount.Minor
			}
			if sum != l.Amount.Minor {
				t.Fatalf("line %d: shares sum to %d of %d", line, sum, l.Amount.Minor)
			}
			return
		}
		t.Fatalf("line %d is not in the latest apportionment: %+v", line, a)
	}
	record("cost-freight", "OCEAN_FREIGHT", api.MethodChargeable, 700_000, 5)
	apportion(api.PreviewConsolCostApportionment{CommandID: "apportion-planned"}, 6)
	shares(api.BasisPlanned, 1, api.MethodChargeable, "MWM", []int64{1200, 2000, 300}, []int64{240_000, 400_000, 60_000})

	// every house loaded: the line is re-apportioned at ACTUAL on the loaded measures, W/M 1250 : 2400 : 351 of 4001,
	// 700000 gives 218695.33 / 419895.03 / 61409.65: the floors sum to 699999 and the minor unit left goes to the largest
	// remainder (SH-3, 2591 of 4001)
	f.w.costBases["CONSOL-OWN"] = ownContainerConsol(f, "CONSOL-OWN", true)
	apportion(api.PreviewConsolCostApportionment{CommandID: "apportion-actual"}, 7)
	shares(api.BasisActual, 1, api.MethodChargeable, "MWM", []int64{1250, 2400, 351}, []int64{218_695, 419_895, 61_410})

	// a line by VOLUME (cm³ 800000 : 2400000 : 300000; 90000 gives 20571.43 / 61714.29 / 7714.29, the unit left to SH-1),
	// one by PIECES (6 : 4 : 2 of 30000) and one by hand with a reason
	record("cost-cfs", "CFS_HANDLING", api.MethodVolume, 90_000, 8)
	record("cost-drayage", "DRAYAGE", api.MethodPieces, 30_000, 8)
	record("cost-seal", "SEAL_AND_LASHING", api.MethodManual, 12_000, 8)
	apportion(api.PreviewConsolCostApportionment{CommandID: "apportion-cfs", LineNos: []int{2}}, 9)
	shares(api.BasisActual, 2, api.MethodVolume, "CMQ", []int64{800_000, 2_400_000, 300_000}, []int64{20_572, 61_714, 7_714})
	apportion(api.PreviewConsolCostApportionment{CommandID: "apportion-drayage", LineNos: []int{3}}, 9)
	shares(api.BasisActual, 3, api.MethodPieces, "PCS", []int64{6, 4, 2}, []int64{15_000, 10_000, 5_000})
	apportion(api.PreviewConsolCostApportionment{CommandID: "apportion-seal", LineNos: []int{4}, Method: api.MethodManual, Reason: "SH-2 was lashed twice",
		Manual: []api.ManualShare{{ShipmentID: "SH-1", Amount: 5_000}, {ShipmentID: "SH-2", Amount: 4_000}, {ShipmentID: "SH-3", Amount: 3_000}}}, 9)
	shares(api.BasisActual, 4, api.MethodManual, "", []int64{5_000, 4_000, 3_000}, []int64{5_000, 4_000, 3_000})
	if l := pool().Apportionments[len(pool().Apportionments)-1].Lines[0]; l.Reason != "SH-2 was lashed twice" {
		t.Fatalf("manual reason: %+v", l)
	}

	// each house's job carries its apportioned cost: 218695+20572+15000+5000, 419895+61714+10000+4000, 61410+7714+5000+3000
	for _, want := range []struct {
		job                   string
		pool, profit, freight int64
	}{{"JOB-1", 259_267, 540_733, 218_695}, {"JOB-2", 495_609, 4_391, 419_895}, {"JOB-3", 77_124, 122_876, 61_410}} {
		p, err := f.module.GetJobProfit(ctx, api.GetJobProfit{TenantID: f.tenant, JobID: want.job})
		if err != nil || p.PoolCost != cny(want.pool) || p.Profit != cny(want.profit) || p.Cost != cny(0) || len(p.PoolShares) != 4 ||
			p.PoolShares[0].PoolID != "P-OWN" || p.PoolShares[0].Stage != api.BasisActual || p.PoolShares[0].Local != cny(want.freight) {
			t.Fatalf("%s profit: %+v %v", want.job, p, err)
		}
	}
	consol, err := f.module.GetConsolProfit(ctx, api.GetConsolProfit{TenantID: f.tenant, ConsolidationID: "CONSOL-OWN"})
	if err != nil || consol.PoolID != "P-OWN" || consol.Pooled != cny(832_000) || consol.Apportioned != cny(832_000) || consol.Unapportioned != cny(0) ||
		consol.Retained != cny(0) || len(consol.Houses) != 3 {
		t.Fatalf("consol profit: %+v %v", consol, err)
	}
	var revenueSum, poolSum int64
	for i, h := range consol.Houses {
		wantPool := []int64{259_267, 495_609, 77_124}[i]
		if h.JobID != "JOB-"+string(rune('1'+i)) || h.Revenue != cny(revenue[h.ShipmentID]) || h.PoolCost != cny(wantPool) ||
			h.Profit != cny(revenue[h.ShipmentID]-wantPool) {
			t.Fatalf("house %d: %+v", i+1, h)
		}
		revenueSum, poolSum = revenueSum+h.Revenue.Minor, poolSum+h.PoolCost.Minor
	}
	if revenueSum != 1_500_000 || poolSum != 832_000 {
		t.Fatalf("houses sum to revenue %d and pool cost %d", revenueSum, poolSum)
	}

	// a house without volume: SH-3 is loaded 500 kg and no volume. CHARGEABLE falls back to its weight (W/M 500); VOLUME
	// is not refused while another house has volume, it gives that house nothing (quantity 0, share 0); a consolidation
	// whose houses all have no volume refuses VOLUME (FIN_POOL_NO_BASIS) and still divides CHARGEABLE by weight.
	flat := ownContainerConsol(f, "CONSOL-NOVOL", true)
	flat.Houses[2].LoadedGrossWeight, flat.Houses[2].LoadedVolume = kg(500), kernel.Quantity{}
	f.w.costBases["CONSOL-NOVOL"] = flat
	none := ownContainerConsol(f, "CONSOL-ZERO", true)
	for i := range none.Houses {
		none.Houses[i].LoadedVolume = kernel.Quantity{}
	}
	f.w.costBases["CONSOL-ZERO"] = none
	for i, c := range []string{"CONSOL-NOVOL", "CONSOL-ZERO"} {
		id := "P-" + c
		must(f.module.OpenConsolCostPool(ctx, api.OpenConsolCostPool{CommandID: "pool-" + c, TenantID: f.tenant, ActorID: "fin-1", PoolID: id, ConsolidationID: c,
			OccurredAt: f.hours(10)}))
		must(f.module.RecordConsolCost(ctx, api.RecordConsolCost{CommandID: "cost-" + c, TenantID: f.tenant, ActorID: "fin-1", PoolID: id, ExpectedRevision: 1,
			ChargeCode: "OCEAN_FREIGHT", Method: api.MethodChargeable, Cost: api.ChargeSide{Amount: cny(100_000)}, Reason: "entered for the container",
			OccurredAt: f.hours(11)}))
		volume := api.PreviewConsolCostApportionment{CommandID: "volume-" + c, TenantID: f.tenant, ActorID: "fin-1", PoolID: id, ExpectedRevision: 2, Method: api.MethodVolume,
			OccurredAt: f.hours(12)}
		if i == 1 {
			if _, err := previewAndSubmit(ctx, f.module, volume); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "FIN_POOL_NO_BASIS") {
				t.Fatalf("VOLUME when no house has volume: %v", err)
			}
			continue
		}
		must(previewAndSubmit(ctx, f.module, volume))
		got, err := f.module.GetConsolCostPool(ctx, api.GetConsolCostPool{TenantID: f.tenant, PoolID: id})
		if err != nil || len(got.Apportionments) != 1 {
			t.Fatalf("%s: %+v %v", id, got, err)
		}
		// 800000 : 2400000 : 0 of 100000 is 25000 / 75000 / 0
		s := got.Apportionments[0].Lines[0].Shares
		if s[0].Amount != cny(25_000) || s[1].Amount != cny(75_000) || s[2].Quantity != 0 || s[2].Amount != cny(0) {
			t.Fatalf("VOLUME with one house without volume: %+v", s)
		}
		volume.CommandID, volume.ExpectedRevision, volume.Method = "chargeable-"+c, 4, api.MethodChargeable
		volume.OccurredAt = f.hours(13)
		must(previewAndSubmit(ctx, f.module, volume))
		if got, err = f.module.GetConsolCostPool(ctx, api.GetConsolCostPool{TenantID: f.tenant, PoolID: id}); err != nil {
			t.Fatal(err)
		}
		// W/M 1250 : 2400 : 500 of 4150: 30120.48 / 57831.33 / 12048.19, the unit left to SH-1 (largest remainder, 2000 of 4150)
		s = got.Apportionments[1].Lines[0].Shares
		if s[0].Quantity != 1250 || s[1].Quantity != 2400 || s[2].Quantity != 500 || s[2].Unit != "MWM" || s[0].Amount != cny(30_121) ||
			s[1].Amount != cny(57_831) || s[2].Amount != cny(12_048) {
			t.Fatalf("CHARGEABLE with a house without volume: %+v", s)
		}
	}
	zero := api.PreviewConsolCostApportionment{CommandID: "chargeable-CONSOL-ZERO", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-CONSOL-ZERO", ExpectedRevision: 2,
		OccurredAt: f.hours(13)}
	must(previewAndSubmit(ctx, f.module, zero))
	got, err := f.module.GetConsolCostPool(ctx, api.GetConsolCostPool{TenantID: f.tenant, PoolID: "P-CONSOL-ZERO"})
	// W/M by weight alone: 1250 : 200 : 351 of 1801; 100000 gives 69405.89 / 11104.94 / 19489.17 (remainders 1595, 1696,
	// 311 of 1801): the two units left go to SH-2 and SH-1, so 69406 / 11105 / 19489
	if s := got.Apportionments[0].Lines[0].Shares; err != nil || s[0].Quantity != 1250 || s[1].Quantity != 200 || s[2].Quantity != 351 ||
		s[0].Amount != cny(69_406) || s[1].Amount != cny(11_105) || s[2].Amount != cny(19_489) {
		t.Fatalf("CHARGEABLE with no volume at all: %+v %v", s, err)
	}
	// A house loaded in part while the others are loaded in full (behaviour as it is, and as the design states the rule: the
	// stage is ACTUAL when every house has LoadedPacks > 0, whether or not the stuffing is complete). SH-3 has 3 of its 6
	// packs in the container, 150 kg and 150,000 cm3 of its 300 kg and 300,000 cm3. The apportionment is stage ACTUAL on what
	// is loaded so far: W/M 1250 : 2400 : 150 of 3800, CNY 100,000 divides 32,894.74 / 63,157.89 / 3,947.37, the floors sum
	// to 99,998 and the two units go to the largest remainders (SH-2 3400, SH-1 2800 of 3800): 32,895 / 63,158 / 3,947.
	part := ownContainerConsol(f, "CONSOL-PART", true)
	part.Houses[2] = ownHouse(f, "SH-3", 6, 300, 300_000, 3, 150, 150_000)
	f.w.costBases["CONSOL-PART"] = part
	must(f.module.OpenConsolCostPool(ctx, api.OpenConsolCostPool{CommandID: "pool-part", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-PART",
		ConsolidationID: "CONSOL-PART", OccurredAt: f.hours(14)}))
	must(f.module.RecordConsolCost(ctx, api.RecordConsolCost{CommandID: "cost-part", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-PART", ExpectedRevision: 1,
		ChargeCode: "OCEAN_FREIGHT", Method: api.MethodChargeable, Cost: api.ChargeSide{PartyID: "airline-mu", Amount: cny(100_000)}, Reason: "entered for the container",
		OccurredAt: f.hours(15)}))
	poolPart := func() api.ConsolCostPool {
		t.Helper()
		p, err := f.module.GetConsolCostPool(ctx, api.GetConsolCostPool{TenantID: f.tenant, PoolID: "P-PART"})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	apportionPart := func(id string, h int) api.PoolApportionment {
		t.Helper()
		must(previewAndSubmit(ctx, f.module, api.PreviewConsolCostApportionment{CommandID: id, TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-PART",
			ExpectedRevision: poolPart().Revision, OccurredAt: f.hours(h)}))
		p := poolPart()
		return p.Apportionments[len(p.Apportionments)-1]
	}
	checkPart := func(a api.PoolApportionment, basisRevision int64, quantities, amounts [3]int64) {
		t.Helper()
		sh := a.Lines[0].Shares
		if a.Stage != api.BasisActual || a.BasisRevision != basisRevision || len(sh) != 3 {
			t.Fatalf("apportionment: %+v", a)
		}
		for i := range sh {
			if sh[i].Quantity != quantities[i] || sh[i].Unit != "MWM" || sh[i].Amount != cny(amounts[i]) {
				t.Fatalf("share %d: %+v, want %d and %d", i+1, sh[i], quantities[i], amounts[i])
			}
		}
	}
	checkPart(apportionPart("apportion-part", 16), 5, [3]int64{1250, 2400, 150}, [3]int64{32_895, 63_158, 3_947})

	// The creditor's invoice is matched final at the line's amount. When the rest of SH-3 is loaded PHY's basis moves on
	// (revision 6, SH-3 at its full 6 packs, 300 kg and 300,000 cm3: W/M 300) and the pool does not close: its shares were
	// made on the part loaded, so it is refused (FIN_POOL_NOT_CLOSABLE) until the line is apportioned again on the current
	// basis. W/M 1250 : 2400 : 300 of 3950: CNY 100,000 divides 31,645.57 / 60,759.49 / 7,594.94, floors 99,998, and the two
	// units go to SH-3 (3700) and SH-1 (2250): 31,646 / 60,759 / 7,595. Then it closes.
	must(f.module.RecordCreditorInvoice(ctx, api.RecordCreditorInvoice{CommandID: "invoice-part", TenantID: f.tenant, ActorID: "ap-1", InvoiceID: "CI-PART",
		LegalEntityID: "LE", CreditorPartyID: "airline-mu", SupplierInvoiceNo: "MU-PART-1", InvoiceDate: f.hours(17).Format(time.DateOnly), Currency: "CNY",
		Lines: []api.CreditorInvoiceLine{{ChargeCode: "OCEAN_FREIGHT", Amount: 100_000}}, OccurredAt: f.hours(17)}))
	invoice, err := f.module.GetCreditorInvoice(ctx, api.GetCreditorInvoice{TenantID: f.tenant, InvoiceID: "CI-PART"})
	if err != nil {
		t.Fatal(err)
	}
	must(f.module.MatchCreditorInvoice(ctx, api.MatchCreditorInvoice{CommandID: "match-part", TenantID: f.tenant, ActorID: "ap-1", InvoiceID: "CI-PART",
		ExpectedRevision: invoice.Revision, Matches: []api.CreditorMatch{{LineNo: 1, Target: api.AccrualRef{Kind: "POOL_COST", ID: "P-PART", LineNo: 1}, Amount: 100_000, Final: true}},
		OccurredAt: f.hours(18)}))
	whole := ownContainerConsol(f, "CONSOL-PART", true)
	whole.Revision = 6
	whole.Houses[2] = ownHouse(f, "SH-3", 6, 300, 300_000, 6, 300, 300_000)
	f.w.costBases["CONSOL-PART"] = whole
	closePart := func(id string, h int) error {
		_, err := f.module.CloseConsolCostPool(ctx, api.CloseConsolCostPool{CommandID: id, TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-PART",
			ExpectedRevision: poolPart().Revision, OccurredAt: f.hours(h)})
		return err
	}
	if err := closePart("close-part-stale", 19); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "FIN_POOL_NOT_CLOSABLE") {
		t.Fatalf("closing on shares made on the part loaded: %v", err)
	}
	checkPart(apportionPart("apportion-part-again", 20), 6, [3]int64{1250, 2400, 300}, [3]int64{31_646, 60_759, 7_595})
	if err := closePart("close-part", 21); err != nil {
		t.Fatalf("closing on the current basis: %v", err)
	}
	if p := poolPart(); p.State != api.PoolClosed || len(p.Apportionments) != 2 || p.Apportionments[0].Lines[0].Shares[2].Quantity != 150 {
		t.Fatalf("the closed pool keeps both apportionments: %+v", p)
	}
}

// The pool of an own-container consolidation is rated like a co-load's: RATE is asked in mode SEA for the planned scope
// of the three houses (12 packs, 1700 kg, 3.2 m³) and its costs become RATED lines of the pool.
func TestOwnContainerPoolIsRatedOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.w.costBases["CONSOL-OWN"] = ownContainerConsol(f, "CONSOL-OWN", false)
	if _, err := f.module.OpenFinancePolicy(ctx, api.OpenFinancePolicy{CommandID: "policy", TenantID: f.tenant, ActorID: "acct-1", LegalEntityID: "LE",
		FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: f.hours(0)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.OpenConsolCostPool(ctx, api.OpenConsolCostPool{CommandID: "pool", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-OWN",
		ConsolidationID: "CONSOL-OWN", OccurredAt: f.hours(1)}); err != nil {
		t.Fatal(err)
	}
	f.w.mu.Lock()
	f.w.consolLines = []rateapi.PricingLine{
		{LineNo: 1, ChargeCode: "OCEAN_FREIGHT", Source: rateapi.SourceRated, HasCost: true, Cost: cny(90_000), CreditorPartyID: "airline-mu"},
		{LineNo: 2, ChargeCode: "CFS_HANDLING", Source: rateapi.SourceRated, HasCost: true, Cost: cny(5_000), CreditorPartyID: "airline-mu"},
	}
	f.w.mu.Unlock()
	if _, err := f.module.RateConsolCostPool(ctx, api.RateConsolCostPool{CommandID: "rate-pool", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-OWN",
		ExpectedRevision: 1, Carrier: "airline-mu", Origin: "CNSHA", Destination: "DEHAM", ServiceLevel: "STANDARD", Method: api.MethodChargeable,
		OccurredAt: f.hours(2)}); err != nil {
		t.Fatalf("rating the pool of an own-container consolidation: %v", err)
	}
	f.w.mu.Lock()
	asked := f.w.asked[len(f.w.asked)-1].(rateapi.RateConsolCost)
	f.w.mu.Unlock()
	if asked.Mode != "SEA" || asked.ConsolidationID != "CONSOL-OWN" || asked.Cargo.Pieces != 12 || asked.Cargo.GrossGrams != 1_700_000 ||
		asked.Cargo.VolumeCM3 != 3_200_000 {
		t.Fatalf("asked RATE for the consolidation: %+v", asked)
	}
	pool, err := f.module.GetConsolCostPool(ctx, api.GetConsolCostPool{TenantID: f.tenant, PoolID: "P-OWN"})
	if err != nil || pool.Mode != "SEA_LCL" || len(pool.Lines) != 2 || pool.Lines[0].Source != "RATED" || pool.Lines[0].ChargeCode != "OCEAN_FREIGHT" ||
		pool.Lines[0].Versions[0].Amount != cny(90_000) || pool.Lines[1].ChargeCode != "CFS_HANDLING" || pool.Lines[1].Versions[0].Amount != cny(5_000) {
		t.Fatalf("pool: %+v %v", pool, err)
	}
}
