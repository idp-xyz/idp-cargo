package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// Contact retention (decision P1). An order's contacts serve it while it is open. Once it is closed or
// cancelled they are restricted: kept only for the customs records, for at least three years, and shown only
// to callers allowed ActionReadRestrictedOrderContacts. When the retention period has ended they may be
// erased, every version at once; the order keeps everything else.

// Actions an Authorizer decides on for contact retention.
const (
	ActionEraseOrderContacts          = "EraseOrderContacts"
	ActionReadRestrictedOrderContacts = "ReadRestrictedOrderContacts"
	ActionReadContactAccessLog        = "ReadContactAccessLog"
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

// ListContactAccess asks for the log of order contact reads, newest first: of one order or all (OrderID
// empty), by one actor or all (ReadBy), recorded in [Since, Until) (either may be zero), at most Limit (1 to
// 1000). ActorID must be authorized for ActionReadContactAccessLog. The log is kept three years (decision P6).
type ListContactAccess struct {
	TenantID string
	ActorID  string
	OrderID  string
	ReadBy   string
	Since    time.Time
	Until    time.Time
	Limit    int
}

// ContactAccess is one logged read of an order's contacts: who, through which action, whether they were
// restricted, for what purpose, and how many contact versions it returned. It holds no contact details.
type ContactAccess struct {
	AccessID   string
	ActorID    string
	Action     string
	OrderID    string
	Restricted bool
	Purpose    kernel.AccessPurpose
	Contacts   int
	RecordedAt time.Time
}

// ContactAccessQueries is COM's read side of the contact access log.
type ContactAccessQueries interface {
	ListContactAccess(ctx context.Context, q ListContactAccess) ([]ContactAccess, error)
}
