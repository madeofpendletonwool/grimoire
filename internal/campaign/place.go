package campaign

import "encoding/json"

// The structured location block (MAD-370). A location entity's Payload is
// free-form JSON like every other kind; the place block lives under the
// "place" key — its own key, so it cannot collide with the "travel" block
// MAD-365 puts on the same payload or a DM's own notes — and decodes exactly
// the way the NPC mind does (NPCAgentOf): tolerant of a missing block, a
// wrong-shaped block or an entity that is not a location, all of which yield
// the zero Place.
//
// The block holds ONLY what the graph cannot say. Present NPCs
// (located_in/contains edges), child locations, items here, secrets (facts
// with visibility='secret'), history (events with location_entity) and
// sited quests already live on the graph and are read live by the dossier
// (internal/place) — copying any of that into the payload would create a
// second source of truth that drifts from the graph within one session.
//
// The description does not go in the payload either: the campaign prose
// index is built from name || summary (0003_knowledge.sql, trigger
// campaign_prose_entity_upd), so a description written into payload is
// invisible to campaign search and every retrieval path the chat uses. The
// one-paragraph read-aloud lives in entities.summary; the structure lives
// here.

// Place is the decoded "place" block of a location entity's payload. Every
// field is optional; a zero Place is a location the DM has not written the
// interior of yet.
type Place struct {
	Kind         string   `json:"kind"`                    // settlement kind: hamlet, town, city, ruin, dungeon, wilderness…
	Scale        string   `json:"scale"`                   // how big it reads at the table
	Population   string   `json:"population"`              // population band, free prose
	Government   string   `json:"government"`              // who holds it
	Services     []string `json:"services"`                // notable services: inn, market, temple…
	Defences     string   `json:"defences"`                // walls, watch and wards
	Climate      string   `json:"climate"`                 // the tag clock.Weather reads (desert/tropical/arctic/…)
	Senses       []string `json:"senses"`                  // sensory notes: what the party hears and smells
	State        string   `json:"state"`                   // current state: flooding, under siege, abandoned…
	Danger       int      `json:"danger"`                  // 0-5-ish, deliberately unnumbered by rules
	PrivateTruth string   `json:"private_truth,omitempty"` // what is really going on; DM-only
}

// placePayloadKey is the payload key the place block is stored under.
const placePayloadKey = "place"

// PlaceOf decodes the place block from an entity's payload, tolerating a
// missing block, a wrong-shaped block, or an entity that is not a location:
// those all yield the zero Place. The dossier renders the zero block as "the
// DM has not written this place yet" rather than refusing — a location with
// no authored interior is still a node of the graph.
func PlaceOf(e *Entity) Place {
	var p Place
	if e == nil || e.Kind != KindLocation {
		return p
	}
	raw, ok := e.Payload[placePayloadKey]
	if !ok {
		return p
	}
	// Re-marshal the stored any and decode into the typed struct, the same
	// trick NPCAgentOf uses: payloads come back from SQLite as
	// map[string]any and a direct type assertion is brittle against
	// hand-edited JSON.
	if b, err := json.Marshal(raw); err == nil {
		_ = json.Unmarshal(b, &p)
	}
	p.Services = cleanList(p.Services)
	p.Senses = cleanList(p.Senses)
	p.Kind = cleanLine(p.Kind)
	p.Scale = cleanLine(p.Scale)
	p.Population = cleanLine(p.Population)
	p.Government = cleanLine(p.Government)
	p.Defences = cleanLine(p.Defences)
	p.Climate = cleanLine(p.Climate)
	p.State = cleanLine(p.State)
	p.PrivateTruth = cleanLine(p.PrivateTruth)
	return p
}

// WithPlace returns a copy of payload with the place block replaced,
// preserving every other key — the travel block above all. nil payload is
// treated as an empty map.
func WithPlace(payload map[string]any, p Place) map[string]any {
	out := make(map[string]any, len(payload)+1)
	for k, v := range payload {
		out[k] = v
	}
	p.Services = cleanList(p.Services)
	p.Senses = cleanList(p.Senses)
	p.Kind = cleanLine(p.Kind)
	p.Scale = cleanLine(p.Scale)
	p.Population = cleanLine(p.Population)
	p.Government = cleanLine(p.Government)
	p.Defences = cleanLine(p.Defences)
	p.Climate = cleanLine(p.Climate)
	p.State = cleanLine(p.State)
	p.PrivateTruth = cleanLine(p.PrivateTruth)
	if b, err := json.Marshal(p); err == nil {
		var block map[string]any
		if json.Unmarshal(b, &block) == nil {
			out[placePayloadKey] = block
		}
	}
	return out
}

// ClimateOf reads a location's climate tag: the place block's climate first,
// then the bare top-level "climate" tag a payload may carry from before the
// block existed. That top-level tag is what the clock's weather read wrote
// against (MAD-365); the place block is its structured home now, and both
// spellings keep working.
func ClimateOf(e *Entity) string {
	if e == nil {
		return ""
	}
	if p := PlaceOf(e); p.Climate != "" {
		return p.Climate
	}
	tag, _ := e.Payload["climate"].(string)
	return tag
}
