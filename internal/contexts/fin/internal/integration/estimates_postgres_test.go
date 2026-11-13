//go:build integration

package integration

import (
	"context"
	"errors"
	"strings"
	"testing"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	rateapi "go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Rated estimates on PostgreSQL (FB2 and FB3 with RATE V0.5 part 2): a job's shipment rated by RATE into RATED charge
// lines for the customer and the suppliers, a pool's consolidation rated into RATED cost lines, a job priced by its
// quotes or by one rated estimate, a pool by one, and what the database refuses past FIN.
func TestRatedEstimatesOnPostgres(t *testing.T) {
	a := creditorWorld(t)
	ctx, f := a.ctx, a.fixture
	f.w.mu.Lock()
	d := f.w.drivers["SH-B"]
	d.Origin, d.Destination = kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: "CNPVG"}, kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: "DEFRA"}
	d.GrossWeight, d.Volume = kernel.Quantity{Unit: "KGM", Amount: 100}, kernel.Quantity{Unit: "MTQ", Amount: 1}
	f.w.drivers["SH-B"] = d
	f.w.shipmentLines = []rateapi.PricingLine{
		{LineNo: 1, ChargeCode: "AIR_FREIGHT", Source: rateapi.SourceRated, HasSell: true, Sell: usd(120_000), HasCost: true, Cost: usd(95_000),
			CreditorPartyID: "airline-mu"},
		{LineNo: 2, ChargeCode: "DOC_FEE", Source: rateapi.SourceRated, HasSell: true, Sell: cny(30_000)},
		{LineNo: 3, ChargeCode: "THC", Source: rateapi.SourceRated, HasCost: true, Cost: cny(8_000), CreditorPartyID: "trucker"},
	}
	f.w.mu.Unlock()

	rate := api.RateFreightJob{CommandID: "rate-B", TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-B", ExpectedRevision: 2, ServiceLevel: "STANDARD",
		OccurredAt: a.hours(3)}
	r := a.must(f.module.RateFreightJob(ctx, rate))
	if r.Version != 3 {
		t.Fatalf("rated: %+v", r)
	}
	f.w.mu.Lock()
	asked := f.w.asked[0].(rateapi.RateShipmentEstimate)
	f.w.mu.Unlock()
	if asked.Origin != "CNPVG" || asked.Destination != "DEFRA" || asked.Mode != "AIR" || asked.ContainerMode != rateapi.ContainerLoose ||
		asked.Cargo.GrossGrams != 100_000 || asked.Cargo.VolumeCM3 != 1_000_000 || asked.Cargo.Pieces != 6 || asked.CustomerPartyID != "party-a" ||
		asked.ShipmentID != "SH-B" || asked.PlanVersion != 2 || asked.ServiceLevel != "STANDARD" || !strings.HasPrefix(asked.BasisID, "FIN-") ||
		asked.CommandID != "fin/rate-B" {
		t.Fatalf("asked RATE: %+v", asked)
	}
	if again, err := f.module.RateFreightJob(ctx, rate); err != nil || again != r {
		t.Fatalf("replay: %+v %v", again, err)
	}
	job, err := f.module.GetFreightJob(ctx, api.GetFreightJob{TenantID: f.tenant, JobID: "JOB-B"})
	if err != nil {
		t.Fatal(err)
	}
	if len(job.Bases) != 1 || job.Bases[0].Purpose != "SHIPMENT_ESTIMATE" || job.Bases[0].PlanVersion != 2 || job.Bases[0].BasisID != asked.BasisID ||
		len(job.Lines) != 4 {
		t.Fatalf("job: %+v", job)
	}
	freight, doc, thc := job.Lines[1], job.Lines[2], job.Lines[3]
	switch {
	case freight.Source != "RATED" || freight.QuoteLineNo != 1 || len(freight.Sell) != 1 || freight.Sell[0].PartyID != "party-a" ||
		freight.Sell[0].Amount != usd(120_000) || len(freight.Cost) != 1 || freight.Cost[0].PartyID != "airline-mu" || freight.Cost[0].Amount != usd(95_000):
		t.Errorf("freight: %+v", freight)
	case doc.Source != "RATED" || len(doc.Sell) != 1 || len(doc.Cost) != 0:
		t.Errorf("doc fee: %+v", doc)
	case thc.Source != "RATED" || len(thc.Sell) != 0 || len(thc.Cost) != 1 || thc.Cost[0].PartyID != "trucker" || thc.Cost[0].Local != cny(8_000):
		t.Errorf("THC: %+v", thc)
	}

	// a job is priced by its quotes or by one rated estimate
	again := rate
	again.CommandID, again.ExpectedRevision = "rate-B-again", job.Revision
	_, err = f.module.RateFreightJob(ctx, again)
	a.rejected("a second estimate", err, "FIN_BASIS_APPLIED")
	_, err = f.module.ApplyPricingBasis(ctx, api.ApplyPricingBasis{CommandID: "quote-B", TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-B",
		ExpectedRevision: job.Revision, QuoteID: "Q-1", OrderID: "O-A", OccurredAt: a.hours(3)})
	a.rejected("a quote on a rated job", err, "FIN_BASIS_APPLIED")
	a.must(f.module.ApplyPricingBasis(ctx, api.ApplyPricingBasis{CommandID: "quote-A", TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-A",
		ExpectedRevision: 2, QuoteID: "Q-1", OrderID: "O-A", OccurredAt: a.hours(3)}))
	_, err = f.module.RateFreightJob(ctx, api.RateFreightJob{CommandID: "rate-A", TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-A", ExpectedRevision: 3,
		ServiceLevel: "STANDARD", OccurredAt: a.hours(3)})
	a.rejected("an estimate on a job priced by a quote", err, "FIN_BASIS_APPLIED")

	// a basis line becomes one charge line
	dup := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		for _, s := range []string{`UPDATE cargo_fin.freight_job SET revision=revision+1 WHERE tenant_id=$1 AND job_id='JOB-B'`,
			`INSERT INTO cargo_fin.charge_line (tenant_id,job_id,line_no,charge_code,source,basis_no,quote_line_no,recorded_by,recorded_at,entered_at)
               VALUES ($1,'JOB-B',9,'AIR_FREIGHT','RATED',1,1,'x',now(),now())`} {
			if _, err := e.Exec(ctx, s, f.tenant); err != nil {
				return err
			}
		}
		return nil
	})
	if state, constraint := sqlState(dup); state != "23505" || constraint != "charge_line_basis_line_once" {
		t.Errorf("a basis line twice: %v", dup)
	}

	// a supplier ORG does not confirm as a creditor, a shipment without a route, and RATE refusing
	for _, s := range []string{"SH-C", "SH-D", "SH-E"} {
		f.w.mu.Lock()
		c := d
		c.ShipmentID = s
		if s == "SH-D" {
			c.Origin = kernel.LocationCode{}
		}
		f.w.drivers[s] = c
		f.w.mu.Unlock()
		a.must(f.module.OpenFreightJob(ctx, api.OpenFreightJob{CommandID: "open-" + s, TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-" + s,
			ShipmentID: s, OccurredAt: a.hours(3)}))
	}
	f.w.mu.Lock()
	f.w.shipmentLines = append(f.w.shipmentLines, rateapi.PricingLine{LineNo: 4, ChargeCode: "PICKUP", Source: rateapi.SourceRated, HasCost: true,
		Cost: cny(1), CreditorPartyID: "party-a"})
	f.w.mu.Unlock()
	rateJob := func(job string) error {
		_, err := f.module.RateFreightJob(ctx, api.RateFreightJob{CommandID: "rate-" + job, TenantID: f.tenant, ActorID: "fin-1", JobID: job,
			ExpectedRevision: 1, ServiceLevel: "STANDARD", OccurredAt: a.hours(4)})
		return err
	}
	a.rejected("a customer as supplier", rateJob("JOB-SH-C"), "FIN_PARTY_ROLE")
	a.rejected("a shipment without a route", rateJob("JOB-SH-D"), "FIN_NOT_PRICEABLE")
	f.w.mu.Lock()
	f.w.rateRefusal = errors.New("no published card for the lane")
	f.w.mu.Unlock()
	a.rejected("RATE refusing", rateJob("JOB-SH-E"), "FIN_NOT_PRICEABLE")
	f.w.mu.Lock()
	f.w.rateRefusal = nil
	f.w.consolLines = []rateapi.PricingLine{
		{LineNo: 1, ChargeCode: "AIR_FREIGHT", Source: rateapi.SourceRated, HasCost: true, Cost: usd(90_000), CreditorPartyID: "airline-mu"},
		{LineNo: 2, ChargeCode: "SECURITY_CHARGE", Source: rateapi.SourceRated, HasCost: true, Cost: cny(5_000), CreditorPartyID: "airline-mu"},
	}
	f.w.mu.Unlock()

	// the pool of CONSOL-2 rated on the BUY side: the carrier's costs as RATED lines
	a.must(f.module.OpenConsolCostPool(ctx, api.OpenConsolCostPool{CommandID: "pool-2", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-2",
		ConsolidationID: "CONSOL-2", OccurredAt: a.hours(4)}))
	ratePool := api.RateConsolCostPool{CommandID: "rate-P-2", TenantID: f.tenant, ActorID: "fin-1", PoolID: "P-2", ExpectedRevision: 1,
		Carrier: "airline-mu", Origin: "CNPVG", Destination: "DEFRA", ServiceLevel: "STANDARD", Method: api.MethodChargeable, OccurredAt: a.hours(4)}
	if r = a.must(f.module.RateConsolCostPool(ctx, ratePool)); r.Version != 2 {
		t.Fatalf("pool rated: %+v", r)
	}
	f.w.mu.Lock()
	consol := f.w.asked[len(f.w.asked)-1].(rateapi.RateConsolCost)
	f.w.mu.Unlock()
	if consol.Carrier != "airline-mu" || consol.ConsolidationID != "CONSOL-2" || consol.ConsolidationRevision != 7 || consol.Cargo.Pieces != 12 ||
		consol.Cargo.GrossGrams != 400_000 || consol.Cargo.VolumeCM3 != 2_400_000 || consol.Mode != "AIR" {
		t.Fatalf("asked RATE for the consolidation: %+v", consol)
	}
	pool, err := f.module.GetConsolCostPool(ctx, api.GetConsolCostPool{TenantID: f.tenant, PoolID: "P-2"})
	if err != nil || len(pool.Lines) != 2 || pool.Lines[0].Source != "RATED" || pool.Lines[0].BasisID != consol.BasisID || pool.Lines[1].BasisLineNo != 2 ||
		pool.Lines[0].Versions[0].CreditorPartyID != "airline-mu" || pool.Lines[0].Method != api.MethodChargeable {
		t.Fatalf("pool: %+v %v", pool, err)
	}
	ratePool.CommandID, ratePool.ExpectedRevision = "rate-P-2-again", pool.Revision
	_, err = f.module.RateConsolCostPool(ctx, ratePool)
	a.rejected("a second estimate of the pool", err, "FIN_BASIS_APPLIED")

	// RATE answered, then FIN failed (a EUR cost has no ESTIMATE rate); retried after FUL's plan moved on, the command id
	// is refused as reused: RATE rated the earlier plan under it
	f.w.mu.Lock()
	f.w.shipmentLines = []rateapi.PricingLine{{LineNo: 1, ChargeCode: "AIR_FREIGHT", Source: rateapi.SourceRated, HasCost: true,
		Cost: kernel.Money{Currency: "EUR", Minor: 100}, CreditorPartyID: "airline-mu"}}
	e := f.w.drivers["SH-E"]
	f.w.mu.Unlock()
	retry := api.RateFreightJob{CommandID: "rate-E-eur", TenantID: f.tenant, ActorID: "fin-1", JobID: "JOB-SH-E", ExpectedRevision: 1, ServiceLevel: "STANDARD",
		OccurredAt: a.hours(4)}
	_, err = f.module.RateFreightJob(ctx, retry)
	a.rejected("a cost in a currency without an ESTIMATE rate", err, "FIN_RATE_MISSING")
	f.w.mu.Lock()
	e.PlanVersion = 3
	f.w.drivers["SH-E"] = e
	f.w.mu.Unlock()
	if _, err = f.module.RateFreightJob(ctx, retry); !errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("a retry after the plan changed: %v", err)
	}

	// past FIN: a second estimate on the job, a RATED line on a quote basis, a second estimate in the pool
	for name, tc := range map[string]struct{ sql, constraint string }{
		"a second estimate on a job": {`UPDATE cargo_fin.freight_job SET revision=revision+1 WHERE tenant_id=$1 AND job_id='JOB-B';
INSERT INTO cargo_fin.job_basis (tenant_id,job_id,basis_no,basis_id,purpose,plan_version,applied_by,applied_at,entered_at)
            VALUES ($1,'JOB-B',2,'FIN-X','SHIPMENT_ESTIMATE',2,'x',now(),now())`, "job_basis_one_kind"},
		"a RATED line on a quote basis": {`UPDATE cargo_fin.freight_job SET revision=revision+1 WHERE tenant_id=$1 AND job_id='JOB-A';
INSERT INTO cargo_fin.charge_line (tenant_id,job_id,line_no,charge_code,source,basis_no,quote_line_no,recorded_by,recorded_at,entered_at)
            VALUES ($1,'JOB-A',9,'THC','RATED',1,1,'x',now(),now())`, "charge_line_of_its_basis"},
		"an estimate without its plan version": {`UPDATE cargo_fin.freight_job SET revision=revision+1 WHERE tenant_id=$1 AND job_id='JOB-SH-D';
INSERT INTO cargo_fin.job_basis (tenant_id,job_id,basis_no,basis_id,purpose,applied_by,applied_at,entered_at)
            VALUES ($1,'JOB-SH-D',1,'FIN-Z','SHIPMENT_ESTIMATE','x',now(),now())`, "job_basis_purpose"},
		"a pool's basis on a job": {`UPDATE cargo_fin.freight_job SET revision=revision+1 WHERE tenant_id=$1 AND job_id='JOB-SH-D';
INSERT INTO cargo_fin.job_basis (tenant_id,job_id,basis_no,basis_id,purpose,plan_version,applied_by,applied_at,entered_at)
            VALUES ($1,'JOB-SH-D',1,'` + pool.Lines[0].BasisID + `','SHIPMENT_ESTIMATE',2,'x',now(),now())`, "job_basis_one_kind"},
		"a basis in two pools": {`UPDATE cargo_fin.consol_cost_pool SET revision=revision+1 WHERE tenant_id=$1 AND pool_id='P-1';
INSERT INTO cargo_fin.pool_cost_line (tenant_id,pool_id,line_no,charge_code,method,recorded_by,recorded_at,entered_at,source,basis_id,basis_line_no)
            VALUES ($1,'P-1',9,'THC','PIECES','x',now(),now(),'RATED','` + pool.Lines[0].BasisID + `',3)`, "pool_one_estimate"},
		"a second estimate in a pool": {`UPDATE cargo_fin.consol_cost_pool SET revision=revision+1 WHERE tenant_id=$1 AND pool_id='P-2';
INSERT INTO cargo_fin.pool_cost_line (tenant_id,pool_id,line_no,charge_code,method,recorded_by,recorded_at,entered_at,source,basis_id,basis_line_no)
            VALUES ($1,'P-2',9,'THC','PIECES','x',now(),now(),'RATED','FIN-Y',1)`, "pool_one_estimate"},
	} {
		err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			for _, s := range strings.Split(tc.sql, ";\n") {
				if _, err := e.Exec(ctx, s, f.tenant); err != nil {
					return err
				}
			}
			return nil
		})
		if state, constraint := sqlState(err); (state != "23514" && state != "23505") || constraint != tc.constraint {
			t.Errorf("%s: %v (%s %s)", name, err, state, constraint)
		}
	}
}

