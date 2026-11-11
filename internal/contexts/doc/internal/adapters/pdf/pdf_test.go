package pdf

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	app "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/application"
	doc "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/domain"
)

var t0 = time.Date(2026, 11, 11, 8, 0, 0, 0, time.UTC)

func house(lines int) app.Rendering {
	party := func(id, name, country string) doc.Party {
		return doc.Party{PartyID: id, LegalName: name, EnglishName: "Party " + id, CountryCode: country,
			Address:     &doc.Address{Lines: []string{"1 Road (Building A)"}, City: "City", CountryCode: country},
			Identifiers: []doc.Identifier{{Type: "CN_USCC", Country: "CN", Value: "91310000MA1FL4XQ3X"}}}
	}
	c := doc.Content{Kind: string(doc.KindHAWB), DocumentID: "DOC-H1", Number: "SHA0000001", MAWB: "781-12345675", IssuerCapacity: "CONSOLIDATOR",
		Issuer: doc.Issuer{Name: "上海示例国际货运代理有限公司", Jurisdiction: "CN"}, IssuePlace: doc.Airport{Code: "CNPVG", IATA: "PVG"}, ReleaseType: "CONSIGNEE_NAMED",
		PaymentIndicator: "PREPAID", Source: doc.SourceRef{Contract: "AirDocFacts v1", MovementID: "AM-1", Version: 1},
		Airline: doc.Airline{Designator: "MU", AWBPrefix: "781"}, Origin: doc.Airport{Code: "CNPVG", IATA: "PVG"}, Destination: doc.Airport{Code: "USJFK", IATA: "JFK"},
		Flights: []doc.Flight{{Flight: "MU587", From: doc.Airport{IATA: "PVG"}, To: doc.Airport{IATA: "JFK"}, ETD: t0}},
		Shipper: party("shipper", "上海发货人有限公司", "CN"), Consignee: party("consignee", "Consignee Inc.", "US"),
		Cargo:               doc.Cargo{Pieces: 6, GrossGrams: 318_000, VolumeCm3: 1_000_000, ChargeableGrams: 318_500},
		Security:            doc.Security{Status: "SPX", Screenings: []doc.Screening{{Screener: "PVG", Method: "XRAY", Reference: "AQ-1", ScreenedAt: t0}}},
		HandlingInformation: "Keep dry"}
	for i := range lines {
		c.Cargo.Lines = append(c.Cargo.Lines, doc.CargoLine{Pieces: 6, PackageType: "CT", GoodsDescription: fmt.Sprintf("Cartons of electronics, line %d", i+1)})
	}
	return app.Rendering{Doc: c, Version: 1, SHA256: strings.Repeat("ab", 32), Status: doc.VersionIssued, IssuedAt: t0}
}

// structure checks a PDF the way a reader starts: the header, every object at the offset the xref table gives, startxref
// at the table, and a /Length for each stream that matches its bytes.
func structure(t *testing.T, pdf []byte) (objects int, pages int) {
	t.Helper()
	if !bytes.HasPrefix(pdf, []byte("%PDF-1.4\n")) || !bytes.HasSuffix(pdf, []byte("%%EOF\n")) {
		t.Fatal("header or trailer")
	}
	m := regexp.MustCompile(`startxref\n(\d+)\n%%EOF\n$`).FindSubmatch(pdf)
	if m == nil {
		t.Fatal("no startxref")
	}
	xref, _ := strconv.Atoi(string(m[1]))
	if !bytes.HasPrefix(pdf[xref:], []byte("xref\n0 ")) {
		t.Fatalf("startxref %d does not point at the table", xref)
	}
	table := strings.Split(string(pdf[xref:]), "\n")
	size, _ := strconv.Atoi(strings.Fields(table[1])[1])
	for i := 1; i < size; i++ {
		offset, _ := strconv.Atoi(table[2+i][:10])
		if want := fmt.Sprintf("%d 0 obj\n", i); !bytes.HasPrefix(pdf[offset:], []byte(want)) {
			t.Fatalf("object %d is not at %d", i, offset)
		}
	}
	for _, s := range regexp.MustCompile(`(?s)<< /Length (\d+) >>\nstream\n(.*?)endstream`).FindAllSubmatch(pdf, -1) {
		if n, _ := strconv.Atoi(string(s[1])); n != len(s[2]) {
			t.Fatalf("stream length %d, %d bytes", n, len(s[2]))
		}
	}
	return size - 1, bytes.Count(pdf, []byte("/Type /Page "))
}

