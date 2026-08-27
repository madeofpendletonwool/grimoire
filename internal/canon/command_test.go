package canon

// The natural-language command interface's tests (MAD-363): the slot-frame
// parser and the referent stack as pure unit tables, and the engine end to
// end over a temp SQLite database with a fake model replaying scripted
// fills — the shape ADR 8 settles for every model-driven stage.
//
// The acceptance load-bearers:
//   - an ambiguous reference returns a question with candidates and stages
//     nothing;
//   - a command naming an entity that does not exist proposes creating it
//     and says so, never inventing an id;
//   - a command the vocabulary does not cover says so plainly;
//   - pronouns bind through the referent stack: accepted proposals are
//     usable, open ones ask, dismissed ones expired;
//   - undo drops the batch, and nothing was written.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
)

/* ---------- the slot-frame parser (pure) ---------- */

func TestParseSlotFrame_CreateEntity(t *testing.T) {
	f, problems := ParseSlotFrame(map[string]any{
		"verb": "create_entity", "name": " Vess the Quiet ", "kind": "npc",
		"summary": "A level 5 necromancer.", "rel_type": "serves", "rel_target": "Duke Aldric Vane",
	})
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if f.Verb != CommandVerbCreateEntity || f.Name != "Vess the Quiet" || f.Kind != "npc" {
		t.Fatalf("frame = %+v", f)
	}
	if f.RelType != "serves" || f.RelTarget != "Duke Aldric Vane" {
		t.Fatalf("relationship slots = %+v", f)
	}
}

func TestParseSlotFrame_Problems(t *testing.T) {
	cases := []struct {
		name    string
		values  map[string]any
		wantSub string
	}{
		{"create needs a name", map[string]any{"verb": "create_entity", "kind": "npc"}, "needs a name"},
		{"create needs a kind", map[string]any{"verb": "create_entity", "name": "Vess"}, "needs a kind"},
		{"rel needs both ends", map[string]any{"verb": "create_relationship", "rel_type": "located_in", "from_entity": "him"}, "both ends"},
		{"no self relationship", map[string]any{"verb": "create_relationship", "rel_type": "knows", "from_entity": "him", "to_entity": "HIM"}, "itself"},
		{"rel needs a type", map[string]any{"verb": "create_relationship", "from_entity": "a", "to_entity": "b"}, "needs a type"},
		{"fact needs a statement", map[string]any{"verb": "create_fact", "subject": "the Duke", "predicate": "owns", "object_kind": "literal", "object_literal": "a ledger"}, "statement"},
		{"fact needs a predicate", map[string]any{"verb": "create_fact", "subject": "the Duke", "statement": "s", "object_kind": "literal", "object_literal": "o"}, "predicate"},
		{"literal fact needs its object", map[string]any{"verb": "create_fact", "subject": "a", "statement": "s", "predicate": "owns", "object_kind": "literal"}, "object"},
		{"entity fact needs its object", map[string]any{"verb": "create_fact", "subject": "a", "statement": "s", "predicate": "owns", "object_kind": "entity"}, "object entity"},
		{"scene needs who", map[string]any{"verb": "add_to_scene", "scene": "The Waystone"}, "who"},
		{"merge needs the survivor", map[string]any{"verb": "merge_names", "entity": "Vess"}, "belongs to"},
		{"merge needs a name not a pronoun", map[string]any{"verb": "merge_names", "entity": "him", "merge_into": "Vess the Quiet"}, "pronoun"},
		{"create's rel needs type and target", map[string]any{"verb": "create_entity", "name": "V", "kind": "npc", "rel_target": "the Duke"}, "both a type and a target"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, problems := ParseSlotFrame(tc.values)
			found := false
			for _, p := range problems {
				if strings.Contains(p, tc.wantSub) {
					found = true
				}
			}
			if !found {
				t.Fatalf("problems %v miss %q", problems, tc.wantSub)
			}
		})
	}
}

