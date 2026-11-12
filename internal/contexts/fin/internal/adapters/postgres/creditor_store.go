package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	app "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/application"
	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// CreditorStore loads and writes creditor invoices and reads the accruals they match (FIN V0.5, FB4).
type CreditorStore struct{ DB *bentopg.DB }

var _ app.CreditorInvoiceStore = CreditorStore{}

type (
	creditorLineRow struct {
		No          int     `json:"line_no"`
		ChargeCode  string  `json:"charge_code"`
		Description *string `json:"description"`
		SupplierRef *string `json:"supplier_ref"`
		Amount      int64   `json:"amount_minor"`
	}
	creditorMatchRow struct {
		No            int     `json:"match_no"`
		LineNo        int     `json:"line_no"`
		Kind          string  `json:"target_kind"`
		JobID         *string `json:"job_id"`
		PoolID        *string `json:"pool_id"`
		TargetLine    int     `json:"target_line"`
		TargetVersion int     `json:"target_version"`
		Amount        int64   `json:"amount_minor"`
		Final         bool    `json:"final"`
		Reason        *string `json:"reason"`
		Local         int64   `json:"local_minor"`
		RateEntryNo   *int    `json:"rate_entry_no"`
		RateType      *string `json:"rate_type"`
		Rate          *int64  `json:"rate"`
		Scale         *int    `json:"scale"`
		FromDigits    *int    `json:"from_digits"`
		ToDigits      *int    `json:"to_digits"`
		RateSource    *string `json:"rate_source"`
		AccrualAmount int64   `json:"accrual_amount_minor"`
		AccrualLocal  int64   `json:"accrual_local_minor"`
		PriorAmount   int64   `json:"prior_amount_minor"`
		PriorLocal    int64   `json:"prior_local_minor"`
		Variance      int64   `json:"variance_minor"`
		VarianceLocal int64   `json:"variance_local_minor"`
	}
	disputeRow struct {
		No               int        `json:"dispute_no"`
		FromState        string     `json:"from_state"`
		Reason           string     `json:"reason"`
		By               string     `json:"disputed_by"`
		At               time.Time  `json:"disputed_at"`
		EnteredAt        time.Time  `json:"entered_at"`
		ResolvedBy       *string    `json:"resolved_by"`
		ResolveReason    *string    `json:"resolve_reason"`
		ResolvedAt       *time.Time `json:"resolved_at"`
		ResolveEnteredAt *time.Time `json:"resolve_entered_at"`
	}
)

func creditorSelect(where string) string {
	key := "c.tenant_id=i.tenant_id AND c.invoice_id=i.invoice_id"
	return `SELECT i.invoice_id,i.legal_entity_id,i.creditor_party_id,i.creditor_revision,i.supplier_invoice_no,i.invoice_date::text,i.currency,
      coalesce(i.tax_document_kind,''),coalesce(i.tax_document_number,''),i.state,i.recorded_by,i.recorded_at,i.entered_at,i.matched_by,i.matched_at,
      i.match_entered_at,coalesce(i.approval_required,false),coalesce(i.threshold_no,0),i.approved_by,i.approval_reason,coalesce(i.self_approved,false),
      i.approved_at,i.approval_entered_at,i.cancel_reason,i.cancelled_by,i.cancelled_at,i.cancel_entered_at,i.revision,` +
		children("creditor_invoice_line", key, "c.line_no") + `,` + children("creditor_invoice_match", key, "c.match_no") + `,` +
		children("creditor_invoice_dispute", key, "c.dispute_no") + ` FROM cargo_fin.creditor_invoice i WHERE i.tenant_id=$1 AND ` + where
}

