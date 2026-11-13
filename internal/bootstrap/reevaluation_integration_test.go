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

// A ONE_OFF quote rated for 100 kg and shipped at 300 kg, with the real ORG, RATE and FIN (RATE V0.5 part 3b-2; FUL
// and PHY stand in): FIN prices the job by the quote, and RATE re-evaluates its per-unit freight on the 300 kg at the
// quote's frozen sell and buy rates while its flat document fee stays as quoted (N1-RATE OD-7 A).
func TestQuoteReevaluatedOnTheShipmentWithTheRealContexts(t *testing.T) {
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
	tenant := uniqueTenant("cargo-od7-")
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
		{"S-CO", rateapi.SideBuy, rateapi.ScopeSupplier, "coloader", []rateapi.RateLine{freight(2000)}},
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
	// quoted for 100 kg: freight 3,000.00 bought at 2,000.00, the document fee 150.00
	must(rateModule.EvaluateRates(ctx, rateapi.EvaluateRates{CommandID: "evaluate", TenantID: tenant, ActorID: "sales-1", SnapshotID: "RS-1",
		Purpose: rateapi.PurposeQuoteLine, LegalEntityID: "LE", Sides: []string{rateapi.SideSell, rateapi.SideBuy}, CustomerPartyID: "party-a",
		Mode: rateapi.ModeAir, ContainerMode: rateapi.ContainerLoose, Origin: "CNPVG", Destination: "DEFRA", ServiceLevel: "STANDARD",
		Commodity: rateapi.CommodityGeneral, Cargo: rateapi.Cargo{Pieces: 2, GrossGrams: 100_000}, OccurredAt: at.Add(2 * time.Minute)}))
	must(rateModule.CreateQuote(ctx, rateapi.CreateQuote{CommandID: "create-Q-1", TenantID: tenant, ActorID: "sales-1", QuoteID: "Q-1", LegalEntityID: "LE",
		CustomerPartyID: "party-a", Currency: "CNY", Lines: []rateapi.QuoteLine{
			{ChargeCode: "AIR_FREIGHT", Source: rateapi.LineRated, SnapshotID: "RS-1", Cost: kernel.Money{Currency: "CNY"}},
			{ChargeCode: "DOC_FEE", Source: rateapi.LineRated, SnapshotID: "RS-1", Cost: kernel.Money{Currency: "CNY"}}},
		Validity: kernel.ValidityInterval{From: at, Until: at.Add(14 * 24 * time.Hour)}, OccurredAt: at.Add(3 * time.Minute)}))
	must(rateModule.AcceptQuote(ctx, rateapi.AcceptQuote{CommandID: "accept-Q-1", TenantID: tenant, ActorID: "sales-1", QuoteID: "Q-1", ExpectedRevision: 1,
		EvidenceKind: rateapi.EvidenceSignedDocument, EvidenceRef: "signed", OccurredAt: at.Add(time.Hour)}))

	// shipped at 300 kg under the quote's order
	ul := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	ful := fulPlan{fulapi.CostDriver{TenantID: tenant, ShipmentID: "SH-Q", Kind: fulapi.DriverShipmentPlanned, Revision: 2, Status: fulapi.StatusPlanned,
		PlanVersion: 1, TransportMode: "AIR", Origin: ul("CNPVG"), Destination: ul("DEFRA"), LegalEntityID: "LE", CustomerPartyID: "party-a", Packs: 6,
		GrossWeight: kernel.Quantity{Unit: "KGM", Amount: 300}, Volume: kernel.Quantity{Unit: "MTQ", Amount: 0},
		Orders: []fulapi.CostOrder{{OrderID: "O-1", QuoteID: "Q-1", QuoteRevision: 2, Packs: 6}}}}
	finModule, err := platform.FIN(orgModule, rateModule, ful, &DocumentSnapshots{}, phyConsol{phyapi.PhysicalCostBasis{}}, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	must(finModule.OpenFinancePolicy(ctx, finapi.OpenFinancePolicy{CommandID: "policy", TenantID: tenant, ActorID: "acct-1", LegalEntityID: "LE",
		FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: at.Add(time.Hour)}))
	must(finModule.OpenFreightJob(ctx, finapi.OpenFreightJob{CommandID: "job", TenantID: tenant, ActorID: "fin-1", JobID: "JOB-Q", ShipmentID: "SH-Q",
		OccurredAt: at.Add(2 * time.Hour)}))
	must(finModule.ApplyPricingBasis(ctx, finapi.ApplyPricingBasis{CommandID: "apply", TenantID: tenant, ActorID: "fin-1", JobID: "JOB-Q", ExpectedRevision: 1,
		QuoteID: "Q-1", OrderID: "O-1", OccurredAt: at.Add(3 * time.Hour)}))
	job, err := finModule.GetFreightJob(ctx, finapi.GetFreightJob{TenantID: tenant, JobID: "JOB-Q"})
	if err != nil {
		t.Fatal(err)
	}
	byCode := map[string]finapi.ChargeLine{}
	for _, l := range job.Lines {
		byCode[l.ChargeCode] = l
	}
	// 300 kg at the quote's 30.00 = 9,000.00, bought at its 20.00 = 6,000.00; the document fee 150.00 as quoted
	f, doc := byCode["AIR_FREIGHT"], byCode["DOC_FEE"]
	if len(job.Bases) != 1 || job.Bases[0].PlanVersion != 1 || f.Sell[0].Amount.Minor != 900_000 || f.Cost[0].Amount.Minor != 600_000 ||
		doc.Sell[0].Amount.Minor != 15_000 {
		t.Fatalf("the job priced by the quote on 300 kg: %+v", job)
	}
}
