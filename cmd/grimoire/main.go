// Command grimoire runs the MTG + D&D rules reference server.
//
// Usage:
//
//	grimoire serve          serve the UI + API (builds the index on first run)
//	grimoire index          (re)build the search index and exit
//	grimoire migrate status show which schema migrations have been applied
//	grimoire migrate up     apply pending schema migrations and exit
//	grimoire migrate down   roll back the most recent migration and exit
//	grimoire campaign check    run the campaign-graph integrity checks
//	grimoire campaign plans    list a campaign's faction plans
//	grimoire campaign simulate preview a simulation tick — advance the world by N days
//	grimoire canon check       run the canon engine's deterministic checks
//	grimoire homebrew lint     lint homebrew monsters and items — a reviewer, not a referee
//	grimoire [-h]           help
//
// Configuration is via environment variables (see .env.example):
//
//	GRIMOIRE_ADDR       listen address for serve (default :8080)
//	GRIMOIRE_DB         SQLite index path (default data/grimoire.db)
//	GRIMOIRE_SESSION_TTL       session lifetime, Go duration (default 720h)
//	GRIMOIRE_OPEN_REGISTRATION keep account creation open after the first
//	                           account exists (default false)
//	GRIMOIRE_INVITE_TTL        how long admin invite links stay usable, Go
//	                           duration (default 168h / 7d; 0 = never expire)
//	GRIMOIRE_ANSWER_CACHE_TTL  how long cached Q&A answers stay fresh, Go
//	                           duration (default 168h / 7d)
//	ANTHROPIC_BASE_URL  LLM endpoint (default https://api.anthropic.com; z.ai: https://api.z.ai/api/anthropic)
//	ANTHROPIC_API_KEY   LLM secret key (enables the Q&A chat)
//	ANTHROPIC_MODEL     model name (e.g. glm-4.6, claude-3-5-sonnet-20241022)
//	ANTHROPIC_FALLBACK_BASE_URL, ANTHROPIC_FALLBACK_API_KEY, ANTHROPIC_FALLBACK_MODEL
//	                    standby provider used when the primary fails (out of
//	                    quota, bad key, overloaded, unreachable). Base URL
//	                    defaults to Anthropic's, model to ANTHROPIC_MODEL.
//	                    Further rungs: ANTHROPIC_FALLBACK_2_*, _3_, ...
//	EMBEDDINGS_BASE_URL OpenAI-compatible embeddings endpoint (default https://api.openai.com/v1)
//	EMBEDDINGS_API_KEY  embeddings secret key (enables semantic retrieval when set with EMBEDDINGS_MODEL)
//	EMBEDDINGS_MODEL    embeddings model name (e.g. text-embedding-3-small)
//	TRANSCRIBE_BASE_URL OpenAI-compatible transcription endpoint (default https://api.openai.com/v1;
//	                    a local whisper server, e.g. the compose "transcribe" profile, works too)
//	TRANSCRIBE_API_KEY  transcription secret key — optional, local backends need none
//	TRANSCRIBE_MODEL    transcription model name — required; unset means the audio
//	                    upload affordance is simply not there (no degraded path)
//	SCRYFALL_BASE_URL   MTG card lookup endpoint (default https://api.scryfall.com; no key needed)
//	MTG_RULES_URL       override MTG comp rules source
//	MTGJSON_URL         override MTGJSON AtomicCards source (card-name dictionary)
//	OPEN5E_BASE_URL     D&D entity lookup endpoint (default https://api.open5e.com; no key needed)
//	DND_REPO            override D&D SRD repo "owner/name"
//	DND_REF             override D&D SRD git ref
//	DND_DOCS_DIR        local D&D documents (markdown/text) imported alongside the SRD
//	GRIMOIRE_EDHREC     enable EDHREC enrichment for the deck builder (default false).
//	                    Unofficial Next.js data routes; cached on disk, ~1 req/s.
//	GRIMOIRE_EDHREC_CACHE_DIR  where the EDHREC cache lives (default data/edhrec-cache)
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/cache"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/carddb"
	"github.com/madeofpendletonwool/grimoire/internal/cards"
	"github.com/madeofpendletonwool/grimoire/internal/chat"
	"github.com/madeofpendletonwool/grimoire/internal/data"
	"github.com/madeofpendletonwool/grimoire/internal/deck"
	"github.com/madeofpendletonwool/grimoire/internal/downtime"
	"github.com/madeofpendletonwool/grimoire/internal/edhrec"
	"github.com/madeofpendletonwool/grimoire/internal/embeddings"
	"github.com/madeofpendletonwool/grimoire/internal/encounter"
	"github.com/madeofpendletonwool/grimoire/internal/entities"
	"github.com/madeofpendletonwool/grimoire/internal/faction"
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
	"github.com/madeofpendletonwool/grimoire/internal/homebrew"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/items"
	"github.com/madeofpendletonwool/grimoire/internal/journey"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/ledger"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
	"github.com/madeofpendletonwool/grimoire/internal/rulings"
	"github.com/madeofpendletonwool/grimoire/internal/server"
	"github.com/madeofpendletonwool/grimoire/internal/share"
	"github.com/madeofpendletonwool/grimoire/internal/sim"
	"github.com/madeofpendletonwool/grimoire/internal/story"
	"github.com/madeofpendletonwool/grimoire/internal/study"
	"github.com/madeofpendletonwool/grimoire/internal/transcribe"
	"github.com/madeofpendletonwool/grimoire/internal/uistate"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help") {
		usage()
		return
	}

	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	switch cmd {
	case "serve":
		if err := runServe(); err != nil {
			log.Fatalf("serve: %v", err)
		}
	case "index":
		if err := runIndex(); err != nil {
			log.Fatalf("index: %v", err)
		}
	case "migrate":
		if err := runMigrate(os.Args[2:]); err != nil {
			log.Fatalf("migrate: %v", err)
		}
	case "campaign":
		if err := runCampaign(os.Args[2:]); err != nil {
			log.Fatalf("campaign: %v", err)
		}
	case "canon":
		if err := runCanon(os.Args[2:]); err != nil {
			log.Fatalf("canon: %v", err)
		}
	case "homebrew":
		if err := runHomebrew(os.Args[2:]); err != nil {
			log.Fatalf("homebrew: %v", err)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `grimoire — MTG + D&D rules reference

Usage:
  grimoire serve     serve the UI + API (builds the index on first run)
  grimoire index     (re)build the search index and exit
  grimoire migrate status|up|down   inspect or move the database schema
  grimoire campaign check [id]      run campaign integrity checks
  grimoire campaign plans [id]      list a campaign's faction plans
  grimoire campaign simulate <id> --days 14 --seed N   preview a simulation tick, offline
  grimoire canon check [id]                 run the deterministic canon checks
  grimoire canon continuity [flags] <id> <prep.json>   pre-session continuity check
  grimoire canon entail [flags] <id> <prose.txt>       entailment pass over prose
  grimoire canon health [flags] [id]          the campaign health report
  grimoire homebrew lint [monster|item] [id...]   lint homebrew — findings, never a verdict

  continuity/entail/health flags: --offline  skip the model pass, deterministic only
  homebrew lint: with no ids, lints every homebrew record; with a kind,
  restricts the shelf; the model pass runs only when chat is configured.

Env (see .env.example):
  GRIMOIRE_ADDR, GRIMOIRE_DB, GRIMOIRE_SESSION_TTL, GRIMOIRE_OPEN_REGISTRATION,
  GRIMOIRE_INVITE_TTL, GRIMOIRE_ANSWER_CACHE_TTL, ANTHROPIC_BASE_URL, ANTHROPIC_API_KEY, ANTHROPIC_MODEL,
  ANTHROPIC_FALLBACK_BASE_URL, ANTHROPIC_FALLBACK_API_KEY, ANTHROPIC_FALLBACK_MODEL (and _2_, _3_, ...),
  EMBEDDINGS_BASE_URL, EMBEDDINGS_API_KEY, EMBEDDINGS_MODEL,
  TRANSCRIBE_BASE_URL, TRANSCRIBE_API_KEY, TRANSCRIBE_MODEL,
  SCRYFALL_BASE_URL, MTG_RULES_URL, MTGJSON_URL, OPEN5E_BASE_URL, DND_REPO, DND_REF, DND_DOCS_DIR,
  GRIMOIRE_EDHREC, GRIMOIRE_EDHREC_CACHE_DIR`)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func dbPath() string { return env("GRIMOIRE_DB", "data/grimoire.db") }
func addr() string   { return env("GRIMOIRE_ADDR", ":8080") }

// sessionTTL reads the session lifetime. An unparseable value falls back to the
// default rather than failing the boot — being logged out is a nuisance, a
// server that refuses to start over a typo is worse.
func sessionTTL() time.Duration {
	raw := os.Getenv("GRIMOIRE_SESSION_TTL")
	if raw == "" {
		return auth.DefaultSessionTTL
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Printf("GRIMOIRE_SESSION_TTL=%q is not a valid duration — using %s", raw, auth.DefaultSessionTTL)
		return auth.DefaultSessionTTL
	}
	return d
}

func openRegistration() bool {
	switch strings.ToLower(os.Getenv("GRIMOIRE_OPEN_REGISTRATION")) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// inviteTTL reads how long a freshly minted invite link stays usable. An empty
// value picks the default (7d); a value of zero ("0s") means invites never
// expire. An unparseable value falls back to the default — a burned invite that
// a friend has to ask about again is a nuisance, a server that refuses to start
// over a typo is worse.
func inviteTTL() time.Duration {
	raw := os.Getenv("GRIMOIRE_INVITE_TTL")
	if raw == "" {
		return auth.DefaultInviteTTL
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		log.Printf("GRIMOIRE_INVITE_TTL=%q is not a valid duration — using %s", raw, auth.DefaultInviteTTL)
		return auth.DefaultInviteTTL
	}
	if d < 0 {
		return 0 // negative is treated as "never expire", same as zero
	}
	return d
}

// answerCacheTTL reads how long a cached Q&A answer stays fresh. An unparseable
// or non-positive value falls back to the default — a stale cache that misses
// is a nuisance, a server that refuses to start over a typo is worse.
func answerCacheTTL() time.Duration {
	raw := os.Getenv("GRIMOIRE_ANSWER_CACHE_TTL")
	if raw == "" {
		return cache.DefaultTTL
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Printf("GRIMOIRE_ANSWER_CACHE_TTL=%q is not a valid duration — using %s", raw, cache.DefaultTTL)
		return cache.DefaultTTL
	}
	return d
}

func llmConfig() llm.Config {
	return llm.Config{
		BaseURL: env("ANTHROPIC_BASE_URL", llm.DefaultBaseURL),
		APIKey:  os.Getenv("ANTHROPIC_API_KEY"),
		Model:   env("ANTHROPIC_MODEL", "glm-4.6"),
	}
}

// llmFallbacks reads the standby providers the chat falls back to when the
// primary fails — an exhausted balance, a dead key, an endpoint having a bad
// day. They are read from ANTHROPIC_FALLBACK_* and then numbered
// ANTHROPIC_FALLBACK_2_*, _3_, ... and are tried in that order. The chain stops
// at the first gap, so a missing key ends it rather than skipping a rung.
//
// A fallback needs only a key: its base URL defaults to Anthropic's (the usual
// destination when a cheaper gateway runs dry) and its model to the primary's
// (right when the same model is served from a second account).
func llmFallbacks(primary llm.Config) []llm.Config {
	var out []llm.Config
	for i := 1; ; i++ {
		prefix := "ANTHROPIC_FALLBACK_"
		if i > 1 {
			prefix = fmt.Sprintf("ANTHROPIC_FALLBACK_%d_", i)
		}
		key := os.Getenv(prefix + "API_KEY")
		if strings.TrimSpace(key) == "" {
			return out
		}
		out = append(out, llm.Config{
			BaseURL: env(prefix+"BASE_URL", llm.DefaultBaseURL),
			APIKey:  key,
			Model:   env(prefix+"MODEL", primary.Model),
		})
	}
}

// llmClient builds the chat client over the primary provider and its fallbacks.
func llmClient() *llm.Client {
	primary := llmConfig()
	return llm.New(primary, llmFallbacks(primary)...)
}

// embeddingsConfig reads the OpenAI-compatible embeddings endpoint settings.
// Default base URL is OpenAI's; z.ai and other compatible gateways work by
// overriding EMBEDDINGS_BASE_URL (e.g. https://api.z.ai/v1).
func embeddingsConfig() embeddings.Config {
	return embeddings.Config{
		BaseURL: env("EMBEDDINGS_BASE_URL", embeddings.DefaultBaseURL),
		APIKey:  os.Getenv("EMBEDDINGS_API_KEY"),
		Model:   env("EMBEDDINGS_MODEL", ""),
	}
}

// embeddingsClient returns a configured embeddings client, or nil when
// embeddings are not configured (the default — retrieval is then FTS5-only).
func embeddingsClient() *embeddings.Client {
	c := embeddings.New(embeddingsConfig())
	if !c.Configured() {
		return nil
	}
	return c
}

// transcribeConfig reads the OpenAI-compatible transcription endpoint
// settings. The model is the switch (the base URL defaults, the key is
// optional because local backends authenticate nobody); unset model means
// the audio→transcript affordance is not there, exactly how embeddings
// behaves.
func transcribeConfig() transcribe.Config {
	return transcribe.Config{
		BaseURL: env("TRANSCRIBE_BASE_URL", transcribe.DefaultBaseURL),
		APIKey:  os.Getenv("TRANSCRIBE_API_KEY"),
		Model:   env("TRANSCRIBE_MODEL", ""),
		Timeout: transcribeTimeout(),
	}
}

// transcribeClient returns a configured transcription client, or nil when
// transcription is not configured (the default).
func transcribeClient() *transcribe.Client {
	c := transcribe.New(transcribeConfig())
	if !c.Configured() {
		return nil
	}
	return c
}

// transcribeTimeout bounds one chunk request. Local CPU whisper is slower
// than realtime, so the default is generous; an unparseable value falls back
// rather than failing the boot.
func transcribeTimeout() time.Duration {
	raw := os.Getenv("TRANSCRIBE_TIMEOUT")
	if raw == "" {
		return 0 // the client's DefaultTimeout
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Printf("TRANSCRIBE_TIMEOUT=%q is not a valid duration — using the default", raw)
		return 0
	}
	return d
}

// transcribeOptions gathers the operator knobs of the transcription job
// path. Recordings wait beside the database file (never /tmp — a reboot must
// not eat a resumable job's audio), and the audio is deleted once the
// transcript lands unless TRANSCRIBE_KEEP_AUDIO opts in.
func transcribeOptions() server.TranscribeOptions {
	maxUpload := int64(1024)
	if v, err := strconv.ParseInt(env("TRANSCRIBE_MAX_UPLOAD_MB", ""), 10, 64); err == nil && v > 0 {
		maxUpload = v
	}
	chunkMB := int64(0)
	if v, err := strconv.ParseInt(env("TRANSCRIBE_CHUNK_MB", ""), 10, 64); err == nil && v > 0 {
		chunkMB = v
	}
	maxDur := 8 * time.Hour
	if raw := os.Getenv("TRANSCRIBE_MAX_DURATION"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			maxDur = d
		} else {
			log.Printf("TRANSCRIBE_MAX_DURATION=%q is not a valid duration — using %s", raw, maxDur)
		}
	}
	keep := false
	switch strings.ToLower(os.Getenv("TRANSCRIBE_KEEP_AUDIO")) {
	case "1", "true", "yes", "on":
		keep = true
	}
	return server.TranscribeOptions{
		Dir:            env("TRANSCRIBE_DIR", filepath.Join(filepath.Dir(dbPath()), "transcribe")),
		KeepAudio:      keep,
		MaxUploadBytes: maxUpload << 20,
		MaxDuration:    maxDur,
		ChunkBytes:     chunkMB << 20,
	}
}

func cardsService() *cards.Service {
	return cards.NewWithBase(env("SCRYFALL_BASE_URL", cards.DefaultBaseURL))
}

// rulingsService shares the Scryfall endpoint with cardsService: the rulings
// layer resolves card names to ids via the same Scryfall API, so a single
// SCRYFALL_BASE_URL override retargets both.
func rulingsService() *rulings.Service {
	return rulings.NewWithBase(env("SCRYFALL_BASE_URL", rulings.DefaultBaseURL))
}

// mtgjsonURL returns the AtomicCards endpoint used to build the card-name
// dictionary.
func mtgjsonURL() string { return env("MTGJSON_URL", data.DefaultMTGJSONURL) }

// edhrecEnabled reads the deck builder's EDHREC enrichment flag. Off by
// default: the routes are unofficial, so enrichment is opt-in.
func edhrecEnabled() bool {
	switch strings.ToLower(os.Getenv("GRIMOIRE_EDHREC")) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// edhrecCacheDir returns where the EDHREC disk cache lives.
func edhrecCacheDir() string {
	return env("GRIMOIRE_EDHREC_CACHE_DIR", filepath.Join(filepath.Dir(dbPath()), "edhrec-cache"))
}

// open5eBaseURL returns the Open5e endpoint used for both the D&D entity
// resolver and the entity-name dictionary build, so one override retargets
// both.
func open5eBaseURL() string { return env("OPEN5E_BASE_URL", entities.DefaultBaseURL) }

func fetchOpts() data.FetchOptions {
	return data.FetchOptions{
		MTGURL:     os.Getenv("MTG_RULES_URL"),
		DNDRepo:    os.Getenv("DND_REPO"),
		DNDRef:     os.Getenv("DND_REF"),
		DNDDocsDir: os.Getenv("DND_DOCS_DIR"),
	}
}

func openStore() (*index.Store, error) {
	if err := ensureDBDir(); err != nil {
		return nil, err
	}
	store, err := index.Open(dbPath())
	if err != nil {
		return nil, err
	}
	// Schema first, always. A failure here is fatal to the caller: serving on
	// a half-applied schema is how a self-hosted box loses data quietly.
	if err := migrate.Up(store.DB()); err != nil {
		store.Close()
		return nil, err
	}
	return store, nil
}

// ensureDBDir creates the directory the SQLite file lives in. Shared by the
// store opener and the migrate subcommands so `grimoire migrate up` works on a
// box that has never booted the server.
func ensureDBDir() error {
	dir := filepath.Dir(dbPath())
	if dir == "" || dir == "." {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create db dir: %w", err)
	}
	return nil
}

// runMigrate implements the operator-facing schema subcommands. They open the
// database directly rather than through index.Open, so `status` reports on the
// schema as it actually is instead of on one the opener just created.
func runMigrate(args []string) error {
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
	}
	if err := ensureDBDir(); err != nil {
		return err
	}
	db, err := index.OpenDB(dbPath())
	if err != nil {
		return err
	}
	defer db.Close()

	switch sub {
	case "status":
		return migrate.Status(db, os.Stdout)
	case "up":
		if err := migrate.Up(db); err != nil {
			return err
		}
		v, err := migrate.Version(db)
		if err != nil {
			return err
		}
		log.Printf("schema up to date at version %d", v)
		return nil
	case "down":
		if err := migrate.Down(db); err != nil {
			return err
		}
		v, err := migrate.Version(db)
		if err != nil {
			return err
		}
		log.Printf("rolled back; schema now at version %d", v)
		return nil
	default:
		return fmt.Errorf("unknown migrate subcommand %q (want status, up or down)", sub)
	}
}

// runCampaign implements the campaign-graph subcommands. `campaign check`
// runs the integrity checks over one campaign (or every campaign) and prints
// the findings; it exits non-zero from the caller's perspective only when a
// check reports an error-severity finding, so a cron or pre-session ritual
// can gate on it. `campaign plans` lists one campaign's faction plans — a
// read-only view of plan state, inspectable with no server and no key.
// `campaign simulate` previews one tick window the same way: pure
// arithmetic, nothing written, no key needed.
func runCampaign(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: grimoire campaign check [campaign-id] | grimoire campaign plans <campaign-id> | grimoire campaign simulate <campaign-id> --days 14 --seed N")
	}
	switch args[0] {
	case "check":
		return runCampaignCheck(args[1:])
	case "plans":
		return runCampaignPlans(args[1:])
	case "simulate":
		return runCampaignSimulate(args[1:])
	default:
		return fmt.Errorf("unknown campaign subcommand %q (want check, plans or simulate)", args[0])
	}
}

