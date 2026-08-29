package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

/*
The location dossier's scoped snapshot (MAD-370).

campaign.LoadSnapshot is DM material by definition — every fact, secrets and
proposals aside included — so the player dossier cannot run over it. This
file builds the same struct from the scope's own reads instead: the entities
it has met, the facts it holds, the events it witnessed, the edges it may
read, and the public quests sited anywhere in the campaign. place.Dossier
then assembles over it exactly as it does over the DM's snapshot — two
dossiers, one function — and the forbidden material is absent because the
rows were never loaded, not because the assembly blanked a field:

  - no secret fact: the strict facts read drops secrets even when granted;
  - no private truth: non-DM entity reads drop payloads, and the place
    block's public half crosses through a decode struct (placeFacade) that
    has no private_truth field to fill;
  - no unmet NPC: an entity the scope has not met is not in the snapshot,
    and its structural edges (located_in/contains between two visible
    entities) never loaded either.
*/

// placeFacade is the decode target for a location's place block at a non-DM
// scope: exactly the public half of campaign.Place. The DM-only half cannot
// cross the scope line through this struct because there is no field for it
// — the same trick FactionFacade pulls with the agent block's two public
// fields.
type placeFacade struct {
	Kind       string   `json:"kind"`
	Scale      string   `json:"scale"`
	Population string   `json:"population"`
	Government string   `json:"government"`
	Services   []string `json:"services"`
	Defences   string   `json:"defences"`
	Climate    string   `json:"climate"`
	Senses     []string `json:"senses"`
	State      string   `json:"state"`
	Danger     int      `json:"danger"`
}

// PlaceSnapshot builds the location dossier's input snapshot at this scope:
// the entities it may read, the facts it holds, the events it witnessed,
// the aware edges, and the public quests with their site links. At the DM
// scope the payload rides along as stored; at every other scope payloads
// are dropped and only the named location's public place block is grafted
// back. locationID names the location the dossier is for; an id that is not
// a visible location grafts nothing and errors nowhere — visibility of the
// entity itself is the caller's read to make.
func (s *Store) PlaceSnapshot(ctx context.Context, scope Scope, campaignID, locationID string) (*campaign.Snapshot, error) {
	return s.placeSnapshot(ctx, scope, campaignID, locationID, false)
}

func (v *playerView) PlaceSnapshot(ctx context.Context, campaignID, locationID string) (*campaign.Snapshot, error) {
	return v.store.placeSnapshot(ctx, v.scope, campaignID, locationID, true)
}

func (s *Store) placeSnapshot(ctx context.Context, scope Scope, campaignID, locationID string, strict bool) (*campaign.Snapshot, error) {
	entities, err := s.entities(ctx, scope, campaignID, "", strict)
	if err != nil {
		return nil, err
	}
	facts, err := s.facts(ctx, scope, campaignID, FactFilter{}, strict)
	if err != nil {
		return nil, err
	}
	events, err := s.timeline(ctx, scope, campaignID, strict)
	if err != nil {
		return nil, err
	}
	rels, err := s.relationships(ctx, scope, campaignID, strict)
	if err != nil {
		return nil, err
	}
	snap := &campaign.Snapshot{
		CampaignID:    campaignID,
		Entities:      entities,
		Facts:         facts,
		Events:        events,
		Relationships: rels,
	}
	if !scope.IsDM() {
		s.graftPublicPlace(ctx, campaignID, locationID, snap.Entities)
	}
	return snap, s.loadPublicQuests(ctx, campaignID, snap)
}

// graftPublicPlace re-attaches the public half of one location's place
// block to the entity the scoped read handed back payload-less. The raw
// payload row is read here, inside the knowledge layer, so no caller ever
// holds the DM-shaped payload at a player scope.
func (s *Store) graftPublicPlace(ctx context.Context, campaignID, locationID string, entities []campaign.Entity) {
	for i := range entities {
		e := &entities[i]
		if e.ID != locationID || e.Kind != campaign.KindLocation {
			continue
		}
		var payload string
		if err := s.db.QueryRowContext(ctx,
			`SELECT payload FROM entities WHERE id = ? AND campaign_id = ?`, locationID, campaignID).Scan(&payload); err != nil {
			return // unreadable payload grafts nothing; the zero block is valid
		}
		var probe map[string]any
		_ = json.Unmarshal([]byte(payload), &probe)
		raw, ok := probe["place"].(map[string]any)
		if !ok {
			return // no authored block; nothing to graft
		}
		block, err := json.Marshal(raw)
		if err != nil {
			return
		}
		var face placeFacade
		if json.Unmarshal(block, &face) != nil {
			return // malformed block reads as unwritten; the dossier shows the zero block
		}
		if b, err := json.Marshal(face); err == nil {
			var m map[string]any
			if json.Unmarshal(b, &m) == nil {
				e.Payload["place"] = m
			}
		}
		return
	}
}

// loadPublicQuests fills the snapshot's quests and their site links with the
// campaign's public quests — the journal's visibility rule, applied to the
// dossier's "quests sited here" list. Secret quests are DM planning
// material; a player must not learn one exists, let alone that it points
// here.
func (s *Store) loadPublicQuests(ctx context.Context, campaignID string, snap *campaign.Snapshot) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, summary, status FROM quests
		 WHERE campaign_id = ? AND visibility = 'public'
		 ORDER BY name COLLATE NOCASE, id`, campaignID)
	if err != nil {
		return fmt.Errorf("place quests: %w", err)
	}
	for rows.Next() {
		var q campaign.Quest
		if err := rows.Scan(&q.ID, &q.Name, &q.Summary, &q.Status); err != nil {
			rows.Close()
			return err
		}
		q.CampaignID = campaignID
		q.Visibility = campaign.QuestVisibilityPublic
		snap.Quests = append(snap.Quests, q)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	rows, err = s.db.QueryContext(ctx, `
		SELECT l.id, l.quest_id, l.entity_id, l.role, l.created_at
		  FROM quest_entities l JOIN quests q ON q.id = l.quest_id
		 WHERE q.campaign_id = ? AND q.visibility = 'public'
		 ORDER BY l.rowid`, campaignID)
	if err != nil {
		return fmt.Errorf("place quest links: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			l       campaign.QuestEntity
			created int64
		)
		if err := rows.Scan(&l.ID, &l.QuestID, &l.EntityID, &l.Role, &created); err != nil {
			return err
		}
		l.CreatedAt = time.UnixMilli(created).UTC()
		snap.QuestEntities = append(snap.QuestEntities, l)
	}
	return rows.Err()
}
