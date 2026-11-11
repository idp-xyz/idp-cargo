// Package domain is the model of BOOK (Carrier Booking) V0.1: a forwarder's booking of carrier capacity for a
// consolidation, recorded as the operator sends the request through the carrier's own channel and as the
// carrier answers, each with its evidence (decisions N1-BOOK OD-1, OD-2). A request is never a confirmation
// and a confirmation is never loading or departure (I-05, UL-17). Confirmed capacity is versioned; every
// version is immutable and allocated to the booking's one consumer (OD-3). It depends on the standard library
// and the kernel only.
package domain

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // cut-off time zones are checked against the IANA database wherever BOOK runs
	"unicode/utf8"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

type TenantID string
type BookingID string

// Mode is the transport mode of a booking's legs.
type Mode string

const (
	ModeAir Mode = "AIR"
	ModeSea Mode = "SEA"
)

// Kind is a booking kind of the China export slice.
type Kind string

const (
	KindAirConsol             Kind = "AIR_CONSOL"
	KindSeaLCLConsolEquipment Kind = "SEA_LCL_CONSOL_EQUIPMENT"
	KindSeaLCLCoLoad          Kind = "SEA_LCL_CO_LOAD"
)

func (k Kind) Valid() bool {
	return k == KindAirConsol || k == KindSeaLCLConsolEquipment || k == KindSeaLCLCoLoad
}

// Mode returns the kind's transport mode.
func (k Kind) Mode() Mode {
	if k == KindAirConsol {
		return ModeAir
	}
	return ModeSea
}

// Role is the ORG partner role the carrier of a booking of this kind holds (OD-4).
func (k Kind) Role() string {
	if k == KindSeaLCLCoLoad {
		return RoleCoLoader
	}
	return RoleCarrier
}

// ORG role codes BOOK asks about.
const (
	RoleCarrier  = "CARRIER"
	RoleCoLoader = "CO_LOADER"
)

// ConsumerConsolidation is the consumer kind of V0.1: a PHY consolidation.
const ConsumerConsolidation = "CONSOL"

var (
	ErrInvalidBooking    = errors.New("book: invalid booking")
	ErrInvalidRequest    = errors.New("book: invalid booking request")
	ErrInvalidCapacity   = errors.New("book: invalid capacity")
	ErrInvalidEvidence   = errors.New("book: a carrier answer and a sent request need evidence: SHA-256, storage id, media type and capture time")
	ErrInvalidReference  = errors.New("book: invalid carrier reference")
	ErrInvalidTransition = errors.New("book: invalid state transition")
	ErrInvalidChange     = errors.New("book: invalid change")
	ErrConflict          = errors.New("book: concurrent revision conflict")
	ErrCarrierCodes      = errors.New("book: an airline needs its IATA designator and AWB prefix in ORG")
	ErrExceedsRequest    = errors.New("book: the carrier confirms more than was requested")
	ErrMAWB              = errors.New("book: an air consolidation's confirmation states exactly one MAWB number, and later answers no other")
)

// Location is a port or airport as BOOK freezes it: its UN/LOCODE and, for AIR, its IATA airport code.
type Location struct {
	Code kernel.LocationCode
	IATA string
}

func (l Location) valid(m Mode) bool {
	if l.Code.Scheme != kernel.UNLOCODE || !l.Code.Valid() {
		return false
	}
	if m == ModeAir {
		return (kernel.LocationCode{Scheme: kernel.IATAAirport, Code: l.IATA}).Valid()
	}
	return l.IATA == ""
}

// RequestedLeg is a leg the forwarder asks for.
type RequestedLeg struct {
	Loading       Location
	Discharge     Location
	Flight        string
	Vessel        string
	Voyage        string
	DepartureDate string // YYYY-MM-DD at the port of loading
}

// Equipment is a number of containers of one ISO 6346 size-type.
type Equipment struct {
	SizeType string
	Count    int64
}

