package workspace

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"sourcenav/internal/semantic"
	"sourcenav/internal/snapshot"
	"sourcenav/internal/syntax"
)

// ErrStaleMovePlan is returned by ApplyMove or UndoMove when a document the
// plan depends on changed after the preview was computed. Nothing is
// modified; the operator must re-run PrepareMove against the current
// snapshot. Edits to documents the plan does not touch never invalidate it.
var ErrStaleMovePlan = errors.New("move plan is stale: a touched document changed after preview")

// Reference decisions understood by move plans.
const (
	// MoveDecisionSwitch rewrites the reference to the new exported name.
	MoveDecisionSwitch = "switch"
	// MoveDecisionKeep leaves the reference on the old name; it then depends
	// on the compatibility entry kept at the old location.
	MoveDecisionKeep = "keep"
	// MoveDecisionUnchanged needs no edit (the exported name is unchanged).
	MoveDecisionUnchanged = "unchanged"
)

// MoveItemSpec identifies one top-level declaration to move, by the document
// and offset of its name, plus the requested compatibility behaviour.
type MoveItemSpec struct {
	URI    snapshot.URI `json:"uri"`
	Offset int          `json:"offset"`
	// NewName optionally re-exports the declaration under a different name
	// in the target module. Empty keeps the current name.
	NewName string `json:"newName,omitempty"`
	// KeepCompat asks for a forwarding entry under the old name at the old
	// location, so users that cannot switch keep their meaning.
	KeepCompat bool `json:"keepCompat,omitempty"`
}

// MoveEdit is one text replacement inside a document. OldText records the
// bytes covered at preview time so ApplyMove can verify them before writing.
// A zero-width range with empty OldText is a pure insertion.
type MoveEdit struct {
	Range   Range  `json:"range"`
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
}

// MoveFileEdit groups the edits a plan would perform in one document, bound
// to the document version they were computed from.
type MoveFileEdit struct {
	URI     snapshot.URI `json:"uri"`
	Version int          `json:"version"`
	Saved   bool         `json:"saved"`
	Edits   []MoveEdit   `json:"edits"`
}

// MoveCompat describes the forwarding entry left at the old location so
// users that keep the old name retain their original meaning.
type MoveCompat struct {
	URI  snapshot.URI `json:"uri"`
	Text string       `json:"text"`
}

// MoveReference is one reference to a moved symbol together with the
// decision the operator confirmed for it. Forced references cannot be
// switched without changing meaning (e.g. the new name is shadowed by a
// local in that scope); ApplyMove rejects plans that override them.
type MoveReference struct {
	Location Location `json:"location"`
	Decision string   `json:"decision"`
	Forced   bool     `json:"forced,omitempty"`
	Reason   string   `json:"reason,omitempty"`
}

// MoveShadowed is a same-name occurrence that is NOT bound to the moved
// symbol (a shadowing local or an unrelated declaration). It is shown for
// transparency and is never edited.
type MoveShadowed struct {
	Location Location `json:"location"`
	Reason   string   `json:"reason"`
}

// MoveItemPlan is the preview of moving one declaration.
type MoveItemPlan struct {
	SymbolID    uint64          `json:"symbolId"`
	Name        string          `json:"name"`
	NewName     string          `json:"newName"`
	Kind        string          `json:"kind"`
	From        snapshot.URI    `json:"from"`
	Declaration Location        `json:"declaration"`
	KeepCompat  bool            `json:"keepCompat"`
	Compat      *MoveCompat     `json:"compat,omitempty"`
	Removal     MoveEdit        `json:"removal"`
	InsertText  string          `json:"insertText"`
	References  []MoveReference `json:"references"`
	Shadowed    []MoveShadowed  `json:"shadowed,omitempty"`
}

// MoveIssue describes one problem found while previewing a move.
type MoveIssue struct {
	Message string       `json:"message"`
	URI     snapshot.URI `json:"uri,omitempty"`
}

// MoveIssues groups the problems of a multi-declaration move by kind so the
// operator can narrow the scope and re-preview:
//   - Conflicts: name collisions the move would create.
//   - Cycles: import (file dependency) cycles newly introduced by the move.
//   - Unresolved: references whose post-move meaning cannot be guaranteed.
type MoveIssues struct {
	Conflicts  []MoveIssue `json:"conflicts,omitempty"`
	Cycles     []MoveIssue `json:"cycles,omitempty"`
	Unresolved []MoveIssue `json:"unresolved,omitempty"`
}

// Blocking reports whether the issues prevent applying the plan. Cycles are
// informational; conflicts and unresolved references must be resolved by
// narrowing the scope and re-previewing.
func (i MoveIssues) Blocking() bool {
	return len(i.Conflicts) > 0 || len(i.Unresolved) > 0
}

// MoveImportEdge is one file-level dependency edge (From uses a global
// declared in To) as it would look after the move. New marks edges the move
// itself introduces.
type MoveImportEdge struct {
	From  snapshot.URI `json:"from"`
	To    snapshot.URI `json:"to"`
	Names []string     `json:"names"`
	New   bool         `json:"new,omitempty"`
}

