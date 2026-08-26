package server

// NPC simulation (MAD-313): "ask as the Duke". Once perspective is
// authorization, roleplaying an NPC stops being generic LLM improv and
// becomes a query over that NPC's mind and record: the structured agent
// fields on the entity (public identity, private truth, ordered goals,
// fears, resources, personality, voice), plus every fact the NPC's awareness
// covers — and nothing else.
//
// The one rule this file inherits from the knowledge layer (ADR 2), word for
// word: perspective is authorization, not instruction. Every campaign row in
// an NPC's context arrives through a scope-filtered SQL query at
// npc:<entity-id>. The Duke does not know the party holds the second relic
// unless a fact says he does, and the model cannot leak it, because it is
// never retrieved — the prompt never contains it, which is exactly what the
// leak test asserts. The system prompt below carries behaviour (react from
// persona and record, cite markers, list inventions as reveals), never
// secrecy.
//
// Output is a suggestion, never a mutation. Nothing here writes the graph:
// the answer and its reveals come back as data. Only an explicit stage=true
// opt-in stages reveals into the canon review queue (canon.StageNPCReveal)
// behind the same "Make it canon" gate every other machine proposal passes,
// and a staged reveal is invisible to every retrieval path until a human
// accepts it (ADR 3).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
)

const (
	// npcFactLimit caps the granted facts fed to one simulation. Awareness
	// rows are DM-granted and bounded by play, not by transcript length;
	// the cap is a runaway guard, not a relevance filter. Newest first.
	npcFactLimit = 40
	// npcEventLimit is how many recently witnessed events ride along.
	npcEventLimit = 8
	// npcEntityHits bounds the query-relevant met-entity retrieval.
	npcEntityHits = 8
	// npcRelLimit caps the visible relationship edges in the context.
	npcRelLimit = 20
	// npcAskTimeout bounds one simulation call end to end.
	npcAskTimeout = 2 * time.Minute
)

/* ---------- the structured agent fields ---------- */

// handleGetNPCAgent returns an npc's decoded agent block. DM-only: the mind
// of an NPC is DM structure, same as the payload it is stored in.
func (s *Server) handleGetNPCAgent(w http.ResponseWriter, r *http.Request) {
	e := s.resolveNPC(w, r)
	if e == nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"npc": e.ID, "agent": campaign.NPCAgentOf(e)})
}

// handlePutNPCAgent replaces an npc's structured agent fields, preserving the
// rest of the entity's payload. DM-only.
func (s *Server) handlePutNPCAgent(w http.ResponseWriter, r *http.Request) {
	e := s.resolveNPC(w, r)
	if e == nil {
		return
	}
	var agent campaign.NPCAgent
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&agent); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	updated, err := s.campaigns.UpdateEntity(r.Context(), e.CampaignID, e.ID,
		nil, nil, nil, campaign.WithAgent(e.Payload, agent))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"npc": e.ID, "agent": campaign.NPCAgentOf(updated)})
}

// resolveNPC loads the npc path value at the DM scope, writing the error
// response itself. It returns nil when the caller should not proceed: 404
// for a foreign or missing entity, 400 for one that is not a live npc.
func (s *Server) resolveNPC(w http.ResponseWriter, r *http.Request) *campaign.Entity {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return nil
	}
	if !a.requireDM(w) {
		return nil
	}
	e, err := s.campaigns.GetEntity(r.Context(), campaign.ScopeDM, a.campaign.ID, r.PathValue("npc"))
	if err != nil {
		writeStoreError(w, err)
		return nil
	}
	if e.Kind != campaign.KindNPC {
		writeError(w, http.StatusBadRequest, fmt.Errorf("entity %s is a %s, not an npc", e.Name, e.Kind))
		return nil
	}
	if e.Status == campaign.StatusDeleted {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%s is deleted", e.Name))
		return nil
	}
	return e
}

/* ---------- grounding ---------- */

// npcGrounding is one question's retrieval for one NPC: the mind (agent
// fields, faction allegiances) and the record (everything the npc:<id> scope
// may read). It is the whole campaign context for the simulation — nothing
// else about the campaign is read for the answer.
type npcGrounding struct {
	npc      campaign.Entity
	agent    campaign.NPCAgent
	facts    []campaign.Fact
	entities []campaign.Entity
	events   []campaign.Event
	rels     []campaign.Relationship
	factions []campaign.Entity
	// empty records that the NPC's awareness covers nothing yet.
	empty bool
}

