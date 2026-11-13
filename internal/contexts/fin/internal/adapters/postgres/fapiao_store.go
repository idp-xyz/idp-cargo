package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FapiaoStore loads and writes fapiao records (FB6); Load locks the record row.
type FapiaoStore struct{ DB *bentopg.DB }

const fapiaoColumns = `legal_entity_id,debtor_party_id,debtor_revision,fapiao_type,currency,profile_version,buyer_title,taxpayer_number,buyer_address,
  buyer_phone,bank_name,bank_account,state,requested_by,requested_at,entered_at,issued_number,issued_on::text,issued_evidence_ref,issued_by,issued_at,
  issued_entered_at,failed_reason,failed_by,failed_at,failed_entered_at,red_number,red_on::text,red_evidence_ref,red_confirmation_ref,red_reason,red_by,
  red_at,red_entered_at,revision`

func (s FapiaoStore) Load(ctx context.Context, tenant fin.TenantID, id string) (*fin.FapiaoRecord, int64, error) {
	return readFapiao(ctx, s.DB, tenant, id, " FOR NO KEY UPDATE")
}

func (s FapiaoStore) Read(ctx context.Context, tenant fin.TenantID, id string) (*fin.FapiaoRecord, int64, error) {
	return readFapiao(ctx, s.DB, tenant, id, "")
}

func readFapiao(ctx context.Context, db *bentopg.DB, tenant fin.TenantID, id, lock string) (*fin.FapiaoRecord, int64, error) {
	q, err := db.ReadExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	st := fin.FapiaoState{TenantID: tenant, FapiaoID: id}
	var currency string
	var address, phone, bank, account, issuedNumber, issuedOn, issuedRef, issuedBy, failReason, failedBy *string
	var redNumber, redOn, redRef, redConfirmation, redReason, redBy *string
	var issuedAt, issuedEntered, failedAt, failEntered, redAt, redEntered *time.Time
	var revision int64
	err = q.QueryRow(ctx, `SELECT `+fapiaoColumns+` FROM cargo_fin.fapiao_record WHERE tenant_id=$1 AND fapiao_id=$2`+lock, string(tenant), id).Scan(
		&st.LegalEntityID, &st.DebtorPartyID, &st.DebtorRevision, &st.Type, &currency, &st.Buyer.ProfileVersion, &st.Buyer.Title, &st.Buyer.TaxpayerNumber,
		&address, &phone, &bank, &account, &st.State, &st.RequestedBy, &st.RequestedAt, &st.EnteredAt, &issuedNumber, &issuedOn, &issuedRef, &issuedBy,
		&issuedAt, &issuedEntered, &failReason, &failedBy, &failedAt, &failEntered, &redNumber, &redOn, &redRef, &redConfirmation, &redReason, &redBy,
		&redAt, &redEntered, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: fapiao record %s", bentorepo.ErrNotFound, id)
	}
	if err != nil {
		return nil, 0, err
	}
	st.Currency = kernel.CurrencyCode(currency)
	st.Buyer.Address, st.Buyer.Phone, st.Buyer.BankName, st.Buyer.BankAccount = deref(address), deref(phone), deref(bank), deref(account)
	st.RequestedAt, st.EnteredAt = st.RequestedAt.UTC(), st.EnteredAt.UTC()
	evidence := func(number, on, ref, confirmation, by *string, at, entered *time.Time) (*fin.PlatformEvidence, error) {
		if number == nil {
			return nil, nil
		}
		d, err := fin.ParseDate(deref(on))
		if err != nil {
			return nil, err
		}
		return &fin.PlatformEvidence{Number: *number, On: d, EvidenceRef: deref(ref), ConfirmationRef: deref(confirmation), By: deref(by),
			At: deref(at).UTC(), EnteredAt: deref(entered).UTC()}, nil
	}
	if st.Issued, err = evidence(issuedNumber, issuedOn, issuedRef, nil, issuedBy, issuedAt, issuedEntered); err != nil {
		return nil, 0, err
	}
	if st.RedLetter, err = evidence(redNumber, redOn, redRef, redConfirmation, redBy, redAt, redEntered); err != nil {
		return nil, 0, err
	}
	st.RedReason, st.FailReason, st.FailedBy = deref(redReason), deref(failReason), deref(failedBy)
	if failedAt != nil {
		st.FailedAt, st.FailEnteredAt = failedAt.UTC(), deref(failEntered).UTC()
	}
	rows, err := q.Query(ctx, `SELECT c.invoice_id, n.number, n.invoice_date::text, c.released FROM cargo_fin.fapiao_covered_note c
      JOIN cargo_fin.debtor_invoice n ON n.tenant_id=c.tenant_id AND n.invoice_id=c.invoice_id WHERE c.tenant_id=$1 AND c.fapiao_id=$2 ORDER BY c.invoice_id`,
		string(tenant), id)
	if err != nil {
		return nil, 0, err
	}
	at := map[string]int{}
	for rows.Next() {
		var n fin.CoveredNote
		var invoiced string
		if err = rows.Scan(&n.InvoiceID, &n.Number, &invoiced, &n.Released); err != nil {
			rows.Close()
			return nil, 0, err
		}
		if n.InvoiceDate, err = fin.ParseDate(invoiced); err != nil {
			rows.Close()
			return nil, 0, err
		}
		at[n.InvoiceID] = len(st.Notes)
		st.Notes = append(st.Notes, n)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, 0, err
	}
	amounts, err := q.Query(ctx, `SELECT invoice_id, treatment, rate_bp, local_minor FROM cargo_fin.fapiao_covered_amount WHERE tenant_id=$1 AND fapiao_id=$2
      ORDER BY invoice_id, treatment, rate_bp`, string(tenant), id)
	if err != nil {
		return nil, 0, err
	}
	defer amounts.Close()
	for amounts.Next() {
		var invoice string
		var a fin.TaxAmount
		if err = amounts.Scan(&invoice, &a.Treatment, &a.RateBP, &a.Local); err != nil {
			return nil, 0, err
		}
		i, ok := at[invoice]
		if !ok {
			return nil, 0, fmt.Errorf("fin: fapiao record %s has amounts of %s, which it does not cover", id, invoice)
		}
		st.Notes[i].Amounts = append(st.Notes[i].Amounts, a)
	}
	if err = amounts.Err(); err != nil {
		return nil, 0, err
	}
	r, err := fin.RestoreFapiao(st)
	return r, revision, err
}

