package translate

import "testing"

func TestRender(t *testing.T) {
	tests := []struct {
		name string
		expr Expr
		want string
	}{
		{
			name: "nil is match everything",
			expr: nil,
			want: "",
		},
		{
			name: "compare",
			expr: Compare{Op: Ge, Field: "views", Value: IntValue(10)},
			want: "views >= 10",
		},
		{
			name: "in list",
			expr: InList{Field: "status", Values: []Value{StringValue("a"), IntValue(1)}},
			want: `status in ["a", 1]`,
		},
		{
			name: "text match",
			expr: TextMatch{Field: "title", Query: "quick brown"},
			want: `TEXT_MATCH(title, "quick brown")`,
		},
		{
			name: "text match escapes",
			expr: TextMatch{Field: "title", Query: `a"b\c`},
			want: `TEXT_MATCH(title, "a\"b\\c")`,
		},
		{
			name: "not null",
			expr: NotNull{Field: "v"},
			want: "v is not null",
		},
		{
			name: "not",
			expr: Not{Child: Compare{Op: Eq, Field: "s", Value: StringValue("x")}},
			want: `not (s == "x")`,
		},
		{
			name: "and parenthesizes every member",
			expr: And{Children: []Expr{
				Compare{Op: Eq, Field: "a", Value: IntValue(1)},
				Compare{Op: Lt, Field: "b", Value: IntValue(2)},
			}},
			want: "(a == 1) and (b < 2)",
		},
		{
			name: "single-child and passes through",
			expr: And{Children: []Expr{Compare{Op: Eq, Field: "a", Value: IntValue(1)}}},
			want: "a == 1",
		},
		{
			name: "empty and is match everything",
			expr: And{},
			want: "",
		},
		{
			name: "or parenthesizes every member",
			expr: Or{Children: []Expr{
				Compare{Op: Eq, Field: "a", Value: IntValue(1)},
				Compare{Op: Eq, Field: "b", Value: IntValue(2)},
			}},
			want: "(a == 1) or (b == 2)",
		},
		{
			name: "nested groups keep their parens under a join",
			expr: And{Children: []Expr{
				Or{Children: []Expr{
					Compare{Op: Eq, Field: "a", Value: IntValue(1)},
					Compare{Op: Eq, Field: "b", Value: IntValue(2)},
				}},
				Not{Child: Compare{Op: Eq, Field: "c", Value: IntValue(3)}},
			}},
			want: "((a == 1) or (b == 2)) and (not (c == 3))",
		},
		{
			name: "never renders the constant-false guard",
			expr: Never{},
			want: "1 != 1",
		},
		{
			name: "float literal",
			expr: Compare{Op: Gt, Field: "price", Value: FloatValue(1.5)},
			want: "price > 1.5",
		},
		{
			name: "bool literal",
			expr: Compare{Op: Ne, Field: "active", Value: BoolValue(false)},
			want: "active != false",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Render(tt.expr)
			if err != nil {
				t.Fatalf("Render() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("Render() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRenderErrors(t *testing.T) {
	tests := []struct {
		name string
		expr Expr
	}{
		{
			name: "field with embedded quote",
			expr: Compare{Op: Eq, Field: `a"b`, Value: IntValue(1)},
		},
		{
			name: "field with whitespace",
			expr: Compare{Op: Eq, Field: "a b", Value: IntValue(1)},
		},
		{
			name: "empty field",
			expr: Compare{Op: Eq, Field: "", Value: IntValue(1)},
		},
		{
			name: "field starting with a digit",
			expr: Compare{Op: Eq, Field: "9a", Value: IntValue(1)},
		},
		{
			name: "unknown comparison operator",
			expr: Compare{Op: "~", Field: "a", Value: IntValue(1)},
		},
		{
			name: "literal without a kind",
			expr: Compare{Op: Eq, Field: "a", Value: Value{}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := Render(tt.expr); err == nil {
				t.Fatalf("Render() = %q, want an error", got)
			}
		})
	}
}
