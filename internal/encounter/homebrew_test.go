package encounter

// The homebrew store's tests (MAD-382): the save contract (a computed CR
// and the calculator's reasoning, always, with no path around them), the
// overlay's read shape, and the shelf's scoping rules.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/statblock"
	"github.com/madeofpendletonwool/grimoire/internal/testdb"
)

// graveMarshal builds the store's canonical test statblock: a CR 7 by the
// calculator's own arithmetic (effective HP 168 against the assumed AC 15,
// 47 damage per round at +4 to hit — the CR 7 row on both halves).
func graveMarshal(name string, avgDamage, toHit int) *statblock.Statblock {
	return &statblock.Statblock{
		Name: name, Size: "Medium", Type: "undead",
		AC: 15, HP: 168, HitDice: "21d8+74",
		Abilities: statblock.Abilities{Str: 18, Dex: 14, Con: 18, Int: 12, Wis: 14, Cha: 16},
		Actions: []statblock.Action{{
			Name: "Graveblade", Kind: "ACTION",
			Desc: fmt.Sprintf("Melee Weapon Attack: +%d to hit, reach 5 ft., one target. Hit: %d (6d8 + 20) slashing damage.", toHit, avgDamage),
		}},
	}
}

func newHomebrewStore(t *testing.T) *HomebrewStore {
	t.Helper()
	return NewHomebrewStore(testdb.Open(t))
}

