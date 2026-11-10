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
	CustomerOrderLineAmended       = "CustomerOrderLineAmended.v1"
	CustomerOrderLineAdded         = "CustomerOrderLineAdded.v1"
	OrderPartySet                  = "OrderPartySet.v1"
	OrderTransportChanged          = "OrderTransportChanged.v1"
)

// EventPayload is the JSON payload of every COM event: identifiers only. SubjectID is the order. Events
// about an obligation add the obligation and the version written or concerned; FulfillmentObligationRevoked.v1
// carries the version that records the revocation. ObligationReserved.v1 and
// ObligationReservationReleased.v1 add the shipment. CustomerOrderLineAmended.v1 and
// CustomerOrderLineAdded.v1 add the line and the version written (C10); they carry no quantities.
type EventPayload struct {
	TenantID          string `json:"tenant_id"`
	SubjectID         string `json:"subject_id"`
	ObligationID      string `json:"obligation_id,omitempty"`
	ObligationVersion int64  `json:"obligation_version,omitempty"`
	ShipmentID        string `json:"shipment_id,omitempty"`
	LineNo            int    `json:"order_line_no,omitempty"`
	LineVersion       int64  `json:"order_line_version,omitempty"`
	// OrderPartySet.v1 names the role, the version written, the kind and whether the version carries an
	// address or contact given on the order; never a name, address or contact (C9, X1).
	PartyRole    string `json:"party_role,omitempty"`
	PartyVersion int64  `json:"party_version,omitempty"`
	PartyKind    string `json:"party_kind,omitempty"`
	Overridden   bool   `json:"overridden,omitempty"`
	// OrderTransportChanged.v1 names the transport version written.
	TransportVersion int64 `json:"transport_version,omitempty"`
}
