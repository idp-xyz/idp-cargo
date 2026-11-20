package domain

import (
	"errors"
	"testing"
	"time"
)

// A house with no live packs leaves a CLOSED sea consolidation whether or not a rework unloaded it (V2.87). The caller
// hands the domain the rework when it found one; with none the entry records none. A CLOSED air consolidation still
// lets a house go only after a rework.

// shutOutHouse is the plan of a house planned by SEA from Shanghai to Hamburg, as seaConsol makes the consolidation.
func shutOutHouse(shipment ShipmentID) LoadIntent {
	i := intent(shipment, 1)
	i.Mode, i.Route = ModeSea, shgHAM
	return i
}

// closedSea returns a CLOSED sea consolidation of the kind with the house SH-1 attached, its ledger, and an OPEN sea
// consolidation of the same kind to transfer to.
func closedSea(t *testing.T, kind Kind) (c, target *Consolidation, m *Membership) {
	t.Helper()
	c, target = seaConsol(t, "C-1", kind), seaConsol(t, "C-2", kind)
	m, _ = NewMembership("T", "SH-1")
	if err := m.Attach(c, shutOutHouse("SH-1"), "ops-1", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(scopes(1), t0.Add(time.Hour), "", "ops-1", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	return c, target, m
}

func TestAHouseWithNoReworkLeavesAClosedSeaConsolidation(t *testing.T) {
	later := t0.Add(6 * time.Hour)
	for _, kind := range []Kind{KindSeaLCLOwn, KindSeaLCLCoload} {
		// each reason an operator gives
		for _, reason := range []Reason{ReasonRollover, ReasonShutOut, ReasonSpaceShortage, ReasonCustomerRequest, ReasonWrongConsol, ReasonFlightOrVesselChange, ReasonCarrierChange} {
			c, _, m := closedSea(t, kind)
			if err := m.Detach(c, reason, "", "ops-1", later); err != nil {
				t.Fatalf("%s, %s: %v", kind, reason, err)
			}
			if _, live := m.Live(); live {
				t.Fatalf("%s, %s: the house is still attached", kind, reason)
			}
		}
		// the reasons an operator does not give
		for name, tc := range map[string]struct {
			reason Reason
			text   string
		}{"OTHER without a text": {ReasonOther, ""}, "SHIPMENT_CANCELLED is PHY's own": {ReasonShipmentCancelled, ""}, "NO_SUCH_REASON": {"NO_SUCH_REASON", ""}, "no reason": {"", ""}} {
			c, _, m := closedSea(t, kind)
			if err := m.Detach(c, tc.reason, tc.text, "ops-1", later); !errors.Is(err, ErrInvalidReason) {
				t.Errorf("%s, %s: %v", kind, name, err)
			}
		}
	}
	c, _, m := closedSea(t, KindSeaLCLOwn)
	if err := m.Detach(c, ReasonOther, "shut out at the CFS gate", "ops-1", later); err != nil {
		t.Fatalf("OTHER with a text: %v", err)
	}
	// the entry records the reason and no rework; the event is the one of every detach
	e := m.State().Entries[0]
	if e.End != EndDetached || e.Reason != ReasonOther || e.ReasonText != "shut out at the CFS gate" || e.ReworkExecutionID != "" || e.ReworkNo != 0 || e.EndedBy != "ops-1" || !e.EndedAt.Equal(later) {
		t.Fatalf("the ended entry records no rework: %+v", e)
	}
	if p := m.Pending(); len(p) != 2 || p[1].Type != eventHouseDetached || p[1].Reason != string(ReasonOther) || p[1].ConsolidationID != "C-1" {
		t.Fatalf("the event is HouseDetached.v1 as for every detach: %+v", p)
	}
	if _, err := RestoreMembership(m.State()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	// nobody, or a time before the ledger's last change, is refused as for any change; and not a second time
	c, _, m = closedSea(t, KindSeaLCLOwn)
	if err := m.Detach(c, ReasonShutOut, "", "", later); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("nobody: %v", err)
	}
	if err := m.Detach(c, ReasonShutOut, "", "ops-1", t0); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("before the ledger's last change: %v", err)
	}
	if err := m.Detach(c, ReasonShutOut, "", "ops-1", later); err != nil {
		t.Fatal(err)
	}
	if err := m.Detach(c, ReasonShutOut, "", "ops-1", later.Add(time.Hour)); !errors.Is(err, ErrNotAttached) {
		t.Fatalf("a second time: %v", err)
	}
}

func TestAHouseOfAClosedAirConsolidationWithNoReworkStaysFrozen(t *testing.T) {
	later := t0.Add(6 * time.Hour)
	c, target, m := closedWith(t) // air
	if err := m.Detach(c, ReasonShutOut, "", "ops-1", later); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("detach: %v", err)
	}
	if err := m.Transfer(c, target, intent("SH-1", 1), ReasonShutOut, "", "ops-1", later); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("transfer: %v", err)
	}
	if e := m.State().Entries[0]; !e.Live() || len(m.State().Entries) != 1 {
		t.Fatalf("the ledger is as it was: %+v", m.State().Entries)
	}
	// with the rework that unloaded it, a house of an air consolidation leaves, as since V2.86
	if err := m.DetachAfterRework(c, reworkOne, ReasonShutOut, "", "ops-1", later); err != nil {
		t.Fatalf("detach after a rework: %v", err)
	}
}