// simulateArgs splits the simulate subcommand's flags out of its arguments.
// Both "--days 14" and "--days=14" forms are accepted.
func simulateArgs(args []string) (days int, seed int64, seedSet bool, rest []string) {
	days = 14
	for i := 0; i < len(args); i++ {
		a := args[i]
		flag, value, hasValue := strings.Cut(a, "=")
		switch flag {
		case "--offline":
			// accepted for symmetry with the canon subcommands; the tick
			// is always offline
		case "--days":
			if !hasValue && i+1 < len(args) {
				i++
				value = args[i]
			}
			if v, err := strconv.Atoi(value); err == nil {
				days = v
			}
		case "--seed":
			if !hasValue && i+1 < len(args) {
				i++
				value = args[i]
			}
			if v, err := strconv.ParseInt(value, 10, 64); err == nil {
				seed, seedSet = v, true
			}
		default:
			rest = append(rest, a)
		}
	}
	return days, seed, seedSet, rest
}

// runCampaignSimulate previews the deterministic outcome of advancing the
// world: grimoire campaign simulate <campaign-id> --days 14 --seed N. The
// online surface's preview is the same computation plus a row; this one
// writes nothing at all — the offline, no-key path beside campaign check.
func runCampaignSimulate(args []string) error {
	days, seed, seedSet, rest := simulateArgs(args)
	if len(rest) != 1 {
		return fmt.Errorf("usage: grimoire campaign simulate <campaign-id> --days 14 --seed N")
	}
	campaignID := rest[0]
	if err := ensureDBDir(); err != nil {
		return err
	}
	db, err := index.OpenDB(dbPath())
	if err != nil {
		return err
	}
	defer db.Close()
	if err := migrate.Up(db); err != nil {
		return err
	}
	camps, err := campaign.New(db)
	if err != nil {
		return err
	}
	factions, err := faction.New(db)
	if err != nil {
		return err
	}
	ctx := context.Background()
	c, err := camps.GetCampaign(ctx, campaignID)
	if err != nil {
		return err
	}
	cal, _, err := camps.GetCalendar(ctx, campaignID)
	if err != nil {
		return err
	}
	snap, err := canon.LoadSnapshot(ctx, db, campaignID)
	if err != nil {
		return err
	}
	snap.Clock = c.Clock
	plans, err := factions.ListPlans(ctx, campaign.ScopeDM, campaignID)
	if err != nil {
		return err
	}
	entries, err := camps.ListScheduledEvents(ctx, campaignID, true)
	if err != nil {
		return err
	}
	if !seedSet {
		seed = sim.DefaultSeed(campaignID, c.Clock, days)
	}
	res := sim.Tick(snap, cal, plans, entries, days, seed)

	log.Printf("%s (%s): day %s -> %s (%dd, seed %d), digest %s",
		c.Name, campaignID, cal.FormatShort(res.FromDay), cal.FormatShort(res.ToDay), days, seed, res.Digest)
	nameOf := func(id string) string {
		for _, e := range snap.Entities {
			if e.ID == id {
				return e.Name
			}
		}
		return id
	}
	for _, pa := range res.Plans {
		status := "unchanged"
		if pa.Moved {
			status = "moved"
		}
		log.Printf("  [plan/%s] %s — %q (%s -> %s)", status, nameOf(pa.FactionEntity), pa.Name,
			pa.Progression.FromState, pa.Progression.ToState)
		log.Printf("    %s", pa.Progression.Summary())
	}
	for _, d := range res.Due {
		log.Printf("  [due] %s — %s", cal.FormatShort(d.Day), d.Name)
	}
	for _, m := range res.Missed {
		log.Printf("  [missed] %s — %s (day %d, still pending behind the clock)", cal.FormatShort(m.Day), m.Name, m.Day)
	}
	for _, a := range res.Actions {
		log.Printf("  [npc] %s (day %d) — %s", a.NPCName, a.Day, a.Summary)
	}
	for _, c := range res.Consequences {
		log.Printf("  [reaction] %s (day %d) — %s", c.ReactorName, c.Day, c.Summary)
	}
	wrote := "nothing is written; stage the outcomes through the API to propose them"
	log.Printf("  %s", wrote)
	return nil
}