// MoveExportChange describes how one module's exported (project-global)
// names change under the plan.
type MoveExportChange struct {
	URI     snapshot.URI `json:"uri"`
	Added   []string     `json:"added,omitempty"`
	Removed []string     `json:"removed,omitempty"`
}

// MovePlan is the confirmed preview of moving one or more top-level
// declarations to another module. It is valid only against the snapshot and
// per-document versions captured in Version and per location.
type MovePlan struct {
	Version       VersionInfo        `json:"version"`
	Target        snapshot.URI       `json:"target"`
	TargetVersion int                `json:"targetVersion"`
	Items         []MoveItemPlan     `json:"items"`
	Issues        MoveIssues         `json:"issues"`
	Imports       []MoveImportEdge   `json:"imports,omitempty"`
	Exports       []MoveExportChange `json:"exports,omitempty"`
	Files         []MoveFileEdit     `json:"files"`
}

// MoveResult reports an applied move. UndoToken restores the pre-move state
// via UndoMove as long as none of the touched documents changed since.
type MoveResult struct {
	Version   VersionInfo    `json:"version"`
	UndoToken int64          `json:"undoToken"`
	Documents []DocumentInfo `json:"documents"`
}

// moveUndoState captures everything UndoMove needs to restore the original
// declaration ownership and reference meaning.
type moveUndoState struct {
	token int64
	prior map[snapshot.URI]snapshot.Document
	post  map[snapshot.URI]int
}

// moveItemInfo is the resolved form of one MoveItemSpec.
type moveItemInfo struct {
	spec    MoveItemSpec
	sym     *semantic.Symbol
	stmt    syntax.Stmt
	decl    semantic.Occurrence
	name    string
	newName string
}

// PrepareMove previews moving the top-level declarations identified by items
// into the module target. Nothing is modified. The preview lists, per
// declaration, every cross-file reference with a compatibility decision
// (switch to the new location/name vs. keep the old entry), same-name
// shadowing occurrences that stay untouched, the compatibility entry text
// when one is needed, the resulting import relations and export changes,
// and — for multi-declaration moves — name conflicts, newly introduced
// dependency cycles and references whose meaning cannot be determined. The
// operator may narrow the scope (drop items) and re-preview at any time.
func (w *Workspace) PrepareMove(target snapshot.URI, items []MoveItemSpec) (MovePlan, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(items) == 0 {
		return MovePlan{}, fmt.Errorf("no declarations selected")
	}
	targetDoc, ok := w.snapshot().Get(target)
	if !ok {
		return MovePlan{}, fmt.Errorf("target module %q is not open", target)
	}
	if perr := w.parseErrs[target]; perr != nil {
		return MovePlan{}, fmt.Errorf("target module %s does not parse: %w", target, perr)
	}

	plan := MovePlan{
		Version:       w.versionInfoLocked(),
		Target:        target,
		TargetVersion: targetDoc.Version,
	}

	seen := map[uint64]bool{}
	var infos []*moveItemInfo
	for _, spec := range items {
		info, err := w.resolveMoveItem(spec, target)
		if err != nil {
			return MovePlan{}, err
		}
		if seen[info.sym.ID] {
			return MovePlan{}, fmt.Errorf("declaration %q selected twice", info.name)
		}
		seen[info.sym.ID] = true
		infos = append(infos, info)
	}

	// Files that cannot be analysed make their references undeterminable.
	for _, u := range w.snapshot().URIs() {
		if perr := w.parseErrs[u]; perr != nil {
			plan.Issues.Unresolved = append(plan.Issues.Unresolved, MoveIssue{
				URI:     u,
				Message: fmt.Sprintf("references in %s cannot be determined: %v", u, perr),
			})
		}
	}

	exported := map[string]string{} // post-move exported name -> origin description
	for _, info := range infos {
		item := w.previewMoveItem(target, info, &plan.Issues)
		if origin, dup := exported[item.NewName]; dup {
			plan.Issues.Conflicts = append(plan.Issues.Conflicts, MoveIssue{
				Message: fmt.Sprintf("name %q would be exported both by %s and by moved declaration %q",
					item.NewName, origin, info.name),
			})
		}
		exported[item.NewName] = fmt.Sprintf("moved declaration %q", info.name)
		if item.KeepCompat {
			if origin, dup := exported[item.Name]; dup {
				plan.Issues.Conflicts = append(plan.Issues.Conflicts, MoveIssue{
					Message: fmt.Sprintf("compatibility entry %q in %s collides with %s",
						item.Name, item.From, origin),
				})
			}
			exported[item.Name] = fmt.Sprintf("compatibility entry for %q", info.name)
		}
		plan.Items = append(plan.Items, *item)
	}

	plan.Imports, plan.Issues.Cycles = w.moveImportRelations(target, infos, plan.Items)
	plan.Exports = moveExportChanges(target, plan.Items)

	docs := map[snapshot.URI]snapshot.Document{}
	for _, u := range w.snapshot().URIs() {
		doc, _ := w.snapshot().Get(u)
		docs[u] = doc
	}
	plan.Files = buildMoveFileEdits(target, docs, plan.Items)
	return plan, nil
}

