// Package http is FUL's inbound HTTP adapter (since V2.107): each route binds the body or the query string to FUL's own
// api command or query, with the tenant and actor of the verified principal, and holds no business rules. FUL
// authorizes its commands itself, before it replays one; the read of a shipment is "ful:Read". A shipment's ETag is
// its revision. The operation plans, evidence currency, load intent, cost driver and the first plan snapshot stay the
// other contexts' reads in process.
package http

import (
	"net/http"

	"go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	"go.idp.xyz/idp-cargo/internal/foundation/httpapi"
)

const bc = "ful"

// Service is what the routes call.
type Service interface {
	api.Commands
	api.ConsolAttachmentCommands
	api.LoadEvidenceCommands
	api.Queries
}

// Kinds maps FUL's error kinds to statuses.
var Kinds = httpapi.Errors{Unauthenticated: api.ErrUnauthenticated, Unauthorized: api.ErrUnauthorized, NotFound: api.ErrNotFound,
	Conflict: api.ErrConflict, IdempotencyConflict: api.ErrIdempotencyConflict, Rejected: api.ErrRejected}

// Register registers FUL's routes.
func Register(r *httpapi.Routes, s Service) {
	k := Kinds
	op := func(method, path, calls, action, summary string) httpapi.Op {
		return httpapi.Op{Method: method, Path: path, Context: bc, Calls: calls, Action: action, Summary: summary}
	}
	create := op("POST", "/v1/shipments", "CreateShipmentFromObligation", api.ActionCreateShipmentFromObligation, "Create a shipment from a reserved obligation")
	create.Status = http.StatusCreated
	httpapi.Command(r, k, create, s.CreateShipmentFromObligation)
	httpapi.Query(r, k, op("GET", "/v1/shipments/{shipment_id}", "GetShipmentPlanSnapshotV2", "", "A shipment: its plan, parts and consolidation"), s.GetShipmentPlanSnapshotV2)
	httpapi.Command(r, k, op("POST", "/v1/shipments/{shipment_id}/obligations", "AddObligationToShipment", api.ActionAddObligationToShipment, "Add a reserved obligation"), s.AddObligationToShipment)
	httpapi.Command(r, k, op("POST", "/v1/shipments/{shipment_id}/obligations/release", "ReleaseObligationFromShipment", api.ActionReleaseObligationFromShipment, "Release an obligation (then release COM's reservation)"), s.ReleaseObligationFromShipment)
	httpapi.Command(r, k, op("POST", "/v1/shipments/{shipment_id}/parts", "ChangeShipmentPart", api.ActionChangeShipmentPart, "Change the part of an obligation the shipment plans"), s.ChangeShipmentPart)
	httpapi.Command(r, k, op("POST", "/v1/shipments/{shipment_id}/plan", "AmendShipmentPlan", api.ActionAmendShipmentPlan, "Amend the plan: mode, route, window"), s.AmendShipmentPlan)
	httpapi.Command(r, k, op("POST", "/v1/shipments/{shipment_id}/confirm", "ConfirmShipmentPlan", api.ActionConfirmShipmentPlan, "Confirm the plan"), s.ConfirmShipmentPlan)
	httpapi.Command(r, k, op("POST", "/v1/shipments/{shipment_id}/cancel", "CancelShipment", api.ActionCancelShipment, "Cancel the shipment"), s.CancelShipment)
	httpapi.Command(r, k, op("POST", "/v1/shipments/{shipment_id}/consol-attachments", "RecordConsolAttachment", api.ActionRecordConsolAttachment, "Record PHY's attachment to a consolidation (after PHY attached it)"), s.RecordConsolAttachment)
	httpapi.Command(r, k, op("POST", "/v1/shipments/{shipment_id}/consol-detachments", "RecordConsolDetachment", api.ActionRecordConsolDetachment, "Record PHY's detachment (after PHY detached or transferred it)"), s.RecordConsolDetachment)
	httpapi.Command(r, k, op("POST", "/v1/shipments/{shipment_id}/load-evidence", "RecordVerifiedLoadEvidence", api.ActionRecordVerifiedLoadEvidence, "Take PHY's latest verified load evidence"), s.RecordVerifiedLoadEvidence)
}
