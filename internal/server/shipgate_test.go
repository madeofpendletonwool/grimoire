package server

// The ship gate (MAD-491, stage 5 of MAD-319): the rehearsal that proves the
// player portal's three defenses at once, against one real campaign.
//
// The fixture is a seated player in a world built to be leaked: a central
// secret the party holds a granting awareness row on, a clue path that points
// at it without stating it, a retcon, a staged proposal, a private place
// truth, a draft handout that names the secret, and a player journal whose
// claim went through the real belief loop into a believes_false row. Every
// write lands through the real surfaces — the invite flow seats the player,
// the journal route takes their entry, the canon engine accepts the belief —
// so the state under test is the state production builds.
//
// The three gates, one per test:
//
//   - TestShipGate_PlayerViewReflectionSweep: every method on the final
//     PlayerView interface, fired by reflection at the seated player's scope,
//     scanned for the planted rows (layer 1 + layer 2 together).
//   - TestShipGate_PlayerJourney: login → seat → dossier → chat → journal →
//     belief flag → handout over HTTP, asserting on assembled prompts and
//     response payloads, never just outputs.
//   - TestShipGate_CanonCheckZeroSpoilerLeaks: `canon check` over the fixture
//     reports zero spoiler_leak findings — and fires one when the state is
//     corrupted, then clears when the DM repairs it (layer 3).
//
// TestShipGate_FixtureHasTeeth keeps the rest honest: the DM's surfaces DO
// carry the secret, so the player assertions are never vacuous.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/board"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/chat"
	"github.com/madeofpendletonwool/grimoire/internal/combat"
	"github.com/madeofpendletonwool/grimoire/internal/dice"
	"github.com/madeofpendletonwool/grimoire/internal/effects"
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/ledger"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/pubsub"
	"github.com/madeofpendletonwool/grimoire/internal/table"
	"github.com/madeofpendletonwool/grimoire/internal/testdb"
)

/* ---------- the harness ---------- */

// newShipGateServer wires the whole portal the way runServe does: the
// campaign graph with its knowledge layer, sessions and player journals,
// campaign chat against a capturing LLM, handouts, the canon engine, and the
// board's mechanical chain over one shared broker. One server, every layer
// the gate must exercise.
func newShipGateServer(t *testing.T, stub *capturingLLM) (*Server, *sql.DB) {
	t.Helper()
	store, err := index.Open(testdb.Path(t))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	users, err := auth.New(store.DB(), 0, 0)
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}
	campaigns, err := campaign.New(store.DB())
	if err != nil {
		t.Fatalf("open campaign store: %v", err)
	}
	knowledgeStore, err := knowledge.New(store.DB())
	if err != nil {
		t.Fatalf("open knowledge store: %v", err)
	}
	sessions, err := gamesession.New(store.DB())
	if err != nil {
		t.Fatalf("open session store: %v", err)
	}
	chats, err := chat.New(store.DB())
	if err != nil {
		t.Fatalf("open chat store: %v", err)
	}
	offlineCanon, err := canon.NewOffline(store.DB())
	if err != nil {
		t.Fatalf("open canon store: %v", err)
	}
	diceStore, err := dice.New(store.DB(), campaigns, sessions)
	if err != nil {
		t.Fatalf("open dice store: %v", err)
	}
	effectEngine, err := effects.New(store.DB(), campaigns, nil)
	if err != nil {
		t.Fatalf("open effects store: %v", err)
	}
	ledgerEngine, err := ledger.New(store.DB(), campaigns, offlineCanon)
	if err != nil {
		t.Fatalf("open ledger store: %v", err)
	}
	combatEngine, err := combat.New(store.DB(), campaigns, sessions, diceStore)
	if err != nil {
		t.Fatalf("open combat store: %v", err)
	}
	combatEngine = combatEngine.WithEffects(effectEngine).WithHitPoints(ledgerEngine).WithResolver(catalogShelf{})

	broker := pubsub.New()
	campaigns.WithBroker(broker)
	diceStore.WithBroker(broker)
	effectEngine.WithBroker(broker)
	ledgerEngine.WithBroker(broker)
	combatEngine.WithBroker(broker)

	boardStore, err := board.New(campaigns, ledgerEngine, effectEngine, combatEngine, users)
	if err != nil {
		t.Fatalf("open board store: %v", err)
	}
	boardStore.WithBroker(broker)
	tableScreen, err := table.New(store.DB(), campaigns, boardStore, combatEngine, table.PublicRolls(diceStore))
	if err != nil {
		t.Fatalf("open table screen store: %v", err)
	}
	tableScreen.WithBroker(broker)

	cfg := llm.Config{BaseURL: stub.baseURL, APIKey: "test-key", Model: "test-model"}
	s, err := New(store, llm.New(cfg), nil, nil, nil, chats, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithCampaign(campaigns, sessions).
		WithCampaigns(campaigns, knowledgeStore).
		WithCanon(offlineCanon).
		WithDice(diceStore).WithEffects(effectEngine).
		WithLedger(ledgerEngine).WithCombat(combatEngine).
		WithBoard(boardStore).WithTable(tableScreen).
		WithHandouts(HandoutOptions{Dir: t.TempDir()})
	return s, store.DB()
}

/* ---------- the rehearsal fixture ---------- */

// shipFixture is the campaign the gate plays on. Every field is something a
// lazy filter would leak: the secret the party has a grant on, the clue path
// pointing at it, the retcon, the proposal, the steward fact the journal
// disbelieves, the draft handout that names the secret, the private truth.
type shipFixture struct {
	campaignID string
	duke       string // the secret's subject
	pc         string // the seated player's bound character
	steward    string // the npc the journal is wrong about
	town       string // the location the dossier renders
	playerName string

	secretID     string // secret visibility, party granted knows — the drop-everything case
	publicID     string // party-known public fact (positive control)
	clueMirrors  string // clue path: public, known, points at the secret
	clueNoon     string // clue path: public, known, points at the secret
	clueCrypt    string // clue path: public, known, subject the town
	retconnedID  string // the superseded row the party learned before the retcon
	replacement  string // what reads serve instead
	proposedID   string // proposed + secret: invisible to every retrieval path
	loyalFactID  string // the steward canon fact the belief contradicts
	privateTruth string // the town payload's DM-only half

	sessionID string
	journalID string // the player-authored source the belief came from

	publishedHandoutID string
	draftHandoutID     string // names the secret; must never cross the scope line
}

