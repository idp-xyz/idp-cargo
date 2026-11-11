// Package postgres persists FIN's finance policies, freight jobs, credit accounts, command receipts and Outbox events
// in the tenant's RLS scope.
package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	app "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/application"
	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

func utc(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC()
}

func orNull[T comparable](v T) any {
	var zero T
	if v == zero {
		return nil
	}
	return v
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

func children(table, key, order string) string {
	return `(SELECT coalesce(json_agg(c ORDER BY ` + order + `), '[]') FROM cargo_fin.` + table + ` c WHERE ` + key + `)`
}

// PolicyStore loads and writes finance policies.
type PolicyStore struct{ DB *bentopg.DB }

var _ app.PolicyStore = PolicyStore{}

type rateRow struct {
	No         int        `json:"entry_no"`
	Type       string     `json:"rate_type"`
	From       string     `json:"from_currency"`
	Rate       int64      `json:"rate"`
	Scale      int        `json:"scale"`
	FromDigits int        `json:"from_digits"`
	ToDigits   int        `json:"to_digits"`
	ValidFrom  time.Time  `json:"valid_from"`
	ValidUntil *time.Time `json:"valid_until"`
	Source     string     `json:"source"`
	By         string     `json:"recorded_by"`
	At         time.Time  `json:"recorded_at"`
	EnteredAt  time.Time  `json:"entered_at"`
	EndedBy    *string    `json:"ended_by"`
	EndedAt    *time.Time `json:"ended_at"`
	EndReason  *string    `json:"end_reason"`
	EndEntered *time.Time `json:"end_entered_at"`
}

func readPolicy(ctx context.Context, q bentopg.Querier, tenant fin.TenantID, le, lock string) (*fin.FinancePolicy, int64, error) {
	if lock != "" {
		var one int
		err := q.QueryRow(ctx, `SELECT 1 FROM cargo_fin.finance_policy WHERE tenant_id=$1 AND legal_entity_id=$2`+lock, string(tenant), le).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, 0, fmt.Errorf("%w: finance policy %s", bentorepo.ErrNotFound, le)
		}
		if err != nil {
			return nil, 0, err
		}
	}
	st := fin.PolicyState{TenantID: tenant, LegalEntityID: le}
	var currency string
	var revision int64
	var rates []byte
	err := q.QueryRow(ctx, `SELECT p.functional_currency,p.book_code,p.opened_by,p.opened_at,p.entered_at,p.revision,`+
		children("exchange_rate_entry", "c.tenant_id=p.tenant_id AND c.legal_entity_id=p.legal_entity_id", "c.entry_no")+`
      FROM cargo_fin.finance_policy p WHERE p.tenant_id=$1 AND p.legal_entity_id=$2`, string(tenant), le).Scan(&currency, &st.BookCode, &st.OpenedBy,
		&st.OpenedAt, &st.EnteredAt, &revision, &rates)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: finance policy %s", bentorepo.ErrNotFound, le)
	}
	if err != nil {
		return nil, 0, err
	}
	var rr []rateRow
	if err = json.Unmarshal(rates, &rr); err != nil {
		return nil, 0, err
	}
	st.FunctionalCurrency, st.OpenedAt, st.EnteredAt = kernel.CurrencyCode(currency), st.OpenedAt.UTC(), st.EnteredAt.UTC()
	for _, r := range rr {
		st.Rates = append(st.Rates, fin.RateEntry{No: r.No, Type: r.Type, From: kernel.CurrencyCode(r.From), Rate: r.Rate, Scale: r.Scale,
			FromDigits: r.FromDigits, ToDigits: r.ToDigits, ValidFrom: utc(r.ValidFrom), ValidUntil: utc(deref(r.ValidUntil)), Source: r.Source, By: r.By,
			At: utc(r.At), EnteredAt: utc(r.EnteredAt), EndedBy: deref(r.EndedBy), EndedAt: utc(deref(r.EndedAt)), EndReason: deref(r.EndReason),
			EndEnteredAt: utc(deref(r.EndEntered))})
	}
	p, err := fin.RestorePolicy(st)
	return p, revision, err
}

