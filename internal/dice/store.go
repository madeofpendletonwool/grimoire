package dice

// The dice store (MAD-420): rolls as rows. The write path mints each
// campaign's seed once, assigns the per-campaign seq that doubles as the
// RNG nonce inside the INSERT (the same atomic pattern session_events
// use), executes the formula against (seed, nonce), and — when a session
// is live — mirrors the roll into the session log as a kind 'roll' event,
// so the feed and the export read one truth. Reads are visibility-scoped
// in the query itself: a player's WHERE clause cannot return a secret
// roll, which is what the leak tests assert.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
	"github.com/madeofpendletonwool/grimoire/internal/pubsub"
)

// Store reads and writes dice rolls on the shared database handle.
type Store struct {
	db        *sql.DB
	campaigns *campaign.Store
	sessions  *gamesession.Store
	broker    *pubsub.Broker
}

// New builds a dice store. campaigns resolves character and target names
// at roll time; sessions writes the session-event mirror and finds the
// campaign's live sitting.
func New(db *sql.DB, campaigns *campaign.Store, sessions *gamesession.Store) (*Store, error) {
	if db == nil {
		return nil, errors.New("dice: nil database handle")
	}
	return &Store{db: db, campaigns: campaigns, sessions: sessions, broker: pubsub.New()}, nil
}

// WithBroker moves the store onto a shared campaign broker (MAD-423):
// rolls notify the campaign topic every board stream also listens to.
// The stream keeps its scope-filtered reads; a ping carries no data.
func (s *Store) WithBroker(b *pubsub.Broker) *Store {
	if b != nil {
		s.broker = b
	}
	return s
}

// Subscribe wakes when anything notifies this campaign — a roll, or any
// other store sharing the broker. The returned cancel must be called
// when the reader goes away.
func (s *Store) Subscribe(campaignID string) (<-chan struct{}, func()) {
	return s.broker.Subscribe(campaignID)
}

/* ---------- the write path ---------- */

// Input is one roll request, already permission-checked by the handler:
// the engine validates the formula, the mode and the vocabulary, and the
// handler has settled visibility and character (players roll public as
// their bound character; the DM may roll secret as anyone).
type Input struct {
	Formula     string
	Mode        string
	Visibility  string
	ContextKind string
	Detail      string
	TargetID    string
	CharacterID string
	SessionID   string
	Actor       string
}

// RollRow is one stored roll. Result is the decoded engine output; the
// stored bytes are the same object, so a feed render and a replay derive
// from one truth. Seed and Nonce are the replay inputs — re-rolling the
// formula against them reproduces the stored dice exactly.
type RollRow struct {
	ID             string
	CampaignID     string
	Seq            int64
	SessionID      string
	SessionEventID string
	Actor          string
	ActorName      string
	CharacterID    string
	CharacterName  string
	ContextKind    string
	Detail         string
	TargetID       string
	TargetName     string
	Formula        string
	Mode           string
	Result         *RollResult
	Seed           int64
	Nonce          int64
	Visibility     string
	CreatedAt      time.Time
}

