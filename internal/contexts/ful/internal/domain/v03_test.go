package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

func airRoute(from time.Time) Route {
	return Route{Mode: ModeAir, Origin: loc("CNPVG"), Destination: loc("USJFK"), Departure: kernel.ValidityInterval{From: from, Until: from.Add(24 * time.Hour)},
		OriginIATA: "PVG", DestinationIATA: "JFK", LocationEdition: testEdition}
}

// F2, option 4: a plan version freezes resolved locations; an AIR route carries both IATA airport codes.
func TestRoutesAreResolvedBeforeTheyArePlanned(t *testing.T) {
	s := created(t)
	s.Acknowledge()
	later := at.Add(time.Hour)
	unresolved := route(at.Add(72 * time.Hour))
	unresolved.LocationEdition = ""
	if err := s.AmendPlan(unresolved, "schedule", "planner-1", later); !errors.Is(err, ErrUnresolvedRoute) {
		t.Fatalf("a route without an edition: %v", err)
	}
	noIATA := airRoute(at.Add(72 * time.Hour))
	noIATA.DestinationIATA = ""
	if err := s.AmendPlan(noIATA, "schedule", "planner-1", later); !errors.Is(err, ErrUnresolvedRoute) {
		t.Fatalf("an AIR route without the destination's IATA code: %v", err)
	}
	seaWithIATA := route(at.Add(72 * time.Hour))
	seaWithIATA.OriginIATA = "SHA"
	if err := s.AmendPlan(seaWithIATA, "schedule", "planner-1", later); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("an IATA code on a SEA route: %v", err)
	}
	if err := s.AmendPlan(airRoute(at.Add(72*time.Hour)), "air freight", "planner-1", later); err != nil {
		t.Fatal(err)
	}
	if err := s.Confirm("planner-2", later); err != nil {
		t.Fatal(err)
	}
	if r := s.Latest().Route; r.OriginIATA != "PVG" || r.LocationEdition != testEdition {
		t.Fatalf("frozen route %+v", r)
	}
}

// A route stored before FUL V0.3 restores, but a plan is confirmed only once its route is resolved.
func TestUnresolvedStoredRoutesMustBeResolvedBeforeConfirmation(t *testing.T) {
	s := created(t)
	st := s.State()
	old := route(at.Add(72 * time.Hour))
	old.LocationEdition = ""
	st.Plans = append(st.Plans, PlanVersion{No: 2, Change: PlanAmended, Route: old, Scope: st.Plans[0].Scope, Reason: "schedule", By: "planner-1", At: at})
	restored, err := RestoreShipment(st)
	if err != nil {
		t.Fatalf("restore a V0.2 route: %v", err)
	}
	if err = restored.Confirm("planner-2", at.Add(time.Hour)); !errors.Is(err, ErrUnresolvedRoute) {
		t.Fatalf("confirm an unresolved route: %v", err)
	}
}

// C11: a shipment plans an obligation only in the mode its order requests.
func TestRequestedModesOfTheOrders(t *testing.T) {
	s := created(t)
	s.Acknowledge()
	later := at.Add(time.Hour)
	if err := s.AmendPlan(route(at.Add(72*time.Hour)), "sea freight", "planner-1", later); err != nil {
		t.Fatal(err)
	}
	airOrder := ref("OB-2", 1, 2)
	airOrder.RequestedMode = ModeAir
	if err := s.AddObligation(airOrder, "planner-1", later); !errors.Is(err, ErrModeMismatch) {
		t.Fatalf("an AIR order into a SEA shipment: %v", err)
	}
	seaOrder := ref("OB-2", 1, 2)
	seaOrder.RequestedMode = ModeSea
	if err := s.AddObligation(seaOrder, "planner-1", later); err != nil {
		t.Fatalf("a SEA order: %v", err)
	}
	anyOrder := ref("OB-3", 1, 2)
	if err := s.AddObligation(anyOrder, "planner-1", later); err != nil {
		t.Fatalf("an order without a requested mode: %v", err)
	}
	if err := s.CheckRequestedModes([]ObligationRef{seaOrder, airOrder}); !errors.Is(err, ErrModeMismatch) {
		t.Fatalf("check fresh references: %v", err)
	}
	invalid := ref("OB-4", 1, 2)
	invalid.RequestedMode = "BARGE"
	if err := s.AddObligation(invalid, "planner-1", later); !errors.Is(err, ErrInvalidObligation) {
		t.Fatalf("an unknown requested mode: %v", err)
	}
}