func TestParseSlotFrame_Normalization(t *testing.T) {
	f, problems := ParseSlotFrame(map[string]any{"verb": " none "})
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if f.Verb != CommandVerbNone {
		t.Fatalf("verb = %q", f.Verb)
	}
	f, problems = ParseSlotFrame(map[string]any{"verb": "create_entity", `name`: `"Vess"`, "kind": "npc"})
	if len(problems) != 0 || f.Name != "Vess" {
		t.Fatalf("quoted name not stripped: %+v %v", f, problems)
	}
	if _, problems := ParseSlotFrame(map[string]any{"verb": "explode"}); problems == nil {
		t.Fatal("unknown verb must be refused")
	}
	// The add_to_scene role defaults to present.
	f, _ = ParseSlotFrame(map[string]any{"verb": "add_to_scene", "entity": "Tom the Innkeeper"})
	if f.Role != "present" {
		t.Fatalf("default role = %q", f.Role)
	}
}

/* ---------- the referent stack (pure) ---------- */

func TestBindPronoun(t *testing.T) {
	stack := []Referent{{ReviewID: "r1", Name: "Vess the Quiet", Kind: "npc"}}
	candidates := []CommandCandidate{{Name: "Vess the Quiet", Kind: "npc"}}

	id, why, _ := bindPronoun(nil, nil)
	if why != referentNone || id != "" {
		t.Fatalf("empty stack: %q %q", why, id)
	}

	id, why, _ = bindPronoun(stack, map[string]referentState{"r1": {Status: ReviewAccepted, ResultRef: "ent-1"}})
	if why != referentBound || id != "ent-1" {
		t.Fatalf("accepted: %q %q", why, id)
	}

	id, why, cands := bindPronoun(stack, map[string]referentState{"r1": {Status: ReviewOpen}})
	if why != referentPending || id != "" {
		t.Fatalf("open: %q %q", why, id)
	}
	if len(cands) != 1 || cands[0].Note == "" {
		t.Fatalf("pending candidates = %+v", cands)
	}

	id, why, _ = bindPronoun(stack, map[string]referentState{"r1": {Status: ReviewDismissed}})
	if why != referentExpired || id != "" {
		t.Fatalf("dismissed: %q %q", why, id)
	}

	// Accepted but nothing applied: a dead end, not a guess.
	id, why, _ = bindPronoun(stack, map[string]referentState{"r1": {Status: ReviewAccepted}})
	if why != referentExpired || id != "" {
		t.Fatalf("accepted-unapplied: %q %q", why, id)
	}

	two := append(stack, Referent{ReviewID: "r2", Name: "The Robed Folk", Kind: "faction"})
	id, why, cands = bindPronoun(two, map[string]referentState{
		"r1": {Status: ReviewAccepted, ResultRef: "ent-1"},
		"r2": {Status: ReviewAccepted, ResultRef: "ent-2"},
	})
	if why != referentAmbiguous || id != "" || len(cands) != 2 {
		t.Fatalf("ambiguous: %q %q %+v", why, id, cands)
	}
	_ = candidates
}

/* ---------- the engine, end to end ---------- */

// newCommandStore boots the canon engine over the seeded fixture with a
// scripted model.
func newCommandStore(t *testing.T, responses ...string) (*sql.DB, *campaign.Fixture, *Store, *fakeModel) {
	t.Helper()
	db, fx, _ := seeded(t)
	campaigns, err := campaign.New(db)
	if err != nil {
		t.Fatalf("campaign store: %v", err)
	}
	knowledgeStore, err := knowledge.New(db)
	if err != nil {
		t.Fatalf("knowledge store: %v", err)
	}
	model := &fakeModel{responses: responses}
	s := newStore(t, db, model, testConfig()).WithGraphStores(campaigns, knowledgeStore)
	return db, fx, s, model
}

func mustJSON(v map[string]any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("marshal: %v", err))
	}
	return string(b)
}

func reviewCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM canon_reviews`).Scan(&n); err != nil {
		t.Fatalf("count reviews: %v", err)
	}
	return n
}

func entityIDByName(t *testing.T, db *sql.DB, campaignID, name string) string {
	t.Helper()
	var id string
	err := db.QueryRow(`
		SELECT e.id FROM entities e JOIN entity_aliases a ON a.entity_id = e.id
		 WHERE e.campaign_id = ? AND a.name = ? COLLATE NOCASE`, campaignID, name).Scan(&id)
	if err != nil {
		t.Fatalf("entity by name %q: %v", name, err)
	}
	return id
}

// scriptCreateVess is the fill for "create a necromancer called Vess who
// secretly works for the Duke".
func scriptCreateVess() string {
	return mustJSON(map[string]any{
		"verb": "create_entity", "name": "Vess the Quiet", "kind": "npc",
		"summary":  "A level 5 necromancer in service to the Duke.",
		"rel_type": "serves", "rel_target": "Duke Aldric Vane",
	})
}

func TestCommand_CreateEntityStagesBatchAndSaysSo(t *testing.T) {
	db, fx, s, _ := newCommandStore(t, scriptCreateVess())
	before := reviewCount(t, db)

	res, err := s.RunCommand(context.Background(), CommandInput{
		CampaignID: fx.Campaign.ID, Text: "Create a level 5 necromancer called Vess who secretly works for the Duke.",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Kind != CommandResultBatch {
		t.Fatalf("kind = %s (%s)", res.Kind, res.Message)
	}
	if !strings.Contains(res.Message, `"Vess the Quiet" does not exist yet`) {
		t.Errorf("message must say the entity will be created: %s", res.Message)
	}
	if res.Batch == nil || res.Batch.Source != BatchSourceNLCommand || len(res.Batch.Items) != 2 {
		t.Fatalf("batch = %+v", res.Batch)
	}
	if reviewCount(t, db) != before+2 {
		t.Fatalf("staged %d items, want 2", reviewCount(t, db)-before)
	}
	// The batch's own items are proposals: nothing landed in the graph.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM entities WHERE campaign_id = ?`, fx.Campaign.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	entitiesBefore := 14 // the seed's roster
	if n != entitiesBefore {
		t.Errorf("entities = %d, want %d (nothing written before acceptance)", n, entitiesBefore)
	}
	// The referent stack recorded the proposal.
	log, err := s.CommandLog(context.Background(), fx.Campaign.ID, 10)
	if err != nil || len(log) != 1 {
		t.Fatalf("log = %v %v", log, err)
	}
	if len(log[0].Referents) != 1 || log[0].Referents[0].Name != "Vess the Quiet" || log[0].Referents[0].Kind != "npc" {
		t.Fatalf("referents = %+v", log[0].Referents)
	}

	// Accepting the batch creates the entity and the relationship: the
	// command line writes through the same gate as everything else.
	decided, err := s.DecideBatch(context.Background(), fx.Campaign.ID, res.Batch.ID, DecisionAccept, nil, "keeper")
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if decided.Batch.Status != BatchAccepted {
		t.Fatalf("batch status = %s", decided.Batch.Status)
	}
	vess := entityIDByName(t, db, fx.Campaign.ID, "Vess the Quiet")
	var one int
	if err := db.QueryRow(`
		SELECT 1 FROM relationships WHERE from_entity = ? AND rel_type = 'serves' AND to_entity = ?`,
		vess, fx.Duke).Scan(&one); err != nil {
		t.Fatalf("relationship not applied: %v", err)
	}
}

