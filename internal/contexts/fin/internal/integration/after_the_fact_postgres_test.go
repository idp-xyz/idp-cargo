//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	finmodule "go.idp.xyz/idp-cargo/internal/contexts/fin"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	"go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
)

// FIN V2.102 (batch B): the after-the-fact review list and the mark of a reviewed week, through FIN's commands on
// PostgreSQL. The world is the air consolidation CONSOL-U of three houses with `lines` cost lines of CNY 1,000.00 by
// CHARGEABLE (line 1 of USD 1,000.00 at 7.1234) apportioned over the three houses; SH-C then leaves (transferred by
// ops-9 after a rework ops-1 asked for) and its former share on every line waits for its attribution. The legal entity's
// settings are LEAN-like: F1 UNRESTRICTED, F3 ONLY_WHEN_AMOUNT_MOVES, F8 DEFAULT_TO_REMAINING. Every attribution is made
// on a clock fixed in a week three to four weeks ahead (week), because an apportionment's week is its system time, which
// FIN's clock gives; the stand-in clock makes the edges of the week testable to the microsecond.

// afterWorld is the world described above, with the week the attributions are made in and its Monday.
func afterWorld(t *testing.T, lines int) (usedWorld, string, time.Time) {
	t.Helper()
	u := newUsedWorld(t)
	u.sealedUnit()
	u.must(u.module.RecordExchangeRate(u.ctx, api.RecordExchangeRate{CommandID: "rate-usd", TenantID: u.tenant, ActorID: "acct-1", LegalEntityID: "LE",
		ExpectedRevision: 1, RateType: api.RateEstimate, From: "USD", Rate: 71_234, Scale: 4, FromDigits: 2, ToDigits: 2,
		ValidFrom: u.at.AddDate(0, 0, -1), ValidUntil: u.at.AddDate(0, 1, 0), Source: "CFETS central parity", OccurredAt: u.hours(0)}))
	u.must(u.module.RecordConsolCost(u.ctx, api.RecordConsolCost{CommandID: "cost-1", TenantID: u.tenant, ActorID: "fin-1", PoolID: u.pool, ExpectedRevision: u.revision(),
		ChargeCode: "AIR_FREIGHT", Method: api.MethodChargeable, Cost: api.ChargeSide{PartyID: "airline-mu", Amount: usd(100_000)}, Reason: "entered for the consolidation", OccurredAt: u.hours(3)}))
	for i := 2; i <= lines; i++ {
		u.record(fmt.Sprintf("cost-%d", i), fmt.Sprintf("CHARGE_%d", i), api.MethodChargeable, 100_000)
	}
	u.changeBasis(func(b *phyapi.PhysicalCostBasis) {
		b.Houses[2] = ownHouse(u.fixture, "SH-C", 3, 50, 600_000, 3, 150, 600_000)
	})
	all := make([]int, lines)
	for i := range all {
		all[i] = i + 1
	}
	u.apportionAll("apportion-1", 4, all)
	u.depart()
	u.configure(time.Time{}, le("fin.F1", "UNRESTRICTED"), le("fin.F3", "ONLY_WHEN_AMOUNT_MOVES"), le("fin.F8", "DEFAULT_TO_REMAINING"))
	start := time.Now().UTC().AddDate(0, 0, 21)
	start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	start = start.AddDate(0, 0, -((int(start.Weekday()) + 6) % 7))
	return u, domain.WeekOf(start), start
}

// bothEntities is the stand-in world that confirms a party's role for LE2 as it does for LE (its own ORG answers for LE only).
type bothEntities struct{ *world }

func (w bothEntities) ResolvePartyRole(ctx context.Context, q orgapi.ResolvePartyRole) (orgapi.PartyRole, error) {
	if q.LegalEntityID == "LE2" {
		q.LegalEntityID = "LE"
	}
	return w.world.ResolvePartyRole(ctx, q)
}

// withModule is the world with a module on the same stand-ins, which also know LE2's parties, and on the clock when it
// is given.
func (u usedWorld) withModule(clock interface{ Now() time.Time }) usedWorld {
	u.t.Helper()
	w := bothEntities{u.w}
	opts := []finmodule.Option{}
	if clock != nil {
		opts = append(opts, finmodule.WithClock(clock))
	}
	m, err := finmodule.New(u.db, w, w, w, w, w, allow{}, opts...)
	if err != nil {
		u.t.Fatal(err)
	}
	f := *u.fixture
	f.module = m
	u.ap = ap{fixture: &f, t: u.t, ctx: u.ctx}
	return u
}

// onClock is the world with FIN's clock stopped at a time.
func (u usedWorld) onClock(at time.Time) usedWorld { return u.withModule(fixedClock{at}) }

// stateLine previews one line as previewer, with the statements, and submits the preview as submitter, both on FIN's
// clock stopped at the time, which is their business time too (the recording window of the kernel allows no other).
func (u usedWorld) stateLine(id, previewer, submitter string, at time.Time, line int, statements ...api.AttributionStatement) (api.Receipt, error) {
	u.t.Helper()
	u = u.onClock(at)
	c := u.apportionCmd(id, 0, []int{line})
	c.ActorID, c.Attributions, c.OccurredAt = previewer, statements, at
	r, err := u.module.PreviewConsolCostApportionment(u.ctx, c)
	if err != nil {
		return api.Receipt{}, err
	}
	s := u.submitCmd(id+"-submit", submitter, int(r.Version), 0)
	s.OccurredAt = at
	return u.module.SubmitConsolCostApportionment(u.ctx, s)
}

