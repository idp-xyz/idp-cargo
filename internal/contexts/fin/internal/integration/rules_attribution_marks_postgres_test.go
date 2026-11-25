//go:build integration

package integration

import (
	"fmt"
	"strings"
	"testing"

	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
)

// FIN V2.98, migration fin/000019: what the database keeps beside the rules that only the domain knows (F4, F8 and F9
// themselves, and whether the amounts of a row carried by ratio are the right proportion), written past FIN on the
// application role. The database cannot know a setting: it holds a row marked by default to nothing, a row marked by
// ratio to a carried one, and an attribution of an apportionment that names no preview to a carried one.

// rawMarked is a preview-less second apportionment of line 1 whose only attribution is SH-C's, with the marks given.
func rawMarked(carried, byDefault, byRatio bool, loss, exception int64, party, voucher string) string {
	rest := 100_000 - loss - exception
	a := rest * 6 / 10
	p := "NULL"
	if party != "" {
		p = "'" + party + "'"
	}
	return rawJoin(rawBump, rawApportionment(2), rawLine(2, "CHARGEABLE", 0, 0), rawShare(2, 1, "SH-A", "JOB-A", true, a), rawShare(2, 2, "SH-B", "JOB-B", true, rest-a),
		fmt.Sprintf(`INSERT INTO cargo_fin.pool_apportionment_attribution (tenant_id,pool_id,apportionment_no,line_no,shipment_id,job_id,loss_minor,loss_local_minor,
            exception_minor,exception_local_minor,party,voucher,departed_at,carried,by_default,by_ratio) VALUES ($1,'P-U',2,1,'SH-C','JOB-C',%d,%d,%d,%d,%s,'%s',now(),%v,%v,%v)`,
			loss, loss, exception, exception, p, voucher, carried, byDefault, byRatio))
}

