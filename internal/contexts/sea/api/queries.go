package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Movement states: OPEN; FINAL once its facts are finalized for DOC, until a link, a refresh, an end, a new seal,
// a new VGM assertion or the carrier's rejection returns it to OPEN. A container fact or the carrier's acceptance
// leaves it FINAL. CANCELLED.
const (
	StateOpen      = "OPEN"
	StateFinal     = "FINAL"
	StateCancelled = "CANCELLED"

	DocFactsFinal      = "FINAL"
	DocFactsSuperseded = "SUPERSEDED"
)

// Movement kinds, the kinds of PHY's sea LCL consolidations (N1-PHY OD-6): the consolidator's own container booked
// with a shipping line, or LCL space bought from a co-loader.
const (
	KindSeaLCLOwn    = "SEA_LCL_OWN"
	KindSeaLCLCoLoad = "SEA_LCL_COLOAD"
)

// Roles of the movement's carrier, as BOOK froze them from ORG.
const (
	RoleCarrier  = "CARRIER"
	RoleCoLoader = "CO_LOADER"
)

// Master bill kinds and statuses.
const (
	MasterBillCarrierMBL        = "CARRIER_MBL"
	MasterBillCarrierSeaWaybill = "CARRIER_SEA_WAYBILL"
	MasterBillCoLoad            = "COLOAD_BILL"

	MasterBillCurrent    = "CURRENT"
	MasterBillSuperseded = "SUPERSEDED"
	MasterBillVoided     = "VOIDED" // the current reference of a cancelled movement
)

// Entry starts and ends of a house association.
const (
	EntryAssociated        = "ASSOCIATED"
	EntryRefreshed         = "REFRESHED"
	EntryDetached          = "DETACHED"
	EntryTransferred       = "TRANSFERRED"
	EntryShipmentCancelled = "SHIPMENT_CANCELLED"
)

// Bases of a house's quantities: PLANNED from FUL's plan; RECEIVED (the lots' latest measures once every lot
// completed receipt) and TENDERED (what was stuffed or tendered in the consolidation, at the lots' latest measures)
// from PHY's evidence as FUL relays it (decision N1-PHY OD-2, option A).
const (
	BasisPlanned  = "PLANNED"
	BasisReceived = "RECEIVED"
	BasisTendered = "TENDERED"
)

// CutOff is a deadline the carrier set for a voyage leg, as BOOK recorded it: an instant with the IANA time zone of
// the place, and the place when it is not the leg's port of loading.
type CutOff struct {
	Kind     string // BOOK's cut-off kinds of SEA, such as VGM or SHIPPING_INSTRUCTIONS_DOCUMENTS
	At       time.Time
	TimeZone string
	Location kernel.LocationCode
}

// VoyageLeg is a leg of the movement as the carrier confirmed it, from the booking's capacity version; ETD and ETA
// are the carrier's estimates, never facts of departure.
type VoyageLeg struct {
	Leg             int
	Vessel          string
	VesselIMO       string // empty when the carrier gave none
	Voyage          string
	From            kernel.LocationCode
	To              kernel.LocationCode
	ETD             time.Time
	ETA             time.Time
	CutOffs         []CutOff
	BookingID       string
	CapacityVersion int
	Current         bool
}

// MasterBill is a master bill reference of the movement as its carrier or co-loader stated it.
type MasterBill struct {
	BillNo     int
	Number     string
	Kind       string
	Status     string
	Evidence   Evidence
	RecordedBy string
	RecordedAt time.Time
}

// HousePart is a part of a COM obligation the house carries, with its order.
type HousePart struct {
	ObligationID      string
	ObligationVersion int64
	OrderID           string
	Packs             int64
	PackageType       string
	GrossWeight       kernel.Quantity
	Volume            kernel.Quantity
	GoodsDescription  string // the order line's, as FUL relays it from COM; empty for parts FUL planned before it kept one
}

