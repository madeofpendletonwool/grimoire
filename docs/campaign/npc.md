# NPC simulation — ask as the Duke

How Grimoire answers *"What would Elara do if the players accuse her?"*
without generic LLM improv.

The whole design is one sentence from the epistemics doc, applied to a
character instead of a player: **perspective is authorization, not
instruction.** An NPC simulation is a retrieval at the `npc:<id>` scope,
fed to the model beside the NPC's authored mind. What the scope does not
return, the NPC does not know — and the model cannot leak it, because it is
never retrieved.

## The mind: structured agent fields

NPC entities carry an `agent` block inside their payload JSON
(`campaign.NPCAgent`, read/written via the API below). Payloads are dropped
at every non-DM scope, so the mind is DM structure exactly like a stat
block — and deliberately contains **no knowledge of its own**: what an NPC
knows is the awareness layer's job, enforced in SQL. A goal can never
smuggle in a fact the NPC has not been granted.

| Field | Meaning |
|---|---|
| `public_identity` | What they seem to be. |
| `private_truth` | What they actually are; the DM knows, the NPC acts from it. |
| `goals` | Ordered — first listed, first pursued. Cited as `[G#]`. |
| `fears` | What they avoid at cost. |
| `resources` | What they can bring to bear. |
| `personality` | How they behave. |
| `voice` | How they speak — feeds the in-voice dialogue. |

An NPC with no authored block is still simulatable from knowledge alone;
the prompt says so rather than refusing.

## The record: `npc:<id>` retrieval

Every campaign row in a simulation context arrives through the wide
knowledge store at `ScopeNPC(entityID)` — the same SQL-enforced scope the
party and character perspectives use (see `epistemics.md`):

- **facts**: exactly what the NPC's awareness rows grant (liveness rules
  included — never proposed, never superseded). A granted *secret* is
  genuinely in their head, marked `(secret)` for the DM; the NPC may act
  on it, conceal it, or lie about it as their goals dictate.
- **events**: only those the NPC personally witnessed.
- **entities / relationships**: the people and places their granted facts
  and witnessed events carry; payloads dropped.
- **prose search**: query-relevant entities, refetched through the scoped
  read path.

The reflection leak test in `internal/knowledge` enrolls the npc scope
automatically; the acceptance test for this feature asserts on the
assembled prompt itself.

## The answer

`POST /api/campaigns/{id}/npc/{npc}/ask` (DM-only):

```json
{"question": "How do you react to the accusation?", "stage": false}
```

The model is instructed to answer in two parts — a **REACTION** (third
person, DM-facing, tied to cited goals) and **IN-VOICE** dialogue — citing
fact markers `[F#]` and goal markers `[G#]`, and ending with a fenced JSON
`reveals` block listing every *invention*: anything the simulation needed
the world to contain that the record did not. The response splits that
block off and returns it as data:

```json
{
  "npc": {"id": "...", "name": "Duke Aldric Vane"},
  "answer": "REACTION — ...\n\nIN-VOICE — ...",
  "citations": {"facts": [...], "goals": [...], "entities": [...], "events": [...], "factions": [...]},
  "reveals": [{"statement": "...", "rationale": "..."}]
}
```

Faction membership colours the answer: the NPC's **own outgoing allegiance
edges** (`member_of`, `serves`, `worships`) attach the faction's public face
to the context. A faction that secretly controls the NPC from outside is
deliberately absent — the NPC does not know they are a puppet. When the
faction engine (Stage 5) lands current plans, this is where they plug in.

## Output is a suggestion, never a mutation

Nothing is written by default. With `"stage": true`, each reveal is staged
into the canon review queue as an `npc_reveal` item — the same human gate
every other machine proposal passes (`canon.StageNPCReveal`):

- idempotent per `(npc, statement)`; the never-resurrect rule applies;
- **accept** writes a canon fact whose provenance says exactly what
  happened: `method = ai_proposed`, `accepted_by` = the deciding human
  (the staging rule's one legal path to canon for a machine proposal);
- **modify then accept** writes the corrected statement;
- **dismiss** writes nothing.

A reveal is deliberately not an extraction candidate: the span rule binds
candidates to verbatim transcript spans, and the adversarial pass's
skepticism is span-quoted — neither has purchase on a simulated reaction.
The human gate is what makes a reveal canon.

## API

| Route | Who | What |
|---|---|---|
| `GET /api/campaigns/{id}/npc/{npc}/agent` | DM | the decoded agent block |
| `PUT /api/campaigns/{id}/npc/{npc}/agent` | DM | replace the agent block, preserving the rest of the payload |
| `POST /api/campaigns/{id}/npc/{npc}/ask` | DM | the simulation (above) |

The mind endpoints exist because the entity PATCH already carries payloads;
they add decoding, validation and a DM-only shape that survives payload
schema drift elsewhere.
