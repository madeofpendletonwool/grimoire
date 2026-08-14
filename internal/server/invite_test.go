package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// registerJSON builds the body for the invite-gated signup endpoint.
func registerJSON(username, password, invite string) string {
	return `{"username":` + quote(username) + `,"password":` + quote(password) + `,"invite":` + quote(invite) + `}`
}

// quote is a tiny JSON string encoder for test bodies, so the file need not pull
// in encoding/json just to interpolate a few fields.
func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// adminSession claims a fresh install (creating the admin) and returns its
// session cookie, so each invite test starts from a known admin.
func adminSession(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	rec := call(s, http.MethodPost, "/api/auth/setup", credsJSON("keeper", "a-fine-passphrase"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup admin: status %d, body %s", rec.Code, rec.Body)
	}
	return sessionFrom(t, rec)
}

// createInvite mints an invite as admin and returns the parsed invite view.
func createInvite(t *testing.T, s *Server, admin *http.Cookie, note string) map[string]any {
	t.Helper()
	body := ""
	if note != "" {
		body = `{"note":` + quote(note) + `}`
	}
	rec := call(s, http.MethodPost, "/api/invites", body, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create invite: status %d, body %s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("create invite body: %v (%s)", err, rec.Body)
	}
	inv, ok := resp["invite"].(map[string]any)
	if !ok {
		t.Fatalf("create invite: no invite in response: %s", rec.Body)
	}
	return inv
}

func TestSetupCreatesAdminAndStateReportsIt(t *testing.T) {
	s, _, _ := newGatedServer(t)
	cookie := adminSession(t, s)

	state := call(s, http.MethodGet, "/api/auth/state", "", cookie)
	var v map[string]any
	if err := json.Unmarshal(state.Body.Bytes(), &v); err != nil {
		t.Fatalf("state: %v", err)
	}
	if v["is_admin"] != true {
		t.Errorf("admin's auth state is_admin = %v, want true", v["is_admin"])
	}
}

func TestRegisterSignsUpAFriendFromAnInvite(t *testing.T) {
	s, _, _ := newGatedServer(t)
	admin := adminSession(t, s)
	inv := createInvite(t, s, admin, "for a friend")
	code, _ := inv["code"].(string)

	rec := call(s, http.MethodPost, "/api/auth/register", registerJSON("friend", "a-fine-passphrase", code))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: status %d, body %s", rec.Code, rec.Body)
	}
	cookie := sessionFrom(t, rec)

	// The friend is signed in, and is not the admin.
	state := call(s, http.MethodGet, "/api/auth/state", "", cookie)
	var v map[string]any
	_ = json.Unmarshal(state.Body.Bytes(), &v)
	if v["authenticated"] != true || v["username"] != "friend" {
		t.Errorf("friend's state = %v, want authenticated as friend", v)
	}
	if v["is_admin"] == true {
		t.Error("a friend who registered via invite must not be an admin")
	}
}

func TestRegisterRejectsBadInvite(t *testing.T) {
	s, _, _ := newGatedServer(t)
	adminSession(t, s) // claim the install so registration is the only path

	// No invite code at all.
	if rec := call(s, http.MethodPost, "/api/auth/register", registerJSON("friend", "a-fine-passphrase", "")); rec.Code != http.StatusBadRequest {
		t.Errorf("empty invite: status %d, want 400", rec.Code)
	}
	// An unknown code.
	if rec := call(s, http.MethodPost, "/api/auth/register", registerJSON("friend", "a-fine-passphrase", "bogus")); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown invite: status %d, want 400", rec.Code)
	}
}

func TestRegisterInviteIsSingleUse(t *testing.T) {
	s, _, _ := newGatedServer(t)
	admin := adminSession(t, s)
	inv := createInvite(t, s, admin, "")
	code, _ := inv["code"].(string)

	if rec := call(s, http.MethodPost, "/api/auth/register", registerJSON("alice", "a-fine-passphrase", code)); rec.Code != http.StatusCreated {
		t.Fatalf("first use: status %d, body %s", rec.Code, rec.Body)
	}
	// The same link cannot mint a second account.
	rec := call(s, http.MethodPost, "/api/auth/register", registerJSON("bob", "a-fine-passphrase", code))
	if rec.Code != http.StatusGone {
		t.Errorf("reuse: status %d, want 410 Gone", rec.Code)
	}
}

