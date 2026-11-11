package api

// Source is the event source of every FIN integration event.
const Source = "idp-cargo.fin"

// Published event types (FinanceEvent v1 carries them to EVT as identifiers). Policy events have the legal entity as
// subject (InvoiceSeriesRegistered <legal entity>/<kind>), job events the job (a note's billed and returned sides are
// ChargeSideRevised too), note events the note (with its job), billing profile events the debtor, credit events
// <legal entity>/<debtor>.
const (
	FinancePolicyOpened  = "FinancePolicyOpened.v1"
	ExchangeRateRecorded = "ExchangeRateRecorded.v1"
	ExchangeRateEnded    = "ExchangeRateEnded.v1"
	FreightJobOpened     = "FreightJobOpened.v1"
	PricingBasisApplied  = "PricingBasisApplied.v1"
	ChargeLineRecorded   = "ChargeLineRecorded.v1"
	ChargeSideRevised    = "ChargeSideRevised.v1"
	FreightJobCancelled  = "FreightJobCancelled.v1"
	CreditTermsSet       = "CreditTermsSet.v1"
	CreditHoldPlaced     = "CreditHoldPlaced.v1"
	CreditHoldReleased   = "CreditHoldReleased.v1"

	ChargeTaxTreatmentSet   = "ChargeTaxTreatmentSet.v1"
	InvoiceSeriesRegistered = "InvoiceSeriesRegistered.v1"
	BillingProfileSet       = "BillingProfileSet.v1"
	DebtorInvoiceIssued     = "DebtorInvoiceIssued.v1"    // a debit note
	DebtorInvoiceCancelled  = "DebtorInvoiceCancelled.v1" // a debit note whose every line a credit note reversed
	CreditNoteIssued        = "CreditNoteIssued.v1"
)

// EventPayload is the JSON payload of every FIN event: identifiers and versions only, never amounts, names or tax
// data.
type EventPayload struct {
	TenantID      string `json:"tenant_id"`
	SubjectID     string `json:"subject_id"`
	LegalEntityID string `json:"legal_entity_id"`
	Version       int    `json:"version,omitempty"`
	ShipmentID    string `json:"shipment_id,omitempty"`
	DebtorPartyID string `json:"debtor_party_id,omitempty"`
	LineNo        int    `json:"line_no,omitempty"` // ChargeLineRecorded and ChargeSideRevised: the line; Version is the side's version
	Side          string `json:"side,omitempty"`    // ChargeSideRevised: SELL or COST
	JobID         string `json:"job_id,omitempty"`  // DebtorInvoiceIssued, DebtorInvoiceCancelled and CreditNoteIssued: the note's job
}
