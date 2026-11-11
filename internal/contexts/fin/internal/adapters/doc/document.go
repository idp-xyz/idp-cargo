// Package doc is FIN's anticorruption layer for DOC (DOC -> FIN): TransportDocumentSnapshot v1, the issued house
// document a debit note bills on. DOC's error kinds are kept as text only.
package doc

import (
	"context"
	"errors"
	"fmt"

	docapi "go.idp.xyz/idp-cargo/internal/contexts/doc/api"
	app "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/application"
	fin "go.idp.xyz/idp-cargo/internal/contexts/fin/internal/domain"
)

// Documents reads DOC. Call it outside FIN's transactions.
type Documents struct {
	DOC docapi.TransportDocumentSnapshotQueries
}

var _ app.Documents = Documents{}

// HouseDocument answers the valid issued version of the shipment's house document; DOC's "not found" (no house
// document, or none issued) is FIN_NO_ISSUED_DOCUMENT, anything else stays an outage.
func (d Documents) HouseDocument(ctx context.Context, tenant fin.TenantID, shipment string) (app.HouseDocument, error) {
	s, err := d.DOC.GetHouseDocumentSnapshot(ctx, docapi.GetHouseDocumentSnapshot{TenantID: string(tenant), ShipmentID: shipment})
	if errors.Is(err, docapi.ErrNotFound) {
		return app.HouseDocument{}, fmt.Errorf("%w: shipment %s: %v", fin.ErrNoIssuedDocument, shipment, err.Error())
	}
	if err != nil {
		return app.HouseDocument{}, err
	}
	if s.VersionStatus != docapi.VersionIssued {
		return app.HouseDocument{}, fmt.Errorf("%w: DOC answers version %d %s", fin.ErrNoIssuedDocument, s.Version, s.VersionStatus)
	}
	return app.HouseDocument{DocumentRef: fin.DocumentRef{DocumentID: s.DocumentID, Version: s.Version, Number: s.Number, MasterNumber: s.MasterNumber},
		ShipmentID: s.ShipmentID, LegalEntityID: s.LegalEntityID}, nil
}
