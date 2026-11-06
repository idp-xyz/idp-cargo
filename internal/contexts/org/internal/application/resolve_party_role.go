package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	org "go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
)

var ErrInvalidQuery = errors.New("org application: invalid query")

// PartyRoleQuery asks whether a party may act in a role for a legal entity at an instant.
type PartyRoleQuery struct {
	TenantID      org.TenantID
	PartyID       org.PartyID
	RoleCode      org.RoleCode
	LegalEntityID org.LegalEntityID
	At            time.Time
}

// PartyRoleAnswer is a positive answer taken from one consistent read of the party.
type PartyRoleAnswer struct {
	Party    *org.BusinessParty
	Revision int64
	Role     org.Role
}

// PartyReader reads a party and its approved roles as one consistent view, without locking it.
type PartyReader interface {
	Read(context.Context, org.TenantID, org.PartyID) (*org.BusinessParty, int64, error)
}

// LegalEntityStatuses reads the current status of a legal entity.
type LegalEntityStatuses interface {
	Status(context.Context, org.TenantID, org.LegalEntityID) (org.LegalEntityStatus, error)
}

// ResolvePartyRole answers PartyRoleQuery. The legal entity must be verified, the party active and the
// role approved and in force at the instant asked about.
type ResolvePartyRole struct {
	Tx       TxRunner
	Parties  PartyReader
	Entities LegalEntityStatuses
}

func (h ResolvePartyRole) Handle(ctx context.Context, q PartyRoleQuery) (PartyRoleAnswer, error) {
	if err := ctx.Err(); err != nil {
		return PartyRoleAnswer{}, err
	}
	if h.Tx == nil || h.Parties == nil || h.Entities == nil {
		return PartyRoleAnswer{}, ErrMissingDependency
	}
	if q.TenantID == "" {
		return PartyRoleAnswer{}, ErrUnauthenticated
	}
	if q.PartyID == "" || q.RoleCode == "" || q.LegalEntityID == "" || q.At.IsZero() {
		return PartyRoleAnswer{}, ErrInvalidQuery
	}
	if scoped, ok := h.Tx.(interface{ TenantScope() string }); ok && scoped.TenantScope() != string(q.TenantID) {
		return PartyRoleAnswer{}, ErrUnauthorized
	}
	var answer PartyRoleAnswer
	err := h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		status, err := h.Entities.Status(txCtx, q.TenantID, q.LegalEntityID)
		if err != nil {
			return err
		}
		if status != org.EntityVerified {
			return fmt.Errorf("%w: %s is %s", org.ErrLegalEntityNotVerified, q.LegalEntityID, status)
		}
		party, revision, err := h.Parties.Read(txCtx, q.TenantID, q.PartyID)
		if err != nil {
			return err
		}
		role, err := party.EffectiveRole(q.RoleCode, q.LegalEntityID, q.At)
		if err != nil {
			return err
		}
		answer = PartyRoleAnswer{Party: party, Revision: revision, Role: role}
		return nil
	})
	if err != nil {
		return PartyRoleAnswer{}, err
	}
	return answer, nil
}
