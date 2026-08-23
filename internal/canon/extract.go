package canon

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

/* ---------- work units and chunking ---------- */

// unit is one work unit: a single session_source with everything extraction
// needs from its row.
type unit struct {
	SourceID  string
	SessionID string
	Kind      string
	Author    string
	Title     string
	Content   string
	Checksum  string
}

// Chunk is one slice of a source, cut on a paragraph boundary when possible
// so a quote is never split mid-sentence by construction. Start and End are
// byte offsets into the source content, half-open: content[start:end].
type Chunk struct {
	Index int
	Start int64
	End   int64
	Text  string
}

// chunkSource cuts content into chunks of roughly target bytes with overlap
// bytes carried forward, preferring paragraph breaks ("\n\n"), then line
// breaks, then a hard cut backed off to a rune boundary. The overlap means a
// span straddling one chunk's end appears whole in the next chunk too — and
// the validator still refuses any quote that is not fully inside the chunk
// the model saw, so a boundary is enforced, never trusted.
func chunkSource(content string, target, overlap int) []Chunk {
	if target <= 0 {
		target = ChunkTarget
	}
	if overlap < 0 || overlap >= target {
		overlap = 0
	}
	var chunks []Chunk
	start := int64(0)
	for start < int64(len(content)) {
		end := start + int64(target)
		if end >= int64(len(content)) {
			end = int64(len(content))
		} else {
			if cut := lastBreak(content, start, end); cut > start {
				end = cut
			} else {
				// hard cut: back off to a rune boundary so offsets
				// stay valid string indices
				for end > start && !utf8.RuneStart(content[end]) {
					end--
				}
			}
		}
		chunks = append(chunks, Chunk{
			Index: len(chunks), Start: start, End: end,
			Text: content[start:end],
		})
		if end >= int64(len(content)) {
			break
		}
		next := end - int64(overlap)
		// the overlap itself must start on a rune boundary
		for next > start && !utf8.RuneStart(content[next]) {
			next--
		}
		if next <= start {
			next = end // degenerate overlap; advance to avoid an infinite loop
		}
		start = next
	}
	return chunks
}

// lastBreak finds the latest paragraph or line break strictly before limit
// and at or after minStart, returning the offset just after the break, or 0
// when none exists.
func lastBreak(content string, minStart, limit int64) int64 {
	window := content[minStart:limit]
	best := int64(-1)
	for _, sep := range []string{"\n\n", "\n"} {
		if i := strings.LastIndex(window, sep); i >= 0 {
			cut := minStart + int64(i+len(sep))
			if cut > best {
				best = cut
			}
		}
	}
	if best <= minStart {
		return 0
	}
	return best
}

/* ---------- staged candidates and drops ---------- */

// Staged is one candidate that passed every rule, ready to write. Payload is
// the normalized wire record as JSON (shape varies by kind); Checksum hashes
// kind, payload and span, and is the key the adversarial pass (MAD-308)
// records verdicts against.
type Staged struct {
	Kind       string
	Payload    []byte
	Confidence float64
	SpanStart  int64
	SpanEnd    int64
	Quote      string
	ChunkIndex int
	Checksum   string
}

// Drop is one candidate the rules rejected. Reasons are the Drop* vocabulary;
// every drop is data — logged per unit and counted in run stats.
type Drop struct {
	Kind       string // candidate kind, or "payload" for parse-level problems
	Ref        string
	Reason     string
	Detail     string
	ChunkIndex int
}

/* ---------- validation context ---------- */

// validateContext is everything the drop rules need beyond the payload: the
// chunk the model saw, the source behind it, and the campaign's entity graph
// and vocabularies. Pure data — the validation itself is a pure function.
type validateContext struct {
	sourceContent string
	chunk         Chunk
	// knownEntities maps campaign entity id -> kind.
	knownEntities map[string]string
	// relTypes is the allowed relationship vocabulary.
	relTypes map[string]bool
}

// slugRe is the local-id grammar, same as Arda's: lowercase slugs only.
const slugChars = "abcdefghijklmnopqrstuvwxyz0123456789_-"

func validSlug(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune(slugChars, r) {
			return false
		}
	}
	return true
}

