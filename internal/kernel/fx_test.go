package kernel

import "testing"

func TestConvertMinor(t *testing.T) {
	for _, tc := range []struct {
		amount, rate    int64
		scale, from, to int
		want            int64
	}{
		{10_000, 71_234, 4, 2, 2, 71_234},   // USD 100.00 at 7.1234 = CNY 712.34
		{1, 71_235, 4, 2, 2, 7},             // 0.0712350 rounds to 0.07
		{5, 1_000, 4, 2, 2, 1},              // 0.005 rounds away from zero
		{-5, 1_000, 4, 2, 2, -1},            // and so does -0.005
		{100, 550, 4, 0, 2, 550},            // JPY 100 at 0.0550 = CNY 5.50
		{123_456, 140_000, 4, 2, 0, 17_284}, // USD 1234.56 at 14.0000 into a currency without minor units
	} {
		got, err := ConvertMinor(tc.amount, tc.rate, tc.scale, tc.from, tc.to)
		if err != nil || got != tc.want {
			t.Errorf("%+v: %d %v", tc, got, err)
		}
	}
	if _, err := ConvertMinor(1<<62, 1<<40, 0, 0, 2); err != ErrAmountOverflow {
		t.Fatalf("overflow: %v", err)
	}
}
