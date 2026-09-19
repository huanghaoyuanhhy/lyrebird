package catalog

import (
	"strconv"
	"strings"
)

// The catalog query AST and its recursive-descent parser. Unquoted
// identifiers fold to lower case (PG catalog semantics); quoted ones keep
// their written case.

type expr interface{}

type literal struct {
	v    any // string, float64, bool, nil
}

type colRef struct {
	tbl  string // qualifier as written (lower-cased); "" when unqualified
	name string
}

type funcCall struct {
	name string // lower-cased, qualified name collapsed to its last part? no: full dotted name, lower-cased
	args []expr
}

type paramRef struct {
	n int // 1-based
}

type binExpr struct {
	op   string // = <> < > <= >= AND OR LIKE ILIKE ~ !~ BETWEEN + - * /
	l, r expr
	// not flips the comparison (NOT LIKE); only meaningful for the
	// comparison-family operators.
	not bool
	// anyList marks the = ANY (expr) form: r is the array operand.
	anyList bool
}

type isNullExpr struct {
	e   expr
	not bool
}

type inExpr struct {
	e    expr
	list []expr
	not  bool
}

type caseExpr struct {
	operand expr // nil for the searched CASE form
	whens   []whenClause
	elseE   expr
}

type whenClause struct {
	cond expr
	then expr
}

type castExpr struct {
	e     expr
	target string // lower-cased type name
}

type subscriptExpr struct {
	base expr
	idx  expr
}

// subqueryExpr is a scalar subquery: (SELECT expr …) evaluated against the
// enclosing row (correlation falls through the evalCtx parent chain).
type subqueryExpr struct {
	st *stmt
}

// arraySubqueryExpr is ARRAY(SELECT …): every row's first output column as
// an array value.
type arraySubqueryExpr struct {
	st *stmt
}

// stmt is one parsed SELECT.
type stmt struct {
	items    []selectItem
	from     tableExpr // nil = FROM-less
	where    expr
	order    []orderItem
	limit    expr
	offset   expr
	distinct bool
	maxParam int
	// union holds UNION / UNION ALL branches after the first select body
	// (psql's describe queries end with a UNION'd publications probe); the
	// first body owns the output shape, ORDER BY and LIMIT span the union.
	union []unionBranch
}

type unionBranch struct {
	st  *stmt
	all bool
}

type selectItem struct {
	e    expr
	name string // output column name
	star bool   // bare or qualified star: e is nil (tbl holds the qualifier)
	tbl  string
}

type orderItem struct {
	e    expr
	desc bool
}

// tableExpr is a FROM clause: a single reference or a join tree.
type tableExpr interface{}

type tableRef struct {
	schema string // lower-cased; pg_catalog folds away at lookup
	name   string
	alias  string
}

type joinExpr struct {
	left, right tableExpr
	kind        string // "join", "left", "right", "full", "cross"
	on          expr   // nil for cross
}

// parser walks the token stream.
type parser struct {
	toks     []token
	i        int
	maxParam int // highest $n seen, for the wire's parameter count
}

func parseSQL(src string) (*stmt, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	st, err := p.parseSelect()
	if err != nil {
		return nil, err
	}
	st.maxParam = p.maxParam
	// optional trailing semicolon, then end of input
	if p.peek().kind == tkOp && p.peek().text == ";" {
		p.next()
	}
	if p.peek().kind != tkEOF {
		return nil, errf(p.peek().pos, "unexpected content after the statement")
	}
	return st, nil
}

func (p *parser) peek() token  { return p.toks[p.i] }
func (p *parser) next() token  { t := p.toks[p.i]; p.i++; return t }

func (p *parser) isKeyword(word string) bool {
	t := p.peek()
	return t.kind == tkKeyword && t.text == word
}

func (p *parser) isOp(op string) bool {
	t := p.peek()
	return t.kind == tkOp && t.text == op
}

