package canon

import (
	"strings"
	"testing"
)

func TestParseWire_PlainJSON(t *testing.T) {
	in := `{"facts":[{"local_id":"f1","statement":"The Duke owns the mines.","subject":"duke","predicate":"owns","object_entity":"mines","quote":"the Duke owns the mines","confidence":0.9}]}`
	out, problems := parseWire(in)
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if len(out.Facts) != 1 || out.Facts[0].LocalID != "f1" {
		t.Fatalf("facts: %+v", out.Facts)
	}
	if out.Facts[0].Confidence != 0.9 {
		t.Errorf("confidence = %v", out.Facts[0].Confidence)
	}
}

func TestParseWire_FencedJSON(t *testing.T) {
	in := "```json\n{\"facts\":[{\"local_id\":\"f1\",\"statement\":\"s\",\"subject\":\"duke\",\"predicate\":\"owns\",\"object_literal\":\"x\",\"quote\":\"q\",\"confidence\":1}]}\n```"
	out, problems := parseWire(in)
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if len(out.Facts) != 1 {
		t.Fatalf("facts: %+v", out.Facts)
	}
}

func TestParseWire_ProseWrapped(t *testing.T) {
	in := "Here is the extraction you asked for:\n\n{\"events\":[{\"local_id\":\"e1\",\"summary\":\"the ambush\",\"quote\":\"q\",\"confidence\":\"0.8\"}]}\n\nHope that helps!"
	out, problems := parseWire(in)
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if len(out.Events) != 1 {
		t.Fatalf("events: %+v", out.Events)
	}
	if out.Events[0].Confidence != 0.8 {
		t.Errorf("stringified confidence not tolerated: %v", out.Events[0].Confidence)
	}
}

func TestParseWire_NullAndAbsentLists(t *testing.T) {
	out, problems := parseWire(`{"facts": null, "new_entities": null}`)
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if out.Facts != nil || out.NewEntities != nil || out.Events != nil {
		t.Fatalf("null lists must read as empty: %+v", out)
	}
}

func TestParseWire_SalvagesValidSiblings(t *testing.T) {
	// The middle record carries a non-numeric confidence; its two siblings
	// must survive it.
	in := `{"facts":[
		{"local_id":"f1","statement":"one","subject":"duke","predicate":"owns","object_literal":"x","quote":"q1","confidence":0.9},
		{"local_id":"f2","statement":"two","subject":"duke","predicate":"owns","object_literal":"y","quote":"q2","confidence":"high"},
		{"local_id":"f3","statement":"three","subject":"duke","predicate":"owns","object_literal":"z","quote":"q3","confidence":0.7}
	]}`
	out, problems := parseWire(in)
	if len(out.Facts) != 2 {
		t.Fatalf("valid siblings must survive: %+v", out.Facts)
	}
	if out.Facts[0].LocalID != "f1" || out.Facts[1].LocalID != "f3" {
		t.Fatalf("wrong survivors: %+v", out.Facts)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "facts[1]") {
		t.Fatalf("problems: %v", problems)
	}
}

func TestParseWire_SalvagesWhenAListIsNotAList(t *testing.T) {
	in := `{"facts": {"oops": true}, "events": [{"local_id":"e1","summary":"s","quote":"q","confidence":0.5}]}`
	out, problems := parseWire(in)
	if len(out.Events) != 1 {
		t.Fatalf("events lost to a sibling field's shape: %+v", out)
	}
	if len(problems) == 0 || !strings.Contains(problems[0], "facts") {
		t.Fatalf("problems: %v", problems)
	}
}

func TestParseWire_MalformedResponse(t *testing.T) {
	out, problems := parseWire("I could not produce JSON, sorry.")
	if len(problems) == 0 {
		t.Fatal("a response with no JSON object must be reported")
	}
	if out.Facts != nil || out.Events != nil {
		t.Fatalf("no candidates from prose: %+v", out)
	}
	out, problems = parseWire(`{"facts": [}`)
	if len(problems) == 0 {
		t.Fatal("truncated JSON must be reported")
	}
	if len(out.Facts) != 0 {
		t.Fatalf("no candidates from truncated JSON: %+v", out)
	}
}

func TestJSONBlock_InfoStringFence(t *testing.T) {
	got, err := jsonBlock("```json\n{\"a\":1}\n```")
	if err != nil {
		t.Fatalf("jsonBlock: %v", err)
	}
	if got != `{"a":1}` {
		t.Fatalf("got %q", got)
	}
	if _, err := jsonBlock("no braces at all"); err == nil {
		t.Fatal("expected an error for prose with no object")
	}
}