// markers are the strings that must never appear on a player surface: the
// secret's text and id, the proposal's, the retcon's, the draft handout's,
// and the private truth. Asserted on raw bodies and assembled prompts alike.
func (f *shipFixture) markers() []string {
	return []string{
		"secretly a vampire", f.secretID,
		"a coffin beneath the keep", f.proposedID,
		"steward signs the mining ledgers", f.retconnedID,
		"The sealed letter", f.draftHandoutID, "do not show the party",
		f.privateTruth,
	}
}

// noLeak fails when any marker appears in a player-facing body or prompt.
func (f *shipFixture) noLeak(t *testing.T, where, body string) {
	t.Helper()
	for _, m := range f.markers() {
		if m != "" && strings.Contains(body, m) {
			t.Fatalf("LEAK: %s contains %q", where, m)
		}
	}
}

// asFixture widens the ship fixture into the campaign tests' fixture shape
// for the shared helpers (thread minting, asking, the board body) that only
// need the campaign and pc ids.
func (f *shipFixture) asFixture() fixture {
	return fixture{campaignID: f.campaignID, pcID: f.pc, dukeID: f.duke}
}

// shipJournalText is the entry the seated player writes through the real
// journal route. The belief's quote is a verbatim span of it.
const shipJournalText = `The dinner sat heavy. We finally discovered the steward is the monster of the keep. He never ages, and the black ledger is his.

I watched his hands when he poured the wine. Tomorrow we confront him at dawn.`

const shipPlaceBlock = `{
	"kind": "town",
	"scale": "large village",
	"population": "about 900",
	"government": "a merchant council",
	"services": ["inn", "market"],
	"defences": "a palisade and a watch of twelve",
	"climate": "temperate",
	"senses": ["gull noise", "damp wool"],
	"state": "flooding after the rains",
	"danger": 2,
	"private_truth": "the crypt beneath the well is the Duke's feeding crypt"
}`

