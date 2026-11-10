package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// ActionReadContactAccessLog lets a caller, such as the tenant's data protection officer, read the log of
// contact reads.
const ActionReadContactAccessLog OrderAction = "ReadContactAccessLog"

// ErrRestrictedPurpose refuses a read of restricted contacts for a purpose they are not kept for.
var ErrRestrictedPurpose = errors.New("com application: the purpose does not allow reading restricted contacts")

// ContactAccess is one logged read of an order's contacts: who read them, through which authorized action,
// whether they were restricted, for what purpose, and how many contact versions with personal data the read
// returned. It holds no contact details. AccessID and RecordedAt are set when the read is logged.
type ContactAccess struct {
	AccessID   string
	ActorID    string
	Action     string
	OrderID    com.OrderID
	Restricted bool
	Purpose    kernel.AccessPurpose
	Contacts   int
	RecordedAt time.Time
}

// AccessFilter selects logged reads: of one order or all, by one actor or all, recorded in [Since, Until)
// (either may be zero), newest first, at most Limit.
type AccessFilter struct {
	OrderID com.OrderID
	ActorID string
	Since   time.Time
	Until   time.Time
	Limit   int
}

// ContactAccessLog writes the log of contact reads in the reading transaction and lists it. The log is kept
// three years (decision P6) and never rewritten.
type ContactAccessLog interface {
	RecordContactsRead(ctx context.Context, tenant com.TenantID, read ContactAccess) error
	ListContactsReads(ctx context.Context, tenant com.TenantID, f AccessFilter) ([]ContactAccess, error)
}

// MaxAccessRows bounds one page of the access log.
const MaxAccessRows = 1000

// ListContactAccess lists the log of contact reads to callers authorized for ReadContactAccessLog.
type ListContactAccess struct {
	Tx   TxRunner
	Log  ContactAccessLog
	Auth Authorization
}

func (h ListContactAccess) Handle(ctx context.Context, s Scope, f AccessFilter) ([]ContactAccess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if h.Tx == nil || h.Log == nil || h.Auth == nil {
		return nil, ErrMissingDependency
	}
	if s.TenantID == "" || strings.TrimSpace(s.ActorID) == "" {
		return nil, ErrUnauthenticated
	}
	if f.Limit < 1 || f.Limit > MaxAccessRows || (!f.Since.IsZero() && !f.Until.IsZero() && !f.Since.Before(f.Until)) {
		return nil, fmt.Errorf("%w: limit %d (1 to %d), since %s, until %s", ErrInvalidQuery, f.Limit, MaxAccessRows, f.Since, f.Until)
	}
	if scoped, ok := h.Tx.(interface{ TenantScope() string }); ok && scoped.TenantScope() != string(s.TenantID) {
		return nil, ErrUnauthorized
	}
	if err := h.Auth.RequireOrderAction(ctx, s, ActionReadContactAccessLog, f.OrderID); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	var out []ContactAccess
	err := h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var err error
		out, err = h.Log.ListContactsReads(txCtx, s.TenantID, f)
		return err
	})
	return out, err
}
