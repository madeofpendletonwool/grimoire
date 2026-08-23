// Package auth stores accounts, server-side sessions, and invite links for
// Grimoire.
//
// The tables live in the same SQLite file as the search index and the chat
// history. That is safe: index.Store.Reset only clears the docs tables, so
// rebuilding the rules index never signs anyone out.
//
// Grimoire is a personal, self-hosted app, so account creation is not open by
// default: whoever reaches an empty install claims it by creating the first
// account — which becomes the admin — and the door is shut behind them. The
// admin then invites friends by minting single-use invite links; signing up
// requires one. (An operator can still flip GRIMOIRE_OPEN_REGISTRATION to leave
// self-service creation open, the original escape hatch.)
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Errors callers are expected to branch on. Authenticate returns
// ErrInvalidCredentials for both an unknown name and a wrong password, so the
// API cannot be used to enumerate accounts.
var (
	ErrNoUsers            = errors.New("no accounts exist")
	ErrInvalidCredentials = errors.New("wrong name or passphrase")
	ErrUsernameTaken      = errors.New("that name is already spoken for")
	ErrNoSession          = errors.New("no valid session")
	ErrInviteInvalid      = errors.New("invite link is not valid")
	ErrInviteUsed         = errors.New("invite link has already been used")
	ErrInviteExpired      = errors.New("invite link has expired")
)

// DefaultSessionTTL is how long a session lives when the operator does not say
// otherwise. Long, because this is a personal app on a personal device and
// being logged out weekly is worse than the marginal risk.
const DefaultSessionTTL = 30 * 24 * time.Hour

// DefaultInviteTTL is how long a freshly minted invite link stays usable when
// the operator does not say otherwise. Long enough to hand a friend a link in
// person, short enough that one left lying around does not stay open forever.
// An inviteTTL of zero means invites never expire.
const DefaultInviteTTL = 7 * 24 * time.Hour

// User is an account. The password hash never leaves the store. IsAdmin is
// true only for the first account ever created — the keeper who can mint
// invites — so an admin is made once, at install time, not by promotion.
type User struct {
	ID        string
	Username  string
	IsAdmin   bool
	CreatedAt time.Time
}

// Session is a server-side session. Token is the secret handed to the browser
// and is only ever populated by StartSession — the database holds a digest of
// it, so a leaked database copy cannot be replayed as a live session.
type Session struct {
	Token     string
	UserID    string
	ExpiresAt time.Time
}

// Store persists users, sessions, and invites.
type Store struct {
	db        *sql.DB
	ttl       time.Duration
	inviteTTL time.Duration
}

