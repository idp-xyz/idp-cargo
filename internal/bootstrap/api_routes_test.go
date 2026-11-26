package bootstrap

import (
	"bytes"
	"os"
	"reflect"
	"slices"
	"sort"
	"testing"

	"go.idp.xyz/idp-cargo/internal/contexts/air"
	airapi "go.idp.xyz/idp-cargo/internal/contexts/air/api"
	"go.idp.xyz/idp-cargo/internal/contexts/book"
	bookapi "go.idp.xyz/idp-cargo/internal/contexts/book/api"
	"go.idp.xyz/idp-cargo/internal/contexts/com"
	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	"go.idp.xyz/idp-cargo/internal/contexts/doc"
	docapi "go.idp.xyz/idp-cargo/internal/contexts/doc/api"
	"go.idp.xyz/idp-cargo/internal/contexts/ful"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	"go.idp.xyz/idp-cargo/internal/contexts/org"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	"go.idp.xyz/idp-cargo/internal/contexts/phy"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/foundation/httpapi"
)

// The published OpenAPI document is the routes' (stage 3, acceptance 1): regenerate it with
// go run ./cmd/cargo-openapi > contracts/openapi/cargo.json.
func TestThePublishedOpenAPIDocumentIsTheRoutes(t *testing.T) {
	doc, err := OpenAPIDocument()
	if err != nil {
		t.Fatal(err)
	}
	published, err := os.ReadFile("../../contracts/openapi/cargo.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(doc, published) {
		t.Fatal("contracts/openapi/cargo.json is not the routes' document: go run ./cmd/cargo-openapi > contracts/openapi/cargo.json")
	}
}

// coverage is a context's public api: the interfaces its module implements, and the methods that stay in process
// (another context's reads, the platform's own).
type coverage struct {
	context    string
	module     any
	interfaces []reflect.Type
	internal   map[string]string // method -> why it has no route
}

// The contexts whose routes are done, each with every public method routed once and the internal ones not at all
// (stage 3, acceptance 1). A context joins this list in the version that routes it.
var covered = []coverage{
	{context: "org", module: (*org.Module)(nil), interfaces: []reflect.Type{
		reflect.TypeFor[orgapi.Commands](), reflect.TypeFor[orgapi.MasterDataCommands](), reflect.TypeFor[orgapi.MasterDataQueries](),
		reflect.TypeFor[orgapi.PartyIdentityCommands](), reflect.TypeFor[orgapi.PartyIdentityQueries](), reflect.TypeFor[orgapi.AddressBookCommands](),
		reflect.TypeFor[orgapi.AddressBookQueries](), reflect.TypeFor[orgapi.ContactAccessQueries](), reflect.TypeFor[orgapi.Queries](),
		reflect.TypeFor[orgapi.LegalEntityQueries](), reflect.TypeFor[orgapi.ContactTransferRegisterCommands](),
		reflect.TypeFor[orgapi.ContactTransferRegisterQueries](), reflect.TypeFor[orgapi.OnboardingCommands](), reflect.TypeFor[orgapi.OnboardingQueries](),
	}, internal: map[string]string{
		"ResolvePartyRole":       "the other contexts' check of a party's role, with the particulars they freeze",
		"RecordOnboardingModule": "the platform's own record of a module's progress, which no person holds",
	}},
	{context: "com", module: (*com.Module)(nil), interfaces: []reflect.Type{
		reflect.TypeFor[comapi.Commands](), reflect.TypeFor[comapi.OrderQueries](), reflect.TypeFor[comapi.OrderPolicies](),
		reflect.TypeFor[comapi.ObligationReservations](), reflect.TypeFor[comapi.OrderPartyCommands](), reflect.TypeFor[comapi.OrderPartyQueries](),
		reflect.TypeFor[comapi.OrderContactRetention](), reflect.TypeFor[comapi.ContactAccessQueries](), reflect.TypeFor[comapi.Queries](),
	}, internal: map[string]string{
		"GetObligationSnapshot":   "FUL's read of an obligation version, v1",
		"GetObligationSnapshotV2": "FUL's read, v2",
		"GetObligationSnapshotV3": "FUL's read, v3; a person reads the order (GetCustomerOrder)",
	}},
	{context: "ful", module: (*ful.Module)(nil), interfaces: []reflect.Type{
		reflect.TypeFor[fulapi.Commands](), reflect.TypeFor[fulapi.ConsolAttachmentCommands](), reflect.TypeFor[fulapi.LoadEvidenceCommands](),
		reflect.TypeFor[fulapi.Queries](), reflect.TypeFor[fulapi.OceanOperationQueries](), reflect.TypeFor[fulapi.AirOperationPlanQueries](),
		reflect.TypeFor[fulapi.LoadIntentQueries](), reflect.TypeFor[fulapi.CostDriverQueries](),
	}, internal: map[string]string{
		"GetShipmentPlanSnapshot":  "the first plan snapshot; a person reads v2",
		"GetOceanOperationPlan":    "SEA's read of a house's plan and evidence",
		"GetOceanEvidenceCurrency": "SEA's check that FUL has PHY's latest facts",
		"GetAirOperationPlan":      "AIR's read of a house's plan",
		"GetLoadIntent":            "PHY's read of what a shipment intends to load",
		"GetShipmentCostDriver":    "FIN's read of a shipment's cost drivers",
	}},
	{context: "phy", module: (*phy.Module)(nil), interfaces: []reflect.Type{
		reflect.TypeFor[phyapi.Commands](), reflect.TypeFor[phyapi.PhysicalCommands](), reflect.TypeFor[phyapi.Queries](),
	}, internal: map[string]string{
		"GetConsolMembership":     "FUL's read of a house's membership",
		"GetPhysicalLoadEvidence": "FUL's read of the verified load evidence",
		"GetPhysicalCostBasis":    "FIN's read of the actuals it apportions by",
	}},
	{context: "book", module: (*book.Module)(nil), interfaces: []reflect.Type{reflect.TypeFor[bookapi.Commands](), reflect.TypeFor[bookapi.Queries]()}},
	{context: "air", module: (*air.Module)(nil), interfaces: []reflect.Type{reflect.TypeFor[airapi.Commands](), reflect.TypeFor[airapi.Queries](),
		reflect.TypeFor[airapi.AirDocFactsQueries]()}},
	{context: "doc", module: (*doc.Module)(nil), interfaces: []reflect.Type{
		reflect.TypeFor[docapi.Commands](), reflect.TypeFor[docapi.Queries](), reflect.TypeFor[docapi.PendingQueries](), reflect.TypeFor[docapi.CarrierCommands](),
		reflect.TypeFor[docapi.CarrierQueries](), reflect.TypeFor[docapi.DispatchCommands](), reflect.TypeFor[docapi.DispatchQueries](),
		reflect.TypeFor[docapi.TransportDocumentSnapshotQueries](),
	}, internal: map[string]string{"GetHouseDocumentSnapshot": "FIN's read of the house bill a debit note is on"}},
}

