package data

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// dndDefaultRef is the default branch of the D&D 5e SRD markdown repo.
const dndDefaultRef = "master"

// dndDefaultRepo is the GitHub "owner/name" of the SRD markdown source.
const dndDefaultRepo = "downfallx/dnd-5e-srd-markdown"

// dndTarballURL returns the codeload tarball URL for a repo + ref.
func dndTarballURL(repo, ref string) string {
	return fmt.Sprintf("https://codeload.github.com/%s/tar.gz/refs/heads/%s", repo, ref)
}

// FetchDND downloads the SRD markdown tarball and returns the raw bytes.
func FetchDND(repo, ref string) ([]byte, error) {
	if repo == "" {
		repo = dndDefaultRepo
	}
	if ref == "" {
		ref = dndDefaultRef
	}
	url := dndTarballURL(repo, ref)
	resp, err := http.Get(url) //nolint:noctx,gosec // static, validated URL
	if err != nil {
		return nil, fmt.Errorf("fetch dnd tarball: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch dnd tarball: %s", resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// headingRe matches a markdown ATX heading, capturing the level and text.
var headingRe = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*$`)

// mdFile maps a path to its raw markdown content.
type mdFile struct {
	Path    string
	Content string
}

// ExtractDNDMarkdown reads a tar.gz archive and returns all .md files.
func ExtractDNDMarkdown(gz []byte) ([]mdFile, error) {
	zr, err := gzip.NewReader(strings.NewReader(string(gz)))
	if err != nil {
		return nil, fmt.Errorf("gunzip dnd tarball: %w", err)
	}
	tr := tar.NewReader(zr)
	var files []mdFile
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read dnd tar entry: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if !strings.HasSuffix(hdr.Name, ".md") {
			continue
		}
		// skip repo meta files and GitHub config/templates
		lower := strings.ToLower(hdr.Name)
		if strings.Contains(lower, "/.github/") || strings.Contains(lower, "\\github\\") {
			continue
		}
		base := pathBase(hdr.Name)
		if base == "README.md" || base == "CHANGELOG.md" || base == "CONTRIBUTING.md" {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("read dnd md %s: %w", hdr.Name, err)
		}
		files = append(files, mdFile{Path: hdr.Name, Content: string(body)})
	}
	return files, nil
}

// ParseDND converts SRD markdown files into a Dataset by splitting each file
// into sections at markdown headings and over-long sections into
// paragraph-bounded chunks (see chunkMarkdown). Each record carries a stable
// path-style Number ("<file>/<section>.<chunk>") used for grounding dedup,
// sibling expansion, and cache keys. The same files also yield the reader tree
// — one guide per file, raw markdown bodies — under guide ids "srd:<slug>".
func ParseDND(files []mdFile, ref string) *Dataset {
	ds := &Dataset{Meta: map[Corpus]CorpusMeta{}}
	var records []Record

	for _, f := range files {
		records = append(records, chunkMarkdown(f, ref)...)
		ds.Reader = append(ds.Reader, buildReaderNodes(f, CorpusDND,
			"srd:"+fileSlug(stripExt(pathBase(f.Path))), docTitle(f), "srd")...)
	}

	ds.Records = records
	ds.Meta[CorpusDND] = CorpusMeta{
		Name:        "D&D 5e SRD",
		Version:     ref,
		SourceURL:   "https://github.com/" + dndDefaultRepo,
		RecordCount: len(records),
	}
	return ds
}

// dndMaxChunkChars bounds a single D&D record's body. The Q&A prompt truncates
// context bodies at 1500 characters, so chunks just under that cap reach the
// model whole instead of losing their tail.
const dndMaxChunkChars = 1400

// DNDSectionKey reduces a D&D record number to its section id: the full
// ordinal path with the chunk suffix dropped — "spells/0003/0042.1" becomes
// "spells/0003/0042". All chunks of one section share it, which is what lets
// retrieval expand a hit into the whole section. A number without a "/" (an
// MTG rule) yields "".
func DNDSectionKey(number string) string {
	if !strings.Contains(number, "/") {
		return ""
	}
	if i := strings.LastIndex(number, "."); i >= 0 {
		return number[:i]
	}
	return number
}

// DNDGroupKey reduces a D&D record number to its top-level group: the file
// slug plus the first ordinal — "spells/0003/0042.1" becomes "spells/0003".
// The group is the D&D analogue of an MTG rule chapter: the whole top-level
// section a hit lands in, pulled in when it fits the grounding budget.
func DNDGroupKey(number string) string {
	key := DNDSectionKey(number)
	if key == "" {
		return ""
	}
	parts := strings.SplitN(key, "/", 3)
	if len(parts) < 2 {
		return ""
	}
	return parts[0] + "/" + parts[1]
}

// chunkMarkdown splits a markdown file into records. Sections are delimited by
// markdown headings, exactly as before, but a section whose cleaned body
// exceeds dndMaxChunkChars is split into paragraph-bounded chunks so no record
// is too large for the Q&A context. Record numbers carry the heading path —
// "<slug>/<ancestor…>/<ordinal>.<chunk>" — so sibling chunks share a section
// key and a whole top-level section shares a group key, mirroring the
// section/chapter duality MTG's numbered rules have. Tables are kept as
// flattened text rows rather than dropped, so tabulated rules stay searchable.
func chunkMarkdown(f mdFile, _ string) []Record {
	source := stripExt(pathBase(f.Path))
	slug := fileSlug(source)
	var records []Record

	lines := strings.Split(f.Content, "\n")

	// ancestor heading titles, indexed by level
	stack := make([]string, 0, 8)
	setLevel := func(level int, title string) {
		if level <= len(stack) {
			stack[level-1] = title
			stack = stack[:level]
		} else {
			// fill gaps
			for len(stack) < level-1 {
				stack = append(stack, "")
			}
			stack = append(stack, title)
		}
	}

	type section struct {
		level   int
		title   string
		parent  string
		ordPath []int // own ordinal plus every ancestor's, root first
		body    *strings.Builder
	}
	var secs []*section
	var cur *section

	// ordStack mirrors the title stack: the ordinal of each open ancestor
	// heading, indexed by level.
	ordStack := make([]int, 0, 8)

	pushSection := func(level int, title string) {
		// ancestors = all stack entries below this level
		parent := joinAncestors(stack, level)
		setLevel(level, title)
		// keep the ordinal stack in step with the title stack
		if level <= len(ordStack) {
			ordStack = ordStack[:level-1]
		} else {
			for len(ordStack) < level-1 {
				ordStack = append(ordStack, -1)
			}
		}
		ord := len(secs)
		ordStack = append(ordStack, ord)
		s := &section{level: level, title: title, parent: parent, ordPath: append([]int(nil), ordStack...), body: &strings.Builder{}}
		secs = append(secs, s)
		cur = s
	}

	// File-level section catches leading content before any heading.
	cur = &section{level: 0, title: titleize(source), parent: "", ordPath: []int{0}, body: &strings.Builder{}}
	secs = append(secs, cur)

	for _, line := range lines {
		if m := headingRe.FindStringSubmatch(line); m != nil {
			level := len(m[1])
			title := strings.TrimSpace(m[2])
			// strip trailing markdown like " {#anchor}"
			if i := strings.Index(title, " {#"); i >= 0 {
				title = strings.TrimSpace(title[:i])
			}
			pushSection(level, title)
			continue
		}
		if cur != nil {
			cur.body.WriteString(line)
			cur.body.WriteByte('\n')
		}
	}

	// Section ordinals are assigned in document order and folded into the
	// record number, so a section and its chunks stay identifiable across
	// rebuilds of identical content. Unheadinged gap levels (-1) are skipped.
	for _, s := range secs {
		body := cleanMarkdown(strings.TrimSpace(s.body.String()))
		if body == "" {
			continue
		}
		title := s.title
		if s.parent != "" {
			title = s.parent + " — " + s.title
		}
		// The ordinal path keeps the heading's level shape: a skipped heading
		// level becomes a literal 0000 segment. That is what distinguishes a
		// document's entry list (spells: "#### X" directly under "## Y" →
		// spells/NNNN/0000/NNNN) from structural subsections ("### X" →
		// spells/NNNN/NNNN) — same file, different depth, different shape.
		// Gap levels before the first heading are not encoded; the file-level
		// root section absorbs them, so a file that starts at "##" does not
		// grow a leading 0000.
		path := s.ordPath
		for len(path) > 0 && path[0] < 0 {
			path = path[1:]
		}
		var pathParts []string
		for _, o := range path {
			if o >= 0 {
				pathParts = append(pathParts, fmt.Sprintf("%04d", o))
			} else {
				pathParts = append(pathParts, "0000")
			}
		}
		prefix := slug + "/" + strings.Join(pathParts, "/")
		chunks := splitBody(body, dndMaxChunkChars)
		for c, chunk := range chunks {
			chunkTitle := title
			if len(chunks) > 1 {
				chunkTitle = fmt.Sprintf("%s (part %d)", title, c+1)
			}
			records = append(records, Record{
				Corpus: CorpusDND,
				Number: fmt.Sprintf("%s.%d", prefix, c),
				Title:  chunkTitle,
				Body:   chunk,
				Source: "D&D 5e SRD — " + source,
			})
		}
	}
	return records
}

// splitBody breaks a cleaned body into chunks of at most max chars, preferring
// paragraph boundaries and falling back to sentence boundaries for a runaway
// paragraph. A final short remainder merges into the previous chunk when it
// fits, so prose is not shredded into stubs.
func splitBody(body string, max int) []string {
	if len(body) <= max {
		return []string{body}
	}
	var chunks []string
	for _, para := range splitParagraphs(body) {
		if len(para) > max {
			// A single paragraph larger than the cap: split at sentences.
			for _, sent := range splitSentences(para, max) {
				if len(chunks) > 0 && len(chunks[len(chunks)-1])+len(sent)+1 <= max {
					chunks[len(chunks)-1] += "\n" + sent
					continue
				}
				chunks = append(chunks, sent)
			}
			continue
		}
		if len(chunks) > 0 && len(chunks[len(chunks)-1])+len(para)+2 <= max {
			chunks[len(chunks)-1] += "\n\n" + para
			continue
		}
		chunks = append(chunks, para)
	}
	if len(chunks) == 0 {
		chunks = append(chunks, body)
	}
	return chunks
}

// splitParagraphs splits on blank lines, dropping empties.
func splitParagraphs(body string) []string {
	raw := strings.Split(body, "\n\n")
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		if strings.TrimSpace(p) != "" {
			out = append(out, strings.TrimSpace(p))
		}
	}
	return out
}

// sentenceEnd finds the next sentence boundary (., !, ?) in s[start:] followed
// by whitespace or end, returning the index just past it, or -1.
func sentenceEnd(s string, start int) int {
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '.', '!', '?':
			j := i + 1
			if j >= len(s) || s[j] == ' ' || s[j] == '\n' {
				return j
			}
		}
	}
	return -1
}

// splitSentences accumulates sentences into pieces of at most max chars. When
// no boundary is found within the cap, a hard split at max keeps progress.
func splitSentences(para string, max int) []string {
	var out []string
	start := 0
	for start < len(para) {
		end := -1
		search := start
		for search-start <= max {
			e := sentenceEnd(para, search)
			if e == -1 || e-start > max {
				break
			}
			end = e
			search = e
		}
		if end == -1 {
			// no sentence boundary within the cap: hard split
			limit := start + max
			if limit > len(para) {
				limit = len(para)
			}
			end = limit
		}
		if end > len(para) {
			end = len(para)
		}
		out = append(out, strings.TrimSpace(para[start:end]))
		start = end
	}
	return out
}

// joinAncestors joins ancestor heading titles for context.
func joinAncestors(stack []string, level int) string {
	var parts []string
	for i := 0; i < level-1 && i < len(stack); i++ {
		if stack[i] != "" {
			parts = append(parts, stack[i])
		}
	}
	return strings.Join(parts, " — ")
}

// readerSection is one heading's worth of a file during the reader scan: its
// heading level (0 for the file root), its title, its ordinal path (mirroring
// chunkMarkdown's numbering so reader nodes and search records share ids), and
// its direct body — the raw markdown between this heading and the next.
type readerSection struct {
	level   int
	title   string
	ordPath []int
	body    strings.Builder
}

// buildReaderNodes walks a file's headings and shapes it into reader nodes:
// the guide root's children are the file's H2s (level 1), their subheadings
// nest below (H3 → level 2, …), and each node's body is the RAW markdown
// directly under its heading — tables and emphasis intact for reading, unlike
// the flattened FTS bodies. Node numbers reuse the record path scheme
// ("spells/0003/0042"), so a search citation can deep-link to its page.
//
// A heading with neither direct body nor descendants is dropped, so a stray
// empty heading from a PDF extraction adds no noise to the table of contents.
func buildReaderNodes(f mdFile, corpus Corpus, guide, guideTitle, guideKind string) []ReaderNode {
	slug := fileSlug(stripExt(pathBase(f.Path)))
	source := guideSourceLabel(guideKind) + " — " + guideTitle
	content := stripBOM(f.Content)

	lines := strings.Split(content, "\n")
	var secs []*readerSection
	var cur *readerSection
	ordStack := make([]int, 0, 8)

	push := func(level int, title string) {
		if level <= len(ordStack) {
			ordStack = ordStack[:level-1]
		} else {
			for len(ordStack) < level-1 {
				ordStack = append(ordStack, -1)
			}
		}
		ordStack = append(ordStack, len(secs))
		secs = append(secs, &readerSection{
			level:   level,
			title:   title,
			ordPath: append([]int(nil), ordStack...),
		})
		cur = secs[len(secs)-1]
	}

	// The file root catches leading content before any heading (an untitled
	// H1's preamble, or a whole heading-less document).
	root := &readerSection{level: 0, title: guideTitle, ordPath: []int{0}}
	secs = append(secs, root)
	cur = root

	for _, line := range lines {
		if m := headingRe.FindStringSubmatch(line); m != nil {
			level := len(m[1])
			title := strings.TrimSpace(m[2])
			if i := strings.Index(title, " {#"); i >= 0 {
				title = strings.TrimSpace(title[:i])
			}
			if level == 1 && len(secs) == 1 {
				// The document's own H1 names the guide; its preamble stays
				// with the root node rather than becoming a sibling chapter.
				root.title = title
				cur = root
				continue
			}
			push(level, title)
			continue
		}
		if cur != nil {
			cur.body.WriteString(line)
			cur.body.WriteByte('\n')
		}
	}

	// hasKids[i]: does section i own any deeper section before a same-or-higher
	// heading closes it? Descendants keep an otherwise childless heading in the
	// tree as a group stop.
	hasKids := make([]bool, len(secs))
	for i := range secs {
		for j := i + 1; j < len(secs); j++ {
			if secs[j].level <= secs[i].level {
				break
			}
			hasKids[i] = true
		}
	}

	pos := 0
	var nodes []ReaderNode
	for i, s := range secs {
		body := strings.TrimSpace(s.body.String())
		isRoot := s.level == 0
		if body == "" && (isRoot || !hasKids[i]) {
			continue // nothing to read and nothing under it
		}
		// Markdown levels map down one: the document's H2s are the guide
		// root's children (reader level 1), their H3s nest at 2, and so on.
		// The H1 named the guide itself; the root node ("Introduction") is
		// level 1 alongside the chapters.
		level := s.level - 1
		if level < 1 {
			level = 1
		}
		title := s.title
		number := readerPathNumber(slug, s.ordPath)
		if isRoot {
			title = "Introduction"
		}
		pos++
		nodes = append(nodes, ReaderNode{
			Corpus: corpus, Guide: guide, GuideTitle: guideTitle, GuideKind: guideKind,
			Number: number, Title: title, Level: level, Position: pos, Body: body, Source: source,
		})
	}
	return nodes
}

// readerPathNumber renders a section's ordinal path as its stable node id,
// using the same shape chunkMarkdown gives records: zero-padded segments under
// the file slug, with skipped heading levels as literal 0000. The root
// section's path is just the slug itself.
func readerPathNumber(slug string, ordPath []int) string {
	path := ordPath
	for len(path) > 0 && path[0] < 0 {
		path = path[1:]
	}
	parts := make([]string, 0, len(path))
	for _, o := range path {
		if o >= 0 {
			parts = append(parts, fmt.Sprintf("%04d", o))
		} else {
			parts = append(parts, "0000")
		}
	}
	return slug + "/" + strings.Join(parts, "/")
}

// guideSourceLabel maps a guide kind to the citation label prefix its nodes
// carry, matching the Source strings the record pipeline already produces.
func guideSourceLabel(kind string) string {
	if kind == "book" {
		return "D&D books"
	}
	return "D&D 5e SRD"
}

// tableRowRe matches a markdown table row: | cell | cell | …
var tableRowRe = regexp.MustCompile(`^\s*\|(.+)\|\s*$`)

// tableSeparatorRe matches a table's alignment row: |---|:---:|---
var tableSeparatorRe = regexp.MustCompile(`^\s*\|[\s:|-]+\|\s*$`)

// htmlTableRe matches a whole HTML table, tags and all.
var htmlTableRe = regexp.MustCompile(`(?is)<table[^>]*>.*?</table>`)

// htmlRowRe matches one HTML table row.
var htmlRowRe = regexp.MustCompile(`(?is)<tr[^>]*>(.*?)</tr>`)

// htmlCellRe matches one HTML table cell, header or data.
var htmlCellRe = regexp.MustCompile(`(?is)<t[dh][^>]*>(.*?)</t[dh]>`)

// htmlTagRe matches any remaining tag, so stray markup never reaches the model.
// A tag name must start with a letter, which keeps prose comparisons ("a DC
// < 10 > the modifier") from being eaten as markup.
var htmlTagRe = regexp.MustCompile(`(?is)</?[a-z][^>]*>`)

// wsRe matches a run of whitespace.
var wsRe = regexp.MustCompile(`\s+`)

// flattenHTMLTables renders HTML tables as text rows, the same shape markdown
// pipe tables get. The SRD's markdown carries its most valuable tables as raw
// HTML — class progression, monster stats, equipment, starting gear — and left
// alone they reach FTS and the model as a wall of <tr>/<td>, which matches no
// query and spends context on markup. Rows of one table stay in one block so
// the chunker keeps the table together.
func flattenHTMLTables(s string) string {
	if !strings.Contains(s, "<") {
		return s
	}
	flattenRows := func(table string) string {
		var rows []string
		for _, m := range htmlRowRe.FindAllStringSubmatch(table, -1) {
			var cells []string
			for _, c := range htmlCellRe.FindAllStringSubmatch(m[1], -1) {
				cell := strings.TrimSpace(wsRe.ReplaceAllString(htmlTagRe.ReplaceAllString(c[1], ""), " "))
				if cell != "" {
					cells = append(cells, cell)
				}
			}
			if len(cells) > 0 {
				rows = append(rows, strings.Join(cells, " — "))
			}
		}
		if len(rows) == 0 {
			return ""
		}
		return "\n" + strings.Join(rows, "\n") + "\n"
	}
	s = htmlTableRe.ReplaceAllStringFunc(s, flattenRows)
	// Rows outside a <table> (a table the source never closed) still flatten.
	s = htmlRowRe.ReplaceAllStringFunc(s, func(row string) string {
		return flattenRows("<table>" + row + "</table>")
	})
	return htmlTagRe.ReplaceAllString(s, "")
}

// cleanMarkdown normalizes a markdown section body into plain text for FTS.
// Tables are flattened rather than dropped — each row becomes "cell — cell"
// on its own line — so tabulated rules (equipment, class tables, spell lists)
// stay searchable. A table with a header row keeps it, which is enough for the
// row text to carry its own context. Both table dialects the sources use are
// handled: markdown pipe rows below, and HTML tables first.
func cleanMarkdown(s string) string {
	s = flattenHTMLTables(s)
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "|") {
			if tableSeparatorRe.MatchString(t) {
				continue
			}
			if m := tableRowRe.FindStringSubmatch(t); m != nil {
				b.WriteString(flattenTableRow(m[1]))
				b.WriteByte('\n')
			}
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	out := b.String()
	// strip image/inline-link syntax noise but keep link text
	linkRe := regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`)
	out = linkRe.ReplaceAllString(out, "$1")
	boldRe := regexp.MustCompile(`\*\*([^*]+)\*\*`)
	out = boldRe.ReplaceAllString(out, "$1")
	italicStar := regexp.MustCompile(`\*([^*]+)\*`)
	out = italicStar.ReplaceAllString(out, "$1")
	italicUnder := regexp.MustCompile(`_([^_\n]+)_`)
	out = italicUnder.ReplaceAllString(out, "$1")
	out = strings.TrimSpace(out)
	return out
}

// flattenTableRow renders one markdown table row as compact text:
// cells joined with " — ", empty cells dropped.
func flattenTableRow(row string) string {
	cells := strings.Split(row, "|")
	parts := make([]string, 0, len(cells))
	for _, c := range cells {
		c = strings.TrimSpace(c)
		if c != "" {
			parts = append(parts, c)
		}
	}
	return strings.Join(parts, " — ")
}

// fileSlug converts a file name into a stable identifier for record numbers:
// lowercase, alphanumerics and hyphens only.
func fileSlug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ', r == '-', r == '_', r == '.', r == '\'', r == '’':
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	if s == "" {
		return "doc"
	}
	return s
}

