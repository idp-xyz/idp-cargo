// Package mcp is FIN's MCP adapter (V2.101): three read-only tools behind the IDP TELOS gateway (list_loss_jobs,
// get_job_profit, list_job_cost_reviews). It turns a call into FIN's api queries in process, with the tenant and the
// actor of the member the gateway's signed assertion names, and holds no business rules: FIN authorizes each query as
// it always does, from the principal this adapter puts on the context. The adapter adds what FIN does not have:
// CargoPai's policy of which gateway tenants and roles may read which legal entities (default deny), cursors bound to
// the caller, limits, fixed error sentences and one audit line per call.
package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	bentoapp "go.idp.xyz/idp-bento-go/application"

	"github.com/jackc/pgx/v5/pgconn"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	"go.idp.xyz/idp-cargo/internal/foundation/identity"
)

// Service is what the tools read of FIN: its queries.
type Service interface {
	ListJobProfits(ctx context.Context, q api.ListJobProfits) (api.JobProfitPage, error)
	GetJobProfit(ctx context.Context, q api.GetJobProfit) (api.JobProfit, error)
	GetJobLegalEntity(ctx context.Context, q api.GetJobLegalEntity) (string, error)
	ListCostReviewRequests(ctx context.Context, q api.ListCostReviewRequests) (api.CostReviewPage, error)
}

// Config is what the tools need besides FIN.
type Config struct {
	// Caller is the verified caller of the request, taken from the gateway's assertion and from nothing else.
	Caller func(ctx context.Context) (Subject, bool)
	Policy Policy
	// CursorKey is the HMAC key of the cursors: 32 bytes or more.
	CursorKey []byte
	// Limits per caller; zero values are the defaults (60 calls a minute, 4 at once).
	Limits Limits
	// Timeout is how long one call may take, FIN's queries included; 20 seconds when zero.
	Timeout time.Duration
	Now     func() time.Time
	Log     *slog.Logger
}

// DefaultTimeout is how long a call may take: well under the gateway's 60 seconds, so that this server decides the
// outcome itself.
const DefaultTimeout = 20 * time.Second

// The sentences a caller reads. Each names its way out; none carries FIN's text, a statement, a stack or an address.
const (
	msgNotFound    = "no such job or request"
	msgNotAllowed  = "you are not allowed to do this in FIN"
	msgInvalid     = "the input is not valid; check the arguments against the tool's schema and call again"
	msgUnavailable = "FIN is not available now; try again later"
	msgTooMany     = "too many requests; try again in a minute"
	msgBadCursor   = "the cursor is not valid for this request, or it has expired; start again without a cursor"
	msgFailed      = "the FIN tool failed to complete the request"
)

// failure is a tool error with its audit outcome. It is what a handler returns for every refusal.
type failure struct{ outcome, msg string }

func (f *failure) Error() string { return f.msg }

type server struct {
	fin     Service
	caller  func(context.Context) (Subject, bool)
	policy  Policy
	cursors cursors
	limiter *limiter
	timeout time.Duration
	now     func() time.Time
	log     *slog.Logger
}

type accessKey struct{}

