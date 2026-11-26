package http

import (
	nethttp "net/http"

	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	"go.idp.xyz/idp-cargo/internal/foundation/httpapi"
)

// The generic routes of FIN (since V2.108), beside the six hand-written ones of V2.99 that EII's contract (docs/api/
// fin-eii.md) describes, which stay as they are. FIN authorizes its commands itself, before it replays one. Its reads
// take no actor, so the route checks: a read that shows a cost (a job's charge lines, the payables, the accruals, a
// creditor's account and payments) is "fin:ReadProfit", as the profit is; the receivables, the policy, the billing
// profiles and the fapiao are "fin:Read".

// bc is FIN's name in permissions ("fin:<Action>").
const bc = "fin"

// Generic is what the generic routes call.
type Generic interface {
	api.Commands
	api.Queries
}

// Kinds maps FIN's error kinds to statuses.
var Kinds = kinds

// RegisterGeneric registers FIN's generic routes.
func RegisterGeneric(r *httpapi.Routes, s Generic) {
	k := Kinds
	cmd := func(path, calls, summary string) httpapi.Op {
		return httpapi.Op{Method: "POST", Path: path, Context: bc, Calls: calls, Action: calls, Summary: summary}
	}
	read := func(path, calls, summary string) httpapi.Op {
		return httpapi.Op{Method: "GET", Path: path, Context: bc, Calls: calls, Summary: summary}
	}
	cost := func(o httpapi.Op) httpapi.Op { o.Action = api.ActionReadProfit; return o }
	created := func(o httpapi.Op) httpapi.Op { o.Status = nethttp.StatusCreated; return o }
	checked := func(o httpapi.Op) httpapi.Op { o.ContextChecks, o.Action = true, ""; return o }
	const le = "/v1/fin/legal-entities/{legal_entity_id}"
	const debtor, creditor = le + "/debtors/{debtor_party_id}", le + "/creditors/{creditor_party_id}"
	const job, receipt, inv, pay, fp = "/v1/fin/jobs/{job_id}", "/v1/fin/receipts/{receipt_id}", "/v1/fin/creditor-invoices/{invoice_id}",
		"/v1/fin/creditor-payments/{payment_id}", "/v1/fin/fapiao/{fapiao_id}"

	// a legal entity's finance policy, exchange rates, tax treatments and invoice series
	httpapi.Command(r, k, created(cmd(le+"/policy", "OpenFinancePolicy", "Open the finance policy: functional currency and book")), s.OpenFinancePolicy)
	httpapi.Query(r, k, read(le+"/policy", "GetFinancePolicy", "The finance policy, its rates, tax treatments and series"), s.GetFinancePolicy)
	httpapi.Command(r, k, cmd(le+"/exchange-rates", "RecordExchangeRate", "Record an exchange rate with its source"), s.RecordExchangeRate)
	httpapi.Command(r, k, cmd(le+"/exchange-rates/{entry_no}/end", "EndExchangeRate", "End an exchange rate"), s.EndExchangeRate)
	httpapi.Command(r, k, cmd(le+"/tax-treatments", "SetChargeTaxTreatment", "Set a charge code's tax treatment"), s.SetChargeTaxTreatment)
	httpapi.Command(r, k, cmd(le+"/invoice-series", "RegisterInvoiceSeries", "Register the invoice number series"), s.RegisterInvoiceSeries)
	httpapi.Query(r, k, read(le+"/credit-exposure", "ListCreditExposure", "Every debtor's credit exposure"), s.ListCreditExposure)
	httpapi.Query(r, k, read(le+"/debit-notes-on-changed-documents", "ListDebitNotesOnChangedDocuments", "Open debit notes whose house document changed after"), s.ListDebitNotesOnChangedDocuments)

	// a debtor: credit, open items, receipts, statement, fapiao
	httpapi.Command(r, k, cmd(debtor+"/credit-terms", "SetCreditTerms", "Set the debtor's credit terms"), s.SetCreditTerms)
	httpapi.Command(r, k, cmd(debtor+"/credit-holds", "PlaceCreditHold", "Place a credit hold"), s.PlaceCreditHold)
	httpapi.Command(r, k, cmd(debtor+"/credit-holds/release", "ReleaseCreditHold", "Release the credit hold"), s.ReleaseCreditHold)
	httpapi.Query(r, k, read(debtor+"/credit", "GetCreditAccount", "The debtor's credit account"), s.GetCreditAccount)
	httpapi.Query(r, k, read(debtor+"/open-items", "ListOpenDebtorItems", "The debtor's open invoices and notes"), s.ListOpenDebtorItems)
	httpapi.Query(r, k, read(debtor+"/receipts", "ListDebtorReceipts", "The debtor's receipts"), s.ListDebtorReceipts)
	httpapi.Query(r, k, read(debtor+"/statement", "GetDebtorStatement", "The debtor's statement (from, to)"), s.GetDebtorStatement)
	httpapi.Query(r, k, read(debtor+"/fapiao", "ListFapiaoRecords", "The debtor's fapiao"), s.ListFapiaoRecords)
	httpapi.Command(r, k, cmd("/v1/fin/billing-profiles/{debtor_party_id}", "SetBillingProfile", "Set a debtor's billing profile"), s.SetBillingProfile)
	httpapi.Query(r, k, read("/v1/fin/billing-profiles/{debtor_party_id}", "GetBillingProfile", "A debtor's billing profile"), s.GetBillingProfile)

	// a creditor: the account, accruals and payments (costs)
	httpapi.Query(r, k, cost(read(creditor+"/account", "GetCreditorAccount", "The creditor's account: fin:ReadProfit")), s.GetCreditorAccount)
	httpapi.Query(r, k, cost(read(creditor+"/accruals", "ListOpenAccruals", "The open accruals: fin:ReadProfit")), s.ListOpenAccruals)
	httpapi.Query(r, k, cost(read(creditor+"/payments", "ListCreditorPayments", "The payments: fin:ReadProfit")), s.ListCreditorPayments)

	// freight jobs: revenue and cost lines, debit and credit notes
	httpapi.Command(r, k, created(cmd("/v1/fin/jobs", "OpenFreightJob", "Open a shipment's freight job")), s.OpenFreightJob)
	httpapi.Query(r, k, cost(read(job, "GetFreightJob", "A job: its charge lines with their costs: fin:ReadProfit")), s.GetFreightJob)
	httpapi.Query(r, k, cost(read("/v1/fin/shipments/{shipment_id}/jobs", "ListJobsForShipment", "A shipment's jobs: fin:ReadProfit")), s.ListJobsForShipment)
	httpapi.Query(r, k, checked(read(job+"/legal-entity", "GetJobLegalEntity", "The job's legal entity: fin:ReadProfit")), s.GetJobLegalEntity)
	httpapi.Command(r, k, cmd(job+"/pricing-basis", "ApplyPricingBasis", "Apply the accepted quote and the order"), s.ApplyPricingBasis)
	httpapi.Command(r, k, cmd(job+"/rate", "RateFreightJob", "Rate the job from RATE's tariffs"), s.RateFreightJob)
	httpapi.Command(r, k, cmd(job+"/charge-lines", "RecordChargeLine", "Record a charge line: its sell and cost sides"), s.RecordChargeLine)
	httpapi.Command(r, k, cmd(job+"/charge-lines/{line_no}/revise", "ReviseChargeSide", "Revise a side of a line, with a reason"), s.ReviseChargeSide)
	httpapi.Command(r, k, cmd(job+"/charge-lines/{line_no}/release-accrual", "ReleaseJobAccrual", "Release a cost line's accrual no invoice will match"), s.ReleaseJobAccrual)
	httpapi.Command(r, k, cmd(job+"/cancel", "CancelFreightJob", "Cancel the job"), s.CancelFreightJob)
	httpapi.Command(r, k, cmd(job+"/debit-notes", "IssueDebitNote", "Issue a debit note on the house's bill"), s.IssueDebitNote)
	httpapi.Command(r, k, cmd(job+"/credit-notes", "IssueCreditNote", "Issue a credit note against a debit note"), s.IssueCreditNote)
	httpapi.Query(r, k, read("/v1/fin/debtor-invoices/{invoice_id}", "GetDebtorInvoice", "A debit or credit note"), s.GetDebtorInvoice)

	// receipts from debtors
	httpapi.Command(r, k, created(cmd("/v1/fin/receipts", "RecordDebtorReceipt", "Record money received")), s.RecordDebtorReceipt)
	httpapi.Query(r, k, read(receipt, "GetDebtorReceipt", "A receipt and its allocations"), s.GetDebtorReceipt)
	httpapi.Command(r, k, cmd(receipt+"/allocations", "AllocateDebtorReceipt", "Allocate a receipt to open items"), s.AllocateDebtorReceipt)
	httpapi.Command(r, k, cmd(receipt+"/allocations/{allocation_no}/reverse", "ReverseDebtorReceiptAllocation", "Reverse an allocation"), s.ReverseDebtorReceiptAllocation)
	httpapi.Command(r, k, cmd(receipt+"/cancel", "CancelDebtorReceipt", "Cancel a receipt"), s.CancelDebtorReceipt)

	// creditors' invoices and payments (costs)
	httpapi.Command(r, k, created(cmd("/v1/fin/creditor-invoices", "RecordCreditorInvoice", "Record a creditor's invoice")), s.RecordCreditorInvoice)
	httpapi.Query(r, k, cost(read("/v1/fin/creditor-invoices", "ListCreditorInvoices", "A legal entity's creditor invoices: fin:ReadProfit")), s.ListCreditorInvoices)
	httpapi.Query(r, k, cost(read(inv, "GetCreditorInvoice", "A creditor invoice: fin:ReadProfit")), s.GetCreditorInvoice)
	httpapi.Command(r, k, cmd(inv+"/match", "MatchCreditorInvoice", "Match it to accrued cost lines"), s.MatchCreditorInvoice)
	httpapi.Command(r, k, cmd(inv+"/approve-variance", "ApproveCostVariance", "Approve the variance (not the matcher's, unless fin:ApproveOwnCostVariance by name)"), s.ApproveCostVariance)
	httpapi.Command(r, k, cmd(inv+"/dispute", "DisputeCreditorInvoice", "Dispute it"), s.DisputeCreditorInvoice)
	httpapi.Command(r, k, cmd(inv+"/resolve-dispute", "ResolveCreditorInvoiceDispute", "Resolve the dispute"), s.ResolveCreditorInvoiceDispute)
	httpapi.Command(r, k, cmd(inv+"/cancel", "CancelCreditorInvoice", "Cancel it"), s.CancelCreditorInvoice)
	httpapi.Command(r, k, created(cmd("/v1/fin/creditor-payments", "RecordCreditorPayment", "Record a payment to a creditor")), s.RecordCreditorPayment)
	httpapi.Query(r, k, cost(read(pay, "GetCreditorPayment", "A creditor payment: fin:ReadProfit")), s.GetCreditorPayment)
	httpapi.Command(r, k, cmd(pay+"/allocations", "AllocateCreditorPayment", "Allocate a payment to invoices"), s.AllocateCreditorPayment)
	httpapi.Command(r, k, cmd(pay+"/allocations/{allocation_no}/reverse", "ReverseCreditorPaymentAllocation", "Reverse an allocation"), s.ReverseCreditorPaymentAllocation)
	httpapi.Command(r, k, cmd(pay+"/cancel", "CancelCreditorPayment", "Cancel a payment"), s.CancelCreditorPayment)

	// fapiao: requested from the tax platform, its answer recorded
	httpapi.Command(r, k, created(cmd("/v1/fin/fapiao", "RequestFapiao", "Request a fapiao for debit notes")), s.RequestFapiao)
	httpapi.Query(r, k, read(fp, "GetFapiaoRecord", "A fapiao record"), s.GetFapiaoRecord)
	httpapi.Command(r, k, cmd(fp+"/issued", "RecordFapiaoIssued", "Record the tax platform's issue"), s.RecordFapiaoIssued)
	httpapi.Command(r, k, cmd(fp+"/failed", "RecordFapiaoFailed", "Record the tax platform's failure"), s.RecordFapiaoFailed)
	httpapi.Command(r, k, cmd(fp+"/red-letter", "RecordRedLetter", "Record a red-letter fapiao"), s.RecordRedLetter)

	registerPoolsAndSettings(r, s)
}

