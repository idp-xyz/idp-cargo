package application

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	org "go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
)

// ResultTransferRegister is the result type of the receipts of ContactTransferRegister v1 commands.
const ResultTransferRegister = "ContactTransferRegister"

// Contact transfer actions an authorizer decides on (ORG V0.7); the subject is the dispatch or the safeguard.
const (
	ActionReserveContactTransfers = "ReserveContactTransfers"
	ActionReleaseContactTransfers = "ReleaseContactTransfers"
	ActionRecordTransferSafeguard = "RecordTransferSafeguard"
)

const (
	eventTransfersReserved = "ContactTransfersReserved.v1"
	eventTransfersReleased = "ContactTransfersReleased.v1"
	eventSafeguardRecorded = "TransferSafeguardRecorded.v1"
	registerSubject        = "contact-transfer-register:"
)

// TransferStore keeps the register: one row per tenant and year, locked while a year's reservations change, and the
// reservations of each dispatch by contact key.
type TransferStore interface {
	// Lock creates the year's register row if needed and locks it; it returns the revision.
	Lock(ctx context.Context, tenant org.TenantID, year int) (int64, error)
	// Advance moves the locked register's revision on.
	Advance(ctx context.Context, tenant org.TenantID, year int, revision int64) (int64, error)
	// Persons counts the distinct contact keys of the year with a reservation in force.
	Persons(ctx context.Context, tenant org.TenantID, year int) (int64, error)
	// Held returns which of the keys hold a reservation in force in the year.
	Held(ctx context.Context, tenant org.TenantID, year int, keys []string) (map[string]bool, error)
	// DispatchKeys returns the keys a dispatch holds in force, and the year they count in.
	DispatchKeys(ctx context.Context, tenant org.TenantID, dispatch string) (int, []string, error)
	// Reserve records the dispatch, once ever (ErrInvalidTransfer when it reserved before, in any year), and its keys.
	Reserve(ctx context.Context, tenant org.TenantID, year int, dispatch string, keys []string, by string, at, entered time.Time) error
	Release(ctx context.Context, tenant org.TenantID, year int, dispatch, by string, at, entered time.Time) (int64, error)
	Safeguard(ctx context.Context, tenant org.TenantID, at time.Time) (org.TransferSafeguard, bool, error)
	InsertSafeguard(ctx context.Context, tenant org.TenantID, s org.TransferSafeguard) error
	Revision(ctx context.Context, tenant org.TenantID, year int) (int64, error)
}

// TransferCommand is one of the register's commands.
type TransferCommand struct {
	CommandID  string
	Scope      Scope
	Action     string
	DispatchID string                 `json:",omitempty"`
	Keys       []string               `json:",omitempty"`
	Safeguard  *org.TransferSafeguard `json:",omitempty"`
	OccurredAt time.Time
}

// TransferResult is what a register command did: the year, the persons counted after it and the state.
type TransferResult struct {
	Receipt
	Year    int
	Persons int64
	State   org.TransferState
}

// MaintainTransferRegister runs ContactTransferRegister v1's commands, each committed with its Outbox event and
// command receipt in one ORG transaction, the year's reservations under the register row's lock.
type MaintainTransferRegister struct {
	Tx         TxRunner
	Store      TransferStore
	Auth       PartyAuthorization
	Receipts   Receipts
	Outbox     IntegrationOutbox
	Clock      Clock
	Thresholds org.TransferThresholds
}