// HouseContainer is a sealed container of the consolidation with the house's packs in it, as PHY verified them and
// FUL relays them: PHY's stuffing (ExecutionID) and planned load unit with its ISO 6346 size-type, the house's
// packs, gross weight and volume in it, PHY's load lines (LineNos, with ExecutionID the PHY evidence ids) and the
// seal PHY recorded. ContainerNumber is the container of the movement's STUFFED visit that keeps this stuffing
// (RecordStuffingDone); empty until then.
type HouseContainer struct {
	ContainerNumber string
	ExecutionID     string
	LoadUnitNo      int
	TypeCode        string
	Packs           int64
	GrossWeight     kernel.Quantity
	Volume          kernel.Quantity
	LineNos         []int
	Seal            string
	SealSHA256      string
	SealedAt        time.Time
}

// House is a house bill association: the FUL shipment, its HBL number and the facts frozen from FUL's plan version
// or from PHY's measures FUL relayed (Basis, with FUL's EvidenceVersion), with its parts as planned and its
// per-container breakdown. Quantities are in the units FUL relays.
type House struct {
	EntryNo         int
	ShipmentID      string
	HBL             string
	FULRevision     int64
	PlanVersion     int
	ContainerMode   string // LCL, or empty when FUL plans none
	Packs           int64
	GrossWeight     kernel.Quantity
	Volume          kernel.Quantity
	Basis           string
	EvidenceVersion int // FUL's evidence version the measures or containers came from; 0 when neither did
	Parts           []HousePart
	Containers      []HouseContainer
	Start           string
	AssociatedAt    time.Time
	WindowDrift     bool // the house's departure window does not contain the first current leg's ETD
}

// OceanMovement is an ocean movement with its voyage legs, master bill references, live houses and totals. Totals
// are given in GRM and CMQ when every live house's weight (volume) is in a metric unit SEA converts (KGM, GRM, TNE;
// MTQ, DMQ, LTR, CMQ), and are empty otherwise; houses without a volume add none.
type OceanMovement struct {
	TenantID               string
	MovementID             string
	Revision               int64
	State                  string
	DocFactsVersion        int // the latest OceanDocFacts version; 0 before the first. Current while State is FINAL.
	Kind                   string
	ConsolidationID        string
	OperatingLegalEntityID string
	CarrierPartyID         string
	CarrierPartyRevision   int64
	CarrierRole            string
	Origin                 kernel.LocationCode
	Destination            kernel.LocationCode
	LocationEdition        string
	BookingID              string
	CapacityVersion        int
	Legs                   []VoyageLeg // current first, then superseded
	MasterBillNumber       string      // the current reference; empty before one is recorded
	MasterBillKind         string
	MasterBills            []MasterBill     // latest first
	Containers             []ContainerVisit // the movement's live container visits, in the order they were assigned
	Houses                 []House          // live
	Packs                  int64
	GrossWeight            kernel.Quantity
	Volume                 kernel.Quantity
}

// HblSeries is a legal entity's HBL numbering: its template, first number and where its sequence stands per period (""
// for a template that never resets). A series whose first number is above 1 stands at the number before it until one
// is taken; a period no number was taken in is not listed.
type HblSeries struct {
	LegalEntityID string
	Template      string
	FirstNumber   int64
	Revision      int64
	Last          []NumberPeriod
}

// NumberPeriod is where a period's sequence stands: the last value taken (or passed over because the number was given
// by hand before), or the series' seed.
type NumberPeriod struct {
	Period string
	Last   int64
}

// Queries.
type (
	GetOceanMovement struct{ TenantID, MovementID string }
	// FindOceanMovementByMasterBill asks which movement holds a number as its current master bill reference of an
	// issuer (the carrier or co-loader).
	FindOceanMovementByMasterBill struct{ TenantID, IssuerPartyID, Number string }
	FindHouseBill                 struct{ TenantID, LegalEntityID, HBL string }
	GetHblSeries                  struct{ TenantID, LegalEntityID string }
)

// HouseBillRef is where an HBL number belongs: its shipment for ever and, while live, the movement.
type HouseBillRef struct {
	LegalEntityID string
	HBL           string
	ShipmentID    string
	MovementID    string // empty when no association is live
	EntryNo       int
}