// New builds an auth store on an existing database handle and ensures its
// schema exists. A ttl of zero picks DefaultSessionTTL. An inviteTTL of zero
// means freshly minted invites never expire; the caller (main) applies the
// default for an unset env var.
func New(db *sql.DB, ttl, inviteTTL time.Duration) (*Store, error) {
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	s := &Store{db: db, ttl: ttl, inviteTTL: inviteTTL}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id            TEXT PRIMARY KEY,
	username      TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL,
	is_admin      INTEGER NOT NULL DEFAULT 0,
	created_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
	token_hash TEXT PRIMARY KEY,
	user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_expiry ON sessions(expires_at);
CREATE TABLE IF NOT EXISTS invites (
	id         TEXT PRIMARY KEY,
	code_hash  TEXT NOT NULL UNIQUE,
	created_by TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL,
	expires_at INTEGER,
	used_by    TEXT REFERENCES users(id) ON DELETE SET NULL,
	used_at    INTEGER,
	note       TEXT
);
CREATE INDEX IF NOT EXISTS invites_created_by ON invites(created_by);
-- The campaign binding table (MAD-305). Migration 0004 owns it and declares
-- the full foreign keys; this declaration exists so a caller that constructs
-- the auth store without the migration runner still gets working plain
-- invites (the pre-migration compatibility pattern the baseline set with
-- users.is_admin). It deliberately carries no REFERENCES campaigns(id): on an
-- auth-only database that parent does not exist, and SQLite would refuse even
-- invite deletions while trying to evaluate the cascade. In production the
-- migration runner has always created the fully-constrained table first, so
-- this statement is a no-op there.
CREATE TABLE IF NOT EXISTS campaign_invites (
	invite_id   TEXT PRIMARY KEY REFERENCES invites(id) ON DELETE CASCADE,
	campaign_id TEXT NOT NULL,
	role        TEXT NOT NULL CHECK (role IN ('dm','player','observer')),
	created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS campaign_invites_campaign ON campaign_invites(campaign_id);
`

// migrate installs the schema and brings an older database up to date. The
// only additive change so far is the accounts is_admin column: an install
// upgraded from before admins existed gets it added, and its oldest account —
// the keeper who was first through the door — is marked admin to match "first
// created user is the admin."
func (s *Store) migrate() error {
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("auth migrate schema: %w", err)
	}
	if err := addColumnIfMissing(s.db, "users", "is_admin", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return fmt.Errorf("auth migrate is_admin: %w", err)
	}
	// First created user is the admin. Idempotent: this only ever sets the
	// oldest account to admin, never clears one, so re-running on upgrade is
	// safe and correct.
	if _, err := s.db.Exec(`UPDATE users SET is_admin = 1
		WHERE id = (SELECT id FROM users ORDER BY created_at ASC, id ASC LIMIT 1)`); err != nil {
		return fmt.Errorf("auth migrate admin backfill: %w", err)
	}
	return nil
}

// addColumnIfMissing runs ALTER TABLE ADD COLUMN when the column is absent.
// SQLite has no IF NOT EXISTS for ADD COLUMN, so the pragma is checked first;
// the query is otherwise free to fail harmlessly if two boots race the check.
func addColumnIfMissing(db *sql.DB, table, column, decl string) error {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return err
	}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			rows.Close()
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, decl))
	return err
}

// Count reports how many accounts exist. Zero means the install is unclaimed
// and the UI should offer to create the first account.
func (s *Store) Count(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return n, nil
}

// First returns the oldest account, which owns anything created before
// authentication existed. It returns ErrNoUsers on an unclaimed install.
func (s *Store) First(ctx context.Context) (*User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, username, is_admin, created_at FROM users ORDER BY created_at ASC, id ASC LIMIT 1`)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoUsers
	}
	return u, err
}

// CreateUser hashes the passphrase and records a new account. The first account
// ever created is the admin — the keeper who can mint invite links — so the
// invariant lives here rather than in the caller. Counting before the insert is
// safe: SQLite serializes writers, and this is a single-household install.
func (s *Store) CreateUser(ctx context.Context, username, password string) (*User, error) {
	return s.createUser(ctx, s.db, username, password)
}

// createUser is the runner-parametric core of CreateUser, so the same logic
// runs inside a RegisterWithInvite transaction. runner is satisfied by both
// *sql.DB and *sql.Tx.
func (s *Store) createUser(ctx context.Context, q runner, username, password string) (*User, error) {
	name, err := normalizeUsername(username)
	if err != nil {
		return nil, err
	}
	if err := checkPassword(password); err != nil {
		return nil, err
	}
	hash, err := hashPassword(password, defaultParams)
	if err != nil {
		return nil, err
	}

	var existing int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&existing); err != nil {
		return nil, fmt.Errorf("count users: %w", err)
	}

	u := &User{ID: uuid.NewString(), Username: name, IsAdmin: existing == 0, CreatedAt: time.Now().UTC()}
	_, err = q.ExecContext(ctx,
		`INSERT INTO users (id, username, password_hash, is_admin, created_at) VALUES (?, ?, ?, ?, ?)`,
		u.ID, u.Username, hash, u.IsAdmin, u.CreatedAt.UnixMilli())
	if err != nil {
		// The UNIQUE index is the only constraint on the table, so a
		// constraint failure can only mean the name is taken.
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, ErrUsernameTaken
		}
		return nil, fmt.Errorf("create user: %w", err)
	}
	return u, nil
}

