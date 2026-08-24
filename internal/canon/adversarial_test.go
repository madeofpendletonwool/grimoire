package canon

// The adversarial second pass's tests (MAD-308), in three layers, the way
// extraction's are: the wire and decision rules as pure functions; the
// evidence assembly (window, references) as pure functions; and the
// end-to-end acceptance over a temp database — candidates in, verdicts
// recorded, downgrades applied, low-agreement items flagged, an upgrade
// proposal rejected and logged, resume by checksum verified — with a fake
// validator replaying scripted verdicts, the way Arda's test_validation_run.py
// does.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
)

/* ---------- the decision rules ---------- */

func TestApplyVerdict(t *testing.T) {
	ff := func(v float64) *flexFloat { f := flexFloat(v); return &f }
	cases := []struct {
		name              string
		current           float64
		wire              verdictWire
		threshold         float64
		wantVerdict       string
		wantStatus        string
		wantAfter         float64
		wantRejection     string
		wantLowAgreement  bool
		wantRationaleNeed string
	}{
		{
			name: "agree above threshold confirms", current: 0.9,
			wire:        verdictWire{Verdict: VerdictAgree, Agreement: ff(0.95), Rationale: "span states it"},
			wantVerdict: VerdictAgree, wantStatus: VerdictApplied, wantAfter: 0.9,
			wantRationaleNeed: "span states it",
		},
		{
			name: "agree at the threshold confirms", current: 0.9,
			wire:        verdictWire{Verdict: VerdictAgree, Agreement: ff(0.8)},
			threshold:   0.8,
			wantVerdict: VerdictAgree, wantStatus: VerdictApplied, wantAfter: 0.9,
		},
		{
			name: "agree below threshold is coerced to a flag", current: 0.9,
			wire:        verdictWire{Verdict: VerdictAgree, Agreement: ff(0.3)},
			threshold:   0.8,
			wantVerdict: VerdictFlagReview, wantStatus: VerdictApplied, wantAfter: 0.9, wantLowAgreement: true,
		},
		{
			name: "valid downgrade lowers confidence", current: 0.95,
			wire:        verdictWire{Verdict: VerdictDowngrade, Agreement: ff(0.9), Proposed: ff(0.5)},
			threshold:   0.8,
			wantVerdict: VerdictDowngrade, wantStatus: VerdictApplied, wantAfter: 0.5,
		},
		{
			name: "downgrade to a higher confidence is rejected as an upgrade", current: 0.9,
			wire:        verdictWire{Verdict: VerdictDowngrade, Agreement: ff(0.95), Proposed: ff(0.99)},
			threshold:   0.8,
			wantVerdict: VerdictFlagReview, wantStatus: VerdictRejected, wantAfter: 0.9, wantRejection: RejectUpgrade,
		},
		{
			name: "downgrade to the same confidence is rejected", current: 0.9,
			wire:        verdictWire{Verdict: VerdictDowngrade, Agreement: ff(0.9), Proposed: ff(0.9)},
			threshold:   0.8,
			wantVerdict: VerdictFlagReview, wantStatus: VerdictRejected, wantAfter: 0.9, wantRejection: RejectUpgrade,
		},
		{
			name: "downgrade without a proposed confidence is rejected", current: 0.9,
			wire:        verdictWire{Verdict: VerdictDowngrade, Agreement: ff(0.9)},
			threshold:   0.8,
			wantVerdict: VerdictFlagReview, wantStatus: VerdictRejected, wantAfter: 0.9, wantRejection: RejectNoConfidence,
		},
		{
			name: "downgrade to an out-of-range confidence is rejected", current: 0.9,
			wire:        verdictWire{Verdict: VerdictDowngrade, Agreement: ff(0.9), Proposed: ff(1.5)},
			threshold:   0.8,
			wantVerdict: VerdictFlagReview, wantStatus: VerdictRejected, wantAfter: 0.9, wantRejection: RejectBadConfidence,
		},
		{
			name: "downgrade below threshold becomes a flag, not a downgrade", current: 0.9,
			wire:        verdictWire{Verdict: VerdictDowngrade, Agreement: ff(0.4), Proposed: ff(0.5)},
			threshold:   0.8,
			wantVerdict: VerdictFlagReview, wantStatus: VerdictApplied, wantAfter: 0.9, wantLowAgreement: true,
		},
		{
			name: "flag review passes through at any agreement", current: 0.7,
			wire:        verdictWire{Verdict: VerdictFlagReview, Agreement: ff(0.2)},
			threshold:   0.8,
			wantVerdict: VerdictFlagReview, wantStatus: VerdictApplied, wantAfter: 0.7,
		},
		{
			name: "unknown verdict is unusable and flags", current: 0.9,
			wire:        verdictWire{Verdict: "upgrade", Agreement: ff(0.99), Proposed: ff(1.0)},
			threshold:   0.8,
			wantVerdict: VerdictFlagReview, wantStatus: VerdictUnparseable, wantAfter: 0.9,
			wantRationaleNeed: "no usable verdict",
		},
		{
			name: "missing agreement is unusable and flags", current: 0.9,
			wire:        verdictWire{Verdict: VerdictAgree},
			threshold:   0.8,
			wantVerdict: VerdictFlagReview, wantStatus: VerdictUnparseable, wantAfter: 0.9,
		},
		{
			name: "out-of-range agreement is unusable and flags", current: 0.9,
			wire:        verdictWire{Verdict: VerdictAgree, Agreement: ff(1.2)},
			threshold:   0.8,
			wantVerdict: VerdictFlagReview, wantStatus: VerdictUnparseable, wantAfter: 0.9,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			threshold := tc.threshold
			if threshold == 0 {
				threshold = DefaultAgreementThreshold
			}
			d := applyVerdict(tc.current, tc.wire, threshold)
			if d.Verdict != tc.wantVerdict || d.Status != tc.wantStatus {
				t.Fatalf("verdict/status = %s/%s, want %s/%s", d.Verdict, d.Status, tc.wantVerdict, tc.wantStatus)
			}
			if d.ConfidenceAfter != tc.wantAfter {
				t.Fatalf("confidence after = %v, want %v", d.ConfidenceAfter, tc.wantAfter)
			}
			if d.ConfidenceBefore != tc.current {
				t.Fatalf("confidence before = %v, want %v", d.ConfidenceBefore, tc.current)
			}
			if d.RejectionReason != tc.wantRejection {
				t.Fatalf("rejection reason = %q, want %q", d.RejectionReason, tc.wantRejection)
			}
			if d.LowAgreement != tc.wantLowAgreement {
				t.Fatalf("low agreement = %v, want %v", d.LowAgreement, tc.wantLowAgreement)
			}
			if tc.wantRationaleNeed != "" && !strings.Contains(d.Rationale, tc.wantRationaleNeed) {
				t.Fatalf("rationale %q missing %q", d.Rationale, tc.wantRationaleNeed)
			}
		})
	}
}

