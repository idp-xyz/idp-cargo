package pdf

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	app "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/application"
	doc "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/domain"
)

// bill is an issued HBL of 3 originals, with a named consignee, a notify party, two cargo lines and two containers.
func bill() app.Rendering {
	party := func(id, name, country string) doc.Party {
		return doc.Party{PartyID: id, LegalName: name, EnglishName: "Party " + id, CountryCode: country, Named: true,
			Address: &doc.Address{Lines: []string{"1 Road"}, City: "City", CountryCode: country}}
	}
	notify := party("notify", "Notify Ltd.", "GB")
	c := doc.Content{Kind: string(doc.KindHBL), DocumentID: "DOC-S1", Number: "SHAS0000001", MasterBill: "MAEU123456789", Vessel: "MAERSK EXAMPLE", Voyage: "612E",
		Issuer: doc.Issuer{Name: "上海示例国际货运代理有限公司", Jurisdiction: "CN"}, IssuePlace: doc.Airport{Code: "CNSHA"}, ReleaseType: doc.ReleaseOriginals, OriginalCount: 3,
		PaymentIndicator: "PREPAID", Source: doc.SourceRef{Contract: "OceanDocFacts v1", MovementID: "SM-1", Version: 1},
		PlaceOfReceipt: "CNNGB", PortOfLoading: "CNSHA", PortOfDischarge: "USLAX", OnBoardAt: time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC),
		Shipper: party("shipper", "上海发货人有限公司", "CN"), Consignee: party("consignee", "Consignee Inc.", "US"), Notify: &notify,
		Cargo: doc.Cargo{Pieces: 60, GrossGrams: 1_250_500, VolumeCm3: 2_500_000,
			Lines: []doc.CargoLine{{Pieces: 40, PackageType: "CT", GoodsDescription: "Cartons of toys"}, {Pieces: 20, PackageType: "PK", GoodsDescription: "Packages of tools"}}},
		Containers: []doc.ShippedContainer{{Number: "MSKU1234565", SizeType: "40HC", SealNumber: "SL998877", Packs: 60, GrossGrams: 1_250_500, VolumeCm3: 2_500_000},
			{Number: "TEMU7654321", SizeType: "20GP", SealNumber: "SL112233", Packs: 5, GrossGrams: 100_000}},
		HandlingInformation: "Keep dry"}
	return app.Rendering{Doc: c, Version: 1, SHA256: strings.Repeat("cd", 32), Status: doc.VersionIssued, IssuedAt: t0}
}

func renderBill(t *testing.T, r app.Rendering) string {
	t.Helper()
	pdf, err := Renderer{}.Render(r)
	if err != nil {
		t.Fatal(err)
	}
	structure(t, pdf)
	return string(pdf)
}

func wants(t *testing.T, s string, in ...string) {
	t.Helper()
	for _, w := range in {
		if !strings.Contains(s, w) {
			t.Errorf("missing %s", w)
		}
	}
}

