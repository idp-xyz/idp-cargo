package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// DOC V0.4, CarrierDocument (candidate 24, RECORD_CARRIER_DOCUMENT): the carrier's own master bill of lading or master
// sea waybill, which the carrier issues and DOC only records. A recorded document has no drafts, versions, approvals,
// originals, rendering or dispatch; it is one immutable content, recorded by one person and verified by another.

var ErrSelfVerification = errors.New("doc: DOC_SELF_VERIFICATION: a carrier document is verified by a second person, not by the one who recorded it")

// Kinds of a carrier document. They are not kinds DOC issues: Kind.valid and the rest of Kind's methods know the four
// kinds of the forwarder's own documents only, so the commands of those documents refuse these two.
const (
	KindMBL              Kind = "MBL"
	KindMasterSeaWaybill Kind = "MASTER_SEA_WAYBILL"
)

// Issuer capacities of a carrier document: the ocean carrier, or the co-loader on a co-load movement.
const (
	CapacityCarrier  = "CARRIER"
	CapacityCoLoader = "CO_LOADER"
)

// Master bill kinds and carrier roles as SEA publishes them in OceanDocFacts v1.
const (
	masterBillCarrierMBL        = "CARRIER_MBL"
	masterBillCarrierSeaWaybill = "CARRIER_SEA_WAYBILL"
	masterBillCoLoad            = "COLOAD_BILL"
	carrierRoleCarrier          = "CARRIER"
	carrierRoleCoLoader         = "CO_LOADER"
)

// States of a carrier document: ISSUED is a document recorded as the carrier issued it, verified or not.
type CarrierState string

const (
	CarrierIssued    CarrierState = "ISSUED"
	CarrierCancelled CarrierState = "CANCELLED"
)

const (
	eventCarrierRecorded  = "CarrierDocumentRecorded.v1"
	eventCarrierVerified  = "CarrierDocumentVerified.v1"
	eventCarrierCancelled = "CarrierDocumentCancelled.v1"
)

var unlocode = regexp.MustCompile(`^[A-Z]{2}[A-Z2-9]{3}$`)
var hexDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// CarrierEvidence refers to the evidence of the carrier's issuance: the digest of the document as the carrier issued
// it, where it is kept and its media type, and when it was captured. DOC keeps the reference, not the file.
type CarrierEvidence struct {
	SHA256     string    `json:"sha256"`
	StorageID  string    `json:"storage_id"`
	MediaType  string    `json:"media_type"`
	CapturedAt time.Time `json:"captured_at"`
}

// CarrierEntered is what the person recording a carrier document enters; the rest comes from SEA's facts.
type CarrierEntered struct {
	Release    string // ORIGINALS or EXPRESS for an MBL; SEA_WAYBILL, or empty, for a master sea waybill
	Originals  int    // 1 to 9 for ORIGINALS, otherwise 0
	IssuePlace string // a UN/LOCODE
	IssuedOn   string // the carrier's date of issue, YYYY-MM-DD
	Evidence   CarrierEvidence
}

// CarrierRef is the carrier (or the co-loader) as ORG knew its party when SEA published the facts.
type CarrierRef struct {
	PartyID       string `json:"party_id"`
	PartyRevision int64  `json:"party_revision"`
	Role          string `json:"role"`
}

// CarrierContent is the content of a recorded carrier document, in the order it is written. It names no person and
// no party: the carrier and the legal entity are identifiers.
type CarrierContent struct {
	Kind            string          `json:"kind"`
	DocumentID      string          `json:"document_id"`
	Number          string          `json:"number"`
	IssuerCapacity  string          `json:"issuer_capacity"`
	Carrier         CarrierRef      `json:"carrier"`
	LegalEntityID   string          `json:"legal_entity_id"`
	ReleaseType     string          `json:"release_type"`
	Originals       int             `json:"originals,omitempty"`
	IssuePlace      string          `json:"issue_place"`
	IssuedOn        string          `json:"issued_on"`
	Source          SourceRef       `json:"source"`
	ConsolidationID string          `json:"consolidation_id"`
	Vessel          string          `json:"vessel"`
	Voyage          string          `json:"voyage"`
	PlaceOfReceipt  string          `json:"place_of_receipt,omitempty"`
	PortOfLoading   string          `json:"port_of_loading"`
	PortOfDischarge string          `json:"port_of_discharge"`
	OnBoardAt       time.Time       `json:"on_board_at,omitzero"`
	Evidence        CarrierEvidence `json:"evidence"`
}

