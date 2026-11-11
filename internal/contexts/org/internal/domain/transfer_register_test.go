package domain

import (
	"errors"
	"testing"
	"time"
)

// ORG V0.7: the register warns from 80,000 persons, blocks from 100,000 without a safeguard and from 1,000,000
// always; the year is China's; keys carry no personal data.
func TestTransferThresholds(t *testing.T) {
	th := DefaultTransferThresholds
	for _, tc := range []struct {
		persons, added int64
		safeguard      bool
		state          TransferState
		err            error
	}{
		{0, 3, false, TransferOK, nil},
		{79_998, 2, false, TransferWarning, nil},
		{99_998, 1, false, TransferWarning, nil},
		{99_999, 1, false, "", ErrTransferThreshold},
		{99_999, 1, true, TransferWarning, nil},
		{150_000, 0, false, TransferBlocked, nil}, // nothing new: keys counted already pass
		{999_999, 1, true, "", ErrTransferAssessment},
	} {
		state, err := th.Admit(tc.persons, tc.added, tc.safeguard)
		if state != tc.state || !errors.Is(err, tc.err) {
			t.Errorf("Admit(%d, %d, %v) = %s, %v; want %s, %v", tc.persons, tc.added, tc.safeguard, state, err, tc.state, tc.err)
		}
	}
	if (TransferThresholds{Warning: 10, Block: 5, Assessment: 20}).Valid() {
		t.Fatal("thresholds must rise")
	}
	newYearEve := time.Date(2026, 12, 31, 17, 0, 0, 0, time.UTC) // 01:00 on 1 January 2027 in China
	if TransferYear(newYearEve) != 2027 {
		t.Fatalf("year %d", TransferYear(newYearEve))
	}
	for k, want := range map[string]bool{"ORG:party-a:C-1": true, "COM:O-1:CONSIGNEE:2": true, "ORG:C-1": false, "ORG:p:zhang@example.cn": false, "Test Person": false, "plain": false, "ORG:a b": false} {
		if ValidContactKey(k) != want {
			t.Errorf("key %q: %v", k, !want)
		}
	}
}
