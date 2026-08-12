package studio

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// shutdownGrace bounds how long Start waits for in-flight requests to drain on
// shutdown before forcing connections closed. A cancelled context must stop the
// server in bounded time even if a handler is stuck, so the wait cannot be
// unbounded.
const shutdownGrace = 5 * time.Second

// Start builds a Studio from config and serves it on addr until ctx is
// cancelled. It owns the listener and the shutdown: when ctx is done the server
// is drained within a bounded grace period and Start returns ctx.Err(), or nil
// if the server stopped for its own reason first.
//
// Request handlers derive their context from ctx, so cancelling ctx cancels
// in-flight runs as well as stopping the listener. A handler that ignores
// cancellation cannot block shutdown indefinitely: after the grace period the
// server forces connections closed.
//
// Start is the explicit opt-in that keeps the UI off by default. A program that
// never calls Start (or never serves Handler itself) has no Studio listening,
// so the UI cannot be reached, and the agents and workflows the program runs
// are unaffected by its absence.
func Start(ctx context.Context, addr string, config Config) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return serve(ctx, listener, config)
}

// serve runs a Studio on an already-bound listener until ctx is cancelled. Start
// is the public entry that binds addr; serve is factored out so a test can bind
// its own ephemeral listener and pass it in, which removes the free-then-rebind
// window a bind-by-address test would otherwise race on. serve always closes the
// listener, through Serve's own shutdown path.
func serve(ctx context.Context, listener net.Listener, config Config) error {
	studio, err := New(config)
	if err != nil {
		_ = listener.Close()
		return err
	}

	server := &http.Server{
		Handler: studio.Handler(),
		// Every request context descends from ctx, so cancelling ctx propagates
		// into running handlers rather than only closing the listener.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()

	select {
	case <-ctx.Done():
		// A cancelled context is the expected stop signal. Drain within a bounded
		// grace period; if handlers do not finish in time, Close forces the
		// remaining connections shut so Start cannot hang. A graceful-shutdown
		// error other than the deadline takes precedence because it describes an
		// unclean stop.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		shutdownErr := server.Shutdown(shutdownCtx)
		if errors.Is(shutdownErr, context.DeadlineExceeded) {
			_ = server.Close()
			return ctx.Err()
		}
		if shutdownErr != nil {
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
