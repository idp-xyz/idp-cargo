package settingstest

import (
	"os"
	"testing"
)

func factors() []Factor {
	return []Factor{{"A", []string{"a0", "a1", "a2"}}, {"B", []string{"b0", "b1"}}, {"C", []string{"c0", "c1"}}, {"D", []string{"d0", "d1"}}}
}

func TestPairwiseCoversEveryValidPairAndOnlyValidRows(t *testing.T) {
	valid := func(m map[string]string) bool { return !(m["D"] == "d1" && m["C"] == "c0") }
	rows := Pairwise(factors(), valid)
	if len(rows) == 0 || len(rows) >= 3*2*2*2 {
		t.Fatalf("%d rows", len(rows))
	}
	if m := Missing(factors(), valid, rows); len(m) != 0 {
		t.Fatalf("missing: %v", m)
	}
	for _, r := range rows {
		if !valid(asMap(factors(), r)) {
			t.Fatalf("an invalid row: %v", r)
		}
	}
	again := Pairwise(factors(), valid)
	if len(again) != len(rows) {
		t.Fatal("not deterministic")
	}
	for i := range rows {
		for j := range rows[i] {
			if rows[i][j] != again[i][j] {
				t.Fatal("not deterministic")
			}
		}
	}
	// the checker finds a missing row
	if m := Missing(factors(), valid, rows[:len(rows)-1]); len(m) == 0 {
		t.Fatal("dropping a row of a minimal greedy cover left nothing missing")
	}
	if Value(factors(), rows[0], "A") == "" {
		t.Fatal("value")
	}
}

func TestTheRefusalConvention(t *testing.T) {
	ok := []string{
		"fin: FIN_PREVIEW_STALE: what the preview was computed on has changed since; preview again and submit the new preview",
		"settings: refused: SETTINGS_SAME_ACTOR: change set x was proposed by ann, who may not approve it; have another approver approve it",
		"sea: conflicts with the stored state: repository: already exists: change set x; propose with another ID",
	}
	ok = append(ok, "fin: FIN_ATTRIBUTION_CONFIRMER: the asker does not confirm who bears its former share; another finance user submits: ops-1 asked for it")
	// a verb followed by a number is a label ("%w: preview 3: ..." wraps FIN_PREVIEW_STALE), not a way out
	bad := []string{"fin: FIN_PREVIEW_STALE: it changed", "it failed; preview again", "fin: nothing to say",
		"fin: FIN_PREVIEW_STALE: it changed: preview 3: the settings version"}
	for _, m := range ok {
		if err := Check(m); err != nil {
			t.Errorf("%q: %v", m, err)
		}
	}
	for _, m := range bad {
		if err := Check(m); err == nil {
			t.Errorf("%q accepted", m)
		}
	}
}

func TestTheConfigurationArrayCoversEveryPairOfItemValues(t *testing.T) {
	rows := Configurations()
	if m := Missing(Items, ValidConfiguration, rows); len(m) != 0 {
		t.Fatalf("missing: %v", m)
	}
	t.Logf("%d configurations", len(rows))
	for _, r := range rows {
		c := ConfigOf(r)
		if !ValidConfiguration(c) {
			t.Fatalf("an invalid configuration: %v", c)
		}
	}
	// the invalid pair is the one the array leaves out, and every other pair with F8 is there
	for _, p := range Missing(Items, nil, rows) {
		if p != "fin.F3=EVERY_ATTRIBUTION with fin.F8=DEFAULT_TO_REMAINING" {
			t.Errorf("a pair of a valid configuration is not covered: %s", p)
		}
	}
	if n := len(rows); n < 12 || n > 30 {
		t.Errorf("%d rows: the largest domain has 4 levels, so at least 12 rows are needed", n)
	}
}

// The domain packages read the covering array from testdata/covering_array.txt: it is what ArrayText writes. Run the test
// with SETTINGSTEST_UPDATE=1 to write it again after the items or the algorithm change.
func TestTheCoveringArrayFileIsWhatTheGeneratorWrites(t *testing.T) {
	if os.Getenv("SETTINGSTEST_UPDATE") != "" {
		if err := os.WriteFile("testdata/covering_array.txt", []byte(ArrayText()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile("testdata/covering_array.txt")
	if err != nil || string(got) != ArrayText() {
		t.Fatalf("testdata/covering_array.txt differs from the generator's array (%v): run SETTINGSTEST_UPDATE=1 go test ./internal/foundation/settings/settingstest/", err)
	}
}
