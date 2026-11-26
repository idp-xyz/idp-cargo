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
		Source      string    `json:"source"`
		BasisID     *string   `json:"basis_id"`
		BasisLineNo *int      `json:"basis_line_no"`
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
		LoadUnitNo       *int      `json:"load_unit_no"` // since V2.104; NULL is the whole consolidation
	}
	apportionmentRow struct {
		No            int       `json:"apportionment_no"`
		BasisRevision int64     `json:"basis_revision"`
		Stage         string    `json:"stage"`
		By            string    `json:"made_by"`
		At            time.Time `json:"made_at"`
		EnteredAt     time.Time `json:"entered_at"`
		PreviewNo     *int      `json:"preview_no"`
		// SettingsVersion is NULL on a row written before V2.97
		SettingsVersion *int `json:"settings_version"`
	}
	previewRow struct {
		No                int             `json:"preview_no"`
		BasisRevision     int64           `json:"basis_revision"`
		Stage             string          `json:"stage"`
		Figures           json.RawMessage `json:"figures"`
		Fingerprint       string          `json:"fingerprint"`
		FingerprintSHA256 string          `json:"fingerprint_sha256"`
		By                string          `json:"made_by"`
		At                time.Time       `json:"made_at"`
		EnteredAt         time.Time       `json:"entered_at"`
		SettingsVersion   *int            `json:"settings_version"`
	}
	apportionedLineRow struct {
		ApportionmentNo int     `json:"apportionment_no"`
		LineNo          int     `json:"line_no"`
		LineVersion     int     `json:"line_version"`
		Method          string  `json:"method"`
		Reason          *string `json:"reason"`
		Amount          *int64  `json:"amount_minor"` // NULL on lines of FIN V0.4: they divided their version's figures
		Local           *int64  `json:"local_minor"`
		ToAllocate      int64   `json:"to_allocate_minor"`
		ToAllocateLocal int64   `json:"to_allocate_local_minor"`
	}
	attributionRow struct {
		ApportionmentNo int       `json:"apportionment_no"`
		LineNo          int       `json:"line_no"`
		ShipmentID      string    `json:"shipment_id"`
		JobID           string    `json:"job_id"`
		Loss            int64     `json:"loss_minor"`
		LossLocal       int64     `json:"loss_local_minor"`
		Exception       int64     `json:"exception_minor"`
		ExceptionLocal  int64     `json:"exception_local_minor"`
		Party           *string   `json:"party"`
		PartyReference  *string   `json:"party_reference"`
		Voucher         string    `json:"voucher"`
		Reason          *string   `json:"reason"`
		DepartedAt      time.Time `json:"departed_at"`
		Carried         bool      `json:"carried"`
		ByDefault       bool      `json:"by_default"`
		ByRatio         bool      `json:"by_ratio"`
	}
	// figureLine and figureShare are a preview's figures as stored: one JSON array of the lines it divided.
	figureLine struct {
		LineNo          int              `json:"line_no"`
		LineVersion     int              `json:"line_version"`
		Method          string           `json:"method"`
		Reason          string           `json:"reason,omitempty"`
		Amount          int64            `json:"amount_minor"`
		Local           int64            `json:"local_minor"`
		Shares          []figureShare    `json:"shares"`
		ToAllocate      int64            `json:"to_allocate_minor"`
		ToAllocateLocal int64            `json:"to_allocate_local_minor"`
		Attributions    []attributionRow `json:"attributions"`
	}
	figureShare struct {
		No         int    `json:"share_no"`
		ShipmentID string `json:"shipment_id"`
		JobID      string `json:"job_id"`
		Used       bool   `json:"used"`
		Quantity   int64  `json:"quantity"`
		Unit       string `json:"unit,omitempty"`
		Amount     int64  `json:"amount_minor"`
		Local      int64  `json:"local_minor"`
		InUnit     *bool  `json:"in_unit,omitempty"` // since V2.104, on a line of one container
	}
	closureRow struct {
		No              int        `json:"closure_no"`
		By              string     `json:"closed_by"`
		At              time.Time  `json:"closed_at"`
		EnteredAt       time.Time  `json:"entered_at"`
		ReopenedBy      *string    `json:"reopened_by"`
		ReopenReason    *string    `json:"reopen_reason"`
		ReopenedAt      *time.Time `json:"reopened_at"`
		ReopenEnteredAt *time.Time `json:"reopen_entered_at"`
		SettingsVersion *int       `json:"settings_version"`
	}
	shareRow struct {
		ApportionmentNo int    `json:"apportionment_no"`
		LineNo          int    `json:"line_no"`
		No              int    `json:"share_no"`
		ShipmentID      string `json:"shipment_id"`
		JobID           string `json:"job_id"`
		Used            bool   `json:"used"`
		Quantity        int64  `json:"quantity"`
		Unit            string `json:"unit"`
		Amount          int64  `json:"amount_minor"`
		Local           int64  `json:"local_minor"`
		InUnit          *bool  `json:"in_unit"` // since V2.104, on a line of one container
	}
)

