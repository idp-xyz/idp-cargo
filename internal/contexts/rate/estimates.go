package rate

import (
	"context"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/contexts/rate/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/rate/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/rate/internal/domain"
)

// RATE V0.5 part 2: rated pricing bases for FIN.

var _ api.PricingBasisCommands = (*Module)(nil)

func (m *Module) estimate(ctx context.Context, tenant, actor, commandID, basisID string, at time.Time, in domain.EvaluationInput) (api.Receipt, error) {
	return cardReceipt(m.cardDeps(tenant, commandID).IssueEstimate(ctx, application.EstimateCommand{CommandID: commandID, Scope: scope(tenant, actor),
		BasisID: basisID, Input: in, OccurredAt: stored(at)}))
}

func lane(mode, container, origin, destination, via, level, carrier string, c api.Cargo) domain.EvaluationInput {
	return domain.EvaluationInput{Mode: domain.Mode(mode), ContainerMode: domain.ContainerMode(container), Origin: origin, Destination: destination,
		Via: via, ServiceLevel: level, Commodity: domain.CommodityGeneral, Carrier: carrier,
		Cargo: domain.Cargo{Pieces: c.Pieces, GrossGrams: c.GrossGrams, VolumeCM3: c.VolumeCM3, Documents: c.Documents}}
}

// RateShipmentEstimate rates a shipment on both sides for its customer and issues a SHIPMENT_ESTIMATE basis.
func (m *Module) RateShipmentEstimate(ctx context.Context, c api.RateShipmentEstimate) (api.Receipt, error) {
	in := lane(c.Mode, c.ContainerMode, c.Origin, c.Destination, c.Via, c.ServiceLevel, c.Carrier, c.Cargo)
	in.Purpose, in.LegalEntityID, in.Customer = domain.PurposeShipmentEstimate, domain.LegalEntityID(c.LegalEntityID), domain.CustomerRef{PartyID: c.CustomerPartyID}
	in.SubjectID, in.SubjectVersion = c.ShipmentID, c.PlanVersion
	return m.estimate(ctx, c.TenantID, c.ActorID, c.CommandID, c.BasisID, c.OccurredAt, in)
}

// RateConsolCost rates a consolidation's BUY side and issues a CONSOL_COST_ESTIMATE basis.
func (m *Module) RateConsolCost(ctx context.Context, c api.RateConsolCost) (api.Receipt, error) {
	in := lane(c.Mode, c.ContainerMode, c.Origin, c.Destination, c.Via, c.ServiceLevel, c.Carrier, c.Cargo)
	in.Purpose, in.LegalEntityID = domain.PurposeConsolCostEstimate, domain.LegalEntityID(c.LegalEntityID)
	in.SubjectID, in.SubjectVersion = c.ConsolidationID, c.ConsolidationRevision
	return m.estimate(ctx, c.TenantID, c.ActorID, c.CommandID, c.BasisID, c.OccurredAt, in)
}

func (m *Module) ratedBasis(ctx context.Context, q api.GetPricingBasis) (api.PricingBasis, error) {
	var out api.PricingBasis
	err := m.reader(ctx, q.TenantID, func(txCtx context.Context) error {
		b, err := postgres.BasisStore{DB: m.db}.Read(txCtx, domain.TenantID(q.TenantID), q.BasisID)
		if err != nil {
			return err
		}
		st := b.State()
		out = api.PricingBasis{TenantID: q.TenantID, BasisID: st.BasisID, Purpose: string(st.Purpose), LegalEntityID: string(st.LegalEntityID),
			Customer: api.CustomerRef{PartyID: st.Customer.PartyID, PartyRevision: st.Customer.PartyRevision}, SubjectKind: st.SubjectKind,
			SubjectID: st.SubjectID, SubjectVersion: st.SubjectVersion, SnapshotID: st.SnapshotID, IssuedAt: st.IssuedAt, Lines: []api.PricingLine{},
			Gaps: []api.PricingGap{}}
		for _, l := range st.Lines {
			pl := api.PricingLine{LineNo: l.LineNo, ChargeCode: string(l.ChargeCode), Source: api.SourceRated, HasSell: l.HasSell, HasCost: l.HasCost,
				CreditorPartyID: l.CreditorPartyID}
			if l.HasSell {
				pl.Sell = l.Sell
			}
			if l.HasCost {
				pl.Cost = l.Cost
			}
			out.Lines = append(out.Lines, pl)
		}
		for _, g := range st.Gaps {
			out.Gaps = append(out.Gaps, api.PricingGap{Side: string(g.Side), ChargeCode: string(g.ChargeCode), Gap: string(g.Gap)})
		}
		return nil
	})
	return out, err
}