// IsAdmin reports whether the user is the install's admin. It is the source of
// truth for gating admin-only endpoints, rather than a value carried on a
// looked-up session, so a stale context can never authorize an admin action.
func (s *Store) IsAdmin(ctx context.Context, userID string) (bool, error) {
	var isAdmin int
	err := s.db.QueryRowContext(ctx, `SELECT is_admin FROM users WHERE id = ?`, userID).Scan(&isAdmin)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("is admin: %w", err)
	}
	return isAdmin == 1, nil
}

// Authenticate checks a passphrase against an account. An unknown name still
// pays for a full argon2 verification against a dummy hash, so a caller cannot
// tell "no such account" from "wrong passphrase" by timing the response.
func (s *Store) Authenticate(ctx context.Context, username, password string) (*User, error) {
	name := strings.ToLower(strings.TrimSpace(username))
	row := s.db.QueryRowContext(ctx,
		`SELECT id, username, is_admin, created_at, password_hash FROM users WHERE username = ?`, name)

	var (
		u       User
		created int64
		hash    string
	)
	switch err := row.Scan(&u.ID, &u.Username, &u.IsAdmin, &created, &hash); {
	case errors.Is(err, sql.ErrNoRows):
		_, _ = verifyPassword(dummyHash(), password)
		return nil, ErrInvalidCredentials
	case err != nil:
		return nil, fmt.Errorf("authenticate: %w", err)
	}

	ok, err := verifyPassword(hash, password)
	if err != nil || !ok {
		return nil, ErrInvalidCredentials
	}
	u.CreatedAt = time.UnixMilli(created).UTC()
	return &u, nil
}

// StartSession mints a session for a user and returns the token to hand to the
// browser. Expired rows are swept here rather than on every request: logins are
// rare, so the cleanup rides along with an operation that is already writing.
func (s *Store) StartSession(ctx context.Context, userID string) (*Session, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	now := time.Now().UTC()
	sess := &Session{Token: token, UserID: userID, ExpiresAt: now.Add(s.ttl)}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		tokenHash(token), userID, now.UnixMilli(), sess.ExpiresAt.UnixMilli()); err != nil {
		return nil, fmt.Errorf("start session: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE expires_at <= ?`, now.UnixMilli()); err != nil {
		return nil, fmt.Errorf("purge sessions: %w", err)
	}
	return sess, nil
}

// Lookup resolves a session token to its user, returning ErrNoSession for a
// token that is unknown, expired, or empty.
func (s *Store) Lookup(ctx context.Context, token string) (*User, error) {
	if token == "" {
		return nil, ErrNoSession
	}
	digest := tokenHash(token)
	row := s.db.QueryRowContext(ctx,
		`SELECT u.id, u.username, u.is_admin, u.created_at, s.expires_at
		   FROM sessions s JOIN users u ON u.id = s.user_id
		  WHERE s.token_hash = ?`, digest)

	var (
		u                User
		created, expires int64
	)
	switch err := row.Scan(&u.ID, &u.Username, &u.IsAdmin, &created, &expires); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNoSession
	case err != nil:
		return nil, fmt.Errorf("lookup session: %w", err)
	}
	if time.Now().UTC().UnixMilli() >= expires {
		// Drop the dead row on the way past so a lapsed session does not
		// linger until the next login sweeps it.
		_, _ = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, digest)
		return nil, ErrNoSession
	}
	u.CreatedAt = time.UnixMilli(created).UTC()
	return &u, nil
}

// EndSession revokes a single session. Revoking an unknown token is not an
// error: the caller wanted to be signed out, and they are.
func (s *Store) EndSession(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash(token)); err != nil {
		return fmt.Errorf("end session: %w", err)
	}
	return nil
}