func TestHouseWaybillRendersToAValidDeterministicPDF(t *testing.T) {
	r := house(3)
	pdf, err := Renderer{}.Render(r)
	if err != nil {
		t.Fatal(err)
	}
	objects, pages := structure(t, pdf)
	if objects != 9 || pages != 1 {
		t.Fatalf("%d objects, %d pages", objects, pages)
	}
	again, _ := Renderer{}.Render(house(3))
	if !bytes.Equal(pdf, again) {
		t.Fatal("the same version renders to other bytes")
	}
	s := string(pdf)
	for _, want := range []string{"(HOUSE AIR WAYBILL)", "(SHA0000001)", "(Master air waybill 781-12345675)", "(1 Road \\(Building A\\))", "(318)",
		"(318.5)", "(1.000 M3)", "(MU587 PVG-JFK 2026-11-11)", "(Executed on 2026-11-11 at PVG \\(CNPVG\\))", "/BaseFont /STSong-Light-UniGB-UCS2-H",
		"<4E0A6D77793A4F8B56FD96458D278FD04EE3740667099650516C53F8>"} { // 上海示例国际货运代理有限公司 in UCS-2
		if !strings.Contains(s, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(s, "NOT VALID") || strings.Contains(s, "Contacts:") || strings.Contains(s, "CreationDate") {
		t.Fatal("an issued preview without contacts carries no mark, contact or date")
	}
	r.Status, r.IssuedAt = doc.VersionDraft, time.Time{}
	r.Contacts = []app.CopyContact{{Role: "SHIPPER", Name: "张三", Mobile: "+86 138 0000 0000", Email: "zhang@example.cn"}}
	draft, _ := Renderer{}.Render(r)
	structure(t, draft)
	if !strings.Contains(string(draft), "stream\nq 0.85 g BT /F1 40.00 Tf 0.80 0.60 -0.60 0.80") {
		t.Error("the draft mark is drawn first, beneath the text")
	}
	for _, want := range []string{"(DRAFT - NOT VALID)", "(Not issued)", "(Contacts:)", "(SHIPPER: )", "<5F204E09>",
		"(, +86 138 0000 0000, zhang@example.cn)"} {
		if !strings.Contains(string(draft), want) {
			t.Errorf("draft copy missing %s", want)
		}
	}
	long, _ := Renderer{}.Render(house(120))
	if _, pages := structure(t, long); pages < 2 || !strings.Contains(string(long), "(HOUSE AIR WAYBILL SHA0000001 \\(continued\\))") {
		t.Fatalf("120 cargo lines on %d pages", pages)
	}
}

func TestWrapKeepsLinesWithinWidth(t *testing.T) {
	for _, s := range []string{"Cartons of electronics and accessories for retail stores in the United States", "上海示例国际货运代理有限公司上海浦东新区机场物流园区一号仓库"} {
		lines := wrap(s, 120, 8.5)
		if len(lines) < 2 || strings.Join(lines, "") == "" {
			t.Fatalf("%q: %v", s, lines)
		}
		for _, l := range lines {
			if Width(l, 8.5) > 120 && !strings.Contains(strings.TrimSpace(l), " ") && len([]rune(l)) > 1 {
				t.Errorf("%q is %.1f wide", l, Width(l, 8.5))
			}
		}
	}
	if got := runs("Tel 电话 +86"); len(got) != 3 || got[0] != "Tel " || got[1] != "电话" || got[2] != " +86" {
		t.Fatalf("runs %q", got)
	}
}
