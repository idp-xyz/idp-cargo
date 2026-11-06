// Package postgres persists RATE quotes, command receipts and Outbox events in the tenant's RLS scope.
package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	app "go.idp.xyz/idp-cargo/internal/contexts/rate/internal/application"
	rate "go.idp.xyz/idp-cargo/internal/contexts/rate/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// QuoteStore loads and writes quotes for commands. Load locks the quote row, so commands on one quote
// run one after another.
type QuoteStore struct{ DB *bentopg.DB }

func (s QuoteStore) Load(ctx context.Context, tenant rate.TenantID, id rate.QuoteID) (*rate.Quote, int64, error) {
	return readQuote(ctx, s.DB, tenant, id, " FOR NO KEY UPDATE")
}

// QuoteReader reads quotes for queries without locking. Lines never change once written and are
// written with their quote, so the two statements below always agree.
type QuoteReader struct{ DB *bentopg.DB }

func (r QuoteReader) Read(ctx context.Context, tenant rate.TenantID, id rate.QuoteID) (*rate.Quote, int64, error) {
	return readQuote(ctx, r.DB, tenant, id, "")
}

func readQuote(ctx context.Context, db *bentopg.DB, tenant rate.TenantID, id rate.QuoteID, lock string) (*rate.Quote, int64, error) {
	q, err := db.ReadExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	s := rate.QuoteState{QuoteDraft: rate.QuoteDraft{TenantID: tenant, QuoteID: id}}
	var entity, party, currency, status string
	var approvedBy, approvalReason, acceptedBy, evidence *string
	var approvedAt, acceptedAt *time.Time
	var acceptedRevision *int64
	var revision int64
	err = q.QueryRow(ctx, `SELECT legal_entity_id,customer_party_id,customer_party_revision,currency,valid_from,valid_until,
        margin_floor_bp,author,status,approved_by,approved_at,approval_reason_ref,
        accepted_by,accepted_at,acceptance_evidence_ref,accepted_customer_revision,revision
      FROM cargo_rate.quote WHERE tenant_id=$1 AND quote_id=$2`+lock, string(tenant), string(id)).Scan(
		&entity, &party, &s.Customer.PartyRevision, &currency, &s.Validity.From, &s.Validity.Until,
		&s.MarginFloorBP, &s.Author, &status, &approvedBy, &approvedAt, &approvalReason,
		&acceptedBy, &acceptedAt, &evidence, &acceptedRevision, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, bentorepo.ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	s.LegalEntityID, s.Customer.PartyID, s.Currency, s.Status = rate.LegalEntityID(entity), party, kernel.CurrencyCode(currency), rate.QuoteStatus(status)
	if approvedBy != nil {
		s.Approval = &rate.PriceApproval{ApprovedBy: *approvedBy, ApprovedAt: *approvedAt, ReasonRef: *approvalReason}
	}
	if acceptedAt != nil {
		s.Acceptance = &rate.Acceptance{AcceptedBy: *acceptedBy, AcceptedAt: *acceptedAt, EvidenceRef: *evidence, CustomerRevision: *acceptedRevision}
	}
	rows, err := q.Query(ctx, `SELECT charge_code,sell_minor,cost_minor FROM cargo_rate.quote_line
      WHERE tenant_id=$1 AND quote_id=$2 ORDER BY line_no`, string(tenant), string(id))
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var code string
		var sell, cost int64
		if err = rows.Scan(&code, &sell, &cost); err != nil {
			return nil, 0, err
		}
		s.Lines = append(s.Lines, rate.Line{ChargeCode: rate.ChargeCode(code), Sell: kernel.Money{Currency: s.Currency, Minor: sell}, Cost: kernel.Money{Currency: s.Currency, Minor: cost}})
	}
	if err = rows.Err(); err != nil {
		return nil, 0, err
	}
	quote, err := rate.RestoreQuote(s)
	return quote, revision, err
}

