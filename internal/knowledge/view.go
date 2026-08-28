package knowledge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

/*
The scoped read paths. Every query below joins the authorization into the SQL
(ADR 2): a row the scope may not see is never loaded, so no caller — and no
model downstream of a caller — can be handed one by mistake.

The shared CTE vocabulary:

	granted(fact_id)   facts a non-DM scope's knowers hold a granting stance
	                   on, and which are live (not proposed, not superseded);
	                   with playerStrict, only non-secret ones. At the DM
	                   scope it degenerates to every live fact the DM may read
	                   (proposed excluded), keeping one query shape.
	visible_events     events the knower witnessed: own participation, plus —
	                   for party and character scopes — any event a pc
	                   participated in.
	visible_entities   entities the knower has met: themselves, entities
	                   carried by granted facts, co-participants of visible
	                   events.

A fact is live when its confidence is not 'proposed' (invisible to every
perspective, the DM's chat included — ADR 3) and it has not been superseded
(retconned history is not current truth).

Conventions the model doc already implies and these queries rely on: entity
summaries and fact and event prose are written player-safe, and secrets live
in facts with visibility 'secret' — that is what the visibility column is
for. Entity payloads are DM structure (NPC agent fields, stat blocks) and are
dropped at every non-DM scope.
*/

// Column lists mirroring the campaign tables. internal/campaign owns the
// tables; this package owns the scoped reads and keeps its own scanners.
const factCols = `id, campaign_id, subject_entity, predicate, object_entity, object_literal,
                  statement, confidence, visibility, created_by, superseded_by, created_at`

const entityCols = `id, campaign_id, kind, name, summary, payload, status, created_at, updated_at`

const eventCols = `id, campaign_id, session_id, summary, clock_at, real_ordinal, location_entity, created_at`

const relCols = `id, from_entity, rel_type, to_entity, strength, justified_by_fact, since_event, created_at`

func scanFact(row interface{ Scan(...any) error }) (*campaign.Fact, error) {
	var (
		f             campaign.Fact
		objectEntity  sql.NullString
		objectLiteral sql.NullString
		supersededBy  sql.NullString
		createdMS     int64
	)
	if err := row.Scan(&f.ID, &f.CampaignID, &f.SubjectEntity, &f.Predicate, &objectEntity,
		&objectLiteral, &f.Statement, &f.Confidence, &f.Visibility, &f.CreatedBy,
		&supersededBy, &createdMS); err != nil {
		return nil, err
	}
	f.ObjectEntity = objectEntity.String
	f.ObjectLiteral = objectLiteral.String
	f.SupersededBy = supersededBy.String
	f.CreatedAt = time.UnixMilli(createdMS).UTC()
	return &f, nil
}

func scanEntity(row interface{ Scan(...any) error }, keepPayload bool) (*campaign.Entity, error) {
	var (
		e           campaign.Entity
		payloadJSON string
		createdMS   int64
		updatedMS   int64
	)
	if err := row.Scan(&e.ID, &e.CampaignID, &e.Kind, &e.Name, &e.Summary, &payloadJSON,
		&e.Status, &createdMS, &updatedMS); err != nil {
		return nil, err
	}
	if keepPayload && payloadJSON != "" && payloadJSON != "{}" {
		_ = json.Unmarshal([]byte(payloadJSON), &e.Payload)
	}
	if e.Payload == nil {
		e.Payload = map[string]any{}
	}
	e.CreatedAt = time.UnixMilli(createdMS).UTC()
	e.UpdatedAt = time.UnixMilli(updatedMS).UTC()
	return &e, nil
}

func scanEvent(row interface{ Scan(...any) error }) (*campaign.Event, error) {
	var (
		e        campaign.Event
		session  sql.NullString
		clock    sql.NullInt64
		location sql.NullString
		created  int64
	)
	if err := row.Scan(&e.ID, &e.CampaignID, &session, &e.Summary, &clock, &e.RealOrdinal,
		&location, &created); err != nil {
		return nil, err
	}
	e.SessionID = session.String
	if clock.Valid {
		v := clock.Int64
		e.ClockAt = &v
	}
	e.LocationEntity = location.String
	e.CreatedAt = time.UnixMilli(created).UTC()
	return &e, nil
}