const poolColumns = `p.pool_id,p.consolidation_id,p.legal_entity_id,p.mode,p.functional_currency,p.book_code,p.state,p.opened_by,p.opened_at,p.entered_at,
  p.revision,`

func poolChildren() string {
	key := "c.tenant_id=p.tenant_id AND c.pool_id=p.pool_id"
	return children("pool_cost_line", key, "c.line_no") + `,` + children("pool_cost_version", key, "c.line_no, c.version_no") + `,` +
		children("pool_apportionment", key, "c.apportionment_no") + `,` + children("pool_apportionment_preview", key, "c.preview_no") + `,` + children("pool_apportionment_line", key, "c.apportionment_no, c.line_no") + `,` +
		children("pool_apportionment_share", key, "c.apportionment_no, c.line_no, c.share_no") + `,` +
		children("pool_apportionment_attribution", key, "c.apportionment_no, c.line_no, c.shipment_id") + `,` +
		children("consol_cost_pool_closure", key, "c.closure_no")
}

func scanPool(row pgx.Row, tenant fin.TenantID) (*fin.ConsolCostPool, int64, error) {
	st := fin.PoolState{TenantID: tenant}
	var currency string
	var revision int64
	var lines, versions, apportionments, previews, apportionedLines, shares, attributions, closures []byte
	err := row.Scan(&st.PoolID, &st.ConsolidationID, &st.LegalEntityID, &st.Mode, &currency, &st.BookCode, &st.State, &st.OpenedBy, &st.OpenedAt,
		&st.EnteredAt, &revision, &lines, &versions, &apportionments, &previews, &apportionedLines, &shares, &attributions, &closures)
	if err != nil {
		return nil, 0, err
	}
	var lr []poolLineRow
	var vr []poolVersionRow
	var ar []apportionmentRow
	var pr []previewRow
	var alr []apportionedLineRow
	var sr []shareRow
	var xr []attributionRow
	var cr []closureRow
	if err = errors.Join(json.Unmarshal(lines, &lr), json.Unmarshal(versions, &vr), json.Unmarshal(apportionments, &ar),
		json.Unmarshal(previews, &pr), json.Unmarshal(apportionedLines, &alr), json.Unmarshal(shares, &sr), json.Unmarshal(attributions, &xr), json.Unmarshal(closures, &cr)); err != nil {
		return nil, 0, err
	}
	for _, c := range cr {
		st.Closures = append(st.Closures, fin.PoolClosure{No: c.No, By: c.By, At: utc(c.At), EnteredAt: utc(c.EnteredAt), ReopenedBy: deref(c.ReopenedBy),
			ReopenReason: deref(c.ReopenReason), ReopenedAt: utc(deref(c.ReopenedAt)), ReopenEnteredAt: utc(deref(c.ReopenEnteredAt)),
			SettingsVersion: deref(c.SettingsVersion)})
	}
	st.FunctionalCurrency, st.OpenedAt, st.EnteredAt = kernel.CurrencyCode(currency), st.OpenedAt.UTC(), st.EnteredAt.UTC()
	for _, l := range lr {
		line := fin.PoolCostLine{No: l.No, ChargeCode: l.ChargeCode, Description: deref(l.Description), SupplierRef: deref(l.SupplierRef),
			Method: l.Method, Source: l.Source, BasisID: deref(l.BasisID), By: l.By, At: utc(l.At), EnteredAt: utc(l.EnteredAt)}
		if l.BasisLineNo != nil {
			line.BasisLineNo = *l.BasisLineNo
		}
		st.Lines = append(st.Lines, line)
	}
	for _, v := range vr {
		if v.LineNo < 1 || v.LineNo > len(st.Lines) {
			return nil, 0, fmt.Errorf("%w: version of pool cost line %d", fin.ErrInvalid, v.LineNo)
		}
		st.Lines[v.LineNo-1].Versions = append(st.Lines[v.LineNo-1].Versions, fin.PoolCostVersion{No: v.No, CreditorPartyID: deref(v.CreditorPartyID),
			CreditorRevision: v.CreditorRevision, Amount: kernel.Money{Currency: kernel.CurrencyCode(v.Currency), Minor: v.Amount}, Local: v.Local,
			FX: fxOf(v.RateEntryNo, v.RateType, v.Rate, v.Scale, v.FromDigits, v.ToDigits, v.RateSource), Status: v.Status, Reason: deref(v.Reason),
			By: v.By, At: utc(v.At), EnteredAt: utc(v.EnteredAt), LoadUnitNo: deref(v.LoadUnitNo)})
	}
	for _, a := range ar {
		st.Apportionments = append(st.Apportionments, fin.Apportionment{No: a.No, BasisRevision: a.BasisRevision, Stage: a.Stage, By: a.By, At: utc(a.At),
			EnteredAt: utc(a.EnteredAt), PreviewNo: deref(a.PreviewNo), SettingsVersion: deref(a.SettingsVersion)})
	}
	for _, v := range pr {
		var figures []figureLine
		if err = json.Unmarshal(v.Figures, &figures); err != nil {
			return nil, 0, err
		}
		preview := fin.ApportionmentPreview{No: v.No, BasisRevision: v.BasisRevision, Stage: v.Stage, By: v.By, At: utc(v.At), EnteredAt: utc(v.EnteredAt),
			Fingerprint: v.Fingerprint, FingerprintSHA256: v.FingerprintSHA256, SettingsVersion: deref(v.SettingsVersion)}
		for _, f := range figures {
			la := fin.LineApportionment{LineNo: f.LineNo, LineVersion: f.LineVersion, Method: f.Method, Reason: f.Reason, Amount: f.Amount, Local: f.Local,
				ToAllocate: f.ToAllocate, ToAllocateLocal: f.ToAllocateLocal}
			for _, x := range f.Attributions {
				la.Attributions = append(la.Attributions, attributionOf(x))
			}
			for _, sh := range f.Shares {
				la.Shares = append(la.Shares, fin.Share{No: sh.No, ShipmentID: sh.ShipmentID, JobID: sh.JobID, Used: sh.Used, Quantity: sh.Quantity, Unit: sh.Unit,
					Amount: sh.Amount, Local: sh.Local, InUnit: sh.InUnit})
			}
			preview.Lines = append(preview.Lines, la)
		}
		st.Previews = append(st.Previews, preview)
	}
	for _, l := range alr {
		if l.ApportionmentNo < 1 || l.ApportionmentNo > len(st.Apportionments) {
			return nil, 0, fmt.Errorf("%w: line of apportionment %d", fin.ErrInvalid, l.ApportionmentNo)
		}
		a := &st.Apportionments[l.ApportionmentNo-1]
		if l.LineNo < 1 || l.LineNo > len(st.Lines) || l.LineVersion < 1 || l.LineVersion > len(st.Lines[l.LineNo-1].Versions) {
			return nil, 0, fmt.Errorf("%w: apportionment %d of line %d version %d", fin.ErrInvalid, l.ApportionmentNo, l.LineNo, l.LineVersion)
		}
		amount, local := l.Amount, l.Local
		if amount == nil || local == nil {
			v := st.Lines[l.LineNo-1].Versions[l.LineVersion-1]
			amount, local = &v.Amount.Minor, &v.Local
		}
		a.Lines = append(a.Lines, fin.LineApportionment{LineNo: l.LineNo, LineVersion: l.LineVersion, Method: l.Method, Reason: deref(l.Reason),
			Amount: *amount, Local: *local, ToAllocate: l.ToAllocate, ToAllocateLocal: l.ToAllocateLocal})
	}
	for _, s := range sr {
		if s.ApportionmentNo < 1 || s.ApportionmentNo > len(st.Apportionments) {
			return nil, 0, fmt.Errorf("%w: share of apportionment %d", fin.ErrInvalid, s.ApportionmentNo)
		}
		a := &st.Apportionments[s.ApportionmentNo-1]
		placed := false
		for i := range a.Lines {
			if a.Lines[i].LineNo == s.LineNo {
				a.Lines[i].Shares = append(a.Lines[i].Shares, fin.Share{No: s.No, ShipmentID: s.ShipmentID, JobID: s.JobID, Used: s.Used, Quantity: s.Quantity,
					Unit: s.Unit, Amount: s.Amount, Local: s.Local, InUnit: s.InUnit})
				placed = true
			}
		}
		if !placed {
			return nil, 0, fmt.Errorf("%w: share of apportionment %d line %d", fin.ErrInvalid, s.ApportionmentNo, s.LineNo)
		}
	}
	for _, x := range xr {
		if x.ApportionmentNo < 1 || x.ApportionmentNo > len(st.Apportionments) {
			return nil, 0, fmt.Errorf("%w: attribution of apportionment %d", fin.ErrInvalid, x.ApportionmentNo)
		}
		a := &st.Apportionments[x.ApportionmentNo-1]
		placed := false
		for i := range a.Lines {
			if a.Lines[i].LineNo == x.LineNo {
				a.Lines[i].Attributions = append(a.Lines[i].Attributions, attributionOf(x))
				placed = true
			}
		}
		if !placed {
			return nil, 0, fmt.Errorf("%w: attribution of apportionment %d line %d", fin.ErrInvalid, x.ApportionmentNo, x.LineNo)
		}
	}
	p, err := fin.RestorePool(st)
	return p, revision, err
}

