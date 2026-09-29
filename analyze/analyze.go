// Package analyze 对单文件做嵌套作用域绑定。跨文件全局表由 index 层注入，
// 本包产出声明、已决局部引用与待解析的全局名字。
package analyze

import (
	"fmt"

	"nav/symbol"
	"nav/syntax"
)

type Diagnostic struct {
	URI   string `json:"uri"`
	Start int    `json:"start"`
	End   int    `json:"end"`
	Msg   string `json:"msg"`
}

// GlobalDecl 是文件顶层声明，用于构建跨文件全局表。
type GlobalDecl struct {
	Name  string
	Kind  symbol.Kind
	Start int
	End   int
}

// FileResult 只依赖单文件内容，可在其他文件变化时缓存复用。
type FileResult struct {
	URI        string
	Globals    []GlobalDecl
	Symbols    []symbol.Symbol
	Refs       []symbol.Ref // 局部引用已绑定 Def；全局引用仅填 GlobalName
	GlobalUses map[string]struct{}
	Diags      []Diagnostic
}

// scope 表示一层词法作用域。localID 不含文件，文件在产出时补齐。
type scope struct {
	parent    *scope
	container string
	isGlobal  bool
	ifCount   int
	vars      map[string]int // name -> 同名声明数（用于槽位）
}

func (sc *scope) lookup(name string) (symbol.ID, bool) {
	for cur := sc; cur != nil; cur = cur.parent {
		if n, ok := cur.vars[name]; ok {
			if cur.isGlobal {
				return symbol.GlobalID(name), true
			}
			return symbol.LocalID("", cur.container, name, symbol.LocalVar, n-1), true
		}
	}
	return symbol.ID{}, false
}

// Analyze 解析并绑定单个文件。
func Analyze(uri, src string) (*FileResult, error) {
	prog, _, err := syntax.Parse(src)
	a := &analyzer{
		uri: uri, src: src,
		res:     &FileResult{URI: uri, GlobalUses: map[string]struct{}{}},
		globals: map[string]bool{},
	}
	if prog != nil {
		a.globalSc = &scope{container: "", isGlobal: true, vars: map[string]int{}}
		// 顶层声明先注册，支持同文件与跨文件前向/相互引用。
		for _, st := range prog.Stmts {
			switch d := st.(type) {
			case *syntax.FuncDecl:
				a.declareGlobal(d.Name, symbol.Func, d.NameR)
			case *syntax.VarDecl:
				a.declareGlobal(d.Name, symbol.GlobalVar, d.NameR)
			}
		}
		for _, st := range prog.Stmts {
			switch d := st.(type) {
			case *syntax.VarDecl:
				if d.Value != nil {
					a.expr(d.Value, a.globalSc)
				}
			case *syntax.FuncDecl:
				a.funcDecl(d)
			case *syntax.ExprStmt:
				a.expr(d.Expr, a.globalSc)
			}
		}
	}
	return a.res, err
}

type analyzer struct {
	uri      string
	src      string
	res      *FileResult
	globals  map[string]bool
	globalSc *scope
}

func (a *analyzer) diag(r syntax.Range, format string, args ...any) {
	a.res.Diags = append(a.res.Diags, Diagnostic{
		URI: a.uri, Start: r.Start, End: r.End, Msg: fmt.Sprintf(format, args...),
	})
}

func (a *analyzer) declareGlobal(name string, kind symbol.Kind, r syntax.Range) {
	if a.globals[name] {
		a.diag(r, "duplicate global declaration %q", name)
		return
	}
	a.globals[name] = true
	a.globalSc.vars[name] = 1
	a.res.Globals = append(a.res.Globals, GlobalDecl{Name: name, Kind: kind, Start: r.Start, End: r.End})
	a.res.Symbols = append(a.res.Symbols, symbol.Symbol{
		ID: symbol.GlobalID(name), Kind: kind, Name: name, File: a.uri,
		DefStart: r.Start, DefEnd: r.End,
	})
}

