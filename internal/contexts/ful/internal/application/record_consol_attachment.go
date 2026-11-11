package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	ful "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/domain"
)

var ErrMembershipMismatch = errors.New("ful application: PHY does not report the shipment's membership as the command states")

// ConsolMembership is a shipment's consol membership as PHY publishes it (ConsolMembership v1, relationship 8):
// the live entry, if any, and for every consolidation the shipment left, how its latest entry there ended.
type ConsolMembership struct {
	Live  bool
	Entry ful.Membership // when Live
	Ended map[string]ful.AttachmentEnd
}

// ConsolMemberships asks PHY for a shipment's consol membership. It is called outside FUL's transactions.
type ConsolMemberships interface {
	Membership(ctx context.Context, tenant ful.TenantID, shipment ful.ShipmentID) (ConsolMembership, error)
}

type RecordConsolAttachmentCommand struct {
	CommandID        string
	Scope            Scope
	ShipmentID       ful.ShipmentID
	ExpectedRevision int64
	ConsolidationID  string
	OccurredAt       time.Time
}

// RecordConsolAttachment records the live membership entry PHY reports for the shipment in the
// consolidation; PHY committed it first (decision N1-PHY, OD-1).
type RecordConsolAttachment struct {
	Deps
	Memberships ConsolMemberships
}

func (h RecordConsolAttachment) Handle(ctx context.Context, c RecordConsolAttachmentCommand) (Receipt, error) {
	if h.Memberships == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorize(ctx, c.Scope, ActionRecordConsolAttachment, c.ShipmentID); err != nil {
		return Receipt{}, err
	}
	m, err := h.Memberships.Membership(ctx, c.Scope.TenantID, c.ShipmentID)
	if err != nil {
		return Receipt{}, err
	}
	if !m.Live || m.Entry.ConsolidationID != c.ConsolidationID {
		return Receipt{}, fmt.Errorf("%w: no live entry in %s", ErrMembershipMismatch, c.ConsolidationID)
	}
	return h.change(ctx, c.Scope, c.CommandID, hash, entered, c.ShipmentID, c.ExpectedRevision, func(s *ful.Shipment) error {
		return s.RecordConsolAttachment(m.Entry, c.Scope.ActorID, c.OccurredAt)
	})
}

type RecordConsolDetachmentCommand struct {
	CommandID        string
	Scope            Scope
	ShipmentID       ful.ShipmentID
	ExpectedRevision int64
	ConsolidationID  string
	OccurredAt       time.Time
}

// RecordConsolDetachment ends the shipment's attachment to a consolidation PHY reports it has left.
type RecordConsolDetachment struct {
	Deps
	Memberships ConsolMemberships
}

func (h RecordConsolDetachment) Handle(ctx context.Context, c RecordConsolDetachmentCommand) (Receipt, error) {
	if h.Memberships == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorize(ctx, c.Scope, ActionRecordConsolDetachment, c.ShipmentID); err != nil {
		return Receipt{}, err
	}
	m, err := h.Memberships.Membership(ctx, c.Scope.TenantID, c.ShipmentID)
	if err != nil {
		return Receipt{}, err
	}
	end, left := m.Ended[c.ConsolidationID]
	if !left || (m.Live && m.Entry.ConsolidationID == c.ConsolidationID) {
		return Receipt{}, fmt.Errorf("%w: the shipment has not left %s", ErrMembershipMismatch, c.ConsolidationID)
	}
	return h.change(ctx, c.Scope, c.CommandID, hash, entered, c.ShipmentID, c.ExpectedRevision, func(s *ful.Shipment) error {
		return s.RecordConsolDetachment(c.ConsolidationID, end, c.Scope.ActorID, c.OccurredAt)
	})
}
