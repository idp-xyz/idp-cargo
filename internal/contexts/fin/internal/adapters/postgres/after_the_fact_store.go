package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	app "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/application"
	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// AfterTheFactStore keeps the marks of the weeks reviewed (each row is only inserted) and reads the weekly list of
// attributions (FIN V2.102, batch B).
type AfterTheFactStore struct{ DB *bentopg.DB }

var _ app.AfterTheFactStore = AfterTheFactStore{}

// Lock takes the lock of a legal entity's week until the transaction ends: reviews of one week take turns on it.
func (s AfterTheFactStore) Lock(ctx context.Context, tenant fin.TenantID, legalEntity, week string) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('cargo_fin.after_the_fact_review/' || $1::text || '/' || $2::text || '/' || $3::text, 0))`,
		string(tenant), legalEntity, week)
	return err
}

// listedLines is the list of a legal entity's week: the attributions stated by an apportionment (not carried) whose
// system time is in [from, to), listed for a reason (fin.ReviewLine.Reasons says the same in the domain): the submitter
// is the previewer, the voucher is empty, or it was made by default. A one-step apportionment names no preview and
// states no attribution (fin/000019), so the join with the preview loses nothing.
const listedLines = `SELECT att.pool_id, att.apportionment_no, att.line_no, att.shipment_id, p.consolidation_id, att.job_id, cv.currency, p.functional_currency,
    att.loss_minor, att.loss_local_minor, att.exception_minor, att.exception_local_minor, coalesce(att.party,''), att.voucher, att.by_default,
    coalesce(a.preview_no,0), a.made_by, coalesce(v.made_by,''), a.entered_at
  FROM cargo_fin.pool_apportionment_attribution att
  JOIN cargo_fin.consol_cost_pool p ON p.tenant_id = att.tenant_id AND p.pool_id = att.pool_id
  JOIN cargo_fin.pool_apportionment a ON a.tenant_id = att.tenant_id AND a.pool_id = att.pool_id AND a.apportionment_no = att.apportionment_no
  JOIN cargo_fin.pool_apportionment_line al ON al.tenant_id = att.tenant_id AND al.pool_id = att.pool_id AND al.apportionment_no = att.apportionment_no AND al.line_no = att.line_no
  JOIN cargo_fin.pool_cost_version cv ON cv.tenant_id = al.tenant_id AND cv.pool_id = al.pool_id AND cv.line_no = al.line_no AND cv.version_no = al.line_version
  LEFT JOIN cargo_fin.pool_apportionment_preview v ON v.tenant_id = a.tenant_id AND v.pool_id = a.pool_id AND v.preview_no = a.preview_no
  WHERE att.tenant_id = $1 AND p.legal_entity_id = $2 AND a.entered_at >= $3 AND a.entered_at < $4 AND NOT att.carried
    AND (v.made_by = a.made_by OR btrim(att.voucher) = '' OR att.by_default)
    AND (att.pool_id, att.apportionment_no, att.line_no, att.shipment_id) > ($5, $6, $7, $8)
  ORDER BY att.pool_id, att.apportionment_no, att.line_no, att.shipment_id
  LIMIT nullif($9::int, 0)`

func (s AfterTheFactStore) Lines(ctx context.Context, tenant fin.TenantID, legalEntity string, from, to time.Time, after fin.ReviewKey, limit int) ([]fin.ReviewLine, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, listedLines, string(tenant), legalEntity, from, to, after.PoolID, after.ApportionmentNo, after.LineNo, after.ShipmentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []fin.ReviewLine
	for rows.Next() {
		var l fin.ReviewLine
		var currency, functional string
		if err = rows.Scan(&l.PoolID, &l.ApportionmentNo, &l.LineNo, &l.ShipmentID, &l.ConsolidationID, &l.JobID, &currency, &functional, &l.Loss, &l.LossLocal,
			&l.Exception, &l.ExceptionLocal, &l.Party, &l.Voucher, &l.ByDefault, &l.PreviewNo, &l.SubmittedBy, &l.PreviewedBy, &l.EnteredAt); err != nil {
			return nil, err
		}
		l.Currency, l.FunctionalCurrency, l.EnteredAt = kernel.CurrencyCode(currency), kernel.CurrencyCode(functional), utc(l.EnteredAt)
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s AfterTheFactStore) Find(ctx context.Context, tenant fin.TenantID, legalEntity, week string) (*fin.AfterTheFactReview, bool, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, false, err
	}
	st := fin.AfterTheFactReviewState{TenantID: tenant, LegalEntityID: legalEntity, Week: week}
	err = q.QueryRow(ctx, `SELECT review_no, week_start, reviewed_by, conclusion, coalesce(note,''), list_size, list_digest, reviewed_at FROM cargo_fin.after_the_fact_review
      WHERE tenant_id = $1 AND legal_entity_id = $2 AND week = $3 ORDER BY review_no DESC LIMIT 1`, string(tenant), legalEntity, week).
		Scan(&st.ReviewNo, &st.WeekStart, &st.ReviewedBy, &st.Conclusion, &st.Note, &st.ListSize, &st.ListDigest, &st.ReviewedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	st.WeekStart, st.ReviewedAt = utc(st.WeekStart), utc(st.ReviewedAt)
	return fin.RestoreAfterTheFactReview(st), err == nil, err
}

func (s AfterTheFactStore) Insert(ctx context.Context, r *fin.AfterTheFactReview) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	st := r.State()
	var note *string
	if st.Note != "" {
		note = &st.Note
	}
	if _, err = q.Exec(ctx, `INSERT INTO cargo_fin.after_the_fact_review (tenant_id,legal_entity_id,week,review_no,week_start,reviewed_by,conclusion,note,list_size,list_digest,reviewed_at)
      VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, string(st.TenantID), st.LegalEntityID, st.Week, st.ReviewNo, st.WeekStart, st.ReviewedBy, st.Conclusion, note, st.ListSize, st.ListDigest,
		st.ReviewedAt); err != nil {
		return err
	}
	lines := r.Lines()
	if len(lines) == 0 {
		return nil
	}
	pools, apportionments, lineNos, shipments := make([]string, len(lines)), make([]int32, len(lines)), make([]int32, len(lines)), make([]string, len(lines))
	for i, l := range lines {
		pools[i], apportionments[i], lineNos[i], shipments[i] = l.PoolID, int32(l.ApportionmentNo), int32(l.LineNo), l.ShipmentID
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_fin.after_the_fact_review_line (tenant_id,legal_entity_id,week,review_no,week_start,pool_id,apportionment_no,line_no,shipment_id)
      SELECT $1,$2,$3,$4,$5,l.pool_id,l.apportionment_no,l.line_no,l.shipment_id FROM unnest($6::text[],$7::int[],$8::int[],$9::text[]) AS l(pool_id,apportionment_no,line_no,shipment_id)`,
		string(st.TenantID), st.LegalEntityID, st.Week, st.ReviewNo, st.WeekStart, pools, apportionments, lineNos, shipments)
	return err
}

// Covers answers, for each line that a review of the week covers, the latest review that does.
func (s AfterTheFactStore) Covers(ctx context.Context, tenant fin.TenantID, legalEntity, week string) (map[fin.ReviewKey]fin.ReviewCover, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (l.pool_id, l.apportionment_no, l.line_no, l.shipment_id) l.pool_id, l.apportionment_no, l.line_no, l.shipment_id,
        r.review_no, r.reviewed_by, r.conclusion, r.reviewed_at
      FROM cargo_fin.after_the_fact_review_line l
      JOIN cargo_fin.after_the_fact_review r ON r.tenant_id = l.tenant_id AND r.legal_entity_id = l.legal_entity_id AND r.week = l.week AND r.review_no = l.review_no
      WHERE l.tenant_id = $1 AND l.legal_entity_id = $2 AND l.week = $3
      ORDER BY l.pool_id, l.apportionment_no, l.line_no, l.shipment_id, r.review_no DESC`, string(tenant), legalEntity, week)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[fin.ReviewKey]fin.ReviewCover{}
	for rows.Next() {
		var k fin.ReviewKey
		var c fin.ReviewCover
		if err = rows.Scan(&k.PoolID, &k.ApportionmentNo, &k.LineNo, &k.ShipmentID, &c.ReviewNo, &c.ReviewedBy, &c.Conclusion, &c.ReviewedAt); err != nil {
			return nil, err
		}
		c.ReviewedAt = utc(c.ReviewedAt)
		out[k] = c
	}
	return out, rows.Err()
}
