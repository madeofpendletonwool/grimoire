package server

// The handout surface (MAD-490): the acceptance walk. A draft is
// invisible to a player by construction — the route cannot produce it
// because the PlayerView it reads through cannot — publish hands it to
// the party, retire takes it back while the DM keeps the history, and the
// image path walks the same line before any byte is served. The writes
// are the DM's alone, checked as routes, not as store calls.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/testdb"
)

// newHandoutTestServer boots the campaign layers plus the handout image
// directory, pointed at a temp dir so the tests can see the files.
func newHandoutTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	store, err := index.Open(testdb.Path(t))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	users, err := auth.New(store.DB(), 0, 0)
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}
	campaigns, err := campaign.New(store.DB())
	if err != nil {
		t.Fatalf("open campaign store: %v", err)
	}
	knowledgeStore, err := knowledge.New(store.DB())
	if err != nil {
		t.Fatalf("open knowledge store: %v", err)
	}
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithCampaigns(campaigns, knowledgeStore)
	dir := t.TempDir()
	s = s.WithHandouts(HandoutOptions{Dir: dir})
	return s, dir
}

// uploadHandoutImage PUTs an image the way the UI's file input does.
func uploadHandoutImage(t *testing.T, s *Server, cid, hid, filename string, content []byte, cookie *http.Cookie) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", filename)
	_, _ = fw.Write(content)
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPut,
		fmt.Sprintf("/api/campaigns/%s/handouts/%s/image", cid, hid), &buf)
	req.Header.Set("content-type", mw.FormDataContentType())
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// handoutFrom decodes the {"handout": {...}} envelope.
func handoutFrom(t *testing.T, rec *recorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode handout response: %v (%s)", err, rec.Body)
	}
	h, ok := body["handout"].(map[string]any)
	if !ok {
		t.Fatalf("no handout in response: %s", rec.Body)
	}
	return h
}

// pngFixture is enough of a PNG that sniffing names it one.
var pngFixture = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0x42}, 64)...)

// handoutListFrom decodes a {"handouts": [...]} envelope into fresh maps
// every call — decoding into a reused map merges stale fields, which is
// exactly the kind of noise a visibility test cannot afford.
func handoutListFrom(t *testing.T, rec *recorder) []map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode handout list: %v (%s)", err, rec.Body)
	}
	raw, _ := body["handouts"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, h := range raw {
		m, _ := h.(map[string]any)
		out = append(out, m)
	}
	return out
}

