//go:build integration

package bootstrap

import (
	"context"
	"testing"
	"time"

	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	rateapi "go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// An AGREEMENT quote with the real ORG, RATE, COM and FIN (RATE V0.5 part 3b; FUL and PHY stand in): RATE checks the
// agreement's lane against the co-loader's buy rate, accepting it publishes the customer's card, COM records more
// orders on it than the built-in policy allows a ONE_OFF quote (OD-5 A), and FIN's estimate of the customer's shipment
// is rated with the agreement (OD-1 A).
func TestAgreementWithTheRealContexts(t *testing.T) {
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
	tenant := uniqueTenant("cargo-agreement-")
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
	freight := func(rate int64) []rateapi.RateEntry {
		return []rateapi.RateEntry{{Mode: rateapi.ModeAir, ContainerMode: rateapi.ContainerLoose, Origin: "CNPVG", Destination: "DEFRA", ServiceLevel: "STANDARD",
			Commodity: rateapi.CommodityGeneral, Lines: []rateapi.RateLine{{ChargeCode: "AIR_FREIGHT", Currency: "CNY", Calculator: rateapi.CalcPerUnit,
				Unit: rateapi.UnitKgChargeable, Rate: rate}}}}
	}
	validity := kernel.ValidityInterval{From: at, Until: at.Add(90 * 24 * time.Hour)}
	for _, c := range []struct {
		id, side, scope, party string
		rate                   int64
	}{{"S-CO", rateapi.SideBuy, rateapi.ScopeSupplier, "coloader", 2000}, {"C-A", rateapi.SideSell, rateapi.ScopeCustomer, "party-a", 3000}} {
		must(rateModule.CreateRateCard(ctx, rateapi.CreateRateCard{CommandID: "card-" + c.id, TenantID: tenant, ActorID: "pricing-1", RateCardID: c.id,
			Side: c.side, Scope: c.scope, LegalEntityID: "LE", PartyID: c.party, Name: c.id, OccurredAt: at}))
		must(rateModule.DraftRateCardVersion(ctx, rateapi.DraftRateCardVersion{CommandID: "draft-" + c.id, TenantID: tenant, ActorID: "pricing-1",
			RateCardID: c.id, ExpectedRevision: 1, ValidFrom: validity.From, ValidUntil: validity.Until, Currency: "CNY", SourceRef: "2026 rates",
			OccurredAt: at}))
		must(rateModule.SetRateCardEntries(ctx, rateapi.SetRateCardEntries{CommandID: "set-" + c.id, TenantID: tenant, ActorID: "pricing-1", RateCardID: c.id,
			ExpectedRevision: 2, Version: 1, Entries: freight(c.rate), OccurredAt: at}))
	}
	must(rateModule.PublishRateCardVersion(ctx, rateapi.PublishRateCardVersion{CommandID: "publish-S-CO", TenantID: tenant, ActorID: "pricing-manager",
		RateCardID: "S-CO", ExpectedRevision: 3, Version: 1, OccurredAt: at.Add(time.Minute)}))
	// 100 kg sells at 3,000.00 against 2,000.00 bought: 33% keeps the 20% floor, no approval
	must(rateModule.CreateQuote(ctx, rateapi.CreateQuote{CommandID: "create-Q-AG", TenantID: tenant, ActorID: "sales-1", QuoteID: "Q-AG", LegalEntityID: "LE",
		CustomerPartyID: "party-a", Currency: "CNY", Kind: rateapi.QuoteAgreement, Agreement: &rateapi.AgreementRef{RateCardID: "C-A", Version: 1},
		Scope: &rateapi.QuoteScope{Mode: rateapi.ModeAir, ContainerMode: rateapi.ContainerLoose}, Validity: validity, OccurredAt: at.Add(2 * time.Minute)}))
	must(rateModule.AcceptQuote(ctx, rateapi.AcceptQuote{CommandID: "accept-Q-AG", TenantID: tenant, ActorID: "sales-1", QuoteID: "Q-AG", ExpectedRevision: 1,
		EvidenceKind: rateapi.EvidenceSignedDocument, EvidenceRef: "agreement 2026", OccurredAt: at.Add(time.Hour)}))

	kg := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }
	for _, id := range []string{"O-1", "O-2"} {
		must(comModule.RecordCustomerOrder(ctx, comapi.RecordCustomerOrder{CommandID: "record-" + id, TenantID: tenant, ActorID: "sales-1", OrderID: id,
			QuoteID: "Q-AG", CustomerPartyID: "party-a", OccurredAt: at.Add(2 * time.Hour), Lines: []comapi.OrderLine{
				{Description: "Pallets of machine parts", Packs: 3, PackageType: "PX", GrossWeight: kg(100)}}}))
	}

	ul := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	ful := fulPlan{fulapi.CostDriver{TenantID: tenant, ShipmentID: "SH-A", Kind: fulapi.DriverShipmentPlanned, Revision: 2, Status: fulapi.StatusPlanned,
		PlanVersion: 1, TransportMode: "AIR", Origin: ul("CNPVG"), Destination: ul("DEFRA"), LegalEntityID: "LE", CustomerPartyID: "party-a", Packs: 3,
		GrossWeight: kg(150), Volume: kernel.Quantity{Unit: "MTQ", Amount: 0}}}
	finModule, err := platform.FIN(orgModule, rateModule, ful, &DocumentSnapshots{}, phyConsol{phyapi.PhysicalCostBasis{}}, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	must(finModule.OpenFinancePolicy(ctx, finapi.OpenFinancePolicy{CommandID: "policy", TenantID: tenant, ActorID: "acct-1", LegalEntityID: "LE",
		FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: at.Add(time.Hour)}))
	must(finModule.OpenFreightJob(ctx, finapi.OpenFreightJob{CommandID: "job", TenantID: tenant, ActorID: "fin-1", JobID: "JOB-A", ShipmentID: "SH-A",
		OccurredAt: at.Add(3 * time.Hour)}))
	must(finModule.RateFreightJob(ctx, finapi.RateFreightJob{CommandID: "rate-job", TenantID: tenant, ActorID: "fin-1", JobID: "JOB-A", ExpectedRevision: 1,
		ServiceLevel: "STANDARD", OccurredAt: at.Add(4 * time.Hour)}))
	job, err := finModule.GetFreightJob(ctx, finapi.GetFreightJob{TenantID: tenant, JobID: "JOB-A"})
	// 150 kg at the agreement's 30.00/kg = 4,500.00, bought at 20.00/kg = 3,000.00
	if err != nil || len(job.Lines) != 1 || job.Lines[0].Sell[0].Amount.Minor != 450_000 || job.Lines[0].Cost[0].Amount.Minor != 300_000 {
		t.Fatalf("the shipment rated with the agreement: %+v %v", job, err)
	}
}
