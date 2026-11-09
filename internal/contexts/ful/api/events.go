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
	ShipmentCancelled          = "ShipmentCancelled.v1"
)

// EventPayload is the JSON payload of every FUL event: identifiers only. SubjectID is the shipment;
// events about one obligation add its id and version.
type EventPayload struct {
	TenantID          string `json:"tenant_id"`
	SubjectID         string `json:"subject_id"`
	ObligationID      string `json:"obligation_id,omitempty"`
	ObligationVersion int64  `json:"obligation_version,omitempty"`
}