func scanCreditor(row pgx.Row, tenant fin.TenantID) (*fin.CreditorInvoice, int64, error) {
	st := fin.CreditorInvoiceState{TenantID: tenant}
	var currency, dated string
	var matchedBy, approvedBy, approvalReason, cancelReason, cancelledBy *string
	var matchedAt, matchEntered, approvedAt, approvalEntered, cancelledAt, cancelEntered *time.Time
	var revision int64
	var lines, matches, disputes []byte
	err := row.Scan(&st.InvoiceID, &st.LegalEntityID, &st.CreditorPartyID, &st.CreditorRevision, &st.SupplierInvoiceNo, &dated, &currency,
		&st.TaxDocumentKind, &st.TaxDocumentNumber, &st.State, &st.RecordedBy, &st.RecordedAt, &st.EnteredAt, &matchedBy, &matchedAt, &matchEntered,
		&st.ApprovalRequired, &st.ThresholdNo, &approvedBy, &approvalReason, &st.SelfApproved, &approvedAt, &approvalEntered, &cancelReason, &cancelledBy,
		&cancelledAt, &cancelEntered, &revision, &lines, &matches, &disputes)
	if err != nil {
		return nil, 0, err
	}
	var lr []creditorLineRow
	var mr []creditorMatchRow
	var dr []disputeRow
	if err = errors.Join(json.Unmarshal(lines, &lr), json.Unmarshal(matches, &mr), json.Unmarshal(disputes, &dr)); err != nil {
		return nil, 0, err
	}
	if st.InvoiceDate, err = fin.ParseDate(dated); err != nil {
		return nil, 0, err
	}
	st.Currency, st.RecordedAt, st.EnteredAt = kernel.CurrencyCode(currency), st.RecordedAt.UTC(), st.EnteredAt.UTC()
	st.MatchedBy, st.MatchedAt, st.MatchEnteredAt = deref(matchedBy), utc(deref(matchedAt)), utc(deref(matchEntered))
	st.ApprovedBy, st.ApprovalReason, st.ApprovedAt, st.ApprovalEnteredAt = deref(approvedBy), deref(approvalReason), utc(deref(approvedAt)),
		utc(deref(approvalEntered))
	st.CancelReason, st.CancelledBy, st.CancelledAt, st.CancelEnteredAt = deref(cancelReason), deref(cancelledBy), utc(deref(cancelledAt)), utc(deref(cancelEntered))
	for _, l := range lr {
		st.Lines = append(st.Lines, fin.CreditorLine{No: l.No, ChargeCode: l.ChargeCode, Description: deref(l.Description), SupplierRef: deref(l.SupplierRef),
			Amount: l.Amount})
	}
	for _, m := range mr {
		id := deref(m.JobID)
		if m.Kind == fin.TargetPoolCost {
			id = deref(m.PoolID)
		}
		st.Matches = append(st.Matches, fin.Match{No: m.No, LineNo: m.LineNo, Target: fin.TargetKey{Kind: m.Kind, ID: id, LineNo: m.TargetLine},
			TargetVersion: m.TargetVersion, Amount: m.Amount, Final: m.Final, Reason: deref(m.Reason), Local: m.Local,
			FX: fxOf(m.RateEntryNo, m.RateType, m.Rate, m.Scale, m.FromDigits, m.ToDigits, m.RateSource), AccrualAmount: m.AccrualAmount,
			AccrualLocal: m.AccrualLocal, PriorAmount: m.PriorAmount, PriorLocal: m.PriorLocal, Variance: m.Variance, VarianceLocal: m.VarianceLocal})
	}
	for _, d := range dr {
		st.Disputes = append(st.Disputes, fin.Dispute{No: d.No, FromState: d.FromState, Reason: d.Reason, By: d.By, At: utc(d.At), EnteredAt: utc(d.EnteredAt),
			ResolvedBy: deref(d.ResolvedBy), ResolveReason: deref(d.ResolveReason), ResolvedAt: utc(deref(d.ResolvedAt)),
			ResolveEnteredAt: utc(deref(d.ResolveEnteredAt))})
	}
	c, err := fin.RestoreCreditorInvoice(st)
	return c, revision, err
}