func (w *Workspace) resolveMoveItem(spec MoveItemSpec, target snapshot.URI) (*moveItemInfo, error) {
	occ, ok := w.findOccurrence(spec.URI, spec.Offset)
	if !ok {
		if perr := w.parseErrs[spec.URI]; perr != nil {
			return nil, perr
		}
		return nil, fmt.Errorf("no symbol at offset %d in %q", spec.Offset, spec.URI)
	}
	if occ.Role != semantic.RoleDecl || occ.Symbol.LocalKey != "" {
		return nil, fmt.Errorf("only top-level public declarations can be moved; %q at offset %d is not one",
			occ.Symbol.Name, spec.Offset)
	}
	st := w.files[spec.URI]
	var stmt syntax.Stmt
	for _, s := range st.tree.Stmts {
		switch n := s.(type) {
		case *syntax.FnStmt:
			if n.NameRange == occ.Range {
				stmt = s
			}
		case *syntax.LetStmt:
			if n.NameRange == occ.Range {
				stmt = s
			}
		}
	}
	if stmt == nil {
		return nil, fmt.Errorf("declaration of %q is not a top-level statement", occ.Symbol.Name)
	}
	name := occ.Symbol.Name
	newName := spec.NewName
	if newName == "" {
		newName = name
	}
	if !syntax.IsIdent(newName) {
		return nil, fmt.Errorf("invalid identifier %q", newName)
	}
	if newName == name && spec.URI == target {
		return nil, fmt.Errorf("declaration %q is already in %s with that name", name, target)
	}
	if spec.KeepCompat && newName == name {
		return nil, fmt.Errorf("keeping a compatibility entry for %q requires a new exported name", name)
	}
	return &moveItemInfo{spec: spec, sym: occ.Symbol, stmt: stmt, decl: occ, name: name, newName: newName}, nil
}

// previewMoveItem classifies every occurrence related to one moved symbol:
// references get a compatibility decision, same-name occurrences bound to
// other symbols are reported as shadowed, and the removal/insert/compat
// texts are computed.
func (w *Workspace) previewMoveItem(target snapshot.URI, info *moveItemInfo, issues *MoveIssues) *MoveItemPlan {
	from := info.spec.URI
	fromDoc, _ := w.snapshot().Get(from)
	fromState := w.files[from]
	stmtRange := moveStmtRange(info.stmt)

	item := &MoveItemPlan{
		SymbolID:    info.sym.ID,
		Name:        info.name,
		NewName:     info.newName,
		Kind:        info.sym.Kind.String(),
		From:        from,
		Declaration: w.toLocation(info.decl),
		KeepCompat:  info.spec.KeepCompat,
	}

	if info.newName != info.name {
		if _, exists := w.reg.GlobalExisting(info.newName); exists {
			issues.Conflicts = append(issues.Conflicts, MoveIssue{
				Message: fmt.Sprintf("name %q already denotes a project-global symbol; exporting moved %q as %q in %s would merge their meanings",
					info.newName, info.name, info.newName, target),
			})
		}
	}
	if target != from {
		if tst := w.files[target]; tst != nil {
			if _, dup := tst.analysis.GlobalDecl(info.newName); dup {
				issues.Conflicts = append(issues.Conflicts, MoveIssue{
					URI: target,
					Message: fmt.Sprintf("target module %s already declares %q; moving %q there would merge two declarations",
						target, info.newName, info.name),
				})
			}
		}
	}

	// Removal of the original statement, swallowing one adjacent newline.
	rStart, rEnd := removalSpan(fromDoc.Text, stmtRange)
	rText, _ := fromDoc.Slice(rStart, rEnd)
	item.Removal = MoveEdit{
		Range:   Range{Range: syntax.Range{Start: rStart, End: rEnd}, Text: rText},
		OldText: rText,
		NewText: "",
	}

	// Text inserted into the target module: the declaration itself, with
	// every occurrence of the moved symbol inside it (its name plus
	// recursive references) rewritten to the new exported name.
	declText, _ := fromDoc.Slice(stmtRange.Start, stmtRange.End)
	if info.newName != info.name {
		var inner []semantic.Occurrence
		for _, occ := range fromState.analysis.Occurrences {
			if occ.Symbol.ID == info.sym.ID && occ.Range.Start >= stmtRange.Start && occ.Range.End <= stmtRange.End {
				inner = append(inner, occ)
			}
		}
		sort.Slice(inner, func(i, j int) bool { return inner[i].Range.Start > inner[j].Range.Start })
		for _, occ := range inner {
			s := occ.Range.Start - stmtRange.Start
			e := occ.Range.End - stmtRange.Start
			declText = declText[:s] + info.newName + declText[e:]
		}
	}
	item.InsertText = ensureTrailingNewline(declText)

	// Classify references and collect shadowed same-name occurrences.
	for _, u := range w.snapshot().URIs() {
		st := w.files[u]
		if st == nil {
			continue
		}
		for _, occ := range st.analysis.Occurrences {
			if occ.Symbol.ID == info.sym.ID {
				if occ.Role == semantic.RoleDecl {
					continue
				}
				if u == from && occ.Range.Start >= stmtRange.Start && occ.Range.End <= stmtRange.End {
					continue // travels with the moved declaration text
				}
				ref := MoveReference{Location: w.toLocation(occ), Decision: MoveDecisionSwitch}
				switch {
				case info.newName == info.name:
					ref.Decision = MoveDecisionUnchanged
					ref.Reason = "exported name unchanged; reference stays valid after the move"
				case localNamesAt(st.tree, occ.Range.Start)[info.newName]:
					ref.Decision = MoveDecisionKeep
					ref.Forced = true
					ref.Reason = fmt.Sprintf("%q is shadowed by a local in this scope; switching would capture the reference", info.newName)
				}
				item.References = append(item.References, ref)
				continue
			}
			if occ.Symbol.Name == info.name || occ.Symbol.Name == info.newName {
				item.Shadowed = append(item.Shadowed, MoveShadowed{
					Location: w.toLocation(occ),
					Reason:   "same name but a different symbol (shadowing local or unrelated declaration); left untouched",
				})
			}
		}
	}
	sort.Slice(item.References, func(i, j int) bool {
		if item.References[i].Location.URI != item.References[j].Location.URI {
			return item.References[i].Location.URI < item.References[j].Location.URI
		}
		return item.References[i].Location.Range.Start < item.References[j].Location.Range.Start
	})
	sort.Slice(item.Shadowed, func(i, j int) bool {
		if item.Shadowed[i].Location.URI != item.Shadowed[j].Location.URI {
			return item.Shadowed[i].Location.URI < item.Shadowed[j].Location.URI
		}
		return item.Shadowed[i].Location.Range.Start < item.Shadowed[j].Location.Range.Start
	})

	// A reference that keeps the old name requires the compatibility entry.
	kept := 0
	for _, ref := range item.References {
		if ref.Decision == MoveDecisionKeep {
			kept++
		}
	}
	if kept > 0 && !item.KeepCompat && allForcedKeep(item.References) {
		item.KeepCompat = true
	}
	if kept > 0 && !item.KeepCompat {
		for _, ref := range item.References {
			if ref.Decision == MoveDecisionKeep {
				issues.Unresolved = append(issues.Unresolved, MoveIssue{
					URI: ref.Location.URI,
					Message: fmt.Sprintf("reference to %q at offset %d keeps the old name but no compatibility entry is kept in %s",
						info.name, ref.Location.Range.Start, from),
				})
			}
		}
	}
	if item.KeepCompat {
		item.Compat = &MoveCompat{URI: from, Text: compatText(info)}
	}
	return item
}

