package domain

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// AddressPurpose is what a party's address is used for (C9): OFFICE for documents and the office, PICKUP,
// DELIVERY and BILLING. Each purpose has at most one default address.
type AddressPurpose string

const (
	AddressOffice   AddressPurpose = "OFFICE"
	AddressPickup   AddressPurpose = "PICKUP"
	AddressDelivery AddressPurpose = "DELIVERY"
	AddressBilling  AddressPurpose = "BILLING"
)

// ContactPurpose is what a party's contact is reached for (C9): BOOKING, DOCUMENTS and ACCOUNTS. Each
// purpose has at most one default contact.
type ContactPurpose string

const (
	ContactBooking   ContactPurpose = "BOOKING"
	ContactDocuments ContactPurpose = "DOCUMENTS"
	ContactAccounts  ContactPurpose = "ACCOUNTS"
)

type AddressID string
type ContactID string

// EntryStatus is the status of an address or a contact. Addresses are ACTIVE or INACTIVE; contacts may also
// be ERASED, when ORG no longer keeps their name and contact details.
type EntryStatus string

const (
	EntryActive   EntryStatus = "ACTIVE"
	EntryInactive EntryStatus = "INACTIVE"
	EntryErased   EntryStatus = "ERASED"
)

// BookAction names the change a revision of an address book records.
type BookAction string

const (
	ActionAddAddress        BookAction = "ADD_ADDRESS"
	ActionChangeAddress     BookAction = "CHANGE_ADDRESS"
	ActionDeactivateAddress BookAction = "DEACTIVATE_ADDRESS"
	ActionSetDefaultAddress BookAction = "SET_DEFAULT_ADDRESS"
	ActionAddContact        BookAction = "ADD_CONTACT"
	ActionChangeContact     BookAction = "CHANGE_CONTACT"
	ActionDeactivateContact BookAction = "DEACTIVATE_CONTACT"
	ActionEraseContact      BookAction = "ERASE_CONTACT"
	ActionSetDefaultContact BookAction = "SET_DEFAULT_CONTACT"
)

// Events of the address book. They carry identifiers only, never addresses, names or contact details.
const (
	EventPartyAddressAdded       = "PartyAddressAdded.v1"
	EventPartyAddressChanged     = "PartyAddressChanged.v1"
	EventPartyAddressDeactivated = "PartyAddressDeactivated.v1"
	EventPartyDefaultAddressSet  = "PartyDefaultAddressSet.v1"
	EventPartyContactAdded       = "PartyContactAdded.v1"
	EventPartyContactChanged     = "PartyContactChanged.v1"
	EventPartyContactDeactivated = "PartyContactDeactivated.v1"
	EventPartyContactErased      = "PartyContactErased.v1"
	EventPartyDefaultContactSet  = "PartyDefaultContactSet.v1"
)

var (
	ErrInvalidAddress   = errors.New("org: invalid address")
	ErrInvalidContact   = errors.New("org: invalid contact")
	ErrInvalidChange    = errors.New("org: invalid address book change")
	ErrShortCodeInUse   = errors.New("org: short code used by another active address of the party")
	ErrEntryExists      = errors.New("org: address or contact id already used for the party")
	ErrUnknownAddress   = errors.New("org: the party has no such address")
	ErrUnknownContact   = errors.New("org: the party has no such contact")
	ErrEntryNotActive   = errors.New("org: address or contact not active")
	ErrNothingChanged   = errors.New("org: the change leaves the address book as it is")
	ErrPartyArchived    = errors.New("org: the party is archived; its address book is closed")
	ErrInconsistentBook = errors.New("org: stored address book is inconsistent")
)

const (
	maxAddressLines = 4
	maxNameRunes    = 200
	maxLineRunes    = 100
	maxCityRunes    = 100
	maxRegionRunes  = 100
	maxPostcode     = 20
	maxReasonRunes  = 500
	maxIDRunes      = 128
)

var (
	shortCodeForm = regexp.MustCompile(`^[A-Z0-9][A-Z0-9_-]{0,19}$`)
	languageForm  = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)
	phoneForm     = regexp.MustCompile(`^\+?[0-9][0-9 ()./-]{2,39}$`)
)

// Address is what ORG keeps of one address of a party (C9, privacy assessment V1.1 section 2): its short code,
// purposes, the name and up to four lines as printed on documents, city, state or province, postcode and
// country, optionally a UN/LOCODE in that country, whether it is a residential address, and optionally a
// version in a local language, such as Chinese, sharing the postcode and country.
type Address struct {
	ShortCode   string
	Purposes    []AddressPurpose
	Name        string
	Lines       []string
	City        string
	Region      string
	Postcode    string
	CountryCode string
	UNLocode    string
	Residential bool
	Local       *LocalAddress
}

