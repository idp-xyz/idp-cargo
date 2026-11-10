package application

import (
	"context"
	"fmt"
	"strings"
	"time"

	org "go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
)

// ResultPartyIdentity is the result type of the receipts of party identity commands.
const ResultPartyIdentity = "BusinessPartyIdentity"

// Party identity actions an authorizer decides on (ORG V0.4).
const (
	ActionRecordPartyIdentifier = "RecordPartyIdentifier"
	ActionRetirePartyIdentifier = "RetirePartyIdentifier"
	ActionSetPartyEnglishName   = "SetPartyEnglishName"
)

// Identity actions of PartyIdentityCommand.
const (
	IdentityRecordIdentifier = "RECORD_IDENTIFIER"
	IdentityRetireIdentifier = "RETIRE_IDENTIFIER"
	IdentitySetEnglishName   = "SET_ENGLISH_NAME"
)

var identityActions = map[string]string{
	IdentityRecordIdentifier: ActionRecordPartyIdentifier,
	IdentityRetireIdentifier: ActionRetirePartyIdentifier,
	IdentitySetEnglishName:   ActionSetPartyEnglishName,
}

// PartyIdentityCommand is one of the party identity commands; Action says which, and only its fields are set.
// ExpectedRevision is the party revision the caller read: identity is part of the party, so these commands
// advance the revision PartyRef carries.
type PartyIdentityCommand struct {
	CommandID        string
	Scope            Scope
	PartyID          org.PartyID
	ExpectedRevision int64
	Action           string
	Identifier       *org.PartyIdentifier `json:",omitempty"` // to record; RecordedBy and RecordedAt are set by ORG
	IdentifierType   org.IdentifierType   `json:",omitempty"` // to retire
	Country          string               `json:",omitempty"` // to retire
	EnglishName      string               `json:",omitempty"`
	EvidenceRef      string               `json:",omitempty"` // for the English name
	Reason           string               `json:",omitempty"` // to retire
}

// IdentifierHolders finds the party that holds an active identifier. The database refuses a second holder as
// well; asking first gives the caller the holder to merge with.
type IdentifierHolders interface {
	Holder(ctx context.Context, tenant org.TenantID, t org.IdentifierType, country, value string) (org.PartyID, bool, error)
}

// MaintainPartyIdentity runs the party identity commands (MD-06; benchmark decision O1 of 2026-11-10): an
// identifier recorded or retired, or the English name set. Each command is one party revision, committed
// with its Outbox event and command receipt in one ORG transaction.
type MaintainPartyIdentity struct {
	Tx       TxRunner
	Parties  PartyStore
	Holders  IdentifierHolders
	Auth     PartyAuthorization
	Receipts Receipts
	Outbox   IntegrationOutbox
	Clock    Clock
}

func (h MaintainPartyIdentity) Handle(ctx context.Context, c PartyIdentityCommand) (Receipt, error) {
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	if h.Tx == nil || h.Parties == nil || h.Holders == nil || h.Auth == nil || h.Receipts == nil || h.Outbox == nil || h.Clock == nil {
		return Receipt{}, ErrMissingDependency
	}
	if c.Scope.TenantID == "" || strings.TrimSpace(c.Scope.ActorID) == "" || c.CommandID == "" {
		return Receipt{}, ErrUnauthenticated
	}
	if scoped, ok := h.Tx.(interface{ TenantScope() string }); ok && scoped.TenantScope() != string(c.Scope.TenantID) {
		return Receipt{}, ErrUnauthorized
	}
	action, known := identityActions[c.Action]
	if !known || c.PartyID == "" || c.ExpectedRevision < 1 || (c.Action == IdentityRecordIdentifier && c.Identifier == nil) {
		return Receipt{}, fmt.Errorf("%w: action %q, party %q, expected revision %d", ErrInvalidCommand, c.Action, c.PartyID, c.ExpectedRevision)
	}
	if err := h.Auth.RequirePartyAction(ctx, c.Scope, action, c.PartyID); err != nil {
		return Receipt{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	hash, err := requestHash(c)
	if err != nil {
		return Receipt{}, err
	}
	var result Receipt
	var mutated *org.BusinessParty
	err = h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
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
		party, revision, err := h.Parties.Load(txCtx, c.Scope.TenantID, c.PartyID)
		if err != nil {
			return err
		}
		if revision != c.ExpectedRevision {
			return fmt.Errorf("%w: party %s is at revision %d, not %d", org.ErrConflict, c.PartyID, revision, c.ExpectedRevision)
		}
		now := h.Clock.Now().UTC().Truncate(time.Microsecond)
		switch c.Action {
		case IdentityRecordIdentifier:
			id := *c.Identifier
			if err = party.RecordIdentifier(id, c.Scope.ActorID, now); err != nil {
				return err
			}
			if recorded, ok := party.Identifier(id.Type, strings.TrimSpace(id.Country)); ok && !recorded.None {
				holder, held, err := h.Holders.Holder(txCtx, c.Scope.TenantID, recorded.Type, recorded.Country, recorded.Value)
				if err != nil {
					return err
				}
				if held && holder != c.PartyID {
					return fmt.Errorf("%w: %s is held by party %s", org.ErrIdentifierInUse, recorded.Key(), holder)
				}
			}
		case IdentityRetireIdentifier:
			err = party.RetireIdentifier(c.IdentifierType, c.Country, c.Reason, c.Scope.ActorID, now)
		case IdentitySetEnglishName:
			err = party.SetEnglishName(c.EnglishName, c.EvidenceRef, now)
		}
		if err != nil {
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
		result = Receipt{CommandID: c.CommandID, ResultType: ResultPartyIdentity, PartyID: c.PartyID, NewRevision: newRevision, RequestHash: hash}
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
	}
	return result, nil
}

// ReadPartyIdentity answers the party identity query: the party with its names, status, active identifiers
// and revision, read as one consistent view.
type ReadPartyIdentity struct {
	Tx      TxRunner
	Parties PartyReader
}

func (h ReadPartyIdentity) Handle(ctx context.Context, tenant org.TenantID, party org.PartyID) (*org.BusinessParty, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if h.Tx == nil || h.Parties == nil {
		return nil, 0, ErrMissingDependency
	}
	if tenant == "" {
		return nil, 0, ErrUnauthenticated
	}
	if party == "" {
		return nil, 0, fmt.Errorf("%w: no party", ErrInvalidQuery)
	}
	if scoped, ok := h.Tx.(interface{ TenantScope() string }); ok && scoped.TenantScope() != string(tenant) {
		return nil, 0, ErrUnauthorized
	}
	var found *org.BusinessParty
	var revision int64
	err := h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var err error
		found, revision, err = h.Parties.Read(txCtx, tenant, party)
		return err
	})
	return found, revision, err
}
