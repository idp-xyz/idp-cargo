package domain

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

var day = 24 * time.Hour

func TestAgreeTermsTakesOverTheQuoteAndCompletesWhatItLeftOpen(t *testing.T) {
	fob := CommercialTerms{IncotermsRule: "FOB", IncotermsVersion: "2020", NamedPlace: "Shanghai port"}
	for name, tc := range map[string]struct {
		quote, given CommercialTerms
		want         OrderTerms
		err          error
	}{
		"nothing agreed or given": {CommercialTerms{}, CommercialTerms{}, OrderTerms{}, nil},
		"the quote's terms": {CommercialTerms{IncotermsRule: "FOB", IncotermsVersion: "2020", NamedPlace: "Shanghai port", ServiceLevel: "EXPRESS"}, CommercialTerms{},
			OrderTerms{CommercialTerms: CommercialTerms{IncotermsRule: "FOB", IncotermsVersion: "2020", NamedPlace: "Shanghai port", ServiceLevel: "EXPRESS"},
				IncotermsSource: TermFromQuote, ServiceLevelSource: TermFromQuote}, nil},
		"the quote's terms repeated": {fob, fob, OrderTerms{CommercialTerms: fob, IncotermsSource: TermFromQuote}, nil},
		"completed on the order": {CommercialTerms{ServiceLevel: "STANDARD"}, fob,
			OrderTerms{CommercialTerms: CommercialTerms{IncotermsRule: "FOB", IncotermsVersion: "2020", NamedPlace: "Shanghai port", ServiceLevel: "STANDARD"},
				IncotermsSource: TermFromOrder, ServiceLevelSource: TermFromQuote}, nil},
		"a service level completed": {fob, CommercialTerms{ServiceLevel: "ECONOMY"},
			OrderTerms{CommercialTerms: CommercialTerms{IncotermsRule: "FOB", IncotermsVersion: "2020", NamedPlace: "Shanghai port", ServiceLevel: "ECONOMY"},
				IncotermsSource: TermFromQuote, ServiceLevelSource: TermFromOrder}, nil},
		"another Incoterms rule": {fob, CommercialTerms{IncotermsRule: "CIF", IncotermsVersion: "2020", NamedPlace: "Hamburg port"}, OrderTerms{}, ErrTermsMismatch},
		"another named place":    {fob, CommercialTerms{IncotermsRule: "FOB", IncotermsVersion: "2020", NamedPlace: "Ningbo port"}, OrderTerms{}, ErrTermsMismatch},
		"another service level":  {CommercialTerms{ServiceLevel: "STANDARD"}, CommercialTerms{ServiceLevel: "EXPRESS"}, OrderTerms{}, ErrTermsMismatch},
		"DAT is not a 2020 rule": {CommercialTerms{}, CommercialTerms{IncotermsRule: "DAT", IncotermsVersion: "2020", NamedPlace: "Rotterdam"}, OrderTerms{}, ErrInvalidOrder},
		"no named place":         {CommercialTerms{}, CommercialTerms{IncotermsRule: "FOB", IncotermsVersion: "2020"}, OrderTerms{}, ErrInvalidOrder},
		"free text on the order": {CommercialTerms{}, CommercialTerms{ServiceLevel: "next flight out"}, OrderTerms{}, ErrInvalidOrder},
	} {
		got, err := AgreeTerms(tc.quote, tc.given)
		if !errors.Is(err, tc.err) || got != tc.want {
			t.Errorf("%s: got %+v, %v; want %+v, %v", name, got, err, tc.want, tc.err)
		}
		if err == nil && (!got.valid() || got.QuoteTerms() != tc.quote) {
			t.Errorf("%s: agreed terms %+v are not valid or do not show the quote's terms", name, got)
		}
	}
}

