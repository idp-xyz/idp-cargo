//go:build integration

package bootstrap

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	docapi "go.idp.xyz/idp-cargo/internal/contexts/doc/api"
	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	rateapi "go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// The own-container ocean consolidation is costed with the real ORG, RATE, COM, FUL, PHY, BOOK, SEA, DOC and FIN
// (AIR is built only because DOC is). Three LCL orders are consolidated in one own container as in
// TestTheOceanLineIsBilledWithEveryContext, and FIN gives the consolidation a cost pool (V2.77, kind SEA_LCL_OWN):
//  1. before any lot is stuffed FIN opens the pool from PHY's cost basis and records the container's ocean freight,
//     which W/M apportions at stage PLANNED on the planned measures;
//  2. once the stuffing is done and FUL has the load evidence, PHY's cost basis shows every house loaded and the line
//     is apportioned again at stage ACTUAL, on the loaded weights and volumes;
//  3. a CFS charge apportioned by volume and a lashing charge by pieces;
//  4. each job carries its shares, and nothing else as cost, and the profit of the jobs agrees with the consolidation's;
//  5. RATE's BUY card of the line rates a charge the pool does not carry by hand (RateConsolCostPool);
//  6. the line's invoice is matched, the lines are apportioned at their final cost and the pool closes.
func TestTheOceanConsolidationIsCostedWithEveryContext(t *testing.T) {
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
	fulModule, phyModule, err := platform.FULAndPHY(comModule, allowAll{}, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	bookModule, err := platform.BOOK(orgModule, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	airModule, err := platform.AIR(bookModule, fulModule, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	documents := &DocumentSnapshots{}
	finModule, err := platform.FIN(orgModule, rateModule, fulModule, documents, phyModule, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	seaModule, err := platform.SEA(bookModule, fulModule, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	docModule, err := platform.DOC(airModule, seaModule, comModule, orgModule, finModule, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	documents.Bind(docModule)
	tenant := uniqueTenant("cargo-ocean-pool-")
	seedORG(t, platform, tenant, "party-a", "party-b", "party-c", "line-1", "shipper-a", "shipper-b", "shipper-c", "consignee-de")
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	departs := at.Add(72 * time.Hour)
	hours := func(h int) time.Time { return at.Add(time.Duration(h) * time.Hour) }
	kg := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }
	unlocode := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	phyOK := func(_ phyapi.Receipt, err error) { t.Helper(); must("PHY", err) }
	evidence := func(source, ref string, h int) phyapi.Evidence {
		return phyapi.Evidence{Source: source, Reference: ref, OccurredAt: hours(h)}
	}
	movement := func() seaapi.OceanMovement {
		t.Helper()
		m, err := seaModule.GetOceanMovement(ctx, seaapi.GetOceanMovement{TenantID: tenant, MovementID: "OM-1"})
		must("movement", err)
		return m
	}

	// ORG: the shippers and the consignee with their roles and office addresses; the shipping line's CARRIER role.
	for party, role := range map[string]string{"shipper-a": "SHIPPER", "shipper-b": "SHIPPER", "shipper-c": "SHIPPER", "consignee-de": "CONSIGNEE"} {
		_, err = orgModule.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "role-" + party, TenantID: tenant, ActorID: "org-officer", LegalEntityID: "LE",
			PartyID: party, ExpectedRevision: 1, RoleCode: role, ValidFrom: at, EvidenceRef: role + "-agreement", OccurredAt: at})
		must("role "+party, err)
		_, err = orgModule.AddPartyAddress(ctx, orgapi.AddPartyAddress{CommandID: "office-" + party, TenantID: tenant, ActorID: "org-officer", PartyID: party,
			AddressID: "HQ-" + party, Address: orgapi.Address{ShortCode: "HQ", Purposes: []string{orgapi.AddressOffice}, Name: "Office of " + party,
				Lines: []string{"1 Harbour Road"}, City: "Hamburg", CountryCode: "DE"}, DefaultFor: []string{orgapi.AddressOffice}})
		must("office "+party, err)
	}
	_, err = orgModule.RecordPartyIdentifier(ctx, orgapi.RecordPartyIdentifier{CommandID: "eori-consignee", TenantID: tenant, ActorID: "org-officer",
		PartyID: "consignee-de", ExpectedRevision: 2, Type: orgapi.IdentifierEUEORI, Country: "DE", Value: "DE1234567890123", EvidenceRef: "customs letter"})
	must("consignee EORI", err)
	_, err = orgModule.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "role-line-1", TenantID: tenant, ActorID: "org-officer", LegalEntityID: "LE",
		PartyID: "line-1", ExpectedRevision: 1, RoleCode: orgapi.RoleCarrier, ValidFrom: at.Add(-time.Hour), EvidenceRef: "service contract", OccurredAt: at})
	must("role line-1", err)

	// two orders by sea of two customers, one shipment each, planned as houses of the own container C-1
	contexts := slice{org: orgModule, rate: rateModule, com: comModule, ful: fulModule, tenant: tenant, at: at, departs: departs}
	// The houses' jobs carry the SELL side only, for the charges the pool bears: the quotes of A and B have an OCEAN_FREIGHT
	// sell line (USD 600.00 and USD 250.00) and no cost side, and C's agreement is a SELL card only. The cost of those
	// charges comes from the pool alone.
	planSeaSellOnly(t, contexts, "party-a", "A", 60_000)
	planSeaSellOnly(t, contexts, "party-b", "B", 25_000)
	// a third customer's order of 800 kg and 2 m3, under an agreement: such an order is not priced by a quote, its
	// shipment is rated from the cards. The agreement is the customer's SELL card for SEA and LCL: OCEAN_FREIGHT per
	// FREIGHT_TONNE (W/M) USD 300.00 and CFS_HANDLING per CBM USD 50.00. The line's BUY card comes later, after the job is
	// rated, and carries only a charge the pool does not carry by hand (THC per CBM).
	rateCard := func(id, side, scope, party string, lines []rateapi.RateLine) error {
		entry := rateapi.RateEntry{Mode: rateapi.ModeSea, ContainerMode: rateapi.ContainerLCL, Origin: "CNSHA", Destination: "DEHAM", ServiceLevel: "STANDARD",
			Commodity: rateapi.CommodityGeneral, Lines: lines}
		if _, err := rateModule.CreateRateCard(ctx, rateapi.CreateRateCard{CommandID: "card-" + id, TenantID: tenant, ActorID: "pricing-1", RateCardID: id, Side: side,
			Scope: scope, LegalEntityID: "LE", PartyID: party, Name: id, OccurredAt: at}); err != nil {
			return err
		}
		if _, err := rateModule.DraftRateCardVersion(ctx, rateapi.DraftRateCardVersion{CommandID: "draft-" + id, TenantID: tenant, ActorID: "pricing-1", RateCardID: id,
			ExpectedRevision: 1, ValidFrom: at, ValidUntil: at.Add(90 * 24 * time.Hour), Currency: "USD", SourceRef: "tariff 2026-11", OccurredAt: at}); err != nil {
			return err
		}
		if _, err := rateModule.SetRateCardEntries(ctx, rateapi.SetRateCardEntries{CommandID: "set-" + id, TenantID: tenant, ActorID: "pricing-1", RateCardID: id,
			ExpectedRevision: 2, Version: 1, Entries: []rateapi.RateEntry{entry}, OccurredAt: at}); err != nil {
			return err
		}
		if scope == rateapi.ScopeCustomer {
			return nil // a customer's card is published by accepting its agreement quote
		}
		_, err := rateModule.PublishRateCardVersion(ctx, rateapi.PublishRateCardVersion{CommandID: "publish-" + id, TenantID: tenant, ActorID: "pricing-manager",
			RateCardID: id, ExpectedRevision: 3, Version: 1, OccurredAt: at.Add(time.Minute)})
		return err
	}
	agreement := func() error {
		if err := rateCard("C-C", rateapi.SideSell, rateapi.ScopeCustomer, "party-c", []rateapi.RateLine{
			{ChargeCode: "OCEAN_FREIGHT", Currency: "USD", Calculator: rateapi.CalcPerUnit, Unit: rateapi.UnitFreightTonne, Rate: 30_000},
			{ChargeCode: "CFS_HANDLING", Currency: "USD", Calculator: rateapi.CalcPerUnit, Unit: rateapi.UnitCBM, Rate: 5_000}}); err != nil {
			return err
		}
		if err := rateCard("S-LINE", rateapi.SideBuy, rateapi.ScopeSupplier, "line-1", []rateapi.RateLine{
			{ChargeCode: "THC", Currency: "USD", Calculator: rateapi.CalcPerUnit, Unit: rateapi.UnitCBM, Rate: 1_000}}); err != nil {
			return err
		}
		_, err := rateModule.CreateQuote(ctx, rateapi.CreateQuote{CommandID: "create-Q-C", TenantID: tenant, ActorID: "sales-1", QuoteID: "Q-C", LegalEntityID: "LE",
			CustomerPartyID: "party-c", Currency: "USD", Kind: rateapi.QuoteAgreement, Agreement: &rateapi.AgreementRef{RateCardID: "C-C", Version: 1},
			Scope: &rateapi.QuoteScope{Mode: rateapi.ModeSea, ContainerMode: rateapi.ContainerLCL}, Validity: kernel.ValidityInterval{From: at, Until: at.Add(90 * 24 * time.Hour)},
			OccurredAt: at.Add(2 * time.Minute)})
		if err != nil {
			return err
		}
		_, err = rateModule.AcceptQuote(ctx, rateapi.AcceptQuote{CommandID: "accept-Q-C", TenantID: tenant, ActorID: "sales-1", QuoteID: "Q-C", ExpectedRevision: 1,
			EvidenceKind: rateapi.EvidenceSignedDocument, EvidenceRef: "agreement 2026", OccurredAt: at.Add(time.Hour)})
		return err
	}
	planSeaWithVolume(t, contexts, "party-c", "C", kg(800), kernel.Quantity{Unit: "CMQ", Amount: 2_000_000}, agreement)
	orderRevision := func(order string) int64 {
		t.Helper()
		p, err := comModule.GetOrderParties(ctx, comapi.GetOrderParties{TenantID: tenant, OrderID: order})
		must("order parties "+order, err)
		return p.Revision
	}
	for _, s := range []string{"A", "B", "C"} {
		for _, p := range []comapi.OrderParty{{Role: comapi.PartyShipper, PartyID: "shipper-" + strings.ToLower(s)}, {Role: comapi.PartyConsignee, PartyID: "consignee-de"}} {
			_, err = comModule.SetOrderParty(ctx, comapi.SetOrderParty{CommandID: "set-" + p.Role + "-" + s, TenantID: tenant, ActorID: "cs-1", OrderID: "O-" + s,
				ExpectedRevision: orderRevision("O-" + s), Party: p, OccurredAt: hours(7)})
			must("set "+p.Role+" of O-"+s, err)
		}
	}
	phyOK(phyModule.CreateConsolidation(ctx, phyapi.CreateConsolidation{CommandID: "consol", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		Mode: phyapi.ModeSea, Kind: phyapi.KindSeaLCLOwn, OperatingLegalEntityID: "LE", Origin: unlocode("CNSHA"), Destination: unlocode("DEHAM"),
		PlannedDeparture: departs.Add(6 * time.Hour), HouseCutOff: departs.Add(-6 * time.Hour), OccurredAt: hours(8)}))
	phyOK(phyModule.PlanLoadUnit(ctx, phyapi.PlanLoadUnit{CommandID: "plan-unit", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ExpectedRevision: 1, Kind: phyapi.LoadUnitContainer, TypeCode: "22G1", OccurredAt: hours(8)}))
	for _, s := range []string{"SH-A", "SH-B", "SH-C"} {
		phyOK(phyModule.AttachHouse(ctx, phyapi.AttachHouse{CommandID: "attach-" + s, TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1", ShipmentID: s,
			OccurredAt: hours(9)}))
		record(t, ctx, fulModule, tenant, "ful-attach-"+s, s, "C-1", 3, hours(9))
	}

	// BOOK and SEA: the booking with the line, the movement, the HBL numbers drawn from the legal entity's series
	etd := departs.Add(6 * time.Hour)
	contexts.bookSea(t, bookModule, "BK-1", "C-1", "EVER GIVEN", "9811000", "0123W", etd)
	_, err = seaModule.OpenOceanMovement(ctx, seaapi.OpenOceanMovement{CommandID: "open-1", TenantID: tenant, ActorID: "ops-1", MovementID: "OM-1",
		ConsolidationID: "C-1", BookingID: "BK-1", OccurredAt: hours(11)})
	must("open movement", err)
	_, err = seaModule.RegisterHblSeries(ctx, seaapi.RegisterHblSeries{CommandID: "series", TenantID: tenant, ActorID: "ops-1", LegalEntityID: "LE",
		Template: "SHAL{seq:7}", FirstNumber: 1, OccurredAt: hours(11)})
	must("HBL series", err)
	for i, s := range []string{"SH-A", "SH-B", "SH-C"} {
		r, err := seaModule.AssociateHouseBill(ctx, seaapi.AssociateHouseBill{CommandID: "hbl-" + s, TenantID: tenant, ActorID: "ops-1", MovementID: "OM-1",
			ExpectedRevision: movement().Revision, ShipmentID: s, OccurredAt: hours(11)})
		if err != nil || r.HBL != []string{"SHAL0000001", "SHAL0000002", "SHAL0000003"}[i] {
			t.Fatalf("the HBL number of %s: %+v %v", s, r, err)
		}
	}

	// FIN, before any lot is received: the legal entity's policy with its ESTIMATE and ACCOUNTING rates, the debtors'
	// billing profiles and credit terms (DOC asks FIN's credit decision when it issues a bill), and the three jobs: A and
	// B from their orders' quotes, C rated from RATE's cards.
	finPolicyRevision := int64(1)
	policyStep := func(name string) func(finapi.Receipt, error) {
		return func(r finapi.Receipt, err error) {
			t.Helper()
			must(name, err)
			finPolicyRevision = r.NewRevision
		}
	}
	_, err = finModule.OpenFinancePolicy(ctx, finapi.OpenFinancePolicy{CommandID: "fin-policy", TenantID: tenant, ActorID: "acct-1", LegalEntityID: "LE",
		FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: hours(11)})
	must("finance policy", err)
	for _, x := range []struct {
		kind   string
		source string
	}{{finapi.RateEstimate, "CFETS central parity"}, {finapi.RateAccounting, "month-end rate"}} {
		policyStep("rate " + x.kind)(finModule.RecordExchangeRate(ctx, finapi.RecordExchangeRate{CommandID: "fin-rate-" + x.kind, TenantID: tenant, ActorID: "acct-1",
			LegalEntityID: "LE", ExpectedRevision: finPolicyRevision, RateType: x.kind, From: "USD", Rate: 71_234, Scale: 4, FromDigits: 2, ToDigits: 2, ValidFrom: at,
			ValidUntil: at.AddDate(0, 1, 0), Source: x.source, OccurredAt: hours(11)}))
	}
	for _, s := range []string{"a", "b", "c"} {
		_, err = finModule.SetBillingProfile(ctx, finapi.SetBillingProfile{CommandID: "fin-profile-" + s, TenantID: tenant, ActorID: "bill-1", DebtorPartyID: "party-" + s,
			BuyerKind: finapi.BuyerForeign, EvidenceRef: "account opening form AO-2026-" + s, OccurredAt: hours(11)})
		must("billing profile of party-"+s, err)
		_, err = finModule.SetCreditTerms(ctx, finapi.SetCreditTerms{CommandID: "fin-terms-" + s, TenantID: tenant, ActorID: "credit-1", LegalEntityID: "LE",
			DebtorPartyID: "party-" + s, Limit: kernel.Money{Currency: "CNY", Minor: 10_000_000}, PaymentDays: 30, PaymentBasis: finapi.TermsFromMonthEnd, OccurredAt: hours(11)})
		must("credit terms of party-"+s, err)
	}
	for _, s := range []string{"A", "B"} {
		job := "JOB-" + s
		r, err := finModule.OpenFreightJob(ctx, finapi.OpenFreightJob{CommandID: "fin-job-" + s, TenantID: tenant, ActorID: "fin-1", JobID: job, ShipmentID: "SH-" + s,
			OccurredAt: hours(11)})
		must("open "+job, err)
		_, err = finModule.ApplyPricingBasis(ctx, finapi.ApplyPricingBasis{CommandID: "fin-quote-" + s, TenantID: tenant, ActorID: "fin-1", JobID: job,
			ExpectedRevision: r.NewRevision, QuoteID: "Q-" + s, OrderID: "O-" + s, OccurredAt: hours(11)})
		must("the pricing basis of "+job+" from the accepted quote", err)
	}
	openedC, err := finModule.OpenFreightJob(ctx, finapi.OpenFreightJob{CommandID: "fin-job-C", TenantID: tenant, ActorID: "fin-1", JobID: "JOB-C", ShipmentID: "SH-C",
		OccurredAt: hours(11)})
	must("open JOB-C", err)
	_, err = finModule.RateFreightJob(ctx, finapi.RateFreightJob{CommandID: "fin-rate-C", TenantID: tenant, ActorID: "fin-1", JobID: "JOB-C",
		ExpectedRevision: openedC.NewRevision, ServiceLevel: "STANDARD", OccurredAt: hours(11)})
	must("RateFreightJob of JOB-C", err)
	// RATE refuses to accept an agreement quote (the state transition from priced to accepted fails, as its margin cannot be
	// checked) while no BUY card of the lane exists, so the line's BUY card exists and has one line, THC per CBM: a charge the
	// pool rates in stage 5. RateFreightJob prices it into JOB-C as a cost with no sell (CNY 142.47). The pool carries it,
	// so FIN is told to release JOB-C's accrual: the charge is in the pool only.
	jobC, err := finModule.GetFreightJob(ctx, finapi.GetFreightJob{TenantID: tenant, JobID: "JOB-C"})
	must("JOB-C", err)
	for _, l := range jobC.Lines {
		if l.ChargeCode != "THC" {
			continue
		}
		_, err = finModule.ReleaseJobAccrual(ctx, finapi.ReleaseJobAccrual{CommandID: "fin-release-thc-C", TenantID: tenant, ActorID: "fin-1", JobID: "JOB-C",
			ExpectedRevision: jobC.Revision, LineNo: l.LineNo, ExpectedSideVersion: l.Cost[len(l.Cost)-1].Version, Reason: "the pool carries the THC of the container",
			OccurredAt: hours(11)})
		must("release the THC accrual of JOB-C", err)
	}
	cny := func(n int64) kernel.Money { return kernel.Money{Currency: "CNY", Minor: n} }
	houses := []struct{ shipment, job string }{{"SH-A", "JOB-A"}, {"SH-B", "JOB-B"}, {"SH-C", "JOB-C"}}
	profit := func(job string) finapi.JobProfit {
		t.Helper()
		p, err := finModule.GetJobProfit(ctx, finapi.GetJobProfit{TenantID: tenant, JobID: job})
		must("profit of "+job, err)
		return p
	}
	// what the three jobs earn at the ESTIMATE rate 7.1234 and spend on their own, which is nothing: A sells USD 600.00
	// (CNY 4,274.04), B USD 250.00 (CNY 1,780.85) and C USD 600.00 + 100.00 (2 freight tonnes at 300.00 and 2 CBM at 50.00,
	// CNY 4,274.04 + 712.34), so revenue 427,404 / 178,085 / 498,638 CNY, cost 0
	for i, want := range []struct{ revenue, cost int64 }{{427_404, 0}, {178_085, 0}, {498_638, 0}} {
		if p := profit(houses[i].job); p.Revenue != cny(want.revenue) || p.Cost != cny(want.cost) || p.PoolCost != cny(0) || len(p.PoolShares) != 0 {
			t.Fatalf("%s before the pool: %+v", houses[i].job, p)
		}
	}

	// The cost pool opens before any lot is received, with the real PHY's cost basis of the planned houses: kind
	// SEA_LCL_OWN (what PHY reports for the own container), planned scope 6 packs and 300 kg for A and B (no volume
	// declared), 800 kg and 2 m3 for C, nothing loaded.
	physicalBasis := func() phyapi.PhysicalCostBasis {
		t.Helper()
		b, err := phyModule.GetPhysicalCostBasis(ctx, phyapi.GetPhysicalCostBasis{TenantID: tenant, ConsolidationID: "C-1"})
		must("PHY's cost basis", err)
		return b
	}
	if b := physicalBasis(); b.Kind != phyapi.KindSeaLCLOwn || b.Mode != phyapi.ModeSea || b.OperatingLegalEntityID != "LE" || len(b.Houses) != 3 {
		t.Fatalf("stage 1: PHY's cost basis: %+v", b)
	} else {
		for i, h := range b.Houses {
			wantKg, wantVolume := int64(300), int64(0)
			if i == 2 {
				wantKg, wantVolume = 800, 2_000_000
			}
			if h.ShipmentID != houses[i].shipment || h.Packs != 6 || h.GrossWeight != kg(wantKg) || h.Volume.Amount != wantVolume || h.LoadedPacks != 0 {
				t.Fatalf("stage 1: planned scope of house %d: %+v", i, h)
			}
		}
	}
	pool := func() finapi.ConsolCostPool {
		t.Helper()
		p, err := finModule.GetConsolCostPool(ctx, finapi.GetConsolCostPool{TenantID: tenant, PoolID: "P-1"})
		must("the pool", err)
		return p
	}
	_, err = finModule.OpenConsolCostPool(ctx, finapi.OpenConsolCostPool{CommandID: "fin-pool", TenantID: tenant, ActorID: "fin-1", PoolID: "P-1",
		ConsolidationID: "C-1", OccurredAt: hours(12)})
	must("open the pool of the own-container consolidation", err)
	if p := pool(); p.Mode != "SEA_LCL" || p.ConsolidationID != "C-1" || p.LegalEntityID != "LE" || p.State != finapi.PoolOpen || p.FunctionalCurrency != "CNY" {
		t.Fatalf("stage 1: the pool: %+v", p)
	}
	recordCost := func(id, code, ref, method string, amount int64, h int) {
		t.Helper()
		_, err := finModule.RecordConsolCost(ctx, finapi.RecordConsolCost{CommandID: id, TenantID: tenant, ActorID: "fin-1", PoolID: "P-1", ExpectedRevision: pool().Revision,
			ChargeCode: code, SupplierRef: ref, Method: method, Cost: finapi.ChargeSide{PartyID: "line-1", Amount: usd(amount)}, Reason: "line's quote for the container",
			OccurredAt: hours(h)})
		must("cost line "+code, err)
	}
	apportion := func(id string, h int, lines ...int) {
		t.Helper()
		_, err := finModule.ApportionConsolCosts(ctx, finapi.ApportionConsolCosts{CommandID: id, TenantID: tenant, ActorID: "fin-1", PoolID: "P-1",
			ExpectedRevision: pool().Revision, LineNos: lines, OccurredAt: hours(h)})
		must("apportion "+id, err)
	}
	// checkShares reads a line's current shares, those of the latest apportionment that covers it: that apportionment's
	// stage, each house's quantity, unit, share in USD and share in CNY (the shares sum to the line's amount and local amount)
	checkShares := func(what, stage string, w ownPoolLine) {
		t.Helper()
		p := pool()
		for n := len(p.Apportionments) - 1; n >= 0; n-- {
			a := p.Apportionments[n]
			for _, l := range a.Lines {
				if l.LineNo != w.line {
					continue
				}
				if a.Stage != stage || l.Method != w.method || len(l.Shares) != 3 {
					t.Fatalf("%s: stage %s, %+v", what, a.Stage, l)
				}
				var sumUSD, sumCNY int64
				for i, s := range l.Shares {
					want := finapi.PoolShare{ShareNo: i + 1, ShipmentID: houses[i].shipment, JobID: houses[i].job, Quantity: w.quantity[i], Unit: w.unit, Amount: usd(w.usd[i]),
						Local: cny(w.cny[i])}
					if s != want {
						t.Fatalf("%s: share %d is %+v, want %+v", what, i+1, s, want)
					}
					sumUSD, sumCNY = sumUSD+s.Amount.Minor, sumCNY+s.Local.Minor
				}
				if sumUSD != l.Amount.Minor || sumCNY != l.Local.Minor || l.Amount.Minor != w.usd[0]+w.usd[1]+w.usd[2] {
					t.Fatalf("%s: the shares do not sum to the line: %+v", what, l)
				}
				return
			}
		}
		t.Fatalf("%s: line %d is in no apportionment: %+v", what, w.line, p.Apportionments)
	}
	// Item 1: the container's ocean freight, USD 1,000.00 (CNY 7,123.40 at 7.1234), by CHARGEABLE, which is W/M for an
	// LCL pool. Nothing is loaded, so the basis is the planned scope and the stage PLANNED: SH-A 300 kg and no volume is
	// 300 freight milli-tonnes, SH-B 300, SH-C 2 m3 over 800 kg is 2000: 2600 in all. USD 100,000 divides 11,538.46 /
	// 11,538.46 / 76,923.08: the floors sum to 99,999 and the unit left goes to the largest remainder, which ties between
	// SH-A and SH-B (1200 of 2600) and goes to the first, so 11,539 / 11,538 / 76,923. CNY 712,340 divides 82,193.08 /
	// 82,193.08 / 547,953.85: floors 82,193 / 82,193 / 547,953 sum to 712,339 and the unit goes to SH-C (2200 of 2600):
	// 82,193 / 82,193 / 547,954.
	recordCost("fin-cost-freight", "OCEAN_FREIGHT", "LINE-BK-1", finapi.MethodChargeable, 100_000, 12)
	apportion("fin-apportion-planned", 12)
	checkShares("stage 1: the freight on the planned scope", finapi.BasisPlanned, ownPoolLine{1, finapi.MethodChargeable, "MWM", [3]int64{300, 300, 2000},
		[3]int64{11_539, 11_538, 76_923}, [3]int64{82_193, 82_193, 547_954}})

	// PHY: both lots received and weighed at the CFS, stuffed into the 22G1 and sealed; FUL takes the evidence of both
	// houses and SEA refreshes them
	c, err := phyModule.GetConsolidation(ctx, phyapi.GetConsolidation{TenantID: tenant, ConsolidationID: "C-1"})
	must("consolidation", err)
	phyOK(phyModule.CloseConsolidation(ctx, phyapi.CloseConsolidation{CommandID: "close", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ExpectedRevision: c.Revision, OccurredAt: hours(13)}))
	cfs := phyapi.Facility{Location: c.Current.Origin, Code: "SHA-CFS1"}
	volumes := map[string]kernel.Quantity{"A": {Unit: "CMQ", Amount: 800_000}, "B": {Unit: "CMQ", Amount: 200_000}, "C": {Unit: "CMQ", Amount: 2_000_000}}
	weights := map[string]int64{"A": 312, "B": 288, "C": 800}
	for _, s := range []string{"A", "B", "C"} {
		lot := "L-" + s
		phyOK(phyModule.ReceiveCargo(ctx, phyapi.ReceiveCargo{CommandID: "rcv-" + s, TenantID: tenant, ActorID: "cfs-1", LotID: lot, ShipmentID: "SH-" + s,
			Facility: cfs, Packs: 6, PackageType: "CT", Condition: phyapi.ConditionGood, Evidence: evidence("CFS-WMS", "GRN-"+s, 14), OccurredAt: hours(14)}))
		phyOK(phyModule.RecordCargoMeasurement(ctx, phyapi.RecordCargoMeasurement{CommandID: "weigh-" + s, TenantID: tenant, ActorID: "cfs-1", LotID: lot,
			ExpectedRevision: 1, GrossWeight: kg(weights[s]), Volume: volumes[s], Method: phyapi.MethodScale, Evidence: evidence("CFS-SCALE", "W-"+s, 14), OccurredAt: hours(14)}))
		phyOK(phyModule.CompleteReceipt(ctx, phyapi.CompleteReceipt{CommandID: "complete-" + s, TenantID: tenant, ActorID: "cfs-1", LotID: lot,
			ExpectedRevision: 2, OccurredAt: hours(14)}))
	}
	phyOK(phyModule.OpenLoadExecution(ctx, phyapi.OpenLoadExecution{CommandID: "stuff", TenantID: tenant, ActorID: "cfs-1", ExecutionID: "X-1",
		ConsolidationID: "C-1", Kind: phyapi.ExecutionStuffing, Facility: cfs, LoadUnitNo: 1, OccurredAt: hours(15)}))
	phyOK(phyModule.StartLoading(ctx, phyapi.StartLoading{CommandID: "start", TenantID: tenant, ActorID: "cfs-1", ExecutionID: "X-1", ExpectedRevision: 1,
		OccurredAt: hours(15)}))
	for i, s := range []string{"A", "B", "C"} {
		phyOK(phyModule.RecordCargoLoaded(ctx, phyapi.RecordCargoLoaded{CommandID: "load-" + s, TenantID: tenant, ActorID: "cfs-1", ExecutionID: "X-1",
			ExpectedRevision: int64(2 + i), LotID: "L-" + s, Packs: 6, Evidence: evidence("CFS-TALLY", "T-"+s, 15), OccurredAt: hours(15)}))
	}
	phyOK(phyModule.CloseLoadExecution(ctx, phyapi.CloseLoadExecution{CommandID: "seal", TenantID: tenant, ActorID: "cfs-1", ExecutionID: "X-1",
		ExpectedRevision: 5, Evidence: evidence(phyapi.EvidenceSeal, "CN-SEAL-77120", 15), OccurredAt: hours(15)}))
	for _, s := range []string{"SH-A", "SH-B", "SH-C"} {
		_, err = fulModule.RecordVerifiedLoadEvidence(ctx, fulapi.RecordVerifiedLoadEvidence{CommandID: "ful-stuffed-" + s, TenantID: tenant, ActorID: "ops-1",
			ShipmentID: s, ExpectedRevision: 4, OccurredAt: hours(16)})
		must("FUL evidence of "+s, err)
		_, err = seaModule.RefreshHouseBillFacts(ctx, seaapi.RefreshHouseBillFacts{CommandID: "refresh-" + s, TenantID: tenant, ActorID: "ops-1", MovementID: "OM-1",
			ExpectedRevision: movement().Revision, ShipmentID: s, OccurredAt: hours(16)})
		must("refresh "+s, err)
	}

	// PHY's cost basis now shows every house loaded: 6 packs each, A 312 kg and 0.8 m3, B 288 kg and 0.2 m3, C 800 kg and
	// 2 m3 (the planned scope stays what it was at attach)
	loaded := []struct{ kilos, cubicCm int64 }{{312, 800_000}, {288, 200_000}, {800, 2_000_000}}
	for i, h := range physicalBasis().Houses {
		if h.ShipmentID != houses[i].shipment || h.LoadedPacks != 6 || h.LoadedGrossWeight != kg(loaded[i].kilos) ||
			h.LoadedVolume != (kernel.Quantity{Unit: "CMQ", Amount: loaded[i].cubicCm}) || h.Packs != 6 || h.GrossWeight != kg(map[int]int64{0: 300, 1: 300, 2: 800}[i]) {
			t.Fatalf("stage 2: PHY's cost basis after the stuffing, house %d: %+v", i, h)
		}
	}
	// Item 2: the same line apportioned again, now at stage ACTUAL on the loaded measures. W/M: SH-A 312 kg under 0.8 m3
	// is 800, SH-B 288 kg over 0.2 m3 is 288, SH-C 800 kg under 2 m3 is 2000: 3088 in all. USD 100,000 divides 25,906.74 /
	// 9,326.42 / 64,766.58: the floors 25,906 / 9,326 / 64,766 sum to 99,998 and the two units left go to the largest
	// remainders (SH-C 2592, SH-A 2272 of 3088), so 25,907 / 9,326 / 64,767. CNY 712,340 divides 184,544.04 / 66,435.85 /
	// 461,360.10: floors 184,544 / 66,435 / 461,360 sum to 712,339 and the unit goes to SH-B (2640 of 3088): 184,544 /
	// 66,436 / 461,360.
	wantFreight := ownPoolLine{1, finapi.MethodChargeable, "MWM", [3]int64{800, 288, 2000}, [3]int64{25_907, 9_326, 64_767}, [3]int64{184_544, 66_436, 461_360}}
	apportion("fin-apportion-actual", 17)
	checkShares("stage 2: the freight on the loaded measures", finapi.BasisActual, wantFreight)
	if p := pool(); len(p.Apportionments) != 2 || p.Apportionments[0].Stage != finapi.BasisPlanned {
		t.Fatalf("stage 2: the planned apportionment stays in the pool's history: %+v", p.Apportionments)
	}

	// SEA: the container, its seal and the stuffing; the verified gross mass, gate-in and loading on board; the
	// carrier's master bill reference; the facts finalized
	r, err := seaModule.AssignContainer(ctx, seaapi.AssignContainer{CommandID: "assign", TenantID: tenant, ActorID: "ops-1", VisitID: "CV-1", MovementID: "OM-1",
		LoadUnitNo: 1, ContainerNumber: "csqu3054383", SizeType: "22G1", TareKg: 2180, MaxGrossKg: 30480, PlateSource: seaapi.PlateCSC, OccurredAt: hours(12)})
	must("assign container", err)
	visit := r.NewRevision
	if r, err = seaModule.RecordContainerSeal(ctx, seaapi.RecordContainerSeal{CommandID: "sea-seal", TenantID: tenant, ActorID: "cfs-1", VisitID: "CV-1",
		ExpectedRevision: visit, Number: "cn-seal-77120", Party: seaapi.SealShipper, AppliedAt: hours(15), Source: "CFS tally T-A", OccurredAt: hours(15)}); err != nil {
		t.Fatalf("seal: %v", err)
	}
	visit = r.NewRevision
	if r, err = seaModule.RecordStuffingDone(ctx, seaapi.RecordStuffingDone{CommandID: "stuffed", TenantID: tenant, ActorID: "cfs-1", VisitID: "CV-1",
		ExpectedRevision: visit, OccurredAt: hours(16)}); err != nil {
		t.Fatalf("stuffing done: %v", err)
	}
	visit = r.NewRevision
	if r, err = seaModule.AssertVgm(ctx, seaapi.AssertVgm{CommandID: "vgm", TenantID: tenant, ActorID: "ops-1", VisitID: "CV-1", ExpectedRevision: visit,
		Mass: kg(3700), Method: seaapi.VgmMethod2, ResponsiblePartyID: "LE", ResponsiblePartyRevision: 1, DeterminedAt: hours(17), Signatory: "Ada Lovelace",
		OccurredAt: hours(17)}); err != nil {
		t.Fatalf("VGM: %v", err)
	}
	visit = r.NewRevision
	if r, err = seaModule.RecordVgmResponse(ctx, seaapi.RecordVgmResponse{CommandID: "vgm-answer", TenantID: tenant, ActorID: "ops-1", VisitID: "CV-1",
		ExpectedRevision: visit, Response: seaapi.VgmAccepted, Source: "carrier portal", OccurredAt: hours(18)}); err != nil {
		t.Fatalf("VGM answer: %v", err)
	}
	visit = r.NewRevision
	for i, e := range []struct{ kind, reference string }{{seaapi.EventGateIn, "GIN-1"}, {seaapi.EventLoaded, "LD-1"}} {
		if r, err = seaModule.RecordContainerEvent(ctx, seaapi.RecordContainerEvent{CommandID: "event-" + e.reference, TenantID: tenant, ActorID: "ops-1",
			VisitID: "CV-1", ExpectedRevision: visit, Kind: e.kind, Location: unlocode("CNSHG"), HappenedAt: hours(19 + i), Source: "terminal", Reference: e.reference,
			OccurredAt: hours(19 + i)}); err != nil {
			t.Fatalf("%s: %v", e.kind, err)
		}
		visit = r.NewRevision
	}
	bill := seaapi.Evidence{SHA256: strings.Repeat("9c", 32), StorageID: "mail/mbl.pdf", MediaType: "application/pdf", CapturedAt: hours(20)}
	_, err = seaModule.RecordMasterBill(ctx, seaapi.RecordMasterBill{CommandID: "mbl", TenantID: tenant, ActorID: "ops-1", MovementID: "OM-1",
		ExpectedRevision: movement().Revision, Number: "linu2611000457", Kind: seaapi.MasterBillCarrierMBL, Evidence: bill, OccurredAt: hours(20)})
	must("master bill", err)
	_, err = seaModule.FinalizeOceanDocFacts(ctx, seaapi.FinalizeOceanDocFacts{CommandID: "finalize", TenantID: tenant, ActorID: "ops-2", MovementID: "OM-1",
		ExpectedRevision: movement().Revision, OccurredAt: hours(21)})
	must("finalize", err)
	facts, err := seaModule.GetOceanDocFacts(ctx, seaapi.GetOceanDocFacts{TenantID: tenant, MovementID: "OM-1"})
	if err != nil || facts.Version != 1 || facts.Status != seaapi.DocFactsFinal || facts.MasterBillNumber != "LINU2611000457" ||
		facts.MasterBillKind != seaapi.MasterBillCarrierMBL || facts.CarrierPartyID != "line-1" || facts.CarrierRole != seaapi.RoleCarrier ||
		facts.Vessel != "EVER GIVEN" || facts.Voyage != "0123W" || facts.PortOfLoading != "CNSHG" || facts.PortOfDischarge != "DEHAM" ||
		!facts.OnBoardAt.Equal(hours(20)) || len(facts.Containers) != 1 || len(facts.Houses) != 3 {
		t.Fatalf("stage 1: the FINAL facts: %+v %v", facts, err)
	}

	// DOC: the authority for house bills, A's house bill of lading as three originals, B's house sea waybill and C's bill
	// released EXPRESS, each issued under the lock of its order's parties
	authority := func(kind string) {
		t.Helper()
		_, err := docModule.RecordIssuanceAuthority(ctx, docapi.RecordIssuanceAuthority{CommandID: "auth-" + kind, TenantID: tenant, ActorID: "legal-1",
			AuthorityID: "AUTH-" + kind, LegalEntityID: "LE", Kind: kind, IssuePlace: "CNSHG", Qualification: kind + " filing", Validity: kernel.ValidityInterval{From: at},
			EvidenceRef: "filings/" + kind, OccurredAt: hours(21)})
		must("authority "+kind, err)
	}
	authority(docapi.KindHBL)
	authority(docapi.KindHouseSeaWaybill)
	for _, d := range []struct {
		id, kind, shipment, order, payment, release string
		originals                                   int
	}{
		{"DOC-HBL-A", docapi.KindHBL, "SH-A", "O-A", docapi.PaymentPrepaid, docapi.ReleaseOriginals, 3},
		{"DOC-HSW-B", docapi.KindHouseSeaWaybill, "SH-B", "O-B", docapi.PaymentCollect, docapi.ReleaseSeaWaybill, 0},
		{"DOC-HBL-C", docapi.KindHBL, "SH-C", "O-C", docapi.PaymentPrepaid, docapi.ReleaseExpress, 0},
	} {
		_, err = docModule.DraftTransportDocument(ctx, docapi.DraftTransportDocument{CommandID: "draft-" + d.id, TenantID: tenant, ActorID: "doc-1", DocumentID: d.id,
			Kind: d.kind, MovementID: "OM-1", ShipmentID: d.shipment, PaymentIndicator: d.payment, Release: d.release, Originals: d.originals, OccurredAt: hours(22)})
		must("draft "+d.id, err)
		_, err = docModule.ApproveTransportDocument(ctx, docapi.ApproveTransportDocument{CommandID: "approve-" + d.id, TenantID: tenant, ActorID: "doc-2", DocumentID: d.id,
			ExpectedRevision: 1, OccurredAt: hours(22)})
		must("approve "+d.id, err)
		_, err = comModule.LockOrderParties(ctx, comapi.LockOrderParties{CommandID: "lock-" + d.id, TenantID: tenant, ActorID: "doc-2", OrderID: d.order,
			ExpectedRevision: orderRevision(d.order), HolderID: d.id, OccurredAt: hours(23)})
		must("lock "+d.order, err)
		_, err = docModule.IssueTransportDocument(ctx, docapi.IssueTransportDocument{CommandID: "issue-" + d.id, TenantID: tenant, ActorID: "doc-2", DocumentID: d.id,
			ExpectedRevision: 2, OccurredAt: hours(24)})
		must("issue "+d.id, err)
		if doc, err := docModule.GetTransportDocument(ctx, docapi.GetTransportDocument{TenantID: tenant, DocumentID: d.id}); err != nil || doc.State != docapi.StateIssued ||
			doc.ConsolidationID != "C-1" || doc.ShipmentID != d.shipment {
			t.Fatalf("the issued %s: %+v %v", d.id, doc, err)
		}
	}

	// Item 3: a CFS charge of USD 100.00 apportioned by VOLUME and a lashing charge of USD 30.00 by PIECES, both on the
	// loaded measures. Volume in cm3: 800,000 / 200,000 / 2,000,000, 3,000,000 in all. USD 10,000 divides 2,666.67 /
	// 666.67 / 6,666.67: the floors 2,666 / 666 / 6,666 sum to 9,998 and the two units left go to the largest remainders,
	// all equal (2,000,000 of 3,000,000), so to the first two houses: 2,667 / 667 / 6,666. CNY 71,234 (10,000 at 7.1234)
	// divides 18,995.73 / 4,748.93 / 47,489.33: floors 18,995 / 4,748 / 47,489 sum to 71,232 and the two units go to the
	// largest remainders (SH-B 2.8M, SH-A 2.2M of 3M): 18,996 / 4,749 / 47,489. Six packs each: USD 3,000 divides 1,000
	// each, and CNY 21,370 (3,000 at 7.1234, rounded) divides 7,123.33 each: the floors sum to 21,369 and the unit goes to
	// the first of three equal remainders: 7,124 / 7,123 / 7,123.
	wantCFS := ownPoolLine{2, finapi.MethodVolume, "CMQ", [3]int64{800_000, 200_000, 2_000_000}, [3]int64{2_667, 667, 6_666}, [3]int64{18_996, 4_749, 47_489}}
	wantLashing := ownPoolLine{3, finapi.MethodPieces, "PCS", [3]int64{6, 6, 6}, [3]int64{1_000, 1_000, 1_000}, [3]int64{7_124, 7_123, 7_123}}
	recordCost("fin-cost-cfs", "CFS_HANDLING", "CFS-1", finapi.MethodVolume, 10_000, 26)
	recordCost("fin-cost-lashing", "LASHING", "CFS-2", finapi.MethodPieces, 3_000, 26)
	apportion("fin-apportion-charges", 26, 2, 3)
	checkShares("stage 3: the CFS charge by volume", finapi.BasisActual, wantCFS)
	checkShares("stage 3: the lashing charge by pieces", finapi.BasisActual, wantLashing)
	checkShares("stage 3: the freight keeps its shares", finapi.BasisActual, wantFreight)

	// Item 4: each job's cost is its share of the pool, and nothing else: the jobs have no cost of their own for the charges
	// the pool bears. The pool costs of SH-A, SH-B and SH-C are the sum of their shares in CNY: 184,544 + 18,996 + 7,124 =
	// 210,664; 66,436 + 4,749 + 7,123 = 78,308; 461,360 + 47,489 + 7,123 = 515,972 (together the pool's 712,340 + 71,234 +
	// 21,370 = 804,944). Revenue is 427,404 / 178,085 / 498,638, so the profit is 427,404 - 210,664 = 216,740; 178,085 -
	// 78,308 = 99,777; 498,638 - 515,972 = -17,334.
	checkJobs := func(what string, pooled, profits [3]int64, shares int) {
		t.Helper()
		revenue := [3]int64{427_404, 178_085, 498_638}
		for i, h := range houses {
			p := profit(h.job)
			if p.Revenue != cny(revenue[i]) || p.Cost != cny(0) || p.PoolCost != cny(pooled[i]) || p.Profit != cny(profits[i]) || len(p.PoolShares) != shares {
				t.Fatalf("%s: the profit of %s: %+v", what, h.job, p)
			}
			for _, s := range p.PoolShares {
				if s.PoolID != "P-1" || s.Stage != finapi.BasisActual {
					t.Fatalf("%s: a pool share of %s: %+v", what, h.job, s)
				}
			}
		}
	}
	// checkConsol reads GetConsolProfit: the pooled cost is the sum of the pool's lines, the houses' revenue the sum of their
	// jobs', and the consolidation's profit, its houses' revenue less the pool's lines, agrees with the jobs'
	checkConsol := func(what, state string, pooled int64, apportioned int64, pool [3]int64, profits [3]int64) {
		t.Helper()
		c, err := finModule.GetConsolProfit(ctx, finapi.GetConsolProfit{TenantID: tenant, ConsolidationID: "C-1"})
		must(what, err)
		if c.PoolID != "P-1" || c.State != state || c.Pooled != cny(pooled) || c.Apportioned != cny(apportioned) || c.Unapportioned != cny(pooled-apportioned) ||
			c.Retained != cny(0) || len(c.Houses) != 3 {
			t.Fatalf("%s: %+v", what, c)
		}
		var revenue, poolCost, profitSum int64
		for i, h := range c.Houses {
			p := profit(houses[i].job)
			if h.ShipmentID != houses[i].shipment || h.JobID != houses[i].job || h.Revenue != p.Revenue || h.Cost != cny(0) || h.PoolCost != cny(pool[i]) ||
				h.Profit != cny(profits[i]) {
				t.Fatalf("%s: house %d: %+v against its job %+v", what, i, h, p)
			}
			revenue, poolCost, profitSum = revenue+h.Revenue.Minor, poolCost+h.PoolCost.Minor, profitSum+h.Profit.Minor
		}
		if revenue != 1_104_127 || poolCost != apportioned || profitSum != revenue-apportioned {
			t.Fatalf("%s: the houses sum to revenue %d, pool cost %d, profit %d", what, revenue, poolCost, profitSum)
		}
	}
	checkJobs("stage 4", [3]int64{210_664, 78_308, 515_972}, [3]int64{216_740, 99_777, -17_334}, 3)
	// the consolidation's revenue is 427,404 + 178,085 + 498,638 = 1,104,127 and the pool holds 804,944 (all apportioned), so
	// its profit is 1,104,127 - 804,944 = 299,183, the sum of the jobs' 216,740 + 99,777 - 17,334
	checkConsol("stage 4: the consolidation's profit", finapi.PoolOpen, 804_944, 804_944, [3]int64{210_664, 78_308, 515_972},
		[3]int64{216_740, 99_777, -17_334})

	// Item 5: RATE rates a charge the pool does not carry by hand, from the line's BUY card (THC per CBM USD 10.00) for the
	// lane and the three houses' planned scope, 18 packs, 1.4 t and 2 m3, so 2 CBM: THC USD 20.00 (2,000 minor units),
	// accrued to the line at 7.1234 (CNY 142.47). The card has no OCEAN_FREIGHT or CFS_HANDLING line, so the pool holds each
	// charge once. (RATE rates the planned scope, which has no volume for A and B; the 3 m3 loaded are not seen by it.)
	_, err = finModule.RateConsolCostPool(ctx, finapi.RateConsolCostPool{CommandID: "fin-rate-pool", TenantID: tenant, ActorID: "fin-1", PoolID: "P-1",
		ExpectedRevision: pool().Revision, Carrier: "line-1", Origin: "CNSHA", Destination: "DEHAM", ServiceLevel: "STANDARD", Method: finapi.MethodChargeable,
		OccurredAt: hours(27)})
	must("RateConsolCostPool of the own-container pool", err)
	rated := pool().Lines
	if len(rated) != 4 {
		t.Fatalf("stage 5: the pool's lines: %+v", rated)
	}
	if l := rated[3]; l.Source != finapi.SourceRated || l.ChargeCode != "THC" || l.Method != finapi.MethodChargeable || len(l.Versions) != 1 || l.Versions[0].Amount != usd(2_000) ||
		l.Versions[0].Local != cny(14_247) || l.Versions[0].CreditorPartyID != "line-1" || l.Versions[0].Status != finapi.StatusAccrued {
		t.Fatalf("stage 5: the rated line: %+v", l)
	}
	for i, l := range rated {
		for _, o := range rated[i+1:] {
			if l.ChargeCode == o.ChargeCode {
				t.Fatalf("stage 5: charge %s is in the pool twice: %+v", l.ChargeCode, rated)
			}
		}
	}
	// the rated line is pooled and not yet apportioned: 804,944 + 14,247 = 819,191
	checkConsol("stage 5: the consolidation's profit with the rated line unapportioned", finapi.PoolOpen, 819_191, 804_944,
		[3]int64{210_664, 78_308, 515_972}, [3]int64{216_740, 99_777, -17_334})
	// apportioned by CHARGEABLE on the loaded measures, W/M 800 : 288 : 2000 of 3088. THC USD 20.00 (2,000) divides 518.13 /
	// 186.53 / 1,295.34: floors 518 / 186 / 1,295 sum to 1,999 and the unit goes to the largest remainder (SH-B, 1632 of
	// 3088): 518 / 187 / 1,295. Its CNY 142.47 (14,247) divides 3,690.93 / 1,328.74 / 9,227.33: floors 3,690 / 1,328 / 9,227
	// sum to 14,245 and the two units go to SH-A (2880) and SH-B (2272): 3,691 / 1,329 / 9,227.
	wantRatedTHC := ownPoolLine{4, finapi.MethodChargeable, "MWM", [3]int64{800, 288, 2000}, [3]int64{518, 187, 1_295}, [3]int64{3_691, 1_329, 9_227}}
	apportion("fin-apportion-rated", 28, 4)
	checkShares("stage 5: the rated THC", finapi.BasisActual, wantRatedTHC)
	// each house's pool cost now adds its share of the rated line: 210,664 + 3,691 = 214,355; 78,308 + 1,329 = 79,637;
	// 515,972 + 9,227 = 525,199 (together the pool's 819,191), and the profits are 427,404 - 214,355 = 213,049; 178,085 -
	// 79,637 = 98,448; 498,638 - 525,199 = -26,561, together 284,936 = 1,104,127 - 819,191: a small profit for the
	// consolidation, with SH-C, the heaviest, bearing more than it sells for
	finalPool, finalProfit := [3]int64{214_355, 79_637, 525_199}, [3]int64{213_049, 98_448, -26_561}
	checkJobs("stage 5", finalPool, finalProfit, 4)
	checkConsol("stage 5: the consolidation's profit with every line apportioned", finapi.PoolOpen, 819_191, 819_191, finalPool, finalProfit)

	// Item 6: the line's invoice for the four lines (USD 1,000.00 + 100.00 + 30.00 + 20.00) is matched final at the accounting
	// rate, which equals the estimate rate, so without variance. Every line is apportioned again at its final cost on PHY's
	// current basis, the pool closes, and the consolidation's profit is the same.
	_, err = finModule.RecordCreditorInvoice(ctx, finapi.RecordCreditorInvoice{CommandID: "fin-invoice", TenantID: tenant, ActorID: "ap-1", InvoiceID: "CI-LINE-1",
		LegalEntityID: "LE", CreditorPartyID: "line-1", SupplierInvoiceNo: "LINE-INV-2611-1", InvoiceDate: hours(29).Format(time.DateOnly), Currency: "USD",
		Lines: []finapi.CreditorInvoiceLine{{ChargeCode: "OCEAN_FREIGHT", Amount: 100_000}, {ChargeCode: "CFS_HANDLING", Amount: 10_000}, {ChargeCode: "LASHING", Amount: 3_000},
			{ChargeCode: "THC", Amount: 2_000}}, OccurredAt: hours(29)})
	must("record the line's invoice", err)
	invoice, err := finModule.GetCreditorInvoice(ctx, finapi.GetCreditorInvoice{TenantID: tenant, InvoiceID: "CI-LINE-1"})
	must("the line's invoice", err)
	matches := make([]finapi.CreditorMatch, 4)
	for i, amount := range []int64{100_000, 10_000, 3_000, 2_000} {
		matches[i] = finapi.CreditorMatch{LineNo: i + 1, Target: finapi.AccrualRef{Kind: "POOL_COST", ID: "P-1", LineNo: i + 1}, Amount: amount, Final: true}
	}
	_, err = finModule.MatchCreditorInvoice(ctx, finapi.MatchCreditorInvoice{CommandID: "fin-match", TenantID: tenant, ActorID: "ap-1", InvoiceID: "CI-LINE-1",
		ExpectedRevision: invoice.Revision, Matches: matches, OccurredAt: hours(30)})
	must("match the line's invoice to the pool", err)
	apportion("fin-apportion-final", 31)
	for i, w := range []ownPoolLine{wantFreight, wantCFS, wantLashing, wantRatedTHC} {
		checkShares("stage 6: line "+string(rune('1'+i))+" at its final cost", finapi.BasisActual, w)
	}
	_, err = finModule.CloseConsolCostPool(ctx, finapi.CloseConsolCostPool{CommandID: "fin-close", TenantID: tenant, ActorID: "fin-1", PoolID: "P-1",
		ExpectedRevision: pool().Revision, OccurredAt: hours(32)})
	must("close the pool", err)
	if p := pool(); p.State != finapi.PoolClosed || len(p.Closures) != 1 {
		t.Fatalf("stage 6: the closed pool: %+v", p)
	}
	closed, err := finModule.GetConsolProfit(ctx, finapi.GetConsolProfit{TenantID: tenant, ConsolidationID: "C-1"})
	must("the closed consolidation's profit", err)
	if closed.Matched != cny(819_191) {
		t.Fatalf("stage 6: matched cost of the closed pool: %+v", closed)
	}
	checkConsol("stage 6: the closed consolidation's profit", finapi.PoolClosed, 819_191, 819_191, finalPool, finalProfit)
	checkJobs("stage 6", finalPool, finalProfit, 4)
}

// ownPoolLine is what one line's shares are expected to be: the line, its method and the unit of the quantities, each
// house's quantity, and each house's share in USD (the line's currency) and in CNY (the functional currency).
type ownPoolLine struct {
	line               int
	method, unit       string
	quantity, usd, cny [3]int64
}

// planSeaSellOnly plans one SEA shipment SH-<suffix> as planSea does (6 cartons of 300 kg, LCL, from Shanghai to Hamburg),
// but its customer's one-off quote has an OCEAN_FREIGHT sell line of the amount given (USD minor units) and no cost
// side: the cost of the freight is the consolidation's, carried by the pool.
func planSeaSellOnly(t *testing.T, s slice, party, suffix string, sell int64) {
	t.Helper()
	ctx, tenant, at, departs := context.Background(), s.tenant, s.at, s.departs
	unlocode := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	weight := kernel.Quantity{Unit: "KGM", Amount: 300}
	steps := []func() error{
		func() error {
			_, err := s.org.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "role-" + suffix, TenantID: tenant, ActorID: "org-officer",
				LegalEntityID: "LE", PartyID: party, ExpectedRevision: 1, RoleCode: "CUSTOMER", ValidFrom: at, EvidenceRef: "contract", OccurredAt: at})
			return err
		},
		func() error {
			_, err := s.rate.CreateQuote(ctx, rateapi.CreateQuote{CommandID: "quote-" + suffix, TenantID: tenant, ActorID: "sales-1",
				QuoteID: "Q-" + suffix, LegalEntityID: "LE", CustomerPartyID: party, Currency: "USD",
				Lines:    []rateapi.QuoteLine{{ChargeCode: "OCEAN_FREIGHT", Sell: usd(sell), Cost: usd(0)}},
				Validity: kernel.ValidityInterval{From: at, Until: at.Add(14 * 24 * time.Hour)}, OccurredAt: at.Add(time.Minute)})
			return err
		},
		func() error {
			_, err := s.rate.AcceptQuote(ctx, rateapi.AcceptQuote{CommandID: "accept-" + suffix, TenantID: tenant, ActorID: "sales-1",
				QuoteID: "Q-" + suffix, ExpectedRevision: 1, EvidenceKind: rateapi.EvidenceEmail, EvidenceRef: "mail", OccurredAt: at.Add(time.Hour)})
			return err
		},
		func() error {
			_, err := s.com.RecordCustomerOrder(ctx, comapi.RecordCustomerOrder{CommandID: "order-" + suffix, TenantID: tenant, ActorID: "cs-1",
				OrderID: "O-" + suffix, QuoteID: "Q-" + suffix, CustomerPartyID: party, OccurredAt: at.Add(2 * time.Hour),
				Transport: comapi.Transport{Mode: comapi.ModeSea, ContainerMode: comapi.ContainerLCL},
				Lines:     []comapi.OrderLine{{Description: "Cartons of furniture parts", Packs: 6, PackageType: "CT", GrossWeight: weight}}})
			return err
		},
		func() error {
			_, err := s.com.AllocateOrderToObligation(ctx, comapi.AllocateOrderToObligation{CommandID: "allocate-" + suffix, TenantID: tenant,
				ActorID: "planner-1", OrderID: "O-" + suffix, ExpectedRevision: 1, LineNo: 1, ObligationID: "OB-" + suffix, Packs: 6,
				GrossWeight: weight, OccurredAt: at.Add(3 * time.Hour)})
			return err
		},
		func() error {
			_, err := s.com.ReserveObligationForFulfillment(ctx, comapi.ReserveObligationForFulfillment{CommandID: "reserve-" + suffix,
				TenantID: tenant, ActorID: "fulfilment", ObligationID: "OB-" + suffix, ObligationVersion: 1, ShipmentID: "SH-" + suffix,
				OccurredAt: at.Add(4 * time.Hour)})
			return err
		},
		func() error {
			_, err := s.ful.CreateShipmentFromObligation(ctx, fulapi.CreateShipmentFromObligation{CommandID: "create-" + suffix, TenantID: tenant,
				ActorID: "planner-1", ShipmentID: "SH-" + suffix, ObligationID: "OB-" + suffix, ObligationVersion: 1, OccurredAt: at.Add(5 * time.Hour)})
			return err
		},
		func() error {
			_, err := s.ful.AmendShipmentPlan(ctx, fulapi.AmendShipmentPlan{CommandID: "plan-" + suffix, TenantID: tenant, ActorID: "planner-1",
				ShipmentID: "SH-" + suffix, ExpectedRevision: 1, TransportMode: fulapi.ModeSea, ContainerMode: fulapi.ContainerLCL, Origin: unlocode("CNSHA"),
				Destination: unlocode("DEHAM"), DepartureWindow: kernel.ValidityInterval{From: departs, Until: departs.Add(48 * time.Hour)},
				Reason: "vessel booked", OccurredAt: at.Add(6 * time.Hour)})
			return err
		},
		func() error {
			_, err := s.ful.ConfirmShipmentPlan(ctx, fulapi.ConfirmShipmentPlan{CommandID: "confirm-" + suffix, TenantID: tenant,
				ActorID: "planner-2", ShipmentID: "SH-" + suffix, ExpectedRevision: 2, OccurredAt: at.Add(7 * time.Hour)})
			return err
		},
	}
	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("planSeaSellOnly %s, step %d: %v", suffix, i, err)
		}
	}
}

