package pg

import (
	"encoding/json"
	"strings"

	"github.com/huanghaoyuanhhy/lyrebird/internal/translate"
)

// The write path is the parallel narrow lane the design proposal settled on
// (docs/design.md, write path): INSERT and UPDATE do not enter the read
// Plan machinery as statements — they parse into their own statement types,
// lower into plain write rows against the schema, and the store executes
// them as inserts and read-modify-write upserts. Strict schema checking
// lives in the store; lowering here only maps SQL literals onto the write
// value vocabulary (translate.WriteRow).

// Insert is one parsed INSERT statement.
type Insert struct {
	// Table is the collection the rows land in, as written.
	Table string

	// columns is the optional target column list; empty means "every
	// writable field, in storage order" (the PG positional default — the
	// schema's function outputs are not writable and never counted).
	columns []string

	// rows holds each VALUES group's literals, positionally aligned with
	// columns.
	rows [][]literal
}

// ParseInsert compiles one INSERT statement: INSERT INTO t [(cols)] VALUES
// (…)[, (…)]. Multi-row inserts stay one statement; anything that changes
// that shape (SELECT sources, ON CONFLICT, DEFAULT keywords) is rejected by
// name. Schema-dependent checks (unknown columns, type mismatches) wait for
// Rows, where the schema is known.
func ParseInsert(sql string) (*Insert, error) {
	toks, err := lex(sql)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}

	if err := p.expectKeyword("INSERT"); err != nil {
		return nil, err
	}
	if err := p.expectKeyword("INTO"); err != nil {
		return nil, err
	}

	t := p.peek()
	if t.kind != tkIdent {
		return nil, parseErrf("expected a table name after INSERT INTO, got %s", describe(t))
	}
	p.advance()
	ins := &Insert{Table: t.text}
	if p.peekOp(".") {
		return nil, unsupportedErrf("schema-qualified table names are not supported: use the bare collection name")
	}
	if p.peekOp("(") {
		// the optional target column list
		p.advance()
		for {
			c, err := p.parseColumnRef()
			if err != nil {
				return nil, err
			}
			ins.columns = append(ins.columns, c)
			if !p.peekOp(",") {
				break
			}
			p.advance()
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		for i, c := range ins.columns {
			for _, prev := range ins.columns[:i] {
				if prev == c {
					return nil, parseErrf("column [%s] appears twice in the column list", c)
				}
			}
		}
	}

	if err := p.expectKeyword("VALUES"); err != nil {
		if p.peek().kind == tkKeyword && p.peek().text == "SELECT" {
			return nil, unsupportedErrf("INSERT ... SELECT is not supported: supply VALUES rows")
		}
		return nil, err
	}
	for {
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		var group []literal
		for {
			lit, err := p.parseWriteLiteral()
			if err != nil {
				return nil, err
			}
			group = append(group, lit)
			if !p.peekOp(",") {
				break
			}
			p.advance()
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		if len(ins.rows) > 0 && len(group) != len(ins.rows[0]) {
			return nil, parseErrf("VALUES groups must all have the same arity: the first has %d values, this one %d", len(ins.rows[0]), len(group))
		}
		ins.rows = append(ins.rows, group)
		if !p.peekOp(",") {
			break
		}
		p.advance()
	}
	if err := writeStatementEnd(p, "INSERT"); err != nil {
		return nil, err
	}
	return ins, nil
}

// Rows lowers the statement into write rows against the schema: numbers
// stay as written (json.Number, no float round-trip), booleans and NULL
// lower directly, and a vector field's pgvector literal parses to fp32s.
// Type matching against the schema happens again in the store — strictly —
// so an unexpected string or a bad number is rejected, never coerced.
func (ins *Insert) Rows(schema translate.Schema) ([]translate.WriteRow, error) {
	if schema == nil {
		schema = translate.MapSchema{}
	}
	cols := ins.columns
	if len(cols) == 0 {
		// the positional default: every writable field in storage order
		cols = schema.Fields()
	}
	out := make([]translate.WriteRow, len(ins.rows))
	for r, group := range ins.rows {
		if len(group) != len(cols) {
			return nil, parseErrf("INSERT has %d values but %d target columns", len(group), len(cols))
		}
		row := make(translate.WriteRow, len(cols))
		for i, lit := range group {
			v, err := writeLiteral(lit, cols[i], schema)
			if err != nil {
				return nil, err
			}
			row[cols[i]] = v
		}
		out[r] = row
	}
	return out, nil
}

// Update is one parsed UPDATE statement: a SET template plus the filter
// naming the rows to rewrite.
type Update struct {
	Table string

	// sets is the SET list: column → literal template applied to every
	// matched row. Expressions (SET n = n + 1) are beyond the lane.
	sets []setClause

	where whereExpr // nil = no WHERE clause (rejected at parse time)
}

type setClause struct {
	column string
	value  literal
}

// ParseUpdate compiles one UPDATE statement. A bare UPDATE without WHERE
// would rewrite the whole collection and is refused the way a read without
// LIMIT is: name the rows.
func ParseUpdate(sql string) (*Update, error) {
	toks, err := lex(sql)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}

	if err := p.expectKeyword("UPDATE"); err != nil {
		return nil, err
	}
	t := p.peek()
	if t.kind != tkIdent {
		return nil, parseErrf("expected a table name after UPDATE, got %s", describe(t))
	}
	p.advance()
	u := &Update{Table: t.text}
	if p.peekOp(".") {
		return nil, unsupportedErrf("schema-qualified table names are not supported: use the bare collection name")
	}
	// UPDATE t AS a / UPDATE t a: no alias — the read path qualifies columns
	// against one, and writes have no use for a second name.
	if p.peekKeyword("AS") || p.peek().kind == tkIdent {
		return nil, unsupportedErrf("UPDATE aliases are not supported: qualify columns with the table name")
	}

	if err := p.expectKeyword("SET"); err != nil {
		return nil, err
	}
	for {
		col, err := p.parseColumnRef()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp("="); err != nil {
			return nil, err
		}
		lit, err := p.parseWriteLiteral()
		if err != nil {
			return nil, err
		}
		u.sets = append(u.sets, setClause{column: col, value: lit})
		if !p.peekOp(",") {
			break
		}
		p.advance()
	}
	for i, s := range u.sets {
		for _, prev := range u.sets[:i] {
			if prev.column == s.column {
				return nil, parseErrf("multiple assignments to column [%s]", s.column)
			}
		}
	}

	if p.peekKeyword("FROM") {
		return nil, unsupportedErrf("UPDATE ... FROM is not supported: writes are single-table")
	}
	if p.peekKeyword("WHERE") {
		p.advance()
		w, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		u.where = w
	} else {
		return nil, unsupportedErrf("UPDATE without WHERE would rewrite every row: add a WHERE clause naming the rows")
	}
	if err := writeStatementEnd(p, "UPDATE"); err != nil {
		return nil, err
	}

	// qualifiers: columns may be written as items.status; anything else
	// names a second table
	if err := u.resolveQualifiers(); err != nil {
		return nil, err
	}
	return u, nil
}

