package server

// The session copilot (MAD-486, stage 5 of MAD-318): the big Ask Grimoire
// box on the DM screen. "They ask the innkeeper about the murders" — the
// copilot answers with the innkeeper's voice, from the innkeeper's actual
// knowledge state, aware of which clues the table has already discovered,
// and surfaces anything the answer invents as a suggested clue to release:
// Accept as canon | Modify | Discard.
//
// The grounding is the braid the issue asks for: the live table (current
// scene with its cast, the active battle if one runs, recent session
// events, the party's vitals) beside the npcask machinery when the
// question names an NPC on stage — the mind at the DM scope, the record
// scope-filtered at npc:<id> in SQL. The one rule is inherited word for
// word: perspective is authorization, not instruction. What the NPC's
// record does not contain, the NPC does not know, and the model cannot
// leak it because it is never retrieved. The hidden-clue list is the DM's
// material, and the system prompt says so: an NPC never speaks a clue
// their record does not carry — a hidden clue reaches the table only as a
// suggested release, through the review queue, never spoken aloud.
//
// Writes happen on exactly one path: the DM's explicit accept. A release
// logs the discovery event against the live session (the Stage 4 capture
// loop — the event is the immutable anchor) and stages the canon item in
// the same review queue every other machine proposal uses: an npc_reveal
// when the answer had a voice, a session_capture proposed_fact otherwise.
// Nothing here writes a fact, an awareness row, or anything a player can
// see; the queue's decision is the only gate, exactly as for the director
// and the builder.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/board"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/combat"
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/story"
)

const (
	// copilotEventLimit is how many recent session events ride the table
	// context — enough beats to know where the night stands.
	copilotEventLimit = 10
	// copilotClueLimit caps each clue list. Discovered clues are the
	// table's own record; hidden clues are the scene's. Both are bounded
	// so a long campaign stays out of the token budget.
	copilotClueLimit = 12
	// copilotPartyLimit caps the party vitals lines.
	copilotPartyLimit = 8
	// copilotTimeout bounds one ask end to end.
	copilotTimeout = 2 * time.Minute
)

/* ---------- the grounding ---------- */

// copilotGrounding is one question's whole context: the live table, the
// clue-discovery state, and — when the question names an NPC on stage —
// that NPC's mind and scope-filtered record. Everything is optional except
// the question itself: a table with no session, no scene and no fight
// still gets an honest answer from whatever is live.
type copilotGrounding struct {
	session *gamesession.Session
	scene   *story.Scene
	combat  *combat.Combat
	order   []combat.Combatant
	party   []board.Member
	events  []gamesession.Event
	// clues the table already holds: public facts plus accepted party
	// discoveries, deduped by fact.
	discovered []campaign.Fact
	// clues in play in the current scene and still hidden.
	hidden []campaign.Fact
	// hiddenDisposition spells each hidden clue's scene disposition.
	hiddenDisposition map[string]string
	// castNames spells the current scene's cast — the prompt's "on
	// stage" line reads names, not ids.
	castNames map[string]string
	// npc is non-nil when the question named an NPC on stage; the npcask
	// grounding (mind + record at npc:<id>) then braids in.
	npc *npcGrounding
}

