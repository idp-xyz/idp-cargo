// Package application runs PHY's use cases for consolidations and their houses. Business rules stay in
// domain; this package orders the steps: replay check, authorization, questions to FUL and the location
// reference data outside any PHY transaction (rule T3), and one transaction for the changed aggregate, its
// Outbox events and the command receipt (rule T2).
//
// Locks: a command that changes a consolidation locks its row FOR UPDATE; a house command locks the
// consolidations it touches FOR SHARE, in id order, and then the house's membership ledger FOR UPDATE. Attach
// and close therefore run one after another, while houses of one consolidation attach concurrently.
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

	phy "go.idp.xyz/idp-cargo/internal/contexts/phy/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

var (
	ErrUnauthenticated     = errors.New("phy application: unauthenticated")
	ErrUnauthorized        = errors.New("phy application: unauthorized")
	ErrIdempotencyConflict = errors.New("phy application: command id reused with a different payload")
	ErrMissingDependency   = errors.New("phy application: required dependency missing")
	ErrInvalidQuery        = errors.New("phy application: invalid query")
	ErrUnknownShipment     = errors.New("phy application: FUL does not know the shipment")
	ErrLocationRefused     = errors.New("phy application: the location reference data does not resolve the location for the transport mode")
)

// Scope is the verified caller of a command.
type Scope struct {
	TenantID phy.TenantID
	ActorID  string
}

type Action string

const (
	ActionCreateConsolidation               Action = "CreateConsolidation"
	ActionAmendConsolidation                Action = "AmendConsolidation"
	ActionCloseConsolidation                Action = "CloseConsolidation"
	ActionReopenConsolidation               Action = "ReopenConsolidation"
	ActionCancelConsolidation               Action = "CancelConsolidation"
	ActionAttachHouse                       Action = "AttachHouse"
	ActionDetachHouse                       Action = "DetachHouse"
	ActionTransferHouse                     Action = "TransferHouse"
	ActionRefreshHouseIntent                Action = "RefreshHouseIntent"
	ActionEndMembershipForCancelledShipment Action = "EndMembershipForCancelledShipment"
)

// Receipt is a committed command's result. Consolidation commands set ConsolidationID and the
// consolidation's revision; house commands also ShipmentID, the membership revision and the entry.
type Receipt struct {
	CommandID       string
	ConsolidationID phy.ConsolidationID
	ShipmentID      phy.ShipmentID
	NewRevision     int64
	EntryNo         int
	RequestHash     string
}

type TxRunner interface {
	WithinTransaction(context.Context, func(context.Context) error) error
}

// House is a live house of a consolidation: its membership ledger's revision and live entry.
type House struct {
	ShipmentID         phy.ShipmentID
	MembershipRevision int64
	Entry              phy.Entry
}

// Summary is an OPEN consolidation for the attach screen.
type Summary struct {
	State      phy.ConsolidationState // with the current version only
	Revision   int64
	LiveHouses int
}

// ListFilter selects OPEN consolidations; zero fields match everything.
type ListFilter struct {
	Mode           phy.Mode
	Origin         kernel.LocationCode
	Destination    kernel.LocationCode
	Operator       string
	DepartureFrom  time.Time
	DepartureUntil time.Time
	Limit          int
}

// ConsolidationStore loads and writes consolidations. Load locks the row FOR UPDATE, for commands that change
// the consolidation; LoadShared locks it FOR SHARE, for house commands; Read takes no lock.
type ConsolidationStore interface {
	Insert(context.Context, *phy.Consolidation) (int64, error)
	Load(context.Context, phy.TenantID, phy.ConsolidationID) (*phy.Consolidation, int64, error)
	LoadShared(context.Context, phy.TenantID, phy.ConsolidationID) (*phy.Consolidation, int64, error)
	Read(context.Context, phy.TenantID, phy.ConsolidationID) (*phy.Consolidation, int64, error)
	Save(context.Context, *phy.Consolidation, int64) (int64, error)
	LiveHouses(context.Context, phy.TenantID, phy.ConsolidationID) ([]House, error)
	ListOpen(context.Context, phy.TenantID, ListFilter) ([]Summary, error)
}