// CarrierDocumentState is a stored carrier document, for persistence.
type CarrierDocumentState struct {
	TenantID        TenantID
	DocumentID      DocumentID
	Kind            Kind
	MovementID      string
	ConsolidationID string
	LegalEntityID   string
	CarrierPartyID  string
	CarrierRole     string
	Number          string
	Doc             CarrierContent
	Content         []byte
	ContentSHA256   string
	SourceVersion   int
	State           CarrierState
	RecordedBy      string
	RecordedAt      time.Time
	EnteredAt       time.Time
	VerifiedBy      string
	VerifiedAt      time.Time
	VerifyEntered   time.Time
	CancelReason    string
	CancelledBy     string
	CancelledAt     time.Time
	Revision        int64
}

// CarrierDocument is the aggregate root: the carrier's master bill as DOC recorded it.
type CarrierDocument struct {
	state   CarrierDocumentState
	pending []Event
}

// carrierKind is the kind and issuer capacity the facts' master bill kind and carrier role give: a co-loader's bill
// is an MBL with the co-loader as its issuer. Any other pair is refused.
func carrierKind(billKind, role string) (Kind, string, bool) {
	switch {
	case billKind == masterBillCarrierMBL && role == carrierRoleCarrier:
		return KindMBL, CapacityCarrier, true
	case billKind == masterBillCarrierSeaWaybill && role == carrierRoleCarrier:
		return KindMasterSeaWaybill, CapacityCarrier, true
	case billKind == masterBillCoLoad && role == carrierRoleCoLoader:
		return KindMBL, CapacityCoLoader, true
	}
	return "", "", false
}

// carrierRelease checks the release and the number of originals the person entered for a kind.
func carrierRelease(kind Kind, e CarrierEntered) (string, error) {
	switch kind {
	case KindMBL:
		switch {
		case e.Release == ReleaseOriginals && e.Originals >= 1 && e.Originals <= 9:
			return ReleaseOriginals, nil
		case e.Release == ReleaseExpress && e.Originals == 0:
			return ReleaseExpress, nil
		}
		return "", fmt.Errorf("%w: a master bill is released as ORIGINALS with 1 to 9 originals, or EXPRESS with none", ErrIncomplete)
	default:
		if (e.Release == "" || e.Release == ReleaseSeaWaybill) && e.Originals == 0 {
			return ReleaseSeaWaybill, nil
		}
		return "", fmt.Errorf("%w: a master sea waybill is released as SEA_WAYBILL with no originals", ErrIncomplete)
	}
}

// carrierEvidence checks the evidence of the carrier's issuance against the time it is recorded.
func carrierEvidence(e CarrierEvidence, at time.Time) bool {
	return hexDigest.MatchString(e.SHA256) && !blank(e.StorageID) && utf8.RuneCountInString(e.StorageID) <= 200 && !blank(e.MediaType) &&
		utf8.RuneCountInString(e.MediaType) <= 100 && !e.CapturedAt.IsZero() && !e.CapturedAt.After(at)
}

// carrierIssueDate checks the carrier's date of issue: a real calendar date, not after tomorrow in China (UTC+8), as a
// date clearly in the future is a typing error.
func carrierIssueDate(day string, at time.Time) bool {
	issued, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return false
	}
	local := at.In(time.FixedZone("UTC+8", 8*3600))
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	return !issued.After(today.AddDate(0, 0, 1))
}

