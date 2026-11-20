package domain

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Opening a sealed container for rework, unloading a whole house, sealing again and giving up (V2.85).

// yard is a CLOSED own-container consolidation with two planned containers and the houses SH-1, SH-2 and SH-3 attached,
// each with its lot (10 cartons of 3,000 kg received at the CFS, weighed and completed). Container 1 (X-1) is loading.
type yard struct {
	c      *Consolidation
	x      *LoadExecution
	entry  map[ShipmentID]Entry
	lot    map[ShipmentID]*CargoLot
	intent map[ShipmentID]LoadIntent
}

func seaHouse(id ShipmentID) LoadIntent {
	i := seaIntent(3000)
	i.ShipmentID = id
	return i
}

func newYard(t *testing.T) *yard {
	t.Helper()
	y := &yard{c: seaConsol(t, "C-S", KindSeaLCLOwn), entry: map[ShipmentID]Entry{}, lot: map[ShipmentID]*CargoLot{}, intent: map[ShipmentID]LoadIntent{}}
	for range 2 {
		if _, err := y.c.PlanLoadUnit(UnitContainer, unit22G1, "ops", t0.Add(30*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	var scopes []CargoScope
	for n, id := range []ShipmentID{"SH-1", "SH-2", "SH-3"} {
		i := seaHouse(id)
		m, _ := NewMembership("T", id)
		if err := m.Attach(y.c, i, "ops", t0.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		y.entry[id], _ = m.Live()
		y.intent[id] = i
		scopes = append(scopes, i.Scope)
		lot := y.receive(t, id, CargoLotID(fmt.Sprintf("L-%d", n+1)), cfs, 10, i)
		y.lot[id] = lot
	}
	if err := y.c.Close(scopes, t0.Add(time.Hour), "", "ops", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var err error
	if y.x, err = OpenStuffing("T", "X-1", y.c, 1, cfs, false, "ops", t0.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err = y.x.Start("ops", t0.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	return y
}

// receive receives packs of the house at the facility in a lot of its own, weighed at 300 kg a pack and completed
// against the plan given (a lot of part of the house is completed against a plan of its own size).
func (y *yard) receive(t *testing.T, id ShipmentID, lotID CargoLotID, f Facility, packs int64, plan LoadIntent) *CargoLot {
	t.Helper()
	plan.Scope.Packs = packs
	plan.Parts = []Part{{ObligationID: "OB-1", ObligationVersion: 1, OrderID: "O-1", Packs: packs, PackageType: "CT", GrossWeight: kg(300 * packs)}}
	plan.Scope.GrossWeight = kg(300 * packs)
	lot, err := ReceiveCargo("T", lotID, f, plan, cartons(packs, ConditionGood, t0.Add(time.Hour)), "ops", t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(lot.Measure(scale(300*packs, t0.Add(2*time.Hour)), "ops", t0.Add(2*time.Hour)), lot.Complete(plan, "ops", t0.Add(2*time.Hour))); err != nil {
		t.Fatal(err)
	}
	return lot
}

// stuffWhole loads all of a lot into the stuffing.
func (y *yard) stuffWhole(t *testing.T, x *LoadExecution, id ShipmentID, lot *CargoLot, at time.Time) {
	t.Helper()
	n := lot.Received()
	loaded := x.LiveFor(id)
	if err := stuff(x, y.c, LoadLine{Packs: n, Evidence: ev("TALLY-"+string(lot.ID()), at)}, lot, y.entry[id], LotLoad{Packs: x.LiveFromLot(lot.ID())}, loaded, at); err != nil {
		t.Fatal(err)
	}
}

// sealed returns the yard with SH-1 and SH-2 in X-1, the container sealed with SEAL-1 at +4h, and SH-2's lot closed at
// +4h.
func sealedYard(t *testing.T) *yard {
	t.Helper()
	y := newYard(t)
	at := t0.Add(4 * time.Hour)
	y.stuffWhole(t, y.x, "SH-1", y.lot["SH-1"], at)
	y.stuffWhole(t, y.x, "SH-2", y.lot["SH-2"], at)
	if err := y.x.Seal(y.c, seal("SEAL-1", at), "ops-seal", at); err != nil {
		t.Fatal(err)
	}
	if err := y.lot["SH-2"].Close(10, 10, y.intent["SH-2"], "ops", at); err != nil {
		t.Fatal(err)
	}
	y.x.Stamp(at.Add(time.Minute))
	y.lot["SH-2"].Stamp(at.Add(time.Minute))
	return y
}

var (
	reworkAt = t0.Add(5 * time.Hour)
	unloadAt = t0.Add(6 * time.Hour)
	resealAt = t0.Add(7 * time.Hour)
)

// reworked is sealedYard opened for rework by ops-asker.
func reworked(t *testing.T) *yard {
	t.Helper()
	y := sealedYard(t)
	if err := y.x.OpenForRework(y.c, "the consignee asked to drop a house", "ops-asker", reworkAt); err != nil {
		t.Fatal(err)
	}
	return y
}

func (y *yard) unload(t *testing.T, id ShipmentID) int {
	t.Helper()
	n, err := y.x.UnloadHouse(id, map[CargoLotID]*CargoLot{y.lot[id].ID(): y.lot[id]}, 0, ev("WH-CONFIRM-"+string(id), unloadAt), "ops-wh", unloadAt)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestOpenForReworkBreaksTheSeal(t *testing.T) {
	y := sealedYard(t)
	was := len(y.x.Pending())
	if err := y.x.OpenForRework(y.c, "the consignee asked to drop a house", "ops-asker", reworkAt); err != nil {
		t.Fatal(err)
	}
	st := y.x.State()
	if y.x.Current() != ExecutionRework || !st.ClosedAt.IsZero() || st.ClosedBy != "" || st.CloseEvidence != (Evidence{}) || !st.ClosedEnteredAt.IsZero() {
		t.Fatalf("a container in rework carries no close facts: %s %+v", y.x.Current(), st)
	}
	if st.Kind != ExecutionStuffing || st.LoadUnitNo != 1 || st.ExecutionID != "X-1" || st.OpenedBy != "ops" || st.StartedBy != "ops" {
		t.Fatalf("it is the same execution: %+v", st)
	}
	if len(st.Reworks) != 1 {
		t.Fatalf("reworks = %+v", st.Reworks)
	}
	r := st.Reworks[0]
	if r.No != 1 || r.AskedBy != "ops-asker" || !r.AskedAt.Equal(reworkAt) || r.Reason != "the consignee asked to drop a house" || !r.Open() ||
		r.BrokenSeal.Source != EvidenceSeal || r.BrokenSeal.Reference != "SEAL-1" || r.BrokenSealedBy != "ops-seal" || !r.BrokenSealedAt.Equal(t0.Add(4*time.Hour)) ||
		!r.BrokenSealedEnteredAt.Equal(t0.Add(4*time.Hour+time.Minute)) || r.EndedBy != "" || !r.EndedAt.IsZero() || r.NewSeal != "" {
		t.Fatalf("the rework's record: %+v", r)
	}
	events := y.x.Pending()
	if len(events) != was+1 || events[was].Type != "LoadExecutionReworkOpened.v1" || events[was].ItemNo != 1 || events[was].ExecutionID != "X-1" ||
		events[was].ConsolidationID != "C-S" || !events[was].At.Equal(reworkAt) || events[was].Reason != "" || events[was].SubjectID != "X-1" {
		t.Fatalf("events = %+v", events)
	}
	y.x.Stamp(reworkAt.Add(time.Minute))
	if got := y.x.State().Reworks[0].AskedEnteredAt; !got.Equal(reworkAt.Add(time.Minute)) {
		t.Fatalf("entered at %v", got)
	}
	if got := y.x.Added().Reworks; len(got) != 1 || got[0].No != 1 {
		t.Fatalf("what a save writes: %+v", got)
	}
	back, err := RestoreLoadExecution(y.x.State())
	if err != nil || back.Current() != ExecutionRework || len(back.Added().Reworks) != 0 {
		t.Fatalf("a container in rework restores: %v", err)
	}
	if y.x.Tendered() != 20 || y.x.LiveFor("SH-1") != 10 {
		t.Fatalf("opening it moves no pack: %d", y.x.Tendered())
	}
}

func TestOpenForReworkRefusals(t *testing.T) {
	long := strings.Repeat("r", 501)
	type tc struct {
		build  func(t *testing.T) (*yard, *Consolidation)
		reason string
		by     string
		at     time.Time
		want   error
		text   string
	}
	sealed := func(t *testing.T) (*yard, *Consolidation) { y := sealedYard(t); return y, y.c }
	inState := func(state ExecutionState) func(t *testing.T) (*yard, *Consolidation) {
		return func(t *testing.T) (*yard, *Consolidation) {
			y := newYard(t)
			switch state {
			case ExecutionOpen:
				var err error
				if y.x, err = OpenStuffing("T", "X-2", y.c, 2, cfs, false, "ops", t0.Add(3*time.Hour)); err != nil {
					t.Fatal(err)
				}
			case ExecutionOnHold:
				if err := y.x.PlaceHold(HoldCustoms, "", ev("CUSTOMS-1", t0.Add(4*time.Hour)), "ops", t0.Add(4*time.Hour)); err != nil {
					t.Fatal(err)
				}
			case ExecutionCancelled:
				if err := y.x.Cancel("rolled", "ops", t0.Add(4*time.Hour)); err != nil {
					t.Fatal(err)
				}
			case ExecutionRework:
				y = reworked(t)
			}
			return y, y.c
		}
	}
	carrier := func(fact CarrierFact, pieces int64) func(t *testing.T) (*yard, *Consolidation) {
		return func(t *testing.T) (*yard, *Consolidation) {
			y := sealedYard(t)
			e := CarrierEvidence{Fact: fact, Pieces: pieces, Transport: "MV-EVER 12", Evidence: ev("CARRIER-1", t0.Add(4*time.Hour+30*time.Minute))}
			if _, _, err := y.x.RecordCarrierEvidence(e, "ops", t0.Add(4*time.Hour+30*time.Minute)); err != nil {
				t.Fatal(err)
			}
			return y, y.c
		}
	}
	tender := func(t *testing.T) (*yard, *Consolidation) {
		c, entry, x := tender(t)
		lot := received(t)
		at := t0.Add(5 * time.Hour)
		if err := errors.Join(x.Load(LoadLine{Packs: 6, Evidence: ev("TALLY", at)}, c, lot, entry, LotLoad{}, 0, "ops", at), x.Close(ev("RECEIPT", at), "ops", at)); err != nil {
			t.Fatal(err)
		}
		return &yard{c: c, x: x}, c
	}
	otherOpen := func(t *testing.T) (*yard, *Consolidation) {
		y := sealedYard(t)
		return y, seaConsol(t, "C-S", KindSeaLCLOwn) // the same consolidation id, OPEN
	}
	elsewhere := func(t *testing.T) (*yard, *Consolidation) {
		y := sealedYard(t)
		return y, seaConsol(t, "C-OTHER", KindSeaLCLOwn)
	}
	reconciled := func(t *testing.T) (*yard, *Consolidation) {
		y := newYard(t)
		at := t0.Add(4 * time.Hour)
		var members []MemberCheck
		for _, id := range []ShipmentID{"SH-1", "SH-2", "SH-3"} {
			y.stuffWhole(t, y.x, id, y.lot[id], at)
			i := y.intent[id]
			i.ConsolidationID = "C-S"
			members = append(members, MemberCheck{Entry: y.entry[id], Intent: i, Loaded: 10})
		}
		if err := errors.Join(y.x.Seal(y.c, seal("SEAL-1", at), "ops", at), y.c.Reconcile(members, 0, 0, "ops", at.Add(10*time.Minute))); err != nil {
			t.Fatal(err)
		}
		return y, y.c
	}
	cases := map[string]tc{
		"a stuffing that is OPEN":       {inState(ExecutionOpen), "r", "ops", reworkAt, ErrInvalidTransition, "only a sealed container (CLOSED) is opened for rework, this one is OPEN"},
		"a stuffing that is LOADING":    {inState(ExecutionLoading), "r", "ops", reworkAt, ErrInvalidTransition, "this one is LOADING"},
		"a stuffing that is ON_HOLD":    {inState(ExecutionOnHold), "r", "ops", reworkAt, ErrInvalidTransition, "this one is ON_HOLD"},
		"a stuffing that is CANCELLED":  {inState(ExecutionCancelled), "r", "ops", reworkAt, ErrInvalidTransition, "this one is CANCELLED"},
		"a container already in REWORK": {inState(ExecutionRework), "r", "ops", resealAt, ErrInvalidTransition, "this one is REWORK"},
		"a tender":                      {tender, "r", "ops", t0.Add(6 * time.Hour), ErrInvalidExecution, "only a stuffing is opened for rework"},
		"another consolidation":         {elsewhere, "r", "ops", reworkAt, ErrInvalidExecution, "with its consolidation"},
		"an OPEN consolidation":         {otherOpen, "r", "ops", reworkAt, ErrNotFrozen, "it is OPEN"},
		"a consolidation reconciled":    {reconciled, "r", "ops", t0.Add(5 * time.Hour), ErrInvalidTransition, "the consolidation is RECONCILED"},
		"carrier evidence recorded":     {carrier(CarrierDeparted, 20), "r", "ops", reworkAt, ErrInvalidTransition, "carrier evidence is recorded on the container"},
		"a loading discrepancy open":    {carrier(CarrierOffloaded, 3), "r", "ops", reworkAt, ErrInvalidTransition, "a loading discrepancy is open"},
		"no reason":                     {sealed, " ", "ops", reworkAt, ErrInvalidExecution, "needs a reason"},
		"a reason of 501 characters":    {sealed, long, "ops", reworkAt, ErrInvalidExecution, "needs a reason"},
		"no actor":                      {sealed, "r", " ", reworkAt, ErrInvalidChange, "author and business time"},
		"before the seal":               {sealed, "r", "ops", t0.Add(4*time.Hour - time.Second), ErrInvalidChange, "not before the execution's last change"},
		"no time":                       {sealed, "r", "ops", time.Time{}, ErrInvalidChange, "author and business time"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			y, consol := c.build(t)
			was, state, reworks := len(y.x.Pending()), y.x.State(), len(y.x.State().Reworks)
			err := y.x.OpenForRework(consol, c.reason, c.by, c.at)
			if !errors.Is(err, c.want) || !strings.Contains(err.Error(), c.text) {
				t.Fatalf("got %v, want %v containing %q", err, c.want, c.text)
			}
			if len(y.x.Pending()) != was || y.x.Current() != state.State || len(y.x.State().Reworks) != reworks || y.x.State().ClosedAt != state.ClosedAt {
				t.Fatalf("a refused command changes nothing: %s, %d events", y.x.Current(), len(y.x.Pending()))
			}
		})
	}
}

func TestUnloadHouseReopensItsClosedLotsAndReversesEveryLine(t *testing.T) {
	// SH-2 has two lots in the container: 6 cartons at the CFS and 4 at another facility; both are closed, as is its
	// house plan (10 packs) loaded
	z := newYard(t)
	at := t0.Add(4 * time.Hour)
	first := z.receive(t, "SH-2", "L-2a", cfs, 6, z.intent["SH-2"])
	second := z.receive(t, "SH-2", "L-2b", Facility{Location: loc("CNSHG"), Code: "CFS-2"}, 4, z.intent["SH-2"])
	z.stuffWhole(t, z.x, "SH-2", first, at)
	z.stuffWhole(t, z.x, "SH-2", second, at)
	z.stuffWhole(t, z.x, "SH-1", z.lot["SH-1"], at)
	if err := z.x.Seal(z.c, seal("SEAL-1", at), "ops-seal", at); err != nil {
		t.Fatal(err)
	}
	for _, l := range []*CargoLot{first, second} {
		if err := l.Close(l.Received(), 10, z.intent["SH-2"], "ops-lot", at); err != nil {
			t.Fatal(err)
		}
		l.Stamp(at.Add(time.Minute))
	}
	if err := z.x.OpenForRework(z.c, "drop SH-2", "ops-asker", reworkAt); err != nil {
		t.Fatal(err)
	}
	z.x.Stamp(reworkAt)
	events := len(z.x.Pending())
	lots := map[CargoLotID]*CargoLot{first.ID(): first, second.ID(): second}
	n, err := z.x.UnloadHouse("SH-2", lots, 0, ev("WH-CONFIRM-2", unloadAt), "ops-wh", unloadAt)
	if err != nil || n != 2 {
		t.Fatalf("unloaded %d lines: %v", n, err)
	}
	if z.x.LiveFor("SH-2") != 0 || z.x.LiveFor("SH-1") != 10 || z.x.Tendered() != 10 {
		t.Fatalf("nothing of the house stays, the other house does: %d %d", z.x.LiveFor("SH-2"), z.x.Tendered())
	}
	rs := z.x.State().Reversals
	if len(rs) != 2 || rs[0].LineNo != 1 || rs[0].Packs != 6 || rs[1].LineNo != 2 || rs[1].Packs != 4 || rs[0].Reason != ReversalUnstuffed || rs[1].Reason != ReversalUnstuffed ||
		rs[0].Evidence.Reference != "WH-CONFIRM-2" || rs[1].Evidence.Reference != "WH-CONFIRM-2" || rs[0].By != "ops-wh" || !rs[0].At.Equal(unloadAt) ||
		rs[0].GrossWeight != kg(1800) || rs[1].GrossWeight != kg(1200) {
		t.Fatalf("reversals = %+v", rs)
	}
	ev := z.x.Pending()[events:]
	if len(ev) != 2 || ev[0].Type != "CargoUnloaded.v1" || ev[0].Reason != "UNSTUFFED" || ev[0].ShipmentID != "SH-2" || ev[0].ItemNo != 1 || ev[1].ItemNo != 2 {
		t.Fatalf("events = %+v", ev)
	}
	for i, l := range []*CargoLot{first, second} {
		st := l.State()
		if st.State != LotReceived || st.ClosedBy != "" || !st.ClosedAt.IsZero() || !st.ClosedEnteredAt.IsZero() || len(st.Reopens) != 1 {
			t.Fatalf("lot %s is received again, with no close facts: %+v", l.ID(), st)
		}
		r := st.Reopens[0]
		if r.No != 1 || r.ExecutionID != "X-1" || r.ReworkNo != 1 || r.ReversalNo != i+1 || r.ClosedBy != "ops-lot" || !r.ClosedAt.Equal(at) ||
			!r.ClosedEnteredAt.Equal(at.Add(time.Minute)) || r.ReopenedBy != "ops-wh" || !r.ReopenedAt.Equal(unloadAt) {
			t.Fatalf("lot %s reopen = %+v", l.ID(), r)
		}
		if p := l.Pending(); p[len(p)-1].ExecutionID != "X-1" || p[len(p)-1].Type != "CargoLotReopened.v1" || p[len(p)-1].ItemNo != 1 || p[len(p)-1].LotID != l.ID() || p[len(p)-1].ShipmentID != "SH-2" {
			t.Fatalf("lot events = %+v", p)
		}
		if len(l.AddedReopens()) != 1 {
			t.Fatalf("what a save writes: %+v", l.AddedReopens())
		}
		l.Stamp(unloadAt.Add(time.Minute))
		if got := l.State().Reopens[0].ReopenedEnteredAt; !got.Equal(unloadAt.Add(time.Minute)) {
			t.Fatalf("entered at %v", got)
		}
		if back, err := RestoreCargoLot(l.State()); err != nil || back.Current() != LotReceived || len(back.AddedReopens()) != 0 {
			t.Fatalf("a reopened lot restores: %v", err)
		}
		// opened again, the lot takes the changes a received lot takes and closes as one does
		if err := l.Close(0, 0, z.intent["SH-2"], "ops-lot", unloadAt.Add(time.Hour)); !errors.Is(err, ErrUnaccounted) {
			t.Fatalf("a lot of which nothing is loaded is not closed again while the house plans more: %v", err)
		}
	}
	if back, err := RestoreLoadExecution(z.x.State()); err != nil || back.LiveFor("SH-2") != 0 {
		t.Fatalf("the unloaded container restores: %v", err)
	}
	// the house stays attached: nothing here ends its membership, and it is not in the container's packing
	if z.x.Current() != ExecutionRework {
		t.Fatalf("still in rework: %s", z.x.Current())
	}
}

func TestUnloadHouseRefusals(t *testing.T) {
	lotsOf := func(y *yard, ids ...ShipmentID) map[CargoLotID]*CargoLot {
		m := map[CargoLotID]*CargoLot{}
		for _, id := range ids {
			m[y.lot[id].ID()] = y.lot[id]
		}
		return m
	}
	type tc struct {
		build     func(t *testing.T) *yard
		shipment  ShipmentID
		lots      func(y *yard) map[CargoLotID]*CargoLot
		elsewhere int64
		evidence  Evidence
		by        string
		at        time.Time
		want      error
		text      string
	}
	good := ev("WH-1", unloadAt)
	own := func(ids ...ShipmentID) func(y *yard) map[CargoLotID]*CargoLot {
		return func(y *yard) map[CargoLotID]*CargoLot { return lotsOf(y, ids...) }
	}
	cases := map[string]tc{
		"a sealed container":          {sealedYard, "SH-2", own("SH-2"), 0, good, "ops", unloadAt, ErrNotLoading, "this one is CLOSED"},
		"a container still loading":   {newYard, "SH-2", own("SH-2"), 0, good, "ops", unloadAt, ErrNotLoading, "this one is LOADING"},
		"a house that is not in it":   {reworked, "SH-3", own("SH-3"), 0, good, "ops", unloadAt, ErrInvalidLoad, "has no live packs in this container"},
		"a house nobody knows":        {reworked, "NO_SUCH_HOUSE", own(), 0, good, "ops", unloadAt, ErrInvalidLoad, "has no live packs in this container"},
		"no house":                    {reworked, "", own(), 0, good, "ops", unloadAt, ErrInvalidLoad, "a house to unload"},
		"a house also in another one": {reworked, "SH-2", own("SH-2"), 4, good, "ops", unloadAt, ErrInvalidLoad, "another container of the consolidation; unloading part of a house is not built"},
		"no evidence":                 {reworked, "SH-2", own("SH-2"), 0, Evidence{}, "ops", unloadAt, ErrInvalidEvidence, ""},
		"evidence from the future":    {reworked, "SH-2", own("SH-2"), 0, ev("WH-1", unloadAt.Add(time.Hour)), "ops", unloadAt, ErrInvalidEvidence, ""},
		"no actor":                    {reworked, "SH-2", own("SH-2"), 0, good, " ", unloadAt, ErrInvalidChange, "author and business time"},
		"before the rework was asked": {reworked, "SH-2", own("SH-2"), 0, ev("WH-1", reworkAt.Add(-time.Hour)), "ops", reworkAt.Add(-time.Second), ErrInvalidChange, "not before the execution's last change"},
		"the lot was not read":        {reworked, "SH-2", own(), 0, good, "ops", unloadAt, ErrInvalidLoad, "which was not read"},
		"a lot of another house":      {reworked, "SH-2", own("SH-1"), 0, good, "ops", unloadAt, ErrInvalidLoad, "which was not read"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			y := c.build(t)
			was, tendered, lotState, lotEvents := len(y.x.Pending()), y.x.Tendered(), y.lot["SH-2"].Current(), len(y.lot["SH-2"].Pending())
			n, err := y.x.UnloadHouse(c.shipment, c.lots(y), c.elsewhere, c.evidence, c.by, c.at)
			if !errors.Is(err, c.want) || !strings.Contains(err.Error(), c.text) || n != 0 {
				t.Fatalf("got %d, %v; want %v containing %q", n, err, c.want, c.text)
			}
			if len(y.x.Pending()) != was || y.x.Tendered() != tendered || y.lot["SH-2"].Current() != lotState || len(y.lot["SH-2"].Pending()) != lotEvents ||
				len(y.x.State().Reversals) != 0 || len(y.lot["SH-2"].State().Reopens) != 0 {
				t.Fatalf("a refused command changes neither the execution nor a lot")
			}
		})
	}
}

func TestUnloadHouseDatedBeforeTheLotsCloseChangesNothing(t *testing.T) {
	y := newYard(t)
	at := t0.Add(4 * time.Hour)
	first := y.receive(t, "SH-2", "L-2a", cfs, 6, y.intent["SH-2"])
	second := y.receive(t, "SH-2", "L-2b", Facility{Location: loc("CNSHG"), Code: "CFS-2"}, 4, y.intent["SH-2"])
	y.stuffWhole(t, y.x, "SH-2", first, at)
	y.stuffWhole(t, y.x, "SH-2", second, at)
	if err := y.x.Seal(y.c, seal("SEAL-1", at), "ops", at); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(6, 10, y.intent["SH-2"], "ops", at); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(4, 10, y.intent["SH-2"], "ops", t0.Add(6*time.Hour+30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := y.x.OpenForRework(y.c, "r", "ops", reworkAt); err != nil {
		t.Fatal(err)
	}
	// the second lot closed at +6h30; the unload is dated +6h, after the first lot's close and before the second's
	_, err := y.x.UnloadHouse("SH-2", map[CargoLotID]*CargoLot{first.ID(): first, second.ID(): second}, 0, ev("WH", unloadAt), "ops", unloadAt)
	if !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("got %v", err)
	}
	if first.Current() != LotClosed || len(first.State().Reopens) != 0 || y.x.LiveFor("SH-2") != 10 || len(y.x.State().Reversals) != 0 {
		t.Fatalf("the first lot is not reopened for a house that cannot be unloaded: %s", first.Current())
	}
}

func TestUnloadHouseWithLotsThatAreNotClosed(t *testing.T) {
	y := reworked(t)
	// SH-1's lot was never closed: the unloading reverses its lines and reopens nothing
	n := y.unload(t, "SH-1")
	if n != 1 || y.lot["SH-1"].Current() != LotReceived || len(y.lot["SH-1"].State().Reopens) != 0 {
		t.Fatalf("an open lot stays as it was: %d %s", n, y.lot["SH-1"].Current())
	}
}

func TestResealAfterRework(t *testing.T) {
	y := reworked(t)
	y.unload(t, "SH-2")
	before := len(y.x.Pending())
	if err := y.x.Seal(y.c, seal("SEAL-2", resealAt), "ops-reseal", resealAt); err != nil {
		t.Fatal(err)
	}
	st := y.x.State()
	if y.x.Current() != ExecutionClosed || st.ClosedBy != "ops-reseal" || !st.ClosedAt.Equal(resealAt) || st.CloseEvidence.Reference != "SEAL-2" || st.CloseEvidence.Source != EvidenceSeal {
		t.Fatalf("sealed again: %s %+v", y.x.Current(), st)
	}
	r := st.Reworks[0]
	if r.End != ReworkSealed || r.EndedBy != "ops-reseal" || !r.EndedAt.Equal(resealAt) || r.NewSeal != "SEAL-2" || r.BrokenSeal.Reference != "SEAL-1" || r.AskedBy != "ops-asker" {
		t.Fatalf("rework record: %+v", r)
	}
	if ev := y.x.Pending()[before:]; len(ev) != 1 || ev[0].Type != "LoadExecutionClosed.v1" || ev[0].ItemNo != 0 || !ev[0].At.Equal(resealAt) {
		t.Fatalf("events = %+v", ev)
	}
	y.x.Stamp(resealAt.Add(time.Minute))
	st = y.x.State()
	if !st.ClosedEnteredAt.Equal(resealAt.Add(time.Minute)) || !st.Reworks[0].EndedEnteredAt.Equal(resealAt.Add(time.Minute)) {
		t.Fatalf("entered at: %+v", st)
	}
	if got := y.x.Added(); len(got.Reworks) != 1 || len(got.EndedReworks) != 0 {
		t.Fatalf("a rework opened and ended in one load is written once: %+v", got)
	}
	// a stored rework that ends is written as an ending
	back, err := RestoreLoadExecution(y.x.State())
	if err != nil || back.Current() != ExecutionClosed {
		t.Fatalf("sealed again restores: %v", err)
	}
	if err = back.OpenForRework(y.c, "again", "ops-asker-2", resealAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := back.Added(); len(got.Reworks) != 1 || got.Reworks[0].No != 2 {
		t.Fatalf("the second rework: %+v", got)
	}
	// like the carrier evidence, a sealed container again takes the carrier's word
	if _, _, err = y.x.RecordCarrierEvidence(CarrierEvidence{Fact: CarrierDeparted, Pieces: 10, Transport: "MV-EVER 12", Evidence: ev("C-1", resealAt)}, "ops", resealAt); err != nil {
		t.Fatalf("a container sealed again takes carrier evidence as a sealed one does: %v", err)
	}
}

func TestResealRefusals(t *testing.T) {
	type tc struct {
		prep     func(t *testing.T, y *yard)
		evidence Evidence
		by       string
		at       time.Time
		consol   func(t *testing.T, y *yard) *Consolidation
		want     error
		text     string
	}
	same := func(t *testing.T, y *yard) *Consolidation { return y.c }
	unloadSH2 := func(t *testing.T, y *yard) { y.unload(t, "SH-2") }
	cases := map[string]tc{
		"the broken seal put back":       {unloadSH2, seal("SEAL-1", resealAt), "ops", resealAt, same, ErrInvalidEvidence, "was broken in rework 1"},
		"an evidence that is not a seal": {unloadSH2, Evidence{Source: "TALLY", Reference: "SEAL-2", OccurredAt: resealAt}, "ops", resealAt, same, ErrInvalidEvidence, "closes with the container's seal"},
		"no seal reference":              {unloadSH2, seal("", resealAt), "ops", resealAt, same, ErrInvalidEvidence, ""},
		"a seal from the future":         {unloadSH2, seal("SEAL-2", resealAt.Add(time.Hour)), "ops", resealAt, same, ErrInvalidEvidence, ""},
		"an empty container": {func(t *testing.T, y *yard) {
			y.unload(t, "SH-1")
			y.unload(t, "SH-2")
		}, seal("SEAL-2", resealAt), "ops", resealAt, same, ErrInvalidLoad, "nothing is stuffed in the container"},
		"before the unloading": {unloadSH2, seal("SEAL-2", unloadAt.Add(-time.Minute)), "ops", unloadAt.Add(-time.Minute), same, ErrInvalidChange, "not before the execution's last change"},
		"no actor":             {unloadSH2, seal("SEAL-2", resealAt), " ", resealAt, same, ErrInvalidChange, "author and business time"},
		"an open consolidation": {unloadSH2, seal("SEAL-2", resealAt), "ops", resealAt, func(t *testing.T, y *yard) *Consolidation { return seaConsol(t, "C-S", KindSeaLCLOwn) },
			ErrNotFrozen, "it is OPEN"},
		"before the consolidation's latest change": {unloadSH2, seal("SEAL-2", resealAt), "ops", resealAt, func(t *testing.T, y *yard) *Consolidation {
			cur := y.c.Current()
			if err := y.c.Amend(Route{}, cur.PlannedDeparture.Add(time.Hour), cur.HouseCutOff, "a late amendment", "ops", resealAt.Add(time.Hour), 3); err != nil {
				t.Fatal(err)
			}
			return y.c
		}, ErrInvalidChange, "not before the consolidation's latest change"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			y := reworked(t)
			c.prep(t, y)
			consol := c.consol(t, y)
			was, state := len(y.x.Pending()), y.x.State()
			err := y.x.Seal(consol, c.evidence, c.by, c.at)
			if !errors.Is(err, c.want) || !strings.Contains(err.Error(), c.text) {
				t.Fatalf("got %v, want %v containing %q", err, c.want, c.text)
			}
			if y.x.Current() != ExecutionRework || len(y.x.Pending()) != was || !y.x.State().Reworks[0].Open() || y.x.State().ClosedAt != state.ClosedAt {
				t.Fatalf("a refused seal changes nothing: %s", y.x.Current())
			}
		})
	}
	t.Run("a seal of an earlier round is not used again", func(t *testing.T) {
		y := reworked(t)
		y.unload(t, "SH-2")
		if err := y.x.Seal(y.c, seal("SEAL-2", resealAt), "ops", resealAt); err != nil {
			t.Fatal(err)
		}
		if err := y.x.OpenForRework(y.c, "again", "ops", resealAt.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		// SEA compares seals by their letters and digits in capitals, so each of these is a seal that was broken put
		// back
		for _, ref := range []string{"SEAL-1", "SEAL-2", "seal-1", "Seal-1", "SEAL 1", "SEAL1", "SEAL_1", "seal-2", "SEAL 2", "SEAL2"} {
			err := y.x.Seal(y.c, seal(ref, resealAt.Add(2*time.Hour)), "ops", resealAt.Add(2*time.Hour))
			if !errors.Is(err, ErrInvalidEvidence) || !strings.Contains(err.Error(), "was on this container before") {
				t.Fatalf("%s: %v", ref, err)
			}
		}
		// a combining mark is dropped by SEA's key and by ours
		for _, ref := range []string{"SEAL-1\u0345", "SEAL\u0301-1", "S\u0345EAL-1"} {
			if err := y.x.Seal(y.c, seal(ref, resealAt.Add(2*time.Hour)), "ops", resealAt.Add(2*time.Hour)); !errors.Is(err, ErrInvalidEvidence) ||
				!strings.Contains(err.Error(), "was on this container before") {
				t.Fatalf("%q: %v", ref, err)
			}
		}
		for _, ref := range []string{"SEAL-3\u00a0", "\u00a0SEAL-3", "SEAL-3\u3000", "\u0085SEAL-3", "SEAL-3\u2003"} {
			if err := y.x.Seal(y.c, seal(ref, resealAt.Add(2*time.Hour)), "ops", resealAt.Add(2*time.Hour)); !errors.Is(err, ErrInvalidEvidence) {
				t.Fatalf("%q: %v", ref, err)
			}
		}
		for _, ref := range []string{"SEAL-1 ", " SEAL-1", " SEAL-3", "SEAL-3\t", "SEAL-3\n"} {
			err := y.x.Seal(y.c, seal(ref, resealAt.Add(2*time.Hour)), "ops", resealAt.Add(2*time.Hour))
			if !errors.Is(err, ErrInvalidEvidence) || !strings.Contains(err.Error(), "has a blank at an end") {
				t.Fatalf("%q: %v", ref, err)
			}
		}
		if err := y.x.Seal(y.c, seal("SEAL-3", resealAt.Add(2*time.Hour)), "ops", resealAt.Add(2*time.Hour)); err != nil {
			t.Fatalf("a new seal: %v", err)
		}
	})
	t.Run("seals that differ only in non-ASCII letters are one seal, refused before the database", func(t *testing.T) {
		y := newYard(t)
		at := t0.Add(4 * time.Hour)
		y.stuffWhole(t, y.x, "SH-1", y.lot["SH-1"], at)
		y.stuffWhole(t, y.x, "SH-2", y.lot["SH-2"], at)
		if err := y.x.Seal(y.c, seal("\u00c9SEAL-5", at), "ops", at); err != nil {
			t.Fatal(err)
		}
		if err := y.x.OpenForRework(y.c, "r", "ops", reworkAt); err != nil {
			t.Fatal(err)
		}
		y.unload(t, "SH-2")
		if err := y.x.Seal(y.c, seal("\u00c0SEAL-5", resealAt), "ops", resealAt); !errors.Is(err, ErrInvalidEvidence) || !strings.Contains(err.Error(), "was on this container before") {
			t.Fatalf("an accented letter is not told apart: %v", err)
		}
	})
	t.Run("the first seal has no blank at an end either", func(t *testing.T) {
		y := newYard(t)
		at := t0.Add(4 * time.Hour)
		y.stuffWhole(t, y.x, "SH-1", y.lot["SH-1"], at)
		was := len(y.x.Pending())
		for _, ref := range []string{" SEAL-1", "SEAL-1 "} {
			if err := y.x.Seal(y.c, seal(ref, at), "ops", at); !errors.Is(err, ErrInvalidEvidence) || !strings.Contains(err.Error(), "has a blank at an end") {
				t.Fatalf("%q: %v", ref, err)
			}
		}
		if y.x.Current() != ExecutionLoading || len(y.x.Pending()) != was {
			t.Fatalf("a refused seal changes nothing: %s", y.x.Current())
		}
	})
}

func TestGiveUpAfterAReworkThatEmptiedTheContainer(t *testing.T) {
	y := reworked(t)
	cancelAfter := t0.Add(8 * time.Hour)
	if err := y.x.Cancel("the consignee dropped the shipment", "ops", cancelAfter); !errors.Is(err, ErrInvalidExecution) || !strings.Contains(err.Error(), "20 packs are still loaded") {
		t.Fatalf("with live packs: %v", err)
	}
	y.unload(t, "SH-2")
	if err := y.x.Cancel("rolled", "ops", cancelAfter); !errors.Is(err, ErrInvalidExecution) || !strings.Contains(err.Error(), "10 packs are still loaded") {
		t.Fatalf("with one house left: %v", err)
	}
	y.unload(t, "SH-1")
	was := len(y.x.Pending())
	if err := y.x.Cancel(" ", "ops", cancelAfter); !errors.Is(err, ErrInvalidExecution) || !strings.Contains(err.Error(), "needs a reason") {
		t.Fatalf("no reason: %v", err)
	}
	if err := y.x.Cancel("the consignee dropped the shipment", "ops-cancel", unloadAt.Add(-time.Minute)); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("before the unloading: %v", err)
	}
	if err := y.x.Cancel("the consignee dropped the shipment", "ops-cancel", cancelAfter); err != nil {
		t.Fatal(err)
	}
	st := y.x.State()
	r := st.Reworks[0]
	if y.x.Current() != ExecutionCancelled || st.CancelledBy != "ops-cancel" || !st.ClosedAt.IsZero() || r.End != ReworkGivenUp || r.EndedBy != "ops-cancel" ||
		!r.EndedAt.Equal(cancelAfter) || r.NewSeal != "" || r.BrokenSeal.Reference != "SEAL-1" {
		t.Fatalf("given up: %s %+v", y.x.Current(), r)
	}
	if ev := y.x.Pending()[was:]; len(ev) != 1 || ev[0].Type != "LoadExecutionCancelled.v1" || ev[0].ItemNo != 1 {
		t.Fatalf("events = %+v", ev)
	}
	y.x.Stamp(cancelAfter.Add(time.Minute))
	if got := y.x.State().Reworks[0].EndedEnteredAt; !got.Equal(cancelAfter.Add(time.Minute)) {
		t.Fatalf("entered at %v", got)
	}
	back, err := RestoreLoadExecution(y.x.State())
	if err != nil || back.Current() != ExecutionCancelled {
		t.Fatalf("restores: %v", err)
	}
	if err = back.Cancel("again", "ops", cancelAfter.Add(time.Hour)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("given up for good: %v", err)
	}
	// the unit is planned again: a new stuffing opens on it, dated not before the give-up
	if _, err = OpenStuffingAfter("T", "X-9", y.c, 1, cfs, false, cancelAfter, "ops", cancelAfter); err != nil {
		t.Fatalf("a new stuffing on the unit: %v", err)
	}
	if _, err = OpenStuffingAfter("T", "X-9", y.c, 1, cfs, true, cancelAfter, "ops", cancelAfter); !errors.Is(err, ErrLoadUnitInUse) {
		t.Fatalf("a unit whose stuffing is in REWORK counts as taken: %v", err)
	}
}

func TestEverythingElseIsRefusedInRework(t *testing.T) {
	y := reworked(t)
	at := resealAt
	cases := map[string]func() error{
		"start": func() error { return y.x.Start("ops", at) },
		"load": func() error {
			return y.x.Load(LoadLine{Packs: 1, Evidence: ev("T", at)}, y.c, y.lot["SH-3"], y.entry["SH-3"], LotLoad{}, 0, "ops", at)
		},
		"unload by line": func() error { return y.x.Unload(1, 1, ReversalUnstuffed, ev("U", at), y.lot["SH-1"], "ops", at) },
		"place a hold":   func() error { return y.x.PlaceHold(HoldCustoms, "", ev("H", at), "ops", at) },
		"release a hold": func() error { return y.x.ReleaseHold(1, ev("R", at), "", "ops", at) },
		"carrier evidence": func() error {
			_, _, err := y.x.RecordCarrierEvidence(CarrierEvidence{Fact: CarrierDeparted, Pieces: 1, Transport: "MV 1", Evidence: ev("C", at)}, "ops", at)
			return err
		},
		"close as a tender": func() error { return y.x.Close(ev("R", at), "ops", at) },
		"open for rework":   func() error { return y.x.OpenForRework(y.c, "again", "ops", at) },
		"resolve a discrepancy": func() error {
			return y.x.ResolveDiscrepancy(1, ResolvedByAccepted, "", "x", "ops", at)
		},
	}
	want := map[string][2]any{
		"start": {ErrInvalidTransition, "in rework is not started"}, "load": {ErrNotLoading, "takes no load"},
		"unload by line": {ErrNotLoading, "by whole houses"}, "place a hold": {ErrNotLoading, "takes no hold"},
		"release a hold": {ErrHoldNotReleasable, "no hold to release"}, "carrier evidence": {ErrInvalidTransition, "takes no carrier evidence"},
		"close as a tender": {ErrInvalidExecution, "closes with the container's seal"}, "open for rework": {ErrInvalidTransition, "this one is REWORK"},
		"resolve a discrepancy": {ErrNotResolvable, "loading discrepancy 1"},
	}
	was := len(y.x.Pending())
	for name, run := range cases {
		err := run()
		w := want[name]
		if !errors.Is(err, w[0].(error)) || !strings.Contains(err.Error(), w[1].(string)) {
			t.Errorf("%s: got %v, want %v containing %q", name, err, w[0], w[1])
		}
	}
	if len(y.x.Pending()) != was || y.x.Current() != ExecutionRework {
		t.Fatalf("nothing changed: %s, %d events", y.x.Current(), len(y.x.Pending()))
	}
	// a hold is not placed by the checks of the limits either, whatever the container weighs
	if y.x.HoldIfOverLimits(y.c, map[CargoLotID]*CargoLot{"L-1": y.lot["SH-1"], "L-2": y.lot["SH-2"]}, ev("X", at), "ops", at) || y.x.HoldForScreening("L-1", ev("X", at), "ops", at) {
		t.Fatal("a container in rework is not held")
	}
	// the container's limits are worked out as for any other
	check, ok := y.x.CheckLimits(y.c, map[CargoLotID]*CargoLot{"L-1": y.lot["SH-1"], "L-2": y.lot["SH-2"]}, AllotLots([]*LoadExecution{y.x}, map[CargoLotID]*CargoLot{"L-1": y.lot["SH-1"], "L-2": y.lot["SH-2"]}))
	if !ok || !check.Checked || check.CargoGrams != 6_000_000 {
		t.Fatalf("limits: %+v %v", check, ok)
	}
}

func TestTwoRoundsOnOneExecution(t *testing.T) {
	y := newYard(t)
	at := t0.Add(4 * time.Hour)
	for _, id := range []ShipmentID{"SH-1", "SH-2", "SH-3"} {
		y.stuffWhole(t, y.x, id, y.lot[id], at)
	}
	if err := y.x.Seal(y.c, seal("SEAL-1", at), "sealer-0", at); err != nil {
		t.Fatal(err)
	}
	for round, step := range []struct {
		house ShipmentID
		seal  string
	}{{"SH-3", "SEAL-2"}, {"SH-2", "SEAL-3"}} {
		at = at.Add(time.Hour)
		if err := y.x.OpenForRework(y.c, fmt.Sprintf("round %d", round+1), fmt.Sprintf("asker-%d", round+1), at); err != nil {
			t.Fatal(err)
		}
		at = at.Add(time.Hour)
		if _, err := y.x.UnloadHouse(step.house, map[CargoLotID]*CargoLot{y.lot[step.house].ID(): y.lot[step.house]}, 0, ev("WH-"+step.seal, at), "ops-wh", at); err != nil {
			t.Fatal(err)
		}
		at = at.Add(time.Hour)
		if err := y.x.Seal(y.c, seal(step.seal, at), fmt.Sprintf("sealer-%d", round+1), at); err != nil {
			t.Fatal(err)
		}
		y.x.Stamp(at)
		y.x.Acknowledge()
	}
	st := y.x.State()
	if y.x.Current() != ExecutionClosed || len(st.Reworks) != 2 || y.x.LiveFor("SH-1") != 10 || y.x.LiveFor("SH-2") != 0 || y.x.LiveFor("SH-3") != 0 {
		t.Fatalf("after two rounds: %s %+v", y.x.Current(), st.Reworks)
	}
	r1, r2 := st.Reworks[0], st.Reworks[1]
	if r1.No != 1 || r1.AskedBy != "asker-1" || r1.BrokenSeal.Reference != "SEAL-1" || r1.NewSeal != "SEAL-2" || r1.EndedBy != "sealer-1" ||
		r2.No != 2 || r2.AskedBy != "asker-2" || r2.BrokenSeal.Reference != "SEAL-2" || r2.BrokenSealedBy != "sealer-1" || r2.NewSeal != "SEAL-3" || r2.EndedBy != "sealer-2" ||
		st.CloseEvidence.Reference != "SEAL-3" || st.ClosedBy != "sealer-2" || st.ExecutionID != "X-1" || st.LoadUnitNo != 1 {
		t.Fatalf("the history of the seals: %+v", st.Reworks)
	}
	if _, err := RestoreLoadExecution(st); err != nil {
		t.Fatalf("two rounds restore: %v", err)
	}
	// SH-3's lot was never closed, and SH-2's neither: the unloadings reopened nothing
	if len(y.lot["SH-2"].State().Reopens) != 0 {
		t.Fatal("an open lot is not reopened")
	}
	// a stored first round is not written again by the second
	back, _ := RestoreLoadExecution(st)
	if err := back.OpenForRework(y.c, "third", "asker-3", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := back.Added(); len(got.Reworks) != 1 || got.Reworks[0].No != 3 || len(got.EndedReworks) != 0 {
		t.Fatalf("what the third opening writes: %+v", got)
	}
}

func TestAConsolidationWithAContainerInReworkDoesNotReopen(t *testing.T) {
	y := reworked(t)
	// the container is not sealed (it is in REWORK) and holds cargo that was not tendered, so only this rule refuses
	err := y.c.Reopen("a late house", "ops", resealAt, 0, Stuffed{Reworking: 1, Last: reworkAt})
	if !errors.Is(err, ErrExecutionsClosed) || !strings.Contains(err.Error(), "1 containers are opened for rework") {
		t.Fatalf("a consolidation with a container in rework does not reopen: %v", err)
	}
	if err = y.c.Reopen("a late house", "ops", resealAt, 0, Stuffed{Last: reworkAt}); err != nil {
		t.Fatalf("without a sealed container or one in rework it reopens: %v", err)
	}
}

func TestReworksAreRestoredWithTheChecksOfTheOtherParts(t *testing.T) {
	// a stored state of every kind, then one thing broken at a time
	build := map[string]func(t *testing.T) LoadExecutionState{
		"in rework": func(t *testing.T) LoadExecutionState { return reworked(t).x.State() },
		"sealed again": func(t *testing.T) LoadExecutionState {
			y := reworked(t)
			y.unload(t, "SH-2")
			if err := y.x.Seal(y.c, seal("SEAL-2", resealAt), "ops", resealAt); err != nil {
				t.Fatal(err)
			}
			return y.x.State()
		},
		"given up": func(t *testing.T) LoadExecutionState {
			y := reworked(t)
			y.unload(t, "SH-1")
			y.unload(t, "SH-2")
			if err := y.x.Cancel("rolled", "ops", resealAt); err != nil {
				t.Fatal(err)
			}
			return y.x.State()
		},
	}
	for name, b := range build {
		if _, err := RestoreLoadExecution(b(t)); err != nil {
			t.Fatalf("%s restores: %v", name, err)
		}
	}
	type bad struct {
		from   string
		change func(st *LoadExecutionState)
		text   string
	}
	for name, c := range map[string]bad{
		"a rework numbered twice":               {"sealed again", func(st *LoadExecutionState) { st.Reworks[0].No = 2 }, "is numbered 2"},
		"a rework with no reason":               {"in rework", func(st *LoadExecutionState) { st.Reworks[0].Reason = " " }, "who asked, when and why"},
		"a rework with no one who asked":        {"in rework", func(st *LoadExecutionState) { st.Reworks[0].AskedBy = "" }, "who asked, when and why"},
		"a rework with no time":                 {"in rework", func(st *LoadExecutionState) { st.Reworks[0].AskedAt = time.Time{} }, "who asked, when and why"},
		"a rework asked before the seal":        {"in rework", func(st *LoadExecutionState) { st.Reworks[0].AskedAt = st.Reworks[0].BrokenSealedAt.Add(-time.Second) }, "the seal it broke"},
		"a broken seal that is not a seal":      {"in rework", func(st *LoadExecutionState) { st.Reworks[0].BrokenSeal.Source = "TALLY" }, "the seal it broke"},
		"a broken seal with no reference":       {"in rework", func(st *LoadExecutionState) { st.Reworks[0].BrokenSeal.Reference = "" }, "the seal it broke"},
		"a broken seal nobody applied":          {"in rework", func(st *LoadExecutionState) { st.Reworks[0].BrokenSealedBy = "" }, "the seal it broke"},
		"a rework in rework that ended":         {"in rework", func(st *LoadExecutionState) { st.Reworks[0].End = ReworkSealed }, "its ending"},
		"an open rework of a sealed container":  {"sealed again", func(st *LoadExecutionState) { st.Reworks[0].End = "" }, "its ending"},
		"an ending with no one":                 {"sealed again", func(st *LoadExecutionState) { st.Reworks[0].EndedBy = "" }, "its ending"},
		"an ending before the rework was asked": {"sealed again", func(st *LoadExecutionState) { st.Reworks[0].EndedAt = st.Reworks[0].AskedAt.Add(-time.Second) }, "its ending"},
		"a new seal that is the broken one": {"sealed again", func(st *LoadExecutionState) {
			st.Reworks[0].NewSeal, st.CloseEvidence.Reference = "SEAL-1", "SEAL-1"
		}, "its new seal"},
		"a new seal that is not the seal":       {"sealed again", func(st *LoadExecutionState) { st.Reworks[0].NewSeal = "SEAL-9" }, "did not end with that seal"},
		"a sealed container by someone else":    {"sealed again", func(st *LoadExecutionState) { st.Reworks[0].EndedBy = "someone" }, "did not end with that seal"},
		"a sealed container at another time":    {"sealed again", func(st *LoadExecutionState) { st.Reworks[0].EndedAt = st.Reworks[0].EndedAt.Add(time.Minute) }, "did not end with that seal"},
		"a rework that was given up but sealed": {"given up", func(st *LoadExecutionState) { st.Reworks[0].End = ReworkSealed; st.Reworks[0].NewSeal = "SEAL-2" }, "a given up container whose last rework was not given up"},
		"a rework given up by another":          {"given up", func(st *LoadExecutionState) { st.Reworks[0].EndedBy = "someone" }, "it was given up"},
		"a rework ended some other way":         {"given up", func(st *LoadExecutionState) { st.Reworks[0].End = "LOST" }, "how it ended"},
		"a container in rework with no rework":  {"in rework", func(st *LoadExecutionState) { st.Reworks = nil }, "no rework"},
		"a loading container with a rework": {"sealed again", func(st *LoadExecutionState) {
			st.State, st.ClosedAt, st.ClosedBy, st.CloseEvidence = ExecutionLoading, time.Time{}, "", Evidence{}
		}, "a rework of a container that is LOADING"},
		"a container in rework with a seal": {"in rework", func(st *LoadExecutionState) {
			st.ClosedAt, st.ClosedBy, st.CloseEvidence = resealAt, "x", seal("SEAL-2", resealAt)
		}, "rework"},
		"a container in rework with only a seal's evidence": {"in rework", func(st *LoadExecutionState) { st.CloseEvidence = seal("SEAL-2", resealAt) }, "rework"},
		"a container in rework with carrier evidence": {"in rework", func(st *LoadExecutionState) {
			st.CarrierEvidence = []CarrierEvidence{{No: 1, Fact: CarrierDeparted, Pieces: 1, By: "ops"}}
		}, "carrier evidence before the tender closed"},
		"a container in rework with a hold placed": {"in rework", func(st *LoadExecutionState) {
			st.Holds = []Hold{{No: 1, Reason: HoldCustoms, By: "ops", At: reworkAt}}
		}, "rework"},
		"a tender with a rework": {"sealed again", func(st *LoadExecutionState) { st.Kind, st.LoadUnitNo = ExecutionTender, 0 }, "a tender has no rework"},
		"the seal of the second round carried before": {"sealed again", func(st *LoadExecutionState) {
			second := st.Reworks[0]
			second.No, second.BrokenSeal.Reference, second.NewSeal = 2, "SEAL-2", "SEAL-1"
			st.Reworks = append(st.Reworks, second)
			st.CloseEvidence.Reference = "SEAL-1"
		}, "its new seal"},
		"the seal of the second round carried before, keyed another way": {"sealed again", func(st *LoadExecutionState) {
			second := st.Reworks[0]
			second.No, second.BrokenSeal.Reference, second.NewSeal = 2, "SEAL-2", "seal 1"
			st.Reworks = append(st.Reworks, second)
			st.CloseEvidence.Reference = "seal 1"
		}, "its new seal"},
		"a second round that breaks another seal": {"sealed again", func(st *LoadExecutionState) {
			second := st.Reworks[0]
			second.No, second.BrokenSeal.Reference, second.NewSeal = 2, "SEAL-7", "SEAL-8"
			st.Reworks = append(st.Reworks, second)
			st.CloseEvidence.Reference = "SEAL-8"
		}, "breaks a seal other than"},
	} {
		t.Run(name, func(t *testing.T) {
			st := build[c.from](t)
			c.change(&st)
			if _, err := RestoreLoadExecution(st); !errors.Is(err, ErrInvalidExecution) || !strings.Contains(err.Error(), c.text) {
				t.Fatalf("restored: %v, want one containing %q", err, c.text)
			}
		})
	}
}

func TestReopensOfALotAreRestoredWithChecks(t *testing.T) {
	y := newYard(t)
	at := t0.Add(4 * time.Hour)
	lot := y.lot["SH-1"]
	y.stuffWhole(t, y.x, "SH-1", lot, at)
	if err := y.x.Seal(y.c, seal("SEAL-1", at), "ops", at); err != nil {
		t.Fatal(err)
	}
	if err := lot.Close(10, 10, y.intent["SH-1"], "ops-lot", at); err != nil {
		t.Fatal(err)
	}
	if err := y.x.OpenForRework(y.c, "r", "ops", reworkAt); err != nil {
		t.Fatal(err)
	}
	if _, err := y.x.UnloadHouse("SH-1", map[CargoLotID]*CargoLot{lot.ID(): lot}, 0, ev("WH", unloadAt), "ops", unloadAt); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(st *CargoLotState){
		"a reopen numbered twice":         func(st *CargoLotState) { st.Reopens[0].No = 2 },
		"a reopen with no execution":      func(st *CargoLotState) { st.Reopens[0].ExecutionID = "" },
		"a reopen with no rework":         func(st *CargoLotState) { st.Reopens[0].ReworkNo = 0 },
		"a reopen with no reversal":       func(st *CargoLotState) { st.Reopens[0].ReversalNo = 0 },
		"a reopen with no one who closed": func(st *CargoLotState) { st.Reopens[0].ClosedBy = "" },
		"a reopen with no closing time":   func(st *CargoLotState) { st.Reopens[0].ClosedAt = time.Time{} },
		"a reopen with no one who opened": func(st *CargoLotState) { st.Reopens[0].ReopenedBy = "" },
		"a reopen before the closing":     func(st *CargoLotState) { st.Reopens[0].ReopenedAt = st.Reopens[0].ClosedAt.Add(-time.Second) },
	} {
		st := lot.State()
		change(&st)
		if _, err := RestoreCargoLot(st); !errors.Is(err, ErrInvalidLot) || !strings.Contains(err.Error(), "reopen") {
			t.Errorf("%s: restored: %v", name, err)
		}
	}
	// closed again after the reopen, as a cancelled shipment's lot is, it carries the later close
	if st := lot.State(); len(st.Reopens) != 1 {
		t.Fatalf("reopens = %+v", st.Reopens)
	}
}

func TestReworkEventsAreTheTypesThePublishedContractNames(t *testing.T) {
	y := newYard(t)
	at := t0.Add(4 * time.Hour)
	for _, id := range []ShipmentID{"SH-1", "SH-2"} {
		y.stuffWhole(t, y.x, id, y.lot[id], at)
	}
	if err := y.x.Seal(y.c, seal("SEAL-1", at), "ops", at); err != nil {
		t.Fatal(err)
	}
	if err := y.lot["SH-2"].Close(10, 10, y.intent["SH-2"], "ops", at); err != nil {
		t.Fatal(err)
	}
	if err := y.x.OpenForRework(y.c, "r", "ops", reworkAt); err != nil {
		t.Fatal(err)
	}
	y.unload(t, "SH-2")
	if err := y.x.Seal(y.c, seal("SEAL-2", resealAt), "ops", resealAt); err != nil {
		t.Fatal(err)
	}
	published := map[string]bool{"LoadExecutionReworkOpened.v1": true, "CargoLotReopened.v1": true, "CargoUnloaded.v1": true, "LoadExecutionClosed.v1": true,
		"LoadExecutionOpened.v1": true, "LoadingStarted.v1": true, "CargoLoaded.v1": true, "CargoReceived.v1": true, "CargoMeasured.v1": true,
		"CargoReceiptCompleted.v1": true, "CargoLotClosed.v1": true}
	emitted := map[string]bool{}
	for _, e := range append(y.x.Pending(), y.lot["SH-2"].Pending()...) {
		if !published[e.Type] {
			t.Errorf("event type %s is not published by package api", e.Type)
		}
		emitted[e.Type] = true
	}
	for _, want := range []string{"LoadExecutionReworkOpened.v1", "CargoLotReopened.v1"} {
		if !emitted[want] {
			t.Errorf("no %s was emitted", want)
		}
	}
}

// A lot closed before its receipt was completed (a cancelled shipment's) goes back to RECEIVING, not RECEIVED.
func TestAReopenedLotGoesBackToTheStateItHadBeforeItWasClosed(t *testing.T) {
	y := newYard(t)
	st := y.lot["SH-1"].State()
	st.State, st.CompletedAt, st.CompletedBy, st.CompletedEnteredAt, st.Measurements = LotClosed, time.Time{}, "", time.Time{}, nil
	st.ClosedBy, st.ClosedAt = "ops", t0.Add(4*time.Hour)
	l, err := RestoreCargoLot(st)
	if err != nil {
		t.Fatal(err)
	}
	l.reopenForRework("X-1", 1, 1, "ops", t0.Add(5*time.Hour))
	if l.Current() != LotReceiving {
		t.Fatalf("reopened to %s", l.Current())
	}
}

// U+0131 (dotless i) and U+017F (long s) have the capitals I and S, so SEA keys them as I and S, and so does ours.
func TestSealsKeyedWithTheLettersWhoseCapitalsAreAsciiAreTheSameSeal(t *testing.T) {
	for broken, again := range map[string]string{"SEALI1": "SEAL\u01311", "SEALS1": "SEAL\u017f1", "SEAL\u01311": "SEALI1", "SEAL\u017f1": "SEALS1"} {
		y := newYard(t)
		at := t0.Add(4 * time.Hour)
		y.stuffWhole(t, y.x, "SH-1", y.lot["SH-1"], at)
		y.stuffWhole(t, y.x, "SH-2", y.lot["SH-2"], at)
		if err := y.x.Seal(y.c, seal(broken, at), "ops", at); err != nil {
			t.Fatal(err)
		}
		if err := y.x.OpenForRework(y.c, "r", "ops", reworkAt); err != nil {
			t.Fatal(err)
		}
		y.unload(t, "SH-2")
		if err := y.x.Seal(y.c, seal(again, resealAt), "ops", resealAt); !errors.Is(err, ErrInvalidEvidence) || !strings.Contains(err.Error(), "was on this container before") {
			t.Fatalf("%q after %q: %v", again, broken, err)
		}
	}
}
