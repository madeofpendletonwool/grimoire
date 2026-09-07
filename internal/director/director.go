// Package director is the state-aware encounter director (MAD-427,
// stage 10 of MAD-417): advisory monster tactics grounded in the live
// battle.
//
// With real mechanical state the director is genuinely smart instead of
// decorative: it knows the wizard is out of slots, the healer is down,
// the fighter saved on 2 hp — and suggests what these monsters would
// plausibly do next, with the state that justifies each suggestion
// riding beside it.
//
// Two rules make the output trustworthy, and both are enforced here
// rather than trusted to the model:
//
//   - Every suggestion cites its basis. The grounding assembles one
//     numbered basis list — statblock text the bestiary holds ([S#])
//     and live state off the tracker, the ledger and the effects
//     engine ([L#]) — and the gate drops any suggestion that cites
//     nothing. A suggestion without a basis never reaches the API.
//   - The model may not assert a number of its own. Every numeric
//     token in a suggestion must appear in the text of the basis lines
//     it cites — the same discipline the tactics prose gate applies to
//     the builder's write-up (encounter.CheckTacticsProse), sized to
//     per-suggestion citations.
//
// The director is advisory only, opt-in per request: it never rolls,
// never takes a turn, never mutates state. That is not a policy the
// handlers enforce — it is the shape of this package. The Service
// holds four read windows (the active battle, statblock lookup,
// derived balances, ongoing effects) and a model client; there is no
// dice handle, no write path, nothing to mutate with. Suggestions are
// returned in the response body and vanish if the DM does not act on
// them.
//
// MAD-318 owns the director's DM-screen surface; this package is the
// engine that surface will read. When that issue promotes, the merge
// proposal is one director, not two.
package director

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/combat"
	"github.com/madeofpendletonwool/grimoire/internal/effects"
	"github.com/madeofpendletonwool/grimoire/internal/encounter"
	"github.com/madeofpendletonwool/grimoire/internal/ledger"
)

/* ---------- the model window ---------- */

// Completion is one model response with its token accounting.
type Completion struct {
	Text         string
	InputTokens  int
	OutputTokens int
}

// ModelClient is the slice of the LLM surface the director needs: one
// non-streaming prompt exchange. The production adapter wraps the
// shared internal/llm client; tests replay scripted responses.
type ModelClient interface {
	// ModelName names the model for the response record.
	ModelName() string
	// Complete answers one system+user exchange.
	Complete(ctx context.Context, system, user string) (Completion, error)
}

/* ---------- the read windows (reads only, by construction) ---------- */

// Combats is the tracker's window on the director: the active battle
// and its order. (nil, nil, nil) is "no fight" — the tracker's own
// contract. The wide combat store satisfies it; so does any double.
type Combats interface {
	Active(ctx context.Context, campaignID string) (*combat.Combat, []combat.Combatant, error)
}

// Statblocks resolves a statblock name into the creature the bestiary
// holds — the same resolver the tracker itself starts battles through,
// so the director cites the statblocks the table is actually fighting.
type Statblocks interface {
	ResolveStatblock(ctx context.Context, owner, campaignID, name string) (encounter.Creature, bool)
}

// Ledger is the ledger's window: one character's derived balances —
// the "out of 3rd levels" facts the director is for.
type Ledger interface {
	Balances(ctx context.Context, campaignID, entityID string) ([]ledger.Balance, error)
}

// Effects is the duration engine's window: one target's ongoing rows
// and the campaign's concentration links.
type Effects interface {
	List(ctx context.Context, campaignID, targetID string, includeEnded bool) ([]effects.Row, error)
	Concentrations(ctx context.Context, campaignID string) ([]effects.Row, error)
}

// Service grounds and advises. It holds no mutable state; every call
// re-reads the battle and re-derives the basis.
type Service struct {
	combats    Combats
	statblocks Statblocks
	ledger     Ledger
	effects    Effects
	model      ModelClient
}

// New builds the director over the read windows. statblocks, ledger
// and effects may be nil — the grounding degrades to whatever it can
// read and says so in its caveats; the model is required for Advise
// and never for Ground.
func New(combats Combats, statblocks Statblocks, ledger Ledger, effects Effects, model ModelClient) *Service {
	return &Service{
		combats: combats, statblocks: statblocks, ledger: ledger,
		effects: effects, model: model,
	}
}

