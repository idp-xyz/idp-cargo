package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func oceanFacts(version int) OceanFacts {
	return OceanFacts{MovementID: "OM-1", Version: version, SHA256: strings.Repeat("cd", 32), Final: true, ConsolidationID: "C-9", LegalEntityID: "LE",
		CarrierPartyID: "line-1", MasterBill: "LINU2611000457", Vessel: "EVER GIVEN", Voyage: "0123W", PlaceOfReceipt: "CNSHA", PortOfLoading: "CNSHG",
		PortOfDischarge: "DEHAM", Edition: "E1", OnBoardAt: t0,
		Houses: []OceanHouse{{ShipmentID: "SH-L", HBL: "SHAL0000001", Packs: 6, Package: "CT", GrossGrams: 312_000, VolumeCm3: 1_000_000,
			Goods: "Cartons of electronics", OrderIDs: []string{"O-L"}}}}
}

func seaOrders(consignee *Party, notify *Party, locks ...string) []OrderParties {
	return []OrderParties{{OrderID: "O-L", Revision: 4, Transport: Transport{Mode: "SEA"}, Shipper: party("shipper", "CN", 1), Consignee: consignee,
		Notify: notify, Locks: locks}}
}

func toOrder(text string) *Party { return &Party{ToOrder: text, Version: 2} }

func TestSeaHouseReleaseAndConsignee(t *testing.T) {
	named, notify := party("consignee", "DE", 1), party("notify", "DE", 1)
	originals := Entered{PaymentIndicator: PaymentPrepaid, Release: ReleaseOriginals, Originals: 3}
	for name, tc := range map[string]struct {
		kind    Kind
		entered Entered
		orders  []OrderParties
		want    error
		release string
	}{
		"an HBL is EXPRESS unless it says otherwise": {KindHBL, entered, seaOrders(named, nil), nil, ReleaseExpress},
		"three originals":                      {KindHBL, originals, seaOrders(named, nil), nil, ReleaseOriginals},
		"no originals at all":                  {KindHBL, Entered{PaymentIndicator: PaymentPrepaid, Release: ReleaseOriginals}, seaOrders(named, nil), ErrInvalidChange, ""},
		"ten originals":                        {KindHBL, Entered{PaymentIndicator: PaymentPrepaid, Release: ReleaseOriginals, Originals: 10}, seaOrders(named, nil), ErrInvalidChange, ""},
		"originals counted on an EXPRESS bill": {KindHBL, Entered{PaymentIndicator: PaymentPrepaid, Originals: 3}, seaOrders(named, nil), ErrInvalidChange, ""},
		"an HBL is not a sea waybill":          {KindHBL, Entered{PaymentIndicator: PaymentPrepaid, Release: ReleaseSeaWaybill}, seaOrders(named, nil), ErrInvalidChange, ""},
		"a sea waybill":                        {KindHouseSeaWaybill, entered, seaOrders(named, nil), nil, ReleaseSeaWaybill},
		"a sea waybill has no originals":       {KindHouseSeaWaybill, originals, seaOrders(named, nil), ErrInvalidChange, ""},
		"to order on originals":                {KindHBL, originals, seaOrders(toOrder("TO ORDER OF BANK OF CHINA"), notify), nil, ReleaseOriginals},
		"to order without a notify party":      {KindHBL, originals, seaOrders(toOrder("TO ORDER"), nil), ErrIncomplete, ""},
		"to order on an EXPRESS bill":          {KindHBL, entered, seaOrders(toOrder("TO ORDER"), notify), ErrIncomplete, ""},
		"to order on a sea waybill":            {KindHouseSeaWaybill, entered, seaOrders(toOrder("TO ORDER"), notify), ErrIncomplete, ""},
		"no consignee":                         {KindHBL, originals, seaOrders(nil, notify), ErrIncomplete, ""},
		"a consignee that is neither":          {KindHBL, originals, seaOrders(&Party{Version: 1}, notify), ErrIncomplete, ""},
	} {
		c, err := OceanHouseContent(tc.kind, "DOC-L", "SH-L", oceanFacts(1), issuer, tc.orders, tc.entered)
		if !errors.Is(err, tc.want) || (err == nil && (c.ReleaseType != tc.release || c.Kind != string(tc.kind) || c.IssuerCapacity != "NVOCC_CARRIER")) {
			t.Errorf("%s: %v %s %s", name, err, c.ReleaseType, c.Kind)
		}
	}
	c, err := OceanHouseContent(KindHBL, "DOC-L", "SH-L", oceanFacts(1), issuer, seaOrders(toOrder("TO ORDER OF BANK OF CHINA"), notify), originals)
	if err != nil || !c.ToOrder() || c.Consignee.ToOrder != "TO ORDER OF BANK OF CHINA" || c.Consignee.PartyID != "" || c.Notify == nil ||
		c.OriginalCount != 3 || c.Orders[0].ConsigneeVersion != 2 || c.complete() != nil {
		t.Fatalf("the bill to order: %+v %v", c, err)
	}
	// two orders of one house whose texts differ are different consignees
	f := oceanFacts(1)
	f.Houses[0].OrderIDs = []string{"O-L", "O-M"}
	two := append(seaOrders(toOrder("TO ORDER"), notify), OrderParties{OrderID: "O-M", Revision: 1, Transport: Transport{Mode: "SEA"},
		Shipper: party("shipper", "CN", 1), Consignee: toOrder("TO ORDER OF BANK OF CHINA"), Notify: notify})
	if _, err = OceanHouseContent(KindHBL, "DOC-L", "SH-L", f, issuer, two, originals); !errors.Is(err, ErrPartiesDiverge) {
		t.Fatalf("two texts to order: %v", err)
	}
}