func TestAnOrderFreezesTheAgreedTermsAndAQuoteWithFreeTextKeepsIt(t *testing.T) {
	d := draft(palletLine)
	d.Quote.Terms = CommercialTerms{ServiceLevel: "next flight out"} // a quote from before RATE's list
	d.Requested = CommercialTerms{IncotermsRule: "FCA", IncotermsVersion: "2020", NamedPlace: "Pudong airport"}
	o, err := RecordOrder(d, 0, recordAt, recordAt)
	if err != nil {
		t.Fatal(err)
	}
	s := o.State()
	if s.Terms.ServiceLevel != "next flight out" || s.Terms.ServiceLevelSource != TermFromQuote || s.Terms.IncotermsSource != TermFromOrder ||
		s.Requested != (CommercialTerms{}) {
		t.Fatalf("terms %+v, requested %+v", s.Terms, s.Requested)
	}
	if _, err = RestoreOrder(s); err != nil {
		t.Fatalf("restore: %v", err)
	}
	s.Terms.ServiceLevelSource = TermFromOrder // free text is never completed on an order
	if _, err = RestoreOrder(s); err == nil {
		t.Fatal("restored free text completed on the order")
	}
}

func TestTheOrderingPartyIsTheQuotesRecipientInItsRole(t *testing.T) {
	agent := draft(palletLine)
	agent.Quote.RecipientRole, agent.Customer.RoleCode = RoleOverseasAgent, RoleOverseasAgent
	if _, err := RecordOrder(agent, 0, recordAt, recordAt); err != nil {
		t.Fatalf("an overseas agent orders on its quote: %v", err)
	}
	for name, edit := range map[string]func(*OrderDraft){
		"confirmed in another role": func(d *OrderDraft) { d.Customer.RoleCode = RoleShipper },
		"no role confirmed":         func(d *OrderDraft) { d.Customer.RoleCode = "" },
	} {
		d := draft(palletLine)
		edit(&d)
		if _, err := RecordOrder(d, 0, recordAt, recordAt); !errors.Is(err, ErrQuoteMismatch) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	for name, edit := range map[string]func(*OrderDraft){
		"an unknown recipient role": func(d *OrderDraft) { d.Quote.RecipientRole, d.Customer.RoleCode = "CARRIER", "CARRIER" },
		"a v1 snapshot with an agent": func(d *OrderDraft) {
			d.Quote.TermsContract, d.Quote.RecipientRole, d.Customer.RoleCode = QuoteSnapshotV1, RoleOverseasAgent, RoleOverseasAgent
		},
		"a v1 snapshot with terms": func(d *OrderDraft) {
			d.Quote.TermsContract, d.Quote.Terms = QuoteSnapshotV1, CommercialTerms{ServiceLevel: "STANDARD"}
		},
		"an unknown snapshot contract": func(d *OrderDraft) { d.Quote.TermsContract = "QuoteSnapshot.v3" },
		"a policy of another legal body": func(d *OrderDraft) {
			d.Policy = OrderPolicy{Scope: PolicyLegalEntity, LegalEntityID: "LE-2", Version: 1, MaxOrdersPerQuote: 1}
		},
	} {
		d := draft(palletLine)
		edit(&d)
		if _, err := RecordOrder(d, 0, recordAt, recordAt); !errors.Is(err, ErrInvalidOrder) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestThePolicyBoundsOrdersPerQuoteAndTheGraceAfterValidity(t *testing.T) {
	until := at.Add(14 * day) // the end of the quote's validity in draft
	three := OrderPolicy{Scope: PolicyLegalEntity, LegalEntityID: "LE", Version: 2, MaxOrdersPerQuote: 3, GraceDays: 2}
	for name, tc := range map[string]struct {
		policy OrderPolicy
		orders int
		at     time.Time
		want   error
	}{
		"the first order, built in":            {BuiltInOrderPolicy(), 0, recordAt, nil},
		"a second order, built in":             {BuiltInOrderPolicy(), 1, recordAt, ErrQuoteFullyOrdered},
		"the third of three":                   {three, 2, recordAt, nil},
		"a fourth of three":                    {three, 3, recordAt, ErrQuoteFullyOrdered},
		"after the validity, built in":         {BuiltInOrderPolicy(), 0, until, ErrOutsideQuoteValidity},
		"within two days of grace":             {three, 0, until.Add(2*day - time.Microsecond), nil},
		"when the grace ends":                  {three, 0, until.Add(2 * day), ErrOutsideQuoteValidity},
		"before the acceptance, despite grace": {three, 0, at.Add(30 * time.Minute), ErrOutsideQuoteValidity},
	} {
		d := draft(palletLine)
		d.Policy = tc.policy
		o, err := RecordOrder(d, tc.orders, tc.at, tc.at)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
		if err == nil && o.State().Policy != tc.policy {
			t.Errorf("%s: the order froze policy %+v", name, o.State().Policy)
		}
	}
}

func TestSetOrderPolicyKeepsVersionsPerScope(t *testing.T) {
	first, err := SetOrderPolicy("T", "LE", nil, 0, 3, 2, "commercial-lead", at)
	if err != nil {
		t.Fatal(err)
	}
	if first.Scope != PolicyLegalEntity || first.Version != 1 || first.MaxOrdersPerQuote != 3 || first.GraceDays != 2 {
		t.Fatalf("first %+v", first)
	}
	second, err := SetOrderPolicy("T", "LE", &first, 1, 1, 0, "commercial-lead", at.Add(time.Hour))
	if err != nil || second.Version != 2 {
		t.Fatalf("second %+v, %v", second, err)
	}
	tenant, err := SetOrderPolicy("T", "", nil, 0, 5, 7, "commercial-lead", at)
	if err != nil || tenant.Scope != PolicyTenant || tenant.LegalEntityID != "" || tenant.Version != 1 {
		t.Fatalf("tenant default %+v, %v", tenant, err)
	}
	for name, tc := range map[string]struct {
		latest    *PolicySetting
		expected  int64
		max, days int
		at        time.Time
		want      error
	}{
		"a stale version":           {&second, 1, 1, 0, at.Add(2 * time.Hour), ErrPolicyVersionStale},
		"a first version expected":  {nil, 1, 1, 0, at, ErrPolicyVersionStale},
		"no order per quote":        {&second, 2, 0, 0, at.Add(2 * time.Hour), ErrInvalidPolicy},
		"a thousand orders":         {&second, 2, 1000, 0, at.Add(2 * time.Hour), ErrInvalidPolicy},
		"31 days of grace":          {&second, 2, 1, 31, at.Add(2 * time.Hour), ErrInvalidPolicy},
		"negative grace":            {&second, 2, 1, -1, at.Add(2 * time.Hour), ErrInvalidPolicy},
		"before the version before": {&second, 2, 1, 0, at, ErrInvalidPolicy},
		"another scope's version":   {&tenant, 1, 1, 0, at.Add(2 * time.Hour), ErrInvalidPolicy},
	} {
		if _, err := SetOrderPolicy("T", "LE", tc.latest, tc.expected, tc.max, tc.days, "commercial-lead", tc.at); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	if _, err := SetOrderPolicy("T", "LE", nil, 0, 1, 0, " ", at); !errors.Is(err, ErrInvalidPolicy) {
		t.Errorf("no author: err = %v", err)
	}
}

func TestAmendLineCorrectsTheDeclarationAboveWhatIsCommitted(t *testing.T) {
	o := recorded(t)
	if err := o.Allocate(1, "OB-1", packsWithVolume(10, 500, 12_000), "planner-1", recordAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := o.Allocate(2, "OB-2", packs(12, 60), "planner-1", recordAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if o.Status() != OrderAllocated {
		t.Fatalf("status %s", o.Status())
	}
	o.Acknowledge()
	later := recordAt.Add(2 * time.Hour)
	// The customer declares 520 kg instead of 500 for the same ten pallets, then two more pallets.
	if err := o.AmendLine(1, "Pallets of machine parts", "PX", packsWithVolume(10, 520, 12_000), "customer corrected the weight", "cs-1", later, later); err != nil {
		t.Fatal(err)
	}
	if err := o.AmendLine(1, "Pallets of machine parts", "PX", packsWithVolume(12, 620, 14_000), "two more pallets", "cs-1", later, later); err != nil {
		t.Fatal(err)
	}
	if l, v, _ := o.Line(1); v != 3 || l.Measure != packsWithVolume(12, 620, 14_000) {
		t.Fatalf("line 1 is %+v at version %d", l, v)
	}
	if o.Status() != OrderPartiallyAllocated {
		t.Fatalf("two pallets are not allocated, but the status is %s", o.Status())
	}
	if left, _ := o.Remaining(1); left != packsWithVolume(2, 120, 2_000) {
		t.Fatalf("remaining %+v", left)
	}
	if got := eventList(o); got != "[CustomerOrderLineAmended.v1 CustomerOrderLineAmended.v1]" {
		t.Fatalf("events %s", got)
	}
	if e := o.Pending()[1]; e.LineNo != 1 || e.LineVersion != 3 {
		t.Fatalf("event %+v", e)
	}
	if n := len(o.AddedLineVersions()); n != 2 {
		t.Fatalf("%d line versions to persist", n)
	}
	for name, tc := range map[string]struct {
		pt     PackageType
		m      Measure
		reason string
		at     time.Time
		want   error
	}{
		"below the committed weight":   {"PX", packsWithVolume(12, 499, 14_000), "lighter", later, ErrInvalidChange},
		"below the committed packs":    {"PX", packsWithVolume(9, 620, 14_000), "fewer", later, ErrInvalidChange},
		"another package type":         {"CT", packsWithVolume(12, 620, 14_000), "repacked", later, ErrInvalidChange},
		"another weight unit":          {"PX", Measure{Packs: 12, GrossWeight: lb(1400), Volume: dm3(14_000)}, "in pounds", later, ErrInvalidChange},
		"the volume dropped":           {"PX", packs(12, 620), "no volume", later, ErrInvalidChange},
		"nothing changes":              {"PX", packsWithVolume(12, 620, 14_000), "same", later, ErrInvalidChange},
		"no reason":                    {"PX", packsWithVolume(12, 630, 14_000), " ", later, ErrInvalidChange},
		"before the line last changed": {"PX", packsWithVolume(12, 630, 14_000), "earlier", later.Add(-time.Minute), ErrInvalidChange},
		"no packs":                     {"PX", packsWithVolume(0, 630, 14_000), "none", later, ErrInvalidOrder},
	} {
		if err := o.AmendLine(1, "Pallets of machine parts", tc.pt, tc.m, tc.reason, "cs-1", tc.at, tc.at); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	if err := o.AmendLine(3, "Drums", "1A", packs(1, 10), "no such line", "cs-1", later, later); !errors.Is(err, ErrInvalidChange) {
		t.Errorf("an unknown line: err = %v", err)
	}
}

func TestALineWithoutActiveObligationsMayChangeItsPackageTypeAndUnits(t *testing.T) {
	o := recorded(t)
	later := recordAt.Add(time.Hour)
	if err := o.AmendLine(2, "Spare parts, palletised", "PX", Measure{Packs: 1, GrossWeight: lb(140)}, "palletised at origin", "cs-1", later, later); err != nil {
		t.Fatal(err)
	}
	if err := o.Allocate(2, "OB-2", Measure{Packs: 1, GrossWeight: lb(140)}, "planner-1", later); err != nil {
		t.Fatalf("allocate the corrected line: %v", err)
	}
}

func TestAddLineNumbersTheLineAfterTheOthers(t *testing.T) {
	o := recorded(t)
	if err := o.Allocate(1, "OB-1", packsWithVolume(10, 500, 12_000), "planner-1", recordAt); err != nil {
		t.Fatal(err)
	}
	if err := o.Allocate(2, "OB-2", packs(12, 60), "planner-1", recordAt); err != nil {
		t.Fatal(err)
	}
	later := recordAt.Add(time.Hour)
	if err := o.AddLine("Cartons of manuals", "CT", packs(2, 15), "customer added two cartons", "cs-1", later, later); err != nil {
		t.Fatal(err)
	}
	if l, v, ok := o.Line(3); !ok || v != 1 || l.LineNo != 3 || l.Measure != packs(2, 15) {
		t.Fatalf("line 3 is %+v at version %d", l, v)
	}
	if o.Status() != OrderPartiallyAllocated || len(o.Lines()) != 3 || len(o.State().Lines) != 2 {
		t.Fatalf("status %s, %d current lines, %d recorded", o.Status(), len(o.Lines()), len(o.State().Lines))
	}
	if err := o.Allocate(3, "OB-3", packs(2, 15), "planner-1", later); err != nil || o.Status() != OrderAllocated {
		t.Fatalf("allocate the added line: %v, status %s", err, o.Status())
	}
	if err := o.AddLine("Cartons", "CT", packs(2, 15), " ", "cs-1", later, later); !errors.Is(err, ErrInvalidChange) {
		t.Errorf("no reason: err = %v", err)
	}
	if err := o.AddLine("Cartons", "CT", packs(2, 15), "before the order", "cs-1", at, at); !errors.Is(err, ErrInvalidChange) {
		t.Errorf("before the order: err = %v", err)
	}
}

func TestACancelledOrderIsNotCorrected(t *testing.T) {
	o := recorded(t)
	if err := o.Cancel("sales-1", recordAt.Add(time.Hour), MinContactRetentionMonths); err != nil {
		t.Fatal(err)
	}
	later := recordAt.Add(2 * time.Hour)
	if err := o.AmendLine(1, "Pallets", "PX", packsWithVolume(10, 520, 12_000), "late", "cs-1", later, later); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("amend: err = %v", err)
	}
	if err := o.AddLine("Cartons", "CT", packs(2, 15), "late", "cs-1", later, later); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("add: err = %v", err)
	}
}

func TestRestoreReplaysLineVersionsInOrder(t *testing.T) {
	o := recorded(t)
	later := recordAt.Add(time.Hour)
	if err := o.AddLine("Cartons of manuals", "CT", packs(2, 15), "added", "cs-1", later, later); err != nil {
		t.Fatal(err)
	}
	if err := o.AmendLine(3, "Cartons of manuals", "CT", packs(3, 20), "one more", "cs-1", later, later); err != nil {
		t.Fatal(err)
	}
	if err := o.AmendLine(1, "Pallets of machine parts", "PX", packsWithVolume(10, 520, 12_000), "heavier", "cs-1", later, later); err != nil {
		t.Fatal(err)
	}
	s := o.State()
	restored, err := RestoreOrder(s)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored.Lines(), o.Lines()) || len(restored.AddedLineVersions()) != 0 {
		t.Fatalf("restored lines %+v", restored.Lines())
	}
	for name, edit := range map[string]func(*OrderState){
		"a version skipped":         func(s *OrderState) { s.LineChanges[1].Version = 3 },
		"an added line out of turn": func(s *OrderState) { s.LineChanges[0].LineNo = 4 },
		"a change without reason":   func(s *OrderState) { s.LineChanges[2].Reason = "" },
		"a change before the order": func(s *OrderState) { s.LineChanges[2].ChangedAt = at },
		"an amendment out of order": func(s *OrderState) { s.LineChanges[0], s.LineChanges[1] = s.LineChanges[1], s.LineChanges[0] },
	} {
		s := o.State()
		edit(&s)
		if _, err := RestoreOrder(s); !errors.Is(err, ErrInvalidOrder) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// Orders on an AGREEMENT quote (RATE V0.5 part 3b, OD-5 A): any number within the agreement's validity, under
// QuoteSnapshotV3, with no total; ONE_OFF quotes keep their total and count.
func TestAgreementOrdersAreNotCountedAgainstThePolicy(t *testing.T) {
	agreement := func() OrderDraft {
		d := draft(palletLine)
		d.Quote.Kind, d.Quote.TermsContract, d.Quote.Total = QuoteAgreement, QuoteSnapshotV3, kernel.Money{Currency: d.Quote.Total.Currency}
		return d
	}
	o, err := RecordOrder(agreement(), 7, recordAt, recordAt)
	if err != nil {
		t.Fatalf("an eighth order on an agreement under the built-in policy: %v", err)
	}
	if _, err = RestoreOrder(o.State()); err != nil {
		t.Fatalf("restored: %v", err)
	}
	if _, err = RecordOrder(agreement(), 0, at.Add(14*day), at.Add(14*day)); !errors.Is(err, ErrOutsideQuoteValidity) {
		t.Fatalf("after the agreement's validity: %v", err)
	}
	for name, edit := range map[string]func(*OrderDraft){
		"an agreement with a total":     func(d *OrderDraft) { d.Quote.Total.Minor = 100 },
		"an agreement under v2":         func(d *OrderDraft) { d.Quote.TermsContract = QuoteSnapshotV2 },
		"a ONE_OFF quote without total": func(d *OrderDraft) { d.Quote.Kind = QuoteOneOff },
		"a quote of another kind":       func(d *OrderDraft) { d.Quote.Kind = "FRAMEWORK" },
	} {
		d := agreement()
		edit(&d)
		if _, err := RecordOrder(d, 0, recordAt, recordAt); !errors.Is(err, ErrInvalidOrder) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