func TestAnIssuedBillOfLadingRendersToAValidDeterministicPDF(t *testing.T) {
	pdf, err := Renderer{}.Render(bill())
	if err != nil {
		t.Fatal(err)
	}
	if objects, pages := structure(t, pdf); objects != 9 || pages != 1 {
		t.Fatalf("%d objects, %d pages", objects, pages)
	}
	again, _ := Renderer{}.Render(bill())
	if !bytes.Equal(pdf, again) {
		t.Fatal("the same version renders to other bytes")
	}
	wants(t, string(pdf), "(BILL OF LADING)", "(SHAS0000001)", "(COPY - NOT NEGOTIABLE. Original bills of lading issued: 3)", "(Master bill of lading MAEU123456789)",
		"(2026-11-13)", "(60)", "(1250.5)", "(2.500 M3)", "(40 CT Cartons of toys)",
		"(MSKU1234565 40HC seal SL998877: 60 packages, 1250.5 kg, 2.500 M3)", "(Issued on 2026-11-11 at CNSHA)", "(Signed for the carrier:", "template IDP-HBL-V1",
		"(Shipper)", "(Issued by)", "(Consignee)", "(Notify party)", "(Vessel and voyage)", "(Freight)", "(Shipped on board)", "(Handling information)",
		"(Packages)", "(Gross weight \\(kg\\))", "(Measurement)", "(As carrier \\(NVOCC\\))", "(CN)", "(Freight PREPAID)", "(Keep dry)")
	// each place follows its own label, and the boxes are in the order the page writes them; two boxes swapped break the chain
	at := 0
	for _, in := range []string{"(Place of receipt)", "(CNNGB)", "(Port of loading)", "(CNSHA)", "(Port of discharge)", "(USLAX)", "(Vessel and voyage)", "(MAERSK EXAMPLE 612E)"} {
		n := strings.Index(string(pdf)[at:], in)
		if n < 0 {
			t.Fatalf("%s is not after the text before it", in)
		}
		at += n + len(in)
	}
	if strings.Contains(string(pdf), "NOT VALID") || strings.Contains(string(pdf), "IDP-AWB-V1") {
		t.Error("an issued bill carries no mark and is not on the air template")
	}
}

func TestTheLineUnderTheTitleFollowsTheRelease(t *testing.T) {
	express := bill()
	express.Doc.ReleaseType, express.Doc.OriginalCount = doc.ReleaseExpress, 0
	e := renderBill(t, express)
	wants(t, e, "(BILL OF LADING)", "(EXPRESS RELEASE - NOT NEGOTIABLE. No original bill of lading is issued.)")
	waybill := bill()
	waybill.Doc.Kind, waybill.Doc.ReleaseType, waybill.Doc.OriginalCount = string(doc.KindHouseSeaWaybill), doc.ReleaseSeaWaybill, 0
	w := renderBill(t, waybill)
	wants(t, w, "(SEA WAYBILL)", "(NOT NEGOTIABLE. Delivery to the named consignee.)")
	for _, other := range []string{"Original bills of lading", "EXPRESS RELEASE", "BILL OF LADING"} {
		if strings.Contains(w, other) {
			t.Errorf("a sea waybill says %q", other)
		}
	}
	if strings.Contains(e, "Original bills of lading") || strings.Contains(e, "Delivery to the named consignee") || strings.Contains(e, "SEA WAYBILL") {
		t.Error("an express bill says another release's line")
	}
}

func TestAConsigneeToOrderPrintsTheOrderOnly(t *testing.T) {
	r := bill()
	r.Doc.Consignee = doc.Party{ToOrder: "TO ORDER OF BANK OF EXAMPLE"}
	s := renderBill(t, r)
	wants(t, s, "(TO ORDER OF BANK OF EXAMPLE)", "(Notify Ltd.)")
	if strings.Contains(s, "Consignee Inc.") || strings.Contains(s, "Party consignee") {
		t.Error("a consignee to order prints party lines")
	}
	if n := strings.Count(s, "(US)"); n != 0 {
		t.Errorf("%d lines of a country for the consignee", n)
	}
	named := renderBill(t, bill())
	wants(t, named, "(Party consignee)", "(Consignee Inc.)", "(1 Road)", "(City US)")
}