// Register adds the three tools to s, and the middleware that identifies the caller, limits it, bounds the call by the
// timeout, puts the principal FIN authorizes on the context, turns every error into a fixed sentence and logs the call.
func Register(s *sdk.Server, fin Service, cfg Config) error {
	switch {
	case fin == nil || cfg.Caller == nil:
		return errors.New("fin mcp: the tools need FIN and a source of the verified caller")
	case len(cfg.CursorKey) < minCursorKey:
		return fmt.Errorf("fin mcp: the cursor key is %d bytes or more", minCursorKey)
	}
	if err := cfg.Policy.Validate(); err != nil {
		return fmt.Errorf("fin mcp: %w", err)
	}
	if cfg.Limits.PerMinute == 0 {
		cfg.Limits.PerMinute = DefaultPerMinute
	}
	if cfg.Limits.Concurrent == 0 {
		cfg.Limits.Concurrent = DefaultConcurrent
	}
	if cfg.Limits.PerMinute < 1 || cfg.Limits.Concurrent < 1 {
		return errors.New("fin mcp: the limits are 1 or more")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	v := &server{fin: fin, caller: cfg.Caller, policy: cfg.Policy, cursors: cursors{key: cfg.CursorKey, now: cfg.Now},
		limiter: newLimiter(cfg.Limits, cfg.Now), timeout: cfg.Timeout, now: cfg.Now, log: cfg.Log}
	tools := declarations()
	sdk.AddTool(s, tools[0], safely(v, toolListLossJobs, v.listLossJobs))
	sdk.AddTool(s, tools[1], safely(v, toolGetJobProfit, v.getJobProfit))
	sdk.AddTool(s, tools[2], safely(v, toolListJobCostReview, v.listJobCostReviews))
	s.AddReceivingMiddleware(v.guard)
	return nil
}

// caller key and hash: tenant and subject, the first 4 bytes of the SHA-256 of them in hex for the log.
func callerKey(s Subject) string { return s.Tenant + "\x00" + s.ID }

func callerHash(s Subject) string {
	sum := sha256.Sum256([]byte(callerKey(s)))
	return hex.EncodeToString(sum[:4])
}

// refusalKey carries, on the context, the refusal the guard has decided: the handler returns it, so that the SDK writes
// the result as it writes every other (with the result type of the protocol), and no tool reaches FIN.
type refusalKey struct{}

// settle makes res the tool error f: one fixed sentence, whatever the SDK or the handler had put in it.
func settle(res *sdk.CallToolResult, f *failure) {
	res.Content, res.StructuredContent = nil, nil
	res.SetError(f)
}

// guard wraps every tools/call: it fixes who calls (default deny), takes the caller's turn, bounds the call, runs it
// and writes the one audit line, which holds the tool, the outcome, the time it took and a short hash of the caller: no
// argument and no amount.
func (v *server) guard(next sdk.MethodHandler) sdk.MethodHandler {
	return func(ctx context.Context, method string, req sdk.Request) (result sdk.Result, err error) {
		if method != "tools/call" {
			return next(ctx, method, req)
		}
		start := v.now()
		tool, who, outcome := "unknown", "-", "ok"
		attrs := []any{}
		if p, ok := req.GetParams().(*sdk.CallToolParamsRaw); ok {
			switch p.Name {
			case toolListLossJobs, toolGetJobProfit, toolListJobCostReview:
				tool = p.Name
			}
		}
		defer func() {
			v.log.Info("fin mcp call", append([]any{"tool", tool, "outcome", outcome, "duration_ms", v.now().Sub(start).Milliseconds(), "caller", who},
				attrs...)...)
		}()
		var refusal *failure
		subject, ok := v.caller(ctx)
		if !ok {
			refusal = &failure{"not_allowed", msgNotAllowed}
		} else {
			who = callerHash(subject)
			if subject.Client != "" {
				attrs = append(attrs, "client", subject.Client)
			}
			if subject.Actor != "" {
				attrs = append(attrs, "actor", subject.Actor)
			}
			if done, ok := v.limiter.take(callerKey(subject)); !ok {
				refusal = &failure{"rate_limited", msgTooMany}
			} else {
				defer done()
				if a, denied := v.policy.resolve(subject); denied != nil {
					refusal = &failure{"not_allowed", msgNotAllowed}
					attrs = append(attrs, "cause", strings.TrimPrefix(denied.Error(), errDenied.Error()+": "))
				} else {
					ctx = context.WithValue(ctx, accessKey{}, a)
					ctx = identity.WithPrincipal(ctx, identity.Principal{TenantID: a.tenant, ActorID: a.actor, Permissions: a.permissions,
						ExpiresAt: start.Add(v.timeout)})
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, v.timeout)
					defer cancel()
				}
			}
		}
		if refusal != nil {
			ctx = context.WithValue(ctx, refusalKey{}, refusal)
		}
		result, err = next(ctx, method, req)
		if err != nil {
			outcome = "protocol_error"
			return result, err
		}
		res, ok := result.(*sdk.CallToolResult)
		switch {
		case !ok:
		case refusal != nil:
			// whatever the SDK made of the arguments, the caller who is refused reads the refusal
			settle(res, refusal)
			outcome = refusal.outcome
		case res.IsError:
			var f *failure
			if errors.As(res.GetError(), &f) {
				outcome = f.outcome
			} else {
				// the SDK's own refusal of the arguments: its text names the schema's keywords and quotes the value, so
				// the caller reads a fixed sentence and the log, which holds no argument, only the outcome
				settle(res, &failure{"invalid_input", msgInvalid})
				outcome = "invalid_input"
			}
		}
		return result, nil
	}
}

