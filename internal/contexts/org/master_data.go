package org

import (
	"context"
	"fmt"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/org/api"
	"go.idp.xyz/idp-cargo/internal/contexts/org/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/org/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
)

// The master data commands and queries (since V2.106).

func (m *Module) RegisterParty(ctx context.Context, cmd api.RegisterParty) (api.Receipt, error) {
	return m.masterData(ctx, cmd.TenantID, cmd.ActorID, application.MasterDataCommand{CommandID: cmd.CommandID, Action: application.ActionRegisterParty,
		Subject: cmd.PartyID, Name: cmd.LegalName, Country: cmd.CountryCode})
}

func (m *Module) ActivateParty(ctx context.Context, cmd api.ActivateParty) (api.Receipt, error) {
	return m.masterData(ctx, cmd.TenantID, cmd.ActorID, application.MasterDataCommand{CommandID: cmd.CommandID, Action: application.ActionActivateParty,
		Subject: cmd.PartyID, ExpectedRevision: cmd.ExpectedRevision, EvidenceRef: cmd.EvidenceRef})
}

func (m *Module) CorrectPartyLegalName(ctx context.Context, cmd api.CorrectPartyLegalName) (api.Receipt, error) {
	return m.masterData(ctx, cmd.TenantID, cmd.ActorID, application.MasterDataCommand{CommandID: cmd.CommandID, Action: application.ActionCorrectPartyLegalName,
		Subject: cmd.PartyID, ExpectedRevision: cmd.ExpectedRevision, Name: cmd.LegalName, EvidenceRef: cmd.EvidenceRef})
}

func (m *Module) SuspendParty(ctx context.Context, cmd api.SuspendParty) (api.Receipt, error) {
	return m.masterData(ctx, cmd.TenantID, cmd.ActorID, application.MasterDataCommand{CommandID: cmd.CommandID, Action: application.ActionSuspendParty,
		Subject: cmd.PartyID, ExpectedRevision: cmd.ExpectedRevision, Reason: cmd.Reason})
}

func (m *Module) ArchiveParty(ctx context.Context, cmd api.ArchiveParty) (api.Receipt, error) {
	return m.masterData(ctx, cmd.TenantID, cmd.ActorID, application.MasterDataCommand{CommandID: cmd.CommandID, Action: application.ActionArchiveParty,
		Subject: cmd.PartyID, ExpectedRevision: cmd.ExpectedRevision, Reason: cmd.Reason})
}

func (m *Module) RegisterLegalEntity(ctx context.Context, cmd api.RegisterLegalEntity) (api.LegalEntityReceipt, error) {
	r, err := m.masterData(ctx, cmd.TenantID, cmd.ActorID, application.MasterDataCommand{CommandID: cmd.CommandID, Action: application.ActionRegisterLegalEntity,
		Subject: cmd.LegalEntityID, Name: cmd.RegisteredName, Country: cmd.Jurisdiction, EvidenceRef: cmd.EvidenceRef})
	return api.LegalEntityReceipt{CommandID: r.CommandID, LegalEntityID: r.PartyID, NewRevision: r.NewRevision}, err
}

func (m *Module) VerifyLegalEntity(ctx context.Context, cmd api.VerifyLegalEntity) (api.LegalEntityReceipt, error) {
	r, err := m.masterData(ctx, cmd.TenantID, cmd.ActorID, application.MasterDataCommand{CommandID: cmd.CommandID, Action: application.ActionVerifyLegalEntity,
		Subject: cmd.LegalEntityID, ExpectedRevision: cmd.ExpectedRevision})
	return api.LegalEntityReceipt{CommandID: r.CommandID, LegalEntityID: r.PartyID, NewRevision: r.NewRevision}, err
}