func TestHomebrewSaveComputesTheCR(t *testing.T) {
	s := newHomebrewStore(t)
	ctx := context.Background()

	// The input has nowhere to put a computed CR — the label is always
	// this server's arithmetic.
	m, err := s.Save(ctx, "dm", "", HomebrewInput{
		Name: "Vashk, the Grave Marshal", Statblock: graveMarshal("Vashk, the Grave Marshal", 47, 4),
		RequestedCR: "7", Tactics: "Opens with the blade, calls the dead when hurt.",
		Lore: "A battlefield commander who did not stop commanding after dying.",
		Role: "boss",
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if m.ComputedCR != "7" {
		t.Fatalf("computed CR = %q, want the calculator's own 7", m.ComputedCR)
	}
	if m.Rating.Label != "7" || m.Rating.Defensive != 7 || m.Rating.Offensive != 7 {
		t.Fatalf("computed detail = %+v, want both halves at 7 with the label", m.Rating)
	}
	if len(m.Rating.Adjustments) == 0 && m.Rating.Confidence == "" {
		t.Fatal("the calculator's reasoning must travel with the row")
	}
	if m.Source != HomebrewDesigned {
		t.Fatalf("source = %q, want designed by default", m.Source)
	}
	if m.Slug != "vashkthegravemarshal" {
		t.Fatalf("slug = %q", m.Slug)
	}

	// The row reloads with its disagreement intact: save the same shape
	// with the damage halved under an unchanged requested CR and the
	// stored pair must disagree, out loud.
	short, err := s.Save(ctx, "dm", "", HomebrewInput{
		Name: "Half-Powered Marshal", Statblock: graveMarshal("Half-Powered Marshal", 27, 4),
		RequestedCR: "7",
	})
	if err != nil {
		t.Fatalf("save short: %v", err)
	}
	if short.ComputedCR == short.RequestedCR {
		t.Fatalf("computed %q equals requested %q — the disagreement must show", short.ComputedCR, short.RequestedCR)
	}
	got, err := s.Get(ctx, "dm", short.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ComputedCR != short.ComputedCR || got.Rating.Label != short.ComputedCR {
		t.Fatalf("round trip lost the computed pair: %+v", got)
	}

	// The one write path refuses shapeless saves.
	for name, in := range map[string]HomebrewInput{
		"no name":       {Statblock: graveMarshal("x", 47, 4)},
		"no statblock":  {Name: "Nameless"},
		"bad role":      {Name: "R", Statblock: graveMarshal("R", 47, 4), Role: "tank"},
		"bad source":    {Name: "S", Statblock: graveMarshal("S", 47, 4), Source: "imported"},
		"empty actions": {Name: "A", Statblock: &statblock.Statblock{AC: 15, HP: 10}},
	} {
		if _, err := s.Save(ctx, "dm", "", in); err == nil {
			t.Fatalf("%s: save must refuse", name)
		}
	}
	if _, err := s.Save(ctx, "", "", HomebrewInput{Name: "N", Statblock: graveMarshal("N", 47, 4)}); err == nil {
		t.Fatal("save without an owner must refuse")
	}
}

func TestHomebrewUpdateReplacesAndRescopes(t *testing.T) {
	s := newHomebrewStore(t)
	ctx := context.Background()

	m, err := s.Save(ctx, "dm", "", HomebrewInput{
		Name: "The Bell Warden", Statblock: graveMarshal("The Bell Warden", 47, 4), RequestedCR: "7",
		CampaignID: "camp-1",
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if m.CampaignID != "camp-1" {
		t.Fatalf("campaign = %q", m.CampaignID)
	}
	// An update with no campaign named keeps the row's scope, and the CR
	// is recomputed over the new statblock, not carried over.
	up, err := s.Save(ctx, "dm", m.ID, HomebrewInput{
		Name: "The Bell Warden", Statblock: graveMarshal("The Bell Warden", 27, 4), RequestedCR: "7",
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if up.CampaignID != "camp-1" {
		t.Fatalf("update lost the campaign scope: %q", up.CampaignID)
	}
	if up.ComputedCR == "7" {
		t.Fatal("update kept the old computed CR — it must recompute")
	}
	// A foreign owner cannot replace the row.
	if _, err := s.Save(ctx, "other", m.ID, HomebrewInput{
		Name: "Stolen", Statblock: graveMarshal("Stolen", 47, 4),
	}); err == nil || errors.Is(err, ErrNotFound) == false {
		t.Fatalf("foreign update err = %v, want ErrNotFound", err)
	}
	// One owner cannot carry two monsters that resolve ambiguously.
	if _, err := s.Save(ctx, "dm", "", HomebrewInput{
		Name: "the bell warden", Statblock: graveMarshal("the bell warden", 47, 4),
	}); err == nil {
		t.Fatal("a duplicate slug must refuse, not shadow")
	}
}

func TestHomebrewListScopesAndTheOverlayShape(t *testing.T) {
	s := newHomebrewStore(t)
	ctx := context.Background()

	mk := func(owner, campaign, name string, dmg int) *HomebrewMonster {
		t.Helper()
		m, err := s.Save(ctx, owner, "", HomebrewInput{
			Name: name, CampaignID: campaign,
			Statblock: graveMarshal(name, dmg, 4), RequestedCR: "7", Role: "boss",
		})
		if err != nil {
			t.Fatalf("save %s: %v", name, err)
		}
		return m
	}
	unscoped := mk("dm", "", "The Road Warden", 47)
	scoped := mk("dm", "camp-1", "Vashk, the Grave Marshal", 47)
	mk("other", "", "Someone Else's Monster", 47)

	// The unscoped shelf is the owner's whole list.
	all, err := s.List(ctx, "dm", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("owner sees %d monsters, want 2 (never another owner's)", len(all))
	}
	// A campaign view leads with the campaign's own, then the shelf.
	scopedList, err := s.List(ctx, "dm", "camp-1")
	if err != nil {
		t.Fatalf("scoped list: %v", err)
	}
	if len(scopedList) != 2 || scopedList[0].ID != scoped.ID || scopedList[1].ID != unscoped.ID {
		t.Fatalf("scoped list order = %+v", scopedList)
	}
	// Another campaign does not see the first campaign's designs.
	other, err := s.List(ctx, "dm", "camp-2")
	if err != nil {
		t.Fatalf("other list: %v", err)
	}
	if len(other) != 1 || other[0].ID != unscoped.ID {
		t.Fatalf("foreign campaign sees %+v", other)
	}
	if _, err := s.Get(ctx, "other", scoped.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign get err = %v, want not found", err)
	}

	// The overlay: the catalog's read shape, tagged homebrew, priced by
	// the existing XP arithmetic.
	creatures, err := s.Overlay(ctx, "dm", "camp-1")
	if err != nil {
		t.Fatalf("overlay: %v", err)
	}
	if len(creatures) != 2 {
		t.Fatalf("overlay carries %d, want 2", len(creatures))
	}
	var vashk *Creature
	for i := range creatures {
		if creatures[i].Name == scoped.Name {
			vashk = &creatures[i]
		}
	}
	if vashk == nil {
		t.Fatal("the campaign's monster is not in the overlay")
	}
	if !vashk.Homebrew || vashk.Doc != "homebrew" {
		t.Fatalf("homebrew must be labelled: %+v", vashk)
	}
	if !containsStr(vashk.Tags, "homebrew") || !containsStr(vashk.Tags, "boss") {
		t.Fatalf("tags = %v, want homebrew and the role", vashk.Tags)
	}
	if vashk.CR != "7" || vashk.CRNum != 7 || vashk.XP != 2900 {
		t.Fatalf("pricing = CR %s (%f) %d XP, want 7 / 2900", vashk.CR, vashk.CRNum, vashk.XP)
	}
	if len(vashk.Attacks) != 1 || vashk.Attacks[0].ToHit != 4 || vashk.Attacks[0].Damage[0].Avg != 47 {
		t.Fatalf("attacks not parsed on the way out: %+v", vashk.Attacks)
	}

	// The campaign names loader the canon engine reads.
	names, err := s.CampaignNames(ctx, "camp-1")
	if err != nil {
		t.Fatalf("campaign names: %v", err)
	}
	if len(names) != 1 || names[0] != scoped.Name {
		t.Fatalf("campaign names = %v", names)
	}

	if err := s.Delete(ctx, "dm", unscoped.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.Delete(ctx, "dm", unscoped.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete err = %v, want not found", err)
	}
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// The stored actions carry their parse — the never-half-parsed contract —
// and a hand-entered statblock is parsed on the way out.
func TestHomebrewCreatureParsesHandEnteredProse(t *testing.T) {
	s := newHomebrewStore(t)
	m, err := s.Save(context.Background(), "dm", "", HomebrewInput{
		Name: "The Unreadable", Source: HomebrewHand, RequestedCR: "7",
		Statblock: &statblock.Statblock{
			Name: "The Unreadable", AC: 15, HP: 168,
			Actions: []statblock.Action{
				{Name: "Graveblade", Kind: "ACTION", Desc: "Melee Weapon Attack: +4 to hit, reach 5 ft., one target. Hit: 47 (6d8 + 20) slashing damage."},
				{Name: "Summon the Hounds", Kind: "ACTION", Desc: "The unreadable calls for aid. Roll for reinforcements."},
			},
		},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if m.Source != HomebrewHand {
		t.Fatalf("source = %q", m.Source)
	}
	c := m.Creature()
	if len(c.Attacks) != 1 || c.Attacks[0].Name != "Graveblade" {
		t.Fatalf("parsed attacks = %+v", c.Attacks)
	}
	if len(c.Unparsed) != 1 || c.Unparsed[0].Name != "Summon the Hounds" {
		t.Fatalf("unparsed actions = %+v", c.Unparsed)
	}
	if !strings.Contains(strings.Join(c.Tags, " "), "homebrew") {
		t.Fatalf("tags = %v", c.Tags)
	}
}
