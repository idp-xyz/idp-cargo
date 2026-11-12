// Package application runs FIN's use cases. Business rules stay in domain; this package orders the steps: replay
// check, authorization, questions to FUL, RATE, DOC and ORG outside any FIN transaction (rule T3), and one transaction
// for the aggregate, its Outbox events and the command receipt (rule T2).
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

	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

var (
	ErrUnauthenticated     = errors.New("fin application: unauthenticated")
	ErrUnauthorized        = errors.New("fin application: unauthorized")
	ErrIdempotencyConflict = errors.New("fin application: command id reused with a different payload")
	ErrMissingDependency   = errors.New("fin application: required dependency missing")
	ErrInvalidQuery        = errors.New("fin application: invalid query")
	ErrUnknownSource       = errors.New("fin application: FUL, RATE, DOC or ORG does not know what the command refers to")
	ErrNoPolicy            = errors.New("fin application: FIN_NO_POLICY: the legal entity has no finance policy; open one first")
	ErrNotVerified         = errors.New("fin application: the legal entity is not VERIFIED in ORG")
)

// Scope is the verified caller of a command.
type Scope struct {
	TenantID fin.TenantID
	ActorID  string
}

// Action is an action an Authorizer decides on.
type Action string

// Receipt is a committed command's result.
type Receipt struct {
	CommandID   string
	SubjectID   string
	NewRevision int64
	Version     int
	Number      string // the number of a note issued
	RequestHash string
}

type TxRunner interface {
	WithinTransaction(context.Context, func(context.Context) error) error
}

// PolicyStore loads and writes finance policies; Load locks the policy row.
type PolicyStore interface {
	Load(ctx context.Context, tenant fin.TenantID, legalEntity string) (*fin.FinancePolicy, int64, error)
	Read(ctx context.Context, tenant fin.TenantID, legalEntity string) (*fin.FinancePolicy, int64, error)
	// Find reads a policy and reports whether the legal entity has one.
	Find(ctx context.Context, tenant fin.TenantID, legalEntity string) (*fin.FinancePolicy, bool, error)
	// Shared is Find holding the policy row FOR SHARE until the transaction ends, so that a rate recorded or ended
	// (which lock it FOR NO KEY UPDATE) and a charge converted at the policy's rates take turns.
	Shared(ctx context.Context, tenant fin.TenantID, legalEntity string) (*fin.FinancePolicy, bool, error)
	Insert(context.Context, *fin.FinancePolicy) (int64, error)
	Save(context.Context, *fin.FinancePolicy, int64) (int64, error)
}

// JobStore loads and writes freight jobs; Load locks the job row.
type JobStore interface {
	Load(ctx context.Context, tenant fin.TenantID, job string) (*fin.FreightJob, int64, error)
	Read(ctx context.Context, tenant fin.TenantID, job string) (*fin.FreightJob, int64, error)
	Insert(context.Context, *fin.FreightJob) (int64, error)
	Save(context.Context, *fin.FreightJob, int64) (int64, error)
	ForShipment(ctx context.Context, tenant fin.TenantID, shipment string) ([]*fin.FreightJob, []int64, error)
	// OpenOf lists the open jobs of a legal entity, of one customer when customer is not empty.
	OpenOf(ctx context.Context, tenant fin.TenantID, legalEntity, customer string) ([]*fin.FreightJob, error)
	// BilledTo lists the jobs of a legal entity with notes to a debtor.
	BilledTo(ctx context.Context, tenant fin.TenantID, legalEntity, debtor string) ([]*fin.FreightJob, error)
	// OfNote reads the job of a note.
	OfNote(ctx context.Context, tenant fin.TenantID, invoiceID string) (*fin.FreightJob, int64, error)
	// WithOpenDebitNotes lists the jobs of a legal entity with an ISSUED debit note (one with a line not reversed).
	WithOpenDebitNotes(ctx context.Context, tenant fin.TenantID, legalEntity string) ([]*fin.FreightJob, error)
}

