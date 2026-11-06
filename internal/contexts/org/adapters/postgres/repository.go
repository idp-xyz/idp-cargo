//go:build bento

package postgres

import (
	"context"

	bentorepo "go.idp.xyz/idp-bento-go/repository"
	org "go.idp.xyz/idp-cargo/internal/contexts/org/domain"
	cargobento "go.idp.xyz/idp-cargo/internal/platform/bento"
)

// PartyRepository exposes PartyStore through Bento's portable repository ports.
type PartyRepository struct{ Store PartyStore }

var (
	_ bentorepo.Loader[cargobento.ScopedPartyKey, *org.BusinessParty]           = PartyRepository{}
	_ bentorepo.Inserter[*org.BusinessParty]                                    = PartyRepository{}
	_ bentorepo.VersionedUpdater[cargobento.ScopedPartyKey, *org.BusinessParty] = PartyRepository{}
)

func (r PartyRepository) Load(ctx context.Context, key cargobento.ScopedPartyKey) (bentorepo.Loaded[*org.BusinessParty], error) {
	party, revision, err := r.Store.Load(ctx, key.TenantID, key.ID)
	if err != nil {
		return bentorepo.Loaded[*org.BusinessParty]{}, err
	}
	return bentorepo.Loaded[*org.BusinessParty]{Aggregate: party, Revision: bentorepo.Revision(revision)}, nil
}

func (r PartyRepository) Insert(ctx context.Context, party *org.BusinessParty) (bentorepo.Revision, error) {
	revision, err := r.Store.Insert(ctx, party)
	return bentorepo.Revision(revision), err
}

func (r PartyRepository) Update(ctx context.Context, key cargobento.ScopedPartyKey, party *org.BusinessParty, expected bentorepo.Revision) (bentorepo.Revision, error) {
	if party == nil || party.Tenant() != key.TenantID || party.ID() != key.ID {
		return 0, org.ErrInvalidIdentity
	}
	revision, err := r.Store.Save(ctx, party, int64(expected))
	return bentorepo.Revision(revision), err
}
