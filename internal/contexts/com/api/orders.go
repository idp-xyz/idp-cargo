package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// GetCustomerOrder reads an order for people (since V2.106 / V2.107): what was ordered, its lines as they stand,
// every obligation version allocated from them, the shipments that reserved them, the transport requested and the
// order's status. It carries no contact: the parties and their contacts are GetOrderParties, under their own
// permission. TenantID must come from a verified principal.
type GetCustomerOrder struct {
	TenantID string
	OrderID  string
}

// OrderLineView is a line as it stands, at its latest version.
type OrderLineView struct {
	LineNo      int
	Version     int64
	Description string
	PackageType string
	Packs       int64
	GrossWeight kernel.Quantity
	Volume      kernel.Quantity // zero when not declared
}

// ObligationView is one version of an obligation.
type ObligationView struct {
	ObligationID string
	Version      int64
	LineNo       int
	Packs        int64
	GrossWeight  kernel.Quantity
	Volume       kernel.Quantity
	Status       string // ObligationActive or ObligationRevoked
	AllocatedBy  string
	AllocatedAt  time.Time
	RevokedBy    string
	RevokedAt    time.Time
}

// ReservationView is a shipment's reservation of an obligation version; ReleasedAt is zero while it holds it.
type ReservationView struct {
	No                int
	ObligationID      string
	ObligationVersion int64
	ShipmentID        string
	ReservedBy        string
	ReservedAt        time.Time
	ReleasedBy        string
	ReleasedAt        time.Time
}

// CustomerOrder is an order as GetCustomerOrder answers it.
type CustomerOrder struct {
	OrderID               string
	Revision              int64
	Status                string
	LegalEntityID         string
	CustomerPartyID       string
	CustomerPartyRevision int64
	CustomerLegalName     string
	QuoteID               string
	QuoteRevision         int64
	Transport             Transport
	Lines                 []OrderLineView
	Obligations           []ObligationView
	Reservations          []ReservationView
	RecordedBy            string
	RecordedAt            time.Time
	CancelledBy           string
	CancelledAt           time.Time
	ClosedBy              string
	ClosedAt              time.Time
}

// OrderQueries is COM's read of an order for people.
type OrderQueries interface {
	GetCustomerOrder(ctx context.Context, q GetCustomerOrder) (CustomerOrder, error)
}
