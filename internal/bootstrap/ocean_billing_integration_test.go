//go:build integration

package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
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

// The ocean line from two orders to the job's profit with the real ORG, RATE, COM, FUL, PHY, BOOK, SEA, DOC and FIN
// (AIR is built only because DOC is). Two customers' LCL orders are consolidated in one own container, booked with a
// shipping line, stuffed, sealed, weighed and gated in, loaded on board and finalized in OceanDocFacts (stage 1). DOC
// issues shipper A's house bill of lading as three originals (2) and shipper B's house sea waybill (3), and records
// the carrier's master bill (4). FIN bills the jobs of the two shipments on those documents: shipment A's job is
// refused a debit note while the bill is a draft and billed once it is issued, and its profit follows the quote (5);
// after the originals are surrendered at origin a further debit note still finds the bill (6); shipment B is billed
// on its waybill (7), and once DOC cancels the waybill a further note is refused and the note issued stays (8). A
// third shipment, with a declared volume and an order under an agreement, is rated by FIN from RATE's cards by weight or
// measurement and per cubic metre, and its job is billed on an HBL released EXPRESS (9).
func TestTheOceanLineIsBilledWithEveryContext(t *testing.T) {
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
	tenant := uniqueTenant("cargo-ocean-bill-")
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
	rejected := func(what string, err error, code string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), code) {
			t.Fatalf("%s: want a refusal with %s, got %v", what, code, err)
		}
	}
	evidence := func(source, ref string, h int) phyapi.Evidence {
		return phyapi.Evidence{Source: source, Reference: ref, OccurredAt: hours(h)}
	}
	movement := func() seaapi.OceanMovement {
		t.Helper()
		m, err := seaModule.GetOceanMovement(ctx, seaapi.GetOceanMovement{TenantID: tenant, MovementID: "OM-1"})
		must("movement", err)
		return m
	}

	// Stage 1: the customers' orders, the parties on them and the movement up to FINAL facts.
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
	contexts.planSea(t, "party-a", "A", comapi.ContainerLCL, fulapi.ContainerLCL)
	contexts.planSea(t, "party-b", "B", comapi.ContainerLCL, fulapi.ContainerLCL)
	// a third customer's order of 800 kg and 2 m3, under an agreement: such an order is not priced by a quote, its
	// shipment is rated from the cards. The agreement is the customer's SELL card for SEA and LCL and the line's BUY
	// card: OCEAN_FREIGHT per FREIGHT_TONNE (W/M) and CFS_HANDLING per CBM.
	rateCard := func(id, side, scope, party string, freight, handling int64) error {
		entry := rateapi.RateEntry{Mode: rateapi.ModeSea, ContainerMode: rateapi.ContainerLCL, Origin: "CNSHA", Destination: "DEHAM", ServiceLevel: "STANDARD",
			Commodity: rateapi.CommodityGeneral, Lines: []rateapi.RateLine{{ChargeCode: "OCEAN_FREIGHT", Currency: "USD", Calculator: rateapi.CalcPerUnit,
				Unit: rateapi.UnitFreightTonne, Rate: freight}, {ChargeCode: "CFS_HANDLING", Currency: "USD", Calculator: rateapi.CalcPerUnit, Unit: rateapi.UnitCBM,
				Rate: handling}}}
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
		if err := rateCard("C-C", rateapi.SideSell, rateapi.ScopeCustomer, "party-c", 5_000, 2_500); err != nil {
			return err
		}
		if err := rateCard("S-LINE", rateapi.SideBuy, rateapi.ScopeSupplier, "line-1", 2_500, 1_500); err != nil {
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

	// PHY: both lots received and weighed at the CFS, stuffed into the 22G1 and sealed; FUL takes the evidence of both
	// houses and SEA refreshes them
	c, err := phyModule.GetConsolidation(ctx, phyapi.GetConsolidation{TenantID: tenant, ConsolidationID: "C-1"})
	must("consolidation", err)
	phyOK(phyModule.CloseConsolidation(ctx, phyapi.CloseConsolidation{CommandID: "close", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ExpectedRevision: c.Revision, OccurredAt: hours(13)}))
	cfs := phyapi.Facility{Location: c.Current.Origin, Code: "SHA-CFS1"}
	volumes := map[string]kernel.Quantity{"C": {Unit: "CMQ", Amount: 2_000_000}}
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

	// FIN, before DOC issues anything: the legal entity's policy with its estimate and invoice rates, the jobs of the
	// two shipments priced from the orders' quotes, and what a debit note needs besides a document: the tax treatments
	// of the charge codes, the debit note series and the debtors' billing profiles and credit terms.
	finPolicyRevision := int64(1)
	policyStep := func(name string) func(finapi.Receipt, error) {
		return func(r finapi.Receipt, err error) {
			t.Helper()
			must(name, err)
			finPolicyRevision = r.NewRevision
		}
	}
	_, err = finModule.OpenFinancePolicy(ctx, finapi.OpenFinancePolicy{CommandID: "fin-policy", TenantID: tenant, ActorID: "acct-1", LegalEntityID: "LE",
		FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: hours(21)})
	must("finance policy", err)
	for _, x := range []struct {
		kind   string
		rate   int64
		source string
	}{{finapi.RateEstimate, 71_234, "CFETS central parity"}, {finapi.RateInvoice, 72_000, "bank selling rate"}} {
		policyStep("rate " + x.kind)(finModule.RecordExchangeRate(ctx, finapi.RecordExchangeRate{CommandID: "fin-rate-" + x.kind, TenantID: tenant, ActorID: "acct-1",
			LegalEntityID: "LE", ExpectedRevision: finPolicyRevision, RateType: x.kind, From: "USD", Rate: x.rate, Scale: 4, FromDigits: 2, ToDigits: 2, ValidFrom: at,
			ValidUntil: at.AddDate(0, 1, 0), Source: x.source, OccurredAt: hours(21)}))
	}
	taxFrom := at.AddDate(0, 0, -1).Format(time.DateOnly)
	for _, x := range []struct {
		code, treatment string
		bp              int
	}{{"OCEAN_FREIGHT", finapi.TaxExemptIntlFreight, 0}, {"DOC_FEE", finapi.TaxTaxable, 600}, {"CFS_HANDLING", finapi.TaxTaxable, 600}} {
		policyStep("tax " + x.code)(finModule.SetChargeTaxTreatment(ctx, finapi.SetChargeTaxTreatment{CommandID: "fin-tax-" + x.code, TenantID: tenant, ActorID: "tax-1",
			LegalEntityID: "LE", ExpectedRevision: finPolicyRevision, ChargeCode: x.code, Treatment: x.treatment, RateBP: x.bp, EffectiveFrom: taxFrom, OccurredAt: hours(21)}))
	}
	policyStep("series")(finModule.RegisterInvoiceSeries(ctx, finapi.RegisterInvoiceSeries{CommandID: "fin-series-dn", TenantID: tenant, ActorID: "acct-1",
		LegalEntityID: "LE", ExpectedRevision: finPolicyRevision, Kind: finapi.KindDebitNote, Template: "SEADN{seq:6}", FirstNumber: 1, OccurredAt: hours(21)}))
	for _, s := range []string{"a", "b", "c"} {
		_, err = finModule.SetBillingProfile(ctx, finapi.SetBillingProfile{CommandID: "fin-profile-" + s, TenantID: tenant, ActorID: "bill-1", DebtorPartyID: "party-" + s,
			BuyerKind: finapi.BuyerForeign, EvidenceRef: "account opening form AO-2026-" + s, OccurredAt: hours(21)})
		must("billing profile of party-"+s, err)
		_, err = finModule.SetCreditTerms(ctx, finapi.SetCreditTerms{CommandID: "fin-terms-" + s, TenantID: tenant, ActorID: "credit-1", LegalEntityID: "LE",
			DebtorPartyID: "party-" + s, Limit: kernel.Money{Currency: "CNY", Minor: 10_000_000}, PaymentDays: 30, PaymentBasis: finapi.TermsFromMonthEnd, OccurredAt: hours(21)})
		must("credit terms of party-"+s, err)
	}
	jobRevision := map[string]int64{}
	for _, s := range []string{"A", "B"} {
		job := "JOB-" + s
		r, err := finModule.OpenFreightJob(ctx, finapi.OpenFreightJob{CommandID: "fin-job-" + s, TenantID: tenant, ActorID: "fin-1", JobID: job, ShipmentID: "SH-" + s,
			OccurredAt: hours(21)})
		must("open "+job, err)
		if r, err = finModule.ApplyPricingBasis(ctx, finapi.ApplyPricingBasis{CommandID: "fin-quote-" + s, TenantID: tenant, ActorID: "fin-1", JobID: job,
			ExpectedRevision: r.NewRevision, QuoteID: "Q-" + s, OrderID: "O-" + s, OccurredAt: hours(21)}); err != nil {
			t.Fatalf("the pricing basis of %s from the accepted quote: %v", job, err)
		}
		jobRevision[job] = r.NewRevision
	}

	debit := func(id, job string, h int, lines ...int) finapi.IssueDebitNote {
		return finapi.IssueDebitNote{CommandID: "fin-" + id, TenantID: tenant, ActorID: "bill-1", JobID: job, ExpectedRevision: jobRevision[job], InvoiceID: id,
			DebtorPartyID: "party-" + strings.ToLower(job[len(job)-1:]), LineNos: lines, Currency: "CNY", InvoiceDate: hours(h).Format(time.DateOnly), OccurredAt: hours(h)}
	}
	issueNote := func(cmd finapi.IssueDebitNote) finapi.Receipt {
		t.Helper()
		r, err := finModule.IssueDebitNote(ctx, cmd)
		if err != nil {
			t.Fatalf("debit note %s: %v", cmd.InvoiceID, err)
		}
		jobRevision[cmd.JobID] = r.NewRevision
		return r
	}
	// Stage 2: DOC, shipment A. The authority for the HBL; the draft, released as three originals; the approval by a
	// second person. FIN is asked for a debit note while the bill is still a draft (stage 5), then COM locks the order's
	// parties and DOC issues the bill on FIN's credit decision. The bill carries SEA's number and the real facts.
	authority := func(kind string) {
		t.Helper()
		_, err := docModule.RecordIssuanceAuthority(ctx, docapi.RecordIssuanceAuthority{CommandID: "auth-" + kind, TenantID: tenant, ActorID: "legal-1",
			AuthorityID: "AUTH-" + kind, LegalEntityID: "LE", Kind: kind, IssuePlace: "CNSHG", Qualification: kind + " filing", Validity: kernel.ValidityInterval{From: at},
			EvidenceRef: "filings/" + kind, OccurredAt: hours(21)})
		must("authority "+kind, err)
	}
	authority(docapi.KindHBL)
	_, err = docModule.DraftTransportDocument(ctx, docapi.DraftTransportDocument{CommandID: "draft-hbl-a", TenantID: tenant, ActorID: "doc-1", DocumentID: "DOC-HBL-A",
		Kind: docapi.KindHBL, MovementID: "OM-1", ShipmentID: "SH-A", PaymentIndicator: docapi.PaymentPrepaid, Release: docapi.ReleaseOriginals, Originals: 3,
		OccurredAt: hours(22)})
	must("draft HBL", err)
	_, err = finModule.IssueDebitNote(ctx, debit("DN-A", "JOB-A", 26, 1))
	rejected("stage 5: a debit note while the HBL is a bare draft", err, "FIN_NO_ISSUED_DOCUMENT")
	_, err = docModule.ApproveTransportDocument(ctx, docapi.ApproveTransportDocument{CommandID: "approve-hbl-a", TenantID: tenant, ActorID: "doc-2", DocumentID: "DOC-HBL-A",
		ExpectedRevision: 1, OccurredAt: hours(22)})
	must("approve HBL", err)

	// Stage 5, first half: FIN refuses a debit note while DOC holds no issued document of the shipment
	_, err = finModule.IssueDebitNote(ctx, debit("DN-A", "JOB-A", 26, 1))
	rejected("stage 5: a debit note while the HBL is approved and not issued", err, "FIN_NO_ISSUED_DOCUMENT")
	if job, err := finModule.GetFreightJob(ctx, finapi.GetFreightJob{TenantID: tenant, JobID: "JOB-A"}); err != nil || job.Kind != finapi.KindSeaExportLCLHouse ||
		job.State != finapi.JobOpen || job.Revision != jobRevision["JOB-A"] || len(job.Notes) != 0 || job.ShipmentID != "SH-A" {
		t.Fatalf("stage 5: the job of SH-A opens as a SEA job and stays untouched by the refusal: %+v %v", job, err)
	}

	issueHBL := func(command string) error {
		_, err := docModule.IssueTransportDocument(ctx, docapi.IssueTransportDocument{CommandID: command, TenantID: tenant, ActorID: "doc-2", DocumentID: "DOC-HBL-A",
			ExpectedRevision: 2, OccurredAt: hours(24)})
		return err
	}
	if err = issueHBL("issue-hbl-unlocked"); !errors.Is(err, docapi.ErrRejected) {
		t.Fatalf("stage 2: issuing before the client locks the parties: %v", err)
	}
	_, err = comModule.LockOrderParties(ctx, comapi.LockOrderParties{CommandID: "lock-hbl-a", TenantID: tenant, ActorID: "doc-2", OrderID: "O-A",
		ExpectedRevision: orderRevision("O-A"), HolderID: "DOC-HBL-A", OccurredAt: hours(23)})
	must("lock O-A", err)
	must("issue HBL", issueHBL("issue-hbl-a"))
	hblDoc, err := docModule.GetTransportDocument(ctx, docapi.GetTransportDocument{TenantID: tenant, DocumentID: "DOC-HBL-A"})
	must("HBL", err)
	if hblDoc.Kind != docapi.KindHBL || hblDoc.State != docapi.StateIssued || hblDoc.Number != "SHAL0000001" || hblDoc.CurrentVersion != 1 || hblDoc.Revision != 3 ||
		hblDoc.MovementID != "OM-1" || hblDoc.ConsolidationID != "C-1" || hblDoc.ShipmentID != "SH-A" || hblDoc.LegalEntityID != "LE" ||
		hblDoc.Versions[0].CreditBasis != docapi.CreditOK || hblDoc.Versions[0].CreditOverrideReason != "" || hblDoc.Versions[0].AuthorityID != "AUTH-HBL" ||
		!hblDoc.Versions[0].IssuedAt.Equal(hours(24)) || hblDoc.Versions[0].SourceFactsVersion != 1 {
		t.Fatalf("stage 2: the issued HBL: %+v", hblDoc)
	}
	hbl, err := docModule.GetDocumentVersion(ctx, docapi.GetDocumentVersion{TenantID: tenant, DocumentID: "DOC-HBL-A"})
	must("HBL version", err)
	if d := hbl.Doc; hbl.Status != docapi.VersionIssued || d.Kind != docapi.KindHBL || d.Number != "SHAL0000001" || d.ReleaseType != docapi.ReleaseOriginals || d.Originals != 3 ||
		d.IssuerCapacity != "NVOCC_CARRIER" || d.MasterBill != "LINU2611000457" || d.Vessel != "EVER GIVEN" || d.Voyage != "0123W" || d.PortOfLoading != "CNSHG" ||
		d.PortOfDischarge != "DEHAM" || !d.OnBoardAt.Equal(hours(20)) || d.ShipmentID != "SH-A" || d.ConsolidationID != "C-1" || d.Shipper.PartyID != "shipper-a" ||
		d.Consignee.PartyID != "consignee-de" || d.Consignee.Address == nil || d.Consignee.Address.ORGAddressID != "HQ-consignee-de" || d.Issuer.LegalEntityID != "LE" ||
		d.Source.Version != 1 || d.Source.SHA256 != facts.SHA256 || d.Source.MovementID != "OM-1" || len(d.Orders) != 1 || d.Orders[0].OrderID != "O-A" ||
		d.Cargo.Pieces != 6 || d.Cargo.GrossGrams != 312_000 || len(d.Containers) != 1 || d.Containers[0] != (docapi.ShippedContainer{Number: "CSQU3054383", SizeType: "22G1",
		SealNumber: "CN-SEAL-77120", Packs: 6, GrossGrams: 312_000}) {
		t.Fatalf("stage 2: the issued HBL's content: %+v", d)
	}
	if originals, err := docModule.ListOriginals(ctx, docapi.ListOriginals{TenantID: tenant, DocumentID: "DOC-HBL-A"}); err != nil || len(originals) != 3 {
		t.Fatalf("stage 2: three originals: %+v %v", originals, err)
	}

	// Stage 5, second half: with the HBL issued, the note succeeds. It carries the bill's id, version, number, the
	// master bill's number as master number and the time DOC issued the bill at; the job's profit follows the quote's
	// line (USD 1,000.00 sell and 500.00 cost at the estimate rate 7.1234); once the sell side is billed it counts at
	// the invoice rate 7.2000, as it does for an air job.
	if snapshot, err := documents.GetHouseDocumentSnapshot(ctx, docapi.GetHouseDocumentSnapshot{TenantID: tenant, ShipmentID: "SH-A"}); err != nil ||
		snapshot.DocumentID != "DOC-HBL-A" || snapshot.Kind != docapi.KindHBL || snapshot.Version != 1 || snapshot.VersionStatus != docapi.VersionIssued ||
		snapshot.Number != "SHAL0000001" || snapshot.MasterNumber != "LINU2611000457" || snapshot.ConsolidationID != "C-1" || snapshot.LegalEntityID != "LE" ||
		snapshot.Origin != "CNSHG" || snapshot.Destination != "DEHAM" || !snapshot.IssuedAt.Equal(hours(24)) || snapshot.Pieces != 6 || snapshot.GrossGrams != 312_000 {
		t.Fatalf("stage 5: DOC's snapshot of the HBL: %+v %v", snapshot, err)
	}
	profit := func(job string) finapi.JobProfit {
		t.Helper()
		p, err := finModule.GetJobProfit(ctx, finapi.GetJobProfit{TenantID: tenant, JobID: job})
		must("profit of "+job, err)
		return p
	}
	if p := profit("JOB-A"); p.Revenue.Minor != 712_340 || p.Cost.Minor != 356_170 || p.Profit.Minor != 356_170 || p.Revenue.Currency != "CNY" ||
		p.Invoiced.Minor != 0 || len(p.ByCode) != 1 || p.ByCode[0].ChargeCode != "OCEAN_FREIGHT" {
		t.Fatalf("stage 5: the profit of JOB-A before billing: %+v", p)
	}
	// a note whose business time is before the HBL's issue is refused: FIN asks DOC for the version valid at that
	// instant and DOC answers "none issued by then", which is not found. FIN's own comparison of the two times
	// (OccurredAt.Before the issue time, in application/usecases.go) is never reached, with the real DOC or with FIN's
	// stand-in, since a document DOC answers was issued at or before the instant asked about.
	_, err = finModule.IssueDebitNote(ctx, debit("DN-A", "JOB-A", 23, 1))
	rejected("stage 5: a debit note dated before the HBL's issue", err, "FIN_NO_ISSUED_DOCUMENT")
	hblIssuedAt := hours(24)
	receipt := issueNote(debit("DN-A", "JOB-A", 26, 1))
	if receipt.Number != "SEADN000001" || receipt.SubjectID != "JOB-A" {
		t.Fatalf("stage 5: the debit note: %+v", receipt)
	}
	noteA, err := finModule.GetDebtorInvoice(ctx, finapi.GetDebtorInvoice{TenantID: tenant, InvoiceID: "DN-A"})
	// USD 1,000.00 at the invoice rate 7.2000
	if err != nil || noteA.Kind != finapi.KindDebitNote || noteA.Total.Minor != 720_000 || noteA.ShipmentID != "SH-A" || noteA.DebtorPartyID != "party-a" ||
		noteA.Document != (finapi.DocumentRef{DocumentID: "DOC-HBL-A", Version: 1, Number: "SHAL0000001", MasterNumber: "LINU2611000457", IssuedAt: hblIssuedAt}) ||
		len(noteA.Lines) != 1 || noteA.Lines[0].FX == nil || noteA.Lines[0].FX.RateType != finapi.RateInvoice {
		t.Fatalf("stage 5: the debit note on the HBL: %+v %v", noteA, err)
	}
	if p := profit("JOB-A"); p.Revenue.Minor != 720_000 || p.Cost.Minor != 356_170 || p.Profit.Minor != 363_830 || p.Invoiced.Minor != 720_000 {
		t.Fatalf("stage 5: the profit of JOB-A after billing: %+v", p)
	}

	// Stage 3: DOC, shipment B: its own authority, a house sea waybill, approved and issued under the lock of its order
	authority(docapi.KindHouseSeaWaybill)
	_, err = docModule.DraftTransportDocument(ctx, docapi.DraftTransportDocument{CommandID: "draft-hsw-b", TenantID: tenant, ActorID: "doc-1", DocumentID: "DOC-HSW-B",
		Kind: docapi.KindHouseSeaWaybill, MovementID: "OM-1", ShipmentID: "SH-B", PaymentIndicator: docapi.PaymentCollect, Release: docapi.ReleaseSeaWaybill,
		OccurredAt: hours(22)})
	must("draft sea waybill", err)
	_, err = docModule.ApproveTransportDocument(ctx, docapi.ApproveTransportDocument{CommandID: "approve-hsw-b", TenantID: tenant, ActorID: "doc-2", DocumentID: "DOC-HSW-B",
		ExpectedRevision: 1, OccurredAt: hours(22)})
	must("approve sea waybill", err)
	_, err = finModule.IssueDebitNote(ctx, debit("DN-B", "JOB-B", 26, 1))
	rejected("stage 7: a debit note while the sea waybill is approved and not issued", err, "FIN_NO_ISSUED_DOCUMENT")
	_, err = comModule.LockOrderParties(ctx, comapi.LockOrderParties{CommandID: "lock-hsw-b", TenantID: tenant, ActorID: "doc-2", OrderID: "O-B",
		ExpectedRevision: orderRevision("O-B"), HolderID: "DOC-HSW-B", OccurredAt: hours(23)})
	must("lock O-B", err)
	_, err = docModule.IssueTransportDocument(ctx, docapi.IssueTransportDocument{CommandID: "issue-hsw-b", TenantID: tenant, ActorID: "doc-2", DocumentID: "DOC-HSW-B",
		ExpectedRevision: 2, OccurredAt: hours(24)})
	must("issue sea waybill", err)
	hsw, err := docModule.GetDocumentVersion(ctx, docapi.GetDocumentVersion{TenantID: tenant, DocumentID: "DOC-HSW-B"})
	must("sea waybill version", err)
	if d := hsw.Doc; hsw.Status != docapi.VersionIssued || d.Kind != docapi.KindHouseSeaWaybill || d.Number != "SHAL0000002" || d.ReleaseType != docapi.ReleaseSeaWaybill ||
		d.Originals != 0 || d.MasterBill != "LINU2611000457" || d.ShipmentID != "SH-B" || d.Shipper.PartyID != "shipper-b" || d.Cargo.GrossGrams != 288_000 ||
		len(d.Containers) != 1 || d.Containers[0].Number != "CSQU3054383" || d.Containers[0].Packs != 6 || d.PaymentIndicator != docapi.PaymentCollect {
		t.Fatalf("stage 3: the issued sea waybill: %+v", d)
	}
	if h, err := docModule.GetTransportDocument(ctx, docapi.GetTransportDocument{TenantID: tenant, DocumentID: "DOC-HSW-B"}); err != nil || h.State != docapi.StateIssued ||
		h.Versions[0].CreditBasis != docapi.CreditOK || h.Versions[0].AuthorityID != "AUTH-HOUSE_SEA_WAYBILL" || h.Revision != 3 {
		t.Fatalf("stage 3: the sea waybill's issue: %+v %v", h, err)
	}
	if docs, err := docModule.ListDocumentsForShipment(ctx, docapi.ListDocumentsForShipment{TenantID: tenant, ShipmentID: "SH-B"}); err != nil || len(docs) != 1 ||
		docs[0].DocumentID != "DOC-HSW-B" {
		t.Fatalf("stage 3: the documents of SH-B: %+v %v", docs, err)
	}

	// Stage 9 needs shipment C's house document: an HBL released EXPRESS, with no originals, under the same authority
	_, err = docModule.DraftTransportDocument(ctx, docapi.DraftTransportDocument{CommandID: "draft-hbl-c", TenantID: tenant, ActorID: "doc-1", DocumentID: "DOC-HBL-C",
		Kind: docapi.KindHBL, MovementID: "OM-1", ShipmentID: "SH-C", PaymentIndicator: docapi.PaymentPrepaid, Release: docapi.ReleaseExpress, OccurredAt: hours(22)})
	must("draft HBL of C", err)
	_, err = docModule.ApproveTransportDocument(ctx, docapi.ApproveTransportDocument{CommandID: "approve-hbl-c", TenantID: tenant, ActorID: "doc-2", DocumentID: "DOC-HBL-C",
		ExpectedRevision: 1, OccurredAt: hours(22)})
	must("approve HBL of C", err)
	_, err = comModule.LockOrderParties(ctx, comapi.LockOrderParties{CommandID: "lock-hbl-c", TenantID: tenant, ActorID: "doc-2", OrderID: "O-C",
		ExpectedRevision: orderRevision("O-C"), HolderID: "DOC-HBL-C", OccurredAt: hours(23)})
	must("lock O-C", err)
	_, err = docModule.IssueTransportDocument(ctx, docapi.IssueTransportDocument{CommandID: "issue-hbl-c", TenantID: tenant, ActorID: "doc-2", DocumentID: "DOC-HBL-C",
		ExpectedRevision: 2, OccurredAt: hours(24)})
	must("issue HBL of C", err)
	hblC, err := docModule.GetDocumentVersion(ctx, docapi.GetDocumentVersion{TenantID: tenant, DocumentID: "DOC-HBL-C"})
	must("HBL of C", err)
	if d := hblC.Doc; d.Number != "SHAL0000003" || d.ReleaseType != docapi.ReleaseExpress || d.Originals != 0 || d.Cargo.GrossGrams != 800_000 ||
		d.Cargo.VolumeCm3 != 2_000_000 || d.MasterBill != "LINU2611000457" {
		t.Fatalf("stage 9: the HBL of C: %+v", d)
	}

	// Stage 4: DOC records the carrier's master bill from the real facts: an MBL, the line as issuer in its capacity
	// as carrier, its number and the party SEA published, verified by a second person
	carrierEvidence := docapi.CarrierEvidence{SHA256: strings.Repeat("9c", 32), StorageID: "mail/mbl.pdf", MediaType: "application/pdf", CapturedAt: hours(24)}
	_, err = docModule.RecordCarrierDocument(ctx, docapi.RecordCarrierDocument{CommandID: "record-mbl", TenantID: tenant, ActorID: "clerk-1", DocumentID: "DOC-MBL-1",
		MovementID: "OM-1", Release: docapi.ReleaseOriginals, Originals: 3, IssuePlace: "CNSHG", IssuedOn: hours(20).Format(time.DateOnly), Evidence: carrierEvidence,
		OccurredAt: hours(25)})
	must("record the carrier's bill", err)
	mbl, err := docModule.GetCarrierDocument(ctx, docapi.GetCarrierDocument{TenantID: tenant, DocumentID: "DOC-MBL-1"})
	must("carrier document", err)
	if mbl.Kind != docapi.KindMBL || mbl.IssuerCapacity != docapi.CapacityCarrier || mbl.Number != "LINU2611000457" || mbl.CarrierPartyID != "line-1" ||
		mbl.CarrierPartyRevision != facts.CarrierPartyRevision || mbl.CarrierRole != seaapi.RoleCarrier || mbl.MovementID != "OM-1" || mbl.ConsolidationID != "C-1" ||
		mbl.LegalEntityID != "LE" || mbl.Release != docapi.ReleaseOriginals || mbl.Originals != 3 || mbl.IssuePlace != "CNSHG" || mbl.State != docapi.CarrierIssued ||
		mbl.SourceVersion != 1 || mbl.SourceSHA256 != facts.SHA256 || mbl.Verified || mbl.RecordedBy != "clerk-1" || mbl.Revision != 1 {
		t.Fatalf("stage 4: the recorded carrier's bill: %+v", mbl)
	}
	var content map[string]any
	if err = json.Unmarshal(mbl.Content, &content); err != nil || content["kind"] != "MBL" || content["number"] != "LINU2611000457" || content["originals"] != float64(3) {
		t.Fatalf("stage 4: the recorded content %s: %v", mbl.Content, err)
	}
	if found, err := docModule.FindCarrierDocumentByMovement(ctx, docapi.FindCarrierDocumentByMovement{TenantID: tenant, MovementID: "OM-1"}); err != nil ||
		found.DocumentID != "DOC-MBL-1" {
		t.Fatalf("stage 4: the carrier's bill by movement: %+v %v", found, err)
	}
	if _, err = docModule.VerifyCarrierDocument(ctx, docapi.VerifyCarrierDocument{CommandID: "verify-mbl-self", TenantID: tenant, ActorID: "clerk-1", DocumentID: "DOC-MBL-1",
		ExpectedRevision: 1, OccurredAt: hours(26)}); !errors.Is(err, docapi.ErrRejected) {
		t.Fatalf("stage 4: the person who recorded the bill verifies it: %v", err)
	}
	_, err = docModule.VerifyCarrierDocument(ctx, docapi.VerifyCarrierDocument{CommandID: "verify-mbl", TenantID: tenant, ActorID: "clerk-2", DocumentID: "DOC-MBL-1",
		ExpectedRevision: 1, OccurredAt: hours(26)})
	must("verify the carrier's bill", err)
	if mbl, err = docModule.GetCarrierDocument(ctx, docapi.GetCarrierDocument{TenantID: tenant, DocumentID: "DOC-MBL-1"}); err != nil || !mbl.Verified || mbl.VerifiedBy != "clerk-2" ||
		mbl.Revision != 2 {
		t.Fatalf("stage 4: the verified carrier's bill: %+v %v", mbl, err)
	}

	// Stage 6: the originals of the HBL are surrendered at origin. The bill is SURRENDERED and keeps its issued
	// version, so a further debit note of the job (a document fee recorded after the surrender; a second debit note,
	// not a credit note) still finds it, with the same number, master number and issue time.
	_, err = docModule.SurrenderOriginals(ctx, docapi.SurrenderOriginals{CommandID: "surrender-hbl", TenantID: tenant, ActorID: "doc-1", DocumentID: "DOC-HBL-A",
		ExpectedRevision: hblDoc.Revision, Evidence: "telex release at origin TR-1", OccurredAt: hours(27)})
	must("surrender the originals", err)
	if h, err := docModule.GetTransportDocument(ctx, docapi.GetTransportDocument{TenantID: tenant, DocumentID: "DOC-HBL-A"}); err != nil || h.State != docapi.StateSurrendered ||
		h.CurrentVersion != 1 || h.Versions[0].Status != docapi.VersionIssued {
		t.Fatalf("stage 6: the surrendered HBL: %+v %v", h, err)
	}
	receipt, err = finModule.RecordChargeLine(ctx, finapi.RecordChargeLine{CommandID: "fin-doc-fee-a", TenantID: tenant, ActorID: "fin-1", JobID: "JOB-A",
		ExpectedRevision: jobRevision["JOB-A"], ChargeCode: "DOC_FEE", Sell: &finapi.ChargeSide{PartyID: "party-a", Amount: kernel.Money{Currency: "CNY", Minor: 30_000}},
		Reason: "HBL fee", OccurredAt: hours(27)})
	must("document fee of JOB-A", err)
	jobRevision["JOB-A"] = receipt.NewRevision
	if snapshot, err := documents.GetHouseDocumentSnapshot(ctx, docapi.GetHouseDocumentSnapshot{TenantID: tenant, ShipmentID: "SH-A"}); err != nil || snapshot.Kind != docapi.KindHBL ||
		snapshot.Version != 1 || snapshot.VersionStatus != docapi.VersionIssued || snapshot.MasterNumber != "LINU2611000457" {
		t.Fatalf("stage 6: DOC's snapshot of the surrendered HBL: %+v %v", snapshot, err)
	}
	receipt = issueNote(debit("DN-A2", "JOB-A", 28, 2))
	if receipt.Number != "SEADN000002" {
		t.Fatalf("stage 6: the second debit note: %+v", receipt)
	}
	noteA2, err := finModule.GetDebtorInvoice(ctx, finapi.GetDebtorInvoice{TenantID: tenant, InvoiceID: "DN-A2"})
	if err != nil || noteA2.Total.Minor != 30_000 || noteA2.Document != noteA.Document || noteA2.Document.Version != 1 {
		t.Fatalf("stage 6: the second note on the surrendered HBL: %+v %v", noteA2, err)
	}
	if again, err := finModule.GetDebtorInvoice(ctx, finapi.GetDebtorInvoice{TenantID: tenant, InvoiceID: "DN-A"}); err != nil || !reflect.DeepEqual(again, noteA) {
		t.Fatalf("stage 6: the first note after the second: %+v %v", again, err)
	}

	// Stage 7: shipment B's job is billed on the house sea waybill: its number, under the same master bill
	receipt = issueNote(debit("DN-B", "JOB-B", 28, 1))
	if receipt.Number != "SEADN000003" || receipt.SubjectID != "JOB-B" {
		t.Fatalf("stage 7: the debit note: %+v", receipt)
	}
	noteB, err := finModule.GetDebtorInvoice(ctx, finapi.GetDebtorInvoice{TenantID: tenant, InvoiceID: "DN-B"})
	if err != nil || noteB.Total.Minor != 720_000 || noteB.ShipmentID != "SH-B" || noteB.DebtorPartyID != "party-b" ||
		noteB.Document != (finapi.DocumentRef{DocumentID: "DOC-HSW-B", Version: 1, Number: "SHAL0000002", MasterNumber: "LINU2611000457", IssuedAt: hours(24)}) {
		t.Fatalf("stage 7: the debit note on the sea waybill: %+v %v", noteB, err)
	}
	if snapshot, err := documents.GetHouseDocumentSnapshot(ctx, docapi.GetHouseDocumentSnapshot{TenantID: tenant, ShipmentID: "SH-B"}); err != nil ||
		snapshot.Kind != docapi.KindHouseSeaWaybill || snapshot.Number != "SHAL0000002" {
		t.Fatalf("stage 7: DOC's snapshot of the sea waybill: %+v %v", snapshot, err)
	}
	// a further charge line to bill, so that the refusal of stage 8 is the document's
	receipt, err = finModule.RecordChargeLine(ctx, finapi.RecordChargeLine{CommandID: "fin-doc-fee-b", TenantID: tenant, ActorID: "fin-1", JobID: "JOB-B",
		ExpectedRevision: jobRevision["JOB-B"], ChargeCode: "DOC_FEE", Sell: &finapi.ChargeSide{PartyID: "party-b", Amount: kernel.Money{Currency: "CNY", Minor: 30_000}},
		Reason: "sea waybill fee", OccurredAt: hours(28)})
	must("document fee of JOB-B", err)
	jobRevision["JOB-B"] = receipt.NewRevision

	// Stage 8: DOC cancels the sea waybill (it has no originals; the client then releases the order's lock in COM). A
	// further note of shipment B is refused, and the note issued before stays as it was.
	_, err = docModule.CancelTransportDocument(ctx, docapi.CancelTransportDocument{CommandID: "cancel-hsw-b", TenantID: tenant, ActorID: "doc-1", DocumentID: "DOC-HSW-B",
		ExpectedRevision: 3, Reason: "issued for the wrong shipper", OccurredAt: hours(29)})
	must("cancel the sea waybill", err)
	_, err = comModule.ReleaseOrderPartyLock(ctx, comapi.ReleaseOrderPartyLock{CommandID: "release-hsw-b", TenantID: tenant, ActorID: "doc-2", OrderID: "O-B",
		ExpectedRevision: orderRevision("O-B"), HolderID: "DOC-HSW-B", OccurredAt: hours(29)})
	must("release O-B", err)
	if h, err := docModule.GetTransportDocument(ctx, docapi.GetTransportDocument{TenantID: tenant, DocumentID: "DOC-HSW-B"}); err != nil || h.State != docapi.StateCancelled {
		t.Fatalf("stage 8: the cancelled sea waybill: %+v %v", h, err)
	}
	if _, err = documents.GetHouseDocumentSnapshot(ctx, docapi.GetHouseDocumentSnapshot{TenantID: tenant, ShipmentID: "SH-B"}); !errors.Is(err, docapi.ErrNotFound) {
		t.Fatalf("stage 8: DOC's snapshot of a cancelled waybill: %v", err)
	}
	_, err = finModule.IssueDebitNote(ctx, debit("DN-B2", "JOB-B", 30, 2))
	rejected("stage 8: a debit note on a cancelled sea waybill", err, "FIN_NO_ISSUED_DOCUMENT")
	if !errors.Is(err, finapi.ErrRejected) {
		t.Fatalf("stage 8: the refusal is a business refusal: %v", err)
	}
	if again, err := finModule.GetDebtorInvoice(ctx, finapi.GetDebtorInvoice{TenantID: tenant, InvoiceID: "DN-B"}); err != nil || !reflect.DeepEqual(again, noteB) {
		t.Fatalf("stage 8: the note issued before the cancellation: %+v %v", again, err)
	}
	if job, err := finModule.GetFreightJob(ctx, finapi.GetFreightJob{TenantID: tenant, JobID: "JOB-B"}); err != nil || job.Revision != jobRevision["JOB-B"] || len(job.Notes) != 1 ||
		job.Lines[1].Sell[len(job.Lines[1].Sell)-1].Status != finapi.StatusEstimated {
		t.Fatalf("stage 8: the job of SH-B after the refusal: %+v %v", job, err)
	}
	if _, err = finModule.GetDebtorInvoice(ctx, finapi.GetDebtorInvoice{TenantID: tenant, InvoiceID: "DN-B2"}); !errors.Is(err, finapi.ErrNotFound) {
		t.Fatalf("stage 8: no second note exists: %v", err)
	}
	// Stage 9: a job of an order under an agreement is rated from the cards, by weight or measurement and per cubic
	// metre. FUL's plan of SH-C is 800 kg and 2 m3 (CMQ 2,000,000), and RATE takes 1 m3 = 1000 kg, so the weight is 0.8
	// freight tonne and the measurement 2.0, and the larger is charged: 2.0 FREIGHT_TONNE (2000 thousandths) and 2 CBM.
	//   SELL OCEAN_FREIGHT USD 50.00 x 2.0 = USD 100.00; at the estimate rate 7.1234 CNY 712.34
	//   SELL CFS_HANDLING  USD 25.00 x 2   = USD  50.00; at 7.1234 CNY 356.17
	//   BUY  OCEAN_FREIGHT USD 25.00 x 2.0 = USD  50.00; at 7.1234 CNY 356.17
	//   BUY  CFS_HANDLING  USD 15.00 x 2   = USD  30.00; at 7.1234 CNY 213.70 (213.702)
	// (the agreement is in one currency, USD, because a quote needs an exchange rate for a line in another)
	// (an order under a one-off quote is priced by its quote, and RateFreightJob refuses its job with FIN_NOT_PRICEABLE)
	driver, err := fulModule.GetShipmentCostDriver(ctx, fulapi.GetShipmentCostDriver{TenantID: tenant, ShipmentID: "SH-C"})
	if err != nil || driver.TransportMode != "SEA" || driver.GrossWeight != kg(800) || driver.Volume != (kernel.Quantity{Unit: "CMQ", Amount: 2_000_000}) ||
		driver.Origin != unlocode("CNSHG") || driver.Destination != unlocode("DEHAM") {
		t.Fatalf("stage 9: FUL's plan of SH-C: %+v %v", driver, err)
	}
	openedC, err := finModule.OpenFreightJob(ctx, finapi.OpenFreightJob{CommandID: "fin-job-C", TenantID: tenant, ActorID: "fin-1", JobID: "JOB-C", ShipmentID: "SH-C",
		OccurredAt: hours(21)})
	must("open JOB-C", err)
	ratedC, err := finModule.RateFreightJob(ctx, finapi.RateFreightJob{CommandID: "fin-rate-C", TenantID: tenant, ActorID: "fin-1", JobID: "JOB-C",
		ExpectedRevision: openedC.NewRevision, ServiceLevel: "STANDARD", OccurredAt: hours(21)})
	must("RateFreightJob of JOB-C", err)
	jobRevision["JOB-C"] = ratedC.NewRevision
	jobC, err := finModule.GetFreightJob(ctx, finapi.GetFreightJob{TenantID: tenant, JobID: "JOB-C"})
	must("JOB-C", err)
	if jobC.Kind != finapi.KindSeaExportLCLHouse || jobC.CustomerPartyID != "party-c" || len(jobC.Lines) != 2 || len(jobC.Bases) != 1 || jobC.Bases[0].Purpose != "SHIPMENT_ESTIMATE" {
		t.Fatalf("stage 9: the rated job: %+v", jobC)
	}
	type side struct {
		party  string
		amount kernel.Money
		local  kernel.Money
	}
	got := map[string][2]side{}
	lineNo := map[string]int{}
	for _, l := range jobC.Lines {
		if l.Source != finapi.SourceRated || len(l.Sell) != 1 || len(l.Cost) != 1 || l.Sell[0].Status != finapi.StatusEstimated || l.Cost[0].Status != finapi.StatusAccrued {
			t.Fatalf("stage 9: line %+v", l)
		}
		got[l.ChargeCode] = [2]side{{l.Sell[0].PartyID, l.Sell[0].Amount, l.Sell[0].Local}, {l.Cost[0].PartyID, l.Cost[0].Amount, l.Cost[0].Local}}
		lineNo[l.ChargeCode] = l.LineNo
	}
	cnyMoney := func(n int64) kernel.Money { return kernel.Money{Currency: "CNY", Minor: n} }
	if want := [2]side{{"party-c", usd(10_000), cnyMoney(71_234)}, {"line-1", usd(5_000), cnyMoney(35_617)}}; got["OCEAN_FREIGHT"] != want {
		t.Fatalf("stage 9: OCEAN_FREIGHT sell and cost: %+v", got["OCEAN_FREIGHT"])
	}
	if want := [2]side{{"party-c", usd(5_000), cnyMoney(35_617)}, {"line-1", usd(3_000), cnyMoney(21_370)}}; got["CFS_HANDLING"] != want {
		t.Fatalf("stage 9: CFS_HANDLING sell and cost: %+v", got["CFS_HANDLING"])
	}
	// the quantities FIN sent are in RATE's snapshot of the basis
	basisC, err := rateModule.GetPricingBasis(ctx, rateapi.GetPricingBasis{TenantID: tenant, BasisID: jobC.Bases[0].BasisID})
	must("the basis of JOB-C", err)
	snapshot, err := rateModule.GetRateSnapshot(ctx, rateapi.GetRateSnapshot{TenantID: tenant, SnapshotID: basisC.SnapshotID})
	must("the rate snapshot of JOB-C", err)
	if snapshot.Cargo.GrossGrams != 800_000 || snapshot.Cargo.VolumeCM3 != 2_000_000 || snapshot.Cargo.Pieces != 6 || snapshot.Mode != rateapi.ModeSea ||
		snapshot.ContainerMode != rateapi.ContainerLCL || snapshot.SubjectID != "SH-C" || len(snapshot.Lines) != 4 {
		t.Fatalf("stage 9: RATE's snapshot: %+v", snapshot)
	}
	for _, l := range snapshot.Lines {
		wantQuantity := map[string]int64{"OCEAN_FREIGHT": 2_000, "CFS_HANDLING": 2_000_000}[l.ChargeCode]
		wantUnit := map[string]string{"OCEAN_FREIGHT": rateapi.UnitFreightTonne, "CFS_HANDLING": rateapi.UnitCBM}[l.ChargeCode]
		if l.Quantity != wantQuantity || l.Unit != wantUnit {
			t.Errorf("stage 9: %s %s: %+v", l.Side, l.ChargeCode, l)
		}
	}
	// the job is billed on the HBL released EXPRESS: its number under the same master bill; USD 100.00 at the invoice
	// rate 7.2000 is CNY 720.00 and the handling USD 50.00 CNY 360.00
	receipt = issueNote(debit("DN-C", "JOB-C", 30, lineNo["OCEAN_FREIGHT"], lineNo["CFS_HANDLING"]))
	noteC, err := finModule.GetDebtorInvoice(ctx, finapi.GetDebtorInvoice{TenantID: tenant, InvoiceID: "DN-C"})
	if err != nil || receipt.Number != "SEADN000004" || noteC.Total.Minor != 108_000 || noteC.DebtorPartyID != "party-c" ||
		noteC.Document != (finapi.DocumentRef{DocumentID: "DOC-HBL-C", Version: 1, Number: "SHAL0000003", MasterNumber: "LINU2611000457", IssuedAt: hours(24)}) {
		t.Fatalf("stage 9: the debit note on the HBL released EXPRESS: %+v %+v %v", receipt, noteC, err)
	}
}

// planSeaWithVolume plans one SEA shipment SH-<suffix> as planSea does, for an LCL order of 6 cartons of the weight
// and volume given; planSea declares no volume. The order's quote is made by quote, after the customer's role is
// approved and before the order is recorded. The shipment ends PLANNED at revision 3.
func planSeaWithVolume(t *testing.T, s slice, party, suffix string, weight, volume kernel.Quantity, quote func() error) {
	t.Helper()
	ctx, tenant, at, departs := context.Background(), s.tenant, s.at, s.departs
	unlocode := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	steps := []func() error{
		func() error {
			_, err := s.org.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "role-" + suffix, TenantID: tenant, ActorID: "org-officer",
				LegalEntityID: "LE", PartyID: party, ExpectedRevision: 1, RoleCode: "CUSTOMER", ValidFrom: at, EvidenceRef: "contract", OccurredAt: at})
			return err
		},
		quote,
		func() error {
			_, err := s.com.RecordCustomerOrder(ctx, comapi.RecordCustomerOrder{CommandID: "order-" + suffix, TenantID: tenant, ActorID: "cs-1",
				OrderID: "O-" + suffix, QuoteID: "Q-" + suffix, CustomerPartyID: party, OccurredAt: at.Add(2 * time.Hour),
				Transport: comapi.Transport{Mode: comapi.ModeSea, ContainerMode: comapi.ContainerLCL},
				Lines:     []comapi.OrderLine{{Description: "Cartons of furniture parts", Packs: 6, PackageType: "CT", GrossWeight: weight, Volume: volume}}})
			return err
		},
		func() error {
			_, err := s.com.AllocateOrderToObligation(ctx, comapi.AllocateOrderToObligation{CommandID: "allocate-" + suffix, TenantID: tenant,
				ActorID: "planner-1", OrderID: "O-" + suffix, ExpectedRevision: 1, LineNo: 1, ObligationID: "OB-" + suffix, Packs: 6,
				GrossWeight: weight, Volume: volume, OccurredAt: at.Add(3 * time.Hour)})
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
			t.Fatalf("planSeaWithVolume %s, step %d: %v", suffix, i, err)
		}
	}
}
