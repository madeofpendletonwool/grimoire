package story

// The spine snapshot: a campaign's whole plan in memory, the one shape
// Validate runs over and the planner view loads. Pure rules, one loader —
// the same split internal/campaign's Snapshot and internal/canon's Snapshot
// set: no database access inside a rule, no rule that needs anything but
// the snapshot.

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// AwarenessGrant is the awareness slice the secret rule joins against: who
// holds what stance on which fact. Only party and pc rows matter to the
// spine — an NPC's knowledge feeds DM-side simulation, not the party's
// reachability.
type AwarenessGrant struct {
	Knower string
	FactID string
	Stance string
}

// Spine is a campaign's acts, scenes (with cast, secrets and outcomes
// attached) and session plans, plus the two joins Validate needs: the
// campaign's quests (for edge checks) and the party-side awareness rows
// (for the already-granted-secret check).
type Spine struct {
	CampaignID string
	Acts       []Act
	Scenes     []Scene
	Plans      []SessionPlan
	Quests     []campaign.Quest
	Awareness  []AwarenessGrant
	// FactStatement renders a secret's text in a finding message; absent
	// for a fact the loader could not see.
	FactStatement map[string]string
	// EntityName renders a cast member's name the same way, and EntityKind
	// says which knowers are party-side (pcs) for the secret rule.
	EntityName map[string]string
	EntityKind map[string]string
}

// LoadSpine reads one campaign's whole spine into memory. DM material by
// definition — secrets, outcomes and prep included — so the callers that
// expose it (the canon engine's loader, the DM-gated API handler) hold the
// DM scope themselves.
func LoadSpine(ctx context.Context, db *sql.DB, campaignID string) (*Spine, error) {
	store, err := New(db)
	if err != nil {
		return nil, err
	}
	sp := &Spine{
		CampaignID:    campaignID,
		FactStatement: map[string]string{},
		EntityName:    map[string]string{},
		EntityKind:    map[string]string{},
	}

	if sp.Acts, err = store.ListActs(ctx, campaign.ScopeDM, campaignID); err != nil {
		return nil, err
	}
	if sp.Scenes, err = store.ListScenes(ctx, campaign.ScopeDM, campaignID, ""); err != nil {
		return nil, err
	}
	for i := range sp.Scenes {
		if err := store.attachSceneDetail(ctx, &sp.Scenes[i]); err != nil {
			return nil, err
		}
	}
	if sp.Plans, err = store.ListPlans(ctx, campaign.ScopeDM, campaignID); err != nil {
		return nil, err
	}

	// The quest machines, for the outcome edge checks.
	graph, err := campaign.New(db)
	if err != nil {
		return nil, err
	}
	if sp.Quests, err = graph.ListQuests(ctx, campaign.ScopeDM, campaignID); err != nil {
		return nil, fmt.Errorf("spine quests: %w", err)
	}

	// Party-side awareness and the names the messages render. A campaign
	// with no knowledge rows simply has no grants; the checks skip.
	rows, err := db.QueryContext(ctx, `
		SELECT knower, fact_id, stance FROM awareness WHERE campaign_id = ?`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("spine awareness: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a AwarenessGrant
		if err := rows.Scan(&a.Knower, &a.FactID, &a.Stance); err != nil {
			return nil, err
		}
		sp.Awareness = append(sp.Awareness, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("spine awareness: %w", err)
	}

	rows, err = db.QueryContext(ctx,
		`SELECT id, kind, name FROM entities WHERE campaign_id = ?`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("spine entities: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, kind, name string
		if err := rows.Scan(&id, &kind, &name); err != nil {
			return nil, err
		}
		sp.EntityName[id] = name
		sp.EntityKind[id] = kind
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("spine entities: %w", err)
	}

	// The secret facts' statements, for findings a human can act on.
	rows, err = db.QueryContext(ctx,
		`SELECT id, statement FROM facts WHERE campaign_id = ?`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("spine facts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, statement string
		if err := rows.Scan(&id, &statement); err != nil {
			return nil, err
		}
		sp.FactStatement[id] = statement
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("spine facts: %w", err)
	}
	return sp, nil
}

// Act returns the act a scene belongs to, and whether it resolved.
func (sp *Spine) actOf(sceneID string) (Act, bool) {
	for i := range sp.Scenes {
		if sp.Scenes[i].ID != sceneID {
			continue
		}
		for j := range sp.Acts {
			if sp.Acts[j].ID == sp.Scenes[i].ActID {
				return sp.Acts[j], true
			}
		}
	}
	return Act{}, false
}
