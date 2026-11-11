package api

// Source is the event source of every DOC integration event.
const Source = "idp-cargo.doc"

// Published event types. Document events have the document as subject; authority events the authority.
// TransportDocumentIssued, TransportDocumentAmended and TransportDocumentCancelled tell CUS and FIN that a house
// manifest or a billing trigger may change (TransportDocumentSnapshot v1 comes later).
const (
	TransportDocumentDrafted            = "TransportDocumentDrafted.v1"
	TransportDocumentApproved           = "TransportDocumentApproved.v1"
	TransportDocumentIssued             = "TransportDocumentIssued.v1"
	TransportDocumentAmendmentOpened    = "TransportDocumentAmendmentOpened.v1"
	TransportDocumentAmended            = "TransportDocumentAmended.v1"
	TransportDocumentAmendmentAbandoned = "TransportDocumentAmendmentAbandoned.v1"
	TransportDocumentCancelled          = "TransportDocumentCancelled.v1"
	IssuanceAuthorityRecorded           = "IssuanceAuthorityRecorded.v1"
	IssuanceAuthorityWithdrawn          = "IssuanceAuthorityWithdrawn.v1"
)

// EventPayload is the JSON payload of every DOC event: identifiers only, never numbers, names, addresses or
// document text.
type EventPayload struct {
	TenantID        string `json:"tenant_id"`
	SubjectID       string `json:"subject_id"`
	Kind            string `json:"kind"`
	Version         int    `json:"version"`
	ShipmentID      string `json:"shipment_id,omitempty"`
	ConsolidationID string `json:"consolidation_id,omitempty"`
	MovementID      string `json:"movement_id,omitempty"`
}
