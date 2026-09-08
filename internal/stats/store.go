package stats

// The reads (MAD-428): the logs Stages 2–5 wrote, queried into the
// fold's inputs. Rolls come from dice_rolls (the roll truth the feed
// serves), hits from the combat journal joined to the combatants they
// landed on, inspiration spends from the ledger's own transactions.
// Every query is ordered, every output is the pure fold — the store
// derives, it never decides.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Store reads the mechanical logs on the shared database handle. It
// holds no state, writes nothing, and subscribes to nothing — a stats
// read is a moment's fold, not a materialized view.
type Store struct {
	db *sql.DB
}

// New builds a stats store over an open, migrated database handle.
func New(db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("stats: nil database handle")
	}
	return &Store{db: db}, nil
}

// Campaign folds one campaign's whole history — the endcap view. dm
// decides whether secret rolls join the fold: the DM's numbers see
// everything, a member's see what the feed would show them.
func (s *Store) Campaign(ctx context.Context, campaignID string, dm bool) (*Stats, error) {
	return s.fold(ctx, campaignID, "", dm)
}

// Session folds one sitting: the rolls it mirrored, the fights its
// battles journaled, the inspiration its rolls spent.
func (s *Store) Session(ctx context.Context, sessionID string, dm bool) (*Stats, error) {
	var campaignID string
	err := s.db.QueryRowContext(ctx,
		`SELECT campaign_id FROM game_sessions WHERE id = ?`, sessionID).Scan(&campaignID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("stats: session %s not found", sessionID)
	}
	if err != nil {
		return nil, fmt.Errorf("stats: load session: %w", err)
	}
	return s.fold(ctx, campaignID, sessionID, dm)
}

// SessionMarkdown renders one session's numbers section for the
// export — the DM's own take-home record, so secret rolls join the
// fold. A quiet session renders "".
func (s *Store) SessionMarkdown(ctx context.Context, sessionID string) (string, error) {
	st, err := s.Session(ctx, sessionID, true)
	if err != nil {
		return "", err
	}
	return st.Markdown(), nil
}

// fold gathers the inputs and runs the pure derivation.
func (s *Store) fold(ctx context.Context, campaignID, sessionID string, dm bool) (*Stats, error) {
	rolls, err := s.rollInputs(ctx, campaignID, sessionID, dm)
	if err != nil {
		return nil, err
	}
	damage, err := s.damageInputs(ctx, campaignID, sessionID)
	if err != nil {
		return nil, err
	}
	inspired, err := s.inspirationSpends(ctx, campaignID, sessionID)
	if err != nil {
		return nil, err
	}
	return Fold(Inputs{Rolls: rolls, Damage: damage, Inspired: inspired}), nil
}

// rollInputs loads the rolls in play order. Non-DM folds filter
// visibility in the query — the leak test's own rule, applied one more
// time: a secret roll is absent from a player's numbers, not merely
// unprinted.
func (s *Store) rollInputs(ctx context.Context, campaignID, sessionID string, dm bool) ([]RollInput, error) {
	q := `
		SELECT COALESCE(character_id, ''), character_name,
		       COALESCE(target_id, ''), target_name,
		       context_kind, COALESCE(mode, ''), dice
		  FROM dice_rolls
		 WHERE campaign_id = ?`
	args := []any{campaignID}
	if sessionID != "" {
		q += ` AND session_id = ?`
		args = append(args, sessionID)
	}
	if !dm {
		q += ` AND visibility = 'public'`
	}
	q += ` ORDER BY seq`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("stats rolls: %w", err)
	}
	defer rows.Close()
	var out []RollInput
	for rows.Next() {
		var (
			r        RollInput
			diceJSON string
		)
		if err := rows.Scan(&r.CharacterID, &r.CharacterName, &r.TargetID, &r.TargetName,
			&r.Context, &r.Mode, &diceJSON); err != nil {
			return nil, err
		}
		r.D20s = keptD20s(diceJSON)
		out = append(out, r)
	}
	return out, rows.Err()
}

// diceShape is the roll's stored JSON as the fold needs it: the terms,
// and within each term the dice with their kept flags.
type diceShape struct {
	Terms []struct {
		Sides int `json:"sides"`
		Dice  []struct {
			Value int  `json:"value"`
			Kept  bool `json:"kept"`
		} `json:"dice"`
	} `json:"terms"`
}

// keptD20s reads the kept d20 values out of one roll's stored dice
// bytes — the natural dice that decided it. Malformed bytes (a future
// shape, a hand-edited row) fold to nothing: the stats never guess.
func keptD20s(diceJSON string) []int {
	if diceJSON == "" || diceJSON == "[]" || diceJSON == "null" {
		return nil
	}
	var shape diceShape
	if err := json.Unmarshal([]byte(diceJSON), &shape); err != nil {
		return nil
	}
	var out []int
	for _, t := range shape.Terms {
		if t.Sides != 20 {
			continue
		}
		for _, d := range t.Dice {
			if d.Kept {
				out = append(out, d.Value)
			}
		}
	}
	return out
}

// damageInputs loads the applied hits: journal rows of kind damage,
// joined to the combatant they landed on, with the source attribution
// the tracker journaled (MAD-428) read out of the payload flat. The
// amount is the row's own — the effective damage after resistance and
// immunity, the number the tracker's columns also fold.
func (s *Store) damageInputs(ctx context.Context, campaignID, sessionID string) ([]DamageInput, error) {
	q := `
		SELECT COALESCE(c.entity_id, ''), c.name, c.side, c.kind,
		       COALESCE(json_extract(l.payload, '$.source_entity'), ''),
		       COALESCE(json_extract(l.payload, '$.source'), ''),
		       l.amount
		  FROM combat_log l
		  JOIN combatants c ON c.id = l.combatant_id
		  JOIN combats cb ON cb.id = l.combat_id
		 WHERE cb.campaign_id = ? AND l.kind = 'damage'`
	args := []any{campaignID}
	if sessionID != "" {
		q += ` AND cb.session_id = ?`
		args = append(args, sessionID)
	}
	q += ` ORDER BY l.rowid`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("stats damage: %w", err)
	}
	defer rows.Close()
	var out []DamageInput
	for rows.Next() {
		var d DamageInput
		if err := rows.Scan(&d.TargetEntity, &d.TargetName, &d.TargetSide, &d.TargetKind,
			&d.SourceEntity, &d.SourceName, &d.Amount); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// inspirationSpends counts the ledger's inspiration spend rows — the
// roll flow writes them, the stats quote them.
func (s *Store) inspirationSpends(ctx context.Context, campaignID, sessionID string) (int, error) {
	q := `SELECT COUNT(*) FROM resource_transactions
		   WHERE campaign_id = ? AND pool = 'feature:inspiration' AND kind = 'spend'`
	args := []any{campaignID}
	if sessionID != "" {
		q += ` AND session_id = ?`
		args = append(args, sessionID)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("stats inspiration: %w", err)
	}
	return n, nil
}
