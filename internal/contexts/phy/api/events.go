package api

// Source is the event source of every PHY integration event.
const Source = "idp-cargo.phy"

// Published event types. Consolidation events have the consolidation as subject; house events the shipment,
// so that a house's attach, transfer and detach stay in order.
const (
	ConsolidationCreated   = "ConsolidationCreated.v1"
	ConsolidationAmended   = "ConsolidationAmended.v1"
	ConsolidationClosed    = "ConsolidationClosed.v1"
	ConsolidationReopened  = "ConsolidationReopened.v1"
	ConsolidationCancelled = "ConsolidationCancelled.v1"
	HouseAttached          = "HouseAttached.v1"
	HouseDetached          = "HouseDetached.v1"
	HouseTransferred       = "HouseTransferred.v1"
	HouseIntentRefreshed   = "HouseIntentRefreshed.v1"
)

// EventPayload is the JSON payload of every PHY event: identifiers only, never names, addresses, contacts,
// goods descriptions or master and house numbers (rule C3, privacy assessment V1.2). Consolidation events
// carry the consolidation version; house events the membership entry, the consolidation the house is in or
// left, the one it came from on a transfer, and the reason code.
type EventPayload struct {
	TenantID            string `json:"tenant_id"`
	SubjectID           string `json:"subject_id"`
	ConsolidationID     string `json:"consolidation_id"`
	Version             int    `json:"version,omitempty"`
	ShipmentID          string `json:"shipment_id,omitempty"`
	EntryNo             int    `json:"entry_no,omitempty"`
	FromConsolidationID string `json:"from_consolidation_id,omitempty"`
	Reason              string `json:"reason,omitempty"`
}
