package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// CustomerRef identifies the ORG party an order is for, at the revision ORG confirmed when the order was
// recorded.
type CustomerRef struct {
	PartyID       string
	PartyRevision int64
}

// ObligationSnapshot is the immutable obligation snapshot of FulfillmentObligationAllocated v1: what COM
// committed to fulfil, for FUL to plan against. It carries no names, which FUL does not consume (MD-05).
// Quantity is the obligation's packs in the package unit of UN/ECE Recommendation 20, X followed by the
// Recommendation 21 package type, such as XPX for pallets. Version 1 knows allocations only: a version
// that revokes an obligation is not found. ObligationSnapshotV2 adds weight, volume, status and
// reservation; v1 stays published until FUL has moved to v2, as the structure baseline requires for
// breaking contract changes.
type ObligationSnapshot struct {
	TenantID          string
	ObligationID      string
	ObligationVersion int64
	OrderID           string
	OrderLineNo       int
	Description       string
	Quantity          kernel.Quantity
	LegalEntityID     string
	Customer          CustomerRef
	QuoteID           string
	QuoteRevision     int64
	AllocatedAt       time.Time
}

// Obligation version statuses in ObligationSnapshotV2.
const (
	ObligationActive  = "ACTIVE"
	ObligationRevoked = "REVOKED"
)

// ObligationSnapshotV2 is one version of a fulfillment obligation as FUL plans against it: the packs,
// package type and declared gross weight and volume COM committed (C3); the version's status and the
// obligation's latest version (C6); and the shipment that holds the version, if exactly one does. Since F1
// several shipments may hold one version, each planning a part of it; HeldByShipmentID is then empty and
// ObligationSnapshotV3 lists them. It carries no names (MD-05).
type ObligationSnapshotV2 struct {
	TenantID          string
	ObligationID      string
	ObligationVersion int64
	LatestVersion     int64
	Status            string // ObligationActive or ObligationRevoked
	OrderID           string
	OrderLineNo       int
	Description       string
	Packs             int64
	PackageType       string
	GrossWeight       kernel.Quantity
	Volume            kernel.Quantity // zero when not declared
	LegalEntityID     string
	Customer          CustomerRef
	QuoteID           string
	QuoteRevision     int64
	AllocatedAt       time.Time
	RevokedAt         time.Time // zero unless Status is ObligationRevoked
	HeldByShipmentID  string    // the shipment holding the version when exactly one does; empty otherwise
}

// ObligationSnapshotV3 is ObligationSnapshotV2 with every shipment that holds the version (F1): FUL may
// plan a part of an obligation in a shipment only while the version is the latest, ACTIVE and held by
// that shipment. Holders are listed in the order they reserved. It carries no names (MD-05).
type ObligationSnapshotV3 struct {
	TenantID          string
	ObligationID      string
	ObligationVersion int64
	LatestVersion     int64
	Status            string // ObligationActive or ObligationRevoked
	OrderID           string
	OrderLineNo       int
	Description       string
	Packs             int64
	PackageType       string
	GrossWeight       kernel.Quantity
	Volume            kernel.Quantity // zero when not declared
	LegalEntityID     string
	Customer          CustomerRef
	QuoteID           string
	QuoteRevision     int64
	AllocatedAt       time.Time
	RevokedAt         time.Time // zero unless Status is ObligationRevoked
	HeldByShipmentIDs []string  // empty when no shipment holds the version
	// RequestedTransportMode is the transport mode the order currently requests (C11; COM V0.6), empty when
	// the order requests none. FUL plans the obligation only in a shipment of that mode.
	RequestedTransportMode string
	// RequestedContainerMode is the container mode the order currently requests (decision N1-PHY OD-9), empty when it
	// requests none: FUL plans the obligation only in a shipment of that container mode.
	RequestedContainerMode string
}

// GetObligationSnapshot asks for one version of an obligation. TenantID must come from a verified
// principal.
type GetObligationSnapshot struct {
	TenantID     string
	ObligationID string
	Version      int64
}

// Queries is COM's read side for other contexts. An unknown obligation or version yields ErrNotFound.
type Queries interface {
	GetObligationSnapshot(ctx context.Context, q GetObligationSnapshot) (ObligationSnapshot, error)
	GetObligationSnapshotV2(ctx context.Context, q GetObligationSnapshot) (ObligationSnapshotV2, error)
	GetObligationSnapshotV3(ctx context.Context, q GetObligationSnapshot) (ObligationSnapshotV3, error)
}