// An own-container consolidation with two planned containers, with the real ORG, RATE, COM, FUL, PHY and FIN: SH-A is
// stuffed wholly into the first container (6 packs), SH-B wholly into the second (6 packs) and SH-C is split over both (4
// packs in the first, 2 in the second). PHY's cost basis gives the split house's packs, weight and volume as the sums over
// the containers, reports both load units, and FIN's CHARGEABLE apportionment at ACTUAL is the one of three houses.
//
// What this shows: a house split over containers is one house for the pool, counted by its summed measures, and its two
// stuffings are neither counted twice nor lost. What it does not show: any cost per container. A cost line belongs to the
// consolidation, not to a container, and the existing methods apportion a line over all its houses, so a container's
// freight is spread over the houses of both containers; FIN does not read the load units PHY reports. Putting a
// container's cost on the houses in it takes MANUAL, a line at a time.
func TestTwoOwnContainersAndASplitHouseAreCostedWithPHYAndFIN(t *testing.T) {
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
	fulModule, phyModule, err := platform.FULAndPHY(comModule, allowAll{}, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	finModule, err := platform.FIN(orgModule, rateModule, fulModule, &DocumentSnapshots{}, phyModule, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	tenant := uniqueTenant("cargo-ocean-two-")
	seedORG(t, platform, tenant, "party-a", "party-b", "party-c", "line-1")
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	departs := at.Add(72 * time.Hour)
	hours := func(h int) time.Time { return at.Add(time.Duration(h) * time.Hour) }
	kg := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }
	cmq := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "CMQ", Amount: n} }
	unlocode := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	phyOK := func(_ phyapi.Receipt, err error) { t.Helper(); must("PHY", err) }
	evidence := func(source, ref string, h int) phyapi.Evidence {
		return phyapi.Evidence{Source: source, Reference: ref, OccurredAt: hours(h)}
	}
	_, err = orgModule.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "role-line-1", TenantID: tenant, ActorID: "org-officer", LegalEntityID: "LE",
		PartyID: "line-1", ExpectedRevision: 1, RoleCode: orgapi.RoleCarrier, ValidFrom: at.Add(-time.Hour), EvidenceRef: "service contract", OccurredAt: at})
	must("role line-1", err)
	contexts := slice{org: orgModule, rate: rateModule, com: comModule, ful: fulModule, tenant: tenant, at: at, departs: departs}
	for _, s := range []string{"A", "B", "C"} {
		planSeaSellOnly(t, contexts, "party-"+strings.ToLower(s), s, 60_000)
	}
	phyOK(phyModule.CreateConsolidation(ctx, phyapi.CreateConsolidation{CommandID: "consol", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		Mode: phyapi.ModeSea, Kind: phyapi.KindSeaLCLOwn, OperatingLegalEntityID: "LE", Origin: unlocode("CNSHA"), Destination: unlocode("DEHAM"),
		PlannedDeparture: departs.Add(6 * time.Hour), HouseCutOff: departs.Add(-6 * time.Hour), OccurredAt: hours(8)}))
	for i := range 2 {
		phyOK(phyModule.PlanLoadUnit(ctx, phyapi.PlanLoadUnit{CommandID: "plan-unit-" + string(rune('1'+i)), TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
			ExpectedRevision: int64(1 + i), Kind: phyapi.LoadUnitContainer, TypeCode: "22G1", OccurredAt: hours(8)}))
	}
	for _, s := range []string{"SH-A", "SH-B", "SH-C"} {
		phyOK(phyModule.AttachHouse(ctx, phyapi.AttachHouse{CommandID: "attach-" + s, TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1", ShipmentID: s,
			OccurredAt: hours(9)}))
		record(t, ctx, fulModule, tenant, "ful-attach-"+s, s, "C-1", 3, hours(9))
	}
	_, err = finModule.OpenFinancePolicy(ctx, finapi.OpenFinancePolicy{CommandID: "fin-policy", TenantID: tenant, ActorID: "acct-1", LegalEntityID: "LE",
		FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: hours(10)})
	must("finance policy", err)
	_, err = finModule.RecordExchangeRate(ctx, finapi.RecordExchangeRate{CommandID: "fin-rate", TenantID: tenant, ActorID: "acct-1", LegalEntityID: "LE",
		ExpectedRevision: 1, RateType: finapi.RateEstimate, From: "USD", Rate: 71_234, Scale: 4, FromDigits: 2, ToDigits: 2, ValidFrom: at,
		ValidUntil: at.AddDate(0, 1, 0), Source: "CFETS central parity", OccurredAt: hours(10)})
	must("estimate rate", err)
	for _, s := range []string{"A", "B", "C"} {
		_, err = finModule.OpenFreightJob(ctx, finapi.OpenFreightJob{CommandID: "fin-job-" + s, TenantID: tenant, ActorID: "fin-1", JobID: "JOB-" + s, ShipmentID: "SH-" + s,
			OccurredAt: hours(10)})
		must("open JOB-"+s, err)
	}
	_, err = finModule.OpenConsolCostPool(ctx, finapi.OpenConsolCostPool{CommandID: "fin-pool", TenantID: tenant, ActorID: "fin-1", PoolID: "P-1", ConsolidationID: "C-1",
		OccurredAt: hours(12)})
	must("open the pool", err)
	_, err = finModule.RecordConsolCost(ctx, finapi.RecordConsolCost{CommandID: "fin-cost", TenantID: tenant, ActorID: "fin-1", PoolID: "P-1", ExpectedRevision: 1,
		ChargeCode: "OCEAN_FREIGHT", Method: finapi.MethodChargeable, Cost: finapi.ChargeSide{PartyID: "line-1", Amount: usd(100_000)}, Reason: "the line's quote for two containers",
		OccurredAt: hours(12)})
	must("cost line", err)

	// both lots weighed at the CFS: A 312 kg and 0.8 m3, B 288 kg and 0.2 m3, C 800 kg and 2 m3, 6 packs each
	c, err := phyModule.GetConsolidation(ctx, phyapi.GetConsolidation{TenantID: tenant, ConsolidationID: "C-1"})
	must("consolidation", err)
	phyOK(phyModule.CloseConsolidation(ctx, phyapi.CloseConsolidation{CommandID: "close", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ExpectedRevision: c.Revision, OccurredAt: hours(13)}))
	cfs := phyapi.Facility{Location: c.Current.Origin, Code: "SHA-CFS1"}
	weights := map[string]int64{"A": 312, "B": 288, "C": 800}
	volumes := map[string]int64{"A": 800_000, "B": 200_000, "C": 2_000_000}
	for _, s := range []string{"A", "B", "C"} {
		lot := "L-" + s
		phyOK(phyModule.ReceiveCargo(ctx, phyapi.ReceiveCargo{CommandID: "rcv-" + s, TenantID: tenant, ActorID: "cfs-1", LotID: lot, ShipmentID: "SH-" + s,
			Facility: cfs, Packs: 6, PackageType: "CT", Condition: phyapi.ConditionGood, Evidence: evidence("CFS-WMS", "GRN-"+s, 14), OccurredAt: hours(14)}))
		phyOK(phyModule.RecordCargoMeasurement(ctx, phyapi.RecordCargoMeasurement{CommandID: "weigh-" + s, TenantID: tenant, ActorID: "cfs-1", LotID: lot,
			ExpectedRevision: 1, GrossWeight: kg(weights[s]), Volume: cmq(volumes[s]), Method: phyapi.MethodScale, Evidence: evidence("CFS-SCALE", "W-"+s, 14), OccurredAt: hours(14)}))
		phyOK(phyModule.CompleteReceipt(ctx, phyapi.CompleteReceipt{CommandID: "complete-" + s, TenantID: tenant, ActorID: "cfs-1", LotID: lot,
			ExpectedRevision: 2, OccurredAt: hours(14)}))
	}
	// two stuffings, one per container: X-1 takes all of A and 4 packs of C, X-2 all of B and the other 2 packs of C
	revision := map[string]int64{}
	for i, x := range []string{"X-1", "X-2"} {
		phyOK(phyModule.OpenLoadExecution(ctx, phyapi.OpenLoadExecution{CommandID: "stuff-" + x, TenantID: tenant, ActorID: "cfs-1", ExecutionID: x,
			ConsolidationID: "C-1", Kind: phyapi.ExecutionStuffing, Facility: cfs, LoadUnitNo: i + 1, OccurredAt: hours(15)}))
		phyOK(phyModule.StartLoading(ctx, phyapi.StartLoading{CommandID: "start-" + x, TenantID: tenant, ActorID: "cfs-1", ExecutionID: x, ExpectedRevision: 1, OccurredAt: hours(15)}))
		revision[x] = 2
	}
	basis := func() phyapi.PhysicalCostBasis {
		t.Helper()
		b, err := phyModule.GetPhysicalCostBasis(ctx, phyapi.GetPhysicalCostBasis{TenantID: tenant, ConsolidationID: "C-1"})
		must("PHY's cost basis", err)
		return b
	}
	for _, l := range []struct {
		x, shipment string
		packs       int64
	}{{"X-1", "A", 6}, {"X-1", "C", 4}, {"X-2", "B", 6}, {"X-2", "C", 2}} {
		phyOK(phyModule.RecordCargoLoaded(ctx, phyapi.RecordCargoLoaded{CommandID: "load-" + l.x + "-" + l.shipment, TenantID: tenant, ActorID: "cfs-1", ExecutionID: l.x,
			ExpectedRevision: revision[l.x], LotID: "L-" + l.shipment, Packs: l.packs, Evidence: evidence("CFS-TALLY", "T-"+l.x+l.shipment, 15), OccurredAt: hours(15)}))
		revision[l.x]++
	}
	// both containers are in use until sealed, then closed
	for i, u := range basis().LoadUnits {
		if u.UnitNo != i+1 || u.Kind != phyapi.LoadUnitContainer || u.TypeCode != "22G1" || u.Status != phyapi.UnitStatusInUse || u.ExecutionID != []string{"X-1", "X-2"}[i] {
			t.Fatalf("load unit %d before the seals: %+v", i+1, u)
		}
	}
	for _, x := range []string{"X-1", "X-2"} {
		phyOK(phyModule.CloseLoadExecution(ctx, phyapi.CloseLoadExecution{CommandID: "seal-" + x, TenantID: tenant, ActorID: "cfs-1", ExecutionID: x,
			ExpectedRevision: revision[x], Evidence: evidence(phyapi.EvidenceSeal, "SEAL-"+x, 15), OccurredAt: hours(15)}))
	}
	b := basis()
	if len(b.LoadUnits) != 2 || b.LoadUnits[0].UnitNo != 1 || b.LoadUnits[1].UnitNo != 2 || b.LoadUnits[0].Status != phyapi.UnitStatusClosed || b.LoadUnits[1].Status != phyapi.UnitStatusClosed {
		t.Fatalf("the load units after the seals: %+v", b.LoadUnits)
	}
	// PHY's cost basis per house: SH-C's loaded packs, weight and volume are the sums over both containers (4 + 2 packs of
	// its one lot of 800 kg and 2 m3). Each stuffing line takes its share of the lot's measure rounded down: 4/6 of 800 kg is
	// 533 and 2/6 is 266, 4/6 of 2,000,000 cm3 is 1,333,333 and 2/6 is 666,666, so the sums are 799 kg and 1,999,999 cm3, one
	// unit short of the lot's (the stand-in numbers of the other tests are whole). Here it changes nothing: W/M is still 2000.
	for i, want := range []struct {
		packs, kilos, cubicCm int64
	}{{6, 312, 800_000}, {6, 288, 200_000}, {6, 799, 1_999_999}} {
		h := b.Houses[i]
		if h.ShipmentID != "SH-"+string(rune('A'+i)) || h.Packs != 6 || h.LoadedPacks != want.packs || h.LoadedGrossWeight != kg(want.kilos) || h.LoadedVolume != cmq(want.cubicCm) {
			t.Fatalf("PHY's cost basis, house %d: %+v", i, h)
		}
	}
	// FIN apportions the freight at ACTUAL on those measures: W/M 800 : 288 : 2000 of 3088, USD 100,000 -> 25,907 / 9,326 /
	// 64,767 and CNY 712,340 -> 184,544 / 66,436 / 461,360 (the hand calculation is in the first test), the same as if the
	// three houses had been stuffed into one container
	_, err = finModule.ApportionConsolCosts(ctx, finapi.ApportionConsolCosts{CommandID: "fin-apportion", TenantID: tenant, ActorID: "fin-1", PoolID: "P-1",
		ExpectedRevision: 2, OccurredAt: hours(17)})
	must("apportion", err)
	pool, err := finModule.GetConsolCostPool(ctx, finapi.GetConsolCostPool{TenantID: tenant, PoolID: "P-1"})
	must("the pool", err)
	if len(pool.Apportionments) != 1 || pool.Apportionments[0].Stage != finapi.BasisActual {
		t.Fatalf("the apportionment: %+v", pool.Apportionments)
	}
	shares := pool.Apportionments[0].Lines[0].Shares
	for i, want := range []struct{ quantity, usd, cny int64 }{{800, 25_907, 184_544}, {288, 9_326, 66_436}, {2000, 64_767, 461_360}} {
		if len(shares) != 3 || shares[i].Quantity != want.quantity || shares[i].Unit != "MWM" || shares[i].Amount != usd(want.usd) ||
			shares[i].Local != (kernel.Money{Currency: "CNY", Minor: want.cny}) {
			t.Fatalf("share %d: %+v, want %+v", i+1, shares, want)
		}
	}
}

