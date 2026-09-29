// Package syntax 是 EduLang 的词法与语法对象。
// 所有节点区间都是源文字节偏移 [Start,End)，可从真实源码取回文本。
package syntax

import "fmt"

type TokenKind int

const (
	TEOF TokenKind = iota
	TIdent
	TInt
	TLet
	TFn
	TIf
	TElse
	TLParen
	TRParen
	TLBrace
	TRBrace
	TComma
	TEq
	TPlus
	TMinus
	TStar
	TSlash
	TGT
	TSemicolon
)

type Token struct {
	Kind  TokenKind
	Text  string
	Start int
	End   int
}

type Range struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// Text 从真实源文字段取回该区间覆盖的文本。
func (r Range) Text(src string) (string, error) {
	if r.Start < 0 || r.End > len(src) || r.Start > r.End {
		return "", fmt.Errorf("range %d:%d out of source bounds (len %d)", r.Start, r.End, len(src))
	}
	return src[r.Start:r.End], nil
}

func (r Range) Valid() bool { return r.Start >= 0 && r.End >= r.Start }

type Node interface{ Pos() Range }

// ---- 表达式 ----

type Expr interface {
	Node
	exprNode()
}

type IdentExpr struct {
	Name string
	R    Range
}

func (e *IdentExpr) exprNode()  {}
func (e *IdentExpr) Pos() Range { return e.R }

type IntExpr struct {
	Value string
	R     Range
}

func (e *IntExpr) exprNode()  {}
func (e *IntExpr) Pos() Range { return e.R }

type BinaryExpr struct {
	Op       Token
	LHS, RHS Expr
}

func (e *BinaryExpr) exprNode()  {}
func (e *BinaryExpr) Pos() Range { return Range{Start: e.LHS.Pos().Start, End: e.RHS.Pos().End} }

type CallExpr struct {
	Callee Expr
	Args   []Expr
	R      Range // 从被调用名到右括号
}

func (e *CallExpr) exprNode()  {}
func (e *CallExpr) Pos() Range { return e.R }

// ---- 声明 / 语句 ----

type VarDecl struct {
	Name  string
	NameR Range
	LetR  Range
	Value Expr
	R     Range
}

func (d *VarDecl) statementNode() {}
func (d *VarDecl) Pos() Range     { return d.R }

type FuncDecl struct {
	Name   string
	NameR  Range
	Params []Param
	Body   *Block
	R      Range
}

type Param struct {
	Name  string
	NameR Range
}

func (d *FuncDecl) statementNode() {}
func (d *FuncDecl) Pos() Range     { return d.R }

type Block struct {
	LBraceR Range
	Stmts   []Stmt
	R       Range
}

func (b *Block) statementNode() {}
func (b *Block) Pos() Range     { return b.R }

type IfStmt struct {
	Cond Expr
	Then *Block
	Else *Block // 可为 nil
	R    Range
}

func (s *IfStmt) statementNode() {}
func (s *IfStmt) Pos() Range     { return s.R }

type ExprStmt struct{ Expr Expr }

func (s *ExprStmt) statementNode() {}
func (s *ExprStmt) Pos() Range     { return s.Expr.Pos() }

type Stmt interface {
	Node
	statementNode()
}

type Program struct {
	Stmts []Stmt
}

func (p *Program) Pos() Range {
	if len(p.Stmts) == 0 {
		return Range{}
	}
	return Range{Start: p.Stmts[0].Pos().Start, End: p.Stmts[len(p.Stmts)-1].Pos().End}
}

// ParseError 标注真实源码位置。
type ParseError struct {
	Msg string
	R   Range
}

func (e *ParseError) Error() string { return fmt.Sprintf("%d: %s", e.R.Start, e.Msg) }
