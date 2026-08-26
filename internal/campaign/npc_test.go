package campaign

import (
	"reflect"
	"testing"
)

func TestNPCAgentOfDecodesAndCleans(t *testing.T) {
	e := &Entity{Kind: KindNPC, Payload: map[string]any{
		"cr": 9,
		"agent": map[string]any{
			"public_identity": "  Ruler   of the marches ",
			"goals":           []any{"preserve his line", "  ", "complete the ritual"},
			"fears":           []any{"fire"},
		},
	}}
	a := NPCAgentOf(e)
	if a.PublicIdentity != "Ruler of the marches" {
		t.Errorf("public identity: %q", a.PublicIdentity)
	}
	if len(a.Goals) != 2 || a.Goals[0] != "preserve his line" || a.Goals[1] != "complete the ritual" {
		t.Errorf("goals order/content: %v", a.Goals)
	}
	if len(a.Fears) != 1 {
		t.Errorf("fears: %v", a.Fears)
	}

	// Missing block, wrong shape, non-npc: all yield the zero agent.
	zero := NPCAgent{}
	if !reflect.DeepEqual(NPCAgentOf(&Entity{}), zero) {
		t.Errorf("missing block must be the zero agent")
	}
	if !reflect.DeepEqual(NPCAgentOf(&Entity{Payload: map[string]any{"agent": "not a map"}}), zero) {
		t.Errorf("wrong shape must be the zero agent")
	}
	if !reflect.DeepEqual(NPCAgentOf(nil), zero) {
		t.Errorf("nil entity must be the zero agent")
	}
}

func TestWithAgentPreservesOtherKeys(t *testing.T) {
	payload := map[string]any{"cr": 9, "notes": "pale"}
	out := WithAgent(payload, NPCAgent{PrivateTruth: "serves the forest", Goals: []string{"a", "", "b"}})
	if out["cr"] != 9 || out["notes"] != "pale" {
		t.Fatalf("other payload keys clobbered: %v", out)
	}
	agent := NPCAgentOf(&Entity{Payload: out})
	if agent.PrivateTruth != "serves the forest" || len(agent.Goals) != 2 {
		t.Fatalf("agent round trip: %+v", agent)
	}
	// The input payload is not mutated.
	if _, exists := payload["agent"]; exists {
		t.Fatalf("input payload was mutated")
	}
}
