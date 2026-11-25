//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
)

// FIN V2.97, migration fin/000018: what the database keeps beside the rules that only the domain knows (which setting
// is in force, the thresholds, who is barred), written past FIN on the application role.

// rawVoucher is an attribution of SH-C to apportionment 2, line 1, with a voucher as SQL text (NULL or a quoted
// string). It is carried: since fin/000019 an apportionment that names no preview, as this raw one, states no attribution.
func rawVoucher(voucher string, loss, exception int64) string {
	party := "NULL"
	if exception > 0 {
		party = "'CARRIER'"
	}
	return fmt.Sprintf(`INSERT INTO cargo_fin.pool_apportionment_attribution (tenant_id,pool_id,apportionment_no,line_no,shipment_id,job_id,loss_minor,loss_local_minor,
            exception_minor,exception_local_minor,party,voucher,departed_at,carried) VALUES ($1,'P-U',2,1,'SH-C','JOB-C',%d,%d,%d,%d,%s,%s,now(),true)`,
		loss, loss, exception, exception, party, voucher)
}

// rawAttributed is a second apportionment of line 1 that attributes SH-C's share, with the shares of A and B making up
// the rest of the line.
func rawAttributed(voucher string, loss, exception int64) string {
	rest := 100_000 - loss - exception
	a := rest * 6 / 10
	return rawJoin(rawBump, rawApportionment(2), rawLine(2, "CHARGEABLE", 0, 0), rawShare(2, 1, "SH-A", "JOB-A", true, a), rawShare(2, 2, "SH-B", "JOB-B", true, rest-a),
		rawVoucher(voucher, loss, exception))
}

