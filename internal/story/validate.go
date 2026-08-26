package story

// Validate: the spine's consistency rules, pure over a Spine. Every rule
// emits campaign.Finding values, so its findings flow into the canon flag
// ledger and `grimoire canon check` (canon.CheckSnapshot appends these)
// rather than standing up a second parallel findings system.
//
// The rules, each with a test that fires it and a test that does not (ADR 8:
// an untested consistency rule is worse than no rule, because it is
// trusted):
//
//   - scene_without_cast      a scene nobody is in
//   - outcome_out_of_act      an outcome pointing at a scene in another act
//   - quest_edge_missing      an outcome naming a quest move the machine
//                             does not have (the machine may have changed
//                             under a planned outcome)
//   - secret_already_granted  a secret placed in play that the party's
//                             awareness already grants — a join, free
//   - act_level_mismatch      a level band that overlaps its neighbour or
//                             leaves a gap

import (
	"fmt"
	"sort"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// Check codes. Stable strings — the flag ledger keys rows on them.
const (
	CheckSceneWithoutCast     = "scene_without_cast"
	CheckOutcomeOutOfAct      = "outcome_out_of_act"
	CheckQuestEdgeMissing     = "quest_edge_missing"
	CheckSecretAlreadyGranted = "secret_already_granted"
	CheckActLevelMismatch     = "act_level_mismatch"
)

// grantingStance mirrors the canon engine's rule: knows, suspects and
// believes_false grant; a deliberate unaware does not.
func grantingStance(stance string) bool {
	return stance == "knows" || stance == "suspects" || stance == "believes_false"
}

// Validate runs every spine rule over a snapshot, pure and deterministic.
// Findings are sorted by check, then record, so output is stable. A spine
// with no acts validates clean — a campaign that has planned nothing has
// nothing wrong.
func Validate(sp *Spine) []campaign.Finding {
	if sp == nil {
		return nil
	}
	var out []campaign.Finding
	out = append(out, checkSceneWithoutCast(sp)...)
	out = append(out, checkOutcomeOutOfAct(sp)...)
	out = append(out, checkQuestEdgeMissing(sp)...)
	out = append(out, checkSecretAlreadyGranted(sp)...)
	out = append(out, checkActLevelMismatch(sp)...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Check != out[j].Check {
			return out[i].Check < out[j].Check
		}
		if out[i].RecordKind != out[j].RecordKind {
			return out[i].RecordKind < out[j].RecordKind
		}
		return out[i].RecordID < out[j].RecordID
	})
	return out
}

// checkSceneWithoutCast: a scene with no cast rows. A scene nobody is in is
// a placeholder that survived — the planner's "what did we forget?" signal,
// the same family as unused_npc.
func checkSceneWithoutCast(sp *Spine) []campaign.Finding {
	var out []campaign.Finding
	for i := range sp.Scenes {
		sc := &sp.Scenes[i]
		if len(sc.Cast) == 0 {
			out = append(out, campaign.Finding{
				Check: CheckSceneWithoutCast, Severity: campaign.SeverityWarn,
				RecordKind: "scene", RecordID: sc.ID,
				Message: fmt.Sprintf("scene %q has no cast; seat someone on stage or cut the scene", sc.Name),
			})
		}
	}
	return out
}

// checkOutcomeOutOfAct: an outcome whose leads_to_scene points at a scene in
// another act. A branch may skip scenes inside its act (and past its act's
// end is exactly what outcomes are for), but pointing into the middle of a
// different act breaks the spine's own ordering — the target act is no
// longer reachable from where the outcome fires.
func checkOutcomeOutOfAct(sp *Spine) []campaign.Finding {
	var out []campaign.Finding
	for i := range sp.Scenes {
		sc := &sp.Scenes[i]
		own, ok := sp.actOf(sc.ID)
		if !ok {
			continue // a scene without an act is a database shape bug, not a rule
		}
		for _, o := range sc.Outcomes {
			if o.LeadsToScene == "" {
				continue
			}
			target, ok := sp.actOf(o.LeadsToScene)
			if !ok || target.ID == own.ID {
				continue
			}
			out = append(out, campaign.Finding{
				Check: CheckOutcomeOutOfAct, Severity: campaign.SeverityError,
				RecordKind: "scene_outcome", RecordID: o.ID,
				Message: fmt.Sprintf("outcome %q of scene %q points at scene %s in act %q; a branch cannot jump into another act",
					o.Label, sc.Name, o.LeadsToScene, target.Name),
			})
		}
	}
	return out
}

