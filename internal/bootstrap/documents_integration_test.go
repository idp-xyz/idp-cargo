//go:build integration

package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	airapi "go.idp.xyz/idp-cargo/internal/contexts/air/api"
	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	docapi "go.idp.xyz/idp-cargo/internal/contexts/doc/api"
	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	orgmodule "go.idp.xyz/idp-cargo/internal/contexts/org"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

func (allowAll) AuthorizeDocAction(context.Context, string, string, string, string) error { return nil }

func (allowAll) AuthorizeFinAction(context.Context, string, string, string, string) error { return nil }

// DOC V0.1 with every real context (ORG, RATE, COM, FUL, PHY, BOOK, AIR, DOC), as the S1 API client runs the steps
// (T6): house A is received, screened, tendered and weighed at the terminal; AIR finalizes the movement's facts.
// DOC drafts the HAWB from them with the shipper and consignee COM froze on the order, and the MAWB with the
// destination agent ORG knows. The HAWB is issued only once the client has locked the order's parties in COM
// (C12), with a credit override while FIN is not built (OD-3 B), under the forwarder's authority at PVG; COM then
// refuses to change the parties. The MAWB is issued under MU's agency. To add a notify party, the HAWB is amended:
// the client releases the lock, and setting the party races the client locking again; whichever wins, the
// document ends consistent with COM. DOC V0.2 then sends the HAWB to the destination agent in the US: the shipper's
// contact, consenting to go abroad, is on the copy once the client has reserved it in ORG's register; the
// consignee's, consenting only to be provided in China, is left off. FIN V0.2 then bills the job on the issued HAWB
// with a debit note and waives the document fee with a credit note.
func TestTransportDocumentsWithEveryContext(t *testing.T) {
	ctx := context.Background()
	platform := openPlatform(t)
	orgModule, err := platform.ORG(allowAll{}, orgmodule.WithTransferThresholds(1, 5, 10))
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
	finModule, err := platform.FIN(orgModule, rateModule, fulModule, documents, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	docModule, err := platform.DOC(airModule, comModule, orgModule, finModule, allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	documents.Bind(docModule)
	tenant := uniqueTenant("cargo-doc-")
	seedORG(t, platform, tenant, "party-a", "airline-mu", "shipper-co", "consignee-us", "notify-us", "agent-ny")
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	departs := at.Add(72 * time.Hour)
	hours := func(h int) time.Time { return at.Add(time.Duration(h) * time.Hour) }
	kg := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }
	evidence := func(source, ref string, h int) phyapi.Evidence {
		return phyapi.Evidence{Source: source, Reference: ref, OccurredAt: hours(h)}
	}
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	phyOK := func(_ phyapi.Receipt, err error) { t.Helper(); must("PHY", err) }

	// ORG: the shipper, consignee and notify party with their roles and office addresses; the destination agent.
	for party, role := range map[string]string{"shipper-co": "SHIPPER", "consignee-us": "CONSIGNEE", "agent-ny": orgapi.RoleOverseasAgent} {
		_, err = orgModule.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "role-" + party, TenantID: tenant, ActorID: "org-officer", LegalEntityID: "LE",
			PartyID: party, ExpectedRevision: 1, RoleCode: role, ValidFrom: at, EvidenceRef: role + "-agreement", OccurredAt: at})
		must("role "+party, err)
	}
	_, err = orgModule.RecordPartyIdentifier(ctx, orgapi.RecordPartyIdentifier{CommandID: "ein-consignee", TenantID: tenant, ActorID: "org-officer",
		PartyID: "consignee-us", ExpectedRevision: 2, Type: orgapi.IdentifierUSEIN, Country: "US", Value: "123456789", EvidenceRef: "irs-letter"})
	must("consignee EIN", err)
	for _, party := range []string{"shipper-co", "consignee-us", "notify-us", "agent-ny"} {
		_, err = orgModule.AddPartyAddress(ctx, orgapi.AddPartyAddress{CommandID: "office-" + party, TenantID: tenant, ActorID: "org-officer", PartyID: party,
			AddressID: "HQ-" + party, Address: orgapi.Address{ShortCode: "HQ", Purposes: []string{orgapi.AddressOffice}, Name: "Office of " + party,
				Lines: []string{"1 Harbour Road"}, City: "New York", CountryCode: "US"}, DefaultFor: []string{orgapi.AddressOffice}})
		must("office "+party, err)
	}

	// The shipper's documents contact, consenting to be provided and to go abroad.
	_, err = orgModule.AddPartyContact(ctx, orgapi.AddPartyContact{CommandID: "contact-shipper", TenantID: tenant, ActorID: "org-officer", PartyID: "shipper-co",
		ExpectedRevision: 1, ContactID: "CT-SHIPPER", Contact: orgapi.Contact{Purposes: []string{orgapi.ContactDocuments}, Name: "Test Person A",
			Mobile: "+86 138 0000 0001", Email: "a@shipper.test"}, DefaultFor: []string{orgapi.ContactDocuments}})
	must("shipper contact", err)
	_, err = orgModule.RecordContactConsent(ctx, orgapi.RecordContactConsent{CommandID: "consent-shipper", TenantID: tenant, ActorID: "org-officer",
		PartyID: "shipper-co", ExpectedRevision: 2, ContactID: "CT-SHIPPER", Consent: orgapi.ContactConsent{NoticeVersion: "PI-RULES-2026.1",
			ProvideToOthers: true, CrossBorder: true, Basis: orgapi.ConsentSubjectConfirmed, EvidenceRef: "signed notice N-1"}})
	must("shipper consent", err)

	// House A to AirDocFacts version 1, as in the measures slice.
	contexts := slice{org: orgModule, rate: rateModule, com: comModule, ful: fulModule, tenant: tenant, at: at, departs: departs}
	contexts.planAir(t, "party-a", "A")
	orderRevision := func() int64 {
		t.Helper()
		p, err := comModule.GetOrderParties(ctx, comapi.GetOrderParties{TenantID: tenant, OrderID: "O-A"})
		must("order parties", err)
		return p.Revision
	}
	inChina := comapi.ContactConsent{NoticeVersion: "PI-RULES-2026.1", ProvideToOthers: true, Basis: comapi.ConsentCustomerUndertaking, EvidenceRef: "contract C-1"}
	for _, p := range []comapi.OrderParty{{Role: comapi.PartyShipper, PartyID: "shipper-co"}, {Role: comapi.PartyConsignee, PartyID: "consignee-us",
		Contact: comapi.ContactChoice{Use: comapi.ContactOverride, Override: &comapi.Contact{Name: "Test Person B", Phone: "+1 718 555 0002"}, Consent: &inChina,
			Reason: "the consignee's import desk"}}} {
		_, err = comModule.SetOrderParty(ctx, comapi.SetOrderParty{CommandID: "set-" + p.Role, TenantID: tenant, ActorID: "cs-1", OrderID: "O-A",
			ExpectedRevision: orderRevision(), Party: p, OccurredAt: hours(7)})
		must("set "+p.Role, err)
	}
	phyOK(phyModule.CreateConsolidation(ctx, phyapi.CreateConsolidation{CommandID: "consol", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		Mode: phyapi.ModeAir, Kind: phyapi.KindAirConsol, OperatingLegalEntityID: "LE", Origin: iataCode("PVG"), Destination: iataCode("JFK"),
		PlannedDeparture: departs.Add(6 * time.Hour), HouseCutOff: departs.Add(-6 * time.Hour), OccurredAt: hours(8)}))
	phyOK(phyModule.AttachHouse(ctx, phyapi.AttachHouse{CommandID: "attach", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1", ShipmentID: "SH-A",
		OccurredAt: hours(9)}))
	record(t, ctx, fulModule, tenant, "ful-attach", "SH-A", "C-1", 3, hours(9))
	contexts.airline(t)
	contexts.bookAir(t, bookModule, "BK-1", "C-1", "781-12345675", departs.Add(6*time.Hour))
	_, err = airModule.OpenAirMovement(ctx, airapi.OpenAirMovement{CommandID: "open", TenantID: tenant, ActorID: "ops-1", MovementID: "AM-1",
		ConsolidationID: "C-1", BookingID: "BK-1", OccurredAt: hours(12)})
	must("open", err)
	_, err = airModule.AssociateHouseWaybill(ctx, airapi.AssociateHouseWaybill{CommandID: "hawb", TenantID: tenant, ActorID: "ops-1", MovementID: "AM-1",
		ExpectedRevision: 1, ShipmentID: "SH-A", HAWB: "SHA0000001", OccurredAt: hours(12)})
	must("associate", err)
	c, err := phyModule.GetConsolidation(ctx, phyapi.GetConsolidation{TenantID: tenant, ConsolidationID: "C-1"})
	must("consolidation", err)
	phyOK(phyModule.ReceiveCargo(ctx, phyapi.ReceiveCargo{CommandID: "rcv", TenantID: tenant, ActorID: "wh-1", LotID: "L-A", ShipmentID: "SH-A",
		Facility: phyapi.Facility{Location: c.Current.Origin, Code: "SHA-WH1"}, Packs: 6, PackageType: "CT", Condition: phyapi.ConditionGood,
		Evidence: evidence("WMS", "GRN-1", 13), OccurredAt: hours(13)}))
	phyOK(phyModule.RecordCargoMeasurement(ctx, phyapi.RecordCargoMeasurement{CommandID: "weigh", TenantID: tenant, ActorID: "wh-1", LotID: "L-A",
		ExpectedRevision: 1, GrossWeight: kg(303), Volume: kernel.Quantity{Unit: "MTQ", Amount: 2}, Method: phyapi.MethodScale,
		Evidence: evidence("SCALE-3", "W-1", 13), OccurredAt: hours(13)}))
	phyOK(phyModule.RecordScreeningResult(ctx, phyapi.RecordScreeningResult{CommandID: "screen", TenantID: tenant, ActorID: "wh-1", LotID: "L-A",
		ExpectedRevision: 2, Outcome: phyapi.ScreeningPassed, Screener: "PVG cargo security station", Method: "XRAY", Reference: "AQ-1",
		Evidence: evidence("PVG-SECURITY", "AQ-1", 13), OccurredAt: hours(13)}))
	phyOK(phyModule.CompleteReceipt(ctx, phyapi.CompleteReceipt{CommandID: "complete", TenantID: tenant, ActorID: "wh-1", LotID: "L-A", ExpectedRevision: 3,
		OccurredAt: hours(14)}))
	phyOK(phyModule.CloseConsolidation(ctx, phyapi.CloseConsolidation{CommandID: "close", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1",
		ExpectedRevision: 1, OccurredAt: hours(15)}))
	phyOK(phyModule.OpenLoadExecution(ctx, phyapi.OpenLoadExecution{CommandID: "tender", TenantID: tenant, ActorID: "ops-1", ExecutionID: "X-1",
		ConsolidationID: "C-1", Kind: phyapi.ExecutionTender, Facility: phyapi.Facility{Location: c.Current.Origin, Code: "PVG-EASTERN"}, OccurredAt: hours(16)}))
	phyOK(phyModule.StartLoading(ctx, phyapi.StartLoading{CommandID: "start", TenantID: tenant, ActorID: "ops-1", ExecutionID: "X-1", ExpectedRevision: 1,
		OccurredAt: hours(16)}))
	phyOK(phyModule.RecordCargoLoaded(ctx, phyapi.RecordCargoLoaded{CommandID: "load", TenantID: tenant, ActorID: "ops-1", ExecutionID: "X-1",
		ExpectedRevision: 2, LotID: "L-A", Packs: 6, Evidence: evidence("TRUCK-APP", "DLV-1", 16), OccurredAt: hours(16)}))
	phyOK(phyModule.CloseLoadExecution(ctx, phyapi.CloseLoadExecution{CommandID: "tender-receipt", TenantID: tenant, ActorID: "ops-1", ExecutionID: "X-1",
		ExpectedRevision: 3, Evidence: evidence("PVG-EASTERN", "JJD-1", 16), OccurredAt: hours(16)}))
	phyOK(phyModule.RecordCargoMeasurement(ctx, phyapi.RecordCargoMeasurement{CommandID: "terminal-weight", TenantID: tenant, ActorID: "ops-1",
		LotID: "L-A", ExpectedRevision: 4, GrossWeight: kg(318), Volume: kernel.Quantity{Unit: "MTQ", Amount: 1}, Method: phyapi.MethodTerminalWeight,
		Evidence: evidence("PVG-EASTERN", "WGT-1", 17), OccurredAt: hours(17)}))
	_, err = fulModule.RecordVerifiedLoadEvidence(ctx, fulapi.RecordVerifiedLoadEvidence{CommandID: "ful-tendered", TenantID: tenant, ActorID: "ops-1",
		ShipmentID: "SH-A", ExpectedRevision: 4, OccurredAt: hours(17)})
	must("FUL evidence", err)
	_, err = airModule.RefreshHouseWaybillFacts(ctx, airapi.RefreshHouseWaybillFacts{CommandID: "air-tendered", TenantID: tenant, ActorID: "ops-1",
		MovementID: "AM-1", ExpectedRevision: 2, ShipmentID: "SH-A", OccurredAt: hours(17)})
	must("AIR refresh", err)
	_, err = airModule.FinalizeAirDocFacts(ctx, airapi.FinalizeAirDocFacts{CommandID: "finalize", TenantID: tenant, ActorID: "ops-2", MovementID: "AM-1",
		ExpectedRevision: 3, OccurredAt: hours(18)})
	must("finalize", err)

	// DOC: authorities, the HAWB under the C12 lock, the MAWB.
	for id, a := range map[string]struct{ kind, airline string }{"AUTH-H": {docapi.KindHAWB, ""}, "AUTH-M": {docapi.KindMAWB, "airline-mu"}} {
		_, err = docModule.RecordIssuanceAuthority(ctx, docapi.RecordIssuanceAuthority{CommandID: "auth-" + id, TenantID: tenant, ActorID: "legal-1",
			AuthorityID: id, LegalEntityID: "LE", Kind: a.kind, IssuePlace: "CNPVG", AirlinePartyID: a.airline, Qualification: a.kind + " authority",
			Validity: kernel.ValidityInterval{From: at}, EvidenceRef: "contracts/" + id, OccurredAt: hours(18)})
		must("authority "+id, err)
	}
	_, err = docModule.DraftTransportDocument(ctx, docapi.DraftTransportDocument{CommandID: "draft-hawb", TenantID: tenant, ActorID: "doc-1",
		DocumentID: "DOC-HAWB-A", Kind: docapi.KindHAWB, MovementID: "AM-1", ShipmentID: "SH-A", PaymentIndicator: docapi.PaymentPrepaid,
		HandlingInformation: "SPX; keep dry", OccurredAt: hours(19)})
	must("draft HAWB", err)
	_, err = docModule.ApproveTransportDocument(ctx, docapi.ApproveTransportDocument{CommandID: "approve-hawb", TenantID: tenant, ActorID: "doc-2",
		DocumentID: "DOC-HAWB-A", ExpectedRevision: 1, OccurredAt: hours(19)})
	must("approve HAWB", err)
	// FIN V0.1: the legal entity's policy and estimate rate, the job of SH-A priced from the accepted quote Q-A, and the
	// customer's credit account; DOC issues the HAWB on FIN's decision.
	_, err = finModule.OpenFinancePolicy(ctx, finapi.OpenFinancePolicy{CommandID: "fin-policy", TenantID: tenant, ActorID: "acct-1", LegalEntityID: "LE",
		FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: hours(19)})
	must("finance policy", err)
	_, err = finModule.RecordExchangeRate(ctx, finapi.RecordExchangeRate{CommandID: "fin-rate", TenantID: tenant, ActorID: "acct-1", LegalEntityID: "LE",
		ExpectedRevision: 1, RateType: finapi.RateEstimate, From: "USD", Rate: 71_234, Scale: 4, FromDigits: 2, ToDigits: 2, ValidFrom: at,
		ValidUntil: at.AddDate(0, 1, 0), Source: "CFETS central parity", OccurredAt: hours(19)})
	must("estimate rate", err)
	_, err = finModule.OpenFreightJob(ctx, finapi.OpenFreightJob{CommandID: "fin-job", TenantID: tenant, ActorID: "fin-1", JobID: "JOB-A", ShipmentID: "SH-A",
		OccurredAt: hours(19)})
	must("freight job", err)
	_, err = finModule.ApplyPricingBasis(ctx, finapi.ApplyPricingBasis{CommandID: "fin-quote", TenantID: tenant, ActorID: "fin-1", JobID: "JOB-A",
		ExpectedRevision: 1, QuoteID: "Q-A", OrderID: "O-A", OccurredAt: hours(19)})
	must("pricing basis from the accepted quote", err)
	profit, err := finModule.GetJobProfit(ctx, finapi.GetJobProfit{TenantID: tenant, JobID: "JOB-A"})
	if err != nil || profit.Revenue.Minor != 712_340 || profit.Cost.Minor != 356_170 || profit.Profit.Minor != 356_170 || profit.Revenue.Currency != "CNY" {
		t.Fatalf("the job's estimate from Q-A (USD 1000 sell, 500 cost at 7.1234): %+v %v", profit, err)
	}
	_, err = finModule.SetCreditTerms(ctx, finapi.SetCreditTerms{CommandID: "fin-terms", TenantID: tenant, ActorID: "credit-1", LegalEntityID: "LE",
		DebtorPartyID: "party-a", Limit: kernel.Money{Currency: "CNY", Minor: 10_000_000}, PaymentDays: 30, PaymentBasis: finapi.TermsFromMonthEnd,
		OccurredAt: hours(19)})
	must("credit terms", err)
	_, err = finModule.PlaceCreditHold(ctx, finapi.PlaceCreditHold{CommandID: "fin-hold", TenantID: tenant, ActorID: "credit-1", LegalEntityID: "LE",
		DebtorPartyID: "party-a", ExpectedRevision: 1, Code: finapi.HoldOverdue, Reason: "October invoice unpaid", OccurredAt: hours(19)})
	must("credit hold", err)
	issueHAWB := func(command string, revision int64) error {
		_, err := docModule.IssueTransportDocument(ctx, docapi.IssueTransportDocument{CommandID: command, TenantID: tenant, ActorID: "doc-2",
			DocumentID: "DOC-HAWB-A", ExpectedRevision: revision, CreditOverrideReason: "FIN credit control not in service", OccurredAt: hours(20)})
		return err
	}
	if err = issueHAWB("issue-hawb-unlocked", 2); !errors.Is(err, docapi.ErrRejected) {
		t.Fatalf("issue before the client locks the parties: %v", err)
	}
	_, err = comModule.LockOrderParties(ctx, comapi.LockOrderParties{CommandID: "lock-hawb", TenantID: tenant, ActorID: "doc-2", OrderID: "O-A",
		ExpectedRevision: orderRevision(), HolderID: "DOC-HAWB-A", OccurredAt: hours(20)})
	must("lock", err)
	if err = issueHAWB("issue-hawb-held", 2); !errors.Is(err, docapi.ErrRejected) || !strings.Contains(err.Error(), "DOC_CREDIT_HOLD") {
		t.Fatalf("FIN holds the customer's credit: %v", err)
	}
	_, err = finModule.ReleaseCreditHold(ctx, finapi.ReleaseCreditHold{CommandID: "fin-release", TenantID: tenant, ActorID: "credit-2", LegalEntityID: "LE",
		DebtorPartyID: "party-a", ExpectedRevision: 2, Reason: "paid", OccurredAt: hours(20)})
	must("release credit hold", err)
	must("issue HAWB", issueHAWB("issue-hawb", 2))
	if h, _ := docModule.GetTransportDocument(ctx, docapi.GetTransportDocument{TenantID: tenant, DocumentID: "DOC-HAWB-A"}); h.Versions[0].CreditBasis != docapi.CreditOK ||
		h.Versions[0].CreditOverrideReason != "" {
		t.Fatalf("issued on FIN's credit decision: %+v", h.Versions[0])
	}
	hawb, err := docModule.GetDocumentVersion(ctx, docapi.GetDocumentVersion{TenantID: tenant, DocumentID: "DOC-HAWB-A"})
	must("HAWB version", err)
	if d := hawb.Doc; d.Number != "SHA0000001" || d.MAWB != "781-12345675" || d.Shipper.PartyID != "shipper-co" || d.Consignee.PartyID != "consignee-us" ||
		d.Consignee.Identifiers[0].Value != "123456789" || d.Consignee.Address.ORGAddressID != "HQ-consignee-us" || d.Cargo.GrossGrams != 318_000 ||
		d.Cargo.ChargeableGrams != 318_000 || d.Cargo.Lines[0].GoodsDescription != "Cartons of electronics" || d.Security.Status != "SPX" ||
		d.Security.Screenings[0].Reference != "AQ-1" || d.Source.Version != 1 || d.HandlingInformation != "SPX; keep dry" || d.Flights[0].Flight != "MU587" ||
		d.Issuer.LegalEntityID != "LE" || d.IssuePlace.IATA != "PVG" || hawb.Status != docapi.VersionIssued {
		t.Fatalf("HAWB: %+v", d)
	}
	_, err = comModule.SetOrderParty(ctx, comapi.SetOrderParty{CommandID: "notify-locked", TenantID: tenant, ActorID: "cs-1", OrderID: "O-A",
		ExpectedRevision: orderRevision(), Party: comapi.OrderParty{Role: comapi.PartyNotify, PartyID: "notify-us"}, OccurredAt: hours(21)})
	if !errors.Is(err, comapi.ErrRejected) || !strings.Contains(err.Error(), "ORDER_PARTIES_LOCKED") {
		t.Fatalf("a party change while the HAWB holds the lock: %v", err)
	}
	fixture, err := platform.newDB()
	must("fixture db", err)
	err = (bento.TenantTransactor{DB: fixture, Tenant: tenant}).WithinTransaction(ctx, func(txCtx context.Context) error {
		e, err := fixture.RequireExecutor(txCtx)
		if err != nil {
			return err
		}
		_, err = e.Exec(txCtx, `INSERT INTO cargo_com.order_transport (tenant_id,order_id,version,transport_mode,movement_type,reason,changed_by,changed_at,entered_at)
            SELECT tenant_id,order_id,max(version)+1,'AIR','PORT_TO_PORT','behind COM','x',now(),now() FROM cargo_com.order_transport
            WHERE tenant_id=$1 AND order_id='O-A' GROUP BY tenant_id,order_id`, tenant)
		return err
	})
	if sqlState(err) != "23514" || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("a transport version written past COM while locked: %v", err)
	}

	_, err = docModule.DraftTransportDocument(ctx, docapi.DraftTransportDocument{CommandID: "draft-mawb", TenantID: tenant, ActorID: "doc-1",
		DocumentID: "DOC-MAWB-1", Kind: docapi.KindMAWB, MovementID: "AM-1", AgentPartyID: "agent-ny", PaymentIndicator: docapi.PaymentCollect,
		OccurredAt: hours(21)})
	must("draft MAWB", err)
	_, err = docModule.ApproveTransportDocument(ctx, docapi.ApproveTransportDocument{CommandID: "approve-mawb", TenantID: tenant, ActorID: "doc-1",
		DocumentID: "DOC-MAWB-1", ExpectedRevision: 1, OccurredAt: hours(21)})
	must("approve MAWB (own)", err)
	_, err = docModule.IssueTransportDocument(ctx, docapi.IssueTransportDocument{CommandID: "issue-mawb", TenantID: tenant, ActorID: "doc-2",
		DocumentID: "DOC-MAWB-1", ExpectedRevision: 2, OccurredAt: hours(22)})
	must("issue MAWB", err)
	mawb, err := docModule.GetDocumentVersion(ctx, docapi.GetDocumentVersion{TenantID: tenant, DocumentID: "DOC-MAWB-1"})
	must("MAWB version", err)
	if d := mawb.Doc; d.Number != "781-12345675" || d.IssuerCapacity != "AGENT_OF_CARRIER" || d.Shipper.PartyID != "LE" || d.Consignee.PartyID != "agent-ny" ||
		d.Consignee.Address == nil || len(d.Cargo.Lines) != 1 || d.Cargo.Lines[0].HAWB != "SHA0000001" || d.Cargo.Pieces != 6 || d.Airline.AWBPrefix != "781" {
		t.Fatalf("MAWB: %+v", d)
	}
	if m, _ := docModule.GetTransportDocument(ctx, docapi.GetTransportDocument{TenantID: tenant, DocumentID: "DOC-MAWB-1"}); !m.Versions[0].SelfApproved ||
		m.Versions[0].CreditBasis != docapi.CreditNotRequired || m.Versions[0].AuthorityID != "AUTH-M" {
		t.Fatalf("MAWB issue: %+v", m.Versions)
	}

	// Add a notify party: the amendment is opened, the client releases the lock, and setting the party races the
	// client locking it again. Whichever wins, the HAWB ends consistent with COM.
	_, err = docModule.OpenDocumentAmendment(ctx, docapi.OpenDocumentAmendment{CommandID: "amend-notify", TenantID: tenant, ActorID: "doc-1",
		DocumentID: "DOC-HAWB-A", ExpectedRevision: 3, Reason: "the consignee asks for a notify party", OccurredAt: hours(22)})
	must("open amendment", err)
	_, err = comModule.ReleaseOrderPartyLock(ctx, comapi.ReleaseOrderPartyLock{CommandID: "release-hawb", TenantID: tenant, ActorID: "doc-2", OrderID: "O-A",
		ExpectedRevision: orderRevision(), HolderID: "DOC-HAWB-A", OccurredAt: hours(22)})
	must("release", err)
	revision := orderRevision()
	results := make(chan error, 2)
	start := make(chan struct{})
	go func() {
		<-start
		_, err := comModule.SetOrderParty(ctx, comapi.SetOrderParty{CommandID: "notify", TenantID: tenant, ActorID: "cs-1", OrderID: "O-A",
			ExpectedRevision: revision, Party: comapi.OrderParty{Role: comapi.PartyNotify, PartyID: "notify-us"}, OccurredAt: hours(23)})
		results <- err
	}()
	go func() {
		<-start
		_, err := comModule.LockOrderParties(ctx, comapi.LockOrderParties{CommandID: "relock", TenantID: tenant, ActorID: "doc-2", OrderID: "O-A",
			ExpectedRevision: revision, HolderID: "DOC-HAWB-A", OccurredAt: hours(23)})
		results <- err
	}()
	close(start)
	first, second := <-results, <-results
	if (first == nil) == (second == nil) {
		t.Fatalf("exactly one of setting the party and locking wins: %v / %v", first, second)
	}
	parties, err := comModule.GetOrderParties(ctx, comapi.GetOrderParties{TenantID: tenant, OrderID: "O-A"})
	must("order parties", err)
	locked, notified := len(parties.Locks) == 1, len(parties.Parties) == 3
	if locked == notified {
		t.Fatalf("either locked without the notify party or notified without the lock: %+v", parties)
	}
	docRevision := int64(4)
	reamend := func(suffix string) {
		t.Helper()
		_, err = docModule.RefreshDocumentDraft(ctx, docapi.RefreshDocumentDraft{CommandID: "refresh-notify" + suffix, TenantID: tenant, ActorID: "doc-1",
			DocumentID: "DOC-HAWB-A", ExpectedRevision: docRevision, Reason: "COM's notify party", OccurredAt: hours(25)})
		must("refresh", err)
		_, err = docModule.ApproveTransportDocument(ctx, docapi.ApproveTransportDocument{CommandID: "approve-amendment" + suffix, TenantID: tenant,
			ActorID: "doc-2", DocumentID: "DOC-HAWB-A", ExpectedRevision: docRevision + 1, OccurredAt: hours(25)})
		must("approve amendment", err)
		_, err = comModule.LockOrderParties(ctx, comapi.LockOrderParties{CommandID: "relock-after" + suffix, TenantID: tenant, ActorID: "doc-2", OrderID: "O-A",
			ExpectedRevision: orderRevision(), HolderID: "DOC-HAWB-A", OccurredAt: hours(25)})
		must("lock again", err)
		_, err = docModule.IssueDocumentAmendment(ctx, docapi.IssueDocumentAmendment{CommandID: "amended" + suffix, TenantID: tenant, ActorID: "doc-2",
			DocumentID: "DOC-HAWB-A", ExpectedRevision: docRevision + 2, CreditOverrideReason: "FIN credit control not in service", OccurredAt: hours(25)})
		must("issue amendment", err)
	}
	if locked {
		// the lock won: the amendment is abandoned under the lock, then the change is made in order
		_, err = docModule.AbandonDocumentAmendment(ctx, docapi.AbandonDocumentAmendment{CommandID: "abandon", TenantID: tenant, ActorID: "doc-1",
			DocumentID: "DOC-HAWB-A", ExpectedRevision: 4, Reason: "the lock won; the notify party waits", OccurredAt: hours(24)})
		must("abandon", err)
		_, err = docModule.OpenDocumentAmendment(ctx, docapi.OpenDocumentAmendment{CommandID: "amend-notify-2", TenantID: tenant, ActorID: "doc-1",
			DocumentID: "DOC-HAWB-A", ExpectedRevision: 5, Reason: "the consignee asks for a notify party", OccurredAt: hours(24)})
		must("open amendment again", err)
		_, err = comModule.ReleaseOrderPartyLock(ctx, comapi.ReleaseOrderPartyLock{CommandID: "release-hawb-2", TenantID: tenant, ActorID: "doc-2",
			OrderID: "O-A", ExpectedRevision: orderRevision(), HolderID: "DOC-HAWB-A", OccurredAt: hours(24)})
		must("release again", err)
		_, err = comModule.SetOrderParty(ctx, comapi.SetOrderParty{CommandID: "notify-2", TenantID: tenant, ActorID: "cs-1", OrderID: "O-A",
			ExpectedRevision: orderRevision(), Party: comapi.OrderParty{Role: comapi.PartyNotify, PartyID: "notify-us"}, OccurredAt: hours(24)})
		must("notify party", err)
		docRevision = 6
		reamend("-2")
	} else {
		reamend("")
	}
	h, err := docModule.GetTransportDocument(ctx, docapi.GetTransportDocument{TenantID: tenant, DocumentID: "DOC-HAWB-A"})
	must("HAWB", err)
	current, err := docModule.GetDocumentVersion(ctx, docapi.GetDocumentVersion{TenantID: tenant, DocumentID: "DOC-HAWB-A"})
	must("current HAWB", err)
	if h.State != docapi.StateIssued || current.Doc.Notify == nil || current.Doc.Notify.PartyID != "notify-us" || h.CurrentVersion != h.LatestVersion ||
		h.Versions[0].Status != docapi.VersionSuperseded {
		t.Fatalf("HAWB with the notify party (race: lock won %v): %+v", locked, h)
	}
	t.Logf("race: lock won %v, party set %v; HAWB versions %d, current %d", locked, notified, h.LatestVersion, h.CurrentVersion)
	if p, err := comModule.GetOrderParties(ctx, comapi.GetOrderParties{TenantID: tenant, OrderID: "O-A"}); err != nil || len(p.Locks) != 1 ||
		p.Locks[0].HolderID != "DOC-HAWB-A" {
		t.Fatalf("the HAWB holds the lock again: %+v %v", p.Locks, err)
	}
	if docs, err := docModule.ListDocumentsForShipment(ctx, docapi.ListDocumentsForShipment{TenantID: tenant, ShipmentID: "SH-A"}); err != nil || len(docs) != 1 ||
		docs[0].DocumentID != "DOC-HAWB-A" {
		t.Fatalf("documents of SH-A: %+v %v", docs, err)
	}

	// DOC V0.2: the HAWB to the destination agent in the US, by e-mail (T6: the client reserves in ORG, sends, records).
	dispatch := func(command, id string, h int) docapi.DocumentDispatch {
		t.Helper()
		_, err := docModule.PrepareDocumentDispatch(ctx, docapi.PrepareDocumentDispatch{CommandID: command, TenantID: tenant, ActorID: "doc-1", DispatchID: id,
			DocumentID: "DOC-HAWB-A", RecipientParty: "agent-ny", RecipientRole: docapi.RecipientDestinationAgent, Channel: docapi.ChannelEmailPDF,
			OccurredAt: hours(h)})
		must("prepare "+id, err)
		d, err := docModule.GetDispatch(ctx, docapi.GetDispatch{TenantID: tenant, DispatchID: id})
		must("dispatch "+id, err)
		return d
	}
	d := dispatch("dispatch-agent", "DSP-AGENT", 26)
	if d.Version != h.CurrentVersion || d.Recipient.Country != "US" || len(d.Contacts) != 2 || d.Contacts[0].Key != "ORG:shipper-co:CT-SHIPPER" ||
		d.Contacts[0].Classification != docapi.ContactCrossBorder || d.Contacts[0].Status != docapi.ContactIncluded || d.Contacts[0].Consent.ORGRevision != 3 ||
		d.Contacts[1].PartyRole != comapi.PartyConsignee || d.Contacts[1].OmitReason != docapi.OmitNoTransferConsent ||
		!slices.Equal(d.CrossBorderKeys, []string{"ORG:shipper-co:CT-SHIPPER"}) || d.RegisterState != orgapi.TransferOK {
		t.Fatalf("dispatch to the agent: %+v", d)
	}
	if _, err = docModule.GetDispatchCopy(ctx, docapi.GetDispatchCopy{TenantID: tenant, ActorID: "doc-1", DispatchID: "DSP-AGENT"}); !errors.Is(err,
		docapi.ErrRejected) || !strings.Contains(err.Error(), "NOT_RESERVED") {
		t.Fatalf("a copy abroad before ORG's reservation: %v", err)
	}
	outcome := func(command, id, outcome string, revision int64, h int) error {
		_, err := docModule.RecordDispatchOutcome(ctx, docapi.RecordDispatchOutcome{CommandID: command, TenantID: tenant, ActorID: "doc-1", DispatchID: id,
			ExpectedRevision: revision, Outcome: outcome, Reference: "smtp:" + command, OccurredAt: hours(h)})
		return err
	}
	if err = outcome("sent-early", "DSP-AGENT", docapi.DispatchSent, 1, 26); !errors.Is(err, docapi.ErrRejected) || !strings.Contains(err.Error(), "NOT_RESERVED") {
		t.Fatalf("sent before ORG's reservation: %v", err)
	}
	reserved, err := orgModule.ReserveContactTransfers(ctx, orgapi.ReserveContactTransfers{CommandID: "reserve-agent", TenantID: tenant, ActorID: "doc-1",
		DispatchID: "DSP-AGENT", ContactKeys: d.CrossBorderKeys, OccurredAt: hours(26)})
	if err != nil || reserved.Persons != 1 || reserved.State != orgapi.TransferWarning {
		t.Fatalf("reservation: %+v %v", reserved, err)
	}
	copied, err := docModule.GetDispatchCopy(ctx, docapi.GetDispatchCopy{TenantID: tenant, ActorID: "doc-1", DispatchID: "DSP-AGENT"})
	must("copy", err)
	if !bytes.Contains(copied.PDF, []byte("(SHIPPER: Test Person A, +86 138 0000 0001, a@shipper.test)")) || bytes.Contains(copied.PDF, []byte("Test Person B")) ||
		!bytes.Contains(copied.PDF, []byte("(SHA0000001)")) {
		t.Fatal("the agent's copy carries the shipper's contact and not the consignee's")
	}
	must("sent", outcome("sent", "DSP-AGENT", docapi.DispatchSent, 1, 26))
	// a second copy to the agent bounces: the person stays counted once, the client releases the reservation
	d2 := dispatch("dispatch-agent-2", "DSP-AGENT-2", 27)
	if d2.RegisterState != orgapi.TransferWarning {
		t.Fatalf("the register's state at the second dispatch: %+v", d2)
	}
	if reserved, err = orgModule.ReserveContactTransfers(ctx, orgapi.ReserveContactTransfers{CommandID: "reserve-agent-2", TenantID: tenant, ActorID: "doc-1",
		DispatchID: "DSP-AGENT-2", ContactKeys: d2.CrossBorderKeys, OccurredAt: hours(27)}); err != nil || reserved.Persons != 1 {
		t.Fatalf("the same person again: %+v %v", reserved, err)
	}
	must("failed", outcome("bounced", "DSP-AGENT-2", docapi.DispatchFailed, 1, 27))
	if reserved, err = orgModule.ReleaseContactTransfers(ctx, orgapi.ReleaseContactTransfers{CommandID: "release-agent-2", TenantID: tenant, ActorID: "doc-1",
		DispatchID: "DSP-AGENT-2", OccurredAt: hours(27)}); err != nil || reserved.Persons != 1 {
		t.Fatalf("release: %+v %v", reserved, err)
	}
	if provisions, err := docModule.ListContactProvisions(ctx, docapi.ListContactProvisions{TenantID: tenant, ActorID: "privacy-officer", From: at,
		Until: hours(48)}); err != nil || len(provisions) != 2 || provisions[0].State != docapi.DispatchSent || provisions[1].State != docapi.DispatchFailed {
		t.Fatalf("provision records: %+v %v", provisions, err)
	}

	// FIN V0.2: the HAWB is issued, so the job is billed on it. A debit note in CNY converts the USD freight at the
	// INVOICE rate, under party-a's billing profile (a foreign buyer, as ORG registers it in the US), the charge codes'
	// tax treatments and the legal entity's debit note series; a credit note then waives the document fee.
	policyRevision := int64(2)
	policyStep := func(name string) func(finapi.Receipt, error) {
		return func(r finapi.Receipt, err error) {
			t.Helper()
			must(name, err)
			policyRevision = r.NewRevision
		}
	}
	policyStep("invoice rate")(finModule.RecordExchangeRate(ctx, finapi.RecordExchangeRate{CommandID: "fin-invoice-rate", TenantID: tenant, ActorID: "acct-1",
		LegalEntityID: "LE", ExpectedRevision: policyRevision, RateType: finapi.RateInvoice, From: "USD", Rate: 72_000, Scale: 4, FromDigits: 2, ToDigits: 2,
		ValidFrom: at, ValidUntil: at.AddDate(0, 1, 0), Source: "bank selling rate", OccurredAt: hours(28)}))
	taxFrom := at.AddDate(0, 0, -1).Format(time.DateOnly)
	for _, x := range []struct {
		code, treatment string
		bp              int
	}{{"FREIGHT", finapi.TaxExemptIntlFreight, 0}, {"DOC_FEE", finapi.TaxTaxable, 600}} {
		policyStep("tax " + x.code)(finModule.SetChargeTaxTreatment(ctx, finapi.SetChargeTaxTreatment{CommandID: "fin-tax-" + x.code, TenantID: tenant,
			ActorID: "tax-1", LegalEntityID: "LE", ExpectedRevision: policyRevision, ChargeCode: x.code, Treatment: x.treatment, RateBP: x.bp,
			EffectiveFrom: taxFrom, OccurredAt: hours(28)}))
	}
	for _, x := range [][2]string{{finapi.KindDebitNote, "SHADN{seq:6}"}, {finapi.KindCreditNote, "SHACN{seq:6}"}} {
		policyStep("series " + x[0])(finModule.RegisterInvoiceSeries(ctx, finapi.RegisterInvoiceSeries{CommandID: "fin-series-" + x[0], TenantID: tenant,
			ActorID: "acct-1", LegalEntityID: "LE", ExpectedRevision: policyRevision, Kind: x[0], Template: x[1], FirstNumber: 1, OccurredAt: hours(28)}))
	}
	_, err = finModule.SetBillingProfile(ctx, finapi.SetBillingProfile{CommandID: "fin-profile", TenantID: tenant, ActorID: "bill-1", DebtorPartyID: "party-a",
		BuyerKind: finapi.BuyerForeign, EvidenceRef: "account opening form AO-2026-118", OccurredAt: hours(28)})
	must("billing profile", err)
	r, err := finModule.RecordChargeLine(ctx, finapi.RecordChargeLine{CommandID: "fin-doc-fee", TenantID: tenant, ActorID: "fin-1", JobID: "JOB-A",
		ExpectedRevision: 2, ChargeCode: "DOC_FEE", Sell: &finapi.ChargeSide{PartyID: "party-a", Amount: kernel.Money{Currency: "CNY", Minor: 30_000}},
		Reason: "HAWB fee", OccurredAt: hours(28)})
	must("document fee", err)
	invoiceDate := hours(28).Format(time.DateOnly)
	r, err = finModule.IssueDebitNote(ctx, finapi.IssueDebitNote{CommandID: "fin-debit", TenantID: tenant, ActorID: "bill-1", JobID: "JOB-A",
		ExpectedRevision: r.NewRevision, InvoiceID: "DN-A", DebtorPartyID: "party-a", LineNos: []int{1, 2}, Currency: "CNY", InvoiceDate: invoiceDate,
		OccurredAt: hours(28)})
	if err != nil || r.Number != "SHADN000001" {
		t.Fatalf("debit note: %+v %v", r, err)
	}
	note, err := finModule.GetDebtorInvoice(ctx, finapi.GetDebtorInvoice{TenantID: tenant, InvoiceID: "DN-A"})
	// USD 1,000.00 at 7.2000 = CNY 7,200.00, and CNY 300.00
	if err != nil || note.Total.Minor != 750_000 || note.Document.DocumentID != "DOC-HAWB-A" || note.Document.Version != h.CurrentVersion ||
		note.Document.Number != "SHA0000001" || note.Document.MasterNumber != "781-12345675" || note.BillingProfile.Title != "Fixture Corp" ||
		note.TermsVersion != 1 || note.PaymentBasis != finapi.TermsFromMonthEnd || len(note.TaxTotals) != 2 {
		t.Fatalf("debit note on the issued HAWB: %+v %v", note, err)
	}
	r, err = finModule.IssueCreditNote(ctx, finapi.IssueCreditNote{CommandID: "fin-credit", TenantID: tenant, ActorID: "bill-1", JobID: "JOB-A",
		ExpectedRevision: r.NewRevision, InvoiceID: "CN-A", DebitNoteID: "DN-A", LineNos: []int{2}, Reason: "fee waived for the first shipment",
		WriteOff: true, InvoiceDate: invoiceDate, OccurredAt: hours(28)})
	if err != nil || r.Number != "SHACN000001" {
		t.Fatalf("credit note: %+v %v", r, err)
	}
	account, err := finModule.GetCreditAccount(ctx, finapi.GetCreditAccount{TenantID: tenant, LegalEntityID: "LE", DebtorPartyID: "party-a"})
	if err != nil || account.Billed.Minor != 720_000 || account.Unbilled.Minor != 0 {
		t.Fatalf("exposure after the credit note: %+v %v", account, err)
	}
}
