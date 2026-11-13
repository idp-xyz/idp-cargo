//go:build integration

package bootstrap

import (
	"context"
	"testing"
	"time"

	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	rateapi "go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// fulPlan answers CostDriver v1 of one planned shipment; phyConsol PhysicalCostBasis v1 of one consolidation.
type fulPlan struct{ d fulapi.CostDriver }

func (f fulPlan) GetShipmentCostDriver(context.Context, fulapi.GetShipmentCostDriver) (fulapi.CostDriver, error) {
	return f.d, nil
}

type phyConsol struct{ b phyapi.PhysicalCostBasis }

func (p phyConsol) GetPhysicalCostBasis(context.Context, phyapi.GetPhysicalCostBasis) (phyapi.PhysicalCostBasis, error) {
	return p.b, nil
}

// RATE V0.5 part 2 with the real ORG, RATE and location reference data (FUL's plan and PHY's consolidation stand in):
// FIN asks RATE to rate a job's shipment and a pool's consolidation, and applies the bases RATE issues.
func TestRatedEstimatesWithTheRealContexts(t *testing.T) {
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
	tenant := uniqueTenant("cargo-rated-")
	seedORG(t, platform, tenant, "party-a", "coloader")
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	for party, role := range map[string]string{"party-a": "CUSTOMER", "coloader": "CO_LOADER"} {
		if _, err = orgModule.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "role-" + party, TenantID: tenant, ActorID: "org-officer", LegalEntityID: "LE",
			PartyID: party, ExpectedRevision: 1, RoleCode: role, ValidFrom: at, ValidUntil: at.Add(365 * 24 * time.Hour), EvidenceRef: "contract",
			OccurredAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	must := func(r any, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	freight := func(rate int64) rateapi.RateLine {
		return rateapi.RateLine{ChargeCode: "AIR_FREIGHT", Currency: "CNY", Calculator: rateapi.CalcPerUnit, Unit: rateapi.UnitKgChargeable, Rate: rate}
	}
	for _, c := range []struct {
		id, side, scope, party string
		lines                  []rateapi.RateLine
	}{
		{"T-AIR", rateapi.SideSell, rateapi.ScopeTariff, "", []rateapi.RateLine{freight(3000), {ChargeCode: "DOC_FEE", Currency: "CNY", Calculator: rateapi.CalcFlat, Rate: 15_000}}},
		{"S-CO", rateapi.SideBuy, rateapi.ScopeSupplier, "coloader", []rateapi.RateLine{freight(2000), {ChargeCode: "THC", Currency: "CNY", Calculator: rateapi.CalcFlat, Rate: 8_000}}},
	} {
		must(rateModule.CreateRateCard(ctx, rateapi.CreateRateCard{CommandID: "card-" + c.id, TenantID: tenant, ActorID: "pricing-1", RateCardID: c.id,
			Side: c.side, Scope: c.scope, LegalEntityID: "LE", PartyID: c.party, Name: c.id, OccurredAt: at}))
		must(rateModule.DraftRateCardVersion(ctx, rateapi.DraftRateCardVersion{CommandID: "draft-" + c.id, TenantID: tenant, ActorID: "pricing-1",
			RateCardID: c.id, ExpectedRevision: 1, ValidFrom: at, Currency: "CNY", SourceRef: "tariff 2026-11", OccurredAt: at}))
		must(rateModule.SetRateCardEntries(ctx, rateapi.SetRateCardEntries{CommandID: "set-" + c.id, TenantID: tenant, ActorID: "pricing-1", RateCardID: c.id,
			ExpectedRevision: 2, Version: 1, Entries: []rateapi.RateEntry{{Mode: rateapi.ModeAir, ContainerMode: rateapi.ContainerLoose, Origin: "CNPVG",
				Destination: "DEFRA", ServiceLevel: "STANDARD", Commodity: rateapi.CommodityGeneral, Lines: c.lines}}, OccurredAt: at}))
		must(rateModule.PublishRateCardVersion(ctx, rateapi.PublishRateCardVersion{CommandID: "publish-" + c.id, TenantID: tenant, ActorID: "pricing-manager",
			RateCardID: c.id, ExpectedRevision: 3, Version: 1, OccurredAt: at.Add(time.Minute)}))
	}
	ul := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	ful := fulPlan{fulapi.CostDriver{TenantID: tenant, ShipmentID: "SH-R", Kind: fulapi.DriverShipmentPlanned, Revision: 2, Status: fulapi.StatusPlanned,
		PlanVersion: 1, TransportMode: "AIR", Origin: ul("CNPVG"), Destination: ul("DEFRA"), LegalEntityID: "LE", CustomerPartyID: "party-a", Packs: 3,
		GrossWeight: kernel.Quantity{Unit: "KGM", Amount: 100}, Volume: kernel.Quantity{Unit: "MTQ", Amount: 1}}}
	phy := phyConsol{phyapi.PhysicalCostBasis{TenantID: tenant, ConsolidationID: "C-R", Revision: 4, Mode: "AIR", Kind: "AIR_CONSOL",
		OperatingLegalEntityID: "LE", State: "PLANNED", Houses: []phyapi.HouseCostBasis{{ShipmentID: "SH-R", MembershipRevision: 1, Packs: 3,
			GrossWeight: kernel.Quantity{Unit: "KGM", Amount: 100}, Volume: kernel.Quantity{Unit: "MTQ", Amount: 1}}}}}
	finModule, err := platform.FIN(orgModule, rateModule, ful, &DocumentSnapshots{}, phy, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	must(finModule.OpenFinancePolicy(ctx, finapi.OpenFinancePolicy{CommandID: "policy", TenantID: tenant, ActorID: "acct-1", LegalEntityID: "LE",
		FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: at.Add(time.Hour)}))
	must(finModule.OpenFreightJob(ctx, finapi.OpenFreightJob{CommandID: "job", TenantID: tenant, ActorID: "fin-1", JobID: "JOB-R", ShipmentID: "SH-R",
		OccurredAt: at.Add(time.Hour)}))
	must(finModule.RateFreightJob(ctx, finapi.RateFreightJob{CommandID: "rate-job", TenantID: tenant, ActorID: "fin-1", JobID: "JOB-R", ExpectedRevision: 1,
		ServiceLevel: "STANDARD", OccurredAt: at.Add(2 * time.Hour)}))
	job, err := finModule.GetFreightJob(ctx, finapi.GetFreightJob{TenantID: tenant, JobID: "JOB-R"})
	if err != nil {
		t.Fatal(err)
	}
	// 1 m3 at 6000 cm3/kg is 166.67 kg, 167 kg in 0.5 kg steps: SELL 30.00/kg = 5,010.00, BUY 20.00/kg = 3,340.00
	byCode := map[string]finapi.ChargeLine{}
	for _, l := range job.Lines {
		byCode[l.ChargeCode] = l
	}
	f, doc, thc := byCode["AIR_FREIGHT"], byCode["DOC_FEE"], byCode["THC"]
	if len(job.Lines) != 3 || len(job.Bases) != 1 || job.Bases[0].Purpose != "SHIPMENT_ESTIMATE" || f.Sell[0].Amount.Minor != 501_000 ||
		f.Cost[0].Amount.Minor != 334_000 || f.Cost[0].PartyID != "coloader" || len(doc.Cost) != 0 || len(thc.Sell) != 0 || thc.Cost[0].Amount.Minor != 8_000 {
		t.Fatalf("job: %+v", job)
	}
	basis, err := rateModule.GetPricingBasis(ctx, rateapi.GetPricingBasis{TenantID: tenant, BasisID: job.Bases[0].BasisID})
	if err != nil || basis.SubjectID != "SH-R" || basis.Customer.PartyID != "party-a" {
		t.Fatalf("RATE's basis: %+v %v", basis, err)
	}
	must(finModule.OpenConsolCostPool(ctx, finapi.OpenConsolCostPool{CommandID: "pool", TenantID: tenant, ActorID: "fin-1", PoolID: "P-R",
		ConsolidationID: "C-R", OccurredAt: at.Add(time.Hour)}))
	must(finModule.RateConsolCostPool(ctx, finapi.RateConsolCostPool{CommandID: "rate-pool", TenantID: tenant, ActorID: "fin-1", PoolID: "P-R",
		ExpectedRevision: 1, Carrier: "coloader", Origin: "CNPVG", Destination: "DEFRA", ServiceLevel: "STANDARD", Method: finapi.MethodChargeable,
		OccurredAt: at.Add(2 * time.Hour)}))
	pool, err := finModule.GetConsolCostPool(ctx, finapi.GetConsolCostPool{TenantID: tenant, PoolID: "P-R"})
	if err != nil || len(pool.Lines) != 2 || pool.Lines[0].Source != "RATED" || pool.Lines[0].ChargeCode != "AIR_FREIGHT" ||
		pool.Lines[0].Versions[0].Amount.Minor != 334_000 || pool.Lines[0].Versions[0].CreditorPartyID != "coloader" {
		t.Fatalf("pool: %+v %v", pool, err)
	}
}
