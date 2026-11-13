//go:build integration

package integration

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// agreementCard opens acme's CUSTOMER card id with a DRAFT version valid over [from, until) holding the entries.
func agreementCard(c cards, id string, from, until time.Time, entries ...api.RateEntry) {
	c.t.Helper()
	c.must(c.module.CreateRateCard(c.ctx, c.create(id, api.SideSell, api.ScopeCustomer, "acme", "sales-1")))
	c.must(c.module.DraftRateCardVersion(c.ctx, c.draft(id, from, until, 0, "sales-1")))
	c.must(c.module.SetRateCardEntries(c.ctx, c.set(id, 1, "sales-1", entries...)))
}

func agreement(c cards, id, card string, from, until time.Time) api.CreateQuote {
	return api.CreateQuote{CommandID: "create-" + id, TenantID: c.tenant, ActorID: "sales-1", QuoteID: id, LegalEntityID: "LE", CustomerPartyID: "acme",
		Currency: "CNY", Kind: api.QuoteAgreement, Agreement: &api.AgreementRef{RateCardID: card, Version: 1},
		Scope: &api.QuoteScope{Mode: api.ModeAir, ContainerMode: api.ContainerLoose}, Validity: kernel.ValidityInterval{From: from, Until: until},
		OccurredAt: c.h(5)}
}

func lane(destination string, lines ...api.RateLine) api.RateEntry {
	e := air(lines...)
	e.Destination = destination
	return e
}