// safely makes a panic in a tool a failure with a reference, as any other error that is none of the known kinds.
func safely[In any](v *server, tool string, h func(context.Context, *sdk.CallToolRequest, In) (*sdk.CallToolResult, any, error)) func(context.Context, *sdk.CallToolRequest, In) (*sdk.CallToolResult, any, error) {
	return func(ctx context.Context, req *sdk.CallToolRequest, in In) (res *sdk.CallToolResult, out any, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				res, out, err = nil, nil, v.failed(tool, fmt.Errorf("panic: %v", rec))
			}
		}()
		return h(ctx, req, in)
	}
}

func newRef() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// failed turns what FIN (or the context) answered into a failure with a fixed sentence. An error that is none of the
// known kinds is logged here with a reference id, which the sentence carries, and told no further.
func (v *server) failed(tool string, err error) error {
	var netErr net.Error
	var pgErr *pgconn.PgError
	var connErr *pgconn.ConnectError
	switch {
	case errors.Is(err, api.ErrUnauthenticated), errors.Is(err, api.ErrUnauthorized):
		return &failure{"not_allowed", msgNotAllowed}
	case errors.Is(err, api.ErrNotFound):
		return &failure{"not_found", msgNotFound}
	case errors.Is(err, api.ErrRejected):
		return &failure{"invalid_input", msgInvalid}
	case errors.Is(err, bentoapp.ErrCommitUncertain), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded),
		errors.As(err, &netErr), errors.As(err, &connErr),
		errors.As(err, &pgErr) && (strings.HasPrefix(pgErr.Code, "08") || strings.HasPrefix(pgErr.Code, "57P") || strings.HasPrefix(pgErr.Code, "53")):
		return &failure{"unavailable", msgUnavailable}
	}
	ref := newRef()
	v.log.Error("fin mcp tool failed", "tool", tool, "ref", ref, "error", err)
	return &failure{"failed", msgFailed + " (reference " + ref + ")"}
}

// accessOf is the caller the guard fixed. A handler that is reached without one refuses.
func accessOf(ctx context.Context) (access, error) {
	if f, ok := ctx.Value(refusalKey{}).(*failure); ok {
		return access{}, f
	}
	a, ok := ctx.Value(accessKey{}).(access)
	if !ok {
		return access{}, &failure{"not_allowed", msgNotAllowed}
	}
	return a, nil
}

func limitOf(n int) int {
	if n == 0 {
		return defaultLimit
	}
	return n
}

// jobEntity answers the legal entity of the job, or a not-found failure when the job does not exist or belongs to a
// legal entity the caller may not see: the two are told alike, so that the existence of a job is not given away. It is
// asked before anything else about the job is read.
func (v *server) jobEntity(ctx context.Context, tool string, a access, jobID string) (string, error) {
	if !identifier.MatchString(jobID) {
		return "", &failure{"invalid_input", msgInvalid}
	}
	entity, err := v.fin.GetJobLegalEntity(ctx, api.GetJobLegalEntity{TenantID: a.tenant, ActorID: a.actor, JobID: jobID})
	if err != nil {
		return "", v.failed(tool, err)
	}
	if !a.sees(entity) {
		// told as a missing job; the audit line says "hidden", so that probing shows
		return "", &failure{"hidden", msgNotFound}
	}
	return entity, nil
}