func (s CreditorStore) read(ctx context.Context, q bentopg.Querier, tenant fin.TenantID, id string) (*fin.CreditorInvoice, int64, error) {
	c, revision, err := scanCreditor(q.QueryRow(ctx, creditorSelect(`i.invoice_id=$2`), string(tenant), id), tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: creditor invoice %s", bentorepo.ErrNotFound, id)
	}
	return c, revision, err
}

// Load locks the invoice row and reads the invoice.
func (s CreditorStore) Load(ctx context.Context, tenant fin.TenantID, id string) (*fin.CreditorInvoice, int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	var one int
	err = q.QueryRow(ctx, `SELECT 1 FROM cargo_fin.creditor_invoice WHERE tenant_id=$1 AND invoice_id=$2 FOR NO KEY UPDATE`, string(tenant), id).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: creditor invoice %s", bentorepo.ErrNotFound, id)
	}
	if err != nil {
		return nil, 0, err
	}
	return s.read(ctx, q, tenant, id)
}

func (s CreditorStore) Read(ctx context.Context, tenant fin.TenantID, id string) (*fin.CreditorInvoice, int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	return s.read(ctx, q, tenant, id)
}

// List lists a legal entity's creditor invoices, of one creditor when creditor is not empty, in the order dated.
func (s CreditorStore) List(ctx context.Context, tenant fin.TenantID, legalEntity, creditor string) ([]*fin.CreditorInvoice, []int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, nil, err
	}
	rows, err := q.Query(ctx, creditorSelect(`i.legal_entity_id=$2 AND ($3::text = '' OR i.creditor_party_id=$3)
      ORDER BY i.invoice_date, i.recorded_at, i.invoice_id`), string(tenant), legalEntity, creditor)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []*fin.CreditorInvoice
	var revisions []int64
	for rows.Next() {
		c, revision, err := scanCreditor(rows, tenant)
		if err != nil {
			return nil, nil, err
		}
		out, revisions = append(out, c), append(revisions, revision)
	}
	return out, revisions, rows.Err()
}

func (s CreditorStore) Insert(ctx context.Context, c *fin.CreditorInvoice) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := c.State()
	var revision int64
	err = q.QueryRow(ctx, `INSERT INTO cargo_fin.creditor_invoice (tenant_id,invoice_id,legal_entity_id,creditor_party_id,creditor_revision,
      supplier_invoice_no,invoice_date,currency,tax_document_kind,tax_document_number,total_minor,state,recorded_by,recorded_at,entered_at)
      VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) RETURNING revision`, string(st.TenantID), st.InvoiceID, st.LegalEntityID,
		st.CreditorPartyID, st.CreditorRevision, st.SupplierInvoiceNo, st.InvoiceDate.Format(time.DateOnly), string(st.Currency), orNull(st.TaxDocumentKind),
		orNull(st.TaxDocumentNumber), st.Total(), st.State, st.RecordedBy, st.RecordedAt, st.EnteredAt).Scan(&revision)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		if pgErr.ConstraintName == "creditor_invoice_number_once" {
			return 0, fmt.Errorf("%w: %s of %s", fin.ErrInvoiceNumber, st.SupplierInvoiceNo, st.CreditorPartyID)
		}
		return 0, fmt.Errorf("%w: creditor invoice %s", bentorepo.ErrAlreadyExists, st.InvoiceID)
	}
	if err != nil {
		return 0, err
	}
	for _, l := range st.Lines {
		if _, err = q.Exec(ctx, `INSERT INTO cargo_fin.creditor_invoice_line (tenant_id,invoice_id,line_no,charge_code,description,supplier_ref,amount_minor)
            VALUES ($1,$2,$3,$4,$5,$6,$7)`, string(st.TenantID), st.InvoiceID, l.No, l.ChargeCode, orNull(l.Description), orNull(l.SupplierRef),
			l.Amount); err != nil {
			return 0, err
		}
	}
	return revision, writeCreditor(ctx, q, c)
}

