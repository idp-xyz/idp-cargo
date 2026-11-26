// Package http is COM's inbound HTTP adapter (since V2.107): each route binds the body or the query string to COM's own
// api command or query (httpapi.Command, httpapi.Query), with the tenant and actor of the verified principal, and holds
// no business rules. COM authorizes its commands itself, before it replays one; the reads are "com:Read", and COM
// authorizes a read of contacts itself ("com:ReadOrderContacts", and once the order is closed
// "com:ReadRestrictedOrderContacts", by name only). An order's ETag is its revision. The obligation snapshots stay
// FUL's reads in process.
package http

import (
	"net/http"

	"go.idp.xyz/idp-cargo/internal/contexts/com/api"
	"go.idp.xyz/idp-cargo/internal/foundation/httpapi"
)

const bc = "com"

// Service is what the routes call.
type Service interface {
	api.Commands
	api.OrderQueries
	api.OrderPolicies
	api.ObligationReservations
	api.OrderPartyCommands
	api.OrderPartyQueries
	api.OrderContactRetention
	api.ContactAccessQueries
}

// Kinds maps COM's error kinds to statuses.
var Kinds = httpapi.Errors{Unauthenticated: api.ErrUnauthenticated, Unauthorized: api.ErrUnauthorized, NotFound: api.ErrNotFound,
	Conflict: api.ErrConflict, IdempotencyConflict: api.ErrIdempotencyConflict, Rejected: api.ErrRejected}

// Register registers COM's routes.
func Register(r *httpapi.Routes, s Service) {
	k := Kinds
	op := func(method, path, calls, action, summary string) httpapi.Op {
		return httpapi.Op{Method: method, Path: path, Context: bc, Calls: calls, Action: action, Summary: summary}
	}
	created := func(o httpapi.Op) httpapi.Op { o.Status = http.StatusCreated; return o }
	checked := func(o httpapi.Op) httpapi.Op { o.ContextChecks, o.Action = true, ""; return o }

	httpapi.Command(r, k, created(op("POST", "/v1/orders", "RecordCustomerOrder", api.ActionRecordCustomerOrder, "Record a customer order on an accepted quote")), s.RecordCustomerOrder)
	httpapi.Query(r, k, op("GET", "/v1/orders/{order_id}", "GetCustomerOrder", "", "An order: lines, obligations, reservations, transport, status"), s.GetCustomerOrder)
	httpapi.Command(r, k, op("POST", "/v1/orders/{order_id}/lines", "AddOrderLine", api.ActionAddOrderLine, "Add a line"), s.AddOrderLine)
	httpapi.Command(r, k, op("POST", "/v1/orders/{order_id}/lines/{line_no}/amend", "AmendOrderLine", api.ActionAmendOrderLine, "Amend a line, with a reason"), s.AmendOrderLine)
	httpapi.Command(r, k, op("POST", "/v1/orders/{order_id}/obligations", "AllocateOrderToObligation", api.ActionAllocateOrderToObligation, "Allocate packs of a line to an obligation"), s.AllocateOrderToObligation)
	httpapi.Command(r, k, op("POST", "/v1/orders/{order_id}/obligations/{obligation_id}/amend", "AmendObligation", api.ActionAmendObligation, "Amend an obligation as a new one"), s.AmendObligation)
	httpapi.Command(r, k, op("POST", "/v1/orders/{order_id}/obligations/{obligation_id}/revoke", "RevokeObligation", api.ActionRevokeObligation, "Revoke an obligation"), s.RevokeObligation)
	httpapi.Command(r, k, op("POST", "/v1/orders/{order_id}/cancel", "CancelCustomerOrder", api.ActionCancelCustomerOrder, "Cancel an order"), s.CancelCustomerOrder)
	httpapi.Command(r, k, op("POST", "/v1/orders/{order_id}/close", "CloseCustomerOrder", api.ActionCloseCustomerOrder, "Close an order"), s.CloseCustomerOrder)
	httpapi.Query(r, k, op("GET", "/v1/orders/{order_id}/parties", "GetOrderParties", "", "The order's parties; with_contacts also needs com:ReadOrderContacts by name, and a purpose"), s.GetOrderParties)
	httpapi.Command(r, k, op("POST", "/v1/orders/{order_id}/parties", "SetOrderParty", api.ActionSetOrderParty, "Set a party of the order (shipper, consignee, notify...)"), s.SetOrderParty)
	httpapi.Command(r, k, op("POST", "/v1/orders/{order_id}/transport", "ChangeOrderTransport", api.ActionChangeOrderTransport, "Change the transport requested"), s.ChangeOrderTransport)
	httpapi.Command(r, k, op("POST", "/v1/orders/{order_id}/party-locks", "LockOrderParties", api.ActionLockOrderParties, "Lock the parties for a document (before DOC issues it)"), s.LockOrderParties)
	httpapi.Command(r, k, op("POST", "/v1/orders/{order_id}/party-locks/release", "ReleaseOrderPartyLock", api.ActionReleaseOrderPartyLock, "Release a document's lock"), s.ReleaseOrderPartyLock)
	httpapi.Command(r, k, op("POST", "/v1/orders/{order_id}/erase-contacts", "EraseOrderContacts", api.ActionEraseOrderContacts, "Erase the order's contacts once retention allows (by name only)"), s.EraseOrderContacts)
	httpapi.Query(r, k, checked(op("GET", "/v1/orders/{order_id}/contact-access", "ListContactAccess", "", "Who read the order's contacts: com:ReadContactAccessLog by name")), s.ListContactAccess)
	httpapi.Query(r, k, checked(op("GET", "/v1/order-contact-erasures", "ListOrdersDueForContactErasure", "", "Orders whose contacts are due for erasure: com:EraseOrderContacts by name")), s.ListOrdersDueForContactErasure)
	httpapi.Command(r, k, op("POST", "/v1/order-policies/{legal_entity_id}", "SetOrderPolicy", api.ActionSetOrderPolicy, "Set a legal entity's order policy"), s.SetOrderPolicy)
	httpapi.Query(r, k, op("GET", "/v1/order-policies/{legal_entity_id}", "GetOrderPolicy", "", "A legal entity's order policy"), s.GetOrderPolicy)
	httpapi.Command(r, k, op("POST", "/v1/obligations/{obligation_id}/reservations", "ReserveObligationForFulfillment", api.ActionReserveObligationForFulfillment, "Reserve an obligation version for a shipment (before FUL plans it)"), s.ReserveObligationForFulfillment)
	httpapi.Command(r, k, op("POST", "/v1/obligations/{obligation_id}/reservations/release", "ReleaseObligationReservation", api.ActionReleaseObligationReservation, "Release a shipment's reservation"), s.ReleaseObligationReservation)
}
