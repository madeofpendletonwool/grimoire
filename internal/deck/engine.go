package deck

import (
	"context"
	"sort"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/carddb"
)

// The synergy engine: given a commander (or bare colors) and theme terms,
// produce explainable card candidates from the local card database. Ranking
// blends EDHREC popularity (the prior) with FTS relevance against the theme
// terms (the local synergy signal), and every suggestion carries the signals
// that produced it so the UI can say why.

// Candidate is one suggested card with the ranking signals behind it.
type Candidate struct {
	*carddb.Card
	Reasons []string `json:"reasons"`
	Score   float64  `json:"-"`
}

// CandidateSource is the card-database surface the engine needs. Satisfied
// by *carddb.Store; an interface so tests run on fixtures.
type CandidateSource interface {
	Candidates(ctx context.Context, allowedMask int, terms []string, exclude map[string]bool, limit int) ([]*carddb.Card, error)
}

// Engine ranks candidates for a deck build.
type Engine struct {
	db CandidateSource
}

// NewEngine builds an engine over a card database.
func NewEngine(db CandidateSource) *Engine { return &Engine{db: db} }

// ThemeTerms extracts usable search terms from a free-text idea: lowercase
// alphanumerics, filler and MTG jargon dropped.
func ThemeTerms(idea string) []string {
	var out []string
	for _, tok := range strings.FieldsFunc(strings.ToLower(idea), func(r rune) bool {
		return !('a' <= r && r <= 'z' || '0' <= r && r <= '9')
	}) {
		if len(tok) < 3 || themeStopword(tok) {
			continue
		}
		out = append(out, tok)
	}
	return out
}

func themeStopword(tok string) bool { return themeStopwords[tok] }

var themeStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "that": true,
	"want": true, "like": true, "budget": true, "deck": true, "decks": true,
	"commander": true, "edh": true, "build": true, "make": true, "builds": true,
	"mid": true, "high": true, "low": true, "cheap": true, "expensive": true,
	"powerful": true, "good": true, "great": true, "fun": true, "jank": true,
	"competitive": true, "casual": true, "aggro": true, "new": true,
}

// Roles are the buckets a Commander draft fills. The engine proposes
// candidates per role so the model picks from a balanced menu rather than a
// single blended list; role detection is keyword-based over oracle text and
// type line — deterministic and explainable.
type Role struct {
	Key   string
	Label string
	Terms []string
}

// Roles returns the draft roles with their search terms.
func Roles() []Role {
	return []Role{
		{Key: "ramp", Label: "Ramp", Terms: []string{"add \\{", "mana", "search your library for a basic land"}},
		{Key: "draw", Label: "Card draw", Terms: []string{"draw", "investigate"}},
		{Key: "interaction", Label: "Interaction", Terms: []string{"destroy", "exile", "counter", "damage to", "sacrifice"}},
		{Key: "theme", Label: "Theme payoffs", Terms: nil}, // filled per idea
	}
}

// RoleOf classifies a card into a role key by text keywords. A card can carry
// several roles; the first matching wins for bucketing purposes.
func RoleOf(c *carddb.Card) string {
	t := strings.ToLower(c.TypeLine + " " + c.OracleText)
	switch {
	case strings.Contains(t, "search your library for") && strings.Contains(t, "land"),
		strings.Contains(t, "add {") && !strings.Contains(strings.ToLower(c.TypeLine), "land"):
		return "ramp"
	case strings.Contains(t, "draw a card"), strings.Contains(t, "draw two"), strings.Contains(t, "draw x"), strings.Contains(t, "investigate"):
		return "draw"
	case strings.Contains(t, "destroy target"), strings.Contains(t, "exile target"),
		strings.Contains(t, "counter target"), strings.Contains(t, "sacrifice"):
		return "interaction"
	}
	return "theme"
}

// BuildCandidates returns ranked candidates for a draft: theme candidates
// from the idea terms plus per-role staples, all inside the allowed color
// identity, deduplicated. excludeNames are already-drafted cards.
func (e *Engine) BuildCandidates(ctx context.Context, allowedMask int, idea string, excludeNames []string, perList int) []Candidate {
	exclude := map[string]bool{}
	for _, n := range excludeNames {
		exclude[strings.ToLower(n)] = true
	}
	if perList <= 0 {
		perList = 40
	}

	seen := map[string]bool{}
	var out []Candidate
	add := func(cards []*carddb.Card, reasons []string, weight float64) {
		for _, c := range cards {
			key := strings.ToLower(c.Name)
			if seen[key] || exclude[key] {
				continue
			}
			seen[key] = true
			score := popularityScore(c.EDHRECRank) * weight
			out = append(out, Candidate{Card: c, Reasons: reasons, Score: score})
		}
	}

	// Theme candidates: the idea's own terms against the FTS index.
	terms := ThemeTerms(idea)
	if len(terms) > 0 {
		cards, err := e.db.Candidates(ctx, allowedMask, terms, exclude, perList)
		if err == nil {
			add(cards, []string{"matches your idea"}, 1.5)
		}
	}
	// Role staples: format-wide popular cards filtered by identity.
	staples, err := e.db.Candidates(ctx, allowedMask, nil, exclude, perList*2)
	if err == nil {
		add(staples, []string{"format staple"}, 1.0)
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > perList*4 {
		out = out[:perList*4]
	}
	return out
}

// popularityScore converts an EDHREC rank (1 = most popular) to a descending
// score. Unranked cards sink below every ranked one but keep a small floor so
// a themed match can still surface.
func popularityScore(rank int) float64 {
	if rank <= 0 {
		return 0.01
	}
	return 1000 / float64(rank+10)
}

// SynergyStat is one card's per-commander EDHREC numbers, as the enrichment
// layer hands them to the engine. Declared here so the deck package does not
// depend on the edhrec client package.
type SynergyStat struct {
	Synergy  float64
	NumDecks int
}

// BoostByStats re-ranks candidates using EDHREC per-commander stats: a card
// with a high synergy float in this commander's decks climbs. Pure function,
// applied by the server when enrichment data is available.
func BoostByStats(cands []Candidate, stats map[string]SynergyStat) {
	for i := range cands {
		if st, ok := stats[strings.ToLower(cands[i].Name)]; ok {
			cands[i].Score += st.Synergy * 500
			if st.Synergy > 0.25 {
				cands[i].Reasons = append(cands[i].Reasons, "high synergy in this commander's decks")
			}
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].Score > cands[j].Score })
}
