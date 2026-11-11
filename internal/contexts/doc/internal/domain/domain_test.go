package domain

import (
	"errors"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

var t0 = time.Date(2026, 11, 11, 8, 0, 0, 0, time.UTC)

var (
	pvg, jfk = Airport{Code: "CNPVG", IATA: "PVG"}, Airport{Code: "USJFK", IATA: "JFK"}
	issuer   = Issuer{LegalEntityID: "LE", Revision: 2, Name: "上海示例国际货运代理有限公司", Jurisdiction: "CN", Verified: true}
)

func facts(version int) Facts {
	house := func(shipment, hawb, order string) FactsHouse {
		return FactsHouse{ShipmentID: shipment, HAWB: hawb, PlanVersion: 2, EvidenceVersion: 3, Pieces: 6, GrossGrams: 318_000, VolumeCm3: 1_000_000,
			ChargeableGrams: 318_000, Security: "SPX", Screenings: []Screening{{LotID: "L-" + shipment, Outcome: "PASSED", Screener: "PVG security", Method: "XRAY",
				Reference: "AQ-" + shipment, ScreenedAt: t0}},
			Parts: []FactsPart{{ObligationID: "OB-" + shipment, ObligationVersion: 1, OrderID: order, Packs: 6, PackageType: "CT", GoodsDescription: "Cartons of electronics"}}}
	}
	return Facts{MovementID: "AM-1", Version: version, SHA256: strings.Repeat("ab", 32), Final: true, ConsolidationID: "C-1", LegalEntityID: "LE",
		Airline: Airline{PartyID: "airline-mu", PartyRevision: 4, Designator: "MU", AWBPrefix: "781"}, MAWB: "781-12345675", Origin: pvg, Destination: jfk,
		Edition: "E1", Flights: []Flight{{Flight: "MU587", From: pvg, To: jfk, ETD: t0.Add(72 * time.Hour)}}, Pieces: 12, GrossGrams: 636_000,
		VolumeCm3: 2_000_000, ChargeableGrams: 636_000, ChargeableRule: "AIR-VOLUMETRIC-6000-HALF-KG-V1", Security: "SPX",
		Houses: []FactsHouse{house("SH-A", "SHA0000001", "O-A"), house("SH-B", "SHA0000002", "O-B")}}
}

func party(id, country string, version int64) *Party {
	p := &Party{PartyID: id, PartyRevision: 3, LegalName: "Party " + id, CountryCode: country, Named: true, Version: version,
		Address: &Address{Source: "ORG", ORGAddressID: "A-" + id, ORGVersion: 1, Name: "Party " + id, Lines: []string{"1 Road"}, CountryCode: country}}
	if country == "CN" {
		p.Identifiers = []Identifier{{Type: "CN_USCC", Country: "CN", Value: "91310000MA1FL4XQ3X"}}
	} else {
		p.Identifiers = []Identifier{{Type: "REGISTRATION", Country: country, None: true}}
	}
	return p
}

func orders(locks ...string) []OrderParties {
	return []OrderParties{{OrderID: "O-A", Revision: 4, Transport: Transport{Mode: "AIR", Movement: "PORT_TO_PORT"}, Shipper: party("shipper", "CN", 1),
		Consignee: party("consignee", "US", 1), Locks: locks}}
}

var entered = Entered{PaymentIndicator: PaymentPrepaid}

func hawb(t *testing.T, version int, o []OrderParties) Content {
	t.Helper()
	c, err := HouseContent("DOC-H1", "SH-A", facts(version), issuer, o, entered)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestHouseContentFromFactsAndOrders(t *testing.T) {
	c := hawb(t, 1, orders())
	if c.Number != "SHA0000001" || c.MAWB != "781-12345675" || c.IssuerCapacity != "CONSOLIDATOR" || c.IssuePlace != pvg || c.Cargo.Pieces != 6 ||
		c.Cargo.ChargeableGrams != 318_000 || c.Cargo.Lines[0].GoodsDescription != "Cartons of electronics" || c.Security.Status != "SPX" ||
		c.Security.Screenings[0].Reference != "AQ-SH-A" || c.Shipper.PartyID != "shipper" || c.Consignee.CountryCode != "US" || len(c.Orders) != 1 ||
		c.Orders[0].Revision != 4 || c.ReleaseType != ReleaseConsigneeNamed || c.Source.Version != 1 {
		t.Fatalf("content: %+v", c)
	}
	notFinal := facts(1)
	notFinal.Final = false
	if _, err := HouseContent("DOC-H1", "SH-A", notFinal, issuer, orders(), entered); !errors.Is(err, ErrSourceNotFinal) {
		t.Fatalf("superseded facts: %v", err)
	}
	if _, err := HouseContent("DOC-H1", "SH-X", facts(1), issuer, orders(), entered); !errors.Is(err, ErrMasterMismatch) {
		t.Fatalf("a shipment not in the facts: %v", err)
	}
	draftLE := issuer
	draftLE.Verified = false
	if _, err := HouseContent("DOC-H1", "SH-A", facts(1), draftLE, orders(), entered); !errors.Is(err, ErrIssuerNotVerified) {
		t.Fatalf("issuer not verified: %v", err)
	}
	if _, err := HouseContent("DOC-H1", "SH-A", facts(1), issuer, orders(), Entered{PaymentIndicator: "CASH"}); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("payment indicator: %v", err)
	}
	noConsignee := orders()
	noConsignee[0].Consignee = &Party{Named: false}
	if _, err := HouseContent("DOC-H1", "SH-A", facts(1), issuer, noConsignee, entered); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("a TO ORDER consignee on an air waybill: %v", err)
	}
	// a house of two orders: the same parties, or DOC_PARTIES_DIVERGE
	two := facts(1)
	two.Houses[0].Parts = append(two.Houses[0].Parts, FactsPart{ObligationID: "OB-2", ObligationVersion: 1, OrderID: "O-A2", Packs: 2, PackageType: "CT",
		GoodsDescription: "Spare parts"})
	second := orders()[0]
	second.OrderID, second.Revision = "O-A2", 7
	if c, err := HouseContent("DOC-H1", "SH-A", two, issuer, append(orders(), second), entered); err != nil || len(c.Orders) != 2 || len(c.Cargo.Lines) != 2 {
		t.Fatalf("two orders, same parties: %+v %v", c.Orders, err)
	}
	second.Consignee = party("other", "US", 1)
	if _, err := HouseContent("DOC-H1", "SH-A", two, issuer, append(orders(), second), entered); !errors.Is(err, ErrPartiesDiverge) {
		t.Fatalf("two orders, two consignees: %v", err)
	}
}