func (p *parser) acceptKeyword(word string) bool {
	if p.isKeyword(word) {
		p.next()
		return true
	}
	return false
}

func (p *parser) acceptOp(op string) bool {
	if p.isOp(op) {
		p.next()
		return true
	}
	return false
}

func (p *parser) expectOp(op string) error {
	if !p.acceptOp(op) {
		return errf(p.peek().pos, "expected %q, got [%s]", op, tokenText(p.peek()))
	}
	return nil
}

func tokenText(t token) string {
	if t.kind == tkEOF {
		return "end of statement"
	}
	return t.text
}

func (p *parser) parseSelect() (*stmt, error) {
	st, err := p.parseSelectCore()
	if err != nil {
		return nil, err
	}
	st.maxParam = p.maxParam

	for p.acceptKeyword("UNION") {
		all := p.acceptKeyword("ALL")
		branch, err := p.parseSelectCore()
		if err != nil {
			return nil, err
		}
		st.union = append(st.union, unionBranch{st: branch, all: all})
	}

	if p.acceptKeyword("ORDER") {
		if err := p.expectKeyword("BY"); err != nil {
			return nil, err
		}
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			term := orderItem{e: e}
			if p.acceptKeyword("DESC") {
				term.desc = true
			} else {
				p.acceptKeyword("ASC")
			}
			st.order = append(st.order, term)
			if !p.acceptOp(",") {
				break
			}
		}
	}

	if p.acceptKeyword("LIMIT") {
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		st.limit = e
	}
	if p.acceptKeyword("OFFSET") {
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		st.offset = e
	}

	if p.isKeyword("FOR") || p.isKeyword("FETCH") || p.isKeyword("INTO") {
		return nil, capErrf("catalog queries do not support %s", strings.ToLower(p.peek().text))
	}

	return st, nil
}

// parseSelectCore parses one select body: the select list, FROM and WHERE —
// everything above ORDER BY, which spans a UNION at the statement level.
func (p *parser) parseSelectCore() (*stmt, error) {
	if !p.acceptKeyword("SELECT") {
		return nil, errf(p.peek().pos, "expected SELECT, got [%s]", tokenText(p.peek()))
	}
	st := &stmt{}
	st.distinct = p.acceptKeyword("DISTINCT")

	for {
		item, err := p.parseSelectItem()
		if err != nil {
			return nil, err
		}
		st.items = append(st.items, item)
		if !p.acceptOp(",") {
			break
		}
	}

	if p.acceptKeyword("FROM") {
		from, err := p.parseFrom()
		if err != nil {
			return nil, err
		}
		st.from = from
	}

	if p.acceptKeyword("WHERE") {
		w, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		st.where = w
	}

	if p.isKeyword("GROUP") || p.isKeyword("HAVING") || p.isKeyword("INTERSECT") ||
		p.isKeyword("EXCEPT") || p.isKeyword("WINDOW") {
		return nil, capErrf("catalog queries do not support %s", strings.ToLower(p.peek().text))
	}

	return st, nil
}

func (p *parser) expectKeyword(word string) error {
	if !p.acceptKeyword(word) {
		return errf(p.peek().pos, "expected %s, got [%s]", word, tokenText(p.peek()))
	}
	return nil
}

func (p *parser) parseSelectItem() (selectItem, error) {
	// bare or qualified star
	if p.isOp("*") {
		p.next()
		return selectItem{star: true}, nil
	}
	if p.peek().kind == tkIdent && p.toks[p.i+1].kind == tkOp && p.toks[p.i+1].text == "." &&
		p.toks[p.i+2].kind == tkOp && p.toks[p.i+2].text == "*" {
		tbl := foldIdent(p.next())
		p.next() // .
		p.next() // *
		return selectItem{star: true, tbl: tbl}, nil
	}

	e, err := p.parseExpr()
	if err != nil {
		return selectItem{}, err
	}
	item := selectItem{e: e, name: derivedName(e)}
	if p.acceptKeyword("AS") {
		t := p.next()
		if t.kind != tkIdent {
			return selectItem{}, errf(t.pos, "expected alias after AS, got [%s]", tokenText(t))
		}
		// unquoted aliases fold to lower case (PG semantics); a quoted alias
		// keeps its written case
		item.name = foldIdent(t)
	} else if p.peek().kind == tkIdent {
		item.name = foldIdent(p.next())
	}
	return item, nil
}

