// The campaign health report (MAD-312): the "🧠 What did we forget?"
// button. The deterministic findings carry the report — dangling hooks,
// unreachable secrets, dormant clues, unused NPCs, stalled regions,
// unfounded relationships, pacing counts — and the model summarizes; it
// does not discover. That division is the whole design: the LLM version of
// "what did we forget" is strictly worse than the joins, so the model's
// prose is a coat of paint over findings that already exist, and anything
// the model might invent has no channel into the report.
//
// The report refreshes the flag ledger on the way in (the same path as
// CheckCampaign), so its findings obey the ledger semantics: a DM who
// dismissed a thread stays dismissed.

package canon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// HealthPromptVersion stamps the narrative pass. Prompts are code: changing
// the prompt content in a way that affects output means bumping this string.
const HealthPromptVersion = "canon-health-001"

// DefaultPacingSessions is how many recent done sessions the pacing block
// covers.
const DefaultPacingSessions = 6

// HealthOptions tunes the report. Zero values pick the defaults.
type HealthOptions struct {
	CheckOptions
	// PacingSessions is how many recent done sessions the pacing block
	// counts (default 6).
	PacingSessions int
}

// DefaultHealthOptions returns the conservative default report configuration.
func DefaultHealthOptions() HealthOptions {
	return HealthOptions{CheckOptions: DefaultCheckOptions(), PacingSessions: DefaultPacingSessions}
}

func (o HealthOptions) withDefaults() HealthOptions {
	o.CheckOptions = o.CheckOptions.withDefaults()
	if o.PacingSessions <= 0 {
		o.PacingSessions = DefaultPacingSessions
	}
	return o
}

// HealthThread is one dropped thread in the report: an orphaned hook, an
// unreachable secret or a dormant clue, with the session it went quiet.
type HealthThread struct {
	Kind        string `json:"kind"` // orphan_thread | unreachable_secret | dormant_clue
	FactID      string `json:"fact_id"`
	Statement   string `json:"statement"`
	Message     string `json:"message"`
	Introduced  int64  `json:"introduced_session,omitempty"` // ordinal, when known
	WentQuiet   int64  `json:"went_quiet_session,omitempty"` // ordinal of the last session that touched it, when known
	SessionsAgo int64  `json:"sessions_quiet,omitempty"`
}

