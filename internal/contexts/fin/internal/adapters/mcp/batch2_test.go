package mcp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
)

// The audit line tells a hidden job from a missing one, though the answer is the same bytes.
func TestTheLogTellsAHiddenJobFromAMissingOne(t *testing.T) {
	fin := richFIN()
	fin.entity = func(_ context.Context, q api.GetJobLegalEntity) (string, error) {
		if q.JobID == "JOB-MISSING" {
			return "", api.ErrNotFound
		}
		return "LE2", nil
	}
	r := newRig(t, fin)
	hidden := r.call(&alice, "get_job_profit", map[string]any{"job_id": "JOB-HIDDEN"})
	missing := r.call(&alice, "get_job_profit", map[string]any{"job_id": "JOB-MISSING"})
	cut := func(s string) string { return s[strings.Index(s, `"result"`):] }
	if hidden.text != missing.text || cut(hidden.body) != cut(missing.body) {
		t.Fatalf("the answers differ: %q %q", hidden.body, missing.body)
	}
	log := r.logged()
	if strings.Count(log, "outcome=hidden") != 1 || strings.Count(log, "outcome=not_found") != 1 {
		t.Fatalf("the log:\n%s", log)
	}
}

// The log line of a refused subject names the cause, and only the cause.
func TestTheLogNamesWhyACallerWasRefused(t *testing.T) {
	r := newRig(t, richFIN())
	for _, c := range []struct {
		who  who
		want string
	}{
		{who{"nowhere", "alice", "analyst"}, "not in the policy"},
		{who{"gw", "alice+x", "analyst"}, "not of the form"},
		{who{"gw", "alice", "nobody"}, "no role of the member grants"},
	} {
		if got := r.call(&c.who, "get_job_profit", map[string]any{"job_id": "JOB-1"}); got.text != msgNotAllowed {
			t.Fatalf("%q", got.text)
		}
		if !strings.Contains(r.logged(), c.want) {
			t.Errorf("the log does not say %q:\n%s", c.want, r.logged())
		}
	}
	if strings.Contains(r.logged(), "JOB-1") {
		t.Error("an argument is in the log")
	}
}

// truncated is told only when the five pages stopped the call: a call that has its rows, with more to read, is not.
func TestTruncatedIsFalseOnAFullPageWithMoreToRead(t *testing.T) {
	fin := richFIN()
	fin.jobs = func(_ context.Context, q api.ListJobProfits) (api.JobProfitPage, error) {
		rows := []api.JobProfitRow{}
		for i := 0; i < q.Limit; i++ {
			rows = append(rows, lossJob(i+1))
		}
		return api.JobProfitPage{LegalEntityID: "LE", AsOf: asOf, Rows: rows, NextAfter: "JOB-X"}, nil
	}
	r := newRig(t, fin)
	got := r.call(&alice, "list_loss_jobs", map[string]any{"legal_entity_id": "LE", "limit": 3})
	if got.data["truncated"] != false || got.data["next_cursor"] == nil {
		t.Fatalf("%v", got.data)
	}
}

// list_job_cost_reviews has the 200-row cap, and asks FIN only for the rows left.
func TestTheReviewsChainHasTheRowCapAndAsksForWhatIsLeft(t *testing.T) {
	var mu sync.Mutex
	var asked []int
	fin := richFIN()
	fin.requests = func(_ context.Context, q api.ListCostReviewRequests) (api.CostReviewPage, error) {
		mu.Lock()
		asked = append(asked, q.Limit)
		mu.Unlock()
		p := api.CostReviewPage{Requests: []api.CostReviewRequest{}, NextAfter: "CRR-9"}
		for i := 0; i < q.Limit; i++ {
			p.Requests = append(p.Requests, api.CostReviewRequest{RequestID: fmt.Sprintf("CRR-%d", i+1), State: api.ReviewOpen})
		}
		return p, nil
	}
	r := newRig(t, fin)
	cursor, total, last := "", 0, reply{}
	for call := 0; call < 10; call++ {
		args := map[string]any{"job_id": "JOB-1", "limit": 30}
		if cursor != "" {
			args["cursor"] = cursor
		}
		last = r.call(&alice, "list_job_cost_reviews", args)
		if last.isError {
			t.Fatal(last.text)
		}
		total += len(last.data["requests"].([]any))
		if cursor, _ = last.data["next_cursor"].(string); cursor == "" {
			break
		}
	}
	if total != 200 || last.data["row_cap_reached"] != true || fmt.Sprint(asked) != "[30 30 30 30 30 30 20]" {
		t.Fatalf("%d rows, cap %v, FIN asked for %v", total, last.data["row_cap_reached"], asked)
	}
}

