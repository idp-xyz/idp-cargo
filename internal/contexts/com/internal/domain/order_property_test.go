package domain

import (
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"testing"
	"testing/quick"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// lifecycleScript is a random order and a random run of commands against it: allocations, revocations,
// amendments, reservations, releases and cancellations, valid or not. Invalid ones include missing lines
// and obligations, non-positive packs and weights, wrong units, used ids, other shipments and stale
// versions.
type lifecycleScript struct {
	Lines []Measure
	Steps []lifecycleStep
}

type lifecycleStep struct {
	Kind     int // 0-3 allocate, 4-5 revoke, 6 amend, 7-8 reserve, 9-10 release, 11 cancel
	LineNo   int
	Measure  Measure
	Target   int   // counts back from the latest obligation allocated so far; beyond the first, an unknown one
	Version  int64 // 0 for the target's latest version
	Shipment int
}

func (lifecycleScript) Generate(r *rand.Rand, size int) reflect.Value {
	var s lifecycleScript
	for n := 1 + r.Intn(3); len(s.Lines) < n; {
		m := Measure{Packs: 1 + r.Int63n(20), GrossWeight: kg(1 + r.Int63n(1000))}
		if r.Intn(2) == 0 {
			m.Volume = dm3(1 + r.Int63n(5000))
		}
		s.Lines = append(s.Lines, m)
	}
	for n := 1 + r.Intn(4*size); len(s.Steps) < n; {
		st := lifecycleStep{Kind: r.Intn(12), LineNo: r.Intn(len(s.Lines) + 2), Target: r.Intn(8), Version: int64(r.Intn(3)), Shipment: r.Intn(3)}
		if r.Intn(100) == 0 {
			st.Kind = 11
		} else if st.Kind == 11 {
			st.Kind = r.Intn(11)
		}
		st.Measure = Measure{Packs: r.Int63n(8) - 1, GrossWeight: kg(r.Int63n(400) - 20)}
		if r.Intn(2) == 0 {
			st.Measure.Volume = dm3(r.Int63n(2500))
		}
		if r.Intn(15) == 0 {
			st.Measure.GrossWeight.Unit = "LBR"
		}
		s.Steps = append(s.Steps, st)
	}
	return reflect.ValueOf(s)
}

// lifecycleModel is an independent account of what the order must accept.
type lifecycleModel struct {
	lines       []Measure
	obligations map[ObligationID]*modelObligation
	cancelled   bool
}

type modelObligation struct {
	lineNo  int
	measure Measure
	version int64
	active  bool
	holders []string // shipments holding the obligation, in the order they reserved it (F1)
}

func (ob *modelObligation) holds(shipment string) bool { return slices.Contains(ob.holders, shipment) }

func (m *lifecycleModel) committed(lineNo int) (packs, weight, volume int64) {
	for _, ob := range m.obligations {
		if ob.active && ob.lineNo == lineNo {
			packs, weight, volume = packs+ob.measure.Packs, weight+ob.measure.GrossWeight.Amount, volume+ob.measure.Volume.Amount
		}
	}
	return packs, weight, volume
}

// fits reports whether measure x fits line lineNo once the active obligation without, if any, is revoked.
func (m *lifecycleModel) fits(lineNo int, x Measure, without ObligationID) bool {
	if lineNo < 1 || lineNo > len(m.lines) || x.Packs <= 0 || x.GrossWeight.Amount <= 0 || x.GrossWeight.Unit != "KGM" ||
		(x.Volume != kernel.Quantity{} && x.Volume.Amount <= 0) {
		return false
	}
	l := m.lines[lineNo-1]
	if x.DeclaresVolume() != l.DeclaresVolume() {
		return false
	}
	packs, weight, volume := m.committed(lineNo)
	if ob := m.obligations[without]; ob != nil && ob.active && ob.lineNo == lineNo {
		packs, weight, volume = packs-ob.measure.Packs, weight-ob.measure.GrossWeight.Amount, volume-ob.measure.Volume.Amount
	}
	return packs+x.Packs <= l.Packs && weight+x.GrossWeight.Amount <= l.GrossWeight.Amount && volume+x.Volume.Amount <= l.Volume.Amount
}

func (m *lifecycleModel) status() OrderStatus {
	if m.cancelled {
		return OrderCancelled
	}
	active, full := false, true
	for no, l := range m.lines {
		packs, _, _ := m.committed(no + 1)
		active, full = active || packs > 0, full && packs == l.Packs
	}
	switch {
	case !active:
		return OrderRecorded
	case full:
		return OrderAllocated
	}
	return OrderPartiallyAllocated
}

func (m *lifecycleModel) revocable(id ObligationID) bool {
	ob := m.obligations[id]
	return !m.cancelled && ob != nil && ob.active && len(ob.holders) == 0
}

// TestLifecycleProperties checks, for random scripts, that the order accepts exactly what the model
// accepts, that a refused command changes nothing, that every accepted command emits its events, that
// active obligations never exceed their line, that a shipment's obligation is never revoked, that the
// status follows the obligations, and that the stored order restores to the same state.
func TestLifecycleProperties(t *testing.T) {
	property := func(s lifecycleScript) bool {
		lines := make([]OrderLine, len(s.Lines))
		for i, m := range s.Lines {
			lines[i] = line(fmt.Sprintf("Item %d", i+1), "PK", m)
		}
		o, err := RecordOrder(draft(lines...), recordAt)
		if err != nil {
			t.Log(err)
			return false
		}
		m := &lifecycleModel{lines: s.Lines, obligations: map[ObligationID]*modelObligation{}}
		var ids []ObligationID
		events := 1
		for i, st := range s.Steps {
			when := recordAt.Add(time.Duration(i) * time.Minute)
			target := ObligationID("OB-unknown")
			if st.Target < len(ids) {
				target = ids[len(ids)-1-st.Target]
			}
			version := st.Version
			if ob := m.obligations[target]; ob != nil && version == 0 {
				version = ob.version
			}
			shipment := fmt.Sprintf("S-%d", st.Shipment)
			newID := ObligationID(fmt.Sprintf("OB-%d", i))
			before, pending := o.State(), len(o.Pending())
			var ok bool
			var apply func()
			switch {
			case st.Kind <= 3:
				ok = !m.cancelled && m.status() != OrderAllocated && m.fits(st.LineNo, st.Measure, "")
				err = o.Allocate(st.LineNo, newID, st.Measure, "planner-1", when)
				apply = func() {
					m.obligations[newID] = &modelObligation{lineNo: st.LineNo, measure: st.Measure, version: 1, active: true}
					ids = append(ids, newID)
					events++
				}
			case st.Kind <= 5:
				ok = m.revocable(target)
				err = o.Revoke(target, "planner-1", when)
				apply = func() {
					m.obligations[target].active, m.obligations[target].version = false, 2
					events++
				}
			case st.Kind == 6:
				ok = m.revocable(target) && m.fits(m.obligations[target].lineNo, st.Measure, target)
				err = o.Amend(target, newID, st.Measure, "planner-1", when)
				apply = func() {
					old := m.obligations[target]
					old.active, old.version = false, 2
					m.obligations[newID] = &modelObligation{lineNo: old.lineNo, measure: st.Measure, version: 1, active: true}
					ids = append(ids, newID)
					events += 2
				}
			case st.Kind <= 8:
				ob := m.obligations[target]
				ok = ob != nil && ob.active && !ob.holds(shipment) && version == ob.version
				err = o.Reserve(target, version, shipment, "fulfilment", when)
				apply = func() { ob.holders = append(ob.holders, shipment); events++ }
			case st.Kind <= 10:
				ob := m.obligations[target]
				ok = ob != nil && ob.holds(shipment) && version == 1
				err = o.ReleaseReservation(target, version, shipment, "fulfilment", when)
				apply = func() {
					ob.holders = slices.DeleteFunc(slices.Clone(ob.holders), func(h string) bool { return h == shipment })
					events++
				}
			default:
				ok = !m.cancelled
				for _, ob := range m.obligations {
					ok = ok && (!ob.active || len(ob.holders) == 0)
				}
				err = o.Cancel("sales-1", when)
				apply = func() {
					for _, ob := range m.obligations {
						if ob.active {
							ob.active, ob.version = false, 2
							events++
						}
					}
					m.cancelled = true
					events++
				}
			}
			if ok != (err == nil) {
				t.Logf("step %d %+v (target %s version %d): model accepts %v, order says %v", i, st, target, version, ok, err)
				return false
			}
			if err != nil {
				if !reflect.DeepEqual(o.State(), before) || len(o.Pending()) != pending {
					t.Logf("step %d %+v: refused, but the order changed", i, st)
					return false
				}
				continue
			}
			apply()
		}
		st := o.State()
		for no := range s.Lines {
			packs, weight, volume := m.committed(no + 1)
			l := s.Lines[no]
			want := Measure{Packs: l.Packs - packs, GrossWeight: kg(l.GrossWeight.Amount - weight)}
			if l.DeclaresVolume() {
				want.Volume = dm3(l.Volume.Amount - volume)
			}
			if remaining, err := o.Remaining(no + 1); err != nil || remaining != want || remaining.negative() {
				t.Logf("line %d: remaining %+v, %v; want %+v", no+1, remaining, err, want)
				return false
			}
		}
		for id, ob := range m.obligations {
			latest, _ := o.Latest(id)
			r, held := o.Held(id)
			var holders []string
			for _, h := range o.Holders(id) {
				holders = append(holders, h.ShipmentID)
			}
			if latest.Version != ob.version || (latest.Status == ObligationActive) != ob.active || held != (len(ob.holders) > 0) ||
				!slices.Equal(holders, ob.holders) || (held && (r.ShipmentID != ob.holders[0] || latest.Status != ObligationActive)) {
				t.Logf("obligation %s: latest %+v, held %v %+v; model %+v", id, latest, held, r, *ob)
				return false
			}
		}
		restored, err := RestoreOrder(st)
		return st.Status == m.status() && err == nil && reflect.DeepEqual(restored.State(), st) && len(o.Pending()) == events
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 500}); err != nil {
		t.Fatal(err)
	}
}
