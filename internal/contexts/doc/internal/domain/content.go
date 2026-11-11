package domain

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
)

// What DOC reads before its transaction (T3) to build a version.

// Facts is what DOC reads of AIR's AirDocFacts v1 version (relationship 11).
type Facts struct {
	MovementID      string
	Version         int
	SHA256          string
	Final           bool // the latest version and current (FINAL), not SUPERSEDED
	ConsolidationID string
	LegalEntityID   string
	Airline         Airline
	MAWB            string
	Origin          Airport
	Destination     Airport
	Edition         string
	Flights         []Flight
	Pieces          int64
	GrossGrams      int64
	VolumeCm3       int64
	ChargeableGrams int64
	ChargeableRule  string
	Security        string
	Houses          []FactsHouse
}

type FactsHouse struct {
	ShipmentID      string
	HAWB            string
	PlanVersion     int
	EvidenceVersion int
	Pieces          int64
	GrossGrams      int64
	VolumeCm3       int64
	ChargeableGrams int64
	Security        string
	Screenings      []Screening
	Parts           []FactsPart
}

type FactsPart struct {
	ObligationID      string
	ObligationVersion int64
	OrderID           string
	Packs             int64
	PackageType       string
	GoodsDescription  string
}

// house returns the house of a shipment in the facts.
func (f Facts) house(shipment string) (FactsHouse, bool) {
	for _, h := range f.Houses {
		if h.ShipmentID == shipment {
			return h, true
		}
	}
	return FactsHouse{}, false
}

// OrderParties is what DOC reads of COM's OrderParties v1 for an order (relationship COM -> DOC).
type OrderParties struct {
	OrderID   string
	Revision  int64
	Customer  string // the ordering party, whose credit FIN decides (C7)
	Transport Transport
	Shipper   *Party
	Consignee *Party
	Notify    *Party
	Locks     []string // the documents holding a lock on the order's parties (C12)
}

// Issuer is the issuing legal entity as ORG's LegalEntityRef v1 reports it.
type Issuer struct {
	LegalEntityID string `json:"legal_entity_id"`
	Revision      int64  `json:"revision"`
	Name          string `json:"name"`
	Jurisdiction  string `json:"jurisdiction"`
	Verified      bool   `json:"-"`
}

// The content of a version: the document as DOC publishes it (TransportDocumentContent v1). It is kept as the
// JSON it was written with, with its SHA-256.

type Airline struct {
	PartyID       string `json:"party_id"`
	PartyRevision int64  `json:"party_revision"`
	Designator    string `json:"designator"`
	AWBPrefix     string `json:"awb_prefix"`
}

type Airport struct {
	Code string `json:"code"` // UN/LOCODE
	IATA string `json:"iata"`
}

type Flight struct {
	Flight string    `json:"flight"`
	From   Airport   `json:"from"`
	To     Airport   `json:"to"`
	ETD    time.Time `json:"etd"`
	ETA    time.Time `json:"eta,omitzero"`
}

type Screening struct {
	LotID      string    `json:"lot_id"`
	Outcome    string    `json:"outcome"`
	Screener   string    `json:"screener"`
	Method     string    `json:"method"`
	Reference  string    `json:"reference"`
	ScreenedAt time.Time `json:"screened_at"`
}

type Identifier struct {
	Type    string `json:"type"`
	Country string `json:"country"`
	Value   string `json:"value,omitempty"`
	None    bool   `json:"none,omitempty"`
}

type Address struct {
	Source       string   `json:"source"` // ORG or ORDER
	ORGAddressID string   `json:"org_address_id,omitempty"`
	ORGVersion   int64    `json:"org_version,omitempty"`
	Name         string   `json:"name,omitempty"`
	Lines        []string `json:"lines,omitempty"`
	City         string   `json:"city,omitempty"`
	Region       string   `json:"region,omitempty"`
	Postcode     string   `json:"postcode,omitempty"`
	CountryCode  string   `json:"country_code"`
	UNLocode     string   `json:"un_locode,omitempty"`
}