// groundCopilot assembles one question's context. Every read runs at the
// DM scope on this DM-only surface, except the NPC record, which runs at
// npc:<id> — the scope filter is the whole point.
func (s *Server) groundCopilot(ctx context.Context, a *campAccess, question, sceneID string) (*copilotGrounding, error) {
	g := &copilotGrounding{}
	cid := a.campaign.ID

	// The live session: the clock the events and the release anchor to.
	if s.sessions != nil {
		list, err := s.sessions.ListSessions(ctx, cid)
		if err != nil {
			return nil, err
		}
		for i := range list {
			if list[i].Status == gamesession.StatusLive {
				ses := list[i]
				g.session = &ses
				break
			}
		}
	}

	// The current scene: the DM's explicit pick, the scene seated in the
	// live session, or the first active one — the same order the screen
	// card uses. GetScene attaches the cast and the secrets in play.
	if s.stories != nil {
		active, err := s.stories.ActiveScenes(ctx, campaign.ScopeDM, cid)
		if err != nil {
			return nil, err
		}
		pick := pickScene(active, g.session, sceneID)
		if pick != "" {
			sc, err := s.stories.GetScene(ctx, campaign.ScopeDM, cid, pick)
			if err != nil {
				return nil, err
			}
			g.scene = sc
		}
	}

	// The fight, if one runs.
	if s.combats != nil {
		fight, order, err := s.combats.Active(ctx, cid)
		if err != nil {
			return nil, err
		}
		if fight != nil {
			g.combat, g.order = fight, order
		}
	}

	// The party's vitals — the board's own read at the DM standing.
	if s.board != nil {
		if snap, err := s.board.Snapshot(ctx, cid, board.DMStanding()); err == nil {
			for _, m := range snap.Members {
				if m.CharacterID == "" {
					continue
				}
				g.party = append(g.party, m)
				if len(g.party) >= copilotPartyLimit {
					break
				}
			}
		}
	}

	// Recent events at the table, oldest first.
	if s.sessions != nil && g.session != nil {
		events, err := s.sessions.ListEvents(ctx, g.session.ID)
		if err != nil {
			return nil, err
		}
		if len(events) > copilotEventLimit {
			events = events[len(events)-copilotEventLimit:]
		}
		g.events = events
	}

	// Clue discovery, the queryable kind: public facts are the table's
	// shared knowledge, and an accepted discovery for the party is the
	// audit trail of a clue earned in play. Both spell "already found".
	seen := map[string]bool{}
	g.discovered = []campaign.Fact{}
	all, err := s.knowledge.Facts(ctx, campaign.ScopeDM, cid, knowledge.FactFilter{})
	if err != nil {
		return nil, err
	}
	for _, f := range all {
		if len(g.discovered) >= copilotClueLimit {
			break
		}
		if f.Visibility != campaign.VisibilityPublic {
			continue
		}
		seen[f.ID] = true
		g.discovered = append(g.discovered, f)
	}
	if len(g.discovered) < copilotClueLimit {
		if discs, err := s.knowledge.Discoveries(ctx, campaign.ScopeDM, cid, ""); err == nil {
			for _, d := range discs {
				if len(g.discovered) >= copilotClueLimit {
					break
				}
				if d.AcceptedBy == "" || d.DiscoveredBy != campaign.PartyKnower || seen[d.FactID] {
					continue
				}
				f, err := s.knowledge.Fact(ctx, campaign.ScopeDM, cid, d.FactID)
				if err != nil {
					continue
				}
				seen[d.FactID] = true
				g.discovered = append(g.discovered, *f)
			}
		}
	}

	// The hidden clues: the scene's secrets in play, spelled at the DM
	// scope for the DM's eyes. This is the release menu, never the NPC's
	// knowledge.
	g.hidden = []campaign.Fact{}
	g.hiddenDisposition = map[string]string{}
	if g.scene != nil {
		for _, sec := range g.scene.Secrets {
			if len(g.hidden) >= copilotClueLimit {
				break
			}
			if seen[sec.FactID] {
				continue // already discovered; it belongs to the other list
			}
			f, err := s.knowledge.Fact(ctx, campaign.ScopeDM, cid, sec.FactID)
			if err != nil {
				continue
			}
			g.hiddenDisposition[f.ID] = sec.Disposition
			g.hidden = append(g.hidden, *f)
		}
		// The cast spelled: the "on stage" line reads names.
		g.castNames = map[string]string{}
		for _, c := range g.scene.Cast {
			if e, err := s.campaigns.GetEntity(ctx, campaign.ScopeDM, cid, c.EntityID); err == nil {
				g.castNames[c.EntityID] = e.Name
			}
		}
	}

	// The braid: when the question names an NPC on stage — the scene's
	// cast, then the fight's entities — their mind and scoped record ride
	// along and the answer finds a voice.
	if npc := s.matchStageNPC(ctx, a, g, question); npc != nil {
		gr, err := s.groundNPC(ctx, a, npc, question)
		if err != nil {
			return nil, err
		}
		g.npc = gr
	}
	return g, nil
}

