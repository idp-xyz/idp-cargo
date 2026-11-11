package phy

import (
	"context"
	"errors"
	"fmt"

	app "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/application"
	ful "go.idp.xyz/idp-cargo/internal/contexts/ful/internal/domain"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Evidence reads a shipment's PhysicalLoadEvidence v1 from PHY (relationship 8) and sums it as FUL keeps it. Call
// it outside FUL's transactions.
type Evidence struct {
	PHY phyapi.PhysicalLoadEvidenceQueries
}

var _ app.PhysicalEvidence = Evidence{}

// total adds quantities of one unit; a quantity in another unit makes the total unknown (zero).
type total struct {
	q       kernel.Quantity
	unknown bool
}

func (t *total) add(q kernel.Quantity) {
	switch {
	case t.unknown || q == (kernel.Quantity{}):
	case t.q.Unit == "" || t.q.Unit == q.Unit:
		t.q = kernel.Quantity{Unit: q.Unit, Amount: t.q.Amount + q.Amount}
	default:
		t.unknown, t.q = true, kernel.Quantity{}
	}
}

func (e Evidence) Evidence(ctx context.Context, tenant ful.TenantID, shipment ful.ShipmentID, consolidation string, air bool) (ful.LoadEvidence, error) {
	got, err := e.PHY.GetPhysicalLoadEvidence(ctx, phyapi.GetPhysicalLoadEvidence{TenantID: string(tenant), ShipmentID: string(shipment)})
	if err != nil {
		if errors.Is(err, phyapi.ErrNotFound) || errors.Is(err, phyapi.ErrRejected) {
			return ful.LoadEvidence{}, fmt.Errorf("%w: %s: %v", app.ErrEvidenceUnavailable, shipment, err.Error())
		}
		return ful.LoadEvidence{}, err
	}
	if got.TenantID != string(tenant) || got.ShipmentID != string(shipment) {
		return ful.LoadEvidence{}, fmt.Errorf("%w: PHY answered for another shipment", app.ErrEvidenceUnavailable)
	}
	out := ful.LoadEvidence{ConsolidationID: consolidation, ReceiptComplete: len(got.Lots) > 0}
	var receivedWeight, receivedVolume, tenderedWeight, tenderedVolume total
	cleared, failed := 0, false
	for _, lot := range got.Lots {
		out.Lots++
		out.Received.Packs += lot.ReceivedPacks
		out.ReceiptComplete = out.ReceiptComplete && lot.Completed
		out.OpenDiscrepancies += len(lot.OpenDiscrepancies)
		if lot.Measurement.Version > 0 {
			receivedWeight.add(lot.Measurement.GrossWeight)
			receivedVolume.add(lot.Measurement.Volume)
		} else {
			receivedWeight.unknown = true // a lot not yet measured: the received weight is not known
		}
		switch lot.Screening.Outcome {
		case phyapi.ScreeningFailed:
			failed = true
		case phyapi.ScreeningPassed, phyapi.ScreeningNotRequired:
			cleared++
		}
	}
	for _, t := range got.Tenders {
		out.OpenDiscrepancies += len(t.OpenDiscrepancies)
		if consolidation == "" || t.ConsolidationID != consolidation {
			continue
		}
		out.Tendered.Packs += t.LoadedPacks
		tenderedWeight.add(t.GrossWeight)
		tenderedVolume.add(t.Volume)
		out.Departed, out.Arrived, out.Offloaded = out.Departed+t.Departed, out.Arrived+t.Arrived, out.Offloaded+t.Offloaded
	}
	if !receivedWeight.unknown {
		out.Received.GrossWeight = receivedWeight.q
	}
	out.Received.Volume, out.Tendered.GrossWeight, out.Tendered.Volume = receivedVolume.q, tenderedWeight.q, tenderedVolume.q
	if air && out.Lots > 0 {
		switch {
		case failed:
			out.Security = ful.SecurityFailed
		case cleared == out.Lots:
			out.Security = ful.SecurityCleared
		default:
			out.Security = ful.SecurityPending
		}
	}
	return out, nil
}
