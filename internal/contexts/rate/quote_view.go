package rate

import (
	"context"
	"fmt"

	"go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/contexts/rate/internal/adapters/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/rate/internal/domain"
)

var _ api.QuoteQueries = (*Module)(nil)

// GetQuote answers a quote as it stands, in any status, with its costs, totals and approval reasons (V0.5 part 3c).
func (m *Module) GetQuote(ctx context.Context, q api.GetQuote) (api.Quote, error) {
	if q.QuoteID == "" {
		return api.Quote{}, fmt.Errorf("%w: no quote", api.ErrNotFound)
	}
	var quote *domain.Quote
	var revision int64
	if err := m.reader(ctx, q.TenantID, func(txCtx context.Context) (err error) {
		quote, revision, err = postgres.QuoteReader{DB: m.db}.Read(txCtx, domain.TenantID(q.TenantID), domain.QuoteID(q.QuoteID))
		return err
	}); err != nil {
		return api.Quote{}, err
	}
	s, t := quote.State(), quote.State().Terms
	out := api.Quote{QuoteID: string(s.QuoteID), Revision: revision, Kind: string(quote.Kind()), Status: string(s.Status),
		LegalEntityID: string(s.LegalEntityID), Customer: api.CustomerRef{PartyID: s.Customer.PartyID, PartyRevision: s.Customer.PartyRevision},
		RecipientRole: string(s.Recipient), Currency: s.Currency, Validity: s.Validity, MarginFloorBP: s.MarginFloorBP, Author: s.Author,
		Terms: api.CommercialTerms{IncotermsRule: t.IncotermsRule, IncotermsVersion: t.IncotermsVersion, NamedPlace: t.NamedPlace,
			ServiceLevel: t.ServiceLevel}, ExpiredAt: s.ExpiredAt, Lines: []api.QuoteViewLine{}}
	if a := s.Acceptance; a != nil {
		out.Customer.PartyRevision = a.CustomerRevision
		out.Acceptance = &api.QuoteAcceptance{AcceptedBy: a.AcceptedBy, AcceptedAt: a.AcceptedAt, RecordedAt: a.RecordedAt,
			EvidenceKind: string(a.EvidenceKind), EvidenceRef: a.EvidenceRef}
	}
	if a := s.Approval; a != nil {
		out.Approval = &api.QuoteApproval{ApprovedBy: a.ApprovedBy, ApprovedAt: a.ApprovedAt, ReasonRef: a.ReasonRef, SelfApproved: a.SelfApproved}
	}
	if a := s.Agreement; a != nil {
		out.Agreement = &api.AgreementRef{RateCardID: string(a.RateCardID), Version: a.Version}
	}
	if sc := s.Scope; sc != nil {
		out.Scope = &api.QuoteScope{Mode: string(sc.Mode), ContainerMode: string(sc.ContainerMode), Origin: sc.Origin, Destination: sc.Destination,
			Via: sc.Via, Carrier: sc.Carrier, Cargo: api.Cargo{Pieces: sc.Cargo.Pieces, GrossGrams: sc.Cargo.GrossGrams, VolumeCM3: sc.Cargo.VolumeCM3,
				Documents: sc.Cargo.Documents}}
	}
	for _, r := range s.ExchangeRates {
		out.ExchangeRates = append(out.ExchangeRates, api.ExchangeRate{From: r.From, Rate: r.Rate, Scale: r.Scale, FromDigits: r.FromDigits,
			QuoteDigits: r.QuoteDigits, Source: r.Source, AsOf: r.AsOf})
	}
	reasons, err := quote.ApprovalReasons()
	if err != nil {
		return api.Quote{}, classify(err)
	}
	for _, r := range reasons {
		out.ApprovalReasons = append(out.ApprovalReasons, string(r))
	}
	if quote.Kind() == domain.KindAgreement {
		return out, nil
	}
	if out.SellTotal, out.CostTotal, err = quote.Totals(); err != nil {
		return api.Quote{}, classify(err)
	}
	converted, err := quote.Converted()
	if err != nil {
		return api.Quote{}, classify(err)
	}
	for i, l := range s.Lines {
		line := api.QuoteViewLine{SnapshotLineV3: api.SnapshotLineV3{SnapshotLineV2: api.SnapshotLineV2{ChargeCode: string(l.ChargeCode), Amount: l.Sell,
			InQuoteCurrency: converted[i], Discount: l.Discount()}, Source: string(domain.SourceManual), Basis: domain.BasisLumpSum,
			CodeUnlisted: l.CodeUnlisted}, Cost: l.Cost}
		if v := l.CodeVersion; v != nil {
			version := *v
			line.CodeVersion = &version
		}
		if l.Source != "" {
			line.Source = string(l.Source)
		}
		if r := l.Rated; r != nil {
			line.SnapshotID = r.SnapshotID
		}
		if l.Basis.Kind == domain.BasisPerUnit {
			line.Basis, line.Unit, line.Rate, line.RateScale, line.Quantity = l.Basis.Kind, string(l.Basis.Unit), l.Basis.Rate.Minor, l.Basis.Rate.Scale,
				l.Basis.Quantity
		}
		out.Lines = append(out.Lines, line)
	}
	return out, nil
}