// LocalAddress is an address written in a local language: a BCP 47 language tag such as zh, and the name,
// lines, city and region in that language.
type LocalAddress struct {
	Language string
	Name     string
	Lines    []string
	City     string
	Region   string
}

// Contact is what ORG keeps of one contact person of a party (privacy assessment V1.1 section 2): purposes,
// name, and at least one of phone, mobile and e-mail. Nothing else about the person is kept.
type Contact struct {
	Purposes []ContactPurpose
	Name     string
	Phone    string
	Mobile   string
	Email    string
}

// AddressVersion is one immutable version of an address. Revision is the address book revision that recorded it.
type AddressVersion struct {
	ID       AddressID
	Version  int64
	Revision int64
	Status   EntryStatus
	Address
}

// ContactVersion is one version of a contact. When a contact is erased, every version keeps its purposes and
// status but loses the name and contact details, and Erased is set.
type ContactVersion struct {
	ID       ContactID
	Version  int64
	Revision int64
	Status   EntryStatus
	Erased   bool
	Contact
}

// BookChange is the record of one address book revision: what changed, who changed it, why and when ORG
// recorded it. A change of a default names the purpose.
type BookChange struct {
	Revision   int64
	Action     BookAction
	EntryID    string
	Purpose    string
	ActorID    string
	Reason     string
	RecordedAt time.Time
}

// DefaultChange records that a revision made an entry the default for a purpose, or, with an empty EntryID,
// left the purpose without a default.
type DefaultChange struct {
	Revision int64
	Purpose  string
	EntryID  string
}

// Unsaved is what an address book recorded since it was loaded, in the order to write it.
type Unsaved struct {
	Changes         []BookChange
	AddressVersions []AddressVersion
	ContactVersions []ContactVersion
	AddressDefaults []DefaultChange
	ContactDefaults []DefaultChange
	Erased          []ContactID
}

// AddressBook is the aggregate of a party's addresses and contacts (C9). It has a revision of its own, apart
// from the party's identity revision that PartyRef carries, so maintaining addresses never changes a
// frozen PartyRef. Every command is one revision; every change of an address or contact is a new version.
type AddressBook struct {
	tenant          TenantID
	party           PartyID
	loaded          int64
	revision        int64
	addresses       map[AddressID]AddressVersion
	contacts        map[ContactID]ContactVersion
	addressDefaults map[AddressPurpose]AddressID
	contactDefaults map[ContactPurpose]ContactID
	unsaved         Unsaved
	pending         []Event
}

// NewAddressBook opens the empty address book of a party that has none yet; its revision is 0.
func NewAddressBook(tenant TenantID, party PartyID) (*AddressBook, error) {
	if strings.TrimSpace(string(tenant)) == "" || strings.TrimSpace(string(party)) == "" {
		return nil, ErrInvalidIdentity
	}
	return &AddressBook{tenant: tenant, party: party, addresses: map[AddressID]AddressVersion{}, contacts: map[ContactID]ContactVersion{},
		addressDefaults: map[AddressPurpose]AddressID{}, contactDefaults: map[ContactPurpose]ContactID{}}, nil
}

