package org

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	app "go.idp.xyz/idp-cargo/internal/contexts/rate/internal/application"
	rate "go.idp.xyz/idp-cargo/internal/contexts/rate/internal/domain"
)

// Parties confirms a rate card's customer or supplier through ORG's query side (RATE V0.5). Call it outside RATE's
// transactions.
type Parties struct{ ORG orgapi.Queries }

var _ app.Parties = Parties{}

// ConfirmRole asks ORG for each role in turn and answers the party's revision for the first one ORG confirms for the
// legal entity at the instant. ORG's answers are kept as text only, so its error kinds never become RATE's.
func (p Parties) ConfirmRole(ctx context.Context, tenant rate.TenantID, party string, entity rate.LegalEntityID, roles []string, at time.Time) (int64, error) {
	var refused []string
	for _, role := range roles {
		r, err := p.ORG.ResolvePartyRole(ctx, orgapi.ResolvePartyRole{TenantID: string(tenant), PartyID: party, RoleCode: role,
			LegalEntityID: string(entity), At: at})
		if errors.Is(err, orgapi.ErrRejected) || errors.Is(err, orgapi.ErrNotFound) {
			refused = append(refused, err.Error())
			continue
		}
		if err != nil {
			return 0, err
		}
		if r.Ref.PartyID != party {
			return 0, fmt.Errorf("%w: ORG answered for another party", app.ErrPartyNotConfirmed)
		}
		return r.Ref.PartyRevision, nil
	}
	return 0, fmt.Errorf("%w: %s holds none of %v for %s: %s", app.ErrPartyNotConfirmed, party, roles, entity, strings.Join(refused, "; "))
}