// TTL is the lifetime new sessions get, so the cookie's Max-Age can match the
// server-side expiry instead of guessing at it.
func (s *Store) TTL() time.Duration { return s.ttl }

// InviteTTL is the lifetime new invites get; zero means invites never expire.
func (s *Store) InviteTTL() time.Duration { return s.inviteTTL }

// tokenHash is what the sessions table stores. SHA-256 is the right primitive
// here rather than argon2: the token is 256 bits of entropy already, so there
// is nothing for an attacker to brute-force and no reason to pay for a slow KDF
// on every request.
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func scanUser(r interface{ Scan(...any) error }) (*User, error) {
	var (
		u       User
		isAdmin int
		created int64
	)
	if err := r.Scan(&u.ID, &u.Username, &isAdmin, &created); err != nil {
		return nil, err
	}
	u.IsAdmin = isAdmin == 1
	u.CreatedAt = time.UnixMilli(created).UTC()
	return &u, nil
}

// Username and passphrase bounds. Deliberately loose — this is one household's
// install, not a public sign-up form — but bounded so nothing unreasonable
// reaches the hasher.
const (
	minUsername = 2
	maxUsername = 40
	minPassword = 8
	maxPassword = 256
)

// normalizeUsername folds case so "Keeper" and "keeper" cannot become two
// accounts. The stored form is the normalized one; there is no separate display
// name to keep in sync.
func normalizeUsername(username string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(username))
	if len([]rune(name)) < minUsername || len([]rune(name)) > maxUsername {
		return "", fmt.Errorf("name must be between %d and %d characters", minUsername, maxUsername)
	}
	for _, r := range name {
		if r <= ' ' || r == 0x7f {
			return "", errors.New("name must not contain spaces or control characters")
		}
	}
	return name, nil
}

func checkPassword(password string) error {
	if len(password) < minPassword {
		return fmt.Errorf("passphrase must be at least %d characters", minPassword)
	}
	if len(password) > maxPassword {
		return fmt.Errorf("passphrase must be at most %d characters", maxPassword)
	}
	return nil
}

/* ---------- invites ---------- */

// Invite is a single-use registration link. Code is the bearer secret and is
// populated only by CreateInvite — it is what the admin copies and hands out,
// and what the invites table stores is only a digest of it, so a leaked
// database copy cannot be replayed to mint an account.
//
// CampaignID and CampaignRole carry the campaign binding (MAD-305): an invite
// minted by a campaign's DM, which writes a campaign_members row for the
// redeeming account. Both are empty on the plain account invites the keeper
// mints.
type Invite struct {
	ID           string
	Code         string // the raw secret; only set on CreateInvite
	CreatedBy    string
	CreatedAt    time.Time
	ExpiresAt    time.Time // zero value means no expiry
	UsedBy       string    // empty until consumed
	UsedAt       time.Time // zero value until consumed
	Note         string
	CampaignID   string // set when the invite carries a campaign binding
	CampaignRole string // dm | player | observer; empty with no binding
}

// Bound reports whether the invite carries a campaign binding.
func (i Invite) Bound() bool { return i.CampaignID != "" }

// Used reports whether the invite has already minted an account. A used invite
// is kept (rather than deleted) so the admin can see who redeemed it and when.
func (i Invite) Used() bool { return i.UsedBy != "" }

// Expired reports whether the invite's TTL has elapsed. No expiry means never.
func (i Invite) Expired() bool { return !i.ExpiresAt.IsZero() && time.Now().UTC().After(i.ExpiresAt) }

// Pending reports whether the invite can still mint an account.
func (i Invite) Pending() bool { return !i.Used() && !i.Expired() }

// maxNote caps an optional, admin-supplied label ("for Alice") so a runaway
// note cannot stuff the row.
const maxNote = 200

