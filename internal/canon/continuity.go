// The pre-session continuity check (MAD-312): Arda's fact-check stage pointed
// forward instead of backward. Arda fact-checks generated prose against the
// records that were supposed to support it after the fact; Grimoire checks
// the DM's prep for the next session against current campaign state before
// the session runs, when a caught conflict still costs nothing.
//
// The load-bearing design rule, straight from the issue: most of these are
// deterministic joins against awareness, not model calls. CheckPrep is pure
// over a snapshot — no DB, no LLM — and catches the canonical failures on
// its own:
//
//   - prep_dead_on_stage: the scene puts an NPC on stage the campaign
//     records as dead, or one the party believes dead;
//   - prep_unheard_name: the scene references an entity no character has
//     ever heard of (zero granting awareness on any fact touching it);
//   - prep_item_misplaced: the prep assumes an item sits somewhere the
//     campaign contradicts — the party has it.
//
// Only the residue — free prose, motives, dates, causal chains the joins
// cannot see — goes to a model, and its output is a finding proposal with
// verbatim quotes on both sides, never a mutation. Reports are ephemeral:
// prep changes every time the DM edits it, so findings return to the caller
// instead of landing in the canon_flags ledger.

package canon

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// Check codes for the continuity pass. Ephemeral: they appear in reports,
// never in the flag ledger.
const (
	CheckPrepDeadOnStage    = "prep_dead_on_stage"
	CheckPrepUnheardName    = "prep_unheard_name"
	CheckPrepItemMisplaced  = "prep_item_misplaced"
	CheckPrepModelConflict  = "prep_model_conflict"
	recordKindPrep          = "prep"
	ContinuityPromptVersion = "canon-continuity-001"
)

// Cast states: how a scene uses an NPC. "alive" means walks, talks or fights;
// "dead" means the scene shows a body; "mentioned" means spoken of only.
const (
	PrepAlive     = "alive"
	PrepDead      = "dead"
	PrepMentioned = "mentioned"
)

// deathPredicates are the fact predicates read as "this entity is dead". The
// closed set is a heuristic, deliberately: predicates are free-form verb
// phrases, so the join trades recall for never guessing wrong. A death
// recorded some other way rides the residue to the model.
var deathPredicates = map[string]bool{
	"died": true, "dead": true, "killed": true, "was_killed": true,
	"slain": true, "was_slain": true, "is_dead": true,
}

// possessionPredicates are the fact predicates read as "this entity is
// somewhere": held_by, kept_in, located_in and their kin.
var possessionPredicates = map[string]bool{
	"held_by": true, "carried_by": true, "possessed_by": true, "bears": true,
	"holds": true, "has": true, "keeps": true, "kept_in": true,
	"stored_in": true, "stashed_in": true, "hidden_in": true, "located_in": true,
}

// Prep is the DM's plan for a session: scenes, who is on stage, and what the
// prep assumes about the world. Refs may be entity ids or names as written —
// the checker resolves both, the way a DM would read them back.
type Prep struct {
	Title  string      `json:"title"`
	Scenes []PrepScene `json:"scenes"`
}

// PrepScene is one planned scene.
type PrepScene struct {
	Name     string       `json:"name"`
	Summary  string       `json:"summary"`
	Location string       `json:"location,omitempty"` // entity id, name or alias
	Cast     []PrepCast   `json:"cast,omitempty"`
	Items    []PrepItem   `json:"items,omitempty"`
	Reveals  []PrepReveal `json:"reveals,omitempty"`
}

// PrepCast is one NPC the scene uses, and how.
type PrepCast struct {
	Ref   string `json:"ref"`
	State string `json:"state"` // alive | dead | mentioned; default alive
}

// PrepItem is an item the prep makes a placement assumption about: AssumedAt
// is where the prep puts it (an entity ref or free text; unresolved text is
// the model's residue, not the join's).
type PrepItem struct {
	Ref       string `json:"ref"`
	AssumedAt string `json:"assumed_at,omitempty"`
}

// PrepReveal is a planned reveal: a fact the scene means to surface. Ref may
// be a fact id or the statement as written.
type PrepReveal struct {
	Fact string `json:"fact"`
}

