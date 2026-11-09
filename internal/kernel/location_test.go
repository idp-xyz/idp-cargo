package kernel

import "testing"

func TestLocationCodeForm(t *testing.T) {
	for _, tc := range []struct {
		code LocationCode
		want bool
	}{
		{LocationCode{UNLOCODE, "CNSHA"}, true},
		{LocationCode{UNLOCODE, "DEHAM"}, true},
		{LocationCode{UNLOCODE, "USNY2"}, true},
		{LocationCode{UNLOCODE, "USNY1"}, false}, // UN/LOCODE leaves out 0 and 1
		{LocationCode{UNLOCODE, "cnsha"}, false},
		{LocationCode{UNLOCODE, "C1SHA"}, false},
		{LocationCode{UNLOCODE, "CNSH"}, false},
		{LocationCode{UNLOCODE, "PVG"}, false},
		{LocationCode{IATAAirport, "PVG"}, true},
		{LocationCode{IATAAirport, "CNSHA"}, false},
		{LocationCode{IATAAirport, "PV1"}, false},
		{LocationCode{"ICAO", "ZSPD"}, false},
		{LocationCode{}, false},
	} {
		if got := tc.code.Valid(); got != tc.want {
			t.Errorf("%+v.Valid() = %v, want %v", tc.code, got, tc.want)
		}
	}
	if !(LocationCode{}).IsZero() || (LocationCode{UNLOCODE, "CNSHA"}).IsZero() {
		t.Error("IsZero")
	}
}