// A house received at the CFS but never stuffed into the sealed own container bears no cost of the container and no
// longer holds up the pool, with the real ORG, RATE, COM, FUL, PHY and FIN (V2.78; BOOK, SEA and DOC are not needed:
// nothing here books, moves or bills the container). Three LCL orders are consolidated in one own container, A and B are
// stuffed and the container sealed, C is received and weighed (800 kg, 2 m3) but never stuffed (the case the V2.77 review
// ran). FIN's pool carries the container's ocean freight, CNY 1,000.00 (100,000 minor), by CHARGEABLE (W/M):
//  1. PHY's cost basis shows C with no loaded packs;
//  2. the freight apportioned over all three, as before, is stage PLANNED and gives C a share, 300 : 300 : 300 milli
//     freight tonnes of the planned scope (6 packs and 300 kg each): 100,000 / 3 is 33,333.33 each, the unit left goes to
//     the first house, so 33,334 / 33,333 / 33,333;
//  3. with the creditor's invoice matched, the pool cannot be closed (FIN_POOL_NOT_CLOSABLE);
//  4. the same line apportioned again with C in NotUsed is stage ACTUAL on the loaded measures of A and B: W/M of A 312 kg
//     under 0.8 m3 is 800, of B 288 kg over 0.2 m3 is 288, 1088 in all; 100,000 divides 73,529.41 / 26,470.59, the floors
//     73,529 and 26,470 leave one unit, to the larger remainder (B): 73,529 / 26,471. C's share is not used and zero;
//  5. C's job carries no pool cost and A's and B's carry their shares;
//  6. the pool closes, and GetConsolProfit answers with all three houses, C bearing nothing: A sells USD 600.00 (CNY
//     4,274.04), B USD 250.00 (CNY 1,780.85), so their profits are 427,404 - 73,529 = 353,875 and 178,085 - 26,471 = 151,614,
//     and C's job, which sells USD 400.00 (CNY 2,849.36), is listed too with no pool cost and a profit of its revenue.
//
// This solves FIN's side of the V2.77 limit: FIN no longer charges the house left behind and the pool can close. It does
// not solve the house itself: C is still attached to the consolidation in PHY, which can neither ship it nor release it
// while the container is sealed, and that is PHY's to solve. FIN does not know which houses are in which container, so
// the person apportioning names the house to leave out.
func TestAHouseLeftOutOfTheContainerBearsNoCostWithEveryContext(t *testing.T) {
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
	fulModule, phyModule, err := platform.FULAndPHY(comModule, allowAll{}, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	finModule, err := platform.FIN(orgModule, rateModule, fulModule, &DocumentSnapshots{}, phyModule, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	tenant := uniqueTenant("cargo-left-out-")
	seedORG(t, platform, tenant, "party-a", "party-b", "party-c", "line-1")
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	departs := at.Add(72 * time.Hour)
	hours := func(h int) time.Time { return at.Add(time.Duration(h) * time.Hour) }
	kg := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }
	cmq := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "CMQ", Amount: n} }
	cny := func(n int64) kernel.Money { return kernel.Money{Currency: "CNY", Minor: n} }
	unlocode := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	phyOK := func(_ phyapi.Receipt, err error) { t.Helper(); must("PHY", err) }
	evidence := func(source, ref string, h int) phyapi.Evidence {
		return phyapi.Evidence{Source: source, Reference: ref, OccurredAt: hours(h)}
	}
	_, err = orgModule.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "role-line-1", TenantID: tenant, ActorID: "org-officer", LegalEntityID: "LE",
		PartyID: "line-1", ExpectedRevision: 1, RoleCode: orgapi.RoleCarrier, ValidFrom: at.Add(-time.Hour), EvidenceRef: "service contract", OccurredAt: at})
	must("role line-1", err)

	// three orders by sea, one shipment each (6 cartons of 300 kg, no volume declared), houses of the own container C-1
	contexts := slice{org: orgModule, rate: rateModule, com: comModule, ful: fulModule, tenant: tenant, at: at, departs: departs}
	planSeaSellOnly(t, contexts, "party-a", "A", 60_000)
	planSeaSellOnly(t, contexts, "party-b", "B", 25_000)
	planSeaSellOnly(t, contexts, "party-c", "C", 40_000)
	phyOK(phyModule.CreateConsolidation(ctx, phyapi.CreateConsolidation{CommandID: "consol", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		Mode: phyapi.ModeSea, Kind: phyapi.KindSeaLCLOwn, OperatingLegalEntityID: "LE", Origin: unlocode("CNSHA"), Destination: unlocode("DEHAM"),
		PlannedDeparture: departs.Add(6 * time.Hour), HouseCutOff: departs.Add(-6 * time.Hour), OccurredAt: hours(8)}))
	phyOK(phyModule.PlanLoadUnit(ctx, phyapi.PlanLoadUnit{CommandID: "plan-unit", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ExpectedRevision: 1, Kind: phyapi.LoadUnitContainer, TypeCode: "22G1", OccurredAt: hours(8)}))
	for _, s := range []string{"SH-A", "SH-B", "SH-C"} {
		phyOK(phyModule.AttachHouse(ctx, phyapi.AttachHouse{CommandID: "attach-" + s, TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1", ShipmentID: s,
			OccurredAt: hours(9)}))
		record(t, ctx, fulModule, tenant, "ful-attach-"+s, s, "C-1", 3, hours(9))
	}

	// FIN: the policy with its ESTIMATE rate (the orders are sold in USD), the three jobs priced from their orders' quotes,
	// and the pool of the consolidation with the container's freight
	_, err = finModule.OpenFinancePolicy(ctx, finapi.OpenFinancePolicy{CommandID: "fin-policy", TenantID: tenant, ActorID: "acct-1", LegalEntityID: "LE",
		FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: hours(11)})
	must("finance policy", err)
	_, err = finModule.RecordExchangeRate(ctx, finapi.RecordExchangeRate{CommandID: "fin-rate-estimate", TenantID: tenant, ActorID: "acct-1", LegalEntityID: "LE",
		ExpectedRevision: 1, RateType: finapi.RateEstimate, From: "USD", Rate: 71_234, Scale: 4, FromDigits: 2, ToDigits: 2, ValidFrom: at, ValidUntil: at.AddDate(0, 1, 0),
		Source: "CFETS central parity", OccurredAt: hours(11)})
	must("estimate rate", err)
	houses := []struct{ shipment, job string }{{"SH-A", "JOB-A"}, {"SH-B", "JOB-B"}, {"SH-C", "JOB-C"}}
	for _, s := range []string{"A", "B", "C"} {
		job := "JOB-" + s
		r, err := finModule.OpenFreightJob(ctx, finapi.OpenFreightJob{CommandID: "fin-job-" + s, TenantID: tenant, ActorID: "fin-1", JobID: job, ShipmentID: "SH-" + s,
			OccurredAt: hours(11)})
		must("open "+job, err)
		_, err = finModule.ApplyPricingBasis(ctx, finapi.ApplyPricingBasis{CommandID: "fin-quote-" + s, TenantID: tenant, ActorID: "fin-1", JobID: job,
			ExpectedRevision: r.NewRevision, QuoteID: "Q-" + s, OrderID: "O-" + s, OccurredAt: hours(11)})
		must("the pricing basis of "+job, err)
	}
	_, err = finModule.OpenConsolCostPool(ctx, finapi.OpenConsolCostPool{CommandID: "fin-pool", TenantID: tenant, ActorID: "fin-1", PoolID: "P-1",
		ConsolidationID: "C-1", OccurredAt: hours(12)})
	must("open the pool", err)
	pool := func() finapi.ConsolCostPool {
		t.Helper()
		p, err := finModule.GetConsolCostPool(ctx, finapi.GetConsolCostPool{TenantID: tenant, PoolID: "P-1"})
		must("the pool", err)
		return p
	}
	_, err = finModule.RecordConsolCost(ctx, finapi.RecordConsolCost{CommandID: "fin-cost-freight", TenantID: tenant, ActorID: "fin-1", PoolID: "P-1",
		ExpectedRevision: pool().Revision, ChargeCode: "OCEAN_FREIGHT", SupplierRef: "LINE-BK-1", Method: finapi.MethodChargeable,
		Cost: finapi.ChargeSide{PartyID: "line-1", Amount: cny(100_000)}, Reason: "line's quote for the container", OccurredAt: hours(12)})
	must("the freight line", err)

	// PHY: the consolidation closed to houses, all three lots received and weighed at the CFS, A and B stuffed and the
	// container sealed; C is never stuffed
	c, err := phyModule.GetConsolidation(ctx, phyapi.GetConsolidation{TenantID: tenant, ConsolidationID: "C-1"})
	must("consolidation", err)
	phyOK(phyModule.CloseConsolidation(ctx, phyapi.CloseConsolidation{CommandID: "close", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ExpectedRevision: c.Revision, OccurredAt: hours(13)}))
	cfs := phyapi.Facility{Location: c.Current.Origin, Code: "SHA-CFS1"}
	weights := map[string]int64{"A": 312, "B": 288, "C": 800}
	volumes := map[string]int64{"A": 800_000, "B": 200_000, "C": 2_000_000}
	for _, s := range []string{"A", "B", "C"} {
		lot := "L-" + s
		phyOK(phyModule.ReceiveCargo(ctx, phyapi.ReceiveCargo{CommandID: "rcv-" + s, TenantID: tenant, ActorID: "cfs-1", LotID: lot, ShipmentID: "SH-" + s,
			Facility: cfs, Packs: 6, PackageType: "CT", Condition: phyapi.ConditionGood, Evidence: evidence("CFS-WMS", "GRN-"+s, 14), OccurredAt: hours(14)}))
		phyOK(phyModule.RecordCargoMeasurement(ctx, phyapi.RecordCargoMeasurement{CommandID: "weigh-" + s, TenantID: tenant, ActorID: "cfs-1", LotID: lot,
			ExpectedRevision: 1, GrossWeight: kg(weights[s]), Volume: cmq(volumes[s]), Method: phyapi.MethodScale, Evidence: evidence("CFS-SCALE", "W-"+s, 14),
			OccurredAt: hours(14)}))
		phyOK(phyModule.CompleteReceipt(ctx, phyapi.CompleteReceipt{CommandID: "complete-" + s, TenantID: tenant, ActorID: "cfs-1", LotID: lot, ExpectedRevision: 2,
			OccurredAt: hours(14)}))
	}
	phyOK(phyModule.OpenLoadExecution(ctx, phyapi.OpenLoadExecution{CommandID: "stuff", TenantID: tenant, ActorID: "cfs-1", ExecutionID: "X-1",
		ConsolidationID: "C-1", Kind: phyapi.ExecutionStuffing, Facility: cfs, LoadUnitNo: 1, OccurredAt: hours(15)}))
	phyOK(phyModule.StartLoading(ctx, phyapi.StartLoading{CommandID: "start", TenantID: tenant, ActorID: "cfs-1", ExecutionID: "X-1", ExpectedRevision: 1,
		OccurredAt: hours(15)}))
	for i, s := range []string{"A", "B"} {
		phyOK(phyModule.RecordCargoLoaded(ctx, phyapi.RecordCargoLoaded{CommandID: "load-" + s, TenantID: tenant, ActorID: "cfs-1", ExecutionID: "X-1",
			ExpectedRevision: int64(2 + i), LotID: "L-" + s, Packs: 6, Evidence: evidence("CFS-TALLY", "T-"+s, 15), OccurredAt: hours(15)}))
	}
	phyOK(phyModule.CloseLoadExecution(ctx, phyapi.CloseLoadExecution{CommandID: "seal", TenantID: tenant, ActorID: "cfs-1", ExecutionID: "X-1",
		ExpectedRevision: 4, Evidence: evidence(phyapi.EvidenceSeal, "CN-SEAL-77120", 15), OccurredAt: hours(15)}))

	// 1. PHY's cost basis: A and B loaded with their measures, C with no loaded packs, and still a live house
	b, err := phyModule.GetPhysicalCostBasis(ctx, phyapi.GetPhysicalCostBasis{TenantID: tenant, ConsolidationID: "C-1"})
	must("PHY's cost basis", err)
	if b.Kind != phyapi.KindSeaLCLOwn || len(b.Houses) != 3 {
		t.Fatalf("PHY's cost basis: %+v", b)
	}
	for i, want := range []struct{ packs, kilos, cubicCm int64 }{{6, 312, 800_000}, {6, 288, 200_000}, {0, 0, 0}} {
		h := b.Houses[i]
		if h.ShipmentID != houses[i].shipment || h.Packs != 6 || h.GrossWeight != kg(300) || h.LoadedPacks != want.packs ||
			(want.packs > 0 && (h.LoadedGrossWeight != kg(want.kilos) || h.LoadedVolume != cmq(want.cubicCm))) {
			t.Fatalf("PHY's cost basis, house %d: %+v", i, h)
		}
	}
	latest := func() finapi.PoolApportionment {
		t.Helper()
		p := pool()
		return p.Apportionments[len(p.Apportionments)-1]
	}
	share := func(no int, quantity, amount int64) finapi.PoolShare {
		return finapi.PoolShare{ShareNo: no, ShipmentID: houses[no-1].shipment, JobID: houses[no-1].job, Quantity: quantity, Unit: "MWM", Amount: cny(amount), Local: cny(amount)}
	}
	closePool := func(id string, h int) error {
		_, err := finModule.CloseConsolCostPool(ctx, finapi.CloseConsolCostPool{CommandID: id, TenantID: tenant, ActorID: "fin-1", PoolID: "P-1",
			ExpectedRevision: pool().Revision, OccurredAt: hours(h)})
		return err
	}

	// 2. the freight over all three houses: PLANNED, and C has a share
	_, err = finModule.ApportionConsolCosts(ctx, finapi.ApportionConsolCosts{CommandID: "fin-apportion-all", TenantID: tenant, ActorID: "fin-1", PoolID: "P-1",
		ExpectedRevision: pool().Revision, OccurredAt: hours(16)})
	must("apportion over all three houses", err)
	all := latest()
	if all.Stage != finapi.BasisPlanned || len(all.Lines) != 1 || all.Lines[0].Method != finapi.MethodChargeable || len(all.Lines[0].Shares) != 3 ||
		all.Lines[0].Shares[0] != share(1, 300, 33_334) || all.Lines[0].Shares[1] != share(2, 300, 33_333) || all.Lines[0].Shares[2] != share(3, 300, 33_333) {
		t.Fatalf("the freight over all three houses: %+v", all)
	}

	// 3. the creditor's invoice is matched final; the pool cannot be closed while C takes part and is not loaded
	_, err = finModule.RecordCreditorInvoice(ctx, finapi.RecordCreditorInvoice{CommandID: "fin-invoice", TenantID: tenant, ActorID: "ap-1", InvoiceID: "CI-LINE-1",
		LegalEntityID: "LE", CreditorPartyID: "line-1", SupplierInvoiceNo: "LINE-INV-2611-1", InvoiceDate: hours(17).Format(time.DateOnly), Currency: "CNY",
		Lines: []finapi.CreditorInvoiceLine{{ChargeCode: "OCEAN_FREIGHT", Amount: 100_000}}, OccurredAt: hours(17)})
	must("record the line's invoice", err)
	invoice, err := finModule.GetCreditorInvoice(ctx, finapi.GetCreditorInvoice{TenantID: tenant, InvoiceID: "CI-LINE-1"})
	must("the line's invoice", err)
	_, err = finModule.MatchCreditorInvoice(ctx, finapi.MatchCreditorInvoice{CommandID: "fin-match", TenantID: tenant, ActorID: "ap-1", InvoiceID: "CI-LINE-1",
		ExpectedRevision: invoice.Revision, Matches: []finapi.CreditorMatch{{LineNo: 1, Target: finapi.AccrualRef{Kind: "POOL_COST", ID: "P-1", LineNo: 1}, Amount: 100_000,
			Final: true}}, OccurredAt: hours(18)})
	must("match the line's invoice", err)
	if err := closePool("fin-close-early", 19); err == nil || !strings.Contains(err.Error(), "FIN_POOL_NOT_CLOSABLE") {
		t.Fatalf("closing with C taking part and not loaded: %v", err)
	}

	// 4. the same line with C left out: ACTUAL, A and B share the whole line by their loaded W/M, C's share is not used
	_, err = finModule.ApportionConsolCosts(ctx, finapi.ApportionConsolCosts{CommandID: "fin-apportion-used", TenantID: tenant, ActorID: "fin-1", PoolID: "P-1",
		ExpectedRevision: pool().Revision, NotUsed: []string{"SH-C"}, OccurredAt: hours(20)})
	must("apportion with SH-C left out", err)
	used := latest()
	notUsed := finapi.PoolShare{ShareNo: 3, ShipmentID: "SH-C", JobID: "JOB-C", NotUsed: true, Amount: cny(0), Local: cny(0)}
	if used.ApportionmentNo != 2 || used.Stage != finapi.BasisActual || len(used.Lines) != 1 || used.Lines[0].Amount != cny(100_000) || len(used.Lines[0].Shares) != 3 ||
		used.Lines[0].Shares[0] != share(1, 800, 73_529) || used.Lines[0].Shares[1] != share(2, 288, 26_471) || used.Lines[0].Shares[2] != notUsed {
		t.Fatalf("the freight with SH-C left out: %+v", used)
	}

	// 5. C's job carries no pool cost
	profit := func(job string) finapi.JobProfit {
		t.Helper()
		p, err := finModule.GetJobProfit(ctx, finapi.GetJobProfit{TenantID: tenant, JobID: job})
		must("profit of "+job, err)
		return p
	}
	if a, bb, cc := profit("JOB-A"), profit("JOB-B"), profit("JOB-C"); a.PoolCost != cny(73_529) || len(a.PoolShares) != 1 || bb.PoolCost != cny(26_471) ||
		len(bb.PoolShares) != 1 || cc.PoolCost != cny(0) || len(cc.PoolShares) != 0 || cc.Revenue != cny(284_936) || cc.Profit != cny(284_936) {
		t.Fatalf("the jobs: A %+v, B %+v, C %+v", a, bb, cc)
	}

	// 6. the pool closes, and GetConsolProfit answers
	must("close the pool", closePool("fin-close", 21))
	if p := pool(); p.State != finapi.PoolClosed || len(p.Apportionments) != 2 || p.Apportionments[0].Stage != finapi.BasisPlanned {
		t.Fatalf("the closed pool: %+v", p)
	}
	consol, err := finModule.GetConsolProfit(ctx, finapi.GetConsolProfit{TenantID: tenant, ConsolidationID: "C-1"})
	must("consolidation profit", err)
	// C stays a house of the consolidation: listed with pool cost zero and its own revenue and profit, so the houses' revenue
	// is the consolidation's, 427,404 + 178,085 + 284,936 = 890,425
	var revenue int64
	for _, h := range consol.Houses {
		revenue += h.Revenue.Minor
	}
	if consol.State != finapi.PoolClosed || consol.Pooled != cny(100_000) || consol.Apportioned != cny(100_000) || consol.Unapportioned != cny(0) || len(consol.Houses) != 3 ||
		consol.Houses[0].ShipmentID != "SH-A" || consol.Houses[0].Revenue != cny(427_404) || consol.Houses[0].PoolCost != cny(73_529) || consol.Houses[0].Profit != cny(353_875) ||
		consol.Houses[1].ShipmentID != "SH-B" || consol.Houses[1].Revenue != cny(178_085) || consol.Houses[1].PoolCost != cny(26_471) || consol.Houses[1].Profit != cny(151_614) ||
		consol.Houses[2].ShipmentID != "SH-C" || consol.Houses[2].JobID != "JOB-C" || consol.Houses[2].Revenue != cny(284_936) || consol.Houses[2].PoolCost != cny(0) ||
		consol.Houses[2].Profit != cny(284_936) || revenue != 890_425 {
		t.Fatalf("the consolidation's profit: %+v", consol)
	}
}

