package gamesession

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestAddEventOrdersByPlayOrder(t *testing.T) {
	s, cid := seeded(t)
	ses := addSession(t, s, cid, "one")
	addEvent(t, s, ses.ID, EventNote, "", "opened the door", nil)
	addEvent(t, s, ses.ID, EventDiscovery, "the cult sigil", "", nil)
	addEvent(t, s, ses.ID, EventEncounter, "the crypt fight", "six skeletons", map[string]any{"cr": "1/2"})

	got, err := s.ListEvents(context.Background(), ses.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("events = %d; want 3", len(got))
	}
	for i, ev := range got {
		if ev.Seq != int64(i+1) {
			t.Fatalf("seq[%d] = %d; want %d", i, ev.Seq, i+1)
		}
	}
	if got[2].Payload["cr"] != "1/2" {
		t.Fatalf("payload = %v", got[2].Payload)
	}
	if got[2].Campaign != cid {
		t.Fatalf("campaign = %s; want %s", got[2].Campaign, cid)
	}
}

func TestAddEventVocabAndRequiredSummary(t *testing.T) {
	s, cid := seeded(t)
	ses := addSession(t, s, cid, "one")
	if _, err := s.AddEvent(context.Background(), ses.ID, "boss_kill", "", "", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("kind err = %v", err)
	}
	for _, kind := range []string{EventQA, EventRuling} {
		if _, err := s.AddEvent(context.Background(), ses.ID, kind, "  ", "", nil); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s without summary: err = %v; want ErrInvalid", kind, err)
		}
	}
	// Notes and discoveries may carry their content in detail/payload with
	// no separate summary line.
	if _, err := s.AddEvent(context.Background(), ses.ID, EventNote, "", "just a note", nil); err != nil {
		t.Errorf("bare note: %v", err)
	}
}

/* ---------- prior-ruling surfacing (MAD-286, retained) ---------- */

// seedRulings plants the fixture the matcher is tested against: one campaign
// with three sessions, rulings spread across them, a Q&A that shares words
// with a ruling (but is not precedent), and a second campaign with a ruling
// that must never surface for the first.
func seedRulings(t *testing.T) (*Store, string, string) {
	t.Helper()
	s, cid := seeded(t)
	if _, err := s.db.Exec(
		`INSERT INTO campaigns (id, owner_id, name, created_at, updated_at) VALUES ('camp2', 'keeper', 'Other campaign', 0, 0)`); err != nil {
		t.Fatalf("second campaign: %v", err)
	}

	s1 := addSession(t, s, cid, "The Ambush")
	s2 := addSession(t, s, cid, "The Crypt")
	other := addSession(t, s, "camp2", "Someone else's game")

	addEvent(t, s, s1.ID, EventRuling,
		"Does hiding work in dim light?",
		"Ruled yes: dim light is lightly obscured, so Stealth has advantage-free play per PHB 177.", nil)
	addEvent(t, s, s2.ID, EventRuling,
		"Can you hide while in dim light after attacking?",
		"Ruled no: you need to take the Hide action again; attacking breaks hiding.", nil)
	addEvent(t, s, s2.ID, EventQA,
		"Does hiding work in dim light?",
		"Answered at the table: see the earlier ruling — yes.", nil)
	addEvent(t, s, other.ID, EventRuling,
		"Does hiding work in dim light?",
		"A different table, a different DM: ruled no here.", nil)
	return s, cid, s2.ID
}

func TestMatchPriorRulings(t *testing.T) {
	s, cid, s2 := seedRulings(t)

	got, err := s.MatchPriorRulings(context.Background(), cid,
		"does hiding work in dim light against the skeletons?", "", 5)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no prior rulings matched")
	}
	for _, m := range got {
		// Only rulings from the same campaign: not the Q&A echo, not the
		// other campaign's ruling.
		if m.EventID == "" || !strings.Contains(strings.ToLower(m.Summary), "hide") &&
			!strings.Contains(strings.ToLower(m.Summary), "hiding") {
			t.Errorf("weak match surfaced: %+v", m)
		}
	}
	if got[0].Ordinal != 1 && got[0].Ordinal != 2 {
		t.Fatalf("match from wrong session: %+v", got[0])
	}
	_ = s2
}

func TestMatchPriorRulingsStaysInCampaign(t *testing.T) {
	s, cid, _ := seedRulings(t)
	got, err := s.MatchPriorRulings(context.Background(), cid, "hiding in dim light", "", 10)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	for _, m := range got {
		if !strings.Contains(m.Session, "Ambush") && !strings.Contains(m.Session, "Crypt") {
			t.Errorf("cross-campaign leak: %+v", m)
		}
	}
	// The other campaign's own match works for its own question.
	got2, err := s.MatchPriorRulings(context.Background(), "camp2", "hiding in dim light", "", 5)
	if err != nil {
		t.Fatalf("match2: %v", err)
	}
	if len(got2) != 1 {
		t.Fatalf("camp2 matches = %d; want 1", len(got2))
	}
}

func TestMatchPriorRulingsMatchesRulingsNotQA(t *testing.T) {
	s, cid, s2 := seedRulings(t)
	// The Q&A event shares the exact question text with the first ruling;
	// only the ruling may come back.
	got, err := s.MatchPriorRulings(context.Background(), cid, "Does hiding work in dim light?", "", 10)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("matches = %d; want exactly the 2 rulings", len(got))
	}
	_ = s2
}

