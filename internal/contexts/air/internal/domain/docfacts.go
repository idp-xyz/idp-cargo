package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
)

// AIR V0.3: the movement's facts finalized for DOC (AirDocFacts v1, relationship 11). Finalizing freezes a
// version of the master and its houses that DOC builds the MAWB and HAWBs on; any later change returns the
// movement to OPEN, and the next finalization publishes the next version, which marks documents built on an
// older one stale (DOC design 24). Issuing, numbering as a legal instrument and parties stay with DOC.

var ErrNotFinalizable = errors.New("air: the movement's facts are not complete for DOC")

// SecuritySPX is the consignment security status of cargo screened by the screening party (CLEARED): secure for
// passenger, all-cargo and all-mail aircraft. AIR never sets it without PHY's screening evidence (G07-03).
const SecuritySPX = "SPX"

// MAWBSourceBooking says the MAWB number came from the airline's booking confirmation (OD-7).
const MAWBSourceBooking = "BOOKING_CONFIRMATION"

// DocFacts is the content of AirDocFacts v1 as published: the master with its airline, MAWB, route, flights,
// totals and security status, and every live house in HAWB order. It carries no parties, contacts, charges or declared
// values (DOC takes parties from COM and ORG).
type DocFacts struct {
	Version                int         `json:"version"`
	MovementID             string      `json:"movement_id"`
	ConsolidationID        string      `json:"consolidation_id"`
	OperatingLegalEntityID string      `json:"operating_legal_entity_id"`
	Airline                DocAirline  `json:"airline"`
	MAWB                   string      `json:"mawb"`
	MAWBSource             string      `json:"mawb_source"`
	BookingID              string      `json:"booking_id"`
	CapacityVersion        int         `json:"capacity_version"`
	Origin                 DocAirport  `json:"origin"`
	Destination            DocAirport  `json:"destination"`
	LocationEdition        string      `json:"location_edition"`
	Flights                []DocFlight `json:"flights"`
	Pieces                 int64       `json:"pieces"`
	GrossWeightGrams       int64       `json:"gross_weight_grams"`
	VolumeCubicCentimetres int64       `json:"volume_cubic_centimetres"`
	ChargeableGrams        int64       `json:"chargeable_grams"`
	ChargeableRule         string      `json:"chargeable_rule"`
	SecurityStatus         string      `json:"security_status"`
	Houses                 []DocHouse  `json:"houses"`
}

type DocAirline struct {
	PartyID       string `json:"party_id"`
	PartyRevision int64  `json:"party_revision"`
	Designator    string `json:"designator"`
	AWBPrefix     string `json:"awb_prefix"`
}

type DocAirport struct {
	Code string `json:"code"` // UN/LOCODE
	IATA string `json:"iata"`
}

type DocFlight struct {
	Flight string     `json:"flight"`
	From   DocAirport `json:"from"`
	To     DocAirport `json:"to"`
	ETD    time.Time  `json:"etd"`
	ETA    time.Time  `json:"eta"`
}

// DocHouse is a live house of the published facts: its HAWB, the FUL plan and evidence versions its facts came
// from, PHY's measures with the documented chargeable weight, the security status with the screening evidence,
// and its parts with the goods description of their order lines.
type DocHouse struct {
	ShipmentID             string      `json:"shipment_id"`
	HAWB                   string      `json:"hawb"`
	PlanVersion            int         `json:"plan_version"`
	EvidenceVersion        int         `json:"evidence_version"`
	Basis                  string      `json:"basis"`
	Pieces                 int64       `json:"pieces"`
	GrossWeightGrams       int64       `json:"gross_weight_grams"`
	VolumeCubicCentimetres int64       `json:"volume_cubic_centimetres"`
	ChargeableGrams        int64       `json:"chargeable_grams"`
	SecurityStatus         string      `json:"security_status"`
	Screenings             []Screening `json:"screenings"`
	Parts                  []DocPart   `json:"parts"`
}

type DocPart struct {
	ObligationID      string `json:"obligation_id"`
	ObligationVersion int64  `json:"obligation_version"`
	OrderID           string `json:"order_id"`
	Packs             int64  `json:"packs"`
	PackageType       string `json:"package_type"`
	GoodsDescription  string `json:"goods_description"`
}

// SHA256 is the SHA-256 of the facts' canonical JSON.
func (f DocFacts) SHA256() string {
	blob, _ := json.Marshal(f)
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}

