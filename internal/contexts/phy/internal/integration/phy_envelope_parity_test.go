//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	pg "go.idp.xyz/idp-cargo/internal/contexts/phy/internal/adapters/postgres"
	phy "go.idp.xyz/idp-cargo/internal/contexts/phy/internal/domain"
)

type envelopeHouse struct {
	wUnit string
	w     int64
	vUnit string // "" = no volume
	v     int64
}

var errRollback = errors.New("rolled back on purpose")

// TestEnvelopeDomainAndDatabaseAgree puts the same live houses through the domain's Close and through the database
// trigger, for one 22G1 (28,230,000 g, 33,200,000 cm3) unless units says otherwise: both must find the plan over the
// envelope, or within it, as expected, so that the Go and SQL conversions cannot drift apart. The houses are written
// in a transaction that is rolled back.
func TestEnvelopeDomainAndDatabaseAgree(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	cases := []struct {
		name   string
		units  []string
		remove int // unit to remove, 0 none
		houses []envelopeHouse
		over   bool
	}{
		{"kg at limit", []string{"22G1"}, 0, []envelopeHouse{{"KGM", 28230, "", 0}}, false},
		{"kg one over", []string{"22G1"}, 0, []envelopeHouse{{"KGM", 28231, "", 0}}, true},
		{"grams one over", []string{"22G1"}, 0, []envelopeHouse{{"GRM", 28230001, "", 0}}, true},
		{"lb 62236", []string{"22G1"}, 0, []envelopeHouse{{"LBR", 62236, "", 0}}, false},
		{"lb 62237", []string{"22G1"}, 0, []envelopeHouse{{"LBR", 62237, "", 0}}, true},
		{"lb per-house ceil", []string{"22G1"}, 0, []envelopeHouse{{"KGM", 28229, "", 0}, {"LBR", 1, "", 0}, {"LBR", 1, "", 0}, {"GRM", 92, "", 0}}, false},
		{"lb per-house ceil +1g", []string{"22G1"}, 0, []envelopeHouse{{"KGM", 28229, "", 0}, {"LBR", 1, "", 0}, {"LBR", 1, "", 0}, {"GRM", 93, "", 0}}, true},
		{"ftq 1172", []string{"22G1"}, 0, []envelopeHouse{{"KGM", 1, "FTQ", 1172}}, false},
		{"ftq 1173", []string{"22G1"}, 0, []envelopeHouse{{"KGM", 1, "FTQ", 1173}}, true},
		{"m3 at limit via cm3", []string{"22G1"}, 0, []envelopeHouse{{"KGM", 1, "CMQ", 33200000}}, false},
		{"m3 one cm3 over", []string{"22G1"}, 0, []envelopeHouse{{"KGM", 1, "CMQ", 33200001}}, true},
		{"ltr+dmq", []string{"22G1"}, 0, []envelopeHouse{{"KGM", 1, "LTR", 16600}, {"KGM", 1, "DMQ", 16600}}, false},
		{"ltr+dmq over", []string{"22G1"}, 0, []envelopeHouse{{"KGM", 1, "LTR", 16600}, {"KGM", 1, "DMQ", 16601}}, true},
		{"no volumes", []string{"22G1"}, 0, []envelopeHouse{{"KGM", 1000, "", 0}, {"KGM", 1000, "", 0}}, false},
		{"one volume missing", []string{"22G1"}, 0, []envelopeHouse{{"KGM", 1000, "", 0}, {"KGM", 1000, "MTQ", 34}}, true},
		{"weight ONZ", []string{"22G1"}, 0, []envelopeHouse{{"ONZ", 1, "", 0}}, true},
		{"volume GLL", []string{"22G1"}, 0, []envelopeHouse{{"KGM", 1, "GLL", 1}}, true},
		{"huge KGM", []string{"22G1"}, 0, []envelopeHouse{{"KGM", 9223372036854775807, "", 0}}, true},
		{"huge TNE x2", []string{"22G1"}, 0, []envelopeHouse{{"TNE", 9223372036854775807, "", 0}, {"TNE", 9223372036854775807, "", 0}}, true},
		{"huge FTQ", []string{"22G1"}, 0, []envelopeHouse{{"KGM", 1, "FTQ", 9223372036854775807}}, true},
		{"two units", []string{"22G1", "22G1"}, 0, []envelopeHouse{{"KGM", 56460, "MTQ", 66}}, false},
		{"two units one removed", []string{"22G1", "22G1"}, 2, []envelopeHouse{{"KGM", 30000, "", 0}}, true},
		{"42G1+45G1 vol", []string{"42G1", "45G1"}, 0, []envelopeHouse{{"KGM", 100, "CMQ", 144000000}}, false},
		{"42G1+45G1 vol over", []string{"42G1", "45G1"}, 0, []envelopeHouse{{"KGM", 100, "CMQ", 144000001}}, true},
		{"no units", nil, 0, []envelopeHouse{{"KGM", 99999, "MTQ", 99}}, false},
	}
	store := pg.ConsolidationStore{DB: f.db}
	for i, tc := range cases {
		id := fmt.Sprintf("C-P%02d", i)
		f.createSea(t, id, api.KindSeaLCLOwn)
		rev := int64(1)
		for n, code := range tc.units {
			ok(t, rev+1)(f.module.PlanLoadUnit(ctx, f.planUnit(fmt.Sprintf("plan-%s-%d", id, n), id, rev, code)))
			rev++
		}
		if tc.remove > 0 {
			if _, err := f.module.RemovePlannedLoadUnit(ctx, api.RemovePlannedLoadUnit{CommandID: "remove-" + id, TenantID: f.tenant, ActorID: "ops-1",
				ConsolidationID: id, ExpectedRevision: rev, UnitNo: tc.remove, Reason: "test", OccurredAt: f.at.Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
		}
		var domainVerdict, dbNull, dbReason string
		err := f.within(t, func(txCtx context.Context, e bentopg.Executor) error {
			for n, h := range tc.houses {
				sh := fmt.Sprintf("SH-%s-%d", id, n)
				var vu, v any
				if h.vUnit != "" {
					vu, v = h.vUnit, h.v
				}
				if _, err := e.Exec(txCtx, `INSERT INTO cargo_phy.consol_membership (tenant_id,shipment_id,stage) VALUES ($1,$2,'MAIN')`, f.tenant, sh); err != nil {
					return err
				}
				if _, err := e.Exec(txCtx, `INSERT INTO cargo_phy.membership_entry (tenant_id,shipment_id,stage,entry_no,consolidation_id,start_kind,
                    ful_revision,plan_version,packs,gross_weight_unit,gross_weight,volume_unit,volume,attached_by,attached_at,entered_at)
                    VALUES ($1,$2,'MAIN',1,$3,'ATTACHED',3,2,1,$4,$5,$6,$7,'test',$8,now())`, f.tenant, sh, id, h.wUnit, h.w, vu, v, f.at.Add(time.Hour)); err != nil {
					return err
				}
			}
			c, _, err := store.Load(txCtx, phy.TenantID(f.tenant), phy.ConsolidationID(id))
			if err != nil {
				return err
			}
			houses, err := store.LiveHouses(txCtx, phy.TenantID(f.tenant), phy.ConsolidationID(id))
			if err != nil {
				return err
			}
			scopes := make([]phy.CargoScope, 0, len(houses))
			for _, h := range houses {
				scopes = append(scopes, h.Entry.Scope)
			}
			cerr := c.Close(scopes, f.at.Add(time.Hour), "", "ops", f.at.Add(2*time.Hour))
			switch {
			case cerr == nil:
				domainVerdict = "within"
			case errors.Is(cerr, phy.ErrOverEnvelope):
				domainVerdict = "OVER (" + cerr.Error()[len(phy.ErrOverEnvelope.Error())+2:] + ")"
			default:
				domainVerdict = "error " + cerr.Error()
			}
			try := func(reason string) string {
				if _, err := e.Exec(txCtx, `SAVEPOINT p`); err != nil {
					return "savepoint " + err.Error()
				}
				_, err := e.Exec(txCtx, `INSERT INTO cargo_phy.consolidation_version (tenant_id,consolidation_id,version_no,change,state,origin_code,
                    destination_code,location_edition,planned_departure,house_cut_off,reason,written_by,written_at,entered_at)
                    SELECT tenant_id,consolidation_id,version_no+1,'CLOSED','CLOSED',origin_code,destination_code,location_edition,planned_departure,
                      house_cut_off,nullif($3,''),'x',now(),now() FROM cargo_phy.consolidation_version WHERE tenant_id=$1 AND consolidation_id=$2
                    ORDER BY version_no DESC LIMIT 1`, f.tenant, id, reason)
				if _, rerr := e.Exec(txCtx, `ROLLBACK TO SAVEPOINT p`); rerr != nil {
					return "rollback " + rerr.Error()
				}
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) {
					return pgErr.ConstraintName + ": " + pgErr.Message
				}
				if err != nil {
					return err.Error()
				}
				return "accepted"
			}
			dbNull, dbReason = try(""), try("accepted overrun")
			return errRollback
		})
		if !errors.Is(err, errRollback) {
			t.Fatalf("%s: %v", tc.name, err)
		}
		domainOver := domainVerdict != "within"
		dbOver := dbNull != "accepted" && dbReason == "accepted"
		dbWithin := dbNull == "accepted" && dbReason != "accepted"
		if domainOver != tc.over || (tc.over && !dbOver) || (!tc.over && !dbWithin) {
			t.Errorf("%s: want over=%v; domain %s | database without a reason: %s | with one: %s", tc.name, tc.over, domainVerdict, dbNull, dbReason)
		}
	}
}
