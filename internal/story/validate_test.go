package story

// Validate tests (ADR 8: an untested consistency rule is worse than no rule,
// because it is trusted). Every rule has a test that fires it and a test
// that does not — each case below builds the smallest spine that isolates
// one rule, so a failure names its rule.

import (
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// spineBuilder assembles a Spine without a database: the rules are pure, so
// their tests never open one.
type spineBuilder struct {
	sp Spine
}

func newSpine() *spineBuilder {
	return &spineBuilder{sp: Spine{
		CampaignID:    "c1",
		FactStatement: map[string]string{"f1": "The Duke is secretly a vampire."},
		EntityName:    map[string]string{"pc1": "Mira", "npc1": "The Duke"},
		EntityKind:    map[string]string{"pc1": campaign.KindPC, "npc1": campaign.KindNPC},
	}}
}

func (b *spineBuilder) act(id string, ordinal int64, lo, hi int) *spineBuilder {
	b.sp.Acts = append(b.sp.Acts, Act{ID: id, CampaignID: b.sp.CampaignID, Ordinal: ordinal,
		Name: "Act " + id, LevelStart: lo, LevelEnd: hi, Status: StatusPlanned})
	return b
}

// scene appends a scene and returns its id; later mutations go through the
// by-id helpers, which re-find the row in the slice — appending between
// mutations would otherwise hand back stale pointers.
func (b *spineBuilder) scene(id, actID string) string {
	b.sp.Scenes = append(b.sp.Scenes, Scene{ID: id, CampaignID: b.sp.CampaignID, ActID: actID,
		Kind: KindSocial, Name: "Scene " + id, Status: StatusPlanned})
	return id
}

// find locates a scene fresh, by id.
func (b *spineBuilder) find(id string) *Scene {
	for i := range b.sp.Scenes {
		if b.sp.Scenes[i].ID == id {
			return &b.sp.Scenes[i]
		}
	}
	panic("test builder: unknown scene " + id)
}

func (b *spineBuilder) seat(sceneID, entityID, role string) *spineBuilder {
	b.find(sceneID).Cast = append(b.find(sceneID).Cast,
		CastMember{SceneID: sceneID, EntityID: entityID, Role: role})
	return b
}

func (b *spineBuilder) outcome(sceneID, label, leadsTo string, t *QuestTransition) *spineBuilder {
	sc := b.find(sceneID)
	sc.Outcomes = append(sc.Outcomes, SceneOutcome{ID: "o-" + label, SceneID: sceneID,
		Label: label, LeadsToScene: leadsTo, QuestTransition: t})
	return b
}

func (b *spineBuilder) plantSecret(sceneID, factID, disposition string) *spineBuilder {
	sc := b.find(sceneID)
	sc.Secrets = append(sc.Secrets, SceneSecret{SceneID: sceneID, FactID: factID,
		Disposition: disposition})
	return b
}

func (b *spineBuilder) grant(knower, factID, stance string) *spineBuilder {
	b.sp.Awareness = append(b.sp.Awareness, AwarenessGrant{Knower: knower, FactID: factID, Stance: stance})
	return b
}

func (b *spineBuilder) quest(id string, m campaign.StateMachine) *spineBuilder {
	b.sp.Quests = append(b.sp.Quests, campaign.Quest{ID: id, CampaignID: b.sp.CampaignID, Machine: m})
	return b
}

func (b *spineBuilder) build() *Spine { return &b.sp }

// codes flattens a finding set for the "does not fire" assertions.
func codes(fs []campaign.Finding) map[string]bool {
	out := map[string]bool{}
	for _, f := range fs {
		out[f.Check] = true
	}
	return out
}

/* ---------- scene_without_cast ---------- */

func TestSceneWithoutCastFires(t *testing.T) {
	b := newSpine().act("a1", 1, 1, 4)
	b.scene("s1", "a1") // no cast
	fs := Validate(b.build())
	if !codes(fs)[CheckSceneWithoutCast] {
		t.Errorf("a scene with no cast must fire scene_without_cast, got %+v", fs)
	}
}

func TestSceneWithoutCastDoesNotFireWhenSeated(t *testing.T) {
	b := newSpine().act("a1", 1, 1, 4)
	s1 := b.scene("s1", "a1")
	b.seat(s1, "npc1", RoleFocus)
	if codes(Validate(b.build()))[CheckSceneWithoutCast] {
		t.Error("a seated scene must not fire scene_without_cast")
	}
}

/* ---------- outcome_out_of_act ---------- */

func TestOutcomeOutOfActFires(t *testing.T) {
	b := newSpine().act("a1", 1, 1, 4).act("a2", 2, 4, 8)
	own := b.scene("s1", "a1")
	other := b.scene("s2", "a2")
	b.seat(own, "npc1", RoleFocus).seat(other, "npc1", RoleFocus)
	b.outcome(own, "A", other, nil)
	if !codes(Validate(b.build()))[CheckOutcomeOutOfAct] {
		t.Error("an outcome pointing into another act must fire outcome_out_of_act")
	}
}

func TestOutcomeOutOfActAllowsItsOwnAct(t *testing.T) {
	b := newSpine().act("a1", 1, 1, 4)
	own := b.scene("s1", "a1")
	later := b.scene("s2", "a1")
	b.seat(own, "npc1", RoleFocus).seat(later, "npc1", RoleFocus)
	b.outcome(own, "A", later, nil)
	if codes(Validate(b.build()))[CheckOutcomeOutOfAct] {
		t.Error("a branch skipping ahead inside its own act is the point of outcomes; it must not fire")
	}
}

/* ---------- quest_edge_missing ---------- */

func TestQuestEdgeMissingFires(t *testing.T) {
	m := campaign.StateMachine{
		Initial: "rumoured",
		States:  campaign.States("rumoured", "found", "ended"),
		Edges:   []campaign.StateEdge{{From: "rumoured", To: "found"}, {From: "found", To: "ended"}},
	}
	b := newSpine().act("a1", 1, 1, 4).quest("q1", m)
	s1 := b.scene("s1", "a1")
	b.seat(s1, "npc1", RoleFocus)
	b.outcome(s1, "A", "", &QuestTransition{QuestID: "q1", From: "rumoured", To: "ended"}) // the machine has no such edge
	if !codes(Validate(b.build()))[CheckQuestEdgeMissing] {
		t.Error("an outcome promising a move the machine does not have must fire quest_edge_missing")
	}
}

func TestQuestEdgeMissingDoesNotFireOnALegalEdge(t *testing.T) {
	m := campaign.StateMachine{
		Initial: "rumoured",
		States:  campaign.States("rumoured", "found", "ended"),
		Edges:   []campaign.StateEdge{{From: "rumoured", To: "found"}, {From: "found", To: "ended"}},
	}
	b := newSpine().act("a1", 1, 1, 4).quest("q1", m)
	s1 := b.scene("s1", "a1")
	b.seat(s1, "npc1", RoleFocus)
	b.outcome(s1, "A", "", &QuestTransition{QuestID: "q1", From: "rumoured", To: "found"})
	if codes(Validate(b.build()))[CheckQuestEdgeMissing] {
		t.Error("an outcome naming a real edge must not fire quest_edge_missing")
	}
}

/* ---------- secret_already_granted ---------- */

func TestSecretAlreadyGrantedFiresForPartyGrant(t *testing.T) {
	b := newSpine().act("a1", 1, 1, 4).grant(campaign.PartyKnower, "f1", "knows")
	s1 := b.scene("s1", "a1")
	b.seat(s1, "npc1", RoleFocus).plantSecret(s1, "f1", DispositionInPlay)
	if !codes(Validate(b.build()))[CheckSecretAlreadyGranted] {
		t.Error("a secret in play the party already holds must fire secret_already_granted")
	}
}

func TestSecretAlreadyGrantedCountsACharacterGrant(t *testing.T) {
	b := newSpine().act("a1", 1, 1, 4).grant("pc1", "f1", "suspects")
	s1 := b.scene("s1", "a1")
	b.seat(s1, "npc1", RoleFocus).plantSecret(s1, "f1", DispositionInPlay)
	if !codes(Validate(b.build()))[CheckSecretAlreadyGranted] {
		t.Error("the character scope reads its own rows plus the party's; a pc grant must count")
	}
}

func TestSecretAlreadyGrantedIgnoresNPCGrantsAndDeliberateUnaware(t *testing.T) {
	for _, grant := range []AwarenessGrant{
		{Knower: "npc1", FactID: "f1", Stance: "knows"},                 // an NPC's knowledge is not the party's
		{Knower: campaign.PartyKnower, FactID: "f1", Stance: "unaware"}, // walked past, still plantable
	} {
		b := newSpine().act("a1", 1, 1, 4).grant(grant.Knower, grant.FactID, grant.Stance)
		s1 := b.scene("s1", "a1")
		b.seat(s1, "npc1", RoleFocus).plantSecret(s1, "f1", DispositionInPlay)
		if codes(Validate(b.build()))[CheckSecretAlreadyGranted] {
			t.Errorf("knower %s with stance %s must not count as granted", grant.Knower, grant.Stance)
		}
	}
}

func TestSecretAlreadyGrantedIgnoresWithheldSecrets(t *testing.T) {
	b := newSpine().act("a1", 1, 1, 4).grant(campaign.PartyKnower, "f1", "knows")
	s1 := b.scene("s1", "a1")
	b.seat(s1, "npc1", RoleFocus).plantSecret(s1, "f1", DispositionWithheld)
	if codes(Validate(b.build()))[CheckSecretAlreadyGranted] {
		t.Error("a withheld secret is not in play; the rule must not fire")
	}
}

func TestSecretAlreadyGrantedIgnoresUnplantedSecrets(t *testing.T) {
	// The party holds the secret but no scene puts it in play: that is
	// unreachable_secret's cousin, not this rule's problem.
	b := newSpine().act("a1", 1, 1, 4).grant(campaign.PartyKnower, "f1", "knows")
	s1 := b.scene("s1", "a1")
	b.seat(s1, "npc1", RoleFocus)
	if codes(Validate(b.build()))[CheckSecretAlreadyGranted] {
		t.Error("a granted secret no scene engages must not fire")
	}
}

/* ---------- act_level_mismatch ---------- */

func TestActLevelMismatchFiresOnOverlap(t *testing.T) {
	b := newSpine().act("a1", 1, 1, 5).act("a2", 2, 4, 9) // a2 starts before a1 ends
	if !codes(Validate(b.build()))[CheckActLevelMismatch] {
		t.Error("overlapping level bands must fire act_level_mismatch")
	}
}

func TestActLevelMismatchFiresOnGap(t *testing.T) {
	b := newSpine().act("a1", 1, 1, 4).act("a2", 2, 6, 10) // nobody planned levels 5
	if !codes(Validate(b.build()))[CheckActLevelMismatch] {
		t.Error("a gap between level bands must fire act_level_mismatch")
	}
}

func TestActLevelMismatchDoesNotFireOnAChainedSpine(t *testing.T) {
	b := newSpine().act("a1", 1, 1, 4).act("a2", 2, 5, 10).act("a3", 3, 11, 12)
	if codes(Validate(b.build()))[CheckActLevelMismatch] {
		t.Error("bands that chain exactly must not fire")
	}
}

func TestActLevelMismatchIgnoresASingleAct(t *testing.T) {
	b := newSpine().act("a1", 1, 1, 20)
	if codes(Validate(b.build()))[CheckActLevelMismatch] {
		t.Error("a single act has no neighbour to mismatch")
	}
}

/* ---------- the whole rule set ---------- */

func TestValidateIsSortedAndEmptySpineIsClean(t *testing.T) {
	if fs := Validate(nil); fs != nil {
		t.Errorf("a nil spine must validate clean, got %+v", fs)
	}
	if fs := Validate(newSpine().build()); len(fs) != 0 {
		t.Errorf("a spine that planned nothing has nothing wrong, got %+v", fs)
	}
	// A spine firing every rule at once comes back sorted by check.
	b := newSpine().
		act("a1", 1, 1, 5).act("a2", 2, 4, 9). // mismatch
		grant(campaign.PartyKnower, "f1", "knows")
	orphan := b.scene("s1", "a1") // no cast
	other := b.scene("s2", "a2")
	b.seat(other, "npc1", RoleFocus)
	b.outcome(orphan, "A", other, &QuestTransition{QuestID: "q-none", From: "a", To: "b"})
	b.plantSecret(other, "f1", DispositionInPlay)
	fs := Validate(b.build())
	if len(fs) != 5 {
		t.Fatalf("the kitchen-sink spine must fire all five rules, got %d: %+v", len(fs), fs)
	}
	for i := 1; i < len(fs); i++ {
		if fs[i].Check < fs[i-1].Check {
			t.Errorf("findings are not sorted by check: %+v", fs)
		}
	}
}
