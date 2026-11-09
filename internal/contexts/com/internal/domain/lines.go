package domain

import (
	"fmt"
	"time"
)

// LineVersion is a version of an order line written after the order was recorded (decision C10). Version 1
// adds a line, numbered after the lines the order has; a later version corrects what the customer declared
// for the line. Versions are never changed; the latest version of a line is the line.
type LineVersion struct {
	OrderLine
	Version   int64
	ChangedBy string
	ChangedAt time.Time // the business time of the change
	EnteredAt time.Time // when COM recorded it (T1)
	Reason    string
}

// lineIndex is the current state of an order's lines: the lines as recorded, with their changes applied.
type lineIndex struct {
	lines      []OrderLine
	versions   []int64
	lastChange []time.Time
}

// applyLineChanges builds the current lines from the recorded lines and the changes in the order they were
// written. Each change must follow on its line: an added line is numbered after the existing ones and a
// correction is the next version of its line, not earlier than the line's last change.
func applyLineChanges(recorded []OrderLine, recordedAt time.Time, changes []LineVersion) (lineIndex, error) {
	ix := lineIndex{lines: append([]OrderLine(nil), recorded...)}
	for range recorded {
		ix.versions = append(ix.versions, 1)
		ix.lastChange = append(ix.lastChange, recordedAt)
	}
	for _, c := range changes {
		switch {
		case blank(c.ChangedBy), !text(c.Reason), c.ChangedAt.Before(recordedAt), !c.valid():
			return lineIndex{}, fmt.Errorf("%w: change of line %d to version %d", ErrInvalidOrder, c.LineNo, c.Version)
		case c.Version == 1 && c.LineNo == len(ix.lines)+1:
			ix.lines = append(ix.lines, c.OrderLine)
			ix.versions = append(ix.versions, 1)
			ix.lastChange = append(ix.lastChange, c.ChangedAt)
		case c.Version > 1 && c.LineNo >= 1 && c.LineNo <= len(ix.lines) && c.Version == ix.versions[c.LineNo-1]+1 &&
			!c.ChangedAt.Before(ix.lastChange[c.LineNo-1]):
			ix.lines[c.LineNo-1], ix.versions[c.LineNo-1], ix.lastChange[c.LineNo-1] = c.OrderLine, c.Version, c.ChangedAt
		default:
			return lineIndex{}, fmt.Errorf("%w: change of line %d to version %d does not follow", ErrInvalidOrder, c.LineNo, c.Version)
		}
	}
	return ix, nil
}

func (l OrderLine) valid() bool {
	return l.LineNo >= 1 && text(l.Description) && l.PackageType.Valid() && l.Measure.valid()
}

// Lines returns the order's current lines: the recorded lines and those added later, at their latest version.
func (o *CustomerOrder) Lines() []OrderLine { return append([]OrderLine(nil), o.lines.lines...) }

// Line returns the current version of a line and its version number.
func (o *CustomerOrder) Line(no int) (OrderLine, int64, bool) {
	if no < 1 || no > len(o.lines.lines) {
		return OrderLine{}, 0, false
	}
	return o.lines.lines[no-1], o.lines.versions[no-1], true
}

// AmendLine corrects what the customer declared for a line (C10): it writes the line's next version, with a
// reason, and needs no new quote. A difference measured at receipt is not a correction; FUL records it. The
// corrected line may not fall below what its active obligations commit; while it has any, its package type
// and units stay. To go lower, the obligations are revoked or amended first (C6).
func (o *CustomerOrder) AmendLine(no int, desc string, pt PackageType, m Measure, reason, by string, at, entered time.Time) error {
	if o.state.Status == OrderCancelled {
		return ErrInvalidTransition
	}
	current, version, ok := o.Line(no)
	if !ok {
		return fmt.Errorf("%w: no line %d", ErrInvalidChange, no)
	}
	next := OrderLine{LineNo: no, Description: desc, PackageType: pt, Measure: m}
	switch {
	case !next.valid():
		return fmt.Errorf("%w: line %d", ErrInvalidOrder, no)
	case blank(by), !text(reason), at.IsZero(), entered.IsZero(), at.Before(o.lines.lastChange[no-1]):
		return ErrInvalidChange
	case next == current:
		return fmt.Errorf("%w: line %d already reads so", ErrInvalidChange, no)
	}
	committed, active, err := o.committed(no)
	if err != nil {
		return err
	}
	if active {
		if pt != current.PackageType || m.GrossWeight.Unit != current.Measure.GrossWeight.Unit ||
			m.DeclaresVolume() != current.Measure.DeclaresVolume() || m.Volume.Unit != current.Measure.Volume.Unit {
			return fmt.Errorf("%w: line %d has active obligations, so its package type and units stay", ErrInvalidChange, no)
		}
		left, err := m.sub(committed)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidChange, err)
		}
		if left.negative() {
			return fmt.Errorf("%w: line %d would fall below what its active obligations commit", ErrInvalidChange, no)
		}
	}
	o.change(LineVersion{OrderLine: next, Version: version + 1, ChangedBy: by, ChangedAt: at.UTC(), EnteredAt: entered.UTC(), Reason: reason},
		eventLineAmended, at)
	return nil
}

// AddLine adds a line to an order after it was recorded, such as goods the customer added (C10). It is
// numbered after the existing lines.
func (o *CustomerOrder) AddLine(desc string, pt PackageType, m Measure, reason, by string, at, entered time.Time) error {
	if o.state.Status == OrderCancelled {
		return ErrInvalidTransition
	}
	next := OrderLine{LineNo: len(o.lines.lines) + 1, Description: desc, PackageType: pt, Measure: m}
	switch {
	case !next.valid():
		return fmt.Errorf("%w: line %d", ErrInvalidOrder, next.LineNo)
	case blank(by), !text(reason), at.IsZero(), entered.IsZero(), at.Before(o.state.RecordedAt):
		return ErrInvalidChange
	}
	o.change(LineVersion{OrderLine: next, Version: 1, ChangedBy: by, ChangedAt: at.UTC(), EnteredAt: entered.UTC(), Reason: reason},
		eventLineAdded, at)
	return nil
}

func (o *CustomerOrder) change(v LineVersion, kind string, at time.Time) {
	o.state.LineChanges = append(o.state.LineChanges, v)
	ix, err := applyLineChanges(o.state.Lines, o.state.RecordedAt, o.state.LineChanges)
	if err != nil { // the checks above admit only changes that follow
		panic(err)
	}
	o.lines = ix
	o.state.Status = o.derivedStatus()
	e := o.event(kind, at)
	e.LineNo, e.LineVersion = v.LineNo, v.Version
	o.pending = append(o.pending, e)
}

// committed returns what the active obligations of a line commit together, and whether it has any.
func (o *CustomerOrder) committed(no int) (Measure, bool, error) {
	var total Measure
	active := false
	for _, ob := range o.current() {
		if ob.LineNo != no || ob.Status != ObligationActive {
			continue
		}
		if !active {
			total, active = ob.Measure, true
			continue
		}
		var err error
		if total, err = total.add(ob.Measure); err != nil {
			return Measure{}, false, err
		}
	}
	return total, active, nil
}

// AddedLineVersions returns the line versions written since the order was recorded or restored, for
// persistence.
func (o *CustomerOrder) AddedLineVersions() []LineVersion {
	return append([]LineVersion(nil), o.state.LineChanges[o.storedChanges:]...)
}
