package postgres

import (
	"context"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	app "go.idp.xyz/idp-cargo/internal/contexts/com/internal/application"
	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// ContactAccessLog keeps the log of order contact reads in cargo_com.order_contact_access (migration 000008).
// Rows are written in the reading transaction, timed by the database, and never rewritten.
type ContactAccessLog struct{ DB *bentopg.DB }

var _ app.ContactAccessLog = ContactAccessLog{}

func (l ContactAccessLog) RecordContactsRead(ctx context.Context, tenant com.TenantID, r app.ContactAccess) error {
	q, err := l.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_com.order_contact_access (tenant_id,order_id,actor_id,action,restricted,purpose,contacts)
      VALUES ($1,$2,$3,$4,$5,$6,$7)`, string(tenant), string(r.OrderID), r.ActorID, r.Action, r.Restricted, string(r.Purpose), r.Contacts)
	return err
}

func (l ContactAccessLog) ListContactsReads(ctx context.Context, tenant com.TenantID, f app.AccessFilter) ([]app.ContactAccess, error) {
	q, err := l.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	var since, until any
	if !f.Since.IsZero() {
		since = f.Since
	}
	if !f.Until.IsZero() {
		until = f.Until
	}
	rows, err := q.Query(ctx, `SELECT access_id::text,actor_id,action,order_id,restricted,purpose,contacts,recorded_at FROM cargo_com.order_contact_access
      WHERE tenant_id=$1 AND ($2='' OR order_id=$2) AND ($3='' OR actor_id=$3)
        AND ($4::timestamptz IS NULL OR recorded_at >= $4) AND ($5::timestamptz IS NULL OR recorded_at < $5)
      ORDER BY recorded_at DESC, access_id LIMIT $6`, string(tenant), string(f.OrderID), f.ActorID, since, until, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []app.ContactAccess
	for rows.Next() {
		var r app.ContactAccess
		var order, purpose string
		var at time.Time
		if err = rows.Scan(&r.AccessID, &r.ActorID, &r.Action, &order, &r.Restricted, &purpose, &r.Contacts, &at); err != nil {
			return nil, err
		}
		r.OrderID, r.Purpose, r.RecordedAt = com.OrderID(order), kernel.AccessPurpose(purpose), at.UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}
