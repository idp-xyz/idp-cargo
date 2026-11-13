package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	com "go.idp.xyz/idp-cargo/internal/contexts/com/internal/domain"
)

// The transport and party columns readOrder adds, in one statement with the order.
const partyColumns = `,
        (SELECT coalesce(json_agg(v ORDER BY v.version), '[]') FROM cargo_com.order_transport v
          WHERE v.tenant_id=o.tenant_id AND v.order_id=o.order_id),
        (SELECT coalesce(json_agg(p ORDER BY p.role, p.version), '[]') FROM cargo_com.order_party p
          WHERE p.tenant_id=o.tenant_id AND p.order_id=o.order_id),
        (SELECT coalesce(json_agg(k ORDER BY k.lock_no), '[]') FROM cargo_com.order_party_lock k
          WHERE k.tenant_id=o.tenant_id AND k.order_id=o.order_id)`

type lockRow struct {
	LockNo           int       `json:"lock_no"`
	HolderID         string    `json:"holder_id"`
	LockedBy         string    `json:"locked_by"`
	LockedAt         time.Time `json:"locked_at"`
	EnteredAt        time.Time `json:"entered_at"`
	ReleasedBy       *string   `json:"released_by"`
	ReleasedAt       time.Time `json:"released_at"`
	ReleaseEnteredAt time.Time `json:"release_entered_at"`
}

// PartiesLocked reports whether a commit failed because a party or transport version was written while a
// transport document held the order's parties locked (C12).
func PartiesLocked(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == "order_parties_locked"
}

func restoreLocks(s *com.OrderState, lockJSON []byte) error {
	var locks []lockRow
	if err := json.Unmarshal(lockJSON, &locks); err != nil {
		return err
	}
	for _, l := range locks {
		s.PartyLocks = append(s.PartyLocks, com.PartyLock{No: l.LockNo, HolderID: l.HolderID, LockedBy: l.LockedBy, LockedAt: utc(l.LockedAt),
			EnteredAt: utc(l.EnteredAt), ReleasedBy: deref(l.ReleasedBy), ReleasedAt: utc(l.ReleasedAt), ReleaseEnteredAt: utc(l.ReleaseEnteredAt)})
	}
	return nil
}