// TestTheCostBasisTellsWhichHouseIsInWhichContainerWithPHYAndFIN runs the scenario of
// TestTwoOwnContainersAndASplitHouseAreCostedWithPHYAndFIN (two own containers, A wholly in the first, B wholly in the
// second, C split 4 + 2 packs) through the real ORG, RATE, COM, FUL, PHY and FIN and checks what V2.79 adds: PHY's cost
// basis says which house is in which container, with the measures of each stuffing, and the state and revision of each
// container's packing list; FIN carries those facts and uses none, so its apportionment of the freight by CHARGEABLE
// gives exactly the shares of that test. The measures are that test's: A 312 kg and 800,000 cm3 in unit 1; B 288 kg and
// 200,000 cm3 in unit 2; C 533 kg and 1,333,333 cm3 (4/6 of its lot) in unit 1 and 266 kg and 666,666 cm3 (2/6) in unit 2.
// A stuffing's revision is 1 when opened, 2 once loading, one more for each of its two loads (4) and 5 once sealed.
func TestTheCostBasisTellsWhichHouseIsInWhichContainerWithPHYAndFIN(t *testing.T) {
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
	fulModule, phyModule, err := platform.FULAndPHY(comModule, allowAll{}, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	finModule, err := platform.FIN(orgModule, rateModule, fulModule, &DocumentSnapshots{}, phyModule, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	tenant := uniqueTenant("cargo-ocean-which-")
	seedORG(t, platform, tenant, "party-a", "party-b", "party-c", "line-1")
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	departs := at.Add(72 * time.Hour)
	hours := func(h int) time.Time { return at.Add(time.Duration(h) * time.Hour) }
	kg := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }
	cmq := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "CMQ", Amount: n} }
	unlocode := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	phyOK := func(_ phyapi.Receipt, err error) { t.Helper(); must("PHY", err) }
	evidence := func(source, ref string, h int) phyapi.Evidence {
		return phyapi.Evidence{Source: source, Reference: ref, OccurredAt: hours(h)}
	}
	_, err = orgModule.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "role-line-1", TenantID: tenant, ActorID: "org-officer", LegalEntityID: "LE",
		PartyID: "line-1", ExpectedRevision: 1, RoleCode: orgapi.RoleCarrier, ValidFrom: at.Add(-time.Hour), EvidenceRef: "service contract", OccurredAt: at})
	must("role line-1", err)
	contexts := slice{org: orgModule, rate: rateModule, com: comModule, ful: fulModule, tenant: tenant, at: at, departs: departs}
	for _, s := range []string{"A", "B", "C"} {
		planSeaSellOnly(t, contexts, "party-"+strings.ToLower(s), s, 60_000)
	}
	phyOK(phyModule.CreateConsolidation(ctx, phyapi.CreateConsolidation{CommandID: "consol", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		Mode: phyapi.ModeSea, Kind: phyapi.KindSeaLCLOwn, OperatingLegalEntityID: "LE", Origin: unlocode("CNSHA"), Destination: unlocode("DEHAM"),
		PlannedDeparture: departs.Add(6 * time.Hour), HouseCutOff: departs.Add(-6 * time.Hour), OccurredAt: hours(8)}))
	for i := range 2 {
		phyOK(phyModule.PlanLoadUnit(ctx, phyapi.PlanLoadUnit{CommandID: "plan-unit-" + string(rune('1'+i)), TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
			ExpectedRevision: int64(1 + i), Kind: phyapi.LoadUnitContainer, TypeCode: "22G1", OccurredAt: hours(8)}))
	}
	for _, s := range []string{"SH-A", "SH-B", "SH-C"} {
		phyOK(phyModule.AttachHouse(ctx, phyapi.AttachHouse{CommandID: "attach-" + s, TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1", ShipmentID: s,
			OccurredAt: hours(9)}))
		record(t, ctx, fulModule, tenant, "ful-attach-"+s, s, "C-1", 3, hours(9))
	}
	_, err = finModule.OpenFinancePolicy(ctx, finapi.OpenFinancePolicy{CommandID: "fin-policy", TenantID: tenant, ActorID: "acct-1", LegalEntityID: "LE",
		FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: hours(10)})
	must("finance policy", err)
	_, err = finModule.RecordExchangeRate(ctx, finapi.RecordExchangeRate{CommandID: "fin-rate", TenantID: tenant, ActorID: "acct-1", LegalEntityID: "LE",
		ExpectedRevision: 1, RateType: finapi.RateEstimate, From: "USD", Rate: 71_234, Scale: 4, FromDigits: 2, ToDigits: 2, ValidFrom: at,
		ValidUntil: at.AddDate(0, 1, 0), Source: "CFETS central parity", OccurredAt: hours(10)})
	must("estimate rate", err)
	for _, s := range []string{"A", "B", "C"} {
		_, err = finModule.OpenFreightJob(ctx, finapi.OpenFreightJob{CommandID: "fin-job-" + s, TenantID: tenant, ActorID: "fin-1", JobID: "JOB-" + s, ShipmentID: "SH-" + s,
			OccurredAt: hours(10)})
		must("open JOB-"+s, err)
	}
	_, err = finModule.OpenConsolCostPool(ctx, finapi.OpenConsolCostPool{CommandID: "fin-pool", TenantID: tenant, ActorID: "fin-1", PoolID: "P-1", ConsolidationID: "C-1",
		OccurredAt: hours(12)})
	must("open the pool", err)
	_, err = finModule.RecordConsolCost(ctx, finapi.RecordConsolCost{CommandID: "fin-cost", TenantID: tenant, ActorID: "fin-1", PoolID: "P-1", ExpectedRevision: 1,
		ChargeCode: "OCEAN_FREIGHT", Method: finapi.MethodChargeable, Cost: finapi.ChargeSide{PartyID: "line-1", Amount: usd(100_000)}, Reason: "the line's quote for two containers",
		OccurredAt: hours(12)})
	must("cost line", err)

	// both lots weighed at the CFS: A 312 kg and 0.8 m3, B 288 kg and 0.2 m3, C 800 kg and 2 m3, 6 packs each
	c, err := phyModule.GetConsolidation(ctx, phyapi.GetConsolidation{TenantID: tenant, ConsolidationID: "C-1"})
	must("consolidation", err)
	phyOK(phyModule.CloseConsolidation(ctx, phyapi.CloseConsolidation{CommandID: "close", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ExpectedRevision: c.Revision, OccurredAt: hours(13)}))
	cfs := phyapi.Facility{Location: c.Current.Origin, Code: "SHA-CFS1"}
	weights := map[string]int64{"A": 312, "B": 288, "C": 800}
	volumes := map[string]int64{"A": 800_000, "B": 200_000, "C": 2_000_000}
	for _, s := range []string{"A", "B", "C"} {
		lot := "L-" + s
		phyOK(phyModule.ReceiveCargo(ctx, phyapi.ReceiveCargo{CommandID: "rcv-" + s, TenantID: tenant, ActorID: "cfs-1", LotID: lot, ShipmentID: "SH-" + s,
			Facility: cfs, Packs: 6, PackageType: "CT", Condition: phyapi.ConditionGood, Evidence: evidence("CFS-WMS", "GRN-"+s, 14), OccurredAt: hours(14)}))
		phyOK(phyModule.RecordCargoMeasurement(ctx, phyapi.RecordCargoMeasurement{CommandID: "weigh-" + s, TenantID: tenant, ActorID: "cfs-1", LotID: lot,
			ExpectedRevision: 1, GrossWeight: kg(weights[s]), Volume: cmq(volumes[s]), Method: phyapi.MethodScale, Evidence: evidence("CFS-SCALE", "W-"+s, 14), OccurredAt: hours(14)}))
		phyOK(phyModule.CompleteReceipt(ctx, phyapi.CompleteReceipt{CommandID: "complete-" + s, TenantID: tenant, ActorID: "cfs-1", LotID: lot,
			ExpectedRevision: 2, OccurredAt: hours(14)}))
	}
	// two stuffings, one per container: X-1 takes all of A and 4 packs of C, X-2 all of B and the other 2 packs of C
	revision := map[string]int64{}
	for i, x := range []string{"X-1", "X-2"} {
		phyOK(phyModule.OpenLoadExecution(ctx, phyapi.OpenLoadExecution{CommandID: "stuff-" + x, TenantID: tenant, ActorID: "cfs-1", ExecutionID: x,
			ConsolidationID: "C-1", Kind: phyapi.ExecutionStuffing, Facility: cfs, LoadUnitNo: i + 1, OccurredAt: hours(15)}))
		phyOK(phyModule.StartLoading(ctx, phyapi.StartLoading{CommandID: "start-" + x, TenantID: tenant, ActorID: "cfs-1", ExecutionID: x, ExpectedRevision: 1, OccurredAt: hours(15)}))
		revision[x] = 2
	}
	basis := func() phyapi.PhysicalCostBasis {
		t.Helper()
		b, err := phyModule.GetPhysicalCostBasis(ctx, phyapi.GetPhysicalCostBasis{TenantID: tenant, ConsolidationID: "C-1"})
		must("PHY's cost basis", err)
		return b
	}
	for _, l := range []struct {
		x, shipment string
		packs       int64
	}{{"X-1", "A", 6}, {"X-1", "C", 4}, {"X-2", "B", 6}, {"X-2", "C", 2}} {
		phyOK(phyModule.RecordCargoLoaded(ctx, phyapi.RecordCargoLoaded{CommandID: "load-" + l.x + "-" + l.shipment, TenantID: tenant, ActorID: "cfs-1", ExecutionID: l.x,
			ExpectedRevision: revision[l.x], LotID: "L-" + l.shipment, Packs: l.packs, Evidence: evidence("CFS-TALLY", "T-"+l.x+l.shipment, 15), OccurredAt: hours(15)}))
		revision[l.x]++
	}
	// both containers are in use until sealed, then closed; each packing list is at the stuffing's revision
	for i, u := range basis().LoadUnits {
		if u.UnitNo != i+1 || u.Kind != phyapi.LoadUnitContainer || u.TypeCode != "22G1" || u.Status != phyapi.UnitStatusInUse || u.ExecutionID != []string{"X-1", "X-2"}[i] ||
			u.ExecutionState != phyapi.ExecutionLoading || u.ExecutionRevision != 4 {
			t.Fatalf("load unit %d before the seals: %+v", i+1, u)
		}
	}
	type unitLoad struct {
		unit, packs, kilos, cubicCm int64
	}
	unitLoads := func(b phyapi.PhysicalCostBasis) [][]unitLoad {
		var out [][]unitLoad
		for _, h := range b.Houses {
			var loads []unitLoad
			for _, e := range h.Units {
				if e.ExecutionID != "X-"+string(rune('0'+e.UnitNo)) || e.LoadedGrossWeight.Unit != "KGM" || e.LoadedVolume.Unit != "CMQ" {
					t.Fatalf("%s's entry %+v", h.ShipmentID, e)
				}
				loads = append(loads, unitLoad{int64(e.UnitNo), e.LoadedPacks, e.LoadedGrossWeight.Amount, e.LoadedVolume.Amount})
			}
			out = append(out, loads)
		}
		return out
	}
	wantLoads := [][]unitLoad{{{1, 6, 312, 800_000}}, {{2, 6, 288, 200_000}}, {{1, 4, 533, 1_333_333}, {2, 2, 266, 666_666}}}
	if got := unitLoads(basis()); !reflect.DeepEqual(got, wantLoads) {
		t.Fatalf("which house is in which container before the seals: %+v, want %+v", got, wantLoads)
	}
	for _, x := range []string{"X-1", "X-2"} {
		phyOK(phyModule.CloseLoadExecution(ctx, phyapi.CloseLoadExecution{CommandID: "seal-" + x, TenantID: tenant, ActorID: "cfs-1", ExecutionID: x,
			ExpectedRevision: revision[x], Evidence: evidence(phyapi.EvidenceSeal, "SEAL-"+x, 15), OccurredAt: hours(15)}))
	}
	b := basis()
	for i, u := range b.LoadUnits {
		if u.ExecutionState != phyapi.ExecutionClosed || u.ExecutionRevision != 5 || u.ExecutionID != []string{"X-1", "X-2"}[i] {
			t.Fatalf("load unit %d after the seals: %+v", i+1, u)
		}
	}
	if got := unitLoads(b); !reflect.DeepEqual(got, wantLoads) {
		t.Fatalf("which house is in which container after the seals: %+v, want %+v", got, wantLoads)
	}
	if len(b.LoadUnits) != 2 || b.LoadUnits[0].UnitNo != 1 || b.LoadUnits[1].UnitNo != 2 || b.LoadUnits[0].Status != phyapi.UnitStatusClosed || b.LoadUnits[1].Status != phyapi.UnitStatusClosed {
		t.Fatalf("the load units after the seals: %+v", b.LoadUnits)
	}
	// PHY's cost basis per house: SH-C's loaded packs, weight and volume are the sums over both containers (4 + 2 packs of
	// its one lot of 800 kg and 2 m3). Each stuffing line takes its share of the lot's measure rounded down: 4/6 of 800 kg is
	// 533 and 2/6 is 266, 4/6 of 2,000,000 cm3 is 1,333,333 and 2/6 is 666,666, so the sums are 799 kg and 1,999,999 cm3, one
	// unit short of the lot's (the stand-in numbers of the other tests are whole). Here it changes nothing: W/M is still 2000.
	for i, want := range []struct {
		packs, kilos, cubicCm int64
	}{{6, 312, 800_000}, {6, 288, 200_000}, {6, 799, 1_999_999}} {
		h := b.Houses[i]
		if h.ShipmentID != "SH-"+string(rune('A'+i)) || h.Packs != 6 || h.LoadedPacks != want.packs || h.LoadedGrossWeight != kg(want.kilos) || h.LoadedVolume != cmq(want.cubicCm) {
			t.Fatalf("PHY's cost basis, house %d: %+v", i, h)
		}
	}
	// FIN apportions the freight at ACTUAL on those measures: W/M 800 : 288 : 2000 of 3088, USD 100,000 -> 25,907 / 9,326 /
	// 64,767 and CNY 712,340 -> 184,544 / 66,436 / 461,360 (the hand calculation is in the first test of this file), the same as if the
	// three houses had been stuffed into one container
	_, err = finModule.ApportionConsolCosts(ctx, finapi.ApportionConsolCosts{CommandID: "fin-apportion", TenantID: tenant, ActorID: "fin-1", PoolID: "P-1",
		ExpectedRevision: 2, OccurredAt: hours(17)})
	must("apportion", err)
	pool, err := finModule.GetConsolCostPool(ctx, finapi.GetConsolCostPool{TenantID: tenant, PoolID: "P-1"})
	must("the pool", err)
	if len(pool.Apportionments) != 1 || pool.Apportionments[0].Stage != finapi.BasisActual {
		t.Fatalf("the apportionment: %+v", pool.Apportionments)
	}
	shares := pool.Apportionments[0].Lines[0].Shares
	for i, want := range []struct{ quantity, usd, cny int64 }{{800, 25_907, 184_544}, {288, 9_326, 66_436}, {2000, 64_767, 461_360}} {
		if len(shares) != 3 || shares[i].Quantity != want.quantity || shares[i].Unit != "MWM" || shares[i].Amount != usd(want.usd) ||
			shares[i].Local != (kernel.Money{Currency: "CNY", Minor: want.cny}) {
			t.Fatalf("share %d: %+v, want %+v", i+1, shares, want)
		}
	}
}

