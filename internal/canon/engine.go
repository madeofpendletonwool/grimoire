// The deterministic consistency engine (MAD-309): pure functions over a
// campaign snapshot — no DB handle inside a rule, no LLM anywhere, every rule
// unit-testable. This is the Arda port's cheapest, most reliable layer: the
// checks the models cannot be trusted to make (did the party already learn
// this? does this quest move exist?) are joins, and joins are free.
//
// The rule set is the union of two halves:
//
//   - the graph-integrity rules internal/campaign already owns
//     (dangling_reference, cause_after_effect, quest_transition_invalid,
//     contradictory_facts, entity_merge_candidate, duplicate_fact,
//     fact_without_provenance). CheckSnapshot runs campaign.Check for those
//     rather than duplicating a single rule — one implementation of each
//     invariant, two entry points.
//
//   - the epistemic, session and encounter rules this file adds:
//     spoiler_leak, knowledge_before_discovery, awareness_without_source,
//     orphan_thread, unreachable_secret, stat_block_unresolved,
//     party_level_drift.
//
// spoiler_leak is the invariant that makes the player portal trustworthy, and
// it is a join, not a model call. orphan_thread and unreachable_secret are
// the parent brainstorm's "what did we forget?" button, delivered
// deterministically: no tokens, no hallucination, milliseconds.
//
// Findings land in the canon_flags ledger (flags.go) with Arda's exact
// semantics: keyed by (check_code, record_kind, record_id), re-runs refresh
// but never clobber a human decision, and open findings the engine stops
// reporting are cleared.

package canon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/encounter"
)

/* ---------- check codes ---------- */

// The epistemic, session and encounter checks this engine adds on top of the
// campaign package's graph-integrity codes (campaign.Check* constants). Codes
// are stable strings: the flag ledger keys rows on them.
const (
	CheckSpoilerLeak              = "spoiler_leak"
	CheckKnowledgeBeforeDiscovery = "knowledge_before_discovery"
	CheckAwarenessWithoutSource   = "awareness_without_source"
	CheckOrphanThread             = "orphan_thread"
	CheckUnreachableSecret        = "unreachable_secret"
	CheckStatBlockUnresolved      = "stat_block_unresolved"
	CheckPartyLevelDrift          = "party_level_drift"
)

// DefaultOrphanSessions is how many sessions a hook, secret or clue may sit
// unreferenced before orphan_thread reports it. Three is the "one session to
// plant it, one to pay it off, one to admit you forgot" default; raise it for
// a slow-burn campaign by setting CheckOptions.OrphanSessions.
const DefaultOrphanSessions = 3

// CheckOptions tunes the deterministic rules. Zero values pick the defaults.
type CheckOptions struct {
	// OrphanSessions is the session gap after which an untouched secret
	// thread becomes an orphan_thread finding (default 3).
	OrphanSessions int
}

// DefaultCheckOptions returns the conservative default rule configuration.
func DefaultCheckOptions() CheckOptions {
	return CheckOptions{OrphanSessions: DefaultOrphanSessions}
}

func (o CheckOptions) withDefaults() CheckOptions {
	if o.OrphanSessions <= 0 {
		o.OrphanSessions = DefaultOrphanSessions
	}
	return o
}

/* ---------- the snapshot ---------- */

