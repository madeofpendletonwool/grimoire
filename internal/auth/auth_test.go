package auth

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// cheapParams keep the tests honest about the format without paying for real
// argon2 cost on every case. Production cost is exercised by defaultParams
// living inside every encoded hash, which decodeHash reads back.
var cheapParams = argon2Params{memory: 8 * 1024, time: 1, threads: 1, keyLen: 32, saltLen: 16}

func testStore(t *testing.T, ttl time.Duration) *Store {
	t.Helper()

	// Hashing dominates these tests otherwise, and the parameters under test
	// are the ones recorded in the hash rather than the ones in this var.
	restore := defaultParams
	defaultParams = cheapParams
	t.Cleanup(func() { defaultParams = restore })

	path := filepath.Join(t.TempDir(), "auth.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	// No invite expiry by default; invite-specific tests use testInviteStore.
	s, err := New(db, ttl, 0)
	if err != nil {
		t.Fatalf("new auth store: %v", err)
	}
	return s
}

// testInviteStore builds a store whose invites expire after inviteTTL, for the
// expiry tests. A session TTL is still required for New.
func testInviteStore(t *testing.T, inviteTTL time.Duration) *Store {
	t.Helper()
	restore := defaultParams
	defaultParams = cheapParams
	t.Cleanup(func() { defaultParams = restore })

	path := filepath.Join(t.TempDir(), "auth.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	s, err := New(db, time.Hour, inviteTTL)
	if err != nil {
		t.Fatalf("new auth store: %v", err)
	}
	return s
}

func mustUser(t *testing.T, s *Store, name, password string) *User {
	t.Helper()
	u, err := s.CreateUser(context.Background(), name, password)
	if err != nil {
		t.Fatalf("create user %q: %v", name, err)
	}
	return u
}

func TestHashPasswordRoundTrip(t *testing.T) {
	encoded, err := hashPassword("a-good-passphrase", cheapParams)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=19$m=8192,t=1,p=1$") {
		t.Errorf("encoded hash does not carry its parameters: %q", encoded)
	}

	tests := []struct {
		name     string
		password string
		want     bool
	}{
		{"correct", "a-good-passphrase", true},
		{"wrong", "a-good-passphras", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := verifyPassword(encoded, tt.password)
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			if got != tt.want {
				t.Errorf("verify(%q) = %v, want %v", tt.password, got, tt.want)
			}
		})
	}
}

func TestHashPasswordSaltsEachHash(t *testing.T) {
	a, _ := hashPassword("same-passphrase", cheapParams)
	b, _ := hashPassword("same-passphrase", cheapParams)
	if a == b {
		t.Error("two hashes of the same passphrase are identical — the salt is not random")
	}
}

func TestVerifyPasswordRejectsMalformedHash(t *testing.T) {
	tests := []struct {
		name    string
		encoded string
	}{
		{"empty", ""},
		{"not phc", "plaintext"},
		{"wrong algorithm", "$argon2i$v=19$m=8192,t=1,p=1$c2FsdHNhbHRzYWx0$aGFzaA"},
		{"wrong version", "$argon2id$v=16$m=8192,t=1,p=1$c2FsdHNhbHRzYWx0$aGFzaA"},
		{"garbled params", "$argon2id$v=19$m=lots$c2FsdHNhbHRzYWx0$aGFzaA"},
		{"bad base64 salt", "$argon2id$v=19$m=8192,t=1,p=1$!!!$aGFzaA"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, err := verifyPassword(tt.encoded, "anything")
			if ok || err == nil {
				t.Errorf("verify(%q) = %v, %v — want a rejection", tt.encoded, ok, err)
			}
		})
	}
}

func TestCreateUserValidation(t *testing.T) {
	s := testStore(t, 0)
	ctx := context.Background()

	tests := []struct {
		name     string
		username string
		password string
		wantErr  bool
	}{
		{"ok", "keeper", "a-fine-passphrase", false},
		{"name too short", "k", "a-fine-passphrase", true},
		{"name with a space", "the keeper", "a-fine-passphrase", true},
		{"passphrase too short", "scribe", "short", true},
		{"trimmed and folded", "  SCRIBE  ", "a-fine-passphrase", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := s.CreateUser(ctx, tt.username, tt.password)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("CreateUser(%q) succeeded, want an error", tt.username)
				}
				return
			}
			if err != nil {
				t.Fatalf("CreateUser(%q): %v", tt.username, err)
			}
			if u.Username != strings.ToLower(strings.TrimSpace(tt.username)) {
				t.Errorf("username = %q, want it trimmed and folded", u.Username)
			}
		})
	}
}

