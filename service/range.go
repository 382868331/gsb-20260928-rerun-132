package service

import "nav/symbol"
import "nav/syntax"

func syntaxRange(rf symbol.Ref) syntax.Range {
	return syntax.Range{Start: rf.Start, End: rf.End}
}
