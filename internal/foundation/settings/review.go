package settings

import (
	"strings"
	"time"
	"unicode/utf8"
)

// The review of a single administrator's self-approval (V2.102, batch F). A loosening that its own proposer approved as the
// tenant's single administrator is an exception: someone else reviews it, once, with a conclusion and a note, from the
// records. The review changes no setting: NOT_ACCEPTED is a record, and undoing the change is a normal change set.

// Conclusions of a review.
const (
	ConclusionAccepted    = "ACCEPTED"
	ConclusionNotAccepted = "NOT_ACCEPTED"
)

// MaxReviewNote is the longest note of a review, in characters.
const MaxReviewNote = 2000

// Kinds of refusal of a review.
const (
	ErrNotSelfApproved kind = "SETTINGS_NOT_SELF_APPROVED"
	ErrReviewer        kind = "SETTINGS_REVIEWER_IS_APPROVER"
	ErrReviewed        kind = "SETTINGS_ALREADY_REVIEWED"
	ErrConclusion      kind = "SETTINGS_REVIEW_CONCLUSION"
)

// SelfReview is the review of one self-approved change set.
type SelfReview struct {
	ChangeID   string
	By         string
	At         time.Time
	Conclusion string
	Note       string
}

// ValidConclusion says that a conclusion is one of the two.
func ValidConclusion(c string) bool { return c == ConclusionAccepted || c == ConclusionNotAccepted }

// CheckConclusion refuses a conclusion that is not ACCEPTED or NOT_ACCEPTED, and a note that is required and missing
// (NOT_ACCEPTED) or given and not 1 to 2000 characters. what names the review ("a review", "the week's review") for the words.
func CheckConclusion(what, conclusion, note string) error {
	switch {
	case !ValidConclusion(conclusion):
		return Refuse(ErrConclusion, "the conclusion of %s is ACCEPTED or NOT_ACCEPTED, not %q; choose one", what, conclusion)
	case conclusion == ConclusionNotAccepted && strings.TrimSpace(note) == "":
		return Refuse(ErrConclusion, "%s that is NOT_ACCEPTED says why in a note of 1 to %d characters; write it", what, MaxReviewNote)
	case note != "" && (strings.TrimSpace(note) == "" || utf8.RuneCountInString(note) > MaxReviewNote):
		return Refuse(ErrConclusion, "the note of %s is 1 to %d characters when given; shorten it, or leave it out for ACCEPTED", what, MaxReviewNote)
	}
	return nil
}

// ReviewSelfApproval answers the review of change set cs by the reviewer at a time. The change set was approved by its own
// proposer as the tenant's single administrator (with an acknowledgement); it has no review yet (existing); the reviewer is
// not the person who approved it; the conclusion is ACCEPTED or NOT_ACCEPTED, and a NOT_ACCEPTED has a note.
func ReviewSelfApproval(cs ChangeSet, existing *SelfReview, reviewer, conclusion, note string, at time.Time) (SelfReview, error) {
	switch {
	case cs.Approval == nil || !cs.Approval.SelfApproved():
		return SelfReview{}, Refuse(ErrNotSelfApproved, "change set %s was not approved by its own proposer as the tenant's single administrator, so it is not reviewed here; "+
			"read the exception report for the self-approved change sets of a period, and review one of those", cs.ID)
	case existing != nil:
		return SelfReview{}, Refuse(ErrReviewed, "change set %s was reviewed by %s at %s (%s) and a review is not changed; "+
			"to undo the change, propose a normal change set that restores the earlier values", cs.ID, existing.By, existing.At.UTC().Format(time.RFC3339), existing.Conclusion)
	case strings.TrimSpace(reviewer) == "" || strings.TrimSpace(reviewer) != reviewer || len(reviewer) > 128 || at.IsZero() || at.Before(cs.Approval.At):
		return SelfReview{}, Refuse(ErrInvalid, "a review names its reviewer (1 to 128 bytes, no spaces around it) and a time not before the approval; try the command again")
	case reviewer == cs.Approval.By:
		return SelfReview{}, Refuse(ErrReviewer, "change set %s was approved by %s, who may not review their own approval; have another holder of the review permission review it", cs.ID, reviewer)
	}
	if err := CheckConclusion("a review", conclusion, note); err != nil {
		return SelfReview{}, err
	}
	return SelfReview{ChangeID: cs.ID, By: reviewer, At: at, Conclusion: conclusion, Note: strings.TrimSpace(note)}, nil
}

// ReviewedLine is a self-approved change set with its review.
type ReviewedLine struct {
	SelfApprovedLine
	Review SelfReview
}

// SplitReviews divides the self-approved change sets of a period into those with no review and those with one.
func SplitReviews(lines []SelfApprovedLine, reviews []SelfReview) (pending []SelfApprovedLine, reviewed []ReviewedLine) {
	by := map[string]SelfReview{}
	for _, r := range reviews {
		by[r.ChangeID] = r
	}
	for _, l := range lines {
		if r, ok := by[l.ChangeID]; ok {
			reviewed = append(reviewed, ReviewedLine{SelfApprovedLine: l, Review: r})
		} else {
			pending = append(pending, l)
		}
	}
	return pending, reviewed
}
