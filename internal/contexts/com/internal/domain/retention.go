package domain

import (
	"errors"
	"fmt"
	"time"
)

// Contact retention (decision P1). An order's contacts serve the order while it is open. Once the order is
// closed or cancelled their purpose has ended: COM keeps them only for the customs records, which must be
// kept three years after release, and shows them only to callers allowed to read restricted contacts. When
// the retention period has ended they may be erased, every version at once; the order keeps everything else.
const (
	MinContactRetentionMonths = 36 // three years (P1; legal counsel to confirm)
	MaxContactRetentionMonths = 120
)

// RetentionCause is what ended the contacts' purpose.
type RetentionCause string

const (
	RetentionClosed    RetentionCause = "CLOSED"
	RetentionCancelled RetentionCause = "CANCELLED"
)

// ContactRetention says until when an order's contacts are kept: Months after StartedAt, the business time
// the order was closed or cancelled. Months are UTC calendar months, added as PostgreSQL adds an interval:
// a day the target month lacks becomes its last day.
type ContactRetention struct {
	Cause         RetentionCause
	StartedAt     time.Time
	Months        int
	RetainedUntil time.Time
}

// ContactErasure records that an order's contacts were erased: who, the business time, and when COM
// recorded it, which is never before the retention's end.
type ContactErasure struct {
	ErasedBy  string
	ErasedAt  time.Time
	EnteredAt time.Time
}

var (
	ErrContactsRetained = errors.New("com: the order's contacts are within their retention period")
	ErrInvalidRetention = errors.New("com: invalid contact retention")
)

// ValidRetentionMonths reports whether COM accepts a retention period: 36 to 120 months.
func ValidRetentionMonths(months int) bool {
	return months >= MinContactRetentionMonths && months <= MaxContactRetentionMonths
}

