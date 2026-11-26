// Package sea is DOC's anticorruption layer for SEA's OceanDocFacts v1. SEA's error kinds are kept as text only.
package sea

import (
	"context"
	"errors"
	"fmt"

	app "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/application"
	doc "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/domain"
	seaapi "go.idp.xyz/idp-cargo/internal/contexts/sea/api"
)

// OceanFacts is the read of OceanDocFacts v1 DOC uses.
type OceanFacts interface {
	GetOceanDocFacts(ctx context.Context, q seaapi.GetOceanDocFacts) (seaapi.OceanDocFacts, error)
}

// HouseBills is SEA's lookup of a house bill number (since V2.105). A reader without it leaves every house bill on
// the movement it was drafted on, as before.
type HouseBills interface {
	FindHouseBill(ctx context.Context, q seaapi.FindHouseBill) (seaapi.HouseBillRef, error)
}

// Carrying asks SEA which movement a house bill number lives in now.
func (f Facts) Carrying(ctx context.Context, tenant doc.TenantID, legalEntity, hbl string) (string, string, bool, error) {
	finder, ok := f.SEA.(HouseBills)
	if !ok {
		return "", "", false, nil
	}
	ref, err := finder.FindHouseBill(ctx, seaapi.FindHouseBill{TenantID: string(tenant), LegalEntityID: legalEntity, HBL: hbl})
	if errors.Is(err, seaapi.ErrNotFound) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return ref.MovementID, ref.ShipmentID, ref.MovementID != "" && ref.HBL == hbl, nil
}

// Facts reads OceanDocFacts v1. Call it outside DOC's transactions.
type Facts struct{ SEA OceanFacts }

var _ app.Ocean = Facts{}

func (f Facts) Latest(ctx context.Context, tenant doc.TenantID, movement string) (doc.OceanFacts, error) {
	got, err := f.SEA.GetOceanDocFacts(ctx, seaapi.GetOceanDocFacts{TenantID: string(tenant), MovementID: movement})
	if errors.Is(err, seaapi.ErrNotFound) {
		return doc.OceanFacts{}, fmt.Errorf("%w: OceanDocFacts of %s: %v", app.ErrUnknownSource, movement, err.Error())
	}
	if err != nil {
		return doc.OceanFacts{}, err
	}
	if got.MovementID != movement {
		return doc.OceanFacts{}, fmt.Errorf("%w: SEA answered for another movement", app.ErrUnknownSource)
	}
	out := doc.OceanFacts{MovementID: movement, Version: got.Version, SHA256: got.SHA256, Final: got.Status == seaapi.DocFactsFinal,
		ConsolidationID: got.ConsolidationID, LegalEntityID: got.OperatingLegalEntityID, CarrierPartyID: got.CarrierPartyID,
		MasterBill: got.MasterBillNumber, CarrierPartyRevision: got.CarrierPartyRevision, CarrierRole: got.CarrierRole,
		MasterBillKind: got.MasterBillKind, Vessel: got.Vessel, Voyage: got.Voyage, PlaceOfReceipt: got.PlaceOfReceipt,
		PortOfLoading: got.PortOfLoading, PortOfDischarge: got.PortOfDischarge, Edition: got.LocationEdition, OnBoardAt: got.OnBoardAt,
		ContainerDates: got.ContainerDates}
	for _, c := range got.Containers {
		out.Containers = append(out.Containers, doc.OceanContainer{VisitID: c.VisitID, Number: c.Number, SizeType: c.SizeType, SealNumber: c.SealNumber,
			OnBoardAt: c.OnBoardAt})
	}
	for _, h := range got.Houses {
		house := doc.OceanHouse{ShipmentID: h.ShipmentID, HBL: h.HBL, Packs: h.Packs, Package: h.PackageType, GrossGrams: h.GrossWeightGrams,
			VolumeCm3: h.VolumeCubicCentimetres, Goods: h.GoodsDescription, OrderIDs: append([]string(nil), h.OrderIDs...)}
		for _, c := range h.Containers {
			house.Containers = append(house.Containers, doc.OceanHouseContainer{VisitID: c.VisitID, Number: c.ContainerNumber, Packs: c.Packs,
				GrossGrams: c.GrossWeightGrams, VolumeCm3: c.VolumeCubicCentimetres})
		}
		out.Houses = append(out.Houses, house)
	}
	return out, nil
}
