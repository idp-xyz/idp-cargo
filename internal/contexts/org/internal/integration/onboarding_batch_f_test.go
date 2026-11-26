//go:build integration

package integration

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/org/api"
)

// V2.102 batch F, ORG: the withdrawal of an onboarding that is not confirmed, and the record of who proposed, who confirmed, which version each
// module put in force, and when. pete holds ProposeOnboarding only (a consultant who did not propose this onboarding).

func TestAnOnboardingNotConfirmedIsWithdrawnByItsConsultantOrItsOwner(t *testing.T) {
	w := newOnboardingWorld(t)
	if _, err := w.propose("ob-1", "ann", "LEAN", 3, false); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("é", 2001)
	for name, c := range map[string]struct {
		actor, reason string
		kind          error
		say           string
	}{
		"someone with neither permission":                {"nobody", "no longer wanted", api.ErrUnauthorized, "withdraws it"},
		"a consultant who did not propose it":            {"pete", "no longer wanted", api.ErrUnauthorized, "the customer's owner (ConfirmOnboarding)"},
		"no reason":                                      {"ann", " \n", api.ErrRejected, "1 to 2000 characters"},
		"a reason of 2001 characters":                    {"ann", long, api.ErrRejected, "1 to 2000 characters"},
		"an actor with spaces around the id (the owner)": {" olga", "no longer wanted", api.ErrRejected, "give it as the identity platform names the person"},
	} {
		_, err := w.withdraw(w.command(), "ob-1", c.actor, c.reason)
		said(t, name, err, c.kind, c.say)
	}
	_, err := w.withdraw(w.command(), "ob-404", "ann", "x")
	said(t, "an unknown onboarding", err, api.ErrNotFound, "ListOnboardings")
	if got := w.get("ob-1"); got.State != "PROPOSED" || got.WithdrawnBy != "" {
		t.Fatalf("a refused withdrawal changed the onboarding: %+v", got)
	}

	// the consultant who proposed it withdraws it: who, when and why are recorded, and the record is immutable
	r, err := w.withdraw("w-1", "ob-1", "ann", "  the customer chose another profile  ")
	if err != nil || r.State != "WITHDRAWN" || r.NewRevision != 2 {
		t.Fatalf("the withdrawal: %+v, %v", r, err)
	}
	o := w.get("ob-1")
	if o.State != "WITHDRAWN" || o.WithdrawnBy != "ann" || o.WithdrawReason != "the customer chose another profile" || o.WithdrawnAt.Before(o.ProposedAt) || time.Since(o.WithdrawnAt) > time.Minute ||
		o.ConfirmedBy != "" {
		t.Fatalf("the record: %+v", o)
	}
	if again, err := w.withdraw("w-1", "ob-1", "ann", "  the customer chose another profile  "); err != nil || !reflect.DeepEqual(again, r) {
		t.Fatalf("a replay: %+v, %v", again, err)
	}
	if again, err := w.withdraw("w-retry", "ob-1", "ann", "the customer chose another profile"); err != nil || again.State != "WITHDRAWN" || again.NewRevision != 2 {
		t.Fatalf("the same person and reason under another command id: %+v, %v", again, err)
	}
	_, err = w.withdraw(w.command(), "ob-1", "olga", "another reason")
	said(t, "a second withdrawal with another reason", err, api.ErrIdempotencyConflict, "withdrawn by ann already")
	_, err = w.withdraw(w.command(), "ob-1", "olga", "the customer chose another profile")
	said(t, "a second withdrawal by another person", err, api.ErrIdempotencyConflict, "withdrawn by ann already")

	// a withdrawn onboarding is never confirmed, and nothing is recorded for it afterwards
	_, err = w.confirm(w.command(), "ob-1", "olga", riskText)
	said(t, "confirming a withdrawn onboarding", err, api.ErrRejected, "withdrawn by ann", "never confirmed", "propose a new onboarding under another id")
	if _, err = w.record("ob-1", "ann", "FIN", "PROPOSED"); err == nil || !strings.Contains(err.Error(), "was withdrawn") {
		t.Fatalf("a module recorded for a withdrawn onboarding: %v", err)
	}
	if got := w.get("ob-1"); got.State != "WITHDRAWN" || got.ConfirmedBy != "" || got.Modules[0].State != "NOT_STARTED" {
		t.Fatalf("after the refusals: %+v", got)
	}

	// the owner withdraws another one; the held change sets the platform made are recorded first, and stay in the record
	if _, err = w.propose("ob-2", "ann", "STRICT", 5, false); err != nil {
		t.Fatal(err)
	}
	if _, err = w.record("ob-2", "ann", "FIN", "PROPOSED"); err != nil {
		t.Fatal(err)
	}
	r, err = w.withdraw("w-2", "ob-2", "olga", "the wrong customer")
	if err != nil || r.State != "WITHDRAWN" || r.NewRevision != 3 {
		t.Fatalf("the owner's withdrawal: %+v, %v", r, err)
	}
	if o = w.get("ob-2"); o.WithdrawnBy != "olga" || o.Modules[0].State != "PROPOSED" || o.Modules[0].ChangeID != "onb-ob-2-fin" {
		t.Fatalf("the record keeps the held change set: %+v", o)
	}
	list, err := w.module.ListOnboardings(context.Background(), api.ListOnboardings{TenantID: w.tenant})
	if err != nil || len(list) != 2 || list[0].State != "WITHDRAWN" || list[1].State != "WITHDRAWN" {
		t.Fatalf("the list: %+v, %v", list, err)
	}

	// a confirmed onboarding is not withdrawn
	if _, err = w.propose("ob-3", "ann", "LEAN", 3, false); err != nil {
		t.Fatal(err)
	}
	if _, err = w.confirm("c-3", "ob-3", "olga", riskText); err != nil {
		t.Fatal(err)
	}
	_, err = w.withdraw(w.command(), "ob-3", "ann", "too late")
	said(t, "withdrawing a confirmed onboarding", err, api.ErrRejected, "confirmed by olga", "is not withdrawn", "propose a change set that restores the earlier values")
	if got := w.get("ob-3"); got.State != "CONFIRMED" || got.WithdrawnBy != "" {
		t.Fatalf("the confirmed onboarding: %+v", got)
	}
}

