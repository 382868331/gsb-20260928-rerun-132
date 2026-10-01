package syntax

import (
	"fmt"
	"unicode"
	"unicode/utf8"
)

type tokenKind int

const (
	tokEOF tokenKind = iota
	tokIdent
	tokInt
	tokKeyword
	tokOp
	tokPunct
)

type token struct {
	kind  tokenKind
	value string
	r     Range
}

type lexer struct {
	src    []byte
	pos    int
	line   int // 1-based line of the current rune
	col    int // 1-based column of the current rune
	tokens []token
}

var keywords = map[string]bool{
	"let": true, "fn": true, "return": true, "if": true, "else": true,
}

// IsIdent reports whether s is a legal Edulang identifier (and not a
// keyword), using the same character rules as the lexer.
func IsIdent(s string) bool {
	if s == "" || keywords[s] {
		return false
	}
	for i, r := range s {
		if i == 0 && unicode.IsDigit(r) {
			return false
		}
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_') {
			return false
		}
	}
	return true
}

func lex(src string) ([]token, error) {
	l := &lexer{src: []byte(src), line: 1, col: 1}
	for l.pos < len(l.src) {
		r, size := utf8.DecodeRune(l.src[l.pos:])
		if r == utf8.RuneError && size == 1 {
			return nil, fmt.Errorf("syntax error at line %d col %d: invalid utf-8", l.line, l.col)
		}
		switch {
		case r == ' ' || r == '\t' || r == '\r' || r == '\n':
			l.advance(r, size)
		case r == '/' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '/':
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.advanceRaw(1)
			}
		case unicode.IsLetter(r) || r == '_':
			l.lexWord()
		case unicode.IsDigit(r):
			l.lexNumber()
		default:
			if err := l.lexPunct(r, size); err != nil {
				return nil, err
			}
		}
	}
	l.tokens = append(l.tokens, token{kind: tokEOF, r: Range{
		Start:       l.pos,
		End:         l.pos,
		StartLine:   l.line,
		StartColumn: l.col,
		EndLine:     l.line,
		EndColumn:   l.col,
	}})
	return l.tokens, nil
}

func (l *lexer) advance(r rune, size int) {
	l.pos += size
	if r == '\n' {
		l.line++
		l.col = 1
	} else {
		l.col++
	}
}

func (l *lexer) advanceRaw(size int) {
	r, _ := utf8.DecodeRune(l.src[l.pos : l.pos+size])
	l.advance(r, size)
}

func (l *lexer) start() (int, int, int) { return l.pos, l.line, l.col }

func (l *lexer) emit(kind tokenKind, value string, start, sLine, sCol int) {
	l.tokens = append(l.tokens, token{
		kind:  kind,
		value: value,
		r: Range{
			Start:       start,
			End:         l.pos,
			StartLine:   sLine,
			StartColumn: sCol,
			EndLine:     l.line,
			EndColumn:   l.col,
		},
	})
}

func (l *lexer) lexWord() {
	start, sLine, sCol := l.start()
	for l.pos < len(l.src) {
		r, size := utf8.DecodeRune(l.src[l.pos:])
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_') {
			break
		}
		l.advance(r, size)
	}
	word := string(l.src[start:l.pos])
	kind := tokIdent
	if keywords[word] {
		kind = tokKeyword
	}
	l.emit(kind, word, start, sLine, sCol)
}

func (l *lexer) lexNumber() {
	start, sLine, sCol := l.start()
	for l.pos < len(l.src) {
		r, size := utf8.DecodeRune(l.src[l.pos:])
		if !unicode.IsDigit(r) {
			break
		}
		l.advance(r, size)
	}
	l.emit(tokInt, string(l.src[start:l.pos]), start, sLine, sCol)
}

func (l *lexer) lexPunct(r rune, size int) error {
	start, sLine, sCol := l.start()
	if (r == '=' || r == '!' || r == '>' || r == '<') && l.pos+1 < len(l.src) && l.src[l.pos+1] == '=' {
		l.advance(r, size)
		l.advanceRaw(1)
		l.emit(tokOp, string(l.src[start:l.pos]), start, sLine, sCol)
		return nil
	}
	switch r {
	case '+', '-', '*', '=', '>', '<':
		l.advance(r, size)
		l.emit(tokOp, string(r), start, sLine, sCol)
	case '(', ')', '{', '}', ',', ';':
		l.advance(r, size)
		l.emit(tokPunct, string(r), start, sLine, sCol)
	default:
		return fmt.Errorf("syntax error at line %d col %d: unexpected character %q", sLine, sCol, r)
	}
	return nil
}
