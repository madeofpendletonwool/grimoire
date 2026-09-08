package combat

// Damage attribution (MAD-428): a hit can name where it came from, and
// the journal carries that name flat — the campaign stats fold "damage
// dealt" from it without parsing prose. The summary keeps the table's
// own voice: "Velren takes 8 slashing from Goblin A".

import (
	"context"
	"testing"
)

func TestDamageJournalsItsSource(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	start, err := h.store.Start(ctx, h.campaign, StartInput{
		PCs:      []string{h.velren},
		Monsters: []MonsterLine{{Name: "Goblin", Count: 1}},
	}, "keeper")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	combatID := start.Combat.ID
	velren := byName(t, start.Order, "Velren")
	goblin := byName(t, start.Order, "Goblin")

	// Goblin A hits Velren; the hit names its source.
	if _, err := h.store.Damage(ctx, h.campaign, combatID, velren.ID, 8, "slashing", "", false, goblin.ID, "keeper"); err != nil {
		t.Fatalf("attributed damage: %v", err)
	}
	// An anonymous hit — a trap, or a table that does not care.
	if _, err := h.store.Damage(ctx, h.campaign, combatID, velren.ID, 3, "fire", "", false, "", "keeper"); err != nil {
		t.Fatalf("anonymous damage: %v", err)
	}
	// A source from another battle is no source at all, not an error.
	if _, err := h.store.Damage(ctx, h.campaign, combatID, velren.ID, 2, "", "", false, "not-a-combatant", "keeper"); err != nil {
		t.Fatalf("foreign source: %v", err)
	}

	entries, err := h.store.Log(ctx, h.campaign, combatID, 0, 0)
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	var damage []LogEntry
	for _, e := range entries {
		if e.Kind == "damage" {
			damage = append(damage, e)
		}
	}
	if len(damage) != 3 {
		t.Fatalf("damage rows: %d, want 3", len(damage))
	}
	if got := damage[0].Payload["source"]; got != "Goblin" {
		t.Fatalf("attributed source name: %v", damage[0].Payload["source"])
	}
	if got := damage[0].Payload["source_id"]; got != goblin.ID {
		t.Fatalf("attributed source id: %v", damage[0].Payload["source_id"])
	}
	if _, ok := damage[0].Payload["source_entity"]; ok {
		t.Fatal("a monster source carries no entity id")
	}
	if want := "Velren takes 8 slashing · 20/28 hp from Goblin"; damage[0].Note != want {
		t.Fatalf("attributed summary: %q, want %q", damage[0].Note, want)
	}
	if _, ok := damage[1].Payload["source"]; ok {
		t.Fatal("an anonymous hit carries no source")
	}
	if _, ok := damage[2].Payload["source"]; ok {
		t.Fatal("a foreign source id is no source")
	}

	// The session mirror carries the same payload — one story, two rows.
	var mirrored int
	if err := h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_events WHERE session_id = ?
		  AND kind = 'combat' AND payload LIKE '%"source":"Goblin"%'`, h.session).Scan(&mirrored); err != nil {
		t.Fatalf("count mirrored sources: %v", err)
	}
	if mirrored != 1 {
		t.Fatalf("mirrored attributed hits: %d, want 1", mirrored)
	}

	// The return hit attributes to a pc: the entity id is what ties the
	// damage back to the character the stats will name.
	if _, err := h.store.Damage(ctx, h.campaign, combatID, goblin.ID, 9, "slashing", "", false, velren.ID, "keeper"); err != nil {
		t.Fatalf("pc-sourced damage: %v", err)
	}
	entries, err = h.store.Log(ctx, h.campaign, combatID, 0, 0)
	if err != nil {
		t.Fatalf("log again: %v", err)
	}
	for _, e := range entries {
		if e.Kind != "damage" || e.CombatantID != goblin.ID {
			continue
		}
		if got := e.Payload["source_entity"]; got != h.velren {
			t.Fatalf("pc source entity: %v, want %s", got, h.velren)
		}
		if got := e.Payload["source"]; got != "Velren" {
			t.Fatalf("pc source name: %v", got)
		}
		return
	}
	t.Fatal("the return hit never landed in the journal")
}
