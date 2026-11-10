package domain

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAReservedObligationStaysUntilItsShipmentReleasesIt(t *testing.T) {
	o := recorded(t)
	if err := o.Allocate(1, "OB-1", packsWithVolume(10, 500, 12_000), "planner-1", recordAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	o.Acknowledge()
	reserveAt := recordAt.Add(2 * time.Hour)
	for name, tc := range map[string]struct {
		id       ObligationID
		version  int64
		shipment string
		at       time.Time
		want     error
	}{
		"unknown obligation":           {"OB-9", 1, "S-1", reserveAt, ErrUnknownObligation},
		"unknown version":              {"OB-1", 2, "S-1", reserveAt, ErrUnknownObligation},
		"no shipment":                  {"OB-1", 1, " ", reserveAt, ErrInvalidChange},
		"shipment id longer than kept": {"OB-1", 1, strings.Repeat("s", maxID+1), reserveAt, ErrInvalidChange},
		"before the allocation":        {"OB-1", 1, "S-1", recordAt, ErrInvalidChange},
	} {
		if err := o.Reserve(tc.id, tc.version, tc.shipment, "fulfilment", tc.at); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	if len(o.Pending()) != 0 || len(o.AddedReservations()) != 0 {
		t.Fatal("refused reservations changed the order")
	}
	if err := o.Reserve("OB-1", 1, "S-1", "fulfilment", reserveAt); err != nil {
		t.Fatal(err)
	}
	if r, held := o.Held("OB-1"); !held || r != (Reservation{No: 1, ObligationID: "OB-1", ObligationVersion: 1, ShipmentID: "S-1",
		ReservedBy: "fulfilment", ReservedAt: reserveAt}) {
		t.Fatalf("held %v: %+v", held, r)
	}
	if o.Status() != OrderPartiallyAllocated {
		t.Fatalf("a reservation does not change the order's status: %s", o.Status())
	}

	// While S-1 holds OB-1, COM refuses to revoke it, to amend it, to cancel its order and to let S-1
	// reserve it again. Other shipments may reserve it too (F1): see TestSeveralShipmentsHoldOneObligation.
	busy := o.State()
	later := reserveAt.Add(time.Hour)
	for name, err := range map[string]error{
		"revoke":                o.Revoke("OB-1", "planner-1", later),
		"amend":                 o.Amend("OB-1", "OB-2", packsWithVolume(10, 500, 12_000), "planner-1", later),
		"cancel":                o.Cancel("sales-1", later, MinContactRetentionMonths),
		"reserve again for S-1": o.Reserve("OB-1", 1, "S-1", "fulfilment", later),
	} {
		if !errors.Is(err, ErrObligationReserved) {
			t.Errorf("%s: err = %v, want %v", name, err, ErrObligationReserved)
		}
	}
	for name, tc := range map[string]struct {
		version  int64
		shipment string
		at       time.Time
		want     error
	}{
		"by another shipment":    {1, "S-2", later, ErrNotReserved},
		"another version":        {2, "S-1", later, ErrNotReserved},
		"before it was reserved": {1, "S-1", reserveAt.Add(-time.Minute), ErrInvalidChange},
	} {
		if err := o.ReleaseReservation("OB-1", tc.version, tc.shipment, "fulfilment", tc.at); !errors.Is(err, tc.want) {
			t.Errorf("release %s: err = %v, want %v", name, err, tc.want)
		}
	}
	if !reflect.DeepEqual(o.State(), busy) {
		t.Fatal("refused changes altered the reserved order")
	}

	// Once stored, released by S-1 and revoked: the history keeps the reservation.
	restored, err := RestoreOrder(o.State())
	if err != nil {
		t.Fatal(err)
	}
	if err = restored.ReleaseReservation("OB-1", 1, "S-1", "fulfilment", later); err != nil {
		t.Fatal(err)
	}
	if released := restored.ReleasedReservations(); len(released) != 1 || released[0].No != 1 || released[0].ReleasedBy != "fulfilment" ||
		!released[0].ReleasedAt.Equal(later) || len(restored.AddedReservations()) != 0 {
		t.Fatalf("released %+v, added %+v", released, restored.AddedReservations())
	}
	if err = restored.ReleaseReservation("OB-1", 1, "S-1", "fulfilment", later); !errors.Is(err, ErrNotReserved) {
		t.Fatalf("releasing twice: err = %v", err)
	}
	if err = restored.Revoke("OB-1", "planner-1", later.Add(-time.Minute)); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("revoking before the release: err = %v", err)
	}
	if err = restored.Revoke("OB-1", "planner-1", later); err != nil {
		t.Fatal(err)
	}
	if err = restored.Reserve("OB-1", 1, "S-3", "fulfilment", later); !errors.Is(err, ErrObligationNotActive) {
		t.Fatalf("reserving a superseded version: err = %v", err)
	}
	if err = restored.Reserve("OB-1", 2, "S-3", "fulfilment", later); !errors.Is(err, ErrObligationNotActive) {
		t.Fatalf("reserving the revocation: err = %v", err)
	}
	if got, want := eventList(restored), "[ObligationReservationReleased.v1/OB-1/1/S-1 FulfillmentObligationRevoked.v1/OB-1/2]"; got != want {
		t.Fatalf("events %s", got)
	}
	if got := eventList(o); got != "[ObligationReserved.v1/OB-1/1/S-1]" {
		t.Fatalf("events of the reservation %s", got)
	}
	restored.Acknowledge()
	if len(restored.ReleasedReservations()) != 0 || len(restored.Added()) != 0 || len(restored.Pending()) != 0 {
		t.Fatal("Acknowledge must clear pending changes")
	}
	again, err := RestoreOrder(restored.State())
	if err != nil || len(again.State().Reservations) != 1 || again.State().Reservations[0].Held() {
		t.Fatalf("restore after release and revocation: %v", err)
	}
}

func TestAReleasedObligationCanBeReservedAgain(t *testing.T) {
	o := recorded(t)
	when := recordAt.Add(time.Hour)
	if err := errors.Join(
		o.Allocate(2, "OB-1", packs(12, 60), "planner-1", when),
		o.Reserve("OB-1", 1, "S-1", "fulfilment", when),
		o.ReleaseReservation("OB-1", 1, "S-1", "fulfilment", when),
		o.Reserve("OB-1", 1, "S-2", "fulfilment", when),
	); err != nil {
		t.Fatal(err)
	}
	if r, held := o.Held("OB-1"); !held || r.No != 2 || r.ShipmentID != "S-2" {
		t.Fatalf("held %v: %+v", held, r)
	}
	if added := o.AddedReservations(); len(added) != 2 || added[0].Held() || !added[1].Held() || len(o.ReleasedReservations()) != 0 {
		t.Fatalf("a reservation made and released before it was stored is one added row: %+v", added)
	}
}

// F1: FUL may plan parts of one obligation in several shipments, so each of them holds its own reservation.
// The obligation cannot be revoked until every one is released.
func TestSeveralShipmentsHoldOneObligation(t *testing.T) {
	o := recorded(t)
	when := recordAt.Add(time.Hour)
	if err := errors.Join(
		o.Allocate(1, "OB-1", packsWithVolume(10, 500, 12_000), "planner-1", when),
		o.Reserve("OB-1", 1, "S-1", "fulfilment", when),
		o.Reserve("OB-1", 1, "S-2", "fulfilment", when.Add(time.Minute)),
	); err != nil {
		t.Fatal(err)
	}
	holders := o.Holders("OB-1")
	if len(holders) != 2 || holders[0].ShipmentID != "S-1" || holders[1].ShipmentID != "S-2" || holders[1].No != 2 {
		t.Fatalf("holders %+v", holders)
	}
	if r, held := o.Held("OB-1"); !held || r.ShipmentID != "S-1" {
		t.Fatalf("Held returns the earliest holder: %v %+v", held, r)
	}
	if err := o.Reserve("OB-1", 1, "S-2", "fulfilment", when.Add(2*time.Minute)); !errors.Is(err, ErrObligationReserved) {
		t.Fatalf("S-2 reserving again: err = %v", err)
	}
	restored, err := RestoreOrder(o.State())
	if err != nil {
		t.Fatalf("an order with two holders of one obligation restores: %v", err)
	}
	release := when.Add(time.Hour)
	if err = restored.ReleaseReservation("OB-1", 1, "S-1", "fulfilment", release); err != nil {
		t.Fatal(err)
	}
	if err = restored.Revoke("OB-1", "planner-1", release); !errors.Is(err, ErrObligationReserved) {
		t.Fatalf("revoking while S-2 still holds it: err = %v", err)
	}
	if err = restored.ReleaseReservation("OB-1", 1, "S-2", "fulfilment", release); err != nil {
		t.Fatal(err)
	}
	if err = restored.Revoke("OB-1", "planner-1", release); err != nil {
		t.Fatalf("revoking once every holder released it: %v", err)
	}

	// A stored order may not show one shipment holding an obligation twice.
	st := o.State()
	st.Reservations = append(st.Reservations, Reservation{No: 3, ObligationID: "OB-1", ObligationVersion: 1, ShipmentID: "S-2",
		ReservedBy: "fulfilment", ReservedAt: when.Add(3 * time.Minute)})
	if _, err = RestoreOrder(st); !errors.Is(err, ErrInvalidOrder) {
		t.Fatalf("one shipment holding twice: err = %v", err)
	}
}
