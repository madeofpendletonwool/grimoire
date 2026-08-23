![Sessions](../assets/sprites/hourglass.png){: style="float:right; margin-left:1rem" align=right }

# Sessions

The Sessions surface (the hourglass button in the sidebar) is the campaign's memory of what was actually played: one record per sitting, the raw material attached to it, and a chronological log of what was ruled, asked, noted and discovered at the table.

Everything downstream of a session — the canon engine's extraction, facts, discoveries — cites back into these records through **addressable spans**: a source, two byte offsets, and the quoted text. When Grimoire later claims "the party learned the merchant is a vampire", the span is what makes that claim resolve to actual words someone actually said.

## Campaigns

Sessions belong to a campaign. The picker at the top of the surface lists every campaign you own or play in; `+` starts a new one. The API surface is the same shape: `GET`/`POST /api/campaigns`.

## A session

A session numbers itself (`1`, `2`, …) within its campaign and moves through three statuses: **planned** → **live** → **done**. Start and end record their own timestamps, so closing a session is one button, not a form.

## Sources — verbatim and immutable

Paste a transcript, DM notes, a player journal, a chat log, or upload `.txt` / `.md` / `.srt` / `.vtt`. Subtitle formats are parsed to text with their timing preserved, so a span in a recording-backed transcript can resolve back to a moment in the session.

Three rules the storage enforces:

- **Verbatim.** The content is stored exactly as ingested (BOM and CRLF normalized). Span offsets are byte offsets into that exact string.
- **Immutable.** There is no edit path. A correction is a new source; the record of what was said never silently changes.
- **Checksummed.** Every source carries a sha256 of its content, so later extraction runs can tell whether the input changed.

Selecting text inside an open source shows the span — the byte offsets a downstream fact would cite.

Author attribution is first-class: a player journal and a DM note can contradict each other on purpose, and which one said a thing is part of the record. DM notes and live marks are visible to the DM only; the filter is applied in the query, not after it.

## The log

Five kinds of entry, in play order: **rulings**, **Q&A**, **notes**, **discoveries** and **encounters**. The `+ Discovery` button is the in-play shortcut — one prompt while the table waits, one log entry.

### "You ruled the other way on this"

When you log a ruling or a question, Grimoire full-text-matches it against every earlier **ruling** in the same campaign and surfaces the top hits above the log: *Session 3 — "Does hiding work in dim light?" — ruled yes.* No model involved; it is an index lookup, which is exactly why it can be trusted at the table.

## Export

**Export Markdown** renders the whole session — header, every source verbatim, the full log — as one document for the group wiki or your own archive. The export is DM-only by design: it contains the DM's notes.
