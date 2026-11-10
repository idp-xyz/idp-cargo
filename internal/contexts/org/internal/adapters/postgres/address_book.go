package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentorepo "go.idp.xyz/idp-bento-go/repository"
	org "go.idp.xyz/idp-cargo/internal/contexts/org/internal/domain"
)

// AddressBookStore keeps party address books in cargo_org (migration 000007). Address and contact versions,
// defaults and the change record of every revision are rows that are never rewritten; only an erasure clears
// the personal fields of a contact's versions. The database checks the book's consistency when the
// transaction commits.
type AddressBookStore struct{ DB *bentopg.DB }

// Load locks the party row for share and the address book row for the transaction, then reads the book.
func (s AddressBookStore) Load(ctx context.Context, tenant org.TenantID, party org.PartyID) (*org.AddressBook, org.PartyStatus, error) {
	q, err := s.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, "", err
	}
	var status string
	err = q.QueryRow(ctx, `SELECT status FROM cargo_org.business_party WHERE tenant_id=$1 AND party_id=$2 FOR SHARE`,
		string(tenant), string(party)).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", bentorepo.ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	var revision int64
	err = q.QueryRow(ctx, `SELECT revision FROM cargo_org.party_address_book WHERE tenant_id=$1 AND party_id=$2 FOR NO KEY UPDATE`,
		string(tenant), string(party)).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		book, err := org.NewAddressBook(tenant, party)
		return book, org.PartyStatus(status), err
	}
	if err != nil {
		return nil, "", err
	}
	book, err := readBook(ctx, q, tenant, party, revision, true)
	return book, org.PartyStatus(status), err
}

// Save writes what the book recorded since it was loaded. A book stored or changed meanwhile is a conflict.
func (s AddressBookStore) Save(ctx context.Context, book *org.AddressBook) error {
	if book == nil {
		return org.ErrInvalidIdentity
	}
	u := book.Unsaved()
	if len(u.Changes) == 0 {
		return nil
	}
	q, err := s.DB.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	tenant, party := string(book.Tenant()), string(book.Party())
	if book.LoadedRevision() == 0 {
		_, err = q.Exec(ctx, `INSERT INTO cargo_org.party_address_book(tenant_id,party_id,revision) VALUES($1,$2,$3)`, tenant, party, book.Revision())
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "party_address_book_pkey" {
			return fmt.Errorf("%w: address book of %s was opened meanwhile", bentorepo.ErrConflict, party)
		}
	} else {
		var tag pgconn.CommandTag
		tag, err = q.Exec(ctx, `UPDATE cargo_org.party_address_book SET revision=$3,updated_at=now()
          WHERE tenant_id=$1 AND party_id=$2 AND revision=$4`, tenant, party, book.Revision(), book.LoadedRevision())
		if err == nil && tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: address book of %s changed meanwhile", bentorepo.ErrConflict, party)
		}
	}
	if err != nil {
		return err
	}
	for _, c := range u.Changes {
		if _, err = q.Exec(ctx, `INSERT INTO cargo_org.party_address_book_change
            (tenant_id,party_id,revision,action,entry_id,purpose,actor_id,reason,recorded_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			tenant, party, c.Revision, string(c.Action), c.EntryID, nullable(c.Purpose), c.ActorID, nullable(c.Reason), c.RecordedAt); err != nil {
			return err
		}
	}
	for _, v := range u.AddressVersions {
		if err = saveAddressVersion(ctx, q, tenant, party, v); err != nil {
			return err
		}
	}
	for _, d := range u.AddressDefaults {
		if _, err = q.Exec(ctx, `INSERT INTO cargo_org.party_address_default(tenant_id,party_id,purpose,revision,address_id) VALUES($1,$2,$3,$4,$5)`,
			tenant, party, d.Purpose, d.Revision, nullable(d.EntryID)); err != nil {
			return err
		}
	}
	// Erase the stored versions first; the versions this book adds for an erased contact are erased already.
	for _, id := range u.Erased {
		if _, err = q.Exec(ctx, `UPDATE cargo_org.party_contact_version SET name=NULL,phone=NULL,mobile=NULL,email=NULL,erased=true
            WHERE tenant_id=$1 AND party_id=$2 AND contact_id=$3 AND NOT erased`, tenant, party, string(id)); err != nil {
			return err
		}
	}
	for _, v := range u.ContactVersions {
		if err = saveContactVersion(ctx, q, tenant, party, v); err != nil {
			return err
		}
	}
	for _, d := range u.ContactDefaults {
		if _, err = q.Exec(ctx, `INSERT INTO cargo_org.party_contact_default(tenant_id,party_id,purpose,revision,contact_id) VALUES($1,$2,$3,$4,$5)`,
			tenant, party, d.Purpose, d.Revision, nullable(d.EntryID)); err != nil {
			return err
		}
	}
	for _, c := range u.Consents {
		if _, err = q.Exec(ctx, `INSERT INTO cargo_org.party_contact_consent
            (tenant_id,party_id,contact_id,revision,notice_version,provide_to_others,cross_border,basis,evidence_ref) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			tenant, party, string(c.ContactID), c.Revision, c.NoticeVersion, c.ProvideToOthers, c.CrossBorder, string(c.Basis), c.EvidenceRef); err != nil {
			return err
		}
	}
	return nil
}