// derivedName is the output name of an unaliased select item: the column's
// name for plain references, otherwise the PostgreSQL ?column? placeholder.
func derivedName(e expr) string {
	switch x := e.(type) {
	case colRef:
		return x.name
	case funcCall:
		return x.name
	}
	return "?column?"
}

// foldIdent lower-cases a bare identifier, keeps a quoted one as written.
func foldIdent(t token) string {
	if t.quoted {
		return t.text
	}
	return strings.ToLower(t.text)
}

func (p *parser) parseFrom() (tableExpr, error) {
	first, err := p.parseTableRef()
	if err != nil {
		return nil, err
	}
	var left tableExpr = first
	for {
		switch {
		case p.isOp(","):
			p.next()
			right, err := p.parseTableRef()
			if err != nil {
				return nil, err
			}
			left = joinExpr{left: left, right: right, kind: "cross"}
		case p.isKeyword("JOIN"), p.isKeyword("INNER"), p.isKeyword("LEFT"),
			p.isKeyword("RIGHT"), p.isKeyword("FULL"), p.isKeyword("CROSS"):
			kind := "join"
			switch {
			case p.acceptKeyword("CROSS"):
				kind = "cross"
			case p.acceptKeyword("LEFT"):
				kind = "left"
				p.acceptKeyword("OUTER")
			case p.acceptKeyword("RIGHT"):
				kind = "right"
				p.acceptKeyword("OUTER")
			case p.acceptKeyword("FULL"):
				kind = "full"
				p.acceptKeyword("OUTER")
			case p.acceptKeyword("INNER"):
			}
			if err := p.expectKeyword("JOIN"); err != nil {
				return nil, err
			}
			right, err := p.parseTableRef()
			if err != nil {
				return nil, err
			}
			j := joinExpr{left: left, right: right, kind: kind}
			if kind != "cross" {
				if err := p.expectKeyword("ON"); err != nil {
					return nil, err
				}
				on, err := p.parseExpr()
				if err != nil {
					return nil, err
				}
				j.on = on
			}
			left = j
		default:
			return left, nil
		}
	}
}

func (p *parser) parseTableRef() (tableRef, error) {
	t := p.peek()
	if t.kind != tkIdent {
		return tableRef{}, errf(t.pos, "expected a table name, got [%s]", tokenText(t))
	}
	first := foldIdent(p.next())
	ref := tableRef{}
	if p.isOp(".") {
		p.next()
		second := p.peek()
		if second.kind != tkIdent {
			return tableRef{}, errf(second.pos, "expected a table name after '.', got [%s]", tokenText(second))
		}
		ref.schema = first
		ref.name = foldIdent(p.next())
	} else {
		ref.name = first
	}
	if p.acceptKeyword("AS") {
		a := p.next()
		if a.kind != tkIdent {
			return tableRef{}, errf(a.pos, "expected alias after AS, got [%s]", tokenText(a))
		}
		ref.alias = foldIdent(a)
	} else if p.peek().kind == tkIdent {
		// implicit alias — only when it cannot start a new clause
		switch p.peek().text {
		case "JOIN", "INNER", "LEFT", "RIGHT", "FULL", "CROSS", "ON", "WHERE",
			"ORDER", "LIMIT", "OFFSET", "GROUP", "HAVING", "UNION":
		default:
			ref.alias = foldIdent(p.next())
		}
	}
	if ref.alias == "" {
		ref.alias = ref.name
	}
	return ref, nil
}

