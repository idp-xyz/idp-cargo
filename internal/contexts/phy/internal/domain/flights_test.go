package domain

import (
	"errors"
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

	// the same flight number without its date is one flight: the second report is the first's again (largest counts)
	y := twoHouses(t)
	reportOn(t, y, CarrierDeparted, "", 10, "MU587", "M-1")
	reportOn(t, y, CarrierOffloaded, "SH-1", 2, "MU587", "H-1")
	reportOn(t, y, CarrierDeparted, "", 2, "MU587", "M-2")
	if p := y.HousePieces("SH-1"); p.Departed != 4 {
		t.Fatalf("one flight number, no dates: %+v", p)
	}

	// what the airline reports beyond a house's pieces is never more than its live pieces, over all flights
	z := twoHouses(t)
	reportOn(t, z, CarrierDeparted, "SH-1", 5, "MU587/01DEC", "H-5")
	reportOn(t, z, CarrierDeparted, "SH-1", 5, "MU587/02DEC", "H-6")
	if f := z.HouseFlights("SH-1"); len(f) != 2 || f[0].Departed != 5 || f[1].Departed != 1 || z.HousePieces("SH-1").Departed != 6 {
		t.Fatalf("a house's flights capped at its pieces: %+v", f)
	}
}
