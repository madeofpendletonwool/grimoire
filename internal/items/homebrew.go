package items

// Homebrew items (MAD-383): the item designer's persistence. A designed
// item is planning material in its own table — never a row in the
// `magic_items` mirror, which is a cache of an upstream sync that
// Catalog.Sync replaces wholesale. Homebrew written there would be
// destroyed on the next refresh, and worse, would silently become "SRD"
// to every surface that trusts the mirror; instead it rides beside the
// SRD as an explicit overlay (Overlay), tagged as homebrew at every hop.
//
// The one rule with teeth: there is no path to save an item that fails
// the structural rules. Create and Update run Design.Validate over the
// design themselves — bad attunement, an ungrammatical recharge, an
// effect with no game-vocabulary expression are all refused — and the
// rarity a row carries is always the DM's own label, echoed, never the
// server's verdict. The designer does not compute one.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// Source is where a homebrew item came from. The designer writes
// designed; the save endpoint also accepts hand for a design the DM
// entered themselves. A third-party importer would name its own values
// when it lands.
const (
	ItemDesigned = "designed"
	ItemHand     = "hand"
)

// HomebrewItem is one designed item: the full structured design, the
// rarity the DM asked for, the derived tags, and the designer's prose.
type HomebrewItem struct {
	ID         string `json:"id"`
	OwnerID    string `json:"owner_id,omitempty"`
	CampaignID string `json:"campaign_id,omitempty"`
	Name       string `json:"name"`
	Slug       string `json:"slug"`
	Design     Design `json:"design"`
	// RequestedRarity is the DM's own label — echoed, never judged.
	RequestedRarity string    `json:"requested_rarity"`
	Tags            []string  `json:"tags,omitempty"`
	Notes           string    `json:"notes,omitempty"` // who made it, and why it exists
	Source          string    `json:"source"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Item renders the designed item in the catalog's read shape, tagged
// homebrew, so it can be searched, filtered, compared against the bands
// and resolved by name — always distinguishable from SRD material. The
// rarity it carries is the DM's own label.
func (m *HomebrewItem) Item() Item {
	it := m.Design.Item("homebrew")
	it.Rarity = m.RequestedRarity
	it.Homebrew = true
	if !hasTag(it.Tags, "homebrew") {
		it.Tags = append([]string{"homebrew"}, it.Tags...)
	}
	return it
}

// HomebrewInput is what a save takes. The rarity is the DM's label; the
// server has no computed rarity to supply or ignore — it validates the
// design and derives the tags, and that is all it derives.
type HomebrewInput struct {
	Name       string
	CampaignID string
	Design     *Design
	Notes      string
	Source     string
}

// HomebrewStore owns the homebrew_items table. It sits on the same shared
// database handle every store uses; the table itself is migration-owned
// (0028), the campaign-core pattern — no package DDL for it.
type HomebrewStore struct {
	db *sql.DB
}

// NewHomebrewStore builds the homebrew item store over an open, migrated
// handle.
func NewHomebrewStore(db *sql.DB) *HomebrewStore {
	return &HomebrewStore{db: db}
}

// Save inserts (or replaces, when id is non-empty and belongs to the
// owner) a homebrew item. It is the one write path, and it enforces the
// contract: a name is required, the design must pass the structural
// rules, and the tags a row carries are the server's derivation of what
// the design says.
func (s *HomebrewStore) Save(ctx context.Context, owner, id string, in HomebrewInput) (*HomebrewItem, error) {
	if strings.TrimSpace(owner) == "" {
		return nil, fmt.Errorf("%w: homebrew items need an owner", ErrInvalid)
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: a homebrew item needs a name", ErrInvalid)
	}
	if in.Design == nil {
		return nil, fmt.Errorf("%w: a homebrew item needs a design", ErrInvalid)
	}
	d := *in.Design
	d.Name = name
	if problems := d.Validate(); len(problems) > 0 {
		return nil, fmt.Errorf("%w: the design fails the structural rules: %s", ErrInvalid, strings.Join(problems, "; "))
	}

	source := strings.TrimSpace(in.Source)
	if source == "" {
		source = ItemDesigned
	}
	if source != ItemDesigned && source != ItemHand {
		return nil, fmt.Errorf("%w: homebrew source %q is not one of [%s, %s]", ErrInvalid, source, ItemDesigned, ItemHand)
	}

	rendered := d.Item("")
	now := time.Now().UTC().UnixMilli()
	m := &HomebrewItem{
		ID: id, OwnerID: owner, CampaignID: strings.TrimSpace(in.CampaignID),
		Name: name, Slug: itemSlug(name),
		Design: d, RequestedRarity: strings.TrimSpace(d.Rarity),
		Tags:  append([]string{"homebrew"}, rendered.Tags...),
		Notes: strings.TrimSpace(in.Notes), Source: source,
	}

	designJSON, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("encode design: %w", err)
	}

	if id == "" {
		m.ID = newID()
		m.CreatedAt = time.UnixMilli(now).UTC()
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO homebrew_items (id, owner_id, campaign_id, name, slug, design,
				requested_rarity, tags, notes, source, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.ID, m.OwnerID, m.CampaignID, m.Name, m.Slug, string(designJSON),
			m.RequestedRarity, strings.Join(m.Tags, ","), m.Notes, m.Source, now, now); err != nil {
			return nil, fmt.Errorf("insert homebrew item: %w", err)
		}
		return m, nil
	}

	// A replace keeps the row's created_at and its campaign scope unless
	// the input names one.
	res, err := s.db.ExecContext(ctx, `
		UPDATE homebrew_items SET name = ?, slug = ?, design = ?, requested_rarity = ?,
			tags = ?, notes = ?, source = ?,
			campaign_id = CASE WHEN ? <> '' THEN ? ELSE campaign_id END, updated_at = ?
		WHERE id = ? AND owner_id = ?`,
		m.Name, m.Slug, string(designJSON), m.RequestedRarity,
		strings.Join(m.Tags, ","), m.Notes, m.Source,
		m.CampaignID, m.CampaignID, now, id, owner)
	if err != nil {
		return nil, fmt.Errorf("update homebrew item: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("%w: homebrew item %s", ErrNotFound, id)
	}
	// The replace keeps the row's created_at and its campaign scope unless
	// the input named one — read the row back so the return carries what
	// actually landed.
	return s.Get(ctx, owner, id)
}

// Get reads one homebrew item by id. Foreign owners are indistinguishable
// from missing ids, the same rule the encounter store sets.
func (s *HomebrewStore) Get(ctx context.Context, owner, id string) (*HomebrewItem, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+homebrewCols+` FROM homebrew_items WHERE id = ? AND owner_id = ?`, id, owner)
	m, err := scanHomebrew(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: homebrew item %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}

// List reads an owner's homebrew items, newest edit first. With a
// campaign named, the campaign's own items come first; the owner's
// unscoped designs follow, so a campaign view is still the whole shelf
// the DM can reach for.
func (s *HomebrewStore) List(ctx context.Context, owner, campaignID string) ([]HomebrewItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+homebrewCols+` FROM homebrew_items
		WHERE owner_id = ? AND (? = '' OR campaign_id = '' OR campaign_id = ?)
		ORDER BY (campaign_id = '') ASC, updated_at DESC`, owner, campaignID, campaignID)
	if err != nil {
		return nil, fmt.Errorf("list homebrew items: %w", err)
	}
	defer rows.Close()
	var out []HomebrewItem
	for rows.Next() {
		m, err := scanHomebrew(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// Delete removes one homebrew item. A staged placement batch is not
// affected — its entity rows live in the campaign graph, not here.
func (s *HomebrewStore) Delete(ctx context.Context, owner, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM homebrew_items WHERE id = ? AND owner_id = ?`, id, owner)
	if err != nil {
		return fmt.Errorf("delete homebrew item: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: homebrew item %s", ErrNotFound, id)
	}
	return nil
}

