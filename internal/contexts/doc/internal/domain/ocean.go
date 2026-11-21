package domain

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Release types of a sea house document. EXPRESS releases an HBL to the named consignee and issues no originals.
// ORIGINALS issues one to nine original bills. SEA_WAYBILL is a house sea waybill: a named consignee and no originals.
const (
	ReleaseExpress    = "EXPRESS"
	ReleaseOriginals  = "ORIGINALS"
	ReleaseSeaWaybill = "SEA_WAYBILL"

	// Statuses of an original bill: out with its holder; back with the issuer for an amendment or a cancellation;
	// surrendered with the whole set at origin (telex release). Each change is a new record.
	OriginalIssued      = "ISSUED"
	OriginalReturned    = "RETURNED"
	OriginalSurrendered = "SURRENDERED_AT_ORIGIN"
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
	OnBoardAt       time.Time // the movement's: the earliest loading of any container or carrier report
	// ContainerDates says that the containers below state their own on-board date (OceanDocFacts published from V2.90).
	// Facts published before state none for any container, and a house is then dated by the movement's date.
	ContainerDates bool
	Containers     []OceanContainer
	Houses         []OceanHouse

	// What the carrier's own document reads (carrier.go): the carrier's party revision, its role (CARRIER, or CO_LOADER
	// on a co-load movement) and the kind of the master bill reference. A house document does not carry them.
	CarrierPartyRevision int64
	CarrierRole          string
	MasterBillKind       string
}

