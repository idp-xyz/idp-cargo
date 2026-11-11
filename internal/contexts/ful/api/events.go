package api

// Source is the event source of every FUL integration event.
const Source = "idp-cargo.ful"

// Published event types.
const (
	ShipmentCreated            = "ShipmentCreated.v1"
	ShipmentObligationAdded    = "ShipmentObligationAdded.v1"
	ShipmentPlanAmended        = "ShipmentPlanAmended.v1"
	ShipmentPlanConfirmed      = "ShipmentPlanConfirmed.v1"
	ShipmentObligationReleased = "ShipmentObligationReleased.v1"
	ShipmentPartChanged        = "ShipmentPartChanged.v1"
	ShipmentCancelled          = "ShipmentCancelled.v1"
	// FUL V0.4
	ShipmentConsolAttachmentRecorded = "ShipmentConsolAttachmentRecorded.v1"
	ShipmentConsolDetachmentRecorded = "ShipmentConsolDetachmentRecorded.v1"
	ShipmentLoadEvidenceRecorded     = "ShipmentLoadEvidenceRecorded.v1"
	ShipmentExecutionStatusChanged   = "ShipmentExecutionStatusChanged.v1"
)

// EventPayload is the JSON payload of every FUL event: identifiers only. SubjectID is the shipment;
// events about one obligation add its id and version, events about a consol attachment the stage and the
// consolidation, events about load evidence the stage, the consolidation it was tendered in (if any) and the
// evidence version; consumers read the status and quantities through FUL's queries.
type EventPayload struct {
	TenantID          string `json:"tenant_id"`
	SubjectID         string `json:"subject_id"`
	ObligationID      string `json:"obligation_id,omitempty"`
	ObligationVersion int64  `json:"obligation_version,omitempty"`
	Stage             string `json:"stage,omitempty"`
	ConsolidationID   string `json:"consolidation_id,omitempty"`
	EvidenceNo        int    `json:"evidence_no,omitempty"`
}
