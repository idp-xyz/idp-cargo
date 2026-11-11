package api

import (
	"context"
	"time"
)

// DOC V0.2: rendering and DocumentDispatch (decisions N1-DOC OD-4 A, OD-5 A; privacy decision P5).
//
// A version renders to PDF under a template (TemplateAWB): the same version, status and contacts give the same
// bytes, and DOC keeps no PDF. Every copy that leaves the tenant is a dispatch to one ORG party over one channel:
// DOC decides each contact of the document's parties for the recipient and records the decision with identifiers
// only (never a name, phone or e-mail), with the copy's SHA-256. The ordering party gets every contact (it gave
// them: NOT_PROVIDED); another party in mainland China only those consenting to be provided to others (PROVISION);
// a party abroad, Hong Kong, Macao and Taiwan included, only those also consenting to go abroad (CROSS_BORDER), and
// only while ORG's ContactTransferRegister v1 is not blocked. A contact without the consent needed is left off the
// copy, with the reason.
//
// The API client sends the copy (T6): it prepares the dispatch, reserves the dispatch's cross-border contact keys in
// ORG (ReserveContactTransfers, keys from GetDispatch), fetches the copy (GetDispatchCopy), sends it, and records the
// outcome. DOC records SENT or UNKNOWN only when ORG holds the reservation; after FAILED the client releases it.

// TemplateAWB is the air waybill layout of V0.2.
const TemplateAWB = "IDP-AWB-V1"

// Recipient roles.
const (
	RecipientOrderingParty    = "ORDERING_PARTY"
	RecipientShipper          = "SHIPPER"
	RecipientConsignee        = "CONSIGNEE"
	RecipientNotifyParty      = "NOTIFY_PARTY"
	RecipientDestinationAgent = "DESTINATION_AGENT"
	RecipientCarrier          = "CARRIER"
	RecipientCustomsBroker    = "CUSTOMS_BROKER"
)

// Channels.
const (
	ChannelEmailPDF      = "EMAIL_PDF"
	ChannelPrintHandover = "PRINT_HANDOVER"
	ChannelPortal        = "PORTAL"
)

// Dispatch states and outcomes: PREPARED until an outcome; SENT and FAILED are final; UNKNOWN (the channel did
// not say) is followed by SENT or FAILED.
const (
	DispatchPrepared = "PREPARED"
	DispatchSent     = "SENT"
	DispatchFailed   = "FAILED"
	DispatchUnknown  = "UNKNOWN"
)

// Contact classifications, legal paths, statuses and reasons for leaving a contact off.
const (
	ContactNotProvided = "NOT_PROVIDED"
	ContactProvision   = "PROVISION"
	ContactCrossBorder = "CROSS_BORDER"

	PathNone             = "NONE"
	PathConsent          = "CONSENT"
	PathThresholdExempt  = "CONSENT_WITH_THRESHOLD_EXEMPTION"
	PathStandardContract = "STANDARD_CONTRACT"
	PathCertification    = "CERTIFICATION"

	ContactIncluded = "INCLUDED"
	ContactOmitted  = "OMITTED"

	OmitNoConsent         = "NO_CONSENT_RECORDED"
	OmitNoProvideConsent  = "NO_CONSENT_TO_PROVIDE"
	OmitNoTransferConsent = "NO_CONSENT_TO_TRANSFER"
	OmitErased            = "ERASED"
)

// Actions an Authorizer decides on for dispatches; the subject is the dispatch, or the tenant for
// ActionReadProvisionRecords.
const (
	ActionPrepareDocumentDispatch = "PrepareDocumentDispatch"
	ActionRecordDispatchOutcome   = "RecordDispatchOutcome"
	// ActionReadProvisionRecords lets an actor list the provision and cross-border records of a period (privacy
	// audit and the tenant's impact assessment).
	ActionReadProvisionRecords = "ReadProvisionRecords"
)

// PrepareDocumentDispatch prepares a copy of a version for an ORG party. Version 0 is the valid issued version, or
// the latest before any issue. A version that is not issued goes to the ordering party only, for approval, marked as
// not valid. DOC first reads
// (T3) the recipient's registration country from ORG, the contacts of a house's first order with their consent from
// COM (purpose DOCUMENTS, with the actor's own authorization for COM's ReadOrderContacts) and ORG's register.
// Refused (ErrRejected) with DOC_TRANSFER_BLOCKED when a contact would go abroad while the register is blocked.
type PrepareDocumentDispatch struct {
	CommandID      string
	TenantID       string
	ActorID        string
	DispatchID     string
	DocumentID     string
	Version        int
	RecipientParty string
	RecipientRole  string
	Channel        string
	OccurredAt     time.Time
}

