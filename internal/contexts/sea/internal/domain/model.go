// Package domain is the model of SEA (Sea Freight) V0.1: the ocean movement of a sea LCL consolidation, which holds
// the voyage legs and cut-offs the carrier or co-loader confirmed in the consolidation's booking, the master bill
// reference the carrier or co-loader states, and the house bill entries of its houses, each with the HBL number the
// operating legal entity's series gave it. Houses are FUL shipments that FUL reports attached to the consolidation
// (decision N1-PHY OD-2, option A). Quantities are FUL's planned ones until FUL relays PHY's measures and the sealed
// containers. Bills of lading are DOC's; container numbers and VGM come with the container visits (part 1e). It
// depends on the standard library and the kernel only.
package domain

import (
	"errors"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

type TenantID string
type MovementID string

var (
	ErrInvalidMovement   = errors.New("sea: invalid ocean movement")
	ErrInvalidBooking    = errors.New("sea: the booking cannot carry the movement")
	ErrInvalidTransition = errors.New("sea: invalid state transition")
	ErrInvalidChange     = errors.New("sea: invalid change")
	ErrConflict          = errors.New("sea: concurrent revision conflict")
	ErrNotAttached       = errors.New("sea: FUL does not report the shipment PLANNED by SEA as LCL in the movement's consolidation, route and legal entity")
	ErrAlreadyAssociated = errors.New("sea: the house is associated already")
	ErrNotAssociated     = errors.New("sea: the house is not associated with the movement")
	ErrStillAttached     = errors.New("sea: FUL still reports the shipment attached to the consolidation")
	ErrNothingToRefresh  = errors.New("sea: FUL reports the plan version, the measures and the containers SEA froze")
	ErrHousesLive        = errors.New("sea: houses are associated with the movement")
	ErrInvalidHBL        = errors.New("sea: an HBL number has one to sixteen capital letters and digits")
	ErrInvalidMasterBill = errors.New("sea: invalid master bill reference")
	ErrInvalidSeries     = errors.New("sea: invalid HBL series")
	ErrSeriesExhausted   = errors.New("sea: the HBL series has no number left")
)

// Kind is the kind of the movement's consolidation (N1-PHY OD-6), which SEA takes from the kind of its booking.
type Kind string

const (
	KindOwn    Kind = "SEA_LCL_OWN"    // the consolidator's own container, booked with a shipping line
	KindCoLoad Kind = "SEA_LCL_COLOAD" // LCL space bought from a co-loader
)

func (k Kind) valid() bool { return k == KindOwn || k == KindCoLoad }

// Role is the ORG partner role the carrier of a movement of this kind holds, as BOOK froze it.
func (k Kind) Role() string {
	if k == KindCoLoad {
		return "CO_LOADER"
	}
	return "CARRIER"
}

// Carrier is the shipping line or co-loader as BOOK froze it from ORG: the party, its revision and its role.
type Carrier struct {
	PartyID       string
	PartyRevision int64
	Role          string
}

func (c Carrier) valid(k Kind) bool {
	return identifier(c.PartyID) && c.PartyRevision > 0 && c.Role == k.Role()
}

var (
	hblForm      = regexp.MustCompile(`^[0-9A-Z]{1,16}$`)
	billForm     = regexp.MustCompile(`^[0-9A-Z]+([/-][0-9A-Z]+)*$`)
	voyageForm   = regexp.MustCompile(`^[0-9A-Z][0-9A-Z/-]{0,16}$`)
	editionForm  = regexp.MustCompile(`^[A-Z0-9][A-Z0-9._-]{0,63}$`)
	zoneForm     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+-]*(/[A-Za-z0-9_+-]+)*$`)
	sizeTypeForm = regexp.MustCompile(`^[0-9A-Z]{2}[A-Z][0-9A-Z]$`)
	packageForm  = regexp.MustCompile(`^[A-Z0-9]{2}$`)
	hexForm      = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidHBL reports whether an HBL number has one to sixteen capital letters and digits. The design gives no form; the
// bound is SEA's own and deliberately narrow, to verify against the manifest and filing formats before the pilot: a
// later migration can widen it, while numbers once given are kept for ever and could not be narrowed.
func ValidHBL(s string) bool { return hblForm.MatchString(s) }

// NormalizeBillNumber returns a master bill number as SEA keeps it: trimmed, in capital letters.
func NormalizeBillNumber(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

// ValidBillNumber reports whether a master bill number, as its carrier or co-loader states it, has capital letters
// and digits with single hyphens or slashes inside, 35 characters at most (the length BOOK keeps carrier references
// at).
func ValidBillNumber(s string) bool { return len(s) <= 35 && billForm.MatchString(s) }

// ValidIMO reports whether a vessel's IMO number has seven digits whose last is the check digit: the sum of the
// first six weighted 7 to 2, modulo 10.
func ValidIMO(s string) bool {
	if len(s) != 7 {
		return false
	}
	sum := 0
	for i := range 7 {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
		if i < 6 {
			sum += int(s[i]-'0') * (7 - i)
		}
	}
	return int(s[6]-'0') == sum%10
}

func port(l kernel.LocationCode) bool { return l.Scheme == kernel.UNLOCODE && l.Valid() }

// Cut-off kinds BOOK records for SEA.
var cutOffKinds = []string{"DOCUMENTS", "MANIFEST", "CY_FULL_CONTAINER", "SHIPPING_INSTRUCTIONS_DOCUMENTS", "VGM", "CUSTOMS_RELEASE", "HAZARDOUS",
	"RECEIVING_COMMENCES"}

// CutOff is a deadline the carrier set for a leg, as BOOK recorded it: an instant with the place's IANA time zone
// and, when it is not the leg's port of loading, the place.
type CutOff struct {
	Kind     string
	At       time.Time
	TimeZone string
	Location kernel.LocationCode
}

func (c CutOff) valid() bool {
	return slices.Contains(cutOffKinds, c.Kind) && !c.At.IsZero() && len(c.TimeZone) <= 64 && zoneForm.MatchString(c.TimeZone) &&
		(c.Location.IsZero() || port(c.Location))
}

// Leg is a voyage leg the carrier confirmed; ETD and ETA are its estimates.
type Leg struct {
	Vessel    string
	VesselIMO string
	Voyage    string
	From      kernel.LocationCode
	To        kernel.LocationCode
	ETD       time.Time
	ETA       time.Time
	CutOffs   []CutOff
}

// Part is a part of a COM obligation version a house carries, as FUL plans it, with the goods description of the
// order line (empty for parts FUL planned before it kept one).
type Part struct {
	ObligationID      string
	ObligationVersion int64
	OrderID           string
	Packs             int64
	PackageType       string
	GrossWeight       kernel.Quantity
	Volume            kernel.Quantity
	GoodsDescription  string
}

func (p Part) valid() bool {
	return identifier(p.ObligationID) && p.ObligationVersion > 0 && identifier(p.OrderID) && p.Packs > 0 && packageForm.MatchString(p.PackageType) &&
		measure(p.GrossWeight) && p.GrossWeight.Amount > 0 && measure(p.Volume) && (p.GoodsDescription == "" || text(p.GoodsDescription))
}

// Stuffing is a sealed container of the consolidation with the house's packs in it, as PHY verified them and FUL
// relays them: PHY's stuffing and planned unit with its ISO 6346 size-type, the house's packs, gross weight and
// volume in it, PHY's load lines of the house with live packs in it, and the seal PHY recorded.
type Stuffing struct {
	ExecutionID string
	LoadUnitNo  int
	TypeCode    string
	Packs       int64
	GrossWeight kernel.Quantity
	Volume      kernel.Quantity
	LineNos     []int
	Seal        string
	SealSHA256  string
	SealedAt    time.Time
}

func (s Stuffing) valid() bool {
	if !identifier(s.ExecutionID) || s.LoadUnitNo < 1 || !sizeTypeForm.MatchString(s.TypeCode) || s.Packs < 1 || !measure(s.GrossWeight) ||
		!measure(s.Volume) || len(s.LineNos) == 0 || !text(s.Seal) || (s.SealSHA256 != "" && !hexForm.MatchString(s.SealSHA256)) || s.SealedAt.IsZero() {
		return false
	}
	for _, n := range s.LineNos {
		if n < 1 {
			return false
		}
	}
	return true
}

func (s Stuffing) same(o Stuffing) bool {
	return s.ExecutionID == o.ExecutionID && s.LoadUnitNo == o.LoadUnitNo && s.TypeCode == o.TypeCode && s.Packs == o.Packs &&
		s.GrossWeight == o.GrossWeight && s.Volume == o.Volume && slices.Equal(s.LineNos, o.LineNos) && s.Seal == o.Seal &&
		s.SealSHA256 == o.SealSHA256 && s.SealedAt.Equal(o.SealedAt)
}

// Basis of a house's quantities: FUL's plan, or PHY's measures as FUL relays them (decision N1-PHY OD-2, option
// A): the lots' latest measures once received, what was stuffed or tendered in the consolidation once tendered.
const (
	BasisPlanned  = "PLANNED"
	BasisReceived = "RECEIVED"
	BasisTendered = "TENDERED"
)

func basisValid(b string) bool { return b == BasisPlanned || b == BasisReceived || b == BasisTendered }

// measure reports whether a quantity is none, or a non-negative amount in a unit code.
func measure(q kernel.Quantity) bool {
	if q == (kernel.Quantity{}) {
		return true
	}
	return q.Unit.Valid() && q.Amount >= 0
}

// Grams converts a weight in KGM, GRM or TNE to grams; other units are not converted.
func Grams(q kernel.Quantity) (int64, bool) {
	return scaled(q, map[kernel.UnitCode]int64{"KGM": 1000, "GRM": 1, "TNE": 1_000_000})
}

// CubicCentimetres converts a volume in MTQ, DMQ, LTR or CMQ to cubic centimetres; other units are not converted.
func CubicCentimetres(q kernel.Quantity) (int64, bool) {
	return scaled(q, map[kernel.UnitCode]int64{"MTQ": 1_000_000, "DMQ": 1000, "LTR": 1000, "CMQ": 1})
}

func scaled(q kernel.Quantity, factors map[kernel.UnitCode]int64) (int64, bool) {
	factor := factors[q.Unit]
	if factor == 0 || q.Amount < 0 || q.Amount > math.MaxInt64/factor {
		return 0, false
	}
	return q.Amount * factor, true
}

const maxText = 500

func blank(s string) bool { return strings.TrimSpace(s) == "" }

func text(s string) bool { return !blank(s) && utf8.RuneCountInString(s) <= maxText }

func short(s string, n int) bool {
	return !blank(s) && strings.TrimSpace(s) == s && utf8.RuneCountInString(s) <= n
}

func identifier(s string) bool { return !blank(s) && utf8.RuneCountInString(s) <= 128 }

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func utc(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC()
}
