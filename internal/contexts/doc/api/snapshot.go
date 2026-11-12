package api

import (
	"context"
	"time"
)

// TransportDocumentSnapshot v1 (DOC -> FIN, registered in Context Map V1.4; CUS later): the valid issued version of
// a shipment's house document, which FIN bills on (MD-19). Drafts and approved versions are never billed on, so this
// query answers issued versions only.

// GetHouseDocumentSnapshot asks for a shipment's house document: its valid issued version, or, with At, the version
// issued last at or before At (ISSUED, or SUPERSEDED by a later amendment). A shipment without a house document, whose
// house document is not issued or is cancelled, or with no version issued by At, yields ErrNotFound. TenantID must come
// from a verified principal.
type GetHouseDocumentSnapshot struct {
	TenantID   string
	ShipmentID string
	At         time.Time
}

// TransportDocumentSnapshot is TransportDocumentSnapshot v1: the document, its valid version and what a bill
// refers to. Weights are in grams; airports are UN/LOCODEs.
type TransportDocumentSnapshot struct {
	DocumentID      string
	Kind            string
	Version         int
	VersionStatus   string // ISSUED, or SUPERSEDED for a version asked at an instant before a later amendment
	SHA256          string
	Number          string
	MasterNumber    string
	ShipmentID      string
	ConsolidationID string
	LegalEntityID   string
	IssuedAt        time.Time
	Origin          string
	Destination     string
	Pieces          int64
	GrossGrams      int64
	ChargeableGrams int64
}

// TransportDocumentSnapshotQueries is DOC's read side for FIN.
type TransportDocumentSnapshotQueries interface {
	GetHouseDocumentSnapshot(ctx context.Context, q GetHouseDocumentSnapshot) (TransportDocumentSnapshot, error)
}