// Queries is SEA's read side. Unknown movements, master bill references, HBLs and series yield ErrNotFound.
type Queries interface {
	VisitQueries
	GetOceanMovement(ctx context.Context, q GetOceanMovement) (OceanMovement, error)
	FindOceanMovementByMasterBill(ctx context.Context, q FindOceanMovementByMasterBill) (OceanMovement, error)
	FindHouseBill(ctx context.Context, q FindHouseBill) (HouseBillRef, error)
	GetHblSeries(ctx context.Context, q GetHblSeries) (HblSeries, error)
	GetOceanDocFacts(ctx context.Context, q GetOceanDocFacts) (OceanDocFacts, error)
}

// GetOceanDocFacts reads a published version. Version 0 reads the latest. Status is FINAL while that version is
// the latest and the movement is FINAL, and SUPERSEDED otherwise.
type GetOceanDocFacts struct {
	TenantID   string
	MovementID string
	Version    int
}

// OceanDocFacts is OceanDocFacts v1 as published. It has no signatory.
type OceanDocFacts struct {
	Status                 string         `json:"-"`
	Version                int            `json:"version"`
	MovementID             string         `json:"movement_id"`
	ConsolidationID        string         `json:"consolidation_id"`
	Kind                   string         `json:"kind"`
	OperatingLegalEntityID string         `json:"operating_legal_entity_id"`
	CarrierPartyID         string         `json:"carrier_party_id"`
	CarrierPartyRevision   int64          `json:"carrier_party_revision"`
	CarrierRole            string         `json:"carrier_role"`
	MasterBillNumber       string         `json:"master_bill_number"`
	MasterBillKind         string         `json:"master_bill_kind"`
	BookingID              string         `json:"booking_id"`
	CapacityVersion        int            `json:"capacity_version"`
	Vessel                 string         `json:"vessel"`
	VesselIMO              string         `json:"vessel_imo,omitempty"`
	Voyage                 string         `json:"voyage"`
	PlaceOfReceipt         string         `json:"place_of_receipt"`
	PortOfLoading          string         `json:"port_of_loading"`
	PortOfDischarge        string         `json:"port_of_discharge"`
	LocationEdition        string         `json:"location_edition"`
	ETD                    time.Time      `json:"etd"`
	OnBoardAt              time.Time      `json:"on_board_at,omitempty"`
	Containers             []DocContainer `json:"containers"`
	Houses                 []DocHouse     `json:"houses"`
	SHA256                 string         `json:"-"`
	FinalizedBy            string         `json:"-"`
	FinalizedAt            time.Time      `json:"-"`
}

// DocContainer is a container named in OceanDocFacts v1.
type DocContainer struct {
	VisitID                  string `json:"visit_id"`
	Number                   string `json:"number"`
	SizeType                 string `json:"size_type"`
	SealNumber               string `json:"seal_number"`
	SealParty                string `json:"seal_party"`
	VGMAssertionID           string `json:"vgm_assertion_id"`
	VGMGrams                 int64  `json:"vgm_grams"`
	VGMMethod                string `json:"vgm_method"`
	ResponsiblePartyID       string `json:"responsible_party_id"`
	ResponsiblePartyRevision int64  `json:"responsible_party_revision"`
	Packs                    int64  `json:"packs"`
	GrossWeightGrams         int64  `json:"gross_weight_grams"`
	VolumeCubicCentimetres   int64  `json:"volume_cubic_centimetres"`
}

// DocHouse is a live house named in OceanDocFacts v1.
type DocHouse struct {
	ShipmentID             string         `json:"shipment_id"`
	HBL                    string         `json:"hbl"`
	PlanVersion            int            `json:"plan_version"`
	Packs                  int64          `json:"packs"`
	PackageType            string         `json:"package_type,omitempty"`
	GrossWeightGrams       int64          `json:"gross_weight_grams"`
	VolumeCubicCentimetres int64          `json:"volume_cubic_centimetres"`
	OrderIDs               []string       `json:"order_ids"`
	GoodsDescription       string         `json:"goods_description,omitempty"`
	Containers             []DocBreakdown `json:"containers"`
}

// DocBreakdown is a house's packs in one container.
type DocBreakdown struct {
	VisitID                string `json:"visit_id"`
	ContainerNumber        string `json:"container_number"`
	Packs                  int64  `json:"packs"`
	GrossWeightGrams       int64  `json:"gross_weight_grams"`
	VolumeCubicCentimetres int64  `json:"volume_cubic_centimetres"`
}