// npcAllegianceRels are the edge types that make a faction part of an NPC's
// own motivation. Only the NPC's outgoing edges count: a faction secretly
// controlling an unwitting NPC is DM structure the NPC's simulation has no
// business knowing (and the faction engine, when it lands, will color this
// further).
var npcAllegianceRels = map[string]bool{
	"member_of": true,
	"serves":    true,
	"worships":  true,
}

// groundNPC assembles one NPC's context. Every campaign read at the NPC's
// perspective runs through the wide knowledge store at npc:<id> — the same
// SQL-enforced scope every other perspective uses. The mind (agent fields,
// allegiances) is read at the DM scope: it is the NPC's own interior, and
// this endpoint is DM-only.
func (s *Server) groundNPC(ctx context.Context, a *campAccess, npc *campaign.Entity, question string) (*npcGrounding, error) {
	g := &npcGrounding{npc: *npc, agent: campaign.NPCAgentOf(npc)}
	cid := a.campaign.ID
	scope := campaign.ScopeNPC(npc.ID)

	facts, err := s.knowledge.Facts(ctx, scope, cid, knowledge.FactFilter{})
	if err != nil {
		return nil, err
	}
	if len(facts) > npcFactLimit {
		facts = facts[:npcFactLimit]
	}
	g.facts = facts

	if timeline, err := s.knowledge.Timeline(ctx, scope, cid); err != nil {
		return nil, err
	} else if len(timeline) > npcEventLimit {
		g.events = timeline[len(timeline)-npcEventLimit:]
	} else {
		g.events = timeline
	}

	rels, err := s.knowledge.Relationships(ctx, scope, cid)
	if err != nil {
		return nil, err
	}
	if len(rels) > npcRelLimit {
		rels = rels[len(rels)-npcRelLimit:]
	}
	g.rels = rels

	// Query-relevant met entities, refetched through the same scoped read
	// path — a prose hit the scope cannot refetch is dropped, defense in
	// depth. Payloads come back empty at this scope: the NPC knows who
	// someone is, not their stat block.
	hits, err := s.knowledge.SearchProse(ctx, scope, cid, question, npcEntityHits)
	if err != nil && !errors.Is(err, knowledge.ErrEmptyQuery) {
		return nil, err
	}
	seen := map[string]bool{npc.ID: true}
	for _, h := range hits {
		if h.Kind != "entity" || seen[h.RefID] {
			continue
		}
		e, err := s.knowledge.Entity(ctx, scope, cid, h.RefID)
		if err != nil {
			continue
		}
		seen[e.ID] = true
		g.entities = append(g.entities, *e)
	}

	// Faction allegiances, DM-side: the NPC's own declared edges. The
	// faction's name and summary are its public face; its current plan is
	// Stage 5's to add.
	dmRels, err := s.campaigns.RelationshipsOf(ctx, campaign.ScopeDM, cid, npc.ID)
	if err != nil {
		return nil, err
	}
	for _, rel := range dmRels {
		if !npcAllegianceRels[rel.RelType] || seen[rel.ToEntity] {
			continue
		}
		f, err := s.campaigns.GetEntity(ctx, campaign.ScopeDM, cid, rel.ToEntity)
		if err != nil || f.Kind != campaign.KindFaction {
			continue
		}
		seen[f.ID] = true
		g.factions = append(g.factions, *f)
	}

	g.empty = len(g.facts) == 0 && len(g.entities) == 0 && len(g.events) == 0
	return g, nil
}

/* ---------- citations ---------- */

type npcFactCitation struct {
	ID         string `json:"id"`
	Statement  string `json:"statement"`
	Visibility string `json:"visibility,omitempty"`
}

type npcEntityCitation struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Summary string `json:"summary,omitempty"`
}

type npcEventCitation struct {
	ID      string `json:"id"`
	Summary string `json:"summary"`
}

