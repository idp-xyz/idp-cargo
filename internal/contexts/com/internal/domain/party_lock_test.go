package domain

import (
	"errors"
	"testing"
	"time"
)

// COM V0.9, C12: transport documents lock the order's parties and transport; COM refuses changes while any lock
// is held, a holder holds one lock and releases it once, and stored locks restore.
func TestPartyLocksHoldTheParties(t *testing.T) {
	d := draft(palletLine)
	d.Transport = Transport{Mode: ModeAir, Movement: PortToPort}
	d.InitialParties = []OrderParty{named(PartyShipper, "S"), named(PartyConsignee, "C")}
	o, err := RecordOrder(d, 0, recordAt, recordAt)
	if err != nil {
		t.Fatal(err)
	}
	o.Acknowledge()
	later := recordAt.Add(time.Hour)
	if err = o.LockParties("", "doc-clerk", later, later); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("a lock without its holder: %v", err)
	}
	if err = o.LockParties("DOC-HAWB-1", "doc-clerk", later, later); err != nil {
		t.Fatal(err)
	}
	if err = o.LockParties("DOC-HAWB-1", "doc-clerk", later, later); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("a second lock of one holder: %v", err)
	}
	if err = o.LockParties("DOC-HAWB-2", "doc-clerk", later, later); err != nil {
		t.Fatalf("another document locks too: %v", err)
	}
	notify := named(PartyNotify, "N")
	if err = o.SetParty(notify, "ops-1", later, later); !errors.Is(err, ErrPartiesLocked) {
		t.Fatalf("set a party while locked: %v", err)
	}
	if err = o.ChangeTransport(Transport{Mode: ModeAir, Movement: DoorToPort}, "door pickup", "ops-1", later, later); !errors.Is(err, ErrPartiesLocked) {
		t.Fatalf("change the transport while locked: %v", err)
	}
	if err = o.ReleasePartyLock("DOC-HAWB-1", "doc-clerk", later, later); err != nil {
		t.Fatal(err)
	}
	if err = o.ReleasePartyLock("DOC-HAWB-1", "doc-clerk", later, later); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("released twice: %v", err)
	}
	if err = o.SetParty(notify, "ops-1", later, later); !errors.Is(err, ErrPartiesLocked) {
		t.Fatalf("another document still holds a lock: %v", err)
	}
	state := o.State()
	state.Status = o.Status()
	restored, err := RestoreOrder(state)
	if err != nil {
		t.Fatal(err)
	}
	if held := restored.PartyLocks(); len(held) != 1 || held[0].HolderID != "DOC-HAWB-2" || len(restored.AddedPartyLocks()) != 0 {
		t.Fatalf("restored locks: %+v", held)
	}
	if err = restored.ReleasePartyLock("DOC-HAWB-2", "doc-clerk", later, later); err != nil {
		t.Fatal(err)
	}
	if released := restored.ReleasedPartyLocks(); len(released) != 1 || released[0].No != 2 {
		t.Fatalf("released stored lock: %+v", released)
	}
	if err = restored.SetParty(notify, "ops-1", later, later); err != nil {
		t.Fatalf("set a party once every lock is released: %v", err)
	}
	if got := eventList(o); got != "[OrderPartiesLocked.v1 OrderPartiesLocked.v1 OrderPartyLockReleased.v1]" {
		t.Fatalf("events = %s", got)
	}
	for _, e := range o.Pending() {
		if e.HolderID == "" {
			t.Fatalf("lock event without its holder: %+v", e)
		}
	}
	broken := o.State()
	broken.Status = o.Status()
	broken.PartyLocks = append([]PartyLock(nil), broken.PartyLocks...)
	broken.PartyLocks[1].HolderID = "DOC-HAWB-1"
	broken.PartyLocks[0].ReleasedAt = broken.PartyLocks[0].LockedAt.Add(-1)
	if _, err = RestoreOrder(broken); err == nil {
		t.Fatal("a lock released before it was taken")
	}
	if err = o.Cancel("ops-1", later, MinContactRetentionMonths); err == nil {
		if err = o.LockParties("DOC-HAWB-3", "doc-clerk", later, later); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("lock a cancelled order: %v", err)
		}
	}
}
