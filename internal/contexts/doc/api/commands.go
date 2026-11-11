// Package api is the published language of DOC (Transport Documents) V0.1, the N1 air line: the house air
// waybill (HAWB) and the master air waybill (MAWB) of an air consolidation. It imports only the standard library
// and the kernel.
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
	KindHAWB = "HAWB"
	KindMAWB = "MAWB"
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
	OccurredAt          time.Time
}

// RefreshDocumentDraft writes the next draft version from AIR's latest facts and COM's or ORG's current parties,
// with a reason; empty entered fields keep those of the latest version. An approved draft needs approval again;
// during an amendment the amendment's draft is replaced and needs approval again. MovementID, for a HAWB, names
// the movement AIR now associates the shipment with (a rollover); empty keeps the document's.
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
// master under the same number.
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
	OccurredAt          time.Time
}

// IssueDocumentAmendment issues the amendment's approved draft under the same number, with the gates of
// IssueTransportDocument; the version it replaces is superseded.
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
// parties must be locked for the document again and unchanged since the issued version.
type AbandonDocumentAmendment struct {
	CommandID        string
	TenantID         string
	ActorID          string
	DocumentID       string
	ExpectedRevision int64
	Reason           string
	OccurredAt       time.Time
}

// CancelTransportDocument cancels a draft, approved, issued or amending document with a reason (an amendment's
// draft is discarded). An issued number stays with its shipment or movement: a later document of the same shipment
// may issue it again, another never. The API client then releases the document's lock in COM.
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
