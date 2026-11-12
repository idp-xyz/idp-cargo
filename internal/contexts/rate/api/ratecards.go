package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// RATE V0.5: the charge-code catalogue (MD-27), rate cards and rate evaluation (R6). Commands take the same envelope
// as quote commands: TenantID and ActorID from a verified principal, OccurredAt the business time within the record
// window (T1). A replayed command returns its receipt; SubjectID is the code, the card or the snapshot.

// Charge groups, calculators, rate units, modes, sides, card scopes and version statuses.
const (
	GroupFreight       = "FREIGHT"
	GroupOrigin        = "ORIGIN"
	GroupDestination   = "DESTINATION"
	GroupDocumentation = "DOCUMENTATION"
	GroupCustoms       = "CUSTOMS"
	GroupSurcharge     = "SURCHARGE"
	GroupDisbursement  = "DISBURSEMENT"
	GroupDiscount      = "DISCOUNT"

	CalcFlat      = "FLAT"
	CalcPerUnit   = "PER_UNIT"
	CalcPercentOf = "PERCENT_OF"

	UnitKgChargeable = "KG_CHARGEABLE" // grams
	UnitKgGross      = "KG_GROSS"      // grams
	UnitCBM          = "CBM"           // cubic centimetres
	UnitFreightTonne = "FREIGHT_TONNE" // thousandths of a W/M
	UnitPiece        = "PIECE"
	UnitDocument     = "DOCUMENT"

	ModeAir        = "AIR"
	ModeSea        = "SEA"
	ContainerLoose = "LOOSE"
	ContainerLCL   = "LCL"

	SideSell      = "SELL"
	SideBuy       = "BUY"
	ScopeTariff   = "TARIFF"
	ScopeCustomer = "CUSTOMER"
	ScopeSupplier = "SUPPLIER"

	VersionDraft     = "DRAFT"
	VersionPublished = "PUBLISHED"
	VersionWithdrawn = "WITHDRAWN"

	CommodityGeneral = "GENERAL"
)

// ChargeCode is a code of the tenant's catalogue at its current version. Version 0 is a starter code the tenant has
// not changed.
type ChargeCode struct {
	Code       string
	NameZH     string
	NameEN     string
	Group      string
	Calculator string
	Unit       string // PER_UNIT codes only
	Modes      []string
	Active     bool
	Version    int
	SetBy      string
	SetAt      time.Time
}

// DefineChargeCode adds a code (two to sixteen upper-case letters, digits or underscores starting with a letter) not
// yet in the catalogue. ReviseChargeCode changes an active code's names, group, defaults or modes at ExpectedVersion
// (0 for a starter code); the code never changes. DeactivateChargeCode stops new rate lines from using it;
// ReactivateChargeCode lets them again.
type (
	DefineChargeCode struct {
		CommandID  string
		TenantID   string
		ActorID    string
		Code       string
		NameZH     string
		NameEN     string
		Group      string
		Calculator string
		Unit       string
		Modes      []string
		OccurredAt time.Time
	}
	ReviseChargeCode struct {
		CommandID       string
		TenantID        string
		ActorID         string
		Code            string
		ExpectedVersion int
		NameZH          string
		NameEN          string
		Group           string
		Calculator      string
		Unit            string
		Modes           []string
		OccurredAt      time.Time
	}
	DeactivateChargeCode struct {
		CommandID       string
		TenantID        string
		ActorID         string
		Code            string
		ExpectedVersion int
		OccurredAt      time.Time
	}
	ReactivateChargeCode struct {
		CommandID       string
		TenantID        string
		ActorID         string
		Code            string
		ExpectedVersion int
		OccurredAt      time.Time
	}
)

// RateBreak is a band of a PER_UNIT line: from From base units on, Rate applies (at the line's RateScale).
type RateBreak struct {
	From int64
	Rate int64
}