func TestHandoutDraftInvisibleToPlayerByConstruction(t *testing.T) {
	s, _ := newHandoutTestServer(t)
	f := buildFixture(t, s)
	dm := dmSession(t, s)
	player := addPlayerMember(t, s, f, "mira", true)
	base := "/api/campaigns/" + f.campaignID + "/handouts"

	// The DM drafts a letter whose very existence is for the DM alone.
	rec := hit(t, s, http.MethodPost, base,
		`{"kind":"handout","title":"The sealed letter","body":"The Duke is a vampire — do not show the party."}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create handout: status %d, body %s", rec.Code, rec.Body)
	}
	draft := handoutFrom(t, rec)
	if draft["status"] != "draft" {
		t.Fatalf("a fresh handout is a draft, got %v", draft["status"])
	}
	hid, _ := draft["id"].(string)

	// The player cannot see it: not in the list, not by id.
	rec = hit(t, s, http.MethodGet, base, "", player)
	if rec.Code != http.StatusOK {
		t.Fatalf("player list: status %d", rec.Code)
	}
	if hs := handoutListFrom(t, rec); len(hs) != 0 {
		t.Fatalf("LEAK: the player's handout list carries a draft: %v", hs)
	}
	if rec := hit(t, s, http.MethodGet, base+"/"+hid, "", player); rec.Code != http.StatusNotFound {
		t.Fatalf("draft by id at the player's scope: status %d, want 404", rec.Code)
	}
	if rec := hit(t, s, http.MethodGet, base+"/"+hid+"/image", "", player); rec.Code != http.StatusNotFound {
		t.Fatalf("draft image at the player's scope: status %d, want 404", rec.Code)
	}
	// A player scope cannot even ask the status question.
	if rec := hit(t, s, http.MethodGet, base+"?status=draft", "", player); rec.Code != http.StatusBadRequest {
		t.Fatalf("status filter at the player's scope: status %d, want 400", rec.Code)
	}

	// The DM sees their desk.
	rec = hit(t, s, http.MethodGet, base, "", dm)
	if hs := handoutListFrom(t, rec); len(hs) != 1 {
		t.Fatalf("dm list = %v; want the draft", hs)
	}

	// Publish: the party reads it.
	if rec := hit(t, s, http.MethodPost, base+"/"+hid+"/publish", "", dm); rec.Code != http.StatusOK {
		t.Fatalf("publish: status %d, body %s", rec.Code, rec.Body)
	}
	rec = hit(t, s, http.MethodGet, base, "", player)
	hs := handoutListFrom(t, rec)
	if len(hs) != 1 {
		t.Fatalf("player list after publish = %v", hs)
	}
	if hs[0]["title"] != "The sealed letter" || hs[0]["status"] != "published" || hs[0]["published_at"] == "" {
		t.Fatalf("published view = %v", hs[0])
	}
	if rec := hit(t, s, http.MethodGet, base+"/"+hid, "", player); rec.Code != http.StatusOK {
		t.Fatalf("published by id at the player's scope: status %d", rec.Code)
	}

	// Retire: off the party's list, kept in the DM's history.
	if rec := hit(t, s, http.MethodPost, base+"/"+hid+"/retire", "", dm); rec.Code != http.StatusOK {
		t.Fatalf("retire: status %d", rec.Code)
	}
	rec = hit(t, s, http.MethodGet, base, "", player)
	if hs := handoutListFrom(t, rec); len(hs) != 0 {
		t.Fatalf("LEAK: a retired handout is still on the player's list: %v", hs)
	}
	if rec := hit(t, s, http.MethodGet, base+"/"+hid, "", player); rec.Code != http.StatusNotFound {
		t.Fatalf("retired by id at the player's scope: status %d, want 404", rec.Code)
	}
	rec = hit(t, s, http.MethodGet, base, "", dm)
	dmList := handoutListFrom(t, rec)
	if len(dmList) != 1 {
		t.Fatalf("the dm keeps the history: %v", dmList)
	}
	if dmList[0]["status"] != "retired" {
		t.Fatalf("dm sees the retired row as %v", dmList[0]["status"])
	}

	// Unpublish: published returns to draft and the player's read loses
	// it — what the party holds they hold; the portal stops offering it.
	rec = hit(t, s, http.MethodPost, base, `{"kind":"handout","title":"Second letter","body":"Simpler times."}`, dm)
	hid2 := handoutFrom(t, rec)["id"].(string)
	hit(t, s, http.MethodPost, base+"/"+hid2+"/publish", "", dm)
	if rec := hit(t, s, http.MethodPost, base+"/"+hid2+"/unpublish", "", dm); rec.Code != http.StatusOK {
		t.Fatalf("unpublish: status %d", rec.Code)
	}
	if rec := hit(t, s, http.MethodGet, base+"/"+hid2, "", player); rec.Code != http.StatusNotFound {
		t.Fatalf("unpublished by id at the player's scope: status %d, want 404", rec.Code)
	}
}

func TestHandoutWritesAreTheDMs(t *testing.T) {
	s, _ := newHandoutTestServer(t)
	f := buildFixture(t, s)
	dm := dmSession(t, s)
	player := addPlayerMember(t, s, f, "thalia", false)
	base := "/api/campaigns/" + f.campaignID + "/handouts"

	rec := hit(t, s, http.MethodPost, base, `{"kind":"handout","title":"X","body":"Y"}`, dm)
	hid := handoutFrom(t, rec)["id"].(string)

	for _, tc := range []struct {
		method, target string
	}{
		{http.MethodPost, base},
		{http.MethodPatch, base + "/" + hid},
		{http.MethodDelete, base + "/" + hid},
		{http.MethodPost, base + "/" + hid + "/publish"},
		{http.MethodPost, base + "/" + hid + "/unpublish"},
		{http.MethodPost, base + "/" + hid + "/retire"},
	} {
		if rec := hit(t, s, tc.method, tc.target, `{}`, player); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s at the player's scope: status %d, want 403", tc.method, tc.target, rec.Code)
		}
	}

	// A caller with no standing learns nothing: the same 404 a wrong
	// campaign id produces.
	outsider := registerAndLogin(t, s, "wanderer")
	for _, target := range []string{base, base + "/" + hid} {
		if rec := hit(t, s, http.MethodGet, target, "", outsider); rec.Code != http.StatusNotFound {
			t.Errorf("outsider read %s: status %d, want 404", target, rec.Code)
		}
	}

	// Shape validation reaches the route: a blank title is a 400, an
	// unknown kind is a 400, an empty body is fine for a draft.
	if rec := hit(t, s, http.MethodPost, base, `{"kind":"handout","title":"  "}`, dm); rec.Code != http.StatusBadRequest {
		t.Errorf("blank title: status %d, want 400", rec.Code)
	}
	if rec := hit(t, s, http.MethodPost, base, `{"kind":"prophecy","title":"X"}`, dm); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown kind: status %d, want 400", rec.Code)
	}

	// The DM edits, publishes, and the edit reaches the row the reader
	// sees.
	if rec := hit(t, s, http.MethodPatch, base+"/"+hid, `{"title":"The edited letter"}`, dm); rec.Code != http.StatusOK {
		t.Fatalf("patch: status %d", rec.Code)
	}
	if rec := hit(t, s, http.MethodPost, base+"/"+hid+"/publish", "", dm); rec.Code != http.StatusOK {
		t.Fatalf("publish: status %d", rec.Code)
	}
	rec = hit(t, s, http.MethodGet, base+"/"+hid, "", player)
	if rec.Code != http.StatusOK {
		t.Fatalf("player read after edit+publish: status %d", rec.Code)
	}
	if h := handoutFrom(t, rec); h["title"] != "The edited letter" {
		t.Fatalf("edited title = %v", h["title"])
	}
}

func TestHandoutImageUploadAndServe(t *testing.T) {
	s, dir := newHandoutTestServer(t)
	f := buildFixture(t, s)
	dm := dmSession(t, s)
	player := addPlayerMember(t, s, f, "mira", true)
	base := "/api/campaigns/" + f.campaignID + "/handouts"

	rec := hit(t, s, http.MethodPost, base, `{"kind":"map","title":"The valley"}`, dm)
	hid := handoutFrom(t, rec)["id"].(string)

	// A map cannot be published without its image.
	if rec := hit(t, s, http.MethodPost, base+"/"+hid+"/publish", "", dm); rec.Code != http.StatusBadRequest {
		t.Fatalf("publish an imageless map: status %d, want 400", rec.Code)
	}

	// A non-image is refused before anything is stored.
	if code, _ := uploadHandoutImage(t, s, f.campaignID, hid, "notes.txt", []byte("just text"), dm); code != http.StatusUnsupportedMediaType {
		t.Fatalf("txt upload: status %d, want 415", code)
	}

	// The real upload: sniffed, stored beside the database, referenced by
	// a generated name, served at the read's own scope.
	code, body := uploadHandoutImage(t, s, f.campaignID, hid, "valley.png", pngFixture, dm)
	if code != http.StatusOK {
		t.Fatalf("png upload: status %d body %v", code, body)
	}
	uploaded, _ := body["handout"].(map[string]any)
	imageURL, _ := uploaded["image_url"].(string)
	if imageURL == "" || uploaded["image_mime"] != "image/png" {
		t.Fatalf("uploaded view = %v", uploaded)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 1 || filepath.Ext(files[0].Name()) != ".png" {
		t.Fatalf("handout dir = %v; want one png", files)
	}

	// A draft's image is a 404 the player cannot distinguish from any
	// other missing thing; the DM reads it fine.
	if rec := hit(t, s, http.MethodGet, baseURL(imageURL), "", player); rec.Code != http.StatusNotFound {
		t.Fatalf("draft image at the player's scope: status %d, want 404", rec.Code)
	}
	rec = hit(t, s, http.MethodGet, baseURL(imageURL), "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("dm image read: status %d", rec.Code)
	}
	if ct := rec.Header().Get("content-type"); ct != "image/png" {
		t.Fatalf("image content-type = %q", ct)
	}
	if !bytes.Equal(rec.Body.Bytes(), pngFixture) {
		t.Fatalf("image bytes did not round-trip (%d bytes)", rec.Body.Len())
	}

	// Publish, and the player's image read works — same route, wider row.
	hit(t, s, http.MethodPost, base+"/"+hid+"/publish", "", dm)
	if rec := hit(t, s, http.MethodGet, baseURL(imageURL), "", player); rec.Code != http.StatusOK {
		t.Fatalf("published image at the player's scope: status %d", rec.Code)
	}

	// Replacing the image leaves one file behind.
	if code, _ := uploadHandoutImage(t, s, f.campaignID, hid, "valley2.png", pngFixture, dm); code != http.StatusOK {
		t.Fatalf("replace upload: status %d", code)
	}
	files, _ = os.ReadDir(dir)
	if len(files) != 1 {
		t.Fatalf("after replace, handout dir holds %d files; want 1", len(files))
	}

	// Deleting the handout removes the row and the file with it.
	if rec := hit(t, s, http.MethodDelete, base+"/"+hid, "", dm); rec.Code != http.StatusNoContent {
		t.Fatalf("delete handout: status %d", rec.Code)
	}
	files, _ = os.ReadDir(dir)
	if len(files) != 0 {
		t.Fatalf("after delete, handout dir holds %d files; want 0", len(files))
	}
	if rec := hit(t, s, http.MethodGet, baseURL(imageURL), "", dm); rec.Code != http.StatusNotFound {
		t.Fatalf("deleted image: status %d, want 404", rec.Code)
	}
}

// baseURL strips the host-free API path back to something hit() can take
// (the view's image_url is already a path).
func baseURL(u string) string { return u }

// registerAndLogin mints a logged-in account with no campaign standing,
// through the keeper's account-invite flow.
func registerAndLogin(t *testing.T, s *Server, name string) *http.Cookie {
	t.Helper()
	inv := createInvite(t, s, dmSession(t, s), "an outsider with no seat")
	code, _ := inv["code"].(string)
	reg := hit(t, s, http.MethodPost, "/api/auth/register",
		`{"username":`+quote(name)+`,"password":"a-fine-passphrase","invite":`+quote(code)+`}`)
	if reg.Code != http.StatusCreated {
		t.Fatalf("register %s: status %d body %s", name, reg.Code, reg.Body)
	}
	return sessionFrom(t, reg)
}
