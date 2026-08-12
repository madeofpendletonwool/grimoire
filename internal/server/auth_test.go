package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/chat"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
)

// newGatedServer builds a server with accounts enabled and nobody signed in,
// so the tests below can drive the first-run flow themselves.
func newGatedServer(t *testing.T) (*Server, *chat.Store, *auth.Store) {
	t.Helper()
	store, err := index.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	chats, err := chat.New(store.DB())
	if err != nil {
		t.Fatalf("open chat store: %v", err)
	}
	users, err := auth.New(store.DB(), 0)
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, chats, Auth{Users: users})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return s, chats, users
}

// call sends a request without a session and returns the raw recorder, so
// tests can inspect cookies and bodies as well as the status.
func call(s *Server, method, target, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func sessionFrom(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatalf("no %s cookie in the response", sessionCookie)
	return nil
}

func credsJSON(username, password string) string {
	return fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)
}

func TestAPIRequiresASession(t *testing.T) {
	s, _, _ := newGatedServer(t)

	// /healthz is Docker's healthcheck; it has no session and must not need
	// one. The auth handshake stays open for the same reason the login page
	// needs it. Everything else is closed.
	tests := []struct {
		name   string
		method string
		target string
		want   int
	}{
		{"healthz stays open", http.MethodGet, "/healthz", http.StatusOK},
		{"auth state stays open", http.MethodGet, "/api/auth/state", http.StatusOK},
		{"meta is gated", http.MethodGet, "/api/meta", http.StatusUnauthorized},
		{"search is gated", http.MethodGet, "/api/search?q=ward", http.StatusUnauthorized},
		{"chat list is gated", http.MethodGet, "/api/chats", http.StatusUnauthorized},
		{"ask is gated", http.MethodPost, "/api/ask", http.StatusUnauthorized},
		{"logout is gated", http.MethodPost, "/api/auth/logout", http.StatusUnauthorized},
		{"an open path cannot be walked out of", http.MethodGet, "/api/auth/state/../chats", http.StatusUnauthorized},
		{"a doubled slash does not slip past", http.MethodGet, "//api/chats", http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := call(s, tt.method, tt.target, "")
			if rec.Code != tt.want {
				t.Errorf("%s %s: status %d, want %d", tt.method, tt.target, rec.Code, tt.want)
			}
			if tt.want != http.StatusUnauthorized {
				return
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("401 body is not JSON: %v (%s)", err, rec.Body)
			}
			if body["error"] == nil {
				t.Errorf("401 body carries no error field: %s", rec.Body)
			}
		})
	}
}

func TestStaticAssetsStayReachableWhenSignedOut(t *testing.T) {
	s, _, _ := newGatedServer(t)
	if rec := call(s, http.MethodGet, "/static/style.css", ""); rec.Code != http.StatusOK {
		t.Errorf("style.css: status %d, want 200 — the gate needs its own stylesheet", rec.Code)
	}
	if rec := call(s, http.MethodGet, "/static/js/auth.js", ""); rec.Code != http.StatusOK {
		t.Errorf("auth.js: status %d, want 200", rec.Code)
	}
}

func TestIndexServesTheGateUntilSignedIn(t *testing.T) {
	s, _, _ := newGatedServer(t)

	rec := call(s, http.MethodGet, "/", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "gate-form") {
		t.Error("signed out, / should render the login screen")
	}

	setup := call(s, http.MethodPost, "/api/auth/setup", credsJSON("keeper", "a-fine-passphrase"))
	if setup.Code != http.StatusCreated {
		t.Fatalf("setup: status %d, body %s", setup.Code, setup.Body)
	}
	rec = call(s, http.MethodGet, "/", "", sessionFrom(t, setup))
	if !strings.Contains(rec.Body.String(), `id="composer"`) {
		t.Error("signed in, / should render the app")
	}
}

