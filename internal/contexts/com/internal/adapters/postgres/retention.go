package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	app "go.idp.xyz/idp-cargo/internal/contexts/com/internal/application"
	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
)

// retentionColumns reads an order's contact retention and erasure (P1) with the rest of the order.
const retentionColumns = `,
        (SELECT row_to_json(r) FROM cargo_com.order_contact_retention r WHERE r.tenant_id=o.tenant_id AND r.order_id=o.order_id),
        (SELECT row_to_json(e) FROM cargo_com.order_contact_erasure e WHERE e.tenant_id=o.tenant_id AND e.order_id=o.order_id)`

type retentionRow struct {
	Cause         string    `json:"cause"`
	StartedAt     time.Time `json:"started_at"`
	Months        int       `json:"months"`
	RetainedUntil time.Time `json:"retained_until"`
}

type erasureRow struct {
	ErasedBy  string    `json:"erased_by"`
	ErasedAt  time.Time `json:"erased_at"`
	EnteredAt time.Time `json:"entered_at"`
}

func restoreRetention(s *com.OrderState, retentionJSON, erasureJSON []byte) error {
	if len(retentionJSON) > 0 {
		var r retentionRow
		if err := json.Unmarshal(retentionJSON, &r); err != nil {
			return err
		}
		s.Retention = &com.ContactRetention{Cause: com.RetentionCause(r.Cause), StartedAt: utc(r.StartedAt), Months: r.Months,
			RetainedUntil: utc(r.RetainedUntil)}
	}
	if len(erasureJSON) > 0 {
		var e erasureRow
		if err := json.Unmarshal(erasureJSON, &e); err != nil {
			return err
		}
		s.Erasure = &com.ContactErasure{ErasedBy: e.ErasedBy, ErasedAt: utc(e.ErasedAt), EnteredAt: utc(e.EnteredAt)}
	}
	return nil
}

// saveRetention writes the contact retention a closure or cancellation started.
func saveRetention(ctx context.Context, q bentopg.Executor, order *com.CustomerOrder) error {
	r, added := order.AddedRetention()
	if !added {
		return nil
	}
	_, err := q.Exec(ctx, `INSERT INTO cargo_com.order_contact_retention (tenant_id,order_id,cause,started_at,months,retained_until,entered_at)
      VALUES ($1,$2,$3,$4,$5,$6,now())`, string(order.Tenant()), string(order.ID()), string(r.Cause), r.StartedAt, r.Months, r.RetainedUntil)
	return err
}

// Erase records the erasure of an order's contacts and clears the personal fields of every contact version
// in the same transaction (P1). The order row and its revision stay as they are.
func (s OrderStore) Erase(ctx context.Context, order *com.CustomerOrder) error {
	e, added := order.AddedErasure()
	if !added {
		return fmt.Errorf("%w: order %s records no new erasure", com.ErrInvalidTransition, order.ID())
	}
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	if _, err = q.Exec(ctx, `INSERT INTO cargo_com.order_contact_erasure (tenant_id,order_id,erased_by,erased_at,entered_at)
      VALUES ($1,$2,$3,$4,$5)`, string(order.Tenant()), string(order.ID()), e.ErasedBy, e.ErasedAt, e.EnteredAt); err != nil {
		return err
	}
	_, err = q.Exec(ctx, `UPDATE cargo_com.order_party_contact SET name=NULL,phone=NULL,mobile=NULL,email=NULL,erased=true
      WHERE tenant_id=$1 AND order_id=$2 AND NOT erased`, string(order.Tenant()), string(order.ID()))
	return err
}

// RetentionReader lists orders whose contacts are due for erasure.
type RetentionReader struct{ DB *bentopg.DB }

var _ app.RetentionReader = RetentionReader{}

// DueForContactErasure returns the orders whose contact retention ended at or before asOf and that still
// hold contacts not erased, earliest end first.
func (r RetentionReader) DueForContactErasure(ctx context.Context, tenant com.TenantID, asOf time.Time, limit int) ([]app.DueOrder, error) {
	q, err := r.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT r.order_id, r.retained_until FROM cargo_com.order_contact_retention r
      WHERE r.tenant_id=$1 AND r.retained_until <= $2
        AND NOT EXISTS (SELECT 1 FROM cargo_com.order_contact_erasure e WHERE e.tenant_id=r.tenant_id AND e.order_id=r.order_id)
        AND EXISTS (SELECT 1 FROM cargo_com.order_party_contact c WHERE c.tenant_id=r.tenant_id AND c.order_id=r.order_id AND NOT c.erased)
      ORDER BY r.retained_until, r.order_id LIMIT $3`, string(tenant), asOf, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []app.DueOrder
	for rows.Next() {
		var id string
		var until time.Time
		if err = rows.Scan(&id, &until); err != nil {
			return nil, err
		}
		out = append(out, app.DueOrder{OrderID: com.OrderID(id), RetainedUntil: until.UTC()})
	}
	return out, rows.Err()
}
