package syntax

import (
	"fmt"
	"unicode"
	"unicode/utf8"
)

func Lex(src string) ([]Token, error) {
	var toks []Token
	i := 0
	for i < len(src) {
		r, size := utf8.DecodeRuneInString(src[i:])
		switch {
		case r == ' ' || r == '\t' || r == '\r' || r == '\n':
			i += size
		case r == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case r == '/' && i+1 < len(src) && src[i+1] == '*':
			start := i
			i += 2
			for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			if i+1 >= len(src) {
				return nil, &ParseError{Msg: "unterminated block comment", R: Range{Start: start, End: len(src)}}
			}
			i += 2
		case unicode.IsLetter(r) || r == '_':
			start := i
			for i < len(src) {
				r2, s2 := utf8.DecodeRuneInString(src[i:])
				if !(unicode.IsLetter(r2) || unicode.IsDigit(r2) || r2 == '_') {
					break
				}
				i += s2
			}
			txt := src[start:i]
			kind := TIdent
			switch txt {
			case "let":
				kind = TLet
			case "fn":
				kind = TFn
			case "if":
				kind = TIf
			case "else":
				kind = TElse
			}
			toks = append(toks, Token{Kind: kind, Text: txt, Start: start, End: i})
		case unicode.IsDigit(r):
			start := i
			for i < len(src) && src[i] >= '0' && src[i] <= '9' {
				i++
			}
			toks = append(toks, Token{Kind: TInt, Text: src[start:i], Start: start, End: i})
		default:
			var kind TokenKind
			switch r {
			case '(':
				kind = TLParen
			case ')':
				kind = TRParen
			case '{':
				kind = TLBrace
			case '}':
				kind = TRBrace
			case ',':
				kind = TComma
			case '=':
				kind = TEq
			case '+':
				kind = TPlus
			case '-':
				kind = TMinus
			case '*':
				kind = TStar
			case '/':
				kind = TSlash
			case '>':
				kind = TGT
			case ';':
				kind = TSemicolon
			default:
				return nil, &ParseError{Msg: fmt.Sprintf("unexpected char %q", r), R: Range{Start: i, End: i + size}}
			}
			toks = append(toks, Token{Kind: kind, Text: string(r), Start: i, End: i + size})
			i += size
		}
	}
	toks = append(toks, Token{Kind: TEOF, Start: len(src), End: len(src)})
	return toks, nil
}
