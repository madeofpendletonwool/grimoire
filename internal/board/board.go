// Package board is the party board (MAD-423, stage 6 of MAD-417): the
// table's live view of the party's mechanical state, over the campaign
// pub/sub (internal/pubsub).
//
// The board reads what Stages 1–5 built — sheets and their projection,
// the resource ledger's derived balances, the effect engine's conditions
// and concentration, the combat tracker's in-fight state — and renders
// one strip per party member. It is a read surface: nothing here writes
// mechanical state, and the stores it reads are reached only through the
// narrow interfaces below, the knowledge layer's PlayerView pattern
// applied to mechanics — a leaky read cannot be written, not just should
// not be.
//
// Visibility is data on the campaign (the board key of the settings
// payload), and the server enforces it by construction: a hidden field is
// never placed on the view struct, so it is absent from the JSON — never
// zeroed, never merely hidden in a client. Three parties read the same
// endpoints and get three shapes:
//
//	the DM          every number, every member, plus the monster side
//	a player        the party's strips under the campaign's config —
//	                exact or word HP, slots visible or private — with
//	                their own character's numbers always exact
//	an observer     the config's shape, no self exception (no self)
//
// The health-word mode replaces numbers with the table's own vocabulary
// — unhurt, hurt, bloodied, down, dead — declared here, not free text.
package board

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/combat"
	"github.com/madeofpendletonwool/grimoire/internal/effects"
	"github.com/madeofpendletonwool/grimoire/internal/ledger"
	"github.com/madeofpendletonwool/grimoire/internal/pubsub"
)

// SettingsKey is where the config lives on the campaign's settings
// payload: settings["board"] = {"hp": "exact"|"word", "slots":
// "visible"|"private"}. The first typed key the free-form payload
// carries; validation is strict, absence is the default.
const SettingsKey = "board"

/* ---------- the narrow reads (the PlayerView pattern, mechanically) ---------- */

// Ledger is the ledger's whole window on the board: derived balances
// and the hp pool's bridge read, nothing else. The wide store satisfies
// it; so does any test double.
type Ledger interface {
	Balances(ctx context.Context, campaignID, entityID string) ([]ledger.Balance, error)
	HPBalance(ctx context.Context, campaignID, entityID string) (current, max int, ok bool)
}

// Effects is the duration engine's window: one target's active ongoing
// rows, and the campaign's concentration links.
type Effects interface {
	List(ctx context.Context, campaignID, targetID string, includeEnded bool) ([]effects.Row, error)
	Concentrations(ctx context.Context, campaignID string) ([]effects.Row, error)
}

// Combats is the tracker's window: the active battle and its order.
// (nil, nil, nil) is "no fight" — the tracker's own contract.
type Combats interface {
	Active(ctx context.Context, campaignID string) (*combat.Combat, []combat.Combatant, error)
}

// Users resolves member ids to the names the table knows them by.
type Users interface {
	Usernames(ctx context.Context, ids []string) (map[string]string, error)
}

// Store builds board snapshots. Construct with New and wire it once; it
// holds no mutable state beyond the shared broker.
type Store struct {
	campaigns *campaign.Store
	ledger    Ledger
	effects   Effects
	combats   Combats
	users     Users
	broker    *pubsub.Broker
}

// New builds a board store over the mechanical stores' read windows.
// ledger, effects and combats may be nil — a board running without one
// of them renders what it has (nil tests wire partial tables).
func New(campaigns *campaign.Store, ledgerStore Ledger, effectStore Effects, combatStore Combats, users Users) (*Store, error) {
	if campaigns == nil {
		return nil, fmt.Errorf("board: the campaign store is required")
	}
	return &Store{
		campaigns: campaigns, ledger: ledgerStore, effects: effectStore,
		combats: combatStore, users: users, broker: pubsub.New(),
	}, nil
}

