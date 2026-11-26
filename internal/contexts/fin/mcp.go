package fin

import (
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	finmcp "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/adapters/mcp"
)

// FIN V2.101: three read-only tools for an MCP server behind the IDP TELOS gateway. The types are the adapter's, named
// here so that the composition root can configure it.
type (
	// MCPConfig is what the tools need besides FIN: who the verified caller is, CargoPai's policy, the cursor key and
	// the limits.
	MCPConfig = finmcp.Config
	// MCPPolicy maps the gateway's tenants and roles to CargoPai's tenant, FIN's permissions and legal entities.
	MCPPolicy = finmcp.Policy
	// MCPSubject is the member the gateway's signed assertion names.
	MCPSubject = finmcp.Subject
	// MCPTenant, MCPRole and MCPEntities are the parts of the policy: a gateway tenant, a role of it, and the legal
	// entities a role sees (a list, or all of the tenant's).
	MCPTenant   = finmcp.TenantPolicy
	MCPRole     = finmcp.RolePolicy
	MCPEntities = finmcp.Entities
	// MCPLimits are the calls a member may make in a minute and at once.
	MCPLimits = finmcp.Limits
)

// RegisterMCP adds FIN's three read-only tools (list_loss_jobs, get_job_profit, list_job_cost_reviews) to s; the
// composition root calls it. FIN authorizes each query as it always does, for the member the configuration names.
func (m *Module) RegisterMCP(s *sdk.Server, cfg MCPConfig) error { return finmcp.Register(s, m, cfg) }
