package bootstrap

import (
	"context"
	"errors"
	"testing"

	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	"go.idp.xyz/idp-cargo/internal/foundation/identity"
)

// FIN's two actions on the settings are granted by name only: "fin:*" and "*" grant every other action of FIN and
// neither of these, and a name grants its own action and not the other.
func TestFinSettingsPermissionsAreGrantedByNameOnly(t *testing.T) {
	as := func(permissions ...string) context.Context {
		return identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "t1", ActorID: "ann", Permissions: permissions})
	}
	for _, c := range []struct {
		name      string
		ctx       context.Context
		other     bool
		configure bool
		approve   bool
	}{
		{"fin:*", as("fin:*"), true, false, false},
		{"*", as("*"), true, false, false},
		{"fin:ConfigureSettings", as("fin:ConfigureSettings"), false, true, false},
		{"fin:ApproveSettings", as("fin:ApproveSettings"), false, false, true},
		{"both names", as("fin:ConfigureSettings", "fin:ApproveSettings"), false, true, true},
		{"fin:* and a name", as("fin:*", "fin:ApproveSettings"), true, false, true},
		{"another context's name", as("sea:ConfigureSettings", "sea:*"), false, false, false},
		{"no permission", as(), false, false, false},
		{"no principal", context.Background(), false, false, false},
	} {
		for action, want := range map[string]bool{finapi.ActionOpenFinancePolicy: c.other, finapi.ActionConfigureSettings: c.configure,
			finapi.ActionApproveSettings: c.approve} {
			err := permissions{}.AuthorizeFinAction(c.ctx, "t1", "ann", action, "subject")
			if (err == nil) != want || (err != nil && !errors.Is(err, identity.ErrDenied)) {
				t.Errorf("%s, %s: %v, want allowed %v", c.name, action, err, want)
			}
		}
	}
	// the principal must be the tenant and the actor the command names, for a name as for any permission
	ctx := as("fin:ConfigureSettings", "fin:ApproveSettings")
	for _, action := range []string{finapi.ActionConfigureSettings, finapi.ActionApproveSettings} {
		if err := (permissions{}).AuthorizeFinAction(ctx, "t2", "ann", action, "x"); !errors.Is(err, identity.ErrDenied) {
			t.Errorf("%s for another tenant: %v", action, err)
		}
		if err := (permissions{}).AuthorizeFinAction(ctx, "t1", "bob", action, "x"); !errors.Is(err, identity.ErrDenied) {
			t.Errorf("%s for another actor: %v", action, err)
		}
	}
}
