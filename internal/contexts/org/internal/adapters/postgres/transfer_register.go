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
	app "go.idp.xyz/idp-cargo/internal/contexts/org/internal/application"
	org "go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
)

// TransferRegister keeps ContactTransferRegister v1 (ORG V0.7).
type TransferRegister struct{ DB *bentopg.DB }

var _ app.TransferStore = TransferRegister{}

func (s TransferRegister) Lock(ctx context.Context, tenant org.TenantID, year int) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	if _, err = q.Exec(ctx, `INSERT INTO cargo_org.contact_transfer_register (tenant_id,year) VALUES ($1,$2) ON CONFLICT DO NOTHING`, string(tenant), year); err != nil {
		return 0, err
	}
	var revision int64
	err = q.QueryRow(ctx, `SELECT revision FROM cargo_org.contact_transfer_register WHERE tenant_id=$1 AND year=$2 FOR UPDATE`, string(tenant), year).Scan(&revision)
	return revision, err
}

func (s TransferRegister) Advance(ctx context.Context, tenant org.TenantID, year int, revision int64) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	var next int64
	err = q.QueryRow(ctx, `UPDATE cargo_org.contact_transfer_register SET revision=revision+1 WHERE tenant_id=$1 AND year=$2 AND revision=$3 RETURNING revision`,
		string(tenant), year, revision).Scan(&next)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: contact transfer register %d", bentorepo.ErrConflict, year)
	}
	return next, err
}

func (s TransferRegister) Revision(ctx context.Context, tenant org.TenantID, year int) (int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return 0, err
	}
	var revision int64
	err = q.QueryRow(ctx, `SELECT revision FROM cargo_org.contact_transfer_register WHERE tenant_id=$1 AND year=$2`, string(tenant), year).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return revision, err
}

func (s TransferRegister) Persons(ctx context.Context, tenant org.TenantID, year int) (int64, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return 0, err
	}
	var n int64
	err = q.QueryRow(ctx, `SELECT count(DISTINCT contact_key) FROM cargo_org.contact_transfer_reservation WHERE tenant_id=$1 AND year=$2 AND released_at IS NULL`,
		string(tenant), year).Scan(&n)
	return n, err
}

func (s TransferRegister) Held(ctx context.Context, tenant org.TenantID, year int, keys []string) (map[string]bool, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT DISTINCT contact_key FROM cargo_org.contact_transfer_reservation WHERE tenant_id=$1 AND year=$2 AND released_at IS NULL
      AND contact_key = ANY($3)`, string(tenant), year, keys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var k string
		if err = rows.Scan(&k); err != nil {
			return nil, err
		}
		out[k] = true
	}
	return out, rows.Err()
}

func (s TransferRegister) DispatchKeys(ctx context.Context, tenant org.TenantID, dispatch string) (int, []string, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return 0, nil, err
	}
	rows, err := q.Query(ctx, `SELECT year, contact_key FROM cargo_org.contact_transfer_reservation WHERE tenant_id=$1 AND dispatch_id=$2 AND released_at IS NULL
      ORDER BY contact_key`, string(tenant), dispatch)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	var year int
	var keys []string
	for rows.Next() {
		var k string
		if err = rows.Scan(&year, &k); err != nil {
			return 0, nil, err
		}
		keys = append(keys, k)
	}
	return year, keys, rows.Err()
}

func (s TransferRegister) Reserve(ctx context.Context, tenant org.TenantID, year int, dispatch string, keys []string, by string, at, entered time.Time) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_org.contact_transfer_dispatch (tenant_id,dispatch_id,year,reserved_by,reserved_at,entered_at)
      VALUES ($1,$2,$3,$4,$5,$6)`, string(tenant), dispatch, year, by, at, entered)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("%w: dispatch %s reserved its contacts before; a dispatch reserves once", org.ErrInvalidTransfer, dispatch)
	}
	if err != nil {
		return err
	}
	for _, k := range keys {
		if _, err = q.Exec(ctx, `INSERT INTO cargo_org.contact_transfer_reservation (tenant_id,year,dispatch_id,contact_key,reserved_by,reserved_at,entered_at)
          VALUES ($1,$2,$3,$4,$5,$6,$7)`, string(tenant), year, dispatch, k, by, at, entered); err != nil {
			return err
		}
	}
	return nil
}

func (s TransferRegister) Release(ctx context.Context, tenant org.TenantID, year int, dispatch, by string, at, entered time.Time) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	tag, err := q.Exec(ctx, `UPDATE cargo_org.contact_transfer_reservation SET released_by=$4,released_at=$5,release_entered_at=$6
      WHERE tenant_id=$1 AND year=$2 AND dispatch_id=$3 AND released_at IS NULL`, string(tenant), year, dispatch, by, at, entered)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s TransferRegister) Safeguard(ctx context.Context, tenant org.TenantID, at time.Time) (org.TransferSafeguard, bool, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return org.TransferSafeguard{}, false, err
	}
	var g org.TransferSafeguard
	var until *time.Time
	err = q.QueryRow(ctx, `SELECT safeguard_id,kind,reference,valid_from,valid_until,evidence_ref,recorded_by,recorded_at,entered_at
      FROM cargo_org.contact_transfer_safeguard WHERE tenant_id=$1 AND valid_from <= $2 AND (valid_until IS NULL OR $2 < valid_until)
      ORDER BY valid_from DESC, safeguard_id LIMIT 1`, string(tenant), at).Scan(&g.SafeguardID, &g.Kind, &g.Reference, &g.Validity.From, &until, &g.EvidenceRef,
		&g.RecordedBy, &g.RecordedAt, &g.EnteredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return org.TransferSafeguard{}, false, nil
	}
	if err != nil {
		return org.TransferSafeguard{}, false, err
	}
	g.Validity.From, g.RecordedAt, g.EnteredAt = g.Validity.From.UTC(), g.RecordedAt.UTC(), g.EnteredAt.UTC()
	if until != nil {
		g.Validity.Until = until.UTC()
	}
	return g, true, nil
}

func (s TransferRegister) InsertSafeguard(ctx context.Context, tenant org.TenantID, g org.TransferSafeguard) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	var until any
	if !g.Validity.Until.IsZero() {
		until = g.Validity.Until
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_org.contact_transfer_safeguard (tenant_id,safeguard_id,kind,reference,valid_from,valid_until,evidence_ref,recorded_by,
        recorded_at,entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, string(tenant), g.SafeguardID, g.Kind, g.Reference, g.Validity.From, until,
		g.EvidenceRef, g.RecordedBy, g.RecordedAt, g.EnteredAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("%w: safeguard %s", bentorepo.ErrAlreadyExists, g.SafeguardID)
	}
	return err
}