// houseC states SH-C's attribution on a line.
func houseC(line int, loss, exception int64, party, voucher string) api.AttributionStatement {
	return api.AttributionStatement{LineNo: line, ShipmentID: "SH-C", Loss: loss, Exception: exception, Party: party, Voucher: voucher}
}

// atTime makes the attribution of SH-C on a line that moves no amount and has no voucher (F3), at the time, by ops-2 and
// fin-2, so that exactly one reason lists it: no voucher.
func (u usedWorld) atTime(id string, line int, at time.Time) {
	u.t.Helper()
	u.must(u.stateLine(id, "ops-2", "fin-2", at, line, houseC(line, 0, 0, "", "")))
}

// listPage reads a page of the list as alice.
func (u usedWorld) listPage(week string, limit int, after string) api.AfterTheFactPage {
	u.t.Helper()
	p, err := u.module.GetAfterTheFactReview(u.ctx, api.GetAfterTheFactReview{TenantID: u.tenant, ActorID: "alice", LegalEntityID: "LE", Week: week, Limit: limit, After: after})
	if err != nil {
		u.t.Fatal(err)
	}
	return p
}

// listed is the list of a week as "pool/apportionment/line/house reason,reason" strings.
func (u usedWorld) listed(week string) []string {
	u.t.Helper()
	var out []string
	for after := ""; ; {
		p := u.listPage(week, 500, after)
		for _, l := range p.Lines {
			out = append(out, fmt.Sprintf("%s/%d/%d/%s %s", l.PoolID, l.ApportionmentNo, l.LineNo, l.ShipmentID, strings.Join(l.Reasons, ",")))
		}
		if after = p.NextAfter; after == "" {
			return out
		}
	}
}

func sameList(t *testing.T, name string, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s:\n got %q\nwant %q", name, got, want)
	}
}

func (u usedWorld) markCmd(id, reviewer, week string) api.MarkAfterTheFactReviewed {
	return api.MarkAfterTheFactReviewed{CommandID: id, TenantID: u.tenant, ActorID: reviewer, LegalEntityID: "LE", Week: week, Note: "every line read against its claim"}
}

