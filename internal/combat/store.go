package combat

// The tracker's rows and orchestration (MAD-422). The pure state machine
// is combat.go; this file is everything that touches a database, the
// dice engine and the session log:
//
//   - Start resolves the lineup — pcs from their sheets, monsters and
//     companions through the statblock resolver (the bestiary mirror
//     plus the campaign's homebrew) — rolls initiative for everyone
//     through the dice engine so every number on the rows has a stored
//     roll behind it, breaks ties the way the table does (re-roll until
//     the order is distinct), and persists the battle with the turn
//     counter parked before the first turn.
//   - Every mid-play write (damage, healing, death saves, legendary
//     actions, conditions) applies the pure rule, persists the row, and
//     appends one combat_log entry — the journal under the state — with
//     a kind 'combat' mirror in the live session's log, the same
//     write-your-own-table-then-append-a-session-event pattern the dice
//     store established.
//   - NextTurn is the on-turn automation: the outgoing combatant's turn
//     ends (legendary budget back), the round wraps when the order does
//     (durations wear a round on both engines — the effects store for
//     entity-backed rows, the local list for statblock-backed ones —
//     and the lair reminder fires on the count-20 crossing), and the
//     incoming combatant's turn starts (reaction back, recharge and
//     death-save prompts surfaced).
//
// Permission rules live in the server file; this store trusts its
// caller's scope decision and validates shape only.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/dice"
	"github.com/madeofpendletonwool/grimoire/internal/effects"
	"github.com/madeofpendletonwool/grimoire/internal/encounter"
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
	"github.com/madeofpendletonwool/grimoire/internal/pubsub"
)

// StatblockResolver resolves a statblock name into the creature the
// fight will use: the campaign's homebrew first (the DM's design is the
// more specific answer about their table), the SRD mirror second. The
// server wires the adapter over the catalog and the homebrew store;
// tests wire a map.
type StatblockResolver interface {
	ResolveStatblock(ctx context.Context, owner, campaignID, name string) (encounter.Creature, bool)
}

// HitPoints is the ledger's window on one character's hit points — the
// bridge MAD-423 built so the tracker and the party board read one truth
// outside a fight. Current hp is a derived pool balance (never stored),
// and writing it back is an ordinary 'set' transaction with provenance.
// The ledger store implements it; ok is false when the character tracks
// no hp pool (no structured sheet), which the caller treats as
// "sheet-fresh".
type HitPoints interface {
	// HPBalance reports the character's current and maximum hit points.
	HPBalance(ctx context.Context, campaignID, entityID string) (current, max int, ok bool)
	// SetHP records the character's current hit points as one visible
	// transaction. A character with no hp pool is a no-op.
	SetHP(ctx context.Context, campaignID, entityID string, amount int, actor, note string) error
}

// maxCombatants bounds one battle's lineup: generous for any real table,
// tight enough that a pasted list is an error, not a fog.
const maxCombatants = 50

// tiebreakRounds bounds the re-roll loop: five rounds of d20s settle any
// honest tie; after that the order sort's determinism takes over.
const tiebreakRounds = 5

// Store runs the combat tracker over the shared database handle.
type Store struct {
	db        *sql.DB
	campaigns *campaign.Store
	sessions  *gamesession.Store
	dice      *dice.Store
	effects   *effects.Store
	resolver  StatblockResolver
	hp        HitPoints
	broker    *pubsub.Broker
	now       func() time.Time
}

// New builds a combat store. The dice engine is required — initiative
// is rolls, and every number on a combatant row keeps its provenance.
// The effects engine (durations on entity-backed combatants) and the
// statblock resolver (monster and companion lookups) wire optionally;
// without the resolver, combats start with pcs only.
func New(db *sql.DB, campaigns *campaign.Store, sessions *gamesession.Store, roller *dice.Store) (*Store, error) {
	if db == nil {
		return nil, errors.New("combat: nil database handle")
	}
	if campaigns == nil {
		return nil, errors.New("combat: the campaign store is required")
	}
	if sessions == nil {
		return nil, errors.New("combat: the session store is required")
	}
	if roller == nil {
		return nil, errors.New("combat: the dice engine is required")
	}
	return &Store{db: db, campaigns: campaigns, sessions: sessions, dice: roller, now: time.Now().UTC}, nil
}

// WithEffects wires the duration engine's combat clock.
func (s *Store) WithEffects(store *effects.Store) *Store {
	s.effects = store
	return s
}

// WithResolver wires statblock lookup for monster and companion lines.
func (s *Store) WithResolver(r StatblockResolver) *Store {
	s.resolver = r
	return s
}

// WithHitPoints wires the ledger's hp bridge (MAD-423): current hit
// points live in the resource ledger between fights, so a battle starts
// from the party's real numbers — not the sheet's max — and a battle's
// end writes the survivors' final hit points back as visible 'set'
// transactions. Without it the tracker runs sheet-fresh, as before.
func (s *Store) WithHitPoints(hp HitPoints) *Store {
	s.hp = hp
	return s
}

// WithBroker moves the store onto a shared campaign broker (MAD-423):
// every committed combat write pings the campaign topic so the party
// board's streams re-derive what their own scope may see.
func (s *Store) WithBroker(b *pubsub.Broker) *Store {
	if b != nil {
		s.broker = b
	}
	return s
}

// notify pings the campaign after a committed change. A nil broker (the
// store running standalone in a test) is a no-op.
func (s *Store) notify(campaignID string) {
	if s.broker != nil {
		s.broker.Notify(campaignID)
	}
}

/* ---------- the rows ---------- */

// Combat is one battle: the state machine's persisted position plus the
// links out to the encounter it came from and the session it logs into.
type Combat struct {
	ID          string    `json:"id"`
	CampaignID  string    `json:"-"`
	EncounterID string    `json:"encounter_id,omitempty"`
	SessionID   string    `json:"session_id,omitempty"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	EndReason   string    `json:"end_reason,omitempty"`
	Round       int       `json:"round"`
	TurnIndex   int       `json:"turn_index"` // -1 = order rolled, nobody has acted yet
	Actor       string    `json:"-"`
	StartedAt   time.Time `json:"started_at"`
	EndedAt     time.Time `json:"ended_at,omitempty"`
	CreatedAt   time.Time `json:"-"`
	UpdatedAt   time.Time `json:"-"`
}

// LogEntry is one row of the append-only journal under the state.
type LogEntry struct {
	ID          string         `json:"id"`
	Seq         int64          `json:"seq"`
	Kind        string         `json:"kind"`
	CombatantID string         `json:"combatant_id,omitempty"`
	Amount      int            `json:"amount,omitempty"`
	Note        string         `json:"note,omitempty"`
	Payload     map[string]any `json:"payload,omitempty"`
	Actor       string         `json:"actor,omitempty"`
	EventID     string         `json:"session_event_id,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
}

/* ---------- starting a battle ---------- */