// With no limits in the configuration a caller has 60 calls a minute and 4 at once.
func TestTheDefaultLimitsAre60AMinuteAnd4AtOnce(t *testing.T) {
	if DefaultPerMinute != 60 || DefaultConcurrent != 4 {
		t.Fatalf("%d a minute, %d at once", DefaultPerMinute, DefaultConcurrent)
	}
	fin := richFIN()
	r := newRig(t, fin, func(c *Config) { c.Limits = Limits{} })
	for i := 1; i <= 60; i++ {
		if got := r.call(&alice, "get_job_profit", map[string]any{"job_id": "JOB-1"}); got.isError {
			t.Fatalf("call %d: %s", i, got.text)
		}
	}
	if got := r.call(&alice, "get_job_profit", map[string]any{"job_id": "JOB-1"}); got.text != msgTooMany {
		t.Fatalf("the 61st call: %q", got.text)
	}
	release := make(chan struct{})
	var started sync.WaitGroup
	started.Add(4)
	var mu sync.Mutex
	n := 0
	fin2 := richFIN()
	fin2.profit = func(_ context.Context, q api.GetJobProfit) (api.JobProfit, error) {
		mu.Lock()
		n++
		first := n <= 4
		mu.Unlock()
		if first {
			started.Done()
			<-release
		}
		return api.JobProfit{JobID: q.JobID}, nil
	}
	r2 := newRig(t, fin2, func(c *Config) { c.Limits = Limits{} })
	var done sync.WaitGroup
	for i := 0; i < 4; i++ {
		done.Add(1)
		go func() { defer done.Done(); r2.call(&alice, "get_job_profit", map[string]any{"job_id": "JOB-1"}) }()
	}
	started.Wait()
	if got := r2.call(&alice, "get_job_profit", map[string]any{"job_id": "JOB-1"}); got.text != msgTooMany {
		t.Errorf("the fifth call at once: %q", got.text)
	}
	close(release)
	done.Wait()
}

// The cursor's MAC has its own domain: a cursor signed over the same parts without the prefix is refused.
func TestACursorSignedWithoutTheDomainPrefixIsRefused(t *testing.T) {
	c := cursors{key: []byte(strings.Repeat("k", 32)), now: time.Now}
	a := access{tenant: "cargo-a", actor: "alice"}
	body := cursorBody{Tool: toolListLossJobs, After: "JOB-1", Rows: 1, Scope: "LE", Expires: time.Now().Add(time.Minute).Unix()}
	raw, _ := json.Marshal(body)
	encoded := b64.EncodeToString(raw)
	forged := func(prefix string) string {
		h := hmac.New(sha256.New, c.key)
		h.Write([]byte(prefix + a.tenant + "\x00" + a.actor + "\x00" + encoded))
		return encoded + "." + b64.EncodeToString(h.Sum(nil))
	}
	want := cursorBody{Tool: toolListLossJobs, Scope: "LE"}
	if _, err := c.open(a, forged(""), want); err == nil {
		t.Error("a cursor without the domain prefix was taken")
	}
	if _, err := c.open(a, forged("idp-cargo fin-mcp cursor v1\x00"), want); err != nil {
		t.Errorf("the right prefix is refused: %v", err)
	}
}