// Insert writes a new quote at revision 1 with its lines.
func (s QuoteStore) Insert(ctx context.Context, quote *rate.Quote) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := quote.State()
	var revision int64
	err = q.QueryRow(ctx, `INSERT INTO cargo_rate.quote (tenant_id,quote_id,legal_entity_id,customer_party_id,customer_party_revision,
        currency,valid_from,valid_until,margin_floor_bp,author,status)
      VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING revision`,
		string(st.TenantID), string(st.QuoteID), string(st.LegalEntityID), st.Customer.PartyID, st.Customer.PartyRevision,
		string(st.Currency), st.Validity.From, st.Validity.Until, st.MarginFloorBP, st.Author, string(st.Status)).Scan(&revision)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "quote_pkey" {
		return 0, bentorepo.ErrAlreadyExists
	}
	if err != nil {
		return 0, err
	}
	for i, l := range st.Lines {
		if _, err = q.Exec(ctx, `INSERT INTO cargo_rate.quote_line (tenant_id,quote_id,line_no,charge_code,sell_minor,cost_minor)
          VALUES ($1,$2,$3,$4,$5,$6)`, string(st.TenantID), string(st.QuoteID), i+1, string(l.ChargeCode), l.Sell.Minor, l.Cost.Minor); err != nil {
			return 0, err
		}
	}
	return revision, nil
}

// Save writes the quote's status, approval and acceptance if the stored revision is still expected.
func (s QuoteStore) Save(ctx context.Context, quote *rate.Quote, expected int64) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := quote.State()
	var approvedBy, approvalReason, acceptedBy, evidence, approvedAt, acceptedAt, acceptedRevision any
	if a := st.Approval; a != nil {
		approvedBy, approvedAt, approvalReason = a.ApprovedBy, a.ApprovedAt, a.ReasonRef
	}
	if a := st.Acceptance; a != nil {
		acceptedBy, acceptedAt, evidence, acceptedRevision = a.AcceptedBy, a.AcceptedAt, a.EvidenceRef, a.CustomerRevision
	}
	var revision int64
	err = q.QueryRow(ctx, `UPDATE cargo_rate.quote SET status=$3,approved_by=$4,approved_at=$5,approval_reason_ref=$6,
        accepted_by=$7,accepted_at=$8,acceptance_evidence_ref=$9,accepted_customer_revision=$10,
        revision=revision+1,updated_at=now()
      WHERE tenant_id=$1 AND quote_id=$2 AND revision=$11 RETURNING revision`,
		string(st.TenantID), string(st.QuoteID), string(st.Status), approvedBy, approvedAt, approvalReason,
		acceptedBy, acceptedAt, evidence, acceptedRevision, expected).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, bentorepo.ErrConflict
	}
	return revision, err
}

// CommandReceipts stores the result of every committed RATE command for replays.
type CommandReceipts struct{ DB *bentopg.DB }

func (s CommandReceipts) Find(ctx context.Context, tenant rate.TenantID, id string) (app.Receipt, bool, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return app.Receipt{}, false, err
	}
	r := app.Receipt{CommandID: id}
	var quote string
	err = q.QueryRow(ctx, `SELECT request_sha256,result_id,result_revision FROM cargo_rate.command_receipt
      WHERE tenant_id=$1 AND command_id=$2`, string(tenant), id).Scan(&r.RequestHash, &quote, &r.NewRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Receipt{}, false, nil
	}
	if err != nil {
		return app.Receipt{}, false, err
	}
	r.QuoteID = rate.QuoteID(quote)
	return r, true, nil
}

func (s CommandReceipts) Save(ctx context.Context, tenant rate.TenantID, r app.Receipt) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_rate.command_receipt (tenant_id,command_id,request_sha256,result_type,result_id,result_revision)
      VALUES ($1,$2,$3,'Quote',$4,$5)`, string(tenant), r.CommandID, r.RequestHash, string(r.QuoteID), r.NewRevision)
	return err
}