func (s PolicyStore) Load(ctx context.Context, tenant fin.TenantID, le string) (*fin.FinancePolicy, int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	return readPolicy(ctx, q, tenant, le, " FOR NO KEY UPDATE")
}

func (s PolicyStore) Read(ctx context.Context, tenant fin.TenantID, le string) (*fin.FinancePolicy, int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	return readPolicy(ctx, q, tenant, le, "")
}

func (s PolicyStore) Find(ctx context.Context, tenant fin.TenantID, le string) (*fin.FinancePolicy, bool, error) {
	p, _, err := s.Read(ctx, tenant, le)
	if errors.Is(err, bentorepo.ErrNotFound) {
		return nil, false, nil
	}
	return p, err == nil, err
}

func (s PolicyStore) Insert(ctx context.Context, p *fin.FinancePolicy) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := p.State()
	var revision int64
	err = q.QueryRow(ctx, `INSERT INTO cargo_fin.finance_policy (tenant_id,legal_entity_id,functional_currency,book_code,opened_by,opened_at,entered_at)
      VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING revision`, string(st.TenantID), st.LegalEntityID, string(st.FunctionalCurrency), st.BookCode, st.OpenedBy,
		st.OpenedAt, st.EnteredAt).Scan(&revision)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return 0, fmt.Errorf("%w: finance policy %s", bentorepo.ErrAlreadyExists, st.LegalEntityID)
	}
	if err != nil {
		return 0, err
	}
	return revision, writeRates(ctx, q, p)
}

func (s PolicyStore) Save(ctx context.Context, p *fin.FinancePolicy, expected int64) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := p.State()
	var revision int64
	err = q.QueryRow(ctx, `UPDATE cargo_fin.finance_policy SET revision=revision+1 WHERE tenant_id=$1 AND legal_entity_id=$2 AND revision=$3 RETURNING revision`,
		string(st.TenantID), st.LegalEntityID, expected).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: finance policy %s", bentorepo.ErrConflict, st.LegalEntityID)
	}
	if err != nil {
		return 0, err
	}
	return revision, writeRates(ctx, q, p)
}

