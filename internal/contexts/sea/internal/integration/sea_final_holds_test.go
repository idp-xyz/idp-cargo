//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/contexts/sea/api"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// What the independent review of 2026-11-18 reproduced with writes past SEA on the application role, each now
// refused by migration 000009. Before it, every statement below committed.

// finalAsPublished checks that OM-1 is FINAL at the revision its facts were published at, with its house and its
// container, and that the published version is the current one.
func (f *fixture) finalAsPublished(t *testing.T, name string) {
	t.Helper()
	ctx := context.Background()
	m := f.get(t, "OM-1")
	facts, err := f.module.GetOceanDocFacts(ctx, api.GetOceanDocFacts{TenantID: f.tenant, MovementID: "OM-1"})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	visits, err := f.module.ListContainerVisits(ctx, api.ListContainerVisits{TenantID: f.tenant, MovementID: "OM-1"})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if m.State != api.StateFinal || m.Revision != 5 || len(m.Houses) != 1 || len(visits) != 1 || visits[0].SealNumber != "CN-SEAL-1" ||
		facts.Status != api.DocFactsFinal || facts.Version != 1 || len(facts.Houses) != 1 || len(facts.Containers) != 1 {
		t.Fatalf("%s: movement %s at %d with %d houses and visits %+v; facts %s version %d", name, m.State, m.Revision, len(m.Houses), visits, facts.Status,
			facts.Version)
	}
}

// refusedBy checks that the database refused a write past SEA with the state and the rule named.
func refusedBy(t *testing.T, name string, err error, state, rule string) {
	t.Helper()
	if sqlState(err) != state || !strings.Contains(refusal(err), rule) {
		t.Fatalf("%s: %v (state %s, %s), want %s %s", name, err, sqlState(err), refusal(err), state, rule)
	}
}

// A house is ended only while its movement is OPEN at commit. Past SEA, the only live house of a FINAL movement
// could be ended: the movement stayed FINAL without a house, its published facts still listing it.
func TestAHouseIsNotEndedPastSEAOnAFinalMovement(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.publishOwnFacts(t)
	end := `UPDATE cargo_sea.house_bill SET end_kind='DETACHED', ended_by='x', ended_at=now(), end_entered_at=now()
        WHERE tenant_id=$1 AND movement_id='OM-1' AND ended_at IS NULL`
	refusedBy(t, "a house ended past SEA on a FINAL movement", f.past(t, end), "23514", "house_bill_ended_on_open_movement")
	f.finalAsPublished(t, "after the refused end")
	// SEA's own end returns the movement to OPEN in the same transaction, and is accepted
	f.w.mu.Lock()
	p := f.w.plans["SH-1"]
	p.ConsolidationID = ""
	f.w.plans["SH-1"] = p
	f.w.mu.Unlock()
	ended, err := f.module.EndHouseBillAssociation(ctx, api.EndHouseBillAssociation{CommandID: "end-1", TenantID: f.tenant, ActorID: "ops-1",
		MovementID: "OM-1", ExpectedRevision: 5, ShipmentID: "SH-1", OccurredAt: f.hours(6)})
	if m := f.get(t, "OM-1"); err != nil || ended.NewRevision != 6 || m.State != api.StateOpen || len(m.Houses) != 0 {
		t.Fatalf("SEA's end: %+v %v; movement %s with %d houses", ended, err, m.State, len(m.Houses))
	}
	if facts, err := f.module.GetOceanDocFacts(ctx, api.GetOceanDocFacts{TenantID: f.tenant, MovementID: "OM-1"}); err != nil ||
		facts.Status != api.DocFactsSuperseded {
		t.Fatalf("the facts after SEA's end: %s %v", facts.Status, err)
	}
}

