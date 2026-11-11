// Package application runs PHY's use cases for consolidations and their houses. Business rules stay in
// domain; this package orders the steps: replay check, authorization, questions to FUL and the location
// reference data outside any PHY transaction (rule T3), and one transaction for the changed aggregate, its
// Outbox events and the command receipt (rule T2).
//
// Locks, always in this order: consolidations, load executions, membership ledgers, cargo lots. A command that
// changes a consolidation locks its row FOR UPDATE; a house command locks the consolidations it touches FOR
// SHARE, in id order, and then the house's membership ledger FOR UPDATE. Attach and close therefore run one
// after another, while houses of one consolidation attach concurrently. A load execution command locks the
// execution's consolidation FOR SHARE and the execution FOR UPDATE; loading then locks the house's ledger FOR
// SHARE and the cargo lot FOR UPDATE, so loads of one lot run one after another and a house never leaves while
// it is being loaded. A cargo lot command locks the lot FOR UPDATE; a FAILED screening result first locks the
// consolidation and the executions it holds.
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

	bentorepo "go.idp.xyz/idp-bento-go/repository"
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
	ActionReconcileConsolidation            Action = "ReconcileConsolidation"
	ActionReceiveCargo                      Action = "ReceiveCargo"
	ActionRecordCargoMeasurement            Action = "RecordCargoMeasurement"
	ActionRecordScreeningResult             Action = "RecordScreeningResult"
	ActionCompleteReceipt                   Action = "CompleteReceipt"
	ActionResolveCargoDiscrepancy           Action = "ResolveCargoDiscrepancy"
	ActionCloseCargoLot                     Action = "CloseCargoLot"
	ActionOpenLoadExecution                 Action = "OpenLoadExecution"
	ActionStartLoading                      Action = "StartLoading"
	ActionRecordCargoLoaded                 Action = "RecordCargoLoaded"
	ActionRecordCargoUnloaded               Action = "RecordCargoUnloaded"
	ActionPlaceLoadExecutionOnHold          Action = "PlaceLoadExecutionOnHold"
	ActionReleaseLoadExecutionHold          Action = "ReleaseLoadExecutionHold"
	ActionCloseLoadExecution                Action = "CloseLoadExecution"
	ActionRecordCarrierLoadEvidence         Action = "RecordCarrierLoadEvidence"
	ActionResolveLoadingDiscrepancy         Action = "ResolveLoadingDiscrepancy"
)

