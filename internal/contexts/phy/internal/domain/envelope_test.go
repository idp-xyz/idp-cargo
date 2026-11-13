package domain

import (
	"errors"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

func planned(weight kernel.Quantity, volume kernel.Quantity) CargoScope {
	return CargoScope{Packs: 1, GrossWeight: weight, Volume: volume}
}

// A 22G1 takes 30,480 kg gross with 2,250 kg of tare: 28,230 kg of cargo, and 33.2 m3.
func TestCloseChecksTheEnvelope(t *testing.T) {
	at := t0.Add(2 * time.Hour)
	mtq := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "MTQ", Amount: n} }
	lbr := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "LBR", Amount: n} }
	withUnit := func(t *testing.T) *Consolidation {
		c := seaConsol(t, "C-1", KindSeaLCLOwn)
		if _, err := c.PlanLoadUnit(UnitContainer, unit22G1, "ops", t0.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		return c
	}
	for name, tc := range map[string]struct {
		consol  func(*testing.T) *Consolidation
		houses  []CargoScope
		overrun string
		want    error
		text    string
	}{
		"within":                       {withUnit, []CargoScope{planned(kg(20000), mtq(10)), planned(kg(8230), mtq(23))}, "", nil, ""},
		"one kilogram over":            {withUnit, []CargoScope{planned(kg(20000), mtq(10)), planned(kg(8231), mtq(1))}, "", ErrOverEnvelope, "28231.000 kg gross, the 1 load units take 28230.000 kg"},
		"accepted overrun":             {withUnit, []CargoScope{planned(kg(28231), mtq(1))}, "the plate allows 32,500 kg", nil, ""},
		"over in volume":               {withUnit, []CargoScope{planned(kg(1000), mtq(20)), planned(kg(1000), mtq(14))}, "", ErrOverEnvelope, "34.000000 m3"},
		"volumes not planned":          {withUnit, []CargoScope{planned(kg(1000), kernel.Quantity{}), planned(kg(1000), mtq(33))}, "", nil, ""},
		"pounds over":                  {withUnit, []CargoScope{planned(lbr(66000), kernel.Quantity{})}, "", ErrOverEnvelope, "29937.097 kg"},
		"pounds within":                {withUnit, []CargoScope{planned(lbr(60000), kernel.Quantity{})}, "", nil, ""},
		"a unit not converted":         {withUnit, []CargoScope{planned(kernel.Quantity{Unit: "ONZ", Amount: 1}, kernel.Quantity{})}, "", ErrOverEnvelope, "planned in ONZ"},
		"a reason within the envelope": {withUnit, []CargoScope{planned(kg(1000), kernel.Quantity{})}, "just in case", ErrInvalidChange, ""},
		"no units":                     {func(t *testing.T) *Consolidation { return seaConsol(t, "C-2", KindSeaLCLOwn) }, []CargoScope{planned(kg(99999), mtq(99))}, "", nil, ""},
		"a reason without units":       {func(t *testing.T) *Consolidation { return seaConsol(t, "C-2", KindSeaLCLOwn) }, []CargoScope{planned(kg(99999), mtq(99))}, "big", ErrInvalidChange, ""},
		"a removed unit is not counted": {func(t *testing.T) *Consolidation {
			c := withUnit(t)
			if _, err := c.PlanLoadUnit(UnitContainer, unit22G1, "ops", t0.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if err := c.RemovePlannedLoadUnit(2, "one is enough", "ops", t0.Add(time.Hour), false); err != nil {
				t.Fatal(err)
			}
			return c
		}, []CargoScope{planned(kg(30000), kernel.Quantity{})}, "", ErrOverEnvelope, "the 1 load units"},
		"two units": {func(t *testing.T) *Consolidation {
			c := withUnit(t)
			if _, err := c.PlanLoadUnit(UnitContainer, unit22G1, "ops", t0.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			return c
		}, []CargoScope{planned(kg(56460), mtq(66))}, "", nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			c := tc.consol(t)
			err := c.Close(tc.houses, t0.Add(time.Hour), tc.overrun, "ops", at)
			if !errors.Is(err, tc.want) || (err == nil) != (tc.want == nil) || (tc.text != "" && !strings.Contains(err.Error(), tc.text)) {
				t.Fatalf("Close = %v; want %v %q", err, tc.want, tc.text)
			}
			if err != nil {
				return
			}
			if v := c.Current(); v.Change != ChangeClosed || v.Reason != tc.overrun {
				t.Fatalf("version = %+v", v)
			}
			if _, err := RestoreConsolidation(c.State()); err != nil {
				t.Fatal(err)
			}
		})
	}
	// A stored close with a blank reason does not restore.
	c := withUnit(t)
	if err := c.Close([]CargoScope{planned(kg(28231), kernel.Quantity{})}, t0.Add(time.Hour), "accepted", "ops", at); err != nil {
		t.Fatal(err)
	}
	st := c.State()
	st.Versions[len(st.Versions)-1].Reason = " "
	if _, err := RestoreConsolidation(st); !errors.Is(err, ErrInvalidConsolidation) {
		t.Fatalf("a blank overrun reason restored: %v", err)
	}
}