func TestCreateUserRejectsDuplicateName(t *testing.T) {
	s := testStore(t, 0)
	ctx := context.Background()
	mustUser(t, s, "keeper", "a-fine-passphrase")

	// Case folding is the point: "Keeper" and "keeper" are one account.
	if _, err := s.CreateUser(ctx, "Keeper", "another-passphrase"); !errors.Is(err, ErrUsernameTaken) {
		t.Errorf("duplicate name: err = %v, want ErrUsernameTaken", err)
	}
}

func TestAuthenticate(t *testing.T) {
	s := testStore(t, 0)
	ctx := context.Background()
	mustUser(t, s, "keeper", "a-fine-passphrase")

	tests := []struct {
		name     string
		username string
		password string
		wantErr  error
	}{
		{"correct", "keeper", "a-fine-passphrase", nil},
		{"correct, odd casing", "KEEPER", "a-fine-passphrase", nil},
		{"wrong passphrase", "keeper", "a-fine-passphras", ErrInvalidCredentials},
		{"unknown account", "nobody", "a-fine-passphrase", ErrInvalidCredentials},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := s.Authenticate(ctx, tt.username, tt.password)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("authenticate: %v", err)
			}
			if u.Username != "keeper" {
				t.Errorf("username = %q, want keeper", u.Username)
			}
		})
	}
}

func TestCountAndFirst(t *testing.T) {
	s := testStore(t, 0)
	ctx := context.Background()

	if n, err := s.Count(ctx); err != nil || n != 0 {
		t.Fatalf("Count on an empty store = %d, %v; want 0", n, err)
	}
	if _, err := s.First(ctx); !errors.Is(err, ErrNoUsers) {
		t.Errorf("First on an empty store: err = %v, want ErrNoUsers", err)
	}

	first := mustUser(t, s, "keeper", "a-fine-passphrase")
	mustUser(t, s, "scribe", "a-fine-passphrase")

	if n, err := s.Count(ctx); err != nil || n != 2 {
		t.Errorf("Count = %d, %v; want 2", n, err)
	}
	got, err := s.First(ctx)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if got.ID != first.ID {
		t.Errorf("First = %q, want the oldest account %q", got.Username, first.Username)
	}
}

func TestSessionLifecycle(t *testing.T) {
	s := testStore(t, time.Hour)
	ctx := context.Background()
	u := mustUser(t, s, "keeper", "a-fine-passphrase")

	sess, err := s.StartSession(ctx, u.ID)
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	if sess.Token == "" {
		t.Fatal("expected a session token")
	}

	got, err := s.Lookup(ctx, sess.Token)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.ID != u.ID {
		t.Errorf("session resolved to %q, want %q", got.Username, u.Username)
	}

	if err := s.EndSession(ctx, sess.Token); err != nil {
		t.Fatalf("end session: %v", err)
	}
	if _, err := s.Lookup(ctx, sess.Token); !errors.Is(err, ErrNoSession) {
		t.Errorf("after logout: err = %v, want ErrNoSession", err)
	}
}

func TestLookupRejectsUnknownAndExpired(t *testing.T) {
	ctx := context.Background()

	live := testStore(t, time.Hour)
	u := mustUser(t, live, "keeper", "a-fine-passphrase")
	if _, err := live.Lookup(ctx, ""); !errors.Is(err, ErrNoSession) {
		t.Errorf("empty token: err = %v, want ErrNoSession", err)
	}
	if _, err := live.Lookup(ctx, "not-a-real-token"); !errors.Is(err, ErrNoSession) {
		t.Errorf("unknown token: err = %v, want ErrNoSession", err)
	}

	// A one-nanosecond lifetime is already over by the time it is stored, so
	// expiry is checked without making the test sleep.
	short, err := New(live.db, time.Nanosecond, 0)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	sess, err := short.StartSession(ctx, u.ID)
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	if _, err := short.Lookup(ctx, sess.Token); !errors.Is(err, ErrNoSession) {
		t.Errorf("expired session: err = %v, want ErrNoSession", err)
	}
	if n := countSessions(t, live); n != 0 {
		t.Errorf("%d expired sessions left behind — Lookup should drop the dead row", n)
	}
}

