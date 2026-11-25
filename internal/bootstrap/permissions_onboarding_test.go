package bootstrap

import (
	"context"
	"errors"
	"testing"

	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/foundation/identity"
)

// V2.102: the two actions of onboarding and the declaration of a single administrator are granted by name only.
func TestOnboardingAndDeclarationPermissionsAreGrantedByNameOnly(t *testing.T) {
	as := func(p ...string) context.Context {
		return identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "t1", ActorID: "ann", Permissions: p})
	}
	check := func(name string, f func(ctx context.Context) error, perm, bc string) {
		for _, wild := range []string{"*", bc + ":*"} {
			if err := f(as(wild)); !errors.Is(err, identity.ErrDenied) {
				t.Errorf("%s with %s: %v", name, wild, err)
			}
		}
		if err := f(as(bc + ":Other")); !errors.Is(err, identity.ErrDenied) {
			t.Errorf("%s with another name: %v", name, err)
		}
		if err := f(as(perm)); err != nil {
			t.Errorf("%s with its name: %v", name, err)
		}
		if err := f(identity.WithPrincipal(context.Background(), identity.Principal{TenantID: "t2", ActorID: "ann", Permissions: []string{perm}})); !errors.Is(err, identity.ErrDenied) {
			t.Errorf("%s for another tenant: %v", name, err)
		}
	}
	for _, a := range []string{orgapi.ActionProposeOnboarding, orgapi.ActionConfirmOnboarding} {
		check(a, func(ctx context.Context) error { return permissions{}.AuthorizePartyAction(ctx, "t1", "ann", a, "ob") }, "org:"+a, "org")
	}
	check("fin declare", func(ctx context.Context) error {
		return permissions{}.AuthorizeFinAction(ctx, "t1", "ann", finapi.ActionDeclareSingleAdministrator, "ob")
	}, "fin:DeclareSingleAdministrator", "fin")
	check("sea declare", func(ctx context.Context) error {
		return permissions{}.AuthorizeSeaAction(ctx, "t1", "ann", seaapi.ActionDeclareSingleAdministrator, "ob")
	}, "sea:DeclareSingleAdministrator", "sea")
	// another party action is still granted by the wildcard
	if err := (permissions{}).AuthorizePartyAction(as("org:*"), "t1", "ann", "RecordPartyContact", "p"); err != nil {
		t.Errorf("org:* for another action: %v", err)
	}
}
