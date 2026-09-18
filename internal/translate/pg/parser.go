package pg

import (
	"strconv"
	"strings"
)

// parser walks the token stream of one statement. The grammar is recursive
// descent over the documented subset; precedence, loosest to tightest, is
// OR, AND, NOT, predicate.
type parser struct {
	toks []token
	i    int
}

func (p *parser) peek() token { return p.toks[p.i] }

func (p *parser) advance() token {
	t := p.toks[p.i]
	if t.kind != tkEOF {
		p.i++
	}
	return t
}

func (p *parser) peekKeyword(word string) bool {
	t := p.peek()
	return t.kind == tkKeyword && t.text == word
}

func (p *parser) peekOp(op string) bool {
	t := p.peek()
	return t.kind == tkOp && t.text == op
}

// peekIdentFold reports whether the next token is the bare identifier text
// (case-insensitive), without consuming it — used for PG noise words that
// must stay usable as names ("rows").
func (p *parser) peekIdentFold(text string) bool {
	t := p.peek()
	return t.kind == tkIdent && strings.EqualFold(t.text, text)
}

func (p *parser) expectKeyword(word string) error {
	if !p.peekKeyword(word) {
		return parseErrf("expected %s, got %s", word, describe(p.peek()))
	}
	p.advance()
	return nil
}

func (p *parser) expectOp(op string) error {
	if !p.peekOp(op) {
		return parseErrf("expected %q, got %s", op, describe(p.peek()))
	}
	p.advance()
	return nil
}

// describe renders one token for error messages.
func describe(t token) string {
	if t.kind == tkEOF {
		return "end of statement"
	}
	return "[" + t.text + "]"
}

// vectorOps are the pgvector distance operators. `<+>` is recognized so its
// rejection names the operator rather than the syntax; it has no Milvus
// metric, so it stays unsupported in every position. The other three order
// vector searches (parseOrderItem) and are rejected in WHERE, where a
// distance predicate filters nothing lyrebird can compute (see
// unsupportedDistance).
var vectorOps = map[string]bool{"<->": true, "<=>": true, "<#>": true, "<+>": true}

func unsupportedDistance(op string) error {
	return unsupportedErrf("distance operator [%s] does not filter in WHERE: order the search by it instead — ORDER BY emb %s '[0.1, …]' LIMIT n (scalar filters stay in WHERE)", op, op)
}

// compareOps are the SQL comparison operators the predicate grammar reads;
// the vector operators are recognized separately and rejected by name.
var compareOps = map[string]bool{
	"=": true, "<>": true, "!=": true, "<": true, "<=": true, ">": true, ">=": true,
}

// flipComparison mirrors an operator when the literal sits on the left
// (`5 < views` means the same as `views > 5`).
var flipComparison = map[string]string{
	"=": "=", "<>": "<>", "!=": "<>",
	"<": ">", "<=": ">=", ">": "<", ">=": "<=",
}

// parseSelect reads everything after the SELECT keyword.
func (p *parser) parseSelect() (*Select, error) {
	if p.peekKeyword("DISTINCT") {
		return nil, unsupportedErrf("SELECT DISTINCT is not supported: no dedup step exists yet")
	}
	if p.peekKeyword("ALL") {
		p.advance() // PG default: explicitly nothing
	}

	sel := &Select{}
	cols, star, err := p.parseSelectList()
	if err != nil {
		return nil, err
	}
	sel.columns, sel.star = cols, star

	if err := p.expectKeyword("FROM"); err != nil {
		return nil, err
	}
	if err := p.parseTableRef(sel); err != nil {
		return nil, err
	}
	if p.peekKeyword("WHERE") {
		p.advance()
		w, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		sel.where = w
	}
	if p.peekKeyword("ORDER") {
		p.advance()
		if err := p.expectKeyword("BY"); err != nil {
			return nil, err
		}
		if err := p.parseOrderBy(sel); err != nil {
			return nil, err
		}
	}
	// PG allows LIMIT and OFFSET in either order; each at most once.
	limitSeen, offsetSeen := false, false
	for p.peekKeyword("LIMIT") || p.peekKeyword("OFFSET") {
		if p.peekKeyword("LIMIT") {
			if limitSeen {
				return nil, parseErrf("duplicate LIMIT clause")
			}
			limitSeen = true
			p.advance()
			if p.peekKeyword("ALL") {
				return nil, unsupportedErrf("SELECT without a row cap is not supported: it would stream the whole collection; add LIMIT")
			}
			n, err := p.parseIntCount("LIMIT")
			if err != nil {
				return nil, err
			}
			if n < 0 {
				return nil, illegalErrf("LIMIT must not be negative, got %d", n)
			}
			sel.limit, sel.hasLimit = int(n), true
		} else {
			if offsetSeen {
				return nil, parseErrf("duplicate OFFSET clause")
			}
			offsetSeen = true
			p.advance()
			n, err := p.parseIntCount("OFFSET")
			if err != nil {
				return nil, err
			}
			if n < 0 {
				return nil, illegalErrf("OFFSET must not be negative, got %d", n)
			}
			sel.offset = int(n)
			if p.peekIdentFold("row") || p.peekIdentFold("rows") {
				p.advance()
			}
		}
	}
	return sel, nil
}

