//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"go.idp.xyz/idp-cargo/internal/contexts/org/api"
)

// V2.102 batch E, migration org/000012: the owner confirms the digest of the plan they read, an actor id has no spaces around it
// and is at most 128 bytes, and the rest of the plan's shape that the database keeps for SEA as for FIN.

// The confirmation carries the SHA-256 of the plan the owner read: it must be the stored plan's, as the acknowledged text must be
// the risk statement. Another, or none, is refused with the way out and records nothing; the right one is kept with the
// confirmation, shown in GetOnboarding, and a retry with it answers the record.
func TestTheConfirmationCarriesTheDigestOfThePlanTheOwnerRead(t *testing.T) {
	w := newOnboardingWorld(t)
	r, err := w.propose("ob-1", "ann", "LEAN", 3, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.PlanSHA256) != 64 || r.PlanSHA256 != api.PlanSHA256(r.Plan) || len(r.Plan) != 2 || r.Plan["FIN"].CatalogueVersion != "fin-v1" {
		t.Fatalf("the proposal's receipt carries the plan and its digest: %+v", r)
	}
	if o := w.get("ob-1"); o.PlanSHA256 != r.PlanSHA256 {
		t.Fatalf("GetOnboarding: %s, the receipt: %s", o.PlanSHA256, r.PlanSHA256)
	}
	for name, digest := range map[string]string{"none": "", "another": strings.Repeat("0", 64), "in capitals": strings.ToUpper(r.PlanSHA256), "padded": " " + r.PlanSHA256} {
		_, err := w.module.ConfirmOnboarding(context.Background(), api.ConfirmOnboarding{CommandID: w.command(), TenantID: w.tenant, ActorID: "olga", OnboardingID: "ob-1", AcknowledgedText: riskText, PlanSHA256: digest})
		said(t, "the digest: "+name, err, api.ErrRejected, "ORG_INVALID_ONBOARDING", "read the onboarding again", "GetOnboarding shows the plan and its PlanSHA256", "confirm what it shows")
	}
	if w.get("ob-1").State != "PROPOSED" {
		t.Fatal("a refused confirmation changed the onboarding")
	}
	c := api.ConfirmOnboarding{CommandID: "conf-1", TenantID: w.tenant, ActorID: "olga", OnboardingID: "ob-1", AcknowledgedText: riskText, PlanSHA256: r.PlanSHA256}
	got, err := w.module.ConfirmOnboarding(context.Background(), c)
	if err != nil || got.State != "CONFIRMED" || got.PlanSHA256 != r.PlanSHA256 || len(got.Plan) != 2 {
		t.Fatalf("the confirmation: %+v, %v", got, err)
	}
	if again, err := w.module.ConfirmOnboarding(context.Background(), c); err != nil || again.State != "CONFIRMED" || again.NewRevision != got.NewRevision {
		t.Fatalf("a retry: %+v, %v", again, err)
	}
	// once confirmed, a retry that carries another digest is not the same confirmation
	c.CommandID, c.PlanSHA256 = "conf-2", strings.Repeat("0", 64)
	_, err = w.module.ConfirmOnboarding(context.Background(), c)
	said(t, "a retry with another digest", err, api.ErrIdempotencyConflict, "was confirmed by olga already")
}

// An owner's actor id is 1 to 128 bytes without spaces around it: ORG refuses another before it records anything.
func TestAnOwnersActorIdHasNoSpacesAndIsAtMost128Bytes(t *testing.T) {
	w := newOnboardingWorld(t)
	r, err := w.propose("ob-1", "ann", "LEAN", 3, false)
	if err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]string{"padded": "olga ", "padded in front": " olga", "129 bytes": strings.Repeat("o", 129), "120 and 40 spaces": strings.Repeat("o", 120) + strings.Repeat(" ", 40)} {
		_, err := w.module.ConfirmOnboarding(context.Background(), api.ConfirmOnboarding{CommandID: w.command(), TenantID: w.tenant, ActorID: id, OnboardingID: "ob-1", AcknowledgedText: riskText, PlanSHA256: r.PlanSHA256})
		said(t, "an owner that is "+name, err, api.ErrRejected, "ORG_INVALID_ONBOARDING", "1 to 128 bytes, without spaces around it")
	}
	_, err = w.module.ProposeOnboarding(context.Background(), api.ProposeOnboarding{CommandID: "p-pad", TenantID: w.tenant, ActorID: "ann ", OnboardingID: "ob-2", Profile: "LEAN", Plan: plan(),
		RiskStatement: riskText, FinanceStaff: 3})
	said(t, "a consultant with a space", err, api.ErrRejected, "ORG_INVALID_ONBOARDING", "no spaces around it")
}