// seedShipGateCampaign builds the rehearsal: the world and its secret
// architecture through DM surfaces, the player seated through the real
// invite flow with their character bound, a typed sheet and a live board,
// published and draft handouts, and the journal-plus-belief loop run to its
// accepted end. It returns the fixture; tests mint their own cookies.
func seedShipGateCampaign(t *testing.T, s *Server, db *sql.DB) *shipFixture {
	t.Helper()
	dm := adminSession(t, s)
	rec := hit(t, s, http.MethodPost, "/api/campaigns",
		`{"name":"The Ashen Court","system":"D&D 5e","premise":"A kingdom consumed by an ancient forest."}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create campaign: status %d, body %s", rec.Code, rec.Body)
	}
	cid := idFrom(t, rec, "campaign")
	f := &shipFixture{campaignID: cid, playerName: "mira", privateTruth: "the crypt beneath the well is the Duke's feeding crypt"}

	mk := func(kind, name string) string {
		body := `{"kind":` + quote(kind) + `,"name":` + quote(name) + `}`
		r := hit(t, s, http.MethodPost, "/api/campaigns/"+cid+"/entities", body, dm)
		if r.Code != http.StatusCreated {
			t.Fatalf("create %s: status %d, body %s", name, r.Code, r.Body)
		}
		return idFrom(t, r, "entity")
	}
	f.duke = mk("npc", "Duke Aldric Vane")
	f.pc = mk("pc", "Mira Thorn")
	f.steward = mk("npc", "Venn the Steward")
	f.town = mk("location", "Blackwater")

	// The town's authored interior: a public half the dossier renders and
	// a private truth no player surface may carry.
	if r := hit(t, s, http.MethodPatch, "/api/campaigns/"+cid+"/entities/"+f.town,
		`{"payload":{"place":`+shipPlaceBlock+`}}`, dm); r.Code != http.StatusOK {
		t.Fatalf("author the town: status %d, body %s", r.Code, r.Body)
	}

	mkFact := func(subject, predicate, object, statement, visibility string) string {
		body := `{"subject":` + quote(subject) + `,"predicate":` + quote(predicate) +
			`,"object_literal":` + quote(object) + `,"statement":` + quote(statement) +
			`,"visibility":` + quote(visibility) + `}`
		r := hit(t, s, http.MethodPost, "/api/campaigns/"+cid+"/facts", body, dm)
		if r.Code != http.StatusCreated {
			t.Fatalf("create fact %q: status %d, body %s", statement, r.Code, r.Body)
		}
		return idFrom(t, r, "fact")
	}

	// The central secret, the clue path that points at it, and the
	// steward's canon record the journal will disbelieve.
	f.secretID = mkFact(f.duke, "secretly_is", "a vampire", "The Duke is secretly a vampire.", "secret")
	f.publicID = mkFact(f.duke, "rules", "the northern marches", "The Duke rules the northern marches.", "public")
	f.clueMirrors = mkFact(f.duke, "keeps", "black lead mirrors", "The mirrors in the Duke's keep are all black lead.", "public")
	f.clueNoon = mkFact(f.duke, "avoids", "the noon hour", "The Duke has not been seen at noon in three years.", "public")
	f.clueCrypt = mkFact(f.town, "hides", "a sealed crypt", "Blackwater hides a crypt beneath the well, sealed with a silver lock.", "public")
	f.loyalFactID = mkFact(f.steward, "is", "a loyal and aging man", "Venn the steward is a loyal and aging man.", "public")

	// The retcon: the party learned the steward signs the ledgers, then the
	// DM replaced it. Superseded history is not current truth, learned or not.
	f.retconnedID = mkFact(f.steward, "signs", "the mining ledgers", "Venn the steward signs the mining ledgers.", "public")
	replacement := fmt.Sprintf(`{"subject":%q,"predicate":"countersigns","object_literal":"through a proxy in town","statement":"Venn the steward countersigns the mining ledgers through a proxy in town.","visibility":"public"}`, f.steward)
	r2 := hit(t, s, http.MethodPost, "/api/campaigns/"+cid+"/facts/"+f.retconnedID+"/supersede", replacement, dm)
	if r2.Code != http.StatusCreated {
		t.Fatalf("supersede: status %d, body %s", r2.Code, r2.Body)
	}
	f.replacement = idFrom(t, r2, "fact")

	// The staged proposal: secret AND proposed — doubly invisible. The API
	// does not write proposals (that is the pipeline's job), so this lands
	// through the store the way an extraction run would.
	proposed, err := s.campaigns.CreateFact(context.Background(), cid, f.duke, "keeps", "",
		"a coffin beneath the keep", "The Duke keeps a coffin beneath the keep.",
		campaign.ConfidenceProposed, campaign.VisibilitySecret, "keeper",
		[]campaign.ProvenanceInput{{
			SessionID: "seed", SourceID: "seed-transcript", SpanStart: 12, SpanEnd: 48,
			Quote:  "a coffin of black oak, nailed shut, beneath the keep",
			Method: campaign.MethodExtracted,
		}})
	if err != nil {
		t.Fatalf("plant proposal: %v", err)
	}
	f.proposedID = proposed.ID

	// Awareness: the party knows the public record, walked the whole clue
	// path, learned the steward's ledger habit before the retcon, and — the
	// load-bearing plant — HOLDS a granting row on the secret itself.
	grant := func(knower, factID string) {
		body := `{"knower":` + quote(knower) + `,"fact_id":` + quote(factID) + `,"stance":"knows"}`
		if r := hit(t, s, http.MethodPut, "/api/campaigns/"+cid+"/awareness", body, dm); r.Code != http.StatusOK {
			t.Fatalf("grant %s on %s: status %d, body %s", knower, factID, r.Code, r.Body)
		}
	}
	for _, fid := range []string{f.publicID, f.clueMirrors, f.clueNoon, f.clueCrypt, f.loyalFactID, f.retconnedID, f.replacement, f.secretID} {
		grant("party", fid)
	}

	// A witnessed event: the dinner where the clues were noticed. This is
	// also what makes the duke, the steward and the town met.
	ev := fmt.Sprintf(`{"summary":"The Duke invited the party to dinner.","clock_at":3,"participants":[{"entity_id":%q,"role":"host"},{"entity_id":%q},{"entity_id":%q},{"entity_id":%q}]}`,
		f.duke, f.steward, f.pc, f.town)
	if r := hit(t, s, http.MethodPost, "/api/campaigns/"+cid+"/events", ev, dm); r.Code != http.StatusCreated {
		t.Fatalf("create event: status %d, body %s", r.Code, r.Body)
	}

	// A quest one state in: the player's journal read shows the visited
	// state and never the unvisited branch. The DM marks it public — a
	// fresh quest is secret by default, exactly the rehearsal's point.
	qbody := `{"name":"Unmask the Duke","visibility":"public","state_machine":{"initial":"suspicious","states":["suspicious","confronted","undead_truth"],"edges":[{"from":"suspicious","to":"confronted"},{"from":"confronted","to":"undead_truth"}]}}`
	r3 := hit(t, s, http.MethodPost, "/api/campaigns/"+cid+"/quests", qbody, dm)
	if r3.Code != http.StatusCreated {
		t.Fatalf("create quest: status %d, body %s", r3.Code, r3.Body)
	}
	questID := idFrom(t, r3, "quest")
	if r := hit(t, s, http.MethodPost, "/api/campaigns/"+cid+"/quests/"+questID+"/transition", `{"to":"confronted"}`, dm); r.Code != http.StatusOK {
		t.Fatalf("quest transition: status %d, body %s", r.Code, r.Body)
	}

	// The seat: a real invite, minted by the DM, redeemed by a new account,
	// the character bound so the member's scope resolves to character:<id>.
	player := seatShipPlayer(t, s, f, dm)

	// The live board: a typed sheet on the pc and real damage on the hp
	// pool the ledger derived from it.
	sheet := `{"classes":[{"class":"wizard","level":5}],"ac":13,"max_hp":32,"spellcasting":{"slots":{"1":4,"2":3,"3":2}}}`
	if r := hit(t, s, http.MethodPut, "/api/campaigns/"+cid+"/characters/"+f.pc+"/sheet", sheet, dm); r.Code != http.StatusOK {
		t.Fatalf("put sheet: status %d, body %s", r.Code, r.Body)
	}
	pool := hpPoolID(t, s, f.asFixture(), dm)
	if r := hit(t, s, http.MethodPost, "/api/campaigns/"+cid+"/characters/"+f.pc+"/resources/"+pool+"/transactions",
		`{"kind":"spend","amount":7,"note":"the dinner went wrong"}`, dm); r.Code != http.StatusCreated {
		t.Fatalf("hp transaction: status %d, body %s", r.Code, r.Body)
	}

	// Handouts: one published letter (public material only) and one draft
	// whose very body is the secret.
	pub := hit(t, s, http.MethodPost, "/api/campaigns/"+cid+"/handouts",
		`{"kind":"handout","title":"A letter from the steward's desk","body":"The Duke thanks you for your service at dinner. The marches are quiet, and the crypt stays sealed."}`, dm)
	if pub.Code != http.StatusCreated {
		t.Fatalf("create handout: status %d, body %s", pub.Code, pub.Body)
	}
	f.publishedHandoutID = handoutFrom(t, pub)["id"].(string)
	if r := hit(t, s, http.MethodPost, "/api/campaigns/"+cid+"/handouts/"+f.publishedHandoutID+"/publish", "", dm); r.Code != http.StatusOK {
		t.Fatalf("publish handout: status %d, body %s", r.Code, r.Body)
	}
	draft := hit(t, s, http.MethodPost, "/api/campaigns/"+cid+"/handouts",
		`{"kind":"handout","title":"The sealed letter","body":"The Duke is a vampire — do not show the party."}`, dm)
	if draft.Code != http.StatusCreated {
		t.Fatalf("create draft: status %d, body %s", draft.Code, draft.Body)
	}
	f.draftHandoutID = handoutFrom(t, draft)["id"].(string)

	// The journal and the belief loop: the player writes the entry through
	// their route, then the DM's post-session canon run — a scripted model,
	// the real engine — stages the claim, pairs it against canon, and the
	// DM accepts. The result is an awareness row believes_false whose
	// provenance is the journal span, with canon untouched.
	f.sessionID = newJournalSession(t, s, cid, "The one with the dinner", dm)
	jr := hit(t, s, http.MethodPost, "/api/campaigns/"+cid+"/sessions/"+f.sessionID+"/journal",
		`{"title":"The ledger","content":`+quote(shipJournalText)+`}`, player)
	if jr.Code != http.StatusCreated {
		t.Fatalf("player journal write: status %d, body %s", jr.Code, jr.Body)
	}
	f.journalID = journalEntryFrom(t, jr)["id"].(string)
	runShipBeliefLoop(t, s, db, f)

	return f
}

// seatShipPlayer mints a campaign invite as the DM, registers a fresh
// account through it (the real redeem path), binds the fixture pc, and
// returns the new session cookie.
func seatShipPlayer(t *testing.T, s *Server, f *shipFixture, dm *http.Cookie) *http.Cookie {
	t.Helper()
	inv := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/invites", `{"role":"player"}`, dm)
	if inv.Code != http.StatusCreated {
		t.Fatalf("mint campaign invite: status %d, body %s", inv.Code, inv.Body)
	}
	code, _ := inviteCodeFrom(t, inv)
	reg := hit(t, s, http.MethodPost, "/api/auth/register",
		`{"username":`+quote(f.playerName)+`,"password":"a-fine-passphrase","invite":`+quote(code)+`}`)
	if reg.Code != http.StatusCreated {
		t.Fatalf("register player: status %d, body %s", reg.Code, reg.Body)
	}
	cookie := sessionFrom(t, reg)
	uid, err := s.users.LookupUseridByName(context.Background(), f.playerName)
	if err != nil {
		t.Fatalf("lookup player id: %v", err)
	}
	if r := hit(t, s, http.MethodPatch, "/api/campaigns/"+f.campaignID+"/members/"+uid,
		`{"character_id":`+quote(f.pc)+`}`, dm); r.Code != http.StatusOK {
		t.Fatalf("bind character: status %d, body %s", r.Code, r.Body)
	}
	return cookie
}

/* ---------- the belief loop, on the real engine ---------- */

// shipCanonModel is a scripted canon.ModelClient: it replays fixed responses
// and records every prompt — the canon package's in-test fakes, made local
// so the server rehearsal can drive the real engine deterministically.
type shipCanonModel struct {
	responses []string
	calls     []string
}

func (m *shipCanonModel) ModelName() string { return "ship-gate-script" }

func (m *shipCanonModel) Complete(ctx context.Context, system, user string) (canon.Completion, error) {
	i := len(m.calls)
	m.calls = append(m.calls, user)
	if i >= len(m.responses) {
		return canon.Completion{}, fmt.Errorf("script exhausted at call %d", i+1)
	}
	return canon.Completion{Text: m.responses[i], InputTokens: 100, OutputTokens: 200}, nil
}

// runShipBeliefLoop is the DM's post-session canon run over the player's
// journal: extraction stages the claim as a belief (never a fact), the
// adversarial pass agrees, the queue pairs the claim against canon, and the
// DM accepts. Fixture integrity is asserted here so every gate test plays
// on a campaign whose belief genuinely came from the journal.
func runShipBeliefLoop(t *testing.T, s *Server, db *sql.DB, f *shipFixture) {
	t.Helper()
	ctx := context.Background()
	extractor := &shipCanonModel{responses: []string{fmt.Sprintf(`{
	  "beliefs": [
	    {"local_id": "mira-believes-monster", "statement": "Venn the steward is the monster of the keep.",
	     "subject": %q, "predicate": "is", "object_entity": "", "object_literal": "the monster of the keep",
	     "discovered_by": %q, "stance": "knows",
	     "method": "he never ages",
	     "quote": "We finally discovered the steward is the monster of the keep.", "confidence": 0.9}
	  ]
	}`, f.steward, f.pc)}}
	validator := &shipCanonModel{responses: []string{
		`{"verdict":"agree","agreement":0.95,"rationale":"The span shows the author asserting the claim in their own voice.","proposed_confidence":null}`}}
	store, err := canon.NewWithValidator(db, extractor, validator, canon.Config{MaxCandidates: 500, BatchSize: 8, Interval: 0})
	if err != nil {
		t.Fatalf("canon store: %v", err)
	}
	store = store.WithGraphStores(s.campaigns, s.knowledge)

	if _, err := store.Extract(ctx, canon.ExtractInput{CampaignID: f.campaignID}); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if _, err := store.Validate(ctx, canon.ValidateInput{CampaignID: f.campaignID}); err != nil {
		t.Fatalf("validate: %v", err)
	}
	reviews, err := store.BuildQueue(ctx, f.campaignID)
	if err != nil {
		t.Fatalf("build queue: %v", err)
	}
	var reviewID string
	for _, r := range reviews {
		if r.Kind == canon.ReviewContradiction || r.Kind == canon.ReviewProposedBelief {
			reviewID = r.ID
		}
	}
	if reviewID == "" {
		t.Fatalf("no belief review in %+v; the journal claim never staged", reviews)
	}
	accepted, err := store.DecideReview(ctx, f.campaignID, reviewID, canon.DecisionAccept,
		"the journal says monster; canon says loyal — record the belief", "keeper", nil)
	if err != nil {
		t.Fatalf("decide review: %v", err)
	}
	if accepted.Status != canon.ReviewAccepted {
		t.Fatalf("accepted = %+v", accepted)
	}

	// Fixture integrity: the belief landed as awareness believes_false at
	// the author's scope, and its provenance is the journal span — the
	// player-written source, not a synthetic row.
	var stance string
	if err := db.QueryRow(`SELECT stance FROM awareness
		WHERE campaign_id = ? AND knower = ? AND fact_id = ?`,
		f.campaignID, f.pc, f.loyalFactID).Scan(&stance); err != nil {
		t.Fatalf("awareness row: %v", err)
	}
	if stance != knowledge.StanceBelievesFalse {
		t.Fatalf("stance = %q; want believes_false", stance)
	}
	var srcID string
	var spanStart, spanEnd int64
	if err := db.QueryRow(`SELECT source_id, COALESCE(span_start,0), COALESCE(span_end,0) FROM discoveries
		WHERE id = ?`, accepted.ResultRef).Scan(&srcID, &spanStart, &spanEnd); err != nil {
		t.Fatalf("discovery %s: %v", accepted.ResultRef, err)
	}
	if srcID != f.journalID {
		t.Fatalf("discovery source = %s; want the player journal %s", srcID, f.journalID)
	}
	if want := "We finally discovered the steward is the monster of the keep."; shipJournalText[spanStart:spanEnd] != want {
		t.Fatalf("discovery span = %q; want %q", shipJournalText[spanStart:spanEnd], want)
	}
}

/* ---------- the player cookie, re-minted per test ---------- */

// shipPlayerLogin is the journey's first step for real: sign in as the
// seated player.
func shipPlayerLogin(t *testing.T, s *Server, f *shipFixture) *http.Cookie {
	t.Helper()
	rec := hit(t, s, http.MethodPost, "/api/auth/login", credsJSON(f.playerName, "a-fine-passphrase"))
	if rec.Code != http.StatusOK {
		t.Fatalf("player login: status %d, body %s", rec.Code, rec.Body)
	}
	return sessionFrom(t, rec)
}

/* ---------- gate 1+2: the PlayerView reflection sweep ---------- */

// playerViewArgCandidates aims one string parameter at the fixture's
// planted rows. The view binds its scope at construction, so unlike the wide
// stores there is no scope argument — everything after the campaign id is
// aimed.
func playerViewArgCandidates(f *shipFixture) []string {
	return []string{
		f.secretID, f.proposedID, f.retconnedID, f.duke, f.steward, f.pc, f.town,
		f.publishedHandoutID, f.draftHandoutID, f.loyalFactID, f.publicID,
		campaign.PartyKnower, "vampire", "", "npc",
	}
}

// scanShipResults walks a reflection result for fact-shaped and
// handout-shaped rows — the leak test's scan narrowed to what this fixture
// plants.
func scanShipResults(v reflect.Value, depth int, facts *[]struct{ id, vis, conf string }, handouts *[]struct{ id, status string }) {
	if depth > 4 {
		return
	}
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface:
		if !v.IsNil() {
			scanShipResults(v.Elem(), depth+1, facts, handouts)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			scanShipResults(v.Index(i), depth+1, facts, handouts)
		}
	case reflect.Struct:
		t := v.Type()
		switch t.Name() {
		case "Fact":
			var row struct{ id, vis, conf string }
			for i := 0; i < t.NumField(); i++ {
				switch t.Field(i).Name {
				case "ID":
					row.id = v.Field(i).String()
				case "Visibility":
					row.vis = v.Field(i).String()
				case "Confidence":
					row.conf = v.Field(i).String()
				}
			}
			*facts = append(*facts, row)
			return
		case "Handout":
			var row struct{ id, status string }
			for i := 0; i < t.NumField(); i++ {
				switch t.Field(i).Name {
				case "ID":
					row.id = v.Field(i).String()
				case "Status":
					row.status = v.Field(i).String()
				}
			}
			*handouts = append(*handouts, row)
			return
		}
		for i := 0; i < t.NumField(); i++ {
			scanShipResults(v.Field(i), depth+1, facts, handouts)
		}
	}
}

// TestShipGate_PlayerViewReflectionSweep is layer 1 and layer 2 in one
// rehearsal: every method on the final PlayerView interface — enumerated by
// reflection, so a method added later joins automatically — fired at the
// seated player's own scope bindings, with arguments aimed at every planted
// row. No secret, no proposal, no retcon, no draft handout may appear in
// any successful result, marshaled or struct-scanned; and the sweep must
// really sweep (rows scanned, methods called) or it fails.
func TestShipGate_PlayerViewReflectionSweep(t *testing.T) {
	stub := newCapturingLLM(t, "ok")
	s, db := newShipGateServer(t, stub)
	f := seedShipGateCampaign(t, s, db)
	ctx := context.Background()

	views := map[string]knowledge.PlayerView{}
	for name, scope := range map[string]knowledge.Scope{
		"character": knowledge.ScopeCharacter(f.pc),
		"party":     knowledge.ScopeParty,
	} {
		v, err := s.knowledge.PlayerViewOf(scope)
		if err != nil {
			t.Fatalf("player view at %s scope: %v", name, err)
		}
		views[name] = v
	}

	iface := reflect.TypeOf((*knowledge.PlayerView)(nil)).Elem()
	strs := playerViewArgCandidates(f)
	filterCandidates := []knowledge.FactFilter{{}, {SubjectEntity: f.duke}, {Stance: knowledge.StanceKnows}}
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()

	called, rowCalls, factsScanned, handoutsScanned := 0, 0, 0, 0
	for _, view := range views {
		vv := reflect.ValueOf(view)
		for i := 0; i < iface.NumMethod(); i++ {
			m := iface.Method(i)
			ft := m.Type // interface methods carry no receiver
			if ft.NumIn() < 2 || ft.In(0) != ctxType {
				continue
			}
			// Every PlayerView method takes the campaign id right after
			// the context; the aimed candidates start after that.
			if ft.In(1).Kind() != reflect.String {
				t.Fatalf("PlayerView.%s does not take the campaign id at position 1; the sweep needs re-aiming", m.Name)
			}
			fn := vv.Method(i)
			positionSets := [][]reflect.Value{{reflect.ValueOf(f.campaignID)}}
			for p := 2; p < ft.NumIn(); p++ {
				switch {
				case ft.In(p).Kind() == reflect.String:
					set := make([]reflect.Value, len(strs))
					for j, s := range strs {
						set[j] = reflect.ValueOf(s)
					}
					positionSets = append(positionSets, set)
				case ft.In(p) == reflect.TypeOf(knowledge.FactFilter{}):
					set := make([]reflect.Value, len(filterCandidates))
					for j, fl := range filterCandidates {
						set[j] = reflect.ValueOf(fl)
					}
					positionSets = append(positionSets, set)
				case ft.In(p).Kind() == reflect.Int:
					positionSets = append(positionSets, []reflect.Value{reflect.ValueOf(20)})
				default:
					t.Fatalf("PlayerView.%s has a param shape the sweep does not know how to aim (%v)", m.Name, ft.In(p))
				}
			}
			var fire func(p int, acc []reflect.Value)
			fire = func(p int, acc []reflect.Value) {
				if p == len(positionSets) {
					args := append([]reflect.Value{reflect.ValueOf(ctx)}, acc...)
					results := fn.Call(args)
					called++
					errVal, _ := results[len(results)-1].Interface().(error)
					var facts []struct{ id, vis, conf string }
					var hs []struct{ id, status string }
					for _, r := range results[:len(results)-1] {
						scanShipResults(r, 0, &facts, &hs)
					}
					if errVal == nil && (len(facts) > 0 || len(hs) > 0) {
						rowCalls++
					}
					for _, row := range hs {
						handoutsScanned++
						if row.status != campaign.HandoutStatusPublished {
							t.Fatalf("LEAK: PlayerView.%s returned handout %s with status %q", m.Name, row.id, row.status)
						}
					}
					for _, row := range facts {
						factsScanned++
						if row.vis == campaign.VisibilitySecret {
							t.Fatalf("LEAK: PlayerView.%s returned secret-visibility fact %s", m.Name, row.id)
						}
						if row.conf == campaign.ConfidenceProposed {
							t.Fatalf("LEAK: PlayerView.%s returned proposed fact %s", m.Name, row.id)
						}
						if row.conf == campaign.ConfidenceRetconned {
							t.Fatalf("LEAK: PlayerView.%s returned retconned fact %s", m.Name, row.id)
						}
					}
					// The payload-level read: the marshaled result is the
					// wire shape a player surface would render.
					for _, r := range results[:len(results)-1] {
						b, err := json.Marshal(r.Interface())
						if err != nil {
							continue
						}
						f.noLeak(t, "PlayerView."+m.Name+" payload", string(b))
					}
					return
				}
				for _, c := range positionSets[p] {
					fire(p+1, append(acc, c))
				}
			}
			fire(0, nil)
		}
	}

	if called == 0 || factsScanned == 0 || handoutsScanned == 0 {
		t.Fatalf("the sweep is vacuous: called=%d row-calls=%d facts=%d handouts=%d", called, rowCalls, factsScanned, handoutsScanned)
	}
	t.Logf("ship gate sweep: %d calls (%d carrying rows), %d fact rows, %d handout rows", called, rowCalls, factsScanned, handoutsScanned)

	// Teeth: the DM's wide store over the same campaign does carry the
	// secret and the draft, and the proposal stays invisible even there —
	// so the sweep's emptiness above is the scope line working, not a
	// fixture that planted nothing.
	dmFacts, err := s.knowledge.Facts(ctx, knowledge.ScopeDM, f.campaignID, knowledge.FactFilter{})
	if err != nil {
		t.Fatal(err)
	}
	secretSeen := false
	for _, fact := range dmFacts {
		if fact.ID == f.secretID {
			secretSeen = true
		}
		if fact.ID == f.proposedID {
			t.Fatal("fixture integrity: the proposal is readable at dm scope through the knowledge store")
		}
	}
	if !secretSeen {
		t.Fatal("fixture integrity: the dm cannot read the secret; the sweep above is vacuous")
	}
	dmHandouts, err := s.knowledge.Handouts(ctx, knowledge.ScopeDM, f.campaignID, knowledge.HandoutFilter{Status: campaign.HandoutStatusDraft})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range dmHandouts {
		if h.ID == f.draftHandoutID {
			found = true
		}
	}
	if !found {
		t.Fatal("fixture integrity: the draft handout is not on the dm's desk; the handout assertions are vacuous")
	}
}

/* ---------- gate 3: canon check, zero spoiler leaks ---------- */

// TestShipGate_CanonCheckZeroSpoilerLeaks is layer 3: the deterministic
// check over the rehearsal reports no spoiler_leak anywhere — and when the
// awareness state is corrupted into the exact cross-knower contradiction
// the check exists for (the party recorded unaware, a pc's grant rendering
// the same fact on their portal), it fires; when the DM repairs the row, it
// clears.
func TestShipGate_CanonCheckZeroSpoilerLeaks(t *testing.T) {
	stub := newCapturingLLM(t, "ok")
	s, db := newShipGateServer(t, stub)
	f := seedShipGateCampaign(t, s, db)
	dm := dmSession(t, s)

	spoilerCount := func() int {
		rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/canon/check", "", dm)
		if rec.Code != http.StatusOK {
			t.Fatalf("canon check: status %d, body %s", rec.Code, rec.Body)
		}
		var body struct {
			Flags []struct {
				CheckCode string `json:"check_code"`
				Status    string `json:"status"`
			} `json:"flags"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode canon check: %v (%s)", err, rec.Body)
		}
		n := 0
		for _, fl := range body.Flags {
			if fl.CheckCode == canon.CheckSpoilerLeak && fl.Status == "open" {
				n++
			}
		}
		return n
	}

	if n := spoilerCount(); n != 0 {
		t.Fatalf("the rehearsal campaign reports %d spoiler_leak findings; the fixture itself leaks", n)
	}

	// Corrupt: a public fact only Mira's surface renders (her grant), while
	// the party row says they walked past it unaware.
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/facts",
		`{"subject":`+quote(f.town)+`,"predicate":"shelters","object_literal":"a hermit","statement":"Blackwater shelters a hermit who knows the Duke's hours.","visibility":"public"}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create hermit fact: status %d, body %s", rec.Code, rec.Body)
	}
	hermit := idFrom(t, rec, "fact")
	grant := func(knower, stance string) {
		body := `{"knower":` + quote(knower) + `,"fact_id":` + quote(hermit) + `,"stance":` + quote(stance) + `}`
		if r := hit(t, s, http.MethodPut, "/api/campaigns/"+f.campaignID+"/awareness", body, dm); r.Code != http.StatusOK {
			t.Fatalf("grant %s %s: status %d, body %s", knower, stance, r.Code, r.Body)
		}
	}
	grant(f.pc, "knows")
	grant("party", "unaware")
	if n := spoilerCount(); n == 0 {
		t.Fatal("teeth: a pc rendering a fact the party is unaware of produced no spoiler_leak finding")
	}

	// Repair: the DM records the party's exposure; the contradiction
	// clears without touching the pc's row.
	grant("party", "knows")
	if n := spoilerCount(); n != 0 {
		t.Fatalf("the repaired campaign still reports %d spoiler_leak findings", n)
	}
}

/* ---------- the journey ---------- */

// TestShipGate_PlayerJourney is the rehearsal the issue names: one seated
// player, every surface they can reach, in the order a session would touch
// them — login, seat, dossier, chat, journal, belief flag, handout — with
// the leak assertions on assembled contexts and payloads at every step, not
// just on outputs.
func TestShipGate_PlayerJourney(t *testing.T) {
	stub := newCapturingLLM(t, "You don't know that.")
	s, db := newShipGateServer(t, stub)
	f := seedShipGateCampaign(t, s, db)
	base := "/api/campaigns/" + f.campaignID
	af := f.asFixture()

	// Login: the seated player signs in.
	player := shipPlayerLogin(t, s, f)

	// Seat: the picker lists the campaign with the member's role, and the
	// campaign view agrees.
	rec := hit(t, s, http.MethodGet, "/api/campaigns", "", player)
	if rec.Code != http.StatusOK {
		t.Fatalf("list campaigns: status %d, body %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"my_role":"player"`) || !strings.Contains(rec.Body.String(), "The Ashen Court") {
		t.Fatalf("the player's campaign list does not seat them: %s", rec.Body)
	}
	f.noLeak(t, "campaign list", rec.Body.String())
	rec = hit(t, s, http.MethodGet, base, "", player)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"my_role":"player"`) {
		t.Fatalf("campaign view: status %d, body %s", rec.Code, rec.Body)
	}
	f.noLeak(t, "campaign view", rec.Body.String())

	// Dossier: the known world — entities, the Duke's bundle, facts, the
	// timeline, the quest journal, the location dossier, the sheet.
	rec = hit(t, s, http.MethodGet, base+"/entities", "", player)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Duke Aldric Vane") || !strings.Contains(rec.Body.String(), "Venn the Steward") {
		t.Fatalf("entities: status %d, body %s", rec.Code, rec.Body)
	}
	f.noLeak(t, "entities", rec.Body.String())

	rec = hit(t, s, http.MethodGet, base+"/entities/"+f.duke, "", player)
	if rec.Code != http.StatusOK {
		t.Fatalf("duke bundle: status %d, body %s", rec.Code, rec.Body)
	}
	for _, want := range []string{"The Duke rules the northern marches.", "black lead", "has not been seen at noon"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("the duke bundle is missing the party's own record (%q):\n%s", want, rec.Body)
		}
	}
	f.noLeak(t, "duke bundle", rec.Body.String())

	rec = hit(t, s, http.MethodGet, base+"/facts", "", player)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "The Duke rules the northern marches.") {
		t.Fatalf("facts: status %d, body %s", rec.Code, rec.Body)
	}
	f.noLeak(t, "facts", rec.Body.String())
	if rec := hit(t, s, http.MethodGet, base+"/facts/"+f.secretID, "", player); rec.Code != http.StatusNotFound {
		t.Fatalf("the secret by id at the player's scope: status %d, want 404", rec.Code)
	}

	rec = hit(t, s, http.MethodGet, base+"/timeline", "", player)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "invited the party to dinner") {
		t.Fatalf("timeline: status %d, body %s", rec.Code, rec.Body)
	}
	f.noLeak(t, "timeline", rec.Body.String())

	rec = hit(t, s, http.MethodGet, base+"/quests/journal", "", player)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Unmask the Duke") || !strings.Contains(rec.Body.String(), "confronted") {
		t.Fatalf("quest journal: status %d, body %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "undead_truth") {
		t.Fatal("LEAK: the player's quest journal shows an unvisited branch")
	}
	f.noLeak(t, "quest journal", rec.Body.String())

	rec = hit(t, s, http.MethodGet, base+"/locations/"+f.town, "", player)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "large village") {
		t.Fatalf("location dossier: status %d, body %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "private_truth") || strings.Contains(rec.Body.String(), `"routes"`) {
		t.Fatal("LEAK: the player's dossier carries dm payload shape")
	}
	f.noLeak(t, "location dossier", rec.Body.String())

	rec = hit(t, s, http.MethodGet, base+"/characters/"+f.pc+"/sheet", "", player)
	if rec.Code != http.StatusOK {
		t.Fatalf("sheet read: status %d, body %s", rec.Code, rec.Body)
	}
	var sheetBody struct {
		Sheet struct {
			MaxHP int `json:"max_hp"`
			AC    int `json:"ac"`
		} `json:"sheet"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sheetBody); err != nil || sheetBody.Sheet.MaxHP != 32 {
		t.Fatalf("sheet read: err=%v body=%s", err, rec.Body)
	}
	f.noLeak(t, "sheet read", rec.Body.String())

	// The board: live state, the player's own strip carrying the damage
	// the ledger recorded.
	board := boardBody(t, s, af, player)
	strip := stripOf(t, board, f.pc)
	if strip["hp"] != float64(25) || strip["max_hp"] != float64(32) {
		t.Fatalf("the player's board strip does not carry the live hp: %v", strip)
	}
	if raw, err := json.Marshal(board); err == nil {
		f.noLeak(t, "board", string(raw))
	}

	// Chat: the Player Grimoire. The assertion is on the assembled prompt
	// the model received, the citations riding the meta frame, and the
	// persisted thread — the context provably never held the secret.
	thread := newCampaignThread(t, s, af, player)
	code, sse := askCampaign(t, s, af, player, thread, "What is the Duke's dark secret?")
	if code != http.StatusOK {
		t.Fatalf("player ask: status %d, body %s", code, sse)
	}
	prompt := stub.lastBody()
	if prompt == "" {
		t.Fatal("the model was never called")
	}
	f.noLeak(t, "player grimoire prompt", prompt)
	if !strings.Contains(prompt, "do not know") {
		t.Fatal("the player system prompt lacks the don't-know discipline")
	}
	if strings.Contains(prompt, "secrets included") {
		t.Fatal("the player prompt carries the dm framing")
	}
	frames := campaignSSEFrames(sse)
	meta, ok := frames["meta"]
	if !ok {
		t.Fatalf("no meta frame in SSE body: %s", sse)
	}
	if raw, err := json.Marshal(meta); err == nil {
		f.noLeak(t, "sse meta frame", string(raw))
	}
	rec = hit(t, s, http.MethodGet, base+"/chats/"+thread, "", player)
	if rec.Code != http.StatusOK {
		t.Fatalf("get thread: status %d, body %s", rec.Code, rec.Body)
	}
	f.noLeak(t, "persisted thread", rec.Body.String())

	// Journal: the player's own record, written through their route and
	// read back; the session source list shows their entry and nothing
	// else a player may not hold.
	entry := hit(t, s, http.MethodPost, base+"/sessions/"+f.sessionID+"/journal",
		`{"title":"After the dinner","content":"We bought silver in the market. If the steward is what I think he is, dawn will tell."}`, player)
	if entry.Code != http.StatusCreated {
		t.Fatalf("player journal write: status %d, body %s", entry.Code, entry.Body)
	}
	rec = hit(t, s, http.MethodGet, base+"/sessions/"+f.sessionID+"/journal", "", player)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "After the dinner") || !strings.Contains(rec.Body.String(), "The ledger") {
		t.Fatalf("journal read: status %d, body %s", rec.Code, rec.Body)
	}
	f.noLeak(t, "journal read", rec.Body.String())
	rec = hit(t, s, http.MethodGet, base+"/sessions/"+f.sessionID+"/sources", "", player)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "player_journal") {
		t.Fatalf("sources list: status %d, body %s", rec.Code, rec.Body)
	}
	f.noLeak(t, "sources list", rec.Body.String())

	// The belief flag: the Player Grimoire's summary renders what Mira is
	// confidently wrong about — the steward belief the loop recorded from
	// her journal — and still never the secret. Asserted on the view's own
	// payload, the input to any rendering of it.
	view, err := s.knowledge.PlayerViewOf(knowledge.ScopeCharacter(f.pc))
	if err != nil {
		t.Fatal(err)
	}
	sum, err := view.Summarize(context.Background(), f.campaignID, f.steward)
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	beliefSeen := false
	var carried strings.Builder
	for _, bucket := range [][]campaign.Fact{sum.Confirmed, sum.Suspected, sum.Incorrect, sum.Unknown} {
		for _, fact := range bucket {
			carried.WriteString(fact.Statement)
			carried.WriteString(" ")
		}
	}
	for _, fact := range sum.Incorrect {
		if fact.ID == f.loyalFactID {
			beliefSeen = true
		}
	}
	if !beliefSeen {
		t.Fatalf("the steward belief never surfaced in the summary:\n%s", carried.String())
	}
	f.noLeak(t, "player summary", carried.String())

	// Handout: the published letter reads; the draft is a 404 by id and
	// absent from the list.
	rec = hit(t, s, http.MethodGet, base+"/handouts", "", player)
	if rec.Code != http.StatusOK {
		t.Fatalf("handouts list: status %d, body %s", rec.Code, rec.Body)
	}
	hs := handoutListFrom(t, rec)
	if len(hs) != 1 || hs[0]["id"] != f.publishedHandoutID {
		t.Fatalf("the player's handout list = %v; want exactly the published letter", hs)
	}
	rec = hit(t, s, http.MethodGet, base+"/handouts/"+f.publishedHandoutID, "", player)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "The Duke thanks you") {
		t.Fatalf("published handout: status %d, body %s", rec.Code, rec.Body)
	}
	f.noLeak(t, "published handout", rec.Body.String())
	if rec := hit(t, s, http.MethodGet, base+"/handouts/"+f.draftHandoutID, "", player); rec.Code != http.StatusNotFound {
		t.Fatalf("the draft by id at the player's scope: status %d, want 404", rec.Code)
	}
}

