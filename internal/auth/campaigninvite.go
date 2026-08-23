package auth

// Campaign invites (MAD-305): the same single-use links the keeper mints,
// carrying an optional campaign + role binding. Redeeming one writes the
// campaign_members row alongside the account — no second password path, no
// second session cookie. The binding table (campaign_invites) is owned by
// migration 0004; this package reads and writes it the way internal/campaign
// reads and writes its own migration-owned tables.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Campaign roles an invite may carry. These mirror campaign_members' CHECK
// constraint; the database rejects anything else too, but a clean error beats
// a constraint traceback.
var validCampaignRoles = map[string]bool{"dm": true, "player": true, "observer": true}

// CreateCampaignInvite mints a single-use invite bound to a campaign and role.
// The caller (the campaign's DM or the keeper, checked at the handler) owns
// verifying the campaign exists and the caller may mint for it; this function
// validates the role vocabulary. As with CreateInvite, the raw code is
// returned exactly once and only its digest is stored.
func (s *Store) CreateCampaignInvite(ctx context.Context, createdBy, note, campaignID, role string) (*Invite, error) {
	if !validCampaignRoles[role] {
		return nil, fmt.Errorf("campaign invite role %q is not dm, player or observer", role)
	}
	if strings.TrimSpace(campaignID) == "" {
		return nil, fmt.Errorf("campaign invite needs a campaign")
	}
	inv, err := s.CreateInvite(ctx, createdBy, note)
	if err != nil {
		return nil, err
	}
	// The binding row rides beside the invite; the invite row itself is
	// written first (its own statement, no tx) so the digest unique
	// constraint stays the single gate on minting.
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO campaign_invites (invite_id, campaign_id, role, created_at) VALUES (?, ?, ?, ?)`,
		inv.ID, campaignID, role, time.Now().UTC().UnixMilli()); err != nil {
		// Don't leave a plain invite behind a failed binding — it would
		// redeem as an account-only invite the DM thought was for players.
		_, _ = s.db.ExecContext(ctx, `DELETE FROM invites WHERE id = ? AND used_by IS NULL`, inv.ID)
		return nil, fmt.Errorf("bind campaign invite: %w", err)
	}
	inv.CampaignID = campaignID
	inv.CampaignRole = role
	return inv, nil
}

// ListCampaignInvites returns the invites bound to one campaign, newest
// first. Raw codes are never present; the campaign's DM sees what is pending,
// spent and expired, the same audit the keeper has on /api/invites.
func (s *Store) ListCampaignInvites(ctx context.Context, campaignID string) ([]Invite, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.id, i.created_by, i.created_at, i.expires_at, i.used_by, i.used_at, i.note,
		       b.campaign_id, b.role
		  FROM invites i JOIN campaign_invites b ON b.invite_id = i.id
		 WHERE b.campaign_id = ?
		 ORDER BY i.created_at DESC, i.id DESC`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("list campaign invites: %w", err)
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		inv, err := scanInvite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *inv)
	}
	return out, rows.Err()
}

// RegisterWithCampaignInvite is RegisterWithInvite plus the campaign join:
// when the code carries a campaign binding, join runs inside the register
// transaction with the binding filled in, so the account, the burned invite
// and the campaign_members row commit together or not at all. join may be nil
// (then this is exactly RegisterWithInvite).
func (s *Store) RegisterWithCampaignInvite(ctx context.Context, username, password, code string, join func(ctx context.Context, tx *sql.Tx, u *User, inv Invite) error) (*User, error) {
	return s.registerWithInvite(ctx, username, password, code, join)
}

// JoinWithInvite consumes a campaign invite for an account that already
// exists, returning the campaign id and role the code was bound to. The
// membership row itself is the caller's to write (it owns campaign_members);
// the consume and the insert are therefore two steps, and the narrow failure
// between them — a burned invite with no membership — is repaired by the DM
// adding the member by hand, which is what the invite was going to do anyway.
func (s *Store) JoinWithInvite(ctx context.Context, code, userID string) (campaignID, role string, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", fmt.Errorf("join tx: %w", err)
	}
	defer tx.Rollback()
	_, campaignID, role, err = s.consumeInvite(ctx, tx, code, userID)
	if err != nil {
		return "", "", err
	}
	if campaignID == "" {
		return "", "", fmt.Errorf("%w: that invite is not for a campaign", ErrInviteInvalid)
	}
	if err := tx.Commit(); err != nil {
		return "", "", fmt.Errorf("join commit: %w", err)
	}
	return campaignID, role, nil
}

// Usernames resolves user ids to usernames for the surfaces that show a
// campaign's members. Unknown ids are simply absent from the map.
func (s *Store) Usernames(ctx context.Context, ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	seen := map[string]bool{}
	var args []any
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		args = append(args, id)
	}
	if len(args) == 0 {
		return out, nil
	}
	placeholders := ""
	for range args {
		placeholders += "?,"
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, username FROM users WHERE id IN (`+placeholders[:len(placeholders)-1]+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("lookup usernames: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// LookupUseridByName finds one user's id by exact username, for handlers that
// take a username in a path or body (campaign membership management). Missing
// names return ErrNoUsers.
func (s *Store) LookupUseridByName(ctx context.Context, username string) (string, error) {
	name, err := normalizeUsername(username)
	if err != nil {
		return "", err
	}
	var id string
	err = s.db.QueryRowContext(ctx, `SELECT id FROM users WHERE username = ?`, name).Scan(&id)
	if err == sql.ErrNoRows {
		return "", ErrNoUsers
	}
	return id, err
}