func TestMasterContent(t *testing.T) {
	agent := *party("agent-ny", "US", 0)
	c, err := MasterContent("DOC-M1", facts(1), issuer, agent, Entered{PaymentIndicator: PaymentCollect})
	if err != nil {
		t.Fatal(err)
	}
	if c.Number != "781-12345675" || c.IssuerCapacity != "AGENT_OF_CARRIER" || c.Shipper.LegalName != issuer.Name || c.Consignee.PartyID != "agent-ny" ||
		len(c.Cargo.Lines) != 2 || c.Cargo.Lines[1].HAWB != "SHA0000002" || c.Cargo.Pieces != 12 || c.Cargo.ChargeableGrams != 636_000 || c.ShipmentID != "" {
		t.Fatalf("master: %+v", c)
	}
	agent.Address = nil
	if _, err = MasterContent("DOC-M1", facts(1), issuer, agent, entered); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("an agent without an address: %v", err)
	}
	d, err := DraftDocument("T", "DOC-M1", c, "doc-1", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(d.Approve("doc-2", t0), d.Issue(IssueGate{Facts: facts(1), IssuerVerified: true, AuthorityID: "AUTH-M", AuthorityVersion: 1}, "doc-2", t0)); err != nil {
		t.Fatal(err)
	}
	if v, _ := d.Current(); v.Issue.Credit.Basis != CreditNotRequired {
		t.Fatalf("a master needs no credit decision: %+v", v.Issue)
	}
}

