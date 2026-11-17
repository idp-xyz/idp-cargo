package api

// Source is the event source of every SEA integration event.
const Source = "idp-cargo.sea"

// Published event types. Movement events have the movement as subject; series events the legal entity.
const (
	OceanMovementOpened       = "OceanMovementOpened.v1"
	VoyageLegsLinked          = "VoyageLegsLinked.v1"
	MasterBillRecorded        = "MasterBillRecorded.v1"
	HouseBillAssociated       = "HouseBillAssociated.v1"
	HouseBillFactsRefreshed   = "HouseBillFactsRefreshed.v1"
	HouseBillAssociationEnded = "HouseBillAssociationEnded.v1"
	OceanMovementCancelled    = "OceanMovementCancelled.v1"
	OceanDocFactsFinalized    = "OceanDocFactsFinalized.v1"
	OceanDocFactsSuperseded   = "OceanDocFactsSuperseded.v1"
	OceanCarrierFactRecorded  = "OceanCarrierFactRecorded.v1"
	HblSeriesRegistered       = "HblSeriesRegistered.v1"
)

// Carrier fact kinds. A fact names a container or a bill, and does not change the movement.
const (
	CarrierLoaded     = "LOADED_ON_BOARD"
	CarrierDeparted   = "DEPARTED"
	CarrierRolled     = "ROLLED"
	CarrierDischarged = "DISCHARGED"
)

// EventPayload is the JSON payload of every SEA event: identifiers only, never master or house bill numbers,
// container or seal numbers, vessel names or quantities (rule C3); consumers read them through queries. A container
// visit's events have the visit as subject and name its movement.
type EventPayload struct {
	TenantID        string `json:"tenant_id"`
	SubjectID       string `json:"subject_id"`
	ConsolidationID string `json:"consolidation_id,omitempty"`
	MovementID      string `json:"movement_id,omitempty"`
	ShipmentID      string `json:"shipment_id,omitempty"`
	EntryNo         int    `json:"entry_no,omitempty"`
	BillNo          int    `json:"bill_no,omitempty"`
	SealNo          int    `json:"seal_no,omitempty"`
	AssertionNo     int    `json:"assertion_no,omitempty"`
	EventNo         int    `json:"event_no,omitempty"`
	FactNo          int    `json:"fact_no,omitempty"`
	FactsVersion    int    `json:"facts_version,omitempty"`
	BookingID       string `json:"booking_id,omitempty"`
	CapacityVersion int    `json:"capacity_version,omitempty"`
	Reason          string `json:"reason,omitempty"`
}
