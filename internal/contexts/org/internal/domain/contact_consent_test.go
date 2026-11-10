package domain

import (
	"errors"
	"testing"
	"time"
)

func undertaking() ContactConsent {
	return ContactConsent{NoticeVersion: "PI-RULES-2026.1", ProvideToOthers: true, CrossBorder: true, Basis: ConsentCustomerUndertaking,
		EvidenceRef: "Service contract SC-2026-001 clause 12"}
}

// A contact's notice and consent are a revision of the address book (decisions P4, P7): recorded for active
// and inactive contacts, narrowed to withdraw, never for an erased contact, and restored with the book.
func TestRecordContactConsent(t *testing.T) {
	b := newBook(t)
	if _, err := b.AddContact("C1", bookingDesk(), []ContactPurpose{ContactBooking}, "clerk", bookAt); err != nil {
		t.Fatal(err)
	}
	if _, err := b.RecordContactConsent("C9", undertaking(), "clerk", bookAt); !errors.Is(err, ErrUnknownContact) {
		t.Fatalf("an unknown contact: %v", err)
	}
	for name, edit := range map[string]func(*ContactConsent){
		"no notice version":   func(c *ContactConsent) { c.NoticeVersion = "" },
		"a notice with space": func(c *ContactConsent) { c.NoticeVersion = "rules 1" },
		"an unknown basis":    func(c *ContactConsent) { c.Basis = "IMPLIED" },
		"no evidence":         func(c *ContactConsent) { c.EvidenceRef = " " },
	} {
		c := undertaking()
		edit(&c)
		if _, err := b.RecordContactConsent("C1", c, "clerk", bookAt); !errors.Is(err, ErrInvalidConsent) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if b.Revision() != 1 {
		t.Fatalf("refused consents changed the book: revision %d", b.Revision())
	}
	v, err := b.RecordContactConsent("C1", undertaking(), "clerk", bookAt)
	if err != nil || v.ID != "C1" || v.Version != 1 || b.Revision() != 2 {
		t.Fatalf("record: %+v %v, revision %d", v, err, b.Revision())
	}
	if r, ok := b.Consent("C1"); !ok || r.Revision != 2 || !r.CrossBorder || r.Basis != ConsentCustomerUndertaking {
		t.Fatalf("consent %+v", r)
	}
	if _, err = b.RecordContactConsent("C1", undertaking(), "clerk", bookAt); !errors.Is(err, ErrNothingChanged) {
		t.Fatalf("the same consent again: %v", err)
	}
	withdrawn := undertaking()
	withdrawn.CrossBorder = false
	if _, err = b.RecordContactConsent("C1", withdrawn, "clerk", bookAt.Add(time.Hour)); err != nil {
		t.Fatalf("withdraw the consent to go abroad: %v", err)
	}
	if _, err = b.DeactivateContact("C1", "left the company", "clerk", bookAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	withdrawn.ProvideToOthers = false
	if _, err = b.RecordContactConsent("C1", withdrawn, "clerk", bookAt.Add(2*time.Hour)); err != nil {
		t.Fatalf("an inactive contact withdraws: %v", err)
	}
	u := b.Unsaved()
	if len(u.Consents) != 3 || u.Changes[len(u.Changes)-1].Action != ActionRecordContactConsent || len(u.ContactVersions) != 2 {
		t.Fatalf("unsaved %d consents, last action %s, %d contact versions", len(u.Consents), u.Changes[len(u.Changes)-1].Action, len(u.ContactVersions))
	}
	events := b.Pending()
	if last := events[len(events)-1]; last.Type != EventPartyContactConsentRecorded || last.EntryID != "C1" {
		t.Fatalf("event %+v", last)
	}
	if _, err = b.EraseContact("C1", "the person asked", "clerk", bookAt.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = b.RecordContactConsent("C1", undertaking(), "clerk", bookAt.Add(3*time.Hour)); !errors.Is(err, ErrEntryNotActive) {
		t.Fatalf("a consent for an erased contact: %v", err)
	}
	if r, ok := b.Consent("C1"); !ok || r.ProvideToOthers || r.CrossBorder {
		t.Fatalf("the erasure keeps the last consent, which holds no personal data: %+v", r)
	}

	restored, err := RestoreAddressBook("T", "P", b.Revision(), nil, b.Contacts(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	current, _ := b.Consent("C1")
	if err = restored.RestoreConsents([]ConsentRecord{current}); err != nil {
		t.Fatal(err)
	}
	for name, records := range map[string][]ConsentRecord{
		"twice":              {current, current},
		"an unknown contact": {{ContactID: "C9", Revision: 2, ContactConsent: undertaking()}},
		"a later revision":   {{ContactID: "C1", Revision: b.Revision() + 1, ContactConsent: undertaking()}},
		"an invalid basis":   {{ContactID: "C1", Revision: 2, ContactConsent: ContactConsent{NoticeVersion: "v1", Basis: "IMPLIED", EvidenceRef: "x"}}},
	} {
		again, _ := RestoreAddressBook("T", "P", b.Revision(), nil, b.Contacts(), nil, nil)
		if err := again.RestoreConsents(records); !errors.Is(err, ErrInconsistentBook) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