func TestMatchPriorRulingsExcludesSelfAndEmpty(t *testing.T) {
	s, cid, _ := seedRulings(t)
	got, err := s.MatchPriorRulings(context.Background(), cid, "Does hiding work in dim light?", "", 10)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("need a baseline match")
	}
	self := got[0]
	again, err := s.MatchPriorRulings(context.Background(), cid, "Does hiding work in dim light?", self.EventID, 10)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	for _, m := range again {
		if m.EventID == self.EventID {
			t.Error("excluded event came back")
		}
	}
	if none, err := s.MatchPriorRulings(context.Background(), cid, "   ", "", 5); err != nil || len(none) != 0 {
		t.Errorf("empty question = %v, %v; want nothing", none, err)
	}
	// FTS operators in the question are data, not query syntax: never an
	// error, never a parse.
	if _, err := s.MatchPriorRulings(context.Background(), cid, `hiding OR "NOT (" dim`, "", 5); err != nil {
		t.Errorf("operator-laced question errored: %v", err)
	}
}

func TestFTSQueryQuotesEverything(t *testing.T) {
	if got := ftsQuery(`Stealth (dim light)`); got != `"Stealth" OR "dim" OR "light"` {
		t.Fatalf("ftsQuery = %q", got)
	}
	if got := ftsQuery(`say "hi"`); got != `"say" OR "hi"` {
		t.Fatalf("embedded quotes = %q", got)
	}
	if got := ftsQuery(`   `); got != "" {
		t.Fatalf("blank = %q", got)
	}
}

/* ---------- export ---------- */

func TestExportMarkdownWholeSessionLog(t *testing.T) {
	s, cid, _ := seedRulings(t)
	ses, err := s.ListSessions(context.Background(), cid)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if _, err := s.AddSource(context.Background(), ses[0].ID, SourceTranscript, "DM", "the recording",
		"DM: The door grinds open.\nMira: I check for traps.", nil); err != nil {
		t.Fatalf("source: %v", err)
	}
	addEvent(t, s, ses[0].ID, EventNote, "", "exported for the group wiki", nil)

	md, err := s.ExportMarkdown(context.Background(), ses[0].ID)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	for _, want := range []string{
		"The Withering Kingdom",
		"Session 1: The Ambush",
		"### the recording",
		"DM: The door grinds open.",
		"**#1 ruling** — Does hiding work in dim light?",
		"Ruled yes",
		"exported for the group wiki",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("export missing %q", want)
		}
	}
}

func TestExportMarkdownFenceSurvivesBackticks(t *testing.T) {
	s, cid := seeded(t)
	ses := addSession(t, s, cid, "one")
	addSource(t, s, ses.ID, SourceDMNotes, "note with ``` fences ``` inside")
	md, err := s.ExportMarkdown(context.Background(), ses.ID)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	// The fence around the content must be strictly longer than any run
	// inside it; a four-backtick fence wrapping three-backtick content.
	if !strings.Contains(md, "````\nnote with ``` fences ``` inside\n````") {
		t.Errorf("fence did not widen:\n%s", md)
	}
}

func TestExportMissingSession(t *testing.T) {
	s, _ := seeded(t)
	if _, err := s.ExportMarkdown(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v; want ErrNotFound", err)
	}
}

/* ---------- the acceptance round-trip ---------- */

// TestRoundTripTranscriptToSpanToExport is the acceptance criterion from the
// issue, end to end over a temp database: upload a transcript, resolve an
// arbitrary span back to its quoted text, export the log.
func TestRoundTripTranscriptToSpanToExport(t *testing.T) {
	s, cid := seeded(t)
	ses := addSession(t, s, cid, "The Round Trip")

	ing, err := IngestByFilename("session.srt", []byte(srtFixture))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	src, err := s.AddSource(context.Background(), ses.ID, SourceTranscript, "DM", "the recording", ing.Text, ing.Timing)
	if err != nil {
		t.Fatalf("source: %v", err)
	}

	// An arbitrary span — chosen from the parsed text, the way extraction
	// will cite it: byte offsets plus the quote.
	span, ok := Locate(ing.Text, "I check for traps.")
	if !ok {
		t.Fatal("locate")
	}
	resolved, err := s.ResolveSpan(context.Background(), src.ID, span.Start, span.End)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Quote != "I check for traps." {
		t.Fatalf("resolved = %q", resolved.Quote)
	}

	// A ruling logged at the table surfaces its own precedent — itself
	// excluded by construction (the caller passes the fresh event's id), and
	// an unrelated question matches nothing from this one-session campaign.
	ev := addEvent(t, s, ses.ID, EventRuling, "Do traps trigger on a failed passive check?", "Ruled: only on interaction.", nil)
	matches, err := s.MatchPriorRulings(context.Background(), cid, "traps traps traps", ev.ID, 5)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("self-cited matches = %d; want 0", len(matches))
	}

	md, err := s.ExportMarkdown(context.Background(), ses.ID)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if !strings.Contains(md, "I check for traps.") || !strings.Contains(md, "**#1 ruling**") {
		t.Fatalf("export incomplete:\n%s", md)
	}
}
