package domain

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// planScript is a random run of commands against one shipment: additions from a pool of obligations
// (some of another customer or in other units), releases, amendments with complete, partial, repeated or
// invalid routes, confirmations and, rarely, a cancellation.
type planScript struct {
	Steps []planStep
}

type planStep struct {
	Kind    int // 0-2 add, 3-4 release, 5-6 amend, 7-8 confirm, 9 cancel
	Pick    int
	Version int64
	Route   int
}

func (planScript) Generate(r *rand.Rand, size int) reflect.Value {
	var s planScript
	for n := 1 + r.Intn(3*size); len(s.Steps) < n; {
		st := planStep{Kind: r.Intn(9), Pick: r.Intn(len(pool)), Version: 1 + int64(r.Intn(4)/3), Route: r.Intn(len(routes))}
		if r.Intn(60) == 0 {
			st.Kind = 9
		}
		s.Steps = append(s.Steps, st)
	}
	return reflect.ValueOf(s)
}

// pool holds obligation versions to add: most fit the shipment created from OB-0, some do not.
var pool = func() []ObligationRef {
	var p []ObligationRef
	for i := 0; i < 8; i++ {
		p = append(p, ref(fmt.Sprintf("OB-%d", i), int64(1+i), int64(50*(i+1))))
	}
	other := ref("OB-Q", 1, 10)
	other.CustomerPartyID = "Q"
	pounds := ref("OB-L", 1, 10)
	pounds.GrossWeight = kernel.Quantity{Unit: "LBR", Amount: 22}
	volume := ref("OB-V", 1, 10)
	volume.Volume = dm3(30)
	return append(p, other, pounds, volume)
}()

var routes = []Route{
	{},
	{Mode: ModeSea, Origin: loc("CNSHA")},
	route(at.Add(72 * time.Hour)),
	route(at.Add(240 * time.Hour)),
	{Mode: ModeAir, Origin: loc("CNSHA"), Destination: loc("DEHAM"), Departure: kernel.ValidityInterval{From: at, Until: at.Add(time.Hour)}},
	{Origin: loc("CNSHA"), Destination: loc("CNSHA")},
}

// TestPlanProperties checks, for random scripts, that the shipment accepts exactly what an independent
// model accepts, that a refused command changes nothing, that plan versions are never rewritten, that one
// customer and legal entity hold, that the latest scope is the sum of the planned obligations, and that
// a stored shipment restores to the same state.
func TestPlanProperties(t *testing.T) {
	property := func(script planScript) bool {
		s, err := CreateShipment("T", "SH-1", pool[0], "planner-1", at)
		if err != nil {
			t.Log(err)
			return false
		}
		status, planned, current := ShipmentDraft, map[ObligationID]bool{"OB-0": true}, Route{}
		for i, st := range script.Steps {
			when := at.Add(time.Duration(i+1) * time.Minute)
			before, events := s.State(), len(s.Pending())
			ob, r := pool[st.Pick], routes[st.Route]
			var ok bool
			var apply func()
			switch {
			case st.Kind <= 2:
				ok = status == ShipmentDraft && ob.CustomerPartyID == "P" && ob.GrossWeight.Unit == "KGM" && ob.Volume == (kernel.Quantity{}) &&
					!planned[ob.ObligationID]
				err = s.AddObligation(ob, "planner-1", when)
				apply = func() { planned[ob.ObligationID] = true }
			case st.Kind <= 4:
				ok = status != ShipmentCancelled && planned[ob.ObligationID] && st.Version == 1 && len(planned) > 1
				err = s.ReleaseObligation(ob.ObligationID, st.Version, "planner-1", when)
				apply = func() { delete(planned, ob.ObligationID); status = ShipmentDraft }
			case st.Kind <= 6:
				ok = status != ShipmentCancelled && r.valid() && !r.Same(current)
				err = s.AmendPlan(r, "re-planned", "planner-1", when)
				apply = func() { current, status = r, ShipmentDraft }
			case st.Kind <= 8:
				ok = status == ShipmentDraft && current.Complete()
				err = s.Confirm("planner-2", when)
				apply = func() { status = ShipmentPlanned }
			default:
				ok = status != ShipmentCancelled
				err = s.Cancel("withdrawn", "planner-1", when)
				apply = func() { status, planned = ShipmentCancelled, map[ObligationID]bool{} }
			}
			if ok != (err == nil) {
				t.Logf("step %d %+v: model accepts %v, shipment says %v", i, st, ok, err)
				return false
			}
			if err != nil {
				if !reflect.DeepEqual(s.State(), before) || len(s.Pending()) != events {
					t.Logf("step %d %+v: refused, but the shipment changed", i, st)
					return false
				}
				continue
			}
			apply()
			after := s.State()
			if !reflect.DeepEqual(after.Plans[:len(before.Plans)], before.Plans) || len(s.Pending()) != events+1 {
				t.Logf("step %d %+v: an earlier plan version changed, or the events are off", i, st)
				return false
			}
		}
		st := s.State()
		got := map[ObligationID]bool{}
		for _, ob := range s.Planned() {
			got[ob.ObligationID] = true
			if ob.CustomerPartyID != "P" || ob.LegalEntityID != "LE" {
				return false
			}
		}
		if st.Status != status || !reflect.DeepEqual(got, planned) || !s.Latest().Route.Same(current) {
			t.Logf("status %s (model %s), planned %v (model %v)", st.Status, status, got, planned)
			return false
		}
		if status != ShipmentCancelled {
			var wholes []CargoScope
			for _, ob := range s.Planned() {
				wholes = append(wholes, ob.Whole())
			}
			if scope, err := scopeOf(wholes); err != nil || scope != s.Latest().Scope {
				return false
			}
		}
		restored, err := RestoreShipment(st)
		return err == nil && reflect.DeepEqual(restored.State(), st)
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 500}); err != nil {
		t.Fatal(err)
	}
}