// RestoreAddressBook rebuilds a stored address book from the current version of each address and contact and
// the current defaults, and checks that they agree.
func RestoreAddressBook(tenant TenantID, party PartyID, revision int64, addresses []AddressVersion, contacts []ContactVersion,
	addressDefaults map[AddressPurpose]AddressID, contactDefaults map[ContactPurpose]ContactID) (*AddressBook, error) {
	b, err := NewAddressBook(tenant, party)
	if err != nil {
		return nil, err
	}
	if revision < 1 {
		return nil, fmt.Errorf("%w: revision %d", ErrInconsistentBook, revision)
	}
	b.loaded, b.revision = revision, revision
	codes := map[string]AddressID{}
	for _, a := range addresses {
		if _, dup := b.addresses[a.ID]; dup || a.Version < 1 || a.Revision < 1 || a.Revision > revision ||
			(a.Status != EntryActive && a.Status != EntryInactive) {
			return nil, fmt.Errorf("%w: address %s", ErrInconsistentBook, a.ID)
		}
		normal, err := a.Address.normalized()
		if err != nil {
			return nil, fmt.Errorf("%w: address %s: %v", ErrInconsistentBook, a.ID, err)
		}
		a.Address = normal
		if a.Status == EntryActive {
			if other, taken := codes[a.ShortCode]; taken {
				return nil, fmt.Errorf("%w: short code %s of %s and %s", ErrInconsistentBook, a.ShortCode, other, a.ID)
			}
			codes[a.ShortCode] = a.ID
		}
		b.addresses[a.ID] = a
	}
	for _, c := range contacts {
		if _, dup := b.contacts[c.ID]; dup || c.Version < 1 || c.Revision < 1 || c.Revision > revision ||
			(c.Status != EntryActive && c.Status != EntryInactive && c.Status != EntryErased) || (c.Status == EntryErased) != c.Erased {
			return nil, fmt.Errorf("%w: contact %s", ErrInconsistentBook, c.ID)
		}
		if c.Erased {
			purposes, err := contactPurposes(c.Purposes)
			if err != nil || c.Name != "" || c.Phone != "" || c.Mobile != "" || c.Email != "" {
				return nil, fmt.Errorf("%w: erased contact %s", ErrInconsistentBook, c.ID)
			}
			c.Purposes = purposes
		} else {
			normal, err := c.Contact.normalized()
			if err != nil {
				return nil, fmt.Errorf("%w: contact %s: %v", ErrInconsistentBook, c.ID, err)
			}
			c.Contact = normal
		}
		b.contacts[c.ID] = c
	}
	for purpose, id := range addressDefaults {
		a, ok := b.addresses[id]
		if !ok || a.Status != EntryActive || !slices.Contains(a.Purposes, purpose) {
			return nil, fmt.Errorf("%w: default %s address %s", ErrInconsistentBook, purpose, id)
		}
		b.addressDefaults[purpose] = id
	}
	for purpose, id := range contactDefaults {
		c, ok := b.contacts[id]
		if !ok || c.Status != EntryActive || !slices.Contains(c.Purposes, purpose) {
			return nil, fmt.Errorf("%w: default %s contact %s", ErrInconsistentBook, purpose, id)
		}
		b.contactDefaults[purpose] = id
	}
	return b, nil
}

// AddAddress adds an address at version 1 and makes it the default for the purposes in defaultFor, which
// must be among its own.
func (b *AddressBook) AddAddress(id AddressID, a Address, defaultFor []AddressPurpose, actor string, at time.Time) (AddressVersion, error) {
	if err := b.begin(actor, at); err != nil {
		return AddressVersion{}, err
	}
	if !validID(string(id)) {
		return AddressVersion{}, fmt.Errorf("%w: address id", ErrInvalidAddress)
	}
	if _, used := b.addresses[id]; used {
		return AddressVersion{}, fmt.Errorf("%w: address %s", ErrEntryExists, id)
	}
	a, err := a.normalized()
	if err != nil {
		return AddressVersion{}, err
	}
	defaults, err := defaultPurposes(defaultFor, a.Purposes, ErrInvalidAddress)
	if err != nil {
		return AddressVersion{}, err
	}
	if err = b.shortCodeFree(a.ShortCode, id); err != nil {
		return AddressVersion{}, err
	}
	rev := b.next(ActionAddAddress, string(id), "", actor, "", at)
	v := AddressVersion{ID: id, Version: 1, Revision: rev, Status: EntryActive, Address: a}
	b.putAddress(v)
	for _, p := range defaults {
		b.setAddressDefault(p, id, rev)
	}
	b.record(EventPartyAddressAdded, at, string(id), 1, "")
	return v, nil
}

// ChangeAddress records the next version of an active address. A purpose the address no longer has stops
// being one it is the default for.
func (b *AddressBook) ChangeAddress(id AddressID, a Address, reason, actor string, at time.Time) (AddressVersion, error) {
	if err := b.begin(actor, at); err != nil {
		return AddressVersion{}, err
	}
	current, err := b.activeAddress(id)
	if err != nil {
		return AddressVersion{}, err
	}
	if reason, err = required(reason); err != nil {
		return AddressVersion{}, err
	}
	if a, err = a.normalized(); err != nil {
		return AddressVersion{}, err
	}
	if a.equal(current.Address) {
		return AddressVersion{}, ErrNothingChanged
	}
	if err = b.shortCodeFree(a.ShortCode, id); err != nil {
		return AddressVersion{}, err
	}
	rev := b.next(ActionChangeAddress, string(id), "", actor, reason, at)
	v := AddressVersion{ID: id, Version: current.Version + 1, Revision: rev, Status: EntryActive, Address: a}
	b.putAddress(v)
	for _, p := range sortedKeys(b.addressDefaults) {
		if b.addressDefaults[p] == id && !slices.Contains(a.Purposes, p) {
			b.setAddressDefault(p, "", rev)
		}
	}
	b.record(EventPartyAddressChanged, at, string(id), v.Version, "")
	return v, nil
}

