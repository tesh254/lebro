package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
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
	if calls := body.calls.Load(); calls != 1 {
		t.Fatalf("response body close calls = %d, want 1", calls)
	}
}

func TestClientStreamReadPrefersCancelledContextOverBodyError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	body := &blockingReadCloser{closed: make(chan struct{})}
	stream := &ClientStream{ctx: ctx, cancel: cancel, body: body, done: make(chan struct{})}
	events := make(chan StreamEvent)

	go stream.read(&http.Response{Body: body}, events)
	stream.Cancel()
	<-stream.done

	if !errors.Is(stream.outcome_, context.Canceled) {
		t.Fatalf("stream error = %v, want context.Canceled", stream.outcome_)
	}
}

type closeProbe struct {
	closed chan struct{}
	once   sync.Once
	calls  atomic.Int32
}

func (p *closeProbe) Close() error {
	p.once.Do(func() {
		p.calls.Add(1)
		close(p.closed)
	})
	return nil
}

type blockingReadCloser struct {
	closed chan struct{}
	once   sync.Once
}

func (r *blockingReadCloser) Read([]byte) (int, error) {
	<-r.closed
	return 0, errors.New("transport closed")
}

func (r *blockingReadCloser) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

var _ io.ReadCloser = (*blockingReadCloser)(nil)
