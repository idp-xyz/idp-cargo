package com

import (
	"context"

	"go.idp.xyz/idp-cargo/internal/contexts/com/api"
	"go.idp.xyz/idp-cargo/internal/contexts/com/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/com/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
)

// Closing orders and erasing their contacts (P1).

func (m *Module) CloseCustomerOrder(ctx context.Context, cmd api.CloseCustomerOrder) (api.Receipt, error) {
	receipt, err := application.CloseCustomerOrder{Deps: m.deps(cmd.TenantID, cmd.CommandID)}.Handle(ctx, application.CloseCustomerOrderCommand{
		CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), OrderID: domain.OrderID(cmd.OrderID),
		ExpectedRevision: cmd.ExpectedRevision, OccurredAt: stored(cmd.OccurredAt),
	})
	return toReceipt(receipt, err)
}

func (m *Module) EraseOrderContacts(ctx context.Context, cmd api.EraseOrderContacts) (api.Receipt, error) {
	receipt, err := application.EraseOrderContacts{Deps: m.deps(cmd.TenantID, cmd.CommandID)}.Handle(ctx, application.EraseOrderContactsCommand{
		CommandID: cmd.CommandID, Scope: scope(cmd.TenantID, cmd.ActorID), OrderID: domain.OrderID(cmd.OrderID), OccurredAt: stored(cmd.OccurredAt),
	})
	return toReceipt(receipt, err)
}

func (m *Module) ListOrdersDueForContactErasure(ctx context.Context, q api.ListOrdersDueForContactErasure) (api.DueOrders, error) {
	due, asOf, err := application.ListOrdersDueForContactErasure{
		Tx: bento.TenantTransactor{DB: m.db, Tenant: q.TenantID}, Orders: postgres.RetentionReader{DB: m.db}, Auth: authorizer{m.auth}, Clock: m.clock,
	}.Handle(ctx, scope(q.TenantID, q.ActorID), q.Limit)
	if err != nil {
		return api.DueOrders{}, classify(err)
	}
	out := api.DueOrders{AsOf: asOf}
	for _, d := range due {
		out.Orders = append(out.Orders, api.DueOrder{OrderID: string(d.OrderID), RetainedUntil: d.RetainedUntil})
	}
	return out, nil
}

func (m *Module) ListContactAccess(ctx context.Context, q api.ListContactAccess) ([]api.ContactAccess, error) {
	reads, err := application.ListContactAccess{Tx: bento.TenantTransactor{DB: m.db, Tenant: q.TenantID}, Log: postgres.ContactAccessLog{DB: m.db},
		Auth: authorizer{m.auth}}.Handle(ctx, scope(q.TenantID, q.ActorID),
		application.AccessFilter{OrderID: domain.OrderID(q.OrderID), ActorID: q.ReadBy, Since: q.Since, Until: q.Until, Limit: q.Limit})
	if err != nil {
		return nil, classify(err)
	}
	out := make([]api.ContactAccess, 0, len(reads))
	for _, r := range reads {
		out = append(out, api.ContactAccess{AccessID: r.AccessID, ActorID: r.ActorID, Action: r.Action, OrderID: string(r.OrderID),
			Restricted: r.Restricted, Purpose: r.Purpose, Contacts: r.Contacts, RecordedAt: r.RecordedAt})
	}
	return out, nil
}
