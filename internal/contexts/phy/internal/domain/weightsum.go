package domain

import "go.idp.xyz/idp-cargo/internal/kernel"

// quantitySum adds quantities of one dimension, a house's weights or a house's volumes. Quantities in one unit are
// added as they are and the sum keeps that unit. Quantities in different units are each converted to the base unit
// (grams, cubic centimetres) by convert, rounding up as toGrams and toCubicCm do, and the sum is published in the base
// unit; converting each quantity on its own makes the sum independent of the order they are added in, and the sum
// exceeds the exact figure by less than one base unit for each quantity.
//
// A quantity that cannot be converted makes a mixed sum a quantity in that unit: the unit that sorts first of the
// unconvertible ones whatever the order, with the amounts in that unit added and at least 1. A reader that converts,
// such as FIN's Grams and CubicCm, refuses a quantity in a unit it does not know, whereas a zero would read as a figure
// of nothing and the base unit with the quantity left out as a wrong one. A quantity PHY cannot convert is one in a
// unit it does not know, one without a unit but with an amount, and one with a negative amount (never produced; a
// negative amount in a known unit is reported in that unit). A single unconvertible unit is added as it is, as it
// always was; so are amounts of one unit, which are not capped (as before V2.84). In a mixed sum every conversion and
// the sum of them are capped at the largest int64. Amounts of zero in different units are a zero in the base unit.
type quantitySum struct {
	unit       kernel.UnitCode
	amount     int64
	any        bool
	mixed      bool
	base       int64
	hasUnknown bool
	unknown    kernel.UnitCode // the smallest unit that cannot be converted
	unknownBy  int64           // the amounts in that unit
}

func (s *quantitySum) add(q kernel.Quantity, convert func(kernel.Quantity) (int64, bool)) {
	if q.Unit == "" && q.Amount == 0 {
		return
	}
	if !s.any {
		s.unit, s.any = q.Unit, true
	} else if s.unit != q.Unit {
		s.mixed = true
	}
	s.amount += q.Amount
	if g, ok := convert(q); ok {
		s.base = addCapped(s.base, g)
		return
	}
	switch {
	case !s.hasUnknown || q.Unit < s.unknown:
		s.unknown, s.unknownBy, s.hasUnknown = q.Unit, q.Amount, true
	case q.Unit == s.unknown:
		s.unknownBy = addCapped(s.unknownBy, q.Amount)
	}
}

func (s quantitySum) quantity(base kernel.UnitCode) kernel.Quantity {
	switch {
	case !s.mixed:
		return kernel.Quantity{Unit: s.unit, Amount: s.amount}
	case s.hasUnknown:
		return kernel.Quantity{Unit: s.unknown, Amount: max(s.unknownBy, 1)}
	}
	return kernel.Quantity{Unit: base, Amount: s.base}
}

// WeightSum adds the weights of one house or one container: one unit as it is, different units in grams (GRM). The zero
// value is an empty sum.
type WeightSum struct{ s quantitySum }

// Add adds a quantity; an empty one (no unit, no amount) is nothing.
func (w *WeightSum) Add(q kernel.Quantity) { w.s.add(q, toGrams) }

// Quantity is the sum: zero when nothing was added.
func (w WeightSum) Quantity() kernel.Quantity { return w.s.quantity("GRM") }

// VolumeSum adds the volumes of one house or one container as WeightSum adds weights: one unit as it is, different
// units in cubic centimetres (CMQ). A volume that is not known for a line is not added: the caller says the sum is not
// known.
type VolumeSum struct{ s quantitySum }

// Add adds a quantity; an empty one (no unit, no amount) is nothing.
func (v *VolumeSum) Add(q kernel.Quantity) { v.s.add(q, toCubicCm) }

// Quantity is the sum: zero when nothing was added.
func (v VolumeSum) Quantity() kernel.Quantity { return v.s.quantity("CMQ") }
