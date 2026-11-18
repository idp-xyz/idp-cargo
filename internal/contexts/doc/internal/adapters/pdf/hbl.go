package pdf

import (
	"fmt"
	"strings"

	app "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/application"
	doc "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/domain"
)

// TemplateHBL names the layout of the house bill of lading and the house sea waybill; a change of layout is a new
// template.
const TemplateHBL = "IDP-HBL-V1"

// joined joins the parts that are not blank with a space.
func joined(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " ")
}

// containerLine is one container of a bill: its number, size type and seal, then what it holds; a part that is empty
// or zero is left out.
func containerLine(k doc.ShippedContainer) string {
	head := k.Number
	if k.SizeType != "" {
		head += " " + k.SizeType
	}
	if k.SealNumber != "" {
		head += " seal " + k.SealNumber
	}
	var held []string
	if k.Packs > 0 {
		held = append(held, fmt.Sprintf("%d packages", k.Packs))
	}
	if k.GrossGrams > 0 {
		held = append(held, kg(k.GrossGrams)+" kg")
	}
	if v := m3(k.VolumeCm3); v != "" {
		held = append(held, v)
	}
	if len(held) == 0 {
		return head
	}
	return head + ": " + strings.Join(held, ", ")
}

// renderHBL lays out a house bill of lading or a house sea waybill under TemplateHBL: the issuer, parties, route and
// vessel, cargo and containers, and the execution box. A rendering is evidence of what was printed, never the
// document: for a bill issued as originals the PDF is a copy, and the system prints no originals. Boxes, continuation
// pages, the mark of a version that is not issued and the contacts of a dispatch copy are as on the air waybill.
func renderHBL(r app.Rendering) ([]byte, error) {
	c := r.Doc
	if !(c.Kind == string(doc.KindHBL) && (c.ReleaseType == doc.ReleaseOriginals || c.ReleaseType == doc.ReleaseExpress)) &&
		!(c.Kind == string(doc.KindHouseSeaWaybill) && c.ReleaseType == doc.ReleaseSeaWaybill) {
		return nil, fmt.Errorf("%w: %s released as %q", doc.ErrNoTemplate, c.Kind, c.ReleaseType)
	}
	issued := !r.IssuedAt.IsZero()
	page := &Page{}
	pages := []*Page{page}
	const left, width = 36.0, PageWidth - 72
	half := width / 2
	title := "BILL OF LADING"
	if c.Kind == string(doc.KindHouseSeaWaybill) {
		title = "SEA WAYBILL"
	}
	top := PageHeight - 40
	page.Text(left, top, 14, title)
	page.Text(left+half, top, 14, c.Number)
	// what the page says of the originals is in the future until the version is issued
	switch c.ReleaseType {
	case doc.ReleaseOriginals:
		when := "to be issued"
		if issued {
			when = "issued"
		}
		page.Text(left, top-14, 7.5, fmt.Sprintf("COPY - NOT NEGOTIABLE. Original bills of lading %s: %d", when, c.OriginalCount))
	case doc.ReleaseExpress:
		will := "will be"
		if issued {
			will = "is"
		}
		page.Text(left, top-14, 7.5, "EXPRESS RELEASE - NOT NEGOTIABLE. No original bill of lading "+will+" issued.")
	case doc.ReleaseSeaWaybill:
		page.Text(left, top-14, 7.5, "NOT NEGOTIABLE. Delivery to the named consignee.")
	}
	page.Text(left, top-24, 7.5, "Master bill of lading "+c.MasterBill)
	top -= 36
	var rest []overflow
	top = pair(page, left, top, half, half, 96, partyLinesMax, "Shipper", partyLines(c.Shipper), "Issued by",
		c.Issuer.Name+"\nAs carrier (NVOCC)\n"+c.Issuer.Jurisdiction, &rest)
	consignee := c.Consignee.ToOrder
	if consignee == "" {
		consignee = partyLines(c.Consignee)
	}
	notify := ""
	if c.Notify != nil {
		notify = partyLines(*c.Notify)
	}
	top = pair(page, left, top, half, half, 96, partyLinesMax, "Consignee", consignee, "Notify party", notify, &rest)
	quarter := width / 4
	places := []struct{ label, text string }{{"Place of receipt", c.PlaceOfReceipt}, {"Port of loading", c.PortOfLoading},
		{"Port of discharge", c.PortOfDischarge}, {"Vessel and voyage", joined(c.Vessel, c.Voyage)}}
	shown := make([]string, len(places))
	route := 40.0
	for i, p := range places {
		shown[i] = clip(p.text, quarter, routeLinesMax, p.label, &rest)
		route = max(route, height(quarter, 40, shown[i]))
	}
	for i, p := range places {
		box(page, left+float64(i)*quarter, top, quarter, route, p.label, shown[i])
	}
	top -= route
	onBoard := "Not stated"
	if !c.OnBoardAt.IsZero() {
		onBoard = c.OnBoardAt.In(chinaTime).Format("2006-01-02")
	}
	top = pair(page, left, top, half, half, 40, routeLinesMax, "Freight", "Freight "+c.PaymentIndicator, "Shipped on board", onBoard, &rest)
	handlingText := clip(c.HandlingInformation, width, handlingMax, "Handling information", &rest)
	handling := height(width, 30, handlingText)
	box(page, left, top, width, handling, "Handling information", handlingText)
	top -= handling

	// cargo
	page.Rect(left, top-26, width, 26)
	for _, col := range []struct {
		x     float64
		label string
	}{{0, "Packages"}, {50, "Gross weight (kg)"}, {130, "Measurement"}} {
		page.Text(left+4+col.x, top-10, 6.5, col.label)
	}
	page.Text(left+4, top-21, 8.5, fmt.Sprint(c.Cargo.Pieces))
	page.Text(left+54, top-21, 8.5, kg(c.Cargo.GrossGrams))
	page.Text(left+134, top-21, 8.5, m3(c.Cargo.VolumeCm3))
	top -= 26
	y := top - 12
	// write puts a text below the last, on a new page when it would reach the execution box or the foot of the page.
	write := func(x, gap, w, size float64, text string) {
		need := float64(len(wrap(text, w, size))) * size * lineGap
		if y-gap-need < signature {
			page.Text(left, bottom-14, 6.5, "Continued on the next page")
			page = &Page{}
			pages = append(pages, page)
			page.Text(left, PageHeight-40, 10, title+" "+c.Number+" (continued)")
			y, gap = PageHeight-60, 0
		}
		y = page.Wrap(x, y-gap, w, size, text)
	}
	// a house of several orders has one line per order with the house's goods: they are printed once
	printed := map[string]bool{}
	for _, l := range c.Cargo.Lines {
		count := ""
		if l.Pieces != 0 {
			count = fmt.Sprint(l.Pieces)
		}
		text := joined(count, l.PackageType, l.GoodsDescription)
		if !printed[text] {
			printed[text] = true
			write(left+4, 0, width-8, boxText, text)
		}
	}
	for _, k := range c.Containers {
		write(left+4, 0, width-8, boxText, containerLine(k))
	}
	for _, o := range rest {
		write(left+4, 8, width-8, boxText, o.label+" (continued):")
		for _, line := range strings.Split(o.text, "\n") {
			write(left+12, 0, width-16, boxText, line)
		}
	}
	for i, k := range r.Contacts {
		if i == 0 {
			write(left+4, 8, width-8, 8, "Contacts:")
		}
		var parts []string
		for _, s := range []string{k.Name, k.Phone, k.Mobile, k.Email} {
			if strings.TrimSpace(s) != "" {
				parts = append(parts, s)
			}
		}
		write(left+12, 0, width-16, 8, k.Role+": "+strings.Join(parts, ", "))
	}

	// execution and identity
	page.Rect(left, 70, width, 50)
	executed := "Not issued"
	if issued {
		executed = "Issued on " + r.IssuedAt.In(chinaTime).Format("2006-01-02") + " at " + c.IssuePlace.Code
	}
	page.Text(left+4, 108, 8.5, executed)
	if issued {
		page.Text(left+4, 94, 8.5, "Signed for the carrier: "+c.Issuer.Name)
	}
	page.Text(left+4, 78, 6.5, fmt.Sprintf("Document %s version %d (%s), content SHA-256 %s", c.DocumentID, r.Version, r.Status, r.SHA256))
	page.Text(left, 56, 6.5, fmt.Sprintf("Source %s %s version %d; template %s", c.Source.Contract, c.Source.MovementID, c.Source.Version, TemplateHBL))
	if r.Status != doc.VersionIssued {
		for _, p := range pages {
			p.Watermark(40, r.Status+" - NOT VALID")
		}
	}
	return Document(title+" "+c.Number, pages), nil
}
