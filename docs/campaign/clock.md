# The campaign clock: calendar, travel time, and the schedule

The clock answers the questions every long campaign eventually asks: *what
date is it?*, *how long does the journey take?*, *what is due to happen?* —
all deterministically, with no model configured and no rows spent on weather.

Three pieces make it up:

- **`internal/clock`** — a pure package. Calendar arithmetic, schedule
  expansion, and weather derivation. No database, no wall clock, no network
  (a test enforces the import ban). Everything is arithmetic over arguments.
- **`internal/campaign/clock.go`** — the rows: `campaign_calendars`,
  `clock_advances`, `scheduled_events` (migration `0016`).
- **The REST surface** — `/api/campaigns/{id}/calendar`, `.../clock`,
  `.../schedule`, `.../travel`.

## The calendar

A campaign's time is a plain day counter; the calendar is what makes the
number *mean* something. A `clock.Calendar` names the months, their lengths,
the weekdays, the seasons (bands of the year, by day-of-year), an optional
leap rule, and an epoch label.

Day **0** is the campaign's start: the first day of year 1, month 1. Negative
days exist and land in year 0 and below. `DateOf(day)` and `DayOf(date)` are
exact inverses — a round-trip property test pins it over multi-year ranges,
for the default calendar and a deliberately lopsided custom one.

No setting ships. Harptos, Golarion and the rest are product IP, and worse,
they would be *our* guesses at someone's table. Every campaign starts on the
generic twelve-month, 360-day **Common Reckoning** (`Firstmonth` through
`Twelfthmonth`, a seven-day week, four 90-day seasons, epoch `CR`), and the
calendar editor is the feature: `GET/PUT /api/campaigns/{id}/calendar` stores
whatever the DM enters. Players may read the calendar — it is how any date is
legible to them — but only the DM may change it.

`Format` renders a day as `"Thirdday, 15 Thirdmonth 12 CR"`; `Parse` accepts
that form and the compact `"12-3-15"`.

## The clock is a ledger

`campaigns.clock` is not a settable integer. It is the cached head of
`clock_advances`: every movement of time — travel, downtime, a session wrap,
a tick, a plain manual move — appends a row (`from_day`, `to_day`, `reason`,
`note`, `session_id`, `created_by`) and updates the head **in one
transaction**, so the two can never disagree (a test advances randomly,
backwards included, and checks).

A `PATCH /api/campaigns/{id}` that changes `clock` keeps working — it records
a `manual` advance. A backwards move is legal (a DM fixing a typo) and is
recorded exactly like a forward one, because faction plans, schedule firing
and downtime all need to answer *"what happened between day A and day B"*,
and a clock that jumps silently makes that unanswerable.

`POST .../clock/advance` takes `{"to": N}` or `{"by": N}`, a reason from
`travel | downtime | session | tick | manual`, an optional note and session.
`GET .../clock` (any member) returns the current day, its formatted date,
weekday, season, and weather; the DM additionally gets the recent ledger.
Query params: `strip=N` (a labelled day strip starting today, for the
calendar strip UI), `due=N` (schedule occurrences due in the next N days),
`location=<entity>` (draw the weather for that location's climate tag).

## The schedule

`scheduled_events` rows are festivals, rituals, caravan arrivals — and NPC
routines: **an NPC's routine is a schedule entry with `entity_id` set**, not
a second table with its own recurrence, firing, and "did it happen".

One deterministic function answers *"what is due in `[from, to)`"*:
`clock.Due(cal, entries, from, to)`. Recurrence expands there, once, nowhere
else:

- `none` — the entry's own day.
- `yearly` — the same month/day each year; years where that date does not
  exist (a leap-day festival in a common year) are skipped, not moved.
- `monthly` — the same day-of-month, skipping months too short for it.
- `every_n_days:N` — `Day, Day+N, Day+2N, …`, starting at the entry's day.

`status` is `pending | fired | cancelled | missed` — recording what happened
is a DM decision, never automatic. `visibility` mirrors facts: **secret
entries are DM-only** and absent — not blanked — from every player response
([ADR 8](../decisions.md#adr-8--the-testing-policy-for-the-campaign-work);
a handler test asserts the body never carries one).

REST: `GET/POST .../schedule`, `PATCH/DELETE .../schedule/{sid}`. `GET`
accepts `?due=N` for the expanded window.

## Travel time

`POST /api/campaigns/{id}/travel` between two `location` entities. Distance
comes from a route graph on the entities themselves: a location's payload may
carry

```json
"travel": {"routes": [{"to": "<entity-id>", "days": 4, "terrain": "road"}]}
```

and the answer is a shortest path by day cost (routes are undirected — a
road runs both ways). No coordinates, no map, no invented geography. **A pair
with no route is a question, not a guess**: the endpoint answers `400` and
asks for a day count; the DM's `days` in the request is then recorded as the
journey's length (an explicit `days` always wins over a computed route). The
clock advances by the cost with reason `travel`, and the ledger note names
the journey.

## Weather

`clock.Weather(seed, day, season, climate)` — deterministic, unstored. The
seed lives on `campaign_calendars` (default: the campaign id); `climate` is
an optional tag on a location's payload (`desert`, `tropical`, `arctic`;
anything else is temperate) that swaps the condition tables and shifts the
temperature. Same campaign, same day, same answer, forever — zero rows, zero
tokens. **Re-rolling the weather means changing the seed**, which is a
recorded decision on the calendar, not a refresh button.

## Integrity findings

Three checks join the register (`campaign.Finding` values, sorted and stable
like the rest):

| code | severity | meaning |
| --- | --- | --- |
| `event_after_clock` | warning | an event dated past the campaign's current day |
| `missed_schedule` | warning | a `pending` entry whose day is behind the clock |
| `clock_never_advanced` | info | sessions played, clock unmoved |

Info-severity findings are nudges, not decisions: they appear in a check's
finding list but do not enter the `canon_flags` ledger, whose accepted /
dismissed / cleared semantics exist for findings a DM acts on.

## The honest fallback

Everything in this document works with no `ANTHROPIC_API_KEY` configured.
The simulation tick, faction plans and downtime resolution (later issues
under MAD-315) consume `clock.Due` and the advance ledger — the deterministic
foundation they stand on is this issue, and none of it spends a token.