// Costs are apportioned by container count and by TEU, with the real ORG, RATE, COM, FUL, PHY and FIN (V2.80). An
// own-container consolidation has a 20-foot (22G1) and a 40-foot (42G1) container: SH-A is stuffed wholly into the
// 20-foot one (6 packs), SH-B wholly into the 40-foot one (6 packs) and SH-C is split over both (4 packs, 2 packs). Before
// the stuffing a line by TEU is refused (FIN_POOL_NO_BASIS: no house is in a container yet). After the stuffings are
// sealed PHY's cost basis tells the houses' containers and the units' TEU, and the lines are apportioned by container
// count and by TEU, once with a house left out; the jobs carry the shares and the pool closes.
func TestCostsAreApportionedByContainerCountAndTEUWithPHYAndFIN(t *testing.T) {
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
	fulModule, phyModule, err := platform.FULAndPHY(comModule, allowAll{}, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	finModule, err := platform.FIN(orgModule, rateModule, fulModule, &DocumentSnapshots{}, phyModule, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	tenant := uniqueTenant("cargo-ocean-teu-")
	seedORG(t, platform, tenant, "party-a", "party-b", "party-c", "line-1")
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	departs := at.Add(72 * time.Hour)
	hours := func(h int) time.Time { return at.Add(time.Duration(h) * time.Hour) }
	kg := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }
	cny := func(n int64) kernel.Money { return kernel.Money{Currency: "CNY", Minor: n} }
	cmq := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "CMQ", Amount: n} }
	unlocode := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	phyOK := func(_ phyapi.Receipt, err error) { t.Helper(); must("PHY", err) }
	evidence := func(source, ref string, h int) phyapi.Evidence {
		return phyapi.Evidence{Source: source, Reference: ref, OccurredAt: hours(h)}
	}
	_, err = orgModule.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "role-line-1", TenantID: tenant, ActorID: "org-officer", LegalEntityID: "LE",
		PartyID: "line-1", ExpectedRevision: 1, RoleCode: orgapi.RoleCarrier, ValidFrom: at.Add(-time.Hour), EvidenceRef: "service contract", OccurredAt: at})
	must("role line-1", err)
	contexts := slice{org: orgModule, rate: rateModule, com: comModule, ful: fulModule, tenant: tenant, at: at, departs: departs}
	for _, s := range []string{"A", "B", "C"} {
		planSeaSellOnly(t, contexts, "party-"+strings.ToLower(s), s, 60_000)
	}
	phyOK(phyModule.CreateConsolidation(ctx, phyapi.CreateConsolidation{CommandID: "consol", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		Mode: phyapi.ModeSea, Kind: phyapi.KindSeaLCLOwn, OperatingLegalEntityID: "LE", Origin: unlocode("CNSHA"), Destination: unlocode("DEHAM"),
		PlannedDeparture: departs.Add(6 * time.Hour), HouseCutOff: departs.Add(-6 * time.Hour), OccurredAt: hours(8)}))
	for i, typeCode := range []string{"22G1", "42G1"} {
		phyOK(phyModule.PlanLoadUnit(ctx, phyapi.PlanLoadUnit{CommandID: "plan-unit-" + string(rune('1'+i)), TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
			ExpectedRevision: int64(1 + i), Kind: phyapi.LoadUnitContainer, TypeCode: typeCode, OccurredAt: hours(8)}))
	}
	for _, s := range []string{"SH-A", "SH-B", "SH-C"} {
		phyOK(phyModule.AttachHouse(ctx, phyapi.AttachHouse{CommandID: "attach-" + s, TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1", ShipmentID: s,
			OccurredAt: hours(9)}))
		record(t, ctx, fulModule, tenant, "ful-attach-"+s, s, "C-1", 3, hours(9))
	}
	_, err = finModule.OpenFinancePolicy(ctx, finapi.OpenFinancePolicy{CommandID: "fin-policy", TenantID: tenant, ActorID: "acct-1", LegalEntityID: "LE",
		FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: hours(10)})
	must("finance policy", err)
	_, err = finModule.RecordExchangeRate(ctx, finapi.RecordExchangeRate{CommandID: "fin-rate", TenantID: tenant, ActorID: "acct-1", LegalEntityID: "LE",
		ExpectedRevision: 1, RateType: finapi.RateEstimate, From: "USD", Rate: 71_234, Scale: 4, FromDigits: 2, ToDigits: 2, ValidFrom: at,
		ValidUntil: at.AddDate(0, 1, 0), Source: "CFETS central parity", OccurredAt: hours(10)})
	must("estimate rate", err)
	for _, s := range []string{"A", "B", "C"} {
		_, err = finModule.OpenFreightJob(ctx, finapi.OpenFreightJob{CommandID: "fin-job-" + s, TenantID: tenant, ActorID: "fin-1", JobID: "JOB-" + s, ShipmentID: "SH-" + s,
			OccurredAt: hours(10)})
		must("open JOB-"+s, err)
	}
	_, err = finModule.OpenConsolCostPool(ctx, finapi.OpenConsolCostPool{CommandID: "fin-pool", TenantID: tenant, ActorID: "fin-1", PoolID: "P-1", ConsolidationID: "C-1",
		OccurredAt: hours(12)})
	must("open the pool", err)
	for i, line := range []struct{ code, method string }{{"OCEAN_FREIGHT", finapi.MethodContainerCount}, {"THC", finapi.MethodTEU}, {"LASHING", finapi.MethodTEU}} {
		_, err = finModule.RecordConsolCost(ctx, finapi.RecordConsolCost{CommandID: "fin-cost-" + line.code, TenantID: tenant, ActorID: "fin-1", PoolID: "P-1",
			ExpectedRevision: int64(1 + i), ChargeCode: line.code, Method: line.method, Cost: finapi.ChargeSide{PartyID: "line-1", Amount: cny(10_001)},
			Reason: "the line's charge for the two containers", OccurredAt: hours(12)})
		must("cost line "+line.code, err)
	}
	pool := func() finapi.ConsolCostPool {
		t.Helper()
		p, err := finModule.GetConsolCostPool(ctx, finapi.GetConsolCostPool{TenantID: tenant, PoolID: "P-1"})
		must("the pool", err)
		return p
	}
	// before the stuffing no house is in any container: a line by TEU, and one by container count, are refused and nothing
	// is stored
	for i, method := range []string{finapi.MethodTEU, finapi.MethodContainerCount} {
		_, err = finModule.ApportionConsolCosts(ctx, finapi.ApportionConsolCosts{CommandID: "fin-planned-" + method, TenantID: tenant, ActorID: "fin-1", PoolID: "P-1",
			ExpectedRevision: pool().Revision, LineNos: []int{2 - i}, OccurredAt: hours(12)})
		if err == nil || !strings.Contains(err.Error(), "FIN_POOL_NO_BASIS") || !strings.Contains(err.Error(), "stuffed") {
			t.Fatalf("%s before the stuffing: %v", method, err)
		}
	}
	if len(pool().Apportionments) != 0 {
		t.Fatalf("a refused apportionment was stored: %+v", pool().Apportionments)
	}

	// both lots weighed at the CFS: A 312 kg and 0.8 m3, B 288 kg and 0.2 m3, C 800 kg and 2 m3, 6 packs each
	c, err := phyModule.GetConsolidation(ctx, phyapi.GetConsolidation{TenantID: tenant, ConsolidationID: "C-1"})
	must("consolidation", err)
	phyOK(phyModule.CloseConsolidation(ctx, phyapi.CloseConsolidation{CommandID: "close", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ExpectedRevision: c.Revision, OccurredAt: hours(13)}))
	cfs := phyapi.Facility{Location: c.Current.Origin, Code: "SHA-CFS1"}
	weights := map[string]int64{"A": 312, "B": 288, "C": 800}
	volumes := map[string]int64{"A": 800_000, "B": 200_000, "C": 2_000_000}
	for _, s := range []string{"A", "B", "C"} {
		lot := "L-" + s
		phyOK(phyModule.ReceiveCargo(ctx, phyapi.ReceiveCargo{CommandID: "rcv-" + s, TenantID: tenant, ActorID: "cfs-1", LotID: lot, ShipmentID: "SH-" + s,
			Facility: cfs, Packs: 6, PackageType: "CT", Condition: phyapi.ConditionGood, Evidence: evidence("CFS-WMS", "GRN-"+s, 14), OccurredAt: hours(14)}))
		phyOK(phyModule.RecordCargoMeasurement(ctx, phyapi.RecordCargoMeasurement{CommandID: "weigh-" + s, TenantID: tenant, ActorID: "cfs-1", LotID: lot,
			ExpectedRevision: 1, GrossWeight: kg(weights[s]), Volume: cmq(volumes[s]), Method: phyapi.MethodScale, Evidence: evidence("CFS-SCALE", "W-"+s, 14), OccurredAt: hours(14)}))
		phyOK(phyModule.CompleteReceipt(ctx, phyapi.CompleteReceipt{CommandID: "complete-" + s, TenantID: tenant, ActorID: "cfs-1", LotID: lot,
			ExpectedRevision: 2, OccurredAt: hours(14)}))
	}
	// two stuffings, one per container: X-1 takes all of A and 4 packs of C, X-2 all of B and the other 2 packs of C
	revision := map[string]int64{}
	for i, x := range []string{"X-1", "X-2"} {
		phyOK(phyModule.OpenLoadExecution(ctx, phyapi.OpenLoadExecution{CommandID: "stuff-" + x, TenantID: tenant, ActorID: "cfs-1", ExecutionID: x,
			ConsolidationID: "C-1", Kind: phyapi.ExecutionStuffing, Facility: cfs, LoadUnitNo: i + 1, OccurredAt: hours(15)}))
		phyOK(phyModule.StartLoading(ctx, phyapi.StartLoading{CommandID: "start-" + x, TenantID: tenant, ActorID: "cfs-1", ExecutionID: x, ExpectedRevision: 1, OccurredAt: hours(15)}))
		revision[x] = 2
	}
	basis := func() phyapi.PhysicalCostBasis {
		t.Helper()
		b, err := phyModule.GetPhysicalCostBasis(ctx, phyapi.GetPhysicalCostBasis{TenantID: tenant, ConsolidationID: "C-1"})
		must("PHY's cost basis", err)
		return b
	}
	for _, l := range []struct {
		x, shipment string
		packs       int64
	}{{"X-1", "A", 6}, {"X-1", "C", 4}, {"X-2", "B", 6}, {"X-2", "C", 2}} {
		phyOK(phyModule.RecordCargoLoaded(ctx, phyapi.RecordCargoLoaded{CommandID: "load-" + l.x + "-" + l.shipment, TenantID: tenant, ActorID: "cfs-1", ExecutionID: l.x,
			ExpectedRevision: revision[l.x], LotID: "L-" + l.shipment, Packs: l.packs, Evidence: evidence("CFS-TALLY", "T-"+l.x+l.shipment, 15), OccurredAt: hours(15)}))
		revision[l.x]++
	}
	// both containers are in use until sealed, then closed
	for i, u := range basis().LoadUnits {
		if u.UnitNo != i+1 || u.Kind != phyapi.LoadUnitContainer || u.TypeCode != []string{"22G1", "42G1"}[i] || u.Status != phyapi.UnitStatusInUse || u.ExecutionID != []string{"X-1", "X-2"}[i] {
			t.Fatalf("load unit %d before the seals: %+v", i+1, u)
		}
	}
	for _, x := range []string{"X-1", "X-2"} {
		phyOK(phyModule.CloseLoadExecution(ctx, phyapi.CloseLoadExecution{CommandID: "seal-" + x, TenantID: tenant, ActorID: "cfs-1", ExecutionID: x,
			ExpectedRevision: revision[x], Evidence: evidence(phyapi.EvidenceSeal, "SEAL-"+x, 15), OccurredAt: hours(15)}))
	}
	// PHY answers the two types' TEU in tenths: a 20-foot container is 10 and a 40-foot 20, and each house's containers
	b := basis()
	if len(b.LoadUnits) != 2 || b.LoadUnits[0].Status != phyapi.UnitStatusClosed || b.LoadUnits[1].Status != phyapi.UnitStatusClosed || b.LoadUnits[0].TypeCode != "22G1" || b.LoadUnits[0].TEUTenths != 10 || b.LoadUnits[1].TypeCode != "42G1" || b.LoadUnits[1].TEUTenths != 20 {
		t.Fatalf("the load units after the seals: %+v", b.LoadUnits)
	}
	for i, want := range [][]int{{1}, {2}, {1, 2}} {
		var got []int
		for _, u := range b.Houses[i].Units {
			got = append(got, u.UnitNo)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("the containers of house %d: %v, want %v", i, got, want)
		}
	}
	apportion := func(id string, h int, lines []int, notUsed ...string) {
		t.Helper()
		_, err := finModule.ApportionConsolCosts(ctx, finapi.ApportionConsolCosts{CommandID: id, TenantID: tenant, ActorID: "fin-1", PoolID: "P-1",
			ExpectedRevision: pool().Revision, LineNos: lines, NotUsed: notUsed, OccurredAt: hours(h)})
		must("apportion "+id, err)
	}
	share := func(no int, quantity int64, unit string, amount int64) finapi.PoolShare {
		return finapi.PoolShare{ShareNo: no, ShipmentID: "SH-" + string(rune('A'+no-1)), JobID: "JOB-" + string(rune('A'+no-1)), Quantity: quantity, Unit: unit,
			Amount: cny(amount), Local: cny(amount)}
	}
	check := func(what string, line int, method string, want ...finapi.PoolShare) {
		t.Helper()
		ap := pool().Apportionments
		for _, la := range ap[len(ap)-1].Lines {
			if la.LineNo != line {
				continue
			}
			if ap[len(ap)-1].Stage != finapi.BasisActual || la.Method != method || !reflect.DeepEqual(la.Shares, want) {
				t.Fatalf("%s: line %d is %s at %s: %+v, want %+v", what, line, la.Method, ap[len(ap)-1].Stage, la.Shares, want)
			}
			return
		}
		t.Fatalf("%s: line %d is not in the latest apportionment", what, line)
	}

	// The three houses by container: SH-A is in the 20-foot (unit 1) only, SH-B in the 40-foot (unit 2) only and SH-C in
	// both, so the containers are 1 : 1 : 2 and the tenths of a TEU 10 : 20 : 30 (10 + 0, 0 + 20, 10 + 20). Each line is CNY
	// 100.01, 10,001 minor:
	//   OCEAN_FREIGHT by container count: 10,001 over 4 is 2,500.25 / 2,500.25 / 5,000.50; the floors leave one unit, to
	//     SH-C (.50 is the largest remainder) -> 2,500 / 2,500 / 5,001
	//   THC by TEU: 10,001 over 60 is 1,666.83 / 3,333.67 / 5,000.50; the floors leave two units, to SH-A (.83) and SH-B
	//     (.67) -> 1,667 / 3,334 / 5,000
	//   LASHING by TEU with SH-B left out: 10 : 30, 10,001 over 40 is 2,500.25 / 7,500.75; the floors leave one unit, to
	//     SH-C -> 2,500 / 7,501; SH-B's share is not used
	apportion("fin-apportion-1", 17, []int{1, 2})
	check("container count", 1, finapi.MethodContainerCount, share(1, 1, "CNT", 2_500), share(2, 1, "CNT", 2_500), share(3, 2, "CNT", 5_001))
	check("TEU", 2, finapi.MethodTEU, share(1, 10, "DTE", 1_667), share(2, 20, "DTE", 3_334), share(3, 30, "DTE", 5_000))
	apportion("fin-apportion-2", 18, []int{3}, "SH-B")
	check("TEU with SH-B left out", 3, finapi.MethodTEU, share(1, 10, "DTE", 2_500),
		finapi.PoolShare{ShareNo: 2, ShipmentID: "SH-B", JobID: "JOB-B", NotUsed: true, Amount: cny(0), Local: cny(0)}, share(3, 30, "DTE", 7_501))

	// each job carries its shares: SH-A 2,500 + 1,667 + 2,500, SH-B 2,500 + 3,334 + 0 and SH-C 5,001 + 5,000 + 7,501, which
	// are the three lines' 30,003 together
	for job, want := range map[string]int64{"JOB-A": 6_667, "JOB-B": 5_834, "JOB-C": 17_502} {
		p, err := finModule.GetJobProfit(ctx, finapi.GetJobProfit{TenantID: tenant, JobID: job})
		must("profit of "+job, err)
		if p.PoolCost != cny(want) {
			t.Fatalf("the pool cost of %s is %v, want %d", job, p.PoolCost, want)
		}
	}
	consol, err := finModule.GetConsolProfit(ctx, finapi.GetConsolProfit{TenantID: tenant, ConsolidationID: "C-1"})
	must("the consolidation's profit", err)
	if consol.Pooled != cny(30_003) || consol.Apportioned != cny(30_003) || consol.Unapportioned != cny(0) || len(consol.Houses) != 3 ||
		consol.Houses[0].PoolCost != cny(6_667) || consol.Houses[1].PoolCost != cny(5_834) || consol.Houses[2].PoolCost != cny(17_502) {
		t.Fatalf("the consolidation's profit: %+v", consol)
	}

	// the carrier's invoice for the three lines is matched final and the pool closes
	_, err = finModule.RecordCreditorInvoice(ctx, finapi.RecordCreditorInvoice{CommandID: "fin-invoice", TenantID: tenant, ActorID: "ap-1", InvoiceID: "CI-LINE-1",
		LegalEntityID: "LE", CreditorPartyID: "line-1", SupplierInvoiceNo: "LINE-INV-2611-2", InvoiceDate: hours(19).Format(time.DateOnly), Currency: "CNY",
		Lines:      []finapi.CreditorInvoiceLine{{ChargeCode: "OCEAN_FREIGHT", Amount: 10_001}, {ChargeCode: "THC", Amount: 10_001}, {ChargeCode: "LASHING", Amount: 10_001}},
		OccurredAt: hours(19)})
	must("record the line's invoice", err)
	invoice, err := finModule.GetCreditorInvoice(ctx, finapi.GetCreditorInvoice{TenantID: tenant, InvoiceID: "CI-LINE-1"})
	must("the line's invoice", err)
	matches := make([]finapi.CreditorMatch, 3)
	for i := range matches {
		matches[i] = finapi.CreditorMatch{LineNo: i + 1, Target: finapi.AccrualRef{Kind: "POOL_COST", ID: "P-1", LineNo: i + 1}, Amount: 10_001, Final: true}
	}
	_, err = finModule.MatchCreditorInvoice(ctx, finapi.MatchCreditorInvoice{CommandID: "fin-match", TenantID: tenant, ActorID: "ap-1", InvoiceID: "CI-LINE-1",
		ExpectedRevision: invoice.Revision, Matches: matches, OccurredAt: hours(20)})
	must("match the line's invoice", err)
	_, err = finModule.CloseConsolCostPool(ctx, finapi.CloseConsolCostPool{CommandID: "fin-close", TenantID: tenant, ActorID: "fin-1", PoolID: "P-1",
		ExpectedRevision: pool().Revision, OccurredAt: hours(21)})
	must("close the pool", err)
	if p := pool(); p.State != finapi.PoolClosed || len(p.Closures) != 1 {
		t.Fatalf("the closed pool: %+v", p)
	}
}

