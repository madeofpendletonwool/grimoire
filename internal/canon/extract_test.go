package canon

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

/* ---------- chunking ---------- */

func TestChunkSource_SingleSmallSource(t *testing.T) {
	content := "one paragraph\n\ntwo paragraph"
	chunks := chunkSource(content, 12000, 800)
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d, want 1", len(chunks))
	}
	c := chunks[0]
	if c.Start != 0 || c.End != int64(len(content)) || c.Text != content {
		t.Fatalf("chunk = %+v", c)
	}
}

func TestChunkSource_CutsOnParagraphBoundaries(t *testing.T) {
	var paras []string
	for i := 0; i < 30; i++ {
		paras = append(paras, strings.Repeat("word ", 60)+"para "+string(rune('a'+i%26)))
	}
	content := strings.Join(paras, "\n\n")
	chunks := chunkSource(content, 2000, 300)
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want several", len(chunks))
	}
	for _, c := range chunks {
		if c.Start != 0 {
			// every non-first chunk begins right after a paragraph break
			if !strings.HasSuffix(content[:c.Start], "\n\n") && !strings.HasSuffix(content[:c.Start], "\n") {
				if c.Start > 0 && c.End-c.Start >= int64(2000) {
					// a full-size chunk must cut on a boundary
					t.Fatalf("chunk %d starts mid-line at %d", c.Index, c.Start)
				}
			}
		}
		if !utf8.ValidString(c.Text) {
			t.Fatalf("chunk %d is not valid UTF-8", c.Index)
		}
	}
	// Coverage: first chunk starts at zero, last ends at the content end,
	// and consecutive chunks overlap or abut with no gap.
	if chunks[0].Start != 0 {
		t.Fatalf("first chunk starts at %d", chunks[0].Start)
	}
	if chunks[len(chunks)-1].End != int64(len(content)) {
		t.Fatalf("last chunk ends at %d of %d", chunks[len(chunks)-1].End, len(content))
	}
	for i := 1; i < len(chunks); i++ {
		if chunks[i].Start >= chunks[i-1].End {
			t.Fatalf("gap between chunk %d and %d", i-1, i)
		}
	}
}

func TestChunkSource_BoundaryStraddlingQuoteIsWholeSomewhere(t *testing.T) {
	// A quote that crosses a chunk cut must appear fully inside at least
	// one chunk — that is what the overlap buys, and it is why the
	// span_outside_chunk rule never rejects a legitimate quote.
	var paras []string
	for i := 0; i < 24; i++ {
		paras = append(paras, strings.Repeat("filler filler filler ", 40))
	}
	tail := "the tail words of one paragraph"
	head := "the head words of the next paragraph"
	paras = append(paras, tail, head)
	for i := 0; i < 10; i++ {
		paras = append(paras, strings.Repeat("trailing filler ", 50))
	}
	content := strings.Join(paras, "\n\n")
	straddle := tail + "\n\n" + head
	if !strings.Contains(content, straddle) {
		t.Fatal("fixture is wrong: straddling quote not in content")
	}
	chunks := chunkSource(content, 1500, 400)
	wholeSomewhere := false
	for _, c := range chunks {
		if strings.Contains(c.Text, straddle) {
			wholeSomewhere = true
		}
	}
	if !wholeSomewhere {
		t.Fatal("a paragraph-straddling quote appears in no single chunk")
	}
}

func TestChunkSource_RuneSafeHardCut(t *testing.T) {
	// One long line of multibyte characters forces the hard-cut path; the
	// cut must land on a rune boundary.
	content := strings.Repeat("骷髅王在他的宝座上", 4000) // 10 runes, 30 bytes per repetition
	chunks := chunkSource(content, 997, 100)
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want several", len(chunks))
	}
	for _, c := range chunks {
		if !utf8.ValidString(c.Text) {
			t.Fatalf("chunk %d is not valid UTF-8", c.Index)
		}
	}
}

/* ---------- drop rules: one test per rule ---------- */

// vctxFixture builds the validation context the drop-rule tests run against:
// a small source, one chunk covering all of it, two known campaign entities,
// and a relationship vocabulary.
func vctxFixture(content string) validateContext {
	return validateContext{
		sourceContent: content,
		chunk:         Chunk{Index: 0, Start: 0, End: int64(len(content)), Text: content},
		knownEntities: map[string]string{"mira": "pc", "tom": "npc", "tavern": "location"},
		relTypes:      map[string]bool{"knows": true, "located_in": true, "enemy_of": true},
	}
}

