// Package settings is the mechanism by which a context's rules that differ from one customer to another are configured,
// not coded (the design of the configuration of the rules, sections 2, 3 and 6). A context registers its items in a
// Catalogue: each item has values ordered from strict to loose, a default (the "standard" profile's), the scopes it may
// be set at and, for a value that needs one, a parameter. A Ledger holds the change sets proposed, approved and
// cancelled so far and answers what is in force for a scope at a time, which version it is, and whether a change set is
// a loosening and so needs a second person.
//
// The package is pure Go: it keeps no state and runs no SQL. A context supplies its catalogue and stores the ledger's
// rows (FIN's are in migrations/fin/000017_fin_settings.sql). Foundation packages import no other.
package settings

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Scope is a level of the chain at which a value is set: the tenant (the company; its ID is empty) or an entity under
// it, such as a legal entity. Stored rows name a scope by kind and ID only, never by its position in the chain.
type Scope struct {
	Kind string
	ID   string
}

// Kinds of scope. A context may add kinds (a branch, a transport mode) without changing a row already stored: the
// chain of a scope comes from the context's Hierarchy, not from the rows.
const (
	KindTenant      = "TENANT"
	KindLegalEntity = "LEGAL_ENTITY"
)

// TenantScope is the company's scope.
var TenantScope = Scope{Kind: KindTenant}

// Chain is the scopes consulted for one scope, the most specific first and the tenant last.
type Chain []Scope

// Hierarchy answers the chain of a scope, or an error when the scope is not one the context knows.
type Hierarchy func(Scope) (Chain, error)

// Param is the parameter a value asks for: an amount in the functional currency of the scope it is given at, in minor
// units, required when the value is chosen and without a preset. It may be given at the Kinds of scope only, because
// the same number means something else in another currency. LooserWhenHigher says that a higher amount lets more
// through, so raising it is a loosening.
type Param struct {
	Name             string
	TitleZH, TitleEN string
	Kinds            []string
	LooserWhenHigher bool
}

// MaxParam bounds a parameter in minor units.
const MaxParam int64 = 1_000_000_000_000_000

// Value is one choice of an item, with its identifier and its titles.
type Value struct {
	ID               string
	TitleZH, TitleEN string
	Param            *Param
}

// Item is one setting of a context.
type Item struct {
	Key              string // for example fin.F1
	TitleZH, TitleEN string
	Values           []Value // from strict to loose
	Default          string  // the identifier of the standard profile's value
	Kinds            []string
	Control          bool // an internal control
	Explanation      string
}

// Rank is the position of a value among the item's, 0 for the strictest; ok is false for an unknown value.
func (it Item) Rank(value string) (rank int, ok bool) {
	i := slices.IndexFunc(it.Values, func(v Value) bool { return v.ID == value })
	return i, i >= 0
}

// Setting is the value in force for one item in one scope.
type Setting struct {
	Value string
	Param *int64
	From  Scope // the scope the value is set at
	Set   bool  // false when the item's default applies
	// ChangeID is the change set that set the value; Exception says that a scope below the tenant holds a value looser
	// than the tenant's, an approved exception that stays in force when the tenant tightens.
	ChangeID  string
	Exception bool
}

// Config is the settings in force for one scope, by item key.
type Config map[string]Setting

// Rule is a rule across items: it answers an error naming the items and the way out when a configuration breaks it.
type Rule func(Config) error

// Reference is a fact a context keeps with its own history and lists in the catalogue, though the settings commands do
// not change it (the design's E1 to E4): what it governs, where it is changed, and whether a change needs approval.
type Reference struct {
	Key              string
	TitleZH, TitleEN string
	Governs          string
	ChangedBy        string
	Approval         bool
	ApprovalNote     string
}

// Catalogue is a context's items, its cross-item rules and its scope hierarchy; Reference lists the reference data the
// context keeps elsewhere.
type Catalogue struct {
	Context   string
	Items     []Item
	Reference []Reference
	Rules     []Rule
	Hierarchy Hierarchy
}

