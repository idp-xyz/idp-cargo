package mcp

import (
	"fmt"
	"strconv"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// The three tools, in the order the catalogue lists them. Their declarations (names, descriptions, schemas and
// annotations) are what the gateway's reviewers approved: a change of any of them, even a word of a description, is a
// drift the gateway blocks the target for until a new review, so a unit test pins their digest.
const (
	toolListLossJobs      = "list_loss_jobs"
	toolGetJobProfit      = "get_job_profit"
	toolListJobCostReview = "list_job_cost_reviews"
)

// Bounds of what a call returns.
const (
	defaultLimit = 20
	maxLimit     = 50
	maxPages     = 5   // FIN pages one list_loss_jobs call reads
	rowCap       = 200 // rows one chain of cursors delivers
	maxCursor    = 1024
)

const basisNote = "Figures under SEPARATE and INCLUDED_MARKED are not comparable."

// Patterns of the inputs (an identifier is FIN's own form of a job and a legal entity id; a cursor is two base64url
// parts).
const (
	idPattern     = `^[A-Za-z0-9][A-Za-z0-9._:/-]*$`
	cursorPattern = `^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$`
)

func idProperty(what string) map[string]any {
	return map[string]any{"type": "string", "minLength": 1, "maxLength": 128, "pattern": idPattern, "description": what}
}

func limitProperty(what string) map[string]any {
	return map[string]any{"type": "integer", "minimum": 1, "maximum": maxLimit, "default": defaultLimit, "description": what}
}

func cursorProperty() map[string]any {
	return map[string]any{"type": "string", "minLength": 1, "maxLength": maxCursor, "pattern": cursorPattern,
		"description": "The next_cursor of the previous call, unchanged. Omit it for the first call."}
}

// object is an input schema that takes the listed properties and nothing else.
func object(required []string, props map[string]any) map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": props}
}

// amount is money as FIN gives it: an integer in minor units and its currency, never a decimal.
type amount struct {
	Currency string `json:"currency" jsonschema:"ISO 4217 currency code."`
	Minor    int64  `json:"minor" jsonschema:"The amount in minor units of the currency, an integer (100.01 CNY is 10001)."`
}

func amountOf(m kernel.Money) amount { return amount{Currency: string(m.Currency), Minor: m.Minor} }

type lossRow struct {
	JobID         string `json:"job_id" jsonschema:"The job."`
	ShipmentID    string `json:"shipment_id" jsonschema:"The shipment the job belongs to."`
	State         string `json:"state" jsonschema:"The job's state."`
	Revenue       amount `json:"revenue" jsonschema:"Revenue in the job's functional currency."`
	Cost          amount `json:"cost" jsonschema:"The job's cost and what it bears of consolidation cost pools; revenue less cost is the profit."`
	Profit        amount `json:"profit" jsonschema:"Revenue less cost; below zero for a row of this list."`
	ProfitBasis   string `json:"profit_basis" jsonschema:"SEPARATE or INCLUDED_MARKED, the basis the profit is told on."`
	ProfitVersion string `json:"profit_version" jsonschema:"An opaque text; compare it for equality only."`
}

type lossOut struct {
	LegalEntityID   string    `json:"legal_entity_id" jsonschema:"The legal entity asked about."`
	AsOf            string    `json:"as_of" jsonschema:"When FIN read the rows, on FIN's clock (RFC 3339)."`
	ProfitBasisNote string    `json:"profit_basis_note" jsonschema:"Whether figures under different profit bases can be compared."`
	Rows            []lossRow `json:"rows" jsonschema:"The loss-making jobs of this call."`
	NextCursor      string    `json:"next_cursor,omitempty" jsonschema:"Pass it unchanged to read on; absent when there is nothing more to read."`
	Truncated       bool      `json:"truncated" jsonschema:"True when the call stopped after five pages of FIN with more to read."`
	RowCapReached   bool      `json:"row_cap_reached" jsonschema:"True when this chain of cursors has delivered its 200 rows and no further cursor is given."`
}

type chargeCodeRow struct {
	ChargeCode string `json:"charge_code" jsonschema:"The charge code."`
	Revenue    amount `json:"revenue" jsonschema:"Estimated revenue of the code."`
	Cost       amount `json:"cost" jsonschema:"Estimated cost of the code."`
	Profit     amount `json:"profit" jsonschema:"Estimated profit of the code."`
}

type poolShareRow struct {
	PoolID     string `json:"pool_id" jsonschema:"The consolidation cost pool."`
	LineNo     int    `json:"line_no" jsonschema:"The pool's cost line."`
	ChargeCode string `json:"charge_code" jsonschema:"The charge code of the line."`
	Method     string `json:"method" jsonschema:"How the line is apportioned."`
	Local      amount `json:"local" jsonschema:"The job's share in the functional currency."`
	Pending    bool   `json:"pending" jsonschema:"True for a former share pending finance's confirmation."`
}

