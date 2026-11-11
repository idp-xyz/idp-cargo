package api

// Source is the event source of every AIR integration event.
const Source = "idp-cargo.air"

// Published event types. Movement events have the movement as subject; series events the legal entity.
const (
	AirMovementOpened            = "AirMovementOpened.v1"
	FlightSegmentsLinked         = "FlightSegmentsLinked.v1"
	MawbAssigned                 = "MawbAssigned.v1"
	MawbVoided                   = "MawbVoided.v1"
	HouseWaybillAssociated       = "HouseWaybillAssociated.v1"
	HouseWaybillFactsRefreshed   = "HouseWaybillFactsRefreshed.v1"
	HouseWaybillAssociationEnded = "HouseWaybillAssociationEnded.v1"
	AirMovementCancelled         = "AirMovementCancelled.v1"
	HawbSeriesRegistered         = "HawbSeriesRegistered.v1"
	// AirDocFactsFinalized: the movement's facts were finalized for DOC as version FactsVersion of AirDocFacts v1;
	// DOC reads it with GetAirDocFacts. A later version supersedes it.
	AirDocFactsFinalized = "AirDocFactsFinalized.v1"
)

// EventPayload is the JSON payload of every AIR event: identifiers only, never MAWB or HAWB numbers, names or
// quantities (rule C3); consumers read numbers through queries.
type EventPayload struct {
	TenantID        string `json:"tenant_id"`
	SubjectID       string `json:"subject_id"`
	ConsolidationID string `json:"consolidation_id,omitempty"`
	ShipmentID      string `json:"shipment_id,omitempty"`
	EntryNo         int    `json:"entry_no,omitempty"`
	BookingID       string `json:"booking_id,omitempty"`
	CapacityVersion int    `json:"capacity_version,omitempty"`
	Reason          string `json:"reason,omitempty"`
	FactsVersion    int    `json:"facts_version,omitempty"`
}
