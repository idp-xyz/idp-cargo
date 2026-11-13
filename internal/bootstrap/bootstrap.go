// Package bootstrap is Cargo's composition root. It owns the connection pool and gives every bounded
// context, and the Outbox relay, an IDP-Bento-Go transaction owner of its own (rule T1). A call from
// one context into another inside a transaction then fails with application.ErrTransactionConflict
// instead of silently joining that transaction.
package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	"go.idp.xyz/idp-cargo/internal/contexts/air"
	airapi "go.idp.xyz/idp-cargo/internal/contexts/air/api"
	"go.idp.xyz/idp-cargo/internal/contexts/book"
	bookapi "go.idp.xyz/idp-cargo/internal/contexts/book/api"
	"go.idp.xyz/idp-cargo/internal/contexts/com"
	comapi "go.idp.xyz/idp-cargo/internal/contexts/com/api"
	"go.idp.xyz/idp-cargo/internal/contexts/doc"
	docapi "go.idp.xyz/idp-cargo/internal/contexts/doc/api"
	"go.idp.xyz/idp-cargo/internal/contexts/fin"
	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	"go.idp.xyz/idp-cargo/internal/contexts/ful"
	fulapi "go.idp.xyz/idp-cargo/internal/contexts/ful/api"
	"go.idp.xyz/idp-cargo/internal/contexts/org"
	orgapi "go.idp.xyz/idp-cargo/internal/contexts/org/api"
	"go.idp.xyz/idp-cargo/internal/contexts/phy"
	phyapi "go.idp.xyz/idp-cargo/internal/contexts/phy/api"
	"go.idp.xyz/idp-cargo/internal/contexts/rate"
	rateapi "go.idp.xyz/idp-cargo/internal/contexts/rate/api"
	"go.idp.xyz/idp-cargo/internal/foundation/bento"
	"go.idp.xyz/idp-cargo/internal/foundation/messaging"
	"go.idp.xyz/idp-cargo/internal/reference/locations"
)

// Config is what every Cargo process needs to reach PostgreSQL.
type Config struct {
	PostgresDSN string
	BentoSchema string // empty means bentopg.DefaultSchema
}

// Platform holds the connection pool that all transaction owners share.
type Platform struct {
	pool   *pgxpool.Pool
	schema string
}

// Open connects to PostgreSQL and checks the Bento schema.
func Open(ctx context.Context, cfg Config) (*Platform, error) {
	if cfg.PostgresDSN == "" {
		return nil, errors.New("bootstrap: PostgreSQL DSN is required")
	}
	schema := cfg.BentoSchema
	if schema == "" {
		schema = bentopg.DefaultSchema
	}
	pool, err := pgxpool.New(ctx, cfg.PostgresDSN)
	if err != nil {
		return nil, err
	}
	p := &Platform{pool: pool, schema: schema}
	db, err := p.newDB()
	if err == nil {
		err = db.CheckSchema(ctx)
	}
	if err != nil {
		pool.Close()
		return nil, err
	}
	return p, nil
}

// Close releases the connection pool.
func (p *Platform) Close() { p.pool.Close() }

// ORG assembles the ORG bounded context.
func (p *Platform) ORG(auth orgapi.Authorizer, opts ...org.Option) (*org.Module, error) {
	db, err := p.newDB()
	if err != nil {
		return nil, err
	}
	return org.New(db, auth, opts...)
}

// RATE assembles the RATE bounded context. org is used only through its query side.
func (p *Platform) RATE(org orgapi.Queries, auth rateapi.Authorizer, margins rateapi.MarginPolicy) (*rate.Module, error) {
	db, err := p.newDB()
	if err != nil {
		return nil, err
	}
	locations, err := p.Locations()
	if err != nil {
		return nil, err
	}
	return rate.New(db, org, auth, margins, rate.WithLocations(locations))
}

// ORGForCOM is what COM asks ORG: role confirmations, party identity and address books (C9).
type ORGForCOM interface {
	orgapi.Queries
	orgapi.PartyIdentityQueries
	orgapi.AddressBookQueries
}

// COM assembles the COM bounded context. org and rate are used only through their query sides; opts configure
// COM, such as its clock or how long it retains the contacts of closed and cancelled orders.
func (p *Platform) COM(org ORGForCOM, rate rateapi.Queries, auth comapi.Authorizer, opts ...com.Option) (*com.Module, error) {
	db, err := p.newDB()
	if err != nil {
		return nil, err
	}
	return com.New(db, org, rate, auth, append([]com.Option{com.WithPartyDirectory(org, org)}, opts...)...)
}