func allForcedKeep(refs []MoveReference) bool {
	kept := 0
	for _, r := range refs {
		if r.Decision == MoveDecisionKeep {
			if !r.Forced {
				return false
			}
			kept++
		}
	}
	return kept > 0
}

// compatText renders the forwarding entry left at the old location.
func compatText(info *moveItemInfo) string {
	switch n := info.stmt.(type) {
	case *syntax.FnStmt:
		params := make([]string, 0, len(n.Params))
		for _, p := range n.Params {
			params = append(params, p.Name)
		}
		return fmt.Sprintf("fn %s(%s) {\n  return %s(%s)\n}\n",
			info.name, strings.Join(params, ", "), info.newName, strings.Join(params, ", "))
	default:
		return fmt.Sprintf("let %s = %s\n", info.name, info.newName)
	}
}

func moveStmtRange(st syntax.Stmt) syntax.Range {
	switch n := st.(type) {
	case *syntax.FnStmt:
		return n.Range
	case *syntax.LetStmt:
		return n.Range
	}
	return syntax.Range{}
}

// removalSpan widens a statement range to swallow one adjacent newline so
// the deletion does not leave a blank line behind.
func removalSpan(text string, r syntax.Range) (int, int) {
	start, end := r.Start, r.End
	if end < len(text) && text[end] == '\n' {
		end++
	} else if start > 0 && text[start-1] == '\n' {
		start--
	}
	return start, end
}

func ensureTrailingNewline(s string) string {
	if !strings.HasSuffix(s, "\n") {
		return s + "\n"
	}
	return s
}

// localNamesAt returns the names lexically visible at offset that are locals
// (parameters and block-scope declarations), used to detect references that
// a rename of the exported name would capture.
func localNamesAt(f *syntax.File, offset int) map[string]bool {
	names := map[string]bool{}
	for _, st := range f.Stmts {
		if walkLocals(st, offset, names) {
			break
		}
	}
	return names
}

