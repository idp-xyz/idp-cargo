package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	bentoapp "go.idp.xyz/idp-bento-go/application"

	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
)

// A caller the policy does not name, or whose roles name no legal entity the call is about, is refused with one
// sentence before FIN is asked anything; a job of a legal entity the caller may not see is told as a job that does not
// exist.
func TestDefaultDenyAndTheLegalEntityScope(t *testing.T) {
	fin := richFIN()
	r := newRig(t, fin)
	nobody := []*who{
		{"gw", "alice", "unknown-role"}, {"gw", "alice", ""}, {"unknown-tenant", "alice", "analyst"}, {"", "alice", "analyst"},
		{"gw", "alice with blanks", "analyst"}, {"GW", "alice", "analyst"}, {"gw", "alice", "ANALYST"},
	}
	for _, c := range nobody {
		for tool, args := range map[string]map[string]any{"list_loss_jobs": {"legal_entity_id": "LE"}, "get_job_profit": {"job_id": "JOB-1"},
			"list_job_cost_reviews": {"job_id": "JOB-1"}} {
			if got := r.call(c, tool, args); !got.isError || got.text != msgNotAllowed {
				t.Errorf("%+v, %s: %q", *c, tool, got.text)
			}
		}
	}
	// no verified caller at all: the request does not name one
	if got := r.call(nil, "list_loss_jobs", map[string]any{"legal_entity_id": "LE"}); !got.isError || got.text != msgNotAllowed {
		t.Errorf("no caller: %q", got.text)
	}
	if fin.total() != 0 {
		t.Fatalf("FIN was asked %d times for callers that are not allowed", fin.total())
	}

	// a legal entity the role does not name
	if got := r.call(&alice, "list_loss_jobs", map[string]any{"legal_entity_id": "LE2"}); !got.isError || got.text != msgNotAllowed || fin.total() != 0 {
		t.Errorf("a legal entity not allowed: %q (FIN asked %d times)", got.text, fin.total())
	}
	// a job of a legal entity the caller may not see: the answer is the one for a job that is not there
	fin.entity = func(_ context.Context, q api.GetJobLegalEntity) (string, error) {
		if q.JobID == "JOB-MISSING" {
			return "", api.ErrNotFound
		}
		return "LE2", nil
	}
	for _, tool := range []string{"get_job_profit", "list_job_cost_reviews"} {
		hidden := r.call(&alice, tool, map[string]any{"job_id": "JOB-HIDDEN"})
		missing := r.call(&alice, tool, map[string]any{"job_id": "JOB-MISSING"})
		if !hidden.isError || hidden.text != msgNotFound || missing.text != msgNotFound || hidden.body[strings.Index(hidden.body, `"result"`):] != missing.body[strings.Index(missing.body, `"result"`):] {
			t.Errorf("%s: a hidden job reads %q, a missing one %q", tool, hidden.text, missing.text)
		}
	}
	// the job's legal entity was asked first, and nothing about the hidden job was read
	if fin.count("GetJobProfit") != 0 || fin.count("ListCostReviewRequests") != 0 || fin.count("GetJobLegalEntity") != 4 {
		t.Errorf("FIN was asked %v", fin.calls)
	}
	// a caller of another role sees LE2; a wide role sees both; the union of roles counts
	if got := r.call(&who{"gw", "alice", "other"}, "get_job_profit", map[string]any{"job_id": "JOB-HIDDEN"}); got.isError {
		t.Errorf("the role that sees LE2: %q", got.text)
	}
	for _, roles := range []string{"wide", "analyst,other", "unknown,other", "wide,analyst", "analyst,wide", "wide,unknown", "unknown,wide"} {
		if got := r.call(&who{"gw", "alice", roles}, "get_job_profit", map[string]any{"job_id": "JOB-HIDDEN"}); got.isError {
			t.Errorf("roles %s: %q", roles, got.text)
		}
	}
	if got := r.call(&who{"gw", "alice", "analyst"}, "get_job_profit", map[string]any{"job_id": "JOB-HIDDEN"}); got.text != msgNotFound {
		t.Errorf("one role: %q", got.text)
	}
}

// FIN is called as the CargoPai tenant and the member the assertion names, with the permissions the roles grant: its
// own authorizer decides from that principal, and what the caller names is not believed.
func TestFINSeesTheTenantAndTheMemberOfTheAssertion(t *testing.T) {
	fin := richFIN()
	fin.jobs = func(_ context.Context, q api.ListJobProfits) (api.JobProfitPage, error) {
		return api.JobProfitPage{LegalEntityID: q.LegalEntityID, AsOf: asOf, Rows: []api.JobProfitRow{lossJob(1)}}, nil
	}
	r := newRig(t, fin)
	for tool, args := range map[string]map[string]any{"list_loss_jobs": {"legal_entity_id": "LE"}, "get_job_profit": {"job_id": "JOB-1"},
		"list_job_cost_reviews": {"job_id": "JOB-1"}} {
		if got := r.call(&who{"gw-b", "erin", "analyst"}, tool, args); got.isError {
			t.Fatalf("%s: %s", tool, got.text)
		}
	}
	fin.mu.Lock()
	defer fin.mu.Unlock()
	if len(fin.queries) != 5 { // two of the three tools read the job's legal entity first
		t.Fatalf("%d queries", len(fin.queries))
	}
	for i, p := range fin.principals {
		if p.TenantID != "cargo-b" || p.ActorID != "erin" || len(p.Permissions) != 1 || p.Permissions[0] != PermissionReadProfit {
			t.Errorf("query %d carried the principal %+v", i, p)
		}
		var tenant, actor string
		switch q := fin.queries[i].(type) {
		case api.ListJobProfits:
			tenant, actor = q.TenantID, q.ActorID
		case api.GetJobProfit:
			tenant, actor = q.TenantID, q.ActorID
		case api.GetJobLegalEntity:
			tenant, actor = q.TenantID, q.ActorID
		case api.ListCostReviewRequests:
			tenant, actor = q.TenantID, q.ActorID
		}
		if tenant != "cargo-b" || actor != "erin" {
			t.Errorf("query %d names %q and %q", i, tenant, actor)
		}
	}
}

