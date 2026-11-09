package api

// Source is the event source of every RATE integration event.
const Source = "idp-cargo.rate"

// Published event types.
const (
	QuotePriced        = "QuotePriced.v1"
	QuotePriceApproved = "QuotePriceApproved.v1"
	QuoteAccepted      = "QuoteAccepted.v1"
	QuoteExpired       = "QuoteExpired.v1"
)

// EventPayload is the JSON payload of every RATE event: identifiers only, no prices or party data.
type EventPayload struct {
	TenantID  string `json:"tenant_id"`
	SubjectID string `json:"subject_id"`
}
