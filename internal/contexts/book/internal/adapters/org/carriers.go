// Package org is BOOK's anticorruption layer for ORG (relationship 30): the carrier's role decision and, for an
// airline, its IATA designator and AWB prefix, frozen into the booking. ORG's error kinds are kept as text only.
package org

import (
	"context"
	"errors"
	"fmt"
	"time"

	app "go.idp.xyz/idp-cargo/internal/contexts/book/internal/application"
	book "go.idp.xyz/idp-cargo/internal/contexts/book/internal/domain"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
)

// Carriers confirms carriers through ORG's query side. Call it outside BOOK's transactions.
type Carriers struct{ ORG orgapi.Queries }

var _ app.Carriers = Carriers{}

func (c Carriers) Carrier(ctx context.Context, tenant book.TenantID, party, role, legalEntity string, at time.Time) (book.Carrier, error) {
	answer, err := c.ORG.ResolvePartyRole(ctx, orgapi.ResolvePartyRole{TenantID: string(tenant), PartyID: party, RoleCode: role,
		LegalEntityID: legalEntity, At: at})
	if errors.Is(err, orgapi.ErrRejected) || errors.Is(err, orgapi.ErrNotFound) {
		return book.Carrier{}, fmt.Errorf("%w: %s as %s: %v", app.ErrCarrierNotConfirmed, party, role, err.Error())
	}
	if err != nil {
		return book.Carrier{}, err
	}
	if answer.Ref.IdentitySource != orgapi.IdentitySource || answer.Ref.TenantID != string(tenant) || answer.Ref.PartyID != party || answer.RoleCode != role {
		return book.Carrier{}, fmt.Errorf("%w: ORG answered for another party or role", app.ErrCarrierNotConfirmed)
	}
	out := book.Carrier{PartyID: party, PartyRevision: answer.Ref.PartyRevision, Role: role, ValidFrom: answer.Validity.From.UTC(),
		RoleEvidence: answer.EvidenceRef}
	if !answer.Validity.Until.IsZero() {
		out.ValidUntil = answer.Validity.Until.UTC()
	}
	for _, id := range answer.Identifiers {
		if id.Country != orgapi.InternationalIssuer || id.None {
			continue
		}
		switch id.Type {
		case orgapi.IdentifierIATAAirline:
			out.IATADesignator = id.Value
		case orgapi.IdentifierIATAAWBPrefix:
			out.AWBPrefix = id.Value
		}
	}
	return out, nil
}
