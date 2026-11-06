package api

// Source is the event source of every COM integration event.
const Source = "idp-cargo.com"

// Published event types.
const (
	CustomerOrderRecorded          = "CustomerOrderRecorded.v1"
	FulfillmentObligationAllocated = "FulfillmentObligationAllocated.v1"
)

// EventPayload is the JSON payload of every COM event: identifiers only. SubjectID is the order;
// FulfillmentObligationAllocated.v1 adds the obligation and its version.
type EventPayload struct {
	TenantID          string `json:"tenant_id"`
	SubjectID         string `json:"subject_id"`
	ObligationID      string `json:"obligation_id,omitempty"`
	ObligationVersion int64  `json:"obligation_version,omitempty"`
}