// Insert writes a requested record with its covered notes and their amounts.
func (s FapiaoStore) Insert(ctx context.Context, r *fin.FapiaoRecord) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := r.State()
	gross, vat := st.Total()
	b := st.Buyer
	var revision int64
	err = q.QueryRow(ctx, `INSERT INTO cargo_fin.fapiao_record (tenant_id,fapiao_id,legal_entity_id,debtor_party_id,debtor_revision,fapiao_type,currency,
      profile_version,buyer_title,taxpayer_number,buyer_address,buyer_phone,bank_name,bank_account,gross_minor,vat_minor,state,requested_by,requested_at,
      entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20) RETURNING revision`, string(st.TenantID), st.FapiaoID,
		st.LegalEntityID, st.DebtorPartyID, st.DebtorRevision, st.Type, string(st.Currency), b.ProfileVersion, b.Title, b.TaxpayerNumber, orNull(b.Address),
		orNull(b.Phone), orNull(b.BankName), orNull(b.BankAccount), gross, vat, st.State, st.RequestedBy, st.RequestedAt, st.EnteredAt).Scan(&revision)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return 0, fmt.Errorf("%w: fapiao record %s", bentorepo.ErrAlreadyExists, st.FapiaoID)
	}
	if err != nil {
		return 0, err
	}
	for _, n := range st.Notes {
		_, err = q.Exec(ctx, `INSERT INTO cargo_fin.fapiao_covered_note (tenant_id,fapiao_id,invoice_id) VALUES ($1,$2,$3)`, string(st.TenantID),
			st.FapiaoID, n.InvoiceID)
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "fapiao_covered_note_once" {
			return 0, fmt.Errorf("%w: debit note %s", fin.ErrFapiaoCovers, n.Number)
		}
		if err != nil {
			return 0, err
		}
		for _, a := range n.Amounts {
			if _, err = q.Exec(ctx, `INSERT INTO cargo_fin.fapiao_covered_amount (tenant_id,fapiao_id,invoice_id,treatment,rate_bp,local_minor)
              VALUES ($1,$2,$3,$4,$5,$6)`, string(st.TenantID), st.FapiaoID, n.InvoiceID, a.Treatment, a.RateBP, a.Local); err != nil {
				return 0, err
			}
		}
	}
	return revision, nil
}

