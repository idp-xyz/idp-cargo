package api

import "context"

// Address purposes (C9). OFFICE is the address for documents and the office. Each purpose has at most one
// default address per party.
const (
	AddressOffice   = "OFFICE"
	AddressPickup   = "PICKUP"
	AddressDelivery = "DELIVERY"
	AddressBilling  = "BILLING"
)

// Contact purposes (C9). Each purpose has at most one default contact per party.
const (
	ContactBooking   = "BOOKING"
	ContactDocuments = "DOCUMENTS"
	ContactAccounts  = "ACCOUNTS"
)

// Statuses of addresses and contacts. An inactive address or contact stays readable; an erased contact has
// lost its name and contact details for good.
const (
	StatusActive   = "ACTIVE"
	StatusInactive = "INACTIVE"
	StatusErased   = "ERASED"
)

// Party actions an Authorizer decides on.
const (
	ActionAddPartyAddress        = "AddPartyAddress"
	ActionChangePartyAddress     = "ChangePartyAddress"
	ActionDeactivatePartyAddress = "DeactivatePartyAddress"
	ActionSetDefaultPartyAddress = "SetDefaultPartyAddress"
	ActionAddPartyContact        = "AddPartyContact"
	ActionChangePartyContact     = "ChangePartyContact"
	ActionDeactivatePartyContact = "DeactivatePartyContact"
	ActionErasePartyContact      = "ErasePartyContact"
	ActionSetDefaultPartyContact = "SetDefaultPartyContact"
	ActionReadPartyContacts      = "ReadPartyContacts"
)

