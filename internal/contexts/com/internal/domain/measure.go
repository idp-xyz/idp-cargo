package domain

import (
	"math"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// PackageType is a package type code in the form of UN/ECE Recommendation 21, such as PX for pallets or CT
// for cartons. COM checks only the form; the code list is reference data (MD-25).
type PackageType string

// Valid reports whether p is two upper-case ASCII letters or digits.
func (p PackageType) Valid() bool {
	return len(p) == 2 && alnum(p[0]) && alnum(p[1])
}

func alnum(b byte) bool { return (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') }

// Measure is what an order line orders or an obligation commits: a number of packs with their declared
// gross weight and, when declared, their volume (C3). Chargeable weight is not part of it; it is a rating
// basis that RATE and FIN own.
type Measure struct {
	Packs       int64
	GrossWeight kernel.Quantity
	Volume      kernel.Quantity // zero when not declared
}

// DeclaresVolume reports whether the measure includes a volume.
func (m Measure) DeclaresVolume() bool { return m.Volume != kernel.Quantity{} }

func (m Measure) valid() bool {
	return m.Packs > 0 && m.GrossWeight.Unit.Valid() && m.GrossWeight.Amount > 0 &&
		(!m.DeclaresVolume() || m.Volume.Unit.Valid() && m.Volume.Amount > 0)
}

// sub returns m − o. Gross weights must be in the same unit, and volumes too; a volume declared on one
// side only is a unit mismatch.
func (m Measure) sub(o Measure) (Measure, error) {
	if (o.Packs > 0 && m.Packs < math.MinInt64+o.Packs) || (o.Packs < 0 && m.Packs > math.MaxInt64+o.Packs) {
		return Measure{}, kernel.ErrAmountOverflow
	}
	weight, err := m.GrossWeight.Sub(o.GrossWeight)
	if err != nil {
		return Measure{}, err
	}
	volume, err := m.Volume.Sub(o.Volume)
	if err != nil {
		return Measure{}, err
	}
	return Measure{Packs: m.Packs - o.Packs, GrossWeight: weight, Volume: volume}, nil
}

// negative reports whether any part of m is below zero.
func (m Measure) negative() bool {
	return m.Packs < 0 || m.GrossWeight.Amount < 0 || m.Volume.Amount < 0
}
