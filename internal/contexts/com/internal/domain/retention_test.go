package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRetentionEndAddsCalendarMonthsAsPostgreSQLDoes(t *testing.T) {
	for _, tc := range []struct{ start, want string }{
		{"2026-11-10T13:00:00.123456Z", "2029-11-10T13:00:00.123456Z"},
		{"2028-02-29T10:00:00Z", "2031-02-28T10:00:00Z"}, // the month lacks the day: its last day
		{"2026-08-31T23:59:59Z", "2029-08-31T23:59:59Z"},
		{"2026-12-31T00:00:00Z", "2029-12-31T00:00:00Z"},
	} {
		start, _ := time.Parse(time.RFC3339Nano, tc.start)
		want, _ := time.Parse(time.RFC3339Nano, tc.want)
		if got := RetentionEnd(start, MinContactRetentionMonths); !got.Equal(want) {
			t.Errorf("%s + 36 months = %s, want %s", tc.start, got.Format(time.RFC3339Nano), tc.want)
		}
	}
	jan31 := time.Date(2027, 1, 31, 8, 0, 0, 0, time.UTC)
	if got := RetentionEnd(jan31, 1); !got.Equal(time.Date(2027, 2, 28, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("31 January + 1 month = %s", got)
	}
	for months, ok := range map[int]bool{35: false, 36: true, 120: true, 121: false} {
		if ValidRetentionMonths(months) != ok {
			t.Errorf("%d months valid = %v", months, !ok)
		}
	}
}

// allocated returns an order whose two lines are allocated in full, with OB-1 reserved by shipment S-1.
func allocated(t *testing.T) *CustomerOrder {
	t.Helper()
	o := recorded(t)
	allocateAt := recordAt.Add(time.Hour)
	if err := errors.Join(
		o.Allocate(1, "OB-1", palletLine.Measure, "planner-1", allocateAt),
		o.Allocate(2, "OB-2", cartonLine.Measure, "planner-1", allocateAt),
		o.Reserve("OB-1", 1, "S-1", "fulfilment", allocateAt.Add(time.Hour)),
	); err != nil {
		t.Fatal(err)
	}
	if o.Status() != OrderAllocated {
		t.Fatalf("status %s", o.Status())
	}
	o.Acknowledge()
	return o
}

func TestCloseFreezesTheOrderAndStartsTheRetention(t *testing.T) {
	open := recorded(t)
	if err := open.Close("cs-1", recordAt.Add(time.Hour), MinContactRetentionMonths); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("closing an order with lines left to allocate: %v", err)
	}
	o := allocated(t)
	closeAt := recordAt.Add(3 * time.Hour)
	for name, err := range map[string]error{
		"35 months":                   o.Close("cs-1", closeAt, 35),
		"before the latest change":    o.Close("cs-1", recordAt.Add(90*time.Minute), MinContactRetentionMonths),
		"without the actor":           o.Close(" ", closeAt, MinContactRetentionMonths),
		"without a business time":     o.Close("cs-1", time.Time{}, MinContactRetentionMonths),
		"before the order was placed": o.Close("cs-1", recordAt.Add(-time.Hour), MinContactRetentionMonths),
	} {
		if err == nil || o.Status() != OrderAllocated {
			t.Errorf("%s: err %v, status %s", name, err, o.Status())
		}
	}
	if err := o.Close("cs-1", closeAt, 60); err != nil {
		t.Fatal(err)
	}
	r, ok := o.AddedRetention()
	if o.Status() != OrderClosed || !ok || r.Cause != RetentionClosed || !r.StartedAt.Equal(closeAt) || r.Months != 60 ||
		!r.RetainedUntil.Equal(time.Date(2031, 11, 6, 13, 0, 0, 0, time.UTC)) || !o.ContactsRestricted() {
		t.Fatalf("closed: %s %+v %v", o.Status(), r, ok)
	}
	if got := eventList(o); got != "[CustomerOrderClosed.v1]" {
		t.Fatalf("events %s", got)
	}
	later := closeAt.Add(time.Hour)
	for name, err := range map[string]error{
		"close again":      o.Close("cs-1", later, MinContactRetentionMonths),
		"amend a line":     o.AmendLine(2, "Cartons", "CT", packs(13, 65), "one more", "cs-1", later, later),
		"add a line":       o.AddLine("Crates", "CR", packs(1, 10), "one more crate", "cs-1", later, later),
		"revoke":           o.Revoke("OB-2", "planner-1", later),
		"amend":            o.Amend("OB-2", "OB-3", packs(12, 60), "planner-1", later),
		"cancel":           o.Cancel("sales-1", later, MinContactRetentionMonths),
		"set a party":      o.SetParty(named(PartyNotify, "N"), "ops-1", later, later),
		"change transport": o.ChangeTransport(Transport{Mode: ModeSea}, "sea after all", "ops-1", later, later),
		"reserve":          o.Reserve("OB-2", 1, "S-2", "fulfilment", later),
	} {
		if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s on a closed order: %v", name, err)
		}
	}
	if err := o.ReleaseReservation("OB-1", 1, "S-1", "fulfilment", later); err != nil {
		t.Fatalf("a shipment releases what it holds on a closed order: %v", err)
	}
	if o.Status() != OrderClosed {
		t.Fatalf("status after the release %s", o.Status())
	}
	restored, err := RestoreOrder(o.State())
	if err != nil || !restored.ContactsRestricted() || restored.Status() != OrderClosed {
		t.Fatalf("restore: %v", err)
	}
	if _, added := restored.AddedRetention(); added {
		t.Fatal("a restored retention is not written again")
	}
}