// A container is assigned only while its movement is not FINAL at commit. Past SEA, a container could be assigned
// to a FINAL movement, whose published facts list its containers. (A stuffing or a release on a FINAL movement is
// refused by the same rule; without a container assigned there, neither can be reached.)
func TestAContainerIsNotAssignedPastSEAOnAFinalMovement(t *testing.T) {
	f := newFixture(t)
	f.publishOwnFacts(t)
	number := containerNumber("MSK", 1)
	if _, err := f.assign("CV-SEA", "OM-1", 2, number, 6); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("SEA's own assignment on a FINAL movement: %v", err)
	}
	assign := fmt.Sprintf(`INSERT INTO cargo_sea.container_visit (tenant_id,visit_id,movement_id,consolidation_id,load_unit_no,container_number,
        size_type,status,assigned_by,assigned_at,entered_at) VALUES ($1,'CV-PAST','OM-1','C-1',2,'%s','22G1','ASSIGNED','x',now(),now())`, number)
	err := f.past(t, assign)
	refusedBy(t, "a container assigned past SEA on a FINAL movement", err, "23514", "container_visit_consistent")
	if !strings.Contains(refusal(err), "while the movement is not final") {
		t.Fatalf("refused by another rule: %s", refusal(err))
	}
	f.finalAsPublished(t, "after the refused assignment")
}

// Every change of a movement is a step of its revision by one. Past SEA, a movement that SEA had returned to OPEN
// for a new seal could be made FINAL again at the revision its facts were published at; and a FINAL movement could
// be opened, changed and made FINAL again with its revision never moving. Both left a FINAL movement on facts that
// no longer held.
func TestAMovementIsNotMadeFinalAgainPastSEAOnFactsThatNoLongerHold(t *testing.T) {
	const step = "changes with a step of its revision by one"
	movement := func(set string) string {
		return `UPDATE cargo_sea.ocean_movement SET ` + set + `, updated_at=now() WHERE tenant_id=$1 AND movement_id='OM-1'`
	}
	t.Run("back to the published revision", func(t *testing.T) {
		f := newFixture(t)
		f.publishOwnFacts(t)
		if sealed, err := f.seal("seal-2", "CV-1", 4, "CUS-1", api.SealCustoms, 6); err != nil || sealed.SealNo != 2 {
			t.Fatalf("SEA's customs seal: %+v %v", sealed, err)
		}
		reopened := func(name string) {
			t.Helper()
			facts, err := f.module.GetOceanDocFacts(context.Background(), api.GetOceanDocFacts{TenantID: f.tenant, MovementID: "OM-1"})
			if m := f.get(t, "OM-1"); err != nil || m.State != api.StateOpen || m.Revision != 6 || facts.Status != api.DocFactsSuperseded {
				t.Fatalf("%s: movement %s at %d, facts %s %v", name, m.State, m.Revision, facts.Status, err)
			}
		}
		reopened("after SEA's seal")
		refusedBy(t, "FINAL at the earlier revision", f.past(t, movement(`state='FINAL', revision=revision-1`)), "23001", step)
		refusedBy(t, "FINAL at the same revision", f.past(t, movement(`state='FINAL'`)), "23001", step)
		// with the step, a FINAL movement needs a facts version written at its revision
		refusedBy(t, "FINAL at the next revision without facts", f.past(t, movement(`state='FINAL', revision=revision+1`)), "23514", "ocean_movement_consistent")
		reopened("after the refused finalizations")
	})
	t.Run("opened and made final at one revision", func(t *testing.T) {
		f := newFixture(t)
		f.publishOwnFacts(t)
		refusedBy(t, "OPEN without a step", f.past(t, movement(`state='OPEN'`)), "23001", step)
		refusedBy(t, "a step of two", f.past(t, movement(`state='OPEN', revision=revision+2`)), "23001", step)
		f.finalAsPublished(t, "after the refused reopenings")
		// opened with a step, the movement takes the seal; made FINAL again it is at a later revision than its facts
		if err := f.past(t, movement(`state='OPEN', revision=revision+1`)); err != nil {
			t.Fatalf("OPEN with a step: %v", err)
		}
		if err := f.past(t, `WITH v AS (UPDATE cargo_sea.container_visit SET revision=revision+1 WHERE tenant_id=$1 AND visit_id='CV-1' RETURNING visit_id)
            INSERT INTO cargo_sea.container_seal (tenant_id,visit_id,seal_no,number,party,applied_at,source,recorded_by,recorded_at,entered_at)
            SELECT $1,visit_id,2,'CUS-9','CUSTOMS',now(),'x','x',now(),now() FROM v`); err != nil {
			t.Fatalf("a seal on the reopened movement: %v", err)
		}
		refusedBy(t, "FINAL again without a step", f.past(t, movement(`state='FINAL'`)), "23001", step)
		refusedBy(t, "FINAL again without new facts", f.past(t, movement(`state='FINAL', revision=revision+1`)), "23514", "ocean_movement_consistent")
		facts, err := f.module.GetOceanDocFacts(context.Background(), api.GetOceanDocFacts{TenantID: f.tenant, MovementID: "OM-1"})
		if m := f.get(t, "OM-1"); err != nil || m.State != api.StateOpen || m.Revision != 6 || facts.Status != api.DocFactsSuperseded {
			t.Fatalf("the movement stays OPEN on superseded facts: %s at %d, facts %s %v", m.State, m.Revision, facts.Status, err)
		}
	})
}

