package pgserver

import (
	"context"
	"errors"

	"github.com/jeroenrinzema/psql-wire/codes"
	psqlerr "github.com/jeroenrinzema/psql-wire/errors"

	"github.com/huanghaoyuanhhy/lyrebird/internal/translate"
)

// wireError renders a pipeline failure as a PG wire error. translate.Error
// carries its SQLSTATE in Type, so classified failures pass through
// verbatim; everything else is either a canceled statement or an internal
// error. The message stays the reason alone — psql already prints the code.
func wireError(err error) error {
	var te *translate.Error
	if errors.As(err, &te) {
		return psqlerr.WithCode(errors.New(te.Reason), codes.Code(te.Type))
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return psqlerr.WithCode(errors.New("canceling statement due to user request"), codes.QueryCanceled)
	}
	return psqlerr.WithCode(err, codes.Internal)
}
