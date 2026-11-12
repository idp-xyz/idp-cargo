package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	app "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/application"
	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// PaymentStore loads and writes creditor payments and reads what they pay (FIN V0.6).
type PaymentStore struct{ DB *bentopg.DB }

var _ app.CreditorPaymentStore = PaymentStore{}

func paymentSelect(where string) string {
	return `SELECT p.payment_id,p.legal_entity_id,p.creditor_party_id,p.creditor_revision,p.currency,p.amount_minor,p.paid_on::text,p.evidence_source,
      p.evidence_ref,p.state,p.recorded_by,p.recorded_at,p.entered_at,p.cancel_reason,p.cancelled_by,p.cancelled_on::text,p.cancelled_at,p.cancel_entered_at,
      p.revision,` + children("creditor_payment_allocation", "c.tenant_id=p.tenant_id AND c.payment_id=p.payment_id", "c.allocation_no") +
		` FROM cargo_fin.creditor_payment p WHERE p.tenant_id=$1 AND ` + where
}

func scanPayment(row pgx.Row, tenant fin.TenantID) (*fin.CreditorPayment, int64, error) {
	st := fin.PaymentState{TenantID: tenant}
	var currency, paid string
	var cancelReason, cancelledBy, cancelledOn *string
	var cancelledAt, cancelEntered *time.Time
	var revision int64
	var allocations []byte
	err := row.Scan(&st.PaymentID, &st.LegalEntityID, &st.CreditorPartyID, &st.CreditorRevision, &currency, &st.Amount, &paid, &st.EvidenceSource,
		&st.EvidenceRef, &st.State, &st.RecordedBy, &st.RecordedAt, &st.EnteredAt, &cancelReason, &cancelledBy, &cancelledOn, &cancelledAt, &cancelEntered,
		&revision, &allocations)
	if err != nil {
		return nil, 0, err
	}
	var ar []allocationRow
	if err = json.Unmarshal(allocations, &ar); err != nil {
		return nil, 0, err
	}
	if st.PaidOn, err = fin.ParseDate(paid); err != nil {
		return nil, 0, err
	}
	if st.CancelledOn, err = optionalDate(cancelledOn); err != nil {
		return nil, 0, err
	}
	st.Currency, st.RecordedAt, st.EnteredAt = kernel.CurrencyCode(currency), st.RecordedAt.UTC(), st.EnteredAt.UTC()
	st.CancelReason, st.CancelledBy, st.CancelledAt, st.CancelEnteredAt = deref(cancelReason), deref(cancelledBy), utc(deref(cancelledAt)), utc(deref(cancelEntered))
	for _, a := range ar {
		on, err := fin.ParseDate(a.Date)
		if err != nil {
			return nil, 0, err
		}
		reversedOn, err := optionalDate(a.ReversedOn)
		if err != nil {
			return nil, 0, err
		}
		st.Allocations = append(st.Allocations, fin.Allocation{No: a.No, InvoiceID: a.InvoiceID, Amount: a.Amount, Date: on, By: a.By, At: utc(a.At),
			EnteredAt: utc(a.EnteredAt), ReversedBy: deref(a.ReversedBy), ReverseReason: deref(a.ReverseReason), ReversedOn: reversedOn,
			ReversedAt: utc(deref(a.ReversedAt)), ReverseEnteredAt: utc(deref(a.ReverseEnteredAt))})
	}
	p, err := fin.RestorePayment(st)
	return p, revision, err
}

// Load locks the payment row and reads the payment.
func (s PaymentStore) Load(ctx context.Context, tenant fin.TenantID, id string) (*fin.CreditorPayment, int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	var one int
	err = q.QueryRow(ctx, `SELECT 1 FROM cargo_fin.creditor_payment WHERE tenant_id=$1 AND payment_id=$2 FOR NO KEY UPDATE`, string(tenant), id).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: creditor payment %s", bentorepo.ErrNotFound, id)
	}
	if err != nil {
		return nil, 0, err
	}
	return scanPayment(q.QueryRow(ctx, paymentSelect(`p.payment_id=$2`), string(tenant), id), tenant)
}

func (s PaymentStore) Read(ctx context.Context, tenant fin.TenantID, id string) (*fin.CreditorPayment, int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	p, revision, err := scanPayment(q.QueryRow(ctx, paymentSelect(`p.payment_id=$2`), string(tenant), id), tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: creditor payment %s", bentorepo.ErrNotFound, id)
	}
	return p, revision, err
}

