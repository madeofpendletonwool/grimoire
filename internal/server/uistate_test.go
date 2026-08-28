package server

// Campaign OS layout surface tests (MAD-366). The load-bearing ones assert on
// HTTP responses: one account cannot read or overwrite another's workspaces,
// and a malformed tree is refused at the edge rather than stored and served
// back to a shell that cannot parse it. The store beneath has its own tests;
// what these prove is that the validation is *reached*.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
	"github.com/madeofpendletonwool/grimoire/internal/uistate"
)

// newUIServer boots a gated server with the layout store wired, and claims the
// install as the keeper so the caller starts signed in.
func newUIServer(t *testing.T) (*Server, *http.Cookie) {
	t.Helper()
	store, err := index.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := migrate.Up(store.DB()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	users, err := auth.New(store.DB(), 0, 0)
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithUIState(uistate.New(store.DB()))

	rec := call(s, http.MethodPost, "/api/auth/setup", credsJSON("keeper", "a-perfectly-fine-passphrase"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup: status %d, body %s", rec.Code, rec.Body)
	}
	return s, sessionFrom(t, rec)
}

// The shape the "At the table" preset produces: chat beside a tabbed pair.
const uiTree = `{"t":"split","dir":"row","fr":[0.6,0.4],"kids":[` +
	`{"t":"leaf","tool":"chat"},` +
	`{"t":"tabs","active":1,"kids":[{"t":"leaf","tool":"sessions"},{"t":"leaf","tool":"encounter"}]}]}`

func saveLayout(t *testing.T, s *Server, who *http.Cookie, corpus string, slot int, name, tree string) *recorder {
	t.Helper()
	body := `{"corpus":` + quote(corpus) + `,"slot":` + itoa(slot) + `,"name":` + quote(name) + `,"tree":` + tree + `}`
	return hit(t, s, http.MethodPut, "/api/ui/layouts", body, who)
}

func layoutsOf(t *testing.T, s *Server, who *http.Cookie, corpus string) []map[string]any {
	t.Helper()
	rec := hit(t, s, http.MethodGet, "/api/ui/layouts?corpus="+corpus, "", who)
	if rec.Code != http.StatusOK {
		t.Fatalf("list layouts: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Layouts []map[string]any `json:"layouts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode layouts: %v (body %s)", err, rec.Body)
	}
	return body.Layouts
}

/* ---------- layouts ---------- */

func TestUILayoutsEmptyIsAListNotNull(t *testing.T) {
	s, keeper := newUIServer(t)
	rec := hit(t, s, http.MethodGet, "/api/ui/layouts?corpus=dnd", "", keeper)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	// The client iterates this without a guard, and JSON null is not a list.
	if !strings.Contains(rec.Body.String(), `"layouts":[]`) {
		t.Errorf("want an empty array, got %s", rec.Body)
	}
}

func TestUILayoutRoundTrip(t *testing.T) {
	s, keeper := newUIServer(t)
	if rec := saveLayout(t, s, keeper, "dnd", 2, "At the table", uiTree); rec.Code != http.StatusOK {
		t.Fatalf("save: status %d, body %s", rec.Code, rec.Body)
	}

	got := layoutsOf(t, s, keeper, "dnd")
	if len(got) != 1 {
		t.Fatalf("want 1 layout, got %d", len(got))
	}
	if got[0]["name"] != "At the table" {
		t.Errorf("name lost: %v", got[0]["name"])
	}
	if got[0]["slot"].(float64) != 2 {
		t.Errorf("slot lost: %v", got[0]["slot"])
	}

	// The tree must come back as a structure, not a re-encoded string — the
	// client feeds it straight to tree.js parse.
	tree, ok := got[0]["tree"].(map[string]any)
	if !ok {
		t.Fatalf("tree came back as %T, want an object", got[0]["tree"])
	}
	if tree["dir"] != "row" {
		t.Errorf("tree shape changed in transit: %v", tree)
	}
}

func TestUILayoutSaveIsIdempotentPerSlot(t *testing.T) {
	s, keeper := newUIServer(t)
	saveLayout(t, s, keeper, "dnd", 1, "Prep", `{"t":"leaf","tool":"planner"}`)
	saveLayout(t, s, keeper, "dnd", 1, "Prep (mine)", `{"t":"leaf","tool":"campaign"}`)

	got := layoutsOf(t, s, keeper, "dnd")
	if len(got) != 1 {
		t.Fatalf("a repeat save created a second row: %d layouts", len(got))
	}
	if got[0]["name"] != "Prep (mine)" {
		t.Errorf("slot not replaced: %v", got[0]["name"])
	}
}

func TestUILayoutsAreScopedPerCorpus(t *testing.T) {
	s, keeper := newUIServer(t)
	saveLayout(t, s, keeper, "dnd", 1, "Prep", `{"t":"leaf","tool":"planner"}`)
	saveLayout(t, s, keeper, "mtg", 1, "Brew", `{"t":"leaf","tool":"deck"}`)

	dnd := layoutsOf(t, s, keeper, "dnd")
	if len(dnd) != 1 || dnd[0]["name"] != "Prep" {
		t.Errorf("D&D slot 1 returned %v", dnd)
	}
	mtg := layoutsOf(t, s, keeper, "mtg")
	if len(mtg) != 1 || mtg[0]["name"] != "Brew" {
		t.Errorf("Magic slot 1 returned %v", mtg)
	}
}

// The authorization case: two accounts share slot numbers, and neither may
// see or overwrite the other's workspace.
func TestUILayoutsDoNotLeakBetweenAccounts(t *testing.T) {
	s, keeper := newUIServer(t)
	saveLayout(t, s, keeper, "dnd", 1, "The keeper's prep", `{"t":"leaf","tool":"planner"}`)

	friend := secondUser(t, s)
	if got := layoutsOf(t, s, friend, "dnd"); len(got) != 0 {
		t.Fatalf("a new account saw someone else's layouts: %v", got)
	}

	if rec := saveLayout(t, s, friend, "dnd", 1, "The friend's prep", `{"t":"leaf","tool":"chat"}`); rec.Code != http.StatusOK {
		t.Fatalf("friend save: status %d, body %s", rec.Code, rec.Body)
	}

	mine := layoutsOf(t, s, keeper, "dnd")
	if len(mine) != 1 || mine[0]["name"] != "The keeper's prep" {
		t.Errorf("the friend's save overwrote the keeper's slot: %v", mine)
	}
	theirs := layoutsOf(t, s, friend, "dnd")
	if len(theirs) != 1 || theirs[0]["name"] != "The friend's prep" {
		t.Errorf("the friend cannot see their own slot: %v", theirs)
	}
}

func TestUIDeleteLayoutClearsOneSlot(t *testing.T) {
	s, keeper := newUIServer(t)
	saveLayout(t, s, keeper, "dnd", 1, "Prep", `{"t":"leaf","tool":"planner"}`)
	saveLayout(t, s, keeper, "dnd", 2, "Table", `{"t":"leaf","tool":"chat"}`)

	if rec := hit(t, s, http.MethodDelete, "/api/ui/layouts/1?corpus=dnd", "", keeper); rec.Code != http.StatusOK {
		t.Fatalf("delete: status %d, body %s", rec.Code, rec.Body)
	}
	got := layoutsOf(t, s, keeper, "dnd")
	if len(got) != 1 || got[0]["slot"].(float64) != 2 {
		t.Errorf("wrong slot cleared: %v", got)
	}
}

func TestUIDeleteLayoutRejectsANonNumericSlot(t *testing.T) {
	s, keeper := newUIServer(t)
	rec := hit(t, s, http.MethodDelete, "/api/ui/layouts/first?corpus=dnd", "", keeper)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400 for a non-numeric slot, got %d (%s)", rec.Code, rec.Body)
	}
}

// Validation lives in the store; this proves the handler reaches it and turns
// the refusal into a 400 rather than a 500 or a silent write.
func TestUISaveLayoutRefusesBadInputWithFourHundred(t *testing.T) {
	s, keeper := newUIServer(t)

	cases := map[string]string{
		"unknown corpus":      `{"corpus":"pokemon","slot":1,"name":"x","tree":{"t":"leaf","tool":"chat"}}`,
		"slot out of range":   `{"corpus":"dnd","slot":99,"name":"x","tree":{"t":"leaf","tool":"chat"}}`,
		"empty name":          `{"corpus":"dnd","slot":1,"name":"","tree":{"t":"leaf","tool":"chat"}}`,
		"missing tree":        `{"corpus":"dnd","slot":1,"name":"x"}`,
		"unknown node kind":   `{"corpus":"dnd","slot":1,"name":"x","tree":{"t":"window","kids":[]}}`,
		"tool with a slash":   `{"corpus":"dnd","slot":1,"name":"x","tree":{"t":"leaf","tool":"../../etc/passwd"}}`,
		"childless split":     `{"corpus":"dnd","slot":1,"name":"x","tree":{"t":"split","dir":"row","kids":[]}}`,
		"bad split dir":       `{"corpus":"dnd","slot":1,"name":"x","tree":{"t":"split","dir":"diagonal","kids":[{"t":"leaf","tool":"chat"},{"t":"leaf","tool":"deck"}]}}`,
		"active out of range": `{"corpus":"dnd","slot":1,"name":"x","tree":{"t":"tabs","active":9,"kids":[{"t":"leaf","tool":"chat"}]}}`,
		"not JSON":            `{"corpus":"dnd",`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rec := hit(t, s, http.MethodPut, "/api/ui/layouts", body, keeper)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d (body %s)", rec.Code, rec.Body)
			}
		})
	}

	if got := layoutsOf(t, s, keeper, "dnd"); len(got) != 0 {
		t.Errorf("a refused layout was stored anyway: %v", got)
	}
}

