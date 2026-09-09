package canon

// The belief loop (MAD-489): a player journal's claims become awareness, not
// facts. One test per drop rule over the pure validator, the queue's two
// belief shapes (agreeing → proposed_belief, contradicting → a contradiction
// pairing journal claim against canon), the accept writes (a discovery with
// the journal span as provenance and an awareness row at the author's
// stance), and the end-to-end acceptance run the issue names: the journal
// says the merchant is a vampire, canon says he is not, the DM accepts, and
// the Player Grimoire answers "what do we know about the merchant?" with the
// belief and still never the secret.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
)

/* ---------- fixtures ---------- */

const journalFixtureContent = `We finally discovered the merchant is a vampire. His ledger never sees daylight.

I keep the silver close. Tomorrow we test him at dawn.`

// beliefVctx is the drop-rule context for journals: Mira's journal, the
// merchant in the entity list, and the campaign's record of what he is.
func beliefVctx(sourceKind string) validateContext {
	content := journalFixtureContent
	return validateContext{
		sourceContent: content,
		chunk:         Chunk{Index: 0, Start: 0, End: int64(len(content)), Text: content},
		sourceKind:    sourceKind,
		sourceAuthor:  "mira-id",
		knownEntities: map[string]string{"mira-id": "pc", "merchant-id": "npc"},
		relTypes:      map[string]bool{"knows": true},
		facts: []promptFact{{
			ID: "fact-human", Subject: "merchant-id", Predicate: "is",
			ObjectLiteral: "a living human", Statement: "The merchant is a living human.",
			Visibility: "public",
		}},
	}
}

func beliefOf(mutate ...func(*WireBelief)) WireBelief {
	b := WireBelief{
		LocalID: "mira-believes-vampire", Statement: "The merchant is a vampire.",
		Subject: "merchant-id", Predicate: "is", ObjectLiteral: "a vampire",
		Stance: "believes_false", Method: "the ledger never sees daylight",
		Quote: "We finally discovered the merchant is a vampire.", Confidence: 0.9,
	}
	for _, m := range mutate {
		if m != nil {
			m(&b)
		}
	}
	return b
}

/* ---------- drop rules ---------- */

func TestBeliefRule_OnlyFromJournals(t *testing.T) {
	vctx := beliefVctx("transcript")
	staged, drops := validatePayload(WirePayload{Beliefs: []WireBelief{beliefOf()}}, vctx, map[string]bool{})
	if len(staged) != 0 {
		t.Fatalf("a transcript staged a belief: %+v", staged)
	}
	if dropReasons(drops)[DropBeliefNotJournal].Reason == "" {
		t.Fatalf("drops = %v; want belief_not_journal", drops)
	}
}

func TestBeliefRule_UnresolvedClaimDrops(t *testing.T) {
	vctx := beliefVctx(JournalSourceKind)
	_, drops := validatePayload(WirePayload{Beliefs: []WireBelief{
		beliefOf(func(b *WireBelief) { b.Predicate = "serves" }), // nothing recorded on merchant serves
	}}, vctx, map[string]bool{})
	if dropReasons(drops)[DropBeliefUnresolved].Reason == "" {
		t.Fatalf("drops = %v; want belief_unresolved", drops)
	}
}

func TestBeliefRule_StanceValidated(t *testing.T) {
	vctx := beliefVctx(JournalSourceKind)
	_, drops := validatePayload(WirePayload{Beliefs: []WireBelief{
		beliefOf(func(b *WireBelief) { b.Stance = "sure-hopes" }),
	}}, vctx, map[string]bool{})
	if dropReasons(drops)[DropBeliefInvalidStance].Reason == "" {
		t.Fatalf("drops = %v; want belief_invalid_stance", drops)
	}
}

func TestBeliefRule_QuoteRuleApplies(t *testing.T) {
	vctx := beliefVctx(JournalSourceKind)
	_, drops := validatePayload(WirePayload{Beliefs: []WireBelief{
		beliefOf(func(b *WireBelief) { b.Quote = "words that appear nowhere in the journal" }),
	}}, vctx, map[string]bool{})
	if dropReasons(drops)[DropQuoteNotInSource].Reason == "" {
		t.Fatalf("drops = %v; want quote_not_in_source", drops)
	}
}