// Item answers the item with the key.
func (c *Catalogue) Item(key string) (Item, bool) {
	i := slices.IndexFunc(c.Items, func(it Item) bool { return it.Key == key })
	if i < 0 {
		return Item{}, false
	}
	return c.Items[i], true
}

var (
	keyPattern   = regexp.MustCompile(`^[a-z]{2,8}\.[A-Z][A-Za-z0-9]{0,31}$`)
	valuePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
)

// Check answers the first way in which the catalogue itself is wrong: a key or value that is not an identifier, one
// used twice, an item with fewer than two values, a default that is not one of them, no scope to set it at, a title
// missing, a parameter without a scope.
func (c *Catalogue) Check() error {
	if c.Context == "" || c.Hierarchy == nil || len(c.Items) == 0 {
		return errors.New("settings: a catalogue has a context, a hierarchy and items")
	}
	keys := map[string]bool{}
	for _, it := range c.Items {
		switch {
		case !keyPattern.MatchString(it.Key) || keys[it.Key]:
			return fmt.Errorf("settings: item key %q is not an identifier or is used twice", it.Key)
		case it.TitleZH == "" || it.TitleEN == "" || it.Explanation == "":
			return fmt.Errorf("settings: item %s needs its titles and an explanation", it.Key)
		case len(it.Values) < 2 || len(it.Kinds) == 0:
			return fmt.Errorf("settings: item %s needs two values or more and a scope to be set at", it.Key)
		}
		keys[it.Key] = true
		if _, ok := it.Rank(it.Default); !ok {
			return fmt.Errorf("settings: the default of %s is not one of its values", it.Key)
		}
		seen := map[string]bool{}
		for _, v := range it.Values {
			if !valuePattern.MatchString(v.ID) || seen[v.ID] || v.TitleZH == "" || v.TitleEN == "" {
				return fmt.Errorf("settings: value %q of %s is not an identifier, is used twice or has no titles", v.ID, it.Key)
			}
			seen[v.ID] = true
			if v.Param != nil && (v.Param.Name == "" || len(v.Param.Kinds) == 0) {
				return fmt.Errorf("settings: the parameter of %s %s needs a name and a scope", it.Key, v.ID)
			}
		}
	}
	return nil
}

// Table renders the catalogue as a Markdown table, one row for each item: its key, titles, values from strict to
// loose with the default in bold, the scopes it may be set at, whether it is an internal control and its explanation.
func (c *Catalogue) Table() string {
	var b strings.Builder
	b.WriteString("| Key | Title | Values, strict to loose (default in bold) | Set at | Internal control | Explanation |\n|---|---|---|---|---|---|\n")
	for _, it := range c.Items {
		var values []string
		for _, v := range it.Values {
			s := v.ID + " (" + v.TitleEN + " / " + v.TitleZH + ")"
			if v.Param != nil {
				s += ", with " + v.Param.Name
			}
			if v.ID == it.Default {
				s = "**" + s + "**"
			}
			values = append(values, s)
		}
		control := "no"
		if it.Control {
			control = "yes"
		}
		fmt.Fprintf(&b, "| %s | %s / %s | %s | %s | %s | %s |\n", it.Key, it.TitleEN, it.TitleZH, strings.Join(values, "; "),
			strings.Join(it.Kinds, ", "), control, it.Explanation)
	}
	if len(c.Reference) > 0 {
		b.WriteString("\nReference data, kept in the context's own records and not changed by the settings commands:\n\n" +
			"| Key | Title | Governs | Changed by | Needs approval |\n|---|---|---|---|---|\n")
		for _, r := range c.Reference {
			approval := "no"
			if r.Approval {
				approval = "yes"
			}
			fmt.Fprintf(&b, "| %s | %s / %s | %s | %s | %s: %s |\n", r.Key, r.TitleEN, r.TitleZH, r.Governs, r.ChangedBy, approval, r.ApprovalNote)
		}
	}
	return b.String()
}
