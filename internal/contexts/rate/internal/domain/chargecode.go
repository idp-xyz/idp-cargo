package domain

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// RATE V0.5: the charge-code catalogue (MD-27). RATE owns the reference definition of charge codes; FIN owns the
// charges assessed and the tax treatment of each code per legal entity. A tenant's catalogue starts as the
// built-in starter list for the launch lines; defining, revising, deactivating or reactivating a code writes a new
// immutable version, so a starter code a tenant never touched stays at version 0.

// ChargeGroup groups charge codes for documents and reports.
type ChargeGroup string

const (
	GroupFreight       ChargeGroup = "FREIGHT"
	GroupOrigin        ChargeGroup = "ORIGIN"
	GroupDestination   ChargeGroup = "DESTINATION"
	GroupDocumentation ChargeGroup = "DOCUMENTATION"
	GroupCustoms       ChargeGroup = "CUSTOMS"
	GroupSurcharge     ChargeGroup = "SURCHARGE"
	GroupDisbursement  ChargeGroup = "DISBURSEMENT"
	GroupDiscount      ChargeGroup = "DISCOUNT"
)

func (g ChargeGroup) Valid() bool {
	switch g {
	case GroupFreight, GroupOrigin, GroupDestination, GroupDocumentation, GroupCustoms, GroupSurcharge, GroupDisbursement, GroupDiscount:
		return true
	}
	return false
}

// Calculator is how a rate line computes its amount: a flat amount per shipment, a rate per unit of a quantity, or
// a percentage of another charge of the same evaluation.
type Calculator string

const (
	CalcFlat      Calculator = "FLAT"
	CalcPerUnit   Calculator = "PER_UNIT"
	CalcPercentOf Calculator = "PERCENT_OF"
)

func (c Calculator) Valid() bool { return c == CalcFlat || c == CalcPerUnit || c == CalcPercentOf }

// RateUnit is the quantity a PER_UNIT line is rated on. Quantities are whole numbers of a base unit: grams for
// the kilogram units, cubic centimetres for CBM, thousandths of a W/M for FREIGHT_TONNE, pieces and documents.
type RateUnit string

const (
	UnitNone         RateUnit = ""
	UnitKgChargeable RateUnit = "KG_CHARGEABLE"
	UnitKgGross      RateUnit = "KG_GROSS"
	UnitCBM          RateUnit = "CBM"
	UnitFreightTonne RateUnit = "FREIGHT_TONNE" // W/M: the greater of gross tonnes and cubic metres
	UnitPiece        RateUnit = "PIECE"
	UnitDocument     RateUnit = "DOCUMENT"
)

func (u RateUnit) Valid() bool {
	switch u {
	case UnitKgChargeable, UnitKgGross, UnitCBM, UnitFreightTonne, UnitPiece, UnitDocument:
		return true
	}
	return false
}

// Base is how many base units make one unit the rate is stated per (a kilogram, a cubic metre, a W/M, a piece).
func (u RateUnit) Base() int64 {
	switch u {
	case UnitKgChargeable, UnitKgGross, UnitFreightTonne:
		return 1000
	case UnitCBM:
		return 1_000_000
	}
	return 1
}

// Mode is the transport mode a charge code, a rate entry or an evaluation is for. S2 rates AIR and SEA (LCL).
type Mode string

const (
	ModeAir Mode = "AIR"
	ModeSea Mode = "SEA"
)

func (m Mode) Valid() bool { return m == ModeAir || m == ModeSea }

// ChargeCodeVersion is one immutable state of a code. Version 0 is the starter list's entry, never stored.
type ChargeCodeVersion struct {
	Version    int
	NameZH     string
	NameEN     string
	Group      ChargeGroup
	Calculator Calculator // the default a rate line starts from
	Unit       RateUnit   // the default unit of a PER_UNIT code
	Modes      []Mode
	Active     bool
	SetBy      string
	SetAt      time.Time
}

// AppliesTo reports whether the code may be used for the mode.
func (v ChargeCodeVersion) AppliesTo(m Mode) bool { return slices.Contains(v.Modes, m) }

// ChargeCodeInput is what defining or revising a code states.
type ChargeCodeInput struct {
	NameZH     string
	NameEN     string
	Group      ChargeGroup
	Calculator Calculator
	Unit       RateUnit
	Modes      []Mode
}

var (
	ErrInvalidChargeCode = errors.New("rate: invalid charge code")
	ErrChargeCodeUnknown = errors.New("rate: RATE_CHARGE_CODE_UNLISTED: the charge code is not in the tenant's catalogue")
	ErrChargeCodeExists  = errors.New("rate: RATE_CHARGE_CODE_DEFINED: the charge code is already in the tenant's catalogue")
)