// Party is a document party as frozen: a named ORG party with its names, identifiers and documentary address.
// Contacts are never part of a document (P5).
type Party struct {
	PartyID       string       `json:"party_id"`
	PartyRevision int64        `json:"party_revision"`
	LegalName     string       `json:"legal_name"`
	EnglishName   string       `json:"english_name,omitempty"`
	CountryCode   string       `json:"country_code"`
	Identifiers   []Identifier `json:"identifiers,omitempty"`
	Address       *Address     `json:"address,omitempty"`
	Overridden    bool         `json:"overridden,omitempty"` // an address given on the order
	Version       int64        `json:"-"`                    // the order party version COM reported
	Named         bool         `json:"-"`                    // false for a TO ORDER consignee
}

// same reports whether two orders name the same party: the same ORG party at the same revision with the same
// documentary address.
func (p *Party) same(o *Party) bool {
	if p == nil || o == nil {
		return p == o
	}
	return p.Named == o.Named && p.PartyID == o.PartyID && p.PartyRevision == o.PartyRevision && reflect.DeepEqual(p.Address, o.Address)
}

type Transport struct {
	Mode     string `json:"mode,omitempty"`
	Movement string `json:"movement,omitempty"`
}

// OrderRef is an order the parties of a house document come from, at the revision and party versions read; the
// document's C12 lock covers them.
type OrderRef struct {
	OrderID          string    `json:"order_id"`
	Revision         int64     `json:"revision"`
	Transport        Transport `json:"transport"`
	ShipperVersion   int64     `json:"shipper_version"`
	ConsigneeVersion int64     `json:"consignee_version"`
	NotifyVersion    int64     `json:"notify_version,omitempty"`
}

type SourceRef struct {
	Contract   string `json:"contract"` // AirDocFacts v1
	MovementID string `json:"movement_id"`
	Version    int    `json:"version"`
	SHA256     string `json:"sha256"`
}

type CargoLine struct {
	OrderID          string `json:"order_id,omitempty"`
	HAWB             string `json:"hawb,omitempty"` // a MAWB's line for one house
	Pieces           int64  `json:"pieces"`
	PackageType      string `json:"package_type,omitempty"`
	GrossGrams       int64  `json:"gross_weight_grams,omitempty"`
	GoodsDescription string `json:"goods_description"`
}

type Cargo struct {
	Pieces          int64       `json:"pieces"`
	GrossGrams      int64       `json:"gross_weight_grams"`
	VolumeCm3       int64       `json:"volume_cubic_centimetres"`
	ChargeableGrams int64       `json:"chargeable_weight_grams"`
	ChargeableRule  string      `json:"chargeable_rule"`
	Lines           []CargoLine `json:"lines"`
}

type Security struct {
	Status     string      `json:"status"` // SPX
	Screenings []Screening `json:"screenings,omitempty"`
}

// Content is a version's content.
type Content struct {
	Kind                string     `json:"kind"`
	DocumentID          string     `json:"document_id"`
	Number              string     `json:"number"`
	MAWB                string     `json:"mawb"`
	IssuerCapacity      string     `json:"issuer_capacity"`
	Issuer              Issuer     `json:"issuer"`
	IssuePlace          Airport    `json:"issue_place"`
	LocationEdition     string     `json:"location_edition"`
	ReleaseType         string     `json:"release_type"`
	PaymentIndicator    string     `json:"payment_indicator"`
	Source              SourceRef  `json:"source"`
	ConsolidationID     string     `json:"consolidation_id"`
	ShipmentID          string     `json:"shipment_id,omitempty"`
	Airline             Airline    `json:"airline"`
	Origin              Airport    `json:"origin"`
	Destination         Airport    `json:"destination"`
	Flights             []Flight   `json:"flights"`
	Shipper             Party      `json:"shipper"`
	Consignee           Party      `json:"consignee"`
	Notify              *Party     `json:"notify_party,omitempty"`
	Orders              []OrderRef `json:"orders,omitempty"`
	Cargo               Cargo      `json:"cargo"`
	Security            Security   `json:"security"`
	HandlingInformation string     `json:"handling_information,omitempty"`
}

