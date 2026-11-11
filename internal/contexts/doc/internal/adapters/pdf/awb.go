package pdf

import (
	"fmt"
	"strings"
	"time"

	app "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/application"
	doc "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/domain"
)

// Template names the layout below; a change of layout is a new template.
const Template = "IDP-AWB-V1"

// Renderer renders air waybill versions under Template.
type Renderer struct{}

var _ app.Renderer = Renderer{}

func (Renderer) Template() string { return Template }

var chinaTime = time.FixedZone("UTC+8", 8*3600)

func kg(grams int64) string {
	return strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintf("%.1f", float64(grams)/1000), "0"), ".")
}

func m3(cm3 int64) string {
	if cm3 == 0 {
		return ""
	}
	return fmt.Sprintf("%.3f M3", float64(cm3)/1_000_000)
}

func partyLines(p doc.Party) string {
	var b strings.Builder
	name := p.LegalName
	if p.EnglishName != "" && p.EnglishName != p.LegalName {
		name = p.EnglishName + "\n" + p.LegalName
	}
	b.WriteString(name)
	if a := p.Address; a != nil {
		for _, l := range a.Lines {
			b.WriteString("\n" + l)
		}
		city := strings.TrimSpace(strings.Join([]string{a.Postcode, a.City, a.Region}, " "))
		b.WriteString("\n" + strings.TrimSpace(city+" "+a.CountryCode))
	} else if p.CountryCode != "" {
		b.WriteString("\n" + p.CountryCode)
	}
	for _, i := range p.Identifiers {
		v := i.Value
		if i.None {
			v = "NONE"
		}
		b.WriteString(fmt.Sprintf("\n%s %s: %s", i.Country, i.Type, v))
	}
	return b.String()
}

// box draws a labelled box and its text, top left at x, top.
func box(p *Page, x, top, w, h float64, label, text string) {
	p.Rect(x, top-h, w, h)
	p.Text(x+4, top-10, 6.5, label)
	p.Wrap(x+4, top-21, w-8, 8.5, text)
}

