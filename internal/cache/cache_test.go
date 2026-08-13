package cache

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func testStore(t *testing.T, ttl time.Duration) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cache.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	s, err := New(db, ttl)
	if err != nil {
		t.Fatalf("new cache store: %v", err)
	}
	return s
}

func TestPutAndGetRoundTrip(t *testing.T) {
	s := testStore(t, DefaultTTL)
	ctx := context.Background()

	sources := json.RawMessage(`[{"number":"702.2"}]`)
	cards := json.RawMessage(`[{"name":"Lightning Bolt"}]`)
	if err := s.Put(ctx, "k1", "mtg", "Deathtouch is lethal.", sources, cards); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, err := s.Get(ctx, "k1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("expected a hit, got nil")
	}
	if got.Answer != "Deathtouch is lethal." {
		t.Errorf("answer = %q", got.Answer)
	}
	if string(got.Sources) != string(sources) {
		t.Errorf("sources = %s, want %s", got.Sources, sources)
	}
	if string(got.Cards) != string(cards) {
		t.Errorf("cards = %s, want %s", got.Cards, cards)
	}
	if time.Since(got.CreatedAt) > 2*time.Second {
		t.Errorf("created_at looks stale: %v", got.CreatedAt)
	}
}

func TestGetMissReturnsNil(t *testing.T) {
	s := testStore(t, DefaultTTL)
	got, err := s.Get(context.Background(), "absent")
	if err != nil {
		t.Fatalf("get miss: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil entry on miss, got %+v", got)
	}
}

func TestGetTreatsExpiredAsMiss(t *testing.T) {
	s := testStore(t, DefaultTTL)
	ctx := context.Background()
	if err := s.Put(ctx, "k", "mtg", "old answer", nil, nil); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Age the row past the TTL so the next read treats it as expired.
	if _, err := s.db.Exec(`UPDATE answer_cache SET created_at = ? WHERE key = ?`,
		time.Now().Add(-(DefaultTTL + time.Hour)).UnixMilli(), "k"); err != nil {
		t.Fatalf("age row: %v", err)
	}
	got, err := s.Get(ctx, "k")
	if err != nil {
		t.Fatalf("get expired: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for an expired entry, got %+v", got)
	}
}

func TestPutRefreshesExistingEntry(t *testing.T) {
	s := testStore(t, DefaultTTL)
	ctx := context.Background()

	if err := s.Put(ctx, "k", "mtg", "first", nil, nil); err != nil {
		t.Fatalf("put first: %v", err)
	}
	if err := s.Put(ctx, "k", "mtg", "second", nil, nil); err != nil {
		t.Fatalf("put second: %v", err)
	}
	got, _ := s.Get(ctx, "k")
	if got.Answer != "second" {
		t.Errorf("answer = %q, want the refreshed value", got.Answer)
	}
}

func TestPutAcceptsNilCitations(t *testing.T) {
	s := testStore(t, DefaultTTL)
	ctx := context.Background()
	if err := s.Put(ctx, "k", "dnd", "no citations here", nil, nil); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, _ := s.Get(ctx, "k")
	if got.Sources != nil || got.Cards != nil {
		t.Errorf("nil citations should stay nil, got sources=%s cards=%s", got.Sources, got.Cards)
	}
}

func TestKeyIsOrderIndependentAndStable(t *testing.T) {
	q := "How does deathtouch work?"
	a := Key("mtg", q, []string{"702.2", "702.2b", "702.2a"})
	b := Key("mtg", q, []string{"702.2a", "702.2", "702.2b"})
	c := Key("mtg", q, []string{"702.2", "702.2a", "702.2b"})
	d := Key("mtg", q, []string{" 702.2 ", "", "702.2a", "702.2b"})
	if a != b || a != c || a != d {
		t.Errorf("key must be independent of source order/blank entries:\n a=%s\n b=%s\n c=%s\n d=%s", a, b, c, d)
	}
	// Deterministic across runs.
	if Key("mtg", q, []string{"702.2"}) != Key("mtg", q, []string{"702.2"}) {
		t.Error("key changed between identical calls")
	}
}

func TestKeyDistinguishesInputs(t *testing.T) {
	base := Key("mtg", "how does deathtouch work", []string{"702.2"})
	cases := map[string]string{
		"different corpus":    Key("dnd", "how does deathtouch work", []string{"702.2"}),
		"different question":  Key("mtg", "how does trample work", []string{"702.2"}),
		"different grounding": Key("mtg", "how does deathtouch work", []string{"702.2", "702.2a"}),
		"empty grounding":     Key("mtg", "how does deathtouch work", nil),
	}
	for name, k := range cases {
		if k == base {
			t.Errorf("key should change for %s, got identical key %s", name, k)
		}
	}
}

func TestKeyNormalizesQuestion(t *testing.T) {
	ids := []string{"702.2"}
	want := Key("mtg", "How does DEATHTOUCH work?", ids)
	for _, q := range []string{
		"how does deathtouch work?",
		"  How   does\tdeathtouch\nwork? ",
		"HOW DOES DEATHTOUCH WORK?",
	} {
		if got := Key("mtg", q, ids); got != want {
			t.Errorf("Key(%q) = %s, want normalized %s", q, got, want)
		}
	}
}

func TestNilTTLFallsBackToDefault(t *testing.T) {
	s := testStore(t, 0)
	if s.TTL() != DefaultTTL {
		t.Errorf("ttl = %v, want default %v", s.TTL(), DefaultTTL)
	}
}
