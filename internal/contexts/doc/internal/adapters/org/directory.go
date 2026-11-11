// Package org is DOC's anticorruption layer for ORG (ORG -> DOC): the issuing legal entity (LegalEntityRef v1) and
// a destination agent in the OVERSEAS_AGENT role with its office address (PartyRef v1). ORG's error kinds are kept
// as text only.
package org

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	app "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/application"
	doc "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/domain"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
)

// ORG is what DOC asks ORG.
type ORG interface {
	orgapi.Queries
	orgapi.LegalEntityQueries
	orgapi.AddressBookQueries
}

// Directory reads ORG. Call it outside DOC's transactions.
type Directory struct{ ORG ORG }

var _ app.Directory = Directory{}

func unknown(what string, err error) error {
	if errors.Is(err, orgapi.ErrNotFound) || errors.Is(err, orgapi.ErrRejected) {
		return fmt.Errorf("%w: %s: %v", app.ErrUnknownSource, what, err.Error())
	}
	return err
}

func (d Directory) Issuer(ctx context.Context, tenant doc.TenantID, legalEntity string) (doc.Issuer, error) {
	le, err := d.ORG.GetLegalEntity(ctx, orgapi.GetLegalEntity{TenantID: string(tenant), LegalEntityID: legalEntity})
	if err != nil {
		return doc.Issuer{}, unknown("legal entity "+legalEntity, err)
	}
	return doc.Issuer{LegalEntityID: le.LegalEntityID, Revision: le.Revision, Name: le.RegisteredName, Jurisdiction: le.Jurisdiction,
		Verified: le.Status == orgapi.LegalEntityVerified}, nil
}

func (d Directory) Agent(ctx context.Context, tenant doc.TenantID, party, legalEntity string, at time.Time) (doc.Party, error) {
	role, err := d.ORG.ResolvePartyRole(ctx, orgapi.ResolvePartyRole{TenantID: string(tenant), PartyID: party, RoleCode: orgapi.RoleOverseasAgent,
		LegalEntityID: legalEntity, At: at})
	if err != nil {
		return doc.Party{}, unknown("destination agent "+party, err)
	}
	out := doc.Party{Named: true, PartyID: role.Ref.PartyID, PartyRevision: role.Ref.PartyRevision, LegalName: role.LegalName, EnglishName: role.EnglishName,
		CountryCode: role.CountryCode}
	for _, i := range role.Identifiers {
		out.Identifiers = append(out.Identifiers, doc.Identifier{Type: i.Type, Country: i.Country, Value: i.Value, None: i.None})
	}
	book, err := d.ORG.GetPartyAddresses(ctx, orgapi.GetPartyAddresses{TenantID: string(tenant), PartyID: party})
	if err != nil {
		return doc.Party{}, unknown("addresses of "+party, err)
	}
	for _, a := range book.Addresses {
		if slices.Contains(a.DefaultFor, orgapi.AddressOffice) {
			out.Address = &doc.Address{Source: "ORG", ORGAddressID: a.AddressID, ORGVersion: a.Version, Name: a.Name, Lines: append([]string(nil), a.Lines...),
				City: a.City, Region: a.Region, Postcode: a.Postcode, CountryCode: a.CountryCode, UNLocode: a.UNLocode}
		}
	}
	return out, nil
}
