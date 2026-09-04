package homebrew

// The retrieval fixture: a tiny offline SRD index. The FTS store is
// opened the way every store opens it, then a pinned set of corpus
// records and reader nodes is inserted directly — no network, no
// upstream fetch — so the nearest-mechanic search is proven against
// documents the test itself controls.

import (
	"context"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/data"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/statblock"
	"github.com/madeofpendletonwool/grimoire/internal/testdb"
)

type docRow struct {
	number, title, body string
}

var lintCorpusDocs = []docRow{
	{
		number: "bestiary/vampire/0001.1", title: "Vampire",
		body: "Medium undead, any evil alignment. Legendary Resistance (3/day). Bite. Melee Weapon Attack: +9 to hit. Hit: 7 (1d8+5) piercing damage plus 10 (3d6) necrotic damage. The vampire regains hit points equal to the necrotic damage taken.",
	},
	{
		number: "spells/holdperson/0001.1", title: "Hold Person",
		body: "2nd-level enchantment. Choose a humanoid you can see. The target must succeed on a Wisdom saving throw or be paralyzed for the duration. At higher levels: one additional humanoid per slot level above 2nd.",
	},
	{
		number: "spells/chilltouch/0001.1", title: "Chill Touch",
		body: "Necromancy cantrip. You create a ghostly, skeletal hand. Melee spell attack: hit deals 1d8 necrotic damage, and the target can't regain hit points until the start of your next turn.",
	},
	{
		number: "items/daggerofvenom/0001.1", title: "Dagger of Venom",
		body: "Weapon (dagger), rare. You gain a +1 bonus to attack and damage rolls. You can use an action to coat the blade in poison; the next hit deals an extra 2d10 poison damage, DC 15 Constitution save.",
	},
	{
		number: "items/flametongue/0001.1", title: "Flame Tongue",
		body: "Weapon (any sword that deals slashing damage), rare. You can use a bonus action to speak the command word; the sword erupts in fire, shedding bright light and dealing an extra 2d6 fire damage.",
	},
}

// openLintIndex opens an index store over a migrated database and fills
// it with the pinned corpus, including the reader node that makes the
// citation's deep link resolvable end to end.
func openLintIndex(t *testing.T) *index.Store {
	t.Helper()
	store, err := index.Open(testdb.Path(t))
	if err != nil {
		t.Fatalf("open index store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, d := range lintCorpusDocs {
		if _, err := store.DB().Exec(
			`INSERT INTO docs(corpus, number, title, body, source) VALUES ('dnd', ?, ?, ?, 'srd')`,
			d.number, d.title, d.body); err != nil {
			t.Fatalf("insert corpus doc %s: %v", d.number, err)
		}
	}
	// The reader node the citation resolves onto: the record number's
	// section key, the way the real rebuild stores D&D pages.
	for _, node := range []string{"bestiary/vampire", "items/flametongue"} {
		if _, err := store.DB().Exec(
			`INSERT INTO reader_nodes(corpus, guide, guide_title, guide_kind, number, title, level, position, body, source)
			 VALUES ('dnd', ?, 'Monster Manual', 'srd', ?, 'Vampire', 2, 1, '', 'srd')`,
			node, node); err != nil {
			t.Fatalf("insert reader node %s: %v", node, err)
		}
	}
	return store
}

// statblockVampire is the pinned retrieval input: an undead bruiser whose
// signature damage and type should surface the corpus's Vampire.
func statblockVampire() statblock.Statblock {
	return statblock.Statblock{
		Name: "Vashk the Night Lord", Size: "Medium", Type: "undead",
		AC: 16, HP: 82,
		Abilities: statblock.Abilities{Str: 18, Dex: 18, Con: 16, Int: 17, Wis: 15, Cha: 18},
		Speeds:    map[string]int{"walk": 30},
		Actions: []statblock.Action{
			{Name: "Bite", Kind: "ACTION", Parsed: true,
				Attack: statblock.Attack{Name: "Bite", Kind: "melee", ToHit: 7, Reach: 5,
					Damage: []statblock.Damage{{Dice: "2d6+4", Avg: 11, Type: "piercing"},
						{Dice: "3d6", Avg: 10, Type: "necrotic"}}}},
		},
	}
}

// TestCitationDeepLinksIntoTheReader: the citation's number — the thing
// the reader API is called with — resolves to a real reader page, the
// same path a search citation takes.
func TestCitationDeepLinksIntoTheReader(t *testing.T) {
	store := openLintIndex(t)
	vampire := statblockVampire()
	e := &Engine{Index: store}
	rep := e.LintMonster(context.Background(), MonsterInput{Statblock: vampire})
	if len(rep.Neighbours) == 0 || rep.Neighbours[0].Title != "Vampire" {
		t.Fatalf("neighbours misordered: %+v", rep.Neighbours)
	}
	guide, node, err := store.ReaderResolve(context.Background(),
		data.CorpusDND, rep.Neighbours[0].Number)
	if err != nil {
		t.Fatal(err)
	}
	if guide == "" || node == "" {
		t.Fatalf("citation number %q does not deep-link into the reader", rep.Neighbours[0].Number)
	}
}
