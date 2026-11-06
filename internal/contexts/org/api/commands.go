package api

import (
	"context"
	"errors"
	"time"
)

// ApproveRole asks ORG to approve a partner role for a business party. TenantID and ActorID must come
// from a verified principal, never from a request body.
type ApproveRole struct {
	CommandID        string
	TenantID         string
	ActorID          string
	LegalEntityID    string
	PartyID          string
	ExpectedRevision int64
	RoleCode         string
	ValidFrom        time.Time
	ValidUntil       time.Time // zero means open-ended
	EvidenceRef      string
	OccurredAt       time.Time
}

// Receipt confirms a committed command. Replaying the same command returns the same receipt.
type Receipt struct {
	CommandID   string
	PartyID     string
	NewRevision int64
}

// Commands is ORG's command entry point.
type Commands interface {
	ApproveRole(ctx context.Context, cmd ApproveRole) (Receipt, error)
}

// Authorizer decides whether a verified actor may approve a role. The identity and policy layer
// supplies it.
type Authorizer interface {
	AuthorizeRoleApproval(ctx context.Context, tenantID, actorID, partyID, roleCode string) error
}

// Error kinds returned by Commands, wrapped around the original cause. Transports map these, not the
// causes, to their status codes.
var (
	ErrUnauthenticated     = errors.New("org: unauthenticated")
	ErrUnauthorized        = errors.New("org: unauthorized")
	ErrNotFound            = errors.New("org: not found")
	ErrConflict            = errors.New("org: revision conflict")
	ErrIdempotencyConflict = errors.New("org: command id reused with a different payload")
	ErrRejected            = errors.New("org: rejected by business rules")
)