// MonsterLine is one statblock on the foe side, count times.
type MonsterLine struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// CompanionLine is one statblock fighting beside the party — the wolf,
// the sidekick, the hireling — under its own name when the table gave it
// one ("Whiskers the wolf"), else the statblock's.
type CompanionLine struct {
	Statblock string `json:"statblock"`
	Name      string `json:"name,omitempty"`
	Count     int    `json:"count"`
}

// StartInput is a battle's lineup, already permission-checked: the pc
// entities by id, the monsters and companions by statblock name.
type StartInput struct {
	Name        string          `json:"name"`
	EncounterID string          `json:"encounter_id"`
	SessionID   string          `json:"session_id"`
	PCs         []string        `json:"pcs"`
	Monsters    []MonsterLine   `json:"monsters"`
	Companions  []CompanionLine `json:"companions"`
}

// StartResult is what starting produced: the combat as persisted, the
// order the rolls made, and the warnings (a pc with no structured sheet
// fights on zeros; the response says so rather than refusing).
type StartResult struct {
	Combat   Combat      `json:"combat"`
	Order    []Combatant `json:"order"`
	Warnings []string    `json:"warnings,omitempty"`
}

// Start resolves the lineup, rolls initiative through the dice engine
// (ties re-rolled the way the table does), and persists the battle with
// round 1 and the turn counter parked before the first turn — the first
// next-turn call begins the opening combatant's turn with its prompts.
// One active combat per campaign: a second start is a conflict.
func (s *Store) Start(ctx context.Context, campaignID string, in StartInput, actor string) (*StartResult, error) {
	total := len(dedupe(in.PCs)) + countLines(in.Monsters, in.Companions)
	if total == 0 {
		return nil, fmt.Errorf("%w: a combat needs combatants", campaign.ErrInvalid)
	}
	if total > maxCombatants {
		return nil, fmt.Errorf("%w: a combat needs at most %d combatants", campaign.ErrInvalid, maxCombatants)
	}
	// The one-battle rule.
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM combats WHERE campaign_id = ? AND status = 'active'`, campaignID).Scan(&one)
	if err == nil {
		return nil, fmt.Errorf("%w: this campaign already has an active combat — end it first", campaign.ErrAlreadyExists)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("combat start check: %w", err)
	}

	encounterID, err := s.validateEncounter(ctx, campaignID, in.EncounterID)
	if err != nil {
		return nil, err
	}
	sessionID, err := s.validateSession(ctx, campaignID, in.SessionID)
	if err != nil {
		return nil, err
	}

	now := s.now()
	var warnings []string
	cs := make([]Combatant, 0, total+4)

	for _, eid := range dedupe(in.PCs) {
		e, err := s.campaigns.GetEntity(ctx, campaign.ScopeDM, campaignID, eid)
		if err != nil {
			return nil, err
		}
		if e.Kind != campaign.KindPC {
			return nil, fmt.Errorf("%w: %s is a %s, not a pc", campaign.ErrInvalid, e.Name, e.Kind)
		}
		sh, ok, err := campaign.SheetOf(e)
		if err != nil {
			return nil, err
		}
		c := Combatant{
			EntityID: e.ID, Name: e.Name, Side: SideParty, Kind: KindPC,
			AC: sh.AC, MaxHP: sh.MaxHP, HP: sh.MaxHP, Snapshot: SnapshotOfSheet(sh),
		}
		// Tonight's numbers start where the last fight left the
		// character, not at the sheet's max: the ledger's hp pool is
		// the ongoing truth (MAD-423). A character with no hp pool —
		// no structured sheet — keeps fighting from the sheet, which
		// was full anyway.
		if s.hp != nil {
			if cur, _, ok := s.hp.HPBalance(ctx, campaignID, eid); ok {
				c.HP = cur
			}
		}
		if !ok || (sh.AC == 0 && sh.MaxHP == 0) {
			warnings = append(warnings, fmt.Sprintf("%s has no structured sheet — fighting on AC 0, HP 0", e.Name))
		}
		cs = append(cs, c)
	}

	resolve := func(name string) (encounter.Creature, error) {
		if s.resolver == nil {
			return encounter.Creature{}, fmt.Errorf("%w: this install resolves no statblocks — pcs only", campaign.ErrInvalid)
		}
		c, ok := s.resolver.ResolveStatblock(ctx, actor, campaignID, name)
		if !ok {
			return encounter.Creature{}, fmt.Errorf("%w: no statblock named %q", campaign.ErrNotFound, name)
		}
		return c, nil
	}
	for _, m := range in.Monsters {
		m.Name = strings.TrimSpace(m.Name)
		if m.Count < 1 {
			m.Count = 1 // a line without a count is one of them
		}
		if m.Name == "" {
			return nil, fmt.Errorf("%w: monster lines need a name", campaign.ErrInvalid)
		}
		creature, err := resolve(m.Name)
		if err != nil {
			return nil, err
		}
		for i := 0; i < m.Count; i++ {
			cs = append(cs, statblockCombatant(creature, SideFoe, KindMonster, instanceName(creature.Name, m.Count, i)))
		}
	}
	for _, comp := range in.Companions {
		comp.Statblock = strings.TrimSpace(comp.Statblock)
		if comp.Count < 1 {
			comp.Count = 1 // a line without a count is one of them
		}
		if comp.Statblock == "" {
			return nil, fmt.Errorf("%w: companion lines need a statblock", campaign.ErrInvalid)
		}
		creature, err := resolve(comp.Statblock)
		if err != nil {
			return nil, err
		}
		for i := 0; i < comp.Count; i++ {
			name := instanceName(creature.Name, comp.Count, i)
			if comp.Count == 1 && strings.TrimSpace(comp.Name) != "" {
				name = strings.TrimSpace(comp.Name)
			}
			cs = append(cs, statblockCombatant(creature, SideParty, KindCompanion, name))
		}
	}

	// Initiative: one stored roll per combatant, then re-rolls while
	// ties stand — the dice engine's campaign seed makes every number
	// reproducible, and the rolls land in the feed like any other.
	for i := range cs {
		total, err := s.rollInitiative(ctx, campaignID, &cs[i], "initiative")
		if err != nil {
			return nil, err
		}
		cs[i].Initiative = total
	}
	for round := 0; round < tiebreakRounds; round++ {
		tied := tiedGroups(cs)
		if len(tied) == 0 {
			break
		}
		for _, group := range tied {
			for _, i := range group {
				total, err := s.rollInitiative(ctx, campaignID, &cs[i], "initiative tiebreak")
				if err != nil {
					return nil, err
				}
				cs[i].Initiative = total
			}
		}
	}
	order := Order(cs)
	for i := range order {
		order[i].Position = i
	}

	combat := Combat{
		ID: uuid.NewString(), CampaignID: campaignID, EncounterID: encounterID, SessionID: sessionID,
		Name: strings.TrimSpace(in.Name), Status: StatusActive, Round: 1, TurnIndex: -1,
		Actor: actor, StartedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if combat.Name == "" {
		combat.Name = defaultCombatName(order)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("combat start: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO combats (id, campaign_id, encounter_id, session_id, name, status, round, turn_index, actor, started_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 'active', 1, -1, ?, ?, ?, ?)`,
		combat.ID, campaignID, nullString(encounterID), nullString(sessionID), combat.Name,
		actor, combat.StartedAt.UnixMilli(), now.UnixMilli(), now.UnixMilli()); err != nil {
		return nil, fmt.Errorf("combat start: %w", err)
	}
	for i := range order {
		order[i].CombatID = combat.ID
		order[i].ID = uuid.NewString()
		if err := insertCombatant(ctx, tx, order[i], now); err != nil {
			return nil, err
		}
	}
	note := combat.Name + " — order: " + OrderLine(order)
	payload := map[string]any{"round": 1, "order": OrderLine(order)}
	if len(warnings) > 0 {
		payload["warnings"] = warnings
	}
	entry, err := s.appendLog(ctx, tx, combat.ID, "start", "", 0, note, payload, actor, now)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("combat start: %w", err)
	}

	entry.EventID = s.mirror(ctx, &combat, note, "", payload)
	s.stampEventID(ctx, entry)
	s.notify(campaignID)
	return &StartResult{Combat: combat, Order: order, Warnings: warnings}, nil
}