// A signatory's name leaves the database only against a read that was logged and committed before. Past SEA, the
// function that logged the read and returned the name could be called in a transaction, or a savepoint, that was
// then rolled back: the caller kept the name and the log had no entry.
func TestASignatoryIsReadOnlyAgainstACommittedLogEntry(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.stuffOwnContainer(t)
	logged := func() (n int) {
		t.Helper()
		if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			return e.QueryRow(ctx, `SELECT count(*) FROM cargo_sea.vgm_signatory_access WHERE tenant_id=$1 AND visit_id='CV-1'`, f.tenant).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	const log = `SELECT cargo_sea.log_vgm_signatory_read('CV-1',1,'nobody-in-particular','CUSTOMS_AUDIT')::text`
	const read = `SELECT cargo_sea.read_logged_vgm_signatory($1::uuid)`
	// the function that did both in one call is gone
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		_, err := e.Exec(ctx, `SELECT cargo_sea.read_vgm_signatory('CV-1',1,'nobody-in-particular','CUSTOMS_AUDIT')`)
		return err
	}); sqlState(err) != "42883" {
		t.Fatalf("the earlier function: %v (state %s), want 42883", err, sqlState(err))
	}
	// logged and read in one transaction: no name, whether the transaction commits or not
	rollback := errors.New("roll back")
	for _, end := range []error{rollback, nil} {
		var access, name *string
		err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			if err := e.QueryRow(ctx, log).Scan(&access); err != nil {
				return err
			}
			if err := e.QueryRow(ctx, read, access).Scan(&name); err != nil {
				return err
			}
			return end
		})
		if !errors.Is(err, end) || access == nil || name != nil {
			t.Fatalf("logged and read in one transaction (ending %v): entry %v, name %v, %v", end, access, name, err)
		}
	}
	// logged in a savepoint that is rolled back, read in the same transaction: no name
	var name *string
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		var access *string
		if _, err := e.Exec(ctx, `SAVEPOINT p`); err != nil {
			return err
		}
		if err := e.QueryRow(ctx, log).Scan(&access); err != nil {
			return err
		}
		if _, err := e.Exec(ctx, `ROLLBACK TO SAVEPOINT p`); err != nil {
			return err
		}
		return e.QueryRow(ctx, read, access).Scan(&name)
	}); err != nil || name != nil {
		t.Fatalf("logged in a savepoint rolled back: name %v, %v", name, err)
	}
	// an entry that was rolled back, read in a later transaction: no name
	var lost *string
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		if err := e.QueryRow(ctx, log).Scan(&lost); err != nil {
			return err
		}
		return rollback
	}); !errors.Is(err, rollback) || lost == nil {
		t.Fatalf("an entry rolled back: %v %v", lost, err)
	}
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error { return e.QueryRow(ctx, read, lost).Scan(&name) }); err != nil || name != nil {
		t.Fatalf("read against an entry rolled back: name %v, %v", name, err)
	}
	// only the transaction that committed its entry left one, and no name has left the database
	if n := logged(); n != 1 {
		t.Fatalf("log entries after the refused reads: %d, want the one that was committed", n)
	}
	// an entry committed first, read in a later transaction: the name
	var access *string
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error { return e.QueryRow(ctx, log).Scan(&access) }); err != nil || access == nil {
		t.Fatalf("an entry committed: %v %v", access, err)
	}
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error { return e.QueryRow(ctx, read, access).Scan(&name) }); err != nil ||
		name == nil || *name != "Ada Lovelace" {
		t.Fatalf("read against a committed entry: name %v, %v", name, err)
	}
	// an entry of this tenant is not read in another's scope
	other := *f
	other.tenant = f.tenant + "-other"
	name = nil
	if err := other.within(t, func(ctx context.Context, e bentopg.Executor) error { return e.QueryRow(ctx, read, access).Scan(&name) }); err != nil || name != nil {
		t.Fatalf("read in another tenant's scope: name %v, %v", name, err)
	}
	// SEA's own read commits its entry and then reads against it
	got, err := f.module.ReadVgmSignatory(ctx, api.ReadVgmSignatory{TenantID: f.tenant, ActorID: "auditor-1", VisitID: "CV-1", Version: 1,
		Purpose: string(kernel.PurposeCustomsAudit)})
	if err != nil || got.Name != "Ada Lovelace" || logged() != 3 {
		t.Fatalf("SEA's read: %+v %v, %d log entries", got, err, logged())
	}
}

