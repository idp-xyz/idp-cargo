package domain

import (
	"errors"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

var (
	at  = time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	sha = Evidence{SHA256: strings.Repeat("ab", 32), StorageID: "mail/draft-bl.pdf", MediaType: "application/pdf", CapturedAt: at}
)

func loc(code string) kernel.LocationCode {
	return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code}
}

func kg(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }

// booking is a confirmed container booking of a shipping line for C-1, Shanghai to Hamburg on one vessel.
func booking(version int) Booking {
	etd := at.Add(96 * time.Hour)
	return Booking{BookingID: "BK-1", Revision: int64(version + 2), Kind: KindOwn, InForce: true, ConsumerID: "C-1", LegalEntityID: "LE",
		Carrier: Carrier{PartyID: "line-1", PartyRevision: 4, Role: "CARRIER"}, Origin: loc("CNSHG"), Destination: loc("DEHAM"), Edition: "E1",
		CapacityVersion: version, Lines: 1, Legs: []Leg{{Vessel: "EVER GIVEN", VesselIMO: "9811000", Voyage: "0123W", From: loc("CNSHG"), To: loc("DEHAM"),
			ETD: etd, ETA: etd.Add(30 * 24 * time.Hour), CutOffs: []CutOff{{Kind: "VGM", At: etd.Add(-48 * time.Hour), TimeZone: "Asia/Shanghai"},
				{Kind: "SHIPPING_INSTRUCTIONS_DOCUMENTS", At: etd.Add(-72 * time.Hour), TimeZone: "Asia/Shanghai", Location: loc("CNSHG")}}}}}
}

func coload() Booking {
	b := booking(1)
	b.Kind, b.Carrier = KindCoLoad, Carrier{PartyID: "coloader-x", PartyRevision: 2, Role: "CO_LOADER"}
	return b
}

// plan is a PLANNED SEA LCL shipment of 6 cartons and 300 kg, attached to a consolidation.
func plan(shipment, consolidation string, version int) Plan {
	return Plan{ShipmentID: shipment, Revision: int64(version + 2), Status: "PLANNED", PlanVersion: version, LegalEntityID: "LE", Mode: "SEA",
		ContainerMode: "LCL", Origin: loc("CNSHG"), Destination: loc("DEHAM"), Window: kernel.ValidityInterval{From: at.Add(72 * time.Hour), Until: at.Add(120 * time.Hour)},
		Packs: 6, GrossWeight: kg(300), ConsolidationID: consolidation,
		Parts: []Part{{ObligationID: "OB-" + shipment, ObligationVersion: 1, OrderID: "O-" + shipment, Packs: 6, PackageType: "CT", GrossWeight: kg(300),
			GoodsDescription: "Furniture parts"}}}
}

// stuffed is the plan once FUL relays PHY's sealed container with the house's six packs in it.
func stuffed(p Plan, evidence int, seal string) Plan {
	p.EvidenceVersion, p.MeasuredBasis, p.MeasuredPacks, p.MeasuredWeight = evidence, BasisTendered, 6, kg(310)
	p.Stuffings = []Stuffing{{ExecutionID: "X-1", LoadUnitNo: 1, TypeCode: "22G1", Packs: 6, GrossWeight: kg(310), LineNos: []int{1}, Seal: seal,
		SealedAt: at.Add(20 * time.Hour)}}
	return p
}

