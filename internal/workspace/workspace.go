// Package workspace ties snapshots, syntax and semantic analysis into a
// session. It owns the current snapshot, accepts unsaved editor overlays,
// performs incremental re-analysis and answers symbol/reference queries whose
// ranges are bound to concrete document versions.
package workspace

import (
	"fmt"
	"sort"
	"sync"

	"sourcenav/internal/semantic"
	"sourcenav/internal/snapshot"
	"sourcenav/internal/syntax"
)

// Range is the wire representation of a source range, including the exact
// source text covered, so callers can verify the location against real bytes.
type Range struct {
	syntax.Range
	Text string `json:"text"`
}

// Location is one versioned identifier location.
type Location struct {
	URI     snapshot.URI `json:"uri"`
	Version int          `json:"version"`
	Range   Range        `json:"range"`
	Role    string       `json:"role"`
	Kind    string       `json:"kind"`
}

// VersionInfo binds a response to the snapshot and per-document versions it
// was computed against.
type VersionInfo struct {
	SnapshotID int64          `json:"snapshotId"`
	Documents  map[string]int `json:"documents"`
}

// DefinitionResult answers a definition query.
type DefinitionResult struct {
	Version    VersionInfo `json:"version"`
	SymbolID   uint64      `json:"symbolId"`
	SymbolName string      `json:"symbolName"`
	Kind       string      `json:"kind"`
	Location   *Location   `json:"location,omitempty"`
}

// ReferencesResult answers a references query.
type ReferencesResult struct {
	Version     VersionInfo `json:"version"`
	SymbolID    uint64      `json:"symbolId"`
	SymbolName  string      `json:"symbolName"`
	Declaration *Location   `json:"declaration,omitempty"`
	References  []Location  `json:"references"`
}

// DocumentInfo describes one document in the current snapshot.
type DocumentInfo struct {
	URI     snapshot.URI `json:"uri"`
	Version int          `json:"version"`
	Saved   bool         `json:"saved"`
	Bytes   int          `json:"bytes"`
}

// StatusResult summarizes the session.
type StatusResult struct {
	Version   VersionInfo    `json:"version"`
	Documents []DocumentInfo `json:"documents"`
}

type fileState struct {
	doc      snapshot.Document
	tree     *syntax.File
	analysis *semantic.FileAnalysis
}

// Workspace is a single navigation session, safe for concurrent use.
type Workspace struct {
	mu           sync.Mutex
	snap         *snapshot.Snapshot
	nextSnap     int64
	reg          *semantic.Registry
	files        map[snapshot.URI]*fileState
	parseErrs    map[snapshot.URI]error
	moveUndo     *moveUndoState
	nextMoveUndo int64
}

// New creates an empty workspace.
func New() *Workspace {
	return &Workspace{
		nextSnap:     1,
		nextMoveUndo: 1,
		reg:          semantic.NewRegistry(),
		files:        make(map[snapshot.URI]*fileState),
		parseErrs:    make(map[snapshot.URI]error),
	}
}

// Open inserts or replaces a document. saved=false models an unsaved editor
// buffer whose contents must still drive the current session.
func (w *Workspace) Open(uri snapshot.URI, text string, version int, saved bool) VersionInfo {
	w.mu.Lock()
	defer w.mu.Unlock()
	doc := snapshot.Document{URI: uri, Text: text, Version: version, Saved: saved}
	w.snap = w.snapshot().WithDocument(w.nextSnap, doc)
	w.nextSnap++
	w.rebuild(uri)
	return w.versionInfoLocked()
}

func (w *Workspace) snapshot() *snapshot.Snapshot {
	if w.snap == nil {
		return snapshot.NewSnapshot(0, nil)
	}
	return w.snap
}

// ApplyEdit replaces one document's text (an unsaved change) and incrementally
// updates analysis. Only the edited file is re-parsed; other files have their
// precomputed occurrences reused, so unaffected references are never rebuilt.
func (w *Workspace) ApplyEdit(uri snapshot.URI, text string, version int) (VersionInfo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.snapshot().Get(uri); !ok {
		return VersionInfo{}, fmt.Errorf("unknown document %q", uri)
	}
	doc := snapshot.Document{URI: uri, Text: text, Version: version, Saved: false}
	w.snap = w.snapshot().WithDocument(w.nextSnap, doc)
	w.nextSnap++
	w.rebuild(uri)
	return w.versionInfoLocked(), nil
}