// Render lays out one version: the issuer, parties, route, flights, cargo, security status, issue place and date;
// a MAWB lists its houses; a version that is not issued carries a draft mark; a dispatch copy lists the contacts
// provided with consent.
func (Renderer) Render(r app.Rendering) ([]byte, error) {
	c := r.Doc
	page := &Page{}
	pages := []*Page{page}
	const left, width = 36.0, PageWidth - 72
	half := width / 2
	title := "HOUSE AIR WAYBILL"
	if c.Kind == string(doc.KindMAWB) {
		title = "AIR WAYBILL (MASTER)"
	}
	top := PageHeight - 40
	page.Text(left, top, 14, title)
	page.Text(left+half, top, 14, c.Number)
	page.Text(left, top-14, 7.5, "Not negotiable. Release to the named consignee. "+c.ReleaseType)
	if c.Kind == string(doc.KindHAWB) {
		page.Text(left+half, top-14, 7.5, "Master air waybill "+c.MAWB)
	}
	top -= 26
	box(page, left, top, half, 96, "Shipper's name and address", partyLines(c.Shipper))
	capacity := "As consolidator"
	if c.IssuerCapacity == "AGENT_OF_CARRIER" {
		capacity = fmt.Sprintf("As agent of the carrier %s (prefix %s)", c.Airline.Designator, c.Airline.AWBPrefix)
	}
	box(page, left+half, top, half, 96, "Issued by", c.Issuer.Name+"\n"+capacity+"\n"+c.Issuer.Jurisdiction)
	top -= 96
	box(page, left, top, half, 96, "Consignee's name and address", partyLines(c.Consignee))
	notify := ""
	if c.Notify != nil {
		notify = partyLines(*c.Notify)
	}
	box(page, left+half, top, half, 96, "Notify party", notify)
	top -= 96
	var flights []string
	for _, f := range c.Flights {
		flights = append(flights, fmt.Sprintf("%s %s-%s %s", f.Flight, f.From.IATA, f.To.IATA, f.ETD.In(chinaTime).Format("2006-01-02")))
	}
	box(page, left, top, half/2, 40, "Airport of departure", c.Origin.IATA+"  "+c.Origin.Code)
	box(page, left+half/2, top, half/2, 40, "Airport of destination", c.Destination.IATA+"  "+c.Destination.Code)
	box(page, left+half, top, half, 40, "Flights", strings.Join(flights, "\n"))
	top -= 40
	box(page, left, top, half, 40, "Accounting information", "Freight "+c.PaymentIndicator)
	security := c.Security.Status
	for _, s := range c.Security.Screenings {
		security += fmt.Sprintf("\n%s %s %s %s", s.Screener, s.Method, s.Reference, s.ScreenedAt.In(chinaTime).Format("2006-01-02 15:04"))
	}
	box(page, left+half, top, half, 40, "Security status", security)
	top -= 40
	box(page, left, top, width, 30, "Handling information", c.HandlingInformation)
	top -= 30

	// cargo
	page.Rect(left, top-26, width, 26)
	cols := []struct {
		x     float64
		label string
	}{{0, "Pieces"}, {50, "Gross weight (kg)"}, {130, "Chargeable weight (kg)"}, {225, "Nature and quantity of goods (incl. volume)"}}
	for _, col := range cols {
		page.Text(left+4+col.x, top-10, 6.5, col.label)
	}
	page.Text(left+4, top-21, 8.5, fmt.Sprint(c.Cargo.Pieces))
	page.Text(left+54, top-21, 8.5, kg(c.Cargo.GrossGrams))
	page.Text(left+134, top-21, 8.5, kg(c.Cargo.ChargeableGrams))
	page.Text(left+229, top-21, 8.5, m3(c.Cargo.VolumeCm3))
	top -= 26
	y := top - 12
	newPage := func() {
		page = &Page{}
		pages = append(pages, page)
		page.Text(left, PageHeight-40, 10, title+" "+c.Number+" (continued)")
		y = PageHeight - 60
	}
	for _, l := range c.Cargo.Lines {
		if y < 150 {
			newPage()
		}
		text := fmt.Sprintf("%d %s %s", l.Pieces, l.PackageType, l.GoodsDescription)
		if l.HAWB != "" {
			text = fmt.Sprintf("HAWB %s  %d pcs  %s kg  %s", l.HAWB, l.Pieces, kg(l.GrossGrams), l.GoodsDescription)
		}
		y = page.Wrap(left+4, y, width-8, 8.5, text)
	}
	if c.Kind == string(doc.KindMAWB) {
		if y < 150 {
			newPage()
		}
		y = page.Wrap(left+4, y-4, width-8, 8.5, fmt.Sprintf("CONSOLIDATION AS PER ATTACHED MANIFEST: %d HOUSE(S)", len(c.Cargo.Lines)))
	}
	if len(r.Contacts) > 0 {
		if y < 170 {
			newPage()
		}
		y = page.Wrap(left+4, y-8, width-8, 8, "Contacts:")
		for _, k := range r.Contacts {
			var parts []string
			for _, s := range []string{k.Name, k.Phone, k.Mobile, k.Email} {
				if strings.TrimSpace(s) != "" {
					parts = append(parts, s)
				}
			}
			y = page.Wrap(left+12, y, width-16, 8, k.Role+": "+strings.Join(parts, ", "))
		}
	}

	// execution and identity
	page.Rect(left, 70, width, 50)
	executed := "Not issued"
	if !r.IssuedAt.IsZero() {
		executed = "Executed on " + r.IssuedAt.In(chinaTime).Format("2006-01-02") + " at " + c.IssuePlace.IATA + " (" + c.IssuePlace.Code + ")"
	}
	page.Text(left+4, 108, 8.5, executed)
	page.Text(left+4, 94, 8.5, "Signature of the issuing carrier or its agent: "+c.Issuer.Name)
	page.Text(left+4, 78, 6.5, fmt.Sprintf("Document %s version %d (%s), content SHA-256 %s", c.DocumentID, r.Version, r.Status, r.SHA256))
	page.Text(left, 56, 6.5, fmt.Sprintf("Source %s %s version %d; template %s", c.Source.Contract, c.Source.MovementID, c.Source.Version, Template))
	if r.Status != doc.VersionIssued {
		for _, p := range pages {
			p.Watermark(40, r.Status+" - NOT VALID")
		}
	}
	return Document(title+" "+c.Number, pages), nil
}
