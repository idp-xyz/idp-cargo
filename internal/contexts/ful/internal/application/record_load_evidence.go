package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	ful "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/domain"
)

var ErrEvidenceUnavailable = errors.New("ful application: PHY does not answer the shipment's load evidence")

// PhysicalEvidence reads PHY's PhysicalLoadEvidence v1 of a shipment (relationship 8) and sums it as FUL keeps
// it: received over the lots, tendered, departed and arrived in the consolidation given (none when empty), and
// for AIR the security state. It is called outside FUL's transactions.
type PhysicalEvidence interface {
	Evidence(ctx context.Context, tenant ful.TenantID, shipment ful.ShipmentID, consolidation string, air bool) (ful.LoadEvidence, error)
}

type RecordVerifiedLoadEvidenceCommand struct {
	CommandID        string
	Scope            Scope
	ShipmentID       ful.ShipmentID
	ExpectedRevision int64
	OccurredAt       time.Time
}

// RecordVerifiedLoadEvidence reads PHY's verified evidence of the shipment before FUL's transaction (T3), for
// the consolidation FUL records it attached to, and records it as a new version when PHY's facts changed; the
// same facts again change nothing and keep the revision. If the attachment changed meanwhile, the command is a
// conflict and the caller tries again.
type RecordVerifiedLoadEvidence struct {
	Deps
	Evidence PhysicalEvidence
}

func (h RecordVerifiedLoadEvidence) Handle(ctx context.Context, c RecordVerifiedLoadEvidenceCommand) (Receipt, error) {
	if h.Evidence == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := h.begin(ctx, c.Scope, c.CommandID, c.OccurredAt, c)
	if err != nil || replayed {
		return stored, err
	}
	if err = h.authorize(ctx, c.Scope, ActionRecordVerifiedLoadEvidence, c.ShipmentID); err != nil {
		return Receipt{}, err
	}
	var consolidation string
	var air bool
	if err = h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		shipment, _, err := h.Shipments.Read(txCtx, c.Scope.TenantID, c.ShipmentID)
		if err != nil {
			return err
		}
		if shipment.Status() == ful.ShipmentCancelled {
			return ful.ErrShipmentCancelled
		}
		if a, ok := shipment.Attachment(ful.StageMain); ok {
			consolidation = a.ConsolidationID
		}
		air = shipment.Latest().Route.Mode == ful.ModeAir
		return nil
	}); err != nil {
		return Receipt{}, err
	}
	evidence, err := h.Evidence.Evidence(ctx, c.Scope.TenantID, c.ShipmentID, consolidation, air)
	if err != nil {
		return Receipt{}, err
	}
	return h.change(ctx, c.Scope, c.CommandID, hash, entered, c.ShipmentID, c.ExpectedRevision, func(s *ful.Shipment) error {
		if a, ok := s.Attachment(ful.StageMain); (ok && a.ConsolidationID != consolidation) || (!ok && consolidation != "") {
			return fmt.Errorf("%w: the shipment's consol attachment changed while PHY was asked; try again", ful.ErrConflict)
		}
		_, err := s.RecordEvidence(evidence, c.Scope.ActorID, c.OccurredAt)
		return err
	})
}