// OfCreditor lists a creditor's payments by a legal entity, in the order paid.
func (s PaymentStore) OfCreditor(ctx context.Context, tenant fin.TenantID, le, creditor string) ([]*fin.CreditorPayment, []int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, nil, err
	}
	rows, err := q.Query(ctx, paymentSelect(`p.legal_entity_id=$2 AND p.creditor_party_id=$3 ORDER BY p.paid_on, p.recorded_at, p.payment_id`), string(tenant),
		le, creditor)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []*fin.CreditorPayment
	var revisions []int64
	for rows.Next() {
		p, revision, err := scanPayment(rows, tenant)
		if err != nil {
			return nil, nil, err
		}
		out, revisions = append(out, p), append(revisions, revision)
	}
	return out, revisions, rows.Err()
}

// OnAccount sums what the recorded payments to a creditor have unallocated, per currency.
func (s PaymentStore) OnAccount(ctx context.Context, tenant fin.TenantID, le, creditor string) ([]kernel.Money, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT p.currency, sum(p.amount_minor - coalesce((SELECT sum(a.amount_minor) FROM cargo_fin.creditor_payment_allocation a
        WHERE a.tenant_id=p.tenant_id AND a.payment_id=p.payment_id AND a.reversed_at IS NULL), 0))::bigint
      FROM cargo_fin.creditor_payment p WHERE p.tenant_id=$1 AND p.legal_entity_id=$2 AND p.creditor_party_id=$3 AND p.state='RECORDED'
      GROUP BY p.currency ORDER BY p.currency`, string(tenant), le, creditor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []kernel.Money
	for rows.Next() {
		var currency string
		var amount int64
		if err = rows.Scan(&currency, &amount); err != nil {
			return nil, err
		}
		if amount != 0 {
			out = append(out, kernel.Money{Currency: kernel.CurrencyCode(currency), Minor: amount})
		}
	}
	return out, rows.Err()
}

// PayableInvoices locks the creditor invoices in order of id until the transaction ends, then reads what an
// allocation needs of each: invoice commands and allocations to an invoice take turns.
func (s PaymentStore) PayableInvoices(ctx context.Context, tenant fin.TenantID, invoiceIDs []string) ([]fin.PayableInvoice, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, err
	}
	locked, err := q.Query(ctx, `SELECT 1 FROM cargo_fin.creditor_invoice WHERE tenant_id=$1 AND invoice_id = ANY($2) ORDER BY invoice_id FOR NO KEY UPDATE`,
		string(tenant), invoiceIDs)
	if err != nil {
		return nil, err
	}
	locked.Close()
	if err = locked.Err(); err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT i.invoice_id, i.supplier_invoice_no, i.legal_entity_id, i.creditor_party_id, i.currency, i.invoice_date::text, i.state,
        coalesce(i.approval_required AND i.approved_at IS NULL, false), i.total_minor,
        coalesce((SELECT sum(a.amount_minor) FROM cargo_fin.creditor_payment_allocation a WHERE a.tenant_id=i.tenant_id AND a.invoice_id=i.invoice_id
          AND a.reversed_at IS NULL), 0)::bigint
      FROM cargo_fin.creditor_invoice i WHERE i.tenant_id=$1 AND i.invoice_id = ANY($2) ORDER BY i.invoice_id`, string(tenant), invoiceIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []fin.PayableInvoice
	for rows.Next() {
		var n fin.PayableInvoice
		var currency, dated string
		if err = rows.Scan(&n.InvoiceID, &n.SupplierInvoiceNo, &n.LegalEntityID, &n.CreditorPartyID, &currency, &dated, &n.State, &n.AwaitingApproval,
			&n.Total, &n.Paid); err != nil {
			return nil, err
		}
		if n.InvoiceDate, err = fin.ParseDate(dated); err != nil {
			return nil, err
		}
		n.Currency = kernel.CurrencyCode(currency)
		out = append(out, n)
	}
	return out, rows.Err()
}

