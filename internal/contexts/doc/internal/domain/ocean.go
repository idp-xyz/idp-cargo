package domain

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Release types of an HBL. EXPRESS releases to the named consignee and issues no originals; the originals of
// ORIGINALS, and a consignee to order, come with the next slice.
const (
	ReleaseExpress   = "EXPRESS"
	ReleaseOriginals = "ORIGINALS"
)

// OceanFacts is what DOC reads of SEA's OceanDocFacts v1 (relationship SEA -> DOC).
type OceanFacts struct {
	MovementID      string
	Version         int
	SHA256          string
	Final           bool
	ConsolidationID string
	LegalEntityID   string
	CarrierPartyID  string
	MasterBill      string
	Vessel          string
	Voyage          string
	PlaceOfReceipt  string
	PortOfLoading   string
	PortOfDischarge string
	Edition         string
	OnBoardAt       time.Time
	Containers      []OceanContainer
	Houses          []OceanHouse
}

type OceanContainer struct {
	VisitID    string
	Number     string
	SizeType   string
	SealNumber string
}

type OceanHouse struct {
	ShipmentID string
	HBL        string
	Packs      int64
	Package    string
	GrossGrams int64
	VolumeCm3  int64
	Goods      string
	OrderIDs   []string
	Containers []OceanHouseContainer
}

type OceanHouseContainer struct {
	VisitID    string
	Number     string
	Packs      int64
	GrossGrams int64
	VolumeCm3  int64
}

// ShippedContainer is one container of an HBL, with the seal the published facts state.
type ShippedContainer struct {
	Number     string `json:"number"`
	SizeType   string `json:"size_type,omitempty"`
	SealNumber string `json:"seal_number"`
	Packs      int64  `json:"packs"`
	GrossGrams int64  `json:"gross_weight_grams"`
	VolumeCm3  int64  `json:"volume_cubic_centimetres"`
}

func (f OceanFacts) house(shipment string) (OceanHouse, bool) {
	for _, h := range f.Houses {
		if h.ShipmentID == shipment {
			return h, true
		}
	}
	return OceanHouse{}, false
}