// RecordDispatchOutcome records what the channel reported, with its reference or evidence (none for UNKNOWN). SENT
// and UNKNOWN are refused (ErrRejected, DOC_TRANSFER_NOT_RESERVED) unless ORG's register holds the dispatch's
// cross-border contact keys.
type RecordDispatchOutcome struct {
	CommandID        string
	TenantID         string
	ActorID          string
	DispatchID       string
	ExpectedRevision int64
	Outcome          string
	Reference        string
	OccurredAt       time.Time
}

// DispatchCommands is DOC's command entry point for dispatches; receipts name the dispatch in DocumentID's place.
type DispatchCommands interface {
	PrepareDocumentDispatch(ctx context.Context, cmd PrepareDocumentDispatch) (Receipt, error)
	RecordDispatchOutcome(ctx context.Context, cmd RecordDispatchOutcome) (Receipt, error)
}

// RenderDocumentVersion renders a version to PDF without contacts (a preview); Version 0 is the valid issued
// version, or the latest before any issue.
type RenderDocumentVersion struct {
	TenantID   string
	DocumentID string
	Version    int
}

// RenderedDocument is a PDF with its SHA-256, template and the version and status it shows.
type RenderedDocument struct {
	DocumentID string
	Version    int
	Status     string
	Template   string
	PDF        []byte
	SHA256     string
}

// GetDispatchCopy renders a dispatch's copy again, with the contacts as COM holds them now; refused (ErrRejected,
// DOC_COPY_CHANGED) when the copy would differ from the one recorded, such as after a contact was erased. ActorID
// reads the contacts from COM.
type GetDispatchCopy struct {
	TenantID   string
	ActorID    string
	DispatchID string
}

// Recipient is a dispatch's recipient: an ORG party with its registration country.
type Recipient struct {
	PartyID string
	Country string
	Role    string
}

// ContactConsent is a contact's consent as recorded with the decision.
type ContactConsent struct {
	NoticeVersion   string
	ProvideToOthers bool
	CrossBorder     bool
	Basis           string
	EvidenceRef     string
	ORGRevision     int64
}

// DispatchContact is the provision record of one contact: its key (ORG:<contact id> or
// COM:<order>:<role>:<party version>), the party role, the classification, the legal path and whether it was on the
// copy, never the contact's details.
type DispatchContact struct {
	No             int
	Key            string
	PartyRole      string
	OrderID        string
	Classification string
	LegalPath      string
	Status         string
	OmitReason     string
	Consent        *ContactConsent
}

// DispatchOutcome is an outcome recorded.
type DispatchOutcome struct {
	No         int
	Outcome    string
	Reference  string
	RecordedBy string
	RecordedAt time.Time
}

// DocumentDispatch is a dispatch with its provision records. CrossBorderKeys are the keys the API client reserves in
// ORG before sending.
type DocumentDispatch struct {
	TenantID        string
	DispatchID      string
	Revision        int64
	DocumentID      string
	Version         int
	Kind            string
	VersionStatus   string
	Recipient       Recipient
	OrderingParty   string
	Channel         string
	State           string
	CopySHA256      string
	Template        string
	RegisterYear    int
	RegisterState   string
	PreparedBy      string
	PreparedAt      time.Time
	Contacts        []DispatchContact
	CrossBorderKeys []string
	Outcomes        []DispatchOutcome
}

// DispatchCopy is a dispatch's copy.
type DispatchCopy struct {
	Dispatch DocumentDispatch
	PDF      []byte
}

// Dispatch queries.
type (
	GetDispatch            struct{ TenantID, DispatchID string }
	ListDocumentDispatches struct{ TenantID, DocumentID string }
	// ListContactProvisions lists the dispatches prepared in [From, Until) with their provision records, for an actor
	// allowed ActionReadProvisionRecords.
	ListContactProvisions struct {
		TenantID string
		ActorID  string
		From     time.Time
		Until    time.Time
	}
)

// DispatchQueries is the read side of rendering and dispatches.
type DispatchQueries interface {
	RenderDocumentVersion(ctx context.Context, q RenderDocumentVersion) (RenderedDocument, error)
	GetDispatchCopy(ctx context.Context, q GetDispatchCopy) (DispatchCopy, error)
	GetDispatch(ctx context.Context, q GetDispatch) (DocumentDispatch, error)
	ListDocumentDispatches(ctx context.Context, q ListDocumentDispatches) ([]DocumentDispatch, error)
	ListContactProvisions(ctx context.Context, q ListContactProvisions) ([]DocumentDispatch, error)
}
