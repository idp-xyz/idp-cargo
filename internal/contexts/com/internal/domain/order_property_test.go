package domain

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// allocationScript is a random order and a random run of allocation attempts against it, including
// attempts on missing lines, with non-positive amounts, in the wrong unit or under a used obligation id.
type allocationScript struct {
	Lines    []int64
	Attempts []allocationAttempt
}

type allocationAttempt struct {
	LineNo int
	Amount int64
	Unit   kernel.UnitCode
	Reuse  bool
}

func (allocationScript) Generate(r *rand.Rand, size int) reflect.Value {
	var s allocationScript
	for n := 1 + r.Intn(4); len(s.Lines) < n; {
		s.Lines = append(s.Lines, 1+r.Int63n(1000))
	}
	for n := 1 + r.Intn(3*size); len(s.Attempts) < n; {
		a := allocationAttempt{LineNo: r.Intn(len(s.Lines) + 2), Amount: r.Int63n(700) - 50, Unit: "KGM", Reuse: r.Intn(8) == 0}
		if r.Intn(10) == 0 {
			a.Unit = "H87"
		}
		s.Attempts = append(s.Attempts, a)
	}
	return reflect.ValueOf(s)
}

// TestAllocationProperties checks, for random scripts, that the model accepts exactly the allocations
// that fit, that a refused allocation changes nothing, that no line is ever over-allocated, that the
// status follows the allocations, and that a stored order restores to the same state.
func TestAllocationProperties(t *testing.T) {
	property := func(s allocationScript) bool {
		lines := make([]OrderLine, len(s.Lines))
		for i, amount := range s.Lines {
			lines[i] = line(fmt.Sprintf("Item %d", i+1), kg(amount))
		}
		o, err := RecordOrder(draft(lines...), recordAt)
		if err != nil {
			t.Log(err)
			return false
		}
		allocated := make([]int64, len(s.Lines)+1)
		used := map[ObligationID]bool{}
		for i, a := range s.Attempts {
			id := ObligationID(fmt.Sprintf("OB-%d", i))
			if a.Reuse && i > 0 {
				id = ObligationID(fmt.Sprintf("OB-%d", i-1))
			}
			fits := a.LineNo >= 1 && a.LineNo <= len(s.Lines) && a.Unit == "KGM" && a.Amount > 0 &&
				allocated[a.LineNo]+a.Amount <= s.Lines[a.LineNo-1] && !used[id]
			before := o.State()
			err := o.Allocate(a.LineNo, id, kernel.Quantity{Unit: a.Unit, Amount: a.Amount}, "planner-1", recordAt.Add(time.Duration(i)*time.Minute))
			if fits != (err == nil) {
				t.Logf("attempt %d %+v: fits %v, err %v", i, a, fits, err)
				return false
			}
			if err != nil {
				if !reflect.DeepEqual(o.State(), before) {
					return false
				}
				continue
			}
			allocated[a.LineNo] += a.Amount
			used[id] = true
		}
		st := o.State()
		full := true
		for no, amount := range s.Lines {
			remaining, err := o.Remaining(no + 1)
			if err != nil || allocated[no+1] > amount || remaining != kg(amount-allocated[no+1]) {
				return false
			}
			full = full && remaining.Amount == 0
		}
		want := OrderPartiallyAllocated
		switch {
		case len(st.Obligations) == 0:
			want = OrderRecorded
		case full:
			want = OrderAllocated
		}
		restored, err := RestoreOrder(st)
		return st.Status == want && err == nil && reflect.DeepEqual(restored.State(), st) && len(o.Pending()) == 1+len(st.Obligations)
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 500}); err != nil {
		t.Fatal(err)
	}
}
