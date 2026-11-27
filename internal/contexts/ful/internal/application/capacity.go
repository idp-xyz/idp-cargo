package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	ful "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/domain"
)

// Stage 4, V2.110: the main carriage's links to BOOK's capacity and the customer's answers to a departure outside the
// window.

const (
	ActionLinkCarrierCapacity      ShipmentAction = "LinkCarrierCapacity"
	ActionRecordCustomerDateAnswer ShipmentAction = "RecordCustomerDateAnswer"
)

var ErrCapacityUnavailable = errors.New("ful application: BOOK holds no confirmed capacity of the booking and line")

// CarrierCapacity reads BOOK's CarrierBooking v1 of a booking (relationship ful -> book) and gives the line asked
// (0: the only one) of its current capacity version. A booking that is not CONFIRMED, holds no current capacity
// version, is allocated to no consolidation or has no such line is ErrCapacityUnavailable. It is called outside FUL's
// transactions (T3).
type CarrierCapacity interface {
	Capacity(ctx context.Context, tenant ful.TenantID, booking string, line int) (ful.Capacity, error)
}

type LinkCarrierCapacityCommand struct {
	CommandID        string
	Scope            Scope
	ShipmentID       ful.ShipmentID
	ExpectedRevision int64
	BookingID        string
	Line             int
	OccurredAt       time.Time
}

// LinkCarrierCapacity reads BOOK's capacity before FUL's transaction and links the main carriage to it; the same
// carriage again changes nothing and keeps the revision.
type LinkCarrierCapacity struct {
	Deps
	Bookings CarrierCapacity
}

func (h LinkCarrierCapacity) Handle(ctx context.Context, c LinkCarrierCapacityCommand) (Receipt, error) {
	if h.Bookings == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorize(ctx, c.Scope, ActionLinkCarrierCapacity, c.ShipmentID); err != nil {
		return Receipt{}, err
	}
	if !identifierLike(c.BookingID) || c.Line < 0 {
		return Receipt{}, fmt.Errorf("%w: booking and line", ful.ErrInvalidChange)
	}
	capacity, err := h.Bookings.Capacity(ctx, c.Scope.TenantID, c.BookingID, c.Line)
	if err != nil {
		return Receipt{}, err
	}
	return h.change(ctx, c.Scope, c.CommandID, hash, entered, c.ShipmentID, c.ExpectedRevision, func(s *ful.Shipment) error {
		_, err := s.LinkCapacity(capacity, c.Scope.ActorID, c.OccurredAt)
		return err
	})
}

type RecordCustomerDateAnswerCommand struct {
	CommandID        string
	Scope            Scope
	ShipmentID       ful.ShipmentID
	ExpectedRevision int64
	LinkNo           int
	Answer           string
	EvidenceRef      string
	OccurredAt       time.Time
}

// RecordCustomerDateAnswer records the customer's answer to the current link's departure outside the window.
type RecordCustomerDateAnswer struct{ Deps }

func (h RecordCustomerDateAnswer) Handle(ctx context.Context, c RecordCustomerDateAnswerCommand) (Receipt, error) {
	hash, entered, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorize(ctx, c.Scope, ActionRecordCustomerDateAnswer, c.ShipmentID); err != nil {
		return Receipt{}, err
	}
	return h.change(ctx, c.Scope, c.CommandID, hash, entered, c.ShipmentID, c.ExpectedRevision, func(s *ful.Shipment) error {
		return s.AnswerDateChange(c.LinkNo, c.Answer, c.EvidenceRef, c.Scope.ActorID, c.OccurredAt)
	})
}

// LinkedShipments lists the ids of the shipments that are not cancelled and hold a capacity link, by id after after:
// those attached on the main carriage to a consolidation, or all of the tenant when it is empty; at most limit.
type LinkedShipments interface {
	LinkedShipments(ctx context.Context, tenant ful.TenantID, consolidation string, after ful.ShipmentID, limit int) ([]ful.ShipmentID, error)
}

// AwaitingCustomer is a shipment whose current link departs outside the window the customer agreed to.
type AwaitingCustomer struct {
	Shipment *ful.Shipment
	Revision int64
	Link     ful.CapacityLink
	Refused  bool
}

// ListDateChangesAwaitingCustomer reads the linked shipments a page at a time, without locking them, and keeps those
// awaiting the customer's answer, up to maxAwaiting of them.
type ListDateChangesAwaitingCustomer struct {
	Tx        TxRunner
	Shipments ShipmentStore
	Linked    LinkedShipments
}

