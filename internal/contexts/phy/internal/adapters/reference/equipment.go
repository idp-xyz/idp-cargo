package reference

import (
	"context"
	"errors"
	"fmt"

	app "go.idp.xyz/idp-cargo/internal/contexts/phy/internal/application"
	phy "go.idp.xyz/idp-cargo/internal/contexts/phy/internal/domain"
	refapi "go.idp.xyz/idp-cargo/internal/reference/api"
)

// Equipment resolves container size-types through internal/reference/api (PB8). Call it outside PHY's transactions.
type Equipment struct{ Ref refapi.Equipment }

var _ app.Equipment = Equipment{}

func (e Equipment) ContainerType(ctx context.Context, code string) (phy.UnitLimits, error) {
	t, err := e.Ref.ResolveContainerType(ctx, code)
	if errors.Is(err, refapi.ErrUnknownEquipment) {
		return phy.UnitLimits{}, fmt.Errorf("%w: %v", app.ErrEquipmentRefused, err.Error())
	}
	if err != nil {
		return phy.UnitLimits{}, err
	}
	return phy.UnitLimits{TypeCode: t.Code, TEUTenths: t.TEUTenths, MaxGrossKg: t.MaxGrossKg, TareKg: t.TareKg, CapacityCM3: t.CapacityCM3,
		Edition: t.Edition}, nil
}