// An empty workspace is a legal thing to save — the user closed every window
// in that slot and that must survive a reload.
func TestUISaveLayoutAcceptsAnEmptyWorkspace(t *testing.T) {
	s, keeper := newUIServer(t)
	if rec := saveLayout(t, s, keeper, "dnd", 3, "Scratch", `null`); rec.Code != http.StatusOK {
		t.Fatalf("want 200 for an empty workspace, got %d (%s)", rec.Code, rec.Body)
	}
	got := layoutsOf(t, s, keeper, "dnd")
	if len(got) != 1 || got[0]["tree"] != nil {
		t.Errorf("empty workspace did not round trip: %v", got)
	}
}

func TestUILayoutsRejectsAnUnknownCorpusOnRead(t *testing.T) {
	s, keeper := newUIServer(t)
	rec := hit(t, s, http.MethodGet, "/api/ui/layouts?corpus=pokemon", "", keeper)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d (%s)", rec.Code, rec.Body)
	}
}

/* ---------- preferences ---------- */

func TestUIPrefsRoundTripAndMerge(t *testing.T) {
	s, keeper := newUIServer(t)

	if rec := hit(t, s, http.MethodPut, "/api/ui/prefs",
		`{"prefs":{"scene":"cave","corpus":"dnd"}}`, keeper); rec.Code != http.StatusOK {
		t.Fatalf("save prefs: status %d, body %s", rec.Code, rec.Body)
	}
	// Saving one setting must not clear the others.
	if rec := hit(t, s, http.MethodPut, "/api/ui/prefs",
		`{"prefs":{"scene":"snowy"}}`, keeper); rec.Code != http.StatusOK {
		t.Fatalf("merge prefs: status %d, body %s", rec.Code, rec.Body)
	}

	rec := hit(t, s, http.MethodGet, "/api/ui/prefs", "", keeper)
	if rec.Code != http.StatusOK {
		t.Fatalf("read prefs: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Prefs map[string]string `json:"prefs"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Prefs["scene"] != "snowy" {
		t.Errorf("scene not updated: %q", body.Prefs["scene"])
	}
	if body.Prefs["corpus"] != "dnd" {
		t.Errorf("merge dropped an untouched key: %v", body.Prefs)
	}
}

func TestUIPrefsDoNotLeakBetweenAccounts(t *testing.T) {
	s, keeper := newUIServer(t)
	hit(t, s, http.MethodPut, "/api/ui/prefs", `{"prefs":{"scene":"cave"}}`, keeper)

	friend := secondUser(t, s)
	rec := hit(t, s, http.MethodGet, "/api/ui/prefs", "", friend)
	if strings.Contains(rec.Body.String(), "cave") {
		t.Errorf("a new account inherited someone else's preferences: %s", rec.Body)
	}
}

func TestUIPrefsRefusesOversizedInput(t *testing.T) {
	s, keeper := newUIServer(t)
	body := `{"prefs":{"scene":"` + strings.Repeat("x", uistate.MaxPrefVal+10) + `"}}`
	if rec := hit(t, s, http.MethodPut, "/api/ui/prefs", body, keeper); rec.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", rec.Code)
	}
}

/* ---------- not wired ---------- */

func TestUIEndpointsReportUnavailableWithoutTheStore(t *testing.T) {
	store, err := index.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := migrate.Up(store.DB()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	users, err := auth.New(store.DB(), 0, 0)
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	rec := call(s, http.MethodPost, "/api/auth/setup", credsJSON("keeper", "a-perfectly-fine-passphrase"))
	keeper := sessionFrom(t, rec)

	for _, c := range []struct{ method, target, body string }{
		{http.MethodGet, "/api/ui/layouts?corpus=dnd", ""},
		{http.MethodPut, "/api/ui/layouts", `{"corpus":"dnd","slot":1,"name":"x","tree":{"t":"leaf","tool":"chat"}}`},
		{http.MethodDelete, "/api/ui/layouts/1?corpus=dnd", ""},
		{http.MethodGet, "/api/ui/prefs", ""},
		{http.MethodPut, "/api/ui/prefs", `{"prefs":{}}`},
	} {
		rec := hit(t, s, c.method, c.target, c.body, keeper)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s: want 503, got %d", c.method, c.target, rec.Code)
		}
	}
}