// RateLine prices one charge code of an entry. Rate is in minor units of Currency times 10^-RateScale: per shipment
// for FLAT, per kilogram, cubic metre, W/M, piece or document for PER_UNIT. PER_UNIT quantities are whole base units
// (grams, cubic centimetres, thousandths of a W/M, pieces, documents), rounded up to Step and raised to MinQuantity;
// a zero Step, MinQuantity or VolumeFactor takes the default of OD-8: KG_CHARGEABLE on AIR 500 g steps and 6000 cm3
// per kg, on SEA 1 t = 1 m3 (1000 cm3 per kg); FREIGHT_TONNE 1 m3 = 1 t (1,000,000 cm3 per tonne) and at least 1 W/M
// (1000); other units steps of 1. A PER_UNIT line rates nothing when the cargo does not state its quantity (a
// NO_QUANTITY gap). Discount codes are quote lines, never rates (R9). PERCENT_OF
// is PercentBP basis points of BaseCode's amount in the same evaluation and currency. Minimum and Maximum (0: none)
// are in minor units. A CallForPricing line has no rate: rating reports a gap. No and Edition are set in views.
type RateLine struct {
	No             int
	ChargeCode     string
	Currency       kernel.CurrencyCode
	Calculator     string
	Unit           string
	Rate           int64
	RateScale      int
	Minimum        int64
	Maximum        int64
	Step           int64
	MinQuantity    int64
	VolumeFactor   int64
	PercentBP      int64
	BaseCode       string
	CallForPricing bool
	Breaks         []RateBreak
}

// RateEntry is a lane: a mode and container mode (AIR LOOSE or SEA LCL), origin, destination and an optional via
// (UN/LOCODEs, or aliases carriers use, resolved for the mode), an optional carrier on SELL entries, a listed service
// level, the GENERAL commodity class, and optionally transit days, a frequency and a validity within the version's.
// Entries and lines are numbered in the order given.
type RateEntry struct {
	No            int
	Mode          string
	ContainerMode string
	Origin        string
	Destination   string
	Via           string
	Edition       string
	Carrier       string
	ServiceLevel  string
	Commodity     string
	TransitDays   int
	Frequency     string
	ValidFrom     time.Time
	ValidUntil    time.Time
	Lines         []RateLine
}

// CreateRateCard opens a card of a legal entity: a SELL TARIFF for any customer, a SELL CUSTOMER card of a party ORG
// confirms in a role a quote goes to (R7: CUSTOMER, OVERSEAS_AGENT, SHIPPER or CONSIGNEE), or a BUY SUPPLIER card of a
// party ORG confirms as CARRIER, CO_LOADER or OVERSEAS_AGENT.
type CreateRateCard struct {
	CommandID     string
	TenantID      string
	ActorID       string
	RateCardID    string
	Side          string
	Scope         string
	LegalEntityID string
	PartyID       string
	Name          string
	OccurredAt    time.Time
}

// DraftRateCardVersion opens the card's next version as a DRAFT, valid [ValidFrom, ValidUntil) (a zero ValidUntil
// leaves it open), copying the entries of version CopyFrom when not 0. A supplier card's version names the supplier's
// tariff or contract (SourceRef); a SEA SELL version may carry a rate filing reference.
type DraftRateCardVersion struct {
	CommandID        string
	TenantID         string
	ActorID          string
	RateCardID       string
	ExpectedRevision int64
	ValidFrom        time.Time
	ValidUntil       time.Time
	Currency         kernel.CurrencyCode
	SourceRef        string
	FilingRef        string
	CopyFrom         int
	OccurredAt       time.Time
}

// SetRateCardEntries replaces the entries of a DRAFT version.
type SetRateCardEntries struct {
	CommandID        string
	TenantID         string
	ActorID          string
	RateCardID       string
	ExpectedRevision int64
	Version          int
	Entries          []RateEntry
	OccurredAt       time.Time
}

// PublishRateCardVersion makes a DRAFT version valid for rating: no other published version of the card valid at the
// same time (RATE_VERSION_OVERLAP), charge codes listed, active and applying to the entry's mode, service levels
// listed, a start not before the record window. A SELL tariff is published by the pricing manager
// (ActionApproveTariff), who approves it; approving a version one prepared needs ActionApproveOwnTariff and is
// flagged: everyone who drafted the version or set its entries prepared it. ORG confirms a supplier card's party again. A
// CUSTOMER card is published by accepting its AGREEMENT quote.
type PublishRateCardVersion struct {
	CommandID        string
	TenantID         string
	ActorID          string
	RateCardID       string
	ExpectedRevision int64
	Version          int
	OccurredAt       time.Time
}

