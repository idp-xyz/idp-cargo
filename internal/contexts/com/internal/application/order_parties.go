package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
)

// Order party actions an Authorizer decides on (C9, C11-C14).
const (
	ActionSetOrderParty        OrderAction = "SetOrderParty"
	ActionChangeOrderTransport OrderAction = "ChangeOrderTransport"
	ActionReadOrderContacts    OrderAction = "ReadOrderContacts"
)

// ORG address purposes and the contact purpose an order carries (C9).
const (
	PurposeOffice    = "OFFICE"
	PurposePickup    = "PICKUP"
	PurposeDelivery  = "DELIVERY"
	ContactDocuments = "DOCUMENTS"
)

// Contact uses of a ContactChoice.
const (
	ContactDefault  = "DEFAULT"
	ContactNamed    = "CONTACT"
	ContactOverride = "OVERRIDE"
	ContactNone     = "NONE"
)

var ErrPartyNotConfirmed = errors.New("com application: ORG did not confirm the order party")

// AddressChoice says which address an order party gets for one purpose: the party's ORG default (empty), a
// named ORG address, or an address given on the order with a reason (Override; only its fields are used).
type AddressChoice struct {
	AddressID string               `json:",omitempty"`
	Override  *com.AddressSnapshot `json:",omitempty"`
	Reason    string               `json:",omitempty"`
}

// ContactChoice says which contact an order party gets (ContactDefault when Use is empty).
type ContactChoice struct {
	Use       string               `json:",omitempty"`
	ContactID string               `json:",omitempty"`
	Override  *com.ContactSnapshot `json:",omitempty"`
	Reason    string               `json:",omitempty"`
}

// PartyInput is an order party as a command gives it.
type PartyInput struct {
	Role     string
	PartyID  string         `json:",omitempty"`
	ToOrder  string         `json:",omitempty"`
	Address  AddressChoice  `json:",omitzero"`
	Pickup   *AddressChoice `json:",omitempty"`
	Delivery *AddressChoice `json:",omitempty"`
	Contact  ContactChoice  `json:",omitzero"`
}

// Directory is what COM asks ORG about order parties. It is called outside COM's transactions (rule T3).
type Directory interface {
	// ConfirmedParty asks ORG to confirm a party in a role for a legal entity at an instant (C9) and returns
	// its record with names and identifiers.
	ConfirmedParty(ctx context.Context, tenant com.TenantID, partyID, role string, entity com.LegalEntityID, at time.Time) (com.PartyRecord, error)
	// ActiveParty returns the record of an active ORG party, without a role (C13).
	ActiveParty(ctx context.Context, tenant com.TenantID, partyID string) (com.PartyRecord, error)
	// Address returns an active ORG address of the party: the named one, or, with addressID empty, the
	// party's default for the purpose; found is false when the party has no default.
	Address(ctx context.Context, tenant com.TenantID, partyID, addressID, purpose string) (a com.AddressSnapshot, found bool, err error)
	// Contact returns an active ORG contact of the party, read with the actor's own authorization: the named
	// one, or, with contactID empty, the default DOCUMENTS contact; found is false when there is none.
	Contact(ctx context.Context, tenant com.TenantID, actor, partyID, contactID string) (c com.ContactSnapshot, found bool, err error)
}

