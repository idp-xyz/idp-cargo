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

// PoolStore loads and writes consol cost pools (FIN V0.4, FB3).
type PoolStore struct{ DB *bentopg.DB }

var _ app.PoolStore = PoolStore{}

type (
	poolLineRow struct {
		No          int       `json:"line_no"`
		ChargeCode  string    `json:"charge_code"`
		Description *string   `json:"description"`
		SupplierRef *string   `json:"supplier_ref"`
		Method      string    `json:"method"`
		By          string    `json:"recorded_by"`
		At          time.Time `json:"recorded_at"`
		EnteredAt   time.Time `json:"entered_at"`
	}
	poolVersionRow struct {
		LineNo           int       `json:"line_no"`
		No               int       `json:"version_no"`
		CreditorPartyID  *string   `json:"creditor_party_id"`
		CreditorRevision int64     `json:"creditor_revision"`
		Currency         string    `json:"currency"`
		Amount           int64     `json:"amount_minor"`
		Local            int64     `json:"local_minor"`
		RateEntryNo      *int      `json:"rate_entry_no"`
		RateType         *string   `json:"rate_type"`
		Rate             *int64    `json:"rate"`
		Scale            *int      `json:"scale"`
		FromDigits       *int      `json:"from_digits"`
		ToDigits         *int      `json:"to_digits"`
		RateSource       *string   `json:"rate_source"`
		Status           string    `json:"status"`
		Reason           *string   `json:"reason"`
		By               string    `json:"recorded_by"`
		At               time.Time `json:"recorded_at"`
		EnteredAt        time.Time `json:"entered_at"`
	}
	apportionmentRow struct {
		No            int       `json:"apportionment_no"`
		BasisRevision int64     `json:"basis_revision"`
		Stage         string    `json:"stage"`
		By            string    `json:"made_by"`
		At            time.Time `json:"made_at"`
		EnteredAt     time.Time `json:"entered_at"`
	}
	apportionedLineRow struct {
		ApportionmentNo int     `json:"apportionment_no"`
		LineNo          int     `json:"line_no"`
		LineVersion     int     `json:"line_version"`
		Method          string  `json:"method"`
		Reason          *string `json:"reason"`
	}
	shareRow struct {
		ApportionmentNo int    `json:"apportionment_no"`
		LineNo          int    `json:"line_no"`
		No              int    `json:"share_no"`
		ShipmentID      string `json:"shipment_id"`
		JobID           string `json:"job_id"`
		Quantity        int64  `json:"quantity"`
		Unit            string `json:"unit"`
		Amount          int64  `json:"amount_minor"`
		Local           int64  `json:"local_minor"`
	}
)

const poolColumns = `p.pool_id,p.consolidation_id,p.legal_entity_id,p.mode,p.functional_currency,p.book_code,p.state,p.opened_by,p.opened_at,p.entered_at,
  p.revision,`

func poolChildren() string {
	key := "c.tenant_id=p.tenant_id AND c.pool_id=p.pool_id"
	return children("pool_cost_line", key, "c.line_no") + `,` + children("pool_cost_version", key, "c.line_no, c.version_no") + `,` +
		children("pool_apportionment", key, "c.apportionment_no") + `,` + children("pool_apportionment_line", key, "c.apportionment_no, c.line_no") + `,` +
		children("pool_apportionment_share", key, "c.apportionment_no, c.line_no, c.share_no")
}