// Capacity is pieces, gross weight and volume for AIR and co-loads, or equipment for a container booking.
type Capacity struct {
	Pieces      int64
	GrossWeight kernel.Quantity
	Volume      kernel.Quantity
	Equipment   []Equipment
}

var (
	flightForm   = regexp.MustCompile(`^([A-Z][0-9A-Z]|[0-9][A-Z])([0-9]{1,4})([A-Z]?)$`)
	sizeTypeForm = regexp.MustCompile(`^[0-9A-Z]{2}[0-9A-Z]{2}$`)
	handlingForm = regexp.MustCompile(`^[A-Z]{3}$`)
	hexForm      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	voyageForm   = regexp.MustCompile(`^[0-9A-Z][0-9A-Z/-]{0,16}$`)
	editionForm  = regexp.MustCompile(`^[A-Z0-9][A-Z0-9._-]{0,63}$`)
)

func (c Capacity) valid(k Kind) bool {
	quantity := func(q kernel.Quantity, required bool) bool {
		if q == (kernel.Quantity{}) {
			return !required
		}
		return q.Unit.Valid() && q.Amount > 0
	}
	switch k {
	case KindAirConsol:
		return c.Pieces > 0 && quantity(c.GrossWeight, true) && quantity(c.Volume, false) && len(c.Equipment) == 0
	case KindSeaLCLCoLoad:
		return c.Pieces == 0 && quantity(c.GrossWeight, true) && quantity(c.Volume, true) && len(c.Equipment) == 0
	case KindSeaLCLConsolEquipment:
		if c.Pieces != 0 || c.GrossWeight != (kernel.Quantity{}) || c.Volume != (kernel.Quantity{}) || len(c.Equipment) == 0 {
			return false
		}
		seen := map[string]bool{}
		for _, e := range c.Equipment {
			if !sizeTypeForm.MatchString(e.SizeType) || e.Count < 1 || seen[e.SizeType] {
				return false
			}
			seen[e.SizeType] = true
		}
		return true
	}
	return false
}

// add sums capacities of one kind; quantities must share units.
func (c Capacity) add(o Capacity) (Capacity, error) {
	sum := Capacity{Pieces: c.Pieces + o.Pieces}
	var err error
	if sum.GrossWeight, err = addQuantity(c.GrossWeight, o.GrossWeight); err != nil {
		return Capacity{}, err
	}
	if sum.Volume, err = addQuantity(c.Volume, o.Volume); err != nil {
		return Capacity{}, err
	}
	counts := map[string]int64{}
	for _, e := range append(append([]Equipment(nil), c.Equipment...), o.Equipment...) {
		counts[e.SizeType] += e.Count
	}
	for _, t := range slices.Sorted(maps.Keys(counts)) {
		sum.Equipment = append(sum.Equipment, Equipment{SizeType: t, Count: counts[t]})
	}
	return sum, nil
}

func addQuantity(a, b kernel.Quantity) (kernel.Quantity, error) {
	switch {
	case a == (kernel.Quantity{}):
		return b, nil
	case b == (kernel.Quantity{}):
		return a, nil
	}
	return a.Add(b)
}

// within reports whether c fits in what was requested, and whether it is less in some dimension.
func (c Capacity) within(requested Capacity) (fits, less bool) {
	q := func(got, asked kernel.Quantity) (bool, bool) {
		if got == (kernel.Quantity{}) {
			return true, asked != (kernel.Quantity{})
		}
		if asked == (kernel.Quantity{}) || got.Unit != asked.Unit {
			return asked == (kernel.Quantity{}), false
		}
		return got.Amount <= asked.Amount, got.Amount < asked.Amount
	}
	fits, less = c.Pieces <= requested.Pieces, c.Pieces < requested.Pieces
	for _, pair := range [][2]kernel.Quantity{{c.GrossWeight, requested.GrossWeight}, {c.Volume, requested.Volume}} {
		f, l := q(pair[0], pair[1])
		fits, less = fits && f, less || l
	}
	asked := map[string]int64{}
	for _, e := range requested.Equipment {
		asked[e.SizeType] = e.Count
	}
	got := map[string]int64{}
	for _, e := range c.Equipment {
		got[e.SizeType] = e.Count
		fits = fits && e.Count <= asked[e.SizeType]
	}
	for t, n := range asked {
		less = less || got[t] < n
	}
	return fits, less
}

