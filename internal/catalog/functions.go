package catalog

import (
	"fmt"
	"strings"
)

// The scalar-function whitelist. Catalog clients call a fixed vocabulary of
// system functions inside their metadata queries; each entry declares its
// return OID (so RowDescription can be built before execution) and its
// implementation over already-evaluated arguments. Anything outside the
// whitelist fails with 42883 — the honest capability answer.

type funcDef struct {
	ret  uint32
	args int // exact arity, or -1 for varargs
	fn   func(ec *evalCtx, args []any) (any, error)
}

var functionRegistry = map[string]*funcDef{}

func register(name string, ret uint32, args int, fn func(ec *evalCtx, args []any) (any, error)) {
	functionRegistry[name] = &funcDef{ret: ret, args: args, fn: fn}
}

func registerNul(name string, args int) {
	register(name, OIDText, args, func(ec *evalCtx, args []any) (any, error) { return nil, nil })
}

func registerTrue(name string, args int) {
	register(name, OIDBool, args, func(ec *evalCtx, args []any) (any, error) { return true, nil })
}

func init() {
	register("version", OIDText, 0, func(ec *evalCtx, _ []any) (any, error) { return VersionString, nil })
	register("current_database", OIDText, 0, func(ec *evalCtx, _ []any) (any, error) { return ec.snap.db, nil })
	register("current_schema", OIDText, 0, func(ec *evalCtx, _ []any) (any, error) { return "public", nil })
	register("current_schemas", OIDTextArray, 1, func(ec *evalCtx, args []any) (any, error) {
		// with the implicit-schemas flag on, PG prepends pg_catalog to the
		// search path; pgjdbc compares pg_temp names against entry [1], which
		// can never match here — there are no temp schemas.
		if b, ok := args[0].(bool); ok && !b {
			return []any{"public"}, nil
		}
		return []any{"pg_catalog", "public"}, nil
	})
	register("pg_backend_pid", OIDInt4, 0, func(ec *evalCtx, _ []any) (any, error) { return float64(1963), nil })
	register("pg_is_in_recovery", OIDBool, 0, func(ec *evalCtx, _ []any) (any, error) { return false, nil })
	register("pg_postmaster_start_time", OIDTsTz, 0, func(ec *evalCtx, _ []any) (any, error) { return "2026-01-01 00:00:00+00", nil })

	register("current_setting", OIDText, -1, func(ec *evalCtx, args []any) (any, error) {
		if len(args) == 0 {
			return nil, errf(0, "current_setting requires a setting name")
		}
		name, _ := toString(args[0])
		if overlay, ok := ec.snap.settings[name]; ok {
			return overlay, nil
		}
		if v, ok := setting(name); ok {
			return v, nil
		}
		if len(args) > 1 {
			if truth(args[1]) {
				return nil, &Error{Type: "42704", Reason: fmt.Sprintf("unrecognized configuration parameter %q", name)}
			}
			return nil, nil
		}
		return nil, &Error{Type: "42704", Reason: fmt.Sprintf("unrecognized configuration parameter %q", name)}
	})
	register("set_config", OIDText, 3, func(ec *evalCtx, args []any) (any, error) {
		name, _ := toString(args[0])
		value, _ := toString(args[1])
		ec.snap.settings[name] = value
		return value, nil
	})

	register("format_type", OIDText, -1, func(ec *evalCtx, args []any) (any, error) {
		if len(args) == 0 {
			return nil, nil
		}
		oid, _ := toFloat(args[0])
		name := typName(uint32(oid))
		if len(args) > 1 {
			if mod, ok := toFloat(args[1]); ok && mod > 4 && name == "varchar" {
				return fmt.Sprintf("%s(%d)", name, int(mod)-4), nil
			}
		}
		return name, nil
	})
	register("pg_get_userbyid", OIDText, 1, func(ec *evalCtx, _ []any) (any, error) { return "lyrebird", nil })
	register("pg_encoding_to_char", OIDText, 1, func(ec *evalCtx, _ []any) (any, error) { return "UTF8", nil })

	// descriptions: none exist yet
	registerNul("obj_description", -1)
	registerNul("shobj_description", -1)
	registerNul("col_description", 2)

	// visibility and privilege predicates: single-owner, single-schema
	// catalogs answer unconditionally
	registerTrue("pg_table_is_visible", 1)
	registerTrue("pg_type_is_visible", 1)
	registerTrue("pg_database_is_visible", 1)
	registerTrue("pg_function_is_visible", 1)
	registerTrue("pg_operator_is_visible", 1)
	registerTrue("pg_conversion_is_visible", 1)
	registerTrue("pg_ts_config_is_visible", 1)
	registerTrue("has_table_privilege", -1)
	registerTrue("has_any_column_privilege", -1)
	registerTrue("has_column_privilege", -1)
	registerTrue("has_schema_privilege", -1)
	registerTrue("has_database_privilege", -1)
	registerTrue("has_function_privilege", -1)
	registerTrue("has_sequence_privilege", -1)
	registerTrue("pg_column_is_updatable", -1)
	registerTrue("pg_relation_is_updatable", -1)

	// definition decompilers: the underlying expressions (column defaults,
	// index predicates, constraint checks) do not exist in lyrebird's model
	registerNul("pg_get_expr", -1)
	registerNul("pg_get_statisticsobjdef_columns", 1)
	registerNul("pg_get_partkeydef", 1)
	registerNul("pg_get_viewdef", -1)
	registerNul("pg_get_triggerdef", -1)
	register("pg_get_constraintdef", OIDText, -1, func(ec *evalCtx, args []any) (any, error) {
		// the one constraint type the catalog reports is PRIMARY KEY;
		// pg_get_constraintdef renders it from the pg_constraint row —
		// answered as NULL here (psql shows an empty definition) — hmm,
		// keep NULL honest: the constraint's parsed form isn't stored.
		return nil, nil
	})
	registerNul("pg_get_indexdef", -1)
	registerNul("pg_get_viewdef", -1)
	registerNul("pg_get_functiondef", -1)
	registerNul("pg_get_partkeydef", 1)
	registerNul("pg_get_serial_sequence", 2)

	// sizes and storage: the gateway owns no storage numbers
	register("pg_relation_size", OIDFloat8, -1, func(ec *evalCtx, _ []any) (any, error) { return float64(0), nil })
	register("pg_total_relation_size", OIDFloat8, -1, func(ec *evalCtx, _ []any) (any, error) { return float64(0), nil })
	register("pg_table_size", OIDFloat8, 1, func(ec *evalCtx, _ []any) (any, error) { return float64(0), nil })
	register("pg_indexes_size", OIDFloat8, 1, func(ec *evalCtx, _ []any) (any, error) { return float64(0), nil })
	register("pg_database_size", OIDFloat8, -1, func(ec *evalCtx, _ []any) (any, error) { return float64(0), nil })
	registerNul("pg_relation_filenode", 1)
	registerNul("pg_relation_filepath", 1)

	// regclass-style resolvers: nothing to resolve to
	registerNul("to_regclass", 1)
	registerNul("to_regnamespace", 1)
	registerNul("to_regproc", 1)
	registerNul("to_regprocedure", 1)
	registerNul("to_regtype", 1)

	register("quote_ident", OIDText, 1, quoteText)
	register("quote_literal", OIDText, 1, quoteText)
	register("quote_nullable", OIDText, 1, quoteText)
	register("format", OIDText, -1, func(ec *evalCtx, args []any) (any, error) {
		if len(args) == 0 || args[0] == nil {
			return nil, nil
		}
		tpl, _ := toString(args[0])
		rest := args[1:]
		var b strings.Builder
		i := 0
		argIdx := 0
		for i < len(tpl) {
			c := tpl[i]
			if c != '%' {
				b.WriteByte(c)
				i++
				continue
			}
			i++
			if i >= len(tpl) {
				break
			}
			switch tpl[i] {
			case 's':
				if argIdx < len(rest) {
					s, _ := toString(rest[argIdx])
					b.WriteString(s)
					argIdx++
				}
			case 'I', 'L':
				// %I identifier / %L literal quoting: render plain — catalog
				// format() calls only format display names
				if argIdx < len(rest) {
					s, _ := toString(rest[argIdx])
					b.WriteString(s)
					argIdx++
				}
			case '%':
				b.WriteByte('%')
			default:
				b.WriteByte('%')
				b.WriteByte(tpl[i])
			}
			i++
		}
		return b.String(), nil
	})
	register("lower", OIDText, 1, func(ec *evalCtx, args []any) (any, error) {
		s, _ := toString(args[0])
		return strings.ToLower(s), nil
	})
	register("upper", OIDText, 1, func(ec *evalCtx, args []any) (any, error) {
		s, _ := toString(args[0])
		return strings.ToUpper(s), nil
	})
	register("array_to_string", OIDText, -1, func(ec *evalCtx, args []any) (any, error) {
		if len(args) < 2 {
			return nil, nil
		}
		arr, ok := args[0].([]any)
		if !ok {
			s, _ := toString(args[0])
			return s, nil
		}
		sep, _ := toString(args[1])
		parts := make([]string, 0, len(arr))
		for _, e := range arr {
			if e == nil {
				continue
			}
			s, _ := toString(e)
			parts = append(parts, s)
		}
		return strings.Join(parts, sep), nil
	})
	register("cardinality", OIDInt4, 1, func(ec *evalCtx, args []any) (any, error) {
		if arr, ok := args[0].([]any); ok {
			return float64(len(arr)), nil
		}
		return float64(0), nil
	})
	register("txid_current", OIDInt8, 0, func(ec *evalCtx, _ []any) (any, error) { return float64(1), nil })

	register("replace", OIDText, 3, func(ec *evalCtx, args []any) (any, error) {
		if args[0] == nil {
			return nil, nil
		}
		s, _ := toString(args[0])
		from, _ := toString(args[1])
		to, _ := toString(args[2])
		return strings.ReplaceAll(s, from, to), nil
	})
	register("substring", OIDText, -1, func(ec *evalCtx, args []any) (any, error) {
		if len(args) < 2 || args[0] == nil {
			return nil, nil
		}
		s, _ := toString(args[0])
		start, ok := toFloat(args[1])
		if !ok {
			return nil, errf(0, "substring start must be a number")
		}
		// PostgreSQL positions are 1-based; start 0 clamps to 1
		begin := int(start) - 1
		if begin < 0 {
			begin = 0
		}
		if begin > len(s) {
			begin = len(s)
		}
		end := len(s)
		if len(args) > 2 {
			length, ok := toFloat(args[2])
			if !ok {
				return nil, errf(0, "substring length must be a number")
			}
			if n := begin + int(length); n < end {
				end = n
			}
			if end < begin {
				end = begin
			}
		}
		return s[begin:end], nil
	})

	// function decompilers alongside the other pg_get_* stubs
	registerNul("pg_get_function_result", 1)
	registerNul("pg_get_function_arguments", 1)
	registerNul("pg_get_function_identity_arguments", 1)
}

func quoteText(ec *evalCtx, args []any) (any, error) {
	if len(args) == 0 {
		return nil, nil
	}
	if args[0] == nil {
		return nil, nil
	}
	s, _ := toString(args[0])
	return "\"" + strings.ReplaceAll(s, "\"", "\"\"") + "\"", nil
}

// lookupFunction resolves a possibly schema-qualified function name to the
// whitelist. Schema qualifiers are decoration here: pg_catalog and
// information_schema functions collapse to their bare names.
func lookupFunction(name string) (*funcDef, bool) {
	for _, prefix := range []string{"pg_catalog.", "information_schema."} {
		if strings.HasPrefix(name, prefix) {
			name = strings.TrimPrefix(name, prefix)
			break
		}
	}
	fd, ok := functionRegistry[name]
	return fd, ok
}