func (h MaintainTransferRegister) Handle(ctx context.Context, c TransferCommand) (TransferResult, error) {
	if err := ctx.Err(); err != nil {
		return TransferResult{}, err
	}
	if h.Tx == nil || h.Store == nil || h.Auth == nil || h.Receipts == nil || h.Outbox == nil || h.Clock == nil || !h.Thresholds.Valid() {
		return TransferResult{}, ErrMissingDependency
	}
	if c.Scope.TenantID == "" || strings.TrimSpace(c.Scope.ActorID) == "" || c.CommandID == "" || c.OccurredAt.IsZero() {
		return TransferResult{}, ErrUnauthenticated
	}
	if scoped, ok := h.Tx.(interface{ TenantScope() string }); ok && scoped.TenantScope() != string(c.Scope.TenantID) {
		return TransferResult{}, ErrUnauthorized
	}
	subject := c.DispatchID
	switch c.Action {
	case ActionReserveContactTransfers:
		if strings.TrimSpace(c.DispatchID) == "" || len(c.Keys) == 0 {
			return TransferResult{}, fmt.Errorf("%w: a reservation names its dispatch and contact keys", ErrInvalidCommand)
		}
		for _, k := range c.Keys {
			if !org.ValidContactKey(k) {
				return TransferResult{}, fmt.Errorf("%w: contact key %q", org.ErrInvalidTransfer, k)
			}
		}
	case ActionReleaseContactTransfers:
		if strings.TrimSpace(c.DispatchID) == "" {
			return TransferResult{}, fmt.Errorf("%w: a release names its dispatch", ErrInvalidCommand)
		}
	case ActionRecordTransferSafeguard:
		if c.Safeguard == nil {
			return TransferResult{}, ErrInvalidCommand
		}
		subject = c.Safeguard.SafeguardID
	default:
		return TransferResult{}, fmt.Errorf("%w: action %q", ErrInvalidCommand, c.Action)
	}
	if err := h.Auth.RequirePartyAction(ctx, c.Scope, c.Action, org.PartyID(subject)); err != nil {
		return TransferResult{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	hash, err := requestHash(c)
	if err != nil {
		return TransferResult{}, err
	}
	var result TransferResult
	err = h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		previous, found, err := h.Receipts.Find(txCtx, c.Scope.TenantID, c.CommandID)
		if err != nil {
			return err
		}
		if found {
			if previous.RequestHash != hash {
				return ErrIdempotencyConflict
			}
			result = TransferResult{Receipt: previous, Persons: previous.Persons, State: org.TransferState(previous.State)}
			result.Year, _ = strconv.Atoi(strings.TrimPrefix(string(previous.PartyID), registerSubject)) // 0 for a safeguard
			return nil
		}
		entered := h.Clock.Now().UTC().Truncate(time.Microsecond)
		at := c.OccurredAt.UTC().Truncate(time.Microsecond)
		event := org.Event{At: at, TenantID: c.Scope.TenantID, EntryID: subject}
		switch c.Action {
		case ActionRecordTransferSafeguard:
			s := *c.Safeguard
			s.RecordedBy, s.RecordedAt, s.EnteredAt = c.Scope.ActorID, at, entered
			if !s.Valid() {
				return fmt.Errorf("%w: safeguard kind STANDARD_CONTRACT or CERTIFICATION, reference, validity and evidence", org.ErrInvalidTransfer)
			}
			if err = h.Store.InsertSafeguard(txCtx, c.Scope.TenantID, s); err != nil {
				return err
			}
			event.Type, event.SubjectID, event.EntryVersion = eventSafeguardRecorded, registerSubject+"safeguards", 1
			result = TransferResult{Receipt: Receipt{CommandID: c.CommandID, ResultType: ResultTransferRegister, PartyID: org.PartyID(event.SubjectID),
				NewRevision: 1, EntryID: subject, EntryVersion: 1, RequestHash: hash}}
		default:
			// the year and the safeguard in force are ORG's own: the transfer happens now, whatever business time the client
			// gives
			year := org.TransferYear(entered)
			if c.Action == ActionReleaseContactTransfers {
				if year, _, err = h.Store.DispatchKeys(txCtx, c.Scope.TenantID, c.DispatchID); err != nil {
					return err
				}
				if year == 0 {
					return fmt.Errorf("%w: dispatch %s holds no reservation", org.ErrInvalidTransfer, c.DispatchID)
				}
			}
			revision, err := h.Store.Lock(txCtx, c.Scope.TenantID, year)
			if err != nil {
				return err
			}
			persons, err := h.Store.Persons(txCtx, c.Scope.TenantID, year)
			if err != nil {
				return err
			}
			_, inForce, err := h.Store.Safeguard(txCtx, c.Scope.TenantID, entered)
			if err != nil {
				return err
			}
			state := h.Thresholds.State(persons, inForce)
			if c.Action == ActionReserveContactTransfers {
				keys := slices.Clone(c.Keys)
				slices.Sort(keys)
				keys = slices.Compact(keys)
				live, err := h.Store.Held(txCtx, c.Scope.TenantID, year, keys)
				if err != nil {
					return err
				}
				var added int64
				for _, k := range keys {
					if !live[k] {
						added++
					}
				}
				if state, err = h.Thresholds.Admit(persons, added, inForce); err != nil {
					return err
				}
				if err = h.Store.Reserve(txCtx, c.Scope.TenantID, year, c.DispatchID, keys, c.Scope.ActorID, at, entered); err != nil {
					return err
				}
				persons += added
				event.Type = eventTransfersReserved
			} else {
				released, err := h.Store.Release(txCtx, c.Scope.TenantID, year, c.DispatchID, c.Scope.ActorID, at, entered)
				if err != nil {
					return err
				}
				if released == 0 { // released by another command while this one waited for the lock
					return fmt.Errorf("%w: dispatch %s holds no reservation", org.ErrInvalidTransfer, c.DispatchID)
				}
				if persons, err = h.Store.Persons(txCtx, c.Scope.TenantID, year); err != nil {
					return err
				}
				state = h.Thresholds.State(persons, inForce)
				event.Type = eventTransfersReleased
			}
			if revision, err = h.Store.Advance(txCtx, c.Scope.TenantID, year, revision); err != nil {
				return err
			}
			event.SubjectID, event.EntryVersion, event.Revision = registerSubject+strconv.Itoa(year), persons, revision
			result = TransferResult{Receipt: Receipt{CommandID: c.CommandID, ResultType: ResultTransferRegister, PartyID: org.PartyID(event.SubjectID),
				NewRevision: revision, RequestHash: hash, Persons: persons, State: string(state)}, Year: year, Persons: persons, State: state}
		}
		if err = h.Outbox.EnqueueORGEvent(txCtx, event); err != nil {
			return err
		}
		return h.Receipts.Save(txCtx, c.Scope.TenantID, result.Receipt)
	})
	if err != nil {
		return TransferResult{}, err
	}
	return result, nil
}

