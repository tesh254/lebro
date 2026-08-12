package studio

import (
	"context"
	"errors"
	"net"
	"net/http"
)

// Start builds a Studio from config and serves it on addr until ctx is
// cancelled. It owns the listener and the shutdown: when ctx is done the server
// is gracefully shut down and Start returns ctx.Err() wrapped, or nil if the
// server stopped for its own reason first.
//
// Start is the explicit opt-in that keeps the UI off by default. A program that
// never calls Start (or never serves Handler itself) has no Studio listening,
// so the UI cannot be reached, and the agents and workflows the program runs
// are unaffected by its absence.
func Start(ctx context.Context, addr string, config Config) error {
	studio, err := New(config)
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	server := &http.Server{Handler: studio.Handler()}

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()

	select {
	case <-ctx.Done():
		// A cancelled context is the expected stop signal. Shut down gracefully
		// and report the cause; a shutdown error, if any, takes precedence
		// because it describes an unclean stop.
		if shutdownErr := server.Shutdown(context.Background()); shutdownErr != nil {
			return shutdownErr
		}
		return ctx.Err()
	case err := <-serveErr:
		// Serve always returns a non-nil error; ErrServerClosed means a caller
		// shut the server down elsewhere, which is not a failure.
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
