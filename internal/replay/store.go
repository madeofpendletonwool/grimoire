package replay

// The replay's reads (MAD-426): everything arrives through narrow
// windows onto the stores that already own the rows — the same
// standing the table screen takes — so this package owns no SQL, no
// schema, and no truth of its own. The Battle is one loaded fight
// (journal, recorded rows, seeds); its Frame method is the scrub, its
// Verify method the checksum. The Timeline is one session's whole
// replayable picture: the log as it was written, with the fights
// indexed out of it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/combat"
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
)

// Combats is the tracker's window: one battle with its order, and the
// journal underneath. The combat store implements it.
type Combats interface {
	Get(ctx context.Context, campaignID, combatID string) (*combat.Combat, []combat.Combatant, error)
	Log(ctx context.Context, campaignID, combatID string, after int64, limit int) ([]combat.LogEntry, error)
}

// Sessions is the session store's window: one sitting and its log in
// play order. The gamesession store implements it.
type Sessions interface {
	GetSession(ctx context.Context, sessionID string) (*gamesession.Session, error)
	ListEvents(ctx context.Context, sessionID string) ([]gamesession.Event, error)
}

// Store runs the replay over the tracker's and the session store's
// windows. Construct with New and wire once.
type Store struct {
	combats  Combats
	sessions Sessions
}

// New builds a replay store. Both windows are required: a replay
// without the journal or without the session log is half a feature.
func New(combats Combats, sessions Sessions) (*Store, error) {
	if combats == nil {
		return nil, errors.New("replay: the combat store is required")
	}
	if sessions == nil {
		return nil, errors.New("replay: the session store is required")
	}
	return &Store{combats: combats, sessions: sessions}, nil
}

/* ---------- one battle, scrubbable ---------- */

// Battle is one fight loaded for replay: the combat row, the journal
// in play order (each row carrying the round it belongs to), the
// recorded end-state rows, and the opening lineup they seed. Loading
// is one pass; every Frame is then an in-memory fold — scrubbing to
// any point costs the same as scrubbing to the end.
type Battle struct {
	Combat combat.Combat
	Events []Event
	Rows   []combat.Combatant // the recorded state the fold must reproduce
	seeds  []combat.Combatant
	names  map[string]string // combatant id -> name, for the journal listing
	maxSeq int64
}

// Battle loads one campaign's combat for replay. A combat outside the
// campaign is the same not-found as a missing one, the scoping rule
// every store keeps.
func (s *Store) Battle(ctx context.Context, campaignID, combatID string) (*Battle, error) {
	c, rows, err := s.combats.Get(ctx, campaignID, combatID)
	if err != nil {
		return nil, err
	}
	log, err := s.combats.Log(ctx, campaignID, combatID, 0, 0)
	if err != nil {
		return nil, err
	}
	events := make([]Event, 0, len(log))
	for _, e := range log {
		events = append(events, Event{
			Seq: e.Seq, Kind: e.Kind, CombatantID: e.CombatantID, Amount: e.Amount,
			Note: e.Note, Actor: e.Actor, Payload: e.Payload, CreatedAt: e.CreatedAt.Format(time.RFC3339),
		})
	}
	walkRounds(events)
	b := &Battle{
		Combat: *c, Events: events, Rows: rows,
		seeds: Seeds(rows, events),
		names: namesOf(rows),
	}
	for _, ev := range events {
		if ev.Seq > b.maxSeq {
			b.maxSeq = ev.Seq
		}
	}
	return b, nil
}

// MaxSeq is the journal's last position — the scrub bar's far end.
func (b *Battle) MaxSeq() int64 { return b.maxSeq }

// JournalName resolves the combatant a journal row names, for display.
func (b *Battle) JournalName(id string) string {
	if id == "" {
		return ""
	}
	return b.names[id]
}

// Frame derives the battle's state after every journal row with seq <=
// at. at 0 is the opening lineup; at past the end is clamped to the
// end. The fold is pure and in-memory, so every scrub position costs
// the same, and feels live.
func (b *Battle) Frame(at int64) (*Frame, error) {
	if at > b.maxSeq {
		at = b.maxSeq
	}
	if at < 0 {
		at = 0
	}
	st, err := Fold(b.seeds, b.Events, at)
	if err != nil {
		return nil, err
	}
	return &Frame{
		At: at, Round: st.Round, TurnIndex: st.TurnIndex, Turn: st.Turn(),
		Status: st.Status, Order: st.Combatants, Checksum: Checksum(st),
	}, nil
}

