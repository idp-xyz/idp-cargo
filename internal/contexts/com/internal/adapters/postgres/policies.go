package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
)

// Policies stores the versions of order policies (C1, C2). The tenant default is kept under an empty legal
// entity.
type Policies struct{ DB *bentopg.DB }

type policyQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (p Policies) InForce(ctx context.Context, tenant com.TenantID, entity com.LegalEntityID) (com.OrderPolicy, error) {
	q, err := p.DB.RequireExecutor(ctx)
	if err != nil {
		return com.OrderPolicy{}, err
	}
	s, found, err := readPolicy(ctx, q, `SELECT tenant_id,legal_entity_id,version,max_orders_per_quote,grace_days,set_by,set_at
      FROM cargo_com.order_policy WHERE tenant_id=$1 AND legal_entity_id IN ($2,'')
      ORDER BY (legal_entity_id = '') ASC, version DESC LIMIT 1`, string(tenant), string(entity))
	if err != nil || !found {
		return com.BuiltInOrderPolicy(), err
	}
	return s.OrderPolicy, nil
}

// Latest takes the lock the version trigger takes, so concurrent commands on one scope run one after the
// other, and returns the scope's latest version.
func (p Policies) Latest(ctx context.Context, tenant com.TenantID, entity com.LegalEntityID) (*com.PolicySetting, error) {
	q, err := p.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, err
	}
	if _, err = q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('cargo_com.order_policy:' || $1 || '/' || $2, 0))`,
		string(tenant), string(entity)); err != nil {
		return nil, err
	}
	s, found, err := readPolicy(ctx, q, `SELECT tenant_id,legal_entity_id,version,max_orders_per_quote,grace_days,set_by,set_at
      FROM cargo_com.order_policy WHERE tenant_id=$1 AND legal_entity_id=$2 ORDER BY version DESC LIMIT 1`, string(tenant), string(entity))
	if err != nil || !found {
		return nil, err
	}
	return &s, nil
}

func readPolicy(ctx context.Context, q policyQuerier, sql string, args ...any) (com.PolicySetting, bool, error) {
	var s com.PolicySetting
	var tenant, entity string
	err := q.QueryRow(ctx, sql, args...).Scan(&tenant, &entity, &s.Version, &s.MaxOrdersPerQuote, &s.GraceDays, &s.SetBy, &s.SetAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return com.PolicySetting{}, false, nil
	}
	if err != nil {
		return com.PolicySetting{}, false, err
	}
	s.TenantID, s.LegalEntityID, s.SetAt = com.TenantID(tenant), com.LegalEntityID(entity), s.SetAt.UTC()
	s.Scope = com.PolicyLegalEntity
	if entity == "" {
		s.Scope = com.PolicyTenant
	}
	if err = s.Validate(); err != nil {
		return com.PolicySetting{}, false, err
	}
	return s, true, nil
}

func (p Policies) Insert(ctx context.Context, s com.PolicySetting) error {
	q, err := p.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_com.order_policy (tenant_id,legal_entity_id,version,max_orders_per_quote,grace_days,set_by,set_at)
      VALUES ($1,$2,$3,$4,$5,$6,$7)`, string(s.TenantID), string(s.LegalEntityID), s.Version, s.MaxOrdersPerQuote, s.GraceDays, s.SetBy, s.SetAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && ((pgErr.Code == "23505" && pgErr.ConstraintName == "order_policy_pkey") ||
		(pgErr.Code == "23514" && pgErr.ConstraintName == "order_policy_sequence")) {
		return fmt.Errorf("%w: %s", com.ErrPolicyVersionStale, pgErr.Message)
	}
	return err
}