// Every row of the guide's table of errors (4.5), adapted to FIN's errors in process, and an error that is none of
// them.
func TestEveryKindOfErrorIsOneFixedSentence(t *testing.T) {
	secret := errors.New(`pq: duplicate key value violates "fin_job_pkey" (SQLSTATE 23505) at 10.0.0.7:5432 token=s3cret`)
	for name, tc := range map[string]struct {
		err     error
		want    string
		outcome string
	}{
		"not found":              {api.ErrNotFound, msgNotFound, "not_found"},
		"wrapped not found":      {fmt.Errorf("%w: job JOB-1 of tenant cargo-a", api.ErrNotFound), msgNotFound, "not_found"},
		"unauthenticated":        {api.ErrUnauthenticated, msgNotAllowed, "not_allowed"},
		"unauthorized":           {fmt.Errorf("%w: no fin:ReadProfit", api.ErrUnauthorized), msgNotAllowed, "not_allowed"},
		"a rejected query":       {fmt.Errorf("%w: a page of 1 to 500", api.ErrRejected), msgInvalid, "invalid_input"},
		"a commit not confirmed": {bentoapp.ErrCommitUncertain, msgUnavailable, "unavailable"},
		"cancelled":              {context.Canceled, msgUnavailable, "unavailable"},
		"timed out":              {fmt.Errorf("read: %w", context.DeadlineExceeded), msgUnavailable, "unavailable"},
		"a network error":        {&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, msgUnavailable, "unavailable"},
		"the database refused":   {&pgconn.ConnectError{Config: &pgconn.Config{}}, msgUnavailable, "unavailable"},
		"a connection exception": {&pgconn.PgError{Code: "08006", Message: "connection failure"}, msgUnavailable, "unavailable"},
		"a server shutdown":      {&pgconn.PgError{Code: "57P01", Message: "terminating"}, msgUnavailable, "unavailable"},
		"too many connections":   {&pgconn.PgError{Code: "53300", Message: "too many"}, msgUnavailable, "unavailable"},
		"EOF":                    {fmt.Errorf("%w", io.ErrUnexpectedEOF), msgFailed, "failed"},
		"a conflict on a read":   {api.ErrConflict, msgFailed, "failed"},
		"another database error": {&pgconn.PgError{Code: "23505", Message: "duplicate key"}, msgFailed, "failed"},
		"an error nobody knows":  {secret, msgFailed, "failed"},
		"a panic-like text":      {errors.New("runtime error: index out of range [3] with length 2\ngoroutine 7 [running]:"), msgFailed, "failed"},
	} {
		fin := richFIN()
		fin.entity = func(context.Context, api.GetJobLegalEntity) (string, error) { return "", tc.err }
		r := newRig(t, fin)
		got := r.call(&alice, "get_job_profit", map[string]any{"job_id": "JOB-1"})
		if !got.isError || !strings.HasPrefix(got.text, tc.want) {
			t.Errorf("%s: %q, want %q", name, got.text, tc.want)
			continue
		}
		if tc.want == msgFailed {
			ref := strings.TrimSuffix(strings.TrimPrefix(got.text, msgFailed+" (reference "), ")")
			if len(ref) != 8 || !strings.Contains(r.logged(), "ref="+ref) {
				t.Errorf("%s: the reference %q is not in the log: %s", name, got.text, r.logged())
			}
			// the detail is in the log and only there
			detail := strings.SplitN(tc.err.Error(), "\n", 2)[0]
			if !strings.Contains(r.logged(), detail[:min(len(detail), 14)]) {
				t.Errorf("%s: the detail is not in the log", name)
			}
		} else if got.text != tc.want {
			t.Errorf("%s: %q says more than %q", name, got.text, tc.want)
		}
		for _, leak := range []string{"SQLSTATE", "10.0.0.7", "s3cret", "fin_job_pkey", "goroutine", "index out of range", "duplicate key", "JOB-1", "cargo-a", "connection"} {
			if strings.Contains(got.body, leak) {
				t.Errorf("%s: the result carries %q: %s", name, leak, got.body)
			}
		}
		if !strings.Contains(r.logged(), "outcome="+tc.outcome) {
			t.Errorf("%s: the log has no outcome %s: %s", name, tc.outcome, r.logged())
		}
	}
}