// Overlay loads an owner's homebrew as the catalog read shape: the
// explicit layer the catalog's Filter, Lookup, Search and Compare take.
// With a campaign named, that campaign's items ride along with the
// owner's unscoped designs — the same shelf List shows.
func (s *HomebrewStore) Overlay(ctx context.Context, owner, campaignID string) ([]Item, error) {
	list, err := s.List(ctx, owner, campaignID)
	if err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(list))
	for i := range list {
		out = append(out, list[i].Item())
	}
	return out, nil
}

// CampaignNames reads one campaign's homebrew item names — the loader the
// canon engine uses when it resolves the campaign's own material.
func (s *HomebrewStore) CampaignNames(ctx context.Context, campaignID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM homebrew_items WHERE campaign_id = ?`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("list campaign homebrew item names: %w", err)
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

const homebrewCols = `id, owner_id, campaign_id, name, slug, design, requested_rarity, tags, notes, source, created_at, updated_at`

func scanHomebrew(row interface{ Scan(...any) error }) (*HomebrewItem, error) {
	var (
		m          HomebrewItem
		designJSON string
		tagsCSV    string
		created    int64
		updated    int64
	)
	if err := row.Scan(&m.ID, &m.OwnerID, &m.CampaignID, &m.Name, &m.Slug, &designJSON,
		&m.RequestedRarity, &tagsCSV, &m.Notes, &m.Source, &created, &updated); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(designJSON), &m.Design); err != nil {
		return nil, fmt.Errorf("homebrew item %s carries an unreadable design: %w", m.ID, err)
	}
	if tagsCSV != "" {
		m.Tags = strings.Split(tagsCSV, ",")
	}
	m.CreatedAt = time.UnixMilli(created).UTC()
	m.UpdatedAt = time.UnixMilli(updated).UTC()
	return &m, nil
}

// itemSlug is the catalog's squash: the key every lookup resolves on.
func itemSlug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// newID mints an unguessable row id: 16 bytes of crypto randomness, hex.
func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is catastrophic by definition; fall back to a
		// time-derived id rather than panicking inside a request.
		return fmt.Sprintf("e%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
