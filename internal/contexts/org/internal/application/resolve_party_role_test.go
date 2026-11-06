package application

import (
	"context"
	"errors"
	"testing"
	"time"

	org "go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
)

type readerFake struct {
	p        *org.BusinessParty
	revision int64
	reads    int
}

func (f *readerFake) Read(context.Context, org.TenantID, org.PartyID) (*org.BusinessParty, int64, error) {
	f.reads++
	return f.p, f.revision, nil
}

type statusFake struct{ status org.LegalEntityStatus }

func (f statusFake) Status(context.Context, org.TenantID, org.LegalEntityID) (org.LegalEntityStatus, error) {
	return f.status, nil
}

type scopedTx struct {
	txFake
	tenant string
}

func (s *scopedTx) TenantScope() string { return s.tenant }

func makeQuery(t *testing.T, entity org.LegalEntityStatus) (ResolvePartyRole, *readerFake) {
	t.Helper()
	p, e := org.RestoreParty("T", "P", "Legal", "US", org.PartyActive, []org.Role{
		{Code: org.RoleCustomer, LegalEntityID: "LE", ValidFrom: now, ValidUntil: now.Add(time.Hour), EvidenceRef: "e", ApprovedBy: "a", ApprovedAt: now},
	})
	if e != nil {
		t.Fatal(e)
	}
	reader := &readerFake{p: p, revision: 4}
	return ResolvePartyRole{Tx: &txFake{}, Parties: reader, Entities: statusFake{entity}}, reader
}

func partyRoleQuery() PartyRoleQuery {
	return PartyRoleQuery{TenantID: "T", PartyID: "P", RoleCode: org.RoleCustomer, LegalEntityID: "LE", At: now.Add(time.Minute)}
}

func TestResolvePartyRoleAnswersFromOneRead(t *testing.T) {
	h, reader := makeQuery(t, org.EntityVerified)
	got, err := h.Handle(context.Background(), partyRoleQuery())
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 4 || got.Role.EvidenceRef != "e" || got.Party.ID() != "P" || reader.reads != 1 {
		t.Fatalf("answer = %+v after %d reads", got, reader.reads)
	}
}

func TestResolvePartyRoleRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		entity org.LegalEntityStatus
		edit   func(*PartyRoleQuery)
		want   error
	}{
		"missing tenant":       {org.EntityVerified, func(q *PartyRoleQuery) { q.TenantID = "" }, ErrUnauthenticated},
		"missing instant":      {org.EntityVerified, func(q *PartyRoleQuery) { q.At = time.Time{} }, ErrInvalidQuery},
		"missing legal entity": {org.EntityVerified, func(q *PartyRoleQuery) { q.LegalEntityID = "" }, ErrInvalidQuery},
		"suspended entity":     {org.EntitySuspended, func(*PartyRoleQuery) {}, org.ErrLegalEntityNotVerified},
		"role expired":         {org.EntityVerified, func(q *PartyRoleQuery) { q.At = now.Add(time.Hour) }, org.ErrRoleNotEffective},
	} {
		h, _ := makeQuery(t, tc.entity)
		q := partyRoleQuery()
		tc.edit(&q)
		if _, err := h.Handle(context.Background(), q); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

func TestResolvePartyRoleRejectsForeignTenantScope(t *testing.T) {
	h, reader := makeQuery(t, org.EntityVerified)
	h.Tx = &scopedTx{tenant: "OTHER"}
	if _, err := h.Handle(context.Background(), partyRoleQuery()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want %v", err, ErrUnauthorized)
	}
	if reader.reads != 0 {
		t.Fatal("a mismatched tenant scope must not read the party")
	}
}
