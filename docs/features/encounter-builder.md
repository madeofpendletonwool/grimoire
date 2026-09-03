![Encounter](../assets/sprites/swords.png){: style="float:right; margin-left:1rem" align=right }

# Encounter Builder

The encounter builder (the crossed-swords button in the sidebar) is a D&D 5e combat prep surface. You tell it about your table — and, if you feel like it, a mood — and it hands back a whole encounter: a name, a roster of real SRD statblocks, terrain, tactics, a twist and scaling advice, with the DMG difficulty recomputed underneath.

The point is that you should not have to arrive knowing which monsters you want. "Something creepy in the swamp" is a complete brief. So is nothing at all.

## The brief

Three inputs, all with usable defaults:

- **The table** — how many players and what level. Two number boxes cover most tables; *Mixed levels* opens a chip editor for the ones they don't.
- **The difficulty** — Easy, Medium, Hard or Deadly, as the 2014 DMG defines them. Under the chips, a line says what that band actually buys you: the adjusted XP to spend, the biggest single monster that fits, and a couple of group shapes at that budget.
- **The idea** — free text, optional. There is a row of starters under the box for when you have none, and **Surprise me** builds without an idea at all and lets the sage invent the concept.

Press **Build the encounter** and the design streams onto the page.

### The objective

Beside the difficulty chips is a second row: what the fight is *about*. **Defeat** — kill the things — is selected by default, so a DM who wants today's behaviour changes nothing. The rest are different fights that happen to contain monsters:

| Objective | The fight | Its parameter |
|---|---|---|
| **Defeat** | The last monster drops, or the party does | — |
| **Survive** | Hold N rounds against reinforcing waves | Rounds (6) |
| **Reach** | Cross the board; the monsters are the obstacle | Rounds before the pursuit closes (5) |
| **Protect** | Something specific must still be standing at the end | What, and how long it can take a hit (5) |
| **Stop** | A clock the enemy is advancing; break it in time | Rounds until it completes (4) |
| **Retrieve** | Take the thing and get out with it | What, and the rounds to get clear (5) |
| **Escape** | Get out; standing to fight is the failure state | Rounds before the way closes (5) |

