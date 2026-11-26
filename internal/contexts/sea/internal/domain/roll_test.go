package domain

import (
	"errors"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// A container rolled to a later vessel (V2.105): a sealed container that is not loaded moves to another movement of its
// consolidation, and its houses with it.
func TestASealedContainerRollsToAnotherMovementWithItsHouses(t *testing.T) {
	m := stuffedMovement(t, "CN-SEAL-1")
	v := assigned(t, m)
	// assigned, not stuffed: refused
	later := open(t, booking(1))
	later.state.MovementID = "OM-2"
	if err := v.RollTo(m, later, "rolled", "ops", at.Add(2*time.Hour)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("an empty container: %v", err)
	}
	sealed(t, v, "CN-SEAL-1", 3)
	done := at.Add(21 * time.Hour)
	if err := v.StuffingDone(m, "", "cfs", done); err != nil {
		t.Fatal(err)
	}
	when := done.Add(time.Hour)
	for name, tc := range map[string]struct {
		to     *Movement
		reason string
		want   error
	}{
		"no reason":         {later, " ", ErrInvalidChange},
		"the same movement": {m, "rolled", ErrInvalidChange},
		"another consol": {func() *Movement {
			o := open(t, booking(1))
			o.state.MovementID, o.state.ConsolidationID = "OM-9", "C-9"
			return o
		}(), "rolled", ErrInvalidChange},
		"a co-load movement": {func() *Movement {
			o := open(t, booking(1))
			o.state.MovementID, o.state.Kind = "OM-8", KindCoLoad
			return o
		}(), "rolled", ErrInvalidChange},
		"a cancelled one": {func() *Movement {
			o := open(t, booking(1))
			o.state.MovementID, o.state.State = "OM-7", StateCancelled
			return o
		}(), "rolled", ErrInvalidTransition},
	} {
		if err := v.RollTo(m, tc.to, tc.reason, "ops", when); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	houses, whole := m.HousesInStuffing(v.State().ExecutionID)
	if len(houses) != 1 || !whole || houses[0].ShipmentID != "SH-1" {
		t.Fatalf("houses in the container: %+v %v", houses, whole)
	}
	if err := v.RollTo(m, later, "shut out, rolled to the next vessel", "ops", when); err != nil {
		t.Fatal(err)
	}
	if err := m.RollOut("SH-1", later.ID(), "ops", when); err != nil {
		t.Fatal(err)
	}
	if err := later.RollIn(m, houses[0], "ops", when); err != nil {
		t.Fatal(err)
	}
	if v.State().MovementID != "OM-2" || len(v.AddedRolls()) != 1 || len(m.Houses()) != 0 || len(later.Houses()) != 1 ||
		later.State().Entries[0].Start != StartRolled || later.State().Entries[0].HBL != "H1" || m.State().Entries[0].End != EndRolled {
		t.Fatalf("rolled: %+v / %+v / %+v", v.State(), m.State().Entries, later.State().Entries)
	}
	// a rolled visit and both movements restore as they were
	if again, err := RestoreVisit(v.State()); err != nil || len(again.State().Rolls) != 1 || again.State().MovementID != "OM-2" {
		t.Fatalf("restore the visit: %v", err)
	}
	if _, err := RestoreMovement(m.State()); err != nil {
		t.Fatalf("restore the movement left: %v", err)
	}
	if _, err := RestoreMovement(later.State()); err != nil {
		t.Fatalf("restore the movement joined: %v", err)
	}
	// a roll that does not end at the visit's movement does not restore
	broken := v.State()
	broken.MovementID = "OM-1"
	if _, err := RestoreVisit(broken); err == nil {
		t.Fatal("a roll that does not end at the visit's movement restored")
	}
	// a loaded container does not roll: the carrier discharges it first
	gated := when.Add(time.Hour)
	if err := v.AssertVGM(later, kernel.Quantity{Unit: "KGM", Amount: plate.TareKg}, VgmMethod1, "LE", 1, gated, "", "Ada", "ops", gated); err != nil {
		t.Fatal(err)
	}
	if _, _, err := v.RecordContainerEvent(EventGateIn, loc("CNSHG"), gated, "terminal", "GIN-1", "ops", gated); err != nil {
		t.Fatal(err)
	}
	if _, _, err := v.RecordContainerEvent(EventLoaded, loc("CNSHG"), gated, "vessel", "LOA-1", "ops", gated); err != nil {
		t.Fatal(err)
	}
	if err := v.RollTo(later, m, "rolled back", "ops", gated); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a loaded container: %v", err)
	}
}

// A movement takes one capacity line of its booking (V2.105): another line's legs, and not a line the booking lacks.
func TestAMovementSailsOnOneLineOfItsBooking(t *testing.T) {
	b := booking(1)
	second := append([]Leg(nil), b.Legs...)
	second[0].Vessel, second[0].ETD = "EVER GLORY", b.Legs[0].ETD.Add(7*24*time.Hour)
	b.Lines, b.LineLegs = 2, [][]Leg{b.Legs, second}
	m, err := OpenMovementOnLine("T", "OM-2", "C-1", b, 2, "ops", at)
	if err != nil || m.Current().Legs[0].Vessel != "EVER GLORY" || m.Current().Line != 2 || m.State().BookingLine != 2 {
		t.Fatalf("line 2: %+v %v", m, err)
	}
	if _, err := OpenMovementOnLine("T", "OM-3", "C-1", b, 3, "ops", at); !errors.Is(err, ErrInvalidBooking) {
		t.Fatalf("line 3 of 2: %v", err)
	}
	// a newer capacity version keeps the movement on its line
	b.CapacityVersion, b.Revision = 2, b.Revision+1
	b.LineLegs[1][0].Voyage = "0131W"
	if err := m.LinkBooking(b, "ops", at.Add(time.Hour)); err != nil || m.Current().Legs[0].Voyage != "0131W" || m.Current().Line != 2 {
		t.Fatalf("relinked: %+v %v", m.Current(), err)
	}
}
