package domain

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

var t0 = time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)

func loc(code, iata string) Location {
	return Location{Code: kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code}, IATA: iata}
}

var (
	pvg, jfk = loc("CNPVG", "PVG"), loc("USJFK", "JFK")
	shg, ham = loc("CNSHG", ""), loc("DEHAM", "")
	airline  = Carrier{PartyID: "MU-PARTY", PartyRevision: 3, Role: RoleCarrier, ValidFrom: t0.Add(-time.Hour), RoleEvidence: "contract",
		IATADesignator: "MU", AWBPrefix: "781"}
	evidence = Evidence{SHA256: strings.Repeat("ab", 32), StorageID: "obj-1", MediaType: "application/pdf", CapturedAt: t0}
	kg       = func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }
)

func airRequest() Request {
	return Request{Legs: []RequestedLeg{{Loading: pvg, Discharge: jfk, Flight: "MU587", DepartureDate: "2026-11-13"}},
		Capacity: Capacity{Pieces: 20, GrossWeight: kg(1000)}, HandlingCodes: []string{"GEN"}, LocationEdition: "E1"}
}

func airLine(pieces, weight int64, etd time.Time) Line {
	return Line{No: 1, Capacity: Capacity{Pieces: pieces, GrossWeight: kg(weight)}, Legs: []Leg{{Loading: pvg, Discharge: jfk, Flight: "MU587",
		ETD: etd, ETA: etd.Add(14 * time.Hour), CutOffs: []CutOff{{Kind: "CARGO_ACCEPTANCE", At: etd.Add(-6 * time.Hour), Zone: "Asia/Shanghai"}}}}}
}

func answer(refs ...Reference) Answer {
	return Answer{Channel: ChannelEmail, Evidence: evidence, CarrierTime: t0.Add(time.Hour), References: refs}
}

const mawb = "781-12345675"