// WithdrawRateCardVersion ends a PUBLISHED version at the time recorded, with a reason: it still rates business times
// before then, nothing after; snapshots that used it keep it, and a later version may start then.
type WithdrawRateCardVersion struct {
	CommandID        string
	TenantID         string
	ActorID          string
	RateCardID       string
	ExpectedRevision int64
	Version          int
	Reason           string
	OccurredAt       time.Time
}

// RateCardVersionHeader is a version without its entries.
type RateCardVersionHeader struct {
	Version        int
	Status         string
	ValidFrom      time.Time
	ValidUntil     time.Time
	Currency       kernel.CurrencyCode
	SourceRef      string
	FilingRef      string
	Preparers      []string // who drafted the version and everyone who set its entries since
	PreparedAt     time.Time
	PublishedBy    string
	PublishedAt    time.Time
	ApprovedBy     string
	SelfApproved   bool
	WithdrawnBy    string
	WithdrawnAt    time.Time
	WithdrawReason string
}

// RateCard is a card with its version headers; Revision is what its next command expects.
type RateCard struct {
	RateCardID    string
	Side          string
	Scope         string
	LegalEntityID string
	PartyID       string
	PartyRevision int64
	Name          string
	CreatedBy     string
	CreatedAt     time.Time
	Revision      int64
	Versions      []RateCardVersionHeader
}

// RateCardVersion is a version with its entries.
type RateCardVersion struct {
	RateCardID string
	RateCardVersionHeader
	Entries []RateEntry
}

// Cargo is what an evaluation rates.
type Cargo struct {
	Pieces     int64
	GrossGrams int64
	VolumeCM3  int64
	Documents  int64
}

// Evaluation purposes; EvaluateRates takes QUOTE_LINE (the estimates for FIN come with PricingBasis v1).
const (
	PurposeQuoteLine          = "QUOTE_LINE"
	PurposeShipmentEstimate   = "SHIPMENT_ESTIMATE"
	PurposeConsolCostEstimate = "CONSOL_COST_ESTIMATE"
)

// EvaluateRates rates a lane and cargo at OccurredAt on the published versions valid then and records a snapshot
// (RATE never invents a price: what it cannot rate is a gap). SELL takes the customer's CUSTOMER card first and the
// tariff for the charge codes it lacks (OD-1 A); BUY takes the given carrier's cards or the lowest total among the
// supplier cards (OD-2 A). ORG confirms a named customer in a role a quote goes to (R7: CUSTOMER, OVERSEAS_AGENT,
// SHIPPER or CONSIGNEE).
type EvaluateRates struct {
	CommandID       string
	TenantID        string
	ActorID         string
	SnapshotID      string
	Purpose         string
	LegalEntityID   string
	Sides           []string
	CustomerPartyID string
	Carrier         string
	Mode            string
	ContainerMode   string
	Origin          string
	Destination     string
	Via             string
	ServiceLevel    string
	Commodity       string
	Cargo           Cargo
	SubjectKind     string // QUOTE, SHIPMENT or CONSOLIDATION, or empty
	SubjectID       string
	SubjectVersion  int64
	OccurredAt      time.Time
}

// RateCandidate is a card version and entry an evaluation considered.
type RateCandidate struct {
	Side       string
	RateCardID string
	Version    int
	EntryNo    int
	Taken      bool
	Reason     string
	Total      []kernel.Money
}

// Gaps of a snapshot line.
const (
	GapNoRate         = "NO_RATE"
	GapCallForPricing = "CALL_FOR_PRICING"
	GapBaseMissing    = "BASE_MISSING"
	GapBaseCurrency   = "BASE_CURRENCY"
	GapNoQuantity     = "NO_QUANTITY"
)

// RateSnapshotLine is a rated charge or a gap: the card, version, entry and line it came from, the quantity in base
// units, the quantity charged (a higher band's lower bound when that costs less), the band and unit rate used, the
// minimum or maximum applied, and the amount rounded half away from zero.
type RateSnapshotLine struct {
	LineNo         int
	Side           string
	ChargeCode     string
	RateCardID     string
	Version        int
	EntryNo        int
	RateLineNo     int
	Calculator     string
	Unit           string
	Quantity       int64
	Charged        int64
	BandFrom       int64
	Rate           int64
	RateScale      int
	PercentBP      int64
	BaseAmount     int64
	MinimumApplied bool
	MaximumApplied bool
	Amount         kernel.Money
	Gap            string
}

