//go:build integration

package bootstrap

import (
	"errors"
	"testing"
	"time"

	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
)

// FUL's OceanEvidenceCurrency v1 (V2.87) through the real PHY: whether RecordVerifiedLoadEvidence would record a new
// evidence version of a shipment now. The query writes nothing, so FUL's revision and evidence version stay as they
// were each time it is asked.
func TestFULSaysWhetherItsEvidenceIsPHYsCurrentOneWithPHY(t *testing.T) {
	d := newReworkDay(t, "cargo-ocean-currency-")
	currency := func(shipment string) fulapi.OceanEvidenceCurrency {
		t.Helper()
		before, err := d.ful.GetOceanOperationPlan(d.ctx, fulapi.GetShipmentPlanSnapshot{TenantID: d.tenant, ShipmentID: shipment})
		d.must("plan", err)
		c, err := d.ful.GetOceanEvidenceCurrency(d.ctx, fulapi.GetShipmentPlanSnapshot{TenantID: d.tenant, ShipmentID: shipment})
		d.must("the currency of "+shipment, err)
		after, err := d.ful.GetOceanOperationPlan(d.ctx, fulapi.GetShipmentPlanSnapshot{TenantID: d.tenant, ShipmentID: shipment})
		d.must("plan", err)
		if before.Revision != after.Revision || before.EvidenceVersion != after.EvidenceVersion || c.Revision != before.Revision || c.EvidenceVersion != before.EvidenceVersion ||
			c.TenantID != d.tenant || c.ShipmentID != shipment || c.Status != before.Status {
			t.Fatalf("the question changed or misreads FUL: plan %d/%d -> %d/%d, answer %+v", before.Revision, before.EvidenceVersion, after.Revision, after.EvidenceVersion, c)
		}
		return c
	}
	// PHY holds the received lot and the sealed stuffing; FUL has recorded nothing yet
	if c := currency("SH-H01"); !c.NewVersionDue || c.EvidenceVersion != 0 {
		t.Fatalf("nothing recorded yet: %+v", c)
	}
	d.must("FUL records", d.fulTakes(16, "SH-H01"))
	if c := currency("SH-H01"); c.NewVersionDue || c.EvidenceVersion != 1 {
		t.Fatalf("recorded: %+v", c)
	}
	// asking again is no recording: FUL's command still finds nothing new
	d.must("FUL records again", d.fulTakes(17, "SH-H01"))
	if c := currency("SH-H01"); c.NewVersionDue || c.EvidenceVersion != 1 {
		t.Fatalf("the same facts again: %+v", c)
	}
	// PHY opens the container for rework: its stuffing is no longer closed
	d.openRework()
	if c := currency("SH-H01"); !c.NewVersionDue || c.EvidenceVersion != 1 {
		t.Fatalf("in rework: %+v", c)
	}
	d.must("FUL records the rework", d.fulTakes(23, "SH-H01"))
	if c := currency("SH-H01"); c.NewVersionDue || c.EvidenceVersion != 2 {
		t.Fatalf("the rework recorded: %+v", c)
	}
	d.unloadH03()
	d.sealAgain()
	if c := currency("SH-H01"); !c.NewVersionDue {
		t.Fatalf("sealed again: %+v", c)
	}
	d.must("FUL records the reseal", d.fulTakes(28, "SH-H01"))
	if c := currency("SH-H01"); c.NewVersionDue || c.EvidenceVersion != 3 {
		t.Fatalf("the reseal recorded: %+v", c)
	}
	// a house PHY detached: FUL still records it in C-1 until it takes the membership, and reads PHY for C-1
	d.detachH03()
	d.must("FUL records H03 in the rework", d.fulTakes(28, "SH-H03"))
	if c := currency("SH-H03"); c.NewVersionDue {
		t.Fatalf("H03 before FUL takes the detachment: %+v", c)
	}
	d.fulTakesTheDetachment(29)
	if c := currency("SH-H03"); !c.NewVersionDue {
		t.Fatalf("H03 after FUL takes the detachment: %+v", c)
	}
	// a shipment FUL does not know
	if _, err := d.ful.GetOceanEvidenceCurrency(d.ctx, fulapi.GetShipmentPlanSnapshot{TenantID: d.tenant, ShipmentID: "SH-NO_SUCH"}); !errors.Is(err, fulapi.ErrNotFound) {
		t.Fatalf("an unknown shipment: %v", err)
	}
	// a shipment no lot of which PHY holds, and FUL with no evidence: the command records an empty first version, so
	// one is due; once recorded none is. An air shipment is asked the same way (FUL reads PHY for air, with the
	// screenings in the evidence): the same comparison, so the same answer. A cancelled shipment is not due: the
	// command refuses it.
	contexts := slice{org: d.org, rate: d.rate, com: d.com, ful: d.ful, tenant: d.tenant, at: d.at, departs: d.at.Add(72 * time.Hour)}
	d.seedParties("party-x", "party-y")
	planSeaSellOnly(t, contexts, "party-x", "X", 60_000)
	contexts.planAir(t, "party-y", "Y")
	for _, s := range []string{"SH-X", "SH-Y"} {
		if c := currency(s); !c.NewVersionDue || c.EvidenceVersion != 0 {
			t.Fatalf("%s: no evidence recorded, none in PHY: %+v", s, c)
		}
		p, err := d.ful.GetOceanOperationPlan(d.ctx, fulapi.GetShipmentPlanSnapshot{TenantID: d.tenant, ShipmentID: s})
		d.must("plan "+s, err)
		if _, err = d.ful.RecordVerifiedLoadEvidence(d.ctx, fulapi.RecordVerifiedLoadEvidence{CommandID: "ful-ev-" + s, TenantID: d.tenant, ActorID: "ops-1", ShipmentID: s,
			ExpectedRevision: p.Revision, OccurredAt: d.hours(29)}); err != nil {
			t.Fatalf("the command records the empty first version of %s: %v", s, err)
		}
		if c := currency(s); c.NewVersionDue || c.EvidenceVersion != 1 {
			t.Fatalf("%s: the empty version recorded: %+v", s, c)
		}
	}
	p, err := d.ful.GetOceanOperationPlan(d.ctx, fulapi.GetShipmentPlanSnapshot{TenantID: d.tenant, ShipmentID: "SH-X"})
	d.must("plan X", err)
	_, err = d.ful.CancelShipment(d.ctx, fulapi.CancelShipment{CommandID: "cancel-x", TenantID: d.tenant, ActorID: "planner-1", ShipmentID: "SH-X", ExpectedRevision: p.Revision,
		Reason: "the customer withdrew", OccurredAt: d.hours(30)})
	d.must("cancel SH-X", err)
	if c := currency("SH-X"); c.NewVersionDue || c.Status != fulapi.StatusCancelled {
		t.Fatalf("a cancelled shipment: %+v", c)
	}
	if _, err = d.ful.RecordVerifiedLoadEvidence(d.ctx, fulapi.RecordVerifiedLoadEvidence{CommandID: "ful-ev-x-cancelled", TenantID: d.tenant, ActorID: "ops-1", ShipmentID: "SH-X",
		ExpectedRevision: p.Revision + 1, OccurredAt: d.hours(31)}); !errors.Is(err, fulapi.ErrRejected) {
		t.Fatalf("the command refuses a cancelled shipment, as the question records nothing for it: %v", err)
	}
}