// RecordCarrierDocument records the carrier's master bill from SEA's FINAL facts. The kind and the issuer capacity
// come from the facts' master bill kind and carrier role, and the number is the facts' master bill number; the
// person recording enters the release, the number of originals, the issue place and date and the evidence.
func RecordCarrierDocument(tenant TenantID, id DocumentID, f OceanFacts, e CarrierEntered, by string, at time.Time) (*CarrierDocument, error) {
	switch {
	case !f.Final:
		return nil, ErrSourceNotFinal
	case !identifier(string(tenant)) || !identifier(string(id)) || blank(by) || by != strings.TrimSpace(by) || at.IsZero():
		return nil, fmt.Errorf("%w: a tenant, a document id, the person recording (without surrounding space) and a time", ErrInvalidDocument)
	case !identifier(f.MovementID) || f.Version < 1 || !hexDigest.MatchString(f.SHA256) || !identifier(f.ConsolidationID) || !identifier(f.LegalEntityID) ||
		!identifier(f.CarrierPartyID) || f.CarrierPartyRevision < 1 || blank(f.MasterBill) || f.MasterBill != strings.TrimSpace(f.MasterBill):
		return nil, fmt.Errorf("%w: OceanDocFacts of %s names no consolidation, legal entity, carrier or master bill number", ErrInvalidDocument, f.MovementID)
	}
	kind, capacity, ok := carrierKind(f.MasterBillKind, f.CarrierRole)
	if !ok {
		return nil, fmt.Errorf("%w: a master bill of kind %q with the carrier in role %q is not one DOC records", ErrInvalidDocument, f.MasterBillKind, f.CarrierRole)
	}
	if utf8.RuneCountInString(f.MasterBill) > 35 {
		return nil, fmt.Errorf("%w: the master bill number is at most 35 characters", ErrInvalidDocument)
	}
	release, err := carrierRelease(kind, e)
	if err != nil {
		return nil, err
	}
	switch {
	case !unlocode.MatchString(e.IssuePlace):
		return nil, fmt.Errorf("%w: the issue place is a UN/LOCODE", ErrInvalidChange)
	case !carrierIssueDate(e.IssuedOn, at):
		return nil, fmt.Errorf("%w: the issue date is a calendar date, YYYY-MM-DD, not after tomorrow", ErrInvalidChange)
	case !carrierEvidence(e.Evidence, at):
		return nil, fmt.Errorf("%w: the evidence is a SHA-256, a storage id, a media type and the time it was captured, not after the record", ErrInvalidChange)
	}
	evidence := e.Evidence
	evidence.CapturedAt = evidence.CapturedAt.UTC()
	c := CarrierContent{Kind: string(kind), DocumentID: string(id), Number: f.MasterBill, IssuerCapacity: capacity,
		Carrier: CarrierRef{PartyID: f.CarrierPartyID, PartyRevision: f.CarrierPartyRevision, Role: f.CarrierRole}, LegalEntityID: f.LegalEntityID,
		ReleaseType: release, Originals: e.Originals, IssuePlace: e.IssuePlace, IssuedOn: e.IssuedOn,
		Source:          SourceRef{Contract: "OceanDocFacts v1", MovementID: f.MovementID, Version: f.Version, SHA256: f.SHA256},
		ConsolidationID: f.ConsolidationID, Vessel: f.Vessel, Voyage: f.Voyage, PlaceOfReceipt: f.PlaceOfReceipt, PortOfLoading: f.PortOfLoading,
		PortOfDischarge: f.PortOfDischarge, OnBoardAt: utc(f.OnBoardAt), Evidence: evidence}
	content, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	x := &CarrierDocument{state: CarrierDocumentState{TenantID: tenant, DocumentID: id, Kind: kind, MovementID: f.MovementID,
		ConsolidationID: f.ConsolidationID, LegalEntityID: f.LegalEntityID, CarrierPartyID: f.CarrierPartyID, CarrierRole: f.CarrierRole, Number: f.MasterBill,
		Doc: c, Content: content, ContentSHA256: Digest(content), SourceVersion: f.Version, State: CarrierIssued, RecordedBy: by, RecordedAt: at.UTC(),
		Revision: 1}}
	x.record(eventCarrierRecorded, at)
	return x, nil
}

