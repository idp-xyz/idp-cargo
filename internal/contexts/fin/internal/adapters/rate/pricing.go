// Package rate is FIN's anticorruption layer for RATE's PricingBasis v1 (RATE -> FIN). RATE's error kinds are kept
// as text only; its quote exchange rates are for pricing and never reach FIN's conversions (R8).
package rate

import (
	"context"
	"errors"
	"fmt"

	app "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/application"
	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	rateapi "go.idp.xyz/idp-cargo/internal/contexts/rate/api"
)

// Pricing reads PricingBasis v1 of purpose QUOTE_ACCEPTED, as quoted or re-evaluated on a shipment's cargo (OD-7 A).
// Call it outside FIN's transactions.
type Pricing struct{ RATE rateapi.PricingBasisQueries }

var _ app.Pricing = Pricing{}

func (p Pricing) QuoteBasis(ctx context.Context, tenant fin.TenantID, quote string, shipped *app.ShippedCargo) (app.PricingBasis, error) {
	q := rateapi.GetPricingBasis{TenantID: string(tenant), QuoteID: quote}
	if shipped != nil {
		q.Shipment = &rateapi.ShippedCargo{ShipmentID: shipped.ShipmentID, PlanVersion: int64(shipped.PlanVersion),
			Cargo: rateapi.Cargo{Pieces: shipped.Pieces, GrossGrams: shipped.GrossGrams, VolumeCM3: shipped.VolumeCM3}}
	}
	b, err := p.RATE.GetPricingBasis(ctx, q)
	if errors.Is(err, rateapi.ErrNotFound) || errors.Is(err, rateapi.ErrRejected) {
		return app.PricingBasis{}, fmt.Errorf("%w: quote %s: %v", fin.ErrNotPriceable, quote, err.Error())
	}
	if err != nil {
		return app.PricingBasis{}, err
	}
	if b.QuoteID != quote || b.Purpose != rateapi.BasisQuoteAccepted {
		return app.PricingBasis{}, fmt.Errorf("%w: RATE answered for another quote or purpose", app.ErrUnknownSource)
	}
	out := app.PricingBasis{BasisID: b.BasisID, QuoteID: b.QuoteID, QuoteRevision: b.QuoteRevision, LegalEntityID: b.LegalEntityID,
		CustomerPartyID: b.Customer.PartyID, CustomerRevision: b.Customer.PartyRevision}
	switch {
	case shipped != nil && (b.SubjectKind != "SHIPMENT" || b.SubjectID != shipped.ShipmentID || b.SubjectVersion != int64(shipped.PlanVersion)):
		return app.PricingBasis{}, fmt.Errorf("%w: RATE re-evaluated the quote on another shipment or plan version", app.ErrUnknownSource)
	case shipped == nil && b.SubjectKind != "":
		return app.PricingBasis{}, fmt.Errorf("%w: RATE answered the quote on a shipment", app.ErrUnknownSource)
	case shipped != nil:
		out.PlanVersion = shipped.PlanVersion
	}
	for _, l := range b.Lines {
		out.Lines = append(out.Lines, fin.BasisLine{LineNo: l.LineNo, ChargeCode: l.ChargeCode, Sell: l.Sell, Cost: l.Cost})
	}
	return out, nil
}
