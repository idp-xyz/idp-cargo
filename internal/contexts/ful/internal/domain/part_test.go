package domain

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

func kernelQ(unit string, n int64) kernel.Quantity {
	return kernel.Quantity{Unit: kernel.UnitCode(unit), Amount: n}
}

// part is n packs weighing w kilograms, without volume.
func part(n, w int64) CargoScope { return CargoScope{Packs: n, GrossWeight: kg(w)} }

// partV is n packs weighing w kilograms with v cubic decimetres.
func partV(n, w, v int64) CargoScope { return CargoScope{Packs: n, GrossWeight: kg(w), Volume: dm3(v)} }

// refV is ref with a declared volume of v cubic decimetres.
func refV(id string, n, w, v int64) ObligationRef {
	o := ref(id, n, w)
	o.Volume = dm3(v)
	return o
}

func TestAPartFitsWhatOtherShipmentsLeave(t *testing.T) {
	ob := refV("OB-1", 30, 1500, 9000)
	for name, tc := range map[string]struct {
		part, elsewhere CargoScope
		ok              bool
	}{
		"all of it":                         {partV(30, 1500, 9000), CargoScope{}, true},
		"a first part":                      {partV(22, 1100, 6600), CargoScope{}, true},
		"the rest after a first part":       {partV(8, 400, 2400), partV(22, 1100, 6600), true},
		"a middle part":                     {partV(5, 250, 1500), partV(22, 1100, 6600), true},
		"more packs than left":              {partV(9, 400, 2400), partV(22, 1100, 6600), false},
		"more weight than left":             {partV(7, 401, 2000), partV(22, 1100, 6600), false},
		"last packs leave weight behind":    {partV(8, 399, 2400), partV(22, 1100, 6600), false},
		"last packs leave volume behind":    {partV(8, 400, 2399), partV(22, 1100, 6600), false},
		"packs left without weight":         {partV(29, 1500, 8000), CargoScope{}, false},
		"packs left without volume":         {partV(29, 1400, 9000), CargoScope{}, false},
		"no packs":                          {partV(0, 100, 100), CargoScope{}, false},
		"no weight":                         {partV(3, 0, 100), CargoScope{}, false},
		"no volume though declared":         {part(3, 100), CargoScope{}, false},
		"weight in another unit":            {CargoScope{Packs: 3, GrossWeight: kernelQ("LBR", 100), Volume: dm3(100)}, CargoScope{}, false},
		"elsewhere in another unit":         {partV(3, 100, 100), CargoScope{Packs: 2, GrossWeight: kernelQ("LBR", 100), Volume: dm3(10)}, false},
		"nothing left after the whole else": {partV(1, 1, 1), partV(30, 1500, 9000), false},
	} {
		if err := ob.fits(tc.part, tc.elsewhere); (err == nil) != tc.ok || (err != nil && !errors.Is(err, ErrInvalidPart)) {
			t.Errorf("%s: err = %v, want ok %v", name, err, tc.ok)
		}
	}
	if err := ref("OB-2", 4, 200).fits(partV(4, 200, 10), CargoScope{}); !errors.Is(err, ErrInvalidPart) {
		t.Errorf("volume on an obligation without one: err = %v", err)
	}
	if rest, err := ob.Remaining(partV(22, 1100, 6600)); err != nil || rest != partV(8, 400, 2400) {
		t.Errorf("remaining = %+v, %v", rest, err)
	}
	if rest, err := ob.Remaining(CargoScope{}); err != nil || rest != ob.Whole() {
		t.Errorf("nothing planned elsewhere leaves the whole: %+v, %v", rest, err)
	}
}

