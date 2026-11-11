// Package air is DOC's anticorruption layer for AIR's AirDocFacts v1 (relationship 11). AIR's error kinds are kept
// as text only.
package air

import (
	"context"
	"errors"
	"fmt"
	"time"

	airapi "go.idp.xyz/idp-cargo/internal/contexts/air/api"
	app "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/application"
	doc "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/domain"
)

// Facts reads AirDocFacts v1. Call it outside DOC's transactions.
type Facts struct{ AIR airapi.AirDocFactsQueries }

var _ app.Facts = Facts{}

func airport(a airapi.Airport) doc.Airport { return doc.Airport{Code: a.Code.Code, IATA: a.IATA} }

func (f Facts) Latest(ctx context.Context, tenant doc.TenantID, movement string) (doc.Facts, error) {
	got, err := f.AIR.GetAirDocFacts(ctx, airapi.GetAirDocFacts{TenantID: string(tenant), MovementID: movement})
	if errors.Is(err, airapi.ErrNotFound) {
		return doc.Facts{}, fmt.Errorf("%w: AirDocFacts of %s: %v", app.ErrUnknownSource, movement, err.Error())
	}
	if err != nil {
		return doc.Facts{}, err
	}
	if got.TenantID != string(tenant) || got.MovementID != movement {
		return doc.Facts{}, fmt.Errorf("%w: AIR answered for another movement", app.ErrUnknownSource)
	}
	out := doc.Facts{MovementID: movement, Version: got.Version, SHA256: got.SHA256, Final: got.Status == airapi.DocFactsFinal,
		ConsolidationID: got.ConsolidationID, LegalEntityID: got.OperatingLegalEntityID,
		Airline: doc.Airline{PartyID: got.AirlinePartyID, PartyRevision: got.AirlinePartyRevision, Designator: got.AirlineDesignator, AWBPrefix: got.AWBPrefix},
		MAWB:    got.MAWB, Origin: airport(got.Origin), Destination: airport(got.Destination), Edition: got.LocationEdition, Pieces: got.Pieces,
		GrossGrams: got.GrossWeight.Amount, VolumeCm3: got.Volume.Amount, ChargeableGrams: got.ChargeableWeight.Amount, ChargeableRule: got.ChargeableRule,
		Security: got.SecurityStatus}
	for _, s := range got.Flights {
		out.Flights = append(out.Flights, doc.Flight{Flight: s.Flight, From: airport(s.From), To: airport(s.To), ETD: s.ETD.UTC(), ETA: utc(s.ETA)})
	}
	for _, h := range got.Houses {
		house := doc.FactsHouse{ShipmentID: h.ShipmentID, HAWB: h.HAWB, PlanVersion: h.PlanVersion, EvidenceVersion: h.EvidenceVersion, Pieces: h.Pieces,
			GrossGrams: h.GrossWeight.Amount, VolumeCm3: h.Volume.Amount, ChargeableGrams: h.ChargeableWeight.Amount, Security: h.SecurityStatus}
		for _, s := range h.Screenings {
			house.Screenings = append(house.Screenings, doc.Screening{LotID: s.LotID, Outcome: s.Outcome, Screener: s.Screener, Method: s.Method,
				Reference: s.Reference, ScreenedAt: s.ScreenedAt.UTC()})
		}
		for _, p := range h.Parts {
			house.Parts = append(house.Parts, doc.FactsPart{ObligationID: p.ObligationID, ObligationVersion: p.ObligationVersion, OrderID: p.OrderID,
				Packs: p.Packs, PackageType: p.PackageType, GoodsDescription: p.GoodsDescription})
		}
		out.Houses = append(out.Houses, house)
	}
	return out, nil
}

func utc(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC()
}
