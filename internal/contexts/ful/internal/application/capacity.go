package application

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if h.Tx == nil || h.Shipments == nil || h.Linked == nil {
		return nil, ErrMissingDependency
	}
	if tenant == "" {
		return nil, ErrUnauthenticated
	}
	if consolidation != "" && !identifierLike(consolidation) {
		return nil, ErrInvalidQuery
	}
	if scoped, ok := h.Tx.(interface{ TenantScope() string }); ok && scoped.TenantScope() != string(tenant) {
		return nil, ErrUnauthorized
	}
	var out []AwaitingCustomer
	err := h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		var after ful.ShipmentID
		for len(out) < maxAwaiting {
			ids, err := h.Linked.LinkedShipments(txCtx, tenant, consolidation, after, maxAwaiting)
			if err != nil {
				return err
			}
			for _, id := range ids {
				s, revision, err := h.Shipments.Read(txCtx, tenant, id)
				if err != nil {
					return err
				}
				if a, ok := s.Attachment(ful.StageMain); consolidation != "" && (!ok || a.ConsolidationID != consolidation) {
					continue
				}
				if link, refused, awaiting := s.AwaitingAnswer(); awaiting && len(out) < maxAwaiting {
					out = append(out, AwaitingCustomer{Shipment: s, Revision: revision, Link: link, Refused: refused})
				}
			}
			if len(ids) < maxAwaiting {
				break
			}
			after = ids[len(ids)-1]
		}
		return nil
	})
	slices.SortFunc(out, func(a, b AwaitingCustomer) int {
		switch {
		case a.Shipment.ID() < b.Shipment.ID():
			return -1
		case a.Shipment.ID() > b.Shipment.ID():
			return 1
		}
		return 0
	})
	return out, err
}

func identifierLike(s string) bool { return s != "" && len(s) <= 128 && strings.TrimSpace(s) == s }
