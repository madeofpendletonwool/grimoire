# The combat tracker: the whole battle as one state machine

Every combatant is the same kind of thing. PCs arrive from their sheets —
the initiative bonus is the sheet's own dex arithmetic, AC and max HP are
the definition the sheet holds. Monsters and companions arrive from the
statblock machinery — the bestiary mirror or the campaign's homebrew —
and the ranger's wolf is a wolf: the same statblock snapshot, fighting on
the party's side. One merged initiative order, ties rolled through the
[dice engine](dice.md), over a persistent encounter state: round, turn,
whose action it is. Stage 5 of the mechanical layer (MAD-422).

The battle survives reload exactly where the table left it, because the
position is persisted, not derived from a chat scroll. And every state
change flows through an append-only journal — one row per mechanical
event — mirrored into the [session log](sessions.md) as kind `combat`
events, the same write-your-own-table-then-append-a-session-event pattern
the dice feed established. Combat is the densest stream of mechanical
events in the game; the journal is what a later replay will read.

## The state machine

- **Start** resolves the lineup and rolls initiative for everyone — each
  roll a public `initiative` roll in the shared feed, so every number on
  a combatant row has provenance back to a stored roll. Ties re-roll the
  way the table does, through the same engine. The turn counter parks
  *before* the first turn; the first `next` begins the opening
  combatant's turn with its prompts. One active combat per campaign — a
  second start is a 409.
- **Snapshots freeze at start.** The sheet and the statblock are
  definitions — slow-changing, shared, correctable; the combatant row is
  tonight's truth: AC, max HP, resistances, the legendary budget
  (derived: the statblock's own legendary-action count), the recharge
  abilities, the lair flag. A sheet edit mid-battle changes nothing
  about the battle already fought.
- **On-turn automation is mechanical and therefore here.** The outgoing
  turn ends (legendary budget back). The round wraps when the order does:
  durations wear a round on **both** engines — the [effect
  engine](effects.md) for entity-backed rows, the combatant's own
  condition list for statblock-backed ones — and the lair reminder fires
  on the count-20 crossing, losing ties exactly as the rule says. The
  incoming turn starts: the reaction comes back, and the prompts surface
  (a monster's `recharge 5-6` abilities; a downed PC's death save).
- **The hit-point grammar is the 2014 rules.** Temp HP drinks first;
  resistance halves (rounding down), immunity zeroes, vulnerability
  doubles, matched against the frozen snapshot's lists. A PC dropped to
  0 is downed and dying — three death-save successes stabilize, three
  failures die, a natural 20 wakes at 1 HP, damage while at 0 is an
  auto-failed save — while monsters and companions die at 0. Damage
  carried past 0 equal to the max is death outright, whatever the side.
  The vampire's bite (`reduce_max`) wears the max HP by the damage that
  landed; a ceiling fallen to zero is death.
- **Concentration prompts ride the damage.** A hit on a concentrator
  surfaces the check — the effect's name and the 2014 DC (10, or half
  the damage taken) — resolved from the effect engine's live links at
  read time.
- **Conditions are the effect engine's rows for PCs** (they persist past
  the battle and ride both clocks; apply them through the effects API)
  **and combat-local state for statblock combatants** — the same
  fifteen-condition vocabulary either way, because a goblin's poison
  does not outlive the goblin.

## The surface

```
GET  /api/campaigns/{id}/combat                                          the active battle (DM)
GET  /api/campaigns/{id}/combats                                         the campaign's battles (DM)
POST /api/campaigns/{id}/combat                                          start: {pcs[], monsters[{name,count}], companions[{statblock,name?,count?}]} (DM)
GET  /api/campaigns/{id}/combats/{cid}                                   one battle: combat, order, journal (DM)
POST /api/campaigns/{id}/combats/{cid}/next                              advance one turn (DM)
POST /api/campaigns/{id}/combats/{cid}/end                               end {reason} (DM)
POST /api/campaigns/{id}/combats/{cid}/combatants/{ctid}/damage          {amount, damage_type?, note?, reduce_max?} (DM)
POST /api/campaigns/{id}/combats/{cid}/combatants/{ctid}/heal            {amount, note?} (DM)
POST /api/campaigns/{id}/combats/{cid}/combatants/{ctid}/temp-hp         {amount, note?} (DM)
POST /api/campaigns/{id}/combats/{cid}/combatants/{ctid}/death-save      {result: success|fail|crit-success|crit-fail} (DM)
POST /api/campaigns/{id}/combats/{cid}/combatants/{ctid}/reaction        {spent?} (DM)
POST /api/campaigns/{id}/combats/{cid}/combatants/{ctid}/legendary       {ability, cost?} (DM)
POST /api/campaigns/{id}/combats/{cid}/combatants/{ctid}/conditions      {name, rounds} (statblock combatants) (DM)
POST /api/campaigns/{id}/combats/{cid}/combatants/{ctid}/conditions/{condid}/end   end one (DM)
POST /api/campaigns/{id}/combats/{cid}/combatants/{ctid}/reveal          {mode: off|hp|word} — the table screen's exposure, foes only (DM)
```

The tracker is the DM's screen: every route is the DM perspective. The
table's shared view is a later surface's question; the journal and the
session log are where players read the battle today — the session export
renders the fight's events like every other kind, initiative rolls
included. Monsters resolve by name through the campaign's shelf —
homebrew first, the SRD mirror second — and letter themselves the way
the table does: two goblins are Goblin A and Goblin B.

## Where the code lives

- `internal/combat/combat.go` — the pure state machine: the snapshot
  derivations, the hit-point grammar, death saves, the order and the
  count-20 crossing. No database, no clock, no network.
- `internal/combat/store.go` — the rows: start through the dice engine,
  the turn engine with both duration engines wired, the journal and the
  session mirror.
- Migration `0033_combat_tracker.sql` — `combats` (one active per
  campaign), `combatants`, `combat_log`, and the `combat` session-event
  kind (the same twelve-step CHECK rebuild 0031 used).
