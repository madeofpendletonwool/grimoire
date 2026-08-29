package campaign

import (
	"reflect"
	"testing"
)

func TestPlaceOfDecodesTheBlock(t *testing.T) {
	e := &Entity{
		Kind: KindLocation,
		Payload: map[string]any{
			"place": map[string]any{
				"kind":          "town",
				"scale":         "large village",
				"population":    "about 900, mostly human",
				"government":    "a merchant council",
				"services":      []any{"inn", "market", "temple"},
				"defences":      "a palisade and a watch of twelve",
				"climate":       "temperate",
				"senses":        []any{"gull noise", "damp wool"},
				"state":         "flooding after the rains",
				"danger":        2,
				"private_truth": "the root's tendrils reach the well",
			},
			"travel": map[string]any{"routes": []any{map[string]any{"to": "x", "days": 2}}},
		},
	}
	p := PlaceOf(e)
	want := Place{
		Kind: "town", Scale: "large village", Population: "about 900, mostly human",
		Government: "a merchant council", Services: []string{"inn", "market", "temple"},
		Defences: "a palisade and a watch of twelve", Climate: "temperate",
		Senses: []string{"gull noise", "damp wool"}, State: "flooding after the rains",
		Danger: 2, PrivateTruth: "the root's tendrils reach the well",
	}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("decode: got %+v, want %+v", p, want)
	}
	// The travel block survives beside the place block untouched.
	if RoutesOf(e) == nil {
		t.Fatal("the travel block must survive beside the place block")
	}
}

func TestPlaceOfYieldsTheZeroBlock(t *testing.T) {
	cases := map[string]*Entity{
		"nil entity":         nil,
		"not a location":     {Kind: KindNPC, Payload: map[string]any{"place": map[string]any{"kind": "town"}}},
		"no payload":         {Kind: KindLocation},
		"no block":           {Kind: KindLocation, Payload: map[string]any{"travel": map[string]any{}}},
		"malformed block":    {Kind: KindLocation, Payload: map[string]any{"place": "nonsense"}},
		"wrong-shaped block": {Kind: KindLocation, Payload: map[string]any{"place": map[string]any{"kind": 7, "danger": "high"}}},
	}
	for name, e := range cases {
		if got := PlaceOf(e); !reflect.DeepEqual(got, Place{}) {
			t.Fatalf("%s: got %+v, want the zero Place", name, got)
		}
	}
}

func TestWithPlaceRoundTripsAndPreservesNeighbours(t *testing.T) {
	orig := map[string]any{
		"travel": map[string]any{"routes": []any{map[string]any{"to": "monastery", "days": float64(1)}}},
		"notes":  "damp eastern edge",
	}
	p := Place{
		Kind: "town", Scale: "large village", Climate: "temperate",
		Services: []string{"inn", "", "market"}, Danger: 2,
		PrivateTruth: "  the root's   tendrils  ",
	}
	out := WithPlace(orig, p)
	if _, ok := out["place"]; !ok {
		t.Fatal("the place block must be written")
	}
	if _, ok := out["travel"]; !ok {
		t.Fatal("the travel block must be preserved")
	}
	if out["notes"] != "damp eastern edge" {
		t.Fatal("unrelated keys must be preserved")
	}
	got := PlaceOf(&Entity{Kind: KindLocation, Payload: out})
	if got.Kind != "town" || got.Climate != "temperate" || got.Danger != 2 {
		t.Fatalf("round trip lost fields: %+v", got)
	}
	if len(got.Services) != 2 { // the empty entry is dropped, order kept
		t.Fatalf("services: %+v", got.Services)
	}
	if got.PrivateTruth != "the root's tendrils" {
		t.Fatalf("private truth is cleaned, not mangled: %q", got.PrivateTruth)
	}
}

func TestClimateOfPrefersTheBlock(t *testing.T) {
	block := &Entity{Kind: KindLocation, Payload: map[string]any{
		"place":   map[string]any{"climate": "arctic"},
		"climate": "temperate",
	}}
	if got := ClimateOf(block); got != "arctic" {
		t.Fatalf("the place block's climate wins: %q", got)
	}
	bare := &Entity{Kind: KindLocation, Payload: map[string]any{"climate": "arctic"}}
	if got := ClimateOf(bare); got != "arctic" {
		t.Fatalf("the bare top-level tag still reads: %q", got)
	}
	if got := ClimateOf(&Entity{Kind: KindLocation}); got != "" {
		t.Fatalf("no tag anywhere: %q", got)
	}
}