func attributionOf(x attributionRow) fin.Attribution {
	return fin.Attribution{ShipmentID: x.ShipmentID, JobID: x.JobID, Loss: x.Loss, LossLocal: x.LossLocal, Exception: x.Exception, ExceptionLocal: x.ExceptionLocal,
		Party: deref(x.Party), PartyReference: deref(x.PartyReference), Voucher: x.Voucher, Reason: deref(x.Reason), DepartedAt: utc(x.DepartedAt), Carried: x.Carried,
		ByDefault: x.ByDefault, ByRatio: x.ByRatio}
}

func rowOfAttribution(x fin.Attribution) attributionRow {
	return attributionRow{ShipmentID: x.ShipmentID, JobID: x.JobID, Loss: x.Loss, LossLocal: x.LossLocal, Exception: x.Exception, ExceptionLocal: x.ExceptionLocal,
		Party: orNullString(x.Party), PartyReference: orNullString(x.PartyReference), Voucher: x.Voucher, Reason: orNullString(x.Reason), DepartedAt: x.DepartedAt, Carried: x.Carried,
		ByDefault: x.ByDefault, ByRatio: x.ByRatio}
}

func orNullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
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

// OpenPools lists the tenant's open pools in pool id order.
func (s PoolStore) OpenPools(ctx context.Context, tenant fin.TenantID) ([]*fin.ConsolCostPool, []int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, nil, err
	}
	rows, err := q.Query(ctx, `SELECT `+poolColumns+poolChildren()+` FROM cargo_fin.consol_cost_pool p WHERE p.tenant_id=$1 AND p.state='OPEN' ORDER BY p.pool_id`,
		string(tenant))
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
	previews := p.AddedPreviews()
	exec := func(sql string, args ...any) error {
		_, err := q.Exec(ctx, sql, append([]any{string(st.TenantID), st.PoolID}, args...)...)
		return err
	}
	for _, l := range lines {
		source := l.Source
		if source == "" {
			source = fin.SourceManual
		}
		var basisLine any
		if l.BasisLineNo != 0 {
			basisLine = l.BasisLineNo
		}
		err := exec(`INSERT INTO cargo_fin.pool_cost_line (tenant_id,pool_id,line_no,charge_code,description,supplier_ref,method,recorded_by,recorded_at,
            entered_at,source,basis_id,basis_line_no) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, l.No, l.ChargeCode, orNull(l.Description),
			orNull(l.SupplierRef), l.Method, l.By, l.At, l.EnteredAt, source, orNull(l.BasisID), basisLine)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && ((pgErr.Code == "23505" && pgErr.ConstraintName == "pool_cost_line_basis_once") ||
			(pgErr.Code == "23514" && pgErr.ConstraintName == "pool_one_estimate")) {
			return fmt.Errorf("%w: basis %s", fin.ErrBasisApplied, l.BasisID)
		}
		if err != nil {
			return err
		}
	}
	for i, v := range versions {
		args := append([]any{lineOf[i], v.No, orNull(v.CreditorPartyID), v.CreditorRevision, string(v.Amount.Currency), v.Amount.Minor, v.Local},
			fxArgs(v.FX)...)
		var unitNo any
		if v.LoadUnitNo > 0 {
			unitNo = v.LoadUnitNo
		}
		args = append(args, v.Status, orNull(v.Reason), v.By, v.At, v.EnteredAt, unitNo)
		if err := exec(`INSERT INTO cargo_fin.pool_cost_version (tenant_id,pool_id,line_no,version_no,creditor_party_id,creditor_revision,currency,
            amount_minor,local_minor,rate_entry_no,rate_type,rate,scale,from_digits,to_digits,rate_source,status,reason,recorded_by,recorded_at,entered_at,load_unit_no)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)`, args...); err != nil {
			return err
		}
	}
	for _, v := range previews {
		figures := make([]figureLine, 0, len(v.Lines))
		for _, la := range v.Lines {
			f := figureLine{LineNo: la.LineNo, LineVersion: la.LineVersion, Method: la.Method, Reason: la.Reason, Amount: la.Amount, Local: la.Local,
				Shares: []figureShare{}, ToAllocate: la.ToAllocate, ToAllocateLocal: la.ToAllocateLocal, Attributions: []attributionRow{}}
			for _, x := range la.Attributions {
				f.Attributions = append(f.Attributions, rowOfAttribution(x))
			}
			for _, sh := range la.Shares {
				f.Shares = append(f.Shares, figureShare{No: sh.No, ShipmentID: sh.ShipmentID, JobID: sh.JobID, Used: sh.Used, Quantity: sh.Quantity, Unit: sh.Unit,
					Amount: sh.Amount, Local: sh.Local, InUnit: sh.InUnit})
			}
			figures = append(figures, f)
		}
		blob, err := json.Marshal(figures)
		if err != nil {
			return err
		}
		if err = exec(`INSERT INTO cargo_fin.pool_apportionment_preview (tenant_id,pool_id,preview_no,basis_revision,stage,figures,fingerprint,fingerprint_sha256,
            made_by,made_at,entered_at,settings_version) VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,$8,$9,$10,$11,$12)`, v.No, v.BasisRevision, v.Stage, string(blob),
			v.Fingerprint, v.FingerprintSHA256, v.By, v.At, v.EnteredAt, v.SettingsVersion); err != nil {
			return err
		}
	}
	for _, a := range apportionments {
		var preview any
		if a.PreviewNo != 0 {
			preview = a.PreviewNo
		}
		err := exec(`INSERT INTO cargo_fin.pool_apportionment (tenant_id,pool_id,apportionment_no,basis_revision,stage,made_by,made_at,entered_at,preview_no,
            settings_version) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, a.No, a.BasisRevision, a.Stage, a.By, a.At, a.EnteredAt, preview, a.SettingsVersion)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "pool_apportionment_one_per_preview" {
			return fmt.Errorf("%w: preview %d of pool %s was submitted already", fin.ErrPreviewSubmitted, a.PreviewNo, st.PoolID)
		}
		if err != nil {
			return err
		}
		for _, la := range a.Lines {
			if err := exec(`INSERT INTO cargo_fin.pool_apportionment_line (tenant_id,pool_id,apportionment_no,line_no,line_version,method,reason,
                amount_minor,local_minor,to_allocate_minor,to_allocate_local_minor) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, a.No, la.LineNo, la.LineVersion,
				la.Method, orNull(la.Reason), la.Amount, la.Local, la.ToAllocate, la.ToAllocateLocal); err != nil {
				return err
			}
			for _, x := range la.Attributions {
				if err := exec(`INSERT INTO cargo_fin.pool_apportionment_attribution (tenant_id,pool_id,apportionment_no,line_no,shipment_id,job_id,loss_minor,
                    loss_local_minor,exception_minor,exception_local_minor,party,party_reference,voucher,reason,departed_at,carried,by_default,by_ratio)
                    VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`, a.No, la.LineNo, x.ShipmentID, x.JobID, x.Loss, x.LossLocal, x.Exception,
					x.ExceptionLocal, orNull(x.Party), orNull(x.PartyReference), x.Voucher, orNull(x.Reason), x.DepartedAt, x.Carried, x.ByDefault, x.ByRatio); err != nil {
					return err
				}
			}
			for _, sh := range la.Shares {
				if err := exec(`INSERT INTO cargo_fin.pool_apportionment_share (tenant_id,pool_id,apportionment_no,line_no,share_no,shipment_id,job_id,
                    used,quantity,unit,amount_minor,local_minor,in_unit) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, a.No, la.LineNo, sh.No, sh.ShipmentID,
					sh.JobID, sh.Used, sh.Quantity, orNull(sh.Unit), sh.Amount, sh.Local, sh.InUnit); err != nil {
					return err
				}
			}
		}
	}
	closures, reopened := p.AddedClosures()
	for _, c := range closures {
		if err := exec(`INSERT INTO cargo_fin.consol_cost_pool_closure (tenant_id,pool_id,closure_no,closed_by,closed_at,entered_at,reopened_by,reopen_reason,
            reopened_at,reopen_entered_at,settings_version) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, c.No, c.By, c.At, c.EnteredAt, orNull(c.ReopenedBy),
			orNull(c.ReopenReason), orNull(c.ReopenedAt), orNull(c.ReopenEnteredAt), c.SettingsVersion); err != nil {
			return err
		}
	}
	for _, c := range reopened {
		if err := exec(`UPDATE cargo_fin.consol_cost_pool_closure SET reopened_by=$4,reopen_reason=$5,reopened_at=$6,reopen_entered_at=$7
            WHERE tenant_id=$1 AND pool_id=$2 AND closure_no=$3`, c.No, c.ReopenedBy, c.ReopenReason, c.ReopenedAt, c.ReopenEnteredAt); err != nil {
			return err
		}
	}
	return nil
}

// SharesOfJob lists a job's shares under each pool line's latest apportionment, by pool and line; a line that left the
// job's house out is not listed: the job carries nothing of it.
func (s PoolStore) SharesOfJob(ctx context.Context, tenant fin.TenantID, job string) ([]app.PoolShare, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT sh.pool_id, p.consolidation_id, sh.line_no, al.line_version, cl.charge_code, al.method, a.stage, sh.quantity, coalesce(sh.unit,''),
        v.currency, sh.amount_minor, sh.local_minor
      FROM cargo_fin.pool_apportionment_share sh
      JOIN cargo_fin.consol_cost_pool p ON p.tenant_id=sh.tenant_id AND p.pool_id=sh.pool_id
      JOIN cargo_fin.pool_apportionment_line al ON al.tenant_id=sh.tenant_id AND al.pool_id=sh.pool_id
        AND al.apportionment_no=sh.apportionment_no AND al.line_no=sh.line_no
      JOIN cargo_fin.pool_apportionment a ON a.tenant_id=sh.tenant_id AND a.pool_id=sh.pool_id AND a.apportionment_no=sh.apportionment_no
      JOIN cargo_fin.pool_cost_line cl ON cl.tenant_id=sh.tenant_id AND cl.pool_id=sh.pool_id AND cl.line_no=sh.line_no
      JOIN cargo_fin.pool_cost_version v ON v.tenant_id=sh.tenant_id AND v.pool_id=sh.pool_id AND v.line_no=al.line_no AND v.version_no=al.line_version
      WHERE sh.tenant_id=$1 AND sh.job_id=$2 AND sh.used AND sh.apportionment_no = (SELECT max(l2.apportionment_no) FROM cargo_fin.pool_apportionment_line l2
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
		if err := rows.Scan(&sh.PoolID, &sh.ConsolidationID, &sh.LineNo, &sh.LineVersion, &sh.ChargeCode, &sh.Method, &sh.Stage, &sh.Quantity, &sh.Unit, &currency, &amount,
			&sh.Local); err != nil {
			return nil, err
		}
		sh.Amount = kernel.Money{Currency: kernel.CurrencyCode(currency), Minor: amount}
		out = append(out, sh)
	}
	return out, rows.Err()
}

// LossesOfJob lists the re-routing losses a job bears under each pool line's latest apportionment (FIN V2.93), by pool
// and line; an attribution without a loss is not listed.
func (s PoolStore) LossesOfJob(ctx context.Context, tenant fin.TenantID, job string) ([]app.PoolLoss, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT x.pool_id, x.line_no, x.voucher, x.departed_at, v.currency, x.loss_minor, x.loss_local_minor
      FROM cargo_fin.pool_apportionment_attribution x
      JOIN cargo_fin.pool_apportionment_line al ON al.tenant_id=x.tenant_id AND al.pool_id=x.pool_id AND al.apportionment_no=x.apportionment_no AND al.line_no=x.line_no
      JOIN cargo_fin.pool_cost_version v ON v.tenant_id=x.tenant_id AND v.pool_id=x.pool_id AND v.line_no=al.line_no AND v.version_no=al.line_version
      WHERE x.tenant_id=$1 AND x.job_id=$2 AND x.loss_minor > 0 AND x.apportionment_no = (SELECT max(l2.apportionment_no) FROM cargo_fin.pool_apportionment_line l2
        WHERE l2.tenant_id=x.tenant_id AND l2.pool_id=x.pool_id AND l2.line_no=x.line_no)
      ORDER BY x.pool_id, x.line_no`, string(tenant), job)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []app.PoolLoss
	for rows.Next() {
		var l app.PoolLoss
		var currency string
		var amount int64
		if err := rows.Scan(&l.PoolID, &l.LineNo, &l.Voucher, &l.DepartedAt, &currency, &amount, &l.Local); err != nil {
			return nil, err
		}
		l.DepartedAt = utc(l.DepartedAt)
		l.Amount = kernel.Money{Currency: kernel.CurrencyCode(currency), Minor: amount}
		out = append(out, l)
	}
	return out, rows.Err()
}

