package pgserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jeroenrinzema/psql-wire"
	"github.com/jeroenrinzema/psql-wire/codes"
	psqlerr "github.com/jeroenrinzema/psql-wire/errors"

	"github.com/huanghaoyuanhhy/lyrebird/internal/catalog"
)

// Session statements: SET / SHOW / RESET / DISCARD and the transaction
// family. lyrebird executes single statements with no transaction state, so
// these answer at the ack level — the CommandComplete tags clients wait on —
// and SHOW reads the same settings table the catalog's pg_settings projects.
// This is what keeps pgjdbc, DataGrip and psql alive from the first packet:
// pgjdbc configures every connection with SETs, DataGrip probes with SHOWs,
// and both open transactions when autocommit is off.

// isSessionStatement classifies a statement's first word.
func isSessionStatement(word string) bool {
	switch word {
	case "SET", "SHOW", "RESET", "DISCARD",
		"BEGIN", "START", "COMMIT", "END", "ROLLBACK", "ABORT":
		return true
	}
	return false
}

// sessionStatement builds the wire answer for one classified session
// statement. words is the tokenized statement; the first word is normalized
// here because a bare word like "show" lexes as an identifier, not a
// keyword.
func (s *Server) sessionStatement(words []string) (wire.PreparedStatements, error) {
	switch strings.ToUpper(words[0]) {
	case "SET":
		// values are accepted, not applied: there is no session state to
		// change (search_path is always public, timezone always UTC)
		return ackStatement("SET"), nil
	case "RESET":
		return ackStatement("RESET"), nil
	case "DISCARD":
		tag := "DISCARD ALL"
		if len(words) > 1 {
			tag = "DISCARD " + strings.ToUpper(words[1])
		}
		return ackStatement(tag), nil
	case "BEGIN", "START":
		return ackStatement("BEGIN"), nil
	case "COMMIT", "END":
		return ackStatement("COMMIT"), nil
	case "ROLLBACK", "ABORT":
		return ackStatement("ROLLBACK"), nil
	case "SHOW":
		return s.showStatement(words)
	}
	return nil, psqlerr.WithCode(fmt.Errorf("unhandled session statement %q", words[0]), codes.Internal)
}

// ackStatement is a statement that produces no rows and reports the given
// CommandComplete tag.
func ackStatement(tag string) wire.PreparedStatements {
	return wire.Prepared(wire.NewStatement(func(ctx context.Context, writer wire.DataWriter, parameters []wire.Parameter) error {
		return writer.Complete(tag)
	}))
}

// showAliases maps PG's multi-word SHOW forms onto their setting names.
var showAliases = map[string]string{
	"time zone":                   "TimeZone",
	"transaction isolation level": "transaction_isolation",
	"session authorization":       "session_authorization",
}

// showSetting resolves the words after SHOW into (canonical name, value):
// the single-word form and the two multi-word aliases PG accepts.
func showSetting(words []string) (name, value string, ok bool) {
	if len(words) < 2 {
		return "", "", false
	}
	phrase := strings.ToLower(strings.Join(words[1:], " "))
	if canonical, aliased := showAliases[phrase]; aliased {
		v, has := catalog.Setting(canonical)
		return canonical, v, has
	}
	v, has := catalog.Setting(words[1])
	return strings.ToLower(words[1]), v, has
}

// showStatement answers SHOW name and SHOW ALL from the settings table.
func (s *Server) showStatement(words []string) (wire.PreparedStatements, error) {
	if len(words) < 2 {
		return nil, psqlerr.WithCode(fmt.Errorf("expected a setting name after SHOW"), codes.Syntax)
	}
	if strings.ToUpper(words[1]) == "ALL" {
		return showAllStatement(), nil
	}
	name, value, ok := showSetting(words)
	if !ok {
		return nil, psqlerr.WithCode(
			fmt.Errorf("unrecognized configuration parameter %q", strings.ToLower(words[1])),
			codes.UndefinedObject)
	}
	columns := wire.Columns{{Name: name, Oid: pgtype.TextOID}}
	return wire.Prepared(wire.NewStatement(func(ctx context.Context, writer wire.DataWriter, parameters []wire.Parameter) error {
		if err := writer.Row([]any{value}); err != nil {
			return err
		}
		return writer.Complete("SHOW")
	}, wire.WithColumns(columns))), nil
}

// showAllStatement renders SHOW ALL: the whole settings table.
func showAllStatement() wire.PreparedStatements {
	columns := wire.Columns{
		{Name: "name", Oid: pgtype.TextOID},
		{Name: "setting", Oid: pgtype.TextOID},
		{Name: "description", Oid: pgtype.TextOID},
	}
	return wire.Prepared(wire.NewStatement(func(ctx context.Context, writer wire.DataWriter, parameters []wire.Parameter) error {
		for _, st := range catalog.AllSettings() {
			if err := writer.Row([]any{st.Name, st.Value, st.Description}); err != nil {
				return err
			}
		}
		return writer.Complete("SHOW")
	}, wire.WithColumns(columns)))
}
