// Package application orchestrates ORG use cases; DDD rules remain in domain.
// Interfaces are intentionally narrow. The production adapter must connect these
// to Bento-Go Transactor, scoped Repository and transactional Outbox on Go1.26.5.
package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	org "go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
)

var (
	ErrUnauthenticated     = errors.New("org application: unauthenticated")
	ErrUnauthorized        = errors.New("org application: unauthorized")
	ErrIdempotencyConflict = errors.New("org application: command id payload mismatch")
	ErrMissingDependency   = errors.New("org application: required dependency missing")
	ErrInvalidCommand      = errors.New("org application: invalid command")
)

type Scope struct {
	TenantID      org.TenantID
	ActorID       string
	LegalEntityID org.LegalEntityID
}
type RoleApprovalCommand struct {
	CommandID        string
	Scope            Scope
	PartyID          org.PartyID
	ExpectedRevision int64
	Role             org.Role
	OccurredAt       time.Time
}

// Receipt is the result of a committed command. A role approval names the party and its new revision; an
// address book command names the party, the book's new revision and the address or contact it concerns with
// that entry's version.
type Receipt struct {
	CommandID    string
	ResultType   string // ResultRoleApproval or ResultAddressBook
	PartyID      org.PartyID
	NewRevision  int64
	EntryID      string
	EntryVersion int64
	RequestHash  string
}
type TxRunner interface {
	WithinTransaction(context.Context, func(context.Context) error) error
}
type PartyStore interface {
	Load(context.Context, org.TenantID, org.PartyID) (*org.BusinessParty, int64, error)
	Save(context.Context, *org.BusinessParty, int64) (int64, error)
}
type EntityEligibility interface {
	RequireVerifiedLegalEntity(context.Context, org.TenantID, org.LegalEntityID) error
}
type Authorization interface {
	RequireRoleApproval(context.Context, Scope, org.PartyID, org.RoleCode) error
}
type Receipts interface {
	Find(context.Context, org.TenantID, string) (Receipt, bool, error)
	Save(context.Context, org.TenantID, Receipt) error
}
type IntegrationOutbox interface {
	EnqueueORGEvent(context.Context, org.Event) error
}

type ApproveRole struct {
	Tx       TxRunner
	Parties  PartyStore
	Entities EntityEligibility
	Auth     Authorization
	Receipts Receipts
	Outbox   IntegrationOutbox
}

func (h ApproveRole) Handle(ctx context.Context, c RoleApprovalCommand) (Receipt, error) {
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	if h.Tx == nil || h.Parties == nil || h.Entities == nil || h.Auth == nil || h.Receipts == nil || h.Outbox == nil {
		return Receipt{}, ErrMissingDependency
	}
	if c.Scope.TenantID == "" || strings.TrimSpace(c.Scope.ActorID) == "" || c.Scope.LegalEntityID == "" ||
		c.CommandID == "" || c.PartyID == "" || c.Role.LegalEntityID != c.Scope.LegalEntityID || c.ExpectedRevision < 1 || c.OccurredAt.IsZero() {
		return Receipt{}, ErrUnauthenticated
	}
	// An explicitly scoped transaction cannot be reused for another tenant.
	if scoped, ok := h.Tx.(interface{ TenantScope() string }); ok && scoped.TenantScope() != string(c.Scope.TenantID) {
		return Receipt{}, ErrUnauthorized
	}
	// Approval identity is derived from the trusted caller, never taken from the request body.
	// An explicit mismatching body value is rejected rather than silently overwritten.
	if c.Role.ApprovedBy != "" && c.Role.ApprovedBy != c.Scope.ActorID {
		return Receipt{}, ErrUnauthorized
	}
	c.Role.ApprovedBy = c.Scope.ActorID
	// Canonical JSON struct controls stable hash; never log body or sensitive values.
	blob, err := json.Marshal(c)
	if err != nil {
		return Receipt{}, err
	}
	digest := sha256.Sum256(blob)
	hash := hex.EncodeToString(digest[:])
	var result Receipt
	var mutated *org.BusinessParty
	err = h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		if err := h.Auth.RequireRoleApproval(txCtx, c.Scope, c.PartyID, c.Role.Code); err != nil {
			return fmt.Errorf("%w: %v", ErrUnauthorized, err)
		}
		previous, found, err := h.Receipts.Find(txCtx, c.Scope.TenantID, c.CommandID)
		if err != nil {
			return err
		}
		if found {
			if previous.RequestHash != hash {
				return ErrIdempotencyConflict
			}
			result = previous
			return nil
		}
		if err = h.Entities.RequireVerifiedLegalEntity(txCtx, c.Scope.TenantID, c.Role.LegalEntityID); err != nil {
			return err
		}
		party, revision, err := h.Parties.Load(txCtx, c.Scope.TenantID, c.PartyID)
		if err != nil {
			return err
		}
		if party.Tenant() != c.Scope.TenantID || party.ID() != c.PartyID {
			return org.ErrCrossTenant
		}
		if revision != c.ExpectedRevision {
			return org.ErrConflict
		}
		if err = party.ApproveRole(c.Role, c.OccurredAt); err != nil {
			return err
		}
		newRevision, err := h.Parties.Save(txCtx, party, revision)
		if err != nil {
			return err
		}
		for _, event := range party.Pending() {
			if err = h.Outbox.EnqueueORGEvent(txCtx, event); err != nil {
				return err
			}
		}
		result = Receipt{CommandID: c.CommandID, ResultType: ResultRoleApproval, PartyID: c.PartyID, NewRevision: newRevision, RequestHash: hash}
		if err = h.Receipts.Save(txCtx, c.Scope.TenantID, result); err != nil {
			return err
		}
		mutated = party
		return nil
	})
	if err != nil {
		return Receipt{}, err
	}
	if mutated != nil {
		mutated.Acknowledge()
	} // Only after durable transaction commit.
	return result, nil
}