// ParseDNDDocs imports local D&D documents (markdown or plain text) from a
// directory and returns their records. Local books — extracted PDFs, house
// documents — ride alongside the fetched SRD: same chunker, same numbering,
// distinct Source so citations can name the book. Files are read
// deterministically by name; unknown extensions are skipped. Each book also
// becomes its own reader guide ("books:<slug>"), so it reads as the book it
// came from rather than dissolving into the SRD.
func ParseDNDDocs(dir string) ([]Record, []ReaderNode, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("read dnd docs dir: %w", err)
	}
	var files []mdFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := strings.ToLower(e.Name())
		if !strings.HasSuffix(name, ".md") && !strings.HasSuffix(name, ".markdown") && !strings.HasSuffix(name, ".txt") {
			continue
		}
		if name == "readme.md" || strings.HasPrefix(name, ".") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, nil, fmt.Errorf("read dnd doc %s: %w", e.Name(), err)
		}
		files = append(files, mdFile{Path: e.Name(), Content: string(raw)})
	}
	if len(files) == 0 {
		return nil, nil, nil
	}
	var records []Record
	var reader []ReaderNode
	for _, f := range files {
		label := docTitle(f)
		slug := fileSlug(stripExt(pathBase(f.Path)))
		for _, r := range chunkMarkdown(f, "") {
			r.Source = "D&D books — " + label
			records = append(records, r)
		}
		reader = append(reader, buildReaderNodes(f, CorpusDND, "books:"+slug, label, "book")...)
	}
	return records, reader, nil
}

