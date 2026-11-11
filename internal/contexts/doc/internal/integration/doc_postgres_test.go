//go:build integration

// Package integration runs DOC against PostgreSQL 16 with stand-ins for AIR's AirDocFacts v1, COM's OrderParties
// v1 and ORG. The real contexts together are tested by the composition root.
package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	airapi "go.idp.xyz/idp-cargo/internal/contexts/air/api"
	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	docmodule "go.idp.xyz/idp-cargo/internal/contexts/doc"
	"go.idp.xyz/idp-cargo/internal/contexts/doc/api"
	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	scope "go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

func testDB(t *testing.T) *bentopg.DB {
	t.Helper()
	dsn := os.Getenv("IDP_CARGO_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("IDP_CARGO_POSTGRES_DSN required in CI; skipping forbidden")
		}
		t.Skip("set IDP_CARGO_POSTGRES_DSN to a PostgreSQL 16 application role")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var major int
	var bypass, super bool
	if err = pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int / 10000, rolbypassrls, rolsuper
        FROM pg_roles WHERE rolname = current_user`).Scan(&major, &bypass, &super); err != nil {
		t.Fatal(err)
	}
	if major != 16 || bypass || super {
		t.Fatalf("need PostgreSQL 16 and an application role without BYPASSRLS or superuser (major %d, bypass %v, super %v)", major, bypass, super)
	}
	db, err := bentopg.NewDB(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.CheckSchema(ctx); err != nil {
		t.Fatal(err)
	}
	return db
}

func unlocode(code string) kernel.LocationCode {
	return kernel.LocationCode{Scheme: kernel.UNLOCODE, Code: code}
}

// world stands in for AIR, COM and ORG: AirDocFacts by movement, order parties by order, the issuing legal entity
// and the destination agent, which tests change.
type world struct {
	mu        sync.Mutex
	at        time.Time
	facts     map[string]airapi.AirDocFacts
	orders    map[string]comapi.OrderParties
	register  string              // the register's state; OK when empty
	safeguard string              // the kind of safeguard in force, if any
	reserved  map[string][]string // the keys ORG holds by dispatch
	crossNow  bool                // whether ORG's current consent of the shipper's contact covers going abroad
	credit    map[string]string   // FIN's credit status by debtor; NO_ACCOUNT when absent
}

// GetCreditStatus answers FIN's CreditStatus v1 from the statuses the test sets.
func (w *world) GetCreditStatus(_ context.Context, q finapi.GetCreditStatus) (finapi.CreditStatus, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := finapi.CreditStatus{LegalEntityID: q.LegalEntityID, DebtorPartyID: q.DebtorPartyID, Status: finapi.CreditNoAccount}
	if s, ok := w.credit[q.DebtorPartyID]; ok {
		out.Status, out.Revision, out.DecisionID = s, 3, q.LegalEntityID+"/"+q.DebtorPartyID+"@3"
	}
	return out, nil
}

func (w *world) GetAirDocFacts(_ context.Context, q airapi.GetAirDocFacts) (airapi.AirDocFacts, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f, ok := w.facts[q.MovementID]
	if !ok {
		return airapi.AirDocFacts{}, fmt.Errorf("%w: stub", airapi.ErrNotFound)
	}
	f.TenantID = q.TenantID
	return f, nil
}

func (w *world) GetOrderParties(_ context.Context, q comapi.GetOrderParties) (comapi.OrderParties, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	o, ok := w.orders[q.OrderID]
	if !ok {
		return comapi.OrderParties{}, fmt.Errorf("%w: stub", comapi.ErrNotFound)
	}
	o.Locks = append([]comapi.PartyLock(nil), o.Locks...)
	o.Parties = append([]comapi.OrderPartyView(nil), o.Parties...)
	for i := range o.Parties {
		if !q.WithContacts {
			o.Parties[i].Contact = nil
		} else if q.Purpose != kernel.PurposeDocuments {
			return comapi.OrderParties{}, errors.New("stub: DOC reads contacts for DOCUMENTS")
		}
	}
	return o, nil
}

func (w *world) ResolvePartyRole(_ context.Context, q orgapi.ResolvePartyRole) (orgapi.PartyRole, error) {
	if q.PartyID != "agent-ny" || q.RoleCode != orgapi.RoleOverseasAgent || q.LegalEntityID != "LE" {
		return orgapi.PartyRole{}, fmt.Errorf("%w: stub", orgapi.ErrRejected)
	}
	return orgapi.PartyRole{Ref: orgapi.PartyRef{TenantID: q.TenantID, PartyID: q.PartyID, PartyRevision: 2, IdentitySource: orgapi.IdentitySource},
		LegalName: "NY Destination Agent Inc.", CountryCode: "US", RoleCode: q.RoleCode, EnglishName: "NY Destination Agent Inc.",
		Identifiers: []orgapi.PartyIdentifier{{Type: orgapi.IdentifierUSEIN, Country: "US", Value: "123456789"}}}, nil
}

func (w *world) GetLegalEntity(_ context.Context, q orgapi.GetLegalEntity) (orgapi.LegalEntityRef, error) {
	return orgapi.LegalEntityRef{TenantID: q.TenantID, LegalEntityID: q.LegalEntityID, Revision: 1, RegisteredName: "上海示例国际货运代理有限公司",
		Jurisdiction: "CN", Status: orgapi.LegalEntityVerified}, nil
}

func (w *world) GetPartyAddresses(_ context.Context, q orgapi.GetPartyAddresses) (orgapi.PartyAddresses, error) {
	return orgapi.PartyAddresses{PartyID: q.PartyID, Revision: 1, Addresses: []orgapi.PartyAddress{{PartyID: q.PartyID, AddressID: "ADDR-1", Version: 1,
		Status: "ACTIVE", DefaultFor: []string{orgapi.AddressOffice}, Address: orgapi.Address{Name: "NY Destination Agent Inc.",
			Lines: []string{"147-35 Farmers Blvd"}, City: "Jamaica", Region: "NY", Postcode: "11434", CountryCode: "US"}}}}, nil
}

func (w *world) GetPartyAddress(context.Context, orgapi.GetPartyAddress) (orgapi.PartyAddress, error) {
	return orgapi.PartyAddress{}, errors.New("not used")
}

// GetPartyContacts answers ORG's current consent of the shipper's contact C-SH, consenting to go abroad while crossNow.
func (w *world) GetPartyContacts(_ context.Context, q orgapi.GetPartyContacts) (orgapi.PartyContacts, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if q.Purpose != kernel.PurposeDocuments || q.ActorID == "" {
		return orgapi.PartyContacts{}, errors.New("stub: DOC reads ORG's contacts for DOCUMENTS, as an actor")
	}
	if q.PartyID != "shipper-O-A" {
		return orgapi.PartyContacts{PartyID: q.PartyID}, nil
	}
	return orgapi.PartyContacts{PartyID: q.PartyID, Revision: 3, Contacts: []orgapi.PartyContact{{ContactID: "C-SH", Version: 2, Status: "ACTIVE",
		Consent: &orgapi.ContactConsent{NoticeVersion: "PI-2026-02", ProvideToOthers: true, CrossBorder: w.crossNow, Basis: orgapi.ConsentSubjectConfirmed,
			EvidenceRef: "consent/C-SH", Revision: 3}}}}, nil
}

// finalize publishes AirDocFacts version v of AM-1: MU587 PVG-JFK with houses SH-A and SH-B, 318 kg each, FINAL
// unless superseded.
func (w *world) finalize(v int, final bool, grams int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	ap := func(code, iata string) airapi.Airport { return airapi.Airport{Code: unlocode(code), IATA: iata} }
	g := func(n int64) kernel.Quantity { return kernel.Quantity{Unit: "GRM", Amount: n} }
	house := func(shipment, hawb, order string) airapi.DocHouse {
		return airapi.DocHouse{ShipmentID: shipment, HAWB: hawb, PlanVersion: 2, EvidenceVersion: 3, Basis: airapi.BasisTendered, Pieces: 6,
			GrossWeight: g(grams), Volume: kernel.Quantity{Unit: "CMQ", Amount: 1_000_000}, ChargeableWeight: g(grams), SecurityStatus: airapi.SecuritySPX,
			Screenings: []airapi.Screening{{LotID: "L-" + shipment, Outcome: "PASSED", Screener: "PVG cargo security station", Method: "XRAY",
				Reference: "AQ-" + shipment, ScreenedAt: w.at}},
			Parts: []airapi.DocPart{{ObligationID: "OB-" + shipment, ObligationVersion: 1, OrderID: order, Packs: 6, PackageType: "CT",
				GoodsDescription: "Cartons of electronics"}}}
	}
	status := airapi.DocFactsFinal
	if !final {
		status = airapi.DocFactsSuperseded
	}
	w.facts["AM-1"] = airapi.AirDocFacts{MovementID: "AM-1", Version: v, Status: status, SHA256: fmt.Sprintf("%064d", v), ConsolidationID: "C-1",
		OperatingLegalEntityID: "LE", AirlinePartyID: "airline-mu", AirlinePartyRevision: 4, AirlineDesignator: "MU", AWBPrefix: "781", MAWB: "781-12345675",
		MAWBSource: airapi.MAWBSourceBookingConfirmation, BookingID: "BK-1", CapacityVersion: 1, Origin: ap("CNPVG", "PVG"), Destination: ap("USJFK", "JFK"),
		LocationEdition: "E1", Flights: []airapi.DocFlight{{Flight: "MU587", From: ap("CNPVG", "PVG"), To: ap("USJFK", "JFK"), ETD: w.at.Add(72 * time.Hour)}},
		Pieces: 12, GrossWeight: g(2 * grams), Volume: kernel.Quantity{Unit: "CMQ", Amount: 2_000_000}, ChargeableWeight: g(2 * grams),
		ChargeableRule: "AIR-VOLUMETRIC-6000-HALF-KG-V1", SecurityStatus: airapi.SecuritySPX,
		Houses: []airapi.DocHouse{house("SH-A", "SHA0000001", "O-A"), house("SH-B", "SHA0000002", "O-B")}}
}

func view(role, party, country string, version int64) comapi.OrderPartyView {
	v := comapi.OrderPartyView{Role: role, Version: version, Kind: comapi.KindParty, PartyID: party, PartyRevision: 3, LegalName: "Party " + party,
		EnglishName: "Party " + party, CountryCode: country, Address: &comapi.FrozenAddress{Source: comapi.FromORG, ORGAddressID: "A-" + party, ORGVersion: 1,
			Address: comapi.Address{Name: "Party " + party, Lines: []string{"1 Road"}, City: "City", CountryCode: country}}}
	if country == "CN" {
		v.Identifiers = []comapi.PartyIdentifier{{Type: "CN_USCC", Country: "CN", Value: "91310000MA1FL4XQ3X"}}
	} else {
		v.Identifiers = []comapi.PartyIdentifier{{Type: "REGISTRATION", Country: country, None: true}}
	}
	return v
}

// order sets an order's parties: shipper and consignee at the given consignee version, locked by holders.
func (w *world) order(id string, consigneeVersion int64, locks ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	o := comapi.OrderParties{OrderID: id, Revision: 3 + consigneeVersion, CustomerPartyID: "customer-" + id,
		Transport: comapi.Transport{Mode: comapi.ModeAir, Movement: comapi.PortToPort},
		Parties:   []comapi.OrderPartyView{view(comapi.PartyShipper, "shipper-"+id, "CN", 1), view(comapi.PartyConsignee, "consignee-"+id, "US", consigneeVersion)}}
	for _, h := range locks {
		o.Locks = append(o.Locks, comapi.PartyLock{HolderID: h, LockedBy: "doc-1", LockedAt: w.at})
	}
	w.orders[id] = o
}

type allow struct{}

func (allow) AuthorizeDocAction(_ context.Context, _, actor, action, _ string) error {
	switch {
	case actor == "intruder":
		return errors.New("forbidden")
	case actor == "doc-junior" && (action == api.ActionApproveOwnTransportDocument || action == api.ActionOverrideCreditCheck):
		return errors.New("not a designated role")
	}
	return nil
}

type fixture struct {
	module *docmodule.Module
	db     *bentopg.DB
	w      *world
	tenant string
	at     time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	at := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Microsecond)
	f := &fixture{db: testDB(t), at: at, tenant: "cargo-doc-" + time.Now().UTC().Format("20060102T150405.000000000"),
		w: &world{at: at, facts: map[string]airapi.AirDocFacts{}, orders: map[string]comapi.OrderParties{}}}
	m, err := docmodule.New(f.db, f.w, f.w, f.w, f.w, allow{})
	if err != nil {
		t.Fatal(err)
	}
	f.module = m
	return f
}

func (f *fixture) hours(h int) time.Time { return f.at.Add(time.Duration(h) * time.Hour) }

func (f *fixture) authority(t *testing.T, id, kind, airline string, h int) {
	t.Helper()
	if _, err := f.module.RecordIssuanceAuthority(context.Background(), api.RecordIssuanceAuthority{CommandID: "auth-" + id, TenantID: f.tenant,
		ActorID: "legal-1", AuthorityID: id, LegalEntityID: "LE", Kind: kind, IssuePlace: "CNPVG", AirlinePartyID: airline,
		Qualification: kind + " qualification", Validity: kernel.ValidityInterval{From: f.at}, EvidenceRef: "evidence/" + id, OccurredAt: f.hours(h)}); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) get(t *testing.T, id string) api.TransportDocument {
	t.Helper()
	d, err := f.module.GetTransportDocument(context.Background(), api.GetTransportDocument{TenantID: f.tenant, DocumentID: id})
	if err != nil {
		t.Fatalf("document %s: %v", id, err)
	}
	return d
}

func (f *fixture) within(t *testing.T, fn func(context.Context, bentopg.Executor) error) error {
	t.Helper()
	return (scope.TenantTransactor{DB: f.db, Tenant: f.tenant}).WithinTransaction(context.Background(), func(txCtx context.Context) error {
		e, err := f.db.RequireExecutor(txCtx)
		if err != nil {
			return err
		}
		return fn(txCtx, e)
	})
}

func (f *fixture) outboxTypes(t *testing.T) []string {
	t.Helper()
	q, err := f.db.ReadExecutor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rows, err := q.Query(context.Background(), `SELECT event_type FROM bento.outbox WHERE scope=$1 AND source=$2 ORDER BY enqueued_seq`, f.tenant, api.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var types []string
	for rows.Next() {
		var kind string
		if err = rows.Scan(&kind); err != nil {
			t.Fatal(err)
		}
		types = append(types, strings.TrimSuffix(kind, ".v1"))
	}
	return types
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// The HAWB of SH-A and the MAWB of C-1 from draft to issue, an amendment issued and one abandoned, the C12 lock,
// the credit override, issuance authorities, stale facts, and a number never issued twice.
func TestTransportDocumentsOnPostgres(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if _, err := f.module.RecordIssuanceAuthority(ctx, api.RecordIssuanceAuthority{CommandID: "auth-bad", TenantID: f.tenant, ActorID: "legal-1",
		AuthorityID: "AUTH-X", LegalEntityID: "LE", Kind: api.KindMAWB, IssuePlace: "CNPVG", Qualification: "agency", Validity: kernel.ValidityInterval{From: f.at},
		EvidenceRef: "e", OccurredAt: f.at}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("an airline agency without its airline: %v", err)
	}
	f.authority(t, "AUTH-H", api.KindHAWB, "", 0)
	f.w.finalize(1, true, 318_000)
	f.w.order("O-A", 1)
	draft := api.DraftTransportDocument{CommandID: "draft-h1", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-H1", Kind: api.KindHAWB,
		MovementID: "AM-1", ShipmentID: "SH-A", PaymentIndicator: api.PaymentPrepaid, OccurredAt: f.hours(1)}
	r, err := f.module.DraftTransportDocument(ctx, draft)
	if err != nil || r.Version != 1 || r.NewRevision != 1 {
		t.Fatalf("draft: %+v %v", r, err)
	}
	if again, err := f.module.DraftTransportDocument(ctx, draft); err != nil || again != r {
		t.Fatalf("replay: %+v %v", again, err)
	}
	second := draft
	second.CommandID, second.DocumentID = "draft-h1b", "DOC-H1B"
	if _, err = f.module.DraftTransportDocument(ctx, second); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("a second HAWB for one shipment: %v", err)
	}
	if _, err = f.module.ApproveTransportDocument(ctx, api.ApproveTransportDocument{CommandID: "approve-own-junior", TenantID: f.tenant, ActorID: "doc-junior",
		DocumentID: "DOC-H1", ExpectedRevision: 1, OccurredAt: f.hours(2)}); err != nil {
		t.Fatalf("approval by another person: %v", err)
	}
	issue := func(command, actor, reason string, revision int64, h int) (api.Receipt, error) {
		return f.module.IssueTransportDocument(ctx, api.IssueTransportDocument{CommandID: command, TenantID: f.tenant, ActorID: actor, DocumentID: "DOC-H1",
			ExpectedRevision: revision, CreditOverrideReason: reason, OccurredAt: f.hours(h)})
	}
	if _, err = issue("issue-unlocked", "doc-2", "FIN not in service", 2, 3); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "NOT_LOCKED") {
		t.Fatalf("parties not locked: %v", err)
	}
	f.w.order("O-A", 1, "DOC-H1")
	if _, err = issue("issue-no-override", "doc-2", "", 2, 3); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "CREDIT_UNDECIDED") {
		t.Fatalf("no credit decision, no override: %v", err)
	}
	if _, err = issue("issue-junior", "doc-junior", "FIN not in service", 2, 3); !errors.Is(err, api.ErrUnauthorized) {
		t.Fatalf("an override by someone not allowed: %v", err)
	}
	if r, err = issue("issue-h1", "doc-2", "FIN not in service", 2, 3); err != nil || r.Version != 1 {
		t.Fatalf("issue: %+v %v", r, err)
	}
	d := f.get(t, "DOC-H1")
	if d.State != api.StateIssued || d.CurrentVersion != 1 || d.Number != "SHA0000001" || d.Versions[0].CreditBasis != api.CreditOverride ||
		d.Versions[0].ApprovedBy != "doc-junior" || d.Versions[0].SelfApproved || d.Versions[0].AuthorityID != "AUTH-H" || d.Versions[0].Status != api.VersionIssued {
		t.Fatalf("issued: %+v", d)
	}
	v, err := f.module.GetDocumentVersion(ctx, api.GetDocumentVersion{TenantID: f.tenant, DocumentID: "DOC-H1"})
	if err != nil || v.Doc.Number != "SHA0000001" || v.Doc.MAWB != "781-12345675" || v.Doc.Shipper.PartyID != "shipper-O-A" || v.Doc.Consignee.CountryCode != "US" ||
		v.Doc.Cargo.ChargeableGrams != 318_000 || v.Doc.Cargo.Lines[0].GoodsDescription != "Cartons of electronics" || v.Doc.Security.Screenings[0].Reference != "AQ-SH-A" ||
		v.Doc.IssuePlace.IATA != "PVG" || v.Doc.Issuer.Name != "上海示例国际货运代理有限公司" || len(v.SHA256) != 64 || v.Doc.Orders[0].ConsigneeVersion != 1 {
		t.Fatalf("version 1: %+v %v", v.Doc, err)
	}

	// AIR finalizes version 2 (a terminal reweigh): the HAWB is amended to version 2, version 1 superseded. The
	// amendment's draft is approved before it is issued.
	f.w.finalize(2, true, 320_000)
	approve := func(command, id string, revision int64, h int) {
		t.Helper()
		if _, err := f.module.ApproveTransportDocument(ctx, api.ApproveTransportDocument{CommandID: command, TenantID: f.tenant, ActorID: "doc-2",
			DocumentID: id, ExpectedRevision: revision, OccurredAt: f.hours(h)}); err != nil {
			t.Fatalf("%s: %v", command, err)
		}
	}
	amendIssue := func(command, id string, revision int64, h int) (api.Receipt, error) {
		return f.module.IssueDocumentAmendment(ctx, api.IssueDocumentAmendment{CommandID: command, TenantID: f.tenant, ActorID: "doc-2", DocumentID: id,
			ExpectedRevision: revision, CreditOverrideReason: "FIN not in service", OccurredAt: f.hours(h)})
	}
	if _, err = f.module.OpenDocumentAmendment(ctx, api.OpenDocumentAmendment{CommandID: "amend-h1", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-H1",
		ExpectedRevision: 3, Reason: "terminal reweigh", OccurredAt: f.hours(4)}); err != nil {
		t.Fatal(err)
	}
	if _, err = amendIssue("amended-h1-unapproved", "DOC-H1", 4, 5); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("an amendment issued unapproved: %v", err)
	}
	approve("approve-amend-h1", "DOC-H1", 4, 5)
	if r, err = amendIssue("amended-h1", "DOC-H1", 5, 5); err != nil || r.Version != 2 {
		t.Fatalf("amendment issued: %+v %v", r, err)
	}
	if d = f.get(t, "DOC-H1"); d.CurrentVersion != 2 || d.Versions[0].Status != api.VersionSuperseded || d.Versions[1].SourceFactsVersion != 2 ||
		d.Versions[1].ApprovedBy != "doc-2" {
		t.Fatalf("amended: %+v", d.Versions)
	}
	// an amendment to change the consignee: the client releases the lock, COM changes the party, the amendment is
	// abandoned and the parties do not match the issued version any more, so the lock must hold the old parties
	if _, err = f.module.OpenDocumentAmendment(ctx, api.OpenDocumentAmendment{CommandID: "amend-h1-party", TenantID: f.tenant, ActorID: "doc-1",
		DocumentID: "DOC-H1", ExpectedRevision: 6, Reason: "consignee to change", OccurredAt: f.hours(6)}); err != nil {
		t.Fatal(err)
	}
	f.w.order("O-A", 2)
	if _, err = f.module.AbandonDocumentAmendment(ctx, api.AbandonDocumentAmendment{CommandID: "abandon-unlocked", TenantID: f.tenant, ActorID: "doc-1",
		DocumentID: "DOC-H1", ExpectedRevision: 7, Reason: "kept as issued", OccurredAt: f.hours(7)}); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("abandon with the lock released: %v", err)
	}
	f.w.order("O-A", 2, "DOC-H1")
	if _, err = f.module.RefreshDocumentDraft(ctx, api.RefreshDocumentDraft{CommandID: "refresh-h1", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-H1",
		ExpectedRevision: 7, Reason: "COM's new consignee", OccurredAt: f.hours(7)}); err != nil {
		t.Fatal(err)
	}
	approve("approve-amend-h1-party", "DOC-H1", 8, 8)
	if r, err = amendIssue("amended-h1-party", "DOC-H1", 9, 8); err != nil || r.Version != 4 {
		t.Fatalf("the amendment with the new consignee: %+v %v", r, err)
	}
	if d = f.get(t, "DOC-H1"); d.Versions[2].Status != api.VersionDiscarded || d.Versions[3].Status != api.VersionIssued {
		t.Fatalf("versions: %+v", d.Versions)
	}
	if v, err = f.module.GetDocumentVersion(ctx, api.GetDocumentVersion{TenantID: f.tenant, DocumentID: "DOC-H1", Version: 4}); err != nil ||
		v.Doc.Orders[0].ConsigneeVersion != 2 {
		t.Fatalf("version 4 froze consignee version 2: %+v %v", v.Doc.Orders, err)
	}

	// The MAWB: no airline agency yet, then one; AIR's facts superseded meanwhile make it stale.
	m := api.DraftTransportDocument{CommandID: "draft-m1", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-M1", Kind: api.KindMAWB, MovementID: "AM-1",
		AgentPartyID: "agent-ny", PaymentIndicator: api.PaymentCollect, OccurredAt: f.hours(9)}
	if _, err = f.module.DraftTransportDocument(ctx, m); err != nil {
		t.Fatal(err)
	}
	if _, err = f.module.ApproveTransportDocument(ctx, api.ApproveTransportDocument{CommandID: "approve-m1-own", TenantID: f.tenant, ActorID: "doc-1",
		DocumentID: "DOC-M1", ExpectedRevision: 1, OccurredAt: f.hours(9)}); err != nil {
		t.Fatal(err)
	}
	issueM := func(command string, revision int64, h int) (api.Receipt, error) {
		return f.module.IssueTransportDocument(ctx, api.IssueTransportDocument{CommandID: command, TenantID: f.tenant, ActorID: "doc-2", DocumentID: "DOC-M1",
			ExpectedRevision: revision, OccurredAt: f.hours(h)})
	}
	if _, err = issueM("issue-m1-no-agency", 2, 10); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "ISSUANCE_BLOCKED") {
		t.Fatalf("no airline agency: %v", err)
	}
	f.authority(t, "AUTH-M", api.KindMAWB, "airline-mu", 10)
	f.w.finalize(3, false, 320_000)
	if _, err = issueM("issue-m1-superseded", 2, 10); !errors.Is(err, api.ErrRejected) || !strings.Contains(err.Error(), "SOURCE_STALE") {
		t.Fatalf("superseded facts: %v", err)
	}
	f.w.finalize(3, true, 320_000)
	if _, err = issueM("issue-m1-newer", 2, 10); !errors.Is(err, api.ErrRejected) {
		t.Fatalf("built on version 2, AIR's latest is 3: %v", err)
	}
	if _, err = f.module.RefreshDocumentDraft(ctx, api.RefreshDocumentDraft{CommandID: "refresh-m1", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-M1",
		ExpectedRevision: 2, Reason: "AirDocFacts version 3", OccurredAt: f.hours(11)}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.module.ApproveTransportDocument(ctx, api.ApproveTransportDocument{CommandID: "approve-m1", TenantID: f.tenant, ActorID: "doc-2",
		DocumentID: "DOC-M1", ExpectedRevision: 3, OccurredAt: f.hours(11)}); err != nil {
		t.Fatal(err)
	}
	if r, err = issueM("issue-m1", 4, 12); err != nil || r.Version != 2 {
		t.Fatalf("MAWB issued: %+v %v", r, err)
	}
	dm := f.get(t, "DOC-M1")
	vm, _ := f.module.GetDocumentVersion(ctx, api.GetDocumentVersion{TenantID: f.tenant, DocumentID: "DOC-M1"})
	if dm.Number != "781-12345675" || dm.Versions[0].SelfApproved != true || dm.Versions[1].CreditBasis != api.CreditNotRequired || vm.Doc.IssuerCapacity != "AGENT_OF_CARRIER" ||
		vm.Doc.Consignee.Address == nil || vm.Doc.Consignee.Address.City != "Jamaica" || len(vm.Doc.Cargo.Lines) != 2 || vm.Doc.PaymentIndicator != api.PaymentCollect {
		t.Fatalf("MAWB: %+v %+v", dm, vm.Doc)
	}
	if list, err := f.module.ListDocumentsForConsolidation(ctx, api.ListDocumentsForConsolidation{TenantID: f.tenant, ConsolidationID: "C-1"}); err != nil ||
		len(list) != 2 || list[0].Kind != api.KindMAWB || list[1].CurrentVersion != 4 {
		t.Fatalf("documents of C-1: %+v %v", list, err)
	}

	// Cancelled after issue, the HAWB's number stays with its shipment: the shipment's next HAWB issues it again; the
	// shipment then rolls to another movement and its HAWB is amended to the new master under the same number;
	// another shipment never gets the number.
	if _, err = f.module.CancelTransportDocument(ctx, api.CancelTransportDocument{CommandID: "cancel-h1", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-H1",
		ExpectedRevision: 10, Reason: "issued on the wrong consignee by mistake", OccurredAt: f.hours(13)}); err != nil {
		t.Fatal(err)
	}
	if d = f.get(t, "DOC-H1"); d.Versions[3].Status != api.VersionCancelled || d.CurrentVersion != 0 {
		t.Fatalf("cancelled: %+v", d)
	}
	again := draft
	again.CommandID, again.DocumentID, again.OccurredAt = "draft-h2", "DOC-H2", f.hours(14)
	f.w.order("O-A", 2, "DOC-H2")
	if _, err = f.module.DraftTransportDocument(ctx, again); err != nil {
		t.Fatalf("a new HAWB once the first is cancelled: %v", err)
	}
	approve("approve-h2", "DOC-H2", 1, 14)
	if _, err = f.module.IssueTransportDocument(ctx, api.IssueTransportDocument{CommandID: "issue-h2", TenantID: f.tenant, ActorID: "doc-2", DocumentID: "DOC-H2",
		ExpectedRevision: 2, CreditOverrideReason: "FIN not in service", OccurredAt: f.hours(14)}); err != nil {
		t.Fatalf("the shipment's own number again: %v", err)
	}
	f.w.mu.Lock()
	rolled := f.w.facts["AM-1"]
	rolled.MovementID, rolled.ConsolidationID, rolled.MAWB, rolled.Version, rolled.SHA256 = "AM-2", "C-2", "781-23456786", 1, fmt.Sprintf("%064d", 21)
	rolled.Houses = rolled.Houses[:1]
	f.w.facts["AM-2"] = rolled
	f.w.mu.Unlock()
	if _, err = f.module.OpenDocumentAmendment(ctx, api.OpenDocumentAmendment{CommandID: "roll-h2", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-H2",
		ExpectedRevision: 3, Reason: "rolled to the next flight under 781-23456786", MovementID: "AM-2", OccurredAt: f.hours(15)}); err != nil {
		t.Fatal(err)
	}
	approve("approve-roll-h2", "DOC-H2", 4, 15)
	if _, err = amendIssue("rolled-h2", "DOC-H2", 5, 15); err != nil {
		t.Fatal(err)
	}
	if h2 := f.get(t, "DOC-H2"); h2.MovementID != "AM-2" || h2.ConsolidationID != "C-2" || h2.Number != "SHA0000001" || h2.CurrentVersion != 2 {
		t.Fatalf("rolled HAWB: %+v", h2)
	}
	if vr, err := f.module.GetDocumentVersion(ctx, api.GetDocumentVersion{TenantID: f.tenant, DocumentID: "DOC-H2"}); err != nil || vr.Doc.MAWB != "781-23456786" {
		t.Fatalf("the amendment names the new master: %+v %v", vr.Doc.MAWB, err)
	}
	f.w.mu.Lock()
	clash := f.w.facts["AM-1"]
	clash.MovementID, clash.Version, clash.SHA256 = "AM-3", 1, fmt.Sprintf("%064d", 31)
	other := clash.Houses[1]
	other.HAWB = "SHA0000001"
	clash.Houses = []airapi.DocHouse{other}
	f.w.facts["AM-3"] = clash
	f.w.mu.Unlock()
	f.w.order("O-B", 1)
	if _, err = f.module.DraftTransportDocument(ctx, api.DraftTransportDocument{CommandID: "draft-b", TenantID: f.tenant, ActorID: "doc-1", DocumentID: "DOC-B",
		Kind: api.KindHAWB, MovementID: "AM-3", ShipmentID: "SH-B", PaymentIndicator: api.PaymentPrepaid, OccurredAt: f.hours(16)}); !errors.Is(err, api.ErrRejected) ||
		!strings.Contains(err.Error(), "NUMBER_ISSUED") {
		t.Fatalf("another shipment with the number: %v", err)
	}
	want := []string{"IssuanceAuthorityRecorded", "TransportDocumentDrafted", "TransportDocumentApproved", "TransportDocumentIssued",
		"TransportDocumentAmendmentOpened", "TransportDocumentApproved", "TransportDocumentAmended", "TransportDocumentAmendmentOpened",
		"TransportDocumentDrafted", "TransportDocumentApproved", "TransportDocumentAmended", "TransportDocumentDrafted", "TransportDocumentApproved",
		"IssuanceAuthorityRecorded", "TransportDocumentDrafted", "TransportDocumentApproved", "TransportDocumentIssued", "TransportDocumentCancelled",
		"TransportDocumentDrafted", "TransportDocumentApproved", "TransportDocumentIssued", "TransportDocumentAmendmentOpened", "TransportDocumentApproved",
		"TransportDocumentAmended"}
	if got := f.outboxTypes(t); !slices.Equal(got, want) {
		t.Fatalf("outbox %v", got)
	}

	// Past DOC on the application role.
	for name, tc := range map[string]struct {
		sql   string
		state string
	}{
		"version immutable":  {`UPDATE cargo_doc.document_version SET reason='x' WHERE tenant_id=$1`, "42501|23001"},
		"issue immutable":    {`UPDATE cargo_doc.document_version_issue SET issued_by='x' WHERE tenant_id=$1`, "42501|23001"},
		"number immutable":   {`UPDATE cargo_doc.issued_number SET number='X' WHERE tenant_id=$1`, "42501|23001"},
		"number changed":     {`UPDATE cargo_doc.transport_document SET number='X' WHERE tenant_id=$1 AND document_id='DOC-M1'`, "42501|23001"},
		"cancelled for good": {`UPDATE cargo_doc.transport_document SET state='DRAFT',cancel_reason=NULL,cancelled_by=NULL,cancelled_at=NULL WHERE tenant_id=$1 AND document_id='DOC-H1'`, "23001"},
		"state behind DOC":   {`UPDATE cargo_doc.transport_document SET state='AMENDING' WHERE tenant_id=$1 AND document_id='DOC-M1'`, "23514"},
		"MAWB moved":         {`UPDATE cargo_doc.transport_document SET movement_id='AM-9' WHERE tenant_id=$1 AND document_id='DOC-M1'`, "23001"},
		"another shipment's number": {`INSERT INTO cargo_doc.issued_number (tenant_id,legal_entity_id,kind,number,subject_id,first_document_id,first_issued_at)
            VALUES ($1,'LE','HAWB','SHA0000001','SH-Z','DOC-M1',now())`, "23505"},
		"version behind DOC": {`INSERT INTO cargo_doc.document_version (tenant_id,document_id,version_no,content,content_sha256,source_movement_id,source_version,
            reason,drafted_by,drafted_at,entered_at) SELECT tenant_id,document_id,9,content,content_sha256,source_movement_id,source_version,'x','x',now(),now()
            FROM cargo_doc.document_version WHERE tenant_id=$1 AND document_id='DOC-M1' AND version_no=1`, "23514"},
		"digest of other bytes": {`INSERT INTO cargo_doc.document_version (tenant_id,document_id,version_no,content,content_sha256,source_movement_id,source_version,
            reason,drafted_by,drafted_at,entered_at) VALUES ($1,'DOC-M1',9,'{"document_id":"DOC-M1","source":{"version":3,"movement_id":"AM-1"}}',repeat('a',64),
            'AM-1',3,'x','x',now(),now())`, "23514"},
		"authority rewritten": {`UPDATE cargo_doc.issuance_authority SET qualification='x' WHERE tenant_id=$1`, "42501|23001"},
	} {
		err := f.within(t, func(ctx context.Context, e bentopg.Executor) error {
			_, err := e.Exec(ctx, tc.sql, f.tenant)
			return err
		})
		if state := sqlState(err); state == "" || !strings.Contains(tc.state, state) {
			t.Errorf("%s: %v (state %s), want %s", name, err, state, tc.state)
		}
		t.Logf("%s refused: %v", name, err)
	}
}

// TestHouseDocumentsRaceForOneShipment drafts eight HAWBs of one shipment at once: one wins.
func TestHouseDocumentsRaceForOneShipment(t *testing.T) {
	f := newFixture(t)
	f.w.finalize(1, true, 318_000)
	f.w.order("O-B", 1)
	const workers = 8
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = f.module.DraftTransportDocument(context.Background(), api.DraftTransportDocument{CommandID: fmt.Sprintf("draft-%d", i), TenantID: f.tenant,
				ActorID: "doc-1", DocumentID: fmt.Sprintf("DOC-B%d", i), Kind: api.KindHAWB, MovementID: "AM-1", ShipmentID: "SH-B",
				PaymentIndicator: api.PaymentPrepaid, OccurredAt: f.at})
		}()
	}
	close(start)
	wg.Wait()
	committed, refused := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			committed++
		case errors.Is(err, api.ErrConflict):
			refused++
		}
	}
	if committed != 1 || refused != workers-1 {
		t.Fatalf("%d committed, %d refused: %v", committed, refused, errs)
	}
}