// MembershipStore loads and writes membership ledgers. Load locks the ledger FOR UPDATE and returns a new,
// empty ledger at revision 0 for a shipment never attached; Read returns ErrNotFound for it. Save with
// revision 0 inserts the ledger.
type MembershipStore interface {
	Load(context.Context, phy.TenantID, phy.ShipmentID) (*phy.Membership, int64, error)
	Read(context.Context, phy.TenantID, phy.ShipmentID) (*phy.Membership, int64, error)
	Save(context.Context, *phy.Membership, int64) (int64, error)
}

type Receipts interface {
	Find(context.Context, phy.TenantID, string) (Receipt, bool, error)
	Save(context.Context, phy.TenantID, Receipt) error
}

type IntegrationOutbox interface {
	EnqueuePHYEvent(context.Context, phy.Event) error
}

type Authorization interface {
	RequireConsolidationAction(context.Context, Scope, Action, phy.ConsolidationID) error
}

// LoadIntents reads FUL's LoadIntent v1 of a shipment (relationship 7). It is called outside PHY's
// transactions; an unknown shipment is ErrUnknownShipment.
type LoadIntents interface {
	LoadIntent(ctx context.Context, tenant phy.TenantID, shipment phy.ShipmentID) (phy.LoadIntent, error)
}

// ResolvedLocation is a location resolved against the location reference data (F2): its UN/LOCODE, for AIR
// its IATA airport code, and the edition used.
type ResolvedLocation struct {
	Code    kernel.LocationCode
	IATA    string
	Edition string
}

// Locations resolves a UN/LOCODE, an alias carriers use, or for AIR an IATA airport code, for a mode. It is
// called outside PHY's transactions.
type Locations interface {
	Resolve(ctx context.Context, mode phy.Mode, l kernel.LocationCode) (ResolvedLocation, error)
}

// Clock tells the time PHY records a command (T1).
type Clock interface{ Now() time.Time }

// Deps are the ports every PHY command uses. Window bounds the business time a command states against the
// time Clock records it (T1).
type Deps struct {
	Tx             TxRunner
	Consolidations ConsolidationStore
	Memberships    MembershipStore
	Receipts       Receipts
	Outbox         IntegrationOutbox
	Auth           Authorization
	Clock          Clock
	Window         kernel.RecordWindow
}