func TestEveryPublicMethodOfARoutedContextHasOneRoute(t *testing.T) {
	routes := map[string][]httpapi.Op{}
	for _, op := range APIRoutes() {
		routes[op.Context+"."+op.Calls] = append(routes[op.Context+"."+op.Calls], op)
	}
	for _, c := range covered {
		seen := map[string]bool{}
		for _, i := range c.interfaces {
			if !reflect.TypeOf(c.module).Implements(i) {
				t.Errorf("%s: the module does not implement %s", c.context, i)
			}
			for m := range i.NumMethod() {
				name := i.Method(m).Name
				if seen[name] {
					continue
				}
				seen[name] = true
				got := routes[c.context+"."+name]
				_, internal := c.internal[name]
				switch {
				case internal && len(got) > 0:
					t.Errorf("%s.%s stays in process (%s) but has a route: %s %s", c.context, name, c.internal[name], got[0].Method, got[0].Path)
				case !internal && len(got) != 1:
					t.Errorf("%s.%s has %d routes, not 1", c.context, name, len(got))
				}
			}
		}
		for name := range c.internal {
			if !seen[name] {
				t.Errorf("%s: %s is listed as internal but is no method of its interfaces", c.context, name)
			}
		}
		var stray []string
		for key, ops := range routes {
			if ops[0].Context == c.context && !ops[0].Legacy && !seen[ops[0].Calls] {
				stray = append(stray, key)
			}
		}
		sort.Strings(stray)
		if len(stray) > 0 {
			t.Errorf("%s: routes call methods outside its listed interfaces: %v", c.context, stray)
		}
	}
}

// The roles name only permissions of the form "<context>:<Action>", "<context>:*".
func TestTheRolesAreWellFormed(t *testing.T) {
	for role, perms := range Roles {
		for _, p := range perms {
			if !permissionForm.MatchString(p) {
				t.Errorf("%s: %q", role, p)
			}
		}
	}
	if _, err := RolePermissions([]string{"ops", "nobody"}, nil); err == nil {
		t.Fatal("an unknown role")
	}
	got, err := RolePermissions([]string{"docs", "privacy"}, []string{"org:Read"})
	if err != nil || !slices.Contains(got, "org:ErasePartyContact") || !slices.Contains(got, "org:ReserveContactTransfers") || !slices.IsSorted(got) ||
		len(slices.Compact(slices.Clone(got))) != len(got) {
		t.Fatalf("docs and privacy: %v %v", got, err)
	}
}