// HoldingsOfJob lists the used shares and the losses a job bears under each pool line's latest apportionment, with
// whether the pool is closed (FIN V2.93): what stops the job from being cancelled.
func (s PoolStore) HoldingsOfJob(ctx context.Context, tenant fin.TenantID, job string) ([]app.PoolHolding, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT x.pool_id, x.line_no, x.loss, p.state = 'CLOSED' FROM (
        SELECT sh.tenant_id, sh.pool_id, sh.line_no, sh.apportionment_no, false AS loss FROM cargo_fin.pool_apportionment_share sh WHERE sh.job_id=$2 AND sh.used
        UNION ALL
        SELECT a.tenant_id, a.pool_id, a.line_no, a.apportionment_no, true FROM cargo_fin.pool_apportionment_attribution a WHERE a.job_id=$2 AND a.loss_minor > 0) x
      JOIN cargo_fin.consol_cost_pool p ON p.tenant_id=x.tenant_id AND p.pool_id=x.pool_id
      WHERE x.tenant_id=$1 AND x.apportionment_no = (SELECT max(l2.apportionment_no) FROM cargo_fin.pool_apportionment_line l2
        WHERE l2.tenant_id=x.tenant_id AND l2.pool_id=x.pool_id AND l2.line_no=x.line_no)
      ORDER BY x.pool_id, x.line_no, x.loss`, string(tenant), job)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []app.PoolHolding
	for rows.Next() {
		var h app.PoolHolding
		if err := rows.Scan(&h.PoolID, &h.LineNo, &h.Loss, &h.Closed); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// RevisionsOf answers the revision of each of the pools named.
func (s PoolStore) RevisionsOf(ctx context.Context, tenant fin.TenantID, pools []string) (map[string]int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT pool_id, revision FROM cargo_fin.consol_cost_pool WHERE tenant_id=$1 AND pool_id = ANY($2)`, string(tenant), pools)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var revision int64
		if err := rows.Scan(&id, &revision); err != nil {
			return nil, err
		}
		out[id] = revision
	}
	return out, rows.Err()
}