func (d Deps) check(ctx context.Context, s Scope, commandID string, occurredAt time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.Tx == nil || d.Consolidations == nil || d.Memberships == nil || d.Receipts == nil || d.Outbox == nil || d.Auth == nil || d.Clock == nil {
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
// returns the time PHY records the command, if the command's business time lies within the record window
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

func (d Deps) authorize(ctx context.Context, s Scope, action Action, ids ...phy.ConsolidationID) error {
	for _, id := range ids {
		if err := d.Auth.RequireConsolidationAction(ctx, s, action, id); err != nil {
			return fmt.Errorf("%w: %v", ErrUnauthorized, err)
		}
	}
	return nil
}

// replay returns the stored receipt of a command whose payload is unchanged.
func (d Deps) replay(ctx context.Context, tenant phy.TenantID, commandID, hash string) (Receipt, bool, error) {
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

// aggregate is a consolidation or a membership ledger whose events a command commits.
type aggregate interface {
	Pending() []phy.Event
	Acknowledge()
}

// write commits mutate's aggregate, its Outbox events and the command receipt in one transaction. A receipt
// stored meanwhile by a concurrent replay wins.
func (d Deps) write(ctx context.Context, s Scope, commandID, hash string, mutate func(context.Context) (Receipt, aggregate, error)) (Receipt, error) {
	var result Receipt
	var changed aggregate
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
		receipt, agg, err := mutate(txCtx)
		if err != nil {
			return err
		}
		for _, e := range agg.Pending() {
			if err = d.Outbox.EnqueuePHYEvent(txCtx, e); err != nil {
				return err
			}
		}
		receipt.CommandID, receipt.RequestHash = commandID, hash
		if err = d.Receipts.Save(txCtx, s.TenantID, receipt); err != nil {
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

// changeConsolidation commits one change to a stored consolidation, locked FOR UPDATE, at the revision the
// caller expects; apply may read the consolidation's live houses in the transaction.
func (d Deps) changeConsolidation(ctx context.Context, s Scope, commandID, hash string, entered time.Time, id phy.ConsolidationID, expected int64,
	apply func(context.Context, *phy.Consolidation) error) (Receipt, error) {
	return d.write(ctx, s, commandID, hash, func(txCtx context.Context) (Receipt, aggregate, error) {
		c, revision, err := d.Consolidations.Load(txCtx, s.TenantID, id)
		if err != nil {
			return Receipt{}, nil, err
		}
		if revision != expected {
			return Receipt{}, nil, phy.ErrConflict
		}
		if err = apply(txCtx, c); err != nil {
			return Receipt{}, nil, err
		}
		c.Stamp(entered)
		revision, err = d.Consolidations.Save(txCtx, c, revision)
		return Receipt{ConsolidationID: id, NewRevision: revision}, c, err
	})
}

// changeHouse commits one change to a shipment's membership ledger at the revision the caller expects, after
// locking the consolidations it touches FOR SHARE in id order. apply gets them in the order given.
func (d Deps) changeHouse(ctx context.Context, s Scope, commandID, hash string, entered time.Time, shipment phy.ShipmentID, expected int64,
	ids []phy.ConsolidationID, apply func(*phy.Membership, []*phy.Consolidation) error) (Receipt, error) {
	return d.write(ctx, s, commandID, hash, func(txCtx context.Context) (Receipt, aggregate, error) {
		consols := make([]*phy.Consolidation, len(ids))
		order := []int{0}
		if len(ids) == 2 {
			order = []int{0, 1}
			if ids[1] < ids[0] {
				order = []int{1, 0}
			}
		}
		for _, i := range order {
			c, _, err := d.Consolidations.LoadShared(txCtx, s.TenantID, ids[i])
			if err != nil {
				return Receipt{}, nil, err
			}
			consols[i] = c
		}
		m, revision, err := d.Memberships.Load(txCtx, s.TenantID, shipment)
		if err != nil {
			return Receipt{}, nil, err
		}
		if revision != expected {
			return Receipt{}, nil, phy.ErrConflict
		}
		if err = apply(m, consols); err != nil {
			return Receipt{}, nil, err
		}
		m.Stamp(entered)
		if revision, err = d.Memberships.Save(txCtx, m, revision); err != nil {
			return Receipt{}, nil, err
		}
		// The receipt names the live entry, or the one a detach ended.
		receipt := Receipt{ShipmentID: shipment, NewRevision: revision}
		if live, ok := m.Live(); ok {
			receipt.ConsolidationID, receipt.EntryNo = live.ConsolidationID, live.No
		}
		for no, e := range m.EndedEntries() {
			if receipt.EntryNo == 0 {
				receipt.ConsolidationID, receipt.EntryNo = e.ConsolidationID, no
			}
		}
		return receipt, m, nil
	})
}

func requestHash(command any) (string, error) {
	blob, err := json.Marshal(command)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:]), nil
}

// resolveRoute resolves a route's locations for a mode, so that a consolidation version freezes the
// UN/LOCODEs, for AIR the IATA airport codes, and the edition used (F2).
func resolveRoute(ctx context.Context, locations Locations, mode phy.Mode, origin, destination kernel.LocationCode) (phy.Route, error) {
	if origin.IsZero() && destination.IsZero() {
		return phy.Route{}, nil
	}
	if locations == nil {
		return phy.Route{}, fmt.Errorf("%w: PHY has no location reference data", ErrMissingDependency)
	}
	if !mode.Valid() || origin.IsZero() || destination.IsZero() {
		return phy.Route{}, phy.ErrInvalidRoute
	}
	from, err := locations.Resolve(ctx, mode, origin)
	if err != nil {
		return phy.Route{}, err
	}
	to, err := locations.Resolve(ctx, mode, destination)
	if err != nil {
		return phy.Route{}, err
	}
	if from.Edition != to.Edition {
		return phy.Route{}, fmt.Errorf("%w: the reference data edition changed while the route was resolved; try again", phy.ErrConflict)
	}
	r := phy.Route{Origin: from.Code, Destination: to.Code, LocationEdition: from.Edition}
	if mode == phy.ModeAir {
		r.OriginIATA, r.DestinationIATA = from.IATA, to.IATA
	}
	return r, nil
}
