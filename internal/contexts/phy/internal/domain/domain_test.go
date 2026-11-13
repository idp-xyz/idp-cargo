package domain

import (
	"errors"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

var t0 = time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)

func loc(code string) kernel.LocationCode {
	return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code}
}

var (
	pvgJFK = Route{Origin: loc("CNPVG"), Destination: loc("USJFK"), OriginIATA: "PVG", DestinationIATA: "JFK", LocationEdition: "E1"}
	pvgLAX = Route{Origin: loc("CNPVG"), Destination: loc("USLAX"), OriginIATA: "PVG", DestinationIATA: "LAX", LocationEdition: "E1"}
	shgHAM = Route{Origin: loc("CNSHG"), Destination: loc("DEHAM"), LocationEdition: "E1"}
)

func kg(n int64) kernel.Quantity { return kernel.Quantity{Unit: "KGM", Amount: n} }

func airConsol(t *testing.T, id ConsolidationID) *Consolidation {
	t.Helper()
	c, err := CreateConsolidation("T", id, ModeAir, KindAirConsol, "LE", pvgJFK, t0.Add(72*time.Hour), t0.Add(60*time.Hour), "ops-1", t0)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func intent(shipment ShipmentID, planVersion int) LoadIntent {
	return LoadIntent{ShipmentID: shipment, Revision: int64(planVersion), Status: IntentPlanned, PlanVersion: planVersion, LegalEntityID: "LE",
		Mode: ModeAir, Route: pvgJFK, DepartureWindow: kernel.ValidityInterval{From: t0.Add(48 * time.Hour), Until: t0.Add(96 * time.Hour)},
		Scope: CargoScope{Packs: 6, GrossWeight: kg(300)},
		Parts: []Part{{ObligationID: "OB-1", ObligationVersion: 1, OrderID: "O-1", Packs: 6, PackageType: "CT", GrossWeight: kg(300)}}}
}

func TestCreateConsolidationRules(t *testing.T) {
	departs, cut := t0.Add(72*time.Hour), t0.Add(60*time.Hour)
	for name, tc := range map[string]struct {
		mode   Mode
		kind   Kind
		route  Route
		dep    time.Time
		cut    time.Time
		want   error
		wantOK bool
	}{
		"air consol":               {ModeAir, KindAirConsol, pvgJFK, departs, cut, nil, true},
		"sea LCL own container":    {ModeSea, KindSeaLCLOwn, shgHAM, departs, cut, nil, true},
		"sea LCL co-load":          {ModeSea, KindSeaLCLCoload, shgHAM, departs, cut, nil, true},
		"air kind on sea":          {ModeSea, KindAirConsol, shgHAM, departs, cut, ErrInvalidConsolidation, false},
		"LCL kind on air":          {ModeAir, KindSeaLCLOwn, pvgJFK, departs, cut, ErrInvalidConsolidation, false},
		"air without IATA":         {ModeAir, KindAirConsol, Route{Origin: loc("CNPVG"), Destination: loc("USJFK"), LocationEdition: "E1"}, departs, cut, ErrInvalidRoute, false},
		"sea with IATA":            {ModeSea, KindSeaLCLOwn, pvgJFK, departs, cut, ErrInvalidRoute, false},
		"unresolved":               {ModeSea, KindSeaLCLOwn, Route{Origin: loc("CNSHG"), Destination: loc("DEHAM")}, departs, cut, ErrInvalidRoute, false},
		"departure in the past":    {ModeAir, KindAirConsol, pvgJFK, t0, t0, ErrInvalidSchedule, false},
		"cut-off after departure":  {ModeAir, KindAirConsol, pvgJFK, departs, departs.Add(time.Minute), ErrInvalidSchedule, false},
		"cut-off at the departure": {ModeAir, KindAirConsol, pvgJFK, departs, departs, nil, true},
	} {
		c, err := CreateConsolidation("T", "C-1", tc.mode, tc.kind, "LE", tc.route, tc.dep, tc.cut, "ops-1", t0)
		if tc.wantOK != (err == nil) || (tc.want != nil && !errors.Is(err, tc.want)) {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if err == nil && (c.Current().State != StateOpen || len(c.Pending()) != 1 || c.Pending()[0].Type != eventConsolidationCreated) {
			t.Errorf("%s: %+v %+v", name, c.Current(), c.Pending())
		}
	}
}

func TestConsolidationLifecycle(t *testing.T) {
	c := airConsol(t, "C-1")
	later := t0.Add(96 * time.Hour)
	if err := c.Amend(Route{}, later, later.Add(-6*time.Hour), "flight change", "ops-1", t0.Add(time.Hour), 3); err != nil {
		t.Fatalf("a flight change moves the consolidation with its houses: %v", err)
	}
	if err := c.Amend(pvgLAX, later, later.Add(-6*time.Hour), "other airport", "ops-1", t0.Add(time.Hour), 1); !errors.Is(err, ErrHousesAttached) {
		t.Fatalf("route change with houses: %v", err)
	}
	if err := c.Amend(Route{}, later, later.Add(-6*time.Hour), "same", "ops-1", t0.Add(time.Hour), 0); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("an amendment that changes nothing: %v", err)
	}
	if err := c.Amend(pvgLAX, later, later.Add(-6*time.Hour), "", "ops-1", t0.Add(time.Hour), 0); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("an amendment without a reason: %v", err)
	}
	if err := c.Close(0, time.Time{}, "ops-1", t0.Add(2*time.Hour)); !errors.Is(err, ErrNoHouses) {
		t.Fatalf("close without houses: %v", err)
	}
	if err := c.Close(2, t0.Add(3*time.Hour), "ops-1", t0.Add(2*time.Hour)); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("close before the last attach: %v", err)
	}
	if err := c.Close(2, t0.Add(time.Hour), "ops-1", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := c.Cancel("x", "ops-1", t0.Add(3*time.Hour), 0, 0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("cancel a closed consolidation: %v", err)
	}
	if err := c.Reopen("", "ops-1", t0.Add(3*time.Hour), 0, Stuffed{}); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("reopen without a reason: %v", err)
	}
	if err := c.Reopen("late booking", "ops-1", t0.Add(3*time.Hour), 0, Stuffed{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Cancel("no cargo", "ops-1", t0.Add(4*time.Hour), 1, 0); !errors.Is(err, ErrHousesAttached) {
		t.Fatalf("cancel with houses: %v", err)
	}
	if err := c.Cancel("no cargo", "ops-1", t0.Add(4*time.Hour), 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := c.Amend(Route{}, later.Add(time.Hour), later, "too late", "ops-1", t0.Add(5*time.Hour), 0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("amend a cancelled consolidation: %v", err)
	}
	var types []string
	for _, e := range c.Pending() {
		types = append(types, e.Type)
	}
	want := []string{eventConsolidationCreated, eventConsolidationAmended, eventConsolidationClosed, eventConsolidationReopened, eventConsolidationCancelled}
	if len(types) != len(want) {
		t.Fatalf("events %v", types)
	}
	for i := range want {
		if types[i] != want[i] || c.Pending()[i].Version != i+1 {
			t.Fatalf("events %v", c.Pending())
		}
	}
	st := c.State()
	if st.Versions[0].PlannedDeparture != t0.Add(72*time.Hour) || st.Versions[1].PlannedDeparture != later {
		t.Fatal("the original plan stays visible after the flight change")
	}
	if _, err := RestoreConsolidation(st); err != nil {
		t.Fatalf("restore: %v", err)
	}
	st.State = StateOpen
	if _, err := RestoreConsolidation(st); err == nil {
		t.Fatal("a state the versions do not make must not restore")
	}
}

func TestAttachRules(t *testing.T) {
	c := airConsol(t, "C-1")
	ok := intent("SH-1", 2)
	for name, tc := range map[string]struct {
		change func(*LoadIntent)
		want   error
	}{
		"draft":                {func(i *LoadIntent) { i.Status = IntentDraft }, ErrIntentNotPlanned},
		"sea shipment":         {func(i *LoadIntent) { i.Mode = ModeSea; i.Route = shgHAM }, ErrModeMismatch},
		"other destination":    {func(i *LoadIntent) { i.Route = pvgLAX }, ErrRouteMismatch},
		"window misses flight": {func(i *LoadIntent) { i.DepartureWindow.Until = t0.Add(70 * time.Hour) }, ErrWindowMismatch},
		"other legal entity":   {func(i *LoadIntent) { i.LegalEntityID = "LE-2" }, ErrOtherLegalEntity},
		"parts not the scope":  {func(i *LoadIntent) { i.Scope.Packs = 7 }, ErrInvalidIntent},
		"no package type": {func(i *LoadIntent) {
			i.Parts = []Part{{ObligationID: "OB-1", ObligationVersion: 1, OrderID: "O-1", Packs: 6, GrossWeight: kg(300)}}
		}, ErrInvalidIntent},
	} {
		i := ok
		i.Parts = append([]Part(nil), ok.Parts...)
		tc.change(&i)
		m, _ := NewMembership("T", "SH-1")
		if err := m.Attach(c, i, "ops-1", t0.Add(time.Hour)); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// N1-PHY OD-9: an LCL consolidation, own container or co-load, takes LCL shipments and those whose container mode
	// is not planned, no FCL one
	for _, kind := range []Kind{KindSeaLCLOwn, KindSeaLCLCoload} {
		sea, err := CreateConsolidation("T", "C-S", ModeSea, kind, "LE", shgHAM, t0.Add(72*time.Hour), t0.Add(60*time.Hour), "ops", t0)
		if err != nil {
			t.Fatal(err)
		}
		for mode, want := range map[string]error{"FCL": ErrFCLHouse, "LCL": nil, "": nil} {
			i := ok
			i.ShipmentID, i.Mode, i.Route, i.ContainerMode = "SH-S", ModeSea, shgHAM, mode
			sm, _ := NewMembership("T", "SH-S")
			if err := sm.Attach(sea, i, "ops-1", t0.Add(time.Hour)); !errors.Is(err, want) {
				t.Errorf("%s, a %q sea shipment: %v", kind, mode, err)
			}
		}
	}
	m, _ := NewMembership("T", "SH-1")
	if err := m.Attach(c, ok, "ops-1", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.Attach(airConsol(t, "C-2"), ok, "ops-1", t0.Add(time.Hour)); !errors.Is(err, ErrAlreadyAttached) {
		t.Fatalf("a second consolidation for the main carriage: %v", err)
	}
	if err := c.Close(1, t0.Add(time.Hour), "ops-1", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	other, _ := NewMembership("T", "SH-2")
	if err := other.Attach(c, intent("SH-2", 1), "ops-1", t0.Add(3*time.Hour)); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("attach after the house cut-off: %v", err)
	}
	if err := m.Detach(c, ReasonCustomerRequest, "", "ops-1", t0.Add(3*time.Hour)); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("detach after the house cut-off: %v", err)
	}
	if err := m.EndForCancelledShipment(c, intent("SH-1", 2), "ops-1", t0.Add(3*time.Hour)); !errors.Is(err, ErrShipmentNotCancelled) {
		t.Fatalf("end for a shipment FUL still plans: %v", err)
	}
	cancelled := intent("SH-1", 2)
	cancelled.Status = IntentCancelled
	if err := m.EndForCancelledShipment(c, cancelled, "ops-1", t0.Add(3*time.Hour)); err != nil {
		t.Fatalf("a cancelled shipment leaves a closed consolidation: %v", err)
	}
}

func TestTransferRefreshAndHistory(t *testing.T) {
	a, b := airConsol(t, "C-A"), airConsol(t, "C-B")
	m, _ := NewMembership("T", "SH-1")
	if err := m.Attach(a, intent("SH-1", 2), "ops-1", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.Transfer(a, a, intent("SH-1", 2), ReasonRollover, "", "ops-1", t0.Add(2*time.Hour)); !errors.Is(err, ErrSameConsolidation) {
		t.Fatalf("transfer to the same consolidation: %v", err)
	}
	if err := m.Transfer(a, b, intent("SH-1", 2), "LATE", "", "ops-1", t0.Add(2*time.Hour)); !errors.Is(err, ErrInvalidReason) {
		t.Fatalf("unknown reason: %v", err)
	}
	if err := m.Transfer(a, b, intent("SH-1", 2), ReasonOther, "", "ops-1", t0.Add(2*time.Hour)); !errors.Is(err, ErrInvalidReason) {
		t.Fatalf("OTHER without a text: %v", err)
	}
	if err := m.Transfer(a, b, intent("SH-1", 2), ReasonRollover, "", "ops-1", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.Detach(a, ReasonCustomerRequest, "", "ops-1", t0.Add(3*time.Hour)); !errors.Is(err, ErrNotAttached) {
		t.Fatalf("detach from the consolidation the house left: %v", err)
	}
	if err := m.Refresh(b, intent("SH-1", 2), "ops-1", t0.Add(3*time.Hour)); !errors.Is(err, ErrNothingToRefresh) {
		t.Fatalf("refresh at the same plan version: %v", err)
	}
	shortload := intent("SH-1", 3)
	shortload.Scope, shortload.Parts[0].Packs, shortload.Parts[0].GrossWeight = CargoScope{Packs: 4, GrossWeight: kg(200)}, 4, kg(200)
	if err := m.Refresh(b, shortload, "ops-1", t0.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.Detach(b, ReasonOther, "customer moved the cargo to sea", "ops-1", t0.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, live := m.Live(); live {
		t.Fatal("no live entry after detach")
	}
	st := m.State()
	if len(st.Entries) != 3 || st.Entries[0].End != EndTransferred || st.Entries[0].Successor != 2 || st.Entries[1].Start != StartTransferred ||
		st.Entries[1].End != EndRefreshed || st.Entries[2].Start != StartRefreshed || st.Entries[2].Scope.Packs != 4 || st.Entries[2].End != EndDetached {
		t.Fatalf("history: %+v", st.Entries)
	}
	var types []string
	for _, e := range m.Pending() {
		types = append(types, e.Type)
	}
	if len(types) != 4 || types[0] != eventHouseAttached || types[1] != eventHouseTransferred || types[2] != eventHouseIntentRefreshed ||
		types[3] != eventHouseDetached || m.Pending()[1].FromConsolidationID != "C-A" || m.Pending()[1].ConsolidationID != "C-B" {
		t.Fatalf("events %+v", m.Pending())
	}
	if _, err := RestoreMembership(st); err != nil {
		t.Fatalf("restore: %v", err)
	}
	st.Entries[2].End, st.Entries[2].Reason, st.Entries[2].ReasonText, st.Entries[2].EndedBy, st.Entries[2].EndedAt = "", "", "", "", time.Time{}
	st.Entries[1].End = ""
	if _, err := RestoreMembership(st); err == nil {
		t.Fatal("two live entries must not restore")
	}
}

func TestCloseConfirmsMembersWithFUL(t *testing.T) {
	c := airConsol(t, "C-1")
	m, _ := NewMembership("T", "SH-1")
	if err := m.Attach(c, intent("SH-1", 2), "ops-1", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	live, _ := m.Live()
	now := intent("SH-1", 2)
	if err := live.Confirm(now); !errors.Is(err, ErrMemberNotAllocated) {
		t.Fatalf("FUL has not recorded the attachment: %v", err)
	}
	now.ConsolidationID, now.Revision = "C-1", 3
	if err := live.Confirm(now); err != nil {
		t.Fatalf("recording the attachment moves FUL's revision, not its plan: %v", err)
	}
	now.PlanVersion = 3
	if err := live.Confirm(now); !errors.Is(err, ErrMemberStale) {
		t.Fatalf("a changed plan: %v", err)
	}
	now.PlanVersion, now.Status = 2, IntentDraft
	if err := live.Confirm(now); !errors.Is(err, ErrMemberStale) {
		t.Fatalf("a plan that needs confirming again: %v", err)
	}
}
