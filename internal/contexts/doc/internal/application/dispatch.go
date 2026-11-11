package application

import (
	"context"
	"fmt"
	"time"

	doc "go.idp.xyz/idp-cargo/internal/contexts/doc/internal/domain"
)

// ActionReadProvisionRecords lets an actor read the provision and cross-border records (privacy audit).
const ActionReadProvisionRecords Action = "ReadProvisionRecords"

// rendering is a version as a rendering shows it with a status: issued versions with their issue time.
func rendering(v doc.Version, status string, contacts []CopyContact) Rendering {
	r := Rendering{Doc: v.Doc, Version: v.No, SHA256: v.SHA256, Status: status, Contacts: contacts}
	if v.Issue != nil && status != doc.VersionDraft && status != doc.VersionApproved {
		r.IssuedAt = v.Issue.At
	}
	return r
}

// version picks a document version: the given one, else the valid issued one, else the latest.
func version(m *doc.Document, no int) (doc.Version, error) {
	versions := m.State().Versions
	if no > 0 {
		if no > len(versions) {
			return doc.Version{}, fmt.Errorf("%w: no version %d", ErrInvalidQuery, no)
		}
		return versions[no-1], nil
	}
	if v, ok := m.Current(); ok && m.State().State != doc.StateCancelled {
		return v, nil
	}
	return m.Latest(), nil
}

// Render renders a version to PDF, without contacts.
func (r Reads) Render(ctx context.Context, renderer Renderer, tenant doc.TenantID, id doc.DocumentID, no int) ([]byte, string, Rendering, error) {
	if renderer == nil {
		return nil, "", Rendering{}, ErrMissingDependency
	}
	m, _, err := r.Document(ctx, tenant, id)
	if err != nil {
		return nil, "", Rendering{}, err
	}
	v, err := version(m, no)
	if err != nil {
		return nil, "", Rendering{}, err
	}
	in := rendering(v, m.Status(v.No), nil)
	pdf, err := renderer.Render(in)
	if err != nil {
		return nil, "", Rendering{}, err
	}
	return pdf, doc.Digest(pdf), in, nil
}

// DispatchInput is what a dispatch is prepared for.
type DispatchInput struct {
	DocumentID doc.DocumentID
	Version    int
	Recipient  string
	Role       string
	Channel    string
}

// copyContacts reads the contacts of a house version's first order, whose parties the version carries, from COM:
// those of the party versions the version froze.
func (d Deps) copyContacts(ctx context.Context, s Scope, v doc.Version) (OrderContacts, error) {
	if v.Doc.Kind != string(doc.KindHAWB) || len(v.Doc.Orders) == 0 {
		return OrderContacts{}, nil
	}
	ref := v.Doc.Orders[0]
	got, err := d.Parties.Contacts(ctx, s.TenantID, s.ActorID, ref.OrderID)
	if err != nil {
		return OrderContacts{}, err
	}
	frozen := map[string]int64{doc.RecipientShipper: ref.ShipperVersion, doc.RecipientConsignee: ref.ConsigneeVersion, doc.RecipientNotifyParty: ref.NotifyVersion}
	out := OrderContacts{OrderingParty: got.OrderingParty}
	for _, c := range got.Contacts {
		if version, ok := frozen[c.PartyRole]; ok && version != 0 && version == c.PartyVersion && c.OrderID == ref.OrderID {
			out.Contacts = append(out.Contacts, c)
		}
	}
	return out, nil
}

// printed returns the contacts a copy prints: those decided INCLUDED, as COM holds them now; ok is false when one
// of them is no longer there as it was decided.
func printed(decided []doc.DispatchContact, sources []doc.ContactSource) ([]CopyContact, bool) {
	byKey := map[string]doc.ContactSource{}
	for _, c := range sources {
		byKey[c.Key] = c
	}
	var out []CopyContact
	for _, d := range decided {
		if d.Status != doc.ContactIncluded {
			continue
		}
		c, ok := byKey[d.Key]
		if !ok || c.Erased {
			return nil, false
		}
		out = append(out, CopyContact{Role: d.PartyRole, Name: c.Name, Phone: c.Phone, Mobile: c.Mobile, Email: c.Email})
	}
	return out, true
}

