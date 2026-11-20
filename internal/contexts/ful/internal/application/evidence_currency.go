package application

import (
	"context"
	"fmt"
	"strings"

	ful "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/domain"
)

// EvidenceCurrency is what FUL answers SEA about a shipment's evidence: the revision and status it read, the version
// FUL last recorded, and whether RecordVerifiedLoadEvidence would record a new one now.
type EvidenceCurrency struct {
	Revision        int64
	Status          ful.ShipmentStatus
	EvidenceVersion int
	NewVersionDue   bool
}

// GetEvidenceCurrency asks whether RecordVerifiedLoadEvidence would record a new evidence version of a shipment now,
// and records nothing. It reads the shipment and PHY's evidence as the command does: the shipment first, without a
// lock, for the consolidation it is attached to; PHY's evidence for that consolidation outside any transaction; the
// shipment again, to find the attachment unchanged, and compares by the domain's own comparison. It locks nothing.
type GetEvidenceCurrency struct {
	Tx        TxRunner
	Shipments ShipmentStore
	Evidence  PhysicalEvidence
}

func (h GetEvidenceCurrency) Handle(ctx context.Context, tenant ful.TenantID, id ful.ShipmentID) (EvidenceCurrency, error) {
	if err := ctx.Err(); err != nil {
		return EvidenceCurrency{}, err
	}
	if h.Tx == nil || h.Shipments == nil || h.Evidence == nil {
		return EvidenceCurrency{}, ErrMissingDependency
	}
	if tenant == "" {
		return EvidenceCurrency{}, ErrUnauthenticated
	}
	if strings.TrimSpace(string(id)) == "" {
		return EvidenceCurrency{}, ErrInvalidQuery
	}
	if scoped, ok := h.Tx.(interface{ TenantScope() string }); ok && scoped.TenantScope() != string(tenant) {
		return EvidenceCurrency{}, ErrUnauthorized
	}
	var out EvidenceCurrency
	var consolidation string
	var air bool
	read := func(txCtx context.Context) (*ful.Shipment, error) {
		shipment, revision, err := h.Shipments.Read(txCtx, tenant, id)
		if err != nil {
			return nil, err
		}
		out.Revision, out.Status, out.EvidenceVersion = revision, shipment.Status(), 0
		if v, ok := shipment.LatestEvidence(); ok {
			out.EvidenceVersion = v.No
		}
		return shipment, nil
	}
	var cancelled bool
	if err := h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		shipment, err := read(txCtx)
		if err != nil {
			return err
		}
		// The command refuses a cancelled shipment before it asks PHY: it records nothing.
		if cancelled = shipment.Status() == ful.ShipmentCancelled; cancelled {
			return nil
		}
		consolidation = ""
		if a, ok := shipment.Attachment(ful.StageMain); ok {
			consolidation = a.ConsolidationID
		}
		air = shipment.Latest().Route.Mode == ful.ModeAir
		return nil
	}); err != nil || cancelled {
		return out, err
	}
	evidence, err := h.Evidence.Evidence(ctx, tenant, id, consolidation, air)
	if err != nil {
		return EvidenceCurrency{}, err
	}
	err = h.Tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		shipment, err := read(txCtx)
		if err != nil {
			return err
		}
		if shipment.Status() == ful.ShipmentCancelled {
			return nil // cancelled while PHY was asked: the command would record nothing
		}
		if a, ok := shipment.Attachment(ful.StageMain); (ok && a.ConsolidationID != consolidation) || (!ok && consolidation != "") {
			return fmt.Errorf("%w: the shipment's consol attachment changed while PHY was asked; try again", ful.ErrConflict)
		}
		out.NewVersionDue, err = shipment.WouldRecordEvidence(evidence)
		return err
	})
	if err != nil {
		return EvidenceCurrency{}, err
	}
	return out, nil
}