// Address is one address of a party as printed on documents: a short code unique among the party's active
// addresses (letters A-Z, digits, '-' and '_', up to 20), its purposes, the name and one to four lines, city,
// state or province, postcode and ISO 3166 country, optionally a UN/LOCODE in that country, whether it is a
// residential address, and optionally the same address in a local language.
type Address struct {
	ShortCode   string
	Purposes    []string
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

// LocalAddress is the address in a local language, named by a BCP 47 tag such as zh; it shares the postcode
// and country.
type LocalAddress struct {
	Language string
	Name     string
	Lines    []string
	City     string
	Region   string
}

// Contact is a contact person of a party: purposes, name and at least one of phone, mobile and e-mail. It is
// personal data; ORG keeps nothing else about the person.
type Contact struct {
	Purposes []string
	Name     string
	Phone    string
	Mobile   string
	Email    string
}

// AddPartyAddress adds an address to a party's address book and makes it the default for the purposes in
// DefaultFor, which must be among its own. ExpectedRevision is the address book revision the caller read, 0
// for a party without addresses or contacts. Every address book command takes effect when ORG records it.
// TenantID and ActorID must come from a verified principal.
type AddPartyAddress struct {
	CommandID        string
	TenantID         string
	ActorID          string
	PartyID          string
	ExpectedRevision int64
	AddressID        string
	Address          Address
	DefaultFor       []string
}

// ChangePartyAddress records the next version of an active address, with a reason. A purpose the address no
// longer has stops being one it is the default for.
type ChangePartyAddress struct {
	CommandID        string
	TenantID         string
	ActorID          string
	PartyID          string
	ExpectedRevision int64
	AddressID        string
	Address          Address
	Reason           string
}

// DeactivatePartyAddress makes an active address inactive, with a reason. It stops being a default and its
// short code may be given to another address. Copies frozen into orders are unaffected.
type DeactivatePartyAddress struct {
	CommandID        string
	TenantID         string
	ActorID          string
	PartyID          string
	ExpectedRevision int64
	AddressID        string
	Reason           string
}

// SetDefaultPartyAddress makes an active address the default for one of its purposes.
type SetDefaultPartyAddress struct {
	CommandID        string
	TenantID         string
	ActorID          string
	PartyID          string
	ExpectedRevision int64
	Purpose          string
	AddressID        string
}

// AddPartyContact adds a contact and makes it the default for the purposes in DefaultFor.
type AddPartyContact struct {
	CommandID        string
	TenantID         string
	ActorID          string
	PartyID          string
	ExpectedRevision int64
	ContactID        string
	Contact          Contact
	DefaultFor       []string
}

// ChangePartyContact records the next version of an active contact, with a reason.
type ChangePartyContact struct {
	CommandID        string
	TenantID         string
	ActorID          string
	PartyID          string
	ExpectedRevision int64
	ContactID        string
	Contact          Contact
	Reason           string
}

// DeactivatePartyContact makes an active contact inactive, with a reason; it stops being a default.
type DeactivatePartyContact struct {
	CommandID        string
	TenantID         string
	ActorID          string
	PartyID          string
	ExpectedRevision int64
	ContactID        string
	Reason           string
}

// ErasePartyContact erases an active or inactive contact: ORG no longer keeps its name, phone, mobile or
// e-mail in any version. Purposes, statuses and who changed what remain. The reason must not repeat personal
// data. Copies frozen into orders are erased by COM under its retention period.
type ErasePartyContact struct {
	CommandID        string
	TenantID         string
	ActorID          string
	PartyID          string
	ExpectedRevision int64
	ContactID        string
	Reason           string
}

// SetDefaultPartyContact makes an active contact the default for one of its purposes.
type SetDefaultPartyContact struct {
	CommandID        string
	TenantID         string
	ActorID          string
	PartyID          string
	ExpectedRevision int64
	Purpose          string
	ContactID        string
}

// AddressBookReceipt confirms a committed address book command: the book's new revision and the address or
// contact concerned with its version. Replaying the same command returns the same receipt.
type AddressBookReceipt struct {
	CommandID    string
	PartyID      string
	NewRevision  int64
	EntryID      string
	EntryVersion int64
}

// AddressBookCommands maintains parties' addresses and contacts (C9). Callers branch on the error kinds:
// ErrConflict for a stale ExpectedRevision or an id already used, ErrNotFound for an unknown party, address
// or contact, ErrRejected for an invalid address or contact, a short code in use, an entry not active, a
// change that changes nothing or an archived party.
type AddressBookCommands interface {
	AddPartyAddress(ctx context.Context, cmd AddPartyAddress) (AddressBookReceipt, error)
	ChangePartyAddress(ctx context.Context, cmd ChangePartyAddress) (AddressBookReceipt, error)
	DeactivatePartyAddress(ctx context.Context, cmd DeactivatePartyAddress) (AddressBookReceipt, error)
	SetDefaultPartyAddress(ctx context.Context, cmd SetDefaultPartyAddress) (AddressBookReceipt, error)
	AddPartyContact(ctx context.Context, cmd AddPartyContact) (AddressBookReceipt, error)
	ChangePartyContact(ctx context.Context, cmd ChangePartyContact) (AddressBookReceipt, error)
	DeactivatePartyContact(ctx context.Context, cmd DeactivatePartyContact) (AddressBookReceipt, error)
	ErasePartyContact(ctx context.Context, cmd ErasePartyContact) (AddressBookReceipt, error)
	SetDefaultPartyContact(ctx context.Context, cmd SetDefaultPartyContact) (AddressBookReceipt, error)
}

// GetPartyAddresses asks for the current version of every address of a party. TenantID must come from a
// verified principal. Ask before opening your own transaction.
type GetPartyAddresses struct {
	TenantID string
	PartyID  string
}

// PartyAddresses is a party's address book without contacts, at Revision (0 when it has none).
type PartyAddresses struct {
	PartyID   string
	Revision  int64
	Addresses []PartyAddress
}

// PartyAddress is one version of an address: what COM freezes on an order, with the address id, short code
// and version as its source. DefaultFor lists the purposes the address is the default for at BookRevision.
type PartyAddress struct {
	PartyID      string
	AddressID    string
	Version      int64
	Status       string
	DefaultFor   []string
	BookRevision int64
	Address
}

// GetPartyAddress asks for one version of an address; Version 0 means the current one.
type GetPartyAddress struct {
	TenantID  string
	PartyID   string
	AddressID string
	Version   int64
}

// GetPartyContacts asks for the current version of every contact of a party. Contacts are personal data:
// ActorID, from a verified principal, must be authorized for ActionReadPartyContacts.
type GetPartyContacts struct {
	TenantID string
	ActorID  string
	PartyID  string
}

// PartyContacts is a party's contacts at the address book Revision (0 when it has none).
type PartyContacts struct {
	PartyID  string
	Revision int64
	Contacts []PartyContact
}

// PartyContact is the current version of a contact. An erased contact has status ERASED and no name or
// contact details.
type PartyContact struct {
	ContactID  string
	Version    int64
	Status     string
	DefaultFor []string
	Contact
}

// AddressBookQueries is ORG's read side of address books for other contexts, such as COM carrying addresses
// and contacts onto an order by role (C9). An unknown party, address or address version yields ErrNotFound.
type AddressBookQueries interface {
	GetPartyAddresses(ctx context.Context, q GetPartyAddresses) (PartyAddresses, error)
	GetPartyAddress(ctx context.Context, q GetPartyAddress) (PartyAddress, error)
	GetPartyContacts(ctx context.Context, q GetPartyContacts) (PartyContacts, error)
}
