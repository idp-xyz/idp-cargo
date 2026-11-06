// Package postgres persists COM orders, command receipts and Outbox events in the tenant's RLS scope.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	app "go.idp.xyz/idp-cargo/internal/contexts/com/internal/application"
	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
	"go.idp.xyz/idp-cargo/internal/kernel"
)

// OrderStore loads and writes orders for commands. Load locks the order row before reading, so commands
// on one order run one after another and see every obligation committed before them.
type OrderStore struct{ DB *bentopg.DB }

func (s OrderStore) Load(ctx context.Context, tenant com.TenantID, id com.OrderID) (*com.CustomerOrder, int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	var one int
	err = q.QueryRow(ctx, `SELECT 1 FROM cargo_com.customer_order WHERE tenant_id=$1 AND order_id=$2 FOR NO KEY UPDATE`,
		string(tenant), string(id)).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, bentorepo.ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	return readOrder(ctx, q, tenant, id)
}

// OrderReader reads orders for queries without locking.
type OrderReader struct{ DB *bentopg.DB }

// ReadObligation reads the order holding one version of an obligation. Obligation rows never change, so
// the order found by the first statement is the one that holds it.
func (r OrderReader) ReadObligation(ctx context.Context, tenant com.TenantID, id com.ObligationID, version int64) (*com.CustomerOrder, error) {
	q, err := r.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	var order string
	err = q.QueryRow(ctx, `SELECT order_id FROM cargo_com.fulfillment_obligation
      WHERE tenant_id=$1 AND obligation_id=$2 AND obligation_version=$3`, string(tenant), string(id), version).Scan(&order)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, bentorepo.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	o, _, err := readOrder(ctx, q, tenant, com.OrderID(order))
	return o, err
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// readOrder reads an order with its lines and obligations in one statement, so all three come from one
// snapshot even without a lock: under READ COMMITTED separate statements could see an allocation that
// committed in between.
func readOrder(ctx context.Context, q queryRower, tenant com.TenantID, id com.OrderID) (*com.CustomerOrder, int64, error) {
	s := com.OrderState{OrderDraft: com.OrderDraft{TenantID: tenant, OrderID: id}}
	var entity, currency, status string
	var roleUntil *time.Time
	var revision int64
	var lineNo, obLineNo []int32
	var lineText, lineUnit, obID, obUnit, obBy []string
	var lineQty, obQty, obVersion []int64
	var obAt []time.Time
	err := q.QueryRow(ctx, `SELECT o.legal_entity_id,o.customer_party_id,o.customer_party_revision,o.customer_legal_name,
        o.customer_country_code,o.customer_role_valid_from,o.customer_role_valid_until,o.customer_role_evidence_ref,
        o.quote_id,o.quote_revision,o.quote_customer_revision,o.quote_currency,o.quote_total_minor,o.quote_valid_from,
        o.quote_valid_until,o.quote_accepted_at,o.quote_acceptance_evidence_ref,o.accepted_terms_sha256,
        o.recorded_by,o.recorded_at,o.status,o.revision,
        ARRAY(SELECT line_no FROM cargo_com.order_line l WHERE l.tenant_id=o.tenant_id AND l.order_id=o.order_id ORDER BY line_no),
        ARRAY(SELECT description FROM cargo_com.order_line l WHERE l.tenant_id=o.tenant_id AND l.order_id=o.order_id ORDER BY line_no),
        ARRAY(SELECT unit_code FROM cargo_com.order_line l WHERE l.tenant_id=o.tenant_id AND l.order_id=o.order_id ORDER BY line_no),
        ARRAY(SELECT quantity FROM cargo_com.order_line l WHERE l.tenant_id=o.tenant_id AND l.order_id=o.order_id ORDER BY line_no),
        ARRAY(SELECT obligation_id FROM cargo_com.fulfillment_obligation f WHERE f.tenant_id=o.tenant_id AND f.order_id=o.order_id
              ORDER BY allocated_at,obligation_id,obligation_version),
        ARRAY(SELECT obligation_version FROM cargo_com.fulfillment_obligation f WHERE f.tenant_id=o.tenant_id AND f.order_id=o.order_id
              ORDER BY allocated_at,obligation_id,obligation_version),
        ARRAY(SELECT line_no FROM cargo_com.fulfillment_obligation f WHERE f.tenant_id=o.tenant_id AND f.order_id=o.order_id
              ORDER BY allocated_at,obligation_id,obligation_version),
        ARRAY(SELECT unit_code FROM cargo_com.fulfillment_obligation f WHERE f.tenant_id=o.tenant_id AND f.order_id=o.order_id
              ORDER BY allocated_at,obligation_id,obligation_version),
        ARRAY(SELECT quantity FROM cargo_com.fulfillment_obligation f WHERE f.tenant_id=o.tenant_id AND f.order_id=o.order_id
              ORDER BY allocated_at,obligation_id,obligation_version),
        ARRAY(SELECT allocated_by FROM cargo_com.fulfillment_obligation f WHERE f.tenant_id=o.tenant_id AND f.order_id=o.order_id
              ORDER BY allocated_at,obligation_id,obligation_version),
        ARRAY(SELECT allocated_at FROM cargo_com.fulfillment_obligation f WHERE f.tenant_id=o.tenant_id AND f.order_id=o.order_id
              ORDER BY allocated_at,obligation_id,obligation_version)
      FROM cargo_com.customer_order o WHERE o.tenant_id=$1 AND o.order_id=$2`, string(tenant), string(id)).Scan(
		&entity, &s.Customer.PartyID, &s.Customer.PartyRevision, &s.Customer.LegalName,
		&s.Customer.CountryCode, &s.Customer.RoleValidity.From, &roleUntil, &s.Customer.RoleEvidenceRef,
		&s.Quote.QuoteID, &s.Quote.QuoteRevision, &s.Quote.CustomerRevision, &currency, &s.Quote.Total.Minor, &s.Quote.Validity.From,
		&s.Quote.Validity.Until, &s.Quote.AcceptedAt, &s.Quote.EvidenceRef, &s.Quote.TermsHash,
		&s.RecordedBy, &s.RecordedAt, &status, &revision,
		&lineNo, &lineText, &lineUnit, &lineQty, &obID, &obVersion, &obLineNo, &obUnit, &obQty, &obBy, &obAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, bentorepo.ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	s.Quote.LegalEntityID, s.Quote.CustomerPartyID, s.Status = com.LegalEntityID(entity), s.Customer.PartyID, com.OrderStatus(status)
	s.Quote.Total.Currency = kernel.CurrencyCode(currency)
	if roleUntil != nil {
		s.Customer.RoleValidity.Until = *roleUntil
	}
	for i := range lineNo {
		s.Lines = append(s.Lines, com.OrderLine{LineNo: int(lineNo[i]), Description: lineText[i],
			Quantity: kernel.Quantity{Unit: kernel.UnitCode(lineUnit[i]), Amount: lineQty[i]}})
	}
	for i := range obID {
		s.Obligations = append(s.Obligations, com.Obligation{ObligationID: com.ObligationID(obID[i]), Version: obVersion[i],
			LineNo: int(obLineNo[i]), Quantity: kernel.Quantity{Unit: kernel.UnitCode(obUnit[i]), Amount: obQty[i]},
			AllocatedBy: obBy[i], AllocatedAt: obAt[i]})
	}
	order, err := com.RestoreOrder(s)
	return order, revision, err
}

// Insert writes a new order at revision 1 with its lines.
func (s OrderStore) Insert(ctx context.Context, order *com.CustomerOrder) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := order.State()
	c, qt := st.Customer, st.Quote
	var roleUntil any
	if !c.RoleValidity.Until.IsZero() {
		roleUntil = c.RoleValidity.Until
	}
	var revision int64
	err = q.QueryRow(ctx, `INSERT INTO cargo_com.customer_order (tenant_id,order_id,legal_entity_id,customer_party_id,
        customer_party_revision,customer_legal_name,customer_country_code,customer_role_valid_from,customer_role_valid_until,
        customer_role_evidence_ref,quote_id,quote_revision,quote_customer_revision,quote_currency,quote_total_minor,
        quote_valid_from,quote_valid_until,quote_accepted_at,quote_acceptance_evidence_ref,accepted_terms_sha256,
        recorded_by,recorded_at,status)
      VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23) RETURNING revision`,
		string(st.TenantID), string(st.OrderID), string(qt.LegalEntityID), c.PartyID,
		c.PartyRevision, c.LegalName, c.CountryCode, c.RoleValidity.From, roleUntil,
		c.RoleEvidenceRef, qt.QuoteID, qt.QuoteRevision, qt.CustomerRevision, string(qt.Total.Currency), qt.Total.Minor,
		qt.Validity.From, qt.Validity.Until, qt.AcceptedAt, qt.EvidenceRef, qt.TermsHash,
		st.RecordedBy, st.RecordedAt, string(st.Status)).Scan(&revision)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case "customer_order_pkey":
			return 0, bentorepo.ErrAlreadyExists
		case "customer_order_one_per_quote":
			return 0, fmt.Errorf("%w: quote %s", app.ErrQuoteAlreadyOrdered, qt.QuoteID)
		}
	}
	if err != nil {
		return 0, err
	}
	for _, l := range st.Lines {
		if _, err = q.Exec(ctx, `INSERT INTO cargo_com.order_line (tenant_id,order_id,line_no,description,unit_code,quantity)
          VALUES ($1,$2,$3,$4,$5,$6)`, string(st.TenantID), string(st.OrderID), l.LineNo, l.Description, string(l.Quantity.Unit), l.Quantity.Amount); err != nil {
			return 0, err
		}
	}
	return revision, nil
}