func scanPool(row pgx.Row, tenant fin.TenantID) (*fin.ConsolCostPool, int64, error) {
	st := fin.PoolState{TenantID: tenant}
	var currency string
	var revision int64
	var lines, versions, apportionments, apportionedLines, shares []byte
	err := row.Scan(&st.PoolID, &st.ConsolidationID, &st.LegalEntityID, &st.Mode, &currency, &st.BookCode, &st.State, &st.OpenedBy, &st.OpenedAt,
		&st.EnteredAt, &revision, &lines, &versions, &apportionments, &apportionedLines, &shares)
	if err != nil {
		return nil, 0, err
	}
	var lr []poolLineRow
	var vr []poolVersionRow
	var ar []apportionmentRow
	var alr []apportionedLineRow
	var sr []shareRow
	if err = errors.Join(json.Unmarshal(lines, &lr), json.Unmarshal(versions, &vr), json.Unmarshal(apportionments, &ar),
		json.Unmarshal(apportionedLines, &alr), json.Unmarshal(shares, &sr)); err != nil {
		return nil, 0, err
	}
	st.FunctionalCurrency, st.OpenedAt, st.EnteredAt = kernel.CurrencyCode(currency), st.OpenedAt.UTC(), st.EnteredAt.UTC()
	for _, l := range lr {
		st.Lines = append(st.Lines, fin.PoolCostLine{No: l.No, ChargeCode: l.ChargeCode, Description: deref(l.Description),
			SupplierRef: deref(l.SupplierRef), Method: l.Method, By: l.By, At: utc(l.At), EnteredAt: utc(l.EnteredAt)})
	}
	for _, v := range vr {
		if v.LineNo < 1 || v.LineNo > len(st.Lines) {
			return nil, 0, fmt.Errorf("%w: version of pool cost line %d", fin.ErrInvalid, v.LineNo)
		}
		st.Lines[v.LineNo-1].Versions = append(st.Lines[v.LineNo-1].Versions, fin.PoolCostVersion{No: v.No, CreditorPartyID: deref(v.CreditorPartyID),
			CreditorRevision: v.CreditorRevision, Amount: kernel.Money{Currency: kernel.CurrencyCode(v.Currency), Minor: v.Amount}, Local: v.Local,
			FX: fxOf(v.RateEntryNo, v.RateType, v.Rate, v.Scale, v.FromDigits, v.ToDigits, v.RateSource), Status: v.Status, Reason: deref(v.Reason),
			By: v.By, At: utc(v.At), EnteredAt: utc(v.EnteredAt)})
	}
	for _, a := range ar {
		st.Apportionments = append(st.Apportionments, fin.Apportionment{No: a.No, BasisRevision: a.BasisRevision, Stage: a.Stage, By: a.By, At: utc(a.At),
			EnteredAt: utc(a.EnteredAt)})
	}
	for _, l := range alr {
		if l.ApportionmentNo < 1 || l.ApportionmentNo > len(st.Apportionments) {
			return nil, 0, fmt.Errorf("%w: line of apportionment %d", fin.ErrInvalid, l.ApportionmentNo)
		}
		a := &st.Apportionments[l.ApportionmentNo-1]
		a.Lines = append(a.Lines, fin.LineApportionment{LineNo: l.LineNo, LineVersion: l.LineVersion, Method: l.Method, Reason: deref(l.Reason)})
	}
	for _, s := range sr {
		if s.ApportionmentNo < 1 || s.ApportionmentNo > len(st.Apportionments) {
			return nil, 0, fmt.Errorf("%w: share of apportionment %d", fin.ErrInvalid, s.ApportionmentNo)
		}
		a := &st.Apportionments[s.ApportionmentNo-1]
		placed := false
		for i := range a.Lines {
			if a.Lines[i].LineNo == s.LineNo {
				a.Lines[i].Shares = append(a.Lines[i].Shares, fin.Share{No: s.No, ShipmentID: s.ShipmentID, JobID: s.JobID, Quantity: s.Quantity,
					Unit: s.Unit, Amount: s.Amount, Local: s.Local})
				placed = true
			}
		}
		if !placed {
			return nil, 0, fmt.Errorf("%w: share of apportionment %d line %d", fin.ErrInvalid, s.ApportionmentNo, s.LineNo)
		}
	}
	p, err := fin.RestorePool(st)
	return p, revision, err
}

func (s PoolStore) read(ctx context.Context, q bentopg.Querier, tenant fin.TenantID, id string) (*fin.ConsolCostPool, int64, error) {
	p, revision, err := scanPool(q.QueryRow(ctx, `SELECT `+poolColumns+poolChildren()+` FROM cargo_fin.consol_cost_pool p
      WHERE p.tenant_id=$1 AND p.pool_id=$2`, string(tenant), id), tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: consol cost pool %s", bentorepo.ErrNotFound, id)
	}
	return p, revision, err
}

