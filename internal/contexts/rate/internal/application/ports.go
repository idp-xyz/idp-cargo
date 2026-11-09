// Package application runs RATE's use cases. Business rules stay in domain; this package orders the
// steps: replay check, authorization, questions to ORG outside any RATE transaction (rule T3), and one
// transaction for the quote, its Outbox events and the command receipt (rule T2).
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

	rate "go.idp.xyz/idp-cargo/internal/contexts/rate/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

var (
	ErrUnauthenticated     = errors.New("rate application: unauthenticated")
	ErrUnauthorized        = errors.New("rate application: unauthorized")
	ErrIdempotencyConflict = errors.New("rate application: command id reused with a different payload")
	ErrMissingDependency   = errors.New("rate application: required dependency missing")
)

// Scope is the verified caller of a command.
type Scope struct {
	TenantID rate.TenantID
	ActorID  string
}

type QuoteAction string

const (
	ActionCreateQuote          QuoteAction = "CreateQuote"
	ActionApproveQuotePrice    QuoteAction = "ApproveQuotePrice"
	ActionApproveOwnQuotePrice QuoteAction = "ApproveOwnQuotePrice" // R3: held only by roles designated to approve their own quotes
	ActionAcceptQuote          QuoteAction = "AcceptQuote"
	ActionExpireQuote          QuoteAction = "ExpireQuote"
)

type Receipt struct {
	CommandID   string
	QuoteID     rate.QuoteID
	NewRevision int64
	RequestHash string
}

type TxRunner interface {
	WithinTransaction(context.Context, func(context.Context) error) error
}

type QuoteStore interface {
	Load(context.Context, rate.TenantID, rate.QuoteID) (*rate.Quote, int64, error)
	Insert(context.Context, *rate.Quote) (int64, error)
	Save(context.Context, *rate.Quote, int64) (int64, error)
}

type Receipts interface {
	Find(context.Context, rate.TenantID, string) (Receipt, bool, error)
	Save(context.Context, rate.TenantID, Receipt) error
}

type IntegrationOutbox interface {
	EnqueueRATEEvent(context.Context, rate.Event) error
}

type Authorization interface {
	RequireQuoteAction(context.Context, Scope, QuoteAction, rate.QuoteID) error
}

// Customers confirms with ORG that a party holds a recipient role (R7) for a legal entity at an instant. It
// is called outside RATE's transactions.
type Customers interface {
	VerifyRecipient(ctx context.Context, tenant rate.TenantID, partyID string, entity rate.LegalEntityID, role rate.RecipientRole, at time.Time) (rate.CustomerRef, error)
}

// Clock tells the time RATE records a command (T1).
type Clock interface{ Now() time.Time }

// MarginPolicy supplies the margin floor, in basis points, for quotes of a legal entity.
type MarginPolicy interface {
	MarginFloorBP(context.Context, rate.TenantID, rate.LegalEntityID) (int, error)
}

// Deps are the ports every RATE command uses. Window bounds the business time a command states against
// the time Clock records it (T1).
type Deps struct {
	Tx       TxRunner
	Quotes   QuoteStore
	Receipts Receipts
	Outbox   IntegrationOutbox
	Auth     Authorization
	Clock    Clock
	Window   kernel.RecordWindow
}

func (d Deps) check(ctx context.Context, s Scope, commandID string, occurredAt time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.Tx == nil || d.Quotes == nil || d.Receipts == nil || d.Outbox == nil || d.Auth == nil || d.Clock == nil {
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

// admit returns the time RATE records a command, if the command's business time lies within the window
// before it (T1). It runs after the replay check, so a command retried later still returns its receipt.
func (d Deps) admit(occurredAt time.Time) (time.Time, error) {
	recorded := d.Clock.Now().UTC().Truncate(time.Microsecond)
	if err := d.Window.Check(occurredAt, recorded); err != nil {
		return time.Time{}, fmt.Errorf("%w: business time %s, recorded %s", err, occurredAt.Format(time.RFC3339), recorded.Format(time.RFC3339))
	}
	return recorded, nil
}

// replay returns the stored receipt of a command whose payload is unchanged.
func (d Deps) replay(ctx context.Context, tenant rate.TenantID, commandID, hash string) (Receipt, bool, error) {
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

// write commits mutate's quote, its Outbox events and the command receipt in one transaction. A receipt
// stored meanwhile by a concurrent replay wins.
func (d Deps) write(ctx context.Context, s Scope, commandID, hash string, mutate func(context.Context) (*rate.Quote, int64, error)) (Receipt, error) {
	var result Receipt
	var changed *rate.Quote
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
		quote, revision, err := mutate(txCtx)
		if err != nil {
			return err
		}
		for _, e := range quote.Pending() {
			if err = d.Outbox.EnqueueRATEEvent(txCtx, e); err != nil {
				return err
			}
		}
		result = Receipt{CommandID: commandID, QuoteID: quote.ID(), NewRevision: revision, RequestHash: hash}
		if err = d.Receipts.Save(txCtx, s.TenantID, result); err != nil {
			return err
		}
		changed = quote
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
