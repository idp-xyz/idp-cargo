package postgres

import (
	"context"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	app "go.idp.xyz/idp-cargo/internal/contexts/org/internal/application"
	org "go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// ContactAccessLog keeps the log of contact reads in cargo_org.party_contact_access (migration 000009). Rows are
// written in the reading transaction, timed by the database, and never rewritten.
type ContactAccessLog struct{ DB *bentopg.DB }

var _ app.ContactAccessLog = ContactAccessLog{}

func (l ContactAccessLog) RecordContactsRead(ctx context.Context, tenant org.TenantID, r app.ContactAccess) error {
	q, err := l.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(r.ContactIDs))
	for _, id := range r.ContactIDs {
		ids = append(ids, string(id))
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_org.party_contact_access (tenant_id,party_id,actor_id,action,purpose,contact_ids)
      VALUES ($1,$2,$3,$4,$5,$6)`, string(tenant), string(r.PartyID), r.ActorID, r.Action, string(r.Purpose), ids)
	return err
}

func (l ContactAccessLog) ListContactsReads(ctx context.Context, tenant org.TenantID, f app.AccessFilter) ([]app.ContactAccess, error) {
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
	rows, err := q.Query(ctx, `SELECT access_id::text,actor_id,action,party_id,purpose,contact_ids,recorded_at FROM cargo_org.party_contact_access
      WHERE tenant_id=$1 AND ($2='' OR party_id=$2) AND ($3='' OR actor_id=$3)
        AND ($4::timestamptz IS NULL OR recorded_at >= $4) AND ($5::timestamptz IS NULL OR recorded_at < $5)
      ORDER BY recorded_at DESC, access_id LIMIT $6`, string(tenant), string(f.PartyID), f.ActorID, since, until, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []app.ContactAccess
	for rows.Next() {
		var r app.ContactAccess
		var party, purpose string
		var ids []string
		var at time.Time
		if err = rows.Scan(&r.AccessID, &r.ActorID, &r.Action, &party, &purpose, &ids, &at); err != nil {
			return nil, err
		}
		r.PartyID, r.Purpose, r.RecordedAt = org.PartyID(party), kernel.AccessPurpose(purpose), at.UTC()
		for _, id := range ids {
			r.ContactIDs = append(r.ContactIDs, org.ContactID(id))
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
