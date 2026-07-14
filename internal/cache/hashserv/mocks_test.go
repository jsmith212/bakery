package hashserv

import (
	"errors"
	"io"
	"log/slog"
)

// errBadToken is what the fake authenticator returns for anything it does not know. The real
// auth.Service is deliberately coarse here too -- unknown, revoked and expired are one error,
// because a precise one is an oracle.
var errBadToken = errors.New("hashserv: bad token")

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
