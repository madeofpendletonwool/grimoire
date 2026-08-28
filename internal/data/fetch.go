package data

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// FetchOptions controls which corpora are fetched and from where. The
// corpus-specific override fields are read by the matching corpus Fetcher.
type FetchOptions struct {
	MTGURL  string // override MTG comp rules URL
	DNDRepo string // override D&D SRD repo "owner/name"
	DNDRef  string // override D&D SRD git ref
	// DNDDocsDir imports local D&D documents (markdown/text, e.g. PDFs run
	// through scripts/extract-dnd-pdfs.py) alongside the fetched SRD. Empty
	// means SRD only.
	DNDDocsDir string
	// Include restricts the build to a subset of registered corpora. A nil/empty
	// map means "all registered corpora", so adding a corpus by registering it
	// is enough for it to be indexed — no literal to update.
	Include map[Corpus]bool
}

// DefaultFetchOptions enables the registered corpora with canonical sources.
func DefaultFetchOptions() FetchOptions {
	return FetchOptions{
		MTGURL:  mtgDefaultURL,
		DNDRepo: dndDefaultRepo,
		DNDRef:  dndDefaultRef,
	}
}

// include reports whether a corpus should be built. An empty Include set means
// "all registered corpora"; otherwise only the listed corpora are included.
func (o FetchOptions) include(c Corpus) bool {
	if len(o.Include) == 0 {
		return true
	}
	return o.Include[c]
}

// BuildDataset fetches and parses the registered corpora and merges them. It is
// driven entirely by the registry: each Definition's Fetcher builds its own
// Dataset, so adding a corpus is a Register call, not an edit here.
func BuildDataset(ctx context.Context, opts FetchOptions) (*Dataset, error) {
	merged := &Dataset{Meta: map[Corpus]CorpusMeta{}}
	for _, d := range Registered() {
		if d.Fetcher == nil || !opts.include(d.Corpus) {
			continue
		}
		ds, err := d.Fetcher(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", d.Corpus, err)
		}
		merge(merged, ds)
	}
	return merged, nil
}

// fetchMTGDataset adapts the MTG parser to the registry Fetcher signature.
func fetchMTGDataset(ctx context.Context, opts FetchOptions) (*Dataset, error) {
	return fetchMTG(ctx, opts.MTGURL)
}

// fetchDNDDataset adapts the D&D parser to the registry Fetcher signature.
// When a local docs directory is configured, those documents are parsed with
// the same chunker and ride alongside the SRD, each book also becoming its own
// reader guide.
func fetchDNDDataset(ctx context.Context, opts FetchOptions) (*Dataset, error) {
	ds, err := fetchDND(ctx, opts.DNDRepo, opts.DNDRef)
	if err != nil {
		return nil, err
	}
	if opts.DNDDocsDir != "" {
		records, reader, err := ParseDNDDocs(opts.DNDDocsDir)
		if err != nil {
			return nil, err
		}
		ds.Records = append(ds.Records, records...)
		ds.Reader = append(ds.Reader, reader...)
		if m, ok := ds.Meta[CorpusDND]; ok {
			m.RecordCount = len(ds.Records)
			ds.Meta[CorpusDND] = m
		}
	}
	return ds, nil
}

func fetchMTG(ctx context.Context, url string) (*Dataset, error) {
	if url == "" || url == mtgDefaultURL {
		// Wizards republishes the rules under a new date-stamped filename on
		// every update and removes the old file, so the pinned URL goes stale.
		// Discover the current link from the canonical rules page and fall
		// back to the pin only when discovery fails.
		if discovered := discoverMTGRulesURL(ctx); discovered != "" {
			url = discovered
		} else {
			url = firstNonEmpty(url, mtgDefaultURL)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch mtg rules: %s", resp.Status)
	}
	ds, err := ParseMTG(resp.Body)
	if err != nil {
		return nil, err
	}
	if m, ok := ds.Meta[CorpusMTG]; ok {
		m.SourceURL = url
		ds.Meta[CorpusMTG] = m
	}
	return ds, nil
}

// discoverMTGRulesURL scrapes the canonical Wizards rules page for the link to
// the current Comprehensive Rules text file. It returns "" on any failure so
// callers can fall back to a pinned URL.
func discoverMTGRulesURL(ctx context.Context) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mtgRulesPageURL, nil)
	if err != nil {
		return ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return ""
	}
	return extractMTGRulesLink(string(body))
}

// extractMTGRulesLink pulls the comp-rules .txt link out of a rules-page HTML
// document, percent-encoding the literal space the href carries in its
// date-stamped filename.
func extractMTGRulesLink(body string) string {
	link := mtgRulesLinkRe.FindString(body)
	return strings.ReplaceAll(link, " ", "%20")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func fetchDND(ctx context.Context, repo, ref string) (*Dataset, error) {
	_ = ctx
	gz, err := FetchDND(repo, ref)
	if err != nil {
		return nil, err
	}
	files, err := ExtractDNDMarkdown(gz)
	if err != nil {
		return nil, err
	}
	if ref == "" {
		ref = dndDefaultRef
	}
	return ParseDND(files, ref), nil
}

func merge(dst, src *Dataset) {
	dst.Records = append(dst.Records, src.Records...)
	dst.Reader = append(dst.Reader, src.Reader...)
	for k, v := range src.Meta {
		dst.Meta[k] = v
	}
}

// ParseMTGReader is a convenience for tests that already have the text.
func ParseMTGReader(r io.Reader) (*Dataset, error) { return ParseMTG(r) }