// registerPoolsAndSettings registers the cost pools, the variance thresholds, the settings and the reviews (since
// V2.108). A pool's figures are costs: "fin:ReadProfit". The settings commands are authorized by FIN with the
// permissions by name ("fin:ConfigureSettings", "fin:ApproveSettings", "fin:ReviewSettingsExceptions",
// "fin:ReviewAfterTheFact"); a change set held for an onboarding is the platform's (its onboarding_id is refused here).
func registerPoolsAndSettings(r *httpapi.Routes, s Generic) {
	k := Kinds
	cmd := func(path, calls, summary string) httpapi.Op {
		return httpapi.Op{Method: "POST", Path: path, Context: bc, Calls: calls, Action: calls, Summary: summary}
	}
	read := func(path, calls, summary string) httpapi.Op {
		return httpapi.Op{Method: "GET", Path: path, Context: bc, Calls: calls, Summary: summary}
	}
	cost := func(o httpapi.Op) httpapi.Op { o.Action = api.ActionReadProfit; return o }
	checked := func(o httpapi.Op) httpapi.Op { o.ContextChecks, o.Action = true, ""; return o }
	const pool, le, change = "/v1/fin/pools/{pool_id}", "/v1/fin/legal-entities/{legal_entity_id}", "/v1/fin/settings-changes/{change_id}"

	// the consolidation cost pools
	open := cmd("/v1/fin/pools", "OpenConsolCostPool", "Open a consolidation's cost pool")
	open.Status = nethttp.StatusCreated
	httpapi.Command(r, k, open, s.OpenConsolCostPool)
	httpapi.Query(r, k, cost(read(pool, "GetConsolCostPool", "A pool: its lines, apportionments and shares: fin:ReadProfit")), s.GetConsolCostPool)
	httpapi.Query(r, k, cost(read(pool+"/pending", "GetConsolCostPoolPendingItems", "What the pool waits for: fin:ReadProfit")), s.GetConsolCostPoolPendingItems)
	httpapi.Command(r, k, cmd(pool+"/rate", "RateConsolCostPool", "Rate the pool from RATE's tariffs"), s.RateConsolCostPool)
	httpapi.Command(r, k, cmd(pool+"/costs", "RecordConsolCost", "Record a cost line (of the consolidation or one container)"), s.RecordConsolCost)
	httpapi.Command(r, k, cmd(pool+"/costs/{line_no}/revise", "ReviseConsolCost", "Revise a cost line, with a reason"), s.ReviseConsolCost)
	httpapi.Command(r, k, cmd(pool+"/costs/{line_no}/release-accrual", "ReleasePoolAccrual", "Release a line's accrual"), s.ReleasePoolAccrual)
	httpapi.Command(r, k, cmd(pool+"/previews", "PreviewConsolCostApportionment", "Preview an apportionment of lines"), s.PreviewConsolCostApportionment)
	httpapi.Query(r, k, cost(read(pool+"/previews/{preview_no}/effect", "GetConsolCostPreviewEffect", "What a preview would change: fin:ReadProfit")), s.GetConsolCostPreviewEffect)
	httpapi.Command(r, k, cmd(pool+"/previews/{preview_no}/submit", "SubmitConsolCostApportionment", "Submit a preview (by another person when the settings ask it)"), s.SubmitConsolCostApportionment)
	httpapi.Command(r, k, cmd(pool+"/apportion-in-one-step", "ApportionConsolCostInOneStep", "Preview and submit at once, when the settings allow it"), s.ApportionConsolCostInOneStep)
	httpapi.Command(r, k, cmd(pool+"/close", "CloseConsolCostPool", "Close the pool"), s.CloseConsolCostPool)
	httpapi.Command(r, k, cmd(pool+"/reopen", "ReopenConsolCostPool", "Reopen the pool"), s.ReopenConsolCostPool)
	httpapi.Query(r, k, cost(read("/v1/fin/consolidations/{consolidation_id}/profit", "GetConsolProfit", "A consolidation's economics through its pool: fin:ReadProfit")), s.GetConsolProfit)

	// the cost variance threshold of a legal entity
	httpapi.Command(r, k, cmd(le+"/variance-thresholds", "SetCostVarianceThreshold", "Propose a cost variance threshold"), s.SetCostVarianceThreshold)
	httpapi.Command(r, k, cmd(le+"/variance-thresholds/{version}/approve", "ApproveCostVarianceThreshold", "Approve it (not by its setter; fin:ApproveSettings by name)"), s.ApproveCostVarianceThreshold)
	httpapi.Command(r, k, cmd(le+"/variance-thresholds/{version}/refuse", "RefuseCostVarianceThreshold", "Refuse it (fin:ApproveSettings by name)"), s.RefuseCostVarianceThreshold)

	// the settings: a change proposed, approved by another person, refused, cancelled; the self-approvals reviewed
	propose := cmd("/v1/fin/settings-changes", "ProposeFinSettingsChange", "Propose a settings change: fin:ConfigureSettings by name")
	propose.Status, propose.Deny = nethttp.StatusCreated, []string{"onboarding_id"}
	httpapi.Command(r, k, propose, s.ProposeFinSettingsChange)
	httpapi.Query(r, k, read("/v1/fin/settings-changes", "ListFinSettingsChanges", "The settings changes"), s.ListFinSettingsChanges)
	impact := checked(read(change+"/impact", "GetFinSettingsChangeImpact", "What a proposed change would change: figures only with fin:ReadProfit"))
	impact.Deny = []string{"entries"}
	httpapi.Query(r, k, impact, s.GetFinSettingsChangeImpact)
	httpapi.Command(r, k, cmd(change+"/approve", "ApproveFinSettingsChange", "Approve it (not by its proposer): fin:ApproveSettings by name"), s.ApproveFinSettingsChange)
	httpapi.Command(r, k, cmd(change+"/refuse", "RefuseFinSettingsChange", "Refuse it: fin:ApproveSettings by name"), s.RefuseFinSettingsChange)
	httpapi.Command(r, k, cmd(change+"/cancel", "CancelFinSettingsChange", "Cancel it: fin:ConfigureSettings or fin:ApproveSettings by name"), s.CancelFinSettingsChange)
	httpapi.Command(r, k, cmd(change+"/review-self-approval", "ReviewFinSelfApprovedSettingsChange", "Review a single administrator's own approval: fin:ReviewSettingsExceptions by name"), s.ReviewFinSelfApprovedSettingsChange)
	httpapi.Query(r, k, read("/v1/fin/settings", "GetFinSettings", "The settings in force for a scope (scope_kind, scope_id, as_of)"), s.GetFinSettings)
	httpapi.Query(r, k, read("/v1/fin/settings-catalogue", "GetFinSettingsCatalogue", "The catalogue of FIN's settings"), s.GetFinSettingsCatalogue)
	httpapi.Query(r, k, checked(read("/v1/fin/settings-exceptions", "GetFinExceptionReport", "The exception report: fin:ApproveSettings by name")), s.GetFinExceptionReport)
	httpapi.Query(r, k, checked(read("/v1/fin/settings-self-approvals", "ListFinSelfApprovalReviews", "Self-approvals and their reviews: fin:ReviewSettingsExceptions by name")), s.ListFinSelfApprovalReviews)

	// the after-the-fact review list of a legal entity, by week
	httpapi.Query(r, k, checked(read(le+"/after-the-fact/{week}", "GetAfterTheFactReview", "A week's list: fin:ReadProfit")), s.GetAfterTheFactReview)
	httpapi.Command(r, k, cmd(le+"/after-the-fact/{week}/reviewed", "MarkAfterTheFactReviewed", "Mark the week reviewed, with a conclusion (fin:ReviewAfterTheFact by name)"), s.MarkAfterTheFactReviewed)
	httpapi.Query(r, k, checked(read(le+"/after-the-fact-pending", "ListAfterTheFactPending", "Weeks that wait for review: fin:ReviewAfterTheFact by name")), s.ListAfterTheFactPending)
}
