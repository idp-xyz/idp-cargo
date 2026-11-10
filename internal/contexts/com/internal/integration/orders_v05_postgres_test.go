//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/com/api"
	rateacl "go.idp.xyz/idp-cargo/internal/contexts/com/internal/adapters/rate"
	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
	rateapi "go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

func (f *fixture) amendLine(order, command string, revision int64, line int, l api.OrderLine, reason string, at time.Time) api.AmendOrderLine {
	return api.AmendOrderLine{CommandID: command, TenantID: f.tenant, ActorID: "cs-1", OrderID: order, ExpectedRevision: revision, LineNo: line,
		Description: l.Description, Packs: l.Packs, PackageType: l.PackageType, GrossWeight: l.GrossWeight, Volume: l.Volume, Reason: reason, OccurredAt: at}
}

func (f *fixture) policy(command, entity string, expected int64, max, grace int) api.SetOrderPolicy {
	return api.SetOrderPolicy{CommandID: command, TenantID: f.tenant, ActorID: "commercial-lead", LegalEntityID: entity,
		ExpectedVersion: expected, MaxOrdersPerQuote: max, GraceDays: grace}
}

// TestOrderCorrectionOnPostgres corrects a line above what its obligations commit, adds a line, allocates
// both and reads the order back from PostgreSQL after every step (C10).
func TestOrderCorrectionOnPostgres(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.module.RecordCustomerOrder(ctx, f.record("O-1", "Q-1", twoLines...)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.AllocateOrderToObligation(ctx, f.allocate("O-1", "allocate-1", 1, 1, "OB-1", cargo{10, kg(500), dm3(12_000)})); err != nil {
		t.Fatal(err)
	}
	heavier := pallets
	heavier.GrossWeight = kg(520)
	later := f.at.Add(2 * time.Hour)
	r, err := f.module.AmendOrderLine(ctx, f.amendLine("O-1", "amend-line-1", 2, 1, heavier, "customer corrected the weight", later))
	if err != nil || r.NewRevision != 3 {
		t.Fatalf("amend line 1: %+v, %v", r, err)
	}
	twoMore := heavier
	twoMore.Packs, twoMore.GrossWeight, twoMore.Volume = 12, kg(620), dm3(14_000)
	if _, err = f.module.AmendOrderLine(ctx, f.amendLine("O-1", "amend-line-2", 3, 1, twoMore, "two more pallets", later)); err != nil {
		t.Fatalf("amend line 1 again: %v", err)
	}
	manuals := api.OrderLine{Description: "Cartons of manuals", Packs: 2, PackageType: "CT", GrossWeight: kg(15)}
	if _, err = f.module.AddOrderLine(ctx, api.AddOrderLine{CommandID: "add-line-1", TenantID: f.tenant, ActorID: "cs-1", OrderID: "O-1",
		ExpectedRevision: 4, Line: manuals, Reason: "customer added two cartons", OccurredAt: later}); err != nil {
		t.Fatalf("add line 3: %v", err)
	}
	// The order is read back from PostgreSQL for each command: the new pallets and the added line allocate.
	if _, err = f.module.AllocateOrderToObligation(ctx, f.allocate("O-1", "allocate-2", 5, 1, "OB-2", cargo{2, kg(120), dm3(2_000)})); err != nil {
		t.Fatalf("allocate the corrected line's remainder: %v", err)
	}
	if _, err = f.module.AllocateOrderToObligation(ctx, f.allocate("O-1", "allocate-3", 6, 3, "OB-3", cargo{2, kg(15), kernel.Quantity{}})); err != nil {
		t.Fatalf("allocate the added line: %v", err)
	}
	if s := f.snapshotV2(t, "OB-3", 1); s.OrderLineNo != 3 || s.Description != "Cartons of manuals" || s.PackageType != "CT" {
		t.Fatalf("snapshot of the added line's obligation %+v", s)
	}
	lighter := twoMore
	lighter.GrossWeight = kg(619)
	for name, cmd := range map[string]api.AmendOrderLine{
		"below what the obligations commit":  f.amendLine("O-1", "amend-below", 7, 1, lighter, "lighter", later),
		"another package type":               f.amendLine("O-1", "amend-type", 7, 1, api.OrderLine{Description: "Crates", Packs: 12, PackageType: "CR", GrossWeight: kg(620), Volume: dm3(14_000)}, "crates", later),
		"no reason":                          f.amendLine("O-1", "amend-no-reason", 7, 1, twoMore, " ", later),
		"backdated beyond the record window": f.amendLine("O-1", "amend-old", 7, 2, cartons, "old", time.Now().UTC().Add(-8*24*time.Hour)),
	} {
		if _, err = f.module.AmendOrderLine(ctx, cmd); !errors.Is(err, api.ErrRejected) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err = f.module.AmendOrderLine(ctx, f.amendLine("O-1", "amend-stale", 6, 2, cartons, "stale", later)); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("a stale revision: err = %v", err)
	}
	want := []string{api.CustomerOrderRecorded, api.FulfillmentObligationAllocated, api.CustomerOrderLineAmended, api.CustomerOrderLineAmended,
		api.CustomerOrderLineAdded, api.FulfillmentObligationAllocated, api.FulfillmentObligationAllocated}
	if got := f.outboxTypes(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("outbox events %v, want %v", got, want)
	}
	var amendments, added int
	var status string
	var entered *time.Time
	if err = f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT (SELECT count(*) FROM cargo_com.order_line_amendment WHERE tenant_id=$1),
            (SELECT count(*) FROM cargo_com.order_line WHERE tenant_id=$1 AND added_at IS NOT NULL),
            o.status, o.entered_at FROM cargo_com.customer_order o WHERE o.tenant_id=$1 AND o.order_id='O-1'`, f.tenant).Scan(&amendments, &added, &status, &entered)
	}); err != nil {
		t.Fatal(err)
	}
	if amendments != 2 || added != 1 || status != "PARTIALLY_ALLOCATED" || entered == nil { // line 2 is not allocated
		t.Fatalf("stored: %d amendments, %d added lines, status %s, entered %v", amendments, added, status, entered)
	}
}

// TestLineChangesAreGuardedInSQL writes line changes as the application role past COM's model.
func TestLineChangesAreGuardedInSQL(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.module.RecordCustomerOrder(ctx, f.record("O-1", "Q-1", twoLines...)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.AllocateOrderToObligation(ctx, f.allocate("O-1", "allocate-1", 1, 1, "OB-1", cargo{4, kg(200), dm3(5_000)})); err != nil {
		t.Fatal(err)
	}
	amend := func(version, changeNo int, pkg string, packs, weight int64, unit string) string {
		return fmt.Sprintf(`INSERT INTO cargo_com.order_line_amendment (tenant_id,order_id,line_no,line_version,description,package_type,packs,
            gross_weight_unit,gross_weight,volume_unit,volume,amended_by,amended_at,entered_at,reason,change_no)
            VALUES ($1,'O-1',1,%d,'Pallets','%s',%d,'%s',%d,'DMQ',12000,'rogue',now(),now(),'rogue',%d)`, version, pkg, packs, unit, weight, changeNo)
	}
	addLine := func(lineNo, changeNo int) string {
		return fmt.Sprintf(`INSERT INTO cargo_com.order_line (tenant_id,order_id,line_no,description,package_type,packs,gross_weight_unit,
            gross_weight,added_by,added_at,added_entered_at,added_reason,change_no)
            VALUES ($1,'O-1',%d,'Cartons','CT',2,'KGM',15,'rogue',now(),now(),'rogue',%d)`, lineNo, changeNo)
	}
	for _, tc := range []struct {
		sql, state, why string
	}{
		{amend(3, 1, "PX", 10, 500, "KGM"), "23514", "an amendment is the line's next version"},
		{amend(2, 2, "PX", 10, 500, "KGM"), "23514", "an amendment is the order's next change"},
		{amend(2, 1, "PX", 3, 500, "KGM"), "23514", "four pallets are committed"},
		{amend(2, 1, "PX", 10, 199, "KGM"), "23514", "200 kg are committed"},
		{amend(2, 1, "CR", 10, 500, "KGM"), "23514", "the package type stays while obligations are active"},
		{amend(2, 1, "PX", 10, 500, "LBR"), "23514", "the unit stays while obligations are active"},
		{addLine(4, 1), "23514", "an added line is numbered after the others"},
		{`INSERT INTO cargo_com.order_line (tenant_id,order_id,line_no,description,package_type,packs,gross_weight_unit,gross_weight)
            VALUES ($1,'O-1',3,'Cartons','CT',2,'KGM',15)`, "23514", "a line without its change is recorded with the order only"},
		{`UPDATE cargo_com.order_line_amendment SET packs=1 WHERE tenant_id=$1`, "42501", "the application role cannot change amendments"},
	} {
		if err := f.exec(t, tc.sql); sqlState(err) != tc.state {
			t.Errorf("%s: err = %v, want SQLSTATE %s", tc.why, err, tc.state)
		}
	}
	if err := f.exec(t, amend(2, 1, "PX", 10, 520, "KGM")); err != nil {
		t.Fatalf("a correction that keeps the commitment: %v", err)
	}
	if err := f.exec(t, addLine(3, 2)); err != nil {
		t.Fatalf("an added line: %v", err)
	}
	if _, err := f.module.CancelCustomerOrder(ctx, f.cancel("O-1", "cancel-1", f.revision(t, "O-1"))); !errors.Is(err, nil) {
		t.Fatalf("COM reads the rows written in SQL and cancels: %v", err)
	}
	if err := f.exec(t, amend(3, 3, "PX", 10, 530, "KGM")); sqlState(err) != "23514" {
		t.Fatalf("a cancelled order is not corrected: err = %v", err)
	}
}

// TestOrderPolicyOnPostgres records policies and orders under them (C1, C2).
func TestOrderPolicyOnPostgres(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if p, err := f.module.GetOrderPolicy(ctx, api.GetOrderPolicy{TenantID: f.tenant, LegalEntityID: "LE"}); err != nil ||
		p != (api.OrderPolicy{Scope: "BUILT_IN", MaxOrdersPerQuote: 1}) {
		t.Fatalf("built-in policy: %+v, %v", p, err)
	}
	if _, err := f.module.RecordCustomerOrder(ctx, f.record("O-1", "Q-1", pallets)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.module.RecordCustomerOrder(ctx, f.record("O-2", "Q-1", pallets)); !errors.Is(err, api.ErrConflict) || !errors.Is(err, com.ErrQuoteFullyOrdered) {
		t.Fatalf("a second order under the built-in policy: err = %v", err)
	}
	if r, err := f.module.SetOrderPolicy(ctx, f.policy("policy-tenant", "", 0, 5, 0)); err != nil || r.Version != 1 || r.LegalEntityID != "" {
		t.Fatalf("tenant default: %+v, %v", r, err)
	}
	r, err := f.module.SetOrderPolicy(ctx, f.policy("policy-LE", "LE", 0, 2, 1))
	if err != nil || r.Version != 1 || r.LegalEntityID != "LE" {
		t.Fatalf("legal entity policy: %+v, %v", r, err)
	}
	if again, err := f.module.SetOrderPolicy(ctx, f.policy("policy-LE", "LE", 0, 2, 1)); err != nil || again != r {
		t.Fatalf("replay: %+v, %v", again, err)
	}
	if _, err = f.module.SetOrderPolicy(ctx, f.policy("policy-LE-stale", "LE", 0, 3, 1)); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("a stale version: err = %v", err)
	}
	if _, err = f.module.SetOrderPolicy(ctx, f.policy("policy-LE-31", "LE", 1, 3, 31)); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("31 days of grace: err = %v", err)
	}
	intruder := f.policy("policy-intruder", "LE", 1, 9, 0)
	intruder.ActorID = "intruder"
	if _, err = f.module.SetOrderPolicy(ctx, intruder); !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("intruder: err = %v", err)
	}
	if p, err := f.module.GetOrderPolicy(ctx, api.GetOrderPolicy{TenantID: f.tenant, LegalEntityID: "LE"}); err != nil ||
		p != (api.OrderPolicy{Scope: "LEGAL_ENTITY", LegalEntityID: "LE", Version: 1, MaxOrdersPerQuote: 2, GraceDays: 1}) {
		t.Fatalf("legal entity policy in force: %+v, %v", p, err)
	}
	if p, err := f.module.GetOrderPolicy(ctx, api.GetOrderPolicy{TenantID: f.tenant, LegalEntityID: "LE-2"}); err != nil || p.Scope != "TENANT" || p.MaxOrdersPerQuote != 5 {
		t.Fatalf("tenant default in force: %+v, %v", p, err)
	}
	second := f.record("O-2", "Q-1", pallets)
	second.CommandID = "record-O-2-again"
	if _, err = f.module.RecordCustomerOrder(ctx, second); err != nil {
		t.Fatalf("a second order under a policy of two: %v", err)
	}
	if _, err = f.module.RecordCustomerOrder(ctx, f.record("O-3", "Q-1", pallets)); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("a third order: err = %v", err)
	}
	if _, err = f.module.CancelCustomerOrder(ctx, f.cancel("O-1", "cancel-1", 1)); err != nil {
		t.Fatal(err)
	}
	third := f.record("O-3", "Q-1", pallets)
	third.CommandID = "record-O-3-again"
	if _, err = f.module.RecordCustomerOrder(ctx, third); err != nil {
		t.Fatalf("a cancelled order no longer counts: %v", err)
	}
	// Q-ENDED's validity ended an hour after f.at; an order three hours later needs the day of grace (C2).
	f.rate.edit["Q-ENDED"] = func(s *rateapi.QuoteSnapshotV2) { s.Validity.Until = f.at.Add(time.Hour) }
	late := f.record("O-4", "Q-ENDED", pallets)
	late.OccurredAt = f.at.Add(3 * time.Hour)
	if _, err = f.module.RecordCustomerOrder(ctx, late); err != nil {
		t.Fatalf("within the grace: %v", err)
	}
	f.rate.edit["Q-ENDED-2"] = f.rate.edit["Q-ENDED"]
	tooLate := f.record("O-5", "Q-ENDED-2", pallets)
	tooLate.OccurredAt = f.at.Add(25 * time.Hour)
	if _, err = f.module.RecordCustomerOrder(ctx, tooLate); !errors.Is(err, api.ErrRejected) || !errors.Is(err, com.ErrOutsideQuoteValidity) {
		t.Fatalf("after the grace: err = %v", err)
	}
	var scope string
	var version int64
	var grace int
	if err = f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT policy_scope,policy_version,grace_days FROM cargo_com.customer_order WHERE tenant_id=$1 AND order_id='O-4'`,
			f.tenant).Scan(&scope, &version, &grace)
	}); err != nil || scope != "LEGAL_ENTITY" || version != 1 || grace != 1 {
		t.Fatalf("O-4 froze %s version %d with %d days of grace, %v", scope, version, grace, err)
	}
	for _, tc := range []struct {
		sql, state, why string
	}{
		{`INSERT INTO cargo_com.order_policy (tenant_id,legal_entity_id,version,max_orders_per_quote,grace_days,set_by,set_at)
            VALUES ($1,'LE',3,1,0,'rogue',now())`, "23514", "a policy version follows the last one"},
		{`UPDATE cargo_com.order_policy SET grace_days=30 WHERE tenant_id=$1`, "42501", "policy versions are never changed"},
		{`INSERT INTO cargo_com.customer_order (tenant_id,order_id,legal_entity_id,customer_party_id,customer_party_revision,customer_legal_name,
            customer_country_code,customer_role_valid_from,customer_role_evidence_ref,quote_id,quote_revision,quote_customer_revision,quote_currency,
            quote_total_minor,quote_valid_from,quote_valid_until,quote_accepted_at,quote_acceptance_evidence_ref,accepted_terms_sha256,recorded_by,
            recorded_at,status,policy_scope,policy_version,max_orders_per_quote,grace_days)
          SELECT tenant_id,'O-ROGUE',legal_entity_id,customer_party_id,customer_party_revision,customer_legal_name,customer_country_code,
            customer_role_valid_from,customer_role_evidence_ref,'Q-ROGUE',quote_revision,quote_customer_revision,quote_currency,quote_total_minor,
            quote_valid_from,quote_valid_until,quote_accepted_at,quote_acceptance_evidence_ref,accepted_terms_sha256,'rogue',recorded_at,'RECORDED',
            'LEGAL_ENTITY',1,9,1 FROM cargo_com.customer_order WHERE tenant_id=$1 AND order_id='O-2'`, "23514", "an order follows the policy in force"},
	} {
		if err := f.exec(t, tc.sql); sqlState(err) != tc.state {
			t.Errorf("%s: err = %v, want SQLSTATE %s", tc.why, err, tc.state)
		}
	}
}