func (s CreditorStore) Save(ctx context.Context, c *fin.CreditorInvoice, expected int64) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := c.State()
	var threshold any
	if st.ThresholdNo > 0 {
		threshold = st.ThresholdNo
	}
	var required, self any
	if !st.MatchedAt.IsZero() {
		required = st.ApprovalRequired
	}
	if !st.ApprovedAt.IsZero() {
		self = st.SelfApproved
	}
	var revision int64
	err = q.QueryRow(ctx, `UPDATE cargo_fin.creditor_invoice SET state=$3,matched_by=$4,matched_at=$5,match_entered_at=$6,approval_required=$7,
      threshold_no=$8,approved_by=$9,approval_reason=$10,self_approved=$11,approved_at=$12,approval_entered_at=$13,cancel_reason=$14,cancelled_by=$15,
      cancelled_at=$16,cancel_entered_at=$17,revision=revision+1,updated_at=now() WHERE tenant_id=$1 AND invoice_id=$2 AND revision=$18 RETURNING revision`,
		string(st.TenantID), st.InvoiceID, st.State, orNull(st.MatchedBy), orNull(st.MatchedAt), orNull(st.MatchEnteredAt), required, threshold,
		orNull(st.ApprovedBy), orNull(st.ApprovalReason), self, orNull(st.ApprovedAt), orNull(st.ApprovalEnteredAt), orNull(st.CancelReason),
		orNull(st.CancelledBy), orNull(st.CancelledAt), orNull(st.CancelEnteredAt), expected).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: creditor invoice %s", bentorepo.ErrConflict, st.InvoiceID)
	}
	if err != nil {
		return 0, err
	}
	return revision, writeCreditor(ctx, q, c)
}