func TestAHouseWithNoReworkIsTransferredFromAClosedSeaConsolidation(t *testing.T) {
	later := t0.Add(6 * time.Hour)
	c, target, m := closedSea(t, KindSeaLCLOwn)
	if err := m.Transfer(c, c, shutOutHouse("SH-1"), ReasonShutOut, "", "ops-1", later); !errors.Is(err, ErrSameConsolidation) {
		t.Fatalf("the same consolidation: %v", err)
	}
	// a CLOSED target
	closedTarget := seaConsol(t, "C-4", KindSeaLCLOwn)
	other, _ := NewMembership("T", "SH-7")
	if err := other.Attach(closedTarget, shutOutHouse("SH-7"), "ops-1", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := closedTarget.Close(scopes(1), t0.Add(time.Hour), "", "ops-1", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.Transfer(c, closedTarget, shutOutHouse("SH-1"), ReasonShutOut, "", "ops-1", later); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("to a CLOSED target: %v", err)
	}
	// a RECONCILED target
	reconciled := seaConsol(t, "C-6", KindSeaLCLOwn)
	r, _ := NewMembership("T", "SH-8")
	if err := r.Attach(reconciled, shutOutHouse("SH-8"), "ops-1", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := reconciled.Close(scopes(1), t0.Add(time.Hour), "", "ops-1", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	entry, _ := r.Live()
	planned := shutOutHouse("SH-8")
	planned.ConsolidationID = "C-6"
	if err := reconciled.Reconcile([]MemberCheck{{Entry: entry, Intent: planned, Loaded: 6}}, 0, 0, "ops", t0.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.Transfer(c, reconciled, shutOutHouse("SH-1"), ReasonShutOut, "", "ops-1", later); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("to a RECONCILED target: %v", err)
	}
	// a target of another mode, and a target that does not admit the house
	if err := m.Transfer(c, airConsol(t, "C-9"), shutOutHouse("SH-1"), ReasonShutOut, "", "ops-1", later); !errors.Is(err, ErrModeMismatch) {
		t.Fatalf("to an air target: %v", err)
	}
	if err := m.Transfer(c, target, intent("SH-1", 1), ReasonShutOut, "", "ops-1", later); !errors.Is(err, ErrModeMismatch) {
		t.Fatalf("to a target that does not admit the house: %v", err)
	}
	if err := m.Transfer(c, target, shutOutHouse("SH-1"), ReasonOther, "", "ops-1", later); !errors.Is(err, ErrInvalidReason) {
		t.Fatalf("OTHER without a text: %v", err)
	}
	if err := m.Transfer(c, target, shutOutHouse("SH-1"), ReasonShipmentCancelled, "", "ops-1", later); !errors.Is(err, ErrInvalidReason) {
		t.Fatalf("SHIPMENT_CANCELLED is PHY's own: %v", err)
	}
	if len(m.State().Entries) != 1 || !m.State().Entries[0].Live() {
		t.Fatalf("every refusal left the ledger as it was: %+v", m.State().Entries)
	}
	if err := m.Transfer(c, target, shutOutHouse("SH-1"), ReasonWrongConsol, "", "ops-1", later); err != nil {
		t.Fatal(err)
	}
	es := m.State().Entries
	if len(es) != 2 || es[0].End != EndTransferred || es[0].Successor != 2 || es[0].Reason != ReasonWrongConsol || es[0].ReworkExecutionID != "" || es[0].ReworkNo != 0 ||
		es[1].Start != StartTransferred || es[1].Predecessor != 1 || es[1].ConsolidationID != "C-2" || !es[1].Live() {
		t.Fatalf("the history, the entry in C-1 recording no rework: %+v", es)
	}
	if p := m.Pending(); len(p) != 2 || p[1].Type != eventHouseTransferred || p[1].FromConsolidationID != "C-1" || p[1].ConsolidationID != "C-2" {
		t.Fatalf("the event is HouseTransferred.v1 as today: %+v", p)
	}
	if _, err := RestoreMembership(m.State()); err != nil {
		t.Fatalf("restore: %v", err)
	}
}

// TestTheEntryRecordsAReworkOnlyWhenTheCallerFoundOne: the same CLOSED sea consolidation, the same house; the caller's
// rework is what the entry records, and with none it records none. A RECONCILED and a CANCELLED consolidation refuse
// with or without one.
func TestTheEntryRecordsAReworkOnlyWhenTheCallerFoundOne(t *testing.T) {
	later := t0.Add(6 * time.Hour)
	c, _, withRework := closedSea(t, KindSeaLCLOwn)
	if err := withRework.DetachAfterRework(c, reworkOne, ReasonShutOut, "", "ops-1", later); err != nil {
		t.Fatal(err)
	}
	if e := withRework.State().Entries[0]; e.ReworkExecutionID != "X-1" || e.ReworkNo != 1 {
		t.Fatalf("with a rework: %+v", e)
	}
	// a rework dated after the leaving still refuses, as in V2.86
	c, _, m := closedSea(t, KindSeaLCLOwn)
	if err := m.DetachAfterRework(c, reworkOne, ReasonShutOut, "", "ops-1", reworkOne.UnloadedAt.Add(-time.Second)); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("before the unloading: %v", err)
	}
	if err := m.Detach(c, ReasonShutOut, "", "ops-1", reworkOne.UnloadedAt.Add(-time.Second)); err != nil {
		t.Fatalf("with no rework there is no unloading to be dated after: %v", err)
	}
	if e := m.State().Entries[0]; e.ReworkExecutionID != "" || e.ReworkNo != 0 {
		t.Fatalf("without: %+v", e)
	}
	// RECONCILED and CANCELLED
	rc, _, rm := closedSea(t, KindSeaLCLOwn)
	entry, _ := rm.Live()
	allocated := shutOutHouse("SH-1")
	allocated.ConsolidationID = "C-1"
	if err := rc.Reconcile([]MemberCheck{{Entry: entry, Intent: allocated, Loaded: 6}}, 0, 0, "ops", t0.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := rm.Detach(rc, ReasonShutOut, "", "ops-1", later); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("RECONCILED: %v", err)
	}
	cancelled := seaConsol(t, "C-3", KindSeaLCLOwn)
	other, _ := NewMembership("T", "SH-5")
	if err := cancelled.Cancel("no cargo", "ops-1", t0.Add(time.Hour), 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := other.Detach(cancelled, ReasonShutOut, "", "ops-1", later); !errors.Is(err, ErrNotAttached) {
		t.Fatalf("a house that is not in the CANCELLED consolidation: %v", err)
	}
	// an OPEN consolidation behaves as it did, with a rework named or not
	open := seaConsol(t, "C-7", KindSeaLCLOwn)
	om, _ := NewMembership("T", "SH-6")
	if err := om.Attach(open, shutOutHouse("SH-6"), "ops-1", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := om.Detach(open, ReasonShutOut, "", "ops-1", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if e := om.State().Entries[0]; e.ReworkNo != 0 {
		t.Fatalf("OPEN: %+v", e)
	}
}

// TestALeavingWithNoReworkIsNotDatedBeforeTheHousesLatestReversal: the caller hands the business time of the house's
// latest reversal in the consolidation; the leaving may be at that time, not before it, and a house never loaded hands
// the zero time.
func TestALeavingWithNoReworkIsNotDatedBeforeTheHousesLatestReversal(t *testing.T) {
	reversed := t0.Add(5 * time.Hour)
	for _, kind := range []Kind{KindSeaLCLOwn, KindSeaLCLCoload} {
		c, target, m := closedSea(t, kind)
		if err := m.DetachAfterReversals(c, reversed, ReasonShutOut, "", "ops-1", reversed.Add(-time.Nanosecond)); !errors.Is(err, ErrInvalidChange) {
			t.Fatalf("%s, a detach before the reversal: %v", kind, err)
		}
		if err := m.TransferAfterReversals(c, target, shutOutHouse("SH-1"), reversed, ReasonShutOut, "", "ops-1", reversed.Add(-time.Nanosecond)); !errors.Is(err, ErrInvalidChange) {
			t.Fatalf("%s, a transfer before the reversal: %v", kind, err)
		}
		if err := m.DetachAfterReversals(c, reversed, ReasonShutOut, "", "ops-1", time.Time{}); !errors.Is(err, ErrInvalidChange) {
			t.Fatalf("%s, no time: %v", kind, err)
		}
		if len(m.State().Entries) != 1 || !m.State().Entries[0].Live() {
			t.Fatalf("%s, every refusal left the ledger as it was: %+v", kind, m.State().Entries)
		}
		if err := m.DetachAfterReversals(c, reversed, ReasonShutOut, "", "ops-1", reversed); err != nil {
			t.Fatalf("%s, a detach at the time of the reversal: %v", kind, err)
		}
		c, target, m = closedSea(t, kind)
		if err := m.TransferAfterReversals(c, target, shutOutHouse("SH-1"), reversed, ReasonShutOut, "", "ops-1", reversed); err != nil {
			t.Fatalf("%s, a transfer at the time of the reversal: %v", kind, err)
		}
		c, _, m = closedSea(t, kind)
		if err := m.DetachAfterReversals(c, time.Time{}, ReasonShutOut, "", "ops-1", t0.Add(3*time.Hour)); err != nil {
			t.Fatalf("%s, a house never loaded: %v", kind, err)
		}
	}
	// an air consolidation stays frozen, and an OPEN one does not look at the time
	ac, atarget, am := closedWith(t)
	if err := am.DetachAfterReversals(ac, time.Time{}, ReasonShutOut, "", "ops-1", t0.Add(6*time.Hour)); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("air: %v", err)
	}
	if err := am.TransferAfterReversals(ac, atarget, intent("SH-1", 1), time.Time{}, ReasonShutOut, "", "ops-1", t0.Add(6*time.Hour)); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("air, transfer: %v", err)
	}
	open := seaConsol(t, "C-7", KindSeaLCLOwn)
	om, _ := NewMembership("T", "SH-6")
	if err := om.Attach(open, shutOutHouse("SH-6"), "ops-1", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := om.DetachAfterReversals(open, t0.Add(9*time.Hour), ReasonShutOut, "", "ops-1", t0.Add(2*time.Hour)); err != nil {
		t.Fatalf("OPEN: %v", err)
	}
}
