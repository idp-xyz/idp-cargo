package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.idp.xyz/idp-cargo/internal/kernel"
)

// FIN V2.102 (batch B): the after-the-fact review of the LEAN profile, its weekly compensation for confirmations that
// have no second person. Each week, every attribution confirmed in it that was stated by a preview and is
//
//   - made with no second person (the person who submitted the apportionment is the person who previewed it),
//   - made with no voucher, or
//   - made by default (F8 DEFAULT_TO_REMAINING)
//
// is listed for finance's head, who marks the week reviewed with a note. The list is read from the apportionments, not
// kept: this file defines what a line of it is, the reasons it is listed and the digest by which a list that changed
// after its review can be told.
//
// What the records can tell, and what they cannot. "No second person" is told by FIN's own rows: an apportionment
// submitted from a preview names the person who made the preview and the person who submitted it, and they are the same
// person. FIN does not record, with an attribution, who asked for the rework that unloaded the house or who ended the
// house's place (PHY holds that, and FIN reads it only at a command), so a submitter who is the rework's asker or the
// house's ender, allowed under F1 UNRESTRICTED or a threshold, is not told as such: the list names the submitter and
// the previewer of each line, and when they are two people it does not ask whether the submitter is the asker or the
// ender. The setting in force is not read either: a preview and its submission by one person are listed under F2
// SUBMITTER_ONLY and under SUBMITTER_AND_PREVIEWER alike, because in both nobody else looked at it. An apportionment
// made in one step names no preview and, by fin/000019, states no attribution (it only carries them), so none of its
// rows is listed: a carried attribution was confirmed by the apportionment that stated it, which the list has in the
// week that stated it.

// Reasons an attribution is listed.
const (
	ReasonNoSecondPerson = "NO_SECOND_PERSON"
	ReasonNoVoucher      = "NO_VOUCHER"
	ReasonMadeByDefault  = "MADE_BY_DEFAULT"
)

// Refusals of the review, each with its way out.
var (
	ErrAfterTheFactReviewer = errors.New("fin: FIN_AFTER_THE_FACT_REVIEWER: the reviewer of a week is neither the submitter nor the previewer of any line of its list: another person reviews it")
	ErrAfterTheFactReviewed = errors.New("fin: FIN_AFTER_THE_FACT_REVIEWED: the week is reviewed already: read it (GetAfterTheFactReview); a week is reviewed once")
	ErrAfterTheFactBegun    = errors.New("fin: FIN_AFTER_THE_FACT_WEEK_NOT_BEGUN: the week has not begun: review it once it has, or name a week that has")
)

// MaxReviewNote is the longest note of a review, in characters.
const MaxReviewNote = 2000

var weekForm = regexp.MustCompile(`^([0-9]{4})-W([0-9]{2})$`)

// ParseWeek reads an ISO week, YYYY-Www, as the Monday at 00:00 UTC that begins it and the Monday after, which ends it
// (the week is a half-open interval of the apportionments' system time). Week 53 is a week only in a year that has one.
func ParseWeek(week string) (start, end time.Time, err error) {
	m := weekForm.FindStringSubmatch(week)
	if m != nil {
		year, _ := strconv.Atoi(m[1])
		no, _ := strconv.Atoi(m[2])
		jan4 := time.Date(year, time.January, 4, 0, 0, 0, 0, time.UTC)
		monday := jan4.AddDate(0, 0, -((int(jan4.Weekday()) + 6) % 7))
		start = monday.AddDate(0, 0, 7*(no-1))
		if y, w := start.ISOWeek(); no >= 1 && y == year && w == no {
			return start, start.AddDate(0, 0, 7), nil
		}
	}
	return time.Time{}, time.Time{}, fmt.Errorf("%w: a week is an ISO week written YYYY-Www, such as 2026-W48 (week 53 exists only in a year that has one)", ErrInvalid)
}

// WeekOf is the ISO week, YYYY-Www, that contains the time.
func WeekOf(t time.Time) string {
	y, w := t.UTC().ISOWeek()
	return fmt.Sprintf("%04d-W%02d", y, w)
}

// ReviewKey orders the lines of a list and is where a page follows: the pool, the apportionment, the line and the house.
type ReviewKey struct {
	PoolID          string
	ApportionmentNo int
	LineNo          int
	ShipmentID      string
}

// ReviewLine is one attribution of the list, as FIN's rows give it: the pool, the apportionment that confirmed it, the
// line and the house; what it put on the house's job and on the consolidation, in the line's currency and in the
// functional currency; who submitted the apportionment and who previewed it; and the marks the reasons read.
type ReviewLine struct {
	ReviewKey
	ConsolidationID    string
	JobID              string
	Currency           kernel.CurrencyCode
	FunctionalCurrency kernel.CurrencyCode
	Loss               int64
	LossLocal          int64
	Exception          int64
	ExceptionLocal     int64
	Party              string
	Voucher            string
	ByDefault          bool
	PreviewNo          int
	SubmittedBy        string
	PreviewedBy        string
	EnteredAt          time.Time
}