func TestRestoreChecksTheClosureAndTheRetention(t *testing.T) {
	o := allocated(t)
	closeAt := recordAt.Add(3 * time.Hour)
	if err := o.Close("cs-1", closeAt, MinContactRetentionMonths); err != nil {
		t.Fatal(err)
	}
	good := o.State()
	for name, edit := range map[string]func(*OrderState){
		"no retention":         func(s *OrderState) { s.Retention = nil },
		"35 months":            func(s *OrderState) { s.Retention.Months, s.Retention.RetainedUntil = 35, RetentionEnd(closeAt, 35) },
		"an end a second late": func(s *OrderState) { s.Retention.RetainedUntil = s.Retention.RetainedUntil.Add(time.Second) },
		"the wrong cause":      func(s *OrderState) { s.Retention.Cause = RetentionCancelled },
		"another start":        func(s *OrderState) { s.Retention.StartedAt = closeAt.Add(time.Minute) },
		"closed by nobody":     func(s *OrderState) { s.ClosedBy = "" },
		"closed and cancelled": func(s *OrderState) { s.CancelledBy, s.CancelledAt = "sales-1", closeAt },
		"an erasure too early": func(s *OrderState) {
			s.Erasure = &ContactErasure{ErasedBy: "dpo", ErasedAt: closeAt, EnteredAt: closeAt}
		},
		"a retention while open": func(s *OrderState) { s.Status, s.ClosedBy, s.ClosedAt = OrderAllocated, "", time.Time{} },
		"an erasure without one": func(s *OrderState) {
			s.Retention, s.Erasure = nil, &ContactErasure{ErasedBy: "dpo", ErasedAt: closeAt, EnteredAt: closeAt}
		},
		"closed before recording": func(s *OrderState) { s.ClosedAt = recordAt.Add(-time.Hour) },
	} {
		s := good
		s.Retention, s.Erasure = copyOf(good.Retention), copyOf(good.Erasure)
		edit(&s)
		if _, err := RestoreOrder(s); !errors.Is(err, ErrInvalidOrder) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestEraseContactsOnlyAfterTheRetention(t *testing.T) {
	open := recorded(t)
	if err := open.EraseContacts("dpo", recordAt, recordAt); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("erasing the contacts of an open order: %v", err)
	}
	o := recorded(t)
	cancelAt := recordAt.Add(time.Hour)
	if err := o.Cancel("sales-1", cancelAt, MinContactRetentionMonths); err != nil {
		t.Fatal(err)
	}
	o.Acknowledge()
	r, _ := o.Retention()
	if r.Cause != RetentionCancelled || !r.RetainedUntil.Equal(RetentionEnd(cancelAt, MinContactRetentionMonths)) {
		t.Fatalf("retention %+v", r)
	}
	end := r.RetainedUntil
	if err := o.EraseContacts("dpo", end, end.Add(-time.Microsecond)); !errors.Is(err, ErrContactsRetained) || !strings.Contains(err.Error(), "2029-11-06") {
		t.Fatalf("a microsecond early: %v", err)
	}
	if err := o.EraseContacts("dpo", cancelAt.Add(-time.Minute), end); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("a business time before the cancellation: %v", err)
	}
	if err := o.EraseContacts("dpo", end.Add(-time.Hour), end); err != nil {
		t.Fatalf("at the end of the retention: %v", err)
	}
	if e, added := o.AddedErasure(); !added || e.ErasedBy != "dpo" || !e.EnteredAt.Equal(end) || eventList(o) != "[CustomerOrderContactsErased.v1]" {
		t.Fatalf("erasure %+v %v %s", e, added, eventList(o))
	}
	if err := o.EraseContacts("dpo", end, end); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("erasing twice: %v", err)
	}
	restored, err := RestoreOrder(o.State())
	if err != nil {
		t.Fatal(err)
	}
	if _, added := restored.AddedErasure(); added {
		t.Fatal("a restored erasure is not written again")
	}
	if _, erased := restored.Erasure(); !erased || restored.Status() != OrderCancelled {
		t.Fatalf("restored: %s", restored.Status())
	}
}