// Entered is what the person drafting enters: the payment indicator and handling information.
type Entered struct {
	PaymentIndicator    string
	HandlingInformation string
}

func (e Entered) valid() bool {
	return (e.PaymentIndicator == PaymentPrepaid || e.PaymentIndicator == PaymentCollect) &&
		(e.HandlingInformation == "" || text(e.HandlingInformation))
}

// common fills what every air waybill takes from the facts and the issuer.
func common(kind Kind, id DocumentID, f Facts, issuer Issuer, entered Entered) (Content, error) {
	switch {
	case !f.Final:
		return Content{}, ErrSourceNotFinal
	case !identifier(f.MovementID) || f.Version < 1 || len(f.SHA256) != 64 || len(f.Flights) == 0 || len(f.Houses) == 0:
		return Content{}, fmt.Errorf("%w: AirDocFacts of %s", ErrInvalidDocument, f.MovementID)
	case !issuer.Verified:
		return Content{}, ErrIssuerNotVerified
	case issuer.LegalEntityID != f.LegalEntityID || !text(issuer.Name):
		return Content{}, fmt.Errorf("%w: the issuer is the movement's operating legal entity %s", ErrInvalidDocument, f.LegalEntityID)
	case !entered.valid():
		return Content{}, fmt.Errorf("%w: payment indicator PREPAID or COLLECT and handling information of at most %d characters", ErrInvalidChange, maxText)
	}
	return Content{Kind: string(kind), DocumentID: string(id), MAWB: f.MAWB, IssuerCapacity: kind.Capacity(), Issuer: issuer, IssuePlace: f.Origin,
		LocationEdition: f.Edition, ReleaseType: ReleaseConsigneeNamed, PaymentIndicator: entered.PaymentIndicator,
		Source: SourceRef{Contract: "AirDocFacts v1", MovementID: f.MovementID, Version: f.Version, SHA256: f.SHA256}, ConsolidationID: f.ConsolidationID,
		Airline: f.Airline, Origin: f.Origin, Destination: f.Destination, Flights: append([]Flight(nil), f.Flights...),
		HandlingInformation: strings.TrimSpace(entered.HandlingInformation)}, nil
}

// HouseContent builds a HAWB version of a shipment: the house from the facts, its number the HAWB AIR drew for
// the shipment, its parties those every order of its parts names (DOC_PARTIES_DIVERGE otherwise), with a named
// consignee (C11).
func HouseContent(id DocumentID, shipment string, f Facts, issuer Issuer, orders []OrderParties, entered Entered) (Content, error) {
	c, err := common(KindHAWB, id, f, issuer, entered)
	if err != nil {
		return Content{}, err
	}
	h, ok := f.house(shipment)
	if !ok {
		return Content{}, fmt.Errorf("%w: %s", ErrMasterMismatch, shipment)
	}
	c.Number, c.ShipmentID = h.HAWB, shipment
	c.Cargo = Cargo{Pieces: h.Pieces, GrossGrams: h.GrossGrams, VolumeCm3: h.VolumeCm3, ChargeableGrams: h.ChargeableGrams, ChargeableRule: f.ChargeableRule}
	var orderIDs []string
	for _, p := range h.Parts {
		c.Cargo.Lines = append(c.Cargo.Lines, CargoLine{OrderID: p.OrderID, Pieces: p.Packs, PackageType: p.PackageType, GoodsDescription: p.GoodsDescription})
		if !slices.Contains(orderIDs, p.OrderID) {
			orderIDs = append(orderIDs, p.OrderID)
		}
	}
	c.Security = Security{Status: h.Security, Screenings: append([]Screening(nil), h.Screenings...)}
	slices.Sort(orderIDs)
	byID := map[string]OrderParties{}
	for _, o := range orders {
		byID[o.OrderID] = o
	}
	var first *OrderParties
	for _, id := range orderIDs {
		o, ok := byID[id]
		if !ok {
			return Content{}, fmt.Errorf("%w: COM's parties of order %s were not read", ErrInvalidDocument, id)
		}
		switch {
		case o.Shipper == nil || !o.Shipper.Named:
			return Content{}, fmt.Errorf("%w: order %s names no shipper", ErrIncomplete, id)
		case o.Consignee == nil || !o.Consignee.Named:
			return Content{}, fmt.Errorf("%w: order %s names no consignee; an air waybill needs a named consignee (C11)", ErrIncomplete, id)
		case o.Transport.Mode != "" && o.Transport.Mode != "AIR":
			return Content{}, fmt.Errorf("%w: order %s requests %s", ErrIncomplete, id, o.Transport.Mode)
		}
		if first == nil {
			first = &o
		} else if !first.Shipper.same(o.Shipper) || !first.Consignee.same(o.Consignee) || !first.Notify.same(o.Notify) {
			return Content{}, fmt.Errorf("%w: orders %s and %s", ErrPartiesDiverge, first.OrderID, id)
		}
		ref := OrderRef{OrderID: id, Revision: o.Revision, Transport: o.Transport, ShipperVersion: o.Shipper.Version, ConsigneeVersion: o.Consignee.Version}
		if o.Notify != nil {
			ref.NotifyVersion = o.Notify.Version
		}
		c.Orders = append(c.Orders, ref)
	}
	if first == nil {
		return Content{}, fmt.Errorf("%w: the house has no parts", ErrInvalidDocument)
	}
	c.Shipper, c.Consignee = *first.Shipper, *first.Consignee
	if first.Notify != nil {
		n := *first.Notify
		c.Notify = &n
	}
	return c, nil
}

