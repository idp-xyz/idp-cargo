package org

import (
	"context"

	"go.idp.xyz/idp-cargo/internal/contexts/org/api"
	"go.idp.xyz/idp-cargo/internal/contexts/org/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/org/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
)

// The address book commands and queries (C9). Each command maps onto application.AddressBookCommand; the
// request hash of the receipt is taken over that mapping.

func (m *Module) AddPartyAddress(ctx context.Context, cmd api.AddPartyAddress) (api.AddressBookReceipt, error) {
	a := addressOf(cmd.Address)
	return m.maintain(ctx, application.AddressBookCommand{
		CommandID: cmd.CommandID, Scope: bookScope(cmd.TenantID, cmd.ActorID), PartyID: domain.PartyID(cmd.PartyID),
		ExpectedRevision: cmd.ExpectedRevision, Action: domain.ActionAddAddress,
		AddressID: domain.AddressID(cmd.AddressID), Address: &a, AddressPurposes: purposes[domain.AddressPurpose](cmd.DefaultFor),
	})
}

func (m *Module) ChangePartyAddress(ctx context.Context, cmd api.ChangePartyAddress) (api.AddressBookReceipt, error) {
	a := addressOf(cmd.Address)
	return m.maintain(ctx, application.AddressBookCommand{
		CommandID: cmd.CommandID, Scope: bookScope(cmd.TenantID, cmd.ActorID), PartyID: domain.PartyID(cmd.PartyID),
		ExpectedRevision: cmd.ExpectedRevision, Action: domain.ActionChangeAddress,
		AddressID: domain.AddressID(cmd.AddressID), Address: &a, Reason: cmd.Reason,
	})
}

func (m *Module) DeactivatePartyAddress(ctx context.Context, cmd api.DeactivatePartyAddress) (api.AddressBookReceipt, error) {
	return m.maintain(ctx, application.AddressBookCommand{
		CommandID: cmd.CommandID, Scope: bookScope(cmd.TenantID, cmd.ActorID), PartyID: domain.PartyID(cmd.PartyID),
		ExpectedRevision: cmd.ExpectedRevision, Action: domain.ActionDeactivateAddress,
		AddressID: domain.AddressID(cmd.AddressID), Reason: cmd.Reason,
	})
}

func (m *Module) SetDefaultPartyAddress(ctx context.Context, cmd api.SetDefaultPartyAddress) (api.AddressBookReceipt, error) {
	return m.maintain(ctx, application.AddressBookCommand{
		CommandID: cmd.CommandID, Scope: bookScope(cmd.TenantID, cmd.ActorID), PartyID: domain.PartyID(cmd.PartyID),
		ExpectedRevision: cmd.ExpectedRevision, Action: domain.ActionSetDefaultAddress,
		AddressID: domain.AddressID(cmd.AddressID), AddressPurposes: []domain.AddressPurpose{domain.AddressPurpose(cmd.Purpose)},
	})
}

func (m *Module) AddPartyContact(ctx context.Context, cmd api.AddPartyContact) (api.AddressBookReceipt, error) {
	c := contactOf(cmd.Contact)
	return m.maintain(ctx, application.AddressBookCommand{
		CommandID: cmd.CommandID, Scope: bookScope(cmd.TenantID, cmd.ActorID), PartyID: domain.PartyID(cmd.PartyID),
		ExpectedRevision: cmd.ExpectedRevision, Action: domain.ActionAddContact,
		ContactID: domain.ContactID(cmd.ContactID), Contact: &c, ContactPurposes: purposes[domain.ContactPurpose](cmd.DefaultFor),
	})
}

func (m *Module) ChangePartyContact(ctx context.Context, cmd api.ChangePartyContact) (api.AddressBookReceipt, error) {
	c := contactOf(cmd.Contact)
	return m.maintain(ctx, application.AddressBookCommand{
		CommandID: cmd.CommandID, Scope: bookScope(cmd.TenantID, cmd.ActorID), PartyID: domain.PartyID(cmd.PartyID),
		ExpectedRevision: cmd.ExpectedRevision, Action: domain.ActionChangeContact,
		ContactID: domain.ContactID(cmd.ContactID), Contact: &c, Reason: cmd.Reason,
	})
}

func (m *Module) DeactivatePartyContact(ctx context.Context, cmd api.DeactivatePartyContact) (api.AddressBookReceipt, error) {
	return m.maintain(ctx, application.AddressBookCommand{
		CommandID: cmd.CommandID, Scope: bookScope(cmd.TenantID, cmd.ActorID), PartyID: domain.PartyID(cmd.PartyID),
		ExpectedRevision: cmd.ExpectedRevision, Action: domain.ActionDeactivateContact,
		ContactID: domain.ContactID(cmd.ContactID), Reason: cmd.Reason,
	})
}