// checkQuestEdgeMissing: an outcome naming a quest move the machine does not
// have. The write path checks the edge at add time; the machine may have
// changed since, which is why this rule exists — the recorded promise and
// the quest's own shape have drifted apart.
func checkQuestEdgeMissing(sp *Spine) []campaign.Finding {
	machines := map[string]campaign.StateMachine{}
	for _, q := range sp.Quests {
		machines[q.ID] = q.Machine
	}
	var out []campaign.Finding
	for i := range sp.Scenes {
		for _, o := range sp.Scenes[i].Outcomes {
			if o.QuestTransition == nil {
				continue
			}
			t := o.QuestTransition
			m, ok := machines[t.QuestID]
			if !ok || !m.HasEdge(t.From, t.To) {
				out = append(out, campaign.Finding{
					Check: CheckQuestEdgeMissing, Severity: campaign.SeverityError,
					RecordKind: "scene_outcome", RecordID: o.ID,
					Message: fmt.Sprintf("outcome %q promises quest move %s -> %s that quest %s's machine does not have",
						o.Label, t.From, t.To, t.QuestID),
				})
			}
		}
	}
	return out
}

// checkSecretAlreadyGranted: a secret placed in play that the party's
// awareness already grants. Planning a secret into a scene is what makes it
// reachable — placing one the party has already learned wastes the scene's
// secret slot and tells the DM their plan is stale. NPC grants are
// deliberately out of scope: an NPC's knowledge feeds DM-side simulation,
// not the party's reachability.
func checkSecretAlreadyGranted(sp *Spine) []campaign.Finding {
	pcs := map[string]bool{}
	for id, kind := range sp.EntityKind {
		pcs[id] = kind == campaign.KindPC
	}
	// Party grants and pc grants both count — the character scope reads its
	// own rows plus the party's.
	granted := map[string]bool{}
	for _, a := range sp.Awareness {
		if a.Knower == campaign.PartyKnower || pcs[a.Knower] {
			if grantingStance(a.Stance) {
				granted[a.FactID] = true
			}
		}
	}
	var out []campaign.Finding
	for i := range sp.Scenes {
		sc := &sp.Scenes[i]
		for _, sec := range sc.Secrets {
			if sec.Disposition != DispositionInPlay || !granted[sec.FactID] {
				continue
			}
			statement := sp.FactStatement[sec.FactID]
			if statement == "" {
				statement = sec.FactID
			}
			out = append(out, campaign.Finding{
				Check: CheckSecretAlreadyGranted, Severity: campaign.SeverityWarn,
				RecordKind: "scene_secret", RecordID: sc.ID + "/" + sec.FactID,
				Message: fmt.Sprintf("scene %q puts secret %q in play, but the party's awareness already grants it — replant or cut it",
					sc.Name, statement),
			})
		}
	}
	return out
}

// checkActLevelMismatch: consecutive acts whose level bands overlap or leave
// a gap. The spine's bands must chain: each act starts where the last one
// ended plus one. Acts are ordered by ordinal; a single act validates clean.
func checkActLevelMismatch(sp *Spine) []campaign.Finding {
	acts := append([]Act(nil), sp.Acts...)
	sort.Slice(acts, func(i, j int) bool { return acts[i].Ordinal < acts[j].Ordinal })
	var out []campaign.Finding
	for i := 1; i < len(acts); i++ {
		prev, next := acts[i-1], acts[i]
		switch {
		case next.LevelStart <= prev.LevelEnd:
			out = append(out, campaign.Finding{
				Check: CheckActLevelMismatch, Severity: campaign.SeverityWarn,
				RecordKind: "act", RecordID: next.ID,
				Message: fmt.Sprintf("act %q (levels %d-%d) overlaps act %q (levels %d-%d); bands must chain, not share",
					next.Name, next.LevelStart, next.LevelEnd, prev.Name, prev.LevelStart, prev.LevelEnd),
			})
		case next.LevelStart > prev.LevelEnd+1:
			out = append(out, campaign.Finding{
				Check: CheckActLevelMismatch, Severity: campaign.SeverityWarn,
				RecordKind: "act", RecordID: next.ID,
				Message: fmt.Sprintf("act %q starts at level %d but act %q ends at %d; the party crosses a gap nobody planned",
					next.Name, next.LevelStart, prev.Name, prev.LevelEnd),
			})
		}
	}
	return out
}