// TransferRegisterView is the register of a tenant and year as ContactTransferRegister v1 reports it.
type TransferRegisterView struct {
	Year         int
	Revision     int64
	Persons      int64
	State        org.TransferState
	Safeguard    *org.TransferSafeguard
	DispatchKeys []string
}

// ReadTransferRegister answers GetContactTransferRegister in one snapshot.
type ReadTransferRegister struct {
	Tx         TxRunner
	Store      TransferStore
	Clock      Clock
	Thresholds org.TransferThresholds
}

func (h ReadTransferRegister) Handle(ctx context.Context, tenant org.TenantID, year int, dispatch string) (TransferRegisterView, error) {
	if h.Tx == nil || h.Store == nil || h.Clock == nil || !h.Thresholds.Valid() {
		return TransferRegisterView{}, ErrMissingDependency
	}
	if tenant == "" {
		return TransferRegisterView{}, ErrUnauthenticated
	}
	now := h.Clock.Now()
	if year == 0 {
		year = org.TransferYear(now)
	}
	view := TransferRegisterView{Year: year}
	err := h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var err error
		if view.Revision, err = h.Store.Revision(txCtx, tenant, year); err != nil {
			return err
		}
		if view.Persons, err = h.Store.Persons(txCtx, tenant, year); err != nil {
			return err
		}
		s, inForce, err := h.Store.Safeguard(txCtx, tenant, now)
		if err != nil {
			return err
		}
		if inForce {
			view.Safeguard = &s
		}
		view.State = h.Thresholds.State(view.Persons, inForce)
		if dispatch != "" {
			// a dispatch's keys, whichever year they count in
			if _, view.DispatchKeys, err = h.Store.DispatchKeys(txCtx, tenant, dispatch); err != nil {
				return err
			}
		}
		return nil
	})
	return view, err
}
