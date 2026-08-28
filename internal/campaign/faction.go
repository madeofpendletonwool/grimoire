package campaign

import "encoding/json"

// Faction dossier payload fields (MAD-366). A faction entity's Payload is
// free-form JSON like every other kind; the agent block lives under the same
// "agent" key an npc uses (an entity is one kind, so the two never collide)
// and decodes exactly the way NPCAgentOf does: tolerant of a missing block, a
// wrong-shaped block or an entity that is not a faction — all yield the zero
// FactionAgent.
//
// The block holds ONLY what the graph cannot say. Territory (owns/contains
// edges), leaders (leads/led_by), members (has_member), allies and enemies
// (allied_with/enemy_of) already live on the relationships table and are read
// from it live — copying any of that into the payload would create a second
// source of truth that drifts from the graph within a session. The
// public/private split mirrors NPCAgent.PublicIdentity/PrivateTruth for the
// same reason: a faction's reputation is player-facing and its actual aim is
// not.

// Faction plan statuses, the CHECK constraint migration 0017 enforces,
// mirrored here like every other schema vocabulary. A plan starts dormant,
// is activated by the DM, and either completes or is abandoned; 'stalled'
// is what the plan_stalled integrity check calls an active plan that has
// stopped moving — the check reports, it never writes.
const (
	PlanDormant   = "dormant"
	PlanActive    = "active"
	PlanStalled   = "stalled"
	PlanComplete  = "complete"
	PlanAbandoned = "abandoned"
)

// FactionAgent is the decoded "agent" block of a faction entity's payload.
// Every field is optional; a zero FactionAgent is a faction the DM has not
// written the interior of yet.
type FactionAgent struct {
	PublicFace        string   `json:"public_face"`        // what the realm believes it is
	PrivateTruth      string   `json:"private_truth"`      // what it actually is
	Doctrine          string   `json:"doctrine"`           // what it teaches or serves
	Goals             []string `json:"goals"`              // ordered; first listed, first pursued
	Reputation        string   `json:"reputation"`         // what is said of it in taverns
	Military          int      `json:"military"`           // 0-5-ish scales, deliberately unnumbered
	Economic          int      `json:"economic"`           // by rules: the DM decides what a 4 means
	Reach             int      `json:"reach"`              // how far its arm extends
	InternalConflicts []string `json:"internal_conflicts"` // the fault lines a clever party can pull
}

// factionPayloadKey is the payload key the agent block is stored under — the
// same key an npc's mind uses; an entity has exactly one kind.
const factionPayloadKey = "agent"

// FactionAgentOf decodes the agent block from a faction entity's payload.
func FactionAgentOf(e *Entity) FactionAgent {
	var a FactionAgent
	if e == nil {
		return a
	}
	raw, ok := e.Payload[factionPayloadKey]
	if !ok {
		return a
	}
	// Re-marshal the stored any and decode into the typed struct, the same
	// trick NPCAgentOf uses: payloads come back from SQLite as
	// map[string]any and a direct type assertion is brittle against
	// hand-edited JSON.
	if b, err := json.Marshal(raw); err == nil {
		_ = json.Unmarshal(b, &a)
	}
	a.Goals = cleanList(a.Goals)
	a.InternalConflicts = cleanList(a.InternalConflicts)
	a.PublicFace = cleanLine(a.PublicFace)
	a.PrivateTruth = cleanLine(a.PrivateTruth)
	a.Doctrine = cleanLine(a.Doctrine)
	a.Reputation = cleanLine(a.Reputation)
	return a
}

// WithFactionAgent returns a copy of payload with the faction agent block
// replaced, preserving every other key. nil payload is treated as an empty
// map.
func WithFactionAgent(payload map[string]any, agent FactionAgent) map[string]any {
	out := make(map[string]any, len(payload)+1)
	for k, v := range payload {
		out[k] = v
	}
	agent.Goals = cleanList(agent.Goals)
	agent.InternalConflicts = cleanList(agent.InternalConflicts)
	agent.PublicFace = cleanLine(agent.PublicFace)
	agent.PrivateTruth = cleanLine(agent.PrivateTruth)
	agent.Doctrine = cleanLine(agent.Doctrine)
	agent.Reputation = cleanLine(agent.Reputation)
	if b, err := json.Marshal(agent); err == nil {
		var block map[string]any
		if json.Unmarshal(b, &block) == nil {
			out[factionPayloadKey] = block
		}
	}
	return out
}

/* ---------- the dossier's edge read ---------- */

// FactionEdges is the dossier's view of a faction's graph position. Every
// field is entity ids; names are the caller's join. The lists are derived,
// never stored — adding an owns edge changes territory with no write to the
// faction entity, which is the rule the dossier exists to keep.
type FactionEdges struct {
	// Territory: targets of the faction's outgoing owns edges and objects of
	// owned_by edges pointing back at it, plus anything it contains.
	Territory []string
	// Leaders: entities the faction leads (leads) or is led by (led_by).
	Leaders []string
	// Members: the faction's has_member edges, either direction.
	Members []string
	// Allies and Enemies: allied_with / enemy_of edges, either direction —
	// the vocabulary seeds them symmetric but a DM may write one side only.
	Allies  []string
	Enemies []string
	// Puppets: what the faction secretly_controls, either direction.
	Puppets []string
}

// factionEdgeGroups maps each controlled edge type to the dossier field it
// feeds. Edges outside the map are not dossier material — they still render
// in the entity sheet's plain Ties list; the dossier is a reading, not a new
// store.
var factionEdgeGroups = map[string]string{
	"owns":                   "territory",
	"owned_by":               "territory",
	"contains":               "territory",
	"leads":                  "leaders",
	"led_by":                 "leaders",
	"has_member":             "members",
	"member_of":              "members",
	"allied_with":            "allies",
	"enemy_of":               "enemies",
	"secretly_controls":      "puppets",
	"secretly_controlled_by": "puppets",
}

// FactionEdgesOf categorizes the edges touching one faction. rels is any
// scope's edge list (the DM's whole graph or a perspective's aware slice);
// only edges with factionID on one end are read, and the other end is
// reported. Ids are deduplicated per group, order preserved.
func FactionEdgesOf(factionID string, rels []Relationship) FactionEdges {
	var out FactionEdges
	seen := map[string]map[string]bool{}
	add := func(group, id string) {
		if id == "" || id == factionID {
			return
		}
		if seen[group] == nil {
			seen[group] = map[string]bool{}
		}
		if seen[group][id] {
			return
		}
		seen[group][id] = true
		switch group {
		case "territory":
			out.Territory = append(out.Territory, id)
		case "leaders":
			out.Leaders = append(out.Leaders, id)
		case "members":
			out.Members = append(out.Members, id)
		case "allies":
			out.Allies = append(out.Allies, id)
		case "enemies":
			out.Enemies = append(out.Enemies, id)
		case "puppets":
			out.Puppets = append(out.Puppets, id)
		}
	}
	for _, r := range rels {
		var other string
		switch {
		case r.FromEntity == factionID:
			other = r.ToEntity
		case r.ToEntity == factionID:
			other = r.FromEntity
		default:
			continue
		}
		if group, ok := factionEdgeGroups[r.RelType]; ok {
			add(group, other)
		}
	}
	return out
}
