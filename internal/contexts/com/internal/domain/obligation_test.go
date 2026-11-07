package domain

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestRevokeFreesTheLineAndKeepsTheVersion(t *testing.T) {
	o := recorded(t)
	if err := errors.Join(
		o.Allocate(1, "OB-1", packsWithVolume(10, 500, 12_000), "planner-1", recordAt.Add(time.Hour)),
		o.Allocate(2, "OB-2", packs(12, 60), "planner-1", recordAt.Add(time.Hour)),
	); err != nil || o.Status() != OrderAllocated {
		t.Fatalf("allocate: %v, status %s", err, o.Status())
	}
	o.Acknowledge()
	before := o.State()
	for name, tc := range map[string]struct {
		id   ObligationID
		by   string
		at   time.Time
		want error
	}{
		"unknown obligation":         {"OB-9", "planner-1", recordAt.Add(2 * time.Hour), ErrUnknownObligation},
		"before it was allocated":    {"OB-1", "planner-1", recordAt.Add(30 * time.Minute), ErrInvalidChange},
		"nobody revoking":            {"OB-1", " ", recordAt.Add(2 * time.Hour), ErrInvalidChange},
		"without an instant at all":  {"OB-1", "planner-1", time.Time{}, ErrInvalidChange},
		"an id the order never held": {"", "planner-1", recordAt.Add(2 * time.Hour), ErrUnknownObligation},
	} {
		if err := o.Revoke(tc.id, tc.by, tc.at); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	if !reflect.DeepEqual(o.State(), before) || len(o.Pending()) != 0 {
		t.Fatalf("refused revocations changed the order")
	}
	if err := o.Revoke("OB-1", "planner-2", recordAt.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	v1, _ := o.Obligation("OB-1", 1)
	v2, ok := o.Obligation("OB-1", 2)
	want := v1
	want.Version, want.Status, want.RevokedBy, want.RevokedAt = 2, ObligationRevoked, "planner-2", recordAt.Add(2*time.Hour)
	if !ok || v2 != want || v1.Status != ObligationActive {
		t.Fatalf("version 1 %+v, version 2 %+v", v1, v2)
	}
	if latest, _ := o.Latest("OB-1"); latest != v2 {
		t.Fatalf("latest %+v", latest)
	}
	if rest, _ := o.Remaining(1); rest != packsWithVolume(10, 500, 12_000) || o.Status() != OrderPartiallyAllocated {
		t.Fatalf("remaining %+v, status %s", rest, o.Status())
	}
	if err := o.Revoke("OB-1", "planner-2", recordAt.Add(3*time.Hour)); !errors.Is(err, ErrObligationNotActive) {
		t.Fatalf("revoking twice: err = %v", err)
	}
	if err := o.Revoke("OB-2", "planner-2", recordAt.Add(3*time.Hour)); err != nil || o.Status() != OrderRecorded {
		t.Fatalf("revoke the last active obligation: %v, status %s", err, o.Status())
	}
	if err := o.Allocate(1, "OB-1", packs(1, 1), "planner-1", recordAt.Add(4*time.Hour)); !errors.Is(err, ErrInvalidAllocation) {
		t.Fatalf("a revoked obligation id is never reused: err = %v", err)
	}
	if err := o.Allocate(1, "OB-3", packsWithVolume(10, 500, 12_000), "planner-1", recordAt.Add(4*time.Hour)); err != nil {
		t.Fatalf("the freed packs are allocatable again: %v", err)
	}
	if got, want := eventList(o), "[FulfillmentObligationRevoked.v1/OB-1/2 FulfillmentObligationRevoked.v1/OB-2/2 "+
		"FulfillmentObligationAllocated.v1/OB-3/1]"; got != want {
		t.Fatalf("events %s", got)
	}
	if added := o.Added(); len(added) != 3 || added[0] != v2 || added[2].ObligationID != "OB-3" {
		t.Fatalf("added %+v", added)
	}
}

func TestAmendReplacesAnObligationOrChangesNothing(t *testing.T) {
	o := recorded(t)
	if err := errors.Join(
		o.Allocate(1, "OB-1", packsWithVolume(6, 300, 7_000), "planner-1", recordAt.Add(time.Hour)),
		o.Allocate(1, "OB-2", packsWithVolume(4, 200, 5_000), "planner-1", recordAt.Add(time.Hour)),
	); err != nil {
		t.Fatal(err)
	}
	o.Acknowledge()
	before := o.State()
	later := recordAt.Add(2 * time.Hour)
	for name, tc := range map[string]struct {
		id, newID ObligationID
		m         Measure
		want      error
	}{
		"more packs than the old one frees": {"OB-1", "OB-3", packsWithVolume(7, 300, 7_000), ErrOverAllocation},
		"the new id is the old one":         {"OB-1", "OB-1", packsWithVolume(6, 300, 7_000), ErrInvalidAllocation},
		"the new id is another obligation":  {"OB-1", "OB-2", packsWithVolume(6, 300, 7_000), ErrInvalidAllocation},
		"no volume on a line with volume":   {"OB-1", "OB-3", packs(6, 300), ErrInvalidAllocation},
		"unknown obligation":                {"OB-9", "OB-3", packsWithVolume(1, 1, 1), ErrUnknownObligation},
	} {
		if err := o.Amend(tc.id, tc.newID, tc.m, "planner-2", later); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
		if !reflect.DeepEqual(o.State(), before) || len(o.Pending()) != 0 || len(o.Added()) != 0 {
			t.Fatalf("%s: a refused amendment changed the order", name)
		}
	}
	if err := o.Amend("OB-1", "OB-3", packsWithVolume(6, 280, 6_500), "planner-2", later); err != nil {
		t.Fatal(err)
	}
	if v2, _ := o.Latest("OB-1"); v2.Version != 2 || v2.Status != ObligationRevoked {
		t.Fatalf("OB-1 after the amendment: %+v", v2)
	}
	if ob, _ := o.Latest("OB-3"); ob.LineNo != 1 || ob.Measure != packsWithVolume(6, 280, 6_500) || ob.AllocatedBy != "planner-2" {
		t.Fatalf("OB-3: %+v", ob)
	}
	if rest, _ := o.Remaining(1); rest != packsWithVolume(0, 20, 500) {
		t.Fatalf("remaining %+v", rest)
	}
	if got, want := eventList(o), "[FulfillmentObligationRevoked.v1/OB-1/2 FulfillmentObligationAllocated.v1/OB-3/1]"; got != want {
		t.Fatalf("events %s", got)
	}
}

func TestCancelRevokesEveryActiveObligation(t *testing.T) {
	o := recorded(t)
	if err := errors.Join(
		o.Allocate(1, "OB-1", packsWithVolume(10, 500, 12_000), "planner-1", recordAt.Add(time.Hour)),
		o.Allocate(2, "OB-2", packs(5, 25), "planner-1", recordAt.Add(time.Hour)),
		o.Revoke("OB-2", "planner-1", recordAt.Add(2*time.Hour)),
		o.Allocate(2, "OB-3", packs(7, 35), "planner-1", recordAt.Add(2*time.Hour)),
	); err != nil {
		t.Fatal(err)
	}
	o.Acknowledge()
	if err := o.Cancel("sales-1", recordAt.Add(time.Hour)); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("cancelling before the last allocation: err = %v", err)
	}
	if err := o.Cancel(" ", recordAt.Add(3*time.Hour)); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("nobody cancelling: err = %v", err)
	}
	if err := o.Cancel("sales-1", recordAt.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	s := o.State()
	if s.Status != OrderCancelled || s.CancelledBy != "sales-1" || !s.CancelledAt.Equal(recordAt.Add(3*time.Hour)) {
		t.Fatalf("order %+v", s)
	}
	for _, id := range []ObligationID{"OB-1", "OB-2", "OB-3"} {
		if ob, _ := o.Latest(id); ob.Status != ObligationRevoked {
			t.Errorf("%s after the cancellation: %+v", id, ob)
		}
	}
	if got, want := eventList(o), "[FulfillmentObligationRevoked.v1/OB-1/2 FulfillmentObligationRevoked.v1/OB-3/2 CustomerOrderCancelled.v1]"; got != want {
		t.Fatalf("events %s", got)
	}
	if added := o.Added(); len(added) != 2 {
		t.Fatalf("added %+v", added)
	}
	restored, err := RestoreOrder(o.State())
	if err != nil || !reflect.DeepEqual(restored.State(), o.State()) {
		t.Fatalf("restore a cancelled order: %v", err)
	}
	later := recordAt.Add(4 * time.Hour)
	for name, err := range map[string]error{
		"allocate": restored.Allocate(1, "OB-4", packsWithVolume(1, 1, 1), "planner-1", later),
		"revoke":   restored.Revoke("OB-1", "planner-1", later),
		"amend":    restored.Amend("OB-1", "OB-4", packsWithVolume(1, 1, 1), "planner-1", later),
		"cancel":   restored.Cancel("sales-1", later),
	} {
		if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s on a cancelled order: err = %v", name, err)
		}
	}
	if err := restored.Reserve("OB-1", 1, "S-1", "fulfilment", later); !errors.Is(err, ErrObligationNotActive) {
		t.Errorf("reserve on a cancelled order: err = %v", err)
	}
}

func TestCancelAnOrderWithoutObligations(t *testing.T) {
	o := recorded(t)
	if err := o.Cancel("sales-1", recordAt); err != nil || o.Status() != OrderCancelled || len(o.Added()) != 0 {
		t.Fatalf("cancel: %v, status %s, added %d", err, o.Status(), len(o.Added()))
	}
	if got := eventList(o); got != "[CustomerOrderRecorded.v1 CustomerOrderCancelled.v1]" {
		t.Fatalf("events %s", got)
	}
}
