package application

import (
	"context"
	"errors"
	"testing"

	org "go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
)

type holderFake struct {
	holder org.PartyID
	held   bool
}

func (f holderFake) Holder(context.Context, org.TenantID, org.IdentifierType, string, string) (org.PartyID, bool, error) {
	return f.holder, f.held, nil
}

func identityHandler(t *testing.T, holders IdentifierHolders) (MaintainPartyIdentity, *partFake, *receiptFake, *outboxFake, *partyAuthFake) {
	t.Helper()
	p, err := org.RegisterParty("T", "P", "示例物流有限公司", "CN", now)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Activate("registry", now); err != nil {
		t.Fatal(err)
	}
	p.Acknowledge()
	parties, receipts, out, auth := &partFake{p: p, revision: 1}, &receiptFake{}, &outboxFake{}, &partyAuthFake{}
	return MaintainPartyIdentity{Tx: &txFake{}, Parties: parties, Holders: holders, Auth: auth, Receipts: receipts, Outbox: out, Clock: clockFake{}},
		parties, receipts, out, auth
}

func usccCommand() PartyIdentityCommand {
	return PartyIdentityCommand{CommandID: "I-1", Scope: Scope{TenantID: "T", ActorID: "clerk"}, PartyID: "P", ExpectedRevision: 1,
		Action: IdentityRecordIdentifier, Identifier: &org.PartyIdentifier{Type: org.IdentifierCNUSCC, Country: "CN", Value: "91350100M000100Y43", EvidenceRef: "registry"}}
}

func TestMaintainPartyIdentityRecordsOneRevision(t *testing.T) {
	h, parties, receipts, out, auth := identityHandler(t, holderFake{})
	r, err := h.Handle(context.Background(), usccCommand())
	if err != nil || r.ResultType != ResultPartyIdentity || r.NewRevision != 2 || parties.saves != 1 || out.calls != 1 || receipts.saves != 1 {
		t.Fatalf("receipt %+v err %v; saves %d events %d receipts %d", r, err, parties.saves, out.calls, receipts.saves)
	}
	if len(auth.actions) != 1 || auth.actions[0] != ActionRecordPartyIdentifier {
		t.Fatalf("authorized %v", auth.actions)
	}
	if added, _ := parties.p.IdentityChanges(); len(added) != 0 {
		t.Fatal("changes are forgotten after the commit")
	}
	if replay, err := h.Handle(context.Background(), usccCommand()); err != nil || replay != r || parties.saves != 1 {
		t.Fatalf("replay %+v %v", replay, err)
	}
}

func TestMaintainPartyIdentityRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		holders IdentifierHolders
		mutate  func(*PartyIdentityCommand)
		want    error
	}{
		"held by another party": {holderFake{holder: "OTHER", held: true}, func(*PartyIdentityCommand) {}, org.ErrIdentifierInUse},
		"held by this party":    {holderFake{holder: "P", held: true}, func(*PartyIdentityCommand) {}, nil},
		"stale revision":        {holderFake{}, func(c *PartyIdentityCommand) { c.ExpectedRevision = 2 }, org.ErrConflict},
		"no identifier":         {holderFake{}, func(c *PartyIdentityCommand) { c.Identifier = nil }, ErrInvalidCommand},
		"unknown action":        {holderFake{}, func(c *PartyIdentityCommand) { c.Action = "MERGE" }, ErrInvalidCommand},
		"bad check character": {holderFake{}, func(c *PartyIdentityCommand) {
			c.Identifier = &org.PartyIdentifier{Type: org.IdentifierCNUSCC, Country: "CN", Value: "91350100M000100Y44", EvidenceRef: "e"}
		}, org.ErrInvalidIdentifier},
		"retire missing": {holderFake{}, func(c *PartyIdentityCommand) {
			c.Action, c.Identifier, c.IdentifierType, c.Country, c.Reason = IdentityRetireIdentifier, nil, org.IdentifierAEO, "CN", "typo"
		}, org.ErrUnknownIdentifier},
	} {
		h, _, receipts, _, _ := identityHandler(t, tc.holders)
		c := usccCommand()
		tc.mutate(&c)
		_, err := h.Handle(context.Background(), c)
		if tc.want == nil {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if !errors.Is(err, tc.want) || receipts.saves != 0 {
			t.Errorf("%s: err = %v, want %v; receipts %d", name, err, tc.want, receipts.saves)
		}
	}
}