func saveAddressVersion(ctx context.Context, q bentopg.Executor, tenant, party string, v org.AddressVersion) error {
	var tag pgconn.CommandTag
	var err error
	if v.Version == 1 {
		tag, err = q.Exec(ctx, `INSERT INTO cargo_org.party_address(tenant_id,party_id,address_id,short_code,status,version) VALUES($1,$2,$3,$4,$5,1)`,
			tenant, party, string(v.ID), v.ShortCode, string(v.Status))
	} else {
		tag, err = q.Exec(ctx, `UPDATE cargo_org.party_address SET short_code=$4,status=$5,version=$6
          WHERE tenant_id=$1 AND party_id=$2 AND address_id=$3 AND version=$7`, tenant, party, string(v.ID), v.ShortCode, string(v.Status), v.Version, v.Version-1)
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: address %s is not at version %d", bentorepo.ErrConflict, v.ID, v.Version-1)
	}
	var language, localName, localCity, localRegion any
	var localLines []string
	if l := v.Local; l != nil {
		language, localName, localLines, localCity, localRegion = l.Language, l.Name, l.Lines, l.City, nullable(l.Region)
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_org.party_address_version
        (tenant_id,party_id,address_id,version,revision,status,short_code,purposes,name,lines,city,region,postcode,country_code,unlocode,residential,
         local_language,local_name,local_lines,local_city,local_region,phone,email)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)`,
		tenant, party, string(v.ID), v.Version, v.Revision, string(v.Status), v.ShortCode, texts(v.Purposes), v.Name, v.Lines, v.City,
		nullable(v.Region), nullable(v.Postcode), v.CountryCode, nullable(v.UNLocode), v.Residential,
		language, localName, localLines, localCity, localRegion, nullable(v.Phone), nullable(v.Email))
	return err
}

func saveContactVersion(ctx context.Context, q bentopg.Executor, tenant, party string, v org.ContactVersion) error {
	var tag pgconn.CommandTag
	var err error
	if v.Version == 1 {
		tag, err = q.Exec(ctx, `INSERT INTO cargo_org.party_contact(tenant_id,party_id,contact_id,status,version) VALUES($1,$2,$3,$4,1)`,
			tenant, party, string(v.ID), string(v.Status))
	} else {
		tag, err = q.Exec(ctx, `UPDATE cargo_org.party_contact SET status=$4,version=$5
          WHERE tenant_id=$1 AND party_id=$2 AND contact_id=$3 AND version=$6`, tenant, party, string(v.ID), string(v.Status), v.Version, v.Version-1)
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: contact %s is not at version %d", bentorepo.ErrConflict, v.ID, v.Version-1)
	}
	_, err = q.Exec(ctx, `INSERT INTO cargo_org.party_contact_version
        (tenant_id,party_id,contact_id,version,revision,status,purposes,name,phone,mobile,email,erased)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		tenant, party, string(v.ID), v.Version, v.Revision, string(v.Status), texts(v.Purposes),
		nullable(v.Name), nullable(v.Phone), nullable(v.Mobile), nullable(v.Email), v.Erased)
	return err
}

