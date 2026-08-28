// Package uistate persists the Campaign OS interface state: saved workspace
// layouts and per-user preferences.
//
// The split of responsibility with the front end is deliberate. This package
// validates the *shape* of a layout — node kinds, nesting depth, node count,
// the character set of an identifier — because those are integrity and abuse
// concerns and the server is the only place they can be enforced. It does not
// validate *which tools exist*: the tool registry lives in
// web/static/js/wm/registry.js, and mirroring it here would mean every new
// tool needed a Go change, which is the coupling the window manager exists to
// remove. A layout naming a tool this release no longer ships is well-formed;
// the client drops that leaf on load and reports it (see tree.js parse).
package uistate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Limits on a stored layout. These mirror the caps in tree.js so a payload
// that round-trips through the browser cannot be rejected on the way back.
const (
	MaxNodes    = 200
	MaxDepth    = 12
	MaxTreeSize = 64 << 10 // 64 KiB of JSON
	MaxNameLen  = 80
	MaxToolLen  = 64
	MaxPrefKey  = 64
	MaxPrefVal  = 512
	MaxPrefs    = 64
	MinSlot     = 1
	MaxSlot     = 9
)

// ErrInvalid marks input the caller got wrong, so handlers can answer 400
// rather than 500. Wrapped with a reason in every case.
var ErrInvalid = errors.New("invalid layout")

var toolID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// Layout is one workspace slot: the Alt+N index, its name, and the serialised
// container tree the front end owns.
type Layout struct {
	Corpus    string          `json:"corpus"`
	Slot      int             `json:"slot"`
	Name      string          `json:"name"`
	Tree      json.RawMessage `json:"tree"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// Store reads and writes interface state in the shared SQLite database. The
// schema is owned by internal/migrate (0017); this package ships no DDL.
type Store struct {
	db *sql.DB
}

func New(db *sql.DB) *Store { return &Store{db: db} }

/* ---------- layouts ---------- */

// Layouts returns every saved workspace for one user and corpus, in slot
// order. An account that has never saved one gets an empty slice, not an
// error — the client seeds it from the presets.
func (s *Store) Layouts(ctx context.Context, userID, corpus string) ([]Layout, error) {
	if err := ValidateCorpus(corpus); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT corpus, slot, name, tree, updated_at
		  FROM user_layouts
		 WHERE user_id = ? AND corpus = ?
		 ORDER BY slot`, userID, corpus)
	if err != nil {
		return nil, fmt.Errorf("uistate: list layouts: %w", err)
	}
	defer rows.Close()

	var out []Layout
	for rows.Next() {
		var l Layout
		var tree string
		var updated int64
		if err := rows.Scan(&l.Corpus, &l.Slot, &l.Name, &tree, &updated); err != nil {
			return nil, fmt.Errorf("uistate: scan layout: %w", err)
		}
		l.Tree = json.RawMessage(tree)
		l.UpdatedAt = time.Unix(updated, 0).UTC()
		out = append(out, l)
	}
	return out, rows.Err()
}

// SaveLayout writes one slot, replacing whatever was there. Validation runs
// first so a malformed tree never reaches the table: a layout is read back at
// every sign-in, and one that cannot be parsed would break the shell rather
// than one window.
func (s *Store) SaveLayout(ctx context.Context, userID string, l Layout) error {
	if err := ValidateCorpus(l.Corpus); err != nil {
		return err
	}
	if l.Slot < MinSlot || l.Slot > MaxSlot {
		return fmt.Errorf("%w: slot %d outside %d-%d", ErrInvalid, l.Slot, MinSlot, MaxSlot)
	}
	if l.Name == "" || len(l.Name) > MaxNameLen {
		return fmt.Errorf("%w: name must be 1-%d characters", ErrInvalid, MaxNameLen)
	}
	if err := ValidateTree(l.Tree); err != nil {
		return err
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO user_layouts (user_id, corpus, slot, name, tree, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, corpus, slot) DO UPDATE SET
			name = excluded.name, tree = excluded.tree, updated_at = excluded.updated_at`,
		userID, l.Corpus, l.Slot, l.Name, string(l.Tree), time.Now().Unix())
	if err != nil {
		return fmt.Errorf("uistate: save layout: %w", err)
	}
	return nil
}

// DeleteLayout clears one slot, which is how "reset to the preset" works: the
// client re-seeds from the preset next time it loads.
func (s *Store) DeleteLayout(ctx context.Context, userID, corpus string, slot int) error {
	if err := ValidateCorpus(corpus); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM user_layouts WHERE user_id = ? AND corpus = ? AND slot = ?`,
		userID, corpus, slot)
	if err != nil {
		return fmt.Errorf("uistate: delete layout: %w", err)
	}
	return nil
}

/* ---------- preferences ---------- */

