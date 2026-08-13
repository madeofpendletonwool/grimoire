// Package entities resolves named entities mentioned in a question (cards,
// spells, monsters, items) into authoritative grounding text, one implementation
// per corpus. Each implementation satisfies data.EntityResolver so the corpus
// registry can declare it; the server feeds the resolved text to the Q&A model
// the way MTG card oracle text is fed today.
//
// Resolvers live here rather than under internal/cards so a new corpus's
// resolver is added without touching the MTG-specific card code.
package entities