// parseSelectList reads the projection: "*" alone, or plain columns.
func (p *parser) parseSelectList() (cols []string, star bool, err error) {
	if p.peekOp("*") {
		p.advance()
		if p.peekOp(",") {
			return nil, false, unsupportedErrf("SELECT * cannot be combined with other items: name the columns instead")
		}
		return nil, true, nil
	}
	for {
		col, err := p.parseSelectItem()
		if err != nil {
			return nil, false, err
		}
		cols = append(cols, col)
		if !p.peekOp(",") {
			return cols, false, nil
		}
		p.advance()
	}
}

// parseSelectItem reads one projection item: a column, and nothing else —
// expressions, functions and aliases name computed columns the read path
// cannot produce yet.
func (p *parser) parseSelectItem() (string, error) {
	t := p.peek()
	switch {
	case t.kind == tkIdent:
		name, err := p.parseColumnRef()
		if err != nil {
			return "", err
		}
		if p.peekOp("(") {
			return "", unsupportedErrf("functions in the SELECT list (count(*), …) are not supported: aggregates are beyond the read path")
		}
		if err := p.rejectCast(); err != nil {
			return "", err
		}
		if p.peekKeyword("AS") || p.peek().kind == tkIdent {
			return "", unsupportedErrf("SELECT aliases are not supported: rename on the client side")
		}
		if p.peek().kind == tkOp && vectorOps[p.peek().text] {
			return "", unsupportedErrf("distance expressions in the SELECT list are not supported: order by the distance (ORDER BY %s %s '[0.1, …]') and project plain columns", name, p.peek().text)
		}
		if p.peek().kind == tkOp && !p.peekOp(",") {
			return "", unsupportedErrf("expressions in the SELECT list ([%s] …) are not supported: project plain columns", name)
		}
		return name, nil
	case t.kind == tkKeyword:
		return "", parseErrf("expected a column or *, got %s", describe(t))
	default:
		return "", unsupportedErrf("SELECT supports plain columns only; %s starts an expression or computed value", describe(t))
	}
}

// parseTableRef reads the FROM target: one table with an optional alias.
func (p *parser) parseTableRef(sel *Select) error {
	t := p.peek()
	if t.kind != tkIdent {
		return parseErrf("expected a table name, got %s", describe(t))
	}
	p.advance()
	sel.Table = t.text

	if p.peekOp(".") {
		return unsupportedErrf("schema-qualified table names are not supported: use the bare collection name")
	}
	if p.peekOp("(") {
		return unsupportedErrf("FROM functions and subqueries are not supported: reads are single-table")
	}
	if p.peekKeyword("AS") {
		p.advance()
		a := p.peek()
		if a.kind != tkIdent {
			return parseErrf("expected an alias after AS, got %s", describe(a))
		}
		p.advance()
		sel.alias = a.text
	} else if p.peek().kind == tkIdent {
		sel.alias = p.advance().text
	}
	if p.peekOp(",") {
		return unsupportedErrf("multiple tables in FROM are not supported: reads are single-table (no joins)")
	}
	for _, kw := range []string{"JOIN", "INNER", "LEFT", "RIGHT", "FULL", "CROSS", "OUTER"} {
		if p.peekKeyword(kw) {
			return unsupportedErrf("joins are not supported: reads are single-table")
		}
	}
	return nil
}