// resolveQuote locates a candidate's quote against the chunk the model saw
// and the source behind it. Returns the span and a drop reason: "" when the
// quote is verbatim inside the chunk; quote_not_in_source when the quote is
// nowhere in the source; span_outside_chunk when it is in the source but not
// in the chunk (the model cited provenance it was not given).
func resolveQuote(vctx *validateContext, quote string) (start, end int64, reason string) {
	if strings.TrimSpace(quote) == "" {
		return 0, 0, DropUncited
	}
	if i := strings.Index(vctx.chunk.Text, quote); i >= 0 {
		s := vctx.chunk.Start + int64(i)
		return s, s + int64(len(quote)), ""
	}
	if strings.Contains(vctx.sourceContent, quote) {
		return 0, 0, DropSpanOutsideChunk
	}
	return 0, 0, DropQuoteNotInSource
}

// validatePayload applies every drop rule to one parsed response and returns
// the candidates worth staging plus the drops. It is pure: same payload and
// context, same result — the property that makes a per-rule test honest.
//
// Order mirrors Arda's: entities first (so later kinds resolve against the
// survivors), then facts, events, relationships, and finally discoveries,
// which may only reference facts this validation kept.
func validatePayload(wire WirePayload, vctx validateContext, seen map[string]bool) ([]Staged, []Drop) {
	var staged []Staged
	var drops []Drop

	stage := func(kind string, payload []byte, confidence float64, spanStart, spanEnd int64, quote string, chunkIndex int, ref string) {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d,%d\x00%s", kind, payload, spanStart, spanEnd, quote)))
		check := hex.EncodeToString(sum[:])
		if seen[check] {
			drops = append(drops, Drop{Kind: kind, Ref: ref, Reason: DropDuplicateCand, Detail: "identical candidate already staged this run", ChunkIndex: chunkIndex})
			return
		}
		seen[check] = true
		staged = append(staged, Staged{
			Kind: kind, Payload: payload, Confidence: confidence,
			SpanStart: spanStart, SpanEnd: spanEnd, Quote: quote,
			ChunkIndex: chunkIndex, Checksum: check,
		})
	}
	drop := func(kind, ref, reason, detail string, chunkIndex int) {
		drops = append(drops, Drop{Kind: kind, Ref: ref, Reason: reason, Detail: detail, ChunkIndex: chunkIndex})
	}

	// pass runs the shared per-candidate checks: confidence range and the
	// span rule. ok=false means dropped.
	pass := func(kind, ref string, confidence float64, quote string) (int64, int64, bool) {
		if confidence < 0 || confidence > 1 {
			drop(kind, ref, DropInvalidConfidence, fmt.Sprintf("confidence %v outside 0..1", confidence), vctx.chunk.Index)
			return 0, 0, false
		}
		start, end, reason := resolveQuote(&vctx, quote)
		if reason != "" {
			drop(kind, ref, reason, "", vctx.chunk.Index)
			return 0, 0, false
		}
		return start, end, true
	}

	/* entities */
	payloadEntities := map[string]WireEntity{} // local_id -> record, survivors only
	localIDs := map[string]bool{}              // all local ids declared in this payload
	for _, e := range wire.NewEntities {
		ref := e.LocalID
		if !validSlug(e.LocalID) {
			drop(KindEntity, ref, DropInvalidID, "local_id must be a lowercase slug", vctx.chunk.Index)
			continue
		}
		if localIDs[e.LocalID] {
			drop(KindEntity, ref, DropDuplicateID, "local_id declared twice", vctx.chunk.Index)
			continue
		}
		if _, exists := vctx.knownEntities[e.LocalID]; exists {
			drop(KindEntity, ref, DropDuplicateID, "local_id shadows a campaign entity; reference it instead", vctx.chunk.Index)
			continue
		}
		if !validEntityKind(e.Kind) {
			drop(KindEntity, ref, DropInvalidKind, fmt.Sprintf("kind %q", e.Kind), vctx.chunk.Index)
			continue
		}
		if strings.TrimSpace(e.Name) == "" {
			drop(KindEntity, ref, DropEmptyStatement, "name is required", vctx.chunk.Index)
			continue
		}
		start, end, ok := pass(KindEntity, ref, float64(e.Confidence), e.Quote)
		if !ok {
			continue
		}
		localIDs[e.LocalID] = true
		payloadEntities[e.LocalID] = e
		payload, _ := json.Marshal(map[string]any{
			"local_id": e.LocalID, "kind": e.Kind,
			"name": strings.TrimSpace(e.Name), "summary": strings.TrimSpace(e.Summary),
		})
		stage(KindEntity, payload, float64(e.Confidence), start, end, e.Quote, vctx.chunk.Index, ref)
	}

	// resolveEntity reports whether an entity reference resolves: a campaign
	// entity id, or a new-entity local_id this payload declared and kept.
	resolveEntity := func(ref string) (kind string, ok bool) {
		if k, exists := vctx.knownEntities[ref]; exists {
			return k, true
		}
		if e, exists := payloadEntities[ref]; exists {
			return e.Kind, true
		}
		return "", false
	}

	/* facts */
	keptFacts := map[string]bool{}
	for _, f := range wire.Facts {
		ref := f.LocalID
		if !validSlug(ref) {
			drop(KindFact, ref, DropInvalidID, "local_id must be a lowercase slug", vctx.chunk.Index)
			continue
		}
		if keptFacts[ref] || localIDs[ref] {
			drop(KindFact, ref, DropDuplicateID, "local_id already used this payload", vctx.chunk.Index)
			continue
		}
		if strings.TrimSpace(f.Statement) == "" {
			drop(KindFact, ref, DropEmptyStatement, "statement is required", vctx.chunk.Index)
			continue
		}
		if strings.TrimSpace(f.Predicate) == "" {
			drop(KindFact, ref, DropEmptyStatement, "predicate is required", vctx.chunk.Index)
			continue
		}
		if (f.ObjectEntity == "") == (strings.TrimSpace(f.ObjectLiteral) == "") {
			drop(KindFact, ref, DropInvalidObject, "object is an entity or a literal, never both and never neither", vctx.chunk.Index)
			continue
		}
		visibility := f.Visibility
		if visibility == "" {
			visibility = "public"
		}
		if visibility != "public" && visibility != "secret" {
			drop(KindFact, ref, DropInvalidVisibility, fmt.Sprintf("visibility %q", f.Visibility), vctx.chunk.Index)
			continue
		}
		dangling := ""
		for _, pair := range []struct{ ref, label string }{{f.Subject, "subject"}, {f.ObjectEntity, "object"}} {
			if pair.ref == "" {
				continue
			}
			if _, ok := resolveEntity(pair.ref); !ok {
				dangling = fmt.Sprintf("%s %q resolves nowhere", pair.label, pair.ref)
				break
			}
		}
		if dangling != "" {
			drop(KindFact, ref, DropUnknownEntity, dangling, vctx.chunk.Index)
			continue
		}
		start, end, ok := pass(KindFact, ref, float64(f.Confidence), f.Quote)
		if !ok {
			continue
		}
		keptFacts[ref] = true
		payload, _ := json.Marshal(map[string]any{
			"local_id": ref, "statement": strings.TrimSpace(f.Statement),
			"subject": f.Subject, "predicate": strings.TrimSpace(f.Predicate),
			"object_entity": f.ObjectEntity, "object_literal": strings.TrimSpace(f.ObjectLiteral),
			"visibility": visibility,
		})
		stage(KindFact, payload, float64(f.Confidence), start, end, f.Quote, vctx.chunk.Index, ref)
	}

	/* events */
	for _, ev := range wire.Events {
		ref := ev.LocalID
		if !validSlug(ref) {
			drop(KindEvent, ref, DropInvalidID, "local_id must be a lowercase slug", vctx.chunk.Index)
			continue
		}
		if localIDs[ref] {
			drop(KindEvent, ref, DropDuplicateID, "local_id already used this payload", vctx.chunk.Index)
			continue
		}
		if strings.TrimSpace(ev.Summary) == "" {
			drop(KindEvent, ref, DropEmptyStatement, "summary is required", vctx.chunk.Index)
			continue
		}
		if ev.Location != "" {
			if _, ok := resolveEntity(ev.Location); !ok {
				drop(KindEvent, ref, DropUnknownEntity, fmt.Sprintf("location %q resolves nowhere", ev.Location), vctx.chunk.Index)
				continue
			}
		}
		participants := make([]map[string]any, 0, len(ev.Participants))
		badParticipant := false
		for _, p := range ev.Participants {
			if _, ok := resolveEntity(p.Entity); !ok {
				drop(KindEvent, ref, DropUnknownEntity, fmt.Sprintf("participant %q resolves nowhere", p.Entity), vctx.chunk.Index)
				badParticipant = true
				break
			}
			participants = append(participants, map[string]any{"entity": p.Entity, "role": p.Role})
		}
		if badParticipant {
			continue
		}
		start, end, ok := pass(KindEvent, ref, float64(ev.Confidence), ev.Quote)
		if !ok {
			continue
		}
		event := map[string]any{
			"local_id": ref, "summary": strings.TrimSpace(ev.Summary),
			"location": ev.Location, "participants": participants,
		}
		if ev.ClockAt != nil {
			event["clock_at"] = int64(*ev.ClockAt)
		}
		payload, _ := json.Marshal(event)
		stage(KindEvent, payload, float64(ev.Confidence), start, end, ev.Quote, vctx.chunk.Index, ref)
	}

	/* relationships */
	for _, r := range wire.Relationships {
		ref := fmt.Sprintf("%s-%s-%s", r.FromEntity, r.RelType, r.ToEntity)
		if !vctx.relTypes[r.RelType] {
			drop(KindRelationship, ref, DropInvalidRelType, fmt.Sprintf("rel_type %q is not in the vocabulary", r.RelType), vctx.chunk.Index)
			continue
		}
		if r.FromEntity == r.ToEntity {
			drop(KindRelationship, ref, DropInvalidRelType, "self-relationship", vctx.chunk.Index)
			continue
		}
		fromOK := false
		toOK := false
		if _, ok := resolveEntity(r.FromEntity); ok {
			fromOK = true
		}
		if _, ok := resolveEntity(r.ToEntity); ok {
			toOK = true
		}
		if !fromOK || !toOK {
			missing := r.FromEntity
			if fromOK {
				missing = r.ToEntity
			}
			drop(KindRelationship, ref, DropUnknownEntity, fmt.Sprintf("endpoint %q resolves nowhere", missing), vctx.chunk.Index)
			continue
		}
		start, end, ok := pass(KindRelationship, ref, float64(r.Confidence), r.Quote)
		if !ok {
			continue
		}
		payload, _ := json.Marshal(map[string]any{
			"from_entity": r.FromEntity, "rel_type": r.RelType, "to_entity": r.ToEntity,
		})
		stage(KindRelationship, payload, float64(r.Confidence), start, end, r.Quote, vctx.chunk.Index, ref)
	}

	/* discoveries */
	for _, d := range wire.Discoveries {
		ref := fmt.Sprintf("%s/%s", d.Fact, d.DiscoveredBy)
		if !keptFacts[d.Fact] {
			drop(KindDiscovery, ref, DropDanglingFactRef, fmt.Sprintf("fact %q was not kept by this payload", d.Fact), vctx.chunk.Index)
			continue
		}
		if d.DiscoveredBy != "party" {
			if _, ok := resolveEntity(d.DiscoveredBy); !ok {
				drop(KindDiscovery, ref, DropUnknownEntity, fmt.Sprintf("discovered_by %q resolves nowhere", d.DiscoveredBy), vctx.chunk.Index)
				continue
			}
		}
		if d.Stance != "knows" && d.Stance != "suspects" && d.Stance != "believes_false" {
			drop(KindDiscovery, ref, DropInvalidStance, fmt.Sprintf("stance %q", d.Stance), vctx.chunk.Index)
			continue
		}
		start, end, ok := pass(KindDiscovery, ref, float64(d.Confidence), d.Quote)
		if !ok {
			continue
		}
		payload, _ := json.Marshal(map[string]any{
			"fact": d.Fact, "discovered_by": d.DiscoveredBy,
			"stance": d.Stance, "method": strings.TrimSpace(d.Method),
		})
		stage(KindDiscovery, payload, float64(d.Confidence), start, end, d.Quote, vctx.chunk.Index, ref)
	}

	return staged, drops
}

