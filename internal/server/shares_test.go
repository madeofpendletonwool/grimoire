package server

import (
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
	"github.com/madeofpendletonwool/grimoire/internal/share"
)

// newShareServer builds a server with chat history, accounts, and shared
// links enabled, with the first keeper signed in (see sessions in
// chats_test.go). A second user is available via secondUser when a test needs
// a stranger.
func newShareServer(t *testing.T) *Server {
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
	users, err := auth.New(store.DB(), 0, 0)
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}
	shares, err := share.New(store.DB())
	if err != nil {
		t.Fatalf("open share store: %v", err)
	}

	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, chats, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithShares(shares)
	signIn(t, s, "keeper", "a-perfectly-fine-passphrase")
	return s
}

// secondUser registers a friend via an admin invite and returns their cookie,
// so authorization tests can act as someone else.
func secondUser(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	admin := adminSession2(t, s)
	inv := createInvite(t, s, admin, "")
	code, _ := inv["code"].(string)
	rec := call(s, http.MethodPost, "/api/auth/register", registerJSON("friend", "a-fine-passphrase", code))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register friend: status %d, body %s", rec.Code, rec.Body)
	}
	return sessionFrom(t, rec)
}

// adminSession2 signs the already-created keeper back in for an admin action.
func adminSession2(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	rec := call(s, http.MethodPost, "/api/auth/login", credsJSON("keeper", "a-perfectly-fine-passphrase"))
	if rec.Code != http.StatusOK {
		t.Fatalf("login keeper: status %d, body %s", rec.Code, rec.Body)
	}
	return sessionFrom(t, rec)
}

// addTurn stores a Q&A pair directly in the chat store and returns the
// assistant message id. The HTTP streaming path is covered by chats_test;
// shares only need stored rows.
func addTurn(t *testing.T, s *Server, chatID, question, answer string) int64 {
	t.Helper()
	if _, err := s.chats.AddMessage(t.Context(), chatID, chat.RoleUser, question, nil, nil, nil, nil); err != nil {
		t.Fatalf("add question: %v", err)
	}
	m, err := s.chats.AddMessage(t.Context(), chatID, chat.RoleAssistant, answer,
		json.RawMessage(`[{"number":"702.2a","title":"Deathtouch","body":"Deathtouch is a static ability.","source":"MTG Comp Rules"}]`),
		json.RawMessage(`[{"name":"Deicide"}]`), nil,
		json.RawMessage(`[{"card_name":"Deicide","source":"wotc","published_at":"2014-04-15","comment":"Deicide exiles the card."}]`))
	if err != nil {
		t.Fatalf("add answer: %v", err)
	}
	return m.ID
}

func createShare(t *testing.T, s *Server, chatID string, messageID int64) (int, string) {
	t.Helper()
	code, body := do(t, s, http.MethodPost,
		fmt.Sprintf("/api/chats/%s/messages/%d/share", chatID, messageID), "")
	url, _ := body["url"].(string)
	return code, url
}

func TestShareMessageCreatesPublicLink(t *testing.T) {
	s := newShareServer(t)
	id := createChat(t, s, "mtg")
	msgID := addTurn(t, s, id, "Does Deicide exile a god?", "Yes — the second ability exiles it.")

	code, url := createShare(t, s, id, msgID)
	if code != http.StatusCreated {
		t.Fatalf("share: status %d, body %v", code, url)
	}
	if !strings.HasPrefix(url, "/s/") || len(url) < len("/s/")+22 {
		t.Fatalf("url = %q, want /s/<22+ char token>", url)
	}

	// The page renders with no session at all: question, answer, citations.
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("public page: status %d, body %s", rec.Code, rec.Body)
	}
	page := rec.Body.String()
	for _, want := range []string{"Does Deicide exile a god?", "Yes — the second ability exiles it.", "702.2a", "Deicide", "noindex", "Answered by a self-hosted Grimoire instance"} {
		if !strings.Contains(page, want) {
			t.Errorf("public page missing %q", want)
		}
	}
	if strings.Contains(page, "friend") {
		// no user detail should leak onto the public page
		t.Errorf("public page leaked account detail")
	}
}

func TestSharePageUnknownAndMalformedTokens(t *testing.T) {
	s := newShareServer(t)
	for _, tok := range []string{"nosuchtoken0000000000000", "short", "%2e%2e%2ftraversal", strings.Repeat("x", 300) + "!!"} {
		req := httptest.NewRequest(http.MethodGet, "/s/"+tok, nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("token %q: status %d, want 404", tok, rec.Code)
		}
	}
}

