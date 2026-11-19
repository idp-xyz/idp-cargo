package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Giving up a stuffing (CancelLoadExecution): each rule of the command, the terminal state and the unit afterwards.

var cancelAt = t0.Add(5 * time.Hour)

// freshStuffing returns the consolidation, the house's entry, the lot (10 cartons of 3,000 kg) and a stuffing of unit 1
// in the state given: OPEN, LOADING (started) or ON_HOLD (a customs hold placed), with nothing loaded.
func freshStuffing(t *testing.T, state ExecutionState) (*Consolidation, Entry, *CargoLot, *LoadExecution) {
	t.Helper()
	c, entry, lot, x := stuffing(t, 30000)
	at := t0.Add(4 * time.Hour)
	switch state {
	case ExecutionOpen:
		var err error
		if x, err = OpenStuffing("T", "X-2", c, 1, cfs, false, "ops", t0.Add(3*time.Hour)); err != nil {
			t.Fatal(err)
		}
	case ExecutionOnHold:
		if err := x.PlaceHold(HoldCustoms, "", ev("CUSTOMS-1", at), "ops", at); err != nil {
			t.Fatal(err)
		}
	}
	if x.Current() != state {
		t.Fatalf("stuffing is %s, want %s", x.Current(), state)
	}
	return c, entry, lot, x
}

func load(t *testing.T, x *LoadExecution, c *Consolidation, lot *CargoLot, entry Entry, packs int64, already LotLoad, at time.Time) {
	t.Helper()
	if err := x.Load(LoadLine{Packs: packs, Evidence: ev("TALLY-"+at.Format("150405.000"), at)}, c, lot, entry, already, already.Packs, "ops", at); err != nil {
		t.Fatal(err)
	}
}

func TestCancelAStuffingWithNoLivePacks(t *testing.T) {
	for _, state := range []ExecutionState{ExecutionOpen, ExecutionLoading, ExecutionOnHold} {
		t.Run(string(state)+" and empty", func(t *testing.T) {
			_, _, _, x := freshStuffing(t, state)
			was := len(x.Pending())
			if err := x.Cancel("the container was rolled", "ops-2", cancelAt); err != nil {
				t.Fatal(err)
			}
			st := x.State()
			if x.Current() != ExecutionCancelled || st.CancelledBy != "ops-2" || !st.CancelledAt.Equal(cancelAt) || st.CancelReason != "the container was rolled" {
				t.Fatalf("state = %s, %+v", x.Current(), st)
			}
			events := x.Pending()
			if len(events) != was+1 || events[was].Type != "LoadExecutionCancelled.v1" || events[was].ExecutionID != "X-2" && events[was].ExecutionID != "X-1" ||
				!events[was].At.Equal(cancelAt) || events[was].Reason != "" || events[was].ConsolidationID != "C-S" || events[was].ItemNo != 1 {
				t.Fatalf("events = %+v", events)
			}
			if st.OpenedBy != "ops" || st.LoadUnitNo != 1 || st.Kind != ExecutionStuffing {
				t.Fatalf("the stuffing keeps what it was: %+v", st)
			}
			x.Stamp(cancelAt.Add(time.Minute))
			if got := x.State().CancelEnteredAt; !got.Equal(cancelAt.Add(time.Minute)) {
				t.Fatalf("entered at %v", got)
			}
			if back, err := RestoreLoadExecution(x.State()); err != nil || back.Current() != ExecutionCancelled {
				t.Fatalf("a cancelled stuffing restores: %v", err)
			}
		})
	}
	// an open hold ends with the execution and stays recorded as placed: nothing is loaded to release
	_, _, _, held := freshStuffing(t, ExecutionOnHold)
	if err := held.Cancel("rolled", "ops", cancelAt); err != nil {
		t.Fatal(err)
	}
	if h := held.State().Holds; len(h) != 1 || !h[0].Placed() || held.Current() != ExecutionCancelled {
		t.Fatalf("holds = %+v", h)
	}
}