func TestSetupIsClosedOnceAKeeperExists(t *testing.T) {
	s, _, _ := newGatedServer(t)

	state := call(s, http.MethodGet, "/api/auth/state", "")
	var v map[string]any
	_ = json.Unmarshal(state.Body.Bytes(), &v)
	if v["setup_required"] != true || v["authenticated"] != false {
		t.Errorf("fresh install state = %v, want setup_required and nobody signed in", v)
	}

	if rec := call(s, http.MethodPost, "/api/auth/setup", credsJSON("keeper", "a-fine-passphrase")); rec.Code != http.StatusCreated {
		t.Fatalf("setup: status %d, body %s", rec.Code, rec.Body)
	}

	rec := call(s, http.MethodPost, "/api/auth/setup", credsJSON("interloper", "a-fine-passphrase"))
	if rec.Code != http.StatusForbidden {
		t.Errorf("second account: status %d, want 403 with registration closed", rec.Code)
	}

	state = call(s, http.MethodGet, "/api/auth/state", "")
	_ = json.Unmarshal(state.Body.Bytes(), &v)
	if v["setup_required"] != false {
		t.Errorf("state after setup = %v, want setup_required false", v)
	}
}

func TestSetupStaysOpenWithOpenRegistration(t *testing.T) {
	s, _, _ := newGatedServer(t)
	s.openRegistration = true

	if rec := call(s, http.MethodPost, "/api/auth/setup", credsJSON("keeper", "a-fine-passphrase")); rec.Code != http.StatusCreated {
		t.Fatalf("first account: status %d, body %s", rec.Code, rec.Body)
	}
	if rec := call(s, http.MethodPost, "/api/auth/setup", credsJSON("scribe", "a-fine-passphrase")); rec.Code != http.StatusCreated {
		t.Errorf("second account: status %d, want 201 with registration open", rec.Code)
	}
	if rec := call(s, http.MethodPost, "/api/auth/setup", credsJSON("KEEPER", "a-fine-passphrase")); rec.Code != http.StatusConflict {
		t.Errorf("duplicate name: status %d, want 409", rec.Code)
	}
}

func TestLoginLogoutRoundTrip(t *testing.T) {
	s, _, _ := newGatedServer(t)
	if rec := call(s, http.MethodPost, "/api/auth/setup", credsJSON("keeper", "a-fine-passphrase")); rec.Code != http.StatusCreated {
		t.Fatalf("setup: status %d, body %s", rec.Code, rec.Body)
	}

	rec := call(s, http.MethodPost, "/api/auth/login", credsJSON("keeper", "a-fine-passphrase"))
	if rec.Code != http.StatusOK {
		t.Fatalf("login: status %d, body %s", rec.Code, rec.Body)
	}
	cookie := sessionFrom(t, rec)
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie = %+v, want HttpOnly and SameSite=Lax", cookie)
	}
	if cookie.Secure {
		t.Error("session cookie is Secure over plain HTTP; the browser would drop it")
	}
	if strings.Contains(rec.Body.String(), "a-fine-passphrase") {
		t.Error("the login response echoes the passphrase")
	}

	if rec := call(s, http.MethodGet, "/api/chats", "", cookie); rec.Code != http.StatusOK {
		t.Errorf("signed in, /api/chats: status %d, want 200", rec.Code)
	}

	if rec := call(s, http.MethodPost, "/api/auth/logout", "", cookie); rec.Code != http.StatusNoContent {
		t.Errorf("logout: status %d, want 204", rec.Code)
	}
	if rec := call(s, http.MethodGet, "/api/chats", "", cookie); rec.Code != http.StatusUnauthorized {
		t.Errorf("after logout the token should be revoked server-side, got status %d", rec.Code)
	}
}

func TestLoginFailuresDoNotLeakWhetherAnAccountExists(t *testing.T) {
	s, _, _ := newGatedServer(t)
	if rec := call(s, http.MethodPost, "/api/auth/setup", credsJSON("keeper", "a-fine-passphrase")); rec.Code != http.StatusCreated {
		t.Fatalf("setup: status %d", rec.Code)
	}

	wrongPassword := call(s, http.MethodPost, "/api/auth/login", credsJSON("keeper", "not-the-passphrase"))
	unknownName := call(s, http.MethodPost, "/api/auth/login", credsJSON("nobody", "not-the-passphrase"))

	if wrongPassword.Code != http.StatusUnauthorized || unknownName.Code != http.StatusUnauthorized {
		t.Fatalf("statuses = %d and %d, want 401 for both", wrongPassword.Code, unknownName.Code)
	}
	if wrongPassword.Body.String() != unknownName.Body.String() {
		t.Errorf("a wrong passphrase (%s) reads differently from an unknown name (%s)",
			strings.TrimSpace(wrongPassword.Body.String()), strings.TrimSpace(unknownName.Body.String()))
	}
}

