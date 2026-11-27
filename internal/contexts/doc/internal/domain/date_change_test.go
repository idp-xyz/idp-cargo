package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// V2.110, the owner's decision of 2026-11-26: a house whose first departure lies outside the window the customer
// agreed to is not issued, sea or air, until the customer's answer is recorded and the movement finalized again; the
// other houses of the movement are issued as before.
func TestAHouseDepartingOutsideTheAgreedWindowIsNotIssued(t *testing.T) {
	// sea: SH-L drifts in the facts its bill was built on
	drifting := twoBoxes(1, day(1))
	for i := range drifting.Houses {
		drifting.Houses[i].WindowDrift = drifting.Houses[i].ShipmentID == "SH-L"
	}
	d := draftedFrom(t, drifting)
	err := d.Issue(gateOf(drifting), "doc-1", t0)
	if !errors.Is(err, ErrDateChangeUnanswered) || !strings.Contains(err.Error(), "DOC_DATE_CHANGE_UNANSWERED") || d.State().State != StateApproved {
		t.Fatalf("a drifting house bill issued: %v %s", err, d.State().State)
	}
	// another house of the movement drifting does not hold SH-L's bill back
	others := twoBoxes(1, day(1))
	for i := range others.Houses {
		others.Houses[i].WindowDrift = others.Houses[i].ShipmentID != "SH-L"
	}
	if err = draftedFrom(t, others).Issue(gateOf(others), "doc-1", t0); err != nil {
		t.Fatalf("SH-L's bill with another house drifting: %v", err)
	}

	// air: SH-A drifts in AIR's facts
	h, err := DraftDocument("T", "DOC-H1", hawb(t, 1, orders()), "doc-1", t0)
	if err != nil {
		t.Fatal(err)
	}
	f := facts(1)
	for i := range f.Houses {
		f.Houses[i].WindowDrift = f.Houses[i].ShipmentID == "SH-A"
	}
	gate := IssueGate{Facts: f, Orders: orders("DOC-H1"), Credit: CreditDecision{Status: "OK", DecisionID: "CR-1"}, IssuerVerified: true,
		AuthorityID: "AUTH-H", AuthorityVersion: 1}
	if err = h.Approve("doc-2", t0); err != nil {
		t.Fatal(err)
	}
	if err = h.Issue(gate, "doc-2", t0.Add(time.Minute)); !errors.Is(err, ErrDateChangeUnanswered) || !strings.Contains(err.Error(), h.State().Number) {
		t.Fatalf("a drifting HAWB issued: %v", err)
	}
	// a master is the carrier's: it is issued whatever its houses' windows
	m, err := MasterContent("DOC-M1", f, issuer, *party("agent-ny", "US", 0), entered)
	if err != nil {
		t.Fatal(err)
	}
	master, err := DraftDocument("T", "DOC-M1", m, "doc-1", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(master.Approve("doc-2", t0), master.Issue(IssueGate{Facts: f, IssuerVerified: true, AuthorityID: "AUTH-M", AuthorityVersion: 1}, "doc-2", t0)); err != nil {
		t.Fatalf("the master with a drifting house: %v", err)
	}
}