// Request is one immutable version of the booking request.
type Request struct {
	No                 int
	ForwarderReference string
	Legs               []RequestedLeg
	Capacity           Capacity
	HandlingCodes      []string
	LocationEdition    string
	Reason             string // why it replaces the previous version
	By                 string
	At                 time.Time
	EnteredAt          time.Time
}

// check validates a request for a kind and, for AIR, the airline's designator.
func (r Request) check(k Kind, designator string) error {
	bad := func(what string) error { return fmt.Errorf("%w: %s", ErrInvalidRequest, what) }
	m := k.Mode()
	if len(r.Legs) == 0 || len(r.Legs) > 9 {
		return bad("one to nine legs")
	}
	for i, l := range r.Legs {
		switch {
		case !l.Loading.valid(m) || !l.Discharge.valid(m) || l.Loading.Code == l.Discharge.Code:
			return bad(fmt.Sprintf("leg %d ports", i+1))
		case i > 0 && l.Loading.Code != r.Legs[i-1].Discharge.Code:
			return bad(fmt.Sprintf("leg %d does not start where leg %d ends", i+1, i))
		case l.DepartureDate != "" && !civilDate(l.DepartureDate):
			return bad(fmt.Sprintf("leg %d departure date", i+1))
		}
		if err := carriage(m, l.Flight, l.Vessel, l.Voyage, designator, false); err != nil {
			return bad(fmt.Sprintf("leg %d: %v", i+1, err))
		}
	}
	if r.Legs[0].DepartureDate == "" {
		return bad("the first leg needs a departure date")
	}
	if !r.Capacity.valid(k) {
		return fmt.Errorf("%w: requested capacity for %s", ErrInvalidCapacity, k)
	}
	seen := map[string]bool{}
	for _, c := range r.HandlingCodes {
		if !handlingForm.MatchString(c) || seen[c] {
			return bad("handling codes are distinct three-letter codes")
		}
		seen[c] = true
	}
	if utf8.RuneCountInString(r.ForwarderReference) > 35 || !editionForm.MatchString(r.LocationEdition) {
		return bad("forwarder reference or location edition")
	}
	return nil
}

// carriage checks a leg's flight (AIR) or vessel and voyage (SEA); required for a confirmed leg.
func carriage(m Mode, flight, vessel, voyage, designator string, required bool) error {
	if m == ModeAir {
		if vessel != "" || voyage != "" {
			return errors.New("no vessel or voyage on AIR")
		}
		if flight == "" {
			if required {
				return errors.New("a flight number")
			}
			return nil
		}
		parts := flightForm.FindStringSubmatch(flight)
		if parts == nil || parts[1] != designator {
			return fmt.Errorf("flight %q of the airline %s", flight, designator)
		}
		return nil
	}
	if flight != "" {
		return errors.New("no flight on SEA")
	}
	if (vessel == "") != (voyage == "") || (required && vessel == "") {
		return errors.New("vessel and voyage together")
	}
	if vessel != "" && (!text(vessel, 35) || !voyageForm.MatchString(voyage)) {
		return errors.New("vessel name or voyage")
	}
	return nil
}

func civilDate(s string) bool {
	_, err := time.Parse(time.DateOnly, s)
	return err == nil
}

// Evidence references the stored document an operator sent or received.
type Evidence struct {
	SHA256     string
	StorageID  string
	MediaType  string
	CapturedAt time.Time
}

func (e Evidence) valid(at time.Time) bool {
	return hexForm.MatchString(e.SHA256) && text(e.StorageID, 256) && text(e.MediaType, 127) && !e.CapturedAt.IsZero() &&
		!e.CapturedAt.After(at)
}

// Channel a request or answer travels by.
type Channel string