// Roll executes and stores one roll. The formula is parsed and
// mode-applied first — malformed input is campaign.ErrInvalid and never
// reaches the database. The per-campaign seq is assigned inside the
// INSERT (atomic under concurrent rolls), doubles as the RNG nonce, and
// the dice are computed from it and the campaign's seed before the row
// is completed in the same transaction. A live session (explicit, or the
// campaign's status=live sitting) gets the kind 'roll' event mirror; a
// campaign with no live sitting still gets its roll — the roller is
// always usable, the session log simply was not open.
func (s *Store) Roll(ctx context.Context, campaignID string, in Input) (*RollRow, error) {
	in.Formula = strings.TrimSpace(in.Formula)
	expr, err := Parse(in.Formula)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", campaign.ErrInvalid, err)
	}
	if in.Mode != ModeNone && in.Mode != ModeAdvantage && in.Mode != ModeDisadvantage {
		return nil, fmt.Errorf("%w: mode %q", campaign.ErrInvalid, in.Mode)
	}
	effective, err := WithMode(expr, in.Mode)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", campaign.ErrInvalid, err)
	}
	switch in.Visibility {
	case "", VisibilityPublic:
		in.Visibility = VisibilityPublic
	case VisibilitySecret:
	default:
		return nil, fmt.Errorf("%w: visibility %q", campaign.ErrInvalid, in.Visibility)
	}
	if in.ContextKind == "" {
		in.ContextKind = ContextOther
	}
	if !ValidContext(in.ContextKind) {
		return nil, fmt.Errorf("%w: context %q", campaign.ErrInvalid, in.ContextKind)
	}
	if len(in.Detail) > 200 {
		return nil, fmt.Errorf("%w: detail longer than 200 characters", campaign.ErrInvalid)
	}

	charName := ""
	if in.CharacterID != "" {
		e, err := s.campaigns.GetEntity(ctx, campaign.ScopeDM, campaignID, in.CharacterID)
		if err != nil {
			return nil, fmt.Errorf("%w: character %s", campaign.ErrNotFound, in.CharacterID)
		}
		charName = e.Name
	}
	targetName := ""
	if in.TargetID != "" {
		e, err := s.campaigns.GetEntity(ctx, campaign.ScopeDM, campaignID, in.TargetID)
		if err != nil {
			return nil, fmt.Errorf("%w: target %s", campaign.ErrNotFound, in.TargetID)
		}
		targetName = e.Name
	}

	// The session the roll belongs to: the explicit one, else the
	// campaign's live sitting, else none — the feed still shows it.
	sessionID := in.SessionID
	if sessionID == "" {
		sessionID = s.liveSession(ctx, campaignID)
	} else if owner := s.sessionCampaign(ctx, sessionID); owner != campaignID {
		return nil, fmt.Errorf("%w: session %s is not this campaign's", campaign.ErrInvalid, sessionID)
	}

	seed, err := s.campaignSeed(ctx, campaignID)
	if err != nil {
		return nil, err
	}

	id := uuid.NewString()
	now := time.Now().UTC()
	actorName := ""
	if in.Actor != "" {
		_ = s.db.QueryRowContext(ctx,
			`SELECT username FROM users WHERE id = ?`, in.Actor).Scan(&actorName)
	}
	row := &RollRow{
		ID: id, CampaignID: campaignID, SessionID: sessionID, Actor: in.Actor, ActorName: actorName,
		CharacterID: in.CharacterID, CharacterName: charName,
		ContextKind: in.ContextKind, Detail: strings.TrimSpace(in.Detail),
		TargetID: in.TargetID, TargetName: targetName,
		Visibility: in.Visibility, CreatedAt: now,
	}

	// One transaction: claim the seq atomically, roll from (seed, seq),
	// complete the row. The dice land after the seq is known because the
	// seq IS the nonce — the same trick that keeps replay reproducible
	// keeps concurrent rolls from sharing a position. The engine columns
	// carry defaults so the two-step row is never half-null outside the
	// transaction.
	// Optional references travel as real NULLs — an empty string is a
	// value, and SQLite's foreign keys agree.
	nullIfEmpty := func(v string) any {
		if v == "" {
			return nil
		}
		return v
	}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		var seq int64
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO dice_rolls (id, campaign_id, seq, actor, character_id, character_name,
				context_kind, detail, target_id, target_name, formula, mode, visibility, created_at)
			VALUES (?, ?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM dice_rolls WHERE campaign_id = ?),
				?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			RETURNING seq`,
			id, campaignID, campaignID, in.Actor, nullIfEmpty(in.CharacterID), charName,
			in.ContextKind, row.Detail, nullIfEmpty(in.TargetID), targetName,
			expr.String(), in.Mode, in.Visibility, now.UnixMilli()).Scan(&seq); err != nil {
			return fmt.Errorf("insert roll: %w", err)
		}
		result := Roll(seed, seq, effective, in.Mode)
		diceJSON, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE dice_rolls SET dice = ?, modifier = ?, total = ?, seed = ?, nonce = ?
			WHERE id = ?`, string(diceJSON), result.Modifier, result.Total, seed, seq, id); err != nil {
			return fmt.Errorf("complete roll: %w", err)
		}
		row.Seq = seq
		row.Formula = expr.String()
		row.Mode = in.Mode
		row.Result = result
		return nil
	})
	if err != nil {
		return nil, err
	}

	// The session-event mirror: who rolled, what, and the dice — the
	// provenance the export prints and later stages replay. Best-effort
	// after the roll commits: a failed mirror never eats the roll, and
	// the feed remains the primary surface.
	if sessionID != "" && s.sessions != nil {
		if ev, err := s.sessions.AddEvent(ctx, sessionID, gamesession.EventRoll,
			row.summaryLine(), row.Detail, row.eventPayload()); err == nil {
			row.SessionEventID = ev.ID
			_, _ = s.db.ExecContext(ctx, `UPDATE dice_rolls SET session_event_id = ? WHERE id = ?`, ev.ID, id)
		}
	}
	s.broker.Notify(campaignID)
	return row, nil
}

