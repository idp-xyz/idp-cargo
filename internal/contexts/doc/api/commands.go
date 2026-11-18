// Package api is the published language of DOC (Transport Documents): the house air waybill (HAWB) and the master
// air waybill (MAWB) of an air consolidation, rendered to PDF and dispatched with the P5 records (dispatch.go), and
// the house bill of lading (HBL) and house sea waybill of a sea shipment, built from SEA's OceanDocFacts v1, with
// the originals of an HBL. It imports only the standard library and the kernel.
//
// A document is drafted from AIR's finalized facts (AirDocFacts v1, relationship 11): a HAWB from the house of one
// FUL shipment, with the shipper, consignee and notify party COM froze on the orders of its parts (OrderParties
// v1); a MAWB from the master with every house, with the issuing legal entity as shipper and the destination agent
// (an ORG party in the OVERSEAS_AGENT role) as consignee. Numbers are AIR's: the HAWB AIR drew for the shipment
// (N1-HAWB) and the MAWB from the airline's booking confirmation; an issued number belongs to its shipment (HAWB)
// or movement (MAWB) for ever. Every version is kept as written; it is drafted, approved and issued, and a
// correction after issue is the next version under the same number (an amendment), approved and issued in turn.
// DOC issues only on AIR's latest FINAL facts, under an issuance authority in force (OD-10), with the order
// parties locked in COM for the document (C12) and, for a house, FIN's credit decision or an override (C7, OD-3).
package api

