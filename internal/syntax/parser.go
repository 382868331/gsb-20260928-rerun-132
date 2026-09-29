package syntax

import "fmt"

type parser struct {
	tokens []token
	pos    int
}

// Parse lexes and parses one Edulang document. The returned tree is purely a
// syntax object: it does not know about symbols, identity or other files.
func Parse(src string) (*File, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{tokens: toks}
	f := &File{}
	for p.peek().kind != tokEOF {
		s, err := p.parseStmt()
		if err != nil {
			return nil, err
		}
		f.Stmts = append(f.Stmts, s)
	}
	f.Range = Range{Start: 0, End: len(src)}
	if n := len(f.Stmts); n > 0 {
		f.Range.StartLine = f.Stmts[0].nodeRange().StartLine
		f.Range.StartColumn = f.Stmts[0].nodeRange().StartColumn
		last := f.Stmts[n-1].nodeRange()
		f.Range.EndLine, f.Range.EndColumn = last.EndLine, last.EndColumn
	}
	return f, nil
}

func (p *parser) peek() token { return p.tokens[p.pos] }
func (p *parser) atEOF() bool { return p.peek().kind == tokEOF }
func (p *parser) next() token { t := p.tokens[p.pos]; p.pos++; return t }

func (p *parser) want(kind tokenKind, value string) (token, error) {
	t := p.peek()
	if t.kind != kind || (value != "" && t.value != value) {
		want := value
		if want == "" {
			want = kindName(kind)
		}
		return t, fmt.Errorf("syntax error at line %d col %d: expected %q, got %q", t.r.StartLine, t.r.StartColumn, want, t.value)
	}
	return p.next(), nil
}

func kindName(k tokenKind) string {
	switch k {
	case tokEOF:
		return "end of file"
	case tokIdent:
		return "identifier"
	case tokInt:
		return "integer"
	case tokKeyword:
		return "keyword"
	case tokOp:
		return "operator"
	case tokPunct:
		return "punctuation"
	}
	return "token"
}

func (p *parser) skipSemi() {
	for p.peek().kind == tokPunct && p.peek().value == ";" {
		p.next()
	}
}

func (p *parser) parseStmt() (Stmt, error) {
	p.skipSemi()
	t := p.peek()
	switch {
	case t.kind == tokKeyword && t.value == "let":
		return p.parseLet()
	case t.kind == tokKeyword && t.value == "fn":
		return p.parseFn()
	case t.kind == tokKeyword && t.value == "return":
		return p.parseReturn()
	case t.kind == tokKeyword && t.value == "if":
		return p.parseIf()
	default:
		return p.parseSimpleStmt()
	}
}

func (p *parser) parseLet() (*LetStmt, error) {
	start := p.next().r // 'let'
	name, err := p.want(tokIdent, "")
	if err != nil {
		return nil, err
	}
	if _, err := p.want(tokOp, "="); err != nil {
		return nil, err
	}
	val, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	p.skipSemi()
	return &LetStmt{Range: span(start, val.nodeRange()), Name: name.value, NameRange: name.r, Value: val}, nil
}

func (p *parser) parseFn() (*FnStmt, error) {
	start := p.next().r // 'fn'
	name, err := p.want(tokIdent, "")
	if err != nil {
		return nil, err
	}
	if _, err := p.want(tokPunct, "("); err != nil {
		return nil, err
	}
	var params []Param
	for !(p.peek().kind == tokPunct && p.peek().value == ")") {
		param, err := p.want(tokIdent, "")
		if err != nil {
			return nil, err
		}
		params = append(params, Param{Name: param.value, NameRange: param.r})
		if p.peek().kind == tokPunct && p.peek().value == "," {
			p.next()
			continue
		}
		break
	}
	if _, err := p.want(tokPunct, ")"); err != nil {
		return nil, err
	}
	body, err := p.parseBlock()
	if err != nil {
		return nil, err
	}
	return &FnStmt{Range: span(start, body.Range), Name: name.value, NameRange: name.r, Params: params, Body: body}, nil
}

func (p *parser) parseReturn() (*ReturnStmt, error) {
	start := p.next().r // 'return'
	if p.stmtBoundary() {
		p.skipSemi()
		return &ReturnStmt{Range: start}, nil
	}
	val, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	p.skipSemi()
	return &ReturnStmt{Range: span(start, val.nodeRange()), Value: val}, nil
}