func TestStartSessionPurgesExpired(t *testing.T) {
	ctx := context.Background()
	live := testStore(t, time.Hour)
	u := mustUser(t, live, "keeper", "a-fine-passphrase")

	short, err := New(live.db, time.Nanosecond, 0)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := short.StartSession(ctx, u.ID); err != nil {
			t.Fatalf("start session: %v", err)
		}
	}

	if _, err := live.StartSession(ctx, u.ID); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if n := countSessions(t, live); n != 1 {
		t.Errorf("%d sessions remain, want only the live one — expired rows should be swept", n)
	}
}

func TestSessionTokenIsNotStoredRaw(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, time.Hour)
	u := mustUser(t, s, "keeper", "a-fine-passphrase")

	sess, err := s.StartSession(ctx, u.ID)
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE token_hash = ?`, sess.Token).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 0 {
		t.Error("the raw session token is stored in the database; only its digest should be")
	}
}

func TestEndSessionOfUnknownTokenIsNotAnError(t *testing.T) {
	s := testStore(t, time.Hour)
	if err := s.EndSession(context.Background(), "not-a-real-token"); err != nil {
		t.Errorf("ending an unknown session: %v", err)
	}
	if err := s.EndSession(context.Background(), ""); err != nil {
		t.Errorf("ending an empty session: %v", err)
	}
}

func countSessions(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	return n
}

/* ---------- admin ---------- */

func TestFirstUserIsAdmin(t *testing.T) {
	s := testStore(t, 0)
	ctx := context.Background()

	keeper := mustUser(t, s, "keeper", "a-fine-passphrase")
	if !keeper.IsAdmin {
		t.Errorf("first account: IsAdmin = false, want true (first user is the admin)")
	}
	friend := mustUser(t, s, "friend", "a-fine-passphrase")
	if friend.IsAdmin {
		t.Errorf("second account: IsAdmin = true, want false (only the first user is admin)")
	}

	// First() agrees: the oldest account is the admin.
	first, err := s.First(ctx)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if first.ID != keeper.ID || !first.IsAdmin {
		t.Errorf("First = %+v, want the keeper as admin", first)
	}
}

func TestIsAdmin(t *testing.T) {
	s := testStore(t, 0)
	ctx := context.Background()
	keeper := mustUser(t, s, "keeper", "a-fine-passphrase")
	friend := mustUser(t, s, "friend", "a-fine-passphrase")

	if ok, err := s.IsAdmin(ctx, keeper.ID); err != nil || !ok {
		t.Errorf("IsAdmin(keeper) = %v, %v; want true", ok, err)
	}
	if ok, err := s.IsAdmin(ctx, friend.ID); err != nil || ok {
		t.Errorf("IsAdmin(friend) = %v, %v; want false", ok, err)
	}
	// An unknown id is not the admin, and not an error.
	if ok, err := s.IsAdmin(ctx, "no-such-user"); err != nil || ok {
		t.Errorf("IsAdmin(unknown) = %v, %v; want false", ok, err)
	}
}

// TestMigrateBackfillsOldestAsAdmin simulates an install upgraded from before
// admins existed: the users table predates the is_admin column. The migration
// must add it and mark the oldest account admin, matching "first created user
// is the admin."
func TestMigrateBackfillsOldestAsAdmin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	// The pre-admin schema: no is_admin column.
	if _, err := db.Exec(`CREATE TABLE users (
		id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE,
		password_hash TEXT NOT NULL, created_at INTEGER NOT NULL)`); err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO users (id, username, password_hash, created_at) VALUES
		('second', 'scribe', 'x', 2000),
		('first',  'keeper', 'x', 1000)`); err != nil {
		t.Fatalf("seed users: %v", err)
	}

	// Migrate. cheapParams would not apply here (no hashing), but the backfill
	// only reads/writes the is_admin column.
	s, err := New(db, 0, 0)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()

	first, err := s.First(ctx)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if first.Username != "keeper" || !first.IsAdmin {
		t.Errorf("oldest account after migrate = %+v, want keeper as admin", first)
	}
	if ok, err := s.IsAdmin(ctx, "second"); err != nil || ok {
		t.Errorf("second account after migrate IsAdmin = %v, %v; want false", ok, err)
	}

	// The column now exists, so a re-run is a harmless no-op.
	if _, err := New(db, 0, 0); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
}

