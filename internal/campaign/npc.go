package campaign

import (
	"encoding/json"
	"strings"
)

// NPC simulation's structured agent fields (MAD-313). An npc entity's Payload
// is free-form JSON; the agent block lives under the "agent" key so it cannot
// collide with the other DM structure a payload carries (stat blocks, notes).
//
// The fields are the NPC's mind, not the world: public identity and private
// truth are what they seem to be and what they actually are; goals are
// ordered (first listed, first pursued); fears, resources, personality and
// voice notes feed the "ask as this NPC" simulation. Knowledge is NOT stored
// here — what an NPC knows is the awareness layer's job, enforced in SQL at
// the npc:<id> scope, so a goal can never smuggle in a fact the NPC has not
// been granted.

// NPCAgent is the decoded "agent" block of an npc entity's payload. Every
// field is optional; a zero NPCAgent is a valid, empty mind.
type NPCAgent struct {
	PublicIdentity string   `json:"public_identity"`
	PrivateTruth   string   `json:"private_truth"`
	Goals          []string `json:"goals"`
	Fears          []string `json:"fears"`
	Resources      []string `json:"resources"`
	Personality    string   `json:"personality"`
	Voice          string   `json:"voice"`
}

// npcPayloadKey is the payload key the agent block is stored under.
const npcPayloadKey = "agent"

// NPCAgentOf decodes the agent block from an entity's payload, tolerating a
// missing block, a wrong-shaped block, or an entity that is not an npc: those
// all yield the zero NPCAgent. The ask path renders the zero agent as "the DM
// has not written this mind yet" rather than refusing — an NPC with no
// authored persona is still simulatable from its knowledge alone.
func NPCAgentOf(e *Entity) NPCAgent {
	var a NPCAgent
	if e == nil {
		return a
	}
	raw, ok := e.Payload[npcPayloadKey]
	if !ok {
		return a
	}
	// Re-marshal the stored any and decode into the typed struct: payloads
	// come back from SQLite as map[string]any, and a direct type assertion
	// would be brittle against hand-edited JSON.
	if b, err := json.Marshal(raw); err == nil {
		_ = json.Unmarshal(b, &a)
	}
	a.Goals = cleanList(a.Goals)
	a.Fears = cleanList(a.Fears)
	a.Resources = cleanList(a.Resources)
	a.PublicIdentity = cleanLine(a.PublicIdentity)
	a.PrivateTruth = cleanLine(a.PrivateTruth)
	a.Personality = cleanLine(a.Personality)
	a.Voice = cleanLine(a.Voice)
	return a
}

// WithAgent returns a copy of payload with the agent block replaced,
// preserving every other key. nil payload is treated as an empty map.
func WithAgent(payload map[string]any, agent NPCAgent) map[string]any {
	out := make(map[string]any, len(payload)+1)
	for k, v := range payload {
		out[k] = v
	}
	agent.Goals = cleanList(agent.Goals)
	agent.Fears = cleanList(agent.Fears)
	agent.Resources = cleanList(agent.Resources)
	agent.PublicIdentity = cleanLine(agent.PublicIdentity)
	agent.PrivateTruth = cleanLine(agent.PrivateTruth)
	agent.Personality = cleanLine(agent.Personality)
	agent.Voice = cleanLine(agent.Voice)
	if b, err := json.Marshal(agent); err == nil {
		var block map[string]any
		if json.Unmarshal(b, &block) == nil {
			out[npcPayloadKey] = block
		}
	}
	return out
}

// cleanList trims each entry and drops the empty ones, preserving order —
// the order IS the priority for goals.
func cleanList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = cleanLine(s); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// cleanLine collapses whitespace runs and trims: hand-typed payload prose
// should not leave stray newlines that wander into prompts.
func cleanLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
