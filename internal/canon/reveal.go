package canon

// NPC reveal staging (MAD-313): the "ask as the Duke" simulation's write path.
//
// The ask endpoint's output is a suggestion, never a mutation: the answer and
// its reveals are returned to the DM and nothing is written. When the DM
// opts in, each reveal — a campaign assertion the NPC made that is not
// already in the record — lands here, in the same review queue every other
// machine proposal uses, behind the same "Make it canon" gate.
//
// A reveal is deliberately NOT an extraction candidate. The span rule binds
// candidates to a verbatim transcript span; a reveal's evidence is the
// scoped grounding the answer was built from, which is recorded in the item's
// detail instead. And the adversarial pass is skipped for the same reason:
// its skeptical stance is span-quoted ("where in the span do they perceive
// it?"), which has no purchase on a simulated reaction. The human gate is
// what makes a reveal canon, exactly as for every other machine proposal.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// StageRevealInput is one NPC reveal to stage. Statement is the campaign
// assertion the NPC made; Rationale is why the simulation produced it;
// Question is what the DM asked.
type StageRevealInput struct {
	CampaignID string
	NPCID      string
	NPCName    string
	Statement  string
	Rationale  string
	Question   string
}

// StageNPCReveal inserts one npc_reveal queue item, idempotent on the
// (npc, statement) pair: a repeat of the exact same reveal is a no-op for
// whatever state that item is in (open or decided), honoring the
// never-resurrect rule. Returns the item as it stands afterwards and whether
// this call staged it new.
func (s *Store) StageNPCReveal(ctx context.Context, in StageRevealInput) (*Review, bool, error) {
	in.Statement = strings.TrimSpace(in.Statement)
	if in.CampaignID == "" {
		return nil, false, fmt.Errorf("%w: campaign id is required", ErrInvalid)
	}
	if in.NPCID == "" {
		return nil, false, fmt.Errorf("%w: npc id is required", ErrInvalid)
	}
	if in.Statement == "" {
		return nil, false, fmt.Errorf("%w: reveal statement is required", ErrInvalid)
	}

	payload := map[string]any{
		"subject":        in.NPCID,
		"predicate":      "reveals",
		"object_literal": in.Statement,
		"statement":      in.Statement,
		"rationale":      in.Rationale,
		"question":       in.Question,
		"npc_name":       in.NPCName,
		// visibility defaults to public on apply; kept explicit in the
		// payload so a modified item can override it.
		"visibility": campaign.VisibilityPublic,
	}
	detail, err := json.Marshal(payload)
	if err != nil {
		return nil, false, fmt.Errorf("encode reveal payload: %w", err)
	}

	sum := sha256.Sum256([]byte(in.NPCID + "\x00" + collapseSpace(in.Statement)))
	dedup := "npc-reveal:" + hex.EncodeToString(sum[:])
	subject := in.NPCName
	if subject == "" {
		subject = "NPC"
	}
	before, err := s.reviewCountByDedup(ctx, in.CampaignID, dedup)
	if err != nil {
		return nil, false, err
	}
	if err := s.insertReview(ctx, in.CampaignID, ReviewNPCReveal, dedup, "", "",
		subject, in.Statement, string(detail)); err != nil {
		return nil, false, err
	}
	rev, err := s.reviewByDedup(ctx, in.CampaignID, dedup)
	if err != nil {
		return nil, false, err
	}
	return rev, before == 0, nil
}

// reviewCountByDedup counts existing items sharing a dedup key.
func (s *Store) reviewCountByDedup(ctx context.Context, campaignID, dedup string) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM canon_reviews WHERE campaign_id = ? AND dedup_key = ?`,
		campaignID, dedup).Scan(&n); err != nil {
		return 0, fmt.Errorf("count reviews by dedup: %w", err)
	}
	return n, nil
}

// reviewByDedup loads the item a dedup key names, whatever its status.
func (s *Store) reviewByDedup(ctx context.Context, campaignID, dedup string) (*Review, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+reviewCols+` FROM canon_reviews WHERE campaign_id = ? AND dedup_key = ?`,
		campaignID, dedup)
	rev, err := scanReview(row)
	if err != nil {
		return nil, fmt.Errorf("load review by dedup: %w", err)
	}
	if err := s.enrich(ctx, rev); err != nil {
		return nil, err
	}
	return rev, nil
}

// collapseSpace normalizes whitespace so trivially-different repeats of one
// reveal still dedup.
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// applyNPCReveal writes an accepted reveal into the graph as a canon fact
// whose provenance says exactly what happened: the model proposed it, and
// decidedBy accepted it through this queue. The payload may be the staged
// one (accept) or a DM-corrected one (modify); both flow through the same
// CreateFact staging rule.
func (s *Store) applyNPCReveal(ctx context.Context, rev *Review, p map[string]any, decidedBy string) (string, error) {
	statement := str(p, "statement")
	if statement == "" {
		return "", fmt.Errorf("%w: reveal payload has no statement", ErrInvalid)
	}
	predicate := str(p, "predicate")
	if predicate == "" {
		predicate = "reveals"
	}
	objectLiteral := str(p, "object_literal")
	if objectLiteral == "" {
		objectLiteral = statement
	}
	visibility := str(p, "visibility")
	if visibility == "" {
		visibility = campaign.VisibilityPublic
	}
	subject, err := s.resolveEntityRef(ctx, rev.CampaignID, str(p, "subject"))
	if err != nil {
		return "", err
	}
	var objectEntity string
	if ref := str(p, "object_entity"); ref != "" {
		objectEntity, err = s.resolveEntityRef(ctx, rev.CampaignID, ref)
		if err != nil {
			return "", err
		}
		// A fact's object is an entity or a literal, never both.
		objectLiteral = ""
	}
	fact, err := s.campaigns.CreateFact(ctx, rev.CampaignID, subject, predicate, objectEntity, objectLiteral,
		statement, campaign.ConfidenceCanon, visibility, decidedBy, []campaign.ProvenanceInput{{
			Quote:      statement,
			Method:     campaign.MethodAIProposed,
			AcceptedBy: decidedBy,
			AcceptedAt: s.now(),
		}})
	if err != nil {
		return "", fmt.Errorf("accept npc reveal: %w", err)
	}
	return fact.ID, nil
}
