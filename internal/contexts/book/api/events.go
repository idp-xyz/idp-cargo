package api

// Source is the event source of every BOOK integration event.
const Source = "idp-cargo.book"

// Published event types; all of a booking share its partition.
const (
	CarrierBookingDrafted        = "CarrierBookingDrafted.v1"
	BookingRequestAmended        = "BookingRequestAmended.v1"
	BookingRequested             = "BookingRequested.v1" // the request was recorded as sent
	CarrierBookingAcknowledged   = "CarrierBookingAcknowledged.v1"
	BookingConfirmed             = "BookingConfirmed.v1" // capacity version 1
	CarrierBookingRejected       = "CarrierBookingRejected.v1"
	BookingCapacityChanged       = "BookingCapacityChanged.v1" // a carrier change or a confirmed change: a new capacity version
	BookingChangeRequested       = "BookingChangeRequested.v1"
	BookingChangeRefused         = "BookingChangeRefused.v1"
	BookingDeclinedByCarrier     = "BookingDeclinedByCarrier.v1"
	BookingCancellationRequested = "BookingCancellationRequested.v1"
	CarrierBookingCancelled      = "CarrierBookingCancelled.v1"
	BookingCapacityAllocated     = "BookingCapacityAllocated.v1"
	BookingCapacityReleased      = "BookingCapacityReleased.v1"
)

// EventPayload is the JSON payload of every BOOK event: identifiers only (rule C3), never carrier references,
// contacts or quantities. Request and capacity versions and the allocation are named where involved.
type EventPayload struct {
	TenantID        string `json:"tenant_id"`
	BookingID       string `json:"booking_id"`
	RequestVersion  int    `json:"request_version,omitempty"`
	CapacityVersion int    `json:"capacity_version,omitempty"`
	AllocationNo    int    `json:"allocation_no,omitempty"`
	ConsumerKind    string `json:"consumer_kind,omitempty"`
	ConsumerID      string `json:"consumer_id,omitempty"`
}
