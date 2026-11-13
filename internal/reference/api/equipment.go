package api

import (
	"context"
	"errors"
)

// ContainerType is an ISO 6346 size-type of the current equipment edition (backlog PB8): its TEU, and the default
// maximum gross, tare and internal capacity a planned load unit freezes. A container's own tare and maximum gross
// are on its CSC plate; these are defaults.
type ContainerType struct {
	Code        string
	Description string
	TEUTenths   int   // TEU in tenths (10 for a 20ft unit, 20 for a 40ft one)
	MaxGrossKg  int64 // kilograms
	TareKg      int64 // kilograms
	CapacityCM3 int64 // cubic centimetres
	Edition     string
}

// Equipment resolves equipment codes against the current edition of the equipment reference data.
type Equipment interface {
	ResolveContainerType(ctx context.Context, code string) (ContainerType, error)
}

// ErrUnknownEquipment is a code the current equipment edition does not know.
var ErrUnknownEquipment = errors.New("reference: no such equipment type in the current edition")