// AGREEMENT quotes on PostgreSQL (RATE V0.5 part 3b): an agreement of two lanes checked against the buy rates at 100 kg
// (one lane without a buy rate needs approval), its draft fixed while quoted, its acceptance publishing the customer
// card that then rates the customer's shipments, its snapshots, and what RATE and the database refuse.
func TestAgreementQuotesOnPostgres(t *testing.T) {
	c := newCards(t)
	publishedTariff(c)
	// coloader-a buys CNPVG-DEFRA at CNY 12.00/kg from 100 kg; nobody sells a buy rate to USLAX
	c.must(c.module.CreateRateCard(c.ctx, c.create("S-A", api.SideBuy, api.ScopeSupplier, "coloader-a", "buyer-1")))
	c.must(c.module.DraftRateCardVersion(c.ctx, c.draft("S-A", c.h(0), time.Time{}, 0, "buyer-1")))
	c.must(c.module.SetRateCardEntries(c.ctx, c.set("S-A", 1, "buyer-1", air(api.RateLine{ChargeCode: "AIR_FREIGHT", Currency: "CNY",
		Calculator: api.CalcPerUnit, Unit: api.UnitKgChargeable, Rate: 2200, Breaks: []api.RateBreak{{From: 100_000, Rate: 1200}}}))))
	c.must(c.module.PublishRateCardVersion(c.ctx, c.publish("S-A", 1, "buyer-1")))

	from, until := c.h(5), c.h(5+90*24)
	agreementCard(c, "C-ACME", from, until, lane("DEFRA", freight(3000)), lane("USLAX", freight(4000)))
	c.must(c.module.CreateQuote(c.ctx, agreement(c, "Q-AG", "C-ACME", from, until)))
	// 100 kg to DEFRA: sell 20.00/kg = 2,000.00 against 12.00/kg = 1,200.00 (40%); USLAX has no buy rate
	if got := c.value(`SELECT q.status||' '||q.kind||' '||string_agg(k.entry_no||':'||k.sell_minor||':'||coalesce(k.cost_minor::text,'-'),' ' ORDER BY k.entry_no)
      FROM cargo_rate.quote q JOIN cargo_rate.quote_agreement_check k ON k.tenant_id=q.tenant_id AND k.quote_id=q.quote_id
      WHERE q.tenant_id=$1 AND q.quote_id='Q-AG' GROUP BY q.status,q.kind`); got != "AWAITING_APPROVAL AGREEMENT 1:200000:120000 2:300000:-" {
		t.Fatalf("agreement checks: %s", got)
	}

	// the draft is fixed while quoted, and quoted by one agreement
	_, err := c.module.SetRateCardEntries(c.ctx, c.set("C-ACME", 1, "sales-1", lane("DEFRA", freight(2000))))
	c.rejected("entries of a quoted draft", err, api.ErrRejected, "RATE_VERSION_QUOTED")
	again := agreement(c, "Q-AG-2", "C-ACME", from, until)
	_, err = c.module.CreateQuote(c.ctx, again)
	c.rejected("a second agreement on the version", err, api.ErrRejected, "quoted by another agreement")

	// approved and accepted: the version is published by the acceptance
	c.must(c.module.ApproveQuotePrice(c.ctx, api.ApproveQuotePrice{CommandID: "approve-Q-AG", TenantID: c.tenant, ActorID: "manager-1", QuoteID: "Q-AG",
		ExpectedRevision: 1, ReasonRef: "USLAX bought spot", OccurredAt: c.h(6)}))
	c.must(c.module.AcceptQuote(c.ctx, api.AcceptQuote{CommandID: "accept-Q-AG", TenantID: c.tenant, ActorID: "sales-2", QuoteID: "Q-AG", ExpectedRevision: 2,
		EvidenceKind: api.EvidenceSignedDocument, EvidenceRef: "agreement signed", OccurredAt: c.h(7)}))
	if v := c.card("C-ACME").Versions[0]; v.Status != "PUBLISHED" || v.PublishedBy != "sales-2" || !v.ValidFrom.Equal(from) || !v.ValidUntil.Equal(until) {
		t.Fatalf("published by the acceptance: %+v", v)
	}
	v3, err := c.module.GetQuoteSnapshotV3(c.ctx, api.GetQuoteSnapshot{TenantID: c.tenant, QuoteID: "Q-AG"})
	switch {
	case err != nil:
		t.Fatal(err)
	case v3.Kind != api.QuoteAgreement || v3.Agreement == nil || *v3.Agreement != (api.AgreementRef{RateCardID: "C-ACME", Version: 1}),
		v3.Total != (kernel.Money{}) || len(v3.Lines) != 0 || v3.Scope == nil || v3.Scope.Mode != api.ModeAir || v3.Scope.Origin != "",
		!slices.Equal(v3.PriceApprovalReasons, []string{"AGREEMENT_BELOW_FLOOR"}) || v3.AcceptanceEvidenceKind != api.EvidenceSignedDocument:
		t.Fatalf("snapshot v3: %+v", v3)
	}
	for name, read := range map[string]func() error{
		"QuoteSnapshot v1": func() error {
			_, err := c.module.GetQuoteSnapshot(c.ctx, api.GetQuoteSnapshot{TenantID: c.tenant, QuoteID: "Q-AG"})
			return err
		},
		"QuoteSnapshot v2": func() error {
			_, err := c.module.GetQuoteSnapshotV2(c.ctx, api.GetQuoteSnapshot{TenantID: c.tenant, QuoteID: "Q-AG"})
			return err
		},
		"a QUOTE_ACCEPTED basis": func() error {
			_, err := c.module.GetPricingBasis(c.ctx, api.GetPricingBasis{TenantID: c.tenant, QuoteID: "Q-AG"})
			return err
		},
	} {
		c.rejected(name, read(), api.ErrRejected, "is an AGREEMENT")
	}

	// the customer's shipments to DEFRA are now rated with the agreement (OD-1 A)
	c.must(c.module.EvaluateRates(c.ctx, api.EvaluateRates{CommandID: "evaluate-after", TenantID: c.tenant, ActorID: "sales-1", SnapshotID: "RS-AFTER",
		Purpose: api.PurposeQuoteLine, LegalEntityID: "LE", Sides: []string{api.SideSell}, CustomerPartyID: "acme", Mode: api.ModeAir,
		ContainerMode: api.ContainerLoose, Origin: "CNPVG", Destination: "DEFRA", ServiceLevel: "STANDARD", Commodity: api.CommodityGeneral,
		Cargo: api.Cargo{Pieces: 3, GrossGrams: 150_000}, OccurredAt: c.h(8)}))
	if got := c.value(`SELECT rate_card_id||' '||amount_minor FROM cargo_rate.rate_snapshot_line WHERE tenant_id=$1 AND snapshot_id='RS-AFTER'
      AND charge_code='AIR_FREIGHT'`); got != "C-ACME 300000" {
		t.Errorf("rated with the agreement: %s", got)
	}

	// an agreement whose lanes all keep the floor needs no approval: the DEFRA lane once Q-AG's agreement ends
	later, end := until, until.Add(30*24*time.Hour)
	agreementCard(c, "C-ACME-DE", later, end, lane("DEFRA", freight(3000)))
	c.must(c.module.CreateQuote(c.ctx, agreement(c, "Q-AG-DE", "C-ACME-DE", later, end)))
	if got := c.value(`SELECT status FROM cargo_rate.quote WHERE tenant_id=$1 AND quote_id='Q-AG-DE'`); got != "PRICED" {
		t.Errorf("an agreement above the floor: %s", got)
	}
	// a lane the customer's agreements rate is not quoted again on another card over the same time, accepted or not
	agreementCard(c, "C-ACME-2", from, until, lane("DEFRA", freight(2500)))
	_, err = c.module.CreateQuote(c.ctx, agreement(c, "Q-AG-3", "C-ACME-2", from, until))
	c.rejected("the lane of an accepted agreement", err, api.ErrRejected, "RATE_LANE_TAKEN")
	agreementCard(c, "C-ACME-3", later, end, lane("DEFRA", freight(2500)))
	_, err = c.module.CreateQuote(c.ctx, agreement(c, "Q-AG-4", "C-ACME-3", later, end))
	c.rejected("the lane of a quoted agreement", err, api.ErrRejected, "RATE_LANE_TAKEN")
	express := lane("DEFRA", freight(2500))
	express.ServiceLevel = "EXPRESS"
	agreementCard(c, "C-ACME-X", from, until, express)
	c.must(c.module.CreateQuote(c.ctx, agreement(c, "Q-AG-X", "C-ACME-X", from, until)))
	// a version of the card that rates during the agreement is found when it is priced, not when it is accepted
	c.must(c.module.DraftRateCardVersion(c.ctx, c.draft("C-ACME", from.Add(24*time.Hour), until, 0, "sales-1")))
	c.must(c.module.SetRateCardEntries(c.ctx, c.set("C-ACME", 2, "sales-1", lane("AEDXB", freight(3000)))))
	second := agreement(c, "Q-AG-V2", "C-ACME", from.Add(24*time.Hour), until)
	second.Agreement.Version = 2
	_, err = c.module.CreateQuote(c.ctx, second)
	c.rejected("over a published version of the card", err, api.ErrRejected, "rates during the agreement")
	// naming a draft needs the right to maintain sell rates
	agreementCard(c, "C-ACME-J", from, until, lane("AEDXB", freight(3000)))
	junior := agreement(c, "Q-AG-J", "C-ACME-J", from, until)
	junior.ActorID = "sales-junior"
	_, err = c.module.CreateQuote(c.ctx, junior)
	c.rejected("without the right to maintain sell rates", err, api.ErrUnauthorized, "forbidden")
	// a code deactivated after the agreement was priced does not stop its acceptance: the customer accepted those rates
	handling := lane("CNSHA", api.RateLine{ChargeCode: "HANDLING", Currency: "CNY", Calculator: api.CalcFlat, Rate: 5_000})
	agreementCard(c, "C-HANDLING", from, until, handling)
	c.must(c.module.CreateQuote(c.ctx, agreement(c, "Q-AG-H", "C-HANDLING", from, until)))
	c.must(c.module.DeactivateChargeCode(c.ctx, api.DeactivateChargeCode{CommandID: "deactivate-handling", TenantID: c.tenant, ActorID: "md-1",
		Code: "HANDLING", ExpectedVersion: 0, OccurredAt: c.h(6)}))
	c.must(c.module.ApproveQuotePrice(c.ctx, api.ApproveQuotePrice{CommandID: "approve-Q-AG-H", TenantID: c.tenant, ActorID: "manager-1", QuoteID: "Q-AG-H",
		ExpectedRevision: 1, ReasonRef: "handling only", OccurredAt: c.h(7)}))
	c.must(c.module.AcceptQuote(c.ctx, api.AcceptQuote{CommandID: "accept-Q-AG-H", TenantID: c.tenant, ActorID: "sales-1", QuoteID: "Q-AG-H", ExpectedRevision: 2,
		EvidenceKind: api.EvidenceEmail, EvidenceRef: "agreed", OccurredAt: c.h(7)}))
	// once an agreement expired unaccepted, its draft changes and is quoted again
	agreementCard(c, "C-SHORT", c.h(5), c.h(10), lane("AEDXB", freight(3000)))
	c.must(c.module.CreateQuote(c.ctx, agreement(c, "Q-SHORT", "C-SHORT", c.h(5), c.h(10))))
	c.must(c.module.ExpireQuote(c.ctx, api.ExpireQuote{CommandID: "expire-Q-SHORT", TenantID: c.tenant, ActorID: "rate-scheduler", QuoteID: "Q-SHORT",
		ExpectedRevision: 1, OccurredAt: c.h(10)}))
	c.must(c.module.SetRateCardEntries(c.ctx, c.set("C-SHORT", 1, "sales-1", lane("AEDXB", freight(3500)))))

	n := 0
	for name, tc := range map[string]struct {
		edit func(*api.CreateQuote)
		text string
	}{
		"lines on an agreement": {func(q *api.CreateQuote) {
			q.Lines = []api.QuoteLine{{ChargeCode: "PICKUP", Sell: cny(1), Cost: cny(0)}}
		}, "not by lines"},
		"a lane in the scope": {func(q *api.CreateQuote) { q.Scope.Origin, q.Scope.Destination = "CNPVG", "DEFRA" }, "its lanes are its card's entries"},
		"no card version":     {func(q *api.CreateQuote) { q.Agreement = nil }, "names its customer card version"},
		"another validity":    {func(q *api.CreateQuote) { q.Validity.Until = until.Add(time.Hour) }, "not the quote's validity"},
		"a tariff":            {func(q *api.CreateQuote) { q.Agreement.RateCardID = "T-AIR" }, "is not a CUSTOMER card"},
		"an unknown card":     {func(q *api.CreateQuote) { q.Agreement.RateCardID = "C-NONE" }, "is not recorded"},
		"a published version": {func(q *api.CreateQuote) { q.Agreement.RateCardID = "C-ACME" }, "takes a DRAFT"},
		"a sea agreement":     {func(q *api.CreateQuote) { q.Scope.Mode, q.Scope.ContainerMode = api.ModeSea, api.ContainerLCL }, "the agreement SEA LCL"},
	} {
		n++
		free := lane("USLAX", freight(3000)) // a lane none of the customer's agreements rates
		free.Origin = "AEDXB"
		agreementCard(c, fmt.Sprintf("C-X-%d", n), from, until, free)
		q := agreement(c, "Q-X", fmt.Sprintf("C-X-%d", n), from, until)
		q.CommandID = "create-" + name
		tc.edit(&q)
		_, err := c.module.CreateQuote(c.ctx, q)
		c.rejected(name, err, api.ErrRejected, tc.text)
	}

	// What the database refuses, as the application role.
	agreementRow := func(id, card string) string {
		return `INSERT INTO cargo_rate.quote (tenant_id,quote_id,legal_entity_id,customer_party_id,customer_party_revision,recipient_role,currency,valid_from,
        valid_until,margin_floor_bp,author,status,scope_mode,scope_container_mode,kind,agreement_rate_card_id,agreement_version_no)
        SELECT $1,'` + id + `','LE','acme',3,'CUSTOMER','CNY',v.valid_from,v.valid_until,0,'x','PRICED','AIR','LOOSE','AGREEMENT','` + card + `',1
        FROM cargo_rate.rate_card_version v WHERE v.tenant_id=$1 AND v.rate_card_id='` + card + `' AND v.version_no=1`
	}
	checkRow := func(id string, entry int) string {
		return fmt.Sprintf(`INSERT INTO cargo_rate.quote_agreement_check (tenant_id,quote_id,entry_no,checked_at,sell_minor,cost_minor)
        VALUES ($1,'%s',%d,now(),100,50)`, id, entry)
	}
	agreementCard(c, "C-DB", from, until, lane("DEFRA", freight(3000)))
	for name, tc := range map[string]struct{ sql, code, constraint string }{
		"an agreement with a line": {agreementRow("Q-D1", "C-DB") + ";\n" + checkRow("Q-D1", 1) + ";\n" +
			`INSERT INTO cargo_rate.quote_line (tenant_id,quote_id,line_no,charge_code,sell_currency,sell_minor,cost_currency,cost_minor,code_version)
            VALUES ($1,'Q-D1',1,'PICKUP','CNY',100,'CNY',0,0)`, "23514", "quote_agreement_consistent"},
		"an agreement without its lane checks": {agreementRow("Q-D2", "C-DB"), "23514", "quote_agreement_consistent"},
		"an agreement checking a lane its card lacks": {agreementRow("Q-D3", "C-DB") + ";\n" + checkRow("Q-D3", 1) + ";\n" + checkRow("Q-D3", 2),
			"23514", "quote_agreement_consistent"},
		"an agreement on a tariff": {agreementRow("Q-D4", "T-AIR") + ";\n" + checkRow("Q-D4", 1), "23514", "quote_agreement_consistent"},
		"lane checks on a ONE_OFF quote": {`INSERT INTO cargo_rate.quote (tenant_id,quote_id,legal_entity_id,customer_party_id,customer_party_revision,
            recipient_role,currency,valid_from,valid_until,margin_floor_bp,author,status) VALUES ($1,'Q-D6','LE','acme',3,'CUSTOMER','CNY',now(),
            now()+interval '1 day',0,'x','PRICED')` + ";\n" + `INSERT INTO cargo_rate.quote_line (tenant_id,quote_id,line_no,charge_code,sell_currency,
            sell_minor,cost_currency,cost_minor,code_version) VALUES ($1,'Q-D6',1,'PICKUP','CNY',100,'CNY',0,0)` + ";\n" + checkRow("Q-D6", 1), "23514",
			"quote_agreement_consistent"},
		"a lane check added later, of an entry its version lacks": {checkRow("Q-AG-DE", 2), "23514", "quote_agreement_consistent"},
		"a second agreement on a quoted version":                  {agreementRow("Q-D7", "C-ACME-DE") + ";\n" + checkRow("Q-D7", 1), "23505", "quote_agreement_version_once"},
		"SET CONSTRAINTS ALL IMMEDIATE, then a line and an unknown lane": {"SET CONSTRAINTS ALL IMMEDIATE;\n" + `WITH q AS (` + agreementRow("Q-D8", "C-DB") +
			` RETURNING tenant_id, quote_id) INSERT INTO cargo_rate.quote_agreement_check (tenant_id,quote_id,entry_no,checked_at,sell_minor,cost_minor)
            SELECT tenant_id, quote_id, 1, now(), 100, 50 FROM q` + ";\n" + checkRow("Q-D8", 7), "23514", "quote_agreement_consistent"},
		"an agreement without its mode": {strings.Replace(agreementRow("Q-D9", "C-DB"), "'AIR','LOOSE','AGREEMENT'", "NULL,NULL,'AGREEMENT'", 1) + ";\n" +
			checkRow("Q-D9", 1), "23514", "quote_scope"},
		"a sea agreement over an air draft": {strings.Replace(agreementRow("Q-D10", "C-DB"), "'AIR','LOOSE','AGREEMENT'", "'SEA','LCL','AGREEMENT'", 1) +
			";\n" + checkRow("Q-D10", 1), "23514", "quote_agreement_consistent"},
		"a customer version published without its agreement": {`UPDATE cargo_rate.rate_card_version SET status='PUBLISHED',published_by='x',
            published_at=now() WHERE tenant_id=$1 AND rate_card_id='C-DB' AND version_no=1`, "23514", "rate_version_publishable"},
		"an entry added to a quoted draft": {`INSERT INTO cargo_rate.rate_entry (tenant_id,rate_card_id,version_no,entry_no,mode,container_mode,origin,
            destination,edition,service_level,commodity,transit_days,frequency) SELECT tenant_id,rate_card_id,version_no,9,mode,container_mode,origin,
            destination,edition,service_level,commodity,transit_days,frequency FROM cargo_rate.rate_entry WHERE tenant_id=$1 AND rate_card_id='C-ACME-DE'
            AND version_no=1 AND entry_no=1`, "23001", ""},
	} {
		refused(t, name, c.statements(tc.sql), tc.code, tc.constraint)
	}
	// written past RATE, an agreement on a lane the customer's accepted agreement rates is not published by its acceptance
	if err := c.statements(agreementRow("Q-D11", "C-DB") + ";\n" + checkRow("Q-D11", 1)); err != nil {
		t.Fatal(err)
	}
	refused(t, "an accepted agreement on a lane taken", c.statements(`UPDATE cargo_rate.quote SET status='ACCEPTED',accepted_by='x',accepted_at=valid_from,
        accepted_recorded_at=now(),acceptance_evidence_kind='EMAIL',acceptance_evidence_ref='m',accepted_customer_revision=3,revision=revision+1,
        updated_at=now() WHERE tenant_id=$1 AND quote_id='Q-D11'`+";\n"+`UPDATE cargo_rate.rate_card_version SET status='PUBLISHED',published_by='x',
        published_at=now() WHERE tenant_id=$1 AND rate_card_id='C-DB' AND version_no=1`), "23514", "rate_customer_lane_taken")
}

