// Package com is DOC's anticorruption layer for COM's OrderParties v1 (COM -> DOC): an order's transport,
// current parties without contacts, and the locks transport documents hold on them (C12). COM's error kinds are
// kept as text only.
package com

import (
	"context"
	"errors"
	"fmt"

	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	app "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/application"
	doc "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/domain"
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