// RetentionEnd returns the end of a retention of months that starts at start.
func RetentionEnd(start time.Time, months int) time.Time {
	t := start.UTC()
	y, m, d := t.Date()
	first := time.Date(y, m+time.Month(months), 1, 0, 0, 0, 0, time.UTC)
	if last := first.AddDate(0, 1, -1).Day(); d > last {
		d = last
	}
	return time.Date(first.Year(), first.Month(), d, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
}

// open refuses changes to an order that is cancelled or closed.
func (o *CustomerOrder) open() error {
	switch o.state.Status {
	case OrderCancelled:
		return fmt.Errorf("%w: the order is cancelled", ErrInvalidTransition)
	case OrderClosed:
		return fmt.Errorf("%w: the order is closed", ErrInvalidTransition)
	}
	return nil
}

// Close records that the work on an order is finished. Only an order whose lines are all allocated can be
// closed. A closed order no longer changes: its lines, obligations, parties and transport stay, it cannot be
// cancelled, and no shipment may reserve its obligations any more; shipments that hold them may still
// release them. Its contacts are then kept for months from at, restricted, before they may be erased (P1).
func (o *CustomerOrder) Close(by string, at time.Time, months int) error {
	if err := o.open(); err != nil {
		return err
	}
	if o.state.Status != OrderAllocated {
		return fmt.Errorf("%w: the order is %s; only an order whose lines are all allocated can be closed", ErrInvalidTransition, o.state.Status)
	}
	if blank(by) || at.IsZero() || at.Before(o.latestChange()) {
		return ErrInvalidChange
	}
	if !ValidRetentionMonths(months) {
		return fmt.Errorf("%w: %d months", ErrInvalidRetention, months)
	}
	o.state.Status, o.state.ClosedBy, o.state.ClosedAt = OrderClosed, by, at.UTC()
	o.startRetention(RetentionClosed, at, months)
	o.record(eventOrderClosed, at, nil, "")
	return nil
}

func (o *CustomerOrder) startRetention(cause RetentionCause, at time.Time, months int) {
	o.state.Retention = &ContactRetention{Cause: cause, StartedAt: at.UTC(), Months: months, RetainedUntil: RetentionEnd(at, months)}
}

// latestChange returns the latest business time recorded on the order.
func (o *CustomerOrder) latestChange() time.Time {
	last := o.state.RecordedAt
	for _, v := range o.state.LineChanges {
		last = later(last, v.ChangedAt)
	}
	for _, ob := range o.state.Obligations {
		last = later(later(last, ob.AllocatedAt), ob.RevokedAt)
	}
	for _, r := range o.state.Reservations {
		last = later(later(last, r.ReservedAt), r.ReleasedAt)
	}
	for _, p := range o.state.Parties {
		last = later(last, p.SetAt)
	}
	for _, v := range o.state.TransportVersions {
		last = later(last, v.ChangedAt)
	}
	return last
}

// EraseContacts erases the contacts of every party version of a closed or cancelled order once their
// retention has ended (P1). entered, when COM records the erasure, decides whether it has ended; at is the
// business time. The order keeps everything else, its revision included.
func (o *CustomerOrder) EraseContacts(by string, at, entered time.Time) error {
	r := o.state.Retention
	switch {
	case r == nil:
		return fmt.Errorf("%w: the order is %s, so its contacts still serve it", ErrInvalidTransition, o.state.Status)
	case o.state.Erasure != nil:
		return fmt.Errorf("%w: the contacts are erased already", ErrInvalidTransition)
	case blank(by), at.IsZero(), entered.IsZero(), at.Before(r.StartedAt):
		return ErrInvalidChange
	case entered.Before(r.RetainedUntil):
		return fmt.Errorf("%w: until %s", ErrContactsRetained, r.RetainedUntil.Format(time.RFC3339))
	}
	o.state.Erasure = &ContactErasure{ErasedBy: by, ErasedAt: at.UTC(), EnteredAt: entered.UTC()}
	o.record(eventContactsErased, at, nil, "")
	return nil
}

// ContactsRestricted reports whether the order's contacts no longer serve it: the order is closed or
// cancelled. Restricted contacts are shown only to callers allowed to read them (P1).
func (o *CustomerOrder) ContactsRestricted() bool { return o.state.Retention != nil }

// Retention returns the order's contact retention, if the order is closed or cancelled.
func (o *CustomerOrder) Retention() (ContactRetention, bool) {
	if r := o.state.Retention; r != nil {
		return *r, true
	}
	return ContactRetention{}, false
}

// Erasure returns the erasure of the order's contacts, if they were erased.
func (o *CustomerOrder) Erasure() (ContactErasure, bool) {
	if e := o.state.Erasure; e != nil {
		return *e, true
	}
	return ContactErasure{}, false
}

// AddedRetention returns the retention started since the order was loaded, for persistence.
func (o *CustomerOrder) AddedRetention() (ContactRetention, bool) {
	if o.retained {
		return ContactRetention{}, false
	}
	return o.Retention()
}

// AddedErasure returns the erasure recorded since the order was loaded, for persistence.
func (o *CustomerOrder) AddedErasure() (ContactErasure, bool) {
	if o.erased {
		return ContactErasure{}, false
	}
	return o.Erasure()
}

// validateRetention checks a restored order's closure, retention and erasure: a closed or cancelled order has
// a retention that starts when it was closed or cancelled and ends where RetentionEnd puts it; an open one has
// neither; an erasure follows the retention's end.
func (o *CustomerOrder) validateRetention() error {
	s := o.state
	closed, cancelled := !s.ClosedAt.IsZero(), !s.CancelledAt.IsZero()
	if closed == blank(s.ClosedBy) || (closed && (cancelled || s.ClosedAt.Before(s.RecordedAt))) {
		return fmt.Errorf("%w: closure", ErrInvalidOrder)
	}
	r := s.Retention
	if r == nil {
		if closed || cancelled || s.Erasure != nil {
			return fmt.Errorf("%w: a closed or cancelled order has a contact retention, and only it", ErrInvalidOrder)
		}
		return nil
	}
	cause, start := RetentionCancelled, s.CancelledAt
	if closed {
		cause, start = RetentionClosed, s.ClosedAt
	}
	if !closed && !cancelled || r.Cause != cause || !r.StartedAt.Equal(start) || !ValidRetentionMonths(r.Months) ||
		!r.RetainedUntil.Equal(RetentionEnd(r.StartedAt, r.Months)) {
		return fmt.Errorf("%w: contact retention", ErrInvalidOrder)
	}
	if e := s.Erasure; e != nil && (blank(e.ErasedBy) || e.ErasedAt.Before(r.StartedAt) || e.EnteredAt.Before(r.RetainedUntil)) {
		return fmt.Errorf("%w: contact erasure", ErrInvalidOrder)
	}
	return nil
}

func copyOf[T any](p *T) *T {
	if p == nil {
		return nil
	}
	c := *p
	return &c
}