func open(t *testing.T, b Booking) *Movement {
	t.Helper()
	m, err := OpenMovement("T", "OM-1", "C-1", b, "ops", at)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestNumberForms(t *testing.T) {
	for number, want := range map[string]bool{"SHAL0000001": true, "A": true, "1234567890123456": true, "12345678901234567": false, "": false,
		"shal1": false, "SHA-1": false, "SHA 1": false} {
		if ValidHBL(number) != want {
			t.Errorf("HBL %q: want %v", number, want)
		}
	}
	for number, want := range map[string]bool{"MAEU123456789": true, "SHA-HAM/2611-0042": true, strings.Repeat("A", 35): true, strings.Repeat("A", 36): false,
		"": false, "-A": false, "A-": false, "A--B": false, "A B": false, "maeu1": false} {
		if ValidBillNumber(number) != want {
			t.Errorf("master bill %q: want %v", number, want)
		}
	}
	if got := NormalizeBillNumber("  maeu123456789 "); got != "MAEU123456789" {
		t.Errorf("normalized %q", got)
	}
	// 9074729 is the IMO's own example: 9*7 + 0*6 + 7*5 + 4*4 + 7*3 + 2*2 = 139.
	for number, want := range map[string]bool{"9074729": true, "9811000": true, "9074728": false, "907472": false, "907472X": false, "": false} {
		if ValidIMO(number) != want {
			t.Errorf("IMO %q: want %v", number, want)
		}
	}
}

func TestOpenTakesTheVoyageFromTheBooking(t *testing.T) {
	m := open(t, booking(1))
	st := m.State()
	if st.Kind != KindOwn || st.Carrier.PartyID != "line-1" || st.Origin != loc("CNSHG") || st.Destination != loc("DEHAM") || st.State != StateOpen ||
		len(st.Links) != 1 || st.Links[0].Legs[0].Vessel != "EVER GIVEN" || len(st.Links[0].Legs[0].CutOffs) != 2 {
		t.Fatalf("movement: %+v", st)
	}
	if events := m.Pending(); len(events) != 2 || events[0].Type != eventOpened || events[1].Type != eventLinked || events[1].CapacityVersion != 1 {
		t.Fatalf("events: %+v", events)
	}
	if co, err := OpenMovement("T", "OM-2", "C-1", coload(), "ops", at); err != nil || co.State().Kind != KindCoLoad {
		t.Fatalf("co-load: %v", err)
	}
	for name, change := range map[string]func(*Booking){
		"an air booking":               func(b *Booking) { b.Kind = "" },
		"not in force":                 func(b *Booking) { b.InForce = false },
		"another consolidation":        func(b *Booking) { b.ConsumerID = "C-9" },
		"a co-loader on a container":   func(b *Booking) { b.Carrier.Role = "CO_LOADER" },
		"no carrier revision":          func(b *Booking) { b.Carrier.PartyRevision = 0 },
		"two capacity lines":           func(b *Booking) { b.Lines = 2 },
		"an airport code":              func(b *Booking) { b.Origin = kernel.LocationCode{Scheme: kernel.IATAAirport, Code: "PVG"} },
		"no route":                     func(b *Booking) { b.Destination = b.Origin },
		"no legs":                      func(b *Booking) { b.Legs = nil },
		"no vessel":                    func(b *Booking) { b.Legs[0].Vessel = "" },
		"a voyage in lower case":       func(b *Booking) { b.Legs[0].Voyage = "0123w" },
		"a wrong IMO check digit":      func(b *Booking) { b.Legs[0].VesselIMO = "9811001" },
		"legs from another port":       func(b *Booking) { b.Legs[0].From = loc("CNNGB") },
		"legs to another port":         func(b *Booking) { b.Legs[0].To = loc("NLRTM") },
		"no ETD":                       func(b *Booking) { b.Legs[0].ETD = time.Time{} },
		"an ETA before the ETD":        func(b *Booking) { b.Legs[0].ETA = b.Legs[0].ETD.Add(-time.Hour) },
		"an AIR cut-off":               func(b *Booking) { b.Legs[0].CutOffs[0].Kind = "CARGO_ACCEPTANCE" },
		"a cut-off twice":              func(b *Booking) { b.Legs[0].CutOffs[1].Kind = "VGM" },
		"a cut-off without a zone":     func(b *Booking) { b.Legs[0].CutOffs[0].TimeZone = "" },
		"a cut-off without an instant": func(b *Booking) { b.Legs[0].CutOffs[0].At = time.Time{} },
	} {
		b := booking(1)
		change(&b)
		if _, err := OpenMovement("T", "OM-1", "C-1", b, "ops", at); !errors.Is(err, ErrInvalidBooking) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := OpenMovement("T", "OM-1", "C-1", booking(1), " ", at); !errors.Is(err, ErrInvalidMovement) {
		t.Errorf("no author: %v", err)
	}
	// a transhipment: two legs that follow each other
	via := booking(1)
	second := via.Legs[0]
	via.Legs[0].To, second.From, second.Vessel, second.VesselIMO, second.CutOffs = loc("SGSIN"), loc("SGSIN"), "MSC OSCAR", "", nil
	second.ETD, second.ETA = via.Legs[0].ETD.Add(12*24*time.Hour), via.Legs[0].ETD.Add(34*24*time.Hour)
	via.Legs[0].ETA = via.Legs[0].ETD.Add(10 * 24 * time.Hour)
	via.Legs = append(via.Legs, second)
	if _, err := OpenMovement("T", "OM-1", "C-1", via, "ops", at); err != nil {
		t.Errorf("two legs: %v", err)
	}
	via.Legs[1].From = loc("MYPKG")
	if _, err := OpenMovement("T", "OM-1", "C-1", via, "ops", at); !errors.Is(err, ErrInvalidBooking) {
		t.Errorf("legs that do not follow: %v", err)
	}
}

func TestLinkTakesANewerCapacityVersion(t *testing.T) {
	m := open(t, booking(1))
	rolled := booking(2)
	rolled.Legs[0].Vessel, rolled.Legs[0].VesselIMO, rolled.Legs[0].Voyage = "MSC OSCAR", "", "0124W"
	if err := m.LinkBooking(booking(1), "ops", at); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("the same capacity version: %v", err)
	}
	if err := m.Associate(plan("SH-1", "C-1", 1), "H1", "ops", at); err != nil {
		t.Fatal(err)
	}
	if err := m.LinkBooking(rolled, "ops", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if cur := m.Current(); cur.CapacityVersion != 2 || cur.Legs[0].Vessel != "MSC OSCAR" || len(m.State().Links) != 2 || len(m.Houses()) != 1 {
		t.Fatalf("rolled: %+v", cur)
	}
	for name, change := range map[string]func(*Booking){
		"another booking":      func(b *Booking) { b.BookingID = "BK-2" },
		"another carrier":      func(b *Booking) { b.Carrier.PartyID = "line-2" },
		"another legal entity": func(b *Booking) { b.LegalEntityID = "LE-2" },
		"another route": func(b *Booking) {
			b.Destination, b.Legs[0].To = loc("NLRTM"), loc("NLRTM")
		},
		"a co-load": func(b *Booking) { b.Kind, b.Carrier.Role = KindCoLoad, "CO_LOADER" },
	} {
		b := booking(3)
		change(&b)
		if err := m.LinkBooking(b, "ops", at.Add(2*time.Hour)); !errors.Is(err, ErrInvalidBooking) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := m.LinkBooking(booking(3), "ops", at); !errors.Is(err, ErrInvalidChange) {
		t.Errorf("a business time before the last change: %v", err)
	}
	// the house's window (72 to 120 hours) contains the first ETD (96 hours) until the vessel is retimed past it
	late := booking(3)
	late.Legs[0].ETD, late.Legs[0].ETA = at.Add(200*time.Hour), at.Add(900*time.Hour)
	late.Legs[0].CutOffs = nil
	if m.WindowDrift(m.Houses()[0]) {
		t.Fatal("no drift before the retiming")
	}
	if err := m.LinkBooking(late, "ops", at.Add(2*time.Hour)); err != nil || !m.WindowDrift(m.Houses()[0]) {
		t.Fatalf("drift after the retiming: %v", err)
	}
}

func TestMasterBillReferences(t *testing.T) {
	m := open(t, booking(1))
	if _, ok := m.CurrentBill(); ok {
		t.Fatal("no master bill at opening")
	}
	late := sha
	late.CapturedAt = at.Add(2 * time.Hour)
	for name, tc := range map[string]struct {
		number, kind string
		evidence     Evidence
	}{
		"a co-load bill on a container":   {"MAEU123456789", BillCoLoad, sha},
		"an unknown kind":                 {"MAEU123456789", "HBL", sha},
		"a number in lower case":          {"maeu123456789", BillCarrierMBL, sha},
		"no number":                       {"", BillCarrierMBL, sha},
		"no evidence":                     {"MAEU123456789", BillCarrierMBL, Evidence{}},
		"evidence captured after":         {"MAEU123456789", BillCarrierMBL, late},
		"evidence without its media type": {"MAEU123456789", BillCarrierMBL, Evidence{SHA256: sha.SHA256, StorageID: "x", CapturedAt: at}},
	} {
		if err := m.RecordMasterBill(tc.number, tc.kind, tc.evidence, "ops", at.Add(time.Hour)); !errors.Is(err, ErrInvalidMasterBill) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := m.RecordMasterBill("MAEU123456789", BillCarrierMBL, sha, "ops", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.RecordMasterBill("MAEU123456789", BillCarrierMBL, sha, "ops", at.Add(2*time.Hour)); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("the current number again: %v", err)
	}
	// the line reissues it as a sea waybill, then under another number
	if err := errors.Join(m.RecordMasterBill("MAEU123456789", BillCarrierSeaWaybill, sha, "ops", at.Add(2*time.Hour)),
		m.RecordMasterBill("MAEU987654321", BillCarrierSeaWaybill, sha, "ops", at.Add(3*time.Hour))); err != nil {
		t.Fatal(err)
	}
	bills := m.State().Bills
	if len(bills) != 3 || bills[0].Status != BillSuperseded || bills[1].Status != BillSuperseded || bills[2].Status != BillCurrent {
		t.Fatalf("bills: %+v", bills)
	}
	if cur, ok := m.CurrentBill(); !ok || cur.Number != "MAEU987654321" || cur.No != 3 {
		t.Fatalf("current: %+v", cur)
	}
	if e := m.Pending(); e[len(e)-1].Type != eventBill || e[len(e)-1].BillNo != 3 {
		t.Fatalf("event: %+v", e[len(e)-1])
	}
	// a restored movement keeps track of the stored bills that leave CURRENT
	restored, err := RestoreMovement(m.State())
	if err != nil {
		t.Fatal(err)
	}
	if err = restored.Cancel("vessel omitted the port", "ops", at.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	retired, _ := restored.Changed()
	if len(retired) != 1 || retired[3].Status != BillVoided {
		t.Fatalf("voided with the movement: %+v", retired)
	}
	if _, ok := restored.CurrentBill(); ok {
		t.Fatal("a cancelled movement has no current master bill")
	}
	if err = restored.RecordMasterBill("MAEU111", BillCarrierMBL, sha, "ops", at.Add(5*time.Hour)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a master bill on a cancelled movement: %v", err)
	}
	if _, err = RestoreMovement(restored.State()); err != nil {
		t.Fatalf("the cancelled movement restores: %v", err)
	}
	// a co-load takes the co-loader's bill only
	co := open(t, coload())
	if err = co.RecordMasterBill("MAEU123456789", BillCarrierMBL, sha, "ops", at); !errors.Is(err, ErrInvalidMasterBill) {
		t.Fatalf("a carrier's bill on a co-load: %v", err)
	}
	if err = co.RecordMasterBill("SHA-HAM/2611-0042", BillCoLoad, sha, "ops", at); err != nil {
		t.Fatal(err)
	}
}

func TestHouseBills(t *testing.T) {
	m := open(t, booking(1))
	for name, change := range map[string]func(*Plan){
		"a draft":                 func(p *Plan) { p.Status = "DRAFT" },
		"planned by AIR":          func(p *Plan) { p.Mode = "AIR" },
		"planned as FCL":          func(p *Plan) { p.ContainerMode = "FCL" },
		"in another consol":       func(p *Plan) { p.ConsolidationID = "C-9" },
		"attached to none":        func(p *Plan) { p.ConsolidationID = "" },
		"of another legal entity": func(p *Plan) { p.LegalEntityID = "LE-2" },
		"to another port":         func(p *Plan) { p.Destination = loc("NLRTM") },
	} {
		p := plan("SH-1", "C-1", 1)
		change(&p)
		if err := m.Associate(p, "H1", "ops", at); !errors.Is(err, ErrNotAttached) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, change := range map[string]func(*Plan){
		"no packs":                   func(p *Plan) { p.Packs = 0 },
		"no parts":                   func(p *Plan) { p.Parts = nil },
		"no weight":                  func(p *Plan) { p.GrossWeight = kernel.Quantity{} },
		"a part without its package": func(p *Plan) { p.Parts[0].PackageType = "" },
		"measures without evidence":  func(p *Plan) { p.MeasuredBasis, p.MeasuredPacks, p.MeasuredWeight = BasisReceived, 6, kg(300) },
		"a container without evidence": func(p *Plan) {
			p.Stuffings = stuffed(*p, 1, "S-1").Stuffings
		},
		"a container without a seal": func(p *Plan) {
			*p = stuffed(*p, 1, "")
		},
		"a container of a malformed size-type": func(p *Plan) {
			*p = stuffed(*p, 1, "S-1")
			p.Stuffings[0].TypeCode = "22g1"
		},
		"a container without load lines": func(p *Plan) {
			*p = stuffed(*p, 1, "S-1")
			p.Stuffings[0].LineNos = nil
		},
		"a stuffing twice": func(p *Plan) {
			*p = stuffed(*p, 1, "S-1")
			p.Stuffings = append(p.Stuffings, p.Stuffings[0])
		},
	} {
		p := plan("SH-1", "C-1", 1)
		change(&p)
		if err := m.Associate(p, "H1", "ops", at); !errors.Is(err, ErrInvalidChange) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := m.Associate(plan("SH-1", "C-1", 1), "h-1", "ops", at); !errors.Is(err, ErrInvalidHBL) {
		t.Fatalf("a malformed HBL: %v", err)
	}
	unplanned := plan("SH-2", "C-1", 1)
	unplanned.ContainerMode = "" // a plan confirmed before the container mode was planned is taken, as PHY takes it
	if err := errors.Join(m.Associate(plan("SH-1", "C-1", 1), "H1", "ops", at), m.Associate(unplanned, "H2", "ops", at)); err != nil {
		t.Fatal(err)
	}
	if err := m.Associate(plan("SH-1", "C-1", 1), "H9", "ops", at); !errors.Is(err, ErrAlreadyAssociated) {
		t.Fatalf("a house twice: %v", err)
	}
	if err := m.Associate(plan("SH-3", "C-1", 1), "H1", "ops", at); !errors.Is(err, ErrAlreadyAssociated) {
		t.Fatalf("an HBL twice: %v", err)
	}
	if e := m.Houses()[0]; e.HBL != "H1" || e.Facts.Basis != BasisPlanned || e.Facts.Evidence != 0 || e.Facts.GrossWeight != kg(300) ||
		e.Facts.Parts[0].GoodsDescription != "Furniture parts" || e.ContainerMode != "LCL" || m.Houses()[1].ContainerMode != "" {
		t.Fatalf("house: %+v", e)
	}

	// Refresh: nothing new is refused; the sealed container makes a new entry with the same HBL; so does a reseal.
	if err := m.Refresh(plan("SH-1", "C-1", 1), "ops", at.Add(time.Hour)); !errors.Is(err, ErrNothingToRefresh) {
		t.Fatalf("nothing to refresh: %v", err)
	}
	if err := m.Refresh(plan("SH-9", "C-1", 1), "ops", at.Add(time.Hour)); !errors.Is(err, ErrNotAssociated) {
		t.Fatalf("refresh of a stranger: %v", err)
	}
	if err := m.Refresh(stuffed(plan("SH-1", "C-1", 1), 2, "CN-SEAL-1"), "ops", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.Refresh(stuffed(plan("SH-1", "C-1", 1), 3, "CN-SEAL-1"), "ops", at.Add(2*time.Hour)); !errors.Is(err, ErrNothingToRefresh) {
		t.Fatalf("a newer evidence version with the same measures and containers: %v", err)
	}
	if err := m.Refresh(stuffed(plan("SH-1", "C-1", 1), 3, "CN-SEAL-2"), "ops", at.Add(2*time.Hour)); err != nil {
		t.Fatalf("another seal: %v", err)
	}
	if err := m.Refresh(stuffed(plan("SH-1", "C-1", 2), 3, "CN-SEAL-2"), "ops", at.Add(3*time.Hour)); err != nil {
		t.Fatalf("another plan version: %v", err)
	}
	if err := m.Refresh(stuffed(plan("SH-1", "C-1", 1), 4, "CN-SEAL-3"), "ops", at.Add(3*time.Hour)); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("an older plan version than the entry's: %v", err)
	}
	st := m.State()
	if len(st.Entries) != 5 || st.Entries[0].End != EndRefreshed || st.Entries[0].Successor != 3 || st.Entries[2].Predecessor != 1 ||
		st.Entries[4].HBL != "H1" || st.Entries[4].Start != StartRefreshed || st.Entries[4].Facts.Basis != BasisTendered ||
		st.Entries[4].Facts.Evidence != 3 || st.Entries[4].Facts.Stuffings[0].Seal != "CN-SEAL-2" || st.Entries[4].Facts.GrossWeight != kg(310) {
		t.Fatalf("entries: %+v", st.Entries)
	}

	// End: still attached is refused; transferred, detached and cancelled each end the entry with their kind.
	if err := m.EndAssociation(plan("SH-1", "C-1", 2), "ops", at.Add(4*time.Hour)); !errors.Is(err, ErrStillAttached) {
		t.Fatalf("still attached: %v", err)
	}
	if err := m.Cancel("no cargo", "ops", at.Add(4*time.Hour)); !errors.Is(err, ErrHousesLive) {
		t.Fatalf("cancel with houses: %v", err)
	}
	if err := m.EndAssociation(plan("SH-1", "C-2", 2), "ops", at.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	cancelled := plan("SH-2", "C-1", 1)
	cancelled.Status = "CANCELLED"
	if err := m.EndAssociation(cancelled, "ops", at.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.Associate(plan("SH-3", "C-1", 1), "H3", "ops", at.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.EndAssociation(plan("SH-3", "", 1), "ops", at.Add(5*time.Hour)); err != nil {
		t.Fatal(err)
	}
	st = m.State()
	if st.Entries[4].End != EndTransferred || st.Entries[1].End != EndCancelled || st.Entries[5].End != EndDetached || len(m.Houses()) != 0 {
		t.Fatalf("ended: %+v", st.Entries)
	}
	if err := m.Cancel("  ", "ops", at.Add(6*time.Hour)); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("cancel without a reason: %v", err)
	}
	if err := m.Cancel("consolidation cancelled", "ops", at.Add(6*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.Associate(plan("SH-4", "C-1", 1), "H4", "ops", at.Add(7*time.Hour)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a house on a cancelled movement: %v", err)
	}
	if _, err := RestoreMovement(m.State()); err != nil {
		t.Fatalf("restore: %v", err)
	}
}

func TestTotalsOnlyInUnitsSEAConverts(t *testing.T) {
	m := open(t, booking(1))
	a, b := plan("SH-1", "C-1", 1), plan("SH-2", "C-1", 1)
	a.Volume, b.GrossWeight = kernel.Quantity{Unit: "MTQ", Amount: 2}, kernel.Quantity{Unit: "TNE", Amount: 1}
	b.Parts[0].GrossWeight = b.GrossWeight
	if err := errors.Join(m.Associate(a, "H1", "ops", at), m.Associate(b, "H2", "ops", at)); err != nil {
		t.Fatal(err)
	}
	if packs, grams, cm3, weighed, measured := m.Totals(); packs != 12 || grams != 1_300_000 || cm3 != 2_000_000 || !weighed || !measured {
		t.Fatalf("totals: %d %d %d %v %v", packs, grams, cm3, weighed, measured)
	}
	// a house weighed in pounds and measured in cubic feet is kept as relayed; the movement then states no totals
	c := plan("SH-3", "C-1", 1)
	c.EvidenceVersion, c.MeasuredBasis, c.MeasuredPacks = 1, BasisReceived, 6
	c.MeasuredWeight, c.MeasuredVolume = kernel.Quantity{Unit: "LBR", Amount: 660}, kernel.Quantity{Unit: "FTQ", Amount: 70}
	if err := m.Associate(c, "H3", "ops", at); err != nil {
		t.Fatal(err)
	}
	if packs, grams, cm3, weighed, measured := m.Totals(); packs != 18 || grams != 0 || cm3 != 0 || weighed || measured {
		t.Fatalf("totals with pounds: %d %d %d %v %v", packs, grams, cm3, weighed, measured)
	}
	if e := m.Houses()[2]; e.Facts.GrossWeight.Unit != "LBR" || e.Facts.Basis != BasisReceived || e.Facts.Evidence != 1 {
		t.Fatalf("house in pounds: %+v", e.Facts)
	}
	if _, _, _, weighed, measured := open(t, booking(1)).Totals(); weighed || measured {
		t.Fatal("a movement without houses states no totals")
	}
}

func TestRestoreRefusesInvalidMovements(t *testing.T) {
	m := open(t, booking(1))
	if err := errors.Join(m.RecordMasterBill("MAEU123456789", BillCarrierMBL, sha, "ops", at),
		m.RecordMasterBill("MAEU987654321", BillCarrierMBL, sha, "ops", at),
		m.Associate(stuffed(plan("SH-1", "C-1", 1), 1, "CN-SEAL-1"), "H1", "ops", at),
		m.Refresh(stuffed(plan("SH-1", "C-1", 2), 1, "CN-SEAL-1"), "ops", at)); err != nil {
		t.Fatal(err)
	}
	m.Stamp(at)
	if _, err := RestoreMovement(m.State()); err != nil {
		t.Fatalf("a valid movement: %v", err)
	}
	// another live entry beside entry 2 (SH-1 under H1), of a shipment and with an HBL
	beside := func(shipment, hbl string) func(*MovementState) {
		return func(st *MovementState) {
			e := st.Entries[1]
			e.No, e.ShipmentID, e.HBL, e.Start, e.Predecessor = 3, shipment, hbl, StartAssociated, 0
			st.Entries = append(st.Entries, e)
		}
	}
	for name, tc := range map[string]struct {
		change func(*MovementState)
		why    string // what the restore reports
	}{
		"a co-loader on a container":    {func(st *MovementState) { st.Carrier.Role = "CO_LOADER" }, "identity"},
		"an unknown kind":               {func(st *MovementState) { st.Kind = "SEA_FCL" }, "identity"},
		"an unknown state":              {func(st *MovementState) { st.State = "SAILED" }, "state SAILED"},
		"FINAL without its facts":       {func(st *MovementState) { st.State = StateFinal }, "FINAL without doc facts"},
		"cancelled without a reason":    {func(st *MovementState) { st.State = StateCancelled }, "cancellation"},
		"no link":                       {func(st *MovementState) { st.Links = nil }, "no booking link"},
		"a link out of number":          {func(st *MovementState) { st.Links[0].No = 2 }, "link 1"},
		"two current master bills":      {func(st *MovementState) { st.Bills[0].Status = BillCurrent }, "master bill 1"},
		"no current master bill":        {func(st *MovementState) { st.Bills[1].Status = BillSuperseded }, "master bill 2"},
		"a co-load bill on a container": {func(st *MovementState) { st.Bills[1].Kind = BillCoLoad }, "master bill 2"},
		"a master bill out of number":   {func(st *MovementState) { st.Bills[1].No = 3 }, "master bill 2"},
		"an entry without packs":        {func(st *MovementState) { st.Entries[1].Facts.Packs = 0 }, "entry 2"},
		"a measured entry without evidence": {func(st *MovementState) {
			st.Entries[1].Facts.Evidence, st.Entries[1].Facts.Stuffings = 0, nil
		}, "entry 2"},
		"an FCL entry":                    {func(st *MovementState) { st.Entries[1].ContainerMode = "FCL" }, "entry 2"},
		"one shipment live twice":         {beside("SH-1", "H3"), "entry 3 live"},
		"one HBL live twice":              {beside("SH-3", "H1"), "entry 3 live"},
		"a live entry beside its refresh": {func(st *MovementState) { st.Entries[0].End, st.Entries[0].Successor = "", 0 }, "entry 2 predecessor"},
		"a refresh of nothing":            {func(st *MovementState) { st.Entries[1].Predecessor = 0 }, "entry 1 successor"},
		"another HBL on refresh":          {func(st *MovementState) { st.Entries[1].HBL = "H2" }, "entry 2 predecessor"},
		"an unknown end":                  {func(st *MovementState) { st.Entries[0].End = "LOST" }, "entry 1 end"},
		"a container without its seal": {func(st *MovementState) {
			st.Entries[1].Facts.Stuffings = []Stuffing{{ExecutionID: "X-1", LoadUnitNo: 1, TypeCode: "22G1", Packs: 6, LineNos: []int{1}, SealedAt: at}}
		}, "entry 2 container"},
		"a container without evidence": {func(st *MovementState) {
			st.Entries[1].Facts.Basis, st.Entries[1].Facts.Evidence = BasisPlanned, 0
		}, "entry 2 container"},
	} {
		st := m.State()
		st.Bills, st.Entries = append([]MasterBill(nil), st.Bills...), append([]Entry(nil), st.Entries...)
		tc.change(&st)
		if _, err := RestoreMovement(st); !errors.Is(err, ErrInvalidMovement) || !strings.HasSuffix(err.Error(), ": "+tc.why) {
			t.Errorf("%s: %v, want %q", name, err, tc.why)
		}
	}
	// a shipment may be named like an HBL: the two are told apart
	named := open(t, booking(1))
	if err := errors.Join(named.Associate(plan("hbl:H2", "C-1", 1), "H1", "ops", at), named.Associate(plan("H1", "C-1", 1), "H2", "ops", at)); err != nil {
		t.Fatal(err)
	}
	named.Stamp(at)
	if _, err := RestoreMovement(named.State()); err != nil {
		t.Fatalf("shipments hbl:H2 and H1 under the HBLs H1 and H2: %v", err)
	}
}

func TestSeries(t *testing.T) {
	s, err := RegisterSeries("T", "LE", "SHAL{seq:7}", 1, "", "ops", at)
	if err != nil || len(s.Pending()) != 1 || s.Pending()[0].Type != "HblSeriesRegistered.v1" || s.Pending()[0].SubjectID != "LE" {
		t.Fatalf("series: %v", err)
	}
	for name, tc := range map[string]struct {
		le, template string
		first        int64
	}{"no legal entity": {"", "SHAL{seq:7}", 1}, "no template": {"LE", "", 1}, "a padded template": {"LE", " SHAL{seq:7}", 1}, "first number zero": {"LE", "SHAL{seq:7}", 0}} {
		if _, err := RegisterSeries("T", tc.le, tc.template, tc.first, "", "ops", at); !errors.Is(err, ErrInvalidSeries) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err = RestoreSeries(s.State()); err != nil {
		t.Fatal(err)
	}
}

func TestFinalizeOceanDocFacts(t *testing.T) {
	p := stuffed(plan("SH-1", "C-1", 1), 1, "CN-SEAL-1")
	m := open(t, booking(1))
	if err := errors.Join(m.Associate(p, "H1", "ops", at), m.RecordMasterBill("MAEU123456789", BillCarrierMBL, sha, "ops", at.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	v := assigned(t, m)
	sealed(t, v, "CN-SEAL-1", 2)
	if err := v.StuffingDone(m, "", "cfs", at.Add(21*time.Hour)); err != nil {
		t.Fatal(err)
	}
	when := at.Add(23 * time.Hour)
	if err := m.Finalize([]Plan{p}, []VisitState{v.State()}, "ops", when); !errors.Is(err, ErrNotFinalizable) {
		t.Fatalf("before the VGM: %v", err)
	}
	if err := v.AssertVGM(m, kernel.Quantity{Unit: "KGM", Amount: plate.TareKg}, VgmMethod1, "LE", 1, at.Add(22*time.Hour), "", "Ada Lovelace", "ops", at.Add(22*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.Finalize([]Plan{p}, []VisitState{v.State()}, "ops", when); err != nil {
		t.Fatal(err)
	}
	got, current, ok := m.DocFactsVersion(0)
	if !ok || !current || m.State().State != StateFinal || strings.Contains(string(got.Content), "Ada") || strings.Contains(string(got.Content), "Lovelace") ||
		len(got.Facts.Containers) != 1 || got.Facts.Containers[0].VGMGrams != plate.TareKg*1000 || got.Facts.Containers[0].VGMAssertionID != "CV-1:1" ||
		got.Facts.Houses[0].Packs != 6 || m.Pending()[len(m.Pending())-1].FactsVersion != 1 {
		t.Fatalf("%s", got.Content)
	}
	if again, err := RestoreMovement(m.State()); err != nil || again.State().State != StateFinal {
		t.Fatalf("restored: %v", err)
	}
	if err := m.RecordMasterBill("MAEU000000001", BillCarrierMBL, sha, "ops", when); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a master bill once FINAL: %v", err)
	}
	next := p
	next.PlanVersion, next.Revision = 2, p.Revision+1
	if err := m.Refresh(next, "ops", when.Add(time.Hour)); err != nil || m.State().State != StateOpen {
		t.Fatalf("reopened: %v %s", err, m.State().State)
	}
	if _, current, ok = m.DocFactsVersion(1); !ok || current {
		t.Fatal("version 1 stayed current")
	}
	events := m.Pending()
	if events[len(events)-2].Type != eventSuperseded || events[len(events)-2].FactsVersion != 1 || events[len(events)-1].Type != eventRefreshed {
		t.Fatalf("events: %+v", events[len(events)-2:])
	}
}
