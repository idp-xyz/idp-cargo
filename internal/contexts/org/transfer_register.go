package org

import (
	"context"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/org/api"
	"go.idp.xyz/idp-cargo/internal/contexts/org/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/org/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
)

// ContactTransferRegister v1 (ORG V0.7).

func (m *Module) transfer(ctx context.Context, tenant, commandID string, c application.TransferCommand) (api.TransferReceipt, error) {
	r, err := application.MaintainTransferRegister{
		Tx:         bento.TenantTransactor{DB: m.db, Tenant: tenant},
		Store:      postgres.TransferRegister{DB: m.db},
		Auth:       authorizer{m.auth},
		Receipts:   postgres.CommandReceipts{DB: m.db},
		Outbox:     postgres.Outbox{Writer: m.outbox, CommandID: commandID, RecordedAt: m.clock.Now()},
		Clock:      m.clock,
		Thresholds: m.thresholds,
	}.Handle(ctx, c)
	if err != nil {
		return api.TransferReceipt{}, classify(err)
	}
	return api.TransferReceipt{CommandID: r.CommandID, Year: r.Year, Persons: r.Persons, State: string(r.State), NewRevision: r.NewRevision}, nil
}

func stamp(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC().Truncate(time.Microsecond)
}

func (m *Module) ReserveContactTransfers(ctx context.Context, cmd api.ReserveContactTransfers) (api.TransferReceipt, error) {
	return m.transfer(ctx, cmd.TenantID, cmd.CommandID, application.TransferCommand{CommandID: cmd.CommandID, Scope: bookScope(cmd.TenantID, cmd.ActorID),
		Action: application.ActionReserveContactTransfers, DispatchID: cmd.DispatchID, Keys: cmd.ContactKeys, OccurredAt: stamp(cmd.OccurredAt)})
}

func (m *Module) ReleaseContactTransfers(ctx context.Context, cmd api.ReleaseContactTransfers) (api.TransferReceipt, error) {
	return m.transfer(ctx, cmd.TenantID, cmd.CommandID, application.TransferCommand{CommandID: cmd.CommandID, Scope: bookScope(cmd.TenantID, cmd.ActorID),
		Action: application.ActionReleaseContactTransfers, DispatchID: cmd.DispatchID, OccurredAt: stamp(cmd.OccurredAt)})
}

func (m *Module) RecordTransferSafeguard(ctx context.Context, cmd api.RecordTransferSafeguard) (api.TransferReceipt, error) {
	return m.transfer(ctx, cmd.TenantID, cmd.CommandID, application.TransferCommand{CommandID: cmd.CommandID, Scope: bookScope(cmd.TenantID, cmd.ActorID),
		Action: application.ActionRecordTransferSafeguard, OccurredAt: stamp(cmd.OccurredAt),
		Safeguard: &domain.TransferSafeguard{SafeguardID: cmd.SafeguardID, Kind: cmd.Kind, Reference: cmd.Reference, Validity: cmd.Validity,
			EvidenceRef: cmd.EvidenceRef}})
}

func (m *Module) GetContactTransferRegister(ctx context.Context, q api.GetContactTransferRegister) (api.ContactTransferRegister, error) {
	v, err := application.ReadTransferRegister{Tx: bento.TenantTransactor{DB: m.db, Tenant: q.TenantID}, Store: postgres.TransferRegister{DB: m.db},
		Clock: m.clock, Thresholds: m.thresholds}.Handle(ctx, domain.TenantID(q.TenantID), q.Year, q.DispatchID)
	if err != nil {
		return api.ContactTransferRegister{}, classify(err)
	}
	out := api.ContactTransferRegister{Year: v.Year, Revision: v.Revision, Persons: v.Persons, State: string(v.State), DispatchKeys: v.DispatchKeys}
	if s := v.Safeguard; s != nil {
		out.Safeguard = &api.TransferSafeguard{SafeguardID: s.SafeguardID, Kind: s.Kind, Reference: s.Reference, Validity: s.Validity}
	}
	return out, nil
}