// AwarenessRow is the awareness slice the epistemic checks join against: who
// holds what stance on which fact, and the discovery that put it there.
type AwarenessRow struct {
	Knower      string // entity id or campaign.PartyKnower
	FactID      string
	Stance      string
	DiscoveryID string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// DiscoveryRow is the discovery trail behind an awareness row.
type DiscoveryRow struct {
	ID           string
	FactID       string
	DiscoveredBy string // entity id or campaign.PartyKnower
	SessionID    string
	CreatedAt    time.Time
}

// SessionRef is one game session as the session-dating checks see it.
type SessionRef struct {
	ID        string
	Ordinal   int64
	Status    string
	StartedAt time.Time // zero when the session never started
	CreatedAt time.Time
}

// date is the moment the session came into the world: its start when it has
// one, otherwise its creation. knowledge_before_discovery dates sessions with
// this.
func (s SessionRef) date() time.Time {
	if !s.StartedAt.IsZero() {
		return s.StartedAt
	}
	return s.CreatedAt
}

// EncounterRef is one planned encounter: an 'encounter' session event on a
// planned session whose payload carries a roster. The payload contract this
// loader accepts is
//
//	{"name": "...", "party": [3,3,2,3], "monsters": [{"name": "Goblin", "cr": "1/4", "count": 6}, ...]}
//
// — the shape the in-play encounter log and the Stage 5 session prep write.
// Events with no monsters array are not planned encounters and are skipped.
type EncounterRef struct {
	EventID   string
	SessionID string
	Name      string
	Party     []int // the levels it was planned against, when recorded
	Monsters  []encounter.Monster
}

// Snapshot is a campaign's whole state as the deterministic engine sees it:
// the campaign graph snapshot (entities, facts, events, relationships,
// quests) plus the epistemic layer (awareness, discoveries), the session
// timeline, the planned encounters, and the two reference sets the encounter
// rules need — the mirrored bestiary and the current party levels. Check
// runs pure over this struct; LoadSnapshot is the only DB toucher.
type Snapshot struct {
	*campaign.Snapshot
	Awareness   []AwarenessRow
	Discoveries []DiscoveryRow
	Sessions    []SessionRef
	Encounters  []EncounterRef
	// Bestiary is the set of squashed monster names the local mirror holds.
	// Empty means the mirror was never synced and stat_block_unresolved is
	// skipped: "cannot resolve" must not become "does not exist".
	Bestiary map[string]bool
	// Party is the campaign's current pc levels, best-effort read from pc
	// entity payloads ("level" key), in name order. Empty means no pc
	// declares a level and party_level_drift is skipped.
	Party []int
	// IntroducedSession maps a fact id to the session its earliest
	// provenance row cites — the session that introduced it. Facts whose
	// provenance cites no session (or a session id that does not resolve)
	// are absent: they cannot be session-dated.
	IntroducedSession map[string]string
}

/* ---------- loading ---------- */

// LoadSnapshot reads one campaign's full engine state into memory. Like
// campaign.LoadSnapshot it is DM material by definition — every fact, secret
// and proposal included — so the callers that expose it (the CLI, the
// DM-gated API handler) hold the DM scope themselves.
func LoadSnapshot(ctx context.Context, db *sql.DB, campaignID string) (*Snapshot, error) {
	graph, err := campaign.LoadSnapshot(ctx, campaign.ScopeDM, db, campaignID)
	if err != nil {
		return nil, err
	}
	snap := &Snapshot{
		Snapshot:          graph,
		Bestiary:          map[string]bool{},
		IntroducedSession: map[string]string{},
	}

	rows, err := db.QueryContext(ctx, `
		SELECT knower, fact_id, stance, COALESCE(discovery_id, ''), created_at, updated_at
		  FROM awareness WHERE campaign_id = ? ORDER BY knower, fact_id`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("engine awareness: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a AwarenessRow
		var created, updated int64
		if err := rows.Scan(&a.Knower, &a.FactID, &a.Stance, &a.DiscoveryID, &created, &updated); err != nil {
			return nil, err
		}
		a.CreatedAt = time.UnixMilli(created).UTC()
		a.UpdatedAt = time.UnixMilli(updated).UTC()
		snap.Awareness = append(snap.Awareness, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("engine awareness: %w", err)
	}

	rows, err = db.QueryContext(ctx, `
		SELECT id, fact_id, discovered_by, COALESCE(session_id, ''), created_at
		  FROM discoveries WHERE campaign_id = ? ORDER BY created_at, id`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("engine discoveries: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var d DiscoveryRow
		var created int64
		if err := rows.Scan(&d.ID, &d.FactID, &d.DiscoveredBy, &d.SessionID, &created); err != nil {
			return nil, err
		}
		d.CreatedAt = time.UnixMilli(created).UTC()
		snap.Discoveries = append(snap.Discoveries, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("engine discoveries: %w", err)
	}

	rows, err = db.QueryContext(ctx, `
		SELECT id, ordinal, status, COALESCE(started_at, 0), created_at
		  FROM game_sessions WHERE campaign_id = ? ORDER BY ordinal`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("engine sessions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s SessionRef
		var started, created int64
		if err := rows.Scan(&s.ID, &s.Ordinal, &s.Status, &started, &created); err != nil {
			return nil, err
		}
		if started > 0 {
			s.StartedAt = time.UnixMilli(started).UTC()
		}
		s.CreatedAt = time.UnixMilli(created).UTC()
		snap.Sessions = append(snap.Sessions, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("engine sessions: %w", err)
	}

	// The session each fact was introduced in: its earliest provenance row's
	// session citation. Unresolvable citations (the seed's pseudo-sessions,
	// imports) are simply not session-dated and orphan_thread skips them.
	rows, err = db.QueryContext(ctx, `
		SELECT p.fact_id, p.session_id FROM fact_provenance p
		 JOIN facts f ON f.id = p.fact_id AND f.campaign_id = ?
		 WHERE p.session_id IS NOT NULL AND p.session_id <> ''
		 ORDER BY p.created_at, p.id`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("engine provenance: %w", err)
	}
	defer rows.Close()
	sessionsByID := map[string]bool{}
	for _, s := range snap.Sessions {
		sessionsByID[s.ID] = true
	}
	for rows.Next() {
		var factID, sessionID string
		if err := rows.Scan(&factID, &sessionID); err != nil {
			return nil, err
		}
		if !sessionsByID[sessionID] {
			continue
		}
		if _, seen := snap.IntroducedSession[factID]; !seen {
			snap.IntroducedSession[factID] = sessionID
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("engine provenance: %w", err)
	}

	if err := loadEncounters(ctx, db, campaignID, snap); err != nil {
		return nil, err
	}

	// The local bestiary mirror lives in a table the encounter package owns
	// and creates on demand; a box that never built the catalog has no table,
	// which means "cannot resolve", never "nothing exists".
	have, err := tableExists(ctx, db, "bestiary")
	if err != nil {
		return nil, err
	}
	if have {
		rows, err = db.QueryContext(ctx, `SELECT key, name FROM bestiary`)
		if err != nil {
			return nil, fmt.Errorf("engine bestiary: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var key, name string
			if err := rows.Scan(&key, &name); err != nil {
				return nil, err
			}
			snap.Bestiary[squashName(name)] = true
			if key != "" {
				snap.Bestiary[key] = true
			}
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("engine bestiary: %w", err)
		}
	}

	// The current party: every live pc's declared level, in name order. The
	// seed and the encounter surfaces both keep levels in the pc payload's
	// "level" key; a pc without one is not level-dated and simply does not
	// contribute.
	type pcLevel struct {
		name  string
		level int
	}
	var pcs []pcLevel
	for _, e := range snap.Entities {
		if e.Kind != campaign.KindPC || e.Status == campaign.StatusDeleted {
			continue
		}
		if lvl, ok := payloadLevel(e.Payload); ok {
			pcs = append(pcs, pcLevel{name: e.Name, level: lvl})
		}
	}
	sort.Slice(pcs, func(i, j int) bool { return pcs[i].name < pcs[j].name })
	for _, p := range pcs {
		snap.Party = append(snap.Party, p.level)
	}
	return snap, nil
}

// loadEncounters reads the planned encounters: 'encounter' events on planned
// sessions whose payload carries a monsters roster.
func loadEncounters(ctx context.Context, db *sql.DB, campaignID string, snap *Snapshot) error {
	rows, err := db.QueryContext(ctx, `
		SELECT e.id, e.session_id, e.summary, e.payload
		  FROM session_events e
		  JOIN game_sessions gs ON gs.id = e.session_id
		 WHERE gs.campaign_id = ? AND e.kind = 'encounter' AND gs.status = 'planned'
		 ORDER BY gs.ordinal, e.seq`, campaignID)
	if err != nil {
		return fmt.Errorf("engine encounters: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ref EncounterRef
		var summary, payloadJSON string
		if err := rows.Scan(&ref.EventID, &ref.SessionID, &summary, &payloadJSON); err != nil {
			return err
		}
		var payload struct {
			Name     string `json:"name"`
			Party    []int  `json:"party"`
			Monsters []struct {
				Name  string `json:"name"`
				CR    string `json:"cr"`
				XP    int    `json:"xp"`
				Count int    `json:"count"`
			} `json:"monsters"`
		}
		if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
			continue // a malformed payload is not engine material
		}
		if len(payload.Monsters) == 0 {
			continue
		}
		ref.Name = payload.Name
		if ref.Name == "" {
			ref.Name = summary
		}
		ref.Party = payload.Party
		for _, m := range payload.Monsters {
			count := m.Count
			if count <= 0 {
				count = 1
			}
			xp := m.XP
			if xp <= 0 && strings.TrimSpace(m.CR) != "" {
				if label, err := encounter.ParseCR(m.CR); err == nil {
					xp, _ = encounter.CRXP(label)
				}
			}
			ref.Monsters = append(ref.Monsters, encounter.Monster{
				Name: strings.TrimSpace(m.Name), CR: strings.TrimSpace(m.CR), XP: xp, Count: count,
			})
		}
		snap.Encounters = append(snap.Encounters, ref)
	}
	return rows.Err()
}

// tableExists reports whether a named table is present.
func tableExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var one int
	err := db.QueryRowContext(ctx,
		`SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check table %s: %w", name, err)
	}
	return true, nil
}

// payloadLevel reads a pc payload's "level" key, tolerating the numeric and
// string forms JSON round-trips produce.
func payloadLevel(payload map[string]any) (int, bool) {
	v, ok := payload["level"]
	if !ok {
		return 0, false
	}
	switch lvl := v.(type) {
	case float64:
		return int(lvl), lvl >= 1 && lvl <= 20
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(lvl))
		return n, err == nil && n >= 1 && n <= 20
	default:
		return 0, false
	}
}

// squashName lowercases and keeps only letters and digits — the same name
// normalizer the cards package, the entity resolver and the bestiary mirror
// use, reimplemented locally because it is unexported there.
func squashName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

/* ---------- the checks ---------- */

// grantingStance reports whether a stance makes a fact render on a surface
// that knower holds: knows, suspects and believes_false grant; a deliberate
// unaware does not.
func grantingStance(stance string) bool {
	return stance == "knows" || stance == "suspects" || stance == "believes_false"
}

// CheckSnapshot runs every deterministic rule over a snapshot and returns the
// findings, sorted by check then record so output is stable. Pure: no DB, no
// clock, no network — safe to run anywhere, offline included.
func CheckSnapshot(snap *Snapshot, opts CheckOptions) []campaign.Finding {
	opts = opts.withDefaults()
	out := campaign.Check(snap.Snapshot)
	out = append(out, checkSpoilerLeak(snap)...)
	out = append(out, checkKnowledgeBeforeDiscovery(snap)...)
	out = append(out, checkAwarenessWithoutSource(snap)...)
	out = append(out, checkOrphanThread(snap, opts)...)
	out = append(out, checkUnreachableSecret(snap)...)
	out = append(out, checkEncounters(snap)...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Check != out[j].Check {
			return out[i].Check < out[j].Check
		}
		if out[i].RecordKind != out[j].RecordKind {
			return out[i].RecordKind < out[j].RecordKind
		}
		return out[i].RecordID < out[j].RecordID
	})
	return out
}

// checkSpoilerLeak: a player-visible surface rendering a fact the party's
// awareness says they are unaware of.
//
// The player-visible surfaces are the party and character scopes, and a fact
// renders on one exactly when it is live (not proposed, not superseded) and a
// granting awareness row exists for the scope's knowers — the same join
// internal/knowledge enforces in SQL. The party's own row cannot both grant
// and say unaware (one row per knower and fact), so the leak this check
// catches is the cross-knower contradiction: the party row explicitly records
// "they walked past this" (stance unaware is stored, never inferred) while a
// pc's grant renders the very same fact on that character's portal. One of
// the two rows is wrong — the DM either records the party's exposure or drops
// the stale denial — and until then the portal cannot be trusted to keep the
// secret. NPC grants are deliberately out of scope: an NPC's knowledge feeds
// DM-side simulation, not player surfaces.
func checkSpoilerLeak(snap *Snapshot) []campaign.Finding {
	facts := map[string]campaign.Fact{}
	for _, f := range snap.Facts {
		facts[f.ID] = f
	}
	pcs := map[string]string{} // entity id -> name
	for _, e := range snap.Entities {
		if e.Kind == campaign.KindPC && e.Status != campaign.StatusDeleted {
			pcs[e.ID] = e.Name
		}
	}
	granted := map[string]string{} // fact id -> first granting pc (by name order)
	for _, a := range snap.Awareness {
		if !grantingStance(a.Stance) {
			continue
		}
		name, isPC := pcs[a.Knower]
		if !isPC {
			continue
		}
		if _, seen := granted[a.FactID]; !seen {
			granted[a.FactID] = name
		}
	}
	var out []campaign.Finding
	for _, a := range snap.Awareness {
		if a.Knower != campaign.PartyKnower || a.Stance != "unaware" {
			continue
		}
		f, ok := facts[a.FactID]
		if !ok {
			continue
		}
		if f.Confidence == campaign.ConfidenceProposed || f.SupersededBy != "" {
			continue // dead facts render nowhere
		}
		pc, renders := granted[f.ID]
		if !renders {
			continue
		}
		out = append(out, campaign.Finding{
			Check: CheckSpoilerLeak, Severity: campaign.SeverityError,
			RecordKind: "fact", RecordID: f.ID,
			Message: fmt.Sprintf("the party is recorded as unaware of %q, but %s's player-visible surface renders it — one of the two awareness rows is wrong",
				f.Statement, pc),
		})
	}
	return out
}

// checkKnowledgeBeforeDiscovery: an awareness row dated before the session
// that produced its discovery. Knowledge cannot predate the scene that
// created it; only an import or a bug writes this. Awareness rows are dated
// by their creation, sessions by their start (falling back to creation), and
// rows whose discovery carries no session are skipped — a discovery the DM
// typed at the table has no session to be dated against.
func checkKnowledgeBeforeDiscovery(snap *Snapshot) []campaign.Finding {
	discoveries := map[string]DiscoveryRow{}
	for _, d := range snap.Discoveries {
		discoveries[d.ID] = d
	}
	sessions := map[string]SessionRef{}
	for _, s := range snap.Sessions {
		sessions[s.ID] = s
	}
	var out []campaign.Finding
	for _, a := range snap.Awareness {
		if a.DiscoveryID == "" {
			continue
		}
		d, ok := discoveries[a.DiscoveryID]
		if !ok || d.SessionID == "" {
			continue
		}
		s, ok := sessions[d.SessionID]
		if !ok {
			continue // dangling sessions are dangling_reference's problem
		}
		if sessionDate := s.date(); a.CreatedAt.Before(sessionDate) {
			out = append(out, campaign.Finding{
				Check: CheckKnowledgeBeforeDiscovery, Severity: campaign.SeverityError,
				RecordKind: "awareness", RecordID: a.Knower + "/" + a.FactID,
				Message: fmt.Sprintf("%s's awareness of fact %s is dated before the session that produced its discovery (%s)",
					a.Knower, a.FactID, s.ID),
			})
		}
	}
	return out
}

// checkAwarenessWithoutSource: a knower holds a granting stance with no
// discovery trail behind it. RecordDiscovery always writes both, so a
// granting row with no matching discovery was written some other way — an
// API call, a backfill, a bug — and "why does Grimoire think Mira knows
// this?" has no answer. Deliberate unaware rows are fine: not knowing
// something needs no audit trail.
func checkAwarenessWithoutSource(snap *Snapshot) []campaign.Finding {
	// The discovery trail per (knower, fact): a character's own discovery or
	// the party's both explain a pc's row, the same way the character scope
	// reads its own rows plus the party's.
	trail := map[string]bool{}
	for _, d := range snap.Discoveries {
		trail[d.DiscoveredBy+"/"+d.FactID] = true
	}
	isPC := map[string]bool{}
	for _, e := range snap.Entities {
		if e.Kind == campaign.KindPC && e.Status != campaign.StatusDeleted {
			isPC[e.ID] = true
		}
	}
	explained := func(a AwarenessRow) bool {
		if trail[a.Knower+"/"+a.FactID] {
			return true
		}
		return isPC[a.Knower] && trail[campaign.PartyKnower+"/"+a.FactID]
	}
	var out []campaign.Finding
	for _, a := range snap.Awareness {
		if !grantingStance(a.Stance) {
			continue
		}
		if a.DiscoveryID != "" {
			continue // linked; knowledge_before_discovery owns dating
		}
		if explained(a) {
			continue
		}
		out = append(out, campaign.Finding{
			Check: CheckAwarenessWithoutSource, Severity: campaign.SeverityReview,
			RecordKind: "awareness", RecordID: a.Knower + "/" + a.FactID,
			Message: fmt.Sprintf("%s holds stance %s on fact %s with no discovery trail; record how they learned it",
				a.Knower, a.Stance, a.FactID),
		})
	}
	return out
}

// checkOrphanThread: a hook, secret or clue introduced N sessions ago and
// never referenced since. In this data model hooks, secrets and clues are
// secret-visibility facts — that is what the visibility column is for — so
// the thread set is the campaign's live, authoritative secrets. A thread is
// orphaned when the session that introduced it is at least OrphanSessions
// behind the table's latest session and nothing has ever touched it: no
// awareness row for any knower (not even a deliberate unaware), no discovery,
// no contradiction registration. Facts that cannot be session-dated (no
// session-cited provenance, or a citation that does not resolve) are skipped:
// "introduced N sessions ago" must mean something.
func checkOrphanThread(snap *Snapshot, opts CheckOptions) []campaign.Finding {
	if len(snap.Sessions) == 0 {
		return nil
	}
	ordinal := map[string]int64{}
	var maxOrdinal int64
	for _, s := range snap.Sessions {
		ordinal[s.ID] = s.Ordinal
		if s.Ordinal > maxOrdinal {
			maxOrdinal = s.Ordinal
		}
	}
	touched := map[string]bool{}
	for _, a := range snap.Awareness {
		touched[a.FactID] = true
	}
	for _, d := range snap.Discoveries {
		touched[d.FactID] = true
	}
	var out []campaign.Finding
	for _, f := range snap.Facts {
		if f.Visibility != campaign.VisibilitySecret || f.SupersededBy != "" {
			continue
		}
		if f.Confidence != campaign.ConfidenceCanon && f.Confidence != campaign.ConfidenceDerived {
			continue
		}
		if touched[f.ID] || snap.CoveredFacts[f.ID] {
			continue
		}
		intro, ok := snap.IntroducedSession[f.ID]
		if !ok {
			continue
		}
		if age := maxOrdinal - ordinal[intro]; age < int64(opts.OrphanSessions) {
			continue
		}
		out = append(out, campaign.Finding{
			Check: CheckOrphanThread, Severity: campaign.SeverityWarn,
			RecordKind: "fact", RecordID: f.ID,
			Message: fmt.Sprintf("secret thread %q was introduced %d sessions ago and nothing has referenced it since",
				f.Statement, maxOrdinal-ordinal[intro]),
		})
	}
	return out
}

// checkUnreachableSecret: a secret with no clue path any character can
// currently reach. Reachability is read off the awareness table the way the
// retrieval path grants: a secret someone holds (knows, suspects, or
// believes_false) is being reached, and a secret carrying even a deliberate
// unaware row has a modeled clue opportunity — the DM marked the moment the
// party walked past it. A secret with no awareness row at all has no path
// anyone can reach: the DM planted it and never placed a lead toward it. That
// is exactly the "what did we forget?" signal, and it costs a join.
func checkUnreachableSecret(snap *Snapshot) []campaign.Finding {
	anyRow := map[string]bool{}
	granting := map[string]bool{}
	for _, a := range snap.Awareness {
		anyRow[a.FactID] = true
		if grantingStance(a.Stance) {
			granting[a.FactID] = true
		}
	}
	var out []campaign.Finding
	for _, f := range snap.Facts {
		if f.Visibility != campaign.VisibilitySecret || f.SupersededBy != "" {
			continue
		}
		if f.Confidence != campaign.ConfidenceCanon && f.Confidence != campaign.ConfidenceDerived {
			continue
		}
		if anyRow[f.ID] || granting[f.ID] {
			continue
		}
		out = append(out, campaign.Finding{
			Check: CheckUnreachableSecret, Severity: campaign.SeverityWarn,
			RecordKind: "fact", RecordID: f.ID,
			Message: fmt.Sprintf("secret %q has no clue path any character can currently reach; plant a lead or an unaware marker",
				f.Statement),
		})
	}
	return out
}

// checkEncounters runs the two planned-encounter rules:
//
//   - stat_block_unresolved: a roster monster whose name resolves to nothing
//     in the local bestiary mirror. Skipped entirely while the mirror is
//     empty — an unsynced bestiary means "cannot resolve", not "does not
//     exist", and flagging every monster of every encounter would be noise.
//
//   - party_level_drift: an encounter whose difficulty band, recomputed under
//     the party as it stands today, no longer matches the band it was planned
//     against. Skipped when either side is unknown: an encounter that
//     recorded no party cannot drift, and a campaign whose pcs declare no
//     levels has no "as it stands today".
func checkEncounters(snap *Snapshot) []campaign.Finding {
	var out []campaign.Finding
	for _, e := range snap.Encounters {
		if len(snap.Bestiary) > 0 {
			var missing []string
			for _, m := range e.Monsters {
				if m.Name == "" {
					continue
				}
				if !snap.Bestiary[squashName(m.Name)] {
					missing = append(missing, m.Name)
				}
			}
			if len(missing) > 0 {
				out = append(out, campaign.Finding{
					Check: CheckStatBlockUnresolved, Severity: campaign.SeverityWarn,
					RecordKind: "session_event", RecordID: e.EventID,
					Message: fmt.Sprintf("planned encounter %q uses monster(s) with no bestiary entry: %s",
						e.Name, strings.Join(missing, ", ")),
				})
			}
		}
		if len(e.Party) > 0 && len(snap.Party) > 0 {
			planned := encounter.Evaluate(e.Party, e.Monsters)
			current := encounter.Evaluate(snap.Party, e.Monsters)
			if planned.Difficulty != current.Difficulty {
				out = append(out, campaign.Finding{
					Check: CheckPartyLevelDrift, Severity: campaign.SeverityWarn,
					RecordKind: "session_event", RecordID: e.EventID,
					Message: fmt.Sprintf("planned encounter %q was built as %s for its party but lands %s for the party as it stands today",
						e.Name, planned.Difficulty, current.Difficulty),
				})
			}
		}
	}
	return out
}