// DeactivateAddress records an INACTIVE version of an active address. It stops being a default; its short
// code may be given to another address. An inactive address stays readable but is not maintained further.
func (b *AddressBook) DeactivateAddress(id AddressID, reason, actor string, at time.Time) (AddressVersion, error) {
	if err := b.begin(actor, at); err != nil {
		return AddressVersion{}, err
	}
	current, err := b.activeAddress(id)
	if err != nil {
		return AddressVersion{}, err
	}
	if reason, err = required(reason); err != nil {
		return AddressVersion{}, err
	}
	rev := b.next(ActionDeactivateAddress, string(id), "", actor, reason, at)
	v := current
	v.Version, v.Revision, v.Status = current.Version+1, rev, EntryInactive
	b.putAddress(v)
	for _, p := range sortedKeys(b.addressDefaults) {
		if b.addressDefaults[p] == id {
			b.setAddressDefault(p, "", rev)
		}
	}
	b.record(EventPartyAddressDeactivated, at, string(id), v.Version, "")
	return v, nil
}

// SetDefaultAddress makes an active address the default for one of its purposes, in place of the previous one.
func (b *AddressBook) SetDefaultAddress(purpose AddressPurpose, id AddressID, actor string, at time.Time) (AddressVersion, error) {
	if err := b.begin(actor, at); err != nil {
		return AddressVersion{}, err
	}
	if !purpose.valid() {
		return AddressVersion{}, fmt.Errorf("%w: purpose %q", ErrInvalidAddress, purpose)
	}
	current, err := b.activeAddress(id)
	if err != nil {
		return AddressVersion{}, err
	}
	if !slices.Contains(current.Purposes, purpose) {
		return AddressVersion{}, fmt.Errorf("%w: address %s is not for %s", ErrInvalidChange, id, purpose)
	}
	if b.addressDefaults[purpose] == id {
		return AddressVersion{}, ErrNothingChanged
	}
	rev := b.next(ActionSetDefaultAddress, string(id), string(purpose), actor, "", at)
	b.setAddressDefault(purpose, id, rev)
	b.record(EventPartyDefaultAddressSet, at, string(id), current.Version, string(purpose))
	return current, nil
}

// AddContact adds a contact at version 1 and makes it the default for the purposes in defaultFor, which must
// be among its own.
func (b *AddressBook) AddContact(id ContactID, c Contact, defaultFor []ContactPurpose, actor string, at time.Time) (ContactVersion, error) {
	if err := b.begin(actor, at); err != nil {
		return ContactVersion{}, err
	}
	if !validID(string(id)) {
		return ContactVersion{}, fmt.Errorf("%w: contact id", ErrInvalidContact)
	}
	if _, used := b.contacts[id]; used {
		return ContactVersion{}, fmt.Errorf("%w: contact %s", ErrEntryExists, id)
	}
	c, err := c.normalized()
	if err != nil {
		return ContactVersion{}, err
	}
	defaults, err := defaultPurposes(defaultFor, c.Purposes, ErrInvalidContact)
	if err != nil {
		return ContactVersion{}, err
	}
	rev := b.next(ActionAddContact, string(id), "", actor, "", at)
	v := ContactVersion{ID: id, Version: 1, Revision: rev, Status: EntryActive, Contact: c}
	b.putContact(v)
	for _, p := range defaults {
		b.setContactDefault(p, id, rev)
	}
	b.record(EventPartyContactAdded, at, string(id), 1, "")
	return v, nil
}

// ChangeContact records the next version of an active contact. A purpose the contact no longer has stops
// being one it is the default for.
func (b *AddressBook) ChangeContact(id ContactID, c Contact, reason, actor string, at time.Time) (ContactVersion, error) {
	if err := b.begin(actor, at); err != nil {
		return ContactVersion{}, err
	}
	current, err := b.activeContact(id)
	if err != nil {
		return ContactVersion{}, err
	}
	if reason, err = required(reason); err != nil {
		return ContactVersion{}, err
	}
	if c, err = c.normalized(); err != nil {
		return ContactVersion{}, err
	}
	if c.equal(current.Contact) {
		return ContactVersion{}, ErrNothingChanged
	}
	rev := b.next(ActionChangeContact, string(id), "", actor, reason, at)
	v := ContactVersion{ID: id, Version: current.Version + 1, Revision: rev, Status: EntryActive, Contact: c}
	b.putContact(v)
	for _, p := range sortedKeys(b.contactDefaults) {
		if b.contactDefaults[p] == id && !slices.Contains(c.Purposes, p) {
			b.setContactDefault(p, "", rev)
		}
	}
	b.record(EventPartyContactChanged, at, string(id), v.Version, "")
	return v, nil
}

