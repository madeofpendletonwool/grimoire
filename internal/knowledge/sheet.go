package knowledge

// The player's own character sheet read (MAD-418, stage 1 of MAD-417). This
// is the one deliberate widening of the player portal's surface in that
// issue, and it is narrower than it looks: a character-scoped player may
// read the typed sheet of exactly the pc their membership row binds — no
// other pc, no other payload key, no facts, no secrets. It is the sheet a
// player already owns on paper, served as data.
//
// The wide store's entity reads refuse non-DM scopes and the player view's
// drop payloads wholesale ("they are DM structure") — both stay exactly as
// they are. This read is separate by construction: it loads one pc row,
// decodes the "sheet" key, and returns nothing else from the payload. The
// unstructured marker travels: a pc without a typed sheet reads as
// structured=false, which is information the player's surface needs and
// nothing more.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/sheet"
)

// SheetRead is one character's typed sheet as a player may see it: the
// entity's identity, the sheet, and whether a typed sheet backs it at all.
type SheetRead struct {
	EntityID   string      `json:"entity_id"`
	Name       string      `json:"name"`
	Status     string      `json:"status"`
	Sheet      sheet.Sheet `json:"sheet,omitempty"`
	Structured bool        `json:"structured"`
}

// CharacterSheet reads the caller's own bound character's sheet. Party and
// npc scopes are refused: the party has no one sheet, and an npc scope is
// not a player.
func (v *playerView) CharacterSheet(ctx context.Context, campaignID, characterID string) (*SheetRead, error) {
	if v.scope.Kind() != campaign.ScopeKindCharacter {
		return nil, fmt.Errorf("%w: the sheet read belongs to the character it is", ErrScope)
	}
	if v.scope.EntityID() != characterID {
		return nil, fmt.Errorf("%w: a player reads their own character's sheet", ErrScope)
	}
	return v.store.characterSheet(ctx, campaignID, characterID)
}

// characterSheet loads one pc row and returns its typed sheet block — the
// narrow query behind the player view's read. No awareness join: the
// character is the caller's own, which the scope check above established.
func (s *Store) characterSheet(ctx context.Context, campaignID, characterID string) (*SheetRead, error) {
	var name, status, payloadJSON string
	err := s.db.QueryRowContext(ctx,
		`SELECT name, status, payload FROM entities
		 WHERE id = ? AND campaign_id = ? AND kind = ?`,
		characterID, campaignID, campaign.KindPC,
	).Scan(&name, &status, &payloadJSON)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: character %s", ErrNotFound, characterID)
	}
	if err != nil {
		return nil, fmt.Errorf("character sheet read: %w", err)
	}
	var payload map[string]any
	_ = json.Unmarshal([]byte(payloadJSON), &payload)
	sheetVal, has, err := sheet.FromPayload(payload)
	if err != nil {
		// A present-but-undecodable sheet is the unstructured marker with
		// the reason attached, not a failed read.
		return &SheetRead{EntityID: characterID, Name: name, Status: status, Structured: false}, nil
	}
	return &SheetRead{
		EntityID:   characterID,
		Name:       name,
		Status:     status,
		Sheet:      sheetVal,
		Structured: has,
	}, nil
}