// runCampaignPlans prints a campaign's faction plans: owner, status, state,
// the active step with its progress, and the remaining checklist — the DM's
// "what is everyone doing" answer straight off the database.
func runCampaignPlans(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: grimoire campaign plans <campaign-id>")
	}
	campaignID := args[0]
	if err := ensureDBDir(); err != nil {
		return err
	}
	db, err := index.OpenDB(dbPath())
	if err != nil {
		return err
	}
	defer db.Close()
	if err := migrate.Up(db); err != nil {
		return err
	}
	camps, err := campaign.New(db)
	if err != nil {
		return err
	}
	factions, err := faction.New(db)
	if err != nil {
		return err
	}
	ctx := context.Background()
	c, err := camps.GetCampaign(ctx, campaignID)
	if err != nil {
		return err
	}
	plans, err := factions.ListPlans(ctx, campaign.ScopeDM, campaignID)
	if err != nil {
		return err
	}
	if len(plans) == 0 {
		log.Printf("%s (%s): no faction plans", c.Name, campaignID)
		return nil
	}
	byID := map[string]string{}
	if entities, err := camps.ListEntities(ctx, campaign.ScopeDM, campaignID, ""); err == nil {
		for _, e := range entities {
			byID[e.ID] = e.Name
		}
	}
	nameOf := func(id string) string {
		if n, ok := byID[id]; ok {
			return n
		}
		return id
	}
	for _, p := range plans {
		owner := nameOf(p.FactionEntity)
		pct := 0
		if step := p.ActiveStep(); step != nil && step.Cost > 0 && p.Progress > 0 {
			pct = int(min(p.Progress/step.Cost, 1) * 100)
		}
		log.Printf("%s — %q [%s] at %s (%d%% into the active step, %s/day)",
			owner, p.Name, p.Status, p.CurrentState, pct, trimRate(p.RatePerDay))
		for _, step := range p.Steps {
			mark := " "
			if done := p.ReachedContains(step.State); done {
				mark = "x"
			}
			stepName := step.Name
			if stepName == "" {
				stepName = step.State
			}
			log.Printf("  [%s] %-28s cost %s", mark, stepName, trimRate(step.Cost))
		}
	}
	return nil
}