// npcCitations is the marker key the answer's inline [F#]/[G#] citations
// resolve against.
type npcCitations struct {
	Facts    []npcFactCitation   `json:"facts"`
	Entities []npcEntityCitation `json:"entities"`
	Events   []npcEventCitation  `json:"events"`
	Goals    []string            `json:"goals"`
	Factions []npcEntityCitation `json:"factions"`
}

func npcCitationsOf(g *npcGrounding) npcCitations {
	out := npcCitations{}
	for i := range g.facts {
		f := g.facts[i]
		out.Facts = append(out.Facts, npcFactCitation{ID: f.ID, Statement: f.Statement, Visibility: f.Visibility})
	}
	for i := range g.entities {
		e := g.entities[i]
		out.Entities = append(out.Entities, npcEntityCitation{ID: e.ID, Kind: e.Kind, Name: e.Name, Summary: e.Summary})
	}
	for i := range g.events {
		out.Events = append(out.Events, npcEventCitation{ID: g.events[i].ID, Summary: g.events[i].Summary})
	}
	out.Goals = g.agent.Goals
	for i := range g.factions {
		f := g.factions[i]
		out.Factions = append(out.Factions, npcEntityCitation{ID: f.ID, Kind: f.Kind, Name: f.Name, Summary: f.Summary})
	}
	return out
}

/* ---------- the prompt ---------- */

// npcSystemPrompt is the standing instruction for one simulation. Behaviour
// only — react from persona and record, cite markers, put inventions in the
// reveals block — never secrecy: the record below was assembled under a SQL
// scope filter, so this prompt has no secret to keep and nothing to leak if
// the model ignores every word of it.
func npcSystemPrompt(name string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `You are simulating %s, a non-player character in a tabletop campaign, for the Dungeon Master who runs them.`, name)
	b.WriteString("\n\nGROUNDING RULES — follow these strictly:")
	b.WriteString(`
1. React ONLY from the persona and the record of what ` + name + ` knows, both provided below. The record is scope-filtered: what it does not contain, ` + name + ` does not know. They may reason as a person generally would, but may NOT assert facts about this campaign's people, places, plots or items that the record does not carry.
2. Answer in exactly two parts:
   REACTION — two to four sentences, third person, DM-facing: what ` + name + ` would do and why, tied to the goals and fears you cite.
   IN-VOICE — what ` + name + ` would actually say, written as speakable dialogue in their voice.
3. Cite inline as you go: record facts as [F#], goals as [G#].
4. A fact marked (secret) in the record is something ` + name + ` genuinely knows. They may act on it, conceal it, or lie about it as their goals dictate — the mark is for the DM, not a restriction on the character.
5. If the simulation needs the world to contain something the record does not — a new name, deed, motive or place — that is an invention, not a fact. List every invention under "reveals" in the closing JSON block, phrased as a campaign fact the DM could accept. Never state an invention as already true in the REACTION or IN-VOICE parts.
6. Keep it usable at the table: concrete, in character, no preamble about being a simulation.

End the reply with a fenced json block of exactly this shape:
` + "```json\n{\"reveals\": [{\"statement\": \"...\", \"rationale\": \"...\"}]}\n```" + `
An answer that invented nothing ends with an empty reveals list.`)
	return b.String()
}