type reroutingRow struct {
	PoolID  string `json:"pool_id" jsonschema:"The consolidation cost pool."`
	LineNo  int    `json:"line_no" jsonschema:"The pool's cost line."`
	Local   amount `json:"local" jsonschema:"The re-routing loss the job bears, in the functional currency."`
	Voucher string `json:"voucher" jsonschema:"The voucher of the loss."`
}

type profitOut struct {
	JobID               string          `json:"job_id" jsonschema:"The job."`
	LegalEntityID       string          `json:"legal_entity_id" jsonschema:"The legal entity the job belongs to."`
	Revision            int64           `json:"revision" jsonschema:"The job's revision."`
	ProfitVersion       string          `json:"profit_version" jsonschema:"An opaque text; compare it for equality only."`
	ProfitBasis         string          `json:"profit_basis" jsonschema:"SEPARATE or INCLUDED_MARKED, the basis the profit is told on."`
	ProfitBasisNote     string          `json:"profit_basis_note" jsonschema:"Whether figures under different profit bases can be compared."`
	SettingsVersion     int             `json:"settings_version" jsonschema:"The version of FIN's settings the figures were read under."`
	Revenue             amount          `json:"revenue" jsonschema:"Revenue."`
	Invoiced            amount          `json:"invoiced" jsonschema:"The part of the revenue that is invoiced."`
	Cost                amount          `json:"cost" jsonschema:"The job's own cost: matched plus accrued."`
	CostMatched         amount          `json:"cost_matched" jsonschema:"The part of the cost matched to creditor invoices."`
	CostAccrued         amount          `json:"cost_accrued" jsonschema:"The part of the cost still accrued."`
	PoolCost            amount          `json:"pool_cost" jsonschema:"What the job bears of consolidation cost pools."`
	Profit              amount          `json:"profit" jsonschema:"Revenue less cost less pool cost."`
	PendingInProfit     amount          `json:"pending_in_profit" jsonschema:"The part of the pool cost that is a former share pending confirmation."`
	FormerSharesPending amount          `json:"former_shares_pending" jsonschema:"Former shares pending confirmation."`
	ReroutingLoss       amount          `json:"rerouting_loss" jsonschema:"Re-routing losses the job bears, within the pool cost."`
	ByChargeCode        []chargeCodeRow `json:"by_charge_code" jsonschema:"Estimated revenue, cost and profit by charge code."`
	PoolShares          []poolShareRow  `json:"pool_shares" jsonschema:"The job's shares of pool cost lines, and its former shares."`
	ReroutingLosses     []reroutingRow  `json:"rerouting_losses" jsonschema:"The re-routing losses the job bears."`
}

type resolutionRow struct {
	ResolvedAt string `json:"resolved_at" jsonschema:"When, on FIN's clock (RFC 3339)."`
	Outcome    string `json:"outcome" jsonschema:"COST_CORRECTED or NO_CHANGE."`
	Note       string `json:"note" jsonschema:"The resolver's note: text a person wrote."`
}

type reviewRow struct {
	RequestID       string         `json:"request_id" jsonschema:"The request, CRR-<n>."`
	State           string         `json:"state" jsonschema:"OPEN or RESOLVED."`
	RequestedAt     string         `json:"requested_at" jsonschema:"When, on FIN's clock (RFC 3339)."`
	Reason          string         `json:"reason" jsonschema:"The reason given: text a person wrote."`
	Revenue         amount         `json:"revenue" jsonschema:"The revenue the requester saw."`
	Cost            amount         `json:"cost" jsonschema:"The cost the requester saw, with pool cost."`
	Profit          amount         `json:"profit" jsonschema:"The profit the requester saw."`
	ProfitBasis     string         `json:"profit_basis" jsonschema:"The basis the figures were told on."`
	SettingsVersion int            `json:"settings_version" jsonschema:"The settings version of the figures."`
	ProfitVersion   string         `json:"profit_version" jsonschema:"The profit version the requester saw; compare it for equality only."`
	Resolution      *resolutionRow `json:"resolution" jsonschema:"How the request ended; null while it is open."`
}

