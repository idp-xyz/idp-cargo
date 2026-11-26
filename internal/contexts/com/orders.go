package com

import (
	"context"

	"go.idp.xyz/idp-cargo/internal/contexts/com/api"
	"go.idp.xyz/idp-cargo/internal/contexts/com/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
)

// GetCustomerOrder reads an order for people.
func (m *Module) GetCustomerOrder(ctx context.Context, q api.GetCustomerOrder) (api.CustomerOrder, error) {
	if q.TenantID == "" {
		return api.CustomerOrder{}, api.ErrUnauthenticated
	}
	var order *domain.CustomerOrder
	var revision int64
	err := (bento.TenantTransactor{DB: m.db, Tenant: q.TenantID}).WithinTransaction(ctx, func(txCtx context.Context) (err error) {
		order, revision, err = postgres.PartyReader{DB: m.db}.ReadOrder(txCtx, domain.TenantID(q.TenantID), domain.OrderID(q.OrderID))
		return err
	})
	if err != nil {
		return api.CustomerOrder{}, classify(err)
	}
	st, t := order.State(), order.Transport()
	out := api.CustomerOrder{OrderID: string(st.OrderID), Revision: revision, Status: string(st.Status), LegalEntityID: string(st.Quote.LegalEntityID),
		CustomerPartyID: st.Customer.PartyID, CustomerPartyRevision: st.Customer.PartyRevision, CustomerLegalName: st.Customer.LegalName,
		QuoteID: st.Quote.QuoteID, QuoteRevision: st.Quote.QuoteRevision,
		Transport:  api.Transport{Mode: string(t.Mode), Movement: string(t.Movement), ContainerMode: string(t.ContainerMode)},
		RecordedBy: st.RecordedBy, RecordedAt: st.RecordedAt, CancelledBy: st.CancelledBy, CancelledAt: st.CancelledAt, ClosedBy: st.ClosedBy, ClosedAt: st.ClosedAt,
		Lines: []api.OrderLineView{}, Obligations: []api.ObligationView{}, Reservations: []api.ReservationView{}}
	versions := map[int]int64{}
	for _, c := range st.LineChanges {
		versions[c.LineNo] = c.Version
	}
	for _, l := range order.Lines() {
		v := versions[l.LineNo]
		if v == 0 {
			v = 1
		}
		out.Lines = append(out.Lines, api.OrderLineView{LineNo: l.LineNo, Version: v, Description: l.Description, PackageType: string(l.PackageType),
			Packs: l.Measure.Packs, GrossWeight: l.Measure.GrossWeight, Volume: l.Measure.Volume})
	}
	for _, o := range st.Obligations {
		out.Obligations = append(out.Obligations, api.ObligationView{ObligationID: string(o.ObligationID), Version: o.Version, LineNo: o.LineNo,
			Packs: o.Measure.Packs, GrossWeight: o.Measure.GrossWeight, Volume: o.Measure.Volume, Status: string(o.Status), AllocatedBy: o.AllocatedBy,
			AllocatedAt: o.AllocatedAt, RevokedBy: o.RevokedBy, RevokedAt: o.RevokedAt})
	}
	for _, r := range st.Reservations {
		out.Reservations = append(out.Reservations, api.ReservationView{No: r.No, ObligationID: string(r.ObligationID), ObligationVersion: r.ObligationVersion,
			ShipmentID: r.ShipmentID, ReservedBy: r.ReservedBy, ReservedAt: r.ReservedAt, ReleasedBy: r.ReleasedBy, ReleasedAt: r.ReleasedAt})
	}
	return out, nil
}