// The request hash of AssertVgm does not cover the signatory's name. Before, it did: with the other fields of the
// command, which the application role may read, a guessed name could be confirmed against the receipt's hash.
func TestAVgmReceiptDoesNotConfirmTheSignatory(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.stuffOwnContainer(t) // AssertVgm "vgm-1" with the signatory "Ada Lovelace"
	sent := api.AssertVgm{CommandID: "vgm-1", TenantID: f.tenant, ActorID: "ops-1", VisitID: "CV-1", ExpectedRevision: 3,
		Mass: kernel.Quantity{Unit: "KGM", Amount: 2200}, Method: api.VgmMethod1, ResponsiblePartyID: "LE", ResponsiblePartyRevision: 1,
		DeterminedAt: f.hours(4), Signatory: "Ada Lovelace", OccurredAt: f.hours(4)}
	hashOf := func(cmd api.AssertVgm) string {
		blob, err := json.Marshal(cmd)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(append([]byte(api.ActionAssertVgm+"\x00"), blob...))
		return hex.EncodeToString(sum[:])
	}
	var stored string
	if err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
		return e.QueryRow(ctx, `SELECT request_sha256::text FROM cargo_sea.command_receipt WHERE tenant_id=$1 AND command_id='vgm-1'`, f.tenant).Scan(&stored)
	}); err != nil {
		t.Fatal(err)
	}
	unnamed := sent
	unnamed.Signatory = ""
	if stored == hashOf(sent) || stored != hashOf(unnamed) {
		t.Fatalf("the stored hash %s covers the name (%v) or is not the command's without it (%v)", stored, stored == hashOf(sent), stored == hashOf(unnamed))
	}
	// the command again is answered its receipt, and so is a retry that states another name: the name is not compared
	first, err := f.module.AssertVgm(ctx, sent)
	if err != nil || first.AssertionNo != 1 || first.NewRevision != 4 {
		t.Fatalf("the replay: %+v %v", first, err)
	}
	renamed := sent
	renamed.Signatory = "Grace Hopper"
	if again, err := f.module.AssertVgm(ctx, renamed); err != nil || again != first {
		t.Fatalf("a retry with another name: %+v %v", again, err)
	}
	// any other field changed is another command under the id
	heavier := sent
	heavier.Mass.Amount = 2300
	if _, err = f.module.AssertVgm(ctx, heavier); !errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("another mass under the id: %v", err)
	}
	// the name stored is the first
	got, err := f.module.ReadVgmSignatory(ctx, api.ReadVgmSignatory{TenantID: f.tenant, ActorID: "auditor-1", VisitID: "CV-1", Version: 1,
		Purpose: string(kernel.PurposeCustomsAudit)})
	if err != nil || got.Name != "Ada Lovelace" {
		t.Fatalf("the name stored: %+v %v", got, err)
	}
}

