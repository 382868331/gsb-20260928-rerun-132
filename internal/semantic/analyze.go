// Package semantic builds symbol identity and lexical resolution on top of the
// immutable syntax objects. Symbols are deliberately separate from syntax
// nodes: a symbol survives re-parses and edits, while ranges belong to a
// specific document version.
package semantic

import (
	"fmt"
	"strconv"

	"sourcenav/internal/snapshot"
	"sourcenav/internal/syntax"
)

type Role int

const (
	RoleUse Role = iota
	RoleDecl
)

// Occurrence is one identity-tagged identifier range inside one document
// version. Range bytes point at the real source text.
type Occurrence struct {
	URI    snapshot.URI `json:"uri"`
	Range  syntax.Range `json:"range"`
	Symbol *Symbol      `json:"-"`
	Role   Role         `json:"role"`
	Kind   Kind         `json:"kind"`

	// Qual, when non-nil, marks a qualified reference (mod.name): Range covers
	// only the selected name and Qual covers the module qualifier, so rewrites
	// can retarget the qualifier independently of the name.
	Qual *syntax.Range `json:"-"`
}

// Diagnostic describes a semantic problem at a real source range.
type Diagnostic struct {
	URI     snapshot.URI `json:"uri"`
	Range   syntax.Range `json:"range"`
	Message string       `json:"message"`
}

// FileAnalysis is everything the semantic layer derives from one document
// version without inspecting other files' internals.
type FileAnalysis struct {
	URI         snapshot.URI
	Version     int
	File        *syntax.File
	Occurrences []Occurrence
	Globals     []Occurrence // declaration occurrences hoisted to project scope
	Diagnostics []Diagnostic
}

// GlobalDecl returns the first declaration occurrence of the named global.
func (a *FileAnalysis) GlobalDecl(name string) (Occurrence, bool) {
	for _, o := range a.Globals {
		if o.Role == RoleDecl && o.Symbol.Name == name {
			return o, true
		}
	}
	return Occurrence{}, false
}

// resolver is anything an identifier occurrence can be looked up against.
// Lexical scopes and the project-global fallback both implement it.
type resolver interface {
	lookup(name string) (*Symbol, bool)
}

// scope is one lexical scope. path is the session-stable lexical nesting path
// used to mint local identity keys.
type scope struct {
	parent resolver
	path   string
	decls  map[string]*Symbol
}

func (s *scope) lookup(name string) (*Symbol, bool) {
	if sym, ok := s.decls[name]; ok {
		return sym, true
	}
	if s.parent != nil {
		return s.parent.lookup(name)
	}
	return nil, false
}

// globalScope resolves only the file's own module-global names. It roots
// every file's resolution chain, which makes cross-file mutual references
// work while guaranteeing file/function-local names can never leak between
// files. Cross-module names are reached only through explicit imports.
type globalScope struct {
	reg *Registry
	mod string
}

func (g globalScope) lookup(name string) (*Symbol, bool) {
	return g.reg.GlobalExisting(g.mod, name)
}

type analyzer struct {
	reg     *Registry
	root    resolver
	uri     snapshot.URI
	mod     string
	imports map[string]string // qualifier -> module path
	file    *syntax.File
	result  *FileAnalysis
}

// AnalyzeFile performs lexical resolution for one document. The registry is
// pre-populated with every project-wide global name (collected from all
// files first), which is what makes mutual references resolve in any order.
func AnalyzeFile(reg *Registry, doc snapshot.Document, tree *syntax.File) (*FileAnalysis, error) {
	a := &analyzer{
		reg:     reg,
		uri:     doc.URI,
		imports: make(map[string]string),
		file:    tree,
		result:  &FileAnalysis{URI: doc.URI, Version: doc.Version, File: tree},
	}
	a.scanDirectives()
	a.root = globalScope{reg: reg, mod: a.mod}
	fileScope := &scope{parent: a.root, path: "", decls: make(map[string]*Symbol)}
	a.hoistTopLevels(fileScope)
	for _, st := range tree.Stmts {
		a.resolveStmt(fileScope, st)
	}
	return a.result, nil
}

// scanDirectives validates the optional module directive and collects import
// qualifiers up front, so exports and qualified references anywhere in the
// file can resolve against them regardless of order.
func (a *analyzer) scanDirectives() {
	seenModule := false
	for i, st := range a.file.Stmts {
		switch n := st.(type) {
		case *syntax.ModuleStmt:
			if seenModule {
				a.result.Diagnostics = append(a.result.Diagnostics, Diagnostic{
					URI: a.uri, Range: n.Range, Message: "duplicate module directive",
				})
				continue
			}
			if i != 0 {
				a.result.Diagnostics = append(a.result.Diagnostics, Diagnostic{
					URI: a.uri, Range: n.Range, Message: "module directive must be the first statement",
				})
			}
			seenModule = true
			a.mod = n.Path
		case *syntax.ImportStmt:
			qual := pathBase(n.Path)
			if n.HasAlias {
				qual = n.Alias
			}
			if _, dup := a.imports[qual]; dup {
				a.result.Diagnostics = append(a.result.Diagnostics, Diagnostic{
					URI: a.uri, Range: n.Range, Message: fmt.Sprintf("duplicate import qualifier %q", qual),
				})
				continue
			}
			a.imports[qual] = n.Path
		}
	}
}