const (
	ChannelCarrierPortal  Channel = "CARRIER_PORTAL"
	ChannelEmail          Channel = "EMAIL"
	ChannelPhone          Channel = "PHONE"
	ChannelCoLoaderPortal Channel = "CO_LOADER_PORTAL"
)

func (c Channel) valid() bool {
	return c == ChannelCarrierPortal || c == ChannelEmail || c == ChannelPhone || c == ChannelCoLoaderPortal
}

// ReferenceType is the type of a carrier reference.
type ReferenceType string

const (
	ReferenceBookingNumber ReferenceType = "BOOKING_NUMBER"
	ReferenceMAWBNumber    ReferenceType = "MAWB_NUMBER"
	ReferenceCoLoad        ReferenceType = "CO_LOAD_REFERENCE"
	ReferenceRequestAck    ReferenceType = "CARRIER_REQUEST_ACK"
)

// Reference is a carrier reference, unique among the tenant's live bookings of the carrier and type.
type Reference struct {
	Type  ReferenceType
	Value string
}

// NormalizeMAWB returns a MAWB number as PPP-SSSSSSSS if it has eleven digits, spaces and a hyphen aside, and
// the eighth digit of the serial is the remainder of the first seven divided by seven (IATA Resolution 600a).
func NormalizeMAWB(s string) (string, bool) {
	digits := strings.NewReplacer("-", "", " ", "").Replace(s)
	if len(digits) != 11 {
		return "", false
	}
	for i := range len(digits) {
		if digits[i] < '0' || digits[i] > '9' {
			return "", false
		}
	}
	serial, _ := strconv.Atoi(digits[3:10])
	if int(digits[10]-'0') != serial%7 {
		return "", false
	}
	return digits[:3] + "-" + digits[3:], true
}

// ValidIMO reports whether a vessel's IMO number has seven digits whose last is the check digit: the sum of
// the first six weighted 7 to 2, modulo 10.
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

// normalized checks references a carrier stated for a booking of a kind, normalizing MAWB numbers.
func normalized(refs []Reference, k Kind, awbPrefix string) ([]Reference, error) {
	var out []Reference
	seen := map[Reference]bool{}
	for _, r := range refs {
		r.Value = strings.ToUpper(strings.TrimSpace(r.Value))
		switch r.Type {
		case ReferenceBookingNumber, ReferenceRequestAck:
		case ReferenceMAWBNumber:
			v, ok := NormalizeMAWB(r.Value)
			if k != KindAirConsol || !ok || v[:3] != awbPrefix {
				return nil, fmt.Errorf("%w: MAWB %q of the airline's prefix %s with a valid check digit", ErrInvalidReference, r.Value, awbPrefix)
			}
			r.Value = v
		case ReferenceCoLoad:
			if k != KindSeaLCLCoLoad {
				return nil, fmt.Errorf("%w: a co-load reference on a co-load booking only", ErrInvalidReference)
			}
		default:
			return nil, fmt.Errorf("%w: type %q", ErrInvalidReference, r.Type)
		}
		if !text(r.Value, 35) {
			return nil, fmt.Errorf("%w: value of %s", ErrInvalidReference, r.Type)
		}
		if !seen[r] {
			out = append(out, r)
			seen[r] = true
		}
	}
	return out, nil
}

// CutOffKind is a deadline a carrier sets.
type CutOffKind string

func (c CutOffKind) valid(m Mode) bool {
	switch c {
	case "CARGO_ACCEPTANCE", "SECURITY_SCREENING":
		return m == ModeAir
	case "DOCUMENTS", "MANIFEST":
		return true
	case "CY_FULL_CONTAINER", "SHIPPING_INSTRUCTIONS_DOCUMENTS", "VGM", "CUSTOMS_RELEASE", "HAZARDOUS", "RECEIVING_COMMENCES":
		return m == ModeSea
	}
	return false
}

// CutOff is an instant with the place's IANA time zone, never a date string (G03-02).
type CutOff struct {
	Kind     CutOffKind
	At       time.Time
	Zone     string
	Location kernel.LocationCode
}