// hblNumber is the form SEA gives an HBL and that this version keeps until the pilot: one to sixteen capital
// letters and digits.
func hblNumber(s string) bool {
	if s == "" || len(s) > 16 {
		return false
	}
	for _, r := range s {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// OceanHouseContent builds an HBL of one shipment. The number is the HBL SEA drew. The consignee is named and the
// release type is EXPRESS; a consignee to order and the originals are the next slice. The on-board date is stated
// when the facts state it, and issuing requires it.
func OceanHouseContent(id DocumentID, shipment string, f OceanFacts, issuer Issuer, orders []OrderParties, entered Entered) (Content, error) {
	switch {
	case !f.Final:
		return Content{}, ErrSourceNotFinal
	case !identifier(f.MovementID) || f.Version < 1 || len(f.SHA256) != 64 || len(f.Houses) == 0 || blank(f.MasterBill) || blank(f.Vessel) || blank(f.Voyage):
		return Content{}, fmt.Errorf("%w: OceanDocFacts of %s", ErrInvalidDocument, f.MovementID)
	case !port(f.PortOfLoading) || !port(f.PortOfDischarge):
		return Content{}, fmt.Errorf("%w: OceanDocFacts of %s names no ports", ErrInvalidDocument, f.MovementID)
	case !issuer.Verified:
		return Content{}, ErrIssuerNotVerified
	case issuer.LegalEntityID != f.LegalEntityID || !text(issuer.Name):
		return Content{}, fmt.Errorf("%w: the issuer is the movement's operating legal entity %s", ErrInvalidDocument, f.LegalEntityID)
	case !entered.valid():
		return Content{}, fmt.Errorf("%w: payment indicator PREPAID or COLLECT and handling information of at most %d characters", ErrInvalidChange, maxText)
	}
	h, ok := f.house(shipment)
	if !ok {
		return Content{}, fmt.Errorf("%w: %s", ErrMasterMismatch, shipment)
	}
	if !hblNumber(h.HBL) {
		return Content{}, fmt.Errorf("%w: HBL %q", ErrInvalidDocument, h.HBL)
	}
	receipt := f.PlaceOfReceipt
	if receipt == "" {
		receipt = f.PortOfLoading
	}
	c := Content{Kind: string(KindHBL), DocumentID: string(id), Number: h.HBL, IssuerCapacity: KindHBL.Capacity(), Issuer: issuer,
		IssuePlace: Airport{Code: f.PortOfLoading}, LocationEdition: f.Edition, ReleaseType: ReleaseExpress, PaymentIndicator: entered.PaymentIndicator,
		Source: SourceRef{Contract: "OceanDocFacts v1", MovementID: f.MovementID, Version: f.Version, SHA256: f.SHA256}, ConsolidationID: f.ConsolidationID,
		ShipmentID: shipment, Origin: Airport{Code: receipt}, Destination: Airport{Code: f.PortOfDischarge}, MasterBill: f.MasterBill, Vessel: f.Vessel,
		Voyage: f.Voyage, PlaceOfReceipt: receipt, PortOfLoading: f.PortOfLoading, PortOfDischarge: f.PortOfDischarge, OnBoardAt: f.OnBoardAt.UTC(),
		HandlingInformation: strings.TrimSpace(entered.HandlingInformation),
		Cargo:               Cargo{Pieces: h.Packs, GrossGrams: h.GrossGrams, VolumeCm3: h.VolumeCm3}}
	if f.OnBoardAt.IsZero() {
		c.OnBoardAt = time.Time{}
	}
	sealOf := map[string]OceanContainer{}
	for _, box := range f.Containers {
		sealOf[box.VisitID] = box
	}
	for _, held := range h.Containers {
		box := sealOf[held.VisitID]
		number := held.Number
		if number == "" {
			number = box.Number
		}
		c.Containers = append(c.Containers, ShippedContainer{Number: number, SizeType: box.SizeType, SealNumber: box.SealNumber, Packs: held.Packs,
			GrossGrams: held.GrossGrams, VolumeCm3: held.VolumeCm3})
	}
	orderIDs := append([]string(nil), h.OrderIDs...)
	if len(orderIDs) == 0 {
		return Content{}, fmt.Errorf("%w: the house names no order", ErrInvalidDocument)
	}
	slices.Sort(orderIDs)
	byID := map[string]OrderParties{}
	for _, o := range orders {
		byID[o.OrderID] = o
	}
	var first *OrderParties
	for _, orderID := range orderIDs {
		o, ok := byID[orderID]
		if !ok {
			return Content{}, fmt.Errorf("%w: COM's parties of order %s were not read", ErrInvalidDocument, orderID)
		}
		switch {
		case o.Shipper == nil || !o.Shipper.Named:
			return Content{}, fmt.Errorf("%w: order %s names no shipper", ErrIncomplete, orderID)
		case o.Consignee == nil || !o.Consignee.Named:
			return Content{}, fmt.Errorf("%w: order %s names no consignee; an HBL to order comes later", ErrIncomplete, orderID)
		case o.Transport.Mode != "" && o.Transport.Mode != "SEA":
			return Content{}, fmt.Errorf("%w: order %s requests %s", ErrIncomplete, orderID, o.Transport.Mode)
		}
		if first == nil {
			first = &o
		} else if !first.Shipper.same(o.Shipper) || !first.Consignee.same(o.Consignee) || !first.Notify.same(o.Notify) {
			return Content{}, fmt.Errorf("%w: orders %s and %s", ErrPartiesDiverge, first.OrderID, orderID)
		}
		ref := OrderRef{OrderID: orderID, Revision: o.Revision, Transport: o.Transport, ShipperVersion: o.Shipper.Version, ConsigneeVersion: o.Consignee.Version}
		if o.Notify != nil {
			ref.NotifyVersion = o.Notify.Version
		}
		c.Orders = append(c.Orders, ref)
		c.Cargo.Lines = append(c.Cargo.Lines, CargoLine{OrderID: orderID, Pieces: h.Packs, PackageType: h.Package, GoodsDescription: h.Goods})
	}
	if len(orderIDs) > 1 {
		for i := range c.Cargo.Lines {
			c.Cargo.Lines[i].Pieces = 0
		}
	}
	c.Shipper, c.Consignee = *first.Shipper, *first.Consignee
	if first.Notify != nil {
		n := *first.Notify
		c.Notify = &n
	}
	return c, nil
}

func port(code string) bool { return len(code) == 5 && code == strings.ToUpper(code) }
