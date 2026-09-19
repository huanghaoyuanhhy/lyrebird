package catalog

import "strings"

// SplitStatements breaks a query string into its semicolon-separated
// statements, respecting string literals, quoted identifiers, escape
// strings and comments (the catalog lexer does the scanning). psql sends
// its \d family as one multi-statement string and psql-wire does not split
// it; the wire layer splits here instead and answers each statement in
// order, the way PostgreSQL does.
//
// A lex error returns the unsplit input — the dispatcher's own error for it
// is more useful than a splitting failure. Empty statements are dropped.
func SplitStatements(sql string) []string {
	toks, err := lex(sql)
	if err != nil {
		return []string{sql}
	}

	parts := make([]string, 0, 4)
	start := 0
	for _, t := range toks {
		if t.kind == tkEOF {
			break
		}
		if t.kind == tkOp && t.text == ";" {
			if part := strings.TrimSpace(sql[start:t.pos]); part != "" {
				parts = append(parts, part)
			}
			start = t.pos + 1
		}
	}
	if part := strings.TrimSpace(sql[start:]); part != "" {
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return []string{sql}
	}
	return parts
}