const maxAwaiting = 200

func (h ListDateChangesAwaitingCustomer) Handle(ctx context.Context, tenant ful.TenantID, consolidation string) ([]AwaitingCustomer, error) {
	var out []AwaitingCustomer
	var page func(context.Context, ful.TenantID, string, ful.ShipmentID, int) ([]ful.ShipmentID, error)
	if h.Linked != nil {
		page = h.Linked.LinkedShipments
	}
	err := scanShipments(ctx, h.Tx, h.Shipments, tenant, consolidation, page, func(s *ful.Shipment, revision int64) bool {
		if link, refused, awaiting := s.AwaitingAnswer(); awaiting {
			out = append(out, AwaitingCustomer{Shipment: s, Revision: revision, Link: link, Refused: refused})
		}
		return len(out) < maxAwaiting
	}, h.Linked == nil)
	return out, err
}

// OffloadedShipments lists the ids of the shipments that are not cancelled and whose latest load evidence shows
// offloaded pieces, by id after after: those attached on the main carriage to a consolidation, or all of the tenant
// when it is empty; at most limit (V2.111).
type OffloadedShipments interface {
	OffloadedShipments(ctx context.Context, tenant ful.TenantID, consolidation string, after ful.ShipmentID, limit int) ([]ful.ShipmentID, error)
}

// AwaitingFlight is a shipment with pieces offloaded and not departed since.
type AwaitingFlight struct {
	Shipment *ful.Shipment
	Revision int64
	Pieces   int64
}

// ListPiecesAwaitingFlight reads the offloaded shipments a page at a time, without locking them, and keeps those with
// pieces still to fly, up to maxAwaiting of them.
type ListPiecesAwaitingFlight struct {
	Tx        TxRunner
	Shipments ShipmentStore
	Offloaded OffloadedShipments
}

func (h ListPiecesAwaitingFlight) Handle(ctx context.Context, tenant ful.TenantID, consolidation string) ([]AwaitingFlight, error) {
	var out []AwaitingFlight
	var page func(context.Context, ful.TenantID, string, ful.ShipmentID, int) ([]ful.ShipmentID, error)
	if h.Offloaded != nil {
		page = h.Offloaded.OffloadedShipments
	}
	err := scanShipments(ctx, h.Tx, h.Shipments, tenant, consolidation, page, func(s *ful.Shipment, revision int64) bool {
		if n := s.AwaitingLaterFlight(); n > 0 {
			out = append(out, AwaitingFlight{Shipment: s, Revision: revision, Pieces: n})
		}
		return len(out) < maxAwaiting
	}, h.Offloaded == nil)
	return out, err
}

// scanShipments reads the shipments page lists, a page at a time and by id, in one transaction without locks, keeping
// those attached to the consolidation when one is given, and hands each to keep until keep says enough.
func scanShipments(ctx context.Context, tx TxRunner, shipments ShipmentStore, tenant ful.TenantID, consolidation string,
	page func(context.Context, ful.TenantID, string, ful.ShipmentID, int) ([]ful.ShipmentID, error), keep func(*ful.Shipment, int64) bool, missing bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if tx == nil || shipments == nil || page == nil || missing {
		return ErrMissingDependency
	}
	if tenant == "" {
		return ErrUnauthenticated
	}
	if consolidation != "" && !identifierLike(consolidation) {
		return ErrInvalidQuery
	}
	if scoped, ok := tx.(interface{ TenantScope() string }); ok && scoped.TenantScope() != string(tenant) {
		return ErrUnauthorized
	}
	return tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var after ful.ShipmentID
		for {
			ids, err := page(txCtx, tenant, consolidation, after, maxAwaiting)
			if err != nil {
				return err
			}
			for _, id := range ids {
				s, revision, err := shipments.Read(txCtx, tenant, id)
				if err != nil {
					return err
				}
				if a, ok := s.Attachment(ful.StageMain); consolidation != "" && (!ok || a.ConsolidationID != consolidation) {
					continue
				}
				if !keep(s, revision) {
					return nil
				}
			}
			if len(ids) < maxAwaiting {
				return nil
			}
			after = ids[len(ids)-1]
		}
	})
}

func identifierLike(s string) bool { return s != "" && len(s) <= 128 && strings.TrimSpace(s) == s }