const dropFixtureContent = `Mira enters the tavern. Tom nods.

Tom says the robed folk came by at dusk. Mira writes this down.`

func factOf(localID, quote string, mutate ...func(*WireFact)) WireFact {
	f := WireFact{
		LocalID: localID, Statement: "a statement", Subject: "mira", Predicate: "met",
		ObjectLiteral: "tom", Visibility: "public", Quote: quote, Confidence: 0.9,
	}
	for _, m := range mutate {
		if m != nil {
			m(&f)
		}
	}
	return f
}

func dropReasons(drops []Drop) map[string]Drop {
	out := map[string]Drop{}
	for _, d := range drops {
		out[d.Reason] = d
	}
	return out
}

func TestDropRule_Uncited(t *testing.T) {
	vctx := vctxFixture(dropFixtureContent)
	staged, drops := validatePayload(WirePayload{Facts: []WireFact{factOf("f1", "", nil)}}, vctx, map[string]bool{})
	if len(staged) != 0 || dropReasons(drops)[DropUncited].Reason == "" {
		t.Fatalf("staged=%d drops=%v", len(staged), drops)
	}
}

func TestDropRule_QuoteNotInSource(t *testing.T) {
	vctx := vctxFixture(dropFixtureContent)
	_, drops := validatePayload(WirePayload{Facts: []WireFact{
		factOf("f1", "words that appear nowhere in the source"),
	}}, vctx, map[string]bool{})
	if dropReasons(drops)[DropQuoteNotInSource].Reason == "" {
		t.Fatalf("drops=%v", drops)
	}
}

func TestDropRule_SpanOutsideChunk(t *testing.T) {
	vctx := vctxFixture(dropFixtureContent)
	// Shrink the chunk to the first line; the quote is real but the model
	// was never shown it.
	vctx.chunk = Chunk{Index: 0, Start: 0, End: 24, Text: dropFixtureContent[:24]}
	_, drops := validatePayload(WirePayload{Facts: []WireFact{
		factOf("f1", "Mira writes this down."),
	}}, vctx, map[string]bool{})
	if dropReasons(drops)[DropSpanOutsideChunk].Reason == "" {
		t.Fatalf("drops=%v", drops)
	}
}

func TestDropRule_InvalidLocalID(t *testing.T) {
	vctx := vctxFixture(dropFixtureContent)
	_, drops := validatePayload(WirePayload{Facts: []WireFact{
		factOf("Not A Slug", "Mira enters the tavern."),
	}}, vctx, map[string]bool{})
	if dropReasons(drops)[DropInvalidID].Reason == "" {
		t.Fatalf("drops=%v", drops)
	}
}

func TestDropRule_DuplicateLocalID(t *testing.T) {
	vctx := vctxFixture(dropFixtureContent)
	_, drops := validatePayload(WirePayload{Facts: []WireFact{
		factOf("f1", "Mira enters the tavern.", nil),
		factOf("f1", "Tom nods."),
	}}, vctx, map[string]bool{})
	if dropReasons(drops)[DropDuplicateID].Reason == "" {
		t.Fatalf("drops=%v", drops)
	}
}

func TestDropRule_NewEntityShadowsCampaignEntity(t *testing.T) {
	vctx := vctxFixture(dropFixtureContent)
	wire := WirePayload{NewEntities: []WireEntity{{
		LocalID: "mira", Kind: "npc", Name: "Mira Again",
		Quote: "Mira enters the tavern.", Confidence: 0.9,
	}}}
	_, drops := validatePayload(wire, vctx, map[string]bool{})
	d := dropReasons(drops)[DropDuplicateID]
	if d.Reason == "" || !strings.Contains(d.Detail, "campaign entity") {
		t.Fatalf("drops=%v", drops)
	}
}

func TestDropRule_InvalidEntityKind(t *testing.T) {
	vctx := vctxFixture(dropFixtureContent)
	wire := WirePayload{NewEntities: []WireEntity{{
		LocalID: "robed-folk", Kind: "person", Name: "The Robed Folk",
		Quote: "the robed folk came by", Confidence: 0.9,
	}}}
	_, drops := validatePayload(wire, vctx, map[string]bool{})
	if dropReasons(drops)[DropInvalidKind].Reason == "" {
		t.Fatalf("drops=%v", drops)
	}
}

