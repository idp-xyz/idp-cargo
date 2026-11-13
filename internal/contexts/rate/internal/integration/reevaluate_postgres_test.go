//go:build integration

package integration

import (
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// An accepted ONE_OFF quote re-evaluated on the cargo a shipment carries (RATE V0.5 part 3b-2, N1-RATE OD-7 A): its
// rated per-unit freight at the quote's frozen sell and buy rates on the weight shipped, its fuel surcharge at its 10%
// of that freight, its flat and manual lines as quoted; the same quote without a shipment as quoted.
func TestQuoteOnShipmentOnPostgres(t *testing.T) {
	c := newCards(t)
	publishedTariff(c)
	c.must(c.module.CreateRateCard(c.ctx, c.create("S-A", api.SideBuy, api.ScopeSupplier, "coloader-a", "buyer-1")))
	c.must(c.module.DraftRateCardVersion(c.ctx, c.draft("S-A", c.h(0), time.Time{}, 0, "buyer-1")))
	c.must(c.module.SetRateCardEntries(c.ctx, c.set("S-A", 1, "buyer-1", air(api.RateLine{ChargeCode: "AIR_FREIGHT", Currency: "CNY",
		Calculator: api.CalcPerUnit, Unit: api.UnitKgChargeable, Rate: 2200, Breaks: []api.RateBreak{{From: 100_000, Rate: 1200}}}))))
	c.must(c.module.PublishRateCardVersion(c.ctx, c.publish("S-A", 1, "buyer-1")))
	c.must(c.module.EvaluateRates(c.ctx, api.EvaluateRates{CommandID: "evaluate-q", TenantID: c.tenant, ActorID: "sales-1", SnapshotID: "RS-Q",
		Purpose: api.PurposeQuoteLine, LegalEntityID: "LE", Sides: []string{api.SideSell, api.SideBuy}, CustomerPartyID: "acme", Mode: api.ModeAir,
		ContainerMode: api.ContainerLoose, Origin: "CNPVG", Destination: "DEFRA", ServiceLevel: "STANDARD", Commodity: api.CommodityGeneral,
		Cargo: api.Cargo{Pieces: 3, GrossGrams: 100_000, VolumeCM3: 900_000}, OccurredAt: c.h(4)}))
	// 150 kg chargeable: freight 3,000.00 bought at 1,800.00, the document fee 150.00 flat, pickup by hand
	c.must(c.module.CreateQuote(c.ctx, api.CreateQuote{CommandID: "create-Q-1", TenantID: c.tenant, ActorID: "sales-1", QuoteID: "Q-1", LegalEntityID: "LE",
		CustomerPartyID: "acme", Currency: "CNY", Lines: []api.QuoteLine{
			{ChargeCode: "AIR_FREIGHT", Source: api.LineRated, SnapshotID: "RS-Q", Cost: cny(0)},
			{ChargeCode: "DOC_FEE", Source: api.LineRated, SnapshotID: "RS-Q", Cost: cny(0)},
			{ChargeCode: "PICKUP", Sell: cny(20_000), Cost: cny(15_000)},
			{ChargeCode: "FUEL_SURCHARGE", Source: api.LineRated, SnapshotID: "RS-Q", Cost: cny(0)}},
		Validity: kernel.ValidityInterval{From: c.h(5), Until: c.h(5 + 14*24)}, OccurredAt: c.h(5)}))
	c.must(c.module.AcceptQuote(c.ctx, api.AcceptQuote{CommandID: "accept-Q-1", TenantID: c.tenant, ActorID: "sales-1", QuoteID: "Q-1", ExpectedRevision: 1,
		EvidenceKind: api.EvidenceEmail, EvidenceRef: "mail", OccurredAt: c.h(6)}))

	quoted, err := c.module.GetPricingBasis(c.ctx, api.GetPricingBasis{TenantID: c.tenant, QuoteID: "Q-1"})
	if err != nil || quoted.BasisID != "QUOTE:Q-1@2" || quoted.SubjectKind != "" || quoted.Lines[0].Sell != cny(300_000) || quoted.Lines[0].Reevaluated {
		t.Fatalf("as quoted: %+v %v", quoted, err)
	}
	// 1,500 kg shipped: 1,500 kg at the frozen 20.00 = 30,000.00, bought at the frozen 12.00 = 18,000.00
	shipped, err := c.module.GetPricingBasis(c.ctx, api.GetPricingBasis{TenantID: c.tenant, QuoteID: "Q-1",
		Shipment: &api.ShippedCargo{ShipmentID: "SH-1", PlanVersion: 2, Cargo: api.Cargo{Pieces: 30, GrossGrams: 1_500_000}}})
	if err != nil {
		t.Fatal(err)
	}
	f, doc, pickup := shipped.Lines[0], shipped.Lines[1], shipped.Lines[2]
	switch {
	case shipped.BasisID != "QUOTE:Q-1@2/SH-1@2" || shipped.SubjectKind != "SHIPMENT" || shipped.SubjectID != "SH-1" || shipped.SubjectVersion != 2:
		t.Fatalf("the basis on the shipment: %+v", shipped)
	case !f.Reevaluated || f.Sell != cny(3_000_000) || f.Cost != cny(1_800_000) || f.Unit != api.UnitKgChargeable || f.Quantity != 1_500_000:
		t.Errorf("freight: %+v", f)
	case doc.Reevaluated || doc.Sell != cny(15_000) || pickup.Reevaluated || pickup.Sell != cny(20_000) || pickup.Cost != cny(15_000):
		t.Errorf("flat and manual lines: %+v %+v", doc, pickup)
	case !shipped.Lines[3].Reevaluated || shipped.Lines[3].Sell != cny(300_000): // 10% of the freight re-evaluated
		t.Errorf("fuel surcharge: %+v", shipped.Lines[3])
	}
	// on the cargo quoted, the quote itself
	same, err := c.module.GetPricingBasis(c.ctx, api.GetPricingBasis{TenantID: c.tenant, QuoteID: "Q-1",
		Shipment: &api.ShippedCargo{ShipmentID: "SH-2", PlanVersion: 1, Cargo: api.Cargo{Pieces: 3, GrossGrams: 100_000, VolumeCM3: 900_000}}})
	if err != nil {
		t.Fatal(err)
	}
	for i, l := range quoted.Lines {
		if same.Lines[i].Sell != l.Sell || same.Lines[i].Cost != l.Cost {
			t.Errorf("on the cargo quoted, line %s: %+v, quoted %+v", l.ChargeCode, same.Lines[i], l)
		}
	}
	for name, q := range map[string]api.GetPricingBasis{
		"a shipment without its plan version": {TenantID: c.tenant, QuoteID: "Q-1", Shipment: &api.ShippedCargo{ShipmentID: "SH-1"}},
		"a shipment id with a slash":          {TenantID: c.tenant, QuoteID: "Q-1", Shipment: &api.ShippedCargo{ShipmentID: "SH/1@9", PlanVersion: 1}},
		"a basis id and a shipment":           {TenantID: c.tenant, BasisID: "QUOTE:Q-1@2", Shipment: &api.ShippedCargo{ShipmentID: "SH-1", PlanVersion: 1}},
		"negative documents": {TenantID: c.tenant, QuoteID: "Q-1", Shipment: &api.ShippedCargo{ShipmentID: "SH-1", PlanVersion: 1,
			Cargo: api.Cargo{Documents: -1}}},
	} {
		_, err := c.module.GetPricingBasis(c.ctx, q)
		c.rejected(name, err, api.ErrRejected, "")
	}
}
