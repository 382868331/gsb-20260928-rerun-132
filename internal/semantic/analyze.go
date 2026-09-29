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

// globalScope resolves only project-global names. It roots every file's
// resolution chain, which makes cross-file mutual references work while
// guaranteeing file/function-local names can never leak between files.
type globalScope struct{ reg *Registry }

func (g globalScope) lookup(name string) (*Symbol, bool) { return g.reg.GlobalExisting(name) }

type analyzer struct {
	reg    *Registry
	root   resolver
	uri    snapshot.URI
	file   *syntax.File
	result *FileAnalysis
}

// AnalyzeFile performs lexical resolution for one document. The registry is
// pre-populated with every project-wide global name (collected from all
// files first), which is what makes mutual references resolve in any order.
func AnalyzeFile(reg *Registry, doc snapshot.Document, tree *syntax.File) (*FileAnalysis, error) {
	a := &analyzer{
		reg:    reg,
		root:   globalScope{reg: reg},
		uri:    doc.URI,
		file:   tree,
		result: &FileAnalysis{URI: doc.URI, Version: doc.Version, File: tree},
	}
	fileScope := &scope{parent: a.root, path: "", decls: make(map[string]*Symbol)}
	a.hoistTopLevels(fileScope)
	for _, st := range tree.Stmts {
		a.resolveStmt(fileScope, st)
	}
	return a.result, nil
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
		sym := a.reg.Global(name, kind)
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
		sym = a.reg.Global(name, KindUnknown)
	}
	a.result.Occurrences = append(a.result.Occurrences, Occurrence{
		URI: a.uri, Range: r, Symbol: sym, Role: RoleUse, Kind: sym.Kind,
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
	case *syntax.CallExpr:
		a.use(s, n.Fun.Name, n.Fun.Range)
		for _, arg := range n.Args {
			a.resolveExpr(s, arg)
		}
	}
}
