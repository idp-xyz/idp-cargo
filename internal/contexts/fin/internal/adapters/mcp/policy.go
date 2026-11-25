package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Subject is the member the gateway's signed assertion names: its tenant (the gateway's own), the subject and the
// roles. Nothing else about a caller is believed.
type Subject struct {
	Tenant string
	ID     string
	Roles  []string
	Client string
	Actor  string
}

// PermissionReadProfit is the only permission this server can grant: the three tools read.
const PermissionReadProfit = "fin:ReadProfit"

// Policy is what CargoPai lets the gateway's tenants and roles do in FIN: for each gateway tenant the CargoPai tenant
// it stands for and, for each role, the FIN permissions and the legal entities its members may see. A tenant or a role
// that is not named has no access (default deny): the gateway's role decides only that a tool may be called at all.
type Policy struct {
	Tenants map[string]TenantPolicy `json:"tenants"`
}

// TenantPolicy is the policy of one gateway tenant.
type TenantPolicy struct {
	CargoTenant string                `json:"cargo_tenant"`
	Roles       map[string]RolePolicy `json:"roles"`
}

// RolePolicy is what a role grants: permissions ("fin:ReadProfit") and the legal entities its members may see, a list
// of identifiers or "*" for every legal entity of the tenant.
type RolePolicy struct {
	Permissions   []string `json:"permissions"`
	LegalEntities Entities `json:"legal_entities"`
}

// Entities is a list of legal entity ids, or all of the tenant's ("*").
type Entities struct {
	All bool
	IDs []string
}

// UnmarshalJSON reads "*" or a list of identifiers.
func (e *Entities) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		if one != "*" {
			return errors.New(`legal_entities is a list of legal entity ids or "*"`)
		}
		*e = Entities{All: true}
		return nil
	}
	var ids []string
	if err := json.Unmarshal(b, &ids); err != nil {
		return errors.New(`legal_entities is a list of legal entity ids or "*"`)
	}
	*e = Entities{IDs: ids}
	return nil
}

// MarshalJSON writes what UnmarshalJSON reads.
func (e Entities) MarshalJSON() ([]byte, error) {
	if e.All {
		return []byte(`"*"`), nil
	}
	return json.Marshal(e.IDs)
}

// identifier is the form of a tenant, a subject and a legal entity id in FIN (the HTTP adapter's, and the tools'
// input).
var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

// tenantID is the form identity gives a tenant and an actor.
var tenantID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,127}$`)

// Validate refuses a policy that names nothing, an identifier of a form FIN does not take, or a permission this server
// does not grant.
func (p Policy) Validate() error {
	if len(p.Tenants) == 0 {
		return errors.New("the policy names no tenant: nobody could use the tools")
	}
	for gw, t := range p.Tenants {
		switch {
		case gw == "" || strings.TrimSpace(gw) != gw:
			return errors.New("a gateway tenant is empty or has blanks around it")
		case !tenantID.MatchString(t.CargoTenant):
			return fmt.Errorf("tenant %q: cargo_tenant %q is not a tenant id", gw, t.CargoTenant)
		case len(t.Roles) == 0:
			return fmt.Errorf("tenant %q names no role", gw)
		}
		for role, r := range t.Roles {
			if role == "" || strings.TrimSpace(role) != role {
				return fmt.Errorf("tenant %q: a role is empty or has blanks around it", gw)
			}
			if len(r.Permissions) == 0 {
				return fmt.Errorf("tenant %q, role %q: no permission", gw, role)
			}
			for _, perm := range r.Permissions {
				if perm != PermissionReadProfit {
					return fmt.Errorf("tenant %q, role %q: permission %q; this server grants only %s", gw, role, perm, PermissionReadProfit)
				}
			}
			if !r.LegalEntities.All && len(r.LegalEntities.IDs) == 0 {
				return fmt.Errorf("tenant %q, role %q: no legal entity", gw, role)
			}
			for _, id := range r.LegalEntities.IDs {
				if !identifier.MatchString(id) {
					return fmt.Errorf("tenant %q, role %q: %q is not a legal entity id", gw, role, id)
				}
			}
		}
	}
	return nil
}

// access is a caller as CargoPai's policy sees it.
type access struct {
	tenant      string
	actor       string
	permissions []string
	all         bool
	entities    map[string]bool
}

// sees reports whether the caller may see the legal entity.
func (a access) sees(entity string) bool { return a.all || a.entities[entity] }

// errDenied is every refusal of the policy; the caller is told only that it is not allowed.
var errDenied = errors.New("the caller is not allowed")

// resolve maps the member to CargoPai's tenant, the actor FIN will see (the subject of the assertion), the union of the
// permissions of the roles the policy names and the union of their legal entities. A tenant or a role the policy does
// not name adds nothing; a member with no role that adds a permission is refused.
func (p Policy) resolve(s Subject) (access, error) {
	t, ok := p.Tenants[s.Tenant]
	if !ok || !tenantID.MatchString(s.ID) {
		return access{}, errDenied
	}
	a := access{tenant: t.CargoTenant, actor: s.ID, entities: map[string]bool{}}
	for _, role := range s.Roles {
		r, ok := t.Roles[role]
		if !ok {
			continue
		}
		for _, perm := range r.Permissions {
			if !slices.Contains(a.permissions, perm) {
				a.permissions = append(a.permissions, perm)
			}
		}
		a.all = a.all || r.LegalEntities.All
		for _, id := range r.LegalEntities.IDs {
			a.entities[id] = true
		}
	}
	if !slices.Contains(a.permissions, PermissionReadProfit) {
		return access{}, errDenied
	}
	return a, nil
}