func TestTheMarksOfAnAttributionOnPostgres(t *testing.T) {
	world := func() usedWorld {
		u := newUsedWorld(t)
		u.record("cost-1", "AIR_FREIGHT", api.MethodChargeable, 100_000)
		u.apportion("apportion-1", 4, []int{1}, "SH-C")
		return u
	}
	for name, c := range map[string]struct {
		sql, check string
	}{
		"a default with a loss":                       {rawMarked(true, true, false, 5_000, 0, "", "CLAIM-1"), "pool_attribution_by_default"},
		"a default with an exception cost":            {rawMarked(true, true, false, 0, 5_000, "CARRIER", "CLAIM-1"), "pool_attribution_by_default"},
		"a default with a voucher":                    {rawMarked(true, true, false, 0, 0, "", "CLAIM-1"), "pool_attribution_by_default"},
		"a default with a party and no amount":        {rawMarked(true, true, false, 0, 0, "CARRIER", ""), ""},
		"a ratio that is not carried":                 {rawMarked(false, false, true, 5_000, 0, "", "CLAIM-1"), "pool_attribution_by_ratio"},
		"a default that is not carried":               {rawMarked(false, true, false, 0, 0, "", ""), "pool_attribution_stated_by_preview"},
		"a stated attribution with no preview":        {rawMarked(false, false, false, 5_000, 0, "", "CLAIM-1"), "pool_attribution_stated_by_preview"},
		"a stated attribution of nothing, no preview": {rawMarked(false, false, false, 0, 0, "", ""), "pool_attribution_stated_by_preview"},
	} {
		err := world().rawRun(c.sql)
		state, constraint := sqlState(err)
		if state != "23514" || (c.check != "" && constraint != c.check) {
			t.Errorf("%s: %v (state %s, constraint %s), want 23514 %s", name, err, state, constraint, c.check)
		}
	}
	for name, sql := range map[string]string{
		"a default, carried":                     rawMarked(true, true, false, 0, 0, "", ""),
		"a ratio, carried":                       rawMarked(true, false, true, 5_000, 10_000, "WAREHOUSE", "CLAIM-1"),
		"a carried attribution":                  rawMarked(true, false, false, 5_000, 0, "", "CLAIM-1"),
		"a default that is also scaled, carried": rawMarked(true, true, true, 0, 0, "", ""),
	} {
		if err := world().rawRun(sql); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// a row written without the marks has them false (the default of the columns)
	u := world()
	if err := u.rawRun(rawAttributed("'CLAIM-1'", 5_000, 0)); err != nil {
		t.Fatal(err)
	}
	if x := lineOf(u.ap.pool(u.pool).Apportionments[1], 1).Attributions; len(x) != 1 || x[0].ByDefault || x[0].ByRatio || !x[0].Carried {
		t.Fatalf("a row written without the marks: %+v", x)
	}
}

// rawFromPreview is the second apportionment, written as a copy of a preview's figures, with the marks of its attributions
// as the SQL expressions given (the preview's own, or their opposites).
func rawFromPreview(no, pn int, byDefault, byRatio string) string {
	from := fmt.Sprintf(`FROM cargo_fin.pool_apportionment_preview v, jsonb_array_elements(v.figures) l WHERE v.tenant_id=$1 AND v.pool_id='P-U' AND v.preview_no=%d`, pn)
	withShares := strings.Replace(from, "l WHERE", "l CROSS JOIN LATERAL jsonb_array_elements(l->'shares') s WHERE", 1)
	withAttributions := strings.Replace(from, "l WHERE", "l CROSS JOIN LATERAL jsonb_array_elements(coalesce(l->'attributions','[]'::jsonb)) x WHERE", 1)
	return strings.ReplaceAll(rawJoin(
		fmt.Sprintf(`INSERT INTO cargo_fin.pool_apportionment (tenant_id,pool_id,apportionment_no,basis_revision,stage,made_by,made_at,entered_at,preview_no,settings_version)
            SELECT $1,'P-U',@NO@,basis_revision,stage,'x',now(),now(),preview_no,settings_version FROM cargo_fin.pool_apportionment_preview WHERE tenant_id=$1 AND pool_id='P-U' AND preview_no=%d`, pn),
		`INSERT INTO cargo_fin.pool_apportionment_line (tenant_id,pool_id,apportionment_no,line_no,line_version,method,reason,amount_minor,local_minor,to_allocate_minor,
            to_allocate_local_minor) SELECT $1,'P-U',@NO@,(l->>'line_no')::integer,(l->>'line_version')::integer,l->>'method',l->>'reason',(l->>'amount_minor')::bigint,
            (l->>'local_minor')::bigint,coalesce((l->>'to_allocate_minor')::bigint,0),coalesce((l->>'to_allocate_local_minor')::bigint,0) `+from,
		`INSERT INTO cargo_fin.pool_apportionment_share (tenant_id,pool_id,apportionment_no,line_no,share_no,shipment_id,job_id,used,quantity,unit,amount_minor,local_minor)
            SELECT $1,'P-U',@NO@,(l->>'line_no')::integer,(s->>'share_no')::integer,s->>'shipment_id',s->>'job_id',(s->>'used')::boolean,(s->>'quantity')::bigint,
            nullif(s->>'unit',''),(s->>'amount_minor')::bigint,(s->>'local_minor')::bigint `+withShares,
		`INSERT INTO cargo_fin.pool_apportionment_attribution (tenant_id,pool_id,apportionment_no,line_no,shipment_id,job_id,loss_minor,loss_local_minor,exception_minor,
            exception_local_minor,party,party_reference,voucher,reason,departed_at,carried,by_default,by_ratio) SELECT $1,'P-U',@NO@,(l->>'line_no')::integer,x->>'shipment_id',
            x->>'job_id',(x->>'loss_minor')::bigint,(x->>'loss_local_minor')::bigint,(x->>'exception_minor')::bigint,(x->>'exception_local_minor')::bigint,x->>'party',
            x->>'party_reference',x->>'voucher',x->>'reason',(x->>'departed_at')::timestamptz,(x->>'carried')::boolean,`+byDefault+`,`+byRatio+` `+withAttributions), "@NO@", fmt.Sprint(no))
}

// previewMark is the mark of an attribution as its preview's figures have it (false when they lack it).
func previewMark(name string) string {
	return fmt.Sprintf(`coalesce((x->>'%s')::boolean, false)`, name)
}

// The check an apportionment of a preview passes at commit compares the marks of each attribution with the preview's
// figures, both ways: a default that is not marked, a ratio that is not marked, and a mark the preview lacks are each
// refused, and an apportionment that is a copy of its preview is accepted; a preview written before the migration has no
// marks in its figures, which read as false.
func TestThePreviewCheckComparesTheMarksOnPostgres(t *testing.T) {
	refused := func(name string, err error) {
		t.Helper()
		if state, constraint := sqlState(err); state != "23514" || constraint != "pool_apportionment_preview_matches" {
			t.Errorf("%s: %v (state %s, constraint %s)", name, err, state, constraint)
		}
	}
	// a preview that attributes by default
	world := func(configure func(u usedWorld)) usedWorld {
		u := formerWorld(t)
		configure(u)
		u.depart()
		return u
	}
	d := world(func(u usedWorld) { u.configure(time0, le("fin.F3", f3Moves), le("fin.F8", f8Dflt)) })
	pre := d.must(d.module.PreviewConsolCostApportionment(d.ctx, d.attributeCmd("preview-1", "ops-2", 6)))
	pn := int(pre.Version)
	if x := lineOf(api.PoolApportionment{Lines: d.ap.pool(d.pool).Previews[pn-1].Lines}, 1).Attributions; len(x) != 1 || !x[0].ByDefault {
		t.Fatalf("the preview: %+v", x)
	}
	refused("a default that the apportionment does not mark", d.rawRun(rawJoin(rawBump, rawFromPreview(2, pn, "false", "false"))))
	if err := d.rawRun(rawJoin(rawBump, rawFromPreview(2, pn, previewMark("by_default"), previewMark("by_ratio")))); err != nil {
		t.Fatalf("a copy of the preview: %v", err)
	}
	if p := d.ap.pool(d.pool); len(p.Apportionments) != 2 || !lineOf(p.Apportionments[1], 1).Attributions[0].ByDefault || p.Previews[pn-1].SubmittedAs != 2 {
		t.Fatalf("the control: %+v", p)
	}

	// a preview that carries by ratio, and one that carries as it is
	for _, c := range []struct {
		f9       string
		ratio    bool
		opposite string
	}{{"KEEP_RATIO", true, "false"}, {"KEEP_AMOUNT", false, "true"}} {
		u := world(func(u usedWorld) {})
		u.attribute("attribute-1", 6, statementOf("C", 5_000, 10_000, api.PartyWarehouse))
		u.reviseLine("revise-1", 1, 120_000, 7)
		u.configure(time0, le("fin.F9", c.f9))
		r := u.must(u.module.PreviewConsolCostApportionment(u.ctx, u.attributeCmd("preview-2", "ops-2", 8)))
		n := int(r.Version)
		if x := lineOf(api.PoolApportionment{Lines: u.ap.pool(u.pool).Previews[n-1].Lines}, 1).Attributions; len(x) != 1 || !x[0].Carried || x[0].ByRatio != c.ratio {
			t.Fatalf("%s: the preview: %+v", c.f9, x)
		}
		refused(c.f9+": the other mark of ratio", u.rawRun(rawJoin(rawBump, rawFromPreview(3, n, previewMark("by_default"), c.opposite))))
		if err := u.rawRun(rawJoin(rawBump, rawFromPreview(3, n, previewMark("by_default"), previewMark("by_ratio")))); err != nil {
			t.Fatalf("%s: a copy of the preview: %v", c.f9, err)
		}
	}

	// a preview written before the migration: its figures have no marks, read as false, and settings_version NULL
	u := world(func(u usedWorld) {})
	r := u.must(u.module.PreviewConsolCostApportionment(u.ctx, u.attributeCmd("preview-1", "ops-2", 6, statementOf("C", 5_000, 10_000, api.PartyWarehouse))))
	n := int(r.Version) + 1
	legacy := fmt.Sprintf(`INSERT INTO cargo_fin.pool_apportionment_preview (tenant_id,pool_id,preview_no,basis_revision,stage,figures,fingerprint,fingerprint_sha256,made_by,made_at,
            entered_at,settings_version) SELECT tenant_id,pool_id,%d,basis_revision,stage,(SELECT jsonb_agg(jsonb_set(l,'{attributions}',(SELECT coalesce(jsonb_agg(x - 'by_default' - 'by_ratio'),
            '[]'::jsonb) FROM jsonb_array_elements(l->'attributions') x))) FROM jsonb_array_elements(v.figures) l),fingerprint,fingerprint_sha256,made_by,made_at,entered_at,NULL
            FROM cargo_fin.pool_apportionment_preview v WHERE tenant_id=$1 AND pool_id='P-U' AND preview_no=%d`, n, int(r.Version))
	if err := u.rawRun(rawJoin(rawBump, legacy, rawFromPreview(2, n, previewMark("by_default"), previewMark("by_ratio")))); err != nil {
		t.Fatalf("an apportionment of a preview with no marks: %v", err)
	}
	if p := u.ap.pool(u.pool); len(p.Apportionments) != 2 || p.Apportionments[1].PreviewNo != n || lineOf(p.Apportionments[1], 1).Attributions[0].ByDefault {
		t.Fatalf("the control: %+v", p)
	}
}