import (
	"context"
	"errors"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Document kinds.
const (
	KindHAWB            = "HAWB"
	KindMAWB            = "MAWB"
	KindHBL             = "HBL"
	KindHouseSeaWaybill = "HOUSE_SEA_WAYBILL"
)

// Payment indicators of the freight.
const (
	PaymentPrepaid = "PREPAID"
	PaymentCollect = "COLLECT"
)

// DraftTransportDocument drafts a document from the latest AirDocFacts version of a movement, which must be FINAL.
// A HAWB names its ShipmentID; a MAWB its AgentPartyID, the destination agent. One document of a kind that is not
// cancelled per shipment (HAWB) or consolidation (MAWB). The issuing legal entity is the movement's operating
// legal entity, VERIFIED in ORG; the issue place is the movement's origin.
//
// An HBL or a house sea waybill is drafted from the latest OceanDocFacts version instead and names its ShipmentID;
// a shipment has one of the two that is not cancelled. Release says how an HBL is released. The consignee is the
// orders': a consignee to order (COM's TO ORDER text) is carried only by an HBL issued as ORIGINALS whose orders
// name a notify party (C11).
type DraftTransportDocument struct {
	CommandID           string
	TenantID            string
	ActorID             string
	DocumentID          string
	Kind                string
	MovementID          string
	ShipmentID          string
	AgentPartyID        string
	PaymentIndicator    string
	HandlingInformation string
	Release             string // an HBL: EXPRESS (also when empty) or ORIGINALS; a house sea waybill: SEA_WAYBILL or empty
	Originals           int    // 1 to 9 when Release is ORIGINALS, otherwise 0
	OccurredAt          time.Time
}

// RefreshDocumentDraft writes the next draft version from AIR's latest facts and COM's or ORG's current parties,
// with a reason; empty entered fields keep those of the latest version. An approved draft needs approval again;
// during an amendment the amendment's draft is replaced and needs approval again. MovementID, for a HAWB, names
// the movement AIR now associates the shipment with (a rollover); empty keeps the document's. An HBL's release and
// number of originals may change: a non-empty Release sets both Release and Originals (EXPRESS with 0 makes an
// ORIGINALS bill EXPRESS, ORIGINALS needs its count); an empty Release with a non-zero Originals changes only the
// number; both empty keep the latest version's. An air waybill has neither.
type RefreshDocumentDraft struct {
	CommandID           string
	TenantID            string
	ActorID             string
	DocumentID          string
	ExpectedRevision    int64
	Reason              string
	PaymentIndicator    string
	HandlingInformation string
	AgentPartyID        string
	MovementID          string
	Release             string // an HBL: EXPRESS or ORIGINALS; a house sea waybill: SEA_WAYBILL; empty keeps the release
	Originals           int    // 1 to 9 when Release is ORIGINALS; with an empty Release, a non-zero number replaces the count
	OccurredAt          time.Time
}

// ApproveTransportDocument approves the draft version, or an amendment's draft, once it is complete for its kind.
// An approver who drafted it must also be allowed ActionApproveOwnTransportDocument, and the approval is flagged
// (OD-7 B).
type ApproveTransportDocument struct {
	CommandID        string
	TenantID         string
	ActorID          string
	DocumentID       string
	ExpectedRevision int64
	OccurredAt       time.Time
}

// IssueTransportDocument issues the approved version. DOC first reads (T3): AIR's latest AirDocFacts version,
// which must be the one drafted and FINAL; for a HAWB, COM's parties of every order, locked for the document and
// unchanged since the draft (the API client locks them first: LockOrderParties); FIN's credit decision for the
// ordering party (FIN is not built: CreditOverrideReason is then required, from an actor allowed
// ActionOverrideCreditCheck). An issuance authority must be in force for the legal entity, kind, issue place and,
// for a MAWB, airline.
type IssueTransportDocument struct {
	CommandID            string
	TenantID             string
	ActorID              string
	DocumentID           string
	ExpectedRevision     int64
	CreditOverrideReason string
	OccurredAt           time.Time
}

// OpenDocumentAmendment drafts the next version of an issued document from AIR's latest facts and the current
// parties, with a reason. The issued version stays valid until the amendment is issued. To change parties, the
// API client releases the document's lock in COM after this command commits, sets the party, then refreshes the
// draft. A HAWB whose shipment was rolled to another movement names it in MovementID: the amendment names the new
// master under the same number. A document issued as originals is amended once every original of the issued
// version is returned (ReturnOriginal); a surrendered document is not amended. Release and Originals change an
// HBL's release and number of originals as RefreshDocumentDraft's do; an amendment to EXPRESS issues no originals.
type OpenDocumentAmendment struct {
	CommandID           string
	TenantID            string
	ActorID             string
	DocumentID          string
	ExpectedRevision    int64
	Reason              string
	PaymentIndicator    string
	HandlingInformation string
	AgentPartyID        string
	MovementID          string
	Release             string // an HBL: EXPRESS or ORIGINALS; a house sea waybill: SEA_WAYBILL; empty keeps the release
	Originals           int    // 1 to 9 when Release is ORIGINALS; with an empty Release, a non-zero number replaces the count
	OccurredAt          time.Time
}

// IssueDocumentAmendment issues the amendment's approved draft under the same number, with the gates of
// IssueTransportDocument; the version it replaces is superseded. A version of originals gets its own set.
type IssueDocumentAmendment struct {
	CommandID            string
	TenantID             string
	ActorID              string
	DocumentID           string
	ExpectedRevision     int64
	CreditOverrideReason string
	OccurredAt           time.Time
}

// AbandonDocumentAmendment discards the amendment's draft with a reason; the issued version stays. A HAWB's order
// parties must be locked for the document again and unchanged since the issued version. The originals returned for
// the amendment are recorded as handed back (ISSUED again), with the reason as evidence.
type AbandonDocumentAmendment struct {
	CommandID        string
	TenantID         string
	ActorID          string
	DocumentID       string
	ExpectedRevision int64
	Reason           string
	OccurredAt       time.Time
}

// ReturnOriginal records that one original of the valid issued version, which is out, is back with the issuer, with
// evidence. The document stays ISSUED.
type ReturnOriginal struct {
	CommandID        string
	TenantID         string
	ActorID          string
	DocumentID       string
	ExpectedRevision int64
	Serial           int
	Evidence         string
	OccurredAt       time.Time
}

// SurrenderOriginals records that every original of the valid issued version was surrendered to the issuer at
// origin (telex release), with evidence. An original returned earlier is surrendered with the rest, whether each is
// out or back with the issuer. The issued version stays valid; the document's state becomes SURRENDERED.
type SurrenderOriginals struct {
	CommandID        string
	TenantID         string
	ActorID          string
	DocumentID       string
	ExpectedRevision int64
	Evidence         string
	OccurredAt       time.Time
}

// CancelTransportDocument cancels a draft, approved, issued, amending or surrendered document with a reason (an
// amendment's draft is discarded). While an original of the valid version is out the cancellation is refused
// (DOC_ORIGINALS_OUT): each is returned first, or all were surrendered. An issued number stays with its shipment or
// movement: a later document of the same shipment may issue it again, another never. The API client then releases
// the document's lock in COM.
type CancelTransportDocument struct {
	CommandID        string
	TenantID         string
	ActorID          string
	DocumentID       string
	ExpectedRevision int64
	Reason           string
	OccurredAt       time.Time
}

// RecordIssuanceAuthority records a legal entity's authority to issue a kind at an issue place (UN/LOCODE),
// with the filing, registration or agency agreement reference, its validity and evidence (OD-10 A). A MAWB
// authority is an airline agency and names the airline's ORG party.
type RecordIssuanceAuthority struct {
	CommandID      string
	TenantID       string
	ActorID        string
	AuthorityID    string
	LegalEntityID  string
	Kind           string
	IssuePlace     string
	AirlinePartyID string
	Qualification  string
	Validity       kernel.ValidityInterval
	EvidenceRef    string
	OccurredAt     time.Time
}

// WithdrawIssuanceAuthority ends an authority from its business time on, with a reason.
type WithdrawIssuanceAuthority struct {
	CommandID        string
	TenantID         string
	ActorID          string
	AuthorityID      string
	ExpectedRevision int64
	Reason           string
	OccurredAt       time.Time
}

// Receipt confirms a committed command; replaying the command returns it again. Version is the document version
// the command wrote or issued; authority commands name the authority in DocumentID's place.
type Receipt struct {
	CommandID   string
	DocumentID  string
	NewRevision int64
	Version     int
}

// Commands is DOC's command entry point.
type Commands interface {
	DraftTransportDocument(ctx context.Context, cmd DraftTransportDocument) (Receipt, error)
	RefreshDocumentDraft(ctx context.Context, cmd RefreshDocumentDraft) (Receipt, error)
	ApproveTransportDocument(ctx context.Context, cmd ApproveTransportDocument) (Receipt, error)
	IssueTransportDocument(ctx context.Context, cmd IssueTransportDocument) (Receipt, error)
	OpenDocumentAmendment(ctx context.Context, cmd OpenDocumentAmendment) (Receipt, error)
	IssueDocumentAmendment(ctx context.Context, cmd IssueDocumentAmendment) (Receipt, error)
	AbandonDocumentAmendment(ctx context.Context, cmd AbandonDocumentAmendment) (Receipt, error)
	ReturnOriginal(ctx context.Context, cmd ReturnOriginal) (Receipt, error)
	SurrenderOriginals(ctx context.Context, cmd SurrenderOriginals) (Receipt, error)
	CancelTransportDocument(ctx context.Context, cmd CancelTransportDocument) (Receipt, error)
	RecordIssuanceAuthority(ctx context.Context, cmd RecordIssuanceAuthority) (Receipt, error)
	WithdrawIssuanceAuthority(ctx context.Context, cmd WithdrawIssuanceAuthority) (Receipt, error)
}

// Actions an Authorizer decides on: the command names, and two more.
const (
	ActionDraftTransportDocument    = "DraftTransportDocument"
	ActionRefreshDocumentDraft      = "RefreshDocumentDraft"
	ActionApproveTransportDocument  = "ApproveTransportDocument"
	ActionIssueTransportDocument    = "IssueTransportDocument"
	ActionOpenDocumentAmendment     = "OpenDocumentAmendment"
	ActionIssueDocumentAmendment    = "IssueDocumentAmendment"
	ActionAbandonDocumentAmendment  = "AbandonDocumentAmendment"
	ActionReturnOriginal            = "ReturnOriginal"
	ActionSurrenderOriginals        = "SurrenderOriginals"
	ActionCancelTransportDocument   = "CancelTransportDocument"
	ActionRecordIssuanceAuthority   = "RecordIssuanceAuthority" // subject: the authority
	ActionWithdrawIssuanceAuthority = "WithdrawIssuanceAuthority"
	// ActionApproveOwnTransportDocument lets a designated role approve a draft it wrote (OD-7 B).
	ActionApproveOwnTransportDocument = "ApproveOwnTransportDocument"
	// ActionOverrideCreditCheck lets an actor issue a house document without FIN's credit decision (OD-3 B).
	ActionOverrideCreditCheck = "OverrideCreditCheck"
)

// Authorizer decides whether a verified actor may perform an action on a document or authority.
type Authorizer interface {
	AuthorizeDocAction(ctx context.Context, tenantID, actorID, action, subjectID string) error
}

// Error kinds returned by Commands and Queries, wrapped around the original cause.
var (
	ErrUnauthenticated     = errors.New("doc: unauthenticated")
	ErrUnauthorized        = errors.New("doc: unauthorized")
	ErrNotFound            = errors.New("doc: not found")
	ErrConflict            = errors.New("doc: conflicts with the stored state")
	ErrIdempotencyConflict = errors.New("doc: command id reused with a different payload")
	ErrRejected            = errors.New("doc: rejected by business rules")
)