// The database's voucher rule: the empty voucher only when the loss and the exception cost are both 0, at most 500
// characters once trimmed, never NULL. It cannot know F3: with EVERY_ATTRIBUTION FIN asks for a voucher on an
// attribution of nothing too, and the database lets it through.
func TestTheVoucherRuleOfTheDatabaseOnPostgres(t *testing.T) {
	world := func() usedWorld {
		u := newUsedWorld(t)
		u.record("cost-1", "AIR_FREIGHT", api.MethodChargeable, 100_000)
		u.apportion("apportion-1", 4, []int{1}, "SH-C")
		return u
	}
	long := "'" + strings.Repeat("x", 501) + "'"
	u := world()
	for name, c := range map[string]struct {
		sql, state, check string
	}{
		"an empty voucher with a loss":               {rawAttributed("''", 5_000, 0), "23514", "pool_apportionment_attribution_voucher_check"},
		"an empty voucher with an exception cost":    {rawAttributed("''", 0, 5_000), "23514", "pool_apportionment_attribution_voucher_check"},
		"a blank voucher with no amount":             {rawAttributed("'   '", 0, 0), "23514", "pool_apportionment_attribution_voucher_check"},
		"a blank voucher with both":                  {rawAttributed("'   '", 5_000, 5_000), "23514", "pool_apportionment_attribution_voucher_check"},
		"a voucher of 501 characters with a loss":    {rawAttributed(long, 5_000, 0), "23514", "pool_apportionment_attribution_voucher_check"},
		"a voucher of 501 characters with no amount": {rawAttributed(long, 0, 0), "23514", "pool_apportionment_attribution_voucher_check"},
		"no voucher at all with no amount":           {rawAttributed("NULL", 0, 0), "23502", ""},
	} {
		err := u.rawRun(c.sql)
		if state, constraint := sqlState(err); state != c.state || (c.check != "" && constraint != c.check) {
			t.Errorf("%s: %v (state %s, constraint %s), want %s %s", name, err, state, constraint, c.state, c.check)
		}
	}
	for name, sql := range map[string]string{
		"an empty voucher with no amount":            rawAttributed("''", 0, 0),
		"a voucher with a loss":                      rawAttributed("'CLAIM-1'", 5_000, 0),
		"a voucher with no amount":                   rawAttributed("'CLAIM-1'", 0, 0),
		"a voucher of 500 characters with an amount": rawAttributed("'"+strings.Repeat("x", 500)+"'", 0, 5_000),
	} {
		if err := world().rawRun(sql); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The version an apportionment records is its preview's: the database refuses one that differs, or none, whatever the
// preview's (only a preview written before the migration has no version, and it is not compared).
func TestAnApportionmentRecordsItsPreviewsSettingsVersionOnPostgres(t *testing.T) {
	setup := func() (usedWorld, int, int) {
		u := previewWorld(t)
		v := u.configure(time0, le("fin.F2", "SUBMITTER_AND_PREVIEWER"))
		r := u.must(u.preview("preview-1", 4, nil, "SH-C"))
		if p := u.ap.pool(u.pool); p.Previews[0].SettingsVersion != v {
			t.Fatalf("the preview records version %d: %+v", v, p.Previews)
		}
		return u, int(r.Version), v
	}
	from := func(preview int, version string) string {
		return fmt.Sprintf(`INSERT INTO cargo_fin.pool_apportionment (tenant_id,pool_id,apportionment_no,basis_revision,stage,made_by,made_at,entered_at,preview_no,settings_version)
            SELECT $1,'P-U',1,basis_revision,stage,'x',now(),now(),%[1]d,%[2]s FROM cargo_fin.pool_apportionment_preview WHERE tenant_id=$1 AND pool_id='P-U' AND preview_no=%[1]d`,
			preview, version)
	}
	body := rawJoin(`INSERT INTO cargo_fin.pool_apportionment_line (tenant_id,pool_id,apportionment_no,line_no,line_version,method,amount_minor,local_minor)
            VALUES ($1,'P-U',1,1,1,'CHARGEABLE',100000,100000)`,
		`INSERT INTO cargo_fin.pool_apportionment_share (tenant_id,pool_id,apportionment_no,line_no,share_no,shipment_id,job_id,used,quantity,unit,amount_minor,local_minor)
            VALUES ($1,'P-U',1,1,1,'SH-A','JOB-A',true,300000,'GRM',60000,60000)`,
		`INSERT INTO cargo_fin.pool_apportionment_share (tenant_id,pool_id,apportionment_no,line_no,share_no,shipment_id,job_id,used,quantity,unit,amount_minor,local_minor)
            VALUES ($1,'P-U',1,1,2,'SH-B','JOB-B',true,200000,'GRM',40000,40000)`,
		`INSERT INTO cargo_fin.pool_apportionment_share (tenant_id,pool_id,apportionment_no,line_no,share_no,shipment_id,job_id,used,quantity,unit,amount_minor,local_minor)
            VALUES ($1,'P-U',1,1,3,'SH-C','JOB-C',false,0,NULL,0,0)`)
	u, pn, v := setup()
	for name, version := range map[string]string{"another version": fmt.Sprint(v + 1), "no settings": "0", "no version": "NULL", "a negative one": "-1"} {
		err := u.rawRun(rawJoin(rawBump, from(pn, version), body))
		state, constraint := sqlState(err)
		want := "pool_apportionment_preview_matches"
		if version == "-1" {
			want = "pool_apportionment_settings_version"
		}
		if state != "23514" || constraint != want {
			t.Errorf("%s: %v (state %s, constraint %s), want %s", name, err, state, constraint, want)
		}
	}
	if err := u.rawRun(rawJoin(rawBump, from(pn, fmt.Sprint(v)), body)); err != nil {
		t.Fatalf("the version of its preview: %v", err)
	}
	if a := u.ap.pool(u.pool).Apportionments; len(a) != 1 || a[0].SettingsVersion != v || a[0].PreviewNo != pn {
		t.Fatalf("the control: %+v", a)
	}
	// a preview made before the migration has no version: its apportionment records the one in force
	old, _, _ := setup()
	legacy := `INSERT INTO cargo_fin.pool_apportionment_preview (tenant_id,pool_id,preview_no,basis_revision,stage,figures,fingerprint,fingerprint_sha256,made_by,made_at,entered_at,
            settings_version) SELECT tenant_id,pool_id,2,basis_revision,stage,figures,fingerprint,fingerprint_sha256,made_by,made_at,entered_at,NULL
            FROM cargo_fin.pool_apportionment_preview WHERE tenant_id=$1 AND pool_id='P-U' AND preview_no=1`
	// a preview's version is not negative
	negative := strings.Replace(legacy, "entered_at,NULL", "entered_at,-1", 1)
	err := old.rawRun(rawJoin(rawBump, negative))
	if state, constraint := sqlState(err); state != "23514" || constraint != "pool_preview_settings_version" {
		t.Errorf("a negative preview version: %v (state %s, constraint %s)", err, state, constraint)
	}
	if err = old.rawRun(rawJoin(rawBump, legacy, from(2, "7"), body)); err != nil {
		t.Fatalf("the apportionment of a preview with no version: %v", err)
	}
}

// A closure records its version once: it is not changed when the pool is reopened or by a raw write, and it is not
// negative.
func TestAClosuresSettingsVersionIsRecordedOnceOnPostgres(t *testing.T) {
	u := previewWorld(t)
	v := u.configure(time0, le("fin.F7", "BLOCKS"))
	r := u.must(u.preview("preview-1", 4, nil, "SH-C"))
	u.must(u.submit("submit-1", "fin-1", int(r.Version), 4))
	u.closePool(5)
	if c := u.ap.pool(u.pool).Closures; len(c) != 1 || c[0].SettingsVersion != v {
		t.Fatalf("the closure: %+v (version %d)", c, v)
	}
	// the application role may update only the four columns of the reopening (the workflow's grant), so the privilege
	// refuses first; the migrator role, which holds the privilege, meets the trigger every closure has (it changes only
	// in the four columns, once)
	err := u.rawRun(`UPDATE cargo_fin.consol_cost_pool_closure SET settings_version=settings_version+1 WHERE tenant_id=$1 AND pool_id='P-U'`)
	if state, _ := sqlState(err); state != "42501" {
		t.Fatalf("a closure's version changed by the application role: %v (state %s)", err, state)
	}
	if dsn := os.Getenv("IDP_CARGO_MIGRATOR_DSN"); dsn != "" {
		conn, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(context.Background())
		_, err = conn.Exec(context.Background(), `UPDATE cargo_fin.consol_cost_pool_closure SET settings_version=settings_version+1 WHERE tenant_id=$1 AND pool_id='P-U'`, u.tenant)
		if state, constraint := sqlState(err); state != "23001" {
			t.Fatalf("a closure's version changed by the migrator role: %v (state %s, constraint %s)", err, state, constraint)
		}
	} else {
		t.Log("IDP_CARGO_MIGRATOR_DSN is not set: the trigger that keeps a closure as written is not exercised")
	}
	u.must(u.module.ReopenConsolCostPool(u.ctx, api.ReopenConsolCostPool{CommandID: "reopen", TenantID: u.tenant, ActorID: "fin-1", PoolID: u.pool, ExpectedRevision: u.revision(),
		Reason: "a late invoice", OccurredAt: u.hours(6)}))
	if c := u.ap.pool(u.pool).Closures; len(c) != 1 || c[0].SettingsVersion != v || c[0].ReopenedBy != "fin-1" {
		t.Fatalf("the closure after the reopening: %+v", c)
	}
	n := previewWorld(t)
	err = n.rawRun(rawJoin(rawBump, `UPDATE cargo_fin.consol_cost_pool SET state='CLOSED' WHERE tenant_id=$1 AND pool_id='P-U'`,
		`INSERT INTO cargo_fin.consol_cost_pool_closure (tenant_id,pool_id,closure_no,closed_by,closed_at,entered_at,settings_version) VALUES ($1,'P-U',1,'x',now(),now(),-1)`))
	if state, constraint := sqlState(err); state != "23514" || constraint != "pool_closure_settings_version" {
		t.Fatalf("a negative version: %v (state %s, constraint %s)", err, state, constraint)
	}
}