func TestDropRule_UnknownEntity(t *testing.T) {
	vctx := vctxFixture(dropFixtureContent)
	_, drops := validatePayload(WirePayload{Facts: []WireFact{
		factOf("f1", "Mira enters the tavern.", func(f *WireFact) { f.Subject = "stranger-who-is-nowhere" }),
	}}, vctx, map[string]bool{})
	if dropReasons(drops)[DropUnknownEntity].Reason == "" {
		t.Fatalf("drops=%v", drops)
	}
}

func TestDropRule_UnknownEntityResolvesThroughPayloadEntity(t *testing.T) {
	// The positive control for unknown_entity: a reference that resolves
	// through a kept new-entity stages fine.
	vctx := vctxFixture(dropFixtureContent)
	wire := WirePayload{
		NewEntities: []WireEntity{{
			LocalID: "robed-folk", Kind: "faction", Name: "The Robed Folk",
			Quote: "the robed folk came by", Confidence: 0.9,
		}},
		Facts: []WireFact{{
			LocalID: "f1", Statement: "Mira met the robed folk's trail.",
			Subject: "mira", Predicate: "met", ObjectEntity: "robed-folk",
			Visibility: "public", Quote: "the robed folk came by", Confidence: 0.8,
		}},
	}
	staged, drops := validatePayload(wire, vctx, map[string]bool{})
	if len(staged) != 2 {
		t.Fatalf("staged=%d drops=%v", len(staged), drops)
	}
}

func TestDropRule_InvalidRelType(t *testing.T) {
	vctx := vctxFixture(dropFixtureContent)
	_, drops := validatePayload(WirePayload{Relationships: []WireRelationship{{
		FromEntity: "tom", RelType: "frenemy_of", ToEntity: "mira",
		Quote: "Tom nods.", Confidence: 0.9,
	}}}, vctx, map[string]bool{})
	if dropReasons(drops)[DropInvalidRelType].Reason == "" {
		t.Fatalf("drops=%v", drops)
	}
}

func TestDropRule_SelfRelationship(t *testing.T) {
	vctx := vctxFixture(dropFixtureContent)
	_, drops := validatePayload(WirePayload{Relationships: []WireRelationship{{
		FromEntity: "tom", RelType: "knows", ToEntity: "tom",
		Quote: "Tom nods.", Confidence: 0.9,
	}}}, vctx, map[string]bool{})
	d := dropReasons(drops)[DropInvalidRelType]
	if d.Reason == "" || !strings.Contains(d.Detail, "self") {
		t.Fatalf("drops=%v", drops)
	}
}

func TestDropRule_InvalidStance(t *testing.T) {
	vctx := vctxFixture(dropFixtureContent)
	wire := WirePayload{
		Facts: []WireFact{factOf("f1", "Mira enters the tavern.", nil)},
		Discoveries: []WireDiscovery{{
			Fact: "f1", DiscoveredBy: "mira", Stance: "pretty sure",
			Method: "saw it", Quote: "Mira enters the tavern.", Confidence: 0.9,
		}},
	}
	_, drops := validatePayload(wire, vctx, map[string]bool{})
	if dropReasons(drops)[DropInvalidStance].Reason == "" {
		t.Fatalf("drops=%v", drops)
	}
}

func TestDropRule_InvalidVisibility(t *testing.T) {
	vctx := vctxFixture(dropFixtureContent)
	_, drops := validatePayload(WirePayload{Facts: []WireFact{
		factOf("f1", "Mira enters the tavern.", func(f *WireFact) { f.Visibility = "hidden" }),
	}}, vctx, map[string]bool{})
	if dropReasons(drops)[DropInvalidVisibility].Reason == "" {
		t.Fatalf("drops=%v", drops)
	}
}

func TestDropRule_InvalidConfidence(t *testing.T) {
	vctx := vctxFixture(dropFixtureContent)
	_, drops := validatePayload(WirePayload{Facts: []WireFact{
		factOf("f1", "Mira enters the tavern.", func(f *WireFact) { f.Confidence = 1.5 }),
	}}, vctx, map[string]bool{})
	if dropReasons(drops)[DropInvalidConfidence].Reason == "" {
		t.Fatalf("drops=%v", drops)
	}
}

