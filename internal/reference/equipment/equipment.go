// Package equipment resolves equipment codes against the current edition of Cargo's equipment reference data in
// cargo_ref (backlog PB8, the F2 pattern; MD-26). It is read-only: editions are loaded by migrations or the
// reference importer, never by contexts.
package equipment

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-cargo/internal/reference/api"
)

var sizeType = regexp.MustCompile(`^[0-9A-Z]{2}[A-Z][0-9A-Z]$`)

// Directory reads cargo_ref on a transaction owner of its own, outside any context's transaction.
type Directory struct{ DB *bentopg.DB }

var _ api.Equipment = Directory{}

// New returns a Directory on db.
func New(db *bentopg.DB) (Directory, error) {
	if db == nil {
		return Directory{}, errors.New("reference: equipment needs a database")
	}
	return Directory{DB: db}, nil
}

// ResolveContainerType answers an ISO 6346 size-type of the current edition.
func (d Directory) ResolveContainerType(ctx context.Context, code string) (api.ContainerType, error) {
	if !sizeType.MatchString(code) {
		return api.ContainerType{}, fmt.Errorf("%w: %q is not an ISO 6346 size-type", api.ErrUnknownEquipment, code)
	}
	var out api.ContainerType
	err := d.DB.Transactor().WithinTransaction(ctx, func(txCtx context.Context) error {
		r, err := d.DB.ReadExecutor(txCtx)
		if err != nil {
			return err
		}
		var teu float64
		err = r.QueryRow(txCtx, `SELECT t.code,t.description,t.teu::float8,t.max_gross_kg,t.tare_kg,t.capacity_cm3,t.edition
            FROM cargo_ref.container_size_type t JOIN cargo_ref.equipment_edition e ON e.edition = t.edition AND e.is_current
            WHERE t.code = $1`, code).Scan(&out.Code, &out.Description, &teu, &out.MaxGrossKg, &out.TareKg, &out.CapacityCM3, &out.Edition)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: size-type %s", api.ErrUnknownEquipment, code)
		}
		out.TEUTenths = int(teu*10 + 0.5)
		return err
	})
	return out, err
}
