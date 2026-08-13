package study

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Concept is the content half of a flashcard — what it asks and what it answers
// — generated from the rules index. It carries no per-user state; the schedule
// lives in SchedState. Splitting content from state lets the generator run over
// the corpus while the store owns each user's progress independently.
type Concept struct {
	Key    string // stable concept id (e.g. a rule number "702.2")
	Corpus string // corpus slug the concept belongs to
	Topic  string // deck slug within the corpus (e.g. "keyword-abilities")
	Front  string // the prompt
	Back   string // the answer / full rule text
	Source string // attribution shown under the card
	Number string // rule number for citation, when the corpus numbers them
	Title  string // short label (the keyword name, condition name, ...)
}

// SchedState is the per-user SM-2 schedule for one concept. A concept the user
// has never seen has a zero value except for the defaults applied in schedule.
type SchedState struct {
	Reps         int
	Lapses       int
	IntervalDays float64
	Ease         float64
	DueAt        time.Time
}

// Card is a concept joined with the user's schedule, ready to present or grade.
type Card struct {
	Concept
	SchedState
	// New reports whether the user has never seen the card. A new card is due
	// immediately; grading it plants the first schedule entry.
	New bool `json:"new"`
}

// Stats summarizes a deck for the study UI.
type Stats struct {
	Total   int `json:"total"`
	New     int `json:"new"`
	Due     int `json:"due"`
	Learned int `json:"learned"`
}

// Generator builds a deck's concept list from the rules index. It is satisfied
// by an adapter over *index.Store so the study package stays free of the index
// dependency — the store owns schedules, the generator owns content.
type Generator interface {
	Concepts(ctx context.Context, corpus, topic string) ([]Concept, error)
}

// ErrDeckEmpty is returned when a study queue is requested for a corpus/topic
// the generator has no concepts for. It is a user-facing "nothing to study"
// signal rather than a server error.
var ErrDeckEmpty = errors.New("no cards available for this deck")

// Store persists per-user review schedules in the shared SQLite database.
type Store struct {
	db *sql.DB
}