// resolveQualifiers strips the table qualifier off SET targets and WHERE
// column references, rejecting references to other tables.
func (u *Update) resolveQualifiers() error {
	strip := func(col string) (string, error) {
		qual, name, dotted := strings.Cut(col, ".")
		if !dotted {
			return col, nil
		}
		if qual == u.Table {
			return name, nil
		}
		return "", unsupportedErrf("column reference [%s] names another table: writes are single-table", col)
	}
	for i, s := range u.sets {
		col, err := strip(s.column)
		if err != nil {
			return err
		}
		u.sets[i].column = col
	}
	where, err := stripWhereQualifiers(u.where, strip)
	if err != nil {
		return err
	}
	u.where = where
	return nil
}

// Lower resolves the SET template against the schema and folds the WHERE
// into the filter plan the caller executes to fetch the matched rows. The
// plan fetches every field (a read-modify-write needs the whole row to
// re-write it) and sets no limit — the caller bounds the match count.
// set maps onto the write value vocabulary exactly like INSERT values.
func (u *Update) Lower(schema translate.Schema) (set translate.WriteRow, plan *translate.Plan, err error) {
	if schema == nil {
		schema = translate.MapSchema{}
	}
	set = make(translate.WriteRow, len(u.sets))
	for _, s := range u.sets {
		v, err := writeLiteral(s.value, s.column, schema)
		if err != nil {
			return nil, nil, err
		}
		set[s.column] = v
	}

	plan = &translate.Plan{Source: translate.SourceFilter{FetchSource: true}}
	w := fold(u.where)
	if isFalse(w) {
		plan.NoMatch = true
		return set, plan, nil
	}
	expr, err := buildWhere(w, schema)
	if err != nil {
		return nil, nil, err
	}
	plan.Expr = expr
	return set, plan, nil
}