// Paid lists the live allocations to a creditor invoice, in the order of their dates.
func (s PaymentStore) Paid(ctx context.Context, tenant fin.TenantID, invoiceID string) ([]app.PaidAllocation, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT a.payment_id, a.allocation_no, a.amount_minor, a.allocated_on::text FROM cargo_fin.creditor_payment_allocation a
      WHERE a.tenant_id=$1 AND a.invoice_id=$2 AND a.reversed_at IS NULL ORDER BY a.allocated_on, a.allocated_at, a.payment_id, a.allocation_no`,
		string(tenant), invoiceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []app.PaidAllocation
	for rows.Next() {
		var x app.PaidAllocation
		var on string
		if err = rows.Scan(&x.PaymentID, &x.AllocationNo, &x.Amount, &on); err != nil {
			return nil, err
		}
		if x.Date, err = fin.ParseDate(on); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s PaymentStore) Insert(ctx context.Context, p *fin.CreditorPayment) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := p.State()
	var revision int64
	err = q.QueryRow(ctx, `INSERT INTO cargo_fin.creditor_payment (tenant_id,payment_id,legal_entity_id,creditor_party_id,creditor_revision,currency,
      amount_minor,paid_on,evidence_source,evidence_ref,state,recorded_by,recorded_at,entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
      RETURNING revision`, string(st.TenantID), st.PaymentID, st.LegalEntityID, st.CreditorPartyID, st.CreditorRevision, string(st.Currency), st.Amount,
		st.PaidOn.Format(time.DateOnly), st.EvidenceSource, st.EvidenceRef, st.State, st.RecordedBy, st.RecordedAt, st.EnteredAt).Scan(&revision)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		if pgErr.ConstraintName == "creditor_payment_evidence_once" {
			return 0, fmt.Errorf("%w: %s %s", fin.ErrEvidenceUsed, st.EvidenceSource, st.EvidenceRef)
		}
		return 0, fmt.Errorf("%w: creditor payment %s", bentorepo.ErrAlreadyExists, st.PaymentID)
	}
	if err != nil {
		return 0, err
	}
	return revision, writePaymentAllocations(ctx, q, p)
}

func (s PaymentStore) Save(ctx context.Context, p *fin.CreditorPayment, expected int64) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := p.State()
	var revision int64
	err = q.QueryRow(ctx, `UPDATE cargo_fin.creditor_payment SET state=$3,cancel_reason=$4,cancelled_by=$5,cancelled_on=$6,cancelled_at=$7,cancel_entered_at=$8,
      revision=revision+1,updated_at=now() WHERE tenant_id=$1 AND payment_id=$2 AND revision=$9 RETURNING revision`, string(st.TenantID), st.PaymentID,
		st.State, orNull(st.CancelReason), orNull(st.CancelledBy), dateOrNull(st.CancelledOn), orNull(st.CancelledAt), orNull(st.CancelEnteredAt),
		expected).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: creditor payment %s", bentorepo.ErrConflict, st.PaymentID)
	}
	if err != nil {
		return 0, err
	}
	return revision, writePaymentAllocations(ctx, q, p)
}

func writePaymentAllocations(ctx context.Context, q bentopg.Executor, p *fin.CreditorPayment) error {
	st := p.State()
	added, reversed := p.Added()
	for _, a := range reversed {
		if _, err := q.Exec(ctx, `UPDATE cargo_fin.creditor_payment_allocation SET reversed_by=$4,reverse_reason=$5,reversed_on=$6,reversed_at=$7,
            reverse_entered_at=$8 WHERE tenant_id=$1 AND payment_id=$2 AND allocation_no=$3`, string(st.TenantID), st.PaymentID, a.No, a.ReversedBy,
			a.ReverseReason, a.ReversedOn.Format(time.DateOnly), a.ReversedAt, a.ReverseEnteredAt); err != nil {
			return err
		}
	}
	for _, a := range added {
		if _, err := q.Exec(ctx, `INSERT INTO cargo_fin.creditor_payment_allocation (tenant_id,payment_id,allocation_no,invoice_id,amount_minor,allocated_on,
            allocated_by,allocated_at,entered_at,reversed_by,reverse_reason,reversed_on,reversed_at,reverse_entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,
            $11,$12,$13,$14)`, string(st.TenantID), st.PaymentID, a.No, a.InvoiceID, a.Amount, a.Date.Format(time.DateOnly), a.By, a.At, a.EnteredAt,
			orNull(a.ReversedBy), orNull(a.ReverseReason), dateOrNull(a.ReversedOn), orNull(a.ReversedAt), orNull(a.ReverseEnteredAt)); err != nil {
			return err
		}
	}
	return nil
}
