// Package application runs FUL's use cases. Business rules stay in domain; this package orders the
// steps: replay check, authorization, questions to COM outside any FUL transaction (rule T3), and one
// transaction for the shipment, its Outbox events and the command receipt (rule T2).
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

	ful "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

var (
	ErrUnauthenticated     = errors.New("ful application: unauthenticated")
	ErrUnauthorized        = errors.New("ful application: unauthorized")
	ErrIdempotencyConflict = errors.New("ful application: command id reused with a different payload")
	ErrMissingDependency   = errors.New("ful application: required dependency missing")
	ErrNotPlannable        = errors.New("ful application: COM does not report the obligation version active, current and reserved for the shipment")
	ErrPlannedElsewhere    = errors.New("ful application: other shipments that are not cancelled plan all of the obligation, or another version of it")
	ErrInvalidQuery        = errors.New("ful application: invalid query")
)

// Scope is the verified caller of a command.
type Scope struct {
	TenantID ful.TenantID
	ActorID  string
}

type ShipmentAction string

const (
	ActionCreateShipmentFromObligation  ShipmentAction = "CreateShipmentFromObligation"
	ActionAddObligationToShipment       ShipmentAction = "AddObligationToShipment"
	ActionAmendShipmentPlan             ShipmentAction = "AmendShipmentPlan"
	ActionConfirmShipmentPlan           ShipmentAction = "ConfirmShipmentPlan"
	ActionReleaseObligationFromShipment ShipmentAction = "ReleaseObligationFromShipment"
	ActionChangeShipmentPart            ShipmentAction = "ChangeShipmentPart"
	ActionCancelShipment                ShipmentAction = "CancelShipment"
	ActionRecordConsolAttachment        ShipmentAction = "RecordConsolAttachment"
	ActionRecordConsolDetachment        ShipmentAction = "RecordConsolDetachment"
)

type Receipt struct {
	CommandID   string
	ShipmentID  ful.ShipmentID
	NewRevision int64
	RequestHash string
}

type TxRunner interface {
	WithinTransaction(context.Context, func(context.Context) error) error
}

// ShipmentStore loads and writes shipments. Load locks the shipment for a command; Read does not. Save
// writes the status and cancellation and what the shipment added or released since it was loaded.
// PlannedElsewhere, called in the command's transaction, locks an obligation for planning and returns the
// sum of the live parts other shipments plan of it (F1).
type ShipmentStore interface {
	Load(context.Context, ful.TenantID, ful.ShipmentID) (*ful.Shipment, int64, error)
	Read(context.Context, ful.TenantID, ful.ShipmentID) (*ful.Shipment, int64, error)
	Insert(context.Context, *ful.Shipment) (int64, error)
	Save(context.Context, *ful.Shipment, int64) (int64, error)
	PlannedElsewhere(ctx context.Context, tenant ful.TenantID, id ful.ObligationID, version int64, except ful.ShipmentID) (ful.CargoScope, error)
}

type Receipts interface {
	Find(context.Context, ful.TenantID, string) (Receipt, bool, error)
	Save(context.Context, ful.TenantID, Receipt) error
}

type IntegrationOutbox interface {
	EnqueueFULEvent(context.Context, ful.Event) error
}

type Authorization interface {
	RequireShipmentAction(context.Context, Scope, ShipmentAction, ful.ShipmentID) error
}

// Obligations asks COM whether FUL may plan an obligation version for a shipment, and returns what COM
// committed: the version must be ACTIVE, the obligation's latest, and reserved for the shipment. It is
// called outside FUL's transactions.
type Obligations interface {
	Plannable(ctx context.Context, tenant ful.TenantID, id ful.ObligationID, version int64, shipment ful.ShipmentID) (ful.ObligationRef, error)
}

// Clock tells the time FUL records a command (T1).
type Clock interface{ Now() time.Time }

// Deps are the ports every FUL command uses. Window bounds the business time a command states against the
// time Clock records it (T1).
type Deps struct {
	Tx        TxRunner
	Shipments ShipmentStore
	Receipts  Receipts
	Outbox    IntegrationOutbox
	Auth      Authorization
	Clock     Clock
	Window    kernel.RecordWindow
}

func (d Deps) check(ctx context.Context, s Scope, commandID string, occurredAt time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.Tx == nil || d.Shipments == nil || d.Receipts == nil || d.Outbox == nil || d.Auth == nil || d.Clock == nil {
		return ErrMissingDependency
	}
	if s.TenantID == "" || strings.TrimSpace(s.ActorID) == "" || commandID == "" || occurredAt.IsZero() {
		return ErrUnauthenticated
	}
	if scoped, ok := d.Tx.(interface{ TenantScope() string }); ok && scoped.TenantScope() != string(s.TenantID) {
		return ErrUnauthorized
	}
	return nil
}

