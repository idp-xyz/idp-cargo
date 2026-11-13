package com

import (
	"context"
	"slices"

	"go.idp.xyz/idp-cargo/internal/contexts/com/api"
	"go.idp.xyz/idp-cargo/internal/contexts/com/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/com/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
)

// The order party commands and query (C9, C11-C14).

func (m *Module) SetOrderParty(ctx context.Context, cmd api.SetOrderParty) (api.Receipt, error) {
	receipt, err := application.SetOrderParty{Deps: m.deps(cmd.TenantID, cmd.CommandID), Directory: m.dir}.Handle(ctx, application.SetOrderPartyCommand{
		CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), OrderID: domain.OrderID(cmd.OrderID),
		ExpectedRevision: cmd.ExpectedRevision, Party: partyInput(cmd.Party), Reason: cmd.Reason, OccurredAt: stored(cmd.OccurredAt),
	})
	return toReceipt(receipt, err)
}

func (m *Module) ChangeOrderTransport(ctx context.Context, cmd api.ChangeOrderTransport) (api.Receipt, error) {
	receipt, err := application.ChangeOrderTransport{Deps: m.deps(cmd.TenantID, cmd.CommandID)}.Handle(ctx, application.ChangeOrderTransportCommand{
		CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), OrderID: domain.OrderID(cmd.OrderID),
		ExpectedRevision: cmd.ExpectedRevision, Transport: transportOf(cmd.Transport), Reason: cmd.Reason, OccurredAt: stored(cmd.OccurredAt),
	})
	return toReceipt(receipt, err)
}

func (m *Module) LockOrderParties(ctx context.Context, cmd api.LockOrderParties) (api.Receipt, error) {
	receipt, err := application.LockOrderParties{Deps: m.deps(cmd.TenantID, cmd.CommandID)}.Handle(ctx, application.PartyLockCommand{
		CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), OrderID: domain.OrderID(cmd.OrderID),
		ExpectedRevision: cmd.ExpectedRevision, HolderID: cmd.HolderID, OccurredAt: stored(cmd.OccurredAt),
	})
	return toReceipt(receipt, err)
}

func (m *Module) ReleaseOrderPartyLock(ctx context.Context, cmd api.ReleaseOrderPartyLock) (api.Receipt, error) {
	receipt, err := application.ReleaseOrderPartyLock{Deps: m.deps(cmd.TenantID, cmd.CommandID)}.Handle(ctx, application.PartyLockCommand{
		CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), OrderID: domain.OrderID(cmd.OrderID),
		ExpectedRevision: cmd.ExpectedRevision, HolderID: cmd.HolderID, OccurredAt: stored(cmd.OccurredAt),
	})
	return toReceipt(receipt, err)
}

func (m *Module) GetOrderParties(ctx context.Context, q api.GetOrderParties) (api.OrderParties, error) {
	order, revision, contacts, err := application.GetOrderParties{
		Tx: bento.TenantTransactor{DB: m.db, Tenant: q.TenantID}, Orders: postgres.PartyReader{DB: m.db}, Auth: authorizer{m.auth},
		Log: postgres.ContactAccessLog{DB: m.db},
	}.Handle(ctx, scope(q.TenantID, q.ActorID), domain.OrderID(q.OrderID), q.WithContacts, q.Purpose)
	if err != nil {
		return api.OrderParties{}, classify(err)
	}
	t := order.Transport()
	out := api.OrderParties{OrderID: string(order.ID()), Revision: revision, CustomerPartyID: order.State().Customer.PartyID,
		Transport: api.Transport{Mode: string(t.Mode), Movement: string(t.Movement), ContainerMode: string(t.ContainerMode)},
		Contacts:  api.ContactsStatus{State: api.ContactsInUse}}
	if r, ok := order.Retention(); ok {
		out.Contacts = api.ContactsStatus{State: api.ContactsRestricted, RetainedUntil: r.RetainedUntil}
	}
	if e, ok := order.Erasure(); ok {
		out.Contacts.State, out.Contacts.ErasedAt = api.ContactsErased, e.ErasedAt
	}
	for _, l := range order.PartyLocks() {
		out.Locks = append(out.Locks, api.PartyLock{HolderID: l.HolderID, LockedBy: l.LockedBy, LockedAt: l.LockedAt})
	}
	for _, p := range order.Parties() {
		v := api.OrderPartyView{Role: p.Role, Version: p.Version, Kind: string(p.Kind), ToOrder: p.ToOrderText, HasContact: p.HasContact,
			Overridden: p.Overridden, Reason: p.Reason, SetBy: p.SetBy, SetAt: p.SetAt,
			Address: frozen(p.Address), Pickup: frozen(p.Pickup), Delivery: frozen(p.Delivery)}
		if p.Kind == domain.KindParty {
			r := p.Party
			v.PartyID, v.PartyRevision, v.LegalName, v.EnglishName, v.CountryCode, v.RoleCode = r.PartyID, r.PartyRevision, r.LegalName, r.EnglishName, r.CountryCode, r.RoleCode
			for _, i := range r.Identifiers {
				v.Identifiers = append(v.Identifiers, api.PartyIdentifier{Type: i.Type, Country: i.Country, Value: i.Value, None: i.None})
			}
		}
		if c, ok := contacts[p.Role][p.Version]; ok {
			v.Contact = &api.FrozenContact{Source: string(c.Source), ORGContactID: c.ORGContactID, ORGVersion: c.ORGVersion,
				Contact: api.Contact{Name: c.Name, Phone: c.Phone, Mobile: c.Mobile, Email: c.Email}, OverriddenBy: c.OverriddenBy, OverrideReason: c.OverrideReason}
			if k := c.Consent; k != nil {
				v.Contact.Consent = &api.ContactConsent{NoticeVersion: k.NoticeVersion, ProvideToOthers: k.ProvideToOthers, CrossBorder: k.CrossBorder,
					Basis: k.Basis, EvidenceRef: k.EvidenceRef, ORGRevision: k.ORGRevision}
			}
		}
		out.Parties = append(out.Parties, v)
	}
	return out, nil
}