// validEntityKind mirrors the entities CHECK constraint.
func validEntityKind(kind string) bool {
	switch kind {
	case "pc", "npc", "faction", "location", "item", "deity", "organization", "creature", "concept":
		return true
	}
	return false
}

/* ---------- the extraction run ---------- */

// ExtractInput selects what one extraction run covers. Zero optional values
// mean the campaign's every source, bounded by the batch size.
type ExtractInput struct {
	CampaignID string
	// SessionID narrows the run to one session's sources.
	SessionID string
	// SourceIDs selects sources explicitly (their campaign must match).
	SourceIDs []string
	// Limit caps the units processed this run; the remainder is deferred
	// and a later run picks them up. Zero uses the configured batch size.
	Limit int
}

// Extract runs the extraction stage over the selected sources.
//
// Per-unit semantics: each source commits in its own transaction — ledger
// row, model outputs, candidates, drops together — so an interrupted run
// resumes exactly where it stopped, and a unit already extracted under the
// same prompt version and input checksum is skipped without a model call.
// Guards stop the run between chunk calls: the USD budget (when prices are
// configured) and the candidate cap; the remainder stays deferred.
func (s *Store) Extract(ctx context.Context, in ExtractInput) (*Run, error) {
	if strings.TrimSpace(in.CampaignID) == "" {
		return nil, fmt.Errorf("%w: campaign id is required", ErrInvalid)
	}
	camp, err := s.loadCampaign(ctx, in.CampaignID)
	if err != nil {
		return nil, err
	}
	tctx, err := s.loadTaskContext(ctx, in.CampaignID)
	if err != nil {
		return nil, err
	}
	units, err := s.loadUnits(ctx, in)
	if err != nil {
		return nil, err
	}

	stats := newRunStats()
	run := &Run{
		ID: uuid.NewString(), CampaignID: in.CampaignID, SessionID: in.SessionID,
		Kind: "extract", PromptVersion: PROMPT_VERSION, Model: s.model.ModelName(),
		Status: RunRunning, Stats: stats, CreatedAt: s.now(), UpdatedAt: s.now(),
	}
	if err := s.insertRun(ctx, run); err != nil {
		return nil, err
	}

	limit := in.Limit
	if limit <= 0 {
		limit = s.cfg.BatchSize
	}

	budgetKnown := s.cfg.BudgetUSD > 0 && s.cfg.PriceInMTok > 0 && s.cfg.PriceOutMTok > 0
	seen := map[string]bool{}
	var lastCall time.Time
	var fatal error
	stopReason := ""

	remaining := limit
	for _, u := range units {
		// The input checksum covers everything the model sees for this
		// unit: prompt version, source checksum, chunk plan, entity
		// list, vocabulary and roster. Same checksum under this version
		// means done and skipped; anything else re-extracts.
		chunks := chunkSource(u.Content, ChunkTarget, ChunkOverlap)
		inputCheck := inputChecksum(u, chunks, tctx)
		done, err := s.ledgerDone(ctx, u.SourceID, PROMPT_VERSION, inputCheck)
		if err != nil {
			fatal = err
			break
		}
		if done {
			stats.UnitsSkipped++
			continue
		}
		// The limit bounds model work: a ledger skip is not work and
		// does not consume the batch, and the run only declares itself
		// stopped on units that actually needed it.
		if remaining <= 0 {
			stopReason = StopUnits
			break
		}
		stats.UnitsTotal++

		unitCtx := taskContext{
			CampaignName: camp.Name, CampaignClock: camp.Clock,
			SourceKind: u.Kind, SourceAuthor: u.Author, SourceTitle: u.Title,
			Entities: tctx.Entities, Roster: tctx.Roster, RelTypes: tctx.RelTypes,
		}

		var unitStaged []Staged
		var unitDrops []Drop
		var outputs []modelOutputRow

		for _, ch := range chunks {
			if s.cfg.MaxCandidates > 0 && stats.StagedTotal() >= s.cfg.MaxCandidates {
				stopReason = StopCandidates
				break
			}
			if budgetKnown && stats.CostUSD >= s.cfg.BudgetUSD {
				stopReason = StopBudget
				break
			}
			if err := s.waitInterval(ctx, &lastCall); err != nil {
				fatal = err
				break
			}
			compl, err := s.model.Complete(ctx, systemPrompt(), userPrompt(unitCtx, ch.Text, ch.Start, ch.End))
			if err != nil {
				fatal = fmt.Errorf("model call failed on source %s chunk %d: %w", u.SourceID, ch.Index, err)
				break
			}
			lastCall = s.now()
			stats.Requests++
			stats.InputTokens += compl.InputTokens
			stats.OutputTokens += compl.OutputTokens
			cost := s.cfg.costUSD(compl.InputTokens, compl.OutputTokens)
			stats.CostUSD += cost

			outputs = append(outputs, modelOutputRow{
				ID: uuid.NewString(), RunID: run.ID, Stage: "extract",
				PromptVersion: PROMPT_VERSION, SourceID: u.SourceID, ChunkIndex: ch.Index,
				Model: s.model.ModelName(), InputTokens: compl.InputTokens,
				OutputTokens: compl.OutputTokens, Raw: compl.Text, CreatedAt: s.now(),
			})

			wire, problems := parseWire(compl.Text)
			for _, p := range problems {
				stats.ParseProblems++
				unitDrops = append(unitDrops, Drop{
					Kind: "payload", Ref: fmt.Sprintf("chunk %d", ch.Index),
					Reason: DropUnparseable, Detail: p, ChunkIndex: ch.Index,
				})
			}
			vctx := validateContext{
				sourceContent: u.Content, chunk: ch,
				knownEntities: tctx.knownEntities, relTypes: tctx.relTypeSet(),
			}
			staged, drops := validatePayload(wire, vctx, seen)
			for _, c := range staged {
				if s.cfg.MaxCandidates > 0 && stats.StagedTotal() >= s.cfg.MaxCandidates {
					unitDrops = append(unitDrops, Drop{
						Kind: c.Kind, Ref: c.Checksum, Reason: DropMaxCandidates,
						Detail: "run reached the candidate cap", ChunkIndex: ch.Index,
					})
					continue
				}
				unitStaged = append(unitStaged, c)
				stats.Staged[c.Kind]++
			}
			unitDrops = append(unitDrops, drops...)
			for _, d := range drops {
				stats.Dropped[d.Reason]++
			}
			stats.Chunks++
		}
		if fatal != nil {
			break
		}

		// The unit's own transaction: everything it produced commits
		// together or not at all. A failed model call leaves no ledger
		// row, so a re-run retries the unit from its first chunk.
		err = s.commitUnit(ctx, run.ID, in.CampaignID, u, inputCheck, len(chunks), unitStaged, unitDrops, outputs, stats)
		if err != nil {
			fatal = err
			break
		}
		stats.UnitsDone++
		remaining--
		if stopReason != "" {
			break
		}
		// A guard that tripped mid-unit (the cap saturating during the
		// last chunk's staging) is honored here: the unit committed, and
		// the run stops rather than starting another unit of work.
		if s.cfg.MaxCandidates > 0 && stats.StagedTotal() >= s.cfg.MaxCandidates {
			stopReason = StopCandidates
			break
		}
	}

	if fatal != nil {
		run.Status = RunFailed
		run.StopReason = StopError
		run.Error = fatal.Error()
	} else if stopReason != "" {
		run.Status = RunStopped
		run.StopReason = stopReason
	} else {
		run.Status = RunCompleted
	}
	run.UpdatedAt = s.now()
	if err := s.finishRun(ctx, run); err != nil {
		if fatal == nil {
			return run, err
		}
	}
	return run, fatal
}