// Reasons are why the attribution is listed, in a fixed order. An attribution with none is not on the list.
func (l ReviewLine) Reasons() []string {
	var out []string
	if l.PreviewedBy != "" && l.PreviewedBy == l.SubmittedBy {
		out = append(out, ReasonNoSecondPerson)
	}
	if strings.TrimSpace(l.Voucher) == "" {
		out = append(out, ReasonNoVoucher)
	}
	if l.ByDefault {
		out = append(out, ReasonMadeByDefault)
	}
	return out
}

// ListDigest is the SHA-256, in lower-case hexadecimal, of the list in its order: every field of every line that a
// reader of the list sees, so that a line added, a line taken away or any figure or person changed gives another digest.
func ListDigest(lines []ReviewLine) string {
	h := sha256.New()
	for _, l := range lines {
		fmt.Fprintf(h, "%q|%d|%d|%q|%q|%q|%s|%s|%d|%d|%d|%d|%q|%q|%t|%d|%q|%q|%s|%s\n", l.PoolID, l.ApportionmentNo, l.LineNo, l.ShipmentID, l.ConsolidationID, l.JobID,
			l.Currency, l.FunctionalCurrency, l.Loss, l.LossLocal, l.Exception, l.ExceptionLocal, l.Party, l.Voucher, l.ByDefault, l.PreviewNo, l.SubmittedBy, l.PreviewedBy,
			l.EnteredAt.UTC().Format(time.RFC3339Nano), strings.Join(l.Reasons(), ","))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// AfterTheFactReviewState is the mark of a reviewed week: who reviewed it and when, the note, and the size and digest
// of the list as it was.
type AfterTheFactReviewState struct {
	TenantID      TenantID
	LegalEntityID string
	Week          string
	WeekStart     time.Time
	ReviewedBy    string
	Note          string
	ListSize      int
	ListDigest    string
	ReviewedAt    time.Time
}

// AfterTheFactReview is a reviewed week, the aggregate of this step. Its row is only inserted.
type AfterTheFactReview struct {
	state   AfterTheFactReviewState
	pending []Event
}

// ReviewSubject is the subject of a legal entity's week: its command subject, its receipt and its event.
func ReviewSubject(legalEntity, week string) string { return legalEntity + "/" + week }

// ReviewAfterTheFact marks a legal entity's week reviewed by the reviewer at a time. lines is the whole list of the
// week as the caller read it under the week's lock and done the mark the week has already, if it has one. The reviewer
// is neither the submitter nor the previewer of any line; a refusal names the first line of the list that says
// otherwise.
func ReviewAfterTheFact(tenant TenantID, legalEntity, week string, lines []ReviewLine, done *AfterTheFactReviewState, reviewer, note string, at time.Time) (*AfterTheFactReview, error) {
	start, _, err := ParseWeek(week)
	switch {
	case err != nil:
		return nil, err
	case blank(note) || utf8.RuneCountInString(note) > MaxReviewNote:
		return nil, fmt.Errorf("%w: a note is 1 to %d characters", ErrInvalid, MaxReviewNote)
	case tenant == "" || !identifier(legalEntity) || !identifier(reviewer) || at.IsZero():
		return nil, fmt.Errorf("%w: a review names its legal entity, its reviewer and a time", ErrInvalid)
	case done != nil:
		return nil, fmt.Errorf("%w (%s of %s was reviewed by %s at %s)", ErrAfterTheFactReviewed, week, legalEntity, done.ReviewedBy, done.ReviewedAt.UTC().Format(time.RFC3339))
	case at.Before(start):
		return nil, fmt.Errorf("%w (%s begins at %s, this server's time is %s)", ErrAfterTheFactBegun, week, start.Format(time.RFC3339), at.UTC().Format(time.RFC3339))
	}
	for _, l := range lines {
		role := ""
		switch {
		case l.SubmittedBy == reviewer && l.PreviewedBy == reviewer:
			role = "submitted and previewed"
		case l.SubmittedBy == reviewer:
			role = "submitted"
		case l.PreviewedBy == reviewer:
			role = "previewed"
		}
		if role != "" {
			return nil, fmt.Errorf("%w (%s %s apportionment %d of pool %s, which lists line %d of house %s)", ErrAfterTheFactReviewer, reviewer, role, l.ApportionmentNo,
				l.PoolID, l.LineNo, l.ShipmentID)
		}
	}
	r := &AfterTheFactReview{state: AfterTheFactReviewState{TenantID: tenant, LegalEntityID: legalEntity, Week: week, WeekStart: start, ReviewedBy: reviewer, Note: note,
		ListSize: len(lines), ListDigest: ListDigest(lines), ReviewedAt: at.UTC()}}
	r.pending = append(r.pending, Event{Type: "AfterTheFactReviewed.v1", At: at.UTC(), TenantID: tenant, SubjectID: ReviewSubject(legalEntity, week), LegalEntityID: legalEntity, Version: 1})
	return r, nil
}

// RestoreAfterTheFactReview rebuilds a stored mark.
func RestoreAfterTheFactReview(s AfterTheFactReviewState) *AfterTheFactReview {
	return &AfterTheFactReview{state: s}
}

func (r *AfterTheFactReview) State() AfterTheFactReviewState { return r.state }
func (r *AfterTheFactReview) Pending() []Event               { return append([]Event(nil), r.pending...) }
func (r *AfterTheFactReview) Acknowledge()                   { r.pending = nil }
