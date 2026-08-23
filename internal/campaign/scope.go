package campaign

import (
	"fmt"
	"strings"
)

// ScopeKind names the four perspectives a retrieval can run under.
type ScopeKind string

const (
	ScopeKindDM        ScopeKind = "dm"
	ScopeKindParty     ScopeKind = "party"
	ScopeKindCharacter ScopeKind = "character"
	ScopeKindNPC       ScopeKind = "npc"
)

// PartyKnower is the literal knower value that means the party as a whole
// rather than one entity. It is reserved: no entity id may collide with it,
// and every knower column accepts either an entity id or exactly this.
const PartyKnower = "party"

// Scope is who is asking: dm, party, character:<id> or npc:<id>.
//
// Perspective is authorization, not instruction (ADR 2): a scope decides
// which rows come back from SQLite, in SQL. Every exported retrieval function
// in this package and in internal/knowledge takes one, and there is no
// unscoped read path outside the DM scope.
//
// In this package the scope guards the DM-side reads: the campaign graph's
// own retrieval functions serve the DM scope and refuse everything else with
// ErrScope. The party / character / npc read paths live in internal/knowledge,
// which joins the awareness tables to enforce them — that package owns the
// epistemics, and duplicating its authorization SQL here would be two
// implementations of the one rule that must never be wrong.
//
// Scope is defined here rather than in internal/knowledge (the name the ADRs
// use) because knowledge imports campaign for the graph types; defining it
// there would be an import cycle. knowledge re-exports it as knowledge.Scope.
type Scope struct {
	kind     ScopeKind
	entityID string
}

// ScopeDM is the DM perspective: everything the campaign holds except
// confidence 'proposed', which is invisible to every retrieval path until a
// human accepts it (ADR 3).
var ScopeDM = Scope{kind: ScopeKindDM}

// ScopeParty is the party's shared knowledge: the awareness rows knower
// 'party' carries.
var ScopeParty = Scope{kind: ScopeKindParty}

// ScopeCharacter is one player character's perspective: their own awareness
// plus the party's. id must be a pc entity of the campaign being read.
func ScopeCharacter(entityID string) Scope {
	return Scope{kind: ScopeKindCharacter, entityID: entityID}
}

// ScopeNPC is one NPC's perspective, for "ask as the Duke" simulation: only
// the awareness rows that NPC's id carries.
func ScopeNPC(entityID string) Scope {
	return Scope{kind: ScopeKindNPC, entityID: entityID}
}

// Kind reports which of the four perspectives this is.
func (s Scope) Kind() ScopeKind { return s.kind }

// EntityID is the character or npc entity the scope is bound to, empty for
// dm and party.
func (s Scope) EntityID() string { return s.entityID }

// IsDM reports whether this is the DM scope.
func (s Scope) IsDM() bool { return s.kind == ScopeKindDM }

// Knower is the primary knower a non-DM scope reads awareness for: 'party'
// for the party scope, the bound entity id otherwise. Empty for dm. Character
// scopes also read the party's rows; see Knowers in internal/knowledge.
func (s Scope) Knower() string {
	switch s.kind {
	case ScopeKindParty:
		return PartyKnower
	case ScopeKindCharacter, ScopeKindNPC:
		return s.entityID
	default:
		return ""
	}
}

// String renders the scope in the wire form ParseScope accepts:
// dm, party, character:<id>, npc:<id>.
func (s Scope) String() string {
	switch s.kind {
	case ScopeKindDM:
		return "dm"
	case ScopeKindParty:
		return "party"
	case ScopeKindCharacter, ScopeKindNPC:
		return string(s.kind) + ":" + s.entityID
	default:
		return string(s.kind)
	}
}

// ParseScope decodes the wire form: "dm", "party", "character:<entity-id>",
// "npc:<entity-id>". Whether the entity actually belongs to the campaign
// being read is checked at query time, not here.
func ParseScope(s string) (Scope, error) {
	kind, id, hasID := strings.Cut(s, ":")
	switch ScopeKind(kind) {
	case ScopeKindDM:
		if hasID {
			return Scope{}, fmt.Errorf("%w: scope %q takes no id", ErrInvalid, s)
		}
		return ScopeDM, nil
	case ScopeKindParty:
		if hasID {
			return Scope{}, fmt.Errorf("%w: scope %q takes no id", ErrInvalid, s)
		}
		return ScopeParty, nil
	case ScopeKindCharacter, ScopeKindNPC:
		if !hasID || strings.TrimSpace(id) == "" || id == PartyKnower {
			return Scope{}, fmt.Errorf("%w: scope %q needs an entity id", ErrInvalid, s)
		}
		return Scope{kind: ScopeKind(kind), entityID: id}, nil
	default:
		return Scope{}, fmt.Errorf("%w: unknown scope %q", ErrInvalid, s)
	}
}

// ErrScope marks a read attempted under a perspective that read path does
// not serve. It is the error the campaign package's own retrieval returns
// for every non-DM scope: those reads belong to internal/knowledge.
var ErrScope = fmt.Errorf("%w: scope not permitted here", ErrInvalid)

// requireDM rejects any scope but the DM's. The campaign graph's exported
// retrieval calls this first; knowledge implements the non-DM reads.
func (s Scope) requireDM() error {
	if !s.IsDM() {
		return fmt.Errorf("%w: %s", ErrScope, s)
	}
	return nil
}
