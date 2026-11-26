package http

import (
	nethttp "net/http"

	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/foundation/httpapi"
)

// The generic routes of SEA (since V2.108), beside its hand-written ones: a container rolled to another movement
// (V2.105), a carrier's loading conflict accepted, a movement's pending items, and SEA's settings. SEA authorizes its
// commands itself, before it replays one: the settings with the permissions by name ("sea:ConfigureSettings",
// "sea:ApproveSettings", "sea:ReviewSettingsExceptions"), a change set held for an onboarding being the platform's
// (its onboarding_id is refused here). The reads are "sea:Read", except the exception report and the self-approval
// reviews, which SEA authorizes by the reader.

// Generic is what the generic routes call.
type Generic interface {
	api.VisitCommands
	api.PendingQueries
	api.SettingsCommands
	api.SettingsQueries
}

// RegisterGeneric registers SEA's generic routes.
func RegisterGeneric(r *httpapi.Routes, s Generic) {
	k := kinds
	cmd := func(path, calls, summary string) httpapi.Op {
		return httpapi.Op{Method: "POST", Path: path, Context: bc, Calls: calls, Action: calls, Summary: summary}
	}
	read := func(path, calls, summary string) httpapi.Op {
		return httpapi.Op{Method: "GET", Path: path, Context: bc, Calls: calls, Summary: summary}
	}
	checked := func(o httpapi.Op) httpapi.Op { o.ContextChecks, o.Action = true, ""; return o }
	const change = "/v1/sea-settings-changes/{change_id}"

	httpapi.Command(r, k, cmd("/v1/container-visits/{visit_id}/roll", "RollContainerToMovement", "Roll a sealed container not on board to another movement of its consolidation, with a reason"), s.RollContainerToMovement)
	httpapi.Command(r, k, cmd("/v1/container-visits/{visit_id}/accept-carrier-conflict", "AcceptCarrierLoadingConflict", "Release a carrier's loading report that contradicts the reseal, with a reason"), s.AcceptCarrierLoadingConflict)
	httpapi.Query(r, k, read("/v1/ocean-movements/{movement_id}/pending", "GetPendingItems", "What a movement waits for"), s.GetPendingItems)

	propose := cmd("/v1/sea-settings-changes", "ProposeSeaSettingsChange", "Propose a settings change: sea:ConfigureSettings by name")
	propose.Status, propose.Deny = nethttp.StatusCreated, []string{"onboarding_id"}
	httpapi.Command(r, k, propose, s.ProposeSeaSettingsChange)
	httpapi.Query(r, k, read("/v1/sea-settings-changes", "ListSeaSettingsChanges", "The settings changes"), s.ListSeaSettingsChanges)
	impact := read(change+"/impact", "GetSeaSettingsChangeImpact", "What a proposed change would change")
	impact.Deny = []string{"entries"}
	httpapi.Query(r, k, impact, s.GetSeaSettingsChangeImpact)
	httpapi.Command(r, k, cmd(change+"/approve", "ApproveSeaSettingsChange", "Approve it (not by its proposer): sea:ApproveSettings by name"), s.ApproveSeaSettingsChange)
	httpapi.Command(r, k, cmd(change+"/refuse", "RefuseSeaSettingsChange", "Refuse it: sea:ApproveSettings by name"), s.RefuseSeaSettingsChange)
	httpapi.Command(r, k, cmd(change+"/cancel", "CancelSeaSettingsChange", "Cancel it"), s.CancelSeaSettingsChange)
	httpapi.Command(r, k, cmd(change+"/review-self-approval", "ReviewSeaSelfApprovedSettingsChange", "Review a single administrator's own approval: sea:ReviewSettingsExceptions by name"), s.ReviewSeaSelfApprovedSettingsChange)
	httpapi.Query(r, k, read("/v1/sea-settings", "GetSeaSettings", "The settings in force for a scope"), s.GetSeaSettings)
	httpapi.Query(r, k, read("/v1/sea-settings-catalogue", "GetSeaSettingsCatalogue", "The catalogue of SEA's settings"), s.GetSeaSettingsCatalogue)
	httpapi.Query(r, k, checked(read("/v1/sea-settings-exceptions", "GetSeaExceptionReport", "The exception report: sea:ApproveSettings by name")), s.GetSeaExceptionReport)
	httpapi.Query(r, k, checked(read("/v1/sea-settings-self-approvals", "ListSeaSelfApprovalReviews", "Self-approvals and their reviews: sea:ReviewSettingsExceptions by name")), s.ListSeaSelfApprovalReviews)
}