func writeCreditor(ctx context.Context, q bentopg.Executor, c *fin.CreditorInvoice) error {
	st := c.State()
	matches, disputes, resolved := c.Added()
	exec := func(sql string, args ...any) error {
		_, err := q.Exec(ctx, sql, append([]any{string(st.TenantID), st.InvoiceID}, args...)...)
		return err
	}
	for _, m := range matches {
		var job, side, pool any
		if m.Target.Kind == fin.TargetJobCost {
			job, side = m.Target.ID, fin.SideCost
		} else {
			pool = m.Target.ID
		}
		args := append([]any{m.No, m.LineNo, m.Target.Kind, job, side, pool, m.Target.LineNo, m.TargetVersion, m.Amount, m.Final, orNull(m.Reason), m.Local},
			fxArgs(m.FX)...)
		args = append(args, m.AccrualAmount, m.AccrualLocal, m.PriorAmount, m.PriorLocal, m.Variance, m.VarianceLocal)
		if err := exec(`INSERT INTO cargo_fin.creditor_invoice_match (tenant_id,invoice_id,match_no,line_no,target_kind,job_id,cost_side,pool_id,target_line,
            target_version,amount_minor,final,reason,local_minor,rate_entry_no,rate_type,rate,scale,from_digits,to_digits,rate_source,accrual_amount_minor,
            accrual_local_minor,prior_amount_minor,prior_local_minor,variance_minor,variance_local_minor) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,
            $14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27)`, args...); err != nil {
			return err
		}
	}
	for _, d := range resolved {
		if err := exec(`UPDATE cargo_fin.creditor_invoice_dispute SET resolved_by=$4,resolve_reason=$5,resolved_at=$6,resolve_entered_at=$7
            WHERE tenant_id=$1 AND invoice_id=$2 AND dispute_no=$3`, d.No, d.ResolvedBy, d.ResolveReason, d.ResolvedAt, d.ResolveEnteredAt); err != nil {
			return err
		}
	}
	for _, d := range disputes {
		if err := exec(`INSERT INTO cargo_fin.creditor_invoice_dispute (tenant_id,invoice_id,dispute_no,from_state,reason,disputed_by,disputed_at,entered_at,
            resolved_by,resolve_reason,resolved_at,resolve_entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, d.No, d.FromState, d.Reason, d.By, d.At,
			d.EnteredAt, orNull(d.ResolvedBy), orNull(d.ResolveReason), orNull(d.ResolvedAt), orNull(d.ResolveEnteredAt)); err != nil {
			return err
		}
	}
	return nil
}

// accrualSelect reads accruals of jobs and pools with their owner, latest version and what live matches consume.
const accrualSelect = `SELECT x.kind, x.id, x.line_no, x.legal_entity_id, x.open, x.charge_code, x.supplier_ref, x.reference, v.version_no, v.status,
    v.currency, coalesce(v.creditor,''), v.amount, v.local, c.amount, c.local, c.final, c.held
  FROM (SELECT 'JOB_COST' AS kind, j.tenant_id, j.job_id AS id, l.line_no, j.legal_entity_id, j.state = 'OPEN' AS open, l.charge_code,
          '' AS supplier_ref, j.shipment_id AS reference
        FROM cargo_fin.freight_job j JOIN cargo_fin.charge_line l ON l.tenant_id = j.tenant_id AND l.job_id = j.job_id
        WHERE j.tenant_id = $1 AND %[1]s
        UNION ALL
        SELECT 'POOL_COST', p.tenant_id, p.pool_id, l.line_no, p.legal_entity_id, p.state = 'OPEN', l.charge_code, coalesce(l.supplier_ref, ''),
          p.consolidation_id
        FROM cargo_fin.consol_cost_pool p JOIN cargo_fin.pool_cost_line l ON l.tenant_id = p.tenant_id AND l.pool_id = p.pool_id
        WHERE p.tenant_id = $1 AND %[2]s) x
  CROSS JOIN LATERAL cargo_fin.accrual_latest(x.tenant_id, x.kind, x.id, x.line_no) v
  CROSS JOIN LATERAL cargo_fin.accrual_consumed(x.tenant_id, x.kind, x.id, x.line_no) c
  WHERE v.version_no IS NOT NULL AND %[3]s
  ORDER BY x.kind, x.id, x.line_no`

func scanAccruals(rows pgx.Rows) ([]fin.AccrualTarget, error) {
	defer rows.Close()
	var out []fin.AccrualTarget
	for rows.Next() {
		var t fin.AccrualTarget
		var currency string
		if err := rows.Scan(&t.Kind, &t.ID, &t.LineNo, &t.LegalEntityID, &t.Open, &t.ChargeCode, &t.SupplierRef, &t.Reference, &t.Version, &t.Status,
			&currency, &t.CreditorPartyID, &t.Amount.Minor, &t.Local, &t.Consumed.Amount, &t.Consumed.Local, &t.Consumed.Final, &t.Consumed.Held); err != nil {
			return nil, err
		}
		t.Amount.Currency = kernel.CurrencyCode(currency)
		out = append(out, t)
	}
	return out, rows.Err()
}

// Targets locks the pools, then the jobs, of the accruals named, each in id order (FOR NO KEY UPDATE), until the
// transaction ends, then reads them: creditor invoices matching, disputing or cancelling, and the pools' and jobs' own
// commands, take turns. Accruals FIN does not know are left out.
func (s CreditorStore) Targets(ctx context.Context, tenant fin.TenantID, keys []fin.TargetKey) ([]fin.AccrualTarget, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, err
	}
	var jobs, pools, jobKeys, poolKeys []string
	var jobLines, poolLines []int32
	for _, k := range keys {
		switch k.Kind {
		case fin.TargetJobCost:
			jobs, jobKeys, jobLines = append(jobs, k.ID), append(jobKeys, k.ID), append(jobLines, int32(k.LineNo))
		case fin.TargetPoolCost:
			pools, poolKeys, poolLines = append(pools, k.ID), append(poolKeys, k.ID), append(poolLines, int32(k.LineNo))
		}
	}
	slices.Sort(jobs)
	slices.Sort(pools)
	for _, lock := range []struct {
		sql string
		ids []string
	}{{`SELECT 1 FROM cargo_fin.consol_cost_pool WHERE tenant_id=$1 AND pool_id = ANY($2) ORDER BY pool_id FOR NO KEY UPDATE`, slices.Compact(pools)},
		{`SELECT 1 FROM cargo_fin.freight_job WHERE tenant_id=$1 AND job_id = ANY($2) ORDER BY job_id FOR NO KEY UPDATE`, slices.Compact(jobs)}} {
		if len(lock.ids) == 0 {
			continue
		}
		rows, err := q.Query(ctx, lock.sql, string(tenant), lock.ids)
		if err != nil {
			return nil, err
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return nil, err
		}
	}
	if len(keys) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, fmt.Sprintf(accrualSelect,
		`(j.job_id, l.line_no) IN (SELECT * FROM unnest($2::text[], $3::int[]))`,
		`(p.pool_id, l.line_no) IN (SELECT * FROM unnest($4::text[], $5::int[]))`, `true`),
		string(tenant), jobKeys, jobLines, poolKeys, poolLines)
	if err != nil {
		return nil, err
	}
	return scanAccruals(rows)
}

// Consumed reads what the live matches of creditor invoices not cancelled consume of a job's cost sides or a pool's
// cost lines, by line.
func (s CreditorStore) Consumed(ctx context.Context, tenant fin.TenantID, kind, id string) (map[int]fin.Consumption, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT m.target_line, sum(m.amount_minor)::bigint, sum(m.local_minor)::bigint, bool_or(m.final),
        bool_or(i.state = 'DISPUTED' OR (i.approval_required AND i.approved_at IS NULL)), array_agg(DISTINCT i.creditor_party_id ORDER BY i.creditor_party_id)
      FROM cargo_fin.creditor_invoice_match m JOIN cargo_fin.creditor_invoice i ON i.tenant_id = m.tenant_id AND i.invoice_id = m.invoice_id
      WHERE m.tenant_id = $1 AND m.target_kind = $2 AND coalesce(m.job_id, m.pool_id) = $3 AND i.state <> 'CANCELLED'
      GROUP BY m.target_line`, string(tenant), kind, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]fin.Consumption{}
	for rows.Next() {
		var line int
		var c fin.Consumption
		if err := rows.Scan(&line, &c.Amount, &c.Local, &c.Final, &c.Held, &c.Creditors); err != nil {
			return nil, err
		}
		out[line] = c
	}
	return out, rows.Err()
}

// LatestMatch answers when an invoice of the legal entity was last matched (zero when none was).
func (s CreditorStore) LatestMatch(ctx context.Context, tenant fin.TenantID, legalEntity string) (time.Time, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return time.Time{}, err
	}
	var latest *time.Time
	err = q.QueryRow(ctx, `SELECT max(matched_at) FROM cargo_fin.creditor_invoice WHERE tenant_id = $1 AND legal_entity_id = $2`, string(tenant),
		legalEntity).Scan(&latest)
	return utc(deref(latest)), err
}

// OpenAccruals lists the accruals of a legal entity's open jobs and pools still expecting an invoice (ACCRUED or
// PARTLY_MATCHED), naming the creditor or none when creditor is not empty.
func (s CreditorStore) OpenAccruals(ctx context.Context, tenant fin.TenantID, legalEntity, creditor string) ([]fin.AccrualTarget, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, fmt.Sprintf(accrualSelect, `j.legal_entity_id = $2 AND j.state = 'OPEN'`, `p.legal_entity_id = $2 AND p.state = 'OPEN'`,
		`v.status = 'ACCRUED' AND NOT c.final AND ($3::text = '' OR v.creditor IS NULL OR v.creditor = $3)`), string(tenant), legalEntity, creditor)
	if err != nil {
		return nil, err
	}
	return scanAccruals(rows)
}
