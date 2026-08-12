package data

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DefaultMTGJSONURL is the canonical MTGJSON AtomicCards endpoint: one entry
// per unique card name across all printings, as gzipped JSON. No API key.
// See https://mtgjson.com/data-models/atomic-cards/.
const DefaultMTGJSONURL = "https://mtgjson.com/api/v5/AtomicCards.json.gz"

// mtgjsonTimeout bounds the AtomicCards download during an index build. The
// dataset is on the order of tens of MiB compressed, so a few minutes is
// generous even on a slow link.
const mtgjsonTimeout = 5 * time.Minute

// maxMTGJSONBytes is a safety ceiling on the decompressed payload (the real
// size is ~100 MiB). It protects the indexer from a misconfigured mirror that
// serves something unbounded.
const maxMTGJSONBytes = 512 << 20

// FetchCardNames downloads MTGJSON's AtomicCards dataset and returns the
// unique card names — the keys of its "data" object. Grimoire uses them as a
// dictionary so the chat can detect card mentions the text heuristics miss
// (lowercase, unquoted, no Title Case). MTGJSON is only consulted during an
// index build, never per question.
func FetchCardNames(ctx context.Context, url string) ([]string, error) {
	if url == "" {
		url = DefaultMTGJSONURL
	}
	inner, cancel := context.WithTimeout(ctx, mtgjsonTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(inner, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("accept", "application/json")
	req.Header.Set("user-agent", "grimoire/1.0 (+https://github.com/madeofpendletonwool/grimoire)")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch mtgjson: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mtgjson: %s", resp.Status)
	}

	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("gunzip mtgjson: %w", err)
	}
	defer zr.Close()
	return atomicCardNames(io.LimitReader(zr, maxMTGJSONBytes))
}

// atomicCardNames streams the AtomicCards payload and returns the keys of the
// top-level "data" object without buffering the whole document. Streaming
// keeps a build's memory small even though the uncompressed dataset is ~100
// MiB: the per-face arrays under each name are skipped rather than decoded.
func atomicCardNames(r io.Reader) ([]string, error) {
	dec := json.NewDecoder(r)
	if err := expectDelim(dec, '{'); err != nil {
		return nil, err
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("mtgjson: expected object key, got %T", keyTok)
		}
		if key == "data" {
			return readAtomicData(dec)
		}
		// Any other top-level value (e.g. "meta") is ignored.
		if err := skipValue(dec); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("mtgjson: payload had no data object")
}

// readAtomicData consumes the "data" object and returns its keys. Each value
// (a per-name array of card faces) is skipped.
func readAtomicData(dec *json.Decoder) ([]string, error) {
	if err := expectDelim(dec, '{'); err != nil {
		return nil, err
	}
	var names []string
	for dec.More() {
		nameTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, ok := nameTok.(string)
		if !ok {
			return nil, fmt.Errorf("mtgjson: expected card name key, got %T", nameTok)
		}
		if err := skipValue(dec); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	// Consume the data object's closing brace.
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return names, nil
}

// expectDelim reads one token and requires it to be the given JSON delimiter.
func expectDelim(dec *json.Decoder, want rune) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok || rune(d) != want {
		return fmt.Errorf("mtgjson: expected %q, got %v", string(want), tok)
	}
	return nil
}

// skipValue advances the decoder past a single JSON value — object, array, or
// scalar — without buffering it. Used to ignore AtomicCards' face arrays.
func skipValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		// Scalar value (string/number/bool/null) already consumed.
		return nil
	}
	if d != '{' && d != '[' {
		return fmt.Errorf("mtgjson: unexpected delimiter %v", d)
	}
	for depth := 1; depth > 0; {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		if dd, ok := t.(json.Delim); ok {
			switch dd {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
}
