package domain

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"
)

// twoHouses tenders SH-1 and SH-2 of C-1, 6 cartons each, at the airline's terminal.
func twoHouses(t *testing.T) *LoadExecution {
	t.Helper()
	c := airConsol(t, "C-1")
	m1, _ := NewMembership("T", "SH-1")
	m2, _ := NewMembership("T", "SH-2")
	if err := errors.Join(m1.Attach(c, intent("SH-1", 1), "ops", t0.Add(time.Hour)), m2.Attach(c, intent("SH-2", 1), "ops", t0.Add(time.Hour)),
		c.Close(scopes(2), t0.Add(time.Hour), "", "ops", t0.Add(2*time.Hour))); err != nil {
		t.Fatal(err)
	}
	x, err := OpenLoadExecution("T", "X-1", c, ExecutionTender, terminal, "ops", t0.Add(4*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	at := t0.Add(5 * time.Hour)
	l1 := received(t)
	l2, err := ReceiveCargo("T", "L-2", warehouse, intent("SH-2", 1), cartons(6, ConditionGood, t0.Add(time.Hour)), "ops", t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	e1, _ := m1.Live()
	e2, _ := m2.Live()
	if err = errors.Join(l2.Measure(scale(300, t0.Add(2*time.Hour)), "ops", t0.Add(2*time.Hour)), l2.Complete(intent("SH-2", 1), "ops", t0.Add(3*time.Hour)),
		x.Start("ops", t0.Add(4*time.Hour)),
		x.Load(LoadLine{Packs: 6, Evidence: ev("TD-1", at)}, c, l1, e1, LotLoad{}, 0, "ops", at),
		x.Load(LoadLine{Packs: 6, Evidence: ev("TD-2", at)}, c, l2, e2, LotLoad{}, 0, "ops", at),
		x.Close(ev("TR", at), "ops", at)); err != nil {
		t.Fatal(err)
	}
	return x
}

func reportOn(t *testing.T, x *LoadExecution, fact CarrierFact, shipment ShipmentID, pieces int64, flight, ref string) {
	t.Helper()
	at := t0.Add(6 * time.Hour)
	if _, _, err := x.RecordCarrierEvidence(CarrierEvidence{Fact: fact, ShipmentID: shipment, Pieces: pieces, Transport: flight,
		Evidence: Evidence{Source: "MU", Reference: ref, OccurredAt: at}}, "ops", at); err != nil {
		t.Fatal(err)
	}
}

// V2.111, the owner's scenario 1: the airline flies 10 of 12 on MU587 of 1 December and offloads 2 of SH-1, then flies
// them on MU587 of 2 December under the same master. Each flight is attributed to the houses, and the totals are the
// flights added up, whether the second flight is reported for the master or for SH-1 itself.
func TestTheOffloadedPiecesFlyOnTheNextFlight(t *testing.T) {
	for name, second := range map[string]ShipmentID{"reported for the master": "", "reported for the house": "SH-1"} {
		x := twoHouses(t)
		reportOn(t, x, CarrierDeparted, "", 10, "MU587/01DEC", "M-1")
		reportOn(t, x, CarrierOffloaded, "SH-1", 2, "MU587/01DEC", "H-1")
		if f := x.HouseFlights("SH-1"); len(f) != 1 || f[0] != (FlightPieces{"MU587/01DEC", HousePieces{Departed: 4, Offloaded: 2}}) {
			t.Fatalf("%s, SH-1 after the first flight: %+v", name, f)
		}
		if p := x.HousePieces("SH-2"); p != (HousePieces{Departed: 6}) {
			t.Fatalf("%s, SH-2: %+v", name, p)
		}
		reportOn(t, x, CarrierDeparted, second, 2, "MU587/02DEC", "M-2")
		want := []FlightPieces{{"MU587/01DEC", HousePieces{Departed: 4, Offloaded: 2}}, {"MU587/02DEC", HousePieces{Departed: 2}}}
		if f := x.HouseFlights("SH-1"); !slices.Equal(f, want) {
			t.Fatalf("%s, SH-1's flights: %+v", name, f)
		}
		if p := x.HousePieces("SH-1"); p != (HousePieces{Departed: 6, Offloaded: 2}) {
			t.Fatalf("%s, SH-1's totals: %+v", name, p)
		}
		if f := x.HouseFlights("SH-2"); len(f) != 1 || f[0].Departed != 6 {
			t.Fatalf("%s, SH-2 flew on the first flight alone: %+v", name, f)
		}
		// the master's arrivals: each flight's arrival covers what departed on it
		reportOn(t, x, CarrierArrived, "", 10, "MU587/01DEC", "A-1")
		reportOn(t, x, CarrierArrived, "", 2, "MU587/02DEC", "A-2")
		if p := x.HousePieces("SH-1"); p.Arrived != 6 {
			t.Fatalf("%s, SH-1 arrived: %+v", name, p)
		}
		if p := x.HousePieces("SH-2"); p.Arrived != 6 {
			t.Fatalf("%s, SH-2 arrived: %+v", name, p)
		}
	}

	// an air flight without its date is refused: the same flight number on another day is another flight
	y := twoHouses(t)
	if _, _, err := y.RecordCarrierEvidence(CarrierEvidence{Fact: CarrierDeparted, Pieces: 10, Transport: "MU587",
		Evidence: Evidence{Source: "MU", Reference: "M-1", OccurredAt: t0.Add(6 * time.Hour)}}, "ops", t0.Add(6*time.Hour)); !errors.Is(err, ErrInvalidCarrier) {
		t.Fatalf("an undated air flight: %v", err)
	}

	// what the airline reports beyond a house's pieces is never more than its live pieces, over all flights
	z := twoHouses(t)
	reportOn(t, z, CarrierDeparted, "SH-1", 5, "MU587/01DEC", "H-5")
	reportOn(t, z, CarrierDeparted, "SH-1", 5, "MU587/02DEC", "H-6")
	if f := z.HouseFlights("SH-1"); len(f) != 2 || f[0].Departed != 5 || f[1].Departed != 1 || z.HousePieces("SH-1").Departed != 6 {
		t.Fatalf("a house's flights capped at its pieces: %+v", f)
	}
}

// reportAt records a carrier report that occurred at a given hour after t0, entered at hour 60.
func reportAt(t *testing.T, x *LoadExecution, fact CarrierFact, shipment ShipmentID, pieces int64, flight, ref string, hour int) {
	t.Helper()
	if _, _, err := x.RecordCarrierEvidence(CarrierEvidence{Fact: fact, ShipmentID: shipment, Pieces: pieces, Transport: flight,
		Evidence: Evidence{Source: "MU", Reference: ref, OccurredAt: t0.Add(time.Duration(hour) * time.Hour)}}, "ops", t0.Add(60*time.Hour)); err != nil {
		t.Fatal(err)
	}
}

// flightsAddUp checks that a house's flights add up to its totals.
func flightsAddUp(t *testing.T, label string, x *LoadExecution, h ShipmentID) {
	t.Helper()
	var sum HousePieces
	for _, f := range x.HouseFlights(h) {
		sum.Departed, sum.Arrived, sum.Offloaded = sum.Departed+f.Departed, sum.Arrived+f.Arrived, sum.Offloaded+f.Offloaded
	}
	if sum != x.HousePieces(h) {
		t.Fatalf("%s: %s's flights %+v do not add up to %+v", label, h, x.HouseFlights(h), x.HousePieces(h))
	}
}

// After the review of V2.111: a house's totals are never less than the rule over all flights gave before, the pieces a
// later master report flew are given to a house only when PHY can tell they were its pieces, and flights are taken in the
// carrier's order.
func TestFlightsNeverCountLessThanTheWholeTender(t *testing.T) {
	type report struct {
		fact     CarrierFact
		shipment ShipmentID
		pieces   int64
		flight   string
		hour     int
	}
	for _, c := range []struct {
		name    string
		reports []report
		want    map[ShipmentID]HousePieces
		flights map[ShipmentID][]string
	}{
		{"an offload reported for the master alone", []report{{CarrierDeparted, "", 10, "MU587/01DEC", 6}, {CarrierOffloaded, "", 2, "MU587/01DEC", 6},
			{CarrierDeparted, "", 2, "MU587/02DEC", 30}},
			map[ShipmentID]HousePieces{"SH-1": {Departed: 6}, "SH-2": {Departed: 6}},
			map[ShipmentID][]string{"SH-1": {"MU587/02DEC"}, "SH-2": {"MU587/02DEC"}}},
		{"the master split over two flights, nothing offloaded", []report{{CarrierDeparted, "", 10, "MU587/01DEC", 6}, {CarrierDeparted, "", 2, "MU587/02DEC", 30}},
			map[ShipmentID]HousePieces{"SH-1": {Departed: 6}, "SH-2": {Departed: 6}}, nil},
		{"offloads named and unnamed: later flights are no house's", []report{{CarrierDeparted, "", 8, "MU587/01DEC", 6}, {CarrierOffloaded, "SH-1", 2, "MU587/01DEC", 6},
			{CarrierOffloaded, "", 2, "MU587/01DEC", 6}, {CarrierDeparted, "", 2, "MU587/02DEC", 30}, {CarrierDeparted, "", 2, "MU587/03DEC", 54}},
			map[ShipmentID]HousePieces{"SH-1": {Departed: 6, Offloaded: 2}, "SH-2": {Departed: 6}},
			map[ShipmentID][]string{"SH-1": {"MU587/01DEC", "MU587/03DEC"}, "SH-2": {"MU587/03DEC"}}},
		{"the next flight recorded first", []report{{CarrierDeparted, "", 2, "MU587/02DEC", 30}, {CarrierDeparted, "", 10, "MU587/01DEC", 6},
			{CarrierOffloaded, "SH-1", 2, "MU587/01DEC", 6}},
			map[ShipmentID]HousePieces{"SH-1": {Departed: 6, Offloaded: 2}, "SH-2": {Departed: 6}},
			map[ShipmentID][]string{"SH-1": {"MU587/01DEC", "MU587/02DEC"}, "SH-2": {"MU587/01DEC"}}},
		{"one house's offload flown over two later flights", []report{{CarrierDeparted, "", 10, "MU587/01DEC", 6}, {CarrierOffloaded, "SH-1", 2, "MU587/01DEC", 6},
			{CarrierDeparted, "", 1, "MU587/02DEC", 30}, {CarrierOffloaded, "SH-1", 1, "MU587/02DEC", 30}, {CarrierDeparted, "", 1, "MU587/03DEC", 54}},
			map[ShipmentID]HousePieces{"SH-1": {Departed: 6, Offloaded: 3}, "SH-2": {Departed: 6}},
			map[ShipmentID][]string{"SH-1": {"MU587/01DEC", "MU587/02DEC", "MU587/03DEC"}}},
		{"two houses' offloads flown in parts", []report{{CarrierDeparted, "", 8, "MU587/01DEC", 6}, {CarrierOffloaded, "SH-1", 2, "MU587/01DEC", 6},
			{CarrierOffloaded, "SH-2", 2, "MU587/01DEC", 6}, {CarrierDeparted, "", 2, "MU587/02DEC", 30}, {CarrierDeparted, "", 2, "MU587/03DEC", 54}},
			map[ShipmentID]HousePieces{"SH-1": {Departed: 6, Offloaded: 2}, "SH-2": {Departed: 6, Offloaded: 2}},
			map[ShipmentID][]string{"SH-1": {"MU587/01DEC", "MU587/03DEC"}, "SH-2": {"MU587/01DEC", "MU587/03DEC"}}},
		{"an arrival counts no piece that has not departed", []report{{CarrierDeparted, "", 10, "MU587/01DEC", 6}, {CarrierOffloaded, "SH-1", 2, "MU587/01DEC", 6},
			{CarrierArrived, "", 12, "MU587/01DEC", 20}},
			map[ShipmentID]HousePieces{"SH-1": {Departed: 4, Arrived: 4, Offloaded: 2}, "SH-2": {Departed: 6, Arrived: 6}}, nil},
	} {
		x := twoHouses(t)
		for i, r := range c.reports {
			reportAt(t, x, r.fact, r.shipment, r.pieces, r.flight, fmt.Sprintf("R-%d", i), r.hour)
		}
		for h, want := range c.want {
			if got := x.HousePieces(h); got != want {
				t.Errorf("%s: %s %+v, want %+v (flights %+v)", c.name, h, got, want, x.HouseFlights(h))
			}
			if got, before := x.HousePieces(h), x.wholeTender(h); got.Departed < min(before.Departed, x.LiveFor(h)) {
				t.Errorf("%s: %s departed %d, fewer than the rule over all flights (%d)", c.name, h, got.Departed, before.Departed)
			}
			flightsAddUp(t, c.name, x, h)
		}
		for h, want := range c.flights {
			var got []string
			for _, f := range x.HouseFlights(h) {
				got = append(got, f.Transport)
			}
			if !slices.Equal(got, want) {
				t.Errorf("%s: %s's flights %v, want %v (%+v)", c.name, h, got, want, x.HouseFlights(h))
			}
		}
	}
}