// The same mapping holds for each FIN query the tools make, not only the first.
func TestEveryQueryOfEveryToolMapsItsErrors(t *testing.T) {
	boom := errors.New("SELECT secret FROM somewhere")
	for name, tc := range map[string]struct {
		set  func(*fakeFIN)
		tool string
		args map[string]any
	}{
		"the loss page": {func(f *fakeFIN) {
			f.jobs = func(context.Context, api.ListJobProfits) (api.JobProfitPage, error) { return api.JobProfitPage{}, boom }
		}, "list_loss_jobs", map[string]any{"legal_entity_id": "LE"}},
		"the job's profit": {func(f *fakeFIN) {
			f.profit = func(context.Context, api.GetJobProfit) (api.JobProfit, error) { return api.JobProfit{}, boom }
		}, "get_job_profit", map[string]any{"job_id": "J"}},
		"the cost reviews": {func(f *fakeFIN) {
			f.requests = func(context.Context, api.ListCostReviewRequests) (api.CostReviewPage, error) {
				return api.CostReviewPage{}, boom
			}
		}, "list_job_cost_reviews", map[string]any{"job_id": "J"}},
	} {
		fin := richFIN()
		tc.set(fin)
		r := newRig(t, fin)
		got := r.call(&alice, tc.tool, tc.args)
		if !got.isError || !strings.HasPrefix(got.text, msgFailed) || strings.Contains(got.body, "SELECT") || !strings.Contains(r.logged(), "SELECT secret") {
			t.Errorf("%s: %q", name, got.text)
		}
	}
}

// A call that FIN does not finish in time is told as FIN not being available, and this server decides it itself.
func TestACallIsBoundedByTheTimeout(t *testing.T) {
	fin := richFIN()
	fin.entity = func(ctx context.Context, _ api.GetJobLegalEntity) (string, error) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(3 * time.Second): // FIN that is never told to stop
			return "LE", nil
		}
	}
	r := newRig(t, fin, func(c *Config) { c.Timeout = 50 * time.Millisecond })
	start := time.Now()
	got := r.call(&alice, "get_job_profit", map[string]any{"job_id": "JOB-1"})
	if got.text != msgUnavailable || time.Since(start) > 2*time.Second {
		t.Fatalf("%q after %s", got.text, time.Since(start))
	}
	if DefaultTimeout != 20*time.Second {
		t.Fatalf("the default timeout is %s, not 20 seconds", DefaultTimeout)
	}
}

// The principal on the context of a FIN query lives as long as the call: the deadline FIN sees is the one this server
// set.
func TestFINSeesTheDeadline(t *testing.T) {
	fin := richFIN()
	var deadline time.Time
	fin.entity = func(ctx context.Context, _ api.GetJobLegalEntity) (string, error) {
		deadline, _ = ctx.Deadline()
		return "LE", nil
	}
	r := newRig(t, fin)
	r.call(&alice, "get_job_profit", map[string]any{"job_id": "JOB-1"})
	if d := time.Until(deadline); d < 15*time.Second || d > 20*time.Second {
		t.Fatalf("FIN's deadline is %s away, want about 20 seconds", d)
	}
}

// Amounts are the integers FIN gave, never rounded through a float: above 2^53, and the extremes of an int64.
func TestAmountsRoundTripAsIntegers(t *testing.T) {
	const big = int64(9007199254740993) // 2^53 + 1
	const low = int64(-9223372036854775807)
	fin := richFIN()
	fin.jobs = func(_ context.Context, q api.ListJobProfits) (api.JobProfitPage, error) {
		row := lossJob(1)
		row.Revenue, row.Cost, row.Profit = cny(big), cny(9223372036854775807), cny(low)
		return api.JobProfitPage{LegalEntityID: q.LegalEntityID, AsOf: asOf, Rows: []api.JobProfitRow{row}}, nil
	}
	fin.profit = func(_ context.Context, q api.GetJobProfit) (api.JobProfit, error) {
		return api.JobProfit{JobID: q.JobID, Revenue: cny(big), Cost: cny(big + 2), PoolCost: cny(big + 4), Profit: cny(low), Invoiced: cny(big + 6), CostMatched: cny(big + 8),
			CostAccrued: cny(big + 10), ByCode: []api.ChargeCodeProfit{{ChargeCode: "X", Revenue: cny(big), Cost: cny(big), Profit: cny(big)}}}, nil
	}
	fin.requests = func(_ context.Context, q api.ListCostReviewRequests) (api.CostReviewPage, error) {
		return api.CostReviewPage{Requests: []api.CostReviewRequest{{RequestID: "CRR-1", State: api.ReviewOpen, Revenue: cny(big), Cost: cny(big + 1), Profit: cny(low)}}}, nil
	}
	r := newRig(t, fin)
	for tool, args := range map[string]map[string]any{"list_loss_jobs": {"legal_entity_id": "LE"}, "get_job_profit": {"job_id": "JOB-1"},
		"list_job_cost_reviews": {"job_id": "JOB-1"}} {
		got := r.call(&alice, tool, args)
		if got.isError {
			t.Fatalf("%s: %s", tool, got.text)
		}
		for _, digits := range []string{fmt.Sprint(big), fmt.Sprint(low)} {
			if !strings.Contains(got.body, `"minor":`+digits) {
				t.Errorf("%s: %s is not in the result as it is: %s", tool, digits, got.body)
			}
		}
	}
	if got := r.call(&alice, "list_loss_jobs", map[string]any{"legal_entity_id": "LE"}); !strings.Contains(got.text, "-92233720368547758.07 CNY") {
		t.Errorf("the text for a person: %q", got.text)
	}
}