func saveLocks(ctx context.Context, q bentopg.Executor, order *com.CustomerOrder) error {
	tenant, id := string(order.Tenant()), string(order.ID())
	for _, l := range order.AddedPartyLocks() {
		var by, at, entered any
		if !l.Held() {
			by, at, entered = l.ReleasedBy, l.ReleasedAt, l.ReleaseEnteredAt
		}
		if _, err := q.Exec(ctx, `INSERT INTO cargo_com.order_party_lock (tenant_id,order_id,lock_no,holder_id,locked_by,locked_at,entered_at,
            released_by,released_at,release_entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, tenant, id, l.No, l.HolderID, l.LockedBy, l.LockedAt,
			l.EnteredAt, by, at, entered); err != nil {
			return err
		}
	}
	for _, l := range order.ReleasedPartyLocks() {
		tag, err := q.Exec(ctx, `UPDATE cargo_com.order_party_lock SET released_by=$4,released_at=$5,release_entered_at=$6
          WHERE tenant_id=$1 AND order_id=$2 AND lock_no=$3 AND released_at IS NULL`, tenant, id, l.No, l.ReleasedBy, l.ReleasedAt, l.ReleaseEnteredAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: party lock %d of order %s", bentorepo.ErrConflict, l.No, id)
		}
	}
	return nil
}

type transportRow struct {
	Version   int64     `json:"version"`
	Mode      *string   `json:"transport_mode"`
	Movement  *string   `json:"movement_type"`
	Container *string   `json:"container_mode"`
	Reason    *string   `json:"reason"`
	ChangedBy string    `json:"changed_by"`
	ChangedAt time.Time `json:"changed_at"`
	EnteredAt time.Time `json:"entered_at"`
}

type partyRow struct {
	Role            string          `json:"role"`
	Version         int64           `json:"version"`
	Kind            string          `json:"kind"`
	ToOrderText     *string         `json:"to_order_text"`
	PartyID         *string         `json:"party_id"`
	PartyRevision   *int64          `json:"party_revision"`
	LegalName       *string         `json:"legal_name"`
	EnglishName     *string         `json:"english_name"`
	CountryCode     *string         `json:"country_code"`
	Identifiers     []identifierDoc `json:"identifiers"`
	RoleCode        *string         `json:"role_code"`
	RoleValidFrom   *time.Time      `json:"role_valid_from"`
	RoleValidUntil  *time.Time      `json:"role_valid_until"`
	RoleEvidenceRef *string         `json:"role_evidence_ref"`
	Address         *addressDoc     `json:"address"`
	Pickup          *addressDoc     `json:"pickup"`
	Delivery        *addressDoc     `json:"delivery"`
	HasContact      bool            `json:"has_contact"`
	Reason          *string         `json:"reason"`
	SetBy           string          `json:"set_by"`
	SetAt           time.Time       `json:"set_at"`
	EnteredAt       time.Time       `json:"entered_at"`
}

type identifierDoc struct {
	Type    string `json:"type"`
	Country string `json:"country"`
	Value   string `json:"value,omitempty"`
	None    bool   `json:"none,omitempty"`
}

// addressDoc is the stored form of a frozen address.
type addressDoc struct {
	Source         string    `json:"source"`
	ORGAddressID   string    `json:"org_address_id,omitempty"`
	ShortCode      string    `json:"short_code,omitempty"`
	ORGVersion     int64     `json:"org_version,omitempty"`
	Name           string    `json:"name"`
	Lines          []string  `json:"lines"`
	City           string    `json:"city"`
	Region         string    `json:"region,omitempty"`
	Postcode       string    `json:"postcode,omitempty"`
	CountryCode    string    `json:"country_code"`
	UNLocode       string    `json:"unlocode,omitempty"`
	Residential    bool      `json:"residential,omitempty"`
	Phone          string    `json:"phone,omitempty"`
	Email          string    `json:"email,omitempty"`
	Local          *localDoc `json:"local,omitempty"`
	OverriddenBy   string    `json:"overridden_by,omitempty"`
	OverrideReason string    `json:"override_reason,omitempty"`
}

type localDoc struct {
	Language string   `json:"language"`
	Name     string   `json:"name"`
	Lines    []string `json:"lines"`
	City     string   `json:"city"`
	Region   string   `json:"region,omitempty"`
}

func toDoc(a *com.AddressSnapshot) *addressDoc {
	if a == nil {
		return nil
	}
	d := &addressDoc{Source: string(a.Source), ORGAddressID: a.ORGAddressID, ShortCode: a.ShortCode, ORGVersion: a.ORGVersion, Name: a.Name,
		Lines: a.Lines, City: a.City, Region: a.Region, Postcode: a.Postcode, CountryCode: a.CountryCode, UNLocode: a.UNLocode,
		Residential: a.Residential, Phone: a.Phone, Email: a.Email, OverriddenBy: a.OverriddenBy, OverrideReason: a.OverrideReason}
	if l := a.Local; l != nil {
		d.Local = &localDoc{Language: l.Language, Name: l.Name, Lines: l.Lines, City: l.City, Region: l.Region}
	}
	return d
}

func fromDoc(d *addressDoc) *com.AddressSnapshot {
	if d == nil {
		return nil
	}
	a := &com.AddressSnapshot{Source: com.AddressSource(d.Source), ORGAddressID: d.ORGAddressID, ShortCode: d.ShortCode, ORGVersion: d.ORGVersion,
		Name: d.Name, Lines: d.Lines, City: d.City, Region: d.Region, Postcode: d.Postcode, CountryCode: d.CountryCode, UNLocode: d.UNLocode,
		Residential: d.Residential, Phone: d.Phone, Email: d.Email, OverriddenBy: d.OverriddenBy, OverrideReason: d.OverrideReason}
	if l := d.Local; l != nil {
		a.Local = &com.LocalAddress{Language: l.Language, Name: l.Name, Lines: l.Lines, City: l.City, Region: l.Region}
	}
	return a
}

// restoreParties adds the stored transport and party versions to an order state being read.
func restoreParties(s *com.OrderState, transportJSON, partyJSON []byte) error {
	var transports []transportRow
	var parties []partyRow
	if err := json.Unmarshal(transportJSON, &transports); err != nil {
		return err
	}
	if err := json.Unmarshal(partyJSON, &parties); err != nil {
		return err
	}
	for _, v := range transports {
		s.TransportVersions = append(s.TransportVersions, com.TransportVersion{Version: v.Version,
			Transport: com.Transport{Mode: com.TransportMode(deref(v.Mode)), Movement: com.MovementType(deref(v.Movement)),
				ContainerMode: com.ContainerMode(deref(v.Container))},
			Reason: deref(v.Reason), ChangedBy: v.ChangedBy, ChangedAt: utc(v.ChangedAt), EnteredAt: utc(v.EnteredAt)})
	}
	for _, p := range parties {
		party := com.OrderParty{Role: p.Role, Version: p.Version, Kind: com.PartyKind(p.Kind), ToOrderText: deref(p.ToOrderText),
			Address: fromDoc(p.Address), Pickup: fromDoc(p.Pickup), Delivery: fromDoc(p.Delivery), HasContact: p.HasContact,
			Reason: deref(p.Reason), SetBy: p.SetBy, SetAt: utc(p.SetAt), EnteredAt: utc(p.EnteredAt)}
		if p.PartyID != nil {
			r := com.PartyRecord{PartyID: *p.PartyID, LegalName: deref(p.LegalName), EnglishName: deref(p.EnglishName),
				CountryCode: deref(p.CountryCode), RoleCode: deref(p.RoleCode), RoleEvidenceRef: deref(p.RoleEvidenceRef)}
			if p.PartyRevision != nil {
				r.PartyRevision = *p.PartyRevision
			}
			if p.RoleValidFrom != nil {
				r.RoleValidity.From = p.RoleValidFrom.UTC()
			}
			if p.RoleValidUntil != nil {
				r.RoleValidity.Until = p.RoleValidUntil.UTC()
			}
			for _, i := range p.Identifiers {
				r.Identifiers = append(r.Identifiers, com.PartyIdentifier{Type: i.Type, Country: i.Country, Value: i.Value, None: i.None})
			}
			party.Party = r
		}
		s.Parties = append(s.Parties, party)
	}
	return nil
}

// saveParties writes the transport and party versions an order added, with the contacts of new versions.
func saveParties(ctx context.Context, q bentopg.Executor, order *com.CustomerOrder) error {
	tenant, id := string(order.Tenant()), string(order.ID())
	for _, v := range order.AddedTransport() {
		if _, err := q.Exec(ctx, `INSERT INTO cargo_com.order_transport (tenant_id,order_id,version,transport_mode,movement_type,container_mode,
            reason,changed_by,changed_at,entered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, tenant, id, v.Version, orNull(string(v.Mode)),
			orNull(string(v.Movement)), orNull(string(v.ContainerMode)), orNull(v.Reason), v.ChangedBy, v.ChangedAt, v.EnteredAt); err != nil {
			return err
		}
	}
	for _, p := range order.AddedParties() {
		var partyID, revision, legalName, english, country, roleCode, from, until, evidence any
		identifiers := []identifierDoc{}
		if p.Kind == com.KindParty {
			r := p.Party
			partyID, revision, legalName, english, country = r.PartyID, r.PartyRevision, r.LegalName, orNull(r.EnglishName), r.CountryCode
			if r.RoleCode != "" {
				roleCode, from, evidence = r.RoleCode, r.RoleValidity.From, r.RoleEvidenceRef
				if !r.RoleValidity.Until.IsZero() {
					until = r.RoleValidity.Until
				}
			}
			for _, i := range r.Identifiers {
				identifiers = append(identifiers, identifierDoc{Type: i.Type, Country: i.Country, Value: i.Value, None: i.None})
			}
		}
		ids, err := json.Marshal(identifiers)
		if err != nil {
			return err
		}
		address, pickup, delivery, err := jsonDocs(p.Address, p.Pickup, p.Delivery)
		if err != nil {
			return err
		}
		if _, err = q.Exec(ctx, `INSERT INTO cargo_com.order_party (tenant_id,order_id,role,version,kind,to_order_text,party_id,party_revision,
            legal_name,english_name,country_code,identifiers,role_code,role_valid_from,role_valid_until,role_evidence_ref,address,pickup,delivery,
            has_contact,overridden,reason,set_by,set_at,entered_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25)`,
			tenant, id, p.Role, p.Version, string(p.Kind), orNull(p.ToOrderText), partyID, revision, legalName, english, country, string(ids),
			roleCode, from, until, evidence, address, pickup, delivery, p.HasContact, p.Overridden, orNull(p.Reason), p.SetBy, p.SetAt, p.EnteredAt); err != nil {
			return err
		}
		if c := p.Contact; c != nil {
			var contactID, contactVersion, notice, provide, abroad, basis, evidence, consentRevision any
			if c.Source == com.FromORG {
				contactID, contactVersion = c.ORGContactID, c.ORGVersion
			}
			if k := c.Consent; k != nil {
				notice, provide, abroad, basis, evidence = k.NoticeVersion, k.ProvideToOthers, k.CrossBorder, k.Basis, k.EvidenceRef
				if k.ORGRevision > 0 {
					consentRevision = k.ORGRevision
				}
			}
			if _, err = q.Exec(ctx, `INSERT INTO cargo_com.order_party_contact (tenant_id,order_id,role,version,source,org_contact_id,
                org_contact_version,name,phone,mobile,email,overridden_by,override_reason,consent_notice_version,consent_provide_to_others,
                consent_cross_border,consent_basis,consent_evidence_ref,consent_org_revision)
                VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`,
				tenant, id, p.Role, p.Version, string(c.Source), contactID, contactVersion, c.Name, orNull(c.Phone), orNull(c.Mobile),
				orNull(c.Email), orNull(c.OverriddenBy), orNull(c.OverrideReason), notice, provide, abroad, basis, evidence, consentRevision); err != nil {
				return err
			}
		}
	}
	return nil
}

func jsonDocs(as ...*com.AddressSnapshot) (out0, out1, out2 any, err error) {
	out := make([]any, len(as))
	for i, a := range as {
		if a == nil {
			continue
		}
		blob, err := json.Marshal(toDoc(a))
		if err != nil {
			return nil, nil, nil, err
		}
		out[i] = string(blob)
	}
	return out[0], out[1], out[2], nil
}

// PartyReader reads orders with their parties for the order-parties query, and the contacts frozen on them.
type PartyReader struct{ DB *bentopg.DB }

func (r PartyReader) ReadOrder(ctx context.Context, tenant com.TenantID, id com.OrderID) (*com.CustomerOrder, int64, error) {
	q, err := r.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, 0, err
	}
	return readOrder(ctx, q, tenant, id)
}