// begin checks a command and returns its stored receipt if the command was already committed. Otherwise it
// returns the time FUL records the command, if the command's business time lies within the record window
// before it (T1); the window is checked after the replay, so a command retried later still gets its receipt.
func (d Deps) begin(ctx context.Context, s Scope, commandID string, occurredAt time.Time, command any) (hash string, entered time.Time, stored Receipt, replayed bool, err error) {
	if err = d.check(ctx, s, commandID, occurredAt); err != nil {
		return "", time.Time{}, Receipt{}, false, err
	}
	if hash, err = requestHash(command); err != nil {
		return "", time.Time{}, Receipt{}, false, err
	}
	if stored, replayed, err = d.replay(ctx, s.TenantID, commandID, hash); err != nil || replayed {
		return hash, time.Time{}, stored, replayed, err
	}
	entered = d.Clock.Now().UTC().Truncate(time.Microsecond)
	if err = d.Window.Check(occurredAt, entered); err != nil {
		return "", time.Time{}, Receipt{}, false, fmt.Errorf("%w: business time %s, recorded %s", err, occurredAt.Format(time.RFC3339), entered.Format(time.RFC3339))
	}
	return hash, entered, Receipt{}, false, nil
}

func (d Deps) authorize(ctx context.Context, s Scope, action ShipmentAction, id ful.ShipmentID) error {
	if err := d.Auth.RequireShipmentAction(ctx, s, action, id); err != nil {
		return fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	return nil
}

// change commits one change to a stored shipment, made at the revision the caller expects, with its
// Outbox events and the command receipt.
func (d Deps) change(ctx context.Context, s Scope, commandID, hash string, entered time.Time, id ful.ShipmentID, expected int64, apply func(*ful.Shipment) error) (Receipt, error) {
	return d.changeIn(ctx, s, commandID, hash, entered, id, expected, func(_ context.Context, shipment *ful.Shipment) error { return apply(shipment) })
}

// changeIn is change for an apply that reads in the command's transaction.
func (d Deps) changeIn(ctx context.Context, s Scope, commandID, hash string, entered time.Time, id ful.ShipmentID, expected int64, apply func(context.Context, *ful.Shipment) error) (Receipt, error) {
	return d.write(ctx, s, commandID, hash, func(txCtx context.Context) (*ful.Shipment, int64, error) {
		shipment, revision, err := d.Shipments.Load(txCtx, s.TenantID, id)
		if err != nil {
			return nil, 0, err
		}
		if revision != expected {
			return nil, 0, ful.ErrConflict
		}
		if err = apply(txCtx, shipment); err != nil {
			return nil, 0, err
		}
		shipment.Stamp(entered)
		newRevision, err := d.Shipments.Save(txCtx, shipment, revision)
		return shipment, newRevision, err
	})
}

// replay returns the stored receipt of a command whose payload is unchanged.
func (d Deps) replay(ctx context.Context, tenant ful.TenantID, commandID, hash string) (Receipt, bool, error) {
	var stored Receipt
	var found bool
	err := d.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		r, ok, err := d.Receipts.Find(txCtx, tenant, commandID)
		if err != nil || !ok {
			return err
		}
		if r.RequestHash != hash {
			return ErrIdempotencyConflict
		}
		stored, found = r, true
		return nil
	})
	return stored, found, err
}

// write commits mutate's shipment, its Outbox events and the command receipt in one transaction. A
// receipt stored meanwhile by a concurrent replay wins.
func (d Deps) write(ctx context.Context, s Scope, commandID, hash string, mutate func(context.Context) (*ful.Shipment, int64, error)) (Receipt, error) {
	var result Receipt
	var changed *ful.Shipment
	err := d.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		r, ok, err := d.Receipts.Find(txCtx, s.TenantID, commandID)
		if err != nil {
			return err
		}
		if ok {
			if r.RequestHash != hash {
				return ErrIdempotencyConflict
			}
			result = r
			return nil
		}
		shipment, revision, err := mutate(txCtx)
		if err != nil {
			return err
		}
		for _, e := range shipment.Pending() {
			if err = d.Outbox.EnqueueFULEvent(txCtx, e); err != nil {
				return err
			}
		}
		result = Receipt{CommandID: commandID, ShipmentID: shipment.ID(), NewRevision: revision, RequestHash: hash}
		if err = d.Receipts.Save(txCtx, s.TenantID, result); err != nil {
			return err
		}
		changed = shipment
		return nil
	})
	if err != nil {
		return Receipt{}, err
	}
	if changed != nil {
		changed.Acknowledge()
	}
	return result, nil
}

func requestHash(command any) (string, error) {
	blob, err := json.Marshal(command)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:]), nil
}

// partToPlan returns the part a command asks for, or, when it asks for none, all that other shipments leave
// of the obligation. Nothing left is ErrPlannedElsewhere.
func partToPlan(ob ful.ObligationRef, requested, elsewhere ful.CargoScope) (ful.CargoScope, error) {
	if requested != (ful.CargoScope{}) {
		return requested, nil
	}
	rest, err := ob.Remaining(elsewhere)
	if err != nil {
		return ful.CargoScope{}, err
	}
	if rest.Packs <= 0 {
		return ful.CargoScope{}, fmt.Errorf("%w: %s", ErrPlannedElsewhere, ob.ObligationID)
	}
	return rest, nil
}
