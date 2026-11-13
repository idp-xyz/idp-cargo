package rate

import (
	"context"
	"errors"
	"fmt"

	app "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/application"
	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	rateapi "go.idp.xyz/idp-cargo/internal/contexts/rate/api"
)

// RATE is what FIN uses of RATE: PricingBasis v1 and the commands that rate into it.
type RATE interface {
	rateapi.PricingBasisQueries
	rateapi.PricingBasisCommands
}

// Rating asks RATE to rate a shipment or a consolidation and reads the basis RATE issued (RATE V0.5 part 2). Call it
// outside FIN's transactions. RATE's error kinds are kept as text only: a lane RATE refuses makes the job or pool not
// priceable, it does not make it "not found".
type Rating struct{ RATE RATE }

var _ app.Rating = Rating{}

func (r Rating) ShipmentEstimate(ctx context.Context, tenant fin.TenantID, q app.EstimateRequest) (app.RatedBasis, error) {
	container := map[string]string{rateapi.ModeAir: rateapi.ContainerLoose, rateapi.ModeSea: rateapi.ContainerLCL}[q.Mode]
	_, err := r.RATE.RateShipmentEstimate(ctx, rateapi.RateShipmentEstimate{CommandID: "fin/" + q.CommandID, TenantID: string(tenant), ActorID: q.ActorID,
		BasisID: q.BasisID, LegalEntityID: q.LegalEntityID, CustomerPartyID: q.CustomerPartyID, ShipmentID: q.SubjectID, PlanVersion: q.SubjectVersion,
		Mode: q.Mode, ContainerMode: container, Origin: q.Origin, Destination: q.Destination, Via: q.Via, ServiceLevel: q.ServiceLevel, Carrier: q.Carrier,
		Cargo: rateapi.Cargo{Pieces: q.Pieces, GrossGrams: q.GrossGrams, VolumeCM3: q.VolumeCM3}, OccurredAt: q.OccurredAt})
	if err != nil {
		return app.RatedBasis{}, refused(err)
	}
	return r.read(ctx, tenant, q.BasisID, rateapi.BasisShipmentEstimate)
}

func (r Rating) ConsolEstimate(ctx context.Context, tenant fin.TenantID, q app.EstimateRequest) (app.RatedBasis, error) {
	container := map[string]string{rateapi.ModeAir: rateapi.ContainerLoose, rateapi.ModeSea: rateapi.ContainerLCL}[q.Mode]
	_, err := r.RATE.RateConsolCost(ctx, rateapi.RateConsolCost{CommandID: "fin/" + q.CommandID, TenantID: string(tenant), ActorID: q.ActorID,
		BasisID: q.BasisID, LegalEntityID: q.LegalEntityID, ConsolidationID: q.SubjectID, ConsolidationRevision: q.SubjectVersion, Mode: q.Mode,
		ContainerMode: container, Origin: q.Origin, Destination: q.Destination, Via: q.Via, ServiceLevel: q.ServiceLevel, Carrier: q.Carrier,
		Cargo: rateapi.Cargo{Pieces: q.Pieces, GrossGrams: q.GrossGrams, VolumeCM3: q.VolumeCM3}, OccurredAt: q.OccurredAt})
	if err != nil {
		return app.RatedBasis{}, refused(err)
	}
	return r.read(ctx, tenant, q.BasisID, rateapi.BasisConsolCostEstimate)
}

func refused(err error) error {
	switch {
	case errors.Is(err, rateapi.ErrRejected), errors.Is(err, rateapi.ErrNotFound):
		return fmt.Errorf("%w: RATE: %v", fin.ErrNotPriceable, err.Error())
	case errors.Is(err, rateapi.ErrUnauthorized), errors.Is(err, rateapi.ErrUnauthenticated):
		return fmt.Errorf("%w: RATE: %v", app.ErrUnauthorized, err.Error())
	case errors.Is(err, rateapi.ErrConflict), errors.Is(err, rateapi.ErrIdempotencyConflict):
		return fmt.Errorf("%w: RATE: %v", fin.ErrConflict, err.Error())
	}
	return err
}

func (r Rating) read(ctx context.Context, tenant fin.TenantID, id, purpose string) (app.RatedBasis, error) {
	b, err := r.RATE.GetPricingBasis(ctx, rateapi.GetPricingBasis{TenantID: string(tenant), BasisID: id})
	if err != nil {
		return app.RatedBasis{}, refused(err)
	}
	if b.BasisID != id || b.Purpose != purpose {
		return app.RatedBasis{}, fmt.Errorf("%w: RATE answered for another basis or purpose", app.ErrUnknownSource)
	}
	out := app.RatedBasis{BasisID: b.BasisID, Purpose: b.Purpose, LegalEntityID: b.LegalEntityID, CustomerPartyID: b.Customer.PartyID,
		SubjectID: b.SubjectID, SubjectVersion: b.SubjectVersion, Gaps: len(b.Gaps)}
	for _, l := range b.Lines {
		if l.Source != rateapi.SourceRated {
			return app.RatedBasis{}, fmt.Errorf("%w: a rated basis with a line of source %s", app.ErrUnknownSource, l.Source)
		}
		out.Lines = append(out.Lines, fin.EstimateLine{LineNo: l.LineNo, ChargeCode: l.ChargeCode, HasSell: l.HasSell, Sell: l.Sell, HasCost: l.HasCost,
			Cost: l.Cost, CreditorPartyID: l.CreditorPartyID})
	}
	return out, nil
}
