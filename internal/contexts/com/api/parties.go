package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Transport modes and movement types an order may request (C11). CargoWise shipments carry the transport mode
// and record a pickup mode and a delivery mode; a movement type maps to that pair.
const (
	ModeAir  = "AIR"
	ModeSea  = "SEA"
	ModeRoad = "ROAD"
	ModeRail = "RAIL"

	DoorToDoor = "DOOR_TO_DOOR"
	DoorToPort = "DOOR_TO_PORT"
	PortToDoor = "PORT_TO_DOOR"
	PortToPort = "PORT_TO_PORT"
)

// Order party roles (C9). The ordering party is the order's customer, not one of these.
const (
	PartyShipper   = "SHIPPER"
	PartyConsignee = "CONSIGNEE"
	PartyNotify    = "NOTIFY_PARTY"
)

// Party kinds and address sources.
const (
	KindParty   = "PARTY"
	KindToOrder = "TO_ORDER"
	FromORG     = "ORG"
	FromOrder   = "ORDER"
)

// Contact uses: DEFAULT (or empty) carries the party's default DOCUMENTS contact from ORG, if it has one;
// CONTACT a named ORG contact; OVERRIDE a contact given on the order; NONE no contact.
const (
	ContactDefault  = "DEFAULT"
	ContactNamed    = "CONTACT"
	ContactOverride = "OVERRIDE"
	ContactNone     = "NONE"
)

// Actions an Authorizer decides on, besides those of commands.go.
const (
	ActionSetOrderParty         = "SetOrderParty"
	ActionChangeOrderTransport  = "ChangeOrderTransport"
	ActionReadOrderContacts     = "ReadOrderContacts"
	ActionLockOrderParties      = "LockOrderParties"
	ActionReleaseOrderPartyLock = "ReleaseOrderPartyLock"
)

// Transport is the requested transport mode, movement type and container mode of an order; each may be empty.
// ContainerMode (decision N1-PHY OD-9) is LCL or FCL on SEA, LOOSE on AIR, and none on ROAD or RAIL yet.
type Transport struct {
	Mode          string
	Movement      string
	ContainerMode string `json:",omitzero"`
}

// Container modes of an order's transport.
const (
	ContainerLCL   = "LCL"
	ContainerFCL   = "FCL"
	ContainerLoose = "LOOSE"
)

// Address is an address given on the order to override ORG (C9).
type Address struct {
	Name        string
	Lines       []string
	City        string
	Region      string
	Postcode    string
	CountryCode string
	UNLocode    string
	Residential bool
	Phone       string
	Email       string
	Local       *LocalAddress
}

// LocalAddress is an address in a local language, such as Chinese.
type LocalAddress struct {
	Language string
	Name     string
	Lines    []string
	City     string
	Region   string
}

// Contact is a contact given on the order to override ORG. It is personal data.
type Contact struct {
	Name   string
	Phone  string
	Mobile string
	Email  string
}

// How a contact's consent was obtained (privacy assessment V1.2, section 4; decision P4).
const (
	ConsentCustomerUndertaking = "CUSTOMER_UNDERTAKING"
	ConsentSubjectConfirmed    = "SUBJECT_CONFIRMED"
)

// ContactConsent is the version of the tenant's personal information rules a person was told of and the
// separate consents the person gave to the contact being provided to others and abroad (decisions P4, P7).
// A contact carried from ORG brings the consent ORG recorded, with ORGRevision; a contact given on the order
// may bring one (ORGRevision 0). DOC and BOOK check it before they send the contact out.
type ContactConsent struct {
	NoticeVersion   string
	ProvideToOthers bool
	CrossBorder     bool
	Basis           string
	EvidenceRef     string
	ORGRevision     int64
}

// AddressChoice says which address an order party gets for one purpose. Empty: the party's default in ORG
// for that purpose (OFFICE for the documentary address, PICKUP, DELIVERY). AddressID: another active ORG
// address of the party. Override: an address given on the order, with a reason; it is never written back to
// ORG, and names the ORG address it replaces (AddressID, or the default) when there is one.
type AddressChoice struct {
	AddressID string
	Override  *Address
	Reason    string
}

// ContactChoice says which contact an order party gets. Contacts are read from ORG with the caller's own
// authorization for ORG's ReadPartyContacts, for the purpose ORDER_PARTY, and bring their consent. A contact
// given on the order (Override) may bring its consent in Consent.
type ContactChoice struct {
	Use       string
	ContactID string
	Override  *Contact
	Consent   *ContactConsent
	Reason    string
}

// OrderParty is a party given for a role of the order. A named party is an ORG party: the shipper and the
// consignee must hold that role in ORG for the quote's legal entity at the order's business time (C9); the
// notify party need only be an active ORG party (C13). The consignee of a SEA order may instead be ToOrder,
// TO ORDER or TO ORDER OF a holder, as text, with a notify party (C9, C11). Pickup applies to the shipper and
// Delivery to the consignee; they are required where the movement starts or ends at a door (C14).
type OrderParty struct {
	Role     string
	PartyID  string
	ToOrder  string
	Address  AddressChoice
	Pickup   *AddressChoice
	Delivery *AddressChoice
	Contact  ContactChoice
}

// SetOrderParty sets or replaces one party of an order (C12): until the order is cancelled, as a new version,
// with a reason when it replaces one. TenantID and ActorID must come from a verified principal; ActorID is
// also the identity contacts are read from ORG with.
type SetOrderParty struct {
	CommandID        string
	TenantID         string
	ActorID          string
	OrderID          string
	ExpectedRevision int64
	Party            OrderParty
	Reason           string
	OccurredAt       time.Time
}