func scanRelationship(row interface{ Scan(...any) error }) (*campaign.Relationship, error) {
	var (
		r         campaign.Relationship
		justified sql.NullString
		since     sql.NullString
		createdMS int64
	)
	if err := row.Scan(&r.ID, &r.FromEntity, &r.RelType, &r.ToEntity, &r.Strength,
		&justified, &since, &createdMS); err != nil {
		return nil, err
	}
	r.JustifiedByFact = justified.String
	r.SinceEvent = since.String
	r.CreatedAt = time.UnixMilli(createdMS).UTC()
	return &r, nil
}

// FactFilter narrows Facts. Zero values mean "no restriction". Stance
// filters the granting awareness row at non-DM scopes (knows, suspects or
// believes_false); at the DM scope, where reads are not grant-based, it is
// refused.
type FactFilter struct {
	SubjectEntity string
	ObjectEntity  string
	Predicate     string
	Stance        string
}

// acl is the per-query authorization, built once per read and rendered into
// the SQL fragments below. playerStrict is the PlayerView rule (ADR 6):
// secret-visibility facts are excluded even when granted.
type acl struct {
	campaignID   string
	dm           bool
	playerStrict bool
	knowers      []string
	ownEntityIDs []string // scope-bound entity ids, for participant joins
	includeParty bool     // party and character scopes see pc-witnessed events
}

func (s *Store) newACL(ctx context.Context, scope Scope, campaignID string, playerStrict bool) (*acl, error) {
	if err := s.resolveScope(ctx, scope, campaignID); err != nil {
		return nil, err
	}
	a := &acl{campaignID: campaignID, dm: scope.IsDM(), playerStrict: playerStrict}
	switch scope.Kind() {
	case campaign.ScopeKindParty:
		a.knowers = []string{campaign.PartyKnower}
		a.includeParty = true
	case campaign.ScopeKindCharacter:
		a.knowers = []string{scope.EntityID(), campaign.PartyKnower}
		a.ownEntityIDs = []string{scope.EntityID()}
		a.includeParty = true
	case campaign.ScopeKindNPC:
		a.knowers = []string{scope.EntityID()}
		a.ownEntityIDs = []string{scope.EntityID()}
	}
	return a, nil
}

// grantedCTE renders the granted CTE and its args. Liveness — not proposed,
// not superseded — is enforced here for every non-DM grant, strict or not:
// the write path permits awareness rows on facts that are later retconned
// (learn, then the DM rewrites history) or proposed while an extraction
// pipeline records who would hold them, and a grant on a dead fact must not
// surface it anywhere, list or single fetch.
func (a *acl) grantedCTE() (string, []any) {
	if a.dm {
		q := `granted AS (
			SELECT f.id AS fact_id FROM facts f
			 WHERE f.campaign_id = ? AND f.confidence <> 'proposed' AND f.superseded_by IS NULL`
		args := []any{a.campaignID}
		if a.playerStrict {
			q += ` AND f.visibility <> 'secret'`
		}
		return q + `)`, args
	}
	q := `granted AS (
		SELECT DISTINCT a.fact_id FROM awareness a
		 JOIN facts pf ON pf.id = a.fact_id AND pf.campaign_id = a.campaign_id
		   AND pf.confidence <> 'proposed' AND pf.superseded_by IS NULL`
	args := []any{}
	if a.playerStrict {
		q += ` AND pf.visibility <> 'secret'`
	}
	q += `
		 WHERE a.campaign_id = ? AND a.knower IN ` + grantPlaceholders(len(a.knowers)) + `
		   AND a.stance IN ('knows','suspects','believes_false'))`
	args = append(args, a.campaignID)
	for _, k := range a.knowers {
		args = append(args, k)
	}
	return q, args
}