func TestRegisterRollsBackOnTakenName(t *testing.T) {
	s, _, _ := newGatedServer(t)
	admin := adminSession(t, s)
	// A user whose name the registration will collide with.
	s.openRegistration = true
	call(s, http.MethodPost, "/api/auth/setup", credsJSON("taken", "a-fine-passphrase"))

	inv := createInvite(t, s, admin, "")
	code, _ := inv["code"].(string)

	// Colliding name fails — 409, not 201.
	if rec := call(s, http.MethodPost, "/api/auth/register", registerJSON("taken", "a-fine-passphrase", code)); rec.Code != http.StatusConflict {
		t.Errorf("colliding name: status %d, want 409", rec.Code)
	}
	// The invite survived the rolled-back attempt and still works for a new name.
	if rec := call(s, http.MethodPost, "/api/auth/register", registerJSON("friend", "a-fine-passphrase", code)); rec.Code != http.StatusCreated {
		t.Errorf("retry on same invite after rollback: status %d, want 201 (body %s)", rec.Code, rec.Body)
	}
}

func TestInviteEndpointsRequireAdmin(t *testing.T) {
	s, _, _ := newGatedServer(t)
	admin := adminSession(t, s)
	inv := createInvite(t, s, admin, "")
	id, _ := inv["id"].(string)

	// Register a non-admin friend to use as the forbidden caller.
	code, _ := inv["code"].(string)
	friend := sessionFrom(t, call(s, http.MethodPost, "/api/auth/register", registerJSON("friend", "a-fine-passphrase", code)))

	cases := []struct {
		name   string
		method string
		target string
	}{
		{"create", http.MethodPost, "/api/invites"},
		{"list", http.MethodGet, "/api/invites"},
		{"revoke", http.MethodDelete, "/api/invites/" + id},
	}
	for _, c := range cases {
		t.Run(c.name+" unauthenticated", func(t *testing.T) {
			if rec := call(s, c.method, c.target, ""); rec.Code != http.StatusUnauthorized {
				t.Errorf("unauthenticated %s: status %d, want 401", c.target, rec.Code)
			}
		})
		t.Run(c.name+" non-admin", func(t *testing.T) {
			if rec := call(s, c.method, c.target, "", friend); rec.Code != http.StatusForbidden {
				t.Errorf("non-admin %s: status %d, want 403", c.target, rec.Code)
			}
		})
	}
}

func TestInviteCreateListRevokeLifecycle(t *testing.T) {
	s, _, _ := newGatedServer(t)
	admin := adminSession(t, s)

	inv := createInvite(t, s, admin, "for Alice")
	if inv["code"] == "" || inv["url"] == "" {
		t.Errorf("created invite must return a one-time code and url: %+v", inv)
	}
	if inv["status"] != "pending" {
		t.Errorf("new invite status = %v, want pending", inv["status"])
	}

	// The list shows the invite but never its raw code.
	rec := call(s, http.MethodGet, "/api/invites", "", admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status %d, body %s", rec.Code, rec.Body)
	}
	var list map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list body: %v", err)
	}
	items, _ := list["invites"].([]any)
	if len(items) != 1 {
		t.Fatalf("list has %d invites, want 1", len(items))
	}
	first := items[0].(map[string]any)
	if c, present := first["code"]; present && c != "" {
		t.Errorf("the invite list returned the raw code (%v); only creation should", c)
	}
	if first["status"] != "pending" {
		t.Errorf("listed status = %v, want pending", first["status"])
	}
	id, _ := first["id"].(string)
	if id == "" {
		t.Fatal("listed invite has no id")
	}

	// Revoke it.
	if rec := call(s, http.MethodDelete, "/api/invites/"+id, "", admin); rec.Code != http.StatusNoContent {
		t.Errorf("revoke: status %d, want 204", rec.Code)
	}
	rec = call(s, http.MethodGet, "/api/invites", "", admin)
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if items, _ = list["invites"].([]any); len(items) != 0 {
		t.Errorf("after revoke, list has %d invites, want none", len(items))
	}
}

// TestRegisterIsOpenWithoutASession guards the one detail that lets a signed-out
// friend follow an invite link: /api/auth/register is on the open path list.
func TestRegisterIsOpenWithoutASession(t *testing.T) {
	s, _, _ := newGatedServer(t)
	// An unauthenticated POST that fails validation (no invite) should still
	// reach the handler — a 400, not the gate's 401.
	rec := call(s, http.MethodPost, "/api/auth/register", registerJSON("x", "y", ""))
	if rec.Code == http.StatusUnauthorized {
		t.Error("register is behind the session gate; an invitee could not sign up")
	}
}