func walkLocals(st syntax.Stmt, offset int, names map[string]bool) bool {
	contains := func(r syntax.Range) bool { return r.Start <= offset && offset < r.End }
	switch n := st.(type) {
	case *syntax.FnStmt:
		if !contains(n.Range) {
			return false
		}
		for _, p := range n.Params {
			names[p.Name] = true
		}
		walkBlockLocals(n.Body, offset, names)
		return true
	case *syntax.BlockStmt:
		if !contains(n.Range) {
			return false
		}
		walkBlockLocals(n, offset, names)
		return true
	case *syntax.IfStmt:
		if !contains(n.Range) {
			return false
		}
		walkBlockLocals(n.Then, offset, names)
		if n.Else != nil {
			walkBlockLocals(n.Else, offset, names)
		}
		return true
	case *syntax.LetStmt:
		return contains(n.Range)
	case *syntax.AssignStmt:
		return contains(n.Range)
	case *syntax.ReturnStmt:
		return contains(n.Range)
	case *syntax.ExprStmt:
		return contains(n.Range)
	}
	return false
}

func walkBlockLocals(b *syntax.BlockStmt, offset int, names map[string]bool) {
	if b == nil {
		return
	}
	for _, st := range b.Stmts {
		switch d := st.(type) {
		case *syntax.LetStmt:
			names[d.Name] = true
		case *syntax.FnStmt:
			names[d.Name] = true
		}
	}
	for _, st := range b.Stmts {
		if walkLocals(st, offset, names) {
			break
		}
	}
}

// moveImportRelations computes the file-level dependency edges that would
// exist after the move (restricted to edges touching the moved modules) and
// the circular dependencies the move would newly introduce.
func (w *Workspace) moveImportRelations(target snapshot.URI, infos []*moveItemInfo, items []MoveItemPlan) ([]MoveImportEdge, []MoveIssue) {
	declBefore := map[string]snapshot.URI{}
	for _, u := range w.snapshot().URIs() {
		st := w.files[u]
		if st == nil {
			continue
		}
		for _, g := range st.analysis.Globals {
			if g.Role == semantic.RoleDecl {
				declBefore[g.Symbol.Name] = u
			}
		}
	}
	declAfter := make(map[string]snapshot.URI, len(declBefore))
	for n, u := range declBefore {
		declAfter[n] = u
	}
	for i, info := range infos {
		if declAfter[info.name] == info.spec.URI {
			delete(declAfter, info.name)
		}
		if items[i].KeepCompat {
			declAfter[info.name] = info.spec.URI
		}
		declAfter[info.newName] = target
	}

	type edge struct{ from, to snapshot.URI }
	build := func(declFile map[string]snapshot.URI, after bool) (map[edge]map[string]bool, map[snapshot.URI]map[snapshot.URI]bool) {
		names := map[edge]map[string]bool{}
		adj := map[snapshot.URI]map[snapshot.URI]bool{}
		add := func(a, b snapshot.URI, n string) {
			if names[edge{a, b}] == nil {
				names[edge{a, b}] = map[string]bool{}
			}
			names[edge{a, b}][n] = true
			if adj[a] == nil {
				adj[a] = map[snapshot.URI]bool{}
			}
			adj[a][b] = true
		}
		for _, u := range w.snapshot().URIs() {
			st := w.files[u]
			if st == nil {
				continue
			}
			for _, occ := range st.analysis.Occurrences {
				if occ.Role != semantic.RoleUse || occ.Symbol.LocalKey != "" {
					continue
				}
				owner, ok := declFile[occ.Symbol.Name]
				if !ok {
					continue
				}
				src := u
				if after {
					for _, info := range infos {
						sr := moveStmtRange(info.stmt)
						if u == info.spec.URI && occ.Range.Start >= sr.Start && occ.Range.End <= sr.End {
							src = target
						}
					}
				}
				if owner == src {
					continue
				}
				add(src, owner, occ.Symbol.Name)
			}
		}
		if after {
			for i, info := range infos {
				if items[i].KeepCompat && info.spec.URI != target {
					add(info.spec.URI, target, info.newName)
				}
			}
		}
		return names, adj
	}

	beforeNames, beforeAdj := build(declBefore, false)
	afterNames, afterAdj := build(declAfter, true)

	modules := map[snapshot.URI]bool{target: true}
	for _, info := range infos {
		modules[info.spec.URI] = true
	}
	var imports []MoveImportEdge
	for e, ns := range afterNames {
		if !modules[e.from] && !modules[e.to] {
			continue
		}
		var names []string
		for n := range ns {
			names = append(names, n)
		}
		sort.Strings(names)
		_, existed := beforeNames[e]
		imports = append(imports, MoveImportEdge{From: e.from, To: e.to, Names: names, New: !existed})
	}
	sort.Slice(imports, func(i, j int) bool {
		if imports[i].From != imports[j].From {
			return imports[i].From < imports[j].From
		}
		return imports[i].To < imports[j].To
	})

	beforeCycles := cyclicComponents(w.snapshot().URIs(), beforeAdj)
	afterCycles := cyclicComponents(w.snapshot().URIs(), afterAdj)
	var cycles []MoveIssue
	for key, members := range afterCycles {
		if _, ok := beforeCycles[key]; ok {
			continue
		}
		cycles = append(cycles, MoveIssue{
			Message: "new circular dependency introduced: " + strings.Join(members, " <-> "),
		})
	}
	sort.Slice(cycles, func(i, j int) bool { return cycles[i].Message < cycles[j].Message })
	return imports, cycles
}

