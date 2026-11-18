package application

import (
	"context"
	"fmt"

	doc "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/domain"
)

// CarrierStore loads and writes the carrier's documents; Load locks the
// document row.
type CarrierStore interface {
	Load(context.Context, doc.TenantID, string) (*doc.CarrierDocument, int64, error)
	Read(context.Context, doc.TenantID, string) (*doc.CarrierDocument, int64, error)
	// ReadLive reads the movement's document that is not cancelled; it is not found when there is none.
	ReadLive(ctx context.Context, tenant doc.TenantID, movement string) (*doc.CarrierDocument, int64, error)
	Insert(context.Context, *doc.CarrierDocument) (int64, error)
	Save(context.Context, *doc.CarrierDocument, int64) (int64, error)
	// NumberMovement returns the movement of the carrier's document that is not cancelled and has the number, if there is one.
	NumberMovement(ctx context.Context, tenant doc.TenantID, carrier, number string) (movement string, found bool, err error)
}

// CarrierInput is what the person recording a carrier document enters: the movement whose latest OceanDocFacts name
// the carrier's bill, and the rest.
type CarrierInput struct {
	MovementID string
	Entered    doc.CarrierEntered
}

// RecordCarrier records the carrier's master bill from SEA's latest facts of the movement, read before DOC's
// transaction (T3). In the transaction it looks for a document of the carrier that is not cancelled and has the number:
// on another movement, the number is refused. The database's unique indexes answer a movement's second document that
// is not cancelled with a conflict, and two commands racing for one number of two movements with DOC_NUMBER_ISSUED.
func (d Deps) RecordCarrier(ctx context.Context, c Command, in CarrierInput) (Receipt, error) {
	if d.Carriers == nil || d.Ocean == nil {
		return Receipt{}, ErrMissingDependency
	}
	if in.MovementID == "" {
		return Receipt{}, fmt.Errorf("%w: a carrier document names the movement whose facts show the carrier's bill", ErrInvalidCommand)
	}
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	facts, err := d.Ocean.Latest(ctx, c.Scope.TenantID, in.MovementID)
	if err != nil {
		return Receipt{}, err
	}
	return d.write(ctx, c, hash, "CarrierDocument", func(txCtx context.Context) (Receipt, aggregate, error) {
		x, err := doc.RecordCarrierDocument(c.Scope.TenantID, doc.DocumentID(c.Subject), facts, in.Entered, c.Scope.ActorID, c.OccurredAt)
		if err != nil {
			return Receipt{}, nil, err
		}
		st := x.State()
		movement, held, err := d.Carriers.NumberMovement(txCtx, st.TenantID, st.CarrierPartyID, st.Number)
		if err != nil {
			return Receipt{}, nil, err
		}
		if held && movement != st.MovementID {
			return Receipt{}, nil, fmt.Errorf("%w: %s %s of carrier %s is on the document of movement %s", doc.ErrNumberIssued, st.Kind, st.Number, st.CarrierPartyID, movement)
		}
		x.Stamp(entered)
		revision, err := d.Carriers.Insert(txCtx, x)
		if err != nil {
			return Receipt{}, nil, err
		}
		return Receipt{SubjectID: c.Subject, NewRevision: revision}, x, nil
	})
}

// changeCarrier runs a command on a stored carrier document at the expected revision.
func (d Deps) changeCarrier(ctx context.Context, c Command, apply func(*doc.CarrierDocument) error) (Receipt, error) {
	if d.Carriers == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	return d.write(ctx, c, hash, "CarrierDocument", func(txCtx context.Context) (Receipt, aggregate, error) {
		x, revision, err := d.Carriers.Load(txCtx, c.Scope.TenantID, c.Subject)
		if err != nil {
			return Receipt{}, nil, err
		}
		if revision != c.ExpectedRevision {
			return Receipt{}, nil, doc.ErrConflict
		}
		if err = apply(x); err != nil {
			return Receipt{}, nil, err
		}
		x.Stamp(entered)
		if revision, err = d.Carriers.Save(txCtx, x, revision); err != nil {
			return Receipt{}, nil, err
		}
		return Receipt{SubjectID: c.Subject, NewRevision: revision}, x, nil
	})
}

// VerifyCarrier records the verification of a carrier document by a person other than the one who recorded it.
func (d Deps) VerifyCarrier(ctx context.Context, c Command) (Receipt, error) {
	return d.changeCarrier(ctx, c, func(x *doc.CarrierDocument) error { return x.Verify(c.Scope.ActorID, c.OccurredAt) })
}

// CancelCarrier cancels a carrier document.
func (d Deps) CancelCarrier(ctx context.Context, c Command, reason string) (Receipt, error) {
	return d.changeCarrier(ctx, c, func(x *doc.CarrierDocument) error { return x.Cancel(reason, c.Scope.ActorID, c.OccurredAt) })
}

// CarrierDocument reads a carrier document.
func (r Reads) CarrierDocument(ctx context.Context, store CarrierStore, tenant doc.TenantID, id string) (*doc.CarrierDocument, int64, error) {
	if tenant == "" || id == "" || store == nil {
		return nil, 0, ErrInvalidQuery
	}
	var x *doc.CarrierDocument
	var revision int64
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) (err error) {
		x, revision, err = store.Read(txCtx, tenant, id)
		return err
	})
	return x, revision, err
}

// CarrierDocumentOf reads the carrier document of a movement that is not cancelled.
func (r Reads) CarrierDocumentOf(ctx context.Context, store CarrierStore, tenant doc.TenantID, movement string) (*doc.CarrierDocument, int64, error) {
	if tenant == "" || movement == "" || store == nil {
		return nil, 0, ErrInvalidQuery
	}
	var x *doc.CarrierDocument
	var revision int64
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) (err error) {
		x, revision, err = store.ReadLive(txCtx, tenant, movement)
		return err
	})
	return x, revision, err
}