// statblockCombatant freezes one creature into a combatant row.
func statblockCombatant(c encounter.Creature, side, kind, name string) Combatant {
	return Combatant{
		Name: name, Side: side, Kind: kind,
		Snapshot: SnapshotOfCreature(c),
		AC:       c.AC, MaxHP: c.HP, HP: c.HP,
	}
}

// instanceName letters a count: one goblin is "Goblin", the second of
// three is "Goblin B".
func instanceName(base string, count, i int) string {
	if count <= 1 {
		return base
	}
	return base + " " + letters(i)
}

// letters renders 0 -> A, 25 -> Z, 26 -> AA.
func letters(i int) string {
	var out []byte
	for n := i; ; {
		out = append([]byte{byte('A' + n%26)}, out...)
		n = n/26 - 1
		if n < 0 {
			break
		}
	}
	return string(out)
}

func defaultCombatName(order []Combatant) string {
	var foes int
	for _, c := range order {
		if c.Side == SideFoe {
			foes++
		}
	}
	if foes == 0 {
		return "Combat"
	}
	if foes == 1 {
		return "Combat — one on the other side"
	}
	return fmt.Sprintf("Combat — %d on the other side", foes)
}

func countLines(monsters []MonsterLine, companions []CompanionLine) int {
	n := 0
	for _, m := range monsters {
		if m.Count > 0 {
			n += m.Count
		}
	}
	for _, c := range companions {
		if c.Count > 0 {
			n += c.Count
		}
	}
	return n
}

// dedupe trims and drops repeats and blanks.
func dedupe(ids []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// rollInitiative rolls 1d20 plus the combatant's bonus through the dice
// engine — the formula is stored on the row, the roll in the feed.
func (s *Store) rollInitiative(ctx context.Context, campaignID string, c *Combatant, detail string) (int, error) {
	c.InitFormula = "1d20"
	switch {
	case c.Snapshot.InitBonus > 0:
		c.InitFormula = fmt.Sprintf("1d20+%d", c.Snapshot.InitBonus)
	case c.Snapshot.InitBonus < 0:
		c.InitFormula = fmt.Sprintf("1d20%d", c.Snapshot.InitBonus)
	}
	roll, err := s.dice.Roll(ctx, campaignID, dice.Input{
		Formula:     c.InitFormula,
		Visibility:  dice.VisibilityPublic,
		ContextKind: dice.ContextInitiative,
		Detail:      detail + " — " + c.Name,
		CharacterID: c.EntityID,
	})
	if err != nil {
		return 0, err
	}
	return roll.Result.Total, nil
}

// tiedGroups returns the index groups sharing an initiative total.
func tiedGroups(cs []Combatant) [][]int {
	byTotal := map[int][]int{}
	for i, c := range cs {
		byTotal[c.Initiative] = append(byTotal[c.Initiative], i)
	}
	var groups [][]int
	for _, idx := range byTotal {
		if len(idx) > 1 {
			groups = append(groups, idx)
		}
	}
	return groups
}

/* ---------- reads ---------- */

// Active reads the campaign's live battle, or nils when none runs.
func (s *Store) Active(ctx context.Context, campaignID string) (*Combat, []Combatant, error) {
	var c Combat
	row := s.db.QueryRowContext(ctx, combatCols+` FROM combats WHERE campaign_id = ? AND status = 'active'`, campaignID)
	err := scanCombat(row, &c)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	cs, err := s.combatants(ctx, c.ID)
	if err != nil {
		return nil, nil, err
	}
	return &c, cs, nil
}

// Get reads one combat with its order.
func (s *Store) Get(ctx context.Context, campaignID, combatID string) (*Combat, []Combatant, error) {
	var c Combat
	row := s.db.QueryRowContext(ctx, combatCols+` FROM combats WHERE id = ? AND campaign_id = ?`, combatID, campaignID)
	err := scanCombat(row, &c)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, fmt.Errorf("%w: combat %s", campaign.ErrNotFound, combatID)
	}
	if err != nil {
		return nil, nil, err
	}
	cs, err := s.combatants(ctx, c.ID)
	if err != nil {
		return nil, nil, err
	}
	return &c, cs, nil
}

