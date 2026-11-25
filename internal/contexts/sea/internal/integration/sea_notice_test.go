//go:build integration

package integration

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
)

// The notice of a proposal names the legal entities that stay looser than the company, as approved exceptions. With 40
// of them it would not fit the receipt's 4000 characters: the notice lists as many as fit, says how many more there are
// and where to read them, and the company's tightening is recorded.
func TestTheNoticeOfAProposalFitsTheReceiptOnPostgres(t *testing.T) {
	var les []string
	for i := 0; i < 40; i++ {
		les = append(les, fmt.Sprintf("LEGAL-ENTITY-SHANGHAI-%03d", i))
	}
	s := newSettingsWorldOf(t, les...)
	s.must(s.propose("c1", "ann", time.Time{}, tenantEntry("SUPERVISOR_ONLY")))
	var entries []api.SeaSettingEntry
	for _, le := range les {
		entries = append(entries, leEntry(le, "WITH_REASON"))
	}
	s.must(s.propose("c2", "ann", time.Time{}, entries...))
	s.must(s.approve("c2", "bob"))
	r, err := s.propose("c3", "ann", time.Time{}, tenantEntry("SUPERVISOR_ONLY"))
	if err != nil {
		t.Fatalf("the company keeps SUPERVISOR_ONLY with 40 approved exceptions: %v", err)
	}
	if len(r.Notice) > 4000 || !strings.Contains(r.Notice, "more legal entities; see GetSeaSettings") || !strings.Contains(r.Notice, "LEGAL-ENTITY-SHANGHAI-000") {
		t.Fatalf("the notice (%d characters): %s", len(r.Notice), r.Notice)
	}
}
