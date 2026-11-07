// Package application runs COM's use cases. Business rules stay in domain; this package orders the
// steps: replay check, authorization, questions to RATE and ORG outside any COM transaction (rule T3),
// and one transaction for the order, its Outbox events and the command receipt (rule T2).
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

	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
)

var (
	ErrUnauthenticated      = errors.New("com application: unauthenticated")
	ErrUnauthorized         = errors.New("com application: unauthorized")
	ErrIdempotencyConflict  = errors.New("com application: command id reused with a different payload")
	ErrMissingDependency    = errors.New("com application: required dependency missing")
	ErrQuoteNotAccepted     = errors.New("com application: RATE has no accepted quote by that id")
	ErrCustomerNotConfirmed = errors.New("com application: ORG did not confirm the customer")
	ErrQuoteAlreadyOrdered  = errors.New("com application: the quote already backs a customer order")
)

// Scope is the verified caller of a command.
type Scope struct {
	TenantID com.TenantID
	ActorID  string
}

type OrderAction string

const (
	ActionRecordCustomerOrder             OrderAction = "RecordCustomerOrder"
	ActionAllocateOrderToObligation       OrderAction = "AllocateOrderToObligation"
	ActionRevokeObligation                OrderAction = "RevokeObligation"
	ActionAmendObligation                 OrderAction = "AmendObligation"
	ActionCancelCustomerOrder             OrderAction = "CancelCustomerOrder"
	ActionReserveObligationForFulfillment OrderAction = "ReserveObligationForFulfillment"
	ActionReleaseObligationReservation    OrderAction = "ReleaseObligationReservation"
)

type Receipt struct {
	CommandID   string
	OrderID     com.OrderID
	NewRevision int64
	RequestHash string
}

type TxRunner interface {
	WithinTransaction(context.Context, func(context.Context) error) error
}

// OrderStore loads and writes orders for commands. Save writes the order's status and what the order
// added since it was loaded: obligation versions, reservations and releases. OrderOf finds the order that
// holds an obligation.
type OrderStore interface {
	Load(context.Context, com.TenantID, com.OrderID) (*com.CustomerOrder, int64, error)
	Insert(context.Context, *com.CustomerOrder) (int64, error)
	Save(context.Context, *com.CustomerOrder, int64) (int64, error)
	OrderOf(context.Context, com.TenantID, com.ObligationID) (com.OrderID, error)
}

type Receipts interface {
	Find(context.Context, com.TenantID, string) (Receipt, bool, error)
	Save(context.Context, com.TenantID, Receipt) error
}

type IntegrationOutbox interface {
	EnqueueCOMEvent(context.Context, com.Event) error
}

type Authorization interface {
	RequireOrderAction(context.Context, Scope, OrderAction, com.OrderID) error
}

// Quotes reads accepted quotes from RATE. It is called outside COM's transactions.
type Quotes interface {
	AcceptedQuote(ctx context.Context, tenant com.TenantID, quoteID string) (com.AcceptedQuote, error)
}

// Customers confirms with ORG that a party is a customer of a legal entity at an instant and returns the
// snapshot to freeze. It is called outside COM's transactions.
type Customers interface {
	VerifyCustomer(ctx context.Context, tenant com.TenantID, partyID string, entity com.LegalEntityID, at time.Time) (com.CustomerSnapshot, error)
}

// Deps are the ports every COM command uses.
type Deps struct {
	Tx       TxRunner
	Orders   OrderStore
	Receipts Receipts
	Outbox   IntegrationOutbox
	Auth     Authorization
}

func (d Deps) check(ctx context.Context, s Scope, commandID string, occurredAt time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.Tx == nil || d.Orders == nil || d.Receipts == nil || d.Outbox == nil || d.Auth == nil {
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

// begin checks a command and returns its stored receipt if the command was already committed.
func (d Deps) begin(ctx context.Context, s Scope, commandID string, occurredAt time.Time, command any) (hash string, stored Receipt, replayed bool, err error) {
	if err = d.check(ctx, s, commandID, occurredAt); err != nil {
		return "", Receipt{}, false, err
	}
	if hash, err = requestHash(command); err != nil {
		return "", Receipt{}, false, err
	}
	stored, replayed, err = d.replay(ctx, s.TenantID, commandID, hash)
	return hash, stored, replayed, err
}

func (d Deps) authorize(ctx context.Context, s Scope, action OrderAction, id com.OrderID) error {
	if err := d.Auth.RequireOrderAction(ctx, s, action, id); err != nil {
		return fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	return nil
}

// orderOf finds the order that holds an obligation, for the commands FUL addresses to an obligation.
func (d Deps) orderOf(ctx context.Context, tenant com.TenantID, id com.ObligationID) (com.OrderID, error) {
	var order com.OrderID
	err := d.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var err error
		order, err = d.Orders.OrderOf(txCtx, tenant, id)
		return err
	})
	return order, err
}

// change commits one change to a stored order with its Outbox events and the command receipt. apply gets
// the order, locked, and the revision it was stored at.
func (d Deps) change(ctx context.Context, s Scope, commandID, hash string, id com.OrderID, apply func(*com.CustomerOrder, int64) error) (Receipt, error) {
	return d.write(ctx, s, commandID, hash, func(txCtx context.Context) (*com.CustomerOrder, int64, error) {
		order, revision, err := d.Orders.Load(txCtx, s.TenantID, id)
		if err != nil {
			return nil, 0, err
		}
		if err = apply(order, revision); err != nil {
			return nil, 0, err
		}
		newRevision, err := d.Orders.Save(txCtx, order, revision)
		return order, newRevision, err
	})
}

// replay returns the stored receipt of a command whose payload is unchanged.
func (d Deps) replay(ctx context.Context, tenant com.TenantID, commandID, hash string) (Receipt, bool, error) {
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

// write commits mutate's order, its Outbox events and the command receipt in one transaction. A receipt
// stored meanwhile by a concurrent replay wins.
func (d Deps) write(ctx context.Context, s Scope, commandID, hash string, mutate func(context.Context) (*com.CustomerOrder, int64, error)) (Receipt, error) {
	var result Receipt
	var changed *com.CustomerOrder
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
		order, revision, err := mutate(txCtx)
		if err != nil {
			return err
		}
		for _, e := range order.Pending() {
			if err = d.Outbox.EnqueueCOMEvent(txCtx, e); err != nil {
				return err
			}
		}
		result = Receipt{CommandID: commandID, OrderID: order.ID(), NewRevision: revision, RequestHash: hash}
		if err = d.Receipts.Save(txCtx, s.TenantID, result); err != nil {
			return err
		}
		changed = order
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
