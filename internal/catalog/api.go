package catalog

import (
	"context"
	"strings"
)

// Prepare parses and validates a catalog query without touching the store:
// the output shape comes from the static table definitions, rows come at
// Exec time from a Provider snapshot.
func Prepare(query string) (*Statement, error) {
	st, err := parseSQL(query)
	if err != nil {
		return nil, err
	}

	sources := []sourceSpec{}
	if st.from != nil {
		sources = flattenFrom(st.from)
	}
	bound := make([]boundDef, len(sources))
	for i, src := range sources {
		def, ok := tableRegistry[src.ref.name]
		if !ok {
			return nil, &Error{Type: "42P01", Reason: "relation \"" + src.ref.name + "\" does not exist"}
		}
		// RIGHT/FULL joins cannot extend the left-deep combination order —
		// catalog clients use LEFT JOIN exclusively; fail loudly otherwise.
		if src.kind == "right" || src.kind == "full" {
			return nil, capErrf("catalog queries support [LEFT] JOIN, not %s JOIN", strings.ToUpper(src.kind))
		}
		bound[i] = boundDef{spec: src, def: def}
	}

	if result, ok := matchSpecial(st, bound); ok {
		return &Statement{cols: result.cols, st: st, sources: sources, defs: defsOf(bound), special: result}, nil
	}

	// function whitelist + output shape, both resolved from the static
	// definitions so failures surface as 42883/42601 before any provider call
	for i := range sources {
		if sources[i].on != nil {
			if err := validateFunctions(sources[i].on); err != nil {
				return nil, err
			}
		}
	}
	for _, item := range st.items {
		if !item.star {
			if err := validateFunctions(item.e); err != nil {
				return nil, err
			}
		}
	}
	if st.where != nil {
		if err := validateFunctions(st.where); err != nil {
			return nil, err
		}
	}
	for _, term := range st.order {
		if err := validateFunctions(term.e); err != nil {
			return nil, err
		}
	}

	cols, err := inferColumns(st, bound)
	if err != nil {
		return nil, err
	}
	return &Statement{cols: cols, st: st, sources: sources, defs: defsOf(bound)}, nil
}

// ParamCount reports the highest $n referenced, so the wire layer can
// announce the parameter count in ParameterDescription.
func (s *Statement) ParamCount() int { return s.st.maxParam }

// validateFunctions rejects function calls outside the whitelist at prepare
// time, so RowDescription never announces a query the engine cannot run.
func validateFunctions(e expr) error {
	switch x := e.(type) {
	case funcCall:
		if _, ok := lookupFunction(x.name); !ok {
			return &Error{Type: "42883", Reason: "function " + x.name + " is not available in catalog queries"}
		}
		for _, arg := range x.args {
			if err := validateFunctions(arg); err != nil {
				return err
			}
		}
		return nil
	case binExpr:
		if err := validateFunctions(x.l); err != nil {
			return err
		}
		if err := validateFunctions(x.r); err != nil {
			return err
		}
		if tail, ok := x.r.(betweenTail); ok {
			if err := validateFunctions(tail.lo); err != nil {
				return err
			}
			return validateFunctions(tail.hi)
		}
		return nil
	case unaryExpr:
		return validateFunctions(x.e)
	case isNullExpr:
		return validateFunctions(x.e)
	case inExpr:
		if err := validateFunctions(x.e); err != nil {
			return err
		}
		for _, le := range x.list {
			if err := validateFunctions(le); err != nil {
				return err
			}
		}
		return nil
	case caseExpr:
		if x.operand != nil {
			if err := validateFunctions(x.operand); err != nil {
				return err
			}
		}
		for _, w := range x.whens {
			if err := validateFunctions(w.cond); err != nil {
				return err
			}
			if err := validateFunctions(w.then); err != nil {
				return err
			}
		}
		if x.elseE != nil {
			return validateFunctions(x.elseE)
		}
		return nil
	case castExpr:
		return validateFunctions(x.e)
	case subscriptExpr:
		if err := validateFunctions(x.base); err != nil {
			return err
		}
		return validateFunctions(x.idx)
	}
	return nil
}

// boundDef pairs a FROM source with its static table definition.
type boundDef struct {
	spec sourceSpec
	def  *tableDef
}

func defsOf(bound []boundDef) []*tableDef {
	defs := make([]*tableDef, len(bound))
	for i, b := range bound {
		defs[i] = b.def
	}
	return defs
}