type OceanContainer struct {
	VisitID    string
	Number     string
	SizeType   string
	SealNumber string
	OnBoardAt  time.Time // this container's own loading; zero when it is not loaded (read only with ContainerDates)
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

// HouseOnBoard is when all of a house's cargo is on board: the latest on-board date among the containers its breakdown
// names. It is zero, with the numbers of the containers that have no date, when any of them is not loaded. Two readings
// stay the movement's date, so that nothing becomes impossible to issue: facts published before the containers carried
// a date (ContainerDates false; they are replaced by SEA's next finalization), and a house whose breakdown names no
// container (a co-load movement has none of its own, and its loading is the carrier's report).
func (f OceanFacts) HouseOnBoard(h OceanHouse) (at time.Time, notLoaded []string) {
	if !f.ContainerDates || len(h.Containers) == 0 {
		return f.OnBoardAt.UTC(), nil
	}
	own := map[string]OceanContainer{}
	for _, box := range f.Containers {
		own[box.VisitID] = box
	}
	for _, held := range h.Containers {
		box, known := own[held.VisitID]
		number := held.Number
		if number == "" {
			number = box.Number
		}
		switch {
		case !known || box.OnBoardAt.IsZero():
			if !slices.Contains(notLoaded, number) {
				notLoaded = append(notLoaded, number)
			}
		case box.OnBoardAt.After(at):
			at = box.OnBoardAt.UTC()
		}
	}
	if len(notLoaded) > 0 {
		return time.Time{}, notLoaded
	}
	return at, nil
}

// shippedContainers is the containers of a house bill: one for each entry of the house's breakdown, with the size type
// and seal of the facts' container of the same visit.
func (f OceanFacts) shippedContainers(h OceanHouse) []ShippedContainer {
	sealOf := map[string]OceanContainer{}
	for _, box := range f.Containers {
		sealOf[box.VisitID] = box
	}
	var out []ShippedContainer
	for _, held := range h.Containers {
		box := sealOf[held.VisitID]
		number := held.Number
		if number == "" {
			number = box.Number
		}
		out = append(out, ShippedContainer{Number: number, SizeType: box.SizeType, SealNumber: box.SealNumber, Packs: held.Packs,
			GrossGrams: held.GrossGrams, VolumeCm3: held.VolumeCm3})
	}
	return out
}

// HouseChanges lists what the facts f now say of a house that the content c of its bill does not: the on-board date,
// the containers and seals, the packs, weights and volume, the voyage, ports and master bill. It compares only what
// comes from SEA, as OceanHouseContent takes it; parties, release and the other entered fields are not compared. A
// house the facts do not name has no changes to list.
func (f OceanFacts) HouseChanges(c Content, shipment string) []string {
	h, ok := f.house(shipment)
	if !ok {
		return nil
	}
	receipt := f.PlaceOfReceipt
	if receipt == "" {
		receipt = f.PortOfLoading
	}
	var out []string
	if at, _ := f.HouseOnBoard(h); !at.Equal(c.OnBoardAt) {
		out = append(out, "the on-board date")
	}
	now, was := f.shippedContainers(h), c.Containers
	if len(now) != len(was) {
		out = append(out, "the containers")
	} else {
		for i := range now {
			if now[i].Number != was[i].Number || now[i].SizeType != was[i].SizeType {
				out = append(out, "the containers")
				break
			}
		}
		for i := range now {
			if now[i].SealNumber != was[i].SealNumber {
				out = append(out, "the seal")
				break
			}
		}
		for i := range now {
			if now[i].Packs != was[i].Packs || now[i].GrossGrams != was[i].GrossGrams || now[i].VolumeCm3 != was[i].VolumeCm3 {
				out = append(out, "the packs, weights or volume")
				break
			}
		}
	}
	if (h.Packs != c.Cargo.Pieces || h.GrossGrams != c.Cargo.GrossGrams || h.VolumeCm3 != c.Cargo.VolumeCm3) && !slices.Contains(out, "the packs, weights or volume") {
		out = append(out, "the packs, weights or volume")
	}
	for _, line := range c.Cargo.Lines {
		if line.GoodsDescription != h.Goods {
			out = append(out, "the goods description")
			break
		}
	}
	for _, line := range c.Cargo.Lines {
		if line.PackageType != h.Package {
			out = append(out, "the package type")
			break
		}
	}
	if f.MovementID != c.Source.MovementID || f.ConsolidationID != c.ConsolidationID {
		out = append(out, "the movement or consolidation")
	}
	if f.MasterBill != c.MasterBill || f.Vessel != c.Vessel || f.Voyage != c.Voyage || f.PortOfLoading != c.PortOfLoading || f.PortOfDischarge != c.PortOfDischarge ||
		receipt != c.PlaceOfReceipt || f.Edition != c.LocationEdition {
		out = append(out, "the voyage, ports or master bill")
	}
	return out
}

// houseNotLoaded is the refusal to issue a house's bill while a container carrying the house has no loading, with the
// ways out; nil when the house is on board. A house the facts do not name is read by the movement's date.
func (f OceanFacts) houseNotLoaded(shipment string) error {
	h, _ := f.house(shipment)
	at, notLoaded := f.HouseOnBoard(h)
	if at.IsZero() && len(notLoaded) == 0 {
		return fmt.Errorf("%w: SEA's facts of version %d state no loading and so no on-board date for house %s: record the loading in SEA, "+
			"by the container's event or the carrier's report, and finalize the movement again, then refresh the draft (or the amendment's draft)", ErrIncomplete, f.Version, h.HBL)
	}
	if at.IsZero() {
		return fmt.Errorf("%w: house %s is not all on board: container %s carrying it has no loading in SEA's facts of version %d; wait for its loading to be recorded and SEA to finalize again, then refresh the draft (or the amendment's draft)", ErrIncomplete, h.HBL, strings.Join(notLoaded, ", "), f.Version)
	}
	return nil
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

// OceanHouseContent builds an HBL or a house sea waybill of one shipment. The number is the one SEA drew. An HBL is
// EXPRESS, or ORIGINALS with one to nine bills. The consignee is the orders': a named party, or the text to order COM
// holds (TO ORDER, or TO ORDER OF a holder), which only an HBL issued as originals carries, with a named notify party
// (C11). A house sea waybill names its consignee and issues no originals. The on-board date is the house's own
// (HouseOnBoard: all of its containers are loaded), stated when the facts state it, and issuing requires it.
func OceanHouseContent(kind Kind, id DocumentID, shipment string, f OceanFacts, issuer Issuer, orders []OrderParties, entered Entered) (Content, error) {
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
	release, originals, err := seaRelease(kind, entered)
	if err != nil {
		return Content{}, err
	}
	receipt := f.PlaceOfReceipt
	if receipt == "" {
		receipt = f.PortOfLoading
	}
	c := Content{Kind: string(kind), DocumentID: string(id), Number: h.HBL, IssuerCapacity: kind.Capacity(), Issuer: issuer,
		IssuePlace: Airport{Code: f.PortOfLoading}, LocationEdition: f.Edition, ReleaseType: release, OriginalCount: originals,
		PaymentIndicator: entered.PaymentIndicator,
		Source:           SourceRef{Contract: "OceanDocFacts v1", MovementID: f.MovementID, Version: f.Version, SHA256: f.SHA256}, ConsolidationID: f.ConsolidationID,
		ShipmentID: shipment, Origin: Airport{Code: receipt}, Destination: Airport{Code: f.PortOfDischarge}, MasterBill: f.MasterBill, Vessel: f.Vessel,
		Voyage: f.Voyage, PlaceOfReceipt: receipt, PortOfLoading: f.PortOfLoading, PortOfDischarge: f.PortOfDischarge,
		HandlingInformation: strings.TrimSpace(entered.HandlingInformation),
		Cargo:               Cargo{Pieces: h.Packs, GrossGrams: h.GrossGrams, VolumeCm3: h.VolumeCm3}}
	c.OnBoardAt, _ = f.HouseOnBoard(h)
	c.Containers = f.shippedContainers(h)
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
		toOrder := o.Consignee != nil && !o.Consignee.Named && !blank(o.Consignee.ToOrder)
		switch {
		case o.Shipper == nil || !o.Shipper.Named:
			return Content{}, fmt.Errorf("%w: order %s names no shipper", ErrIncomplete, orderID)
		case o.Consignee == nil || (!o.Consignee.Named && !toOrder):
			return Content{}, fmt.Errorf("%w: order %s names no consignee", ErrIncomplete, orderID)
		case o.Consignee.ToOrder != "" && !toOrderText(o.Consignee.ToOrder):
			return Content{}, fmt.Errorf("%w: order %s's consignee text is not TO ORDER, or TO ORDER OF a holder, on one line of at most %d characters (C11)",
				ErrIncomplete, orderID, maxText)
		case toOrder && (kind != KindHBL || release != ReleaseOriginals):
			return Content{}, fmt.Errorf("%w: order %s's consignee is %s, which only an HBL issued as originals carries (C11)", ErrIncomplete, orderID,
				o.Consignee.ToOrder)
		case toOrder && (o.Notify == nil || !o.Notify.Named):
			return Content{}, fmt.Errorf("%w: order %s names no notify party for a bill to order (C11)", ErrIncomplete, orderID)
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

// seaRelease is the release an HBL or a house sea waybill is drafted with.
func seaRelease(kind Kind, entered Entered) (string, int, error) {
	switch kind {
	case KindHBL:
		release := entered.Release
		if release == "" {
			release = ReleaseExpress
		}
		switch release {
		case ReleaseExpress:
			if entered.Originals != 0 {
				return "", 0, fmt.Errorf("%w: an EXPRESS bill issues no originals", ErrInvalidChange)
			}
			return release, 0, nil
		case ReleaseOriginals:
			if entered.Originals < 1 || entered.Originals > 9 {
				return "", 0, fmt.Errorf("%w: originals are from 1 to 9", ErrInvalidChange)
			}
			return release, entered.Originals, nil
		default:
			return "", 0, fmt.Errorf("%w: an HBL is EXPRESS or ORIGINALS", ErrInvalidChange)
		}
	case KindHouseSeaWaybill:
		if entered.Release != "" && entered.Release != ReleaseSeaWaybill {
			return "", 0, fmt.Errorf("%w: a house sea waybill is SEA_WAYBILL", ErrInvalidChange)
		}
		if entered.Originals != 0 {
			return "", 0, fmt.Errorf("%w: a house sea waybill issues no originals", ErrInvalidChange)
		}
		return ReleaseSeaWaybill, 0, nil
	default:
		return "", 0, fmt.Errorf("%w: kind %s", ErrInvalidDocument, kind)
	}
}

func port(code string) bool { return len(code) == 5 && code == strings.ToUpper(code) }
