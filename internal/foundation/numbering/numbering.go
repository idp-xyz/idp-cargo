// Package numbering adapts idp-platform-go's numbering to Cargo (MD-28): the platform owns the template language and
// the sequence allocation; each bounded context owns its document types, its templates and its issued numbers, and keeps
// the platform's sequence table in its own schema. It is shared by the contexts and depends on none of them.
package numbering

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	platform "go.idp.xyz/idp-platform-go/numbering"
	platformpg "go.idp.xyz/idp-platform-go/numbering/postgres"
)

var (
	ErrInvalidTemplate = errors.New("numbering: a template is a literal prefix, then literals, {yyyy}, {yy}, {mm}, {q} (a month or quarter with its year) and one {seq:N} with N from 4 to 10")
	ErrExhausted       = errors.New("numbering: the sequence of the period has no number left within its width")
	ErrSeeded          = errors.New("numbering: the sequence is in use already")
)

// Cargo uses a subset of the platform's tokens: date parts for the reset period and one zero-padded sequence. Random,
// scrambled and time-of-day tokens would make document numbers unordered or unaccountable.
var (
	allowed = map[string]bool{"yyyy": true, "yy": true, "mm": true, "q": true, "seq": true}
	token   = regexp.MustCompile(`\{([a-z0-9]+)(?::(\d+))?\}`)
)

// Template is a parsed numbering template.
type Template struct {
	raw    string
	prefix string
	width  int
	t      *platform.Template
}

// Parse reads a template of Cargo's subset: it starts with a literal prefix and holds exactly one {seq:N}.
func Parse(raw string) (Template, error) {
	t, err := platform.Parse(raw)
	if err != nil {
		return Template{}, fmt.Errorf("%w: %v", ErrInvalidTemplate, err)
	}
	found := token.FindAllStringSubmatchIndex(raw, -1)
	if len(found) == 0 || found[0][0] == 0 {
		return Template{}, fmt.Errorf("%w: %q has no literal prefix", ErrInvalidTemplate, raw)
	}
	out := Template{raw: raw, prefix: raw[:found[0][0]], t: t}
	seqs := 0
	present := map[string]bool{}
	literal := token.ReplaceAllString(raw, "")
	for _, m := range found {
		name := raw[m[2]:m[3]]
		present[name] = true
		if !allowed[name] {
			return Template{}, fmt.Errorf("%w: token {%s}", ErrInvalidTemplate, name)
		}
		if name == "seq" {
			seqs++
			if m[4] < 0 || raw[m[4]] == '0' {
				return Template{}, fmt.Errorf("%w: {seq} needs its width, without a leading zero", ErrInvalidTemplate)
			}
			out.width, _ = strconv.Atoi(raw[m[4]:m[5]])
		} else if m[4] >= 0 {
			return Template{}, fmt.Errorf("%w: {%s} takes no width", ErrInvalidTemplate, name)
		}
	}
	switch {
	case seqs != 1, out.width < 4 || out.width > 10:
		return Template{}, fmt.Errorf("%w: %q needs one {seq:N} with N from 4 to 10", ErrInvalidTemplate, raw)
	case strings.ContainsAny(literal, "{}"):
		return Template{}, fmt.Errorf("%w: braces outside tokens in %q", ErrInvalidTemplate, raw)
	case (present["mm"] || present["q"]) && !present["yyyy"] && !present["yy"]:
		// the sequence restarts every month or quarter; without the year the numbers of a period recur a year later
		return Template{}, fmt.Errorf("%w: %q has a month or quarter without its year", ErrInvalidTemplate, raw)
	}
	return out, nil
}

// Resets reports whether the sequence restarts in each period (a template with a date token).
func (t Template) Resets() bool { return t.t.Granularity() != platform.Continuous }

// Raw returns the template as written; Prefix its literal prefix; Width the sequence's digits.
func (t Template) Raw() string    { return t.raw }
func (t Template) Prefix() string { return t.prefix }
func (t Template) Width() int     { return t.width }

// Period returns the reset period of a business date (UTC): the year, quarter or month the template's finest date
// token names, or "" for a sequence that never resets.
func (t Template) Period(date time.Time) string { return t.t.PeriodKey(date.UTC()) }

// Render writes the number of a sequence value on a business date; a value wider than the template's width is
// ErrExhausted (the platform would print it wider).
func (t Template) Render(seq int64, date time.Time) (string, error) {
	if seq < 1 || len(strconv.FormatInt(seq, 10)) > t.width {
		return "", fmt.Errorf("%w: %d in %s", ErrExhausted, seq, t.raw)
	}
	return t.t.Render(platform.RenderInput{Seq: seq, At: date.UTC()})
}

// Longest renders the template's widest number, to check it against a document's number rules.
func (t Template) Longest() (string, error) {
	return t.Render(pow10(t.width)-1, time.Date(2099, 12, 31, 0, 0, 0, 0, time.UTC))
}

func pow10(n int) int64 {
	p := int64(1)
	for range n {
		p *= 10
	}
	return p
}

// DocType names a document type's sequence for a legal entity: <kind>/<legal entity>. The scope is the tenant, which
// the sequence table's row-level security checks.
func DocType(kind, legalEntity string) string { return kind + "/" + legalEntity }

// Sequences takes numbers from the platform's sequence table in one context's schema. Every call runs inside the
// command's transaction, so a failed command releases what it took.
type Sequences struct {
	db     *bentopg.DB
	schema string
	store  *platformpg.SequenceStore
}

// NewSequences reads and writes <schema>.numbering_sequence.
func NewSequences(db *bentopg.DB, schema string) (*Sequences, error) {
	store, err := platformpg.NewSequenceStore(db, platformpg.WithSchema(schema))
	if err != nil {
		return nil, err
	}
	return &Sequences{db: db, schema: store.Schema(), store: store}, nil
}

// Next takes the next value of a tenant's sequence of a document type in a period.
func (s *Sequences) Next(ctx context.Context, tenant, docType, period string) (int64, error) {
	return s.store.Next(ctx, platform.SequenceKey{Scope: tenant, DocType: docType, Period: period})
}

// Seed starts a sequence after last, so that its next value is last+1 (a first number above 1). A sequence that
// exists already is ErrSeeded.
func (s *Sequences) Seed(ctx context.Context, tenant, docType, period string, last int64) error {
	if last < 1 {
		return nil
	}
	q, err := s.db.RequireExecutor(ctx)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO `+pgx.Identifier{s.schema}.Sanitize()+`.numbering_sequence (scope, doc_type, period, next_val) VALUES ($1, $2, $3, $4)`,
		tenant, docType, period, last)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("%w: %s %s", ErrSeeded, docType, period)
	}
	return err
}

// Last returns the last value taken per period of a tenant's sequence of a document type.
func (s *Sequences) Last(ctx context.Context, tenant, docType string) (map[string]int64, error) {
	q, err := s.db.ReadExecutor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT period, next_val FROM `+pgx.Identifier{s.schema}.Sanitize()+`.numbering_sequence WHERE scope = $1 AND doc_type = $2`,
		tenant, docType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var period string
		var last int64
		if err = rows.Scan(&period, &last); err != nil {
			return nil, err
		}
		out[period] = last
	}
	return out, rows.Err()
}
