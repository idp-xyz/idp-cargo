package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	org "go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Result types of command receipts.
const (
	ResultRoleApproval = "BusinessPartyRoleApproved"
	ResultAddressBook  = "PartyAddressBook"
)

// Party actions an authorizer decides on, besides role approval: the nine address book commands and reading
// a party's contacts, which privacy assessment V1.1 (control 1) authorizes separately.
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
	ActionRecordContactConsent   = "RecordContactConsent"
	ActionReadContactAccessLog   = "ReadContactAccessLog"
)

var commandActions = map[org.BookAction]string{
	org.ActionAddAddress: ActionAddPartyAddress, org.ActionChangeAddress: ActionChangePartyAddress,
	org.ActionDeactivateAddress: ActionDeactivatePartyAddress, org.ActionSetDefaultAddress: ActionSetDefaultPartyAddress,
	org.ActionAddContact: ActionAddPartyContact, org.ActionChangeContact: ActionChangePartyContact,
	org.ActionDeactivateContact: ActionDeactivatePartyContact, org.ActionEraseContact: ActionErasePartyContact,
	org.ActionSetDefaultContact: ActionSetDefaultPartyContact, org.ActionRecordContactConsent: ActionRecordContactConsent,
}

// AddressBookCommand is one of the nine address book commands; Action says which, and only its fields are
// set. ExpectedRevision is the address book revision the caller read, 0 for a party without one.
type AddressBookCommand struct {
	CommandID        string
	Scope            Scope
	PartyID          org.PartyID
	ExpectedRevision int64
	Action           org.BookAction
	AddressID        org.AddressID        `json:",omitempty"`
	Address          *org.Address         `json:",omitempty"`
	AddressPurposes  []org.AddressPurpose `json:",omitempty"` // defaults of an added address, or the purpose of a default
	ContactID        org.ContactID        `json:",omitempty"`
	Contact          *org.Contact         `json:",omitempty"`
	ContactPurposes  []org.ContactPurpose `json:",omitempty"` // defaults of an added contact, or the purpose of a default
	Consent          *org.ContactConsent  `json:",omitempty"`
	Reason           string               `json:",omitempty"`
}

// AddressBookStore loads and writes address books for commands. Load locks the party row for share, so the
// party cannot be archived meanwhile, and the address book for the transaction; it returns an empty book at
// revision 0 when the party has none. Save writes what the book recorded since it was loaded, provided the
// stored revision is still the one loaded.
type AddressBookStore interface {
	Load(ctx context.Context, tenant org.TenantID, party org.PartyID) (*org.AddressBook, org.PartyStatus, error)
	Save(ctx context.Context, book *org.AddressBook) error
}

// PartyAuthorization asks the identity and policy layer whether the caller may perform a party action.
type PartyAuthorization interface {
	RequirePartyAction(ctx context.Context, s Scope, action string, party org.PartyID) error
}

// Clock tells the time ORG records a command.
type Clock interface{ Now() time.Time }

// MaintainAddressBook runs the address book commands (C9). Each command is one revision of the party's
// address book, committed with its Outbox event and command receipt in one ORG transaction. Changes take
// effect when ORG records them; they cannot be backdated.
type MaintainAddressBook struct {
	Tx       TxRunner
	Books    AddressBookStore
	Auth     PartyAuthorization
	Receipts Receipts
	Outbox   IntegrationOutbox
	Clock    Clock
}

func (h MaintainAddressBook) Handle(ctx context.Context, c AddressBookCommand) (Receipt, error) {
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	if h.Tx == nil || h.Books == nil || h.Auth == nil || h.Receipts == nil || h.Outbox == nil || h.Clock == nil {
		return Receipt{}, ErrMissingDependency
	}
	if c.Scope.TenantID == "" || strings.TrimSpace(c.Scope.ActorID) == "" || c.CommandID == "" {
		return Receipt{}, ErrUnauthenticated
	}
	if scoped, ok := h.Tx.(interface{ TenantScope() string }); ok && scoped.TenantScope() != string(c.Scope.TenantID) {
		return Receipt{}, ErrUnauthorized
	}
	action, known := commandActions[c.Action]
	if !known || c.PartyID == "" || c.ExpectedRevision < 0 {
		return Receipt{}, fmt.Errorf("%w: action %q, party %q, expected revision %d", ErrInvalidCommand, c.Action, c.PartyID, c.ExpectedRevision)
	}
	if err := h.Auth.RequirePartyAction(ctx, c.Scope, action, c.PartyID); err != nil {
		return Receipt{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	hash, err := requestHash(c)
	if err != nil {
		return Receipt{}, err
	}
	var result Receipt
	var mutated *org.AddressBook
	err = h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		previous, found, err := h.Receipts.Find(txCtx, c.Scope.TenantID, c.CommandID)
		if err != nil {
			return err
		}
		if found {
			if previous.RequestHash != hash {
				return ErrIdempotencyConflict
			}
			result = previous
			return nil
		}
		book, status, err := h.Books.Load(txCtx, c.Scope.TenantID, c.PartyID)
		if err != nil {
			return err
		}
		if err = org.RequireMaintainable(status); err != nil {
			return err
		}
		if book.Revision() != c.ExpectedRevision {
			return fmt.Errorf("%w: address book of %s is at revision %d, not %d", org.ErrConflict, c.PartyID, book.Revision(), c.ExpectedRevision)
		}
		entry, version, err := apply(book, c, h.Clock.Now().UTC().Truncate(time.Microsecond))
		if err != nil {
			return err
		}
		if err = h.Books.Save(txCtx, book); err != nil {
			return err
		}
		for _, event := range book.Pending() {
			if err = h.Outbox.EnqueueORGEvent(txCtx, event); err != nil {
				return err
			}
		}
		result = Receipt{CommandID: c.CommandID, ResultType: ResultAddressBook, PartyID: c.PartyID, NewRevision: book.Revision(),
			EntryID: entry, EntryVersion: version, RequestHash: hash}
		if err = h.Receipts.Save(txCtx, c.Scope.TenantID, result); err != nil {
			return err
		}
		mutated = book
		return nil
	})
	if err != nil {
		return Receipt{}, err
	}
	if mutated != nil {
		mutated.Acknowledge()
	}
	return result, nil
}

