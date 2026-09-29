// Package snapshot holds immutable project snapshots and versioned documents.
//
// A Snapshot is a frozen view of every open document in a session. Editing a
// document (including unsaved editor overlays) produces a new Snapshot instead
// of mutating the previous one, so navigation results can always be bound to a
// concrete document version and source text.
package snapshot

import (
	"fmt"
	"sort"
)

// URI identifies a document inside one session, e.g. "file:///proj/main.edu".
type URI string

// Document is one immutable version of a file's text.
type Document struct {
	URI     URI    `json:"uri"`
	Version int    `json:"version"`
	Text    string `json:"text"`
	Saved   bool   `json:"saved"`
}

// Slice returns the source text covered by a byte-offset range. It panics on
// out-of-bounds ranges, which would indicate a position-tracking bug.
func (d Document) Slice(start, end int) (string, error) {
	if start < 0 || end < start || end > len(d.Text) {
		return "", fmt.Errorf("range [%d,%d) out of bounds for %s (len=%d)", start, end, d.URI, len(d.Text))
	}
	return d.Text[start:end], nil
}

// Snapshot is an immutable, read-only map of URI -> Document.
type Snapshot struct {
	ID    int64
	docs  map[URI]Document
	order []URI
}

// NewSnapshot creates a snapshot from the given documents.
func NewSnapshot(id int64, docs map[URI]Document) *Snapshot {
	s := &Snapshot{ID: id, docs: make(map[URI]Document, len(docs))}
	for u, d := range docs {
		d.URI = u
		s.docs[u] = d
		s.order = append(s.order, u)
	}
	sort.Slice(s.order, func(i, j int) bool { return s.order[i] < s.order[j] })
	return s
}

// Get returns the document and whether the URI is present.
func (s *Snapshot) Get(u URI) (Document, bool) {
	d, ok := s.docs[u]
	return d, ok
}

// URIs returns all document URIs in deterministic order.
func (s *Snapshot) URIs() []URI { return append([]URI(nil), s.order...) }

// WithDocument returns a new Snapshot containing one replaced/inserted
// document; the receiver is left untouched.
func (s *Snapshot) WithDocument(id int64, d Document) *Snapshot {
	next := make(map[URI]Document, len(s.docs)+1)
	for u, doc := range s.docs {
		next[u] = doc
	}
	next[d.URI] = d
	return NewSnapshot(id, next)
}
