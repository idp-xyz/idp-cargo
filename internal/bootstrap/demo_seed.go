package bootstrap

// The test tenant of EII: SeedDemoTenant seeds the tenant eii-test through the contexts' real commands: ORG's roles,
// RATE's quotes, COM's orders, FUL's shipments, PHY's consolidations, stuffings, rework and transfer, and FIN's jobs,
// charge lines and cost pools (cmd/cargo-demo runs it; process entry points reach the contexts only through this
// package).
//
// Every amount, id and business time is fixed in the program (the business times are hours from the day's start), so
// that a second run sends the same commands, which the contexts answer from their receipts: it changes nothing. The one
// thing it writes otherwise is ORG's master data (the parties and the legal entity), which ORG has no command for: the
// cross-context tests write the same rows as fixtures, and so does this seed, with a statement of its own
// (demoOrgMasterData), until the IDP identity platform is connected.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	rateapi "go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// SeedDemoTenant seeds the tenant, with the day's start at (the hours of every command's business time count from it),
// on the database of the application role in dsn, and prints what it seeded to out.
//
// It writes ORG's master data (five parties and the legal entity LE) with statements of its own, because ORG has no
// command that makes them, so it writes only test tenants: a tenant id that does not start with "eii-" or "demo-" is
// refused.
func SeedDemoTenant(ctx context.Context, dsn, tenant string, at time.Time, out io.Writer) error {
	if !strings.HasPrefix(tenant, "eii-") && !strings.HasPrefix(tenant, "demo-") {
		return fmt.Errorf("the demo seed writes test tenants only, and %q is not one: name a tenant that starts with eii- or demo- (for example eii-test)", tenant)
	}
	platform, err := Open(ctx, Config{PostgresDSN: dsn})
	if err != nil {
		return err
	}
	defer platform.Close()
	if err = demoOrgMasterData(ctx, dsn, tenant); err != nil {
		return err
	}
	s := &demoSeeder{ctx: ctx, tenant: tenant, at: at.UTC().Truncate(time.Microsecond), out: out}
	if err = s.open(platform); err != nil {
		return err
	}
	if err = s.run(); err != nil {
		return err
	}
	return s.report()
}

// demoAllowAll authorizes every action: the seed runs as named actors of the test tenant, not as the API's callers.
type demoAllowAll struct{}

func (demoAllowAll) AuthorizeShipmentAction(context.Context, string, string, string, string) error {
	return nil
}
func (demoAllowAll) AuthorizeRoleApproval(context.Context, string, string, string, string) error {
	return nil
}
func (demoAllowAll) AuthorizePartyAction(context.Context, string, string, string, string) error {
	return nil
}
func (demoAllowAll) AuthorizeQuoteAction(context.Context, string, string, string, string) error {
	return nil
}
func (demoAllowAll) AuthorizeOrderAction(context.Context, string, string, string, string) error {
	return nil
}
func (demoAllowAll) AuthorizeConsolidationAction(context.Context, string, string, string, string) error {
	return nil
}
func (demoAllowAll) AuthorizeFinAction(context.Context, string, string, string, string) error {
	return nil
}

type demoMargin int

func (f demoMargin) MarginFloorBP(context.Context, string, string) (int, error) { return int(f), nil }

// demoCustomers are the three demoCustomers of the tenant; the carrier, the trucker and the agent are its creditors.
var demoCustomers = []struct{ id, name string }{
	{"cust-huaxin", "Shanghai Huaxin Trading Co., Ltd."}, {"cust-lanxin", "Ningbo Lanxin Import and Export Co., Ltd."}, {"cust-orient", "Shenzhen Orient Home Goods Co., Ltd."},
}

// demoJob is an ordinary house job of the seed: its ocean freight sold in the quote, a document fee sold, and three
// costs, in CNY minor units. Its profit is freight + doc - ocean - thc - truck.
type demoJob struct{ freight, doc, ocean, thc, truck int64 }

func (j demoJob) profit() int64 { return j.freight + j.doc - j.ocean - j.thc - j.truck }

