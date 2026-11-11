package kernel

import (
	"strings"
	"testing"
)

func TestContactKeys(t *testing.T) {
	for in, want := range map[string]string{
		ORGContactKey("shipper-co", "CT-SHIPPER"): "ORG:shipper-co:CT-SHIPPER",
		OrderContactKey("O-A", "CONSIGNEE", 2):    "COM:O-A:CONSIGNEE:2",
	} {
		if in != want || !ValidContactKey(in) {
			t.Errorf("%s, want %s", in, want)
		}
	}
	for _, id := range []string{"zhang.san@example.cn", "张三", "a b", "+8613800138000", strings.Repeat("x", 129)} {
		k := ORGContactKey("party-a", id)
		if !strings.HasPrefix(k, "SHA:") || len(k) != 68 || !ValidContactKey(k) || k != ORGContactKey("party-a", id) || k == ORGContactKey("party-b", id) || strings.Contains(k, "zhang") {
			t.Errorf("%q: %s", id, k)
		}
	}
	if k := OrderContactKey("O-A", "AGENT", 1); !strings.HasPrefix(k, "SHA:") {
		t.Errorf("a role outside the grammar: %s", k)
	}
	if ContactKey("SHA:"+strings.Repeat("a", 64)) == "SHA:"+strings.Repeat("a", 64) {
		t.Error("a digest form given as an id is digested again, so nobody chooses a key's digest")
	}
	for _, k := range []string{"ORG:", "ORG:a", "ORG:p:a b", "ORG:a b", "COM:O-A:SHIPPER:0", "zhang@example.cn", "SHA:abc", "ORG:zhang@example.cn"} {
		if ValidContactKey(k) {
			t.Errorf("%q is not a key", k)
		}
	}
}