func TestCancelAStuffingThatHeldPacksAndNoLongerDoes(t *testing.T) {
	c, entry, lot, x := freshStuffing(t, ExecutionLoading)
	at := t0.Add(4 * time.Hour)
	load(t, x, c, lot, entry, 4, LotLoad{}, at)
	if err := x.Cancel("rolled", "ops", cancelAt); !errors.Is(err, ErrInvalidExecution) || !strings.Contains(err.Error(), "4 packs are still loaded") {
		t.Fatalf("a stuffing with live packs: %v", err)
	}
	if err := x.Unload(1, 3, ReversalMisloaded, ev("UN-1", at), lot, "ops", at); err != nil {
		t.Fatal(err)
	}
	if err := x.Cancel("rolled", "ops", cancelAt); !errors.Is(err, ErrInvalidExecution) || !strings.Contains(err.Error(), "1 packs are still loaded") {
		t.Fatalf("a pack still live: %v", err)
	}
	if x.Current() != ExecutionLoading || len(x.Pending()) != 4 {
		t.Fatalf("a refused cancellation changes nothing: %s, %d events", x.Current(), len(x.Pending()))
	}
	if err := x.Unload(1, 1, ReversalMisloaded, ev("UN-2", at), lot, "ops", at); err != nil {
		t.Fatal(err)
	}
	if err := x.Cancel("rolled", "ops", cancelAt); err != nil {
		t.Fatalf("every loaded pack unloaded again: %v", err)
	}
	st := x.State()
	if len(st.Lines) != 1 || len(st.Reversals) != 2 || x.Tendered() != 0 {
		t.Fatalf("a cancelled stuffing keeps its lines and reversals as history: %+v", st)
	}
	// the unit takes a new stuffing: the domain asks for one opened on a unit that has none that is not cancelled
	if y, err := OpenStuffing("T", "X-9", c, 1, cfs, false, "ops", cancelAt); err != nil || y.State().LoadUnitNo != 1 {
		t.Fatalf("a new stuffing on the unit: %v", err)
	}
	if _, err := OpenStuffing("T", "X-9", c, 1, cfs, true, "ops", cancelAt); !errors.Is(err, ErrLoadUnitInUse) {
		t.Fatalf("a unit with a stuffing that is not cancelled takes no other: %v", err)
	}
}