func (a *analyzer) funcDecl(d *syntax.FuncDecl) {
	// 参数与函数体共享函数作用域：函数体内顶层 let 遮蔽参数。
	sc := &scope{parent: a.globalSc, container: d.Name, vars: map[string]int{}}
	for _, p := range d.Params {
		slot := sc.vars[p.Name]
		sc.vars[p.Name] = slot + 1
		a.res.Symbols = append(a.res.Symbols, symbol.Symbol{
			ID:   symbol.LocalID(a.uri, d.Name, p.Name, symbol.Param, slot),
			Kind: symbol.Param, Name: p.Name, File: a.uri,
			DefStart: p.NameR.Start, DefEnd: p.NameR.End, Container: d.Name,
		})
	}
	if d.Body != nil {
		a.blockStmts(d.Body, sc)
	}
}

// blockStmts 在已有作用域内执行语句。
func (a *analyzer) blockStmts(b *syntax.Block, sc *scope) {
	for _, st := range b.Stmts {
		switch s := st.(type) {
		case *syntax.VarDecl:
			a.localVar(s, sc)
		case *syntax.IfStmt:
			a.ifStmt(s, sc)
		case *syntax.ExprStmt:
			a.expr(s.Expr, sc)
		}
	}
}

// enterBlock 为嵌套块（if/else）建立新作用域，容器路径含稳定序号。
func (a *analyzer) enterBlock(parent *scope, label string) *scope {
	idx := parent.ifCount
	parent.ifCount++
	path := label
	if parent.container != "" {
		path = parent.container + "/" + label
	}
	return &scope{parent: parent, container: fmt.Sprintf("%s#%d", path, idx), vars: map[string]int{}}
}

func (a *analyzer) localVar(d *syntax.VarDecl, sc *scope) {
	if !sc.isGlobal {
		slot := sc.vars[d.Name]
		sc.vars[d.Name] = slot + 1
		a.res.Symbols = append(a.res.Symbols, symbol.Symbol{
			ID:   symbol.LocalID(a.uri, sc.container, d.Name, symbol.LocalVar, slot),
			Kind: symbol.LocalVar, Name: d.Name, File: a.uri,
			DefStart: d.NameR.Start, DefEnd: d.NameR.End, Container: sc.container,
		})
	}
	if d.Value != nil {
		a.expr(d.Value, sc)
	}
}

func (a *analyzer) ifStmt(s *syntax.IfStmt, sc *scope) {
	if s.Cond != nil {
		a.expr(s.Cond, sc)
	}
	if s.Then != nil {
		then := a.enterBlock(sc, "if")
		a.blockStmts(s.Then, then)
	}
	if s.Else != nil {
		els := a.enterBlock(sc, "else")
		a.blockStmts(s.Else, els)
	}
}

func (a *analyzer) expr(e syntax.Expr, sc *scope) {
	switch x := e.(type) {
	case *syntax.IdentExpr:
		if id, ok := sc.lookup(x.Name); ok {
			if !id.Global {
				id.File = a.uri
			}
			a.addRef(x.R, id, "", true)
		} else {
			a.res.GlobalUses[x.Name] = struct{}{}
			a.addRef(x.R, symbol.ID{}, x.Name, false)
		}
	case *syntax.BinaryExpr:
		a.expr(x.LHS, sc)
		a.expr(x.RHS, sc)
	case *syntax.CallExpr:
		a.expr(x.Callee, sc)
		for _, arg := range x.Args {
			a.expr(arg, sc)
		}
	case *syntax.IntExpr:
	}
}

func (a *analyzer) addRef(r syntax.Range, id symbol.ID, globalName string, resolved bool) {
	txt := ""
	if tr, err := r.Text(a.src); err == nil {
		txt = tr
	}
	a.res.Refs = append(a.res.Refs, symbol.Ref{
		File: a.uri, Start: r.Start, End: r.End, Text: txt,
		Def: id, GlobalName: globalName, Resolved: resolved,
	})
}