// T1: Stamp records when FUL entered what a command added, and only that.
func TestStampRecordsTheEntryTime(t *testing.T) {
	s := created(t)
	entered := at.Add(5 * time.Minute)
	s.Stamp(entered)
	if got := s.State().Plans[0].EnteredAt; !got.Equal(entered) {
		t.Fatalf("plan 1 entered at %v", got)
	}
	s.Acknowledge()
	if err := s.AmendPlan(route(at.Add(72*time.Hour)), "schedule", "planner-1", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	s.Stamp(at.Add(2 * time.Hour))
	plans := s.State().Plans
	if !plans[0].EnteredAt.Equal(entered) || !plans[1].EnteredAt.Equal(at.Add(2*time.Hour)) {
		t.Fatalf("entered times %v %v", plans[0].EnteredAt, plans[1].EnteredAt)
	}
}

// N1-PHY OD-9: a shipment plans a container mode of its transport mode, and plans an obligation only in the
// container mode its order requests.
func TestRequestedContainerModes(t *testing.T) {
	s := created(t)
	s.Acknowledge()
	later := at.Add(time.Hour)
	fcl := route(at.Add(72 * time.Hour))
	fcl.ContainerMode = ContainerLoose
	if err := s.AmendPlan(fcl, "loose on sea", "planner-1", later); err == nil {
		t.Fatal("LOOSE planned on a SEA route")
	}
	lcl := route(at.Add(72 * time.Hour))
	lcl.ContainerMode = ContainerLCL
	if err := s.AmendPlan(lcl, "sea LCL", "planner-1", later); err != nil {
		t.Fatal(err)
	}
	fclOrder := ref("OB-2", 1, 2)
	fclOrder.RequestedMode, fclOrder.RequestedContainerMode = ModeSea, ContainerFCL
	if err := s.AddObligation(fclOrder, "planner-1", later); !errors.Is(err, ErrModeMismatch) {
		t.Fatalf("an FCL order into an LCL shipment: %v", err)
	}
	lclOrder := ref("OB-2", 1, 2)
	lclOrder.RequestedMode, lclOrder.RequestedContainerMode = ModeSea, ContainerLCL
	if err := s.AddObligation(lclOrder, "planner-1", later); err != nil {
		t.Fatalf("an LCL order: %v", err)
	}
	if err := s.AddObligation(ref("OB-3", 1, 2), "planner-1", later); err != nil {
		t.Fatalf("an order requesting no container mode: %v", err)
	}
	if err := s.CheckRequestedModes([]ObligationRef{lclOrder, fclOrder}); !errors.Is(err, ErrModeMismatch) {
		t.Fatalf("check fresh references: %v", err)
	}
	noMode := ref("OB-4", 1, 2)
	noMode.RequestedContainerMode = ContainerLCL
	if err := s.AddObligation(noMode, "planner-1", later); !errors.Is(err, ErrInvalidObligation) {
		t.Fatalf("a container mode without a mode: %v", err)
	}
	if got := s.Latest().Route.ContainerMode; got != ContainerLCL {
		t.Fatalf("a plan version added with a part keeps the container mode: %q", got)
	}
}

// Commands recorded before the container mode hash as they did: an empty one is left out of the route's JSON; and a
// route differs from another by its container mode alone.
func TestRouteContainerModeHashAndSame(t *testing.T) {
	r := route(at.Add(72 * time.Hour))
	blob, err := json.Marshal(r)
	if err != nil || strings.Contains(string(blob), "ContainerMode") {
		t.Fatalf("%s %v", blob, err)
	}
	lcl := r
	lcl.ContainerMode = ContainerLCL
	if r.Same(lcl) || !lcl.Same(lcl) {
		t.Fatal("a route differs by its container mode alone")
	}
}
