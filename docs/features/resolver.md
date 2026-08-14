![Resolver](../assets/sprites/mirror.png){: style="float:right; margin-left:1rem" align=right }

# Interaction Resolver

Resolve mode (the crystal-ball toggle in the top bar, **Magic only**) answers the questions a rules lookup can't: *given this board and this sequence of plays, what exactly happens, and in what order?*

It is built for Commander, where a single board wipe can stack a dozen dies-triggers across two players.

## How you ask

You state the board and the sequence in a compact, line-oriented form:

```
Board                         Sequence
You: Blood Artist             1. Opp casts Wrath of God
You: Zulaport Cutthroat
You: Doomed Traveler
```

- Each permanent is `[controller]: <card> [# tapped | +1/+1 counters: N]`
- Each sequence step is `[N.] <action>` — the actor can live in the prose ("Opp casts…")

An optional free-form note adds anything the two boxes can't express.

## What it grounds in

The resolver looks up every named card on Scryfall, then grounds in the full interaction chapters:

- **117** — priority and the stack
- **603** — triggered abilities and APNAP ordering
- **613** — interaction of continuous effects / layers
- **616** — replacement and prevention effects

plus any keyword abilities and state-based actions the specific cards pull in. It then streams a **numbered, cited walkthrough** — each step of the resolution names the rule that governs it.

!!! warning "An assistant, not a Comprehensive-Rules oracle"

    The resolver is LLM-assisted: it reasons over the provided text and cites the rule at each step, but the UI is honest that it is an assistant rather than an engine — verify anything match-deciding against the rule it cites.

A resolve is stateless and not saved; the Q&A chat is where saved conversations live. The endpoint (`POST /api/resolve`) streams its walkthrough over SSE — see [HTTP API](../deployment/api.md).