func TestAnAmountForAPersonIsFormattedFromItsInteger(t *testing.T) {
	for in, want := range map[amount]string{
		{"CNY", -8001}: "-80.01 CNY", {"CNY", 10001}: "100.01 CNY", {"CNY", 5}: "0.05 CNY", {"CNY", 0}: "0.00 CNY", {"JPY", -1234}: "-1234 JPY",
		{"CNY", -9223372036854775808}: "-92233720368547758.08 CNY", {"ZZZ", 7}: "7 minor units of ZZZ",
	} {
		if got := humanAmount(in); got != want {
			t.Errorf("%+v: %q, want %q", in, got, want)
		}
	}
}

// Paging of list_loss_jobs.
func TestAnEmptyPageWithANextAfterContinuesAndFivePagesStopTheCall(t *testing.T) {
	// FIN's pages are empty (the scan window held no loss) with a next_after, the sixth has a row
	fin := richFIN()
	page := 0
	fin.jobs = func(_ context.Context, q api.ListJobProfits) (api.JobProfitPage, error) {
		page++
		p := api.JobProfitPage{LegalEntityID: q.LegalEntityID, AsOf: asOf, Rows: []api.JobProfitRow{}, NextAfter: fmt.Sprintf("JOB-%03d", page*100)}
		if page == 3 {
			p.Rows = []api.JobProfitRow{lossJob(300)}
		}
		return p, nil
	}
	r := newRig(t, fin)
	got := r.call(&alice, "list_loss_jobs", map[string]any{"legal_entity_id": "LE", "limit": 10})
	if got.isError || fin.count("ListJobProfits") != 5 {
		t.Fatalf("%d pages of FIN: %s", fin.count("ListJobProfits"), got.body)
	}
	if rows, _ := got.data["rows"].([]any); len(rows) != 1 || got.data["truncated"] != true || got.data["next_cursor"] == nil || got.data["row_cap_reached"] != false {
		t.Fatalf("the five pages stopped the call, with one row, a cursor and truncated: %v", got.data)
	}
	// FIN is asked for the rows still missing, from where the last page stopped
	fin.mu.Lock()
	limits, afters := []int{}, []string{}
	for _, q := range fin.queries {
		if q, ok := q.(api.ListJobProfits); ok {
			limits, afters = append(limits, q.Limit), append(afters, q.After)
			if !q.LossOnly || q.LegalEntityID != "LE" {
				t.Errorf("a query for %+v", q)
			}
		}
	}
	fin.mu.Unlock()
	wantLimits, wantAfters := []int{10, 10, 10, 9, 9}, []string{"", "JOB-100", "JOB-200", "JOB-300", "JOB-400"}
	if fmt.Sprint(limits) != fmt.Sprint(wantLimits) || fmt.Sprint(afters) != fmt.Sprint(wantAfters) {
		t.Fatalf("FIN was asked %v after %v, want %v after %v", limits, afters, wantLimits, wantAfters)
	}
	// the cursor goes on from the fifth page's next_after
	cursor := got.data["next_cursor"].(string)
	page = 100 // the next pages end the chain
	fin.jobs = func(_ context.Context, q api.ListJobProfits) (api.JobProfitPage, error) {
		if q.After != "JOB-500" {
			t.Errorf("the cursor continues after %q, not JOB-500", q.After)
		}
		return api.JobProfitPage{LegalEntityID: "LE", AsOf: asOf, Rows: []api.JobProfitRow{lossJob(501)}}, nil
	}
	more := r.call(&alice, "list_loss_jobs", map[string]any{"legal_entity_id": "LE", "limit": 10, "cursor": cursor})
	if more.isError || more.data["next_cursor"] != nil || more.data["truncated"] != false || more.data["row_cap_reached"] != false {
		t.Fatalf("the end of the chain: %s", more.body)
	}
}