// DocFactsVersion is one immutable published version.
type DocFactsVersion struct {
	No        int
	Facts     DocFacts
	SHA256    string
	By        string
	At        time.Time
	EnteredAt time.Time
}

// Finalize publishes the next AirDocFacts version of an OPEN movement with at least one live house. current is
// FUL's AirOperationPlan of every live house as read just before (T3): each must still report the house attached
// with the plan version and measures AIR froze (else the house is refreshed or ended first). Every house must be
// on PHY's measures (RECEIVED or TENDERED), cleared by screening (SPX) with the evidence, with the goods
// description of every part, and depart within its window (else FUL re-plans first). The movement becomes FINAL.
func (m *Movement) Finalize(current []Plan, by string, at time.Time) error {
	if m.state.State != StateOpen {
		return ErrInvalidTransition
	}
	houses := m.Houses()
	if len(houses) == 0 {
		return fmt.Errorf("%w: no house", ErrNotFinalizable)
	}
	plans := map[string]Plan{}
	for _, p := range current {
		plans[p.ShipmentID] = p
	}
	for _, h := range houses {
		p, ok := plans[h.ShipmentID]
		if !ok {
			return fmt.Errorf("%w: FUL's plan of %s was not read", ErrNotFinalizable, h.ShipmentID)
		}
		if err := m.admits(p); err != nil {
			return fmt.Errorf("%w: STALE_HOUSE: %s %s: %v; end its association first", ErrNotFinalizable, h.HAWB, h.ShipmentID, err)
		}
		if f, err := facts(p); err != nil || p.PlanVersion != h.PlanVersion || !sameMeasures(f, h.Facts) {
			return fmt.Errorf("%w: STALE_HOUSE: FUL reports newer facts of %s %s; refresh it first", ErrNotFinalizable, h.HAWB, h.ShipmentID)
		}
		f := h.Facts
		switch {
		case f.Basis == BasisPlanned:
			return fmt.Errorf("%w: %s %s is on planned quantities; FUL relays PHY's measures first", ErrNotFinalizable, h.HAWB, h.ShipmentID)
		case f.Security != SecurityCleared || len(f.Screenings) == 0:
			return fmt.Errorf("%w: %s %s is not cleared by screening (%q)", ErrNotFinalizable, h.HAWB, h.ShipmentID, f.Security)
		case m.WindowDrift(h):
			return fmt.Errorf("%w: WINDOW_DRIFT: %s %s does not depart within its window; FUL re-plans it first", ErrNotFinalizable, h.HAWB, h.ShipmentID)
		}
		for _, s := range f.Screenings {
			if s.Outcome != "PASSED" && s.Outcome != "NOT_REQUIRED" {
				return fmt.Errorf("%w: %s %s lot %s screening %s", ErrNotFinalizable, h.HAWB, h.ShipmentID, s.LotID, s.Outcome)
			}
		}
		for _, p := range f.Parts {
			if blank(p.GoodsDescription) {
				return fmt.Errorf("%w: %s %s has no goods description for %s; FUL re-plans it from COM first", ErrNotFinalizable, h.HAWB, h.ShipmentID,
					p.ObligationID)
			}
		}
	}
	if err := m.changeable(by, at); err != nil {
		return err
	}
	facts := m.docFacts(len(m.state.DocFacts) + 1)
	m.state.DocFacts = append(m.state.DocFacts, DocFactsVersion{No: facts.Version, Facts: facts, SHA256: facts.SHA256(), By: by, At: at.UTC()})
	m.state.State = StateFinal
	m.record(eventFinalized, at, "", 0, "")
	m.pending[len(m.pending)-1].FactsVersion = facts.Version
	return nil
}

func docAirport(a Airport) DocAirport { return DocAirport{Code: a.Code.Code, IATA: a.IATA} }

