Landed: the faction dossier and faction plans (MAD-366), fully deterministic — not one model call.

**PR:** https://github.com/madeofpendletonwool/grimoire/pull/42 (branch `agent/lead-software-engineer-server/01a0478e`, commit `2f9c5d4`)

What shipped:

- **Migration `0017`** — the issue claimed `0015`, but `0014`/`0015`/`0016` (mtg_core, command_transcript, campaign_clock) landed first, so this takes the next genuinely free number per the issue's own rebase rule. Tables: `faction_plans`, `faction_plan_steps`, `faction_plan_transitions`.
- **`internal/faction`** — the pure progression model. `Advance(plan, days, mods)`: gain is `rate × days` plus each modifier's signed term (`rate × days × (factor−1)`), progress pays the active step's cost with **carry** (one 30-day advance and thirty 1-day advances land identically — test-pinned), and a plan never crosses an edge its machine doesn't declare — it halts with the work banked instead.
- **The dossier is a read, not a write** — territory/leaders/members/allies/enemies/puppets are categorized live from graph edges (`campaign.FactionEdgesOf`); the payload holds only the authored interior (`FactionAgent`, NPC-style decode, public/private split). Adding an `owns` edge changes territory with **no write to the faction entity** — asserted at store and API level.
- **Player scope** — the dossier serves public face, reputation, aware edges and aware facts, and **no plan, no `PrivateTruth`, no secret fact**: the player read goes through a new whitelisted `knowledge.PlayerView.FactionFacade` (returns exactly two payload fields, structurally incapable of more), asserted on the raw response body in the leak test's shape. The knowledge leak test itself still passes over the new method.
- **Interference is a rule, not a mood** — steps carry authored preconditions (entity live / edge exists / fact holds / enemy plan not yet at a named state) each with an `if_broken` reaction (a signed factor + reason). The store derives the modifier set from live graph state; every advance records its arithmetic in the transition ledger, so "why is the Cult at 62%?" has a named-terms answer.
- **Integrity** — `plan_illegal_state` + `plan_without_faction` (errors), `plan_stalled` (warn, 30 in-world days), `faction_no_antagonist` (info).
- **Surfaces** — REST (`GET .../factions`, scope-filtered `.../factions/{eid}`, DM-only plan CRUD + transition), CLI `grimoire campaign plans <id>`, and the faction sheet page in the UI: dossier, live graph position, plan progress bars, step checklists with live precondition chips, and buttons for exactly the moves the machine declares.

`go test -race -count=1 ./...` green across every package. CI on the PR is running.

Out of scope as written: the simulation tick (advancing plans as time passes) is the next stage; generated faction content belongs to MAD-361's skeleton generator.
