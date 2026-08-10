package data

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"regexp"
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
// into sections at markdown headings. Each section becomes one Record.
func ParseDND(files []mdFile, ref string) *Dataset {
	ds := &Dataset{Meta: map[Corpus]CorpusMeta{}}
	var records []Record

	for _, f := range files {
		records = append(records, chunkMarkdown(f, ref)...)
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

// chunkMarkdown splits a markdown file into records, one per heading section.
// The heading chain (ancestor titles) is joined to give each chunk context.
func chunkMarkdown(f mdFile, _ string) []Record {
	source := strings.TrimSuffix(pathBase(f.Path), ".md")
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
		level  int
		title  string
		parent string
		body   *strings.Builder
	}
	var secs []*section
	var cur *section

	pushSection := func(level int, title string) {
		// ancestors = all stack entries below this level
		parent := joinAncestors(stack, level)
		setLevel(level, title)
		s := &section{level: level, title: title, parent: parent, body: &strings.Builder{}}
		secs = append(secs, s)
		cur = s
	}

	// File-level section catches leading content before any heading.
	cur = &section{level: 0, title: titleize(source), parent: "", body: &strings.Builder{}}
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

	for _, s := range secs {
		body := strings.TrimSpace(s.body.String())
		if body == "" && s.level > 0 {
			continue
		}
		// skip front-matter / tiny stubs
		if len(body) < 3 && s.title == "" {
			continue
		}
		title := s.title
		if s.parent != "" {
			title = s.parent + " — " + s.title
		}
		records = append(records, Record{
			Corpus: CorpusDND,
			Number: "",
			Title:  title,
			Body:   cleanMarkdown(body),
			Source: "D&D 5e SRD — " + source,
		})
	}
	return records
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

// cleanMarkdown strips a few common markdown constructs to keep FTS text clean.
func cleanMarkdown(s string) string {
	// strip tables (lines starting with |) — keep it simple
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "|") {
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

func pathBase(p string) string {
	p = strings.TrimSuffix(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func titleize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
