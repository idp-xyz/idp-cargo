package domain

import "testing"

// The latest approval a match is not timed before is that of an approved version written since V2.100: a version
// written before has no proposal time (its in-force time is a business time), and one that waits or was refused is not
// in force, whatever time a row holds.
func TestTheLatestApprovalCountsOnlyApprovedVersionsOfThisKind(t *testing.T) {
	base := policy(t).State()
	build := func(versions ...VarianceThreshold) *FinancePolicy {
		st := base
		for i := range versions {
			versions[i].No = i + 1
		}
		st.Thresholds = versions
		p, err := RestorePolicy(st)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if got := build().LatestApprovalAt(); !got.IsZero() {
		t.Fatalf("no version: %s", got)
	}
	approved := VarianceThreshold{State: ThresholdApproved, ProposedAt: hour(1), InForceAt: hour(2)}
	legacy := VarianceThreshold{State: ThresholdApproved, InForceAt: hour(9)} // no proposal time: written before
	waiting := VarianceThreshold{State: ThresholdProposed, ProposedAt: hour(3), InForceAt: hour(8)}
	refused := VarianceThreshold{State: ThresholdRefused, ProposedAt: hour(3), InForceAt: hour(7)}
	if got := build(approved, legacy, waiting, refused).LatestApprovalAt(); !got.Equal(hour(2)) {
		t.Fatalf("an approved version, a legacy one, one that waits and a refused one: %s, want the approved version's %s", got, hour(2))
	}
	if got := build(legacy).LatestApprovalAt(); !got.IsZero() {
		t.Fatalf("a legacy version alone: %s", got)
	}
	later := VarianceThreshold{State: ThresholdApproved, ProposedAt: hour(4), InForceAt: hour(5)}
	if got := build(approved, later, legacy).LatestApprovalAt(); !got.Equal(hour(5)) {
		t.Fatalf("two approved versions: %s", got)
	}
}