// A receipt written before V2.71 was hashed with the name. The same command is still answered it; another name
// under that id is another payload, as it was then.
func TestAVgmReceiptHashedWithTheNameIsStillAnswered(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.stuffOwnContainer(t)
	sent := api.AssertVgm{CommandID: "vgm-earlier", TenantID: f.tenant, ActorID: "ops-1", VisitID: "CV-1", ExpectedRevision: 3,
		Mass: kernel.Quantity{Unit: "KGM", Amount: 2200}, Method: api.VgmMethod1, ResponsiblePartyID: "LE", ResponsiblePartyRevision: 1,
		DeterminedAt: f.hours(4), Signatory: "Ada Lovelace", OccurredAt: f.hours(4)}
	blob, err := json.Marshal(sent)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(append([]byte(api.ActionAssertVgm+"\x00"), blob...))
	// the receipt as V2.70 stored it for this command: the visit's revision 4, assertion 1
	if err = f.past(t, fmt.Sprintf(`INSERT INTO cargo_sea.command_receipt (tenant_id,command_id,request_sha256,result_type,result_id,result_revision,
        result_assertion_no) VALUES ($1,'vgm-earlier','%s','ContainerVisit','CV-1',4,1)`, hex.EncodeToString(sum[:]))); err != nil {
		t.Fatalf("a receipt as V2.70 stored it: %v", err)
	}
	if again, err := f.module.AssertVgm(ctx, sent); err != nil || again.AssertionNo != 1 || again.NewRevision != 4 || again.VisitID != "CV-1" {
		t.Fatalf("the earlier receipt: %+v %v", again, err)
	}
	renamed := sent
	renamed.Signatory = "Grace Hopper"
	if _, err = f.module.AssertVgm(ctx, renamed); !errors.Is(err, api.ErrIdempotencyConflict) {
		t.Fatalf("another name under an earlier receipt: %v", err)
	}
}

// Published facts may contain the word "signatory" in a goods description; they have no member named for one.
// Before, the database refused the word anywhere in the text: such a house could never be finalized, and the
// finalization failed with the database's error instead of a refusal.
func TestAGoodsDescriptionMayContainTheWordSignatory(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.stuffOwnContainer(t)
	const description = "Rubber stamps for the authorised signatory"
	f.w.plan("SH-1", "C-1", 2)
	f.w.mu.Lock()
	p := f.w.plans["SH-1"]
	p.Parts[0].GoodsDescription = description
	f.w.plans["SH-1"] = p
	f.w.mu.Unlock()
	f.stuffing("SH-1", "X-1", 1, "22G1", "CN-SEAL-1")
	if _, err := f.module.RefreshHouseBillFacts(ctx, api.RefreshHouseBillFacts{CommandID: "refresh-2", TenantID: f.tenant, ActorID: "ops-1", MovementID: "OM-1",
		ExpectedRevision: 4, ShipmentID: "SH-1", OccurredAt: f.hours(5)}); err != nil {
		t.Fatal(err)
	}
	got, err := f.module.FinalizeOceanDocFacts(ctx, api.FinalizeOceanDocFacts{CommandID: "fin-word", TenantID: f.tenant, ActorID: "ops-1", MovementID: "OM-1",
		ExpectedRevision: 5, OccurredAt: f.hours(6)})
	if err != nil || got.FactsVersion != 1 || f.get(t, "OM-1").State != api.StateFinal {
		t.Fatalf("finalized: %+v %v", got, err)
	}
	content := f.publishedContent(t, "OM-1", 1)
	if !strings.Contains(content, description) || strings.Contains(content, "Ada") || strings.Contains(content, "Lovelace") {
		t.Fatalf("the published facts: %s", content)
	}
	// a member named for a signatory is still refused, at any depth; the facts written here are otherwise well formed
	for name, forged := range map[string]string{
		"at the top":     `{"version":2,"movement_id":"OM-1","houses":[{"hbl":"X"}],"signatory":"Ada Lovelace"}`,
		"in a container": `{"version":2,"movement_id":"OM-1","houses":[{"hbl":"X"}],"containers":[{"vgm":{"vgm_signatory_name":"Ada Lovelace"}}]}`,
	} {
		err = f.past(t, fmt.Sprintf(`INSERT INTO cargo_sea.ocean_doc_facts_version (tenant_id,movement_id,version_no,content,content_sha256,
            movement_revision,finalized_by,finalized_at,entered_at)
            SELECT $1,'OM-1',2,s.c,encode(sha256(convert_to(s.c,'UTF8')),'hex'),7,'x',now(),now() FROM (SELECT '%s'::text AS c) s`, forged))
		refusedBy(t, "a signatory "+name, err, "23514", "ocean_doc_facts_version_content")
	}
}
