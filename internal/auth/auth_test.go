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

	s, err := New(db, ttl)
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
	short, err := New(live.db, time.Nanosecond)
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

	short, err := New(live.db, time.Nanosecond)
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
