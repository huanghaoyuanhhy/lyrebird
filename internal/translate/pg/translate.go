package pg

import (
	"fmt"

	"github.com/huanghaoyuanhhy/lyrebird/internal/translate"
)

// SQLSTATE codes fill translate.Error.Type on this frontend. The PG wire
// protocol carries the code verbatim, so a classified failure renders a
// native error with no mapping table in the server shell. Status keeps the
// shared struct's HTTP meaning (every translation failure is 400); wire
// responses will read the code, not the status.
const (
	errSyntax      = "42601" // syntax_error
	errBadValue    = "22023" // invalid_parameter_value
	errUnsupported = "0A000" // feature_not_supported
)

func parseErrf(format string, args ...any) *translate.Error {
	return &translate.Error{Type: errSyntax, Status: 400, Reason: fmt.Sprintf(format, args...)}
}

func illegalErrf(format string, args ...any) *translate.Error {
	return &translate.Error{Type: errBadValue, Status: 400, Reason: fmt.Sprintf(format, args...)}
}

func unsupportedErrf(format string, args ...any) *translate.Error {
	return &translate.Error{Type: errUnsupported, Status: 400, Reason: fmt.Sprintf(format, args...)}
}

// maxResultWindow caps OFFSET+LIMIT, parity with the ES front: the store
// sorts client-side within one window, so both protocols draw the same line.
const maxResultWindow = 10000

// Select is one parsed SELECT statement. It is the collection-name carrier
// the plan deliberately is not: SQL names its table in FROM, while
// translate.Plan stays protocol-agnostic and the store Executor takes the
// collection as a separate argument (the ES front sources the same name
// from the URL path).
type Select struct {
	// Table is the collection this statement reads: the (unquoted) FROM
	// name, as written — no case folding (see the package documentation).
	Table string

	// alias is set when the statement says FROM items i; column qualifiers
	// are checked against it and the table name.
	alias string

	// columns lists the projected fields; star means SELECT * (every field).
	columns []string
	star    bool

	where  whereExpr // nil = no WHERE clause
	order  []orderItem
	limit  int
	offset int

	// distance is the pgvector ORDER BY term (`emb <=> '[0.1, …]'`), nil
	// when ordering by plain columns. It is statement-wide, not an item of
	// order: a distance ordering is the whole sort (the ANN top-k), so a
	// second ordering term is rejected at parse time.
	distance *orderDistance

	// hasLimit distinguishes LIMIT 0 (a valid, count-only plan) from a
	// statement with no LIMIT clause at all.
	hasLimit bool
}

// orderItem is one ORDER BY term.
type orderItem struct {
	column string
	desc   bool
}

// orderDistance is the parsed pgvector distance ordering: the vector field,
// the operator as written, and the vector literal's text (the part between
// the quotes). Fields resolve and the literal parses into numbers at
// lowering time, where the schema is known.
type orderDistance struct {
	column string
	op     string
	vector string
}

// Parse compiles one SQL statement (the SELECT subset described in the
// package documentation) into a Select. Everything that needs no schema
// happens here: syntax errors, capability rejections, paging bounds. The
// caller needs the returned Select.Table to resolve the collection's schema
// before lowering.
func Parse(sql string) (*Select, error) {
	toks, err := lex(sql)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}

	first := p.peek()
	if first.kind == tkEOF {
		return nil, parseErrf("empty statement")
	}
	if first.kind != tkKeyword {
		return nil, parseErrf("expected a SQL statement, got %s", describe(first))
	}
	if first.text == "SELECT" {
		p.advance()
	} else {
		return nil, unsupportedErrf("only SELECT statements are supported; [%s] is beyond lyrebird's read path (DDL mapping is Phase 3)", first.text)
	}

	sel, err := p.parseSelect()
	if err != nil {
		return nil, err
	}
	if err := resolveQualifiers(sel); err != nil {
		return nil, err
	}
	if err := statementEnd(p); err != nil {
		return nil, err
	}
	// policy after structure: a malformed statement reports its syntax, and
	// only a well-formed one without a row cap gets the capability message
	if !sel.hasLimit {
		return nil, unsupportedErrf("SELECT without a row cap is not supported: it would stream the whole collection; add LIMIT")
	}
	return sel, nil
}

// statementEnd accepts EOF or one trailing semicolon; anything else is
// either an unsupported clause (GROUP BY, UNION, …) or stray syntax.
func statementEnd(p *parser) error {
	if p.peekOp(";") {
		p.advance()
		if p.peek().kind != tkEOF {
			return parseErrf("one statement at a time: unexpected content after the first ';'")
		}
		return nil
	}
	t := p.peek()
	if t.kind == tkEOF {
		return nil
	}
	if t.kind == tkKeyword {
		if unsupportedClauses[t.text] {
			return unsupportedErrf("clause [%s] is beyond the lyrebird SELECT subset: single-table scalar reads only", t.text)
		}
		if t.text == "LIMIT" || t.text == "OFFSET" || t.text == "ORDER" || t.text == "WHERE" || t.text == "FROM" {
			return parseErrf("unexpected [%s]: each clause may appear once", t.text)
		}
	}
	return parseErrf("unexpected %s after the statement", describe(t))
}

// unsupportedClauses are the keywords that start a valid PG clause the
// subset rejects by name rather than as stray syntax.
var unsupportedClauses = map[string]bool{
	"GROUP": true, "HAVING": true, "WINDOW": true,
	"UNION": true, "INTERSECT": true, "EXCEPT": true,
	"FOR": true, "FETCH": true, "INTO": true,
}

// Columns resolves the projected column list: the named columns in written
// order, or — for SELECT * — every field the schema knows, in storage order.
// The wire layer needs this before executing: RowDescription precedes the
// rows it describes, so the star cannot wait for the store's answer.
func (s *Select) Columns(schema translate.Schema) []string {
	if !s.star {
		return append([]string{}, s.columns...)
	}
	if schema == nil {
		return nil
	}
	return schema.Fields()
}

// Plan lowers the statement into a store-executable plan. schema answers
// field-type questions for Select.Table (Executor.Schema); nil treats every
// field as unknown, enough until the Phase 3 schema catalog.
func (s *Select) Plan(schema translate.Schema) (*translate.Plan, error) {
	if schema == nil {
		schema = translate.MapSchema{}
	}
	if s.offset+s.limit > maxResultWindow {
		return nil, illegalErrf("result window too large: OFFSET %d + LIMIT %d must not exceed %d (the store sorts client-side within one window)", s.offset, s.limit, maxResultWindow)
	}

	plan := &translate.Plan{
		Offset: s.offset,
		Limit:  s.limit,
		Source: translate.SourceFilter{FetchSource: true, Includes: s.columns},
	}
	for _, o := range s.order {
		plan.Sort = append(plan.Sort, translate.SortClause{Field: o.column, Desc: o.desc})
	}
	if s.distance != nil {
		spec, err := searchSpec(s.distance, schema)
		if err != nil {
			return nil, err
		}
		plan.Search = spec
	}

	w := fold(s.where)
	if isFalse(w) {
		plan.NoMatch = true
		return plan, nil
	}
	expr, err := buildWhere(w, schema)
	if err != nil {
		return nil, err
	}
	plan.Expr = expr
	return plan, nil
}
