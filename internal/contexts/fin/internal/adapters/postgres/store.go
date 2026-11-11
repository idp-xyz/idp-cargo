// Package postgres persists FIN's finance policies, freight jobs with their notes, billing profiles, credit accounts,
// command receipts and Outbox events in the tenant's RLS scope.
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

type taxRow struct {
	No            int       `json:"version_no"`
	ChargeCode    string    `json:"charge_code"`
	Treatment     string    `json:"treatment"`
	RateBP        *int      `json:"rate_bp"`
	EffectiveFrom string    `json:"effective_from"`
	By            string    `json:"set_by"`
	At            time.Time `json:"set_at"`
	EnteredAt     time.Time `json:"entered_at"`
}

type seriesRow struct {
	Kind      string    `json:"kind"`
	Prefix    string    `json:"prefix"`
	Width     int       `json:"width"`
	Start     int64     `json:"start_seq"`
	Next      int64     `json:"next_seq"`
	By        string    `json:"registered_by"`
	At        time.Time `json:"registered_at"`
	EnteredAt time.Time `json:"entered_at"`
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
	var rates, taxes, series []byte
	key := "c.tenant_id=p.tenant_id AND c.legal_entity_id=p.legal_entity_id"
	err := q.QueryRow(ctx, `SELECT p.functional_currency,p.book_code,p.opened_by,p.opened_at,p.entered_at,p.revision,`+
		children("exchange_rate_entry", key, "c.entry_no")+`,`+children("charge_tax_treatment", key, "c.version_no")+`,`+
		children("invoice_series", key, "c.kind")+`
      FROM cargo_fin.finance_policy p WHERE p.tenant_id=$1 AND p.legal_entity_id=$2`, string(tenant), le).Scan(&currency, &st.BookCode, &st.OpenedBy,
		&st.OpenedAt, &st.EnteredAt, &revision, &rates, &taxes, &series)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: finance policy %s", bentorepo.ErrNotFound, le)
	}
	if err != nil {
		return nil, 0, err
	}
	var rr []rateRow
	var tr []taxRow
	var sr []seriesRow
	if err = errors.Join(json.Unmarshal(rates, &rr), json.Unmarshal(taxes, &tr), json.Unmarshal(series, &sr)); err != nil {
		return nil, 0, err
	}
	for _, t := range tr {
		from, err := fin.ParseDate(t.EffectiveFrom)
		if err != nil {
			return nil, 0, err
		}
		st.Taxes = append(st.Taxes, fin.TaxTreatment{No: t.No, ChargeCode: t.ChargeCode, Treatment: t.Treatment, RateBP: deref(t.RateBP), EffectiveFrom: from,
			By: t.By, At: utc(t.At), EnteredAt: utc(t.EnteredAt)})
	}
	for _, x := range sr {
		st.Series = append(st.Series, fin.Series{Kind: x.Kind, Prefix: x.Prefix, Width: x.Width, Start: x.Start, Next: x.Next, By: x.By, At: utc(x.At),
			EnteredAt: utc(x.EnteredAt)})
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

func (s PolicyStore) Shared(ctx context.Context, tenant fin.TenantID, le string) (*fin.FinancePolicy, bool, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, false, err
	}
	p, _, err := readPolicy(ctx, q, tenant, le, " FOR SHARE")
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
	for _, t := range p.AddedTaxes() {
		if _, err := q.Exec(ctx, `INSERT INTO cargo_fin.charge_tax_treatment (tenant_id,legal_entity_id,version_no,charge_code,treatment,rate_bp,effective_from,
            set_by,set_at,entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, string(st.TenantID), st.LegalEntityID, t.No, t.ChargeCode, t.Treatment,
			orNull(t.RateBP), t.EffectiveFrom.Format(time.DateOnly), t.By, t.At, t.EnteredAt); err != nil {
			return err
		}
	}
	for _, x := range p.AddedSeries() {
		if _, err := q.Exec(ctx, `INSERT INTO cargo_fin.invoice_series (tenant_id,legal_entity_id,kind,prefix,width,start_seq,next_seq,registered_by,
            registered_at,entered_at) VALUES ($1,$2,$3,$4,$5,$6,$6,$7,$8,$9)`, string(st.TenantID), st.LegalEntityID, x.Kind, x.Prefix, x.Width, x.Start, x.By,
			x.At, x.EnteredAt); err != nil {
			return err
		}
	}
	return nil
}

// Series draws numbers from the legal entities' series.
type Series struct{ DB *bentopg.DB }

var _ app.SeriesStore = Series{}

// Next takes the next sequence value of a series, holding the series row until the transaction ends; a value past
// the series' width is refused by the database (invoice_series_range).
func (s Series) Next(ctx context.Context, tenant fin.TenantID, legalEntity, kind string) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	var seq int64
	err = q.QueryRow(ctx, `UPDATE cargo_fin.invoice_series SET next_seq=next_seq+1 WHERE tenant_id=$1 AND legal_entity_id=$2 AND kind=$3
      RETURNING next_seq-1`, string(tenant), legalEntity, kind).Scan(&seq)
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, fmt.Errorf("%w: %s of %s", fin.ErrNoSeries, kind, legalEntity)
	case errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == "invoice_series_range":
		return 0, fmt.Errorf("%w: %s of %s", fin.ErrSeriesExhausted, kind, legalEntity)
	}
	return seq, err
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
		InvoiceID     *string   `json:"invoice_id"`
		InvoiceLineNo *int      `json:"invoice_line_no"`
		By            string    `json:"recorded_by"`
		At            time.Time `json:"recorded_at"`
		EnteredAt     time.Time `json:"entered_at"`
	}
	noteRow struct {
		ID             string     `json:"invoice_id"`
		Kind           string     `json:"kind"`
		Number         string     `json:"number"`
		Seq            int64      `json:"number_seq"`
		Debtor         string     `json:"debtor_party_id"`
		DebtorRevision int64      `json:"debtor_revision"`
		ProfileVersion int        `json:"profile_version"`
		Currency       string     `json:"currency"`
		InvoiceDate    string     `json:"invoice_date"`
		DueDate        *string    `json:"due_date"`
		PaymentDays    *int       `json:"payment_days"`
		PaymentBasis   *string    `json:"payment_basis"`
		TermsVersion   *int       `json:"terms_version"`
		DocumentID     *string    `json:"document_id"`
		DocumentVer    *int       `json:"document_version"`
		DocumentNumber *string    `json:"document_number"`
		MasterNumber   *string    `json:"master_number"`
		DocumentAt     *time.Time `json:"document_issued_at"`
		CreditsID      *string    `json:"credits_invoice_id"`
		Reason         *string    `json:"reason"`
		Total          int64      `json:"total_minor"`
		LocalTotal     int64      `json:"local_total_minor"`
		State          string     `json:"state"`
		By             string     `json:"issued_by"`
		At             time.Time  `json:"issued_at"`
		EnteredAt      time.Time  `json:"entered_at"`
	}
	noteLineRow struct {
		InvoiceID     string  `json:"invoice_id"`
		No            int     `json:"line_no"`
		JobLineNo     int     `json:"job_line_no"`
		SideVersion   *int    `json:"side_version"`
		CreditsLineNo *int    `json:"credits_line_no"`
		ChargeCode    string  `json:"charge_code"`
		Currency      string  `json:"currency"`
		Amount        int64   `json:"amount_minor"`
		Invoice       int64   `json:"invoice_minor"`
		Local         int64   `json:"local_minor"`
		RateEntryNo   *int    `json:"rate_entry_no"`
		RateType      *string `json:"rate_type"`
		Rate          *int64  `json:"rate"`
		Scale         *int    `json:"scale"`
		FromDigits    *int    `json:"from_digits"`
		ToDigits      *int    `json:"to_digits"`
		RateSource    *string `json:"rate_source"`
		TaxVersion    int     `json:"tax_version"`
		Treatment     string  `json:"treatment"`
		RateBP        *int    `json:"rate_bp"`
	}
)

func fxOf(entry *int, rateType *string, rate *int64, scale, from, to *int, source *string) *fin.FxSnapshot {
	if rate == nil {
		return nil
	}
	return &fin.FxSnapshot{EntryNo: deref(entry), Type: deref(rateType), Rate: *rate, Scale: deref(scale), FromDigits: deref(from), ToDigits: deref(to),
		Source: deref(source)}
}

func fxArgs(fx *fin.FxSnapshot) []any {
	if fx == nil {
		return []any{nil, nil, nil, nil, nil, nil, nil}
	}
	return []any{fx.EntryNo, fx.Type, fx.Rate, fx.Scale, fx.FromDigits, fx.ToDigits, fx.Source}
}

func optionalDate(s *string) (time.Time, error) {
	if s == nil {
		return time.Time{}, nil
	}
	return fin.ParseDate(*s)
}

func notesOf(nr []noteRow, lr []noteLineRow) ([]fin.Note, error) {
	var notes []fin.Note
	index := map[string]int{}
	for _, n := range nr {
		invoice, err := fin.ParseDate(n.InvoiceDate)
		if err != nil {
			return nil, err
		}
		due, err := optionalDate(n.DueDate)
		if err != nil {
			return nil, err
		}
		index[n.ID] = len(notes)
		notes = append(notes, fin.Note{ID: n.ID, Kind: n.Kind, Number: n.Number, Seq: n.Seq, DebtorPartyID: n.Debtor, DebtorRevision: n.DebtorRevision,
			ProfileVersion: n.ProfileVersion, Currency: kernel.CurrencyCode(n.Currency), InvoiceDate: invoice, DueDate: due, PaymentDays: deref(n.PaymentDays),
			PaymentBasis: deref(n.PaymentBasis), TermsVersion: deref(n.TermsVersion), Document: fin.DocumentRef{DocumentID: deref(n.DocumentID),
				Version: deref(n.DocumentVer), Number: deref(n.DocumentNumber), MasterNumber: deref(n.MasterNumber),
				IssuedAt: utc(deref(n.DocumentAt))}, CreditsID: deref(n.CreditsID),
			Reason: deref(n.Reason), State: n.State, Total: n.Total, LocalTotal: n.LocalTotal, By: n.By, At: utc(n.At), EnteredAt: utc(n.EnteredAt)})
	}
	for _, l := range lr {
		i, ok := index[l.InvoiceID]
		if !ok {
			return nil, fmt.Errorf("%w: line of note %s", fin.ErrInvalid, l.InvoiceID)
		}
		notes[i].Lines = append(notes[i].Lines, fin.NoteLine{No: l.No, JobLineNo: l.JobLineNo, SideVersion: deref(l.SideVersion),
			CreditsLineNo: deref(l.CreditsLineNo), ChargeCode: l.ChargeCode, Amount: kernel.Money{Currency: kernel.CurrencyCode(l.Currency), Minor: l.Amount},
			Invoice: l.Invoice, Local: l.Local, FX: fxOf(l.RateEntryNo, l.RateType, l.Rate, l.Scale, l.FromDigits, l.ToDigits, l.RateSource),
			Tax: fin.TaxRef{No: l.TaxVersion, Treatment: l.Treatment, RateBP: deref(l.RateBP)}})
	}
	return notes, nil
}

const jobColumns = `j.job_id,j.legal_entity_id,j.shipment_id,j.kind,j.functional_currency,j.book_code,j.customer_party_id,j.plan_version,j.state,j.opened_by,
  j.opened_at,j.entered_at,j.cancel_reason,j.cancelled_by,j.cancelled_at,j.cancel_entered_at,j.revision,`

func jobChildren() string {
	key := "c.tenant_id=j.tenant_id AND c.job_id=j.job_id"
	return children("job_basis", key, "c.basis_no") + `,` + children("charge_line", key, "c.line_no") + `,` +
		children("charge_side_version", key, "c.line_no, c.side DESC, c.version_no") + `,` +
		children("debtor_invoice", key, "c.issued_at, c.entered_at, c.invoice_id") + `,` + children("debtor_invoice_line", key, "c.invoice_id, c.line_no")
}

func scanJob(row pgx.Row, tenant fin.TenantID) (*fin.FreightJob, int64, error) {
	st := fin.JobState{TenantID: tenant}
	var currency string
	var cancelReason, cancelledBy *string
	var cancelledAt, cancelEntered *time.Time
	var revision int64
	var bases, lines, sides, notes, noteLines []byte
	err := row.Scan(&st.JobID, &st.LegalEntityID, &st.ShipmentID, &st.Kind, &currency, &st.BookCode, &st.CustomerPartyID, &st.PlanVersion, &st.State,
		&st.OpenedBy, &st.OpenedAt, &st.EnteredAt, &cancelReason, &cancelledBy, &cancelledAt, &cancelEntered, &revision, &bases, &lines, &sides, &notes,
		&noteLines)
	if err != nil {
		return nil, 0, err
	}
	var br []basisRow
	var lr []lineRow
	var sr []sideRow
	var nr []noteRow
	var nlr []noteLineRow
	if err = errors.Join(json.Unmarshal(bases, &br), json.Unmarshal(lines, &lr), json.Unmarshal(sides, &sr), json.Unmarshal(notes, &nr),
		json.Unmarshal(noteLines, &nlr)); err != nil {
		return nil, 0, err
	}
	if st.Notes, err = notesOf(nr, nlr); err != nil {
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
			InvoiceID: deref(v.InvoiceID), InvoiceLineNo: deref(v.InvoiceLineNo), By: v.By, At: utc(v.At), EnteredAt: utc(v.EnteredAt),
			FX: fxOf(v.RateEntryNo, v.RateType, v.Rate, v.Scale, v.FromDigits, v.ToDigits, v.RateSource)}
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

// BilledTo lists the jobs of a legal entity with notes to a debtor.
func (s JobStore) BilledTo(ctx context.Context, tenant fin.TenantID, legalEntity, debtor string) ([]*fin.FreightJob, error) {
	out, _, err := s.list(ctx, tenant, `j.legal_entity_id=$2 AND EXISTS (SELECT 1 FROM cargo_fin.debtor_invoice n WHERE n.tenant_id=j.tenant_id
      AND n.job_id=j.job_id AND n.debtor_party_id=$3)`, legalEntity, debtor)
	return out, err
}

// OfNote reads the job of a note.
func (s JobStore) OfNote(ctx context.Context, tenant fin.TenantID, invoiceID string) (*fin.FreightJob, int64, error) {
	out, revisions, err := s.list(ctx, tenant, `EXISTS (SELECT 1 FROM cargo_fin.debtor_invoice n WHERE n.tenant_id=j.tenant_id AND n.job_id=j.job_id
      AND n.invoice_id=$2)`, invoiceID)
	if err != nil {
		return nil, 0, err
	}
	if len(out) != 1 {
		return nil, 0, fmt.Errorf("%w: note %s", bentorepo.ErrNotFound, invoiceID)
	}
	return out[0], revisions[0], nil
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
	notes, cancelled := j.AddedNotes()
	for _, n := range notes {
		if err := insertNote(ctx, q, st, n); err != nil {
			return err
		}
	}
	for _, n := range cancelled {
		if _, err := q.Exec(ctx, `UPDATE cargo_fin.debtor_invoice SET state=$3 WHERE tenant_id=$1 AND invoice_id=$2`, string(st.TenantID), n.ID,
			n.State); err != nil {
			return err
		}
	}
	for i, v := range sides {
		args := append([]any{lineOf[i], v.Side, v.No, orNull(v.PartyID), v.PartyRevision, string(v.Amount.Currency), v.Amount.Minor, v.Local}, fxArgs(v.FX)...)
		args = append(args, v.Status, orNull(v.Reason), orNull(v.InvoiceID), orNull(v.InvoiceLineNo), v.By, v.At, v.EnteredAt)
		if err := exec(`INSERT INTO cargo_fin.charge_side_version (tenant_id,job_id,line_no,side,version_no,party_id,party_revision,currency,amount_minor,
            local_minor,rate_entry_no,rate_type,rate,scale,from_digits,to_digits,rate_source,status,reason,invoice_id,invoice_line_no,recorded_by,recorded_at,
            entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)`, args...); err != nil {
			return err
		}
	}
	return nil
}

func dateOrNull(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Format(time.DateOnly)
}

// insertNote writes a note and its lines; a duplicate id, number or billed side is a conflict.
func insertNote(ctx context.Context, q bentopg.Executor, st fin.JobState, n fin.Note) error {
	var days, basis any
	if n.Kind == fin.KindDebitNote {
		days, basis = n.PaymentDays, n.PaymentBasis
	}
	_, err := q.Exec(ctx, `INSERT INTO cargo_fin.debtor_invoice (tenant_id,invoice_id,kind,job_id,legal_entity_id,number,number_seq,debtor_party_id,
        debtor_revision,profile_version,currency,invoice_date,due_date,payment_days,payment_basis,terms_version,document_id,document_version,document_number,
        master_number,document_issued_at,credits_invoice_id,reason,total_minor,local_total_minor,state,issued_by,issued_at,entered_at)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29)`,
		string(st.TenantID), n.ID, n.Kind, st.JobID, st.LegalEntityID, n.Number, n.Seq, n.DebtorPartyID, n.DebtorRevision, n.ProfileVersion,
		string(n.Currency), n.InvoiceDate.Format(time.DateOnly), dateOrNull(n.DueDate), days, basis, orNull(n.TermsVersion), orNull(n.Document.DocumentID),
		orNull(n.Document.Version), orNull(n.Document.Number), orNull(n.Document.MasterNumber), orNull(n.Document.IssuedAt), orNull(n.CreditsID),
		orNull(n.Reason), n.Total,
		n.LocalTotal, n.State, n.By, n.At, n.EnteredAt)
	if err = noteConflict(err, n); err != nil {
		return err
	}
	for _, l := range n.Lines {
		var credits any
		if l.CreditsLineNo > 0 {
			credits = n.CreditsID
		}
		args := append([]any{string(st.TenantID), n.ID, l.No, st.JobID, l.JobLineNo, orNull(l.SideVersion), credits, orNull(l.CreditsLineNo),
			l.ChargeCode, string(l.Amount.Currency), l.Amount.Minor, l.Invoice, l.Local}, fxArgs(l.FX)...)
		args = append(args, l.Tax.No, l.Tax.Treatment, orNull(l.Tax.RateBP))
		_, err = q.Exec(ctx, `INSERT INTO cargo_fin.debtor_invoice_line (tenant_id,invoice_id,line_no,job_id,job_line_no,side_version,credits_invoice_id,
            credits_line_no,charge_code,currency,amount_minor,invoice_minor,local_minor,rate_entry_no,rate_type,rate,scale,from_digits,to_digits,rate_source,
            tax_version,treatment,rate_bp) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)`, args...)
		if err = noteConflict(err, n); err != nil {
			return err
		}
	}
	return nil
}

func noteConflict(err error, n fin.Note) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == "debtor_invoice_kind" {
		return fmt.Errorf("%w: note %s breaks the rules of its kind: %v", fin.ErrInvalid, n.ID, pgErr.Message)
	}
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case "debtor_invoice_pkey":
			return fmt.Errorf("%w: note %s", bentorepo.ErrAlreadyExists, n.ID)
		case "debtor_invoice_number", "debtor_invoice_line_side_once", "debtor_invoice_line_reversed_once":
			return fmt.Errorf("%w: note %s (%s)", bentorepo.ErrConflict, n.ID, pgErr.ConstraintName)
		}
	}
	return err
}

// ProfileStore loads and writes billing profiles: a profile is its versions, and a version written twice at once is
// a conflict on the key.
type ProfileStore struct{ DB *bentopg.DB }

var _ app.ProfileStore = ProfileStore{}

func readProfile(ctx context.Context, q bentopg.Querier, tenant fin.TenantID, debtor string) (*fin.BillingProfile, bool, error) {
	rows, err := q.Query(ctx, `SELECT version_no,buyer_kind,title,taxpayer_number,party_revision,registered_address,registered_phone,bank_name,bank_account,
      evidence_ref,set_by,set_at,entered_at FROM cargo_fin.billing_profile_version WHERE tenant_id=$1 AND debtor_party_id=$2 ORDER BY version_no`,
		string(tenant), debtor)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	st := fin.ProfileState{TenantID: tenant, DebtorPartyID: debtor}
	for rows.Next() {
		var v fin.ProfileVersion
		var taxpayer, address, phone, bank, account *string
		if err = rows.Scan(&v.No, &v.BuyerKind, &v.Title, &taxpayer, &v.PartyRevision, &address, &phone, &bank, &account, &v.EvidenceRef, &v.By, &v.At,
			&v.EnteredAt); err != nil {
			return nil, false, err
		}
		v.TaxpayerNumber, v.RegisteredAddress, v.RegisteredPhone, v.BankName, v.BankAccount = deref(taxpayer), deref(address), deref(phone), deref(bank),
			deref(account)
		v.At, v.EnteredAt = v.At.UTC(), v.EnteredAt.UTC()
		st.Versions = append(st.Versions, v)
	}
	if err = rows.Err(); err != nil {
		return nil, false, err
	}
	if len(st.Versions) == 0 {
		return nil, false, nil
	}
	b, err := fin.RestoreProfile(st)
	return b, err == nil, err
}

func (s ProfileStore) Load(ctx context.Context, tenant fin.TenantID, debtor string) (*fin.BillingProfile, bool, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, false, err
	}
	return readProfile(ctx, q, tenant, debtor)
}

func (s ProfileStore) Read(ctx context.Context, tenant fin.TenantID, debtor string) (*fin.BillingProfile, bool, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, false, err
	}
	return readProfile(ctx, q, tenant, debtor)
}

func (s ProfileStore) Save(ctx context.Context, b *fin.BillingProfile) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	st := b.State()
	for _, v := range b.Added() {
		_, err = q.Exec(ctx, `INSERT INTO cargo_fin.billing_profile_version (tenant_id,debtor_party_id,version_no,buyer_kind,title,taxpayer_number,party_revision,
            registered_address,registered_phone,bank_name,bank_account,evidence_ref,set_by,set_at,entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
			string(st.TenantID), st.DebtorPartyID, v.No, v.BuyerKind, v.Title, orNull(v.TaxpayerNumber), v.PartyRevision, orNull(v.RegisteredAddress),
			orNull(v.RegisteredPhone), orNull(v.BankName), orNull(v.BankAccount), v.EvidenceRef, v.By, v.At, v.EnteredAt)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return fmt.Errorf("%w: billing profile %s version %d", bentorepo.ErrConflict, st.DebtorPartyID, v.No)
		}
		if err != nil {
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
	var number *string
	err = q.QueryRow(ctx, `SELECT request_sha256,result_id,result_revision,result_version,result_number FROM cargo_fin.command_receipt
      WHERE tenant_id=$1 AND command_id=$2`, string(tenant), id).Scan(&r.RequestHash, &r.SubjectID, &r.NewRevision, &version, &number)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Receipt{}, false, nil
	}
	if err != nil {
		return app.Receipt{}, false, err
	}
	r.Version, r.Number = deref(version), deref(number)
	return r, true, nil
}

func (s CommandReceipts) Save(ctx context.Context, tenant fin.TenantID, r app.Receipt, kind string) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_fin.command_receipt (tenant_id,command_id,request_sha256,result_type,result_id,result_revision,result_version,
      result_number) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, string(tenant), r.CommandID, r.RequestHash, kind, r.SubjectID, r.NewRevision, r.Version, orNull(r.Number))
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
		ShipmentID: e.ShipmentID, DebtorPartyID: e.DebtorPartyID, LineNo: e.LineNo, Side: e.Side, JobID: e.JobID})
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
	case "finance_policy_consistent", "freight_job_consistent", "credit_account_consistent", "fin_records_with_parent", "freight_job_invoices_consistent",
		"invoice_series_consistent", "billing_profile_consistent":
		return true
	}
	return false
}

// TaxInUse reports whether a tax treatment failed because a debit note bills its charge code at or after its date.
func TaxInUse(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == "charge_tax_treatment_in_use"
}

// RateInUse reports whether ending a rate entry failed because a charge was converted at it after the new end.
func RateInUse(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == "exchange_rate_entry_in_use"
}