// WithBroker moves the store onto a shared campaign broker (MAD-423).
func (s *Store) WithBroker(b *pubsub.Broker) *Store {
	if b != nil {
		s.broker = b
	}
	return s
}

// SubscribeAs is the stream the board surface holds open: a wake channel
// for the campaign topic and a presence entry for the user. Present
// counts presence (who is at the table); the dice feed's anonymous
// subscriptions do not.
func (s *Store) SubscribeAs(campaignID, userID string) (<-chan struct{}, func()) {
	return s.broker.SubscribeAs(campaignID, userID)
}

// Notify pings a campaign's streams — the settings handler after a
// config change, the stream handler on a presence change.
func (s *Store) Notify(campaignID string) {
	s.broker.Notify(campaignID)
}

// Present lists the usernames with a board stream open on this campaign.
func (s *Store) Present(ctx context.Context, campaignID string) []string {
	ids := s.broker.Present(campaignID)
	if len(ids) == 0 || s.users == nil {
		return ids
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	names, err := s.users.Usernames(ctx, ids)
	if err != nil {
		return ids
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if n, ok := names[id]; ok && n != "" {
			out = append(out, n)
			continue
		}
		out = append(out, id)
	}
	return out
}

/* ---------- the standing */

// Standing is the viewer's resolved position in the campaign — what
// resolveCampaignAccess learned from the member row, handed to the
// builder. The builder trusts it and nothing else.
type Standing struct {
	DM bool
	// Own is the set of character ids the viewer plays (players bound
	// to a pc; observers and the DM carry none). A player's own strip
	// is always exact: 5e players know their own numbers.
	Own map[string]bool
}

// DMStanding is the DM's (or keeper's) standing.
func DMStanding() Standing { return Standing{DM: true} }

// PlayerStanding is a member's: exact on their own bound characters.
func PlayerStanding(characterIDs ...string) Standing {
	own := make(map[string]bool, len(characterIDs))
	for _, id := range characterIDs {
		own[id] = true
	}
	return Standing{Own: own}
}

/* ---------- the views ---------- */

// SlotLevel is one spell level's summary: what is left of what size.
type SlotLevel struct {
	Level int `json:"level"`
	Left  int `json:"left"`
	Max   int `json:"max"`
}

// ConditionView is one ongoing thing on a character: the poisoned, the
// bless, the hunter's mark — with its countdown display and whether it
// holds the character's concentration.
type ConditionView struct {
	Name          string `json:"name"`
	Kind          string `json:"kind,omitempty"`
	Display       string `json:"display,omitempty"`
	Concentration bool   `json:"concentration,omitempty"`
}

// Member is one strip. Fields the campaign's config hides — or that are
// another player's private numbers — are absent from the JSON, never
// zeroed: the builder never constructs them.
type Member struct {
	CharacterID   string          `json:"character_id"`
	Name          string          `json:"name"`
	Classes       string          `json:"classes,omitempty"`
	Level         int             `json:"level,omitempty"`
	AC            int             `json:"ac,omitempty"`
	Present       bool            `json:"present"`
	Unstructured  bool            `json:"unstructured,omitempty"`
	Health        string          `json:"health,omitempty"` // the word — word mode, and always for the downed
	Down          bool            `json:"down,omitempty"`
	Dead          bool            `json:"dead,omitempty"`
	HP            *int            `json:"hp,omitempty"`      // exact mode only (and always the DM's)
	TempHP        *int            `json:"temp_hp,omitempty"` // mid-combat temp hp, exact mode only
	MaxHP         *int            `json:"max_hp,omitempty"`  // exact mode only
	Conditions    []ConditionView `json:"conditions,omitempty"`
	Concentrating string          `json:"concentrating,omitempty"` // what the character holds
	Slots         []SlotLevel     `json:"slots,omitempty"`         // visible mode, or the viewer's own
	Warnings      []string        `json:"warnings,omitempty"`      // word-safe in word mode
}

// TurnEntry is one line of the initiative order — names and positions,
// the public shape of the battle. Numbers stay on the DM's monster
// side; initiative itself was rolled in the open.
type TurnEntry struct {
	Name       string `json:"name"`
	Initiative int    `json:"initiative"`
	PC         bool   `json:"pc"`
	Active     bool   `json:"active"`
	Down       bool   `json:"down,omitempty"`
	Dead       bool   `json:"dead,omitempty"`
}

// CombatView is the party-safe battle summary: the round, whose turn it
// is, the order. The monster side's numbers are the DM's alone until a
// later stage chooses to reveal them (MAD-425's table screen).
type CombatView struct {
	Name  string      `json:"name"`
	Round int         `json:"round"`
	Turn  string      `json:"turn"` // whose action it is
	Order []TurnEntry `json:"order"`
}

// MonsterView is one foe on the DM's board — the monster side, exact.
// ID and CombatID ride along for the table screen's reveal toggle
// (MAD-425): which combatant row, in which battle. Reveal says what
// (if anything) the room may read of it. This view is the DM's alone —
// the public shape is the table screen's own, built from the reveal
// flag and never from here.
type MonsterView struct {
	ID         string          `json:"id"`
	CombatID   string          `json:"combat_id"`
	Name       string          `json:"name"`
	AC         int             `json:"ac,omitempty"`
	HP         int             `json:"hp"`
	MaxHP      int             `json:"max_hp"`
	TempHP     int             `json:"temp_hp,omitempty"`
	Down       bool            `json:"down,omitempty"`
	Dead       bool            `json:"dead,omitempty"`
	Reveal     string          `json:"reveal,omitempty"`
	Conditions []ConditionView `json:"conditions,omitempty"`
}

// Snapshot is one board read: the config as this table set it, the
// strips, presence, and — when a fight runs — the public battle plus,
// for the DM, the monster side. Snapshots carry no timestamps: two
// identical reads serialize identically, which is what the stream's
// change detection compares.
type Snapshot struct {
	CampaignID string        `json:"campaign_id"`
	Config     Config        `json:"config"`
	DM         bool          `json:"dm"`
	Members    []Member      `json:"members"`
	Present    []string      `json:"present"`
	Combat     *CombatView   `json:"combat,omitempty"`
	Monsters   []MonsterView `json:"monsters,omitempty"` // the DM's read
}

/* ---------- the builder ---------- */

// Snapshot assembles the board for one viewer. The mechanical truth is
// gathered once (DM-wide); the viewer's shape is decided per member
// while the view structs are built — the enforcement point.
func (s *Store) Snapshot(ctx context.Context, campaignID string, viewer Standing) (*Snapshot, error) {
	c, err := s.campaigns.GetCampaign(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	cfg := ConfigOf(c.Settings[SettingsKey])
	members, err := s.campaigns.Members(ctx, campaignID)
	if err != nil {
		return nil, err
	}

	// One presence set: characters whose player is connected map from
	// the broker's user ids through the member rows.
	presentUsers := make(map[string]bool)
	for _, u := range s.broker.Present(campaignID) {
		presentUsers[u] = true
	}

	snap := &Snapshot{
		CampaignID: campaignID, Config: cfg, DM: viewer.DM,
		Members: []Member{}, Present: s.Present(ctx, campaignID),
	}

	// The in-fight state, when a battle runs: one map by entity for the
	// party side, the order for the summary, the foe side for the DM.
	var live map[string]*combat.Combatant
	var fight *combat.Combat
	var order []combat.Combatant
	if s.combats != nil {
		fight, order, err = s.combats.Active(ctx, campaignID)
		if err != nil {
			return nil, err
		}
		if fight != nil {
			live = make(map[string]*combat.Combatant, len(order))
			for i := range order {
				if order[i].EntityID != "" {
					live[order[i].EntityID] = &order[i]
				}
			}
		}
	}

	// The strips: one per bound character (deduped — a character two
	// members bound is one body), plus unbound members rendered as
	// presence rows the table can still see.
	seen := make(map[string]bool)
	for _, m := range members {
		if m.CharacterID == "" {
			continue
		}
		if seen[m.CharacterID] {
			continue
		}
		seen[m.CharacterID] = true
		strip, err := s.member(ctx, campaignID, m.CharacterID, viewer, cfg, live, presentUsers, members)
		if err != nil {
			continue // a deleted or unreadable character is a gap, not a 500
		}
		snap.Members = append(snap.Members, *strip)
	}

	// The battle's public shape, plus the DM's monster side.
	if fight != nil {
		cv := &CombatView{Name: fight.Name, Round: fight.Round}
		turn := fight.TurnIndex
		for i := range order {
			c := order[i]
			cv.Order = append(cv.Order, TurnEntry{
				Name: c.Name, Initiative: c.Initiative,
				PC:     c.Kind == combat.KindPC || c.Kind == combat.KindCompanion,
				Active: i == turn, Down: c.Downed && !c.Dead, Dead: c.Dead,
			})
			if i == turn {
				cv.Turn = c.Name
			}
			if viewer.DM && c.Side == combat.SideFoe {
				snap.Monsters = append(snap.Monsters, MonsterView{
					ID: c.ID, CombatID: fight.ID, Name: c.Name, AC: c.AC, HP: c.HP, MaxHP: c.EffectiveMax(),
					TempHP: c.TempHP, Down: c.Downed && !c.Dead, Dead: c.Dead,
					Reveal: c.Reveal, Conditions: combatConditions(c),
				})
			}
		}
		snap.Combat = cv
	}
	return snap, nil
}

// member builds one strip, applying the visibility rules while the
// struct is constructed. The DM sees every number; a player sees the
// config's shape with their own character always exact; observers see
// the config's shape.
func (s *Store) member(ctx context.Context, campaignID, characterID string, viewer Standing, cfg Config, live map[string]*combat.Combatant, presentUsers map[string]bool, members []campaign.Member) (*Member, error) {
	e, err := s.campaigns.GetEntity(ctx, campaign.ScopeDM, campaignID, characterID)
	if err != nil {
		return nil, err
	}
	m := &Member{
		CharacterID: characterID, Name: e.Name,
		Present: characterPresent(characterID, presentUsers, members),
	}
	if e.Status == campaign.StatusDeleted {
		return m, nil
	}

	// The sheet's queryable numbers — level, the classes label, ac —
	// off the payload's typed sheet block (the projection's own
	// spelling, re-derived here so the board reads one truth).
	var structured bool
	if sh, ok, err := campaign.SheetOf(e); err == nil && ok && sh.MaxHP > 0 {
		structured = true
		m.Classes, m.Level, m.AC = sh.ClassesLabel(), sh.TotalLevel(), sh.AC
	}
	m.Unstructured = !structured

	// Where current hp lives: the fight while one runs, the ledger's
	// pool between fights.
	hp, temp, maxHP, downed, dead := s.hpOf(ctx, campaignID, characterID, live, e)

	exact := viewer.DM || cfg.HP == HPExact || viewer.Own[characterID]
	if exact {
		if maxHP > 0 {
			m.HP, m.MaxHP = &hp, &maxHP
			if temp > 0 {
				m.TempHP = &temp
			}
		}
	} else {
		// Word mode, someone else's strip: the word only. Down and dead
		// are words the table speaks aloud; they stay.
		if maxHP > 0 {
			m.Health = HealthWord(hp, maxHP, downed, dead)
		}
	}
	m.Down = downed && !dead
	m.Dead = dead

	// Conditions and concentration — public table state.
	if s.effects != nil {
		if rows, err := s.effects.List(ctx, campaignID, characterID, false); err == nil {
			for _, r := range rows {
				if r.Status != effects.StatusActive {
					continue
				}
				m.Conditions = append(m.Conditions, ConditionView{
					Name: r.Name, Kind: r.Kind, Display: r.Running.Display(),
					Concentration: r.Concentration,
				})
			}
		}
		if conc, err := s.effects.Concentrations(ctx, campaignID); err == nil {
			for _, r := range conc {
				if r.SourceID == characterID {
					m.Concentrating = r.Name
					break
				}
			}
		}
	}

	// Slots and warnings, from the ledger's derived balances — visible
	// per config, or always on the viewer's own strip (and the DM's).
	if s.ledger != nil {
		balances, err := s.ledger.Balances(ctx, campaignID, characterID)
		if err == nil {
			slotsVisible := viewer.DM || cfg.Slots == SlotsVisible || viewer.Own[characterID]
			hpVisible := exact && maxHP > 0
			m.Slots, m.Warnings = summarize(balances, slotsVisible, hpVisible, hp, maxHP)
		}
	}
	return m, nil
}

// hpOf resolves a character's current hit points: the live combat row
// while a fight runs (the tracker is the fast state), the ledger's hp
// pool between fights (derived, never stored — MAD-423's bridge), and
// the sheet's max when neither exists.
func (s *Store) hpOf(ctx context.Context, campaignID, characterID string, live map[string]*combat.Combatant, e *campaign.Entity) (hp, temp, max int, downed, dead bool) {
	if c, ok := live[characterID]; ok {
		return c.HP, c.TempHP, c.EffectiveMax(), c.Downed, c.Dead
	}
	if s.ledger != nil {
		if cur, size, ok := s.ledger.HPBalance(ctx, campaignID, characterID); ok {
			return cur, 0, size, cur <= 0, false
		}
	}
	// No pool, no fight: the sheet's max is all the truth there is.
	if sh, ok, err := campaign.SheetOf(e); err == nil && ok {
		return sh.MaxHP, 0, sh.MaxHP, false, false
	}
	return 0, 0, 0, false, false
}

/* ---------- small helpers ---------- */

// characterPresent reports whether the character's bound member is
// connected.
func characterPresent(characterID string, presentUsers map[string]bool, members []campaign.Member) bool {
	for _, m := range members {
		if m.CharacterID == characterID {
			if presentUsers[m.UserID] {
				return true
			}
		}
	}
	return false
}

// combatConditions renders a combatant's in-fight conditions (the
// tracker's own rows, MAD-422) into board views.
func combatConditions(c combat.Combatant) []ConditionView {
	var out []ConditionView
	for _, cond := range c.Conditions {
		out = append(out, ConditionView{Name: cond.Name, Kind: "condition"})
	}
	return out
}

// summarize renders slot levels and pool warnings from derived
// balances. hpVisible decides whether the hp warning may carry a number
// (word mode never does — the health word carries the news).
func summarize(balances []ledger.Balance, slotsVisible, hpVisible bool, hp, max int) ([]SlotLevel, []string) {
	var slots []SlotLevel
	var warnings []string
	for _, b := range balances {
		switch b.Pool.Kind {
		case ledger.KindSlot:
			if slotsVisible {
				lvl, _ := strconv.Atoi(b.Pool.Name)
				slots = append(slots, SlotLevel{Level: lvl, Left: b.Current, Max: b.Pool.Size})
			}
			if slotsVisible && b.Pool.Size > 0 && b.Current == 0 {
				warnings = append(warnings, "out of "+b.Pool.DisplayName())
			}
		case ledger.KindFeature:
			if slotsVisible && b.Pool.Size > 0 && b.Current == 0 {
				warnings = append(warnings, "out of "+b.Pool.DisplayName())
			}
		}
	}
	if hpVisible && max > 0 && hp > 0 && hp*4 <= max {
		warnings = append(warnings, fmt.Sprintf("%d hp", hp))
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].Level < slots[j].Level })
	return slots, warnings
}
