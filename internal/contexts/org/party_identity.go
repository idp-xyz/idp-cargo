package org

import (
	"context"

	"go.idp.xyz/idp-cargo/internal/contexts/org/api"
	"go.idp.xyz/idp-cargo/internal/contexts/org/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/org/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
)

// The party identity commands and query (ORG V0.4, MD-06).

func (m *Module) RecordPartyIdentifier(ctx context.Context, cmd api.RecordPartyIdentifier) (api.Receipt, error) {
	id := domain.PartyIdentifier{Type: domain.IdentifierType(cmd.Type), Country: cmd.Country, Value: cmd.Value, None: cmd.None, EvidenceRef: cmd.EvidenceRef}
	return m.maintainIdentity(ctx, application.PartyIdentityCommand{
		CommandID: cmd.CommandID, Scope: bookScope(cmd.TenantID, cmd.ActorID), PartyID: domain.PartyID(cmd.PartyID),
		ExpectedRevision: cmd.ExpectedRevision, Action: application.IdentityRecordIdentifier, Identifier: &id,
	})
}

func (m *Module) RetirePartyIdentifier(ctx context.Context, cmd api.RetirePartyIdentifier) (api.Receipt, error) {
	return m.maintainIdentity(ctx, application.PartyIdentityCommand{
		CommandID: cmd.CommandID, Scope: bookScope(cmd.TenantID, cmd.ActorID), PartyID: domain.PartyID(cmd.PartyID),
		ExpectedRevision: cmd.ExpectedRevision, Action: application.IdentityRetireIdentifier,
		IdentifierType: domain.IdentifierType(cmd.Type), Country: cmd.Country, Reason: cmd.Reason,
	})
}

func (m *Module) SetPartyEnglishName(ctx context.Context, cmd api.SetPartyEnglishName) (api.Receipt, error) {
	return m.maintainIdentity(ctx, application.PartyIdentityCommand{
		CommandID: cmd.CommandID, Scope: bookScope(cmd.TenantID, cmd.ActorID), PartyID: domain.PartyID(cmd.PartyID),
		ExpectedRevision: cmd.ExpectedRevision, Action: application.IdentitySetEnglishName,
		EnglishName: cmd.EnglishName, EvidenceRef: cmd.EvidenceRef,
	})
}

func (m *Module) GetPartyIdentity(ctx context.Context, q api.GetPartyIdentity) (api.PartyIdentity, error) {
	party, revision, err := application.ReadPartyIdentity{
		Tx: bento.TenantTransactor{DB: m.db, Tenant: q.TenantID}, Parties: postgres.PartyReader{DB: m.db},
	}.Handle(ctx, domain.TenantID(q.TenantID), domain.PartyID(q.PartyID))
	if err != nil {
		return api.PartyIdentity{}, classify(err)
	}
	return api.PartyIdentity{PartyID: string(party.ID()), Revision: revision, Status: string(party.Status()), LegalName: party.Name(),
		EnglishName: party.EnglishName(), CountryCode: party.Country(), Identifiers: identifiersOf(party)}, nil
}

func (m *Module) maintainIdentity(ctx context.Context, c application.PartyIdentityCommand) (api.Receipt, error) {
	r, err := application.MaintainPartyIdentity{
		Tx:       bento.TenantTransactor{DB: m.db, Tenant: string(c.Scope.TenantID)},
		Parties:  postgres.PartyStore{DB: m.db},
		Holders:  postgres.IdentifierHolders{DB: m.db},
		Auth:     authorizer{m.auth},
		Receipts: postgres.CommandReceipts{DB: m.db},
		Outbox:   postgres.Outbox{Writer: m.outbox, CommandID: c.CommandID, RecordedAt: m.clock.Now()},
		Clock:    m.clock,
	}.Handle(ctx, c)
	if err != nil {
		return api.Receipt{}, classify(err)
	}
	return api.Receipt{CommandID: r.CommandID, PartyID: string(r.PartyID), NewRevision: r.NewRevision}, nil
}

func identifiersOf(p *domain.BusinessParty) []api.PartyIdentifier {
	ids := p.Identifiers()
	if len(ids) == 0 {
		return nil
	}
	out := make([]api.PartyIdentifier, len(ids))
	for i, id := range ids {
		out[i] = api.PartyIdentifier{Type: string(id.Type), Country: id.Country, Value: id.Value, None: id.None}
	}
	return out
}