func TestTheRowCapStopsAChainOfCursorsAt200(t *testing.T) {
	fin := richFIN()
	fin.jobs = func(_ context.Context, q api.ListJobProfits) (api.JobProfitPage, error) {
		p := api.JobProfitPage{LegalEntityID: q.LegalEntityID, AsOf: asOf, Rows: []api.JobProfitRow{}}
		start := 0
		if q.After != "" {
			fmt.Sscanf(q.After, "JOB-%d", &start)
		}
		for i := 1; i <= q.Limit; i++ {
			p.Rows = append(p.Rows, lossJob(start+i))
		}
		p.NextAfter = fmt.Sprintf("JOB-%03d", start+q.Limit) // there is always more
		return p, nil
	}
	r := newRig(t, fin)
	cursor, total := "", 0
	for call := 1; call <= 6; call++ {
		args := map[string]any{"legal_entity_id": "LE", "limit": 50}
		if cursor != "" {
			args["cursor"] = cursor
		}
		got := r.call(&alice, "list_loss_jobs", args)
		if got.isError {
			t.Fatalf("call %d: %s", call, got.text)
		}
		rows, _ := got.data["rows"].([]any)
		total += len(rows)
		cursor, _ = got.data["next_cursor"].(string)
		if call < 4 && (cursor == "" || got.data["row_cap_reached"] != false) {
			t.Fatalf("call %d: %v", call, got.data)
		}
		if call == 4 {
			if cursor != "" || got.data["row_cap_reached"] != true || total != 200 || !strings.Contains(got.text, "200 rows") {
				t.Fatalf("the fourth call ends the chain at %d rows: %v %q", total, got.data, got.text)
			}
			break
		}
	}
	// a smaller limit that does not divide 200: the last call asks for what is left
	fin2 := &fakeFIN{jobs: fin.jobs}
	r2 := newRig(t, fin2)
	cursor, total = "", 0
	for call := 0; call < 10 && total < 200; call++ {
		args := map[string]any{"legal_entity_id": "LE", "limit": 30}
		if cursor != "" {
			args["cursor"] = cursor
		}
		got := r2.call(&alice, "list_loss_jobs", args)
		rows, _ := got.data["rows"].([]any)
		total += len(rows)
		cursor, _ = got.data["next_cursor"].(string)
		if total == 200 && (cursor != "" || got.data["row_cap_reached"] != true) {
			t.Fatalf("at 200 rows: %v", got.data)
		}
	}
	if total != 200 {
		t.Fatalf("%d rows", total)
	}
}

// A chain that ends because FIN has no more is not a chain that reached the cap, even at exactly 200 rows.
func TestTheEndOfFINsRowsIsNotTheCap(t *testing.T) {
	fin := richFIN()
	fin.jobs = func(_ context.Context, q api.ListJobProfits) (api.JobProfitPage, error) {
		return api.JobProfitPage{LegalEntityID: "LE", AsOf: asOf, Rows: []api.JobProfitRow{lossJob(1)}}, nil
	}
	r := newRig(t, fin)
	got := r.call(&alice, "list_loss_jobs", map[string]any{"legal_entity_id": "LE"})
	if got.data["next_cursor"] != nil || got.data["row_cap_reached"] != false || got.data["truncated"] != false {
		t.Fatalf("%v", got.data)
	}
}