// The list: each reason, a line with several, a carried attribution left out, the amounts in both currencies, the
// people, another legal entity's attribution left out, and paging.
//
//	ap2 line 1 (USD 1,000.00): loss USD 50.00 with a voucher, previewed and submitted by fin-2: NO_SECOND_PERSON
//	ap3 line 2: no amount, no voucher (F3), ops-2 and fin-2: NO_VOUCHER
//	ap4 line 3: loss 10.00 with a voucher, ops-2 and fin-2: not listed (a second person, a voucher)
//	ap5 line 4: nobody states it, F8 makes it, ops-1 previews and submits: all three reasons
//	ap6 line 5: made by default, ops-2 and fin-2: NO_VOUCHER and MADE_BY_DEFAULT
//	ap7 line 1 again by ops-2 alone, carrying ap2's attribution: not listed (carried)
func TestTheAfterTheFactListOnPostgres(t *testing.T) {
	u, week, start := afterWorld(t, 5)
	h := func(n int) time.Time { return start.Add(time.Duration(n) * time.Hour) }
	u.must(u.stateLine("ap2", "fin-2", "fin-2", h(1), 1, houseC(1, 5_000, 0, "", "CLAIM-1")))
	u.atTime("ap3", 2, h(2))
	u.must(u.stateLine("ap4", "ops-2", "fin-2", h(3), 3, houseC(3, 1_000, 0, "", "CLAIM-3")))
	u.must(u.stateLine("ap5", "ops-1", "ops-1", h(4), 4))
	u.must(u.stateLine("ap6", "ops-2", "fin-2", h(5), 5))
	u.must(u.stateLine("ap7", "ops-2", "ops-2", h(6), 1))
	// a correction of line 1, with its reason and voucher, by fin-2 alone: a later apportionment on an earlier line
	corrected := houseC(1, 6_000, 0, "", "CLAIM-1B")
	corrected.Reason = "the claim was revised"
	u.must(u.stateLine("ap8", "fin-2", "fin-2", h(6).Add(time.Minute), 1, corrected))
	if n := len(u.ap.pool(u.pool).Apportionments); n != 8 {
		t.Fatalf("%d apportionments", n)
	}

	sameList(t, "the list", u.listed(week),
		"P-U/2/1/SH-C NO_SECOND_PERSON",
		"P-U/3/2/SH-C NO_VOUCHER",
		"P-U/5/4/SH-C NO_SECOND_PERSON,NO_VOUCHER,MADE_BY_DEFAULT",
		"P-U/6/5/SH-C NO_VOUCHER,MADE_BY_DEFAULT",
		"P-U/8/1/SH-C NO_SECOND_PERSON")
	page := u.listPage(week, 10, "")
	if page.LegalEntityID != "LE" || page.Week != week || !page.WeekStart.Equal(start) || !page.WeekEnd.Equal(start.AddDate(0, 0, 7)) || page.NextAfter != "" || page.Review.Reviewed {
		t.Fatalf("the page: %+v", page)
	}
	first := page.Lines[0]
	if first.ConsolidationID != "CONSOL-U" || first.JobID != "JOB-C" || first.PreviewNo != 2 || !first.EnteredAt.Equal(h(1)) || first.SubmittedBy != "fin-2" || first.PreviewedBy != "fin-2" ||
		first.Loss != usd(5_000) || first.LossLocal != cny(35_617) || first.Exception != usd(0) || first.ExceptionLocal != cny(0) || first.Voucher != "CLAIM-1" || first.Party != "" {
		t.Fatalf("the first line, USD 50.00 of a USD line, in both currencies: %+v", first)
	}
	if l := page.Lines[1]; l.SubmittedBy != "fin-2" || l.PreviewedBy != "ops-2" || l.Voucher != "" || l.Loss != cny(0) || l.Exception != cny(0) {
		t.Fatalf("the second line: %+v", l)
	}
	if l := page.Lines[2]; l.SubmittedBy != "ops-1" || l.PreviewedBy != "ops-1" || l.Voucher != "" || l.LineNo != 4 || l.ApportionmentNo != 5 {
		t.Fatalf("the third line, made by default: %+v", l)
	}

	// paging: one line a page, then three and two; the cursor is the page's last line, and a page does not repeat or skip
	var paged []string
	after := ""
	for i := 0; i < 5; i++ {
		p := u.listPage(week, 1, after)
		if len(p.Lines) != 1 || (p.NextAfter == "") != (i == 4) {
			t.Fatalf("page %d: %+v", i, p)
		}
		paged = append(paged, fmt.Sprintf("%d", p.Lines[0].ApportionmentNo))
		after = p.NextAfter
	}
	sameList(t, "the pages of one", paged, "2", "3", "5", "6", "8")
	p3 := u.listPage(week, 3, "")
	p1 := u.listPage(week, 3, p3.NextAfter)
	if len(p3.Lines) != 3 || p3.NextAfter == "" || len(p1.Lines) != 2 || p1.Lines[0].ApportionmentNo != 6 || p1.NextAfter != "" {
		t.Fatalf("pages of three: %+v and %+v", p3, p1)
	}
	if exact := u.listPage(week, 5, ""); len(exact.Lines) != 5 || exact.NextAfter != "" {
		t.Fatalf("a page that holds the whole list has no next page: %+v", exact)
	}

	// another legal entity's attribution, in the same week, is in its own list and not in this one
	u.otherEntity(h(7))
	sameList(t, "the list of LE, with LE2's attribution made", u.listed(week),
		"P-U/2/1/SH-C NO_SECOND_PERSON", "P-U/3/2/SH-C NO_VOUCHER", "P-U/5/4/SH-C NO_SECOND_PERSON,NO_VOUCHER,MADE_BY_DEFAULT", "P-U/6/5/SH-C NO_VOUCHER,MADE_BY_DEFAULT", "P-U/8/1/SH-C NO_SECOND_PERSON")
	other, err := u.module.GetAfterTheFactReview(u.ctx, api.GetAfterTheFactReview{TenantID: u.tenant, ActorID: "alice", LegalEntityID: "LE2", Week: week, Limit: 10})
	if err != nil || len(other.Lines) != 1 || other.Lines[0].PoolID != "P-2" || other.Lines[0].ShipmentID != "SH-Z" ||
		strings.Join(other.Lines[0].Reasons, ",") != "NO_SECOND_PERSON" || other.Lines[0].Loss != cny(100) {
		t.Fatalf("the list of LE2: %+v %v", other, err)
	}

	// the other weeks are empty, and the query refuses what it cannot read
	sameList(t, "the week before", u.listed(domain.WeekOf(start.AddDate(0, 0, -1))))
	sameList(t, "the week after", u.listed(domain.WeekOf(start.AddDate(0, 0, 7))))
	for name, q := range map[string]api.GetAfterTheFactReview{
		"a week that is not one":        {Week: "2026-W54", Limit: 1},
		"a week written otherwise":      {Week: "2026-48", Limit: 1},
		"no legal entity":               {Week: week, Limit: 1, LegalEntityID: "-"},
		"a page of 0":                   {Week: week, Limit: 0},
		"a page of 501":                 {Week: week, Limit: 501},
		"a cursor that is not one":      {Week: week, Limit: 1, After: "x"},
		"a legal entity with no policy": {Week: week, Limit: 1, LegalEntityID: "LE-9"},
	} {
		q.TenantID, q.ActorID = u.tenant, "alice"
		if q.LegalEntityID == "" {
			q.LegalEntityID = "LE"
		}
		if q.LegalEntityID == "-" {
			q.LegalEntityID = ""
		}
		if _, err := u.module.GetAfterTheFactReview(u.ctx, q); !errors.Is(err, api.ErrRejected) {
			t.Errorf("%s: %v, want a refusal", name, err)
		}
	}
	if _, err := u.module.GetAfterTheFactReview(u.ctx, api.GetAfterTheFactReview{TenantID: u.tenant, ActorID: "intruder", LegalEntityID: "LE", Week: week, Limit: 1}); !errors.Is(err, api.ErrUnauthorized) {
		t.Errorf("an actor with no permission: %v", err)
	}
	if _, err := u.module.GetAfterTheFactReview(u.ctx, api.GetAfterTheFactReview{TenantID: u.tenant, LegalEntityID: "LE", Week: week, Limit: 1}); !errors.Is(err, api.ErrUnauthenticated) {
		t.Errorf("no actor: %v", err)
	}
}

