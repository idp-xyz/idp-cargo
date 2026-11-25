package sea

import (
	"testing"

	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/contexts/sea/internal/application"
	"go.idp.xyz/idp-cargo/internal/contexts/sea/internal/domain"
)

// The names the application and the domain keep for the published ones are the published ones: the permission the
// release of a carrier's contradiction asks for under SUPERVISOR_ONLY, the item and the values of S1.
func TestTheSettingsNamesAreThePublishedOnes(t *testing.T) {
	if string(application.ActionSuperviseCarrierConflicts) != api.ActionSuperviseCarrierConflicts {
		t.Errorf("the supervisor permission: %q and %q", application.ActionSuperviseCarrierConflicts, api.ActionSuperviseCarrierConflicts)
	}
	if application.SettingS1 != api.SettingS1 || domain.S1SupervisorOnly != api.S1SupervisorOnly || domain.S1WithReason != api.S1WithReason {
		t.Errorf("S1: %q %q %q", application.SettingS1, domain.S1SupervisorOnly, domain.S1WithReason)
	}
}

func TestWhoReleasesAConflictUnderEachValueOfS1(t *testing.T) {
	if releaseBy(domain.S1SupervisorOnly) != api.ReleaseBySupervisor || releaseBy(domain.S1WithReason) != api.ReleaseByAnyOperator ||
		releaseBy("") != api.ReleaseByAnyOperator {
		t.Fatal("who may release")
	}
}
