package server

// The tactical analysis surface (MAD-381). The design flow computes the
// analysis over the roster the model just chose and gates the model's
// tactics prose against it; this endpoint is the live half — the same pure
// arithmetic, recomputed whenever the DM edits the roster, so the read on
// screen is always the server's current one. No model is involved here:
// every number in a tactics block traces to a derivation, which is the
// whole point of the block.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/encounter"
)

// TacticsProse is the model-written half of the tactics block, riding under
// the derived analysis with the gate's verdict attached. ProseStatus is
// "ok" when every number in it traced, "rejected" when the gate found an
// invented figure, and "none" when there was no prose to check.
type TacticsProse struct {
	Prose       string                     `json:"prose"`
	ProseStatus string                     `json:"prose_status"`
	Violations  []encounter.ProseViolation `json:"violations,omitempty"`
}

// partyFacts maps a campaign's party table into the facts the analysis
// reads. Every field rides only when the pc declared it — the analysis
// treats an undeclared number as unknown, never as zero.
func partyFacts(table *campaign.PartyTable) []encounter.PCFact {
	if table == nil {
		return nil
	}
	var out []encounter.PCFact
	for _, m := range table.Members {
		out = append(out, encounter.PCFact{
			Name:    m.Name,
			Class:   m.Block.Class,
			Level:   m.Block.Level,
			AC:      m.Block.AC,
			MaxHP:   m.Block.MaxHP,
			Saves:   m.Block.Saves,
			Resists: m.Block.DamageResistances,
		})
	}
	return out
}

// levelFacts is the bare-levels party: enough to say who is at the table,
// too little to aim at anyone. The analysis degrades to statblock-only
// output and its caveat says so.
func levelFacts(party []int) []encounter.PCFact {
	if len(party) == 0 {
		return nil
	}
	out := make([]encounter.PCFact, 0, len(party))
	for i, lvl := range party {
		out = append(out, encounter.PCFact{Name: fmt.Sprintf("Character %d", i+1), Level: lvl})
	}
	return out
}

// rosterFacts resolves a roster against the mirrored bestiary, the
// homebrew overlay asked first when one is handed. Entries nothing resolves
// for are skipped and counted — the analysis prices only statblocks it
// has, and says what it left out.
func rosterFacts(cat *encounter.Catalog, hb *encounter.Overlay, monsters []encounter.Monster) (roster []encounter.Combatant, missing int) {
	if cat == nil && hb.Len() == 0 {
		return nil, len(monsters)
	}
	for _, m := range monsters {
		c, ok := cat.Lookup(m.Name, hb)
		if !ok {
			missing++
			continue
		}
		roster = append(roster, encounter.Combatant{Creature: c, Count: m.Count})
	}
	return roster, missing
}

// appendMissingCaveat records unresolved roster entries on the analysis.
func appendMissingCaveat(a *encounter.Analysis, missing int) {
	if a == nil || missing == 0 {
		return
	}
	a.Caveats = append(a.Caveats, fmt.Sprintf(
		"%d roster entries have no bestiary statblock and were not priced", missing))
}

// wavesForDerives the analysis' wave note from the declared objective: a
// survive fight's roster arrives over rounds, and the read says so.
func wavesForObjective(obj encounter.Objective) int {
	obj = obj.Normalized()
	if obj.Kind != encounter.Survive {
		return 0
	}
	return encounter.WavesFor(obj.Rounds)
}

// handleTactics computes the tactical read for a party and a roster. With a
// campaign named, the party block fills the facts (DM scope — a party sheet
// names what each character can still do); without one, bare levels degrade
// the read to the monsters' side, and the caveat says so.
func (s *Server) handleTactics(w http.ResponseWriter, r *http.Request) {
	if !s.encountersEnabled(w) {
		return
	}
	var req struct {
		Party      []int                `json:"party"`
		Monsters   []encounter.Monster  `json:"monsters"`
		CampaignID string               `json:"campaign_id"`
		Terrain    *encounter.Terrain   `json:"terrain"`
		Objective  *encounter.Objective `json:"objective"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	if err := validateParty(req.Party); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	monsters, err := normalizeMonsters(req.Monsters)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := validateObjective(req.Objective); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := validateTerrain(req.Terrain); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	party := levelFacts(req.Party)
	if strings.TrimSpace(req.CampaignID) != "" {
		table := s.campaignParty(w, r, req.CampaignID)
		if table == nil {
			return
		}
		if facts := partyFacts(table); len(facts) > 0 {
			party = facts
		}
	}

	roster, missing := rosterFacts(s.catalog, s.homebrewOverlay(r, strings.TrimSpace(req.CampaignID)), monsters)
	obj := encounter.Objective{}
	if req.Objective != nil {
		obj = *req.Objective
	}
	analysis := encounter.Analyze(encounter.TacticsInput{
		Party:   party,
		Roster:  roster,
		Terrain: req.Terrain,
		Waves:   wavesForObjective(obj),
	})
	appendMissingCaveat(analysis, missing)
	writeJSON(w, http.StatusOK, map[string]any{"tactics": analysis})
}

// tacticsBlock is the JSON shape both flows hand the UI: the derived
// analysis, and — from the design flow only — the model's gated prose
// beside it.
type tacticsBlock struct {
	*encounter.Analysis
	Prose *TacticsProse `json:"prose,omitempty"`
}

// withProse attaches the model's gated tactics prose to an analysis. The
// gate never fails the design: the analysis is server arithmetic and stands
// on its own, and the verdict rides beside the prose so the UI can say
// exactly which figure did not trace.
func withProse(in encounter.TacticsInput, a *encounter.Analysis, designProse string) tacticsBlock {
	block := tacticsBlock{Analysis: a}
	prose := encounter.SectionOf(designProse, "tactics")
	if strings.TrimSpace(prose) == "" {
		return block
	}
	out := &TacticsProse{Prose: prose, ProseStatus: "ok"}
	if violations := encounter.CheckTacticsProse(in, a, prose); len(violations) > 0 {
		out.ProseStatus = "rejected"
		out.Violations = violations
	}
	block.Prose = out
	return block
}