type reviewsOut struct {
	JobID         string      `json:"job_id" jsonschema:"The job."`
	LegalEntityID string      `json:"legal_entity_id" jsonschema:"The legal entity the job belongs to."`
	Requests      []reviewRow `json:"requests" jsonschema:"The job's cost review requests in the order made."`
	NextCursor    string      `json:"next_cursor,omitempty" jsonschema:"Pass it unchanged to read on; absent when there is nothing more to read."`
	RowCapReached bool        `json:"row_cap_reached" jsonschema:"True when this chain of cursors has delivered its 200 rows and no further cursor is given."`
}

// Inputs. The schemas below are the declaration; these are what a call that passed them is read into.
type (
	listLossIn struct {
		LegalEntityID string `json:"legal_entity_id"`
		Limit         int    `json:"limit,omitempty"`
		Cursor        string `json:"cursor,omitempty"`
	}
	profitIn struct {
		JobID string `json:"job_id"`
	}
	reviewsIn struct {
		JobID  string `json:"job_id"`
		State  string `json:"state,omitempty"`
		Limit  int    `json:"limit,omitempty"`
		Cursor string `json:"cursor,omitempty"`
	}
)

func ptr[T any](v T) *T { return &v }

// declarations are the three tools as the catalogue lists them.
func declarations() []*sdk.Tool {
	ann := func() *sdk.ToolAnnotations {
		return &sdk.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)}
	}
	var out []*sdk.Tool
	for _, d := range []struct {
		name, title, description string
		input                    map[string]any
		output                   *jsonschema.Schema
	}{
		{toolListLossJobs, "List loss-making jobs",
			"List the freight jobs of one legal entity whose profit is below zero, with revenue, cost and profit as integers in " +
				"minor units of the job's currency. Returns at most `limit` jobs per call and at most 200 along one chain of " +
				"cursors; pass the returned next_cursor, unchanged, to read on. When truncated is true the call stopped early " +
				"and more may follow; when row_cap_reached is true no further cursor is given. Use get_job_profit for the parts " +
				"of one job.",
			object([]string{"legal_entity_id"}, map[string]any{
				"legal_entity_id": idProperty("The legal entity."),
				"limit":           limitProperty("Jobs per call (default 20)."),
				"cursor":          cursorProperty(),
			}), schemaOf[lossOut]()},
		{toolGetJobProfit, "Get the profit of a job",
			"Read the profit of one freight job with its parts: revenue, invoiced part, cost (matched and accrued), cost of " +
				"consolidation pools, re-routing losses and the figures by charge code, as integers in minor units, with the " +
				"profit_version to compare for equality only. Use list_loss_jobs to find jobs.",
			object([]string{"job_id"}, map[string]any{"job_id": idProperty("The job.")}), schemaOf[profitOut]()},
		{toolListJobCostReview, "List the cost reviews of a job",
			"List the cost review requests finance holds for one freight job in the order made, each with the figures its " +
				"requester saw and its resolution when it has one. The reason and the note are text people wrote. Returns at most " +
				"`limit` requests per call; pass the returned next_cursor, unchanged, to read on.",
			object([]string{"job_id"}, map[string]any{
				"job_id": idProperty("The job."),
				"state": map[string]any{"type": "string", "enum": []string{"OPEN", "RESOLVED"},
					"description": "Only requests in this state; both when omitted."},
				"limit":  limitProperty("Requests per call (default 20)."),
				"cursor": cursorProperty(),
			}), schemaOf[reviewsOut]()},
	} {
		out = append(out, &sdk.Tool{Name: d.name, Title: d.title, Description: d.description, InputSchema: d.input, OutputSchema: d.output, Annotations: ann()})
	}
	return out
}

// schemaOf is the schema of a result type. It is built here, not left to the SDK: the SDK would validate the result
// through a float64, which cannot hold every integer amount (above 2^53 it rounds), so the result is written with its
// integers as FIN gave them and the schema only declares it (a test checks one against the other).
func schemaOf[T any]() *jsonschema.Schema {
	s, err := jsonschema.For[T](nil)
	if err != nil {
		panic(fmt.Sprintf("fin mcp: the schema of a result: %v", err))
	}
	return s
}

// humanAmount is an amount for a person to read, from its integer: -8001 CNY is "-80.01 CNY".
func humanAmount(a amount) string {
	digits, ok := kernel.MinorDigits(kernel.CurrencyCode(a.Currency))
	if !ok {
		return fmt.Sprintf("%d minor units of %s", a.Minor, a.Currency)
	}
	u := uint64(a.Minor)
	if a.Minor < 0 {
		u = -u
	}
	s := strconv.FormatUint(u, 10)
	if digits > 0 {
		for len(s) <= digits {
			s = "0" + s
		}
		s = s[:len(s)-digits] + "." + s[len(s)-digits:]
	}
	if a.Minor < 0 {
		s = "-" + s
	}
	return s + " " + a.Currency
}
