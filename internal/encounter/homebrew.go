package encounter

// Homebrew monsters (MAD-382): the monster designer's persistence. A
// designed creature is planning material in its own table — never a row in
// the `bestiary` mirror, which is a cache of an upstream sync that
// Catalog.Sync replaces wholesale. Homebrew written there would be
// destroyed on the next refresh, and worse, would silently become "SRD" to
// every surface that trusts the mirror; instead it rides beside the SRD as
// an explicit overlay (Overlay), tagged as homebrew at every hop.
//
// The one rule with teeth: there is no path to save a monster without its
// computed CR and the calculator's reasoning. Create and Update run
// statblock.ComputeCR over the stored statblock themselves and ignore any
// computed value a caller supplies — the label a homebrew monster carries
// is always this server's arithmetic, never the model's claim about itself.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/madeofpendletonwool/grimoire/internal/statblock"
)

// Source is where a homebrew monster came from. The designer writes
// designed; the save endpoint also accepts hand for a statblock the DM
// typed in themselves. The third-party importer (its own issue) names its
// own values when it lands.
const (
	HomebrewDesigned = "designed"
	HomebrewHand     = "hand"
)

// EncounterRoles is the declared vocabulary an encounter role draws from —
// the niche a designed creature fills in a fight, the same split the
// builder's pool tiers use. The role rides the homebrew overlay's tags, so
// a pool built with homebrew in it can tier a designed creature the way it
// tiers the SRD.
var EncounterRoles = []string{
	"boss", "brute", "controller", "artillery", "skirmisher", "support", "minion", "wildcard",
}

// HomebrewMonster is one designed creature: the full structured statblock,
// the CR the brief asked for, the CR the calculator actually computed, the
// calculator's whole reasoning, and the designer's prose — tactics, lore
// and the encounter role.
type HomebrewMonster struct {
	ID          string              `json:"id"`
	OwnerID     string              `json:"owner_id"`
	CampaignID  string              `json:"campaign_id,omitempty"`
	Name        string              `json:"name"`
	Slug        string              `json:"slug"`
	Statblock   statblock.Statblock `json:"statblock"`
	RequestedCR string              `json:"requested_cr"`
	ComputedCR  string              `json:"computed_cr"`
	Rating      statblock.Rating    `json:"computed_detail"`
	Tactics     string              `json:"tactics,omitempty"`
	Lore        string              `json:"lore,omitempty"`
	Role        string              `json:"encounter_role,omitempty"`
	Source      string              `json:"source"`
	CreatedAt   time.Time           `json:"created_at"`
	UpdatedAt   time.Time           `json:"updated_at"`
}

// Creature renders the monster in the catalog's read shape, tagged
// homebrew, so a designed creature can be picked into a pool, resolved by
// name in a design, rendered as a statblock and priced by the existing XP
// arithmetic — always distinguishable from an SRD entry.
func (m *HomebrewMonster) Creature() Creature {
	sb := m.Statblock
	c := Creature{
		Slug:       m.Slug,
		Name:       m.Name,
		Doc:        "homebrew",
		CR:         m.ComputedCR,
		CRNum:      m.Rating.CR,
		XP:         xpForLabel(m.ComputedCR),
		Type:       sb.Type,
		Size:       sb.Size,
		AC:         sb.AC,
		HP:         sb.HP,
		HitDice:    sb.HitDice,
		Abilities:  &sb.Abilities,
		Saves:      sb.Saves,
		Skills:     sb.Skills,
		ProfBonus:  sb.ProfBonus,
		Resist:     strings.Join(sb.Resist, ", "),
		Immune:     strings.Join(sb.Immune, ", "),
		Vulnerable: strings.Join(sb.Vulnerable, ", "),
		Tags:       []string{"homebrew"},
		Homebrew:   true,
	}
	if sb.Lair {
		c.LairAction = true
	}
	for _, sp := range sb.Speeds {
		if sp > 0 {
			c.Speeds = sb.Speeds
			break
		}
	}
	for _, t := range sb.Traits {
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		c.Traits = append(c.Traits, NamedText{Name: t.Name, Desc: t.Desc})
		if strings.Contains(strings.ToLower(t.Name), "spellcasting") {
			c.Spellcasting = true
		}
		if strings.Contains(strings.ToLower(t.Name), "lair action") {
			c.LairAction = true
		}
	}
	for _, a := range sb.Actions {
		if strings.TrimSpace(a.Name) == "" {
			continue
		}
		c.Actions = append(c.Actions, NamedText{
			Name: a.Name, Desc: a.Desc, Kind: a.Kind, Usage: a.Usage, Cost: a.Cost,
		})
		if strings.Contains(strings.ToLower(a.Name), "spellcasting") {
			c.Spellcasting = true
		}
		// The same never-half-parsed contract the mirror keeps: each action
		// is either a structured Attack or explicitly unparsed. A stored
		// design already carries its parse; a hand-entered one is parsed
		// here, now, on the way out.
		if a.Parsed && a.Attack.Parsed() {
			c.Attacks = append(c.Attacks, a.Attack)
			continue
		}
		if atk, ok := statblock.ParseAttack(a.Name, a.Desc); ok {
			c.Attacks = append(c.Attacks, atk)
		} else {
			c.Unparsed = append(c.Unparsed, UnparsedAction{Name: a.Name, Desc: a.Desc})
		}
	}
	c.Tags = append(c.Tags, deriveTags(c)...)
	if m.Role != "" {
		c.Tags = append(c.Tags, m.Role)
	}
	return c
}