// result writes the tool's result: a short text for the model and the structured content with its integers as they are.
func result(text string, structured any) (*sdk.CallToolResult, any, error) {
	raw, err := json.Marshal(structured)
	if err != nil {
		return nil, nil, err
	}
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: text}}, StructuredContent: json.RawMessage(raw)}, nil, nil
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func (v *server) listLossJobs(ctx context.Context, _ *sdk.CallToolRequest, in listLossIn) (*sdk.CallToolResult, any, error) {
	a, err := accessOf(ctx)
	if err != nil {
		return nil, nil, err
	}
	if !identifier.MatchString(in.LegalEntityID) {
		return nil, nil, &failure{"invalid_input", msgInvalid}
	}
	if !a.sees(in.LegalEntityID) {
		return nil, nil, &failure{"not_allowed", msgNotAllowed}
	}
	after, delivered := "", 0
	if in.Cursor != "" {
		c, err := v.cursors.open(a, in.Cursor, cursorBody{Tool: toolListLossJobs, Scope: in.LegalEntityID})
		if err != nil {
			return nil, nil, &failure{"bad_cursor", msgBadCursor}
		}
		after, delivered = c.After, c.Rows
	}
	want := min(limitOf(in.Limit), rowCap-delivered)
	out := lossOut{LegalEntityID: in.LegalEntityID, ProfitBasisNote: basisNote, Rows: []lossRow{}}
	// FIN's page may hold fewer rows than asked and still have a next page, and a next page may be empty: read on while
	// next_after is not empty, up to five pages, never until a page is short
	for pages := 0; pages < maxPages && len(out.Rows) < want; pages++ {
		page, err := v.fin.ListJobProfits(ctx, api.ListJobProfits{TenantID: a.tenant, ActorID: a.actor, LegalEntityID: in.LegalEntityID,
			LossOnly: true, Limit: want - len(out.Rows), After: after})
		if err != nil {
			return nil, nil, v.failed(toolListLossJobs, err)
		}
		out.AsOf = stamp(page.AsOf)
		for _, r := range page.Rows {
			out.Rows = append(out.Rows, lossRow{JobID: r.JobID, ShipmentID: r.ShipmentID, State: r.State, Revenue: amountOf(r.Revenue), Cost: amountOf(r.Cost),
				Profit: amountOf(r.Profit), ProfitBasis: r.ProfitBasis, ProfitVersion: r.ProfitVersion})
		}
		if after = page.NextAfter; after == "" {
			break
		}
	}
	if after != "" {
		out.Truncated = len(out.Rows) < want
		if delivered+len(out.Rows) >= rowCap {
			out.RowCapReached = true
		} else if out.NextCursor, err = v.cursors.seal(a, cursorBody{Tool: toolListLossJobs, After: after, Rows: delivered + len(out.Rows),
			Scope: in.LegalEntityID}); err != nil {
			return nil, nil, v.failed(toolListLossJobs, err)
		}
	}
	text := fmt.Sprintf("%d loss-making jobs of legal entity %s (as of %s).", len(out.Rows), out.LegalEntityID, out.AsOf)
	if len(out.Rows) > 0 {
		worst := out.Rows[0]
		for _, r := range out.Rows[1:] {
			if r.Profit.Minor < worst.Profit.Minor {
				worst = r
			}
		}
		text += fmt.Sprintf(" Worst: %s, profit %s.", worst.JobID, humanAmount(worst.Profit))
	}
	switch {
	case out.RowCapReached:
		text += " The 200 rows of this chain are delivered; ask about one job or another legal entity."
	case out.NextCursor != "" && out.Truncated:
		text += " The call stopped early; pass next_cursor to continue."
	case out.NextCursor != "":
		text += " More: pass next_cursor."
	}
	return result(text, out)
}

func (v *server) getJobProfit(ctx context.Context, _ *sdk.CallToolRequest, in profitIn) (*sdk.CallToolResult, any, error) {
	a, err := accessOf(ctx)
	if err != nil {
		return nil, nil, err
	}
	entity, err := v.jobEntity(ctx, toolGetJobProfit, a, in.JobID)
	if err != nil {
		return nil, nil, err
	}
	p, err := v.fin.GetJobProfit(ctx, api.GetJobProfit{TenantID: a.tenant, ActorID: a.actor, JobID: in.JobID})
	if err != nil {
		return nil, nil, v.failed(toolGetJobProfit, err)
	}
	out := profitOut{JobID: p.JobID, LegalEntityID: entity, Revision: p.Revision, ProfitVersion: p.ProfitVersion, ProfitBasis: p.ProfitBasis, ProfitBasisNote: basisNote,
		SettingsVersion: p.SettingsVersion, Revenue: amountOf(p.Revenue), Invoiced: amountOf(p.Invoiced), Cost: amountOf(p.Cost), CostMatched: amountOf(p.CostMatched),
		CostAccrued: amountOf(p.CostAccrued), PoolCost: amountOf(p.PoolCost), Profit: amountOf(p.Profit), PendingInProfit: amountOf(p.PendingInProfit),
		FormerSharesPending: amountOf(p.FormerSharesPending), ReroutingLoss: amountOf(p.ReroutingLoss), ByChargeCode: []chargeCodeRow{}, PoolShares: []poolShareRow{},
		ReroutingLosses: []reroutingRow{}}
	for _, c := range p.ByCode {
		out.ByChargeCode = append(out.ByChargeCode, chargeCodeRow{ChargeCode: c.ChargeCode, Revenue: amountOf(c.Revenue), Cost: amountOf(c.Cost), Profit: amountOf(c.Profit)})
	}
	for _, s := range append(append([]api.JobPoolShare{}, p.PoolShares...), p.FormerShares...) {
		out.PoolShares = append(out.PoolShares, poolShareRow{PoolID: s.PoolID, LineNo: s.LineNo, ChargeCode: s.ChargeCode, Method: s.Method, Local: amountOf(s.Local), Pending: s.Pending})
	}
	for _, l := range p.ReroutingLosses {
		out.ReroutingLosses = append(out.ReroutingLosses, reroutingRow{PoolID: l.PoolID, LineNo: l.LineNo, Local: amountOf(l.Local), Voucher: l.Voucher})
	}
	text := fmt.Sprintf("Job %s of legal entity %s: revenue %s, cost %s, pool cost %s, profit %s (basis %s).", out.JobID, entity, humanAmount(out.Revenue),
		humanAmount(out.Cost), humanAmount(out.PoolCost), humanAmount(out.Profit), out.ProfitBasis)
	return result(text, out)
}