// cyclicComponents returns the strongly connected components that form a
// cycle (more than one node, or a self-loop), keyed by their sorted members.
func cyclicComponents(nodes []snapshot.URI, adj map[snapshot.URI]map[snapshot.URI]bool) map[string][]string {
	index := map[snapshot.URI]int{}
	low := map[snapshot.URI]int{}
	onStack := map[snapshot.URI]bool{}
	var stack []snapshot.URI
	counter := 0
	out := map[string][]string{}

	var strongconnect func(v snapshot.URI)
	strongconnect = func(v snapshot.URI) {
		index[v] = counter
		low[v] = counter
		counter++
		stack = append(stack, v)
		onStack[v] = true
		for succ := range adj[v] {
			if _, seen := index[succ]; !seen {
				strongconnect(succ)
				if low[succ] < low[v] {
					low[v] = low[succ]
				}
			} else if onStack[succ] && index[succ] < low[v] {
				low[v] = index[succ]
			}
		}
		if low[v] == index[v] {
			var comp []snapshot.URI
			for {
				w2 := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w2] = false
				comp = append(comp, w2)
				if w2 == v {
					break
				}
			}
			selfLoop := len(comp) == 1 && adj[comp[0]][comp[0]]
			if len(comp) > 1 || selfLoop {
				members := make([]string, 0, len(comp))
				for _, u := range comp {
					members = append(members, string(u))
				}
				sort.Strings(members)
				out[strings.Join(members, "|")] = members
			}
		}
	}
	for _, v := range nodes {
		if _, seen := index[v]; !seen {
			strongconnect(v)
		}
	}
	return out
}

// moveExportChanges describes how each module's exported names change.
func moveExportChanges(target snapshot.URI, items []MoveItemPlan) []MoveExportChange {
	type agg struct{ added, removed map[string]bool }
	byURI := map[snapshot.URI]*agg{}
	get := func(u snapshot.URI) *agg {
		if byURI[u] == nil {
			byURI[u] = &agg{added: map[string]bool{}, removed: map[string]bool{}}
		}
		return byURI[u]
	}
	for _, item := range items {
		if !item.KeepCompat {
			get(item.From).removed[item.Name] = true
		}
		get(target).added[item.NewName] = true
	}
	var out []MoveExportChange
	for u, a := range byURI {
		ec := MoveExportChange{URI: u}
		for n := range a.added {
			if !a.removed[n] {
				ec.Added = append(ec.Added, n)
			}
		}
		for n := range a.removed {
			if !a.added[n] {
				ec.Removed = append(ec.Removed, n)
			}
		}
		if len(ec.Added) == 0 && len(ec.Removed) == 0 {
			continue
		}
		sort.Strings(ec.Added)
		sort.Strings(ec.Removed)
		out = append(out, ec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URI < out[j].URI })
	return out
}

// buildMoveFileEdits turns the confirmed per-reference decisions into
// concrete per-document edits. It is pure: PrepareMove and ApplyMove both
// use it, so the preview shows exactly what apply will write.
func buildMoveFileEdits(target snapshot.URI, docs map[snapshot.URI]snapshot.Document, items []MoveItemPlan) []MoveFileEdit {
	edits := map[snapshot.URI][]MoveEdit{}
	appends := map[snapshot.URI][]string{}
	add := func(u snapshot.URI, e MoveEdit) { edits[u] = append(edits[u], e) }

	for _, item := range items {
		add(item.From, item.Removal)
		appends[target] = append(appends[target], ensureTrailingNewline(item.InsertText))
		if item.KeepCompat && item.Compat != nil {
			appends[item.Compat.URI] = append(appends[item.Compat.URI], ensureTrailingNewline(item.Compat.Text))
		}
		for _, ref := range item.References {
			if ref.Decision != MoveDecisionSwitch || item.NewName == item.Name {
				continue
			}
			add(ref.Location.URI, MoveEdit{
				Range:   ref.Location.Range,
				OldText: ref.Location.Range.Text,
				NewText: item.NewName,
			})
		}
	}

	// Materialise end-of-file insertions as a single zero-width edit per
	// document, so apply-time offset validation stays against the original
	// document text.
	for u, parts := range appends {
		doc, ok := docs[u]
		if !ok {
			continue
		}
		cur := doc.Text
		var combined string
		for _, p := range parts {
			if len(cur) > 0 && !strings.HasSuffix(cur, "\n") {
				combined += "\n"
				cur += "\n"
			}
			combined += p
			cur += p
		}
		off := len(doc.Text)
		add(u, MoveEdit{
			Range:   Range{Range: syntax.Range{Start: off, End: off}},
			OldText: "",
			NewText: combined,
		})
	}

	var files []MoveFileEdit
	for u, es := range edits {
		doc, ok := docs[u]
		if !ok {
			continue
		}
		sort.Slice(es, func(i, j int) bool { return es[i].Range.Start < es[j].Range.Start })
		files = append(files, MoveFileEdit{URI: u, Version: doc.Version, Saved: doc.Saved, Edits: es})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].URI < files[j].URI })
	return files
}

