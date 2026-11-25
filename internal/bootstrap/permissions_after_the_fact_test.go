package bootstrap

import (
	"context"
	"errors"
	"testing"

	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	"go.idp.xyz/idp-cargo/internal/foundation/identity"
)

// FIN's permission to review the after-the-fact list (V2.102, batch B) is granted by name only: "fin:*" and "*" grant
// every other action of FIN, the reading of the list among them (ReadProfit), and not this one; a name grants its own
// action and not the settings' two, and the settings' names do not grant it.
func TestFinReviewAfterTheFactIsGrantedByNameOnly(t *testing.T) {
	as := func(permissions ...string) context.Context {
		return identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "t1", ActorID: "ann", Permissions: permissions})
	}
	for _, c := range []struct {
		name   string
		ctx    context.Context
		read   bool
		review bool
	}{
		{"fin:*", as("fin:*"), true, false},
		{"*", as("*"), true, false},
		{"fin:ReviewAfterTheFact", as("fin:ReviewAfterTheFact"), false, true},
		{"fin:ReadProfit and the name", as("fin:ReadProfit", "fin:ReviewAfterTheFact"), true, true},
		{"fin:* and the name", as("fin:*", "fin:ReviewAfterTheFact"), true, true},
		{"the settings' names", as("fin:ConfigureSettings", "fin:ApproveSettings"), false, false},
		{"another context's name", as("sea:ReviewAfterTheFact", "sea:*"), false, false},
		{"no permission", as(), false, false},
		{"no principal", context.Background(), false, false},
	} {
		for action, want := range map[string]bool{finapi.ActionReadProfit: c.read, finapi.ActionReviewAfterTheFact: c.review} {
			err := permissions{}.AuthorizeFinAction(c.ctx, "t1", "ann", action, "LE/2026-W48")
			if (err == nil) != want || (err != nil && !errors.Is(err, identity.ErrDenied)) {
				t.Errorf("%s, %s: %v, want allowed %v", c.name, action, err, want)
			}
		}
	}
	// the principal must be the tenant and the actor the command names
	ctx := as("fin:ReviewAfterTheFact")
	if err := (permissions{}).AuthorizeFinAction(ctx, "t2", "ann", finapi.ActionReviewAfterTheFact, "x"); !errors.Is(err, identity.ErrDenied) {
		t.Errorf("for another tenant: %v", err)
	}
	if err := (permissions{}).AuthorizeFinAction(ctx, "t1", "bob", finapi.ActionReviewAfterTheFact, "x"); !errors.Is(err, identity.ErrDenied) {
		t.Errorf("for another actor: %v", err)
	}
}
