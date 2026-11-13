package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	app "go.idp.xyz/idp-cargo/internal/contexts/rate/internal/application"
	rate "go.idp.xyz/idp-cargo/internal/contexts/rate/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// BasisStore writes and reads issued pricing bases (RATE V0.5 part 2).
type BasisStore struct{ DB *bentopg.DB }

var _ app.BasisStore = BasisStore{}

func (s BasisStore) Insert(ctx context.Context, b *rate.PricingBasis) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	st := b.State()
	t := string(st.TenantID)
	var customer, revision any
	if st.Customer.PartyID != "" {
		customer, revision = st.Customer.PartyID, st.Customer.PartyRevision
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_rate.pricing_basis (tenant_id,basis_id,purpose,legal_entity_id,customer_party_id,customer_revision,subject_kind,
        subject_id,subject_version,snapshot_id,issued_by,issued_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, t, st.BasisID, string(st.Purpose),
		string(st.LegalEntityID), customer, revision, st.SubjectKind, st.SubjectID, st.SubjectVersion, st.SnapshotID, st.IssuedBy, st.IssuedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return bentorepo.ErrAlreadyExists
	}
	if err != nil {
		return err
	}
	for _, l := range st.Lines {
		var sellCurrency, sell, sellLine, costCurrency, cost, costLine any
		if l.HasSell {
			sellCurrency, sell, sellLine = string(l.Sell.Currency), l.Sell.Minor, l.SellLineNo
		}
		if l.HasCost {
			costCurrency, cost, costLine = string(l.Cost.Currency), l.Cost.Minor, l.CostLineNo
		}
		if _, err = q.Exec(ctx, `INSERT INTO cargo_rate.pricing_basis_line (tenant_id,basis_id,line_no,charge_code,sell_currency,sell_minor,sell_line_no,
            cost_currency,cost_minor,cost_line_no,creditor_party_id) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, t, st.BasisID, l.LineNo,
			string(l.ChargeCode), sellCurrency, sell, sellLine, costCurrency, cost, costLine, orNull(l.CreditorPartyID)); err != nil {
			return err
		}
	}
	for i, g := range st.Gaps {
		if _, err = q.Exec(ctx, `INSERT INTO cargo_rate.pricing_basis_gap (tenant_id,basis_id,gap_no,side,charge_code,gap) VALUES ($1,$2,$3,$4,$5,$6)`,
			t, st.BasisID, i+1, string(g.Side), orNull(string(g.ChargeCode)), string(g.Gap)); err != nil {
			return err
		}
	}
	return nil
}

func (s BasisStore) Read(ctx context.Context, tenant rate.TenantID, id string) (*rate.PricingBasis, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	st := rate.PricingBasisState{TenantID: tenant, BasisID: id}
	var purpose, entity string
	var customer *string
	var revision *int64
	err = q.QueryRow(ctx, `SELECT purpose,legal_entity_id,customer_party_id,customer_revision,subject_kind,subject_id,subject_version,snapshot_id,issued_by,
        issued_at FROM cargo_rate.pricing_basis WHERE tenant_id=$1 AND basis_id=$2`, string(tenant), id).Scan(&purpose, &entity, &customer, &revision,
		&st.SubjectKind, &st.SubjectID, &st.SubjectVersion, &st.SnapshotID, &st.IssuedBy, &st.IssuedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, bentorepo.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	st.Purpose, st.LegalEntityID, st.Customer.PartyID, st.IssuedAt = rate.SnapshotPurpose(purpose), rate.LegalEntityID(entity), deref(customer), st.IssuedAt.UTC()
	if revision != nil {
		st.Customer.PartyRevision = *revision
	}
	rows, err := q.Query(ctx, `SELECT line_no,charge_code,sell_currency,sell_minor,sell_line_no,cost_currency,cost_minor,cost_line_no,creditor_party_id
      FROM cargo_rate.pricing_basis_line WHERE tenant_id=$1 AND basis_id=$2 ORDER BY line_no`, string(tenant), id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var l rate.BasisLine
		var code string
		var sellCurrency, costCurrency, creditor *string
		var sell, cost *int64
		var sellLine, costLine *int
		if err = rows.Scan(&l.LineNo, &code, &sellCurrency, &sell, &sellLine, &costCurrency, &cost, &costLine, &creditor); err != nil {
			rows.Close()
			return nil, err
		}
		l.ChargeCode, l.CreditorPartyID = rate.ChargeCode(code), deref(creditor)
		if sell != nil {
			l.HasSell, l.Sell, l.SellLineNo = true, kernel.Money{Currency: kernel.CurrencyCode(*sellCurrency), Minor: *sell}, *sellLine
		}
		if cost != nil {
			l.HasCost, l.Cost, l.CostLineNo = true, kernel.Money{Currency: kernel.CurrencyCode(*costCurrency), Minor: *cost}, *costLine
		}
		st.Lines = append(st.Lines, l)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows, err = q.Query(ctx, `SELECT side,charge_code,gap FROM cargo_rate.pricing_basis_gap WHERE tenant_id=$1 AND basis_id=$2 ORDER BY gap_no`,
		string(tenant), id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var side, gap string
		var code *string
		if err = rows.Scan(&side, &code, &gap); err != nil {
			return nil, err
		}
		st.Gaps = append(st.Gaps, rate.BasisGap{Side: rate.Side(side), ChargeCode: rate.ChargeCode(deref(code)), Gap: rate.Gap(gap)})
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return rate.RestoreBasis(st), nil
}
