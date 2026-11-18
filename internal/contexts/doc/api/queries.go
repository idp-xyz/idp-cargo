package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Document states. SURRENDERED is an issued document whose originals were all surrendered at origin (telex
// release): its issued version stays the valid one.
const (
	StateDraft       = "DRAFT"
	StateApproved    = "APPROVED"
	StateIssued      = "ISSUED"
	StateAmending    = "AMENDING"
	StateSurrendered = "SURRENDERED"
	StateCancelled   = "CANCELLED"
)

// Release types of a version: an air waybill is CONSIGNEE_NAMED; an HBL is EXPRESS (no originals, released to the
// named consignee) or ORIGINALS (one to nine original bills); a house sea waybill is SEA_WAYBILL.
const (
	ReleaseConsigneeNamed = "CONSIGNEE_NAMED"
	ReleaseExpress        = "EXPRESS"
	ReleaseOriginals      = "ORIGINALS"
	ReleaseSeaWaybill     = "SEA_WAYBILL"
)

// Version statuses: DRAFT or APPROVED while in progress; ISSUED for the valid version; SUPERSEDED once an
// amendment was issued; CANCELLED for the version valid when the document was cancelled; DISCARDED for a draft
// replaced by a refresh, an abandoned amendment or one cancelled with the document.
const (
	VersionDraft      = "DRAFT"
	VersionApproved   = "APPROVED"
	VersionIssued     = "ISSUED"
	VersionSuperseded = "SUPERSEDED"
	VersionCancelled  = "CANCELLED"
	VersionDiscarded  = "DISCARDED"
)

// Credit bases of an issue (C7).
const (
	CreditOK          = "FIN_OK"
	CreditOverride    = "OVERRIDE"
	CreditNotRequired = "NOT_REQUIRED"
)

// VersionSummary is one version of a document with how it went.
type VersionSummary struct {
	Version              int
	Status               string
	SHA256               string
	SourceFactsVersion   int // the AirDocFacts version it was built on
	Reason               string
	DraftedBy            string
	DraftedAt            time.Time
	ApprovedBy           string
	ApprovedAt           time.Time
	SelfApproved         bool
	IssuedBy             string
	IssuedAt             time.Time
	AuthorityID          string
	CreditBasis          string
	CreditOverrideReason string
	DiscardReason        string
}

// TransportDocument is a document with its versions. CurrentVersion is the valid issued version (0 before the
// first issue); LatestVersion the draft in progress or the issued one.
type TransportDocument struct {
	TenantID        string
	DocumentID      string
	Revision        int64
	Kind            string
	State           string
	Number          string
	MovementID      string
	ConsolidationID string
	ShipmentID      string
	LegalEntityID   string
	CurrentVersion  int
	LatestVersion   int
	Versions        []VersionSummary
	CancelReason    string
	CancelledAt     time.Time
}

// DocumentVersion is a version with its content: Content is the JSON exactly as DOC wrote it, SHA256 its digest,
// and Doc the content read from it (TransportDocumentContent v1).
type DocumentVersion struct {
	DocumentID string
	Version    int
	Status     string
	Content    []byte
	SHA256     string
	Doc        DocumentContent
}

// TransportDocumentContent v1. Weights are in grams and volumes in cubic centimetres. An air waybill's airports carry
// their UN/LOCODE and the IATA code frozen in AIR's facts; an HBL and a house sea waybill carry the ports, the
// vessel, the master bill and the on-board date from OceanDocFacts, and an HBL issued as originals their number.
// Parties carry no contacts; the consignee of a bill to order is the order's text (Party.ToOrder).
type DocumentContent struct {
	Kind                string             `json:"kind"`
	DocumentID          string             `json:"document_id"`
	Number              string             `json:"number"`
	MAWB                string             `json:"mawb,omitempty"`
	IssuerCapacity      string             `json:"issuer_capacity"` // CONSOLIDATOR, AGENT_OF_CARRIER or NVOCC_CARRIER
	Issuer              Issuer             `json:"issuer"`
	IssuePlace          Airport            `json:"issue_place"`
	LocationEdition     string             `json:"location_edition"`
	ReleaseType         string             `json:"release_type"` // CONSIGNEE_NAMED, EXPRESS, ORIGINALS or SEA_WAYBILL
	PaymentIndicator    string             `json:"payment_indicator"`
	Source              SourceRef          `json:"source"`
	ConsolidationID     string             `json:"consolidation_id"`
	ShipmentID          string             `json:"shipment_id,omitempty"`
	Airline             Airline            `json:"airline"`
	Origin              Airport            `json:"origin"`
	Destination         Airport            `json:"destination"`
	Flights             []Flight           `json:"flights,omitempty"`
	Shipper             Party              `json:"shipper"`
	Consignee           Party              `json:"consignee"`
	Notify              *Party             `json:"notify_party,omitempty"`
	Orders              []OrderRef         `json:"orders,omitempty"`
	Cargo               Cargo              `json:"cargo"`
	Security            Security           `json:"security"`
	HandlingInformation string             `json:"handling_information,omitempty"`
	MasterBill          string             `json:"master_bill_number,omitempty"`
	Vessel              string             `json:"vessel,omitempty"`
	Voyage              string             `json:"voyage,omitempty"`
	PlaceOfReceipt      string             `json:"place_of_receipt,omitempty"`
	PortOfLoading       string             `json:"port_of_loading,omitempty"`
	PortOfDischarge     string             `json:"port_of_discharge,omitempty"`
	OnBoardAt           time.Time          `json:"on_board_at,omitzero"`
	Containers          []ShippedContainer `json:"containers,omitempty"`
	Originals           int                `json:"originals,omitempty"`
}

// ShippedContainer is one container of an HBL, as the published facts stated it.
type ShippedContainer struct {
	Number     string `json:"number"`
	SizeType   string `json:"size_type,omitempty"`
	SealNumber string `json:"seal_number"`
	Packs      int64  `json:"packs"`
	GrossGrams int64  `json:"gross_weight_grams"`
	VolumeCm3  int64  `json:"volume_cubic_centimetres"`
}

type Issuer struct {
	LegalEntityID string `json:"legal_entity_id"`
	Revision      int64  `json:"revision"`
	Name          string `json:"name"`
	Jurisdiction  string `json:"jurisdiction"`
}

type Airport struct {
	Code string `json:"code"`
	IATA string `json:"iata"`
}

type SourceRef struct {
	Contract   string `json:"contract"`
	MovementID string `json:"movement_id"`
	Version    int    `json:"version"`
	SHA256     string `json:"sha256"`
}

type Airline struct {
	PartyID       string `json:"party_id"`
	PartyRevision int64  `json:"party_revision"`
	Designator    string `json:"designator"`
	AWBPrefix     string `json:"awb_prefix"`
}

type Flight struct {
	Flight string    `json:"flight"`
	From   Airport   `json:"from"`
	To     Airport   `json:"to"`
	ETD    time.Time `json:"etd"`
	ETA    time.Time `json:"eta,omitzero"`
}

type Identifier struct {
	Type    string `json:"type"`
	Country string `json:"country"`
	Value   string `json:"value,omitempty"`
	None    bool   `json:"none,omitempty"`
}

type Address struct {
	Source       string   `json:"source"`
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

type Party struct {
	PartyID       string       `json:"party_id"`
	PartyRevision int64        `json:"party_revision"`
	LegalName     string       `json:"legal_name"`
	EnglishName   string       `json:"english_name,omitempty"`
	CountryCode   string       `json:"country_code"`
	Identifiers   []Identifier `json:"identifiers,omitempty"`
	Address       *Address     `json:"address,omitempty"`
	Overridden    bool         `json:"overridden,omitempty"`
	ToOrder       string       `json:"to_order,omitempty"` // the consignee of a bill to order: the order's text, no party
}

type Transport struct {
	Mode     string `json:"mode,omitempty"`
	Movement string `json:"movement,omitempty"`
}

type OrderRef struct {
	OrderID          string    `json:"order_id"`
	Revision         int64     `json:"revision"`
	Transport        Transport `json:"transport"`
	ShipperVersion   int64     `json:"shipper_version"`
	ConsigneeVersion int64     `json:"consignee_version"`
	NotifyVersion    int64     `json:"notify_version,omitempty"`
}

type CargoLine struct {
	OrderID          string `json:"order_id,omitempty"`
	HAWB             string `json:"hawb,omitempty"`
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

type Screening struct {
	LotID      string    `json:"lot_id"`
	Outcome    string    `json:"outcome"`
	Screener   string    `json:"screener"`
	Method     string    `json:"method"`
	Reference  string    `json:"reference"`
	ScreenedAt time.Time `json:"screened_at"`
}

type Security struct {
	Status     string      `json:"status"`
	Screenings []Screening `json:"screenings,omitempty"`
}

// DocumentSummary is a document in a list.
type DocumentSummary struct {
	DocumentID     string
	Kind           string
	State          string
	Number         string
	Revision       int64
	CurrentVersion int
	LatestVersion  int
}

// IssuanceAuthority is a legal entity's authority to issue a kind at a place (OD-10 A).
type IssuanceAuthority struct {
	AuthorityID    string
	Revision       int64
	LegalEntityID  string
	Kind           string
	IssuePlace     string
	AirlinePartyID string
	Qualification  string
	Validity       kernel.ValidityInterval
	EvidenceRef    string
	RecordedBy     string
	RecordedAt     time.Time
	WithdrawnBy    string
	WithdrawnAt    time.Time
	WithdrawReason string
}

// Queries.
type (
	GetTransportDocument struct{ TenantID, DocumentID string }
	// GetDocumentVersion reads a version; Version 0 reads the valid issued one, or the latest before any issue.
	GetDocumentVersion struct {
		TenantID   string
		DocumentID string
		Version    int
	}
	ListDocumentsForShipment      struct{ TenantID, ShipmentID string }
	ListDocumentsForConsolidation struct{ TenantID, ConsolidationID string }
	GetIssuanceAuthority          struct{ TenantID, AuthorityID string }
)

// Queries is DOC's read side. Unknown documents, versions and authorities yield ErrNotFound.
type Queries interface {
	GetTransportDocument(ctx context.Context, q GetTransportDocument) (TransportDocument, error)
	GetDocumentVersion(ctx context.Context, q GetDocumentVersion) (DocumentVersion, error)
	ListDocumentsForShipment(ctx context.Context, q ListDocumentsForShipment) ([]DocumentSummary, error)
	ListDocumentsForConsolidation(ctx context.Context, q ListDocumentsForConsolidation) ([]DocumentSummary, error)
	GetIssuanceAuthority(ctx context.Context, q GetIssuanceAuthority) (IssuanceAuthority, error)
	ListOriginals(ctx context.Context, q ListOriginals) ([]Original, error)
}

// Statuses of an original bill.
const (
	OriginalIssued      = "ISSUED"                // out, with its holder
	OriginalReturned    = "RETURNED"              // back with the issuer, for an amendment or a cancellation
	OriginalSurrendered = "SURRENDERED_AT_ORIGIN" // surrendered with the whole set at origin (telex release)
)

// Original is one record of an original bill of an issued version. A serial's records are numbered from 1; the one
// with the highest number is the original's status. The first is its issue; later ones carry evidence.
type Original struct {
	Version  int
	Serial   int
	Record   int
	Status   string
	By       string
	At       time.Time
	Evidence string
}

// ListOriginals reads the original records of a document, by version, serial and record number. A document
// without originals has none.
type ListOriginals struct {
	TenantID   string
	DocumentID string
}