// resolveParties turns the parties a command gives into order party versions, asking ORG. entity and at are
// the quote's legal entity and the command's business time; t is the order's transport.
func resolveParties(ctx context.Context, dir Directory, s Scope, entity com.LegalEntityID, at time.Time, t com.Transport, inputs []PartyInput) ([]com.OrderParty, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	if dir == nil {
		return nil, fmt.Errorf("%w: COM has no ORG directory for order parties", ErrMissingDependency)
	}
	out := make([]com.OrderParty, 0, len(inputs))
	for _, in := range inputs {
		p, err := resolveParty(ctx, dir, s, entity, at, t, in)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func resolveParty(ctx context.Context, dir Directory, s Scope, entity com.LegalEntityID, at time.Time, t com.Transport, in PartyInput) (com.OrderParty, error) {
	p := com.OrderParty{Role: in.Role}
	if strings.TrimSpace(in.ToOrder) != "" {
		if in.PartyID != "" || in.Address != (AddressChoice{}) || in.Pickup != nil || in.Contact != (ContactChoice{}) {
			return com.OrderParty{}, fmt.Errorf("%w: a TO ORDER consignee is text only, with at most a delivery address given on the order", com.ErrInvalidParty)
		}
		p.Kind, p.ToOrderText = com.KindToOrder, in.ToOrder
		if in.Delivery != nil {
			if in.Delivery.Override == nil || in.Delivery.AddressID != "" {
				return com.OrderParty{}, fmt.Errorf("%w: a TO ORDER consignee's delivery address is given on the order", com.ErrInvalidParty)
			}
			d, err := override(*in.Delivery, nil, s.ActorID)
			if err != nil {
				return com.OrderParty{}, err
			}
			p.Delivery = &d
		}
		return p, nil
	}
	if strings.TrimSpace(in.PartyID) == "" {
		return com.OrderParty{}, fmt.Errorf("%w: %s needs an ORG party or, for the consignee, TO ORDER", com.ErrInvalidParty, in.Role)
	}
	p.Kind = com.KindParty
	var err error
	switch in.Role {
	case com.PartyShipper, com.PartyConsignee:
		p.Party, err = dir.ConfirmedParty(ctx, s.TenantID, in.PartyID, in.Role, entity, at)
	case com.PartyNotify:
		p.Party, err = dir.ActiveParty(ctx, s.TenantID, in.PartyID)
	default:
		return com.OrderParty{}, fmt.Errorf("%w: role %q", com.ErrInvalidParty, in.Role)
	}
	if err != nil {
		return com.OrderParty{}, err
	}
	address, err := chooseAddress(ctx, dir, s, in.PartyID, in.Address, PurposeOffice)
	if err != nil {
		return com.OrderParty{}, err
	}
	p.Address = &address
	if in.Pickup != nil || (in.Role == com.PartyShipper && t.PickupAtDoor()) {
		if in.Role != com.PartyShipper {
			return com.OrderParty{}, fmt.Errorf("%w: only the shipper has a pickup address", com.ErrInvalidParty)
		}
		pickup, err := chooseAddress(ctx, dir, s, in.PartyID, orEmpty(in.Pickup), PurposePickup)
		if err != nil {
			return com.OrderParty{}, err
		}
		p.Pickup = &pickup
	}
	if in.Delivery != nil || (in.Role == com.PartyConsignee && t.DeliveryAtDoor()) {
		if in.Role != com.PartyConsignee {
			return com.OrderParty{}, fmt.Errorf("%w: only the consignee has a delivery address", com.ErrInvalidParty)
		}
		delivery, err := chooseAddress(ctx, dir, s, in.PartyID, orEmpty(in.Delivery), PurposeDelivery)
		if err != nil {
			return com.OrderParty{}, err
		}
		p.Delivery = &delivery
	}
	p.Contact, err = chooseContact(ctx, dir, s, in.PartyID, in.Contact)
	return p, err
}

// chooseAddress returns the address for a purpose: an override (naming the ORG address it replaces), a named
// ORG address, or the ORG default; without a default the order must choose one.
func chooseAddress(ctx context.Context, dir Directory, s Scope, party string, c AddressChoice, purpose string) (com.AddressSnapshot, error) {
	if c.Override != nil {
		var replaced *com.AddressSnapshot
		if a, found, err := dir.Address(ctx, s.TenantID, party, c.AddressID, purpose); err != nil {
			return com.AddressSnapshot{}, err
		} else if found {
			replaced = &a
		}
		return override(c, replaced, s.ActorID)
	}
	if c.Reason != "" {
		return com.AddressSnapshot{}, fmt.Errorf("%w: a reason belongs to an address given on the order", com.ErrInvalidParty)
	}
	a, found, err := dir.Address(ctx, s.TenantID, party, c.AddressID, purpose)
	if err != nil {
		return com.AddressSnapshot{}, err
	}
	if !found {
		return com.AddressSnapshot{}, fmt.Errorf("%w: party %s has no default %s address; choose one or give it on the order", com.ErrAddressRequired, party, purpose)
	}
	return a, nil
}

func override(c AddressChoice, replaced *com.AddressSnapshot, actor string) (com.AddressSnapshot, error) {
	if strings.TrimSpace(c.Reason) == "" {
		return com.AddressSnapshot{}, fmt.Errorf("%w: an address given on the order needs a reason", com.ErrInvalidParty)
	}
	a := *c.Override
	a.Source, a.OverriddenBy, a.OverrideReason = com.FromOrder, actor, strings.TrimSpace(c.Reason)
	a.ORGAddressID, a.ShortCode, a.ORGVersion = "", "", 0
	if replaced != nil {
		a.ORGAddressID, a.ORGVersion = replaced.ORGAddressID, replaced.ORGVersion
	}
	return a, nil
}

func chooseContact(ctx context.Context, dir Directory, s Scope, party string, c ContactChoice) (*com.ContactSnapshot, error) {
	switch c.Use {
	case ContactNone:
		return nil, nil
	case ContactOverride:
		if c.Override == nil || strings.TrimSpace(c.Reason) == "" {
			return nil, fmt.Errorf("%w: a contact given on the order needs the contact and a reason", com.ErrInvalidParty)
		}
		k := *c.Override
		k.Source, k.OverriddenBy, k.OverrideReason, k.ORGContactID, k.ORGVersion = com.FromOrder, s.ActorID, strings.TrimSpace(c.Reason), "", 0
		return &k, nil
	case "", ContactDefault, ContactNamed:
		if c.Use == ContactNamed && c.ContactID == "" || c.Use != ContactNamed && c.ContactID != "" {
			return nil, fmt.Errorf("%w: CONTACT names a contact, DEFAULT does not", com.ErrInvalidParty)
		}
		k, found, err := dir.Contact(ctx, s.TenantID, s.ActorID, party, c.ContactID)
		if err != nil || !found {
			return nil, err
		}
		return &k, nil
	}
	return nil, fmt.Errorf("%w: contact use %q", com.ErrInvalidParty, c.Use)
}

func orEmpty(c *AddressChoice) AddressChoice {
	if c == nil {
		return AddressChoice{}
	}
	return *c
}

type SetOrderPartyCommand struct {
	CommandID        string
	Scope            Scope
	OrderID          com.OrderID
	ExpectedRevision int64
	Party            PartyInput
	Reason           string
	OccurredAt       time.Time
}

// SetOrderParty sets or replaces one party of an order (C9, C12). COM reads the order for its legal entity and
// transport, asks ORG outside its transaction, then writes the party version if the order is still at the
// expected revision; the domain checks the transport's rules again.
type SetOrderParty struct {
	Deps
	Directory Directory
}

func (h SetOrderParty) Handle(ctx context.Context, c SetOrderPartyCommand) (Receipt, error) {
	hash, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	entered, err := h.admit(c.OccurredAt)
	if err != nil {
		return Receipt{}, err
	}
	if err = h.authorize(ctx, c.Scope, ActionSetOrderParty, c.OrderID); err != nil {
		return Receipt{}, err
	}
	var entity com.LegalEntityID
	var transport com.Transport
	err = h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		order, _, err := h.Orders.Load(txCtx, c.Scope.TenantID, c.OrderID)
		if err != nil {
			return err
		}
		entity, transport = order.State().Quote.LegalEntityID, order.Transport()
		return nil
	})
	if err != nil {
		return Receipt{}, err
	}
	parties, err := resolveParties(ctx, h.Directory, c.Scope, entity, c.OccurredAt, transport, []PartyInput{c.Party})
	if err != nil {
		return Receipt{}, err
	}
	party := parties[0]
	party.Reason = c.Reason
	return h.change(ctx, c.Scope, c.CommandID, hash, c.OrderID, func(order *com.CustomerOrder, revision int64) error {
		if revision != c.ExpectedRevision {
			return com.ErrConflict
		}
		return order.SetParty(party, c.Scope.ActorID, c.OccurredAt, entered)
	})
}