func pathBase(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '.' {
			return path[i+1:]
		}
	}
	return path
}

func (a *analyzer) hoistTopLevels(fileScope *scope) {
	for _, st := range a.file.Stmts {
		var (
			name string
			r    syntax.Range
			kind Kind
		)
		switch s := st.(type) {
		case *syntax.FnStmt:
			name, r, kind = s.Name, s.NameRange, KindFunc
		case *syntax.LetStmt:
			name, r, kind = s.Name, s.NameRange, KindVar
		default:
			continue
		}
		if _, dup := fileScope.decls[name]; dup {
			a.result.Diagnostics = append(a.result.Diagnostics, Diagnostic{
				URI: a.uri, Range: r, Message: fmt.Sprintf("redeclared global name %q", name),
			})
			continue
		}
		sym := a.reg.Global(a.mod, name, kind)
		fileScope.decls[name] = sym
		occ := Occurrence{URI: a.uri, Range: r, Symbol: sym, Role: RoleDecl, Kind: kind}
		a.result.Occurrences = append(a.result.Occurrences, occ)
		a.result.Globals = append(a.result.Globals, occ)
	}
}

func (a *analyzer) child(parent resolver, ordinal int) *scope {
	path := strconv.Itoa(ordinal)
	return &scope{parent: parent, path: path, decls: make(map[string]*Symbol)}
}

// declareLocal mints an identity keyed by file + lexical path + name, so
// shadowed same-named variables always get distinct symbols.
func (a *analyzer) declareLocal(s *scope, name string, kind Kind, r syntax.Range) *Symbol {
	if _, dup := s.decls[name]; dup {
		a.result.Diagnostics = append(a.result.Diagnostics, Diagnostic{
			URI: a.uri, Range: r, Message: fmt.Sprintf("redeclared local name %q", name),
		})
		return s.decls[name]
	}
	key := LocalKey(string(a.uri), s.path, name+":"+strconv.Itoa(r.Start))
	sym := a.reg.Local(key, name, kind)
	s.decls[name] = sym
	a.result.Occurrences = append(a.result.Occurrences, Occurrence{
		URI: a.uri, Range: r, Symbol: sym, Role: RoleDecl, Kind: kind,
	})
	return sym
}

func (a *analyzer) resolveStmt(s *scope, st syntax.Stmt) {
	switch n := st.(type) {
	case *syntax.LetStmt:
		if s.path == "" {
			// File-level declaration was hoisted; resolve initializer only.
			a.resolveExpr(s, n.Value)
			return
		}
		a.resolveExpr(s, n.Value)
		a.declareLocal(s, n.Name, KindVar, n.NameRange)
	case *syntax.AssignStmt:
		a.resolveExpr(s, n.Value)
		a.use(s, n.Name, n.NameRange)
	case *syntax.FnStmt:
		a.resolveFunction(s, n)
	case *syntax.ReturnStmt:
		if n.Value != nil {
			a.resolveExpr(s, n.Value)
		}
	case *syntax.ExprStmt:
		a.resolveExpr(s, n.X)
	case *syntax.ModuleStmt, *syntax.ImportStmt:
		// Directives were handled by scanDirectives.
	case *syntax.ExportStmt:
		a.resolveExport(s, n)
	case *syntax.BlockStmt:
		nested := a.child(s, n.Range.Start)
		for _, inner := range n.Stmts {
			a.resolveStmt(nested, inner)
		}
	case *syntax.IfStmt:
		a.resolveExpr(s, n.Cond)
		then := a.child(s, n.Then.Range.Start)
		for _, inner := range n.Then.Stmts {
			a.resolveStmt(then, inner)
		}
		if n.Else != nil {
			els := a.child(s, n.Else.Range.Start)
			for _, inner := range n.Else.Stmts {
				a.resolveStmt(els, inner)
			}
		}
	}
}