// otherEntity makes an attribution in LE2's pool P-2 on the three houses SH-X, SH-Y and SH-Z, SH-Z having left, stated
// by fin-2 alone (so that it would be listed) at the time.
func (u usedWorld) otherEntity(at time.Time) {
	u.t.Helper()
	u = u.withModule(nil)
	u.must(u.module.OpenFinancePolicy(u.ctx, api.OpenFinancePolicy{CommandID: "policy-2", TenantID: u.tenant, ActorID: "acct-1", LegalEntityID: "LE2",
		FunctionalCurrency: "CNY", BookCode: "MAIN", OccurredAt: u.hours(0)}))
	u.w.mu.Lock()
	for _, s := range []string{"X", "Y", "Z"} {
		d := u.w.drivers["SH-A"]
		d.ShipmentID, d.LegalEntityID = "SH-"+s, "LE2"
		u.w.drivers["SH-"+s] = d
	}
	b := u.w.costBases["CONSOL-U"]
	b.ConsolidationID, b.OperatingLegalEntityID, b.FormerHouses = "CONSOL-2", "LE2", nil
	b.Houses = []phyapi.HouseCostBasis{ownHouse(u.fixture, "SH-X", 4, 300, 1_200_000, 4, 300, 1_200_000), ownHouse(u.fixture, "SH-Y", 2, 100, 1_200_000, 2, 100, 1_200_000),
		ownHouse(u.fixture, "SH-Z", 3, 50, 600_000, 3, 150, 600_000)}
	u.w.costBases["CONSOL-2"] = b
	u.w.mu.Unlock()
	for _, s := range []string{"X", "Y", "Z"} {
		u.must(u.module.OpenFreightJob(u.ctx, api.OpenFreightJob{CommandID: "open-" + s, TenantID: u.tenant, ActorID: "fin-1", JobID: "JOB-" + s, ShipmentID: "SH-" + s, OccurredAt: u.hours(1)}))
	}
	v := u
	v.pool = "P-2"
	v.must(v.module.OpenConsolCostPool(v.ctx, api.OpenConsolCostPool{CommandID: "pool-2", TenantID: v.tenant, ActorID: "fin-1", PoolID: "P-2", ConsolidationID: "CONSOL-2", OccurredAt: v.hours(2)}))
	v.record("cost-le2", "AIR_FREIGHT", api.MethodChargeable, 100_000)
	v.apportionAll("apportion-le2", 4, []int{1})
	u.w.mu.Lock()
	b = u.w.costBases["CONSOL-2"]
	b.Revision, b.Houses = 8, b.Houses[:2]
	b.FormerHouses = []phyapi.FormerHouse{{ShipmentID: "SH-Z", End: phyapi.FormerTransferred, TransferredTo: "CONSOL-3", Reason: "WRONG_CONSOL", EndedBy: "ops-9", EndedAt: u.hours(5),
		EndedEnteredAt: u.hours(5), ReworkFrom: phyapi.ReworkFromEntry, ReworkExecutionID: "X-1", ReworkNo: 1, ReworkUnitNo: 1, ReworkAskedBy: "ops-1"}}
	u.w.costBases["CONSOL-2"] = b
	u.w.mu.Unlock()
	v.must(v.stateLine("attribute-le2", "fin-2", "fin-2", at, 1, api.AttributionStatement{LineNo: 1, ShipmentID: "SH-Z", Loss: 100, Voucher: "CLAIM-LE2"}))
}

// The week is the half-open interval from Monday 00:00:00 UTC to the next Monday, by the apportionment's system time to
// the microsecond: an attribution at the Monday's first instant is in it, one a microsecond before is in the week
// before, one a microsecond before the next Monday is in it and one at the next Monday is in the week after.
func TestTheWeekOfTheListHasItsBoundsOnPostgres(t *testing.T) {
	u, week, start := afterWorld(t, 4)
	end := start.AddDate(0, 0, 7)
	u.atTime("before", 1, start.Add(-time.Microsecond))
	u.atTime("first", 2, start)
	u.atTime("last", 3, end.Add(-time.Microsecond))
	u.atTime("after", 4, end)
	sameList(t, "the week before", u.listed(domain.WeekOf(start.Add(-time.Microsecond))), "P-U/2/1/SH-C NO_VOUCHER")
	sameList(t, "the week", u.listed(week), "P-U/3/2/SH-C NO_VOUCHER", "P-U/4/3/SH-C NO_VOUCHER")
	sameList(t, "the week after", u.listed(domain.WeekOf(end)), "P-U/5/4/SH-C NO_VOUCHER")
	if domain.WeekOf(start.Add(-time.Microsecond)) == week || domain.WeekOf(end) == week {
		t.Fatal("the bounds are not another week's")
	}
}