// npcUserMessage assembles the final prompt turn: the mind, then the record,
// then the question. This is the exact text the model receives — the
// function the leak assertions in the tests read, so it stays pure and
// testable.
func npcUserMessage(g *npcGrounding, question string) string {
	name := g.npc.Name
	var b strings.Builder

	fmt.Fprintf(&b, "=== THE MIND OF %s ===\n", strings.ToUpper(name))
	agent := g.agent
	if agent.PublicIdentity != "" {
		fmt.Fprintf(&b, "Public identity: %s\n", agent.PublicIdentity)
	}
	if agent.PrivateTruth != "" {
		fmt.Fprintf(&b, "Private truth (the DM knows; %s acts from it): %s\n", name, agent.PrivateTruth)
	}
	if agent.Personality != "" {
		fmt.Fprintf(&b, "Personality: %s\n", agent.Personality)
	}
	if agent.Voice != "" {
		fmt.Fprintf(&b, "Voice notes: %s\n", agent.Voice)
	}
	if len(agent.Goals) > 0 {
		b.WriteString("Goals, in priority order:\n")
		for i, goal := range agent.Goals {
			fmt.Fprintf(&b, "  [G%d] %s\n", i+1, goal)
		}
	}
	if len(agent.Fears) > 0 {
		b.WriteString("Fears:\n")
		for _, fear := range agent.Fears {
			fmt.Fprintf(&b, "  - %s\n", fear)
		}
	}
	if len(agent.Resources) > 0 {
		b.WriteString("Resources at their disposal:\n")
		for _, res := range agent.Resources {
			fmt.Fprintf(&b, "  - %s\n", res)
		}
	}
	if len(agent.Goals) == 0 && agent.PrivateTruth == "" && agent.Personality == "" {
		fmt.Fprintf(&b, "(The DM has not written this mind yet — infer %s's motives only from the record below.)\n", name)
	}
	if len(g.factions) > 0 {
		b.WriteString("\nFaction allegiances (these commitments shape every motive):\n")
		for i := range g.factions {
			f := g.factions[i]
			line := fmt.Sprintf("  - %s", f.Name)
			if f.Summary != "" {
				line += ": " + f.Summary
			}
			b.WriteString(line + "\n")
		}
	}

	fmt.Fprintf(&b, "\n=== WHAT %s KNOWS — the whole record at this perspective ===\n", strings.ToUpper(name))
	if g.empty {
		b.WriteString("(nothing — no fact, entity or event is known at this perspective yet)\n")
	}
	for i := range g.facts {
		f := g.facts[i]
		mark := fmt.Sprintf("[F%d] %s", i+1, f.Statement)
		if f.Visibility == campaign.VisibilitySecret {
			mark = fmt.Sprintf("[F%d] (secret) %s", i+1, f.Statement)
		}
		b.WriteString(mark + "\n")
	}
	if len(g.entities) > 0 {
		b.WriteString("\nPeople and places they have dealings with:\n")
		for i := range g.entities {
			e := g.entities[i]
			line := fmt.Sprintf("  - %s (%s)", e.Name, e.Kind)
			if e.Summary != "" {
				line += ": " + e.Summary
			}
			b.WriteString(line + "\n")
		}
	}
	if len(g.rels) > 0 {
		b.WriteString("\nStanding relationships:\n")
		for _, rel := range g.rels {
			fmt.Fprintf(&b, "  - %s → %s → %s\n", entityLabel(g, rel.FromEntity), rel.RelType, entityLabel(g, rel.ToEntity))
		}
	}
	if len(g.events) > 0 {
		b.WriteString("\nEvents they witnessed, oldest first:\n")
		for _, ev := range g.events {
			fmt.Fprintf(&b, "  - %s\n", ev.Summary)
		}
	}

	fmt.Fprintf(&b, "\nQuestion for %s: %s\n", name, question)
	return b.String()
}

// entityLabel names one end of a relationship edge, falling back to the id
// for entities outside the assembled context.
func entityLabel(g *npcGrounding, id string) string {
	if id == g.npc.ID {
		return g.npc.Name
	}
	for i := range g.entities {
		if g.entities[i].ID == id {
			return g.entities[i].Name
		}
	}
	for i := range g.factions {
		if g.factions[i].ID == id {
			return g.factions[i].Name
		}
	}
	return id
}

/* ---------- the reveals tail ---------- */

// npcReveal is one invention the simulation flagged: a campaign assertion
// that was not in the record. A suggestion, never a fact.
type npcReveal struct {
	Statement string `json:"statement"`
	Rationale string `json:"rationale"`
}

type npcRevealBlock struct {
	Reveals []npcReveal `json:"reveals"`
}