// PrepareDispatch prepares a copy for a recipient: it reads the recipient's country from ORG, the contacts and their
// consent from COM and the register from ORG before DOC's transaction (T3), decides each contact, renders the copy
// and records its SHA-256 with the provision rows.
func (d Deps) PrepareDispatch(ctx context.Context, c Command, in DispatchInput) (Receipt, error) {
	if d.Dispatches == nil || d.Renderer == nil || d.Parties == nil || d.Directory == nil {
		return Receipt{}, ErrMissingDependency
	}
	if in.DocumentID == "" || in.Version < 0 || in.Recipient == "" {
		return Receipt{}, fmt.Errorf("%w: a dispatch names its document, a version (0: the current one) and its recipient", ErrInvalidCommand)
	}
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	var m *doc.Document
	if err = d.Tx.WithinTransaction(ctx, func(txCtx context.Context) (err error) {
		m, _, err = d.Documents.Read(txCtx, c.Scope.TenantID, in.DocumentID)
		return err
	}); err != nil {
		return Receipt{}, err
	}
	v, err := version(m, in.Version)
	if err != nil {
		return Receipt{}, err
	}
	country, err := d.Directory.Country(ctx, c.Scope.TenantID, in.Recipient)
	if err != nil {
		return Receipt{}, err
	}
	contacts, err := d.copyContacts(ctx, c.Scope, v)
	if err != nil {
		return Receipt{}, err
	}
	register, err := d.Directory.Register(ctx, c.Scope.TenantID, c.Subject)
	if err != nil {
		return Receipt{}, err
	}
	recipient := doc.Recipient{PartyID: in.Recipient, Country: country, Role: in.Role}
	decided, err := doc.Decide(recipient, contacts.OrderingParty, contacts.Contacts, register)
	if err != nil {
		return Receipt{}, err
	}
	shown, _ := printed(decided, contacts.Contacts)
	status := m.Status(v.No)
	pdf, err := d.Renderer.Render(rendering(v, status, shown))
	if err != nil {
		return Receipt{}, err
	}
	state := doc.DispatchState{DispatchID: c.Subject, DocumentID: in.DocumentID, Version: v.No, VersionStatus: status, Recipient: recipient,
		OrderingParty: contacts.OrderingParty, Channel: in.Channel, Contacts: decided, CopySHA256: doc.Digest(pdf), Template: d.Renderer.Template(),
		RegisterYear: register.Year, RegisterState: register.State}
	return d.write(ctx, c, hash, "DocumentDispatch", func(txCtx context.Context) (Receipt, aggregate, error) {
		current, _, err := d.Documents.Read(txCtx, c.Scope.TenantID, in.DocumentID)
		if err != nil {
			return Receipt{}, nil, err
		}
		x, err := doc.PrepareDispatch(state, current, c.Scope.ActorID, c.OccurredAt)
		if err != nil {
			return Receipt{}, nil, err
		}
		x.Stamp(entered)
		revision, err := d.Dispatches.Insert(txCtx, x)
		return Receipt{SubjectID: c.Subject, NewRevision: revision, Version: v.No}, x, err
	})
}