/* ---------- the verdict wire ---------- */

func TestParseVerdictResponse(t *testing.T) {
	t.Run("plain json", func(t *testing.T) {
		w, problems := parseVerdictResponse(`{"verdict":"agree","agreement":0.9,"rationale":"ok","proposed_confidence":null}`)
		if len(problems) != 0 || w.Verdict != "agree" || w.Agreement == nil || float64(*w.Agreement) != 0.9 || w.Proposed != nil {
			t.Fatalf("wire = %+v problems %v", w, problems)
		}
	})
	t.Run("fenced json with preamble", func(t *testing.T) {
		w, problems := parseVerdictResponse("Here is my verdict:\n```json\n{\"verdict\": \"flag_review\", \"agreement\": 0.4}\n```")
		if len(problems) != 0 || w.Verdict != "flag_review" {
			t.Fatalf("wire = %+v problems %v", w, problems)
		}
	})
	t.Run("stringified numbers", func(t *testing.T) {
		w, problems := parseVerdictResponse(`{"verdict":"downgrade","agreement":"0.85","proposed_confidence":"0.4"}`)
		if len(problems) != 0 || w.Agreement == nil || float64(*w.Agreement) != 0.85 || w.Proposed == nil || float64(*w.Proposed) != 0.4 {
			t.Fatalf("wire = %+v problems %v", w, problems)
		}
	})
	t.Run("garbage yields problems not panic", func(t *testing.T) {
		w, problems := parseVerdictResponse("sorry, I cannot produce JSON today")
		if len(problems) == 0 || w.Verdict != "" {
			t.Fatalf("wire = %+v problems %v", w, problems)
		}
	})
	t.Run("missing fields are reported", func(t *testing.T) {
		_, problems := parseVerdictResponse(`{"rationale":"nothing else"}`)
		joined := strings.Join(problems, "; ")
		for _, want := range []string{"verdict: missing", "agreement: missing"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("problems %q missing %q", joined, want)
			}
		}
	})
	t.Run("non-numeric agreement is reported", func(t *testing.T) {
		_, problems := parseVerdictResponse(`{"verdict":"agree","agreement":"high"}`)
		if len(problems) == 0 || !strings.Contains(strings.Join(problems, "; "), "agreement") {
			t.Fatalf("problems = %v", problems)
		}
	})
}

/* ---------- evidence assembly ---------- */

func TestBuildWindow(t *testing.T) {
	content := "alpha line one\nbeta line two\n\ngamma the span sits here\ndelta follows\n\nepsilon ends"
	span := strings.Index(content, "the span sits here")
	spanEnd := span + len("the span sits here")

	t.Run("window opens on a line and contains the span", func(t *testing.T) {
		start, end := buildWindow(content, int64(span), int64(spanEnd), 25)
		if start > int64(span) || end < int64(spanEnd) {
			t.Fatalf("window %d..%d does not contain span %d..%d", start, end, span, spanEnd)
		}
		if start > 0 && content[start-1] != '\n' {
			t.Fatalf("window start %d is mid-line: %q", start, content[start:start+10])
		}
	})

	t.Run("span at the very start clamps to zero", func(t *testing.T) {
		start, end := buildWindow(content, 0, 5, 10)
		if start != 0 {
			t.Fatalf("start = %d, want 0", start)
		}
		if end < 5 {
			t.Fatalf("end = %d truncates span", end)
		}
	})

	t.Run("span at the very end clamps to len", func(t *testing.T) {
		start, end := buildWindow(content, int64(len(content)-6), int64(len(content)), 10)
		if end != int64(len(content)) {
			t.Fatalf("end = %d, want %d", end, len(content))
		}
		if start > int64(len(content)-6) {
			t.Fatalf("start = %d truncates span", start)
		}
	})

	t.Run("window larger than the source yields the whole source", func(t *testing.T) {
		start, end := buildWindow("short text", 2, 6, 500)
		if start != 0 || end != int64(len("short text")) {
			t.Fatalf("window = %d..%d, want whole source", start, end)
		}
	})
}