// List reads the campaign's battles, newest first.
func (s *Store) List(ctx context.Context, campaignID string) ([]Combat, error) {
	rows, err := s.db.QueryContext(ctx, combatCols+` FROM combats WHERE campaign_id = ? ORDER BY created_at DESC, rowid DESC`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("combat list: %w", err)
	}
	defer rows.Close()
	var out []Combat
	for rows.Next() {
		var c Combat
		if err := scanCombat(rows, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Log reads the journal in play order. after and limit bound the read;
// limit 0 reads to the end.
func (s *Store) Log(ctx context.Context, campaignID, combatID string, after int64, limit int) ([]LogEntry, error) {
	if _, _, err := s.Get(ctx, campaignID, combatID); err != nil {
		return nil, err
	}
	q := `SELECT id, seq, kind, COALESCE(combatant_id, ''), amount, note, payload, actor,
	             COALESCE(session_event_id, ''), created_at
	      FROM combat_log WHERE combat_id = ?`
	args := []any{combatID}
	if after > 0 {
		q += ` AND seq > ?`
		args = append(args, after)
	}
	q += ` ORDER BY seq`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("combat log: %w", err)
	}
	defer rows.Close()
	var out []LogEntry
	for rows.Next() {
		var e LogEntry
		var payload string
		var created int64
		if err := rows.Scan(&e.ID, &e.Seq, &e.Kind, &e.CombatantID, &e.Amount, &e.Note,
			&payload, &e.Actor, &e.EventID, &created); err != nil {
			return nil, err
		}
		e.Payload = decodePayload(payload)
		e.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// combatants reads one battle's order.
func (s *Store) combatants(ctx context.Context, combatID string) ([]Combatant, error) {
	rows, err := s.db.QueryContext(ctx, combatantCols+` FROM combatants WHERE combat_id = ? ORDER BY position`, combatID)
	if err != nil {
		return nil, fmt.Errorf("combatants: %w", err)
	}
	defer rows.Close()
	var out []Combatant
	for rows.Next() {
		c, err := scanCombatant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// loadCombatant reads one combatant with its owning combat.
func (s *Store) loadCombatant(ctx context.Context, campaignID, combatID, combatantID string) (*Combat, *Combatant, error) {
	combat, _, err := s.Get(ctx, campaignID, combatID)
	if err != nil {
		return nil, nil, err
	}
	row := s.db.QueryRowContext(ctx, combatantCols+` FROM combatants WHERE id = ? AND combat_id = ?`, combatantID, combatID)
	c, err := scanCombatant(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, fmt.Errorf("%w: combatant %s", campaign.ErrNotFound, combatantID)
	}
	if err != nil {
		return nil, nil, err
	}
	return combat, &c, nil
}

/* ---------- damage, healing, temp hp ---------- */

// ConcentrationPrompt is the check damage demands of a concentrator:
// the effect holding and the DC (10, or half the damage taken, whichever
// is higher — the 2014 concentration rule).
type ConcentrationPrompt struct {
	Spell  string `json:"spell"`
	DC     int    `json:"dc"`
	Source string `json:"source_id"`
}

// ChangeResult is what one mid-play write changed on one combatant.
type ChangeResult struct {
	Combatant     Combatant             `json:"combatant"`
	Outcome       DamageOutcome         `json:"outcome,omitempty"`
	Concentration []ConcentrationPrompt `json:"concentration_checks,omitempty"`
	Summary       string                `json:"summary"`
}

// Damage applies one hit, persists it, and journals it. reduceMax is the
// vampire's bite — the max HP wears by the damage that landed.
func (s *Store) Damage(ctx context.Context, campaignID, combatID, combatantID string, amount int, dtype, note string, reduceMax bool, actor string) (*ChangeResult, error) {
	combat, c, err := s.loadCombatant(ctx, campaignID, combatID, combatantID)
	if err != nil {
		return nil, err
	}
	if combat.Status != StatusActive {
		return nil, fmt.Errorf("%w: this combat has ended", campaign.ErrInvalid)
	}
	after, outcome, err := ApplyDamage(*c, amount, dtype, reduceMax)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", campaign.ErrInvalid, err)
	}
	now := s.now()
	payload := map[string]any{
		"asked": outcome.Asked, "effective": outcome.Effective,
		"before": outcome.Before, "after": outcome.After,
	}
	if outcome.Type != "" {
		payload["damage_type"] = outcome.Type
	}
	if outcome.Absorbed > 0 {
		payload["absorbed_by_temp_hp"] = outcome.Absorbed
	}
	if outcome.Overflow > 0 {
		payload["carried_past_zero"] = outcome.Overflow
	}
	if outcome.ReducedMax > 0 {
		payload["max_hp_reduced_by"] = outcome.ReducedMax
		payload["max_hp"] = after.EffectiveMax()
	}
	if outcome.WentDown {
		payload["went_down"] = true
	}
	if outcome.Died {
		payload["died"] = true
	}
	if outcome.DeathFail {
		payload["death_save_failures"] = after.DeathFailures
	}
	if note = strings.TrimSpace(note); note != "" {
		payload["note"] = note
	}
	summary := outcome.Summary(after)
	if note != "" {
		summary += " — " + note
	}
	if err := s.writeCombatant(ctx, combat, &after, "damage", outcome.Effective, summary, payload, actor, now); err != nil {
		return nil, err
	}
	out := &ChangeResult{Combatant: after, Outcome: outcome, Summary: summary}
	out.Concentration = s.concentrationPrompts(ctx, campaignID, &after, outcome.Effective)
	return out, nil
}

// Heal applies healing, persists it, and journals it.
func (s *Store) Heal(ctx context.Context, campaignID, combatID, combatantID string, amount int, note, actor string) (*ChangeResult, error) {
	combat, c, err := s.loadCombatant(ctx, campaignID, combatID, combatantID)
	if err != nil {
		return nil, err
	}
	if combat.Status != StatusActive {
		return nil, fmt.Errorf("%w: this combat has ended", campaign.ErrInvalid)
	}
	before, wasDown := c.HP, c.Downed
	after, err := ApplyHeal(*c, amount)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", campaign.ErrInvalid, err)
	}
	now := s.now()
	summary := fmt.Sprintf("%s heals %d · %d/%d hp", after.Name, amount, after.HP, after.EffectiveMax())
	if wasDown && !after.Downed {
		summary = fmt.Sprintf("%s heals %d and wakes · %d/%d hp", after.Name, amount, after.HP, after.EffectiveMax())
	}
	payload := map[string]any{"before": before, "after": after.HP}
	if wasDown && !after.Downed {
		payload["woke"] = true
	}
	if note = strings.TrimSpace(note); note != "" {
		payload["note"] = note
		summary += " — " + note
	}
	if err := s.writeCombatant(ctx, combat, &after, "heal", amount, summary, payload, actor, now); err != nil {
		return nil, err
	}
	return &ChangeResult{Combatant: after, Summary: summary}, nil
}

// TempHP grants temp hit points — the larger coat wins, they never
// stack — and journals it.
func (s *Store) TempHP(ctx context.Context, campaignID, combatID, combatantID string, amount int, note, actor string) (*ChangeResult, error) {
	combat, c, err := s.loadCombatant(ctx, campaignID, combatID, combatantID)
	if err != nil {
		return nil, err
	}
	if combat.Status != StatusActive {
		return nil, fmt.Errorf("%w: this combat has ended", campaign.ErrInvalid)
	}
	after, err := GrantTempHP(*c, amount)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", campaign.ErrInvalid, err)
	}
	now := s.now()
	summary := fmt.Sprintf("%s gains %d temp hp", after.Name, after.TempHP)
	payload := map[string]any{"temp_hp": after.TempHP}
	if note = strings.TrimSpace(note); note != "" {
		payload["note"] = note
		summary += " — " + note
	}
	if err := s.writeCombatant(ctx, combat, &after, "temp_hp", amount, summary, payload, actor, now); err != nil {
		return nil, err
	}
	return &ChangeResult{Combatant: after, Summary: summary}, nil
}

// Reaction records the combatant's reaction spent or ready again.
func (s *Store) Reaction(ctx context.Context, campaignID, combatID, combatantID string, spent bool, actor string) (*ChangeResult, error) {
	combat, c, err := s.loadCombatant(ctx, campaignID, combatID, combatantID)
	if err != nil {
		return nil, err
	}
	if combat.Status != StatusActive {
		return nil, fmt.Errorf("%w: this combat has ended", campaign.ErrInvalid)
	}
	c.ReactionSpent = spent
	now := s.now()
	verb := "ready"
	if spent {
		verb = "spent"
	}
	summary := fmt.Sprintf("%s's reaction is %s", c.Name, verb)
	payload := map[string]any{"reaction_spent": spent}
	if err := s.writeCombatant(ctx, combat, c, "reaction", 0, summary, payload, actor, now); err != nil {
		return nil, err
	}
	return &ChangeResult{Combatant: *c, Summary: summary}, nil
}

// Legendary records one legendary-action spend.
func (s *Store) Legendary(ctx context.Context, campaignID, combatID, combatantID, ability string, cost int, actor string) (*ChangeResult, error) {
	combat, c, err := s.loadCombatant(ctx, campaignID, combatID, combatantID)
	if err != nil {
		return nil, err
	}
	if combat.Status != StatusActive {
		return nil, fmt.Errorf("%w: this combat has ended", campaign.ErrInvalid)
	}
	after, err := c.SpendLegendary(cost)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", campaign.ErrInvalid, err)
	}
	now := s.now()
	summary := fmt.Sprintf("%s uses %s (legendary, %d of %d spent)",
		after.Name, strings.TrimSpace(ability), after.LegendaryUsed, after.Snapshot.LegendaryMax)
	payload := map[string]any{"ability": strings.TrimSpace(ability), "cost": cost,
		"used": after.LegendaryUsed, "max": after.Snapshot.LegendaryMax}
	if err := s.writeCombatant(ctx, combat, &after, "legendary", cost, summary, payload, actor, now); err != nil {
		return nil, err
	}
	return &ChangeResult{Combatant: after, Summary: summary}, nil
}

// concentrationPrompts surfaces the concentration checks damage
// demands: every effect the damaged combatant's entity concentrates on,
// at the 2014 DC.
func (s *Store) concentrationPrompts(ctx context.Context, campaignID string, c *Combatant, damageTaken int) []ConcentrationPrompt {
	if s.effects == nil || c.EntityID == "" || damageTaken < 1 {
		return nil
	}
	rows, err := s.effects.Concentrations(ctx, campaignID)
	if err != nil {
		return nil
	}
	dc := 10
	if half := damageTaken / 2; half > dc {
		dc = half
	}
	var out []ConcentrationPrompt
	for _, r := range rows {
		if r.SourceID == c.EntityID {
			out = append(out, ConcentrationPrompt{Spell: r.Name, DC: dc, Source: r.SourceID})
		}
	}
	return out
}

/* ---------- death saves ---------- */

// SaveResult is what one death-save write changed.
type SaveResult struct {
	Combatant Combatant   `json:"combatant"`
	Outcome   SaveOutcome `json:"outcome"`
	Summary   string      `json:"summary"`
}

// DeathSave records one death-save result for a downed pc.
func (s *Store) DeathSave(ctx context.Context, campaignID, combatID, combatantID, result, actor string) (*SaveResult, error) {
	combat, c, err := s.loadCombatant(ctx, campaignID, combatID, combatantID)
	if err != nil {
		return nil, err
	}
	if combat.Status != StatusActive {
		return nil, fmt.Errorf("%w: this combat has ended", campaign.ErrInvalid)
	}
	after, outcome, err := RecordDeathSave(*c, result)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", campaign.ErrInvalid, err)
	}
	now := s.now()
	payload := map[string]any{
		"result": result, "successes": after.DeathSuccesses, "failures": after.DeathFailures,
		"stable": after.Stable, "dead": after.Dead,
	}
	summary := outcome.Summary(after)
	if err := s.writeCombatant(ctx, combat, &after, "death_save", 0, summary, payload, actor, now); err != nil {
		return nil, err
	}
	return &SaveResult{Combatant: after, Outcome: outcome, Summary: summary}, nil
}

/* ---------- conditions (statblock-backed combatants) ---------- */

// ConditionResult is what one condition write changed.
type ConditionResult struct {
	Combatant Combatant `json:"combatant"`
	Condition Condition `json:"condition"`
	Summary   string    `json:"summary"`
}

// ApplyCondition adds one condition to a statblock-backed combatant.
// pcs carry their conditions as duration-engine rows (the effects API)
// — those persist past the battle and ride both clocks; this is refused
// here by design. The name must be one of the fifteen.
func (s *Store) ApplyCondition(ctx context.Context, campaignID, combatID, combatantID, name string, rounds int, actor string) (*ConditionResult, error) {
	combat, c, err := s.loadCombatant(ctx, campaignID, combatID, combatantID)
	if err != nil {
		return nil, err
	}
	if combat.Status != StatusActive {
		return nil, fmt.Errorf("%w: this combat has ended", campaign.ErrInvalid)
	}
	canon := effects.ConditionName(name)
	if canon == "" {
		return nil, fmt.Errorf("%w: %q is not one of the game's conditions", campaign.ErrInvalid, name)
	}
	after, cond, err := ApplyLocalCondition(*c, canon, rounds, actor)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", campaign.ErrInvalid, err)
	}
	now := s.now()
	summary := fmt.Sprintf("%s is %s (%d round%s)", after.Name, canon, rounds, sPlural(rounds))
	payload := map[string]any{"condition": canon, "rounds": rounds, "id": cond.ID}
	if err := s.writeCombatant(ctx, combat, &after, "condition", 0, summary, payload, actor, now); err != nil {
		return nil, err
	}
	return &ConditionResult{Combatant: after, Condition: cond, Summary: summary}, nil
}

