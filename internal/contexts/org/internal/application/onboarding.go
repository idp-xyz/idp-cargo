package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	bentorepo "go.idp.xyz/idp-bento-go/repository"
	org "go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
)

// Customer onboarding (V2.102): the record of a consultant's proposal, the owner's confirmation and each module's
// change set and state. Every command takes the onboarding's lock first, so that a retry that overlaps its first try
// waits for it and answers what it did.

// Actions an authorizer decides on; the subject is the onboarding. RecordOnboardingModule is the platform's own, not the
// owner's: it is a permission of its own.
const (
	ActionProposeOnboarding      = "ProposeOnboarding"
	ActionConfirmOnboarding      = "ConfirmOnboarding"
	ActionRecordOnboardingModule = "RecordOnboardingModule"
)

const (
	eventOnboardingProposed  = "OnboardingProposed.v1"
	eventOnboardingConfirmed = "OnboardingConfirmed.v1"
)

// OnboardingRecord is an onboarding as stored.
type OnboardingRecord struct {
	Onboarding   org.Onboarding
	CommandID    string
	Hash         string
	Confirmation *org.Confirmation
	// ConfirmCommandID is the command that confirmed it.
	ConfirmCommandID string
	Modules          []org.ModuleState
}

// State is the onboarding's state.
func (r OnboardingRecord) State() string { return org.StateOf(r.Confirmation != nil, r.Modules) }

// OnboardingStore keeps the onboardings. Lock takes the onboarding's lock until the transaction ends.
type OnboardingStore interface {
	Lock(ctx context.Context, tenant org.TenantID, id string) error
	Find(ctx context.Context, tenant org.TenantID, id string) (OnboardingRecord, bool, error)
	List(ctx context.Context, tenant org.TenantID) ([]OnboardingRecord, error)
	Insert(ctx context.Context, tenant org.TenantID, o org.Onboarding, commandID, hash string) error
	InsertConfirmation(ctx context.Context, tenant org.TenantID, id string, o org.Onboarding, commandID string, c org.Confirmation) error
	InsertModule(ctx context.Context, tenant org.TenantID, id string, confirmation org.Confirmation, m org.ModuleState, at time.Time) error
}

// Onboardings runs the onboarding commands and queries.
type Onboardings struct {
	Tx     TxRunner
	Store  OnboardingStore
	Auth   PartyAuthorization
	Outbox IntegrationOutbox
	Clock  Clock
}

// ProposeCommand is a consultant's proposal as asked.
type ProposeCommand struct {
	CommandID  string
	Scope      Scope
	Onboarding org.Onboarding
}

// ConfirmCommand is the owner's confirmation as asked.
type ConfirmCommand struct {
	CommandID    string
	Scope        Scope
	OnboardingID string
	Acknowledged string
	// PlanSHA256 is the digest of the plan the owner read (GetOnboarding, or the proposal's receipt).
	PlanSHA256 string
	// Administrator is the owner's declaration of the customer's single administrator, or empty.
	Administrator string
}

// ModuleCommand records what a module did for a confirmed onboarding.
type ModuleCommand struct {
	CommandID    string
	Scope        Scope
	OnboardingID string
	Module       string
	ChangeID     string
	State        string
}

// OnboardingResult is the onboarding after a command, and the notice it gives.
type OnboardingResult struct {
	Record   OnboardingRecord
	Notice   string
	Revision int64
}

func (h Onboardings) ready(scope Scope, commandID string) error {
	if h.Tx == nil || h.Store == nil || h.Auth == nil || h.Outbox == nil || h.Clock == nil {
		return ErrMissingDependency
	}
	if scope.TenantID == "" || strings.TrimSpace(scope.ActorID) == "" || commandID == "" {
		return ErrUnauthenticated
	}
	if scoped, ok := h.Tx.(interface{ TenantScope() string }); ok && scoped.TenantScope() != string(scope.TenantID) {
		return ErrUnauthorized
	}
	return nil
}