// eventsCTE renders the visible_events CTE and its args.
func (a *acl) eventsCTE() (string, []any) {
	if a.dm {
		return `visible_events AS (
			SELECT id AS event_id FROM events WHERE campaign_id = ?)`, []any{a.campaignID}
	}
	if a.includeParty {
		q := `visible_events AS (
			SELECT DISTINCT ep.event_id FROM event_participants ep
			 JOIN entities pe ON pe.id = ep.entity_id AND pe.campaign_id = ?
			 WHERE (pe.kind = 'pc'`
		args := []any{a.campaignID}
		if len(a.ownEntityIDs) > 0 {
			q += ` OR ep.entity_id IN ` + grantPlaceholders(len(a.ownEntityIDs))
			for _, id := range a.ownEntityIDs {
				args = append(args, id)
			}
		}
		return q + `))`, args
	}
	// npc scope: only the events that entity personally witnessed
	return `visible_events AS (
		SELECT DISTINCT ep.event_id FROM event_participants ep
		 JOIN entities pe ON pe.id = ep.entity_id AND pe.campaign_id = ?
		 WHERE ep.entity_id IN ` + grantPlaceholders(len(a.ownEntityIDs)) + `)`,
		append([]any{a.campaignID}, anySlice(a.ownEntityIDs)...)
}

// entitiesCTE renders the visible_entities CTE and its args. Depends on the
// granted and visible_events CTEs appearing earlier in the statement.
func (a *acl) entitiesCTE() (string, []any) {
	q := `visible_entities AS (
		SELECT e.id FROM entities e
		 WHERE e.campaign_id = ? AND e.status <> 'deleted'`
	args := []any{a.campaignID}
	if !a.dm {
		q += ` AND (
			  e.id IN ` + grantPlaceholders(max(1, len(a.ownEntityIDs)))
		if len(a.ownEntityIDs) == 0 {
			args = append(args, "") // party knower: no own entity id — matches nothing
		} else {
			args = append(args, anySlice(a.ownEntityIDs)...)
		}
		q += `
			  OR EXISTS (SELECT 1 FROM granted g
			              JOIN facts gf ON gf.id = g.fact_id
			             WHERE gf.subject_entity = e.id OR gf.object_entity = e.id)
			  OR EXISTS (SELECT 1 FROM event_participants ep
			             WHERE ep.entity_id = e.id
			               AND ep.event_id IN (SELECT event_id FROM visible_events))
			  OR EXISTS (SELECT 1 FROM events le
			             WHERE le.id IN (SELECT event_id FROM visible_events)
			               AND le.location_entity = e.id))`
	}
	return q + `)`, args
}

// relsCTE renders the visible relationship ids. An edge is visible when it
// was established at an event the knower witnessed, or when it is one of the
// unambiguously observable structural edges — met someone, sits inside
// somewhere — between two visible entities. justified_by_fact deliberately
// grants nothing: it is provenance for the DM ("this edge is derived from
// that fact"), not a knowledge transfer — the fact itself is what Facts
// gates, and a believes_false grant on a justification must not hand a
// perspective the structural edge the DM hung off it. Everything else
// (owns, serves, member_of, worships, betrayed, secretly_controls, ...) is
// asserted DM structure and stays DM-only.
func (a *acl) relsCTE() (string, []any) {
	if a.dm {
		return `visible_rels AS (
			SELECT r.id FROM relationships r
			 WHERE r.from_entity IN (SELECT id FROM entities WHERE campaign_id = ?))`,
			[]any{a.campaignID}
	}
	return `visible_rels AS (
		SELECT r.id FROM relationships r
		 WHERE r.from_entity IN (SELECT id FROM entities WHERE campaign_id = ` + sqlLiteral(a.campaignID) + `)
		   AND (
			r.since_event IS NOT NULL AND r.since_event IN (SELECT event_id FROM visible_events)
			OR (r.rel_type IN ('knows','located_in','contains')
			    AND r.from_entity IN (SELECT id FROM visible_entities)
			    AND r.to_entity IN (SELECT id FROM visible_entities))))`, nil
}

// sqlLiteral renders a SQL string literal. Used for the campaign id only —
// a server-generated uuid, never user text — where threading a placeholder
// through three stacked CTE builders would buy nothing.
func sqlLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func anySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