// The life of a HAWB: draft, refresh, approval (self-approval flagged), the issue gates, an amendment issued, one
// abandoned, cancellation; versions are kept as written and restore from their bytes.
func TestHouseWaybillLifecycle(t *testing.T) {
	d, err := DraftDocument("T", "DOC-H1", hawb(t, 1, orders()), "doc-1", t0)
	if err != nil {
		t.Fatal(err)
	}
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	if err = d.Refresh(hawb(t, 2, orders()), "", "doc-1", at(1)); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("a refresh without a reason: %v", err)
	}
	if err = d.Refresh(hawb(t, 2, orders()), "AIR finalized version 2", "doc-1", at(1)); err != nil {
		t.Fatal(err)
	}
	other := hawb(t, 2, orders())
	other.Number = "SHA0000009"
	if err = d.Refresh(other, "another number", "doc-1", at(1)); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("another number is another document: %v", err)
	}
	if !d.SelfApproval("doc-1") || d.SelfApproval("doc-2") {
		t.Fatal("self approval")
	}
	if err = d.Approve("doc-1", at(2)); err != nil {
		t.Fatal(err)
	}
	if v := d.Latest(); v.Approval == nil || !v.Approval.SelfApproved || d.Status(1) != VersionDiscarded || d.Status(2) != VersionApproved {
		t.Fatalf("approved v2: %+v %s %s", v.Approval, d.Status(1), d.Status(2))
	}
	gate := IssueGate{Facts: facts(2), Orders: orders("DOC-H1"), Credit: CreditDecision{Status: "NONE"}, OverrideReason: "FIN not in service",
		IssuerVerified: true, AuthorityID: "AUTH-H", AuthorityVersion: 1}
	for name, tc := range map[string]struct {
		change func(*IssueGate)
		want   error
	}{
		"newer facts":       {func(g *IssueGate) { g.Facts = facts(3) }, ErrSourceStale},
		"superseded facts":  {func(g *IssueGate) { g.Facts.Final = false }, ErrSourceStale},
		"not locked":        {func(g *IssueGate) { g.Orders = orders("DOC-OTHER") }, ErrNotLocked},
		"order not read":    {func(g *IssueGate) { g.Orders = nil }, ErrNotLocked},
		"parties changed":   {func(g *IssueGate) { g.Orders[0].Consignee.Version = 2 }, ErrSourceStale},
		"transport changed": {func(g *IssueGate) { g.Orders[0].Transport.Movement = "DOOR_TO_PORT" }, ErrSourceStale},
		"credit hold":       {func(g *IssueGate) { g.Credit.Status = "HOLD" }, ErrCreditHold},
		"no override":       {func(g *IssueGate) { g.OverrideReason = "" }, ErrCreditUndecided},
		"no authority":      {func(g *IssueGate) { g.AuthorityID = "" }, ErrIssuanceBlocked},
		"issuer suspended":  {func(g *IssueGate) { g.IssuerVerified = false }, ErrIssuerNotVerified},
	} {
		g := gate
		g.Orders = orders("DOC-H1")
		tc.change(&g)
		if err = d.Issue(g, "doc-2", at(3)); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err = d.Issue(gate, "doc-2", at(3)); err != nil {
		t.Fatal(err)
	}
	v2, _ := d.Current()
	if v2.No != 2 || v2.Issue.Credit.Basis != CreditOverride || v2.Issue.Credit.Reason != "FIN not in service" || d.Status(2) != VersionIssued {
		t.Fatalf("issued v2: %+v", v2.Issue)
	}
	if err = d.Refresh(hawb(t, 2, orders()), "after issue", "doc-1", at(4)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("refresh an issued document: %v", err)
	}
	// amendment issued: version 3 under the same number, version 2 superseded
	if err = d.OpenAmendment(hawb(t, 3, orders()), "terminal weight corrected", "doc-1", at(5)); err != nil {
		t.Fatal(err)
	}
	if d.Status(2) != VersionIssued || d.Status(3) != VersionDraft {
		t.Fatalf("amending: %s %s", d.Status(2), d.Status(3))
	}
	gate.Facts = facts(3)
	if err = d.IssueAmendment(gate, "doc-2", at(6)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("an amendment issued unapproved: %v", err)
	}
	if err = d.Approve("doc-2", at(6)); err != nil {
		t.Fatal(err)
	}
	if d.Status(3) != VersionApproved || d.State().State != StateAmending {
		t.Fatalf("approved amendment: %s %s", d.Status(3), d.State().State)
	}
	if err = d.IssueAmendment(gate, "doc-2", at(6)); err != nil {
		t.Fatal(err)
	}
	if v, _ := d.Current(); v.No != 3 || d.Status(2) != VersionSuperseded || d.Status(3) != VersionIssued {
		t.Fatalf("amended: %d %s %s", v.No, d.Status(2), d.Status(3))
	}
	// amendment abandoned: the parties must be locked again for the document, unchanged
	if err = d.OpenAmendment(hawb(t, 3, orders()), "notify party to be added", "doc-1", at(7)); err != nil {
		t.Fatal(err)
	}
	if err = d.AbandonAmendment(orders(), "the customer withdrew the change", "doc-1", at(8)); !errors.Is(err, ErrNotLocked) {
		t.Fatalf("abandon without the lock: %v", err)
	}
	if err = d.AbandonAmendment(orders("DOC-H1"), "the customer withdrew the change", "doc-1", at(8)); err != nil {
		t.Fatal(err)
	}
	if v, _ := d.Current(); v.No != 3 || d.Status(4) != VersionDiscarded || d.State().State != StateIssued {
		t.Fatalf("abandoned: %d %s", v.No, d.Status(4))
	}
	var types []string
	for _, e := range d.Pending() {
		types = append(types, strings.TrimSuffix(strings.TrimPrefix(e.Type, "TransportDocument"), ".v1"))
	}
	if strings.Join(types, " ") != "Drafted Drafted Approved Issued AmendmentOpened Approved Amended AmendmentOpened AmendmentAbandoned" {
		t.Fatalf("events %v", types)
	}
	// restore from the stored bytes, then cancel
	d.Stamp(at(9))
	stored := d.State()
	for i := range stored.Versions {
		stored.Versions[i].Doc = Content{}
	}
	back, err := RestoreDocument(stored)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := back.Current(); v.No != 3 || v.Doc.Number != "SHA0000001" || v.Issue.EnteredAt.IsZero() {
		t.Fatalf("restored current: %+v", v)
	}
	if err = back.Cancel("shipment withdrawn by the customer", "doc-1", at(10)); err != nil {
		t.Fatal(err)
	}
	if back.Status(3) != VersionCancelled || back.Status(2) != VersionSuperseded || back.State().State != StateCancelled {
		t.Fatalf("cancelled: %s %s", back.Status(2), back.Status(3))
	}
	versions, approvals, issues, discards := back.Added()
	if len(versions)+len(approvals)+len(issues)+len(discards) != 0 {
		t.Fatal("cancelling adds no version records")
	}
	tampered := d.State()
	tampered.Versions[0].Content = append([]byte(nil), tampered.Versions[0].Content...)
	tampered.Versions[0].Content[len(tampered.Versions[0].Content)-2] = ' '
	if _, err = RestoreDocument(tampered); err == nil {
		t.Fatal("bytes that do not match their digest")
	}
}