// An AGREEMENT quote created while the draft's entries are replaced (8 rounds, head starts alternated): the quote is
// created either way, and its lane check is that of the entries that hold when it commits. Replaced after the quote
// read them, they are refused (RATE_VERSION_QUOTED); replaced before, the quote checks the new ones.
func TestAgreementRacesAnEntryChange(t *testing.T) {
	var outcomes [2]int
	for round := range 8 {
		c := newCards(t)
		from, until := c.h(5), c.h(5+90*24)
		agreementCard(c, "C-RACE", from, until, lane("DEFRA", freight(3000)))
		set := c.set("C-RACE", 1, "sales-1", lane("DEFRA", freight(2500)))
		var quoteErr, setErr error
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i, fn := range []func(){
			func() { _, quoteErr = c.module.CreateQuote(c.ctx, agreement(c, "Q-RACE", "C-RACE", from, until)) },
			func() { _, setErr = c.module.SetRateCardEntries(c.ctx, set) },
		} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if i == round%2 {
					time.Sleep(time.Duration(rand.IntN(3000)) * time.Microsecond)
				}
				fn()
			}()
		}
		close(start)
		wg.Wait()
		if quoteErr != nil {
			t.Fatalf("round %d: the agreement: %v", round, quoteErr)
		}
		sell := c.value(`SELECT sell_minor::text FROM cargo_rate.quote_agreement_check WHERE tenant_id=$1 AND quote_id='Q-RACE' AND entry_no=1`)
		switch {
		case setErr == nil && sell == "150000":
			outcomes[1]++
		case errors.Is(setErr, api.ErrRejected) && strings.Contains(setErr.Error(), "RATE_VERSION_QUOTED") && sell == "200000":
			outcomes[0]++
		default:
			t.Fatalf("round %d: entries %v, the agreement checked %s", round, setErr, sell)
		}
	}
	t.Logf("the entries were refused %d rounds and replaced first %d", outcomes[0], outcomes[1])
}