/* ---------- the grounding ---------- */

// Basis kinds: where a citable fact came from.
const (
	KindStatblock = "statblock" // text the bestiary holds
	KindState     = "state"     // live mechanical state
)

// Basis is one citable fact the director's suggestions may rest on:
// statblock text or live state, numbered for citation ([S1], [L2]).
type Basis struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Source string `json:"source"`
	Text   string `json:"text"`
}

// Grounding is the director's whole read of one battle: the citable
// basis, plus the frame lines the prompt leads with. It is assembled
// fresh per request; nothing here is stored.
type Grounding struct {
	CombatID string
	Name     string
	Round    int
	Turn     string // whose action it is; "" before the first turn
	Basis    []Basis
	Caveats  []string
}

// maxBasis bounds one grounding. Generous for a real table (a dozen
// combatants, a shelf of actions each); tight enough that a runaway
// roster cannot build an unbounded prompt.
const maxBasis = 160

// Ground assembles the basis for the campaign's active battle: the
// statblocks its foes and companions resolve to, and the live state of
// everyone in the order. The DM's user id scopes the homebrew overlay
// the statblock resolution reads — the same scope the tracker used to
// start the fight. IDs are numbered per kind after assembly (S1..,
// L1..) so the grouped prompt and the response cite the same lines.
func (s *Service) Ground(ctx context.Context, owner, campaignID string) (*Grounding, error) {
	if s.combats == nil {
		return nil, fmt.Errorf("director: no combat window")
	}
	fight, order, err := s.combats.Active(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	if fight == nil {
		return nil, fmt.Errorf("%w: no active battle to direct", campaign.ErrInvalid)
	}
	g := &Grounding{CombatID: fight.ID, Name: fight.Name, Round: fight.Round}
	if fight.TurnIndex >= 0 && fight.TurnIndex < len(order) {
		g.Turn = order[fight.TurnIndex].Name
	}

	var caveats []string
	add := func(b Basis) {
		if len(g.Basis) >= maxBasis {
			return
		}
		g.Basis = append(g.Basis, b)
	}

	s.statblockBasis(ctx, owner, campaignID, order, add, &caveats)
	s.stateBasis(ctx, campaignID, fight, order, add)
	if len(g.Basis) >= maxBasis {
		caveats = append(caveats, fmt.Sprintf("the basis was capped at %d lines", maxBasis))
	}
	g.Caveats = caveats

	si, li := 0, 0
	for i := range g.Basis {
		switch g.Basis[i].Kind {
		case KindStatblock:
			si++
			g.Basis[i].ID = fmt.Sprintf("S%d", si)
		default:
			li++
			g.Basis[i].ID = fmt.Sprintf("L%d", li)
		}
	}
	return g, nil
}

// statblockBasis cites the statblocks the fight is made of: one line
// per distinct statblock summary, then its actions and traits as the
// bestiary holds them. Resolution runs through the same resolver the
// tracker started the battle with; a name that no longer resolves is a
// caveat, never a guess.
func (s *Service) statblockBasis(ctx context.Context, owner, campaignID string, order []combat.Combatant, add func(Basis), caveats *[]string) {
	if s.statblocks == nil {
		*caveats = append(*caveats, "no statblock window — the grounding carries live state only")
		return
	}
	seen := map[string]bool{}
	for _, c := range order {
		if c.Kind == combat.KindPC {
			continue // a sheet, not a statblock
		}
		name := c.Snapshot.Statblock
		if name == "" {
			name = c.Name
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		creature, ok := s.statblocks.ResolveStatblock(ctx, owner, campaignID, name)
		if !ok {
			*caveats = append(*caveats, fmt.Sprintf("%q no longer resolves against the bestiary — cited from the live state only", name))
			continue
		}
		add(statblockSummaryBasis(creature))
		for _, t := range creature.Traits {
			add(Basis{Kind: KindStatblock, Source: name + " — " + t.Name, Text: t.Name + ": " + t.Desc})
		}
		for _, a := range creature.Actions {
			add(Basis{Kind: KindStatblock, Source: name + " — " + a.Name, Text: actionText(a)})
		}
		if creature.LairAction {
			add(Basis{Kind: KindStatblock, Source: name + " — lair actions",
				Text: name + " has lair actions; they fire on initiative count 20, losing ties"})
		}
	}
}

// statblockSummaryBasis is one statblock's numbers line: what the
// bestiary holds for the name the fight carries.
func statblockSummaryBasis(creature encounter.Creature) Basis {
	name := creature.Name
	var b strings.Builder
	fmt.Fprintf(&b, "%s: CR %s", name, creature.CR)
	if creature.XP > 0 {
		fmt.Fprintf(&b, " (%d XP)", creature.XP)
	}
	if creature.AC > 0 {
		fmt.Fprintf(&b, ", AC %d", creature.AC)
	}
	if creature.HP > 0 {
		fmt.Fprintf(&b, ", HP %d", creature.HP)
	}
	if len(creature.Speeds) > 0 {
		fmt.Fprintf(&b, ", speed %s", speedLine(creature.Speeds))
	}
	for _, def := range []struct{ label, val string }{
		{"resistant to", creature.Resist}, {"immune to", creature.Immune}, {"vulnerable to", creature.Vulnerable},
	} {
		if strings.TrimSpace(def.val) != "" {
			fmt.Fprintf(&b, ", %s %s", def.label, def.val)
		}
	}
	return Basis{Kind: KindStatblock, Source: name, Text: b.String()}
}

// actionText renders one statblock action with its usage grammar —
// the line a suggestion would quote.
func actionText(a encounter.NamedText) string {
	label := a.Name
	var tags []string
	if a.Kind != "" && a.Kind != "ACTION" {
		tags = append(tags, strings.ToLower(strings.ReplaceAll(a.Kind, "_", " ")))
	}
	if u := strings.TrimSpace(a.Usage); u != "" {
		tags = append(tags, u)
	}
	if len(tags) > 0 {
		label += " (" + strings.Join(tags, ", ") + ")"
	}
	return label + ": " + a.Desc
}

// speedLine renders a creature's speeds compactly and in a stable
// order (walk first, then the rest alphabetically).
func speedLine(speeds map[string]int) string {
	if len(speeds) == 0 {
		return ""
	}
	keys := make([]string, 0, len(speeds))
	for k := range speeds {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i] == "walk" {
			return true
		}
		if keys[j] == "walk" {
			return false
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d ft", k, speeds[k]))
	}
	return strings.Join(parts, ", ")
}

// stateBasis cites the live battle: the round and turn, every
// combatant's current numbers, and — for the party's entity-backed
// members — the ledger's derived balances and the effects engine's
// ongoing rows. This is the half that makes the director state-aware:
// the wizard's empty slots and the healer's death saves are basis
// lines like any other, citable and gated.
func (s *Service) stateBasis(ctx context.Context, campaignID string, fight *combat.Combat, order []combat.Combatant, add func(Basis)) {
	src := "live state"
	if fight.TurnIndex >= 0 && fight.TurnIndex < len(order) {
		add(Basis{Kind: KindState, Source: src,
			Text: fmt.Sprintf("Round %d, and it is %s's turn", fight.Round, order[fight.TurnIndex].Name)})
	} else {
		add(Basis{Kind: KindState, Source: src,
			Text: fmt.Sprintf("Round %d; the order is rolled and nobody has acted yet", fight.Round)})
	}

	conc := map[string]string{}
	if s.effects != nil {
		if rows, err := s.effects.Concentrations(ctx, campaignID); err == nil {
			for _, r := range rows {
				if r.Status == effects.StatusActive {
					conc[r.SourceID] = r.Name
				}
			}
		}
	}

	for _, c := range order {
		add(Basis{Kind: KindState, Source: src + " — " + c.Name, Text: combatantState(c, conc[c.EntityID])})
		if c.Kind == combat.KindPC && c.EntityID != "" && s.ledger != nil {
			if balances, err := s.ledger.Balances(ctx, campaignID, c.EntityID); err == nil {
				for _, line := range balanceLines(balances) {
					add(Basis{Kind: KindState, Source: src + " — " + c.Name, Text: line})
				}
			}
		}
		if s.effects != nil && c.EntityID != "" {
			if rows, err := s.effects.List(ctx, campaignID, c.EntityID, false); err == nil {
				for _, r := range rows {
					if r.Status != effects.StatusActive {
						continue
					}
					add(Basis{Kind: KindState, Source: src + " — " + c.Name,
						Text: fmt.Sprintf("%s is under an ongoing effect: %s", c.Name, r.Name)})
				}
			}
		}
	}
}

// combatantState is one combatant's live line: the numbers the tracker
// holds right now.
func combatantState(c combat.Combatant, concentrating string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s, %s): HP %d/%d", c.Name, c.Kind, c.Side, c.HP, c.EffectiveMax())
	if c.TempHP > 0 {
		fmt.Fprintf(&b, " plus %d temp", c.TempHP)
	}
	if c.HP > 0 && c.EffectiveMax() > 0 && c.HP*2 <= c.EffectiveMax() {
		b.WriteString(", bloodied")
	}
	switch {
	case c.Dead:
		b.WriteString(", dead")
	case c.Downed && c.Stable:
		b.WriteString(", downed and stable")
	case c.Downed:
		fmt.Fprintf(&b, ", downed and dying (%d death save failures, %d successes)", c.DeathFailures, c.DeathSuccesses)
	}
	for _, cond := range c.Conditions {
		if cond.Concentration {
			fmt.Fprintf(&b, ", %s (concentration)", cond.Name)
		} else {
			fmt.Fprintf(&b, ", %s", cond.Name)
		}
	}
	if concentrating != "" {
		fmt.Fprintf(&b, ", concentrating on %s", concentrating)
	}
	if c.ReactionSpent {
		b.WriteString(", reaction already spent this round")
	}
	if c.Snapshot.LegendaryMax > 0 {
		fmt.Fprintf(&b, ", %d of %d legendary actions left",
			c.Snapshot.LegendaryMax-c.LegendaryUsed, c.Snapshot.LegendaryMax)
	}
	return b.String()
}

// balanceLines renders the ledger's derived balances a director cares
// about mid-fight: every spell level (casters' slots are the question
// the DM is always asking), and any other pool that is no longer full.
// The hp pool and currency stay out — the tracker owns hp in a fight,
// and gold does not aim a scimitar.
func balanceLines(balances []ledger.Balance) []string {
	var out []string
	for _, bal := range balances {
		switch bal.Pool.Kind {
		case ledger.KindHP, ledger.KindCurrency:
			continue
		case ledger.KindSlot:
			out = append(out, fmt.Sprintf("%s-level spell slots: %d of %d left",
				ordinal(bal.Pool.Name), bal.Current, bal.Pool.Size))
		default:
			if bal.Pool.Size > 0 && bal.Current >= bal.Pool.Size {
				continue // full pools are not news
			}
			if bal.Pool.Size > 0 {
				out = append(out, fmt.Sprintf("%s: %d of %d left", bal.Pool.DisplayName(), bal.Current, bal.Pool.Size))
			} else {
				out = append(out, fmt.Sprintf("%s: %d spent", bal.Pool.DisplayName(), bal.Spent))
			}
		}
	}
	return out
}

// ordinal renders a slot pool's numeric name as an ordinal: "1"
// becomes "1st", "2" "2nd". Anything unparseable rides as it is.
func ordinal(name string) string {
	n, ok := atoiSafe(name)
	if !ok {
		return name
	}
	switch {
	case n%100 >= 11 && n%100 <= 13:
		return fmt.Sprintf("%dth", n)
	case n%10 == 1:
		return fmt.Sprintf("%dst", n)
	case n%10 == 2:
		return fmt.Sprintf("%dnd", n)
	case n%10 == 3:
		return fmt.Sprintf("%drd", n)
	default:
		return fmt.Sprintf("%dth", n)
	}
}

func atoiSafe(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
		if n > 1_000 {
			return 0, false
		}
	}
	return n, true
}

