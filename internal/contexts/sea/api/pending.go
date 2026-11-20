package api

import "context"

// SEA V2.88: what stands between a movement and its finalization. SEA refuses at its external commitments (Finalize)
// and at physical rules, and records the rest: a refresh or an association writes what it can, and what a downstream
// rule cannot take becomes a pending item. FUL and SEA are still read by hand, so the list says what to re-read or
// record.

// GetPendingItems lists a movement's pending items. It is read-only and reads FUL's plans of the live houses and
// whether FUL's evidence of each is PHY's current one, as FinalizeOceanDocFacts does, outside any transaction. A
// cancelled movement has none. The blocking items are those FinalizeOceanDocFacts refuses on, in the order it names
// them.
type GetPendingItems struct{ TenantID, MovementID string }

// PendingItem is one pending item. Kind is the code below; Scope is ScopeHouse or ScopeContainer and SubjectID the
// shipment or the container's number; Message says who does what; Blocking says whether FinalizeOceanDocFacts refuses
// while it is open. A warning (Blocking false) asks for a check and blocks nothing.
type PendingItem struct {
	Kind      string
	Scope     string
	SubjectID string
	Message   string
	Blocking  bool
}

// PendingQueries is SEA's read side for pending items. An unknown movement yields ErrNotFound.
type PendingQueries interface {
	GetPendingItems(ctx context.Context, q GetPendingItems) ([]PendingItem, error)
}

// Scopes and kinds of pending items.
const (
	PendingScopeHouse     = "HOUSE"
	PendingScopeContainer = "CONTAINER"

	PendingHousePlanNotRead      = "HOUSE_PLAN_NOT_READ"
	PendingHouseNotReported      = "HOUSE_NOT_REPORTED"
	PendingFULBehind             = "FUL_BEHIND_PHY"
	PendingHouseStale            = "HOUSE_STALE"
	PendingContainerNoSeal       = "CONTAINER_NO_SEAL"
	PendingHousesDisagree        = "HOUSES_DISAGREE_ON_SEAL"
	PendingSealNotOnContainer    = "PHY_SEAL_NOT_ON_CONTAINER"
	PendingLaterShipperSeal      = "LATER_SHIPPER_SEAL"
	PendingGatedBeforeReseal     = "GATED_IN_BEFORE_RESEAL"
	PendingNoVGM                 = "NO_VGM_BY_CUTOFF"
	PendingVGMBeforeReseal       = "VGM_BEFORE_RESEAL"
	PendingHouseNotStuffed       = "HOUSE_NOT_STUFFED"
	PendingHouseUnknownContainer = "HOUSE_NAMES_UNKNOWN_CONTAINER"
	PendingHousePacks            = "HOUSE_PACKS_NOT_STUFFED"

	// Warnings.
	WarningVGMEnteredBeforeSealKnown = "VGM_ENTERED_BEFORE_SEAL_KNOWN"
	WarningResealDatedBeforeStuffing = "RESEAL_DATED_BEFORE_STUFFING"
)
