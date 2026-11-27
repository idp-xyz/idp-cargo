//go:build integration

package bootstrap

import (
	"slices"
	"strings"
	"testing"
	"time"

	bookapi "go.idp.xyz/idp-cargo/internal/contexts/book/api"
	docapi "go.idp.xyz/idp-cargo/internal/contexts/doc/api"
	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// The owner's acceptance of V2.105 (验收标准_V2.105_整箱改到另一航次.md), with every context on PostgreSQL: the two boxes
// of the owner's scheme in C-1, both sealed with their VGMs, on EVER GIVEN in OM-1. The carrier shuts box B out and gives
// it a new booking on EVER GLORY a week later. Box B moves to a movement of its own with H04 and H05, its seal and its
// VGM; box A sails as planned. Each movement has its own master bill and is finalized on its own; H04's and H05's house
// bills move to the new movement and are issued there. Box B's storage fee of 300.00 is a line of container 2: H04
// 200.00, H05 100.00.
func TestBoxBIsRolledToALaterVesselAndSailsInAMovementOfItsOwnWithEveryContext(t *testing.T) {
	d := newSeaLineDay(t, "cargo-roll-")

	// 1. As stage 2c: both boxes stuffed and sealed, their VGMs, the master bill, OM-1 finalized with five houses; the five
	// house bills drafted and approved on OM-1. FIN divides each box's freight over its own container (V2.104).
	d.must("FUL takes the evidence", d.fulTakes(16, "SH-H01", "SH-H02", "SH-H03", "SH-H04", "SH-H05"))
	for _, s := range []string{"SH-H01", "SH-H02", "SH-H03", "SH-H04", "SH-H05"} {
		d.must("refresh "+s, d.refresh(s, 17))
	}
	d.visit["CV-1"], d.visit["CV-2"] = d.cvOf("CV-1").Revision, d.cvOf("CV-2").Revision
	d.must("SEA's seal of box A", d.boxSeal("CV-1", "seal-x-1", 17))
	d.must("SEA's seal of box B", d.boxSeal("CV-2", "seal-x-2", 17))
	for _, v := range []string{"CV-1", "CV-2"} {
		d.must("stuffing done "+v, d.boxStuffingDone(v, 18))
	}
	d.must("VGM of box A", d.boxVGM("CV-1", 2900, 19))
	d.must("VGM of box B", d.boxVGM("CV-2", 3000, 19))
	d.masterBill(18)
	d.finalizeOf("OM-1", "fin-1", 20)
	bills := map[string]string{}
	for _, h := range seaLineHouses {
		bills[h] = d.prepare("SH-"+h, "OM-1", 21)
	}
	for i, unit := range []int{1, 2} {
		_, err := d.fin.ReviseConsolCost(d.ctx, finapi.ReviseConsolCost{CommandID: "fin-box-" + string(rune('a'+i)), TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-1",
			ExpectedRevision: d.pool().Revision, LineNo: i + 1, ExpectedVersion: 1, Value: finapi.ChargeSide{Amount: kernel.Money{Currency: "CNY", Minor: []int64{300_000, 240_000}[i]}},
			Reason: "the freight of one box", MoveToLoadUnit: true, LoadUnitNo: unit, OccurredAt: d.hours(16)})
		d.must("box freight", err)
	}
	d.must("the three lines", d.apportionLines("fin-app-all", nil, nil))
	d.wantJobs("before the roll", map[string]int64{"H01": 106_000, "H02": 156_000, "H03": 56_000, "H04": 166_000, "H05": 86_000}, 570_000, 350_000)

	// 2. The carrier rolls box B: a new booking BK-3 for C-1 on EVER GLORY, a week later. OM-2 is opened on it and box B
	// rolled there, with a reason.
	slice{tenant: d.tenant, at: d.at, departs: d.at.Add(72 * time.Hour)}.bookSea(d.t, d.book, "BK-3", "C-1", "EVER GLORY", "", "0130W", d.at.Add(240*time.Hour))
	_, err := d.sea.OpenOceanMovement(d.ctx, seaapi.OpenOceanMovement{CommandID: "open-om2", TenantID: d.tenant, ActorID: "ops-1", MovementID: "OM-2", ConsolidationID: "C-1",
		BookingID: "BK-3", OccurredAt: d.hours(23)})
	d.must("open OM-2 for C-1", err)
	_, err = d.sea.RollContainerToMovement(d.ctx, seaapi.RollContainerToMovement{CommandID: "roll-box-b", TenantID: d.tenant, ActorID: "ops-1", VisitID: "CV-2",
		ExpectedRevision: d.cvOf("CV-2").Revision, ToMovementID: "OM-2", Reason: "shut out at the terminal; the carrier rolled it to EVER GLORY 0130W", OccurredAt: d.hours(24)})
	d.must("roll box B", err)
	om1, err := d.sea.GetOceanMovement(d.ctx, seaapi.GetOceanMovement{TenantID: d.tenant, MovementID: "OM-1"})
	d.must("OM-1", err)
	om2, err := d.sea.GetOceanMovement(d.ctx, seaapi.GetOceanMovement{TenantID: d.tenant, MovementID: "OM-2"})
	d.must("OM-2", err)
	if len(om1.Houses) != 3 || len(om1.Containers) != 1 || om1.Containers[0].VisitID != "CV-1" || om1.State != seaapi.StateOpen || om1.Legs[0].Vessel != "EVER GIVEN" {
		t.Fatalf("step 2, OM-1: %+v", om1)
	}
	if len(om2.Houses) != 2 || om2.Houses[0].ShipmentID != "SH-H04" || om2.Houses[1].ShipmentID != "SH-H05" || om2.Houses[0].Start != "ROLLED_WITH_CONTAINER" ||
		len(om2.Containers) != 1 || om2.Containers[0].VisitID != "CV-2" || om2.Legs[0].Vessel != "EVER GLORY" {
		t.Fatalf("step 2, OM-2: %+v", om2)
	}
	cv2 := d.cvOf("CV-2")
	if cv2.MovementID != "OM-2" || len(cv2.Seals) != 1 || len(cv2.Assertions) != 1 || len(cv2.Rolls) != 1 || cv2.Rolls[0].FromMovementID != "OM-1" || cv2.Rolls[0].ToMovementID != "OM-2" ||
		!strings.Contains(cv2.Rolls[0].Reason, "shut out") || cv2.Rolls[0].By != "ops-1" || !cv2.Rolls[0].At.Equal(d.hours(24)) {
		t.Fatalf("step 2, box B: %+v", cv2)
	}

	// 3. Each movement sails on its own: box A loaded on EVER GIVEN and OM-1 finalized again with three houses; OM-2 gets
	// its own master bill, box B is loaded on EVER GLORY and OM-2 finalized with two houses.
	d.visit["CV-1"], d.visit["CV-2"] = d.cvOf("CV-1").Revision, d.cvOf("CV-2").Revision
	d.load("CV-1", 26)
	d.finalizeOf("OM-1", "fin-2", 26)
	m2 := func() int64 {
		m, err := d.sea.GetOceanMovement(d.ctx, seaapi.GetOceanMovement{TenantID: d.tenant, MovementID: "OM-2"})
		d.must("OM-2", err)
		return m.Revision
	}
	_, err = d.sea.RecordMasterBill(d.ctx, seaapi.RecordMasterBill{CommandID: "mbl-om2", TenantID: d.tenant, ActorID: "ops-1", MovementID: "OM-2", ExpectedRevision: m2(),
		Number: "linu2612000777", Kind: seaapi.MasterBillCarrierMBL, Evidence: seaapi.Evidence{SHA256: strings.Repeat("9e", 32), StorageID: "mail/mbl-om2.pdf",
			MediaType: "application/pdf", CapturedAt: d.hours(30)}, OccurredAt: d.hours(30)})
	d.must("OM-2's master bill", err)
	_, err = d.dm().RefreshDocumentDraft(d.ctx, docapi.RefreshDocumentDraft{CommandID: "refresh-h04-early", TenantID: d.tenant, ActorID: "doc-1", DocumentID: bills["H04"],
		ExpectedRevision: 2, Reason: "SEA's latest facts", OccurredAt: d.hours(31)})
	if err == nil || !strings.Contains(err.Error(), "sails in OM-2 now") || !strings.Contains(err.Error(), "DOC_SOURCE_NOT_FINAL") {
		t.Fatalf("step 3, H04's draft before OM-2 is finalized: %v", err)
	}
	t.Logf("step 3, H04's draft before OM-2 is finalized: %v", err)
	d.load("CV-2", 40)
	d.finalizeOf("OM-2", "fin-om2", 40)
	f1, f2 := d.facts("OM-1"), d.facts("OM-2")
	if f1.Version != 2 || f1.Status != seaapi.DocFactsFinal || len(f1.Houses) != 3 || len(f1.Containers) != 1 || f1.Containers[0].SealNumber != "SEAL-X-1" {
		t.Fatalf("step 3, OM-1's facts: %+v", shown(f1))
	}
	if f2.Version != 1 || f2.Status != seaapi.DocFactsFinal || len(f2.Houses) != 2 || len(f2.Containers) != 1 || f2.Containers[0].SealNumber != "SEAL-X-2" ||
		!strings.EqualFold(f2.MasterBillNumber, "linu2612000777") || f2.Vessel != "EVER GLORY" || !f2.Containers[0].OnBoardAt.Equal(d.hours(40)) {
		t.Fatalf("step 3, OM-2's facts: %+v", shown(f2))
	}

	// 4. DOC: H01 to H03's bills are refreshed on OM-1's version 2 and issued; H04's and H05's drafts move to OM-2 and are
	// issued there, on EVER GLORY and its master bill.
	for _, h := range []string{"H01", "H02", "H03"} {
		d.refreshBill(bills[h], 2, "", 41)
		d.approve(bills[h], 3, 41)
		d.must("issue "+h, d.issue(bills[h], 4, 42))
		if doc, v := d.bill(bills[h]); doc.State != docapi.StateIssued || doc.MovementID != "OM-1" || v.Doc.Source.Version != 2 {
			t.Fatalf("step 4, %s: %+v %+v", h, doc, v.Doc)
		}
	}
	// H04's draft follows its container by an ordinary refresh, with no movement named (the owner's decision: a bill not
	// issued moves to the new voyage by itself); H05's names OM-2, as a transfer does. EVER GLORY sails after H04's and
	// H05's departure window, so DOC issues neither until the customer's answer is recorded (V2.110, the owner's decision
	// of 2026-11-26): FUL follows the roll to BK-3, the customer accepts, SEA takes the agreed window and finalizes OM-2
	// again, and the drafts are refreshed on that version and issued.
	for _, h := range []string{"H04", "H05"} {
		d.refreshBill(bills[h], 2, map[string]string{"H04": "", "H05": "OM-2"}[h], 41)
		d.approve(bills[h], 3, 41)
		if err := d.issue(bills[h], 4, 42); err == nil || !strings.Contains(err.Error(), "DOC_DATE_CHANGE_UNANSWERED") {
			t.Fatalf("step 4, %s's bill before the customer's answer: %v", h, err)
		}
	}
	d.acceptLaterVessel("OM-2", "BK-3", 0, 43, "SH-H04", "SH-H05")
	for _, h := range []string{"H04", "H05"} {
		d.refreshBill(bills[h], 4, "", 45)
		d.approve(bills[h], 5, 45)
		d.must("issue "+h, d.issue(bills[h], 6, 46))
		if doc, v := d.bill(bills[h]); doc.State != docapi.StateIssued || doc.MovementID != "OM-2" || doc.ConsolidationID != "C-1" || v.Doc.Source.Version != 2 ||
			!v.Doc.OnBoardAt.Equal(d.hours(40)) {
			t.Fatalf("step 4, %s: %+v %+v", h, doc, v.Doc)
		}
	}
	d.wantPending("OM-1 after the issue", "OM-1", "")
	d.wantPending("OM-2 after the issue", "OM-2", "")

	// 5. FIN: box B's storage fee of 300.00, a line of container 2: H04 200.00, H05 100.00. The freight stays as it was.
	_, err = d.fin.RecordConsolCost(d.ctx, finapi.RecordConsolCost{CommandID: "fin-cost-storage", TenantID: d.tenant, ActorID: "fin-1", PoolID: "P-1", ExpectedRevision: d.pool().Revision,
		ChargeCode: "STORAGE", Description: "storage of box B", Method: finapi.MethodChargeable, Cost: finapi.ChargeSide{PartyID: "line-1", Amount: kernel.Money{Currency: "CNY", Minor: 30_000}},
		Reason: "storage of box B at the terminal until EVER GLORY", OccurredAt: d.later(), LoadUnitNo: 2})
	d.must("the storage fee", err)
	d.must("the storage fee is apportioned", d.apportionLines("fin-app-storage", []int{4}, nil))
	d.wantShares("step 5", 4, map[string]int64{"H04": 20_000, "H05": 10_000})
	d.wantShares("step 5", 1, map[string]int64{"H01": 100_000, "H02": 150_000, "H03": 50_000})
	d.wantShares("step 5", 2, map[string]int64{"H04": 160_000, "H05": 80_000})
	d.wantJobs("after the roll", map[string]int64{"H01": 106_000, "H02": 156_000, "H03": 56_000, "H04": 186_000, "H05": 96_000}, 600_000, 320_000)
}

// The same booking (V2.105, the owner's second case): the carrier keeps BK-1 and gives it a new capacity version of two
// lines, line 1 on EVER GIVEN for box A and line 2 on EVER GLORY a week later for box B. OM-1 takes version 2 on its line
// 1; OM-2 is opened on line 2 and box B rolled there. H04's house bill is drafted on OM-2 and issued on EVER GLORY.
func TestBoxBIsRolledUnderTheSameBookingToItsSecondLineWithEveryContext(t *testing.T) {
	d := newSeaLineDay(t, "cargo-roll-line-")
	d.must("FUL takes the evidence", d.fulTakes(16, "SH-H01", "SH-H02", "SH-H03", "SH-H04", "SH-H05"))
	for _, s := range []string{"SH-H01", "SH-H02", "SH-H03", "SH-H04", "SH-H05"} {
		d.must("refresh "+s, d.refresh(s, 17))
	}
	d.visit["CV-1"], d.visit["CV-2"] = d.cvOf("CV-1").Revision, d.cvOf("CV-2").Revision
	d.must("SEA's seal of box A", d.boxSeal("CV-1", "seal-x-1", 17))
	d.must("SEA's seal of box B", d.boxSeal("CV-2", "seal-x-2", 17))
	for _, v := range []string{"CV-1", "CV-2"} {
		d.must("stuffing done "+v, d.boxStuffingDone(v, 18))
	}
	d.must("VGM of box A", d.boxVGM("CV-1", 2900, 19))
	d.must("VGM of box B", d.boxVGM("CV-2", 3000, 19))
	d.masterBill(18)
	d.finalizeOf("OM-1", "fin-1", 20)

	// BOOK: version 2 of BK-1 splits it over two voyages
	b, err := d.book.GetCarrierBooking(d.ctx, bookapi.GetCarrierBooking{TenantID: d.tenant, BookingID: "BK-1"})
	d.must("BK-1", err)
	first := b.Capacity.Lines[0].Legs[0]
	unlocode := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	box := bookapi.Capacity{Equipment: []bookapi.Equipment{{SizeType: "22G1", Count: 1}}}
	leg := func(vessel, voyage string, etd time.Time) []bookapi.ConfirmedLeg {
		return []bookapi.ConfirmedLeg{{Loading: unlocode("CNSHA"), Discharge: unlocode("DEHAM"), Vessel: vessel, Voyage: voyage, ETD: etd, ETA: etd.Add(30 * 24 * time.Hour),
			CutOffs: first.CutOffs}}
	}
	later := first.ETD.Add(7 * 24 * time.Hour)
	// C-1 fills two boxes: the request is changed to two, which the carrier answers by splitting them over two sailings
	two := bookapi.Capacity{Equipment: []bookapi.Equipment{{SizeType: "22G1", Count: 2}}}
	_, err = d.book.RequestBookingChange(d.ctx, bookapi.RequestBookingChange{CommandID: "ask-two", TenantID: d.tenant, ActorID: "ops-1", BookingID: "BK-1", ExpectedRevision: b.Revision,
		Request: bookapi.Request{Legs: []bookapi.RequestedLeg{{Loading: unlocode("CNSHA"), Discharge: unlocode("DEHAM"), DepartureDate: first.ETD.Format(time.DateOnly)}}, Capacity: two},
		Reason:  "C-1 fills two containers", Channel: bookapi.ChannelCarrierPortal, Evidence: bookapi.Evidence{SHA256: strings.Repeat("6d", 32), StorageID: "mail/ask.eml",
			MediaType: "message/rfc822", CapturedAt: d.hours(21)}, OccurredAt: d.hours(21)})
	d.must("BOOK asks for two boxes", err)
	_, err = d.book.RecordChangeConfirmed(d.ctx, bookapi.RecordChangeConfirmed{CommandID: "split", TenantID: d.tenant, ActorID: "ops-1", BookingID: "BK-1", ExpectedRevision: b.Revision + 1,
		Answer: bookapi.Answer{Channel: bookapi.ChannelEmail, Evidence: bookapi.Evidence{SHA256: strings.Repeat("6c", 32), StorageID: "mail/split.eml",
			MediaType: "message/rfc822", CapturedAt: d.hours(22)}},
		Lines:      []bookapi.CapacityLine{{Capacity: box, Legs: leg(first.Vessel, first.Voyage, first.ETD)}, {Capacity: box, Legs: leg("EVER GLORY", "0130W", later)}},
		OccurredAt: d.hours(22)})
	d.must("BOOK splits BK-1", err)

	// SEA: OM-1 takes version 2 (its line 1), OM-2 opens on line 2, box B rolls there
	_, err = d.sea.LinkCarrierBooking(d.ctx, seaapi.LinkCarrierBooking{CommandID: "link-v2", TenantID: d.tenant, ActorID: "ops-1", MovementID: "OM-1",
		ExpectedRevision: d.movement().Revision, OccurredAt: d.hours(23)})
	d.must("OM-1 takes version 2", err)
	_, err = d.sea.OpenOceanMovement(d.ctx, seaapi.OpenOceanMovement{CommandID: "open-om2", TenantID: d.tenant, ActorID: "ops-1", MovementID: "OM-2", ConsolidationID: "C-1",
		BookingID: "BK-1", BookingLine: 2, OccurredAt: d.hours(23)})
	d.must("open OM-2 on line 2", err)
	_, err = d.sea.RollContainerToMovement(d.ctx, seaapi.RollContainerToMovement{CommandID: "roll-box-b", TenantID: d.tenant, ActorID: "ops-1", VisitID: "CV-2",
		ExpectedRevision: d.cvOf("CV-2").Revision, ToMovementID: "OM-2", Reason: "the carrier moved it to line 2 of the booking, EVER GLORY 0130W", OccurredAt: d.hours(24)})
	d.must("roll box B", err)
	om1, err := d.sea.GetOceanMovement(d.ctx, seaapi.GetOceanMovement{TenantID: d.tenant, MovementID: "OM-1"})
	d.must("OM-1", err)
	om2, err := d.sea.GetOceanMovement(d.ctx, seaapi.GetOceanMovement{TenantID: d.tenant, MovementID: "OM-2"})
	d.must("OM-2", err)
	if om1.BookingLine != 1 || om1.CapacityVersion != 2 || om1.Legs[0].Vessel != first.Vessel || len(om1.Houses) != 3 || len(om1.Containers) != 1 {
		t.Fatalf("OM-1: %+v", om1)
	}
	if om2.BookingID != "BK-1" || om2.BookingLine != 2 || om2.CapacityVersion != 2 || om2.Legs[0].Vessel != "EVER GLORY" || len(om2.Houses) != 2 || len(om2.Containers) != 1 ||
		om2.Containers[0].VisitID != "CV-2" {
		t.Fatalf("OM-2: %+v", om2)
	}

	// each sails and is finalized on its own; H04's bill is drafted on OM-2 and issued on EVER GLORY
	d.visit["CV-1"], d.visit["CV-2"] = d.cvOf("CV-1").Revision, d.cvOf("CV-2").Revision
	d.load("CV-1", 26)
	d.finalizeOf("OM-1", "fin-2", 26)
	_, err = d.sea.RecordMasterBill(d.ctx, seaapi.RecordMasterBill{CommandID: "mbl-om2", TenantID: d.tenant, ActorID: "ops-1", MovementID: "OM-2", ExpectedRevision: om2.Revision,
		Number: "linu2612000778", Kind: seaapi.MasterBillCarrierMBL, Evidence: seaapi.Evidence{SHA256: strings.Repeat("9f", 32), StorageID: "mail/mbl-om2.pdf",
			MediaType: "application/pdf", CapturedAt: d.hours(30)}, OccurredAt: d.hours(30)})
	d.must("OM-2's master bill", err)
	d.load("CV-2", 40)
	d.finalizeOf("OM-2", "fin-om2", 40)
	if f1, f2 := d.facts("OM-1"), d.facts("OM-2"); len(f1.Houses) != 3 || f1.Vessel != first.Vessel || len(f2.Houses) != 2 || f2.Vessel != "EVER GLORY" || f2.BookingID != "BK-1" ||
		!strings.EqualFold(f2.MasterBillNumber, "linu2612000778") {
		t.Fatalf("the facts: %+v / %+v", shown(f1), shown(f2))
	}
	// EVER GLORY sails after H04's window: its bill waits for the customer's answer (V2.110), recorded against line 2
	id := d.prepare("SH-H04", "OM-2", 41)
	if err := d.issue(id, 2, 42); err == nil || !strings.Contains(err.Error(), "DOC_DATE_CHANGE_UNANSWERED") {
		t.Fatalf("H04's bill before the customer's answer: %v", err)
	}
	d.acceptLaterVessel("OM-2", "BK-1", 2, 43, "SH-H04")
	if hs := d.facts("OM-2").Houses; len(hs) != 2 || hs[0].ShipmentID != "SH-H04" || hs[0].WindowDrift || !hs[1].WindowDrift {
		t.Fatalf("OM-2's houses after H04's customer answered and H05's did not: %+v", hs)
	}
	d.refreshBill(id, 2, "", 45)
	d.approve(id, 3, 45)
	d.must("issue H04's bill", d.issue(id, 4, 46))
	if doc, v := d.bill(id); doc.State != docapi.StateIssued || doc.MovementID != "OM-2" || !v.Doc.OnBoardAt.Equal(d.hours(40)) || v.Doc.Source.Version != 2 {
		t.Fatalf("H04's bill: %+v %+v", doc, v.Doc)
	}
}

// acceptLaterVessel follows a roll for the houses (V2.110): FUL links each house's main carriage to the booking and line
// it sails on now, which departs after its window; FUL's work list of C-1 shows it until the customer accepts the later
// vessel; SEA then takes the window the customer agreed to, refreshing each house in the movement, and finalizes the
// movement again, with none of the houses drifting.
func (d *loadingDay) acceptLaterVessel(movement, booking string, line, hour int, shipments ...string) {
	d.t.Helper()
	execution := func(s string) fulapi.ShipmentExecution {
		e, err := d.ful.GetShipmentExecution(d.ctx, fulapi.GetShipmentExecution{TenantID: d.tenant, ShipmentID: s})
		d.must("FUL's execution of "+s, err)
		return e
	}
	for _, s := range shipments {
		_, err := d.ful.LinkCarrierCapacity(d.ctx, fulapi.LinkCarrierCapacity{CommandID: "link-" + s + "-" + booking, TenantID: d.tenant, ActorID: "ops-1",
			ShipmentID: s, ExpectedRevision: execution(s).Revision, BookingID: booking, Line: line, OccurredAt: d.hours(hour)})
		d.must("FUL follows "+s+" to "+booking, err)
	}
	awaiting, err := d.ful.ListDateChangesAwaitingCustomer(d.ctx, fulapi.ListDateChangesAwaitingCustomer{TenantID: d.tenant, ConsolidationID: "C-1"})
	d.must("FUL's work list", err)
	for _, s := range shipments {
		if !slices.ContainsFunc(awaiting, func(a fulapi.DateChangeAwaitingCustomer) bool { return a.ShipmentID == s && a.BookingID == booking }) {
			d.t.Fatalf("%s is not on FUL's work list: %+v", s, awaiting)
		}
		e := execution(s)
		_, err = d.ful.RecordCustomerDateAnswer(d.ctx, fulapi.RecordCustomerDateAnswer{CommandID: "answer-" + s + "-" + booking, TenantID: d.tenant, ActorID: "ops-1",
			ShipmentID: s, ExpectedRevision: e.Revision, LinkNo: e.CurrentLink, Answer: fulapi.AnswerAccepted,
			EvidenceRef: "the customer's mail accepting the later vessel", OccurredAt: d.hours(hour)})
		d.must("the customer's answer for "+s, err)
		if e = execution(s); e.AwaitingCustomer {
			d.t.Fatalf("%s still awaits the customer: %+v", s, e)
		}
	}
	for _, s := range shipments {
		m, err := d.sea.GetOceanMovement(d.ctx, seaapi.GetOceanMovement{TenantID: d.tenant, MovementID: movement})
		d.must(movement, err)
		_, err = d.sea.RefreshHouseBillFacts(d.ctx, seaapi.RefreshHouseBillFacts{CommandID: "agreed-" + s, TenantID: d.tenant, ActorID: "ops-1", MovementID: movement,
			ExpectedRevision: m.Revision, ShipmentID: s, OccurredAt: d.hours(hour + 1)})
		d.must("SEA takes the agreed window of "+s, err)
	}
	d.finalizeOf(movement, "fin-"+movement+"-agreed", hour+1)
	for _, h := range d.facts(movement).Houses {
		if h.WindowDrift && slices.Contains(shipments, h.ShipmentID) {
			d.t.Fatalf("%s drifts in %s's facts after the customer's answer", h.ShipmentID, movement)
		}
	}
}
