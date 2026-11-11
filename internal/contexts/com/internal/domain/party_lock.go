package domain

import (
	"errors"
	"fmt"
	"time"
)

// COM V0.9, C12 (DOC OD-2 option A): once DOC issues a house document built from an order's parties, the
// parties are locked for the document. While any lock is held COM refuses to set a party or change the
// transport; the document holder releases its lock when the document is cancelled or amended with other
// parties. As with FUL's reservations, the lock is COM's own fact, set and cleared through COM's api; COM never
// asks DOC. Locks are numbered from 1 within the order and kept after release.

var ErrPartiesLocked = errors.New("com: ORDER_PARTIES_LOCKED: a transport document holds the order's parties; its holder releases the lock first")

// PartyLock is a lock on the order's parties and transport held by a transport document.
type PartyLock struct {
	No               int
	HolderID         string // the DOC document id
	LockedBy         string
	LockedAt         time.Time
	EnteredAt        time.Time
	ReleasedBy       string
	ReleasedAt       time.Time // zero while held
	ReleaseEnteredAt time.Time
}

// Held reports whether the lock is in force.
func (l PartyLock) Held() bool { return l.ReleasedAt.IsZero() }

// LockParties locks the order's parties and transport for a holder, while the order is open and the holder
// holds no lock on it yet.
func (o *CustomerOrder) LockParties(holder, by string, at, entered time.Time) error {
	if err := o.open(); err != nil {
		return err
	}
	if !identifier(holder) || blank(by) || at.IsZero() || entered.IsZero() {
		return ErrInvalidChange
	}
	if o.lockedBy(holder) >= 0 {
		return fmt.Errorf("%w: %s holds a lock on the order's parties already", ErrInvalidChange, holder)
	}
	o.state.PartyLocks = append(o.state.PartyLocks, PartyLock{No: len(o.state.PartyLocks) + 1, HolderID: holder, LockedBy: by, LockedAt: at.UTC(),
		EnteredAt: entered.UTC()})
	e := o.event(eventPartiesLocked, at)
	e.HolderID = holder
	o.pending = append(o.pending, e)
	return nil
}

// ReleasePartyLock releases the lock a holder holds, at any status of the order.
func (o *CustomerOrder) ReleasePartyLock(holder, by string, at, entered time.Time) error {
	i := o.lockedBy(holder)
	if i < 0 {
		return fmt.Errorf("%w: %s holds no lock on the order's parties", ErrInvalidChange, holder)
	}
	l := &o.state.PartyLocks[i]
	if blank(by) || at.IsZero() || entered.IsZero() || at.Before(l.LockedAt) {
		return ErrInvalidChange
	}
	l.ReleasedBy, l.ReleasedAt, l.ReleaseEnteredAt = by, at.UTC(), entered.UTC()
	if i < o.storedLocks {
		o.releasedLocks = append(o.releasedLocks, i)
	}
	e := o.event(eventPartyLockReleased, at)
	e.HolderID = holder
	o.pending = append(o.pending, e)
	return nil
}

// PartyLocks returns the locks in force, in the order taken.
func (o *CustomerOrder) PartyLocks() []PartyLock {
	var out []PartyLock
	for _, l := range o.state.PartyLocks {
		if l.Held() {
			out = append(out, l)
		}
	}
	return out
}

// unlocked refuses a change of the order's parties or transport while a lock is held.
func (o *CustomerOrder) unlocked() error {
	if held := o.PartyLocks(); len(held) > 0 {
		return fmt.Errorf("%w (%s)", ErrPartiesLocked, held[0].HolderID)
	}
	return nil
}

func (o *CustomerOrder) lockedBy(holder string) int {
	for i, l := range o.state.PartyLocks {
		if l.HolderID == holder && l.Held() {
			return i
		}
	}
	return -1
}

// AddedPartyLocks returns the locks taken since the order was recorded or restored, and ReleasedPartyLocks the
// stored locks released since, for persistence.
func (o *CustomerOrder) AddedPartyLocks() []PartyLock {
	return append([]PartyLock(nil), o.state.PartyLocks[o.storedLocks:]...)
}

func (o *CustomerOrder) ReleasedPartyLocks() []PartyLock {
	out := make([]PartyLock, len(o.releasedLocks))
	for k, i := range o.releasedLocks {
		out[k] = o.state.PartyLocks[i]
	}
	return out
}

// restorePartyLocks checks stored locks: numbered from 1, released at most once and not before they were taken,
// and at most one in force per holder.
func (o *CustomerOrder) restorePartyLocks() error {
	o.state.PartyLocks = append([]PartyLock(nil), o.state.PartyLocks...)
	held := map[string]bool{}
	for i, l := range o.state.PartyLocks {
		if l.No != i+1 || !identifier(l.HolderID) || blank(l.LockedBy) || l.LockedAt.IsZero() ||
			(!l.Held() && (blank(l.ReleasedBy) || l.ReleasedAt.Before(l.LockedAt))) || (l.Held() && held[l.HolderID]) {
			return fmt.Errorf("%w: party lock %d", ErrInvalidOrder, i+1)
		}
		if l.Held() {
			held[l.HolderID] = true
		}
	}
	o.storedLocks = len(o.state.PartyLocks)
	return nil
}