// Numbers is the platform's sequence of a tenant's document type (idp-platform-go, MD-28), in FIN's schema: Next takes
// the next value of a period in the command's transaction, Seed starts a sequence after a value, Last answers the last
// value per period. Document types are <kind>/<legal entity>.
type Numbers interface {
	Next(ctx context.Context, tenant, docType, period string) (int64, error)
	Seed(ctx context.Context, tenant, docType, period string, last int64) error
	Last(ctx context.Context, tenant, docType string) (map[string]int64, error)
}

// ProfileStore loads and writes billing profiles and reports whether the debtor has one; a version written twice at
// once is a conflict.
type ProfileStore interface {
	Load(ctx context.Context, tenant fin.TenantID, debtor string) (*fin.BillingProfile, bool, error)
	Read(ctx context.Context, tenant fin.TenantID, debtor string) (*fin.BillingProfile, bool, error)
	Save(context.Context, *fin.BillingProfile) error
}

// CreditStore loads and writes credit accounts; Load locks the account row and reports whether it exists.
type CreditStore interface {
	Load(ctx context.Context, tenant fin.TenantID, legalEntity, debtor string) (*fin.CreditAccount, int64, bool, error)
	Read(ctx context.Context, tenant fin.TenantID, legalEntity, debtor string) (*fin.CreditAccount, int64, error)
	Insert(context.Context, *fin.CreditAccount) (int64, error)
	Save(context.Context, *fin.CreditAccount, int64) (int64, error)
	List(ctx context.Context, tenant fin.TenantID, legalEntity string) ([]*fin.CreditAccount, []int64, error)
}

// OnAccount is what a debtor's recorded receipts have unallocated in a currency.
type OnAccount struct {
	DebtorPartyID string
	Amount        kernel.Money
}

// Settlement is a live allocation of a receipt to a debit note, in the note's currency.
type Settlement struct {
	ReceiptID    string
	AllocationNo int
	Amount       int64
	Date         time.Time
}

// DebtorReceiptStore loads and writes debtor receipts; Load locks the receipt row.
type DebtorReceiptStore interface {
	Load(ctx context.Context, tenant fin.TenantID, receipt string) (*fin.DebtorReceipt, int64, error)
	Read(ctx context.Context, tenant fin.TenantID, receipt string) (*fin.DebtorReceipt, int64, error)
	Insert(context.Context, *fin.DebtorReceipt) (int64, error)
	Save(context.Context, *fin.DebtorReceipt, int64) (int64, error)
	// OfDebtor lists a debtor's receipts with a legal entity, in the order received.
	OfDebtor(ctx context.Context, tenant fin.TenantID, legalEntity, debtor string) ([]*fin.DebtorReceipt, []int64, error)
	// OnAccount sums what the recorded receipts of a legal entity have unallocated, per debtor and currency (of one
	// debtor when debtor is not empty).
	OnAccount(ctx context.Context, tenant fin.TenantID, legalEntity, debtor string) ([]OnAccount, error)
	// SettlementNotes locks the jobs of the notes, in order of job id, until the transaction ends, then reads what an
	// allocation needs of each note: job commands and allocations to a job's notes take turns.
	SettlementNotes(ctx context.Context, tenant fin.TenantID, invoiceIDs []string) ([]fin.SettlementNote, error)
	// Settlements lists the live allocations to a debit note.
	Settlements(ctx context.Context, tenant fin.TenantID, invoiceID string) ([]Settlement, error)
}

type Receipts interface {
	Find(ctx context.Context, tenant fin.TenantID, commandID string) (Receipt, bool, error)
	Save(ctx context.Context, tenant fin.TenantID, r Receipt, kind string) error
}

type IntegrationOutbox interface {
	EnqueueFINEvent(context.Context, fin.Event) error
}

type Authorization interface {
	RequireFinAction(ctx context.Context, s Scope, action Action, subject string) error
}

// Clock tells the time FIN records a command (T1).
type Clock interface{ Now() time.Time }

// CostOrder is an order a shipment plans parts of, with the accepted quote FUL kept for it.
type CostOrder struct {
	OrderID       string
	QuoteID       string
	QuoteRevision int64
}

// CostDriver is what FIN reads of FUL's CostDriver v1 (kind SHIPMENT_PLANNED).
type CostDriver struct {
	ShipmentID      string
	Status          string // DRAFT, PLANNED or CANCELLED
	PlanVersion     int
	TransportMode   string
	LegalEntityID   string
	CustomerPartyID string
	Orders          []CostOrder
}