// parseColumnRef reads name or name.qualifier, returned joined with the dot
// (Milvus field names cannot contain one, so the join is unambiguous);
// resolveQualifiers checks the qualifier once FROM is known.
func (p *parser) parseColumnRef() (string, error) {
	t := p.peek()
	if t.kind != tkIdent {
		return "", parseErrf("expected a column name, got %s", describe(t))
	}
	p.advance()
	if err := rejectDotted(t); err != nil {
		return "", err
	}
	name := t.text
	if !p.peekOp(".") {
		return name, nil
	}
	p.advance()
	if p.peekOp("*") {
		return "", unsupportedErrf("qualified star ([%s].*) is not supported: name the columns", name)
	}
	t2 := p.peek()
	if t2.kind != tkIdent {
		return "", parseErrf("expected a column name after [%s.], got %s", name, describe(t2))
	}
	p.advance()
	if err := rejectDotted(t2); err != nil {
		return "", err
	}
	return name + "." + t2.text, nil
}

// rejectDotted refuses quoted identifiers that contain a dot: they would be
// indistinguishable from a qualifier join further down.
func rejectDotted(t token) error {
	if t.quoted && strings.Contains(t.text, ".") {
		return parseErrf("quoted identifier [%s] contains a dot: field and table names are plain identifiers", t.text)
	}
	return nil
}

// rejectCast fails the ::type postfix the subset does not carry (literals
// must already be in storage form).
func (p *parser) rejectCast() error {
	if p.peekOp("::") {
		return unsupportedErrf("casts ([expr]::type) are not supported: pass literals already in storage form")
	}
	return nil
}

// parseOrderBy reads comma-separated ORDER BY terms. A distance term
// (ORDER BY emb <=> '[…]') is the ANN top-k and must stand alone: it is the
// whole sort, so a second ordering term is rejected by name.
func (p *parser) parseOrderBy(sel *Select) error {
	for {
		item, dist, err := p.parseOrderItem()
		if err != nil {
			return err
		}
		if dist != nil {
			if sel.distance != nil || len(sel.order) > 0 || p.peekOp(",") {
				return unsupportedErrf("a distance ORDER BY is the whole sort (the ANN top-k): no second ordering term")
			}
			sel.distance = dist
			return nil
		}
		if sel.distance != nil {
			return unsupportedErrf("a distance ORDER BY is the whole sort (the ANN top-k): no second ordering term")
		}
		sel.order = append(sel.order, item)
		if !p.peekOp(",") {
			return nil
		}
		p.advance()
	}
}

func (p *parser) parseOrderItem() (orderItem, *orderDistance, error) {
	col, err := p.parseColumnRef()
	if err != nil {
		return orderItem{}, nil, err
	}
	if p.peekOp("(") {
		return orderItem{}, nil, unsupportedErrf("ORDER BY functions and expressions are not supported: order by a column")
	}
	if err := p.rejectCast(); err != nil {
		return orderItem{}, nil, err
	}
	if p.peek().kind == tkOp && vectorOps[p.peek().text] {
		op := p.advance().text
		dist, err := p.parseDistanceTail(col, op)
		return orderItem{}, dist, err
	}
	item := orderItem{column: col}
	switch {
	case p.peekKeyword("ASC"):
		p.advance()
	case p.peekKeyword("DESC"):
		p.advance()
		item.desc = true
	}
	return item, nil, nil
}

// parseDistanceTail reads the vector literal after `col op` and the sort
// direction. DESC means farthest-first, which no vector index answers —
// rejecting it beats silently computing exact distances over every entity.
func (p *parser) parseDistanceTail(col, op string) (*orderDistance, error) {
	t := p.peek()
	if t.kind != tkString {
		return nil, parseErrf("distance operator [%s] takes a vector literal in single quotes, got %s", op, describe(t))
	}
	p.advance()
	d := &orderDistance{column: col, op: op, vector: t.text}
	switch {
	case p.peekKeyword("ASC"):
		p.advance()
	case p.peekKeyword("DESC"):
		return nil, unsupportedErrf("DESC on a distance ordering means farthest-first, which no vector index answers: ANN search returns nearest-first")
	}
	return d, nil
}