// F1: a carrier loads 22 of 30 pallets. The planner reduces the part in the first shipment and plans the
// other 8 in a second one; COM's obligation is untouched.
func TestAShortloadIsReplannedInFUL(t *testing.T) {
	ob := refV("OB-1", 30, 1500, 9000)
	first, err := CreateShipmentForPart("T", "SH-1", ob, ob.Whole(), CargoScope{}, "planner-1", at)
	if err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(first.AmendPlan(route(at.Add(24*time.Hour)), "booked", "planner-1", at), first.Confirm("planner-2", at.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	first.Acknowledge()
	stored, err := RestoreShipment(first.State())
	if err != nil {
		t.Fatal(err)
	}
	later := at.Add(time.Hour)
	for name, tc := range map[string]struct {
		id      ObligationID
		version int64
		part    CargoScope
		reason  string
		want    error
	}{
		"no reason":          {"OB-1", 1, partV(22, 1100, 6600), " ", ErrInvalidPlan},
		"unchanged":          {"OB-1", 1, ob.Whole(), "shortload", ErrInvalidPart},
		"another obligation": {"OB-9", 1, partV(22, 1100, 6600), "shortload", ErrNotPlanned},
		"another version":    {"OB-1", 2, partV(22, 1100, 6600), "shortload", ErrNotPlanned},
		"leaves no weight":   {"OB-1", 1, partV(22, 1500, 6600), "shortload", ErrInvalidPart},
	} {
		if err := stored.ChangePart(tc.id, tc.version, tc.part, CargoScope{}, tc.reason, "planner-1", later); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	if len(stored.Pending()) != 0 || len(stored.AddedPlans()) != 0 {
		t.Fatal("refused changes changed the shipment")
	}
	if err = stored.ChangePart("OB-1", 1, partV(22, 1100, 6600), CargoScope{}, "MU587 loaded 22 of 30 pallets", "planner-1", later); err != nil {
		t.Fatal(err)
	}
	st := stored.State()
	latest := stored.Latest()
	if st.Status != ShipmentDraft || latest.Change != PlanPartChanged || latest.Reason != "MU587 loaded 22 of 30 pallets" ||
		latest.Scope != partV(22, 1100, 6600) || !latest.Route.Same(route(at.Add(24*time.Hour))) {
		t.Fatalf("after the change: status %s, plan %+v", st.Status, latest)
	}
	if len(st.Obligations) != 2 || st.Obligations[0].Planned() || st.Obligations[0].Part != ob.Whole() ||
		!st.Obligations[1].Planned() || st.Obligations[1].Part != partV(22, 1100, 6600) {
		t.Fatalf("entries %+v", st.Obligations)
	}
	if released := stored.ReleasedObligations(); len(released) != 1 || released[1].ReleasedBy != "planner-1" {
		t.Fatalf("released %+v", released)
	}
	if added := stored.AddedObligations(); len(added) != 1 || added[0].Part != partV(22, 1100, 6600) {
		t.Fatalf("added %+v", added)
	}
	if got := eventList(stored); got != "[ShipmentPartChanged.v1/OB-1/1]" {
		t.Fatalf("events %s", got)
	}
	if _, err = RestoreShipment(stored.State()); err != nil {
		t.Fatalf("a changed part restores: %v", err)
	}

	// The 8 pallets left are planned in a second shipment, for the next flight.
	elsewhere := partV(22, 1100, 6600)
	if _, err = CreateShipmentForPart("T", "SH-2", ob, partV(9, 400, 2400), elsewhere, "planner-1", later); !errors.Is(err, ErrInvalidPart) {
		t.Fatalf("more than is left: err = %v", err)
	}
	rest, err := ob.Remaining(elsewhere)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CreateShipmentForPart("T", "SH-2", ob, rest, elsewhere, "planner-1", later)
	if err != nil {
		t.Fatal(err)
	}
	if second.Latest().Scope != partV(8, 400, 2400) || !reflect.DeepEqual(second.Planned(), []ObligationRef{ob}) {
		t.Fatalf("second shipment plans %+v", second.Latest().Scope)
	}

	// A shipment may also take a part of an obligation that another one already plans a part of.
	// (One shipment does not mix cargo with and without a volume, so its first obligation declares one.)
	third, err := CreateShipment("T", "SH-3", refV("OB-2", 4, 200, 1000), "planner-1", later)
	if err != nil {
		t.Fatal(err)
	}
	if err = third.AddPart(ob, partV(3, 150, 900), elsewhere, "planner-1", later); err != nil {
		t.Fatalf("adding a part: %v", err)
	}
	if err = third.AddPart(ob, partV(3, 150, 900), elsewhere, "planner-1", later); !errors.Is(err, ErrAlreadyPlanned) {
		t.Fatalf("adding the same obligation twice: err = %v", err)
	}

	// A cancelled shipment changes no part.
	if err = second.Cancel("customer cancelled", "planner-1", later); err != nil {
		t.Fatal(err)
	}
	if err = second.ChangePart("OB-1", 1, partV(7, 350, 2100), elsewhere, "x", "planner-1", later); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("changing a cancelled shipment: err = %v", err)
	}
}
