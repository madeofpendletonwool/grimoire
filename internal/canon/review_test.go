package canon

// The review queue (MAD-310): the human gate's acceptance run, the way Arda's
// test_review_queue.py works — transcript in, extraction, adversarial pass,
// queue build, a human accept, and the fact is canon with provenance pointing
// at the right span and retrievable at the right scopes and no others. Plus
// the queue semantics: a decided item keeps its decision forever and a
// re-proposed finding opens a NEW item rather than resurrecting.

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
)

// reviewStore builds a canon store with the graph and knowledge stores wired,
// so the review queue can write canon on accept.
func reviewStore(t *testing.T, db *sql.DB, extractor, validator ModelClient) *Store {
	t.Helper()
	s, err := NewWithValidator(db, extractor, validator, testConfig())
	if err != nil {
		t.Fatalf("canon store: %v", err)
	}
	cs, err := campaign.New(db)
	if err != nil {
		t.Fatal(err)
	}
	ks, err := knowledge.New(db)
	if err != nil {
		t.Fatal(err)
	}
	return s.WithGraphStores(cs, ks)
}

// queueFixture runs extraction and the adversarial pass over the fixture
// transcript and builds the queue, returning the store, campaign id and the
// queue itself.
func queueFixture(t *testing.T) (*Store, string, []Review) {
	t.Helper()
	db, campaignID, _ := extractFixture(t)
	ctx := context.Background()

	validator := &verdictModel{responses: fixtureVerdicts()}
	s := reviewStore(t, db, &fakeModel{}, validator)
	if _, err := s.Validate(ctx, ValidateInput{CampaignID: campaignID}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	reviews, err := s.BuildQueue(ctx, campaignID)
	if err != nil {
		t.Fatalf("BuildQueue: %v", err)
	}
	return s, campaignID, reviews
}

func reviewByKind(reviews []Review, kind string) *Review {
	for i := range reviews {
		if reviews[i].Kind == kind {
			return &reviews[i]
		}
	}
	return nil
}

func TestReview_BuildQueueFromValidatedCandidates(t *testing.T) {
	_, _, reviews := queueFixture(t)

	// The fixture verdicts exercise four queue outcomes: the entity agrees,
	// the fact downgrades (still proposed), the discovery flags (low
	// agreement), the relationship downgrades; the event's verdict is a
	// rejected upgrade and must NOT reach the queue.
	if len(reviews) != 4 {
		t.Fatalf("reviews = %d, want 4 (kinds %v)", len(reviews), kinds(reviews))
	}
	for _, kind := range []string{
		ReviewProposedEntity, ReviewProposedFact, ReviewLowAgreement, ReviewProposedRelationship,
	} {
		if reviewByKind(reviews, kind) == nil {
			t.Fatalf("missing review kind %q in %v", kind, kinds(reviews))
		}
	}

	// The fact item carries its rendering material: statement, verbatim
	// quote, span, source identity, context, and the adversarial verdict.
	fact := reviewByKind(reviews, ReviewProposedFact)
	if fact.Summary != "The Duke's men passed through Blackwater twice this month." {
		t.Fatalf("fact summary = %q", fact.Summary)
	}
	if fact.Quote != "The Duke's men came through twice this month." {
		t.Fatalf("fact quote = %q", fact.Quote)
	}
	if fact.SourceKind != "transcript" || fact.SourceAuthor != "DM" {
		t.Fatalf("fact source = %q/%q", fact.SourceKind, fact.SourceAuthor)
	}
	if !strings.Contains(fact.ContextBefore, "The party enters the Waystone Inn") {
		t.Fatalf("context before missing: %.120s", fact.ContextBefore)
	}
	if fact.Verdict != VerdictDowngrade || fact.Agreement != 0.9 {
		t.Fatalf("fact verdict = %q agreement %v", fact.Verdict, fact.Agreement)
	}
	if fact.Confidence != 0.5 {
		t.Fatalf("fact confidence after downgrade = %v, want 0.5", fact.Confidence)
	}
	if fact.Rationale == "" {
		t.Fatal("fact rationale empty")
	}

	// The low-agreement item is the discovery the checker could not decide.
	low := reviewByKind(reviews, ReviewLowAgreement)
	if low.CandidateID == "" {
		t.Fatal("low agreement item lost its candidate")
	}
	if _, ok := low.Payload["discovered_by"]; !ok {
		t.Fatalf("low agreement payload is not a discovery: %v", low.Payload)
	}
}

func TestReview_AcceptFactWritesCanon(t *testing.T) {
	s, campaignID, reviews := queueFixture(t)
	ctx := context.Background()

	fact := reviewByKind(reviews, ReviewProposedFact)
	accepted, err := s.DecideReview(ctx, campaignID, fact.ID, DecisionAccept, "looks right", "keeper", nil)
	if err != nil {
		t.Fatalf("DecideReview accept: %v", err)
	}
	if accepted.Status != ReviewAccepted || accepted.DecidedBy != "keeper" {
		t.Fatalf("accepted review = %+v", accepted)
	}
	if accepted.ResultRef == "" {
		t.Fatal("accept recorded no result_ref")
	}

	// The fact exists at canon confidence with extracted provenance pointing
	// at the candidate's span, and the provenance records who accepted and
	// when.
	cs, err := campaign.New(s.db)
	if err != nil {
		t.Fatal(err)
	}
	f, err := cs.GetFact(ctx, campaign.ScopeDM, campaignID, accepted.ResultRef)
	if err != nil {
		t.Fatalf("GetFact: %v", err)
	}
	if f.Confidence != campaign.ConfidenceCanon {
		t.Fatalf("confidence = %q, want canon", f.Confidence)
	}
	if f.Visibility != campaign.VisibilityPublic {
		t.Fatalf("visibility = %q", f.Visibility)
	}
	if f.SubjectEntity == "" || f.Predicate != "visited" {
		t.Fatalf("fact = %+v", f)
	}
	prov, err := cs.FactProvenance(ctx, campaign.ScopeDM, campaignID, accepted.ResultRef)
	if err != nil || len(prov) != 1 {
		t.Fatalf("provenance = %+v err %v", prov, err)
	}
	p := prov[0]
	if p.Method != campaign.MethodExtracted || p.SessionID == "" || p.SourceID == "" {
		t.Fatalf("provenance = %+v", p)
	}
	if p.AcceptedBy != "keeper" || p.AcceptedAt.IsZero() {
		t.Fatalf("acceptance not recorded on provenance: %+v", p)
	}
	if p.SpanStart != fact.SpanStart || p.SpanEnd != fact.SpanEnd || p.Quote != fact.Quote {
		t.Fatalf("provenance span = %d..%d %q, want %d..%d %q",
			p.SpanStart, p.SpanEnd, p.Quote, fact.SpanStart, fact.SpanEnd, fact.Quote)
	}

	// The span resolves back to the quote through the session layer — the
	// span rule's round trip after acceptance.
	var content string
	if err := s.db.QueryRow(`SELECT content FROM session_sources WHERE id = ?`, p.SourceID).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if content[p.SpanStart:p.SpanEnd] != p.Quote {
		t.Fatalf("accepted provenance span does not resolve to its quote")
	}

	// The DM scope retrieves it; the party scope does not — the fact is canon
	// but nothing has granted the party awareness of it yet.
	ks, err := knowledge.New(s.db)
	if err != nil {
		t.Fatal(err)
	}
	dmFacts, err := ks.Facts(ctx, knowledge.ScopeDM, campaignID, knowledge.FactFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if !factsContain(dmFacts, "twice this month") {
		t.Fatal("accepted fact not retrievable at the DM scope")
	}
	partyFacts, err := ks.Facts(ctx, knowledge.ScopeParty, campaignID, knowledge.FactFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if factsContain(partyFacts, "twice this month") {
		t.Fatal("accepted fact leaked to the party scope before any discovery granted it")
	}

	// Re-deciding the same item is refused: a decided item keeps its decision.
	if _, err := s.DecideReview(ctx, campaignID, fact.ID, DecisionDismiss, "", "keeper", nil); err == nil {
		t.Fatal("re-deciding a decided item must fail")
	}
}

func TestReview_AcceptDiscoveryResolvesFactReference(t *testing.T) {
	s, campaignID, reviews := queueFixture(t)
	ctx := context.Background()

	// Accept the fact first, then the low-agreement discovery that says the
	// party learned it.
	fact := reviewByKind(reviews, ReviewProposedFact)
	if _, err := s.DecideReview(ctx, campaignID, fact.ID, DecisionAccept, "", "keeper", nil); err != nil {
		t.Fatalf("accept fact: %v", err)
	}
	disc := reviewByKind(reviews, ReviewLowAgreement)
	acc, err := s.DecideReview(ctx, campaignID, disc.ID, DecisionAccept, "", "keeper", nil)
	if err != nil {
		t.Fatalf("accept discovery: %v", err)
	}

	ks, err := knowledge.New(s.db)
	if err != nil {
		t.Fatal(err)
	}
	d, err := ks.GetDiscovery(ctx, knowledge.ScopeDM, campaignID, acc.ResultRef)
	if err != nil {
		t.Fatalf("GetDiscovery: %v", err)
	}
	if d.AcceptedBy != "keeper" || d.AcceptedAt.IsZero() {
		t.Fatalf("discovery acceptance not recorded: %+v", d)
	}
	// The discovery's awareness row now grants the party the fact: the party
	// scope can retrieve it.
	partyFacts, err := ks.Facts(ctx, knowledge.ScopeParty, campaignID, knowledge.FactFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if !factsContain(partyFacts, "twice this month") {
		t.Fatal("accepted discovery did not grant the party the fact")
	}
}

func TestReview_DismissNeverResurrectsAndReproposalOpensNewItem(t *testing.T) {
	s, campaignID, reviews := queueFixture(t)
	ctx := context.Background()

	rel := reviewByKind(reviews, ReviewProposedRelationship)
	if _, err := s.DecideReview(ctx, campaignID, rel.ID, DecisionDismiss, "not a real change", "keeper", nil); err != nil {
		t.Fatalf("dismiss: %v", err)
	}

	// Re-building the queue over the same validated candidates must not
	// resurrect the dismissed item: the dedup key is already decided.
	after, err := s.BuildQueue(ctx, campaignID)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if got := reviewByKind(after, ReviewProposedRelationship); got == nil || got.Status != ReviewDismissed {
		t.Fatalf("dismissed relationship item was not preserved: %+v", got)
	}
	if open, _ := s.Reviews(ctx, campaignID, ReviewOpen); containsReview(open, rel.ID) {
		t.Fatal("dismissed item resurrected as open")
	}

	// A genuinely new occurrence of the same finding opens a NEW item. Stage
	// a fresh candidate with the same relationship payload (a new id is a new
	// finding by construction) and validate it as agree.
	var relCandID, relPayload string
	rows, err := s.db.Query(`SELECT id, payload FROM canon_candidates WHERE campaign_id = ? AND kind = 'relationship'`, campaignID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := rows.Scan(&relCandID, &relPayload); err != nil {
			t.Fatal(err)
		}
	}
	if relCandID == "" {
		t.Fatal("no relationship candidate found")
	}
	var newID string
	if err := s.db.QueryRow(`
		INSERT INTO canon_candidates (id, run_id, campaign_id, session_id, source_id, chunk_index, kind, payload, confidence, span_start, span_end, quote, checksum, created_at)
		SELECT 'reproposed-' || lower(hex(randomblob(4))), run_id, campaign_id, session_id, source_id, chunk_index, kind, payload, confidence, span_start, span_end, quote, checksum, created_at
		  FROM canon_candidates WHERE id = ? RETURNING id`, relCandID).Scan(&newID); err != nil {
		t.Fatalf("re-propose candidate: %v", err)
	}
	// A verdict for the new candidate: agree, so it is a proposed_relationship.
	if err := s.commitVerdictDirect(ctx, newID, VerdictAgree, VerdictApplied, 0.9); err != nil {
		t.Fatal(err)
	}

	rebuilt, err := s.BuildQueue(ctx, campaignID)
	if err != nil {
		t.Fatalf("rebuild after re-proposal: %v", err)
	}
	var newOpen *Review
	for i := range rebuilt {
		if rebuilt[i].CandidateID == newID {
			newOpen = &rebuilt[i]
		}
	}
	if newOpen == nil || newOpen.Status != ReviewOpen {
		t.Fatalf("re-proposed finding did not open a new item: %+v", newOpen)
	}
	// The old dismissed item is still dismissed, not resurrected.
	if got := reviewByKind(rebuilt, ReviewProposedRelationship); got != nil && got.ID == rel.ID && got.Status != ReviewDismissed {
		t.Fatalf("old item status changed: %+v", got)
	}
}

func TestReview_AcceptRequiresGraphStores(t *testing.T) {
	db, campaignID, _ := extractFixture(t)
	ctx := context.Background()
	validator := &verdictModel{responses: fixtureVerdicts()}
	s, err := NewWithValidator(db, &fakeModel{}, validator, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Validate(ctx, ValidateInput{CampaignID: campaignID}); err != nil {
		t.Fatal(err)
	}
	reviews, err := s.BuildQueue(ctx, campaignID)
	if err != nil {
		t.Fatal(err)
	}
	fact := reviewByKind(reviews, ReviewProposedFact)
	if fact == nil {
		t.Fatal("no fact item")
	}
	if _, err := s.DecideReview(ctx, campaignID, fact.ID, DecisionAccept, "", "keeper", nil); err == nil {
		t.Fatal("accept without graph stores must fail")
	}
}

func TestReview_ExportApplied(t *testing.T) {
	s, campaignID, reviews := queueFixture(t)
	ctx := context.Background()
	fact := reviewByKind(reviews, ReviewProposedFact)
	if _, err := s.DecideReview(ctx, campaignID, fact.ID, DecisionAccept, "", "keeper", nil); err != nil {
		t.Fatalf("accept: %v", err)
	}
	md, err := s.ExportApplied(ctx, campaignID, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md, "Applied canon changes") || !strings.Contains(md, "twice this month") {
		t.Fatalf("export missing applied change:\n%s", md)
	}
}

func TestReview_AcceptContradictionRegistersPair(t *testing.T) {
	db, fx, sessionID := seeded(t)
	ctx := context.Background()
	addSource(t, db, sessionID, "transcript", fixtureTranscript)

	s := reviewStore(t, db, &fakeModel{}, &uniformVerdictModel{})

	// Two fact candidates asserting different objects for the same subject and
	// predicate — the "two credible sources disagree" case.
	candA := insertFactCandidate(t, s, fx, sessionID, "the mines", "The Duke visited the mines.", 0.9)
	candB := insertFactCandidate(t, s, fx, sessionID, "nowhere", "The Duke visited nowhere.", 0.9)

	detail, _ := json.Marshal(map[string]any{
		"subject": fx.Duke, "predicate": "visited", "sides": []string{candA, candB},
	})
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO canon_reviews (id, campaign_id, kind, status, dedup_key, candidate_id, flag_id,
			subject, summary, detail, created_at, updated_at)
		VALUES (?, ?, 'contradiction', 'open', 'contradiction:test', NULL, NULL,
			'Contradiction', 'Two sources disagree', ?, ?, ?)`,
		"rid-contra", fx.Campaign.ID, string(detail), s.now().UnixMilli(), s.now().UnixMilli()); err != nil {
		t.Fatal(err)
	}

	rev, err := s.DecideReview(ctx, fx.Campaign.ID, "rid-contra", DecisionAccept, "", "keeper", nil)
	if err != nil {
		t.Fatalf("accept contradiction: %v", err)
	}
	if rev.Status != ReviewAccepted || rev.ResultRef == "" {
		t.Fatalf("contradiction accept = %+v", rev)
	}

	// Both facts landed contested and the register holds the pair.
	cs, err := campaign.New(s.db)
	if err != nil {
		t.Fatal(err)
	}
	cons, err := cs.Contradictions(ctx, campaign.ScopeDM, fx.Campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cons) != 2 { // the seed pair plus the accepted one
		t.Fatalf("contradictions = %d, want 2 (seed + accepted)", len(cons))
	}
	var facts []campaign.Fact
	for _, con := range cons {
		versions, err := cs.FactVersions(ctx, campaign.ScopeDM, fx.Campaign.ID, con.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range versions {
			f, err := cs.GetFact(ctx, campaign.ScopeDM, fx.Campaign.ID, v.FactID)
			if err != nil {
				t.Fatal(err)
			}
			facts = append(facts, *f)
		}
	}
	contested := 0
	for _, f := range facts {
		if f.Confidence == campaign.ConfidenceContested {
			contested++
		}
	}
	if contested < 4 { // the seed pair (2) plus the accepted pair (2)
		t.Fatalf("contested facts = %d, want >= 4", contested)
	}
}

// insertFactCandidate stages a fact candidate row directly, for contradiction
// tests that need two conflicting sources. The predicate is 'visited'.
func insertFactCandidate(t *testing.T, s *Store, fx *campaign.Fixture, sessionID, objectLiteral, statement string, confidence float64) string {
	t.Helper()
	return insertFactCandidateAbout(t, s, fx, sessionID, "cand-"+objectLiteral, fx.Duke, "visited", objectLiteral, statement, confidence)
}

// insertFactCandidateAbout stages a fact candidate with an explicit id,
// subject and predicate, so tests can point a fact at a staged entity's
// local_id and stay clear of the seed fixture's own predicates.
func insertFactCandidateAbout(t *testing.T, s *Store, fx *campaign.Fixture, sessionID, id, subject, predicate, objectLiteral, statement string, confidence float64) string {
	t.Helper()
	src := lookupSourceID(t, s.db, sessionID)
	payload, _ := json.Marshal(map[string]any{
		"local_id":       "fact-" + id,
		"statement":      statement,
		"subject":        subject,
		"predicate":      predicate,
		"object_entity":  "",
		"object_literal": objectLiteral,
		"visibility":     "public",
	})
	insertCandidateRow(t, s, fx, sessionID, src, id, "fact", payload, confidence)
	return id
}

// insertEntityCandidate stages an entity candidate whose local_id other
// staged candidates can reference.
func insertEntityCandidate(t *testing.T, s *Store, fx *campaign.Fixture, sessionID, localID string) string {
	t.Helper()
	src := lookupSourceID(t, s.db, sessionID)
	payload, _ := json.Marshal(map[string]any{
		"local_id": localID,
		"kind":     "faction",
		"name":     "The " + localID,
		"summary":  "Staged entity " + localID,
	})
	insertCandidateRow(t, s, fx, sessionID, src, "ent-"+localID, "entity", payload, 0.9)
	return "ent-" + localID
}

// insertCandidateRow is the shared insert behind the staged-candidate
// helpers; the run row must exist for the foreign key.
func insertCandidateRow(t *testing.T, s *Store, fx *campaign.Fixture, sessionID, src, id, kind string, payload []byte, confidence float64) {
	t.Helper()
	if _, err := s.db.Exec(`
		INSERT INTO canon_runs (id, campaign_id, session_id, kind, prompt_version, model, status, stop_reason, stats, error, created_at, updated_at)
		VALUES ('run', ?, ?, 'extract', 'test', 'test', 'completed', '', '{}', '', 0, 0)
		ON CONFLICT (id) DO NOTHING`, fx.Campaign.ID, sessionID); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if _, err := s.db.Exec(`
		INSERT INTO canon_candidates (id, run_id, campaign_id, session_id, source_id, chunk_index, kind, payload, confidence, span_start, span_end, quote, checksum, created_at)
		VALUES (?, 'run', ?, ?, ?, 0, ?, ?, ?, 0, 10, 'verbatim', ?, 0)`,
		id, fx.Campaign.ID, sessionID, src, kind, string(payload), confidence, "checksum-"+id); err != nil {
		t.Fatalf("insert candidate: %v", err)
	}
}

// TestReview_BuildQueueGroupsContradictions exercises the real grouping pass:
// two agree-verdicted fact candidates that assert different objects for the
// same subject and predicate are claimed by a single contradiction item and
// never get individual proposed_fact items.
func TestReview_BuildQueueGroupsContradictions(t *testing.T) {
	db, fx, sessionID := seeded(t)
	ctx := context.Background()
	addSource(t, db, sessionID, "transcript", fixtureTranscript)
	s := reviewStore(t, db, &fakeModel{}, &uniformVerdictModel{})

	candA := insertFactCandidateAbout(t, s, fx, sessionID, "cand-mines", fx.Duke, "traveled_to", "the mines", "The Duke traveled to the mines.", 0.9)
	candB := insertFactCandidateAbout(t, s, fx, sessionID, "cand-nowhere", fx.Duke, "traveled_to", "nowhere", "The Duke traveled to nowhere.", 0.9)
	// A third fact on the same subject but a different predicate must NOT be
	// grouped with the conflicting pair.
	candC := insertFactCandidateAbout(t, s, fx, sessionID, "cand-other", fx.Duke, "signed", "the ledger", "The Duke signed the ledger.", 0.9)
	for _, id := range []string{candA, candB, candC} {
		if err := s.commitVerdictDirect(ctx, id, VerdictAgree, VerdictApplied, 0.9); err != nil {
			t.Fatal(err)
		}
	}

	reviews, err := s.BuildQueue(ctx, fx.Campaign.ID)
	if err != nil {
		t.Fatalf("BuildQueue: %v", err)
	}
	var contra *Review
	factItems := 0
	for i := range reviews {
		switch reviews[i].Kind {
		case ReviewContradiction:
			contra = &reviews[i]
		case ReviewProposedFact:
			factItems++
		}
	}
	if contra == nil {
		t.Fatalf("no contradiction item in %v", kinds(reviews))
	}
	if factItems != 1 {
		t.Fatalf("proposed_fact items = %d, want 1 (the non-conflicting third fact)", factItems)
	}
	var detail struct {
		Subject   string   `json:"subject"`
		Predicate string   `json:"predicate"`
		Sides     []string `json:"sides"`
	}
	if err := json.Unmarshal([]byte(contra.Detail), &detail); err != nil {
		t.Fatalf("contradiction detail: %v", err)
	}
	if len(detail.Sides) != 2 || detail.Predicate != "traveled_to" {
		t.Fatalf("contradiction sides = %+v", detail)
	}

	// Rebuilding mints nothing new: the dedup keys are already seen.
	again, err := s.BuildQueue(ctx, fx.Campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != len(reviews) {
		t.Fatalf("rebuild minted new items: %d then %d", len(reviews), len(again))
	}
}

// TestReview_ContradictionAcceptReusesDecidedSide covers the cross-run case:
// a fact accepted on its own item, then a conflicting candidate staged later.
// Accepting the contradiction must reuse the already-canon fact, not mint a
// duplicate of it.
func TestReview_ContradictionAcceptReusesDecidedSide(t *testing.T) {
	db, fx, sessionID := seeded(t)
	ctx := context.Background()
	addSource(t, db, sessionID, "transcript", fixtureTranscript)
	s := reviewStore(t, db, &fakeModel{}, &uniformVerdictModel{})

	candA := insertFactCandidateAbout(t, s, fx, sessionID, "cand-mines", fx.Duke, "traveled_to", "the mines", "The Duke traveled to the mines.", 0.9)
	if err := s.commitVerdictDirect(ctx, candA, VerdictAgree, VerdictApplied, 0.9); err != nil {
		t.Fatal(err)
	}
	first, err := s.BuildQueue(ctx, fx.Campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	itemA := reviewByKind(first, ReviewProposedFact)
	if itemA == nil {
		t.Fatalf("first build had no proposed_fact: %v", kinds(first))
	}
	acceptedA, err := s.DecideReview(ctx, fx.Campaign.ID, itemA.ID, DecisionAccept, "", "keeper", nil)
	if err != nil {
		t.Fatalf("accept A: %v", err)
	}

	// A later run stages the conflicting account. The new build claims both
	// sides into one contradiction item and does not re-offer B on its own.
	candB := insertFactCandidateAbout(t, s, fx, sessionID, "cand-nowhere", fx.Duke, "traveled_to", "nowhere", "The Duke traveled to nowhere.", 0.9)
	if err := s.commitVerdictDirect(ctx, candB, VerdictAgree, VerdictApplied, 0.9); err != nil {
		t.Fatal(err)
	}
	second, err := s.BuildQueue(ctx, fx.Campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	contra := reviewByKind(second, ReviewContradiction)
	if contra == nil {
		t.Fatalf("second build had no contradiction: %v", kinds(second))
	}
	var openFactItems int
	for _, r := range second {
		if r.Kind == ReviewProposedFact && r.Status == ReviewOpen {
			openFactItems++
		}
	}
	if openFactItems != 0 {
		t.Fatalf("conflicting side was also offered individually (%d open proposed_fact)", openFactItems)
	}

	accepted, err := s.DecideReview(ctx, fx.Campaign.ID, contra.ID, DecisionAccept, "", "keeper", nil)
	if err != nil {
		t.Fatalf("accept contradiction: %v", err)
	}

	// Exactly one new fact was written: A's accepted fact is reused, not
	// duplicated, and both sides land contested.
	cs, err := campaign.New(s.db)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := cs.ListFacts(ctx, campaign.ScopeDM, fx.Campaign.ID, campaign.FactFilter{Predicate: "traveled_to"})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 {
		t.Fatalf("traveled_to facts = %d, want 2 (reuse, not duplicate)", len(facts))
	}
	seenA := false
	for _, f := range facts {
		if f.Confidence != campaign.ConfidenceContested {
			t.Fatalf("side fact %s confidence = %q, want contested", f.ID, f.Confidence)
		}
		if f.ID == acceptedA.ResultRef {
			seenA = true
		}
	}
	if !seenA {
		t.Fatal("the originally accepted fact is not part of the contradiction")
	}

	// The register's versions for the accepted contradiction include the
	// reused fact.
	versions, err := cs.FactVersions(ctx, campaign.ScopeDM, fx.Campaign.ID, accepted.ResultRef)
	if err != nil {
		t.Fatal(err)
	}
	foundA := false
	for _, v := range versions {
		if v.FactID == acceptedA.ResultRef {
			foundA = true
		}
	}
	if len(versions) != 2 || !foundA {
		t.Fatalf("contradiction versions = %+v, want 2 including the reused fact", versions)
	}
}

// TestReview_AcceptResolvesStagedEntityReference: a fact about an entity the
// same extraction proposed can be accepted once the entity is — the staged
// local_id resolves to the created entity id. Accepting out of order fails
// with instructions instead of an opaque not-found.
func TestReview_AcceptResolvesStagedEntityReference(t *testing.T) {
	db, fx, sessionID := seeded(t)
	ctx := context.Background()
	addSource(t, db, sessionID, "transcript", fixtureTranscript)
	s := reviewStore(t, db, &fakeModel{}, &uniformVerdictModel{})

	entCand := insertEntityCandidate(t, s, fx, sessionID, "robed-folk")
	factCand := insertFactCandidateAbout(t, s, fx, sessionID, "cand-robed", "robed-folk", "came_from", "Greyfall", "The robed folk came from Greyfall.", 0.9)
	for _, id := range []string{entCand, factCand} {
		if err := s.commitVerdictDirect(ctx, id, VerdictAgree, VerdictApplied, 0.95); err != nil {
			t.Fatal(err)
		}
	}
	reviews, err := s.BuildQueue(ctx, fx.Campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	ent := reviewByKind(reviews, ReviewProposedEntity)
	fact := reviewByKind(reviews, ReviewProposedFact)
	if ent == nil || fact == nil {
		t.Fatalf("queue = %v, want entity + fact", kinds(reviews))
	}

	// Out of order: the reference cannot resolve yet.
	if _, err := s.DecideReview(ctx, fx.Campaign.ID, fact.ID, DecisionAccept, "", "keeper", nil); err == nil ||
		!strings.Contains(err.Error(), "accept the entity") {
		t.Fatalf("accept before the entity must fail with instructions, got %v", err)
	}
	// The failed accept left the item open (claim rolled back).
	still, err := s.getReview(ctx, fx.Campaign.ID, fact.ID)
	if err != nil {
		t.Fatal(err)
	}
	if still.Status != ReviewOpen {
		t.Fatalf("failed accept left the item %s, want open", still.Status)
	}

	acceptedEnt, err := s.DecideReview(ctx, fx.Campaign.ID, ent.ID, DecisionAccept, "", "keeper", nil)
	if err != nil {
		t.Fatalf("accept entity: %v", err)
	}
	acceptedFact, err := s.DecideReview(ctx, fx.Campaign.ID, fact.ID, DecisionAccept, "", "keeper", nil)
	if err != nil {
		t.Fatalf("accept fact: %v", err)
	}
	cs, err := campaign.New(s.db)
	if err != nil {
		t.Fatal(err)
	}
	f, err := cs.GetFact(ctx, campaign.ScopeDM, fx.Campaign.ID, acceptedFact.ResultRef)
	if err != nil {
		t.Fatal(err)
	}
	if f.SubjectEntity != acceptedEnt.ResultRef {
		t.Fatalf("fact subject = %q, want the accepted entity %q", f.SubjectEntity, acceptedEnt.ResultRef)
	}
}

// TestReview_ModifyWritesReplacementPayload covers the modify-then-accept
// path: the DM's replacement payload is what lands as canon, and the item
// records 'modified'. An empty replacement payload is refused.
func TestReview_ModifyWritesReplacementPayload(t *testing.T) {
	s, campaignID, reviews := queueFixture(t)
	ctx := context.Background()

	fact := reviewByKind(reviews, ReviewProposedFact)
	modified := map[string]any{}
	for k, v := range fact.Payload {
		modified[k] = v
	}
	modified["statement"] = "The Duke's men came through once this month."
	raw, _ := json.Marshal(modified)

	rev, err := s.DecideReview(ctx, campaignID, fact.ID, DecisionModify, "Tom said twice, but the ledger says once", "keeper", raw)
	if err != nil {
		t.Fatalf("modify accept: %v", err)
	}
	if rev.Status != ReviewModified || rev.ResultRef == "" {
		t.Fatalf("modified review = %+v", rev)
	}
	cs, err := campaign.New(s.db)
	if err != nil {
		t.Fatal(err)
	}
	f, err := cs.GetFact(ctx, campaign.ScopeDM, campaignID, rev.ResultRef)
	if err != nil {
		t.Fatal(err)
	}
	if f.Statement != "The Duke's men came through once this month." {
		t.Fatalf("canon fact statement = %q, want the DM's replacement", f.Statement)
	}

	// Modify with no replacement payload is refused.
	rel := reviewByKind(reviews, ReviewProposedRelationship)
	if _, err := s.DecideReview(ctx, campaignID, rel.ID, DecisionModify, "", "keeper", nil); err == nil {
		t.Fatal("modify without a payload must fail")
	}
}

// TestReview_AcceptAgreementBatch covers the queue's one batch affordance:
// open proposed_* items the checker agreed with at or above the threshold
// are accepted in reference order (entities first); everything else stays
// open for an individual decision.
func TestReview_AcceptAgreementBatch(t *testing.T) {
	db, fx, sessionID := seeded(t)
	ctx := context.Background()
	addSource(t, db, sessionID, "transcript", fixtureTranscript)
	s := reviewStore(t, db, &fakeModel{}, &uniformVerdictModel{})

	// An entity and a fact about it, both agreed: the batch must accept the
	// entity first or the fact's reference cannot resolve.
	entCand := insertEntityCandidate(t, s, fx, sessionID, "robed-folk")
	factCand := insertFactCandidateAbout(t, s, fx, sessionID, "cand-robed-batch", "robed-folk", "came_from", "Greyfall", "The robed folk came from Greyfall.", 0.9)
	// A downgrade-verdicted candidate is not 'agree' and must be skipped.
	downCand := insertFactCandidate(t, s, fx, sessionID, "the mines", "The Duke visited the mines.", 0.9)
	for _, id := range []string{entCand, factCand} {
		if err := s.commitVerdictDirect(ctx, id, VerdictAgree, VerdictApplied, 0.95); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.commitVerdictDirect(ctx, downCand, VerdictDowngrade, VerdictApplied, 0.9); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BuildQueue(ctx, fx.Campaign.ID); err != nil {
		t.Fatal(err)
	}

	res, err := s.AcceptAgreement(ctx, fx.Campaign.ID, 0.8, "keeper")
	if err != nil {
		t.Fatalf("AcceptAgreement: %v", err)
	}
	if res.Accepted != 2 || len(res.Failed) != 0 {
		t.Fatalf("batch = %+v, want 2 accepted and no failures", res)
	}
	open, err := s.Reviews(ctx, fx.Campaign.ID, ReviewOpen)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].CandidateID != downCand {
		t.Fatalf("open after batch = %+v, want only the downgrade-verdicted fact", open)
	}

	// A threshold above the verdicts accepts nothing.
	if _, err := s.DecideReview(ctx, fx.Campaign.ID, open[0].ID, DecisionDismiss, "", "keeper", nil); err != nil {
		t.Fatal(err)
	}
	res, err = s.AcceptAgreement(ctx, fx.Campaign.ID, 0.99, "keeper")
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted != 0 {
		t.Fatalf("batch at 0.99 accepted %d, want 0", res.Accepted)
	}
}

/* ---------- helpers ---------- */

func kinds(reviews []Review) []string {
	out := make([]string, 0, len(reviews))
	for _, r := range reviews {
		out = append(out, r.Kind)
	}
	return out
}

func factsContain(facts []campaign.Fact, needle string) bool {
	for _, f := range facts {
		if strings.Contains(f.Statement, needle) {
			return true
		}
	}
	return false
}

func containsReview(reviews []Review, id string) bool {
	for _, r := range reviews {
		if r.ID == id {
			return true
		}
	}
	return false
}

// commitVerdictDirect writes a verdict row for a candidate directly, the way
// the adversarial pass would, so tests can stage a re-proposed finding.
func (s *Store) commitVerdictDirect(ctx context.Context, candidateID, verdict, status string, agreement float64) error {
	var runID, campaignID, checksum string
	if err := s.db.QueryRowContext(ctx,
		`SELECT run_id, campaign_id, checksum FROM canon_candidates WHERE id = ?`, candidateID).
		Scan(&runID, &campaignID, &checksum); err != nil {
		return err
	}
	raw, _ := json.Marshal(map[string]any{"verdict": verdict, "agreement": agreement})
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO canon_verdicts (id, run_id, campaign_id, candidate_id, prompt_version, input_checksum,
			verdict, status, agreement, rationale, proposed_confidence, confidence_before, confidence_after,
			rejection_reason, model, input_tokens, output_tokens, raw, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, 0.9, 0.9, '', 'test', 0, 0, ?, ?)`,
		uuid.NewString(), runID, campaignID, candidateID, VALIDATE_PROMPT_VERSION, checksum,
		verdict, status, agreement, "test rationale", string(raw), s.now().UnixMilli())
	return err
}