func TestADraftBillSaysWhatIsNotStated(t *testing.T) {
	r := bill()
	r.Status, r.IssuedAt, r.Doc.OnBoardAt = doc.VersionDraft, time.Time{}, time.Time{}
	s := renderBill(t, r)
	wants(t, s, "(Not stated)", "(Not issued)", "(DRAFT - NOT VALID)", "(COPY - NOT NEGOTIABLE. Original bills of lading to be issued: 3)")
	for _, not := range []string{"Issued on", "Signed for the carrier", "lading issued: 3"} {
		if strings.Contains(s, not) {
			t.Errorf("a draft says %q", not)
		}
	}
	r.Status = doc.VersionApproved
	approved := renderBill(t, r)
	wants(t, approved, "(APPROVED - NOT VALID)", "(Not issued)", "to be issued: 3")
	if strings.Contains(approved, "Signed for the carrier") {
		t.Error("an approved version is signed")
	}
	r.Doc.ReleaseType, r.Doc.OriginalCount, r.Status = doc.ReleaseExpress, 0, doc.VersionDraft
	express := renderBill(t, r)
	wants(t, express, "(EXPRESS RELEASE - NOT NEGOTIABLE. No original bill of lading will be issued.)")
	if strings.Contains(express, "is issued") {
		t.Error("a draft express bill says one is issued")
	}
	issued := bill()
	issued.Doc.ReleaseType, issued.Doc.OriginalCount = doc.ReleaseExpress, 0
	wants(t, renderBill(t, issued), "(EXPRESS RELEASE - NOT NEGOTIABLE. No original bill of lading is issued.)")
}

func TestALongBillContinuesOnFurtherPages(t *testing.T) {
	r := bill()
	r.Doc.Cargo.Lines = nil
	r.Doc.Containers = nil
	for i := range 80 {
		r.Doc.Cargo.Lines = append(r.Doc.Cargo.Lines, doc.CargoLine{Pieces: 2, PackageType: "CT", GoodsDescription: fmt.Sprintf("Cartons of electronics, line %d", i+1)})
		r.Doc.Containers = append(r.Doc.Containers, doc.ShippedContainer{Number: fmt.Sprintf("MSKU%07d", i), SizeType: "20GP", SealNumber: fmt.Sprintf("SL%d", i), Packs: 2})
	}
	r.Doc.HandlingInformation = strings.Repeat("Keep dry and away from heat, stow upright, do not stack more than two high. ", 12)
	r.Doc.Shipper.LegalName = "湖北测试集团"
	s := renderBill(t, r)
	_, pages := structure(t, []byte(s))
	if pages < 3 {
		t.Fatalf("a long bill on %d pages", pages)
	}
	wants(t, s, "(BILL OF LADING SHAS0000001 \\(continued\\))", "(Handling information \\(continued\\):)", "<"+ucs2("湖北测试集团")+">")
	for i := range 80 {
		wants(t, s, fmt.Sprintf("(2 CT Cartons of electronics, line %d)", i+1), fmt.Sprintf("(MSKU%07d 20GP seal SL%d: 2 packages)", i, i))
	}
	r.Status, r.IssuedAt = doc.VersionDraft, time.Time{}
	if n := strings.Count(renderBill(t, r), "(DRAFT - NOT VALID)"); n != pages {
		t.Errorf("%d marks on %d pages", n, pages)
	}
	for _, once := range []string{"(Issued on 2026-11-11 at CNSHA)", "(Signed for the carrier:", "(Source "} {
		if n := strings.Count(s, once); n != 1 {
			t.Errorf("%s printed %d times; the execution box is on the last page only", once, n)
		}
	}
	if n := strings.Count(s, "(Continued on the next page)"); n != pages-1 {
		t.Errorf("%d continuation notes on %d pages", n, pages)
	}
}

func TestACargoLineWithoutACountAndABareContainer(t *testing.T) {
	r := bill()
	r.Doc.Cargo.Lines = []doc.CargoLine{{PackageType: "CT", GoodsDescription: "Mixed cartons of goods"}}
	r.Doc.Containers = []doc.ShippedContainer{{Number: "MSKU1234565", Packs: 7}, {Number: "TEMU7654321", SizeType: "20GP", SealNumber: "SL1"}}
	s := renderBill(t, r)
	wants(t, s, "(CT Mixed cartons of goods)", "(MSKU1234565: 7 packages)", "(TEMU7654321 20GP seal SL1)")
	if strings.Contains(s, "(0 CT") || strings.Contains(s, "seal )") || strings.Contains(s, "MSKU1234565  ") {
		t.Error("an empty part is printed")
	}
}

