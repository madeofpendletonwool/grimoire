# The Guide

In-app onboarding: a tool that teaches the app by watching you use it.

The design decision behind it is [ADR 26](../decisions.md) — onboarding is a
tool gated on server state, never a tour of clicks. This page is what it
teaches and why in that order.

## Why a window and not an overlay

A coach mark has to fight a tiling shell. Tools mount on a later tick through a
dynamic import, windows move, a split invalidates every cached rectangle — and
when the tour finishes it is gone, so whatever you half-learned in week one is
unavailable in week six, which is exactly when you finally open Downtime.

The Guide is one registry entry and one module. It opens in a window, sits
beside the tool it is talking about, stays as long as it is useful, and is
reopened from the picker or the leader chord like anything else.

The one thing a window cannot teach is the chrome it is sitting in — the
workspace strip, the tool picker, the window menu. Those get a five-step
overlay (`web/static/js/tour.js`), reachable from the Guide's footer, and they
are the only things that do.

## The gate

**A step is done because the act happened.** Every step carries a
`done(snapshot)` predicate over one read of `GET /api/onboarding/state`.
Nothing counts clicks; nothing remembers a dismissed card.

That rule buys two things. An onboarding that advances on a Next button teaches
the Next button and nothing else. And a DM who founded their campaign a week
before opening the Guide finds that step already behind them, rather than being
asked to pretend.

Steps are judged independently rather than as a chain. The order is the order
we suggest; an encounter built before the Planner was ever opened is still an
encounter, and the checkmark says so.

The snapshot refreshes on mount, on the **Check again** button, and when the
tab becomes visible again after fifteen seconds — never on a timer, and never
on the window manager's change event, which fires on every gutter drag.

## The DM track

Six steps, and the sixth points back at the second: the loop is the lesson.

| Step | Done when |
|---|---|
| **Found the table** | a campaign exists |
| **Decide what's true** | any proposal batch has been decided |
| **Lay out the spine** | an act or a session plan exists |
| **Build an encounter** | a campaign encounter exists |
| **Invite the party** | an invite has been issued, or a member has joined |
| **Run it** | a session exists |

**Decide what's true** is the one that matters most. It exists to teach
[ADR 3](../decisions.md): nothing a generator writes is canon until the DM
accepts it, and proposals are invisible to players — and to the rest of the
app — until they pass that gate. A DM who cannot find the NPCs the skeleton
"made" has not lost them; they have not been decided yet. Without this card
that reads as a bug.

## The player track

Four steps, no prep phase.

| Step | Done when |
|---|---|
| **Take your seat** | the account stands in a campaign |
| **Your character** | a character is bound to the member row |
| **At the table** | the player has rolled |
| **What the party knows** | the player has written a journal entry |

**What the party knows** is the whole track, honestly. It carries both halves
of the epistemics in the words a player needs: the campaign Grimoire answers as
what the party *has learned*, not as what is true — so an answer you know to be
wrong is your character being wrong, not the app failing — and a journal entry
is recorded as a belief its author holds, never as a fact about the world
([ADR 23](../decisions.md)).

**An observer gets a reduced track.** They carry no binding and write no
journal by design ([ADR 22](../decisions.md)), so those two steps are filtered
out rather than left permanently unreachable.

## The fork

An account with no campaigns has never answered the only question that shapes
the whole shell: do you run a table, or are you joining one?

`seatRoleOf` hands such an account the DM seat for want of any evidence — the
right default, and the wrong thing to leave unasked, because a player who
registered a minute ago lands in a cockpit of tools the server will refuse
them. The Guide's first card asks, and records the answer in the `guide.track`
preference.

The answer is **advisory**. Real standing outranks it in both directions:
someone who answered "I'm joining one" and then founded a campaign is offered
the DM track, because the walk they chose can no longer advance.

## Invites, and the one link

A campaign invite is already a single link that does everything. The DM
generates one in the Campaign tool and sends `…/?invite=CODE`:

- a **stranger** following it registers at the gate, and the membership is
  written in the same transaction as the account;
- an **existing account** following it has the code spent by the shell at boot
  against `POST /api/campaigns/join` — the same invite row, the other door.

The second case used to fail silently: a signed-in visitor is served the app
rather than the gate, so nothing ever read the parameter and the DM's link
appeared to do nothing. The shell now claims it, clears it from the address bar
so a reload cannot re-spend a burned code, and reports a refusal in words.

An account that lands with campaign standing and no corpus preference is
switched to D&D. Campaigns are a D&D surface, and the Magic default meant an
invited player's first sight of Grimoire was the wrong game.

## Where the code is

| Path | What |
|---|---|
| `web/static/js/guidevm.js` | the track tables, the gate, the fold — DOM-free, so `node --test` covers all of it |
| `web/static/js/guide.js` | the tool contract, the fork card, painting, "Show me" |
| `web/static/js/tour.js` | the five-step shell-chrome overlay |
| `internal/server/onboarding.go` | the snapshot: counts, scoped once, no step ids |
| `jstest/guidevm.test.js` | the gate's invariants |
| `internal/server/onboarding_test.go` | that a player's snapshot carries none of the DM's prep |

Adding a step is one entry in `guidevm.js`. Adding a *fact* for a step to read
is one count in `onboarding.go`. The two move independently, which is the
point.
