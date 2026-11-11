// Package com is DOC's anticorruption layer for COM's OrderParties v1 (COM -> DOC): an order's transport,
// current parties without contacts, and the locks transport documents hold on them (C12); for a dispatch copy, the
// ordering party and the contacts with their consent (P5). COM's error kinds are kept as text only.
package com

import (
	"context"
	"errors"
	"fmt"

	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	app "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/application"
	doc "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Parties reads OrderParties v1 without contacts. Call it outside DOC's transactions.
type Parties struct{ COM comapi.OrderPartyQueries }

var _ app.Parties = Parties{}

func (p Parties) Order(ctx context.Context, tenant doc.TenantID, actor, order string) (doc.OrderParties, error) {
	got, err := p.COM.GetOrderParties(ctx, comapi.GetOrderParties{TenantID: string(tenant), ActorID: actor, OrderID: order})
	if errors.Is(err, comapi.ErrNotFound) {
		return doc.OrderParties{}, fmt.Errorf("%w: order %s: %v", app.ErrUnknownSource, order, err.Error())
	}
	if err != nil {
		return doc.OrderParties{}, err
	}
	if got.OrderID != order {
		return doc.OrderParties{}, fmt.Errorf("%w: COM answered for another order", app.ErrUnknownSource)
	}
	out := doc.OrderParties{OrderID: order, Revision: got.Revision, Transport: doc.Transport{Mode: got.Transport.Mode, Movement: got.Transport.Movement}}
	for _, v := range got.Parties {
		party := partyOf(v)
		switch v.Role {
		case comapi.PartyShipper:
			out.Shipper = party
		case comapi.PartyConsignee:
			out.Consignee = party
		case comapi.PartyNotify:
			out.Notify = party
		}
	}
	for _, l := range got.Locks {
		out.Locks = append(out.Locks, l.HolderID)
	}
	return out, nil
}

// Contacts reads the contacts frozen on an order for the purpose DOCUMENTS, with the caller's own authorization
// (COM logs the read). Once the order is closed its contacts are restricted, and once erased gone (P1): neither is
// used for a document again, so each party's contact counts as erased.
func (p Parties) Contacts(ctx context.Context, tenant doc.TenantID, actor, order string) (app.OrderContacts, error) {
	q := comapi.GetOrderParties{TenantID: string(tenant), ActorID: actor, OrderID: order}
	got, err := p.COM.GetOrderParties(ctx, q)
	if errors.Is(err, comapi.ErrNotFound) {
		return app.OrderContacts{}, fmt.Errorf("%w: order %s: %v", app.ErrUnknownSource, order, err.Error())
	}
	if err != nil {
		return app.OrderContacts{}, err
	}
	usable := got.Contacts.State == comapi.ContactsInUse
	if usable {
		q.WithContacts, q.Purpose = true, kernel.PurposeDocuments
		if got, err = p.COM.GetOrderParties(ctx, q); err != nil {
			return app.OrderContacts{}, err
		}
	}
	if got.OrderID != order {
		return app.OrderContacts{}, fmt.Errorf("%w: COM answered for another order", app.ErrUnknownSource)
	}
	out := app.OrderContacts{OrderingParty: got.CustomerPartyID}
	for _, v := range got.Parties {
		if !v.HasContact {
			continue
		}
		c := doc.ContactSource{Key: kernel.OrderContactKey(order, v.Role, v.Version), PartyRole: v.Role, PartyID: v.PartyID, OrderID: order,
			PartyVersion: v.Version, Source: doc.SourceOrder, Erased: true}
		if f := v.Contact; usable && f != nil {
			if f.Source == comapi.FromORG {
				c.Key, c.Source, c.ORGContactID = kernel.ORGContactKey(v.PartyID, f.ORGContactID), doc.SourceORG, f.ORGContactID
			}
			c.Erased, c.Name, c.Phone, c.Mobile, c.Email = false, f.Contact.Name, f.Contact.Phone, f.Contact.Mobile, f.Contact.Email
			if k := f.Consent; k != nil {
				c.Consent = &doc.Consent{NoticeVersion: k.NoticeVersion, ProvideToOthers: k.ProvideToOthers, CrossBorder: k.CrossBorder, Basis: k.Basis,
					ORGRevision: k.ORGRevision}
			}
		}
		out.Contacts = append(out.Contacts, c)
	}
	return out, nil
}

func partyOf(v comapi.OrderPartyView) *doc.Party {
	p := &doc.Party{Version: v.Version, Named: v.Kind == comapi.KindParty, PartyID: v.PartyID, PartyRevision: v.PartyRevision, LegalName: v.LegalName,
		EnglishName: v.EnglishName, CountryCode: v.CountryCode, Overridden: v.Address != nil && v.Address.Source == comapi.FromOrder}
	for _, i := range v.Identifiers {
		p.Identifiers = append(p.Identifiers, doc.Identifier{Type: i.Type, Country: i.Country, Value: i.Value, None: i.None})
	}
	if a := v.Address; a != nil {
		p.Address = &doc.Address{Source: a.Source, ORGAddressID: a.ORGAddressID, ORGVersion: a.ORGVersion, Name: a.Address.Name,
			Lines: append([]string(nil), a.Address.Lines...), City: a.Address.City, Region: a.Address.Region, Postcode: a.Address.Postcode,
			CountryCode: a.Address.CountryCode, UNLocode: a.Address.UNLocode}
	}
	return p
}