// DispatchCopy renders a dispatch's copy again from the version and the contacts as COM holds them now, and returns
// it only if it is the copy recorded.
func (d Deps) DispatchCopy(ctx context.Context, s Scope, id string) ([]byte, *doc.Dispatch, error) {
	if d.Dispatches == nil || d.Renderer == nil || d.Parties == nil {
		return nil, nil, ErrMissingDependency
	}
	var x *doc.Dispatch
	var m *doc.Document
	err := d.Tx.WithinTransaction(ctx, func(txCtx context.Context) (err error) {
		if x, _, err = d.Dispatches.Read(txCtx, s.TenantID, id); err != nil {
			return err
		}
		m, _, err = d.Documents.Read(txCtx, s.TenantID, x.State().DocumentID)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	st := x.State()
	if st.Template != d.Renderer.Template() {
		return nil, nil, fmt.Errorf("%w: rendered under template %s", doc.ErrCopyChanged, st.Template)
	}
	v, err := version(m, st.Version)
	if err != nil {
		return nil, nil, err
	}
	contacts, err := d.copyContacts(ctx, s, v)
	if err != nil {
		return nil, nil, err
	}
	shown, ok := printed(st.Contacts, contacts.Contacts)
	if !ok {
		return nil, nil, doc.ErrCopyChanged
	}
	pdf, err := d.Renderer.Render(rendering(v, st.VersionStatus, shown))
	if err != nil {
		return nil, nil, err
	}
	if doc.Digest(pdf) != st.CopySHA256 {
		return nil, nil, doc.ErrCopyChanged
	}
	return pdf, x, nil
}

// RecordOutcome records a dispatch's outcome; SENT and UNKNOWN read ORG's register first (T3) to check it holds the
// dispatch's cross-border contacts.
func (d Deps) RecordOutcome(ctx context.Context, c Command, outcome, reference string) (Receipt, error) {
	if d.Dispatches == nil || d.Directory == nil {
		return Receipt{}, ErrMissingDependency
	}
	hash, entered, stored, replayed, err := d.begin(ctx, c)
	if err != nil || replayed {
		return stored, err
	}
	var reserved []string
	if outcome != doc.DispatchFailed {
		register, err := d.Directory.Register(ctx, c.Scope.TenantID, c.Subject)
		if err != nil {
			return Receipt{}, err
		}
		reserved = register.DispatchKeys
	}
	return d.write(ctx, c, hash, "DocumentDispatch", func(txCtx context.Context) (Receipt, aggregate, error) {
		x, revision, err := d.Dispatches.Load(txCtx, c.Scope.TenantID, c.Subject)
		if err != nil {
			return Receipt{}, nil, err
		}
		if revision != c.ExpectedRevision {
			return Receipt{}, nil, doc.ErrConflict
		}
		if err = x.RecordOutcome(outcome, reference, reserved, c.Scope.ActorID, c.OccurredAt); err != nil {
			return Receipt{}, nil, err
		}
		x.Stamp(entered)
		if revision, err = d.Dispatches.Save(txCtx, x, revision); err != nil {
			return Receipt{}, nil, err
		}
		return Receipt{SubjectID: c.Subject, NewRevision: revision, Version: x.State().Version}, x, nil
	})
}

// Dispatch reads a dispatch.
func (r Reads) Dispatch(ctx context.Context, store DispatchStore, tenant doc.TenantID, id string) (*doc.Dispatch, int64, error) {
	if tenant == "" || id == "" || store == nil {
		return nil, 0, ErrInvalidQuery
	}
	var x *doc.Dispatch
	var revision int64
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) (err error) {
		x, revision, err = store.Read(txCtx, tenant, id)
		return err
	})
	return x, revision, err
}

// Dispatches lists a document's dispatches.
func (r Reads) Dispatches(ctx context.Context, store DispatchStore, tenant doc.TenantID, id doc.DocumentID) ([]*doc.Dispatch, error) {
	if tenant == "" || id == "" || store == nil {
		return nil, ErrInvalidQuery
	}
	var out []*doc.Dispatch
	err := r.Tx.WithinTransaction(ctx, func(txCtx context.Context) (err error) {
		out, err = store.List(txCtx, tenant, id)
		return err
	})
	return out, err
}

// Provisions lists the dispatches prepared in a period with their provision records, for an actor allowed
// ReadProvisionRecords (privacy audit; the tenant's impact assessment).
func (d Deps) Provisions(ctx context.Context, s Scope, from, until time.Time) ([]*doc.Dispatch, error) {
	if d.Dispatches == nil || d.Auth == nil {
		return nil, ErrMissingDependency
	}
	if s.TenantID == "" || s.ActorID == "" {
		return nil, ErrUnauthenticated
	}
	if from.IsZero() || !until.After(from) {
		return nil, fmt.Errorf("%w: a period", ErrInvalidQuery)
	}
	if err := d.authorize(ctx, s, ActionReadProvisionRecords, string(s.TenantID)); err != nil {
		return nil, err
	}
	var out []*doc.Dispatch
	err := d.Tx.WithinTransaction(ctx, func(txCtx context.Context) (err error) {
		out, err = d.Dispatches.Provisions(txCtx, s.TenantID, from, until)
		return err
	})
	return out, err
}