// AddressBookReader reads address books for queries. It locks the book row for share, so the statements
// that follow see one revision: writers wait for the read, and the read waits for a writer to commit.
type AddressBookReader struct{ DB *bentopg.DB }

func (r AddressBookReader) Read(ctx context.Context, tenant org.TenantID, party org.PartyID, withContacts bool) (*org.AddressBook, error) {
	q, err := r.DB.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	var one int
	err = q.QueryRow(ctx, `SELECT 1 FROM cargo_org.business_party WHERE tenant_id=$1 AND party_id=$2`, string(tenant), string(party)).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, bentorepo.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var revision int64
	err = q.QueryRow(ctx, `SELECT revision FROM cargo_org.party_address_book WHERE tenant_id=$1 AND party_id=$2 FOR SHARE`,
		string(tenant), string(party)).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return org.NewAddressBook(tenant, party)
	}
	if err != nil {
		return nil, err
	}
	return readBook(ctx, q, tenant, party, revision, withContacts)
}

func (r AddressBookReader) AddressVersion(ctx context.Context, tenant org.TenantID, party org.PartyID, id org.AddressID, version int64) (org.AddressVersion, error) {
	q, err := r.DB.ReadExecutor(ctx)
	if err != nil {
		return org.AddressVersion{}, err
	}
	rows, err := q.Query(ctx, addressColumns+` FROM cargo_org.party_address_version v
        WHERE v.tenant_id=$1 AND v.party_id=$2 AND v.address_id=$3 AND v.version=$4`, string(tenant), string(party), string(id), version)
	if err != nil {
		return org.AddressVersion{}, err
	}
	versions, err := scanAddresses(rows)
	if err != nil {
		return org.AddressVersion{}, err
	}
	if len(versions) != 1 {
		return org.AddressVersion{}, fmt.Errorf("%w: %s has no version %d", org.ErrUnknownAddress, id, version)
	}
	return versions[0], nil
}

const addressColumns = `SELECT v.address_id,v.version,v.revision,v.status,v.short_code,v.purposes,v.name,v.lines,v.city,
  coalesce(v.region,''),coalesce(v.postcode,''),v.country_code,coalesce(v.unlocode,''),v.residential,
  v.local_language,coalesce(v.local_name,''),v.local_lines,coalesce(v.local_city,''),coalesce(v.local_region,''),
  coalesce(v.phone,''),coalesce(v.email,'')`

// readBook reads the current version of every address and, if asked, contact, and the current defaults.
func readBook(ctx context.Context, q bentopg.Querier, tenant org.TenantID, party org.PartyID, revision int64, withContacts bool) (*org.AddressBook, error) {
	t, p := string(tenant), string(party)
	rows, err := q.Query(ctx, addressColumns+` FROM cargo_org.party_address h
        JOIN cargo_org.party_address_version v ON v.tenant_id=h.tenant_id AND v.party_id=h.party_id AND v.address_id=h.address_id AND v.version=h.version
        WHERE h.tenant_id=$1 AND h.party_id=$2 ORDER BY h.address_id`, t, p)
	if err != nil {
		return nil, err
	}
	addresses, err := scanAddresses(rows)
	if err != nil {
		return nil, err
	}
	addressDefaults := map[org.AddressPurpose]org.AddressID{}
	if err = readDefaults(ctx, q, `SELECT purpose,address_id FROM (SELECT DISTINCT ON (purpose) purpose,address_id
        FROM cargo_org.party_address_default WHERE tenant_id=$1 AND party_id=$2 ORDER BY purpose,revision DESC) d WHERE address_id IS NOT NULL`,
		t, p, func(purpose, id string) { addressDefaults[org.AddressPurpose(purpose)] = org.AddressID(id) }); err != nil {
		return nil, err
	}
	var contacts []org.ContactVersion
	contactDefaults := map[org.ContactPurpose]org.ContactID{}
	if withContacts {
		if contacts, err = readContacts(ctx, q, t, p); err != nil {
			return nil, err
		}
		if err = readDefaults(ctx, q, `SELECT purpose,contact_id FROM (SELECT DISTINCT ON (purpose) purpose,contact_id
            FROM cargo_org.party_contact_default WHERE tenant_id=$1 AND party_id=$2 ORDER BY purpose,revision DESC) d WHERE contact_id IS NOT NULL`,
			t, p, func(purpose, id string) { contactDefaults[org.ContactPurpose(purpose)] = org.ContactID(id) }); err != nil {
			return nil, err
		}
	}
	book, err := org.RestoreAddressBook(tenant, party, revision, addresses, contacts, addressDefaults, contactDefaults)
	if err != nil || !withContacts {
		return book, err
	}
	consents, err := readConsents(ctx, q, t, p)
	if err != nil {
		return nil, err
	}
	return book, book.RestoreConsents(consents)
}

