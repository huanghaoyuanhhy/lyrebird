package pg

import "strings"

// tokenKind classifies one lexed SQL token.
type tokenKind int

const (
	tkEOF     tokenKind = iota
	tkKeyword           // reserved word; text holds the canonical upper-case form
	tkIdent             // bare or "quoted" identifier; text holds the name as written
	tkString            // single-quoted literal; text holds the value with '' unescaped
	tkNumber            // numeric literal; text holds the digits as written
	tkOp                // operator or punctuation; text holds the characters
	tkParam             // $n placeholder; text holds "$n"
)

type token struct {
	kind   tokenKind
	text   string
	pos    int // byte offset into the source, for error messages
	quoted bool
}

// keywords are the reserved words the grammar consumes, plus the first
// keywords of non-SELECT statements and unsupported clauses, so those get a
// capability message instead of a bare syntax error. The treatment is
// stricter than PG's reserved-word list; a double-quoted identifier still
// rescues any colliding name.
var keywords = map[string]bool{
	// statement body
	"SELECT": true, "FROM": true, "WHERE": true, "AND": true, "OR": true,
	"NOT": true, "IN": true, "IS": true, "NULL": true, "BETWEEN": true,
	"AS": true, "ASC": true, "DESC": true, "DISTINCT": true, "LIMIT": true,
	"OFFSET": true, "TRUE": true, "FALSE": true, "ORDER": true, "BY": true,
	"ALL": true, "LIKE": true, "ILIKE": true,
	// unsupported clauses, rejected by name
	"GROUP": true, "HAVING": true, "WINDOW": true,
	"UNION": true, "INTERSECT": true, "EXCEPT": true,
	"FOR": true, "FETCH": true, "INTO": true, "WITH": true,
	"JOIN": true, "INNER": true, "LEFT": true, "RIGHT": true, "FULL": true,
	"OUTER": true, "CROSS": true, "ON": true, "USING": true,
	// non-SELECT statements, rejected by name
	"INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true,
	"CREATE": true, "DROP": true, "ALTER": true, "TRUNCATE": true,
	"BEGIN": true, "COMMIT": true, "ROLLBACK": true,
	"SET": true, "SHOW": true, "RESET": true,
	"PREPARE": true, "EXECUTE": true, "EXPLAIN": true, "ANALYZE": true,
	"VACUUM": true, "COPY": true, "GRANT": true, "REVOKE": true,
	"VALUES": true, "TABLE": true, "CALL": true, "LOCK": true,
}

// operators lists the operator/punctuation tokens longest first, so "<=>"
// wins over "<" and "::" over ":".
var operators = []string{
	"<=>", "<->", "<#>", "<+>", "::", "<=", ">=", "<>", "!=",
	"=", "<", ">", "*", "(", ")", ",", ".", ";", "+", "-",
}

// lex turns the statement into a token stream ending with tkEOF. It fails
// on unterminated literals, stray characters and dollar quoting — things
// the grammar would only trip over later with a worse message.
func lex(src string) ([]token, error) {
	var toks []token
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '-' && strings.HasPrefix(src[i:], "--"):
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && strings.HasPrefix(src[i:], "/*"):
			end, ok := lexBlockComment(src, i)
			if !ok {
				return nil, parseErrf("unterminated /* comment")
			}
			i = end
		case c == '\'':
			text, n, err := lexQuoted(src, i, '\'')
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{kind: tkString, text: text, pos: i})
			i += n
		case c == '"':
			text, n, err := lexQuoted(src, i, '"')
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{kind: tkIdent, text: text, pos: i, quoted: true})
			i += n
		case isDigit(c) || (c == '.' && i+1 < len(src) && isDigit(src[i+1])):
			n := numberLength(src, i)
			toks = append(toks, token{kind: tkNumber, text: src[i : i+n], pos: i})
			i += n
		case isIdentStart(c):
			j := i
			for j < len(src) && isIdentPart(src[j]) {
				j++
			}
			word := src[i:j]
			if upper := strings.ToUpper(word); keywords[upper] {
				toks = append(toks, token{kind: tkKeyword, text: upper, pos: i})
			} else {
				toks = append(toks, token{kind: tkIdent, text: word, pos: i})
			}
			i = j
		case c == '$' && i+1 < len(src) && isDigit(src[i+1]):
			j := i + 1
			for j < len(src) && isDigit(src[j]) {
				j++
			}
			toks = append(toks, token{kind: tkParam, text: src[i:j], pos: i})
			i = j
		case c == '$':
			return nil, parseErrf("dollar-quoted strings are not supported: use single quotes")
		default:
			op := ""
			for _, candidate := range operators {
				if strings.HasPrefix(src[i:], candidate) {
					op = candidate
					break
				}
			}
			if op == "" {
				return nil, parseErrf("unexpected character %q at offset %d", c, i)
			}
			toks = append(toks, token{kind: tkOp, text: op, pos: i})
			i += len(op)
		}
	}
	return append(toks, token{kind: tkEOF, pos: i}), nil
}

// lexBlockComment returns the offset past a (nesting, like PG) /* comment.
func lexBlockComment(src string, start int) (int, bool) {
	depth := 1
	i := start + 2
	for depth > 0 {
		if i+1 >= len(src) {
			return 0, false
		}
		switch {
		case src[i] == '/' && src[i+1] == '*':
			depth++
			i += 2
		case src[i] == '*' && src[i+1] == '/':
			depth--
			i += 2
		default:
			i++
		}
	}
	return i, true
}

// lexQuoted reads a single-quoted string or double-quoted identifier whose
// terminator is doubled (” or "") to embed itself.
func lexQuoted(src string, start int, quote byte) (text string, n int, err error) {
	var b strings.Builder
	i := start + 1
	for {
		if i >= len(src) {
			return "", 0, parseErrf("unterminated %c-quoted literal", quote)
		}
		if src[i] == quote {
			if i+1 < len(src) && src[i+1] == quote {
				b.WriteByte(quote)
				i += 2
				continue
			}
			return b.String(), i + 1 - start, nil
		}
		b.WriteByte(src[i])
		i++
	}
}

// numberLength measures an SQL numeric literal: digits, optional fraction,
// optional exponent ("1", "1.5", ".5", "1e-3").
func numberLength(src string, i int) int {
	j := i
	for j < len(src) && isDigit(src[j]) {
		j++
	}
	if j < len(src) && src[j] == '.' {
		j++
		for j < len(src) && isDigit(src[j]) {
			j++
		}
	}
	if j < len(src) && (src[j] == 'e' || src[j] == 'E') {
		k := j + 1
		if k < len(src) && (src[k] == '+' || src[k] == '-') {
			k++
		}
		if k < len(src) && isDigit(src[k]) {
			j = k
			for j < len(src) && isDigit(src[j]) {
				j++
			}
		}
	}
	return j - i
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isIdentStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
}

func isIdentPart(c byte) bool { return isIdentStart(c) || isDigit(c) }