// splitReveals separates the model's reply into the prose answer and its
// closing reveals block. Only the final fenced block counts — the contract
// asks for one block at the end — and a block that is absent, unparseable,
// or reveal-less changes nothing: the reveals are advisory, so losing them
// to a formatting hiccup is noted, never fatal. A final block that names
// "reveals" is stripped from the prose even when malformed, so the DM never
// reads raw JSON as dialogue.
func splitReveals(text string) (answer string, reveals []npcReveal, parseErr error) {
	pos := fencePositions(text)
	if len(pos) < 2 {
		return text, nil, nil
	}
	open, close := pos[len(pos)-2], pos[len(pos)-1]
	if strings.TrimSpace(text[close+3:]) != "" {
		// The last fenced block is not the reply's tail — it is dialogue
		// or quotation, not the reveals contract.
		return text, nil, nil
	}
	block := text[open+3 : close]
	if nl := strings.IndexByte(block, '\n'); nl >= 0 && !strings.Contains(block[:nl], "{") {
		block = block[nl+1:] // drop the fence's info string (```json)
	}
	answer = strings.TrimSpace(text[:open])
	var parsed npcRevealBlock
	if err := json.Unmarshal([]byte(strings.TrimSpace(block)), &parsed); err != nil {
		return answer, nil, err
	}
	for _, rv := range parsed.Reveals {
		rv.Statement = strings.TrimSpace(rv.Statement)
		if rv.Statement == "" {
			continue
		}
		reveals = append(reveals, npcReveal{Statement: rv.Statement, Rationale: strings.TrimSpace(rv.Rationale)})
	}
	return answer, reveals, nil
}

// fencePositions lists every ``` offset in text, in order.
func fencePositions(text string) []int {
	var out []int
	off := 0
	for {
		i := strings.Index(text[off:], "```")
		if i < 0 {
			return out
		}
		off += i
		out = append(out, off)
		off += 3
	}
}

/* ---------- the ask handler ---------- */

// handleNPCAsk answers "what would this NPC do?" for the DM: reaction
// prediction and in-voice dialogue, grounded in the NPC's mind and scoped
// record, cited to the facts and goals used, with any invention returned as
// reveal suggestions. Writes nothing unless the DM explicitly asks for the
// reveals to be staged into the review queue.
func (s *Server) handleNPCAsk(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	npc, err := s.campaigns.GetEntity(r.Context(), campaign.ScopeDM, a.campaign.ID, r.PathValue("npc"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if npc.Kind != campaign.KindNPC {
		writeError(w, http.StatusBadRequest, fmt.Errorf("entity %s is a %s, not an npc", npc.Name, npc.Kind))
		return
	}
	if npc.Status == campaign.StatusDeleted {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%s is deleted", npc.Name))
		return
	}

	var req struct {
		Question string `json:"question"`
		Stage    bool   `json:"stage"`
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
			"NPC simulation is not configured. Set ANTHROPIC_API_KEY (and optionally ANTHROPIC_BASE_URL / ANTHROPIC_MODEL) to enable it."))
		return
	}
	if req.Stage && !s.canonEnabled(w) {
		return
	}

	g, err := s.groundNPC(r.Context(), a, npc, req.Question)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), npcAskTimeout)
	defer cancel()
	raw, err := s.llm.AnswerPrompt(ctx, npcSystemPrompt(npc.Name), npcUserMessage(g, req.Question))
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("the simulation could not be run: %v", err))
		return
	}
	answer, reveals, parseErr := splitReveals(raw)
	if reveals == nil {
		reveals = []npcReveal{}
	}

	resp := map[string]any{
		"npc":       map[string]any{"id": npc.ID, "name": npc.Name},
		"answer":    answer,
		"citations": npcCitationsOf(g),
		"reveals":   reveals,
	}
	if parseErr != nil {
		resp["reveal_parse_error"] = parseErr.Error()
	}

	// The one write path, and only on explicit opt-in: stage each reveal
	// into the same review queue every other machine proposal uses.
	if req.Stage && len(reveals) > 0 {
		type stagedReveal struct {
			ReviewID      string `json:"review_id"`
			Statement     string `json:"statement"`
			AlreadyQueued bool   `json:"already_queued"`
		}
		var staged []stagedReveal
		for _, rv := range reveals {
			rev, fresh, err := s.canon.StageNPCReveal(r.Context(), canon.StageRevealInput{
				CampaignID: a.campaign.ID,
				NPCID:      npc.ID,
				NPCName:    npc.Name,
				Statement:  rv.Statement,
				Rationale:  rv.Rationale,
				Question:   req.Question,
			})
			if err != nil {
				writeStoreError(w, err)
				return
			}
			staged = append(staged, stagedReveal{ReviewID: rev.ID, Statement: rv.Statement, AlreadyQueued: !fresh})
		}
		resp["staged"] = staged
	}
	writeJSON(w, http.StatusOK, resp)
}
