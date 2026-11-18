// DOC V0.4: the carrier's own documents (candidate 24, RECORD_CARRIER_DOCUMENT).
//
// A recorded carrier document is the carrier's master bill of lading (MBL), or its master sea waybill, as the carrier
// issued it: DOC records it and its verification, it does not issue, render or dispatch it. It has no drafts, versions,
// approvals, issue gates, rendering or dispatch, and no original of it is tracked one by one: DOC records the release
// (the carrier's originals and their number, or the telex or express release, or a sea waybill) and nothing of
// what becomes of the originals. When a co-loader carries the movement, the co-loader's bill is recorded the same way,
// as an MBL whose issuer capacity is CO_LOADER.
//
// The kind, the issuer capacity, the carrier, the number, the vessel and the ports come from SEA's OceanDocFacts v1 of
// the movement, which must be FINAL; the person recording enters the release, the number of originals, the issue place
// (a UN/LOCODE) and date, and refers to the evidence of the carrier's issuance: its SHA-256, where it is kept and when
// it was captured. DOC keeps that reference, not the file, and does not check it. DOC records that a second person
// verified the record; a person does not verify a record they recorded themselves. A recorded document is cancelled,
// not amended: a corrected one is cancelled and recorded again. A movement has one document that is not cancelled,
// and for a tenant and a carrier a number is on at most one document that is not cancelled: a cancelled record frees
// the number.
package api

import (
	"context"
	"time"
)

// Kinds of a carrier document; the commands of the documents DOC issues refuse them.
const (
	KindMBL              = "MBL"
	KindMasterSeaWaybill = "MASTER_SEA_WAYBILL"
)

// Issuer capacities of a carrier document: the carrier itself, or the co-loader on a co-load movement.
const (
	CapacityCarrier  = "CARRIER"
	CapacityCoLoader = "CO_LOADER"
)

// States of a carrier document: ISSUED is a document recorded as the carrier issued it, verified or not.
const (
	CarrierIssued    = "ISSUED"
	CarrierCancelled = "CANCELLED"
)

// Actions an Authorizer decides on for carrier documents; the subject is the document.
const (
	ActionRecordCarrierDocument = "RecordCarrierDocument"
	ActionVerifyCarrierDocument = "VerifyCarrierDocument"
	ActionCancelCarrierDocument = "CancelCarrierDocument"
)

// Events of a carrier document have the document as subject and name its kind, movement and consolidation, as the
// other DOC events do.
const (
	CarrierDocumentRecorded  = "CarrierDocumentRecorded.v1"
	CarrierDocumentVerified  = "CarrierDocumentVerified.v1"
	CarrierDocumentCancelled = "CarrierDocumentCancelled.v1"
)

// CarrierEvidence refers to the evidence of the carrier's issuance: the SHA-256 of the carrier's document (64
// lower-case hexadecimal characters), a storage id of 1 to 200 characters, a media type of 1 to 100 characters and
// the time it was captured, which is not after the record.
type CarrierEvidence struct {
	SHA256     string
	StorageID  string
	MediaType  string
	CapturedAt time.Time
}

// RecordCarrierDocument records the carrier's bill of a movement from the latest OceanDocFacts of the movement, which
// must be FINAL (DOC_SOURCE_NOT_FINAL). Release is ORIGINALS with Originals from 1 to 9, or EXPRESS with none, for an
// MBL; SEA_WAYBILL, or empty, with none for a master sea waybill (DOC_INCOMPLETE). IssuePlace is a UN/LOCODE and
// IssuedOn the carrier's date of issue, YYYY-MM-DD, not after tomorrow in China (ErrRejected). A number that is
// on another document of the carrier that is not cancelled is refused (DOC_NUMBER_ISSUED); a movement with a document
// that is not cancelled is a conflict. The receipt names the document in DocumentID.
type RecordCarrierDocument struct {
	CommandID  string
	TenantID   string
	ActorID    string
	DocumentID string
	MovementID string
	Release    string
	Originals  int
	IssuePlace string
	IssuedOn   string
	Evidence   CarrierEvidence
	OccurredAt time.Time
}

// VerifyCarrierDocument records that the actor, a second person, verified the record. The actor is not the person
// who recorded it (ErrRejected, DOC_SELF_VERIFICATION), and a document is verified once.
type VerifyCarrierDocument struct {
	CommandID        string
	TenantID         string
	ActorID          string
	DocumentID       string
	ExpectedRevision int64
	OccurredAt       time.Time
}

// CancelCarrierDocument cancels a recorded document, verified or not, with a reason of 1 to 500 characters. The
// number stays with the movement it was recorded for.
type CancelCarrierDocument struct {
	CommandID        string
	TenantID         string
	ActorID          string
	DocumentID       string
	ExpectedRevision int64
	Reason           string
	OccurredAt       time.Time
}

// CarrierCommands is DOC's command entry point for the carrier's documents.
type CarrierCommands interface {
	RecordCarrierDocument(ctx context.Context, cmd RecordCarrierDocument) (Receipt, error)
	VerifyCarrierDocument(ctx context.Context, cmd VerifyCarrierDocument) (Receipt, error)
	CancelCarrierDocument(ctx context.Context, cmd CancelCarrierDocument) (Receipt, error)
}

// CarrierDocument is a recorded carrier document. Content is the JSON that was recorded, byte for byte, of which
// ContentSHA256 is the digest. Like the other DOC queries, the reads carry no actor.
type CarrierDocument struct {
	DocumentID           string
	Kind                 string
	Number               string
	MovementID           string
	ConsolidationID      string
	LegalEntityID        string
	CarrierPartyID       string
	CarrierPartyRevision int64
	CarrierRole          string
	IssuerCapacity       string
	Release              string
	Originals            int
	IssuePlace           string
	IssuedOn             string
	Evidence             CarrierEvidence
	SourceVersion        int
	SourceSHA256         string
	Content              []byte
	ContentSHA256        string
	State                string
	RecordedBy           string
	RecordedAt           time.Time
	Verified             bool
	VerifiedBy           string
	VerifiedAt           time.Time
	CancelReason         string
	CancelledBy          string
	CancelledAt          time.Time
	Revision             int64
}

// Carrier document queries.
type (
	GetCarrierDocument struct{ TenantID, DocumentID string }
	// FindCarrierDocumentByMovement reads the movement's document that is not cancelled; none is ErrNotFound.
	FindCarrierDocumentByMovement struct{ TenantID, MovementID string }
)

// CarrierQueries is the read side of the carrier's documents.
type CarrierQueries interface {
	GetCarrierDocument(ctx context.Context, q GetCarrierDocument) (CarrierDocument, error)
	FindCarrierDocumentByMovement(ctx context.Context, q FindCarrierDocumentByMovement) (CarrierDocument, error)
}