// pickScene is the screen card's order: the DM's pick, the scene seated in
// the live session, the first active scene.
func pickScene(active []story.Scene, session *gamesession.Session, sceneID string) string {
	if sceneID != "" {
		for _, sc := range active {
			if sc.ID == sceneID {
				return sc.ID
			}
		}
	}
	if session != nil {
		for _, sc := range active {
			if sc.SessionID == session.ID {
				return sc.ID
			}
		}
	}
	if len(active) > 0 {
		return active[0].ID
	}
	return ""
}

// matchStageNPC finds the NPC on stage the question names: the cast of the
// active scenes in role order (focus first), then the fight's entities. A
// name matches when one of its words (4+ letters, case-folded) appears as
// a word in the question — "the duke", "ask Aldric". First match wins;
// the cast order keeps it the DM's obvious choice.
func (s *Server) matchStageNPC(ctx context.Context, a *campAccess, g *copilotGrounding, question string) *campaign.Entity {
	if s.stories == nil || len(question) == 0 {
		return nil
	}
	qWords := wordSet(question)
	check := func(id string) *campaign.Entity {
		if id == "" {
			return nil
		}
		e, err := s.campaigns.GetEntity(ctx, campaign.ScopeDM, a.campaign.ID, id)
		if err != nil || e.Kind != campaign.KindNPC || e.Status == campaign.StatusDeleted {
			return nil
		}
		for w := range wordSet(e.Name) {
			if qWords[w] {
				return e
			}
		}
		return nil
	}
	// The active scenes' cast, current scene first, roles in board order.
	scenes := []story.Scene{}
	if g.scene != nil {
		scenes = append(scenes, *g.scene)
	}
	if active, err := s.stories.ActiveScenes(ctx, campaign.ScopeDM, a.campaign.ID); err == nil {
		for _, sc := range active {
			if g.scene == nil || sc.ID != g.scene.ID {
				scenes = append(scenes, sc)
			}
		}
	}
	roleRank := func(role string) int {
		switch role {
		case "focus":
			return 0
		case "present":
			return 1
		case "offstage":
			return 2
		case "mentioned":
			return 3
		}
		return 4
	}
	for _, sc := range scenes {
		cast := append([]story.CastMember(nil), sc.Cast...)
		for i := 1; i < len(cast); i++ {
			for j := i; j > 0 && roleRank(cast[j].Role) < roleRank(cast[j-1].Role); j-- {
				cast[j], cast[j-1] = cast[j-1], cast[j]
			}
		}
		for _, c := range cast {
			if e := check(c.EntityID); e != nil {
				return e
			}
		}
	}
	for _, c := range g.order {
		if e := check(c.EntityID); e != nil {
			return e
		}
	}
	return nil
}