// MasterContent builds a MAWB version: the master from the facts with a line per house (the manifest), its
// number the MAWB from the booking confirmation, the issuing legal entity as shipper and the destination agent
// as consignee.
func MasterContent(id DocumentID, f Facts, issuer Issuer, agent Party, entered Entered) (Content, error) {
	c, err := common(KindMAWB, id, f, issuer, entered)
	if err != nil {
		return Content{}, err
	}
	if !agent.Named || !identifier(agent.PartyID) || agent.Address == nil {
		return Content{}, fmt.Errorf("%w: the destination agent needs its office address in ORG", ErrIncomplete)
	}
	c.Number = f.MAWB
	c.Shipper = Party{PartyID: issuer.LegalEntityID, PartyRevision: issuer.Revision, LegalName: issuer.Name, CountryCode: issuer.Jurisdiction}
	c.Consignee = agent
	c.Cargo = Cargo{Pieces: f.Pieces, GrossGrams: f.GrossGrams, VolumeCm3: f.VolumeCm3, ChargeableGrams: f.ChargeableGrams, ChargeableRule: f.ChargeableRule}
	for _, h := range f.Houses {
		var goods []string
		for _, p := range h.Parts {
			if !slices.Contains(goods, p.GoodsDescription) {
				goods = append(goods, p.GoodsDescription)
			}
		}
		c.Cargo.Lines = append(c.Cargo.Lines, CargoLine{HAWB: h.HAWB, Pieces: h.Pieces, GrossGrams: h.GrossGrams, GoodsDescription: strings.Join(goods, "; ")})
	}
	c.Security = Security{Status: f.Security}
	return c, nil
}

// complete checks what a version of its kind needs before approval: for a HAWB, a Chinese shipper's customs or
// credit code and the consignee's identifier or its recorded absence, as manifests need them (O1).
func (c Content) complete() error {
	if c.Kind != string(KindHAWB) {
		return nil
	}
	has := func(p Party, types ...string) bool {
		for _, i := range p.Identifiers {
			if slices.Contains(types, i.Type) && (i.Value != "" || i.None) {
				return true
			}
		}
		return false
	}
	if c.Shipper.CountryCode == "CN" && !has(c.Shipper, "CN_USCC", "CN_CUSTOMS") {
		return fmt.Errorf("%w: a Chinese shipper's unified social credit code or customs code", ErrIncomplete)
	}
	if c.Consignee.CountryCode != "CN" && len(c.Consignee.Identifiers) == 0 {
		return fmt.Errorf("%w: the consignee's registration code, or its recorded absence", ErrIncomplete)
	}
	return nil
}