// Load locks the pool row and reads the pool.
func (s PoolStore) Load(ctx context.Context, tenant fin.TenantID, id string) (*fin.ConsolCostPool, int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	var one int
	err = q.QueryRow(ctx, `SELECT 1 FROM cargo_fin.consol_cost_pool WHERE tenant_id=$1 AND pool_id=$2 FOR NO KEY UPDATE`, string(tenant), id).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: consol cost pool %s", bentorepo.ErrNotFound, id)
	}
	if err != nil {
		return nil, 0, err
	}
	return s.read(ctx, q, tenant, id)
}

func (s PoolStore) Read(ctx context.Context, tenant fin.TenantID, id string) (*fin.ConsolCostPool, int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	return s.read(ctx, q, tenant, id)
}

// ForConsolidation lists a consolidation's pools in the order opened.
func (s PoolStore) ForConsolidation(ctx context.Context, tenant fin.TenantID, consolidation string) ([]*fin.ConsolCostPool, []int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, nil, err
	}
	rows, err := q.Query(ctx, `SELECT `+poolColumns+poolChildren()+` FROM cargo_fin.consol_cost_pool p
      WHERE p.tenant_id=$1 AND p.consolidation_id=$2 ORDER BY p.opened_at, p.pool_id`, string(tenant), consolidation)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []*fin.ConsolCostPool
	var revisions []int64
	for rows.Next() {
		p, revision, err := scanPool(rows, tenant)
		if err != nil {
			return nil, nil, err
		}
		out, revisions = append(out, p), append(revisions, revision)
	}
	return out, revisions, rows.Err()
}

func (s PoolStore) Insert(ctx context.Context, p *fin.ConsolCostPool) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := p.State()
	var revision int64
	err = q.QueryRow(ctx, `INSERT INTO cargo_fin.consol_cost_pool (tenant_id,pool_id,consolidation_id,legal_entity_id,mode,functional_currency,book_code,
        state,opened_by,opened_at,entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING revision`,
		string(st.TenantID), st.PoolID, st.ConsolidationID, st.LegalEntityID, st.Mode, string(st.FunctionalCurrency), st.BookCode, st.State, st.OpenedBy,
		st.OpenedAt, st.EnteredAt).Scan(&revision)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case "consol_cost_pool_pkey":
			return 0, fmt.Errorf("%w: consol cost pool %s", bentorepo.ErrAlreadyExists, st.PoolID)
		case "consol_cost_pool_one_per_consol":
			return 0, fmt.Errorf("%w: consolidation %s has a pool of %s already", bentorepo.ErrAlreadyExists, st.ConsolidationID, st.LegalEntityID)
		}
	}
	if err != nil {
		return 0, err
	}
	return revision, writePool(ctx, q, p)
}

func (s PoolStore) Save(ctx context.Context, p *fin.ConsolCostPool, expected int64) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := p.State()
	var revision int64
	err = q.QueryRow(ctx, `UPDATE cargo_fin.consol_cost_pool SET state=$3,revision=revision+1,updated_at=now()
      WHERE tenant_id=$1 AND pool_id=$2 AND revision=$4 RETURNING revision`, string(st.TenantID), st.PoolID, st.State, expected).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: consol cost pool %s", bentorepo.ErrConflict, st.PoolID)
	}
	if err != nil {
		return 0, err
	}
	return revision, writePool(ctx, q, p)
}