func (v *server) listJobCostReviews(ctx context.Context, _ *sdk.CallToolRequest, in reviewsIn) (*sdk.CallToolResult, any, error) {
	a, err := accessOf(ctx)
	if err != nil {
		return nil, nil, err
	}
	if in.State != "" && in.State != api.ReviewOpen && in.State != api.ReviewResolved {
		return nil, nil, &failure{"invalid_input", msgInvalid}
	}
	entity, err := v.jobEntity(ctx, toolListJobCostReview, a, in.JobID)
	if err != nil {
		return nil, nil, err
	}
	scope := cursorBody{Tool: toolListJobCostReview, Scope: in.JobID, State: in.State}
	after, delivered := "", 0
	if in.Cursor != "" {
		c, err := v.cursors.open(a, in.Cursor, scope)
		if err != nil {
			return nil, nil, &failure{"bad_cursor", msgBadCursor}
		}
		after, delivered = c.After, c.Rows
	}
	page, err := v.fin.ListCostReviewRequests(ctx, api.ListCostReviewRequests{TenantID: a.tenant, ActorID: a.actor, JobID: in.JobID, State: in.State,
		Limit: min(limitOf(in.Limit), rowCap-delivered), After: after})
	if err != nil {
		return nil, nil, v.failed(toolListJobCostReview, err)
	}
	out := reviewsOut{JobID: in.JobID, LegalEntityID: entity, Requests: []reviewRow{}}
	open := 0
	for _, r := range page.Requests {
		row := reviewRow{RequestID: r.RequestID, State: r.State, RequestedAt: stamp(r.RequestedAt), Reason: r.Reason, Revenue: amountOf(r.Revenue),
			Cost: amountOf(r.Cost), Profit: amountOf(r.Profit), ProfitBasis: r.ProfitBasis, SettingsVersion: r.SettingsVersion, ProfitVersion: r.ProfitVersion}
		if r.Resolution != nil {
			row.Resolution = &resolutionRow{ResolvedAt: stamp(r.Resolution.ResolvedAt), Outcome: r.Resolution.Outcome, Note: r.Resolution.Note}
		}
		if r.State == api.ReviewOpen {
			open++
		}
		out.Requests = append(out.Requests, row)
	}
	if page.NextAfter != "" {
		if delivered+len(out.Requests) >= rowCap {
			out.RowCapReached = true
		} else {
			scope.After, scope.Rows = page.NextAfter, delivered+len(out.Requests)
			if out.NextCursor, err = v.cursors.seal(a, scope); err != nil {
				return nil, nil, v.failed(toolListJobCostReview, err)
			}
		}
	}
	text := fmt.Sprintf("%d cost review requests of job %s (%d open).", len(out.Requests), in.JobID, open)
	switch {
	case out.RowCapReached:
		text += " The 200 rows of this chain are delivered."
	case out.NextCursor != "":
		text += " More: pass next_cursor."
	}
	return result(text, out)
}
