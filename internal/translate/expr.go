package translate

import (
	"fmt"
	"strconv"
	"strings"
)

// Expr is one node of the Milvus boolean-expression dialect, held as a tree:
// the contract between translators and store is the tree, not a rendered
// string, so consumers (field renaming in Phase 3, scoring rewrites in
// Phase 5) can transform plans without parsing text. Both translators (es, pg)
// emit the same dialect; Render turns it into the string Milvus executes.
//
// A nil Expr means "match everything".
type Expr interface{ isExpr() }

type (
	// Compare is `Field op Value` with op one of the CompareOp constants.
	Compare struct {
		Op    CompareOp
		Field string
		Value Value
	}
	// InList is `Field in [v1, v2, ...]`.
	InList struct {
		Field  string
		Values []Value
	}
	// TextMatch is the analyzed-text token match: TEXT_MATCH(Field, Query).
	// Query holds space-separated terms, already tokenized by the translator.
	TextMatch struct {
		Field string
		Query string
	}
	// NotNull is the nullable-field existence check: Field is not null.
	NotNull struct{ Field string }
	// Not is logical negation.
	Not struct{ Child Expr }
	// And is an n-ary conjunction. One child behaves as the child itself;
	// zero children render as "match everything".
	And struct{ Children []Expr }
	// Or is an n-ary disjunction, same degenerate cases as And.
	Or struct{ Children []Expr }
	// Never is the constant-false filter. Render emits the `1 != 1` guard —
	// isolated here so the workaround has exactly one home while Milvus
	// acceptance of bare constant expressions is unverified.
	Never struct{}
)

func (Compare) isExpr()   {}
func (InList) isExpr()    {}
func (TextMatch) isExpr() {}
func (NotNull) isExpr()   {}
func (Not) isExpr()       {}
func (And) isExpr()       {}
func (Or) isExpr()        {}
func (Never) isExpr()     {}

// Compile-time check that every node implements Expr.
var _ = []Expr{Compare{}, InList{}, TextMatch{}, NotNull{}, Not{}, And{}, Or{}, Never{}}

// CompareOp is a Milvus comparison operator; the rendered form is the value.
type CompareOp string

const (
	Eq CompareOp = "=="
	Ne CompareOp = "!="
	Gt CompareOp = ">"
	Ge CompareOp = ">="
	Lt CompareOp = "<"
	Le CompareOp = "<="
)

var compareOps = map[CompareOp]bool{Eq: true, Ne: true, Gt: true, Ge: true, Lt: true, Le: true}

// ValueKind discriminates Value; the zero kind is invalid so a half-built
// Value fails at render instead of leaking into the expression.
type ValueKind int

const (
	KindInvalid ValueKind = iota
	KindString
	KindInt
	KindFloat
	KindBool
)

// Value is a typed literal. Translators resolve protocol-side coercions
// (date strings to epoch millis, numeric strings, ...) so a Value entering
// the tree is already in its storage encoding; Render only adds syntax.
type Value struct {
	Kind  ValueKind
	Str   string
	Int   int64
	Float float64
	Bool  bool
}

func StringValue(s string) Value { return Value{Kind: KindString, Str: s} }
func IntValue(i int64) Value     { return Value{Kind: KindInt, Int: i} }
func FloatValue(f float64) Value { return Value{Kind: KindFloat, Float: f} }
func BoolValue(b bool) Value     { return Value{Kind: KindBool, Bool: b} }

// Render renders expr as a Milvus boolean expression. nil renders as ""
// (match everything). Field names are validated here — the one place the
// dialect reaches text — so a client-supplied field cannot splice operators
// into the expression.
func Render(expr Expr) (string, error) {
	var b strings.Builder
	if err := renderExpr(&b, expr); err != nil {
		return "", err
	}
	return b.String(), nil
}

func renderExpr(b *strings.Builder, expr Expr) error {
	switch e := expr.(type) {
	case nil:
		return nil
	case Compare:
		if !compareOps[e.Op] {
			return fmt.Errorf("invalid comparison operator %q", string(e.Op))
		}
		field, err := renderField(e.Field)
		if err != nil {
			return err
		}
		v, err := renderLiteral(e.Value)
		if err != nil {
			return err
		}
		b.WriteString(field + " " + string(e.Op) + " " + v)
	case InList:
		field, err := renderField(e.Field)
		if err != nil {
			return err
		}
		vals := make([]string, len(e.Values))
		for i, v := range e.Values {
			if vals[i], err = renderLiteral(v); err != nil {
				return err
			}
		}
		b.WriteString(field + " in [" + strings.Join(vals, ", ") + "]")
	case TextMatch:
		field, err := renderField(e.Field)
		if err != nil {
			return err
		}
		b.WriteString("TEXT_MATCH(" + field + ", " + quoteLiteral(e.Query) + ")")
	case NotNull:
		field, err := renderField(e.Field)
		if err != nil {
			return err
		}
		b.WriteString(field + " is not null")
	case Not:
		b.WriteString("not (")
		if err := renderExpr(b, e.Child); err != nil {
			return err
		}
		b.WriteByte(')')
	case And:
		return renderJoin(b, e.Children, " and ")
	case Or:
		return renderJoin(b, e.Children, " or ")
	case Never:
		b.WriteString("1 != 1")
	default:
		return fmt.Errorf("unknown expression node %T", expr)
	}
	return nil
}

// renderJoin combines sub-expressions; every member of a multi-item join is
// parenthesized so nested and/or groups cannot change meaning, while a single
// item passes through unwrapped to keep simple queries readable.
func renderJoin(b *strings.Builder, children []Expr, op string) error {
	switch len(children) {
	case 0:
		return nil
	case 1:
		return renderExpr(b, children[0])
	}
	for i, c := range children {
		if i > 0 {
			b.WriteString(op)
		}
		b.WriteByte('(')
		if err := renderExpr(b, c); err != nil {
			return err
		}
		b.WriteByte(')')
	}
	return nil
}

// renderField validates a field reference before it enters the expression:
// identifiers are [A-Za-z_][A-Za-z0-9_]*. Nested-path syntax needs verifying
// against Milvus before being allowed through; widen there, not here.
func renderField(f string) (string, error) {
	if f == "" {
		return "", fmt.Errorf("empty field name")
	}
	for i, r := range f {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return "", fmt.Errorf("invalid field name %q: must match [A-Za-z_][A-Za-z0-9_]*", f)
		}
	}
	return f, nil
}

func renderLiteral(v Value) (string, error) {
	switch v.Kind {
	case KindString:
		return quoteLiteral(v.Str), nil
	case KindInt:
		return strconv.FormatInt(v.Int, 10), nil
	case KindFloat:
		return strconv.FormatFloat(v.Float, 'f', -1, 64), nil
	case KindBool:
		return strconv.FormatBool(v.Bool), nil
	default:
		return "", fmt.Errorf("literal has no kind set (got %d)", v.Kind)
	}
}

var literalEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

func quoteLiteral(s string) string { return `"` + literalEscaper.Replace(s) + `"` }
