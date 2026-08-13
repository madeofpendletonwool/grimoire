// Package study persists per-user spaced-repetition reviews over the rules
// corpus, turning the existing FTS5 index into a question bank.
//
// The reviews table is a sibling of the rules docs and chat history in the same
// SQLite file. index.Store.Reset only clears the docs tables, so rebuilding the
// rules index never drops a review schedule — the concept keys are stable rule
// numbers, so a graded card keeps its place across a reindex.
package study

import "time"

// Grade is the reader's self-assessment of a single review, on the SM-2 quality
// scale. The four-button UI maps onto the classic 0–5 quality range: Again is a
// lapse (quality below the pass threshold), the others are passes of increasing
// ease.
type Grade int

const (
	// GradeAgain is a forgotten card: the schedule resets and the card comes
	// back within the session.
	GradeAgain Grade = 2
	// GradeHard is a correct but effortful recall: the ease drops and the next
	// interval grows slowly.
	GradeHard Grade = 3
	// GradeGood is a confident recall: the ease holds and the interval grows at
	// the current rate.
	GradeGood Grade = 4
	// GradeEasy is an instant recall: the ease rises and the next interval
	// grows quickly.
	GradeEasy Grade = 5
)

// ParseGrade maps a UI grade slug ("again".."easy") to its quality, returning
// ok=false for an unknown value so the handler can 400 rather than guess.
func ParseGrade(s string) (Grade, bool) {
	switch s {
	case "again":
		return GradeAgain, true
	case "hard":
		return GradeHard, true
	case "good":
		return GradeGood, true
	case "easy":
		return GradeEasy, true
	default:
		return 0, false
	}
}

// String returns the UI slug for a grade.
func (g Grade) String() string {
	switch g {
	case GradeAgain:
		return "again"
	case GradeHard:
		return "hard"
	case GradeGood:
		return "good"
	case GradeEasy:
		return "easy"
	default:
		return "unknown"
	}
}

// SM-2 constants. The algorithm is the published SuperMemo 2 schedule; the one
// deviation is that a lapsed card is marked due immediately rather than a day
// out, so "Again" resurfaces the card within the current session instead of
// burying it until tomorrow.
const (
	// defaultEase is the starting easiness factor. 2.5 is the SM-2 default.
	defaultEase = 2.5
	// minEase is the floor an easiness factor can sink to. Below this the
	// schedule collapses (cards never advance), so SM-2 clamps it.
	minEase = 1.3
	// passThreshold is the lowest quality that counts as a successful recall.
	// Below it the repetition count resets.
	passThreshold = 3
)

// schedule advances an item's review state under SM-2 for a graded recall. It
// is pure: the same inputs always produce the same outputs, so it is easy to
// exercise against the canonical SM-2 examples. The next due time is returned
// rather than read from a clock so callers (and tests) control "now".
func schedule(s SchedState, q Grade, now time.Time) SchedState {
	out := s
	if out.Ease == 0 {
		out.Ease = defaultEase
	}
	if int(q) < passThreshold {
		// A lapse: the card is forgotten, so the repetition count resets and
		// it is due again now (within this session) for relearning.
		out.Reps = 0
		out.Lapses++
		out.IntervalDays = 0
		out.DueAt = now
		out.Ease = clampEase(out.Ease + easeDelta(q))
		return out
	}

	out.Reps++
	switch out.Reps {
	case 1:
		out.IntervalDays = 1
	case 2:
		out.IntervalDays = 6
	default:
		out.IntervalDays = round(out.IntervalDays * out.Ease)
	}
	out.Ease = clampEase(out.Ease + easeDelta(q))
	out.DueAt = now.Add(time.Duration(out.IntervalDays * float64(24*time.Hour)))
	return out
}

// easeDelta is the SM-2 easiness adjustment for a quality of recall. Recall
// quality is on the 0–5 scale; the published formula penalizes low quality
// quadratically and rewards easy recall linearly.
func easeDelta(q Grade) float64 {
	n := 5.0 - float64(q)
	return 0.1 - n*(0.08+n*0.02)
}

func clampEase(e float64) float64 {
	if e < minEase {
		return minEase
	}
	return e
}

// round rounds to the nearest whole number of days. SM-2 intervals are whole
// days; the half-day comes only from the ease factor multiplication.
func round(v float64) float64 {
	if v < 0 {
		return 0
	}
	whole := float64(int64(v))
	if v-whole >= 0.5 {
		whole++
	}
	return whole
}