func (p *parser) parseIf() (*IfStmt, error) {
	start := p.next().r // 'if'
	cond, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	then, err := p.parseBlock()
	if err != nil {
		return nil, err
	}
	s := &IfStmt{Range: start, Cond: cond, Then: then}
	if p.peek().kind == tokKeyword && p.peek().value == "else" {
		p.next()
		if p.peek().kind == tokKeyword && p.peek().value == "if" {
			elseIf, err := p.parseIf()
			if err != nil {
				return nil, err
			}
			// Wrap the chained if in an implicit block so it shares scope shape.
			s.Else = &BlockStmt{Range: elseIf.Range, Stmts: []Stmt{elseIf}}
		} else {
			elseBlock, err := p.parseBlock()
			if err != nil {
				return nil, err
			}
			s.Else = elseBlock
		}
	}
	s.Range = span(start, endRange(s))
	p.skipSemi()
	return s, nil
}

func endRange(s *IfStmt) Range {
	if s.Else != nil {
		return s.Else.Range
	}
	return s.Then.Range
}

func (p *parser) parseBlock() (*BlockStmt, error) {
	open, err := p.want(tokPunct, "{")
	if err != nil {
		return nil, err
	}
	b := &BlockStmt{}
	p.skipSemi()
	for !(p.peek().kind == tokPunct && p.peek().value == "}") && !p.atEOF() {
		s, err := p.parseStmt()
		if err != nil {
			return nil, err
		}
		b.Stmts = append(b.Stmts, s)
		p.skipSemi()
	}
	close, err := p.want(tokPunct, "}")
	if err != nil {
		return nil, err
	}
	b.Range = span(open.r, close.r)
	return b, nil
}

// stmtBoundary reports whether the current token cannot begin an expression.
func (p *parser) stmtBoundary() bool {
	t := p.peek()
	if t.kind == tokEOF {
		return true
	}
	if t.kind == tokPunct && (t.value == ";" || t.value == "}" || t.value == ")") {
		return true
	}
	if t.kind == tokKeyword {
		return true
	}
	return false
}

func (p *parser) parseSimpleStmt() (Stmt, error) {
	expr, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	// Assignment is the only statement that begins with an identifier followed by '='.
	if id, ok := expr.(*Ident); ok && p.peek().kind == tokOp && p.peek().value == "=" {
		p.next() // '='
		val, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		p.skipSemi()
		return &AssignStmt{Range: span(id.Range, val.nodeRange()), Name: id.Name, NameRange: id.Range, Value: val}, nil
	}
	p.skipSemi()
	return &ExprStmt{Range: expr.nodeRange(), X: expr}, nil
}

var precedence = map[string]int{
	"==": 1, "!=": 1, ">": 1, "<": 1, ">=": 1, "<=": 1,
	"+": 2, "-": 2,
	"*": 3,
}

func (p *parser) parseExpr() (Expr, error) {
	return p.parseBinary(0)
}

func (p *parser) parseBinary(minPrec int) (Expr, error) {
	left, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.kind != tokOp || precedence[t.value] < minPrec {
			break
		}
		op := p.next()
		right, err := p.parseBinary(precedence[op.value] + 1)
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Range: span(left.nodeRange(), right.nodeRange()), X: left, Op: op.value, Y: right}
	}
	return left, nil
}

func (p *parser) parsePrimary() (Expr, error) {
	t := p.peek()
	switch t.kind {
	case tokInt:
		p.next()
		return &BasicLit{Range: t.r, Value: t.value}, nil
	case tokIdent:
		p.next()
		id := &Ident{Range: t.r, Name: t.value}
		if p.peek().kind == tokPunct && p.peek().value == "(" {
			return p.parseCall(id)
		}
		return id, nil
	case tokPunct:
		if t.value == "(" {
			p.next()
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if _, err := p.want(tokPunct, ")"); err != nil {
				return nil, err
			}
			return e, nil
		}
	}
	return nil, fmt.Errorf("syntax error at line %d col %d: unexpected token %q", t.r.StartLine, t.r.StartColumn, t.value)
}

func (p *parser) parseCall(fun *Ident) (*CallExpr, error) {
	p.next() // '('
	c := &CallExpr{Fun: fun}
	for !(p.peek().kind == tokPunct && p.peek().value == ")") {
		arg, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		c.Args = append(c.Args, arg)
		if p.peek().kind == tokPunct && p.peek().value == "," {
			p.next()
			continue
		}
		break
	}
	close, err := p.want(tokPunct, ")")
	if err != nil {
		return nil, err
	}
	c.Range = span(fun.Range, close.r)
	return c, nil
}

func span(a, b Range) Range {
	return Range{
		Start: a.Start, End: b.End,
		StartLine: a.StartLine, StartColumn: a.StartColumn,
		EndLine: b.EndLine, EndColumn: b.EndColumn,
	}
}
