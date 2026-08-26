package story

// Pace: how many sessions an act needs, derived — not guessed — from the
// level band and the DMG XP tables internal/encounter already carries
// (encounter.ThresholdFor, the per-character thresholds by level and band).
//
// The model, pinned to those tables:
//
//   - The cost of leaving level L is the challenge the next tier sizes: the
//     Hard threshold at L+1, per character.
//   - A session at level L delivers, per character, the Medium threshold at
//     L — the share of one solid medium encounter. (Adjusted XP is a
//     party-level figure; a Medium encounter for that party splits into
//     exactly Medium(L) per character, which is why the per-character
//     thresholds are the right currency.)
//
// sessions(L -> L+1) = ceil(Hard(L+1) / Medium(L)), floored at 1. The ratio
// is not constant — the thresholds double through tier 1 and flatten later —
// which is the real pacing curve: early levels cost two to three sessions,
// late tiers settle near two. A 1-12 band across four acts lands on 26
// sessions (9/7/6/4), the half-year of weekly play a campaign like that
// actually takes.

import (
	"math"

	"github.com/madeofpendletonwool/grimoire/internal/encounter"
)

// ActPace is one act's slice of the band: the crossings it owns and how many
// sessions they cost.
type ActPace struct {
	Act        int `json:"act"`
	LevelStart int `json:"level_start"`
	LevelEnd   int `json:"level_end"`
	Sessions   int `json:"sessions"`
}

// Pacing is a whole band split across acts.
type Pacing struct {
	LevelStart    int       `json:"level_start"`
	LevelEnd      int       `json:"level_end"`
	ActCount      int       `json:"act_count"`
	TotalSessions int       `json:"total_sessions"`
	PerAct        []ActPace `json:"per_act"`
}

// Pace splits a level band across actCount acts and says how many sessions
// each needs. Levels are clamped to 1-20 and the band is ordered, so a DM
// who types "12 to 1" gets the same answer as "1 to 12". actCount below one
// is treated as one act. Each act is floored at one session — an act nobody
// plays is not an act.
func Pace(levelStart, levelEnd, actCount int) Pacing {
	if levelStart < 1 {
		levelStart = 1
	}
	if levelEnd > 20 {
		levelEnd = 20
	}
	if levelStart > levelEnd {
		levelStart, levelEnd = levelEnd, levelStart
	}
	if actCount < 1 {
		actCount = 1
	}

	crossings := levelCrossings(levelStart, levelEnd)
	p := Pacing{LevelStart: levelStart, LevelEnd: levelEnd, ActCount: actCount}
	p.PerAct = make([]ActPace, actCount)

	// Spread the crossings as evenly as integer slices allow, in order:
	// act i owns crossings [i*n/actCount, (i+1)*n/actCount).
	n := len(crossings)
	for i := 0; i < actCount; i++ {
		lo := i * n / actCount
		hi := (i + 1) * n / actCount
		ap := ActPace{Act: i + 1, LevelStart: levelStart, LevelEnd: levelStart}
		if hi > lo {
			ap.LevelStart = crossings[lo].from
			ap.LevelEnd = crossings[hi-1].to
			for _, c := range crossings[lo:hi] {
				ap.Sessions += c.sessions
			}
		}
		if ap.Sessions < 1 {
			ap.Sessions = 1 // an empty slice of band still costs a session
		}
		p.PerAct[i] = ap
		p.TotalSessions += ap.Sessions
	}
	return p
}

// crossing is one level boundary: the sessions it costs to leave `from`.
type crossing struct {
	from, to int
	sessions int
}

// levelCrossings prices every boundary in the band off the encounter
// package's tables: ceil(Hard(next) / Medium(current)), floored at 1.
func levelCrossings(levelStart, levelEnd int) []crossing {
	var out []crossing
	for lvl := levelStart; lvl < levelEnd; lvl++ {
		medium, _ := encounter.ThresholdFor(lvl, encounter.BandMedium)
		hard, _ := encounter.ThresholdFor(lvl+1, encounter.BandHard)
		sessions := 1
		if medium > 0 && hard > medium {
			sessions = int(math.Ceil(float64(hard) / float64(medium)))
		}
		out = append(out, crossing{from: lvl, to: lvl + 1, sessions: sessions})
	}
	return out
}

// SessionsToLevel prices a single level crossing on its own — the unit both
// Pace and the planner's readout are built from.
func SessionsToLevel(level int) int {
	for _, c := range levelCrossings(level, level+1) {
		return c.sessions
	}
	return 1
}