// HomebrewInput is what a save takes. ComputedCR and Rating are
// deliberately absent: the store computes them, always.
type HomebrewInput struct {
	Name        string
	CampaignID  string
	Statblock   *statblock.Statblock
	RequestedCR string
	Tactics     string
	Lore        string
	Role        string
	Source      string
}

// HomebrewStore owns the homebrew_monsters table. It sits on the same
// shared database handle every store uses; the table itself is
// migration-owned (0027), the campaign-core pattern — no package DDL for it.
type HomebrewStore struct {
	db *sql.DB
}

// NewHomebrewStore builds the homebrew store over an open, migrated handle.
func NewHomebrewStore(db *sql.DB) *HomebrewStore {
	return &HomebrewStore{db: db}
}

// Save inserts (or replaces, when id is non-empty and belongs to the
// owner) a homebrew monster, computing the CR and keeping the calculator's
// full reasoning. It is the one write path, and it enforces the contract:
// a statblock is required, a name is required, and what lands in
// computed_cr/computed_detail is this server's arithmetic.
func (s *HomebrewStore) Save(ctx context.Context, owner, id string, in HomebrewInput) (*HomebrewMonster, error) {
	if strings.TrimSpace(owner) == "" {
		return nil, fmt.Errorf("%w: homebrew monsters need an owner", ErrInvalid)
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: a homebrew monster needs a name", ErrInvalid)
	}
	if in.Statblock == nil {
		return nil, fmt.Errorf("%w: a homebrew monster needs a statblock", ErrInvalid)
	}
	if len(in.Statblock.Actions) == 0 {
		return nil, fmt.Errorf("%w: a homebrew monster needs at least one action — the calculator must have something to price", ErrInvalid)
	}
	sb := *in.Statblock
	sb.Name = name
	// The stored actions carry their parse — the same one-bucket-or-the-
	// other contract every statblock in the app keeps. Re-parse on the way
	// in so a stored design is never trusted over the deterministic parser.
	for i := range sb.Actions {
		if atk, ok := statblock.ParseAttack(sb.Actions[i].Name, sb.Actions[i].Desc); ok {
			sb.Actions[i].Parsed = true
			sb.Actions[i].Attack = atk
			sb.Actions[i].Unparse = ""
		} else {
			sb.Actions[i].Parsed = false
			sb.Actions[i].Attack = statblock.Attack{}
			sb.Actions[i].Unparse = "not parseable into a structured attack; prose kept verbatim"
		}
	}
	// The legendary flag is derived, not asserted: a creature with
	// legendary actions is legendary, whatever the draft claimed.
	for _, a := range sb.Actions {
		if a.Legendary() {
			sb.Legendary = true
		}
	}
	if sb.ProfBonus <= 0 {
		if cr, ok := statblock.ParseLabel(in.RequestedCR); ok {
			sb.ProfBonus = statblock.ProficiencyFor(cr)
		}
	}
	rating := statblock.ComputeCR(sb)

	role := strings.TrimSpace(in.Role)
	if role != "" && !validRole(role) {
		return nil, fmt.Errorf("%w: encounter role %q is not one of [%s]", ErrInvalid, role, strings.Join(EncounterRoles, ", "))
	}
	source := strings.TrimSpace(in.Source)
	if source == "" {
		source = HomebrewDesigned
	}
	if source != HomebrewDesigned && source != HomebrewHand {
		return nil, fmt.Errorf("%w: homebrew source %q is not one of [%s, %s]", ErrInvalid, source, HomebrewDesigned, HomebrewHand)
	}

	now := time.Now().UTC().UnixMilli()
	m := &HomebrewMonster{
		ID: id, OwnerID: owner, CampaignID: strings.TrimSpace(in.CampaignID),
		Name: name, Slug: homebrewSlug(name),
		Statblock:   sb,
		RequestedCR: strings.TrimSpace(in.RequestedCR),
		ComputedCR:  rating.Label, Rating: rating,
		Tactics: strings.TrimSpace(in.Tactics), Lore: strings.TrimSpace(in.Lore),
		Role: role, Source: source,
	}
	if m.ComputedCR == "" {
		// ComputeCR is total — this is a belt-and-braces guard the compiler
		// cannot prove, and the contract allows no way past it.
		return nil, fmt.Errorf("%w: the calculator returned no rating for %q", ErrInvalid, name)
	}

	sbJSON, err := json.Marshal(sb)
	if err != nil {
		return nil, fmt.Errorf("encode statblock: %w", err)
	}
	ratingJSON, err := json.Marshal(rating)
	if err != nil {
		return nil, fmt.Errorf("encode computed detail: %w", err)
	}

	if id == "" {
		m.ID = newID()
		m.CreatedAt = time.UnixMilli(now).UTC()
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO homebrew_monsters (id, owner_id, campaign_id, name, slug, statblock,
				requested_cr, computed_cr, computed_detail, tactics, lore, encounter_role, source, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.ID, m.OwnerID, m.CampaignID, m.Name, m.Slug, string(sbJSON),
			m.RequestedCR, m.ComputedCR, string(ratingJSON), m.Tactics, m.Lore, m.Role, m.Source, now, now); err != nil {
			return nil, fmt.Errorf("insert homebrew monster: %w", err)
		}
		return m, nil
	}

	// A replace keeps the row's created_at and its campaign scope unless
	// the input names one.
	res, err := s.db.ExecContext(ctx, `
		UPDATE homebrew_monsters SET name = ?, slug = ?, statblock = ?, requested_cr = ?,
			computed_cr = ?, computed_detail = ?, tactics = ?, lore = ?, encounter_role = ?,
			source = ?, campaign_id = CASE WHEN ? <> '' THEN ? ELSE campaign_id END, updated_at = ?
		WHERE id = ? AND owner_id = ?`,
		m.Name, m.Slug, string(sbJSON), m.RequestedCR,
		m.ComputedCR, string(ratingJSON), m.Tactics, m.Lore, m.Role,
		m.Source, m.CampaignID, m.CampaignID, now, id, owner)
	if err != nil {
		return nil, fmt.Errorf("update homebrew monster: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("%w: homebrew monster %s", ErrNotFound, id)
	}
	// The replace keeps the row's created_at and its campaign scope unless
	// the input named one — read the row back so the return carries what
	// actually landed.
	return s.Get(ctx, owner, id)
}

// Get reads one homebrew monster by id. Foreign owners are indistinguishable
// from missing ids, the same rule the encounter store sets.
func (s *HomebrewStore) Get(ctx context.Context, owner, id string) (*HomebrewMonster, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+homebrewCols+` FROM homebrew_monsters WHERE id = ? AND owner_id = ?`, id, owner)
	m, err := scanHomebrew(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: homebrew monster %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}

// List reads an owner's homebrew monsters, newest edit first. With a
// campaign named, the campaign's own monsters come first; the owner's
// unscoped designs follow, so a campaign view is still the whole shelf the
// DM can reach for.
func (s *HomebrewStore) List(ctx context.Context, owner, campaignID string) ([]HomebrewMonster, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+homebrewCols+` FROM homebrew_monsters
		WHERE owner_id = ? AND (? = '' OR campaign_id = '' OR campaign_id = ?)
		ORDER BY (campaign_id = '') ASC, updated_at DESC`, owner, campaignID, campaignID)
	if err != nil {
		return nil, fmt.Errorf("list homebrew monsters: %w", err)
	}
	defer rows.Close()
	var out []HomebrewMonster
	for rows.Next() {
		m, err := scanHomebrew(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// Delete removes one homebrew monster. A staged placement batch is not
// affected — its entity rows live in the campaign graph, not here.
func (s *HomebrewStore) Delete(ctx context.Context, owner, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM homebrew_monsters WHERE id = ? AND owner_id = ?`, id, owner)
	if err != nil {
		return fmt.Errorf("delete homebrew monster: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: homebrew monster %s", ErrNotFound, id)
	}
	return nil
}

// Overlay loads an owner's homebrew as the catalog read shape: the
// explicit layer the catalog's Filter, Lookup and Search take. With a
// campaign named, that campaign's monsters ride along with the owner's
// unscoped designs — the same shelf List shows.
func (s *HomebrewStore) Overlay(ctx context.Context, owner, campaignID string) ([]Creature, error) {
	monsters, err := s.List(ctx, owner, campaignID)
	if err != nil {
		return nil, err
	}
	out := make([]Creature, 0, len(monsters))
	for i := range monsters {
		out = append(out, monsters[i].Creature())
	}
	return out, nil
}

// CampaignNames reads one campaign's homebrew names — the loader the canon
// engine uses to resolve planned encounters against the overlay.
func (s *HomebrewStore) CampaignNames(ctx context.Context, campaignID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM homebrew_monsters WHERE campaign_id = ?`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("list campaign homebrew names: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

const homebrewCols = `id, owner_id, campaign_id, name, slug, statblock, requested_cr, computed_cr, computed_detail, tactics, lore, encounter_role, source, created_at, updated_at`

func scanHomebrew(row interface{ Scan(...any) error }) (*HomebrewMonster, error) {
	var (
		m          HomebrewMonster
		sbJSON     string
		ratingJSON string
		created    int64
		updated    int64
	)
	if err := row.Scan(&m.ID, &m.OwnerID, &m.CampaignID, &m.Name, &m.Slug, &sbJSON,
		&m.RequestedCR, &m.ComputedCR, &ratingJSON, &m.Tactics, &m.Lore, &m.Role, &m.Source, &created, &updated); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(sbJSON), &m.Statblock); err != nil {
		return nil, fmt.Errorf("homebrew %s carries an unreadable statblock: %w", m.ID, err)
	}
	// The rating is the row's own reasoning; a row that predates a schema
	// change in the Rating shape still loads — the computed CR label is the
	// contract, the detail is advisory on read.
	if ratingJSON != "" {
		if err := json.Unmarshal([]byte(ratingJSON), &m.Rating); err != nil {
			m.Rating = statblock.Rating{Label: m.ComputedCR, CR: crValue(m.ComputedCR)}
		}
	}
	if m.Rating.Label == "" {
		m.Rating.Label = m.ComputedCR
		m.Rating.CR = crValue(m.ComputedCR)
	}
	m.CreatedAt = time.UnixMilli(created).UTC()
	m.UpdatedAt = time.UnixMilli(updated).UTC()
	return &m, nil
}

func validRole(role string) bool {
	for _, r := range EncounterRoles {
		if r == role {
			return true
		}
	}
	return false
}

// homebrewSlug is the catalog's squash: the key every lookup resolves on.
func homebrewSlug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// xpForLabel prices a computed CR off the Monster Manual table, the same
// arithmetic every roster uses. An unreadable label prices at zero — which
// shows up immediately in the builder's XP readout rather than hiding.
func xpForLabel(label string) int {
	xp, _ := CRXP(label)
	return xp
}