func writeRates(ctx context.Context, q bentopg.Executor, p *fin.FinancePolicy) error {
	st := p.State()
	for _, e := range p.EndedRates() {
		if _, err := q.Exec(ctx, `UPDATE cargo_fin.exchange_rate_entry SET valid_until=$4,ended_by=$5,ended_at=$6,end_reason=$7,end_entered_at=$8
            WHERE tenant_id=$1 AND legal_entity_id=$2 AND entry_no=$3`, string(st.TenantID), st.LegalEntityID, e.No, e.ValidUntil, e.EndedBy, e.EndedAt,
			e.EndReason, e.EndEnteredAt); err != nil {
			return err
		}
	}
	for _, e := range p.AddedRates() {
		if _, err := q.Exec(ctx, `INSERT INTO cargo_fin.exchange_rate_entry (tenant_id,legal_entity_id,entry_no,rate_type,from_currency,rate,scale,
            from_digits,to_digits,valid_from,valid_until,source,recorded_by,recorded_at,entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
			string(st.TenantID), st.LegalEntityID, e.No, e.Type, string(e.From), e.Rate, e.Scale, e.FromDigits, e.ToDigits, e.ValidFrom, e.ValidUntil,
			e.Source, e.By, e.At, e.EnteredAt); err != nil {
			return err
		}
	}
	return nil
}

// JobStore loads and writes freight jobs.
type JobStore struct{ DB *bentopg.DB }

var _ app.JobStore = JobStore{}

type (
	basisRow struct {
		No            int       `json:"basis_no"`
		BasisID       string    `json:"basis_id"`
		QuoteID       string    `json:"quote_id"`
		QuoteRevision int64     `json:"quote_revision"`
		OrderID       string    `json:"order_id"`
		By            string    `json:"applied_by"`
		At            time.Time `json:"applied_at"`
		EnteredAt     time.Time `json:"entered_at"`
	}
	lineRow struct {
		No          int       `json:"line_no"`
		ChargeCode  string    `json:"charge_code"`
		Description *string   `json:"description"`
		Source      string    `json:"source"`
		BasisNo     *int      `json:"basis_no"`
		QuoteLineNo *int      `json:"quote_line_no"`
		Reason      *string   `json:"reason"`
		By          string    `json:"recorded_by"`
		At          time.Time `json:"recorded_at"`
		EnteredAt   time.Time `json:"entered_at"`
	}
	sideRow struct {
		LineNo        int       `json:"line_no"`
		Side          string    `json:"side"`
		No            int       `json:"version_no"`
		PartyID       *string   `json:"party_id"`
		PartyRevision int64     `json:"party_revision"`
		Currency      string    `json:"currency"`
		Amount        int64     `json:"amount_minor"`
		Local         int64     `json:"local_minor"`
		RateEntryNo   *int      `json:"rate_entry_no"`
		RateType      *string   `json:"rate_type"`
		Rate          *int64    `json:"rate"`
		Scale         *int      `json:"scale"`
		FromDigits    *int      `json:"from_digits"`
		ToDigits      *int      `json:"to_digits"`
		RateSource    *string   `json:"rate_source"`
		Status        string    `json:"status"`
		Reason        *string   `json:"reason"`
		By            string    `json:"recorded_by"`
		At            time.Time `json:"recorded_at"`
		EnteredAt     time.Time `json:"entered_at"`
	}
)

const jobColumns = `j.job_id,j.legal_entity_id,j.shipment_id,j.kind,j.functional_currency,j.book_code,j.customer_party_id,j.plan_version,j.state,j.opened_by,
  j.opened_at,j.entered_at,j.cancel_reason,j.cancelled_by,j.cancelled_at,j.cancel_entered_at,j.revision,`

func jobChildren() string {
	key := "c.tenant_id=j.tenant_id AND c.job_id=j.job_id"
	return children("job_basis", key, "c.basis_no") + `,` + children("charge_line", key, "c.line_no") + `,` +
		children("charge_side_version", key, "c.line_no, c.side DESC, c.version_no")
}

func scanJob(row pgx.Row, tenant fin.TenantID) (*fin.FreightJob, int64, error) {
	st := fin.JobState{TenantID: tenant}
	var currency string
	var cancelReason, cancelledBy *string
	var cancelledAt, cancelEntered *time.Time
	var revision int64
	var bases, lines, sides []byte
	err := row.Scan(&st.JobID, &st.LegalEntityID, &st.ShipmentID, &st.Kind, &currency, &st.BookCode, &st.CustomerPartyID, &st.PlanVersion, &st.State,
		&st.OpenedBy, &st.OpenedAt, &st.EnteredAt, &cancelReason, &cancelledBy, &cancelledAt, &cancelEntered, &revision, &bases, &lines, &sides)
	if err != nil {
		return nil, 0, err
	}
	var br []basisRow
	var lr []lineRow
	var sr []sideRow
	if err = errors.Join(json.Unmarshal(bases, &br), json.Unmarshal(lines, &lr), json.Unmarshal(sides, &sr)); err != nil {
		return nil, 0, err
	}
	st.FunctionalCurrency, st.OpenedAt, st.EnteredAt = kernel.CurrencyCode(currency), st.OpenedAt.UTC(), st.EnteredAt.UTC()
	st.CancelReason, st.CancelledBy, st.CancelledAt, st.CancelEnteredAt = deref(cancelReason), deref(cancelledBy), utc(deref(cancelledAt)), utc(deref(cancelEntered))
	for _, b := range br {
		st.Bases = append(st.Bases, fin.Basis{No: b.No, BasisID: b.BasisID, QuoteID: b.QuoteID, QuoteRevision: b.QuoteRevision, OrderID: b.OrderID,
			By: b.By, At: utc(b.At), EnteredAt: utc(b.EnteredAt)})
	}
	for _, l := range lr {
		st.Lines = append(st.Lines, fin.ChargeLine{No: l.No, ChargeCode: l.ChargeCode, Description: deref(l.Description), Source: l.Source,
			BasisNo: deref(l.BasisNo), QuoteLineNo: deref(l.QuoteLineNo), Reason: deref(l.Reason), By: l.By, At: utc(l.At), EnteredAt: utc(l.EnteredAt)})
	}
	for _, v := range sr {
		if v.LineNo < 1 || v.LineNo > len(st.Lines) {
			return nil, 0, fmt.Errorf("%w: side of line %d", fin.ErrInvalid, v.LineNo)
		}
		sv := fin.SideVersion{Side: v.Side, No: v.No, PartyID: deref(v.PartyID), PartyRevision: v.PartyRevision,
			Amount: kernel.Money{Currency: kernel.CurrencyCode(v.Currency), Minor: v.Amount}, Local: v.Local, Status: v.Status, Reason: deref(v.Reason),
			By: v.By, At: utc(v.At), EnteredAt: utc(v.EnteredAt)}
		if v.Rate != nil {
			sv.FX = &fin.FxSnapshot{EntryNo: deref(v.RateEntryNo), Type: deref(v.RateType), Rate: *v.Rate, Scale: deref(v.Scale),
				FromDigits: deref(v.FromDigits), ToDigits: deref(v.ToDigits), Source: deref(v.RateSource)}
		}
		l := &st.Lines[v.LineNo-1]
		if v.Side == fin.SideSell {
			l.Sell = append(l.Sell, sv)
		} else {
			l.Cost = append(l.Cost, sv)
		}
	}
	j, err := fin.RestoreJob(st)
	return j, revision, err
}

func (s JobStore) read(ctx context.Context, q bentopg.Querier, tenant fin.TenantID, id string) (*fin.FreightJob, int64, error) {
	j, revision, err := scanJob(q.QueryRow(ctx, `SELECT `+jobColumns+jobChildren()+` FROM cargo_fin.freight_job j WHERE j.tenant_id=$1 AND j.job_id=$2`,
		string(tenant), id), tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: freight job %s", bentorepo.ErrNotFound, id)
	}
	return j, revision, err
}

func (s JobStore) Load(ctx context.Context, tenant fin.TenantID, id string) (*fin.FreightJob, int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	var one int
	err = q.QueryRow(ctx, `SELECT 1 FROM cargo_fin.freight_job WHERE tenant_id=$1 AND job_id=$2 FOR NO KEY UPDATE`, string(tenant), id).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: freight job %s", bentorepo.ErrNotFound, id)
	}
	if err != nil {
		return nil, 0, err
	}
	return s.read(ctx, q, tenant, id)
}

func (s JobStore) Read(ctx context.Context, tenant fin.TenantID, id string) (*fin.FreightJob, int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	return s.read(ctx, q, tenant, id)
}

func (s JobStore) list(ctx context.Context, tenant fin.TenantID, where string, args ...any) ([]*fin.FreightJob, []int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, nil, err
	}
	rows, err := q.Query(ctx, `SELECT `+jobColumns+jobChildren()+` FROM cargo_fin.freight_job j WHERE j.tenant_id=$1 AND `+where+` ORDER BY j.opened_at, j.job_id`,
		append([]any{string(tenant)}, args...)...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []*fin.FreightJob
	var revisions []int64
	for rows.Next() {
		j, revision, err := scanJob(rows, tenant)
		if err != nil {
			return nil, nil, err
		}
		out, revisions = append(out, j), append(revisions, revision)
	}
	return out, revisions, rows.Err()
}

func (s JobStore) ForShipment(ctx context.Context, tenant fin.TenantID, shipment string) ([]*fin.FreightJob, []int64, error) {
	return s.list(ctx, tenant, `j.shipment_id=$2`, shipment)
}

func (s JobStore) OpenOf(ctx context.Context, tenant fin.TenantID, legalEntity, customer string) ([]*fin.FreightJob, error) {
	out, _, err := s.list(ctx, tenant, `j.state='OPEN' AND j.legal_entity_id=$2 AND ($3::text = '' OR j.customer_party_id=$3)`, legalEntity, customer)
	return out, err
}

func (s JobStore) Insert(ctx context.Context, j *fin.FreightJob) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := j.State()
	var revision int64
	err = q.QueryRow(ctx, `INSERT INTO cargo_fin.freight_job (tenant_id,job_id,legal_entity_id,shipment_id,kind,functional_currency,book_code,
        customer_party_id,plan_version,state,opened_by,opened_at,entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING revision`,
		string(st.TenantID), st.JobID, st.LegalEntityID, st.ShipmentID, st.Kind, string(st.FunctionalCurrency), st.BookCode, st.CustomerPartyID,
		st.PlanVersion, st.State, st.OpenedBy, st.OpenedAt, st.EnteredAt).Scan(&revision)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case "freight_job_pkey":
			return 0, fmt.Errorf("%w: freight job %s", bentorepo.ErrAlreadyExists, st.JobID)
		case "freight_job_one_per_shipment":
			return 0, fmt.Errorf("%w: shipment %s has a job of %s already", fin.ErrNotOpenable, st.ShipmentID, st.LegalEntityID)
		}
	}
	if err != nil {
		return 0, err
	}
	return revision, writeJob(ctx, q, j)
}

func (s JobStore) Save(ctx context.Context, j *fin.FreightJob, expected int64) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := j.State()
	var revision int64
	err = q.QueryRow(ctx, `UPDATE cargo_fin.freight_job SET state=$3,cancel_reason=$4,cancelled_by=$5,cancelled_at=$6,cancel_entered_at=$7,
        revision=revision+1,updated_at=now() WHERE tenant_id=$1 AND job_id=$2 AND revision=$8 RETURNING revision`, string(st.TenantID), st.JobID, st.State,
		orNull(st.CancelReason), orNull(st.CancelledBy), orNull(st.CancelledAt), orNull(st.CancelEnteredAt), expected).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: freight job %s", bentorepo.ErrConflict, st.JobID)
	}
	if err != nil {
		return 0, err
	}
	if st.State == fin.JobCancelled {
		if _, err = q.Exec(ctx, `UPDATE cargo_fin.job_basis SET live=false WHERE tenant_id=$1 AND job_id=$2 AND live`, string(st.TenantID), st.JobID); err != nil {
			return 0, err
		}
	}
	return revision, writeJob(ctx, q, j)
}

func writeJob(ctx context.Context, q bentopg.Executor, j *fin.FreightJob) error {
	st := j.State()
	bases, lines, sides, lineOf := j.Added()
	exec := func(sql string, args ...any) error {
		_, err := q.Exec(ctx, sql, append([]any{string(st.TenantID), st.JobID}, args...)...)
		return err
	}
	for _, b := range bases {
		err := exec(`INSERT INTO cargo_fin.job_basis (tenant_id,job_id,basis_no,basis_id,quote_id,quote_revision,order_id,applied_by,applied_at,entered_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, b.No, b.BasisID, b.QuoteID, b.QuoteRevision, b.OrderID, b.By, b.At, b.EnteredAt)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "job_basis_once" {
			return fmt.Errorf("%w: quote %s for order %s", fin.ErrBasisApplied, b.QuoteID, b.OrderID)
		}
		if err != nil {
			return err
		}
	}
	for _, l := range lines {
		if err := exec(`INSERT INTO cargo_fin.charge_line (tenant_id,job_id,line_no,charge_code,description,source,basis_no,quote_line_no,reason,recorded_by,
            recorded_at,entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, l.No, l.ChargeCode, orNull(l.Description), l.Source, orNull(l.BasisNo),
			orNull(l.QuoteLineNo), orNull(l.Reason), l.By, l.At, l.EnteredAt); err != nil {
			return err
		}
	}
	for i, v := range sides {
		var entry, rate, scale, from, to any
		var rateType, source any
		if fx := v.FX; fx != nil {
			entry, rateType, rate, scale, from, to, source = fx.EntryNo, fx.Type, fx.Rate, fx.Scale, fx.FromDigits, fx.ToDigits, fx.Source
		}
		if err := exec(`INSERT INTO cargo_fin.charge_side_version (tenant_id,job_id,line_no,side,version_no,party_id,party_revision,currency,amount_minor,
            local_minor,rate_entry_no,rate_type,rate,scale,from_digits,to_digits,rate_source,status,reason,recorded_by,recorded_at,entered_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)`, lineOf[i], v.Side, v.No, orNull(v.PartyID), v.PartyRevision,
			string(v.Amount.Currency), v.Amount.Minor, v.Local, entry, rateType, rate, scale, from, to, source, v.Status, orNull(v.Reason), v.By, v.At,
			v.EnteredAt); err != nil {
			return err
		}
	}
	return nil
}

// CreditStore loads and writes credit accounts.
type CreditStore struct{ DB *bentopg.DB }

var _ app.CreditStore = CreditStore{}

type (
	termsRow struct {
		No        int       `json:"version_no"`
		Limit     int64     `json:"limit_minor"`
		Currency  string    `json:"currency"`
		Days      int       `json:"payment_days"`
		Basis     string    `json:"payment_basis"`
		By        string    `json:"set_by"`
		At        time.Time `json:"set_at"`
		EnteredAt time.Time `json:"entered_at"`
	}
	holdRow struct {
		No               int        `json:"hold_no"`
		Code             string     `json:"code"`
		Reason           string     `json:"reason"`
		By               string     `json:"placed_by"`
		At               time.Time  `json:"placed_at"`
		EnteredAt        time.Time  `json:"entered_at"`
		ReleasedBy       *string    `json:"released_by"`
		ReleasedAt       *time.Time `json:"released_at"`
		ReleaseReason    *string    `json:"release_reason"`
		ReleaseEnteredAt *time.Time `json:"release_entered_at"`
	}
)

func accountSelect(where string) string {
	key := "c.tenant_id=a.tenant_id AND c.legal_entity_id=a.legal_entity_id AND c.debtor_party_id=a.debtor_party_id"
	return `SELECT a.legal_entity_id,a.debtor_party_id,a.currency,a.state,a.revision,` + children("credit_terms_version", key, "c.version_no") + `,` +
		children("credit_hold", key, "c.hold_no") + ` FROM cargo_fin.credit_account a WHERE a.tenant_id=$1 AND ` + where
}

func scanAccount(row pgx.Row, tenant fin.TenantID) (*fin.CreditAccount, int64, error) {
	st := fin.CreditState{TenantID: tenant}
	var currency string
	var revision int64
	var terms, holds []byte
	if err := row.Scan(&st.LegalEntityID, &st.DebtorPartyID, &currency, &st.State, &revision, &terms, &holds); err != nil {
		return nil, 0, err
	}
	var tr []termsRow
	var hr []holdRow
	if err := errors.Join(json.Unmarshal(terms, &tr), json.Unmarshal(holds, &hr)); err != nil {
		return nil, 0, err
	}
	st.Currency = kernel.CurrencyCode(currency)
	for _, t := range tr {
		st.Terms = append(st.Terms, fin.TermsVersion{No: t.No, Limit: kernel.Money{Currency: kernel.CurrencyCode(t.Currency), Minor: t.Limit},
			PaymentDays: t.Days, PaymentBasis: t.Basis, By: t.By, At: utc(t.At), EnteredAt: utc(t.EnteredAt)})
	}
	for _, h := range hr {
		st.Holds = append(st.Holds, fin.Hold{No: h.No, Code: h.Code, Reason: h.Reason, By: h.By, At: utc(h.At), EnteredAt: utc(h.EnteredAt),
			ReleasedBy: deref(h.ReleasedBy), ReleasedAt: utc(deref(h.ReleasedAt)), ReleaseReason: deref(h.ReleaseReason),
			ReleaseEnteredAt: utc(deref(h.ReleaseEnteredAt))})
	}
	a, err := fin.RestoreAccount(st)
	return a, revision, err
}

func (s CreditStore) Load(ctx context.Context, tenant fin.TenantID, le, debtor string) (*fin.CreditAccount, int64, bool, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, 0, false, err
	}
	var one int
	err = q.QueryRow(ctx, `SELECT 1 FROM cargo_fin.credit_account WHERE tenant_id=$1 AND legal_entity_id=$2 AND debtor_party_id=$3 FOR NO KEY UPDATE`,
		string(tenant), le, debtor).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	a, revision, err := scanAccount(q.QueryRow(ctx, accountSelect(`a.legal_entity_id=$2 AND a.debtor_party_id=$3`), string(tenant), le, debtor), tenant)
	return a, revision, err == nil, err
}

func (s CreditStore) Read(ctx context.Context, tenant fin.TenantID, le, debtor string) (*fin.CreditAccount, int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	a, revision, err := scanAccount(q.QueryRow(ctx, accountSelect(`a.legal_entity_id=$2 AND a.debtor_party_id=$3`), string(tenant), le, debtor), tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: credit account %s/%s", bentorepo.ErrNotFound, le, debtor)
	}
	return a, revision, err
}

func (s CreditStore) List(ctx context.Context, tenant fin.TenantID, le string) ([]*fin.CreditAccount, []int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, nil, err
	}
	rows, err := q.Query(ctx, accountSelect(`a.legal_entity_id=$2`)+` ORDER BY a.debtor_party_id`, string(tenant), le)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []*fin.CreditAccount
	var revisions []int64
	for rows.Next() {
		a, revision, err := scanAccount(rows, tenant)
		if err != nil {
			return nil, nil, err
		}
		out, revisions = append(out, a), append(revisions, revision)
	}
	return out, revisions, rows.Err()
}

func (s CreditStore) Insert(ctx context.Context, a *fin.CreditAccount) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := a.State()
	var revision int64
	err = q.QueryRow(ctx, `INSERT INTO cargo_fin.credit_account (tenant_id,legal_entity_id,debtor_party_id,currency,state) VALUES ($1,$2,$3,$4,$5)
      RETURNING revision`, string(st.TenantID), st.LegalEntityID, st.DebtorPartyID, string(st.Currency), st.State).Scan(&revision)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return 0, fmt.Errorf("%w: credit account %s/%s", bentorepo.ErrConflict, st.LegalEntityID, st.DebtorPartyID)
	}
	if err != nil {
		return 0, err
	}
	return revision, writeAccount(ctx, q, a)
}

func (s CreditStore) Save(ctx context.Context, a *fin.CreditAccount, expected int64) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := a.State()
	var revision int64
	err = q.QueryRow(ctx, `UPDATE cargo_fin.credit_account SET state=$4,revision=revision+1,updated_at=now() WHERE tenant_id=$1 AND legal_entity_id=$2
      AND debtor_party_id=$3 AND revision=$5 RETURNING revision`, string(st.TenantID), st.LegalEntityID, st.DebtorPartyID, st.State, expected).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: credit account %s/%s", bentorepo.ErrConflict, st.LegalEntityID, st.DebtorPartyID)
	}
	if err != nil {
		return 0, err
	}
	return revision, writeAccount(ctx, q, a)
}

func writeAccount(ctx context.Context, q bentopg.Executor, a *fin.CreditAccount) error {
	st := a.State()
	terms, holds, released := a.Added()
	exec := func(sql string, args ...any) error {
		_, err := q.Exec(ctx, sql, append([]any{string(st.TenantID), st.LegalEntityID, st.DebtorPartyID}, args...)...)
		return err
	}
	for _, t := range terms {
		if err := exec(`INSERT INTO cargo_fin.credit_terms_version (tenant_id,legal_entity_id,debtor_party_id,version_no,limit_minor,currency,payment_days,
            payment_basis,set_by,set_at,entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, t.No, t.Limit.Minor, string(t.Limit.Currency), t.PaymentDays,
			t.PaymentBasis, t.By, t.At, t.EnteredAt); err != nil {
			return err
		}
	}
	for _, h := range released {
		if err := exec(`UPDATE cargo_fin.credit_hold SET released_by=$5,released_at=$6,release_reason=$7,release_entered_at=$8
            WHERE tenant_id=$1 AND legal_entity_id=$2 AND debtor_party_id=$3 AND hold_no=$4`, h.No, h.ReleasedBy, h.ReleasedAt, h.ReleaseReason,
			h.ReleaseEnteredAt); err != nil {
			return err
		}
	}
	for _, h := range holds {
		if err := exec(`INSERT INTO cargo_fin.credit_hold (tenant_id,legal_entity_id,debtor_party_id,hold_no,code,reason,placed_by,placed_at,entered_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, h.No, h.Code, h.Reason, h.By, h.At, h.EnteredAt); err != nil {
			return err
		}
	}
	return nil
}

// CommandReceipts stores the result of every committed FIN command for replays.
type CommandReceipts struct{ DB *bentopg.DB }

var _ app.Receipts = CommandReceipts{}

func (s CommandReceipts) Find(ctx context.Context, tenant fin.TenantID, id string) (app.Receipt, bool, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return app.Receipt{}, false, err
	}
	r := app.Receipt{CommandID: id}
	var version *int
	err = q.QueryRow(ctx, `SELECT request_sha256,result_id,result_revision,result_version FROM cargo_fin.command_receipt WHERE tenant_id=$1 AND command_id=$2`,
		string(tenant), id).Scan(&r.RequestHash, &r.SubjectID, &r.NewRevision, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Receipt{}, false, nil
	}
	if err != nil {
		return app.Receipt{}, false, err
	}
	r.Version = deref(version)
	return r, true, nil
}

func (s CommandReceipts) Save(ctx context.Context, tenant fin.TenantID, r app.Receipt, kind string) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_fin.command_receipt (tenant_id,command_id,request_sha256,result_type,result_id,result_revision,result_version)
      VALUES ($1,$2,$3,$4,$5,$6,$7)`, string(tenant), r.CommandID, r.RequestHash, kind, r.SubjectID, r.NewRevision, r.Version)
	return err
}

// Outbox writes FIN events to the Bento Outbox in the command's transaction.
type Outbox struct {
	Writer     eventing.OutboxWriter
	CommandID  string
	RecordedAt time.Time
}

func (o Outbox) EnqueueFINEvent(ctx context.Context, e fin.Event) error {
	envelope, err := FINEnvelope(o.CommandID, e, o.RecordedAt)
	if err != nil {
		return err
	}
	return o.Writer.Enqueue(ctx, envelope)
}

// FINEnvelope maps a FIN domain event to its Bento envelope; a retried command yields the same event ID.
func FINEnvelope(commandID string, e fin.Event, recordedAt time.Time) (eventing.Envelope, error) {
	digest := sha256.Sum256([]byte(commandID + "\x00" + e.Type + "\x00" + string(e.TenantID) + "\x00" + e.SubjectID + "\x00" + strconv.Itoa(e.LineNo) + "\x00" +
		e.Side + "\x00" + strconv.Itoa(e.Version)))
	payload, err := json.Marshal(api.EventPayload{TenantID: string(e.TenantID), SubjectID: e.SubjectID, LegalEntityID: e.LegalEntityID, Version: e.Version,
		ShipmentID: e.ShipmentID, DebtorPartyID: e.DebtorPartyID, LineNo: e.LineNo, Side: e.Side})
	if err != nil {
		return eventing.Envelope{}, err
	}
	envelope := eventing.Envelope{SpecVersion: eventing.SpecVersionV1, ID: eventing.EventID(hex.EncodeToString(digest[:])), Source: api.Source,
		Type: eventing.EventType(e.Type), Version: 1, Scope: string(e.TenantID), Subject: e.SubjectID, PartitionKey: string(e.TenantID) + ":" + e.SubjectID,
		OccurredAt: e.At.UTC(), RecordedAt: recordedAt.UTC(), ContentType: eventing.JSONContentType, Payload: payload}
	return envelope, envelope.Validate()
}

// Conflict reports whether a commit failed on one of FIN's deferred checks.
func Conflict(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		return false
	}
	switch pgErr.ConstraintName {
	case "finance_policy_consistent", "freight_job_consistent", "credit_account_consistent", "fin_records_with_parent":
		return true
	}
	return false
}

// RateInUse reports whether ending a rate entry failed because a charge was converted at it after the new end.
func RateInUse(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == "exchange_rate_entry_in_use"
}