func transportOf(t api.Transport) domain.Transport {
	return domain.Transport{Mode: domain.TransportMode(t.Mode), Movement: domain.MovementType(t.Movement),
		ContainerMode: domain.ContainerMode(t.ContainerMode)}
}

func partyInputs(in []api.OrderParty) []application.PartyInput {
	var out []application.PartyInput
	for _, p := range in {
		out = append(out, partyInput(p))
	}
	return out
}

func partyInput(p api.OrderParty) application.PartyInput {
	in := application.PartyInput{Role: p.Role, PartyID: p.PartyID, ToOrder: p.ToOrder, Address: addressChoice(p.Address),
		Contact: application.ContactChoice{Use: p.Contact.Use, ContactID: p.Contact.ContactID, Reason: p.Contact.Reason}}
	if p.Pickup != nil {
		c := addressChoice(*p.Pickup)
		in.Pickup = &c
	}
	if p.Delivery != nil {
		c := addressChoice(*p.Delivery)
		in.Delivery = &c
	}
	if k := p.Contact.Override; k != nil {
		in.Contact.Override = &domain.ContactSnapshot{Name: k.Name, Phone: k.Phone, Mobile: k.Mobile, Email: k.Email}
		if c := p.Contact.Consent; c != nil {
			in.Contact.Override.Consent = &domain.ContactConsent{NoticeVersion: c.NoticeVersion, ProvideToOthers: c.ProvideToOthers,
				CrossBorder: c.CrossBorder, Basis: c.Basis, EvidenceRef: c.EvidenceRef, ORGRevision: c.ORGRevision}
		}
	}
	return in
}

func addressChoice(c api.AddressChoice) application.AddressChoice {
	out := application.AddressChoice{AddressID: c.AddressID, Reason: c.Reason}
	if a := c.Override; a != nil {
		out.Override = &domain.AddressSnapshot{Name: a.Name, Lines: slices.Clone(a.Lines), City: a.City, Region: a.Region, Postcode: a.Postcode,
			CountryCode: a.CountryCode, UNLocode: a.UNLocode, Residential: a.Residential, Phone: a.Phone, Email: a.Email}
		if l := a.Local; l != nil {
			out.Override.Local = &domain.LocalAddress{Language: l.Language, Name: l.Name, Lines: slices.Clone(l.Lines), City: l.City, Region: l.Region}
		}
	}
	return out
}

func frozen(a *domain.AddressSnapshot) *api.FrozenAddress {
	if a == nil {
		return nil
	}
	out := &api.FrozenAddress{Source: string(a.Source), ORGAddressID: a.ORGAddressID, ShortCode: a.ShortCode, ORGVersion: a.ORGVersion,
		OverriddenBy: a.OverriddenBy, OverrideReason: a.OverrideReason,
		Address: api.Address{Name: a.Name, Lines: slices.Clone(a.Lines), City: a.City, Region: a.Region, Postcode: a.Postcode, CountryCode: a.CountryCode,
			UNLocode: a.UNLocode, Residential: a.Residential, Phone: a.Phone, Email: a.Email}}
	if l := a.Local; l != nil {
		out.Address.Local = &api.LocalAddress{Language: l.Language, Name: l.Name, Lines: slices.Clone(l.Lines), City: l.City, Region: l.Region}
	}
	return out
}
