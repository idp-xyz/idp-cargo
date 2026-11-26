// Package http is DOC's inbound HTTP adapter (since V2.109): each route binds the body or the query string to DOC's own
// api command or query, with the tenant and actor of the verified principal, and holds no business rules. DOC
// authorizes its commands itself, before it replays one; the reads are "doc:Read", except the dispatch copy (it
// carries contacts, read with the actor's own COM and ORG rights) and the contact provision records
// ("doc:ReadProvisionRecords", by name only), which DOC authorizes itself. A document's ETag is its revision. A
// rendered version and a dispatch copy answer the PDF as base64 in JSON. FIN's snapshot of a house's bill stays in
// process. Before issuing, the client locks the order's parties in COM; before a dispatch it reserves the contact
// transfers in ORG (T6).
package http

import (
	"net/http"

	"go.idp.xyz/idp-cargo/internal/contexts/doc/api"
	"go.idp.xyz/idp-cargo/internal/foundation/httpapi"
)

const bc = "doc"

// Service is what the routes call.
type Service interface {
	api.Commands
	api.Queries
	api.PendingQueries
	api.CarrierCommands
	api.CarrierQueries
	api.DispatchCommands
	api.DispatchQueries
}

// Kinds maps DOC's error kinds to statuses.
var Kinds = httpapi.Errors{Unauthenticated: api.ErrUnauthenticated, Unauthorized: api.ErrUnauthorized, NotFound: api.ErrNotFound,
	Conflict: api.ErrConflict, IdempotencyConflict: api.ErrIdempotencyConflict, Rejected: api.ErrRejected}