// TestConcurrentOrdersOnOneQuoteRespectThePolicy races eight orders on one quote under a policy of three.
func TestConcurrentOrdersOnOneQuoteRespectThePolicy(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.module.SetOrderPolicy(ctx, f.policy("policy-LE", "LE", 0, 3, 0)); err != nil {
		t.Fatal(err)
	}
	const workers = 8
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = f.module.RecordCustomerOrder(ctx, f.record(fmt.Sprintf("O-%d", i), "Q-1", pallets))
		}(i)
	}
	close(start)
	wg.Wait()
	committed, full := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			committed++
		case errors.Is(err, com.ErrQuoteFullyOrdered):
			full++
		}
	}
	if committed != 3 || full != workers-3 {
		t.Fatalf("%d committed, %d refused as fully ordered; errors %v", committed, full, errs)
	}
}

// TestTermsAndOrderingRoleOnPostgres records orders that complete the quote's terms and an order placed by
// an overseas agent the quote went to (C8, R7).
func TestTermsAndOrderingRoleOnPostgres(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.rate.edit["Q-FOB"] = func(s *rateapi.QuoteSnapshotV2) {
		s.Terms = rateapi.CommercialTerms{IncotermsRule: "FOB", IncotermsVersion: "2020", NamedPlace: "Shanghai port"}
	}
	complete := f.record("O-1", "Q-FOB", pallets)
	complete.Terms = api.CommercialTerms{ServiceLevel: "EXPRESS"}
	if _, err := f.module.RecordCustomerOrder(ctx, complete); err != nil {
		t.Fatal(err)
	}
	f.rate.edit["Q-AGENT"] = func(s *rateapi.QuoteSnapshotV2) { s.RecipientRole = "OVERSEAS_AGENT" }
	if _, err := f.module.RecordCustomerOrder(ctx, f.record("O-2", "Q-AGENT", pallets)); err != nil {
		t.Fatalf("an overseas agent orders on its quote: %v", err)
	}
	f.rate.edit["Q-OTHER"] = f.rate.edit["Q-FOB"]
	contradict := f.record("O-3", "Q-OTHER", pallets)
	contradict.Terms = api.CommercialTerms{IncotermsRule: "CIF", IncotermsVersion: "2020", NamedPlace: "Hamburg port"}
	unlisted := f.record("O-4", "Q-4", pallets)
	unlisted.Terms = api.CommercialTerms{ServiceLevel: "PRIORITY"}
	backdated := f.record("O-5", "Q-5", pallets)
	backdated.OccurredAt = time.Now().UTC().Add(-8 * 24 * time.Hour)
	for name, cmd := range map[string]api.RecordCustomerOrder{
		"other Incoterms than the quote's": contradict,
		"an unlisted service level":        unlisted,
		"beyond the record window":         backdated,
	} {
		if _, err := f.module.RecordCustomerOrder(ctx, cmd); !errors.Is(err, api.ErrRejected) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	type stored struct {
		role, recipient, contract, hash, rule, place, ruleSource, level, levelSource string
	}
	read := func(order string) (s stored) {
		var rule, place, ruleSource, level, levelSource *string
		if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			return e.QueryRow(ctx, `SELECT customer_role_code,quote_recipient_role,accepted_terms_contract,accepted_terms_sha256,
                incoterms_rule,named_place,incoterms_source,service_level,service_level_source FROM cargo_com.customer_order
                WHERE tenant_id=$1 AND order_id=$2`, f.tenant, order).Scan(&s.role, &s.recipient, &s.contract, &s.hash,
				&rule, &place, &ruleSource, &level, &levelSource)
		}); err != nil {
			t.Fatal(err)
		}
		for dst, src := range map[*string]*string{&s.rule: rule, &s.place: place, &s.ruleSource: ruleSource, &s.level: level, &s.levelSource: levelSource} {
			if src != nil {
				*dst = *src
			}
		}
		return s
	}
	fob := snapshot(f.tenant, "Q-FOB", f.at)
	f.rate.edit["Q-FOB"](&fob)
	hash, _ := rateacl.TermsHashV2(fob)
	if got, want := read("O-1"), (stored{"CUSTOMER", "CUSTOMER", "QuoteSnapshot.v2", hash, "FOB", "Shanghai port", "QUOTE", "EXPRESS", "ORDER"}); got != want {
		t.Fatalf("O-1 stored %+v, want %+v", got, want)
	}
	if got := read("O-2"); got.role != "OVERSEAS_AGENT" || got.recipient != "OVERSEAS_AGENT" {
		t.Fatalf("O-2 stored %+v", got)
	}
}