// Leg is a confirmed leg; ETD and ETA are the carrier's estimates.
type Leg struct {
	Loading   Location
	Discharge Location
	Flight    string
	Vessel    string
	VesselIMO string
	Voyage    string
	ETD       time.Time
	ETA       time.Time
	CutOffs   []CutOff
}

// Line is a confirmed capacity line.
type Line struct {
	No               int
	Legs             []Leg
	Capacity         Capacity
	CarrierReference string
}

// checkLines validates confirmed lines against a request: legs chained from its first port to its last,
// flights of the airline or vessels and voyages, estimated times in order, cut-offs before the leg's ETD.
func checkLines(lines []Line, k Kind, request Request, designator string) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidCapacity, fmt.Sprintf(format, args...))
	}
	m := k.Mode()
	if len(lines) == 0 || len(lines) > 20 {
		return bad("one to twenty capacity lines")
	}
	first, last := request.Legs[0].Loading.Code, request.Legs[len(request.Legs)-1].Discharge.Code
	for n, line := range lines {
		if line.No != n+1 || len(line.Legs) == 0 || len(line.Legs) > 9 || !line.Capacity.valid(k) ||
			(line.CarrierReference != "" && !text(line.CarrierReference, 35)) {
			return bad("line %d needs legs and a capacity of its kind", n+1)
		}
		for i, l := range line.Legs {
			switch {
			case !l.Loading.valid(m) || !l.Discharge.valid(m) || l.Loading.Code == l.Discharge.Code:
				return bad("line %d leg %d ports", n+1, i+1)
			case i == 0 && l.Loading.Code != first, i == len(line.Legs)-1 && l.Discharge.Code != last:
				return bad("line %d runs from %s to %s, the request from %s to %s", n+1, line.Legs[0].Loading.Code.Code,
					line.Legs[len(line.Legs)-1].Discharge.Code.Code, first.Code, last.Code)
			case i > 0 && l.Loading.Code != line.Legs[i-1].Discharge.Code:
				return bad("line %d leg %d does not start where leg %d ends", n+1, i+1, i)
			case l.ETD.IsZero() || (!l.ETA.IsZero() && !l.ETA.After(l.ETD)) || (i > 0 && l.ETD.Before(line.Legs[i-1].ETD)):
				return bad("line %d leg %d estimated departure and arrival", n+1, i+1)
			case l.VesselIMO != "" && (m != ModeSea || !ValidIMO(l.VesselIMO)):
				return bad("line %d leg %d IMO number", n+1, i+1)
			}
			if err := carriage(m, l.Flight, l.Vessel, l.Voyage, designator, true); err != nil {
				return bad("line %d leg %d: %v", n+1, i+1, err)
			}
			kinds := map[CutOffKind]bool{}
			for _, c := range l.CutOffs {
				if !c.Kind.valid(m) || kinds[c.Kind] || c.At.IsZero() || c.At.After(l.ETD) || !zone(c.Zone) ||
					(!c.Location.IsZero() && (c.Location.Scheme != kernel.UNLOCODE || !c.Location.Valid())) {
					return bad("line %d leg %d cut-off %s: a kind of the mode, once, before the ETD, with an IANA zone", n+1, i+1, c.Kind)
				}
				kinds[c.Kind] = true
			}
		}
	}
	return nil
}

func zone(name string) bool {
	if name == "" || name == "Local" {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}

// total sums the capacity of lines.
func total(lines []Line) (Capacity, error) {
	var sum Capacity
	for _, l := range lines {
		var err error
		if sum, err = sum.add(l.Capacity); err != nil {
			return Capacity{}, fmt.Errorf("%w: %w", ErrInvalidCapacity, err)
		}
	}
	return sum, nil
}

const maxText = 500

func blank(s string) bool { return strings.TrimSpace(s) == "" }

func text(s string, max int) bool { return !blank(s) && utf8.RuneCountInString(s) <= max }

func identifier(s string) bool { return text(s, 128) }

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