// Register registers DOC's routes.
func Register(r *httpapi.Routes, s Service) {
	k := Kinds
	cmd := func(path, calls, summary string) httpapi.Op {
		return httpapi.Op{Method: "POST", Path: path, Context: bc, Calls: calls, Action: calls, Summary: summary}
	}
	read := func(path, calls, summary string) httpapi.Op {
		return httpapi.Op{Method: "GET", Path: path, Context: bc, Calls: calls, Summary: summary}
	}
	created := func(o httpapi.Op) httpapi.Op { o.Status = http.StatusCreated; return o }
	checked := func(o httpapi.Op) httpapi.Op { o.ContextChecks, o.Action = true, ""; return o }
	const d, carrier, dispatch = "/v1/documents/{document_id}", "/v1/carrier-documents/{document_id}", "/v1/dispatches/{dispatch_id}"

	// transport documents: HAWB, HBL, house sea waybill, MAWB
	httpapi.Command(r, k, created(cmd("/v1/documents", "DraftTransportDocument", "Draft a transport document from the movement's facts")), s.DraftTransportDocument)
	httpapi.Query(r, k, read(d, "GetTransportDocument", "A document: its state, number and versions"), s.GetTransportDocument)
	httpapi.Query(r, k, read(d+"/versions/{version}", "GetDocumentVersion", "A version's content (0: the latest)"), s.GetDocumentVersion)
	httpapi.Query(r, k, read(d+"/versions/{version}/pdf", "RenderDocumentVersion", "A version rendered (PDF as base64), without contacts"), s.RenderDocumentVersion)
	httpapi.Query(r, k, read("/v1/shipments/{shipment_id}/documents", "ListDocumentsForShipment", "A shipment's documents"), s.ListDocumentsForShipment)
	httpapi.Query(r, k, read("/v1/consolidations/{consolidation_id}/documents", "ListDocumentsForConsolidation", "A consolidation's documents"), s.ListDocumentsForConsolidation)
	httpapi.Command(r, k, cmd(d+"/refresh", "RefreshDocumentDraft", "Refresh the draft from the latest facts (a rolled house follows its container)"), s.RefreshDocumentDraft)
	httpapi.Command(r, k, cmd(d+"/approve", "ApproveTransportDocument", "Approve the draft (one's own draft also needs doc:ApproveOwnTransportDocument)"), s.ApproveTransportDocument)
	httpapi.Command(r, k, cmd(d+"/issue", "IssueTransportDocument", "Issue it (overriding a credit hold needs doc:OverrideCreditCheck)"), s.IssueTransportDocument)
	httpapi.Command(r, k, cmd(d+"/amendments", "OpenDocumentAmendment", "Open an amendment of an issued document"), s.OpenDocumentAmendment)
	httpapi.Command(r, k, cmd(d+"/amendments/issue", "IssueDocumentAmendment", "Issue the amendment under the same number"), s.IssueDocumentAmendment)
	httpapi.Command(r, k, cmd(d+"/amendments/abandon", "AbandonDocumentAmendment", "Abandon the amendment"), s.AbandonDocumentAmendment)
	httpapi.Command(r, k, cmd(d+"/originals/return", "ReturnOriginal", "Record an original returned"), s.ReturnOriginal)
	httpapi.Command(r, k, cmd(d+"/originals/surrender", "SurrenderOriginals", "Record the originals surrendered at origin"), s.SurrenderOriginals)
	httpapi.Query(r, k, read(d+"/originals", "ListOriginals", "The originals and where each is"), s.ListOriginals)
	httpapi.Command(r, k, cmd(d+"/cancel", "CancelTransportDocument", "Cancel it"), s.CancelTransportDocument)
	httpapi.Query(r, k, read(d+"/pending", "GetPendingItemsOfDocument", "What the document waits for"), s.GetPendingItemsOfDocument)
	httpapi.Query(r, k, read("/v1/movements/{movement_id}/pending-documents", "GetPendingItemsOfMovement", "What a movement's documents wait for"), s.GetPendingItemsOfMovement)

	// issuance authorities
	httpapi.Command(r, k, cmd("/v1/issuance-authorities", "RecordIssuanceAuthority", "Record a legal entity's authority to issue a kind of document"), s.RecordIssuanceAuthority)
	httpapi.Query(r, k, read("/v1/issuance-authorities/{authority_id}", "GetIssuanceAuthority", "An issuance authority"), s.GetIssuanceAuthority)
	httpapi.Command(r, k, cmd("/v1/issuance-authorities/{authority_id}/withdraw", "WithdrawIssuanceAuthority", "Withdraw an issuance authority"), s.WithdrawIssuanceAuthority)

	// the carrier's documents: an MBL as the line issued it
	httpapi.Command(r, k, cmd("/v1/carrier-documents", "RecordCarrierDocument", "Record the carrier's document, with its evidence"), s.RecordCarrierDocument)
	httpapi.Query(r, k, read(carrier, "GetCarrierDocument", "A carrier's document"), s.GetCarrierDocument)
	httpapi.Query(r, k, read("/v1/movements/{movement_id}/carrier-document", "FindCarrierDocumentByMovement", "A movement's carrier document"), s.FindCarrierDocumentByMovement)
	httpapi.Command(r, k, cmd(carrier+"/verify", "VerifyCarrierDocument", "Verify it (not by who recorded it)"), s.VerifyCarrierDocument)
	httpapi.Command(r, k, cmd(carrier+"/cancel", "CancelCarrierDocument", "Cancel it"), s.CancelCarrierDocument)

	// dispatches: sending a document to a recipient
	httpapi.Command(r, k, cmd("/v1/dispatches", "PrepareDocumentDispatch", "Prepare a dispatch of an issued version (after ORG's reservation)"), s.PrepareDocumentDispatch)
	httpapi.Query(r, k, read(dispatch, "GetDispatch", "A dispatch"), s.GetDispatch)
	httpapi.Query(r, k, checked(read(dispatch+"/copy", "GetDispatchCopy", "The copy sent, with its contacts (the actor's COM and ORG rights)")), s.GetDispatchCopy)
	httpapi.Command(r, k, cmd(dispatch+"/outcome", "RecordDispatchOutcome", "Record what became of the dispatch"), s.RecordDispatchOutcome)
	httpapi.Query(r, k, read(d+"/dispatches", "ListDocumentDispatches", "A document's dispatches"), s.ListDocumentDispatches)
	httpapi.Query(r, k, checked(read("/v1/contact-provisions", "ListContactProvisions", "Contacts provided abroad in a window: doc:ReadProvisionRecords by name")), s.ListContactProvisions)
}