// EndCondition removes one local condition by id.
func (s *Store) EndCondition(ctx context.Context, campaignID, combatID, combatantID, condID, actor string) (*ConditionResult, error) {
	combat, c, err := s.loadCombatant(ctx, campaignID, combatID, combatantID)
	if err != nil {
		return nil, err
	}
	if combat.Status != StatusActive {
		return nil, fmt.Errorf("%w: this combat has ended", campaign.ErrInvalid)
	}
	after, cond, err := EndLocalCondition(*c, condID)
	if err != nil {
		return nil, fmt.Errorf("%w: no such condition", campaign.ErrNotFound)
	}
	now := s.now()
	summary := fmt.Sprintf("%s is no longer %s", after.Name, cond.Name)
	payload := map[string]any{"condition": cond.Name, "id": cond.ID}
	if err := s.writeCombatant(ctx, combat, &after, "condition_end", 0, summary, payload, actor, now); err != nil {
		return nil, err
	}
	return &ConditionResult{Combatant: after, Condition: cond, Summary: summary}, nil
}

/* ---------- the reveal (MAD-425) ---------- */

// Reveal sets one foe's exposure on the table screen: hidden, exact hit
// points, or the health word. It is the DM's presentation choice, not a
// mechanical change — no number moves — but it lands in the journal
// beside every other DM act on the battle, and the notify wakes the
// table screen's streams so the room's view changes live. PCs and
// companions are refused: their hit points are the party's own numbers,
// governed by the board's visibility config, not the DM's per-monster
// toggle.
func (s *Store) Reveal(ctx context.Context, campaignID, combatID, combatantID, mode, actor string) (*ChangeResult, error) {
	if !ValidRevealMode(mode) {
		return nil, fmt.Errorf("%w: reveal mode %q is not one of off, hp, word", campaign.ErrInvalid, mode)
	}
	combat, c, err := s.loadCombatant(ctx, campaignID, combatID, combatantID)
	if err != nil {
		return nil, err
	}
	if combat.Status != StatusActive {
		return nil, fmt.Errorf("%w: this combat has ended", campaign.ErrInvalid)
	}
	if c.Side != SideFoe {
		return nil, fmt.Errorf("%w: only the other side's numbers are the DM's to reveal", campaign.ErrInvalid)
	}
	c.Reveal = mode
	now := s.now()
	summary := fmt.Sprintf("%s's numbers are the DM's alone", c.Name)
	if mode == RevealHP {
		summary = fmt.Sprintf("the table sees %s's hit points", c.Name)
	} else if mode == RevealWord {
		summary = fmt.Sprintf("the table sees how %s looks", c.Name)
	}
	payload := map[string]any{"reveal": mode}
	if err := s.writeCombatant(ctx, combat, c, "reveal", 0, summary, payload, actor, now); err != nil {
		return nil, err
	}
	return &ChangeResult{Combatant: *c, Summary: summary}, nil
}