// The review: another person, once, replayed. The reviewer is neither the submitter nor the previewer of any line of the
// week's list (the refusal names the line and the way out); a week is reviewed once and only once it has begun; a
// replay answers the stored receipt and writes nothing more; the week's answer says who reviewed it and when, and the
// list still matches.
func TestAWeekIsReviewedOnceByAnotherPersonAndReplayedOnPostgres(t *testing.T) {
	u, week, start := afterWorld(t, 3)
	u.atTime("one", 1, start.Add(time.Hour))
	u.must(u.stateLine("two", "ops-2", "ops-2", start.Add(2*time.Hour), 2, houseC(2, 100, 0, "", "CLAIM-2"))) // one person, with a voucher
	sameList(t, "the list", u.listed(week), "P-U/2/1/SH-C NO_VOUCHER", "P-U/3/2/SH-C NO_SECOND_PERSON")
	after := u.onClock(start.AddDate(0, 0, 7).Add(time.Hour)) // the week is over
	for _, who := range []string{"fin-2", "ops-2"} {
		_, err := after.module.MarkAfterTheFactReviewed(after.ctx, after.markCmd("bar-"+who, who, week))
		after.refused("the reviewer "+who, err, "FIN_AFTER_THE_FACT_REVIEWER", "another person reviews it", "apportionment 2 of pool P-U", "line 1 of house SH-C")
	}
	if p := u.listPage(week, 10, ""); p.Review.Reviewed {
		t.Fatalf("a refused review marked the week: %+v", p.Review)
	}
	cmd := after.markCmd("review-1", "head-1", week)
	r := after.must(after.module.MarkAfterTheFactReviewed(after.ctx, cmd))
	if r.SubjectID != "LE/"+week || r.NewRevision != 1 || r.Version != 2 || r.CommandID != "review-1" {
		t.Fatalf("the receipt: %+v", r)
	}
	if again := after.must(after.module.MarkAfterTheFactReviewed(after.ctx, cmd)); again != r {
		t.Fatalf("the replay: %+v, then %+v", r, again)
	}
	changed := cmd
	changed.Note = "another note"
	if _, err := after.module.MarkAfterTheFactReviewed(after.ctx, changed); !errors.Is(err, api.ErrIdempotencyConflict) {
		t.Errorf("a command id reused with another note: %v", err)
	}
	if n := countOf(u.outboxTypes(t), "AfterTheFactReviewed"); n != 1 {
		t.Fatalf("%d events", n)
	}
	var subject string
	if err := u.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT subject FROM bento.outbox WHERE scope=$1 AND event_type='AfterTheFactReviewed.v1'`, u.tenant).Scan(&subject)
	}); err != nil || subject != "LE/"+week {
		t.Fatalf("the event's subject: %q %v", subject, err)
	}
	m := u.listPage(week, 10, "").Review
	if !m.Reviewed || m.ReviewedBy != "head-1" || !m.ReviewedAt.Equal(start.AddDate(0, 0, 7).Add(time.Hour)) || m.Note != cmd.Note || m.ListSize != 2 || len(m.ListDigest) != 64 ||
		m.CurrentSize != 2 || m.CurrentDigest != m.ListDigest || !m.Matches {
		t.Fatalf("the review: %+v", m)
	}
	// a second review of the week, by anyone, is refused, naming who reviewed it
	for _, who := range []string{"head-1", "head-2"} {
		_, err := after.module.MarkAfterTheFactReviewed(after.ctx, after.markCmd("review-2-"+who, who, week))
		after.refused("a second review by "+who, err, "FIN_AFTER_THE_FACT_REVIEWED", "reviewed by head-1")
	}
	// refusals of the command's form, each naming what to do
	for name, c := range map[string]api.MarkAfterTheFactReviewed{
		"a week that is not one": after.markCmd("bad-week", "head-1", "2026-W54"),
		"an empty note": func() api.MarkAfterTheFactReviewed {
			c := after.markCmd("bad-note", "head-1", week)
			c.Note = " "
			return c
		}(),
		"a note of 2001": func() api.MarkAfterTheFactReviewed {
			c := after.markCmd("long-note", "head-1", week)
			c.Note = strings.Repeat("n", 2001)
			return c
		}(),
		"no policy": func() api.MarkAfterTheFactReviewed {
			c := after.markCmd("no-policy", "head-1", week)
			c.LegalEntityID = "LE-9"
			return c
		}(),
	} {
		if _, err := after.module.MarkAfterTheFactReviewed(after.ctx, c); !errors.Is(err, api.ErrRejected) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := after.module.MarkAfterTheFactReviewed(after.ctx, func() api.MarkAfterTheFactReviewed { c := after.markCmd("intruder", "intruder", week); return c }()); !errors.Is(err, api.ErrUnauthorized) {
		t.Errorf("an actor with no permission: %v", err)
	}
	// a week that has not begun is not reviewed; a week with nothing listed is, with a list of 0
	future := domain.WeekOf(start.AddDate(0, 0, 14))
	_, err := after.module.MarkAfterTheFactReviewed(after.ctx, after.markCmd("future", "head-1", future))
	after.refused("a week that has not begun", err, "FIN_AFTER_THE_FACT_WEEK_NOT_BEGUN", "review it once it has")
	empty := domain.WeekOf(start.AddDate(0, 0, -7))
	if r = after.must(after.module.MarkAfterTheFactReviewed(after.ctx, after.markCmd("empty", "fin-2", empty))); r.Version != 0 {
		t.Fatalf("a week with an empty list: %+v", r)
	}
	if m = u.listPage(empty, 10, "").Review; !m.Reviewed || m.ListSize != 0 || !m.Matches || m.ReviewedBy != "fin-2" {
		t.Fatalf("the review of the empty week: %+v", m)
	}
}

// The digest: a week reviewed while it runs matches its list until an attribution is recorded in it afterwards, and then
// the answer says the list does not match, with the size now; an attribution at the next Monday, in the week after, does
// not change it. (A list cannot change once the week is over, because an apportionment is stamped with the system time
// of its recording: only an attribution recorded within the week after the review can change what was reviewed.)
func TestTheDigestTellsAListThatChangedAfterItsReviewOnPostgres(t *testing.T) {
	u, week, start := afterWorld(t, 4)
	end := start.AddDate(0, 0, 7)
	u.atTime("one", 1, start.Add(time.Hour))
	review := u.onClock(start.Add(24 * time.Hour))
	review.must(review.module.MarkAfterTheFactReviewed(review.ctx, review.markCmd("review", "head-1", week)))
	if m := u.listPage(week, 10, "").Review; !m.Reviewed || m.ListSize != 1 || m.CurrentSize != 1 || !m.Matches {
		t.Fatalf("the review before the change: %+v", m)
	}
	u.atTime("late", 2, end.Add(-time.Microsecond))
	m := u.listPage(week, 10, "").Review
	if !m.Reviewed || m.ListSize != 1 || m.CurrentSize != 2 || m.Matches || m.CurrentDigest == m.ListDigest || m.ReviewedBy != "head-1" {
		t.Fatalf("the review after an attribution later in the week: %+v", m)
	}
	u.atTime("next", 3, end)
	if m2 := u.listPage(week, 10, "").Review; m2.CurrentSize != 2 || m2.CurrentDigest != m.CurrentDigest || m2.Matches {
		t.Fatalf("an attribution of the week after changed this week's list: %+v", m2)
	}
	// a week reviewed after it ended, with a list that does not change, matches at every later read
	next := u.onClock(end.Add(time.Hour))
	next.must(next.module.MarkAfterTheFactReviewed(next.ctx, next.markCmd("review-next", "head-1", domain.WeekOf(end))))
	u.atTime("later", 4, end.AddDate(0, 0, 7))
	if m3 := u.listPage(domain.WeekOf(end), 10, "").Review; !m3.Reviewed || m3.ListSize != 1 || !m3.Matches {
		t.Fatalf("the week reviewed after it ended: %+v", m3)
	}
}

// holdWeek takes the week's lock FIN's review takes, on a second connection, inside a transaction, and answers the
// connection's backend pid; release commits it.
func holdWeek(t *testing.T, u usedWorld, week string) (pid int, release func()) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("IDP_CARGO_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('cargo_fin.after_the_fact_review/' || $1::text || '/' || $2::text || '/' || $3::text, 0))`,
		u.tenant, "LE", week); err == nil {
		err = tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid)
	}
	if err != nil {
		t.Fatal(err)
	}
	var once bool
	release = func() {
		if !once {
			once = true
			tx.Commit(ctx)
			conn.Close(ctx)
		}
	}
	t.Cleanup(release)
	return pid, release
}

