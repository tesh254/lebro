package studio

import (
	"context"
	"net"
)

// ServeForTest exposes the internal serve entry to the external test package so
// a test can pass a listener it bound itself, avoiding the port race a
// bind-by-address test would have between freeing and rebinding an ephemeral
// port.
func ServeForTest(ctx context.Context, listener net.Listener, config Config) error {
	return serve(ctx, listener, config)
}
