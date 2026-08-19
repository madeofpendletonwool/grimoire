package carddb

import (
	"context"
	"testing"
)

func TestNormalizeName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Kodama's Reach", "kodamas reach"},
		{"Kodama’s Reach", "kodamas reach"},
		{"Lim-Dûl's Vault", "lim duls vault"},
		{"Æther Vial", "aether vial"},
		{"Fire // Ice", "fire // ice"},
		{"  Sol   Ring ", "sol ring"},
	}
	for _, c := range cases {
		if got := NormalizeName(c.in); got != c.want {
			t.Errorf("NormalizeName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSimilarityRejectsUnrelatedNames(t *testing.T) {
	if s := similarity("sol ring", "solfatara"); s >= 0.86 {
		t.Errorf("similarity(sol ring, solfatara) = %v, want below the bar", s)
	}
	if s := similarity("command tower", "command power plant"); s >= 0.86 {
		t.Errorf("similarity(command tower, command power plant) = %v, want below the bar", s)
	}
	if s := similarity("kodamas reach", "kodamas reach"); s != 1 {
		t.Errorf("identical names scored %v, want 1", s)
	}
}

func TestResolve(t *testing.T) {
	store, _ := populateFixture(t, kaaliaFixture(t))
	ctx := context.Background()

	cases := []struct{ in, want string }{
		{"Sol Ring", "Sol Ring"},
		{"sol ring", "Sol Ring"},
		{"  Sol Ring  ", "Sol Ring"},
		// The set code an exporter appends is stripped upstream; a name that
		// still differs only by punctuation or case resolves here.
		{"COUNTERSPELL", "Counterspell"},
		// A double-faced card written as its front face alone.
		{"Fire", "Fire // Ice"},
		{"Fire // Ice", "Fire // Ice"},
		// A typo close enough to be certain about.
		{"Angel of Serenty", "Angel of Serenity"},
	}
	for _, c := range cases {
		got, ok := store.Resolve(ctx, c.in)
		if !ok {
			t.Errorf("Resolve(%q) missed, want %q", c.in, c.want)
			continue
		}
		if got.Name != c.want {
			t.Errorf("Resolve(%q) = %q, want %q", c.in, got.Name, c.want)
		}
	}

	// The important half: a name that is not in the database comes back as a
	// miss, not as whatever the text search ranked first.
	for _, miss := range []string{"Command Tower", "Definitely Not A Card", "Blex, Vexing Pest"} {
		if got, ok := store.Resolve(ctx, miss); ok {
			t.Errorf("Resolve(%q) = %q, want no match", miss, got.Name)
		}
	}
}

func TestCommandersPreferThemeAndExactColors(t *testing.T) {
	store, _ := populateFixture(t, tribalFixture(t))
	ctx := context.Background()

	// "goblins", in Gruul. The one Gruul Goblin legend must lead, ahead of the
	// far more popular mono-red Goblin legends that also fit inside RG.
	got, err := store.Commanders(ctx, MaskForColors("RG"), []string{"goblins"}, 5)
	if err != nil {
		t.Fatalf("Commanders: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("Commanders returned nothing")
	}
	if got[0].Name != "Rulik Mons, Warren Chief" {
		names := make([]string, len(got))
		for i, c := range got {
			names[i] = c.Name
		}
		t.Errorf("top commander = %q, want Rulik Mons, Warren Chief (got %v)", got[0].Name, names)
	}
	// Everything returned still has to fit inside the requested identity.
	for _, c := range got {
		if !c.IdentityAllowed(MaskForColors("RG")) {
			t.Errorf("%q (%s) is outside RG", c.Name, c.ColorIdentity)
		}
		if !c.CommanderLegal {
			t.Errorf("%q is not commander-legal", c.Name)
		}
	}
}

// tribalFixture is the case from the bug report: several popular mono-red
// Goblin legends, one obscure Gruul one, and a Gruul legend with no tribal
// connection at all.
func tribalFixture(t *testing.T) []byte {
	return fixtureAtomic(t, map[string][]map[string]any{
		"Rulik Mons, Warren Chief": {{
			"manaCost": "{2}{R}{G}", "manaValue": 4,
			"type":             "Legendary Creature — Goblin Warrior",
			"text":             "Whenever Rulik Mons attacks, create a 1/1 red Goblin creature token.",
			"colorIdentity":    []string{"R", "G"},
			"edhrecRank":       rank(6000),
			"leadershipSkills": ls(true),
			"legalities":       map[string]string{"commander": "Legal"},
		}},
		"Krenko, Mob Boss": {{
			"manaCost": "{2}{R}{R}", "manaValue": 4,
			"type":             "Legendary Creature — Goblin Warrior",
			"text":             "{T}: Create X 1/1 red Goblin creature tokens, where X is the number of Goblins you control.",
			"colorIdentity":    []string{"R"},
			"edhrecRank":       rank(1094),
			"leadershipSkills": ls(true),
			"legalities":       map[string]string{"commander": "Legal"},
		}},
		"Krenko, Tin Street Kingpin": {{
			"manaCost": "{2}{R}", "manaValue": 3,
			"type":             "Legendary Creature — Goblin Warrior",
			"text":             "Whenever Krenko attacks, put a +1/+1 counter on it, then create tokens.",
			"colorIdentity":    []string{"R"},
			"edhrecRank":       rank(887),
			"leadershipSkills": ls(true),
			"legalities":       map[string]string{"commander": "Legal"},
		}},
		"Omnath, Locus of Rage": {{
			"manaCost": "{3}{R}{R}{G}{G}", "manaValue": 7,
			"type":             "Legendary Creature — Elemental",
			"text":             "Landfall — Whenever a land enters under your control, create a 5/5 red and green Elemental creature token.",
			"colorIdentity":    []string{"R", "G"},
			"edhrecRank":       rank(946),
			"leadershipSkills": ls(true),
			"legalities":       map[string]string{"commander": "Legal"},
		}},
		"Goblin Guide": {{
			"manaCost": "{R}", "manaValue": 1,
			"type":          "Creature — Goblin Scout",
			"text":          "Haste. Whenever Goblin Guide attacks, defending player reveals the top card of their library.",
			"colorIdentity": []string{"R"},
			"edhrecRank":    rank(200),
			"legalities":    map[string]string{"commander": "Legal"},
		}},
	})
}