func TestSessionCookieIsSecureBehindTLSTermination(t *testing.T) {
	s, _, _ := newGatedServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/setup",
		strings.NewReader(credsJSON("keeper", "a-fine-passphrase")))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("X-Forwarded-Proto", "https, http")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("setup: status %d, body %s", rec.Code, rec.Body)
	}
	if !sessionFrom(t, rec).Secure {
		t.Error("cookie is not Secure though the client-facing hop was HTTPS")
	}
}

func TestChatsAreScopedToTheSignedInUser(t *testing.T) {
	s, _, _ := newGatedServer(t)
	s.openRegistration = true

	keeper := sessionFrom(t, call(s, http.MethodPost, "/api/auth/setup", credsJSON("keeper", "a-fine-passphrase")))
	scribe := sessionFrom(t, call(s, http.MethodPost, "/api/auth/setup", credsJSON("scribe", "a-fine-passphrase")))

	created := call(s, http.MethodPost, "/api/chats", `{"corpus":"mtg"}`, keeper)
	if created.Code != http.StatusCreated {
		t.Fatalf("create chat: status %d, body %s", created.Code, created.Body)
	}
	var body map[string]any
	_ = json.Unmarshal(created.Body.Bytes(), &body)
	id := body["chat"].(map[string]any)["id"].(string)

	if rec := call(s, http.MethodGet, "/api/chats/"+id, "", scribe); rec.Code != http.StatusNotFound {
		t.Errorf("another keeper reading the conversation: status %d, want 404", rec.Code)
	}

	listed := call(s, http.MethodGet, "/api/chats", "", scribe)
	_ = json.Unmarshal(listed.Body.Bytes(), &body)
	if chats, _ := body["chats"].([]any); len(chats) != 0 {
		t.Errorf("another keeper sees %d conversations, want none", len(chats))
	}
}

func TestSetupAdoptsPreAuthenticationChats(t *testing.T) {
	s, chats, _ := newGatedServer(t)
	ctx := context.Background()

	// Stand in for an install upgraded from before there were accounts.
	before, err := chats.Create(ctx, chat.AnonymousUser, "mtg", "written before accounts")
	if err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	setup := call(s, http.MethodPost, "/api/auth/setup", credsJSON("keeper", "a-fine-passphrase"))
	if setup.Code != http.StatusCreated {
		t.Fatalf("setup: status %d, body %s", setup.Code, setup.Body)
	}

	rec := call(s, http.MethodGet, "/api/chats/"+before.ID, "", sessionFrom(t, setup))
	if rec.Code != http.StatusOK {
		t.Fatalf("the first keeper should inherit pre-authentication history: status %d", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if got := body["chat"].(map[string]any)["title"]; got != "written before accounts" {
		t.Errorf("adopted conversation title = %v", got)
	}
}

func TestAdoptAnonymousChatsIsIdempotent(t *testing.T) {
	_, chats, users := newGatedServer(t)
	ctx := context.Background()

	if _, err := chats.Create(ctx, chat.AnonymousUser, "mtg", "orphan"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Nobody to adopt into yet.
	if n, err := AdoptAnonymousChats(ctx, users, chats); err != nil || n != 0 {
		t.Fatalf("on an unclaimed install = %d, %v; want 0, nil", n, err)
	}

	if _, err := users.CreateUser(ctx, "keeper", "a-fine-passphrase"); err != nil {
		t.Fatalf("create user: %v", err)
	}
	n, err := AdoptAnonymousChats(ctx, users, chats)
	if err != nil || n != 1 {
		t.Fatalf("first run = %d, %v; want 1, nil", n, err)
	}
	if n, err := AdoptAnonymousChats(ctx, users, chats); err != nil || n != 0 {
		t.Errorf("second run = %d, %v; want 0, nil", n, err)
	}
}
