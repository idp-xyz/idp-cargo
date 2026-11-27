package domain

import (
	"errors"
	"testing"
	"time"
)

// V2.111: FUL keeps the shipment's pieces on each flight with its load evidence; they add up to its departed, arrived
// and offloaded pieces, and the pieces offloaded and not departed since await a later flight.
func TestFlightsAddUpAndOffloadedPiecesAwaitALaterFlight(t *testing.T) {
	s, route := planned(t) // 4 packs
	at := s.Latest().At.Add(time.Hour)
	if err := s.RecordConsolAttachment(Membership{ConsolidationID: "C-1", EntryNo: 1, Revision: 1, AttachedAt: at, Mode: ModeSea,
		Origin: route.Origin, Destination: route.Destination}, "ops", at); err != nil {
		t.Fatal(err)
	}
	base := LoadEvidence{Lots: 1, ReceiptComplete: true, Received: CargoScope{Packs: 4, GrossWeight: kg(400)}, ConsolidationID: "C-1",
		Tendered: CargoScope{Packs: 4, GrossWeight: kg(400)}}
	for name, bad := range map[string][]Flight{
		"flights adding up to more":   {{Transport: "MU587/01DEC", Departed: 3}},
		"a flight twice":              {{Transport: "MU587/01DEC", Departed: 1}, {Transport: "MU587/01DEC", Departed: 1}},
		"a flight with nothing on it": {{Transport: "MU587/01DEC", Departed: 2}, {Transport: "MU587/02DEC"}},
		"a flight without a name":     {{Transport: " ", Departed: 2}},
	} {
		e := base
		e.Departed, e.Offloaded, e.Flights = 2, 0, bad
		if _, err := s.RecordEvidence(e, "ops", at); !errors.Is(err, ErrInvalidEvidence) {
			t.Errorf("%s: %v", name, err)
		}
	}
	first := base
	first.Departed, first.Offloaded = 2, 2
	first.Flights = []Flight{{Transport: "MU587/01DEC", Departed: 2, Offloaded: 2}}
	if _, err := s.RecordEvidence(first, "ops", at); err != nil {
		t.Fatal(err)
	}
	if n := s.AwaitingLaterFlight(); n != 2 || s.Execution() != ExecutionPartiallyDeparted {
		t.Fatalf("after the first flight: %d awaiting, %s", n, s.Execution())
	}
	second := first
	second.Departed = 4
	second.Flights = []Flight{{Transport: "MU587/01DEC", Departed: 2, Offloaded: 2}, {Transport: "MU587/02DEC", Departed: 2}}
	if added, err := s.RecordEvidence(second, "ops", at.Add(time.Hour)); err != nil || !added {
		t.Fatalf("the second flight: %v %v", added, err)
	}
	if n := s.AwaitingLaterFlight(); n != 0 || s.Execution() != ExecutionDeparted {
		t.Fatalf("after the second flight: %d awaiting, %s", n, s.Execution())
	}
	v, _ := s.LatestEvidence()
	if len(v.Evidence.Flights) != 2 || v.Evidence.Flights[1].Transport != "MU587/02DEC" {
		t.Fatalf("the flights kept: %+v", v.Evidence.Flights)
	}
	if _, err := RestoreShipment(s.State()); err != nil {
		t.Fatal(err)
	}
	// after the review: what was tendered and what arrived count too. Tendered short: 2 of the 4 planned, 1 offloaded and
	// flown the next day: nothing awaits although the plan holds 4.
	short, route2 := planned(t)
	if err := short.RecordConsolAttachment(Membership{ConsolidationID: "C-1", EntryNo: 1, Revision: 1, AttachedAt: at, Mode: ModeSea,
		Origin: route2.Origin, Destination: route2.Destination}, "ops", at); err != nil {
		t.Fatal(err)
	}
	tendered := base
	tendered.Tendered = CargoScope{Packs: 2, GrossWeight: kg(200)}
	tendered.Departed, tendered.Offloaded = 2, 1
	tendered.Flights = []Flight{{Transport: "MU587/01DEC", Departed: 1, Offloaded: 1}, {Transport: "MU587/02DEC", Departed: 1}}
	if _, err := short.RecordEvidence(tendered, "ops", at); err != nil {
		t.Fatal(err)
	}
	if n := short.AwaitingLaterFlight(); n != 0 {
		t.Fatalf("all tendered flew: %d awaiting", n)
	}
	// an arrival recorded for the offloaded pieces without their later departure: nothing awaits
	arrived := base
	arrived.Departed, arrived.Arrived, arrived.Offloaded = 2, 4, 2
	arrived.Flights = []Flight{{Transport: "MU587/01DEC", Departed: 2, Arrived: 2, Offloaded: 2}, {Transport: "MU587/02DEC", Arrived: 2}}
	if _, err := short.RecordEvidence(arrived, "ops", at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n := short.AwaitingLaterFlight(); n != 0 {
		t.Fatalf("all arrived: %d awaiting", n)
	}
	// a flight reference is PHY's form
	odd := base
	odd.Departed, odd.Flights = 1, []Flight{{Transport: "\tMU587", Departed: 1}}
	if _, err := short.RecordEvidence(odd, "ops", at.Add(2*time.Minute)); !errors.Is(err, ErrInvalidEvidence) {
		t.Fatalf("a flight reference not of PHY's form: %v", err)
	}

	// evidence without flights keeps the digest it had before V2.111
	plain := base
	if plain.Digest() == first.Digest() || len(plain.Flights) != 0 {
		t.Fatal("digest")
	}
}
