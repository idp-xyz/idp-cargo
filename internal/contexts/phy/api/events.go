package api

// Source is the event source of every PHY integration event.
const Source = "idp-cargo.phy"

// Published event types. Consolidation events have the consolidation as subject; house events the shipment,
// so that a house's attach, transfer and detach stay in order; cargo lot events the lot; load execution events
// the execution.
const (
	ConsolidationCreated    = "ConsolidationCreated.v1"
	ConsolidationAmended    = "ConsolidationAmended.v1"
	ConsolidationClosed     = "ConsolidationClosed.v1"
	ConsolidationReopened   = "ConsolidationReopened.v1"
	ConsolidationCancelled  = "ConsolidationCancelled.v1"
	ConsolidationReconciled = "ConsolidationReconciled.v1"
	HouseAttached           = "HouseAttached.v1"
	HouseDetached           = "HouseDetached.v1"
	HouseTransferred        = "HouseTransferred.v1"
	HouseIntentRefreshed    = "HouseIntentRefreshed.v1"

	CargoReceived               = "CargoReceived.v1"
	CargoMeasured               = "CargoMeasured.v1"
	ScreeningResultRecorded     = "ScreeningResultRecorded.v1"
	CargoReceiptCompleted       = "CargoReceiptCompleted.v1"
	CargoDiscrepancyRecorded    = "CargoDiscrepancyRecorded.v1"
	CargoDiscrepancyResolved    = "CargoDiscrepancyResolved.v1"
	CargoLotClosed              = "CargoLotClosed.v1"
	LoadExecutionOpened         = "LoadExecutionOpened.v1"
	LoadingStarted              = "LoadingStarted.v1"
	CargoLoaded                 = "CargoLoaded.v1"
	CargoUnloaded               = "CargoUnloaded.v1"
	LoadExecutionHeld           = "LoadExecutionHeld.v1"
	LoadExecutionHoldReleased   = "LoadExecutionHoldReleased.v1"
	LoadExecutionResumed        = "LoadExecutionResumed.v1"
	LoadExecutionClosed         = "LoadExecutionClosed.v1"
	CarrierLoadEvidenceRecorded = "CarrierLoadEvidenceRecorded.v1"
	LoadingDiscrepancyRecorded  = "LoadingDiscrepancyRecorded.v1"
	LoadingDiscrepancyResolved  = "LoadingDiscrepancyResolved.v1"
)

// EventPayload is the JSON payload of every PHY event: identifiers only, never names, addresses, contacts,
// goods descriptions, master and house numbers or carrier message bodies (rule C3, privacy assessment V1.2).
// Consolidation events carry the consolidation version; house events the membership entry, the consolidation
// the house is in or left, the one it came from on a transfer, and the reason code. Cargo lot events carry the
// lot, its shipment and the item (receipt line, measurement version, screening result or discrepancy); load
// execution events the execution, its consolidation, the shipment and lot a line or hold is about, the item
// (line, reversal, hold, carrier evidence or discrepancy) and a reason, hold, fact, kind or resolution code.
type EventPayload struct {
	TenantID            string `json:"tenant_id"`
	SubjectID           string `json:"subject_id"`
	ConsolidationID     string `json:"consolidation_id"`
	Version             int    `json:"version,omitempty"`
	ShipmentID          string `json:"shipment_id,omitempty"`
	EntryNo             int    `json:"entry_no,omitempty"`
	FromConsolidationID string `json:"from_consolidation_id,omitempty"`
	Reason              string `json:"reason,omitempty"`
	LotID               string `json:"lot_id,omitempty"`
	ExecutionID         string `json:"execution_id,omitempty"`
	ItemNo              int    `json:"item_no,omitempty"`
}