// Receipt is a committed command's result. Consolidation commands set ConsolidationID and the
// consolidation's revision; house commands also ShipmentID, the membership revision and the entry; cargo lot
// commands LotID and the lot's revision; load execution commands ExecutionID, its consolidation and the
// execution's revision. ItemNo is the line, measurement, screening result, hold, evidence or discrepancy written.
type Receipt struct {
	CommandID       string
	ConsolidationID phy.ConsolidationID
	ShipmentID      phy.ShipmentID
	LotID           phy.CargoLotID
	ExecutionID     phy.LoadExecutionID
	NewRevision     int64
	EntryNo         int
	ItemNo          int
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
// empty ledger at revision 0 for a shipment never attached; LoadShared locks it FOR SHARE, for loading; Read
// returns ErrNotFound for it. Save with revision 0 inserts the ledger.
type MembershipStore interface {
	Load(context.Context, phy.TenantID, phy.ShipmentID) (*phy.Membership, int64, error)
	LoadShared(context.Context, phy.TenantID, phy.ShipmentID) (*phy.Membership, int64, error)
	Read(context.Context, phy.TenantID, phy.ShipmentID) (*phy.Membership, int64, error)
	Save(context.Context, *phy.Membership, int64) (int64, error)
}

// LotStore loads and writes cargo lots. Load locks the lot FOR UPDATE; Read takes no lock. Loaded returns a
// lot's live loaded packs across every load execution. LockShipments locks the shipments' lots FOR SHARE and
// OpenDiscrepancies counts their unresolved discrepancies.
type LotStore interface {
	Insert(context.Context, *phy.CargoLot) (int64, error)
	Load(context.Context, phy.TenantID, phy.CargoLotID) (*phy.CargoLot, int64, error)
	Read(context.Context, phy.TenantID, phy.CargoLotID) (*phy.CargoLot, int64, error)
	Save(context.Context, *phy.CargoLot, int64) (int64, error)
	Loaded(context.Context, phy.TenantID, phy.CargoLotID) (int64, error)
	ByShipment(context.Context, phy.TenantID, phy.ShipmentID) ([]LotRead, error)
	LockShipments(context.Context, phy.TenantID, []phy.ShipmentID) error
	OpenDiscrepancies(context.Context, phy.TenantID, []phy.ShipmentID) (int, error)
}

// LotRead is a stored lot with its revision and live loaded packs.
type LotRead struct {
	Lot      *phy.CargoLot
	Revision int64
	Loaded   int64
}

// DiscrepancyFilter selects open discrepancies: of a consolidation (its executions and the lots of its live
// houses), of a shipment, or of the tenant.
type DiscrepancyFilter struct {
	ConsolidationID phy.ConsolidationID
	ShipmentID      phy.ShipmentID
	Limit           int
}

// OpenDiscrepancy is an open discrepancy of a cargo lot (LotID) or of a load execution (ExecutionID).
type OpenDiscrepancy struct {
	LotID           phy.CargoLotID
	ExecutionID     phy.LoadExecutionID
	ConsolidationID phy.ConsolidationID
	ShipmentID      phy.ShipmentID
	No              int
	Kind            string
	Expected        int64
	Observed        int64
	Unit            string
	RaisedAt        time.Time
}

// DiscrepancyList lists open discrepancies by the time they were raised.
type DiscrepancyList interface {
	ListOpen(context.Context, phy.TenantID, DiscrepancyFilter) ([]OpenDiscrepancy, error)
}

// ExecutionRead is a stored load execution with its revision.
type ExecutionRead struct {
	Execution *phy.LoadExecution
	Revision  int64
}

// ExecutionStore loads and writes load executions. Consolidation returns the consolidation an execution
// tenders, which never changes, so that a command locks it first; Load locks the execution FOR UPDATE; Read
// takes no lock. ByConsolidation reads a consolidation's executions; WithLiveLot the ids of the LOADING or
// ON_HOLD executions with live packs of a lot; HouseLoaded a house's live loaded packs in a consolidation.
type ExecutionStore interface {
	Insert(context.Context, *phy.LoadExecution) (int64, error)
	Consolidation(context.Context, phy.TenantID, phy.LoadExecutionID) (phy.ConsolidationID, error)
	Load(context.Context, phy.TenantID, phy.LoadExecutionID) (*phy.LoadExecution, int64, error)
	Read(context.Context, phy.TenantID, phy.LoadExecutionID) (*phy.LoadExecution, int64, error)
	Save(context.Context, *phy.LoadExecution, int64) (int64, error)
	ByConsolidation(context.Context, phy.TenantID, phy.ConsolidationID) ([]ExecutionRead, error)
	ByShipment(context.Context, phy.TenantID, phy.ShipmentID) ([]ExecutionRead, error)
	WithLiveLot(context.Context, phy.TenantID, phy.CargoLotID) ([]phy.LoadExecutionID, error)
	HouseLoaded(context.Context, phy.TenantID, phy.ConsolidationID, phy.ShipmentID) (int64, error)
}

type Receipts interface {
	Find(context.Context, phy.TenantID, string) (Receipt, bool, error)
	Save(context.Context, phy.TenantID, Receipt) error
}

type IntegrationOutbox interface {
	EnqueuePHYEvent(context.Context, phy.Event) error
}

// Authorization decides on consolidation, house and load execution commands by the consolidation, and on cargo
// lot commands by the lot.
type Authorization interface {
	RequireConsolidationAction(context.Context, Scope, Action, phy.ConsolidationID) error
	RequireLotAction(context.Context, Scope, Action, phy.CargoLotID) error
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
	Lots           LotStore
	Executions     ExecutionStore
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
	if d.Tx == nil || d.Consolidations == nil || d.Memberships == nil || d.Lots == nil || d.Executions == nil || d.Receipts == nil ||
		d.Outbox == nil || d.Auth == nil || d.Clock == nil {
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

func (d Deps) authorizeLot(ctx context.Context, s Scope, action Action, id phy.CargoLotID) error {
	if err := d.Auth.RequireLotAction(ctx, s, action, id); err != nil {
		return fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	return nil
}

func notFound(err error) bool { return errors.Is(err, bentorepo.ErrNotFound) }

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

// aggregate is a consolidation, a membership ledger, a cargo lot or a load execution whose events a command
// commits.
type aggregate interface {
	Pending() []phy.Event
	Acknowledge()
}

// group is several aggregates one command commits together.
type group []aggregate

func (g group) Pending() []phy.Event {
	var events []phy.Event
	for _, a := range g {
		events = append(events, a.Pending()...)
	}
	return events
}

func (g group) Acknowledge() {
	for _, a := range g {
		a.Acknowledge()
	}
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

// changeLot commits one change to a stored cargo lot, locked FOR UPDATE, at the revision the caller expects;
// apply returns the item it wrote.
func (d Deps) changeLot(ctx context.Context, s Scope, commandID, hash string, entered time.Time, id phy.CargoLotID, expected int64,
	apply func(context.Context, *phy.CargoLot) (int, error)) (Receipt, error) {
	return d.write(ctx, s, commandID, hash, func(txCtx context.Context) (Receipt, aggregate, error) {
		l, revision, err := d.Lots.Load(txCtx, s.TenantID, id)
		if err != nil {
			return Receipt{}, nil, err
		}
		if revision != expected {
			return Receipt{}, nil, phy.ErrConflict
		}
		item, err := apply(txCtx, l)
		if err != nil {
			return Receipt{}, nil, err
		}
		l.Stamp(entered)
		revision, err = d.Lots.Save(txCtx, l, revision)
		return Receipt{LotID: id, NewRevision: revision, ItemNo: item}, l, err
	})
}

// executionCommand begins a load execution command: the replay check, then the execution's consolidation, which
// never changes and is what the command is authorized on.
func (d Deps) executionCommand(ctx context.Context, s Scope, commandID string, occurredAt time.Time, command any, action Action,
	id phy.LoadExecutionID) (hash string, entered time.Time, consolidation phy.ConsolidationID, stored Receipt, replayed bool, err error) {
	if hash, entered, stored, replayed, err = d.begin(ctx, s, commandID, occurredAt, command); err != nil || replayed {
		return hash, entered, "", stored, replayed, err
	}
	if err = d.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		consolidation, err = d.Executions.Consolidation(txCtx, s.TenantID, id)
		return err
	}); err != nil {
		return "", time.Time{}, "", Receipt{}, false, err
	}
	return hash, entered, consolidation, Receipt{}, false, d.authorize(ctx, s, action, consolidation)
}

// changeExecution commits one change to a stored load execution at the revision the caller expects, after
// locking its consolidation FOR SHARE and the execution FOR UPDATE; apply returns the item it wrote. A change
// that adds nothing (a carrier message recorded already) keeps the revision.
func (d Deps) changeExecution(ctx context.Context, s Scope, commandID, hash string, entered time.Time, consolidation phy.ConsolidationID,
	id phy.LoadExecutionID, expected int64, apply func(context.Context, *phy.Consolidation, *phy.LoadExecution) (int, error)) (Receipt, error) {
	return d.write(ctx, s, commandID, hash, func(txCtx context.Context) (Receipt, aggregate, error) {
		c, _, err := d.Consolidations.LoadShared(txCtx, s.TenantID, consolidation)
		if err != nil {
			return Receipt{}, nil, err
		}
		x, revision, err := d.Executions.Load(txCtx, s.TenantID, id)
		if err != nil {
			return Receipt{}, nil, err
		}
		if revision != expected {
			return Receipt{}, nil, phy.ErrConflict
		}
		item, err := apply(txCtx, c, x)
		if err != nil {
			return Receipt{}, nil, err
		}
		if len(x.Pending()) > 0 {
			x.Stamp(entered)
			if revision, err = d.Executions.Save(txCtx, x, revision); err != nil {
				return Receipt{}, nil, err
			}
		}
		return Receipt{ExecutionID: id, ConsolidationID: consolidation, NewRevision: revision, ItemNo: item}, x, nil
	})
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
	ids []phy.ConsolidationID, apply func(context.Context, *phy.Membership, []*phy.Consolidation) error) (Receipt, error) {
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
		if err = apply(txCtx, m, consols); err != nil {
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