// parseIntCount reads the (possibly signed) integer a LIMIT/OFFSET clause
// needs. PG parses negative counts and rejects them later; lyrebird does
// the same so the error stays a value error, not a syntax error.
func (p *parser) parseIntCount(clause string) (int64, error) {
	negative := false
	if p.peekOp("-") {
		p.advance()
		negative = true
	}
	t := p.peek()
	if t.kind != tkNumber {
		return 0, parseErrf("%s must be an integer, got %s", clause, describe(t))
	}
	p.advance()
	n, err := strconv.ParseInt(t.text, 10, 64)
	if err != nil {
		return 0, parseErrf("%s must be an integer, got [%s]", clause, t.text)
	}
	if negative {
		n = -n
	}
	return n, nil
}

// parseOr reads the OR level of the WHERE grammar.
func (p *parser) parseOr() (whereExpr, error) {
	first, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	parts := []whereExpr{first}
	for p.peekKeyword("OR") {
		p.advance()
		next, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		parts = append(parts, next)
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	return wOr{parts}, nil
}

// parseAnd reads the AND level of the WHERE grammar.
func (p *parser) parseAnd() (whereExpr, error) {
	first, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	parts := []whereExpr{first}
	for p.peekKeyword("AND") {
		p.advance()
		next, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		parts = append(parts, next)
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	return wAnd{parts}, nil
}

func (p *parser) parseNot() (whereExpr, error) {
	if p.peekKeyword("NOT") {
		p.advance()
		child, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return wNot{child}, nil
	}
	return p.parsePredicate()
}

// parsePredicate reads one comparison level: a parenthesized group, a
// column–literal comparison, IN / BETWEEN / IS NULL / LIKE, a distance
// operator, or a bare boolean value.
func (p *parser) parsePredicate() (whereExpr, error) {
	if p.peekOp("(") {
		p.advance()
		inner, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		return inner, nil
	}

	left, err := p.parseValue()
	if err != nil {
		return nil, err
	}

	switch {
	case p.peekKeyword("IS"):
		p.advance()
		negated := false
		if p.peekKeyword("NOT") {
			p.advance()
			negated = true
		}
		if err := p.expectKeyword("NULL"); err != nil {
			return nil, err
		}
		col, err := columnOf(left, "IS NULL")
		if err != nil {
			return nil, err
		}
		return wNull{col: col, negated: negated}, nil

	case p.peekKeyword("NOT"):
		p.advance()
		return p.parseNegatedTail(left)

	case p.peekKeyword("IN"):
		p.advance()
		return p.parseInTail(left, false)

	case p.peekKeyword("BETWEEN"):
		p.advance()
		return p.parseBetweenTail(left, false)

	case p.peekKeyword("LIKE") || p.peekKeyword("ILIKE"):
		word := p.advance().text
		return nil, unsupportedErrf("[%s] is not supported: lyrebird maps full-text search, not substring patterns; query Milvus directly until the text path lands", word)

	case p.peek().kind == tkOp && vectorOps[p.peek().text]:
		op := p.advance().text
		return nil, unsupportedDistance(op)

	case p.peek().kind == tkOp && compareOps[p.peek().text]:
		op := p.advance().text
		right, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		return combineComparison(left, op, right)

	default:
		return barePredicate(left)
	}
}

// parseNegatedTail reads what follows the NOT of "x NOT IN (…)" and
// "x NOT BETWEEN … AND …".
func (p *parser) parseNegatedTail(left valueNode) (whereExpr, error) {
	switch {
	case p.peekKeyword("IN"):
		p.advance()
		return p.parseInTail(left, true)
	case p.peekKeyword("BETWEEN"):
		p.advance()
		return p.parseBetweenTail(left, true)
	case p.peekKeyword("LIKE") || p.peekKeyword("ILIKE"):
		word := p.advance().text
		return nil, unsupportedErrf("[%s] is not supported: lyrebird maps full-text search, not substring patterns; query Milvus directly until the text path lands", word)
	default:
		return nil, parseErrf("expected IN, BETWEEN or LIKE after NOT, got %s", describe(p.peek()))
	}
}

// parseInTail reads the parenthesized literal list of IN, resolving the
// three-valued edge cases: a NULL member adds nothing to a positive IN
// (UNKNOWN rows never pass) and kills a negated one outright.
func (p *parser) parseInTail(left valueNode, negated bool) (whereExpr, error) {
	col, err := columnOf(left, "IN")
	if err != nil {
		return nil, err
	}
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	if p.peekKeyword("SELECT") {
		return nil, unsupportedErrf("subqueries are not supported: reads are single-table")
	}
	var vals []literal
	hadNull := false
	for {
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		if v.col != "" {
			return nil, unsupportedErrf("IN lists hold literals only; [%s] is a column (column-to-column matching is not supported)", v.col)
		}
		if v.lit.kind == litNull {
			hadNull = true
		} else {
			vals = append(vals, v.lit)
		}
		if !p.peekOp(",") {
			break
		}
		p.advance()
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	if negated && hadNull {
		return wFalse{}, nil
	}
	return wIn{col: col, vals: vals, negated: negated}, nil
}

// parseBetweenTail reads "BETWEEN lo AND hi"; the AND belongs to the
// between, not the enclosing boolean level.
func (p *parser) parseBetweenTail(left valueNode, negated bool) (whereExpr, error) {
	col, err := columnOf(left, "BETWEEN")
	if err != nil {
		return nil, err
	}
	lo, err := p.parseValue()
	if err != nil {
		return nil, err
	}
	if err := p.expectKeyword("AND"); err != nil {
		return nil, err
	}
	hi, err := p.parseValue()
	if err != nil {
		return nil, err
	}
	if lo.col != "" || hi.col != "" {
		return nil, unsupportedErrf("BETWEEN bounds must be literals")
	}
	if lo.lit.kind == litNull || hi.lit.kind == litNull {
		// x BETWEEN lo AND NULL is UNKNOWN whenever lo fails to hold it —
		// no row passes; the exact translation is the empty filter.
		return wFalse{}, nil
	}
	return wBetw{col: col, lo: lo.lit, hi: hi.lit, negated: negated}, nil
}

// combineComparison pairs one column with one literal, flipping a
// literal-first comparison. A NULL side never passes a row (three-valued
// logic), which folds to the constant false.
func combineComparison(left valueNode, op string, right valueNode) (whereExpr, error) {
	if left.lit.kind == litNull || right.lit.kind == litNull {
		return wFalse{}, nil
	}
	switch {
	case left.col == "" && right.col == "":
		return nil, unsupportedErrf("comparison between two constants filters nothing; put the condition on a column")
	case left.col != "" && right.col != "":
		return nil, unsupportedErrf("column-to-column comparison [%s] %s [%s] is not supported: compare a column to a literal", left.col, op, right.col)
	}
	if left.col != "" {
		return wCmp{op: op, col: left.col, val: right.lit}, nil
	}
	// literal on the left, so the operator mirrors
	return wCmp{op: flipComparison[op], col: right.col, val: left.lit}, nil
}

// barePredicate handles a value with no comparison after it: TRUE/FALSE
// constants (nil = matches everything), a NULL (never passes), or a bare
// column — PG requires that to be boolean, so it lowers to col == true.
func barePredicate(v valueNode) (whereExpr, error) {
	switch {
	case v.col != "":
		return wCol{col: v.col}, nil
	case v.lit.kind == litBool:
		if v.lit.text == "true" {
			return nil, nil
		}
		return wFalse{}, nil
	case v.lit.kind == litNull:
		return wFalse{}, nil
	default:
		return nil, parseErrf("expected a comparison after [%s]: a bare literal is not a predicate", v.lit.text)
	}
}

// parseValue reads one comparison operand, rejecting a trailing ::type cast
// on any of its forms.
func (p *parser) parseValue() (valueNode, error) {
	v, err := p.parseValuePrimary()
	if err != nil {
		return valueNode{}, err
	}
	return p.valueTail(v)
}

// parseValuePrimary reads one comparison operand: a column reference, a
// string / number / boolean / NULL literal (numbers take an optional sign),
// or rejects what cannot stand here (subqueries, parameters).
func (p *parser) parseValuePrimary() (valueNode, error) {
	t := p.peek()
	switch {
	case t.kind == tkIdent:
		col, err := p.parseColumnRef()
		if err != nil {
			return valueNode{}, err
		}
		if p.peekOp("(") {
			return valueNode{}, unsupportedErrf("function calls ([%s](…)) are not supported in predicates", col)
		}
		return valueNode{col: col}, nil
	case t.kind == tkString:
		p.advance()
		return valueNode{lit: literal{kind: litString, text: t.text}}, nil
	case t.kind == tkNumber:
		p.advance()
		return valueNode{lit: literal{kind: litNumber, text: t.text}}, nil
	case t.kind == tkKeyword && (t.text == "TRUE" || t.text == "FALSE"):
		p.advance()
		return valueNode{lit: literal{kind: litBool, text: strings.ToLower(t.text)}}, nil
	case t.kind == tkKeyword && t.text == "NULL":
		p.advance()
		return valueNode{lit: literal{kind: litNull}}, nil
	case t.kind == tkOp && (t.text == "-" || t.text == "+"):
		p.advance()
		n := p.peek()
		if n.kind != tkNumber {
			return valueNode{}, parseErrf("expected a number after [%s], got %s", t.text, describe(n))
		}
		p.advance()
		text := n.text
		if t.text == "-" {
			text = "-" + text
		}
		return valueNode{lit: literal{kind: litNumber, text: text}}, nil
	case t.kind == tkParam:
		return valueNode{}, unsupportedErrf("query parameters ([%s]) are not wired yet: they arrive with the PG wire protocol (pgserver)", t.text)
	case t.kind == tkOp && t.text == "(":
		return valueNode{}, unsupportedErrf("subqueries are not supported: reads are single-table")
	}
	return valueNode{}, parseErrf("expected a column or literal, got %s", describe(t))
}

// valueTail rejects what may follow any operand: a ::type cast. Called after
// every successful parseValue branch.
func (p *parser) valueTail(v valueNode) (valueNode, error) {
	if err := p.rejectCast(); err != nil {
		return valueNode{}, err
	}
	return v, nil
}

// resolveQualifiers checks every column reference against the statement's
// single table: items.status and i.status (FROM items i) strip to status;
// any other qualifier would name a second table, and reads cannot join.
func resolveQualifiers(sel *Select) error {
	strip := func(col string) (string, error) {
		qual, name, dotted := strings.Cut(col, ".")
		if !dotted {
			return col, nil
		}
		if qual == sel.Table || qual == sel.alias {
			return name, nil
		}
		return "", unsupportedErrf("column reference [%s] names another table: reads are single-table", col)
	}
	for i, c := range sel.columns {
		col, err := strip(c)
		if err != nil {
			return err
		}
		sel.columns[i] = col
	}
	where, err := stripWhereQualifiers(sel.where, strip)
	if err != nil {
		return err
	}
	sel.where = where
	for i, o := range sel.order {
		col, err := strip(o.column)
		if err != nil {
			return err
		}
		sel.order[i].column = col
	}
	if sel.distance != nil {
		col, err := strip(sel.distance.column)
		if err != nil {
			return err
		}
		sel.distance.column = col
	}
	return nil
}

// stripWhereQualifiers walks the WHERE tree applying one qualifier-stripping
// function to every column reference.
func stripWhereQualifiers(w whereExpr, strip func(string) (string, error)) (whereExpr, error) {
	switch e := w.(type) {
	case nil, wFalse:
		return w, nil
	case wCol:
		col, err := strip(e.col)
		if err != nil {
			return nil, err
		}
		e.col = col
		return e, nil
	case wCmp:
		col, err := strip(e.col)
		if err != nil {
			return nil, err
		}
		e.col = col
		return e, nil
	case wIn:
		col, err := strip(e.col)
		if err != nil {
			return nil, err
		}
		e.col = col
		return e, nil
	case wBetw:
		col, err := strip(e.col)
		if err != nil {
			return nil, err
		}
		e.col = col
		return e, nil
	case wNull:
		col, err := strip(e.col)
		if err != nil {
			return nil, err
		}
		e.col = col
		return e, nil
	case wNot:
		child, err := stripWhereQualifiers(e.child, strip)
		if err != nil {
			return nil, err
		}
		e.child = child
		return e, nil
	case wAnd:
		parts, err := stripParts(e.parts, strip)
		if err != nil {
			return nil, err
		}
		e.parts = parts
		return e, nil
	case wOr:
		parts, err := stripParts(e.parts, strip)
		if err != nil {
			return nil, err
		}
		e.parts = parts
		return e, nil
	}
	return w, nil
}

func stripParts(parts []whereExpr, strip func(string) (string, error)) ([]whereExpr, error) {
	out := make([]whereExpr, len(parts))
	for i, part := range parts {
		stripped, err := stripWhereQualifiers(part, strip)
		if err != nil {
			return nil, err
		}
		out[i] = stripped
	}
	return out, nil
}
