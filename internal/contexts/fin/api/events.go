package api

// Source is the event source of every FIN integration event.
const Source = "idp-cargo.fin"

// Published event types (FinanceEvent v1 carries them to EVT as identifiers). Policy events have the legal entity as
// subject (InvoiceSeriesRegistered <legal entity>/<kind>), job events the job (a note's billed and returned sides are
// ChargeSideRevised too), note events the note (with its job), billing profile events the debtor, credit events
// <legal entity>/<debtor>, receipt events the debtor receipt (allocation events with the allocation's number as Version
// and the debit note).
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

	DebtorReceiptRecorded           = "DebtorReceiptRecorded.v1"
	DebtorReceiptAllocated          = "DebtorReceiptAllocated.v1"
	DebtorReceiptAllocationReversed = "DebtorReceiptAllocationReversed.v1"
	DebtorReceiptCancelled          = "DebtorReceiptCancelled.v1"

	// Pool events have the pool as subject and the consolidation in ShipmentID; Version is the cost line recorded or
	// revised, or the apportionment made.
	ConsolCostPoolOpened   = "ConsolCostPoolOpened.v1"
	ConsolCostRecorded     = "ConsolCostRecorded.v1"
	ConsolCostRevised      = "ConsolCostRevised.v1"
	ConsolCostsApportioned = "ConsolCostsApportioned.v1"
	ConsolCostPoolClosed   = "ConsolCostPoolClosed.v1"   // Version: the closure
	ConsolCostPoolReopened = "ConsolCostPoolReopened.v1" // Version: the closure reopened

	// AccrualReleased has the job (LineNo, Side COST, Version the side's RELEASED version) or the pool (LineNo,
	// Version the line's RELEASED version) as subject.
	AccrualReleased = "AccrualReleased.v1"

	// Creditor invoice events have the invoice as subject and its creditor; CreditorInvoiceMatched's Version is the
	// number of matches, the dispute events' the dispute. SettlementDisputed of FinanceEvent v1 is
	// CreditorInvoiceDisputed.
	CostVarianceThresholdSet       = "CostVarianceThresholdSet.v1" // subject the legal entity; Version the threshold
	CreditorInvoiceRecorded        = "CreditorInvoiceRecorded.v1"
	CreditorInvoiceMatched         = "CreditorInvoiceMatched.v1"
	CostVarianceApproved           = "CostVarianceApproved.v1"
	CreditorInvoiceDisputed        = "CreditorInvoiceDisputed.v1"
	CreditorInvoiceDisputeResolved = "CreditorInvoiceDisputeResolved.v1"
	CreditorInvoiceCancelled       = "CreditorInvoiceCancelled.v1"
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
	LineNo        int    `json:"line_no,omitempty"`    // ChargeLineRecorded and ChargeSideRevised: the line; Version is the side's version
	Side          string `json:"side,omitempty"`       // ChargeSideRevised: SELL or COST
	JobID         string `json:"job_id,omitempty"`     // DebtorInvoiceIssued, DebtorInvoiceCancelled and CreditNoteIssued: the note's job
	InvoiceID     string `json:"invoice_id,omitempty"` // DebtorReceiptAllocated and DebtorReceiptAllocationReversed: the debit note
	// CreditorPartyID is the creditor of a creditor invoice's events.
	CreditorPartyID string `json:"creditor_party_id,omitempty"`
}