/* ---------- the prompt ---------- */

// directorTimeout bounds one advisory call end to end.
const directorTimeout = 2 * time.Minute

// SystemPrompt is the standing instruction: grounded, cited, no
// invented numbers, advisory only. Behaviour, never secrecy — the
// basis list below is the whole context, and the DM gate already held
// it back from every other reader.
func SystemPrompt() string {
	var b strings.Builder
	b.WriteString(`You are the encounter director advising a Dungeon Master mid-battle: you suggest what the monsters would plausibly do next. The DM decides everything — you never roll, never take a turn, never change state; you advise.

GROUNDING RULES — follow these strictly:
1. Ground every suggestion in the numbered basis lines provided: statblock facts are [S#], live battle state is [L#]. Suggest only moves the cited lines support — a monster may use only the actions, traits and numbers its statblock carries, aimed at targets the live state shows.
2. Each suggestion must cite at least one basis id, and every citation must be a real id from the list. A suggestion with no valid citation is discarded.
3. Assert no number of your own: every number in a suggestion must appear in the text of a basis line it cites. Paraphrase freely; invent no figures, ranges, hit bonuses or hit-point counts.
4. Two to four suggestions, each for a concrete monster or monster group, each one usable at the table in a sentence or two: what it does, at whom, and why — the why tied to the cited state.
5. No preamble about being an advisor. The reply is a fenced json block and nothing else, of exactly this shape:
` + "```json\n{\"suggestions\": [{\"actor\": \"Goblin 1\", \"action\": \"...\", \"reasoning\": \"...\", \"basis\": [\"S3\", \"L2\"]}]}\n```")
	return b.String()
}

