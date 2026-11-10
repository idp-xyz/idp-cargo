package api

import (
	"context"
	"time"
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
	ActionSetOrderParty        = "SetOrderParty"
	ActionChangeOrderTransport = "ChangeOrderTransport"
	ActionReadOrderContacts    = "ReadOrderContacts"
)

// Transport is the requested transport mode and movement type of an order; either may be empty.
type Transport struct {
	Mode     string
	Movement string
}

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
// authorization for ORG's ReadPartyContacts.
type ContactChoice struct {
	Use       string
	ContactID string
	Override  *Contact
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

// OrderPartyCommands is COM's entry point for order parties and transport (C9, C11-C14). Errors:
// ErrRejected for a party or address ORG does not confirm, a rule of the transport broken, or a missing
// address; ErrConflict for a stale revision; ErrUnauthorized also when ORG refuses the caller its contacts.
type OrderPartyCommands interface {
	SetOrderParty(ctx context.Context, cmd SetOrderParty) (Receipt, error)
	ChangeOrderTransport(ctx context.Context, cmd ChangeOrderTransport) (Receipt, error)
}

// GetOrderParties asks for an order's transport and current parties. WithContacts adds the frozen contacts,
// for callers the Authorizer allows ActionReadOrderContacts (FUL, DOC).
type GetOrderParties struct {
	TenantID     string
	ActorID      string
	OrderID      string
	WithContacts bool
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

// OrderParties is an order's transport and current parties at Revision.
type OrderParties struct {
	OrderID   string
	Revision  int64
	Transport Transport
	Parties   []OrderPartyView
}

// OrderPartyQueries is the read side of order parties.
type OrderPartyQueries interface {
	GetOrderParties(ctx context.Context, q GetOrderParties) (OrderParties, error)
}