// DeactivateContact records an INACTIVE version of an active contact. It stops being a default.
func (b *AddressBook) DeactivateContact(id ContactID, reason, actor string, at time.Time) (ContactVersion, error) {
	if err := b.begin(actor, at); err != nil {
		return ContactVersion{}, err
	}
	current, err := b.activeContact(id)
	if err != nil {
		return ContactVersion{}, err
	}
	if reason, err = required(reason); err != nil {
		return ContactVersion{}, err
	}
	rev := b.next(ActionDeactivateContact, string(id), "", actor, reason, at)
	v := current
	v.Version, v.Revision, v.Status = current.Version+1, rev, EntryInactive
	b.putContact(v)
	b.clearContactDefaults(id, rev)
	b.record(EventPartyContactDeactivated, at, string(id), v.Version, "")
	return v, nil
}

// EraseContact erases an active or inactive contact (privacy assessment V1.1, control 2): an ERASED version
// is recorded, and every earlier version loses the name and contact details. Purposes, statuses and the
// record of who changed what remain. Copies already frozen into orders are COM's to erase.
func (b *AddressBook) EraseContact(id ContactID, reason, actor string, at time.Time) (ContactVersion, error) {
	if err := b.begin(actor, at); err != nil {
		return ContactVersion{}, err
	}
	current, ok := b.contacts[id]
	if !ok {
		return ContactVersion{}, fmt.Errorf("%w: %s", ErrUnknownContact, id)
	}
	if current.Status == EntryErased {
		return ContactVersion{}, ErrNothingChanged
	}
	reason, err := required(reason)
	if err != nil {
		return ContactVersion{}, err
	}
	rev := b.next(ActionEraseContact, string(id), "", actor, reason, at)
	v := ContactVersion{ID: id, Version: current.Version + 1, Revision: rev, Status: EntryErased, Erased: true,
		Contact: Contact{Purposes: current.Purposes}}
	b.unsaved.Erased = append(b.unsaved.Erased, id)
	for i := range b.unsaved.ContactVersions {
		if b.unsaved.ContactVersions[i].ID == id {
			b.unsaved.ContactVersions[i].Erased = true
			b.unsaved.ContactVersions[i].Contact = Contact{Purposes: b.unsaved.ContactVersions[i].Purposes}
		}
	}
	b.putContact(v)
	b.clearContactDefaults(id, rev)
	b.record(EventPartyContactErased, at, string(id), v.Version, "")
	return v, nil
}

// SetDefaultContact makes an active contact the default for one of its purposes, in place of the previous one.
func (b *AddressBook) SetDefaultContact(purpose ContactPurpose, id ContactID, actor string, at time.Time) (ContactVersion, error) {
	if err := b.begin(actor, at); err != nil {
		return ContactVersion{}, err
	}
	if !purpose.valid() {
		return ContactVersion{}, fmt.Errorf("%w: purpose %q", ErrInvalidContact, purpose)
	}
	current, err := b.activeContact(id)
	if err != nil {
		return ContactVersion{}, err
	}
	if !slices.Contains(current.Purposes, purpose) {
		return ContactVersion{}, fmt.Errorf("%w: contact %s is not for %s", ErrInvalidChange, id, purpose)
	}
	if b.contactDefaults[purpose] == id {
		return ContactVersion{}, ErrNothingChanged
	}
	rev := b.next(ActionSetDefaultContact, string(id), string(purpose), actor, "", at)
	b.setContactDefault(purpose, id, rev)
	b.record(EventPartyDefaultContactSet, at, string(id), current.Version, string(purpose))
	return current, nil
}

// RequireMaintainable refuses changes to the address book of an archived party.
func RequireMaintainable(status PartyStatus) error {
	switch status {
	case PartyDraft, PartyActive, PartySuspended:
		return nil
	case PartyArchived:
		return ErrPartyArchived
	}
	return fmt.Errorf("%w: party status %q", ErrInvalidIdentity, status)
}

func (b *AddressBook) Tenant() TenantID      { return b.tenant }
func (b *AddressBook) Party() PartyID        { return b.party }
func (b *AddressBook) Revision() int64       { return b.revision }
func (b *AddressBook) LoadedRevision() int64 { return b.loaded }
func (b *AddressBook) Pending() []Event      { return append([]Event(nil), b.pending...) }
func (b *AddressBook) Acknowledge()          { b.pending = nil }
func (b *AddressBook) Address(id AddressID) (AddressVersion, bool) {
	a, ok := b.addresses[id]
	return cloneAddressVersion(a), ok
}

