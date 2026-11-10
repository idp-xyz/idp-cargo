package api

import (
	"context"
	"time"
)

// Contact retention (decision P1). An order's contacts serve it while it is open. Once it is closed or
// cancelled they are restricted: kept only for the customs records, for at least three years, and shown only
// to callers allowed ActionReadRestrictedOrderContacts. When the retention period has ended they may be
// erased, every version at once; the order keeps everything else.

// Actions an Authorizer decides on for contact retention.
const (
	ActionEraseOrderContacts          = "EraseOrderContacts"
	ActionReadRestrictedOrderContacts = "ReadRestrictedOrderContacts"
)

// Where an order's contacts stand.
const (
	ContactsInUse      = "IN_USE"     // the order is open
	ContactsRestricted = "RESTRICTED" // the order is closed or cancelled; kept until RetainedUntil
	ContactsErased     = "ERASED"
)

// ContactsStatus says where an order's contacts stand. RetainedUntil is set once the order is closed or
// cancelled, ErasedAt once its contacts are erased.
type ContactsStatus struct {
	State         string
	RetainedUntil time.Time
	ErasedAt      time.Time
}

// EraseOrderContacts erases the contacts frozen on a closed or cancelled order once their retention period
// has ended by COM's clock. It changes neither the order nor its revision; the receipt names the revision the
// order stays at. ErrRejected while the period runs, for an open order, or when the contacts are erased
// already.
type EraseOrderContacts struct {
	CommandID  string
	TenantID   string
	ActorID    string
	OrderID    string
	OccurredAt time.Time
}

// ListOrdersDueForContactErasure asks for the orders whose contacts may be erased now, earliest first, at
// most Limit (1 to 1000). Callers need ActionEraseOrderContacts.
type ListOrdersDueForContactErasure struct {
	TenantID string
	ActorID  string
	Limit    int
}

// DueOrder is an order whose contacts are due for erasure.
type DueOrder struct {
	OrderID       string
	RetainedUntil time.Time
}

// DueOrders lists the orders due at AsOf, by COM's clock.
type DueOrders struct {
	AsOf   time.Time
	Orders []DueOrder
}

// OrderContactRetention is COM's entry point for erasing the contacts of closed and cancelled orders.
type OrderContactRetention interface {
	EraseOrderContacts(ctx context.Context, cmd EraseOrderContacts) (Receipt, error)
	ListOrdersDueForContactErasure(ctx context.Context, q ListOrdersDueForContactErasure) (DueOrders, error)
}