// What the database refuses of the digest, the actors and the plan of SEA, by raw writes on the application role.
func TestOnboardingDatabaseRulesOfBatchERefuseRawWrites(t *testing.T) {
	w := newOnboardingWorld(t)
	const hash = "0000000000000000000000000000000000000000000000000000000000000000"
	const entry = `{"ScopeKind":"TENANT","ScopeID":"","Key":"fin.F7","Value":"BLOCKS"}`
	many := func(n int) string { return strings.TrimSuffix(strings.Repeat(entry+",", n), ",") }
	planOf := func(fin, sea string) string { return `{"FIN":` + fin + `,"SEA":` + sea + `}` }
	good := `{"CatalogueVersion":"v","Entries":[` + entry + `]}`
	proposal := func(id, by string, extra map[string]string) string {
		cols := map[string]string{"profile": "'LEAN'", "overrides": "'{}'", "risk_statement": "'the statement'", "finance_staff": "2", "single_administrator": "false",
			"proposed_by": "'" + by + "'", "command_id": "'c-" + id + "'", "onboarding_id": "'" + id + "'", "plan": "'" + planOf(good, good) + "'", "plan_sha256": "'" + hash + "'"}
		for k, v := range extra {
			cols[k] = v
		}
		return fmt.Sprintf(`INSERT INTO cargo_org.onboarding (tenant_id,onboarding_id,command_id,request_hash,profile,overrides,plan,plan_sha256,risk_statement,finance_staff,
          single_administrator,proposed_by,proposed_at) VALUES ($1,%s,%s,'%s',%s,%s,%s,%s,%s,%s,%s,%s,now())`, cols["onboarding_id"], cols["command_id"], hash, cols["profile"], cols["overrides"],
			cols["plan"], cols["plan_sha256"], cols["risk_statement"], cols["finance_staff"], cols["single_administrator"], cols["proposed_by"])
	}
	// confirmation confirms as the confirmer, naming the administrator and carrying the digest given ("" is the proposal's own)
	confirmation := func(id, confirmer, administrator, digest string) string {
		d := "plan_sha256"
		if digest != "" {
			d = "'" + digest + "'"
		}
		return fmt.Sprintf(`INSERT INTO cargo_org.onboarding_confirmation (tenant_id,onboarding_id,command_id,proposed_by,proposed_at,confirmed_by,confirmed_at,acknowledged_text,
          single_administrator,administrator,plan_sha256) SELECT tenant_id,onboarding_id,'k-'||onboarding_id,proposed_by,proposed_at,'%s',proposed_at,risk_statement,single_administrator,%s,%s
          FROM cargo_org.onboarding WHERE tenant_id=$1 AND onboarding_id='%s'`, confirmer, administrator, d, id)
	}
	type rule struct {
		name       string
		stmts      []string
		state      string
		constraint string
	}
	for _, c := range []rule{
		{"a confirmation of another plan's digest", []string{proposal("a", "ann", nil), confirmation("a", "olga", "NULL", strings.Repeat("f", 64))}, "23503", "onboarding_confirmation_matches_proposal"},
		{"a digest that is not hexadecimal", []string{proposal("a", "ann", map[string]string{"plan_sha256": "'zz'"})}, "23514", "onboarding_plan_sha256"},
		{"a digest in capitals", []string{proposal("a", "ann", map[string]string{"plan_sha256": "'" + strings.Repeat("A", 64) + "'"})}, "23514", "onboarding_plan_sha256"},
		{"a digest of 63 characters", []string{proposal("a", "ann", map[string]string{"plan_sha256": "'" + strings.Repeat("a", 63) + "'"})}, "23514", "onboarding_plan_sha256"},
		{"an administrator of 129 characters", []string{proposal("a", "ann", map[string]string{"single_administrator": "true"}), confirmation("a", "olga", "'"+strings.Repeat("d", 129)+"'", "")}, "23514",
			"onboarding_administrator_length"},
		{"a confirmer with a space around the id", []string{proposal("a", "ann", nil), confirmation("a", "olga ", "NULL", "")}, "23514", "onboarding_confirmation_confirmed_by_check"},
		{"a confirmer of 129 characters", []string{proposal("a", "ann", nil), confirmation("a", strings.Repeat("o", 129), "NULL", "")}, "23514", "onboarding_confirmation_confirmed_by_check"},
		{"a proposer with a space around the id", []string{proposal("a", "ann ", nil)}, "23514", "onboarding_proposed_by_check"},
		{"a proposer of 129 characters", []string{proposal("a", strings.Repeat("a", 129), nil)}, "23514", "onboarding_proposed_by_check"},
		{"a SEA plan of 65 entries", []string{proposal("a", "ann", map[string]string{"plan": "'" + planOf(good, `{"CatalogueVersion":"v","Entries":[`+many(65)+`]}`) + "'"})}, "23514", "onboarding_plan"},
		{"a SEA plan of no entries", []string{proposal("a", "ann", map[string]string{"plan": "'" + planOf(good, `{"CatalogueVersion":"v","Entries":[]}`) + "'"})}, "23514", "onboarding_plan"},
		{"a SEA catalogue version that is a number", []string{proposal("a", "ann", map[string]string{"plan": "'" + planOf(good, `{"CatalogueVersion":7,"Entries":[`+entry+`]}`) + "'"})}, "23514", "onboarding_plan"},
		{"a SEA catalogue version that is blank", []string{proposal("a", "ann", map[string]string{"plan": "'" + planOf(good, `{"CatalogueVersion":" ","Entries":[`+entry+`]}`) + "'"})}, "23514", "onboarding_plan"},
		{"a SEA catalogue version of 65 characters", []string{proposal("a", "ann", map[string]string{"plan": "'" + planOf(good, `{"CatalogueVersion":"`+strings.Repeat("v", 65)+`","Entries":[`+entry+`]}`) + "'"})},
			"23514", "onboarding_plan"},
		{"a FIN catalogue version that is a number", []string{proposal("a", "ann", map[string]string{"plan": "'" + planOf(`{"CatalogueVersion":7,"Entries":[`+entry+`]}`, good) + "'"})}, "23514", "onboarding_plan"},
		{"a SEA plan whose entries are not an array", []string{proposal("a", "ann", map[string]string{"plan": "'" + planOf(good, `{"CatalogueVersion":"v","Entries":{}}`) + "'"})}, "23514", "onboarding_plan"},
	} {
		err := w.raw(c.stmts...)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != c.state || (c.constraint != "" && pgErr.ConstraintName != c.constraint) {
			t.Errorf("%s: %v, want %s %s", c.name, err, c.state, c.constraint)
		}
	}
	// the same rows, within the limits, are accepted: what was refused above was the rule
	if err := w.raw(proposal("ok", strings.Repeat("a", 128), map[string]string{"single_administrator": "true",
		"plan": "'" + planOf(`{"CatalogueVersion":"`+strings.Repeat("v", 64)+`","Entries":[`+many(64)+`]}`, `{"CatalogueVersion":"v","Entries":[`+many(64)+`]}`) + "'"}),
		confirmation("ok", strings.Repeat("o", 128), "'"+strings.Repeat("d", 128)+"'", "")); err != nil {
		t.Fatalf("rows at the limits: %v", err)
	}
}