// waitInterval enforces the configured minimum spacing between model calls —
// one global politeness gate per run.
func (s *Store) waitInterval(ctx context.Context, lastCall *time.Time) error {
	if s.cfg.Interval <= 0 || lastCall.IsZero() {
		return nil
	}
	wait := s.cfg.Interval - s.now().Sub(*lastCall)
	if wait <= 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// inputChecksum hashes everything the model sees for one unit: prompt
// version, source checksum, the chunk plan, the entity list, the
// relationship vocabulary and the roster. Re-extract happens exactly when
// any of those change.
func inputChecksum(u unit, chunks []Chunk, tctx *campaignContext) string {
	var b strings.Builder
	b.WriteString(PROMPT_VERSION)
	b.WriteString("\x00")
	b.WriteString(u.Checksum)
	fmt.Fprintf(&b, "\x00%d/%d/%d", ChunkTarget, ChunkOverlap, len(chunks))
	for _, ch := range chunks {
		fmt.Fprintf(&b, "\x00%d,%d", ch.Start, ch.End)
	}
	for _, e := range tctx.Entities {
		fmt.Fprintf(&b, "\x00%s|%s|%s|%s", e.ID, e.Kind, e.Name, strings.Join(e.Aliases, ","))
	}
	b.WriteString("\x00")
	b.WriteString(strings.Join(tctx.RelTypes, ","))
	b.WriteString("\x00")
	for _, r := range tctx.Roster {
		fmt.Fprintf(&b, "\x00%s", r.ID)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// campaignHeader is the slice of the campaign row the prompt renders.
type campaignHeader struct {
	Name  string
	Clock int64
}

// loadCampaign fetches the campaign header the prompt renders.
func (s *Store) loadCampaign(ctx context.Context, id string) (campaignHeader, error) {
	var out campaignHeader
	err := s.db.QueryRowContext(ctx,
		`SELECT name, clock FROM campaigns WHERE id = ?`, id).Scan(&out.Name, &out.Clock)
	if err == sql.ErrNoRows {
		return out, fmt.Errorf("%w: campaign %s", ErrNotFound, id)
	}
	return out, err
}

// campaignContext is the run-level slice of the campaign the prompts and the
// checksum are built from.
type campaignContext struct {
	Entities []promptEntity
	Roster   []promptEntity
	RelTypes []string

	knownEntities map[string]string
}

func (c campaignContext) relTypeSet() map[string]bool {
	out := make(map[string]bool, len(c.RelTypes))
	for _, r := range c.RelTypes {
		out[r] = true
	}
	return out
}

// loadTaskContext reads the entity list (with aliases), the pc roster and
// the relationship vocabulary — once per run, ordered deterministically so
// the input checksum is stable for a given campaign state.
func (s *Store) loadTaskContext(ctx context.Context, campaignID string) (*campaignContext, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.kind, e.name FROM entities e
		 WHERE e.campaign_id = ? AND e.status <> 'deleted'
		 ORDER BY e.name COLLATE NOCASE, e.id`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("load entities: %w", err)
	}
	defer rows.Close()
	tctx := &campaignContext{knownEntities: map[string]string{}}
	type ent struct{ id, kind, name string }
	var ents []ent
	for rows.Next() {
		var e ent
		if err := rows.Scan(&e.id, &e.kind, &e.name); err != nil {
			return nil, err
		}
		ents = append(ents, e)
		tctx.knownEntities[e.id] = e.kind
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	aliasRows, err := s.db.QueryContext(ctx, `
		SELECT a.entity_id, a.name FROM entity_aliases a
		 JOIN entities e ON e.id = a.entity_id
		 WHERE e.campaign_id = ? AND a.kind <> 'canonical'
		 ORDER BY a.entity_id, a.name`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("load aliases: %w", err)
	}
	defer aliasRows.Close()
	aliases := map[string][]string{}
	for aliasRows.Next() {
		var id, name string
		if err := aliasRows.Scan(&id, &name); err != nil {
			return nil, err
		}
		aliases[id] = append(aliases[id], name)
	}
	if err := aliasRows.Err(); err != nil {
		return nil, err
	}

	for _, e := range ents {
		pe := promptEntity{ID: e.id, Kind: e.kind, Name: e.name, Aliases: aliases[e.id]}
		tctx.Entities = append(tctx.Entities, pe)
		if e.kind == "pc" {
			tctx.Roster = append(tctx.Roster, pe)
		}
	}
	sortEntityList(tctx.Entities)
	sortEntityList(tctx.Roster)

	relRows, err := s.db.QueryContext(ctx,
		`SELECT name FROM relationship_types ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("load relationship types: %w", err)
	}
	defer relRows.Close()
	for relRows.Next() {
		var name string
		if err := relRows.Scan(&name); err != nil {
			return nil, err
		}
		tctx.RelTypes = append(tctx.RelTypes, name)
	}
	return tctx, relRows.Err()
}

// loadUnits gathers the run's work units, in session play order.
func (s *Store) loadUnits(ctx context.Context, in ExtractInput) ([]unit, error) {
	q := `
		SELECT src.id, src.session_id, src.kind, src.author, src.title, src.content, src.checksum
		  FROM session_sources src
		  JOIN game_sessions gs ON gs.id = src.session_id
		 WHERE gs.campaign_id = ?`
	args := []any{in.CampaignID}
	if in.SessionID != "" {
		q += ` AND src.session_id = ?`
		args = append(args, in.SessionID)
	}
	if len(in.SourceIDs) > 0 {
		q += ` AND src.id IN (` + placeholders(len(in.SourceIDs)) + `)`
		for _, id := range in.SourceIDs {
			args = append(args, id)
		}
	}
	q += ` ORDER BY gs.ordinal, src.created_at, src.id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("load sources: %w", err)
	}
	defer rows.Close()
	var units []unit
	for rows.Next() {
		var u unit
		if err := rows.Scan(&u.SourceID, &u.SessionID, &u.Kind, &u.Author, &u.Title, &u.Content, &u.Checksum); err != nil {
			return nil, err
		}
		units = append(units, u)
	}
	return units, rows.Err()
}

func placeholders(n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += "?,"
	}
	return "(" + out[:len(out)-1] + ")"
}
