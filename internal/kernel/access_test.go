package kernel

import "testing"

func TestAccessPurposes(t *testing.T) {
	for p, want := range map[AccessPurpose][2]bool{
		PurposeDocuments: {true, false}, PurposeOrderParty: {true, false}, PurposeCustomsAudit: {true, true},
		PurposeLegalClaim: {true, true}, PurposeDataSubjectRequest: {true, true}, "MARKETING": {false, false}, "": {false, false},
	} {
		if p.Valid() != want[0] || p.ForRetainedData() != want[1] {
			t.Errorf("%q: valid %v, for retained data %v", p, p.Valid(), p.ForRetainedData())
		}
	}
}