// The containers must be sealed (V2.80, the review's case), with the real ORG, RATE, COM, FUL, PHY and FIN. A 20-foot and a
// 40-foot container; SH-A has all 6 packs in the 20-foot one, SH-B all 6 in the 40-foot one and SH-C is split: 4 of its 6
// packs go into the 20-foot one first and the other 2 into the 40-foot one later. Every house has loaded packs, so the
// stage is ACTUAL, but SH-C would be counted in one container only, and the pool would close on that after the first
// container is sealed. So a line by container count or by TEU is refused (FIN_POOL_NO_BASIS) while any container of
// the consolidation is not sealed: after the loads and before any seal, and with the 20-foot container sealed and the
// 40-foot one still loading. A line by SHIPMENT of the same pool is accepted in between. After both seals both methods
// are accepted: SH-C is in 2 containers and 30 tenths of a TEU, and the pool closes.
func TestContainerMethodsNeedEverySealedContainerWithPHYAndFIN(t *testing.T) {
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
	fulModule, phyModule, err := platform.FULAndPHY(comModule, allowAll{}, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	finModule, err := platform.FIN(orgModule, rateModule, fulModule, &DocumentSnapshots{}, phyModule, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	tenant := uniqueTenant("cargo-ocean-sealed-")
	seedORG(t, platform, tenant, "party-a", "party-b", "party-c", "line-1")
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	departs := at.Add(72 * time.Hour)
	hours := func(h int) time.Time { return at.Add(time.Duration(h) * time.Hour) }
	kg := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }
	cny := func(n int64) kernel.Money { return kernel.Money{Currency: "CNY", Minor: n} }
	cmq := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "CMQ", Amount: n} }
	unlocode := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	phyOK := func(_ phyapi.Receipt, err error) { t.Helper(); must("PHY", err) }
	evidence := func(source, ref string, h int) phyapi.Evidence {
		return phyapi.Evidence{Source: source, Reference: ref, OccurredAt: hours(h)}
	}
	_, err = orgModule.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "role-line-1", TenantID: tenant, ActorID: "org-officer", LegalEntityID: "LE",
		PartyID: "line-1", ExpectedRevision: 1, RoleCode: orgapi.RoleCarrier, ValidFrom: at.Add(-time.Hour), EvidenceRef: "service contract", OccurredAt: at})
	must("role line-1", err)
	contexts := slice{org: orgModule, rate: rateModule, com: comModule, ful: fulModule, tenant: tenant, at: at, departs: departs}
	for _, s := range []string{"A", "B", "C"} {
		planSeaSellOnly(t, contexts, "party-"+strings.ToLower(s), s, 60_000)
	}
	phyOK(phyModule.CreateConsolidation(ctx, phyapi.CreateConsolidation{CommandID: "consol", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		Mode: phyapi.ModeSea, Kind: phyapi.KindSeaLCLOwn, OperatingLegalEntityID: "LE", Origin: unlocode("CNSHA"), Destination: unlocode("DEHAM"),
		PlannedDeparture: departs.Add(6 * time.Hour), HouseCutOff: departs.Add(-6 * time.Hour), OccurredAt: hours(8)}))
	for i, typeCode := range []string{"22G1", "42G1"} {
		phyOK(phyModule.PlanLoadUnit(ctx, phyapi.PlanLoadUnit{CommandID: "plan-unit-" + string(rune('1'+i)), TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
			ExpectedRevision: int64(1 + i), Kind: phyapi.LoadUnitContainer, TypeCode: typeCode, OccurredAt: hours(8)}))
	}
	for _, s := range []string{"SH-A", "SH-B", "SH-C"} {
		phyOK(phyModule.AttachHouse(ctx, phyapi.AttachHouse{CommandID: "attach-" + s, TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1", ShipmentID: s,
			OccurredAt: hours(9)}))
		record(t, ctx, fulModule, tenant, "ful-attach-"+s, s, "C-1", 3, hours(9))
	}
	_, err = finModule.OpenFinancePolicy(ctx, finapi.OpenFinancePolicy{CommandID: "fin-policy", TenantID: tenant, ActorID: "acct-1", LegalEntityID: "LE",
		FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: hours(10)})
	must("finance policy", err)
	_, err = finModule.RecordExchangeRate(ctx, finapi.RecordExchangeRate{CommandID: "fin-rate", TenantID: tenant, ActorID: "acct-1", LegalEntityID: "LE",
		ExpectedRevision: 1, RateType: finapi.RateEstimate, From: "USD", Rate: 71_234, Scale: 4, FromDigits: 2, ToDigits: 2, ValidFrom: at,
		ValidUntil: at.AddDate(0, 1, 0), Source: "CFETS central parity", OccurredAt: hours(10)})
	must("estimate rate", err)
	for _, s := range []string{"A", "B", "C"} {
		_, err = finModule.OpenFreightJob(ctx, finapi.OpenFreightJob{CommandID: "fin-job-" + s, TenantID: tenant, ActorID: "fin-1", JobID: "JOB-" + s, ShipmentID: "SH-" + s,
			OccurredAt: hours(10)})
		must("open JOB-"+s, err)
	}
	_, err = finModule.OpenConsolCostPool(ctx, finapi.OpenConsolCostPool{CommandID: "fin-pool", TenantID: tenant, ActorID: "fin-1", PoolID: "P-1", ConsolidationID: "C-1",
		OccurredAt: hours(12)})
	must("open the pool", err)
	for i, line := range []struct{ code, method string }{{"OCEAN_FREIGHT", finapi.MethodContainerCount}, {"THC", finapi.MethodTEU}, {"LASHING", finapi.MethodShipment}} {
		_, err = finModule.RecordConsolCost(ctx, finapi.RecordConsolCost{CommandID: "fin-cost-" + line.code, TenantID: tenant, ActorID: "fin-1", PoolID: "P-1",
			ExpectedRevision: int64(1 + i), ChargeCode: line.code, Method: line.method, Cost: finapi.ChargeSide{PartyID: "line-1", Amount: cny(10_001)},
			Reason: "the line's charge for the two containers", OccurredAt: hours(12)})
		must("cost line "+line.code, err)
	}
	pool := func() finapi.ConsolCostPool {
		t.Helper()
		p, err := finModule.GetConsolCostPool(ctx, finapi.GetConsolCostPool{TenantID: tenant, PoolID: "P-1"})
		must("the pool", err)
		return p
	}

	// both lots weighed at the CFS: A 312 kg and 0.8 m3, B 288 kg and 0.2 m3, C 800 kg and 2 m3, 6 packs each
	c, err := phyModule.GetConsolidation(ctx, phyapi.GetConsolidation{TenantID: tenant, ConsolidationID: "C-1"})
	must("consolidation", err)
	phyOK(phyModule.CloseConsolidation(ctx, phyapi.CloseConsolidation{CommandID: "close", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ExpectedRevision: c.Revision, OccurredAt: hours(13)}))
	cfs := phyapi.Facility{Location: c.Current.Origin, Code: "SHA-CFS1"}
	weights := map[string]int64{"A": 312, "B": 288, "C": 800}
	volumes := map[string]int64{"A": 800_000, "B": 200_000, "C": 2_000_000}
	for _, s := range []string{"A", "B", "C"} {
		lot := "L-" + s
		phyOK(phyModule.ReceiveCargo(ctx, phyapi.ReceiveCargo{CommandID: "rcv-" + s, TenantID: tenant, ActorID: "cfs-1", LotID: lot, ShipmentID: "SH-" + s,
			Facility: cfs, Packs: 6, PackageType: "CT", Condition: phyapi.ConditionGood, Evidence: evidence("CFS-WMS", "GRN-"+s, 14), OccurredAt: hours(14)}))
		phyOK(phyModule.RecordCargoMeasurement(ctx, phyapi.RecordCargoMeasurement{CommandID: "weigh-" + s, TenantID: tenant, ActorID: "cfs-1", LotID: lot,
			ExpectedRevision: 1, GrossWeight: kg(weights[s]), Volume: cmq(volumes[s]), Method: phyapi.MethodScale, Evidence: evidence("CFS-SCALE", "W-"+s, 14), OccurredAt: hours(14)}))
		phyOK(phyModule.CompleteReceipt(ctx, phyapi.CompleteReceipt{CommandID: "complete-" + s, TenantID: tenant, ActorID: "cfs-1", LotID: lot,
			ExpectedRevision: 2, OccurredAt: hours(14)}))
	}
	// two stuffings, one per container: X-1 takes all of A and 4 packs of C, X-2 all of B and the other 2 packs of C
	revision := map[string]int64{}
	for i, x := range []string{"X-1", "X-2"} {
		phyOK(phyModule.OpenLoadExecution(ctx, phyapi.OpenLoadExecution{CommandID: "stuff-" + x, TenantID: tenant, ActorID: "cfs-1", ExecutionID: x,
			ConsolidationID: "C-1", Kind: phyapi.ExecutionStuffing, Facility: cfs, LoadUnitNo: i + 1, OccurredAt: hours(15)}))
		phyOK(phyModule.StartLoading(ctx, phyapi.StartLoading{CommandID: "start-" + x, TenantID: tenant, ActorID: "cfs-1", ExecutionID: x, ExpectedRevision: 1, OccurredAt: hours(15)}))
		revision[x] = 2
	}
	basis := func() phyapi.PhysicalCostBasis {
		t.Helper()
		b, err := phyModule.GetPhysicalCostBasis(ctx, phyapi.GetPhysicalCostBasis{TenantID: tenant, ConsolidationID: "C-1"})
		must("PHY's cost basis", err)
		return b
	}
	load := func(x, shipment string, packs int64) {
		t.Helper()
		phyOK(phyModule.RecordCargoLoaded(ctx, phyapi.RecordCargoLoaded{CommandID: "load-" + x + "-" + shipment, TenantID: tenant, ActorID: "cfs-1", ExecutionID: x,
			ExpectedRevision: revision[x], LotID: "L-" + shipment, Packs: packs, Evidence: evidence("CFS-TALLY", "T-"+x+shipment, 15), OccurredAt: hours(15)}))
		revision[x]++
	}
	seal := func(x string) {
		t.Helper()
		phyOK(phyModule.CloseLoadExecution(ctx, phyapi.CloseLoadExecution{CommandID: "seal-" + x, TenantID: tenant, ActorID: "cfs-1", ExecutionID: x,
			ExpectedRevision: revision[x], Evidence: evidence(phyapi.EvidenceSeal, "SEAL-"+x, 15), OccurredAt: hours(15)}))
		revision[x]++
	}
	apportion := func(id string, lines ...int) error {
		t.Helper()
		_, err := finModule.ApportionConsolCosts(ctx, finapi.ApportionConsolCosts{CommandID: id, TenantID: tenant, ActorID: "fin-1", PoolID: "P-1",
			ExpectedRevision: pool().Revision, LineNos: lines, OccurredAt: hours(17)})
		return err
	}
	refused := func(what, id string) {
		t.Helper()
		stored := len(pool().Apportionments)
		for i, line := range []int{1, 2} {
			err := apportion(id+"-"+string(rune('a'+i)), line)
			if err == nil || !strings.Contains(err.Error(), "FIN_POOL_NO_BASIS") || !strings.Contains(err.Error(), "sealed") {
				t.Fatalf("%s: line %d: %v", what, line, err)
			}
		}
		if n := len(pool().Apportionments); n != stored {
			t.Fatalf("%s: %d apportionments stored, %d before", what, n, stored)
		}
	}
	// SH-A all in the 20-foot container, SH-B all in the 40-foot one, SH-C 4 of its 6 packs in the 20-foot one
	load("X-1", "A", 6)
	load("X-2", "B", 6)
	load("X-1", "C", 4)
	b := basis()
	if b.Houses[2].LoadedPacks != 4 || len(b.Houses[2].Units) != 1 || b.Houses[2].Units[0].UnitNo != 1 || b.LoadUnits[0].Status != phyapi.UnitStatusInUse || b.LoadUnits[1].Status != phyapi.UnitStatusInUse {
		t.Fatalf("PHY's cost basis after the loads: %+v", b)
	}
	refused("before any seal", "fin-unsealed")
	// the line by SHIPMENT is accepted in between: the rule is the container methods' only
	if err := apportion("fin-shipment", 3); err != nil {
		t.Fatalf("a line by SHIPMENT with the containers not sealed: %v", err)
	}
	seal("X-1")
	if b = basis(); b.LoadUnits[0].Status != phyapi.UnitStatusClosed || b.LoadUnits[1].Status != phyapi.UnitStatusInUse {
		t.Fatalf("the load units after the first seal: %+v", b.LoadUnits)
	}
	refused("after the 20-foot container is sealed", "fin-one-sealed")
	// the other 2 packs of SH-C go into the 40-foot container, which is then sealed
	load("X-2", "C", 2)
	seal("X-2")
	if err := apportion("fin-sealed", 1, 2); err != nil {
		t.Fatalf("with both containers sealed: %v", err)
	}
	// SH-C is in both containers: 1 : 1 : 2 containers and 10 : 20 : 30 tenths (10,001 minor each: 2,500 / 2,500 / 5,001 and
	// 1,667 / 3,334 / 5,000, as in the other test)
	last := pool().Apportionments
	one := last[len(last)-1]
	want := map[int][3]int64{1: {2_500, 2_500, 5_001}, 2: {1_667, 3_334, 5_000}}
	quantity := map[int][3]int64{1: {1, 1, 2}, 2: {10, 20, 30}}
	if one.Stage != finapi.BasisActual || len(one.Lines) != 2 {
		t.Fatalf("the apportionment: %+v", one)
	}
	for _, la := range one.Lines {
		for i, s := range la.Shares {
			if s.Amount != cny(want[la.LineNo][i]) || s.Quantity != quantity[la.LineNo][i] {
				t.Fatalf("line %d share %d: %+v", la.LineNo, i+1, s)
			}
		}
	}
	// the invoice is matched final and the pool closes
	_, err = finModule.RecordCreditorInvoice(ctx, finapi.RecordCreditorInvoice{CommandID: "fin-invoice", TenantID: tenant, ActorID: "ap-1", InvoiceID: "CI-LINE-1",
		LegalEntityID: "LE", CreditorPartyID: "line-1", SupplierInvoiceNo: "LINE-INV-2611-3", InvoiceDate: hours(19).Format(time.DateOnly), Currency: "CNY",
		Lines:      []finapi.CreditorInvoiceLine{{ChargeCode: "OCEAN_FREIGHT", Amount: 10_001}, {ChargeCode: "THC", Amount: 10_001}, {ChargeCode: "LASHING", Amount: 10_001}},
		OccurredAt: hours(19)})
	must("record the line's invoice", err)
	invoice, err := finModule.GetCreditorInvoice(ctx, finapi.GetCreditorInvoice{TenantID: tenant, InvoiceID: "CI-LINE-1"})
	must("the line's invoice", err)
	matches := make([]finapi.CreditorMatch, 3)
	for i := range matches {
		matches[i] = finapi.CreditorMatch{LineNo: i + 1, Target: finapi.AccrualRef{Kind: "POOL_COST", ID: "P-1", LineNo: i + 1}, Amount: 10_001, Final: true}
	}
	_, err = finModule.MatchCreditorInvoice(ctx, finapi.MatchCreditorInvoice{CommandID: "fin-match", TenantID: tenant, ActorID: "ap-1", InvoiceID: "CI-LINE-1",
		ExpectedRevision: invoice.Revision, Matches: matches, OccurredAt: hours(20)})
	must("match the line's invoice", err)
	_, err = finModule.CloseConsolCostPool(ctx, finapi.CloseConsolCostPool{CommandID: "fin-close", TenantID: tenant, ActorID: "fin-1", PoolID: "P-1",
		ExpectedRevision: pool().Revision, OccurredAt: hours(21)})
	must("close the pool", err)
	if p := pool(); p.State != finapi.PoolClosed {
		t.Fatalf("the pool: %+v", p)
	}
}