// writeLiteral maps one SQL literal onto the write value vocabulary for the
// named column. A string naming a vector field is the pgvector literal form
// ('[0.1, …]') and parses to fp32s here, where the schema is known.
func writeLiteral(l literal, col string, schema translate.Schema) (any, error) {
	switch l.kind {
	case litNull:
		return nil, nil
	case litBool:
		return l.text == "true", nil
	case litNumber:
		// keep the digits as written: the store validates per storage type,
		// and Int64 fields must not lose precision to a float round-trip
		return json.Number(l.text), nil
	case litString:
		if schema.FieldType(col) == translate.TypeVector {
			return parseVector(l.text)
		}
		return l.text, nil
	default:
		return nil, parseErrf("unsupported literal kind for [%s]", col)
	}
}

// parseWriteLiteral reads one literal for the write path: strings, numbers
// (optionally signed), TRUE/FALSE, NULL. Everything else a value position
// can hold — column references (self-referencing SET), function calls,
// DEFAULT, bind parameters — is rejected with a message naming the gap.
func (p *parser) parseWriteLiteral() (literal, error) {
	t := p.peek()
	switch {
	case t.kind == tkString:
		p.advance()
		return literal{kind: litString, text: t.text}, nil
	case t.kind == tkNumber:
		p.advance()
		return literal{kind: litNumber, text: t.text}, nil
	case t.kind == tkKeyword && t.text == "TRUE":
		p.advance()
		return literal{kind: litBool, text: "true"}, nil
	case t.kind == tkKeyword && t.text == "FALSE":
		p.advance()
		return literal{kind: litBool, text: "false"}, nil
	case t.kind == tkKeyword && t.text == "NULL":
		p.advance()
		return literal{kind: litNull}, nil
	case t.kind == tkKeyword && t.text == "DEFAULT":
		return literal{}, unsupportedErrf("DEFAULT is not supported: supply the value explicitly (the store has no server-side defaults)")
	case t.kind == tkOp && (t.text == "-" || t.text == "+"):
		p.advance()
		n := p.peek()
		if n.kind != tkNumber {
			return literal{}, parseErrf("expected a number after [%s], got %s", t.text, describe(n))
		}
		p.advance()
		text := n.text
		if t.text == "-" {
			text = "-" + text
		}
		return literal{kind: litNumber, text: text}, nil
	case t.kind == tkIdent:
		return literal{}, unsupportedErrf("write values are literals only: [%s] is a column (SET expressions like [n = n + 1] are beyond the write lane)", t.text)
	case t.kind == tkParam:
		return literal{}, unsupportedErrf("bind parameters are not wired into writes yet: inline the literal")
	default:
		return literal{}, parseErrf("expected a literal value, got %s", describe(t))
	}
}

// writeStatementEnd accepts EOF or one trailing semicolon, with the write
// path's own named rejections.
func writeStatementEnd(p *parser, what string) error {
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
		switch t.text {
		case "ON":
			return unsupportedErrf("ON CONFLICT is not supported: a duplicate key fails the write (check existence first)")
		case "RETURNING":
			return unsupportedErrf("RETURNING is not supported: read the rows back with SELECT")
		}
	}
	return parseErrf("unexpected %s after the %s statement", describe(t), what)
}
