package kernel

import "testing"

func TestValidIncoterm(t *testing.T) {
	for _, c := range []struct {
		edition, rule string
		want          bool
	}{
		{"2020", "FOB", true}, {"2020", "DPU", true}, {"2020", "DAT", false},
		{"2010", "DAT", true}, {"2010", "DPU", false}, {"2000", "FOB", false}, {"2020", "fob", false}, {"", "", false},
	} {
		if got := ValidIncoterm(c.edition, c.rule); got != c.want {
			t.Errorf("ValidIncoterm(%q, %q) = %v, want %v", c.edition, c.rule, got, c.want)
		}
	}
}
