package domain

import (
	"errors"
	"testing"
)

// FIN V2.100: the cost variance threshold (E1) under approval. A version is looser when its amount or its rate is
// higher than the version in force at FIN's clock, and the first is always looser; a looser version waits for another
// person, a version that is not looser is in force at once.

func newThresholds(t *testing.T) *FinancePolicy {
	t.Helper()
	p := policy(t)
	if err := p.ProposeVarianceThreshold(10_000, 100, "acct-1", hour(3), hour(3)); err != nil {
		t.Fatal(err)
	}
	if err := p.ApproveVarianceThreshold(1, "ctrl-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTheFirstThresholdIsLooserAndWaitsForAnotherPerson(t *testing.T) {
	p := policy(t)
	if err := p.ProposeVarianceThreshold(10_000, 100, "acct-1", hour(3), hour(3)); err != nil {
		t.Fatal(err)
	}
	if v := p.State().Thresholds[0]; !v.Looser || v.State != ThresholdProposed || v.Auto || !v.ProposedAt.Equal(hour(3)) {
		t.Fatalf("the first version: %+v", v)
	}
	if p.ThresholdAt(hour(9)) != nil || p.ProposedThreshold() == nil {
		t.Fatal("a proposed version is not in force")
	}
	if err := p.ApproveVarianceThreshold(1, "acct-1", hour(4)); !errors.Is(err, ErrThresholdApproval) {
		t.Fatalf("the setter approving: %v", err)
	}
	if err := p.RefuseVarianceThreshold(1, "no", "acct-1", hour(4)); !errors.Is(err, ErrThresholdApproval) {
		t.Fatalf("the setter refusing: %v", err)
	}
	if err := p.ApproveVarianceThreshold(1, "ctrl-1", hour(4)); err != nil {
		t.Fatal(err)
	}
	v := p.State().Thresholds[0]
	if v.State != ThresholdApproved || v.DecidedBy != "ctrl-1" || v.Auto || !v.InForceAt.Equal(hour(4)) {
		t.Fatalf("approved: %+v", v)
	}
	if p.ThresholdAt(hour(3)) != nil || p.ThresholdAt(hour(4)).No != 1 || len(p.AddedDecisions()) != 1 {
		t.Fatal("in force from the approval, not from the proposal")
	}
}

func TestAThresholdIsLooserByAmountOrByRateOnly(t *testing.T) {
	for name, c := range map[string]struct {
		amount     int64
		rate       int
		looser     bool
		wantStatus string
	}{
		"a higher amount":               {10_001, 100, true, ThresholdProposed},
		"a higher rate":                 {10_000, 101, true, ThresholdProposed},
		"a higher amount, a lower rate": {20_000, 50, true, ThresholdProposed},
		"a lower amount, a higher rate": {5_000, 200, true, ThresholdProposed},
		"a lower amount and rate":       {9_999, 99, false, ThresholdApproved},
		"the same":                      {10_000, 100, false, ThresholdApproved},
		"zero":                          {0, 0, false, ThresholdApproved},
		"a lower amount, the same rate": {1, 100, false, ThresholdApproved},
		"the same amount, a lower rate": {10_000, 0, false, ThresholdApproved},
	} {
		p := newThresholds(t)
		if err := p.ProposeVarianceThreshold(c.amount, c.rate, "acct-1", hour(5), hour(5)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		v := p.State().Thresholds[1]
		if v.Looser != c.looser || v.State != c.wantStatus {
			t.Errorf("%s: %+v", name, v)
		}
		if !c.looser && (!v.Auto || v.DecidedBy != "acct-1" || !v.InForceAt.Equal(hour(5)) || p.ThresholdAt(hour(5)).No != 2) {
			t.Errorf("%s: a version that is not looser is in force at once, approved by its setter: %+v", name, v)
		}
		if c.looser && p.ThresholdAt(hour(9)).No != 1 {
			t.Errorf("%s: the version in force changed before an approval", name)
		}
	}
}

func TestASecondThresholdWaitsForTheFirstToBeDecided(t *testing.T) {
	p := newThresholds(t)
	if err := p.ProposeVarianceThreshold(50_000, 100, "acct-1", hour(5), hour(5)); err != nil {
		t.Fatal(err)
	}
	err := p.ProposeVarianceThreshold(1, 1, "acct-1", hour(6), hour(6))
	if !errors.Is(err, ErrThresholdProposed) {
		t.Fatalf("a second set while one is proposed: %v", err)
	}
	if err := p.ApproveVarianceThreshold(1, "ctrl-1", hour(6)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("approving a version that is not the one waiting: %v", err)
	}
	if err := p.RefuseVarianceThreshold(2, " ", "ctrl-1", hour(6)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a refusal without a reason: %v", err)
	}
	if err := p.RefuseVarianceThreshold(2, "too loose", "ctrl-1", hour(6)); err != nil {
		t.Fatal(err)
	}
	if v := p.State().Thresholds[1]; v.State != ThresholdRefused || v.RefuseReason != "too loose" || !v.InForceAt.IsZero() {
		t.Fatalf("refused: %+v", v)
	}
	if p.ThresholdAt(hour(9)).No != 1 || p.ProposedThreshold() != nil {
		t.Fatal("a refused version is never in force")
	}
	if err := p.ApproveVarianceThreshold(2, "ctrl-1", hour(7)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("approving a refused version: %v", err)
	}
	// the next version is compared with the one in force, not with the refused one
	if err := p.ProposeVarianceThreshold(10_000, 100, "acct-1", hour(7), hour(7)); err != nil || p.State().Thresholds[2].Looser {
		t.Fatalf("a version after a refusal: %v %+v", err, p.State().Thresholds)
	}
	if err := p.ProposeVarianceThreshold(1, 1, "acct-1", hour(8), hour(6)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("FIN's clock behind the latest record: %v", err)
	}
}

// A match is weighed against the threshold in force at its system time, not at its business time: an approval makes the
// threshold in force from its own time, and a match read just before keeps the earlier one.
func TestAMatchReadsTheThresholdInForceAtItsSystemTime(t *testing.T) {
	p := accountingPolicy(t)
	if err := p.ProposeVarianceThreshold(10_000, 100, "acct-1", hour(1), hour(1)); err != nil {
		t.Fatal(err)
	}
	if err := p.ApproveVarianceThreshold(1, "ctrl-1", hour(2)); err != nil {
		t.Fatal(err)
	}
	if err := p.ProposeVarianceThreshold(50_000, 100, "acct-1", hour(3), hour(3)); err != nil {
		t.Fatal(err)
	}
	pool := poolAccrual() // 1,000.00 USD accrued; 1,050.00 USD invoiced: a variance of 436.60 CNY
	match := func(business, system int) *CreditorInvoice {
		c := recorded(t, CreditorLineInput{ChargeCode: "AIR_FREIGHT", Amount: 105_000})
		if err := c.MatchAt([]MatchItem{to(1, pool, 105_000, true)}, []AccrualTarget{pool}, p, "ap-1", hour(business), hour(system)); err != nil {
			t.Fatal(err)
		}
		return c
	}
	if err := p.ApproveVarianceThreshold(2, "ctrl-1", hour(6)); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		business, system int
		threshold        int
		required         bool
	}{
		"before the approval, a business time after it": {9, 5, 1, true},
		"at the approval":                     {5, 6, 2, false},
		"after it, a business time before it": {5, 8, 2, false},
	} {
		got := match(c.business, c.system)
		if st := got.State(); st.ThresholdNo != c.threshold || st.ApprovalRequired != c.required || !st.MatchEnteredAt.Equal(hour(c.system)) {
			t.Errorf("%s: %+v", name, st)
		}
	}
}