func (a *analyzer) resolveFunction(enclosing resolver, fn *syntax.FnStmt) {
	if s, isLexical := enclosing.(*scope); isLexical && s.path == "" {
		// Top-level function: its declaration occurrence was hoisted when the
		// file scope was populated, so only parameters and the body are new.
	} else if s, isLexical := enclosing.(*scope); isLexical {
		a.declareLocal(s, fn.Name, KindFunc, fn.NameRange)
	}
	paramScope := &scope{
		parent: enclosing,
		path:   "f@" + strconv.Itoa(fn.NameRange.Start) + ":params",
		decls:  make(map[string]*Symbol),
	}
	for _, p := range fn.Params {
		if _, dup := paramScope.decls[p.Name]; dup {
			a.result.Diagnostics = append(a.result.Diagnostics, Diagnostic{
				URI: a.uri, Range: p.NameRange, Message: fmt.Sprintf("duplicate parameter %q", p.Name),
			})
			continue
		}
		key := LocalKey(string(a.uri), paramScope.path, p.Name+":"+strconv.Itoa(p.NameRange.Start))
		sym := a.reg.Local(key, p.Name, KindVar)
		paramScope.decls[p.Name] = sym
		a.result.Occurrences = append(a.result.Occurrences, Occurrence{
			URI: a.uri, Range: p.NameRange, Symbol: sym, Role: RoleDecl, Kind: KindVar,
		})
	}
	body := &scope{parent: paramScope, path: "f@" + strconv.Itoa(fn.NameRange.Start), decls: make(map[string]*Symbol)}
	for _, inner := range fn.Body.Stmts {
		a.resolveStmt(body, inner)
	}
}

func (a *analyzer) use(s resolver, name string, r syntax.Range) {
	sym, ok := s.lookup(name)
	if !ok {
		a.result.Diagnostics = append(a.result.Diagnostics, Diagnostic{
			URI: a.uri, Range: r, Message: fmt.Sprintf("undeclared name %q", name),
		})
		sym = a.reg.Global(a.mod, name, KindUnknown)
	}
	a.result.Occurrences = append(a.result.Occurrences, Occurrence{
		URI: a.uri, Range: r, Symbol: sym, Role: RoleUse, Kind: sym.Kind,
	})
}

// resolveExport binds a re-export: the local name must denote a declaration
// visible from this file (its own top level or a name exported by an imported
// module). The external name is registered as an alias of the same symbol
// identity, so consumers of the alias bind to the original declaration.
func (a *analyzer) resolveExport(s *scope, n *syntax.ExportStmt) {
	sym, ok := s.lookup(n.Local)
	if !ok {
		for _, path := range a.imports {
			if g, found := a.reg.GlobalExisting(path, n.Local); found {
				sym, ok = g, true
				break
			}
		}
	}
	if !ok {
		a.result.Diagnostics = append(a.result.Diagnostics, Diagnostic{
			URI: a.uri, Range: n.LocalRange,
			Message: fmt.Sprintf("export of undeclared name %q", n.Local),
		})
		return
	}
	external := n.Local
	if n.HasExternal {
		external = n.External
	}
	a.reg.Alias(GlobalKey(a.mod, external), sym)
	a.result.Occurrences = append(a.result.Occurrences, Occurrence{
		URI: a.uri, Range: n.LocalRange, Symbol: sym, Role: RoleUse, Kind: sym.Kind,
	})
}

// resolveSelector resolves mod.name: the qualifier must be an imported
// module, and the selected name must be exported by that module.
func (a *analyzer) resolveSelector(n *syntax.SelectorExpr) {
	path, ok := a.imports[n.Qual]
	if !ok {
		a.result.Diagnostics = append(a.result.Diagnostics, Diagnostic{
			URI: a.uri, Range: n.QualRange,
			Message: fmt.Sprintf("unknown module qualifier %q", n.Qual),
		})
		return
	}
	sym, ok := a.reg.GlobalExisting(path, n.Sel)
	if !ok {
		a.result.Diagnostics = append(a.result.Diagnostics, Diagnostic{
			URI: a.uri, Range: n.SelRange,
			Message: fmt.Sprintf("module %q has no exported name %q", path, n.Sel),
		})
		sym = a.reg.Global(path, n.Sel, KindUnknown)
	}
	qual := n.QualRange
	a.result.Occurrences = append(a.result.Occurrences, Occurrence{
		URI: a.uri, Range: n.SelRange, Symbol: sym, Role: RoleUse, Kind: sym.Kind,
		Qual: &qual,
	})
}

func (a *analyzer) resolveExpr(s resolver, e syntax.Expr) {
	switch n := e.(type) {
	case *syntax.Ident:
		a.use(s, n.Name, n.Range)
	case *syntax.BasicLit:
		// literal: no names
	case *syntax.BinaryExpr:
		a.resolveExpr(s, n.X)
		a.resolveExpr(s, n.Y)
	case *syntax.SelectorExpr:
		a.resolveSelector(n)
	case *syntax.CallExpr:
		a.resolveExpr(s, n.Fun)
		for _, arg := range n.Args {
			a.resolveExpr(s, arg)
		}
	}
}
