package domain

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

var t0 = time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)

func airport(code, iata string) Airport {
	return Airport{Code: kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code}, IATA: iata}
}

var (
	pvg, jfk, anc = airport("CNPVG", "PVG"), airport("USJFK", "JFK"), airport("USANC", "ANC")
	mu            = Airline{PartyID: "MU", PartyRevision: 5, Designator: "MU", AWBPrefix: "781"}
)

func booking(version int, etd time.Time) Booking {
	return Booking{BookingID: "BK-1", Revision: int64(version + 2), AirConsol: true, InForce: true, ConsumerID: "C-1", LegalEntityID: "LE",
		Airline: mu, Origin: pvg, Destination: jfk, Edition: "E1", MAWB: "781-12345675", CapacityVersion: version, Lines: 1,
		Segments: []Segment{{Flight: "MU587", From: pvg, To: jfk, ETD: etd, ETA: etd.Add(14 * time.Hour)}}}
}

func plan(shipment string, version int) Plan {
	kg := kernel.Quantity{Unit: "KGM", Amount: 300}
	return Plan{ShipmentID: shipment, Revision: int64(version + 1), Status: "PLANNED", PlanVersion: version, LegalEntityID: "LE", Mode: "AIR",
		Origin: pvg.Code, Destination: jfk.Code, Window: kernel.ValidityInterval{From: t0.Add(48 * time.Hour), Until: t0.Add(96 * time.Hour)},
		Pieces: 6, GrossWeight: kg, Volume: kernel.Quantity{Unit: "DMQ", Amount: 2400},
		Parts:           []Part{{ObligationID: "OB-" + shipment, ObligationVersion: 1, OrderID: "O-" + shipment, Packs: 6, PackageType: "CT", GrossWeight: kg}},
		ConsolidationID: "C-1"}
}