// demoJobs are 27 ordinary jobs: 17 at a profit, 7 at a loss and 3 at exactly zero.
var demoJobs = []demoJob{
	{450000, 30000, 320000, 45000, 20000}, {380000, 30000, 290000, 40000, 18000}, {520000, 35000, 360000, 52000, 25000}, {275000, 25000, 200000, 35000, 15000},
	{610000, 40000, 430000, 60000, 30000}, {330000, 30000, 240000, 38000, 16000}, {415000, 30000, 300000, 42000, 22000}, {298000, 25000, 215000, 36000, 14000},
	{560000, 35000, 400000, 55000, 28000}, {240000, 20000, 170000, 30000, 12000}, {480000, 30000, 350000, 46000, 21000}, {365000, 30000, 265000, 39000, 17000},
	{505000, 35000, 370000, 50000, 26000}, {310000, 25000, 225000, 34000, 15000}, {445000, 30000, 320000, 44000, 20000}, {275000, 25000, 205000, 33000, 13000},
	{590000, 40000, 410000, 58000, 29000},
	{200000, 15000, 230000, 40000, 18000}, {310000, 20000, 320000, 50000, 25000}, {180000, 10000, 205000, 35000, 20000}, {420000, 25000, 430000, 60000, 30000},
	{150000, 12000, 175000, 30000, 15000}, {275000, 20000, 285000, 45000, 22000}, {360000, 30000, 380000, 55000, 28000},
	{300000, 20000, 230000, 60000, 30000}, {250000, 15000, 200000, 45000, 20000}, {400000, 30000, 320000, 70000, 40000},
}

func demoCNY(minor int64) kernel.Money { return kernel.Money{Currency: "CNY", Minor: minor} }

