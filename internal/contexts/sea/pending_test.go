package sea

import (
	"testing"

	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/contexts/sea/internal/domain"
)

// The published pending kinds and scopes are the model's.
func TestPublishedPendingKindsAreTheModels(t *testing.T) {
	for _, pair := range [][2]string{
		{api.PendingScopeHouse, domain.ScopeHouse}, {api.PendingScopeContainer, domain.ScopeContainer},
		{api.PendingHousePlanNotRead, domain.PendHousePlanNotRead}, {api.PendingHouseNotReported, domain.PendHouseNotReported},
		{api.PendingFULBehind, domain.PendFULBehind}, {api.PendingHouseStale, domain.PendHouseStale},
		{api.PendingContainerNoSeal, domain.PendContainerNoSeal}, {api.PendingHousesDisagree, domain.PendHousesDisagree},
		{api.PendingSealNotOnContainer, domain.PendSealNotOnContainer}, {api.PendingLaterShipperSeal, domain.PendLaterShipperSeal},
		{api.PendingGatedBeforeReseal, domain.PendGatedBeforeReseal}, {api.PendingNoVGM, domain.PendNoVGM},
		{api.PendingVGMBeforeReseal, domain.PendVGMBeforeReseal}, {api.PendingHouseNotStuffed, domain.PendHouseNotStuffed},
		{api.PendingHouseUnknownContainer, domain.PendHouseUnknownContainer}, {api.PendingHousePacks, domain.PendHousePacks},
		{api.WarningVGMEnteredBeforeSealKnown, domain.WarnVGMEnteredBeforeSealKnown}, {api.WarningResealDatedBeforeStuffing, domain.WarnResealDatedBeforeStuffing},
	} {
		if pair[0] != pair[1] {
			t.Errorf("api %q, model %q", pair[0], pair[1])
		}
	}
}