// Two reviews of one week at once take turns on the week's lock. The lock is held on a second connection; the reviews
// are started one after another, each only once the sessions before it are blocked by the holder (pg_blocking_pids); the
// lock is released and the first started is the one that is made, the other is refused as reviewed, naming the first.
// Two copies of one command answer one receipt and write one row and one event.
func TestTwoReviewsOfOneWeekAtOnceTakeTurnsOnPostgres(t *testing.T) {
	u, week, start := afterWorld(t, 2)
	u.atTime("one", 1, start.Add(time.Hour))
	after := u.onClock(start.AddDate(0, 0, 7).Add(time.Hour))
	pid, release := holdWeek(t, u, week)
	var receipts [2]api.Receipt
	var errs [2]error
	done := make(chan int, 2)
	reviewers := []string{"head-1", "head-2"}
	for i := range reviewers {
		go func() {
			receipts[i], errs[i] = after.module.MarkAfterTheFactReviewed(after.ctx, after.markCmd("review-"+reviewers[i], reviewers[i], week))
			done <- i
		}()
		waitBlockedBy(t, pid, i+1)
	}
	release()
	<-done
	<-done
	if errs[0] != nil || receipts[0].SubjectID != "LE/"+week {
		t.Fatalf("the first started: %+v %v", receipts[0], errs[0])
	}
	after.refused("the second started", errs[1], "FIN_AFTER_THE_FACT_REVIEWED", "reviewed by head-1")
	if m := u.listPage(week, 10, "").Review; m.ReviewedBy != "head-1" {
		t.Fatalf("the week was reviewed by %s", m.ReviewedBy)
	}

	// the same command twice: both answer one receipt
	week2 := domain.WeekOf(start.AddDate(0, 0, -7))
	pid, release = holdWeek(t, u, week2)
	cmd := after.markCmd("same", "head-1", week2)
	for i := range receipts {
		go func() {
			receipts[i], errs[i] = after.module.MarkAfterTheFactReviewed(after.ctx, cmd)
			done <- i
		}()
		waitBlockedBy(t, pid, i+1)
	}
	release()
	<-done
	<-done
	if errs[0] != nil || errs[1] != nil || receipts[0] != receipts[1] || receipts[0].SubjectID != "LE/"+week2 {
		t.Fatalf("two copies of one command: %+v %+v, %v %v", receipts[0], receipts[1], errs[0], errs[1])
	}
	if n := countOf(u.outboxTypes(t), "AfterTheFactReviewed"); n != 2 {
		t.Fatalf("%d events for two weeks", n)
	}
}

