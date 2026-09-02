package canon

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/testdb"
)

// openDB hands the test a private, fully migrated database — a file copy of
// the template testdb builds once per binary. The canon tables exist only
// through the migration runner; tests that need to drive the runner itself
// over a fresh replay (the adversarial downgrade cycle) still can, since a
// copy is stamped at the latest version and behaves like any migrated file.
func openDB(t *testing.T) *sql.DB {
	t.Helper()
	return testdb.Open(t)
}

// seeded builds the fixture stack the e2e tests run on: the campaign seed's
// entities and graph, one session, and (via addSource) transcript sources.
func seeded(t *testing.T) (*sql.DB, *campaign.Fixture, string) {
	t.Helper()
	db := openDB(t)
	if _, err := db.Exec(
		`INSERT INTO users (id, username, password_hash, is_admin, created_at) VALUES ('keeper', 'keeper', 'x', 0, 0)`); err != nil {
		t.Fatalf("insert owner: %v", err)
	}
	fx, err := campaign.Seed(context.Background(), db, "keeper", "")
	if err != nil {
		t.Fatalf("campaign seed: %v", err)
	}
	var sid string
	if err := db.QueryRow(`
		INSERT INTO game_sessions (id, campaign_id, ordinal, name, status, created_at, updated_at)
		VALUES ('sess-1', ?, 1, 'Session 1', 'done', 0, 0)
		RETURNING id`, fx.Campaign.ID).Scan(&sid); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	return db, fx, sid
}

var sourceSeq atomic.Int64

// addSource inserts a transcript source directly — the gamesession package's
// AddSource is the API path, but the canon tests only need the row, and the
// checksum must match what extraction recomputes from content.
func addSource(t *testing.T, db *sql.DB, sessionID, kind, content string) string {
	t.Helper()
	id := fmt.Sprintf("src-%d", sourceSeq.Add(1))
	sum := sha256.Sum256([]byte(content))
	if _, err := db.Exec(`
		INSERT INTO session_sources (id, session_id, kind, author, title, content, checksum, created_at)
		VALUES (?, ?, ?, 'DM', '', ?, ?, 0)`,
		id, sessionID, kind, content, hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("insert source: %v", err)
	}
	return id
}

// fixtureTranscript is the session transcript the fake model "reads". The
// fixture response's quotes are verbatim substrings of exactly this text.
const fixtureTranscript = `DM: The party enters the Waystone Inn. Tom the Innkeeper nods and pulls Bran aside.

DM: The ledger on the counter lists the Eastern Mines, and the signature at the bottom is the Duke's steward's. Mira reads the ledger carefully, frowning at the dates.

Tom says: "The Duke's men came through twice this month. Nobody comes twice unless they're fetching something."

Thalia asks Tom whether anyone else has been asking about the mines. Tom glances at the door and says the robed folk from Greyfall asked the same, three nights back.

Later, on the road, the party is ambushed by robed figures near the falls. Keth turns one back with his shield; the others scatter into the dark.`

// fixtureResponse renders the model's scripted reply with real fixture ids.
// It exercises every candidate kind, quotes that are verbatim in
// fixtureTranscript, and one deliberate drop of each flavor the e2e asserts.
func fixtureResponse(fx *campaign.Fixture) string {
	return fmt.Sprintf(`{
  "new_entities": [
    {"local_id": "robed-folk", "kind": "faction", "name": "The Robed Folk",
     "summary": "Robed figures operating out of Greyfall.",
     "quote": "the robed folk from Greyfall asked the same, three nights back",
     "confidence": 0.9}
  ],
  "facts": [
    {"local_id": "duke-men-came-twice", "statement": "The Duke's men passed through Blackwater twice this month.",
     "subject": %q, "predicate": "visited", "object_entity": "", "object_literal": "twice this month",
     "visibility": "public",
     "quote": "The Duke's men came through twice this month.", "confidence": 0.95},
    {"local_id": "uncited-fact", "statement": "A fact with no quote must be dropped.",
     "subject": %q, "predicate": "knows_of", "object_entity": %q, "object_literal": "",
     "visibility": "public", "quote": "", "confidence": 0.9},
    {"local_id": "invented-fact", "statement": "A fact quoting words that are not in the source must be dropped.",
     "subject": %q, "predicate": "fears", "object_entity": %q, "object_literal": "",
     "visibility": "public", "quote": "these words appear nowhere in the transcript", "confidence": 0.9},
    {"local_id": "dangling-fact", "statement": "A fact referencing an unknown entity must be dropped.",
     "subject": "someone-who-does-not-exist", "predicate": "met", "object_entity": %q, "object_literal": "",
     "visibility": "public", "quote": "Tom the Innkeeper nods and pulls Bran aside", "confidence": 0.9}
  ],
  "events": [
    {"local_id": "road-ambush", "summary": "The party is ambushed by robed figures on the Greyfall road.",
     "clock_at": null, "location": %q,
     "participants": [{"entity": %q, "role": "party"}],
     "quote": "the party is ambushed by robed figures near the falls", "confidence": 0.9}
  ],
  "discoveries": [
    {"fact": "duke-men-came-twice", "discovered_by": "party", "stance": "knows",
     "method": "Tom told them at the Waystone",
     "quote": "The Duke's men came through twice this month.", "confidence": 0.9}
  ],
  "relationships": [
    {"from_entity": %q, "rel_type": "located_in", "to_entity": %q,
     "quote": "The party enters the Waystone Inn. Tom the Innkeeper nods", "confidence": 0.6}
  ]
}`, fx.Duke, fx.Duke, fx.Mira, fx.Duke, fx.Mira, fx.Tom, fx.Blackwater, fx.Thalia, fx.Tom, fx.Blackwater)
}

// fakeModel replays scripted responses in call order, recording every prompt
// it was handed. The e2e tests assert on the call count to prove ledger
// skips make no model calls at all.
type fakeModel struct {
	responses []string
	errs      []error
	calls     []string
}

func (f *fakeModel) ModelName() string { return "fake-extractor" }

func (f *fakeModel) Complete(ctx context.Context, system, user string) (Completion, error) {
	i := len(f.calls)
	f.calls = append(f.calls, user)
	if i < len(f.errs) && f.errs[i] != nil {
		return Completion{}, f.errs[i]
	}
	if i >= len(f.responses) {
		return Completion{}, fmt.Errorf("script exhausted at call %d", i+1)
	}
	return Completion{Text: f.responses[i], InputTokens: 100, OutputTokens: 200}, nil
}

// newStore builds a canon store over the seeded db with the given script.
// Interval zero keeps the tests fast; the waitInterval behavior has its own
// focused test.
func newStore(t *testing.T, db *sql.DB, m ModelClient, cfg Config) *Store {
	t.Helper()
	s, err := New(db, m, cfg)
	if err != nil {
		t.Fatalf("canon store: %v", err)
	}
	return s
}

func testConfig() Config {
	return Config{MaxCandidates: 500, BatchSize: 8, Interval: 0}
}