/* ---------- the turn engine ---------- */

// TurnPrompt is one thing the incoming turn asks of the table: a
// monster's recharge rolls, or a downed pc's death save.
type TurnPrompt struct {
	Kind   string `json:"kind"` // recharge | death_save
	Name   string `json:"name,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// ExpiredCondition is one condition the round wore away, with the
// combatant it left.
type ExpiredCondition struct {
	CombatantID string `json:"combatant_id"`
	Name        string `json:"name"`
}

// TurnResult is what one next-turn call produced: the persisted
// position, the order with live state, the prompts the incoming turn
// surfaced, and what the wrap (when it wrapped) wore away.
type TurnResult struct {
	Combat            Combat             `json:"combat"`
	Order             []Combatant        `json:"order"`
	Summary           string             `json:"summary"`
	RoundWrapped      bool               `json:"round_wrapped"`
	LairReminder      bool               `json:"lair_reminder"`
	Prompts           []TurnPrompt       `json:"prompts,omitempty"`
	ExpiredConditions []ExpiredCondition `json:"expired_conditions,omitempty"`
	ExpiredEffects    []effects.Row      `json:"-"`
	Alive             map[string]int     `json:"alive"`
}

// NextTurn advances the state machine one turn: the outgoing turn ends
// (legendary budget back), the order wraps when it wraps (round up,
// durations wear on both engines, lair reminder on the count-20
// crossing), and the incoming turn starts (reaction back, prompts up).
func (s *Store) NextTurn(ctx context.Context, campaignID, combatID, actor string) (*TurnResult, error) {
	combat, order, err := s.Get(ctx, campaignID, combatID)
	if err != nil {
		return nil, err
	}
	if combat.Status != StatusActive {
		return nil, fmt.Errorf("%w: this combat has ended", campaign.ErrInvalid)
	}
	now := s.now()
	out := &TurnResult{Combat: *combat, Order: order}
	live := make([]Combatant, len(order))
	copy(live, order)
	dirty := map[int]bool{}

	// The outgoing turn ends. Before the first turn (-1) nobody ends;
	// the count itself stands in as "above 20" so a lair whose round
	// opens at or under 20 still leads the order.
	prevInit := LairInitiative + 1
	if combat.TurnIndex >= 0 && combat.TurnIndex < len(live) {
		live[combat.TurnIndex] = live[combat.TurnIndex].EndTurn()
		prevInit = live[combat.TurnIndex].Initiative
		dirty[combat.TurnIndex] = true
	}

	next, wrapped := NextAlive(live, combat.TurnIndex)
	incoming := live[next].StartTurn()
	live[next] = incoming
	dirty[next] = true

	round := combat.Round
	if wrapped {
		round++
		out.RoundWrapped = true
		var expiredConds []ExpiredCondition
		for i := range live {
			ticked, expired := TickConditions(live[i])
			if len(expired) > 0 || ticked.Conditions != nil {
				live[i] = ticked
				dirty[i] = true
			}
			for _, cond := range expired {
				expiredConds = append(expiredConds, ExpiredCondition{CombatantID: ticked.ID, Name: cond.Name})
			}
		}
		out.ExpiredConditions = expiredConds
		if s.effects != nil {
			if _, expiredRows, err := s.effects.AdvanceRounds(ctx, campaignID, 1); err == nil {
				out.ExpiredEffects = expiredRows
			}
		}
	}

	// The lair reminder: at count 20, losing all ties.
	hasLair := false
	for _, c := range live {
		if c.Snapshot.Lair {
			hasLair = true
			break
		}
	}
	out.LairReminder = hasLair && LairFires(prevInit, incoming.Initiative, wrapped)

	// The incoming turn's prompts.
	for _, ab := range incoming.Snapshot.Recharge {
		out.Prompts = append(out.Prompts, TurnPrompt{Kind: "recharge", Name: ab.Name, Detail: ab.Usage})
	}
	if incoming.Downed && !incoming.Stable && incoming.Kind == KindPC {
		out.Prompts = append(out.Prompts, TurnPrompt{Kind: "death_save", Name: incoming.Name,
			Detail: fmt.Sprintf("%d success%s, %d failure%s", incoming.DeathSuccesses,
				sPlural(incoming.DeathSuccesses), incoming.DeathFailures, sPlural(incoming.DeathFailures))})
	}
	out.Alive = aliveBySide(live)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("combat turn: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		UPDATE combats SET round = ?, turn_index = ?, actor = ?, updated_at = ? WHERE id = ?`,
		round, next, actor, now.UnixMilli(), combat.ID); err != nil {
		return nil, fmt.Errorf("combat turn: %w", err)
	}
	for i := range live {
		if dirty[i] {
			if err := updateCombatant(ctx, tx, live[i], now); err != nil {
				return nil, err
			}
		}
	}
	summary := fmt.Sprintf("Round %d — %s's turn", round, incoming.Name)
	payload := map[string]any{"round": round, "turn_index": next, "combatant": incoming.Name}
	detail := ""
	if out.RoundWrapped {
		payload["new_round"] = round
	}
	if out.LairReminder {
		payload["lair_action"] = "initiative count 20"
		detail = "lair action at initiative count 20"
	}
	if n := len(out.ExpiredConditions); n > 0 {
		names := make([]string, 0, n)
		for _, ec := range out.ExpiredConditions {
			names = append(names, ec.Name)
		}
		payload["expired_conditions"] = names
	}
	entry, err := s.appendLog(ctx, tx, combat.ID, "turn", incoming.ID, 0, summary, payload, actor, now)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("combat turn: %w", err)
	}

	after := *combat
	after.Round, after.TurnIndex, after.UpdatedAt = round, next, now
	entry.EventID = s.mirror(ctx, &after, summary, detail, payload)
	s.stampEventID(ctx, entry)
	s.notify(campaignID)
	out.Combat = after
	out.Order = live
	return out, nil
}