func writePool(ctx context.Context, q bentopg.Executor, p *fin.ConsolCostPool) error {
	st := p.State()
	lines, versions, lineOf, apportionments := p.Added()
	exec := func(sql string, args ...any) error {
		_, err := q.Exec(ctx, sql, append([]any{string(st.TenantID), st.PoolID}, args...)...)
		return err
	}
	for _, l := range lines {
		if err := exec(`INSERT INTO cargo_fin.pool_cost_line (tenant_id,pool_id,line_no,charge_code,description,supplier_ref,method,recorded_by,recorded_at,
            entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, l.No, l.ChargeCode, orNull(l.Description), orNull(l.SupplierRef), l.Method, l.By, l.At,
			l.EnteredAt); err != nil {
			return err
		}
	}
	for i, v := range versions {
		args := append([]any{lineOf[i], v.No, orNull(v.CreditorPartyID), v.CreditorRevision, string(v.Amount.Currency), v.Amount.Minor, v.Local},
			fxArgs(v.FX)...)
		args = append(args, v.Status, orNull(v.Reason), v.By, v.At, v.EnteredAt)
		if err := exec(`INSERT INTO cargo_fin.pool_cost_version (tenant_id,pool_id,line_no,version_no,creditor_party_id,creditor_revision,currency,
            amount_minor,local_minor,rate_entry_no,rate_type,rate,scale,from_digits,to_digits,rate_source,status,reason,recorded_by,recorded_at,entered_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)`, args...); err != nil {
			return err
		}
	}
	for _, a := range apportionments {
		if err := exec(`INSERT INTO cargo_fin.pool_apportionment (tenant_id,pool_id,apportionment_no,basis_revision,stage,made_by,made_at,entered_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, a.No, a.BasisRevision, a.Stage, a.By, a.At, a.EnteredAt); err != nil {
			return err
		}
		for _, la := range a.Lines {
			if err := exec(`INSERT INTO cargo_fin.pool_apportionment_line (tenant_id,pool_id,apportionment_no,line_no,line_version,method,reason)
                VALUES ($1,$2,$3,$4,$5,$6,$7)`, a.No, la.LineNo, la.LineVersion, la.Method, orNull(la.Reason)); err != nil {
				return err
			}
			for _, sh := range la.Shares {
				if err := exec(`INSERT INTO cargo_fin.pool_apportionment_share (tenant_id,pool_id,apportionment_no,line_no,share_no,shipment_id,job_id,
                    quantity,unit,amount_minor,local_minor) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, a.No, la.LineNo, sh.No, sh.ShipmentID, sh.JobID,
					sh.Quantity, orNull(sh.Unit), sh.Amount, sh.Local); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// SharesOfJob lists a job's shares under each pool line's latest apportionment, by pool and line.
func (s PoolStore) SharesOfJob(ctx context.Context, tenant fin.TenantID, job string) ([]app.PoolShare, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT sh.pool_id, sh.line_no, al.line_version, cl.charge_code, al.method, a.stage, sh.quantity, coalesce(sh.unit,''),
        v.currency, sh.amount_minor, sh.local_minor
      FROM cargo_fin.pool_apportionment_share sh
      JOIN cargo_fin.pool_apportionment_line al ON al.tenant_id=sh.tenant_id AND al.pool_id=sh.pool_id
        AND al.apportionment_no=sh.apportionment_no AND al.line_no=sh.line_no
      JOIN cargo_fin.pool_apportionment a ON a.tenant_id=sh.tenant_id AND a.pool_id=sh.pool_id AND a.apportionment_no=sh.apportionment_no
      JOIN cargo_fin.pool_cost_line cl ON cl.tenant_id=sh.tenant_id AND cl.pool_id=sh.pool_id AND cl.line_no=sh.line_no
      JOIN cargo_fin.pool_cost_version v ON v.tenant_id=sh.tenant_id AND v.pool_id=sh.pool_id AND v.line_no=al.line_no AND v.version_no=al.line_version
      WHERE sh.tenant_id=$1 AND sh.job_id=$2 AND sh.apportionment_no = (SELECT max(l2.apportionment_no) FROM cargo_fin.pool_apportionment_line l2
        WHERE l2.tenant_id=sh.tenant_id AND l2.pool_id=sh.pool_id AND l2.line_no=sh.line_no)
      ORDER BY sh.pool_id, sh.line_no`, string(tenant), job)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []app.PoolShare
	for rows.Next() {
		var sh app.PoolShare
		var currency string
		var amount int64
		if err := rows.Scan(&sh.PoolID, &sh.LineNo, &sh.LineVersion, &sh.ChargeCode, &sh.Method, &sh.Stage, &sh.Quantity, &sh.Unit, &currency, &amount,
			&sh.Local); err != nil {
			return nil, err
		}
		sh.Amount = kernel.Money{Currency: kernel.CurrencyCode(currency), Minor: amount}
		out = append(out, sh)
	}
	return out, rows.Err()
}
