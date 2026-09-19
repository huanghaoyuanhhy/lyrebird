package catalog

import "fmt"

// Error is a catalog-layer failure: the query text is client-authored SQL,
// so failures must come back as PG syntax/capability errors, not 500s.
// Type carries the SQLSTATE verbatim, the same contract translate.Error
// uses (pgserver renders it with no mapping).
type Error struct {
	Type   string // SQLSTATE code: 42601 syntax, 42883 undefined function, 0A000 beyond the subset
	Reason string
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Type, e.Reason) }

func errf(pos int, format string, args ...any) error {
	return &Error{Type: "42601", Reason: fmt.Sprintf(format, args...)}
}

func capErrf(format string, args ...any) error {
	return &Error{Type: "0A000", Reason: fmt.Sprintf(format, args...)}
}

func funcErrf(name string) error {
	return &Error{Type: "42883", Reason: fmt.Sprintf("function %s is not available in catalog queries", name)}
}
