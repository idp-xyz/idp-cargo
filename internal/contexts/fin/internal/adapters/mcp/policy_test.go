package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestThePolicyReadsAListOrAnAsteriskOfLegalEntities(t *testing.T) {
	var p Policy
	text := `{"tenants":{"t":{"cargo_tenant":"c","roles":{"a":{"permissions":["fin:ReadProfit"],"legal_entities":["LE","LE2"]},
		"b":{"permissions":["fin:ReadProfit"],"legal_entities":"*"}}}}}`
	if err := json.Unmarshal([]byte(text), &p); err != nil || p.Validate() != nil {
		t.Fatalf("%v %v", err, p.Validate())
	}
	a, b := p.Tenants["t"].Roles["a"].LegalEntities, p.Tenants["t"].Roles["b"].LegalEntities
	if a.All || len(a.IDs) != 2 || !b.All {
		t.Fatalf("%+v %+v", a, b)
	}
	out, _ := json.Marshal(p)
	var again Policy
	if json.Unmarshal(out, &again) != nil || !again.Tenants["t"].Roles["b"].LegalEntities.All {
		t.Fatalf("the policy does not read back: %s", out)
	}
	for _, bad := range []string{`"LE"`, `5`, `{"a":1}`, `["LE", 5]`, `null5`} {
		var e Entities
		if json.Unmarshal([]byte(bad), &e) == nil {
			t.Errorf("legal_entities %s was read", bad)
		}
	}
}

func TestAPolicyThatGrantsMoreOrNamesNothingIsRefused(t *testing.T) {
	with := func(mutate func(*TenantPolicy)) Policy {
		p := testPolicy()
		tp := p.Tenants["gw"]
		mutate(&tp)
		p.Tenants["gw"] = tp
		return p
	}
	read := []string{PermissionReadProfit}
	for name, p := range map[string]Policy{
		"no tenant":                   {},
		"no role":                     with(func(t *TenantPolicy) { t.Roles = nil }),
		"no cargo tenant":             with(func(t *TenantPolicy) { t.CargoTenant = "" }),
		"a cargo tenant with a blank": with(func(t *TenantPolicy) { t.CargoTenant = "a b" }),
		"no permission":               with(func(t *TenantPolicy) { t.Roles["analyst"] = RolePolicy{LegalEntities: Entities{All: true}} }),
		"every permission": with(func(t *TenantPolicy) {
			t.Roles["analyst"] = RolePolicy{Permissions: []string{"*"}, LegalEntities: Entities{All: true}}
		}),
		"fin:*": with(func(t *TenantPolicy) {
			t.Roles["analyst"] = RolePolicy{Permissions: []string{"fin:*"}, LegalEntities: Entities{All: true}}
		}),
		"a write permission": with(func(t *TenantPolicy) {
			t.Roles["analyst"] = RolePolicy{Permissions: []string{"fin:ResolveCostReview"}, LegalEntities: Entities{All: true}}
		}),
		"no legal entity": with(func(t *TenantPolicy) { t.Roles["analyst"] = RolePolicy{Permissions: read} }),
		"a bad legal entity": with(func(t *TenantPolicy) {
			t.Roles["analyst"] = RolePolicy{Permissions: read, LegalEntities: Entities{IDs: []string{"L E"}}}
		}),
		"an empty role name": with(func(t *TenantPolicy) { t.Roles[""] = RolePolicy{Permissions: read, LegalEntities: Entities{All: true}} }),
	} {
		if err := p.Validate(); err == nil {
			t.Errorf("%s: valid", name)
		}
	}
	if err := testPolicy().Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Policy{Tenants: map[string]TenantPolicy{" gw": {CargoTenant: "c", Roles: map[string]RolePolicy{"r": {Permissions: read, LegalEntities: Entities{All: true}}}}}}).Validate(); err == nil ||
		!strings.Contains(err.Error(), "gateway tenant") {
		t.Errorf("a gateway tenant with a blank: %v", err)
	}
}
