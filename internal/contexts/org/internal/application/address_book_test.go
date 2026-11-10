package application

import (
	"context"
	"errors"
	"testing"
	"time"

	org "go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
)

type clockFake struct{}

func (clockFake) Now() time.Time { return now }

type bookFake struct {
	book   *org.AddressBook
	status org.PartyStatus
	saves  int
}

func (f *bookFake) Load(context.Context, org.TenantID, org.PartyID) (*org.AddressBook, org.PartyStatus, error) {
	if f.book == nil {
		b, err := org.NewAddressBook("T", "P")
		return b, f.status, err
	}
	return f.book, f.status, nil
}

func (f *bookFake) Save(_ context.Context, b *org.AddressBook) error {
	f.saves++
	f.book = b
	return nil
}

type partyAuthFake struct {
	fail    error
	actions []string
}

func (f *partyAuthFake) RequirePartyAction(_ context.Context, _ Scope, action string, _ org.PartyID) error {
	f.actions = append(f.actions, action)
	return f.fail
}

func addressCommand() AddressBookCommand {
	return AddressBookCommand{CommandID: "A-1", Scope: Scope{TenantID: "T", ActorID: "clerk"}, PartyID: "P", Action: org.ActionAddAddress,
		AddressID: "A1", Address: &org.Address{ShortCode: "HQ", Purposes: []org.AddressPurpose{org.AddressOffice}, Name: "Fixture Co",
			Lines: []string{"1 Road"}, City: "Shanghai", CountryCode: "CN"}, AddressPurposes: []org.AddressPurpose{org.AddressOffice}}
}

func bookHandler() (MaintainAddressBook, *bookFake, *receiptFake, *outboxFake, *partyAuthFake) {
	books, receipts, out, auth := &bookFake{status: org.PartyActive}, &receiptFake{}, &outboxFake{}, &partyAuthFake{}
	return MaintainAddressBook{Tx: &txFake{}, Books: books, Auth: auth, Receipts: receipts, Outbox: out, Clock: clockFake{}}, books, receipts, out, auth
}

func TestMaintainAddressBookCommitsOneRevisionWithEventAndReceipt(t *testing.T) {
	h, books, receipts, out, auth := bookHandler()
	r, err := h.Handle(context.Background(), addressCommand())
	if err != nil || r.ResultType != ResultAddressBook || r.NewRevision != 1 || r.EntryID != "A1" || r.EntryVersion != 1 || r.RequestHash == "" {
		t.Fatalf("receipt %+v err %v", r, err)
	}
	if books.saves != 1 || out.calls != 1 || receipts.saves != 1 || len(auth.actions) != 1 || auth.actions[0] != ActionAddPartyAddress {
		t.Fatalf("saves %d, events %d, receipts %d, authorized %v", books.saves, out.calls, receipts.saves, auth.actions)
	}
	if len(books.book.Pending()) != 0 {
		t.Fatal("events are acknowledged only after the commit")
	}
	replay, err := h.Handle(context.Background(), addressCommand())
	if err != nil || replay != r || books.saves != 1 || out.calls != 1 {
		t.Fatalf("replay %+v err %v; saves %d events %d", replay, err, books.saves, out.calls)
	}
	changed := addressCommand()
	changed.Address.City = "Ningbo"
	if _, err = h.Handle(context.Background(), changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed payload under the same id: %v", err)
	}
}

func TestMaintainAddressBookRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*AddressBookCommand, *bookFake, *partyAuthFake, *outboxFake)
		want   error
	}{
		"no actor":          {func(c *AddressBookCommand, _ *bookFake, _ *partyAuthFake, _ *outboxFake) { c.Scope.ActorID = " " }, ErrUnauthenticated},
		"unknown action":    {func(c *AddressBookCommand, _ *bookFake, _ *partyAuthFake, _ *outboxFake) { c.Action = "MERGE" }, ErrInvalidCommand},
		"negative revision": {func(c *AddressBookCommand, _ *bookFake, _ *partyAuthFake, _ *outboxFake) { c.ExpectedRevision = -1 }, ErrInvalidCommand},
		"authorizer refuses": {func(_ *AddressBookCommand, _ *bookFake, a *partyAuthFake, _ *outboxFake) {
			a.fail = errors.New("policy")
		}, ErrUnauthorized},
		"stale revision": {func(c *AddressBookCommand, _ *bookFake, _ *partyAuthFake, _ *outboxFake) { c.ExpectedRevision = 3 }, org.ErrConflict},
		"archived party": {func(_ *AddressBookCommand, b *bookFake, _ *partyAuthFake, _ *outboxFake) {
			b.status = org.PartyArchived
		}, org.ErrPartyArchived},
		"no address": {func(c *AddressBookCommand, _ *bookFake, _ *partyAuthFake, _ *outboxFake) { c.Address = nil }, org.ErrInvalidAddress},
		"two purposes": {func(c *AddressBookCommand, _ *bookFake, _ *partyAuthFake, _ *outboxFake) {
			c.Action = org.ActionSetDefaultAddress
			c.AddressPurposes = nil
		}, org.ErrInvalidChange},
		"outbox fails": {func(_ *AddressBookCommand, _ *bookFake, _ *partyAuthFake, o *outboxFake) {
			o.fail = errors.New("outbox down")
		}, nil},
	} {
		h, books, receipts, out, auth := bookHandler()
		c := addressCommand()
		tc.mutate(&c, books, auth, out)
		_, err := h.Handle(context.Background(), c)
		if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
		if receipts.saves != 0 {
			t.Errorf("%s: a refused command left a receipt", name)
		}
	}
}

func TestReadAddressBookAuthorizesContactsOnly(t *testing.T) {
	auth := &partyAuthFake{}
	h := ReadAddressBook{Tx: &txFake{}, Books: bookReaderFake{}, Auth: auth}
	if _, err := h.Addresses(context.Background(), "T", "P"); err != nil || len(auth.actions) != 0 {
		t.Fatalf("addresses: %v, authorized %v", err, auth.actions)
	}
	if _, err := h.Contacts(context.Background(), Scope{TenantID: "T"}, "P"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("contacts without an actor: %v", err)
	}
	auth.fail = errors.New("policy")
	if _, err := h.Contacts(context.Background(), Scope{TenantID: "T", ActorID: "clerk"}, "P"); !errors.Is(err, ErrUnauthorized) ||
		len(auth.actions) != 1 || auth.actions[0] != ActionReadPartyContacts {
		t.Fatalf("contacts refused: %v, authorized %v", err, auth.actions)
	}
	if _, _, err := h.Address(context.Background(), "T", "P", "A1", -1); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("negative version: %v", err)
	}
}

type bookReaderFake struct{}

func (bookReaderFake) Read(context.Context, org.TenantID, org.PartyID, bool) (*org.AddressBook, error) {
	return org.NewAddressBook("T", "P")
}

func (bookReaderFake) AddressVersion(context.Context, org.TenantID, org.PartyID, org.AddressID, int64) (org.AddressVersion, error) {
	return org.AddressVersion{}, org.ErrUnknownAddress
}