// Frame is one scrub position's answer: where the battle stands at
// that journal row.
type Frame struct {
	At        int64              `json:"at"`
	Round     int                `json:"round"`
	TurnIndex int                `json:"turn_index"`
	Turn      string             `json:"turn,omitempty"`
	Status    string             `json:"status"`
	Order     []combat.Combatant `json:"order"`
	Checksum  string             `json:"checksum"`
}

// VerifyResult is the checksum assertion's outcome: the folded
// end-state's checksum, whether it reproduces the recorded rows
// exactly, and — when it does not — where the journal and the rows
// disagree, field by field.
type VerifyResult struct {
	Matches     bool     `json:"matches"`
	Checksum    string   `json:"checksum"`
	Differences []string `json:"differences,omitempty"`
}

// Verify folds the whole journal and asserts the derived end-state
// against the recorded combatant rows — the acceptance check, run
// anywhere it matters. Round and turn pointer ride along, so a battle
// that ended mid-round is caught too.
func (b *Battle) Verify() VerifyResult {
	st, err := Fold(b.seeds, b.Events, b.maxSeq)
	if err != nil {
		return VerifyResult{Matches: false, Differences: []string{err.Error()}}
	}
	folded := canonOf(st)
	recorded := canonOfRows(b.Combat.Round, b.Combat.TurnIndex, b.Rows)
	out := VerifyResult{Checksum: Checksum(st), Matches: fmtJSON(folded) == fmtJSON(recorded)}
	if out.Matches {
		return out
	}
	out.Differences = diffStates(folded, recorded)
	return out
}

// diffStates names every disagreement between the folded and recorded
// canonical states, in a stable order.
func diffStates(folded, recorded canonState) []string {
	var out []string
	if folded.Round != recorded.Round {
		out = append(out, fmt.Sprintf("round: journal derives %d, rows record %d", folded.Round, recorded.Round))
	}
	if folded.TurnIndex != recorded.TurnIndex {
		out = append(out, fmt.Sprintf("turn: journal derives position %d, rows record %d", folded.TurnIndex, recorded.TurnIndex))
	}
	byID := make(map[string]canonFighter, len(recorded.Combatants))
	for _, f := range recorded.Combatants {
		byID[f.ID] = f
	}
	for _, f := range folded.Combatants {
		r, ok := byID[f.ID]
		if !ok {
			out = append(out, fmt.Sprintf("%s: in the journal's battle, not on the rows", f.Name))
			continue
		}
		out = append(out, diffFighter(f, r)...)
	}
	if len(out) == 0 {
		out = append(out, "the states serialize differently but every field compared equal")
	}
	return out
}

func diffFighter(f, r canonFighter) []string {
	var out []string
	note := func(field string, want, got any) {
		if fmt.Sprint(want) != fmt.Sprint(got) {
			out = append(out, fmt.Sprintf("%s %s: journal derives %v, row records %v", f.Name, field, want, got))
		}
	}
	note("hp", f.HP, r.HP)
	note("temp hp", f.TempHP, r.TempHP)
	note("max hp reduction", f.HPReduction, r.HPReduction)
	note("downed", f.Downed, r.Downed)
	note("stable", f.Stable, r.Stable)
	note("dead", f.Dead, r.Dead)
	note("death-save successes", f.DeathSuccesses, r.DeathSuccesses)
	note("death-save failures", f.DeathFailures, r.DeathFailures)
	note("reaction", f.ReactionSpent, r.ReactionSpent)
	note("legendary used", f.LegendaryUsed, r.LegendaryUsed)
	note("conditions", fmtJSON(f.Conditions), fmtJSON(r.Conditions))
	return out
}

func fmtJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// walkRounds stamps each journal row with the round it belongs to:
// turn rows carry the round (and new_round on a wrap), every row in
// between inherits the standing one, start opens at 1 and end closes
// on the payload's own round.
func walkRounds(events []Event) {
	round := 1
	for i := range events {
		ev := &events[i]
		switch ev.Kind {
		case "start":
			if r, ok := pInt(ev.Payload, "round"); ok && r > 0 {
				round = r
			}
		case "turn":
			if r, ok := pInt(ev.Payload, "new_round"); ok {
				round = r
			} else if r, ok := pInt(ev.Payload, "round"); ok {
				round = r
			}
		case "end":
			if r, ok := pInt(ev.Payload, "round"); ok && r > 0 {
				round = r
			}
		}
		ev.Round = round
	}
}

func namesOf(rows []combat.Combatant) map[string]string {
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.ID] = r.Name
	}
	return out
}

/* ---------- one session, replayable ---------- */

// Fight is one battle in a session's timeline, indexed out of the log
// itself: the combat events' own combat_id, in first-appearance order.
type Fight struct {
	CombatID  string `json:"combat_id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	Round     int    `json:"round"`
	LogLen    int    `json:"log_len"`
	StartedAt string `json:"started_at,omitempty"`
	EndedAt   string `json:"ended_at,omitempty"`
}

// TimelineEvent is one session log event in the replay's shape: the
// log's own words, with the combat a kind 'combat' event belongs to
// pulled out where a timeline can link it.
type TimelineEvent struct {
	Seq       int64  `json:"seq"`
	Kind      string `json:"kind"`
	Summary   string `json:"summary,omitempty"`
	Detail    string `json:"detail,omitempty"`
	CombatID  string `json:"combat_id,omitempty"`
	Round     int    `json:"round,omitempty"`
	CreatedAt string `json:"created_at"`
}

// Timeline is a session's whole replayable picture: the sitting, the
// log in play order, and the fights the log holds.
type Timeline struct {
	Session struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Status  string `json:"status"`
		Ordinal int64  `json:"ordinal"`
	} `json:"session"`
	Events []TimelineEvent `json:"events"`
	Fights []Fight         `json:"fights,omitempty"`
}

// Session builds a session's timeline. A session outside the campaign
// is not found, the scoping rule as ever.
func (s *Store) Session(ctx context.Context, campaignID, sessionID string) (*Timeline, error) {
	ses, err := s.sessions.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if ses.Campaign != campaignID {
		return nil, fmt.Errorf("%w: session %s", campaign.ErrNotFound, sessionID)
	}
	events, err := s.sessions.ListEvents(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	tl := &Timeline{Events: make([]TimelineEvent, 0, len(events))}
	tl.Session.ID, tl.Session.Name, tl.Session.Status, tl.Session.Ordinal = ses.ID, ses.Name, ses.Status, ses.Ordinal

	seen := map[string]bool{}
	for _, ev := range events {
		te := TimelineEvent{
			Seq: ev.Seq, Kind: ev.Kind, Summary: ev.Summary, Detail: ev.Detail,
			CreatedAt: ev.CreatedAt.Format(time.RFC3339),
		}
		if ev.Kind == gamesession.EventCombat {
			if id, _ := ev.Payload["combat_id"].(string); id != "" {
				te.CombatID = id
				if r, ok := ev.Payload["round"].(float64); ok {
					te.Round = int(r)
				}
				if !seen[id] {
					seen[id] = true
					tl.Fights = append(tl.Fights, Fight{CombatID: id})
				}
			}
		}
		tl.Events = append(tl.Events, te)
	}
	// Enrich each indexed fight from the tracker's own rows — through
	// the window, so scope stays the campaign's call.
	for i := range tl.Fights {
		f := &tl.Fights[i]
		c, _, err := s.combats.Get(ctx, campaignID, f.CombatID)
		if err != nil {
			continue // an unresolvable mirror is skipped, not fatal to the timeline
		}
		f.Name, f.Status, f.Round = c.Name, c.Status, c.Round
		f.StartedAt = c.StartedAt.Format(time.RFC3339)
		if !c.EndedAt.IsZero() {
			f.EndedAt = c.EndedAt.Format(time.RFC3339)
		}
		if log, err := s.combats.Log(ctx, campaignID, f.CombatID, 0, 0); err == nil {
			f.LogLen = len(log)
		}
	}
	return tl, nil
}