// FUL assembles the FUL bounded context. com is used only through its query side; routes are resolved
// against the location reference data (F2), read on a transaction owner of its own. opts configure FUL; use
// FULAndPHY for a FUL that records consol attachments.
func (p *Platform) FUL(com comapi.Queries, auth fulapi.Authorizer, opts ...ful.Option) (*ful.Module, error) {
	db, err := p.newDB()
	if err != nil {
		return nil, err
	}
	locations, err := p.Locations()
	if err != nil {
		return nil, err
	}
	return ful.New(db, com, auth, append([]ful.Option{ful.WithLocations(locations)}, opts...)...)
}

// PHY assembles the PHY bounded context for consolidations. ful is used only through LoadIntent v1
// (relationship 7); routes are resolved against the location reference data (F2).
func (p *Platform) PHY(ful fulapi.LoadIntentQueries, auth phyapi.Authorizer, opts ...phy.Option) (*phy.Module, error) {
	db, err := p.newDB()
	if err != nil {
		return nil, err
	}
	locations, err := p.Locations()
	if err != nil {
		return nil, err
	}
	return phy.New(db, ful, auth, append([]phy.Option{phy.WithLocations(locations)}, opts...)...)
}

// FULAndPHY assembles FUL and PHY, which read each other's query sides: PHY reads FUL's LoadIntent v1 before it
// attaches or closes (relationship 7), and FUL reads PHY's ConsolMembership v1 before it records an attachment
// and PhysicalLoadEvidence v1 before it records load evidence (relationship 8). Neither writes to the other; the
// API client runs the steps (T6).
func (p *Platform) FULAndPHY(com comapi.Queries, fulAuth fulapi.Authorizer, phyAuth phyapi.Authorizer) (*ful.Module, *phy.Module, error) {
	forward := &phyQueries{}
	fulModule, err := p.FUL(com, fulAuth, ful.WithConsolMemberships(forward), ful.WithPhysicalEvidence(forward))
	if err != nil {
		return nil, nil, err
	}
	phyModule, err := p.PHY(fulModule, phyAuth)
	if err != nil {
		return nil, nil, err
	}
	forward.phy = phyModule
	return fulModule, phyModule, nil
}

// phyQueries forwards FUL's questions to PHY (ConsolMembership v1 and PhysicalLoadEvidence v1), which is
// assembled after FUL.
type phyQueries struct {
	phy *phy.Module
}

func (c *phyQueries) GetConsolMembership(ctx context.Context, q phyapi.GetConsolMembership) (phyapi.ConsolMembership, error) {
	if c.phy == nil {
		return phyapi.ConsolMembership{}, errors.New("bootstrap: PHY is not assembled")
	}
	return c.phy.GetConsolMembership(ctx, q)
}

func (c *phyQueries) GetPhysicalLoadEvidence(ctx context.Context, q phyapi.GetPhysicalLoadEvidence) (phyapi.PhysicalLoadEvidence, error) {
	if c.phy == nil {
		return phyapi.PhysicalLoadEvidence{}, errors.New("bootstrap: PHY is not assembled")
	}
	return c.phy.GetPhysicalLoadEvidence(ctx, q)
}

// BOOK assembles the BOOK bounded context. org confirms carriers through its query side (relationship 30);
// legs are resolved against the location reference data (F2).
func (p *Platform) BOOK(org orgapi.Queries, auth bookapi.Authorizer, opts ...book.Option) (*book.Module, error) {
	db, err := p.newDB()
	if err != nil {
		return nil, err
	}
	locations, err := p.Locations()
	if err != nil {
		return nil, err
	}
	return book.New(db, org, auth, append([]book.Option{book.WithLocations(locations)}, opts...)...)
}

// AIR assembles the AIR bounded context. book answers CarrierBooking v1 (relationship 5) and ful AirOperationPlan
// v1 (relationship 9), each through its query side.
func (p *Platform) AIR(book bookapi.Queries, ful fulapi.AirOperationPlanQueries, auth airapi.Authorizer, opts ...air.Option) (*air.Module, error) {
	db, err := p.newDB()
	if err != nil {
		return nil, err
	}
	return air.New(db, book, ful, auth, opts...)
}

// ORGForDOC is what DOC reads of ORG: legal entities, party roles, address books, party identities and the contact
// transfer register.
type ORGForDOC interface {
	orgapi.Queries
	orgapi.LegalEntityQueries
	orgapi.AddressBookQueries
	orgapi.PartyIdentityQueries
	orgapi.ContactTransferRegisterQueries
}

