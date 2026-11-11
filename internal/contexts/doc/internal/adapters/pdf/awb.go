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

// kg shows grams as kilograms to the tenth, rounded half up.
func kg(grams int64) string {
	tenths := (grams + 50) / 100
	if tenths%10 == 0 {
		return fmt.Sprint(tenths / 10)
	}
	return fmt.Sprintf("%d.%d", tenths/10, tenths%10)
}

// m3 shows cubic centimetres as cubic metres to the thousandth, rounded half up.
func m3(cm3 int64) string {
	if cm3 == 0 {
		return ""
	}
	milli := (cm3 + 500) / 1000
	return fmt.Sprintf("%d.%03d M3", milli/1000, milli%1000)
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

const (
	boxText   = 8.5
	lineGap   = 1.25
	signature = 130.0 // nothing is written below this on the last page: the execution box and footer
	bottom    = 50.0  // nor below this on other pages
)

// height is the height a box needs for its text, at least min.
func height(w, min float64, text string) float64 {
	return max(min, 21+float64(len(wrap(text, w-8, boxText)))*boxText*lineGap+2)
}

// box draws a labelled box and its text, top left at x, top.
func box(p *Page, x, top, w, h float64, label, text string) {
	p.Rect(x, top-h, w, h)
	p.Text(x+4, top-10, 6.5, label)
	p.Wrap(x+4, top-21, w-8, boxText, text)
}

// pair draws two boxes side by side as high as the taller needs, at least min, and returns the top below them.
func pair(p *Page, x, top, w1, w2, min float64, label1, text1, label2, text2 string) float64 {
	h := max(height(w1, min, text1), height(w2, min, text2))
	box(p, x, top, w1, h, label1, text1)
	box(p, x+w1, top, w2, h, label2, text2)
	return top - h
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
	capacity := "As consolidator"
	if c.IssuerCapacity == "AGENT_OF_CARRIER" {
		capacity = fmt.Sprintf("As agent of the carrier %s (prefix %s)", c.Airline.Designator, c.Airline.AWBPrefix)
	}
	top = pair(page, left, top, half, half, 96, "Shipper's name and address", partyLines(c.Shipper), "Issued by",
		c.Issuer.Name+"\n"+capacity+"\n"+c.Issuer.Jurisdiction)
	notify := ""
	if c.Notify != nil {
		notify = partyLines(*c.Notify)
	}
	top = pair(page, left, top, half, half, 96, "Consignee's name and address", partyLines(c.Consignee), "Notify party", notify)
	var flights []string
	for _, f := range c.Flights {
		flights = append(flights, fmt.Sprintf("%s %s-%s %s", f.Flight, f.From.IATA, f.To.IATA, f.ETD.In(chinaTime).Format("2006-01-02")))
	}
	airports := height(half, 40, strings.Join(flights, "\n"))
	box(page, left, top, half/2, airports, "Airport of departure", c.Origin.IATA+"  "+c.Origin.Code)
	box(page, left+half/2, top, half/2, airports, "Airport of destination", c.Destination.IATA+"  "+c.Destination.Code)
	box(page, left+half, top, half, airports, "Flights", strings.Join(flights, "\n"))
	top -= airports
	security := c.Security.Status
	for _, s := range c.Security.Screenings {
		security += fmt.Sprintf("\n%s %s %s %s", s.Screener, s.Method, s.Reference, s.ScreenedAt.In(chinaTime).Format("2006-01-02 15:04"))
	}
	top = pair(page, left, top, half, half, 40, "Accounting information", "Freight "+c.PaymentIndicator, "Security status", security)
	handling := height(width, 30, c.HandlingInformation)
	box(page, left, top, width, handling, "Handling information", c.HandlingInformation)
	top -= handling

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
	// write puts a text below the last, on a new page when it would reach the execution box (which a later page may
	// leave room for) or the foot of the page.
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
	for _, l := range c.Cargo.Lines {
		text := fmt.Sprintf("%d %s %s", l.Pieces, l.PackageType, l.GoodsDescription)
		if l.HAWB != "" {
			text = fmt.Sprintf("HAWB %s  %d pcs  %s kg  %s", l.HAWB, l.Pieces, kg(l.GrossGrams), l.GoodsDescription)
		}
		write(left+4, 0, width-8, boxText, text)
	}
	if c.Kind == string(doc.KindMAWB) {
		write(left+4, 4, width-8, boxText, fmt.Sprintf("CONSOLIDATION AS PER ATTACHED MANIFEST: %d HOUSE(S)", len(c.Cargo.Lines)))
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