/* ---------- scoped reads ---------- */

// Facts returns the facts a scope may read, newest first. The DM scope reads
// every live fact; a non-DM scope reads exactly what its knowers' awareness
// grants — including a secret the knower has discovered, which is the point
// of discovery, and never a proposed fact under any scope.
func (s *Store) facts(ctx context.Context, scope Scope, campaignID string, filter FactFilter, strict bool) ([]campaign.Fact, error) {
	a, err := s.newACL(ctx, scope, campaignID, strict)
	if err != nil {
		return nil, err
	}
	if filter.Stance != "" {
		if !grantingStances[filter.Stance] {
			return nil, fmt.Errorf("%w: stance filter %q is not a granting stance", ErrInvalid, filter.Stance)
		}
		if a.dm {
			return nil, fmt.Errorf("%w: stance filtering is a non-DM concern; the dm scope reads facts directly", ErrInvalid)
		}
	}
	join := ""
	joinArgs := []any{}
	if !a.dm {
		join = ` JOIN awareness va ON va.fact_id = f.id AND va.campaign_id = f.campaign_id
		          AND va.knower IN ` + grantPlaceholders(len(a.knowers)) + `
		          AND va.stance IN ('knows','suspects','believes_false')`
		for _, k := range a.knowers {
			joinArgs = append(joinArgs, k)
		}
		if filter.Stance != "" {
			join += ` AND va.stance = ` + sqlLiteral(filter.Stance)
		}
	}
	q, args := a.grantedCTE()
	full := `WITH ` + q + `
		SELECT DISTINCT f.id, f.campaign_id, f.subject_entity, f.predicate, f.object_entity, f.object_literal,
		                 f.statement, f.confidence, f.visibility, f.created_by, f.superseded_by, f.created_at
		  FROM facts f` + join + `
		 WHERE f.campaign_id = ? AND f.id IN (SELECT fact_id FROM granted)`
	if !a.dm {
		// The granted CTE already enforces liveness on every grant path;
		// this second filter is kept deliberately, the same defense in
		// depth the stance filter has at both enforcement points.
		full += ` AND f.confidence <> 'proposed' AND f.superseded_by IS NULL`
	}
	// Placeholder order matches SQL order: CTE args, then the join's
	// knower placeholders, then the outer WHERE.
	args = append(args, joinArgs...)
	args = append(args, a.campaignID)
	if filter.SubjectEntity != "" {
		full += ` AND f.subject_entity = ?`
		args = append(args, filter.SubjectEntity)
	}
	if filter.ObjectEntity != "" {
		full += ` AND f.object_entity = ?`
		args = append(args, filter.ObjectEntity)
	}
	if filter.Predicate != "" {
		full += ` AND f.predicate = ?`
		args = append(args, filter.Predicate)
	}
	full += ` ORDER BY f.created_at DESC, f.id`
	rows, err := s.db.QueryContext(ctx, full, args...)
	if err != nil {
		return nil, fmt.Errorf("scoped facts: %w", err)
	}
	defer rows.Close()
	var out []campaign.Fact
	for rows.Next() {
		f, err := scanFact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

// Fact returns one fact if the scope may read it. A fact the scope has not
// been granted is indistinguishable from a missing one.
func (s *Store) fact(ctx context.Context, scope Scope, campaignID, factID string, strict bool) (*campaign.Fact, error) {
	a, err := s.newACL(ctx, scope, campaignID, strict)
	if err != nil {
		return nil, err
	}
	q, args := a.grantedCTE()
	full := `WITH ` + q + `
		SELECT ` + factCols + ` FROM facts f
		 WHERE f.campaign_id = ? AND f.id IN (SELECT fact_id FROM granted) AND f.id = ?`
	args = append(args, a.campaignID, factID)
	row := s.db.QueryRowContext(ctx, full, args...)
	f, err := scanFact(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: fact %s", ErrNotFound, factID)
	}
	return f, err
}

// Entities returns the entities a scope may read, ordered by name. The DM
// reads all (payload attached); a non-DM scope reads the entities it has met
// — itself, entities carried by granted facts, co-participants of witnessed
// events — with entity payloads dropped: payloads are DM structure, not
// player-facing prose.
func (s *Store) entities(ctx context.Context, scope Scope, campaignID, kind string, strict bool) ([]campaign.Entity, error) {
	a, err := s.newACL(ctx, scope, campaignID, strict)
	if err != nil {
		return nil, err
	}
	gq, gargs := a.grantedCTE()
	eq, eargs := a.eventsCTE()
	vq, vargs := a.entitiesCTE()
	full := `WITH ` + gq + `, ` + eq + `, ` + vq + `
		SELECT ` + entityCols + ` FROM entities e
		 WHERE e.id IN (SELECT id FROM visible_entities)`
	args := append(append(append([]any{}, gargs...), eargs...), vargs...)
	if kind != "" {
		if kind != campaign.KindPC && kind != campaign.KindNPC && kind != campaign.KindFaction &&
			kind != campaign.KindLocation && kind != campaign.KindItem && kind != campaign.KindDeity &&
			kind != campaign.KindOrganization && kind != campaign.KindCreature && kind != campaign.KindConcept {
			return nil, fmt.Errorf("%w: entity kind %q", ErrInvalid, kind)
		}
		full += ` AND e.kind = ?`
		args = append(args, kind)
	}
	full += ` ORDER BY e.name COLLATE NOCASE, e.id`
	rows, err := s.db.QueryContext(ctx, full, args...)
	if err != nil {
		return nil, fmt.Errorf("scoped entities: %w", err)
	}
	defer rows.Close()
	var out []campaign.Entity
	for rows.Next() {
		e, err := scanEntity(rows, a.dm)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// Entity returns one entity if the scope may read it, payload kept only at
// the DM scope.
func (s *Store) entity(ctx context.Context, scope Scope, campaignID, id string, strict bool) (*campaign.Entity, error) {
	a, err := s.newACL(ctx, scope, campaignID, strict)
	if err != nil {
		return nil, err
	}
	gq, gargs := a.grantedCTE()
	eq, eargs := a.eventsCTE()
	vq, vargs := a.entitiesCTE()
	full := `WITH ` + gq + `, ` + eq + `, ` + vq + `
		SELECT ` + entityCols + ` FROM entities e
		 WHERE e.id IN (SELECT id FROM visible_entities) AND e.id = ?`
	args := append(append(append(append([]any{}, gargs...), eargs...), vargs...), id)
	row := s.db.QueryRowContext(ctx, full, args...)
	e, err := scanEntity(row, a.dm)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: entity %s", ErrNotFound, id)
	}
	return e, err
}

// Timeline returns the events the scope witnessed, in play order, with
// participants attached (they were there too). Causal event links are DM
// material — "revealed" edges especially — and are attached only at the DM
// scope.
func (s *Store) timeline(ctx context.Context, scope Scope, campaignID string, strict bool) ([]campaign.Event, error) {
	a, err := s.newACL(ctx, scope, campaignID, strict)
	if err != nil {
		return nil, err
	}
	gq, gargs := a.grantedCTE()
	eq, eargs := a.eventsCTE()
	full := `WITH ` + gq + `, ` + eq + `
		SELECT ` + eventCols + ` FROM events ev
		 WHERE ev.id IN (SELECT event_id FROM visible_events)
		 ORDER BY ev.real_ordinal`
	args := append(append([]any{}, gargs...), eargs...)
	rows, err := s.db.QueryContext(ctx, full, args...)
	if err != nil {
		return nil, fmt.Errorf("scoped timeline: %w", err)
	}
	defer rows.Close()
	var out []campaign.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if err := s.attachEventDetail(ctx, &out[i], a.dm); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// attachEventDetail loads participants for every scope and causal links for
// the DM only.
func (s *Store) attachEventDetail(ctx context.Context, e *campaign.Event, links bool) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, event_id, entity_id, role FROM event_participants WHERE event_id = ? ORDER BY entity_id`, e.ID)
	if err != nil {
		return fmt.Errorf("load participants: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p campaign.EventParticipant
		if err := rows.Scan(&p.ID, &p.EventID, &p.EntityID, &p.Role); err != nil {
			return err
		}
		e.Participants = append(e.Participants, p)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !links {
		return nil
	}
	rows, err = s.db.QueryContext(ctx,
		`SELECT id, from_event, to_event, link FROM event_links
		 WHERE from_event = ? OR to_event = ? ORDER BY from_event, to_event`, e.ID, e.ID)
	if err != nil {
		return fmt.Errorf("load links: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var l campaign.EventLinkRef
		if err := rows.Scan(&l.ID, &l.FromEvent, &l.ToEvent, &l.Link); err != nil {
			return err
		}
		e.Links = append(e.Links, l)
	}
	return rows.Err()
}

// Relationships returns the edges a scope may read (see relsCTE for what
// makes an edge visible). The DM reads all.
func (s *Store) relationships(ctx context.Context, scope Scope, campaignID string, strict bool) ([]campaign.Relationship, error) {
	a, err := s.newACL(ctx, scope, campaignID, strict)
	if err != nil {
		return nil, err
	}
	gq, gargs := a.grantedCTE()
	eq, eargs := a.eventsCTE()
	vq, vargs := a.entitiesCTE()
	rq, rargs := a.relsCTE()
	full := `WITH ` + gq + `, ` + eq + `, ` + vq + `, ` + rq + `
		SELECT ` + relCols + ` FROM relationships r
		 WHERE r.id IN (SELECT id FROM visible_rels)
		 ORDER BY r.created_at, r.id`
	args := append(append(append(append([]any{}, gargs...), eargs...), vargs...), rargs...)
	rows, err := s.db.QueryContext(ctx, full, args...)
	if err != nil {
		return nil, fmt.Errorf("scoped relationships: %w", err)
	}
	defer rows.Close()
	var out []campaign.Relationship
	for rows.Next() {
		r, err := scanRelationship(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// ProseHit is one full-text match over campaign prose: an entity, a fact or
// an event, the matched text, and — for fact hits — the visibility and
// confidence that let callers (and the leak test) see what class of fact
// surfaced.
type ProseHit struct {
	Kind       string // entity | fact | event
	RefID      string
	Snippet    string
	Visibility string // fact hits only
	Confidence string // fact hits only
}

// ErrEmptyQuery is returned when a prose search sanitizes to nothing.
var ErrEmptyQuery = errors.New("empty query")

// proseStopwords are dropped before matching. Questions are conversational
// ("where is the Black Sun headquarters?") and AND-ing "where*" against a
// fact's statement would starve the match; the nouns carry the query.
var proseStopwords = map[string]bool{
	"a": true, "an": true, "the": true, "this": true, "that": true,
	"these": true, "those": true, "is": true, "are": true, "was": true,
	"were": true, "be": true, "been": true, "am": true, "do": true,
	"does": true, "did": true, "of": true, "in": true, "on": true,
	"at": true, "to": true, "for": true, "from": true, "with": true,
	"about": true, "and": true, "or": true, "not": true, "no": true,
	"what": true, "where": true, "who": true, "whom": true, "whose": true,
	"which": true, "why": true, "how": true, "when": true,
	"can": true, "could": true, "should": true, "would": true, "will": true,
	"i": true, "we": true, "you": true, "they": true, "it": true,
	"he": true, "she": true, "me": true, "us": true, "them": true,
	"my": true, "our": true, "your": true, "their": true, "its": true,
	"his": true, "her": true, "there": true, "here": true,
	"know": true, "knows": true, "known": true, "tell": true, "say": true,
	"says": true, "said": true, "any": true, "some": true, "all": true,
}

// proseTokens trims and stopword-filters the query into matchable tokens.
// Possessives split on the apostrophe ("Duke's" → "duke") — the FTS phrase
// for "Duke's" is two tokens and prefix-matches nothing a table full of
// "Vane's" and "the party's" would need to.
func proseTokens(q string) []string {
	var toks []string
	for _, tok := range strings.Fields(q) {
		for _, part := range strings.FieldsFunc(strings.Trim(tok, "\"'*()?,.;:!"), func(r rune) bool {
			return r == '\'' || r == '’'
		}) {
			if len(part) <= 1 || proseStopwords[strings.ToLower(part)] {
				continue
			}
			toks = append(toks, part)
		}
	}
	return toks
}

func prefixPhrases(tokens []string) []string {
	parts := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		parts = append(parts, fmt.Sprintf("%q*", tok))
	}
	return parts
}

// toFTSQuery converts free text into a safe FTS5 MATCH expression: each token
// a prefix phrase, tokens AND-ed — the same shape internal/index uses for its
// rules corpora.
func toFTSQuery(q string) (string, error) {
	parts := prefixPhrases(proseTokens(q))
	if len(parts) == 0 {
		return "", ErrEmptyQuery
	}
	return strings.Join(parts, " "), nil
}

// toFTSQueryRelaxed is the fallback when the AND query finds nothing: tokens
// OR-ed, so a question sharing no full keyword set with any one row still
// surfaces its best matches by rank. The authorization is untouched — only
// the match expression widens; a row the scope cannot read stays unreadable
// whichever expression runs.
func toFTSQueryRelaxed(q string) (string, error) {
	parts := prefixPhrases(proseTokens(q))
	if len(parts) == 0 {
		return "", ErrEmptyQuery
	}
	return strings.Join(parts, " OR "), nil
}

// SearchProse runs full-text search over campaign prose — entity names and
// summaries, fact statements, event summaries — restricted to rows the scope
// may read. The authorization rides the same CTEs as the structured reads,
// in the same SQL statement as the MATCH: a secret fact's text is indexed,
// but a scope that cannot read the fact cannot surface its snippet.
func (s *Store) searchProse(ctx context.Context, scope Scope, campaignID, query string, limit int, strict, relaxed bool) ([]ProseHit, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	match, err := matchExpr(query, relaxed)
	if err != nil {
		return nil, err
	}
	a, err := s.newACL(ctx, scope, campaignID, strict)
	if err != nil {
		return nil, err
	}
	gq, gargs := a.grantedCTE()
	eq, eargs := a.eventsCTE()
	vq, vargs := a.entitiesCTE()
	full := `WITH ` + gq + `, ` + eq + `, ` + vq + `
		SELECT p.kind, p.ref_id, snippet(campaign_prose, 0, '', '', ' … ', 12),
		       COALESCE((SELECT f.visibility FROM facts f WHERE f.id = p.ref_id), ''),
		       COALESCE((SELECT f.confidence FROM facts f WHERE f.id = p.ref_id), '')
		  FROM campaign_prose p
		 WHERE campaign_prose MATCH ?
		   AND p.campaign_id = ?
		   AND CASE p.kind
		         WHEN 'entity' THEN p.ref_id IN (SELECT id FROM visible_entities)
		         WHEN 'fact'   THEN p.ref_id IN (SELECT fact_id FROM granted)
		         WHEN 'event'  THEN p.ref_id IN (SELECT event_id FROM visible_events)
		         ELSE 0 END
		 ORDER BY rank
		 LIMIT ?`
	args := append(append(append(append([]any{}, gargs...), eargs...), vargs...), match, a.campaignID, limit)
	rows, err := s.db.QueryContext(ctx, full, args...)
	if err != nil {
		return nil, fmt.Errorf("scoped prose search: %w", err)
	}
	defer rows.Close()
	var out []ProseHit
	for rows.Next() {
		var h ProseHit
		if err := rows.Scan(&h.Kind, &h.RefID, &h.Snippet, &h.Visibility, &h.Confidence); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// matchExpr builds the FTS5 MATCH expression for one search: the strict AND
// form, or the relaxed OR form used as its fallback.
func matchExpr(query string, relaxed bool) (string, error) {
	if relaxed {
		return toFTSQueryRelaxed(query)
	}
	return toFTSQuery(query)
}

/* ---------- exported wrappers ---------- */

// Facts returns the facts a scope may read, newest first. The DM scope reads
// every live fact; a non-DM scope reads exactly what its knowers' awareness
// grants — including a secret the knower has discovered, which is the point
// of discovery, and never a proposed fact under any scope.
func (s *Store) Facts(ctx context.Context, scope Scope, campaignID string, filter FactFilter) ([]campaign.Fact, error) {
	return s.facts(ctx, scope, campaignID, filter, false)
}

// Fact returns one fact if the scope may read it. A fact the scope has not
// been granted is indistinguishable from a missing one.
func (s *Store) Fact(ctx context.Context, scope Scope, campaignID, factID string) (*campaign.Fact, error) {
	return s.fact(ctx, scope, campaignID, factID, false)
}

// Entities returns the entities a scope may read, ordered by name, with
// entity payloads dropped at every non-DM scope.
func (s *Store) Entities(ctx context.Context, scope Scope, campaignID, kind string) ([]campaign.Entity, error) {
	return s.entities(ctx, scope, campaignID, kind, false)
}

// Entity returns one entity if the scope may read it.
func (s *Store) Entity(ctx context.Context, scope Scope, campaignID, id string) (*campaign.Entity, error) {
	return s.entity(ctx, scope, campaignID, id, false)
}

// Timeline returns the events the scope witnessed, in play order.
func (s *Store) Timeline(ctx context.Context, scope Scope, campaignID string) ([]campaign.Event, error) {
	return s.timeline(ctx, scope, campaignID, false)
}

// Relationships returns the edges a scope may read.
func (s *Store) Relationships(ctx context.Context, scope Scope, campaignID string) ([]campaign.Relationship, error) {
	return s.relationships(ctx, scope, campaignID, false)
}

// SearchProse runs full-text search over campaign prose, restricted to rows
// the scope may read.
func (s *Store) SearchProse(ctx context.Context, scope Scope, campaignID, query string, limit int) ([]ProseHit, error) {
	return s.searchProse(ctx, scope, campaignID, query, limit, false, false)
}

// SearchProseRelaxed is the ranked fallback when SearchProse's AND match
// finds nothing: tokens OR-ed, best matches first. The scope's authorization
// is identical — only the match expression widens.
func (s *Store) SearchProseRelaxed(ctx context.Context, scope Scope, campaignID, query string, limit int) ([]ProseHit, error) {
	return s.searchProse(ctx, scope, campaignID, query, limit, false, true)
}

// FactionFacade returns a faction's player-facing self-presentation from its
// payload's agent block: the public face and the reputation, and nothing
// else. The entity must be visible at the scope; every other payload field —
// PrivateTruth above all — is DM structure this method has no way to return,
// which is the whole point: the player dossier reads a faction's face
// without the payload ever crossing the scope line.
func (s *Store) FactionFacade(ctx context.Context, scope Scope, campaignID, id string) (face, reputation string, err error) {
	e, err := s.entity(ctx, scope, campaignID, id, true)
	if err != nil {
		return "", "", err
	}
	if e.Kind != campaign.KindFaction {
		return "", "", fmt.Errorf("%w: %s is a %s, not a faction", ErrInvalid, id, e.Kind)
	}
	var payload string
	if err := s.db.QueryRowContext(ctx,
		`SELECT payload FROM entities WHERE id = ? AND campaign_id = ?`, id, campaignID).Scan(&payload); err != nil {
		return "", "", fmt.Errorf("faction facade: %w", err)
	}
	var payloadBlock struct {
		Agent struct {
			PublicFace string `json:"public_face"`
			Reputation string `json:"reputation"`
		} `json:"agent"`
	}
	// A payload without an agent block renders as an unwritten face, not an
	// error: a faction with no authored interior still has a dossier.
	_ = json.Unmarshal([]byte(payload), &payloadBlock)
	return strings.Join(strings.Fields(payloadBlock.Agent.PublicFace), " "),
		strings.Join(strings.Fields(payloadBlock.Agent.Reputation), " "), nil
}