// summaryLine is the one-line label the session log and the feed share:
// "Mira Thorn — 2d6+3 → 11 · Fireball damage against the goblins".
func (r *RollRow) summaryLine() string {
	who := r.CharacterName
	if who == "" {
		who = "the table"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %s", who, r.Notation())
	if r.Mode != "" {
		fmt.Fprintf(&b, " (%s)", r.Mode)
	}
	fmt.Fprintf(&b, " → %d", r.Result.Total)
	return b.String()
}

// notation renders the roll's display formula with its natural dice:
// "2d6 [4, 5] + 3"; a die the keep clause dropped wears a tilde.
func (r *RollRow) Notation() string {
	if r.Result == nil {
		return r.Formula
	}
	var b strings.Builder
	for i, t := range r.Result.Terms {
		if i == 0 {
			if t.Sign < 0 {
				b.WriteByte('-')
			}
		} else if t.Sign < 0 {
			b.WriteString(" - ")
		} else {
			b.WriteString(" + ")
		}
		b.WriteString(t.Formula)
		if t.Kind == "dice" && len(t.Dice) > 0 {
			b.WriteString(" [")
			for j, d := range t.Dice {
				if j > 0 {
					b.WriteString(", ")
				}
				if !d.Kept {
					b.WriteString("~")
				}
				fmt.Fprintf(&b, "%d", d.Value)
			}
			b.WriteByte(']')
		}
	}
	return b.String()
}

// eventPayload is the session event's payload: the complete roll minus
// the row bookkeeping — everything a replay or a citation needs.
func (r *RollRow) eventPayload() map[string]any {
	return map[string]any{
		"roll_id":    r.ID,
		"formula":    r.Formula,
		"mode":       r.Mode,
		"notation":   r.Notation(),
		"dice":       r.Result.Terms,
		"modifier":   r.Result.Modifier,
		"total":      r.Result.Total,
		"visibility": r.Visibility,
		"context":    r.ContextKind,
		"character":  r.CharacterName,
		"target":     r.TargetName,
	}
}

// liveSession finds the campaign's live sitting, newest ordinal first —
// the sitting the DM started for tonight.
func (s *Store) liveSession(ctx context.Context, campaignID string) string {
	var id string
	err := s.db.QueryRowContext(ctx, `
		SELECT id FROM game_sessions WHERE campaign_id = ? AND status = ?
		ORDER BY ordinal DESC LIMIT 1`, campaignID, gamesession.StatusLive).Scan(&id)
	if err != nil {
		return ""
	}
	return id
}

// sessionCampaign resolves which campaign a session belongs to, "" when
// unknown.
func (s *Store) sessionCampaign(ctx context.Context, sessionID string) string {
	var id string
	if err := s.db.QueryRowContext(ctx,
		`SELECT campaign_id FROM game_sessions WHERE id = ?`, sessionID).Scan(&id); err != nil {
		return ""
	}
	return id
}

// campaignSeed reads the campaign's RNG seed, minting it once on first
// roll. INSERT OR IGNORE + re-read keeps concurrent first-rolls on one
// seed instead of racing to own it.
func (s *Store) campaignSeed(ctx context.Context, campaignID string) (int64, error) {
	var seed int64
	err := s.db.QueryRowContext(ctx,
		`SELECT seed FROM dice_seeds WHERE campaign_id = ?`, campaignID).Scan(&seed)
	if err == nil {
		return seed, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("read dice seed: %w", err)
	}
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return 0, fmt.Errorf("mint dice seed: %w", err)
	}
	// Mask to 63 bits: the value travels as an INTEGER and must stay a
	// positive int64 on every path that copies it.
	fresh := int64(uint64(buf[0])|uint64(buf[1])<<8|uint64(buf[2])<<16|uint64(buf[3])<<24|
		uint64(buf[4])<<32|uint64(buf[5])<<40|uint64(buf[6])<<48|uint64(buf[7])<<56) & int64(^uint64(0)>>1)
	if fresh == 0 {
		fresh = 1
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO dice_seeds (campaign_id, seed, created_at) VALUES (?, ?, ?)`,
		campaignID, fresh, time.Now().UTC().UnixMilli()); err != nil {
		return 0, fmt.Errorf("store dice seed: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT seed FROM dice_seeds WHERE campaign_id = ?`, campaignID).Scan(&seed); err != nil {
		return 0, fmt.Errorf("read dice seed: %w", err)
	}
	return seed, nil
}