// New builds a study store on an existing database handle and ensures its
// schema exists. The reviews table is a sibling of the docs and chat tables;
// index.Store.Reset leaves it alone, so a reindex never drops a schedule.
func New(db *sql.DB) (*Store, error) {
	s := &Store{db: db}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("study migrate: %w", err)
	}
	return s, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS reviews (
	user_id      TEXT NOT NULL,
	concept_key  TEXT NOT NULL,
	corpus       TEXT NOT NULL,
	topic        TEXT NOT NULL,
	reps         INTEGER NOT NULL DEFAULT 0,
	lapses       INTEGER NOT NULL DEFAULT 0,
	interval_days REAL NOT NULL DEFAULT 0,
	ease         REAL NOT NULL DEFAULT 2.5,
	due_at       INTEGER NOT NULL,
	updated_at   INTEGER NOT NULL,
	PRIMARY KEY (user_id, concept_key)
);
CREATE INDEX IF NOT EXISTS reviews_deck ON reviews(user_id, corpus, topic, due_at);
`

// Queue returns the cards due for review now, plus enough unseen cards to fill a
// session up to limit. Due cards lead (most overdue first), then new cards in
// concept order, so a session always opens on the cards the reader most needs.
func (s *Store) Queue(ctx context.Context, userID, corpus, topic string, limit int, gen Generator) ([]Card, error) {
	concepts, err := gen.Concepts(ctx, corpus, topic)
	if err != nil {
		return nil, err
	}
	if len(concepts) == 0 {
		return nil, ErrDeckEmpty
	}
	if limit <= 0 {
		limit = defaultSessionSize
	}

	states, err := s.states(ctx, userID, corpus, topic)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()

	cards := make([]Card, 0, len(concepts))
	for _, c := range concepts {
		st, seen := states[c.Key]
		card := Card{Concept: c, SchedState: st, New: !seen}
		if !seen {
			// A brand-new card is due immediately.
			card.DueAt = now
		}
		cards = append(cards, card)
	}

	var due, fresh, later []Card
	for _, c := range cards {
		switch {
		case c.New:
			fresh = append(fresh, c)
		case !c.DueAt.After(now):
			due = append(due, c)
		default:
			later = append(later, c)
		}
	}
	// Most overdue first so a long-lapsed card doesn't linger behind fresher
	// ones; ties break by concept key for stable ordering.
	sort.SliceStable(due, func(i, j int) bool {
		if !due[i].DueAt.Equal(due[j].DueAt) {
			return due[i].DueAt.Before(due[j].DueAt)
		}
		return due[i].Key < due[j].Key
	})
	sort.SliceStable(fresh, func(i, j int) bool { return fresh[i].Key < fresh[j].Key })

	out := append([]Card{}, due...)
	if len(out) < limit {
		out = append(out, fresh[:min(len(fresh), limit-len(out))]...)
	}
	// Fill the session with scheduled-but-not-due cards only when there is no
	// due or new work at all — a session that mixes in future reviews would
	// front-load the schedule and defeat the spacing.
	if len(out) == 0 && len(later) > 0 {
		sort.SliceStable(later, func(i, j int) bool { return later[i].DueAt.Before(later[j].DueAt) })
		out = append(out, later[:min(len(later), limit)]...)
	}
	return out, nil
}

// Grade records a review for a concept and returns the rescheduled card. The
// concept content is supplied by the caller (the handler has it in hand from the
// queue), so the store never has to re-run the generator on the write path.
func (s *Store) Grade(ctx context.Context, userID string, c Concept, g Grade) (Card, error) {
	st, err := s.state(ctx, userID, c.Key)
	if err != nil {
		return Card{}, err
	}
	next := schedule(st, g, time.Now().UTC())
	if err := s.save(ctx, userID, c, next); err != nil {
		return Card{}, err
	}
	return Card{Concept: c, SchedState: next, New: false}, nil
}

// Stats summarizes a deck's progress for the study UI.
func (s *Store) Stats(ctx context.Context, userID, corpus, topic string, gen Generator) (Stats, error) {
	concepts, err := gen.Concepts(ctx, corpus, topic)
	if err != nil {
		return Stats{}, err
	}
	if len(concepts) == 0 {
		return Stats{}, nil
	}
	states, err := s.states(ctx, userID, corpus, topic)
	if err != nil {
		return Stats{}, err
	}
	now := time.Now().UTC()
	var stats Stats
	stats.Total = len(concepts)
	for _, c := range concepts {
		st, seen := states[c.Key]
		if !seen {
			stats.New++
			continue
		}
		if !st.DueAt.After(now) {
			stats.Due++
		}
		if st.Reps > 0 {
			stats.Learned++
		}
	}
	return stats, nil
}

// Reset clears every review for a user. It exists for tests and for a future
// "reset my progress" affordance; production requests do not call it.
func (s *Store) Reset(ctx context.Context, userID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM reviews WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("reset reviews: %w", err)
	}
	return nil
}

// defaultSessionSize is the queue length when a caller does not say otherwise.
// A study session of ~20 cards is short enough to finish and long enough to
// move a schedule.
const defaultSessionSize = 20

// states loads the schedule for every concept the user has touched in a deck.
func (s *Store) states(ctx context.Context, userID, corpus, topic string) (map[string]SchedState, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT concept_key, reps, lapses, interval_days, ease, due_at
		  FROM reviews
		 WHERE user_id = ? AND corpus = ? AND topic = ?`, userID, corpus, topic)
	if err != nil {
		return nil, fmt.Errorf("load reviews: %w", err)
	}
	defer rows.Close()

	out := make(map[string]SchedState)
	for rows.Next() {
		var (
			key      string
			dueMilli int64
			st       SchedState
		)
		if err := rows.Scan(&key, &st.Reps, &st.Lapses, &st.IntervalDays, &st.Ease, &dueMilli); err != nil {
			return nil, err
		}
		st.DueAt = time.UnixMilli(dueMilli).UTC()
		out[key] = st
	}
	return out, rows.Err()
}

// state loads the schedule for a single concept. A never-seen concept returns a
// zero SchedState and no error; schedule applies the defaults.
func (s *Store) state(ctx context.Context, userID, key string) (SchedState, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT reps, lapses, interval_days, ease, due_at
		  FROM reviews WHERE user_id = ? AND concept_key = ?`, userID, key)
	var (
		dueMilli int64
		st       SchedState
	)
	if err := row.Scan(&st.Reps, &st.Lapses, &st.IntervalDays, &st.Ease, &dueMilli); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SchedState{}, nil
		}
		return SchedState{}, fmt.Errorf("load review: %w", err)
	}
	st.DueAt = time.UnixMilli(dueMilli).UTC()
	return st, nil
}

func (s *Store) save(ctx context.Context, userID string, c Concept, st SchedState) error {
	now := time.Now().UTC().UnixMilli()
	if _, err := s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO reviews
			(user_id, concept_key, corpus, topic, reps, lapses, interval_days, ease, due_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		userID, c.Key, c.Corpus, c.Topic, st.Reps, st.Lapses, st.IntervalDays, st.Ease, st.DueAt.UnixMilli(), now); err != nil {
		return fmt.Errorf("save review: %w", err)
	}
	return nil
}

// TopicFor returns the default deck slug for a corpus — the corpus picker in the
// UI selects a deck, so a request that omits the topic still lands somewhere
// useful. An explicit topic always wins.
func TopicFor(corpus, topic string) string {
	if t := strings.TrimSpace(topic); t != "" {
		return t
	}
	switch strings.ToLower(corpus) {
	case "dnd":
		return TopicConditions
	default:
		return TopicKeywordAbilities
	}
}

// Deck slugs. The generator recognizes these; an unknown topic yields an empty
// deck (ErrDeckEmpty at the queue), so the UI can offer a corpus's default
// without a separate topic-discovery endpoint.
const (
	TopicKeywordAbilities = "keyword-abilities" // MTG chapter 702
	TopicConditions       = "conditions"        // D&D SRD conditions
)
