package httpapi

import (
	"context"
	"sync"
	"testing"
)

func TestClientStreamCancelClosesResponseBody(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	body := &closeProbe{closed: make(chan struct{})}
	stream := &ClientStream{ctx: ctx, cancel: cancel, body: body}

	stream.Cancel()
	stream.Cancel()

	select {
	case <-body.closed:
	default:
		t.Fatal("response body was not closed")
	}
	if ctx.Err() == nil {
		t.Fatal("stream context was not cancelled")
	}
	if body.calls != 1 {
		t.Fatalf("response body close calls = %d, want 1", body.calls)
	}
}

type closeProbe struct {
	closed chan struct{}
	once   sync.Once
	calls  int
}

func (p *closeProbe) Close() error {
	p.calls++
	p.once.Do(func() { close(p.closed) })
	return nil
}