// ApplyMove commits a previously prepared move plan. It first re-validates
// the plan against the current snapshot: every document the plan writes to
// must still be at the previewed version and every removed or replaced byte
// range must still hold the previewed text. Decisions already confirmed for
// references are honoured exactly; overriding a forced decision or dropping
// a required compatibility entry is rejected. Any mismatch fails with
// ErrStaleMovePlan (or a decision error) and changes nothing. On success all
// documents are rewritten in one snapshot, so a move can never leave some
// files moved and others not.
func (w *Workspace) ApplyMove(plan MovePlan) (MoveResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if plan.Issues.Blocking() {
		return MoveResult{}, fmt.Errorf("move plan has blocking issues (conflicts or unresolved references); narrow the scope and re-preview")
	}

	// Re-derive the effective compatibility requirements from the confirmed
	// decisions and validate them.
	items := make([]MoveItemPlan, len(plan.Items))
	copy(items, plan.Items)
	for i := range items {
		item := &items[i]
		kept := 0
		for _, ref := range item.References {
			switch ref.Decision {
			case MoveDecisionSwitch, MoveDecisionKeep, MoveDecisionUnchanged:
			default:
				return MoveResult{}, fmt.Errorf("unknown decision %q for reference at %s offset %d",
					ref.Decision, ref.Location.URI, ref.Location.Range.Start)
			}
			if ref.Decision == MoveDecisionSwitch && ref.Forced {
				return MoveResult{}, fmt.Errorf("reference to %q at %s offset %d cannot switch to %q: %s",
					item.Name, ref.Location.URI, ref.Location.Range.Start, item.NewName, ref.Reason)
			}
			if ref.Decision == MoveDecisionKeep {
				kept++
			}
		}
		if kept > 0 && !item.KeepCompat {
			return MoveResult{}, fmt.Errorf("%d reference(s) keep the old name %q but the compatibility entry in %s was dropped",
				kept, item.Name, item.From)
		}
		if item.KeepCompat {
			if item.Compat == nil {
				return MoveResult{}, fmt.Errorf("compatibility entry for %q is missing from the plan", item.Name)
			}
			if item.NewName == item.Name {
				return MoveResult{}, fmt.Errorf("compatibility entry for %q requires a new exported name", item.Name)
			}
		}
	}

	// Freshness: only documents the plan actually writes to are checked, so
	// unrelated confirmed edits elsewhere neither block nor are lost.
	expected := map[snapshot.URI]int{}
	expect := func(u snapshot.URI, v int) error {
		if prev, ok := expected[u]; ok && prev != v {
			return fmt.Errorf("%w: inconsistent versions for %s in plan", ErrStaleMovePlan, u)
		}
		expected[u] = v
		return nil
	}
	if err := expect(plan.Target, plan.TargetVersion); err != nil {
		return MoveResult{}, err
	}
	for i := range items {
		item := &items[i]
		if err := expect(item.From, item.Declaration.Version); err != nil {
			return MoveResult{}, err
		}
		if item.KeepCompat && item.Compat != nil {
			if err := expect(item.Compat.URI, item.Declaration.Version); err != nil {
				return MoveResult{}, err
			}
		}
		for _, ref := range item.References {
			if ref.Decision == MoveDecisionSwitch && item.NewName != item.Name {
				if err := expect(ref.Location.URI, ref.Location.Version); err != nil {
					return MoveResult{}, err
				}
			}
		}
	}
	docs := map[snapshot.URI]snapshot.Document{}
	for u, v := range expected {
		doc, ok := w.snapshot().Get(u)
		if !ok {
			return MoveResult{}, fmt.Errorf("%w: %s no longer open", ErrStaleMovePlan, u)
		}
		if doc.Version != v {
			return MoveResult{}, fmt.Errorf("%w: %s at version %d, plan expected %d",
				ErrStaleMovePlan, u, doc.Version, v)
		}
		docs[u] = doc
	}
	// Verify the bytes every removal and switched reference depends on.
	for i := range items {
		item := &items[i]
		fromDoc := docs[item.From]
		if err := verifyMoveEdit(fromDoc, item.Removal); err != nil {
			return MoveResult{}, err
		}
		for _, ref := range item.References {
			if ref.Decision != MoveDecisionSwitch || item.NewName == item.Name {
				continue
			}
			doc := docs[ref.Location.URI]
			start, end := ref.Location.Range.Start, ref.Location.Range.End
			if start < 0 || end < start || end > len(doc.Text) {
				return MoveResult{}, fmt.Errorf("%w: reference range [%d,%d) invalid for %s",
					ErrStaleMovePlan, start, end, ref.Location.URI)
			}
			if doc.Text[start:end] != ref.Location.Range.Text {
				return MoveResult{}, fmt.Errorf("%w: %s offset %d now holds %q, plan expected %q",
					ErrStaleMovePlan, ref.Location.URI, start, doc.Text[start:end], ref.Location.Range.Text)
			}
		}
	}

	// Recompute the edits from the confirmed decisions (identical to the
	// preview when decisions were left unchanged).
	fileEdits := buildMoveFileEdits(plan.Target, docs, items)

	// Apply every document's edits back-to-front, verifying covered bytes.
	type pendingDoc struct {
		uri  snapshot.URI
		text string
	}
	var pending []pendingDoc
	for _, fe := range fileEdits {
		doc := docs[fe.URI]
		text := doc.Text
		edits := append([]MoveEdit(nil), fe.Edits...)
		sort.Slice(edits, func(i, j int) bool { return edits[i].Range.Start > edits[j].Range.Start })
		lastStart := len(text) + 1
		for _, e := range edits {
			start, end := e.Range.Start, e.Range.End
			if start < 0 || end < start || end > len(text) || end > lastStart {
				return MoveResult{}, fmt.Errorf("%w: edit range [%d,%d) invalid for %s",
					ErrStaleMovePlan, start, end, fe.URI)
			}
			if text[start:end] != e.OldText {
				return MoveResult{}, fmt.Errorf("%w: %s offset %d now holds %q, plan expected %q",
					ErrStaleMovePlan, fe.URI, start, text[start:end], e.OldText)
			}
			text = text[:start] + e.NewText + text[end:]
			lastStart = start
		}
		pending = append(pending, pendingDoc{uri: fe.URI, text: text})
	}

	// Commit atomically: one new snapshot carrying every rewritten document.
	undo := &moveUndoState{
		token: w.nextMoveUndo,
		prior: map[snapshot.URI]snapshot.Document{},
		post:  map[snapshot.URI]int{},
	}
	w.nextMoveUndo++
	snap := w.snapshot()
	for _, pd := range pending {
		doc, _ := snap.Get(pd.uri)
		undo.prior[pd.uri] = doc
		doc.Text = pd.text
		doc.Version++
		doc.Saved = false
		snap = snap.WithDocument(w.nextSnap, doc)
		undo.post[pd.uri] = doc.Version
	}
	w.snap = snap
	w.nextSnap++
	for _, pd := range pending {
		w.rebuild(pd.uri)
	}
	w.moveUndo = undo

	res := MoveResult{Version: w.versionInfoLocked(), UndoToken: undo.token}
	for _, u := range w.snapshot().URIs() {
		doc, _ := w.snapshot().Get(u)
		res.Documents = append(res.Documents, DocumentInfo{
			URI: u, Version: doc.Version, Saved: doc.Saved, Bytes: len(doc.Text),
		})
	}
	return res, nil
}