// Verified tells whether a second person verified the record.
func (x *CarrierDocument) Verified() bool { return x.state.VerifiedBy != "" }

// Verify records that a person other than the one who recorded the document verified the record. A
// document is verified once, and only while it is ISSUED.
func (x *CarrierDocument) Verify(by string, at time.Time) error {
	st := &x.state
	switch {
	case st.State != CarrierIssued || x.Verified():
		return ErrInvalidTransition
	case blank(by) || by != strings.TrimSpace(by) || at.IsZero():
		return fmt.Errorf("%w: a verification names its author and a time", ErrInvalidChange)
	case by == st.RecordedBy:
		return ErrSelfVerification
	case at.Before(st.RecordedAt):
		return fmt.Errorf("%w: a verification is not dated before the record", ErrInvalidChange)
	}
	st.VerifiedBy, st.VerifiedAt = by, at.UTC()
	st.Revision++
	x.record(eventCarrierVerified, at)
	return nil
}

// Cancel cancels a recorded document, verified or not, with a reason. The number stays with the movement it was first
// recorded for.
func (x *CarrierDocument) Cancel(reason, by string, at time.Time) error {
	st := &x.state
	switch {
	case st.State != CarrierIssued:
		return ErrInvalidTransition
	case !text(reason):
		return fmt.Errorf("%w: a cancellation needs a reason", ErrInvalidChange)
	case blank(by) || by != strings.TrimSpace(by) || at.IsZero() || at.Before(later(st.RecordedAt, st.VerifiedAt)):
		return fmt.Errorf("%w: a cancellation names its author and a time not before the record or its verification", ErrInvalidChange)
	}
	st.State, st.CancelReason, st.CancelledBy, st.CancelledAt = CarrierCancelled, reason, by, at.UTC()
	st.Revision++
	x.record(eventCarrierCancelled, at)
	return nil
}

func (x *CarrierDocument) record(kind string, at time.Time) {
	st := x.state
	x.pending = append(x.pending, Event{Type: kind, At: at.UTC(), TenantID: st.TenantID, DocumentID: string(st.DocumentID), Kind: string(st.Kind),
		Version: int(st.Revision), ConsolidationID: st.ConsolidationID, MovementID: st.MovementID})
}

// Stamp records when DOC entered what was written since the document was loaded (T1).
func (x *CarrierDocument) Stamp(entered time.Time) {
	if x.state.EnteredAt.IsZero() {
		x.state.EnteredAt = entered.UTC()
	}
	if x.Verified() && x.state.VerifyEntered.IsZero() {
		x.state.VerifyEntered = entered.UTC()
	}
}

// RestoreCarrierDocument rebuilds a stored carrier document: its content must be the bytes its digest covers, and
// say what the columns say.
func RestoreCarrierDocument(st CarrierDocumentState) (*CarrierDocument, error) {
	var c CarrierContent
	if len(st.Content) == 0 || Digest(st.Content) != st.ContentSHA256 || json.Unmarshal(st.Content, &c) != nil || c.DocumentID != string(st.DocumentID) ||
		c.Kind != string(st.Kind) || c.Number != st.Number || c.Source.MovementID != st.MovementID || c.Source.Version != st.SourceVersion || st.Revision < 1 {
		return nil, fmt.Errorf("%w: carrier document %s content", ErrInvalidDocument, st.DocumentID)
	}
	st.Doc = c
	st.Content = append([]byte(nil), st.Content...)
	return &CarrierDocument{state: st}, nil
}

// State returns a copy for persistence and read models.
func (x *CarrierDocument) State() CarrierDocumentState {
	st := x.state
	st.Content = append([]byte(nil), st.Content...)
	return st
}

func (x *CarrierDocument) Pending() []Event { return append([]Event(nil), x.pending...) }

// Acknowledge is permitted only after the transaction holding the document and its Outbox events commits.
func (x *CarrierDocument) Acknowledge() { x.pending = nil }
