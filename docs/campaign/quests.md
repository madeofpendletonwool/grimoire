# Quests: named branches, terminal states, and the links into the graph

A quest was already a state machine — `campaign.ParseStateMachine` parses
and validates it, `HasEdge` enforces legal moves, and `TransitionQuest`
ties every move to the event that caused it. That part was right and is
untouched.

What was missing is everything around the machine: the branch you take has
to be distinguishable from the branch you decline (*trust the survivor* vs
*accuse the survivor*), a quest has to know who gave it, where it happens,
and what it pays, some states are endings, and the whole thing has to sit
somewhere in the campaign's act structure. Stage 1 is deterministic: not one
model call.

## The machine JSON grows, and stays backwards compatible

`state_machine` is a JSON column, so most of this needed no migration. Both
shapes parse; the store always writes the keyed form.

The original shape — every quest authored before MAD-369:

```json
{"initial": "unknown", "states": ["unknown", "found", "done"],
 "edges": [{"from": "unknown", "to": "found"}, {"from": "found", "to": "done"}]}
```

The keyed form:

```json
{"initial": "unknown",
 "states": [
   {"key": "unknown", "label": "The miners are missing"},
   {"key": "found", "label": "The caravan is found",
    "detail": "DM only: the robed figures took them east."},
   {"key": "done", "label": "The miners come home", "terminal": "success"}],
 "edges": [
   {"from": "unknown", "to": "found", "label": "find the caravan"},
   {"from": "found", "to": "trusted", "label": "trust the survivor",
    "requires": ["<fact-id>"]}]}
```

- **`label`** — the human name of a state or a branch. Two edges out of one
  state are distinguished by exactly this field.
- **`detail`** — DM-facing prose. Never served to a player scope.
- **`terminal`** — `""` for a passing state; `success`, `failure` or
  `abandoned` for an ending. `quest_dead_end` does not fire on an ending.
- **`requires`** — fact ids the move presumes the party discovered. A
  recorded move along an edge whose requirement nobody grants is
  `quest_transition_ungrounded` below.

A plain-string `states` array parses into keyed states with empty labels and
re-serialises to the keyed form without losing a state — a test pins the
round-trip. No migration rewrites a DM's JSON in place.

## The columns and the two join tables (migration 0021)

`quests` gains:

- `summary` — one line of what the quest is.
- `status` — `active | complete | failed | abandoned`. Only an active quest
  moves; `DELETE` on the REST surface is the soft abandon (everything
  survives).
- `visibility` — `public | secret`, default **secret**: a quest the party
  has not been offered is DM planning material, and the journal reads
  public quests only.
- `act_id` — **with no foreign key**, the precedent `0002` and `0003` set
  for their `session_id` columns. The store validates it when set through
  the API; `dangling_reference` sweeps whatever bypassed it.

`quest_entities` (`quest_id, entity_id, role`) ties a quest into the graph:
one join table rather than five nullable columns, because a quest touches
many entities and "who else is in this" is the question the graph exists to
answer. Roles: `giver | subject | obstacle | reward | site`. The same entity
may hold two roles; the pair `(quest, entity, role)` is unique.

`quest_state_facts` (`quest_id, state_key, fact_id, disposition`) ties a
quest to the knowledge layer:

- `requires` — the state presumes the party discovered the fact;
- `reveals` — the state is a clue path to (likely secret) fact, which is
  exactly what `unreachable_secret` scores.

There is deliberately **no REST write surface** for this table in stage 1:
hand-authored campaigns and the generators (MAD-371) are the writers, and
`dangling_reference` owns whatever they get wrong.

## Editing a machine may not orphan history

`PATCH /api/campaigns/{id}/quests/{qid}` refuses to delete a state or an
edge that a recorded `quest_transitions` row already used — and the error
names the transition that blocks it. The alternative is a quest whose own
history is permanently invalid under `quest_transition_invalid`. The state
the quest currently occupies is protected the same way.

Everything else on the PATCH is the ordinary authored columns: `name`,
`summary`, `status`, `visibility`, `act_id` (empty string clears it),
`state_machine`.

## The deterministic checks

New rules live in `internal/campaign/integrity.go` beside
`quest_transition_invalid`, as ordinary `campaign.Finding` values, so they
flow into the flag ledger and `grimoire campaign check`:

| Check | Severity | Fires when |
| --- | --- | --- |
| `quest_state_unreachable` | warning | a declared state has no path from `initial` |
| `quest_dead_end` | warning | a non-terminal state has no outgoing edge |
| `quest_no_ending` | warning | an active quest has no terminal state reachable from where it sits |
| `quest_transition_ungrounded` | warning | a recorded move's edge `requires` a fact the party (the `party` knower or any pc) holds no granting stance on |

`quest_entities` rows pointing at a deleted entity, `quest_state_facts`
rows pointing at a missing fact or an undeclared state, and `act_id`
pointing at a missing act all extend the existing `dangling_reference`
rather than standing up parallel checks. Every rule has a test that fires
it and a test that does not (ADR 8).

## The player-visible journal

`ListQuests` and `GetQuest` are DM-only, and rather than let the player
portal (MAD-319) grow its own read, this issue ships it:
`GET /api/campaigns/{id}/quests/journal` — every scope the campaign
resolves, `knowledge.QuestJournal` underneath.

The journal carries, per public quest: the name, the summary, the status,
the current state's label, and the states already visited — initial state
first, then the moves in the order they happened. It carries **no
unvisited branch (key or label — keys leak), no state `detail`, no edge
`label` or `requires`** — the branch not taken is the campaign's biggest
single spoiler. The projection is built field by field in
`internal/knowledge/journal.go`, not by filtering the DM shape, so a field
added to `Quest` later cannot leak by accident. A test in the shape of
`leak_test.go` asserts all of it.

## The board

The campaign view's side column gains the quest board. The DM sees every
quest's machine drawn with the same hand-rolled deterministic SVG the
neighbourhood map uses — no library, no physics, whole-pixel geometry —
with the current state in gold, endings ringed, edges already taken inked,
and untaken branches ghosted; a move control walks the current state's real
edges, and abandoning is the soft delete. Everyone else sees the journal:
where each public quest stands and the trail of states already visited.