func TestCommand_PronounBindsAfterAcceptance(t *testing.T) {
	db, fx, s, _ := newCommandStore(t, scriptCreateVess(), mustJSON(map[string]any{
		"verb": "create_relationship", "rel_type": "located_in",
		"from_entity": "him", "to_entity": "Blackwater",
	}))
	ctx := context.Background()
	first, err := s.RunCommand(ctx, CommandInput{CampaignID: fx.Campaign.ID, Text: "create a necromancer called Vess who serves the Duke"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideBatch(ctx, fx.Campaign.ID, first.Batch.ID, DecisionAccept, nil, "keeper"); err != nil {
		t.Fatal(err)
	}
	vess := entityIDByName(t, db, fx.Campaign.ID, "Vess the Quiet")

	res, err := s.RunCommand(ctx, CommandInput{CampaignID: fx.Campaign.ID, Text: "Put him in Blackwater."})
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != CommandResultBatch {
		t.Fatalf("kind = %s (%s)", res.Kind, res.Message)
	}
	item := res.Batch.Items[0]
	if item.Payload["from_entity"] != vess || item.Payload["to_entity"] != fx.Blackwater {
		t.Fatalf("payload = %+v — the pronoun must resolve to the accepted entity's real id", item.Payload)
	}
}

func TestCommand_PronounWhileProposalOpenAsks(t *testing.T) {
	db, fx, s, _ := newCommandStore(t, scriptCreateVess(), mustJSON(map[string]any{
		"verb": "create_relationship", "rel_type": "located_in",
		"from_entity": "him", "to_entity": "Blackwater",
	}))
	ctx := context.Background()
	if _, err := s.RunCommand(ctx, CommandInput{CampaignID: fx.Campaign.ID, Text: "create Vess"}); err != nil {
		t.Fatal(err)
	}
	before := reviewCount(t, db)

	res, err := s.RunCommand(ctx, CommandInput{CampaignID: fx.Campaign.ID, Text: "Put him in Blackwater."})
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != CommandResultQuestion {
		t.Fatalf("kind = %s (%s)", res.Kind, res.Message)
	}
	if !strings.Contains(res.Question.Question, "still a proposal") || !strings.Contains(res.Question.Question, "Vess the Quiet") {
		t.Fatalf("question = %s", res.Question.Question)
	}
	if reviewCount(t, db) != before {
		t.Fatalf("staged something anyway: %d -> %d", before, reviewCount(t, db))
	}
}

func TestCommand_PronounAfterDismissExpired(t *testing.T) {
	_, fx, s, _ := newCommandStore(t, scriptCreateVess(), mustJSON(map[string]any{
		"verb": "create_relationship", "rel_type": "located_in",
		"from_entity": "him", "to_entity": "Blackwater",
	}))
	ctx := context.Background()
	first, err := s.RunCommand(ctx, CommandInput{CampaignID: fx.Campaign.ID, Text: "create Vess"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideBatch(ctx, fx.Campaign.ID, first.Batch.ID, DecisionDismiss, nil, "keeper"); err != nil {
		t.Fatal(err)
	}
	res, err := s.RunCommand(ctx, CommandInput{CampaignID: fx.Campaign.ID, Text: "Put him in Blackwater."})
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != CommandResultQuestion || !strings.Contains(res.Question.Question, "dismissed") {
		t.Fatalf("kind = %s question = %+v", res.Kind, res.Question)
	}
}

func TestCommand_AmbiguousReferenceAsksWithCandidates(t *testing.T) {
	db, fx, s, _ := newCommandStore(t, mustJSON(map[string]any{
		"verb": "create_relationship", "rel_type": "located_in",
		"from_entity": "Tom the Innkeeper", "to_entity": "The Duke",
	}))
	ctx := context.Background()
	campaigns, _ := campaign.New(db)
	// Two entities carrying "The Duke" — the entity_merge_candidate shape.
	second, err := campaigns.CreateEntity(ctx, fx.Campaign.ID, campaign.KindNPC, "The Duke", "A rival duke.", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := campaigns.AddEntityName(ctx, fx.Campaign.ID, fx.Elara, "The Duke", campaign.NameAlias); err != nil {
		t.Fatal(err)
	}
	_ = second
	before := reviewCount(t, db)

	res, err := s.RunCommand(ctx, CommandInput{CampaignID: fx.Campaign.ID, Text: "Put Tom in the Duke's court."})
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != CommandResultQuestion {
		t.Fatalf("kind = %s (%s)", res.Kind, res.Message)
	}
	if !strings.Contains(res.Question.Question, `"The Duke" matches several`) {
		t.Fatalf("question = %s", res.Question.Question)
	}
	if len(res.Question.Candidates) != 2 {
		t.Fatalf("candidates = %+v", res.Question.Candidates)
	}
	if reviewCount(t, db) != before {
		t.Fatalf("staged something anyway: %d -> %d", before, reviewCount(t, db))
	}
}

func TestCommand_ZeroMatchReferenceAsks(t *testing.T) {
	db, fx, s, _ := newCommandStore(t, mustJSON(map[string]any{
		"verb": "create_relationship", "rel_type": "located_in",
		"from_entity": "Vess", "to_entity": "Blackwater",
	}))
	before := reviewCount(t, db)
	res, err := s.RunCommand(context.Background(), CommandInput{
		CampaignID: fx.Campaign.ID, Text: "Put Vess in Blackwater.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != CommandResultQuestion {
		t.Fatalf("kind = %s (%s)", res.Kind, res.Message)
	}
	if !strings.Contains(res.Question.Question, `Nothing in this campaign answers to "Vess"`) {
		t.Fatalf("question = %s", res.Question.Question)
	}
	if !strings.Contains(res.Question.Question, "Create it first") {
		t.Fatalf("question should offer creation first: %s", res.Question.Question)
	}
	if reviewCount(t, db) != before {
		t.Fatalf("staged something anyway")
	}
}

func TestCommand_NearDuplicateProposesTheExistingEntity(t *testing.T) {
	db, fx, s, _ := newCommandStore(t, mustJSON(map[string]any{
		"verb": "create_entity", "name": "Duke Aldric Vane", "kind": "npc", "summary": "Again.",
	}))
	before := reviewCount(t, db)
	res, err := s.RunCommand(context.Background(), CommandInput{
		CampaignID: fx.Campaign.ID, Text: "Create an npc called Duke Aldric Vane.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != CommandResultQuestion {
		t.Fatalf("kind = %s (%s)", res.Kind, res.Message)
	}
	if !strings.Contains(res.Message, "already names") || !strings.Contains(res.Message, "duplicate") {
		t.Fatalf("message = %s", res.Message)
	}
	if len(res.Question.Candidates) != 1 || res.Question.Candidates[0].ID != fx.Duke {
		t.Fatalf("candidates = %+v", res.Question.Candidates)
	}
	if reviewCount(t, db) != before {
		t.Fatalf("a near-duplicate must stage nothing")
	}
}

func TestCommand_UnsupportedSaysSoPlainly(t *testing.T) {
	db, fx, s, _ := newCommandStore(t, mustJSON(map[string]any{"verb": "none"}))
	before := reviewCount(t, db)
	res, err := s.RunCommand(context.Background(), CommandInput{
		CampaignID: fx.Campaign.ID, Text: "What do the players know about the mines?",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != CommandResultUnsupported {
		t.Fatalf("kind = %s", res.Kind)
	}
	if !strings.Contains(res.Message, "not one of the commands") {
		t.Fatalf("message = %s", res.Message)
	}
	if reviewCount(t, db) != before {
		t.Fatalf("an unsupported command must stage nothing")
	}
	// The refusal is on the transcript too.
	log, err := s.CommandLog(context.Background(), fx.Campaign.ID, 10)
	if err != nil || len(log) != 1 || log[0].Kind != CommandResultUnsupported {
		t.Fatalf("log = %+v %v", log, err)
	}
}

func TestCommand_UndoDropsTheOpenBatch(t *testing.T) {
	db, fx, s, model := newCommandStore(t, scriptCreateVess())
	ctx := context.Background()
	first, err := s.RunCommand(ctx, CommandInput{CampaignID: fx.Campaign.ID, Text: "create Vess"})
	if err != nil {
		t.Fatal(err)
	}
	calls := len(model.calls)

	res, err := s.RunCommand(ctx, CommandInput{CampaignID: fx.Campaign.ID, Text: "Undo that."})
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != CommandResultUndo {
		t.Fatalf("kind = %s (%s)", res.Kind, res.Message)
	}
	if !strings.Contains(res.Message, "nothing had been written") {
		t.Fatalf("message = %s", res.Message)
	}
	if len(model.calls) != calls {
		t.Fatalf("undo must not call the model")
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM proposal_batches WHERE id = ?`, first.Batch.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != BatchDismissed {
		t.Fatalf("batch status = %s", status)
	}
	// Nothing landed: no Vess in the graph.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM entities WHERE campaign_id = ? AND name = 'Vess the Quiet'`, fx.Campaign.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("undo wrote an entity anyway")
	}
	// A second undo has nothing to drop.
	res, err = s.RunCommand(ctx, CommandInput{CampaignID: fx.Campaign.ID, Text: "undo"})
	if err != nil || res.Kind != CommandResultNoop || !strings.Contains(res.Message, "Nothing to undo") {
		t.Fatalf("second undo = %+v %v", res, err)
	}
}

func TestCommand_AddToSceneWritesCast(t *testing.T) {
	db, fx, s, _ := newCommandStore(t,
		mustJSON(map[string]any{"verb": "add_to_scene", "entity": "Tom the Innkeeper"}),
		mustJSON(map[string]any{"verb": "add_to_scene", "entity": "Tom the Innkeeper", "scene": "The Waystone at Night", "role": "focus"}),
	)
	ctx := context.Background()
	// A planned next session with two scenes.
	if _, err := db.Exec(`INSERT INTO game_sessions (id, campaign_id, ordinal, name, status, created_at, updated_at)
		VALUES ('sess-2', ?, 2, 'Session 2', 'planned', 0, 0)`, fx.Campaign.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO acts (id, campaign_id, ordinal, name, level_start, level_end, status, created_at, updated_at)
		VALUES ('act-1', ?, 1, 'Act I', 1, 3, 'planned', 0, 0)`, fx.Campaign.ID); err != nil {
		t.Fatal(err)
	}
	for i, sc := range []struct{ id, name, kind string }{
		{"scene-a", "The Road In", "travel"}, {"scene-b", "The Waystone at Night", "social"},
	} {
		if _, err := db.Exec(`INSERT INTO scenes (id, campaign_id, act_id, session_id, ordinal, kind, name, purpose, status, created_at, updated_at)
			VALUES (?, ?, 'act-1', 'sess-2', ?, ?, ?, '', 'planned', 0, 0)`, sc.id, fx.Campaign.ID, i+1, sc.kind, sc.name); err != nil {
			t.Fatal(err)
		}
	}

	// Several scenes and no scene named: ask which, with the scenes as
	// candidates, and stage nothing.
	res, err := s.RunCommand(ctx, CommandInput{CampaignID: fx.Campaign.ID, Text: "Add Tom to next session."})
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != CommandResultQuestion || !strings.Contains(res.Question.Question, "several scenes") {
		t.Fatalf("kind = %s question = %+v", res.Kind, res.Question)
	}
	if len(res.Question.Candidates) != 2 || res.Question.Candidates[1].ID != "scene-b" {
		t.Fatalf("candidates = %+v", res.Question.Candidates)
	}

	// Naming the scene writes the cast row — the spine write a DM makes.
	res, err = s.RunCommand(ctx, CommandInput{CampaignID: fx.Campaign.ID, Text: "Add Tom to the Waystone at Night scene, as the focus."})
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != CommandResultWritten {
		t.Fatalf("kind = %s (%s)", res.Kind, res.Message)
	}
	var role string
	if err := db.QueryRow(`SELECT role FROM scene_cast WHERE scene_id = 'scene-b' AND entity_id = ?`, fx.Tom).Scan(&role); err != nil {
		t.Fatalf("cast row missing: %v", err)
	}
	if role != "focus" {
		t.Fatalf("role = %s", role)
	}
}

func TestCommand_MergeNamesWritesAlias(t *testing.T) {
	db, fx, s, _ := newCommandStore(t, mustJSON(map[string]any{
		"verb": "merge_names", "entity": "Vess", "merge_into": "Tom the Innkeeper",
	}))
	res, err := s.RunCommand(context.Background(), CommandInput{
		CampaignID: fx.Campaign.ID, Text: "Merge Vess into Tom the Innkeeper.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != CommandResultWritten {
		t.Fatalf("kind = %s (%s)", res.Kind, res.Message)
	}
	if got := entityIDByName(t, db, fx.Campaign.ID, "Vess"); got != fx.Tom {
		t.Fatalf("Vess resolves to %s, want Tom %s", got, fx.Tom)
	}
}

func TestCommand_DuplicateRelationshipIsANoop(t *testing.T) {
	db, fx, s, _ := newCommandStore(t, mustJSON(map[string]any{
		"verb": "create_relationship", "rel_type": "located_in",
		"from_entity": "Tom the Innkeeper", "to_entity": "Blackwater",
	}))
	before := reviewCount(t, db)
	res, err := s.RunCommand(context.Background(), CommandInput{
		CampaignID: fx.Campaign.ID, Text: "Put Tom in Blackwater.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != CommandResultNoop || !strings.Contains(res.Message, "already exists") {
		t.Fatalf("kind = %s message = %s", res.Kind, res.Message)
	}
	if reviewCount(t, db) != before {
		t.Fatalf("a duplicate staged something")
	}
}

func TestCommand_CreateFactStagesSecret(t *testing.T) {
	db, fx, s, _ := newCommandStore(t, mustJSON(map[string]any{
		"verb": "create_fact", "subject": "Duke Aldric Vane",
		"statement": "The Duke keeps a ledger of every debt in the marches.",
		"predicate": "owns", "object_kind": "entity", "object_entity": "Eastern Mines",
		"visibility": "secret",
	}))
	res, err := s.RunCommand(context.Background(), CommandInput{
		CampaignID: fx.Campaign.ID, Text: "Record secretly that the Duke keeps a ledger of every debt.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != CommandResultBatch {
		t.Fatalf("kind = %s (%s)", res.Kind, res.Message)
	}
	if !strings.Contains(res.Message, "secret") {
		t.Fatalf("message = %s", res.Message)
	}
	item := res.Batch.Items[0]
	if item.Payload["visibility"] != "secret" || item.Payload["subject"] != fx.Duke || item.Payload["object_entity"] != fx.Mines {
		t.Fatalf("payload = %+v", item.Payload)
	}
	// Accepting lands the fact with ai_proposed provenance.
	if _, err := s.DecideBatch(context.Background(), fx.Campaign.ID, res.Batch.ID, DecisionAccept, nil, "keeper"); err != nil {
		t.Fatal(err)
	}
	var visibility string
	if err := db.QueryRow(`SELECT visibility FROM facts WHERE statement = ?`,
		"The Duke keeps a ledger of every debt in the marches.").Scan(&visibility); err != nil {
		t.Fatalf("fact missing: %v", err)
	}
	if visibility != "secret" {
		t.Fatalf("visibility = %s", visibility)
	}
}

func TestCommand_OfflineRefusesWithoutModel(t *testing.T) {
	db, fx, _ := seeded(t)
	campaigns, _ := campaign.New(db)
	knowledgeStore, _ := knowledge.New(db)
	offline, err := NewOffline(db)
	if err != nil {
		t.Fatal(err)
	}
	s := offline.WithGraphStores(campaigns, knowledgeStore)
	if _, err := s.RunCommand(context.Background(), CommandInput{
		CampaignID: fx.Campaign.ID, Text: "create Vess",
	}); !IsOffline(err) {
		t.Fatalf("err = %v, want offline", err)
	}
	// Undo still works offline: it never needs the model.
	if _, err := s.RunCommand(context.Background(), CommandInput{
		CampaignID: fx.Campaign.ID, Text: "undo",
	}); err != nil {
		t.Fatalf("offline undo: %v", err)
	}
}

func TestCommand_ValidationRepairPath(t *testing.T) {
	// A fill whose rel_type is outside the controlled vocabulary: the
	// harness retries once with the problem, the repair succeeds.
	bad := mustJSON(map[string]any{
		"verb": "create_relationship", "rel_type": "smuggles_for",
		"from_entity": "Tom the Innkeeper", "to_entity": "Blackwater",
	})
	_, fx, s, model := newCommandStore(t, bad, mustJSON(map[string]any{
		"verb": "create_relationship", "rel_type": "knows",
		"from_entity": "Tom the Innkeeper", "to_entity": "Blackwater",
	}))
	res, err := s.RunCommand(context.Background(), CommandInput{
		CampaignID: fx.Campaign.ID, Text: "Tom knows Blackwater's ways.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != CommandResultBatch {
		t.Fatalf("kind = %s (%s)", res.Kind, res.Message)
	}
	if len(model.calls) != 2 {
		t.Fatalf("model calls = %d, want 2 (the repair retry)", len(model.calls))
	}
}

func TestCommand_TranscriptOrdersNewestFirst(t *testing.T) {
	_, fx, s, _ := newCommandStore(t, scriptCreateVess(), mustJSON(map[string]any{"verb": "none"}))
	ctx := context.Background()
	if _, err := s.RunCommand(ctx, CommandInput{CampaignID: fx.Campaign.ID, Text: "create Vess"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunCommand(ctx, CommandInput{CampaignID: fx.Campaign.ID, Text: "hello there"}); err != nil {
		t.Fatal(err)
	}
	log, err := s.CommandLog(ctx, fx.Campaign.ID, 10)
	if err != nil || len(log) != 2 {
		t.Fatalf("log = %+v %v", log, err)
	}
	if log[0].Kind != CommandResultUnsupported || log[1].Kind != CommandResultBatch {
		t.Fatalf("order = %+v", log)
	}
	if log[1].Verb != CommandVerbCreateEntity {
		t.Fatalf("verb = %s", log[1].Verb)
	}
}

func TestCommand_Bounds(t *testing.T) {
	_, fx, s, _ := newCommandStore(t)
	if _, err := s.RunCommand(context.Background(), CommandInput{CampaignID: fx.Campaign.ID, Text: "  "}); err == nil {
		t.Fatal("empty command must be refused")
	}
	long := strings.Repeat("a", commandMaxLen+1)
	if _, err := s.RunCommand(context.Background(), CommandInput{CampaignID: fx.Campaign.ID, Text: long}); err == nil {
		t.Fatal("over-long command must be refused")
	}
	if _, err := s.RunCommand(context.Background(), CommandInput{CampaignID: "missing", Text: "undo"}); err == nil {
		t.Fatal("foreign campaign must be refused")
	}
}

func TestCommand_PromptCarriesTheClosedVocabulary(t *testing.T) {
	_, fx, s, model := newCommandStore(t, scriptCreateVess())
	if _, err := s.RunCommand(context.Background(), CommandInput{
		CampaignID: fx.Campaign.ID, Text: "create Vess",
	}); err != nil {
		t.Fatal(err)
	}
	if len(model.calls) != 1 {
		t.Fatalf("calls = %d", len(model.calls))
	}
	prompt := model.calls[0]
	for _, marker := range []string{
		"verb", "located_in", `"command"`, "Duke Aldric Vane", "npc",
		"predicates_in_use", "owns",
	} {
		if !strings.Contains(prompt, marker) {
			t.Errorf("prompt missing %q", marker)
		}
	}
	if strings.Contains(prompt, "SELECT") || strings.Contains(prompt, "INSERT") {
		t.Errorf("the prompt must carry no SQL")
	}
	_ = fmt.Sprint()
}
