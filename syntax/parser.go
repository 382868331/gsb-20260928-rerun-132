package syntax

import "fmt"

// Parse 把源码解析为语法树。错误不阻止返回部分 AST。
func Parse(src string) (*Program, []Token, error) {
	toks, err := Lex(src)
	if err != nil {
		return &Program{}, toks, err
	}
	p := &parser{toks: toks}
	prog := p.parseProgram()
	if p.err != nil {
		err = p.err
	}
	return prog, toks, err
}

type parser struct {
	toks []Token
	pos  int
	err  error
}

func (p *parser) peek() Token { return p.toks[p.pos] }
func (p *parser) next() Token { t := p.toks[p.pos]; p.pos++; return t }
func (p *parser) fail(t Token, format string, args ...any) {
	if p.err == nil {
		p.err = &ParseError{Msg: fmt.Sprintf(format, args...), R: Range{Start: t.Start, End: t.End}}
	}
}
func (p *parser) accept(k TokenKind) (Token, bool) {
	if p.peek().Kind == k {
		return p.next(), true
	}
	return Token{}, false
}
func (p *parser) expect(k TokenKind, what string) Token {
	t := p.peek()
	if t.Kind != k {
		p.fail(t, "expected %s", what)
		return t
	}
	return p.next()
}

func (p *parser) parseProgram() *Program {
	prog := &Program{}
	for p.peek().Kind != TEOF {
		var s Stmt
		switch p.peek().Kind {
		case TFn:
			s = p.parseFunc()
		case TLet:
			s = p.parseVar()
		default:
			s = p.parseExprStmt()
		}
		if s != nil {
			prog.Stmts = append(prog.Stmts, s)
		}
		for p.peek().Kind == TSemicolon {
			p.next()
		}
	}
	return prog
}

func (p *parser) parseVar() *VarDecl {
	let := p.expect(TLet, "let")
	name := p.expect(TIdent, "identifier")
	p.expect(TEq, "=")
	val := p.parseExpr()
	end := p.peek().Start
	if val != nil {
		end = val.Pos().End
	}
	return &VarDecl{
		Name: name.Text, NameR: rng(name), LetR: rng(let), Value: val,
		R: Range{Start: let.Start, End: end},
	}
}

func (p *parser) parseFunc() *FuncDecl {
	fn := p.expect(TFn, "fn")
	name := p.expect(TIdent, "identifier")
	p.expect(TLParen, "(")
	var params []Param
	for p.peek().Kind != TRParen && p.peek().Kind != TEOF {
		pt := p.expect(TIdent, "parameter name")
		params = append(params, Param{Name: pt.Text, NameR: rng(pt)})
		if _, ok := p.accept(TComma); !ok {
			break
		}
	}
	p.expect(TRParen, ")")
	body := p.parseBlock()
	end := body.Pos().End
	return &FuncDecl{Name: name.Text, NameR: rng(name), Params: params, Body: body,
		R: Range{Start: fn.Start, End: end}}
}

func (p *parser) parseBlock() *Block {
	lb := p.expect(TLBrace, "{")
	b := &Block{LBraceR: rng(lb)}
	start := lb.Start
	for p.peek().Kind != TRBrace && p.peek().Kind != TEOF {
		var s Stmt
		switch p.peek().Kind {
		case TLet:
			s = p.parseVar()
		case TIf:
			s = p.parseIf()
		default:
			s = p.parseExprStmt()
		}
		if s != nil {
			b.Stmts = append(b.Stmts, s)
		}
		for p.peek().Kind == TSemicolon {
			p.next()
		}
	}
	rb := p.expect(TRBrace, "}")
	b.R = Range{Start: start, End: max(rb.End, start)}
	return b
}

func (p *parser) parseIf() *IfStmt {
	ifTok := p.expect(TIf, "if")
	cond := p.parseExpr()
	then := p.parseBlock()
	s := &IfStmt{Cond: cond, Then: then}
	if _, ok := p.accept(TElse); ok {
		s.Else = p.parseBlock()
	}
	end := then.Pos().End
	if s.Else != nil {
		end = s.Else.Pos().End
	}
	s.R = Range{Start: ifTok.Start, End: end}
	return s
}

func (p *parser) parseExprStmt() Stmt {
	e := p.parseExpr()
	if e == nil {
		t := p.next()
		p.fail(t, "unexpected token %q", t.Text)
		return nil
	}
	return &ExprStmt{Expr: e}
}

// 优先级：加减 < 乘除；调用与原子最高。
func (p *parser) parseExpr() Expr { return p.parseBinary(0) }

func (p *parser) parseBinary(minPrec int) Expr {
	lhs := p.parseUnary()
	for {
		op := p.peek()
		prec := binPrec(op.Kind)
		if prec < minPrec {
			return lhs
		}
		p.next()
		rhs := p.parseBinary(prec + 1)
		lhs = &BinaryExpr{Op: op, LHS: lhs, RHS: rhs}
	}
}

func (p *parser) parseUnary() Expr {
	if t := p.peek(); t.Kind == TMinus {
		p.next()
		inner := p.parseUnary()
		return &BinaryExpr{Op: t, LHS: &IntExpr{Value: "0", R: Range{Start: t.Start, End: t.Start}}, RHS: inner}
	}
	return p.parsePostfix()
}

func (p *parser) parsePostfix() Expr {
	e := p.parseAtom()
	if p.peek().Kind == TLParen {
		lp := p.next()
		var args []Expr
		for p.peek().Kind != TRParen && p.peek().Kind != TEOF {
			args = append(args, p.parseExpr())
			if _, ok := p.accept(TComma); !ok {
				break
			}
		}
		rp := p.expect(TRParen, ")")
		e = &CallExpr{Callee: e, Args: args, R: Range{Start: e.Pos().Start, End: max(rp.End, lp.End)}}
	}
	return e
}

func (p *parser) parseAtom() Expr {
	t := p.peek()
	switch t.Kind {
	case TInt:
		p.next()
		return &IntExpr{Value: t.Text, R: rng(t)}
	case TIdent:
		p.next()
		return &IdentExpr{Name: t.Text, R: rng(t)}
	case TLParen:
		p.next()
		e := p.parseExpr()
		p.expect(TRParen, ")")
		return e
	default:
		return nil
	}
}

func binPrec(k TokenKind) int {
	switch k {
	case TGT:
		return 0
	case TPlus, TMinus:
		return 1
	case TStar, TSlash:
		return 2
	default:
		return -1
	}
}

func rng(t Token) Range { return Range{Start: t.Start, End: t.End} }