// The database keeps what it can of the review, by raw writes on the application role: one row for a legal entity and a
// week, a legal entity with a policy, the form of the week and its Monday, a review made after its week began, the size
// of the note, the reviewer, the size and the digest, no change and no removal, and row security. Past what it keeps,
// the controls: a row that breaks nothing is written, and the bar on the reviewer and the list are the domain's alone:
// the database accepts a review by the submitter of a line and a size and digest that are not the list's.
func TestTheDatabaseKeepsTheReviewRulesOnPostgres(t *testing.T) {
	u, _, start := afterWorld(t, 1)
	u.atTime("one", 1, start.Add(time.Hour))
	f := u.fixture
	insert := func(entity, week, startAt, by, note string, size int, digest, reviewedAt string) string {
		return fmt.Sprintf(`INSERT INTO cargo_fin.after_the_fact_review (tenant_id,legal_entity_id,week,week_start,reviewed_by,note,list_size,list_digest,reviewed_at)
            VALUES ($1,'%s','%s','%s','%s','%s',%d,'%s','%s')`, entity, week, startAt, by, note, size, digest, reviewedAt)
	}
	hex := strings.Repeat("a", 64)
	row := func(week, startAt string) string {
		return insert("LE", week, startAt+"+00", "head-1", "a note", 1, hex, "2099-01-01 00:00:00+00")
	}
	w := "2026-W48"
	monday := "2026-11-23 00:00:00"
	with := func(sql, old, new string) string { return strings.Replace(sql, old, new, 1) }
	ok := row(w, monday)
	if err := u.rawRun(ok); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ sql, state, check string }{
		"a second review of the week":     {row(w, monday), "23505", "after_the_fact_review_pkey"},
		"a legal entity with no policy":   {with(row("2026-W49", "2026-11-30 00:00:00"), "'LE'", "'LE-9'"), "23503", ""},
		"a week with no W":                {row("2026-49", "2026-11-30 00:00:00"), "23514", "after_the_fact_review_week_form"},
		"week 00":                         {row("2026-W00", "2025-12-22 00:00:00"), "23514", "after_the_fact_review_week_form"},
		"week 54":                         {row("2026-W54", "2027-01-11 00:00:00"), "23514", "after_the_fact_review_week_form"},
		"a lower-case w":                  {row("2026-w49", "2026-11-30 00:00:00"), "23514", "after_the_fact_review_week_form"},
		"a week of one digit":             {row("2026-W9", "2026-02-23 00:00:00"), "23514", "after_the_fact_review_week_form"},
		"a Monday that is not the week's": {row("2026-W49", "2026-11-23 00:00:00"), "23514", "after_the_fact_review_week_start"},
		"a Tuesday":                       {row("2026-W49", "2026-12-01 00:00:00"), "23514", "after_the_fact_review_week_start"},
		"a Monday at 00:00:01":            {row("2026-W49", "2026-11-30 00:00:01"), "23514", "after_the_fact_review_week_start"},
		"week 53 of a year that has 52":   {row("2027-W53", "2027-01-04 00:00:00"), "23514", "after_the_fact_review_week_start"},
		"a review before its week began":  {with(row("2026-W49", "2026-11-30 00:00:00"), "2099-01-01 00:00:00+00", "2026-11-29 23:59:59.999999+00"), "23514", "after_the_fact_review_week_begun"},
		"an empty note":                   {with(row("2026-W49", "2026-11-30 00:00:00"), "'a note'", "' '"), "23514", "after_the_fact_review_note_check"},
		"a note of 2001 characters":       {with(row("2026-W49", "2026-11-30 00:00:00"), "'a note'", "repeat('n',2001)"), "23514", "after_the_fact_review_note_check"},
		"an empty reviewer":               {with(row("2026-W49", "2026-11-30 00:00:00"), "'head-1'", "''"), "23514", "after_the_fact_review_reviewed_by_check"},
		"a reviewer of 129 characters":    {with(row("2026-W49", "2026-11-30 00:00:00"), "'head-1'", "repeat('r',129)"), "23514", "after_the_fact_review_reviewed_by_check"},
		"a negative list size":            {with(row("2026-W49", "2026-11-30 00:00:00"), ",1,'"+hex, ",-1,'"+hex), "23514", "after_the_fact_review_list_size_check"},
		"a digest in capitals":            {with(row("2026-W49", "2026-11-30 00:00:00"), hex, strings.Repeat("A", 64)), "23514", "after_the_fact_review_list_digest_check"},
		"a digest of 63 characters":       {with(row("2026-W49", "2026-11-30 00:00:00"), hex, hex[:63]), "23514", "after_the_fact_review_list_digest_check"},
		"a row changed":                   {`UPDATE cargo_fin.after_the_fact_review SET note='changed' WHERE tenant_id=$1`, "42501|23001", ""},
		"a row removed":                   {`DELETE FROM cargo_fin.after_the_fact_review WHERE tenant_id=$1`, "42501|23001", ""},
		"a row of another tenant":         {with(row("2026-W49", "2026-11-30 00:00:00"), "VALUES ($1,", "VALUES ($1||'x',"), "42501", ""},
	} {
		err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			_, err := e.Exec(ctx, c.sql, f.tenant)
			return err
		})
		state, constraint := sqlState(err)
		if state == "" || !strings.Contains(c.state, state) || c.check != "" && constraint != c.check {
			t.Errorf("%s: %v (state %s, constraint %s), want %s %s", name, err, state, constraint, c.state, c.check)
		}
	}
	// the longest note and the longest reviewer are accepted; the controls of the form
	for name, sql := range map[string]string{
		"a note of 2000 characters":                          with(row("2026-W49", "2026-11-30 00:00:00"), "'a note'", "repeat('n',2000)"),
		"week 53 of a year that has":                         row("2026-W53", "2026-12-28 00:00:00"),
		"week 01 of the next year":                           row("2027-W01", "2027-01-04 00:00:00"),
		"week 01 with the days before it in the year before": row("2026-W01", "2025-12-29 00:00:00"),
		// what only the domain keeps: a review by the submitter of the week's list, with a size and a digest that are not its
		"a review by the submitter, of a list it does not match": with(with(row("2026-W50", "2026-12-07 00:00:00"), "'head-1'", "'fin-2'"), ",1,'"+hex, ",7,'"+hex),
	} {
		if err := u.rawRun(sql); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// row security is enabled and forced, and another tenant sees none of the rows
	var enabled, forced bool
	var seen int
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		if err := e.QueryRow(ctx, `SELECT c.relrowsecurity, c.relforcerowsecurity FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
            WHERE n.nspname = 'cargo_fin' AND c.relname = 'after_the_fact_review'`).Scan(&enabled, &forced); err != nil {
			return err
		}
		return e.QueryRow(ctx, `SELECT count(*) FROM cargo_fin.after_the_fact_review WHERE tenant_id <> $1`, f.tenant).Scan(&seen)
	}); err != nil || !enabled || !forced || seen != 0 {
		t.Fatalf("row security: enabled %v, forced %v, %d rows of other tenants, %v", enabled, forced, seen, err)
	}
}