/* ---------- the reads ---------- */

// Feed lists a campaign's rolls in play order. after 0 returns the
// newest window (the feed's first paint); after > 0 returns everything
// past that cursor (the stream's follow-up reads). Non-DM reads filter
// visibility in the query itself — a secret roll is absent from the
// player's rows, not hidden by the caller after the fact.
func (s *Store) Feed(ctx context.Context, campaignID string, after int64, limit int, dm bool) ([]RollRow, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	visClause := ""
	if !dm {
		visClause = " AND r.visibility = 'public'"
	}
	query := func(orderDesc bool) string {
		dir := "ASC"
		if orderDesc {
			dir = "DESC"
		}
		return fmt.Sprintf(`
			SELECT r.id, r.campaign_id, r.seq, COALESCE(r.session_id, ''), COALESCE(r.session_event_id, ''),
				r.actor, COALESCE(u.username, ''), COALESCE(r.character_id, ''), r.character_name,
				r.context_kind, r.detail, COALESCE(r.target_id, ''), r.target_name,
				r.formula, COALESCE(r.mode, ''), r.dice, r.modifier, r.total, r.seed, r.nonce,
				r.visibility, r.created_at
			FROM dice_rolls r
			LEFT JOIN users u ON u.id = r.actor
			WHERE r.campaign_id = ? AND r.seq > ?%s
			ORDER BY r.seq %s
			LIMIT ?`, visClause, dir)
	}

	var rows []RollRow
	var err error
	if after > 0 {
		rows, err = s.queryRolls(ctx, query(false), campaignID, after, limit)
	} else {
		rows, err = s.queryRolls(ctx, query(true), campaignID, 0, limit)
		if err == nil {
			for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// LatestSeq is the campaign's highest roll seq at this scope — the cursor
// a stream client resumes from.
func (s *Store) LatestSeq(ctx context.Context, campaignID string, dm bool) (int64, error) {
	q := `SELECT COALESCE(MAX(seq), 0) FROM dice_rolls WHERE campaign_id = ?`
	if !dm {
		q += ` AND visibility = 'public'`
	}
	var seq int64
	if err := s.db.QueryRowContext(ctx, q, campaignID).Scan(&seq); err != nil {
		return 0, err
	}
	return seq, nil
}

func (s *Store) queryRolls(ctx context.Context, query, campaignID string, after int64, limit int) ([]RollRow, error) {
	rows, err := s.db.QueryContext(ctx, query, campaignID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list rolls: %w", err)
	}
	defer rows.Close()
	var out []RollRow
	for rows.Next() {
		r, err := scanRoll(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func scanRoll(row interface{ Scan(...any) error }) (*RollRow, error) {
	var (
		r         RollRow
		diceJSON  string
		modifier  int
		total     int
		createdMS int64
	)
	if err := row.Scan(&r.ID, &r.CampaignID, &r.Seq, &r.SessionID, &r.SessionEventID,
		&r.Actor, &r.ActorName, &r.CharacterID, &r.CharacterName,
		&r.ContextKind, &r.Detail, &r.TargetID, &r.TargetName,
		&r.Formula, &r.Mode, &diceJSON, &modifier, &total, &r.Seed, &r.Nonce,
		&r.Visibility, &createdMS); err != nil {
		return nil, err
	}
	r.CreatedAt = time.UnixMilli(createdMS).UTC()
	r.Result = &RollResult{
		Formula: r.Formula, Mode: r.Mode, Modifier: modifier, Total: total,
	}
	if diceJSON != "" && diceJSON != "[]" && diceJSON != "null" {
		_ = json.Unmarshal([]byte(diceJSON), r.Result)
	}
	return &r, nil
}

/* ---------- helpers ---------- */

// withTx runs fn in one transaction, rolling back on error.
func (s *Store) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
