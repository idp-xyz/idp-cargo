package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	org "go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
)

// identifierColumns selects a party's active identifiers as one JSON array, so a query can read them in the
// same statement, and so the same snapshot, as the party row.
const identifierColumns = `(SELECT coalesce(json_agg(json_build_object('type',i.identifier_type,'country',i.issuing_country,
    'value',coalesce(i.value,''),'none',i.none,'evidence',i.evidence_ref,'by',i.recorded_by,'at',i.recorded_at)
    ORDER BY i.identifier_type,i.issuing_country),'[]')::text
  FROM cargo_org.party_identifier i WHERE i.tenant_id=p.tenant_id AND i.party_id=p.party_id AND i.retired_at IS NULL)`

type storedIdentifier struct {
	Type     string    `json:"type"`
	Country  string    `json:"country"`
	Value    string    `json:"value"`
	None     bool      `json:"none"`
	Evidence string    `json:"evidence"`
	By       string    `json:"by"`
	At       time.Time `json:"at"`
}

func decodeIdentifiers(blob string) ([]org.PartyIdentifier, error) {
	var stored []storedIdentifier
	if err := json.Unmarshal([]byte(blob), &stored); err != nil {
		return nil, err
	}
	out := make([]org.PartyIdentifier, len(stored))
	for i, s := range stored {
		out[i] = org.PartyIdentifier{Type: org.IdentifierType(s.Type), Country: s.Country, Value: s.Value, None: s.None,
			EvidenceRef: s.Evidence, RecordedBy: s.By, RecordedAt: s.At}
	}
	return out, nil
}

// activeIdentifiers reads a party's active identifiers; Load calls it after locking the party row.
func activeIdentifiers(ctx context.Context, q bentopg.Querier, tenant org.TenantID, party org.PartyID) ([]org.PartyIdentifier, error) {
	var blob string
	err := q.QueryRow(ctx, `SELECT `+identifierColumns+` FROM cargo_org.business_party p WHERE p.tenant_id=$1 AND p.party_id=$2`,
		string(tenant), string(party)).Scan(&blob)
	if err != nil {
		return nil, err
	}
	return decodeIdentifiers(blob)
}

// saveIdentifiers writes the retirements and then the identifiers recorded since the party was loaded, at the
// party revision that records them. Identifier rows have stable ids, so writing a party twice adds nothing.
// A value another party holds violates party_identifier_value_active and is reported as ErrIdentifierInUse.
func saveIdentifiers(ctx context.Context, q bentopg.Executor, p *org.BusinessParty, revision int64) error {
	added, retired := p.IdentityChanges()
	for _, r := range retired {
		if _, err := q.Exec(ctx, `UPDATE cargo_org.party_identifier SET retired_by=$5,retired_at=$6,retire_reason=$7,retired_revision=$8
          WHERE tenant_id=$1 AND party_id=$2 AND identifier_type=$3 AND issuing_country=$4 AND retired_at IS NULL`,
			string(p.Tenant()), string(p.ID()), string(r.Type), r.Country, r.RetiredBy, r.RetiredAt.UTC().Truncate(time.Microsecond), r.Reason, revision); err != nil {
			return err
		}
	}
	for _, id := range added {
		at := id.RecordedAt.UTC().Truncate(time.Microsecond)
		digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%t\x00%s\x00%s", id.Type, id.Country, id.Value, id.None, id.RecordedBy, at.Format(time.RFC3339Nano))))
		var value any
		if !id.None {
			value = id.Value
		}
		_, err := q.Exec(ctx, `INSERT INTO cargo_org.party_identifier
          (tenant_id,party_id,identifier_id,identifier_type,issuing_country,value,none,evidence_ref,recorded_by,recorded_at,recorded_revision)
          VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT (tenant_id,party_id,identifier_id) DO NOTHING`,
			string(p.Tenant()), string(p.ID()), hex.EncodeToString(digest[:]), string(id.Type), id.Country, value, id.None, id.EvidenceRef, id.RecordedBy, at, revision)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "party_identifier_value_active" {
			return fmt.Errorf("%w: %s", org.ErrIdentifierInUse, id.Key())
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// IdentifierHolders finds the party that holds an active identifier value.
type IdentifierHolders struct{ DB *bentopg.DB }

func (h IdentifierHolders) Holder(ctx context.Context, tenant org.TenantID, t org.IdentifierType, country, value string) (org.PartyID, bool, error) {
	q, err := h.DB.ReadExecutor(ctx)
	if err != nil {
		return "", false, err
	}
	var party string
	err = q.QueryRow(ctx, `SELECT party_id FROM cargo_org.party_identifier
      WHERE tenant_id=$1 AND identifier_type=$2 AND issuing_country=$3 AND value=$4 AND retired_at IS NULL`,
		string(tenant), string(t), country, value).Scan(&party)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return org.PartyID(party), true, nil
}
