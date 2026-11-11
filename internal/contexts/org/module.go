// Package org assembles the ORG bounded context. Code outside ORG uses it through package api;
// everything under internal/ is private to ORG.
package org

import (
	"context"
	"errors"
	"fmt"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	"go.idp.xyz/idp-cargo/internal/contexts/org/api"
	"go.idp.xyz/idp-cargo/internal/contexts/org/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/org/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
)

// Module wires ORG's application services to PostgreSQL through IDP-Bento-Go.
type Module struct {
	db     *bentopg.DB
	outbox *bentooutbox.Store
	auth   api.Authorizer
	clock  bento.SystemClock
}

var (
	_ api.Commands              = (*Module)(nil)
	_ api.Queries               = (*Module)(nil)
	_ api.AddressBookCommands   = (*Module)(nil)
	_ api.AddressBookQueries    = (*Module)(nil)
	_ api.PartyIdentityCommands = (*Module)(nil)
	_ api.PartyIdentityQueries  = (*Module)(nil)
	_ api.ContactAccessQueries  = (*Module)(nil)
	_ api.LegalEntityQueries    = (*Module)(nil)
)

func New(db *bentopg.DB, auth api.Authorizer) (*Module, error) {
	if db == nil || auth == nil {
		return nil, errors.New("org: module needs a database and an authorizer")
	}
	outbox, err := bentooutbox.NewStore(db)
	if err != nil {
		return nil, err
	}
	return &Module{db: db, outbox: outbox, auth: auth}, nil
}

func (m *Module) ApproveRole(ctx context.Context, cmd api.ApproveRole) (api.Receipt, error) {
	legalEntity := domain.LegalEntityID(cmd.LegalEntityID)
	handler := application.ApproveRole{
		Tx:       bento.TenantTransactor{DB: m.db, Tenant: cmd.TenantID},
		Parties:  postgres.PartyStore{DB: m.db},
		Entities: postgres.LegalEntityEligibility{DB: m.db},
		Auth:     authorizer{m.auth},
		Receipts: postgres.CommandReceipts{DB: m.db},
		Outbox:   postgres.Outbox{Writer: m.outbox, CommandID: cmd.CommandID, RecordedAt: m.clock.Now()},
	}
	receipt, err := handler.Handle(ctx, application.RoleApprovalCommand{
		CommandID:        cmd.CommandID,
		Scope:            application.Scope{TenantID: domain.TenantID(cmd.TenantID), ActorID: cmd.ActorID, LegalEntityID: legalEntity},
		PartyID:          domain.PartyID(cmd.PartyID),
		ExpectedRevision: cmd.ExpectedRevision,
		Role: domain.Role{
			Code: domain.RoleCode(cmd.RoleCode), LegalEntityID: legalEntity,
			ValidFrom: cmd.ValidFrom, ValidUntil: cmd.ValidUntil, EvidenceRef: cmd.EvidenceRef,
		},
		OccurredAt: cmd.OccurredAt,
	})
	if err != nil {
		return api.Receipt{}, classify(err)
	}
	return api.Receipt{CommandID: receipt.CommandID, PartyID: string(receipt.PartyID), NewRevision: receipt.NewRevision}, nil
}

func (m *Module) ResolvePartyRole(ctx context.Context, q api.ResolvePartyRole) (api.PartyRole, error) {
	handler := application.ResolvePartyRole{
		Tx:       bento.TenantTransactor{DB: m.db, Tenant: q.TenantID},
		Parties:  postgres.PartyReader{DB: m.db},
		Entities: postgres.LegalEntityStatuses{DB: m.db},
	}
	answer, err := handler.Handle(ctx, application.PartyRoleQuery{
		TenantID: domain.TenantID(q.TenantID), PartyID: domain.PartyID(q.PartyID), RoleCode: domain.RoleCode(q.RoleCode),
		LegalEntityID: domain.LegalEntityID(q.LegalEntityID), At: q.At,
	})
	if err != nil {
		return api.PartyRole{}, classify(err)
	}
	party, role := answer.Party, answer.Role
	return api.PartyRole{
		Ref: api.PartyRef{
			TenantID: string(party.Tenant()), PartyID: string(party.ID()), PartyRevision: answer.Revision,
			IdentitySource: api.IdentitySource, LegalEntityScope: string(role.LegalEntityID),
		},
		LegalName: party.Name(), CountryCode: party.Country(), RoleCode: string(role.Code),
		Validity: role.Validity(), EvidenceRef: role.EvidenceRef,
		EnglishName: party.EnglishName(), Identifiers: identifiersOf(party),
	}, nil
}

