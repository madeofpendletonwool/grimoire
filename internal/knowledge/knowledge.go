// Package knowledge is the campaign's epistemic layer: who knows what, how
// they found out, and the scope-enforced retrieval built on both.
//
// The rule this package exists to enforce (ADR 2, docs/campaign/epistemics.md):
//
//	Perspective is authorization, not instruction.
//
// A scope decides which rows come back from SQLite. It is never expressed to
// a model as "do not tell the players about the vampire", and it is never
// applied after rows are loaded — every query below joins the authorization
// into the SQL itself.
//
// Two stores live here:
//
//   - Store, the wide store the DM paths use. It serves every scope, including
//     the non-DM ones (a granted fact — one awareness says the knower holds it
//     — flows to exactly the knower's scope; that is how NPC simulation and
//     the party's own view work).
//   - PlayerView, the narrow interface player-facing handlers are wired to
//     (ADR 6). It has no method capable of returning a secret-visibility or
//     proposed row at all: secrets are excluded even when the party's
//     awareness grants them, so a leak becomes a compile error rather than
//     something review has to catch.
//
// A fact is "granted" to a knower when an awareness row for that knower holds
// stance knows, suspects or believes_false on it — everything but a deliberate
// unaware. A fact no scope is granted is invisible to every non-DM read path,
// public or secret: the player Grimoire is physically incapable of retrieving
// a fact the party has not discovered. Stance unaware is stored, not inferred
// from absence, so "they walked past the ledger" is a row too.
//
// confidence 'proposed' is invisible to every scope including the DM's (ADR
// 3): candidates are staged for human review, never served.
package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// Scope is who is asking. The type is defined in internal/campaign and
// aliased here (the ADRs call it knowledge.Scope) because knowledge imports
// campaign for the graph types — defining it here would be an import cycle.
type Scope = campaign.Scope

// The scope constructors, re-exported so callers of this package never need
// the campaign package for a scope.
var (
	ScopeDM        = campaign.ScopeDM
	ScopeParty     = campaign.ScopeParty
	ScopeCharacter = campaign.ScopeCharacter
	ScopeNPC       = campaign.ScopeNPC
)

// Errors. The knowledge layer reuses the campaign vocabulary: callers branch
// on one set of sentinels across both packages.
var (
	ErrNotFound = campaign.ErrNotFound
	ErrInvalid  = campaign.ErrInvalid
	// ErrScope marks a read the given perspective is not permitted — most
	// often a DM-only path handed a non-DM scope.
	ErrScope = campaign.ErrScope
)

// Store reads (and records) the knowledge layer on the shared database
// handle. The schema is owned by migration 0003; this package creates no
// tables, following the pattern internal/campaign set.
type Store struct {
	db *sql.DB
}

// New builds a knowledge store on an open, migrated database handle.
func New(db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("knowledge: nil database handle")
	}
	return &Store{db: db}, nil
}

// knowers is the set of awareness knower values a scope reads: the party
// scope reads the party's shared rows; a character reads their own rows plus
// the party's; an npc reads only its own. The DM reads none of them — the DM
// scope bypasses the awareness gate entirely.
func knowers(scope Scope) []string {
	switch scope.Kind() {
	case campaign.ScopeKindParty:
		return []string{campaign.PartyKnower}
	case campaign.ScopeKindCharacter:
		return []string{scope.EntityID(), campaign.PartyKnower}
	case campaign.ScopeKindNPC:
		return []string{scope.EntityID()}
	default:
		return nil
	}
}

// grantPlaceholders renders (?,...) for a knower set.
func grantPlaceholders(n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += "?,"
	}
	if out == "" {
		return "(NULL)"
	}
	return "(" + out[:len(out)-1] + ")"
}

// validateKnower checks that knower is 'party' or a live entity of the
// campaign. It returns ErrNotFound for foreign ids, the same as missing ones.
func (s *Store) validateKnower(ctx context.Context, knower, campaignID string) error {
	if knower == campaign.PartyKnower {
		return nil
	}
	var status string
	err := s.db.QueryRowContext(ctx,
		`SELECT status FROM entities WHERE id = ? AND campaign_id = ?`, knower, campaignID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: knower %s", ErrNotFound, knower)
	}
	if err != nil {
		return fmt.Errorf("check knower: %w", err)
	}
	if status == campaign.StatusDeleted {
		return fmt.Errorf("%w: knower %s is deleted", ErrInvalid, knower)
	}
	return nil
}

// resolveScope checks that a non-DM scope's bound entity belongs to the
// campaign being read. Character scopes must point at a pc — that is what
// campaign_members.character_id resolves them through (ADR 4).
func (s *Store) resolveScope(ctx context.Context, scope Scope, campaignID string) error {
	if scope.IsDM() || scope.Kind() == campaign.ScopeKindParty {
		return nil
	}
	var kind string
	err := s.db.QueryRowContext(ctx,
		`SELECT kind FROM entities WHERE id = ? AND campaign_id = ?`, scope.EntityID(), campaignID).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: scope entity %s", ErrNotFound, scope.EntityID())
	}
	if err != nil {
		return fmt.Errorf("check scope entity: %w", err)
	}
	if scope.Kind() == campaign.ScopeKindCharacter && kind != campaign.KindPC {
		return fmt.Errorf("%w: character scope %s is not a pc", ErrScope, scope)
	}
	return nil
}