// ChangeOrderTransport records the order's next transport mode and movement type, with a reason (C11). The
// parties must still fit: a TO ORDER consignee only on SEA, and pickup and delivery addresses where the
// movement starts or ends at a door.
type ChangeOrderTransport struct {
	CommandID        string
	TenantID         string
	ActorID          string
	OrderID          string
	ExpectedRevision int64
	Transport        Transport
	Reason           string
	OccurredAt       time.Time
}

// LockOrderParties locks the order's parties and transport for a transport document, OrderPartyLock v1 (C12,
// DOC OD-2 option A): from the document's issue until it is cancelled or amended with other parties, COM
// refuses SetOrderParty and ChangeOrderTransport (ORDER_PARTIES_LOCKED). HolderID is the DOC document id;
// ExpectedRevision is the order revision the caller read the parties at. A holder holds one lock per order;
// several documents may each hold one. COM never asks DOC: the API client locks for the document (T6).
type LockOrderParties struct {
	CommandID        string
	TenantID         string
	ActorID          string
	OrderID          string
	ExpectedRevision int64
	HolderID         string
	OccurredAt       time.Time
}

// ReleaseOrderPartyLock releases the lock a transport document holds on the order's parties.
type ReleaseOrderPartyLock struct {
	CommandID        string
	TenantID         string
	ActorID          string
	OrderID          string
	ExpectedRevision int64
	HolderID         string
	OccurredAt       time.Time
}

// OrderPartyCommands is COM's entry point for order parties and transport (C9, C11-C14) and their locks (C12).
// Errors: ErrRejected for a party or address ORG does not confirm, a rule of the transport broken, a missing
// address, or parties a transport document holds locked; ErrConflict for a stale revision; ErrUnauthorized also
// when ORG refuses the caller its contacts.
type OrderPartyCommands interface {
	SetOrderParty(ctx context.Context, cmd SetOrderParty) (Receipt, error)
	ChangeOrderTransport(ctx context.Context, cmd ChangeOrderTransport) (Receipt, error)
	LockOrderParties(ctx context.Context, cmd LockOrderParties) (Receipt, error)
	ReleaseOrderPartyLock(ctx context.Context, cmd ReleaseOrderPartyLock) (Receipt, error)
}

// GetOrderParties asks for an order's transport and current parties. WithContacts adds the frozen contacts,
// for callers the Authorizer allows ActionReadOrderContacts (FUL, DOC). Once the order is closed or cancelled
// its contacts are restricted and need ActionReadRestrictedOrderContacts instead; erased contacts are never
// returned (P1).
//
// With contacts, Purpose says why they are read; restricted contacts are read only for CUSTOMS_AUDIT,
// LEGAL_CLAIM or DATA_SUBJECT_REQUEST (PIPL article 47). COM logs every read of contacts with its purpose; a
// read it cannot log returns nothing.
type GetOrderParties struct {
	TenantID     string
	ActorID      string
	OrderID      string
	WithContacts bool
	Purpose      kernel.AccessPurpose
}

// PartyIdentifier is an identifier of an ORG party, as frozen with the party.
type PartyIdentifier struct {
	Type    string
	Country string
	Value   string
	None    bool
}

// FrozenAddress is an address frozen on the order, with its source: ORG (address id, short code and
// version) or the order (who overrode it and why, and the ORG address it replaced, if any).
type FrozenAddress struct {
	Source         string
	ORGAddressID   string
	ShortCode      string
	ORGVersion     int64
	Address        Address
	OverriddenBy   string
	OverrideReason string
}

// FrozenContact is a contact frozen on the order, returned only with WithContacts.
type FrozenContact struct {
	Source         string
	ORGContactID   string
	ORGVersion     int64
	Contact        Contact
	OverriddenBy   string
	OverrideReason string
	Consent        *ContactConsent // nil when none was recorded
}

// OrderPartyView is the current version of an order party: OrderParties v1.
type OrderPartyView struct {
	Role          string
	Version       int64
	Kind          string
	ToOrder       string
	PartyID       string
	PartyRevision int64
	LegalName     string
	EnglishName   string
	CountryCode   string
	Identifiers   []PartyIdentifier
	RoleCode      string
	Address       *FrozenAddress
	Pickup        *FrozenAddress
	Delivery      *FrozenAddress
	HasContact    bool
	Contact       *FrozenContact
	Overridden    bool
	Reason        string
	SetBy         string
	SetAt         time.Time
}

// PartyLock is a lock in force on the order's parties and transport: the transport document holding it.
type PartyLock struct {
	HolderID string
	LockedBy string
	LockedAt time.Time
}

// OrderParties is an order's transport and current parties at Revision, where its contacts stand, and the
// locks transport documents hold on them (C12). CustomerPartyID is the ordering party: a copy of a document sent to
// it provides no one's contacts to others (P5).
type OrderParties struct {
	OrderID         string
	Revision        int64
	CustomerPartyID string
	Transport       Transport
	Parties         []OrderPartyView
	Contacts        ContactsStatus
	Locks           []PartyLock
}

// OrderPartyQueries is the read side of order parties.
type OrderPartyQueries interface {
	GetOrderParties(ctx context.Context, q GetOrderParties) (OrderParties, error)
}