// parseExpr parses the OR level; deeper levels chain from here.
func (p *parser) parseExpr() (expr, error) { return p.parseOr() }

func (p *parser) parseOr() (expr, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.acceptKeyword("OR") {
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = binExpr{op: "OR", l: left, r: right}
	}
	return left, nil
}

func (p *parser) parseAnd() (expr, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.acceptKeyword("AND") {
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		left = binExpr{op: "AND", l: left, r: right}
	}
	return left, nil
}

func (p *parser) parseNot() (expr, error) {
	if p.acceptKeyword("NOT") {
		e, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return unaryExpr{op: "NOT", e: e}, nil
	}
	return p.parseComparison()
}

type unaryExpr struct {
	op string
	e  expr
}

func (p *parser) parseComparison() (expr, error) {
	left, err := p.parseAdditive()
	if err != nil {
		return nil, err
	}
	for {
		// COLLATE is a postfix no-op here (lyrebird owns no collations);
		// clients attach it to operands in WHERE and ORDER BY
		if p.acceptKeyword("COLLATE") {
			if err := p.skipQualifiedName(); err != nil {
				return nil, err
			}
			continue
		}
		// OPERATOR(pg_catalog.~) — the schema-qualified operator wrapper
		// psql's describe queries use for regex matches
		if left2, ok, err := p.parseOperatorWrapper(left); err != nil {
			return nil, err
		} else if ok {
			left = left2
			continue
		}
		switch {
		case p.isOp("=") || p.isOp("<>") || p.isOp("!=") || p.isOp("<") ||
			p.isOp(">") || p.isOp("<=") || p.isOp(">=") ||
			p.isOp("~") || p.isOp("!~") || p.isOp("~*") || p.isOp("!~*"):
			op := p.next().text
			quantifier := ""
			if p.acceptKeyword("ANY") || p.acceptKeyword("ALL") {
				// SOME/ANY/ALL (expr): the right side becomes the array
				quantifier = "any"
				if err := p.expectOp("("); err != nil {
					return nil, err
				}
				var err error
				right, err := p.parseExpr()
				if err != nil {
					return nil, err
				}
				if err := p.expectOp(")"); err != nil {
					return nil, err
				}
				left = binExpr{op: op, l: left, r: right, anyList: true}
				_ = quantifier
				continue
			}
			right, err := p.parseAdditive()
			if err != nil {
				return nil, err
			}
			if op == "!=" {
				op = "<>"
			}
			left = binExpr{op: op, l: left, r: right}
		case p.isKeyword("LIKE") || p.isKeyword("ILIKE"):
			op := strings.ToLower(p.next().text)
			right, err := p.parseAdditive()
			if err != nil {
				return nil, err
			}
			left = binExpr{op: op, l: left, r: right}
		case p.isKeyword("NOT") && p.toks[p.i+1].kind == tkKeyword &&
			(p.toks[p.i+1].text == "LIKE" || p.toks[p.i+1].text == "ILIKE" ||
				p.toks[p.i+1].text == "IN" || p.toks[p.i+1].text == "BETWEEN"):
			p.next() // NOT
			op := strings.ToLower(p.next().text)
			switch op {
			case "like", "ilike":
				right, err := p.parseAdditive()
				if err != nil {
					return nil, err
				}
				left = binExpr{op: op, l: left, r: right, not: true}
			case "in":
				list, err := p.parseParenList()
				if err != nil {
					return nil, err
				}
				left = inExpr{e: left, list: list, not: true}
			case "between":
				right, err := p.parseBetweenTail()
				if err != nil {
					return nil, err
				}
				left = binExpr{op: "BETWEEN", l: left, r: right, not: true}
			}
		case p.isKeyword("IN"):
			p.next()
			list, err := p.parseParenList()
			if err != nil {
				return nil, err
			}
			left = inExpr{e: left, list: list}
		case p.isKeyword("BETWEEN"):
			p.next()
			right, err := p.parseBetweenTail()
			if err != nil {
				return nil, err
			}
			left = binExpr{op: "BETWEEN", l: left, r: right}
		case p.isKeyword("IS"):
			p.next()
			not := p.acceptKeyword("NOT")
			if p.acceptKeyword("NULL") {
				left = isNullExpr{e: left, not: not}
				continue
			}
			if p.acceptKeyword("TRUE") || p.acceptKeyword("FALSE") {
				// IS TRUE / IS FALSE — treat NULL-strictness edge as identity
				continue
			}
			return nil, errf(p.peek().pos, "unsupported IS form: [%s]", tokenText(p.peek()))
		default:
			return left, nil
		}
	}
}