// demoOrgMasterData writes the parties and the legal entity LE that ORG has no command for, once (a second run finds
// them).
func demoOrgMasterData(ctx context.Context, dsn, tenant string) error {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, tenant); err != nil {
		return err
	}
	parties := []struct{ id, name, country string }{{"line-1", "Oriental Ocean Line Co., Ltd.", "CN"}, {"truck-1", "Shanghai Baosheng Trucking Co., Ltd.", "CN"}}
	for _, c := range demoCustomers {
		parties = append(parties, struct{ id, name, country string }{c.id, c.name, "CN"})
	}
	for _, p := range parties {
		if _, err = tx.Exec(ctx, `INSERT INTO cargo_org.business_party(tenant_id,party_id,legal_name,registration_country,status) VALUES($1,$2,$3,$4,'ACTIVE')
          ON CONFLICT DO NOTHING`, tenant, p.id, p.name, p.country); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO cargo_org.legal_entity(tenant_id,legal_entity_id,registered_name,jurisdiction,registration_evidence_ref,status)
      VALUES($1,'LE','Shanghai Cargo Forwarding Co., Ltd.','CN','registry-verified','VERIFIED') ON CONFLICT DO NOTHING`, tenant); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type demoSeeder struct {
	ctx    context.Context
	tenant string
	at     time.Time
	out    io.Writer

	org interface {
		ApproveRole(context.Context, orgapi.ApproveRole) (orgapi.Receipt, error)
	}
	rate interface {
		CreateQuote(context.Context, rateapi.CreateQuote) (rateapi.Receipt, error)
		AcceptQuote(context.Context, rateapi.AcceptQuote) (rateapi.Receipt, error)
	}
	com interface {
		RecordCustomerOrder(context.Context, comapi.RecordCustomerOrder) (comapi.Receipt, error)
		AllocateOrderToObligation(context.Context, comapi.AllocateOrderToObligation) (comapi.Receipt, error)
		ReserveObligationForFulfillment(context.Context, comapi.ReserveObligationForFulfillment) (comapi.Receipt, error)
	}
	ful interface {
		CreateShipmentFromObligation(context.Context, fulapi.CreateShipmentFromObligation) (fulapi.Receipt, error)
		AmendShipmentPlan(context.Context, fulapi.AmendShipmentPlan) (fulapi.Receipt, error)
		ConfirmShipmentPlan(context.Context, fulapi.ConfirmShipmentPlan) (fulapi.Receipt, error)
		RecordConsolAttachment(context.Context, fulapi.RecordConsolAttachment) (fulapi.Receipt, error)
	}
	phy interface {
		phyapi.Commands
		GetConsolidation(context.Context, phyapi.GetConsolidation) (phyapi.Consolidation, error)
	}
	fin interface {
		finapi.Commands
		finapi.Queries
	}
}

func (s *demoSeeder) hours(h int) time.Time { return s.at.Add(time.Duration(h) * time.Hour) }

func (s *demoSeeder) open(p *Platform) error {
	org, err := p.ORG(demoAllowAll{})
	if err != nil {
		return err
	}
	rate, err := p.RATE(org, demoAllowAll{}, demoMargin(2000))
	if err != nil {
		return err
	}
	com, err := p.COM(org, rate, demoAllowAll{})
	if err != nil {
		return err
	}
	ful, phy, err := p.FULAndPHY(com, demoAllowAll{}, demoAllowAll{})
	if err != nil {
		return err
	}
	fin, err := p.FIN(org, rate, ful, &DocumentSnapshots{}, phy, demoAllowAll{})
	if err != nil {
		return err
	}
	s.org, s.rate, s.com, s.ful, s.phy, s.fin = org, rate, com, ful, phy, fin
	return nil
}

func (s *demoSeeder) logf(format string, a ...any) { fmt.Fprintf(s.out, format+"\n", a...) }

// plan runs the eight steps of the day's order of a customer for one shipment: a one-off quote in CNY with one line of
// ocean freight sold, its acceptance, the customer's order, its allocation and reservation, the shipment from the
// obligation, its plan amended and confirmed.
func (s *demoSeeder) plan(party, suffix string, freight int64) error {
	ctx, tenant, at := s.ctx, s.tenant, s.at
	unlocode := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	weight := kernel.Quantity{Unit: "KGM", Amount: 300}
	departs := at.Add(72 * time.Hour)
	steps := []func() error{
		func() error {
			_, err := s.rate.CreateQuote(ctx, rateapi.CreateQuote{CommandID: "quote-" + suffix, TenantID: tenant, ActorID: "sales-1", QuoteID: "Q-" + suffix, LegalEntityID: "LE",
				CustomerPartyID: party, Currency: "CNY", Lines: []rateapi.QuoteLine{{ChargeCode: "OCEAN_FREIGHT", Sell: demoCNY(freight), Cost: demoCNY(0)}},
				Validity: kernel.ValidityInterval{From: at, Until: at.Add(14 * 24 * time.Hour)}, OccurredAt: at.Add(time.Minute)})
			return err
		},
		func() error {
			_, err := s.rate.AcceptQuote(ctx, rateapi.AcceptQuote{CommandID: "accept-" + suffix, TenantID: tenant, ActorID: "sales-1", QuoteID: "Q-" + suffix, ExpectedRevision: 1,
				EvidenceKind: rateapi.EvidenceEmail, EvidenceRef: "mail", OccurredAt: at.Add(time.Hour)})
			return err
		},
		func() error {
			_, err := s.com.RecordCustomerOrder(ctx, comapi.RecordCustomerOrder{CommandID: "order-" + suffix, TenantID: tenant, ActorID: "cs-1", OrderID: "O-" + suffix, QuoteID: "Q-" + suffix,
				CustomerPartyID: party, OccurredAt: at.Add(2 * time.Hour), Transport: comapi.Transport{Mode: comapi.ModeSea, ContainerMode: comapi.ContainerLCL},
				Lines: []comapi.OrderLine{{Description: "Cartons of furniture parts", Packs: 6, PackageType: "CT", GrossWeight: weight}}})
			return err
		},
		func() error {
			_, err := s.com.AllocateOrderToObligation(ctx, comapi.AllocateOrderToObligation{CommandID: "allocate-" + suffix, TenantID: tenant, ActorID: "planner-1", OrderID: "O-" + suffix,
				ExpectedRevision: 1, LineNo: 1, ObligationID: "OB-" + suffix, Packs: 6, GrossWeight: weight, OccurredAt: at.Add(3 * time.Hour)})
			return err
		},
		func() error {
			_, err := s.com.ReserveObligationForFulfillment(ctx, comapi.ReserveObligationForFulfillment{CommandID: "reserve-" + suffix, TenantID: tenant, ActorID: "fulfilment",
				ObligationID: "OB-" + suffix, ObligationVersion: 1, ShipmentID: "SH-" + suffix, OccurredAt: at.Add(4 * time.Hour)})
			return err
		},
		func() error {
			_, err := s.ful.CreateShipmentFromObligation(ctx, fulapi.CreateShipmentFromObligation{CommandID: "create-" + suffix, TenantID: tenant, ActorID: "planner-1",
				ShipmentID: "SH-" + suffix, ObligationID: "OB-" + suffix, ObligationVersion: 1, OccurredAt: at.Add(5 * time.Hour)})
			return err
		},
		func() error {
			_, err := s.ful.AmendShipmentPlan(ctx, fulapi.AmendShipmentPlan{CommandID: "plan-" + suffix, TenantID: tenant, ActorID: "planner-1", ShipmentID: "SH-" + suffix, ExpectedRevision: 1,
				TransportMode: fulapi.ModeSea, ContainerMode: fulapi.ContainerLCL, Origin: unlocode("CNSHA"), Destination: unlocode("DEHAM"),
				DepartureWindow: kernel.ValidityInterval{From: departs, Until: departs.Add(48 * time.Hour)}, Reason: "vessel booked", OccurredAt: at.Add(6 * time.Hour)})
			return err
		},
		func() error {
			_, err := s.ful.ConfirmShipmentPlan(ctx, fulapi.ConfirmShipmentPlan{CommandID: "confirm-" + suffix, TenantID: tenant, ActorID: "planner-2", ShipmentID: "SH-" + suffix,
				ExpectedRevision: 2, OccurredAt: at.Add(7 * time.Hour)})
			return err
		},
	}
	for i, step := range steps {
		if err := step(); err != nil {
			return fmt.Errorf("plan %s, step %d: %w", suffix, i+1, err)
		}
	}
	return nil
}

// openJob opens the shipment's job and applies its quote; it answers the job's revision.
func (s *demoSeeder) openJob(suffix string) (int64, error) {
	if _, err := s.fin.OpenFreightJob(s.ctx, finapi.OpenFreightJob{CommandID: "fin-job-" + suffix, TenantID: s.tenant, ActorID: "fin-1", JobID: "JOB-" + suffix, ShipmentID: "SH-" + suffix,
		OccurredAt: s.hours(10)}); err != nil {
		return 0, fmt.Errorf("open JOB-%s: %w", suffix, err)
	}
	r, err := s.fin.ApplyPricingBasis(s.ctx, finapi.ApplyPricingBasis{CommandID: "fin-apply-" + suffix, TenantID: s.tenant, ActorID: "fin-1", JobID: "JOB-" + suffix, ExpectedRevision: 1,
		QuoteID: "Q-" + suffix, OrderID: "O-" + suffix, OccurredAt: s.hours(11)})
	if err != nil {
		return 0, fmt.Errorf("apply the quote to JOB-%s: %w", suffix, err)
	}
	return r.NewRevision, nil
}

// line records a charge line of a job; it answers the job's new revision.
func (s *demoSeeder) line(suffix, tag string, revision int64, code string, sell, cost *finapi.ChargeSide, reason string) (int64, error) {
	r, err := s.fin.RecordChargeLine(s.ctx, finapi.RecordChargeLine{CommandID: "fin-line-" + suffix + "-" + tag, TenantID: s.tenant, ActorID: "fin-1", JobID: "JOB-" + suffix, ExpectedRevision: revision,
		ChargeCode: code, Sell: sell, Cost: cost, Reason: reason, OccurredAt: s.hours(12)})
	if err != nil {
		return 0, fmt.Errorf("line %s of JOB-%s: %w", tag, suffix, err)
	}
	return r.NewRevision, nil
}

func (s *demoSeeder) run() error {
	ctx, tenant, at := s.ctx, s.tenant, s.at
	// finance policy of the legal entity, the carrier's and the trucker's roles, the demoCustomers' roles
	if _, err := s.fin.OpenFinancePolicy(ctx, finapi.OpenFinancePolicy{CommandID: "fin-policy", TenantID: tenant, ActorID: "acct-1", LegalEntityID: "LE", FunctionalCurrency: "CNY",
		BookCode: "MAIN", OccurredAt: s.hours(10)}); err != nil {
		return fmt.Errorf("finance policy: %w", err)
	}
	roles := []struct{ party, role, evidence string }{{"line-1", orgapi.RoleCarrier, "service contract"}, {"truck-1", "CO_LOADER", "haulage agreement"}}
	for _, c := range demoCustomers {
		roles = append(roles, struct{ party, role, evidence string }{c.id, "CUSTOMER", "contract"})
	}
	for _, r := range roles {
		if _, err := s.org.ApproveRole(ctx, orgapi.ApproveRole{CommandID: "role-" + r.party, TenantID: tenant, ActorID: "org-officer", LegalEntityID: "LE", PartyID: r.party, ExpectedRevision: 1,
			RoleCode: r.role, ValidFrom: at.Add(-time.Hour), EvidenceRef: r.evidence, OccurredAt: at}); err != nil {
			return fmt.Errorf("role %s of %s: %w", r.role, r.party, err)
		}
	}
	// the 27 ordinary jobs
	for i, j := range demoJobs {
		suffix := fmt.Sprintf("S%02d", i+1)
		customer := demoCustomers[i%len(demoCustomers)].id
		if err := s.plan(customer, suffix, j.freight); err != nil {
			return err
		}
		revision, err := s.openJob(suffix)
		if err != nil {
			return err
		}
		for _, l := range []struct {
			tag, code    string
			sell, cost   *finapi.ChargeSide
			reason       string
			amountOfLine int64
		}{
			{"doc", "DOC_FEE", &finapi.ChargeSide{PartyID: customer, Amount: demoCNY(j.doc)}, nil, "house document fee", j.doc},
			{"ocean", "OCEAN_FREIGHT", nil, &finapi.ChargeSide{PartyID: "line-1", Amount: demoCNY(j.ocean)}, "the line's freight for the shipment", j.ocean},
			{"thc", "THC_ORIGIN", nil, &finapi.ChargeSide{PartyID: "line-1", Amount: demoCNY(j.thc)}, "terminal handling at origin", j.thc},
			{"truck", "TRUCKING", nil, &finapi.ChargeSide{PartyID: "truck-1", Amount: demoCNY(j.truck)}, "pickup at the factory", j.truck},
		} {
			if revision, err = s.line(suffix, l.tag, revision, l.code, l.sell, l.cost, l.reason); err != nil {
				return err
			}
		}
	}
	s.logf("seeded %d ordinary jobs", len(demoJobs))
	return s.boxA()
}

// boxA is the owner's box A (V2.93's cross-context test): C-1 with the houses H01, H02 and H03 of 10, 15 and 5 W/M in
// one container, the line's freight of CNY 3,000.00 apportioned 1,000.00, 1,500.00 and 500.00; H03 unloaded in a rework
// that ops-1 asked for, the container sealed again, H03 transferred to C-2 by ops-9 and C-2's pool P-2 of CNY 100.01
// apportioned to it; then finance attributes the former share of H03 in C-1: loss 100.00 and exception cost 200.00,
// voucher CLAIM-1.
func (s *demoSeeder) boxA() error {
	ctx, tenant := s.ctx, s.tenant
	unlocode := func(code string) kernel.LocationCode { return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code} }
	evidence := func(source, ref string, h int) phyapi.Evidence {
		return phyapi.Evidence{Source: source, Reference: ref, OccurredAt: s.hours(h)}
	}
	step := func(what string, err error) error {
		if err != nil {
			return fmt.Errorf("box A, %s: %w", what, err)
		}
		return nil
	}
	houses := []struct {
		id      string
		party   string
		freight int64
		kilos   int64
		cm3     int64
	}{{"H01", "cust-huaxin", 380000, 312, 10_000_000}, {"H02", "cust-lanxin", 300000, 300, 15_000_000}, {"H03", "cust-orient", 12000, 288, 5_000_000}}
	for _, h := range houses {
		if err := s.plan(h.party, h.id, h.freight); err != nil {
			return err
		}
	}
	departs := s.at.Add(72 * time.Hour)
	consol := func(id string, hour int, command string) error {
		_, err := s.phy.CreateConsolidation(ctx, phyapi.CreateConsolidation{CommandID: command, TenantID: tenant, ActorID: "ops-1", ConsolidationID: id, Mode: phyapi.ModeSea,
			Kind: phyapi.KindSeaLCLOwn, OperatingLegalEntityID: "LE", Origin: unlocode("CNSHA"), Destination: unlocode("DEHAM"), PlannedDeparture: departs.Add(6 * time.Hour),
			HouseCutOff: departs.Add(-6 * time.Hour), OccurredAt: s.hours(hour)})
		return err
	}
	if err := step("create C-1", consol("C-1", 8, "consol")); err != nil {
		return err
	}
	consolRevision := int64(1)
	r, err := s.phy.PlanLoadUnit(ctx, phyapi.PlanLoadUnit{CommandID: "plan-unit-1", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1", ExpectedRevision: consolRevision,
		Kind: phyapi.LoadUnitContainer, TypeCode: "22G1", OccurredAt: s.hours(8)})
	if err = step("plan the container", err); err != nil {
		return err
	}
	consolRevision = r.NewRevision
	for _, h := range houses {
		sh := "SH-" + h.id
		if _, err = s.phy.AttachHouse(ctx, phyapi.AttachHouse{CommandID: "attach-" + sh, TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1", ShipmentID: sh, OccurredAt: s.hours(9)}); err != nil {
			return step("attach "+sh, err)
		}
		if _, err = s.ful.RecordConsolAttachment(ctx, fulapi.RecordConsolAttachment{CommandID: "ful-attach-" + sh, TenantID: tenant, ActorID: "ops-1", ShipmentID: sh, ExpectedRevision: 3,
			ConsolidationID: "C-1", OccurredAt: s.hours(9)}); err != nil {
			return step("FUL records "+sh, err)
		}
	}
	for _, h := range houses {
		if _, err = s.openJob(h.id); err != nil {
			return err
		}
	}
	var poolRevision int64
	pr, err := s.fin.OpenConsolCostPool(ctx, finapi.OpenConsolCostPool{CommandID: "fin-pool", TenantID: tenant, ActorID: "fin-1", PoolID: "P-1", ConsolidationID: "C-1", OccurredAt: s.hours(12)})
	if err = step("open P-1", err); err != nil {
		return err
	}
	poolRevision = pr.NewRevision
	if _, err = s.phy.CloseConsolidation(ctx, phyapi.CloseConsolidation{CommandID: "close", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-1", ExpectedRevision: consolRevision,
		OccurredAt: s.hours(13)}); err != nil {
		return step("close C-1", err)
	}
	// the facility lies where the consolidation departs from, as PHY keeps its code
	c1, err := s.phy.GetConsolidation(ctx, phyapi.GetConsolidation{TenantID: tenant, ConsolidationID: "C-1"})
	if err = step("read C-1", err); err != nil {
		return err
	}
	cfs := phyapi.Facility{Location: c1.Current.Origin, Code: "SHA-CFS1"}
	for _, h := range houses {
		lot := "L-" + h.id
		if _, err = s.phy.ReceiveCargo(ctx, phyapi.ReceiveCargo{CommandID: "rcv-" + h.id, TenantID: tenant, ActorID: "cfs-1", LotID: lot, ShipmentID: "SH-" + h.id, Facility: cfs, Packs: 6,
			PackageType: "CT", Condition: phyapi.ConditionGood, Evidence: evidence("CFS-WMS", "GRN-"+h.id, 14), OccurredAt: s.hours(14)}); err != nil {
			return step("receive "+h.id, err)
		}
		if _, err = s.phy.RecordCargoMeasurement(ctx, phyapi.RecordCargoMeasurement{CommandID: "weigh-" + h.id, TenantID: tenant, ActorID: "cfs-1", LotID: lot, ExpectedRevision: 1,
			GrossWeight: kernel.Quantity{Unit: "KGM", Amount: h.kilos}, Volume: kernel.Quantity{Unit: "CMQ", Amount: h.cm3}, Method: phyapi.MethodScale,
			Evidence: evidence("CFS-SCALE", "W-"+h.id, 14), OccurredAt: s.hours(14)}); err != nil {
			return step("weigh "+h.id, err)
		}
		if _, err = s.phy.CompleteReceipt(ctx, phyapi.CompleteReceipt{CommandID: "complete-" + h.id, TenantID: tenant, ActorID: "cfs-1", LotID: lot, ExpectedRevision: 2, OccurredAt: s.hours(14)}); err != nil {
			return step("complete the receipt of "+h.id, err)
		}
	}
	// the stuffing X-1: opened and started (revision 2), each house loaded, sealed
	x := int64(1)
	if _, err = s.phy.OpenLoadExecution(ctx, phyapi.OpenLoadExecution{CommandID: "stuff-X-1", TenantID: tenant, ActorID: "cfs-1", ExecutionID: "X-1", ConsolidationID: "C-1",
		Kind: phyapi.ExecutionStuffing, Facility: cfs, LoadUnitNo: 1, OccurredAt: s.hours(15)}); err != nil {
		return step("open X-1", err)
	}
	if _, err = s.phy.StartLoading(ctx, phyapi.StartLoading{CommandID: "start-X-1", TenantID: tenant, ActorID: "cfs-1", ExecutionID: "X-1", ExpectedRevision: x, OccurredAt: s.hours(15)}); err != nil {
		return step("start X-1", err)
	}
	x = 2
	for _, h := range houses {
		if _, err = s.phy.RecordCargoLoaded(ctx, phyapi.RecordCargoLoaded{CommandID: "load-X-1-" + h.id, TenantID: tenant, ActorID: "cfs-1", ExecutionID: "X-1", ExpectedRevision: x,
			LotID: "L-" + h.id, Packs: 6, Evidence: evidence("CFS-TALLY", "T-X-1"+h.id, 15), OccurredAt: s.hours(15)}); err != nil {
			return step("load "+h.id, err)
		}
		x++
	}
	if _, err = s.phy.CloseLoadExecution(ctx, phyapi.CloseLoadExecution{CommandID: "seal-X-1", TenantID: tenant, ActorID: "cfs-1", ExecutionID: "X-1", ExpectedRevision: x,
		Evidence: evidence(phyapi.EvidenceSeal, "SEAL-X-1", 15), OccurredAt: s.hours(15)}); err != nil {
		return step("seal X-1", err)
	}
	x++
	// the line's freight, previewed and submitted by fin-1
	cost, err := s.fin.RecordConsolCost(ctx, finapi.RecordConsolCost{CommandID: "fin-freight", TenantID: tenant, ActorID: "fin-1", PoolID: "P-1", ExpectedRevision: poolRevision,
		ChargeCode: "OCEAN_FREIGHT", Method: finapi.MethodChargeable, Cost: finapi.ChargeSide{PartyID: "line-1", Amount: demoCNY(300_000)}, Reason: "the line's freight for the container",
		OccurredAt: s.hours(16)})
	if err = step("the freight", err); err != nil {
		return err
	}
	minute := func(n int) time.Time { return s.hours(30).Add(time.Duration(n) * time.Minute) }
	pre, err := s.fin.PreviewConsolCostApportionment(ctx, finapi.PreviewConsolCostApportionment{CommandID: "fin-pre-1", TenantID: tenant, ActorID: "fin-1", PoolID: "P-1",
		ExpectedRevision: cost.NewRevision, OccurredAt: minute(0)})
	if err = step("the first preview", err); err != nil {
		return err
	}
	sub, err := s.fin.SubmitConsolCostApportionment(ctx, finapi.SubmitConsolCostApportionment{CommandID: "fin-sub-1", TenantID: tenant, ActorID: "fin-1", PoolID: "P-1",
		ExpectedRevision: pre.NewRevision, PreviewNo: pre.Version, OccurredAt: minute(1)})
	if err = step("the first submission", err); err != nil {
		return err
	}
	// the rework ops-1 asked for: X-1 opened for rework, H03's lot closed and H03 unloaded, the container sealed again
	if _, err = s.phy.OpenStuffingForRework(ctx, phyapi.OpenStuffingForRework{CommandID: "rework-1", TenantID: tenant, ActorID: "ops-1", ExecutionID: "X-1", ExpectedRevision: x,
		Reason: "rework rework-1", OccurredAt: s.hours(22)}); err != nil {
		return step("open X-1 for rework", err)
	}
	x++
	if _, err = s.phy.CloseCargoLot(ctx, phyapi.CloseCargoLot{CommandID: "close-lot-H03", TenantID: tenant, ActorID: "cfs-1", LotID: "L-H03", ExpectedRevision: 3, OccurredAt: s.hours(16)}); err != nil {
		return step("close the lot of H03", err)
	}
	if _, err = s.phy.UnloadHouseForRework(ctx, phyapi.UnloadHouseForRework{CommandID: "unload-H03", TenantID: tenant, ActorID: "wh-1", ExecutionID: "X-1", ExpectedRevision: x,
		ShipmentID: "SH-H03", Evidence: evidence("CFS-WMS", "UNSTUFF-H03", 23), OccurredAt: s.hours(23)}); err != nil {
		return step("unload H03", err)
	}
	x++
	if _, err = s.phy.CloseLoadExecution(ctx, phyapi.CloseLoadExecution{CommandID: "reseal-1", TenantID: tenant, ActorID: "cfs-2", ExecutionID: "X-1", ExpectedRevision: x,
		Evidence: evidence(phyapi.EvidenceSeal, "SEAL-reseal-1", 24), OccurredAt: s.hours(24)}); err != nil {
		return step("seal X-1 again", err)
	}
	// C-2, and H03 transferred to it by ops-9; C-2's pool P-2 with one cost, apportioned to H03
	if err = step("create C-2", consol("C-2", 8, "consol-2")); err != nil {
		return err
	}
	if _, err = s.phy.PlanLoadUnit(ctx, phyapi.PlanLoadUnit{CommandID: "plan-c2", TenantID: tenant, ActorID: "ops-1", ConsolidationID: "C-2", ExpectedRevision: 1, Kind: phyapi.LoadUnitContainer,
		TypeCode: "22G1", OccurredAt: s.hours(8)}); err != nil {
		return step("plan the unit of C-2", err)
	}
	if _, err = s.phy.TransferHouse(ctx, phyapi.TransferHouse{CommandID: "transfer-h03", TenantID: tenant, ActorID: "ops-9", ConsolidationID: "C-1", ToConsolidationID: "C-2", ShipmentID: "SH-H03",
		ExpectedMembershipRevision: 1, Reason: phyapi.ReasonWrongConsol, OccurredAt: s.hours(26)}); err != nil {
		return step("transfer H03", err)
	}
	p2, err := s.fin.OpenConsolCostPool(ctx, finapi.OpenConsolCostPool{CommandID: "fin-pool2", TenantID: tenant, ActorID: "fin-1", PoolID: "P-2", ConsolidationID: "C-2", OccurredAt: s.hours(33)})
	if err = step("open P-2", err); err != nil {
		return err
	}
	c2, err := s.fin.RecordConsolCost(ctx, finapi.RecordConsolCost{CommandID: "fin-cost2", TenantID: tenant, ActorID: "fin-1", PoolID: "P-2", ExpectedRevision: p2.NewRevision, ChargeCode: "OCEAN_FREIGHT",
		Method: finapi.MethodChargeable, Cost: finapi.ChargeSide{PartyID: "line-1", Amount: demoCNY(10_001)}, Reason: "the line's charge for the second container", OccurredAt: s.hours(33)})
	if err = step("the cost of P-2", err); err != nil {
		return err
	}
	pre2, err := s.fin.PreviewConsolCostApportionment(ctx, finapi.PreviewConsolCostApportionment{CommandID: "fin-apportion-p2", TenantID: tenant, ActorID: "fin-1", PoolID: "P-2",
		ExpectedRevision: c2.NewRevision, LineNos: []int{1}, OccurredAt: s.hours(34)})
	if err = step("preview P-2", err); err != nil {
		return err
	}
	if _, err = s.fin.SubmitConsolCostApportionment(ctx, finapi.SubmitConsolCostApportionment{CommandID: "fin-apportion-p2-submit", TenantID: tenant, ActorID: "fin-1", PoolID: "P-2",
		ExpectedRevision: pre2.NewRevision, PreviewNo: pre2.Version, OccurredAt: s.hours(34)}); err != nil {
		return step("submit P-2", err)
	}
	// finance attributes H03's former share in C-1: ops-2 previews, fin-1 (who asked for no rework and ended no house)
	// submits
	attr, err := s.fin.PreviewConsolCostApportionment(ctx, finapi.PreviewConsolCostApportionment{CommandID: "fin-pre-attr", TenantID: tenant, ActorID: "ops-2", PoolID: "P-1",
		ExpectedRevision: sub.NewRevision, LineNos: []int{1}, Attributions: []finapi.AttributionStatement{{LineNo: 1, ShipmentID: "SH-H03", Loss: 10_000, Exception: 20_000,
			Party: finapi.PartyPending, Voucher: "CLAIM-1"}}, OccurredAt: minute(2)})
	if err = step("the preview with the attribution", err); err != nil {
		return err
	}
	if _, err = s.fin.SubmitConsolCostApportionment(ctx, finapi.SubmitConsolCostApportionment{CommandID: "fin-sub-fin-1", TenantID: tenant, ActorID: "fin-1", PoolID: "P-1",
		ExpectedRevision: attr.NewRevision, PreviewNo: attr.Version, OccurredAt: minute(3)}); err != nil {
		return step("the submission with the attribution", err)
	}
	s.logf("seeded box A: C-1 with H01, H02 and H03, H03 transferred to C-2, its former share attributed")
	return nil
}

// report lists the legal entity's job profits as FIN answers them and a digest of the list, which a second run repeats.
func (s *demoSeeder) report() error {
	page, err := s.fin.ListJobProfits(s.ctx, finapi.ListJobProfits{TenantID: s.tenant, ActorID: "demo", LegalEntityID: "LE", Limit: 500})
	if err != nil {
		return err
	}
	var text strings.Builder
	var losses, zero, gains int
	for _, r := range page.Rows {
		fmt.Fprintf(&text, "%s %s %s revenue=%d cost=%d profit=%d basis=%s\n", r.JobID, r.ShipmentID, r.CustomerPartyID, r.Revenue.Minor, r.Cost.Minor, r.Profit.Minor, r.ProfitBasis)
		switch {
		case r.Profit.Minor < 0:
			losses++
		case r.Profit.Minor == 0:
			zero++
		default:
			gains++
		}
	}
	sum := sha256.Sum256([]byte(text.String()))
	s.logf("tenant %s: %d jobs in legal entity LE: %d at a profit, %d at a loss, %d at zero", s.tenant, len(page.Rows), gains, losses, zero)
	s.logf("digest of the figures (job, shipment, customer, revenue, cost, profit, basis): %s", hex.EncodeToString(sum[:]))
	return nil
}