func TestCancelRefusals(t *testing.T) {
	at := t0.Add(4 * time.Hour)
	sealed := func(t *testing.T) *LoadExecution {
		c, entry, lot, x := freshStuffing(t, ExecutionLoading)
		load(t, x, c, lot, entry, 1, LotLoad{}, at)
		if err := x.Seal(c, seal("SEAL-1", at), "ops", at); err != nil {
			t.Fatal(err)
		}
		return x
	}
	cancelled := func(t *testing.T) *LoadExecution {
		_, _, _, x := freshStuffing(t, ExecutionLoading)
		if err := x.Cancel("rolled", "ops", cancelAt); err != nil {
			t.Fatal(err)
		}
		return x
	}
	tenderOf := func(t *testing.T) *LoadExecution { _, _, x := tender(t); return x }
	closedTender := func(t *testing.T) *LoadExecution {
		c, entry, x := tender(t)
		lot := received(t)
		if err := x.Load(LoadLine{Packs: 6, Evidence: ev("TALLY", t0.Add(4*time.Hour))}, c, lot, entry, LotLoad{}, 0, "ops", t0.Add(4*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := x.Close(ev("RCPT-1", t0.Add(5*time.Hour)), "ops", t0.Add(5*time.Hour)); err != nil {
			t.Fatal(err)
		}
		return x
	}
	long := strings.Repeat("x", 501)
	for name, tc := range map[string]struct {
		x      func(*testing.T) *LoadExecution
		reason string
		by     string
		at     time.Time
		want   error
		says   string
	}{
		"a sealed container":   {sealed, "rolled", "ops", cancelAt, ErrInvalidTransition, "is CLOSED"},
		"a cancelled stuffing": {cancelled, "again", "ops", cancelAt.Add(time.Hour), ErrInvalidTransition, "is CANCELLED"},
		"a tender":             {tenderOf, "rolled", "ops", cancelAt, ErrInvalidExecution, "only a stuffing"},
		"a closed tender":      {closedTender, "rolled", "ops", cancelAt, ErrInvalidExecution, "only a stuffing"},
		"no reason":            {func(t *testing.T) *LoadExecution { _, _, _, x := freshStuffing(t, ExecutionLoading); return x }, "", "ops", cancelAt, ErrInvalidExecution, "reason"},
		"a blank reason":       {func(t *testing.T) *LoadExecution { _, _, _, x := freshStuffing(t, ExecutionLoading); return x }, " \t", "ops", cancelAt, ErrInvalidExecution, "reason"},
		"a reason over 500":    {func(t *testing.T) *LoadExecution { _, _, _, x := freshStuffing(t, ExecutionLoading); return x }, long, "ops", cancelAt, ErrInvalidExecution, "reason"},
		"no actor":             {func(t *testing.T) *LoadExecution { _, _, _, x := freshStuffing(t, ExecutionLoading); return x }, "rolled", " ", cancelAt, ErrInvalidChange, ""},
		"no time":              {func(t *testing.T) *LoadExecution { _, _, _, x := freshStuffing(t, ExecutionLoading); return x }, "rolled", "ops", time.Time{}, ErrInvalidChange, ""},
		"before the start":     {func(t *testing.T) *LoadExecution { _, _, _, x := freshStuffing(t, ExecutionLoading); return x }, "rolled", "ops", t0.Add(3*time.Hour - time.Second), ErrInvalidChange, ""},
		"before the opening":   {func(t *testing.T) *LoadExecution { _, _, _, x := freshStuffing(t, ExecutionOpen); return x }, "rolled", "ops", t0.Add(3*time.Hour - time.Second), ErrInvalidChange, ""},
		"before the last unload": {func(t *testing.T) *LoadExecution {
			c, entry, lot, x := freshStuffing(t, ExecutionLoading)
			load(t, x, c, lot, entry, 2, LotLoad{}, at)
			if err := x.Unload(1, 2, ReversalMisloaded, ev("UN-1", at.Add(time.Hour)), lot, "ops", at.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			return x
		}, "rolled", "ops", at.Add(time.Minute), ErrInvalidChange, ""},
		"before the hold": {func(t *testing.T) *LoadExecution { _, _, _, x := freshStuffing(t, ExecutionOnHold); return x }, "rolled", "ops", t0.Add(3*time.Hour + time.Minute), ErrInvalidChange, ""},
	} {
		t.Run(name, func(t *testing.T) {
			x := tc.x(t)
			state, events := x.Current(), len(x.Pending())
			err := x.Cancel(tc.reason, tc.by, tc.at)
			if !errors.Is(err, tc.want) || (tc.says != "" && !strings.Contains(err.Error(), tc.says)) {
				t.Fatalf("Cancel = %v, want %v saying %q", err, tc.want, tc.says)
			}
			if x.Current() != state || len(x.Pending()) != events {
				t.Fatalf("a refused cancellation changes nothing: %s, %d events", x.Current(), len(x.Pending()))
			}
		})
	}
	// 500 characters are enough, counted as characters and not as bytes
	_, _, _, x := freshStuffing(t, ExecutionLoading)
	if err := x.Cancel(strings.Repeat("é", 500), "ops", cancelAt); err != nil {
		t.Fatalf("a reason of 500 characters: %v", err)
	}
}

// A cancelled stuffing is terminal: it takes no command, keeps what it was and adds nothing.
func TestACancelledStuffingTakesNoFurtherCommand(t *testing.T) {
	c, entry, lot, x := freshStuffing(t, ExecutionOnHold)
	if err := x.Cancel("rolled", "ops", cancelAt); err != nil {
		t.Fatal(err)
	}
	before := x.State()
	events := len(x.Pending())
	later := cancelAt.Add(time.Hour)
	for name, err := range map[string]error{
		"start":      x.Start("ops", later),
		"load":       x.Load(LoadLine{Packs: 1, Evidence: ev("T", later)}, c, lot, entry, LotLoad{}, 0, "ops", later),
		"unload":     x.Unload(1, 1, ReversalMisloaded, ev("U", later), lot, "ops", later),
		"hold":       x.PlaceHold(HoldDamage, "", ev("H", later), "ops", later),
		"release":    x.ReleaseHold(1, ev("R", later), "", "ops", later),
		"seal":       x.Seal(c, seal("S", later), "ops", later),
		"close":      x.Close(ev("C", later), "ops", later),
		"cancel":     x.Cancel("again", "ops", later),
		"resolution": x.ResolveDiscrepancy(1, ResolvedByAccepted, "", "ok", "ops", later),
	} {
		if err == nil {
			t.Errorf("%s: accepted on a cancelled stuffing", name)
		}
	}
	if _, _, err := x.RecordCarrierEvidence(CarrierEvidence{Fact: CarrierAccepted, Pieces: 1, Transport: "V1", Evidence: ev("CE", later)}, "ops", later); err == nil {
		t.Error("carrier evidence: accepted on a cancelled stuffing")
	}
	if x.HoldForScreening("L-1", ev("SCR", later), "ops", later) || x.HoldIfOverLimits(c, map[CargoLotID]*CargoLot{lot.ID(): lot}, ev("L", later), "ops", later) {
		t.Error("a cancelled stuffing is not held again")
	}
	if got := x.State(); got.State != ExecutionCancelled || len(got.Holds) != len(before.Holds) || !got.Holds[0].Placed() || got.CancelReason != "rolled" ||
		len(got.Lines) != 0 || len(x.Pending()) != events {
		t.Fatalf("a cancelled stuffing changed: %+v", got)
	}
}

func TestACancelledStuffingIsRestoredOnlyAsOne(t *testing.T) {
	_, _, _, x := freshStuffing(t, ExecutionLoading)
	if err := x.Cancel("rolled", "ops", cancelAt); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*LoadExecutionState){
		"without a reason":  func(s *LoadExecutionState) { s.CancelReason = "" },
		"without an author": func(s *LoadExecutionState) { s.CancelledBy = "" },
		"without a time":    func(s *LoadExecutionState) { s.CancelledAt = time.Time{} },
		"before it opened":  func(s *LoadExecutionState) { s.CancelledAt = s.OpenedAt.Add(-time.Second) },
		"before it started": func(s *LoadExecutionState) { s.CancelledAt = s.StartedAt.Add(-time.Second) },
		"as a tender":       func(s *LoadExecutionState) { s.Kind, s.LoadUnitNo = ExecutionTender, 0 },
		"also closed":       func(s *LoadExecutionState) { s.ClosedAt, s.ClosedBy = cancelAt, "ops" },
		"with carrier proof": func(s *LoadExecutionState) {
			s.CarrierEvidence = []CarrierEvidence{{No: 1, Fact: CarrierAccepted, Pieces: 1, By: "ops"}}
		},
		"with live packs": func(s *LoadExecutionState) {
			s.Lines = []LoadLine{{No: 1, ShipmentID: "SH-1", LotID: "L-1", EntryNo: 1, Packs: 1, GrossWeight: kg(1), Basis: BasisApportioned, MeasurementNo: 1, By: "ops"}}
		},
	} {
		st := x.State()
		change(&st)
		if _, err := RestoreLoadExecution(st); !errors.Is(err, ErrInvalidExecution) {
			t.Errorf("%s: restored: %v", name, err)
		}
	}
	st := x.State()
	st.State = ExecutionLoading
	if _, err := RestoreLoadExecution(st); !errors.Is(err, ErrInvalidExecution) {
		t.Errorf("cancellation fields on a loading stuffing: %v", err)
	}
	// each cancellation fact alone on a stuffing that is not cancelled, and each missing on one that is
	for name, change := range map[string]func(*LoadExecutionState){
		"a reason alone":  func(s *LoadExecutionState) { s.CancelledBy, s.CancelledAt = "", time.Time{} },
		"an author alone": func(s *LoadExecutionState) { s.CancelReason, s.CancelledAt = "", time.Time{} },
		"a time alone":    func(s *LoadExecutionState) { s.CancelReason, s.CancelledBy = "", "" },
	} {
		st = x.State()
		st.State = ExecutionLoading
		change(&st)
		if _, err := RestoreLoadExecution(st); !errors.Is(err, ErrInvalidExecution) {
			t.Errorf("%s on a loading stuffing: %v", name, err)
		}
	}
	// a stuffing cancelled while OPEN never started
	_, _, _, open := freshStuffing(t, ExecutionOpen)
	if err := open.Cancel("never used", "ops", cancelAt); err != nil {
		t.Fatal(err)
	}
	if back, err := RestoreLoadExecution(open.State()); err != nil || !back.State().StartedAt.IsZero() {
		t.Fatalf("restoring a stuffing cancelled while OPEN: %v", err)
	}
}

// A cancelled stuffing holds nothing and its last change is the cancellation, which a reopen of the consolidation is
// dated not before (Stuffed.Last is built from it).
func TestACancelledStuffingHoldsNothing(t *testing.T) {
	_, _, _, x := freshStuffing(t, ExecutionLoading)
	if err := x.Cancel("rolled", "ops", cancelAt); err != nil {
		t.Fatal(err)
	}
	if x.Tendered() != 0 || x.LastChange() != cancelAt {
		t.Fatalf("tendered %d, last change %v", x.Tendered(), x.LastChange())
	}
}

// A new stuffing of a unit, and the unit's removal, are dated not before the latest cancellation of a stuffing of it.
func TestAUnitIsStuffedOrRemovedNotBeforeTheCancellation(t *testing.T) {
	c, _, _, _ := freshStuffing(t, ExecutionLoading)
	if _, err := OpenStuffingAfter("T", "X-9", c, 1, cfs, false, cancelAt, "ops", cancelAt.Add(-time.Second)); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("a stuffing dated before the cancellation: %v", err)
	}
	if _, err := OpenStuffingAfter("T", "X-9", c, 1, cfs, false, cancelAt, "ops", cancelAt); err != nil {
		t.Fatalf("a stuffing dated at the cancellation: %v", err)
	}
	if _, err := OpenStuffingAfter("T", "X-9", c, 1, cfs, false, time.Time{}, "ops", cancelAt); err != nil {
		t.Fatalf("a unit never cancelled: %v", err)
	}
	if err := c.RemovePlannedLoadUnitAfter(1, "rolled", "ops", cancelAt.Add(-time.Second), false, cancelAt); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("a removal dated before the cancellation: %v", err)
	}
	if err := c.RemovePlannedLoadUnitAfter(1, "rolled", "ops", cancelAt, false, cancelAt); err != nil {
		t.Fatalf("a removal dated at the cancellation: %v", err)
	}
}
