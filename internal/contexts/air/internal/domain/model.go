// Package domain is the model of AIR (Air Freight) V0.1 and V0.2: the air movement of an air consolidation, which holds
// the MAWB number and flights the airline confirmed in the consolidation's booking (decision N1-PHY OD-7), and
// the house air waybill entries of its houses, each with the HAWB number the operating legal entity's series
// gave it (decision N1-HAWB). Houses are FUL shipments that FUL reports attached to the consolidation (OD-2).
// Quantities are FUL's planned ones until FUL relays PHY's measures (V0.2: received, then tendered); documents
// are DOC's. It depends on the standard library and the kernel only.
package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

type TenantID string
type MovementID string

var (
	ErrInvalidMovement   = errors.New("air: invalid air movement")
	ErrInvalidBooking    = errors.New("air: the booking cannot carry the movement")
	ErrInvalidTransition = errors.New("air: invalid state transition")
	ErrInvalidChange     = errors.New("air: invalid change")
	ErrConflict          = errors.New("air: concurrent revision conflict")
	ErrNotAttached       = errors.New("air: FUL does not report the shipment PLANNED by AIR in the movement's consolidation, route and legal entity")
	ErrAlreadyAssociated = errors.New("air: the house is associated already")
	ErrNotAssociated     = errors.New("air: the house is not associated with the movement")
	ErrStillAttached     = errors.New("air: FUL still reports the shipment attached to the consolidation")
	ErrNothingToRefresh  = errors.New("air: FUL reports the plan version and the measures AIR froze")
	ErrHousesLive        = errors.New("air: houses are associated with the movement")
	ErrInvalidHAWB       = errors.New("air: a HAWB number has one to twelve capital letters and digits")
	ErrUnits             = errors.New("air: weight in KGM, GRM or TNE and volume in MTQ, DMQ, CMQ or LTR")
	ErrInvalidSeries     = errors.New("air: invalid HAWB series")
	ErrSeriesExhausted   = errors.New("air: the HAWB series has no number left")
)

// Airport is an airport as AIR freezes it: its UN/LOCODE and IATA code.
type Airport struct {
	Code kernel.LocationCode
	IATA string
}

func (a Airport) valid() bool {
	return a.Code.Scheme == kernel.UNLOCODE && a.Code.Valid() && (kernel.LocationCode{Scheme: kernel.IATAAirport, Code: a.IATA}).Valid()
}

// Airline is the airline as BOOK froze it from ORG: the party, its revision, IATA designator and AWB prefix.
type Airline struct {
	PartyID       string
	PartyRevision int64
	Designator    string
	AWBPrefix     string
}

func (a Airline) valid() bool {
	return identifier(a.PartyID) && a.PartyRevision > 0 && designatorForm.MatchString(a.Designator) && prefixForm.MatchString(a.AWBPrefix)
}

var (
	designatorForm = regexp.MustCompile(`^([A-Z][0-9A-Z]|[0-9][A-Z])$`)
	prefixForm     = regexp.MustCompile(`^[0-9]{3}$`)
	flightForm     = regexp.MustCompile(`^([A-Z][0-9A-Z]|[0-9][A-Z])[0-9]{1,4}[A-Z]?$`)
	hawbForm       = regexp.MustCompile(`^[0-9A-Z]{1,12}$`)
	editionForm    = regexp.MustCompile(`^[A-Z0-9][A-Z0-9._-]{0,63}$`)
)

// ValidMAWB reports whether a MAWB number has the form PPP-SSSSSSSS with the eighth serial digit the remainder of
// the first seven divided by seven.
func ValidMAWB(s string) bool {
	if len(s) != 12 || s[3] != '-' || !prefixForm.MatchString(s[:3]) {
		return false
	}
	for i := 4; i < 12; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	serial, _ := strconv.Atoi(s[4:11])
	return int(s[11]-'0') == serial%7
}

// ValidHAWB reports whether a HAWB number has one to twelve capital letters and digits.
func ValidHAWB(s string) bool { return hawbForm.MatchString(s) }

// Segment is a flight the airline confirmed; ETD and ETA are its estimates.
type Segment struct {
	Flight string
	From   Airport
	To     Airport
	ETD    time.Time
	ETA    time.Time
}

// Part is a part of a COM obligation version a house carries, as FUL plans it.
type Part struct {
	ObligationID      string
	ObligationVersion int64
	OrderID           string
	Packs             int64
	PackageType       string
	GrossWeight       kernel.Quantity
	Volume            kernel.Quantity
}

// ChargeableRule names the rule documented chargeable weights are computed by: the greater of gross weight and
// volumetric weight at 6,000 cm3 per kg, rounded up to the next half kilogram (IATA practice). It moves to the
// reference data with its editions later (backlog PB8).
const ChargeableRule = "AIR-VOLUMETRIC-6000-HALF-KG-V1"

// Basis of a house's quantities: FUL's plan, or PHY's measures as FUL relays them (decision N1-PHY OD-2, option
// A): the lots' latest measures once received, what was tendered in the consolidation once tendered.
const (
	BasisPlanned  = "PLANNED"
	BasisReceived = "RECEIVED"
	BasisTendered = "TENDERED"
)

func basisValid(b string) bool { return b == BasisPlanned || b == BasisReceived || b == BasisTendered }

// Security states FUL relays from PHY's screening results of AIR cargo; empty before any cargo is received.
const (
	SecurityPending = "PENDING"
	SecurityCleared = "CLEARED"
	SecurityFailed  = "FAILED"
)

func securityValid(s string) bool {
	return s == "" || s == SecurityPending || s == SecurityCleared || s == SecurityFailed
}

// Grams converts a weight to grams.
func Grams(q kernel.Quantity) (int64, error) {
	factor := map[kernel.UnitCode]int64{"KGM": 1000, "GRM": 1, "TNE": 1_000_000}[q.Unit]
	if factor == 0 || q.Amount < 0 {
		return 0, fmt.Errorf("%w: weight %s", ErrUnits, q.Unit)
	}
	return q.Amount * factor, nil
}

// CubicCentimetres converts a volume to cubic centimetres; zero is no volume.
func CubicCentimetres(q kernel.Quantity) (int64, error) {
	if q == (kernel.Quantity{}) {
		return 0, nil
	}
	factor := map[kernel.UnitCode]int64{"MTQ": 1_000_000, "DMQ": 1000, "LTR": 1000, "CMQ": 1}[q.Unit]
	if factor == 0 || q.Amount < 0 {
		return 0, fmt.Errorf("%w: volume %s", ErrUnits, q.Unit)
	}
	return q.Amount * factor, nil
}

// Chargeable computes the documented chargeable weight in grams under ChargeableRule.
func Chargeable(grossGrams, cubicCentimetres int64) int64 {
	volumetric := (cubicCentimetres + 5) / 6 // 1,000 g per 6,000 cm3, rounded up
	w := max(grossGrams, volumetric)
	return (w + 499) / 500 * 500
}

const maxText = 500

func blank(s string) bool { return strings.TrimSpace(s) == "" }

func text(s string) bool { return !blank(s) && utf8.RuneCountInString(s) <= maxText }

func identifier(s string) bool { return !blank(s) && utf8.RuneCountInString(s) <= 128 }

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