func TestABillCopyListsItsContacts(t *testing.T) {
	r := bill()
	r.Contacts = []app.CopyContact{{Role: "SHIPPER", Name: "张三", Mobile: "+86 138 0000 0000", Email: "zhang@example.cn"}, {Role: "CONSIGNEE", Name: "Jane Doe", Phone: "+1 212 555 0100"}}
	s := renderBill(t, r)
	wants(t, s, "(Contacts:)", "(CONSIGNEE: Jane Doe, +1 212 555 0100)", "<5F204E09>", "(, +86 138 0000 0000, zhang@example.cn)")
	if strings.Contains(renderBill(t, bill()), "Contacts:") {
		t.Error("a preview lists contacts")
	}
}

func TestAHouseOfSeveralOrdersPrintsItsGoodsOnce(t *testing.T) {
	r := bill()
	same := doc.CargoLine{PackageType: "CT", GoodsDescription: "Cartons of one house"}
	r.Doc.Cargo.Lines = []doc.CargoLine{same, same, same}
	if n := strings.Count(renderBill(t, r), "(CT Cartons of one house)"); n != 1 {
		t.Errorf("three orders print the goods %d times", n)
	}
	r.Doc.Cargo.Lines = []doc.CargoLine{same, {PackageType: "CT", GoodsDescription: "Cartons of another house"}}
	s := renderBill(t, r)
	wants(t, s, "(CT Cartons of one house)", "(CT Cartons of another house)")
}

func TestAReleaseTheTemplateDoesNotKnowIsRefused(t *testing.T) {
	for name, c := range map[string]struct{ kind, release string }{"empty": {string(doc.KindHBL), ""}, "unknown": {string(doc.KindHBL), "SURRENDERED"},
		"an HBL as a sea waybill": {string(doc.KindHBL), doc.ReleaseSeaWaybill}, "a sea waybill as originals": {string(doc.KindHouseSeaWaybill), doc.ReleaseOriginals},
		"a sea waybill released express": {string(doc.KindHouseSeaWaybill), doc.ReleaseExpress}} {
		r := bill()
		r.Doc.Kind, r.Doc.ReleaseType = c.kind, c.release
		if pdf, err := (Renderer{}).Render(r); !errors.Is(err, doc.ErrNoTemplate) || pdf != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The release line and the master bill's line are on different baselines, and a master number of the longest length
// ends before the right margin.
func TestTheMasterBillHasALineOfItsOwn(t *testing.T) {
	r := bill()
	r.Doc.ReleaseType, r.Doc.OriginalCount = doc.ReleaseExpress, 0
	r.Doc.MasterBill = strings.Repeat("W", 35)
	s := renderBill(t, r)
	at := func(text string) (x, y float64) {
		m := regexp.MustCompile(`Tm \(` + regexp.QuoteMeta(text) + `\)`).FindStringIndex(s)
		if m == nil {
			t.Fatalf("no text %q", text)
		}
		var size float64
		line := s[strings.LastIndex(s[:m[0]], "BT"):m[0]]
		if _, err := fmt.Sscanf(line, "BT /F1 %f Tf 1 0 0 1 %f %f ", &size, &x, &y); err != nil || size != 7.5 {
			t.Fatalf("the text %q is not drawn at 7.5: %q %v", text, line, err)
		}
		return x, y
	}
	release := "EXPRESS RELEASE - NOT NEGOTIABLE. No original bill of lading is issued."
	master := "Master bill of lading " + strings.Repeat("W", 35)
	rx, ry := at(release)
	mx, my := at(master)
	if ry-my != 10 || mx != rx {
		t.Errorf("release line at %v,%v and master bill at %v,%v", rx, ry, mx, my)
	}
	if end := mx + Width(master, 7.5); end > PageWidth-36 {
		t.Errorf("the master bill ends at %.1f, past the margin", end)
	}
}