// NameResolution records how one prep reference resolved against the graph.
type NameResolution struct {
	Ref      string `json:"ref"`
	EntityID string `json:"entity_id,omitempty"`
	Name     string `json:"name,omitempty"`
	Kind     string `json:"kind,omitempty"`
	Resolved bool   `json:"resolved"`
}

// ContinuityReport is the whole pre-session check: the deterministic
// findings (which carry the result), the model's residue findings when a
// model is configured, the name-resolution table so the DM can see what the
// checker read, and the token accounting the budget guards ride on.
type ContinuityReport struct {
	CampaignID    string             `json:"campaign_id"`
	Offline       bool               `json:"offline"`
	PromptVersion string             `json:"prompt_version"`
	Findings      []campaign.Finding `json:"findings"`
	ModelFindings []campaign.Finding `json:"model_findings,omitempty"`
	Names         []NameResolution   `json:"names,omitempty"`
	Problems      []string           `json:"problems,omitempty"`
	InputTokens   int                `json:"input_tokens"`
	OutputTokens  int                `json:"output_tokens"`
	CostUSD       float64            `json:"cost_usd"`
}

/* ---------- deterministic pass ---------- */

// prepIndex is the resolution and awareness machinery the checks join on.
type prepIndex struct {
	snap     *Snapshot
	bySquash map[string]campaign.Entity // squashed canonical name or alias -> entity
	isPC     map[string]bool
	// partyHeard: entity id -> one live fact statement about that entity
	// the party or a pc holds a granting stance on.
	partyHeard map[string]string
}

func buildPrepIndex(snap *Snapshot) *prepIndex {
	idx := &prepIndex{
		snap: snap, bySquash: map[string]campaign.Entity{}, isPC: map[string]bool{},
		partyHeard: map[string]string{},
	}
	for _, e := range snap.Entities {
		if e.Status == campaign.StatusDeleted {
			continue
		}
		idx.bySquash[squashName(e.Name)] = e
		if e.Kind == campaign.KindPC {
			idx.isPC[e.ID] = true
		}
	}
	for _, n := range snap.Names {
		if e, ok := entityByID(snap, n.EntityID); ok && e.Status != campaign.StatusDeleted {
			idx.bySquash[squashName(n.Name)] = e
		}
	}
	// What the party has heard: a granting awareness row for the party or a
	// pc on a live fact whose subject or object is the entity. The same join
	// internal/knowledge enforces in SQL for scoped retrieval.
	for _, a := range snap.Awareness {
		if !grantingStance(a.Stance) {
			continue
		}
		if a.Knower != campaign.PartyKnower && !idx.isPC[a.Knower] {
			continue
		}
		f, ok := factByID(snap, a.FactID)
		if !ok || f.SupersededBy != "" {
			continue
		}
		for _, ent := range []string{f.SubjectEntity, f.ObjectEntity} {
			if ent == "" {
				continue
			}
			if _, exists := entityByID(snap, ent); exists {
				if _, seen := idx.partyHeard[ent]; !seen {
					idx.partyHeard[ent] = f.Statement
				}
			}
		}
	}
	return idx
}

// resolve maps a prep ref to an entity: exact id first, then squashed name or
// alias — the same normalizer the entity resolver and bestiary mirror use.
func (idx *prepIndex) resolve(ref string) (campaign.Entity, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return campaign.Entity{}, false
	}
	if e, ok := entityByID(idx.snap, ref); ok && e.Status != campaign.StatusDeleted {
		return e, true
	}
	if e, ok := idx.bySquash[squashName(ref)]; ok {
		return e, true
	}
	return campaign.Entity{}, false
}

func entityByID(snap *Snapshot, id string) (campaign.Entity, bool) {
	for _, e := range snap.Entities {
		if e.ID == id {
			return e, true
		}
	}
	return campaign.Entity{}, false
}

func factByID(snap *Snapshot, id string) (campaign.Fact, bool) {
	for _, f := range snap.Facts {
		if f.ID == id {
			return f, true
		}
	}
	return campaign.Fact{}, false
}

