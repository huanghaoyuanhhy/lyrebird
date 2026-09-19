package catalog

// Named special cases: client queries whose shape outgrows the general
// engine (derived tables, window functions, table functions) run through
// dedicated row generators instead of being parsed into ever-deeper engine
// features. Each case is registered with the client method it serves, and
// its test fixtures are that client's real SQL text (see special_test.go).

import "context"

// specialCase recognizes one known client query shape.
type specialCase struct {
	// name identifies the client method this case serves (e.g. the pgjdbc
	// DatabaseMetaData method), for logs and test names.
	name string
	// match inspects the parsed FROM sources; it must be conservative — a
	// false positive silently replaces the general engine's answer.
	match func(st *stmt, bound []boundDef) bool
	// run generates the output directly from a provider snapshot.
	run func(ctx context.Context, snap *snapshot, st *stmt, params []any) ([][]any, error)
	// cols is the output shape.
	cols []Col
}

var specialCases = []specialCase{}

// matchSpecial finds a special case for the parsed query. Only the first
// match wins; order the registry from most to least specific.
func matchSpecial(st *stmt, bound []boundDef) (*specialResult, bool) {
	for _, sc := range specialCases {
		if sc.match(st, bound) {
			caseCopy := sc
			return &specialResult{
				cols: caseCopy.cols,
				run: func(ctx context.Context, snap *snapshot) ([][]any, error) {
					return caseCopy.run(ctx, snap, st, snap.params)
				},
			}, true
		}
	}
	return nil, false
}