type ChangeOrderTransportCommand struct {
	CommandID        string
	Scope            Scope
	OrderID          com.OrderID
	ExpectedRevision int64
	Transport        com.Transport
	Reason           string
	OccurredAt       time.Time
}

// ChangeOrderTransport records the order's next transport mode and movement type (C11).
type ChangeOrderTransport struct{ Deps }

func (h ChangeOrderTransport) Handle(ctx context.Context, c ChangeOrderTransportCommand) (Receipt, error) {
	hash, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	entered, err := h.admit(c.OccurredAt)
	if err != nil {
		return Receipt{}, err
	}
	if err = h.authorize(ctx, c.Scope, ActionChangeOrderTransport, c.OrderID); err != nil {
		return Receipt{}, err
	}
	return h.change(ctx, c.Scope, c.CommandID, hash, c.OrderID, func(order *com.CustomerOrder, revision int64) error {
		if revision != c.ExpectedRevision {
			return com.ErrConflict
		}
		return order.ChangeTransport(c.Transport, c.Reason, c.Scope.ActorID, c.OccurredAt, entered)
	})
}

// PartyReader reads an order with its parties for queries, and the contacts frozen on it.
type PartyReader interface {
	ReadOrder(ctx context.Context, tenant com.TenantID, id com.OrderID) (*com.CustomerOrder, int64, error)
	ReadContacts(ctx context.Context, tenant com.TenantID, id com.OrderID) (map[string]map[int64]com.ContactSnapshot, error)
}

