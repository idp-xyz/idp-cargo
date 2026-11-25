package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	app "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/application"
	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// CostReviewStore keeps cost review requests and their resolutions; each row is only inserted.
type CostReviewStore struct{ DB *bentopg.DB }

var _ app.CostReviewStore = CostReviewStore{}

// Next takes the tenant's numbering lock until the transaction ends and answers the next number. The request's own
// insert takes the same lock (fin/000020), so the number holds.
func (s CostReviewStore) Next(ctx context.Context, tenant fin.TenantID) (int, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	if _, err = q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('cargo_fin.cost_review_numbers/' || $1::text, 0))`, string(tenant)); err != nil {
		return 0, err
	}
	var last int
	err = q.QueryRow(ctx, `SELECT coalesce(max(request_no),0) FROM cargo_fin.cost_review_request WHERE tenant_id=$1`, string(tenant)).Scan(&last)
	return last + 1, err
}

func (s CostReviewStore) OpenOf(ctx context.Context, tenant fin.TenantID, job string) (string, bool, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return "", false, err
	}
	var id string
	err = q.QueryRow(ctx, `SELECT r.request_id FROM cargo_fin.cost_review_request r WHERE r.tenant_id=$1 AND r.job_id=$2
      AND NOT EXISTS (SELECT 1 FROM cargo_fin.cost_review_resolution x WHERE x.tenant_id=r.tenant_id AND x.request_no=r.request_no)
      ORDER BY r.request_no LIMIT 1`, string(tenant), job).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

func (s CostReviewStore) Insert(ctx context.Context, r *fin.CostReview) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	st := r.State()
	_, err = q.Exec(ctx, `INSERT INTO cargo_fin.cost_review_request (tenant_id,request_no,request_id,job_id,shipment_id,legal_entity_id,requested_by,reason,
        currency,revenue_minor,cost_minor,profit_minor,profit_basis,settings_version,profit_version,requested_at)
      VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`, string(st.TenantID), st.No, st.RequestID, st.JobID, st.ShipmentID, st.LegalEntityID,
		st.RequestedBy, st.Reason, string(st.Currency), st.Revenue, st.Cost, st.Profit, st.ProfitBasis, st.SettingsVersion, st.ProfitVersion, st.RequestedAt)
	return err
}

func (s CostReviewStore) InsertResolution(ctx context.Context, r *fin.CostReview) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	st := r.State()
	if st.Resolution == nil {
		return fmt.Errorf("%w: the request is not resolved", fin.ErrInvalid)
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_fin.cost_review_resolution (tenant_id,request_no,requested_by,requested_at,resolved_by,outcome,note,resolved_at)
      VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, string(st.TenantID), st.No, st.RequestedBy, st.RequestedAt, st.Resolution.By, st.Resolution.Outcome, st.Resolution.Note, st.Resolution.At)
	return err
}

const reviewColumns = `r.request_no,r.request_id,r.job_id,r.shipment_id,r.legal_entity_id,r.requested_by,r.reason,r.currency,r.revenue_minor,r.cost_minor,
  r.profit_minor,r.profit_basis,r.settings_version,r.profit_version,r.requested_at,x.resolved_by,x.outcome,x.note,x.resolved_at`

func scanReview(tenant fin.TenantID, row pgx.Row) (*fin.CostReview, error) {
	st := fin.CostReviewState{TenantID: tenant}
	var currency string
	var by, outcome, note *string
	var at *time.Time
	if err := row.Scan(&st.No, &st.RequestID, &st.JobID, &st.ShipmentID, &st.LegalEntityID, &st.RequestedBy, &st.Reason, &currency, &st.Revenue, &st.Cost,
		&st.Profit, &st.ProfitBasis, &st.SettingsVersion, &st.ProfitVersion, &st.RequestedAt, &by, &outcome, &note, &at); err != nil {
		return nil, err
	}
	st.Currency, st.RequestedAt = kernel.CurrencyCode(currency), utc(st.RequestedAt)
	if by != nil {
		st.Resolution = &fin.CostReviewResolution{By: *by, Outcome: *outcome, Note: *note, At: utc(*at)}
	}
	return fin.RestoreCostReview(st), nil
}

const reviewFrom = ` FROM cargo_fin.cost_review_request r LEFT JOIN cargo_fin.cost_review_resolution x ON x.tenant_id=r.tenant_id AND x.request_no=r.request_no `

func (s CostReviewStore) Read(ctx context.Context, tenant fin.TenantID, id string) (*fin.CostReview, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	r, err := scanReview(tenant, q.QueryRow(ctx, `SELECT `+reviewColumns+reviewFrom+`WHERE r.tenant_id=$1 AND r.request_id=$2`, string(tenant), id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: cost review request %s", bentorepo.ErrNotFound, id)
	}
	return r, err
}

func (s CostReviewStore) List(ctx context.Context, tenant fin.TenantID, f app.CostReviewFilter) ([]*fin.CostReview, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT `+reviewColumns+reviewFrom+`WHERE r.tenant_id=$1 AND ($2::text='' OR r.job_id=$2) AND r.request_no > $3
      AND ($4::boolean IS NULL OR (x.request_no IS NOT NULL) = $4) ORDER BY r.request_no LIMIT $5`, string(tenant), f.JobID, f.After, f.Resolved, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*fin.CostReview
	for rows.Next() {
		r, err := scanReview(tenant, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