// The onboarding's record says who proposed and when, who confirmed and when, the plan and its SHA-256, and for each module its change id, its
// state and the settings version it put in force.
func TestTheOnboardingRecordSaysWhoProposedWhoConfirmedAndWhichVersionEachModuleMade(t *testing.T) {
	w := newOnboardingWorld(t)
	r, err := w.propose("ob-1", "ann", "LEAN", 3, false)
	if err != nil {
		t.Fatal(err)
	}
	o := w.get("ob-1")
	if o.ProposedBy != "ann" || o.ProposedAt.IsZero() || o.PlanSHA256 != r.PlanSHA256 || o.PlanSHA256 != api.PlanSHA256(o.Plan) || o.ConfirmedBy != "" || !o.ConfirmedAt.IsZero() {
		t.Fatalf("after the proposal: %+v", o)
	}
	for _, m := range []string{"FIN", "SEA"} {
		if _, err = w.record("ob-1", "ann", m, "PROPOSED"); err != nil {
			t.Fatal(err)
		}
	}
	o = w.get("ob-1")
	for i, m := range o.Modules {
		if m.State != "PROPOSED" || m.ChangeID != "onb-ob-1-"+strings.ToLower([]string{"FIN", "SEA"}[i]) || m.Version != 0 {
			t.Fatalf("the held change set of %s: %+v", m.Module, m)
		}
	}
	if _, err = w.confirm("conf-1", "ob-1", "olga", riskText); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		module  string
		version int
	}{{"FIN", 4}, {"SEA", 9}} {
		if _, err = w.module.RecordOnboardingModule(context.Background(), api.RecordOnboardingModule{CommandID: w.command(), TenantID: w.tenant, ActorID: "olga", OnboardingID: "ob-1",
			Module: step.module, ChangeID: "onb-ob-1-" + strings.ToLower(step.module), State: "APPROVED", Version: step.version}); err != nil {
			t.Fatal(err)
		}
	}
	o = w.get("ob-1")
	if o.State != "COMPLETE" || o.ConfirmedBy != "olga" || o.ConfirmedAt.Before(o.ProposedAt) || o.PlanSHA256 != r.PlanSHA256 || o.Modules[0].Version != 4 || o.Modules[1].Version != 9 ||
		o.Modules[0].State != "APPROVED" || o.Modules[1].State != "APPROVED" || o.WithdrawnBy != "" {
		t.Fatalf("the record: %+v", o)
	}
	// the version is written once: the same record again answers it, another version does not overwrite it
	if _, err = w.module.RecordOnboardingModule(context.Background(), api.RecordOnboardingModule{CommandID: w.command(), TenantID: w.tenant, ActorID: "olga", OnboardingID: "ob-1",
		Module: "FIN", ChangeID: "onb-ob-1-fin", State: "APPROVED", Version: 99}); err != nil {
		t.Fatal(err)
	}
	if got := w.get("ob-1").Modules[0].Version; got != 4 {
		t.Fatalf("the recorded version was overwritten: %d", got)
	}
}
