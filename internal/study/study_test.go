package study

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "study.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	s, err := New(db)
	if err != nil {
		t.Fatalf("new study store: %v", err)
	}
	return s
}

// stubGen is a deterministic Generator for exercising the store without the
// index. Concepts come back in declaration order.
type stubGen struct {
	concepts []Concept
}

func (g stubGen) Concepts(_ context.Context, corpus, topic string) ([]Concept, error) {
	var out []Concept
	for _, c := range g.concepts {
		if c.Corpus == corpus && c.Topic == topic {
			out = append(out, c)
		}
	}
	return out, nil
}

func deck(corpus, topic string, keys ...string) []Concept {
	out := make([]Concept, 0, len(keys))
	for _, k := range keys {
		out = append(out, Concept{
			Key: k, Corpus: corpus, Topic: topic, Title: k,
			Front: k + " front", Back: k + " back", Source: "test",
		})
	}
	return out
}

func TestQueueEmptyDeck(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	gen := stubGen{}
	if _, err := s.Queue(ctx, "u", "mtg", "keyword-abilities", 10, gen); !errors.Is(err, ErrDeckEmpty) {
		t.Errorf("err = %v, want ErrDeckEmpty", err)
	}
}

func TestQueueAllNewOnFirstSession(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	gen := stubGen{deck("mtg", "keyword-abilities", "702.2", "702.3", "702.4")}
	cards, err := s.Queue(ctx, "u", "mtg", "keyword-abilities", 10, gen)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	if len(cards) != 3 {
		t.Fatalf("got %d cards, want 3", len(cards))
	}
	for _, c := range cards {
		if !c.New {
			t.Errorf("card %q should be new on first session", c.Key)
		}
		if c.Front == "" || c.Back == "" {
			t.Errorf("card %q missing content", c.Key)
		}
	}
	// New cards come back in concept order so a session reads like the rulebook.
	if cards[0].Key != "702.2" || cards[2].Key != "702.4" {
		t.Errorf("order = %v %v %v, want rulebook order", cards[0].Key, cards[1].Key, cards[2].Key)
	}
}

func TestQueueRespectsLimit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	gen := stubGen{deck("mtg", "keyword-abilities", "1", "2", "3", "4", "5")}
	cards, err := s.Queue(ctx, "u", "mtg", "keyword-abilities", 2, gen)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	if len(cards) != 2 {
		t.Errorf("got %d cards, want 2 (limit)", len(cards))
	}
}

func TestQueueDueCardsLead(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	gen := stubGen{deck("mtg", "keyword-abilities", "1", "2", "3")}

	// Grade "1" as Again so it is due immediately (within the relearn window),
	// then build a queue large enough to include it and the new cards.
	if _, err := s.Grade(ctx, "u", gen.concepts[0], GradeAgain); err != nil {
		t.Fatalf("grade: %v", err)
	}
	cards, err := s.Queue(ctx, "u", "mtg", "keyword-abilities", 10, gen)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	if len(cards) == 0 {
		t.Fatal("expected cards, got none")
	}
	if cards[0].Key != "1" {
		t.Errorf("due card should lead, got %q", cards[0].Key)
	}
	if cards[0].New {
		t.Errorf("graded card should not be new")
	}
}

func TestQueueScopedToUser(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	gen := stubGen{deck("mtg", "keyword-abilities", "1", "2")}

	// alice grades everything; bob's queue should be untouched.
	if _, err := s.Grade(ctx, "alice", gen.concepts[0], GradeGood); err != nil {
		t.Fatalf("alice grade: %v", err)
	}
	bobCards, err := s.Queue(ctx, "bob", "mtg", "keyword-abilities", 10, gen)
	if err != nil {
		t.Fatalf("bob queue: %v", err)
	}
	for _, c := range bobCards {
		if !c.New {
			t.Errorf("bob should see only new cards, but %q has reps=%d", c.Key, c.Reps)
		}
	}
}

