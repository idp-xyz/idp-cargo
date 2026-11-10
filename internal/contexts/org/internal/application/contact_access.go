package application

import (
	"context"
	"fmt"
	"strings"
	"time"

	org "go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// ContactAccess is one logged read of a party's contacts: who read them, through which authorized action,
// for what purpose, and which contacts with personal data the read returned. It holds no contact details.
// AccessID and RecordedAt are set when the read is logged.
type ContactAccess struct {
	AccessID   string
	ActorID    string
	Action     string
	PartyID    org.PartyID
	Purpose    kernel.AccessPurpose
	ContactIDs []org.ContactID
	RecordedAt time.Time
}

// AccessFilter selects logged reads: of one party or all, by one actor or all, recorded in [Since, Until)
// (either may be zero), newest first, at most Limit.
type AccessFilter struct {
	PartyID org.PartyID
	ActorID string
	Since   time.Time
	Until   time.Time
	Limit   int
}

// ContactAccessLog writes the log of contact reads in the reading transaction and lists it. The log is kept
// three years (decision P6) and never rewritten.
type ContactAccessLog interface {
	RecordContactsRead(ctx context.Context, tenant org.TenantID, read ContactAccess) error
	ListContactsReads(ctx context.Context, tenant org.TenantID, f AccessFilter) ([]ContactAccess, error)
}

// MaxAccessRows bounds one page of the access log.
const MaxAccessRows = 1000

// ListContactAccess lists the log of contact reads to callers authorized for ReadContactAccessLog, such as the
// tenant's data protection officer.
type ListContactAccess struct {
	Tx   TxRunner
	Log  ContactAccessLog
	Auth PartyAuthorization
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
	if err := h.Auth.RequirePartyAction(ctx, s, ActionReadContactAccessLog, f.PartyID); err != nil {
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
