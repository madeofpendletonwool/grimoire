// Package auth stores accounts and server-side sessions for Grimoire.
//
// The tables live in the same SQLite file as the search index and the chat
// history. That is safe: index.Store.Reset only clears the docs tables, so
// rebuilding the rules index never signs anyone out.
//
// Grimoire is a personal, self-hosted app, so there is no registration flow to
// speak of: whoever reaches an empty install claims it by creating the first
// account, and the door is shut behind them unless the operator opens it again.
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
)

// DefaultSessionTTL is how long a session lives when the operator does not say
// otherwise. Long, because this is a personal app on a personal device and
// being logged out weekly is worse than the marginal risk.
const DefaultSessionTTL = 30 * 24 * time.Hour

// User is an account. The password hash never leaves the store.
type User struct {
	ID        string
	Username  string
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

// Store persists users and sessions.
type Store struct {
	db  *sql.DB
	ttl time.Duration
}

// New builds an auth store on an existing database handle and ensures its
// schema exists. A ttl of zero picks DefaultSessionTTL.
func New(db *sql.DB, ttl time.Duration) (*Store, error) {
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	s := &Store{db: db, ttl: ttl}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("auth migrate: %w", err)
	}
	return s, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id            TEXT PRIMARY KEY,
	username      TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL,
	created_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
	token_hash TEXT PRIMARY KEY,
	user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_expiry ON sessions(expires_at);
`

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
		`SELECT id, username, created_at FROM users ORDER BY created_at ASC, id ASC LIMIT 1`)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoUsers
	}
	return u, err
}

// CreateUser hashes the passphrase and records a new account.
func (s *Store) CreateUser(ctx context.Context, username, password string) (*User, error) {
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

	u := &User{ID: uuid.NewString(), Username: name, CreatedAt: time.Now().UTC()}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO users (id, username, password_hash, created_at) VALUES (?, ?, ?, ?)`,
		u.ID, u.Username, hash, u.CreatedAt.UnixMilli())
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

// Authenticate checks a passphrase against an account. An unknown name still
// pays for a full argon2 verification against a dummy hash, so a caller cannot
// tell "no such account" from "wrong passphrase" by timing the response.
func (s *Store) Authenticate(ctx context.Context, username, password string) (*User, error) {
	name := strings.ToLower(strings.TrimSpace(username))
	row := s.db.QueryRowContext(ctx,
		`SELECT id, username, created_at, password_hash FROM users WHERE username = ?`, name)

	var (
		u       User
		created int64
		hash    string
	)
	switch err := row.Scan(&u.ID, &u.Username, &created, &hash); {
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
		`SELECT u.id, u.username, u.created_at, s.expires_at
		   FROM sessions s JOIN users u ON u.id = s.user_id
		  WHERE s.token_hash = ?`, digest)

	var (
		u                User
		created, expires int64
	)
	switch err := row.Scan(&u.ID, &u.Username, &created, &expires); {
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
		created int64
	)
	if err := r.Scan(&u.ID, &u.Username, &created); err != nil {
		return nil, err
	}
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
