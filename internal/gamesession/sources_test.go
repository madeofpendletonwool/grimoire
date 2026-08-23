package gamesession

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func TestAddSourceChecksumsVerbatimContent(t *testing.T) {
	s, cid := seeded(t)
	ses := addSession(t, s, cid, "one")
	content := "DM: The door grinds open.\nMira: I check for traps."
	src := addSource(t, s, ses.ID, SourceTranscript, content)

	sum := sha256.Sum256([]byte(content))
	if src.Checksum != hex.EncodeToString(sum[:]) {
		t.Fatalf("checksum = %s; want %s", src.Checksum, hex.EncodeToString(sum[:]))
	}
	if src.ByteSize != int64(len(content)) {
		t.Fatalf("byte size = %d; want %d", src.ByteSize, len(content))
	}

	got, err := s.GetSource(context.Background(), src.ID)
	if err != nil {
		t.Fatalf("get source: %v", err)
	}
	if got.Content != content {
		t.Fatalf("content not verbatim: %q", got.Content)
	}
	if got.Campaign != cid {
		t.Fatalf("campaign = %s; want %s", got.Campaign, cid)
	}
}

func TestAddSourceRejectsBadKindAndEmpty(t *testing.T) {
	s, cid := seeded(t)
	ses := addSession(t, s, cid, "one")
	if _, err := s.AddSource(context.Background(), ses.ID, "podcast", "", "", "x", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("kind err = %v; want ErrInvalid", err)
	}
	if _, err := s.AddSource(context.Background(), ses.ID, SourceTranscript, "", "", "   ", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty err = %v; want ErrInvalid", err)
	}
}

func TestListSourcesFiltersDMOnlyKinds(t *testing.T) {
	s, cid := seeded(t)
	ses := addSession(t, s, cid, "one")
	addSource(t, s, ses.ID, SourceTranscript, "shared")
	addSource(t, s, ses.ID, SourceDMNotes, "the vampire secret")
	addSource(t, s, ses.ID, SourceLiveMark, "mid-session mark")
	addSource(t, s, ses.ID, SourcePlayerJournal, "Mira's journal")

	dmView, err := s.ListSources(context.Background(), ses.ID, true)
	if err != nil {
		t.Fatalf("dm list: %v", err)
	}
	if len(dmView) != 4 {
		t.Fatalf("dm sees %d sources; want 4", len(dmView))
	}

	playerView, err := s.ListSources(context.Background(), ses.ID, false)
	if err != nil {
		t.Fatalf("player list: %v", err)
	}
	if len(playerView) != 2 {
		t.Fatalf("player sees %d sources; want 2", len(playerView))
	}
	for _, src := range playerView {
		if DMOnlySources[src.Kind] {
			t.Errorf("player list leaked %q source", src.Kind)
		}
	}
}

func TestSourceTimingRoundTrip(t *testing.T) {
	s, cid := seeded(t)
	ses := addSession(t, s, cid, "one")
	cues := []Cue{{StartMS: 0, EndMS: 4000, Text: "hello"}, {StartMS: 5000, EndMS: 8000, Text: "again"}}
	src, err := s.AddSource(context.Background(), ses.ID, SourceTranscript, "DM", "", "hello\n\nagain", cues)
	if err != nil {
		t.Fatalf("add timed source: %v", err)
	}
	got, err := s.GetSource(context.Background(), src.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Timing) != 2 || got.Timing[1].Text != "again" || got.Timing[1].EndMS != 8000 {
		t.Fatalf("timing = %+v; want two cues intact", got.Timing)
	}
}

/* ---------- spans ---------- */

func TestResolveSpanRoundTrip(t *testing.T) {
	s, cid := seeded(t)
	ses := addSession(t, s, cid, "one")
	content := "DM: The door grinds open.\nMira: I check for traps."
	src := addSource(t, s, ses.ID, SourceTranscript, content)

	// Locate a quote someone might cite, then resolve the offsets back.
	quote := "I check for traps"
	span, ok := Locate(content, quote)
	if !ok {
		t.Fatal("locate failed on a verbatim quote")
	}
	resolved, err := s.ResolveSpan(context.Background(), src.ID, span.Start, span.End)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Quote != quote {
		t.Fatalf("round trip = %q; want %q", resolved.Quote, quote)
	}
	if resolved.SourceID != src.ID {
		t.Fatalf("source id = %s; want %s", resolved.SourceID, src.ID)
	}
}

func TestResolveSpanRejectsOutOfBounds(t *testing.T) {
	s, cid := seeded(t)
	ses := addSession(t, s, cid, "one")
	src := addSource(t, s, ses.ID, SourceTranscript, "short")
	cases := []struct {
		name       string
		start, end int64
	}{
		{"negative", -1, 3},
		{"empty", 2, 2},
		{"inverted", 3, 2},
		{"past end", 1, 100},
	}
	for _, tc := range cases {
		if _, err := s.ResolveSpan(context.Background(), src.ID, tc.start, tc.end); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v; want ErrInvalid", tc.name, err)
		}
	}
}

func TestResolveSpanIsByteAddressed(t *testing.T) {
	s, cid := seeded(t)
	ses := addSession(t, s, cid, "one")
	// "é" is two bytes in UTF-8; the span offsets must be byte offsets into
	// the exact stored string, not rune indexes.
	content := "Mira lançou a magia"
	src := addSource(t, s, ses.ID, SourceTranscript, content)
	quote := "lançou"
	span, ok := Locate(content, quote)
	if !ok {
		t.Fatal("locate failed")
	}
	if int(span.End-span.Start) != len(quote) {
		t.Fatalf("span width = %d; want %d bytes", span.End-span.Start, len(quote))
	}
	resolved, err := s.ResolveSpan(context.Background(), src.ID, span.Start, span.End)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Quote != quote {
		t.Fatalf("quote = %q; want %q", resolved.Quote, quote)
	}
}

func TestLocateSpanRejectsInventedQuote(t *testing.T) {
	s, cid := seeded(t)
	ses := addSession(t, s, cid, "one")
	src := addSource(t, s, ses.ID, SourceTranscript, "what was said")
	paraphrase := "what was kinda said"
	if _, err := s.LocateSpan(context.Background(), src.ID, paraphrase); !errors.Is(err, ErrNotFound) {
		t.Fatalf("paraphrase err = %v; want ErrNotFound", err)
	}
}

func TestLocateHandlesMultilineQuote(t *testing.T) {
	content := "one\ntwo\nthree"
	span, ok := Locate(content, "two\nthree")
	if !ok || span.Quote != "two\nthree" {
		t.Fatalf("multiline locate = %+v ok=%v", span, ok)
	}
	if !strings.HasPrefix("x"+content[span.Start:span.End], "xtwo") {
		t.Fatal("offsets do not address the quote")
	}
}