// UserMessage assembles the prompt turn: the basis, grouped, then the
// DM's question when one rode along. This is the exact text the model
// receives; it stays pure so tests can read it.
func UserMessage(g *Grounding, question string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== THE BATTLE: %s (round %d", g.Name, g.Round)
	if g.Turn != "" {
		fmt.Fprintf(&b, ", %s to act", g.Turn)
	}
	b.WriteString(") ===\n\n")

	b.WriteString("=== STATBLOCK FACTS ([S#] — the bestiary's own text) ===\n")
	statblocks := 0
	for _, line := range g.Basis {
		if line.Kind == KindStatblock {
			fmt.Fprintf(&b, "  [%s] %s\n", line.ID, line.Text)
			statblocks++
		}
	}
	if statblocks == 0 {
		b.WriteString("  (no statblock lines resolved — ground in the live state alone)\n")
	}

	b.WriteString("\n=== LIVE STATE ([L#] — the battle as it stands right now) ===\n")
	states := 0
	for _, line := range g.Basis {
		if line.Kind == KindState {
			fmt.Fprintf(&b, "  [%s] %s\n", line.ID, line.Text)
			states++
		}
	}
	if states == 0 {
		b.WriteString("  (no live state lines)\n")
	}

	for _, c := range g.Caveats {
		fmt.Fprintf(&b, "\nCaveat: %s\n", c)
	}
	if q := strings.TrimSpace(question); q != "" {
		fmt.Fprintf(&b, "\nThe DM asks: %s\n", q)
	}
	return b.String()
}

/* ---------- the advisory call ---------- */

// Result is one advisory pass: the suggestions that survived the gate
// and how many were dropped.
type Result struct {
	Suggestions []Suggestion
	Dropped     int
}

// Suggestion is one gated advisory move: the model's actor, action
// and reasoning, with the basis ids it cited.
type Suggestion struct {
	Actor     string   `json:"actor"`
	Action    string   `json:"action"`
	Reasoning string   `json:"reasoning"`
	BasisIDs  []string `json:"basis"`
}

// ModelName names the model behind the advisory passes, for the
// response record; "" when none is wired.
func (s *Service) ModelName() string {
	if s.model == nil {
		return ""
	}
	return s.model.ModelName()
}

// Advise runs one advisory pass: the model writes suggestions over the
// grounding, and the gate decides which survive. Fail-closed — a model
// reply that cites nothing yields an empty result, never an ungated
// suggestion.
func (s *Service) Advise(ctx context.Context, g *Grounding, question string) (Result, error) {
	if s.model == nil {
		return Result{}, fmt.Errorf("director: no model wired")
	}
	ctx, cancel := context.WithTimeout(ctx, directorTimeout)
	defer cancel()
	raw, err := s.model.Complete(ctx, SystemPrompt(), UserMessage(g, question))
	if err != nil {
		return Result{}, err
	}
	suggestions, dropped, err := Gate(g, raw.Text)
	if err != nil {
		return Result{Dropped: dropped}, err
	}
	return Result{Suggestions: suggestions, Dropped: dropped}, nil
}
