// Package http is PHY's inbound HTTP adapter (since V2.108): each route binds the body or the query string to PHY's own
// api command or query, with the tenant and actor of the verified principal, and holds no business rules. PHY
// authorizes its commands itself, before it replays one; the reads are "phy:Read". A consolidation's, a cargo lot's
// and a load execution's ETag is its revision; a house's membership revision is in the body
// (expected_membership_revision), as PHY's commands take it. FUL's reads of a membership and of the verified load
// evidence, and FIN's cost basis, stay in process. After PHY attaches, detaches or transfers a house, or closes a
// stuffing, the client records it in FUL (T6).
package http

import (
	"net/http"

	"go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/foundation/httpapi"
)

const bc = "phy"

// Service is what the routes call.
type Service interface {
	api.Commands
	api.PhysicalCommands
	api.Queries
}

// Kinds maps PHY's error kinds to statuses.
var Kinds = httpapi.Errors{Unauthenticated: api.ErrUnauthenticated, Unauthorized: api.ErrUnauthorized, NotFound: api.ErrNotFound,
	Conflict: api.ErrConflict, IdempotencyConflict: api.ErrIdempotencyConflict, Rejected: api.ErrRejected}

// Register registers PHY's routes.
func Register(r *httpapi.Routes, s Service) {
	k := Kinds
	cmd := func(path, calls, summary string) httpapi.Op {
		return httpapi.Op{Method: "POST", Path: path, Context: bc, Calls: calls, Action: calls, Summary: summary}
	}
	read := func(path, calls, summary string) httpapi.Op {
		return httpapi.Op{Method: "GET", Path: path, Context: bc, Calls: calls, Summary: summary}
	}
	created := func(o httpapi.Op) httpapi.Op { o.Status = http.StatusCreated; return o }
	const c, h = "/v1/consolidations/{consolidation_id}", "/v1/consolidations/{consolidation_id}/houses/{shipment_id}"
	const lot, x = "/v1/cargo-lots/{lot_id}", "/v1/load-executions/{execution_id}"

	// consolidations
	httpapi.Command(r, k, created(cmd("/v1/consolidations", "CreateConsolidation", "Create a consolidation")), s.CreateConsolidation)
	httpapi.Query(r, k, read("/v1/consolidations", "ListOpenConsolidations", "Open consolidations, for attaching houses (origin and destination as CNSHA or IATA:PVG)"), s.ListOpenConsolidations)
	httpapi.Query(r, k, read(c, "GetConsolidation", "A consolidation: its houses, load units and status"), s.GetConsolidation)
	httpapi.Command(r, k, cmd(c+"/amend", "AmendConsolidation", "Amend the route, departure or cut-off"), s.AmendConsolidation)
	httpapi.Command(r, k, cmd(c+"/close", "CloseConsolidation", "Close it to new houses"), s.CloseConsolidation)
	httpapi.Command(r, k, cmd(c+"/reopen", "ReopenConsolidation", "Reopen it"), s.ReopenConsolidation)
	httpapi.Command(r, k, cmd(c+"/cancel", "CancelConsolidation", "Cancel it"), s.CancelConsolidation)
	httpapi.Command(r, k, cmd(c+"/reconcile", "ReconcileConsolidation", "Reconcile it once its executions are closed"), s.ReconcileConsolidation)
	httpapi.Command(r, k, cmd(c+"/load-units", "PlanLoadUnit", "Plan a load unit (a container, a ULD)"), s.PlanLoadUnit)
	httpapi.Command(r, k, cmd(c+"/load-units/{unit_no}/remove", "RemovePlannedLoadUnit", "Remove a planned load unit"), s.RemovePlannedLoadUnit)
	httpapi.Command(r, k, cmd(c+"/houses", "AttachHouse", "Attach a house (then record it in FUL)"), s.AttachHouse)
	httpapi.Command(r, k, cmd(h+"/detach", "DetachHouse", "Detach a house (then record it in FUL)"), s.DetachHouse)
	httpapi.Command(r, k, cmd(h+"/transfer", "TransferHouse", "Transfer a house to another consolidation (then record it in FUL)"), s.TransferHouse)
	httpapi.Command(r, k, cmd(h+"/refresh", "RefreshHouseIntent", "Refresh a house's intent from FUL's latest plan"), s.RefreshHouseIntent)
	httpapi.Command(r, k, cmd(h+"/end-cancelled", "EndMembershipForCancelledShipment", "End the membership of a shipment FUL cancelled"), s.EndMembershipForCancelledShipment)

	// cargo lots: receiving at the warehouse
	httpapi.Command(r, k, cmd(lot+"/receipts", "ReceiveCargo", "Receive cargo into a lot (If-Match \"0\" for a new lot)"), s.ReceiveCargo)
	httpapi.Query(r, k, read(lot, "GetCargoLot", "A cargo lot: received, measured, screened, discrepancies"), s.GetCargoLot)
	httpapi.Command(r, k, cmd(lot+"/measurements", "RecordCargoMeasurement", "Record the measured weight and volume"), s.RecordCargoMeasurement)
	httpapi.Command(r, k, cmd(lot+"/screenings", "RecordScreeningResult", "Record a security screening result"), s.RecordScreeningResult)
	httpapi.Command(r, k, cmd(lot+"/complete", "CompleteReceipt", "Complete the receipt"), s.CompleteReceipt)
	httpapi.Command(r, k, cmd(lot+"/discrepancies/{discrepancy_no}/resolve", "ResolveCargoDiscrepancy", "Resolve a receiving discrepancy"), s.ResolveCargoDiscrepancy)
	httpapi.Command(r, k, cmd(lot+"/close", "CloseCargoLot", "Close the lot once all its packs are loaded"), s.CloseCargoLot)

	// load executions: stuffing a container, building a ULD
	httpapi.Command(r, k, created(cmd("/v1/load-executions", "OpenLoadExecution", "Open a stuffing or build-up of a load unit")), s.OpenLoadExecution)
	httpapi.Query(r, k, read(x, "GetLoadExecution", "A load execution: its lines, holds, seal and evidence"), s.GetLoadExecution)
	httpapi.Command(r, k, cmd(x+"/start", "StartLoading", "Start loading"), s.StartLoading)
	httpapi.Command(r, k, cmd(x+"/loaded", "RecordCargoLoaded", "Record packs of a lot loaded"), s.RecordCargoLoaded)
	httpapi.Command(r, k, cmd(x+"/unloaded", "RecordCargoUnloaded", "Record packs of a line unloaded"), s.RecordCargoUnloaded)
	httpapi.Command(r, k, cmd(x+"/holds", "PlaceLoadExecutionOnHold", "Place it on hold"), s.PlaceLoadExecutionOnHold)
	httpapi.Command(r, k, cmd(x+"/holds/{hold_no}/release", "ReleaseLoadExecutionHold", "Release a hold"), s.ReleaseLoadExecutionHold)
	httpapi.Command(r, k, cmd(x+"/close", "CloseLoadExecution", "Close it: the container sealed, the ULD built (then FUL takes the evidence)"), s.CloseLoadExecution)
	httpapi.Command(r, k, cmd(x+"/cancel", "CancelLoadExecution", "Cancel it"), s.CancelLoadExecution)
	httpapi.Command(r, k, cmd(x+"/rework", "OpenStuffingForRework", "Open a sealed container for rework, with a reason"), s.OpenStuffingForRework)
	httpapi.Command(r, k, cmd(x+"/rework/unload", "UnloadHouseForRework", "Unload a whole house in a rework"), s.UnloadHouseForRework)
	httpapi.Command(r, k, cmd(x+"/carrier-evidence", "RecordCarrierLoadEvidence", "Record the carrier's evidence of the load"), s.RecordCarrierLoadEvidence)
	httpapi.Command(r, k, cmd(x+"/discrepancies/{discrepancy_no}/resolve", "ResolveLoadingDiscrepancy", "Resolve a loading discrepancy"), s.ResolveLoadingDiscrepancy)
	httpapi.Query(r, k, read("/v1/discrepancies", "ListOpenDiscrepancies", "Open discrepancies of a consolidation or a shipment"), s.ListOpenDiscrepancies)
}