// readConsents reads the current consent of each contact that has one.
func readConsents(ctx context.Context, q bentopg.Querier, tenant, party string) ([]org.ConsentRecord, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (contact_id) contact_id,revision,notice_version,provide_to_others,cross_border,basis,evidence_ref
      FROM cargo_org.party_contact_consent WHERE tenant_id=$1 AND party_id=$2 ORDER BY contact_id,revision DESC`, tenant, party)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []org.ConsentRecord
	for rows.Next() {
		var r org.ConsentRecord
		var id, basis string
		if err = rows.Scan(&id, &r.Revision, &r.NoticeVersion, &r.ProvideToOthers, &r.CrossBorder, &basis, &r.EvidenceRef); err != nil {
			return nil, err
		}
		r.ContactID, r.Basis = org.ContactID(id), org.ConsentBasis(basis)
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanAddresses(rows pgx.Rows) ([]org.AddressVersion, error) {
	defer rows.Close()
	var out []org.AddressVersion
	for rows.Next() {
		var v org.AddressVersion
		var id, status string
		var purposes, localLines []string
		var language *string
		var local org.LocalAddress
		if err := rows.Scan(&id, &v.Version, &v.Revision, &status, &v.ShortCode, &purposes, &v.Name, &v.Lines, &v.City,
			&v.Region, &v.Postcode, &v.CountryCode, &v.UNLocode, &v.Residential,
			&language, &local.Name, &localLines, &local.City, &local.Region, &v.Phone, &v.Email); err != nil {
			return nil, err
		}
		v.ID, v.Status = org.AddressID(id), org.EntryStatus(status)
		for _, x := range purposes {
			v.Purposes = append(v.Purposes, org.AddressPurpose(x))
		}
		if language != nil {
			local.Language, local.Lines = *language, localLines
			v.Local = &local
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func readContacts(ctx context.Context, q bentopg.Querier, tenant, party string) ([]org.ContactVersion, error) {
	rows, err := q.Query(ctx, `SELECT v.contact_id,v.version,v.revision,v.status,v.purposes,coalesce(v.name,''),coalesce(v.phone,''),
        coalesce(v.mobile,''),coalesce(v.email,''),v.erased
      FROM cargo_org.party_contact h
      JOIN cargo_org.party_contact_version v ON v.tenant_id=h.tenant_id AND v.party_id=h.party_id AND v.contact_id=h.contact_id AND v.version=h.version
      WHERE h.tenant_id=$1 AND h.party_id=$2 ORDER BY h.contact_id`, tenant, party)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []org.ContactVersion
	for rows.Next() {
		var v org.ContactVersion
		var id, status string
		var purposes []string
		if err = rows.Scan(&id, &v.Version, &v.Revision, &status, &purposes, &v.Name, &v.Phone, &v.Mobile, &v.Email, &v.Erased); err != nil {
			return nil, err
		}
		v.ID, v.Status = org.ContactID(id), org.EntryStatus(status)
		for _, x := range purposes {
			v.Purposes = append(v.Purposes, org.ContactPurpose(x))
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func readDefaults(ctx context.Context, q bentopg.Querier, sql, tenant, party string, put func(purpose, id string)) error {
	rows, err := q.Query(ctx, sql, tenant, party)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var purpose, id string
		if err = rows.Scan(&purpose, &id); err != nil {
			return err
		}
		put(purpose, id)
	}
	return rows.Err()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func texts[S ~string](in []S) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = string(s)
	}
	return out
}