// docTitle names a local document for citation. A document's own H1 wins — the
// PDF extractor writes the book's real title there ("Xanathar's Guide to
// Everything", "Sage Advice Compendium"), which is what a reader should see
// under an answer and what the prompt's Sage Advice rule matches on. A file
// without an H1 falls back to its name.
func docTitle(f mdFile) string {
	for _, line := range strings.Split(stripBOM(f.Content), "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if m := headingRe.FindStringSubmatch(t); m != nil && len(m[1]) == 1 {
			if title := strings.TrimSpace(m[2]); title != "" {
				return title
			}
		}
		break // content before any H1 means the file has no title line
	}
	return titleize(strings.TrimSuffix(f.Path, filepath.Ext(f.Path)))
}

// stripBOM removes a leading UTF-8 byte-order mark. The SRD's markdown ships
// with one, and it would otherwise hide the first heading from the title scan.
func stripBOM(s string) string {
	return strings.TrimPrefix(s, "\ufeff")
}

// DNDDocsFingerprint summarizes a local-docs directory's contents — file
// names, sizes, modification times — so a boot can detect that the books
// changed since the index was built and rebuild without anyone running a
// command. Content is not hashed: these are multi-megabyte books, and
// name+size+mtime catches every edit a person makes. A missing or empty
// directory yields a stable empty fingerprint.
func DNDDocsFingerprint(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var parts []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s:%d:%d", e.Name(), info.Size(), info.ModTime().UnixNano()))
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

func pathBase(p string) string {
	p = strings.TrimSuffix(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// stripExt removes a trailing file extension (".md", ".txt") from a name.
func stripExt(name string) string {
	if i := strings.LastIndex(name, "."); i > 0 {
		return name[:i]
	}
	return name
}

func titleize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