var chargeCodeForm = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,15}$`)

// ValidChargeCode reports whether code has the form of a listed charge code: two to sixteen upper-case letters,
// digits or underscores starting with a letter (the form of service-level codes, C8).
func ValidChargeCode(code ChargeCode) bool { return chargeCodeForm.MatchString(string(code)) }

// StarterChargeCodes is the built-in starter list for the launch lines (China export air consolidation and export
// ocean LCL): every tenant's catalogue starts with it.
var StarterChargeCodes = map[ChargeCode]ChargeCodeVersion{
	"AIR_FREIGHT":      starter("空运费", "Air freight", GroupFreight, CalcPerUnit, UnitKgChargeable, ModeAir),
	"FUEL_SURCHARGE":   starter("燃油附加费", "Fuel surcharge", GroupSurcharge, CalcPerUnit, UnitKgChargeable, ModeAir),
	"SECURITY_CHARGE":  starter("安检附加费", "Security surcharge", GroupSurcharge, CalcPerUnit, UnitKgChargeable, ModeAir),
	"THC":              starter("起运地码头操作费", "Origin terminal handling", GroupOrigin, CalcPerUnit, UnitKgChargeable, ModeAir, ModeSea),
	"HANDLING":         starter("操作费", "Handling", GroupOrigin, CalcFlat, UnitNone, ModeAir, ModeSea),
	"CFS_HANDLING":     starter("拼箱站操作费", "CFS handling", GroupOrigin, CalcPerUnit, UnitFreightTonne, ModeSea),
	"DOC_FEE":          starter("文件费", "Documentation fee", GroupDocumentation, CalcFlat, UnitNone, ModeAir, ModeSea),
	"CUSTOMS_CLEARING": starter("报关费", "Customs clearance", GroupCustoms, CalcFlat, UnitNone, ModeAir, ModeSea),
	"PICKUP":           starter("提货费", "Pickup", GroupOrigin, CalcFlat, UnitNone, ModeAir, ModeSea),
	"OCEAN_FREIGHT":    starter("海运费", "Ocean freight", GroupFreight, CalcPerUnit, UnitFreightTonne, ModeSea),
	"DEST_CHARGES":     starter("目的港代收费用", "Destination charges collected for agents", GroupDestination, CalcFlat, UnitNone, ModeAir, ModeSea),
	"DISCOUNT":         starter("折扣", "Discount", GroupDiscount, CalcFlat, UnitNone, ModeAir, ModeSea),
}

func starter(zh, en string, g ChargeGroup, c Calculator, u RateUnit, modes ...Mode) ChargeCodeVersion {
	return ChargeCodeVersion{NameZH: zh, NameEN: en, Group: g, Calculator: c, Unit: u, Modes: modes, Active: true, SetBy: "starter list"}
}

// ChargeCodeEntry is a code with its current version, as the catalogue lists it.
type ChargeCodeEntry struct {
	Code    ChargeCode
	Current ChargeCodeVersion
}

// ChargeCodeAggregate is a tenant's charge code: its current version and the version written since it was loaded.
type ChargeCodeAggregate struct {
	tenant  TenantID
	code    ChargeCode
	current ChargeCodeVersion
	defined bool // a stored version or a starter entry exists
	added   *ChargeCodeVersion
	pending []Event
}

// Event types of the catalogue.
const (
	EventChargeCodeDefined     = "ChargeCodeDefined.v1"
	EventChargeCodeRevised     = "ChargeCodeRevised.v1"
	EventChargeCodeDeactivated = "ChargeCodeDeactivated.v1"
	EventChargeCodeReactivated = "ChargeCodeReactivated.v1"
)

// RestoreChargeCode rebuilds a code from its latest stored version, or from the starter list (found false and a
// starter entry), or as not defined at all.
func RestoreChargeCode(tenant TenantID, code ChargeCode, latest ChargeCodeVersion, found bool) *ChargeCodeAggregate {
	c := &ChargeCodeAggregate{tenant: tenant, code: code}
	switch {
	case found:
		c.current, c.defined = latest, true
	default:
		if s, ok := StarterChargeCodes[code]; ok {
			c.current, c.defined = s, true
		}
	}
	return c
}

func (c *ChargeCodeAggregate) Code() ChargeCode           { return c.code }
func (c *ChargeCodeAggregate) Defined() bool              { return c.defined }
func (c *ChargeCodeAggregate) Current() ChargeCodeVersion { return c.current }
func (c *ChargeCodeAggregate) Added() *ChargeCodeVersion  { return c.added }
func (c *ChargeCodeAggregate) Pending() []Event           { return append([]Event(nil), c.pending...) }
func (c *ChargeCodeAggregate) Acknowledge()               { c.pending = nil }
func (c *ChargeCodeAggregate) Tenant() TenantID           { return c.tenant }
func (c *ChargeCodeAggregate) Version() int               { return c.current.Version }
func (c *ChargeCodeAggregate) expected(version int) error {
	if version != c.current.Version {
		return fmt.Errorf("%w: charge code %s is at version %d, not %d", ErrConflict, c.code, c.current.Version, version)
	}
	return nil
}

// Define adds a code not yet in the tenant's catalogue.
func (c *ChargeCodeAggregate) Define(in ChargeCodeInput, by string, at time.Time) error {
	if !ValidChargeCode(c.code) {
		return fmt.Errorf("%w: %q is not two to sixteen upper-case letters, digits or underscores starting with a letter", ErrInvalidChargeCode, c.code)
	}
	if c.defined {
		return fmt.Errorf("%w: %s", ErrChargeCodeExists, c.code)
	}
	return c.write(in, true, EventChargeCodeDefined, by, at)
}

// Revise changes a defined code's names, group, defaults or modes at the expected version; the code never changes.
func (c *ChargeCodeAggregate) Revise(version int, in ChargeCodeInput, by string, at time.Time) error {
	if !c.defined {
		return fmt.Errorf("%w: %s", ErrChargeCodeUnknown, c.code)
	}
	if err := c.expected(version); err != nil {
		return err
	}
	if !c.current.Active {
		return fmt.Errorf("%w: charge code %s is inactive; reactivate it first", ErrInvalidTransition, c.code)
	}
	return c.write(in, true, EventChargeCodeRevised, by, at)
}

// Deactivate stops new quote lines, rate lines and charges from using the code; existing ones keep it.
func (c *ChargeCodeAggregate) Deactivate(version int, by string, at time.Time) error {
	return c.activate(version, false, EventChargeCodeDeactivated, by, at)
}

// Reactivate lets the code be used again.
func (c *ChargeCodeAggregate) Reactivate(version int, by string, at time.Time) error {
	return c.activate(version, true, EventChargeCodeReactivated, by, at)
}

func (c *ChargeCodeAggregate) activate(version int, active bool, kind, by string, at time.Time) error {
	if !c.defined {
		return fmt.Errorf("%w: %s", ErrChargeCodeUnknown, c.code)
	}
	if err := c.expected(version); err != nil {
		return err
	}
	if c.current.Active == active {
		return fmt.Errorf("%w: charge code %s is already %s", ErrInvalidTransition, c.code, map[bool]string{true: "active", false: "inactive"}[active])
	}
	cur := c.current
	return c.write(ChargeCodeInput{NameZH: cur.NameZH, NameEN: cur.NameEN, Group: cur.Group, Calculator: cur.Calculator, Unit: cur.Unit,
		Modes: cur.Modes}, active, kind, by, at)
}

func (c *ChargeCodeAggregate) write(in ChargeCodeInput, active bool, kind, by string, at time.Time) error {
	if c.added != nil {
		return fmt.Errorf("%w: one change of a charge code per command", ErrInvalidTransition)
	}
	if err := in.check(); err != nil {
		return err
	}
	if strings.TrimSpace(by) == "" || at.IsZero() {
		return fmt.Errorf("%w: a change needs an actor and a time", ErrInvalidChargeCode)
	}
	modes := append([]Mode(nil), in.Modes...)
	slices.Sort(modes)
	v := ChargeCodeVersion{Version: c.current.Version + 1, NameZH: strings.TrimSpace(in.NameZH), NameEN: strings.TrimSpace(in.NameEN),
		Group: in.Group, Calculator: in.Calculator, Unit: in.Unit, Modes: slices.Compact(modes), Active: active, SetBy: by, SetAt: at.UTC()}
	c.current, c.defined, c.added = v, true, &v
	c.pending = append(c.pending, Event{Type: kind, At: v.SetAt, TenantID: c.tenant, SubjectID: string(c.code), Version: v.Version})
	return nil
}

func (in ChargeCodeInput) check() error {
	name := func(s string) bool {
		s = strings.TrimSpace(s)
		return s != "" && utf8.RuneCountInString(s) <= 100
	}
	switch {
	case !name(in.NameZH) || !name(in.NameEN):
		return fmt.Errorf("%w: a Chinese and an English name of 1 to 100 characters", ErrInvalidChargeCode)
	case !in.Group.Valid():
		return fmt.Errorf("%w: group %q", ErrInvalidChargeCode, in.Group)
	case !in.Calculator.Valid():
		return fmt.Errorf("%w: calculator %q", ErrInvalidChargeCode, in.Calculator)
	case in.Calculator == CalcPerUnit && !in.Unit.Valid():
		return fmt.Errorf("%w: a PER_UNIT code needs a rate unit", ErrInvalidChargeCode)
	case in.Calculator != CalcPerUnit && in.Unit != UnitNone:
		return fmt.Errorf("%w: only a PER_UNIT code has a rate unit", ErrInvalidChargeCode)
	case len(in.Modes) == 0:
		return fmt.Errorf("%w: a code applies to AIR, SEA or both", ErrInvalidChargeCode)
	}
	for _, m := range in.Modes {
		if !m.Valid() {
			return fmt.Errorf("%w: mode %q", ErrInvalidChargeCode, m)
		}
	}
	return nil
}