// Save writes the order's status and the obligations it added, if the stored revision is still expected.
func (s OrderStore) Save(ctx context.Context, order *com.CustomerOrder, expected int64) (int64, error) {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return 0, err
	}
	st := order.State()
	var revision int64
	err = q.QueryRow(ctx, `UPDATE cargo_com.customer_order SET status=$3,revision=revision+1,updated_at=now()
      WHERE tenant_id=$1 AND order_id=$2 AND revision=$4 RETURNING revision`,
		string(st.TenantID), string(st.OrderID), string(st.Status), expected).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, bentorepo.ErrConflict
	}
	if err != nil {
		return 0, err
	}
	for _, ob := range order.Added() {
		_, err = q.Exec(ctx, `INSERT INTO cargo_com.fulfillment_obligation (tenant_id,obligation_id,obligation_version,order_id,
            line_no,unit_code,quantity,allocated_by,allocated_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			string(st.TenantID), string(ob.ObligationID), ob.Version, string(st.OrderID),
			ob.LineNo, string(ob.Quantity.Unit), ob.Quantity.Amount, ob.AllocatedBy, ob.AllocatedAt)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "fulfillment_obligation_pkey" {
			return 0, fmt.Errorf("%w: obligation %s", bentorepo.ErrAlreadyExists, ob.ObligationID)
		}
		if err != nil {
			return 0, err
		}
	}
	return revision, nil
}

// CommandReceipts stores the result of every committed COM command for replays.
type CommandReceipts struct{ DB *bentopg.DB }

func (s CommandReceipts) Find(ctx context.Context, tenant com.TenantID, id string) (app.Receipt, bool, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return app.Receipt{}, false, err
	}
	r := app.Receipt{CommandID: id}
	var order string
	err = q.QueryRow(ctx, `SELECT request_sha256,result_id,result_revision FROM cargo_com.command_receipt
      WHERE tenant_id=$1 AND command_id=$2`, string(tenant), id).Scan(&r.RequestHash, &order, &r.NewRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Receipt{}, false, nil
	}
	if err != nil {
		return app.Receipt{}, false, err
	}
	r.OrderID = com.OrderID(order)
	return r, true, nil
}

func (s CommandReceipts) Save(ctx context.Context, tenant com.TenantID, r app.Receipt) error {
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_com.command_receipt (tenant_id,command_id,request_sha256,result_type,result_id,result_revision)
      VALUES ($1,$2,$3,'CustomerOrder',$4,$5)`, string(tenant), r.CommandID, r.RequestHash, string(r.OrderID), r.NewRevision)
	return err
}