// Addresses returns the current version of every address, ordered by id.
func (b *AddressBook) Addresses() []AddressVersion {
	out := make([]AddressVersion, 0, len(b.addresses))
	for _, id := range sortedKeys(b.addresses) {
		out = append(out, cloneAddressVersion(b.addresses[id]))
	}
	return out
}

// Contacts returns the current version of every contact, ordered by id.
func (b *AddressBook) Contacts() []ContactVersion {
	out := make([]ContactVersion, 0, len(b.contacts))
	for _, id := range sortedKeys(b.contacts) {
		c := b.contacts[id]
		c.Purposes = slices.Clone(c.Purposes)
		out = append(out, c)
	}
	return out
}

// DefaultAddress returns the default address for a purpose.
func (b *AddressBook) DefaultAddress(p AddressPurpose) (AddressID, bool) {
	id, ok := b.addressDefaults[p]
	return id, ok
}

// DefaultContact returns the default contact for a purpose.
func (b *AddressBook) DefaultContact(p ContactPurpose) (ContactID, bool) {
	id, ok := b.contactDefaults[p]
	return id, ok
}

// AddressDefaultsOf lists the purposes an address is the default for.
func (b *AddressBook) AddressDefaultsOf(id AddressID) []AddressPurpose {
	var out []AddressPurpose
	for _, p := range sortedKeys(b.addressDefaults) {
		if b.addressDefaults[p] == id {
			out = append(out, p)
		}
	}
	return out
}

// ContactDefaultsOf lists the purposes a contact is the default for.
func (b *AddressBook) ContactDefaultsOf(id ContactID) []ContactPurpose {
	var out []ContactPurpose
	for _, p := range sortedKeys(b.contactDefaults) {
		if b.contactDefaults[p] == id {
			out = append(out, p)
		}
	}
	return out
}

// Unsaved returns what the book recorded since it was loaded.
func (b *AddressBook) Unsaved() Unsaved {
	u := b.unsaved
	return Unsaved{
		Changes: slices.Clone(u.Changes), AddressVersions: slices.Clone(u.AddressVersions), ContactVersions: slices.Clone(u.ContactVersions),
		AddressDefaults: slices.Clone(u.AddressDefaults), ContactDefaults: slices.Clone(u.ContactDefaults), Erased: slices.Clone(u.Erased),
	}
}

func (b *AddressBook) begin(actor string, at time.Time) error {
	if strings.TrimSpace(actor) == "" || at.IsZero() {
		return ErrInvalidChange
	}
	return nil
}

func (b *AddressBook) next(action BookAction, entry, purpose, actor, reason string, at time.Time) int64 {
	b.revision++
	b.unsaved.Changes = append(b.unsaved.Changes, BookChange{Revision: b.revision, Action: action, EntryID: entry, Purpose: purpose,
		ActorID: actor, Reason: reason, RecordedAt: at.UTC()})
	return b.revision
}

func (b *AddressBook) record(kind string, at time.Time, entry string, version int64, purpose string) {
	b.pending = append(b.pending, Event{Type: kind, At: at.UTC(), TenantID: b.tenant, SubjectID: string(b.party),
		EntryID: entry, EntryVersion: version, Revision: b.revision, Purpose: purpose})
}

func (b *AddressBook) putAddress(v AddressVersion) {
	b.addresses[v.ID] = v
	b.unsaved.AddressVersions = append(b.unsaved.AddressVersions, cloneAddressVersion(v))
}

func (b *AddressBook) putContact(v ContactVersion) {
	b.contacts[v.ID] = v
	v.Purposes = slices.Clone(v.Purposes)
	b.unsaved.ContactVersions = append(b.unsaved.ContactVersions, v)
}

func (b *AddressBook) setAddressDefault(p AddressPurpose, id AddressID, rev int64) {
	if id == "" {
		delete(b.addressDefaults, p)
	} else {
		b.addressDefaults[p] = id
	}
	b.unsaved.AddressDefaults = append(b.unsaved.AddressDefaults, DefaultChange{Revision: rev, Purpose: string(p), EntryID: string(id)})
}

func (b *AddressBook) setContactDefault(p ContactPurpose, id ContactID, rev int64) {
	if id == "" {
		delete(b.contactDefaults, p)
	} else {
		b.contactDefaults[p] = id
	}
	b.unsaved.ContactDefaults = append(b.unsaved.ContactDefaults, DefaultChange{Revision: rev, Purpose: string(p), EntryID: string(id)})
}