/* ---------- teeth ---------- */

// TestShipGate_FixtureHasTeeth proves the rehearsal planted what it thinks
// it planted: the DM's surfaces carry the secret, the draft handout and the
// retcon's history, so every player-side emptiness above is the scope line
// working and not an empty stage.
func TestShipGate_FixtureHasTeeth(t *testing.T) {
	stub := newCapturingLLM(t, "He is a vampire.")
	s, db := newShipGateServer(t, stub)
	f := seedShipGateCampaign(t, s, db)
	dm := dmSession(t, s)
	base := "/api/campaigns/" + f.campaignID
	af := f.asFixture()

	rec := hit(t, s, http.MethodGet, base+"/facts", "", dm)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "secretly a vampire") {
		t.Fatalf("the dm's fact list does not carry the secret: %s", rec.Body)
	}
	rec = hit(t, s, http.MethodGet, base+"/facts/"+f.secretID, "", dm)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "provenance") {
		t.Fatalf("the dm's fact detail: status %d, body %s", rec.Code, rec.Body)
	}
	if rec := hit(t, s, http.MethodGet, base+"/facts?superseded=1", "", dm); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Venn the steward signs the mining ledgers") {
		t.Fatalf("the dm cannot read retconned history on request: %s", rec.Body)
	}
	rec = hit(t, s, http.MethodGet, base+"/handouts", "", dm)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "The sealed letter") {
		t.Fatalf("the dm's desk does not carry the draft: %s", rec.Body)
	}

	thread := newCampaignThread(t, s, af, dm)
	code, _ := askCampaign(t, s, af, dm, thread, "What is the Duke's dark secret?")
	if code != http.StatusOK {
		t.Fatalf("dm ask: status %d", code)
	}
	prompt := stub.lastBody()
	if !strings.Contains(prompt, "secretly a vampire") || !strings.Contains(prompt, "(secret)") || !strings.Contains(prompt, "secrets included") {
		t.Fatalf("the dm prompt does not ground and mark the secret:\n%s", prompt)
	}
}
