// Package syntax parses the Edulang teaching language into a concrete syntax
// tree. Every node carries the Range it occupied in the real source text, so
// later symbol/reference ranges can always be traced back to exact bytes.
package syntax

// Range is a half-open byte interval [Start,End) in one document, with 1-based
// line and 1-based UTF-8 column information for editor display.
type Range struct {
	Start       int `json:"start"`
	End         int `json:"end"`
	StartLine   int `json:"startLine"`
	StartColumn int `json:"startColumn"`
	EndLine     int `json:"endLine"`
	EndColumn   int `json:"endColumn"`
}

// Contains reports whether offset falls inside the range.
func (r Range) Contains(offset int) bool { return r.Start <= offset && offset < r.End }

// Node is implemented by every syntax tree node.
type Node interface{ nodeRange() Range }

// File is the root node of one document.
type File struct {
	Range Range
	Stmts []Stmt
}

func (f *File) nodeRange() Range { return f.Range }

// Stmt is a statement node.
type Stmt interface {
	Node
	stmtNode()
}

// Expr is an expression node.
type Expr interface {
	Node
	exprNode()
}

// LetStmt: let name = expr
type LetStmt struct {
	Range     Range
	Name      string
	NameRange Range
	Value     Expr
}

// FnStmt: fn name(params) { body }
type FnStmt struct {
	Range     Range
	Name      string
	NameRange Range
	Params    []Param
	Body      *BlockStmt
}

// Param is one function parameter declaration.
type Param struct {
	Name      string
	NameRange Range
}

// BlockStmt: { stmts... }
type BlockStmt struct {
	Range Range
	Stmts []Stmt
}

// ReturnStmt: return expr?
type ReturnStmt struct {
	Range Range
	Value Expr
}

// AssignStmt: name = expr
type AssignStmt struct {
	Range     Range
	Name      string
	NameRange Range
	Value     Expr
}

// ExprStmt wraps a bare expression used as a statement.
type ExprStmt struct {
	Range Range
	X     Expr
}

// Ident is an identifier occurrence in expression position.
type Ident struct {
	Range Range
	Name  string
}

// BasicLit is an integer literal.
type BasicLit struct {
	Range Range
	Value string
}

// SelectorExpr: mod.name — a qualified name whose qualifier must resolve to
// an imported module (never a local variable).
type SelectorExpr struct {
	Range     Range
	Qual      string
	QualRange Range
	Sel       string
	SelRange  Range
}

// CallExpr: callee(args...); callee is a bare or qualified name.
type CallExpr struct {
	Range Range
	Fun   Expr
	Args  []Expr
}

// BinaryExpr: left op right
type BinaryExpr struct {
	Range Range
	X     Expr
	Op    string
	Y     Expr
}

// IfStmt: if cond { then } else { else }
type IfStmt struct {
	Range Range
	Cond  Expr
	Then  *BlockStmt
	Else  *BlockStmt
}

// ModuleStmt: module "path"
// It declares the module this document belongs to. At most one may appear,
// and only as the first non-comment directive.
type ModuleStmt struct {
	Range     Range
	Path      string
	PathRange Range
}

// ImportStmt: import "path" [as local]
// Local is empty when the module is bound under its final path component.
type ImportStmt struct {
	Range      Range
	Path       string
	PathRange  Range
	HasAlias   bool
	Alias      string
	AliasRange Range
}

// ExportStmt: export [as external] local
// External is empty when the re-export keeps the local name. Re-exports keep
// the same symbol identity: consumers bind to the original declaration, never
// to a copy.
type ExportStmt struct {
	Range       Range
	Local       string
	LocalRange  Range
	HasExternal bool
	External    string
	ExtRange    Range
}

func (s *LetStmt) stmtNode()           {}
func (s *FnStmt) stmtNode()            {}
func (s *BlockStmt) stmtNode()         {}
func (s *ReturnStmt) stmtNode()        {}
func (s *AssignStmt) stmtNode()        {}
func (s *ExprStmt) stmtNode()          {}
func (s *IfStmt) stmtNode()            {}
func (s *ModuleStmt) stmtNode()        {}
func (s *ImportStmt) stmtNode()        {}
func (s *ExportStmt) stmtNode()        {}
func (s *LetStmt) nodeRange() Range    { return s.Range }
func (s *FnStmt) nodeRange() Range     { return s.Range }
func (s *BlockStmt) nodeRange() Range  { return s.Range }
func (s *ReturnStmt) nodeRange() Range { return s.Range }
func (s *AssignStmt) nodeRange() Range { return s.Range }
func (s *ExprStmt) nodeRange() Range   { return s.Range }
func (s *IfStmt) nodeRange() Range     { return s.Range }
func (s *ModuleStmt) nodeRange() Range { return s.Range }
func (s *ImportStmt) nodeRange() Range { return s.Range }
func (s *ExportStmt) nodeRange() Range { return s.Range }

func (e *Ident) exprNode()        {}
func (e *BasicLit) exprNode()     {}
func (e *CallExpr) exprNode()     {}
func (e *BinaryExpr) exprNode()   {}
func (e *SelectorExpr) exprNode() {}
func (e *Ident) nodeRange() Range      { return e.Range }
func (e *BasicLit) nodeRange() Range   { return e.Range }
func (e *CallExpr) nodeRange() Range   { return e.Range }
func (e *BinaryExpr) nodeRange() Range { return e.Range }
func (e *SelectorExpr) nodeRange() Range { return e.Range }
