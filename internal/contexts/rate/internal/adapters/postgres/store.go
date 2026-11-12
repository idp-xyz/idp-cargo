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
	var entity, party, currency, status, recipient string
	var approvedBy, approvalReason, acceptedBy, evidence, evidenceKind, rule, version, place, service *string
	var approvedAt, acceptedAt, acceptedRecordedAt, expiredAt *time.Time
	var acceptedRevision *int64
	var selfApproved, unlisted bool
	var revision int64
	err = q.QueryRow(ctx, `SELECT legal_entity_id,customer_party_id,customer_party_revision,recipient_role,currency,valid_from,valid_until,
        margin_floor_bp,author,status,approved_by,approved_at,approval_reason_ref,self_approved,
        accepted_by,accepted_at,accepted_recorded_at,acceptance_evidence_kind,acceptance_evidence_ref,accepted_customer_revision,
        expired_at,incoterms_rule,incoterms_version,named_place,service_level,service_level_unlisted,revision
      FROM cargo_rate.quote WHERE tenant_id=$1 AND quote_id=$2`+lock, string(tenant), string(id)).Scan(
		&entity, &party, &s.Customer.PartyRevision, &recipient, &currency, &s.Validity.From, &s.Validity.Until,
		&s.MarginFloorBP, &s.Author, &status, &approvedBy, &approvedAt, &approvalReason, &selfApproved,
		&acceptedBy, &acceptedAt, &acceptedRecordedAt, &evidenceKind, &evidence, &acceptedRevision,
		&expiredAt, &rule, &version, &place, &service, &unlisted, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, bentorepo.ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	s.LegalEntityID, s.Customer.PartyID, s.Currency, s.Status = rate.LegalEntityID(entity), party, kernel.CurrencyCode(currency), rate.QuoteStatus(status)
	s.Recipient = rate.RecipientRole(recipient)
	s.Terms = rate.CommercialTerms{IncotermsRule: deref(rule), IncotermsVersion: deref(version), NamedPlace: deref(place), ServiceLevel: deref(service),
		ServiceLevelUnlisted: unlisted}
	if approvedBy != nil {
		s.Approval = &rate.PriceApproval{ApprovedBy: *approvedBy, ApprovedAt: *approvedAt, ReasonRef: *approvalReason, SelfApproved: selfApproved}
	}
	if acceptedAt != nil {
		s.Acceptance = &rate.Acceptance{AcceptedBy: *acceptedBy, AcceptedAt: *acceptedAt, EvidenceKind: rate.EvidenceKind(deref(evidenceKind)),
			EvidenceRef: *evidence, CustomerRevision: *acceptedRevision}
		if acceptedRecordedAt != nil {
			s.Acceptance.RecordedAt = *acceptedRecordedAt
		}
	}
	if expiredAt != nil {
		s.ExpiredAt = *expiredAt
	}
	rows, err := q.Query(ctx, `SELECT charge_code,sell_currency,sell_minor,cost_currency,cost_minor FROM cargo_rate.quote_line
      WHERE tenant_id=$1 AND quote_id=$2 ORDER BY line_no`, string(tenant), string(id))
	if err != nil {
		return nil, 0, err
	}
	for rows.Next() {
		var code, sellCurrency, costCurrency string
		var sell, cost int64
		if err = rows.Scan(&code, &sellCurrency, &sell, &costCurrency, &cost); err != nil {
			rows.Close()
			return nil, 0, err
		}
		s.Lines = append(s.Lines, rate.Line{ChargeCode: rate.ChargeCode(code), Sell: kernel.Money{Currency: kernel.CurrencyCode(sellCurrency), Minor: sell},
			Cost: kernel.Money{Currency: kernel.CurrencyCode(costCurrency), Minor: cost}})
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, 0, err
	}
	rates, err := q.Query(ctx, `SELECT from_currency,rate,scale,from_digits,quote_digits,source,as_of FROM cargo_rate.quote_exchange_rate
      WHERE tenant_id=$1 AND quote_id=$2 ORDER BY from_currency`, string(tenant), string(id))
	if err != nil {
		return nil, 0, err
	}
	defer rates.Close()
	for rates.Next() {
		var r rate.ExchangeRate
		var from string
		var scale, fromDigits, quoteDigits int16
		if err = rates.Scan(&from, &r.Rate, &scale, &fromDigits, &quoteDigits, &r.Source, &r.AsOf); err != nil {
			return nil, 0, err
		}
		r.From, r.Scale, r.FromDigits, r.QuoteDigits, r.AsOf = kernel.CurrencyCode(from), int(scale), int(fromDigits), int(quoteDigits), r.AsOf.UTC()
		s.ExchangeRates = append(s.ExchangeRates, r)
	}
	if err = rates.Err(); err != nil {
		return nil, 0, err
	}
	quote, err := rate.RestoreQuote(s)
	return quote, revision, err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// orNull returns nil for an empty string, so that the column stores NULL.
func orNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Insert writes a new quote at revision 1 with its lines.
func (s QuoteStore) Insert(ctx context.Context, quote *rate.Quote) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := quote.State()
	var revision int64
	t := st.Terms
	err = q.QueryRow(ctx, `INSERT INTO cargo_rate.quote (tenant_id,quote_id,legal_entity_id,customer_party_id,customer_party_revision,
        recipient_role,currency,valid_from,valid_until,margin_floor_bp,author,status,incoterms_rule,incoterms_version,named_place,service_level)
      VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) RETURNING revision`,
		string(st.TenantID), string(st.QuoteID), string(st.LegalEntityID), st.Customer.PartyID, st.Customer.PartyRevision,
		string(st.Recipient), string(st.Currency), st.Validity.From, st.Validity.Until, st.MarginFloorBP, st.Author, string(st.Status),
		orNull(t.IncotermsRule), orNull(t.IncotermsVersion), orNull(t.NamedPlace), orNull(t.ServiceLevel)).Scan(&revision)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "quote_pkey" {
		return 0, bentorepo.ErrAlreadyExists
	}
	if err != nil {
		return 0, err
	}
	for i, l := range st.Lines {
		if _, err = q.Exec(ctx, `INSERT INTO cargo_rate.quote_line (tenant_id,quote_id,line_no,charge_code,sell_currency,sell_minor,cost_currency,cost_minor)
          VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, string(st.TenantID), string(st.QuoteID), i+1, string(l.ChargeCode),
			string(l.Sell.Currency), l.Sell.Minor, string(l.Cost.Currency), l.Cost.Minor); err != nil {
			return 0, err
		}
	}
	for _, r := range st.ExchangeRates {
		if _, err = q.Exec(ctx, `INSERT INTO cargo_rate.quote_exchange_rate (tenant_id,quote_id,from_currency,rate,scale,from_digits,quote_digits,source,as_of)
          VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, string(st.TenantID), string(st.QuoteID), string(r.From), r.Rate, r.Scale, r.FromDigits,
			r.QuoteDigits, r.Source, r.AsOf); err != nil {
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
	var approvedBy, approvalReason, acceptedBy, evidence, evidenceKind, approvedAt, acceptedAt, acceptedRecordedAt, acceptedRevision, expiredAt any
	selfApproved := false
	if a := st.Approval; a != nil {
		approvedBy, approvedAt, approvalReason, selfApproved = a.ApprovedBy, a.ApprovedAt, a.ReasonRef, a.SelfApproved
	}
	if a := st.Acceptance; a != nil {
		acceptedBy, acceptedAt, evidenceKind, evidence, acceptedRevision = a.AcceptedBy, a.AcceptedAt, string(a.EvidenceKind), a.EvidenceRef, a.CustomerRevision
		if !a.RecordedAt.IsZero() {
			acceptedRecordedAt = a.RecordedAt
		}
	}
	if !st.ExpiredAt.IsZero() {
		expiredAt = st.ExpiredAt
	}
	var revision int64
	err = q.QueryRow(ctx, `UPDATE cargo_rate.quote SET status=$3,approved_by=$4,approved_at=$5,approval_reason_ref=$6,self_approved=$7,
        accepted_by=$8,accepted_at=$9,accepted_recorded_at=$10,acceptance_evidence_kind=$11,acceptance_evidence_ref=$12,
        accepted_customer_revision=$13,expired_at=$14,revision=revision+1,updated_at=now()
      WHERE tenant_id=$1 AND quote_id=$2 AND revision=$15 RETURNING revision`,
		string(st.TenantID), string(st.QuoteID), string(st.Status), approvedBy, approvedAt, approvalReason, selfApproved,
		acceptedBy, acceptedAt, acceptedRecordedAt, evidenceKind, evidence, acceptedRevision, expiredAt, expected).Scan(&revision)
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
	err = q.QueryRow(ctx, `SELECT request_sha256,result_type,result_id,result_revision FROM cargo_rate.command_receipt
      WHERE tenant_id=$1 AND command_id=$2`, string(tenant), id).Scan(&r.RequestHash, &r.Kind, &r.SubjectID, &r.NewRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Receipt{}, false, nil
	}
	if err != nil {
		return app.Receipt{}, false, err
	}
	if r.Kind == "Quote" {
		r.QuoteID = rate.QuoteID(r.SubjectID)
	}
	return r, true, nil
}

func (s CommandReceipts) Save(ctx context.Context, tenant rate.TenantID, r app.Receipt) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	kind, subject := r.Kind, r.SubjectID
	if kind == "" {
		kind, subject = "Quote", string(r.QuoteID)
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_rate.command_receipt (tenant_id,command_id,request_sha256,result_type,result_id,result_revision)
      VALUES ($1,$2,$3,$4,$5,$6)`, string(tenant), r.CommandID, r.RequestHash, kind, subject, r.NewRevision)
	return err
}