// HealthEntity is one flagged entity: an unused NPC or a stalled region.
type HealthEntity struct {
	CheckCode string `json:"check_code"`
	EntityID  string `json:"entity_id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Message   string `json:"message"`
}

// HealthRelationship is one unfounded relationship.
type HealthRelationship struct {
	CheckCode    string `json:"check_code"`
	Relationship string `json:"relationship_id"`
	FromName     string `json:"from"`
	RelType      string `json:"rel_type"`
	ToName       string `json:"to"`
	Message      string `json:"message"`
}

// SessionPacing is one session's event mix: combat (encounters), finds
// (discoveries), questions (qa), rulings and notes. The honest deterministic
// proxies for "combat / social / exploration" — the kind log is what the
// table actually wrote down.
type SessionPacing struct {
	SessionID   string `json:"session_id"`
	Ordinal     int64  `json:"ordinal"`
	Name        string `json:"name"`
	Encounters  int    `json:"encounters"`
	Discoveries int    `json:"discoveries"`
	QA          int    `json:"qa"`
	Rulings     int    `json:"rulings"`
	Notes       int    `json:"notes"`
}

// HealthReport is the whole "what did we forget?" answer.
type HealthReport struct {
	CampaignID     string               `json:"campaign_id"`
	CampaignName   string               `json:"campaign_name"`
	GeneratedAt    time.Time            `json:"-"`
	GeneratedAtMS  int64                `json:"generated_at"`
	Offline        bool                 `json:"offline"`
	PromptVersion  string               `json:"prompt_version"`
	Threads        []HealthThread       `json:"threads,omitempty"`
	Clues          []HealthThread       `json:"clues,omitempty"`
	UnusedNPCs     []HealthEntity       `json:"unused_npcs,omitempty"`
	DormantRegions []HealthEntity       `json:"dormant_regions,omitempty"`
	Unresolved     []HealthRelationship `json:"unresolved_relationships,omitempty"`
	Pacing         []SessionPacing      `json:"pacing,omitempty"`
	OpenFlagCount  int                  `json:"open_flags"`
	Narrative      string               `json:"narrative,omitempty"`
	Problems       []string             `json:"problems,omitempty"`
	InputTokens    int                  `json:"input_tokens"`
	OutputTokens   int                  `json:"output_tokens"`
	CostUSD        float64              `json:"cost_usd"`
}

// HealthReport runs the deterministic engine, refreshes the flag ledger, and
// assembles the report; a model client, when wired, adds the narrative
// summary over the deterministic sections and nothing else.
func (s *Store) HealthReport(ctx context.Context, campaignID string, opts HealthOptions) (*HealthReport, error) {
	opts = opts.withDefaults()
	snap, err := LoadSnapshot(ctx, s.db, campaignID)
	if err != nil {
		return nil, err
	}
	findings := CheckSnapshot(snap, opts.CheckOptions)
	if err := s.refreshFlags(ctx, campaignID, findings); err != nil {
		return nil, err
	}
	rep := &HealthReport{
		CampaignID:    campaignID,
		GeneratedAt:   s.now(),
		PromptVersion: HealthPromptVersion,
		Offline:       true,
	}
	rep.GeneratedAtMS = rep.GeneratedAt.UnixMilli()
	rep.CampaignName = campaignName(ctx, s.db, campaignID)

	threadsByCheck := map[string][]HealthThread{}
	entityByCheck := map[string][]HealthEntity{}
	var unresolved []HealthRelationship
	openCount := 0
	for _, f := range findings {
		switch f.Check {
		case CheckOrphanThread, CheckUnreachableSecret, CheckDormantClue:
			t := HealthThread{Kind: f.Check, FactID: f.RecordID, Message: f.Message}
			if fact, ok := factByID(snap, f.RecordID); ok {
				t.Statement = fact.Statement
				if intro, ok := snap.IntroducedSession[fact.ID]; ok {
					if sess, ok := sessionByID(snap, intro); ok {
						t.Introduced = sess.Ordinal
					}
				}
				if last := lastTouchSession(snap, fact.ID); last != nil {
					t.WentQuiet = last.Ordinal
					if t.Introduced > 0 {
						t.SessionsAgo = maxOrdinal(snap) - last.Ordinal
					}
				}
			}
			threadsByCheck[f.Check] = append(threadsByCheck[f.Check], t)
		case CheckUnusedNPC, CheckDormantRegion:
			h := HealthEntity{CheckCode: f.Check, EntityID: f.RecordID, Message: f.Message}
			if e, ok := entityByID(snap, f.RecordID); ok {
				h.Name, h.Kind = e.Name, e.Kind
			}
			entityByCheck[f.Check] = append(entityByCheck[f.Check], h)
		case CheckUnfoundedRelationship:
			r := HealthRelationship{CheckCode: f.Check, Relationship: f.RecordID, Message: f.Message}
			for _, rel := range snap.Relationships {
				if rel.ID != f.RecordID {
					continue
				}
				r.RelType = rel.RelType
				if e, ok := entityByID(snap, rel.FromEntity); ok {
					r.FromName = e.Name
				}
				if e, ok := entityByID(snap, rel.ToEntity); ok {
					r.ToName = e.Name
				}
			}
			unresolved = append(unresolved, r)
		}
	}
	// Open flags after the refresh, decided-and-gone excluded.
	flags, err := s.Flags(ctx, campaignID, FlagOpen)
	if err != nil {
		return nil, err
	}
	openCount = len(flags)

	rep.Threads = append(threadsByCheck[CheckOrphanThread], threadsByCheck[CheckUnreachableSecret]...)
	rep.Clues = threadsByCheck[CheckDormantClue]
	rep.UnusedNPCs = entityByCheck[CheckUnusedNPC]
	rep.DormantRegions = entityByCheck[CheckDormantRegion]
	rep.Unresolved = unresolved
	rep.OpenFlagCount = openCount

	pacing, err := s.sessionPacing(ctx, campaignID, opts.PacingSessions)
	if err != nil {
		return nil, err
	}
	rep.Pacing = pacing

	if s.model == nil {
		return rep, nil
	}
	narrative, inTok, outTok, cost, problems, err := s.healthNarrativePass(ctx, rep)
	if err != nil {
		return nil, err
	}
	rep.Offline = false
	rep.Narrative = narrative
	rep.InputTokens, rep.OutputTokens = inTok, outTok
	rep.CostUSD = cost
	rep.Problems = problems
	return rep, nil
}

// sessionPacing counts the event-kind mix over the last N done sessions,
// oldest first in the result. A session with no events at all still appears
// with zeros — a quiet session is pacing information, not an absence.
func (s *Store) sessionPacing(ctx context.Context, campaignID string, n int) ([]SessionPacing, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT gs.id, gs.ordinal, gs.name, e.kind, COUNT(e.id)
		  FROM game_sessions gs
		  LEFT JOIN session_events e ON e.session_id = gs.id
		 WHERE gs.campaign_id = ? AND gs.status = 'done'
		 GROUP BY gs.id, gs.ordinal, gs.name, e.kind
		 ORDER BY gs.ordinal DESC
		 LIMIT ?`, campaignID, 5*n) // event kinds cap the rows per session
	if err != nil {
		return nil, fmt.Errorf("health pacing: %w", err)
	}
	defer rows.Close()
	bySession := map[string]*SessionPacing{}
	var order []string
	for rows.Next() {
		var id, name string
		var kind sql.NullString
		var ordinal, count int64
		if err := rows.Scan(&id, &ordinal, &name, &kind, &count); err != nil {
			return nil, err
		}
		p, ok := bySession[id]
		if !ok {
			p = &SessionPacing{SessionID: id, Ordinal: ordinal, Name: name}
			bySession[id] = p
			order = append(order, id)
		}
		if !kind.Valid {
			continue // the empty row of an event-less session
		}
		switch kind.String {
		case "encounter":
			p.Encounters += int(count)
		case "discovery":
			p.Discoveries += int(count)
		case "qa":
			p.QA += int(count)
		case "ruling":
			p.Rulings += int(count)
		case "note":
			p.Notes += int(count)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []SessionPacing
	for i := len(order) - 1; i >= 0 && len(out) < n; i-- { // newest first, then reverse
		out = append(out, *bySession[order[i]])
	}
	return out, nil
}

// healthNarrativePass summarizes the deterministic sections in the model's
// own words. The model receives the findings as JSON and returns prose; it
// has no channel to add, remove or alter findings — its output lands in
// Narrative verbatim and nothing else reads it.
func (s *Store) healthNarrativePass(ctx context.Context, rep *HealthReport) (string, int, int, float64, []string, error) {
	sections, err := json.Marshal(map[string]any{
		"threads":                  nonNil(rep.Threads),
		"clues":                    nonNil(rep.Clues),
		"unused_npcs":              nonNil(rep.UnusedNPCs),
		"dormant_regions":          nonNil(rep.DormantRegions),
		"unresolved_relationships": nonNil(rep.Unresolved),
		"pacing":                   nonNil(rep.Pacing),
		"open_flags":               rep.OpenFlagCount,
	})
	if err != nil {
		return "", 0, 0, 0, nil, err
	}
	var lastCall time.Time
	if err := s.waitInterval(ctx, &lastCall); err != nil {
		return "", 0, 0, 0, nil, err
	}
	completion, err := s.model.Complete(ctx, healthSystemPrompt(), healthUserPrompt(rep.CampaignName, sections))
	if err != nil {
		return "", 0, 0, 0, nil, fmt.Errorf("health narrative pass: %w", err)
	}
	cost := s.cfg.costUSD(completion.InputTokens, completion.OutputTokens)
	if s.cfg.BudgetUSD > 0 && cost > s.cfg.BudgetUSD {
		return "", completion.InputTokens, completion.OutputTokens, cost,
			[]string{fmt.Sprintf("narrative pass cost %.4f USD exceeds the run budget %.2f USD; report shipped without narrative", cost, s.cfg.BudgetUSD)},
			nil
	}
	narrative := strings.TrimSpace(completion.Text)
	if len(narrative) > 4000 {
		narrative = narrative[:4000] + "…"
	}
	return narrative, completion.InputTokens, completion.OutputTokens, cost, nil, nil
}

// healthSystemPrompt is the narrative pass's standing instruction: the
// model is an editor summarizing findings that already exist.
func healthSystemPrompt() string {
	return `You are the campaign editor summarizing a D&D campaign's health report. You will receive the report's deterministic findings as JSON: dropped threads, unreachable secrets, dormant clues, unused NPCs, stalled regions, unfounded relationships, and the pacing mix of recent sessions.

YOUR JOB: summarize, in the DM's voice, what the campaign is forgetting and where it is drifting. Two or three short paragraphs at most.

HARD RULES:
- You may NOT add findings. Every thread, clue, name and region you mention must come from the JSON you were given. If the JSON is empty everywhere, say the campaign is in good shape — do not invent concerns.
- You may NOT drop severity implicitly: lead with the dropped threads and dormant clues, then the pacing drift if the numbers show it.
- No headings, no bullet lists, no markdown — plain prose a DM reads in five seconds.
- Never suggest mechanics the report does not contain. You summarize; you do not discover.`
}

// healthUserPrompt renders the deterministic sections for the narrative.
func healthUserPrompt(campaignName string, sections json.RawMessage) string {
	var b strings.Builder
	fmt.Fprintf(&b, "TASK: Summarize this campaign health report.\n\nCampaign: %s\n\nFINDINGS (JSON — the only source you may draw on):\n%s\n\n", campaignName, sections)
	b.WriteString("Write the summary following every rule in the system message. Plain prose only.")
	return b.String()
}

/* ---------- small helpers ---------- */

// campaignName reads the campaign's display name for the report header.
func campaignName(ctx context.Context, db *sql.DB, campaignID string) string {
	var name string
	if err := db.QueryRowContext(ctx, `SELECT name FROM campaigns WHERE id = ?`, campaignID).Scan(&name); err != nil {
		return campaignID
	}
	return name
}

func sessionByID(snap *Snapshot, id string) (SessionRef, bool) {
	for _, s := range snap.Sessions {
		if s.ID == id {
			return s, true
		}
	}
	return SessionRef{}, false
}

// lastTouchSession finds the session it went quiet: the latest session that
// produced a discovery touching the thread, or the session that introduced
// it when nothing ever did.
func lastTouchSession(snap *Snapshot, factID string) *SessionRef {
	var best *SessionRef
	for _, d := range snap.Discoveries {
		if d.FactID != factID || d.SessionID == "" {
			continue
		}
		if sess, ok := sessionByID(snap, d.SessionID); ok {
			if best == nil || sess.Ordinal > best.Ordinal {
				s := sess
				best = &s
			}
		}
	}
	if best != nil {
		return best
	}
	if intro, ok := snap.IntroducedSession[factID]; ok {
		if sess, ok := sessionByID(snap, intro); ok {
			s := sess
			return &s
		}
	}
	return nil
}

func maxOrdinal(snap *Snapshot) int64 {
	var max int64
	for _, s := range snap.Sessions {
		if s.Ordinal > max {
			max = s.Ordinal
		}
	}
	return max
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