func (m *Module) ErasePartyContact(ctx context.Context, cmd api.ErasePartyContact) (api.AddressBookReceipt, error) {
	return m.maintain(ctx, application.AddressBookCommand{
		CommandID: cmd.CommandID, Scope: bookScope(cmd.TenantID, cmd.ActorID), PartyID: domain.PartyID(cmd.PartyID),
		ExpectedRevision: cmd.ExpectedRevision, Action: domain.ActionEraseContact,
		ContactID: domain.ContactID(cmd.ContactID), Reason: cmd.Reason,
	})
}

func (m *Module) SetDefaultPartyContact(ctx context.Context, cmd api.SetDefaultPartyContact) (api.AddressBookReceipt, error) {
	return m.maintain(ctx, application.AddressBookCommand{
		CommandID: cmd.CommandID, Scope: bookScope(cmd.TenantID, cmd.ActorID), PartyID: domain.PartyID(cmd.PartyID),
		ExpectedRevision: cmd.ExpectedRevision, Action: domain.ActionSetDefaultContact,
		ContactID: domain.ContactID(cmd.ContactID), ContactPurposes: []domain.ContactPurpose{domain.ContactPurpose(cmd.Purpose)},
	})
}

func (m *Module) RecordContactConsent(ctx context.Context, cmd api.RecordContactConsent) (api.AddressBookReceipt, error) {
	c := cmd.Consent
	return m.maintain(ctx, application.AddressBookCommand{
		CommandID: cmd.CommandID, Scope: bookScope(cmd.TenantID, cmd.ActorID), PartyID: domain.PartyID(cmd.PartyID),
		ExpectedRevision: cmd.ExpectedRevision, Action: domain.ActionRecordContactConsent, ContactID: domain.ContactID(cmd.ContactID),
		Consent: &domain.ContactConsent{NoticeVersion: c.NoticeVersion, ProvideToOthers: c.ProvideToOthers, CrossBorder: c.CrossBorder,
			Basis: domain.ConsentBasis(c.Basis), EvidenceRef: c.EvidenceRef},
	})
}

func (m *Module) ListContactAccess(ctx context.Context, q api.ListContactAccess) ([]api.ContactAccess, error) {
	reads, err := application.ListContactAccess{Tx: bento.TenantTransactor{DB: m.db, Tenant: q.TenantID}, Log: postgres.ContactAccessLog{DB: m.db},
		Auth: authorizer{m.auth}}.Handle(ctx, bookScope(q.TenantID, q.ActorID),
		application.AccessFilter{PartyID: domain.PartyID(q.PartyID), ActorID: q.ReadBy, Since: q.Since, Until: q.Until, Limit: q.Limit})
	if err != nil {
		return nil, classify(err)
	}
	out := make([]api.ContactAccess, 0, len(reads))
	for _, r := range reads {
		out = append(out, api.ContactAccess{AccessID: r.AccessID, ActorID: r.ActorID, Action: r.Action, PartyID: string(r.PartyID),
			Purpose: r.Purpose, ContactIDs: texts(r.ContactIDs), RecordedAt: r.RecordedAt})
	}
	return out, nil
}

func (m *Module) GetPartyAddresses(ctx context.Context, q api.GetPartyAddresses) (api.PartyAddresses, error) {
	book, err := m.reader(q.TenantID).Addresses(ctx, domain.TenantID(q.TenantID), domain.PartyID(q.PartyID))
	if err != nil {
		return api.PartyAddresses{}, classify(err)
	}
	out := api.PartyAddresses{PartyID: q.PartyID, Revision: book.Revision()}
	for _, a := range book.Addresses() {
		out.Addresses = append(out.Addresses, partyAddressOf(book, a))
	}
	return out, nil
}

func (m *Module) GetPartyAddress(ctx context.Context, q api.GetPartyAddress) (api.PartyAddress, error) {
	a, book, err := m.reader(q.TenantID).Address(ctx, domain.TenantID(q.TenantID), domain.PartyID(q.PartyID), domain.AddressID(q.AddressID), q.Version)
	if err != nil {
		return api.PartyAddress{}, classify(err)
	}
	return partyAddressOf(book, a), nil
}