// The list is read with ReadProfit and the review is made with ReviewAfterTheFact, each alone: a module whose authorizer
// allows only ReadProfit reads the list and cannot review, and one that allows only ReviewAfterTheFact reviews and cannot
// read it. A refused review writes nothing, and a command id a refused person used is refused again once it is allowed
// for another (the receipt is not answered to someone who may not make the command).
func TestTheListNeedsReadProfitAndTheReviewItsOwnPermissionOnPostgres(t *testing.T) {
	u, week, start := afterWorld(t, 1)
	u.atTime("one", 1, start.Add(time.Hour))
	build := func(allowed onlyActions) *finmodule.Module {
		m, err := finmodule.New(u.db, u.w, u.w, u.w, u.w, u.w, allowed, finmodule.WithClock(fixedClock{start.AddDate(0, 0, 7).Add(time.Hour)}))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	reader, reviewer := build(onlyActions{api.ActionReadProfit}), build(onlyActions{api.ActionReviewAfterTheFact})
	q := api.GetAfterTheFactReview{TenantID: u.tenant, ActorID: "alice", LegalEntityID: "LE", Week: week, Limit: 10}
	if p, err := reader.GetAfterTheFactReview(u.ctx, q); err != nil || len(p.Lines) != 1 {
		t.Fatalf("a module that allows ReadProfit reads the list: %+v %v", p, err)
	}
	if _, err := reviewer.GetAfterTheFactReview(u.ctx, q); !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("ReviewAfterTheFact alone reads the list: %v", err)
	}
	cmd := u.markCmd("review", "head-1", week)
	if _, err := reader.MarkAfterTheFactReviewed(u.ctx, cmd); !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("ReadProfit alone reviews: %v", err)
	}
	if p := u.listPage(week, 10, ""); p.Review.Reviewed {
		t.Fatalf("a refused review marked the week: %+v", p.Review)
	}
	if r, err := reviewer.MarkAfterTheFactReviewed(u.ctx, cmd); err != nil || r.SubjectID != "LE/"+week {
		t.Fatalf("ReviewAfterTheFact reviews: %+v %v", r, err)
	}
	if _, err := reader.MarkAfterTheFactReviewed(u.ctx, cmd); !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("a replay by a module that may not review answers the receipt: %v", err)
	}
}
