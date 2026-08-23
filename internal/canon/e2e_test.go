package canon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
)

/*
The end-to-end acceptance run (the way Arda's test_validation_run.py works):
a transcript in, staged candidates out, drops logged with reasons, the ledger
written, resume verified — all over a temp SQLite database with a fake model
replaying a fixture response.
*/

func TestExtract_EndToEnd(t *testing.T) {
	db, fx, sessionID := seeded(t)
	ctx := context.Background()
	sourceID := addSource(t, db, sessionID, "transcript", fixtureTranscript)

	model := &fakeModel{responses: []string{fixtureResponse(fx)}}
	s := newStore(t, db, model, testConfig())

	run, err := s.Extract(ctx, ExtractInput{CampaignID: fx.Campaign.ID})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if run.Status != RunCompleted {
		t.Fatalf("status = %q, error %q", run.Status, run.Error)
	}
	if len(model.calls) != 1 {
		t.Fatalf("model calls = %d, want 1 (single-chunk source)", len(model.calls))
	}

	// Candidates staged: the five survivors of the fixture response.
	cands, err := s.ListCandidates(ctx, fx.Campaign.ID, CandidateFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 5 {
		t.Fatalf("candidates = %d, want 5", len(cands))
	}
	kinds := map[string]int{}
	for _, c := range cands {
		kinds[c.Kind]++
		if c.SessionID != sessionID || c.SourceID != sourceID {
			t.Fatalf("candidate %s cites (%s, %s), want (%s, %s)", c.ID, c.SessionID, c.SourceID, sessionID, sourceID)
		}
		if c.Confidence <= 0 || c.Confidence > 1 {
			t.Fatalf("candidate %s confidence %v", c.ID, c.Confidence)
		}
	}
	for kind, want := range map[string]int{
		KindFact: 1, KindEvent: 1, KindDiscovery: 1, KindRelationship: 1, KindEntity: 1,
	} {
		if kinds[kind] != want {
			t.Fatalf("kind %s staged %d, want %d (kinds %v)", kind, kinds[kind], want, kinds)
		}
	}

	// Every staged span resolves back to its quote through the session
	// layer — the span rule's round trip.
	gs, err := gamesession.New(db)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cands {
		span, err := gs.ResolveSpan(ctx, c.SourceID, c.SpanStart, c.SpanEnd)
		if err != nil {
			t.Fatalf("resolve span for %s: %v", c.ID, err)
		}
		if span.Quote != c.Quote {
			t.Fatalf("candidate %s quote %q does not match its span %q", c.ID, c.Quote, span.Quote)
		}
	}

	// Drops logged with reasons: uncited, quote_not_in_source,
	// unknown_entity — the three planted violations.
	drops, err := s.ListDrops(ctx, fx.Campaign.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]bool{}
	for _, d := range drops {
		reasons[d.Reason] = true
	}
	for _, want := range []string{DropUncited, DropQuoteNotInSource, DropUnknownEntity} {
		if !reasons[want] {
			t.Fatalf("drop reason %q missing from %v", want, drops)
		}
	}
	if run.Stats.Dropped[DropUncited] != 1 || run.Stats.Dropped[DropQuoteNotInSource] != 1 || run.Stats.Dropped[DropUnknownEntity] != 1 {
		t.Fatalf("stats.dropped = %v", run.Stats.Dropped)
	}

	// Raw model output stored verbatim with tokens and the input reference.
	outputs, err := s.ModelOutputs(ctx, fx.Campaign.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(outputs) != 1 {
		t.Fatalf("model outputs = %d, want 1", len(outputs))
	}
	o := outputs[0]
	if o.SourceID != sourceID || o.PromptVersion != PROMPT_VERSION || o.Model != "fake-extractor" {
		t.Fatalf("model output = %+v", o)
	}
	if o.InputTokens != 100 || o.OutputTokens != 200 {
		t.Fatalf("tokens = %d/%d", o.InputTokens, o.OutputTokens)
	}
	if !strings.Contains(o.Raw, "duke-men-came-twice") {
		t.Fatalf("raw response not stored verbatim: %.80s", o.Raw)
	}

	// The ledger row: done, one chunk, staged and dropped counted.
	ledger, err := s.LedgerForSource(ctx, fx.Campaign.ID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger) != 1 {
		t.Fatalf("ledger rows = %d, want 1", len(ledger))
	}
	l := ledger[0]
	if l.Status != LedgerDone || l.Chunks != 1 || l.Staged != 5 || l.Dropped != 3 {
		t.Fatalf("ledger = %+v", l)
	}
	if l.PromptVersion != PROMPT_VERSION || l.InputChecksum == "" || l.RunID != run.ID {
		t.Fatalf("ledger = %+v", l)
	}

	// Run stats carry the whole story.
	if run.Stats.UnitsDone != 1 || run.Stats.UnitsSkipped != 0 || run.Stats.Requests != 1 ||
		run.Stats.InputTokens != 100 || run.Stats.OutputTokens != 200 || run.Stats.StagedTotal() != 5 {
		t.Fatalf("stats = %+v", run.Stats)
	}

	// The prompt the model saw: the chunk, the entity list, the roster,
	// the vocabulary — the extraction contract, on the wire.
	prompt := model.calls[0]
	for _, want := range []string{
		fx.Duke, fx.Tom, fx.Blackwater, fx.Thalia, fx.Mira,
		"located_in", "PARTY ROSTER", "CAMPAIGN ENTITIES", fixtureTranscript[:40],
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("model prompt missing %q", want)
		}
	}
}

func TestExtract_IsIdempotentUnderSameVersionAndChecksum(t *testing.T) {
	db, fx, sessionID := seeded(t)
	ctx := context.Background()
	addSource(t, db, sessionID, "transcript", fixtureTranscript)

	model := &fakeModel{responses: []string{fixtureResponse(fx)}}
	s := newStore(t, db, model, testConfig())

	if _, err := s.Extract(ctx, ExtractInput{CampaignID: fx.Campaign.ID}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if len(model.calls) != 1 {
		t.Fatalf("calls after first run = %d", len(model.calls))
	}

	// Re-run: same prompt version, same input checksum — the ledger skips
	// the unit without a model call.
	run, err := s.Extract(ctx, ExtractInput{CampaignID: fx.Campaign.ID})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(model.calls) != 1 {
		t.Fatalf("ledger skip still called the model: %d calls", len(model.calls))
	}
	if run.Status != RunCompleted || run.Stats.UnitsSkipped != 1 || run.Stats.UnitsDone != 0 {
		t.Fatalf("second run = %+v", run.Stats)
	}

	// Changed campaign state (a new entity) changes the input checksum and
	// re-extracts under the same prompt version.
	cs, err := campaign.New(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CreateEntity(ctx, fx.Campaign.ID, campaign.KindNPC, "The Steward", "Signs the ledgers.", nil); err != nil {
		t.Fatal(err)
	}
	model.responses = append(model.responses, fixtureResponse(fx))
	run, err = s.Extract(ctx, ExtractInput{CampaignID: fx.Campaign.ID})
	if err != nil {
		t.Fatalf("third run: %v", err)
	}
	if len(model.calls) != 2 {
		t.Fatalf("changed input must re-extract: %d calls", len(model.calls))
	}
	ledger, err := s.LedgerForSource(ctx, fx.Campaign.ID, lookupSourceID(t, db, sessionID))
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger) != 2 {
		t.Fatalf("ledger rows = %d, want 2 (one per input checksum)", len(ledger))
	}
}

func TestExtract_InterruptedRunResumesWhereItStopped(t *testing.T) {
	db, fx, sessionID := seeded(t)
	ctx := context.Background()
	src1 := addSource(t, db, sessionID, "transcript", fixtureTranscript)
	secondTranscript := strings.ReplaceAll(fixtureTranscript, "Waystone Inn", "Broken Bell") + "\n\nKeth prays at the shrine."
	src2 := addSource(t, db, sessionID, "player_journal", secondTranscript)

	// The first call answers; the second fails the run mid-flight.
	model := &fakeModel{
		responses: []string{fixtureResponse(fx)},
		errs:      []error{nil, errors.New("endpoint melted down")},
	}
	s := newStore(t, db, model, testConfig())

	run, err := s.Extract(ctx, ExtractInput{CampaignID: fx.Campaign.ID})
	if err == nil {
		t.Fatal("a failed model call must surface the error")
	}
	if run == nil || run.Status != RunFailed || run.StopReason != StopError {
		t.Fatalf("run = %+v", run)
	}
	if !strings.Contains(run.Error, "endpoint melted down") {
		t.Fatalf("run error = %q", run.Error)
	}

	// Unit one committed; unit two left no ledger row.
	ledger1, err := s.LedgerForSource(ctx, fx.Campaign.ID, src1)
	if err != nil || len(ledger1) != 1 || ledger1[0].Status != LedgerDone {
		t.Fatalf("source 1 ledger = %+v err %v", ledger1, err)
	}
	ledger2, err := s.LedgerForSource(ctx, fx.Campaign.ID, src2)
	if err != nil || len(ledger2) != 0 {
		t.Fatalf("failed unit must leave no ledger row: %+v err %v", ledger2, err)
	}
	cands, err := s.ListCandidates(ctx, fx.Campaign.ID, CandidateFilter{SourceID: src1})
	if err != nil || len(cands) != 5 {
		t.Fatalf("committed unit's candidates = %d err %v", len(cands), err)
	}

	// Resume: source one is skipped without a call, source two is
	// extracted, and the run completes.
	model.responses = []string{fixtureResponse(fx), fixtureResponse(fx), fixtureResponse(fx)}
	model.errs = nil
	model.calls = nil
	run, err = s.Extract(ctx, ExtractInput{CampaignID: fx.Campaign.ID})
	if err != nil {
		t.Fatalf("resume run: %v", err)
	}
	if run.Status != RunCompleted {
		t.Fatalf("status = %q (%s)", run.Status, run.Error)
	}
	if len(model.calls) != 1 {
		t.Fatalf("resume must call the model once, not %d times (skip must be free)", len(model.calls))
	}
	if run.Stats.UnitsSkipped != 1 || run.Stats.UnitsDone != 1 {
		t.Fatalf("resume stats = %+v", run.Stats)
	}
	cands2, err := s.ListCandidates(ctx, fx.Campaign.ID, CandidateFilter{SourceID: src2})
	if err != nil || len(cands2) != 4 {
		// Four, not five: the second source edited "Waystone Inn" to
		// "Broken Bell", so the relationship whose quote cites the
		// Waystone line legitimately drops (quote_not_in_source).
		t.Fatalf("source 2 candidates = %d err %v", len(cands2), err)
	}
}

func TestExtract_BudgetGuardStopsTheRun(t *testing.T) {
	db, fx, sessionID := seeded(t)
	ctx := context.Background()
	// A source long enough for several chunks at the 12k target.
	var paras []string
	for i := 0; i < 300; i++ {
		paras = append(paras, fmt.Sprintf("Beat %d: the party argues about the map, again, at length, in the rain, and nobody remembers the last ruling about maps.", i))
	}
	long := strings.Join(paras, "\n\n")
	addSource(t, db, sessionID, "transcript", long)

	// Each fake call costs 100*3/1e6 + 200*15/1e6 = 0.0033 USD; a budget
	// of 0.005 admits one call and stops before the third (the second
	// pushes spent to 0.0066, over the line for the pre-call check).
	cfg := testConfig()
	cfg.BudgetUSD = 0.005
	cfg.PriceInMTok = 3
	cfg.PriceOutMTok = 15
	empty := `{"facts":[]}`
	model := &fakeModel{responses: []string{empty, empty, empty, empty, empty, empty}}
	s := newStore(t, db, model, cfg)

	run, err := s.Extract(ctx, ExtractInput{CampaignID: fx.Campaign.ID})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if run.Status != RunStopped || run.StopReason != StopBudget {
		t.Fatalf("run = %q/%q", run.Status, run.StopReason)
	}
	if run.Stats.Requests != 2 {
		t.Fatalf("requests = %d, want 2 (budget checked before each call)", run.Stats.Requests)
	}
	if run.Stats.CostUSD <= 0.0065 || run.Stats.CostUSD >= 0.0067 {
		t.Fatalf("cost = %v, want ~0.0066 (two admitted calls)", run.Stats.CostUSD)
	}
	// The unit still committed what it had, ledger done: resume semantics
	// are per unit, and the budget is a run-level guard.
	ledger, err := s.LedgerForSource(ctx, fx.Campaign.ID, lookupSourceID(t, db, sessionID))
	if err != nil || len(ledger) != 1 || ledger[0].Status != LedgerDone {
		t.Fatalf("ledger = %+v err %v", ledger, err)
	}
}

func TestExtract_CandidateCapStopsAndLogsOverflow(t *testing.T) {
	db, fx, sessionID := seeded(t)
	ctx := context.Background()
	addSource(t, db, sessionID, "transcript", fixtureTranscript)

	cfg := testConfig()
	cfg.MaxCandidates = 2
	model := &fakeModel{responses: []string{fixtureResponse(fx)}}
	s := newStore(t, db, model, cfg)

	run, err := s.Extract(ctx, ExtractInput{CampaignID: fx.Campaign.ID})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if run.Status != RunStopped || run.StopReason != StopCandidates {
		t.Fatalf("run = %q/%q", run.Status, run.StopReason)
	}
	if run.Stats.StagedTotal() != 2 {
		t.Fatalf("staged = %d, want the cap", run.Stats.StagedTotal())
	}
	drops, err := s.ListDrops(ctx, fx.Campaign.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	overflow := 0
	for _, d := range drops {
		if d.Reason == DropMaxCandidates {
			overflow++
		}
	}
	// 5 staged-worthy candidates minus the 2 the cap admitted: the
	// overflow is accounted, never silent.
	if overflow != 3 {
		t.Fatalf("max_candidates drops = %d, want 3 (drops: %v)", overflow, drops)
	}
}

func TestExtract_UnparseableResponseIsLoggedNotFatal(t *testing.T) {
	db, fx, sessionID := seeded(t)
	ctx := context.Background()
	addSource(t, db, sessionID, "transcript", fixtureTranscript)

	model := &fakeModel{responses: []string{"sorry, I cannot produce JSON today"}}
	s := newStore(t, db, model, testConfig())

	run, err := s.Extract(ctx, ExtractInput{CampaignID: fx.Campaign.ID})
	if err != nil {
		t.Fatalf("an unparseable response is data, not a run failure: %v", err)
	}
	if run.Status != RunCompleted {
		t.Fatalf("status = %q", run.Status)
	}
	if run.Stats.ParseProblems != 1 {
		t.Fatalf("parse problems = %d", run.Stats.ParseProblems)
	}
	drops, err := s.ListDrops(ctx, fx.Campaign.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(drops) != 1 || drops[0].Reason != DropUnparseable {
		t.Fatalf("drops = %v", drops)
	}
	// The raw response is still stored verbatim — provenance floors out
	// in what the model actually said.
	outputs, err := s.ModelOutputs(ctx, fx.Campaign.ID, run.ID)
	if err != nil || len(outputs) != 1 || outputs[0].Raw != "sorry, I cannot produce JSON today" {
		t.Fatalf("outputs = %+v err %v", outputs, err)
	}
}

func TestExtract_BatchSizeDefersTheRemainder(t *testing.T) {
	db, fx, sessionID := seeded(t)
	ctx := context.Background()
	addSource(t, db, sessionID, "transcript", fixtureTranscript)
	addSource(t, db, sessionID, "dm_notes", "The Duke met the steward at dusk.")

	cfg := testConfig()
	cfg.BatchSize = 1
	model := &fakeModel{responses: []string{fixtureResponse(fx), `{"facts":[]}`}}
	s := newStore(t, db, model, cfg)

	run, err := s.Extract(ctx, ExtractInput{CampaignID: fx.Campaign.ID})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if run.Status != RunStopped || run.StopReason != StopUnits {
		t.Fatalf("run = %q/%q", run.Status, run.StopReason)
	}
	if len(model.calls) != 1 || run.Stats.UnitsDone != 1 {
		t.Fatalf("calls = %d done = %d", len(model.calls), run.Stats.UnitsDone)
	}
}

/*
The leak acceptance: no staged candidate is readable through any scoped
retrieval path. Staged candidates live only in canon_candidates — nothing
extraction produces enters the campaign graph — so every knowledge-scoped
read and every campaign-table count must come back unchanged. Reuses the
MAD-304 leak-test fixture (campaign.Seed) and walks the same scoped paths.
*/

func TestExtract_NoCandidateReadableThroughAnyScope(t *testing.T) {
	db, fx, sessionID := seeded(t)
	ctx := context.Background()
	addSource(t, db, sessionID, "transcript", fixtureTranscript)

	ks, err := knowledge.New(db)
	if err != nil {
		t.Fatal(err)
	}
	scopes := []knowledge.Scope{
		knowledge.ScopeDM, knowledge.ScopeParty,
		knowledge.ScopeCharacter(fx.Thalia), knowledge.ScopeNPC(fx.Elara),
	}

	snapshot := graphCounts(t, db)
	model := &fakeModel{responses: []string{fixtureResponse(fx)}}
	s := newStore(t, db, model, testConfig())
	run, err := s.Extract(ctx, ExtractInput{CampaignID: fx.Campaign.ID})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if run.Stats.StagedTotal() != 5 {
		t.Fatalf("fixture must stage 5 candidates to give the leak test teeth: %+v", run.Stats)
	}

	// 1. Nothing entered the graph: raw table counts unchanged.
	after := graphCounts(t, db)
	for table, before := range snapshot {
		if after[table] != before {
			t.Fatalf("table %s grew %d -> %d: extraction must write only to the review queue",
				table, before, after[table])
		}
	}

	// 2. The scoped read paths return none of it — facts, entities,
	// timeline, relationships, prose search (statement words, quote words,
	// new-entity names), and the campaign store's DM reads.
	cands, err := s.ListCandidates(ctx, fx.Campaign.ID, CandidateFilter{})
	if err != nil {
		t.Fatal(err)
	}
	// Needles are phrases unique to the transcript and the staged
	// candidates — checked against the seed fixture, which itself
	// contains "Waystone" (Tom's summary) and "robed figures" (the seed
	// flashback event), so the needle phrases pin the candidates exactly.
	needles := []string{
		"twice this month", "The Duke's men", "The Robed Folk",
		"Waystone Inn", "ambushed by robed figures",
	}
	for _, scope := range scopes {
		facts, err := ks.Facts(ctx, scope, fx.Campaign.ID, knowledge.FactFilter{})
		if err != nil {
			t.Fatalf("Facts at %s: %v", scope, err)
		}
		assertNoNeedle(t, "Facts", scope.String(), factsText(facts), needles)
		entities, err := ks.Entities(ctx, scope, fx.Campaign.ID, "")
		if err != nil {
			t.Fatalf("Entities at %s: %v", scope, err)
		}
		assertNoNeedle(t, "Entities", scope.String(), entitiesText(entities), needles)
		timeline, err := ks.Timeline(ctx, scope, fx.Campaign.ID)
		if err != nil {
			t.Fatalf("Timeline at %s: %v", scope, err)
		}
		assertNoNeedle(t, "Timeline", scope.String(), eventsText(timeline), needles)
		rels, err := ks.Relationships(ctx, scope, fx.Campaign.ID)
		if err != nil {
			t.Fatalf("Relationships at %s: %v", scope, err)
		}
		assertNoNeedle(t, "Relationships", scope.String(), relsText(rels), needles)
		for _, q := range []string{"Waystone", "twice this month"} {
			hits, err := ks.SearchProse(ctx, scope, fx.Campaign.ID, q, 50)
			if err != nil && !errors.Is(err, knowledge.ErrEmptyQuery) {
				t.Fatalf("SearchProse %q at %s: %v", q, scope, err)
			}
			assertNoNeedle(t, "SearchProse "+q, scope.String(), hitsText(hits), needles)
		}
	}

	// 3. The queue itself can read them — the teeth check: the candidates
	// exist, they are just not retrievable through any scoped path.
	if len(cands) != 5 {
		t.Fatalf("candidates = %d", len(cands))
	}
}

// graphCounts snapshots the graph tables the extraction must not touch.
func graphCounts(t *testing.T, db *sql.DB) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range []string{
		"facts", "fact_provenance", "events", "event_participants", "event_links",
		"entities", "entity_aliases", "relationships", "discoveries", "awareness",
	} {
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		out[table] = n
	}
	return out
}

// lookupSourceID returns the session's first source id, for ledger lookups
// in tests that insert sources directly.
func lookupSourceID(t *testing.T, db *sql.DB, sessionID string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(
		`SELECT id FROM session_sources WHERE session_id = ? ORDER BY created_at, id LIMIT 1`,
		sessionID).Scan(&id); err != nil {
		t.Fatalf("lookup source: %v", err)
	}
	return id
}

/* helpers rendering scoped results as searchable text */

func factsText(fs []campaign.Fact) string {
	var b strings.Builder
	for _, f := range fs {
		b.WriteString(f.Statement)
		b.WriteString(" ")
	}
	return b.String()
}

func entitiesText(es []campaign.Entity) string {
	var b strings.Builder
	for _, e := range es {
		b.WriteString(e.Name)
		b.WriteString(" ")
		b.WriteString(e.Summary)
		b.WriteString(" ")
	}
	return b.String()
}

func eventsText(ev []campaign.Event) string {
	var b strings.Builder
	for _, e := range ev {
		b.WriteString(e.Summary)
		b.WriteString(" ")
	}
	return b.String()
}

func relsText(rs []campaign.Relationship) string {
	var b strings.Builder
	for _, r := range rs {
		fmt.Fprintf(&b, "%s %s %s ", r.FromEntity, r.RelType, r.ToEntity)
	}
	return b.String()
}

func hitsText(hs []knowledge.ProseHit) string {
	var b strings.Builder
	for _, h := range hs {
		b.WriteString(h.Snippet)
		b.WriteString(" ")
	}
	return b.String()
}

func assertNoNeedle(t *testing.T, where, scope, haystack string, needles []string) {
	t.Helper()
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			t.Fatalf("LEAK: %s at scope %s surfaced %q", where, scope, n)
		}
	}
}
