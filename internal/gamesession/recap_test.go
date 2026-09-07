package gamesession

// The fights recap's tests (MAD-426): a session whose log holds combat
// events exports a "The fights" section — the journal's own words
// grouped by round, the standing the rows recorded — and a session
// with no fights carries no empty section.

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// seedFight plants one battle under the session: a combats row, a
// small order, and a journal the recap renders. The payloads are the
// tracker's own shapes.
func seedFight(t *testing.T, s *Store, campaignID, sessionID, combatID string) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO combats
		(id, campaign_id, session_id, name, status, round, turn_index, actor, started_at, ended_at, created_at, updated_at)
		VALUES (?, ?, ?, 'The Ambush at Cold Ford', 'ended', 2, 1, 'keeper', 1000000, 2000000, 1000000, 2000000)`,
		combatID, campaignID, sessionID); err != nil {
		t.Fatalf("insert combat: %v", err)
	}
	order := []struct {
		id, name string
		hp, max  int
		dead     bool
	}{
		{"c-1", "Velren", 5, 28, false},
		{"c-2", "Goblin A", 0, 7, true},
		{"c-3", "Goblin B", 7, 7, false},
	}
	for i, c := range order {
		if _, err := s.db.Exec(`INSERT INTO combatants
			(id, combat_id, name, side, kind, statblock, initiative, ac, max_hp, hp, downed, stable, dead, position, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'pc', '{}', 22, 15, ?, ?, 0, 0, ?, ?, 1000000, 1000000)`,
			c.id, combatID, c.name, side(i), c.max, c.hp, deadInt(c.dead), i); err != nil {
			t.Fatalf("insert combatant %s: %v", c.name, err)
		}
	}
	rows := []struct {
		seq     int64
		kind    string
		note    string
		payload string
	}{
		{1, "start", "The Ambush at Cold Ford — order: Velren 22, Goblin A 15, Goblin B 12", `{"round":1}`},
		{2, "turn", "Round 1 — Velren's turn", `{"round":1,"turn_index":0,"combatant":"Velren"}`},
		{3, "damage", "Goblin A takes 7 — Goblin A dies · 0/7 hp", `{"asked":7,"effective":7,"before":7,"after":0,"died":true}`},
		{4, "turn", "Round 1 — Goblin B's turn", `{"round":1,"turn_index":2,"combatant":"Goblin B"}`},
		{5, "damage", "Velren takes 23 fire — Velren goes down · 5/28 hp", `{"asked":23,"effective":23,"before":28,"after":5,"went_down":true}`},
		{6, "turn", "Round 2 — Velren's turn", `{"round":2,"turn_index":0,"new_round":2,"combatant":"Velren"}`},
		{7, "end", "The Ambush at Cold Ford ends after 2 rounds — 1 of the party standing, 1 of the foe", `{"round":2,"alive":{"party":2,"foe":1}}`},
	}
	for _, r := range rows {
		if _, err := s.db.Exec(`INSERT INTO combat_log
			(id, combat_id, seq, kind, amount, note, payload, actor, created_at)
			VALUES (?, ?, ?, ?, 0, ?, ?, 'keeper', 1000000)`,
			combatID+"-"+fmt.Sprint(r.seq), combatID, r.seq, r.kind, r.note, r.payload); err != nil {
			t.Fatalf("insert journal row %d: %v", r.seq, err)
		}
	}
	for _, note := range []string{
		"The Ambush at Cold Ford — order: Velren 22, Goblin A 15, Goblin B 12",
		"Round 1 — Velren's turn",
		"Goblin A takes 7 — Goblin A dies · 0/7 hp",
		"Round 1 — Goblin B's turn",
		"Velren takes 23 fire — Velren goes down · 5/28 hp",
		"Round 2 — Velren's turn",
		"The Ambush at Cold Ford ends after 2 rounds — 1 of the party standing, 1 of the foe",
	} {
		addEvent(t, s, sessionID, EventCombat, note, "", map[string]any{"combat_id": combatID, "round": 1})
	}
}

func side(i int) string {
	if i == 0 {
		return "party"
	}
	return "foe"
}

func deadInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestExportCarriesTheFightsRecap(t *testing.T) {
	s, cid := seeded(t)
	ctx := context.Background()
	ses := addSession(t, s, cid, "The Ambush")
	addEvent(t, s, ses.ID, EventNote, "the plan is set", "", nil)
	seedFight(t, s, cid, ses.ID, "fight-1")

	md, err := s.ExportMarkdown(ctx, ses.ID)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	for _, want := range []string{
		"## The fights",
		"### The Ambush at Cold Ford — ended after 2 rounds",
		"**Round 1**",
		"**Round 2**",
		"- Goblin A takes 7 — Goblin A dies · 0/7 hp",
		"Where they stood: **Velren** 5/28 hp · **Goblin A** dead · **Goblin B** 7/7 hp",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("export missing %q:\n%s", want, md)
		}
	}
	// The section rides beside the narrative log, after it.
	if !strings.Contains(md, "## Log") || strings.Index(md, "## The fights") < strings.Index(md, "## Log") {
		t.Fatalf("the fights section is not beside the log:\n%s", md)
	}
}

func TestExportWithoutFightsCarriesNoSection(t *testing.T) {
	s, cid := seeded(t)
	ctx := context.Background()
	ses := addSession(t, s, cid, "A quiet sitting")
	addEvent(t, s, ses.ID, EventNote, "nothing fought tonight", "", nil)

	md, err := s.ExportMarkdown(ctx, ses.ID)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if strings.Contains(md, "The fights") {
		t.Fatalf("a fightless session exported a fights section:\n%s", md)
	}
}
