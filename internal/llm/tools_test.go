package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeFetcher records what the model asked for and answers with fixed text.
type fakeFetcher struct {
	mu       sync.Mutex
	looked   []string
	searched []string
	err      error
}

func (f *fakeFetcher) LookupRule(_ context.Context, number string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.looked = append(f.looked, number)
	if f.err != nil {
		return "", f.err
	}
	return "### " + number + "\nIf all its targets are illegal, the spell doesn't resolve.", nil
}

func (f *fakeFetcher) SearchRules(_ context.Context, query string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searched = append(f.searched, query)
	if f.err != nil {
		return "", f.err
	}
	return "### 608.2b\nTargets are rechecked on resolution.", nil
}

// toolThenAnswer serves a two-round exchange: the first response asks for a
// rule lookup, the second answers. It also captures every request body so a
// test can assert the tool result was actually sent back.
func toolThenAnswer(t *testing.T, bodies *[]map[string]any) http.HandlerFunc {
	t.Helper()
	var mu sync.Mutex
	round := 0
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		*bodies = append(*bodies, body)
		round++
		n := round
		mu.Unlock()

		w.Header().Set("content-type", "application/json")
		if n == 1 {
			fmt.Fprint(w, `{"stop_reason":"tool_use","content":[
				{"type":"text","text":"The linchpin rule is not in the excerpts; let me fetch it."},
				{"type":"tool_use","id":"tu_1","name":"lookup_rule","input":{"number":"608.2b"}}
			],"usage":{"input_tokens":10,"output_tokens":5}}`)
			return
		}
		fmt.Fprint(w, `{"stop_reason":"end_turn","content":[
			{"type":"text","text":"Rule 608.2b decides it: the spell does not resolve."}
		],"usage":{"input_tokens":20,"output_tokens":9}}`)
	}
}

// Before this, a model that noticed the deciding rule was missing could only
// say so in prose — the answer named the gap instead of closing it. With a
// fetcher attached it must go and get the rule, and the answer must be built
// from both rounds.
func TestAnswerFetchesAMissingRule(t *testing.T) {
	var bodies []map[string]any
	up := httptest.NewServer(toolThenAnswer(t, &bodies))
	defer up.Close()

	f := &fakeFetcher{}
	c := New(Config{BaseURL: up.URL, APIKey: "k", Model: "m"})
	out, err := c.Answer(context.Background(), Request{
		CorpusName: "Magic: The Gathering",
		Question:   "Does equipping hexproof in response stop the spell?",
		Fetcher:    f,
	})
	if err != nil {
		t.Fatalf("answer: %v", err)
	}

	if len(f.looked) != 1 || f.looked[0] != "608.2b" {
		t.Fatalf("looked up %v, want [608.2b]", f.looked)
	}
	for _, want := range []string{"let me fetch it", "608.2b decides it"} {
		if !strings.Contains(out, want) {
			t.Errorf("answer lost a round: %q missing from %q", want, out)
		}
	}
	if len(bodies) != 2 {
		t.Fatalf("made %d calls, want 2", len(bodies))
	}
	// The first call must offer the tools; the second must carry the result.
	if _, ok := bodies[0]["tools"]; !ok {
		t.Error("first request offered no tools")
	}
	if !strings.Contains(fmt.Sprint(bodies[1]["messages"]), "targets are illegal") {
		t.Errorf("the fetched rule never reached the model: %v", bodies[1]["messages"])
	}
}

// Without a fetcher the exchange must stay exactly as it was: one round, no
// tools on the wire.
func TestAnswerWithoutFetcherOffersNoTools(t *testing.T) {
	var bodies []map[string]any
	up := httptest.NewServer(toolThenAnswer(t, &bodies))
	defer up.Close()

	c := New(Config{BaseURL: up.URL, APIKey: "k", Model: "m"})
	if _, err := c.Answer(context.Background(), Request{CorpusName: "Magic: The Gathering", Question: "q"}); err != nil {
		t.Fatalf("answer: %v", err)
	}
	if len(bodies) != 1 {
		t.Fatalf("made %d calls, want 1", len(bodies))
	}
	if _, ok := bodies[0]["tools"]; ok {
		t.Error("tools were offered with no fetcher configured")
	}
}

// A lookup that fails must come back to the model as an error result it can
// report, never as a dropped exchange — an answer that says "I couldn't look
// that up" is useful; no answer at all is not.
func TestFailedLookupStillAnswers(t *testing.T) {
	var bodies []map[string]any
	up := httptest.NewServer(toolThenAnswer(t, &bodies))
	defer up.Close()

	c := New(Config{BaseURL: up.URL, APIKey: "k", Model: "m"})
	out, err := c.Answer(context.Background(), Request{
		CorpusName: "Magic: The Gathering", Question: "q",
		Fetcher: &fakeFetcher{err: fmt.Errorf("index offline")},
	})
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if !strings.Contains(out, "608.2b decides it") {
		t.Errorf("answer did not survive a failed lookup: %q", out)
	}
	if !strings.Contains(fmt.Sprint(bodies[1]["messages"]), "index offline") {
		t.Errorf("the failure was not reported to the model: %v", bodies[1]["messages"])
	}
}

// The model announces a lookup so the UI can say what is being consulted;
// without it a mid-answer fetch is silence on the wire.
func TestOnLookupIsAnnounced(t *testing.T) {
	var bodies []map[string]any
	up := httptest.NewServer(toolThenAnswer(t, &bodies))
	defer up.Close()

	var gotTool, gotArg string
	c := New(Config{BaseURL: up.URL, APIKey: "k", Model: "m"})
	_, err := c.Answer(context.Background(), Request{
		CorpusName: "Magic: The Gathering", Question: "q", Fetcher: &fakeFetcher{},
		OnLookup: func(tool, arg string) { gotTool, gotArg = tool, arg },
	})
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if gotTool != "lookup_rule" || gotArg != "608.2b" {
		t.Errorf("OnLookup(%q, %q), want (lookup_rule, 608.2b)", gotTool, gotArg)
	}
}

// A streamed answer must be able to do both at once: forward text to the
// reader as it arrives and reassemble the tool call whose arguments trickle in
// as partial JSON across several events.
func TestReadStreamReassemblesToolCalls(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Checking "}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"the rule."}}`,
		``,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_9","name":"lookup_rule"}}`,
		``,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"num"}}`,
		``,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"ber\":\"608.2b\"}"}}`,
		``,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		``,
	}, "\n")

	var streamed strings.Builder
	ex, err := readStream(strings.NewReader(body), func(s string) error {
		streamed.WriteString(s)
		return nil
	})
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if streamed.String() != "Checking the rule." {
		t.Errorf("streamed %q", streamed.String())
	}
	if ex.StopReason != "tool_use" {
		t.Errorf("stop reason %q", ex.StopReason)
	}
	if len(ex.Tools) != 1 || ex.Tools[0].Name != "lookup_rule" {
		t.Fatalf("tools = %+v", ex.Tools)
	}
	if got := string(ex.Tools[0].Input); got != `{"number":"608.2b"}` {
		t.Errorf("tool input = %s", got)
	}
	// The partial-JSON fragments must not have leaked into the answer text.
	if strings.Contains(ex.Text, "608.2b") {
		t.Errorf("tool arguments leaked into the answer: %q", ex.Text)
	}
}
