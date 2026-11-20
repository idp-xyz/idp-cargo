package domain

import (
	"errors"
	"testing"
	"time"
)

// A house unloaded whole in the rework of a sealed container leaves the CLOSED consolidation (V2.86). The domain keeps
// what the commands hand it: the rework that unloaded the house, found by the application under the house's ledger
// lock.

var reworkOne = ReworkRef{ExecutionID: "X-1", No: 1, UnloadedAt: t0.Add(5 * time.Hour)}

// closedWith returns a CLOSED air consolidation with the houses SH-1 and SH-2 attached, SH-1's ledger and SH-1's live
// entry, and an OPEN air consolidation C-2 to transfer to.
func closedWith(t *testing.T) (c, target *Consolidation, m *Membership) {
	t.Helper()
	c, target = airConsol(t, "C-1"), airConsol(t, "C-2")
	m, _ = NewMembership("T", "SH-1")
	if err := m.Attach(c, intent("SH-1", 1), "ops-1", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(scopes(2), t0.Add(time.Hour), "", "ops-1", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	return c, target, m
}

func TestAHouseUnloadedInAReworkIsDetachedFromAClosedConsolidation(t *testing.T) {
	later := t0.Add(6 * time.Hour)
	c, _, m := closedWith(t)
	// without a rework the house stays frozen, whatever the reason (the commands pass none when the house was never
	// loaded, or never unloaded in a rework)
	if err := m.Detach(c, ReasonShutOut, "", "ops-1", later); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("a CLOSED consolidation, no rework: %v", err)
	}
	// each reason an operator gives, and the ones it does not
	for _, reason := range []Reason{ReasonRollover, ReasonShutOut, ReasonSpaceShortage, ReasonCustomerRequest, ReasonWrongConsol, ReasonFlightOrVesselChange, ReasonCarrierChange} {
		c, _, m := closedWith(t)
		if err := m.DetachAfterRework(c, reworkOne, reason, "", "ops-1", later); err != nil {
			t.Fatalf("%s: %v", reason, err)
		}
		if _, live := m.Live(); live {
			t.Fatalf("%s: the house is still attached", reason)
		}
	}
	for name, tc := range map[string]struct {
		reason Reason
		text   string
	}{"OTHER without a text": {ReasonOther, ""}, "SHIPMENT_CANCELLED is PHY's own": {ReasonShipmentCancelled, ""}, "NO_SUCH_REASON": {"NO_SUCH_REASON", ""}, "no reason": {"", ""}} {
		c, _, m := closedWith(t)
		if err := m.DetachAfterRework(c, reworkOne, tc.reason, tc.text, "ops-1", later); !errors.Is(err, ErrInvalidReason) {
			t.Errorf("%s: %v", name, err)
		}
	}
	c, _, m = closedWith(t)
	if err := m.DetachAfterRework(c, reworkOne, ReasonOther, "re-routed to the next sailing", "ops-1", later); err != nil {
		t.Fatalf("OTHER with a text: %v", err)
	}
	e := m.State().Entries[0]
	if e.End != EndDetached || e.Reason != ReasonOther || e.ReasonText != "re-routed to the next sailing" || e.ReworkExecutionID != "X-1" || e.ReworkNo != 1 || e.EndedBy != "ops-1" ||
		!e.EndedAt.Equal(later) {
		t.Fatalf("the ended entry: %+v", e)
	}
	if p := m.Pending(); len(p) != 2 || p[1].Type != eventHouseDetached || p[1].Reason != string(ReasonOther) || p[1].ConsolidationID != "C-1" {
		t.Fatalf("the event is HouseDetached.v1 with the reason as today: %+v", p)
	}
	if ended := m.EndedEntries(); len(ended) != 0 {
		t.Fatalf("an entry written in this unit of work is not a stored one ended: %v", ended)
	}
	// the ledger restores with the link, and a link without its pair, or on a live entry or a refresh, is invalid
	if _, err := RestoreMembership(m.State()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for name, mutate := range map[string]func(*Entry){
		"an execution without a number": func(e *Entry) { e.ReworkNo = 0 },
		"a number without an execution": func(e *Entry) { e.ReworkExecutionID = "" },
		"a negative number":             func(e *Entry) { e.ReworkNo = -1 },
		"a blank execution":             func(e *Entry) { e.ReworkExecutionID = " " },
		"a cancelled shipment":          func(e *Entry) { e.End, e.Reason, e.ReasonText = EndShipmentCancelled, ReasonShipmentCancelled, "" },
	} {
		st := m.State()
		mutate(&st.Entries[0])
		if _, err := RestoreMembership(st); !errors.Is(err, ErrInvalidMembership) {
			t.Errorf("%s: restored: %v", name, err)
		}
	}
	open, _ := NewMembership("T", "SH-3")
	if err := open.Attach(airConsol(t, "C-9"), intent("SH-3", 1), "ops-1", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	st := open.State()
	st.Entries[0].ReworkExecutionID, st.Entries[0].ReworkNo = "X-1", 1
	if _, err := RestoreMembership(st); !errors.Is(err, ErrInvalidMembership) {
		t.Errorf("a live entry that names a rework: %v", err)
	}
}

func TestTheReworkOfADetachIsCheckedAgainstTheTimeAndTheStateOfTheConsolidation(t *testing.T) {
	c, _, m := closedWith(t)
	if err := m.DetachAfterRework(c, reworkOne, ReasonShutOut, "", "ops-1", reworkOne.UnloadedAt.Add(-time.Second)); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("leaving before the house was unloaded: %v", err)
	}
	for name, bad := range map[string]ReworkRef{"no execution": {No: 1}, "no number": {ExecutionID: "X-1"}, "a negative number": {ExecutionID: "X-1", No: -1}} {
		bad.UnloadedAt = reworkOne.UnloadedAt
		if err := m.DetachAfterRework(c, bad, ReasonShutOut, "", "ops-1", t0.Add(6*time.Hour)); !errors.Is(err, ErrInvalidChange) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := m.DetachAfterRework(c, reworkOne, ReasonShutOut, "", "", t0.Add(6*time.Hour)); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("nobody: %v", err)
	}
	if err := m.DetachAfterRework(c, reworkOne, ReasonShutOut, "", "ops-1", t0.Add(6*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.DetachAfterRework(c, reworkOne, ReasonShutOut, "", "ops-1", t0.Add(7*time.Hour)); !errors.Is(err, ErrNotAttached) {
		t.Fatalf("a second time: %v", err)
	}
	// a cancelled and a reconciled consolidation refuse, with or without a rework
	cancelled := airConsol(t, "C-3")
	other, _ := NewMembership("T", "SH-5")
	if err := cancelled.Cancel("no cargo", "ops-1", t0.Add(time.Hour), 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := other.DetachAfterRework(cancelled, reworkOne, ReasonShutOut, "", "ops-1", t0.Add(6*time.Hour)); !errors.Is(err, ErrNotAttached) {
		t.Fatalf("a house that is not in the cancelled consolidation: %v", err)
	}
}

func TestAHouseOfAReconciledConsolidationStaysFrozen(t *testing.T) {
	c, _, m := closedWith(t)
	entry, _ := m.Live()
	allocated := intent("SH-1", 1)
	allocated.ConsolidationID = "C-1"
	if err := c.Reconcile([]MemberCheck{{Entry: entry, Intent: allocated, Loaded: 6}}, 0, 0, "ops", t0.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.DetachAfterRework(c, reworkOne, ReasonShutOut, "", "ops-1", t0.Add(6*time.Hour)); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("detach from a RECONCILED consolidation: %v", err)
	}
	if err := m.TransferAfterRework(c, airConsol(t, "C-2"), intent("SH-1", 1), reworkOne, ReasonShutOut, "", "ops-1", t0.Add(6*time.Hour)); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("transfer from a RECONCILED consolidation: %v", err)
	}
}

func TestAHouseUnloadedInAReworkIsTransferredFromAClosedConsolidation(t *testing.T) {
	later := t0.Add(6 * time.Hour)
	c, target, m := closedWith(t)
	if err := m.Transfer(c, target, intent("SH-1", 1), ReasonShutOut, "", "ops-1", later); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("a CLOSED consolidation, no rework: %v", err)
	}
	if err := m.TransferAfterRework(c, c, intent("SH-1", 1), reworkOne, ReasonShutOut, "", "ops-1", later); !errors.Is(err, ErrSameConsolidation) {
		t.Fatalf("the same consolidation: %v", err)
	}
	closedTarget := airConsol(t, "C-4")
	other, _ := NewMembership("T", "SH-7")
	if err := other.Attach(closedTarget, intent("SH-7", 1), "ops-1", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := closedTarget.Close(scopes(1), t0.Add(time.Hour), "", "ops-1", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.TransferAfterRework(c, closedTarget, intent("SH-1", 1), reworkOne, ReasonShutOut, "", "ops-1", later); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("to a CLOSED target: %v", err)
	}
	cancelledTarget := airConsol(t, "C-5")
	if err := cancelledTarget.Cancel("none", "ops-1", t0.Add(time.Hour), 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := m.TransferAfterRework(c, cancelledTarget, intent("SH-1", 1), reworkOne, ReasonShutOut, "", "ops-1", later); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("to a CANCELLED target: %v", err)
	}
	// RECONCILED target: a CLOSED consolidation whose only house is loaded as planned
	reconciled := airConsol(t, "C-6")
	r, _ := NewMembership("T", "SH-8")
	if err := r.Attach(reconciled, intent("SH-8", 1), "ops-1", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := reconciled.Close(scopes(1), t0.Add(time.Hour), "", "ops-1", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	entry, _ := r.Live()
	planned := intent("SH-8", 1)
	planned.ConsolidationID = "C-6"
	if err := reconciled.Reconcile([]MemberCheck{{Entry: entry, Intent: planned, Loaded: 6}}, 0, 0, "ops", t0.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.TransferAfterRework(c, reconciled, intent("SH-1", 1), reworkOne, ReasonShutOut, "", "ops-1", later); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("to a RECONCILED target: %v", err)
	}
	if err := m.TransferAfterRework(c, shgConsol(t), intent("SH-1", 1), reworkOne, ReasonShutOut, "", "ops-1", later); !errors.Is(err, ErrModeMismatch) {
		t.Fatalf("to a target of another mode: %v", err)
	}
	if err := m.TransferAfterRework(c, target, intent("SH-1", 1), reworkOne, ReasonOther, "", "ops-1", later); !errors.Is(err, ErrInvalidReason) {
		t.Fatalf("OTHER without a text: %v", err)
	}
	if len(m.State().Entries) != 1 || m.State().Entries[0].End != "" {
		t.Fatalf("every refusal left the ledger as it was: %+v", m.State().Entries)
	}
	if err := m.TransferAfterRework(c, target, intent("SH-1", 1), reworkOne, ReasonWrongConsol, "", "ops-1", later); err != nil {
		t.Fatal(err)
	}
	es := m.State().Entries
	if len(es) != 2 || es[0].End != EndTransferred || es[0].Successor != 2 || es[0].ReworkExecutionID != "X-1" || es[0].ReworkNo != 1 || es[0].Reason != ReasonWrongConsol ||
		es[1].Start != StartTransferred || es[1].Predecessor != 1 || es[1].ConsolidationID != "C-2" || es[1].ReworkNo != 0 || es[1].ReworkExecutionID != "" || !es[1].Live() {
		t.Fatalf("the history: %+v", es)
	}
	if p := m.Pending(); len(p) != 2 || p[1].Type != eventHouseTransferred || p[1].FromConsolidationID != "C-1" || p[1].ConsolidationID != "C-2" || p[1].Reason != string(ReasonWrongConsol) {
		t.Fatalf("the event is HouseTransferred.v1 as today: %+v", p)
	}
	if _, err := RestoreMembership(m.State()); err != nil {
		t.Fatalf("restore: %v", err)
	}
}

func TestTheLatestReworkIsTheOneTheEntryRecordsAndOpenConsolidationsNameNone(t *testing.T) {
	// an OPEN consolidation lets the house go as it always did and records no rework, though the caller names one
	open := airConsol(t, "C-1")
	m, _ := NewMembership("T", "SH-1")
	if err := m.Attach(open, intent("SH-1", 1), "ops-1", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.DetachAfterRework(open, ReworkRef{ExecutionID: "X-1", No: 2, UnloadedAt: t0.Add(5 * time.Hour)}, ReasonShutOut, "", "ops-1", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if e := m.State().Entries[0]; e.ReworkNo != 0 || e.ReworkExecutionID != "" {
		t.Fatalf("an entry ended in an OPEN consolidation names no rework: %+v", e)
	}
	// of two rounds the caller hands the latest (the application's query picks it); the entry records what it is given
	c, _, m2 := closedWith(t)
	if err := m2.DetachAfterRework(c, ReworkRef{ExecutionID: "X-1", No: 2, UnloadedAt: t0.Add(5 * time.Hour)}, ReasonShutOut, "", "ops-1", t0.Add(7*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if e := m2.State().Entries[0]; e.ReworkExecutionID != "X-1" || e.ReworkNo != 2 {
		t.Fatalf("the latest round: %+v", e)
	}
}

func shgConsol(t *testing.T) *Consolidation {
	t.Helper()
	return seaConsol(t, "C-SEA", KindSeaLCLOwn)
}
