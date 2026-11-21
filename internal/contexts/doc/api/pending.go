package api

import "context"

// DOC V2.90: what stands between a sea house bill and a bill that says what SEA now says. DOC reads it on demand from
// the document and SEA's latest OceanDocFacts of the document's movement, as the commands do, and stores nothing.

// Kinds of pending items.
const (
	// PendingHouseNotAllLoaded: a container carrying the house has no loading in SEA's facts, or they state no loading
	// at all. Blocks issue. For a bill already issued it is not raised: see PendingIssuedContentChanged.
	PendingHouseNotAllLoaded = "HOUSE_NOT_ALL_LOADED"
	// PendingSourceSuperseded: a draft, approved bill or amendment's draft was built on facts that are no longer SEA's
	// latest FINAL version. Refresh it. An issued bill does not have it.
	PendingSourceSuperseded = "SOURCE_SUPERSEDED"
	// PendingIssuedContentChanged: what SEA's latest FINAL facts now say of the house (the on-board date, the
	// containers or seal, the packs or weights, the voyage) differs from what the issued bill states; the message names
	// what. Open an amendment. An issued bill whose facts were finalized again with nothing changed for its house has
	// no item.
	PendingIssuedContentChanged = "ISSUED_CONTENT_CHANGED"
	// PendingHouseLeft: the document's house is not in SEA's latest facts of its movement. A draft is refreshed onto
	// the movement the house was transferred to, or cancelled; an issued bill stays valid, and an amendment that names
	// the new movement moves it, or it is cancelled.
	PendingHouseLeft = "HOUSE_LEFT_MOVEMENT"
	// PendingSourceNotFinal: SEA's movement is open. Wait for SEA's finalization.
	PendingSourceNotFinal = "SOURCE_NOT_FINAL"
)

// GetPendingItemsOfMovement lists the pending items of the sea house bills (HBL and house sea waybill, neither
// cancelled nor surrendered: a surrendered one has none) whose document names the SEA movement, in the order of their
// ids. A movement SEA has published no facts of (unknown, or never finalized) has no bill to list and answers an empty
// list.
type GetPendingItemsOfMovement struct{ TenantID, MovementID string }

// GetPendingItemsOfDocument lists the pending items of one document: none for a document that is not a sea house bill
// or is cancelled or surrendered. An unknown document yields ErrNotFound.
type GetPendingItemsOfDocument struct{ TenantID, DocumentID string }

// PendingItem is one pending item. Message says who does what; Blocking says whether the bill cannot be issued while it
// stands (always false for a bill already issued, which has no issue waiting).
type PendingItem struct {
	Kind       string
	DocumentID string
	ShipmentID string
	Number     string
	Message    string
	Blocking   bool
}

// PendingQueries is DOC's read side for pending items. It is read-only and writes nothing.
type PendingQueries interface {
	GetPendingItemsOfMovement(ctx context.Context, q GetPendingItemsOfMovement) ([]PendingItem, error)
	GetPendingItemsOfDocument(ctx context.Context, q GetPendingItemsOfDocument) ([]PendingItem, error)
}