// TestEstimatesRaceForOneJob races two rated estimates of JOB-B at the same revision: exactly one is applied, the
// other hears a revision conflict (or, having read the job after, FIN_BASIS_APPLIED); RATE issues a basis for each,
// and the loser's basis is applied to no job.
func TestEstimatesRaceForOneJob(t *testing.T) {
	setup := func(t *testing.T) ap {
		a := creditorWorld(t)
		a.w.mu.Lock()
		d := a.w.drivers["SH-B"]
		d.Origin, d.Destination = kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: "CNPVG"}, kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: "DEFRA"}
		d.GrossWeight = kernel.Quantity{Unit: "KGM", Amount: 100}
		a.w.drivers["SH-B"] = d
		a.w.shipmentLines = []rateapi.PricingLine{{LineNo: 1, ChargeCode: "DOC_FEE", Source: rateapi.SourceRated, HasSell: true, Sell: cny(30_000)}}
		a.w.mu.Unlock()
		return a
	}
	rate := func(id string) func(a ap) error {
		return func(a ap) error {
			_, err := a.module.RateFreightJob(a.ctx, api.RateFreightJob{CommandID: id, TenantID: a.tenant, ActorID: "fin-1", JobID: "JOB-B", ExpectedRevision: 2,
				ServiceLevel: "STANDARD", OccurredAt: a.hours(3)})
			return err
		}
	}
	race(t, setup, rate("rate-1"), rate("rate-2"), func(round int, _ bool, err error) {
		if !errors.Is(err, api.ErrConflict) && !strings.Contains(err.Error(), "FIN_BASIS_APPLIED") {
			t.Fatalf("round %d: lost with %v", round, err)
		}
	})
}