// apply runs one command on the book and returns the address or contact it concerns and that entry's version.
func apply(book *org.AddressBook, c AddressBookCommand, now time.Time) (string, int64, error) {
	actor := c.Scope.ActorID
	needAddress := func() (org.Address, error) {
		if c.Address == nil {
			return org.Address{}, fmt.Errorf("%w: no address", org.ErrInvalidAddress)
		}
		return *c.Address, nil
	}
	needContact := func() (org.Contact, error) {
		if c.Contact == nil {
			return org.Contact{}, fmt.Errorf("%w: no contact", org.ErrInvalidContact)
		}
		return *c.Contact, nil
	}
	onePurpose := func(n int) error {
		if n != 1 {
			return fmt.Errorf("%w: a default is set for exactly one purpose", org.ErrInvalidChange)
		}
		return nil
	}
	var a org.AddressVersion
	var k org.ContactVersion
	var err error
	switch c.Action {
	case org.ActionAddAddress:
		var addr org.Address
		if addr, err = needAddress(); err == nil {
			a, err = book.AddAddress(c.AddressID, addr, c.AddressPurposes, actor, now)
		}
	case org.ActionChangeAddress:
		var addr org.Address
		if addr, err = needAddress(); err == nil {
			a, err = book.ChangeAddress(c.AddressID, addr, c.Reason, actor, now)
		}
	case org.ActionDeactivateAddress:
		a, err = book.DeactivateAddress(c.AddressID, c.Reason, actor, now)
	case org.ActionSetDefaultAddress:
		if err = onePurpose(len(c.AddressPurposes)); err == nil {
			a, err = book.SetDefaultAddress(c.AddressPurposes[0], c.AddressID, actor, now)
		}
	case org.ActionAddContact:
		var contact org.Contact
		if contact, err = needContact(); err == nil {
			k, err = book.AddContact(c.ContactID, contact, c.ContactPurposes, actor, now)
		}
	case org.ActionChangeContact:
		var contact org.Contact
		if contact, err = needContact(); err == nil {
			k, err = book.ChangeContact(c.ContactID, contact, c.Reason, actor, now)
		}
	case org.ActionDeactivateContact:
		k, err = book.DeactivateContact(c.ContactID, c.Reason, actor, now)
	case org.ActionEraseContact:
		k, err = book.EraseContact(c.ContactID, c.Reason, actor, now)
	case org.ActionSetDefaultContact:
		if err = onePurpose(len(c.ContactPurposes)); err == nil {
			k, err = book.SetDefaultContact(c.ContactPurposes[0], c.ContactID, actor, now)
		}
	case org.ActionRecordContactConsent:
		if c.Consent == nil {
			err = fmt.Errorf("%w: no consent", org.ErrInvalidConsent)
		} else {
			k, err = book.RecordContactConsent(c.ContactID, *c.Consent, actor, now)
		}
	default:
		err = fmt.Errorf("%w: action %q", ErrInvalidCommand, c.Action)
	}
	if err != nil {
		return "", 0, err
	}
	if a.ID != "" {
		return string(a.ID), a.Version, nil
	}
	return string(k.ID), k.Version, nil
}

// AddressBookReader reads address books for queries. Read returns the party's book, with contacts only when
// asked, or an empty book at revision 0 when the party has none; it fails with Bento's ErrNotFound for an
// unknown party. AddressVersion returns one stored version of an address.
type AddressBookReader interface {
	Read(ctx context.Context, tenant org.TenantID, party org.PartyID, withContacts bool) (*org.AddressBook, error)
	AddressVersion(ctx context.Context, tenant org.TenantID, party org.PartyID, id org.AddressID, version int64) (org.AddressVersion, error)
}