// DOC assembles the DOC bounded context. air answers AirDocFacts v1 (relationship 11), com OrderParties v1, org
// LegalEntityRef v1, PartyRef v1 and ContactTransferRegister v1, and fin CreditStatus v1, each through its query side.
func (p *Platform) DOC(air airapi.AirDocFactsQueries, com comapi.OrderPartyQueries, org ORGForDOC, credit finapi.CreditStatusQueries, auth docapi.Authorizer,
	opts ...doc.Option) (*doc.Module, error) {
	db, err := p.newDB()
	if err != nil {
		return nil, err
	}
	return doc.New(db, air, com, org, credit, auth, opts...)
}

// ORGForFIN is what FIN reads of ORG: party roles, legal entities and party identities.
type ORGForFIN interface {
	orgapi.Queries
	orgapi.LegalEntityQueries
	orgapi.PartyIdentityQueries
}

// FIN assembles the FIN bounded context. org answers LegalEntityRef v1, PartyRef v1 and party identities, rate
// PricingBasis v1 (and rates shipments and consolidations into it when FIN asks, RATE V0.5), ful CostDriver v1, doc
// TransportDocumentSnapshot v1 and phy PhysicalCostBasis v1, each through its query side. DOC asks FIN for
// CreditStatus v1 in turn, so FIN is assembled first with a DocumentSnapshots that is bound to DOC once DOC exists.
func (p *Platform) FIN(org ORGForFIN, rate fin.RATE, ful fulapi.CostDriverQueries, doc docapi.TransportDocumentSnapshotQueries,
	phy phyapi.PhysicalCostBasisQueries, auth finapi.Authorizer, opts ...fin.Option) (*fin.Module, error) {
	db, err := p.newDB()
	if err != nil {
		return nil, err
	}
	return fin.New(db, org, rate, ful, doc, phy, auth, opts...)
}

// ErrDocumentsUnbound is returned while a DocumentSnapshots is not bound to DOC.
var ErrDocumentsUnbound = errors.New("bootstrap: DOC's document snapshots are not bound yet")

// DocumentSnapshots forwards TransportDocumentSnapshot v1 to DOC once bound: FIN and DOC ask each other (CreditStatus
// v1 one way, TransportDocumentSnapshot v1 the other), so the one assembled first holds this until the other exists.
// Bind it once, before serving.
type DocumentSnapshots struct {
	mu  sync.RWMutex
	doc docapi.TransportDocumentSnapshotQueries
}

// Bind connects the snapshots to DOC.
func (s *DocumentSnapshots) Bind(doc docapi.TransportDocumentSnapshotQueries) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.doc = doc
}

func (s *DocumentSnapshots) GetHouseDocumentSnapshot(ctx context.Context, q docapi.GetHouseDocumentSnapshot) (docapi.TransportDocumentSnapshot, error) {
	s.mu.RLock()
	doc := s.doc
	s.mu.RUnlock()
	if doc == nil {
		return docapi.TransportDocumentSnapshot{}, ErrDocumentsUnbound
	}
	return doc.GetHouseDocumentSnapshot(ctx, q)
}

// Locations assembles the read-only location reference data (repository structure baseline §11).
func (p *Platform) Locations() (locations.Directory, error) {
	db, err := p.newDB()
	if err != nil {
		return locations.Directory{}, err
	}
	return locations.New(db)
}

// RelayConfig configures the Outbox relay that publishes to an HTTP sink.
type RelayConfig struct {
	SinkURL        string
	Batch          int
	LeaseFor       time.Duration
	PublishTimeout time.Duration
	MaxAttempts    uint32
	MaxFailures    uint32
	RetryBase      time.Duration
	RetryMax       time.Duration
	IdleWait       time.Duration
	Logger         *slog.Logger
}

// Relay assembles the Outbox relay.
func (p *Platform) Relay(cfg RelayConfig) (messaging.Relay, error) {
	db, err := p.newDB()
	if err != nil {
		return messaging.Relay{}, err
	}
	store, err := bentooutbox.NewStore(db)
	if err != nil {
		return messaging.Relay{}, err
	}
	return messaging.Relay{
		Outbox: store, Publisher: messaging.Webhook{URL: cfg.SinkURL}, Clock: bento.SystemClock{},
		Batch: cfg.Batch, LeaseFor: cfg.LeaseFor, PublishTimeout: cfg.PublishTimeout,
		MaxAttempts: cfg.MaxAttempts, MaxFailures: cfg.MaxFailures,
		RetryBase: cfg.RetryBase, RetryMax: cfg.RetryMax, IdleWait: cfg.IdleWait, Logger: cfg.Logger,
	}, nil
}

// newDB creates a new transaction owner on the shared pool.
func (p *Platform) newDB() (*bentopg.DB, error) {
	return bentopg.NewDB(p.pool, bentopg.WithSchema(p.schema))
}
