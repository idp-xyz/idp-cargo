package domain

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

var at = time.Date(2026, 11, 7, 8, 0, 0, 0, time.UTC)

func kg(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }

func dm3(n int64) kernel.Quantity { return kernel.Quantity{Unit: "DMQ", Amount: n} }

// ref is an obligation version of customer P and legal entity LE: n pallets weighing w kilograms.
func ref(id string, n, w int64) ObligationRef {
	return ObligationRef{ObligationID: ObligationID(id), ObligationVersion: 1, OrderID: "O-1", CustomerPartyID: "P", LegalEntityID: "LE",
		Packs: n, PackageType: "PX", GrossWeight: kg(w)}
}

func loc(code string) kernel.LocationCode {
	return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code}
}

// route is a complete route by sea from Shanghai to Hamburg, departing within two days of from.
func route(from time.Time) Route {
	return Route{Mode: ModeSea, Origin: loc("CNSHA"), Destination: loc("DEHAM"), Departure: kernel.ValidityInterval{From: from, Until: from.Add(48 * time.Hour)}}
}

func created(t *testing.T) *Shipment {
	t.Helper()
	s, err := CreateShipment("T", "SH-1", ref("OB-1", 4, 200), "planner-1", at)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func eventList(s *Shipment) string {
	var got []string
	for _, e := range s.Pending() {
		k := e.Type
		if e.ObligationID != "" {
			k += fmt.Sprintf("/%s/%d", e.ObligationID, e.ObligationVersion)
		}
		got = append(got, k)
	}
	return fmt.Sprint(got)
}

func TestCreateShipmentStartsADraftFromOneObligation(t *testing.T) {
	s := created(t)
	st := s.State()
	if st.Status != ShipmentDraft || st.CustomerPartyID != "P" || st.LegalEntityID != "LE" || len(st.Obligations) != 1 ||
		st.Plans[0] != (PlanVersion{No: 1, Change: PlanCreated, Scope: CargoScope{Packs: 4, GrossWeight: kg(200)}, By: "planner-1", At: at}) {
		t.Fatalf("state %+v", st)
	}
	if got := eventList(s); got != "[ShipmentCreated.v1/OB-1/1]" {
		t.Fatalf("events %s", got)
	}
	for name, tc := range map[string]struct {
		id   ShipmentID
		ob   ObligationRef
		want error
	}{
		"no shipment id":           {" ", ref("OB-1", 4, 200), ErrInvalidShipment},
		"shipment id too long":     {ShipmentID(strings.Repeat("s", maxID+1)), ref("OB-1", 4, 200), ErrInvalidShipment},
		"no packs":                 {"SH-1", ref("OB-1", 0, 200), ErrInvalidObligation},
		"no obligation version":    {"SH-1", func() ObligationRef { o := ref("OB-1", 4, 200); o.ObligationVersion = 0; return o }(), ErrInvalidObligation},
		"volume unit but no value": {"SH-1", func() ObligationRef { o := ref("OB-1", 4, 200); o.Volume = dm3(0); return o }(), ErrInvalidObligation},
	} {
		if _, err := CreateShipment("T", tc.id, tc.ob, "planner-1", at); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

func TestAddObligationKeepsOneCustomerAndSumsTheScope(t *testing.T) {
	s := created(t)
	s.Acknowledge()
	later := at.Add(time.Hour)
	with := func(edit func(*ObligationRef)) ObligationRef {
		o := ref("OB-2", 6, 300)
		edit(&o)
		return o
	}
	before := s.State()
	for name, tc := range map[string]struct {
		ob   ObligationRef
		at   time.Time
		want error
	}{
		"another customer":         {with(func(o *ObligationRef) { o.CustomerPartyID = "Q" }), later, ErrOtherCustomer},
		"another legal entity":     {with(func(o *ObligationRef) { o.LegalEntityID = "LE-2" }), later, ErrOtherCustomer},
		"already planned":          {ref("OB-1", 4, 200), later, ErrAlreadyPlanned},
		"weight in pounds":         {with(func(o *ObligationRef) { o.GrossWeight = kernel.Quantity{Unit: "LBR", Amount: 9} }), later, kernel.ErrUnitMismatch},
		"a volume the first lacks": {with(func(o *ObligationRef) { o.Volume = dm3(5) }), later, kernel.ErrUnitMismatch},
		"before the last change":   {ref("OB-2", 6, 300), at.Add(-time.Minute), ErrInvalidChange},
	} {
		if err := s.AddObligation(tc.ob, "planner-1", tc.at); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	if !reflect.DeepEqual(s.State(), before) || len(s.Pending()) != 0 {
		t.Fatal("refused additions changed the shipment")
	}
	if err := s.AddObligation(ref("OB-2", 6, 300), "planner-1", later); err != nil {
		t.Fatal(err)
	}
	if p := s.Latest(); p.No != 2 || p.Change != PlanObligationAdded || p.Scope != (CargoScope{Packs: 10, GrossWeight: kg(500)}) {
		t.Fatalf("plan %+v", p)
	}
	if got := eventList(s); got != "[ShipmentObligationAdded.v1/OB-2/1]" {
		t.Fatalf("events %s", got)
	}
	if len(s.AddedObligations()) != 1 || len(s.AddedPlans()) != 1 {
		t.Fatalf("added %d obligations, %d plans", len(s.AddedObligations()), len(s.AddedPlans()))
	}
}

func TestAmendAndConfirmThePlan(t *testing.T) {
	s := created(t)
	s.Acknowledge()
	depart := at.Add(72 * time.Hour)
	partial := Route{Mode: ModeSea, Origin: loc("CNSHA")}
	if err := s.AmendPlan(partial, "port of loading agreed", "planner-1", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.Confirm("planner-2", at.Add(time.Hour)); !errors.Is(err, ErrIncompletePlan) {
		t.Fatalf("confirm a partial route: err = %v", err)
	}
	full := route(depart)
	for name, tc := range map[string]struct {
		route  Route
		reason string
	}{
		"no reason":             {full, " "},
		"the same route":        {partial, "again"},
		"an IATA airport":       {Route{Mode: ModeAir, Origin: kernel.LocationCode{Scheme: kernel.IATAAirport, Code: "PVG"}}, "air"},
		"origin is destination": {Route{Origin: loc("CNSHA"), Destination: loc("CNSHA")}, "loop"},
		"departure without end": {Route{Departure: kernel.ValidityInterval{From: depart}}, "open window"},
		"an unknown mode":       {Route{Mode: "BARGE"}, "barge"},
		"a malformed UN/LOCODE": {Route{Origin: loc("CN-SH")}, "typo"},
	} {
		if err := s.AmendPlan(tc.route, tc.reason, "planner-1", at.Add(2*time.Hour)); !errors.Is(err, ErrInvalidPlan) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if err := s.AmendPlan(full, "carrier schedule received", "planner-1", at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.Confirm("planner-2", at.Add(3*time.Hour)); err != nil || s.Status() != ShipmentPlanned {
		t.Fatalf("confirm: %v, status %s", err, s.Status())
	}
	if err := s.Confirm("planner-2", at.Add(3*time.Hour)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("confirm twice: err = %v", err)
	}
	if err := s.AddObligation(ref("OB-2", 1, 10), "planner-1", at.Add(3*time.Hour)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("add to a confirmed plan: err = %v", err)
	}
	rolled := route(depart.Add(7 * 24 * time.Hour))
	if err := s.AmendPlan(rolled, "rolled to next week's sailing", "planner-1", at.Add(4*time.Hour)); err != nil || s.Status() != ShipmentDraft {
		t.Fatalf("amend a confirmed plan: %v, status %s", err, s.Status())
	}
	if err := s.Confirm("planner-2", at.Add(5*time.Hour)); err != nil {
		t.Fatal(err)
	}
	st := s.State()
	if len(st.Plans) != 4 || !st.Plans[2].Route.Same(full) || st.Plans[3].Reason != "rolled to next week's sailing" ||
		len(st.Confirmations) != 2 || st.Confirmations[0].PlanNo != 3 || st.Confirmations[1].PlanNo != 4 {
		t.Fatalf("history: plans %+v, confirmations %+v", st.Plans, st.Confirmations)
	}
	want := "[ShipmentPlanAmended.v1 ShipmentPlanAmended.v1 ShipmentPlanConfirmed.v1 ShipmentPlanAmended.v1 ShipmentPlanConfirmed.v1]"
	if got := eventList(s); got != want {
		t.Fatalf("events %s", got)
	}
}

func TestReleaseAndCancel(t *testing.T) {
	s := created(t)
	if err := errors.Join(
		s.AddObligation(ref("OB-2", 6, 300), "planner-1", at.Add(time.Hour)),
		s.AmendPlan(route(at.Add(72*time.Hour)), "schedule", "planner-1", at.Add(time.Hour)),
		s.Confirm("planner-2", at.Add(2*time.Hour)),
	); err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreShipment(s.State())
	if err != nil {
		t.Fatal(err)
	}
	s = restored
	for name, tc := range map[string]struct {
		id      ObligationID
		version int64
		want    error
	}{
		"not planned":     {"OB-9", 1, ErrNotPlanned},
		"another version": {"OB-1", 2, ErrNotPlanned},
	} {
		if err := s.ReleaseObligation(tc.id, tc.version, "planner-1", at.Add(3*time.Hour)); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if err := s.ReleaseObligation("OB-1", 1, "planner-1", at.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if s.Status() != ShipmentDraft || s.Latest().Scope != (CargoScope{Packs: 6, GrossWeight: kg(300)}) || s.Latest().Change != PlanObligationReleased {
		t.Fatalf("after the release: status %s, plan %+v", s.Status(), s.Latest())
	}
	if err := s.ReleaseObligation("OB-2", 1, "planner-1", at.Add(3*time.Hour)); !errors.Is(err, ErrLastObligation) {
		t.Fatalf("release the last obligation: err = %v", err)
	}
	if released := s.ReleasedObligations(); len(released) != 1 || released[1].ObligationID != "OB-1" || released[1].ReleasedBy != "planner-1" {
		t.Fatalf("released %+v", released)
	}
	if err := s.Cancel(" ", "planner-1", at.Add(4*time.Hour)); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("cancel without a reason: err = %v", err)
	}
	plans := s.State().Plans
	if err := s.Cancel("customer withdrew the cargo", "planner-1", at.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	st := s.State()
	if st.Status != ShipmentCancelled || len(s.Planned()) != 0 || !reflect.DeepEqual(st.Plans, plans) || len(s.ReleasedObligations()) != 2 {
		t.Fatalf("cancelled: %+v", st)
	}
	later := at.Add(5 * time.Hour)
	for name, err := range map[string]error{
		"add":     s.AddObligation(ref("OB-3", 1, 10), "planner-1", later),
		"amend":   s.AmendPlan(route(later), "again", "planner-1", later),
		"confirm": s.Confirm("planner-2", later),
		"release": s.ReleaseObligation("OB-2", 1, "planner-1", later),
		"cancel":  s.Cancel("again", "planner-1", later),
	} {
		if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s on a cancelled shipment: err = %v", name, err)
		}
	}
	if got := eventList(s); got != "[ShipmentObligationReleased.v1/OB-1/1 ShipmentCancelled.v1]" {
		t.Fatalf("events %s", got)
	}
	if _, err := RestoreShipment(s.State()); err != nil {
		t.Fatalf("restore a cancelled shipment: %v", err)
	}
}

func TestRestoreShipmentChecksConsistency(t *testing.T) {
	s := created(t)
	if err := errors.Join(
		s.AddObligation(ref("OB-2", 6, 300), "planner-1", at.Add(time.Hour)),
		s.AmendPlan(route(at.Add(72*time.Hour)), "schedule", "planner-1", at.Add(time.Hour)),
		s.Confirm("planner-2", at.Add(2*time.Hour)),
	); err != nil {
		t.Fatal(err)
	}
	valid := s.State()
	restored, err := RestoreShipment(valid)
	if err != nil || !reflect.DeepEqual(restored.State(), valid) || len(restored.AddedPlans()) != 0 || len(restored.Pending()) != 0 {
		t.Fatalf("round trip: %v", err)
	}
	for name, edit := range map[string]func(*ShipmentState){
		"status behind the confirmation": func(st *ShipmentState) { st.Status = ShipmentDraft },
		"unknown status":                 func(st *ShipmentState) { st.Status = "LOADED" },
		"obligation of another customer": func(st *ShipmentState) { st.Obligations[1].CustomerPartyID = "Q" },
		"obligation planned twice":       func(st *ShipmentState) { st.Obligations[1].ObligationID = "OB-1" },
		"scope is not the sum":           func(st *ShipmentState) { st.Obligations[1].Packs = 7 },
		"release without who":            func(st *ShipmentState) { st.Obligations[1].ReleasedAt = at.Add(3 * time.Hour) },
		"plan numbering":                 func(st *ShipmentState) { st.Plans[1].No = 5 },
		"amendment without reason":       func(st *ShipmentState) { st.Plans[2].Reason = "" },
		"second plan marked created":     func(st *ShipmentState) { st.Plans[1].Change = PlanCreated },
		"confirmation of a partial route": func(st *ShipmentState) {
			st.Confirmations = append([]Confirmation{{PlanNo: 1, By: "x", At: at}}, st.Confirmations...)
		},
		"cancelled with a planned obligation": func(st *ShipmentState) {
			st.Status, st.CancelledBy, st.CancelledAt, st.CancelReason = ShipmentCancelled, "x", at.Add(3*time.Hour), "x"
		},
		"no obligations": func(st *ShipmentState) { st.Obligations = nil },
	} {
		st := s.State()
		edit(&st)
		if _, err := RestoreShipment(st); !errors.Is(err, ErrInvalidShipment) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