func verifyMoveEdit(doc snapshot.Document, e MoveEdit) error {
	start, end := e.Range.Start, e.Range.End
	if start < 0 || end < start || end > len(doc.Text) {
		return fmt.Errorf("%w: edit range [%d,%d) invalid for %s", ErrStaleMovePlan, start, end, doc.URI)
	}
	if doc.Text[start:end] != e.OldText {
		return fmt.Errorf("%w: %s offset %d now holds %q, plan expected %q",
			ErrStaleMovePlan, doc.URI, start, doc.Text[start:end], e.OldText)
	}
	return nil
}

// UndoMove restores the exact pre-move document contents captured by the
// matching ApplyMove, recovering the original declaration ownership and
// reference meaning. It refuses (ErrStaleMovePlan) when any document the
// move touched changed since the move was applied; edits to unrelated
// documents never block undo and are preserved.
func (w *Workspace) UndoMove(token int64) (MoveResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	undo := w.moveUndo
	if undo == nil || undo.token != token {
		return MoveResult{}, fmt.Errorf("no applied move matches undo token %d", token)
	}
	for u, postVersion := range undo.post {
		doc, ok := w.snapshot().Get(u)
		if !ok {
			return MoveResult{}, fmt.Errorf("%w: %s no longer open", ErrStaleMovePlan, u)
		}
		if doc.Version != postVersion {
			return MoveResult{}, fmt.Errorf("%w: %s changed after the move (version %d, expected %d)",
				ErrStaleMovePlan, u, doc.Version, postVersion)
		}
	}

	snap := w.snapshot()
	for u, prior := range undo.prior {
		doc, _ := snap.Get(u)
		doc.Text = prior.Text
		doc.Saved = prior.Saved
		doc.Version++
		snap = snap.WithDocument(w.nextSnap, doc)
	}
	w.snap = snap
	w.nextSnap++
	for u := range undo.prior {
		w.rebuild(u)
	}
	w.moveUndo = nil

	res := MoveResult{Version: w.versionInfoLocked(), UndoToken: token}
	for _, u := range w.snapshot().URIs() {
		doc, _ := w.snapshot().Get(u)
		res.Documents = append(res.Documents, DocumentInfo{
			URI: u, Version: doc.Version, Saved: doc.Saved, Bytes: len(doc.Text),
		})
	}
	return res, nil
}