// Save writes the record's issue, failure or red letter at the expected revision, the numbers it recorded and the
// release of its notes.
func (s FapiaoStore) Save(ctx context.Context, r *fin.FapiaoRecord, expected int64) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := r.State()
	issued, red := st.Issued, st.RedLetter
	if issued == nil {
		issued = &fin.PlatformEvidence{}
	}
	if red == nil {
		red = &fin.PlatformEvidence{}
	}
	var revision int64
	err = q.QueryRow(ctx, `UPDATE cargo_fin.fapiao_record SET state=$3,issued_number=$4,issued_on=$5,issued_evidence_ref=$6,issued_by=$7,issued_at=$8,
      issued_entered_at=$9,failed_reason=$10,failed_by=$11,failed_at=$12,failed_entered_at=$13,red_number=$14,red_on=$15,red_evidence_ref=$16,
      red_confirmation_ref=$17,red_reason=$18,red_by=$19,red_at=$20,red_entered_at=$21,revision=revision+1,updated_at=now()
      WHERE tenant_id=$1 AND fapiao_id=$2 AND revision=$22 RETURNING revision`, string(st.TenantID), st.FapiaoID, st.State, orNull(issued.Number),
		dateOrNull(issued.On), orNull(issued.EvidenceRef), orNull(issued.By), orNull(issued.At), orNull(issued.EnteredAt), orNull(st.FailReason),
		orNull(st.FailedBy), orNull(st.FailedAt), orNull(st.FailEnteredAt), orNull(red.Number), dateOrNull(red.On), orNull(red.EvidenceRef),
		orNull(red.ConfirmationRef), orNull(st.RedReason), orNull(red.By), orNull(red.At), orNull(red.EnteredAt), expected).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: fapiao record %s", bentorepo.ErrConflict, st.FapiaoID)
	}
	if err != nil {
		return 0, err
	}
	// a move records one number: the fapiao's as it is issued, the red-letter fapiao's as it is red-lettered
	kind, number := "", ""
	switch st.State {
	case fin.FapiaoIssued:
		kind, number = "ISSUED", issued.Number
	case fin.FapiaoRedLettered:
		kind, number = "RED_LETTER", red.Number
	}
	if kind != "" {
		_, err = q.Exec(ctx, `INSERT INTO cargo_fin.fapiao_number (tenant_id,legal_entity_id,number,fapiao_id,kind) VALUES ($1,$2,$3,$4,$5)`,
			string(st.TenantID), st.LegalEntityID, number, st.FapiaoID, kind)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "fapiao_number_pkey" {
			return 0, fmt.Errorf("%w: %s", fin.ErrFapiaoNumber, number)
		}
		if err != nil {
			return 0, err
		}
	}
	if !st.Live() {
		if _, err = q.Exec(ctx, `UPDATE cargo_fin.fapiao_covered_note SET released=true WHERE tenant_id=$1 AND fapiao_id=$2 AND NOT released`,
			string(st.TenantID), st.FapiaoID); err != nil {
			return 0, err
		}
	}
	return revision, nil
}