func TestApprovalNeedsACompleteDocument(t *testing.T) {
	o := orders()
	o[0].Shipper.Identifiers = nil
	d, err := DraftDocument("T", "DOC-H1", hawb(t, 1, o), "doc-1", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Approve("doc-2", t0); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("a Chinese shipper without its credit or customs code: %v", err)
	}
	if err = d.Issue(IssueGate{}, "doc-2", t0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("issue a draft: %v", err)
	}
}

// Added reports what was written since the document was loaded, once.
func TestAddedRecords(t *testing.T) {
	d, _ := DraftDocument("T", "DOC-H1", hawb(t, 1, orders()), "doc-1", t0)
	if v, a, i, x := d.Added(); len(v) != 1 || len(a)+len(i)+len(x) != 0 {
		t.Fatal("a new draft adds its version")
	}
	d.Acknowledge()
	if err := d.Approve("doc-2", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if v, a, _, _ := d.Added(); len(v) != 0 || len(a) != 1 || a[0].No != 1 {
		t.Fatalf("the approval of a stored version: %d %d", len(v), len(a))
	}
}

func TestIssuanceAuthority(t *testing.T) {
	base := IssuanceAuthority{TenantID: "T", AuthorityID: "AUTH-M", LegalEntityID: "LE", Kind: KindMAWB, IssuePlace: "CNPVG", AirlinePartyID: "airline-mu",
		Qualification: "MU cargo agency agreement 2026-017", Validity: kernel.ValidityInterval{From: t0, Until: t0.Add(365 * 24 * time.Hour)},
		EvidenceRef: "contracts/mu-agency.pdf"}
	noAirline := base
	noAirline.AirlinePartyID = ""
	if _, err := RecordIssuanceAuthority(noAirline, "legal-1", t0); !errors.Is(err, ErrInvalidAuthority) {
		t.Fatalf("an airline agency without its airline: %v", err)
	}
	a, err := RecordIssuanceAuthority(base, "legal-1", t0)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Covers("LE", KindMAWB, "CNPVG", "airline-mu", t0.Add(time.Hour)) || a.Covers("LE", KindMAWB, "CNPVG", "airline-ca", t0.Add(time.Hour)) ||
		a.Covers("LE", KindHAWB, "CNPVG", "", t0.Add(time.Hour)) || a.Covers("LE", KindMAWB, "CNSHA", "airline-mu", t0.Add(time.Hour)) ||
		a.Covers("LE", KindMAWB, "CNPVG", "airline-mu", t0.Add(-time.Hour)) {
		t.Fatal("covers")
	}
	if err = a.Withdraw("agency ended", "legal-1", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if a.Covers("LE", KindMAWB, "CNPVG", "airline-mu", t0.Add(3*time.Hour)) || !a.Covers("LE", KindMAWB, "CNPVG", "airline-mu", t0.Add(time.Hour)) {
		t.Fatal("withdrawn from an instant on")
	}
	if err = a.Withdraw("again", "legal-1", t0.Add(3*time.Hour)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("withdrawn twice: %v", err)
	}
	if _, err = RestoreIssuanceAuthority(*a); err != nil {
		t.Fatal(err)
	}
}

// A HAWB follows its shipment to another movement (a rollover) by an amendment under the same number; a MAWB never
// changes movement. An amendment in progress is cancelled with the document.
func TestRolloverAndCancellingAnAmendment(t *testing.T) {
	d, err := DraftDocument("T", "DOC-H1", hawb(t, 1, orders()), "doc-1", t0)
	if err != nil {
		t.Fatal(err)
	}
	gate := IssueGate{Facts: facts(1), Orders: orders("DOC-H1"), Credit: CreditDecision{Status: "OK", DecisionID: "CR-1"}, IssuerVerified: true,
		AuthorityID: "AUTH-H", AuthorityVersion: 1}
	if err = errors.Join(d.Approve("doc-2", t0), d.Issue(gate, "doc-2", t0)); err != nil {
		t.Fatal(err)
	}
	rolled := facts(1)
	rolled.MovementID, rolled.ConsolidationID, rolled.MAWB = "AM-2", "C-2", "781-23456786"
	c, err := HouseContent("DOC-H1", "SH-A", rolled, issuer, orders(), entered)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.OpenAmendment(c, "rolled to MU589 under 781-23456786", "doc-1", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if st := d.State(); st.MovementID != "AM-2" || st.ConsolidationID != "C-2" || st.Number != "SHA0000001" {
		t.Fatalf("rolled HAWB: %+v", st)
	}
	if _, err = RestoreDocument(d.State()); err != nil {
		t.Fatalf("a rolled HAWB restores: %v", err)
	}
	if err = d.Cancel("the shipment is withdrawn", "doc-1", t0.Add(2*time.Hour)); err != nil {
		t.Fatalf("cancel an amendment in progress: %v", err)
	}
	if d.Status(2) != VersionDiscarded || d.Status(1) != VersionCancelled {
		t.Fatalf("cancelled amendment: %s %s", d.Status(1), d.Status(2))
	}
	m, _ := MasterContent("DOC-M1", facts(1), issuer, *party("agent-ny", "US", 0), entered)
	master, _ := DraftDocument("T", "DOC-M1", m, "doc-1", t0)
	other, _ := MasterContent("DOC-M1", rolled, issuer, *party("agent-ny", "US", 0), entered)
	other.Number = m.Number
	if err = master.Refresh(other, "another movement", "doc-1", t0); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("a MAWB of another movement: %v", err)
	}
}