func (m *Module) GetPartyContacts(ctx context.Context, q api.GetPartyContacts) (api.PartyContacts, error) {
	book, err := m.reader(q.TenantID).Contacts(ctx, bookScope(q.TenantID, q.ActorID), domain.PartyID(q.PartyID), q.Purpose)
	if err != nil {
		return api.PartyContacts{}, classify(err)
	}
	out := api.PartyContacts{PartyID: q.PartyID, Revision: book.Revision()}
	for _, c := range book.Contacts() {
		contact := api.PartyContact{
			ContactID: string(c.ID), Version: c.Version, Status: string(c.Status), DefaultFor: texts(book.ContactDefaultsOf(c.ID)),
			Contact: api.Contact{Purposes: texts(c.Purposes), Name: c.Name, Phone: c.Phone, Mobile: c.Mobile, Email: c.Email},
		}
		if r, ok := book.Consent(c.ID); ok {
			contact.Consent = &api.ContactConsent{NoticeVersion: r.NoticeVersion, ProvideToOthers: r.ProvideToOthers, CrossBorder: r.CrossBorder,
				Basis: string(r.Basis), EvidenceRef: r.EvidenceRef, Revision: r.Revision}
		}
		out.Contacts = append(out.Contacts, contact)
	}
	return out, nil
}

func (m *Module) maintain(ctx context.Context, c application.AddressBookCommand) (api.AddressBookReceipt, error) {
	r, err := application.MaintainAddressBook{
		Tx:       bento.TenantTransactor{DB: m.db, Tenant: string(c.Scope.TenantID)},
		Books:    postgres.AddressBookStore{DB: m.db},
		Auth:     authorizer{m.auth},
		Receipts: postgres.CommandReceipts{DB: m.db},
		Outbox:   postgres.Outbox{Writer: m.outbox, CommandID: c.CommandID, RecordedAt: m.clock.Now()},
		Clock:    m.clock,
	}.Handle(ctx, c)
	if err != nil {
		return api.AddressBookReceipt{}, classify(err)
	}
	return api.AddressBookReceipt{CommandID: r.CommandID, PartyID: string(r.PartyID), NewRevision: r.NewRevision,
		EntryID: r.EntryID, EntryVersion: r.EntryVersion}, nil
}

func (m *Module) reader(tenant string) application.ReadAddressBook {
	return application.ReadAddressBook{
		Tx:    bento.TenantTransactor{DB: m.db, Tenant: tenant},
		Books: postgres.AddressBookReader{DB: m.db},
		Auth:  authorizer{m.auth},
		Log:   postgres.ContactAccessLog{DB: m.db},
	}
}

func bookScope(tenant, actor string) application.Scope {
	return application.Scope{TenantID: domain.TenantID(tenant), ActorID: actor}
}

func addressOf(a api.Address) domain.Address {
	out := domain.Address{ShortCode: a.ShortCode, Purposes: purposes[domain.AddressPurpose](a.Purposes), Name: a.Name, Lines: a.Lines,
		City: a.City, Region: a.Region, Postcode: a.Postcode, CountryCode: a.CountryCode, UNLocode: a.UNLocode, Residential: a.Residential,
		Phone: a.Phone, Email: a.Email}
	if l := a.Local; l != nil {
		out.Local = &domain.LocalAddress{Language: l.Language, Name: l.Name, Lines: l.Lines, City: l.City, Region: l.Region}
	}
	return out
}

func contactOf(c api.Contact) domain.Contact {
	return domain.Contact{Purposes: purposes[domain.ContactPurpose](c.Purposes), Name: c.Name, Phone: c.Phone, Mobile: c.Mobile, Email: c.Email}
}

func partyAddressOf(book *domain.AddressBook, v domain.AddressVersion) api.PartyAddress {
	out := api.PartyAddress{
		PartyID: string(book.Party()), AddressID: string(v.ID), Version: v.Version, Status: string(v.Status),
		DefaultFor: texts(book.AddressDefaultsOf(v.ID)), BookRevision: book.Revision(),
		Address: api.Address{ShortCode: v.ShortCode, Purposes: texts(v.Purposes), Name: v.Name, Lines: v.Lines, City: v.City,
			Region: v.Region, Postcode: v.Postcode, CountryCode: v.CountryCode, UNLocode: v.UNLocode, Residential: v.Residential,
			Phone: v.Phone, Email: v.Email},
	}
	if l := v.Local; l != nil {
		out.Local = &api.LocalAddress{Language: l.Language, Name: l.Name, Lines: l.Lines, City: l.City, Region: l.Region}
	}
	return out
}

func purposes[P ~string](in []string) []P {
	if in == nil {
		return nil
	}
	out := make([]P, len(in))
	for i, s := range in {
		out[i] = P(s)
	}
	return out
}

func texts[S ~string](in []S) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = string(s)
	}
	return out
}