// Prefs returns every stored preference for a user. Never nil, so callers can
// index it without a check.
func (s *Store) Prefs(ctx context.Context, userID string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, value FROM user_prefs WHERE user_id = ?`, userID)
	if err != nil {
		return nil, fmt.Errorf("uistate: list prefs: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("uistate: scan pref: %w", err)
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SetPrefs merges the given keys into the user's preferences. Absent keys are
// left alone, so the client can save one setting without shipping the rest.
func (s *Store) SetPrefs(ctx context.Context, userID string, prefs map[string]string) error {
	if len(prefs) > MaxPrefs {
		return fmt.Errorf("%w: at most %d preferences per request", ErrInvalid, MaxPrefs)
	}
	for k, v := range prefs {
		if k == "" || len(k) > MaxPrefKey {
			return fmt.Errorf("%w: preference key must be 1-%d characters", ErrInvalid, MaxPrefKey)
		}
		if len(v) > MaxPrefVal {
			return fmt.Errorf("%w: preference %q exceeds %d characters", ErrInvalid, k, MaxPrefVal)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("uistate: begin: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	for k, v := range prefs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO user_prefs (user_id, key, value, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(user_id, key) DO UPDATE SET
				value = excluded.value, updated_at = excluded.updated_at`,
			userID, k, v, now); err != nil {
			return fmt.Errorf("uistate: set pref %q: %w", k, err)
		}
	}
	return tx.Commit()
}

/* ---------- validation ---------- */

// ValidateCorpus rejects anything but the two rule sets. The column has a
// CHECK constraint too; this turns the failure into a 400 with a reason
// instead of a driver error.
func ValidateCorpus(corpus string) error {
	if corpus != "mtg" && corpus != "dnd" {
		return fmt.Errorf("%w: corpus must be mtg or dnd, got %q", ErrInvalid, clip(corpus))
	}
	return nil
}

// ValidateTree checks that a serialised layout is well-formed and bounded.
//
// It is deliberately structural. A tree that nests 400 deep or names a tool
// with 2 MB of text is an integrity problem the server must refuse; a tree
// naming a tool that no longer exists is a product question the client
// answers by dropping that leaf.
func ValidateTree(raw json.RawMessage) error {
	if len(raw) == 0 {
		return fmt.Errorf("%w: tree is empty", ErrInvalid)
	}
	if len(raw) > MaxTreeSize {
		return fmt.Errorf("%w: tree exceeds %d bytes", ErrInvalid, MaxTreeSize)
	}

	// An empty workspace serialises as null, which is legal and means "no
	// windows open in this slot".
	var probe any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return fmt.Errorf("%w: tree is not valid JSON: %v", ErrInvalid, err)
	}
	if probe == nil {
		return nil
	}

	var node treeNode
	if err := json.Unmarshal(raw, &node); err != nil {
		return fmt.Errorf("%w: tree is not a layout node: %v", ErrInvalid, err)
	}
	count := 0
	return walk(&node, 0, &count)
}

type treeNode struct {
	T      string     `json:"t"`
	Tool   string     `json:"tool"`
	Dir    string     `json:"dir"`
	Active *int       `json:"active"`
	Fr     []float64  `json:"fr"`
	Kids   []treeNode `json:"kids"`
}

func walk(n *treeNode, depth int, count *int) error {
	if depth > MaxDepth {
		return fmt.Errorf("%w: tree deeper than %d", ErrInvalid, MaxDepth)
	}
	if *count++; *count > MaxNodes {
		return fmt.Errorf("%w: tree has more than %d nodes", ErrInvalid, MaxNodes)
	}

	switch n.T {
	case "leaf":
		if !toolID.MatchString(n.Tool) {
			return fmt.Errorf("%w: %q is not a tool id", ErrInvalid, clip(n.Tool))
		}
		if len(n.Kids) != 0 {
			return fmt.Errorf("%w: a leaf cannot have children", ErrInvalid)
		}
		return nil

	case "split":
		if n.Dir != "row" && n.Dir != "col" {
			return fmt.Errorf("%w: split dir must be row or col, got %q", ErrInvalid, clip(n.Dir))
		}
		if len(n.Fr) != 0 && len(n.Fr) != len(n.Kids) {
			return fmt.Errorf("%w: split has %d fractions for %d children", ErrInvalid, len(n.Fr), len(n.Kids))
		}
		for _, f := range n.Fr {
			if f <= 0 {
				return fmt.Errorf("%w: split fraction must be positive", ErrInvalid)
			}
		}

	case "tabs":
		if n.Active != nil && (*n.Active < 0 || *n.Active >= len(n.Kids)) {
			return fmt.Errorf("%w: active tab %d outside 0-%d", ErrInvalid, *n.Active, len(n.Kids)-1)
		}

	default:
		return fmt.Errorf("%w: unknown node kind %q", ErrInvalid, clip(n.T))
	}

	if len(n.Kids) == 0 {
		return fmt.Errorf("%w: a %s container needs children", ErrInvalid, n.T)
	}
	for i := range n.Kids {
		if err := walk(&n.Kids[i], depth+1, count); err != nil {
			return err
		}
	}
	return nil
}

// clip keeps a rejected value out of the response at full length — the reason
// is echoed to the client, and an unbounded field should not be.
func clip(s string) string {
	if len(s) > MaxToolLen {
		return s[:MaxToolLen] + "…"
	}
	return s
}
