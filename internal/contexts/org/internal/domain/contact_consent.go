package domain

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Notice and consent of a contact (privacy assessment V1.2, sections 4 and 5; decisions P4 and P7). The tenant
// is the personal information handler; for contacts of customers and their trading partners it relies on the
// customer's undertaking, in its contract, that it told the person of the tenant's personal information rules
// and obtained consent, or on the person's own confirmation. ORG records which version of the rules the person
// was told of and whether the person consented separately to the contact being provided to others (PIPL
// article 23) and abroad (article 39). Recording a narrower consent is how consent is withdrawn.

// ConsentBasis says how the consent was obtained.
type ConsentBasis string

const (
	ConsentCustomerUndertaking ConsentBasis = "CUSTOMER_UNDERTAKING" // the customer undertook it in its contract
	ConsentSubjectConfirmed    ConsentBasis = "SUBJECT_CONFIRMED"    // the person confirmed it
)

// ActionRecordContactConsent records a contact's notice and consent.
const ActionRecordContactConsent BookAction = "RECORD_CONTACT_CONSENT"

// EventPartyContactConsentRecorded names the contact and the address book revision; never the consent.
const EventPartyContactConsentRecorded = "PartyContactConsentRecorded.v1"

// ContactConsent is the notice a person was given and the separate consents the person gave. EvidenceRef
// points to where the undertaking or confirmation is kept, such as a contract number.
type ContactConsent struct {
	NoticeVersion   string
	ProvideToOthers bool
	CrossBorder     bool
	Basis           ConsentBasis
	EvidenceRef     string
}

// ConsentRecord is a contact's consent as recorded at an address book revision.
type ConsentRecord struct {
	ContactID ContactID
	Revision  int64
	ContactConsent
}

var noticeVersionForm = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

const maxEvidenceRunes = 200

func (c ContactConsent) normalized() (ContactConsent, error) {
	out := c
	out.NoticeVersion, out.EvidenceRef = strings.TrimSpace(c.NoticeVersion), strings.TrimSpace(c.EvidenceRef)
	var ok bool
	switch {
	case !noticeVersionForm.MatchString(out.NoticeVersion):
		return ContactConsent{}, fmt.Errorf("%w: notice version", ErrInvalidConsent)
	case out.Basis != ConsentCustomerUndertaking && out.Basis != ConsentSubjectConfirmed:
		return ContactConsent{}, fmt.Errorf("%w: basis %q", ErrInvalidConsent, out.Basis)
	}
	if out.EvidenceRef, ok = text(out.EvidenceRef, 1, maxEvidenceRunes); !ok {
		return ContactConsent{}, fmt.Errorf("%w: evidence", ErrInvalidConsent)
	}
	return out, nil
}

// RecordContactConsent records the notice and consent of an active or inactive contact; an erased contact has
// none to record. It is one revision of the address book and leaves the contact's version as it is.
func (b *AddressBook) RecordContactConsent(id ContactID, c ContactConsent, actor string, at time.Time) (ContactVersion, error) {
	if err := b.begin(actor, at); err != nil {
		return ContactVersion{}, err
	}
	current, ok := b.contacts[id]
	if !ok {
		return ContactVersion{}, fmt.Errorf("%w: %s", ErrUnknownContact, id)
	}
	if current.Status == EntryErased {
		return ContactVersion{}, fmt.Errorf("%w: contact %s is erased", ErrEntryNotActive, id)
	}
	c, err := c.normalized()
	if err != nil {
		return ContactVersion{}, err
	}
	if previous, has := b.consents[id]; has && previous.ContactConsent == c {
		return ContactVersion{}, ErrNothingChanged
	}
	rev := b.next(ActionRecordContactConsent, string(id), "", actor, "", at)
	r := ConsentRecord{ContactID: id, Revision: rev, ContactConsent: c}
	b.consents[id] = r
	b.unsaved.Consents = append(b.unsaved.Consents, r)
	b.record(EventPartyContactConsentRecorded, at, string(id), current.Version, "")
	current.Purposes = append([]ContactPurpose(nil), current.Purposes...)
	return current, nil
}

// Consent returns a contact's current consent, if one was recorded.
func (b *AddressBook) Consent(id ContactID) (ConsentRecord, bool) {
	r, ok := b.consents[id]
	return r, ok
}

// RestoreConsents puts back the current consent of each contact of a restored book: one per contact, of a
// contact the book has, recorded at a revision the book has reached.
func (b *AddressBook) RestoreConsents(records []ConsentRecord) error {
	for _, r := range records {
		_, known := b.contacts[r.ContactID]
		_, twice := b.consents[r.ContactID]
		normal, err := r.ContactConsent.normalized()
		if !known || twice || r.Revision < 1 || r.Revision > b.revision || err != nil || normal != r.ContactConsent {
			return fmt.Errorf("%w: consent of contact %s", ErrInconsistentBook, r.ContactID)
		}
		b.consents[r.ContactID] = r
	}
	return nil
}