func TestDropRule_InvalidObject(t *testing.T) {
	vctx := vctxFixture(dropFixtureContent)
	_, drops := validatePayload(WirePayload{Facts: []WireFact{
		factOf("f1", "Mira enters the tavern.", func(f *WireFact) { f.ObjectEntity = "tavern" }),
		// object_entity AND object_literal both set now
	}}, vctx, map[string]bool{})
	if dropReasons(drops)[DropInvalidObject].Reason == "" {
		t.Fatalf("drops=%v", drops)
	}
	_, drops = validatePayload(WirePayload{Facts: []WireFact{
		factOf("f1", "Mira enters the tavern.", func(f *WireFact) { f.ObjectLiteral = "" }),
		// neither set now
	}}, vctx, map[string]bool{})
	if dropReasons(drops)[DropInvalidObject].Reason == "" {
		t.Fatalf("drops=%v", drops)
	}
}

func TestDropRule_DanglingFactRef(t *testing.T) {
	vctx := vctxFixture(dropFixtureContent)
	_, drops := validatePayload(WirePayload{Discoveries: []WireDiscovery{{
		Fact: "fact-that-was-dropped", DiscoveredBy: "mira", Stance: "knows",
		Method: "saw it", Quote: "Mira enters the tavern.", Confidence: 0.9,
	}}}, vctx, map[string]bool{})
	if dropReasons(drops)[DropDanglingFactRef].Reason == "" {
		t.Fatalf("drops=%v", drops)
	}
}

func TestDropRule_EmptyStatement(t *testing.T) {
	vctx := vctxFixture(dropFixtureContent)
	_, drops := validatePayload(WirePayload{Facts: []WireFact{
		factOf("f1", "Mira enters the tavern.", func(f *WireFact) { f.Statement = "  " }),
	}}, vctx, map[string]bool{})
	if dropReasons(drops)[DropEmptyStatement].Reason == "" {
		t.Fatalf("drops=%v", drops)
	}
}

func TestDropRule_DuplicateCandidate(t *testing.T) {
	vctx := vctxFixture(dropFixtureContent)
	f := factOf("f1", "Mira enters the tavern.", nil)
	// Pre-seed the run-level seen map as though the identical candidate
	// was already staged from an overlapping chunk.
	_, drops := validatePayload(WirePayload{Facts: []WireFact{f}}, vctx, map[string]bool{})
	if len(drops) != 0 {
		t.Fatalf("first occurrence must stage: %v", drops)
	}
	seen := map[string]bool{}
	staged1, _ := validatePayload(WirePayload{Facts: []WireFact{f}}, vctx, seen)
	for _, c := range staged1 {
		seen[c.Checksum] = true
	}
	_, drops = validatePayload(WirePayload{Facts: []WireFact{f}}, vctx, seen)
	if dropReasons(drops)[DropDuplicateCand].Reason == "" {
		t.Fatalf("drops=%v", drops)
	}
}

func TestValidStagedCandidateCarriesSpanAndChecksum(t *testing.T) {
	vctx := vctxFixture(dropFixtureContent)
	quote := "Mira enters the tavern."
	staged, drops := validatePayload(WirePayload{Facts: []WireFact{
		factOf("f1", quote),
	}}, vctx, map[string]bool{})
	if len(drops) != 0 || len(staged) != 1 {
		t.Fatalf("staged=%d drops=%v", len(staged), drops)
	}
	c := staged[0]
	if c.SpanStart != 0 || c.SpanEnd != int64(len(quote)) {
		t.Fatalf("span = [%d,%d), want [0,%d)", c.SpanStart, c.SpanEnd, len(quote))
	}
	if c.Quote != quote || c.Checksum == "" {
		t.Fatalf("candidate = %+v", c)
	}
	if !strings.Contains(string(c.Payload), `"local_id":"f1"`) {
		t.Fatalf("payload = %s", c.Payload)
	}
}

/* ---------- prompts and config ---------- */

func TestPromptVersionPinned(t *testing.T) {
	if PROMPT_VERSION != "canon-extract-001" {
		t.Fatalf("PROMPT_VERSION = %q", PROMPT_VERSION)
	}
}