func opened(t *testing.T) *Movement {
	t.Helper()
	m, err := OpenMovement("T", "AM-1", "C-1", booking(1, t0.Add(72*time.Hour)), "ops-1", t0)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestChargeableWeightAndUnits(t *testing.T) {
	for _, tc := range []struct{ grams, cm3, want int64 }{
		{300_000, 2_400_000, 400_000}, // 2.4 m3 is 400 kg volumetric
		{300_000, 0, 300_000},
		{100_100, 0, 100_500}, // rounded up to the next half kilogram
		{1, 1, 500},
		{50_000, 600_001, 100_500}, // 600,001 cm3 is 100,000.2 g volumetric
	} {
		if got := Chargeable(tc.grams, tc.cm3); got != tc.want {
			t.Errorf("Chargeable(%d g, %d cm3) = %d, want %d", tc.grams, tc.cm3, got, tc.want)
		}
	}
	if _, err := Grams(kernel.Quantity{Unit: "LBR", Amount: 1}); !errors.Is(err, ErrUnits) {
		t.Fatalf("pounds: %v", err)
	}
	if v, _ := CubicCentimetres(kernel.Quantity{Unit: "MTQ", Amount: 2}); v != 2_000_000 {
		t.Fatal("cubic metres")
	}
	if !ValidMAWB("781-12345675") || ValidMAWB("781-12345674") || ValidMAWB("78112345675") {
		t.Fatal("MAWB form")
	}
}

func TestOpenAndLinkFromTheBooking(t *testing.T) {
	etd := t0.Add(72 * time.Hour)
	for name, tc := range map[string]func(*Booking){
		"not in force":          func(b *Booking) { b.InForce = false },
		"another consolidation": func(b *Booking) { b.ConsumerID = "C-2" },
		"split booking":         func(b *Booking) { b.Lines = 2 },
		"other prefix":          func(b *Booking) { b.MAWB = "999-12345675" },
		"flight of another":     func(b *Booking) { b.Segments[0].Flight = "CA981" },
		"flights to elsewhere":  func(b *Booking) { b.Segments[0].To = anc },
	} {
		b := booking(1, etd)
		b.Segments = append([]Segment(nil), b.Segments...)
		tc(&b)
		if _, err := OpenMovement("T", "AM-1", "C-1", b, "ops-1", t0); !errors.Is(err, ErrInvalidBooking) {
			t.Errorf("%s: %v", name, err)
		}
	}
	m := opened(t)
	via := booking(2, etd.Add(24*time.Hour))
	via.Segments = []Segment{{Flight: "MU587", From: pvg, To: anc, ETD: etd.Add(24 * time.Hour)}, {Flight: "MU7001", From: anc, To: jfk, ETD: etd.Add(34 * time.Hour)}}
	if err := m.LinkBooking(booking(1, etd), "ops-1", t0.Add(time.Hour)); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("the same capacity version again: %v", err)
	}
	other := via
	other.MAWB = "781-23456786"
	if err := m.LinkBooking(other, "ops-1", t0.Add(time.Hour)); !errors.Is(err, ErrInvalidBooking) {
		t.Fatalf("another MAWB is another movement: %v", err)
	}
	if err := m.LinkBooking(via, "ops-1", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if c := m.Current(); c.CapacityVersion != 2 || len(c.Segments) != 2 || len(m.State().Links) != 2 {
		t.Fatalf("relinked: %+v", c)
	}
	if _, err := RestoreMovement(m.State()); err != nil {
		t.Fatal(err)
	}
}

func TestHouseWaybills(t *testing.T) {
	m := opened(t)
	at := t0.Add(time.Hour)
	for name, change := range map[string]func(*Plan){
		"draft":                func(p *Plan) { p.Status = "DRAFT" },
		"another consol":       func(p *Plan) { p.ConsolidationID = "C-2" },
		"another legal entity": func(p *Plan) { p.LegalEntityID = "LE-2" },
		"another destination":  func(p *Plan) { p.Destination = anc.Code },
		"by sea":               func(p *Plan) { p.Mode = "SEA" },
	} {
		p := plan("SH-1", 2)
		change(&p)
		if err := m.Associate(p, "SHA0000001", "ops-1", at); !errors.Is(err, ErrNotAttached) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := m.Associate(plan("SH-1", 2), "sha-1", "ops-1", at); !errors.Is(err, ErrInvalidHAWB) {
		t.Fatalf("a HAWB in lower case: %v", err)
	}
	if err := m.Associate(plan("SH-1", 2), "SHA0000001", "ops-1", at); err != nil {
		t.Fatal(err)
	}
	if err := m.Associate(plan("SH-2", 2), "SHA0000001", "ops-1", at); !errors.Is(err, ErrAlreadyAssociated) {
		t.Fatalf("one HAWB for two houses: %v", err)
	}
	if err := m.Associate(plan("SH-2", 2), "SHA0000002", "ops-1", at); err != nil {
		t.Fatal(err)
	}
	h := m.Houses()[0]
	if h.Facts.ChargeableGrams != 400_000 || h.Facts.Rule != ChargeableRule || h.Facts.Basis != BasisPlanned {
		t.Fatalf("facts: %+v", h.Facts)
	}
	if pieces, grams, cm3, chargeable := m.Totals(); pieces != 12 || grams != 600_000 || cm3 != 4_800_000 || chargeable != 800_000 {
		t.Fatalf("totals: %d %d %d %d", pieces, grams, cm3, chargeable)
	}
	if err := m.Refresh(plan("SH-1", 2), "ops-1", at); !errors.Is(err, ErrNothingToRefresh) {
		t.Fatalf("refresh at the same plan version: %v", err)
	}
	short := plan("SH-1", 3)
	short.Pieces, short.GrossWeight, short.Volume = 4, kernel.Quantity{Unit: "KGM", Amount: 200}, kernel.Quantity{}
	short.Parts[0].Packs, short.Parts[0].GrossWeight = 4, short.GrossWeight
	if err := m.Refresh(short, "ops-1", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.EndAssociation(plan("SH-2", 2), "ops-1", at.Add(time.Hour)); !errors.Is(err, ErrStillAttached) {
		t.Fatalf("end a house FUL still attaches: %v", err)
	}
	moved := plan("SH-2", 2)
	moved.ConsolidationID = "C-9"
	if err := m.EndAssociation(moved, "ops-1", at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	cancelled := plan("SH-1", 3)
	cancelled.Status, cancelled.ConsolidationID = "CANCELLED", ""
	if err := m.Cancel("no cargo", "ops-1", at.Add(3*time.Hour)); !errors.Is(err, ErrHousesLive) {
		t.Fatalf("cancel with a house: %v", err)
	}
	if err := m.EndAssociation(cancelled, "ops-1", at.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.Cancel("no cargo", "ops-1", at.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	st := m.State()
	if len(st.Entries) != 3 || st.Entries[0].End != EndRefreshed || st.Entries[2].HAWB != "SHA0000001" || st.Entries[2].Facts.ChargeableGrams != 200_000 ||
		st.Entries[1].End != EndTransferred || st.Entries[2].End != EndCancelled || !st.MAWB.Voided {
		t.Fatalf("history: %+v", st.Entries)
	}
	var types []string
	for _, e := range m.Pending() {
		types = append(types, e.Type)
	}
	want := []string{eventOpened, eventAssigned, eventLinked, eventAssociated, eventAssociated, eventRefreshed, eventEnded, eventEnded, eventVoided, eventCancelled}
	if fmt.Sprint(types) != fmt.Sprint(want) {
		t.Fatalf("events %v", types)
	}
	if _, err := RestoreMovement(st); err != nil {
		t.Fatal(err)
	}
}

func TestWindowDrift(t *testing.T) {
	m := opened(t)
	if err := m.Associate(plan("SH-1", 2), "H1", "ops-1", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if m.WindowDrift(m.Houses()[0]) {
		t.Fatal("the flight lies in the window")
	}
	if err := m.LinkBooking(booking(2, t0.Add(100*time.Hour)), "ops-1", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !m.WindowDrift(m.Houses()[0]) {
		t.Fatal("a flight after the window drifts")
	}
}

// A series is a template and a first number, fixed once; AIR's application checks the template against the
// platform's language and the HAWB form, and the platform's sequence draws the numbers.
func TestHawbSeries(t *testing.T) {
	for _, bad := range []struct {
		template string
		first    int64
	}{{"", 1}, {" SHA{seq:7}", 1}, {"SHA{seq:7}", 0}, {strings.Repeat("S", 41), 1}} {
		if _, err := RegisterSeries("T", "LE", bad.template, bad.first, "", "ops", t0); !errors.Is(err, ErrInvalidSeries) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	s, err := RegisterSeries("T", "LE", "SHA{yy}{seq:6}", 9998, "2026", "ops", t0)
	if err != nil {
		t.Fatal(err)
	}
	if st := s.State(); st.Template != "SHA{yy}{seq:6}" || st.FirstNumber != 9998 || st.SeedPeriod != "2026" || len(s.Pending()) != 1 {
		t.Fatalf("series: %+v", st)
	}
	if _, err = RestoreSeries(s.State()); err != nil {
		t.Fatalf("restore: %v", err)
	}
}

// TestRandomHousesKeepTheirInvariants runs random associations, refreshes and ends of five houses. Whatever is
// accepted, every house has at most one live entry, a HAWB belongs to one live house, a refreshed house keeps its
// HAWB, and the movement restores.
func TestRandomHousesKeepTheirInvariants(t *testing.T) {
	for seed := uint64(1); seed <= 200; seed++ {
		r := rand.New(rand.NewPCG(seed, 3))
		m := opened(t)
		plans := map[string]int{}
		hawbs := map[string]string{}
		at := t0
		for step := range 40 {
			at = at.Add(time.Minute)
			s := fmt.Sprintf("SH-%d", r.IntN(5))
			if plans[s] == 0 {
				plans[s] = 1
			}
			p := plan(s, plans[s])
			switch r.IntN(4) {
			case 0:
				hawb := hawbs[s]
				if hawb == "" {
					hawb = fmt.Sprintf("H%d", r.IntN(7))
				}
				if m.Associate(p, hawb, "ops", at) == nil {
					hawbs[s] = hawb
				}
			case 1:
				plans[s]++
				_ = m.Refresh(plan(s, plans[s]), "ops", at)
			case 2:
				p.ConsolidationID = "C-9"
				_ = m.EndAssociation(p, "ops", at)
			case 3:
				p.Status, p.ConsolidationID = "CANCELLED", ""
				_ = m.EndAssociation(p, "ops", at)
			}
			if _, err := RestoreMovement(m.State()); err != nil {
				t.Fatalf("seed %d step %d: %v", seed, step, err)
			}
			for _, e := range m.State().Entries {
				if e.HAWB != hawbs[e.ShipmentID] {
					t.Fatalf("seed %d step %d: %s carries %s, its HAWB is %s", seed, step, e.ShipmentID, e.HAWB, hawbs[e.ShipmentID])
				}
			}
		}
	}
}

// AIR V0.2: a house takes PHY's measures as FUL relays them, received then tendered (a terminal weight), with
// the security state; the same plan and measures again are nothing to refresh.
func TestHouseTakesRelayedMeasures(t *testing.T) {
	m := opened(t)
	at := t0.Add(time.Hour)
	measured := func(version int, basis string, grams int64, security string) Plan {
		p := plan("SH-1", 1)
		p.EvidenceVersion, p.MeasuredBasis, p.MeasuredPieces, p.Security = version, basis, 6, security
		p.MeasuredWeight, p.MeasuredVolume = kernel.Quantity{Unit: "GRM", Amount: grams}, kernel.Quantity{Unit: "MTQ", Amount: 0}
		return p
	}
	bad := measured(0, BasisReceived, 310_000, SecurityPending)
	if err := m.Associate(bad, "SHA1", "ops-1", at); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("measures without FUL's evidence version: %v", err)
	}
	if err := m.Associate(measured(1, BasisReceived, 310_000, SecurityPending), "SHA1", "ops-1", at); err != nil {
		t.Fatal(err)
	}
	h := m.Houses()[0]
	if h.Facts.Basis != BasisReceived || h.Facts.Evidence != 1 || h.Facts.GrossWeight.Amount != 310_000 || h.Facts.Volume != (kernel.Quantity{}) ||
		h.Facts.ChargeableGrams != 310_000 || h.Facts.Parts[0].Packs != 6 {
		t.Fatalf("received measures, no volume, planned parts: %+v", h.Facts)
	}
	if err := m.Refresh(measured(2, BasisReceived, 310_000, SecurityPending), "ops-1", at); !errors.Is(err, ErrNothingToRefresh) {
		t.Fatalf("a newer evidence version with the same measures: %v", err)
	}
	if err := m.Refresh(measured(3, BasisTendered, 312_200, SecurityCleared), "ops-1", at); err != nil {
		t.Fatal(err)
	}
	h = m.Houses()[0]
	if h.Facts.Basis != BasisTendered || h.Facts.Security != SecurityCleared || h.Facts.ChargeableGrams != 312_500 || h.HAWB != "SHA1" || h.Start != StartRefreshed {
		t.Fatalf("tendered at the terminal weight, screened: %+v", h)
	}
	if pieces, grams, _, chargeable := m.Totals(); pieces != 6 || grams != 312_200 || chargeable != 312_500 {
		t.Fatalf("master totals on the measures: %d %d %d", pieces, grams, chargeable)
	}
	if _, err := RestoreMovement(m.State()); err != nil {
		t.Fatal(err)
	}
	unmeasured := plan("SH-9", 1)
	unmeasured.EvidenceVersion = 4
	if f, err := facts(unmeasured); err != nil || f.Basis != BasisPlanned || f.Evidence != 0 {
		t.Fatalf("planned facts carry no evidence version: %+v %v", f, err)
	}
	broken := m.State()
	broken.Entries[0].Facts.Evidence = 0
	if _, err := RestoreMovement(broken); err == nil {
		t.Fatal("measures need FUL's evidence version")
	}
}

// AIR V0.3: a movement's facts are finalized for DOC once every house is on PHY's measures, cleared by screening
// with the evidence and described; FUL's current plan of every house must match what AIR froze. A later change
// returns the movement to OPEN, and the next finalization publishes the next version.
func TestFinalizeDocFacts(t *testing.T) {
	m := opened(t)
	at := t0.Add(time.Hour)
	screened := func(lot, outcome, ref string) Screening {
		return Screening{LotID: lot, Outcome: outcome, Screener: "PVG cargo security", Method: "XRY", Reference: ref, ScreenedAt: t0.Add(30 * time.Minute)}
	}
	ready := func(shipment string) Plan {
		p := plan(shipment, 2)
		p.EvidenceVersion, p.MeasuredBasis, p.MeasuredPieces, p.Security = 3, BasisTendered, 6, SecurityCleared
		p.MeasuredWeight, p.MeasuredVolume = kernel.Quantity{Unit: "GRM", Amount: 318_000}, kernel.Quantity{Unit: "MTQ", Amount: 1}
		p.Parts[0].GoodsDescription = "Auto parts, cartons"
		p.Screenings = []Screening{screened("L-"+shipment, "PASSED", "X-"+shipment)}
		return p
	}
	if err := m.Finalize(nil, "ops-1", at); !errors.Is(err, ErrNotFinalizable) {
		t.Fatalf("no house: %v", err)
	}
	if err := m.Associate(plan("SH-1", 2), "SHA1", "ops-1", at); err != nil {
		t.Fatal(err)
	}
	if err := m.Finalize([]Plan{plan("SH-1", 2)}, "ops-1", at); !errors.Is(err, ErrNotFinalizable) {
		t.Fatalf("planned quantities: %v", err)
	}
	// in this order, each case's measures or screenings differ from the one before, so each refresh takes
	for _, tc := range []struct {
		name   string
		change func(*Plan)
	}{
		{"no description", func(p *Plan) { p.Parts[0].GoodsDescription = "" }},
		{"pending", func(p *Plan) { p.Security = SecurityPending }},
		{"no evidence", func(p *Plan) { p.Screenings = nil }},
		{"failed lot", func(p *Plan) { p.Screenings = append(p.Screenings, screened("L-2", "FAILED", "X-2")) }},
		{"no volume", func(p *Plan) { p.MeasuredVolume = kernel.Quantity{} }},
	} {
		p := ready("SH-1")
		tc.change(&p)
		if err := m.Refresh(p, "ops-1", at); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if err := m.Finalize([]Plan{p}, "ops-1", at); !errors.Is(err, ErrNotFinalizable) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	if err := m.Refresh(ready("SH-1"), "ops-1", at); err != nil {
		t.Fatal(err)
	}
	newer := ready("SH-1")
	newer.PlanVersion, newer.Revision = 3, 4
	moved := ready("SH-1")
	moved.ConsolidationID = "C-2"
	remeasured := ready("SH-1")
	remeasured.MeasuredWeight.Amount = 320_000
	for name, current := range map[string][]Plan{"not read": nil, "newer plan": {newer}, "transferred": {moved}, "remeasured": {remeasured}} {
		if err := m.Finalize(current, "ops-1", at); !errors.Is(err, ErrNotFinalizable) {
			t.Errorf("stale, %s: %v", name, err)
		}
	}
	if err := m.Associate(ready("SH-2"), "SHA2", "ops-1", at); err != nil {
		t.Fatal(err)
	}
	if err := m.Finalize([]Plan{ready("SH-1"), ready("SH-2")}, "", at); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("no author: %v", err)
	}
	if err := m.Finalize([]Plan{ready("SH-1"), ready("SH-2")}, "ops-2", at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	v, current, ok := m.LatestDocFacts()
	f := v.Facts
	if !ok || !current || m.State().State != StateFinal || v.No != 1 || v.SHA256 != Digest(v.Content) || v.By != "ops-2" {
		t.Fatalf("version 1: %+v %v %v", v, current, ok)
	}
	if f.MAWB != "781-12345675" || f.MAWBSource != MAWBSourceBooking || f.SecurityStatus != SecuritySPX || f.Pieces != 12 || f.GrossWeightGrams != 636_000 ||
		f.VolumeCubicCentimetres != 2_000_000 || f.ChargeableGrams != 636_000 || len(f.Flights) != 1 || f.Flights[0].Flight != "MU587" ||
		f.Origin.IATA != "PVG" || f.Airline.AWBPrefix != "781" || len(f.Houses) != 2 {
		t.Fatalf("master: %+v", f)
	}
	h := f.Houses[0]
	if h.HAWB != "SHA1" || h.Basis != BasisTendered || h.EvidenceVersion != 3 || h.GrossWeightGrams != 318_000 || h.VolumeCubicCentimetres != 1_000_000 ||
		h.ChargeableGrams != 318_000 || h.SecurityStatus != SecuritySPX || len(h.Screenings) != 1 || h.Screenings[0].Reference != "X-SH-1" ||
		h.Parts[0].GoodsDescription != "Auto parts, cartons" {
		t.Fatalf("house: %+v", h)
	}
	if e := m.Pending()[len(m.Pending())-1]; e.Type != eventFinalized || e.FactsVersion != 1 || e.BookingID != "BK-1" {
		t.Fatalf("event: %+v", e)
	}
	if err := m.Finalize([]Plan{ready("SH-1"), ready("SH-2")}, "ops-2", at.Add(time.Minute)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("finalized already: %v", err)
	}
	if err := m.Cancel("no cargo", "ops-2", at.Add(time.Minute)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("cancel a FINAL movement: %v", err)
	}
	// a stored version is read back from its published bytes alone, whatever the facts type renders today; bytes
	// that do not match their digest do not restore; a FINAL movement whose houses are not those of its latest
	// version restores, and the version reads as not current
	stored := m.State()
	stored.DocFacts = []DocFactsVersion{{No: 1, Content: v.Content, SHA256: v.SHA256, By: v.By, At: v.At}}
	back, err := RestoreMovement(stored)
	if err != nil {
		t.Fatal(err)
	}
	if rv, current, _ := back.LatestDocFacts(); !current || rv.Facts.Houses[1].Parts[0].GoodsDescription != "Auto parts, cartons" || rv.Facts.MAWB != f.MAWB {
		t.Fatalf("read back: %+v", rv.Facts)
	}
	extended := stored
	extended.DocFacts = []DocFactsVersion{{No: 1, Content: append([]byte(`{"future_field":1,`), v.Content[1:]...), By: v.By, At: v.At}}
	extended.DocFacts[0].SHA256 = Digest(extended.DocFacts[0].Content)
	if _, err = RestoreMovement(extended); err != nil {
		t.Fatalf("content with a field this code does not know: %v", err)
	}
	tampered := stored
	tampered.DocFacts = []DocFactsVersion{{No: 1, Content: append(append([]byte(nil), v.Content[:len(v.Content)-1]...), ' ', '}'), SHA256: v.SHA256,
		By: v.By, At: v.At}}
	if _, err = RestoreMovement(tampered); err == nil {
		t.Fatal("bytes that do not match their digest")
	}
	drifted := m.State()
	drifted.Entries = append([]Entry(nil), drifted.Entries...)
	drifted.Entries[len(drifted.Entries)-1].PlanVersion = 9
	if d, err := RestoreMovement(drifted); err != nil {
		t.Fatal(err)
	} else if _, current, _ := d.LatestDocFacts(); current {
		t.Fatal("a version that no longer describes the houses is not current")
	}
	// a new screening reference is a change: the movement is OPEN again and version 1 superseded
	rescreened := ready("SH-2")
	rescreened.Screenings[0].Reference = "X-SH-2-B"
	if err := m.Refresh(rescreened, "ops-1", at.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, current, _ := m.LatestDocFacts(); current || m.State().State != StateOpen {
		t.Fatal("a change returns the movement to OPEN")
	}
	if p := m.Pending(); p[len(p)-2].Type != eventSuperseded || p[len(p)-2].FactsVersion != 1 || p[len(p)-1].Type != eventRefreshed {
		t.Fatalf("superseded, then refreshed: %+v", p[len(p)-2:])
	}
	if err := m.Finalize([]Plan{ready("SH-1"), rescreened}, "ops-2", at.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if v1, current, ok := m.DocFactsVersion(1); !ok || current || v1.Facts.Houses[1].Screenings[0].Reference != "X-SH-2" {
		t.Fatal("version 1 stays as published, superseded")
	}
	if v2, current, ok := m.DocFactsVersion(2); !ok || !current || v2.Facts.Houses[1].Screenings[0].Reference != "X-SH-2-B" {
		t.Fatal("version 2 current")
	}
	// a schedule change from FINAL returns it to OPEN too
	if err := m.LinkBooking(booking(2, t0.Add(80*time.Hour)), "ops-1", at.Add(4*time.Minute)); err != nil || m.State().State != StateOpen {
		t.Fatalf("relink from FINAL: %v", err)
	}
	if _, err := RestoreMovement(m.State()); err != nil {
		t.Fatal(err)
	}
	if err := m.LinkBooking(booking(3, t0.Add(100*time.Hour)), "ops-1", at.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := m.Finalize([]Plan{ready("SH-1"), rescreened}, "ops-2", at.Add(6*time.Minute)); !errors.Is(err, ErrNotFinalizable) {
		t.Fatalf("a flight outside the houses' window: %v", err)
	}
}
