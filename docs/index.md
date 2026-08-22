<div class="gr-hero">
  <img class="gr-sigil" src="assets/sprites/spellbook@3x.png" alt="" width="96" height="96">
  <p class="gr-title">Grimoire</p>
  <p class="gr-sub">A self-hosted sage for <em>Magic: The Gathering</em> and D&amp;D 5e — grounded in the real rules, real card text, official rulings and real SRD entries, every source cited.</p>
  <p class="gr-actions">
    <a href="deployment/quickstart/" class="md-button">Deploy it</a>
    <a href="features/chat/" class="md-button">Take the tour</a>
  </p>
</div>

Grimoire is a chat-first **rules reference** you run for yourself. Ask a question, get an answer streamed token by token with every source it consulted cited beneath — the Comprehensive Rule it applied, the oracle text of the card it checked, the official ruling that decides it. Full-text search and card lookup sit a keystroke away beside the conversation.

It ships as a **single Go binary** in a tiny Docker container, with no hard dependencies (pure-Go SQLite + FTS5, no CGO):

```bash
cp .env.example .env          # fill in ANTHROPIC_API_KEY to enable the chat
docker compose up --build     # the index builds itself on first start
```

Open <http://localhost:8080>, claim the keeper account, and you are in. Search works immediately; the Q&A chat is enabled once a key is set. The full walk-through is in [Quick Start](deployment/quickstart.md).

---

## What's inside

<div class="grid cards" markdown>

-   ![Chat](assets/sprites/staff.png){: loading=lazy }

    **[Q&A Chat](features/chat.md)**

    A conversation with a sage that answers grounded in retrieved rules — and cites every source it consulted.

-   ![Search](assets/sprites/magnifier.png){: loading=lazy }

    **[Search & Command Palette](features/search.md)**

    Full-text search over both rule sets and Scryfall, a keystroke away, without leaving the chat.

-   ![Card lookup](assets/sprites/card.png){: loading=lazy }

    **[Card & Entity Lookup](features/lookup.md)**

    Real oracle text and official rulings from Scryfall; SRD entities from Open5e. No invented card effects.

-   ![Resolver](assets/sprites/mirror.png){: loading=lazy }

    **[Interaction Resolver](features/resolver.md)**

    State a board and a sequence of plays; get the stack, APNAP ordering and layers walked step by cited step.

-   ![Study](assets/sprites/hourglass.png){: loading=lazy }

    **[Study Mode](features/study.md)**

    The corpus as a spaced-repetition deck — MTG keywords and D&D conditions, scheduled by SM-2.

-   ![Encounter builder](assets/sprites/swords.png){: loading=lazy }

    **[Encounter Builder](features/encounter-builder.md)**

    Describe a mood, or nothing at all, and get a whole D&D encounter back — real SRD statblocks, on budget.

-   ![The tome](assets/sprites/bookGold.png){: loading=lazy }

    **[The Tome & Themes](features/tome.md)**

    An interface assembled from real pixel art: nine-sliced parchment, sprite icons, four measured themes.

</div>

## For keepers

- **[Quick Start](deployment/quickstart.md)** — Docker, bare metal, volumes, rebuilding the index.
- **[Configuration](deployment/configuration.md)** — every environment variable, with defaults.
- **[Accounts & Invites](deployment/accounts.md)** — the keeper, single-use invite links, the security model.
- **[HTTP API](deployment/api.md)** — the JSON + SSE API the UI itself speaks.

## For contributors

- **[Getting Started](development/index.md)** — build, vet, test, and how the front end is embedded.
- **[Architecture](development/architecture.md)** — the package tree, the one SQLite file, the data sources.
- **[Design System](DESIGN.md)** — the token system, both icon systems, the nine-slice contract, theme derivation.

!!! tip "This site is skinned with the app's own art"

    The parchment, the stone, the gold, the sprites and the fonts on these pages are the same derived assets the app ships — see [The Tome & Themes](features/tome.md) for how the whole system works.