func (m *Module) masterData(ctx context.Context, tenant, actor string, c application.MasterDataCommand) (api.Receipt, error) {
	c.Scope = bookScope(tenant, actor)
	r, err := application.MaintainMasterData{
		Tx:            bento.TenantTransactor{DB: m.db, Tenant: tenant},
		Parties:       postgres.PartyStore{DB: m.db},
		Registry:      postgres.PartyStore{DB: m.db},
		LegalEntities: postgres.LegalEntityStore{DB: m.db},
		Auth:          authorizer{m.auth},
		Receipts:      postgres.CommandReceipts{DB: m.db},
		Outbox:        postgres.Outbox{Writer: m.outbox, CommandID: c.CommandID, RecordedAt: m.clock.Now()},
		Clock:         m.clock,
	}.Handle(ctx, c)
	if err != nil {
		return api.Receipt{}, classify(err)
	}
	return api.Receipt{CommandID: r.CommandID, PartyID: string(r.PartyID), NewRevision: r.NewRevision}, nil
}

func (m *Module) ListParties(ctx context.Context, q api.ListParties) (api.PartyList, error) {
	switch {
	case q.TenantID == "":
		return api.PartyList{}, api.ErrUnauthenticated
	case q.Limit < 0 || q.Limit > 200:
		return api.PartyList{}, fmt.Errorf("%w: a page is 1 to 200 parties", api.ErrRejected)
	case q.Status != "" && q.Status != api.PartyDraft && q.Status != api.PartyActive && q.Status != api.PartySuspended && q.Status != api.PartyArchived:
		return api.PartyList{}, fmt.Errorf("%w: status %q is DRAFT, ACTIVE, SUSPENDED or ARCHIVED", api.ErrRejected, q.Status)
	}
	limit := q.Limit
	if limit == 0 {
		limit = 50
	}
	var rows []postgres.PartySummaryRecord
	err := (bento.TenantTransactor{DB: m.db, Tenant: q.TenantID}).WithinTransaction(ctx, func(txCtx context.Context) (err error) {
		rows, err = postgres.PartyListing{DB: m.db}.List(txCtx, domain.TenantID(q.TenantID), q.Status, q.RoleCode, q.NameContains, q.After, limit+1)
		return err
	})
	if err != nil {
		return api.PartyList{}, classify(err)
	}
	out := api.PartyList{Parties: []api.PartySummary{}}
	for i, r := range rows {
		if i == limit {
			out.Next = rows[limit-1].PartyID
			break
		}
		s := api.PartySummary{PartyID: r.PartyID, Revision: r.Revision, LegalName: r.LegalName, EnglishName: r.EnglishName, CountryCode: r.Country,
			Status: r.Status, Roles: []api.ApprovedRole{}}
		for _, role := range r.Roles {
			s.Roles = append(s.Roles, api.ApprovedRole{RoleCode: string(role.Code), LegalEntityID: string(role.LegalEntityID), ValidFrom: role.ValidFrom.UTC(),
				ValidUntil: utcOrZero(role.ValidUntil), EvidenceRef: role.EvidenceRef, ApprovedBy: role.ApprovedBy, ApprovedAt: role.ApprovedAt.UTC()})
		}
		out.Parties = append(out.Parties, s)
	}
	return out, nil
}

func (m *Module) ListLegalEntities(ctx context.Context, q api.ListLegalEntities) ([]api.LegalEntityRef, error) {
	if q.TenantID == "" {
		return nil, api.ErrUnauthenticated
	}
	var rows []postgres.LegalEntityRecord
	var ids []string
	err := (bento.TenantTransactor{DB: m.db, Tenant: q.TenantID}).WithinTransaction(ctx, func(txCtx context.Context) (err error) {
		rows, ids, err = postgres.LegalEntityListing{DB: m.db}.List(txCtx, domain.TenantID(q.TenantID))
		return err
	})
	if err != nil {
		return nil, classify(err)
	}
	out := []api.LegalEntityRef{}
	for i, r := range rows {
		out = append(out, api.LegalEntityRef{TenantID: q.TenantID, LegalEntityID: ids[i], Revision: r.Revision, RegisteredName: r.RegisteredName,
			Jurisdiction: r.Jurisdiction, Status: r.Status})
	}
	return out, nil
}

// utcOrZero is t in UTC, or the zero time for an open end.
func utcOrZero(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.UTC()
}
