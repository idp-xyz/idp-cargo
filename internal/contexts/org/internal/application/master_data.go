package application

import (
	"context"
	"fmt"
	"strings"
	"time"

	org "go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
)

// The master data commands (since V2.106): a party registered, activated, its legal name corrected, suspended and
// archived; a legal entity registered and verified. Each is one revision, committed with its Outbox event and its
// receipt in one ORG transaction, after the actor is authorized and before an earlier command is replayed.

// Result types of the master data receipts.
const (
	ResultParty       = "BusinessParty"
	ResultLegalEntity = "LegalEntity"
)

// Master data actions an authorizer decides on, and the actions of MasterDataCommand.
const (
	ActionRegisterParty         = "RegisterParty"
	ActionActivateParty         = "ActivateParty"
	ActionCorrectPartyLegalName = "CorrectPartyLegalName"
	ActionSuspendParty          = "SuspendParty"
	ActionArchiveParty          = "ArchiveParty"
	ActionRegisterLegalEntity   = "RegisterLegalEntity"
	ActionVerifyLegalEntity     = "VerifyLegalEntity"
)

// MasterDataCommand is one master data command; Action says which, and only its fields are set. Subject is the party
// or the legal entity. ExpectedRevision is 0 for a registration and the revision the caller read otherwise.
type MasterDataCommand struct {
	CommandID        string
	Scope            Scope
	Action           string
	Subject          string
	ExpectedRevision int64
	Name             string `json:",omitempty"` // the legal or registered name
	Country          string `json:",omitempty"` // the country of registration or jurisdiction
	EvidenceRef      string `json:",omitempty"`
	Reason           string `json:",omitempty"`
}

// PartyRegistry adds a party.
type PartyRegistry interface {
	Insert(context.Context, *org.BusinessParty) (int64, error)
}

// LegalEntityStore loads, adds and saves legal entities; Load locks the row.
type LegalEntityStore interface {
	Load(context.Context, org.TenantID, org.LegalEntityID) (*org.LegalEntity, int64, error)
	Insert(context.Context, *org.LegalEntity) (int64, error)
	Save(context.Context, *org.LegalEntity, int64) (int64, error)
}

// MaintainMasterData runs the master data commands.
type MaintainMasterData struct {
	Tx            TxRunner
	Parties       PartyStore
	Registry      PartyRegistry
	LegalEntities LegalEntityStore
	Auth          PartyAuthorization
	Receipts      Receipts
	Outbox        IntegrationOutbox
	Clock         Clock
}

var masterDataActions = map[string]bool{ActionRegisterParty: true, ActionActivateParty: true, ActionCorrectPartyLegalName: true, ActionSuspendParty: true,
	ActionArchiveParty: true, ActionRegisterLegalEntity: true, ActionVerifyLegalEntity: true}