// skipQualifiedName consumes ident(.ident)* — a COLLATE source's tail.
func (p *parser) skipQualifiedName() error {
	t := p.peek()
	if t.kind != tkIdent {
		return errf(t.pos, "expected a name, got [%s]", tokenText(t))
	}
	p.next()
	for p.isOp(".") {
		p.next()
		t := p.peek()
		if t.kind != tkIdent {
			return errf(t.pos, "expected a name after '.', got [%s]", tokenText(t))
		}
		p.next()
	}
	return nil
}

// parseOperatorWrapper consumes OPERATOR(schema.op) in infix position and
// re-enters the comparison chain with it as the operator. ok is false when
// the next tokens are not an OPERATOR wrapper.
func (p *parser) parseOperatorWrapper(left expr) (expr, bool, error) {
	if p.peek().kind != tkIdent || !strings.EqualFold(p.peek().text, "OPERATOR") {
		return nil, false, nil
	}
	if p.toks[p.i+1].kind != tkOp || p.toks[p.i+1].text != "(" {
		return nil, false, nil
	}
	p.next() // OPERATOR
	p.next() // (
	var parts []string
	for {
		t := p.peek()
		if t.kind == tkOp && t.text == ")" {
			p.next()
			break
		}
		if t.kind == tkEOF {
			return nil, false, errf(t.pos, "unterminated OPERATOR(…)")
		}
		parts = append(parts, t.text)
		p.next()
	}
	op := parts[len(parts)-1] // the operator itself: pg_catalog.~ → ~
	switch op {
	case "=", "<", ">", "<=", ">=", "<>", "~", "!~", "~*", "!~*":
	default:
		return nil, false, capErrf("operator %q is not available in catalog queries", op)
	}
	right, err := p.parseAdditive()
	if err != nil {
		return nil, false, err
	}
	return binExpr{op: op, l: left, r: right}, true, nil
}

func (p *parser) parseBetweenTail() (expr, error) {
	lo, err := p.parseAdditive()
	if err != nil {
		return nil, err
	}
	if err := p.expectKeyword("AND"); err != nil {
		return nil, err
	}
	hi, err := p.parseAdditive()
	if err != nil {
		return nil, err
	}
	return betweenTail{lo: lo, hi: hi}, nil
}

type betweenTail struct{ lo, hi expr }