// GetOrderParties returns an order's transport and current parties; contacts only to callers authorized for
// ReadOrderContacts (privacy assessment V1.1, control 6). Once the order is closed or cancelled its contacts
// are restricted: only callers authorized for ReadRestrictedOrderContacts get them (P1). Erased contacts are
// never returned.
type GetOrderParties struct {
	Tx     TxRunner
	Orders PartyReader
	Auth   Authorization
}

func (h GetOrderParties) Handle(ctx context.Context, s Scope, id com.OrderID, withContacts bool) (*com.CustomerOrder, int64, map[string]map[int64]com.ContactSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, nil, err
	}
	if h.Tx == nil || h.Orders == nil || h.Auth == nil {
		return nil, 0, nil, ErrMissingDependency
	}
	if s.TenantID == "" {
		return nil, 0, nil, ErrUnauthenticated
	}
	if strings.TrimSpace(string(id)) == "" {
		return nil, 0, nil, ErrInvalidQuery
	}
	if scoped, ok := h.Tx.(interface{ TenantScope() string }); ok && scoped.TenantScope() != string(s.TenantID) {
		return nil, 0, nil, ErrUnauthorized
	}
	if withContacts && strings.TrimSpace(s.ActorID) == "" {
		return nil, 0, nil, ErrUnauthenticated
	}
	order, revision, _, err := h.read(ctx, s, id, false)
	if err != nil || !withContacts {
		return order, revision, nil, err
	}
	// The caller is authorized for the order as it was read; if it was closed or cancelled since, again.
	restricted := order.ContactsRestricted()
	if err = h.authorizeContacts(ctx, s, id, restricted); err != nil {
		return nil, 0, nil, err
	}
	order, revision, contacts, err := h.read(ctx, s, id, true)
	if err != nil {
		return nil, 0, nil, err
	}
	if order.ContactsRestricted() && !restricted {
		if err = h.authorizeContacts(ctx, s, id, true); err != nil {
			return nil, 0, nil, err
		}
	}
	return order, revision, contacts, nil
}

func (h GetOrderParties) authorizeContacts(ctx context.Context, s Scope, id com.OrderID, restricted bool) error {
	action := ActionReadOrderContacts
	if restricted {
		action = ActionReadRestrictedOrderContacts
	}
	if err := h.Auth.RequireOrderAction(ctx, s, action, id); err != nil {
		return fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	return nil
}

func (h GetOrderParties) read(ctx context.Context, s Scope, id com.OrderID, withContacts bool) (order *com.CustomerOrder, revision int64, contacts map[string]map[int64]com.ContactSnapshot, err error) {
	err = h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var err error
		if order, revision, err = h.Orders.ReadOrder(txCtx, s.TenantID, id); err != nil || !withContacts {
			return err
		}
		contacts, err = h.Orders.ReadContacts(txCtx, s.TenantID, id)
		return err
	})
	return order, revision, contacts, err
}
