package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Consolidation states.
const (
	StateOpen       = "OPEN"
	StateClosed     = "CLOSED"
	StateReconciled = "RECONCILED" // its physical facts are complete (ReconcileConsolidation)
	StateCancelled  = "CANCELLED"
)

// StageMain is the main carriage, the only stage a membership has in N1.
const StageMain = "MAIN"

// How a membership entry started and ended.
const (
	EntryAttached          = "ATTACHED"
	EntryTransferred       = "TRANSFERRED"
	EntryRefreshed         = "REFRESHED"
	EntryDetached          = "DETACHED"
	EntryShipmentCancelled = "SHIPMENT_CANCELLED"
)

// ConsolidationVersion is one immutable version of a consolidation: its route, schedule and state after a
// change, and why. The original plan stays visible after a rollover.
type ConsolidationVersion struct {
	Version          int
	Change           string // CREATED, AMENDED, CLOSED, REOPENED, CANCELLED, RECONCILED
	State            string
	Origin           kernel.LocationCode
	Destination      kernel.LocationCode
	OriginIATA       string
	DestinationIATA  string
	LocationEdition  string
	PlannedDeparture time.Time
	HouseCutOff      time.Time
	Reason           string
	WrittenAt        time.Time // business time
	EnteredAt        time.Time // when PHY recorded it (T1)
}

// Member is a live house of a consolidation with the plan PHY verified and the scope it froze.
type Member struct {
	ShipmentID         string
	EntryNo            int
	MembershipRevision int64
	FULRevision        int64
	PlanVersion        int
	Packs              int64
	GrossWeight        kernel.Quantity
	Volume             kernel.Quantity // zero when the shipment declares none
	AttachedAt         time.Time
}

// PlannedLoadUnit is a load unit planned for a consolidation, with the limits of its type frozen from the
// equipment reference data (TEU in tenths; kilograms; cubic centimetres) and the edition. A unit is IN_USE while
// its stuffing is open and CLOSED once it is closed (sealed); removed units keep their number and say why.
type PlannedLoadUnit struct {
	UnitNo           int
	Kind             string // LoadUnitContainer
	TypeCode         string
	TEUTenths        int
	MaxGrossKg       int64
	TareKg           int64
	CapacityCM3      int64
	EquipmentEdition string
	Status           string // UnitStatusPlanned, UnitStatusInUse, UnitStatusClosed or UnitStatusRemoved
	ExecutionID      string // the stuffing opened on the unit
	PlannedAt        time.Time
	RemoveReason     string
	RemovedAt        time.Time
}

// Consolidation is a consolidation with its current version, its version history, its live houses and its
// planned load units. It carries no names and no carrier references.
type Consolidation struct {
	TenantID               string
	ConsolidationID        string
	Revision               int64
	Mode                   string
	Kind                   string
	OperatingLegalEntityID string
	State                  string
	Current                ConsolidationVersion
	Versions               []ConsolidationVersion
	Members                []Member
	LoadUnits              []PlannedLoadUnit // every unit planned, removed ones included (PHY V0.3)
}

// ConsolidationSnapshot is what ConsolMembership v1 tells of the consolidation a house is in.
type ConsolidationSnapshot struct {
	ConsolidationID        string
	Revision               int64
	Mode                   string
	Kind                   string
	OperatingLegalEntityID string
	State                  string
	Origin                 kernel.LocationCode
	Destination            kernel.LocationCode
	OriginIATA             string
	DestinationIATA        string
	LocationEdition        string
	PlannedDeparture       time.Time
	HouseCutOff            time.Time
}

// EntryPart is a part of a COM obligation version the house planned when PHY verified it.
type EntryPart struct {
	ObligationID      string
	ObligationVersion int64
	OrderID           string
	Packs             int64
	PackageType       string
	GrossWeight       kernel.Quantity
	Volume            kernel.Quantity
}

// MembershipEntry is one immutable entry of a house's membership ledger: the consolidation, the FUL revision
// and plan version PHY verified and the scope it froze, and, once ended, how and why.
type MembershipEntry struct {
	EntryNo         int
	ConsolidationID string
	Start           string // EntryAttached, EntryTransferred or EntryRefreshed
	PredecessorNo   int
	FULRevision     int64
	PlanVersion     int
	Packs           int64
	GrossWeight     kernel.Quantity
	Volume          kernel.Quantity
	Parts           []EntryPart
	AttachedAt      time.Time // business time
	EnteredAt       time.Time // when PHY recorded it (T1)
	End             string    // empty while live; EntryDetached, EntryTransferred, EntryRefreshed or EntryShipmentCancelled
	Reason          string
	EndedAt         time.Time
	SuccessorNo     int
}

// ConsolMembership is ConsolMembership v1 (relationship 8, PHY -> FUL): a shipment's membership on its main
// carriage. When Live, Entry is the live entry and Consolidation the consolidation it is in; History holds
// every entry in order, the live one included. FUL records its attachment from it and never decides it.
type ConsolMembership struct {
	TenantID      string
	ShipmentID    string
	Stage         string
	Revision      int64
	Live          bool
	Entry         MembershipEntry
	Consolidation ConsolidationSnapshot
	History       []MembershipEntry
}

// GetConsolidation asks for a consolidation. TenantID must come from a verified principal.
type GetConsolidation struct {
	TenantID        string
	ConsolidationID string
}

// GetConsolMembership asks for a shipment's membership. A shipment never attached yields ErrNotFound.
type GetConsolMembership struct {
	TenantID   string
	ShipmentID string
}

// ListOpenConsolidations asks for OPEN consolidations for the attach screen, by planned departure. Empty
// filters match everything; DepartureFrom and DepartureUntil bound the planned departure as [from, until).
// Limit is 1 to 200; zero means 50.
type ListOpenConsolidations struct {
	TenantID               string
	Mode                   string
	Origin                 kernel.LocationCode // UN/LOCODE as frozen on the consolidation
	Destination            kernel.LocationCode
	OperatingLegalEntityID string
	DepartureFrom          time.Time
	DepartureUntil         time.Time
	Limit                  int
}

// ConsolidationSummary is a row of ListOpenConsolidations.
type ConsolidationSummary struct {
	ConsolidationID        string
	Revision               int64
	Mode                   string
	Kind                   string
	OperatingLegalEntityID string
	Origin                 kernel.LocationCode
	Destination            kernel.LocationCode
	OriginIATA             string
	DestinationIATA        string
	PlannedDeparture       time.Time
	HouseCutOff            time.Time
	LiveHouses             int
}

// ConsolMembershipQueries is ConsolMembership v1, which FUL reads (relationship 8).
type ConsolMembershipQueries interface {
	GetConsolMembership(ctx context.Context, q GetConsolMembership) (ConsolMembership, error)
}

// Queries is PHY's read side for consolidations and their physical facts. An unknown consolidation yields
// ErrNotFound.
type Queries interface {
	ConsolMembershipQueries
	PhysicalCostBasisQueries
	GetConsolidation(ctx context.Context, q GetConsolidation) (Consolidation, error)
	ListOpenConsolidations(ctx context.Context, q ListOpenConsolidations) ([]ConsolidationSummary, error)
	PhysicalQueries
}
