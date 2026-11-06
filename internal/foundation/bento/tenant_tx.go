package bento

import (
	"context"
	"errors"
	"fmt"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
)

type tenantContextKey struct{}

var ErrTenantScopeConflict = errors.New("bento: cross-tenant nested transaction")

// TenantTransactor runs a Bento transaction bound to one tenant through the transaction-local
// app.tenant_id setting that the RLS policies read. A tenant marker in the Go context blocks
// accidental nested cross-tenant switching; RLS remains defense in depth, not protection from
// compromised SQL.
type TenantTransactor struct {
	DB     *bentopg.DB
	Tenant string
}

func (t TenantTransactor) TenantScope() string { return t.Tenant }

func (t TenantTransactor) WithinTransaction(ctx context.Context, fn func(context.Context) error) error {
	if t.DB == nil || t.Tenant == "" || fn == nil {
		return errors.New("bento: tenant transactor not configured")
	}
	if previous, ok := ctx.Value(tenantContextKey{}).(string); ok && previous != t.Tenant {
		return ErrTenantScopeConflict
	}
	return t.DB.Transactor().WithinTransaction(ctx, func(txCtx context.Context) error {
		exec, err := t.DB.RequireExecutor(txCtx)
		if err != nil {
			return err
		}
		_, err = exec.Exec(txCtx, "SELECT pg_catalog.set_config('app.tenant_id',$1,true)", t.Tenant)
		if err != nil {
			return fmt.Errorf("bento: bind tenant scope: %w", err)
		}
		return fn(context.WithValue(txCtx, tenantContextKey{}, t.Tenant))
	})
}
