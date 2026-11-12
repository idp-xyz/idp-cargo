package api

// Source is the event source of every RATE integration event.
const Source = "idp-cargo.rate"

// Published event types.
const (
	QuotePriced        = "QuotePriced.v1"
	QuotePriceApproved = "QuotePriceApproved.v1"
	QuoteAccepted      = "QuoteAccepted.v1"
	QuoteExpired       = "QuoteExpired.v1"

	ChargeCodeDefined        = "ChargeCodeDefined.v1"
	ChargeCodeRevised        = "ChargeCodeRevised.v1"
	ChargeCodeDeactivated    = "ChargeCodeDeactivated.v1"
	ChargeCodeReactivated    = "ChargeCodeReactivated.v1"
	RateCardVersionPublished = "RateCardVersionPublished.v1"
	RateCardVersionWithdrawn = "RateCardVersionWithdrawn.v1"
	RatesEvaluated           = "RatesEvaluated.v1"
)

// EventPayload is the JSON payload of every RATE event: identifiers only, no prices or party data.
// SubjectID is the quote, the charge code, the rate card or the snapshot; Version is the code's or the card's version.
type EventPayload struct {
	TenantID  string `json:"tenant_id"`
	SubjectID string `json:"subject_id"`
	Version   int    `json:"version,omitempty"`
}