func normalizeNote(note string) string {
	note = strings.TrimSpace(note)
	if len([]rune(note)) > maxNote {
		note = string([]rune(note)[:maxNote])
	}
	return note
}

// CreateInvite mints a new single-use invite on behalf of an admin. The raw
// code is returned once, here; only its digest is stored. A zero inviteTTL on
// the store means the invite never expires.
func (s *Store) CreateInvite(ctx context.Context, createdBy, note string) (*Invite, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("invite code: %w", err)
	}
	code := base64.RawURLEncoding.EncodeToString(raw)

	now := time.Now().UTC()
	inv := &Invite{
		ID:        uuid.NewString(),
		Code:      code,
		CreatedBy: createdBy,
		CreatedAt: now,
		Note:      normalizeNote(note),
	}
	if s.inviteTTL > 0 {
		inv.ExpiresAt = now.Add(s.inviteTTL)
	}
	var expires sql.NullInt64
	if !inv.ExpiresAt.IsZero() {
		expires = sql.NullInt64{Int64: inv.ExpiresAt.UnixMilli(), Valid: true}
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO invites (id, code_hash, created_by, created_at, expires_at, note)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		inv.ID, tokenHash(code), inv.CreatedBy, inv.CreatedAt.UnixMilli(), expires, inv.Note)
	if err != nil {
		return nil, fmt.Errorf("create invite: %w", err)
	}
	return inv, nil
}