func (b *AddressBook) clearContactDefaults(id ContactID, rev int64) {
	for _, p := range sortedKeys(b.contactDefaults) {
		if b.contactDefaults[p] == id {
			b.setContactDefault(p, "", rev)
		}
	}
}

func (b *AddressBook) activeAddress(id AddressID) (AddressVersion, error) {
	a, ok := b.addresses[id]
	if !ok {
		return AddressVersion{}, fmt.Errorf("%w: %s", ErrUnknownAddress, id)
	}
	if a.Status != EntryActive {
		return AddressVersion{}, fmt.Errorf("%w: address %s is %s", ErrEntryNotActive, id, a.Status)
	}
	return cloneAddressVersion(a), nil
}

func (b *AddressBook) activeContact(id ContactID) (ContactVersion, error) {
	c, ok := b.contacts[id]
	if !ok {
		return ContactVersion{}, fmt.Errorf("%w: %s", ErrUnknownContact, id)
	}
	if c.Status != EntryActive {
		return ContactVersion{}, fmt.Errorf("%w: contact %s is %s", ErrEntryNotActive, id, c.Status)
	}
	c.Purposes = slices.Clone(c.Purposes)
	return c, nil
}

func (b *AddressBook) shortCodeFree(code string, self AddressID) error {
	for id, a := range b.addresses {
		if id != self && a.Status == EntryActive && a.ShortCode == code {
			return fmt.Errorf("%w: %s is used by %s", ErrShortCodeInUse, code, id)
		}
	}
	return nil
}

func (p AddressPurpose) valid() bool {
	switch p {
	case AddressOffice, AddressPickup, AddressDelivery, AddressBilling:
		return true
	}
	return false
}

func (p ContactPurpose) valid() bool {
	switch p {
	case ContactBooking, ContactDocuments, ContactAccounts:
		return true
	}
	return false
}

// normalized trims the address, sorts its purposes and checks every field.
func (a Address) normalized() (Address, error) {
	bad := func(field string) (Address, error) { return Address{}, fmt.Errorf("%w: %s", ErrInvalidAddress, field) }
	out := Address{ShortCode: strings.TrimSpace(a.ShortCode), CountryCode: strings.TrimSpace(a.CountryCode),
		UNLocode: strings.TrimSpace(a.UNLocode), Residential: a.Residential}
	if !shortCodeForm.MatchString(out.ShortCode) {
		return bad("short code")
	}
	purposes, ok := sortedPurposes(a.Purposes, AddressPurpose.valid)
	if !ok {
		return bad("purposes")
	}
	out.Purposes = purposes
	var okName, okLines, okCity, okRegion, okPostcode bool
	out.Name, okName = text(a.Name, 1, maxNameRunes)
	out.Lines, okLines = lines(a.Lines)
	out.City, okCity = text(a.City, 1, maxCityRunes)
	out.Region, okRegion = text(a.Region, 0, maxRegionRunes)
	out.Postcode, okPostcode = text(a.Postcode, 0, maxPostcode)
	switch {
	case !okName:
		return bad("name")
	case !okLines:
		return bad("lines")
	case !okCity:
		return bad("city")
	case !okRegion:
		return bad("region")
	case !okPostcode:
		return bad("postcode")
	case len(out.CountryCode) != 2 || !upperASCII(out.CountryCode):
		return bad("country code")
	}
	if out.UNLocode != "" {
		code := kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: out.UNLocode}
		if !code.Valid() || out.UNLocode[:2] != out.CountryCode {
			return bad("UN/LOCODE")
		}
	}
	if a.Local != nil {
		l := LocalAddress{Language: strings.TrimSpace(a.Local.Language)}
		l.Name, okName = text(a.Local.Name, 1, maxNameRunes)
		l.Lines, okLines = lines(a.Local.Lines)
		l.City, okCity = text(a.Local.City, 1, maxCityRunes)
		l.Region, okRegion = text(a.Local.Region, 0, maxRegionRunes)
		if !languageForm.MatchString(l.Language) || !okName || !okLines || !okCity || !okRegion {
			return bad("local address")
		}
		out.Local = &l
	}
	return out, nil
}

func (a Address) equal(o Address) bool {
	same := a.ShortCode == o.ShortCode && slices.Equal(a.Purposes, o.Purposes) && a.Name == o.Name && slices.Equal(a.Lines, o.Lines) &&
		a.City == o.City && a.Region == o.Region && a.Postcode == o.Postcode && a.CountryCode == o.CountryCode &&
		a.UNLocode == o.UNLocode && a.Residential == o.Residential && (a.Local == nil) == (o.Local == nil)
	if !same || a.Local == nil {
		return same
	}
	l, m := a.Local, o.Local
	return l.Language == m.Language && l.Name == m.Name && slices.Equal(l.Lines, m.Lines) && l.City == m.City && l.Region == m.Region
}

