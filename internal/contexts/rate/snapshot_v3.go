package rate

import (
	"context"

	"go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/contexts/rate/internal/domain"
)

var _ api.QuoteSnapshotV3Queries = (*Module)(nil)

// GetQuoteSnapshotV3 answers QuoteSnapshotV3 of an accepted quote: QuoteSnapshotV2 with the kind, the scope and the V0.5
// line details (RATE V0.5 part 3a). Quotes priced before V0.5 read as ONE_OFF, without a scope, with MANUAL lump sums.
func (m *Module) GetQuoteSnapshotV3(ctx context.Context, q api.GetQuoteSnapshot) (api.QuoteSnapshotV3, error) {
	v2, err := m.GetQuoteSnapshotV2(ctx, q)
	if err != nil {
		return api.QuoteSnapshotV3{}, err
	}
	quote, _, err := m.accepted(ctx, q)
	if err != nil {
		return api.QuoteSnapshotV3{}, err
	}
	s := quote.State()
	out := api.QuoteSnapshotV3{QuoteSnapshotV2: v2, Kind: "ONE_OFF"}
	if sc := s.Scope; sc != nil {
		out.Scope = &api.QuoteScope{Mode: string(sc.Mode), ContainerMode: string(sc.ContainerMode), Origin: sc.Origin, Destination: sc.Destination,
			Via: sc.Via, Carrier: sc.Carrier, Cargo: api.Cargo{Pieces: sc.Cargo.Pieces, GrossGrams: sc.Cargo.GrossGrams, VolumeCM3: sc.Cargo.VolumeCM3,
				Documents: sc.Cargo.Documents}}
	}
	for i, l := range s.Lines {
		line := api.SnapshotLineV3{SnapshotLineV2: v2.Lines[i], Source: string(domain.SourceManual), Basis: domain.BasisLumpSum,
			CodeVersion: l.CodeVersion, CodeUnlisted: l.CodeUnlisted}
		if l.Source != "" {
			line.Source = string(l.Source)
		}
		if r := l.Rated; r != nil {
			line.SnapshotID, line.RatedSell, line.OverrideReason = r.SnapshotID, r.RatedSell, r.OverrideReason
		}
		if l.Basis.Kind == domain.BasisPerUnit {
			line.Basis, line.Unit, line.Rate, line.RateScale, line.Quantity = l.Basis.Kind, string(l.Basis.Unit), l.Basis.Rate.Minor, l.Basis.Rate.Scale,
				l.Basis.Quantity
		}
		out.Lines = append(out.Lines, line)
	}
	return out, nil
}