/* ---------- invites ---------- */

func TestCreateInviteReturnsCodeOnce(t *testing.T) {
	s := testInviteStore(t, time.Hour) // a TTL-bearing store mints expiring invites
	ctx := context.Background()
	admin := mustUser(t, s, "keeper", "a-fine-passphrase")

	inv, err := s.CreateInvite(ctx, admin.ID, "for Alice")
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	if inv.Code == "" || inv.ID == "" {
		t.Errorf("invite missing code/id: %+v", inv)
	}
	if inv.Note != "for Alice" {
		t.Errorf("note = %q, want %q", inv.Note, "for Alice")
	}
	if inv.ExpiresAt.IsZero() {
		t.Error("a default-TTL store should mint invites that expire; this one never expires")
	}
	if !inv.Pending() {
		t.Error("a fresh invite should be pending")
	}

	listed, err := s.ListInvites(ctx)
	if err != nil {
		t.Fatalf("list invites: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("len(listed) = %d, want 1", len(listed))
	}
	if listed[0].Code != "" {
		t.Error("ListInvites returned the raw code; only the digest should ever leave the store")
	}
	if listed[0].Note != "for Alice" {
		t.Errorf("listed note = %q", listed[0].Note)
	}
	if !rawInviteCodeIsNotStored(t, s, inv.Code) {
		t.Error("the raw invite code is stored in the database; only its digest should be")
	}
}

func rawInviteCodeIsNotStored(t *testing.T, s *Store, code string) bool {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM invites WHERE code_hash = ?`, code).Scan(&n); err != nil {
		t.Fatalf("query invites: %v", err)
	}
	return n == 0
}

func TestInviteNeverExpiresWhenTTLZero(t *testing.T) {
	s := testInviteStore(t, 0) // zero TTL => never expire
	ctx := context.Background()
	admin := mustUser(t, s, "keeper", "a-fine-passphrase")

	inv, err := s.CreateInvite(ctx, admin.ID, "")
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	if !inv.ExpiresAt.IsZero() {
		t.Errorf("zero-TTL store minted an invite with ExpiresAt = %v; want no expiry", inv.ExpiresAt)
	}
	if inv.Expired() {
		t.Error("a never-expiring invite reports Expired")
	}
}

func TestConsumeInvite(t *testing.T) {
	s := testStore(t, 0)
	ctx := context.Background()
	admin := mustUser(t, s, "keeper", "a-fine-passphrase")
	friend := mustUser(t, s, "friend", "a-fine-passphrase")

	inv, err := s.CreateInvite(ctx, admin.ID, "")
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	if err := s.ConsumeInvite(ctx, inv.Code, friend.ID); err != nil {
		t.Fatalf("consume a fresh invite: %v", err)
	}
	// Single-use: a second consume of the same code is rejected as used.
	if err := s.ConsumeInvite(ctx, inv.Code, friend.ID); !errors.Is(err, ErrInviteUsed) {
		t.Errorf("reuse: err = %v, want ErrInviteUsed", err)
	}
	// An unknown code is invalid, not "used".
	if err := s.ConsumeInvite(ctx, "not-a-real-code", friend.ID); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("unknown code: err = %v, want ErrInviteInvalid", err)
	}
	if err := s.ConsumeInvite(ctx, "", friend.ID); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("empty code: err = %v, want ErrInviteInvalid", err)
	}
}

func TestConsumeExpiredInvite(t *testing.T) {
	s := testInviteStore(t, time.Nanosecond) // already past by the time it is stored
	ctx := context.Background()
	admin := mustUser(t, s, "keeper", "a-fine-passphrase")

	inv, err := s.CreateInvite(ctx, admin.ID, "")
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	if err := s.ConsumeInvite(ctx, inv.Code, admin.ID); !errors.Is(err, ErrInviteExpired) {
		t.Errorf("expired invite: err = %v, want ErrInviteExpired", err)
	}
}

func TestRevokeInvite(t *testing.T) {
	s := testStore(t, 0)
	ctx := context.Background()
	admin := mustUser(t, s, "keeper", "a-fine-passphrase")

	inv, err := s.CreateInvite(ctx, admin.ID, "")
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	if err := s.RevokeInvite(ctx, inv.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	listed, err := s.ListInvites(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 0 {
		t.Errorf("after revoke, %d invites remain, want none", len(listed))
	}
	// A revoked invite's code no longer validates.
	if err := s.ConsumeInvite(ctx, inv.Code, admin.ID); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("revoked invite consume: err = %v, want ErrInviteInvalid", err)
	}
	// Revoking an unknown id is not an error.
	if err := s.RevokeInvite(ctx, "no-such-invite"); err != nil {
		t.Errorf("revoke unknown: %v", err)
	}
}

func TestRegisterWithInvite(t *testing.T) {
	s := testStore(t, 0)
	ctx := context.Background()
	admin := mustUser(t, s, "keeper", "a-fine-passphrase")
	inv, err := s.CreateInvite(ctx, admin.ID, "for a friend")
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}

	u, err := s.RegisterWithInvite(ctx, "newfriend", "a-fine-passphrase", inv.Code)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if u.IsAdmin {
		t.Error("a registered account must never be the admin")
	}
	// The invite is now spent.
	if _, err := s.RegisterWithInvite(ctx, "another", "a-fine-passphrase", inv.Code); !errors.Is(err, ErrInviteUsed) {
		t.Errorf("reuse after register: err = %v, want ErrInviteUsed", err)
	}
}

// TestRegisterRollsBackOnTakenName checks the transactional guarantee: when the
// account cannot be created (name taken), neither a half-made account nor a
// burned invite is left behind — the friend can retry with a new name on the
// same invite.
func TestRegisterRollsBackOnTakenName(t *testing.T) {
	s := testStore(t, 0)
	ctx := context.Background()
	admin := mustUser(t, s, "keeper", "a-fine-passphrase")
	// Pre-create a user that collides with the registration attempt.
	collide, err := s.CreateUser(ctx, "taken", "a-fine-passphrase")
	if err != nil {
		t.Fatalf("seed collision: %v", err)
	}
	inv, err := s.CreateInvite(ctx, admin.ID, "")
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}

	if _, err := s.RegisterWithInvite(ctx, "taken", "a-fine-passphrase", inv.Code); !errors.Is(err, ErrUsernameTaken) {
		t.Errorf("colliding name: err = %v, want ErrUsernameTaken", err)
	}
	// The invite survived the rollback and is still usable.
	u, err := s.RegisterWithInvite(ctx, "friend", "a-fine-passphrase", inv.Code)
	if err != nil {
		t.Fatalf("retry on the same invite after a rolled-back attempt: %v", err)
	}
	if u.ID == collide.ID {
		t.Error("the retried registration resolved to the colliding account")
	}
}

func TestRegisterRejectsBadInvite(t *testing.T) {
	s := testStore(t, 0)
	ctx := context.Background()
	mustUser(t, s, "keeper", "a-fine-passphrase")

	if _, err := s.RegisterWithInvite(ctx, "friend", "a-fine-passphrase", ""); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("empty invite: err = %v, want ErrInviteInvalid", err)
	}
	if _, err := s.RegisterWithInvite(ctx, "friend", "a-fine-passphrase", "bogus"); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("unknown invite: err = %v, want ErrInviteInvalid", err)
	}
	// And no user was created on the way.
	if n, _ := s.Count(ctx); n != 1 {
		t.Errorf("user count after failed registrations = %d, want 1 (only the keeper)", n)
	}
}