// wordSet folds text into the set of its 4+-letter words.
func wordSet(text string) map[string]bool {
	out := map[string]bool{}
	var b strings.Builder
	flush := func() {
		if b.Len() >= 4 {
			out[strings.ToLower(b.String())] = true
		}
		b.Reset()
	}
	for _, r := range text {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '\'' {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

/* ---------- the prompt ---------- */

// copilotSystemPrompt is the standing instruction. Behaviour, never
// secrecy: the hidden clues ride the DM-only prompt as release material,
// and the rule that keeps them out of an unknowing NPC's mouth is stated
// here because the scope filter alone cannot express "the DM may see it,
// the character may not say it".
func copilotSystemPrompt(npcName string) string {
	var b strings.Builder
	b.WriteString(`You are the session copilot for this campaign's Dungeon Master, answering live at the table. Speed and usability beat completeness: the DM's attention is on the players, not you.`)
	if npcName != "" {
		fmt.Fprintf(&b, ` The question is put to %s, an NPC currently on stage, and you answer as them.`, npcName)
	}

	b.WriteString("\n\nGROUNDING RULES — follow these strictly:")
	if npcName != "" {
		fmt.Fprintf(&b, `
1. Answer in exactly two parts:
   REACTION — two to four sentences, third person, DM-facing: what %s would do and why, tied to the goals and fears you cite.
   IN-VOICE — what %s would actually say, written as speakable dialogue in their voice.
2. %s reacts ONLY from their own persona and record (the sections below marked as theirs). That record is scope-filtered: what it does not contain, %s does not know. They may reason as a person generally would, but may NOT assert campaign facts their record does not carry.`, npcName, npcName, npcName, npcName)
	} else {
		b.WriteString(`
1. Answer the DM directly: concrete, brief, usable at the table — what the live context and the clues support, and nothing more.`)
	}
	b.WriteString(`
3. The clues listed as STILL HIDDEN are the DM's release material, never the characters' knowledge. Unless an NPC's own record carries one, they must not speak, hint at, or act on a hidden clue. If the question would be answered by a hidden clue, have the character deflect in their voice and put the clue in the reveals block instead.
4. Never re-release a clue the table ALREADY HOLDS as if it were new.
5. Cite record facts inline as you go: [F#] for facts, [G#] for goals, [D#] for clues the table holds, [H#] for hidden clues you are proposing to release.
6. If the answer needs the world to contain something no section carries — a new name, deed, motive or place — that is an invention, not a fact. Never state it as true in the answer; list it under "reveals" in the closing JSON block, phrased as a clue the DM could release.`)
	if npcName == "" {
		b.WriteString("\n7. A fact marked (secret) in a record is DM material: report it to the DM plainly.")
	}
	b.WriteString(`
End the reply with a fenced json block of exactly this shape:
` + "```json\n{\"reveals\": [{\"statement\": \"...\", \"rationale\": \"...\"}]}\n```" + `
An answer that invented nothing ends with an empty reveals list.`)
	return b.String()
}

// copilotUserMessage assembles the final user turn: the live table, the
// clue-discovery state, the voice on stage when there is one, and the
// question. Pure, so the leak assertions can read exactly what the model
// received.
func copilotUserMessage(g *copilotGrounding, question string) string {
	var b strings.Builder

	b.WriteString("=== THE TABLE NOW ===\n")
	if g.session != nil {
		fmt.Fprintf(&b, "Live session: %s (session %d)\n", g.session.Name, g.session.Ordinal)
	} else {
		b.WriteString("Live session: none — the table is between sessions.\n")
	}
	if g.scene != nil {
		fmt.Fprintf(&b, "Scene: %s (%s)", g.scene.Name, g.scene.Kind)
		if g.scene.Purpose != "" {
			fmt.Fprintf(&b, " — %s", g.scene.Purpose)
		}
		b.WriteString("\n")
		if len(g.scene.Cast) > 0 {
			b.WriteString("On stage: ")
			for i, c := range g.scene.Cast {
				if i > 0 {
					b.WriteString(", ")
				}
				fmt.Fprintf(&b, "%s (%s)", sceneEntityName(g, c.EntityID), c.Role)
			}
			b.WriteString("\n")
		}
	} else {
		b.WriteString("Scene: none active.\n")
	}
	if g.combat != nil {
		fmt.Fprintf(&b, "Combat: %s, round %d", g.combat.Name, g.combat.Round)
		for i := range g.order {
			if g.order[i].Position == g.combat.TurnIndex && g.combat.TurnIndex >= 0 {
				fmt.Fprintf(&b, " — acting: %s", g.order[i].Name)
				break
			}
		}
		b.WriteString("\n")
		for i := range g.order {
			c := g.order[i]
			fmt.Fprintf(&b, "  - %s: %s%s\n", c.Name, combatantState(c), combatConditionLine(c))
		}
	} else {
		b.WriteString("Combat: none.\n")
	}
	if len(g.party) > 0 {
		b.WriteString("Party:\n")
		for _, m := range g.party {
			b.WriteString("  - " + partyLine(m) + "\n")
		}
	}
	if len(g.events) > 0 {
		b.WriteString("Recent events at the table, oldest first:\n")
		for _, ev := range g.events {
			fmt.Fprintf(&b, "  - (%s) %s\n", ev.Kind, ev.Summary)
		}
	}

	b.WriteString("\n=== CLUES THE TABLE ALREADY HOLDS — discovered, never re-release ===\n")
	if len(g.discovered) == 0 {
		b.WriteString("(nothing discovered yet)\n")
	}
	for i := range g.discovered {
		fmt.Fprintf(&b, "  [D%d] %s\n", i+1, g.discovered[i].Statement)
	}

	b.WriteString("\n=== CLUES IN PLAY, STILL HIDDEN — the DM's release material ===\n")
	if len(g.hidden) == 0 {
		b.WriteString("(no hidden clues in play in this scene)\n")
	}
	for i := range g.hidden {
		fmt.Fprintf(&b, "  [H%d] %s\n", i+1, g.hidden[i].Statement)
	}

	if g.npc != nil {
		b.WriteString("\n=== THE VOICE ON STAGE ===\n")
		b.WriteString(npcMindSection(g.npc))
		b.WriteString(npcRecordSection(g.npc))
	}

	fmt.Fprintf(&b, "\nQuestion: %s\n", question)
	return b.String()
}

// sceneEntityName spells a cast member from the grounding's own name
// table; an entity the table never loaded falls back to the short id,
// the same fallback the screen chips use.
func sceneEntityName(g *copilotGrounding, id string) string {
	if g.castNames != nil {
		if name, ok := g.castNames[id]; ok && name != "" {
			return name
		}
	}
	if g.npc != nil && g.npc.npc.ID == id {
		return g.npc.npc.Name
	}
	return id
}

// combatantState is one fighter's vitals, spelled for prose.
func combatantState(c combat.Combatant) string {
	switch {
	case c.Dead:
		return "dead"
	case c.Downed:
		if c.Stable {
			return "downed, stable"
		}
		return "downed"
	default:
		return fmt.Sprintf("%d/%d hp", c.HP+c.TempHP, c.EffectiveMax())
	}
}

func combatConditionLine(c combat.Combatant) string {
	if len(c.Conditions) == 0 {
		return ""
	}
	names := make([]string, 0, len(c.Conditions))
	for _, cond := range c.Conditions {
		names = append(names, cond.Name)
	}
	return " (" + strings.Join(names, ", ") + ")"
}

// partyLine spells one party strip: the vitals the board already decided
// this table shows.
func partyLine(m board.Member) string {
	var parts []string
	parts = append(parts, m.Name)
	switch {
	case m.Dead:
		parts = append(parts, "dead")
	case m.Down:
		parts = append(parts, "downed")
	case m.HP != nil && m.MaxHP != nil:
		parts = append(parts, fmt.Sprintf("%d/%d hp", *m.HP, *m.MaxHP))
	case m.Health != "":
		parts = append(parts, m.Health)
	}
	if m.Concentrating != "" {
		parts = append(parts, "concentrating: "+m.Concentrating)
	}
	if len(m.Conditions) > 0 {
		names := make([]string, 0, len(m.Conditions))
		for _, c := range m.Conditions {
			names = append(names, c.Name)
		}
		parts = append(parts, strings.Join(names, ", "))
	}
	if m.Inspired {
		parts = append(parts, "inspired")
	}
	return strings.Join(parts, " — ")
}

/* ---------- citations ---------- */

// clueCitation is one clue fact in the meta frame: discovered or hidden,
// the id linking back to the facts surface.
type clueCitation struct {
	ID          string `json:"id"`
	Statement   string `json:"statement"`
	Visibility  string `json:"visibility,omitempty"`
	Disposition string `json:"disposition,omitempty"`
}

// clueCitationsOf maps one clue list into the meta frame; hidden clues
// carry their scene disposition.
func clueCitationsOf(facts []campaign.Fact, dispositions map[string]string) []clueCitation {
	out := make([]clueCitation, 0, len(facts))
	for i := range facts {
		c := clueCitation{ID: facts[i].ID, Statement: facts[i].Statement, Visibility: facts[i].Visibility}
		if dispositions != nil {
			c.Disposition = dispositions[facts[i].ID]
		}
		out = append(out, c)
	}
	return out
}

/* ---------- the ask handler ---------- */

// handleCopilotAsk streams one play-mode answer as SSE: meta (the live
// context it grounded in — session, scene, combat summary, the voice, the
// clue lists, the citations), delta, done (the clean answer with the
// reveals tail split off), error. DM-only; nothing is written.
func (s *Server) handleCopilotAsk(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req struct {
		Question string `json:"question"`
		SceneID  string `json:"scene_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	req.Question = strings.TrimSpace(req.Question)
	if req.Question == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("question is required"))
		return
	}
	if !s.llm.Configured() {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf(
			"The session copilot is not configured. Set ANTHROPIC_API_KEY (and optionally ANTHROPIC_BASE_URL / ANTHROPIC_MODEL) to enable it."))
		return
	}

	g, err := s.groundCopilot(r.Context(), a, req.Question, strings.TrimSpace(req.SceneID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	npcName := ""
	meta := map[string]any{"discovered": clueCitationsOf(g.discovered, nil)}
	if g.session != nil {
		meta["session"] = map[string]any{"id": g.session.ID, "name": g.session.Name, "ordinal": g.session.Ordinal}
	}
	if g.scene != nil {
		meta["scene"] = map[string]any{"id": g.scene.ID, "name": g.scene.Name, "kind": g.scene.Kind}
		meta["hidden"] = clueCitationsOf(g.hidden, g.hiddenDisposition)
	}
	if g.combat != nil {
		meta["combat"] = map[string]any{"id": g.combat.ID, "name": g.combat.Name, "round": g.combat.Round}
	}
	if g.npc != nil {
		npcName = g.npc.npc.Name
		meta["npc"] = map[string]any{"id": g.npc.npc.ID, "name": npcName}
		meta["citations"] = npcCitationsOf(g.npc)
	}

	sse := newSSEWriter(w)
	sse.send("meta", meta)

	ctx, cancel := context.WithTimeout(r.Context(), copilotTimeout)
	defer cancel()
	system := copilotSystemPrompt(npcName)
	user := copilotUserMessage(g, req.Question)
	answer, streamErr := s.llm.StreamChat(ctx, system, []llm.Turn{{Role: "user", Content: user}}, func(text string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return sse.send("delta", map[string]any{"text": text})
	})
	answer = strings.TrimSpace(answer)
	if answer == "" && streamErr != nil {
		sse.send("error", map[string]any{"error": fmt.Sprintf("the copilot could not be reached: %v", streamErr)})
		return
	}
	clean, reveals, parseErr := splitReveals(answer)
	if reveals == nil {
		reveals = []npcReveal{}
	}
	done := map[string]any{"answer": clean, "reveals": reveals}
	if parseErr != nil {
		// Advisory, never fatal — the npcask tolerance.
		done["reveal_parse_error"] = parseErr.Error()
	}
	sse.send("done", done)
	if streamErr != nil {
		sse.send("error", map[string]any{"error": fmt.Sprintf("the answer was cut short: %v", streamErr)})
	}
}

/* ---------- the release handler ---------- */

// handleCopilotRelease is the copilot's only write, and only on the DM's
// explicit accept (a modify is an accept of an edited statement). It logs
// the discovery event against the live session — the Stage 4 anchor, the
// immutable record — and stages the canon item in the review queue: an
// npc_reveal when the answer had a voice, a session_capture proposed_fact
// otherwise. No fact, no awareness row, nothing player-visible is written
// here; the queue's decision remains the only gate.
func (s *Server) handleCopilotRelease(w http.ResponseWriter, r *http.Request) {
	if !s.canonEnabled(w) {
		return
	}
	if !s.sessionsEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req struct {
		Statement string `json:"statement"`
		Rationale string `json:"rationale"`
		Question  string `json:"question"`
		NPCID     string `json:"npc_id"`
		Subject   string `json:"subject"`
		SceneID   string `json:"scene_id"`
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	req.Statement = strings.TrimSpace(req.Statement)
	if req.Statement == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("a statement is required"))
		return
	}

	// The anchor: the given session must be this campaign's; otherwise
	// the live one. A release with no session to anchor to is refused —
	// go live first.
	var ses *gamesession.Session
	if sid := strings.TrimSpace(req.SessionID); sid != "" {
		got, ok := s.sessionInCampaign(w, r, a.campaign.ID, sid)
		if !ok {
			return
		}
		ses = got
	} else {
		list, err := s.sessions.ListSessions(r.Context(), a.campaign.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		for i := range list {
			if list[i].Status == gamesession.StatusLive {
				s := list[i]
				ses = &s
				break
			}
		}
	}
	if ses == nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("no live session to anchor the release — go live first"))
		return
	}

	// The voice, when the answer had one: the npc must be a live npc of
	// this campaign, resolved the way the ask endpoint resolves it.
	var npc *campaign.Entity
	if nid := strings.TrimSpace(req.NPCID); nid != "" {
		e, err := s.campaigns.GetEntity(r.Context(), campaign.ScopeDM, a.campaign.ID, nid)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if e.Kind != campaign.KindNPC || e.Status == campaign.StatusDeleted {
			writeError(w, http.StatusBadRequest, fmt.Errorf("entity %s is not a live npc", e.Name))
			return
		}
		npc = e
	}
	subject := strings.TrimSpace(req.Subject)
	if npc == nil && subject == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("a release needs the npc who revealed it or a subject entity"))
		return
	}

	// The Stage 4 anchor: the discovery event is the immutable record of
	// the moment, whatever the queue later decides.
	payload := map[string]any{"source": "copilot", "release": true}
	if req.Question != "" {
		payload["question"] = req.Question
	}
	if npc != nil {
		payload["npc_id"] = npc.ID
	}
	if sid := strings.TrimSpace(req.SceneID); sid != "" {
		payload["scene_id"] = sid
	}
	ev, err := s.sessions.AddEvent(r.Context(), ses.ID, gamesession.EventDiscovery, req.Statement, strings.TrimSpace(req.Rationale), payload)
	if err != nil {
		writeCampaignError(w, err)
		return
	}

	staged := map[string]any{}
	if npc != nil {
		rev, fresh, err := s.canon.StageNPCReveal(r.Context(), canon.StageRevealInput{
			CampaignID: a.campaign.ID,
			NPCID:      npc.ID,
			NPCName:    npc.Name,
			Statement:  req.Statement,
			Rationale:  strings.TrimSpace(req.Rationale),
			Question:   req.Question,
		})
		if err != nil {
			writeStoreError(w, err)
			return
		}
		staged = map[string]any{"kind": "npc_reveal", "review_id": rev.ID, "already_queued": !fresh}
	} else {
		batch, err := s.canon.StageBatch(r.Context(), canon.BatchInput{
			CampaignID: a.campaign.ID,
			Source:     canon.BatchSourceSessionCapture,
			Prompt: fmt.Sprintf("Session copilot release from session %d (%s): the discovery logged at seq %d. The event is the record; this proposes the fact.",
				ses.Ordinal, ses.Name, ev.Seq),
			CreatedBy: userID(r),
			Items: []canon.BatchItemInput{{
				ID:   "fact",
				Kind: "fact",
				Payload: map[string]any{
					"statement":        req.Statement,
					"subject":          subject,
					"predicate":        "reveals",
					"visibility":       campaign.VisibilityPublic,
					"session_id":       ses.ID,
					"session_event_id": ev.ID,
				},
			}},
		})
		if err != nil {
			writeStoreError(w, err)
			return
		}
		staged = map[string]any{"kind": "session_capture", "batch_id": batch.ID}
	}
	writeJSON(w, http.StatusCreated, map[string]any{"event": toSessionEventView(*ev), "staged": staged})
}