// trimRate renders a number without trailing zeros for the CLI listing.
func trimRate(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func runCampaignCheck(args []string) error {
	if len(args) > 1 {
		return fmt.Errorf("usage: grimoire campaign check [campaign-id]")
	}
	if err := ensureDBDir(); err != nil {
		return err
	}
	db, err := index.OpenDB(dbPath())
	if err != nil {
		return err
	}
	defer db.Close()
	if err := migrate.Up(db); err != nil {
		return err
	}

	var ids []string
	if len(args) == 1 {
		ids = []string{args[0]}
	} else {
		rows, err := db.Query(`SELECT id FROM campaigns ORDER BY name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			return err
		}
	}

	errorCount := 0
	for _, id := range ids {
		var name string
		if err := db.QueryRow(`SELECT name FROM campaigns WHERE id = ?`, id).Scan(&name); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("campaign %s: %w", id, campaign.ErrNotFound)
			}
			return err
		}
		findings, err := campaign.Integrity(context.Background(), campaign.ScopeDM, db, id)
		if err != nil {
			return err
		}
		if len(findings) == 0 {
			log.Printf("%s (%s): clean", name, id)
			continue
		}
		for _, f := range findings {
			if f.Severity == campaign.SeverityError {
				errorCount++
			}
			log.Printf("%s (%s): [%s/%s] %s", name, id, f.Severity, f.Check, f.Message)
		}
	}
	if errorCount > 0 {
		return fmt.Errorf("%d error-severity finding(s); the graph contradicts itself", errorCount)
	}
	return nil
}

// runCanon implements the canon-engine subcommands. `canon check` runs the
// deterministic consistency engine over one campaign (or every campaign) —
// no model, no key, pure offline — refreshes the flag ledger, and prints the
// flags. It exits non-zero when an open error-severity flag remains, so a
// pre-session ritual can gate on it; accepted and dismissed findings never
// fail the run, because a human already ruled on them.
//
// `canon continuity`, `canon entail` and `canon health` (MAD-312) are the
// Stage 4 surfaces: deterministic cores with an optional model pass, which
// runs when a key is configured unless --offline says otherwise.
func runCanon(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: grimoire canon check|continuity|entail|health ...")
	}
	switch args[0] {
	case "check":
		return runCanonCheck(args[1:])
	case "continuity":
		return runCanonContinuity(args[1:])
	case "entail":
		return runCanonEntail(args[1:])
	case "health":
		return runCanonHealth(args[1:])
	default:
		return fmt.Errorf("unknown canon subcommand %q (want check, continuity, entail or health)", args[0])
	}
}

// canonFlagArgs splits --offline out of a subcommand's arguments.
func canonFlagArgs(args []string) (offline bool, rest []string) {
	for _, a := range args {
		if a == "--offline" {
			offline = true
			continue
		}
		rest = append(rest, a)
	}
	return offline, rest
}

// canonStoreFor opens the canon store the Stage 4 subcommands share: the
// model client is wired when chat is configured (and not forced offline), so
// the residue/narrative passes run; otherwise the store is offline and the
// deterministic passes answer alone.
func canonStoreFor(db *sql.DB, offline bool) (*canon.Store, error) {
	if !offline {
		if client := llmClient(); client != nil && client.Configured() {
			cfg := canon.ConfigFromEnv(os.Getenv)
			return canon.NewWithValidator(db, canon.NewLLMModel(client),
				canon.NewLLMValidator(client, cfg.ValidateModel), cfg)
		}
		log.Printf("no model configured — running the deterministic passes only")
	}
	return canon.NewOffline(db)
}

// runCanonContinuity checks a prep document against campaign state:
// grimoire canon continuity [--offline] <campaign-id> <prep.json>, where
// prep.json is the Prep JSON ({"title": ..., "scenes": [...]}) and "-" reads
// it from stdin.
func runCanonContinuity(args []string) error {
	offline, args := canonFlagArgs(args)
	if len(args) != 2 {
		return fmt.Errorf("usage: grimoire canon continuity [--offline] <campaign-id> <prep.json|->")
	}
	id, prepPath := args[0], args[1]
	raw, err := readInput(prepPath)
	if err != nil {
		return err
	}
	var prep canon.Prep
	if err := json.Unmarshal(raw, &prep); err != nil {
		return fmt.Errorf("prep %s: %w", prepPath, err)
	}
	db, store, err := openCanonStore(offline)
	if err != nil {
		return err
	}
	defer db.Close()
	rep, err := store.CheckContinuity(context.Background(), id, &prep)
	if err != nil {
		return err
	}
	log.Printf("%s: %d deterministic finding(s)%s", id, len(rep.Findings), canonMode(rep.Offline))
	for _, f := range rep.Findings {
		log.Printf("  [%s/%s] %s — %s", f.Severity, f.Check, f.RecordID, f.Message)
	}
	for _, f := range rep.ModelFindings {
		log.Printf("  [review/%s] (model) %s — %s", f.Check, f.RecordID, f.Message)
	}
	for _, p := range rep.Problems {
		log.Printf("  problem: %s", p)
	}
	return nil
}

// runCanonEntail runs the entailment pass over prose before the DM sees it:
// grimoire canon entail [--offline] <campaign-id> <prose.txt|->.
func runCanonEntail(args []string) error {
	offline, args := canonFlagArgs(args)
	if len(args) != 2 {
		return fmt.Errorf("usage: grimoire canon entail [--offline] <campaign-id> <prose.txt|->")
	}
	id, prosePath := args[0], args[1]
	raw, err := readInput(prosePath)
	if err != nil {
		return err
	}
	db, store, err := openCanonStore(offline)
	if err != nil {
		return err
	}
	defer db.Close()
	rep, err := store.CheckEntailment(context.Background(), id, canon.EntailInput{Prose: string(raw)})
	if err != nil {
		return err
	}
	log.Printf("%s: judged against %d record(s)%s", id, len(rep.Records), canonMode(rep.Offline))
	for _, f := range rep.Findings {
		log.Printf("  [%s/%s] %s", f.Severity, f.Check, f.Message)
	}
	for _, c := range rep.Claims {
		log.Printf("  %s: %s — %s", c.Verdict, c.Claim, c.Reason)
	}
	for _, p := range rep.Problems {
		log.Printf("  problem: %s", p)
	}
	return nil
}

// runCanonHealth prints the campaign health report:
// grimoire canon health [--offline] [campaign-id] (every campaign when
// omitted).
func runCanonHealth(args []string) error {
	offline, args := canonFlagArgs(args)
	if len(args) > 1 {
		return fmt.Errorf("usage: grimoire canon health [--offline] [campaign-id]")
	}
	db, store, err := openCanonStore(offline)
	if err != nil {
		return err
	}
	defer db.Close()

	var ids []string
	if len(args) == 1 {
		ids = []string{args[0]}
	} else {
		rows, err := db.Query(`SELECT id FROM campaigns ORDER BY name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			return err
		}
	}
	for _, id := range ids {
		rep, err := store.HealthReport(context.Background(), id, canon.DefaultHealthOptions())
		if err != nil {
			return err
		}
		log.Printf("%s (%s): %d thread(s), %d clue(s), %d unused npc(s), %d stalled region(s), %d unfounded relationship(s), %d open flag(s)%s",
			rep.CampaignName, id, len(rep.Threads), len(rep.Clues), len(rep.UnusedNPCs),
			len(rep.DormantRegions), len(rep.Unresolved), rep.OpenFlagCount, canonMode(rep.Offline))
		for _, th := range rep.Threads {
			log.Printf("  [%s] %s", th.Kind, th.Message)
		}
		for _, th := range rep.Clues {
			log.Printf("  [%s] %s", th.Kind, th.Message)
		}
		for _, n := range rep.UnusedNPCs {
			log.Printf("  [%s] %s", n.CheckCode, n.Message)
		}
		for _, r := range rep.DormantRegions {
			log.Printf("  [%s] %s", r.CheckCode, r.Message)
		}
		for _, r := range rep.Unresolved {
			log.Printf("  [%s] %s", r.CheckCode, r.Message)
		}
		for _, p := range rep.Pacing {
			log.Printf("  pacing: session %d (%s): %d encounter(s), %d discovery/ies, %d qa, %d ruling(s), %d note(s)",
				p.Ordinal, p.Name, p.Encounters, p.Discoveries, p.QA, p.Rulings, p.Notes)
		}
		if rep.Narrative != "" {
			log.Printf("  narrative: %s", rep.Narrative)
		}
		for _, p := range rep.Problems {
			log.Printf("  problem: %s", p)
		}
	}
	return nil
}

// canonMode renders the offline/model suffix for the Stage 4 subcommands.
func canonMode(offline bool) string {
	if offline {
		return " (offline)"
	}
	return ""
}

// openCanonStore opens and migrates the database and builds the canon store.
func openCanonStore(offline bool) (*sql.DB, *canon.Store, error) {
	if err := ensureDBDir(); err != nil {
		return nil, nil, err
	}
	db, err := index.OpenDB(dbPath())
	if err != nil {
		return nil, nil, err
	}
	if err := migrate.Up(db); err != nil {
		db.Close()
		return nil, nil, err
	}
	store, err := canonStoreFor(db, offline)
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	return db, store, nil
}

// readInput reads a file, or stdin when the path is "-".
func readInput(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

func runCanonCheck(args []string) error {
	if len(args) > 1 {
		return fmt.Errorf("usage: grimoire canon check [campaign-id]")
	}
	if err := ensureDBDir(); err != nil {
		return err
	}
	db, err := index.OpenDB(dbPath())
	if err != nil {
		return err
	}
	defer db.Close()
	if err := migrate.Up(db); err != nil {
		return err
	}
	store, err := canon.NewOffline(db)
	if err != nil {
		return err
	}

	var ids []string
	if len(args) == 1 {
		ids = []string{args[0]}
	} else {
		rows, err := db.Query(`SELECT id FROM campaigns ORDER BY name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			return err
		}
	}

	errorCount := 0
	for _, id := range ids {
		var name string
		if err := db.QueryRow(`SELECT name FROM campaigns WHERE id = ?`, id).Scan(&name); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("campaign %s: %w", id, campaign.ErrNotFound)
			}
			return err
		}
		flags, err := store.CheckCampaign(context.Background(), id, canon.DefaultCheckOptions())
		if err != nil {
			return err
		}
		open := 0
		for _, f := range flags {
			if f.Status == canon.FlagOpen {
				open++
				if f.Severity == string(campaign.SeverityError) {
					errorCount++
				}
			}
		}
		if open == 0 && len(flags) == 0 {
			log.Printf("%s (%s): clean", name, id)
			continue
		}
		log.Printf("%s (%s): %d flag(s)", name, id, len(flags))
		for _, f := range flags {
			log.Printf("  [%s/%s] %s — %s", f.Severity, f.CheckCode, f.Status, f.Message)
		}
	}
	if errorCount > 0 {
		return fmt.Errorf("%d open error-severity flag(s); the campaign contradicts itself", errorCount)
	}
	return nil
}

// ensureIndexed builds the index if the store is empty.
func ensureIndexed(ctx context.Context, store *index.Store) error {
	ok, err := store.Indexed(ctx)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	log.Println("index empty — building (this fetches MTG + D&D rules; first run takes a moment)...")
	return buildIndex(ctx, store)
}

func buildIndex(ctx context.Context, store *index.Store) error {
	ds, err := data.BuildDataset(ctx, fetchOpts())
	if err != nil {
		return err
	}
	total := len(ds.Records)
	for c, m := range ds.Meta {
		log.Printf("  indexed %-3s %s — %d records (version %s)", c, m.Name, m.RecordCount, m.Version)
	}
	log.Printf("indexing %d total records into SQLite + FTS5...", total)
	if err := store.Index(ctx, ds); err != nil {
		return err
	}
	log.Printf("index built: %d records", total)

	// Semantic vectors are best-effort, like the card dictionary: without them
	// retrieval is FTS5-only; with them the Q&A step gains semantic recall for
	// questions that share no keywords with the rule they need.
	if err := store.IndexEmbeddings(ctx); err != nil {
		log.Printf("embeddings skipped: %v (retrieval will be FTS5-only)", err)
	} else if embedded, _ := store.Embedded(ctx); embedded {
		log.Printf("indexed semantic vectors")
	}

	// The card database and the card-name dictionary share one AtomicCards
	// download: Populate captures the full card rows for the deck builder and
	// returns the names for the chat's dictionary. Best-effort — the app works
	// without it, the deck builder just reports unavailable.
	if err := buildCardDatabase(ctx, store); err != nil {
		log.Printf("card database skipped: %v (deck builder will be unavailable until reindex)", err)
	}

	// The D&D entity-name dictionary is equally best-effort: without it the
	// D&D chat detects entity mentions by text heuristics only.
	if err := buildDNDNameDictionary(ctx, store); err != nil {
		log.Printf("d&d name dictionary skipped: %v (chat will detect entities by text heuristics only)", err)
	}

	// Record the local-books fingerprint the index was built against, so the
	// next boot can tell whether the library changed and rebuild on its own.
	if err := store.SetMeta(ctx, "dnd_docs_fingerprint", data.DNDDocsFingerprint(fetchOpts().DNDDocsDir)); err != nil {
		log.Printf("record books fingerprint: %v", err)
	}
	return nil
}

// buildCardDatabase populates the deck builder's card tables from MTGJSON
// AtomicCards and refreshes the chat's card-name dictionary from the same
// download.
func buildCardDatabase(ctx context.Context, store *index.Store) error {
	names, err := carddb.Populate(ctx, store.DB(), mtgjsonURL())
	if err != nil {
		return fmt.Errorf("populate card database: %w", err)
	}
	if err := store.IndexCards(ctx, names); err != nil {
		return fmt.Errorf("index card names: %w", err)
	}
	log.Printf("indexed %d cards from MTGJSON", len(names))
	return nil
}

// buildDNDNameDictionary lists the SRD entity names from Open5e (spells,
// creatures, magic items, feats, conditions, weapons) and stores them so the
// D&D chat can detect entity mentions the text heuristics miss — the
// counterpart of the MTG card-name dictionary.
func buildDNDNameDictionary(ctx context.Context, store *index.Store) error {
	names, err := data.FetchDNDNames(ctx, open5eBaseURL())
	if err != nil {
		return fmt.Errorf("fetch open5e names: %w", err)
	}
	if err := store.IndexEntityNames(ctx, names); err != nil {
		return fmt.Errorf("index entity names: %w", err)
	}
	log.Printf("indexed %d D&D entity names from Open5e", len(names))
	return nil
}

// loadCardDictionary builds the in-memory card dictionary from the store. If
// the table is empty (a fresh serve that did not run a full index build, or
// an install upgraded from before the dictionary existed) it fetches MTGJSON
// once. A nil dictionary is returned when no names are available; the chat
// then falls back to text heuristics.
func loadCardDictionary(ctx context.Context, store *index.Store) *cards.Dictionary {
	names, err := store.LoadCardNames(ctx)
	if err != nil {
		log.Printf("load card names: %v", err)
		return nil
	}
	if len(names) == 0 {
		if err := buildCardDatabase(ctx, store); err != nil {
			log.Printf("card dictionary unavailable: %v", err)
			return nil
		}
		names, err = store.LoadCardNames(ctx)
		if err != nil {
			log.Printf("load card names: %v", err)
			return nil
		}
	}
	if len(names) == 0 {
		return nil
	}
	d := cards.NewDictionary(names)
	log.Printf("loaded card dictionary: %d names", d.Size())
	return d
}

// loadDNDNameDictionary builds the in-memory D&D entity dictionary from the
// store, fetching Open5e's name listings once if the table is empty (a fresh
// serve without a full index build). A nil dictionary degrades detection to
// text heuristics.
func loadDNDNameDictionary(ctx context.Context, store *index.Store) *cards.Dictionary {
	names, err := store.LoadEntityNames(ctx)
	if err != nil {
		log.Printf("load entity names: %v", err)
		return nil
	}
	if len(names) == 0 {
		if err := buildDNDNameDictionary(ctx, store); err != nil {
			log.Printf("d&d name dictionary unavailable: %v", err)
			return nil
		}
		names, err = store.LoadEntityNames(ctx)
		if err != nil {
			log.Printf("load entity names: %v", err)
			return nil
		}
	}
	if len(names) == 0 {
		return nil
	}
	d := cards.NewDictionary(names)
	log.Printf("loaded D&D name dictionary: %d names", d.Size())
	return d
}

func runIndex() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	store, err := openStore()
	if err != nil {
		return err
	}
	defer store.Close()
	store.SetEmbedder(embeddingsClient())

	if err := store.Reset(); err != nil {
		return fmt.Errorf("reset: %w", err)
	}
	return buildIndex(ctx, store)
}

func runServe() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	store, err := openStore()
	if err != nil {
		return err
	}
	defer store.Close()
	embedClient := embeddingsClient()
	store.SetEmbedder(embedClient)

	if err := ensureIndexed(ctx, store); err != nil {
		return fmt.Errorf("ensure index: %w", err)
	}

	// An install whose index predates the reading surface has an FTS index but
	// empty reader tables. Backfill it in the background (same rebuild the
	// admin button runs) so the books appear without a terminal session; the
	// app serves fine from the old index while it runs.
	if ok, err := store.ReaderIndexed(ctx); err != nil {
		log.Printf("reader check: %v", err)
	} else if !ok {
		log.Println("reading surface empty — rebuilding the index in the background to add the books...")
		if err := buildIndex(ctx, store); err != nil {
			log.Printf("reader backfill failed (search unaffected): %v", err)
		}
	}

	// A changed local library reindexes on its own: fingerprint the books
	// directory and compare against the one the index was built with. Nobody
	// should need a terminal because a PDF was added.
	if dir := fetchOpts().DNDDocsDir; dir != "" {
		now := data.DNDDocsFingerprint(dir)
		built, err := store.GetMeta(ctx, "dnd_docs_fingerprint")
		if err != nil {
			log.Printf("books fingerprint check: %v", err)
		} else if now != built {
			log.Println("local D&D books changed since the last build — reindexing...")
			if err := buildIndex(ctx, store); err != nil {
				log.Printf("books reindex failed (serving the existing index): %v", err)
			}
		}
	}

	// Populate semantic vectors when embeddings are configured but a rules
	// index already exists without them (e.g. enabled after first boot). A
	// failure is best-effort: retrieval falls back to FTS5-only.
	if embedClient != nil {
		embedded, embedErr := store.Embedded(ctx)
		if embedErr != nil {
			log.Printf("embeddings check: %v", embedErr)
		} else if !embedded {
			log.Printf("indexing semantic vectors (model %s)...", embedClient.Model())
			if err := store.IndexEmbeddings(ctx); err != nil {
				log.Printf("embeddings skipped: %v (retrieval will be FTS5-only)", err)
			} else {
				log.Printf("indexed semantic vectors (model %s)", embedClient.Model())
			}
		}
	}

	cardDict := loadCardDictionary(ctx, store)
	dndDict := loadDNDNameDictionary(ctx, store)

	// Wire each corpus's entity resolver onto its registered definition. MTG's
	// Scryfall resolver shares the card service + dictionary the chat already
	// uses; D&D's Open5e resolver grounds spells/creatures/items/feats from the
	// SRD, with its own name dictionary for the mentions the heuristics miss.
	// Done after the dictionaries load so a best-effort dictionary miss
	// (text-heuristics-only detection) still leaves each corpus a working
	// resolver.
	data.SetResolver(data.CorpusMTG, entities.NewScryfall(cardsService(), cardDict))
	open5e := entities.NewWithBase(open5eBaseURL())
	open5e.SetDictionary(dndDict)
	data.SetResolver(data.CorpusDND, open5e)

	// Chat history shares the index's SQLite file and connection pool.
	// Rebuilding the rules index does not touch these tables.
	chats, err := chat.New(store.DB())
	if err != nil {
		return err
	}

	// Cached Q&A answers share that file too, for the same reason: a rules
	// reindex only clears the docs tables, so cached entries survive the
	// rebuild (they stop hitting on their own once the grounding shifts).
	answers, err := cache.New(store.DB(), answerCacheTTL())
	if err != nil {
		return err
	}

	// Accounts and sessions share that file too, for the same reason.
	users, err := auth.New(store.DB(), sessionTTL(), inviteTTL())
	if err != nil {
		return err
	}

	// The campaign graph and the knowledge layer share it as well; the schema
	// for both is migration-owned (0002, 0003, 0004), applied by openStore
	// before anything in this function runs.
	campaigns, err := campaign.New(store.DB())
	if err != nil {
		return err
	}
	knowledge, err := knowledge.New(store.DB())
	if err != nil {
		return err
	}

	// Spaced-repetition review schedules share that file too: the concept keys
	// are stable rule numbers, so a reindex never strands a user's progress.
	studies, err := study.New(store.DB())
	if err != nil {
		return err
	}

	// Saved encounters share that file as well; difficulty verdicts are never
	// stored, only the party and roster they are recomputed from.
	encounters, err := encounter.New(store.DB())
	if err != nil {
		return err
	}

	// The SRD bestiary is mirrored into the same file so the encounter
	// designer can see the whole shelf at once — filtering by challenge
	// rating and by how a creature fights needs every statblock, not one
	// lookup at a time. Loading is instant; a missing or stale mirror is
	// refreshed in the background so a cold start still serves immediately.
	bestiary, err := encounter.NewCatalog(store.DB(), open5eBaseURL())
	if err != nil {
		return err
	}
	if err := bestiary.Load(); err != nil {
		log.Printf("bestiary load: %v", err)
	}
	if bestiary.Stale() {
		go func() {
			if err := bestiary.EnsureFresh(context.Background()); err != nil {
				log.Printf("bestiary sync: %v (the encounter designer waits for a mirror)", err)
				return
			}
			log.Printf("bestiary mirrored: %d SRD creatures", bestiary.Count())
		}()
	} else {
		log.Printf("bestiary ready: %d SRD creatures", bestiary.Count())
	}

	// The SRD magic items are mirrored the same way (MAD-383): the item
	// designer's rarity bands and nearest-neighbour reads need the whole
	// shelf, not one lookup at a time. Same deal as the bestiary: load is
	// instant, a cold start serves immediately and fetches behind.
	itemCatalog, err := items.NewCatalog(store.DB(), open5eBaseURL())
	if err != nil {
		return err
	}
	if err := itemCatalog.Load(); err != nil {
		log.Printf("magic items load: %v", err)
	}
	if itemCatalog.Stale() {
		go func() {
			if err := itemCatalog.EnsureFresh(context.Background()); err != nil {
				log.Printf("magic items sync: %v (the item designer waits for a mirror)", err)
				return
			}
			log.Printf("magic items mirrored: %d SRD items", itemCatalog.Count())
		}()
	} else {
		log.Printf("magic items ready: %d SRD items", itemCatalog.Count())
	}

	// The deck builder's card database shares it too. An install that indexed
	// before the deck builder existed has an empty table; populate it once
	// from MTGJSON rather than reporting the feature unavailable forever.
	cardStore, err := carddb.New(store.DB())
	if err != nil {
		return err
	}
	if n, _ := cardStore.Count(); n == 0 {
		if err := buildCardDatabase(ctx, store); err != nil {
			log.Printf("card database unavailable: %v (deck builder disabled until a reindex)", err)
			cardStore = nil
		}
	}

	// Saved decks, same shared file, same reindex-safe treatment.
	decks, err := deck.New(store.DB())
	if err != nil {
		return err
	}

	// EDHREC enrichment is opt-in behind GRIMOIRE_EDHREC=1.
	edhrecClient := edhrec.New(edhrec.Options{
		Enabled:  edhrecEnabled(),
		CacheDir: edhrecCacheDir(),
	})
	if edhrecClient.Enabled() {
		log.Printf("EDHREC enrichment enabled (cache: %s)", edhrecCacheDir())
	}

	// Shared answer links share that file too: a share is a snapshot, so the
	// tables reference nothing that a chat deletion or index rebuild touches.
	shares, err := share.New(store.DB())
	if err != nil {
		return err
	}

	// Upgrade path: an install that predates accounts has history filed under
	// the anonymous owner. Hand it to the first keeper on the way up, so it is
	// already theirs by the time they sign in.
	if adopted, err := server.AdoptAnonymousChats(ctx, users, chats); err != nil {
		return fmt.Errorf("adopt anonymous chats: %w", err)
	} else if adopted > 0 {
		log.Printf("adopted %d pre-authentication conversations", adopted)
	}

	// The session layer rides on the campaign store: the session tables
	// exist only through the migrations, so the store opens straight
	// onto the migrated handle.
	gameSessions, err := gamesession.New(store.DB())
	if err != nil {
		return err
	}

	// The narrative spine (MAD-360): acts, scenes and session plans. Its
	// tables exist only through the migrations too, and its planning rules
	// are pure functions — no key, no model.
	stories, err := story.New(store.DB())
	if err != nil {
		return err
	}

	// The faction plan store (MAD-366) rides on the same migrations: the
	// dossier reads degrade gracefully, the plan endpoints need it.
	factions, err := faction.New(store.DB())
	if err != nil {
		return err
	}

	// The canon engine's deterministic checks need no model and work on a
	// box with no key configured at all. When chat IS configured, the same
	// store also carries the model client so the Stage 4 surfaces
	// (MAD-312: continuity residue, entailment, the health narrative) can
	// run in-process; extraction and adversarial validation stay
	// CLI/worker concerns, and the review queue (MAD-310) is wired onto
	// the graph stores so accepting a finding writes canon through the
	// same campaign and knowledge paths the DM-facing API uses.
	var canonEngine *canon.Store
	chatClient := llmClient()
	if chatClient.Configured() {
		canonCfg := canon.ConfigFromEnv(os.Getenv)
		canonEngine, err = canon.NewWithValidator(store.DB(),
			canon.NewLLMModel(chatClient), canon.NewLLMValidator(chatClient, canonCfg.ValidateModel), canonCfg)
	} else {
		canonEngine, err = canon.NewOffline(store.DB())
	}
	if err != nil {
		return err
	}
	canonEngine = canonEngine.WithGraphStores(campaigns, knowledge).WithFactions(factions)

	// The simulation tick (MAD-367): pure arithmetic over the campaign
	// snapshot, staged through the proposal gate. Its store completes a
	// decided tick batch — the clock move — so it is wired back onto the
	// canon engine as the tick finalizer.
	simEngine, err := sim.New(store.DB(), campaigns, factions, canonEngine)
	if err != nil {
		return err
	}
	canonEngine = canonEngine.WithTickFinalizer(simEngine)

	// Downtime resolution (MAD-368): the tick pointed at one character,
	// same gate, reason 'downtime' on the clock. Its store is the downtime
	// finalizer.
	downtimeEngine, err := downtime.New(store.DB(), campaigns, factions, canonEngine)
	if err != nil {
		return err
	}
	canonEngine = canonEngine.WithDowntimeFinalizer(downtimeEngine)

	// Journeys (MAD-375): the road between two places at the DM's asked
	// density, same gate, reason 'travel' on the clock. Its store is the
	// journey finalizer.
	journeyEngine, err := journey.New(store.DB(), campaigns, canonEngine, knowledge)
	if err != nil {
		return err
	}
	canonEngine = canonEngine.WithJourneyFinalizer(journeyEngine)

	// The resource ledger (MAD-419): pools, transactions and rests over the
	// typed sheets. Its store completes a decided rest batch — the ledger
	// writes and the long rest's clock move — so it is wired back onto the
	// canon engine as the rest finalizer.
	ledgerEngine, err := ledger.New(store.DB(), campaigns, canonEngine)
	if err != nil {
		return err
	}
	canonEngine = canonEngine.WithRestFinalizer(ledgerEngine)

	srv, err := server.New(store, chatClient, cardsService(), rulingsService(), cardDict, chats, answers, studies,
		server.Auth{Users: users, OpenRegistration: openRegistration()},
		func(ctx context.Context) error { return buildIndex(ctx, store) })
	if err != nil {
		return err
	}
	srv = srv.WithShares(shares)
	srv = srv.WithCampaign(campaigns, gameSessions)
	srv = srv.WithFactions(factions)
	srv = srv.WithEncounters(encounters, encounter.NewBestiaryWithBase(open5eBaseURL()), bestiary)
	srv = srv.WithHomebrew(encounter.NewHomebrewStore(store.DB()))
	srv = srv.WithItems(itemCatalog, items.NewHomebrewStore(store.DB()))
	srv = srv.WithCampaigns(campaigns, knowledge)
	srv = srv.WithCanon(canonEngine)
	srv = srv.WithStory(stories)
	srv = srv.WithSim(simEngine).WithDowntime(downtimeEngine).WithJourneys(journeyEngine).WithLedger(ledgerEngine)
	srv = srv.WithUIState(uistate.New(store.DB()))
	srv = srv.WithTranscriber(transcribeClient(), transcribeOptions())
	if cardStore != nil {
		srv = srv.WithDeckBuilder(cardStore, decks, edhrecClient)
	}

	// Jobs interrupted by a shutdown resume from their per-chunk ledger;
	// a no-op when the transcription hook is not configured.
	srv.ResumeTranscriptions()

	httpSrv := &http.Server{
		Addr:              addr(),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdown, sc := context.WithTimeout(context.Background(), 10*time.Second)
		defer sc()
		_ = httpSrv.Shutdown(shutdown)
	}()

	embedStatus := "off"
	if embedClient != nil {
		embedStatus = embedClient.Model()
	}
	transcribeStatus := "off"
	if tc := transcribeClient(); tc != nil {
		transcribeStatus = tc.Model()
	}
	chatStatus := fmt.Sprintf("%t", chatClient.Configured())
	if fb := chatClient.FallbackModels(); len(fb) > 0 {
		chatStatus += fmt.Sprintf(", falling back to %s", strings.Join(fb, " then "))
	}
	log.Printf("Grimoire listening on %s (chat configured: %s, embeddings: %s, transcription: %s)",
		addr(), chatStatus, embedStatus, transcribeStatus)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	log.Println("shutdown complete")
	return nil
}

/* ---------- the homebrew linter ---------- */

func runHomebrew(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: grimoire homebrew lint [monster|item] [id ...]")
	}
	switch args[0] {
	case "lint":
		return runHomebrewLint(args[1:])
	default:
		return fmt.Errorf("unknown homebrew subcommand %q (want lint)", args[0])
	}
}

// homebrewLintModel adapts the canon engine's model client onto the
// linter's, so the write-up pass shares the chat configuration when it
// is set — and never runs when it is not.
type homebrewLintModel struct{ m canon.ModelClient }

func (h homebrewLintModel) ModelName() string { return h.m.ModelName() }

func (h homebrewLintModel) Complete(ctx context.Context, system, user string) (homebrew.Completion, error) {
	c, err := h.m.Complete(ctx, system, user)
	return homebrew.Completion(c), err
}

// runHomebrewLint lints homebrew records the way `grimoire canon check`
// checks campaigns: deterministic output, one line per finding, and a
// non-zero exit when anything structurally invalid turns up.
//
//	grimoire homebrew lint [monster|item] [id ...]
//
// With no ids, every homebrew record is linted; with a kind, the shelf
// is restricted. The structural and computed checks run with no model
// and no network; retrieval reads the local index; the model pass runs
// only when chat is configured.
func runHomebrewLint(args []string) error {
	kind := ""
	var ids []string
	for _, a := range args {
		switch a {
		case "monster", "monsters":
			if kind != "" && kind != "monster" {
				return fmt.Errorf("lint one kind at a time")
			}
			kind = "monster"
		case "item", "items":
			if kind != "" && kind != "item" {
				return fmt.Errorf("lint one kind at a time")
			}
			kind = "item"
		default:
			ids = append(ids, a)
		}
	}
	if err := ensureDBDir(); err != nil {
		return err
	}
	store, err := index.Open(dbPath())
	if err != nil {
		return err
	}
	defer store.Close()
	db := store.DB()
	if err := migrate.Up(db); err != nil {
		return err
	}

	engine := &homebrew.Engine{Index: store, Corpus: data.CorpusDND}
	if client := llmClient(); client != nil && client.Configured() {
		engine.Model = homebrewLintModel{canon.NewLLMModel(client)}
	} else {
		log.Printf("no model configured — running the deterministic checks only")
	}

	monsters := encounter.NewHomebrewStore(db)
	shelf := items.NewHomebrewStore(db)
	catalog, err := items.NewCatalog(db, os.Getenv("OPEN5E_BASE_URL"))
	if err != nil {
		return err
	}
	// Offline by design: the mirror is read, never synced here. An empty
	// mirror degrades into the report's notices rather than a guess.
	if err := catalog.Load(); err != nil {
		return err
	}
	corpus := catalog.All()
	if len(corpus) == 0 {
		log.Printf("item mirror is empty — the rarity comparison will be skipped")
	}

	ctx := context.Background()
	lintErrorCount := 0
	printReport := func(name, kindLabel, id string, rep *homebrew.Report) {
		if len(rep.Findings) == 0 && rep.WrittenUp == "" {
			log.Printf("%s (%s %s): clean", name, kindLabel, id)
			return
		}
		log.Printf("%s (%s %s): %d finding(s)", name, kindLabel, id, len(rep.Findings))
		for _, f := range rep.Findings {
			log.Printf("  [%s/%s] %s — %s", f.Severity, f.Check, f.Subject, f.Message)
			switch {
			case f.Basis.Arithmetic != "":
				log.Printf("      basis (%s): %s", f.Basis.Origin, f.Basis.Arithmetic)
			case f.Basis.Rule != "":
				log.Printf("      basis (%s): %s", f.Basis.Origin, f.Basis.Rule)
			case f.Basis.Citation != nil:
				log.Printf("      basis (%s): %s (%s %s)",
					f.Basis.Origin, f.Basis.Citation.Title, f.Basis.Citation.Corpus, f.Basis.Citation.Number)
			}
			if f.Severity == homebrew.SeverityError {
				lintErrorCount++
			}
		}
		for _, n := range rep.Notices {
			log.Printf("  note: %s", n)
		}
		switch rep.WrittenUp {
		case homebrew.WriteUpWritten:
			log.Printf("  write-up: %s", rep.WriteUp)
		case homebrew.WriteUpRejected:
			log.Printf("  write-up rejected by the prose gate: %s", rep.WriteUpNote)
		}
	}

	lintMonster := func(owner, id string) error {
		m, err := monsters.Get(ctx, owner, id)
		if err != nil {
			return err
		}
		rep := engine.LintMonster(ctx, homebrew.MonsterInput{
			Statblock:   m.Statblock,
			RequestedCR: m.RequestedCR,
		})
		printReport(m.Name, "monster", m.ID, rep)
		return nil
	}
	lintItem := func(owner, id string) error {
		m, err := shelf.Get(ctx, owner, id)
		if err != nil {
			return err
		}
		rep := engine.LintItem(ctx, homebrew.ItemInput{Design: m.Design, Corpus: corpus})
		printReport(m.Name, "item", m.ID, rep)
		return nil
	}
	ownerOf := func(table, id string) (string, error) {
		var owner string
		err := db.QueryRow(`SELECT owner_id FROM `+table+` WHERE id = ?`, id).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("%s %s: %w", table, id, campaign.ErrNotFound)
		}
		return owner, err
	}

	switch {
	case kind == "monster" && len(ids) > 0:
		for _, id := range ids {
			owner, err := ownerOf("homebrew_monsters", id)
			if err != nil {
				return err
			}
			if err := lintMonster(owner, id); err != nil {
				return err
			}
		}
	case kind == "item" && len(ids) > 0:
		for _, id := range ids {
			owner, err := ownerOf("homebrew_items", id)
			if err != nil {
				return err
			}
			if err := lintItem(owner, id); err != nil {
				return err
			}
		}
	case len(ids) > 0:
		return fmt.Errorf("give a kind (monster or item) before specific ids")
	default:
		// Everything on both shelves, the way `canon check` walks every
		// campaign.
		rows, err := db.Query(`SELECT owner_id, id FROM homebrew_monsters ORDER BY updated_at DESC`)
		if err != nil {
			return err
		}
		type ref struct{ owner, id string }
		var monsterRefs, itemRefs []ref
		for rows.Next() {
			var r ref
			if err := rows.Scan(&r.owner, &r.id); err != nil {
				rows.Close()
				return err
			}
			monsterRefs = append(monsterRefs, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = db.Query(`SELECT owner_id, id FROM homebrew_items ORDER BY updated_at DESC`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r ref
			if err := rows.Scan(&r.owner, &r.id); err != nil {
				rows.Close()
				return err
			}
			itemRefs = append(itemRefs, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(monsterRefs) == 0 && len(itemRefs) == 0 {
			log.Printf("no homebrew to lint")
			return nil
		}
		for _, r := range monsterRefs {
			if err := lintMonster(r.owner, r.id); err != nil {
				return err
			}
		}
		for _, r := range itemRefs {
			if err := lintItem(r.owner, r.id); err != nil {
				return err
			}
		}
	}

	if lintErrorCount > 0 {
		return fmt.Errorf("%d structurally invalid finding(s) — the linter is a reviewer; fix them or weigh them", lintErrorCount)
	}
	return nil
}
