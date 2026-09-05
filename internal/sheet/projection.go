package sheet

// The query projection (migration 0029): a narrow, always-derivable row per
// pc entity carrying the numbers query surfaces filter and sort on — level,
// classes label, max hp, ac, and whether a typed sheet backs them at all.
//
// It is maintained, never trusted: every sheet write refreshes the one row
// (SyncEntity, called by the server's write paths) and server start
// re-derives the whole table from the payloads (SyncProjections). That
// makes the table a cache with a boot-time rebuild, the same contract the
// bestiary mirror holds — except its upstream is this database's own
// payloads, so it can never go stale past the next boot.
//
// Backfill lives here and not in the migration's SQL on purpose: turning a
// payload into projection numbers is the typed reader's job (with its
// tolerance rules), and a json_extract one-liner would silently invent
// numbers the typed reader rejects — the exact thing MAD-418 forbids.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Projection is one pc's derived query row.
type Projection struct {
	EntityID   string `json:"entity_id"`
	CampaignID string `json:"campaign_id"`
	Level      int    `json:"level"`
	Classes    string `json:"classes"`
	MaxHP      int    `json:"max_hp"`
	AC         int    `json:"ac"`
	Structured bool   `json:"structured"`
}

// Project derives one pc's projection from its payload. When a typed sheet
// is present it is the definition; when it is not, the legacy top-level
// party keys are read with the same tolerance the party block reads them —
// existing keys are derived fact, absent keys stay zero, nothing is
// invented. The second return reports whether the pc is gone from the party
// (deleted) and its row should not exist at all.
func Project(entityID, campaignID string, payload map[string]any, deleted bool) (Projection, bool) {
	p := Projection{EntityID: entityID, CampaignID: campaignID}
	if deleted {
		return p, true
	}
	s, hasSheet, err := FromPayload(payload)
	if err != nil || (hasSheet && s.isZeroAfterNormalize()) {
		// A sheet key that cannot decode is unstructured: the marker says
		// so, and the legacy keys still project.
		hasSheet = false
	}
	if hasSheet {
		p.Structured = true
		p.Level = s.TotalLevel()
		p.Classes = s.ClassesLabel()
		p.MaxHP = s.MaxHP
		p.AC = s.AC
	}
	if !p.Structured {
		p.Level = tolerantInt(payload["level"])
		p.MaxHP = tolerantInt(payload["max_hp"])
		p.AC = tolerantInt(payload["ac"])
		if class, ok := payload["class"].(string); ok {
			p.Classes = strings.TrimSpace(class)
		}
	}
	return p, false
}

// SyncProjections re-derives the whole projection table from the entities
// it mirrors. Idempotent; safe to run at every boot. Rows for entities that
// disappeared are removed so the table tracks the party the table sees.
func SyncProjections(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx,
		`SELECT id, campaign_id, payload, status FROM entities WHERE kind = 'pc'`)
	if err != nil {
		return fmt.Errorf("sheet projection scan: %w", err)
	}
	type row struct {
		p    Projection
		drop bool
	}
	var keep []row
	for rows.Next() {
		var id, campaignID, payloadJSON, status string
		if err := rows.Scan(&id, &campaignID, &payloadJSON, &status); err != nil {
			rows.Close()
			return fmt.Errorf("sheet projection scan: %w", err)
		}
		var payload map[string]any
		_ = json.Unmarshal([]byte(payloadJSON), &payload)
		p, drop := Project(id, campaignID, payload, status == "deleted")
		keep = append(keep, row{p, drop})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("sheet projection scan: %w", err)
	}
	rows.Close()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sheet projection sync: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM pc_sheet_projection`); err != nil {
		return fmt.Errorf("sheet projection sync: %w", err)
	}
	now := time.Now().UnixMilli()
	for _, r := range keep {
		if r.drop {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO pc_sheet_projection (entity_id, campaign_id, level, classes, max_hp, ac, structured, synced_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			r.p.EntityID, r.p.CampaignID, r.p.Level, r.p.Classes, r.p.MaxHP, r.p.AC, boolToInt(r.p.Structured), now,
		); err != nil {
			return fmt.Errorf("sheet projection sync: %w", err)
		}
	}
	return tx.Commit()
}

// SyncEntity refreshes one entity's projection row after a sheet write (or
// any entity write that may have changed the payload). Cheaper than the
// full sync and immediately consistent for the row just written.
func SyncEntity(ctx context.Context, db *sql.DB, campaignID, entityID string) error {
	var payloadJSON, status string
	err := db.QueryRowContext(ctx,
		`SELECT payload, status FROM entities WHERE id = ? AND kind = 'pc'`, entityID,
	).Scan(&payloadJSON, &status)
	if err == sql.ErrNoRows {
		// Not a pc (any more): the row has no business existing.
		_, err := db.ExecContext(ctx, `DELETE FROM pc_sheet_projection WHERE entity_id = ?`, entityID)
		if err != nil {
			return fmt.Errorf("sheet projection sync: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("sheet projection sync: %w", err)
	}
	var payload map[string]any
	_ = json.Unmarshal([]byte(payloadJSON), &payload)
	p, drop := Project(entityID, campaignID, payload, status == "deleted")
	if drop {
		if _, err := db.ExecContext(ctx, `DELETE FROM pc_sheet_projection WHERE entity_id = ?`, entityID); err != nil {
			return fmt.Errorf("sheet projection sync: %w", err)
		}
		return nil
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO pc_sheet_projection (entity_id, campaign_id, level, classes, max_hp, ac, structured, synced_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(entity_id) DO UPDATE SET
		   campaign_id = excluded.campaign_id,
		   level = excluded.level,
		   classes = excluded.classes,
		   max_hp = excluded.max_hp,
		   ac = excluded.ac,
		   structured = excluded.structured,
		   synced_at = excluded.synced_at`,
		p.EntityID, p.CampaignID, p.Level, p.Classes, p.MaxHP, p.AC, boolToInt(p.Structured), time.Now().UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("sheet projection sync: %w", err)
	}
	return nil
}

// Projections reads one campaign's projection rows in name order of the
// entities they mirror — the party board's query surface. DM material by
// construction (it is what the payloads hold); the authorization happens in
// the caller, exactly like PartySnapshot.
func Projections(ctx context.Context, db *sql.DB, campaignID string) ([]Projection, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT p.entity_id, p.campaign_id, p.level, p.classes, p.max_hp, p.ac, p.structured
		 FROM pc_sheet_projection p
		 JOIN entities e ON e.id = p.entity_id
		 WHERE p.campaign_id = ? AND e.status != 'deleted'
		 ORDER BY e.name COLLATE NOCASE`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("sheet projections: %w", err)
	}
	defer rows.Close()
	var out []Projection
	for rows.Next() {
		var p Projection
		var structured int
		if err := rows.Scan(&p.EntityID, &p.CampaignID, &p.Level, &p.Classes, &p.MaxHP, &p.AC, &structured); err != nil {
			return nil, fmt.Errorf("sheet projections: %w", err)
		}
		p.Structured = structured == 1
		out = append(out, p)
	}
	return out, rows.Err()
}

/* ---------- small helpers ---------- */

// tolerantInt reads a JSON number or its string spelling — payloads come
// back from SQLite as float64s and hand-edited payloads spell numbers as
// strings; both have always been readable.
func tolerantInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case string:
		var i int
		if _, err := fmt.Sscanf(strings.TrimSpace(n), "%d", &i); err == nil {
			return i
		}
	}
	return 0
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