func TestGradePersistsAcrossInstances(t *testing.T) {
	// Reviews must survive a reload (a new Store on the same DB), since the
	// acceptance criterion is per-user persistence.
	path := filepath.Join(t.TempDir(), "study.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)

	s1, err := New(db)
	if err != nil {
		t.Fatalf("s1: %v", err)
	}
	c := Concept{Key: "702.2", Corpus: "mtg", Topic: "keyword-abilities", Title: "Deathtouch"}
	card, err := s1.Grade(context.Background(), "u", c, GradeGood)
	if err != nil {
		t.Fatalf("grade: %v", err)
	}
	if card.Reps != 1 {
		t.Errorf("reps = %d, want 1 after first pass", card.Reps)
	}

	// A fresh store on the same DB should see the schedule.
	s2, err := New(db)
	if err != nil {
		t.Fatalf("s2: %v", err)
	}
	st, err := s2.state(context.Background(), "u", "702.2")
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if st.Reps != 1 {
		t.Errorf("after reload, reps = %d, want 1", st.Reps)
	}
	if st.Ease != defaultEase {
		t.Errorf("after reload, ease = %v, want default %v", st.Ease, defaultEase)
	}
}

func TestStatsReportsProgress(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	concepts := deck("mtg", "keyword-abilities", "1", "2", "3", "4")
	gen := stubGen{concepts: concepts}

	stats, err := s.Stats(ctx, "u", "mtg", "keyword-abilities", gen)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Total != 4 || stats.New != 4 || stats.Due != 0 || stats.Learned != 0 {
		t.Errorf("fresh stats = %+v, want 4 total / 4 new", stats)
	}

	// Grade two as Good (learned, scheduled into the future) and one as Again
	// (lapsed, due now).
	if _, err := s.Grade(ctx, "u", concepts[0], GradeGood); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Grade(ctx, "u", concepts[1], GradeGood); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Grade(ctx, "u", concepts[2], GradeAgain); err != nil {
		t.Fatal(err)
	}
	stats, err = s.Stats(ctx, "u", "mtg", "keyword-abilities", gen)
	if err != nil {
		t.Fatalf("stats after grading: %v", err)
	}
	if stats.New != 1 {
		t.Errorf("new = %d, want 1", stats.New)
	}
	if stats.Learned != 2 {
		t.Errorf("learned = %d, want 2 (only the Good passes)", stats.Learned)
	}
	if stats.Due != 1 {
		t.Errorf("due = %d, want 1 (the Again lapse)", stats.Due)
	}
}

func TestParseGrade(t *testing.T) {
	tests := []struct {
		in   string
		want Grade
		ok   bool
	}{
		{"again", GradeAgain, true},
		{"hard", GradeHard, true},
		{"good", GradeGood, true},
		{"easy", GradeEasy, true},
		{"AGAIN", GradeAgain, false}, // case-sensitive, like the UI slugs
		{"", 0, false},
		{"nope", 0, false},
	}
	for _, tt := range tests {
		g, ok := ParseGrade(tt.in)
		if ok != tt.ok || (ok && g != tt.want) {
			t.Errorf("ParseGrade(%q) = %v, %v; want %v, %v", tt.in, g, ok, tt.want, tt.ok)
		}
	}
}

func TestTopicFor(t *testing.T) {
	tests := []struct {
		corpus, topic, want string
	}{
		{"mtg", "", TopicKeywordAbilities},
		{"dnd", "", TopicConditions},
		{"mtg", "conditions", "conditions"}, // explicit topic wins
		{"unknown", "", TopicKeywordAbilities},
	}
	for _, tt := range tests {
		if got := TopicFor(tt.corpus, tt.topic); got != tt.want {
			t.Errorf("TopicFor(%q, %q) = %q, want %q", tt.corpus, tt.topic, got, tt.want)
		}
	}
}

func TestScheduleAgainLapseResetsAndReturnsSoon(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// A card two reps in.
	st := SchedState{Reps: 2, IntervalDays: 6, Ease: defaultEase, DueAt: now}
	next := schedule(st, GradeAgain, now)
	if next.Reps != 0 {
		t.Errorf("reps = %d, want 0 after lapse", next.Reps)
	}
	if next.Lapses != 1 {
		t.Errorf("lapses = %d, want 1", next.Lapses)
	}
	// A lapse is due immediately so it resurfaces in the current session.
	if !next.DueAt.Equal(now) {
		t.Errorf("lapse due at %v, want %v (now)", next.DueAt, now)
	}
}

func TestScheduleGoodAdvances(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// First pass: reps 0 -> 1, interval 1 day.
	next := schedule(SchedState{}, GradeGood, now)
	if next.Reps != 1 || next.IntervalDays != 1 {
		t.Errorf("first good: reps=%d interval=%v, want 1 / 1", next.Reps, next.IntervalDays)
	}
	if !next.DueAt.Equal(now.Add(24 * time.Hour)) {
		t.Errorf("first good due at %v, want +1d", next.DueAt)
	}
	// Second pass: interval 6 days.
	next = schedule(next, GradeGood, now)
	if next.Reps != 2 || next.IntervalDays != 6 {
		t.Errorf("second good: reps=%d interval=%v, want 2 / 6", next.Reps, next.IntervalDays)
	}
	// Third pass: interval = round(prev * ease) = round(6 * 2.5) = 15.
	next = schedule(next, GradeGood, now)
	if next.Reps != 3 || next.IntervalDays != 15 {
		t.Errorf("third good: reps=%d interval=%v, want 3 / 15", next.Reps, next.IntervalDays)
	}
	// Good holds the ease steady.
	if next.Ease != defaultEase {
		t.Errorf("ease drifted to %v, want %v", next.Ease, defaultEase)
	}
}

func TestScheduleHardDropsEase(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	next := schedule(SchedState{Reps: 2, IntervalDays: 6, Ease: defaultEase, DueAt: now}, GradeHard, now)
	if next.Ease >= defaultEase {
		t.Errorf("hard should drop ease, got %v", next.Ease)
	}
	if next.Ease < minEase {
		t.Errorf("ease below floor: %v", next.Ease)
	}
}

func TestScheduleEasyRaisesEase(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	next := schedule(SchedState{Reps: 2, IntervalDays: 6, Ease: defaultEase, DueAt: now}, GradeEasy, now)
	if next.Ease != defaultEase+0.1 {
		t.Errorf("easy should raise ease by 0.1, got %v", next.Ease)
	}
}

func TestScheduleEaseFloor(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Hammer Again repeatedly: ease must not fall below minEase.
	st := SchedState{Ease: minEase}
	for i := 0; i < 5; i++ {
		st = schedule(st, GradeAgain, now)
	}
	if st.Ease < minEase {
		t.Errorf("ease = %v, want >= floor %v", st.Ease, minEase)
	}
}
