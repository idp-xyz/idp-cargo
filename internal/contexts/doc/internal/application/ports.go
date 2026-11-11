// Package application runs DOC's use cases. Business rules stay in domain; this package orders the steps: replay
// check, authorization, questions to AIR, COM, ORG and FIN outside any DOC transaction (rule T3), and one
// transaction for the document or authority, its Outbox events and the command receipt (rule T2).
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

	doc "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

var (
	ErrUnauthenticated     = errors.New("doc application: unauthenticated")
	ErrUnauthorized        = errors.New("doc application: unauthorized")
	ErrIdempotencyConflict = errors.New("doc application: command id reused with a different payload")
	ErrMissingDependency   = errors.New("doc application: required dependency missing")
	ErrInvalidQuery        = errors.New("doc application: invalid query")
	ErrInvalidCommand      = errors.New("doc application: invalid command")
	ErrUnknownSource       = errors.New("doc application: AIR, COM or ORG does not know what the document refers to")
)

// Scope is the verified caller of a command.
type Scope struct {
	TenantID doc.TenantID
	ActorID  string
}

// Action is an action an Authorizer decides on.
type Action string

const (
	ActionApproveOwn     Action = "ApproveOwnTransportDocument"
	ActionOverrideCredit Action = "OverrideCreditCheck"
)

// Receipt is a committed command's result.
type Receipt struct {
	CommandID   string
	SubjectID   string
	NewRevision int64
	Version     int
	RequestHash string
}

type TxRunner interface {
	WithinTransaction(context.Context, func(context.Context) error) error
}

// DocumentStore loads and writes documents; Load locks the document row.
type DocumentStore interface {
	Load(context.Context, doc.TenantID, doc.DocumentID) (*doc.Document, int64, error)
	Read(context.Context, doc.TenantID, doc.DocumentID) (*doc.Document, int64, error)
	Insert(context.Context, *doc.Document) (int64, error)
	Save(context.Context, *doc.Document, int64) (int64, error)
	List(ctx context.Context, tenant doc.TenantID, shipment, consolidation string) ([]Summary, error)
}

// Summary is a document in a list.
type Summary struct {
	DocumentID     doc.DocumentID
	Kind           doc.Kind
	State          doc.State
	Number         string
	Revision       int64
	CurrentVersion int
	LatestVersion  int
}

// AuthorityStore loads and writes issuance authorities and finds the one in force.
type AuthorityStore interface {
	Load(context.Context, doc.TenantID, string) (*doc.IssuanceAuthority, int64, error)
	Read(context.Context, doc.TenantID, string) (*doc.IssuanceAuthority, int64, error)
	Insert(context.Context, *doc.IssuanceAuthority) (int64, error)
	Save(context.Context, *doc.IssuanceAuthority, int64) (int64, error)
	// InForce returns an authority covering the legal entity, kind, place and airline at an instant.
	InForce(ctx context.Context, tenant doc.TenantID, legalEntity string, kind doc.Kind, place, airline string, at time.Time) (*doc.IssuanceAuthority, bool, error)
}

type Receipts interface {
	Find(context.Context, doc.TenantID, string) (Receipt, bool, error)
	Save(context.Context, doc.TenantID, Receipt, string) error
}

type IntegrationOutbox interface {
	EnqueueDOCEvent(context.Context, doc.Event) error
}

type Authorization interface {
	RequireDocAction(ctx context.Context, s Scope, action Action, subject string) error
}

// Facts reads AIR's latest AirDocFacts v1 version of a movement (relationship 11), outside DOC's transactions.
type Facts interface {
	Latest(ctx context.Context, tenant doc.TenantID, movement string) (doc.Facts, error)
}

// Parties reads COM's OrderParties v1 of an order (COM -> DOC), outside DOC's transactions, without contacts.
type Parties interface {
	Order(ctx context.Context, tenant doc.TenantID, actor, order string) (doc.OrderParties, error)
}

// Directory reads ORG (ORG -> DOC): the issuing legal entity (LegalEntityRef v1) and a destination agent in the
// OVERSEAS_AGENT role for the legal entity with its office address (PartyRef v1).
type Directory interface {
	Issuer(ctx context.Context, tenant doc.TenantID, legalEntity string) (doc.Issuer, error)
	Agent(ctx context.Context, tenant doc.TenantID, party, legalEntity string, at time.Time) (doc.Party, error)
}