// Save marks the current version of a document as persisted on disk.
func (w *Workspace) Save(uri snapshot.URI) (VersionInfo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	doc, ok := w.snapshot().Get(uri)
	if !ok {
		return VersionInfo{}, fmt.Errorf("unknown document %q", uri)
	}
	doc.Saved = true
	w.snap = w.snapshot().WithDocument(w.nextSnap, doc)
	w.nextSnap++
	state := w.files[uri]
	if state != nil {
		state.doc = doc
	}
	return w.versionInfoLocked(), nil
}

// rebuild re-parses and re-analyses one file. Global identities across files
// are registered up front from every file's syntax tree, so changing one
// document only requires a local re-parse plus rebinding of names it defines.
func (w *Workspace) rebuild(changed snapshot.URI) {
	doc, ok := w.snapshot().Get(changed)
	if !ok {
		delete(w.files, changed)
		delete(w.parseErrs, changed)
		return
	}
	oldTree := (*syntax.File)(nil)
	if st := w.files[changed]; st != nil {
		oldTree = st.tree
	}
	affected := map[string]bool{}
	for _, n := range globalNames(oldTree) {
		affected[n] = true
	}
	tree, err := syntax.Parse(doc.Text)
	if err != nil {
		delete(w.files, changed)
		w.parseErrs[changed] = err
	} else {
		delete(w.parseErrs, changed)
		for _, n := range globalNames(tree) {
			affected[n] = true
		}
	}

	// Pass 1 (syntax only): register every project-global name and kind from
	// every parseable file. This makes mutual references resolvable regardless
	// of declaration order and across files.
	live := map[string]bool{}
	for _, u := range w.snapshot().URIs() {
		st := w.files[u]
		var t *syntax.File
		switch {
		case u == changed:
			t = tree
		case st != nil:
			t = st.tree
		default:
			continue
		}
		if t == nil {
			continue
		}
		for _, s := range t.Stmts {
			switch n := s.(type) {
			case *syntax.FnStmt:
				w.reg.Global(n.Name, semantic.KindFunc)
				live[n.Name] = true
			case *syntax.LetStmt:
				w.reg.Global(n.Name, semantic.KindVar)
				live[n.Name] = true
			}
		}
	}
	w.reg.PruneGlobals(live)

	if tree != nil {
		a, aerr := semantic.AnalyzeFile(w.reg, doc, tree)
		if aerr != nil {
			w.parseErrs[changed] = aerr
		} else {
			w.files[changed] = &fileState{doc: doc, tree: tree, analysis: a}
		}
	}

	// Incremental rebinding: only other files that mention an affected global
	// name are re-analysed. Their cached syntax trees are reused (no re-parse),
	// and occurrences of untouched symbols keep identical stable identities.
	for u, st := range w.files {
		if u == changed || st == nil {
			continue
		}
		if !mentionsAny(st.analysis, affected) {
			continue
		}
		a, aerr := semantic.AnalyzeFile(w.reg, st.doc, st.tree)
		if aerr == nil {
			st.analysis = a
		}
	}
}

func globalNames(t *syntax.File) []string {
	if t == nil {
		return nil
	}
	var names []string
	for _, s := range t.Stmts {
		switch n := s.(type) {
		case *syntax.FnStmt:
			names = append(names, n.Name)
		case *syntax.LetStmt:
			names = append(names, n.Name)
		}
	}
	return names
}

func mentionsAny(a *semantic.FileAnalysis, names map[string]bool) bool {
	for _, o := range a.Occurrences {
		if names[o.Symbol.Name] {
			return true
		}
	}
	return false
}

func (w *Workspace) versionInfoLocked() VersionInfo {
	vi := VersionInfo{SnapshotID: w.snap.ID, Documents: make(map[string]int)}
	for _, u := range w.snap.URIs() {
		if d, ok := w.snap.Get(u); ok {
			vi.Documents[string(u)] = d.Version
		}
	}
	return vi
}

// findOccurrence returns the use/declaration occurrence covering offset.
func (w *Workspace) findOccurrence(uri snapshot.URI, offset int) (semantic.Occurrence, bool) {
	st, ok := w.files[uri]
	if !ok {
		return semantic.Occurrence{}, false
	}
	for _, o := range st.analysis.Occurrences {
		if o.Range.Contains(offset) {
			return o, true
		}
	}
	return semantic.Occurrence{}, false
}