The objective is not a label — it changes the arithmetic, and says so. Under the budget readout, every adjustment it applied appears with its reason: survive arrives in waves and each wave is priced at its own multiplier (the DMG's table assumes every monster is on the board at once, and the verdict checks the total across all of them); reach concentrates the budget into fewer, faster monsters; protect buys a bigger board because the monsters split their fire; escape is priced above the party deliberately rather than by accident. The same objective, party and band always produce the same numbers.

Picking a non-defeat objective reveals its one or two parameters and grows the encounter a short **"How this ends"** block: success, failure, and the clock if there is one — the round budget the combat tracker (MAD-318) reads. The block is server text, not the model's.

### Terrain and hazards

A `reach` objective on a flat empty room is a footrace, so the battlefield is generated *with* the objective, not decorated on afterwards. The server picks from a declared vocabulary — cover, elevation, difficult ground, chokepoints, concealment, water, darkness — and each feature declares what it does mechanically (`difficult_ground` costs 2 feet of movement per foot; `chokepoint` is one creature wide), so it is a structured value the app can reason over, never prose to re-read.

Where the story calls for one, the fight carries a **hazard**: a save DC, a damage expression, a trigger and an area, priced the way the DMG prices traps — the DC and damage come straight from the book's tier tables for the party's level and the requested band, and a hazard moves the aim a step up so the roster doesn't also carry the band. The model's job is naming and describing what it was handed; every number on the battlefield is the server's.

### From a campaign

If you run a campaign, the disclosure under the party boxes lists the ones you keep. Picking one prefills the boxes from the campaign's declared [party block](../campaign/encounters.md) — every pc that carries a level, in name order — with a *from your campaign — 4 characters, level 5* line above them. The line's **edit** button (or your first edit of the numbers) takes the table back: the campaign is a prefill, never a requirement. A campaign that declares no levels leaves the boxes alone.

Saving with a campaign picked stores the encounter against that campaign — the one record a planned fight has, so the continuity engine can check it: an encounter whose roster names a monster with no bestiary entry raises `stat_block_unresolved` in `canon check` whether it arrived as session prep or as a saved record. Without a campaign, everything works exactly as it always did.

## What the server decides, and what the model decides

The split matters, because it is why the result lands on the difficulty you asked for instead of near it.

**The server** computes the budget from the DMG's threshold tables, works out the shapes that budget can take (one big monster, a pair, a pack, a gang, a horde — each with its own encounter multiplier and therefore its own raw-XP allowance), and assembles a shortlist of real SRD creatures inside those challenge-rating windows. It reads your idea for deterministic signal first: named creature types gate the shortlist, and words like *underwater*, *cave*, *ambush* or *boss* map onto qualities derived from the statblocks themselves.

**The model** picks from that shortlist and writes the encounter around its choices. It cannot invent a monster, rename one, or change a statblock number — every name it writes is resolved back against the local bestiary before you see it, and anything that does not resolve is reported ("Not in the SRD, so left out: …") rather than rendered as real. XP is always re-derived from the Monster Manual's challenge-rating table, never taken from the model's word for it.

The verdict panel then recomputes the difficulty from the party and the finished roster, so the number under the encounter is the server's arithmetic, not the model's claim.

## Revising

The **Revise** box argues with the result: *fewer goblins and one nastier thing*, *move it underwater*, *make it hard*. The whole encounter is rewritten with the instruction applied, still inside the budget, still from verified statblocks. Your original idea stays in force — a revision adjusts the encounter, it does not replace the brief.

## The roster

Each line in the roster is a control and a disclosure at once. The `−` `+` `✕` buttons adjust counts and the verdict follows live; clicking the name expands the creature's **full SRD statblock** in place — armour class, hit points, speeds, senses, resistances and immunities, every trait and every action, including reactions and legendary actions. Under the statblock is a row of derived tags (`flying`, `spellcaster`, `ambusher`, `damage-immune`, …) — the same vocabulary the shortlist was filtered with.

**Add a monster by hand** is still there, under the disclosure at the bottom of the brief column: search the SRD by name and add whatever you want. The designer proposes; you decide.

## The difficulty gauge

The band word, the adjusted XP with the multiplier that produced it, and a bar that runs to half again the Deadly threshold — so a deadly encounter reads as *how far* past the line rather than pinning at full. The four DMG thresholds sit on the bar as tick marks and below it as a row with the margin to each: `Hard 3,000 · 2,400 over`.

The maths is the 2014 DMG's, computed on the server: per-character XP thresholds by level, the Encounter Multipliers table, and the party-size adjustment (fewer than three characters moves one rung up, six or more moves one rung down).

## The local bestiary

The whole SRD bestiary is mirrored into the app's SQLite file on first start — a few hundred creatures with their statblocks — and served from memory after that. This is what makes the designer possible: choosing *what would fit here* needs the whole shelf at once, not one lookup at a time. It also means monster search is instant and works with no route to the internet.

The mirror is refreshed if it is missing or more than a month old, in the background, so a cold start still serves immediately. An install that has never mirrored pays the fetch on its first design request.

## Saving and taking it away

Name it and **Save**; the roster, the party, the objective and its terrain, and the whole write-up are stored with your account (and, when a campaign is picked, scoped to that campaign — see [From a campaign](#from-a-campaign)). The saved-encounter picker at the top of the surface loads them back with the verdict recomputed and the "How this ends" block intact. **Copy** puts the entire encounter on the clipboard as plain text, because prep tends to end up in someone's own notes app. **Ask the sage about this** drops the encounter into the chat as a question, grounded in the indexed DMG.

## Requirements

Designing needs the model — set `ANTHROPIC_API_KEY` (see [Configuration](../deployment/configuration.md)). Without it, the rest of the surface still works: manual monster search, the roster, the budget readout and the difficulty verdict are all server-side arithmetic over the local bestiary and need no LLM at all.