// ReadAddressBook answers the address book queries. Addresses are business data of organizations and need
// no authorization of their own; contacts are personal data and do (privacy assessment V1.1, control 1).
// Every read of contacts states its purpose and is logged in the reading transaction, so a read that cannot
// be logged returns nothing (privacy assessment V1.2, section 8).
type ReadAddressBook struct {
	Tx    TxRunner
	Books AddressBookReader
	Auth  PartyAuthorization
	Log   ContactAccessLog
}

// Addresses returns the party's address book without contacts.
func (h ReadAddressBook) Addresses(ctx context.Context, tenant org.TenantID, party org.PartyID) (*org.AddressBook, error) {
	if err := h.check(ctx, tenant, party); err != nil {
		return nil, err
	}
	return h.read(ctx, tenant, party, false)
}

// Address returns one version of an address (0 for the current one) and the book it was read with.
func (h ReadAddressBook) Address(ctx context.Context, tenant org.TenantID, party org.PartyID, id org.AddressID, version int64) (org.AddressVersion, *org.AddressBook, error) {
	if err := h.check(ctx, tenant, party); err != nil {
		return org.AddressVersion{}, nil, err
	}
	if id == "" || version < 0 {
		return org.AddressVersion{}, nil, fmt.Errorf("%w: address %q version %d", ErrInvalidQuery, id, version)
	}
	var found org.AddressVersion
	var book *org.AddressBook
	err := h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var err error
		if book, err = h.Books.Read(txCtx, tenant, party, false); err != nil {
			return err
		}
		current, ok := book.Address(id)
		if !ok {
			return fmt.Errorf("%w: %s", org.ErrUnknownAddress, id)
		}
		if version == 0 || version == current.Version {
			found = current
			return nil
		}
		if version > current.Version {
			return fmt.Errorf("%w: %s has no version %d", org.ErrUnknownAddress, id, version)
		}
		found, err = h.Books.AddressVersion(txCtx, tenant, party, id, version)
		return err
	})
	if err != nil {
		return org.AddressVersion{}, nil, err
	}
	return found, book, nil
}

// Contacts returns the party's address book with contacts, if the caller may read them, and logs the read
// with its purpose.
func (h ReadAddressBook) Contacts(ctx context.Context, s Scope, party org.PartyID, purpose kernel.AccessPurpose) (*org.AddressBook, error) {
	if err := h.check(ctx, s.TenantID, party); err != nil {
		return nil, err
	}
	if strings.TrimSpace(s.ActorID) == "" {
		return nil, ErrUnauthenticated
	}
	if h.Auth == nil || h.Log == nil {
		return nil, ErrMissingDependency
	}
	if !purpose.Valid() {
		return nil, fmt.Errorf("%w: purpose %q", ErrInvalidQuery, purpose)
	}
	if err := h.Auth.RequirePartyAction(ctx, s, ActionReadPartyContacts, party); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	var book *org.AddressBook
	err := h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var err error
		if book, err = h.Books.Read(txCtx, s.TenantID, party, true); err != nil {
			return err
		}
		read := ContactAccess{ActorID: s.ActorID, Action: ActionReadPartyContacts, PartyID: party, Purpose: purpose}
		for _, c := range book.Contacts() {
			if !c.Erased {
				read.ContactIDs = append(read.ContactIDs, c.ID)
			}
		}
		return h.Log.RecordContactsRead(txCtx, s.TenantID, read)
	})
	if err != nil {
		return nil, err
	}
	return book, nil
}

func (h ReadAddressBook) check(ctx context.Context, tenant org.TenantID, party org.PartyID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if h.Tx == nil || h.Books == nil {
		return ErrMissingDependency
	}
	if tenant == "" {
		return ErrUnauthenticated
	}
	if party == "" {
		return fmt.Errorf("%w: no party", ErrInvalidQuery)
	}
	if scoped, ok := h.Tx.(interface{ TenantScope() string }); ok && scoped.TenantScope() != string(tenant) {
		return ErrUnauthorized
	}
	return nil
}

func (h ReadAddressBook) read(ctx context.Context, tenant org.TenantID, party org.PartyID, withContacts bool) (*org.AddressBook, error) {
	var book *org.AddressBook
	err := h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var err error
		book, err = h.Books.Read(txCtx, tenant, party, withContacts)
		return err
	})
	return book, err
}

// requestHash is the SHA-256 of a command's canonical JSON. Commands carry personal data, so only the hash
// is stored, never the body.
func requestHash(command any) (string, error) {
	blob, err := json.Marshal(command)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(blob)
	return hex.EncodeToString(digest[:]), nil
}
