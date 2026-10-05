package syntax

import (
	"fmt"
	"strings"
)

type parser struct {
	tokens []token
	pos    int
	depth  int
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
	case t.kind == tokKeyword && t.value == "module":
		if p.depth > 0 {
			return nil, fmt.Errorf("syntax error at line %d col %d: module directive is only allowed at file top level", t.r.StartLine, t.r.StartColumn)
		}
		return p.parseModule()
	case t.kind == tokKeyword && t.value == "import":
		if p.depth > 0 {
			return nil, fmt.Errorf("syntax error at line %d col %d: import directive is only allowed at file top level", t.r.StartLine, t.r.StartColumn)
		}
		return p.parseImport()
	case t.kind == tokKeyword && t.value == "export":
		if p.depth > 0 {
			return nil, fmt.Errorf("syntax error at line %d col %d: export directive is only allowed at file top level", t.r.StartLine, t.r.StartColumn)
		}
		return p.parseExport()
	case t.kind == tokKeyword && t.value == "return":
		return p.parseReturn()
	case t.kind == tokKeyword && t.value == "if":
		return p.parseIf()
	default:
		return p.parseSimpleStmt()
	}
}

// unquote turns a quoted source literal into its path contents, validating
// that it looks like a module path (dot-separated identifiers).
func unquote(raw string) (string, error) {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return "", fmt.Errorf("malformed string literal %q", raw)
	}
	path := raw[1 : len(raw)-1]
	if path == "" {
		return "", fmt.Errorf("empty module path")
	}
	for _, part := range strings.Split(path, ".") {
		if !IsIdent(part) {
			return "", fmt.Errorf("invalid module path %q: %q is not an identifier", path, part)
		}
	}
	return path, nil
}

func pathBase(path string) string {
	if i := strings.LastIndex(path, "."); i >= 0 {
		return path[i+1:]
	}
	return path
}

// ModulePath returns the module path declared by the file's module
// directive, or "" for an anonymous-world document.
func ModulePath(f *File) string {
	if f == nil {
		return ""
	}
	for _, s := range f.Stmts {
		if m, ok := s.(*ModuleStmt); ok {
			return m.Path
		}
	}
	return ""
}

// Imports maps every import alias of the file to the module path it binds.
// The alias is the explicit 'as' name when present, otherwise the final
// path component.
func Imports(f *File) map[string]string {
	out := map[string]string{}
	if f == nil {
		return out
	}
	for _, s := range f.Stmts {
		imp, ok := s.(*ImportStmt)
		if !ok {
			continue
		}
		alias := imp.Alias
		if !imp.HasAlias {
			alias = pathBase(imp.Path)
		}
		out[alias] = imp.Path
	}
	return out
}

func (p *parser) parseModule() (*ModuleStmt, error) {
	start := p.next() // 'module'
	lit, err := p.want(tokString, "")
	if err != nil {
		return nil, err
	}
	path, err := unquote(lit.value)
	if err != nil {
		return nil, fmt.Errorf("syntax error at line %d col %d: %v", lit.r.StartLine, lit.r.StartColumn, err)
	}
	p.skipSemi()
	return &ModuleStmt{Range: span(start.r, lit.r), Path: path, PathRange: lit.r}, nil
}

func (p *parser) parseImport() (*ImportStmt, error) {
	start := p.next() // 'import'
	lit, err := p.want(tokString, "")
	if err != nil {
		return nil, err
	}
	path, err := unquote(lit.value)
	if err != nil {
		return nil, fmt.Errorf("syntax error at line %d col %d: %v", lit.r.StartLine, lit.r.StartColumn, err)
	}
	s := &ImportStmt{Range: span(start.r, lit.r), Path: path, PathRange: lit.r}
	if p.peek().kind == tokKeyword && p.peek().value == "as" {
		// 'as' is a contextual keyword here so it stays usable as an identifier.
	} else if p.peek().kind == tokIdent && p.peek().value == "as" {
		p.next()
		alias, err := p.want(tokIdent, "")
		if err != nil {
			return nil, err
		}
		if !IsIdent(alias.value) {
			return nil, fmt.Errorf("syntax error at line %d col %d: bad import alias %q", alias.r.StartLine, alias.r.StartColumn, alias.value)
		}
		s.HasAlias = true
		s.Alias, s.AliasRange = alias.value, alias.r
		s.Range = span(start.r, alias.r)
	}
	p.skipSemi()
	return s, nil
}

func (p *parser) parseExport() (*ExportStmt, error) {
	start := p.next() // 'export'
	local, err := p.want(tokIdent, "")
	if err != nil {
		return nil, err
	}
	s := &ExportStmt{Range: span(start.r, local.r), Local: local.value, LocalRange: local.r}
	if p.peek().kind == tokIdent && p.peek().value == "as" {
		p.next()
		ext, err := p.want(tokIdent, "")
		if err != nil {
			return nil, err
		}
		s.HasExternal = true
		s.External, s.ExtRange = ext.value, ext.r
		s.Range = span(start.r, ext.r)
	}
	p.skipSemi()
	return s, nil
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
	p.depth++
	defer func() { p.depth-- }()
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
		var qual string
		var qualRange Range
		expr := Expr(&Ident{Range: t.r, Name: t.value})
		qual, qualRange = t.value, t.r
		for p.peek().kind == tokPunct && p.peek().value == "." {
			p.next() // dot
			sel, err := p.want(tokIdent, "")
			if err != nil {
				return nil, err
			}
			expr = &SelectorExpr{
				Range:     span(t.r, sel.r),
				Qual:      qual,
				QualRange: qualRange,
				Sel:       sel.value,
				SelRange:  sel.r,
			}
			qual, qualRange = sel.value, sel.r
		}
		if p.peek().kind == tokPunct && p.peek().value == "(" {
			return p.parseCall(expr)
		}
		return expr, nil
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

func (p *parser) parseCall(fun Expr) (*CallExpr, error) {
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
	c.Range = span(fun.nodeRange(), close.r)
	return c, nil
}

func span(a, b Range) Range {
	return Range{
		Start: a.Start, End: b.End,
		StartLine: a.StartLine, StartColumn: a.StartColumn,
		EndLine: b.EndLine, EndColumn: b.EndColumn,
	}
}