func TestSystemPromptCarriesThePrimeDirective(t *testing.T) {
	sp := systemPrompt()
	for _, want := range []string{
		"EXTRACTOR, NOT ORACLE", "VERBATIM", "FORBIDDEN", "one atomic statement",
	} {
		if !strings.Contains(sp, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
}

func TestUserPromptRendersContext(t *testing.T) {
	tctx := taskContext{
		CampaignName: "The Withering Kingdom", CampaignClock: 41,
		SourceKind: "transcript", SourceAuthor: "DM",
		Entities: []promptEntity{{ID: "mira", Kind: "pc", Name: "Mira", Aliases: []string{"the wizard"}}},
		Roster:   []promptEntity{{ID: "mira", Kind: "pc", Name: "Mira"}},
		RelTypes: []string{"knows", "located_in"},
	}
	up := userPrompt(tctx, "chunk text here", 12, 34)
	for _, want := range []string{
		"The Withering Kingdom", "in-world day 41", "transcript", "by DM",
		"mira (pc, Mira; aka the wizard)", "knows, located_in", "PARTY ROSTER",
		"bytes 12..34", "chunk text here",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("user prompt missing %q", want)
		}
	}
}

func TestInputChecksumTracksCampaignState(t *testing.T) {
	u := unit{Checksum: "abc"}
	chunks := chunkSource("some content\n\nmore content", 12000, 800)
	base := campaignContext{
		Entities: []promptEntity{{ID: "mira", Kind: "pc", Name: "Mira"}},
		RelTypes: []string{"knows"},
	}
	a := inputChecksum(u, chunks, &base)
	if a != inputChecksum(u, chunks, &base) {
		t.Fatal("checksum is not deterministic")
	}
	changed := campaignContext{
		Entities: append([]promptEntity{}, base.Entities...),
		RelTypes: base.RelTypes,
	}
	changed.Entities = append(changed.Entities, promptEntity{ID: "tom", Kind: "npc", Name: "Tom"})
	if a == inputChecksum(u, chunks, &changed) {
		t.Fatal("checksum must change when the entity list changes")
	}
}

func TestCostUSD(t *testing.T) {
	cfg := Config{PriceInMTok: 3, PriceOutMTok: 15}
	got := cfg.costUSD(1_000_000, 1_000_000)
	if got < 17.99 || got > 18.01 {
		t.Fatalf("costUSD = %v, want 18", got)
	}
	if (Config{}).costUSD(1000, 1000) != 0 {
		t.Fatal("unknown prices must cost zero, never a guess")
	}
}

func TestConfigFromEnv(t *testing.T) {
	cfg := ConfigFromEnv(func(k string) string {
		switch k {
		case "CANON_BUDGET_USD":
			return "5.5"
		case "CANON_MAX_CANDIDATES":
			return "25"
		case "CANON_BATCH_SIZE":
			return "3"
		case "CANON_REQUEST_INTERVAL":
			return "1500ms"
		case "CANON_PRICE_IN_MTOK":
			return "2"
		case "CANON_PRICE_OUT_MTOK":
			return "8"
		}
		return ""
	})
	if cfg.BudgetUSD != 5.5 || cfg.MaxCandidates != 25 || cfg.BatchSize != 3 ||
		cfg.Interval != 1500*time.Millisecond || cfg.PriceInMTok != 2 || cfg.PriceOutMTok != 8 {
		t.Fatalf("cfg = %+v", cfg)
	}
	// Junk falls back to defaults, never panics.
	def := DefaultConfig()
	cfg = ConfigFromEnv(func(k string) string {
		if k == "CANON_MAX_CANDIDATES" {
			return "not-a-number"
		}
		if k == "CANON_REQUEST_INTERVAL" {
			return "tomorrow"
		}
		return ""
	})
	if cfg.MaxCandidates != def.MaxCandidates || cfg.Interval != def.Interval {
		t.Fatalf("junk values must fall back: %+v", cfg)
	}
}

func TestWaitIntervalSpacesCalls(t *testing.T) {
	s := &Store{cfg: Config{Interval: 40 * time.Millisecond}, now: time.Now().UTC}
	base := time.Now().UTC()
	s.now = func() time.Time { return base }
	last := base
	if err := s.waitInterval(context.Background(), &last); err != nil {
		t.Fatal("first call must not wait")
	}
	// Advance the fake clock 10ms of the 40ms interval: the next call must
	// wait out the remaining ~30ms in real time.
	s.now = func() time.Time { return base.Add(10 * time.Millisecond) }
	start := time.Now()
	if err := s.waitInterval(context.Background(), &last); err != nil {
		t.Fatalf("waitInterval: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 25*time.Millisecond {
		t.Fatalf("waited only %v, want ~30ms", elapsed)
	}
}