func sPlural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func aliveBySide(order []Combatant) map[string]int {
	out := map[string]int{SideParty: 0, SideFoe: 0}
	for _, c := range order {
		if !c.Dead {
			out[c.Side]++
		}
	}
	return out
}

/* ---------- ending ---------- */

// EndResult is what ending produced.
type EndResult struct {
	Combat  Combat         `json:"combat"`
	Order   []Combatant    `json:"order"`
	Summary string         `json:"summary"`
	Alive   map[string]int `json:"alive"`
}

// End closes the battle: status ended, the survivor counts recorded,
// the final journal entry and session mirror written.
func (s *Store) End(ctx context.Context, campaignID, combatID, reason, actor string) (*EndResult, error) {
	combat, order, err := s.Get(ctx, campaignID, combatID)
	if err != nil {
		return nil, err
	}
	if combat.Status != StatusActive {
		return nil, fmt.Errorf("%w: this combat has already ended", campaign.ErrInvalid)
	}
	now := s.now()
	alive := aliveBySide(order)
	combat.Status = StatusEnded
	combat.EndReason = strings.TrimSpace(reason)
	combat.EndedAt = now
	combat.UpdatedAt = now

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("combat end: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		UPDATE combats SET status = 'ended', end_reason = ?, ended_at = ?, actor = ?, updated_at = ?
		 WHERE id = ? AND campaign_id = ? AND status = 'active'`,
		combat.EndReason, now.UnixMilli(), actor, now.UnixMilli(), combatID, campaignID); err != nil {
		return nil, fmt.Errorf("combat end: %w", err)
	}
	summary := fmt.Sprintf("%s ends after %d round%s — %d of the party standing, %d of the foe",
		combat.Name, combat.Round, sPlural(combat.Round), alive[SideParty], alive[SideFoe])
	if combat.EndReason != "" {
		summary += " (" + combat.EndReason + ")"
	}
	payload := map[string]any{"round": combat.Round, "alive": alive}
	if combat.EndReason != "" {
		payload["reason"] = combat.EndReason
	}
	entry, err := s.appendLog(ctx, tx, combatID, "end", "", 0, summary, payload, actor, now)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("combat end: %w", err)
	}
	entry.EventID = s.mirror(ctx, combat, summary, "", payload)
	s.stampEventID(ctx, entry)

	// The ledger write-back: the battle's survivors carry their final
	// hit points out of the fight as one visible 'set' transaction each,
	// so the board and the resource reads stay one truth between fights
	// (MAD-423). Best-effort, like the session mirror — a failed
	// write-back never un-ends a battle; the DM's correction path is the
	// resources surface. Temp hit points do not survive the fight.
	if s.hp != nil {
		for _, c := range order {
			if c.Kind != KindPC || c.EntityID == "" {
				continue
			}
			_ = s.hp.SetHP(ctx, campaignID, c.EntityID, c.HP, actor,
				fmt.Sprintf("%s ends — %s at %d hp", combat.Name, c.Name, c.HP))
		}
	}
	s.notify(campaignID)
	return &EndResult{Combat: *combat, Order: order, Summary: summary, Alive: alive}, nil
}

/* ---------- the persistence helpers ---------- */

const combatCols = `SELECT id, campaign_id, COALESCE(encounter_id, ''), COALESCE(session_id, ''),
                    name, status, COALESCE(end_reason, ''), round, turn_index, actor,
                    started_at, COALESCE(ended_at, 0), created_at, updated_at`

func scanCombat(row interface{ Scan(...any) error }, c *Combat) error {
	var started, ended, created, updated int64
	if err := row.Scan(&c.ID, &c.CampaignID, &c.EncounterID, &c.SessionID,
		&c.Name, &c.Status, &c.EndReason, &c.Round, &c.TurnIndex, &c.Actor,
		&started, &ended, &created, &updated); err != nil {
		return err
	}
	c.StartedAt = time.UnixMilli(started).UTC()
	if ended > 0 {
		c.EndedAt = time.UnixMilli(ended).UTC()
	}
	c.CreatedAt = time.UnixMilli(created).UTC()
	c.UpdatedAt = time.UnixMilli(updated).UTC()
	return nil
}

const combatantCols = `SELECT id, combat_id, COALESCE(entity_id, ''), name, side, kind, statblock,
                       initiative, init_bonus, init_formula, ac, max_hp, hp_reduction, hp, temp_hp,
                       downed, stable, dead, death_successes, death_failures, reaction_spent,
                       legendary_used, conditions, reveal, position, created_at, updated_at`

func scanCombatant(row interface{ Scan(...any) error }) (Combatant, error) {
	var (
		c                        Combatant
		statblockJSON, condsJSON string
		downed, stable, dead     int
		reaction                 int
		created, updated         int64
	)
	if err := row.Scan(&c.ID, &c.CombatID, &c.EntityID, &c.Name, &c.Side, &c.Kind, &statblockJSON,
		&c.Initiative, &c.Snapshot.InitBonus, &c.InitFormula, &c.AC, &c.MaxHP, &c.HPReduction,
		&c.HP, &c.TempHP, &downed, &stable, &dead, &c.DeathSuccesses, &c.DeathFailures,
		&reaction, &c.LegendaryUsed, &condsJSON, &c.Reveal, &c.Position, &created, &updated); err != nil {
		return Combatant{}, err
	}
	c.Downed, c.Stable, c.Dead, c.ReactionSpent = downed == 1, stable == 1, dead == 1, reaction == 1
	c.CreatedAt, c.UpdatedAt = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
	if statblockJSON != "" && statblockJSON != "{}" {
		if err := json.Unmarshal([]byte(statblockJSON), &c.Snapshot); err != nil {
			return Combatant{}, fmt.Errorf("decode statblock snapshot: %w", err)
		}
	}
	if condsJSON != "" && condsJSON != "[]" && condsJSON != "null" {
		if err := json.Unmarshal([]byte(condsJSON), &c.Conditions); err != nil {
			return Combatant{}, fmt.Errorf("decode conditions: %w", err)
		}
	}
	return c, nil
}

func insertCombatant(ctx context.Context, tx *sql.Tx, c Combatant, now time.Time) error {
	snap, err := json.Marshal(c.Snapshot)
	if err != nil {
		return fmt.Errorf("encode snapshot: %w", err)
	}
	conds, err := json.Marshal(c.Conditions)
	if err != nil {
		return fmt.Errorf("encode conditions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO combatants (id, combat_id, entity_id, name, side, kind, statblock,
		                        initiative, init_bonus, init_formula, ac, max_hp, hp_reduction, hp, temp_hp,
		                        downed, stable, dead, death_successes, death_failures, reaction_spent,
		                        legendary_used, conditions, reveal, position, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.CombatID, nullString(c.EntityID), c.Name, c.Side, c.Kind, string(snap),
		c.Initiative, c.Snapshot.InitBonus, c.InitFormula, c.AC, c.MaxHP, c.HPReduction, c.HP, c.TempHP,
		boolInt(c.Downed), boolInt(c.Stable), boolInt(c.Dead), c.DeathSuccesses, c.DeathFailures,
		boolInt(c.ReactionSpent), c.LegendaryUsed, string(conds), c.Reveal, c.Position,
		now.UnixMilli(), now.UnixMilli()); err != nil {
		return fmt.Errorf("insert combatant: %w", err)
	}
	return nil
}

func updateCombatant(ctx context.Context, tx *sql.Tx, c Combatant, now time.Time) error {
	conds, err := json.Marshal(c.Conditions)
	if err != nil {
		return fmt.Errorf("encode conditions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE combatants SET statblock = ?, initiative = ?, ac = ?, max_hp = ?, hp_reduction = ?,
		                      hp = ?, temp_hp = ?, downed = ?, stable = ?, dead = ?,
		                      death_successes = ?, death_failures = ?, reaction_spent = ?,
		                      legendary_used = ?, conditions = ?, reveal = ?, updated_at = ?
		 WHERE id = ? AND combat_id = ?`,
		mustJSON(c.Snapshot), c.Initiative, c.AC, c.MaxHP, c.HPReduction,
		c.HP, c.TempHP, boolInt(c.Downed), boolInt(c.Stable), boolInt(c.Dead),
		c.DeathSuccesses, c.DeathFailures, boolInt(c.ReactionSpent),
		c.LegendaryUsed, string(conds), c.Reveal, now.UnixMilli(), c.ID, c.CombatID); err != nil {
		return fmt.Errorf("update combatant: %w", err)
	}
	return nil
}

