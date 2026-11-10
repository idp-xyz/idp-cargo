package org

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	app "go.idp.xyz/idp-cargo/internal/contexts/com/internal/application"
	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
)

// Directory answers COM's questions about order parties through ORG's query side (C9): role confirmations
// with identity (ORG V0.4), the identity of an active party, addresses and contacts (ORG V0.3). ORG's error
// kinds are kept as text only, so they never become COM's. Call it outside COM's transactions.
type Directory struct {
	Roles      orgapi.Queries
	Identities orgapi.PartyIdentityQueries
	Book       orgapi.AddressBookQueries
}

var _ app.Directory = Directory{}

func (d Directory) ConfirmedParty(ctx context.Context, tenant com.TenantID, partyID, role string, entity com.LegalEntityID, at time.Time) (com.PartyRecord, error) {
	answer, err := d.Roles.ResolvePartyRole(ctx, orgapi.ResolvePartyRole{TenantID: string(tenant), PartyID: partyID, RoleCode: role,
		LegalEntityID: string(entity), At: at})
	if err != nil {
		return com.PartyRecord{}, notConfirmed(err)
	}
	if answer.Ref.IdentitySource != orgapi.IdentitySource || answer.Ref.TenantID != string(tenant) || answer.Ref.PartyID != partyID || answer.RoleCode != role {
		return com.PartyRecord{}, fmt.Errorf("%w: ORG answered for another party or role", app.ErrPartyNotConfirmed)
	}
	return com.PartyRecord{PartyID: partyID, PartyRevision: answer.Ref.PartyRevision, LegalName: answer.LegalName, EnglishName: answer.EnglishName,
		CountryCode: answer.CountryCode, Identifiers: identifiers(answer.Identifiers), RoleCode: answer.RoleCode,
		RoleValidity: answer.Validity, RoleEvidenceRef: answer.EvidenceRef}, nil
}

func (d Directory) ActiveParty(ctx context.Context, tenant com.TenantID, partyID string) (com.PartyRecord, error) {
	identity, err := d.Identities.GetPartyIdentity(ctx, orgapi.GetPartyIdentity{TenantID: string(tenant), PartyID: partyID})
	if err != nil {
		return com.PartyRecord{}, notConfirmed(err)
	}
	if identity.PartyID != partyID || identity.Status != "ACTIVE" {
		return com.PartyRecord{}, fmt.Errorf("%w: party %s is %s", app.ErrPartyNotConfirmed, partyID, identity.Status)
	}
	return com.PartyRecord{PartyID: partyID, PartyRevision: identity.Revision, LegalName: identity.LegalName, EnglishName: identity.EnglishName,
		CountryCode: identity.CountryCode, Identifiers: identifiers(identity.Identifiers)}, nil
}

func (d Directory) Address(ctx context.Context, tenant com.TenantID, partyID, addressID, purpose string) (com.AddressSnapshot, bool, error) {
	if addressID != "" {
		a, err := d.Book.GetPartyAddress(ctx, orgapi.GetPartyAddress{TenantID: string(tenant), PartyID: partyID, AddressID: addressID})
		if err != nil {
			return com.AddressSnapshot{}, false, notConfirmed(err)
		}
		if a.Status != orgapi.StatusActive {
			return com.AddressSnapshot{}, false, fmt.Errorf("%w: address %s of party %s is %s", app.ErrPartyNotConfirmed, addressID, partyID, a.Status)
		}
		return snapshot(a), true, nil
	}
	book, err := d.Book.GetPartyAddresses(ctx, orgapi.GetPartyAddresses{TenantID: string(tenant), PartyID: partyID})
	if err != nil {
		return com.AddressSnapshot{}, false, notConfirmed(err)
	}
	for _, a := range book.Addresses {
		if a.Status == orgapi.StatusActive && slices.Contains(a.DefaultFor, purpose) {
			return snapshot(a), true, nil
		}
	}
	return com.AddressSnapshot{}, false, nil
}

func (d Directory) Contact(ctx context.Context, tenant com.TenantID, actor, partyID, contactID string) (com.ContactSnapshot, bool, error) {
	contacts, err := d.Book.GetPartyContacts(ctx, orgapi.GetPartyContacts{TenantID: string(tenant), ActorID: actor, PartyID: partyID})
	if errors.Is(err, orgapi.ErrUnauthorized) {
		return com.ContactSnapshot{}, false, fmt.Errorf("%w: ORG does not let %s read the contacts of %s: %v", app.ErrUnauthorized, actor, partyID, err.Error())
	}
	if err != nil {
		return com.ContactSnapshot{}, false, notConfirmed(err)
	}
	for _, c := range contacts.Contacts {
		if c.Status != orgapi.StatusActive {
			continue
		}
		if contactID == c.ContactID || contactID == "" && slices.Contains(c.DefaultFor, orgapi.ContactDocuments) {
			return com.ContactSnapshot{Source: com.FromORG, ORGContactID: c.ContactID, ORGVersion: c.Version,
				Name: c.Name, Phone: c.Phone, Mobile: c.Mobile, Email: c.Email}, true, nil
		}
	}
	if contactID != "" {
		return com.ContactSnapshot{}, false, fmt.Errorf("%w: party %s has no active contact %s", app.ErrPartyNotConfirmed, partyID, contactID)
	}
	return com.ContactSnapshot{}, false, nil
}

// notConfirmed keeps ORG's refusal as text; other failures pass through.
func notConfirmed(err error) error {
	if errors.Is(err, orgapi.ErrRejected) || errors.Is(err, orgapi.ErrNotFound) {
		return fmt.Errorf("%w: %v", app.ErrPartyNotConfirmed, err.Error())
	}
	return err
}

func identifiers(in []orgapi.PartyIdentifier) []com.PartyIdentifier {
	var out []com.PartyIdentifier
	for _, i := range in {
		out = append(out, com.PartyIdentifier{Type: i.Type, Country: i.Country, Value: i.Value, None: i.None})
	}
	return out
}

func snapshot(a orgapi.PartyAddress) com.AddressSnapshot {
	out := com.AddressSnapshot{Source: com.FromORG, ORGAddressID: a.AddressID, ShortCode: a.ShortCode, ORGVersion: a.Version,
		Name: a.Name, Lines: slices.Clone(a.Lines), City: a.City, Region: a.Region, Postcode: a.Postcode, CountryCode: a.CountryCode,
		UNLocode: a.UNLocode, Residential: a.Residential, Phone: a.Phone, Email: a.Email}
	if l := a.Local; l != nil {
		out.Local = &com.LocalAddress{Language: l.Language, Name: l.Name, Lines: slices.Clone(l.Lines), City: l.City, Region: l.Region}
	}
	return out
}