func TestBeliefRule_KnowerDefaultsToAuthorAndMustResolve(t *testing.T) {
	// Empty discovered_by falls back to the journal's author.
	vctx := beliefVctx(JournalSourceKind)
	staged, drops := validatePayload(WirePayload{Beliefs: []WireBelief{
		beliefOf(func(b *WireBelief) { b.DiscoveredBy = "" }),
	}}, vctx, map[string]bool{})
	if len(staged) != 1 || staged[0].Kind != KindBelief {
		t.Fatalf("author default: staged=%+v drops=%v", staged, drops)
	}
	var p map[string]any
	if err := json.Unmarshal(staged[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p["discovered_by"] != "mira-id" {
		t.Fatalf("discovered_by = %v; want the journal's author", p["discovered_by"])
	}

	// A knower that resolves nowhere is dropped, not guessed.
	vctx2 := beliefVctx(JournalSourceKind)
	vctx2.sourceAuthor = "someone-not-in-the-entity-list"
	_, drops2 := validatePayload(WirePayload{Beliefs: []WireBelief{
		beliefOf(func(b *WireBelief) { b.DiscoveredBy = "" }),
	}}, vctx2, map[string]bool{})
	if dropReasons(drops2)[DropUnknownEntity].Reason == "" {
		t.Fatalf("drops = %v; want unknown_entity for the knower", drops2)
	}
}

func TestBeliefRule_ContradictionCoercesKnowsToBelievesFalse(t *testing.T) {
	// The journal speaks the contradicting claim as discovered truth
	// ("we finally discovered"); the honest record is believes_false on
	// the canon fact, whatever stance the model offered.
	vctx := beliefVctx(JournalSourceKind)
	staged, drops := validatePayload(WirePayload{Beliefs: []WireBelief{
		beliefOf(func(b *WireBelief) { b.Stance = "knows" }),
	}}, vctx, map[string]bool{})
	if len(staged) != 1 {
		t.Fatalf("staged=%d drops=%v", len(staged), drops)
	}
	var p map[string]any
	if err := json.Unmarshal(staged[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p["stance"] != "believes_false" || p["contradicts"] != true || p["fact"] != "fact-human" {
		t.Fatalf("belief payload = %v", p)
	}
	if p["claim"] != "The merchant is a vampire." {
		t.Fatalf("claim = %v", p["claim"])
	}
}

func TestBeliefRule_AgreeingClaimKeepsItsStance(t *testing.T) {
	vctx := beliefVctx(JournalSourceKind)
	staged, drops := validatePayload(WirePayload{Beliefs: []WireBelief{
		beliefOf(func(b *WireBelief) {
			b.ObjectLiteral = "a living human"
			b.Stance = "suspects"
			b.Statement = "The merchant is a living human."
		}),
	}}, vctx, map[string]bool{})
	if len(staged) != 1 {
		t.Fatalf("staged=%d drops=%v", len(staged), drops)
	}
	var p map[string]any
	_ = json.Unmarshal(staged[0].Payload, &p)
	if p["stance"] != "suspects" || p["contradicts"] != false {
		t.Fatalf("agreeing belief payload = %v", p)
	}
}

/* ---------- the end-to-end belief loop ---------- */

// beliefStage is the e2e fixture stack: the seed campaign, a merchant with a
// public nature fact and a secret, Mira's journal against session one, and a
// canon store wired for review.
type beliefStage struct {
	db         *sql.DB
	store      *Store
	extractor  *fakeModel
	fx         *campaign.Fixture
	campaignID string
	sessionID  string
	merchant   string
	publicFact string
	secretFact string
	journalID  string
}

func beliefAgreeValidator() *uniformVerdictModel {
	return &uniformVerdictModel{response: `{"verdict":"agree","agreement":0.95,"rationale":"The span shows the author asserting the claim in their own voice.","proposed_confidence":null}`}
}

// seedBeliefStage builds the campaign the acceptance run plays on. The
// journal's belief quotes are verbatim substrings of journalText.
const journalText = `We finally discovered the merchant is a vampire. The ledger never sees daylight and neither does he.

I keep the silver close tonight. Tomorrow we test him at dawn.`

func seedBeliefStage(t *testing.T, buildResponse func(fx *campaign.Fixture, merchantID string) string) *beliefStage {
	t.Helper()
	db, fx, sessionID := seeded(t)
	ctx := context.Background()

	cs, err := campaign.New(db)
	if err != nil {
		t.Fatal(err)
	}
	merchant, err := cs.CreateEntity(ctx, fx.Campaign.ID, campaign.KindNPC,
		"Sela the Merchant", "A spice trader with pale, careful hands.", nil)
	if err != nil {
		t.Fatal(err)
	}
	publicFact, err := cs.CreateFact(ctx, fx.Campaign.ID, merchant.ID, "is", "", "a living human",
		"Sela the merchant is a living human.", campaign.ConfidenceCanon, campaign.VisibilityPublic,
		"keeper", []campaign.ProvenanceInput{{Method: campaign.MethodDMAuthored}})
	if err != nil {
		t.Fatal(err)
	}
	secretFact, err := cs.CreateFact(ctx, fx.Campaign.ID, merchant.ID, "hunts", "", "the cultists who burned his caravan",
		"Sela secretly hunts the cultists who burned his caravan.", campaign.ConfidenceCanon,
		campaign.VisibilitySecret, "keeper", []campaign.ProvenanceInput{{Method: campaign.MethodDMAuthored}})
	if err != nil {
		t.Fatal(err)
	}

	// Mira's journal: the player-write the routes would have landed, with
	// the author bound server-side to her character.
	sum := sha256.Sum256([]byte(journalText))
	journalID := "journal-mira-1"
	if _, err := db.Exec(`
		INSERT INTO session_sources (id, session_id, kind, author, title, content, checksum, created_at)
		VALUES (?, ?, 'player_journal', ?, 'The one with the merchant', ?, ?, 0)`,
		journalID, sessionID, fx.Mira, journalText, hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("insert journal: %v", err)
	}

	var responses []string
	if buildResponse != nil {
		responses = []string{buildResponse(fx, merchant.ID)}
	}
	extractor := &fakeModel{responses: responses}
	ks, err := knowledge.New(db)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewWithValidator(db, extractor, beliefAgreeValidator(), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	store = store.WithGraphStores(cs, ks)
	return &beliefStage{
		db: db, store: store, extractor: extractor, fx: fx,
		campaignID: fx.Campaign.ID, sessionID: sessionID,
		merchant: merchant.ID, publicFact: publicFact.ID, secretFact: secretFact.ID,
		journalID: journalID,
	}
}

func journalResponse(fx *campaign.Fixture, merchantID string) string {
	return fmt.Sprintf(`{
  "beliefs": [
    {"local_id": "mira-believes-vampire", "statement": "Sela the merchant is a vampire.",
     "subject": %q, "predicate": "is", "object_entity": "", "object_literal": "a vampire",
     "discovered_by": %q, "stance": "knows",
     "method": "the ledger never sees daylight",
     "quote": "We finally discovered the merchant is a vampire.", "confidence": 0.9}
  ]
}`, merchantID, fx.Mira)
}

func TestBelief_EndToEndContradiction(t *testing.T) {
	st := seedBeliefStage(t, func(fx *campaign.Fixture, merchantID string) string {
		return journalResponse(fx, merchantID)
	})
	ctx := context.Background()

	// 1. Extraction: the journal's claim stages as a belief, not a fact —
	// and the prompt carried the campaign's record and the author.
	run, err := st.store.Extract(ctx, ExtractInput{CampaignID: st.campaignID})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if run.Status != RunCompleted {
		t.Fatalf("run = %+v", run)
	}
	cands, err := st.store.ListCandidates(ctx, st.campaignID, CandidateFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Kind != KindBelief {
		t.Fatalf("candidates = %+v; want exactly one belief", cands)
	}
	var nFacts int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM facts WHERE campaign_id = ?`, st.campaignID).Scan(&nFacts); err != nil {
		t.Fatal(err)
	}
	if nFacts < 3 {
		// The seed's facts plus the merchant's two — the belief wrote nothing.
		t.Fatalf("facts = %d", nFacts)
	}
	if len(st.extractor.calls) != 1 {
		t.Fatalf("extractor calls = %d", len(st.extractor.calls))
	}
	prompt := st.extractor.calls[0]
	for _, want := range []string{
		"PLAYER'S JOURNAL", "CAMPAIGN FACTS", "a living human", st.merchant, st.fx.Mira,
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("journal prompt missing %q", want)
		}
	}

	// 2. The adversarial pass, then the queue: one contradiction item
	// pairing the claim against canon.
	if _, err := st.store.Validate(ctx, ValidateInput{CampaignID: st.campaignID}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	reviews, err := st.store.BuildQueue(ctx, st.campaignID)
	if err != nil {
		t.Fatalf("BuildQueue: %v", err)
	}
	var contradiction *Review
	for i := range reviews {
		if reviews[i].Kind == ReviewContradiction {
			contradiction = &reviews[i]
		}
	}
	if contradiction == nil {
		t.Fatalf("reviews = %+v; want a contradiction item", reviews)
	}
	if !strings.Contains(contradiction.Summary, "a vampire") || !strings.Contains(contradiction.Summary, "a living human") {
		t.Fatalf("contradiction summary = %q; want claim vs canon", contradiction.Summary)
	}
	var detail struct {
		Kind  string   `json:"kind"`
		Fact  string   `json:"fact"`
		Sides []string `json:"sides"`
	}
	if err := json.Unmarshal([]byte(contradiction.Detail), &detail); err != nil || detail.Kind != "belief" ||
		detail.Fact != st.publicFact || len(detail.Sides) != 1 {
		t.Fatalf("contradiction detail = %s (%v)", contradiction.Detail, err)
	}

	// 3. The DM accepts: awareness believes_false whose provenance points
	// at the journal span; canon untouched; no correction anywhere.
	accepted, err := st.store.DecideReview(ctx, st.campaignID, contradiction.ID, DecisionAccept,
		"the journal says vampire; canon says human — record the belief", "keeper", nil)
	if err != nil {
		t.Fatalf("DecideReview: %v", err)
	}
	if accepted.Status != ReviewAccepted || accepted.ResultRef == "" {
		t.Fatalf("accepted = %+v", accepted)
	}
	var stance string
	if err := st.db.QueryRow(`SELECT stance FROM awareness
		WHERE campaign_id = ? AND knower = ? AND fact_id = ?`,
		st.campaignID, st.fx.Mira, st.publicFact).Scan(&stance); err != nil {
		t.Fatalf("awareness row: %v", err)
	}
	if stance != "believes_false" {
		t.Fatalf("stance = %q; want believes_false", stance)
	}
	var srcID string
	var spanStart, spanEnd int64
	if err := st.db.QueryRow(`SELECT source_id, COALESCE(span_start,0), COALESCE(span_end,0) FROM discoveries
		WHERE id = ?`, accepted.ResultRef).Scan(&srcID, &spanStart, &spanEnd); err != nil {
		t.Fatalf("discovery %s: %v", accepted.ResultRef, err)
	}
	if srcID != st.journalID {
		t.Fatalf("discovery source = %s; want the journal", srcID)
	}
	if want := "We finally discovered the merchant is a vampire."; journalText[spanStart:spanEnd] != want {
		t.Fatalf("discovery span = %q; want %q", journalText[spanStart:spanEnd], want)
	}

	// 4. The Player Grimoire answers "what do we know about the merchant?"
	// with the belief — and still never the secret.
	ks := beliefKnowledgeStore(t, st)
	view, err := ks.PlayerViewOf(knowledge.ScopeCharacter(st.fx.Mira))
	if err != nil {
		t.Fatal(err)
	}
	sum, err := view.Summarize(ctx, st.campaignID, st.merchant)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	found := false
	for _, f := range sum.Incorrect {
		if f.ID == st.publicFact {
			found = true
		}
	}
	if !found {
		t.Fatalf("summary.Incorrect = %+v; want the public fact the belief contradicts", sum.Incorrect)
	}
	var leak strings.Builder
	for _, bucket := range [][]campaign.Fact{sum.Confirmed, sum.Suspected, sum.Incorrect, sum.Unknown} {
		for _, f := range bucket {
			leak.WriteString(f.Statement)
			leak.WriteString(" ")
		}
	}
	if strings.Contains(leak.String(), "cultists") {
		t.Fatalf("LEAK: the Player Grimoire surfaced the secret: %s", leak.String())
	}
}

func TestBelief_AgreeingClaimBecomesProposedBelief(t *testing.T) {
	// The other shape: the journal's claim AGREES with canon. The queue
	// item is a proposed_belief, and accepting records a discovery at the
	// claimed stance — the character learned the true thing.
	st := seedBeliefStage(t, func(fx *campaign.Fixture, merchantID string) string {
		return fmt.Sprintf(`{
  "beliefs": [
    {"local_id": "mira-suspects-human", "statement": "Sela is a living human.",
     "subject": %q, "predicate": "is", "object_entity": "", "object_literal": "a living human",
     "discovered_by": %q, "stance": "suspects",
     "method": "I finally looked at him in daylight",
     "quote": "Tomorrow we test him at dawn.", "confidence": 0.8}
  ]
}`, merchantID, fx.Mira)
	})
	ctx := context.Background()

	if _, err := st.store.Extract(ctx, ExtractInput{CampaignID: st.campaignID}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if _, err := st.store.Validate(ctx, ValidateInput{CampaignID: st.campaignID}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	reviews, err := st.store.BuildQueue(ctx, st.campaignID)
	if err != nil {
		t.Fatalf("BuildQueue: %v", err)
	}
	if len(reviews) != 1 || reviews[0].Kind != ReviewProposedBelief {
		t.Fatalf("reviews = %+v; want one proposed_belief", reviews)
	}
	accepted, err := st.store.DecideReview(ctx, st.campaignID, reviews[0].ID, DecisionAccept, "yes", "keeper", nil)
	if err != nil {
		t.Fatalf("DecideReview: %v", err)
	}
	if accepted.Status != ReviewAccepted {
		t.Fatalf("accepted = %+v", accepted)
	}
	var stance string
	if err := st.db.QueryRow(`SELECT stance FROM awareness
		WHERE campaign_id = ? AND knower = ? AND fact_id = ?`,
		st.campaignID, st.fx.Mira, st.publicFact).Scan(&stance); err != nil {
		t.Fatal(err)
	}
	if stance != "suspects" {
		t.Fatalf("stance = %q; want suspects", stance)
	}
}

func TestBelief_DismissLeavesJournalStanding(t *testing.T) {
	// The player is never corrected: dismissing the contradiction records
	// no belief at all, and the journal stands as written.
	st := seedBeliefStage(t, func(fx *campaign.Fixture, merchantID string) string {
		return journalResponse(fx, merchantID)
	})
	ctx := context.Background()
	if _, err := st.store.Extract(ctx, ExtractInput{CampaignID: st.campaignID}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.store.Validate(ctx, ValidateInput{CampaignID: st.campaignID}); err != nil {
		t.Fatal(err)
	}
	reviews, err := st.store.BuildQueue(ctx, st.campaignID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reviews) != 1 {
		t.Fatalf("reviews = %+v", reviews)
	}
	if _, err := st.store.DecideReview(ctx, st.campaignID, reviews[0].ID, DecisionDismiss, "not canon material", "keeper", nil); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM awareness WHERE campaign_id = ?`, st.campaignID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("dismissed contradiction wrote %d awareness rows", n)
	}
	var content string
	if err := st.db.QueryRow(`SELECT content FROM session_sources WHERE id = ?`, st.journalID).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if content != journalText {
		t.Fatalf("the journal was mutated")
	}
}

/* ---------- helpers ---------- */

func beliefKnowledgeStore(t *testing.T, st *beliefStage) *knowledge.Store {
	t.Helper()
	ks, err := knowledge.New(st.db)
	if err != nil {
		t.Fatal(err)
	}
	return ks
}