func proposalHash(o org.Onboarding) (string, error) {
	// the time is ORG's, and the plan is what the build resolved from what was asked: neither is part of what the caller sent, so a
	// replay of the same proposal, with the same command id, after a deploy that resolves it otherwise, answers the stored record
	o.ProposedAt, o.Plan, o.PlanSHA256 = time.Time{}, nil, ""
	raw, err := json.Marshal(o)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// revision counts the records an onboarding has: its proposal, its confirmation and each module row.
func revision(r OnboardingRecord) int64 {
	n := int64(1 + len(r.Modules))
	if r.Confirmation != nil {
		n++
	}
	return n
}

// Propose records a consultant's proposal. The same command again answers the record; the same onboarding id with
// another content is refused.
func (h Onboardings) Propose(ctx context.Context, c ProposeCommand) (OnboardingResult, error) {
	if err := ctx.Err(); err != nil {
		return OnboardingResult{}, err
	}
	if err := h.ready(c.Scope, c.CommandID); err != nil {
		return OnboardingResult{}, err
	}
	o := c.Onboarding
	o.ProposedBy = c.Scope.ActorID
	if err := o.Validate(); err != nil {
		return OnboardingResult{}, err
	}
	if err := h.Auth.RequirePartyAction(ctx, c.Scope, ActionProposeOnboarding, org.PartyID(o.ID)); err != nil {
		return OnboardingResult{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	hash, err := proposalHash(o)
	if err != nil {
		return OnboardingResult{}, err
	}
	var out OnboardingResult
	err = h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		if err := h.Store.Lock(txCtx, c.Scope.TenantID, o.ID); err != nil {
			return err
		}
		rec, found, err := h.Store.Find(txCtx, c.Scope.TenantID, o.ID)
		if err != nil {
			return err
		}
		if found {
			if rec.CommandID != c.CommandID || rec.Hash != hash {
				return fmt.Errorf("%w: onboarding %s exists with another command or content; read it (GetOnboarding), or propose with another onboarding id", ErrIdempotencyConflict, o.ID)
			}
			out = OnboardingResult{Record: rec, Notice: org.StrictNotice(rec.Onboarding.Profile, rec.Onboarding.FinanceStaff), Revision: revision(rec)}
			return nil
		}
		o.ProposedAt = h.Clock.Now().UTC().Truncate(time.Microsecond)
		o.PlanSHA256 = org.PlanDigest(o.Plan)
		if err = h.Store.Insert(txCtx, c.Scope.TenantID, o, c.CommandID, hash); err != nil {
			return err
		}
		if err = h.Outbox.EnqueueORGEvent(txCtx, org.Event{Type: eventOnboardingProposed, At: o.ProposedAt, TenantID: c.Scope.TenantID, SubjectID: o.ID}); err != nil {
			return err
		}
		rec = OnboardingRecord{Onboarding: o, CommandID: c.CommandID, Hash: hash}
		out = OnboardingResult{Record: rec, Notice: org.StrictNotice(o.Profile, o.FinanceStaff), Revision: revision(rec)}
		return nil
	})
	return out, err
}

// Confirm records the owner's confirmation. The confirmer is not the proposer, and acknowledges the risk statement
// exactly as proposed; they declare a single administrator, who is not the proposer, if and only if the proposal says
// the customer does. The same actor with the same text and administrator again answers the record, whatever the command id.
func (h Onboardings) Confirm(ctx context.Context, c ConfirmCommand) (OnboardingResult, error) {
	if err := ctx.Err(); err != nil {
		return OnboardingResult{}, err
	}
	if err := h.ready(c.Scope, c.CommandID); err != nil {
		return OnboardingResult{}, err
	}
	if c.OnboardingID == "" {
		return OnboardingResult{}, fmt.Errorf("%w: a confirmation names its onboarding", ErrInvalidCommand)
	}
	if !org.ValidActor(c.Scope.ActorID) {
		return OnboardingResult{}, fmt.Errorf("%w: the owner's actor id is 1 to 128 bytes, without spaces around it; give it as the identity platform names the person", org.ErrInvalidOnboarding)
	}
	if err := h.Auth.RequirePartyAction(ctx, c.Scope, ActionConfirmOnboarding, org.PartyID(c.OnboardingID)); err != nil {
		return OnboardingResult{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	var out OnboardingResult
	err := h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		if err := h.Store.Lock(txCtx, c.Scope.TenantID, c.OnboardingID); err != nil {
			return err
		}
		rec, found, err := h.Store.Find(txCtx, c.Scope.TenantID, c.OnboardingID)
		switch {
		case err != nil:
			return err
		case !found:
			return fmt.Errorf("%w: onboarding %s; list the tenant's onboardings (ListOnboardings) for the ids", bentorepo.ErrNotFound, c.OnboardingID)
		}
		if k := rec.Confirmation; k != nil {
			if k.By != c.Scope.ActorID || k.AcknowledgedText != c.Acknowledged || k.Administrator != c.Administrator || k.PlanSHA256 != c.PlanSHA256 {
				return fmt.Errorf("%w: onboarding %s was confirmed by %s already; read it (GetOnboarding)", ErrIdempotencyConflict, c.OnboardingID, k.By)
			}
			out = OnboardingResult{Record: rec, Notice: org.StrictNotice(rec.Onboarding.Profile, rec.Onboarding.FinanceStaff), Revision: revision(rec)}
			return nil
		}
		switch {
		case c.Scope.ActorID == rec.Onboarding.ProposedBy:
			return fmt.Errorf("%w: onboarding %s was proposed by %s, who may not confirm it; have the customer's appointed owner confirm it", org.ErrInvalidOnboarding,
				c.OnboardingID, c.Scope.ActorID)
		case c.Acknowledged != rec.Onboarding.RiskStatement:
			return fmt.Errorf("%w: the text acknowledged is not the risk statement proposed; read the statement (GetOnboarding), and acknowledge it exactly as it is", org.ErrInvalidOnboarding)
		case c.PlanSHA256 != rec.Onboarding.PlanSHA256:
			return fmt.Errorf("%w: the SHA-256 given (%q) is not the digest of the plan proposed (%s); read the onboarding again (GetOnboarding shows the plan and its PlanSHA256), "+
				"and confirm what it shows", org.ErrInvalidOnboarding, c.PlanSHA256, rec.Onboarding.PlanSHA256)
		case rec.Onboarding.SingleAdministrator && strings.TrimSpace(c.Administrator) == "":
			return fmt.Errorf("%w: the proposal says the customer declares a single administrator, and the owner names them when confirming; give the administrator's actor id "+
				"(AdministratorID), or have the consultant propose a new onboarding, under another id, that declares none", org.ErrInvalidOnboarding)
		case !rec.Onboarding.SingleAdministrator && c.Administrator != "":
			return fmt.Errorf("%w: the proposal does not say the customer declares a single administrator, so none is named when confirming; leave AdministratorID empty, or have "+
				"the consultant propose a new onboarding, under another id, that declares one", org.ErrInvalidOnboarding)
		case rec.Onboarding.SingleAdministrator && (strings.TrimSpace(c.Administrator) != c.Administrator || len(c.Administrator) > 128):
			return fmt.Errorf("%w: the administrator is an actor id of 1 to 128 bytes, without spaces around it; give it as the identity platform names the person", org.ErrInvalidOnboarding)
		case rec.Onboarding.SingleAdministrator && c.Administrator == rec.Onboarding.ProposedBy:
			return fmt.Errorf("%w: the administrator the owner names is not the consultant %s who proposed the onboarding; name the customer's own administrator", org.ErrInvalidOnboarding,
				rec.Onboarding.ProposedBy)
		}
		k := org.Confirmation{By: c.Scope.ActorID, At: h.Clock.Now().UTC().Truncate(time.Microsecond), AcknowledgedText: c.Acknowledged, Administrator: c.Administrator, PlanSHA256: c.PlanSHA256}
		if k.At.Before(rec.Onboarding.ProposedAt) {
			k.At = rec.Onboarding.ProposedAt
		}
		if err = h.Store.InsertConfirmation(txCtx, c.Scope.TenantID, c.OnboardingID, rec.Onboarding, c.CommandID, k); err != nil {
			return err
		}
		if err = h.Outbox.EnqueueORGEvent(txCtx, org.Event{Type: eventOnboardingConfirmed, At: k.At, TenantID: c.Scope.TenantID, SubjectID: c.OnboardingID}); err != nil {
			return err
		}
		rec.Confirmation, rec.ConfirmCommandID = &k, c.CommandID
		out = OnboardingResult{Record: rec, Notice: org.StrictNotice(rec.Onboarding.Profile, rec.Onboarding.FinanceStaff), Revision: revision(rec)}
		return nil
	})
	return out, err
}

// RecordModule records a module's change set and state for a confirmed onboarding, by the confirmer acting as the
// platform (the permission RecordOnboardingModule, which no person holds), once for each state.
// A state already recorded with the same change set answers the record again.
func (h Onboardings) RecordModule(ctx context.Context, c ModuleCommand) (OnboardingResult, error) {
	if err := ctx.Err(); err != nil {
		return OnboardingResult{}, err
	}
	if err := h.ready(c.Scope, c.CommandID); err != nil {
		return OnboardingResult{}, err
	}
	if !slices.Contains(org.Modules, c.Module) || (c.State != org.ModuleProposed && c.State != org.ModuleApproved) || c.OnboardingID == "" {
		return OnboardingResult{}, fmt.Errorf("%w: a module record names its onboarding, FIN or SEA, and the state PROPOSED or APPROVED", ErrInvalidCommand)
	}
	if err := h.Auth.RequirePartyAction(ctx, c.Scope, ActionRecordOnboardingModule, org.PartyID(c.OnboardingID)); err != nil {
		return OnboardingResult{}, fmt.Errorf("%w: %v; only the platform's onboarding records a module's progress, from the module's own change set, so no person does: "+
			"send ConfirmOnboarding again, which completes what is missing", ErrUnauthorized, err)
	}
	var out OnboardingResult
	err := h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		if err := h.Store.Lock(txCtx, c.Scope.TenantID, c.OnboardingID); err != nil {
			return err
		}
		rec, found, err := h.Store.Find(txCtx, c.Scope.TenantID, c.OnboardingID)
		switch {
		case err != nil:
			return err
		case !found || rec.Confirmation == nil:
			return fmt.Errorf("%w: onboarding %s is not confirmed; the owner confirms it first (ConfirmOnboarding)", org.ErrInvalidOnboarding, c.OnboardingID)
		case rec.Confirmation.By != c.Scope.ActorID:
			return fmt.Errorf("%w: the modules of onboarding %s are recorded by its confirmer, %s", org.ErrInvalidOnboarding, c.OnboardingID, rec.Confirmation.By)
		case c.ChangeID != org.ModuleChangeID(c.OnboardingID, c.Module):
			return fmt.Errorf("%w: the change set of %s for onboarding %s is %s", org.ErrInvalidOnboarding, c.Module, c.OnboardingID, org.ModuleChangeID(c.OnboardingID, c.Module))
		}
		for _, m := range rec.Modules {
			if m.Module == c.Module && m.State == c.State {
				out = OnboardingResult{Record: rec, Notice: org.StrictNotice(rec.Onboarding.Profile, rec.Onboarding.FinanceStaff), Revision: revision(rec)}
				return nil
			}
		}
		m := org.ModuleState{Module: c.Module, State: c.State, ChangeID: c.ChangeID}
		if err = h.Store.InsertModule(txCtx, c.Scope.TenantID, c.OnboardingID, *rec.Confirmation, m, h.Clock.Now().UTC().Truncate(time.Microsecond)); err != nil {
			return err
		}
		rec.Modules = append(rec.Modules, m)
		out = OnboardingResult{Record: rec, Notice: org.StrictNotice(rec.Onboarding.Profile, rec.Onboarding.FinanceStaff), Revision: revision(rec)}
		return nil
	})
	return out, err
}

// Get answers one onboarding in a snapshot.
func (h Onboardings) Get(ctx context.Context, tenant org.TenantID, id string) (OnboardingRecord, error) {
	if h.Tx == nil || h.Store == nil {
		return OnboardingRecord{}, ErrMissingDependency
	}
	if tenant == "" || id == "" {
		return OnboardingRecord{}, ErrInvalidQuery
	}
	var rec OnboardingRecord
	err := h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var found bool
		var err error
		if rec, found, err = h.Store.Find(txCtx, tenant, id); err == nil && !found {
			err = fmt.Errorf("%w: onboarding %s; list the tenant's onboardings (ListOnboardings) for the ids", bentorepo.ErrNotFound, id)
		}
		return err
	})
	return rec, err
}

// List answers the tenant's onboardings in the order proposed, in a snapshot.
func (h Onboardings) List(ctx context.Context, tenant org.TenantID) ([]OnboardingRecord, error) {
	if h.Tx == nil || h.Store == nil {
		return nil, ErrMissingDependency
	}
	if tenant == "" {
		return nil, ErrInvalidQuery
	}
	var recs []OnboardingRecord
	err := h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var err error
		recs, err = h.Store.List(txCtx, tenant)
		return err
	})
	return recs, err
}
