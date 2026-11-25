package bootstrap

import (
	"context"
	"errors"
	"testing"

	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/foundation/identity"
)

// SEA's three actions of V2.100, ConfigureSettings, ApproveSettings and SuperviseCarrierConflicts, are granted by name
// only: "sea:*" and "*" grant every other action of SEA and none of these, and a name grants its own action and not the
// others.
func TestSeaSettingsPermissionsAreGrantedByNameOnly(t *testing.T) {
	as := func(permissions ...string) context.Context {
		return identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "t1", ActorID: "ann", Permissions: permissions})
	}
	for _, c := range []struct {
		name                           string
		ctx                            context.Context
		other, configure, approve, sup bool
	}{
		{"sea:*", as("sea:*"), true, false, false, false},
		{"*", as("*"), true, false, false, false},
		{"sea:ConfigureSettings", as("sea:ConfigureSettings"), false, true, false, false},
		{"sea:ApproveSettings", as("sea:ApproveSettings"), false, false, true, false},
		{"sea:SuperviseCarrierConflicts", as("sea:SuperviseCarrierConflicts"), false, false, false, true},
		{"the three names", as("sea:ConfigureSettings", "sea:ApproveSettings", "sea:SuperviseCarrierConflicts"), false, true, true, true},
		{"sea:* and a name", as("sea:*", "sea:SuperviseCarrierConflicts"), true, false, false, true},
		{"the other action by its own name", as("sea:AcceptCarrierLoadingConflict"), true, false, false, false},
		{"another context's names", as("fin:ConfigureSettings", "fin:*", "fin:ApproveSettings"), false, false, false, false},
		{"no permission", as(), false, false, false, false},
		{"no principal", context.Background(), false, false, false, false},
	} {
		for action, want := range map[string]bool{seaapi.ActionAcceptCarrierLoadingConflict: c.other, seaapi.ActionConfigureSettings: c.configure,
			seaapi.ActionApproveSettings: c.approve, seaapi.ActionSuperviseCarrierConflicts: c.sup} {
			err := permissions{}.AuthorizeSeaAction(c.ctx, "t1", "ann", action, "subject")
			if (err == nil) != want || (err != nil && !errors.Is(err, identity.ErrDenied)) {
				t.Errorf("%s, %s: %v, want allowed %v", c.name, action, err, want)
			}
		}
	}
	// the other two actions that are granted by name only stay so
	if err := (permissions{}).AuthorizeSeaAction(as("sea:*"), "t1", "ann", seaapi.ActionReadVgmSignatory, "x"); !errors.Is(err, identity.ErrDenied) {
		t.Errorf("sea:* grants a signatory's name: %v", err)
	}
	// the principal must be the tenant and the actor the command names, for a name as for any permission
	ctx := as("sea:ConfigureSettings", "sea:ApproveSettings", "sea:SuperviseCarrierConflicts")
	for _, action := range []string{seaapi.ActionConfigureSettings, seaapi.ActionApproveSettings, seaapi.ActionSuperviseCarrierConflicts} {
		if err := (permissions{}).AuthorizeSeaAction(ctx, "t2", "ann", action, "x"); !errors.Is(err, identity.ErrDenied) {
			t.Errorf("%s for another tenant: %v", action, err)
		}
		if err := (permissions{}).AuthorizeSeaAction(ctx, "t1", "bob", action, "x"); !errors.Is(err, identity.ErrDenied) {
			t.Errorf("%s for another actor: %v", action, err)
		}
	}
}