func TestACursorIsBoundToItsCallerRequestAndTime(t *testing.T) {
	fin := richFIN()
	fin.jobs = func(_ context.Context, q api.ListJobProfits) (api.JobProfitPage, error) {
		return api.JobProfitPage{LegalEntityID: q.LegalEntityID, AsOf: asOf, Rows: []api.JobProfitRow{lossJob(1)}, NextAfter: "JOB-001"}, nil
	}
	fin.requests = func(_ context.Context, q api.ListCostReviewRequests) (api.CostReviewPage, error) {
		return api.CostReviewPage{Requests: []api.CostReviewRequest{{RequestID: "CRR-1", State: api.ReviewOpen}}, NextAfter: "CRR-1"}, nil
	}
	r := newRig(t, fin)
	first := r.call(&alice, "list_loss_jobs", map[string]any{"legal_entity_id": "LE", "limit": 1})
	cursor, _ := first.data["next_cursor"].(string)
	reviews := r.call(&alice, "list_job_cost_reviews", map[string]any{"job_id": "JOB-1", "state": "OPEN", "limit": 1})
	reviewCursor, _ := reviews.data["next_cursor"].(string)
	if cursor == "" || reviewCursor == "" {
		t.Fatalf("no cursors: %v %v", first.data, reviews.data)
	}
	if got := r.call(&alice, "list_loss_jobs", map[string]any{"legal_entity_id": "LE", "cursor": cursor}); got.isError {
		t.Fatalf("the caller's own cursor: %q", got.text)
	}
	before := fin.count("ListJobProfits")
	flip := func(s string, at int) string {
		b := []byte(s)
		if b[at] == 'A' {
			b[at] = 'B'
		} else {
			b[at] = 'A'
		}
		return string(b)
	}
	dot := strings.Index(cursor, ".")
	wide := &who{"gw", "alice", "wide"}
	// a job and a legal entity may have the same id: the cursor of the reviews of job "LE" is not one of the losses of
	// "LE"
	same := r.call(&alice, "list_job_cost_reviews", map[string]any{"job_id": "LE", "limit": 1})
	sameCursor, _ := same.data["next_cursor"].(string)
	if sameCursor == "" {
		t.Fatalf("no cursor for job LE: %s", same.body)
	}
	if got := r.call(&alice, "list_loss_jobs", map[string]any{"legal_entity_id": "LE", "cursor": sameCursor}); got.text != msgBadCursor {
		t.Errorf("the reviews' cursor used for the losses: %q", got.text)
	}
	for name, tc := range map[string]struct {
		c       *who
		tool    string
		args    map[string]any
		advance time.Duration
	}{
		"another member's":                     {&bob, "list_loss_jobs", map[string]any{"legal_entity_id": "LE", "cursor": cursor}, 0},
		"another tenant's member of that name": {&who{"gw-b", "alice", "analyst"}, "list_loss_jobs", map[string]any{"legal_entity_id": "LE", "cursor": cursor}, 0},
		"for another legal entity":             {wide, "list_loss_jobs", map[string]any{"legal_entity_id": "LE2", "cursor": cursor}, 0},
		"a changed body":                       {&alice, "list_loss_jobs", map[string]any{"legal_entity_id": "LE", "cursor": flip(cursor, 3)}, 0},
		"a changed signature":                  {&alice, "list_loss_jobs", map[string]any{"legal_entity_id": "LE", "cursor": flip(cursor, dot+3)}, 0},
		"a cursor of another tool":             {&alice, "list_loss_jobs", map[string]any{"legal_entity_id": "LE", "cursor": reviewCursor}, 0},
		"a signature of nothing":               {&alice, "list_loss_jobs", map[string]any{"legal_entity_id": "LE", "cursor": cursor[:dot] + ".AAAA"}, 0},
		"a body without its signature":         {&alice, "list_loss_jobs", map[string]any{"legal_entity_id": "LE", "cursor": cursor[:dot] + "."}, 0},
		"an expired one":                       {&alice, "list_loss_jobs", map[string]any{"legal_entity_id": "LE", "cursor": cursor}, 15*time.Minute + time.Second},
	} {
		r.advance(tc.advance)
		got := r.call(tc.c, tc.tool, tc.args)
		want := msgBadCursor
		if got.isError && got.text == msgInvalid { // a cursor the schema's pattern does not take is refused earlier
			want = msgInvalid
		}
		if !got.isError || got.text != want {
			t.Errorf("%s: %q", name, got.text)
		}
		r.advance(-tc.advance)
	}
	if fin.count("ListJobProfits") != before {
		t.Fatalf("FIN was asked for %d pages on cursors that are not good", fin.count("ListJobProfits")-before)
	}
	// the reviews' cursor is bound to the job and the state it was given for
	for name, args := range map[string]map[string]any{
		"another job":     {"job_id": "JOB-2", "state": "OPEN", "cursor": reviewCursor},
		"another state":   {"job_id": "JOB-1", "state": "RESOLVED", "cursor": reviewCursor},
		"no state":        {"job_id": "JOB-1", "cursor": reviewCursor},
		"the loss cursor": {"job_id": "JOB-1", "state": "OPEN", "cursor": cursor},
	} {
		if got := r.call(&alice, "list_job_cost_reviews", args); got.text != msgBadCursor {
			t.Errorf("%s: %q", name, got.text)
		}
	}
	if got := r.call(&alice, "list_job_cost_reviews", map[string]any{"job_id": "JOB-1", "state": "OPEN", "cursor": reviewCursor}); got.isError {
		t.Errorf("the reviews' own cursor: %q", got.text)
	}
	// still good just before it expires
	r.advance(14 * time.Minute)
	if got := r.call(&alice, "list_loss_jobs", map[string]any{"legal_entity_id": "LE", "cursor": cursor}); got.isError {
		t.Errorf("a cursor of 14 minutes: %q", got.text)
	}
}

// Calls per minute and calls at once, per caller, in memory.
func TestRateAndConcurrencyLimitsPerCaller(t *testing.T) {
	fin := richFIN()
	r := newRig(t, fin, func(c *Config) { c.Limits = Limits{PerMinute: 3, Concurrent: 2} })
	args := map[string]any{"job_id": "JOB-1"}
	for i := 0; i < 3; i++ {
		if got := r.call(&alice, "get_job_profit", args); got.isError {
			t.Fatalf("call %d: %q", i, got.text)
		}
	}
	if got := r.call(&alice, "get_job_profit", args); !got.isError || got.text != msgTooMany {
		t.Fatalf("the fourth call in a minute: %q", got.text)
	}
	if got := r.call(&bob, "get_job_profit", args); got.isError {
		t.Fatalf("another caller is not limited by alice's calls: %q", got.text)
	}
	r.advance(61 * time.Second)
	if got := r.call(&alice, "get_job_profit", args); got.isError {
		t.Fatalf("after a minute: %q", got.text)
	}

	// at once: two calls wait in FIN, the third is refused, a caller of another name is not
	release := make(chan struct{})
	var started sync.WaitGroup
	started.Add(2)
	var once sync.Mutex
	n := 0
	fin.profit = func(ctx context.Context, q api.GetJobProfit) (api.JobProfit, error) {
		once.Lock()
		n++
		first := n <= 2
		once.Unlock()
		if first {
			started.Done()
			<-release
		}
		return api.JobProfit{JobID: q.JobID}, nil
	}
	r2 := newRig(t, fin, func(c *Config) { c.Limits = Limits{PerMinute: 100, Concurrent: 2} })
	var done sync.WaitGroup
	for i := 0; i < 2; i++ {
		done.Add(1)
		go func() { defer done.Done(); r2.call(&alice, "get_job_profit", args) }()
	}
	started.Wait()
	if got := r2.call(&alice, "get_job_profit", args); !got.isError || got.text != msgTooMany {
		t.Errorf("the third call at once: %q", got.text)
	}
	if got := r2.call(&bob, "get_job_profit", args); got.isError {
		t.Errorf("another caller at once: %q", got.text)
	}
	close(release)
	done.Wait()
	if got := r2.call(&alice, "get_job_profit", args); got.isError {
		t.Errorf("after the calls ended: %q", got.text)
	}
}