func TestPayloadRefs(t *testing.T) {
	t.Run("fact", func(t *testing.T) {
		refs := payloadRefs(KindFact, []byte(`{"subject":"duke","predicate":"owns","object_entity":"mines","object_literal":""}`))
		if len(refs) != 2 || refs[0] != "duke" || refs[1] != "mines" {
			t.Fatalf("refs = %v", refs)
		}
	})
	t.Run("fact with literal object lists only the subject", func(t *testing.T) {
		refs := payloadRefs(KindFact, []byte(`{"subject":"duke","object_literal":"twice this month","object_entity":""}`))
		if len(refs) != 1 || refs[0] != "duke" {
			t.Fatalf("refs = %v", refs)
		}
	})
	t.Run("event participants and location, deduped", func(t *testing.T) {
		refs := payloadRefs(KindEvent, []byte(`{"location":"falls","participants":[{"entity":"thalia","role":"party"},{"entity":"falls","role":"place"}]}`))
		if len(refs) != 2 || refs[0] != "falls" || refs[1] != "thalia" {
			t.Fatalf("refs = %v", refs)
		}
	})
	t.Run("relationship endpoints", func(t *testing.T) {
		refs := payloadRefs(KindRelationship, []byte(`{"from_entity":"tom","rel_type":"located_in","to_entity":"blackwater"}`))
		if len(refs) != 2 || refs[0] != "tom" || refs[1] != "blackwater" {
			t.Fatalf("refs = %v", refs)
		}
	})
	t.Run("discovery skips party", func(t *testing.T) {
		refs := payloadRefs(KindDiscovery, []byte(`{"fact":"f1","discovered_by":"party","stance":"knows"}`))
		if len(refs) != 0 {
			t.Fatalf("refs = %v, want none for party", refs)
		}
	})
	t.Run("discovery names the discoverer", func(t *testing.T) {
		refs := payloadRefs(KindDiscovery, []byte(`{"fact":"f1","discovered_by":"mira","stance":"knows"}`))
		if len(refs) != 1 || refs[0] != "mira" {
			t.Fatalf("refs = %v", refs)
		}
	})
	t.Run("entity payload has no campaign refs", func(t *testing.T) {
		refs := payloadRefs(KindEntity, []byte(`{"local_id":"robed-folk","name":"The Robed Folk"}`))
		if len(refs) != 0 {
			t.Fatalf("refs = %v", refs)
		}
	})
}

/* ---------- configuration ---------- */

func TestValidateConfigFromEnv(t *testing.T) {
	cfg := ConfigFromEnv(func(k string) string {
		switch k {
		case "CANON_AGREEMENT_THRESHOLD":
			return "0.65"
		case "CANON_VALIDATE_MODEL":
			return "glm-4.5-air"
		}
		return ""
	})
	if cfg.AgreementThreshold != 0.65 {
		t.Fatalf("threshold = %v, want 0.65", cfg.AgreementThreshold)
	}
	if cfg.ValidateModel != "glm-4.5-air" {
		t.Fatalf("validate model = %q", cfg.ValidateModel)
	}

	cfg = ConfigFromEnv(func(k string) string {
		if k == "CANON_AGREEMENT_THRESHOLD" {
			return "1.5" // out of range: ignored, default holds
		}
		return ""
	})
	if cfg.AgreementThreshold != DefaultAgreementThreshold {
		t.Fatalf("threshold = %v, want default %v", cfg.AgreementThreshold, DefaultAgreementThreshold)
	}
}