// seaBill drafts, approves and issues an HBL of n originals (0: EXPRESS).
func seaBill(t *testing.T, n int) (*Document, []OrderParties) {
	t.Helper()
	e := entered
	if n > 0 {
		e = Entered{PaymentIndicator: PaymentPrepaid, Release: ReleaseOriginals, Originals: n}
	}
	o := seaOrders(party("consignee", "DE", 1), nil, "DOC-L")
	c, err := OceanHouseContent(KindHBL, "DOC-L", "SH-L", oceanFacts(1), issuer, o, e)
	if err != nil {
		t.Fatal(err)
	}
	d, err := DraftDocument("T", "DOC-L", c, "doc-1", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(d.Approve("doc-2", t0), d.Issue(seaGate(1, o), "doc-1", t0)); err != nil {
		t.Fatal(err)
	}
	return d, o
}

func seaGate(version int, o []OrderParties) IssueGate {
	return IssueGate{Ocean: oceanFacts(version), Orders: o, OverrideReason: "the account is opened by hand", IssuerVerified: true, AuthorityID: "A",
		AuthorityVersion: 1}
}

func statuses(d *Document, version int) string {
	var out []string
	for serial := 1; serial <= 9; serial++ {
		if s, ok := d.originalStatus(version, serial); ok {
			out = append(out, s)
		}
	}
	return strings.Join(out, " ")
}

func TestOriginalsSurrendered(t *testing.T) {
	d, _ := seaBill(t, 3)
	if got := statuses(d, 1); got != "ISSUED ISSUED ISSUED" || len(d.AddedOriginals()) != 3 {
		t.Fatalf("issued: %s", got)
	}
	amendment, _ := OceanHouseContent(KindHBL, "DOC-L", "SH-L", oceanFacts(2), issuer, seaOrders(party("consignee", "DE", 1), nil),
		Entered{PaymentIndicator: PaymentPrepaid, Release: ReleaseOriginals, Originals: 3})
	if err := d.Cancel("a mistake", "doc-1", t0); !errors.Is(err, ErrOriginalsOut) {
		t.Fatalf("cancelled with the originals out: %v", err)
	}
	if err := d.OpenAmendment(amendment, "the goods", "doc-1", t0); !errors.Is(err, ErrOriginalsOut) {
		t.Fatalf("amended with the originals out: %v", err)
	}
	if err := d.SurrenderOriginals(" ", "doc-1", t0); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("surrendered without evidence: %v", err)
	}
	if err := d.SurrenderOriginals("telex release of 12 November", "doc-1", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	cur, _ := d.Current()
	if got := statuses(d, 1); got != "SURRENDERED_AT_ORIGIN SURRENDERED_AT_ORIGIN SURRENDERED_AT_ORIGIN" || d.State().State != StateSurrendered ||
		cur.No != 1 || d.Status(1) != VersionIssued || d.Pending()[len(d.Pending())-1].Type != eventSurrendered {
		t.Fatalf("surrendered: %s %s", got, d.State().State)
	}
	for _, o := range d.Originals() {
		if want := map[string]int{OriginalIssued: 1, OriginalSurrendered: 2}[o.Status]; o.Record != want {
			t.Fatalf("record numbers: %+v", o)
		}
	}
	if err := d.validate(); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"surrendered twice":         d.SurrenderOriginals("again", "doc-1", t0.Add(2*time.Hour)),
		"returned once surrendered": d.ReturnOriginal(1, "at the counter", "doc-1", t0.Add(2*time.Hour)),
		"amended once surrendered":  d.OpenAmendment(amendment, "the goods", "doc-1", t0.Add(2*time.Hour)),
	} {
		if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := d.Cancel("issued by mistake", "doc-1", t0.Add(2*time.Hour)); err != nil || d.State().State != StateCancelled || d.Status(1) != VersionCancelled {
		t.Fatalf("a surrendered bill is cancelled, its originals being with the issuer: %v", err)
	}
}

func TestOriginalsReturnedForAnAmendment(t *testing.T) {
	d, o := seaBill(t, 2)
	for name, err := range map[string]error{
		"an original the bill has not": d.ReturnOriginal(3, "at the counter", "doc-1", t0),
		"original zero":                d.ReturnOriginal(0, "at the counter", "doc-1", t0),
	} {
		if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := d.ReturnOriginal(1, "", "doc-1", t0); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("returned without evidence: %v", err)
	}
	if err := d.ReturnOriginal(1, "at the counter", "doc-1", t0); err != nil {
		t.Fatal(err)
	}
	amendment, _ := OceanHouseContent(KindHBL, "DOC-L", "SH-L", oceanFacts(2), issuer, o, Entered{PaymentIndicator: PaymentPrepaid, Release: ReleaseOriginals,
		Originals: 2})
	for name, tc := range map[string]struct{ got, want error }{
		"returned twice":               {d.ReturnOriginal(1, "at the counter", "doc-1", t0), ErrInvalidTransition},
		"amended with one still out":   {d.OpenAmendment(amendment, "the goods", "doc-1", t0), ErrOriginalsOut},
		"cancelled with one still out": {d.Cancel("a mistake", "doc-1", t0), ErrOriginalsOut},
	} {
		if !errors.Is(tc.got, tc.want) {
			t.Errorf("%s: %v", name, tc.got)
		}
	}
	if err := errors.Join(d.ReturnOriginal(2, "at the counter", "doc-1", t0), d.OpenAmendment(amendment, "the goods", "doc-1", t0)); err != nil {
		t.Fatal(err)
	}
	// abandoned: the originals returned for it are handed back, and the issued version is the document again
	if err := d.AbandonAmendment(o, "not needed after all", "doc-1", t0); err != nil {
		t.Fatal(err)
	}
	if got := statuses(d, 1); got != "ISSUED ISSUED" || d.State().State != StateIssued {
		t.Fatalf("after the abandoned amendment: %s", got)
	}
	for _, x := range d.Originals() {
		if x.Record == 3 && (x.Status != OriginalIssued || x.Evidence != "not needed after all") {
			t.Fatalf("handed back: %+v", x)
		}
	}
	// amended: version 2 has originals of its own, those of version 1 stay returned
	if err := errors.Join(d.ReturnOriginal(1, "at the counter", "doc-1", t0), d.ReturnOriginal(2, "at the counter", "doc-1", t0),
		d.OpenAmendment(amendment, "the goods", "doc-1", t0), d.Approve("doc-2", t0), d.IssueAmendment(seaGate(2, o), "doc-1", t0)); err != nil {
		t.Fatal(err)
	}
	if one, two := statuses(d, 1), statuses(d, 3); one != "RETURNED RETURNED" || two != "ISSUED ISSUED" || d.State().State != StateIssued {
		t.Fatalf("after the amendment: version 1 %q, version 3 %q", one, two)
	}
	if err := d.SurrenderOriginals("telex", "doc-1", t0); err != nil || statuses(d, 1) != "RETURNED RETURNED" {
		t.Fatalf("the amended bill surrendered: %v", err)
	}
	// a document cancelled while amending: the originals are with the issuer
	d, o = seaBill(t, 1)
	if err := errors.Join(d.ReturnOriginal(1, "at the counter", "doc-1", t0), d.OpenAmendment(amendment1(t, o), "the goods", "doc-1", t0),
		d.Cancel("withdrawn", "doc-1", t0)); err != nil {
		t.Fatal(err)
	}
}

// Return, amend, abandon, surrender: the abandoned amendment's discarded draft is the latest version, the originals
// handed back are out again, and surrendering them is allowed and leaves the document SURRENDERED on its issued version.
func TestOriginalsSurrenderedAfterAnAbandonedAmendment(t *testing.T) {
	d, o := seaBill(t, 2)
	amendment, err := OceanHouseContent(KindHBL, "DOC-L", "SH-L", oceanFacts(2), issuer, o, Entered{PaymentIndicator: PaymentPrepaid, Release: ReleaseOriginals,
		Originals: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(d.ReturnOriginal(1, "at the counter", "doc-1", t0.Add(time.Hour)), d.ReturnOriginal(2, "at the counter", "doc-1", t0.Add(2*time.Hour)),
		d.OpenAmendment(amendment, "the goods", "doc-1", t0.Add(3*time.Hour)), d.AbandonAmendment(o, "withdrawn", "doc-1", t0.Add(4*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if got := statuses(d, 1); got != "ISSUED ISSUED" || d.State().State != StateIssued || d.Status(1) != VersionIssued || d.Status(2) != VersionDiscarded {
		t.Fatalf("after the abandoned amendment: %s %s, versions %s and %s", got, d.State().State, d.Status(1), d.Status(2))
	}
	if err = d.SurrenderOriginals("telex release", "doc-1", t0.Add(5*time.Hour)); err != nil {
		t.Fatalf("surrendered after the abandoned amendment: %v", err)
	}
	cur, _ := d.Current()
	if got := statuses(d, 1); got != "SURRENDERED_AT_ORIGIN SURRENDERED_AT_ORIGIN" || d.State().State != StateSurrendered || cur.No != 1 || d.Status(1) != VersionIssued ||
		d.Status(2) != VersionDiscarded {
		t.Fatalf("surrendered: %s %s, current version %d", got, d.State().State, cur.No)
	}
	if err = d.validate(); err != nil {
		t.Fatal(err)
	}
}

// An original that is already back with the issuer is surrendered with the rest: three originals, the second
// returned, then all three surrendered, the second with its third record. So are three that were all returned.
func TestOriginalsSurrenderedWithOneAlreadyReturned(t *testing.T) {
	d, _ := seaBill(t, 3)
	if err := d.ReturnOriginal(2, "at the counter", "doc-1", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := d.SurrenderOriginals("telex release", "doc-1", t0.Add(2*time.Hour)); err != nil {
		t.Fatalf("surrendered with one original already returned: %v", err)
	}
	cur, _ := d.Current()
	if got := statuses(d, 1); got != "SURRENDERED_AT_ORIGIN SURRENDERED_AT_ORIGIN SURRENDERED_AT_ORIGIN" || d.State().State != StateSurrendered ||
		cur.No != 1 || d.Status(1) != VersionIssued || d.Pending()[len(d.Pending())-1].Type != eventSurrendered {
		t.Fatalf("surrendered: %s %s", got, d.State().State)
	}
	records := map[int]int{}
	for _, o := range d.Originals() {
		records[o.Serial]++
		if o.Record != records[o.Serial] {
			t.Fatalf("record numbers: %+v", o)
		}
	}
	if records[1] != 2 || records[2] != 3 || records[3] != 2 {
		t.Fatalf("records of the three originals: %v", records)
	}
	if err := d.validate(); err != nil {
		t.Fatal(err)
	}
	d, _ = seaBill(t, 2)
	if err := errors.Join(d.ReturnOriginal(1, "at the counter", "doc-1", t0.Add(time.Hour)), d.ReturnOriginal(2, "at the counter", "doc-1", t0.Add(time.Hour)),
		d.SurrenderOriginals("telex release", "doc-1", t0.Add(2*time.Hour))); err != nil {
		t.Fatalf("surrendered with every original already returned: %v", err)
	}
	if got := statuses(d, 1); got != "SURRENDERED_AT_ORIGIN SURRENDERED_AT_ORIGIN" || d.State().State != StateSurrendered || len(d.Originals()) != 6 {
		t.Fatalf("surrendered after both were returned: %s %s", got, d.State().State)
	}
}

func amendment1(t *testing.T, o []OrderParties) Content {
	t.Helper()
	c, err := OceanHouseContent(KindHBL, "DOC-L", "SH-L", oceanFacts(2), issuer, o, Entered{PaymentIndicator: PaymentPrepaid, Release: ReleaseOriginals,
		Originals: 1})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestABillWithoutOriginalsHasNone(t *testing.T) {
	d, _ := seaBill(t, 0)
	if len(d.Originals()) != 0 {
		t.Fatalf("an EXPRESS bill has originals: %+v", d.Originals())
	}
	for name, err := range map[string]error{
		"surrendered": d.SurrenderOriginals("telex", "doc-1", t0),
		"returned":    d.ReturnOriginal(1, "at the counter", "doc-1", t0),
	} {
		if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := d.Cancel("a mistake", "doc-1", t0); err != nil {
		t.Fatalf("an EXPRESS bill is cancelled as before: %v", err)
	}
}

// A return and a surrender are not dated before the latest record of an original: the business time of the commands
// that write originals counts for the document's last change. The same instant is accepted.
func TestOriginalsAreNotDatedBeforeTheirLatestRecord(t *testing.T) {
	h := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Hour) }
	d, _ := seaBill(t, 2)
	if err := d.ReturnOriginal(1, "at the counter", "doc-1", h(5)); err != nil {
		t.Fatal(err)
	}
	if err := d.ReturnOriginal(2, "at the counter", "doc-1", h(4)); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("returned before the latest record of another original: %v", err)
	}
	if err := d.ReturnOriginal(2, "at the counter", "doc-1", h(5)); err != nil {
		t.Fatalf("returned at the instant of the latest record: %v", err)
	}
	d, _ = seaBill(t, 2)
	if err := d.ReturnOriginal(1, "at the counter", "doc-1", h(5)); err != nil {
		t.Fatal(err)
	}
	if err := d.SurrenderOriginals("telex release", "doc-1", h(4)); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("surrendered before the latest record of an original: %v", err)
	}
	if err := d.SurrenderOriginals("telex release", "doc-1", h(5)); err != nil || d.State().State != StateSurrendered {
		t.Fatalf("surrendered at the instant of the latest record: %v", err)
	}
}

// complete holds a house sea document's release and number of originals together, and a consignee to order to a text of
// the form COM holds; the build refuses the same texts, naming the order. Each wrong content is a valid one with the field
// under test changed.
func TestAHouseSeaDocumentIsCompleteOrNot(t *testing.T) {
	notify := party("notify", "DE", 1)
	build := func(kind Kind, o []OrderParties, e Entered) Content {
		t.Helper()
		c, err := OceanHouseContent(kind, "DOC-L", "SH-L", oceanFacts(1), issuer, o, e)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	bill := build(KindHBL, seaOrders(toOrder("TO ORDER OF BANK OF CHINA"), notify), Entered{PaymentIndicator: PaymentPrepaid, Release: ReleaseOriginals, Originals: 3})
	express := build(KindHBL, seaOrders(party("consignee", "DE", 1), nil), entered)
	waybill := build(KindHouseSeaWaybill, seaOrders(party("consignee", "DE", 1), nil), entered)
	text := func(s string) func(*Content) { return func(c *Content) { c.Consignee.ToOrder = s } }
	count := func(n int) func(*Content) { return func(c *Content) { c.OriginalCount = n } }
	for name, tc := range map[string]struct {
		base   Content
		change func(*Content)
		want   error
	}{
		"a bill to order as built":       {bill, func(*Content) {}, nil},
		"a bill to order, TO ORDER only": {bill, text("TO ORDER"), nil},
		"a text of 500 characters":       {bill, text("TO ORDER OF " + strings.Repeat("X", 500-12)), nil},
		"one original":                   {bill, count(1), nil},
		"nine originals":                 {bill, count(9), nil},
		"an EXPRESS bill as built":       {express, func(*Content) {}, nil},
		"a house sea waybill as built":   {waybill, func(*Content) {}, nil},
		"to order on EXPRESS":            {bill, func(c *Content) { c.ReleaseType, c.OriginalCount = ReleaseExpress, 0 }, ErrIncomplete},
		"to order on a house sea waybill": {bill, func(c *Content) {
			c.Kind, c.ReleaseType, c.OriginalCount = string(KindHouseSeaWaybill), ReleaseSeaWaybill, 0
		}, ErrIncomplete},
		"to order without a notify party":         {bill, func(c *Content) { c.Notify = nil }, ErrIncomplete},
		"a text of blanks":                        {bill, text("   "), ErrIncomplete},
		"a text in lower case":                    {bill, text("to order"), ErrIncomplete},
		"a name that is not a text to order":      {bill, text("ACME GMBH, HAMBURG"), ErrIncomplete},
		"a text of 501 characters":                {bill, text("TO ORDER OF " + strings.Repeat("X", 501-12)), ErrIncomplete},
		"a text with a line break":                {bill, text("TO ORDER OF BANK\nOF CHINA"), ErrIncomplete},
		"a text ending in a line break":           {bill, text("TO ORDER\n"), ErrIncomplete},
		"a text with a tab":                       {bill, text("TO ORDER OF BANK\tOF CHINA"), ErrIncomplete},
		"a text that names no holder":             {bill, text("TO ORDER OF "), ErrIncomplete},
		"ORIGINALS with no originals":             {bill, count(0), ErrIncomplete},
		"ORIGINALS with ten originals":            {bill, count(10), ErrIncomplete},
		"EXPRESS with two originals":              {express, count(2), ErrIncomplete},
		"a house sea waybill with one original":   {waybill, count(1), ErrIncomplete},
		"a house sea waybill released as EXPRESS": {waybill, func(c *Content) { c.ReleaseType = ReleaseExpress }, ErrIncomplete},
		"an HBL released as SEA_WAYBILL":          {express, func(c *Content) { c.ReleaseType = ReleaseSeaWaybill }, ErrIncomplete},
		"an HBL released to its named consignee":  {express, func(c *Content) { c.ReleaseType = ReleaseConsigneeNamed }, ErrIncomplete},
	} {
		c := tc.base
		tc.change(&c)
		if err := c.complete(); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	for name, s := range map[string]string{
		"blanks":          "   ",
		"lower case":      "to order",
		"a name":          "ACME GMBH, HAMBURG",
		"501 characters":  "TO ORDER OF " + strings.Repeat("X", 501-12),
		"a line break":    "TO ORDER OF BANK\nOF CHINA",
		"a tab":           "TO ORDER OF BANK\tOF CHINA",
		"no holder named": "TO ORDER OF ",
	} {
		_, err := OceanHouseContent(KindHBL, "DOC-L", "SH-L", oceanFacts(1), issuer, seaOrders(toOrder(s), notify),
			Entered{PaymentIndicator: PaymentPrepaid, Release: ReleaseOriginals, Originals: 3})
		if !errors.Is(err, ErrIncomplete) || !strings.Contains(err.Error(), "O-L") {
			t.Errorf("built with the consignee text %s: %v", name, err)
		}
	}
}

// An air waybill has no originals and no release but its named consignee: a refresh or an amendment that names an
// original or another release for a HAWB is refused, and the release the latest version carries over is accepted.
func TestAnAirWaybillTakesNoOriginals(t *testing.T) {
	for name, tc := range map[string]struct {
		entered Entered
		want    error
	}{
		"one original":                          {Entered{PaymentIndicator: PaymentPrepaid, Originals: 1}, ErrInvalidChange},
		"released as ORIGINALS":                 {Entered{PaymentIndicator: PaymentPrepaid, Release: ReleaseOriginals}, ErrInvalidChange},
		"released as ORIGINALS of one":          {Entered{PaymentIndicator: PaymentPrepaid, Release: ReleaseOriginals, Originals: 1}, ErrInvalidChange},
		"released as EXPRESS":                   {Entered{PaymentIndicator: PaymentPrepaid, Release: ReleaseExpress}, ErrInvalidChange},
		"released to the named consignee":       {Entered{PaymentIndicator: PaymentPrepaid, Release: ReleaseConsigneeNamed}, nil},
		"released to the named consignee, none": {Entered{PaymentIndicator: PaymentPrepaid, Release: ReleaseConsigneeNamed, Originals: 0}, nil},
		"no release":                            {entered, nil},
	} {
		for kind, build := range map[string]func(Entered) (Content, error){
			"HAWB": func(e Entered) (Content, error) { return HouseContent("DOC-H1", "SH-A", facts(1), issuer, orders(), e) },
			"MAWB": func(e Entered) (Content, error) { return common(KindMAWB, "DOC-M1", facts(1), issuer, e) },
		} {
			c, err := build(tc.entered)
			if !errors.Is(err, tc.want) || (err == nil && c.ReleaseType != ReleaseConsigneeNamed) {
				t.Errorf("%s of a %s: %v, release %s", name, kind, err, c.ReleaseType)
			}
		}
	}
}
