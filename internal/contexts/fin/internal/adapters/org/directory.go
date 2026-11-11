// Package org is FIN's anticorruption layer for ORG (ORG -> FIN): legal entities (LegalEntityRef v1), the roles
// of debtors and creditors (PartyRef v1) and debtors' identity for billing profiles. ORG's error kinds are kept as
// text only.
package org

import (
	"context"
	"errors"
	"fmt"
	"time"

	app "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/application"
	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
)

// ORG is what FIN asks ORG.
type ORG interface {
	orgapi.Queries
	orgapi.LegalEntityQueries
	orgapi.PartyIdentityQueries
}

// Directory reads ORG. Call it outside FIN's transactions.
type Directory struct{ ORG ORG }

var _ app.Directory = Directory{}

func (d Directory) LegalEntityVerified(ctx context.Context, tenant fin.TenantID, legalEntity string) (bool, error) {
	le, err := d.ORG.GetLegalEntity(ctx, orgapi.GetLegalEntity{TenantID: string(tenant), LegalEntityID: legalEntity})
	if errors.Is(err, orgapi.ErrNotFound) {
		return false, fmt.Errorf("%w: legal entity %s: %v", app.ErrUnknownSource, legalEntity, err.Error())
	}
	if err != nil {
		return false, err
	}
	return le.LegalEntityID == legalEntity && le.Status == orgapi.LegalEntityVerified, nil
}

// PartyInRole asks ORG for each role in turn and answers the party's revision under the first it holds.
func (d Directory) PartyInRole(ctx context.Context, tenant fin.TenantID, party, legalEntity string, roles []string, at time.Time) (int64, error) {
	var refused []string
	for _, role := range roles {
		r, err := d.ORG.ResolvePartyRole(ctx, orgapi.ResolvePartyRole{TenantID: string(tenant), PartyID: party, RoleCode: role, LegalEntityID: legalEntity, At: at})
		if errors.Is(err, orgapi.ErrRejected) || errors.Is(err, orgapi.ErrNotFound) {
			refused = append(refused, role)
			continue
		}
		if err != nil {
			return 0, err
		}
		if r.Ref.PartyID != party {
			return 0, fmt.Errorf("%w: ORG answered for another party", app.ErrUnknownSource)
		}
		return r.Ref.PartyRevision, nil
	}
	return 0, fmt.Errorf("%w: %s holds none of %v for %s", fin.ErrPartyRole, party, refused, legalEntity)
}

// PartyIdentity asks ORG for a party's identity: the unified social credit code is the active CN_USCC identifier
// registered in CN, if any.
func (d Directory) PartyIdentity(ctx context.Context, tenant fin.TenantID, party string) (fin.PartyIdentity, error) {
	id, err := d.ORG.GetPartyIdentity(ctx, orgapi.GetPartyIdentity{TenantID: string(tenant), PartyID: party})
	if errors.Is(err, orgapi.ErrNotFound) {
		return fin.PartyIdentity{}, fmt.Errorf("%w: party %s: %v", app.ErrUnknownSource, party, err.Error())
	}
	if err != nil {
		return fin.PartyIdentity{}, err
	}
	if id.PartyID != party {
		return fin.PartyIdentity{}, fmt.Errorf("%w: ORG answered for another party", app.ErrUnknownSource)
	}
	out := fin.PartyIdentity{PartyID: id.PartyID, Revision: id.Revision, Status: id.Status, LegalName: id.LegalName, EnglishName: id.EnglishName,
		CountryCode: id.CountryCode}
	for _, x := range id.Identifiers {
		if x.Type == orgapi.IdentifierCNUSCC && x.Country == "CN" && !x.None {
			out.USCC = x.Value
		}
	}
	return out, nil
}