// ListInvites returns every invite the admin has minted, newest first. The raw
// code is never present: it was returned once at creation and is not stored.
// Campaign bindings ride along for the invite manager's campaign rows.
func (s *Store) ListInvites(ctx context.Context) ([]Invite, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.id, i.created_by, i.created_at, i.expires_at, i.used_by, i.used_at, i.note,
		       COALESCE(b.campaign_id, ''), COALESCE(b.role, '')
		  FROM invites i LEFT JOIN campaign_invites b ON b.invite_id = i.id
		 ORDER BY i.created_at DESC, i.id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list invites: %w", err)
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

// RevokeInvite deletes an invite, pending or not. Revoking an unknown id is not
// an error: the caller wanted it gone, and it is. Deleting a spent invite is
// how the admin trims the list once they no longer need the audit row.
func (s *Store) RevokeInvite(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM invites WHERE id = ?`, id); err != nil {
		return fmt.Errorf("revoke invite: %w", err)
	}
	return nil
}

// ConsumeInvite validates a code and marks it used by userID in one atomic
// step, so two requests presenting the same link cannot both mint an account.
// The WHERE used_by IS NULL guard makes the UPDATE a no-op for a code that was
// redeemed in the gap between the SELECT and the UPDATE.
func (s *Store) ConsumeInvite(ctx context.Context, code, userID string) error {
	_, _, _, err := s.consumeInvite(ctx, s.db, code, userID)
	return err
}

// consumeInvite is the runner-parametric core of ConsumeInvite; it returns the
// invite's id and campaign binding (both empty strings when unbound) on
// success, so RegisterWithInvite and the campaign join path can write the
// campaign_members row against the same transaction.
func (s *Store) consumeInvite(ctx context.Context, q runner, code, userID string) (string, string, string, error) {
	if code == "" {
		return "", "", "", ErrInviteInvalid
	}
	row := q.QueryRowContext(ctx, `
		SELECT i.id, i.expires_at, i.used_by, COALESCE(b.campaign_id, ''), COALESCE(b.role, '')
		  FROM invites i LEFT JOIN campaign_invites b ON b.invite_id = i.id
		 WHERE i.code_hash = ?`, tokenHash(code))
	var (
		id         string
		expires    sql.NullInt64
		usedByID   sql.NullString
		campaignID string
		role       string
	)
	if err := row.Scan(&id, &expires, &usedByID, &campaignID, &role); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", "", ErrInviteInvalid
		}
		return "", "", "", fmt.Errorf("consume invite lookup: %w", err)
	}
	if usedByID.Valid {
		return "", "", "", ErrInviteUsed
	}
	if expires.Valid && time.Now().UTC().UnixMilli() >= expires.Int64 {
		return "", "", "", ErrInviteExpired
	}
	now := time.Now().UTC()
	res, err := q.ExecContext(ctx,
		`UPDATE invites SET used_by = ?, used_at = ? WHERE id = ? AND used_by IS NULL`,
		userID, now.UnixMilli(), id)
	if err != nil {
		return "", "", "", fmt.Errorf("consume invite update: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", "", "", fmt.Errorf("consume invite rows: %w", err)
	}
	if n == 0 {
		// Lost the race: another request redeemed it between the SELECT and
		// the UPDATE. Read back as used rather than guessing.
		return "", "", "", ErrInviteUsed
	}
	return id, campaignID, role, nil
}

// RegisterWithInvite validates an invite, creates a non-admin account, and
// consumes the invite — all in one transaction, so a failure mid-way leaves
// neither a half-made account nor a burned invite. The new account is never an
// admin: invites only exist once the install's admin has been bootstrapped, so
// a user count above zero already governs createUser's admin rule.
func (s *Store) RegisterWithInvite(ctx context.Context, username, password, code string) (*User, error) {
	return s.registerWithInvite(ctx, username, password, code, nil)
}

// registerWithInvite is the shared core of RegisterWithInvite and
// RegisterWithCampaignInvite. join, when set, runs inside the same transaction
// after the invite is consumed — with the invite's campaign binding filled in
// — so a campaign invite cannot burn itself without also writing the
// membership row it promised.
func (s *Store) registerWithInvite(ctx context.Context, username, password, code string, join func(ctx context.Context, tx *sql.Tx, u *User, inv Invite) error) (*User, error) {
	// Validate the cheap inputs before opening a transaction, so a malformed
	// request pays nothing for a connection.
	if _, err := normalizeUsername(username); err != nil {
		return nil, err
	}
	if err := checkPassword(password); err != nil {
		return nil, err
	}
	if code == "" {
		return nil, ErrInviteInvalid
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("register tx: %w", err)
	}
	defer tx.Rollback() // noop after a successful commit

	// Create the account first, then consume the invite against the real id.
	// Both run inside the transaction, so an invalid or already-spent invite
	// rolls the account back rather than leaving an orphan the friend cannot
	// retry because the name is now taken.
	u, err := s.createUser(ctx, tx, username, password)
	if err != nil {
		return nil, err
	}
	_, campaignID, role, err := s.consumeInvite(ctx, tx, code, u.ID)
	if err != nil {
		return nil, err
	}
	if join != nil {
		if err := join(ctx, tx, u, Invite{CampaignID: campaignID, CampaignRole: role}); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("register commit: %w", err)
	}
	return u, nil
}

// runner is the subset of *sql.DB the tx-aware auth helpers need, satisfied by
// both *sql.DB and *sql.Tx.
type runner interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func scanInvite(r interface{ Scan(...any) error }) (*Invite, error) {
	var (
		inv        Invite
		expires    sql.NullInt64
		usedBy     sql.NullString
		usedAt     sql.NullInt64
		created    int64
		note       sql.NullString
		campaignID sql.NullString
		role       sql.NullString
	)
	if err := r.Scan(&inv.ID, &inv.CreatedBy, &created, &expires, &usedBy, &usedAt, &note, &campaignID, &role); err != nil {
		return nil, err
	}
	inv.CreatedAt = time.UnixMilli(created).UTC()
	if expires.Valid {
		inv.ExpiresAt = time.UnixMilli(expires.Int64).UTC()
	}
	inv.UsedBy = usedBy.String
	if usedAt.Valid {
		inv.UsedAt = time.UnixMilli(usedAt.Int64).UTC()
	}
	inv.Note = note.String
	inv.CampaignID = campaignID.String
	inv.CampaignRole = role.String
	return &inv, nil
}