// normalized trims the contact, sorts its purposes and checks every field.
func (c Contact) normalized() (Contact, error) {
	bad := func(field string) (Contact, error) { return Contact{}, fmt.Errorf("%w: %s", ErrInvalidContact, field) }
	purposes, err := contactPurposes(c.Purposes)
	if err != nil {
		return Contact{}, err
	}
	out := Contact{Purposes: purposes, Phone: strings.TrimSpace(c.Phone), Mobile: strings.TrimSpace(c.Mobile), Email: strings.TrimSpace(c.Email)}
	var ok bool
	if out.Name, ok = text(c.Name, 1, maxNameRunes); !ok {
		return bad("name")
	}
	if out.Phone != "" && !phone(out.Phone) {
		return bad("phone")
	}
	if out.Mobile != "" && !phone(out.Mobile) {
		return bad("mobile")
	}
	if out.Email != "" && !email(out.Email) {
		return bad("e-mail")
	}
	if out.Phone == "" && out.Mobile == "" && out.Email == "" {
		return bad("a phone, mobile or e-mail is required")
	}
	return out, nil
}

func (c Contact) equal(o Contact) bool {
	return slices.Equal(c.Purposes, o.Purposes) && c.Name == o.Name && c.Phone == o.Phone && c.Mobile == o.Mobile && c.Email == o.Email
}

func contactPurposes(in []ContactPurpose) ([]ContactPurpose, error) {
	purposes, ok := sortedPurposes(in, ContactPurpose.valid)
	if !ok {
		return nil, fmt.Errorf("%w: purposes", ErrInvalidContact)
	}
	return purposes, nil
}

// sortedPurposes sorts a non-empty list of distinct valid purposes.
func sortedPurposes[P ~string](in []P, valid func(P) bool) ([]P, bool) {
	if len(in) == 0 {
		return nil, false
	}
	out := slices.Clone(in)
	slices.Sort(out)
	for i, p := range out {
		if !valid(p) || (i > 0 && out[i-1] == p) {
			return nil, false
		}
	}
	return out, true
}

// defaultPurposes checks that the purposes an entry is to be the default for are distinct and among its own.
func defaultPurposes[P ~string](in, own []P, kind error) ([]P, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := slices.Clone(in)
	slices.Sort(out)
	for i, p := range out {
		if !slices.Contains(own, p) || (i > 0 && out[i-1] == p) {
			return nil, fmt.Errorf("%w: default for %q", kind, p)
		}
	}
	return out, nil
}

func required(reason string) (string, error) {
	r, ok := text(reason, 1, maxReasonRunes)
	if !ok {
		return "", fmt.Errorf("%w: a reason is required", ErrInvalidChange)
	}
	return r, nil
}

// text trims a single-line value and checks its length in characters; control characters are refused.
func text(s string, minRunes, maxRunes int) (string, bool) {
	s = strings.TrimSpace(s)
	n := utf8.RuneCountInString(s)
	if !utf8.ValidString(s) || n < minRunes || n > maxRunes {
		return "", false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return s, true
}

func lines(in []string) ([]string, bool) {
	if len(in) == 0 || len(in) > maxAddressLines {
		return nil, false
	}
	out := make([]string, len(in))
	for i, l := range in {
		var ok bool
		if out[i], ok = text(l, 1, maxLineRunes); !ok {
			return nil, false
		}
	}
	return out, true
}

func phone(s string) bool {
	if !phoneForm.MatchString(s) {
		return false
	}
	digits := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits++
		}
	}
	return digits >= 3
}

// email checks the form local@domain.tld only; whether the mailbox exists is not ORG's to know.
func email(s string) bool {
	if len(s) > 254 || strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return false
	}
	local, domain, ok := strings.Cut(s, "@")
	if !ok || local == "" || len(local) > 64 || strings.Contains(domain, "@") || !strings.Contains(domain, ".") {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if label == "" {
			return false
		}
	}
	return true
}

func validID(s string) bool {
	_, ok := text(s, 1, maxIDRunes)
	return ok && s == strings.TrimSpace(s)
}

func upperASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}

func cloneAddressVersion(a AddressVersion) AddressVersion {
	a.Purposes, a.Lines = slices.Clone(a.Purposes), slices.Clone(a.Lines)
	if a.Local != nil {
		l := *a.Local
		l.Lines = slices.Clone(l.Lines)
		a.Local = &l
	}
	return a
}

func sortedKeys[K ~string, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