// ReadContacts returns the contacts frozen on an order, by role and version; erased ones are left out.
func (r PartyReader) ReadContacts(ctx context.Context, tenant com.TenantID, id com.OrderID) (map[string]map[int64]com.ContactSnapshot, error) {
	q, err := r.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT role,version,source,coalesce(org_contact_id,''),coalesce(org_contact_version,0),name,
        coalesce(phone,''),coalesce(mobile,''),coalesce(email,''),coalesce(overridden_by,''),coalesce(override_reason,''),
        consent_notice_version,coalesce(consent_provide_to_others,false),coalesce(consent_cross_border,false),coalesce(consent_basis,''),
        coalesce(consent_evidence_ref,''),coalesce(consent_org_revision,0)
      FROM cargo_com.order_party_contact WHERE tenant_id=$1 AND order_id=$2 AND NOT erased`, string(tenant), string(id))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[int64]com.ContactSnapshot{}
	for rows.Next() {
		var role, source string
		var version int64
		var c com.ContactSnapshot
		var notice *string
		var k com.ContactConsent
		if err = rows.Scan(&role, &version, &source, &c.ORGContactID, &c.ORGVersion, &c.Name, &c.Phone, &c.Mobile, &c.Email,
			&c.OverriddenBy, &c.OverrideReason, &notice, &k.ProvideToOthers, &k.CrossBorder, &k.Basis, &k.EvidenceRef, &k.ORGRevision); err != nil {
			return nil, err
		}
		if notice != nil {
			k.NoticeVersion = *notice
			c.Consent = &k
		}
		c.Source = com.AddressSource(source)
		if out[role] == nil {
			out[role] = map[int64]com.ContactSnapshot{}
		}
		out[role][version] = c
	}
	return out, rows.Err()
}