func TestNewWithValidatorFallsBackAndNormalizes(t *testing.T) {
	db, _, _ := seeded(t)
	m := &uniformVerdictModel{response: "{}"}
	s, err := NewWithValidator(db, m, nil, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if s.validator != s.model {
		t.Fatal("nil validator must fall back to the extractor's client")
	}
	if s.cfg.AgreementThreshold != DefaultAgreementThreshold {
		t.Fatalf("threshold = %v, want default", s.cfg.AgreementThreshold)
	}
}

func TestNewLLMValidatorOverridesModel(t *testing.T) {
	base := llm.New(llm.Config{APIKey: "test-key", Model: "extractor-model"})
	if got := NewLLMValidator(base, "validator-model").ModelName(); got != "validator-model" {
		t.Fatalf("model = %q, want validator-model", got)
	}
	if got := NewLLMValidator(base, "").ModelName(); got != "extractor-model" {
		t.Fatalf("model = %q, want extractor-model", got)
	}
}

/* ---------- fakes ---------- */

// verdictModel replays scripted responses keyed by a needle that appears in
// exactly one candidate's prompt, so assertions do not depend on the order
// candidates come off the queue (ids are UUIDs; the millisecond ties).
type verdictModel struct {
	responses map[string]string
	calls     []string
}

func (v *verdictModel) ModelName() string { return "fake-validator" }

func (v *verdictModel) Complete(ctx context.Context, system, user string) (Completion, error) {
	v.calls = append(v.calls, user)
	for needle, resp := range v.responses {
		if strings.Contains(user, needle) {
			return Completion{Text: resp, InputTokens: 50, OutputTokens: 80}, nil
		}
	}
	return Completion{}, fmt.Errorf("no scripted verdict for prompt:\n%s", user)
}

// uniformVerdictModel answers every call with the same response.
type uniformVerdictModel struct {
	response string
	calls    []string
}

func (u *uniformVerdictModel) ModelName() string { return "fake-validator" }

func (u *uniformVerdictModel) Complete(ctx context.Context, system, user string) (Completion, error) {
	u.calls = append(u.calls, user)
	return Completion{Text: u.response, InputTokens: 50, OutputTokens: 80}, nil
}

// extractFixture runs one extraction over the fixture transcript and returns
// the store's five staged candidates, ready for validation.
func extractFixture(t *testing.T) (*sql.DB, string, []Candidate) {
	t.Helper()
	db, fx, sessionID := seeded(t)
	addSource(t, db, sessionID, "transcript", fixtureTranscript)
	extractor := &fakeModel{responses: []string{fixtureResponse(fx)}}
	s := newStore(t, db, extractor, testConfig())
	if _, err := s.Extract(context.Background(), ExtractInput{CampaignID: fx.Campaign.ID}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(extractor.calls) != 1 {
		t.Fatalf("extractor calls = %d", len(extractor.calls))
	}
	cands, err := s.ListCandidates(context.Background(), fx.Campaign.ID, CandidateFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 5 {
		t.Fatalf("candidates = %d, want 5", len(cands))
	}
	return db, fx.Campaign.ID, cands
}

// fixtureVerdicts scripts one verdict per fixture candidate, chosen so every
// verdict outcome is exercised: agree, an applied downgrade, a rejected
// upgrade, a low-agreement flag, and a second applied downgrade.
func fixtureVerdicts() map[string]string {
	return map[string]string{
		"NEW ENTITY: The Robed Folk": `{"verdict":"agree","agreement":0.95,"rationale":"The span names the robed folk and where they came from; the entity is the span's own subject.","proposed_confidence":null}`,

		"STATEMENT: The Duke's men passed through Blackwater twice this month.": `{"verdict":"downgrade","agreement":0.9,"rationale":"The span shows Tom saying \"The Duke's men came through twice this month\" — it supports 'came through', but 'twice this month' is Tom's own count, not something the span independently shows.","proposed_confidence":0.5}`,

		"EVENT: The party is ambushed by robed figures on the Greyfall road.": `{"verdict":"downgrade","agreement":0.95,"rationale":"The span supports the ambush but the location is not named; proposing a higher confidence anyway.","proposed_confidence":0.99}`,

		"METHOD: Tom told them at the Waystone": `{"verdict":"agree","agreement":0.3,"rationale":"The span shows Tom speaking, but the party's presence at the counter is only implied by the scene heading.","proposed_confidence":null}`,

		"RELATIONSHIP: ": `{"verdict":"downgrade","agreement":0.85,"rationale":"The span places Tom inside the Waystone Inn, so 'located in' holds, but as an introduction, not a change the fiction stresses.","proposed_confidence":0.3}`,
	}
}

/* ---------- end to end ---------- */

func TestValidate_EndToEnd(t *testing.T) {
	db, campaignID, cands := extractFixture(t)
	ctx := context.Background()

	validator := &verdictModel{responses: fixtureVerdicts()}
	s, err := NewWithValidator(db, &fakeModel{}, validator, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.Validate(ctx, ValidateInput{CampaignID: campaignID})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if run.Status != RunCompleted || run.Kind != RunValidate || run.PromptVersion != VALIDATE_PROMPT_VERSION {
		t.Fatalf("run = %q/%q/%q error %q", run.Status, run.Kind, run.PromptVersion, run.Error)
	}
	if run.Model != "fake-validator" {
		t.Fatalf("run model = %q, want the validator's, not the extractor's", run.Model)
	}
	if len(validator.calls) != 5 || run.Stats.UnitsDone != 5 || run.Stats.Requests != 5 {
		t.Fatalf("calls = %d, stats = %+v", len(validator.calls), run.Stats)
	}

	// The verdict ledger, per candidate.
	verdicts, err := s.Verdicts(ctx, campaignID, VerdictFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(verdicts) != 5 {
		t.Fatalf("verdicts = %d, want 5", len(verdicts))
	}
	byCandidate := map[string]Verdict{}
	for _, v := range verdicts {
		byCandidate[v.CandidateID] = v
		if v.RunID != run.ID || v.Model != "fake-validator" || v.Raw == "" {
			t.Fatalf("verdict = %+v", v)
		}
		if v.InputTokens != 50 || v.OutputTokens != 80 {
			t.Fatalf("tokens = %d/%d", v.InputTokens, v.OutputTokens)
		}
	}

	type expectation struct {
		kind         string
		verdict      string
		status       string
		confBefore   float64
		confAfter    float64
		rejectReason string
		lowAgreement bool
	}
	want := map[string]expectation{
		KindEntity:       {KindEntity, VerdictAgree, VerdictApplied, 0.9, 0.9, "", false},
		KindFact:         {KindFact, VerdictDowngrade, VerdictApplied, 0.95, 0.5, "", false},
		KindEvent:        {KindEvent, VerdictFlagReview, VerdictRejected, 0.9, 0.9, RejectUpgrade, false},
		KindDiscovery:    {KindDiscovery, VerdictFlagReview, VerdictApplied, 0.9, 0.9, "", true},
		KindRelationship: {KindRelationship, VerdictDowngrade, VerdictApplied, 0.6, 0.3, "", false},
	}
	after, err := s.ListCandidates(ctx, campaignID, CandidateFilter{})
	if err != nil {
		t.Fatal(err)
	}
	confidence := map[string]float64{}
	for _, c := range after {
		confidence[c.ID] = c.Confidence
	}
	for _, c := range cands {
		w := want[c.Kind]
		v, ok := byCandidate[c.ID]
		if !ok {
			t.Fatalf("kind %s candidate %s has no verdict", c.Kind, c.ID)
		}
		if v.Verdict != w.verdict || v.Status != w.status {
			t.Fatalf("kind %s verdict/status = %s/%s, want %s/%s", c.Kind, v.Verdict, v.Status, w.verdict, w.status)
		}
		if v.ConfidenceBefore != w.confBefore || v.ConfidenceAfter != w.confAfter {
			t.Fatalf("kind %s before/after = %v/%v, want %v/%v", c.Kind, v.ConfidenceBefore, v.ConfidenceAfter, w.confBefore, w.confAfter)
		}
		if v.RejectionReason != w.rejectReason {
			t.Fatalf("kind %s rejection = %q, want %q", c.Kind, v.RejectionReason, w.rejectReason)
		}
		if confidence[c.ID] != w.confAfter {
			t.Fatalf("kind %s stored confidence = %v, want %v (the downgrade must land)", c.Kind, confidence[c.ID], w.confAfter)
		}
	}

	// Rationales quote what the span does and does not show.
	for _, v := range verdicts {
		if strings.TrimSpace(v.Rationale) == "" {
			t.Fatalf("verdict %s has no rationale", v.ID)
		}
	}

	// Stats carry the whole story.
	if run.Stats.Staged[VerdictAgree] != 1 || run.Stats.Staged[VerdictDowngrade] != 2 || run.Stats.Staged[VerdictFlagReview] != 1 {
		t.Fatalf("staged = %v", run.Stats.Staged)
	}
	if run.Stats.Dropped[RejectUpgrade] != 1 || run.Stats.Dropped["low_agreement"] != 1 {
		t.Fatalf("dropped = %v", run.Stats.Dropped)
	}
	if run.Stats.InputTokens != 250 || run.Stats.OutputTokens != 400 {
		t.Fatalf("tokens = %d/%d", run.Stats.InputTokens, run.Stats.OutputTokens)
	}

	// The rejected upgrade is in the ledger with its proposal recorded and
	// the monotonicity reason — logged, not applied.
	var rejected Verdict
	for _, v := range verdicts {
		if v.Status == VerdictRejected {
			rejected = v
		}
	}
	if rejected.ID == "" || !rejected.HasProposed || rejected.RejectionReason != RejectUpgrade {
		t.Fatalf("rejected verdict = %+v", rejected)
	}
}

func TestValidate_PromptShowsOnlyTheCandidatesEvidence(t *testing.T) {
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

	var factPrompt, discoveryPrompt, eventPrompt string
	for _, p := range validator.calls {
		switch {
		case strings.Contains(p, "STATEMENT: The Duke's men passed through"):
			factPrompt = p
		case strings.Contains(p, "METHOD: Tom told them at the Waystone"):
			discoveryPrompt = p
		case strings.Contains(p, "EVENT: The party is ambushed"):
			eventPrompt = p
		}
	}
	if factPrompt == "" || discoveryPrompt == "" || eventPrompt == "" {
		t.Fatalf("did not see all three prompts: %d calls", len(validator.calls))
	}

	// The system prompt's prime directive, verbatim in spirit.
	if !strings.Contains(validateSystemPrompt(), "Outside knowledge may deepen suspicion, never confirmation") {
		t.Fatal("system prompt lost the prime directive")
	}

	// The fact prompt: its quote, its window with the span marked, and only
	// its referenced entity — not the rest of the campaign's entity list.
	for _, want := range []string{
		"QUOTED SPAN", "CONTEXT WINDOW", "⟦", "⟧",
		"The Duke's men came through twice this month.",
		"REFERENCED CAMPAIGN ENTITIES",
	} {
		if !strings.Contains(factPrompt, want) {
			t.Errorf("fact prompt missing %q", want)
		}
	}
	if entitySection := sectionOf(factPrompt, "REFERENCED CAMPAIGN ENTITIES"); entitySection != "" {
		if n := strings.Count(entitySection, "\n- "); n != 1 {
			t.Errorf("fact prompt references %d entities, want 1 (the subject only):\n%s", n, entitySection)
		}
	} else {
		t.Error("fact prompt has no entity section terminator")
	}

	// The event prompt references its location and its participant: two.
	if entitySection := sectionOf(eventPrompt, "REFERENCED CAMPAIGN ENTITIES"); entitySection != "" {
		if n := strings.Count(entitySection, "\n- "); n != 2 {
			t.Errorf("event prompt references %d entities, want 2 (location + participant):\n%s", n, entitySection)
		}
	}

	// The discovery prompt asks the perception question and shows the fact
	// it claims was learned.
	for _, want := range []string{
		"DISCOVERY CHECK", "which character, in the fiction, perceived this",
		"THE FACT THIS DISCOVERY CLAIMS WAS LEARNED: The Duke's men passed through Blackwater twice this month.",
	} {
		if !strings.Contains(discoveryPrompt, want) {
			t.Errorf("discovery prompt missing %q", want)
		}
	}

	// The evidence boundary: the fact candidate references exactly one
	// entity (its subject, the Duke — an npc); the roster and the rest of
	// the campaign's entities never appear. The bullet count above pins
	// this; the referenced-entity kind makes it explicit.
	if entitySection := sectionOf(factPrompt, "REFERENCED CAMPAIGN ENTITIES"); !strings.Contains(entitySection, "(npc,") {
		t.Errorf("fact prompt's referenced entity should be the Duke (npc):\n%s", entitySection)
	}
}

// sectionOf returns the prompt section under a header, up to the blank line
// that ends it.
func sectionOf(prompt, header string) string {
	i := strings.Index(prompt, header)
	if i < 0 {
		return ""
	}
	rest := prompt[i:]
	if j := strings.Index(rest, "\n\n"); j >= 0 {
		return rest[:j]
	}
	return rest
}

func TestValidate_IsIdempotentUnderSameVersionAndChecksum(t *testing.T) {
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
	if len(validator.calls) != 5 {
		t.Fatalf("first run calls = %d", len(validator.calls))
	}

	// Re-run: same prompt version, same checksums — the verdict ledger skips
	// every candidate without a model call.
	validator.calls = nil
	run, err := s.Validate(ctx, ValidateInput{CampaignID: campaignID})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(validator.calls) != 0 {
		t.Fatalf("ledger skip still called the model: %d calls", len(validator.calls))
	}
	if run.Status != RunCompleted || run.Stats.UnitsSkipped != 5 || run.Stats.UnitsDone != 0 {
		t.Fatalf("second run = %+v", run.Stats)
	}

	// A changed candidate (a new checksum — here, a re-staged variant of the
	// same fact) is new work: exactly one call.
	var candID string
	if err := db.QueryRow(`SELECT id FROM canon_candidates WHERE kind = ? LIMIT 1`, KindFact).Scan(&candID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO canon_candidates (id, run_id, campaign_id, session_id, source_id, chunk_index, kind, payload, confidence, span_start, span_end, quote, checksum, created_at)
		SELECT 'cand-changed', run_id, campaign_id, session_id, source_id, chunk_index, kind, payload, confidence, span_start, span_end, quote, 'changed-checksum', created_at + 1
		FROM canon_candidates WHERE id = ?`, candID); err != nil {
		t.Fatal(err)
	}

	validator.calls = nil
	run, err = s.Validate(ctx, ValidateInput{CampaignID: campaignID})
	if err != nil {
		t.Fatalf("third run: %v", err)
	}
	if len(validator.calls) != 1 || run.Stats.UnitsSkipped != 5 || run.Stats.UnitsDone != 1 {
		t.Fatalf("changed checksum must re-validate: calls = %d stats = %+v", len(validator.calls), run.Stats)
	}
}

func TestValidate_BudgetGuardStopsTheRun(t *testing.T) {
	db, campaignID, _ := extractFixture(t)
	ctx := context.Background()

	// Each fake call costs 50*3/1e6 + 80*15/1e6 = 0.00135 USD; a budget of
	// 0.005 admits four calls and stops before the fifth (0.0054 >= 0.005).
	cfg := testConfig()
	cfg.BudgetUSD = 0.005
	cfg.PriceInMTok = 3
	cfg.PriceOutMTok = 15
	validator := &uniformVerdictModel{response: `{"verdict":"agree","agreement":0.9,"rationale":"fine","proposed_confidence":null}`}
	s, err := NewWithValidator(db, &fakeModel{}, validator, cfg)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.Validate(ctx, ValidateInput{CampaignID: campaignID})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunStopped || run.StopReason != StopBudget {
		t.Fatalf("run = %q/%q", run.Status, run.StopReason)
	}
	if run.Stats.Requests != 4 || run.Stats.UnitsDone != 4 {
		t.Fatalf("requests = %d done = %d, want 4/4 (budget checked before each call)", run.Stats.Requests, run.Stats.UnitsDone)
	}
	// The deferred remainder keeps its verdict-less state: a later run
	// picks the fifth candidate up.
	validator.calls = nil
	run, err = s.Validate(ctx, ValidateInput{CampaignID: campaignID})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunCompleted || run.Stats.UnitsDone != 1 || run.Stats.UnitsSkipped != 4 {
		t.Fatalf("resume run = %+v", run.Stats)
	}
}

func TestValidate_BatchSizeDefersTheRemainder(t *testing.T) {
	db, campaignID, _ := extractFixture(t)
	ctx := context.Background()

	cfg := testConfig()
	cfg.BatchSize = 2
	validator := &uniformVerdictModel{response: `{"verdict":"agree","agreement":0.9,"rationale":"fine","proposed_confidence":null}`}
	s, err := NewWithValidator(db, &fakeModel{}, validator, cfg)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.Validate(ctx, ValidateInput{CampaignID: campaignID})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunStopped || run.StopReason != StopUnits {
		t.Fatalf("run = %q/%q", run.Status, run.StopReason)
	}
	if len(validator.calls) != 2 || run.Stats.UnitsDone != 2 {
		t.Fatalf("calls = %d done = %d", len(validator.calls), run.Stats.UnitsDone)
	}
}

func TestValidate_UnparseableResponseFlagsRatherThanPasses(t *testing.T) {
	db, campaignID, cands := extractFixture(t)
	ctx := context.Background()

	validator := &uniformVerdictModel{response: "sorry, I cannot produce JSON today"}
	s, err := NewWithValidator(db, &fakeModel{}, validator, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.Validate(ctx, ValidateInput{CampaignID: campaignID})
	if err != nil {
		t.Fatalf("an unparseable response is data, not a run failure: %v", err)
	}
	if run.Status != RunCompleted {
		t.Fatalf("status = %q", run.Status)
	}
	if run.Stats.Dropped["unparseable_response"] != 5 || run.Stats.ParseProblems == 0 {
		t.Fatalf("stats = %+v", run.Stats)
	}
	verdicts, err := s.Verdicts(ctx, campaignID, VerdictFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(verdicts) != 5 {
		t.Fatalf("verdicts = %d", len(verdicts))
	}
	for _, v := range verdicts {
		if v.Verdict != VerdictFlagReview || v.Status != VerdictUnparseable || v.Agreement != 0 {
			t.Fatalf("verdict = %+v, want a conservative flag", v)
		}
		if !strings.Contains(v.Rationale, "no usable verdict") || !strings.Contains(v.Rationale, "no JSON object") {
			t.Fatalf("rationale = %q", v.Rationale)
		}
		if v.Raw != "sorry, I cannot produce JSON today" {
			t.Fatalf("raw not stored verbatim: %q", v.Raw)
		}
	}
	// No confidence moved: an unusable verdict applies nothing.
	after, err := s.ListCandidates(ctx, campaignID, CandidateFilter{})
	if err != nil {
		t.Fatal(err)
	}
	before := map[string]float64{}
	for _, c := range cands {
		before[c.ID] = c.Confidence
	}
	for _, c := range after {
		if c.Confidence != before[c.ID] {
			t.Fatalf("candidate %s confidence moved %v -> %v on an unparseable verdict", c.ID, before[c.ID], c.Confidence)
		}
	}
}

func TestValidate_FailedModelCallLeavesNoVerdictRow(t *testing.T) {
	db, campaignID, _ := extractFixture(t)
	ctx := context.Background()

	validator := &failingVerdictModel{}
	s, err := NewWithValidator(db, &fakeModel{}, validator, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.Validate(ctx, ValidateInput{CampaignID: campaignID})
	if err == nil {
		t.Fatal("a failed model call must surface the error")
	}
	if run == nil || run.Status != RunFailed || run.StopReason != StopError {
		t.Fatalf("run = %+v", run)
	}
	verdicts, err := s.Verdicts(ctx, campaignID, VerdictFilter{})
	if err != nil || len(verdicts) != 0 {
		t.Fatalf("failed run must leave no verdict rows: %+v err %v", verdicts, err)
	}
}

type failingVerdictModel struct{}

func (failingVerdictModel) ModelName() string { return "fake-validator" }

func (failingVerdictModel) Complete(ctx context.Context, system, user string) (Completion, error) {
	return Completion{}, fmt.Errorf("validator endpoint melted down")
}

/* ---------- the monotonicity guard under concurrent writers ---------- */

func TestValidate_DowngradeNeverRaisesAnAlreadyLoweredCandidate(t *testing.T) {
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

	// Another pass (the deterministic engine, say) has since lowered the
	// fact candidate below what this verdict proposed. Re-validating under
	// a changed checksum proposes 0.5 again; the guarded UPDATE must refuse
	// to raise 0.2 back to 0.5, and the verdict row must record the truth.
	var candID string
	if err := db.QueryRow(`SELECT id FROM canon_candidates WHERE kind = ?`, KindFact).Scan(&candID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE canon_candidates SET confidence = 0.2 WHERE id = ?`, candID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE canon_candidates SET checksum = 'changed-checksum' WHERE id = ?`, candID); err != nil {
		t.Fatal(err)
	}
	run, err := s.Validate(ctx, ValidateInput{CampaignID: campaignID})
	if err != nil {
		t.Fatal(err)
	}
	if run.Stats.UnitsDone != 1 {
		t.Fatalf("stats = %+v", run.Stats)
	}
	var conf float64
	if err := db.QueryRow(`SELECT confidence FROM canon_candidates WHERE id = ?`, candID).Scan(&conf); err != nil {
		t.Fatal(err)
	}
	if conf != 0.2 {
		t.Fatalf("confidence = %v, want 0.2 — a downgrade must never raise", conf)
	}
	verdicts, err := s.Verdicts(ctx, campaignID, VerdictFilter{CandidateID: candID})
	if err != nil {
		t.Fatal(err)
	}
	if len(verdicts) != 2 {
		t.Fatalf("verdicts = %d, want 2 (one per checksum)", len(verdicts))
	}
	// Same-millisecond creations order by random UUID id, so pick the
	// re-validation by its checksum rather than by position.
	var latest Verdict
	for _, v := range verdicts {
		if v.InputChecksum == "changed-checksum" {
			latest = v
		}
	}
	if latest.ID == "" {
		t.Fatal("no verdict under the changed checksum")
	}
	if latest.ConfidenceAfter != 0.2 {
		t.Fatalf("recorded confidence_after = %v, want the actual 0.2", latest.ConfidenceAfter)
	}
}

/* ---------- the migration ---------- */

// TestValidationMigrationPreservesExtractionData exercises the populated
// 6 -> 7 upgrade: an install that already ran extraction, with runs,
// candidates, drops and raw outputs, must come through with every row intact
// and FKs still live — the rebuild may not cascade anything away.
func TestValidationMigrationPreservesExtractionData(t *testing.T) {
	db, fx, sessionID := seeded(t)
	ctx := context.Background()
	addSource(t, db, sessionID, "transcript", fixtureTranscript)

	extractor := &fakeModel{responses: []string{fixtureResponse(fx)}}
	s := newStore(t, db, extractor, testConfig())
	run, err := s.Extract(ctx, ExtractInput{CampaignID: fx.Campaign.ID})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	counts := func() (runs, cands, drops, outs int) {
		for _, row := range []struct {
			q  string
			to *int
		}{
			{"SELECT COUNT(*) FROM canon_runs", &runs},
			{"SELECT COUNT(*) FROM canon_candidates", &cands},
			{"SELECT COUNT(*) FROM canon_drops", &drops},
			{"SELECT COUNT(*) FROM model_outputs", &outs},
		} {
			if err := db.QueryRow(row.q).Scan(row.to); err != nil {
				t.Fatalf("count: %v", err)
			}
		}
		return
	}
	beforeRuns, beforeCands, beforeDrops, beforeOuts := counts()
	if beforeRuns != 1 || beforeCands != 5 || beforeDrops != 3 || beforeOuts != 1 {
		t.Fatalf("fixture must have real extraction data: %d/%d/%d/%d", beforeRuns, beforeCands, beforeDrops, beforeOuts)
	}

	// 7 -> 6 -> 7 over the populated database.
	if err := migrate.Down(db); err != nil {
		t.Fatalf("down to 6: %v", err)
	}
	dRuns, dCands, dDrops, dOuts := counts()
	if dRuns != 1 || dCands != 5 || dDrops != 3 || dOuts != 1 {
		t.Fatalf("down lost extraction data: %d/%d/%d/%d", dRuns, dCands, dDrops, dOuts)
	}
	if err := migrate.Up(db); err != nil {
		t.Fatalf("back up to 7: %v", err)
	}
	aRuns, aCands, aDrops, aOuts := counts()
	if aRuns != 1 || aCands != 5 || aDrops != 3 || aOuts != 1 {
		t.Fatalf("upgrade lost extraction data: %d/%d/%d/%d", aRuns, aCands, aDrops, aOuts)
	}

	// The widened kind CHECK now accepts validate runs, the candidates'
	// provenance still resolves, and FKs still cascade on run delete.
	if _, err := db.Exec(`INSERT INTO canon_runs (id, campaign_id, session_id, kind, prompt_version, model, status, stats, error, created_at, updated_at)
		VALUES ('vrun', ?, ?, 'validate', ?, 'm', 'completed', '{}', '', 99, 99)`,
		fx.Campaign.ID, sessionID, VALIDATE_PROMPT_VERSION); err != nil {
		t.Fatalf("validate run rejected by the widened CHECK: %v", err)
	}
	var candID string
	if err := db.QueryRow(`SELECT id FROM canon_candidates LIMIT 1`).Scan(&candID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO canon_verdicts (id, run_id, campaign_id, candidate_id, prompt_version, input_checksum, verdict, status, agreement, rationale, proposed_confidence, confidence_before, confidence_after, rejection_reason, model, input_tokens, output_tokens, raw, created_at)
		VALUES ('vv', 'vrun', ?, ?, ?, 'k', 'flag_review', 'applied', 0.5, 'r', NULL, 0.9, 0.9, '', 'm', 1, 1, 'raw', 99)`,
		fx.Campaign.ID, candID, VALIDATE_PROMPT_VERSION); err != nil {
		t.Fatalf("insert verdict: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM canon_runs WHERE id = ?`, run.ID); err != nil {
		t.Fatalf("delete run: %v", err)
	}
	if n := func() int {
		var c int
		if err := db.QueryRow(`SELECT COUNT(*) FROM canon_candidates`).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}(); n != 0 {
		t.Fatalf("candidates after deleting their run = %d, want 0 (the cascade must still work)", n)
	}
}
