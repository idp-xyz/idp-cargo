package domain

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// A planned load unit is an entity of the consolidation (PLAN_LOAD_UNIT, REMOVE_PLANNED_LOAD_UNIT): a unit the
// consolidation's cargo is to be loaded into, with the limits of its type frozen from the reference data with the
// edition (backlog PB8). V0.3 plans containers on own-container LCL consolidations (SEA_LCL_OWN), which are stuffed by
// the forwarder; a co-loader's consolidation (SEA_LCL_COLOAD) has none, its stuffing being the co-loader's; ULD and
// loose units come with ULD build-up. Whether a unit is in use or closed is a fact of its load executions; the unit
// itself is only planned or removed.
const (
	UnitContainer LoadUnitKind = "CONTAINER"

	UnitPlanned UnitStatus = "PLANNED"
	UnitRemoved UnitStatus = "REMOVED"
)

type (
	LoadUnitKind string
	UnitStatus   string
)

var (
	ErrInvalidLoadUnit = errors.New("phy: invalid load unit")
	ErrLoadUnitKind    = errors.New("phy: PHY_LOAD_UNIT_KIND: the consolidation takes no load unit of that kind")
	ErrLoadUnitInUse   = errors.New("phy: PHY_LOAD_UNIT_IN_USE: the load unit has its stuffing")
)

// gramsPer and cubicCmPer convert the weights and volumes a container's limits are checked against (UN/ECE
// Recommendation 20 codes).
var (
	gramsPer   = map[kernel.UnitCode]int64{"GRM": 1, "KGM": 1000, "TNE": 1000000}
	cubicCmPer = map[kernel.UnitCode]int64{"CMQ": 1, "DMQ": 1000, "LTR": 1000, "MTQ": 1000000}
)

// checkable reports whether a line's weight, and its volume if it gives one, convert to grams and cubic centimetres.
func checkable(l LoadLine) bool {
	_, weight := gramsPer[l.GrossWeight.Unit]
	_, volume := cubicCmPer[l.Volume.Unit]
	return weight && (l.Volume == (kernel.Quantity{}) || volume)
}

// thousandths writes an amount in thousandths as a decimal, such as 32250.000 for 32,250,000 grams in kilograms.
func thousandths(n int64) string { return fmt.Sprintf("%d.%03d", n/1000, n%1000) }

// scaled multiplies an amount by a factor, capped at math.MaxInt64; addCapped adds two amounts likewise.
func scaled(amount, factor int64) int64 {
	if amount > 0 && factor > 0 && amount > math.MaxInt64/factor {
		return math.MaxInt64
	}
	return amount * factor
}