// Credit reads FIN's credit decision for the ordering parties of orders (CreditStatus v1, FIN -> DOC).
type Credit interface {
	Decision(ctx context.Context, tenant doc.TenantID, orders []string) (doc.CreditDecision, error)
}

// Clock tells the time DOC records a command (T1).
type Clock interface{ Now() time.Time }

// Deps are the ports every DOC command uses.
type Deps struct {
	Tx          TxRunner
	Documents   DocumentStore
	Authorities AuthorityStore
	Receipts    Receipts
	Outbox      IntegrationOutbox
	Auth        Authorization
	Clock       Clock
	Window      kernel.RecordWindow
	Facts       Facts
	Parties     Parties
	Directory   Directory
	Credit      Credit
}

// Command is what every DOC command states; Payload is the command as the caller sent it.
type Command struct {
	CommandID        string
	Scope            Scope
	Action           Action
	Subject          string
	ExpectedRevision int64
	OccurredAt       time.Time
	Payload          any
}

func (d Deps) begin(ctx context.Context, c Command) (hash string, entered time.Time, stored Receipt, replayed bool, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	if d.Tx == nil || d.Documents == nil || d.Authorities == nil || d.Receipts == nil || d.Outbox == nil || d.Auth == nil || d.Clock == nil {
		return "", time.Time{}, Receipt{}, false, ErrMissingDependency
	}
	if c.Scope.TenantID == "" || strings.TrimSpace(c.Scope.ActorID) == "" || c.CommandID == "" || c.OccurredAt.IsZero() {
		return "", time.Time{}, Receipt{}, false, ErrUnauthenticated
	}
	if scoped, ok := d.Tx.(interface{ TenantScope() string }); ok && scoped.TenantScope() != string(c.Scope.TenantID) {
		return "", time.Time{}, Receipt{}, false, ErrUnauthorized
	}
	blob, err := json.Marshal(c.Payload)
	if err != nil {
		return "", time.Time{}, Receipt{}, false, err
	}
	sum := sha256.Sum256(blob)
	hash = hex.EncodeToString(sum[:])
	if err = d.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		r, ok, err := d.Receipts.Find(txCtx, c.Scope.TenantID, c.CommandID)
		if err != nil || !ok {
			return err
		}
		if r.RequestHash != hash {
			return ErrIdempotencyConflict
		}
		stored, replayed = r, true
		return nil
	}); err != nil || replayed {
		return hash, time.Time{}, stored, replayed, err
	}
	if err = d.authorize(ctx, c.Scope, c.Action, c.Subject); err != nil {
		return "", time.Time{}, Receipt{}, false, err
	}
	entered = d.Clock.Now().UTC().Truncate(time.Microsecond)
	if err = d.Window.Check(c.OccurredAt, entered); err != nil {
		return "", time.Time{}, Receipt{}, false, fmt.Errorf("%w: business time %s, recorded %s", err, c.OccurredAt.Format(time.RFC3339), entered.Format(time.RFC3339))
	}
	return hash, entered, Receipt{}, false, nil
}

func (d Deps) authorize(ctx context.Context, s Scope, action Action, subject string) error {
	if err := d.Auth.RequireDocAction(ctx, s, action, subject); err != nil {
		return fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	return nil
}

type aggregate interface {
	Pending() []doc.Event
	Acknowledge()
}

// write commits mutate's aggregate, its Outbox events and the receipt in one transaction.
func (d Deps) write(ctx context.Context, c Command, hash, kind string, mutate func(context.Context) (Receipt, aggregate, error)) (Receipt, error) {
	var result Receipt
	var changed aggregate
	err := d.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		r, ok, err := d.Receipts.Find(txCtx, c.Scope.TenantID, c.CommandID)
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
		receipt, agg, err := mutate(txCtx)
		if err != nil {
			return err
		}
		for _, e := range agg.Pending() {
			if err = d.Outbox.EnqueueDOCEvent(txCtx, e); err != nil {
				return err
			}
		}
		receipt.CommandID, receipt.RequestHash = c.CommandID, hash
		if err = d.Receipts.Save(txCtx, c.Scope.TenantID, receipt, kind); err != nil {
			return err
		}
		result, changed = receipt, agg
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
