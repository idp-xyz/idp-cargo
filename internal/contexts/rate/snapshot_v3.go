package rate

import (
	"context"

	"go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/contexts/rate/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

var _ api.QuoteSnapshotV3Queries = (*Module)(nil)

// GetQuoteSnapshotV3 answers QuoteSnapshotV3 of an accepted quote: QuoteSnapshotV2 with the kind, the scope and the V0.5
// line details (RATE V0.5 part 3a), from one read of the quote. Quotes priced before V0.5 read as ONE_OFF, without a
// scope, with MANUAL lump sums and no catalogue version.
func (m *Module) GetQuoteSnapshotV3(ctx context.Context, q api.GetQuoteSnapshot) (api.QuoteSnapshotV3, error) {
	quote, revision, err := m.accepted(ctx, q)
	if err != nil {
		return api.QuoteSnapshotV3{}, err
	}
	v2, err := snapshotV2(quote, revision)
	if err != nil {
		return api.QuoteSnapshotV3{}, err
	}
	s := quote.State()
	out := api.QuoteSnapshotV3{QuoteSnapshotV2: v2, Kind: string(quote.Kind())}
	if a := s.Agreement; a != nil {
		// an agreement prices by lane with its card; it has no total COM could take as a commitment (OD-5)
		out.Agreement, out.Total = &api.AgreementRef{RateCardID: string(a.RateCardID), Version: a.Version}, kernel.Money{}
	}
	if sc := s.Scope; sc != nil {
		out.Scope = &api.QuoteScope{Mode: string(sc.Mode), ContainerMode: string(sc.ContainerMode), Origin: sc.Origin, Destination: sc.Destination,
			Via: sc.Via, Carrier: sc.Carrier, Cargo: api.Cargo{Pieces: sc.Cargo.Pieces, GrossGrams: sc.Cargo.GrossGrams, VolumeCM3: sc.Cargo.VolumeCM3,
				Documents: sc.Cargo.Documents}}
	}
	for i, l := range s.Lines {
		line := api.SnapshotLineV3{SnapshotLineV2: v2.Lines[i], Source: string(domain.SourceManual), Basis: domain.BasisLumpSum,
			CodeUnlisted: l.CodeUnlisted}
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