// One structured line per call, with the tool, the outcome, the time and a short hash of the caller; nothing of what
// was asked and no amount.
func TestOneLogLinePerCallWithNoArgumentsAndNoAmounts(t *testing.T) {
	fin := richFIN()
	fin.entity = func(context.Context, api.GetJobLegalEntity) (string, error) { return "LE2", nil }
	r := newRig(t, fin)
	r.call(&alice, "get_job_profit", map[string]any{"job_id": "JOB-SECRET-ID"})
	r.call(&alice, "list_loss_jobs", map[string]any{"legal_entity_id": "LE-SECRET"})
	r.call(&alice, "list_loss_jobs", map[string]any{"legal_entity_id": "LE"})
	r.call(&alice, "list_job_cost_reviews", map[string]any{"job_id": "JOB-1", "tenant_id": "x"})
	r.call(&who{"gw", "mallory", "unknown"}, "get_job_profit", map[string]any{"job_id": "JOB-1"})
	lines := []string{}
	for _, l := range strings.Split(strings.TrimSpace(r.logged()), "\n") {
		if strings.Contains(l, "fin mcp call ") || strings.Contains(l, "msg=\"fin mcp call\"") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 5 {
		t.Fatalf("%d lines for 5 calls:\n%s", len(lines), r.logged())
	}
	sum := sha256.Sum256([]byte("gw\x00alice"))
	short := hex.EncodeToString(sum[:4])
	for i, want := range []struct{ tool, outcome string }{{"get_job_profit", "hidden"}, {"list_loss_jobs", "not_allowed"}, {"list_loss_jobs", "ok"},
		{"list_job_cost_reviews", "invalid_input"}, {"get_job_profit", "not_allowed"}} {
		l := lines[i]
		for _, part := range []string{"tool=" + want.tool, "outcome=" + want.outcome, "duration_ms=", "caller="} {
			if !strings.Contains(l, part) {
				t.Errorf("line %d lacks %s: %s", i, part, l)
			}
		}
		allowed := map[string]bool{"time": true, "level": true, "msg": true, "tool": true, "outcome": true, "duration_ms": true, "caller": true, "client": true, "actor": true, "cause": true}
		for _, field := range logFields(l) {
			if !allowed[field] {
				t.Errorf("line %d has the attribute %q, which is not one of the tool, the outcome, the time, the caller's hash, the client and the actor: %s", i, field, l)
			}
		}
		if i < 4 && !regexp.MustCompile(`caller=`+short+`( |$)`).MatchString(l) {
			t.Errorf("line %d: the caller is not %s: %s", i, short, l)
		}
		if !strings.Contains(l, "client=idp-host") || !strings.Contains(l, "actor=idp-host") {
			t.Errorf("line %d: the assertion's client and actor are not in it: %s", i, l)
		}
	}
	for _, leak := range []string{"SECRET", "alice", "JOB-1", "-8001", "20001", "12000", "minor"} {
		if strings.Contains(r.logged(), leak) {
			t.Errorf("the log carries %q:\n%s", leak, r.logged())
		}
	}
}

// logFields are the keys of a line of the text handler: key=value pairs, a value that holds blanks in quotes.
func logFields(line string) []string {
	var keys []string
	for _, m := range regexp.MustCompile(`(?:^| )([a-z_]+)=("(?:[^"\\]|\\.)*"|\S*)`).FindAllStringSubmatch(line, -1) {
		keys = append(keys, m[1])
	}
	return keys
}

// A handler that panics is told as a failure with a reference, not as a dead connection.
func TestAPanicInAToolIsAFailureWithAReference(t *testing.T) {
	fin := richFIN()
	fin.entity = func(context.Context, api.GetJobLegalEntity) (string, error) {
		panic("index out of range at /srv/fin/secret.go:12")
	}
	r := newRig(t, fin)
	got := r.call(&alice, "get_job_profit", map[string]any{"job_id": "JOB-1"})
	if !got.isError || !strings.HasPrefix(got.text, msgFailed+" (reference ") || strings.Contains(got.body, "secret.go") || !strings.Contains(r.logged(), "secret.go") {
		t.Fatalf("%q", got.text)
	}
}

func TestTheConfigurationIsCheckedAtRegistration(t *testing.T) {
	ok := Config{Caller: func(context.Context) (Subject, bool) { return Subject{}, false }, Policy: testPolicy(), CursorKey: make([]byte, 32)}
	for name, mutate := range map[string]func(*Config){
		"no caller":        func(c *Config) { c.Caller = nil },
		"a short key":      func(c *Config) { c.CursorKey = make([]byte, 31) },
		"no policy":        func(c *Config) { c.Policy = Policy{} },
		"a negative rate":  func(c *Config) { c.Limits.PerMinute = -1 },
		"a negative limit": func(c *Config) { c.Limits.Concurrent = -1 },
		"a write permission": func(c *Config) {
			p := testPolicy()
			p.Tenants["gw"].Roles["analyst"] = RolePolicy{Permissions: []string{"fin:RequestCostReview"}, LegalEntities: Entities{All: true}}
			c.Policy = p
		},
	} {
		c := ok
		mutate(&c)
		if err := Register(newServer(), &fakeFIN{}, c); err == nil {
			t.Errorf("%s: registered", name)
		}
	}
	if err := Register(newServer(), nil, ok); err == nil {
		t.Error("registered without FIN")
	}
	if err := Register(newServer(), &fakeFIN{}, ok); err != nil {
		t.Fatal(err)
	}
}

// Every result, a refusal as much as an answer, is written as the SDK writes a result of the protocol of 2026-07-28,
// with its result type: a result the middleware made itself would lack it.
func TestEveryResultCarriesTheResultTypeOfTheProtocol(t *testing.T) {
	fin := richFIN()
	r := newRig(t, fin, func(c *Config) { c.Limits = Limits{PerMinute: 5, Concurrent: 4} })
	typeOf := func(got reply) any {
		res, _ := got.rpc["result"].(map[string]any)
		return res["resultType"]
	}
	for name, got := range map[string]reply{
		"an answer":                        r.call(&alice, "get_job_profit", map[string]any{"job_id": "JOB-1"}),
		"no caller":                        r.call(nil, "get_job_profit", map[string]any{"job_id": "JOB-1"}),
		"a role not named":                 r.call(&who{"gw", "x", "nobody"}, "get_job_profit", map[string]any{"job_id": "JOB-1"}),
		"invalid input":                    r.call(&alice, "get_job_profit", map[string]any{"job_id": 1}),
		"invalid input of one not allowed": r.call(&who{"gw", "x", "nobody"}, "get_job_profit", map[string]any{"job_id": 1}),
		"a legal entity not allowed":       r.call(&alice, "list_loss_jobs", map[string]any{"legal_entity_id": "LE2"}),
	} {
		if typeOf(got) != "complete" {
			t.Errorf("%s: no result type: %s", name, got.body)
		}
	}
	for i := 0; i < 5; i++ {
		r.call(&bob, "get_job_profit", map[string]any{"job_id": "JOB-1"})
	}
	if got := r.call(&bob, "get_job_profit", map[string]any{"job_id": "JOB-1"}); got.text != msgTooMany || typeOf(got) != "complete" {
		t.Errorf("too many calls: %s", got.body)
	}
	// a caller who is refused reads the refusal, not what the SDK thought of the arguments
	if got := r.call(&who{"gw", "x", "nobody"}, "get_job_profit", map[string]any{"job_id": 1}); got.text != msgNotAllowed {
		t.Errorf("a refused caller with invalid input reads %q", got.text)
	}
}

// The handlers do not lean on the SDK's check of the arguments alone: given what the schema would have refused, each
// refuses it itself, before FIN is asked.
func TestTheHandlersCheckTheirArgumentsThemselves(t *testing.T) {
	fin := richFIN()
	r := newRig(t, fin)
	v := &server{fin: fin, policy: r.policyForTest(), cursors: cursors{key: bytes.Repeat([]byte("k"), 32), now: time.Now}, log: slog.Default(), now: time.Now}
	ctx := context.WithValue(context.Background(), accessKey{}, access{tenant: "cargo-a", actor: "alice", all: true, permissions: []string{PermissionReadProfit}})
	for _, bad := range []string{"", "A B", "LE\n", "-LE", strings.Repeat("A", 129)} {
		for name, call := range map[string]func() error{
			"get_job_profit":        func() error { _, _, err := v.getJobProfit(ctx, nil, profitIn{JobID: bad}); return err },
			"list_loss_jobs":        func() error { _, _, err := v.listLossJobs(ctx, nil, listLossIn{LegalEntityID: bad}); return err },
			"list_job_cost_reviews": func() error { _, _, err := v.listJobCostReviews(ctx, nil, reviewsIn{JobID: bad}); return err },
		} {
			var f *failure
			if err := call(); !errors.As(err, &f) || f.outcome != "invalid_input" {
				t.Errorf("%s with %q: %v", name, bad, err)
			}
		}
	}
	var f *failure
	if _, _, err := v.listJobCostReviews(ctx, nil, reviewsIn{JobID: "JOB-1", State: "DONE"}); !errors.As(err, &f) || f.outcome != "invalid_input" {
		t.Errorf("a state that is none: %v", err)
	}
	// and without the guard's access nothing is read
	if _, _, err := v.getJobProfit(context.Background(), nil, profitIn{JobID: "JOB-1"}); !errors.As(err, &f) || f.outcome != "not_allowed" {
		t.Errorf("a handler reached without a caller: %v", err)
	}
	if fin.total() != 0 {
		t.Fatalf("FIN was asked %d times", fin.total())
	}
}
