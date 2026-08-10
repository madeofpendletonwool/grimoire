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
}

// CorpusMeta holds descriptive metadata about a corpus.
type CorpusMeta struct {
	Name        string // display name
	Version     string // rules version / source ref
	SourceURL   string // where the data came from
	RecordCount int
}