func drafted(t *testing.T) *Booking {
	t.Helper()
	b, err := DraftBooking("T", "B-1", KindAirConsol, "LE", airline, "C-1", "", airRequest(), "ops-1", t0)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMAWBAndIMOCheckDigits(t *testing.T) {
	for in, want := range map[string]string{"781-12345675": "781-12345675", "78112345675": "781-12345675", "781 1234 5675": "781-12345675"} {
		if got, ok := NormalizeMAWB(in); !ok || got != want {
			t.Errorf("%s: %s %v", in, got, ok)
		}
	}
	for _, bad := range []string{"781-12345674", "78-12345675", "781-1234567A", "7811234567"} {
		if _, ok := NormalizeMAWB(bad); ok {
			t.Errorf("%s accepted", bad)
		}
	}
	if !ValidIMO("9074729") || ValidIMO("9074728") || ValidIMO("907472") {
		t.Fatal("IMO check digit")
	}
}

func TestAirBookingLifecycle(t *testing.T) {
	b := drafted(t)
	etd := t0.Add(72 * time.Hour)
	changed := airRequest()
	changed.Capacity.Pieces, changed.Capacity.GrossWeight = 24, kg(1200)
	at := func(h int) time.Time { return t0.Add(time.Duration(h) * time.Hour) }
	if err := errors.Join(
		b.AmendRequest(changed, "more cartons", "ops-1", at(1)),
		b.RecordSent(ChannelCarrierPortal, evidence, "ops-1", at(2)),
		b.RecordAcknowledgement(answer(Reference{Type: ReferenceRequestAck, Value: "ack-77"}), "ops-1", at(3)),
		b.RecordConfirmation(answer(Reference{Type: ReferenceMAWBNumber, Value: "78112345675"}), Grant{Lines: []Line{airLine(20, 1000, etd)}, Edition: "E1"}, "ops-1", at(4)),
	); err != nil {
		t.Fatal(err)
	}
	c, _ := b.Capacity()
	if b.Status() != StatusConfirmed || c.Reason != CapacityPartialConfirmation || b.MAWB() != mawb {
		t.Fatalf("confirmed less than requested: %s %s %s", b.Status(), c.Reason, b.MAWB())
	}
	if a, ok := b.ActiveAllocation(); !ok || a.ConsumerID != "C-1" || a.CapacityVersion != 1 {
		t.Fatalf("allocation %+v", a)
	}
	if err := b.RecordCarrierChange(CapacityScheduleChange, answer(Reference{Type: ReferenceMAWBNumber, Value: "781-23456786"}),
		Grant{Lines: []Line{airLine(20, 1000, etd.Add(24*time.Hour))}, Edition: "E1"}, "ops-1", at(5)); !errors.Is(err, ErrMAWB) {
		t.Fatalf("a different MAWB on the same booking: %v", err)
	}
	if err := errors.Join(
		b.RecordCarrierChange(CapacityScheduleChange, answer(), Grant{Lines: []Line{airLine(20, 1000, etd.Add(24*time.Hour))}, Edition: "E1"}, "ops-1", at(5)),
		b.RequestChange(changed, "the airline offered the rest", ChannelEmail, evidence, "ops-1", at(6)),
		b.RecordChangeConfirmed(answer(), Grant{Lines: []Line{airLine(24, 1200, etd.Add(24*time.Hour))}, Edition: "E1"}, "ops-1", at(7)),
		b.Reassign("C-2", "the consolidation was replaced", "ops-1", at(8)),
		b.RequestCancellation("customer withdrew", ChannelPhone, evidence, "ops-1", at(9)),
		b.RecordCancellationConfirmed(answer(), "ops-1", at(10)),
	); err != nil {
		t.Fatal(err)
	}
	st := b.State()
	if b.Status() != StatusCancelled || len(st.Capacities) != 3 || st.Capacities[1].Reason != CapacityScheduleChange || st.Capacities[2].Reason != CapacityRequestedChange {
		t.Fatalf("capacity history: %s %+v", b.Status(), st.Capacities)
	}
	if _, ok := b.ActiveAllocation(); ok || st.ConsumerID != "C-2" {
		t.Fatal("a cancelled booking keeps no allocation")
	}
	var types []string
	for _, e := range b.Pending() {
		types = append(types, e.Type)
	}
	want := []string{eventDrafted, eventRequestAmended, eventRequested, eventAcknowledged, eventConfirmed, eventAllocated, eventCapacityChanged,
		eventChangeRequested, eventCapacityChanged, eventReleased, eventAllocated, eventCancellationRequested, eventCancelled, eventReleased}
	if !slices.Equal(types, want) {
		t.Fatalf("events %v", types)
	}
	b.Stamp(at(11))
	if _, err := RestoreBooking(b.State()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	st = b.State()
	st.Status = StatusConfirmed
	if _, err := RestoreBooking(st); err == nil {
		t.Fatal("a status the steps do not make must not restore")
	}
}

func TestBookingRules(t *testing.T) {
	etd := t0.Add(72 * time.Hour)
	grant := Grant{Lines: []Line{airLine(20, 1000, etd)}, Edition: "E1"}
	sent := func() *Booking {
		b := drafted(t)
		if err := b.RecordSent(ChannelEmail, evidence, "ops-1", t0.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		return b
	}
	noCodes := airline
	noCodes.AWBPrefix = ""
	if _, err := DraftBooking("T", "B-1", KindAirConsol, "LE", noCodes, "C-1", "", airRequest(), "ops-1", t0); !errors.Is(err, ErrCarrierCodes) {
		t.Fatalf("an airline without its AWB prefix: %v", err)
	}
	ca := airRequest()
	ca.Legs[0].Flight = "CA981"
	if _, err := DraftBooking("T", "B-1", KindAirConsol, "LE", airline, "C-1", "", ca, "ops-1", t0); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a flight of another airline: %v", err)
	}
	coloader := Carrier{PartyID: "CL", PartyRevision: 1, Role: RoleCarrier, ValidFrom: t0, RoleEvidence: "x"}
	coload := Request{Legs: []RequestedLeg{{Loading: shg, Discharge: ham, DepartureDate: "2026-11-20"}}, LocationEdition: "E1",
		Capacity: Capacity{GrossWeight: kg(800), Volume: kernel.Quantity{Unit: "DMQ", Amount: 3000}}}
	if _, err := DraftBooking("T", "B-2", KindSeaLCLCoLoad, "LE", coloader, "C-1", "", coload, "ops-1", t0); !errors.Is(err, ErrInvalidBooking) {
		t.Fatalf("a co-load needs a CO_LOADER: %v", err)
	}
	coloader.Role = RoleCoLoader
	if _, err := DraftBooking("T", "B-2", KindSeaLCLCoLoad, "LE", coloader, "C-1", "", coload, "ops-1", t0); err != nil {
		t.Fatalf("co-load: %v", err)
	}
	b := drafted(t)
	if err := b.RecordConfirmation(answer(Reference{Type: ReferenceMAWBNumber, Value: mawb}), grant, "ops-1", t0.Add(time.Hour)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a request is never a confirmation: %v", err)
	}
	if err := b.RecordSent(ChannelEmail, Evidence{}, "ops-1", t0.Add(time.Hour)); !errors.Is(err, ErrInvalidEvidence) {
		t.Fatalf("sent without evidence: %v", err)
	}
	for name, tc := range map[string]struct {
		answer Answer
		grant  Grant
		want   error
	}{
		"no MAWB":             {answer(), grant, ErrMAWB},
		"bad check digit":     {answer(Reference{Type: ReferenceMAWBNumber, Value: "781-12345674"}), grant, ErrInvalidReference},
		"other prefix":        {answer(Reference{Type: ReferenceMAWBNumber, Value: "784-12345675"}), grant, ErrInvalidReference},
		"no evidence":         {Answer{Channel: ChannelEmail, References: []Reference{{Type: ReferenceMAWBNumber, Value: mawb}}}, grant, ErrInvalidEvidence},
		"more than requested": {answer(Reference{Type: ReferenceMAWBNumber, Value: mawb}), Grant{Lines: []Line{airLine(21, 1000, etd)}, Edition: "E1"}, ErrExceedsRequest},
		"cut-off after ETD": {answer(Reference{Type: ReferenceMAWBNumber, Value: mawb}), func() Grant {
			l := airLine(20, 1000, etd)
			l.Legs[0].CutOffs[0].At = etd.Add(time.Minute)
			return Grant{Lines: []Line{l}, Edition: "E1"}
		}(), ErrInvalidCapacity},
		"unknown zone": {answer(Reference{Type: ReferenceMAWBNumber, Value: mawb}), func() Grant {
			l := airLine(20, 1000, etd)
			l.Legs[0].CutOffs[0].Zone = "Asia/Pudong"
			return Grant{Lines: []Line{l}, Edition: "E1"}
		}(), ErrInvalidCapacity},
		"SEA cut-off on AIR": {answer(Reference{Type: ReferenceMAWBNumber, Value: mawb}), func() Grant {
			l := airLine(20, 1000, etd)
			l.Legs[0].CutOffs[0].Kind = "VGM"
			return Grant{Lines: []Line{l}, Edition: "E1"}
		}(), ErrInvalidCapacity},
		"other destination": {answer(Reference{Type: ReferenceMAWBNumber, Value: mawb}), func() Grant {
			l := airLine(20, 1000, etd)
			l.Legs[0].Discharge = loc("USLAX", "LAX")
			return Grant{Lines: []Line{l}, Edition: "E1"}
		}(), ErrInvalidCapacity},
	} {
		if err := sent().RecordConfirmation(tc.answer, tc.grant, "ops-1", t0.Add(2*time.Hour)); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	b = sent()
	if err := b.RecordRejection(answer(), "ops-1", t0.Add(2*time.Hour)); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("a rejection without the carrier's reason: %v", err)
	}
	a := answer()
	a.ReasonText = "no space"
	if err := b.RecordRejection(a, "ops-1", t0.Add(2*time.Hour)); err != nil || b.Status() != StatusRejected {
		t.Fatalf("rejection: %v", err)
	}
	b = sent()
	if err := b.RecordConfirmation(answer(Reference{Type: ReferenceMAWBNumber, Value: mawb}), grant, "ops-1", t0.Add(30*time.Minute)); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("an answer before the request was sent: %v", err)
	}
	if err := b.RecordConfirmation(answer(Reference{Type: ReferenceMAWBNumber, Value: mawb}), grant, "ops-1", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if c, _ := b.Capacity(); c.Reason != CapacityInitial {
		t.Fatalf("a full confirmation: %s", c.Reason)
	}
	if err := b.Reassign("C-1", "same", "ops-1", t0.Add(3*time.Hour)); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("reassign to the same consolidation: %v", err)
	}
	a.ReasonText = "aircraft change"
	if err := b.RecordDecline(a, "ops-1", t0.Add(3*time.Hour)); err != nil || b.Status() != StatusDeclinedByCarrier {
		t.Fatalf("decline: %v", err)
	}
	if _, ok := b.ActiveAllocation(); ok {
		t.Fatal("a declined booking keeps no allocation")
	}
	w := drafted(t)
	if err := w.WithdrawDraft("not needed", "ops-1", t0.Add(time.Hour)); err != nil || w.Status() != StatusCancelled {
		t.Fatalf("withdraw: %v", err)
	}
	if _, err := RestoreBooking(w.State()); err != nil {
		t.Fatalf("restore a withdrawn draft: %v", err)
	}
}

func TestContainerBookingEquipment(t *testing.T) {
	line := Carrier{PartyID: "COSCO", PartyRevision: 1, Role: RoleCarrier, ValidFrom: t0, RoleEvidence: "contract"}
	request := Request{Legs: []RequestedLeg{{Loading: shg, Discharge: ham, Vessel: "COSCO SHIPPING ARIES", Voyage: "045W", DepartureDate: "2026-11-20"}},
		Capacity: Capacity{Equipment: []Equipment{{SizeType: "45G1", Count: 2}, {SizeType: "22G1", Count: 1}}}, LocationEdition: "E1"}
	b, err := DraftBooking("T", "B-S", KindSeaLCLConsolEquipment, "LE", line, "C-S", "", request, "ops-1", t0)
	if err != nil {
		t.Fatal(err)
	}
	etd := t0.Add(10 * 24 * time.Hour)
	leg := Leg{Loading: shg, Discharge: ham, Vessel: "COSCO SHIPPING ARIES", VesselIMO: "9074729", Voyage: "045W", ETD: etd,
		CutOffs: []CutOff{{Kind: "VGM", At: etd.Add(-48 * time.Hour), Zone: "Asia/Shanghai"}, {Kind: "CY_FULL_CONTAINER", At: etd.Add(-36 * time.Hour), Zone: "Asia/Shanghai"}}}
	if err = errors.Join(
		b.RecordSent(ChannelCarrierPortal, evidence, "ops-1", t0.Add(time.Hour)),
		b.RecordConfirmation(answer(Reference{Type: ReferenceBookingNumber, Value: "cosu6390123450"}),
			Grant{Lines: []Line{{No: 1, Legs: []Leg{leg}, Capacity: Capacity{Equipment: []Equipment{{SizeType: "45G1", Count: 2}}}}}, Edition: "E1"}, "ops-1", t0.Add(2*time.Hour)),
	); err != nil {
		t.Fatal(err)
	}
	c, _ := b.Capacity()
	if c.Reason != CapacityPartialConfirmation || b.References()[0].Value != "COSU6390123450" {
		t.Fatalf("partial container confirmation: %s %+v", c.Reason, b.References())
	}
	if err = b.RecordCarrierChange(CapacityRollover, answer(), Grant{Lines: []Line{{No: 1, Legs: []Leg{leg},
		Capacity: Capacity{Equipment: []Equipment{{SizeType: "45G1", Count: 3}}}}}, Edition: "E1"}, "ops-1", t0.Add(3*time.Hour)); !errors.Is(err, ErrExceedsRequest) {
		t.Fatalf("a rollover grants no more than requested: %v", err)
	}
}

// TestRandomBookingsKeepTheirInvariants runs random sequences of booking commands. Whatever is accepted, the
// booking restores (its status follows its steps), at most one allocation is active and only on the current
// capacity of a booking in force, and an air consolidation's booking never holds two MAWB numbers.
func TestRandomBookingsKeepTheirInvariants(t *testing.T) {
	mawbs := []string{"781-12345675", "781-23456786"}
	for seed := uint64(1); seed <= 300; seed++ {
		r := rand.New(rand.NewPCG(seed, 11))
		b := drafted(t)
		at := t0
		etd := t0.Add(96 * time.Hour)
		for step := range 25 {
			at = at.Add(time.Minute)
			refs := []Reference{}
			if r.IntN(2) == 0 {
				refs = append(refs, Reference{Type: ReferenceMAWBNumber, Value: mawbs[r.IntN(2)]})
			}
			a := answer(refs...)
			a.ReasonText = "carrier said so"
			g := Grant{Lines: []Line{airLine(int64(1+r.IntN(20)), int64(100+r.IntN(900)), etd)}, Edition: "E1"}
			switch r.IntN(13) {
			case 0:
				_ = b.RecordSent(ChannelEmail, evidence, "ops", at)
			case 1:
				_ = b.RecordAcknowledgement(a, "ops", at)
			case 2, 3:
				_ = b.RecordConfirmation(a, g, "ops", at)
			case 4:
				_ = b.RecordCarrierChange(CapacityRollover, a, g, "ops", at)
			case 5:
				_ = b.RequestChange(airRequest(), "more", ChannelEmail, evidence, "ops", at)
			case 6:
				_ = b.RecordChangeConfirmed(a, g, "ops", at)
			case 7:
				_ = b.RecordChangeRefused(a, "ops", at)
			case 8:
				_ = b.RecordDecline(a, "ops", at)
			case 9:
				_ = b.RequestCancellation("not needed", ChannelEmail, evidence, "ops", at)
			case 10:
				_ = b.RecordCancellationConfirmed(a, "ops", at)
			case 11:
				_ = b.Reassign(fmt.Sprintf("C-%d", step), "replaced", "ops", at)
			case 12:
				_ = b.RecordRejection(a, "ops", at)
			}
			if _, err := RestoreBooking(b.State()); err != nil {
				t.Fatalf("seed %d step %d: %v", seed, step, err)
			}
			active := 0
			latest := map[int]Allocation{}
			for _, al := range b.State().Allocations {
				latest[al.No] = al
			}
			for _, al := range latest {
				if al.Status == AllocationActive {
					active++
				}
			}
			if active > 1 || b.mawbs() > 1 || (b.Status().Terminal() && active > 0) {
				t.Fatalf("seed %d step %d: %d active allocations, %d MAWBs, status %s", seed, step, active, b.mawbs(), b.Status())
			}
		}
	}
}
