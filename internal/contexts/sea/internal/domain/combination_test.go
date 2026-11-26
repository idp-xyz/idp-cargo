package domain

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"testing"
)

// V2.102 batch C: S1 on every configuration of the pairwise covering array (the same array as FIN's, whose S1 column takes
// both values with every value of every other item), and on 200 seeded random releases: the release of a carrier's
// conflict under S1 by someone with and without the supervisor's permission, with the trail and the refusals checked.

func rulesFor(value string, version, hour int) RuleSettings {
	return RuleSettings{Version: version, S1: value, At: hr(hour)}
}

// releaseChecked releases the conflict of the day under S1 and checks what must hold: SUPERVISOR_ONLY refuses without the
// permission (naming it and the way out, writing nothing), WITH_REASON releases whoever asks; a release records the
// version, the value and the time it was decided under; and a conflict released lets the final declaration pass.
func releaseChecked(t *testing.T, label, s1 string, version int, supervisor bool, hour int) {
	t.Helper()
	d := conflictDay(t, "CSQU3054383")
	rules := rulesFor(s1, version, hour)
	err := d.releaseUnder(rules, supervisor, hour)
	if s1 == S1SupervisorOnly && !supervisor {
		if !errors.Is(err, ErrNotSupervisor) {
			t.Fatalf("%s: a release under SUPERVISOR_ONLY without the permission: %v", label, err)
		}
		if cerr := refusalConvention(err.Error()); cerr != nil {
			t.Fatalf("%s: a refusal without its way out: %v", label, cerr)
		}
		if len(d.v.State().Releases) != 0 || d.finalize(hour+3) == nil {
			t.Fatalf("%s: a refused release wrote, or the conflict no longer holds the final declaration back", label)
		}
		return
	}
	if err != nil {
		t.Fatalf("%s: a release that S1 allows: %v", label, err)
	}
	r := d.v.State().Releases
	if len(r) != 1 || r[0].SettingsVersion != version || r[0].S1 != s1 || !r[0].EnteredAt.Equal(hr(hour)) {
		t.Fatalf("%s: the release records what it was decided under: %+v", label, r)
	}
	if err = d.finalize(hour + 3); err != nil {
		t.Fatalf("%s: the final declaration after a release: %v", label, err)
	}
}

func TestS1OnEveryConfigurationOfTheCoveringArray(t *testing.T) {
	_, rows := coveringArray(t)
	seen := map[string]int{}
	for i, c := range rows {
		s1 := c["sea.S1"]
		seen[s1]++
		for _, supervisor := range []bool{false, true} {
			releaseChecked(t, fmt.Sprintf("configuration %d (%v), supervisor %v", i, map[string]string(c), supervisor), s1, i+1, supervisor, 27)
		}
	}
	if seen[S1SupervisorOnly] == 0 || seen[S1WithReason] == 0 {
		t.Fatalf("the array leaves a value of S1 out: %v", seen)
	}
	t.Logf("%d configurations of the covering array, S1 %v", len(rows), seen)
}

func TestRandomReleasesKeepTheTrailAndTheRefusals(t *testing.T) {
	seeds := make([]int64, 200)
	for i := range seeds {
		seeds[i] = int64(i + 1)
	}
	if v := os.Getenv("SEA_PROPERTY_SEED"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		seeds = []int64{n}
	}
	for _, seed := range seeds {
		rng := rand.New(rand.NewSource(seed))
		s1 := []string{S1SupervisorOnly, S1WithReason}[rng.Intn(2)]
		version, hour := rng.Intn(50), 27+rng.Intn(3)
		releaseChecked(t, fmt.Sprintf("seed %d (replay with SEA_PROPERTY_SEED=%d)", seed, seed), s1, version, rng.Intn(2) == 0, hour)
	}
}