func (h MaintainMasterData) Handle(ctx context.Context, c MasterDataCommand) (Receipt, error) {
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	if h.Tx == nil || h.Parties == nil || h.Registry == nil || h.LegalEntities == nil || h.Auth == nil || h.Receipts == nil || h.Outbox == nil || h.Clock == nil {
		return Receipt{}, ErrMissingDependency
	}
	if c.Scope.TenantID == "" || strings.TrimSpace(c.Scope.ActorID) == "" || c.CommandID == "" {
		return Receipt{}, ErrUnauthenticated
	}
	if scoped, ok := h.Tx.(interface{ TenantScope() string }); ok && scoped.TenantScope() != string(c.Scope.TenantID) {
		return Receipt{}, ErrUnauthorized
	}
	registering := c.Action == ActionRegisterParty || c.Action == ActionRegisterLegalEntity
	if !masterDataActions[c.Action] || strings.TrimSpace(c.Subject) == "" || len(c.Subject) > 128 || (registering != (c.ExpectedRevision == 0)) || c.ExpectedRevision < 0 {
		return Receipt{}, fmt.Errorf("%w: action %q, subject %q, expected revision %d", ErrInvalidCommand, c.Action, c.Subject, c.ExpectedRevision)
	}
	if err := h.Auth.RequirePartyAction(ctx, c.Scope, c.Action, org.PartyID(c.Subject)); err != nil {
		return Receipt{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	hash, err := requestHash(c)
	if err != nil {
		return Receipt{}, err
	}
	var result Receipt
	var acknowledge func()
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
		now := h.Clock.Now().UTC().Truncate(time.Microsecond)
		var revision int64
		var events []org.Event
		resultType := ResultParty
		switch c.Action {
		case ActionRegisterParty:
			p, err := org.RegisterParty(c.Scope.TenantID, org.PartyID(c.Subject), c.Name, c.Country, now)
			if err != nil {
				return err
			}
			if revision, err = h.Registry.Insert(txCtx, p); err != nil {
				return err
			}
			events, acknowledge = p.Pending(), p.Acknowledge
		case ActionActivateParty, ActionCorrectPartyLegalName, ActionSuspendParty, ActionArchiveParty:
			p, stored, err := h.Parties.Load(txCtx, c.Scope.TenantID, org.PartyID(c.Subject))
			if err != nil {
				return err
			}
			if stored != c.ExpectedRevision {
				return fmt.Errorf("%w: party %s is at revision %d, not %d", org.ErrConflict, c.Subject, stored, c.ExpectedRevision)
			}
			switch c.Action {
			case ActionActivateParty:
				err = p.Activate(c.EvidenceRef, now)
			case ActionCorrectPartyLegalName:
				err = p.CorrectLegalName(c.Name, c.EvidenceRef, now)
			case ActionSuspendParty:
				err = p.Suspend(c.Reason, now)
			case ActionArchiveParty:
				err = p.Archive(c.Reason, now)
			}
			if err != nil {
				return err
			}
			if revision, err = h.Parties.Save(txCtx, p, stored); err != nil {
				return err
			}
			events, acknowledge = p.Pending(), p.Acknowledge
		case ActionRegisterLegalEntity:
			resultType = ResultLegalEntity
			e, err := org.RegisterLegalEntity(c.Scope.TenantID, org.LegalEntityID(c.Subject), strings.TrimSpace(c.Name), c.Country, c.EvidenceRef, now)
			if err != nil {
				return err
			}
			if revision, err = h.LegalEntities.Insert(txCtx, e); err != nil {
				return err
			}
			events, acknowledge = e.Pending(), e.Acknowledge
		case ActionVerifyLegalEntity:
			resultType = ResultLegalEntity
			e, stored, err := h.LegalEntities.Load(txCtx, c.Scope.TenantID, org.LegalEntityID(c.Subject))
			if err != nil {
				return err
			}
			if stored != c.ExpectedRevision {
				return fmt.Errorf("%w: legal entity %s is at revision %d, not %d", org.ErrConflict, c.Subject, stored, c.ExpectedRevision)
			}
			if err = e.Verify(c.Scope.ActorID, now); err != nil {
				return fmt.Errorf("%w: legal entity %s is %s; only a DRAFT one is verified", err, c.Subject, e.Status())
			}
			if revision, err = h.LegalEntities.Save(txCtx, e, stored); err != nil {
				return err
			}
			events, acknowledge = e.Pending(), e.Acknowledge
		}
		for _, event := range events {
			if err = h.Outbox.EnqueueORGEvent(txCtx, event); err != nil {
				return err
			}
		}
		result = Receipt{CommandID: c.CommandID, ResultType: resultType, PartyID: org.PartyID(c.Subject), NewRevision: revision, RequestHash: hash}
		return h.Receipts.Save(txCtx, c.Scope.TenantID, result)
	})
	if err != nil {
		return Receipt{}, err
	}
	if acknowledge != nil {
		acknowledge()
	}
	return result, nil
}