// inferColumns derives the output shape: source columns from their table
// definitions, function calls from the whitelist, everything else from the
// expression kind. RowDescription needs this before any provider call.
func inferColumns(st *stmt, bound []boundDef) ([]Col, error) {
	var cols []Col
	for _, item := range st.items {
		if !item.star {
			oid := inferOID(item.e, bound)
			cols = append(cols, Col{Name: item.name, Oid: oid})
			continue
		}
		expanded := false
		for _, b := range bound {
			// qualified stars match the alias as written (c.*), bare stars
			// expand every source in order
			if item.tbl != "" && b.spec.ref.alias != item.tbl {
				continue
			}
			for _, c := range b.def.cols {
				cols = append(cols, Col{Name: c.name, Oid: c.oid})
			}
			expanded = true
		}
		if !expanded {
			if item.tbl != "" {
				return nil, &Error{Type: "42P01", Reason: "relation \"" + item.tbl + "\" does not exist"}
			}
			return nil, &Error{Type: "42601", Reason: "cannot expand *: no FROM tables"}
		}
	}
	return cols, nil
}

func inferOID(e expr, bound []boundDef) uint32 {
	switch x := e.(type) {
	case literal:
		switch x.v.(type) {
		case bool:
			return OIDBool
		case float64:
			return OIDFloat8
		default:
			return OIDText
		}
	case colRef:
		if x.tbl == "" {
			if _, ok := pseudoVars[x.name]; ok {
				return OIDText
			}
			for _, b := range bound {
				if i := b.def.colIndex(x.name); i >= 0 {
					return b.def.cols[i].oid
				}
			}
			return OIDText
		}
		for _, b := range bound {
			if b.spec.ref.alias != x.tbl {
				continue
			}
			if i := b.def.colIndex(x.name); i >= 0 {
				return b.def.cols[i].oid
			}
		}
		return OIDText
	case funcCall:
		if fd, ok := lookupFunction(x.name); ok {
			return fd.ret
		}
		return OIDText
	case binExpr:
		switch x.op {
		case "AND", "OR", "=", "<>", "<", ">", "<=", ">=", "LIKE", "ILIKE",
			"~", "!~", "~*", "!~*", "BETWEEN":
			return OIDBool
		default:
			return OIDFloat8
		}
	case isNullExpr, inExpr:
		return OIDBool
	case caseExpr:
		if len(x.whens) > 0 {
			return inferOID(x.whens[0].then, bound)
		}
		return OIDText
	case castExpr:
		switch x.target {
		case "bool", "boolean":
			return OIDBool
		case "int2", "int4", "int8", "integer", "smallint", "bigint", "oid",
			"float4", "float8", "real", "double precision", "numeric":
			return OIDFloat8
		default:
			return OIDText
		}
	case subscriptExpr:
		return OIDText
	case paramRef:
		return OIDText
	case unaryExpr:
		return OIDFloat8
	}
	return OIDText
}

// Prepareable reports whether the statement is worth a catalog parse: only
// SELECT can be one (session statements and DDL are classified elsewhere;
// WITH/subquery forms are not part of the catalog dialect).
func Prepareable(sql string) bool {
	toks, err := lex(sql)
	if err != nil {
		return false
	}
	return len(toks) > 1 && toks[0].kind == tkKeyword && toks[0].text == "SELECT"
}

// Words tokenizes a statement into its word tokens: keywords upper-cased,
// identifiers as written, literals and operators as text. The session layer
// classifies ack-able statements with it, without the engine.
func Words(sql string) ([]string, error) {
	toks, err := lex(sql)
	if err != nil {
		return nil, err
	}
	words := make([]string, 0, len(toks))
	for _, t := range toks {
		if t.kind == tkEOF {
			break
		}
		words = append(words, t.text)
	}
	return words, nil
}

// ReferencesCatalogTables reports whether the statement text plausibly
// references a virtual catalog table. The dispatcher uses it to decide
// whether a catalog-parse failure belongs to the catalog engine (surface
// its error) or to the collection pipeline (fall through). Lexing is
// lenient: any lex error answers "no".
func ReferencesCatalogTables(sql string) bool {
	toks, err := lex(sql)
	if err != nil {
		return false
	}
	for i, t := range toks {
		if t.kind != tkIdent {
			continue
		}
		switch strings.ToLower(t.text) {
		case "pg_catalog", "information_schema":
			return true
		}
		if i == 0 {
			continue
		}
		prev := toks[i-1]
		refPosition := (prev.kind == tkKeyword && (prev.text == "FROM" || prev.text == "JOIN")) ||
			(prev.kind == tkOp && prev.text == ",")
		if refPosition {
			if _, ok := tableRegistry[strings.ToLower(t.text)]; ok {
				return true
			}
		}
	}
	return false
}

// specialResult is a named special case's execution plan: explicit output
// columns and a runner that generates rows directly, bypassing the general
// engine (see special.go for the registry).
type specialResult struct {
	cols []Col
	run  func(ctx context.Context, snap *snapshot) ([][]any, error)
}
