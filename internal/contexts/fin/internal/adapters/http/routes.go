package http

import (
	nethttp "net/http"

	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	"go.idp.xyz/idp-cargo/internal/foundation/httpapi"
)

// The generic routes of FIN (since V2.110), beside the six hand-written ones of V2.99 that EII's contract (docs/api/
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
}