// docFacts builds the content of a version from the movement as it stands.
func (m *Movement) docFacts(version int) DocFacts {
	st, link := m.state, m.Current()
	pieces, grams, cm3, chargeable := m.Totals()
	f := DocFacts{Version: version, MovementID: string(st.MovementID), ConsolidationID: st.ConsolidationID, OperatingLegalEntityID: st.LegalEntityID,
		Airline: DocAirline{PartyID: st.Airline.PartyID, PartyRevision: st.Airline.PartyRevision, Designator: st.Airline.Designator,
			AWBPrefix: st.Airline.AWBPrefix},
		MAWB: st.MAWB.Number, MAWBSource: MAWBSourceBooking, BookingID: link.BookingID, CapacityVersion: link.CapacityVersion,
		Origin: docAirport(st.Origin), Destination: docAirport(st.Destination), LocationEdition: st.Edition, Pieces: pieces, GrossWeightGrams: grams,
		VolumeCubicCentimetres: cm3, ChargeableGrams: chargeable, ChargeableRule: ChargeableRule, SecurityStatus: SecuritySPX}
	for _, s := range link.Segments {
		f.Flights = append(f.Flights, DocFlight{Flight: s.Flight, From: docAirport(s.From), To: docAirport(s.To), ETD: s.ETD.UTC(), ETA: utc(s.ETA)})
	}
	houses := m.Houses()
	slices.SortFunc(houses, func(a, b Entry) int { return strings.Compare(a.HAWB, b.HAWB) }) // a stable order across versions
	for _, h := range houses {
		g, _ := Grams(h.Facts.GrossWeight)
		v, _ := CubicCentimetres(h.Facts.Volume)
		d := DocHouse{ShipmentID: h.ShipmentID, HAWB: h.HAWB, PlanVersion: h.PlanVersion, EvidenceVersion: h.Facts.Evidence, Basis: h.Facts.Basis,
			Pieces: h.Facts.Pieces, GrossWeightGrams: g, VolumeCubicCentimetres: v, ChargeableGrams: h.Facts.ChargeableGrams, SecurityStatus: SecuritySPX}
		for _, s := range h.Facts.Screenings {
			s.ScreenedAt = s.ScreenedAt.UTC()
			d.Screenings = append(d.Screenings, s)
		}
		for _, p := range h.Facts.Parts {
			d.Parts = append(d.Parts, DocPart{ObligationID: p.ObligationID, ObligationVersion: p.ObligationVersion, OrderID: p.OrderID, Packs: p.Packs,
				PackageType: p.PackageType, GoodsDescription: p.GoodsDescription})
		}
		f.Houses = append(f.Houses, d)
	}
	return f
}

// reopen returns a FINAL movement to OPEN on any change: the published version stays as it was and the next
// finalization publishes the next one.
func (m *Movement) reopen() {
	if m.state.State == StateFinal {
		m.state.State = StateOpen
	}
}

// open reports whether the movement takes changes: OPEN, or FINAL (which a change returns to OPEN).
func (m *Movement) open() bool { return m.state.State == StateOpen || m.state.State == StateFinal }

func (m *Movement) validateDocFacts() error {
	for i, v := range m.state.DocFacts {
		if v.No != i+1 || v.Facts.Version != v.No || v.Facts.MovementID != string(m.state.MovementID) || v.SHA256 != v.Facts.SHA256() ||
			blank(v.By) || v.At.IsZero() || (i > 0 && v.At.Before(m.state.DocFacts[i-1].At)) {
			return fmt.Errorf("doc facts version %d", i+1)
		}
	}
	if n := len(m.state.DocFacts); m.state.State == StateFinal && (n == 0 || m.docFacts(n).SHA256() != m.state.DocFacts[n-1].SHA256) {
		return fmt.Errorf("FINAL without its doc facts as they stand")
	}
	return nil
}

// LatestDocFacts returns the latest published version and whether it is current: the movement is FINAL and has
// not changed since. A version that is not current is SUPERSEDED for DOC.
func (m *Movement) LatestDocFacts() (DocFactsVersion, bool, bool) {
	n := len(m.state.DocFacts)
	if n == 0 {
		return DocFactsVersion{}, false, false
	}
	return m.state.DocFacts[n-1], m.state.State == StateFinal, true
}

// DocFactsVersion returns a published version by number and whether it is current.
func (m *Movement) DocFactsVersion(no int) (DocFactsVersion, bool, bool) {
	if no < 1 || no > len(m.state.DocFacts) {
		return DocFactsVersion{}, false, false
	}
	return m.state.DocFacts[no-1], no == len(m.state.DocFacts) && m.state.State == StateFinal, true
}

func sameScreenings(a, b []Screening) bool {
	return len(a) == len(b) && (len(a) == 0 || reflect.DeepEqual(a, b))
}