func TestShareSnapshotSurvivesChatDeletion(t *testing.T) {
	s := newShareServer(t)
	id := createChat(t, s, "mtg")
	msgID := addTurn(t, s, id, "Does Deicide exile a god?", "Yes — exiles it.")
	_, url := createShare(t, s, id, msgID)

	if code, _ := do(t, s, http.MethodDelete, "/api/chats/"+id, ""); code != http.StatusNoContent {
		t.Fatalf("delete chat: status %d", code)
	}

	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("public page after chat deletion: status %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Does Deicide exile a god?") {
		t.Error("snapshot lost the question after chat deletion")
	}
}

func TestShareRevokeReturnsGone(t *testing.T) {
	s := newShareServer(t)
	id := createChat(t, s, "mtg")
	msgID := addTurn(t, s, id, "Does Deicide exile a god?", "Yes.")
	_, url := createShare(t, s, id, msgID)
	token := strings.TrimPrefix(url, "/s/")

	if code, _ := do(t, s, http.MethodDelete, "/api/shares/"+token, ""); code != http.StatusNoContent {
		t.Fatalf("revoke: status %d", code)
	}

	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusGone {
		t.Errorf("revoked page: status %d, want 410", rec.Code)
	}
}

func TestShareAuthorization(t *testing.T) {
	s := newShareServer(t)
	stranger := secondUser(t, s)

	id := createChat(t, s, "mtg")
	msgID := addTurn(t, s, id, "Does Deicide exile a god?", "Yes.")
	_, url := createShare(t, s, id, msgID)
	token := strings.TrimPrefix(url, "/s/")

	// A stranger cannot share out of someone else's conversation — and the
	// 404 is indistinguishable from an unknown chat.
	target := fmt.Sprintf("/api/chats/%s/messages/%d/share", id, msgID)
	req := httptest.NewRequest(http.MethodPost, target, nil)
	req.AddCookie(stranger)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("stranger share: status %d, want 404", rec.Code)
	}

	// A stranger cannot revoke the owner's link either.
	req = httptest.NewRequest(http.MethodDelete, "/api/shares/"+token, nil)
	req.AddCookie(stranger)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("stranger revoke: status %d, want 404", rec.Code)
	}

	// The link still stands.
	req = httptest.NewRequest(http.MethodGet, url, nil)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("page after stranger's attempts: status %d, want 200", rec.Code)
	}

	// Unauthenticated callers cannot use the API at all.
	req = httptest.NewRequest(http.MethodPost, target, nil)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("signed-out share: status %d, want 401", rec.Code)
	}
}

func TestShareOnlyAnswers(t *testing.T) {
	s := newShareServer(t)
	id := createChat(t, s, "mtg")
	if _, err := s.chats.AddMessage(t.Context(), id, chat.RoleUser, "a question", nil, nil, nil, nil); err != nil {
		t.Fatalf("add question: %v", err)
	}
	code, _ := createShare(t, s, id, 1)
	if code != http.StatusNotFound {
		t.Errorf("sharing a user message: status %d, want 404", code)
	}

	code, _ = createShare(t, s, id, 999)
	if code != http.StatusNotFound {
		t.Errorf("sharing an unknown message: status %d, want 404", code)
	}
}

func TestShareListAndDuplicateShares(t *testing.T) {
	s := newShareServer(t)
	id := createChat(t, s, "mtg")
	msgID := addTurn(t, s, id, "Does Deicide exile a god?", "Yes.")

	// Sharing the same message twice mints two links, no panic.
	_, first := createShare(t, s, id, msgID)
	_, second := createShare(t, s, id, msgID)
	if first == second {
		t.Fatal("sharing twice returned the same link")
	}

	code, body := do(t, s, http.MethodGet, "/api/shares", "")
	if code != http.StatusOK {
		t.Fatalf("list: status %d", code)
	}
	list, _ := body["shares"].([]any)
	if len(list) != 2 {
		t.Fatalf("list = %d shares, want 2", len(list))
	}
	entry, _ := list[0].(map[string]any)
	if entry["question"] != "Does Deicide exile a god?" {
		t.Errorf("entry question = %v", entry["question"])
	}
	if u, _ := entry["url"].(string); !strings.HasPrefix(u, "http") {
		t.Errorf("entry url = %q, want absolute", u)
	}
}

func TestSharesDisabledWithoutStore(t *testing.T) {
	store, err := index.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if code, _ := do(t, s, http.MethodGet, "/api/shares", ""); code != http.StatusServiceUnavailable {
		t.Errorf("list: status %d, want 503", code)
	}
	if code, _ := do(t, s, http.MethodPost, "/api/chats/x/messages/1/share", ""); code != http.StatusServiceUnavailable {
		t.Errorf("share: status %d, want 503", code)
	}
	req := httptest.NewRequest(http.MethodGet, "/s/anything0000000000000000", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("page: status %d, want 404", rec.Code)
	}
}
