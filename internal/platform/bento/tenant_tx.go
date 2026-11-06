//go:build bento

// Package bento is the integration seam to the *real* idp-bento-go release candidate.
// A tenant marker in Go context blocks accidental nested cross-tenant switching;
// PostgreSQL RLS remains defense-in-depth and not protection from compromised SQL.
package bento

import (
	"context"
	"errors"
	"fmt"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	org "go.idp.xyz/idp-cargo/internal/contexts/org/domain"
)

type tenantContextKey struct{}

var ErrTenantScopeConflict = errors.New("org: cross-tenant nested transaction")

type TenantTransactor struct {
	DB     *bentopg.DB
	Tenant org.TenantID
}

func (t TenantTransactor) TenantScope() org.TenantID { return t.Tenant }
func (t TenantTransactor) WithinTransaction(ctx context.Context, fn func(context.Context) error) error {
	if t.DB == nil || t.Tenant == "" || fn == nil {
		return errors.New("org: tenant transactor not configured")
	}
	if previous, ok := ctx.Value(tenantContextKey{}).(org.TenantID); ok && previous != t.Tenant {
		return ErrTenantScopeConflict
	}
	return t.DB.Transactor().WithinTransaction(ctx, func(txCtx context.Context) error {
		exec, err := t.DB.RequireExecutor(txCtx)
		if err != nil {
			return err
		}
		_, err = exec.Exec(txCtx, "SELECT pg_catalog.set_config('app.tenant_id',$1,true)", string(t.Tenant))
		if err != nil {
			return fmt.Errorf("org: bind tenant scope: %w", err)
		}
		return fn(context.WithValue(txCtx, tenantContextKey{}, t.Tenant))
	})
}
