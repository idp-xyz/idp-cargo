package api

// Source is the event source of every COM integration event.
const Source = "idp-cargo.com"

// Published event types.
const (
	CustomerOrderRecorded          = "CustomerOrderRecorded.v1"
	CustomerOrderCancelled         = "CustomerOrderCancelled.v1"
	FulfillmentObligationAllocated = "FulfillmentObligationAllocated.v1"
	FulfillmentObligationRevoked   = "FulfillmentObligationRevoked.v1"
	ObligationReserved             = "ObligationReserved.v1"
	ObligationReservationReleased  = "ObligationReservationReleased.v1"
)

// EventPayload is the JSON payload of every COM event: identifiers only. SubjectID is the order. Events
// about an obligation add the obligation and the version written or concerned; FulfillmentObligationRevoked.v1
// carries the version that records the revocation. ObligationReserved.v1 and
// ObligationReservationReleased.v1 add the shipment.
type EventPayload struct {
	TenantID          string `json:"tenant_id"`
	SubjectID         string `json:"subject_id"`
	ObligationID      string `json:"obligation_id,omitempty"`
	ObligationVersion int64  `json:"obligation_version,omitempty"`
	ShipmentID        string `json:"shipment_id,omitempty"`
}