func (w *Workspace) toLocation(o semantic.Occurrence) Location {
	doc, _ := w.snapshot().Get(o.URI)
	loc := Location{
		URI: o.URI, Version: doc.Version,
		Range: Range{Range: o.Range},
		Kind:  o.Symbol.Kind.String(),
	}
	if text, err := doc.Slice(o.Range.Start, o.Range.End); err == nil {
		loc.Range.Text = text
	}
	if o.Role == semantic.RoleDecl {
		loc.Role = "declaration"
	} else {
		loc.Role = "reference"
	}
	return loc
}

// Definition resolves the declaration of the identifier at uri/offset.
func (w *Workspace) Definition(uri snapshot.URI, offset int) (DefinitionResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	o, ok := w.findOccurrence(uri, offset)
	if !ok {
		if perr := w.parseErrs[uri]; perr != nil {
			return DefinitionResult{}, perr
		}
		return DefinitionResult{}, fmt.Errorf("no symbol at offset %d in %q", offset, uri)
	}
	res := DefinitionResult{
		Version: w.versionInfoLocked(), SymbolID: o.Symbol.ID,
		SymbolName: o.Symbol.Name, Kind: o.Symbol.Kind.String(),
	}
	if loc, found := w.declarationOf(o.Symbol); found {
		res.Location = &loc
	}
	return res, nil
}

func (w *Workspace) declarationOf(sym *semantic.Symbol) (Location, bool) {
	for _, u := range w.snapshot().URIs() {
		st := w.files[u]
		if st == nil {
			continue
		}
		for _, o := range st.analysis.Occurrences {
			if o.Role == semantic.RoleDecl && o.Symbol.ID == sym.ID {
				return w.toLocation(o), true
			}
		}
	}
	// Local symbols: the declaration lives in the same file and was emitted
	// before uses; scan it explicitly.
	return Location{}, false
}

// References returns every occurrence of the identifier at uri/offset,
// including its declaration. Results are ordered by file then position and
// carry each document's version.
func (w *Workspace) References(uri snapshot.URI, offset int) (ReferencesResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	o, ok := w.findOccurrence(uri, offset)
	if !ok {
		if perr := w.parseErrs[uri]; perr != nil {
			return ReferencesResult{}, perr
		}
		return ReferencesResult{}, fmt.Errorf("no symbol at offset %d in %q", offset, uri)
	}
	res := ReferencesResult{
		Version: w.versionInfoLocked(), SymbolID: o.Symbol.ID, SymbolName: o.Symbol.Name,
	}
	var locs []Location
	for _, u := range w.snapshot().URIs() {
		st := w.files[u]
		if st == nil {
			continue
		}
		for _, occ := range st.analysis.Occurrences {
			if occ.Symbol.ID != o.Symbol.ID {
				continue
			}
			loc := w.toLocation(occ)
			if occ.Role == semantic.RoleDecl {
				l := loc
				res.Declaration = &l
			} else {
				locs = append(locs, loc)
			}
		}
	}
	sort.Slice(locs, func(i, j int) bool {
		if locs[i].URI != locs[j].URI {
			return locs[i].URI < locs[j].URI
		}
		return locs[i].Range.Start < locs[j].Range.Start
	})
	res.References = locs
	return res, nil
}

// Status reports snapshot, versions and parse diagnostics.
func (w *Workspace) Status() StatusResult {
	w.mu.Lock()
	defer w.mu.Unlock()
	res := StatusResult{Version: w.versionInfoLocked()}
	for _, u := range w.snapshot().URIs() {
		doc, _ := w.snapshot().Get(u)
		info := DocumentInfo{URI: u, Version: doc.Version, Saved: doc.Saved, Bytes: len(doc.Text)}
		res.Documents = append(res.Documents, info)
	}
	return res
}

// ParseError returns the current syntax error for a file, if any.
func (w *Workspace) ParseError(uri snapshot.URI) error { return w.parseErrs[uri] }

// Diagnostics returns semantic diagnostics reported for one document.
func (w *Workspace) Diagnostics(uri snapshot.URI) []semantic.Diagnostic {
	w.mu.Lock()
	defer w.mu.Unlock()
	if st := w.files[uri]; st != nil {
		return append([]semantic.Diagnostic(nil), st.analysis.Diagnostics...)
	}
	return nil
}
