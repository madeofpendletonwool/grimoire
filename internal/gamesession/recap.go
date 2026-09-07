// The fights section of the session export (MAD-426): "how the fight
// actually went", generated from the log. The narrative recap is the
// event log rendered whole (export.go); this is the mechanical recap
// beside it — each fight the session's combat events name, replayed as
// prose from the combat journal (the same rows the replay engine
// folds), with the standing the rows recorded when it ended.

package gamesession

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// fightHead is what the combats row lends the section: the name, where
// the battle stands, how long it ran.
type fightHead struct {
	Name      string
	Status    string
	Round     int
	StartedMS int64
	EndedMS   int64
}

// fightRow is one combat_log row, in the shape the recap reads it.
type fightRow struct {
	Seq     int64
	Kind    string
	Note    string
	Payload map[string]any
}

// fightsMarkdown renders the "## The fights" section for one session:
// every combat its kind 'combat' events name, in first-appearance
// order, each as the journal's own words grouped by round with the
// final standing beside them. A session with no fights gets "" — the
// export carries no empty section.
func (s *Store) fightsMarkdown(ctx context.Context, sessionID string, events []Event) (string, error) {
	var ids []string
	seen := map[string]bool{}
	for _, ev := range events {
		if ev.Kind != EventCombat {
			continue
		}
		id, _ := ev.Payload["combat_id"].(string)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return "", nil
	}

	var b strings.Builder
	b.WriteString("## The fights\n\n")
	for _, id := range ids {
		head, ok, err := s.fightHead(ctx, id)
		if err != nil {
			return "", err
		}
		if !ok {
			continue
		}
		rows, err := s.fightRows(ctx, id)
		if err != nil {
			return "", err
		}
		standing, err := s.fightStanding(ctx, id)
		if err != nil {
			return "", err
		}

		name := head.Name
		if name == "" {
			name = "The fight"
		}
		status := head.Status
		if status == StatusEndedLabel {
			status = fmt.Sprintf("ended after %d round%s", head.Round, pluralMS(head.Round))
		}
		fmt.Fprintf(&b, "### %s — %s\n\n", name, status)
		if head.StartedMS > 0 {
			window := msClock(head.StartedMS)
			if head.EndedMS > 0 {
				window += "–" + msClock(head.EndedMS)
			}
			fmt.Fprintf(&b, "_fought %s_\n\n", window)
		}

		round := 0
		for _, row := range rows {
			next := rowRound(row, round)
			if next != round {
				round = next
				fmt.Fprintf(&b, "**Round %d**\n\n", round)
			}
			if note := strings.TrimSpace(row.Note); note != "" {
				fmt.Fprintf(&b, "- %s\n", note)
			}
		}
		if round > 0 {
			b.WriteString("\n")
		}
		if standing != "" {
			fmt.Fprintf(&b, "Where they stood: %s\n\n", standing)
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n", nil
}

// fightHead reads the combats row for one fight.
func (s *Store) fightHead(ctx context.Context, combatID string) (fightHead, bool, error) {
	var h fightHead
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(name, ''), status, round, started_at, COALESCE(ended_at, 0)
		   FROM combats WHERE id = ?`, combatID).
		Scan(&h.Name, &h.Status, &h.Round, &h.StartedMS, &h.EndedMS)
	if err == sql.ErrNoRows {
		return h, false, nil
	}
	if err != nil {
		return h, false, fmt.Errorf("fight head: %w", err)
	}
	return h, true, nil
}

// fightRows reads one fight's journal in play order.
func (s *Store) fightRows(ctx context.Context, combatID string) ([]fightRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, kind, COALESCE(note, ''), COALESCE(payload, '{}')
		   FROM combat_log WHERE combat_id = ? ORDER BY seq`, combatID)
	if err != nil {
		return nil, fmt.Errorf("fight rows: %w", err)
	}
	defer rows.Close()
	var out []fightRow
	for rows.Next() {
		var r fightRow
		var payload string
		if err := rows.Scan(&r.Seq, &r.Kind, &r.Note, &payload); err != nil {
			return nil, err
		}
		r.Payload = map[string]any{}
		if payload != "" {
			_ = json.Unmarshal([]byte(payload), &r.Payload)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// fightStanding renders the end-state line: every combatant, where the
// rows say they finished — the same numbers the replay's checksum
// asserts.
func (s *Store) fightStanding(ctx context.Context, combatID string) (string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, hp, max_hp, hp_reduction, temp_hp, downed, stable, dead
		   FROM combatants WHERE combat_id = ? ORDER BY position`, combatID)
	if err != nil {
		return "", fmt.Errorf("fight standing: %w", err)
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var name string
		var hp, max, reduction, temp int
		var downed, stable, dead bool
		if err := rows.Scan(&name, &hp, &max, &reduction, &temp, &downed, &stable, &dead); err != nil {
			return "", err
		}
		effective := max - reduction
		if effective < 0 {
			effective = 0
		}
		switch {
		case dead:
			parts = append(parts, fmt.Sprintf("**%s** dead", name))
		case downed && stable:
			parts = append(parts, fmt.Sprintf("**%s** stable at 0 hp", name))
		case downed:
			parts = append(parts, fmt.Sprintf("**%s** down at 0 hp", name))
		default:
			cell := fmt.Sprintf("**%s** %d/%d hp", name, hp, effective)
			if temp > 0 {
				cell += fmt.Sprintf(" (+%d temp)", temp)
			}
			parts = append(parts, cell)
		}
	}
	return strings.Join(parts, " · "), rows.Err()
}

// rowRound walks one journal row's round: turn rows carry it (and a
// wrap's new_round), start opens at one, end closes on its own.
func rowRound(row fightRow, current int) int {
	switch row.Kind {
	case "start":
		if r := payloadInt(row.Payload, "round"); r > 0 {
			return r
		}
	case "turn":
		if r := payloadInt(row.Payload, "new_round"); r > 0 {
			return r
		}
		if r := payloadInt(row.Payload, "round"); r > 0 {
			return r
		}
	case "end":
		if r := payloadInt(row.Payload, "round"); r > 0 {
			return r
		}
	}
	return current
}

// payloadInt reads a JSON-decoded number the way it decodes.
func payloadInt(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

// StatusEndedLabel is the combats row's own spelling for a finished
// fight — kept local so the recap renders the tracker's vocabulary
// without importing the tracker (which imports this package).
const StatusEndedLabel = "ended"

func pluralMS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// msClock renders a stored epoch-milli stamp as the log's clock style.
func msClock(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("15:04 MST")
}