// RateSnapshot is a recorded evaluation, replayable with its EngineVersion.
type RateSnapshot struct {
	SnapshotID     string
	Purpose        string
	LegalEntityID  string
	Sides          []string
	Customer       CustomerRef
	Carrier        string
	Mode           string
	ContainerMode  string
	Origin         string
	Destination    string
	Via            string
	Edition        string
	ServiceLevel   string
	Commodity      string
	At             time.Time
	Cargo          Cargo
	SubjectKind    string
	SubjectID      string
	SubjectVersion int64
	EngineVersion  string
	Candidates     []RateCandidate
	Lines          []RateSnapshotLine
	EvaluatedBy    string
	RecordedAt     time.Time
}

// Queries of the catalogue, cards and snapshots. ListChargeCodes lists the catalogue in code order (inactive codes
// too when asked); ListRateCards selects by side, scope, legal entity, party and a time a published version is valid
// at (empty fields select all).
type (
	GetChargeCode   struct{ TenantID, Code string }
	ListChargeCodes struct {
		TenantID        string
		IncludeInactive bool
	}
	GetRateCard        struct{ TenantID, RateCardID string }
	GetRateCardVersion struct {
		TenantID   string
		RateCardID string
		Version    int
	}
	ListRateCards struct {
		TenantID      string
		Side          string
		Scope         string
		LegalEntityID string
		PartyID       string
		ValidAt       time.Time
	}
	GetRateSnapshot struct{ TenantID, SnapshotID string }
)

// RateCardCommands is RATE's command entry point for the catalogue, cards and rating.
type RateCardCommands interface {
	DefineChargeCode(ctx context.Context, cmd DefineChargeCode) (Receipt, error)
	ReviseChargeCode(ctx context.Context, cmd ReviseChargeCode) (Receipt, error)
	DeactivateChargeCode(ctx context.Context, cmd DeactivateChargeCode) (Receipt, error)
	ReactivateChargeCode(ctx context.Context, cmd ReactivateChargeCode) (Receipt, error)
	CreateRateCard(ctx context.Context, cmd CreateRateCard) (Receipt, error)
	DraftRateCardVersion(ctx context.Context, cmd DraftRateCardVersion) (Receipt, error)
	SetRateCardEntries(ctx context.Context, cmd SetRateCardEntries) (Receipt, error)
	PublishRateCardVersion(ctx context.Context, cmd PublishRateCardVersion) (Receipt, error)
	WithdrawRateCardVersion(ctx context.Context, cmd WithdrawRateCardVersion) (Receipt, error)
	EvaluateRates(ctx context.Context, cmd EvaluateRates) (Receipt, error)
}

// RateCardQueries is RATE's read side for the catalogue, cards and snapshots (FIN reads the catalogue over
// RATE -> FIN; no other context reads cards directly).
type RateCardQueries interface {
	GetChargeCode(ctx context.Context, q GetChargeCode) (ChargeCode, error)
	ListChargeCodes(ctx context.Context, q ListChargeCodes) ([]ChargeCode, error)
	GetRateCard(ctx context.Context, q GetRateCard) (RateCard, error)
	GetRateCardVersion(ctx context.Context, q GetRateCardVersion) (RateCardVersion, error)
	ListRateCards(ctx context.Context, q ListRateCards) ([]RateCard, error)
	GetRateSnapshot(ctx context.Context, q GetRateSnapshot) (RateSnapshot, error)
}

// Actions on the catalogue, cards and rating an Authorizer decides on; quoteID is then the code, card or snapshot.
const (
	ActionMaintainChargeCodes = "MaintainChargeCodes"
	ActionMaintainSellRates   = "MaintainSellRates"
	ActionMaintainBuyRates    = "MaintainBuyRates"
	ActionApproveTariff       = "ApproveTariff"    // the pricing manager publishing a SELL tariff (R1)
	ActionApproveOwnTariff    = "ApproveOwnTariff" // allow it only to roles designated to approve what they prepared (R3)
	ActionEvaluateRates       = "EvaluateRates"
)