// writeCombatant persists one combatant change and its journal entry in
// one transaction, then mirrors the entry into the live session's log —
// the row, the journal and the session event tell one story.
func (s *Store) writeCombatant(ctx context.Context, combat *Combat, c *Combatant, kind string, amount int, summary string, payload map[string]any, actor string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("combat %s: %w", kind, err)
	}
	defer tx.Rollback()
	if err := updateCombatant(ctx, tx, *c, now); err != nil {
		return err
	}
	entry, err := s.appendLog(ctx, tx, combat.ID, kind, c.ID, amount, summary, payload, actor, now)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("combat %s: %w", kind, err)
	}
	entry.EventID = s.mirror(ctx, combat, summary, "", payload)
	s.stampEventID(ctx, entry)
	s.notify(combat.CampaignID)
	return nil
}

// appendLog writes one journal row, seq assigned atomically in the
// INSERT — the session_events pattern.
func (s *Store) appendLog(ctx context.Context, tx *sql.Tx, combatID, kind, combatantID string, amount int, note string, payload map[string]any, actor string, now time.Time) (LogEntry, error) {
	if payload == nil {
		payload = map[string]any{}
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return LogEntry{}, fmt.Errorf("encode payload: %w", err)
	}
	e := LogEntry{
		ID: uuid.NewString(), Kind: kind, CombatantID: strings.TrimSpace(combatantID),
		Amount: amount, Note: note, Payload: payload, Actor: actor, CreatedAt: now,
	}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO combat_log (id, combat_id, seq, kind, combatant_id, amount, note, payload, actor, created_at)
		VALUES (?, ?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM combat_log WHERE combat_id = ?), ?, ?, ?, ?, ?, ?, ?)
		RETURNING seq`,
		e.ID, combatID, combatID, kind, nullString(e.CombatantID), amount, note,
		string(payloadJSON), actor, now.UnixMilli()).Scan(&e.Seq)
	if err != nil {
		return LogEntry{}, fmt.Errorf("insert combat log: %w", err)
	}
	return e, nil
}

// stampEventID points one journal row at its session mirror.
func (s *Store) stampEventID(ctx context.Context, entry LogEntry) {
	if entry.EventID == "" {
		return
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE combat_log SET session_event_id = ? WHERE id = ?`,
		entry.EventID, entry.ID)
}

// mirror appends the kind 'combat' session event for the combat's live
// sitting — the same resolution the dice store does: the combat's own
// session when it has one, else the campaign's live sitting, else no
// mirror (the journal stands alone).
func (s *Store) mirror(ctx context.Context, combat *Combat, summary, detail string, payload map[string]any) string {
	sessionID := combat.SessionID
	if sessionID == "" {
		sessionID = s.liveSession(ctx, combat.CampaignID)
	}
	if sessionID == "" {
		return ""
	}
	if payload == nil {
		payload = map[string]any{}
	}
	payload["combat_id"] = combat.ID
	if combat.Round > 0 {
		payload["round"] = combat.Round
	}
	ev, err := s.sessions.AddEvent(ctx, sessionID, gamesession.EventCombat, summary, detail, payload)
	if err != nil {
		return ""
	}
	return ev.ID
}

// liveSession finds the campaign's live sitting — the dice store's own
// resolution, shared because there is one truth about which table is
// live.
func (s *Store) liveSession(ctx context.Context, campaignID string) string {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM game_sessions WHERE campaign_id = ? AND status = 'live' ORDER BY ordinal DESC LIMIT 1`,
		campaignID).Scan(&id)
	if err != nil {
		return ""
	}
	return id
}

/* ---------- link validation ---------- */

func (s *Store) validateEncounter(ctx context.Context, campaignID, encounterID string) (string, error) {
	encounterID = strings.TrimSpace(encounterID)
	if encounterID == "" {
		return "", nil
	}
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM encounters WHERE id = ? AND campaign_id = ?`, encounterID, campaignID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: encounter %s", campaign.ErrNotFound, encounterID)
	}
	if err != nil {
		return "", fmt.Errorf("check encounter: %w", err)
	}
	return encounterID, nil
}

func (s *Store) validateSession(ctx context.Context, campaignID, sessionID string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "", nil
	}
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM game_sessions WHERE id = ? AND campaign_id = ?`, sessionID, campaignID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: session %s", campaign.ErrNotFound, sessionID)
	}
	if err != nil {
		return "", fmt.Errorf("check session: %w", err)
	}
	return sessionID, nil
}

/* ---------- small helpers ---------- */

func decodePayload(s string) map[string]any {
	out := map[string]any{}
	if s == "" || s == "{}" || s == "null" {
		return out
	}
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}