func (p *parser) parseParenList() ([]expr, error) {
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	var list []expr
	for {
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		list = append(list, e)
		if p.acceptOp(",") {
			continue
		}
		break
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	return list, nil
}

// parseAdditive handles +, - and string concatenation (||).
func (p *parser) parseAdditive() (expr, error) {
	left, err := p.parseMultiplicative()
	if err != nil {
		return nil, err
	}
	for p.isOp("+") || p.isOp("-") || p.isOp("||") {
		op := p.next().text
		right, err := p.parseMultiplicative()
		if err != nil {
			return nil, err
		}
		left = binExpr{op: op, l: left, r: right}
	}
	return left, nil
}

func (p *parser) parseMultiplicative() (expr, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for p.isOp("*") || p.isOp("/") {
		op := p.next().text
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = binExpr{op: op, l: left, r: right}
	}
	return left, nil
}

func (p *parser) parseUnary() (expr, error) {
	if p.isOp("-") {
		p.next()
		e, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return unaryExpr{op: "NEG", e: e}, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (expr, error) {
	t := p.peek()
	switch {
	case t.kind == tkNumber:
		p.next()
		v, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return nil, errf(t.pos, "invalid number [%s]", t.text)
		}
		return p.postfix(literal{v: v})
	case t.kind == tkString:
		p.next()
		return p.postfix(literal{v: t.text})
	case t.kind == tkParam:
		p.next()
		n, err := strconv.Atoi(strings.TrimPrefix(t.text, "$"))
		if err != nil {
			return nil, errf(t.pos, "invalid parameter [%s]", t.text)
		}
		if n > p.maxParam {
			p.maxParam = n
		}
		return p.postfix(paramRef{n: n})
	case t.kind == tkKeyword && t.text == "NULL":
		p.next()
		return p.postfix(literal{v: nil})
	case t.kind == tkKeyword && t.text == "TRUE":
		p.next()
		return p.postfix(literal{v: true})
	case t.kind == tkKeyword && t.text == "FALSE":
		p.next()
		return p.postfix(literal{v: false})
	case t.kind == tkKeyword && t.text == "CASE":
		return p.parseCase()
	case t.kind == tkKeyword && t.text == "CAST":
		return p.parseCast()
	case t.kind == tkKeyword && t.text == "EXISTS":
		return nil, capErrf("catalog queries do not support EXISTS")
	case t.kind == tkIdent && strings.EqualFold(t.text, "ARRAY") &&
		p.toks[p.i+1].kind == tkOp && p.toks[p.i+1].text == "(" &&
		p.toks[p.i+2].kind == tkKeyword && p.toks[p.i+2].text == "SELECT":
		// ARRAY(SELECT …): the first output column of every subquery row
		p.next()
		p.next()
		sub, err := p.parseSelect()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		return p.postfix(arraySubqueryExpr{st: sub})
	case t.kind == tkOp && t.text == "(":
		p.next()
		// scalar subquery: (SELECT …) evaluated per outer row
		if p.isKeyword("SELECT") {
			sub, err := p.parseSelect()
			if err != nil {
				return nil, err
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			return p.postfix(subqueryExpr{st: sub})
		}
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		return p.postfix(e)
	case t.kind == tkIdent:
		return p.parseIdentExpr()
	default:
		return nil, errf(t.pos, "unexpected token [%s]", tokenText(t))
	}
}

// parseIdentExpr handles plain identifiers, qualified names, function calls
// and pseudo-variable keywords (current_user etc. lex as identifiers).
func (p *parser) parseIdentExpr() (expr, error) {
	parts := []string{foldIdent(p.next())}
	for p.isOp(".") {
		p.next()
		t := p.peek()
		if t.kind != tkIdent {
			return nil, errf(t.pos, "expected a name after '.', got [%s]", tokenText(t))
		}
		parts = append(parts, foldIdent(p.next()))
	}

	if p.isOp("(") {
		// function call: the name is the dotted path joined back together
		p.next()
		var args []expr
		if !p.isOp(")") {
			parsed, err := p.parseFunctionArgs(strings.Join(parts, "."))
			if err != nil {
				return nil, err
			}
			args = parsed
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		return p.postfix(funcCall{name: strings.Join(parts, "."), args: args})
	}

	if len(parts) == 1 {
		return p.postfix(colRef{name: parts[0]})
	}
	return p.postfix(colRef{tbl: parts[len(parts)-2], name: parts[len(parts)-1]})
}

// parseFunctionArgs parses a call's argument list. SUBSTRING's keyword
// argument forms fold into positional shape so the whitelist sees one
// spelling: SUBSTRING(x FROM start [FOR len]) → substring(x, start[, len]),
// SUBSTRING(x FOR len) → substring(x, 1, len).
func (p *parser) parseFunctionArgs(name string) ([]expr, error) {
	var args []expr
	arg, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	args = append(args, arg)
	if p.acceptKeyword("FROM") {
		start, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		args = append(args, start)
		if p.acceptKeyword("FOR") {
			length, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			args = append(args, length)
		}
		return args, nil
	}
	if p.acceptKeyword("FOR") {
		length, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		return append(args, literal{v: float64(1)}, length), nil
	}
	for p.acceptOp(",") {
		// DISTINCT inside arg lists is accepted and ignored — the whitelist
		// rejects aggregates anyway.
		p.acceptKeyword("DISTINCT")
		arg, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		args = append(args, arg)
	}
	return args, nil
}

// parseCase handles both CASE forms: searched (CASE WHEN cond THEN …) and
// simple (CASE operand WHEN value THEN …).
func (p *parser) parseCase() (expr, error) {
	p.next() // CASE
	ce := caseExpr{}
	if !p.isKeyword("WHEN") {
		operand, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		ce.operand = operand
	}
	for p.acceptKeyword("WHEN") {
		cond, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if err := p.expectKeyword("THEN"); err != nil {
			return nil, err
		}
		then, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		ce.whens = append(ce.whens, whenClause{cond: cond, then: then})
	}
	if len(ce.whens) == 0 {
		return nil, errf(p.peek().pos, "CASE requires at least one WHEN")
	}
	if p.acceptKeyword("ELSE") {
		elseE, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		ce.elseE = elseE
	}
	if err := p.expectKeyword("END"); err != nil {
		return nil, err
	}
	return p.postfix(ce)
}

// parseCast accepts CAST(expr AS type) and evaluates as a best-effort
// coercion (identity for the text family, numeric parse for numbers).
func (p *parser) parseCast() (expr, error) {
	p.next() // CAST
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	e, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if err := p.expectKeyword("AS"); err != nil {
		return nil, err
	}
	target, err := p.parseTypeName()
	if err != nil {
		return nil, err
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	return p.postfix(castExpr{e: e, target: target})
}

// parseTypeName reads a (possibly schema-qualified) type name and swallows
// its modifiers: pg_catalog.varchar(64), numeric(10,2), timestamp(3) with
// time zone. The target is the bare type (last segment), lower-cased.
func (p *parser) parseTypeName() (string, error) {
	t := p.peek()
	if t.kind != tkIdent {
		return "", errf(t.pos, "expected a type name, got [%s]", tokenText(t))
	}
	target := foldIdent(p.next())
	for p.isOp(".") {
		p.next()
		t := p.peek()
		if t.kind != tkIdent {
			return "", errf(t.pos, "expected a type name after '.', got [%s]", tokenText(t))
		}
		target = foldIdent(p.next())
	}
	// swallow type parameters and trailing words: varchar(64),
	// timestamp(3) with time zone, double precision
	if p.acceptOp("(") {
		depth := 1
		for depth > 0 && p.peek().kind != tkEOF {
			switch p.next().text {
			case "(":
				depth++
			case ")":
				depth--
			}
		}
	}
	for p.peek().kind == tkIdent {
		switch strings.ToLower(p.peek().text) {
		case "time", "zone", "with", "without", "precision":
			p.next()
		default:
			return "", errf(p.peek().pos, "unexpected token in type name: [%s]", p.peek().text)
		}
	}
	return target, nil
}

// postfix applies trailing operators: subscripts (current_schemas(true)[1])
// and casts (x::text, 'pg_class'::regclass).
func (p *parser) postfix(e expr) (expr, error) {
	for {
		switch {
		case p.isOp("["):
			p.next()
			idx, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if err := p.expectOp("]"); err != nil {
				return nil, err
			}
			e = subscriptExpr{base: e, idx: idx}
		case p.isOp("::"):
			p.next()
			target, err := p.parseTypeName()
			if err != nil {
				return nil, err
			}
			e = castExpr{e: e, target: target}
		default:
			return e, nil
		}
	}
}