// Notes locks the jobs of the debit notes, in order of job id, until the transaction ends, then reads what a fapiao
// request needs of each: job commands (credit notes among them) and fapiao requests on a job's notes take turns.
func (s FapiaoStore) Notes(ctx context.Context, tenant fin.TenantID, invoiceIDs []string) ([]fin.FapiaoNote, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, err
	}
	locked, err := q.Query(ctx, `SELECT j.job_id FROM cargo_fin.freight_job j WHERE j.tenant_id=$1 AND j.job_id IN (SELECT n.job_id
      FROM cargo_fin.debtor_invoice n WHERE n.tenant_id=$1 AND n.invoice_id = ANY($2)) ORDER BY j.job_id FOR NO KEY UPDATE`, string(tenant), invoiceIDs)
	if err != nil {
		return nil, err
	}
	locked.Close()
	if err = locked.Err(); err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT n.invoice_id, n.kind, n.state, n.number, n.legal_entity_id, n.debtor_party_id, n.invoice_date::text
      FROM cargo_fin.debtor_invoice n WHERE n.tenant_id=$1 AND n.invoice_id = ANY($2) ORDER BY n.invoice_id`, string(tenant), invoiceIDs)
	if err != nil {
		return nil, err
	}
	var out []fin.FapiaoNote
	at := map[string]int{}
	for rows.Next() {
		var n fin.FapiaoNote
		var invoiced string
		if err = rows.Scan(&n.InvoiceID, &n.Kind, &n.State, &n.Number, &n.LegalEntityID, &n.DebtorPartyID, &invoiced); err != nil {
			rows.Close()
			return nil, err
		}
		if n.InvoiceDate, err = fin.ParseDate(invoiced); err != nil {
			rows.Close()
			return nil, err
		}
		at[n.InvoiceID] = len(out)
		out = append(out, n)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	// a debit note's own lines, and the credit note lines that reverse them, per treatment and rate
	amounts, err := q.Query(ctx, `SELECT coalesce(l.credits_invoice_id, l.invoice_id), l.treatment, coalesce(l.rate_bp, 0), sum(l.local_minor)::bigint
      FROM cargo_fin.debtor_invoice_line l WHERE l.tenant_id=$1 AND (l.invoice_id = ANY($2) OR l.credits_invoice_id = ANY($2))
      GROUP BY 1, 2, 3 HAVING sum(l.local_minor) <> 0 ORDER BY 1, 2, 3`, string(tenant), invoiceIDs)
	if err != nil {
		return nil, err
	}
	defer amounts.Close()
	for amounts.Next() {
		var invoice string
		var a fin.TaxAmount
		if err = amounts.Scan(&invoice, &a.Treatment, &a.RateBP, &a.Local); err != nil {
			return nil, err
		}
		if i, ok := at[invoice]; ok {
			out[i].Amounts = append(out[i].Amounts, a)
		}
	}
	return out, amounts.Err()
}

// Covering answers the requested or issued record that covers a debit note, if any.
func (s FapiaoStore) Covering(ctx context.Context, tenant fin.TenantID, invoiceID string) (string, bool, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return "", false, err
	}
	var id string
	err = q.QueryRow(ctx, `SELECT fapiao_id FROM cargo_fin.fapiao_covered_note WHERE tenant_id=$1 AND invoice_id=$2 AND NOT released`, string(tenant),
		invoiceID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

// OfDebtor lists a debtor's records with a legal entity, in the order requested.
func (s FapiaoStore) OfDebtor(ctx context.Context, tenant fin.TenantID, legalEntity, debtor string) ([]*fin.FapiaoRecord, []int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, nil, err
	}
	rows, err := q.Query(ctx, `SELECT fapiao_id FROM cargo_fin.fapiao_record WHERE tenant_id=$1 AND legal_entity_id=$2 AND debtor_party_id=$3
      ORDER BY requested_at, fapiao_id`, string(tenant), legalEntity, debtor)
	if err != nil {
		return nil, nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, nil, err
	}
	var out []*fin.FapiaoRecord
	var revisions []int64
	for _, id := range ids {
		r, revision, err := s.Read(ctx, tenant, id)
		if err != nil {
			return nil, nil, err
		}
		out, revisions = append(out, r), append(revisions, revision)
	}
	return out, revisions, nil
}