// Shipments reads FUL's CostDriver v1 (FUL -> FIN), outside FIN's transactions.
type Shipments interface {
	CostDriver(ctx context.Context, tenant fin.TenantID, shipment string) (CostDriver, error)
}

// PricingBasis is what FIN reads of RATE's PricingBasis v1 of purpose QUOTE_ACCEPTED.
type PricingBasis struct {
	BasisID          string
	QuoteID          string
	QuoteRevision    int64
	LegalEntityID    string
	CustomerPartyID  string
	CustomerRevision int64
	Lines            []fin.BasisLine
}

// Pricing reads RATE's PricingBasis v1 (RATE -> FIN), outside FIN's transactions.
type Pricing interface {
	QuoteBasis(ctx context.Context, tenant fin.TenantID, quote string) (PricingBasis, error)
}

// Directory reads ORG (ORG -> FIN), outside FIN's transactions: whether a legal entity is VERIFIED, the revision of
// a party ORG confirms in one of the roles for a legal entity at an instant (fin.ErrPartyRole otherwise), and a party's
// identity (names, country and unified social credit code).
type Directory interface {
	LegalEntityVerified(ctx context.Context, tenant fin.TenantID, legalEntity string) (bool, error)
	PartyInRole(ctx context.Context, tenant fin.TenantID, party, legalEntity string, roles []string, at time.Time) (int64, error)
	PartyIdentity(ctx context.Context, tenant fin.TenantID, party string) (fin.PartyIdentity, error)
}

// HouseDocument is what FIN reads of DOC's TransportDocumentSnapshot v1: the valid issued version of a shipment's
// house document.
type HouseDocument struct {
	fin.DocumentRef
	ShipmentID    string
	LegalEntityID string
}

// Documents reads DOC (DOC -> FIN), outside FIN's transactions: the shipment's house document version valid at an
// instant (the valid one now with a zero instant); fin.ErrNoIssuedDocument when there is none.
type Documents interface {
	HouseDocument(ctx context.Context, tenant fin.TenantID, shipment string, at time.Time) (HouseDocument, error)
}

// Deps are the ports every FIN command uses.
type Deps struct {
	Tx        TxRunner
	Policies  PolicyStore
	Jobs      JobStore
	Credit    CreditStore
	Receipts  Receipts
	Outbox    IntegrationOutbox
	Auth      Authorization
	Clock     Clock
	Window    kernel.RecordWindow
	Shipments Shipments
	Pricing   Pricing
	Directory Directory
	Documents Documents
	Numbers   Numbers
	Profiles  ProfileStore
	// DebtorReceipts holds the money received from debtors; Receipts above are the command receipts.
	DebtorReceipts DebtorReceiptStore
}

// Command is what every FIN command carries: Subject is the legal entity, the job, <legal entity>/<debtor> or the
// debtor receipt.
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
	if d.Tx == nil || d.Policies == nil || d.Jobs == nil || d.Credit == nil || d.Numbers == nil || d.Profiles == nil || d.Receipts == nil || d.Outbox == nil ||
		d.Auth == nil || d.Clock == nil || d.DebtorReceipts == nil {
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
	if err = d.Auth.RequireFinAction(ctx, c.Scope, c.Action, c.Subject); err != nil {
		return "", time.Time{}, Receipt{}, false, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	entered = d.Clock.Now().UTC().Truncate(time.Microsecond)
	if err = d.Window.Check(c.OccurredAt, entered); err != nil {
		return "", time.Time{}, Receipt{}, false, fmt.Errorf("%w: business time %s, recorded %s", err, c.OccurredAt.Format(time.RFC3339), entered.Format(time.RFC3339))
	}
	return hash, entered, Receipt{}, false, nil
}

type aggregate interface {
	Pending() []fin.Event
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
			if err = d.Outbox.EnqueueFINEvent(txCtx, e); err != nil {
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

// read runs fn in a read transaction.
func (d Deps) read(ctx context.Context, fn func(context.Context) error) error {
	return d.Tx.WithinTransaction(ctx, fn)
}