// GetLegalEntity answers LegalEntityRef v1.
func (m *Module) GetLegalEntity(ctx context.Context, q api.GetLegalEntity) (api.LegalEntityRef, error) {
	if q.TenantID == "" || q.LegalEntityID == "" {
		return api.LegalEntityRef{}, classify(application.ErrInvalidQuery)
	}
	var r postgres.LegalEntityRecord
	err := (bento.TenantTransactor{DB: m.db, Tenant: q.TenantID}).WithinTransaction(ctx, func(txCtx context.Context) error {
		var err error
		r, err = postgres.LegalEntityRecords{DB: m.db}.Read(txCtx, domain.TenantID(q.TenantID), domain.LegalEntityID(q.LegalEntityID))
		return err
	})
	if err != nil {
		return api.LegalEntityRef{}, classify(err)
	}
	return api.LegalEntityRef{TenantID: q.TenantID, LegalEntityID: q.LegalEntityID, Revision: r.Revision, RegisteredName: r.RegisteredName,
		Jurisdiction: r.Jurisdiction, Status: r.Status}, nil
}

type authorizer struct{ api.Authorizer }

func (a authorizer) RequireRoleApproval(ctx context.Context, s application.Scope, p domain.PartyID, r domain.RoleCode) error {
	return a.AuthorizeRoleApproval(ctx, string(s.TenantID), s.ActorID, string(p), string(r))
}

func (a authorizer) RequirePartyAction(ctx context.Context, s application.Scope, action string, p domain.PartyID) error {
	return a.AuthorizePartyAction(ctx, string(s.TenantID), s.ActorID, action, string(p))
}

// classify wraps known ORG and Bento causes in their api error kind and passes anything else through.
func classify(err error) error {
	var kind error
	switch {
	case errors.Is(err, application.ErrUnauthenticated):
		kind = api.ErrUnauthenticated
	case errors.Is(err, application.ErrUnauthorized), errors.Is(err, domain.ErrCrossTenant), errors.Is(err, bento.ErrTenantScopeConflict):
		kind = api.ErrUnauthorized
	case errors.Is(err, application.ErrIdempotencyConflict):
		kind = api.ErrIdempotencyConflict
	case errors.Is(err, domain.ErrConflict), errors.Is(err, bentorepo.ErrConflict), errors.Is(err, domain.ErrEntryExists),
		errors.Is(err, domain.ErrIdentifierInUse):
		kind = api.ErrConflict
	case errors.Is(err, bentorepo.ErrNotFound), errors.Is(err, domain.ErrUnknownAddress), errors.Is(err, domain.ErrUnknownContact),
		errors.Is(err, domain.ErrUnknownIdentifier):
		kind = api.ErrNotFound
	case errors.Is(err, domain.ErrInvalidIdentity), errors.Is(err, domain.ErrInvalidTransition),
		errors.Is(err, domain.ErrRoleOverlap), errors.Is(err, domain.ErrMissingEvidence),
		errors.Is(err, domain.ErrPartyNotActive), errors.Is(err, domain.ErrRoleNotEffective),
		errors.Is(err, domain.ErrLegalEntityNotVerified), errors.Is(err, application.ErrInvalidQuery),
		errors.Is(err, application.ErrInvalidCommand), errors.Is(err, domain.ErrInvalidAddress), errors.Is(err, domain.ErrInvalidContact),
		errors.Is(err, domain.ErrInvalidChange), errors.Is(err, domain.ErrShortCodeInUse), errors.Is(err, domain.ErrEntryNotActive),
		errors.Is(err, domain.ErrNothingChanged), errors.Is(err, domain.ErrPartyArchived),
		errors.Is(err, domain.ErrInvalidIdentifier), errors.Is(err, domain.ErrIdentifierExists), errors.Is(err, domain.ErrInvalidConsent):
		kind = api.ErrRejected
	default:
		return err
	}
	return fmt.Errorf("%w: %w", kind, err)
}
