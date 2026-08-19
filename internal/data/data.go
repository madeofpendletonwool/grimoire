package data

// Corpus identifies a rule system.
type Corpus string

const (
	CorpusMTG Corpus = "mtg"
	CorpusDND Corpus = "dnd"
)

// Record is a single indexable unit of rules text.
type Record struct {
	Corpus Corpus // mtg | dnd
	Number string // rule number / identifier, e.g. "205.1a" or "" for free text
	Title  string // section / heading / glossary term
	Body   string // the rule / definition / section text
	Source string // origin file or section name
}

// Dataset is the full parsed corpus set, ready to index.
type Dataset struct {
	Records []Record
	Meta    map[Corpus]CorpusMeta
	// Reader carries the book-shaped view of the same corpora: the guide /
	// chapter / section tree the reader surface pages through. It is built
	// alongside Records by the parsers and stored separately from the FTS5
	// index, so search and reading never constrain each other.
	Reader []ReaderNode
}

// ReaderNode is one addressable stop in a corpus's reading tree: a chapter, a
// section, a glossary term — any heading a reader can open as a page. Nodes
// nest by Level within a Guide; Position orders them like the pages they came
// from. Bodies are reading-fidelity text (raw markdown for D&D sources, plain
// rule text for MTG), not the flattened FTS bodies.
type ReaderNode struct {
	Corpus     Corpus
	Guide      string // guide slug, unique within a corpus ("rules", "srd:spells", "books:phb")
	GuideTitle string // display title of the guide ("Comprehensive Rules", "Spells")
	GuideKind  string // "rules" | "srd" | "book"
	Number     string // node id, unique within (corpus, guide): "101", "spells/0003/0042"
	Title      string // display title
	Level      int    // 1 = chapter (guide root's children), 2+ = nested sections
	Position   int    // document order within the guide
	Body       string
	Source     string
}

// CorpusMeta holds descriptive metadata about a corpus.
type CorpusMeta struct {
	Name        string // display name
	Version     string // rules version / source ref
	SourceURL   string // where the data came from
	RecordCount int
}