// CheckPrep runs the deterministic continuity rules over a snapshot and a
// prep document. Pure: no DB, no clock, no network — the offline guarantee
// the CLI's --offline flag and the offline store both rest on. The returned
// findings are sorted by check then scene for stable output.
func CheckPrep(snap *Snapshot, prep *Prep) ([]campaign.Finding, []NameResolution) {
	idx := buildPrepIndex(snap)
	var out []campaign.Finding
	var names []NameResolution
	record := func(scene string, i int) string {
		return fmt.Sprintf("%s/%d", scene, i)
	}
	for _, sc := range prep.Scenes {
		scene := sc.Name
		if scene == "" {
			scene = fmt.Sprintf("scene-%d", len(names)+1)
		}
		for i, cast := range sc.Cast {
			res := NameResolution{Ref: cast.Ref}
			e, ok := idx.resolve(cast.Ref)
			if ok {
				res = NameResolution{Ref: cast.Ref, EntityID: e.ID, Name: e.Name, Kind: e.Kind, Resolved: true}
			}
			names = append(names, res)
			if !ok {
				continue // a name from nowhere is new content, not a conflict
			}
			if cast.State == "" {
				cast.State = PrepAlive
			}
			if cast.State != PrepAlive {
				continue // a dead or mentioned cast member conflicts with nothing
			}
			if e.Status == campaign.StatusDead || e.Status == campaign.StatusDestroyed {
				out = append(out, campaign.Finding{
					Check: CheckPrepDeadOnStage, Severity: campaign.SeverityError,
					RecordKind: recordKindPrep, RecordID: record(scene, i),
					Message: fmt.Sprintf("scene %q puts %s on stage, but the campaign records them as %s",
						scene, e.Name, e.Status),
				})
				continue
			}
			// The party believes them dead: canon may say alive (the twist
			// case), so this is review, not error — the DM decides whether
			// the reveal is intended and a discovery is planned.
			if stmt := partyBelievesDead(snap, idx, e.ID); stmt != "" {
				out = append(out, campaign.Finding{
					Check: CheckPrepDeadOnStage, Severity: campaign.SeverityReview,
					RecordKind: recordKindPrep, RecordID: record(scene, i),
					Message: fmt.Sprintf("the party believes %s dead (%q), but scene %q has them walk in alive — deliberate twist, or a slip to fix?",
						e.Name, stmt, scene),
				})
			}
		}
		// The unheard-name join covers every structured reference: cast,
		// location, items and reveals. It only fires when the campaign has
		// table history — before the first session everything is unheard.
		if len(snap.Sessions) > 0 {
			refs := []string{sc.Location}
			for _, c := range sc.Cast {
				refs = append(refs, c.Ref)
			}
			for _, it := range sc.Items {
				refs = append(refs, it.Ref)
			}
			for _, rv := range sc.Reveals {
				refs = append(refs, rv.Fact)
			}
			seenInScene := map[string]bool{}
			for _, ref := range refs {
				if ref == "" || seenInScene[ref] {
					continue
				}
				seenInScene[ref] = true
				e, ok := idx.resolve(ref)
				if !ok || idx.isPC[e.ID] {
					continue
				}
				if _, heard := idx.partyHeard[e.ID]; heard {
					continue
				}
				// A reveal reference may name a fact id, not an entity.
				if _, isFact := factByID(snap, ref); isFact {
					continue
				}
				out = append(out, campaign.Finding{
					Check: CheckPrepUnheardName, Severity: campaign.SeverityReview,
					RecordKind: recordKindPrep, RecordID: scene + "/" + squashName(e.Name),
					Message: fmt.Sprintf("scene %q references %q, a name no character has heard — no fact about them has ever reached the party",
						scene, e.Name),
				})
			}
		}
		for i, item := range sc.Items {
			e, ok := idx.resolve(item.Ref)
			if !ok || item.AssumedAt == "" {
				continue
			}
			at, resolved := idx.resolve(item.AssumedAt)
			if !resolved {
				continue // free text like "the vault" is the model's residue
			}
			holder, stmt := partyKnownHolder(snap, idx, e.ID)
			if holder == "" || holder == at.ID {
				continue
			}
			out = append(out, campaign.Finding{
				Check: CheckPrepItemMisplaced, Severity: campaign.SeverityError,
				RecordKind: recordKindPrep, RecordID: record(scene, i),
				Message: fmt.Sprintf("the prep assumes %s is at %s, but the party holds it: %q",
					e.Name, at.Name, stmt),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Check != out[j].Check {
			return out[i].Check < out[j].Check
		}
		return out[i].RecordID < out[j].RecordID
	})
	return out, names
}

// partyBelievesDead returns the statement of a live death fact about the
// entity that the party or a pc holds a granting stance on, or "".
func partyBelievesDead(snap *Snapshot, idx *prepIndex, entityID string) string {
	for _, f := range snap.Facts {
		if f.SubjectEntity != entityID || !deathPredicates[normalizePredicate(f.Predicate)] {
			continue
		}
		if f.SupersededBy != "" || f.Confidence == campaign.ConfidenceProposed {
			continue
		}
		for _, a := range snap.Awareness {
			if a.FactID != f.ID || !grantingStance(a.Stance) {
				continue
			}
			if a.Knower == campaign.PartyKnower || idx.isPC[a.Knower] {
				return f.Statement
			}
		}
	}
	return ""
}

// partyKnownHolder returns the entity currently holding an item per a live
// possession fact the party knows, plus the fact's statement. Empty holder
// means no party-known placement exists.
func partyKnownHolder(snap *Snapshot, idx *prepIndex, itemID string) (string, string) {
	for _, f := range snap.Facts {
		if f.SubjectEntity != itemID || !possessionPredicates[normalizePredicate(f.Predicate)] {
			continue
		}
		if f.SupersededBy != "" || f.Confidence == campaign.ConfidenceProposed || f.ObjectEntity == "" {
			continue
		}
		for _, a := range snap.Awareness {
			if a.FactID != f.ID || !grantingStance(a.Stance) {
				continue
			}
			if a.Knower == campaign.PartyKnower || idx.isPC[a.Knower] {
				return f.ObjectEntity, f.Statement
			}
		}
	}
	return "", ""
}

// normalizePredicate lowercases and trims a predicate so the closed
// vocabularies match what free-form authoring actually produces.
func normalizePredicate(p string) string {
	return strings.ToLower(strings.TrimSpace(p))
}

/* ---------- the store entry point ---------- */

// CheckContinuity runs the pre-session check over one campaign: the
// deterministic pass always, the model residue pass only when a model client
// is wired. An offline store returns the deterministic report with
// Offline=true and never touches a model.
func (s *Store) CheckContinuity(ctx context.Context, campaignID string, prep *Prep) (*ContinuityReport, error) {
	if prep == nil {
		prep = &Prep{}
	}
	snap, err := LoadSnapshot(ctx, s.db, campaignID)
	if err != nil {
		return nil, err
	}
	findings, names := CheckPrep(snap, prep)
	rep := &ContinuityReport{
		CampaignID: campaignID, Offline: true,
		PromptVersion: ContinuityPromptVersion,
		Findings:      findings, Names: names,
	}
	if s.model == nil {
		return rep, nil
	}
	modelFindings, inTok, outTok, cost, problems, err := s.prepResiduePass(ctx, snap, prep)
	if err != nil {
		return nil, err
	}
	rep.Offline = false
	rep.ModelFindings = modelFindings
	rep.InputTokens, rep.OutputTokens = inTok, outTok
	rep.CostUSD = cost
	rep.Problems = problems
	return rep, nil
}

// prepResiduePass sends what the joins cannot judge — scene prose against
// the party's knowledge and the canon around the referenced entities — to
// the model in one exchange. Every conflict it returns must quote the prep
// verbatim and the records verbatim; anything else is dropped and logged.
// The model proposes findings; it never mutates anything.
func (s *Store) prepResiduePass(ctx context.Context, snap *Snapshot, prep *Prep) ([]campaign.Finding, int, int, float64, []string, error) {
	if len(prep.Scenes) == 0 {
		return nil, 0, 0, 0, nil, nil
	}
	var lastCall time.Time
	if err := s.waitInterval(ctx, &lastCall); err != nil {
		return nil, 0, 0, 0, nil, err
	}
	prepText, recordsText := renderPrepContext(snap, prep)
	user := continuityUserPrompt(prep.Title, prepText, recordsText)
	completion, err := s.model.Complete(ctx, continuitySystemPrompt(), user)
	if err != nil {
		return nil, 0, 0, 0, nil, fmt.Errorf("continuity model pass: %w", err)
	}
	cost := s.cfg.costUSD(completion.InputTokens, completion.OutputTokens)
	if s.cfg.BudgetUSD > 0 && cost > s.cfg.BudgetUSD {
		return nil, completion.InputTokens, completion.OutputTokens, cost,
			[]string{fmt.Sprintf("continuity pass cost %.4f USD exceeds the run budget %.2f USD; residue unchecked", cost, s.cfg.BudgetUSD)},
			nil
	}
	findings, problems := parsePrepConflicts(completion.Text, prepText, recordsText)
	return findings, completion.InputTokens, completion.OutputTokens, cost, problems, nil
}

// renderPrepContext assembles the two texts the residue pass quotes against:
// the prep as the model sees it, and the records (party knowledge plus canon
// around referenced entities) it judges by.
func renderPrepContext(snap *Snapshot, prep *Prep) (prepText, recordsText string) {
	idx := buildPrepIndex(snap)
	var pb, rb strings.Builder
	fmt.Fprintf(&pb, "PREP: %s\n", prep.Title)
	for i, sc := range prep.Scenes {
		fmt.Fprintf(&pb, "\nScene %d: %s\n", i+1, sc.Name)
		if sc.Summary != "" {
			fmt.Fprintf(&pb, "%s\n", sc.Summary)
		}
		if sc.Location != "" {
			fmt.Fprintf(&pb, "Location: %s\n", resolveLabel(idx, sc.Location))
		}
		for _, c := range sc.Cast {
			state := c.State
			if state == "" {
				state = PrepAlive
			}
			fmt.Fprintf(&pb, "Cast: %s (%s)\n", resolveLabel(idx, c.Ref), state)
		}
		for _, it := range sc.Items {
			if it.AssumedAt != "" {
				fmt.Fprintf(&pb, "Item: %s assumed at %s\n", resolveLabel(idx, it.Ref), resolveLabel(idx, it.AssumedAt))
			}
		}
		for _, rv := range sc.Reveals {
			fmt.Fprintf(&pb, "Reveal: %s\n", rv.Fact)
		}
	}

	fmt.Fprintf(&rb, "WHAT THE PARTY KNOWS (live facts the party or a character holds):\n")
	count := 0
	facts := append([]campaign.Fact(nil), snap.Facts...)
	sort.Slice(facts, func(i, j int) bool { return facts[i].CreatedAt.After(facts[j].CreatedAt) })
	for _, f := range facts {
		if f.SupersededBy != "" || f.Confidence == campaign.ConfidenceProposed {
			continue
		}
		held := false
		for _, a := range snap.Awareness {
			if a.FactID == f.ID && grantingStance(a.Stance) &&
				(a.Knower == campaign.PartyKnower || idx.isPC[a.Knower]) {
				held = true
				break
			}
		}
		if !held {
			continue
		}
		fmt.Fprintf(&rb, "- %s\n", f.Statement)
		count++
		if count >= 80 {
			break
		}
	}
	if count == 0 {
		fmt.Fprintf(&rb, "(nothing recorded yet)\n")
	}
	return pb.String(), rb.String()
}

// resolveLabel renders a prep ref for the prompt, preferring the entity's
// canonical name so the model reads what the graph reads.
func resolveLabel(idx *prepIndex, ref string) string {
	if e, ok := idx.resolve(ref); ok {
		return e.Name
	}
	return ref
}

// parsePrepConflicts decodes the model's conflict list, enforcing the quote
// discipline on both sides: a quote that is not verbatim in the prep text,
// or evidence that is not verbatim in the records text, drops the item with
// a problem logged — exactly the extraction span rule, pointed at prose.
func parsePrepConflicts(text, prepText, recordsText string) ([]campaign.Finding, []string) {
	var problems []string
	block, err := jsonBlock(text)
	if err != nil {
		return nil, []string{err.Error()}
	}
	var wire struct {
		Conflicts []struct {
			Scene    string `json:"scene"`
			Quote    string `json:"quote"`
			Conflict string `json:"conflict"`
			Evidence string `json:"evidence"`
		} `json:"conflicts"`
	}
	if err := json.Unmarshal([]byte(block), &wire); err != nil {
		return nil, []string{fmt.Sprintf("invalid JSON: %v", err)}
	}
	var out []campaign.Finding
	for i, c := range wire.Conflicts {
		if strings.TrimSpace(c.Quote) == "" || strings.TrimSpace(c.Conflict) == "" {
			problems = append(problems, fmt.Sprintf("conflicts[%d]: missing quote or conflict", i))
			continue
		}
		if !strings.Contains(prepText, c.Quote) {
			problems = append(problems, fmt.Sprintf("conflicts[%d]: quote not verbatim in the prep", i))
			continue
		}
		if strings.TrimSpace(c.Evidence) != "" && !strings.Contains(recordsText, c.Evidence) {
			problems = append(problems, fmt.Sprintf("conflicts[%d]: evidence %q not verbatim in the records", i, c.Evidence))
			continue
		}
		scene := strings.TrimSpace(c.Scene)
		if scene == "" {
			scene = "(unnamed scene)"
		}
		msg := fmt.Sprintf("%s (model pass, quoted from the prep: %q)", c.Conflict, c.Quote)
		if strings.TrimSpace(c.Evidence) != "" {
			msg = fmt.Sprintf("%s — records say: %q (model pass, quoted from the prep: %q)", c.Conflict, c.Evidence, c.Quote)
		}
		out = append(out, campaign.Finding{
			Check: CheckPrepModelConflict, Severity: campaign.SeverityReview,
			RecordKind: recordKindPrep, RecordID: scene,
			Message: msg,
		})
	}
	return out, problems
}

// continuitySystemPrompt is the standing instruction for the residue pass.
// The prime directive mirrors Arda's checker: outside knowledge may deepen
// suspicion, never confirmation; every conflict must quote both sides.
func continuitySystemPrompt() string {
	return `You are the continuity editor for a D&D campaign. The DM has written prep for the next session. Deterministic joins have already checked the mechanical conflicts (dead NPCs on stage, unheard names, misplaced items). Your job is the RESIDUE those joins cannot see: dates, motives, causal chains, beliefs and timeline contradictions between the planned scenes and the recorded campaign state.

PRIME DIRECTIVE — QUOTE OR STAY SILENT. You judge ONLY by the records provided. Every conflict you report MUST carry (1) a "quote" copied VERBATIM from the prep text and (2) "evidence" copied VERBATIM from the records. A conflict you cannot quote on both sides is omitted, never guessed. Do not use your knowledge of tabletop tropes or D&D lore to rescue a conflict; outside knowledge may deepen suspicion, never confirmation.

Connective tissue is legitimate: atmosphere, pacing, new scene dressing, new minor characters the prep itself introduces — none of those are conflicts. A conflict is a planned scene contradicting what the records say is true, or what the records say the party believes.

OUTPUT FORMAT:
Return ONE JSON object and nothing else — no prose, no markdown fences:
{"conflicts": [{"scene": "scene name", "quote": "verbatim words from the prep", "conflict": "one or two sentences on what contradicts what", "evidence": "verbatim words from the records"}]}
Empty list is fine. Emit nothing rather than something you cannot quote.`
}

// continuityUserPrompt renders the residue task: the prep text and the
// records text, with the version stamped so ledger semantics stay possible.
func continuityUserPrompt(title, prepText, recordsText string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "TASK: Check this session prep for continuity conflicts with the recorded campaign state.\n\n")
	if strings.TrimSpace(title) != "" {
		fmt.Fprintf(&b, "Prep title: %s\n\n", title)
	}
	b.WriteString(prepText)
	b.WriteString("\n\n")
	b.WriteString(recordsText)
	b.WriteString("\n\nReport every conflict the prep has with the records, following every rule in the system message. Return only the JSON object.")
	return b.String()
}
