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
//
// Snapshot makes a read: Bento begins every transaction at READ COMMITTED, where each statement takes a new snapshot,
// so a query reading an aggregate and its related rows in several statements could see a commit land between them.
// With Snapshot the transaction becomes REPEATABLE READ and READ ONLY before its first statement, so every statement
// reads the snapshot taken at the first one and nothing is written. Inside another transaction it is that one's.
type TenantTransactor struct {
	DB       *bentopg.DB
	Tenant   string
	Snapshot bool
}

func (t TenantTransactor) TenantScope() string { return t.Tenant }

func (t TenantTransactor) WithinTransaction(ctx context.Context, fn func(context.Context) error) error {
	if t.DB == nil || t.Tenant == "" || fn == nil {
		return errors.New("bento: tenant transactor not configured")
	}
	if previous, ok := ctx.Value(tenantContextKey{}).(string); ok && previous != t.Tenant {
		return ErrTenantScopeConflict
	}
	_, outside := t.DB.RequireExecutor(ctx) // an error: no transaction in ctx yet
	return t.DB.Transactor().WithinTransaction(ctx, func(txCtx context.Context) error {
		exec, err := t.DB.RequireExecutor(txCtx)
		if err != nil {
			return err
		}
		if t.Snapshot && outside != nil { // a new transaction: nothing has run in it yet
			if _, err = exec.Exec(txCtx, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ ONLY"); err != nil {
				return fmt.Errorf("bento: snapshot read: %w", err)
			}
		}
		_, err = exec.Exec(txCtx, "SELECT pg_catalog.set_config('app.tenant_id',$1,true)", t.Tenant)
		if err != nil {
			return fmt.Errorf("bento: bind tenant scope: %w", err)
		}
		return fn(context.WithValue(txCtx, tenantContextKey{}, t.Tenant))
	})
}
