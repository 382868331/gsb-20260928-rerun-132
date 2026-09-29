package analyze

import "nav/syntax"

func mustRange(start, end int) syntax.Range { return syntax.Range{Start: start, End: end} }