func addCapped(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

var sizeTypeCode = regexp.MustCompile(`^[0-9A-Z]{2}[A-Z][0-9A-Z]$`)

// UnitLimits are a load unit type's limits as the reference data gives them: the ISO 6346 size-type, its TEU in
// tenths, maximum gross, tare and capacity, and the edition they come from. They are defaults: a container's own
// plate data is SEA's.
type UnitLimits struct {
	TypeCode    string
	TEUTenths   int
	MaxGrossKg  int64
	TareKg      int64
	CapacityCM3 int64
	Edition     string
}

func (l UnitLimits) valid() bool {
	return sizeTypeCode.MatchString(l.TypeCode) && l.TEUTenths > 0 && l.TEUTenths <= 30 && l.TareKg > 0 && l.MaxGrossKg > l.TareKg &&
		l.CapacityCM3 > 0 && identifier(l.Edition)
}

// PlannedLoadUnit is a unit planned for the consolidation, numbered in the order planned.
type PlannedLoadUnit struct {
	No               int
	Kind             LoadUnitKind
	Limits           UnitLimits
	Status           UnitStatus
	PlannedBy        string
	PlannedAt        time.Time
	PlannedEnteredAt time.Time
	RemoveReason     string
	RemovedBy        string
	RemovedAt        time.Time
	RemoveEnteredAt  time.Time
}

// PlanLoadUnit plans a load unit of a type the reference data knows: a CONTAINER on an own-container LCL
// consolidation that is OPEN or CLOSED. It answers the unit's number.
func (c *Consolidation) PlanLoadUnit(kind LoadUnitKind, limits UnitLimits, by string, at time.Time) (int, error) {
	if s := c.Current().State; s != StateOpen && s != StateClosed {
		return 0, ErrInvalidTransition
	}
	if kind != UnitContainer || c.state.Kind != KindSeaLCLOwn {
		return 0, fmt.Errorf("%w: a %s consolidation, a %s unit; containers are planned on SEA_LCL_OWN, ULD and loose units come with "+
			"ULD build-up", ErrLoadUnitKind, c.state.Kind, kind)
	}
	if !limits.valid() {
		return 0, fmt.Errorf("%w: limits of %q", ErrInvalidLoadUnit, limits.TypeCode)
	}
	if err := c.changeable(by, at); err != nil {
		return 0, err
	}
	u := PlannedLoadUnit{No: len(c.state.LoadUnits) + 1, Kind: kind, Limits: limits, Status: UnitPlanned, PlannedBy: by, PlannedAt: at.UTC()}
	c.state.LoadUnits = append(c.state.LoadUnits, u)
	c.unitEvent(eventLoadUnitPlanned, u.No, at)
	return u.No, nil
}

// RemovePlannedLoadUnit removes a planned unit with a reason, while the consolidation is OPEN or CLOSED and no
// stuffing is opened on the unit (inUse, read under the consolidation's lock).
func (c *Consolidation) RemovePlannedLoadUnit(no int, reason, by string, at time.Time, inUse bool) error {
	if s := c.Current().State; s != StateOpen && s != StateClosed {
		return ErrInvalidTransition
	}
	if no < 1 || no > len(c.state.LoadUnits) || c.state.LoadUnits[no-1].Status != UnitPlanned {
		return fmt.Errorf("%w: no planned load unit %d", ErrInvalidLoadUnit, no)
	}
	if !text(reason) {
		return fmt.Errorf("%w: a removal gives its reason (500 characters)", ErrInvalidChange)
	}
	if inUse {
		return fmt.Errorf("%w: unit %d", ErrLoadUnitInUse, no)
	}
	if err := c.changeable(by, at); err != nil {
		return err
	}
	u := &c.state.LoadUnits[no-1]
	u.Status, u.RemoveReason, u.RemovedBy, u.RemovedAt = UnitRemoved, reason, by, at.UTC()
	c.removed = append(c.removed, no)
	c.unitEvent(eventLoadUnitRemoved, no, at)
	return nil
}

// unitEvent records a load unit event: the unit is its item; a removal's reason, free text, stays out (rule C3).
func (c *Consolidation) unitEvent(kind string, no int, at time.Time) {
	c.pending = append(c.pending, Event{Type: kind, At: at.UTC(), TenantID: c.state.TenantID, SubjectID: string(c.state.ConsolidationID),
		ConsolidationID: c.state.ConsolidationID, ItemNo: no})
}

// LiveLoadUnits returns the planned units not removed.
func (c *Consolidation) LiveLoadUnits() []PlannedLoadUnit {
	var out []PlannedLoadUnit
	for _, u := range c.state.LoadUnits {
		if u.Status == UnitPlanned {
			out = append(out, u)
		}
	}
	return out
}

// AddedLoadUnits returns the units planned since the consolidation was restored, as they now are; RemovedLoadUnits
// the units removed since then, in the order removed. A unit planned and removed since is in both: the store writes
// it as planned, then its removal.
func (c *Consolidation) AddedLoadUnits() []PlannedLoadUnit {
	return append([]PlannedLoadUnit(nil), c.state.LoadUnits[c.units:]...)
}

func (c *Consolidation) RemovedLoadUnits() []PlannedLoadUnit {
	var out []PlannedLoadUnit
	for _, no := range c.removed {
		out = append(out, c.state.LoadUnits[no-1])
	}
	return out
}

// validUnits checks stored units: numbered from 1, of the consolidation's kind, with valid limits and their removal
// recorded exactly when removed.
func validUnits(st ConsolidationState) bool {
	for i, u := range st.LoadUnits {
		removed := u.Status == UnitRemoved
		if u.No != i+1 || u.Kind != UnitContainer || st.Kind != KindSeaLCLOwn || !u.Limits.valid() ||
			(u.Status != UnitPlanned && !removed) || blank(u.PlannedBy) || u.PlannedAt.IsZero() ||
			removed != text(u.RemoveReason) || removed == blank(u.RemovedBy) || removed == u.RemovedAt.IsZero() ||
			(removed && u.RemovedAt.Before(u.PlannedAt)) {
			return false
		}
	}
	return true
}
